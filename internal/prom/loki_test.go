package prom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeLoki answers /loki/api/v1/query_range as Loki 3.7.9 was measured to:
// by GET only, as Grafana's data source proxy allows; each point on a
// whole multiple of the step since the epoch, the first at or before the
// start and the last at or after the end; one series, its value the
// point's time in seconds; and errors in plain text.
func fakeLoki(t *testing.T, asked *[]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/loki/api/v1/query_range" {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, "non allow-listed POSTs not allowed on proxied loki datasource")
			return
		}
		q := r.URL.Query()
		*asked = append(*asked, q.Get("start")+" "+q.Get("end")+" "+q.Get("step"))
		switch q.Get("query") {
		case "bad(":
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, "parse error at line 1, col 5: syntax error: unexpected $end")
			return
		case `{app="shop"}`:
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{"app":"shop"},"values":[["1794423880500000000","msg=ok"]]}]}}`)
			return
		}
		start, _ := strconv.ParseFloat(q.Get("start"), 64)
		end, _ := strconv.ParseFloat(q.Get("end"), 64)
		step, _ := strconv.ParseFloat(q.Get("step"), 64)
		if n := (end - start) / step; n > 11000 {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, "exceeded maximum resolution of 11,000 points per timeseries")
			return
		}
		ms := int64(math.Round(step * 1000))
		first := int64(math.Floor(start*1000/float64(ms))) * ms
		var vals []string
		for p := first; float64(p) < end*1000+float64(ms); p += ms {
			vals = append(vals, fmt.Sprintf(`[%s,"%d"]`, strconv.FormatFloat(float64(p)/1000, 'f', -1, 64), p/1000))
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"app":"shop"},"values":[%s]}],"stats":{}}}`,
			strings.Join(vals, ","))
	}))
}

// TestLokiAnswersAsPrometheus: a range off the step's grid, as a group a
// Loki runs 4s past its interval, is asked at a finer step, and only its
// own points kept: Loki would put them on the grid.
func TestLokiAnswersAsPrometheus(t *testing.T) {
	var asked []string
	srv := fakeLoki(t, &asked)
	defer srv.Close()
	c := New(srv.URL, "test", Loki())
	start := time.Unix(1794424474, 0)
	got, err := c.QueryRange(context.Background(), "sum(rate({app=\"shop\"}[1m]))", start, start.Add(time.Minute), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var at []int64
	for _, s := range got[0].Samples {
		at = append(at, s.T.Unix())
	}
	want := []int64{1794424474, 1794424484, 1794424494, 1794424504, 1794424514, 1794424524, 1794424534}
	if fmt.Sprint(at) != fmt.Sprint(want) || len(asked) != 1 || !strings.HasSuffix(asked[0], " 2") {
		t.Errorf("points at %v, asked %q: want %v, asked every 2s", at, asked, want)
	}
	// On the grid, asked at the step itself.
	asked = nil
	got, err = c.QueryRange(context.Background(), "x", time.Unix(1794424470, 0), time.Unix(1794424530, 0), 10*time.Second)
	if err != nil || len(got[0].Samples) != 7 || !strings.HasSuffix(asked[0], " 10") {
		t.Errorf("%v %v %q", err, got, asked)
	}
	// A step in milliseconds, as Grafana's for six hours, from a time on
	// its grid: asked in milliseconds.
	asked = nil
	from := time.UnixMilli(1794423866172)
	got, err = c.QueryRange(context.Background(), "x", from, from.Add(10*1964*time.Millisecond), 1964*time.Millisecond)
	if err != nil || len(got[0].Samples) != 11 || !got[0].Samples[0].T.Equal(from) || asked[0] != "1794423866.172 1794423885.812 1.964" {
		t.Errorf("%v %d %q", err, len(got[0].Samples), asked)
	}
	// A day at 1m, placed 7s off: asked every second, a request within
	// Loki's 11,000 points.
	asked = nil
	day := time.Unix(1794384007, 0)
	got, err = c.QueryRange(context.Background(), "x", day, day.Add(24*time.Hour), time.Minute)
	if err != nil || len(got[0].Samples) != 1441 || len(asked) < 9 {
		t.Errorf("%v %d samples in %d requests", err, len(got[0].Samples), len(asked))
	}
}

func TestLokiErrors(t *testing.T) {
	var asked []string
	srv := fakeLoki(t, &asked)
	defer srv.Close()
	c := New(srv.URL, "test", Loki())
	now := time.Unix(1794424470, 0)
	_, err := c.QueryRange(context.Background(), "bad(", now, now, time.Minute)
	var api *APIError
	if !errors.As(err, &api) || api.Status != http.StatusBadRequest || api.Msg != "parse error at line 1, col 5: syntax error: unexpected $end" {
		t.Errorf("a refusal: %v", err)
	}
	_, err = c.QueryRange(context.Background(), `{app="shop"}`, now, now, time.Minute)
	if !errors.As(err, &api) || api.Status != http.StatusBadRequest || !strings.Contains(api.Msg, "a log query") {
		t.Errorf("a log query: %v", err)
	}
	// Prometheus's client posts, which the proxy refuses.
	if _, err := New(srv.URL, "test").QueryRange(context.Background(), "x", now, now, time.Minute); err == nil {
		t.Error("a POST to Loki's proxy should be refused")
	}
}

func TestLokiStepOf(t *testing.T) {
	for _, c := range []struct {
		start int64 // ms
		step  time.Duration
		want  time.Duration
	}{
		{1794424470000, 10 * time.Second, 10 * time.Second},
		{1794424474000, 10 * time.Second, 2 * time.Second},
		{1794424471000, 10 * time.Second, time.Second},
		{1794424471000, time.Minute, time.Second},
		{1794424440000, time.Minute, time.Minute},
		{1794423866172, 1964 * time.Millisecond, 1964 * time.Millisecond},
		{-5000, 10 * time.Second, 5 * time.Second},
	} {
		if got := lokiStep(time.UnixMilli(c.start), c.step); got != c.want {
			t.Errorf("from %d every %s: %s, want %s", c.start, c.step, got, c.want)
		}
	}
}

// TestLokiMillisecondSteps: Grafana's Loki data source asks a window over
// about three hours at a step in milliseconds, its window over 11,000
// rounded up: 982ms for 3h, 1.31s for 4h, 1.637s for 5h, 2.619s for 8h,
// 7.855s for a day. Loki writes such a point's time as seconds with a
// fraction (1794424474.31); read as a float64 and multiplied, it was up to
// 128ns off its millisecond, and every point not a whole number of steps
// from the start was dropped: half of them at 1.31s, three in four at
// 1.637s and 7.855s. Every point is kept.
func TestLokiMillisecondSteps(t *testing.T) {
	var asked []string
	srv := fakeLoki(t, &asked)
	defer srv.Close()
	c := New(srv.URL, "test", Loki())
	// 3h, 3.5h, 4h, 5h, 6h, 7h, 8h, 12h and a day.
	for _, ms := range []int64{982, 1146, 1310, 1637, 1964, 2291, 2619, 3928, 7855} {
		step := time.Duration(ms) * time.Millisecond
		start := time.UnixMilli(1794424474000 / ms * ms) // on the step's grid
		got, err := c.QueryRange(context.Background(), `sum(count_over_time({app="shop"}[1m]))`, start, start.Add(100*step), step)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, s := range got {
			n += len(s.Samples)
		}
		if n != 101 {
			t.Errorf("step %s: kept %d of Loki's 101 points", step, n)
		}
	}
}

// TestParseTime: a time as the APIs write it, read exactly.
func TestParseTime(t *testing.T) {
	for in, want := range map[string]int64{
		"1794424474.31": 1794424474310000000, "1794424474": 1794424474000000000, "1794424475.637": 1794424475637000000,
		"1794424475.6370000001": 1794424475637000000, "-1.5": -1500000000, "1.794424474e9": 1794424474000000000,
	} {
		got, err := ParseTime(in)
		if err != nil || got.UnixNano() != want {
			t.Errorf("%s: %d %v, want %d", in, got.UnixNano(), err, want)
		}
	}
	for _, in := range []string{"", "x", "1.2.3", "NaN", "1e300", "--1"} {
		if _, err := ParseTime(in); err == nil {
			t.Errorf("%q should be refused", in)
		}
	}
}

// FuzzLokiAnswer: whatever a Loki answers, QueryRange answers or refuses,
// and never panics.
func FuzzLokiAnswer(f *testing.F) {
	f.Add([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"a":"b"},"values":[[1794424474.31,"1"],[1794424475.62,"NaN"]]}]}}`), int64(1794424474310), int64(1310))
	f.Add([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`), int64(0), int64(1))
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer srv.Close()
	c := New(srv.URL, "fuzz", Loki())
	f.Fuzz(func(t *testing.T, b []byte, startMs, stepMs int64) {
		if stepMs <= 0 || stepMs > 1e9 || startMs < -1e15 || startMs > 1e15 {
			return
		}
		body = b
		start := time.UnixMilli(startMs)
		step := time.Duration(stepMs) * time.Millisecond
		_, _ = c.queryRange(context.Background(), "x", start, start.Add(3*step), step)
		_ = lokiStep(start, step)
	})
}

// TestRealLokiMillisecondSteps: what a real Loki 3.7.9 answered a range of
// 100 steps at 1.146s, 1.31s, 1.637s, 2.291s, 2.619s and 7.855s, the
// steps Grafana asks 3.5, 4, 5, 7 and 8 hours and a day at
// (testdata/loki-3.7.9-ms-steps.json): its times written to the hundredth
// or the thousandth of a second, as 1794431717.24. Every point it answered
// is kept; read as float64s, half and more were lost.
func TestRealLokiMillisecondSteps(t *testing.T) {
	b, err := os.ReadFile("testdata/loki-3.7.9-ms-steps.json")
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Query  string
		Ranges []struct {
			StepMs  int64 `json:"step_ms"`
			StartMs int64 `json:"start_ms"`
			EndMs   int64 `json:"end_ms"`
			Points  int
			Answer  json.RawMessage
		}
	}
	if err := json.Unmarshal(b, &rec); err != nil || len(rec.Ranges) != 6 {
		t.Fatalf("%v %d", err, len(rec.Ranges))
	}
	for _, r := range rec.Ranges {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { w.Write(r.Answer) }))
		step := time.Duration(r.StepMs) * time.Millisecond
		got, err := New(srv.URL, "test", Loki()).QueryRange(context.Background(), rec.Query, time.UnixMilli(r.StartMs), time.UnixMilli(r.EndMs), step)
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, s := range got {
			n += len(s.Samples)
		}
		if n != r.Points || n < 80 {
			t.Errorf("step %s: kept %d of Loki's %d points", step, n, r.Points)
		}
	}
}

// TestLokiSeriesLimit: Loki refuses a range whose series, over the whole
// range, are more than its max_query_series, and says to shorten it; the
// range is asked again in halves, each on the range's own grid, as a
// group placed 4s into its interval is, until each half holds few enough.
// At one step it still fails, as Loki's ruler would.
func TestLokiSeriesLimit(t *testing.T) {
	var asked int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		q := r.URL.Query()
		start, _ := strconv.ParseFloat(q.Get("start"), 64)
		end, _ := strconv.ParseFloat(q.Get("end"), 64)
		step, _ := strconv.ParseFloat(q.Get("step"), 64)
		if (end-start)/step > 20 || q.Get("query") == "wide" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, "maximum number of series (500) reached for a single query; consider reducing query cardinality by adding more specific stream selectors, reducing the time range, or aggregating results with functions like sum(), count() or topk()")
			return
		}
		var vals []string
		for p := start; p <= end+1e-9; p += step {
			vals = append(vals, fmt.Sprintf(`[%s,"1"]`, strconv.FormatFloat(p, 'f', -1, 64)))
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[%s]}]}}`, strings.Join(vals, ","))
	}))
	defer srv.Close()
	c := New(srv.URL, "test", Loki())
	start := time.Unix(1794424474, 0) // 4s past the 10s grid
	got, err := c.QueryRange(context.Background(), "q", start, start.Add(100*10*time.Second), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Samples) != 101 {
		t.Fatalf("%d series: %+v", len(got), got)
	}
	for _, s := range got[0].Samples {
		if s.T.Unix()%10 != 4 {
			t.Fatalf("a point off the range's grid: %s", s.T)
		}
	}
	if _, err := c.QueryRange(context.Background(), "wide", start, start, 10*time.Second); err == nil || !strings.Contains(err.Error(), "maximum number of series") {
		t.Errorf("one step over the limit: %v", err)
	}
}

// TestParseTimeEdges: what the APIs write, read exactly; the rest refused
// or read to the millisecond.
func TestParseTimeEdges(t *testing.T) {
	for _, c := range []struct {
		in   string
		want time.Time
		bad  bool
	}{
		{in: "1794424474.31", want: time.UnixMilli(1794424474310)},
		{in: "1794424474.310", want: time.UnixMilli(1794424474310)},
		{in: "1794424474", want: time.Unix(1794424474, 0)},
		{in: "1794424474.123456789", want: time.Unix(1794424474, 123456789)},
		{in: "1794424474.1234567891", want: time.Unix(1794424474, 123456789)}, // past ns: cut
		{in: "1.7944e9", want: time.Unix(1794400000, 0)},
		{in: "1.79442447431e9", want: time.UnixMilli(1794424474310)},
		{in: "-1.5", want: time.UnixMilli(-1500)},
		{in: "-0.001", want: time.UnixMilli(-1)},
		{in: "0", want: time.Unix(0, 0)},
		{in: "999999999999.999", want: time.UnixMilli(999999999999999)},
		{in: "9999999999999", bad: true},
		{in: "1e13", bad: true},
		{in: `"1794424474.31"`, bad: true}, // quoted: refused, as before
		{in: "NaN", bad: true},
		{in: "Inf", bad: true},
		{in: "", bad: true},
		{in: "-", bad: true},
		{in: "1.2.3", bad: true},
		{in: "0x10", bad: true},
	} {
		got, err := ParseTime(c.in)
		switch {
		case c.bad && err == nil:
			t.Errorf("%q: read as %s, want refused", c.in, got.Format(time.RFC3339Nano))
		case !c.bad && err != nil:
			t.Errorf("%q: %v", c.in, err)
		case !c.bad && !got.Equal(c.want):
			t.Errorf("%q: %s, want %s", c.in, got.Format(time.RFC3339Nano), c.want.UTC().Format(time.RFC3339Nano))
		}
	}
}

// TestLokiMillisecondStepsSweep: every step from 982ms to 7.855s that
// Grafana's 11,000-point rule makes (span over 11,000, to the ms, for
// spans of 3h to 24h, a minute apart), every point Loki answers kept.
func TestLokiMillisecondStepsSweep(t *testing.T) {
	var asked []string
	srv := fakeLoki(t, &asked)
	defer srv.Close()
	c := New(srv.URL, "test", Loki())
	seen := map[int64]bool{}
	bad := 0
	for span := 3 * time.Hour; span <= 24*time.Hour; span += time.Minute {
		ms := (span.Milliseconds() + 10999) / 11000
		if seen[ms] {
			continue
		}
		seen[ms] = true
		step := time.Duration(ms) * time.Millisecond
		start := time.UnixMilli(1794424474000 / ms * ms)
		got, err := c.QueryRange(context.Background(), `sum(count_over_time({app="shop"}[1m]))`, start, start.Add(50*step), step)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, s := range got {
			n += len(s.Samples)
		}
		if n != 51 && bad < 10 {
			bad++
			t.Errorf("step %s: kept %d of 51", step, n)
		}
	}
	t.Logf("%d steps from 982ms to 7.855s", len(seen))
}

// TestLokiSeriesLimitHalving: a Loki that says "maximum number of series"
// for any range of more than limit points. The range is halved on its own
// grid (every point asked is start + k·step), the requests stay within
// about two a point, and a single evaluation over the limit fails.
func TestLokiSeriesLimitHalving(t *testing.T) { seriesLimit(t, 100) }

// TestLokiSeriesLimitOneEvaluation: Loki's ruler asks one evaluation at a
// time. Where each evaluation's series are within max_query_series but two
// consecutive evaluations' together aren't (series churning at the limit),
// the halving went down to two points and stopped, so the replay failed
// though Loki's ruler evaluated every one; it goes down to one.
func TestLokiSeriesLimitOneEvaluation(t *testing.T) { seriesLimit(t, 1) }

func seriesLimit(t *testing.T, limits ...int) {
	for _, limit := range limits {
		var reqs int
		offGrid := 0
		start := time.Unix(1794424474, 0).Add(4 * time.Second) // off the minute's grid, as a placed group
		step := time.Minute
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqs++
			q := r.URL.Query()
			s, _ := strconv.ParseFloat(q.Get("start"), 64)
			e, _ := strconv.ParseFloat(q.Get("end"), 64)
			if (int64(s)-start.Unix())%60 != 0 {
				offGrid++
			}
			if int((e-s)/60)+1 > limit {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, "maximum number of series (500) reached for a single query; consider reducing the time range")
				return
			}
			var vals []string
			for p := int64(s); p <= int64(e); p += 60 {
				vals = append(vals, fmt.Sprintf(`[%d,"1"]`, p))
			}
			fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"a":"b"},"values":[%s]}]}}`, strings.Join(vals, ","))
		}))
		c := New(srv.URL, "test", Loki())
		got, err := c.QueryRange(context.Background(), "x", start, start.Add(1439*step), step)
		srv.Close()
		n := 0
		for _, s := range got {
			n += len(s.Samples)
		}
		t.Logf("limit %d points: %d requests for 1440 points, %d kept, %d off the grid, err %v", limit, reqs, n, offGrid, err)
		if err != nil || n != 1440 || offGrid > 0 || reqs > 2*1440 {
			t.Errorf("limit %d: %d requests, %d kept, %d off grid, %v", limit, reqs, n, offGrid, err)
		}
	}
	// One evaluation over the limit: refused, after a handful of requests.
	reqs := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs++
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "maximum number of series (500) reached for a single query")
	}))
	defer srv.Close()
	start := time.Unix(1794424440, 0)
	_, err := New(srv.URL, "test", Loki()).QueryRange(context.Background(), "x", start, start.Add(1439*time.Minute), time.Minute)
	t.Logf("always over: %d requests, %v", reqs, err)
	if err == nil || reqs > 20 {
		t.Errorf("always over the limit: %d requests, err %v", reqs, err)
	}
}
