package verdict

import (
	"context"
	"sort"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/rules"
)

// SpanCheck is a replay beside what really fired over a span, as `lapilli
// serve` keeps it: each firing is counted in the span its start is in, and
// an alike pair in the span of the earlier of the two, so spans side by side
// count each once.
type SpanCheck struct {
	// Rules are the alerts checked, by name; one that fired neither way is
	// there with nothing counted.
	Rules map[string]*SpanRule
	// Elsewhere are alerts that really fired that no rule given is named,
	// with how many times.
	Elsewhere map[string]int
}

// SpanRule is one alert's replay beside what really fired, over a span.
type SpanRule struct {
	Replayed, Real, Alike int
	// OffSum is how far apart the alike ones started, all told.
	OffSum time.Duration
	// Misses are the firings found only one way.
	Misses []Miss
}

// Miss is a firing found only in the replay or only in what really fired.
type Miss struct {
	Way string // "replayed" or "real"
	firing.Firing
}

// Window is how far apart two firings of rules may start and still be
// alike: two evaluations, as CheckReplay has it. A span's firings are read
// this far either side of it.
func Window(known []rules.Rule, step time.Duration) time.Duration {
	w := 2 * step
	for _, r := range known {
		w = max(w, 2*max(step, r.Interval))
	}
	return w
}

// CheckSpan sets the firings replayed beside those that really fired, over
// [from, to): both as read from from less Window to to plus it, so that no
// firing in the span is too near an end to match. Rules that couldn't be
// replayed aren't checked. It reports false if ctx was done first.
func CheckSpan(ctx context.Context, known []rules.Rule, replayed, real []firing.Firing, from, to time.Time, step time.Duration) (*SpanCheck, bool) {
	c := &SpanCheck{Rules: map[string]*SpanRule{}, Elsewhere: map[string]int{}}
	type alert struct {
		within      time.Duration
		unsupported bool
		replayed    []firing.Firing
		real        []firing.Firing
	}
	alerts := map[string]*alert{}
	for _, r := range known {
		a := alerts[r.Name]
		if a == nil {
			a = &alert{}
			alerts[r.Name] = a
		}
		a.within = max(a.within, 2*max(step, r.Interval))
		a.unsupported = a.unsupported || r.Unsupported != ""
	}
	in := func(t time.Time) bool { return !t.Before(from) && t.Before(to) }
	near := func(f firing.Firing, w time.Duration) bool {
		return !f.Start.Before(from.Add(-w)) && f.Start.Before(to.Add(w))
	}
	for _, f := range replayed {
		if a := alerts[f.Rule]; a != nil && near(f, a.within) {
			a.replayed = append(a.replayed, f)
		}
	}
	for _, f := range real {
		switch a := alerts[f.Rule]; {
		case a == nil:
			if in(f.Start) {
				c.Elsewhere[f.Rule]++
			}
		case near(f, a.within):
			a.real = append(a.real, f)
		}
	}
	for name, a := range alerts {
		if ctx.Err() != nil {
			return c, false
		}
		if a.unsupported {
			continue
		}
		r := &SpanRule{}
		c.Rules[name] = r
		firing.Sort(a.replayed)
		firing.Sort(a.real)
		used := make([]bool, len(a.real))
		for _, f := range a.replayed {
			i := sort.Search(len(a.real), func(i int) bool { return !a.real[i].Start.Before(f.Start.Add(-a.within)) })
			matched := -1
			for ; i < len(a.real) && !a.real[i].Start.After(f.Start.Add(a.within)); i++ {
				if !used[i] && labelsAgree(f.Labels, a.real[i].Labels) {
					matched = i
					break
				}
			}
			if matched < 0 {
				if in(f.Start) {
					r.Replayed++
					r.Misses = append(r.Misses, Miss{"replayed", f})
				}
				continue
			}
			used[matched] = true
			x := a.real[matched]
			if in(earlier(f.Start, x.Start)) {
				r.Replayed++
				r.Real++
				r.Alike++
				r.OffSum += x.Start.Sub(f.Start).Abs()
			}
		}
		for i, x := range a.real {
			if !used[i] && in(x.Start) {
				r.Real++
				r.Misses = append(r.Misses, Miss{"real", x})
			}
		}
	}
	return c, true
}

func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
