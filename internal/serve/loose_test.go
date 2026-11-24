package serve

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// A rule whose firings carry no service catches an incident in checkout, as
// --match says when only one has the label, and the incident's page and the
// rule's mark the catch, in each language. With --match-strict, given at a
// start, every firing is judged again, once, and the catch is gone; the
// Settings page says which is in effect.
func TestLooseCatchServe(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, func(c *Config) {
		extra := "groups:\n  - name: nodes\n    rules:\n      - alert: DiskFull\n        expr: disk > 1\n        for: 2m\n"
		if err := os.WriteFile(filepath.Join(c.RulesPath, "nodes.yaml"), []byte(extra), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	h := s.Handler()
	_, page, _ := get(t, h, "/incidents/INC-1")
	if !strings.Contains(page, "DiskFull") || !strings.Contains(page, "(no service label)") {
		t.Errorf("incident page: want DiskFull caught, marked loose:\n%s", page)
	}
	if n := strings.Count(page, "(no service label)"); n != 1 {
		t.Errorf("incident page: %d catches marked loose, want 1 (Errors fired about checkout)", n)
	}
	if _, body, _ := get(t, h, "/rules/DiskFull"); !strings.Contains(body, "(no service label)") {
		t.Errorf("rule page: %s", body)
	}
	if _, body, _ := get(t, h, "/rules/Errors"); strings.Contains(body, "(no service label)") {
		t.Errorf("rule page of a rule that fired with the label: %s", body)
	}
	if ko := pageIn(t, h, "/incidents/INC-1", "", "ko"); !strings.Contains(ko, "(service 라벨 없음)") {
		t.Errorf("incident page in Korean: %s", ko)
	}
	_, js, _ := get(t, h, "/api/v1/settings")
	if !strings.Contains(js, `"match_strict": false`) {
		t.Errorf("settings: %s", js)
	}
	if _, st, _ := get(t, h, "/settings"); !strings.Contains(st, "<code>--match-strict</code>: Off") {
		t.Errorf("settings page: %s", st)
	}

	// A start with --match-strict.
	s.cfg.Options.MatchStrict = true
	s.jobs(ctx, false)
	s.jobs(ctx, false)
	if v, _ := s.l.Meta(ctx, linkedMeta); v != judgedBy+"m" {
		t.Errorf("the ledger was last judged by %q, want %sm", v, judgedBy)
	}
	_, page, _ = get(t, h, "/incidents/INC-1")
	if in, err := s.l.IncidentByID(ctx, "INC-1"); err != nil || len(in.CaughtBy) != 1 || in.CaughtBy[0].Rule != "Errors" || strings.Contains(page, "(no service label)") {
		t.Errorf("INC-1 with --match-strict: caught by %+v (%v), want Errors alone, nothing marked", in.CaughtBy, err)
	}
	_, js, _ = get(t, h, "/api/v1/settings")
	if !strings.Contains(js, `"match_strict": true`) || !strings.Contains(js, `"linked_only_judging": false`) {
		t.Errorf("settings: %s", js)
	}
	if _, st, _ := get(t, h, "/settings"); !strings.Contains(st, "<code>--match-strict</code>: On") {
		t.Errorf("settings page: %s", st)
	}
}

// A ledger judged before schema 032 has no loose mark on its catches, and
// the first start after the upgrade judges every firing again, once, so
// that serve shows the mark lapilli verdict does; a second start doesn't.
func TestLooseCatchAfterUpgrade(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, func(c *Config) {
		extra := "groups:\n  - name: nodes\n    rules:\n      - alert: DiskFull\n        expr: disk > 1\n        for: 2m\n"
		if err := os.WriteFile(filepath.Join(c.RulesPath, "nodes.yaml"), []byte(extra), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	h := s.Handler()
	// The ledger as 031 left it: nothing loose, and judged by the release
	// before, which wrote no version into meta.
	ledgerExec(t, s, `UPDATE catches SET loose = ''`)
	if err := s.l.SetMeta(ctx, linkedMeta, ""); err != nil {
		t.Fatal(err)
	}
	if _, page, _ := get(t, h, "/incidents/INC-1"); strings.Contains(page, "(no service label)") {
		t.Fatalf("before the upgrade's start, nothing is marked:\n%s", page)
	}
	n := 0
	was := judgeAll
	judgeAll = func(l *ledger.Ledger, ctx context.Context, o verdict.Options, done func(float64)) error {
		n++
		return was(l, ctx, o, done)
	}
	t.Cleanup(func() { judgeAll = was })
	start := func() {
		s.linkedKnown.Lock()
		s.linkedKnown.read = false
		s.linkedKnown.Unlock()
		s.logLinked(ctx)
		s.jobs(ctx, false)
	}
	start()
	if _, page, _ := get(t, h, "/incidents/INC-1"); n != 1 || strings.Count(page, "(no service label)") != 1 {
		t.Errorf("after the first start: judged all %d times, want 1; the mark:\n%s", n, page)
	}
	start()
	if n != 1 {
		t.Errorf("after the second start: judged all %d times, want still 1", n)
	}
}

// The mark names the labels that firings lacked, not all of --match: with
// --match service,team, a rule whose firings carry the service but no team
// is marked no team, and one with neither, no service, team.
func TestLooseCatchServeNamesTheLabelsLacked(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, func(c *Config) {
		extra := "groups:\n  - name: nodes\n    rules:\n      - alert: DiskFull\n        expr: disk > 1\n        for: 2m\n"
		if err := os.WriteFile(filepath.Join(c.RulesPath, "nodes.yaml"), []byte(extra), 0o644); err != nil {
			t.Fatal(err)
		}
		inc := "incidents:\n  - {id: INC-1, title: Checkout down, start: 2026-11-02T06:01:00Z, end: 2026-11-02T06:40:00Z, labels: {service: checkout, team: pay}}\n"
		if err := os.WriteFile(c.Incidents, []byte(inc), 0o644); err != nil {
			t.Fatal(err)
		}
		c.Options.Match = []string{"service", "team"}
	})
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	h := s.Handler()
	_, page, _ := get(t, h, "/incidents/INC-1")
	if !strings.Contains(page, "(no service, team label)") || !strings.Contains(page, "(no team label)") || strings.Contains(page, "(no service/team label)") {
		t.Errorf("incident page:\n%s", page)
	}
	if _, body, _ := get(t, h, "/rules/Errors"); !strings.Contains(body, "(no team label)") || strings.Contains(body, "(no service") {
		t.Errorf("rule page of a rule with the service: %s", body)
	}
}

// The bar for noisy is told as a round percentage: 0.29 is 29%, not
// 28.999999999999996%.
func TestPercentOf(t *testing.T) {
	for p, want := range map[float64]string{0.29: "29%", 0.07: "7%", 0.125: "12.5%", 0.5: "50%", 0.3: "30%", 0.1234: "12.34%", 1: "100%"} {
		if got := percentOf(p); got != want {
			t.Errorf("percentOf(%v) = %q, want %q", p, got, want)
		}
	}
}
