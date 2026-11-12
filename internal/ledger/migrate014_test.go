package ledger

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
)

// A ledger at schema 13 with a team, a note, a daily window and
// firings, opened by this build, migrates to 014 and on, and judges the
// same.
func TestMigrate014(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "l.db")
	l, err := Open(ctx, path, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var fs []firing.Firing
	for d := 0; d < 14; d++ {
		s := base.AddDate(0, 0, d).Add(2*time.Hour + 30*time.Minute)
		fs = append(fs, firing.Firing{Rule: "Backup", Labels: map[string]string{"team": "a"}, Start: s, End: s.Add(5 * time.Minute)})
	}
	record(t, l, base, base.AddDate(0, 0, 14), fs)
	if _, err := l.PutTeam(ctx, Team{Name: "a", Rules: "^Backup"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutWindow(ctx, Window{Title: "nightly", Start: base.Add(2 * time.Hour), End: base.Add(3 * time.Hour), Every: "day"}, "ana", opts); err != nil {
		t.Fatal(err)
	}
	if err := l.JudgePending(ctx, opts); err != nil {
		t.Fatal(err)
	}
	before := excused(t, l, base, base.AddDate(0, 0, 14), time.UTC)
	// Back to 013: what 014 made, and what came after it.
	for _, q := range append(append(append(slices.Clone(undo017), undo016...), undo015...), "DROP TABLE grants", "ALTER TABLE windows DROP COLUMN team_id", "ALTER TABLE windows DROP COLUMN repeat_on", "PRAGMA user_version = 13") {
		if _, err := l.w.ExecContext(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	l.Close()
	l2, err := Open(ctx, path, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	var v int
	l2.w.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	ws, err := l2.Windows(ctx, base, base.AddDate(0, 0, 14))
	if err != nil || len(ws) != 1 || ws[0].TeamID != 0 || ws[0].On != "" {
		t.Fatalf("windows after migrating: %+v %v", ws, err)
	}
	if err := l2.JudgePending(ctx, opts); err != nil {
		t.Fatal(err)
	}
	after := excused(t, l2, base, base.AddDate(0, 0, 14), time.UTC)
	if want := schemaVersion(t); v != want {
		t.Errorf("user_version %d, want %d", v, want)
	}
	if len(before) != 14 || len(after) != 14 {
		t.Errorf("before %d after %d", len(before), len(after))
	}
	if _, err := l2.PutGrant(ctx, Grant{Who: "user:bo"}, "ana"); err != nil {
		t.Error(err)
	}
	if _, err := l2.w.ExecContext(ctx, "INSERT INTO grants (who, role, team_id, created) VALUES ('user:x', 'admin', 0, 0)"); err == nil {
		t.Error("CHECK let an admin grant in")
	}
}
