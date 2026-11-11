package rules

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// What a managed Prometheus ruler holds, changed where it is kept
// (docs/design/rulers.md): a Mimir or Cortex group, an Amazon namespace's
// rule file, or an Azure rule group, each written whole with the one rule
// changed.

// TreeHash is what a document hashes to as a tree: its YAML or JSON
// decoded, written out as JSON with its keys in order. A ruler that writes
// the same group out with other indentation or quoting gives the same
// hash.
func TreeHash(data []byte) (string, error) {
	docs, err := decodeAll(data)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(plainTree(docs))
	if err != nil {
		return "", err
	}
	return Hash(b), nil
}

// plainTree is a decoded tree that JSON can write: YAML's maps with keys
// other than strings have them written as text.
func plainTree(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[k] = plainTree(v)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[fmt.Sprint(k)] = plainTree(v)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = plainTree(v)
		}
		return out
	}
	return v
}

// IsAzureGroup reports whether a document is an Azure Prometheus rule
// group, as its API gives one: a JSON resource.
func IsAzureGroup(data []byte) bool {
	t := bytes.TrimSpace(data)
	if len(t) == 0 || t[0] != '{' {
		return false
	}
	var m struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(t, &m) == nil && strings.EqualFold(m.Type, azureRuleGroup)
}

// decodeJSON reads a JSON document as a tree, its numbers as written.
func decodeJSON(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("more than one JSON document")
	}
	if m == nil {
		return nil, errors.New("not a JSON object")
	}
	return m, nil
}

// CanonJSON is a JSON document written out as Lapilli keeps an Azure rule
// group: its keys in order, indented, its numbers as written, so that a
// change to one rule is a change to its lines alone.
func CanonJSON(data []byte) ([]byte, error) {
	m, err := decodeJSON(data)
	if err != nil {
		return nil, err
	}
	return canon(m)
}

func canon(m map[string]any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// EditTree is an Azure rule group with the edit made in its tree, as Check
// makes one to compare: properties.rules, the rule by its alert, a new for
// written as Azure writes one (PT10M), a number in its expression, an
// annotation, or enabled: false to turn it off. A group's last rule isn't
// deleted: a person deletes the rule group.
func EditTree(data []byte, e Edit) ([]byte, error) {
	m, err := decodeJSON(data)
	if err != nil || !strings.EqualFold(str(m["type"]), azureRuleGroup) {
		return nil, ErrNotEditable
	}
	var doc any = m
	n, err := change(&doc, nil, e)
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, ErrNotEditable
	}
	return canon(m)
}

// azureChange makes an edit in an Azure rule group's tree, and says how
// many rules it changed.
func azureChange(m map[string]any, e Edit) (int, error) {
	props, _ := m["properties"].(map[string]any)
	if props == nil || (e.Group != "" && str(m["name"]) != e.Group) {
		return 0, nil
	}
	rs, _ := props["rules"].([]any)
	n := 0
	for i := 0; i < len(rs); i++ {
		r, _ := rs[i].(map[string]any)
		if r == nil || r["alert"] == nil || str(r["alert"]) != e.Name {
			continue
		}
		n++
		switch {
		case e.Delete && len(rs) == 1:
			return 0, ErrNotEditable // a person deletes the rule group
		case e.Delete:
			rs = append(rs[:i:i], rs[i+1:]...)
			props["rules"] = rs
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
				r["for"] = ISODuration(*e.For)
			}
			if e.Pause {
				r["enabled"] = false
			}
			if e.From != "" {
				x, _ := r["expression"].(string)
				if strings.Count(x, e.From) != 1 {
					return 0, ErrNotEditable
				}
				r["expression"] = strings.Replace(x, e.From, e.To, 1)
			}
		}
	}
	return n, nil
}

// EditTreeThreshold is EditTree with v written in place of b's number in
// r's expression, as EditThreshold writes it into a file.
func EditTreeThreshold(data []byte, r Rule, e Edit, b Bound, v float64) ([]byte, error) {
	cs := comparisons(r.Expr)
	if r.Graph != nil || b.At < 0 || b.At >= len(cs) {
		return nil, ErrNotEditable
	}
	c := cs[b.At]
	after := r.WithBound(b, v)
	if after.Expr == r.Expr {
		return nil, ErrNotEditable
	}
	n := after.Expr[c.at : len(after.Expr)-(len(r.Expr)-c.end)]
	for k := 0; k <= 16 && k <= c.at; k++ {
		before := r.Expr[c.at-k : c.at]
		e.From, e.To = before+r.Expr[c.at:c.end], before+n
		out, err := EditTree(data, e)
		if err == nil {
			return out, nil
		}
		if !errors.Is(err, ErrNotEditable) {
			return nil, err
		}
	}
	return nil, ErrNotEditable
}

// ISODuration writes a duration as ISO 8601, as Azure writes them: PT10M,
// PT1H30M, P1D.
func ISODuration(d time.Duration) string {
	if d <= 0 {
		return "PT0S"
	}
	var b strings.Builder
	b.WriteString("P")
	if days := d / (24 * time.Hour); days > 0 {
		fmt.Fprintf(&b, "%dD", days)
		d -= days * 24 * time.Hour
	}
	if d == 0 {
		return b.String()
	}
	b.WriteString("T")
	if h := d / time.Hour; h > 0 {
		fmt.Fprintf(&b, "%dH", h)
		d -= h * time.Hour
	}
	if m := d / time.Minute; m > 0 {
		fmt.Fprintf(&b, "%dM", m)
		d -= m * time.Minute
	}
	if d > 0 {
		b.WriteString(strconv.FormatFloat(d.Seconds(), 'f', -1, 64) + "S")
	}
	return b.String()
}

// PutBack is the document now with the rule e names put back as it was in
// was: in place of the rule as it is now, or, when it is gone, back where
// it was among the rules as they are now; its group too, when that went
// with it. Every other rule stays as it is now. It is made by lines in a
// YAML rule file, in the tree in an Azure rule group, and read back: now's
// tree with was's rule in that place, and nothing else.
func PutBack(now, was []byte, e Edit) ([]byte, error) {
	if IsAzureGroup(was) {
		return putBackTree(now, was, e)
	}
	out, want, err := putBackLines(now, was, e)
	if err != nil {
		return nil, err
	}
	got, err := decodeAll(out)
	if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		return nil, ErrNotEditable
	}
	if _, err := Parse(out); err != nil {
		return nil, ErrNotEditable
	}
	return out, nil
}

// entryName is what a rule in a group is known by: its alert, or a
// recording rule's record.
func entryName(n *yaml.Node) string {
	for _, k := range []string{"alert", "record"} {
		if v := value(n, k); v != nil {
			return k + ":" + v.Value
		}
	}
	return ""
}

// rootGroups is a rule file's groups: its root, and the list.
func rootGroups(data []byte) (*yaml.Node, *yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil || len(doc.Content) != 1 {
		return nil, nil, ErrNotEditable
	}
	root := doc.Content[0]
	groups := value(root, "groups")
	if groups == nil || groups.Kind != yaml.SequenceNode {
		return nil, nil, ErrNotEditable
	}
	return root, groups, nil
}

// itemLines are the lines of the i'th item of a block list, from its dash
// to its end, and the column its content starts at.
func itemLines(lines []string, list *yaml.Node, i int) ([]string, int) {
	n := list.Content[i]
	return lines[n.Line-1 : endOf(lines, list, i)+1], n.Column
}

// indented is lines moved by delta columns: right when it is more than
// none, left when less, where every line has the room.
func indented(lines []string, delta int) ([]string, bool) {
	out := make([]string, len(lines))
	for i, l := range lines {
		switch {
		case strings.TrimSpace(l) == "":
			out[i] = l
		case delta >= 0:
			out[i] = strings.Repeat(" ", delta) + l
		case leading(l) >= -delta:
			out[i] = l[-delta:]
		default:
			return nil, false
		}
	}
	return out, true
}

// putBackLines puts the rule back by lines, and gives the tree it should
// read as.
func putBackLines(now, was []byte, e Edit) ([]byte, any, error) {
	_, wasGroups, err := rootGroups(was)
	if err != nil {
		return nil, nil, err
	}
	wasLines := strings.SplitAfter(string(was), "\n")
	gi := slices.IndexFunc(wasGroups.Content, func(g *yaml.Node) bool { return groupName(g) == e.Group })
	if gi < 0 {
		return nil, nil, ErrNotEditable
	}
	wasGroup := wasGroups.Content[gi]
	wasRules := value(wasGroup, "rules")
	if wasRules == nil || wasRules.Kind != yaml.SequenceNode || wasRules.Style&yaml.FlowStyle != 0 || wasGroups.Style&yaml.FlowStyle != 0 {
		return nil, nil, ErrNotEditable
	}
	ri := slices.IndexFunc(wasRules.Content, func(r *yaml.Node) bool { return isRule(r, e) })
	if ri < 0 {
		return nil, nil, ErrNotEditable
	}
	wasTree, err := decodeAll(was)
	if err != nil || len(wasTree) != 1 {
		return nil, nil, ErrNotEditable
	}
	nowTree, err := decodeAll(now)
	if err != nil || len(nowTree) != 1 {
		return nil, nil, ErrNotEditable
	}
	wasGroupTree := treeGroups(wasTree[0])[gi]
	wasRuleTree := treeRules(wasGroupTree)[ri]

	_, nowGroups, err := rootGroups(now)
	if err != nil {
		return nil, nil, err
	}
	lines := strings.SplitAfter(string(now), "\n")
	splice := func(at, to int, with []string) []byte {
		out := append(append(append([]string{}, lines[:at]...), with...), lines[to:]...)
		return []byte(strings.Join(out, ""))
	}
	ng := slices.IndexFunc(nowGroups.Content, func(g *yaml.Node) bool { return groupName(g) == e.Group })
	if ng < 0 {
		// The group is gone: put back as it was kept, but holding only this
		// rule, after the group before it that is still there. The group's
		// other rules went since, by a person's hand or another change, and
		// aren't this one's to bring back.
		one, err := alone(was, e)
		if err != nil {
			return nil, nil, err
		}
		_, oneGroups, err := rootGroups(one)
		if err != nil {
			return nil, nil, err
		}
		og := slices.IndexFunc(oneGroups.Content, func(g *yaml.Node) bool { return groupName(g) == e.Group })
		oneTree, err := decodeAll(one)
		if og < 0 || err != nil || len(oneTree) != 1 {
			return nil, nil, ErrNotEditable
		}
		if len(nowGroups.Content) == 0 {
			// No group at all, as a ruler's group that went: the file of
			// that one group, holding that one rule.
			if len(oneGroups.Content) != 1 {
				return nil, nil, ErrNotEditable
			}
			return one, oneTree[0], nil
		}
		if nowGroups.Style&yaml.FlowStyle != 0 {
			return nil, nil, ErrNotEditable
		}
		item, col := itemLines(strings.SplitAfter(string(one), "\n"), oneGroups, og)
		item, ok := indented(item, nowGroups.Content[0].Column-col)
		if !ok {
			return nil, nil, ErrNotEditable
		}
		oneGroupTree := treeGroups(oneTree[0])[og]
		at, k := placeAfter(oneGroups.Content[:og], nowGroups, lines, func(g *yaml.Node) string { return "group:" + groupName(g) })
		want := wantTree(nowTree[0], func(m map[string]any) bool {
			gs := treeGroups(m)
			if gs == nil {
				return false
			}
			m["groups"] = slices.Insert(gs, k, oneGroupTree)
			return true
		})
		return splice(at, at, item), want, nil
	}
	nowGroup := nowGroups.Content[ng]
	nowRules := value(nowGroup, "rules")
	if nowRules == nil || nowRules.Kind != yaml.SequenceNode || nowRules.Style&yaml.FlowStyle != 0 || len(nowRules.Content) == 0 {
		return nil, nil, ErrNotEditable
	}
	item, col := itemLines(wasLines, wasRules, ri)
	item, ok := indented(item, nowRules.Content[0].Column-col)
	if !ok {
		return nil, nil, ErrNotEditable
	}
	if k := slices.IndexFunc(nowRules.Content, func(r *yaml.Node) bool { return isRule(r, e) }); k >= 0 {
		if slices.ContainsFunc(nowRules.Content[k+1:], func(r *yaml.Node) bool { return isRule(r, e) }) {
			return nil, nil, ErrNotEditable // two alike: which is a guess
		}
		if nowRules.Content[k].Style&yaml.FlowStyle != 0 {
			return nil, nil, ErrNotEditable
		}
		want := wantTree(nowTree[0], func(m map[string]any) bool {
			rs := treeRules(treeGroups(m)[ng])
			rs[k] = wasRuleTree
			return true
		})
		return splice(nowRules.Content[k].Line-1, endOf(lines, nowRules, k)+1, item), want, nil
	}
	at, k := placeAfter(wasRules.Content[:ri], nowRules, lines, entryName)
	want := wantTree(nowTree[0], func(m map[string]any) bool {
		g, _ := treeGroups(m)[ng].(map[string]any)
		if g == nil {
			return false
		}
		g["rules"] = slices.Insert(treeRules(g), k, wasRuleTree)
		return true
	})
	return splice(at, at, item), want, nil
}

// alone is a rule file with every other rule of e's group cut from it, the
// comments just above each with it: e's group as it would be put back
// holding e's rule alone.
func alone(was []byte, e Edit) ([]byte, error) {
	for range 100000 {
		_, groups, err := rootGroups(was)
		if err != nil {
			return nil, err
		}
		gi := slices.IndexFunc(groups.Content, func(g *yaml.Node) bool { return groupName(g) == e.Group })
		if gi < 0 {
			return nil, ErrNotEditable
		}
		rs := value(groups.Content[gi], "rules")
		if rs == nil || rs.Kind != yaml.SequenceNode || rs.Style&yaml.FlowStyle != 0 {
			return nil, ErrNotEditable
		}
		k := slices.IndexFunc(rs.Content, func(r *yaml.Node) bool { return !isRule(r, e) })
		if k < 0 {
			return was, nil
		}
		if was, err = cutItem(strings.SplitAfter(string(was), "\n"), rs, k); err != nil {
			return nil, err
		}
	}
	return nil, ErrNotEditable
}

// placeAfter is where an item goes back in list: on the line after the
// nearest of those before it (in was) that list still has, or else before
// list's first and its comments; and its index there.
func placeAfter(before []*yaml.Node, list *yaml.Node, lines []string, name func(*yaml.Node) string) (int, int) {
	for j := len(before) - 1; j >= 0; j-- {
		id := name(before[j])
		if id == "" {
			continue
		}
		if k := slices.IndexFunc(list.Content, func(n *yaml.Node) bool { return name(n) == id }); k >= 0 {
			return endOf(lines, list, k) + 1, k + 1
		}
	}
	// Before the first, and the comments just above it, which are its own.
	first := list.Content[0]
	at := first.Line - 1
	for at > 0 && strings.HasPrefix(strings.TrimSpace(lines[at-1]), "#") && leading(lines[at-1]) >= first.Column-3 {
		at--
	}
	return at, 0
}

// wantTree is a tree changed by f; nil when f can't change it.
func wantTree(t any, f func(map[string]any) bool) any {
	m, _ := t.(map[string]any)
	if m == nil || !f(m) {
		return nil
	}
	return m
}

func treeGroups(t any) []any {
	m, _ := t.(map[string]any)
	gs, _ := m["groups"].([]any)
	return gs
}

func treeRules(g any) []any {
	m, _ := g.(map[string]any)
	rs, _ := m["rules"].([]any)
	return rs
}

// putBackTree puts an Azure rule group's rule back in its tree: in place
// of the rule as it is now, or back where it was among the rules as they
// are now.
func putBackTree(now, was []byte, e Edit) ([]byte, error) {
	n, err := decodeJSON(now)
	if err != nil {
		return nil, ErrNotEditable
	}
	w, err := decodeJSON(was)
	if err != nil {
		return nil, ErrNotEditable
	}
	if !strings.EqualFold(str(n["type"]), azureRuleGroup) || str(n["name"]) != e.Group || str(w["name"]) != e.Group {
		return nil, ErrNotEditable
	}
	nowRules, wasRules := azureRules(n), azureRules(w)
	is := func(r any) bool {
		m, _ := r.(map[string]any)
		return m != nil && m["alert"] != nil && str(m["alert"]) == e.Name
	}
	ri := slices.IndexFunc(wasRules, is)
	if ri < 0 || nowRules == nil {
		return nil, ErrNotEditable
	}
	name := func(r any) string {
		m, _ := r.(map[string]any)
		for _, k := range []string{"alert", "record"} {
			if m != nil && m[k] != nil {
				return k + ":" + str(m[k])
			}
		}
		return ""
	}
	if k := slices.IndexFunc(nowRules, is); k >= 0 {
		if slices.ContainsFunc(nowRules[k+1:], is) {
			return nil, ErrNotEditable
		}
		nowRules[k] = wasRules[ri]
	} else {
		at := 0
		for j := ri - 1; j >= 0 && at == 0; j-- {
			if id := name(wasRules[j]); id != "" {
				if k := slices.IndexFunc(nowRules, func(r any) bool { return name(r) == id }); k >= 0 {
					at = k + 1
				}
			}
		}
		nowRules = slices.Insert(nowRules, at, wasRules[ri])
	}
	n["properties"].(map[string]any)["rules"] = nowRules
	out, err := canon(n)
	if err != nil {
		return nil, err
	}
	if _, err := Parse(out); err != nil {
		return nil, ErrNotEditable
	}
	return out, nil
}

func azureRules(m map[string]any) []any {
	props, _ := m["properties"].(map[string]any)
	rs, _ := props["rules"].([]any)
	return rs
}

// Shape is what a ruler's unit holds, but for how it is written: each
// alert rule's whole entry, in its group, every key of it (an Azure
// rule's actions and resolveConfiguration, an annotation no check reads),
// its durations as lengths of time, so 90s and 1m30s are one; and the
// rest of its tree with the alert rules taken out (and, for an Azure rule
// group, what Azure sets itself). Two units of one shape hold the same
// rules: a unit read back is compared with what was written so, as a
// ruler writes a group out again in its own way.
func Shape(raw []byte, azureSets []string) (string, error) {
	if _, err := Parse(raw); err != nil {
		return "", err
	}
	docs, err := decodeAll(raw)
	if err != nil || len(docs) != 1 {
		return "", ErrNotEditable
	}
	rest := plainTree(docs[0])
	m, _ := rest.(map[string]any)
	var alerts []string
	take := func(group string, rs any) any {
		l, ok := rs.([]any)
		if !ok {
			return rs
		}
		return slices.DeleteFunc(l, func(r any) bool {
			rm, _ := r.(map[string]any)
			if _, alert := rm["alert"]; !alert {
				return false
			}
			b, _ := json.Marshal(normEntry(rm))
			alerts = append(alerts, group+"\x00"+string(b))
			return true
		})
	}
	switch {
	case m == nil:
	case strings.EqualFold(str(m["type"]), azureRuleGroup):
		for _, k := range azureSets {
			delete(m, k)
		}
		if props, _ := m["properties"].(map[string]any); props != nil {
			props["rules"] = take(str(m["name"]), props["rules"])
		}
	default:
		for _, g := range treeGroups(m) {
			if gm, _ := g.(map[string]any); gm != nil {
				gm["rules"] = take(treeGroup(gm), gm["rules"])
			}
		}
	}
	slices.Sort(alerts)
	b, err := json.Marshal(rest)
	if err != nil {
		return "", err
	}
	return strings.Join(alerts, "\n") + "\n" + Hash(b), nil
}

// stripRule is a unit's tree with the rule e names taken out, and a rule
// file's group with it, when that leaves it empty.
func stripRule(t any, e Edit) any {
	m, _ := t.(map[string]any)
	if m == nil {
		return t
	}
	is := func(r any) bool {
		rm, _ := r.(map[string]any)
		return rm != nil && rm["alert"] != nil && str(rm["alert"]) == e.Name
	}
	if strings.EqualFold(str(m["type"]), azureRuleGroup) {
		if props, _ := m["properties"].(map[string]any); props != nil && str(m["name"]) == e.Group {
			if rs, ok := props["rules"].([]any); ok {
				props["rules"] = slices.DeleteFunc(rs, is)
			}
		}
		return m
	}
	gs, ok := m["groups"].([]any)
	if !ok {
		return m
	}
	m["groups"] = slices.DeleteFunc(gs, func(g any) bool {
		gm, _ := g.(map[string]any)
		if gm == nil || treeGroup(gm) != e.Group {
			return false
		}
		rs, ok := gm["rules"].([]any)
		if !ok {
			return false
		}
		gm["rules"] = slices.DeleteFunc(rs, is)
		return len(gm["rules"].([]any)) == 0
	})
	return m
}

// Adds reports whether out is now with the rule e names put in, or put in
// place of the entry it has there, and nothing else: the two alike once
// that rule is taken out of each, and its group when that leaves it
// empty. An undo puts back that one rule, never others with it.
func Adds(now, out []byte, e Edit) bool {
	a, err1 := decodeAll(now)
	b, err2 := decodeAll(out)
	if err1 != nil || err2 != nil || len(a) != 1 || len(b) != 1 {
		return false
	}
	// One rule of its name in place of one, or one more than there was.
	if n, m := entries(a[0], e), entries(b[0], e); m != 1 || n > 1 {
		return false
	}
	return reflect.DeepEqual(stripRule(plainTree(a[0]), e), stripRule(plainTree(b[0]), e))
}

// entries is how many rules of e's name a unit's group holds.
func entries(t any, e Edit) int {
	n := 0
	m, _ := plainTree(t).(map[string]any)
	count := func(rs any) {
		l, _ := rs.([]any)
		for _, r := range l {
			if rm, _ := r.(map[string]any); rm != nil && rm["alert"] != nil && str(rm["alert"]) == e.Name {
				n++
			}
		}
	}
	switch {
	case m == nil:
	case strings.EqualFold(str(m["type"]), azureRuleGroup):
		if props, _ := m["properties"].(map[string]any); props != nil && str(m["name"]) == e.Group {
			count(props["rules"])
		}
	default:
		for _, g := range treeGroups(m) {
			if gm, _ := g.(map[string]any); gm != nil && treeGroup(gm) == e.Group {
				count(gm["rules"])
			}
		}
	}
	return n
}

// normEntry is a rule's entry with its durations as lengths of time
// (a for, keep_firing_for, Azure's timeToResolve), as a ruler writing
// 90s as 1m30s keeps the same rule.
func normEntry(rm map[string]any) map[string]any {
	out := make(map[string]any, len(rm))
	for k, v := range rm {
		out[k] = v
	}
	for _, k := range []string{"for", "keep_firing_for"} {
		if v, ok := out[k]; ok {
			if d, err := isoDuration(v); err == nil {
				out[k] = int64(d)
			}
		}
	}
	if rc, ok := out["resolveConfiguration"].(map[string]any); ok {
		c := make(map[string]any, len(rc))
		for k, v := range rc {
			c[k] = v
		}
		if d, err := isoDuration(c["timeToResolve"]); err == nil && c["timeToResolve"] != nil {
			c["timeToResolve"] = int64(d)
		}
		out["resolveConfiguration"] = c
	}
	return out
}

// Entry is the rule e names as a unit holds it, every key of it, its
// durations as lengths of time; ok is false when the unit holds none, or
// more than one.
func Entry(raw []byte, e Edit) (map[string]any, bool) {
	docs, err := decodeAll(raw)
	if err != nil || len(docs) != 1 {
		return nil, false
	}
	m, _ := plainTree(docs[0]).(map[string]any)
	var found []map[string]any
	look := func(rs any) {
		l, _ := rs.([]any)
		for _, r := range l {
			if rm, _ := r.(map[string]any); rm != nil && rm["alert"] != nil && str(rm["alert"]) == e.Name {
				found = append(found, rm)
			}
		}
	}
	switch {
	case m == nil:
	case strings.EqualFold(str(m["type"]), azureRuleGroup):
		if props, _ := m["properties"].(map[string]any); props != nil && str(m["name"]) == e.Group {
			look(props["rules"])
		}
	default:
		for _, g := range treeGroups(m) {
			if gm, _ := g.(map[string]any); gm != nil && treeGroup(gm) == e.Group {
				look(gm["rules"])
			}
		}
	}
	if len(found) != 1 {
		return nil, false
	}
	return normEntry(found[0]), true
}
