package replay

import (
	"context"
	"testing"
	"time"

	"lapilli.dev/internal/rules"
)

// A group placed within its interval is replayed from its last evaluation
// before the period, so a firing on when the period starts that ends before
// the group's first evaluation in it is replayed as it fired. The group
// runs every five minutes, at 2m30s past: its evaluations fall at
// 23:57:30, 00:02:30, 00:07:30 on, the period starting at 00:00.
func TestPlacedFiringOnAtTheStart(t *testing.T) {
	from := t0
	at := func(h, m, s int) time.Time {
		return t0.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second)
	}
	// holds says the condition holds at an evaluation in [a, b).
	holds := func(a, b time.Time) stepped {
		return stepped{"x > 0": func(t time.Time) (float64, bool) { return 1, !t.Before(a) && t.Before(b) }}
	}
	for _, c := range []struct {
		name     string
		rule     rules.Rule
		h        stepped
		start    time.Time
		end      time.Time
		noFiring bool
	}{
		{"ends before the first evaluation in the period", rules.Rule{},
			holds(at(-1, 57, 0), at(-1, 59, 0)), from, at(0, 2, 30), false},
		{"goes on past it", rules.Rule{},
			holds(at(-1, 57, 0), at(0, 4, 0)), from, at(0, 7, 30), false},
		{"for, met before the last evaluation", rules.Rule{For: 3 * time.Minute},
			holds(at(-1, 47, 0), at(-1, 58, 0)), from, at(0, 2, 30), false},
		{"keep_firing_for, held on from one before", rules.Rule{KeepFiringFor: 7 * time.Minute},
			holds(at(-1, 52, 0), at(-1, 53, 0)), from, at(0, 7, 30), false},
		// Gone before the last evaluation before the period: not on at its
		// start, so not reported.
		{"ended before the period", rules.Rule{},
			holds(at(-1, 47, 0), at(-1, 53, 0)), time.Time{}, time.Time{}, true},
		// Pending at the last evaluation, a for not yet met: it never fired.
		{"for, not met", rules.Rule{For: 10 * time.Minute},
			holds(at(-1, 57, 0), at(-1, 59, 0)), time.Time{}, time.Time{}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := c.rule
			r.Name, r.Expr, r.Interval, r.Phase = "X", "x > 0", 5*time.Minute, 2*time.Minute+30*time.Second
			_, fs := Run(context.Background(), c.h, one([]rules.Rule{r}), from, from.Add(15*time.Minute), time.Minute, 1)
			if c.noFiring {
				if len(fs) != 0 {
					t.Fatalf("%+v, want none", fs)
				}
				return
			}
			if len(fs) != 1 || !fs[0].Start.Equal(c.start) || !fs[0].End.Equal(c.end) {
				t.Fatalf("%+v, want one from %s to %s", fs, c.start.Format("15:04:05"), c.end.Format("15:04:05"))
			}
		})
	}
}

// A group whose first evaluation in the period falls on its start has no
// evaluation before it to ask for; one placed later asks from its last
// evaluation before it, whole steps back, and a "for" earlier than that.
func TestPlacedStartOfTheReplay(t *testing.T) {
	step := time.Minute
	r := rules.Rule{Name: "X", Expr: "x > 0", Interval: 5 * time.Minute, For: step}
	st := stint{Rule: r, lo: t0, hi: t0.Add(time.Hour), before: true}
	// On the grid of the period's start: the "for" before it.
	if lo, _ := st.span(step); !lo.Equal(t0.Add(-step)) {
		t.Errorf("on the start: from %s, want %s", lo, t0.Add(-step))
	}
	// Placed 2m past: its first evaluation in the period is at 00:02, its
	// last before it at 23:57, asked from 23:56 (the "for"), and one
	// evaluation earlier.
	st.Phase = 2 * time.Minute
	if lo, _ := st.span(step); !lo.Equal(t0.Add(-3*time.Minute - step - 5*time.Minute)) {
		t.Errorf("placed: from %s", lo)
	}
	// No "for" nor keep_firing_for: the evaluation itself.
	st.For = 0
	if lo, _ := st.span(step); !lo.Equal(t0.Add(-3 * time.Minute)) {
		t.Errorf("placed, no for: from %s", lo)
	}
}
