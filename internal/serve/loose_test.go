package serve

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if v, _ := s.l.Meta(ctx, linkedMeta); v != "m" {
		t.Errorf("the ledger was last judged by %q, want m", v)
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
