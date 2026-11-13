package replay

import (
	"context"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
)

// What the review before release found, each kept as a test.

// parsed is the one rule in a document, as the only version.
func parsed(t *testing.T, y string) []Version {
	t.Helper()
	rs, err := rules.Parse([]byte(y))
	if err != nil || len(rs) != 1 || rs[0].Unsupported != "" {
		t.Fatalf("%+v %v", rs, err)
	}
	return one(rs)
}

const keepRule = `apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    interval: 1m
    rules:
      - uid: k1
        title: K
        condition: B
        keepFiringFor: 5m
        noDataState: NoData
        data:
          - {refId: A, datasourceUid: prom, model: {expr: x, instant: true}}
          - refId: B
            datasourceUid: __expr__
            model: {type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [0]}}]}
`

// Grafana's state manager moves an alert only on a result: a pending alert
// whose series has gone never becomes Alerting while gone; once stale it
// is Normal, with no Recovering, so keep_firing_for doesn't hold it; an
// alert in the NoData state is let go the same way.
func TestStatesWhenMissing(t *testing.T) {
	n := math.NaN()
	pods := func(a []float64) history {
		return history{"x": {
			warm(at(map[string]string{"pod": "a"}, a...), 0),
			warm(at(map[string]string{"pod": "b"}, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0), 0),
		}}
	}
	// Pending at 0, missing at 1, stale at 2: it never fires.
	_, fs := Run(context.Background(), pods([]float64{1, n, n, n, n, n, n, n}), grafanaRule(t, "        for: 1m"), t0, t0.Add(7*time.Minute), time.Minute, 1)
	if got := spans(fs); got != "" {
		t.Errorf("pending, then gone: %s", got)
	}
	// Pending at 0, missing at 1, back at 2 with its minute up: fires at 2,
	// and resolves when stale, at 5.
	_, fs = Run(context.Background(), pods([]float64{1, n, 1, 1, n, n, n, n}), grafanaRule(t, "        for: 1m"), t0, t0.Add(7*time.Minute), time.Minute, 1)
	if got := spans(fs); got != "2-5[pod=a]" {
		t.Errorf("pending, gone, back: %s", got)
	}
	// Firing 0-2, gone from 3, stale at 4: Normal, keep_firing_for or not.
	_, fs = Run(context.Background(), pods([]float64{1, 1, 1, n, n, n, n, n, n, n, n, n, n, n, n}), parsed(t, keepRule), t0, t0.Add(14*time.Minute), time.Minute, 1)
	if got := spans(fs); got != "0-4[pod=a]" {
		t.Errorf("keep_firing_for, gone: %s", got)
	}
	// NoData from 0, data again at 5: the NoData alert resolves then.
	h := history{"x": {warm(at(nil, n, n, n, n, n, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0), 0)}}
	_, fs = Run(context.Background(), h, parsed(t, keepRule), t0, t0.Add(14*time.Minute), time.Minute, 1)
	if got := spans(fs); got != "0-5[datasource_uid=prom ref_id=A grafana_alertstate=nodata]" {
		t.Errorf("keep_firing_for, NoData: %s", got)
	}
	// keep_firing_for does hold an alert whose value goes to 0: Recovering.
	h = history{"x": {warm(at(nil, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0), 0)}}
	_, fs = Run(context.Background(), h, parsed(t, keepRule), t0, t0.Add(14*time.Minute), time.Minute, 1)
	if got := spans(fs); got != "0-7[]" {
		t.Errorf("keep_firing_for, recovering: %s", got)
	}
}

const diffRule = `apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    interval: 1m
    rules:
      - uid: c1
        title: Jump
        condition: C
        data:
          - {refId: A, datasourceUid: prom, relativeTimeRange: {from: 120, to: 0}, model: {expr: x, intervalMs: 60000}}
          - refId: C
            datasourceUid: __expr__
            model:
              type: classic_conditions
              conditions:
                - {evaluator: {type: gt, params: [10]}, operator: {type: and}, query: {params: [A]}, reducer: {type: diff}}
`

// failingWindow fails to answer for a window's points, as a server under
// load does.
type failingWindow struct{ history }

func (f failingWindow) QueryRange(ctx context.Context, expr string, s, e time.Time, step time.Duration) ([]prom.Series, error) {
	if expr == "((x) >= -Inf)" {
		return nil, context.DeadlineExceeded
	}
	return f.history.QueryRange(ctx, expr, s, e, step)
}

// A window's first numbers that couldn't be asked for fail the rule's
// replay, rather than reading as no data.
func TestFirstsFailing(t *testing.T) {
	vs := parsed(t, diffRule)
	r := vs[0].Rules[0]
	c := r.Graph.Steps["C"].Classic.Conds[0]
	asks := r.Graph.Asks
	n := math.NaN()
	h := history{
		asks[c.Value].PromQL(0):   {at(nil, n, n, 5, 20, 30, 31, 31)},
		asks[c.Oldest].PromQL(0):  {at(nil, n, n, n, n, 5, 20, 30)},
		asks[c.Present].PromQL(0): {at(nil, n, n, 1, 2, 3, 3, 3)},
		"((x) >= -Inf)":           {at(nil, n, n, 5, 20, 30, 31, 31)},
	}
	ran, fs := Run(context.Background(), failingWindow{h}, vs, t0, t0.Add(7*time.Minute), time.Minute, 1)
	if len(fs) != 0 || !strings.HasPrefix(ran[0].Unsupported, "query failed for now: the first numbers of A's window") {
		t.Errorf("%s, %q", spans(fs), ran[0].Unsupported)
	}
}

// windowCount counts requests, and the points asked of the window's.
type windowCount struct {
	history
	calls, firsts, points atomic.Int64
}

func (c *windowCount) QueryRange(ctx context.Context, expr string, s, e time.Time, step time.Duration) ([]prom.Series, error) {
	c.calls.Add(1)
	if expr == "((x) >= -Inf)" {
		c.firsts.Add(1)
		c.points.Add(int64(e.Sub(s)/step) + 1)
		return nil, nil
	}
	return c.history.QueryRange(ctx, expr, s, e, step)
}

// A diff over an hour, on series younger than the window at every
// evaluation: a window's first numbers are asked for once a window, not
// once an evaluation (a day was 1,441 requests, 5.19M points).
func TestFirstsAskedOnce(t *testing.T) {
	y := strings.Replace(strings.Replace(diffRule, "from: 120", "from: 3600", 1), "intervalMs: 60000", "intervalMs: 1000", 1)
	vs := parsed(t, y)
	r := vs[0].Rules[0]
	c := r.Graph.Steps["C"].Classic.Conds[0]
	asks := r.Graph.Asks
	val, nan := make([]float64, 24*60+1), make([]float64, 24*60+1)
	for i := range val {
		val[i], nan[i] = 1, math.NaN()
	}
	h := &windowCount{history: history{
		asks[c.Value].PromQL(0):   {at(nil, val...)},
		asks[c.Oldest].PromQL(0):  {at(nil, nan...)},
		asks[c.Present].PromQL(0): {at(nil, val...)},
	}}
	Run(context.Background(), h, vs, t0, t0.Add(24*time.Hour), time.Minute, 1)
	if n := h.firsts.Load(); n > 26 {
		t.Errorf("a window's first numbers asked for %d times over a day of an hour's windows (%d points)", n, h.points.Load())
	}
}

// byExpr counts the requests that reach the history, by expression.
type byExpr struct {
	history
	mu sync.Mutex
	n  map[string]int
}

func (b *byExpr) QueryRange(ctx context.Context, expr string, s, e time.Time, step time.Duration) ([]prom.Series, error) {
	b.mu.Lock()
	b.n[expr]++
	b.mu.Unlock()
	return b.history.QueryRange(ctx, expr, s, e, step)
}

// Grafana rules alike in several sets, as serve's hourly comparisons and
// --compare have them, share their requests mid-day too.
func TestGrafanaShared(t *testing.T) {
	vs := grafanaRule(t, "        noDataState: NoData")
	var vals []float64
	for range 240 {
		vals = append(vals, 1)
	}
	h := &byExpr{history: history{"x": {warm(at(nil, vals...), 0)}}, n: map[string]int{}}
	RunEach(context.Background(), h, [][]Version{vs, vs, vs, vs}, t0.Add(2*time.Hour), t0.Add(3*time.Hour), time.Minute, 1)
	if h.n["x"] != 1 {
		t.Errorf("4 sets, an hour mid-day: %d requests for x", h.n["x"])
	}
}

func missingRule(t *testing.T, interval, missing string) []Version {
	return parsed(t, `apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    interval: `+interval+`
    rules:
      - uid: m1
        title: M
        condition: B
        missing_series_evals_to_resolve: `+missing+`
        data:
          - {refId: A, datasourceUid: prom, model: {expr: x, instant: true}}
          - refId: B
            datasourceUid: __expr__
            model: {type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [0]}}]}
`)
}

// However many evaluations missing_series_evals_to_resolve asks to wait,
// the states are worked out from a day before at most; and its interval
// times it doesn't overflow into no evaluation at all.
func TestWarmupBounded(t *testing.T) {
	var vals []float64
	for range 61 {
		vals = append(vals, 1)
	}
	for _, n := range []string{"100000", "1000000"} {
		h := &byExpr{history: history{"x": {at(nil, vals...)}}, n: map[string]int{}}
		Run(context.Background(), h, missingRule(t, "1m", n), t0, t0.Add(time.Hour), time.Minute, 1)
		if h.n["x"] > 2 {
			t.Errorf("missing %s: %d requests for an hour", n, h.n["x"])
		}
	}
	var days []prom.Sample
	for d := -3; d <= 3; d++ {
		days = append(days, prom.Sample{T: t0.Add(time.Duration(d) * 24 * time.Hour), V: 1})
	}
	h := &byExpr{history: history{"x": {{Samples: days}}}, n: map[string]int{}}
	_, fs := Run(context.Background(), h, missingRule(t, "24h", "1000000"), t0, t0.Add(72*time.Hour), 24*time.Hour, 1)
	if len(fs) != 1 || h.n["x"] == 0 {
		t.Errorf("a day's interval: %d requests, firings %s", h.n["x"], spans(fs))
	}
}

// percent_diff of a window holding one number, 0: Grafana's calculateDiff
// leaves it at 0, which passes lt 5; two numbers of 0 divide by nothing.
func TestPercentDiffAlone(t *testing.T) {
	y := strings.Replace(strings.Replace(diffRule, "type: diff}", "type: percent_diff}", 1), "type: gt, params: [10]", "type: lt, params: [5]", 1)
	vs := parsed(t, y)
	r := vs[0].Rules[0]
	c := r.Graph.Steps["C"].Classic.Conds[0]
	asks := r.Graph.Asks
	h := history{
		asks[c.Value].PromQL(0):   {warm(at(nil, 0, 0, 0, 0), 0)},
		asks[c.Oldest].PromQL(0):  {warm(at(nil, 0, 0, 0, 0), 0)},
		asks[c.Present].PromQL(0): {warm(at(nil, 1, 1, 1, 1), 1)},
		asks[c.Points].PromQL(0):  {warm(at(nil, 1, 1, 2, 2), 1)},
	}
	_, fs := Run(context.Background(), h, vs, t0, t0.Add(3*time.Minute), time.Minute, 1)
	if got := spans(fs); got != "0-2[]" {
		t.Errorf("got %s, want 0-2[]", got)
	}
}

// A Grafana rule replayed with other "for"s, as longer ones for a noisy
// rule are looked for: each worked out in Grafana's states.
func TestWithForGrafana(t *testing.T) {
	n := math.NaN()
	h := history{"x": {
		warm(at(map[string]string{"pod": "a"}, 1, 1, 1, n, n, n, 1, 1, 1, 1, 1, 1), 0),
		warm(at(map[string]string{"pod": "b"}, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0), 0),
	}}
	vs := grafanaRule(t, "        noDataState: OK")
	got, err := WithFor(context.Background(), h, vs[0].Rules[0], []time.Duration{0, 2 * time.Minute, 4 * time.Minute}, t0, t0.Add(11*time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// Missing at 3 and 4, stale at 5: pending from 0 never fired with 4m.
	for d, want := range map[time.Duration]string{0: "0-4[pod=a] 6-on[pod=a]", 2 * time.Minute: "2-4[pod=a] 8-on[pod=a]", 4 * time.Minute: "10-on[pod=a]"} {
		if s := spans(got[d]); s != want {
			t.Errorf("for %s: %s, want %s", d, s, want)
		}
	}
}
