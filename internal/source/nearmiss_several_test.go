package source

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// several answers expressions of names compared with numbers, joined by
// and, or and unless, left to right: latency is 0.88 and requests 8.5 from
// 10:00 to 10:30 on the Tuesday, and otherwise 0.1 and 50; errors is 1.
type several struct {
	asked atomic.Int64
	// values, when set, are each name's instead.
	values map[string]func(time.Time) float64
}

func (h *several) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	h.asked.Add(1)
	tue := t0.Add(24 * time.Hour)
	value := func(name string, t time.Time) float64 {
		if v, ok := h.values[name]; ok {
			return v(t)
		}
		during := !t.Before(tue.Add(10*time.Hour)) && t.Before(tue.Add(10*time.Hour+30*time.Minute))
		switch {
		case name == "latency" && during:
			return 0.88
		case name == "latency":
			return 0.1
		case name == "requests" && during:
			return 8.5
		case name == "requests":
			return 50
		}
		return 1
	}
	// Each operand is a name, or a name compared with a number.
	type operand struct {
		name string
		b    *rules.Bound
	}
	var ops []operand
	var joins []string
	for _, part := range strings.Fields(expr) {
		switch {
		case part == "and" || part == "or" || part == "unless":
			joins = append(joins, part)
		case part == ">" || part == "<" || part == ">=" || part == "<=":
			ops[len(ops)-1].b = &rules.Bound{Op: part}
		case len(ops) > 0 && ops[len(ops)-1].b != nil && ops[len(ops)-1].b.Value == 0 && len(ops) > len(joins):
			n, _ := strconv.ParseFloat(part, 64)
			ops[len(ops)-1].b.Value = n
		default:
			ops = append(ops, operand{name: part})
		}
	}
	at := func(o operand, t time.Time) (float64, bool) {
		v := value(o.name, t)
		return v, o.b == nil || o.b.Holds(v)
	}
	s := prom.Series{Labels: map[string]string{"service": "checkout"}}
	for t := start; !t.After(end); t = t.Add(step) {
		v, ok := at(ops[0], t)
		for i, j := range joins {
			w, holds := at(ops[i+1], t)
			switch j {
			case "and":
				ok = ok && holds
			case "unless":
				ok = ok && !holds
			case "or":
				if !ok && holds {
					v, ok = w, true
				}
			}
		}
		if ok {
			s.Samples = append(s.Samples, prom.Sample{T: t, V: v})
		}
	}
	if len(s.Samples) == 0 {
		return nil, nil
	}
	return []prom.Series{s}, nil
}

func TestNearMissesOfSeveral(t *testing.T) {
	o := verdict.Options{From: t0, To: t0.Add(7 * 24 * time.Hour), Lead: 15 * time.Minute, Match: []string{"service"},
		RefireWithin: time.Hour, UnknownLen: time.Hour}
	tue := t0.Add(24 * time.Hour)
	incs := []incident.Incident{{ID: "INC-1", Start: tue.Add(10 * time.Hour), End: tue.Add(10*time.Hour + 30*time.Minute),
		Labels: incident.Labels{"service": {"checkout"}}}}
	for _, c := range []struct {
		expr    string
		of      string
		unless  bool
		bound   float64
		new     float64
		reached float64 // 0 when it wasn't read
	}{
		// Too few requests for the guard: with 8 it would have fired.
		{"latency > 0.85 and requests > 10", "requests", false, 10, 8, 0},
		// The exclusion kept it quiet: with < 8.5, 8.5 requests don't.
		{"latency > 0.85 unless requests < 9", "requests", true, 9, 8.5, 0},
		// Either of two: latency came within a fifth of 0.95.
		{"latency > 0.95 or errors > 5", "latency", false, 0.95, 0.87, 0},
		// The first of an and: its values are read, the guard holding.
		{"latency > 0.9 and requests > 1", "latency", false, 0.9, 0.87, 0.88},
	} {
		rs := []rules.Rule{{Name: "R", Expr: c.expr, For: 5 * time.Minute}}
		h := &several{}
		versions := []replay.Version{{Rules: rs}}
		_, fs := replay.Run(context.Background(), h, versions, o.From, o.To, time.Minute, 4)
		rep := verdict.Judge(rs, fs, incs, o)
		if !rep.Incidents[0].Missed() {
			t.Fatalf("%s: INC-1 caught: %+v", c.expr, rep.Incidents[0])
		}
		nms, done, err := NearMisses(context.Background(), h, versions, rep, incs, o, time.Minute, 4, nil)
		if err != nil || !done || len(nms) != 1 || nms[0].Value == nil {
			t.Errorf("%s: %v %v %+v", c.expr, err, done, nms)
			continue
		}
		nm, v := nms[0], nms[0].Value
		read := v.Reached != nil
		if v.Of != c.of || v.Unless != c.unless || v.Bound != c.bound || v.New != c.new || read != (c.reached != 0) || read && *v.Reached != c.reached ||
			nm.Delay != 5*time.Minute {
			t.Errorf("%s: %+v %+v", c.expr, nm, v)
		}
		// With the new number, it fires once that week, on INC-1.
		if nm.After == nil || nm.After.Firings != 1 || nm.After.White != 1 {
			t.Errorf("%s: cost %+v", c.expr, nm.After)
		}
		// A few questions, not one for each round number.
		if n := h.asked.Load(); n > 60 {
			t.Errorf("%s: asked %d", c.expr, n)
		}
	}

	// A guard far short: none within a fifth.
	rs := []rules.Rule{{Name: "R", Expr: "latency > 0.85 and requests > 20", For: 5 * time.Minute}}
	rep := verdict.Judge(rs, nil, incs, o)
	nms, done, err := NearMisses(context.Background(), &several{}, []replay.Version{{Rules: rs}}, rep, incs, o, time.Minute, 4, nil)
	if err != nil || !done || len(nms) != 0 {
		t.Errorf("far short: %v %v %+v", err, done, nms)
	}
}

// A Grafana range: latency within 0.9 and 2 came to 0.88.
func TestNearMissesOfARange(t *testing.T) {
	o := verdict.Options{From: t0, To: t0.Add(7 * 24 * time.Hour), Lead: 15 * time.Minute, Match: []string{"service"},
		RefireWithin: time.Hour, UnknownLen: time.Hour}
	tue := t0.Add(24 * time.Hour)
	incs := []incident.Incident{{ID: "INC-1", Start: tue.Add(10 * time.Hour), End: tue.Add(10*time.Hour + 30*time.Minute),
		Labels: incident.Labels{"service": {"checkout"}}}}
	r := rules.Rule{Name: "R", For: 5 * time.Minute, Digest: "d", Graph: &rules.Graph{Queries: map[string]string{"A": "latency"}, Cond: "C",
		Steps: map[string]rules.Step{"C": {Threshold: &rules.Threshold{Input: "A", Type: "within_range", A: 0.9, B: 2}}}}}
	rs := []rules.Rule{r}
	h := &valued{}
	versions := []replay.Version{{Rules: rs}}
	_, fs := replay.Run(context.Background(), h, versions, o.From, o.To, time.Minute, 4)
	rep := verdict.Judge(rs, fs, incs, o)
	nms, done, err := NearMisses(context.Background(), h, versions, rep, incs, o, time.Minute, 4, nil)
	if err != nil || !done || len(nms) != 1 || nms[0].Value == nil {
		t.Fatalf("%v %v %+v", err, done, nms)
	}
	if v := nms[0].Value; v.Op != ">" || v.Bound != 0.9 || v.New != 0.87 || v.Reached == nil || *v.Reached != 0.88 || v.Of != "" {
		t.Errorf("%+v", v)
	}
	// With 0.87 it fires on INC-1, and on Thursday's 0.875, for nothing.
	if a := nms[0].After; a == nil || a.Firings != 2 || a.Black != 1 {
		t.Errorf("cost %+v", a)
	}
}

// A guard's number that holds more fires the rule earlier, before the
// incident, so the furthest catches nothing; a nearer one still does. And a
// firing the window's start makes up, its condition holding from long
// before, catches nothing.
func TestNearMissesOfSeveralBefore(t *testing.T) {
	o := verdict.Options{From: t0, To: t0.Add(7 * 24 * time.Hour), Lead: 15 * time.Minute, Match: []string{"service"},
		RefireWithin: time.Hour, UnknownLen: time.Hour}
	tue := t0.Add(24 * time.Hour)
	between := func(t time.Time, from, to time.Duration) bool {
		return !t.Before(tue.Add(from)) && t.Before(tue.Add(to))
	}
	for _, c := range []struct {
		name     string
		start    time.Duration // the incident's, on the Tuesday
		until    time.Duration // latency's 0.88, from 08:00
		interval time.Duration
		requests func(time.Time) float64
		new      float64 // 0 for none
	}{
		// 8.5 from 08:00, 9.5 from 10:00: with 8 it fires at 08:05, before
		// the incident at 10:00; with 9, at 10:05.
		{"climbing", 10 * time.Hour, 10*time.Hour + 30*time.Minute, 0, func(t time.Time) float64 {
			switch {
			case between(t, 10*time.Hour, 10*time.Hour+30*time.Minute):
				return 9.5
			case between(t, 8*time.Hour, 10*time.Hour):
				return 8.5
			}
			return 50
		}, 9},
		// 8.5 from 08:00 to 11:00, evaluated every 2m: with 8 it fires at
		// 08:06, long before the incident at 10:01.
		{"from long before", 10*time.Hour + time.Minute, 11 * time.Hour, 2 * time.Minute, func(t time.Time) float64 {
			if between(t, 8*time.Hour, 11*time.Hour) {
				return 8.5
			}
			return 50
		}, 0},
	} {
		latency := func(t time.Time) float64 {
			if between(t, 8*time.Hour, c.until) {
				return 0.88
			}
			return 0.1
		}
		h := &several{values: map[string]func(time.Time) float64{"latency": latency, "requests": c.requests}}
		incs := []incident.Incident{{ID: "INC-1", Start: tue.Add(c.start), End: tue.Add(c.start + 30*time.Minute),
			Labels: incident.Labels{"service": {"checkout"}}}}
		rs := []rules.Rule{{Name: "R", Expr: "latency > 0.85 and requests > 10", For: 5 * time.Minute, Interval: c.interval}}
		versions := []replay.Version{{Rules: rs}}
		_, fs := replay.Run(context.Background(), h, versions, o.From, o.To, time.Minute, 4)
		rep := verdict.Judge(rs, fs, incs, o)
		if !rep.Incidents[0].Missed() {
			t.Fatalf("%s: caught", c.name)
		}
		nms, done, err := NearMisses(context.Background(), h, versions, rep, incs, o, time.Minute, 4, nil)
		if err != nil || !done || (len(nms) == 1) != (c.new != 0) {
			t.Errorf("%s: %v %v %+v", c.name, err, done, nms)
			continue
		}
		if c.new == 0 {
			continue
		}
		if nm := nms[0]; nm.Value == nil || nm.Value.New != c.new || nm.Delay != 5*time.Minute || nm.After == nil || nm.After.White != 1 {
			t.Errorf("%s: %+v %+v", c.name, nm, nm.Value)
		}
	}
}
