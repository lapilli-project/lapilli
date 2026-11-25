package serve

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/graph"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// The service graph on the Settings page, under the incident criteria
// (docs/design/dependencies.md, "In lapilli serve"): --depends, its labels,
// depth and direction, an admin's to change and to try on the last four
// weeks first, as --impact is: the catches it would add, by rule, and how
// the verdict would move. Saved, the replica that leads reads the graph over
// those weeks (dependsAgain) and the ledger is judged again.

// triedDependsRules is how many rules a try names at most.
const triedDependsRules = 20

// dependsView is the service graph as the Settings page shows it: each
// setting, the form's values (as typed when tried, else in effect), whether
// it is on, the label of the match whose values are its services, how much of
// it the ledger holds, whether it waits to be read, why a read failed, and a
// try's result.
type dependsView struct {
	NoHistory bool
	Settings  []settingView
	Expr      string
	Labels    string
	Depth     string
	Direction string
	On        bool
	// Label is the label of the match in effect whose values are the graph's
	// services; "" when the graph is on but the match has none of them.
	Label string
	// Edges is how many edges the ledger holds, counting each for every hour
	// it held, and Since and Until the span they were read over.
	Edges        int64
	Since, Until time.Time
	// Waiting says the graph in effect wasn't read yet: at the next round of
	// the replica that leads it is read over the last four weeks.
	Waiting bool
	// Err is why the graph couldn't be read, as the ledger keeps it (the
	// leader's, seen on every replica); Next, when a read over the last four
	// weeks that failed is tried again, after Tries tries.
	Err   string
	Next  time.Time
	Tries int
	// Holes are the stretches inside the span read that no read covers, Failed
	// the spans the last run couldn't read, to be read once more, and Gaps
	// those that failed twice: known to be missing, and not read again. At
	// most graphHolesShown of each, More the others.
	Holes, Failed, Gaps [][2]time.Time
	More                int
	// Over says the last judgement of the span read was made with no graph,
	// for its incidents' windows had more edges than are loaded.
	Over  bool
	Tried *dependsTry
}

// graphHolesShown is how many holes, failed spans and gaps the Settings page
// lists of each.
const graphHolesShown = 10

// dependsTry is a try's result: the period, the graph as typed, why it
// couldn't be tried (Err, a text in the reader's language), how many pairs of
// services it has (Edges), the rules that would catch incidents by it and
// how many each (Rules, the first triedDependsRules of More more), and the
// verdict in effect and with it. Same says it is the graph the ledger holds:
// nothing would change.
type dependsTry struct {
	From, To  time.Time
	Expr      string
	Labels    string
	Depth     string
	Direction string
	Off       bool
	Same      bool
	Err       string
	Edges     int
	Added     int
	Rules     []triedRule
	More      int
	Rows      []triedRow
}

// triedRule is a rule that would catch incidents by dependency: how many
// incidents it would catch that it doesn't now.
type triedRule struct {
	Rule      string
	Incidents int
	Path      string
	Hops      []string
}

// dependsViewOf is the service graph as it is in effect.
func (s *Server) dependsViewOf(ctx context.Context, set map[string]ledger.Setting) (*dependsView, error) {
	c := s.dependsOf(set)
	v := &dependsView{NoHistory: s.h == nil, Depth: strconv.Itoa(c.Depth), Direction: c.Direction, On: c.on()}
	v.Expr = c.Expr
	if c.From != "" {
		v.Labels = c.From + "," + c.To
	}
	started := s.cfg.Depends
	show := map[string]struct{ start, now string }{
		"depends":           {orOff(started.Expr), orOff(c.Expr)},
		"depends-labels":    {dependsLabelsText(started.From, started.To), v.Labels},
		"depends-depth":     {strconv.Itoa(max(started.Depth, verdict.DependsDefaultDepth)), v.Depth},
		"depends-direction": {orDown(started.Direction), c.Direction},
	}
	for _, st := range dependsSettable {
		x := settingView{Key: st.key, Flag: st.flag, CommandLine: show[st.key].start, Value: show[st.key].now}
		if ch, ok := set[st.key]; ok {
			x.Changed, x.By, x.Updated = true, ch.By, ch.Updated
		}
		v.Settings = append(v.Settings, x)
	}
	if !c.on() {
		return v, nil
	}
	v.Label = c.labelOf(s.matchOf(set))
	held, err := s.l.EdgesKey(ctx)
	if err != nil {
		return nil, err
	}
	v.Waiting = held != c.graphKey()
	var spans [][2]time.Time
	if v.Edges, spans, err = s.l.EdgesKept(ctx); err != nil {
		return nil, err
	}
	if len(spans) > 0 {
		v.Since, v.Until = spans[0][0], spans[len(spans)-1][1]
	}
	st, err := s.l.EdgesStatus(ctx)
	if err != nil {
		return nil, err
	}
	if st.Key == "" || st.Key == c.graphKey() {
		v.Err, v.Next, v.Tries = st.Error, st.Next, st.Tries
	}
	for i := 1; i < len(spans); i++ {
		v.Holes = append(v.Holes, [2]time.Time{spans[i-1][1], spans[i][0]})
	}
	if v.Failed, err = s.l.EdgeRetries(ctx); err != nil {
		return nil, err
	}
	if v.Gaps, err = s.l.EdgeGaps(ctx); err != nil {
		return nil, err
	}
	shown := func(x [][2]time.Time) [][2]time.Time {
		if len(x) > graphHolesShown {
			v.More += len(x) - graphHolesShown
			return x[:graphHolesShown]
		}
		return x
	}
	v.Holes, v.Failed, v.Gaps = shown(v.Holes), shown(v.Failed), shown(v.Gaps)
	if !v.Since.IsZero() {
		if v.Over, err = s.l.EdgesOver(ctx, v.Since, v.Until); err != nil {
			return nil, err
		}
	}
	return v, nil
}

func orOff(expr string) string {
	if expr == "" {
		return impactOff
	}
	return expr
}

func orDown(d string) string {
	if d == "" {
		return verdict.DependsDown
	}
	return d
}

func dependsLabelsText(from, to string) string {
	if from == "" || to == "" {
		return ""
	}
	return from + "," + to
}

// errDependsNeedsMatch is a graph asked for with no label of the match to
// name its services by.
var errDependsNeedsMatch = bad("the service graph's services are values of a label of the match, and the match in effect has none (--match, or --depends-match)")

// errDependsNeedsLabels is an expression with no labels to name its callers
// and callees by.
var errDependsNeedsLabels = bad("the service graph's series each name a caller and a callee: give --depends-labels, the label of the caller and the label of the callee, with the expression")

// errDependsLabels is --depends-labels not two different labels.
var errDependsLabels = bad("--depends-labels: want FROM,TO, two different labels, the caller's then the callee's")

// dependsForm is what the form says of the graph, checked: as the Settings
// page keeps it ("" for what is as it was started with: nothing to keep).
type dependsForm struct {
	expr, labels, depth, direction string
}

// readDependsForm reads and checks the graph's fields of a form or a try's
// query, each as the page would keep it.
func (s *Server) readDependsForm(get func(string) string) (dependsForm, error) {
	var f dependsForm
	f.expr = impactText(get("depends"))
	if f.expr != "" && f.expr != impactOff {
		if err := checkImpact(f.expr); err != nil {
			return f, err
		}
	}
	f.labels = strings.TrimSpace(get("depends-labels"))
	if f.labels != "" {
		if _, _, ok := splitDependsLabels(f.labels); !ok {
			return f, errDependsLabels
		}
	}
	f.depth = strings.TrimSpace(get("depends-depth"))
	if f.depth != "" {
		if n, err := strconv.Atoi(f.depth); err != nil || n < 1 || n > graph.MaxDepth {
			return f, bad("--depends-depth %s: want a number of calls, 1 to %d", f.depth, graph.MaxDepth)
		}
	}
	f.direction = strings.TrimSpace(get("depends-direction"))
	if f.direction != "" && f.direction != verdict.DependsDown && f.direction != verdict.DependsBoth {
		return f, bad("--depends-direction %s: want down or both", f.direction)
	}
	return f, nil
}

// dependsSettings is the settings the form would leave: a value the command
// line has is kept as none, and so is one given back to it (reset).
func (s *Server) dependsSettings(f dependsForm, reset map[string]bool) map[string]string {
	d := s.cfg.Depends
	out := map[string]string{}
	put := func(key, v, started string) {
		switch {
		case reset[key] || v == "" || v == started:
			out[key] = ""
		default:
			out[key] = v
		}
	}
	switch {
	case f.expr == "" || f.expr == impactOff:
		out["depends"] = impactOff
		if d.Expr == "" {
			out["depends"] = ""
		}
	default:
		put("depends", f.expr, impactText(d.Expr))
	}
	if reset["depends"] {
		out["depends"] = ""
	}
	put("depends-labels", f.labels, dependsLabelsText(d.From, d.To))
	put("depends-depth", f.depth, strconv.Itoa(max(d.Depth, verdict.DependsDefaultDepth)))
	put("depends-direction", f.direction, orDown(d.Direction))
	return out
}

// postDepends saves the service graph, an admin's: an expression that reads,
// or empty (or off) for none, its labels, depth and direction. One that
// would be read with another expression, labels or step than the one in
// effect is read in full over the last four weeks first, as a try reads it,
// unless a try did within triedFor: what can't be read within the bounds
// isn't saved. Saved, the replica that leads reads it again
// (dependsAgain), which judges the ledger again.
func (s *Server) postDepends(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	ctx := r.Context()
	f, err := s.readDependsForm(r.PostFormValue)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	set, err := s.l.Settings(ctx)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	reset := map[string]bool{}
	for _, st := range dependsSettable {
		reset[st.key] = r.PostFormValue("reset-"+st.key) != ""
	}
	values := s.dependsSettings(f, reset)
	if f.expr != "" && f.expr != impactOff && s.h == nil {
		s.oops(w, r, errNoHistory)
		return
	}
	after := withSettings(set, values, s.user(r), s.now())
	before, next := s.dependsOf(set), s.dependsOf(after)
	if next.Expr != "" && (next.From == "" || next.To == "") {
		s.oops(w, r, errDependsNeedsLabels)
		return
	}
	if next.on() && next.labelOf(s.matchOf(after)) == "" {
		s.oops(w, r, errDependsNeedsMatch)
		return
	}
	if next.on() && next.graphKey() != before.graphKey() {
		if !s.takeTry(ctx, s.user(r)) {
			s.oops(w, r, errImpactTries)
			return
		}
		if !s.tried.recent("depends "+next.graphKey(), time.Now()) {
			if err := s.readDependsToSave(ctx, next); err != nil {
				s.oops(w, r, err)
				return
			}
		}
	}
	if err := s.l.SetSettings(ctx, values, s.user(r)); err != nil {
		s.oops(w, r, err)
		return
	}
	if before.graphKey() != next.graphKey() || before.Depth != next.Depth || before.Direction != next.Direction || before.Match != next.Match {
		s.poke()
	}
	back(w, r, "/settings#service-graph")
}

// readDependsToSave reads a graph about to be saved over the span a try
// reads, in the heavy turn.
func (s *Server) readDependsToSave(ctx context.Context, c dependsConf) error {
	from, to, err := s.trySpan(ctx)
	if err != nil {
		return err
	}
	done, err := s.busy(ctx)
	if err != nil {
		return err
	}
	defer done()
	if _, err := s.readGraphWithin(ctx, c, from, to); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return bad("the service graph couldn't be read over the last four weeks within --timeout (%s at most) and --max-samples, so nothing was saved: %v",
			verdict.Duration(tryWithin), promText(err))
	}
	s.tried.put("depends "+c.graphKey(), time.Now())
	return nil
}

// readGraphWithin reads the graph over the span a try reads, within
// tryWithin and --max-samples, with the lead before it.
func (s *Server) readGraphWithin(ctx context.Context, c dependsConf, from, to time.Time) (*graph.Graph, error) {
	ctx, cancel := context.WithTimeout(ctx, min(s.cfg.Timeout, tryWithin))
	defer cancel()
	g, err := graph.Read(ctx, s.h, c.query(), from.Add(-max(s.cfg.Options.MaxLead(), time.Hour)), to)
	if err != nil {
		return nil, err
	}
	if g.TooMany() {
		return nil, errDependsTooMany
	}
	return g, nil
}

// tryDepends tries the graph the Settings page's form gives, an admin's,
// from this site, as tryImpact tries a criterion: checked as saving it is,
// then read over the last four weeks, within tryWithin and --max-samples,
// one heavy request at a time and impactTriesAnHour an admin; what it
// couldn't read is said in its result, the form kept as typed. The last
// four weeks are then judged with and without it, nothing saved.
func (s *Server) tryDepends(r *http.Request, v *criteriaView) error {
	ctx := r.Context()
	if s.role(r) != admin {
		return s.forbidden(admin)
	}
	if !fromHere(r) {
		return errNotFromHere
	}
	q := r.URL.Query()
	l := lang(r)
	try := &dependsTry{Expr: impactText(q.Get("depends")), Labels: strings.TrimSpace(q.Get("depends-labels")),
		Depth: strings.TrimSpace(q.Get("depends-depth")), Direction: strings.TrimSpace(q.Get("depends-direction"))}
	if v.Depends == nil {
		v.Depends = &dependsView{}
	}
	v.Depends.Tried = try
	v.Depends.Expr, v.Depends.Labels, v.Depends.Depth, v.Depends.Direction = try.Expr, try.Labels, try.Depth, try.Direction
	f, err := s.readDependsForm(q.Get)
	if err != nil {
		try.Err = message(l, err)
		return nil
	}
	set, err := s.l.Settings(ctx)
	if err != nil {
		return err
	}
	tried := s.dependsOf(withSettings(set, s.dependsSettings(f, nil), s.user(r), s.now()))
	if tried.Expr != "" && (tried.From == "" || tried.To == "") {
		try.Err = message(l, errDependsNeedsLabels)
		return nil
	}
	try.Off = !tried.on()
	if !try.Off {
		if s.h == nil {
			try.Err = message(l, errNoHistory)
			return nil
		}
		if tried.labelOf(s.matchOf(set)) == "" {
			try.Err = message(l, errDependsNeedsMatch)
			return nil
		}
		if !s.takeTry(ctx, s.user(r)) {
			return errImpactTries
		}
	}
	if try.From, try.To, err = s.trySpan(ctx); err != nil {
		return err
	}
	if n, err := s.l.CountFirings(ctx, try.From, try.To); err != nil {
		return err
	} else if n > reportFirings {
		return bad(tooManyToTry, n, reportFirings)
	}
	done, err := s.busy(ctx)
	if err != nil {
		return err
	}
	defer done()
	var g *graph.Graph
	if !try.Off {
		if g, err = s.readGraphWithin(ctx, tried, try.From, try.To); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			try.Err = tr(l, "Prometheus couldn't read it within %s and --max-samples: %s", verdict.Duration(min(s.cfg.Timeout, tryWithin)), promText(err))
			return nil
		}
		try.Edges = g.Edges()
		s.tried.put("depends "+tried.graphKey(), time.Now())
	}
	return s.judgeDependsTried(ctx, l, try, tried, g, set)
}

// judgeDependsTried judges the try's period with the graph in effect (the
// ledger's) and with the one tried: the catches by dependency it would add,
// by rule, and the verdict in each. Tried, the graph the ledger holds, read
// already, it changes nothing.
func (s *Server) judgeDependsTried(ctx context.Context, l string, try *dependsTry, c dependsConf, g *graph.Graph, set map[string]ledger.Setting) error {
	o, err := s.options(ctx)
	if err != nil {
		return err
	}
	held, err := s.l.EdgesKey(ctx)
	if err != nil {
		return err
	}
	from, to := try.From, try.To
	o.From, o.To = from, to
	try.Same = !try.Off && held == c.graphKey() && o.Depends != nil && o.Depends.Depth == c.Depth && o.Depends.Direction == c.Direction &&
		o.Depends.Label == c.labelOf(o.Match)
	wf, wt := s.l.UnitsOf(o, from, to)
	around, err := s.l.RecordIncidents(ctx, verdict.RecordFrom(wf, wt), wt, o.UnknownLen)
	if err != nil {
		return err
	}
	incs := make([]incident.Incident, 0, len(around))
	for _, in := range around {
		incs = append(incs, in.Incident)
	}
	now := o
	if now, err = s.l.WithGraph(ctx, now, incs); err != nil {
		return err
	}
	with := o
	with.Depends = nil
	if !try.Off {
		with.Depends = &verdict.Dependencies{Graph: g, Label: c.labelOf(o.Match), Depth: c.Depth, Direction: c.Direction}
	}
	if try.Same {
		with = now
	}
	fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: from, To: to})
	if err != nil {
		return err
	}
	plain := make([]firing.Firing, 0, len(fs))
	live := make([]ledger.Firing, 0, len(fs))
	for _, f := range fs {
		if !f.Excused {
			plain = append(plain, f.Firing)
			live = append(live, f)
		}
	}
	if rsp, err := s.previewResponses(ctx, live, o); err != nil {
		return err
	} else if rsp != nil {
		now.Responder, with.Responder = rsp, rsp
	}
	said, err := s.l.ReplayVerdicts(ctx, from, to)
	if err != nil {
		return err
	}
	now.Verdicts, with.Verdicts = said, said
	nowRep := verdict.Judge(nil, plain, incs, now)
	withRep := verdict.Judge(nil, plain, incs, with)
	units := s.l.SpanUnits(o, from, to)
	a := triedSumOf(nowRep, verdict.RecordOf(incs, now), now, units, s.l.Midnight, from, to)
	b := triedSumOf(withRep, verdict.RecordOf(incs, with), with, units, s.l.Midnight, from, to)
	row := func(label, x, y string) {
		try.Rows = append(try.Rows, triedRow{Label: label, Now: x, With: y, Changed: x != y})
	}
	of := func(n, m int) string { return tr(l, "%d of %d", n, m) }
	row("Firings judged", of(a.judged, a.firings), of(b.judged, b.firings))
	row("Precision", pct(a.white, a.judged), pct(b.white, b.judged))
	row("Noisy rules", fmt.Sprint(a.noisy), fmt.Sprint(b.noisy))
	row("Incidents missed", fmt.Sprint(a.missed), fmt.Sprint(b.missed))
	// What it adds: the rules that catch an incident by dependency with the
	// graph tried and don't without it, by how many incidents each.
	caught := func(rep *verdict.Report) map[[2]string]bool {
		out := map[[2]string]bool{}
		for _, ir := range rep.Incidents {
			if ir.Start.Before(from) || !ir.Start.Before(to) {
				continue
			}
			for _, c := range ir.CaughtBy {
				out[[2]string{ir.ID, c.Rule}] = true
			}
		}
		return out
	}
	was := caught(nowRep)
	byRule := map[string]*triedRule{}
	for _, ir := range withRep.Incidents {
		if ir.Start.Before(from) || !ir.Start.Before(to) {
			continue
		}
		for _, c := range ir.CaughtBy {
			if was[[2]string{ir.ID, c.Rule}] || c.Dependency == nil {
				continue
			}
			x := byRule[c.Rule]
			if x == nil {
				x = &triedRule{Rule: c.Rule, Path: verdict.DependsText(c.Dependency), Hops: c.Dependency}
				byRule[c.Rule] = x
			}
			x.Incidents++
			try.Added++
		}
	}
	rules := make([]triedRule, 0, len(byRule))
	for _, x := range byRule {
		rules = append(rules, *x)
	}
	slices.SortFunc(rules, func(x, y triedRule) int {
		if x.Incidents != y.Incidents {
			return y.Incidents - x.Incidents
		}
		return strings.Compare(x.Rule, y.Rule)
	})
	if len(rules) > triedDependsRules {
		try.More = len(rules) - triedDependsRules
		rules = rules[:triedDependsRules]
	}
	try.Rules = rules
	return nil
}
