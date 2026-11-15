package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// TestProposalsNowScale: a thousand proposals of a longer for, each with
// its backtest, two thirds waiting for approval, against a thousand rules
// that count events. What is worked out of each when it is read (whether
// it is stale, whether its for outlasts one event, whether its backtest's
// period had a record) keeps the proposals list, its stale ones, and the
// API's list, worked out afresh, each under a second; the times are
// logged, the median of seven.
func TestProposalsNowScale(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	ctx := context.Background()
	s, dir := server(t, nil)
	const n = 1000
	var b strings.Builder
	b.WriteString("groups:\n")
	for g := 0; g < 10; g++ {
		fmt.Fprintf(&b, "  - name: g%d\n    rules:\n", g)
		for i := g; i < n; i += 10 {
			fmt.Fprintf(&b, "      - {alert: Restarts%04d, expr: 'increase(restarts_total{i=\"%d\"}[30m]) > 0.5', for: 1m}\n", i, i)
		}
	}
	os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(b.String()), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	latest, _, err := s.l.Latest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	all, names, err := s.named(ctx, latest.ID)
	if err != nil {
		t.Fatal(err)
	}
	states := []string{ledger.StateApproved, ledger.StateApplied, ledger.StateDropped}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("Restarts%04d", i)
		j := slices.Index(names, name)
		if j < 0 {
			t.Fatalf("no %s", name)
		}
		ev := map[string]any{"from": "suggestion", "for_ns": time.Hour, "edit": map[string]any{"name": name, "group": all[j].Group, "for": time.Hour},
			"backtest": map[string]any{"from": now.Add(-12 * 7 * 24 * time.Hour), "to": now.Add(-time.Hour),
				"before": map[string]int{"firings": 141, "white": 0}, "after": map[string]int{"firings": 3, "white": 0},
				"lost": []string{}, "gained": []string{}, "later": []string{}}}
		if i%2 == 0 {
			ev["version"] = "test"
		}
		raw, _ := json.Marshal(ev)
		p := ledger.Proposal{Rule: name, Kind: ledger.KindFor, Path: ledger.PathGit, MadeBy: fmt.Sprint("p", i%80, "@example.com"),
			Before: "for: 1m\n", After: "for: 1h\n", Diff: "-for: 1m\n+for: 1h\n", BeforeHash: ruleHash(all[j]), Evidence: raw}
		id, err := s.l.Propose(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if i%3 == 0 {
			if _, err := s.l.Move(ctx, id, []string{ledger.StateProposed}, states[i%len(states)], "ana", ledger.Step{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	h := s.Handler()
	for _, path := range []string{"/proposals", "/proposals?state=all", "/proposals?stale=1", "/api/v1/proposals", "/api/v1/proposals?state=proposed"} {
		var took []time.Duration
		var w *httptest.ResponseRecorder
		for range 7 {
			s.cache.Lock()
			s.cache.gen = ""
			s.cache.Unlock()
			began := time.Now()
			w = httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			took = append(took, time.Since(began))
		}
		slices.Sort(took)
		t.Logf("%-40s %d %8d bytes  median %s (fastest %s, slowest %s)", path, w.Code, w.Body.Len(), took[3].Round(time.Microsecond),
			took[0].Round(time.Microsecond), took[6].Round(time.Microsecond))
		if w.Code != http.StatusOK {
			t.Errorf("%s: %d", path, w.Code)
		}
		if budget := time.Duration(float64(time.Second) * slow); took[3] > budget {
			t.Errorf("%s took %s, over %s", path, took[3], budget)
		}
	}
}
