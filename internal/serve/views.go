package serve

import (
	"context"
	"fmt"
	"html/template"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// weeks is how many weeks the overview and the rules' sparklines show.
const weeks = 12

func noisy(firings, white int, o verdict.Options) bool {
	n, p := o.NoisyFirings, o.NoisyPrecision
	if n == 0 {
		n = verdict.NoisyFirings
	}
	if p == 0 {
		p = verdict.NoisyPrecision
	}
	return firings > 0 && firings >= n && float64(white)/float64(firings) < p
}

// tallies are each rule's stones in each of the last n weeks.
type tallies struct {
	weeks []int64                          // Mondays, the earliest first
	by    map[int64]map[int64]ledger.Tally // week to rule id to stones
	rules map[int64]ledger.RuleRow
	names map[string]int64 // rule ids by name, made when first needed
	o     verdict.Options
}

func (s *Server) tallies(ctx context.Context, n int) (*tallies, error) {
	o, err := s.options(ctx)
	if err != nil {
		return nil, err
	}
	now := ledger.Week(s.l.Day(s.now()))
	t := &tallies{by: map[int64]map[int64]ledger.Tally{}, o: o}
	for i := n - 1; i >= 0; i-- {
		w := now - int64(7*i)
		t.weeks = append(t.weeks, w)
		t.by[w] = map[int64]ledger.Tally{}
	}
	rows, err := s.l.Weeks(ctx, t.weeks[0], now)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		t.by[r.At][r.Rule] = r
	}
	if t.rules, err = s.l.RuleTable(ctx); err != nil {
		return nil, err
	}
	return t, nil
}

// id is a rule's id, by its name.
func (t *tallies) id(name string) int64 {
	if t.names == nil {
		t.names = map[string]int64{}
		for id, r := range t.rules {
			t.names[r.Name] = id
		}
	}
	return t.names[name]
}

func (t *tallies) noisy(week, rule int64) bool {
	x := t.by[week][rule]
	return noisy(x.Firings, x.White, t.o)
}

// state is whether an incident was caught, caught only by rules noisy that
// week, or missed.
func (t *tallies) state(in ledger.Incident, week int64) string {
	if len(in.CaughtBy) == 0 {
		return "missed"
	}
	for _, c := range in.CaughtBy {
		if !t.noisy(week, t.id(c.Rule)) {
			return "caught"
		}
	}
	return "noisy"
}

type spark struct {
	X, WhiteY, WhiteH, BlackY, BlackH float64
	Title                             string
}

type ruleLine struct {
	Name, Severity, Group                  string
	Firings, White, Black, Refired, Streak int
	Precision                              string
	Noisy                                  bool
	Spark                                  []spark
	Suggestion, Unsupported                string
}

// ruleLines are each rule's stones over the last n weeks, the noisiest
// first, and the rules that never fired.
func (s *Server) ruleLines(ctx context.Context, t *tallies, n int) ([]ruleLine, []string, error) {
	sugg, err := s.suggestions(ctx)
	if err != nil {
		return nil, nil, err
	}
	sum := map[int64]*ruleLine{}
	recent := t.weeks[len(t.weeks)-n:]
	for _, w := range recent {
		for id, x := range t.by[w] {
			l := sum[id]
			if l == nil {
				r := t.rules[id]
				l = &ruleLine{Name: r.Name, Severity: r.Severity, Group: r.Group, Unsupported: r.Unsupported}
				sum[id] = l
			}
			l.Firings += x.Firings
			l.White += x.White
			l.Refired += x.Refired
		}
	}
	var out []ruleLine
	for id, l := range sum {
		l.Black = l.Firings - l.White
		l.Noisy = noisy(l.Firings, l.White, t.o)
		l.Precision = "–"
		if l.Firings > 0 {
			l.Precision = fmt.Sprintf("%.0f%%", 100*float64(l.White)/float64(l.Firings))
		}
		// Weeks running it has been noisy, up to the last whole week.
		for i := len(t.weeks) - 2; i >= 0 && t.noisy(t.weeks[i], id); i-- {
			l.Streak++
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
		l.Suggestion = sugg[l.Name]
		out = append(out, *l)
	}
	sort.Slice(out, func(a, b int) bool {
		x, y := out[a], out[b]
		if x.Black != y.Black {
			return x.Black > y.Black
		}
		if x.Firings != y.Firings {
			return x.Firings > y.Firings
		}
		return x.Name < y.Name
	})
	var silent []string
	if latest, ok, err := s.l.Latest(ctx); err != nil {
		return nil, nil, err
	} else if ok {
		fired := map[string]bool{}
		for _, l := range out {
			fired[l.Name] = true
		}
		for _, name := range latest.Names {
			if name != "" && !fired[name] && !slices.Contains(silent, name) {
				silent = append(silent, name)
			}
		}
		slices.Sort(silent)
	}
	return out, silent, nil
}

// suggestions are the latest week's, by rule.
func (s *Server) suggestions(ctx context.Context) (map[string]string, error) {
	rs, err := s.l.Results(ctx, "suggestions", 1)
	if err != nil || len(rs) == 0 {
		return map[string]string{}, err
	}
	var ss []verdict.Suggestion
	if err := rs[0].Decode(&ss); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, x := range ss {
		out[x.Rule] = fmt.Sprintf(`"for" %s → %s: fired %d → %d, still catching %d`, verdict.Duration(x.For), verdict.Duration(x.NewFor),
			x.Before.Firings, x.After.Firings, x.Incidents)
	}
	return out, nil
}

type rulesView struct {
	Weeks  int
	Lines  []ruleLine
	Silent []string
}

func (s *Server) rulesPage(r *http.Request) (any, error) {
	n := 4
	switch r.URL.Query().Get("weeks") {
	case "1":
		n = 1
	case "12":
		n = 12
	}
	t, err := s.tallies(r.Context(), weeks)
	if err != nil {
		return nil, err
	}
	lines, silent, err := s.ruleLines(r.Context(), t, n)
	if err != nil {
		return nil, err
	}
	return rulesView{n, lines, silent}, nil
}

type change struct {
	Since                   time.Time
	Expr, For, Keep, Labels string
	Gone                    bool
}

type reviewView struct {
	At       time.Time
	From, To time.Time
	Rules    []verdict.RuleChange
	Incs     []verdict.IncidentChange
}

// reviews are the latest change reviews, those naming the rule when one is
// given.
func (s *Server) reviews(ctx context.Context, rule string, limit int) ([]reviewView, error) {
	rs, err := s.l.Results(ctx, "review", 50)
	if err != nil {
		return nil, err
	}
	var out []reviewView
	for _, r := range rs {
		var c verdict.Comparison
		if err := r.Decode(&c); err != nil {
			return nil, err
		}
		if rule != "" && !slices.ContainsFunc(c.Rules, func(rc verdict.RuleChange) bool { return rc.Rule == rule }) {
			continue
		}
		out = append(out, reviewView{r.At, c.From, c.To, c.Rules, c.Incidents})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

type ruleView struct {
	ruleLine
	Changes []change
	Reviews []reviewView
	Caught  []incidentLine
	Latest  []firingLine
	Known   bool
}

func (s *Server) rulePage(r *http.Request) (any, error) {
	ctx := r.Context()
	name := r.PathValue("name")
	t, err := s.tallies(ctx, weeks)
	if err != nil {
		return nil, err
	}
	lines, silent, err := s.ruleLines(ctx, t, weeks)
	if err != nil {
		return nil, err
	}
	v := ruleView{ruleLine: ruleLine{Name: name, Precision: "–"}}
	if i := slices.IndexFunc(lines, func(l ruleLine) bool { return l.Name == name }); i >= 0 {
		v.ruleLine, v.Known = lines[i], true
	}
	v.Known = v.Known || slices.Contains(silent, name)
	if !s.cfg.Alerts {
		// Its versions over the twelve weeks shown.
		vs, err := s.versions(ctx, s.l.Midnight(t.weeks[0]), s.now())
		if err != nil {
			return nil, err
		}
		var last *rules.Rule
		for _, ver := range vs {
			i := slices.IndexFunc(ver.Rules, func(x rules.Rule) bool { return x.Name == name })
			var cur *rules.Rule
			if i >= 0 {
				cur = &ver.Rules[i]
				v.Known = true
				if v.Group == "" {
					v.Group = cur.Group
				}
			}
			switch {
			case cur == nil && last != nil:
				v.Changes = append(v.Changes, change{Since: ver.Since, Gone: true})
			case cur != nil && (last == nil || last.Key() != cur.Key()):
				v.Changes = append(v.Changes, describe(*cur, ver.Since))
			}
			last = cur
		}
		slices.Reverse(v.Changes)
	}
	if !v.Known {
		return nil, ledger.ErrNotFound
	}
	if v.Reviews, err = s.reviews(ctx, name, 10); err != nil {
		return nil, err
	}
	caught, err := s.l.CaughtBy(ctx, name, 20)
	if err != nil {
		return nil, err
	}
	for _, in := range caught {
		v.Caught = append(v.Caught, s.incidentLine(in, t.state(in, ledger.Week(s.l.Day(in.Start)))))
	}
	fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: time.Unix(0, 0), To: s.now().Add(time.Hour), Rule: name, Limit: 50})
	if err != nil {
		return nil, err
	}
	for _, f := range fs {
		v.Latest = append(v.Latest, s.firingLine(f))
	}
	return v, nil
}

func describe(r rules.Rule, since time.Time) change {
	c := change{Since: since, For: verdict.Duration(r.For)}
	if r.KeepFiringFor > 0 {
		c.Keep = verdict.Duration(r.KeepFiringFor)
	}
	switch {
	case r.Graph != nil:
		var qs []string
		for _, ref := range slices.Sorted(maps.Keys(r.Graph.Queries)) {
			qs = append(qs, ref+": "+r.Graph.Queries[ref])
		}
		c.Expr = strings.Join(qs, "\n")
	default:
		c.Expr = r.Expr
	}
	var ls []string
	for _, k := range slices.Sorted(maps.Keys(r.Labels)) {
		ls = append(ls, k+"="+r.Labels[k])
	}
	c.Labels = strings.Join(ls, " ")
	return c
}

type incidentLine struct {
	ledger.Incident
	// Status is caught, noisy (only noisy rules caught it) or missed; or
	// why an impact window doesn't count: covered, confirmed, dismissed.
	Status string
	First  string
	Mine   bool // written in the UI, so it can be changed there
}

func (s *Server) incidentLine(in ledger.Incident, state string) incidentLine {
	l := incidentLine{Incident: in, Status: state, Mine: in.Origin == "ui"}
	if in.Origin == "impact" && !in.Counts {
		switch in.State {
		case "":
			l.Status = "covered"
		default:
			l.Status = in.State
		}
	}
	if len(in.CaughtBy) > 0 {
		c := in.CaughtBy[0]
		l.First = c.Rule + " " + verdict.Delay(c.Delay)
	}
	return l
}

type incidentsView struct {
	Weeks int
	Lines []incidentLine
	Match string
	Now   time.Time
}

func (s *Server) incidentsPage(r *http.Request) (any, error) {
	ctx := r.Context()
	n := 4
	if r.URL.Query().Get("weeks") == "12" {
		n = 12
	}
	t, err := s.tallies(ctx, weeks)
	if err != nil {
		return nil, err
	}
	incs, err := s.l.Incidents(ctx, s.l.Midnight(t.weeks[len(t.weeks)-n]), s.now().Add(24*time.Hour), true)
	if err != nil {
		return nil, err
	}
	v := incidentsView{Weeks: n, Match: strings.Join(t.o.Match, ", "), Now: s.now()}
	for _, in := range incs {
		v.Lines = append(v.Lines, s.incidentLine(in, t.state(in, ledger.Week(s.l.Day(in.Start)))))
	}
	slices.Reverse(v.Lines)
	return v, nil
}

type incidentView struct {
	incidentLine
	Labels string
	Match  string
}

func (s *Server) incidentPage(r *http.Request) (any, error) {
	ctx := r.Context()
	in, err := s.l.IncidentByID(ctx, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	t, err := s.tallies(ctx, weeks)
	if err != nil {
		return nil, err
	}
	return incidentView{s.incidentLine(in, t.state(in, ledger.Week(s.l.Day(in.Start)))), labelText(in.Labels), strings.Join(t.o.Match, ", ")}, nil
}

func labelText(l incident.Labels) string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(l)) {
		for _, v := range l[k] {
			out = append(out, k+"="+v)
		}
	}
	return strings.Join(out, " ")
}

type firingLine struct {
	ledger.Firing
	Labels string
}

func (s *Server) firingLine(f ledger.Firing) firingLine {
	var ls []string
	for _, k := range slices.Sorted(maps.Keys(f.Labels)) {
		ls = append(ls, k+"="+f.Labels[k])
	}
	return firingLine{f, strings.Join(ls, " ")}
}

type firingsView struct {
	Day, Rule  string
	Black      bool
	Lines      []firingLine
	Next       string
	Prev, Then string // the day before and after
}

func (s *Server) firingsPage(r *http.Request) (any, error) {
	q := r.URL.Query()
	day := s.now().In(s.cfg.Loc)
	if v := q.Get("day"); v != "" {
		d, err := time.ParseInLocation("2006-01-02", v, s.cfg.Loc)
		if err != nil {
			return nil, bad("day %q: want a date like 2026-11-02", v)
		}
		day = d
	}
	d := s.l.Day(day)
	v := firingsView{Day: s.l.Midnight(d).In(s.cfg.Loc).Format("2006-01-02"), Rule: q.Get("rule"), Black: q.Get("black") != "",
		Prev: s.l.Midnight(d - 1).In(s.cfg.Loc).Format("2006-01-02"), Then: s.l.Midnight(d + 1).In(s.cfg.Loc).Format("2006-01-02")}
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	const page = 100
	fs, err := s.l.Firings(r.Context(), ledger.FiringQuery{From: s.l.Midnight(d), To: s.l.Midnight(d + 1), Rule: v.Rule, Black: v.Black, Before: before, Limit: page})
	if err != nil {
		return nil, err
	}
	for _, f := range fs {
		v.Lines = append(v.Lines, s.firingLine(f))
	}
	if len(fs) == page {
		nq := url.Values{"day": {v.Day}, "before": {fmt.Sprint(fs[len(fs)-1].ID)}}
		if v.Rule != "" {
			nq.Set("rule", v.Rule)
		}
		if v.Black {
			nq.Set("black", "1")
		}
		v.Next = "/firings?" + nq.Encode()
	}
	return v, nil
}

type settingView struct {
	Key, Flag   string
	CommandLine string
	Changed     bool
	Value       string // in effect
	By          string
	Updated     time.Time
}

type settingsView struct {
	Settings []settingView
	Changes  []ledger.Change
	Try      *tryView
}

func (s *Server) settingsView(ctx context.Context) (settingsView, error) {
	o, err := s.options(ctx)
	if err != nil {
		return settingsView{}, err
	}
	set, err := s.l.Settings(ctx)
	if err != nil {
		return settingsView{}, err
	}
	var v settingsView
	for _, st := range settable {
		x := settingView{Key: st.key, Flag: st.flag, CommandLine: show(s.cfg.Options, st.key), Value: show(o, st.key)}
		if c, ok := set[st.key]; ok {
			x.Changed, x.By, x.Updated = true, c.By, c.Updated
		}
		v.Settings = append(v.Settings, x)
	}
	cs, err := s.l.Changes(ctx, 100)
	if err != nil {
		return v, err
	}
	for _, c := range cs {
		if strings.HasPrefix(c.What, "setting ") && len(v.Changes) < 20 {
			v.Changes = append(v.Changes, c)
		}
	}
	return v, nil
}

func (s *Server) settingsPage(r *http.Request) (any, error) {
	v, err := s.settingsView(r.Context())
	if err != nil {
		return nil, err
	}
	if r.URL.Query().Get("try") == "" {
		return v, nil
	}
	o, err := s.options(r.Context())
	if err != nil {
		return nil, err
	}
	tried := o
	for _, st := range settable {
		if val := strings.TrimSpace(r.URL.Query().Get(st.key)); val != "" {
			if err := apply(&tried, st.key, val); err != nil {
				return nil, bad("%s %s: %v", st.flag, val, err)
			}
		}
	}
	if v.Try, err = s.try(r.Context(), o, tried); err != nil {
		return nil, err
	}
	return v, nil
}

type tryView struct {
	From, To time.Time
	Settings []settingView
	Rules    []verdict.RuleChange
	Incs     []tryIncident
	Same     bool
}

type tryIncident struct {
	ID, Title  string
	Start      time.Time
	Was, Would string
}

// try judges the last seven days with other settings, without saving them,
// and sets the result beside the ledger's.
func (s *Server) try(ctx context.Context, o, tried verdict.Options) (*tryView, error) {
	to := s.now()
	from := to.Add(-7 * 24 * time.Hour)
	o.From, o.To = from, to
	tried.From, tried.To = from, to
	now, err := s.l.Report(ctx, from, to, o, false)
	if err != nil {
		return nil, err
	}
	fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: from, To: to})
	if err != nil {
		return nil, err
	}
	plain := make([]firing.Firing, len(fs))
	for i, f := range fs {
		plain[i] = f.Firing
	}
	incs := make([]incident.Incident, len(now.Incidents))
	for i, in := range now.Incidents {
		incs[i] = in.Incident
	}
	would := verdict.Judge(nil, plain, incs, tried)
	v := &tryView{From: from, To: to}
	for _, st := range settable {
		v.Settings = append(v.Settings, settingView{Key: st.key, Flag: st.flag, Value: show(tried, st.key), CommandLine: show(o, st.key)})
	}
	var names []string
	for _, r := range now.Rules {
		names = append(names, r.Rule)
	}
	cmp := verdict.Compare(now, would, names, 0)
	for _, rc := range cmp.Rules {
		b, a := rc.Before, rc.After
		if b == nil || a == nil || b.White != a.White || b.Noisy != a.Noisy || b.Refired != a.Refired {
			v.Rules = append(v.Rules, rc)
		}
	}
	state := func(ir verdict.IncidentResult, noisyRule map[string]bool) string {
		switch {
		case len(ir.CaughtBy) == 0:
			return "missed"
		case slices.ContainsFunc(ir.CaughtBy, func(c verdict.Catch) bool { return !noisyRule[c.Rule] }):
			return "caught"
		}
		return "noisy only"
	}
	nb, na := map[string]bool{}, map[string]bool{}
	for _, r := range now.Rules {
		nb[r.Rule] = r.Noisy
	}
	for _, r := range would.Rules {
		na[r.Rule] = r.Noisy
	}
	for i, in := range now.Incidents {
		if i >= len(would.Incidents) {
			break
		}
		was, wd := state(in, nb), state(would.Incidents[i], na)
		if was != wd {
			v.Incs = append(v.Incs, tryIncident{in.ID, in.Title, in.Start, was, wd})
		}
	}
	v.Same = len(v.Rules) == 0 && len(v.Incs) == 0
	return v, nil
}

type runsView struct {
	Runs    []ledger.Run
	Changes []ledger.Change
	Cursor  time.Time
	Counts  map[string]int
	Took    time.Duration
}

func (s *Server) runsPage(r *http.Request) (any, error) {
	runs, err := s.l.Runs(r.Context(), 100)
	if err != nil {
		return nil, err
	}
	cs, err := s.l.Changes(r.Context(), 100)
	if err != nil {
		return nil, err
	}
	cursor, err := s.l.Cursor(r.Context())
	if err != nil {
		return nil, err
	}
	s.status.Lock()
	counts := maps.Clone(s.status.runs)
	took := s.status.took
	s.status.Unlock()
	return runsView{runs, cs, cursor, counts, took}, nil
}

var funcs = template.FuncMap{
	"dur":   verdict.Duration,
	"delay": verdict.Delay,
	"path":  url.PathEscape,
	"at": func(loc *time.Location, t time.Time) string {
		if t.IsZero() {
			return "–"
		}
		return t.In(loc).Format("2006-01-02 15:04")
	},
	"day": func(loc *time.Location, t time.Time) string { return t.In(loc).Format("2006-01-02") },
	"input": func(loc *time.Location, t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.In(loc).Format("2006-01-02T15:04")
	},
	"pct": func(r *verdict.RuleResult) string {
		if r == nil {
			return "–"
		}
		p, ok := r.Precision()
		if !ok {
			return "–"
		}
		return fmt.Sprintf("%.0f%%", 100*p)
	},
	"fired": func(r *verdict.RuleResult) int {
		if r == nil {
			return 0
		}
		return r.Firings
	},
	"caught": func(r *verdict.RuleResult) int {
		if r == nil {
			return 0
		}
		return r.White
	},
	"catches": func(cs []verdict.Catch) string {
		var out []string
		for _, c := range cs {
			x := c.Rule + " " + verdict.Delay(c.Delay)
			if c.Noisy {
				x += " (noisy)"
			}
			out = append(out, x)
		}
		if len(out) == 0 {
			return "nothing"
		}
		return strings.Join(out, ", ")
	},
	"nav": func() []struct{ Page, Href, Name string } {
		return []struct{ Page, Href, Name string }{
			{"overview", "/", "Overview"}, {"rules", "/rules", "Rules"}, {"incidents", "/incidents", "Incidents"},
			{"firings", "/firings", "Firings"}, {"settings", "/settings", "Settings"}, {"runs", "/runs", "Runs"},
		}
	},
}
