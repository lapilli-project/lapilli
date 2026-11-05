package ledger

import (
	"context"
	"errors"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
)

// A quiet window excuses the firings in it that caught nothing; those that
// caught something stay white; taken away, the window excuses nothing.
func TestQuietWindow(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	svc := func(s string) map[string]string { return map[string]string{"service": s} }
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{
		{Rule: "Errors", Labels: svc("checkout"), Start: t0.Add(2 * time.Hour), End: t0.Add(3 * time.Hour)},
		{Rule: "Errors", Labels: svc("checkout"), Start: t0.Add(4 * time.Hour), End: t0.Add(5 * time.Hour)},
		{Rule: "Errors", Labels: svc("search"), Start: t0.Add(4 * time.Hour), End: t0.Add(5 * time.Hour)},
		{Rule: "Disk", Labels: svc("checkout"), Start: t0.Add(4 * time.Hour), End: t0.Add(5 * time.Hour)},
	})
	if _, err := l.SyncFile(ctx, []incident.Incident{{ID: "INC-1", Start: t0.Add(4*time.Hour + 5*time.Minute), Labels: incident.Labels{"service": {"search"}}}}, opts); err != nil {
		t.Fatal(err)
	}
	week := func() Tally {
		ws, err := l.Weeks(ctx, l.Day(t0), l.Day(t0))
		if err != nil {
			t.Fatal(err)
		}
		var sum Tally
		for _, w := range ws {
			sum.Firings += w.Firings
			sum.White += w.White
			sum.Excused += w.Excused
		}
		return sum
	}
	if w := week(); w.Firings != 4 || w.White != 1 || w.Excused != 0 {
		t.Fatalf("before: %+v", w)
	}
	// A deploy of Errors, 3:30 to 4:30: the checkout firing at 4:00 is
	// excused; search's caught INC-1 and stays; Disk's isn't Errors.
	id, err := l.PutWindow(ctx, Window{Title: "deploy", Start: t0.Add(210 * time.Minute), End: t0.Add(270 * time.Minute), Rules: "^Errors$"}, "ana", opts)
	if err != nil {
		t.Fatal(err)
	}
	if w := week(); w.Firings != 3 || w.White != 1 || w.Excused != 1 {
		t.Errorf("with the window: %+v", w)
	}
	rep, err := l.Report(ctx, t0, t0.Add(24*time.Hour), opts, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rep.Rules {
		if r.Rule == "Errors" && (r.Firings != 2 || r.Excused != 1 || r.Black != 1) {
			t.Errorf("report: %+v", r)
		}
	}
	if len(rep.Firings) != 3 {
		t.Errorf("an excused firing isn't in the report's firings: %d", len(rep.Firings))
	}
	// Only checkout's, by label.
	w, _ := l.WindowByID(ctx, id)
	w.Rules, w.Labels = "", incident.Labels{"service": {"checkout"}}
	if _, err := l.PutWindow(ctx, w, "ana", opts); err != nil {
		t.Fatal(err)
	}
	if w := week(); w.Excused != 2 {
		t.Errorf("checkout's two at 4:00 excused, by label: %+v", w)
	}
	if err := l.DeleteWindow(ctx, id, "ana", opts); err != nil {
		t.Fatal(err)
	}
	if w := week(); w.Firings != 4 || w.Excused != 0 {
		t.Errorf("after the window went: %+v", w)
	}
	// A window made before its firings excuses them as they are recorded.
	if _, err := l.PutWindow(ctx, Window{Title: "load test", Start: t0.Add(30 * time.Hour), End: t0.Add(31 * time.Hour)}, "ana", opts); err != nil {
		t.Fatal(err)
	}
	record(t, l, t0.Add(24*time.Hour), t0.Add(48*time.Hour), []firing.Firing{{Rule: "Errors", Labels: svc("cart"), Start: t0.Add(30*time.Hour + time.Minute)}})
	days, _ := l.Days(ctx, l.Day(t0)+1, l.Day(t0)+1)
	if len(days) != 1 || days[0].Excused != 1 || days[0].Firings != 0 {
		t.Errorf("recorded into a window: %+v", days)
	}
	for _, bad := range []Window{{Title: "x"}, {Title: "x", Start: t0, End: t0.Add(40 * 24 * time.Hour)}, {Title: "x", Start: t0, End: t0.Add(time.Hour), Rules: "("}} {
		if _, err := l.PutWindow(ctx, bad, "ana", opts); !errors.As(err, new(ErrInvalid)) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}

// Spans to judge again outlive a stop halfway.
func TestPending(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{{Rule: "Errors", Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)}})
	err := l.write(ctx, func(tx *txn) error {
		if _, err := tx.ExecContext(tx.ctx, "INSERT INTO windows (title, start_at, end_at, updated) VALUES ('x', ?, ?, 0)", t0.Unix(), t0.Add(3*time.Hour).Unix()); err != nil {
			return err
		}
		return pend(tx, [][2]int64{{t0.Unix(), t0.Add(3 * time.Hour).Unix()}})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.JudgePending(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if days, _ := l.Days(ctx, l.Day(t0), l.Day(t0)); len(days) != 1 || days[0].Excused != 1 {
		t.Errorf("days %+v", days)
	}
}

func TestTeamsNotesTasksComments(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	id, err := l.PutTeam(ctx, Team{Name: "payments", Labels: incident.Labels{"service": {"checkout", "cart"}}}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutTeam(ctx, Team{Name: "payments", Rules: "x"}, "ana"); err == nil {
		t.Error("two teams of one name")
	}
	if _, err := l.PutTeam(ctx, Team{Name: "empty"}, "ana"); err == nil {
		t.Error("a team that owns nothing")
	}
	if err := l.PutNote(ctx, Note{Rule: "Errors", TeamID: id, Status: "tune", Runbook: "https://runbooks.example/errors"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if err := l.PutNote(ctx, Note{Rule: "Errors", Runbook: "javascript:alert(1)"}, "ana"); err == nil {
		t.Error("a runbook that isn't http")
	}
	notes, _ := l.Notes(ctx)
	if notes["Errors"].TeamID != id || notes["Errors"].Status != "tune" {
		t.Errorf("notes %+v", notes)
	}
	tid, err := l.PutTask(ctx, Task{Title: "Raise Errors' for to 15m", Kind: "tune", Rule: "Errors", TeamID: id, Due: t0}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	task, _ := l.TaskByID(ctx, tid)
	task.Status = "done"
	if _, err := l.PutTask(ctx, task, "bo"); err != nil {
		t.Fatal(err)
	}
	if open, _ := l.Tasks(ctx, TaskQuery{Status: []string{"open", "doing"}}); len(open) != 0 {
		t.Errorf("open tasks %+v", open)
	}
	if done, _ := l.Tasks(ctx, TaskQuery{Rule: "Errors"}); len(done) != 1 || done[0].By != "bo" || !done[0].Due.Equal(t0) {
		t.Errorf("tasks %+v", done)
	}
	// Deleting the team leaves the note and the task no one's.
	if err := l.DeleteTeam(ctx, id, "ana"); err != nil {
		t.Fatal(err)
	}
	if notes, _ := l.Notes(ctx); notes["Errors"].TeamID != 0 {
		t.Errorf("the note still names a team gone: %+v", notes["Errors"])
	}
	// A comment is on something there: a rule a run has seen.
	if _, err := l.PutComment(ctx, Comment{On: "rule", Key: "Errors", Body: "fires on every deploy"}, "ana"); err == nil {
		t.Error("a comment on a rule no run has seen")
	}
	record(t, l, t0, t0.Add(time.Hour), []firing.Firing{{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: t0, End: t0.Add(time.Minute)}})
	cid, err := l.PutComment(ctx, Comment{On: "rule", Key: "Errors", Body: "fires on every deploy"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutComment(ctx, Comment{ID: cid, Body: "no"}, "bo"); !errors.Is(err, ErrNotAuthor) {
		t.Errorf("someone else changed a comment: %v", err)
	}
	if err := l.DeleteComment(ctx, cid, "ana"); err != nil {
		t.Fatal(err)
	}
	if cs, _ := l.Comments(ctx, "rule", "Errors"); len(cs) != 0 {
		t.Errorf("comments %+v", cs)
	}
	if err := l.DeleteTask(ctx, tid, "ana"); err != nil {
		t.Fatal(err)
	}
	if ch, _ := l.Changes(ctx, 20); len(ch) < 8 {
		t.Errorf("every write noted: %d changes", len(ch))
	}
}
