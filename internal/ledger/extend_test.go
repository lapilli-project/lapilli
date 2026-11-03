package ledger

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
)

// A window every day at 02:00 for an hour, in a zone with daylight saving:
// it stays at 02:00 on the clock, and stops after its last day.
func TestRepeatingWindow(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	ctx := context.Background()
	l := open(t, ny)
	// Daylight saving ends on 2026-11-01 in New York.
	day := func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, ny) }
	var fs []firing.Firing
	for d := 28; d <= 31; d++ {
		fs = append(fs, firing.Firing{Rule: "Backup", Start: day(d, 2, 30), End: day(d, 2, 40)}, // in it
			firing.Firing{Rule: "Backup", Start: day(d, 4, 0), End: day(d, 4, 10)}) // not
	}
	fs = append(fs, firing.Firing{Rule: "Backup", Start: time.Date(2026, 11, 2, 2, 30, 0, 0, ny), End: time.Date(2026, 11, 2, 2, 40, 0, 0, ny)}, // after DST: still 02:30
		firing.Firing{Rule: "Backup", Start: time.Date(2026, 11, 4, 2, 30, 0, 0, ny), End: time.Date(2026, 11, 4, 2, 40, 0, 0, ny)}) // after its last day
	record(t, l, day(27, 0, 0), time.Date(2026, 11, 5, 0, 0, 0, 0, ny), fs)
	_, err = l.PutWindow(ctx, Window{Title: "backups", Start: day(28, 2, 0), End: day(28, 3, 0), Every: "day",
		Until: time.Date(2026, 11, 3, 0, 0, 0, 0, ny)}, "ana", opts)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := l.Firings(ctx, FiringQuery{From: day(27, 0, 0), To: time.Date(2026, 11, 5, 0, 0, 0, 0, ny), Excused: true})
	if len(got) != 5 {
		t.Errorf("%d excused, want the five at 02:30 to 2026-11-02", len(got))
	}
	for _, f := range got {
		if h := f.Start.In(ny).Hour(); h != 2 {
			t.Errorf("excused at %s", f.Start.In(ny))
		}
	}
	for _, bad := range []Window{
		{Title: "x", Start: day(28, 0, 0), End: day(29, 0, 0), Every: "day"},
		{Title: "x", Start: day(28, 0, 0), End: day(28, 1, 0), Every: "month"},
		{Title: "x", Start: day(28, 0, 0), End: day(28, 1, 0), Every: "week", Until: day(20, 0, 0)},
	} {
		if _, err := l.PutWindow(ctx, bad, "ana", opts); err == nil {
			t.Errorf("%+v kept", bad)
		}
	}
}

// What happens is written to the outbox only when it is to be told, once
// for each place; a notice that fails is tried later, and given up on.
func TestOutbox(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if _, err := l.PutTask(ctx, Task{Title: "untold"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if due, _ := l.Due(ctx, time.Now(), 10); len(due) != 0 {
		t.Fatalf("told with nowhere to tell: %+v", due)
	}
	l.TellTo([]string{"webhook", "slack"})
	id, err := l.PutTask(ctx, Task{Title: "Tune Errors", Kind: "tune", Rule: "Errors", Link: "https://tracker.example/LAP-1"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	task, _ := l.TaskByID(ctx, id)
	task.Assignee, task.Status = "bo", "doing"
	if _, err := l.PutTask(ctx, task, "ana"); err != nil {
		t.Fatal(err)
	}
	due, err := l.Due(ctx, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	for _, n := range due {
		events = append(events, n.Dest+" "+n.Event)
	}
	want := []string{"webhook task.created", "slack task.created", "webhook task.assigned", "slack task.assigned", "webhook task.status", "slack task.status"}
	if len(events) != len(want) {
		t.Fatalf("events %q, want %q", events, want)
	}
	var body struct {
		Task Task   `json:"task"`
		By   string `json:"by"`
	}
	if err := json.Unmarshal(due[0].Body, &body); err != nil || body.Task.Link != "https://tracker.example/LAP-1" || body.By != "ana" {
		t.Errorf("body %s %v", due[0].Body, err)
	}
	if err := l.Sent(ctx, due[0].ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < GiveUpAfter; i++ {
		if err := l.Failed(ctx, due[1].ID, "HTTP 500"); err != nil {
			t.Fatal(err)
		}
	}
	later, _ := l.Due(ctx, time.Now().Add(24*time.Hour), 10)
	for _, n := range later {
		if n.ID == due[0].ID || n.ID == due[1].ID {
			t.Errorf("notice %d is due again", n.ID)
		}
	}
	if n, _ := l.Unsent(ctx); n != len(due)-1 {
		t.Errorf("unsent %d, want %d", n, len(due)-1)
	}
	if _, err := l.PutTask(ctx, Task{Title: "x", Link: "javascript:alert(1)"}, "ana"); err == nil {
		t.Error("a link that isn't a web page")
	}
}

// An overdue task is told of once for its due day; moved, once more.
func TestOverdue(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	l.TellTo([]string{"webhook"})
	id, _ := l.PutTask(ctx, Task{Title: "late", Due: t0}, "ana")
	l.PutTask(ctx, Task{Title: "done already", Due: t0, Status: "done"}, "ana")
	for range 2 {
		if n, err := l.Overdue(ctx, t0.Add(48*time.Hour)); err != nil || n > 1 {
			t.Fatalf("overdue %d %v", n, err)
		}
	}
	task, _ := l.TaskByID(ctx, id)
	task.Due = t0.Add(24 * time.Hour)
	l.PutTask(ctx, task, "ana")
	if n, _ := l.Overdue(ctx, t0.Add(48*time.Hour)); n != 1 {
		t.Errorf("moved and late again: %d", n)
	}
	due, _ := l.Due(ctx, time.Now(), 50)
	late := 0
	for _, n := range due {
		if n.Event == "task.overdue" {
			late++
		}
	}
	if late != 2 {
		t.Errorf("%d overdue notices, want 2", late)
	}
}
