package replay

import (
	"context"
	"errors"
	"slices"
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
	fs := Run(context.Background(), h, rs, t0, t0.Add(9*time.Minute), time.Minute)
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
	fs := Run(context.Background(), h, rs, t0, t0.Add(10*time.Minute), time.Minute)
	if len(fs) != 1 || fs[0].Rule != "Down" || !fs[0].Start.Equal(t0) {
		t.Errorf("firings %+v", fs)
	}
	if rs[1].Unsupported == "" || rs[2].Unsupported != "classic_conditions expressions" {
		t.Errorf("a failed query should mark its rule, and only its rule: %+v", rs)
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
	if got := at(Run(context.Background(), h, rs, t0, t0.Add(5*time.Minute), time.Minute)); !slices.Equal(got, []int{2, 3}) {
		t.Errorf("instant: firing minutes %v, want [2 3]", got)
	}
	// Range inputs over 3m: the mean over the window is 1/3 at minutes 2, 3
	// and 4, under the 0.5 threshold, so nothing fires.
	rs, _ = rules.Parse(doc("false"))
	if w := rs[0].Graph.Steps["D"].Reduce.Window; w != 3*time.Minute {
		t.Fatalf("window %s", w)
	}
	if fs := Run(context.Background(), h, rs, t0, t0.Add(5*time.Minute), time.Minute); len(fs) != 0 {
		t.Errorf("range: %+v, want no firing", fs)
	}
}
