package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
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
// for each place, as it was kept; a notice that fails is tried later, and
// given up on.
func TestOutbox(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if _, err := l.PutTask(ctx, Task{Title: "untold"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if due, _ := l.Due(ctx, "webhook", time.Now(), 10); len(due) != 0 {
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
	// Made with an assignee: made, and assigned.
	if _, err := l.PutTask(ctx, Task{Title: "Write a runbook", Assignee: "cy"}, "ana"); err != nil {
		t.Fatal(err)
	}
	events := func(dest string) ([]Notice, []string) {
		due, err := l.Due(ctx, dest, time.Now(), 10)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, n := range due {
			out = append(out, n.Event)
		}
		return due, out
	}
	due, got := events("webhook")
	want := []string{"task.created", "task.assigned", "task.status", "task.created", "task.assigned"}
	if !slices.Equal(got, want) {
		t.Fatalf("events %q, want %q", got, want)
	}
	if _, slack := events("slack"); !slices.Equal(slack, want) {
		t.Errorf("slack's events %q, want %q", slack, want)
	}
	var body struct {
		Task Task   `json:"task"`
		By   string `json:"by"`
	}
	if err := json.Unmarshal(due[0].Body, &body); err != nil || body.Task.Link != "https://tracker.example/LAP-1" || body.By != "ana" ||
		body.Task.Created.IsZero() || body.Task.CreatedBy != "ana" {
		t.Errorf("the task as kept: %s %v", due[0].Body, err)
	}
	if err := l.Sent(ctx, due[0].ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < GiveUpAfter; i++ {
		if err := l.Failed(ctx, due[1].ID, "HTTP 500"); err != nil {
			t.Fatal(err)
		}
	}
	later, _ := l.Due(ctx, "webhook", time.Now().Add(24*time.Hour), 10)
	for _, n := range later {
		if n.ID == due[0].ID || n.ID == due[1].ID {
			t.Errorf("notice %d is due again", n.ID)
		}
	}
	if n, _ := l.Unsent(ctx); n != 2*len(want)-1 {
		t.Errorf("unsent %d, want %d", n, 2*len(want)-1)
	}
	if _, err := l.PutTask(ctx, Task{Title: "x", Link: "javascript:alert(1)"}, "ana"); err == nil {
		t.Error("a link that isn't a web page")
	}

	// A patch changes only what it has: the link a tracker puts back
	// leaves the assignee someone gave meanwhile.
	if _, err := l.PatchTask(ctx, id, []byte(`{"link": "https://tracker.example/LAP-2"}`), "tracker"); err != nil {
		t.Fatal(err)
	}
	task, _ = l.TaskByID(ctx, id)
	if task.Link != "https://tracker.example/LAP-2" || task.Assignee != "bo" || task.Status != "doing" || task.Title != "Tune Errors" {
		t.Errorf("patched: %+v", task)
	}
	if _, err := l.PatchTask(ctx, id, []byte(`{"status": "nonsense"}`), "tracker"); err == nil {
		t.Error("a patch is checked as a whole task is")
	}
	if _, err := l.PatchTask(ctx, 999, []byte(`{}`), "tracker"); !errors.Is(err, ErrNotFound) {
		t.Errorf("no such task: %v", err)
	}

	// Done keeps when it was done, while it stays done.
	l.PatchTask(ctx, id, []byte(`{"status": "done"}`), "bo")
	task, _ = l.TaskByID(ctx, id)
	doneAt := task.Done
	l.PatchTask(ctx, id, []byte(`{"body": "tuned to 10m"}`), "bo")
	if task, _ = l.TaskByID(ctx, id); doneAt.IsZero() || !task.Done.Equal(doneAt) {
		t.Errorf("done at %v, then %v", doneAt, task.Done)
	}
	l.PatchTask(ctx, id, []byte(`{"status": "open"}`), "bo")
	if task, _ = l.TaskByID(ctx, id); !task.Done.IsZero() {
		t.Errorf("open again, but done at %v", task.Done)
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
	due, _ := l.Due(ctx, "webhook", time.Now(), 50)
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

// What a job copies isn't told, nor is writing it again a change; a person's
// writing is.
func TestCopierIsNotTold(t *testing.T) {
	l := open(t, time.UTC)
	ctx := context.Background()
	l.TellTo([]string{"webhook"})
	o := verdict.Options{Lead: time.Minute, UnknownLen: time.Hour}
	at := time.Date(2026, 11, 2, 1, 2, 3, 400, time.UTC)
	in := incident.Incident{ID: "PD-1", Title: "x", Start: at}
	for range 2 {
		if _, err := l.Put(ctx, in, Copier, o); err != nil {
			t.Fatal(err)
		}
	}
	due, err := l.Due(ctx, "webhook", time.Now(), 10)
	if err != nil || len(due) != 0 {
		t.Errorf("told of what a job copied: %v %v", due, err)
	}
	cs, err := l.ChangesSince(ctx, time.Unix(0, 0), 10)
	if err != nil || len(cs) != 1 {
		t.Errorf("written twice, as it was, even under the second: %v %v", cs, err)
	}
	if _, err := l.Put(ctx, incident.Incident{ID: "A-1", Start: at}, "ana", o); err != nil {
		t.Fatal(err)
	}
	if due, _ := l.Due(ctx, "webhook", time.Now(), 10); len(due) != 1 {
		t.Errorf("a person's: %v", due)
	}
}

// What is copied in is the tool's and what people write is theirs; labels
// and times are bounded; and one id the file can't have keeps none of the
// rest of the file out.
func TestCopiedAndWritten(t *testing.T) {
	l := open(t, time.UTC)
	ctx := context.Background()
	o := verdict.Options{Lead: time.Minute, UnknownLen: time.Hour}
	at := time.Date(2026, 11, 2, 1, 0, 0, 0, time.UTC)
	if _, err := l.Put(ctx, incident.Incident{ID: "A-1", Start: at}, "ana", o); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Put(ctx, incident.Incident{ID: "A-1", Start: at.Add(time.Hour)}, Copier, o); !errors.Is(err, ErrNotYours) {
		t.Errorf("the copier over a person's: %v", err)
	}
	if err := l.Delete(ctx, "A-1", Copier, o); !errors.Is(err, ErrNotYours) {
		t.Errorf("the copier deleting a person's: %v", err)
	}
	if _, err := l.Put(ctx, incident.Incident{ID: "PD-1", Start: at}, Copier, o); err != nil {
		t.Fatal(err)
	}
	if in, _ := l.IncidentByID(ctx, "PD-1"); in.Origin != "copy" {
		t.Errorf("origin %q", in.Origin)
	}
	if _, err := l.Put(ctx, incident.Incident{ID: "PD-1", Start: at, Title: "mine now"}, "ana", o); !errors.Is(err, ErrNotYours) {
		t.Errorf("a person over a copy: %v", err)
	}
	big := incident.Labels{}
	for i := range 32 {
		big[fmt.Sprint("k", i)] = []string{strings.Repeat("<", 256), strings.Repeat("&", 256)}
	}
	if _, err := l.Put(ctx, incident.Incident{ID: "B-1", Start: at, Labels: big}, "ana", o); err == nil {
		t.Error("labels over 16 KB were taken")
	}
	// What is kept is as written, not six times its size.
	if _, err := l.Put(ctx, incident.Incident{ID: "B-2", Start: at, Labels: incident.Labels{"x": {"<a&b>"}}}, "ana", o); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := l.r.QueryRowContext(ctx, "SELECT labels FROM incidents WHERE key = 'B-2'").Scan(&raw); err != nil || raw != `{"x":["<a&b>"]}` {
		t.Errorf("labels kept as %q: %v", raw, err)
	}
	for _, when := range []time.Time{time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(1969, 12, 31, 0, 0, 0, 0, time.UTC)} {
		if _, err := l.Put(ctx, incident.Incident{ID: "C-1", Start: when}, "ana", o); err == nil {
			t.Errorf("a start of %s was taken", when)
		}
	}
	// The file has an id a person used, a new one, and a change.
	if _, err := l.SyncFile(ctx, []incident.Incident{{ID: "F-1", Start: at}}, o); err != nil {
		t.Fatal(err)
	}
	changed, err := l.SyncFile(ctx, []incident.Incident{{ID: "F-1", Start: at, Title: "changed"}, {ID: "A-1", Start: at}, {ID: "F-2", Start: at}}, o)
	if !errors.Is(err, ErrTaken) || !changed || !strings.Contains(err.Error(), "A-1") {
		t.Errorf("changed %v, %v", changed, err)
	}
	if in, err := l.IncidentByID(ctx, "F-1"); err != nil || in.Title != "changed" {
		t.Errorf("F-1: %+v %v", in, err)
	}
	if _, err := l.IncidentByID(ctx, "F-2"); err != nil {
		t.Errorf("F-2: %v", err)
	}
	if in, _ := l.IncidentByID(ctx, "A-1"); in.Origin != "ui" {
		t.Errorf("A-1 taken over by the file: %+v", in)
	}
}
