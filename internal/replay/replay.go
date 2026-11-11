// Package replay runs alert rules against history to find when they would
// have fired.
package replay

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/sse"
)

// History is the part of a Prometheus API replay uses.
type History interface {
	QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error)
}

// Unfinished marks a rule whose replay the deadline cut short.
const Unfinished = "not finished before the timeout"

// Version is a set of rules and when it took effect; it is in effect until
// the next version does. A zero From is from before the period.
type Version struct {
	From  time.Time
	Rules []rules.Rule
}

// Run replays the rules in effect over [from, to], a few at a time, and
// returns those rules and when each would have fired. versions are in order
// of From. A rule whose queries fail gets Unsupported set rather than
// failing the run: one bad expression shouldn't hide the verdict on the
// rest.
//
// When a version takes over, its rules pick up the previous version's
// alerts as the rule's own system does on a reload: Prometheus carries them
// over by group, name and labels, pending or firing; Grafana carries them
// over by uid, but starts afresh when the rule changed in what its
// fingerprint holds (rules.Rule.Fingerprint): not its annotations.
//
// Rules that need the same query over the same day share one request; up to
// parallel rules are replayed at once.
func Run(ctx context.Context, h History, versions []Version, from, to time.Time, step time.Duration, parallel int) ([]rules.Rule, []firing.Firing) {
	ran, fs := RunEach(ctx, h, [][]Version{versions}, from, to, step, parallel)
	return ran[0], fs[0]
}

// RunEach replays each set of versions as Run does, over the same period,
// and the sets share their requests too: to set one set of rules beside
// another.
func RunEach(ctx context.Context, h History, sets [][]Version, from, to time.Time, step time.Duration, parallel int) ([][]rules.Rule, [][]firing.Firing) {
	var runs [][]stint
	var setOf []int
	for i, versions := range sets {
		for _, run := range lineUp(versions, from, to, step) {
			runs = append(runs, run)
			setOf = append(setOf, i)
		}
	}
	ran := make([]rules.Rule, len(runs))
	results := make([][]firing.Firing, len(runs))
	sh := newShared(h)
	askers := make([]*asker, len(runs))
	order := make([]int, len(runs))
	asks := make([]string, len(runs))
	weight := map[string]int{} // requests, by the queries a rule asks for
	for i, run := range runs {
		ran[i] = run[len(run)-1].Rule
		order[i] = i
		var reqs []request
		exprs := map[string]bool{}
		for j, st := range run {
			if st.Unsupported != "" {
				break
			}
			lo, hi := st.span(step)
			for _, r := range queries(st.Rule, lo, hi, step, st.warm(j)) {
				reqs = append(reqs, r)
				exprs[r.expr] = true
			}
		}
		askers[i] = sh.expect(reqs)
		asks[i] = strings.Join(slices.Sorted(maps.Keys(exprs)), "\x00")
		weight[asks[i]] = max(weight[asks[i]], len(reqs))
	}
	// Rules that ask for the same queries go together, so they ask at about
	// the same time and an answer isn't held long; those that ask the most
	// go first, so they don't finish last.
	slices.SortStableFunc(order, func(a, b int) int {
		if wa, wb := weight[asks[a]], weight[asks[b]]; wa != wb {
			return wb - wa
		}
		return strings.Compare(asks[a], asks[b])
	})
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(parallel, 1))
	for _, i := range order {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i], ran[i].Unsupported, ran[i].Failed = replayRun(ctx, askers[i], runs[i], to, step)
			askers[i].done()
		}()
	}
	wg.Wait()
	// A rule paused for part of the period was replayed for the rest; it
	// just didn't fire while paused.
	type named struct {
		set  int
		name string
	}
	live := map[named]bool{}
	for i, r := range ran {
		k := named{setOf[i], r.Name}
		live[k] = live[k] || r.Unsupported == ""
	}
	outRan := make([][]rules.Rule, len(sets))
	outFs := make([][]firing.Firing, len(sets))
	for i := range ran {
		if ran[i].Unsupported == rules.Paused && live[named{setOf[i], ran[i].Name}] {
			ran[i].Unsupported = ""
		}
		outRan[setOf[i]] = append(outRan[setOf[i]], ran[i])
		outFs[setOf[i]] = append(outFs[setOf[i]], results[i]...)
	}
	for _, fs := range outFs {
		firing.Sort(fs)
	}
	return outRan, outFs
}

// WithFor replays a rule over [from, to] with each of several "for"s in
// place of its own, asking for its history once.
func WithFor(ctx context.Context, h History, r rules.Rule, fors []time.Duration, from, to time.Time, step time.Duration) (map[time.Duration][]firing.Firing, error) {
	if len(fors) == 0 || r.Unsupported != "" {
		return nil, nil
	}
	out := map[time.Duration][]firing.Firing{}
	if r.Graph != nil {
		// Grafana's "for" is in its states: each is replayed of its own.
		for _, d := range fors {
			rd := r
			rd.For = d
			series, err := Condition(ctx, h, rd, from, to, step)
			if err != nil {
				return nil, err
			}
			out[d] = fire(rd, series, step, firing.For(0), to)
		}
		return out, nil
	}
	series, err := Condition(ctx, h, r, from.Add(-slices.Max(fors)), to, step)
	if err != nil {
		return nil, err
	}
	for _, d := range fors {
		// As a replay does, an alert pending since before the period fires
		// at its start at the earliest.
		fireAt := func(pending time.Time) time.Time {
			if t := pending.Add(d); t.After(from) {
				return t
			}
			return from
		}
		out[d] = fire(r, series, step, fireAt, to)
	}
	return out, nil
}

// Held is when a rule's condition held over [from, to]: each stretch a label
// set held without a break is a firing with no "for" or keep_firing_for,
// from its first evaluation that held to after its last. One still holding
// at to ends there.
func Held(ctx context.Context, h History, r rules.Rule, from, to time.Time, step time.Duration) ([]firing.Firing, error) {
	if r.Unsupported != "" {
		return nil, nil
	}
	r.For, r.KeepFiringFor = 0, 0
	series, err := Condition(ctx, h, r, from, to, step)
	if err != nil {
		return nil, err
	}
	fs := fire(r, series, step, firing.For(0), to)
	for i := range fs {
		if fs[i].End.IsZero() {
			fs[i].End = to
		}
	}
	return fs, nil
}

// Every is how often a replay at step evaluates a rule: as often as its
// group is when that is a whole number of steps, otherwise every step.
func Every(r rules.Rule, step time.Duration) time.Duration {
	if r.Interval > step && r.Interval%step == 0 {
		return r.Interval
	}
	return step
}

// placed is, for a Prometheus rule whose group's place in its interval is
// known (Phase), how far past the steps' grid its evaluations fall, when a
// replay at step evaluates it as often as its group is: its interval a
// whole number of steps. ok is false otherwise: its evaluations are on the
// steps' grid.
func placed(r rules.Rule, step time.Duration) (off time.Duration, ok bool) {
	if r.Graph != nil || r.Phase <= 0 || r.Interval < step || r.Interval%step != 0 || r.Phase >= r.Interval {
		return 0, false
	}
	return r.Phase % step, true
}

// evaluations are the first and last of a Prometheus rule's evaluations in a
// chunk of the period: the chunk's steps, or each that much later where
// Prometheus places the rule's group in its interval, when that is known,
// and up to to.
func evaluations(r rules.Rule, c [2]time.Time, step time.Duration, to time.Time) (time.Time, time.Time, bool) {
	off, _ := placed(r, step)
	a, b := c[0].Add(off), c[1].Add(off)
	if b.After(to) {
		b = b.Add(-step)
	}
	return a, b, !b.Before(a)
}

// A stint is one version of a rule while it was in effect: evaluated at the
// steps from lo up to, not including, hi.
type stint struct {
	rules.Rule
	lo, hi time.Time
	before bool // in effect before the period, so pending from before it too
}

// warm reports whether a stint, the jth of its run, carries alerts in from
// before it: one in effect before the period, or that follows another of
// its rule. A rule new in the period, or a Grafana rule that changed,
// starts afresh.
func (st stint) warm(j int) bool { return j > 0 || st.before }

// span is what a stint is replayed over: from when it took effect, or, if
// that was before the period, from "for" and keep_firing_for earlier, so
// alerts already pending, or held on, carry in.
func (st stint) span(step time.Duration) (time.Time, time.Time) {
	lo := st.lo
	if st.before {
		lo = lo.Add(-st.For - st.KeepFiringFor)
	}
	return lo, st.hi.Add(-step)
}

// lineUp puts each rule's stints in order, in runs whose alerts carry over
// from one stint to the next. A rule unchanged across versions is one stint.
func lineUp(versions []Version, from, to time.Time, step time.Duration) [][]stint {
	// Versions take over at the first step on or after their From.
	at := func(t time.Time) time.Time {
		if !t.After(from) {
			return from
		}
		return from.Add((t.Sub(from) + step - 1) / step * step)
	}
	var runs [][]stint
	open := map[string]int{} // a rule's identity to its run, while in effect
	for i, v := range versions {
		lo, hi := at(v.From), to.Add(step)
		if i+1 < len(versions) && at(versions[i+1].From).Before(hi) {
			hi = at(versions[i+1].From)
		}
		if !lo.Before(hi) {
			continue // replaced before it took effect
		}
		now := map[string]int{}
		dups := map[string]int{}
		for _, r := range v.Rules {
			id := identity(r, dups)
			j, ok := open[id]
			if _, twice := now[id]; twice {
				ok = false // two rules claiming one uid: keep them apart
			}
			switch {
			case !ok || runs[j][len(runs[j])-1].Fingerprint != r.Fingerprint:
				// new, or a Grafana rule changed in what Grafana starts its
				// alerts afresh on
				runs = append(runs, []stint{{Rule: r, lo: lo, hi: hi, before: v.From.Before(from)}})
				j = len(runs) - 1
			case same(runs[j][len(runs[j])-1].Rule, r):
				runs[j][len(runs[j])-1].hi = hi
			default:
				runs[j] = append(runs[j], stint{Rule: r, lo: lo, hi: hi})
			}
			now[id] = j
		}
		open = now
	}
	return runs
}

// identity is what a reload carries alerts over by: a Grafana rule's uid;
// otherwise the group, name and labels, with duplicates matched in order.
func identity(r rules.Rule, dups map[string]int) string {
	if r.UID != "" {
		return "uid\x00" + r.UID
	}
	k := r.Group + "\x00" + r.Name + "\x00" + key(r.Labels)
	dups[k]++
	return k + "\x00" + strconv.Itoa(dups[k])
}

func same(a, b rules.Rule) bool {
	return a.Expr == b.Expr && a.For == b.For && a.KeepFiringFor == b.KeepFiringFor && a.Interval == b.Interval && a.QueryOffset == b.QueryOffset &&
		a.Digest == b.Digest && a.Unsupported == b.Unsupported
}

// replayRun replays one run of stints and says why it couldn't, if it
// couldn't; and which of its queries outside its condition failed.
func replayRun(ctx context.Context, h History, run []stint, to time.Time, step time.Duration) ([]firing.Firing, string, []string) {
	merged := map[string]*prom.Series{}
	failed := &failedRefs{refs: map[string]bool{}}
	ctx = context.WithValue(ctx, failedKey{}, failed)
	for j, st := range run {
		if st.Unsupported != "" {
			return nil, st.Unsupported, nil
		}
		lo, hi := st.span(step)
		series, err := condition(ctx, h, st.Rule, lo, hi, step, false, st.warm(j))
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, Unfinished, nil
		case err != nil:
			return nil, "query failed: " + err.Error(), nil
		}
		for _, s := range series {
			l := maps.Clone(s.Labels)
			delete(l, "__name__")
			k := key(l)
			if merged[k] == nil {
				merged[k] = &prom.Series{Labels: l}
			}
			merged[k].Samples = append(merged[k].Samples, s.Samples...)
		}
	}
	series := make([]prom.Series, 0, len(merged))
	for _, k := range slices.Sorted(maps.Keys(merged)) {
		series = append(series, *merged[k])
	}
	last := run[len(run)-1]
	fs := fire(last.Rule, series, step, fireAt(run), to)
	if !last.hi.After(to) {
		// Taken away or started afresh: its alerts resolved then, whatever
		// keep_firing_for would have held.
		gone := last.hi.Add(step)
		for i := range fs {
			if fs[i].End.IsZero() || fs[i].End.After(gone) {
				fs[i].End = gone
			}
		}
	}
	return fs, "", failed.sorted()
}

// failedKey is where a replay's context keeps the queries outside a
// Grafana rule's condition that failed.
type failedKey struct{}

type failedRefs struct {
	mu   sync.Mutex
	refs map[string]bool
}

func (f *failedRefs) sorted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.refs) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(f.refs))
}

// noteFailed tells the replay's context that a query of a rule failed.
func noteFailed(ctx context.Context, ref string) {
	if f, ok := ctx.Value(failedKey{}).(*failedRefs); ok {
		f.mu.Lock()
		f.refs[ref] = true
		f.mu.Unlock()
	}
}

// fire works out a rule's firings from the steps its condition held,
// evaluated as often as its group is when that is a whole number of steps:
// a 5m group sees its condition every fifth step, and fires and resolves
// only then.
//
// A Grafana rule's series are already of the evaluations its alerts were
// firing at, Grafana's states with its "for" and keep_firing_for worked
// out: they fire as they are.
func fire(r rules.Rule, series []prom.Series, step time.Duration, fireAt func(time.Time) time.Time, end time.Time) []firing.Firing {
	keep := r.KeepFiringFor
	if r.Graph != nil {
		fireAt, keep = firing.For(0), 0
	}
	every := step
	if Every(r, step) > step && len(series) > 0 && len(series[0].Samples) > 0 {
		every = Every(r, step)
		var phase int64
		series, phase = evaluated(r, series, step, every)
		at := fireAt
		fireAt = func(pending time.Time) time.Time {
			t := at(pending)
			if r := (t.UnixNano() - phase) % int64(every); r != 0 {
				t = t.Add(every - time.Duration(r))
			}
			return t
		}
	}
	return firing.FromSeries(r.Name, r.Labels, series, every, fireAt, keep, end)
}

// evaluated keeps of each series the steps a rule evaluated every every is
// evaluated at, and says where on the grid those fall.
func evaluated(r rules.Rule, series []prom.Series, step, every time.Duration) ([]prom.Series, int64) {
	if every <= step || len(series) == 0 || len(series[0].Samples) == 0 {
		return series, 0
	}
	// Evaluations fall on the steps' own grid: the Unix epoch's, unless a
	// period was given in odd seconds; or where the rule's group is placed,
	// when that is known.
	phase := series[0].Samples[0].T.UnixNano() % int64(step)
	if _, ok := placed(r, step); ok {
		phase = int64(r.Phase)
	}
	on := func(t time.Time) bool { return (t.UnixNano()-phase)%int64(every) == 0 }
	var thin []prom.Series
	for _, s := range series {
		var kept []prom.Sample
		for _, smp := range s.Samples {
			if on(smp.T) {
				kept = append(kept, smp)
			}
		}
		if len(kept) > 0 {
			thin = append(thin, prom.Series{Labels: s.Labels, Samples: kept})
		}
	}
	return thin, phase
}

// Values is, of a rule whose condition compares a value with b, the value at
// each of its evaluations over [from, to] past the furthest round number
// within rules.NearBy of b: those a number that near would hold for. A
// Prometheus rule is asked as it is, with that number, so the history
// returns no more than that; a Grafana rule's value is worked out and the
// rest left out. A bound whose values can't be read has none.
func Values(ctx context.Context, h History, r rules.Rule, b rules.Bound, from, to time.Time, step time.Duration) ([]prom.Series, error) {
	ask, ok := r.ValueOf(b)
	if !ok || ask.Unsupported != "" {
		return nil, nil
	}
	edge := b.Furthest()
	series, err := condition(ctx, h, ask, from, to, step, true, false)
	if err != nil {
		return nil, err
	}
	var out []prom.Series
	for _, s := range series {
		l := maps.Clone(s.Labels)
		delete(l, "__name__")
		smp := slices.DeleteFunc(s.Samples, func(x prom.Sample) bool { return !b.Whole(edge, x.V) })
		if len(smp) > 0 {
			out = append(out, prom.Series{Labels: l, Samples: smp})
		}
	}
	out, _ = evaluated(r, out, step, Every(r, step))
	return out, nil
}

// Fired is when a rule fired over [from, to], with its "for" and
// keep_firing_for, as a replay of it alone over that time fires it: all of
// its firings, a label set that held at from pending from then; and those
// known to have started when they did, without what held at from and what
// followed it within keep_firing_for (firing.After). Firings still on at to
// have no End.
func Fired(ctx context.Context, h History, r rules.Rule, from, to time.Time, step time.Duration) (all, known []firing.Firing, err error) {
	if r.Unsupported != "" {
		return nil, nil, nil
	}
	series, err := Condition(ctx, h, r, from, to, step)
	if err != nil {
		return nil, nil, err
	}
	every := Every(r, step)
	thin, _ := evaluated(r, series, step, every)
	keep := r.KeepFiringFor
	if r.Graph != nil {
		keep = 0
	}
	return fire(r, series, step, firing.For(r.For), to), fire(r, firing.After(thin, from, every, keep), step, firing.For(r.For), to), nil
}

// fireAt is when a label set pending since a time fires: once it has been
// pending for the "for" of the stint in effect, and not before that stint
// took over.
func fireAt(run []stint) func(time.Time) time.Time {
	return func(pending time.Time) time.Time {
		i := 0
		for i+1 < len(run) && !pending.Before(run[i+1].lo) {
			i++
		}
		for ; ; i++ {
			t := pending.Add(run[i].For)
			if t.Before(run[i].lo) {
				t = run[i].lo
			}
			if i+1 == len(run) || t.Before(run[i].hi) {
				return t
			}
		}
	}
}

// chunk is how many steps one request covers: a day at one minute, well
// under the 11,000 points per series Prometheus allows.
const chunk = 1440

// chunks splits [from, to] into requests on a grid that doesn't depend on
// from, so rules that look back different lengths still ask for the same
// days, and can share the answers.
func chunks(from, to time.Time, step time.Duration) [][2]time.Time {
	length := chunk * step
	// The grid runs from the Unix epoch when from is on a step there, as
	// it is unless a period was given in odd seconds; otherwise from from.
	base := time.Unix(0, 0).UTC()
	if from.Sub(base)%step != 0 {
		base = from
	}
	var out [][2]time.Time
	// From the first chunk on, each starts a length after the one before:
	// counting from base each time would overflow a Duration past 2262.
	next := base.Add((from.Sub(base)/length + 1) * length)
	for c := from; !c.After(to); c, next = next, next.Add(length) {
		end := next.Add(-step)
		if end.After(to) {
			end = to
		}
		out = append(out, [2]time.Time{c, end})
	}
	return out
}

// warmup is how far before from a Grafana rule's queries must start for a
// reduce over several steps to see a whole window, as Grafana's query would.
func warmup(g *rules.Graph, step time.Duration) time.Duration {
	var warm time.Duration
	for _, st := range g.Steps {
		if st.Reduce != nil {
			warm = max(warm, (st.Reduce.Window+step-1)/step*step)
		}
	}
	return warm
}

// queries lists what Condition asks for over [from, to], request by
// request.
func queries(r rules.Rule, from, to time.Time, step time.Duration, warm bool) []request {
	var out []request
	add := func(expr string, from time.Time, lag time.Duration) {
		for _, c := range chunks(from, to, step) {
			out = append(out, request{expr, c[0].Add(-lag).Unix(), c[1].Add(-lag).Unix()})
		}
	}
	if r.Graph == nil {
		for _, c := range chunks(from, to, step) {
			if a, b, ok := evaluations(r, c, step, to); ok {
				out = append(out, request{r.Expr, a.Add(-r.Lag()).Unix(), b.Add(-r.Lag()).Unix()})
			}
		}
		return out
	}
	start := grafanaStart(r, from, step, warm)
	for _, ref := range asked(r.Graph, false) {
		expr, _ := r.Graph.Expr(ref, r.AskStep(ref))
		add(expr, start, max(r.Graph.Shift[ref], r.Late))
	}
	return out
}

// grafanaStart is where a Grafana rule's evaluation over a period from
// from starts: with warm, its alerts worked out from before it; and before
// that, as far back again as the widest reduce over several steps needs to
// see a whole window.
func grafanaStart(r rules.Rule, from time.Time, step time.Duration, warm bool) time.Time {
	if warm {
		from = from.Add(-newStates(r, Every(r, step)).warmup() / step * step)
	}
	return from.Add(-warmup(r.Graph, step))
}

// shifted asks h for expr over [start, end] as it stood lag earlier: at
// each step, the history up to lag before it, as a rule sees it when its
// queries are asked that much earlier.
func shifted(ctx context.Context, h History, expr string, start, end time.Time, step, lag time.Duration) ([]prom.Series, error) {
	series, err := h.QueryRange(ctx, expr, start.Add(-lag), end.Add(-lag), step)
	if err != nil || lag == 0 {
		return series, err
	}
	out := make([]prom.Series, len(series))
	for i, s := range series {
		smp := make([]prom.Sample, len(s.Samples))
		for j, x := range s.Samples {
			smp[j] = prom.Sample{T: x.T.Add(lag), V: x.V}
		}
		out[i] = prom.Series{Labels: s.Labels, Samples: smp}
	}
	return out, nil
}

// Condition returns, for each label set, the steps in [from, to] at which the
// rule's condition held. It fetches and evaluates a day at a time, so a long
// period takes no more memory than a day.
//
// A Grafana rule is evaluated as often as its group is, and its alerts
// held as Grafana's state manager holds them: an evaluation at which a
// query returned nothing is NoData, and becomes what the rule's
// noDataState says; an alert whose series has gone stays as it was until
// it has been gone MissingToResolve evaluations. A query error can't be
// told from history, and isn't replayed.
func Condition(ctx context.Context, h History, r rules.Rule, from, to time.Time, step time.Duration) ([]prom.Series, error) {
	return condition(ctx, h, r, from, to, step, false, true)
}

// condition is Condition; with values, it is the value of a Grafana rule's
// condition at each of its evaluations, where that isn't 0 or NaN, with no
// states. Without warm, a Grafana rule's alerts start afresh at from, as
// Grafana starts them when the rule changes.
func condition(ctx context.Context, h History, r rules.Rule, from, to time.Time, step time.Duration, values, warm bool) ([]prom.Series, error) {
	out := map[string]*prom.Series{}
	add := func(k string, l map[string]string, smps ...prom.Sample) {
		s, ok := out[k]
		if !ok {
			s = &prom.Series{Labels: l}
			out[k] = s
		}
		s.Samples = append(s.Samples, smps...)
	}
	result := func() []prom.Series {
		res := make([]prom.Series, 0, len(out))
		for _, k := range slices.Sorted(maps.Keys(out)) {
			res = append(res, *out[k])
		}
		return res
	}
	if r.Graph == nil {
		for _, c := range chunks(from, to, step) {
			a, b, ok := evaluations(r, c, step, to)
			if !ok {
				continue
			}
			part, err := shifted(ctx, h, r.Expr, a, b, step, r.Lag())
			if err != nil {
				return nil, err
			}
			for _, s := range part {
				add(key(s.Labels), s.Labels, s.Samples...)
			}
		}
		return result(), nil
	}
	g := r.Graph
	refs := asked(g, values)
	under := map[string]bool{}
	for _, ref := range queriesUnder(g, g.Cond) {
		under[ref] = true
	}
	// past keeps what each reduce saw, by series, for windows that span
	// several steps, and days.
	type point struct {
		t int64
		v float64
	}
	past := map[string]map[string][]point{}
	var reduces []string
	for _, ref := range slices.Sorted(maps.Keys(g.Steps)) {
		if g.Steps[ref].Reduce != nil {
			reduces = append(reduces, ref)
		}
	}
	every := Every(r, step)
	phase := from.UnixNano() % int64(step)
	// Grafana's alerts, worked out from before from, so the evaluations
	// from it find those Grafana would have had.
	start := grafanaStart(r, from, step, warm && !values).Add(warmup(g, step))
	var held *grafanaStates
	var known *dataSeen
	if !values {
		held = newStates(r, every)
		known = &dataSeen{probed: map[string]bool{}, seen: map[string]bool{}}
	}
	firsts := map[int]*windowFirsts{}
	var failed error
	for _, c := range chunks(start.Add(-warmup(g, step)), to, step) {
		// The queries of one day are asked for together.
		answers := make([][]prom.Series, len(refs))
		errs := make([]error, len(refs))
		var wg sync.WaitGroup
		for i, ref := range refs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				expr, _ := g.Expr(ref, r.AskStep(ref))
				answers[i], errs[i] = shifted(ctx, h, expr, c[0], c[1], step, max(g.Shift[ref], r.Late))
			}()
		}
		wg.Wait()
		// A query the condition reads that fails fails the rule. One
		// outside it the server refuses as written (400) returned nothing
		// to Grafana every time, which takes it as no data, as Grafana
		// 13.3.1 was seen to; one that fails otherwise is taken as not
		// known to have returned anything or not.
		unasked, refused := map[string]bool{}, map[string]bool{}
		for i, err := range errs {
			var api *prom.APIError
			switch {
			case err == nil:
			case under[refs[i]] || ctx.Err() != nil:
				return nil, err
			case errors.As(err, &api) && api.Status == http.StatusBadRequest:
				refused[refs[i]] = true
			default:
				unasked[refs[i]] = true
				for ref, d := range g.Data {
					if d.Present == refs[i] {
						noteFailed(ctx, ref)
					}
				}
			}
		}
		data := map[string]map[int64]sse.Set{}
		for i, ref := range refs {
			series := answers[i]
			byT := map[int64]sse.Set{}
			for _, s := range series {
				l := maps.Clone(s.Labels)
				delete(l, "__name__")
				k := key(l)
				for _, smp := range s.Samples {
					t := smp.T.Unix()
					byT[t] = append(byT[t], sse.Item{Labels: l, Key: k, V: smp.V})
				}
			}
			data[ref] = byT
		}
		for at := c[0]; !at.After(c[1]); at = at.Add(step) {
			t := at.Unix()
			memo := map[string]sse.Set{}
			var eval func(ref string) sse.Set
			eval = func(ref string) sse.Set {
				if s, ok := memo[ref]; ok {
					return s
				}
				var s sse.Set
				switch st, isStep := g.Steps[ref]; {
				case !isStep:
					s = data[ref][t]
				case st.Threshold != nil:
					for _, it := range eval(st.Threshold.Input) {
						v := 0.0
						if st.Threshold.Pass(it.V) {
							v = 1
						}
						s = append(s, sse.Item{Labels: it.Labels, Key: it.Key, V: v})
					}
				case st.Reduce != nil:
					rd := st.Reduce
					if past[ref] == nil {
						past[ref] = map[string][]point{}
					}
					for _, it := range eval(rd.Input) {
						k := it.Key
						if k == "" {
							k = key(it.Labels)
						}
						pts := append(past[ref][k], point{t, it.V})
						for len(pts) > 0 && pts[0].t <= t-int64(rd.Window/time.Second) && rd.Window > 0 {
							pts = pts[1:]
						}
						if rd.Window == 0 {
							pts = pts[len(pts)-1:]
						}
						past[ref][k] = pts
						vs := make([]float64, len(pts))
						for i, p := range pts {
							vs[i] = p.v
						}
						s = append(s, sse.Item{Labels: it.Labels, Key: it.Key, V: reduce(rd.Fn, vs)})
					}
				case st.Classic != nil:
					var err error
					if s, err = classicAt(ctx, h, r, st.Classic, at, eval, firsts); err != nil && failed == nil {
						failed = err
					}
				default:
					s = st.Math.Eval(func(r string) (sse.Set, bool) { return eval(r), true })
				}
				memo[ref] = s
				return s
			}
			// The steps before start are for reduces to see whole windows;
			// a rule is evaluated only every so often.
			if at.Before(start) || (at.UnixNano()-phase)%int64(every) != 0 {
				for _, ref := range reduces {
					eval(ref)
				}
				continue
			}
			cond := eval(g.Cond)
			if failed != nil {
				return nil, failed
			}
			if values {
				for _, it := range cond {
					if it.V == 0 || math.IsNaN(it.V) {
						continue
					}
					k := it.Key
					if k == "" {
						k = key(it.Labels)
					}
					add(k, it.Labels, prom.Sample{T: at, V: it.V})
				}
				continue
			}
			noData, unknown, err := known.noData(ctx, h, r, data, unasked, refused, at)
			if err != nil {
				return nil, err
			}
			var now []heldAt
			if unknown && len(noData) == 0 && len(cond) == 0 {
				// Nothing to say: a query the condition reads has no
				// history yet. The alerts stay as they are.
				now = held.firing()
			} else {
				now = held.eval(at, cond, noData)
			}
			if !at.Before(from) {
				for _, s := range now {
					add(s.key, s.labels, prom.Sample{T: at, V: 1})
				}
			}
		}
	}
	return result(), nil
}

// dataSeen is, for one replay of a Grafana rule, which of its data source
// queries have returned something: in the evaluations so far, or in the
// day before the first that returned nothing.
type dataSeen struct {
	seen, probed map[string]bool
}

// noData is, when a data source query of r returned nothing at at, the
// labels of Grafana's NoData result: for each data source, its uid, and the
// refIds of its queries that returned nothing. A query is taken to have
// returned nothing only once it has returned something, in this replay or
// in the day before: before then, the history may simply not be there
// (before the server's retention, before the metric was, or on another
// data source than the one read), and Grafana's NoData can't be told from
// it. unknown says such a query returned nothing. A question about the day
// before that fails fails the replay, as a query does.
func (k *dataSeen) noData(ctx context.Context, h History, r rules.Rule, data map[string]map[int64]sse.Set, failed, refused map[string]bool, at time.Time) (out []map[string]string, unknown bool, err error) {
	g := r.Graph
	t := at.Unix()
	byUID := map[string][]string{}
	for _, ref := range slices.Sorted(maps.Keys(g.Data)) {
		d := g.Data[ref]
		if failed[d.Present] {
			unknown = true
			continue
		}
		if refused[d.Present] {
			byUID[d.UID] = append(byUID[d.UID], ref)
			continue
		}
		if len(data[d.Present][t]) > 0 {
			k.seen[ref] = true
			continue
		}
		if !k.seen[ref] && !k.probed[ref] {
			k.probed[ref] = true
			if k.seen[ref], err = before(ctx, h, r, ref, at); err != nil {
				return nil, false, err
			}
		}
		if !k.seen[ref] {
			unknown = true
			continue
		}
		byUID[d.UID] = append(byUID[d.UID], ref)
	}
	for _, uid := range slices.Sorted(maps.Keys(byUID)) {
		out = append(out, map[string]string{"datasource_uid": uid, "ref_id": strings.Join(byUID[uid], ",")})
	}
	return out, unknown, nil
}

// probeStep is how finely the day before is looked through: as often as
// the rule is evaluated, a minute at most, so a query that returned
// something for only a minute, between two of a coarser grid, is seen.
func probeStep(r rules.Rule) time.Duration {
	if r.Interval <= 0 || r.Interval > time.Minute {
		return time.Minute
	}
	return r.Interval
}

// probe is the question before asks: when, in the day before, the query
// last returned anything; nothing when it didn't.
func probe(expr string, step time.Duration) string {
	return "max(max_over_time(timestamp((" + expr + "))[1d:" + promStep(step) + "]))"
}

// A seenMemo keeps, for each probe asked of one Prometheus, the last time
// its query returned anything, so a later replay, a serve run an hour on,
// needn't ask again while that is still within the day. It is let go whole
// when full.
type seenMemo struct {
	sync.Mutex
	m map[string]time.Time
}

const maxSeenMemo = 10_000

var memos sync.Map // *prom.Client to its *seenMemo

// memoOf is the memo of the Prometheus h asks, nil when h isn't one.
func memoOf(h History) *seenMemo {
	if a, ok := h.(*asker); ok {
		h = a.s.h
	}
	c, ok := h.(*prom.Client)
	if !ok {
		return nil
	}
	m, _ := memos.LoadOrStore(c, &seenMemo{m: map[string]time.Time{}})
	return m.(*seenMemo)
}

// before reports whether a data source query returned anything in the day
// before at.
func before(ctx context.Context, h History, r rules.Rule, ref string, at time.Time) (bool, error) {
	expr, ok := r.Graph.Queries[ref]
	if !ok {
		return false, nil
	}
	when := at.Add(-max(r.Graph.Shift[ref], r.Late))
	q := probe(expr, probeStep(r))
	memo := memoOf(h)
	if memo != nil {
		memo.Lock()
		last, ok := memo.m[q]
		memo.Unlock()
		if ok && last.After(when.Add(-24*time.Hour)) && !last.After(when) {
			return true, nil
		}
	}
	var series []prom.Series
	var err error
	if a, ok := h.(*asker); ok {
		series, err = a.s.probe(ctx, q, when)
	} else {
		series, err = h.QueryRange(ctx, q, when, when, time.Minute)
	}
	if err != nil {
		return false, err
	}
	for _, s := range series {
		for _, smp := range s.Samples {
			if math.IsNaN(smp.V) || math.IsInf(smp.V, 0) {
				continue
			}
			if memo != nil {
				seen := time.Unix(0, int64(smp.V*1e9))
				memo.Lock()
				if len(memo.m) >= maxSeenMemo {
					clear(memo.m)
				}
				if seen.After(memo.m[q]) {
					memo.m[q] = seen
				}
				memo.Unlock()
			}
			return true, nil
		}
	}
	return false, nil
}

// classicAt is what classic conditions come to at one evaluation: a series
// with no labels, 1 when they hold, 0 when not, none when Grafana would say
// there was no data; or, for c.Of, the value that condition compares.
func classicAt(ctx context.Context, h History, r rules.Rule, c *rules.Classic, at time.Time, eval func(string) sse.Set,
	firsts map[int]*windowFirsts) (sse.Set, error) {
	var failed error
	series := func(i int) []rules.Reduced {
		cd := c.Conds[i]
		byKey := func(ref string) map[string]float64 {
			m := map[string]float64{}
			if ref == "" {
				return m
			}
			for _, it := range eval(ref) {
				k := it.Key
				if k == "" {
					k = key(it.Labels)
				}
				m[k] = it.V
			}
			return m
		}
		values, oldest, points := byKey(cd.Value), byKey(cd.Oldest), byKey(cd.Points)
		var out []rules.Reduced
		for _, it := range eval(cd.Present) {
			k := it.Key
			if k == "" {
				k = key(it.Labels)
			}
			v, ok := values[k]
			o, has := oldest[k]
			if cd.Diff() && ok && !has {
				// No number at the window's start: Grafana takes the first
				// in the window, which only the window's points say.
				w := firsts[i]
				if w == nil {
					w = &windowFirsts{}
					firsts[i] = w
				}
				var err error
				if o, has, err = w.first(ctx, h, r, cd, at, k); err != nil && failed == nil {
					failed = err
				}
			}
			out = append(out, cd.Reduce(rules.Reduced{V: v, OK: ok}, rules.Reduced{V: o, OK: has}, points[k] == 1))
		}
		return out
	}
	// One alert, with no labels of its own.
	one := func(v float64) sse.Set { return sse.Set{{Labels: map[string]string{}, V: v}} }
	if c.Of > 0 {
		v := c.ValueOf(c.Of-1, series)
		if failed != nil || math.IsNaN(v) {
			return nil, failed
		}
		return one(v), nil
	}
	firing, noData := c.Eval(series)
	switch {
	case failed != nil:
		return nil, failed
	case noData:
		return nil, nil
	case firing:
		return one(1), nil
	}
	return one(0), nil
}

// windowFirsts are, for a classic condition, the first numbers of its
// series' windows, as last asked for: a series that appeared within the
// window keeps its first number until the window has passed it, so it is
// asked for again only for a series not asked for yet, or once that has.
type windowFirsts struct {
	byKey map[string]firstAt
}

// firstAt is a series' first number in a window, and when it was; when
// none was found, when it was asked.
type firstAt struct {
	t     time.Time
	v     float64
	found bool
}

// first is the first number in the window ending at at of the series with
// key k, as Grafana's range query has it, asking for the window's points
// when it isn't known.
func (w *windowFirsts) first(ctx context.Context, h History, r rules.Rule, cd rules.ClassicCond, at time.Time, k string) (float64, bool, error) {
	lag := max(r.Graph.Shift[cd.Value], r.Late)
	end := at.Add(-lag)
	if f, ok := w.byKey[k]; ok && !f.t.Before(end.Add(-cd.Window)) {
		return f.v, f.found, nil
	}
	series, err := h.QueryRange(ctx, "(("+cd.Expr+") >= -Inf)", end.Add(-cd.Window), end, cd.Res)
	if err != nil {
		return 0, false, fmt.Errorf("the first numbers of %s's window: %w", cd.Query, err)
	}
	if w.byKey == nil {
		w.byKey = map[string]firstAt{}
	}
	for _, s := range series {
		l := maps.Clone(s.Labels)
		delete(l, "__name__")
		for _, smp := range s.Samples {
			if !math.IsNaN(smp.V) && !smp.T.Before(end.Add(-cd.Window)) && !smp.T.After(end) {
				w.byKey[key(l)] = firstAt{t: smp.T, v: smp.V, found: true}
				break
			}
		}
	}
	f, ok := w.byKey[k]
	if !ok || f.t.Before(end.Add(-cd.Window)) {
		// None found: not asked for again until a window from now.
		f = firstAt{t: end}
		w.byKey[k] = f
	}
	return f.v, f.found, nil
}

// asked are the refs a Grafana rule's evaluation asks the history for: the
// queries its condition depends on and, unless only its values are wanted,
// what says whether each data source query returned anything.
func asked(g *rules.Graph, values bool) []string {
	refs := queriesUnder(g, g.Cond)
	if !values {
		for _, d := range g.Data {
			if !slices.Contains(refs, d.Present) {
				refs = append(refs, d.Present)
			}
		}
	}
	sort.Strings(refs)
	return refs
}

// queriesUnder lists the queries a node depends on.
func queriesUnder(g *rules.Graph, ref string) []string {
	seen := map[string]bool{}
	var walk func(string)
	walk = func(r string) {
		if seen[r] {
			return
		}
		seen[r] = true
		if st, ok := g.Steps[r]; ok {
			for _, u := range st.Uses() {
				walk(u)
			}
		}
	}
	walk(ref)
	var out []string
	for r := range seen {
		if _, ok := g.Expr(r, 0); ok {
			if _, isStep := g.Steps[r]; !isStep {
				out = append(out, r)
			}
		}
	}
	sort.Strings(out)
	return out
}

// reduce collapses values the way Grafana's reducers do.
func reduce(fn string, vs []float64) float64 {
	if len(vs) == 0 {
		return math.NaN()
	}
	switch fn {
	case "last":
		return vs[len(vs)-1]
	case "count":
		return float64(len(vs))
	}
	acc := vs[0]
	for _, v := range vs[1:] {
		switch fn {
		case "max":
			acc = max(acc, v)
		case "min":
			acc = min(acc, v)
		default: // sum, mean
			acc += v
		}
	}
	if fn == "mean" {
		acc /= float64(len(vs))
	}
	return acc
}

func key(l map[string]string) string { return sse.LabelKey(l) }

// promStep is a step as PromQL writes it: whole seconds, or milliseconds.
func promStep(d time.Duration) string {
	if d%time.Second == 0 {
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	}
	return strconv.FormatInt(int64(d/time.Millisecond), 10) + "ms"
}
