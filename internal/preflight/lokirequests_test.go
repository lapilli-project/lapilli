package preflight

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/serve"
)

// fakeLoki answers every range query as a Loki does, a point on each
// whole multiple of the step since the epoch in the range, and counts them.
type fakeLoki struct{ requests atomic.Int64 }

func (l *fakeLoki) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	start, _ := strconv.ParseFloat(q.Get("start"), 64)
	end, _ := strconv.ParseFloat(q.Get("end"), 64)
	step, _ := strconv.ParseFloat(q.Get("step"), 64)
	l.requests.Add(1)
	ms := int64(math.Round(step * 1000))
	if ms <= 0 {
		http.Error(w, "step", http.StatusBadRequest)
		return
	}
	var vals []string
	for p := int64(math.Floor(start*1000/float64(ms))) * ms; float64(p) < end*1000+float64(ms); p += ms {
		vals = append(vals, fmt.Sprintf(`[%s,"1"]`, strconv.FormatFloat(float64(p)/1000, 'f', -1, 64)))
	}
	fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"app":"shop"},"values":[%s]}]}}`, strings.Join(vals, ","))
}

type noProm struct{}

func (noProm) QueryRange(context.Context, string, time.Time, time.Time, time.Duration) ([]prom.Series, error) {
	return nil, fmt.Errorf("no Prometheus")
}

// lokiRules are rules in LogQL, of Loki's ruler and of Grafana, with short
// and long windows.
func lokiRules(t *testing.T) map[string]rules.Rule {
	t.Helper()
	out := map[string]rules.Rule{}
	for name, window := range map[string]string{"ruler 5m": "5m", "ruler 1h": "1h"} {
		rs, err := rules.Parse([]byte(fmt.Sprintf("groups:\n  - name: logs\n    interval: 1m\n    rules:\n      - alert: LogErrors\n        expr: sum(count_over_time({app=\"shop\"}[%s])) > 10\n        for: 5m\n", window)))
		if err != nil {
			t.Fatal(err)
		}
		rs[0].Logs = rules.LokiRuler
		out[name] = rs[0]
	}
	for name, window := range map[string]int{"grafana 5m": 300, "grafana 1h": 3600} {
		rs, err := rules.Parse([]byte(fmt.Sprintf(`apiVersion: 1
groups:
  - orgId: 1
    name: logs
    folder: Logs
    interval: 1m
    rules:
      - uid: r1
        title: LogErrors
        condition: C
        data:
          - refId: A
            relativeTimeRange: {from: %d, to: 0}
            datasourceUid: lk
            model: {datasource: {type: loki, uid: lk}, expr: 'sum(count_over_time({app="shop"}[1m]))', queryType: range, intervalMs: 1000, maxDataPoints: 43200}
          - refId: B
            datasourceUid: __expr__
            model: {type: reduce, expression: A, reducer: max}
          - refId: C
            datasourceUid: __expr__
            model: {type: threshold, expression: B, conditions: [{evaluator: {type: gt, params: [10]}}]}
`, window)))
		if err != nil {
			t.Fatal(err)
		}
		rules.Replaying{Kinds: map[string]string{"lk": rules.KindLoki}}.Apply(rs)
		if rs[0].Unsupported != "" || rs[0].Logs != "lk" {
			t.Fatalf("%s: %q %q", name, rs[0].Unsupported, rs[0].Logs)
		}
		out[name] = rs[0]
	}
	return out
}

// The estimate of a run's requests to Loki is what the replay asks: the
// ranges of a LogQL query in Grafana's rule, a batch of evaluations at a
// time and each in the pieces Loki's client makes of it, and of Loki's
// ruler's rule, counted from the same code (replay.Plan). A fake Loki
// counts what a replay of the same rule over the same period asks, as
// serve's runs do: from the rule's margin before the period.
func TestEstimateLokiRequestsAreTheReplays(t *testing.T) {
	to := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	for name, r := range lokiRules(t) {
		for _, days := range []int{1, 7, 28} {
			t.Run(fmt.Sprintf("%s over %dd", name, days), func(t *testing.T) {
				srv := &fakeLoki{}
				ts := httptest.NewServer(srv)
				defer ts.Close()
				c := config()
				c.Serve.Loki = ts.URL
				p := newRun(c)
				step := c.Serve.Step
				from := to.Add(-time.Duration(days) * 24 * time.Hour)
				got := p.piece(context.Background(), []rules.Rule{r}, from, to)
				estimated := got["loki"] + got["grafana"]

				since := from.Add(-serve.Margin(r, step)).Truncate(step)
				h := replay.WithLogs(noProm{}, func(rules.Rule) (replay.History, error) {
					return prom.New(ts.URL, "test", prom.Loki()), nil
				})
				ran, _ := replay.Run(context.Background(), h, []replay.Version{{Rules: []rules.Rule{r}}}, since, to, step, 1)
				if ran[0].Unsupported != "" {
					t.Fatalf("not replayed: %s", ran[0].Unsupported)
				}
				asked := int(srv.requests.Load())
				t.Logf("estimated %d, asked %d", estimated, asked)
				if asked == 0 || estimated != asked || got["prometheus"] != 0 {
					t.Errorf("estimated %d requests (%v), the replay asked Loki %d", estimated, got, asked)
				}
			})
		}
	}
}
