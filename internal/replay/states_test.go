package replay

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
)

// grafanaRule is a Grafana rule on query x, with threshold x > 0, and what
// is written after its title.
func grafanaRule(t *testing.T, extra string) []Version {
	t.Helper()
	rs, err := rules.Parse([]byte(`apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    interval: 1m
    rules:
      - uid: u1
        title: R
        condition: B
` + extra + `
        data:
          - {refId: A, datasourceUid: prom, model: {expr: x, instant: true}}
          - refId: B
            datasourceUid: __expr__
            model: {type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [0]}}]}
`))
	if err != nil || len(rs) != 1 || rs[0].Unsupported != "" {
		t.Fatalf("%+v %v", rs, err)
	}
	return one(rs)
}

// at is a series with the given value at each minute from t0 it has one;
// NaN for a minute it hasn't.
func at(labels map[string]string, vs ...float64) prom.Series {
	s := prom.Series{Labels: labels}
	for i, v := range vs {
		if !math.IsNaN(v) {
			s.Samples = append(s.Samples, prom.Sample{T: t0.Add(time.Duration(i) * time.Minute), V: v})
		}
	}
	return s
}

// warm is s with v at each minute of the hour before t0: Grafana's alerts
// are worked out from before a period, so as to have those it had.
func warm(s prom.Series, v float64) prom.Series {
	var smp []prom.Sample
	for m := -60; m < 0; m++ {
		smp = append(smp, prom.Sample{T: t0.Add(time.Duration(m) * time.Minute), V: v})
	}
	return prom.Series{Labels: s.Labels, Samples: append(smp, s.Samples...)}
}

// spans writes firings as start-end in minutes from t0, with any label
// but the rule's, for comparing.
func spans(fs []firing.Firing) string {
	var out []string
	for _, f := range fs {
		end := "on"
		if !f.End.IsZero() {
			end = fmt.Sprint(int(f.End.Sub(t0) / time.Minute))
		}
		var ls []string
		for _, k := range []string{"pod", "datasource_uid", "ref_id", StateLabel} {
			if v, ok := f.Labels[k]; ok {
				ls = append(ls, k+"="+v)
			}
		}
		out = append(out, fmt.Sprintf("%d-%s%v", int(f.Start.Sub(t0)/time.Minute), end, ls))
	}
	return strings.Join(out, " ")
}

// gappy is x: 1 for minutes 0-4, nothing 5-9, 0 for 10-14, nothing 15-19.
func gappy() history {
	n := math.NaN()
	return history{"x": {warm(at(nil, 1, 1, 1, 1, 1, n, n, n, n, n, 0, 0, 0, 0, 0, n, n, n, n, n), 0)}}
}

// Grafana 13.3's state manager, noDataState by noDataState, as watched
// against a real Grafana (docs/design/states.md).
func TestNoDataStates(t *testing.T) {
	for _, c := range []struct{ state, for_, want string }{
		// Its own alert in the NoData state, the rule's other alert missing
		// two evaluations before it resolves.
		{"NoData", "0s", "0-6[] 5-10[datasource_uid=prom ref_id=A grafana_alertstate=nodata] 15-on[datasource_uid=prom ref_id=A grafana_alertstate=nodata]"},
		{"NoData", "2m", "2-6[] 7-10[datasource_uid=prom ref_id=A grafana_alertstate=nodata] 17-on[datasource_uid=prom ref_id=A grafana_alertstate=nodata]"},
		// The rule's alert holds on through no data, and is held from it
		// when it had been normal.
		{"Alerting", "0s", "0-10[] 15-on[]"},
		{"Alerting", "2m", "2-10[] 17-on[]"},
		// It lets the alert go at once.
		{"OK", "0s", "0-5[]"},
		// It keeps the alert as it was: firing through the first gap,
		// normal through the second.
		{"KeepLast", "0s", "0-10[]"},
	} {
		vs := grafanaRule(t, "        noDataState: "+c.state+"\n        for: "+c.for_)
		_, fs := Run(context.Background(), gappy(), vs, t0, t0.Add(20*time.Minute), time.Minute, 1)
		if got := spans(fs); got != c.want {
			t.Errorf("%s for %s:\n got %s\nwant %s", c.state, c.for_, got, c.want)
		}
	}
}

// With no alert to take it, a NoData result in Alerting is an alert of its
// own, with the data source's labels: here the query returned something in
// the day before, and nothing since until minute 3.
func TestNoDataFirst(t *testing.T) {
	x := func(t time.Time) (float64, bool) {
		m := t.Sub(t0) / time.Minute
		return 1, t.Sub(t0)%time.Minute == 0 && (m == 3 || m == 4)
	}
	h := stepped{"x": x, "count_over_time((x)[1d:5m])": func(time.Time) (float64, bool) { return 1, true }}
	vs := grafanaRule(t, "        noDataState: Alerting")
	_, fs := Run(context.Background(), h, vs, t0, t0.Add(6*time.Minute), time.Minute, 1)
	// The no data alert, missing at 3, goes after two evaluations, as it is
	// firing, not pending; the rule's own, once it has it, holds on.
	if got, want := spans(fs), "0-4[datasource_uid=prom ref_id=A] 3-on[]"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// Before a query has returned anything, in the replay or the day before,
// its history may just not be there (before the server's retention, before
// the metric was, or on another data source than the one read): nothing is
// taken to be NoData. As the review found it: a rule at the default
// noDataState, with "for", fired for all of a history that starts at
// minute 10.
func TestNoDataBeforeHistory(t *testing.T) {
	n := math.NaN()
	var vals []float64
	for i := range 21 {
		v := 0.0
		if i < 10 {
			v = n
		}
		vals = append(vals, v)
	}
	for _, state := range []string{"NoData", "Alerting"} {
		vs := grafanaRule(t, "        noDataState: "+state+"\n        for: 5m")
		_, fs := Run(context.Background(), history{"x": {at(nil, vals...)}}, vs, t0, t0.Add(20*time.Minute), time.Minute, 1)
		if len(fs) != 0 {
			t.Errorf("%s, history from minute 10: %s", state, spans(fs))
		}
	}
	// A query that returned nothing ever, beside one that did: the
	// condition reads only the one that did, which never holds.
	rs, err := rules.Parse([]byte(`apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    interval: 1m
    rules:
      - uid: o1
        title: Other
        condition: B
        data:
          - {refId: A, datasourceUid: prom, model: {expr: x, instant: true}}
          - {refId: Z, datasourceUid: loki-or-another, model: {expr: z, instant: true}}
          - refId: B
            datasourceUid: __expr__
            model: {type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [5]}}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	h := history{"x": {warm(at(nil, 1, 1, 1, 1, 1), 1)}, "z": nil}
	if _, fs := Run(context.Background(), h, one(rs), t0, t0.Add(4*time.Minute), time.Minute, 1); len(fs) != 0 {
		t.Errorf("a query with no history: %s", spans(fs))
	}
	// Once it has returned something, its nothing is NoData.
	h["z"] = []prom.Series{at(nil, 1, n, n, n, n)}
	if _, fs := Run(context.Background(), h, one(rs), t0, t0.Add(4*time.Minute), time.Minute, 1); spans(fs) != "1-on[datasource_uid=loki-or-another ref_id=Z grafana_alertstate=nodata]" {
		t.Errorf("a query that had history: %s", spans(fs))
	}
}

// A condition that comes to nothing, its queries having returned
// something, is NoData too, with no data source's labels; with no alert
// to take it, OK and KeepLast hold nothing.
func TestNoDataOther(t *testing.T) {
	rs, err := rules.Parse([]byte(`apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    interval: 1m
    rules:
      - uid: j1
        title: Join
        condition: C
        data:
          - {refId: A, datasourceUid: prom, model: {expr: a, instant: true}}
          - {refId: B, datasourceUid: prom, model: {expr: b, instant: true}}
          - {refId: C, datasourceUid: __expr__, model: {type: math, expression: '$A + $B'}}
`))
	if err != nil {
		t.Fatal(err)
	}
	pod := func(p string) map[string]string { return map[string]string{"pod": p} }
	h := history{"a": {warm(at(pod("x"), 1, 1, 1, 1), 1)}, "b": {warm(at(pod("y"), 1, 1, 1, 1), 1)}}
	_, fs := Run(context.Background(), h, one(rs), t0, t0.Add(3*time.Minute), time.Minute, 1)
	if got, want := spans(fs), "0-on[grafana_alertstate=nodata]"; got != want {
		t.Errorf("nothing joins: got %s, want %s", got, want)
	}
	n := math.NaN()
	never := history{"x": {at(nil, n, n, n, n, n)}}
	for _, state := range []string{"OK", "KeepLast"} {
		if _, fs := Run(context.Background(), never, grafanaRule(t, "        noDataState: "+state), t0, t0.Add(4*time.Minute), time.Minute, 1); len(fs) != 0 {
			t.Errorf("%s, never any data: %s", state, spans(fs))
		}
	}
	// A period starting in a stretch of no data: the alert Grafana had
	// before it holds on, worked out from the hour before.
	_, fs = Run(context.Background(), gappy(), grafanaRule(t, "        noDataState: Alerting"), t0.Add(6*time.Minute), t0.Add(12*time.Minute), time.Minute, 1)
	if got, want := spans(fs), "6-10[]"; got != want {
		t.Errorf("from within no data: got %s, want %s", got, want)
	}
}

// An alert whose series has gone stays as it was for
// missing_series_evals_to_resolve evaluations, 2 when not set.
func TestMissingSeries(t *testing.T) {
	n := math.NaN()
	h := history{"x": {
		at(map[string]string{"pod": "a"}, 1, 1, 1, 1, 1, 1, 1, 1),
		at(map[string]string{"pod": "b"}, 1, 1, 1, n, n, n, 1, 1),
	}}
	for extra, want := range map[string]string{
		"": "0-on[pod=a] 0-4[pod=b] 6-on[pod=b]",
		"        missing_series_evals_to_resolve: 1": "0-on[pod=a] 0-3[pod=b] 6-on[pod=b]",
		"        missing_series_evals_to_resolve: 4": "0-on[pod=a] 0-on[pod=b]",
	} {
		_, fs := Run(context.Background(), h, grafanaRule(t, "        noDataState: OK\n"+extra), t0, t0.Add(7*time.Minute), time.Minute, 1)
		if got := spans(fs); got != want {
			t.Errorf("%q: got %s, want %s", extra, got, want)
		}
	}
	h["x"][1] = at(map[string]string{"pod": "b"}, 1, 1, 1, n, n, n, n, n)
	_, fs := Run(context.Background(), h, grafanaRule(t, "        noDataState: OK"), t0, t0.Add(7*time.Minute), time.Minute, 1)
	if got, want := spans(fs), "0-on[pod=a] 0-4[pod=b]"; got != want {
		t.Errorf("gone for good: got %s, want %s", got, want)
	}
}

// Anything but 0 is Alerting, NaN too, as Grafana's evaluation reads a
// condition's value; nothing at all is NoData.
func TestNaNAndNothing(t *testing.T) {
	rs, err := rules.Parse([]byte(`apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    interval: 1m
    rules:
      - uid: u2
        title: Ratio
        condition: C
        noDataState: Alerting
        data:
          - {refId: A, datasourceUid: prom, model: {expr: a, instant: true}}
          - {refId: B, datasourceUid: prom, model: {expr: b, instant: true}}
          - {refId: C, datasourceUid: __expr__, model: {type: math, expression: '$A / $B'}}
`))
	if err != nil {
		t.Fatal(err)
	}
	pod := func(p string) map[string]string { return map[string]string{"pod": p} }
	h := history{
		"a": {warm(at(pod("x"), 0, 0, 0, 0), 0), warm(at(pod("y"), 0, 0, 0, 0), 0)},
		// x: 0/1, then 0/0; y: in b under another pod, so nothing joins.
		"b": {warm(at(pod("x"), 1, 1, 0, 0), 1), warm(at(pod("z"), 1, 1, 1, 1), 1)},
	}
	_, fs := Run(context.Background(), h, one(rs), t0, t0.Add(4*time.Minute), time.Minute, 1)
	if got, want := spans(fs), "2-on[pod=x]"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// A query that returns nothing makes the rule NoData even when its
// condition doesn't use it, as Grafana asks every query.
func TestNoDataUnusedQuery(t *testing.T) {
	rs, err := rules.Parse([]byte(`apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    interval: 1m
    rules:
      - uid: u3
        title: Unused
        condition: B
        data:
          - {refId: A, datasourceUid: prom, model: {expr: x, instant: true}}
          - {refId: Z, datasourceUid: other, model: {expr: z, instant: true}}
          - refId: B
            datasourceUid: __expr__
            model: {type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [5]}}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	n := math.NaN()
	h := history{"x": {at(nil, 1, 1, 1, 1, 1)}, "z": {at(nil, 1, n, n, 1, 1)}}
	_, fs := Run(context.Background(), h, one(rs), t0, t0.Add(4*time.Minute), time.Minute, 1)
	if got, want := spans(fs), "1-3[datasource_uid=other ref_id=Z grafana_alertstate=nodata]"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// Classic conditions, evaluated as Grafana's: diff over a window, the
// oldest number asked for where the window has none at its start.
func TestClassicReplay(t *testing.T) {
	rs, err := rules.Parse([]byte(`apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    interval: 1m
    rules:
      - uid: c1
        title: Jump
        condition: C
        noDataState: OK
        data:
          - {refId: A, datasourceUid: prom, relativeTimeRange: {from: 120, to: 0}, model: {expr: x, intervalMs: 60000}}
          - refId: C
            datasourceUid: __expr__
            model:
              type: classic_conditions
              conditions:
                - {evaluator: {type: gt, params: [10]}, operator: {type: and}, query: {params: [A]}, reducer: {type: diff}}
`))
	if err != nil || rs[0].Unsupported != "" {
		t.Fatal(rs, err)
	}
	c := rs[0].Graph.Steps["C"].Classic.Conds[0]
	asks := rs[0].Graph.Asks
	n := math.NaN()
	// x, by minute: appears at 2, then climbs; the replay asks what the
	// server would answer: the newest number in each window, the number at
	// its start, which series there were; and, where a window has none at
	// its start, the window's points.
	h := history{
		asks[c.Value].PromQL(0):   {at(nil, n, n, 5, 20, 30, 31, 31)},
		asks[c.Oldest].PromQL(0):  {at(nil, n, n, n, n, 5, 20, 30)},
		asks[c.Present].PromQL(0): {at(nil, n, n, 1, 2, 3, 3, 3)},
		"((x) >= -Inf)":           {at(nil, n, n, 5, 20, 30, 31, 31)},
	}
	_, fs := Run(context.Background(), h, one(rs), t0, t0.Add(7*time.Minute), time.Minute, 1)
	// 2: 5-5 = 0 (one number); 3: 20-5 = 15, the 5 from the window's
	// points; 4: 30-5 = 25; 5: 31-20 = 11; 6: 31-30 = 1.
	if got, want := spans(fs), "3-6[]"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	// Series there, but none with a number to reduce: no data, from the
	// classic conditions, with no data source's labels.
	rs, err = rules.Parse([]byte(`apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    interval: 1m
    rules:
      - uid: c2
        title: Avg
        condition: C
        data:
          - {refId: A, datasourceUid: prom, relativeTimeRange: {from: 120, to: 0}, model: {expr: y, intervalMs: 60000}}
          - refId: C
            datasourceUid: __expr__
            model:
              type: classic_conditions
              conditions:
                - {evaluator: {type: gt, params: [10]}, operator: {type: and}, query: {params: [A]}, reducer: {type: avg}}
`))
	if err != nil {
		t.Fatal(err)
	}
	c = rs[0].Graph.Steps["C"].Classic.Conds[0]
	asks = rs[0].Graph.Asks
	h = history{asks[c.Value].PromQL(0): {warm(at(nil, 1, n, n, 1, 1), 1)}, asks[c.Present].PromQL(0): {warm(at(nil, 2, 2, 2, 2, 2), 2)}}
	_, fs = Run(context.Background(), h, one(rs), t0, t0.Add(4*time.Minute), time.Minute, 1)
	if got, want := spans(fs), "1-3[grafana_alertstate=nodata]"; got != want {
		t.Errorf("no number: got %s, want %s", got, want)
	}
}

// A query whose window ends before the evaluation (relativeTimeRange's
// to), a Prometheus group's query_offset, and --late each read the history
// that much earlier.
func TestShifted(t *testing.T) {
	n := math.NaN()
	h := history{"x": {at(nil, n, n, 1, 1, 1, n, n, n, n, n)}}
	rs, err := rules.Parse([]byte("groups:\n  - name: g\n    query_offset: 2m\n    rules:\n      - {alert: P, expr: x}\n"))
	if err != nil {
		t.Fatal(err)
	}
	_, fs := Run(context.Background(), h, one(rs), t0, t0.Add(10*time.Minute), time.Minute, 1)
	if got, want := spans(fs), "4-7[]"; got != want {
		t.Errorf("query_offset: got %s, want %s", got, want)
	}
	// What held before the period, read at its start.
	early := stepped{"x": func(t time.Time) (float64, bool) { return 1, t.Before(t0) && !t.Before(t0.Add(-2*time.Minute)) }}
	_, fs = Run(context.Background(), early, one(rs), t0, t0.Add(10*time.Minute), time.Minute, 1)
	if got, want := spans(fs), "0-2[]"; got != want {
		t.Errorf("query_offset from before: got %s, want %s", got, want)
	}
	// Late, when longer, reads earlier still; when shorter, the offset holds.
	for late, want := range map[time.Duration]string{3 * time.Minute: "5-8[]", time.Minute: "4-7[]"} {
		r := rs[0]
		r.Late = late
		_, fs = Run(context.Background(), h, one([]rules.Rule{r}), t0, t0.Add(10*time.Minute), time.Minute, 1)
		if got := spans(fs); got != want {
			t.Errorf("late %s: got %s, want %s", late, got, want)
		}
	}
	vs := grafanaRule(t, "        noDataState: OK")
	g := vs[0].Rules[0]
	g.Late = 2 * time.Minute
	_, fs = Run(context.Background(), h, one([]rules.Rule{g}), t0, t0.Add(10*time.Minute), time.Minute, 1)
	if got, want := spans(fs), "4-7[]"; got != want {
		t.Errorf("grafana, late: got %s, want %s", got, want)
	}
}

// stepped answers only at the steps asked for: start, then every step.
type stepped map[string]func(time.Time) (float64, bool)

func (h stepped) QueryRange(_ context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	f := h[expr]
	s := prom.Series{Labels: map[string]string{}}
	for t := start; !t.After(end); t = t.Add(step) {
		if v, ok := f(t); ok {
			s.Samples = append(s.Samples, prom.Sample{T: t, V: v})
		}
	}
	return []prom.Series{s}, nil
}

// A group placed 30s into its minute, as the rules API's last evaluation
// says, is evaluated there: it sees a window of 20s that one on the
// minute misses.
func TestPlaced(t *testing.T) {
	api := `{"status":"success","data":{"groups":[{"name":"g","file":"f.yaml","interval":60,"lastEvaluation":"2026-11-11T12:40:30.123456Z",
	  "rules":[{"type":"alerting","name":"Spike","query":"spike","duration":0}]}]}}`
	rs, err := rules.FromAPI([]byte(api))
	if err != nil || len(rs) != 1 || rs[0].Phase != 30*time.Second {
		t.Fatalf("%+v %v", rs, err)
	}
	// spike holds from 20s to 40s past minutes 2 to 4.
	h := stepped{"spike": func(t time.Time) (float64, bool) {
		m, s := t.Sub(t0)/time.Minute, t.Sub(t0)%time.Minute
		return 1, m >= 2 && m <= 4 && s >= 20*time.Second && s < 40*time.Second
	}}
	_, fs := Run(context.Background(), h, one(rs), t0, t0.Add(10*time.Minute), time.Minute, 1)
	if len(fs) != 1 || !fs[0].Start.Equal(t0.Add(2*time.Minute+30*time.Second)) || !fs[0].End.Equal(t0.Add(5*time.Minute+30*time.Second)) {
		t.Errorf("placed: %+v", fs)
	}
	// With a five-minute group, every fifth step, past the phase.
	five := rs[0]
	five.Interval, five.Phase = 5*time.Minute, 2*time.Minute+30*time.Second
	_, fs = Run(context.Background(), h, one([]rules.Rule{five}), t0, t0.Add(11*time.Minute), time.Minute, 1)
	if len(fs) != 1 || !fs[0].Start.Equal(t0.Add(2*time.Minute+30*time.Second)) || !fs[0].End.Equal(t0.Add(7*time.Minute+30*time.Second)) {
		t.Errorf("five minutes: %+v", fs)
	}
	plain := rs[0]
	plain.Phase = 0
	if _, fs = Run(context.Background(), h, one([]rules.Rule{plain}), t0, t0.Add(10*time.Minute), time.Minute, 1); len(fs) != 0 {
		t.Errorf("on the minute: %+v", fs)
	}
}
