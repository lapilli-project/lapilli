package replay

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
)

// points is a history whose every query returns one series, its value at
// each step that function of the time; it keeps what it was asked.
type points struct {
	mu    sync.Mutex
	name  string
	v     func(t time.Time) (float64, bool)
	asked []string
}

func (p *points) QueryRange(_ context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	p.mu.Lock()
	p.asked = append(p.asked, fmt.Sprintf("%s %s %s %s", expr, start.UTC().Format("15:04:05.000"), end.UTC().Format("15:04:05.000"), step))
	p.mu.Unlock()
	var s prom.Series
	s.Labels = map[string]string{"from": p.name}
	for t := start; !t.After(end); t = t.Add(step) {
		if v, ok := p.v(t); ok {
			s.Samples = append(s.Samples, prom.Sample{T: t, V: v})
		}
	}
	if len(s.Samples) == 0 {
		return nil, nil
	}
	return []prom.Series{s}, nil
}

// TestLogReduce: a Grafana reduce over a LogQL range query's points, as
// Grafana's Loki data source asks them: every Res on its grid, from the
// window before the evaluation, less its window's end, to that end, both
// in; one range asked for the whole stretch.
func TestLogReduce(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 19, 0, 0, 0, time.UTC)
	// The value at each second is its second of the minute; nothing from
	// 19:02:00 to 19:02:59.
	h := &points{v: func(t time.Time) (float64, bool) { return float64(t.Second()), t.Minute() != 2 }}
	a := rules.Ask{Expr: `sum(count_over_time({app="x"}[1m]))`, Window: 30 * time.Second, Res: time.Second, Logs: true}
	for fn, want := range map[string]string{
		// At 19:01:00 less 10s: the points from 19:00:20 to 19:00:50.
		"max": "50", "min": "20", "mean": "35", "sum": "1085", "count": "31", "last": "50",
	} {
		a.Fn = fn
		got, err := logReduce(context.Background(), h, a, t0.Add(time.Minute), t0.Add(4*time.Minute), time.Minute, 10*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		var vals []string
		for _, s := range got[0].Samples {
			vals = append(vals, fmt.Sprintf("%s=%g", s.T.Format("04"), s.V))
		}
		// 19:03 reads 19:02:20 to 19:02:50: no point, no value.
		if len(got) != 1 || !strings.HasPrefix(strings.Join(vals, " "), "01="+want+" 02=") || strings.Contains(strings.Join(vals, " "), "03=") {
			t.Errorf("%s: %v, want 01=%s, and none at 03", fn, vals, want)
		}
	}
	if len(h.asked) != 6 || h.asked[0] != `sum(count_over_time({app="x"}[1m])) 19:00:20.000 19:03:50.000 1s` {
		t.Errorf("asked %q: one range a reduce", h.asked)
	}
	// A step in milliseconds: the window's ends are on its grid since the
	// epoch, outward, as Loki answers: 19:00:28.528 to 19:01:01.916, 18
	// points.
	h.asked = nil
	a.Fn, a.Res = "count", 1964*time.Millisecond
	got, err := logReduce(context.Background(), h, a, t0.Add(time.Minute), t0.Add(time.Minute), time.Minute, 0)
	if err != nil || len(got) != 1 || got[0].Samples[0].V != 18 || !strings.HasPrefix(h.asked[0], "sum(count_over_time({app=\"x\"}[1m])) 19:00:28.528 19:01:01.916") {
		t.Errorf("%v %+v %q", err, got, h.asked)
	}
}

// TestRoutedToLoki: a rule written in LogQL is asked of its Loki, a PromQL
// rule of the same text of Prometheus, and the two never share an answer;
// without a Loki, the LogQL rule isn't replayed.
func TestRoutedToLoki(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 19, 0, 0, 0, time.UTC)
	promH := &points{name: "prometheus", v: func(t time.Time) (float64, bool) { return 1, t.Minute()%10 < 5 }}
	lokiH := &points{name: "loki", v: func(t time.Time) (float64, bool) { return 1, t.Minute()%10 >= 5 }}
	expr := `sum(count_over_time({app="shop"}[5m])) > 0`
	rs := []rules.Rule{{Name: "P", Expr: expr}, {Name: "L", Expr: expr, Logs: rules.LokiRuler}}
	h := WithLogs(promH, func(r rules.Rule) (History, error) {
		if r.Logs != rules.LokiRuler {
			t.Errorf("asked for %q", r.Logs)
		}
		return lokiH, nil
	})
	ran, fs := Run(context.Background(), h, []Version{{Rules: rs}}, t0, t0.Add(time.Hour), time.Minute, 2)
	for _, r := range ran {
		if r.Unsupported != "" {
			t.Errorf("%s: %s", r.Name, r.Unsupported)
		}
	}
	for _, f := range fs {
		if (f.Rule == "P") != (f.Labels["from"] == "prometheus") || f.Start.Minute()%10 != map[string]int{"P": 0, "L": 5}[f.Rule] {
			t.Errorf("%s fired %v from %s", f.Rule, f.Labels, f.Start.Format("15:04"))
		}
	}
	// P at 19:00 and every ten minutes to 20:00, L at 19:05 to 19:55.
	if len(fs) != 13 || len(promH.asked) == 0 || len(lokiH.asked) == 0 {
		t.Errorf("%d firings, Prometheus asked %d, Loki %d", len(fs), len(promH.asked), len(lokiH.asked))
	}
	if Base(h) != History(promH) {
		t.Error("Base should be the Prometheus")
	}
	// Without WithLogs, nothing to ask.
	ran, _ = Run(context.Background(), promH, []Version{{Rules: rs}}, t0, t0.Add(time.Hour), time.Minute, 2)
	if ran[0].Unsupported != "" || !strings.Contains(ran[1].Unsupported, "no Loki to ask") {
		t.Errorf("%q %q", ran[0].Unsupported, ran[1].Unsupported)
	}
	if _, err := Condition(context.Background(), promH, rs[1], t0, t0.Add(time.Hour), time.Minute); err == nil {
		t.Error("a LogQL rule's condition without a Loki should fail")
	}
}

// TestLokiNoData: a Grafana rule on Loki whose query returned nothing is
// NoData only once it is known to have returned something: in the day
// before, asked of Loki over the day, as LogQL has no subqueries.
func TestLokiNoData(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 19, 0, 0, 0, time.UTC)
	rs, err := rules.Parse([]byte(lokiNoData))
	if err != nil {
		t.Fatal(err)
	}
	for _, seen := range []bool{true, false} {
		// Something at 12:00 the day before, if seen; nothing since.
		lokiH := &points{v: func(t time.Time) (float64, bool) { return 3, seen && t.Equal(t0.Add(-7*time.Hour)) }}
		h := WithLogs(&points{v: func(time.Time) (float64, bool) { return 0, false }}, func(rules.Rule) (History, error) { return lokiH, nil })
		ran, fs := Run(context.Background(), h, []Version{{Rules: rs}}, t0, t0.Add(10*time.Minute), time.Minute, 1)
		if ran[0].Unsupported != "" {
			t.Fatal(ran[0].Unsupported)
		}
		if (len(fs) == 1 && fs[0].Labels["grafana_alertstate"] == "nodata") != seen {
			t.Errorf("seen the day before %v: %+v", seen, fs)
		}
		probed := false
		for _, a := range lokiH.asked {
			probed = probed || strings.HasPrefix(a, `sum(count_over_time({app="gappy"}[30s])) 18:`) && strings.HasSuffix(a, " 1m0s")
		}
		if !probed {
			t.Errorf("the day before wasn't asked of Loki: %q", lokiH.asked)
		}
	}
}

const lokiNoData = `apiVersion: 1
groups:
  - orgId: 1
    name: logs
    folder: Logs
    interval: 1m
    rules:
      - uid: gappy
        title: Gappy
        condition: B
        noDataState: NoData
        data:
          - refId: A
            queryType: instant
            relativeTimeRange: {from: 600, to: 0}
            datasourceUid: loki
            model: {datasource: {type: loki, uid: loki}, expr: 'sum(count_over_time({app="gappy"}[30s]))', queryType: instant}
          - refId: B
            datasourceUid: __expr__
            model: {type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [0]}}]}
`

// alignedLoki answers /loki/api/v1/query_range as Loki 3.7.9 does: points
// on whole multiples of the step since the epoch, from the one at or before
// start to the one at or after end, times written as model.Time.String()
// writes them (seconds, a fraction for milliseconds), each value 1. With
// empty, it answers no series, but still counts what it was asked.
type alignedLoki struct {
	empty          bool
	requests, asks atomic.Int64 // requests, and points asked for
}

func (l *alignedLoki) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	start, _ := strconv.ParseFloat(q.Get("start"), 64)
	end, _ := strconv.ParseFloat(q.Get("end"), 64)
	step, _ := strconv.ParseFloat(q.Get("step"), 64)
	ms := int64(math.Round(step * 1000))
	l.requests.Add(1)
	if ms <= 0 { // a step under a millisecond: counted, answered empty
		l.asks.Add(int64((end-start)/step) + 1)
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
		return
	}
	first := int64(math.Floor(start*1000/float64(ms))) * ms
	if n := (end - start) / step; n > 11000 {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "exceeded maximum resolution of 11,000 points per timeseries")
		return
	}
	var vals []string
	for p := first; float64(p) < end*1000+float64(ms); p += ms {
		l.asks.Add(1)
		if !l.empty {
			vals = append(vals, fmt.Sprintf(`[%s,"1"]`, strconv.FormatFloat(float64(p)/1000, 'f', -1, 64)))
		}
	}
	if l.empty {
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
		return
	}
	fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"app":"shop"},"values":[%s]}]}}`, strings.Join(vals, ","))
}

// lokiReduceDoc is a Grafana rule on Loki, as Grafana's editor writes one,
// in a provisioning file.
func lokiReduceDoc(window int, extra, reducer string, over float64) string {
	return fmt.Sprintf(`apiVersion: 1
groups:
  - orgId: 1
    name: logs
    folder: Logs
    interval: 1m
    rules:
      - uid: r1
        title: R1
        condition: C
        data:
          - refId: A
            relativeTimeRange: {from: %d, to: 0}
            datasourceUid: lk
            model: {datasource: {type: loki, uid: lk}, expr: 'sum(count_over_time({app="shop"}[1m]))', queryType: range, intervalMs: 1000, maxDataPoints: 43200%s}
          - refId: B
            datasourceUid: __expr__
            model: {type: reduce, expression: A, reducer: %s}
          - refId: C
            datasourceUid: __expr__
            model: {type: threshold, expression: B, conditions: [{evaluator: {type: gt, params: [%g]}}]}
`, window, extra, reducer, over)
}

// lokiReduceRule is a Grafana rule on Loki, as Grafana's editor writes one: a
// range query over a window, reduced, and a threshold; extra goes in its
// query's model.
func lokiReduceRule(t *testing.T, window int, extra, reducer string, over float64) rules.Rule {
	t.Helper()
	doc := lokiReduceDoc(window, extra, reducer, over)
	rs, err := rules.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	rules.Replaying{Kinds: map[string]string{"lk": rules.KindLoki}}.Apply(rs)
	if rs[0].Unsupported != "" || rs[0].Logs != "lk" {
		t.Fatalf("%q %q", rs[0].Unsupported, rs[0].Logs)
	}
	return rs[0]
}

func lokiHistory(url string) History {
	l := prom.New(url, "test", prom.Loki())
	return WithLogs(noPrometheus{}, func(rules.Rule) (History, error) { return l, nil })
}

type noPrometheus struct{}

func (noPrometheus) QueryRange(context.Context, string, time.Time, time.Time, time.Duration) ([]prom.Series, error) {
	return nil, fmt.Errorf("no prometheus")
}

// TestLokiFourHourCount: a Grafana rule counting a Loki query's points over
// four hours. Grafana asks Loki at 1.31s (14,400s over 11,000, rounded up
// to the millisecond), and Loki has a point every 1.31s: about 10,993 in
// each window, over the rule's 10,000, so Grafana fires at every
// evaluation. Read as float64s, half of the points were lost, the count
// was half, and the replay never fired.
func TestLokiFourHourCount(t *testing.T) {
	r := lokiReduceRule(t, 14400, "", "count", 10000)
	if a := r.Graph.Asks["B"]; a.Res != 1310*time.Millisecond || !a.Logs {
		t.Fatalf("ask %+v: want Res 1.31s, as Grafana asks", a)
	}
	srv := httptest.NewServer(&alignedLoki{})
	defer srv.Close()
	t0 := time.Date(2026, 11, 11, 19, 0, 0, 0, time.UTC)
	got, err := Condition(context.Background(), lokiHistory(srv.URL), r, t0, t0.Add(10*time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	held := 0
	for _, s := range got {
		held += len(s.Samples)
	}
	if held != 11 {
		t.Errorf("the condition held at %d of 11 evaluations; Grafana's count was over 10,000 at each", held)
	}
}

// TestLokiTinyStep: a rule whose Loki range query asks a point every
// millisecond (its own step, 0.001 seconds, or intervalMs 1) over a 10s
// window. Grafana asks 10,000 points an evaluation, which Loki allows; a
// replay would ask them for the whole period, 86.4 million a series a
// day, and serve held a day of them at once. It isn't replayed, says the
// step, and Loki isn't asked; at Grafana's usual 1s it is.
func TestLokiTinyStep(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 19, 0, 0, 0, time.UTC)
	for _, extra := range []string{", step: 1ms", ", step: 0.001", ", intervalMs: 1", ", step: 1ns"} {
		doc := strings.Replace(lokiReduceDoc(10, extra, "max", 0), "intervalMs: 1000, ", "", strings.Count(extra, "intervalMs"))
		rs, err := rules.Parse([]byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		rules.Replaying{Kinds: map[string]string{"lk": rules.KindLoki}}.Apply(rs)
		if !strings.Contains(rs[0].Unsupported, "a point every") || !strings.Contains(rs[0].Unsupported, "no finer than every 1s") {
			t.Errorf("%s: %q", extra, rs[0].Unsupported)
		}
		l := &alignedLoki{}
		srv := httptest.NewServer(l)
		ran, _ := Run(context.Background(), lokiHistory(srv.URL), []Version{{Rules: rs}}, t0, t0.Add(time.Hour), time.Minute, 1)
		srv.Close()
		if l.requests.Load() != 0 || ran[0].Unsupported == "" {
			t.Errorf("%s: Loki asked %d times", extra, l.requests.Load())
		}
	}
	r := lokiReduceRule(t, 10, "", "max", 0)
	l := &alignedLoki{empty: true}
	srv := httptest.NewServer(l)
	defer srv.Close()
	if _, err := Condition(context.Background(), lokiHistory(srv.URL), r, t0, t0.Add(time.Hour), time.Minute); err != nil {
		t.Fatal(err)
	}
	// The hour and a window, at 1s, and the day before at the rule's
	// minute: about 5,000 points, where a millisecond asked millions.
	if n := l.asks.Load(); n > 6000 {
		t.Errorf("an hour at 1s asked for %d points", n)
	}
}

// requestSpans is a history that keeps how many of a step's points each request
// requestSpans.
type requestSpans struct {
	points
	mu  sync.Mutex
	max int64
	n   int
}

func (s *requestSpans) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	s.mu.Lock()
	s.max, s.n = max(s.max, int64(end.Sub(start)/step)+1), s.n+1
	s.mu.Unlock()
	return s.points.QueryRange(ctx, expr, start, end, step)
}

// TestLogReduceBatches: a day of a reduce at 1s is asked for and reduced
// a batch of evaluations at a time, each at most reducePoints of the
// query's points beyond a window's, not a day's 86,400 held at once; and
// comes out as asked for at once.
func TestLogReduceBatches(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	v := func(t time.Time) (float64, bool) { return float64(t.Unix() % 97), true }
	a := rules.Ask{Fn: "max", Expr: "q", Window: 5 * time.Minute, Res: time.Second, Logs: true}
	h := &requestSpans{points: points{v: v}}
	got, err := logReduce(context.Background(), h, a, t0, t0.Add(24*time.Hour), time.Minute, 0)
	if err != nil {
		t.Fatal(err)
	}
	if h.max > reducePoints+301 || h.n < 8 {
		t.Errorf("%d requests, the largest of %d points", h.n, h.max)
	}
	if len(got) != 1 || len(got[0].Samples) != 1441 {
		t.Fatalf("%+v", got)
	}
	for _, s := range got[0].Samples {
		if s.V != 96 {
			t.Errorf("%s: max %g, want 96", s.T.Format("15:04"), s.V)
			break
		}
	}
}

// TestLokiProbeStepFloor: a Grafana rule file whose group's interval is a
// nanosecond, its query on Loki returning nothing at first. Whether it
// returned anything the day before was asked at the group's interval:
// requests of 1,440ns until the deadline, which failed the run. It is
// asked no finer than every second.
func TestLokiProbeStepFloor(t *testing.T) {
	doc := `apiVersion: 1
groups:
  - orgId: 1
    name: logs
    folder: Logs
    interval: 1ns
    rules:
      - uid: gappy
        title: Gappy
        condition: B
        noDataState: NoData
        data:
          - refId: A
            queryType: instant
            relativeTimeRange: {from: 600, to: 0}
            datasourceUid: loki
            model: {datasource: {type: loki, uid: loki}, expr: 'sum(count_over_time({app="gone"}[30s]))', queryType: instant}
          - refId: B
            datasourceUid: __expr__
            model: {type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [0]}}]}
`
	rs, err := rules.Parse([]byte(doc))
	if err != nil || rs[0].Unsupported != "" || rs[0].Logs != "loki" {
		t.Fatalf("%v %q %q", err, rs[0].Unsupported, rs[0].Logs)
	}
	l := &alignedLoki{empty: true}
	srv := httptest.NewServer(l)
	defer srv.Close()
	t0 := time.Date(2026, 11, 11, 19, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ran, _ := Run(ctx, lokiHistory(srv.URL), []Version{{Rules: rs}}, t0, t0.Add(10*time.Minute), time.Minute, 1)
	if l.requests.Load() > 100 || ran[0].Unsupported != "" {
		t.Errorf("ten minutes of one rule asked Loki %d times (%q)", l.requests.Load(), ran[0].Unsupported)
	}
}
