package serve

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
)

// A rule an agent compares over MCP is replayed as the rules in effect are,
// --late too: with its metrics taken to arrive 10m late, a rule added
// catches INC-1 10m later than without.
func TestCompareLate(t *testing.T) {
	added := `groups:
  - name: shop
    rules:
      - alert: MoreErrors
        expr: errors > 5
        for: 15m
        labels: {severity: page}
`
	delay := func(late rules.Lateness) float64 {
		t.Helper()
		ctx := context.Background()
		s, _ := server(t, func(c *Config) {
			c.UserHeader = "X-Forwarded-Email"
			c.Editors = []string{"ed@example.com"}
			c.MCPCompare = "editors"
			c.Late = late
		})
		raw := ""
		if err := s.look(ctx, &raw); err != nil {
			t.Fatal(err)
		}
		s.jobs(ctx, true)
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()
		var cmp compareOut
		if failed, text := call(t, connect(t, ts.URL, "ed@example.com"), "compare", map[string]any{"rules": added}, &cmp); failed {
			t.Fatal(text)
		}
		for _, in := range cmp.Incidents {
			for _, c := range in.After {
				if in.ID == "INC-1" && c.Rule == "MoreErrors" {
					return c.Delay
				}
			}
		}
		t.Fatalf("MoreErrors didn't catch INC-1: %+v", cmp)
		return 0
	}
	on, off := delay(rules.Lateness{{D: 10 * time.Minute}}), delay(nil)
	if on-off != (10 * time.Minute).Seconds() {
		t.Errorf("caught %vs after it, late; %vs, not", on, off)
	}
}

// A proposal's backtest replays the rule as the runs do, --late too: with
// Errors's metrics taken to arrive 30m late, for: 2m catches INC-1 31m in
// and for: 15m, 44m in, after it ended at 39m; on time, both catch it.
func TestBacktestLate(t *testing.T) {
	for _, c := range []struct {
		late rules.Lateness
		lost []string
	}{{nil, []string{}}, {rules.Lateness{{D: 30 * time.Minute}}, []string{"INC-1"}}} {
		s, _ := changesServer(t, func(cfg *Config) { cfg.Late = c.late })
		ev := proposalEvidence(state(t, s, proposeFor(t, s.Handler(), "15m")))
		if ev.Backtest == nil || !slices.Equal(ev.Backtest.Lost, c.lost) {
			t.Errorf("--late %v: the backtest: %+v", c.late, ev.Backtest)
		}
	}
}

// F1: with --grafana, the scrape interval each data source says it has is
// read, an hour at most before, and a rule's $__rate_interval put in with
// it, as Grafana does: its runs, and anything replayed, read 2m for a
// data source of 30s.
func TestGrafanaDataSourceScrape(t *testing.T) {
	ctx := context.Background()
	var asked int
	refuse := true
	g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/provisioning/alert-rules/export":
			io.WriteString(w, `apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    interval: 1m
    rules:
      - uid: m30
        title: Errors
        condition: B
        data:
          - {refId: A, datasourceUid: prom30, relativeTimeRange: {from: 600, to: 0}, model: {expr: 'errors > 5 and on() vector($__rate_interval_ms)', intervalMs: 1000, maxDataPoints: 43200, instant: true}}
          - refId: B
            datasourceUid: __expr__
            model: {type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [0]}}]}
`)
		case "/api/datasources/uid/prom30":
			asked++
			if refuse {
				http.Error(w, `{"message": "Access denied"}`, http.StatusForbidden)
				return
			}
			io.WriteString(w, `{"uid": "prom30", "jsonData": {"timeInterval": "30s"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer g.Close()
	s, _ := server(t, func(c *Config) { c.RulesPath, c.Grafana = "", g.URL })
	query := func() string {
		t.Helper()
		for range 3 {
			raw := ""
			if err := s.look(ctx, &raw); err != nil {
				t.Fatal(err)
			}
		}
		vs, err := s.versions(ctx, time.Unix(0, 0), s.now())
		if err != nil || len(vs) != 1 || len(vs[0].Rules) != 1 {
			t.Fatalf("%v %+v", err, vs)
		}
		return vs[0].Rules[0].Graph.Queries["A"]
	}
	// Refused, Grafana's 15s; read an hour on, the data source's.
	if q := query(); q != "errors > 5 and on() vector(60000)" || asked != 1 {
		t.Errorf("refused: %s, asked %d times", q, asked)
	}
	refuse = false
	later := s.now().Add(61 * time.Minute)
	s.now = func() time.Time { return later }
	if q := query(); q != "errors > 5 and on() vector(120000)" || asked != 2 {
		t.Errorf("%s, asked %d times", q, asked)
	}
}

// askedHistory answers as history does, keeps what it was asked, and
// serves Prometheus's configuration.
type askedHistory struct {
	*history
	mu    sync.Mutex
	asked []string
}

func (h *askedHistory) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	h.mu.Lock()
	h.asked = append(h.asked, expr)
	h.mu.Unlock()
	return h.history.QueryRange(ctx, expr, start, end, step)
}

func (h *askedHistory) StatusConfig(context.Context) (string, error) {
	return "global:\n  scrape_interval: 1m\nscrape_configs:\n  - job_name: fid\n    scrape_interval: 15s\n", nil
}

// F2: without --scrape-interval, serve's runs ask a Grafana reduce the
// server works out at the interval Prometheus's configuration says the
// series it reads are scraped at, the range check on or not.
func TestServeReplayScrape(t *testing.T) {
	ctx := context.Background()
	s, dir := server(t, nil)
	os.Remove(filepath.Join(dir, "shop.yaml"))
	os.WriteFile(filepath.Join(dir, "max.yaml"), []byte(`apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    interval: 1m
    rules:
      - uid: cmax
        title: C max
        condition: C
        data:
          - {refId: A, datasourceUid: prom, relativeTimeRange: {from: 60, to: 0}, model: {expr: 'fv{job="fid"}', intervalMs: 1000, maxDataPoints: 43200, range: true}}
          - refId: C
            datasourceUid: __expr__
            model: {type: classic_conditions, conditions: [{evaluator: {type: gt, params: [50]}, operator: {type: and}, query: {params: [A]}, reducer: {type: max}}]}
`), 0o644)
	h := &askedHistory{history: &history{}}
	s.h = h
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	want := `max_over_time(((fv{job="fid"}) >= -Inf)[1m7s500ms:15s])`
	h.mu.Lock()
	defer h.mu.Unlock()
	if !slices.Contains(h.asked, want) {
		t.Errorf("asked %q, want %q", h.asked, want)
	}
}
