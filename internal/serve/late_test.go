package serve

import (
	"context"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

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
