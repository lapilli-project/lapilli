package serve

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/rules"
)

// addVersion writes a version of the rules into a history, with its "for".
func addVersion(t *testing.T, hist string, at time.Time, forDur string) {
	t.Helper()
	v := filepath.Join(hist, at.Format("20060102T150405Z"))
	if err := os.MkdirAll(v, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v, "rules.yaml"), []byte(renderedVersion(forDur)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// With --rules-history and no --rules, serve follows the history: a first
// start takes it as with --rules, its newest version the rules in effect;
// a version added later is recorded from its own time, and the hours since
// then, already replayed, are replayed again with it. One from after now
// waits for its time; one from before the last followed is passed over.
func TestFollow(t *testing.T) {
	ctx := context.Background()
	hist := t.TempDir()
	tuned := now.Add(-40 * time.Hour)
	addVersion(t, hist, now.Add(-90*time.Hour), "2m")
	addVersion(t, hist, tuned, "15m")
	sel, _ := rules.ParseSelector("grafana_alert=1")
	s, _ := server(t, func(c *Config) { c.RulesPath, c.RulesHistory, c.RulesSelect = "", hist, sel })
	if !s.following() || !s.replaying() {
		t.Fatal("not following")
	}
	if err := s.history(ctx); err != nil {
		t.Fatal(err)
	}
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	vs, err := s.l.Versions(ctx, time.Unix(0, 0), now, false)
	if err != nil || len(vs) != 2 || !vs[1].Since.Equal(tuned) {
		t.Fatalf("versions %+v %v", vs, err)
	}
	s.jobs(ctx, true)
	if fs := searchFirings(t, s); len(fs) != 10 {
		t.Fatalf("%d firings of the search alert, want the 10 before the change", len(fs))
	}

	// A commit from 35 hours ago, fetched now: its "for" of 2m fires the
	// search alert in the five hours from then, which were replayed with
	// 15m. From after now: not yet.
	lowered := now.Add(-35 * time.Hour)
	addVersion(t, hist, lowered, "2m")
	addVersion(t, hist, now.Add(time.Hour), "30m")
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	vs, _ = s.l.Versions(ctx, time.Unix(0, 0), now.Add(2*time.Hour), false)
	if len(vs) != 3 || !vs[2].Since.Equal(lowered) {
		t.Fatalf("versions %+v", vs)
	}
	if again, err := s.l.AgainFrom(ctx); err != nil || !again.Equal(lowered) {
		t.Errorf("again from %v %v, want %v", again, err, lowered)
	}
	if latest, _, _ := s.l.Latest(ctx); latest.ID != vs[2].ID {
		t.Errorf("the rules in effect are version %d, want %d", latest.ID, vs[2].ID)
	}
	s.jobs(ctx, true)
	if fs := searchFirings(t, s); len(fs) != 15 {
		t.Errorf("%d firings of the search alert, want 15: the five since %s replayed again", len(fs), lowered)
	}
	if again, _ := s.l.AgainFrom(ctx); !again.IsZero() {
		t.Errorf("still to replay again from %v", again)
	}

	// One that changes nothing: no version, but followed.
	same := now.Add(-30 * time.Hour)
	addVersion(t, hist, same, "2m")
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.l.Meta(ctx, "followed"); v != strconv.FormatInt(same.Unix(), 10) {
		t.Errorf("followed to %s, want %d", v, same.Unix())
	}

	// One from before the last followed, added late: passed over.
	addVersion(t, hist, now.Add(-38*time.Hour), "45m")
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.l.Versions(ctx, time.Unix(0, 0), now, false); len(again) != 3 {
		t.Errorf("%d versions after one from before the last", len(again))
	}

	// A later start: the ledger keeps its own, and doesn't say it passes
	// the directory over: it follows it.
	var logged bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logged, nil))
	if err := s.history(ctx); err != nil || strings.Contains(logged.String(), "passed over") {
		t.Errorf("%v: %s", err, logged.String())
	}
}

// A version from before --backfill, added late, is replayed again from
// --backfill's start at most; a history with nothing in it yet leaves the
// rules unread, and runs wait.
func TestFollowBounds(t *testing.T) {
	ctx := context.Background()
	hist := t.TempDir()
	sel, _ := rules.ParseSelector("grafana_alert=1")
	s, _ := server(t, func(c *Config) { c.RulesPath, c.RulesHistory, c.RulesSelect = "", hist, sel })
	if err := s.history(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.look(ctx, new(string)); err == nil || !strings.Contains(err.Error(), "--rules-history") {
		t.Errorf("an empty history: %v", err)
	}
	addVersion(t, hist, now.Add(-100*time.Hour), "15m")
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	addVersion(t, hist, now.Add(-99*time.Hour), "2m")
	// Followed after the one from before the ledger began.
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	if again, err := s.l.AgainFrom(ctx); err != nil || !again.Equal(now.Add(-s.cfg.Backfill)) {
		t.Errorf("again from %v %v, want --backfill's start", again, err)
	}
}

// A ledger that read --rules, then follows a history: what the history
// holds from before the runs began isn't taken, only what came after.
func TestFollowAfterRules(t *testing.T) {
	ctx := context.Background()
	hist := t.TempDir()
	addVersion(t, hist, now.Add(-100*time.Hour), "15m")
	addVersion(t, hist, now.Add(-10*time.Hour), "30m")
	sel, _ := rules.ParseSelector("grafana_alert=1")
	s, _ := server(t, func(c *Config) { c.RulesSelect = sel })
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	s.cfg.RulesPath, s.cfg.RulesHistory = "", hist
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	vs, err := s.l.Versions(ctx, time.Unix(0, 0), now, false)
	if err != nil || len(vs) != 2 || !vs[0].Since.IsZero() || !vs[1].Since.Equal(now.Add(-10*time.Hour)) {
		t.Errorf("versions %+v %v", vs, err)
	}
}
