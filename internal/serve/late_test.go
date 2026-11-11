package serve

import (
	"context"
	"net/http/httptest"
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
