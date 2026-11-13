package prom

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// matrix writes an answer of series, each its labels and a sample at every
// step from start to end.
func matrix(labels []string, start, end, step int64) []byte {
	var b strings.Builder
	b.WriteString(`{"status":"success","data":{"resultType":"matrix","result":[`)
	for i, l := range labels {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"metric":{` + l + `},"values":[`)
		for t := start; t <= end; t += step {
			if t > start {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `[%d,"1"]`, t)
		}
		b.WriteString(`]}`)
	}
	b.WriteString(`]}}`)
	return []byte(b.String())
}

// ALERTS{alertstate="firing"} over a large estate (serve's replay check,
// work.go; lapilli verdict --check-replay): 12,000 alerts firing together,
// each with the labels kube-prometheus's alerts carry, some 1.4 KB as
// labelCost counts them. Under 50,000 series and 5,000,000 samples, it
// reads: an answer's labels may take 128 MiB, 50,000 series of 2.7 KB.
func TestWideAnswerRead(t *testing.T) {
	const n = 12000
	labels := make([]string, n)
	for i := range labels {
		labels[i] = fmt.Sprintf(`"__name__":"ALERTS","alertname":"KubeContainerWaiting","alertstate":"firing","container":"app-%05d","endpoint":"http","instance":"10.12.%d.%d:8080","job":"kube-state-metrics","namespace":"team-payments-prod","pod":"checkout-api-7d8f9c6b5-%05d","prometheus":"monitoring/k8s","service":"kube-prometheus-stack-kube-state-metrics","severity":"warning","uid":"0b5c1f0e-4d0a-4c39-9a51-%012d"`, i, i/250, i%250, i, i)
	}
	var asked atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		s, _ := strconv.ParseInt(r.FormValue("start"), 10, 64)
		e, _ := strconv.ParseInt(r.FormValue("end"), 10, 64)
		st, _ := strconv.ParseFloat(r.FormValue("step"), 64)
		w.Header().Set("Content-Type", "application/json")
		w.Write(matrix(labels, s, e, int64(st)))
	}))
	t.Cleanup(srv.Close)
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	got, err := New(srv.URL, "t").QueryRange(context.Background(), `ALERTS{alertstate="firing"}`, t0, t0.Add(10*time.Minute), time.Minute)
	if err != nil || len(got) != n {
		t.Errorf("%d alerts firing together, ten minutes at a minute (%d requests): %d series read, %v", n, asked.Load(), len(got), err)
	}
}

// Each answer is held to its bounds, but what the halves of a range
// answer was joined (fetch, join) with no bound: a hostile API says "too
// many samples" to every range of more than one step, and answers each
// step with series of its own. An hour at a minute, 61 steps of 1,000
// series, is 61,000 series held; a day's chunk, 1,440 steps of 50,000, is
// 72 million. What the answers to one range hold in all is bounded
// (100,000 series, maxRangeSeries; DefaultMaxSamples, 10,000,000 samples,
// or a Client's WithMaxSamples; and 256 MiB of labels, maxRangeLabels, as
// built; less here), and past it the range isn't read, said with the bound
// and how to read it all the same.
//
// Not by what one answer may hold (50,000 series): the halves of a day a
// Prometheus finds too many samples in may hold 30,000 series each and
// 60,000 together, as pods come and go, and a week's days more.
func TestRangeAnswersBoundedInAll(t *testing.T) {
	series, labels := maxRangeSeries, maxRangeLabels
	t.Cleanup(func() { maxRangeSeries, maxRangeLabels = series, labels })
	const per = 1000
	var asked atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		s, _ := strconv.ParseInt(r.FormValue("start"), 10, 64)
		e, _ := strconv.ParseInt(r.FormValue("end"), 10, 64)
		w.Header().Set("Content-Type", "application/json")
		if e > s {
			w.WriteHeader(http.StatusUnprocessableEntity)
			w.Write([]byte(`{"status":"error","errorType":"execution","error":"query processing would load too many samples into memory in query execution"}`))
			return
		}
		labels := make([]string, per)
		for i := range labels {
			labels[i] = fmt.Sprintf(`"s":"%d-%d"`, s, i)
		}
		w.Write(matrix(labels, s, e, 60))
	}))
	t.Cleanup(srv.Close)
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		series, samples, labels int
		want                    string
	}{
		{20000, 1 << 30, 1 << 40, "more than 20000 series in all, the most a range is read with: read it over a shorter range (--since)"},
		{1 << 30, 20000, 1 << 40, "more than 20000 samples in all (--max-samples): read it at a longer --step, over a shorter range (--since), or with a larger --max-samples"},
		{1 << 30, 1 << 30, 1 << 20, "more than 1 MiB of labels in all, the most a range is read with"},
	} {
		maxRangeSeries, maxRangeLabels = c.series, c.labels
		asked.Store(0)
		got, err := New(srv.URL, "t", WithMaxSamples(c.samples)).QueryRange(context.Background(), "up", t0, t0.Add(time.Hour), time.Minute)
		if len(got) != 0 || err == nil || !strings.Contains(err.Error(), c.want) || Refused(err) == false {
			t.Errorf("%s: %d series in %d requests, %v", c.want, len(got), asked.Load(), err)
		}
	}
	maxRangeSeries, maxRangeLabels = series, labels
	got, err := New(srv.URL, "t").QueryRange(context.Background(), "up", t0, t0.Add(time.Hour), time.Minute)
	if len(got) != 61*per || err != nil {
		t.Errorf("as built: %d series, %v", len(got), err)
	}
}

// What an answer that isn't the API's says is kept to its first 4 KiB, and
// said with what the request carried taken out: a token longer than that,
// echoed at its start, is whole nowhere in what is kept, and was said
// from its first byte; its start is taken out too.
func TestCutAnswerLongToken(t *testing.T) {
	token := "ey" + strings.Repeat("J0eXAiOiJKV1QiLCJhbGciOi", 210) // 5,042 bytes
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") + "\n<html>signed in</html>\n"))
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "t", WithToken(func(context.Context) (string, error) { return token, nil }))
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	_, err := c.QueryRange(context.Background(), "up", t0, t0.Add(time.Hour), time.Minute)
	if err == nil || strings.Contains(err.Error(), token[:20]) || !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("%v", err)
	}
}
