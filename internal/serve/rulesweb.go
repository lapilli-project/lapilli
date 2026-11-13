package serve

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// The rules list (docs/design/ui.md): each rule's numbers, with badges on
// three axes (how it did, whether its backtest can be trusted, what people
// mean to do with it) and what is under way; facets to pick by, counted;
// the search box; a chip for each filter in effect.

// ruleRow is a rule as the list shows it: its line, and what it is on
// each axis.
type ruleRow struct {
	ruleLine
	// Perf is how it did over the weeks shown: noisy, ok, silent (no
	// firing) or spare (another rule caught what it caught, as soon).
	Perf string
	// Trust are what its stones can't be taken for granted for: differs
	// (replayed otherwise than it really fired), unreplayed, recorded
	// (copied in, its condition unknown).
	Trust []string
	// Has is what is under way: proposal, facts (problems of fact in what
	// it says).
	Has []string
	// Facts is how many problems of fact what it says has.
	Facts int
	// labels are those it fires with, for label:; sev and group, its
	// severity and group, from them and the rules in effect where the
	// ledger hasn't them.
	labels     map[string]string
	sev, group string
}

// facetOption is a value of a facet: Label a text to translate, or, Raw,
// the value as the rules have it (a severity).
type facetOption struct {
	Value, Label string
	Raw          bool
	Count        int
	Checked      bool
}

// facet is a group of options, each a value of Param: OR within it, AND
// across facets.
type facet struct {
	Param, Name string
	Options     []facetOption
}

// chip is a filter in effect, and the same URL without it. Group is a text
// to translate; Value, too, unless Raw.
type chip struct {
	Group, Value string
	Raw          bool
	Href         string
}

type rulesList struct {
	rulesView
	// Rows are the rules shown; Quiet, the rules that didn't fire among
	// those picked, listed below them unless Performance picks them;
	// QuietOlder says some of those fired before the weeks shown.
	Rows       []ruleRow
	Quiet      []ruleRow
	QuietOlder bool
	// Shown and Total are how many rules are shown, of how many there are
	// for the team and the weeks.
	Shown, Total int
	Facets       []facet
	Chips        []chip
	// Q is the search box: its words, labels and groups, and what it
	// didn't understand, Bad.
	Q   string
	Bad []string
	// Hidden are the form's context: team, weeks, sort. Clear is the same
	// page without a filter but the team and the weeks.
	Hidden []field
	Clear  string
	Active bool
	Empty  *emptyView
}

// The facets, in the order the list shows them.
var facetsOf = []struct {
	param, name string
	values      []string
	labels      []string
}{
	{"perf", "Performance", perfValues, []string{"Noisy", "OK", "Silent", "Spare"}},
	{"severity", "Severity", nil, nil},
	{"trust", "Trust", trustValues, []string{"Differs", "Not replayed", "Recorded only"}},
	{"status", "Disposition", statusValues, []string{"keep", "tune", "retire", "Undecided"}},
	{"has", "Progress", hasValues, []string{"Proposed", "Problems to check"}},
}

// values are a row's values in a facet.
func (r ruleRow) values(param string) []string {
	switch param {
	case "perf":
		return []string{r.Perf}
	case "severity":
		return []string{cmpOr(r.sev, "none")}
	case "trust":
		return r.Trust
	case "status":
		return []string{cmpOr(r.Status, "none")}
	case "has":
		return r.Has
	}
	return nil
}

// picks are the values picked in each facet.
type picks map[string][]string

// in reports whether a row is among those picked in a facet: any value
// picked, OR; with none picked, any row, but in Performance, where a rule
// that didn't fire is listed apart unless picked.
func (p picks) in(r ruleRow, param string) bool {
	want := p[param]
	if len(want) == 0 {
		return param != "perf" || r.Perf != "silent"
	}
	return slices.ContainsFunc(r.values(param), func(v string) bool {
		return slices.ContainsFunc(want, func(w string) bool { return strings.EqualFold(v, w) })
	})
}

// but reports whether a row is among those picked in every facet but one.
func (p picks) but(r ruleRow, skip string) bool {
	for _, f := range facetsOf {
		if f.param != skip && !p.in(r, f.param) {
			return false
		}
	}
	return true
}

// textFilter is what the search box asks for: every word in the name,
// every label, every group.
type textFilter struct {
	words  []string
	labels [][2]string
	groups []string
}

func (f textFilter) match(r ruleRow) bool {
	name := strings.ToLower(r.Name)
	for _, w := range f.words {
		if !strings.Contains(name, w) {
			return false
		}
	}
	for _, kv := range f.labels {
		if v, ok := r.labels[kv[0]]; !ok || !strings.EqualFold(v, kv[1]) {
			return false
		}
	}
	for _, g := range f.groups {
		if !strings.EqualFold(r.group, g) {
			return false
		}
	}
	return true
}

// rulesList is the rules page. A search box with filters a facet has is
// sent to the URL that has them as parameters first.
func (s *Server) rulesList(r *http.Request) (any, error) {
	ctx := r.Context()
	c := s.pageContext(r)
	// The URL as it came: without the reader's own team, when it was taken
	// for them, so that the links made from it take it again.
	asked := r.URL.Query()
	if c.Mine {
		asked.Del("team")
	}
	if out, changed := canonical(asked, c.Teams); changed {
		return nil, redirect{"/rules?" + out.Encode()}
	}
	d, err := s.rulesOf(r)
	if err != nil {
		return nil, err
	}
	v := rulesList{rulesView: d.rulesView}
	cursor, err := s.l.Cursor(ctx)
	if err != nil {
		return nil, err
	}
	meta, err := s.ruleMeta(ctx)
	if err != nil {
		return nil, err
	}

	// Every rule there is for the team and the weeks: those that fired,
	// those in effect that never fired in the twelve weeks kept, and those
	// in effect that fired in them before the weeks shown.
	var all []ruleRow
	listed := map[string]bool{}
	for _, l := range d.Lines {
		all = append(all, s.row(l, d, meta))
		listed[l.Name] = true
	}
	inEffect := map[string]bool{}
	for _, n := range d.Silent {
		inEffect[n] = true
	}
	if latest, ok, err := s.l.Latest(ctx); err != nil {
		return nil, err
	} else if ok {
		for _, n := range latest.Names {
			inEffect[n] = true
		}
	}
	quiet := slices.Clone(d.Silent)
	older := map[string]bool{}
	for _, w := range d.t.weeks {
		for id, x := range d.t.by[w] {
			rr := d.t.rules[id]
			if x.Firings+x.Excused > 0 && !listed[rr.Name] && !older[rr.Name] && (inEffect[rr.Name] || rr.Source != "") {
				older[rr.Name] = true
				quiet = append(quiet, rr.Name)
			}
		}
	}
	slices.Sort(quiet)
	for _, name := range slices.Compact(quiet) {
		all = append(all, s.row(s.quietLine(name, d, meta), d, meta))
	}
	v.Total = len(all)

	// What is picked: the facets' parameters, and the search box.
	p := picks{}
	for _, f := range facetsOf {
		for _, x := range asked[f.param] {
			if x = strings.TrimSpace(x); x != "" && (f.values == nil || slices.Contains(f.values, strings.ToLower(x))) {
				if f.values != nil {
					x = strings.ToLower(x)
				}
				if !slices.Contains(p[f.param], x) {
					p[f.param] = append(p[f.param], x)
				}
			}
		}
	}
	var tf textFilter
	var kept []string
	for _, t := range tokens(asked.Get("q")) {
		kept = append(kept, t.String())
		switch {
		case t.Bad != "" || t.Key == "team" || t.Key == "severity" || t.Key == "is" || t.Key == "status" || t.Key == "has":
			// Understood tokens of these keys are parameters by now: what
			// is left wasn't understood.
			v.Bad = append(v.Bad, t.String())
		case t.Key == "label":
			k, val, _ := strings.Cut(t.Value, "=")
			tf.labels = append(tf.labels, [2]string{k, val})
		case t.Key == "group":
			tf.groups = append(tf.groups, t.Value)
		default:
			tf.words = append(tf.words, strings.ToLower(t.Value))
		}
	}
	v.Q = strings.Join(kept, " ")

	// The rows, the quiet ones, and each option's count: how many rules
	// picking it would show, with what the other facets pick.
	counts := map[string]map[string]int{}
	severities := map[string]bool{}
	for _, row := range all {
		severities[cmpOr(row.sev, "none")] = true
		if !tf.match(row) {
			continue
		}
		for _, f := range facetsOf {
			if !p.but(row, f.param) {
				continue
			}
			if counts[f.param] == nil {
				counts[f.param] = map[string]int{}
			}
			for _, x := range row.values(f.param) {
				counts[f.param][strings.ToLower(x)]++
			}
		}
		switch {
		case p.but(row, "") && p.in(row, "perf"):
			v.Rows = append(v.Rows, row)
		case row.Perf == "silent" && len(p["perf"]) == 0 && p.but(row, "perf"):
			v.Quiet = append(v.Quiet, row)
			v.QuietOlder = v.QuietOlder || older[row.Name]
		}
	}
	v.Shown = len(v.Rows)
	for _, f := range facetsOf {
		fc := facet{Param: f.param, Name: f.name}
		values, labels := f.values, f.labels
		if f.param == "severity" {
			for _, x := range p["severity"] {
				severities[x] = true
			}
			values = slices.SortedFunc(maps.Keys(severities), func(a, b string) int {
				if (a == "none") != (b == "none") {
					return map[bool]int{true: 1, false: -1}[a == "none"]
				}
				return strings.Compare(a, b)
			})
		}
		for i, x := range values {
			o := facetOption{Value: x, Count: counts[f.param][strings.ToLower(x)],
				Checked: slices.ContainsFunc(p[f.param], func(w string) bool { return strings.EqualFold(w, x) })}
			switch {
			case labels != nil:
				o.Label = labels[i]
			case x == "none":
				o.Label = "No severity"
			default:
				o.Label, o.Raw = x, true
			}
			fc.Options = append(fc.Options, o)
		}
		v.Facets = append(v.Facets, fc)
	}

	// The chips, each the same URL without its filter; the team's gives all
	// teams. Clear keeps the team, the weeks and the order.
	without := func(change func(url.Values)) string {
		q := url.Values{}
		for k, vs := range asked {
			q[k] = slices.Clone(vs)
		}
		change(q)
		if q.Get("q") == "" {
			q.Del("q")
		}
		return "/rules?" + q.Encode()
	}
	if c.Team != "" {
		v.Chips = append(v.Chips, chip{Group: "Team", Value: c.Team, Raw: true, Href: without(func(q url.Values) { q.Set("team", "") })})
	}
	for _, fc := range v.Facets {
		for _, o := range fc.Options {
			if o.Checked {
				v.Chips = append(v.Chips, chip{Group: fc.Name, Value: o.Label, Raw: o.Raw, Href: without(func(q url.Values) {
					q[fc.Param] = slices.DeleteFunc(q[fc.Param], func(w string) bool { return strings.EqualFold(strings.TrimSpace(w), o.Value) })
				})})
			}
		}
	}
	rest := func(keep func(qword) bool) string {
		var out []string
		for _, t := range tokens(asked.Get("q")) {
			if keep(t) {
				out = append(out, t.String())
			}
		}
		return strings.Join(out, " ")
	}
	if words := rest(func(t qword) bool { return t.Key == "" }); words != "" {
		v.Chips = append(v.Chips, chip{Group: "Name has", Value: words, Raw: true, Href: without(func(q url.Values) {
			q.Set("q", rest(func(t qword) bool { return t.Key != "" }))
		})})
	}
	for i, t := range tokens(asked.Get("q")) {
		if t.Bad != "" || (t.Key != "label" && t.Key != "group") {
			continue
		}
		g := map[string]string{"label": "Label", "group": "Group"}[t.Key]
		v.Chips = append(v.Chips, chip{Group: g, Value: t.Value, Raw: true, Href: without(func(q url.Values) {
			var out []string
			for j, u := range tokens(asked.Get("q")) {
				if j != i {
					out = append(out, u.String())
				}
			}
			q.Set("q", strings.Join(out, " "))
		})})
	}
	v.Active = len(v.Chips) > 0 && !(len(v.Chips) == 1 && c.Team != "")
	v.Clear = c.Link("/rules", "sort", v.Sort)
	v.Hidden = c.hidden()
	if v.Sort != "" {
		v.Hidden = append(v.Hidden, field{"sort", v.Sort})
	}

	switch {
	case cursor.IsZero():
		v.Empty = &emptyView{Kind: "first", Title: "Nothing judged yet",
			Text: "The first run replays the rules over the weeks before it started; this page fills in when it has. It can take a few minutes for a busy set of rules."}
	case v.Shown == 0 && (v.Active || len(v.Bad) > 0):
		v.Empty = &emptyView{Kind: "notfound", Title: "No rule matches these filters.", Links: []emptyLink{{"Clear filters", v.Clear}}}
		if c.Team != "" {
			v.Empty.Links = append(v.Empty.Links, emptyLink{"The same filters, in all teams", without(func(q url.Values) { q.Set("team", "") })})
		}
	case v.Shown == 0:
		v.Empty = &emptyView{Kind: "notfound", Title: "No rule fired in these weeks."}
	}
	return v, nil
}

// row is a rule's line as the list shows it.
func (s *Server) row(l ruleLine, d rulesData, meta map[string]ruleMeta) ruleRow {
	r := ruleRow{ruleLine: l, labels: meta[l.Name].labels, Facts: max(l.Content.Facts, d.Facts[l.Name])}
	switch {
	case l.Firings+l.Excused == 0:
		r.Perf = "silent"
	case l.Noisy:
		r.Perf = "noisy"
	case l.Spare:
		r.Perf = "spare"
	default:
		r.Perf = "ok"
	}
	if l.Differs {
		r.Trust = append(r.Trust, "differs")
	}
	if l.Unsupported != "" {
		r.Trust = append(r.Trust, "unreplayed")
	}
	if l.Source != "" {
		r.Trust = append(r.Trust, "recorded")
	}
	if l.Proposed {
		r.Has = append(r.Has, "proposal")
	}
	if r.Facts > 0 {
		r.Has = append(r.Has, "facts")
	}
	r.sev, r.group = cmpOr(l.Severity, r.labels["severity"]), cmpOr(l.Group, meta[l.Name].group)
	return r
}

// quietLine is the line of a rule that didn't fire in the weeks shown: its
// numbers none, its twelve weeks drawn as they were.
func (s *Server) quietLine(name string, d rulesData, meta map[string]ruleMeta) ruleLine {
	t := d.t
	l := ruleLine{Name: name, Precision: "–", RefiredShare: "–", Without: "–", SomeRules: t.o.SomeRules}
	id := t.id(name)
	if rr, ok := t.rules[id]; ok && id != 0 {
		l.Severity, l.Group, l.Unsupported, l.Source = rr.Severity, rr.Group, rr.Unsupported, rr.Source
	}
	most := 1
	for _, w := range t.weeks {
		most = max(most, t.by[w][id].Firings)
	}
	for i, w := range t.weeks {
		x := t.by[w][id]
		const h = 18.0
		wh, bh := h*float64(x.White)/float64(most), h*float64(x.Black())/float64(most)
		l.Spark = append(l.Spark, spark{X: float64(i) * 6, WhiteY: h - wh, WhiteH: wh, BlackY: h - wh - bh, BlackH: bh,
			Title: fmt.Sprintf("%s: %d ○ %d ●", s.l.Midnight(w).In(s.cfg.Loc).Format("01-02"), x.White, x.Black())})
	}
	l.Severity, l.Group = cmpOr(l.Severity, meta[name].labels["severity"]), cmpOr(l.Group, meta[name].group)
	l.Team, l.Status = d.own.teamOf(name), d.own.notes[name].Status
	l.Proposed = d.proposed[name]
	return l
}

// ruleMeta is what the rules in effect say of a rule: its group, and the
// labels it fires with.
type ruleMeta struct {
	group  string
	labels map[string]string
}

// ruleMeta is what the latest version of the rules says of each, and, of a
// rule recorded elsewhere, the labels all its alerts have.
func (s *Server) ruleMeta(ctx context.Context) (map[string]ruleMeta, error) {
	out := map[string]ruleMeta{}
	shared, err := s.l.SharedLabels(ctx)
	if err != nil {
		return nil, err
	}
	for name, ls := range shared {
		out[name] = ruleMeta{labels: ls}
	}
	if s.replaying() {
		vs, err := s.versions(ctx, s.now(), s.now())
		if err != nil {
			return nil, err
		}
		if len(vs) > 0 {
			for _, r := range vs[len(vs)-1].Rules {
				out[r.Name] = ruleMeta{group: r.Group, labels: r.AlertLabels()}
			}
		}
	}
	return out, nil
}
