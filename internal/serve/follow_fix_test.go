package serve

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/ledger/ledgertest"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// writeVersion writes a rule file as a version of a history.
func writeVersion(t *testing.T, hist string, at time.Time, body string) {
	t.Helper()
	v := filepath.Join(hist, at.Format("20060102T150405Z"))
	if err := os.MkdirAll(v, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v, "rules.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// start is what Run does before it serves: the history, then a look.
func (s *Server) start(ctx context.Context) error {
	if err := s.history(ctx); err != nil {
		return err
	}
	return s.look(ctx, new(string))
}

// Following never stops on one version: a commit that renders no rule
// (a Deployment alone), whose ConfigMaps lost the selected label, or whose
// rules don't parse is passed over, said once, and the version before
// stays in effect; the versions after it are recorded as they come.
func TestFollowPassesUnreadable(t *testing.T) {
	ctx := context.Background()
	hist := t.TempDir()
	addVersion(t, hist, now.Add(-90*time.Hour), "2m")
	addVersion(t, hist, now.Add(-40*time.Hour), "15m")
	sel, _ := rules.ParseSelector("grafana_alert=1")
	var logged bytes.Buffer
	s, _ := server(t, func(c *Config) { c.RulesPath, c.RulesHistory, c.RulesSelect = "", hist, sel })
	s.log = slog.New(slog.NewTextHandler(&logged, nil))
	if err := s.start(ctx); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	writeVersion(t, hist, now.Add(-36*time.Hour), "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: shop}\n")
	writeVersion(t, hist, now.Add(-35*time.Hour), strings.Replace(renderedVersion("2m"), `grafana_alert: "1"`, `grafana_alert: "0"`, 1))
	writeVersion(t, hist, now.Add(-34*time.Hour), "groups: [ {")
	fixed := now.Add(-20 * time.Hour)
	addVersion(t, hist, fixed, "2m")
	raw := ""
	for i := range 3 {
		if err := s.look(ctx, &raw); err != nil {
			t.Fatalf("look %d: %v", i, err)
		}
	}
	vs, _ := s.l.Versions(ctx, time.Unix(0, 0), now, false)
	if len(vs) != 3 || !vs[2].Since.Equal(fixed) {
		t.Errorf("%d versions, the latest from %v: want the fix, from %v", len(vs), vs[len(vs)-1].Since, fixed)
	}
	if n := strings.Count(logged.String(), "a version that can't be read is passed over"); n != 3 {
		t.Errorf("said %d times, want once each of 3:\n%s", n, logged.String())
	}
	// Until the fix came, the version before stayed in effect: its rules
	// are the newest that read.
	s2, _ := server(t, func(c *Config) { c.RulesPath, c.RulesHistory, c.RulesSelect = "", hist, sel })
	os.RemoveAll(filepath.Join(hist, fixed.Format("20060102T150405Z")))
	rs, _, err := s2.readRules(ctx)
	if err != nil || len(rs) != 1 || rs[0].For != 15*time.Minute {
		t.Errorf("the rules in effect: %+v %v", rs, err)
	}
}

// A version that reads and one that doesn't, found in one look: the one
// that reads is recorded and its hours replayed again.
func TestFollowGoodThenBad(t *testing.T) {
	ctx := context.Background()
	hist := t.TempDir()
	addVersion(t, hist, now.Add(-90*time.Hour), "15m")
	sel, _ := rules.ParseSelector("grafana_alert=1")
	s, _ := server(t, func(c *Config) { c.RulesPath, c.RulesHistory, c.RulesSelect = "", hist, sel })
	if err := s.start(ctx); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	good := now.Add(-48 * time.Hour)
	addVersion(t, hist, good, "2m") // fires the search alert from then
	writeVersion(t, hist, now.Add(-30*time.Hour), "apiVersion: v1\nkind: Service\nmetadata: {name: s}\n")
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	if again, _, _ := s.l.AgainFrom(ctx); !again.Equal(good) {
		t.Errorf("again from %v, want %v", again, good)
	}
	s.jobs(ctx, true)
	if n := len(searchFirings(t, s)); n != 18 {
		t.Errorf("%d search firings, want the 18 since %v", n, good)
	}
}

// gated holds the first replay of "errors > 5" until released: a run
// under way.
type gated struct {
	history
	on       bool
	once     sync.Once
	entered  chan struct{}
	released chan struct{}
}

func (g *gated) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if g.on && expr == "errors > 5" {
		g.once.Do(func() {
			close(g.entered)
			<-g.released
		})
	}
	return g.history.QueryRange(ctx, expr, start, end, step)
}

func followServer(t *testing.T, h replay.History, hist string) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{
		RulesHistory: hist, Impact: "impact", ImpactFor: 5 * time.Minute,
		Options: verdict.Options{Lead: 15 * time.Minute, RefireWithin: time.Hour, UnknownLen: time.Hour, Match: []string{"service"}},
		Step:    time.Minute, Loc: time.UTC, Ledger: filepath.Join(dir, "ledger"), LedgerURLFile: ledgertest.File(t),
		Every: time.Hour, Settle: time.Hour, Backfill: 3 * 24 * time.Hour, Watch: time.Minute,
		Digest: Weekly{time.Monday, 9, 0}, Timeout: time.Minute, Parallel: 4, Listen: "127.0.0.1:0", Version: "test",
		AllowedHosts: []string{"example.com"},
	}
	s, err := New(context.Background(), cfg, h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func checkoutFirings(t *testing.T, s *Server) []ledger.Firing {
	t.Helper()
	fs, err := s.l.Firings(context.Background(), ledger.FiringQuery{From: now.Add(-72 * time.Hour), To: now})
	if err != nil {
		t.Fatal(err)
	}
	var out []ledger.Firing
	for _, f := range fs {
		if f.Labels["service"] == "checkout" {
			out = append(out, f)
		}
	}
	return out
}

// A version recorded by the watcher while the worker's run, begun before
// it, is replaying its hours with the rules before: the run's end doesn't
// forget it, and the next run replays those hours with it, as if no run
// had been under way.
func TestFollowDuringRun(t *testing.T) {
	ctx := context.Background()
	hist := t.TempDir()
	v := func(forDur string) string { return strings.Replace(ruleFile, "for: 2m", "for: "+forDur, 1) }
	writeVersion(t, hist, now.Add(-90*time.Hour), v("2m"))
	h := &gated{entered: make(chan struct{}), released: make(chan struct{})}
	s := followServer(t, h, hist)
	s.now = func() time.Time { return now.Add(-32 * time.Hour) }
	if err := s.start(ctx); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true) // the runs got to 32 hours ago
	s.now = func() time.Time { return now }
	h.on = true
	done := make(chan struct{})
	go func() { s.jobs(ctx, true); close(done) }()
	<-h.entered
	// Meanwhile, a commit from 31 hours ago raises "for" to 2h, under
	// which the hour-long checkout firing 30 hours ago never fires.
	writeVersion(t, hist, now.Add(-31*time.Hour), v("2h"))
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	close(h.released)
	<-done
	s.jobs(ctx, true) // the next tick
	got := len(checkoutFirings(t, s))

	s2 := followServer(t, &gated{}, hist)
	if err := s2.start(ctx); err != nil {
		t.Fatal(err)
	}
	s2.jobs(ctx, true)
	if want := len(checkoutFirings(t, s2)); got != want {
		t.Errorf("%d checkout firings with the version recorded during a run, %d without", got, want)
	}
}

// A day checked again after a version is followed keeps only what the
// check finds then: a miss isn't kept twice, and a rule the version
// dropped keeps no check of the days it no longer has firings on.
func TestFollowChecksAgain(t *testing.T) {
	ctx := context.Background()
	hist := t.TempDir()
	two := ruleFile + "      - alert: Busy\n        expr: busy > 1\n        labels: {severity: page}\n"
	writeVersion(t, hist, now.Add(-90*time.Hour), two)
	s, _ := server(t, func(c *Config) { c.RulesPath, c.RulesHistory, c.CheckReplay = "", hist, true })
	s.h = &alertsHistory{}
	if err := s.start(ctx); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	if b, _ := s.l.Checks(ctx, now.Add(-7*24*time.Hour), now); b["Busy"].Replayed == 0 {
		t.Fatalf("Busy wasn't checked: %+v", b)
	}
	// A commit 45 hours ago: Busy dropped, Errors as it was but for its
	// group's interval.
	writeVersion(t, hist, now.Add(-45*time.Hour), strings.Replace(ruleFile, "    rules:", "    interval: 1m\n    rules:", 1))
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	ms, _ := s.l.CheckMisses(ctx, "Errors")
	seen := map[string]int{}
	for _, m := range ms {
		seen[m.Way+" "+m.Start.String()+" "+m.Labels["service"]]++
	}
	for k, n := range seen {
		if n > 1 {
			t.Errorf("a miss kept %d times: %s", n, k)
		}
	}
	busy, _ := s.l.Firings(ctx, ledger.FiringQuery{From: now.Add(-72 * time.Hour), To: now, Rule: "Busy"})
	b, _ := s.l.Checks(ctx, now.Add(-7*24*time.Hour), now)
	if len(busy) == 0 && b["Busy"].Replayed > 0 {
		t.Errorf("Busy has no firing left, but its check says %d replayed", b["Busy"].Replayed)
	}
}

// Following beside --loki-rules, each version of the history is read with
// Loki's rules beside it, as the rules read now are: a commit from five
// hours ago takes effect from then, not when it is seen.
func TestFollowBesideLoki(t *testing.T) {
	ctx := context.Background()
	lokiDir := filepath.Join(t.TempDir(), "loki")
	os.MkdirAll(lokiDir, 0o755)
	os.WriteFile(filepath.Join(lokiDir, "logs.yaml"), []byte("groups:\n  - name: logs\n    rules:\n      - alert: LogErrors\n        expr: sum(count_over_time({app=\"x\"} |= \"error\" [5m])) > 5\n"), 0o644)
	hist := t.TempDir()
	v := func(forDur string) string { return strings.Replace(ruleFile, "for: 2m", "for: "+forDur, 1) }
	writeVersion(t, hist, now.Add(-90*time.Hour), v("2m"))
	s, _ := server(t, func(c *Config) {
		c.RulesPath, c.RulesHistory, c.Loki, c.LokiRules = "", hist, "http://loki:3100", lokiDir
	})
	if err := s.start(ctx); err != nil {
		t.Fatal(err)
	}
	commit := now.Add(-5 * time.Hour)
	writeVersion(t, hist, commit, v("15m"))
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	vs, _ := s.l.Versions(ctx, time.Unix(0, 0), now, true)
	if len(vs) != 2 || !vs[1].Since.Equal(commit) {
		t.Fatalf("versions %+v, want one from %v", vs, commit)
	}
	for _, x := range vs {
		rs, _ := rules.FromDocs(x.Docs)
		if len(rs) != 2 {
			t.Errorf("version %d holds %d rules, want the history's and Loki's", x.ID, len(rs))
		}
	}
	// Loki's rules changed now make a version from now; a commit from three
	// hours ago, fetched after, still takes effect from its time.
	os.WriteFile(filepath.Join(lokiDir, "logs.yaml"), []byte("groups:\n  - name: logs\n    rules:\n      - alert: LogErrors\n        expr: sum(count_over_time({app=\"x\"} |= \"error\" [5m])) > 9\n"), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	later := now.Add(-3 * time.Hour)
	writeVersion(t, hist, later, v("30m"))
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	vs, _ = s.l.Versions(ctx, time.Unix(0, 0), now, false)
	found := false
	for _, x := range vs {
		found = found || x.Since.Equal(later)
	}
	if !found {
		t.Errorf("no version from %v, the commit's time, after one recorded now for Loki's rules", later)
	}
}
