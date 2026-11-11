package ledger

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	"lapilli.dev/internal/verdict"
)

// undo015 takes back what 015 made: a ledger as 014 left it, comments as
// 003 made them, their ids as SQLite gives a table without AUTOINCREMENT.
var undo015 = []string{
	"DROP TABLE on_call_turns",
	`CREATE TABLE comments_014 (
		id INTEGER PRIMARY KEY,
		on_kind TEXT NOT NULL,
		on_key TEXT NOT NULL,
		body TEXT NOT NULL,
		created INTEGER NOT NULL,
		created_by TEXT NOT NULL DEFAULT '',
		updated INTEGER NOT NULL
	)`,
	"INSERT INTO comments_014 SELECT id, on_kind, on_key, body, created, created_by, updated FROM comments",
	"DROP TABLE comments",
	"ALTER TABLE comments_014 RENAME TO comments",
	"CREATE INDEX comments_on ON comments (on_kind, on_key, created)",
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
	// A comment's id is never another's again, the last one deleted's
	// included: a tracker job finds what it posted by the id.
	last, err := l2.PutComment(ctx, Comment{On: "task", Key: "1", Body: "Rasied it."}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	if err := l2.DeleteComment(ctx, last, "ana"); err != nil {
		t.Fatal(err)
	}
	if next, err := l2.PutComment(ctx, Comment{On: "task", Key: "1", Body: "Raised it."}, "ana"); err != nil || next == last {
		t.Errorf("after comment %d was deleted, the next is %d (%v)", last, next, err)
	}
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if changed, err := l2.PutTurns(ctx, "payments", at, []verdict.RotaTurn{{From: at, To: at.Add(time.Hour), Who: "Jane Doe"}}); err != nil || !changed {
		t.Errorf("keeping turns: %v %v", changed, err)
	}
}

// Names from a tracker and from an on-call feed follow the names rule of
// the ledger (Hidden): a joiner between two emoji, or between two letters
// of Persian, shows; a soft hyphen, or a mark that turns text around,
// doesn't: a turn naming one is refused, and a comment by one is kept as
// someone's.
func TestInputNamesAsHidden(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	task, err := l.PutTask(ctx, Task{Title: "Tune Errors"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour)
	for i, c := range []struct {
		name string
		ok   bool
	}{
		{"Ana \U0001F469\u200D\U0001F4BB", true},
		{"\u0645\u06CC\u200C\u062E\u0648\u0627\u0647\u0645", true},
		{"Ja\u00ADne", false},
		{"Jane\u202EDoe", false},
	} {
		if _, err := l.CopyComment(ctx, task, "x", c.name, fmt.Sprint("r", i), at); err != nil {
			t.Fatal(err)
		}
		cs, _ := l.Comments(ctx, "task", fmt.Sprint(task))
		if got := cs[len(cs)-1].Author; (got == c.name) != c.ok || !c.ok && got != "someone" {
			t.Errorf("an author %q kept as %q", c.name, got)
		}
		from := time.Date(2026, 11, 1+i, 0, 0, 0, 0, time.UTC)
		_, err = l.PutTurns(ctx, "s", from, []verdict.RotaTurn{{From: from, To: from.Add(time.Hour), Who: c.name}})
		if (err == nil) != c.ok {
			t.Errorf("a turn of %q: %v", c.name, err)
		}
	}
}
