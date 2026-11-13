package ledger

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// fill writes a little of everything a ledger keeps: firings and their
// stones, incidents of each origin, a version of the rules and its
// documents, people's teams, notes, windows, tasks (one deleted, so the
// next id is past it) and comments, a grant, a report, notifications, a
// source's firings, on-call turns, results, the outbox.
func fill(t testing.TB, l *Ledger) {
	t.Helper()
	ctx := context.Background()
	l.TellTo([]string{"webhook"})
	defer l.TellTo(nil)
	if _, err := l.SyncFile(ctx, []incident.Incident{{ID: "INC-1", Title: "Checkout down", Start: t0.Add(2 * time.Hour), End: t0.Add(3 * time.Hour),
		Labels: incident.Labels{"service": {"checkout"}}}}, opts); err != nil {
		t.Fatal(err)
	}
	docs := []rules.Doc{{Source: "shop.yaml", Data: []byte("groups: []\n")}}
	if _, err := l.AddVersion(ctx, Version{Since: t0, Digest: "d1", Docs: docs, Names: []string{"Errors", "Latency"}}); err != nil {
		t.Fatal(err)
	}
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{
		{Rule: "Errors", Labels: map[string]string{"service": "checkout", "pod": "a"}, Start: t0.Add(2*time.Hour + 5*time.Minute), End: t0.Add(2*time.Hour + 20*time.Minute)},
		{Rule: "Errors", Labels: map[string]string{"service": "checkout", "pod": "a"}, Start: t0.Add(2*time.Hour + 30*time.Minute), End: t0.Add(2*time.Hour + 40*time.Minute)},
		{Rule: "Latency", Labels: map[string]string{"service": "search"}, Start: t0.Add(9 * time.Hour)},
		{Rule: "Latency", Labels: map[string]string{"service": "search", "zone": "ü-1"}, Start: t0.Add(10 * time.Hour), End: t0.Add(11 * time.Hour)},
	})
	if _, err := l.Put(ctx, incident.Incident{Title: "Search slow", Start: t0.Add(9 * time.Hour), End: t0.Add(10 * time.Hour),
		Labels: incident.Labels{"service": {"search"}}}, "ana", opts); err != nil {
		t.Fatal(err)
	}
	team, err := l.PutTeam(ctx, Team{Name: "shop", Rules: "^Errors$", Labels: map[string][]string{"service": {"checkout"}}}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.PutNote(ctx, Note{Rule: "Errors", Status: "keep", Runbook: "https://runbooks.example.com/errors"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutWindow(ctx, Window{Title: "deploy", Start: t0.Add(12 * time.Hour), End: t0.Add(13 * time.Hour)}, "ana", opts); err != nil {
		t.Fatal(err)
	}
	gone, err := l.PutTask(ctx, Task{Title: "Tune Errors", Rule: "Errors"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutTask(ctx, Task{Title: "Look at Latency", Rule: "Latency", TeamID: team}, "ana"); err != nil {
		t.Fatal(err)
	}
	last, err := l.PutTask(ctx, Task{Title: "Last", Rule: "Errors"}, "bo")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{gone, last} {
		if err := l.DeleteTask(ctx, id, "ana"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.PutComment(ctx, Comment{On: "rule", Key: "Errors", Body: "Noisy at night, isn't it?"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutGrant(ctx, Grant{Who: "user:bo", TeamID: team}, "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutScheduled(ctx, Scheduled{Name: "shop weekly", Schedule: "weekly mon@09:00", Zone: "ledger", Sections: []string{"summary"},
		Dests: []string{"webhook"}, Enabled: true}, "ana", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Notify(ctx, Notification{At: t0.Add(2*time.Hour + 6*time.Minute), Source: "alertmanager", Receiver: "pager", Group: `{}:{alertname="Errors"}`,
		Alerts: []Alert{{Rule: "Errors", Labels: map[string]string{"service": "checkout", "pod": "a"}, Start: t0.Add(2*time.Hour + 5*time.Minute)}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutFirings(ctx, "cw", t0, t0.Add(24*time.Hour), []firing.Firing{{Rule: "cpu-high", Labels: map[string]string{"instance": "i-1"},
		Start: t0.Add(4 * time.Hour), End: t0.Add(5 * time.Hour)}}, FiringsCopier, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutTurns(ctx, "shop", t0, []verdict.RotaTurn{{From: t0, To: t0.Add(12 * time.Hour), Who: "Ana"}}); err != nil {
		t.Fatal(err)
	}
	if err := l.AddResult(ctx, "suggestions", t0.Add(24*time.Hour), t0, t0.Add(24*time.Hour), map[string]int{"Errors": 2}); err != nil {
		t.Fatal(err)
	}
	if err := l.SetMeta(ctx, "ledger-id", "0123456789ab"); err != nil {
		t.Fatal(err)
	}
}

// A ledger copied into an empty one is the same ledger: every row, read
// back alike, and the same answers; the next id each table gives comes
// after the highest it ever gave. A ledger that isn't empty is refused.
func TestCopy(t *testing.T) {
	ctx := context.Background()
	seoul, _ := time.LoadLocation("Asia/Seoul")
	src, err := Open(ctx, filepath.Join(t.TempDir(), "ledger.db"), seoul)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	fill(t, src)
	where := place(t)
	dst, err := openAt(ctx, where, nil)
	if err != nil {
		t.Fatal(err)
	}
	var progress []string
	rep, err := Copy(ctx, src, dst, func(table string, rows int64) { progress = append(progress, table) })
	if err != nil {
		t.Fatal(err)
	}
	names, _ := src.tables(ctx)
	if len(rep.Tables) != len(names) || len(progress) != len(names) {
		t.Fatalf("%d tables copied of %d: %v", len(rep.Tables), len(names), rep.Tables)
	}
	rows := map[string]int64{}
	for _, tb := range rep.Tables {
		rows[tb.Name] = tb.Rows
	}
	for _, tb := range []string{"firings", "series", "rules", "incidents", "catches", "days", "weeks", "versions", "docs", "teams", "notes",
		"windows", "tasks", "comments", "grants", "reports", "notifications", "notified", "sources", "on_call_turns", "results", "outbox", "changes", "meta"} {
		if rows[tb] == 0 {
			t.Errorf("%s: nothing copied", tb)
		}
	}
	dst.Close()
	// Opened as serve opens it, in its zone.
	dst, err = openAt(ctx, where, seoul)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	same := func(what string, f func(l *Ledger) (any, error)) {
		t.Helper()
		a, err := f(src)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		b, err := f(dst)
		if err != nil {
			t.Fatalf("%s, copied: %v", what, err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s:\n  %+v\ncopied:\n  %+v", what, a, b)
		}
	}
	same("the report", func(l *Ledger) (any, error) { return l.Report(ctx, t0, t0.Add(24*time.Hour), opts, true) })
	same("the weeks", func(l *Ledger) (any, error) { return l.Weeks(ctx, Week(l.Day(t0))-7, Week(l.Day(t0))+7) })
	same("the tasks", func(l *Ledger) (any, error) { return l.Tasks(ctx, TaskQuery{}) })
	same("the changes", func(l *Ledger) (any, error) { return l.Changes(ctx, 100) })
	same("the notifications", func(l *Ledger) (any, error) { return l.Notifications(ctx, t0, t0.Add(24*time.Hour)) })
	same("the sources", func(l *Ledger) (any, error) { return l.Sources(ctx) })
	same("the ledger's id", func(l *Ledger) (any, error) { return l.Meta(ctx, "ledger-id") })
	// The deleted task's id isn't given again, here or there.
	a, err := src.PutTask(ctx, Task{Title: "After"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	b, err := dst.PutTask(ctx, Task{Title: "After"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("the next task is %d, copied %d", a, b)
	}
	// Not into one that holds a ledger.
	if _, err := Copy(ctx, src, dst, nil); !errors.Is(err, ErrNotEmpty) || !strings.Contains(err.Error(), "in firings") {
		t.Errorf("into a ledger that isn't empty: %v", err)
	}
}

// What a Postgres ledger holds copies back into SQLite alike: a way back.
func TestCopyBack(t *testing.T) {
	ctx := context.Background()
	pg := open(t, time.UTC)
	if pg.d != postgres {
		t.Skip("the ledgers are SQLite's")
	}
	fill(t, pg)
	back, err := Open(ctx, filepath.Join(t.TempDir(), "ledger.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	if _, err := Copy(ctx, pg, back, nil); err != nil {
		t.Fatal(err)
	}
	a, err := pg.PutTask(ctx, Task{Title: "After"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	b, err := back.PutTask(ctx, Task{Title: "After"}, "ana")
	if err != nil || a != b {
		t.Errorf("the next task is %d, copied back %d (%v)", a, b, err)
	}
}

// A ledger that holds no more than a value kept for serve isn't empty.
func TestCopyRefusesAKeptValue(t *testing.T) {
	ctx := context.Background()
	src, err := Open(ctx, filepath.Join(t.TempDir(), "ledger.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst := open(t, time.UTC)
	if err := dst.SetMeta(ctx, "ledger-id", "0123456789ab"); err != nil {
		t.Fatal(err)
	}
	if _, err := Copy(ctx, src, dst, nil); !errors.Is(err, ErrNotEmpty) || !strings.Contains(err.Error(), "1 in meta") {
		t.Errorf("into a ledger holding its id: %v", err)
	}
}

// What a copy is read back by tells two ledgers apart by any value of any
// row, and two alike as alike, whichever database each is in.
func TestChecksumTellsApart(t *testing.T) {
	ctx := context.Background()
	a := open(t, time.UTC)
	b, err := Open(ctx, filepath.Join(t.TempDir(), "ledger.db"), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	cols, err := a.columns(ctx, "tasks")
	if err != nil {
		t.Fatal(err)
	}
	sum := func(l *Ledger) string {
		s, _, err := checksum(ctx, l.r, "tasks", cols)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	for _, l := range []*Ledger{a, b} {
		if _, err := l.PutTask(ctx, Task{Title: "Tune Errors", Rule: "Errors"}, "ana"); err != nil {
			t.Fatal(err)
		}
		if _, err := l.w.ExecContext(ctx, "UPDATE tasks SET created = 1, updated = 1"); err != nil {
			t.Fatal(err)
		}
	}
	if sum(a) != sum(b) {
		t.Fatal("alike, told apart")
	}
	if _, err := b.w.ExecContext(ctx, "UPDATE tasks SET title = 'Tune errors'"); err != nil {
		t.Fatal(err)
	}
	if sum(a) == sum(b) {
		t.Error("a title changed, read back alike")
	}
	if _, err := b.w.ExecContext(ctx, "UPDATE tasks SET title = 'Tune Errors', due = 5"); err != nil {
		t.Fatal(err)
	}
	if sum(a) == sum(b) {
		t.Error("a NULL made 5, read back alike")
	}
}

// A copy that wrote other than it read is refused when read back, before
// it commits: one value of one row changed on its way fails it, and the
// ledger copied into is left empty, to be copied into again.
func TestCopyReadBack(t *testing.T) {
	ctx := context.Background()
	src, err := Open(ctx, filepath.Join(t.TempDir(), "ledger.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	fill(t, src)
	dst := open(t, nil)
	copying = func(table string, row []any) {
		if table == "tasks" {
			row[1] = "changed on its way"
		}
	}
	defer func() { copying = nil }()
	if _, err := Copy(ctx, src, dst, nil); err == nil || !strings.Contains(err.Error(), "tasks: read back, the copy differs") {
		t.Errorf("a row changed on its way: %v", err)
	}
	names, err := dst.tables(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := empty(ctx, dst, names); err != nil {
		t.Errorf("a copy refused left the ledger copied into holding it: %v", err)
	}
	copying = nil
	if _, err := Copy(ctx, src, dst, nil); err != nil {
		t.Errorf("copied again: %v", err)
	}
}

// A ledger written while it is copied, by a serve left running, is
// copied as it was when the copy began, and read back as it was then: the
// copy holds the ledger before the write, and gives next the id the write
// took.
func TestCopyWrittenMeanwhile(t *testing.T) {
	ctx := context.Background()
	src, err := Open(ctx, filepath.Join(t.TempDir(), "ledger.db"), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	fill(t, src)
	before, err := src.Tasks(ctx, TaskQuery{})
	if err != nil {
		t.Fatal(err)
	}
	dst := open(t, time.UTC)
	var meanwhile int64
	copying = func(table string, row []any) {
		if meanwhile == 0 && table == "tasks" {
			if meanwhile, err = src.PutTask(ctx, Task{Title: "Meanwhile"}, "ana"); err != nil {
				t.Errorf("a write meanwhile: %v", err)
			}
		}
	}
	defer func() { copying = nil }()
	if _, err := Copy(ctx, src, dst, nil); err != nil {
		t.Fatalf("copied while written: %v", err)
	}
	if meanwhile == 0 {
		t.Fatal("nothing written meanwhile")
	}
	got, err := dst.Tasks(ctx, TaskQuery{})
	if err != nil || len(got) != len(before) {
		t.Errorf("the copy's tasks: %d, %v; the ledger had %d when the copy began", len(got), err, len(before))
	}
	if id, err := dst.PutTask(ctx, Task{Title: "next"}, "ana"); err != nil || id != meanwhile {
		t.Errorf("the copy's next task: %d %v, want %d", id, err, meanwhile)
	}
}
