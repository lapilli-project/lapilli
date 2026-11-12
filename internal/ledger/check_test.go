package ledger

import (
	"context"
	"fmt"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/verdict"
)

// A day checked again replaces what was kept for it, never adds to it; a
// rule keeps its last twenty misses; --keep drops days and misses as it does
// firings.
func TestPutCheck(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	day := t0
	var misses []verdict.Miss
	for i := range 25 {
		misses = append(misses, verdict.Miss{Way: "real", Firing: firing.Firing{Rule: "A", Start: day.Add(time.Duration(i) * time.Minute)}})
	}
	c := &verdict.SpanCheck{Rules: map[string]*verdict.SpanRule{"A": {Replayed: 10, Real: 12, Alike: 9, OffSum: 9 * time.Minute, Misses: misses}},
		Elsewhere: map[string]int{"Watchdog": 3}}
	for range 2 {
		if err := l.PutCheck(ctx, day, c, nil, day.Add(24*time.Hour), 0); err != nil {
			t.Fatal(err)
		}
	}
	cs, err := l.Checks(ctx, day, day.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if a := cs["A"]; a.Replayed != 10 || a.Real != 12 || a.Alike != 9 || a.Off != time.Minute || a.Either() != 13 {
		t.Errorf("A: %+v", a)
	}
	ms, err := l.CheckMisses(ctx, "A")
	if err != nil || len(ms) != maxMisses || !ms[0].Start.Equal(day.Add(24*time.Minute)) {
		t.Errorf("misses: %d %v, the latest %v", len(ms), err, ms[0].Start)
	}
	if el, _ := l.CheckElsewhere(ctx, day, day.Add(24*time.Hour)); el["Watchdog"] != 3 {
		t.Errorf("elsewhere: %v", el)
	}
	if to, _ := l.CheckedTo(ctx); !to.Equal(day.Add(24 * time.Hour)) {
		t.Errorf("checked to %s", to)
	}
	if err := l.Prune(ctx, day.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	cs, _ = l.Checks(ctx, day, day.Add(24*time.Hour))
	ms, _ = l.CheckMisses(ctx, "A")
	el, _ := l.CheckElsewhere(ctx, day, day.Add(24*time.Hour))
	if len(cs)+len(ms)+len(el) != 0 {
		t.Errorf("kept past --keep: %v %v %v", cs, ms, el)
	}
}

// The rules a run couldn't replay are those of the runs meeting a span.
func TestFailedRules(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	for i, failed := range []map[string]string{{"A": "boom"}, {"B": "boom"}} {
		from := t0.Add(time.Duration(i) * 48 * time.Hour)
		if err := l.AddRun(ctx, Run{Kind: "test", Started: from, Finished: from, From: from, To: from.Add(24 * time.Hour), Failed: failed}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := l.FailedRules(ctx, t0.Add(12*time.Hour), t0.Add(30*time.Hour))
	if err != nil || fmt.Sprint(got) != "map[A:true]" {
		t.Errorf("failed: %v %v", got, err)
	}
}

// Where the clocks skip midnight, a day still starts on itself: the check
// steps from day to day and never stays on one.
func TestMidnightSkipped(t *testing.T) {
	for _, zone := range []string{"America/Santiago", "Atlantic/Azores", "America/Havana", "Asia/Seoul", "Europe/Berlin"} {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatal(err)
		}
		l := open(t, loc)
		start := l.Day(time.Date(2026, 1, 1, 12, 0, 0, 0, loc))
		for d := start; d < start+366; d++ {
			if got := l.day(l.midnight(d)); got != d {
				t.Fatalf("%s: day %d starts at %s, on day %d", zone, d, time.Unix(l.midnight(d), 0).In(loc), got)
			}
		}
	}
}

// The check begins where the runs that replay began, not a digest's or a
// review's.
func TestRunsFrom(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	for _, r := range []Run{
		{Kind: "digest", From: t0.Add(-7 * 24 * time.Hour), To: t0},
		{Kind: "backfill", From: t0.Add(-24 * time.Hour), To: t0},
		{Kind: "hourly", From: t0, To: t0.Add(time.Hour)},
	} {
		r.Started, r.Finished = r.To, r.To
		if err := l.AddRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := l.RunsFrom(ctx); err != nil || !got.Equal(t0.Add(-24*time.Hour)) {
		t.Errorf("runs from %s %v", got, err)
	}
}

// Rules that share an alert name are counted together, under each name the
// ledger judges them by, and once in a sum.
func TestCheckShared(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	c := &verdict.SpanCheck{Rules: map[string]*verdict.SpanRule{"Errors": {Replayed: 4, Real: 3, Alike: 3}, "Disk": {Replayed: 1, Real: 1, Alike: 1}}}
	if err := l.PutCheck(ctx, t0, c, map[string][]string{"Errors": {"Errors [a]", "Errors [b]"}, "Disk": {"Disk"}}, t0.Add(24*time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	cs, err := l.Checks(ctx, t0, t0.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if a, b := cs["Errors [a]"], cs["Errors [b]"]; !a.Shared || a.Alert != "Errors" || a.Replayed != 4 || b != a || cs["Disk"].Shared {
		t.Errorf("checks: %+v", cs)
	}
	everyone := func(string) bool { return true }
	all, err := l.CheckTotal(ctx, t0, t0.Add(24*time.Hour), everyone)
	if err != nil || all.Replayed != 5 || all.Real != 4 {
		t.Errorf("sum: %+v %v", all, err)
	}
	if differ := Differing(cs, everyone, 1, 0.9); len(differ) != 2 {
		t.Errorf("differ %v", differ)
	}
	// The next day, one of them alone has the name: both days are counted,
	// each once.
	next := t0.Add(24 * time.Hour)
	c = &verdict.SpanCheck{Rules: map[string]*verdict.SpanRule{"Errors": {Replayed: 7, Real: 7, Alike: 7}}}
	if err := l.PutCheck(ctx, next, c, map[string][]string{"Errors": {"Errors [b]"}}, next.Add(24*time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	if all, err := l.CheckTotal(ctx, t0, next.Add(24*time.Hour), func(name string) bool { return name != "Disk" }); err != nil || all.Replayed != 11 {
		t.Errorf("over both days: %+v %v; want 4 + 7 replayed", all, err)
	}
}
