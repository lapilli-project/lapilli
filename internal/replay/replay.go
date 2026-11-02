// Package replay runs alert rules against history to find when they would
// have fired.
package replay

import (
	"context"
	"maps"
	"math"
	"slices"
	"sort"
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

// Run replays every rule over [from, to], a few at a time, and returns when
// each would have fired. A rule whose queries fail gets Unsupported set
// rather than failing the run: one bad expression shouldn't hide the
// verdict on the rest.
func Run(ctx context.Context, h History, rs []rules.Rule, from, to time.Time, step time.Duration) []firing.Firing {
	results := make([][]firing.Firing, len(rs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i := range rs {
		r := &rs[i]
		if r.Unsupported != "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			series, err := Condition(ctx, h, *r, from.Add(-r.For), to, step)
			switch {
			case err != nil && ctx.Err() != nil:
				r.Unsupported = Unfinished
				return
			case err != nil:
				r.Unsupported = "query failed: " + err.Error()
				return
			}
			results[i] = firing.FromSeries(r.Name, r.Labels, series, step, r.For, to)
		}()
	}
	wg.Wait()
	var out []firing.Firing
	for _, fs := range results {
		out = append(out, fs...)
	}
	firing.Sort(out)
	return out
}

// Condition returns, for each label set, the steps at which the rule's
// condition held.
func Condition(ctx context.Context, h History, r rules.Rule, from, to time.Time, step time.Duration) ([]prom.Series, error) {
	if r.Graph == nil {
		return h.QueryRange(ctx, r.Expr, from, to, step)
	}
	g := r.Graph
	// Fetch the queries the condition depends on.
	data := map[string]map[int64]sse.Set{}
	stamps := map[int64]bool{}
	for _, ref := range queriesUnder(g, g.Cond) {
		series, err := h.QueryRange(ctx, g.Queries[ref], from, to, step)
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
		for _, it := range eval(g.Cond) {
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
