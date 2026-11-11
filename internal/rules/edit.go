package rules

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
	// For, when set, is the rule's new "for".
	For *time.Duration
	// From and To, when set, replace the one place From is written in the
	// rule's expression: a threshold's number.
	From, To string
	// Pause pauses a Grafana rule: isPaused: true.
	Pause bool
	// Delete takes the rule out.
	Delete bool
}

// ErrNotEditable is a document a change can't be made in by editing it:
// it isn't YAML holding the rule as a Prometheus rule file, a
// PrometheusRule, a ConfigMap holding either, or a Grafana provisioning
// file; or the change would need more than replacing what is written.
var ErrNotEditable = errors.New("the change can't be written into this document: make it where the rule is written")

// EditDoc is the document with the edit made.
func EditDoc(data []byte, e Edit) ([]byte, error) {
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
			inner, err := EditDoc([]byte(v.Value), e)
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
		out, err := cut(lines, groups, group)
		return out, true, err
	}
	out, err := editRule(lines, in, at, e)
	return out, true, err
}

// groupName is a group's name as rules are read with it: a Grafana
// group's under its folder.
func groupName(g *yaml.Node) string {
	n := value(g, "name")
	if n == nil {
		return ""
	}
	if f := value(g, "folder"); f != nil && f.Value != "" {
		return f.Value + "/" + n.Value
	}
	return n.Value
}

// isRule reports whether a rule's node is the one an edit is for: by its
// alert, or a Grafana rule's uid, or title when it has no uid given.
func isRule(r *yaml.Node, e Edit) bool {
	if r.Kind != yaml.MappingNode {
		return false
	}
	if a := value(r, "alert"); a != nil {
		return a.Value == e.Name
	}
	if u := value(r, "uid"); u != nil && e.UID != "" {
		return u.Value == e.UID
	}
	t := value(r, "title")
	return t != nil && t.Value == e.Name
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
		return cut(lines, rules, i)
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

// cut takes the i'th item of a list out: its lines, and the comments just
// above it, which are its own.
func cut(lines []string, list *yaml.Node, i int) ([]byte, error) {
	n := list.Content[i]
	if n.Style&yaml.FlowStyle != 0 {
		return nil, ErrNotEditable
	}
	first, last := n.Line-1, endOf(lines, list, i)
	for first > 0 && strings.HasPrefix(strings.TrimSpace(lines[first-1]), "#") && leading(lines[first-1]) >= n.Column-3 {
		first--
	}
	out := append(append([]string{}, lines[:first]...), lines[last+1:]...)
	return []byte(strings.Join(out, "")), nil
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
