package rules

import (
	"testing"
	"time"
)

// TestDurationsAsPrometheus: durations read as Prometheus writes them, a
// week, a year, a day and a half: a namespaced Google Rules of for: 1w,
// which Prometheus and Google's managed service run, reads, where it was
// an error once its rules' errors were said.
func TestDurationsAsPrometheus(t *testing.T) {
	for in, want := range map[string]time.Duration{"1w": 7 * 24 * time.Hour, "1y": 365 * 24 * time.Hour, "1d12h": 36 * time.Hour, "2d": 48 * time.Hour,
		"1h30m": 90 * time.Minute, "90s": 90 * time.Second, "1.5h": 90 * time.Minute, "500ms": 500 * time.Millisecond, "0": 0} {
		if got, err := duration(in); err != nil || got != want {
			t.Errorf("%q: %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"1x", "5 minutes", "h", "1d1y", "-1h", "101y"} {
		if _, err := duration(in); err == nil {
			t.Errorf("%q read", in)
		}
	}
	doc := "apiVersion: monitoring.googleapis.com/v1\nkind: Rules\nmetadata: {name: r, namespace: shop}\nspec:\n  groups:\n  - name: g\n    rules:\n" +
		"    - {alert: Slow, expr: x > 1, for: 1w}\n    - {alert: Slower, expr: x > 2, for: 1d12h}\n"
	if rs, err := Parse([]byte(doc)); err != nil || len(rs) != 2 || rs[0].For != 7*24*time.Hour || rs[1].For != 36*time.Hour {
		t.Errorf("%+v, %v", rs, err)
	}
}
