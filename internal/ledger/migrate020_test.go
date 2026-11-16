package ledger

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"lapilli.dev/internal/ledger/ledgertest"
)

// undo020 takes back what 020 made, for the tests of the migrations
// before it.
var undo020 = []string{
	"DROP INDEX tasks_goal",
	"ALTER TABLE tasks DROP COLUMN goal",
	"DROP TABLE goals",
}

// A ledger at 019 with tasks migrates to 020: no goal is declared, every
// task is kept as it was, of no goal, and one can be made of a goal.
func TestMigrate020(t *testing.T) {
	ledgertest.SQLiteOnly(t, "a ledger an earlier lapilli wrote")
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")
	l, err := Open(ctx, path, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	id, err := l.PutTask(ctx, Task{Title: "tune it", Kind: "tune", Rule: "Errors"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range append(undo020, "PRAGMA user_version = 19") {
		if _, err := l.w.ExecContext(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	l.Close()
	l2, err := Open(ctx, path, time.UTC)
	if err != nil {
		t.Fatalf("migrating to 020: %v", err)
	}
	defer l2.Close()
	var v int
	l2.w.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	if want := schemaVersion(t); v != want || want < 20 {
		t.Errorf("migrated to %d, want %d", v, want)
	}
	gs, err := l2.Goals(ctx)
	if err != nil || len(gs) != 0 {
		t.Errorf("goals after migrating: %+v %v", gs, err)
	}
	x, err := l2.TaskByID(ctx, id)
	if err != nil || x.Title != "tune it" || x.Goal != "" {
		t.Errorf("the task after migrating: %+v %v", x, err)
	}
	if _, err := l2.MakeGoalTask(ctx, Task{Title: "Goal none-missed missed", Kind: "decide", Goal: "none-missed"}, 20400, "ana"); err != nil {
		t.Error(err)
	}
}
