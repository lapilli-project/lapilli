package serve

import (
	"context"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// leftAloneRequests is the most requests a backtest's replay of the rules
// left alone may ask of the history, as planned before it asks, where the
// lane has no rate to say it by (--max-rate): with one, it is what the
// lane's share of it asks within --backtest-timeout, as a backtest itself
// is held to (fits).
const leftAloneRequests = 20000

// leftAloneRules is the most rules left alone a replay of them takes: the
// pull request's rules it leaves alike and serve's own it doesn't hold, in
// all. A replay plans a request once however many rules ask it, but each
// rule is read, kept and judged, so that past this many it replays none:
// every incident the record leaves open is not known, counted in
// missed_limited. It is beside backtestRules, the most rules that differ.
const leftAloneRules = 500

// leftAloneHeld is what holding a backtest to the incidents found: serve's
// record of the rules left alone (rec), and, of an incident the record left
// not known where a replay of those rules may settle it, what the replay
// found (answers). limited are those it left not known because it reached
// its limit.
type leftAloneHeld struct {
	rec     *leftAlone
	answers map[caughtAt]aloneAnswer
	limited int
}

// aloneAnswer is verdict.LeftAlone's answer to an incident, as a replay of
// the rules left alone gave it.
type aloneAnswer struct {
	caught, known bool
}

// catches is verdict.LeftAlone: the replay's answer where it settled an
// incident, else the record's.
func (h *leftAloneHeld) catches(in incident.Incident) (caught, known bool) {
	if h.rec == nil {
		return false, false
	}
	if a, ok := h.answers[caughtAt{in.ID, in.Start.Unix()}]; ok {
		return a.caught, a.known
	}
	return h.rec.catches(in)
}

// unfinishedCompared says a rule a backtest's own replay judged was not
// finished before the timeout: the backtest fails, and nothing more is
// replayed for it.
func unfinishedCompared(cmp *verdict.Comparison) bool {
	for _, rc := range cmp.Rules {
		for _, r := range []*verdict.RuleResult{rc.Before, rc.After} {
			if r != nil && r.Unsupported == rules.Unfinished {
				return true
			}
		}
	}
	return false
}

// holdPrepared reads the record of the rules a backtest's change leaves
// alone, and, where it can't settle an incident the rules that differ lost,
// replays them (replayLeftAlone), within left, what is left of the
// backtest's --backtest-timeout. nil when nothing was replayed after the
// change, or the backtest's own replay didn't finish (it fails then): no
// incident caught before is missed. With no incident lost, nothing of the
// record is read.
func (s *Server) holdPrepared(ctx context.Context, cmp *verdict.Comparison, pr *prepared, from, to time.Time, o verdict.Options, left time.Duration) (*leftAloneHeld, error) {
	if cmp.Before == nil || cmp.After == nil || unfinishedCompared(cmp) {
		return nil, nil
	}
	lost := cmp.LostIncidents()
	held := &leftAloneHeld{answers: map[caughtAt]aloneAnswer{}}
	if len(lost) == 0 {
		return held, nil
	}
	rec, err := s.leftAloneRecord(ctx, pr, from, to, o, lost)
	if err != nil {
		return nil, err
	}
	held.rec = rec
	// A rule after the change that couldn't be replayed leaves what no rule
	// catches not known, whatever the rules left alone say: nothing is
	// replayed for it.
	if cmp.AfterUnreplayed() {
		return held, nil
	}
	var open []aloneCase
	for _, in := range lost {
		caught, known, need := rec.judge(in)
		if !caught && !known {
			open = append(open, aloneCase{in: in, need: need})
			continue
		}
		// What the record settled stays so: widen reads again, and may fail
		// or find more versions than it keeps, which must not unsettle it.
		held.answers[caughtAt{in.ID, in.Start.Unix()}] = aloneAnswer{caught: caught, known: known}
	}
	if len(open) == 0 {
		return held, nil
	}
	if err := s.replayLeftAlone(ctx, held, pr, open, from, to, o, left); err != nil {
		return nil, err
	}
	return held, nil
}

// aloneCase is an incident the record leaves not known, and what a replay
// needs to settle it.
type aloneCase struct {
	in   incident.Incident
	need replayNeed
}

// aloneWindow is a span of one rule's replay and the cases it is asked for.
type aloneWindow struct {
	from, to time.Time
	cases    []int
}

// aloneBucket is the rules replayed over one span, which RunEach replays
// together, sharing their reads: by their place in the rules left alone
// (leftAlone.alone), not copies of them.
type aloneBucket struct {
	from, to time.Time
	rules    []int
	cases    [][]int // of each rule, the cases it is asked for
}

// vet holds each rule left alone to what the port holds a rule that
// differs: taken (what the rules in effect read) and checkRule, the same
// functions with the same maxima. One that fails is refused, never
// replayed: an incident that needs it stays not known. A rule that is
// unsupported as read stays as it is, and fails when it is replayed.
func (a *leftAlone) vet(pr *prepared) {
	check := &prepared{reasons: map[string]bool{}}
	for i := range a.alone {
		r := &a.alone[i]
		rule := r.rule
		why := takenAs(rule, pr.eff, true)
		if why == "" {
			why = check.checkRule(&rule)
		}
		if why != "" {
			r.refused = true
			continue
		}
		if rule.Unsupported != r.rule.Unsupported {
			r.rule.Unsupported = rule.Unsupported
		}
	}
}

// replayLeftAlone settles the incidents open by replaying the rules left
// alone, in the backtest's lane (the history it asks, its share of the
// rate, its cap, what it counts): only over each incident's catch window and
// each rule's reach before it, merged where they overlap so that nothing is
// asked twice; every rule the pull request leaves alone, and every rule
// serve runs that it doesn't hold, except those whose record answers for
// them over that time (held), judged as lapilli verdict judges an incident:
//
//   - caught by any of them: caught;
//   - every rule the pull request leaves alone replayed, and every rule of
//     serve's that caught it in the record, none catching it, and nothing
//     else left it unsettled (a catch by a rule whose firings are copied in,
//     a name told apart): missed, as the incident is not known no longer;
//   - one that couldn't be replayed, or that the port's checks of a rule
//     that differs refuse (vet), or a Grafana rule that fires on at the
//     replay's first evaluation, so that where it began isn't known: not
//     known.
//
// It is bounded by what the backtest is: at most leftAloneRules rules
// that are replayed (not those the record holds), past which nothing is;
// the requests it plans, with an allowance for
// the Grafana rules' extra reads (unsure), may not pass what the lane's
// share of the rate asks within left, what is left of --backtest-timeout
// (leftAloneRequests with no rate), with none left nothing is planned; it
// plans and runs within that time and stops when the job cancels it; and a
// rule whose answers pass --max-samples can't be replayed. The incidents
// past the bound stay not known, said so (held.limited), as do those a rule
// couldn't finish.
func (s *Server) replayLeftAlone(ctx context.Context, held *leftAloneHeld, pr *prepared, open []aloneCase, from, to time.Time, o verdict.Options, left time.Duration) error {
	rec, p := held.rec, s.btPort
	slices.SortStableFunc(open, func(a, b aloneCase) int { return a.in.Start.Compare(b.in.Start) })
	step := s.cfg.Step
	limit := func(cs []aloneCase) {
		for _, c := range cs {
			held.answers[caughtAt{c.in.ID, c.in.Start.Unix()}] = aloneAnswer{}
			if !c.need.hard {
				held.limited++
			}
		}
	}
	budget := s.aloneBudget(left)
	if budget <= 0 || rec.replayCount(open) > leftAloneRules {
		limit(open)
		return nil
	}
	rctx, cancel := context.WithTimeout(s.askedFor(ctx), left)
	defer cancel()
	// Past its time, or cancelled: not known, as past any other bound.
	stopped := func() bool { return rctx.Err() != nil }
	if err := s.widen(rctx, rec); err != nil {
		if stopped() {
			limit(open)
			return nil
		}
		return err
	}
	if rec.replayCount(open) > leftAloneRules {
		limit(open)
		return nil
	}
	rec.vet(pr)
	// The Lokis the replay asks keep the lane's share while it runs, as the
	// backtest's own do.
	asking := p.pacesOf(pr.gone, pr.came, rec.replayed())
	p.asking.Store(&asking)
	// What each case asks, by rule, once: the rules the record doesn't
	// answer for over its time. Of a rule refused, only that it must
	// answer: the case is not known, never missed.
	type unsettled struct{ unfinished, failed bool }
	var (
		bad   = make([]unsettled, len(open))
		named = func(c aloneCase, name string) bool { return c.need.names[name] || rec.same[name] }
		asks  = make([][]int, len(open))
		wins  = make([]map[int][2]time.Time, len(open))
	)
	for i, c := range open {
		if stopped() {
			limit(open)
			return nil
		}
		wins[i] = map[int][2]time.Time{}
		for j, r := range rec.alone {
			from, to, ok := s.aloneSpan(rec, r, c.in, step)
			if !ok || !rec.asks(r, c) {
				continue
			}
			if r.refused {
				bad[i].failed = bad[i].failed || named(c, r.rule.Name)
				continue
			}
			asks[i] = append(asks[i], j)
			wins[i][j] = [2]time.Time{from, to}
		}
	}
	byRule := func(n int) map[int][]aloneWindow {
		ws := map[int][]aloneWindow{}
		for i := range open[:n] {
			for _, j := range asks[i] {
				ws[j] = append(ws[j], aloneWindow{from: wins[i][j][0], to: wins[i][j][1], cases: []int{i}})
			}
		}
		for j, w := range ws {
			ws[j] = mergeWindows(w)
		}
		return ws
	}
	bucketsOf := func(ws map[int][]aloneWindow) []aloneBucket {
		type span struct{ from, to time.Time }
		at := map[span]int{}
		var out []aloneBucket
		for _, j := range slices.Sorted(maps.Keys(ws)) {
			for _, w := range ws[j] {
				k := span{w.from, w.to}
				i, ok := at[k]
				if !ok {
					i = len(out)
					at[k] = i
					out = append(out, aloneBucket{from: w.from, to: w.to})
				}
				out[i].rules = append(out[i].rules, j)
				out[i].cases = append(out[i].cases, w.cases)
			}
		}
		return out
	}
	rulesOf := func(b aloneBucket) []rules.Rule {
		out := make([]rules.Rule, len(b.rules))
		for i, j := range b.rules {
			out[i] = rec.alone[j].rule
		}
		return out
	}
	planned := func(n int) int {
		total := 0
		for _, b := range bucketsOf(byRule(n)) {
			if stopped() {
				return math.MaxInt
			}
			pl := replay.Plan(p.lane, []replay.Version{{Rules: rulesOf(b)}}, b.from, b.to, step)
			// A Grafana rule may ask more than Plan counts (replay.Planned
			// says Unsure): an allowance of twice the queries a rule of its
			// kind has at most, which a query of data returning nothing and
			// a classic condition's window each add one to.
			total += pl.Requests + pl.Unsure*2*backtestGrafanaQueries
		}
		return total
	}
	// The most incidents, the earliest first, whose replay fits.
	lo, hi := 0, len(open)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if planned(mid) <= budget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	fit := lo
	limit(open[fit:])
	if fit == 0 {
		return nil
	}
	buckets := bucketsOf(byRule(fit))
	// Each bucket's rules replayed together; the buckets a few at a time,
	// each within the lane's cap. A firing of a Grafana rule already on at a
	// bucket's first evaluation, where it began unknown, is kept apart
	// (cold): it never catches an incident, and one it could have caught is
	// not known.
	var (
		mu   sync.Mutex
		fs   []firing.Firing
		cold []firing.Firing
		ran  []rules.Rule
		wg   sync.WaitGroup
		sem  = make(chan struct{}, max(p.parallel, 1))
	)
	for _, b := range buckets {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			rs := rulesOf(b)
			got, fired := replay.RunEach(rctx, p.lane, [][]replay.Version{{{Rules: rs}}}, b.from, b.to, step, p.parallel)
			// The cases each rule is asked for, by name.
			cases := map[string][][]int{}
			for i, r := range rs {
				cases[r.Name] = append(cases[r.Name], b.cases[i])
			}
			kept, first := splitCold(fired[0], rs, b.from, step)
			mu.Lock()
			defer mu.Unlock()
			ran = append(ran, got[0]...)
			fs = append(fs, kept...)
			cold = append(cold, first...)
			for _, r := range got[0] {
				// A rule that fires nothing (paused, disabled, no query)
				// settles nothing against an incident, as it catches nothing.
				if r.Unsupported == "" || rules.CantFire(r.Unsupported) {
					continue
				}
				for _, cs := range cases[r.Name] {
					for _, ci := range cs {
						if named(open[ci], r.Name) {
							bad[ci].failed = true
							bad[ci].unfinished = bad[ci].unfinished || r.Unsupported == rules.Unfinished || strings.HasPrefix(r.Unsupported, rules.TooWide)
						}
					}
				}
			}
		})
	}
	wg.Wait()
	// Judged as lapilli verdict judges the rules left alone: the same catch
	// rules, the incidents' labels, the settings in effect.
	o.From, o.To, o.SomeRules = from, to, true
	var err error
	if o.Verdicts, err = s.l.ReplayVerdicts(ctx, from, to); err != nil {
		return err
	}
	incs := make([]incident.Incident, fit)
	for i, c := range open[:fit] {
		incs[i] = c.in
	}
	caught, unclear, err := judgeAlone(rctx, ran, fs, cold, incs, o)
	if err != nil {
		if !stopped() {
			return err
		}
		// Past its time while judging: nothing of the replay settled.
		for i := range open[:fit] {
			bad[i].failed, bad[i].unfinished = true, true
		}
		caught, unclear = make([]bool, fit), make([]bool, fit)
	}
	for i, c := range open[:fit] {
		k := caughtAt{c.in.ID, c.in.Start.Unix()}
		switch {
		case caught[i]:
			held.answers[k] = aloneAnswer{caught: true, known: true}
		case c.need.hard || bad[i].failed || unclear[i]:
			held.answers[k] = aloneAnswer{}
			if !c.need.hard && bad[i].unfinished {
				held.limited++
			}
		default:
			held.answers[k] = aloneAnswer{known: true}
		}
	}
	return nil
}

// judgeAlone says of each incident whether a firing of the replay catches it
// (caught), as lapilli verdict judges one, with the catch rule alone:
// verdict.Catches of a firing the period holds and the rule isn't judged by
// nothing, one a person said unrelated to it left out; each incident looks
// until the first catch, and nothing else is worked out of the rules, as
// verdict.Judge does (Report.Credit's work grows with the square of the
// rules that caught an incident). unclear is of an incident none caught
// that a firing on at the replay's first evaluation (cold) was about, as
// it may have been the catch were its start known. It stops when ctx does.
func judgeAlone(ctx context.Context, ran []rules.Rule, fs, cold []firing.Firing, incs []incident.Incident, o verdict.Options) (caught, unclear []bool, err error) {
	forOf := map[string]time.Duration{}
	for _, r := range ran {
		forOf[r.Name] = max(forOf[r.Name], r.For)
	}
	firing.Sort(fs)
	_, people := verdict.Place(o.Verdicts, fs, o.Match, o.VerdictReach(func(rule string) time.Duration { return forOf[rule] }))
	judged := map[string]bool{}
	var cands []int
	for i, f := range fs {
		if !f.Active(o.From, o.To) {
			continue
		}
		nj, ok := judged[f.Rule]
		if !ok {
			nj = o.NotJudged(f.Rule)
			judged[f.Rule] = nj
		}
		if !nj {
			cands = append(cands, i)
		}
	}
	caught, unclear = make([]bool, len(incs)), make([]bool, len(incs))
	for i, in := range incs {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		for _, fi := range cands {
			if !verdict.Catches(fs[fi], in, o) {
				continue
			}
			if said := people[fi]; said != nil && slices.Contains(said.Unrelated, in.ID) {
				continue
			}
			caught[i] = true
			break
		}
		if !caught[i] {
			for _, f := range cold {
				if !o.NotJudged(f.Rule) && verdict.FiringDuring(in, []firing.Firing{f}, o) {
					unclear[i] = true
					break
				}
			}
		}
	}
	return caught, unclear, nil
}

// aloneBudget is the most requests a replay of the rules left alone may
// plan within left: none when no time is left.
func (s *Server) aloneBudget(left time.Duration) int {
	if left <= 0 {
		return 0
	}
	if s.aloneRequests > 0 {
		return s.aloneRequests
	}
	if r := s.btPort.rate; r > 0 {
		return max(1, int(r*left.Seconds()))
	}
	return leftAloneRequests
}

// aloneSpan is the span r is replayed over for c: the incident's catch
// window, its lead before its start to its end, and the rule's reach
// before it (its for and keep_firing_for, its group's interval, and how late
// its queries read; a Grafana rule's alerts are worked out from before that
// by the replay itself, which warms its stint up as it does a rule in
// effect before the period), on the step, and no later than the replay's period ends.
func (s *Server) aloneSpan(a *leftAlone, r aloneRule, in incident.Incident, step time.Duration) (from, to time.Time, ok bool) {
	from = in.Start.Add(-a.o.MaxLead() - r.reach - r.rule.Lag()).Truncate(step)
	to = in.EndOr(a.o.UnknownLen)
	if t := to.Truncate(step); t.Before(to) {
		to = t.Add(step)
	}
	if now := s.now().Truncate(step); to.After(now) {
		to = now
	}
	return from, to, to.After(from)
}

// asks says whether r is replayed for c: not where its record answers for
// it over c's time (held), and the record covers it: the runs covered the
// time, none failed there to replay it, and it wasn't one serve couldn't
// replay the last time, as of a rule serve runs itself as of the pull
// request's rules left alone.
func (a *leftAlone) asks(r aloneRule, c aloneCase) bool {
	if !a.held(r.rule.Name, c.in) {
		return true
	}
	if !a.runsCover(c.in) || a.bad[r.rule.Name] {
		return true
	}
	if !r.pr && a.failedIn(a.failedServe, c.in) {
		return true
	}
	return false
}

// failedIn says one of spans meets in's catch window.
func (a *leftAlone) failedIn(spans [][2]time.Time, in incident.Incident) bool {
	from, to := in.Start.Add(-a.o.MaxLead()), in.EndOr(a.o.UnknownLen)
	return slices.ContainsFunc(spans, func(span [2]time.Time) bool { return !span[0].After(to) && span[1].After(from) })
}

// runsCover says serve's runs covered in's catch window and none that met
// it couldn't replay a rule left alike.
func (a *leftAlone) runsCover(in incident.Incident) bool {
	from, to := in.Start.Add(-a.o.MaxLead()), in.EndOr(a.o.UnknownLen)
	if a.runsFrom.IsZero() || a.runsFrom.After(from) || a.cursor.Before(to) {
		return false
	}
	return !a.failedIn(a.failed, in)
}

// splitCold sets apart, of the firings a replay of rs from from gave, those
// already on at its first evaluation, where they began isn't known (cold).
// Only a Grafana rule's can be: its replay reports a firing on from the
// start at the first evaluation on its group's grid, which is later than
// the first step when its interval is longer than the step; a Prometheus
// rule's state from the start is exact, so one on the whole time began at
// the start, as lapilli verdict has it.
func splitCold(fired []firing.Firing, rs []rules.Rule, from time.Time, step time.Duration) (kept, cold []firing.Firing) {
	until := map[string]time.Duration{}
	for _, r := range rs {
		if r.Graph != nil {
			until[r.Name] = max(until[r.Name], replay.Every(r, step))
		}
	}
	for _, f := range fired {
		if f.Start.Before(from.Add(until[f.Rule])) {
			cold = append(cold, f)
		} else {
			kept = append(kept, f)
		}
	}
	return kept, cold
}

// replayCount is how many rules a replay for open would ask: those the
// pull request leaves alike and, until widen has taken them in, those of
// serve's own, but for the ones the record holds for every incident open
// (asks).
func (a *leftAlone) replayCount(open []aloneCase) int {
	n := 0
	count := func(r aloneRule) {
		if slices.ContainsFunc(open, func(c aloneCase) bool { return a.asks(r, c) }) {
			n++
		}
	}
	for _, r := range a.alone {
		count(r)
	}
	if !a.widened {
		for _, r := range a.serve {
			count(aloneRule{rule: r})
		}
	}
	return n
}

// replayed are the rules a replay of the rules left alone asks.
func (a *leftAlone) replayed() []rules.Rule {
	out := make([]rules.Rule, 0, len(a.alone))
	for _, r := range a.alone {
		out = append(out, r.rule)
	}
	return out
}

// mergeWindows joins the windows that overlap or meet, the cases they are
// for together, the earliest first.
func mergeWindows(ws []aloneWindow) []aloneWindow {
	slices.SortFunc(ws, func(a, b aloneWindow) int { return a.from.Compare(b.from) })
	var out []aloneWindow
	for _, w := range ws {
		if n := len(out); n > 0 && !w.from.After(out[n-1].to) {
			if w.to.After(out[n-1].to) {
				out[n-1].to = w.to
			}
			out[n-1].cases = append(out[n-1].cases, w.cases...)
			continue
		}
		out = append(out, w)
	}
	return out
}
