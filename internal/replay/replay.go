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
func Run(ctx context.Context, h History, versions []Version, from, to time.Time, step time.Duration) ([]rules.Rule, []firing.Firing) {
	runs := lineUp(versions, from, to, step)
	ran := make([]rules.Rule, len(runs))
	results := make([][]firing.Firing, len(runs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, run := range runs {
		ran[i] = run[len(run)-1].Rule
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i], ran[i].Unsupported = replayRun(ctx, h, run, to, step)
		}()
	}
	wg.Wait()
	// A rule paused for part of the period was replayed for the rest; it
	// just didn't fire while paused.
	live := map[string]bool{}
	for _, r := range ran {
		live[r.Name] = live[r.Name] || r.Unsupported == ""
	}
	for i := range ran {
		if ran[i].Unsupported == rules.Paused && live[ran[i].Name] {
			ran[i].Unsupported = ""
		}
	}
	var out []firing.Firing
	for _, fs := range results {
		out = append(out, fs...)
	}
	firing.Sort(out)
	return ran, out
}

// A stint is one version of a rule while it was in effect: evaluated at the
// steps from lo up to, not including, hi.
type stint struct {
	rules.Rule
	lo, hi time.Time
	before bool // in effect before the period, so pending from before it too
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
	return a.Expr == b.Expr && a.For == b.For && a.Digest == b.Digest && a.Unsupported == b.Unsupported
}

// replayRun replays one run of stints and says why it couldn't, if it
// couldn't.
func replayRun(ctx context.Context, h History, run []stint, to time.Time, step time.Duration) ([]firing.Firing, string) {
	merged := map[string]*prom.Series{}
	for _, st := range run {
		if st.Unsupported != "" {
			return nil, st.Unsupported
		}
		lo := st.lo
		if st.before {
			lo = lo.Add(-st.For)
		}
		series, err := Condition(ctx, h, st.Rule, lo, st.hi.Add(-step), step)
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
	end := to
	if !last.hi.After(to) {
		end = last.hi.Add(step) // taken away or started afresh: its alerts resolved then
	}
	return firing.FromSeries(last.Name, last.Labels, series, step, fireAt(run), end), ""
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

// Condition returns, for each label set, the steps at which the rule's
// condition held.
func Condition(ctx context.Context, h History, r rules.Rule, from, to time.Time, step time.Duration) ([]prom.Series, error) {
	if r.Graph == nil {
		return h.QueryRange(ctx, r.Expr, from, to, step)
	}
	g := r.Graph
	// A reduce over several steps needs the steps before from as well, as
	// Grafana's query would have them.
	var warm time.Duration
	for _, st := range g.Steps {
		if st.Reduce != nil {
			warm = max(warm, (st.Reduce.Window+step-1)/step*step)
		}
	}
	// Fetch the queries the condition depends on.
	data := map[string]map[int64]sse.Set{}
	stamps := map[int64]bool{}
	for _, ref := range queriesUnder(g, g.Cond) {
		series, err := h.QueryRange(ctx, g.Queries[ref], from.Add(-warm), to, step)
		if err != nil {
			return nil, err
		}
		byT := map[int64]sse.Set{}
		for _, s := range series {
			l := maps.Clone(s.Labels)
			delete(l, "__name__")
			for _, smp := range s.Samples {
				t := smp.T.Unix()
				byT[t] = append(byT[t], sse.Item{Labels: l, V: smp.V})
				stamps[t] = true
			}
		}
		data[ref] = byT
	}

	out := map[string]*prom.Series{}
	// past keeps what each reduce saw, by series, for windows that span
	// several steps.
	type point struct {
		t int64
		v float64
	}
	past := map[string]map[string][]point{}
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
					s = append(s, sse.Item{Labels: it.Labels, V: v})
				}
			case st.Reduce != nil:
				rd := st.Reduce
				if past[ref] == nil {
					past[ref] = map[string][]point{}
				}
				for _, it := range eval(rd.Input) {
					k := key(it.Labels)
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
					s = append(s, sse.Item{Labels: it.Labels, V: reduce(rd.Fn, vs)})
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
			k := key(it.Labels)
			s, ok := out[k]
			if !ok {
				s = &prom.Series{Labels: it.Labels}
				out[k] = s
			}
			s.Samples = append(s.Samples, prom.Sample{T: time.Unix(t, 0).UTC(), V: it.V})
		}
	}
	res := make([]prom.Series, 0, len(out))
	for _, k := range slices.Sorted(maps.Keys(out)) {
		res = append(res, *out[k])
	}
	return res, nil
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

func key(l map[string]string) string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(l)) {
		b.WriteString(k + "=" + l[k] + "\x00")
	}
	return b.String()
}
