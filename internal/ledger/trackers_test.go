package ledger

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// backdate moves a task's last change, and the changes noted for it, to
// times given in order, the earliest first, as if made then.
func backdate(t *testing.T, l *Ledger, id int64, at ...time.Time) {
	t.Helper()
	ctx := context.Background()
	rows, err := l.r.QueryContext(ctx, "SELECT id FROM changes WHERE what IN (?, ?) ORDER BY id", fmt.Sprintf("task %d made", id), fmt.Sprintf("task %d changed", id))
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for rows.Next() {
		var x int64
		rows.Scan(&x)
		ids = append(ids, x)
	}
	rows.Close()
	if len(ids) != len(at) {
		t.Fatalf("task %d has %d changes, %d times given", id, len(ids), len(at))
	}
	for i, x := range ids {
		if _, err := l.w.ExecContext(ctx, "UPDATE changes SET at = ? WHERE id = ?", at[i].Unix(), x); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.w.ExecContext(ctx, "UPDATE tasks SET updated = ? WHERE id = ?", at[len(at)-1].Unix(), id); err != nil {
		t.Fatal(err)
	}
}

// A tracker job is told who set each task's status and when: the change
// that moved it there, not the latest edit of anything else, and not a
// change to another task that had its id before.
func TestTrackerStatusSet(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	t1 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	tm := func(h int) time.Time { return t1.Add(time.Duration(h) * time.Hour) }
	str := func(s string) *string { return &s }
	put := func(task Task, by string) int64 {
		t.Helper()
		id, err := l.PutTask(ctx, task, by)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	patch := func(id int64, p, by string) {
		t.Helper()
		if _, err := l.PatchTask(ctx, id, []byte(p), by); err != nil {
			t.Fatal(err)
		}
	}
	// 1: the job closed it as its issue was; a person then reassigned it,
	// and the job wrote nothing about its status since.
	a := put(Task{Title: "Closed by the job", Link: "https://tracker.example.com/issues/1"}, "ana")
	if _, err := l.TrackTask(ctx, a, nil, str("done"), TaskTracker); err != nil {
		t.Fatal(err)
	}
	patch(a, `{"assignee": "cy"}`, "bo")
	patch(a, `{"status": "done"}`, "bo") // the same status again
	backdate(t, l, a, tm(0), tm(1), tm(2), tm(3))
	// 2: a person closed it; the job then linked it.
	b := put(Task{Title: "Closed by a person"}, "ana")
	patch(b, `{"status": "dropped"}`, "bo")
	if _, err := l.TrackTask(ctx, b, str("https://tracker.example.com/issues/2"), nil, TaskTracker); err != nil {
		t.Fatal(err)
	}
	backdate(t, l, b, tm(0), tm(4), tm(5))
	// 3: open since it was made.
	c := put(Task{Title: "Open since made", Link: "https://tracker.example.com/issues/3"}, "ana")
	patch(c, `{"title": "Open, retitled"}`, "bo")
	backdate(t, l, c, tm(6), tm(7))
	// 4: had the id of a task deleted before it was made.
	d := put(Task{Title: "Its id taken again", Link: "https://tracker.example.com/issues/4"}, "ana")
	patch(d, `{"status": "doing"}`, "bo")
	if err := l.DeleteTask(ctx, d, "bo"); err != nil {
		t.Fatal(err)
	}
	if x := put(Task{Title: "Its id taken again", Status: "doing", Link: "https://tracker.example.com/issues/4"}, "cy"); x != d {
		t.Skipf("the id wasn't taken again: %d, was %d", x, d)
	}
	backdate(t, l, d, tm(8), tm(9), tm(10))
	// 5: closed by the job, reopened by a person, closed again by another.
	e := put(Task{Title: "Closed twice", Link: "https://tracker.example.com/issues/5"}, "ana")
	if _, err := l.TrackTask(ctx, e, nil, str("done"), TaskTracker); err != nil {
		t.Fatal(err)
	}
	patch(e, `{"status": "open"}`, "bo")
	patch(e, `{"status": "done"}`, "cy")
	backdate(t, l, e, tm(11), tm(12), tm(13), tm(14))

	ts, err := l.TrackerTasks(ctx, TrackerQuery{Linked: true})
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]string{}
	for _, x := range ts {
		got[x.ID] = fmt.Sprintf("%s %s %s", x.Status, x.StatusBy, x.StatusAt.Format("15:04"))
	}
	for id, want := range map[int64]string{a: "done tasks-token 10:00", b: "dropped bo 13:00", c: "open ana 15:00", d: "doing cy 19:00", e: "done cy 23:00"} {
		if got[id] != want {
			t.Errorf("task %d: %q, want %q", id, got[id], want)
		}
	}

	// A task whose changes were pruned: neither who nor when.
	if _, err := l.w.ExecContext(ctx, "DELETE FROM changes WHERE what LIKE ?", fmt.Sprintf("task %d %%", c)); err != nil {
		t.Fatal(err)
	}
	ts, _ = l.TrackerTasks(ctx, TrackerQuery{Linked: true, Status: []string{"open"}})
	if len(ts) != 1 || ts[0].ID != c || ts[0].StatusBy != "" || !ts[0].StatusAt.IsZero() {
		t.Errorf("pruned: %+v", ts)
	}
}

// updated_after leaves out the tasks not changed since, so a job checks
// tasks closed lately however many were closed before.
func TestTrackerUpdatedAfter(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	old := time.Now().Add(-90 * 24 * time.Hour)
	var ids []int64
	for i, status := range []string{"done", "dropped", "done", "open"} {
		id, err := l.PutTask(ctx, Task{Title: fmt.Sprintf("Task %d", i), Status: status, Link: "https://tracker.example.com/issues/1"}, "ana")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	backdate(t, l, ids[0], old)
	backdate(t, l, ids[1], old)
	backdate(t, l, ids[3], old)
	for _, c := range []struct {
		q    TrackerQuery
		want []int64
	}{
		{TrackerQuery{Linked: true, Status: []string{"done", "dropped"}}, ids[:3]},
		{TrackerQuery{Linked: true, Status: []string{"done", "dropped"}, UpdatedAfter: time.Now().Add(-30 * 24 * time.Hour)}, ids[2:3]},
		{TrackerQuery{Linked: true, Status: []string{"done", "dropped"}, UpdatedAfter: old}, ids[2:3]},
		{TrackerQuery{Linked: true, Status: []string{"done", "dropped"}, UpdatedAfter: old.Add(-time.Second)}, ids[:3]},
		{TrackerQuery{Linked: true, Status: []string{"open"}, UpdatedAfter: time.Now().Add(-30 * 24 * time.Hour)}, nil},
	} {
		ts, err := l.TrackerTasks(ctx, c.q)
		if err != nil {
			t.Fatal(err)
		}
		var got []int64
		for _, x := range ts {
			got = append(got, x.ID)
		}
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%+v: %v, want %v", c.q, got, c.want)
		}
	}
}
