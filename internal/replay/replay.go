// Package replay runs alert rules against history to find when they would
// have fired.
package replay

import (
	"context"
	"maps"
	"math"
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
// over by uid, but starts afresh when the rule changed.
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
		for _, st := range run {
			if st.Unsupported != "" {
				break
			}
			lo, hi := st.span(step)
			for _, r := range queries(st.Rule, lo, hi, step) {
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
			results[i], ran[i].Unsupported = replayRun(ctx, askers[i], runs[i], to, step)
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
	series, err := Condition(ctx, h, r, from.Add(-slices.Max(fors)), to, step)
	if err != nil {
		return nil, err
	}
	out := map[time.Duration][]firing.Firing{}
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
	series, err := Condition(ctx, h, r, from, to, step)
	if err != nil {
		return nil, err
	}
	r.For, r.KeepFiringFor = 0, 0
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

// A stint is one version of a rule while it was in effect: evaluated at the
// steps from lo up to, not including, hi.
type stint struct {
	rules.Rule
	lo, hi time.Time
	before bool // in effect before the period, so pending from before it too
}

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
			case !ok || runs[j][len(runs[j])-1].Digest != r.Digest:
				// new, or a Grafana rule that changed
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
	return a.Expr == b.Expr && a.For == b.For && a.KeepFiringFor == b.KeepFiringFor && a.Interval == b.Interval &&
		a.Digest == b.Digest && a.Unsupported == b.Unsupported
}

// replayRun replays one run of stints and says why it couldn't, if it
// couldn't.
func replayRun(ctx context.Context, h History, run []stint, to time.Time, step time.Duration) ([]firing.Firing, string) {
	merged := map[string]*prom.Series{}
	for _, st := range run {
		if st.Unsupported != "" {
			return nil, st.Unsupported
		}
		lo, hi := st.span(step)
		series, err := Condition(ctx, h, st.Rule, lo, hi, step)
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, Unfinished
		case err != nil:
			return nil, "query failed: " + err.Error()
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
	return fs, ""
}

// fire works out a rule's firings from the steps its condition held,
// evaluated as often as its group is when that is a whole number of steps:
// a 5m group sees its condition every fifth step, and fires and resolves
// only then.
func fire(r rules.Rule, series []prom.Series, step time.Duration, fireAt func(time.Time) time.Time, end time.Time) []firing.Firing {
	every := step
	if Every(r, step) > step && len(series) > 0 && len(series[0].Samples) > 0 {
		every = Every(r, step)
		// Evaluations fall on the steps' own grid: the Unix epoch's, unless a
		// period was given in odd seconds. Prometheus offsets each group
		// within its interval as well, which a replay can't know.
		phase := series[0].Samples[0].T.UnixNano() % int64(step)
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
		series = thin
		at := fireAt
		fireAt = func(pending time.Time) time.Time {
			t := at(pending)
			if r := (t.UnixNano() - phase) % int64(every); r != 0 {
				t = t.Add(every - time.Duration(r))
			}
			return t
		}
	}
	return firing.FromSeries(r.Name, r.Labels, series, every, fireAt, r.KeepFiringFor, end)
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
func queries(r rules.Rule, from, to time.Time, step time.Duration) []request {
	var out []request
	add := func(expr string, from time.Time) {
		for _, c := range chunks(from, to, step) {
			out = append(out, request{expr, c[0].Unix(), c[1].Unix()})
		}
	}
	if r.Graph == nil {
		add(r.Expr, from)
		return out
	}
	for _, ref := range queriesUnder(r.Graph, r.Graph.Cond) {
		add(r.Graph.Queries[ref], from.Add(-warmup(r.Graph, step)))
	}
	return out
}

// Condition returns, for each label set, the steps in [from, to] at which the
// rule's condition held. It fetches and evaluates a day at a time, so a long
// period takes no more memory than a day.
func Condition(ctx context.Context, h History, r rules.Rule, from, to time.Time, step time.Duration) ([]prom.Series, error) {
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
			part, err := h.QueryRange(ctx, r.Expr, c[0], c[1], step)
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
	refs := queriesUnder(g, g.Cond)
	// past keeps what each reduce saw, by series, for windows that span
	// several steps, and days.
	type point struct {
		t int64
		v float64
	}
	past := map[string]map[string][]point{}
	for _, c := range chunks(from.Add(-warmup(g, step)), to, step) {
		// The queries of one day are asked for together.
		answers := make([][]prom.Series, len(refs))
		errs := make([]error, len(refs))
		var wg sync.WaitGroup
		for i, ref := range refs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				answers[i], errs[i] = h.QueryRange(ctx, g.Queries[ref], c[0], c[1], step)
			}()
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				return nil, err
			}
		}
		data := map[string]map[int64]sse.Set{}
		stamps := map[int64]bool{}
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
					stamps[t] = true
				}
			}
			data[ref] = byT
		}
		for _, t := range slices.Sorted(maps.Keys(stamps)) {
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
				default:
					s = st.Math.Eval(func(r string) (sse.Set, bool) { return eval(r), true })
				}
				memo[ref] = s
				return s
			}
			cond := eval(g.Cond)
			if t < from.Unix() {
				continue
			}
			for _, it := range cond {
				if it.V == 0 || math.IsNaN(it.V) {
					continue
				}
				k := it.Key
				if k == "" {
					k = key(it.Labels)
				}
				add(k, it.Labels, prom.Sample{T: time.Unix(t, 0).UTC(), V: it.V})
			}
		}
	}
	return result(), nil
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
		if _, ok := g.Queries[r]; ok {
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
