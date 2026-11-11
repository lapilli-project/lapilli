package prom

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
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
