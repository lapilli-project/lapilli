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
		if err := l.PutCheck(ctx, day, c, day.Add(24*time.Hour)); err != nil {
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
