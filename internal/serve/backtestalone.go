package serve

import (
	"context"
	"maps"
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
	if a, ok := h.answers[caughtAt{in.ID, in.Start.Unix()}]; ok {
		return a.caught, a.known
	}
	return h.rec.catches(in)
}

// holdPrepared reads the record of the rules a backtest's change leaves
// alone, and, where it can't settle an incident the rules that differ lost,
// replays them (replayLeftAlone). nil when nothing was replayed after the
// change: no incident caught before is missed.
func (s *Server) holdPrepared(ctx context.Context, cmp *verdict.Comparison, pr *prepared, from, to time.Time, o verdict.Options) (*leftAloneHeld, error) {
	if cmp.Before == nil || cmp.After == nil {
		return nil, nil
	}
	rec, err := s.leftAloneRecord(ctx, pr, from, to, o)
	if err != nil {
		return nil, err
	}
	held := &leftAloneHeld{rec: rec, answers: map[caughtAt]aloneAnswer{}}
	// A rule after the change that couldn't be replayed leaves what no rule
	// catches not known, whatever the rules left alone say: nothing is
	// replayed for it.
	if cmp.AfterUnreplayed() {
		return held, nil
	}
	var open []aloneCase
	for _, in := range cmp.LostIncidents() {
		if caught, known, need := rec.judge(in); !caught && !known {
			open = append(open, aloneCase{in: in, need: need})
		}
	}
	if len(open) == 0 {
		return held, nil
	}
	if err := s.replayLeftAlone(ctx, held, pr, open, from, to, o); err != nil {
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
// together, sharing their reads.
type aloneBucket struct {
	from, to time.Time
	rules    []rules.Rule
	cases    [][]int // of each rule, the cases it is asked for
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
//   - one that couldn't be replayed: not known.
//
// It is bounded by what the backtest is: the requests it plans may not pass
// what the lane's share of the rate asks within --backtest-timeout
// (leftAloneRequests with no rate), it runs within that time, and a rule
// whose answers pass --max-samples can't be replayed. The incidents past
// the plan stay not known, said so (held.limited), as do those a rule
// couldn't finish.
func (s *Server) replayLeftAlone(ctx context.Context, held *leftAloneHeld, pr *prepared, open []aloneCase, from, to time.Time, o verdict.Options) error {
	rec, p := held.rec, s.btPort
	slices.SortStableFunc(open, func(a, b aloneCase) int { return a.in.Start.Compare(b.in.Start) })
	step := s.cfg.Step
	// The Lokis the replay asks keep the lane's share while it runs, as the
	// backtest's own do.
	asking := p.pacesOf(pr.gone, pr.came, rec.replayed())
	p.asking.Store(&asking)
	// What each case asks, by rule: the windows to replay.
	byRule := func(n int) map[int][]aloneWindow {
		ws := map[int][]aloneWindow{}
		for i, c := range open[:n] {
			for j, r := range rec.alone {
				if from, to, ok := s.aloneSpan(rec, r, c.in, step); ok && rec.asks(r, c) {
					ws[j] = append(ws[j], aloneWindow{from: from, to: to, cases: []int{i}})
				}
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
				out[i].rules = append(out[i].rules, rec.alone[j].rule)
				out[i].cases = append(out[i].cases, w.cases)
			}
		}
		return out
	}
	planned := func(n int) int {
		total := 0
		for _, b := range bucketsOf(byRule(n)) {
			total += replay.Plan(p.lane, []replay.Version{{Rules: b.rules}}, b.from, b.to, step).Requests
		}
		return total
	}
	// The most incidents, the earliest first, whose replay fits.
	budget := s.aloneBudget()
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
	for _, c := range open[fit:] {
		held.answers[caughtAt{c.in.ID, c.in.Start.Unix()}] = aloneAnswer{}
		if !c.need.hard {
			held.limited++
		}
	}
	if fit == 0 {
		return nil
	}
	buckets := bucketsOf(byRule(fit))
	rctx, cancel := context.WithTimeout(s.askedFor(ctx), s.backtestTimeout())
	defer cancel()
	// Each bucket's rules replayed together; the buckets a few at a time,
	// each within the lane's cap.
	type unsettled struct{ unfinished, failed bool }
	var (
		mu    sync.Mutex
		fs    []firing.Firing
		ran   []rules.Rule
		bad   = make([]unsettled, fit)
		wg    sync.WaitGroup
		sem   = make(chan struct{}, max(p.parallel, 1))
		named = func(c aloneCase, name string) bool { return c.need.names[name] || rec.same[name] }
	)
	for _, b := range buckets {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			got, fired := replay.RunEach(rctx, p.lane, [][]replay.Version{{{Rules: b.rules}}}, b.from, b.to, step, p.parallel)
			mu.Lock()
			defer mu.Unlock()
			ran = append(ran, got[0]...)
			fs = append(fs, fired[0]...)
			for _, r := range got[0] {
				if r.Unsupported == "" || r.Unsupported == rules.Paused {
					continue
				}
				for i, cases := range b.cases {
					if b.rules[i].Name != r.Name {
						continue
					}
					for _, ci := range cases {
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
	var incs []incident.Incident
	for _, c := range open[:fit] {
		incs = append(incs, c.in)
	}
	o.From, o.To, o.SomeRules = from, to, true
	var err error
	if o.Verdicts, err = s.l.ReplayVerdicts(ctx, from, to); err != nil {
		return err
	}
	rep := verdict.Judge(ran, fs, incs, o)
	caught := map[caughtAt]bool{}
	for _, x := range rep.Incidents {
		if len(x.CaughtBy) > 0 {
			caught[caughtAt{x.ID, x.Start.Unix()}] = true
		}
	}
	for i, c := range open[:fit] {
		k := caughtAt{c.in.ID, c.in.Start.Unix()}
		switch {
		case caught[k]:
			held.answers[k] = aloneAnswer{caught: true, known: true}
		case c.need.hard || bad[i].failed:
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

// aloneBudget is the most requests a replay of the rules left alone may
// plan.
func (s *Server) aloneBudget() int {
	if s.aloneRequests > 0 {
		return s.aloneRequests
	}
	if r := s.btPort.rate; r > 0 {
		return max(1, int(r*s.backtestTimeout().Seconds()))
	}
	return leftAloneRequests
}

// aloneSpan is the span r is replayed over for c: the incident's catch
// window, its lead before its start to its end, and the rule's reach
// before it (its for, its group's interval, and how late its queries read),
// on the step, and no later than the replay's period ends.
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
// it over c's time (held), and, of a rule the pull request leaves alone,
// the record covers it (the runs, none failed, replayable then).
func (a *leftAlone) asks(r aloneRule, c aloneCase) bool {
	if a.held(r.rule.Name, c.in) && (!r.pr || a.runsCover(c.in) && !a.bad[r.rule.Name]) {
		return false
	}
	return true
}

// runsCover says serve's runs covered in's catch window and none that met
// it couldn't replay a rule left alike.
func (a *leftAlone) runsCover(in incident.Incident) bool {
	from, to := in.Start.Add(-a.o.MaxLead()), in.EndOr(a.o.UnknownLen)
	if a.runsFrom.IsZero() || a.runsFrom.After(from) || a.cursor.Before(to) {
		return false
	}
	meets := func(span [2]time.Time) bool { return !span[0].After(to) && span[1].After(from) }
	return !slices.ContainsFunc(a.failed, meets)
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
