package ledger

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	"lapilli.dev/internal/verdict"
)

// undo015 takes back what 015 made: a ledger as 014 left it.
var undo015 = []string{
	"DROP TABLE on_call_turns",
	"DROP INDEX comments_ref",
	"ALTER TABLE comments DROP COLUMN origin",
	"ALTER TABLE comments DROP COLUMN ref",
	"ALTER TABLE comments DROP COLUMN author",
}

// schemaVersion is the version a ledger this build opens is at: one for
// each file of schema/, applied in their order.
func schemaVersion(t *testing.T) int {
	t.Helper()
	files, err := fs.Glob(schema, "schema/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	return len(files)
}

// A new ledger is at 015, after 014 (the files are applied by their place
// in the order, so 015 must come after 014); a ledger 014 left, with a
// task and a comment on it, migrates to 015: its comment kept, as people's,
// on-call turns and comments copied from a tracker taken.
func TestMigrate015(t *testing.T) {
	ctx := context.Background()
	files, _ := fs.Glob(schema, "schema/*.sql")
	if len(files) != 15 || files[13] != "schema/014.sql" || files[14] != "schema/015.sql" {
		t.Fatalf("schema files %v", files)
	}
	path := filepath.Join(t.TempDir(), "l.db")
	l, err := Open(ctx, path, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	var v int
	l.w.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	if v != 15 {
		t.Fatalf("a new ledger at %d", v)
	}
	task, err := l.PutTask(ctx, Task{Title: "Tune Errors"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutComment(ctx, Comment{On: "task", Key: "1", Body: "Raised it."}, "ana"); err != nil {
		t.Fatal(err)
	}
	for _, q := range append(undo015, "PRAGMA user_version = 14") {
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
	l2.w.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	if v != 15 {
		t.Errorf("migrated to %d", v)
	}
	cs, err := l2.Comments(ctx, "task", "1")
	if err != nil || len(cs) != 1 || cs[0].Body != "Raised it." || cs[0].Origin != "" || cs[0].Ref != "" || cs[0].By != "ana" {
		t.Fatalf("the comment after migrating: %+v %v", cs, err)
	}
	if kept, err := l2.CopyComment(ctx, task, "From the tracker.", "someone", "c-1", time.Now().Add(-time.Hour)); err != nil || !kept {
		t.Errorf("copying a comment in: %v %v", kept, err)
	}
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if changed, err := l2.PutTurns(ctx, "payments", at, []verdict.RotaTurn{{From: at, To: at.Add(time.Hour), Who: "Jane Doe"}}); err != nil || !changed {
		t.Errorf("keeping turns: %v %v", changed, err)
	}
}
