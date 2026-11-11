package source

import (
	"testing"
	"time"
)

// A data source's scrape interval as Grafana writes one; what can't be
// read is none, Grafana's 15s.
func TestGrafanaInterval(t *testing.T) {
	for in, want := range map[string]time.Duration{"30s": 30 * time.Second, "1m": time.Minute, "500ms": 500 * time.Millisecond,
		"15": 15 * time.Second, "": 0, "soon": 0, "-5s": 0, "1e9": 0} {
		if got := grafanaInterval(in); got != want {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
	}
}
