package serve

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
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
	if why != "query failed for now: HTTP 429: slow down" {
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
