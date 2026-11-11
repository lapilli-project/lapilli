package replay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
)

// What the verification before release found, each kept as a test.

// counted answers as h does, counting the questions with match in them,
// or with exact, that are match; and with fail, failing them.
type counted struct {
	h     History
	match string
	exact bool
	fail  bool
	n     atomic.Int64
}

func (c *counted) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if c.exact && expr == c.match || !c.exact && strings.Contains(expr, c.match) {
		c.n.Add(1)
		if c.fail {
			return nil, errors.New("503 Service Unavailable")
		}
	}
	return c.h.QueryRange(ctx, expr, start, end, step)
}

// tenSeconds is a rule evaluated every 10s, NoData when x returns nothing.
const tenSeconds = `apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    interval: 10s
    rules:
      - uid: u1
        title: R
        condition: B
        noDataState: NoData
        data:
          - {refId: A, datasourceUid: prom, model: {expr: x, instant: true}}
          - refId: B
            datasourceUid: __expr__
            model: {type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [0]}}]}
`

// x returned something for 30s, 80 minutes before the period, between two
// minutes; nothing since.
func gone(t time.Time) (float64, bool) {
	d := t.Sub(t0)
	return 1, d >= -80*time.Minute+10*time.Second && d <= -80*time.Minute+40*time.Second
}

// F4: a question about the day before that fails fails the rule, as a query
// does, rather than reading as no history: that took the rule's NoData
// alert away, and nothing said so.
func TestProbeFails(t *testing.T) {
	h := &counted{h: stepped{"x": gone}, match: "timestamp((", fail: true}
	ran, fs := Run(context.Background(), h, parsed(t, tenSeconds), t0, t0.Add(5*time.Minute), 10*time.Second, 1)
	if !strings.HasPrefix(ran[0].Unsupported, "query failed: ") || len(fs) != 0 || h.n.Load() == 0 {
		t.Errorf("%q, %s", ran[0].Unsupported, spans(fs))
	}
}

// F5: the day before is looked through as often as the rule is evaluated,
// a minute at most: x, there for 30s between two minutes, was seen by
// Grafana's evaluations every 10s, and its going is NoData from the start.
func TestProbeSeesBetweenMinutes(t *testing.T) {
	ran, fs := Run(context.Background(), stepped{"x": gone}, parsed(t, tenSeconds), t0, t0.Add(5*time.Minute), 10*time.Second, 1)
	if ran[0].Unsupported != "" || len(fs) != 1 || fs[0].Labels[StateLabel] != NoDataState || !fs[0].Start.Equal(t0) {
		t.Errorf("%q, %+v", ran[0].Unsupported, fs)
	}
	if got := probeStep(rules.Rule{Interval: 5 * time.Minute}); got != time.Minute {
		t.Errorf("a 5m rule probed every %s", got)
	}
}

// F5: sets of rules set beside each other (--compare, serve's comparisons)
// ask once whether a query returned anything in the day before.
func TestProbeShared(t *testing.T) {
	h := &counted{h: stepped{"x": gone}, match: "timestamp(("}
	v := parsed(t, tenSeconds)
	_, fs := RunEach(context.Background(), h, [][]Version{v, v, v}, t0, t0.Add(5*time.Minute), 10*time.Second, 3)
	if h.n.Load() != 1 || len(fs[0]) != 1 || len(fs[2]) != 1 {
		t.Errorf("asked %d times: %v", h.n.Load(), fs)
	}
}

// F5: what a Prometheus said about a query's day before is kept, so a
// replay an hour on, as serve runs one, doesn't ask again while the last
// time it returned anything is within the day; past it, it asks.
func TestProbeKept(t *testing.T) {
	var probes atomic.Int64
	last := t0.Add(-80 * time.Minute)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.FormValue("query")
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(q, "timestamp((") {
			probes.Add(1)
			fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[%s,"%d"]]}]}}`, r.FormValue("end"), last.Unix())
			return
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	}))
	defer srv.Close()
	c := prom.New(srv.URL, "lapilli-test")
	v := parsed(t, tenSeconds)
	for i, from := range []time.Time{t0, t0.Add(time.Hour), t0.Add(2 * time.Hour), t0.Add(23 * time.Hour)} {
		ran, fs := Run(context.Background(), c, v, from, from.Add(5*time.Minute), 10*time.Second, 1)
		if ran[0].Unsupported != "" || len(fs) != 1 {
			t.Fatalf("run %d: %q %+v", i, ran[0].Unsupported, fs)
		}
	}
	if n := probes.Load(); n != 2 {
		t.Errorf("asked %d times; once, and once more past the day", n)
	}
}

// mixed is a rule whose condition reads x, on Prometheus, beside a query
// on Loki whose values an annotation shows, and an expression over that
// one the condition doesn't use.
const mixed = `apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    interval: 1m
    rules:
      - uid: mix
        title: Mixed
        condition: B
        noDataState: NoData
        data:
          - {refId: A, datasourceUid: prom, model: {expr: x, instant: true}}
          - {refId: L, datasourceUid: loki, model: {expr: 'sum(count_over_time({app="shop"} |= "error" [5m]))', instant: true}}
          - {refId: E, datasourceUid: __expr__, model: {type: math, expression: $L * 2}}
          - refId: B
            datasourceUid: __expr__
            model: {type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [0]}}]}
`

// F6: a query on a data source the condition doesn't read isn't asked of
// the history, which is the condition's: the rule is replayed as its
// condition says, and said to be replayed in part. Asked, Prometheus
// refused the LogQL and the rule failed, where 0.25.0 replayed it.
func TestOtherDataSource(t *testing.T) {
	rs, err := rules.Parse([]byte(mixed))
	if err != nil || len(rs) != 1 || rs[0].Unsupported != "" {
		t.Fatalf("%v %+v", err, rs)
	}
	g := rs[0].Graph
	if g.Other["L"] != "loki" || g.Queries["L"] != "" || g.Data["L"].UID != "" || g.Steps["E"].Math != nil {
		t.Errorf("read %+v", g)
	}
	h := history{"x": {warm(at(nil, 1, 1, 1, 1, 1), 1)}}
	ran, fs := Run(context.Background(), h, one(rs), t0, t0.Add(4*time.Minute), time.Minute, 1)
	if ran[0].Unsupported != "" || spans(fs) != "0-on[]" || !slices.Contains(ran[0].Unseen(), rules.UnseenOther) {
		t.Errorf("%q %s %v", ran[0].Unsupported, spans(fs), ran[0].Unseen())
	}
}

// F6: a query outside the condition, on its data source, that fails
// doesn't fail the rule: whether it returned anything isn't known, so it
// makes nothing NoData, and the rule is said to be replayed in part.
func TestOutsideQueryFails(t *testing.T) {
	y := strings.Replace(mixed, `{refId: L, datasourceUid: loki, model: {expr: 'sum(count_over_time({app="shop"} |= "error" [5m]))', instant: true}}`,
		`{refId: L, datasourceUid: prom, model: {expr: y, instant: true}}`, 1)
	rs, err := rules.Parse([]byte(y))
	if err != nil || len(rs) != 1 || rs[0].Unsupported != "" || len(rs[0].Graph.Other) != 0 {
		t.Fatalf("%v %+v", err, rs)
	}
	h := &counted{h: history{"x": {warm(at(nil, 1, 1, 1, 1, 1), 1)}}, match: "y", fail: true}
	ran, fs := Run(context.Background(), h, one(rs), t0, t0.Add(4*time.Minute), time.Minute, 1)
	if ran[0].Unsupported != "" || spans(fs) != "0-on[]" || !slices.Equal(ran[0].Failed, []string{"L"}) ||
		!slices.Contains(ran[0].Unseen(), rules.UnseenFailed) || h.n.Load() == 0 {
		t.Errorf("%q %s %v %v", ran[0].Unsupported, spans(fs), ran[0].Failed, ran[0].Unseen())
	}
	// The condition's own query failing fails the rule, as before.
	h = &counted{h: history{"y": nil}, match: "x", exact: true, fail: true}
	if ran, _ := Run(context.Background(), h, one(rs), t0, t0.Add(4*time.Minute), time.Minute, 1); !strings.HasPrefix(ran[0].Unsupported, "query failed") {
		t.Errorf("the condition's query failing: %q", ran[0].Unsupported)
	}
}
