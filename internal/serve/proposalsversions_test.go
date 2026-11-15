package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// againstVersions gives s, its rules in dir, m rules that count events,
// Many0000 and on, each for: 1m, and n proposals of a for of 1h, the k-th
// for Many<k> as version k had it, made while it was in effect: version
// k+1 has that rule's for 2m, so each was made against a version of its
// own, none the latest. Each is moved to state, unless that is proposed.
func againstVersions(t *testing.T, s *Server, dir string, n, m int, state string) []int64 {
	t.Helper()
	ctx := context.Background()
	at := now
	s.now = func() time.Time { return at }
	write := func(k int) {
		var b strings.Builder
		b.WriteString("groups:\n  - name: many\n    rules:\n")
		for i := 0; i < m; i++ {
			f := "1m"
			if i < k {
				f = "2m"
			}
			fmt.Fprintf(&b, "      - {alert: Many%04d, expr: 'increase(restarts_total{i=\"%d\"}[30m]) > 0.5', for: %s}\n", i, i, f)
		}
		if err := os.WriteFile(filepath.Join(dir, "many.yaml"), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		at = at.Add(time.Minute)
		if err := s.look(ctx, new(string)); err != nil {
			t.Fatal(err)
		}
	}
	hour := time.Hour
	var ids []int64
	for k := 0; k < n; k++ {
		write(k)
		name := fmt.Sprintf("Many%04d", k)
		f, err := s.current(ctx, name, false)
		if err != nil {
			t.Fatal(err)
		}
		want := f.rule
		want.For = hour
		ev, _ := json.Marshal(evidence{From: "suggestion", Edit: &rules.Edit{Name: name, Group: "many", For: &hour}, For: hour, Semantics: verdict.Semantics, Version: "test",
			Backtest: &backtest{From: at.Add(-weekLen), To: at, Before: side{Firings: 9}, After: side{Firings: 1}, Lost: []string{}, Gained: []string{}, Later: []string{}}})
		id, err := s.l.Propose(ctx, ledger.Proposal{Rule: name, Kind: ledger.KindFor, Path: ledger.PathGit, MadeBy: ledger.Proposer,
			Before: ruleText(f.rule), BeforeHash: ruleHash(f.rule), After: ruleText(want), AfterHash: ruleHash(want), Diff: "-for: 1m\n+for: 1h\n", Evidence: ev})
		if err != nil {
			t.Fatal(err)
		}
		ledgerExec(t, s, "UPDATE proposals SET made = ? WHERE id = ?", at.Unix(), id)
		if state != ledger.StateProposed {
			if _, err := s.l.Move(ctx, id, []string{ledger.StateProposed}, state, "ana", ledger.Step{}); err != nil {
				t.Fatal(err)
			}
		}
		ids = append(ids, id)
	}
	write(n)
	return ids
}
