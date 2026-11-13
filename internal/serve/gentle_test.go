package serve

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/source"
)

// TestTurnedAwayKept: a rule whose query was turned away to the end is
// kept as it was, and why is kept in the ledger without how often it was
// asked, which differs from one run to the next.
func TestTurnedAwayKept(t *testing.T) {
	ctx := context.Background()
	s, rules := server(t, func(c *Config) { c.Backfill = 3 * time.Hour })
	os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte(ruleFile+"      - alert: Turned\n        expr: turned\n"), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	table, err := s.l.RuleTable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var why string
	for _, r := range table {
		if r.Name == "Turned" {
			why = r.Unsupported
		}
	}
	if why != "query failed: the history couldn't be read: HTTP 429: slow down" {
		t.Errorf("kept %q", why)
	}
	missed, _ := s.l.Missed(ctx)
	if _, ok := missed["Turned"]; !ok {
		t.Errorf("missed %v: Turned should be replayed again", missed)
	}
}

// TestLokiRulerShares: lapilli serve asks a Loki's ruler where it runs its
// groups, each minute, by the Limiter its queries are asked by (--max-rate,
// and its push back).
func TestLokiRulerShares(t *testing.T) {
	var ruler atomic.Int64
	lokiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/prometheus/api/v1/rules" {
			ruler.Add(1)
		}
		fmt.Fprint(w, `{"status":"success","data":{"groups":[]}}`)
	}))
	defer lokiSrv.Close()
	l := prom.NewLimiter(1) // a request a second
	s, _ := server(t, func(c *Config) { c.Loki, c.LokiLoaded, c.LokiLimit = lokiSrv.URL, true, l })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	for range 3 {
		if _, _, err := s.extras(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Three, a second apart.
	if took := time.Since(start); ruler.Load() != 3 || took < 1900*time.Millisecond {
		t.Errorf("the ruler asked %d times in %s, at a request a second", ruler.Load(), took)
	}
}

// TestLokiPausedLooks: while a Loki's queries are told to wait, lapilli
// serve's read of the rules each minute doesn't wait with them: where its
// ruler runs its groups isn't asked, and is kept as read before, and a
// change to the Prometheus rules is recorded all the same.
func TestLokiPausedLooks(t *testing.T) {
	var places atomic.Int64
	lokiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/loki/api/v1/query_range":
			w.Header().Set("Retry-After", "60")
			http.Error(w, "too many outstanding requests", http.StatusTooManyRequests)
		case "/loki/api/v1/rules":
			fmt.Fprint(w, "shop:\n    - name: api\n      rules:\n        - alert: LogErrors\n          expr: sum(count_over_time({app=\"c\"} |= \"error\" [5m])) > 5\n")
		case "/prometheus/api/v1/rules":
			places.Add(1)
			fmt.Fprint(w, `{"status":"success","data":{"groups":[{"name":"api","file":"shop","interval":60,"lastEvaluation":"2026-11-13T09:00:17Z"}]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer lokiSrv.Close()
	l := prom.NewLimiter(0)
	s, rulesDir := server(t, func(c *Config) { c.Loki, c.LokiLoaded, c.LokiLimit = lokiSrv.URL, true, l })
	ctx := context.Background()
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	want := map[rules.Place]time.Duration{{Namespace: "shop", Group: "api"}: 17 * time.Second}
	if !maps.Equal(s.places.m, want) || places.Load() != 1 {
		t.Fatalf("places %v, asked %d times", s.places.m, places.Load())
	}
	// A replay's query is told to wait a minute.
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	t0 := time.Now().Add(-time.Hour)
	if _, err := (source.Loki{URL: lokiSrv.URL, Limit: l}).History("t").QueryRange(qctx, `sum(count_over_time({a="b"}[1m]))`, t0, t0.Add(time.Hour), time.Minute); err == nil {
		t.Fatal("want the 429")
	}
	// Meanwhile the Prometheus rules change.
	os.WriteFile(filepath.Join(rulesDir, "shop.yaml"), []byte(ruleFile+"      - alert: New\n        expr: up == 0\n"), 0o644)
	was, start := raw, time.Now()
	if err := s.look(ctx, &raw); err != nil || raw == was || time.Since(start) > 10*time.Second {
		t.Fatalf("the look, beside a Loki told to wait: %v after %s, recorded %v", err, time.Since(start), raw != was)
	}
	if !maps.Equal(s.places.m, want) || places.Load() != 1 {
		t.Errorf("places %v, asked %d times: want kept, not asked", s.places.m, places.Load())
	}
}
