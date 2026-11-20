package ledger

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"
)

// A Router picks, as each event is written, the places of those it may go
// to that it goes to: one row each, in the write, and none for the rest,
// whatever writes it (a task's change, TellOnce, a goal missed). It is
// shown what is told of the event. Without one, every place is told.
func TestOutboxRouted(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	l.TellTo([]string{"webhook", "route:ops"})
	defer l.TellTo(nil)
	type asked struct {
		event string
		about map[string]any
		dests []string
	}
	var seen []asked
	l.RouteBy(func(event string, about []byte, dests []string) []string {
		var a map[string]any
		json.Unmarshal(about, &a)
		seen = append(seen, asked{event, a, slices.Clone(dests)})
		if event == "task.created" {
			return []string{"route:ops"}
		}
		return dests[:1]
	})
	if _, err := l.PutTask(ctx, Task{Title: "tune Errors", Kind: "tune"}, "ana"); err != nil {
		t.Fatal(err)
	}
	told := []Told{{Dest: "webhook", About: map[string]any{"rule": "Errors"}}, {Dest: "route:ops", About: map[string]any{"rule": "Errors"}}}
	if err := l.TellOnce(ctx, "differs.Errors", "1", "replay.differs", told); err != nil {
		t.Fatal(err)
	}
	if err := l.TellGoalMissed(ctx, "g", []int64{0}, 1, slices.Clone(told)); err != nil {
		t.Fatal(err)
	}
	rows := func(dest string) []string {
		due, err := l.Due(ctx, dest, time.Now().Add(time.Hour), 10)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, n := range due {
			out = append(out, n.Event)
		}
		return out
	}
	if got := rows("route:ops"); !slices.Equal(got, []string{"task.created"}) {
		t.Errorf("route:ops: %v", got)
	}
	if got := rows("webhook"); !slices.Equal(got, []string{"replay.differs", "goal.missed"}) {
		t.Errorf("webhook: %v", got)
	}
	if len(seen) != 3 || seen[0].about["task"] == nil || seen[0].about["by"] != "ana" || seen[1].about["rule"] != "Errors" ||
		!slices.Equal(seen[1].dests, []string{"webhook", "route:ops"}) || seen[2].event != "goal.missed" {
		t.Errorf("asked: %+v", seen)
	}
	// Told once all the same: the key is set though no place was told.
	if err := l.TellOnce(ctx, "differs.Errors", "1", "replay.differs", told); err != nil {
		t.Fatal(err)
	}
	if got := rows("webhook"); len(got) != 2 {
		t.Errorf("told twice: %v", got)
	}
	l.RouteBy(nil)
	if _, err := l.PutTask(ctx, Task{Title: "write a runbook", Kind: "write"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if got := rows("webhook"); len(got) != 3 || len(rows("route:ops")) != 2 {
		t.Errorf("without a router, every place: %v %v", got, rows("route:ops"))
	}
}

// A place's standing says how many wait, and LastError why the latest
// still unsent failed: one waiting, before one given up on; none once
// sent.
func TestOutboxLastError(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	l.TellTo([]string{"route:ops"})
	defer l.TellTo(nil)
	for _, title := range []string{"a", "b", "c"} {
		if _, err := l.PutTask(ctx, Task{Title: title, Kind: "tune"}, "ana"); err != nil {
			t.Fatal(err)
		}
	}
	due, err := l.Due(ctx, "route:ops", time.Now(), 10)
	if err != nil || len(due) != 3 {
		t.Fatal(err, len(due))
	}
	if why, err := l.LastError(ctx, "route:ops"); err != nil || why != "" {
		t.Errorf("none failed: %q %v", why, err)
	}
	for i := 0; i < GiveUpAfter; i++ {
		if err := l.Failed(ctx, due[0].ID, "HTTP 410: gone"); err != nil {
			t.Fatal(err)
		}
	}
	if why, _ := l.LastError(ctx, "route:ops"); why != "HTTP 410: gone" {
		t.Errorf("given up: %q", why)
	}
	if err := l.Failed(ctx, due[1].ID, "HTTP 503: busy"); err != nil {
		t.Fatal(err)
	}
	if why, _ := l.LastError(ctx, "route:ops"); why != "HTTP 503: busy" {
		t.Errorf("waiting, before given up: %q", why)
	}
	st, err := l.Standing(ctx, []string{"route:ops"}, time.Now().Add(-time.Hour))
	if err != nil || st.Waiting != 2 || st.Failed != 1 || st.GivenUp != 1 {
		t.Errorf("standing: %+v %v", st, err)
	}
	if err := l.Sent(ctx, due[1].ID); err != nil {
		t.Fatal(err)
	}
	if why, _ := l.LastError(ctx, "route:ops"); why != "HTTP 410: gone" {
		t.Errorf("sent: %q", why)
	}
}
