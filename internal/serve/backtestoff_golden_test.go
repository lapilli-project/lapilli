package serve

import (
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// updateBacktestOff writes testdata/backtestoff afresh. It was run once,
// on main as it was before the backtest port (ab023bd1, taken out with git
// archive and this file dropped in), so that lapilli serve without
// --backtest-listen is held to the release before it, byte for byte:
// off, nothing of the port changes what serve shows
// (docs/design/backtests.md).
var updateBacktestOff = flag.Bool("update-backtest-off", false, "write testdata/backtestoff afresh")

// backtestOffSeries are the series of /metrics whose values time
// something, or say when, by the clock of the machine the test runs on
// (the last run's time taken, a source's last copy-in, the waits for the
// history), not by the runs' now; backtestOffClock, a time a page says,
// which is that clock's when it is after the runs' now (a change made
// here, as the ledger keeps it).
var (
	backtestOffSeries = regexp.MustCompile(`(?m)^((?:lapilli_serve_run_duration_seconds|lapilli_serve_source_last_timestamp_seconds|lapilli_serve_history_wait_seconds_total)(?:\{[^}]*\})?) \S+$`)
	backtestOffClock  = regexp.MustCompile(`20[0-9]{2}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}(?::[0-9]{2}(?:\.[0-9]+)?)?(?:Z|[+-][0-9]{2}:[0-9]{2})?`)
)

// backtestOffNormal is a page or /metrics with what one run of the test
// gives otherwise than the next, on either side, stood in for by <clock>:
// the same on main and on this branch.
func backtestOffNormal(s string) string {
	s = backtestOffSeries.ReplaceAllString(s, "$1 <clock>")
	return backtestOffClock.ReplaceAllStringFunc(s, func(t string) string {
		if t[:10] > "2026-11-10" {
			return "<clock>"
		}
		return t
	})
}

// Without --backtest-listen, the Run history and /metrics are the release
// before's to the byte, but for what one run of the test gives otherwise
// than the next (backtestOffNormal): of a deployment just started, and of
// one with something on every page (pagesServer); in English and in
// another language; as anyone and as an admin.
func TestBacktestOffGolden(t *testing.T) {
	ctx := context.Background()
	plain, _ := server(t, func(c *Config) { c.UserHeader, c.Admins = "X-Forwarded-Email", []string{"ana@example.com"} })
	raw := ""
	if err := plain.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	plain.jobs(ctx, true)
	plain.ready.Store(true)
	full := pagesServer(t)
	full.ready.Store(true)
	dir := filepath.Join("testdata", "backtestoff")
	for _, c := range []struct {
		name string
		s    *Server
		path string
		lang string
		who  string
	}{
		{"plain-runs.html", plain, "/runs", "en", ""},
		{"plain-runs-admin-ko.html", plain, "/runs", "ko", "ana@example.com"},
		{"plain-metrics.prom", plain, "/metrics", "", ""},
		{"full-runs.html", full, "/runs", "en", ""},
		{"full-runs-es.html", full, "/runs", "es", ""},
		{"full-metrics.prom", full, "/metrics", "", ""},
	} {
		r := httptest.NewRequest(http.MethodGet, c.path, nil)
		r.Header.Set("Sec-Fetch-Site", "none")
		if c.lang != "" {
			r.Header.Set("Accept-Language", c.lang)
		}
		if c.who != "" {
			r.Header.Set("X-Forwarded-Email", c.who)
		}
		w := httptest.NewRecorder()
		c.s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", c.name, w.Code, w.Body.String())
		}
		got := backtestOffNormal(w.Body.String())
		path := filepath.Join(dir, c.name)
		if *updateBacktestOff {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if want := string(b); got != want {
			t.Errorf("%s: %s", c.name, firstDiff(want, got))
		}
	}
}
