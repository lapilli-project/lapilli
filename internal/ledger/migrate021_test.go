package ledger

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger/ledgertest"
)

// undo021 takes back what 021 made, for the tests of the migrations
// before it.
var undo021 = []string{
	"DROP TABLE marked_weeks",
	"DROP TABLE marked_days",
	"DROP TABLE marked",
	"DROP TABLE marked_kinds",
	"ALTER TABLE firings DROP COLUMN marked",
}

// A ledger at 020, with firings and their tallies, migrates to 021: its
// firings marked with nothing, its tallies as they were, no kind kept;
// then a marked window marks them.
func TestMigrate021(t *testing.T) {
	ledgertest.SQLiteOnly(t, "a ledger an earlier lapilli wrote")
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")
	l, err := Open(ctx, path, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	at := t0.Add(10 * time.Hour)
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: at, End: at.Add(time.Minute)}})
	for _, q := range append(slices.Concat(undo024, undo023, undo022, undo021), "PRAGMA user_version = 20") {
		if _, err := l.w.ExecContext(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	l.Close()
	l2, err := Open(ctx, path, time.UTC)
	if err != nil {
		t.Fatalf("migrating to 021: %v", err)
	}
	defer l2.Close()
	var v, marked int
	l2.w.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	if want := schemaVersion(t); v != want || want < 21 {
		t.Errorf("migrated to %d, want %d", v, want)
	}
	l2.w.QueryRowContext(ctx, "SELECT count(*) FROM firings WHERE marked != 0").Scan(&marked)
	days, err := l2.Days(ctx, l2.Day(t0), l2.Day(t0))
	if err != nil || len(days) != 1 || days[0].Firings != 1 || marked != 0 {
		t.Fatalf("after migrating: %+v %v, %d marked", days, err, marked)
	}
	if ks, err := l2.MarkedKinds(ctx); err != nil || len(ks) != 0 {
		t.Errorf("kinds after migrating: %v %v", ks, err)
	}
	if _, err := l2.PutMarked(ctx, Marked{Kind: "sale", Title: "Autumn sale", Start: t0, End: t0.Add(24 * time.Hour)}, "ana", opts); err != nil {
		t.Fatal(err)
	}
	fs, err := l2.Firings(ctx, FiringQuery{From: t0, To: t0.Add(24 * time.Hour)})
	if err != nil || len(fs) != 1 || !slices.Equal(fs[0].Kinds, []string{"sale"}) {
		t.Errorf("firings after a window was kept: %+v %v", fs, err)
	}
}
