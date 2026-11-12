package rules

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// A chart rendered with helm template, or what kustomize build writes, is
// one stream of many documents: the rules (ConfigMaps of rule files or of
// Grafana provisioning files, PrometheusRules) among Deployments, Services
// and the rest. Reading it keeps the documents of rules and passes the rest
// over, counting them; a selector keeps only the labelled ones.

// Selector is a Kubernetes label selector, as kubectl -l takes one:
// requirements separated by commas, all of which must hold, each
// key=value (or ==), key!=value, key in (a,b), key notin (a,b), key (it
// has the label), !key (it hasn't), or key>N and key<N (its value a whole
// number greater or less than N). The zero Selector matches everything. A
// set with an empty value in it, a in (x,), is refused, though kubectl
// takes it: written so, it is a slip.
type Selector struct {
	text string
	reqs []requirement
}

type requirement struct {
	key string
	// op is =, !=, in, notin, exists, !, gt or lt.
	op     string
	values []string
}

var (
	labelName   = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)
	labelValue  = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?)?$`)
	dnsPrefix   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	setReq      = regexp.MustCompile(`^(\S+)\s+(in|notin)\s*\((.*)\)$`)
	wholeNumber = regexp.MustCompile(`^[0-9]{1,18}$`)
)

// ParseSelector reads a label selector; an empty one matches everything.
func ParseSelector(s string) (Selector, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Selector{}, nil
	}
	if len(s) > 4096 {
		return Selector{}, errors.New("a label selector of more than 4096 characters")
	}
	sel := Selector{text: s}
	depth, start := 0, 0
	var parts []string
	for i, c := range s {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
		if depth < 0 {
			return Selector{}, fmt.Errorf("%q: its parentheses don't match", s)
		}
	}
	if depth != 0 {
		return Selector{}, fmt.Errorf("%q: its parentheses don't match", s)
	}
	parts = append(parts, s[start:])
	for _, p := range parts {
		r, err := parseRequirement(strings.TrimSpace(p))
		if err != nil {
			return Selector{}, fmt.Errorf("%q: %w", s, err)
		}
		sel.reqs = append(sel.reqs, r)
	}
	return sel, nil
}

func parseRequirement(p string) (requirement, error) {
	var r requirement
	switch {
	case p == "":
		return r, errors.New("an empty requirement: want key=value, key!=value, key in (a,b), key notin (a,b), key or !key")
	case strings.HasPrefix(p, "!"):
		r = requirement{key: strings.TrimSpace(p[1:]), op: "!"}
	case setReq.MatchString(p):
		m := setReq.FindStringSubmatch(p)
		r = requirement{key: m[1], op: m[2]}
		for _, v := range strings.Split(m[3], ",") {
			if v = strings.TrimSpace(v); v == "" {
				return r, fmt.Errorf("%q: an empty value in a set: write %s %s (x,y)", p, m[1], m[2])
			}
			r.values = append(r.values, v)
		}
	case strings.ContainsAny(p, "<>"):
		i := strings.IndexAny(p, "<>")
		r = requirement{key: strings.TrimSpace(p[:i]), op: map[byte]string{'>': "gt", '<': "lt"}[p[i]], values: []string{strings.TrimSpace(p[i+1:])}}
		if !wholeNumber.MatchString(r.values[0]) {
			return r, fmt.Errorf("%q: > and < take a whole number", p)
		}
	case strings.Contains(p, "!="):
		k, v, _ := strings.Cut(p, "!=")
		r = requirement{key: strings.TrimSpace(k), op: "!=", values: []string{strings.TrimSpace(v)}}
	case strings.Contains(p, "=="):
		k, v, _ := strings.Cut(p, "==")
		r = requirement{key: strings.TrimSpace(k), op: "=", values: []string{strings.TrimSpace(v)}}
	case strings.Contains(p, "="):
		k, v, _ := strings.Cut(p, "=")
		r = requirement{key: strings.TrimSpace(k), op: "=", values: []string{strings.TrimSpace(v)}}
	default:
		r = requirement{key: p, op: "exists"}
	}
	if !validKey(r.key) {
		return r, fmt.Errorf("%q isn't a label's name", r.key)
	}
	for _, v := range r.values {
		if !labelValue.MatchString(v) {
			return r, fmt.Errorf("%q isn't a label's value", v)
		}
	}
	return r, nil
}

// validKey reports whether k is a label's name: a name, with a DNS prefix
// and a slash before it or none.
func validKey(k string) bool {
	prefix, name, ok := strings.Cut(k, "/")
	if !ok {
		return labelName.MatchString(k)
	}
	return len(prefix) <= 253 && dnsPrefix.MatchString(prefix) && labelName.MatchString(name)
}

// String is the selector as it was given.
func (s Selector) String() string { return s.text }

// Empty reports whether the selector matches everything.
func (s Selector) Empty() bool { return len(s.reqs) == 0 }

// Matches reports whether labels meet every requirement.
func (s Selector) Matches(labels map[string]string) bool {
	for _, r := range s.reqs {
		v, has := labels[r.key]
		var ok bool
		switch r.op {
		case "=":
			ok = has && v == r.values[0]
		case "!=":
			ok = !has || v != r.values[0]
		case "in":
			ok = has && slices.Contains(r.values, v)
		case "notin":
			ok = !has || !slices.Contains(r.values, v)
		case "exists":
			ok = has
		case "!":
			ok = !has
		case "gt", "lt":
			n, err := strconv.ParseInt(v, 10, 64)
			want, _ := strconv.ParseInt(r.values[0], 10, 64)
			ok = err == nil && (r.op == "gt" && n > want || r.op == "lt" && n < want) // no label: no number
		}
		if !ok {
			return false
		}
	}
	return true
}

// Picked is what reading rules passed over in streams of documents.
type Picked struct {
	// Other are documents that hold no rules: a rendered chart's
	// Deployments, Services and ConfigMaps of other things.
	Other int
	// Unselected are documents of rules whose labels the selector didn't
	// match.
	Unselected int
	// Broken are ConfigMaps holding a rule file that doesn't read: kept
	// if they hold others that do, and said.
	Broken int
}

// Add counts what another read passed over too.
func (p *Picked) Add(q Picked) {
	p.Other += q.Other
	p.Unselected += q.Unselected
	p.Broken += q.Broken
}

// String says what was passed over, "" for nothing.
func (p Picked) String() string {
	var says []string
	switch p.Other {
	case 0:
	case 1:
		says = append(says, "1 document that holds no rules")
	default:
		says = append(says, fmt.Sprintf("%d documents that hold no rules", p.Other))
	}
	switch p.Unselected {
	case 0:
	case 1:
		says = append(says, "1 document of rules the selector doesn't match")
	default:
		says = append(says, fmt.Sprintf("%d documents of rules the selector doesn't match", p.Unselected))
	}
	switch p.Broken {
	case 0:
	case 1:
		says = append(says, "1 ConfigMap with a rule file that doesn't read")
	default:
		says = append(says, fmt.Sprintf("%d ConfigMaps with a rule file that doesn't read", p.Broken))
	}
	return strings.Join(says, ", and ")
}

// Picking is how ReadPicked reads.
type Picking struct {
	// Select keeps, of the documents that are Kubernetes objects (a kind
	// and metadata), those whose labels it matches. A document that isn't
	// one, such as a Prometheus rule file, has no labels to select by, and
	// is kept.
	Select Selector
	// Trim keeps in the documents given back only the documents of rules
	// read, so that nothing passed over, such as a rendered chart's
	// Secrets, is kept with them. Without it, a file is given back as it
	// is, unless the selector left something out of it: reading the
	// documents again with FromDocs must leave out what it did.
	Trim bool
}

// ErrNoneSelected is a read whose documents of rules the selector matched
// none of.
var ErrNoneSelected = errors.New("no document of rules has the labels asked for")

// ReadPicked is Read, as Picking says: of a stream of documents, only the
// documents of rules, and with a selector, only the labelled ones; the
// documents given back hold only what was kept, when Picking.Trim says so
// or the selector left something out. picked says what was passed over.
func ReadPicked(path string, p Picking) ([]Rule, []Doc, Picked, error) {
	var picked Picked
	fi, err := os.Stat(path)
	if err != nil {
		return nil, nil, picked, err
	}
	if !fi.IsDir() {
		rs, d, err := loadPicked(path, p, &picked)
		if err != nil {
			return nil, nil, picked, err
		}
		return rs, []Doc{d}, picked, nil
	}
	walk := path
	if l, err := os.Lstat(path); err == nil && l.Mode()&os.ModeSymlink != 0 {
		walk = path + string(filepath.Separator)
	}
	root := filepath.Clean(path)
	var out []Rule
	var docs []Doc
	err = filepath.WalkDir(walk, func(f string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if filepath.Clean(f) != root && strings.HasPrefix(e.Name(), ".") {
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if e.IsDir() {
			return nil
		}
		if st, err := os.Stat(f); err != nil || !st.Mode().IsRegular() {
			return nil
		}
		rs, d, err := loadPicked(f, p, &picked)
		switch {
		case errors.Is(err, errNoRules), errors.Is(err, ErrNoneSelected):
			return nil
		case err != nil && filepath.Dir(f) != root:
			return nil
		case err != nil:
			return err
		}
		out = append(out, rs...)
		docs = append(docs, d)
		return nil
	})
	if err != nil {
		return nil, nil, picked, err
	}
	if len(out) == 0 {
		if picked.Unselected > 0 {
			return nil, nil, picked, fmt.Errorf("%s: %w (%s): %d documents of rules have other labels", path, ErrNoneSelected, p.Select, picked.Unselected)
		}
		return nil, nil, picked, fmt.Errorf("%s: %w", path, ErrNoneInDir)
	}
	return out, docs, picked, nil
}

// loadPicked reads one file as ReadPicked does, adding what it passed over
// to picked.
func loadPicked(path string, p Picking, picked *Picked) ([]Rule, Doc, error) {
	b, err := readFile(path)
	if err != nil {
		return nil, Doc{}, err
	}
	kept, q, err := Pick(b, p.Select)
	if err != nil {
		return nil, Doc{}, fmt.Errorf("%s: %w", path, err)
	}
	picked.Add(q)
	if q.Unselected > 0 || p.Trim && q.Other > 0 {
		b = kept
	}
	rs, err := Parse(b)
	switch {
	case errors.Is(err, errNoRules) && q.Unselected > 0:
		return nil, Doc{}, fmt.Errorf("%s: %w (%s): %d documents of rules have other labels", path, ErrNoneSelected, p.Select, q.Unselected)
	case err != nil:
		return nil, Doc{}, fmt.Errorf("%s: %w", path, err)
	}
	for i := range rs {
		rs[i].Source = path
	}
	return rs, Doc{Source: path, Data: b}, nil
}

// MaxFile is the most a file of rules is read of: more is refused. A chart
// rendered whole is far less; a file more is likely not rules, or made to
// take a reader's memory.
const MaxFile = 16 << 20

// readFile reads a file of rules, MaxFile at most.
func readFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxFile+1))
	if err == nil && len(b) > MaxFile {
		err = fmt.Errorf("%s: more than %d MiB: a file of rules is less", path, MaxFile>>20)
	}
	return b, err
}

// Pick keeps of a stream of documents those that hold rules and, of the
// Kubernetes objects among them, those whose labels sel matches: each as
// it was written, but for a List (or a list of documents) some of whose
// items were left out, which is written afresh with the others. It says
// what it passed over. A stream that isn't YAML is refused as Parse refuses
// it; a document of rules that can't be read is refused only when kept.
func Pick(b []byte, sel Selector) ([]byte, Picked, error) {
	var picked Picked
	pieces := split(b)
	var out [][]byte
	whole := true // each piece one document, read alone
	for _, piece := range pieces {
		docs, err := decodeAll(piece)
		if err != nil || len(docs) > 1 {
			whole = false
			break
		}
		if len(docs) == 0 || docs[0] == nil {
			continue // only comments, as helm template writes for an empty template
		}
		keep, rewritten, q := pickDoc(docs[0], sel)
		picked.Add(q)
		switch {
		case !keep:
		case rewritten != nil:
			out = append(out, rewritten)
		default:
			out = append(out, piece)
		}
	}
	if !whole {
		// Not cut where YAML cuts it: the documents decoded from the whole,
		// and those kept written afresh.
		picked, out = Picked{}, nil
		docs, err := decodeAll(b)
		if err != nil {
			return nil, picked, err
		}
		for _, d := range docs {
			if d == nil {
				continue
			}
			keep, rewritten, q := pickDoc(d, sel)
			picked.Add(q)
			if !keep {
				continue
			}
			if rewritten == nil {
				if rewritten, err = yaml.Marshal(d); err != nil {
					return nil, picked, err
				}
			}
			out = append(out, rewritten)
		}
	}
	return join(out), picked, nil
}

// pickDoc says whether to keep a document: one that holds rules and is
// selected, or a List, or a list of documents, some of whose items are;
// for such a list missing items, the list written afresh with those kept.
func pickDoc(doc any, sel Selector) (keep bool, rewritten []byte, picked Picked) {
	l, ok := doc.([]any)
	if !ok {
		keep, kept, changed, picked := pickItem(doc, sel)
		if keep && changed {
			rewritten, _ = yaml.Marshal(kept)
		}
		return keep, rewritten, picked
	}
	// A list of documents, as a cloud's CLI lists resources.
	var kept []any
	changed := false
	for _, it := range l {
		k, v, c, q := pickItem(it, sel)
		picked.Add(q)
		if k {
			kept = append(kept, v)
		}
		changed = changed || c || !k
	}
	if len(kept) == 0 {
		return false, nil, picked
	}
	if changed {
		rewritten, _ = yaml.Marshal(kept)
	}
	return true, rewritten, picked
}

// pickItem says whether to keep a document, and what of it: a List with
// only the items kept, which changed says it is.
func pickItem(doc any, sel Selector) (keep bool, kept any, changed bool, picked Picked) {
	m, _ := doc.(map[string]any)
	if m == nil {
		picked.Other++
		return false, nil, false, picked
	}
	if isList(m) {
		// kubectl get -o yaml: its items, as Parse reads them.
		var items []any
		for _, it := range list(m["items"]) {
			k, v, c, q := pickItem(it, sel)
			picked.Add(q)
			if k {
				items = append(items, v)
			}
			changed = changed || c || !k
		}
		if len(items) == 0 {
			return false, nil, false, picked
		}
		if !changed {
			return true, m, false, picked
		}
		c := make(map[string]any, len(m))
		for k, v := range m {
			c[k] = v
		}
		c["items"] = items
		return true, c, true, picked
	}
	_, ok, _ := parseDoc(m)
	if brokenRuleFile(m) {
		picked.Broken++
	}
	switch {
	case !ok && picked.Broken > 0:
		return false, nil, false, picked
	case !ok:
		picked.Other++
		return false, nil, false, picked
	case !sel.Empty() && isObject(m) && !sel.Matches(objectLabels(m)):
		picked.Unselected++
		return false, nil, false, picked
	}
	return true, m, false, picked
}

// ruleKinds are the kinds of Kubernetes objects rules are read from; a
// List of any kind (ConfigMapList, PrometheusRuleList) is read for its
// items.
var ruleKinds = map[string]bool{"ConfigMap": true, "PrometheusRule": true, "Rules": true, "ClusterRules": true, "GlobalRules": true}

// isList reports whether a document is a list of objects: kind List, or
// a kind's own list, as the API writes it.
func isList(m map[string]any) bool {
	kind := str(m["kind"])
	_, items := m["items"].([]any)
	return kind == "List" || strings.HasSuffix(kind, "List") && items
}

// brokenRuleFile reports whether a ConfigMap holds a rule file that
// doesn't read: a value that is YAML, in the shape of rules, that Parse
// refuses. A value that isn't YAML at all, a configuration file, isn't.
func brokenRuleFile(m map[string]any) bool {
	if m["kind"] != "ConfigMap" {
		return false
	}
	data, _ := m["data"].(map[string]any)
	for _, v := range data {
		s, _ := v.(string)
		if _, err := decodeAll([]byte(s)); err != nil {
			continue
		}
		if _, err := Parse([]byte(s)); err != nil && !errors.Is(err, errNoRules) {
			return true
		}
	}
	return false
}

// isObject reports whether a document is a Kubernetes object, which has
// labels to select by: a kind and metadata.
func isObject(m map[string]any) bool {
	_, meta := m["metadata"].(map[string]any)
	return str(m["kind"]) != "" && meta
}

func objectLabels(m map[string]any) map[string]string {
	meta, _ := m["metadata"].(map[string]any)
	return labels(meta["labels"])
}

// split cuts a stream where a line starts a document (---) or ends one
// (...), as YAML cuts it: such a line is never inside a document, not even
// in a block of text. Each piece but the first starts with the line that
// started it.
func split(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i := 0; i < len(b); {
		end := bytes.IndexByte(b[i:], '\n')
		next := len(b)
		if end >= 0 {
			next = i + end + 1
		}
		line := b[i:next]
		switch {
		case marker(line, "---") && i > start:
			out = append(out, b[start:i])
			start = i
		case marker(line, "..."):
			out = append(out, b[start:next])
			start = next
		}
		i = next
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

// marker reports whether a line is the marker m, alone or followed by a
// space, a tab or its end.
func marker(line []byte, m string) bool {
	if !bytes.HasPrefix(line, []byte(m)) {
		return false
	}
	rest := line[len(m):]
	return len(rest) == 0 || rest[0] == ' ' || rest[0] == '\t' || rest[0] == '\n' || rest[0] == '\r'
}

// join writes documents as one stream, each after the first starting with
// a line of its own.
func join(docs [][]byte) []byte {
	var out bytes.Buffer
	for i, d := range docs {
		if i > 0 && !marker(d, "---") {
			out.WriteString("---\n")
		}
		out.Write(d)
		if len(d) > 0 && d[len(d)-1] != '\n' {
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}
