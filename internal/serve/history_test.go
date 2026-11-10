package serve

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// historyDir writes versions of the rules, each with its "for", into a
// directory of the kind --rules-history reads.
func historyDir(t *testing.T, versions map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, forDur := range versions {
		v := filepath.Join(dir, name)
		if err := os.MkdirAll(v, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(v, "shop.yaml"), []byte(strings.Replace(ruleFile, "for: 2m", "for: "+forDur, 1)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// searchFirings are the ledger's firings of the search alert in the
// backfill: the history answers it true for ten minutes in each of the
// twenty hours from 50h ago, so a "for" of 2m fires each time and one of
// 15m never does.
func searchFirings(t *testing.T, s *Server) []ledger.Firing {
	t.Helper()
	fs, err := s.l.Firings(context.Background(), ledger.FiringQuery{From: now.Add(-72 * time.Hour), To: now})
	if err != nil {
		t.Fatal(err)
	}
	var out []ledger.Firing
	for _, f := range fs {
		if f.Labels["service"] == "search" {
			out = append(out, f)
		}
	}
	return out
}

// A first start with --rules-history replays each day of its backfill with
// that day's rules: a rule's "for" was raised 40 hours ago, and the firings
// before then are judged with the "for" it had.
func TestRulesHistory(t *testing.T) {
	ctx := context.Background()
	tuned := now.Add(-40 * time.Hour)
	hist := historyDir(t, map[string]string{
		now.Add(-100 * time.Hour).Format(time.RFC3339): "30m", // replaced before the backfill starts: not needed
		now.Add(-90 * time.Hour).Format(time.RFC3339):  "2m",  // in effect when the backfill starts
		tuned.Format("20060102T150405Z"):               "15m", // raised since
		now.Add(-20 * time.Hour).Format(time.RFC3339):  "15m", // the same again: no version
		".data": "2m", // a mounted volume's own: passed over
	})
	s, rulesDir := server(t, func(c *Config) { c.RulesHistory = hist })
	// The rules as they are now are the newest version's.
	if err := os.WriteFile(filepath.Join(rulesDir, "shop.yaml"), []byte(strings.Replace(ruleFile, "for: 2m", "for: 15m", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.history(ctx); err != nil {
		t.Fatal(err)
	}
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	vs, err := s.l.Versions(ctx, time.Unix(0, 0), now, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 2 || !vs[0].Since.IsZero() || !vs[1].Since.Equal(tuned) {
		var got []string
		for _, v := range vs {
			got = append(got, v.Since.String())
		}
		t.Fatalf("versions from %q, want from before the ledger, and from %s", got, tuned)
	}
	s.jobs(ctx, true)
	fs := searchFirings(t, s)
	if len(fs) != 10 {
		t.Errorf("%d firings of the search alert, want the 10 before the change", len(fs))
	}
	for _, f := range fs {
		if !f.Start.Before(tuned) {
			t.Errorf("a firing at %s, after the change", f.Start)
		}
	}
	// A later start passes the directory over: the ledger keeps its own.
	if err := os.MkdirAll(filepath.Join(hist, now.Add(-10*time.Hour).Format(time.RFC3339)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.history(ctx); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.l.Versions(ctx, time.Unix(0, 0), now, false); len(again) != 2 {
		t.Errorf("a later start made %d versions", len(again))
	}

	// Without the history, the same backfill judges every hour with today's
	// rules, and the search alert never fires.
	s2, rulesDir2 := server(t, nil)
	os.WriteFile(filepath.Join(rulesDir2, "shop.yaml"), []byte(strings.Replace(ruleFile, "for: 2m", "for: 15m", 1)), 0o644)
	if err := s2.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	s2.jobs(ctx, true)
	if fs := searchFirings(t, s2); len(fs) != 0 {
		t.Errorf("without the history, %d firings of the search alert", len(fs))
	}
}

// A history that can't be read stops the start, and says where.
func TestRulesHistoryRefused(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		dir   func() string
		says  string
		named string
	}{
		{"a name that isn't a time", func() string {
			return historyDir(t, map[string]string{"last-week": "2m"})
		}, "named by when it took effect", "last-week"},
		{"rules that don't read, after some that do", func() string {
			dir := historyDir(t, map[string]string{now.Add(-100 * time.Hour).Format(time.RFC3339): "2m", now.Add(-10 * time.Hour).Format(time.RFC3339): "2m"})
			os.WriteFile(filepath.Join(dir, now.Add(-10*time.Hour).Format(time.RFC3339), "shop.yaml"), []byte("groups: [ {"), 0o644)
			return dir
		}, "", now.Add(-10 * time.Hour).Format(time.RFC3339)},
		{"a version with no rules", func() string {
			dir := t.TempDir()
			os.MkdirAll(filepath.Join(dir, "20261030T080000Z"), 0o755)
			return dir
		}, "", "20261030T080000Z"},
	} {
		dir := c.dir()
		s, _ := server(t, func(cfg *Config) { cfg.RulesHistory = dir })
		err := s.history(ctx)
		if err == nil || !strings.Contains(err.Error(), c.says) || !strings.Contains(err.Error(), c.named) || !strings.Contains(err.Error(), "--rules-history") {
			t.Errorf("%s: %v", c.name, err)
		}
		if vs, _ := s.l.Versions(ctx, time.Unix(0, 0), now, false); len(vs) != 0 {
			t.Errorf("%s: kept %d versions", c.name, len(vs))
		}
	}
}

// What a first start passes over, rather than stop on: versions from after
// now (a commit dated ahead of the clock), and a directory left empty, as
// when git couldn't be reached. Each warns, and today's rules are taken; a
// later start reads nothing at all, so an unreadable directory can't stop
// it either.
func TestRulesHistoryPassedOver(t *testing.T) {
	ctx := context.Background()
	dir := historyDir(t, map[string]string{now.Add(-100 * time.Hour).Format(time.RFC3339): "2m", now.Add(time.Hour).Format(time.RFC3339): "15m"})
	s, _ := server(t, func(cfg *Config) { cfg.RulesHistory = dir })
	if err := s.history(ctx); err != nil {
		t.Fatalf("a version from after now: %v", err)
	}
	vs, _ := s.l.Versions(ctx, time.Unix(0, 0), now.Add(2*time.Hour), false)
	if len(vs) != 1 || !vs[0].Since.IsZero() {
		t.Errorf("versions: %+v; want the one from before now alone", vs)
	}
	empty := t.TempDir()
	s2, _ := server(t, func(cfg *Config) { cfg.RulesHistory = empty })
	if err := s2.history(ctx); err != nil {
		t.Errorf("an empty directory: %v", err)
	}
	if vs, _ := s2.l.Versions(ctx, time.Unix(0, 0), now, false); len(vs) != 0 {
		t.Errorf("an empty directory kept %d versions", len(vs))
	}
	// A later start: the ledger has its versions; a directory that can't
	// be read now doesn't matter.
	s.cfg.RulesHistory = filepath.Join(dir, "gone")
	if err := s.history(ctx); err != nil {
		t.Errorf("a later start with no directory: %v", err)
	}
}
