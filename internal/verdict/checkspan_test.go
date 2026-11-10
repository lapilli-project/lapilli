package verdict

import (
	"context"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/rules"
)

// Spans side by side count each firing once: an alike pair in the span of
// the earlier start, a firing found one way in the span of its own; and
// what is read past a span's ends only matches, never counts.
func TestCheckSpan(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	step := time.Minute
	known := []rules.Rule{{Name: "A"}, {Name: "B"}, {Name: "C", Unsupported: "Loki"}}
	f := func(rule string, start time.Time, labels map[string]string) firing.Firing {
		return firing.Firing{Rule: rule, Start: start, End: start.Add(10 * time.Minute), Labels: labels}
	}
	svc := func(s string) map[string]string { return map[string]string{"service": s} }
	replayed := []firing.Firing{
		f("A", at.Add(time.Hour), svc("x")),          // alike, 30s off
		f("A", at.Add(2*time.Hour), svc("x")),        // only replayed
		f("A", at.Add(day-30*time.Second), svc("x")), // alike across midnight: the first day's
		f("A", at.Add(day+time.Hour), svc("x")),      // the second day's, alike
		f("B", at.Add(3*time.Hour), svc("y")),        // labels disagree: one each way
		f("C", at.Add(time.Hour), nil),               // not replayed: not checked
		f("A", at.Add(-time.Minute), svc("x")),       // before: matches only
	}
	real := []firing.Firing{
		f("A", at.Add(time.Hour+30*time.Second), svc("x")),
		f("A", at.Add(day+30*time.Second), svc("x")),
		f("A", at.Add(day+time.Hour), svc("x")),
		f("B", at.Add(3*time.Hour), svc("z")),
		f("A", at.Add(5*time.Hour), map[string]string{"service": "x", "cluster": "eu"}), // only real
		f("Watchdog", at.Add(4*time.Hour), nil),
		f("A", at.Add(-time.Minute), svc("x")),
	}
	one, ok := CheckSpan(ctx, known, replayed, real, at, at.Add(day), step)
	if !ok {
		t.Fatal("unfinished")
	}
	two, _ := CheckSpan(ctx, known, replayed, real, at.Add(day), at.Add(2*day), step)
	a1, a2, b1 := one.Rules["A"], two.Rules["A"], one.Rules["B"]
	if a1.Alike != 2 || a1.Replayed != 3 || a1.Real != 3 || a1.OffSum != 90*time.Second {
		t.Errorf("A, the first day: %+v", a1)
	}
	if a2.Alike != 1 || a2.Replayed != 1 || a2.Real != 1 {
		t.Errorf("A, the second day: %+v", a2)
	}
	if b1.Alike != 0 || b1.Replayed != 1 || b1.Real != 1 || len(b1.Misses) != 2 {
		t.Errorf("B: %+v", b1)
	}
	if _, ok := one.Rules["C"]; ok {
		t.Errorf("a rule not replayed was checked")
	}
	if one.Elsewhere["Watchdog"] != 1 || len(two.Elsewhere) != 0 {
		t.Errorf("elsewhere: %v %v", one.Elsewhere, two.Elsewhere)
	}
	if len(a1.Misses) != 2 || a1.Misses[0].Way != "replayed" || a1.Misses[1].Way != "real" {
		t.Errorf("A's misses: %+v", a1.Misses)
	}
}

// An alert that fired again and again across midnight is paired alike from
// either day: each firing is counted once, however far back its run of
// firings goes.
func TestCheckSpanAcrossMidnight(t *testing.T) {
	ctx := context.Background()
	mid := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	at := func(h, m, s int) time.Time { return mid.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second) }
	f := func(start time.Time) firing.Firing { return firing.Firing{Rule: "A", Start: start, End: start.Add(20 * time.Second)} }
	replayed := []firing.Firing{f(at(-1, 58, 30)), f(at(0, 0, 0))}
	real := []firing.Firing{f(at(-1, 57, 59)), f(at(-1, 59, 20)), f(at(0, 0, 50))}
	known := []rules.Rule{{Name: "A"}}
	var r, x, alike int
	for _, day := range []time.Time{mid.Add(-24 * time.Hour), mid} {
		c, _ := CheckSpan(ctx, known, replayed, real, day, day.Add(24*time.Hour), time.Minute)
		r += c.Rules["A"].Replayed
		x += c.Rules["A"].Real
		alike += c.Rules["A"].Alike
	}
	if r != 2 || x != 3 || alike != 2 {
		t.Errorf("over both days: %d replayed, %d real, %d alike; want 2, 3, 2", r, x, alike)
	}
}
