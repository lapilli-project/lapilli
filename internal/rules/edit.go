package rules

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Changing a rule where it is written (docs/design/changes.md): the
// document it was read from, changed only on the lines the change is on,
// so comments, order, quoting and indentation stay as they were. The
// YAML's tree says where the rule is; the text is edited there.

// Edit is a change to one rule in a document.
type Edit struct {
	// Name and Group, and UID for a Grafana rule, say which rule: the
	// rule's own name, as written, not one it was told apart by.
	Name, Group, UID string
	// Namespace is a ruler's namespace, or an Azure rule group's resource
	// group, for a rule read with --ruler.
	Namespace string `json:",omitempty"`
	// For, when set, is the rule's new "for".
	For *time.Duration
	// From and To, when set, replace the one place From is written in the
	// rule's expression: a threshold's number.
	From, To string
	// Pause pauses a Grafana rule: isPaused: true.
	Pause bool
	// Delete takes the rule out.
	Delete bool
	// Annotation, when set, is one of the rule's annotations to set to
	// Value, or, with Unset, to take out: what it says, not when it fires.
	Annotation, Value string
	Unset             bool
}

// ErrNotEditable is a document a change can't be made in by editing it:
// it isn't YAML holding the rule as a Prometheus rule file, a
// PrometheusRule, a ConfigMap holding either, or a Grafana provisioning
// file; or the change would need more than replacing what is written.
var ErrNotEditable = errors.New("the change can't be written into this document: make it where the rule is written")

// EditDoc is the document with the edit made. What it gives is read back
// and checked: the same document but for the change, or ErrNotEditable.
func EditDoc(data []byte, e Edit) ([]byte, error) {
	out, err := editDoc(data, e)
	if err != nil {
		return nil, err
	}
	if err := Check(data, out, e); err != nil {
		return nil, err
	}
	return out, nil
}

// EditThreshold is the document with v written in place of b's number in
// r's expression, as WithBound writes it: the number, with as much of what
// is written before it as tells it apart from the rest of the expression.
func EditThreshold(data []byte, r Rule, e Edit, b Bound, v float64) ([]byte, error) {
	cs := comparisons(r.Expr)
	if r.Graph != nil || b.At < 0 || b.At >= len(cs) {
		return nil, ErrNotEditable
	}
	c := cs[b.At]
	after := r.WithBound(b, v)
	if after.Expr == r.Expr {
		return nil, ErrNotEditable
	}
	// What WithBound wrote in the number's place.
	n := after.Expr[c.at : len(after.Expr)-(len(r.Expr)-c.end)]
	for k := 0; k <= 16 && k <= c.at; k++ {
		before := r.Expr[c.at-k : c.at]
		if strings.ContainsAny(before, "\n\r") {
			break
		}
		e.From, e.To = before+r.Expr[c.at:c.end], before+n
		out, err := EditDoc(data, e)
		if err == nil {
			return out, nil
		}
		if !errors.Is(err, ErrNotEditable) {
			return nil, err
		}
	}
	return nil, ErrNotEditable
}

func editDoc(data []byte, e Edit) ([]byte, error) {
	lines := strings.SplitAfter(string(data), "\n")
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if err != nil {
			break
		}
		if out, ok, err := editIn(&doc, lines, e); ok || err != nil {
			return out, err
		}
	}
	return nil, ErrNotEditable
}

// editIn makes the edit in one document of the file, when the rule is in
// it.
func editIn(doc *yaml.Node, lines []string, e Edit) ([]byte, bool, error) {
	if len(doc.Content) == 0 {
		return nil, false, nil
	}
	root := doc.Content[0]
	// A ConfigMap: each value of data that holds the rule, as a block of
	// its own.
	if value(root, "kind") != nil && value(root, "kind").Value == "ConfigMap" {
		data := value(root, "data")
		if data == nil || data.Kind != yaml.MappingNode {
			return nil, false, nil
		}
		for i := 1; i < len(data.Content); i += 2 {
			v := data.Content[i]
			if v.Kind != yaml.ScalarNode || !strings.Contains(v.Value, e.Name) {
				continue
			}
			inner, err := editDoc([]byte(v.Value), e)
			if errors.Is(err, ErrNotEditable) {
				continue
			}
			if err != nil {
				return nil, false, err
			}
			if v.Style != yaml.LiteralStyle {
				return nil, false, ErrNotEditable
			}
			out, err := replaceBlock(lines, v, string(inner))
			return out, true, err
		}
		return nil, false, nil
	}
	groups := value(root, "groups")
	if spec := value(root, "spec"); groups == nil && spec != nil {
		groups = value(spec, "groups") // a PrometheusRule
	}
	if groups == nil || groups.Kind != yaml.SequenceNode {
		return nil, false, nil
	}
	// One rule, and one only: two alike would leave which to change to a
	// guess.
	var in *yaml.Node
	at, group := -1, -1
	for gi, g := range groups.Content {
		rs := value(g, "rules")
		if rs == nil || rs.Kind != yaml.SequenceNode || (e.Group != "" && groupName(g) != e.Group) {
			continue
		}
		for i, r := range rs.Content {
			if !isRule(r, e) {
				continue
			}
			if in != nil {
				return nil, false, ErrNotEditable
			}
			in, at, group = rs, i, gi
		}
	}
	if in == nil {
		return nil, false, nil
	}
	if e.Delete && len(in.Content) == 1 {
		// The group's only rule: the group goes, unless it is the file's
		// only one, which would leave a file of no rules.
		if len(groups.Content) == 1 || groups.Style&yaml.FlowStyle != 0 {
			return nil, true, ErrNotEditable
		}
		out, err := cutItem(lines, groups, group)
		return out, true, err
	}
	out, err := editRule(lines, in, at, e)
	return out, true, err
}

// groupName is a group's name as rules are read with it: a Grafana
// group's under its folder, both with $$ read as $, as Grafana's file
// provisioning reads them.
func groupName(g *yaml.Node) string {
	n := value(g, "name")
	if n == nil {
		return ""
	}
	if f := value(g, "folder"); f != nil && f.Value != "" {
		return provisioned(f.Value) + "/" + provisioned(n.Value)
	}
	return n.Value
}

// isRule reports whether a rule's node is the one an edit is for: by its
// alert, or a Grafana rule's uid, or title when it has no uid given, read
// as Grafana reads them. Nothing an edit writes in a Grafana rule is a
// field Grafana interpolates but its duration: annotations and queries are
// taken as written, so the file keeps its own escaping.
func isRule(r *yaml.Node, e Edit) bool {
	if r.Kind != yaml.MappingNode {
		return false
	}
	if a := value(r, "alert"); a != nil {
		return a.Value == e.Name
	}
	if u := value(r, "uid"); u != nil && e.UID != "" {
		return provisioned(u.Value) == e.UID
	}
	t := value(r, "title")
	return t != nil && provisioned(t.Value) == e.Name
}

// editRule makes the edit in the rule that is the i'th of rules.
func editRule(lines []string, rules *yaml.Node, i int, e Edit) ([]byte, error) {
	r := rules.Content[i]
	// Lines are edited: rules written inline, as JSON is, aren't.
	if rules.Style&yaml.FlowStyle != 0 || r.Style&yaml.FlowStyle != 0 {
		return nil, ErrNotEditable
	}
	last := endOf(lines, rules, i)
	if e.Delete {
		return cutItem(lines, rules, i)
	}
	if e.Annotation != "" {
		return annotate(lines, r, last, e)
	}
	out := append([]string{}, lines...)
	// A key added goes after the rule's last line, indented as its first
	// key is.
	add := func(key, v string) {
		line := strings.Repeat(" ", r.Column-1) + key + ": " + v + "\n"
		if last >= len(out) || !strings.HasSuffix(out[last], "\n") {
			line = "\n" + line
		}
		out = append(out[:last+1], append([]string{line}, out[last+1:]...)...)
	}
	// set writes a scalar's new value where its old one is.
	set := func(key, v string) bool {
		f := value(r, key)
		if f == nil {
			return false
		}
		if f.Kind != yaml.ScalarNode || f.Line-1 >= len(out) {
			return false
		}
		l := out[f.Line-1]
		start := f.Column - 1
		out[f.Line-1] = l[:start] + v + l[scalarEnd(l, start):]
		return true
	}
	if e.Pause {
		if value(r, "title") == nil {
			return nil, ErrNotEditable // a rule file has no pause
		}
		if f := value(r, "isPaused"); f != nil {
			if !set("isPaused", "true") {
				return nil, ErrNotEditable
			}
		} else {
			add("isPaused", "true")
		}
	}
	if e.From != "" {
		x := value(r, "expr")
		if x == nil {
			return nil, ErrNotEditable
		}
		end := last
		if k := nextKey(r, "expr"); k != nil {
			end = k.Line - 2
		}
		at, n := -1, 0
		for l := x.Line - 1; l <= end; l++ {
			n += strings.Count(out[l], e.From)
			if at < 0 && strings.Contains(out[l], e.From) {
				at = l
			}
		}
		if n != 1 {
			return nil, ErrNotEditable
		}
		out[at] = strings.Replace(out[at], e.From, e.To, 1)
	}
	if e.For != nil {
		d := promDuration(*e.For)
		if value(r, "for") != nil {
			if !set("for", d) {
				return nil, ErrNotEditable
			}
		} else {
			add("for", d)
		}
	}
	return []byte(strings.Join(out, "")), nil
}

// cutItem takes the i'th item of a list out: its lines, and the comments just
// above it, which are its own.
func cutItem(lines []string, list *yaml.Node, i int) ([]byte, error) {
	first, last, err := itemSpan(lines, list, i)
	if err != nil {
		return nil, err
	}
	out := append(append([]string{}, lines[:first]...), lines[last+1:]...)
	return []byte(strings.Join(out, "")), nil
}

// itemSpan is the lines the i'th item of a list is on, from the comments
// just above it, which are its own, to its end.
func itemSpan(lines []string, list *yaml.Node, i int) (first, last int, err error) {
	n := list.Content[i]
	if n.Style&yaml.FlowStyle != 0 {
		return 0, 0, ErrNotEditable
	}
	first, last = n.Line-1, endOf(lines, list, i)
	for first > 0 && strings.HasPrefix(strings.TrimSpace(lines[first-1]), "#") && leading(lines[first-1]) >= n.Column-3 {
		first--
	}
	return first, last, nil
}

// annotationKey is what an annotation's name is, to be written as a key.
var annotationKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)

// annotate sets one annotation of the rule r, whose lines end at last, or
// takes it out: the value written on a line of its own as a double-quoted
// scalar, so nothing in it can be read as another key. Every other line
// stays as it was.
func annotate(lines []string, r *yaml.Node, last int, e Edit) ([]byte, error) {
	if !annotationKey.MatchString(e.Annotation) || (!e.Unset && strings.ContainsAny(e.Value, "\r\x00")) {
		return nil, ErrNotEditable
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e.Value); err != nil {
		return nil, ErrNotEditable
	}
	quoted := strings.TrimSuffix(b.String(), "\n")
	splice := func(from, to int, with ...string) []byte {
		out := append(append(append([]string{}, lines[:from]...), with...), lines[to+1:]...)
		return []byte(strings.Join(out, ""))
	}
	// Where the annotations key is, and the line before the key after it.
	ai := -1
	for i := 0; i+1 < len(r.Content); i += 2 {
		if r.Content[i].Value == "annotations" {
			ai = i
		}
	}
	if ai < 0 {
		if e.Unset {
			return nil, ErrNotEditable // nothing to take out
		}
		pad := strings.Repeat(" ", r.Column-1)
		add := []string{pad + "annotations:\n", pad + "  " + e.Annotation + ": " + quoted + "\n"}
		if last >= len(lines) || !strings.HasSuffix(lines[last], "\n") {
			add[0] = "\n" + add[0]
		}
		return splice(last+1, last, add...), nil
	}
	key, ann := r.Content[ai], r.Content[ai+1]
	if ann.Kind != yaml.MappingNode || ann.Style&yaml.FlowStyle != 0 || len(ann.Content) == 0 || key.Line == ann.Content[0].Line {
		return nil, ErrNotEditable
	}
	// back is the last line from from to to that isn't blank or a comment.
	back := func(from, to int) int {
		for to > from && blankOrComment(lines[to]) {
			to--
		}
		return to
	}
	end := last
	if ai+2 < len(r.Content) {
		end = r.Content[ai+2].Line - 2
	}
	end = back(key.Line-1, end)
	for j := 0; j+1 < len(ann.Content); j += 2 {
		k := ann.Content[j]
		if k.Value != e.Annotation {
			continue
		}
		to := end
		if j+2 < len(ann.Content) {
			to = back(k.Line-1, ann.Content[j+2].Line-2)
		}
		if e.Unset && len(ann.Content) == 2 {
			return splice(key.Line-1, end), nil // the only one: annotations go
		}
		if e.Unset {
			return splice(k.Line-1, to), nil
		}
		return splice(k.Line-1, to, strings.Repeat(" ", k.Column-1)+e.Annotation+": "+quoted+"\n"), nil
	}
	if e.Unset {
		return nil, ErrNotEditable
	}
	line := strings.Repeat(" ", ann.Content[0].Column-1) + e.Annotation + ": " + quoted + "\n"
	if !strings.HasSuffix(lines[end], "\n") {
		line = "\n" + line
	}
	return splice(end+1, end, line), nil
}

// endOf is the last line of the i'th rule of rules: before the next
// rule's dash, or the last line the rule's own nodes are on, past any
// block they end in.
func endOf(lines []string, rules *yaml.Node, i int) int {
	if i+1 < len(rules.Content) {
		next := rules.Content[i+1].Line - 1
		// Up to the line before the next rule's dash, leaving out blank
		// lines and comments between them, which are the next one's.
		end := next - 1
		for end > rules.Content[i].Line-1 && blankOrComment(lines[end]) {
			end--
		}
		return end
	}
	last := deepest(rules.Content[i])
	// A block scalar runs on past where its node starts.
	end := last
	indent := rules.Content[i].Column - 1
	for l := last + 1; l < len(lines); l++ {
		t := strings.TrimRight(lines[l], "\r\n")
		if strings.TrimSpace(t) == "" {
			continue
		}
		if leading(t) <= indent {
			break
		}
		end = l
	}
	return end
}

// deepest is the last line a node or those in it starts on, from 0.
func deepest(n *yaml.Node) int {
	last := n.Line - 1
	for _, c := range n.Content {
		last = max(last, deepest(c))
	}
	return last
}

func blankOrComment(l string) bool {
	t := strings.TrimSpace(l)
	return t == "" || strings.HasPrefix(t, "#")
}

func leading(l string) int { return len(l) - len(strings.TrimLeft(l, " ")) }

// scalarEnd is where a scalar written from start ends on its line: its
// closing quote, or before a comment or the line's end.
func scalarEnd(l string, start int) int {
	l = strings.TrimRight(l, "\r\n")
	if start >= len(l) {
		return len(l)
	}
	switch l[start] {
	case '"', '\'':
		if j := strings.IndexByte(l[start+1:], l[start]); j >= 0 {
			return start + 1 + j + 1
		}
		return len(l)
	}
	if j := strings.Index(l[start:], " #"); j >= 0 {
		return start + j
	}
	return len(strings.TrimRight(l, " "))
}

// replaceBlock puts text in place of a literal block's lines, indented as
// they were.
func replaceBlock(lines []string, v *yaml.Node, text string) ([]byte, error) {
	start := v.Line // the line after the one with |
	end := start - 1
	indent := -1
	for l := start; l < len(lines); l++ {
		t := strings.TrimRight(lines[l], "\r\n")
		if strings.TrimSpace(t) == "" {
			end = l
			continue
		}
		if indent < 0 {
			indent = leading(t)
		}
		if leading(t) < indent {
			break
		}
		end = l
	}
	if indent < 0 {
		return nil, ErrNotEditable
	}
	// Blank lines at its end belong to what comes after.
	for end >= start && strings.TrimSpace(lines[end]) == "" {
		end--
	}
	pad := strings.Repeat(" ", indent)
	var body []string
	for _, l := range strings.SplitAfter(strings.TrimRight(text, "\n")+"\n", "\n") {
		if l == "" {
			continue
		}
		if strings.TrimSpace(l) == "" {
			body = append(body, "\n")
			continue
		}
		body = append(body, pad+l)
	}
	out := append(append(append([]string{}, lines[:start]...), body...), lines[end+1:]...)
	return []byte(strings.Join(out, "")), nil
}

// value is a mapping's value for a key, nil when it has none.
func value(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// nextKey is the key after key in a mapping, nil for the last.
func nextKey(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+2 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+2]
		}
	}
	return nil
}

// Hash is a document's hash, as the ledger and the change recipes know it.
func Hash(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// Diff is a unified diff of a document before and after a change, named
// for name, with three lines of context.
func Diff(name string, before, after []byte) string {
	a := strings.SplitAfter(string(before), "\n")
	b := strings.SplitAfter(string(after), "\n")
	if len(a) > 0 && a[len(a)-1] == "" {
		a = a[:len(a)-1]
	}
	if len(b) > 0 && b[len(b)-1] == "" {
		b = b[:len(b)-1]
	}
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	if p == len(a) && p == len(b) {
		return ""
	}
	from := max(0, p-3)
	aEnd, bEnd := min(len(a), len(a)-s+3), min(len(b), len(b)-s+3)
	var out strings.Builder
	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n@@ -%d,%d +%d,%d @@\n", name, name, from+1, aEnd-from, from+1, bEnd-from)
	line := func(prefix, l string) {
		out.WriteString(prefix + strings.TrimRight(l, "\n") + "\n")
	}
	for _, l := range a[from:p] {
		line(" ", l)
	}
	for _, l := range a[p : len(a)-s] {
		line("-", l)
	}
	for _, l := range b[p : len(b)-s] {
		line("+", l)
	}
	for _, l := range a[len(a)-s : aEnd] {
		line(" ", l)
	}
	return out.String()
}

// Check reports whether after is before with the edit made and nothing
// else: both read as YAML, the edit made to before's tree, and the two
// compared whole, annotations, comments' neighbours and all. A change
// written in the wrong place, or one that took more with it, is
// ErrNotEditable.
func Check(before, after []byte, e Edit) error {
	a, err := decodeAll(before)
	if err != nil {
		return ErrNotEditable
	}
	b, err := decodeAll(after)
	if err != nil {
		return ErrNotEditable
	}
	if len(a) != len(b) {
		return ErrNotEditable
	}
	done := 0
	for i := range a {
		n, err := change(&a[i], b[i], e)
		if err != nil {
			return err
		}
		done += n
	}
	if done != 1 || !reflect.DeepEqual(a, b) {
		return ErrNotEditable
	}
	return nil
}

func decodeAll(data []byte) ([]any, error) {
	// In linear time: what a ruler or a cloud holds is outside input, and
	// yaml.v3 decoding a mapping straight to Go values takes time that
	// grows with the square of its keys.
	return DecodeYAML(context.Background(), data)
}

// change makes the edit in a document's tree, as EditDoc makes it in its
// text, and says how many rules it changed. A ConfigMap's rules are in its
// values, a document of their own: the one after changed.
func change(doc *any, after any, e Edit) (int, error) {
	m, _ := (*doc).(map[string]any)
	if m == nil {
		return 0, nil
	}
	if strings.EqualFold(str(m["type"]), azureRuleGroup) {
		return azureChange(m, e)
	}
	if m["kind"] == "ConfigMap" {
		data, _ := m["data"].(map[string]any)
		am, _ := after.(map[string]any)
		ad, _ := am["data"].(map[string]any)
		n := 0
		for k, v := range data {
			x, ok1 := v.(string)
			y, ok2 := ad[k].(string)
			if !ok1 || !ok2 || x == y {
				continue
			}
			if err := Check([]byte(x), []byte(y), e); err != nil {
				return 0, err
			}
			data[k] = y
			n++
		}
		return n, nil
	}
	groups, _ := m["groups"].([]any)
	holder := m
	if spec, _ := m["spec"].(map[string]any); groups == nil && spec != nil {
		groups, _ = spec["groups"].([]any)
		holder = spec
	}
	n := 0
	for gi := 0; gi < len(groups); gi++ {
		g, _ := groups[gi].(map[string]any)
		rs, _ := g["rules"].([]any)
		if g == nil || (e.Group != "" && treeGroup(g) != e.Group) {
			continue
		}
		for i := 0; i < len(rs); i++ {
			r, _ := rs[i].(map[string]any)
			if !treeRule(r, e) {
				continue
			}
			n++
			switch {
			case e.Delete && len(rs) == 1:
				groups = append(groups[:gi:gi], groups[gi+1:]...)
				holder["groups"] = groups
				gi--
			case e.Delete:
				rs = append(rs[:i:i], rs[i+1:]...)
				g["rules"] = rs
				i--
			case e.Annotation != "":
				a, _ := r["annotations"].(map[string]any)
				switch {
				case e.Unset:
					delete(a, e.Annotation)
					if len(a) == 0 {
						delete(r, "annotations")
					}
				case a == nil:
					r["annotations"] = map[string]any{e.Annotation: e.Value}
				default:
					a[e.Annotation] = e.Value
				}
			default:
				if e.For != nil {
					r["for"] = promDuration(*e.For)
				}
				if e.Pause {
					r["isPaused"] = true
				}
				if e.From != "" {
					x, _ := r["expr"].(string)
					if strings.Count(x, e.From) != 1 {
						return 0, ErrNotEditable
					}
					r["expr"] = strings.Replace(x, e.From, e.To, 1)
				}
			}
			if e.Delete && len(rs) == 0 {
				break
			}
		}
	}
	return n, nil
}

func treeGroup(g map[string]any) string {
	name := fmt.Sprint(g["name"])
	if f, _ := g["folder"].(string); f != "" {
		return provisioned(f) + "/" + provisioned(name)
	}
	return name
}

func treeRule(r map[string]any, e Edit) bool {
	if r == nil {
		return false
	}
	if a, ok := r["alert"]; ok {
		return fmt.Sprint(a) == e.Name
	}
	if u, ok := r["uid"]; ok && e.UID != "" {
		return provisioned(fmt.Sprint(u)) == e.UID
	}
	t, ok := r["title"]
	return ok && provisioned(fmt.Sprint(t)) == e.Name
}
