package replay

import (
	"context"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
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

// Every replay of a placed group starts where Run's does: a longer "for"
// tried, the condition's stretches, and the firings of a rule with another
// number all see a firing on when the period starts that ends before the
// group's first evaluation in it. A longer "for" quiets it only when the
// evaluations before the period say it would not have fired by then.
func TestPlacedReplaysAgreeAtTheStart(t *testing.T) {
	from := t0
	at := func(m, s int) time.Time { return from.Add(time.Duration(m)*time.Minute + time.Duration(s)*time.Second) }
	// Held at the evaluations from 23:42:30 to 23:57:30, then not: on at
	// the period's start for 3m, and until 00:02:30.
	h := stepped{"x > 0": func(t time.Time) (float64, bool) { return 1, !t.Before(at(-18, 0)) && t.Before(at(-2, 0)) }}
	r := rules.Rule{Name: "X", Expr: "x > 0", Interval: 5 * time.Minute, Phase: 2*time.Minute + 30*time.Second, For: 3 * time.Minute}
	to := from.Add(15 * time.Minute)
	ctx := context.Background()
	_, run := Run(ctx, h, one([]rules.Rule{r}), from, to, time.Minute, 1)
	if len(run) != 1 || !run[0].Start.Equal(from) || !run[0].End.Equal(at(2, 30)) {
		t.Fatalf("Run: %+v", run)
	}
	alts, err := WithFor(ctx, h, r, []time.Duration{10 * time.Minute, 30 * time.Minute}, from, to, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// 10m is met by 23:52:30, before the last evaluation: not quieted. 30m
	// isn't met by it (held since 23:42:30): quieted, for real.
	if f := alts[10*time.Minute]; len(f) != 1 || !f[0].Start.Equal(from) || !f[0].End.Equal(at(2, 30)) {
		t.Errorf("a for of 10m: %+v, want the firing Run has", f)
	}
	if f := alts[30*time.Minute]; len(f) != 0 {
		t.Errorf("a for of 30m: %+v, want none", f)
	}
	all, _, err := Fired(ctx, h, r, from, to, time.Minute)
	if err != nil || len(all) != 1 || !all[0].Start.Equal(from) {
		t.Errorf("Fired: %+v %v", all, err)
	}
	held, err := Held(ctx, h, r, from, to, time.Minute)
	if err != nil || len(held) != 1 || !held[0].Start.Equal(from) || !held[0].End.Equal(at(2, 30)) {
		t.Errorf("Held: %+v %v", held, err)
	}
}

// A group whose file gives no interval, placed at the one the rules API
// says (Placed), is replayed as one with that interval is, though the
// rule's own Interval stays zero.
func TestPlacedAtTheAPIsInterval(t *testing.T) {
	h := stepped{"x > 0": func(t time.Time) (float64, bool) {
		return 1, !t.Before(t0.Add(-3*time.Minute)) && t.Before(t0.Add(-time.Minute))
	}}
	r := rules.Rule{Name: "X", Expr: "x > 0", Phase: 2*time.Minute + 30*time.Second, Placed: 5 * time.Minute}
	_, fs := Run(context.Background(), h, one([]rules.Rule{r}), t0, t0.Add(15*time.Minute), time.Minute, 1)
	if len(fs) != 1 || !fs[0].Start.Equal(t0) || !fs[0].End.Equal(t0.Add(2*time.Minute+30*time.Second)) {
		t.Errorf("%+v", fs)
	}
	r.Placed = 0
	if _, fs = Run(context.Background(), h, one([]rules.Rule{r}), t0, t0.Add(15*time.Minute), time.Minute, 1); len(fs) != 0 {
		t.Errorf("without it, on the grid: %+v", fs)
	}
}

// two series, a and b, each holding where its function says.
type twoSeries struct{ a, b func(time.Time) bool }

func (h twoSeries) QueryRange(_ context.Context, _ string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	sa := prom.Series{Labels: map[string]string{"s": "a"}}
	sb := prom.Series{Labels: map[string]string{"s": "b"}}
	for t := start; !t.After(end); t = t.Add(step) {
		if h.a(t) {
			sa.Samples = append(sa.Samples, prom.Sample{T: t, V: 1})
		}
		if h.b(t) {
			sb.Samples = append(sb.Samples, prom.Sample{T: t, V: 1})
		}
	}
	var out []prom.Series
	for _, s := range []prom.Series{sa, sb} {
		if len(s.Samples) > 0 {
			out = append(out, s)
		}
	}
	return out, nil
}

// A series that held only before the period doesn't change what is known to
// have started in it: the firing of another, that began after the period's
// start, is known with or without it.
func TestFiredKnownIgnoresASeriesOfBefore(t *testing.T) {
	at := func(m, s int) time.Time { return t0.Add(time.Duration(m)*time.Minute + time.Duration(s)*time.Second) }
	in := func(a, b time.Time) func(time.Time) bool {
		return func(t time.Time) bool { return !t.Before(a) && t.Before(b) }
	}
	b := func(t time.Time) bool { return in(at(0, 0), at(2, 0))(t) || in(at(3, 0), at(14, 0))(t) }
	r := rules.Rule{Name: "X", Expr: "x", Interval: 5 * time.Minute, Phase: 2*time.Minute + 30*time.Second}
	ctx := context.Background()
	_, with, err := Fired(ctx, twoSeries{in(at(-3, 0), at(-2, 0)), b}, r, t0, t0.Add(20*time.Minute), time.Minute)
	_, without, err2 := Fired(ctx, twoSeries{func(time.Time) bool { return false }, b}, r, t0, t0.Add(20*time.Minute), time.Minute)
	if err != nil || err2 != nil || len(without) == 0 || len(with) != len(without) {
		t.Errorf("known with a series of before: %+v, without: %+v (%v %v)", with, without, err, err2)
	}
}

// A rule that took effect after the evaluation before the period has no
// firing from it: it wasn't there to evaluate.
func TestPlacedRuleNewJustBeforeThePeriod(t *testing.T) {
	h := stepped{"x > 0": func(t time.Time) (float64, bool) {
		return 1, !t.Before(t0.Add(-3*time.Minute)) && t.Before(t0.Add(-time.Minute))
	}}
	r := rules.Rule{Name: "X", Expr: "x > 0", Interval: 5 * time.Minute, Phase: 2*time.Minute + 30*time.Second}
	ctx := context.Background()
	_, fs := Run(ctx, h, []Version{{From: t0.Add(-time.Minute), Rules: []rules.Rule{r}}}, t0, t0.Add(15*time.Minute), time.Minute, 1)
	if len(fs) != 0 {
		t.Errorf("a rule added at 23:59 fired from an evaluation at 23:57:30: %+v", fs)
	}
	// In effect from 23:55, it was there to run it.
	_, fs = Run(ctx, h, []Version{{From: t0.Add(-5 * time.Minute), Rules: []rules.Rule{r}}}, t0, t0.Add(15*time.Minute), time.Minute, 1)
	if len(fs) != 1 || !fs[0].Start.Equal(t0) {
		t.Errorf("in effect before it: %+v", fs)
	}
}

// A placed group evaluates a fraction of a second past the whole second its
// place says, so an evaluation falling on the period's end is made after it:
// what it writes is past the period, and a replay that counted it would have
// a firing the real server's series, read up to the end, never had. The
// group runs every five minutes at 2m past, a minute step: 00:02 and 00:07;
// the period ends at 00:07 and the condition holds at that evaluation alone.
func TestPlacedEvaluationOnThePeriodsEnd(t *testing.T) {
	end := t0.Add(7 * time.Minute)
	h := stepped{"x > 0": func(t time.Time) (float64, bool) { return 1, t.Equal(end) }}
	r := rules.Rule{Name: "X", Expr: "x > 0", Interval: 5 * time.Minute, Phase: 2 * time.Minute}
	if _, fs := Run(context.Background(), h, one([]rules.Rule{r}), t0, end, time.Minute, 1); len(fs) != 0 {
		t.Errorf("placed: %+v, want none", fs)
	}
	// Not placed, it is evaluated on the steps' grid, and at the end.
	r.Phase = 0
	r.Interval = time.Minute
	if _, fs := Run(context.Background(), h, one([]rules.Rule{r}), t0, end, time.Minute, 1); len(fs) != 1 {
		t.Errorf("not placed: %+v, want one", fs)
	}
}
