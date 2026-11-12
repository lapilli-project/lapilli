package ledger

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// undo016 takes back what 016 made: tasks as 003 and 004 made them, their
// ids as SQLite gives a table without AUTOINCREMENT, and no next id kept.
var undo016 = []string{
	`CREATE TABLE tasks_015 (
		id INTEGER PRIMARY KEY,
		title TEXT NOT NULL,
		kind TEXT NOT NULL DEFAULT '',
		rule TEXT NOT NULL DEFAULT '',
		incident TEXT NOT NULL DEFAULT '',
		team_id INTEGER REFERENCES teams (id) ON DELETE SET NULL,
		assignee TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'open',
		due INTEGER,
		body TEXT NOT NULL DEFAULT '',
		created INTEGER NOT NULL,
		created_by TEXT NOT NULL DEFAULT '',
		updated INTEGER NOT NULL,
		updated_by TEXT NOT NULL DEFAULT '',
		link TEXT NOT NULL DEFAULT '',
		overdue_told INTEGER,
		done_at INTEGER
	)`,
	"INSERT INTO tasks_015 SELECT * FROM tasks",
	"DROP TABLE tasks",
	"ALTER TABLE tasks_015 RENAME TO tasks",
	"CREATE INDEX tasks_open ON tasks (status, updated)",
	"CREATE INDEX tasks_rule ON tasks (rule)",
	"CREATE INDEX tasks_incident ON tasks (incident)",
	"DELETE FROM sqlite_sequence WHERE name = 'tasks'",
}

// A task's id is never another's again, the newest deleted's included: the
// tracker jobs find a task's issue by its id (lapilli-task-ID), and a new
// task, maybe another team's, given a deleted one's id would take over its
// issue, its comments both ways and its close. A proposal made into the
// deleted task is made into none, not into the next.
func TestTaskIDNeverAgain(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	old, err := l.PutTask(ctx, Task{Title: "Tune Errors"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := l.Propose(ctx, proposal("HighErrors", KindFor))
	if err != nil {
		t.Fatal(err)
	}
	if err := l.SetProposalTask(ctx, pid, old); err != nil {
		t.Fatal(err)
	}
	if err := l.DeleteTask(ctx, old, "ana"); err != nil {
		t.Fatal(err)
	}
	if p, err := l.ProposalByID(ctx, pid); err != nil || p.TaskID != 0 {
		t.Errorf("proposal %d still made into the deleted task: %d (%v)", pid, p.TaskID, err)
	}
	next, err := l.PutTask(ctx, Task{Title: "Unrelated"}, "bo")
	if err != nil {
		t.Fatal(err)
	}
	if next == old {
		t.Errorf("a new task was given the deleted task %d's id", old)
	}
	if p, err := l.ProposalOfTask(ctx, next); !errors.Is(err, ErrNotFound) {
		t.Errorf("the new task %d is taken as made from proposal #%d (%v)", next, p.ID, err)
	}
}

// A ledger 015 left (v0.27.0 and v0.28.0), its newest task deleted and a
// proposal still made into it, migrates to 016: its tasks and their ids
// kept; the next id past the deleted one's, which the changes noted; the
// proposal made into none.
func TestMigrate016(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "l.db")
	l, err := Open(ctx, path, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if v := schemaVersion(t); v < 16 {
		t.Fatalf("schema files: %d", v)
	}
	var ids []int64
	for _, title := range []string{"Tune Errors", "Raise Latency", "Drop Spare"} {
		id, err := l.PutTask(ctx, Task{Title: title, Assignee: "ana"}, "ana")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	pid, err := l.Propose(ctx, proposal("HighErrors", KindFor))
	if err != nil {
		t.Fatal(err)
	}
	if err := l.SetProposalTask(ctx, pid, ids[2]); err != nil {
		t.Fatal(err)
	}
	for _, q := range append(append(slices.Clone(undo017), undo016...), "PRAGMA user_version = 15") {
		if _, err := l.w.ExecContext(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	// As 015's DeleteTask deleted: the task, not the proposal's link.
	if _, err := l.w.ExecContext(ctx, "DELETE FROM tasks WHERE id = ?", ids[2]); err != nil {
		t.Fatal(err)
	}
	if _, err := l.w.ExecContext(ctx, "INSERT INTO changes (at, by, what) VALUES (1, 'ana', ?)", "task 3 deleted"); err != nil {
		t.Fatal(err)
	}
	l.Close()

	l2, err := Open(ctx, path, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	var v int
	l2.w.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	if want := schemaVersion(t); v != want {
		t.Errorf("migrated to %d, want %d", v, want)
	}
	ts, err := l2.Tasks(ctx, TaskQuery{})
	if err != nil || len(ts) != 2 || ts[0].ID+ts[1].ID != ids[0]+ids[1] || ts[0].Assignee != "ana" {
		t.Fatalf("tasks after migrating: %+v %v", ts, err)
	}
	if p, err := l2.ProposalByID(ctx, pid); err != nil || p.TaskID != 0 {
		t.Errorf("proposal %d made into the deleted task %d after migrating: %d (%v)", pid, ids[2], p.TaskID, err)
	}
	next, err := l2.PutTask(ctx, Task{Title: "Unrelated"}, "bo")
	if err != nil || next <= ids[2] {
		t.Errorf("after migrating, a new task is %d, the deleted one's %d (%v)", next, ids[2], err)
	}
	// And from then on, each new one past the last.
	if err := l2.DeleteTask(ctx, next, "bo"); err != nil {
		t.Fatal(err)
	}
	if again, err := l2.PutTask(ctx, Task{Title: "Unrelated"}, "bo"); err != nil || again <= next {
		t.Errorf("after task %d was deleted, the next is %d (%v)", next, again, err)
	}
}
