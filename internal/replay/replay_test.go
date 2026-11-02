package replay

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
)

var t0 = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

// history serves canned series by expression, over the requested range.
type history map[string][]prom.Series

func (h history) QueryRange(_ context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	s, ok := h[expr]
	if !ok {
		return nil, errors.New("unknown expression " + expr)
	}
	var out []prom.Series
	for _, x := range s {
		y := prom.Series{Labels: x.Labels}
		for _, smp := range x.Samples {
			if !smp.T.Before(start) && !smp.T.After(end) {
				y.Samples = append(y.Samples, smp)
			}
		}
		out = append(out, y)
	}
	return out, nil
}

// values makes one series with a value per minute from t0.
func values(labels map[string]string, vs ...float64) prom.Series {
	s := prom.Series{Labels: labels}
	for i, v := range vs {
		s.Samples = append(s.Samples, prom.Sample{T: t0.Add(time.Duration(i) * time.Minute), V: v})
	}
	return s
}

func TestGrafanaRule(t *testing.T) {
	// "errors when the volume gate is open": $A * ($B >= 20), threshold > 5.
	rs, err := rules.Parse([]byte(`apiVersion: 1
groups:
  - name: g
    rules:
      - title: Gated errors
        condition: C
        for: 2m
        data:
          - {refId: A, datasourceUid: gmp, model: {expr: errors, instant: true}}
          - {refId: B, datasourceUid: gmp, model: {expr: volume, instant: true}}
          - {refId: M, datasourceUid: __expr__, model: {type: math, expression: '$A * ($B >= 20)'}}
          - refId: C
            datasourceUid: __expr__
            model: {type: threshold, expression: M, conditions: [{evaluator: {type: gt, params: [5]}}]}
`))
	if err != nil || rs[0].Graph == nil {
		t.Fatalf("%+v %v", rs, err)
	}
	svc := map[string]string{"service": "checkout"}
	h := history{
		// errors are high from minute 2 to 7; volume is high only from 4
		"errors": {values(svc, 0, 0, 9, 9, 9, 9, 9, 9, 0, 0)},
		"volume": {values(svc, 5, 5, 5, 5, 30, 30, 30, 30, 30, 30)},
	}
	_, fs := Run(context.Background(), h, one(rs), t0, t0.Add(9*time.Minute), time.Minute)
	if len(fs) != 1 {
		t.Fatalf("firings %+v", fs)
	}
	// The condition holds from minute 4 to 7; with for: 2m it fires at 6 and
	// resolves at 8.
	if !fs[0].Start.Equal(t0.Add(6*time.Minute)) || !fs[0].End.Equal(t0.Add(8*time.Minute)) || fs[0].Labels["service"] != "checkout" {
		t.Errorf("firing %+v", fs[0])
	}
}

func TestPrometheusRuleAndFailure(t *testing.T) {
	rs := []rules.Rule{
		{Name: "Down", Expr: "up == 0"},
		{Name: "Typo", Expr: "up =="},
		{Name: "Skipped", Unsupported: "classic_conditions expressions"},
	}
	h := history{"up == 0": {values(map[string]string{"job": "api"}, 0, 0, 0)}}
	ran, fs := Run(context.Background(), h, one(rs), t0, t0.Add(10*time.Minute), time.Minute)
	if len(fs) != 1 || fs[0].Rule != "Down" || !fs[0].Start.Equal(t0) {
		t.Errorf("firings %+v", fs)
	}
	if len(ran) != 3 || ran[0].Unsupported != "" || ran[1].Unsupported == "" || ran[2].Unsupported != "classic_conditions expressions" {
		t.Errorf("a failed query should mark its rule, and only its rule: %+v", ran)
	}
}

func TestReduceOverMath(t *testing.T) {
	doc := func(instant string) []byte {
		return []byte(`apiVersion: 1
groups:
  - name: g
    rules:
      - title: Replica gap
        condition: E
        data:
          - {refId: A, datasourceUid: gmp, relativeTimeRange: {from: 180, to: 0}, model: {expr: available, instant: ` + instant + `}}
          - {refId: B, datasourceUid: gmp, relativeTimeRange: {from: 180, to: 0}, model: {expr: wanted, instant: ` + instant + `}}
          - {refId: C, datasourceUid: __expr__, model: {type: math, expression: '$B - $A'}}
          - {refId: D, datasourceUid: __expr__, model: {type: reduce, expression: C, reducer: mean}}
          - refId: E
            datasourceUid: __expr__
            model: {type: threshold, expression: D, conditions: [{evaluator: {type: gt, params: [0.5]}}]}
`)
	}
	deploy := map[string]string{"deployment": "api"}
	h := history{
		"available": {values(deploy, 3, 3, 2, 3, 3, 3)}, // one replica short at minute 2 only
		"wanted":    {values(deploy, 3, 3, 3, 3, 3, 3)},
	}
	at := func(fs []firing.Firing) []int {
		var out []int
		for _, f := range fs {
			out = append(out, int(f.Start.Sub(t0)/time.Minute), int(f.End.Sub(t0)/time.Minute))
		}
		return out
	}
	// Instant inputs: the mean of one value is the value, so the gap shows
	// at minute 2 alone.
	rs, err := rules.Parse(doc("true"))
	if err != nil || rs[0].Graph.Steps["D"].Reduce == nil || rs[0].Graph.Steps["D"].Reduce.Window != 0 {
		t.Fatalf("%+v %v", rs, err)
	}
	if _, fs := Run(context.Background(), h, one(rs), t0, t0.Add(5*time.Minute), time.Minute); !slices.Equal(at(fs), []int{2, 3}) {
		t.Errorf("instant: firing minutes %v, want [2 3]", at(fs))
	}
	// Range inputs over 3m: the mean over the window is 1/3 at minutes 2, 3
	// and 4, under the 0.5 threshold, so nothing fires.
	rs, _ = rules.Parse(doc("false"))
	if w := rs[0].Graph.Steps["D"].Reduce.Window; w != 3*time.Minute {
		t.Fatalf("window %s", w)
	}
	if _, fs := Run(context.Background(), h, one(rs), t0, t0.Add(5*time.Minute), time.Minute); len(fs) != 0 {
		t.Errorf("range: %+v, want no firing", fs)
	}
	// A replay that starts at minute 2 still sees minutes 0 and 1 in the
	// window: the mean at 2 is 1/3 as before, not the 1 of a window cut off
	// where the replay starts.
	if _, fs := Run(context.Background(), h, one(rs), t0.Add(2*time.Minute), t0.Add(5*time.Minute), time.Minute); len(fs) != 0 {
		t.Errorf("range from minute 2: %+v, want no firing", fs)
	}
}

func one(rs []rules.Rule) []Version { return []Version{{Rules: rs}} }

// minutes gives each firing as its start and end, in minutes from t0; -1
// for an end it hasn't got.
func minutes(fs []firing.Firing) [][2]int {
	var out [][2]int
	for _, f := range fs {
		end := -1
		if !f.End.IsZero() {
			end = int(f.End.Sub(t0) / time.Minute)
		}
		out = append(out, [2]int{int(f.Start.Sub(t0) / time.Minute), end})
	}
	return out
}

func TestVersions(t *testing.T) {
	svc := map[string]string{"service": "checkout"}
	ones := func(from, to int) prom.Series {
		vs := make([]float64, to+1)
		for i := from; i <= to; i++ {
			vs[i] = 1
		}
		s := values(svc, vs...)
		s.Samples = slices.DeleteFunc(s.Samples, func(x prom.Sample) bool { return x.V == 0 })
		return s
	}
	h := history{
		"errors > 5": {ones(10, 49)},
		"errors > 1": {ones(10, 49)},
		"late":       {ones(28, 49)},
		"none":       nil,
	}
	rule := func(name, expr string, forDur time.Duration) rules.Rule {
		return rules.Rule{Name: name, Group: "shop", Expr: expr, For: forDur}
	}
	// The rules change at minute 30 (at 29:20, taking effect at the next
	// step), and the period is minutes 0 to 59.
	change := t0.Add(29*time.Minute + 20*time.Second)
	for _, c := range []struct {
		name         string
		before, from []rules.Rule
		want         [][2]int
	}{
		{"unchanged: one firing, not two",
			[]rules.Rule{rule("Errors", "errors > 5", 5*time.Minute)},
			[]rules.Rule{rule("Errors", "errors > 5", 5*time.Minute)},
			[][2]int{{15, 50}}},
		{"added: pending from when it took effect",
			[]rules.Rule{rule("Other", "none", 0)},
			[]rules.Rule{rule("Other", "none", 0), rule("Errors", "errors > 5", 5*time.Minute)},
			[][2]int{{35, 50}}},
		{"taken away: resolved then",
			[]rules.Rule{rule("Errors", "errors > 5", 5*time.Minute)},
			[]rules.Rule{rule("Other", "none", 0)},
			[][2]int{{15, 30}}},
		{"Prometheus rule changed: the alert carries over",
			[]rules.Rule{rule("Errors", "errors > 5", 5*time.Minute)},
			[]rules.Rule{rule("Errors", "errors > 1", 5*time.Minute)},
			[][2]int{{15, 50}}},
		{"Prometheus rule changed while pending: pending carries over, for the new for",
			[]rules.Rule{rule("Late", "late", 10*time.Minute)},
			[]rules.Rule{rule("Late", "late", 3*time.Minute)},
			[][2]int{{31, 50}}},
		{"labels changed: a different alert",
			[]rules.Rule{rule("Errors", "errors > 5", 5*time.Minute)},
			[]rules.Rule{func() rules.Rule {
				r := rule("Errors", "errors > 5", 5*time.Minute)
				r.Labels = map[string]string{"severity": "page"}
				return r
			}()},
			[][2]int{{15, 30}, {35, 50}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, fs := Run(context.Background(), h, []Version{{Rules: c.before}, {From: change, Rules: c.from}}, t0, t0.Add(59*time.Minute), time.Minute)
			if got := minutes(fs); !slices.Equal(got, c.want) {
				t.Errorf("firings %v, want %v", got, c.want)
			}
		})
	}
}

func TestGrafanaVersions(t *testing.T) {
	doc := func(threshold string, paused bool) []rules.Rule {
		rs, err := rules.Parse([]byte(`apiVersion: 1
groups:
  - name: g
    rules:
      - uid: errors-1
        title: Errors
        isPaused: ` + strconv.FormatBool(paused) + `
        condition: C
        for: 5m
        data:
          - {refId: A, datasourceUid: gmp, model: {expr: errors, instant: true}}
          - refId: C
            datasourceUid: __expr__
            model: {type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [` + threshold + `]}}]}
`))
		if err != nil {
			t.Fatal(err)
		}
		return rs
	}
	vs := make([]float64, 60)
	for i := 10; i < 50; i++ {
		vs[i] = 9
	}
	h := history{"errors": {values(map[string]string{"service": "checkout"}, vs...)}}
	change := t0.Add(30 * time.Minute)
	for _, c := range []struct {
		name      string
		threshold string
		paused    bool
		want      [][2]int
	}{
		{"unchanged: carries on", "5", false, [][2]int{{15, 50}}},
		// Grafana resets a rule's state when the rule is edited.
		{"edited: resolved, then pending afresh", "6", false, [][2]int{{15, 30}, {35, 50}}},
		{"paused: resolved, and replayed until then", "5", true, [][2]int{{15, 30}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ran, fs := Run(context.Background(), h, []Version{{Rules: doc("5", false)}, {From: change, Rules: doc(c.threshold, c.paused)}}, t0, t0.Add(59*time.Minute), time.Minute)
			if got := minutes(fs); !slices.Equal(got, c.want) {
				t.Errorf("firings %v, want %v", got, c.want)
			}
			for _, r := range ran {
				if r.Unsupported != "" {
					t.Errorf("%s: %s, want it replayed", r.Name, r.Unsupported)
				}
			}
		})
	}
}
