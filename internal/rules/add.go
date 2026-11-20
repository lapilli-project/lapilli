package rules

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// Adding a rule (docs/design/expectations.md, step 5): an alert rule a
// person or an agent wrote, read strictly as a Prometheus alerting rule
// and written out again by Lapilli, never as it was given, into the place
// the proposal names: a group of a rule file, a PrometheusRule or a rule
// file a ConfigMap holds, or a new group after the last of them.

// Added is an alerting rule a proposal adds, as read: what decides when
// it fires, its labels and its annotations, nothing else.
type Added struct {
	Alert         string
	Expr          string
	For           time.Duration
	KeepFiringFor time.Duration
	Labels        map[string]string
	Annotations   map[string]string
}

// The bounds of a rule added.
const (
	MaxAddedText       = 64 << 10 // the text given
	MaxAddedExpr       = 16 << 10 // its expression
	MaxAddedLabels     = 20       // labels, and annotations, each
	MaxAddedLabel      = 1 << 10  // a label's value
	MaxAddedAnnotation = 4 << 10  // an annotation's value
	MaxAddedFor        = 48 * time.Hour
)

// AddedName is what an added rule's name is: letters, digits, _, :, - and
// ., 200 of them at most, a letter or _ first. It goes into a file, a
// path, a pull request and a cloud's alarm name, so nothing else.
var AddedName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_:.-]{0,199}$`)

// addedKey is what a label's or an annotation's name is.
var addedKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)

// ErrAdded is a rule given that isn't read as one: what it says is why.
type ErrAdded struct{ Why string }

func (e ErrAdded) Error() string { return "the rule: " + e.Why }

func addedErr(format string, args ...any) error { return ErrAdded{fmt.Sprintf(format, args...)} }

// ParseAdded reads a rule given as one Prometheus alerting rule, as a rule
// file's rules list holds one (alert, expr, for, keep_firing_for, labels,
// annotations): one document, a mapping of those keys alone, each once,
// every value text as written (a number or a word YAML reads otherwise is
// to be quoted), no anchor, alias or tag; bounded. A recording rule, a
// group or a file of rules isn't one.
func ParseAdded(text []byte) (Added, error) {
	var a Added
	switch {
	case len(text) > MaxAddedText:
		return a, addedErr("%d bytes at most", MaxAddedText)
	case !utf8.Valid(text):
		return a, addedErr("not UTF-8")
	}
	if err := Nodes(text); err != nil {
		return a, addedErr("%v", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(text))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		return a, addedErr("not YAML: %v", clipErr(err))
	}
	var more yaml.Node
	if err := dec.Decode(&more); err == nil {
		return a, addedErr("give one rule, one document")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return a, addedErr("give one alerting rule: alert, expr, and for, keep_firing_for, labels or annotations")
	}
	root := doc.Content[0]
	if err := plainNodes(root); err != nil {
		return a, err
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		if seen[k.Value] {
			return a, addedErr("%s is given twice", k.Value)
		}
		seen[k.Value] = true
		switch k.Value {
		case "alert", "expr", "for", "keep_firing_for":
			if v.Kind != yaml.ScalarNode || v.ShortTag() != "!!str" {
				return a, addedErr("%s is a text: quote it", k.Value)
			}
		case "labels", "annotations":
			m, err := textMap(k.Value, v)
			if err != nil {
				return a, err
			}
			if k.Value == "labels" {
				a.Labels = m
			} else {
				a.Annotations = m
			}
			continue
		case "record":
			return a, addedErr("a recording rule isn't added here: give an alerting rule")
		default:
			return a, addedErr("%q isn't a key of an alerting rule here: alert, expr, for, keep_firing_for, labels and annotations", clip(k.Value, 64))
		}
		switch k.Value {
		case "alert":
			a.Alert = v.Value
		case "expr":
			a.Expr = v.Value
		case "for", "keep_firing_for":
			d, err := addedDuration(k.Value, v.Value)
			if err != nil {
				return a, err
			}
			if k.Value == "for" {
				a.For = d
			} else {
				a.KeepFiringFor = d
			}
		}
	}
	return a, a.Check()
}

// Check holds an added rule to its bounds: a name a metric's letters, an
// expression of one line or more with nothing that doesn't show but line
// breaks and tabs, durations of 48 hours at most, labels and annotations
// named as Prometheus names them, not its own (__), with values that show.
func (a Added) Check() error {
	switch {
	case !AddedName.MatchString(a.Alert):
		return addedErr("its alert, the name, is letters, digits, _, :, - and ., 200 at most, a letter or _ first")
	case strings.TrimSpace(a.Expr) == "":
		return addedErr("it has no expr")
	case len(a.Expr) > MaxAddedExpr:
		return addedErr("its expr is %d bytes at most", MaxAddedExpr)
	case !shows(a.Expr, true):
		return addedErr("its expr has a character that doesn't show")
	case a.For < 0 || a.For > MaxAddedFor || a.KeepFiringFor < 0 || a.KeepFiringFor > MaxAddedFor:
		return addedErr("for and keep_firing_for are 48h at most")
	case len(a.Labels) > MaxAddedLabels || len(a.Annotations) > MaxAddedLabels:
		return addedErr("%d labels and %d annotations at most", MaxAddedLabels, MaxAddedLabels)
	}
	for _, m := range []struct {
		what string
		kv   map[string]string
		most int
	}{{"label", a.Labels, MaxAddedLabel}, {"annotation", a.Annotations, MaxAddedAnnotation}} {
		for k, v := range m.kv {
			switch {
			case !addedKey.MatchString(k) || strings.HasPrefix(k, "__"):
				return addedErr("a %s's name is letters, digits and _, 63 at most, not starting with __", m.what)
			case k == "alertname":
				return addedErr("alertname is the rule's name, not a label of its own")
			case len(v) > m.most:
				return addedErr("a %s's value is %d bytes at most", m.what, m.most)
			case !shows(v, m.what == "annotation"):
				return addedErr("the %s %s has a character that doesn't show", m.what, k)
			}
		}
	}
	return nil
}

// shows reports whether a text holds only what shows: no control or format
// character, but line breaks and tabs where lines may be.
func shows(s string, lines bool) bool {
	for _, c := range s {
		if (unicode.IsControl(c) && !(lines && (c == '\n' || c == '\t'))) || unicode.Is(unicode.Cf, c) || c == utf8.RuneError ||
			c == ' ' || c == ' ' || c == '\u0085' {
			return false
		}
	}
	return true
}

// plainNodes refuses what YAML could read past what is written: an anchor,
// an alias, a tag, a merge.
func plainNodes(n *yaml.Node) error {
	switch {
	case n.Kind == yaml.AliasNode, n.Anchor != "":
		return addedErr("an anchor or an alias isn't read here: write it out")
	case n.Tag != "" && n.Style&yaml.TaggedStyle != 0:
		return addedErr("a tag isn't read here")
	case n.Kind == yaml.ScalarNode && n.Value == "<<":
		return addedErr("a merge isn't read here: write it out")
	}
	for _, c := range n.Content {
		if err := plainNodes(c); err != nil {
			return err
		}
	}
	return nil
}

// textMap is a mapping of texts: labels, or annotations.
func textMap(what string, v *yaml.Node) (map[string]string, error) {
	if v.Kind != yaml.MappingNode {
		return nil, addedErr("its %s are a mapping of names to texts", what)
	}
	out := map[string]string{}
	for i := 0; i+1 < len(v.Content); i += 2 {
		k, x := v.Content[i], v.Content[i+1]
		if k.Kind != yaml.ScalarNode || x.Kind != yaml.ScalarNode || k.ShortTag() != "!!str" || x.ShortTag() != "!!str" {
			return nil, addedErr("its %s are names and texts: quote a number or a word YAML reads otherwise", what)
		}
		if _, ok := out[k.Value]; ok {
			return nil, addedErr("%s %s is given twice", what, clip(k.Value, 64))
		}
		out[k.Value] = x.Value
	}
	return out, nil
}

// addedDuration reads a for or a keep_firing_for as Prometheus writes one:
// 5m, 1h30m, 0s.
func addedDuration(key, v string) (time.Duration, error) {
	d, ok := readRange(v)
	if v == "0" || v == "0s" {
		return 0, nil
	}
	if !ok {
		return 0, addedErr("its %s %q is a duration such as 5m or 1h30m", key, clip(v, 32))
	}
	return d, nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func clipErr(err error) string { return clip(err.Error(), 200) }

// quoted is a scalar written double-quoted: nothing in it read as YAML.
func quoted(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v, Style: yaml.DoubleQuotedStyle}
}

func plainText(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v} }

// textsNode is a mapping of texts, by name, each value double-quoted.
func textsNode(m map[string]string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		n.Content = append(n.Content, plainText(k), quoted(m[k]))
	}
	return n
}

// Node is the rule as Lapilli writes it: its keys in one order, its name,
// expression, labels and annotations double-quoted (an expression of more
// than one line as a literal block), its durations as Prometheus writes
// them.
func (a Added) Node() *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	expr := quoted(a.Expr)
	if strings.Contains(a.Expr, "\n") && !strings.HasPrefix(a.Expr, " ") && !strings.HasSuffix(a.Expr, " ") {
		expr.Style = yaml.LiteralStyle
	}
	n.Content = append(n.Content, plainText("alert"), quoted(a.Alert), plainText("expr"), expr)
	if a.For > 0 {
		n.Content = append(n.Content, plainText("for"), plainText(promDuration(a.For)))
	}
	if a.KeepFiringFor > 0 {
		n.Content = append(n.Content, plainText("keep_firing_for"), plainText(promDuration(a.KeepFiringFor)))
	}
	if len(a.Labels) > 0 {
		n.Content = append(n.Content, plainText("labels"), textsNode(a.Labels))
	}
	if len(a.Annotations) > 0 {
		n.Content = append(n.Content, plainText("annotations"), textsNode(a.Annotations))
	}
	return n
}

// YAML is the rule as Lapilli writes it, alone: what a proposal shows.
func (a Added) YAML() []byte {
	b, _ := encodeNode(a.Node())
	return b
}

// Tree is the rule as a YAML reader decodes it.
func (a Added) Tree() map[string]any {
	out := map[string]any{"alert": a.Alert, "expr": a.Expr}
	if a.For > 0 {
		out["for"] = promDuration(a.For)
	}
	if a.KeepFiringFor > 0 {
		out["keep_firing_for"] = promDuration(a.KeepFiringFor)
	}
	for _, x := range []struct {
		k string
		m map[string]string
	}{{"labels", a.Labels}, {"annotations", a.Annotations}} {
		if len(x.m) > 0 {
			m := map[string]any{}
			for k, v := range x.m {
				m[k] = v
			}
			out[x.k] = m
		}
	}
	return out
}

func encodeNode(n *yaml.Node) ([]byte, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// addedItem is a list's item as Lapilli writes it: "- " and the node,
// each line moved right by pad.
func addedItem(n *yaml.Node, pad int) ([]string, error) {
	b, err := encodeNode(&yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{n}})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.SplitAfter(string(b), "\n") {
		if l == "" {
			continue
		}
		if strings.TrimSpace(l) == "" {
			out = append(out, l)
			continue
		}
		out = append(out, strings.Repeat(" ", pad)+l)
	}
	return out, nil
}

// groupNode is a new group holding the rule: its name, its interval when
// one is given, and its rules.
func groupNode(group string, interval time.Duration, a Added) *yaml.Node {
	g := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	g.Content = append(g.Content, plainText("name"), quoted(group))
	if interval > 0 {
		g.Content = append(g.Content, plainText("interval"), plainText(promDuration(interval)))
	}
	g.Content = append(g.Content, plainText("rules"), &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{a.Node()}})
	return g
}

// groupTree is a new group as a YAML reader decodes it.
func groupTree(group string, interval time.Duration, a Added) map[string]any {
	g := map[string]any{"name": group, "rules": []any{a.Tree()}}
	if interval > 0 {
		g["interval"] = promDuration(interval)
	}
	return g
}

// ErrNoPlace is a document a rule can't be added to as asked: the group
// isn't in it once (or is, for a new one), it isn't a rule file, a
// PrometheusRule or a ConfigMap of one, or its rules are written inline.
var ErrNoPlace = errors.New("the rule can't be added to this document as asked: add it where it is written")

// AddRule is a document with a rule added at the end of a group's rules,
// or, with newGroup, in a new group of that name, with interval when one
// is given, after the last: in a Prometheus rule file, a PrometheusRule
// (or Google Cloud's Rules, its groups under spec), or a rule file a
// ConfigMap holds in a literal block. A Grafana provisioning file holds
// Grafana's rules, not these. Every other line stays as it was; what is
// written is read back, the document's tree compared whole with the rule,
// or the group, put in it, and nothing else.
func AddRule(data []byte, group string, newGroup bool, interval time.Duration, a Added) ([]byte, error) {
	if group == "" || len(group) > 256 || !shows(group, false) {
		return nil, ErrNoPlace
	}
	out, err := addRule(data, group, newGroup, interval, a)
	if err != nil {
		return nil, err
	}
	before, err := decodeAll(data)
	if err != nil {
		return nil, ErrNoPlace
	}
	after, err := decodeAll(out)
	if err != nil || len(before) != len(after) {
		return nil, ErrNoPlace
	}
	added := 0
	for i := range before {
		n, ok := addedTree(before[i], after[i], group, newGroup, interval, a)
		if !ok {
			return nil, ErrNoPlace
		}
		added += n
	}
	if added != 1 {
		return nil, ErrNoPlace
	}
	if _, err := Parse(out); err != nil {
		return nil, ErrNoPlace
	}
	return out, nil
}

func addRule(data []byte, group string, newGroup bool, interval time.Duration, a Added) ([]byte, error) {
	lines := strings.SplitAfter(string(data), "\n")
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var out []byte
	found := 0
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			break
		}
		got, ok, err := addIn(&doc, lines, group, newGroup, interval, a)
		if err != nil {
			return nil, err
		}
		if ok {
			found++
			out = got
		}
	}
	if found != 1 {
		return nil, ErrNoPlace
	}
	return out, nil
}

// addIn adds the rule in one document of the file, when the group is in
// it (or, for a new one, it holds groups and not that one).
func addIn(doc *yaml.Node, lines []string, group string, newGroup bool, interval time.Duration, a Added) ([]byte, bool, error) {
	if len(doc.Content) == 0 {
		return nil, false, nil
	}
	root := doc.Content[0]
	if k := value(root, "kind"); k != nil && k.Value == "ConfigMap" {
		data := value(root, "data")
		if data == nil || data.Kind != yaml.MappingNode {
			return nil, false, nil
		}
		var out []byte
		found := 0
		for i := 1; i < len(data.Content); i += 2 {
			v := data.Content[i]
			if v.Kind != yaml.ScalarNode || !strings.Contains(v.Value, "groups") {
				continue
			}
			inner, err := addRule([]byte(v.Value), group, newGroup, interval, a)
			if errors.Is(err, ErrNoPlace) {
				continue
			}
			if err != nil {
				return nil, false, err
			}
			if v.Style != yaml.LiteralStyle {
				return nil, false, ErrNoPlace
			}
			if out, err = replaceBlock(lines, v, string(inner)); err != nil {
				return nil, false, err
			}
			found++
		}
		if found > 1 {
			return nil, false, ErrNoPlace
		}
		return out, found == 1, nil
	}
	groups := value(root, "groups")
	if spec := value(root, "spec"); groups == nil && spec != nil {
		groups = value(spec, "groups")
	}
	if groups == nil || groups.Kind != yaml.SequenceNode || len(groups.Content) == 0 {
		return nil, false, nil
	}
	if groups.Style&yaml.FlowStyle != 0 {
		return nil, false, ErrNoPlace
	}
	at := -1
	for i, g := range groups.Content {
		if g.Kind != yaml.MappingNode || value(g, "folder") != nil {
			return nil, false, ErrNoPlace // a Grafana group, or not a group
		}
		if groupName(g) == group {
			if at >= 0 {
				return nil, false, ErrNoPlace
			}
			at = i
		}
	}
	var list *yaml.Node
	var node *yaml.Node
	switch {
	case newGroup && at >= 0:
		return nil, false, ErrNoPlace // there is one of that name
	case newGroup:
		list, node = groups, groupNode(group, interval, a)
	case at < 0:
		return nil, false, nil
	default:
		rs := value(groups.Content[at], "rules")
		if rs == nil || rs.Kind != yaml.SequenceNode || rs.Style&yaml.FlowStyle != 0 || len(rs.Content) == 0 {
			return nil, false, ErrNoPlace
		}
		list, node = rs, a.Node()
	}
	for _, it := range list.Content {
		if it.Style&yaml.FlowStyle != 0 {
			return nil, false, ErrNoPlace
		}
	}
	// The item goes after the list's last, its dash where the first's is.
	first := list.Content[0]
	dash := lines[first.Line-1]
	pad := leading(dash)
	if !strings.HasPrefix(strings.TrimLeft(dash, " "), "-") || pad >= first.Column-1 {
		return nil, false, ErrNoPlace
	}
	item, err := addedItem(node, pad)
	if err != nil {
		return nil, false, err
	}
	end := endOf(lines, list, len(list.Content)-1) + 1
	out := append([]string{}, lines[:end]...)
	if n := len(out); n > 0 && !strings.HasSuffix(out[n-1], "\n") {
		out[n-1] += "\n"
	}
	out = append(append(out, item...), lines[end:]...)
	return []byte(strings.Join(out, "")), true, nil
}

// addedTree reports whether after is before with the rule added as asked,
// and nothing else, and how many it added (0 or 1): a ConfigMap's entry
// read as the rule file it holds.
func addedTree(before, after any, group string, newGroup bool, interval time.Duration, a Added) (int, bool) {
	b := plainTree(before)
	af := plainTree(after)
	if m, _ := b.(map[string]any); m != nil && m["kind"] == "ConfigMap" {
		am, _ := af.(map[string]any)
		bd, _ := m["data"].(map[string]any)
		ad, _ := am["data"].(map[string]any)
		if am == nil || len(bd) != len(ad) {
			return 0, false
		}
		n := 0
		for k, v := range bd {
			was, _ := v.(string)
			is, ok := ad[k].(string)
			if !ok {
				return 0, false
			}
			if was == is {
				continue
			}
			x, err1 := decodeAll([]byte(was))
			y, err2 := decodeAll([]byte(is))
			if err1 != nil || err2 != nil || len(x) != len(y) {
				return 0, false
			}
			for i := range x {
				k, ok := addedTree(x[i], y[i], group, newGroup, interval, a)
				if !ok {
					return 0, false
				}
				n += k
			}
		}
		rest := maps.Clone(m)
		restAfter := maps.Clone(am)
		delete(rest, "data")
		delete(restAfter, "data")
		return n, reflect.DeepEqual(rest, restAfter)
	}
	m, _ := b.(map[string]any)
	if m == nil {
		return 0, reflect.DeepEqual(b, af)
	}
	gs := treeGroups(m)
	if gs == nil {
		if spec, _ := m["spec"].(map[string]any); spec != nil {
			gs, _ = spec["groups"].([]any)
		}
	}
	if gs == nil {
		return 0, reflect.DeepEqual(b, af)
	}
	want := deepClone(b).(map[string]any)
	wantIn := want
	if _, ok := m["groups"]; !ok {
		wantIn = want["spec"].(map[string]any)
	}
	wgs := wantIn["groups"].([]any)
	n := 0
	if newGroup {
		wantIn["groups"] = append(wgs, groupTree(group, interval, a))
		n = 1
	} else {
		for i, g := range wgs {
			gm, _ := g.(map[string]any)
			if gm != nil && treeGroup(gm) == group {
				gm["rules"] = append(treeRules(gm), a.Tree())
				wgs[i] = gm
				n++
			}
		}
	}

	if !reflect.DeepEqual(want, af) {
		// The document doesn't hold the group: as it was, untouched.
		return 0, reflect.DeepEqual(b, af)
	}
	return n, n == 1
}

// deepClone is a decoded tree's copy, its maps and lists its own.
func deepClone(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[k] = deepClone(v)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = deepClone(v)
		}
		return out
	}
	return v
}

// AddsRule reports whether out is now, a ruler's unit, with one rule more,
// the rule a, at the end of group's rules (made as a new group, holding it
// alone, with interval when one is given, when now has no such group),
// and nothing else.
func AddsRule(now, out []byte, group string, interval time.Duration, a Added) bool {
	b, err1 := decodeAll(out)
	if err1 != nil || len(b) != 1 {
		return false
	}
	var a0 any = map[string]any{"groups": []any{}}
	if len(bytes.TrimSpace(now)) > 0 {
		x, err := decodeAll(now)
		if err != nil || len(x) != 1 {
			return false
		}
		a0 = x[0]
	}
	has := false
	for _, g := range treeGroups(plainTree(a0)) {
		if gm, _ := g.(map[string]any); gm != nil && treeGroup(gm) == group {
			has = true
		}
	}
	n, ok := addedTree(a0, b[0], group, !has, interval, a)
	return ok && n == 1
}
