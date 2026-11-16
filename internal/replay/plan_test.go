package replay

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
)

// tallied is a history that counts what it is asked, by request, and
// answers each with a series holding a sample at every step, so that no
// query of a Grafana rule returns nothing.
type tallied struct {
	mu   *sync.Mutex // shared with the histories its by is
	name string
	by   map[string]int
	asks map[request]int
}

func (c *tallied) QueryRange(_ context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	c.mu.Lock()
	c.by[c.name]++
	c.asks[request{expr: expr, start: start.Unix(), end: end.Unix()}]++
	c.mu.Unlock()
	s := prom.Series{Labels: map[string]string{"job": "a"}}
	for t := start; !t.After(end); t = t.Add(step) {
		s.Samples = append(s.Samples, prom.Sample{T: t, V: 1})
	}
	return []prom.Series{s}, nil
}

// randomRules are n rules of PromQL, some alike in what they ask, some of
// LogQL for a Loki ruler, some of Grafana, with every way a rule's
// requests can differ: for, keep_firing_for, the group's interval and
// place in it, its query offset, how late the metrics arrive.
func randomRules(t *testing.T, r *rand.Rand, n int) []rules.Rule {
	pick := func(ds ...time.Duration) time.Duration { return ds[r.IntN(len(ds))] }
	var out []rules.Rule
	for i := range n {
		x := rules.Rule{Name: fmt.Sprintf("R%d", i), Group: fmt.Sprintf("g%d", r.IntN(3)),
			Expr:          fmt.Sprintf(`rate(errors_total{job="j%d"}[5m]) > 1`, r.IntN(n/2+1)),
			For:           pick(0, time.Minute, 5*time.Minute, time.Hour, 24*time.Hour),
			KeepFiringFor: pick(0, 0, 10*time.Minute),
			Interval:      pick(0, time.Minute, 2*time.Minute, 5*time.Minute, 90*time.Second),
			QueryOffset:   pick(0, 0, 30*time.Second, time.Minute),
			Late:          pick(0, 0, 2*time.Minute),
		}
		if x.Interval > 0 && r.IntN(3) == 0 {
			x.Phase = time.Duration(r.Int64N(int64(x.Interval)/int64(time.Second))) * time.Second
		}
		switch r.IntN(8) {
		case 0:
			x.Logs = rules.LokiRuler
			x.Expr = fmt.Sprintf(`sum(rate({app="a%d"} |= "error" [5m])) > 1`, r.IntN(3))
		case 1:
			vs := grafanaRule(t, fmt.Sprintf("        for: %s", pick(0, time.Minute, 5*time.Minute)))
			g := vs[0].Rules[0]
			g.Name, g.UID = x.Name, fmt.Sprintf("u%d", i)
			x = g
		}
		out = append(out, x)
	}
	return out
}

// The requests Plan counts are exactly those Run sends, request by
// request and by history, over random rules, steps, periods and versions:
// the estimate lapilli preflight gives can't drift from the replay.
func TestPlanIsRun(t *testing.T) {
	r := rand.New(rand.NewPCG(42, 1))
	for c := range 120 {
		step := []time.Duration{time.Minute, 30 * time.Second, 2 * time.Minute}[r.IntN(3)]
		from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(r.IntN(48*60)) * time.Minute).Truncate(step)
		to := from.Add(time.Duration(30+r.IntN(4*24*60)) * time.Minute).Truncate(step)
		all := randomRules(t, r, 3+r.IntN(30))
		versions := []Version{{Rules: all[:len(all)/2+1]}}
		if r.IntN(2) == 0 {
			// A version that takes over in the period, some rules alike.
			at := from.Add(to.Sub(from) / time.Duration(2+r.IntN(3))).Truncate(time.Second)
			later := append([]rules.Rule{}, all[len(all)/3:]...)
			if len(later) > 0 {
				later[0].For += time.Minute
			}
			versions = append(versions, Version{From: at, Rules: later})
		}
		mu := &sync.Mutex{}
		prometheus := &tallied{mu: mu, name: "", by: map[string]int{}, asks: map[request]int{}}
		loki := &tallied{mu: mu, name: rules.LokiRuler, by: prometheus.by, asks: map[request]int{}}
		h := WithLogs(prometheus, func(rules.Rule) (History, error) { return loki, nil })
		if c%5 == 0 {
			h = prometheus // no Loki: the rules of LogQL ask nothing
		}
		plan := Plan(h, versions, from, to, step)
		Run(context.Background(), h, versions, from, to, step, 1+r.IntN(8))
		sent := 0
		for _, n := range prometheus.by {
			sent += n
		}
		if plan.Requests != sent {
			t.Fatalf("case %d (step %s, %s → %s): Plan counts %d requests, Run sent %d", c, step, from, to, plan.Requests, sent)
		}
		for name, n := range prometheus.by {
			if plan.By[name] != n {
				t.Errorf("case %d: Plan counts %d requests to %q, Run sent %d", c, plan.By[name], name, n)
			}
		}
		for _, x := range []*tallied{prometheus, loki} {
			for req, n := range x.asks {
				if n != 1 {
					t.Errorf("case %d: %q over [%d, %d] asked %d times: the rules alike share it", c, req.expr, req.start, req.end, n)
				}
			}
		}
	}
}

// What Run can't ask is planned as nothing: a rule of LogQL with no Loki,
// a rule already unsupported.
func TestPlanNothing(t *testing.T) {
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	rs := []rules.Rule{{Name: "L", Logs: rules.LokiRuler, Expr: `count_over_time({a="b"}[5m]) > 1`}, {Name: "U", Expr: "up == 0", Unsupported: "no"}}
	if p := Plan(&tallied{mu: &sync.Mutex{}, by: map[string]int{}, asks: map[request]int{}}, []Version{{Rules: rs}}, from, from.Add(time.Hour), time.Minute); p.Requests != 0 {
		t.Errorf("%+v", p)
	}
}
