package ledger

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/verdict"
)

// A named signal caught up after a miss a piece at a time, as lapilli
// serve reads it (a turn's pieces to E, the next turn on from PieceResume
// before E), leaves the ledger as one read over the whole gap at once:
// the same windows, keys, bounds and labels, the same firings tied to the
// same signals, the same stones and the same weeks. (A window found short
// in one piece and much longer in the next can take a key of its own,
// "-2", as at runs' boundaries: no case here is one.)
func TestPiecesEqualWhole(t *testing.T) {
	ctx := context.Background()
	T := t0.Add(6 * time.Hour) // where it was missed from
	minutes := func(service string, from, to time.Time) prom.Series {
		s := prom.Series{Labels: map[string]string{"service": service}}
		for at := from; !at.After(to); at = at.Add(time.Minute) {
			s.Samples = append(s.Samples, prom.Sample{T: at, V: 1})
		}
		return s
	}
	alert := func(service string, start time.Time) firing.Firing {
		return firing.Firing{Rule: "Errors", Labels: map[string]string{"service": service}, Start: start, End: start.Add(8 * time.Minute)}
	}
	E := T.Add(time.Hour) // where the first turn's pieces end, but for the last
	for _, c := range []struct {
		name    string
		for_    time.Duration
		settle  time.Duration
		series  []prom.Series
		firings []firing.Firing
		e       time.Time // where the first turn's pieces end, if not E
	}{
		{"a window not yet on for its for at the boundary, another ended in the hour before it", 5 * time.Minute, time.Hour,
			[]prom.Series{minutes("checkout", T.Add(10*time.Minute), T.Add(40*time.Minute)), minutes("cart", E.Add(-3*time.Minute), E.Add(20*time.Minute))},
			[]firing.Firing{alert("cart", E.Add(2*time.Minute)), alert("checkout", E.Add(5*time.Minute))}, time.Time{}},
		{"a window on for its for across the boundary", 5 * time.Minute, time.Hour,
			[]prom.Series{minutes("cart", E.Add(-30*time.Minute), E.Add(30*time.Minute))},
			[]firing.Firing{alert("cart", E.Add(10*time.Minute))}, time.Time{}},
		{"a window not yet on for its for, alone", 5 * time.Minute, time.Hour,
			[]prom.Series{minutes("cart", E.Add(-3*time.Minute), E.Add(20*time.Minute))},
			[]firing.Firing{alert("cart", E.Add(2*time.Minute))}, time.Time{}},
		{"the boundary on a firing's start", 5 * time.Minute, time.Hour,
			[]prom.Series{minutes("cart", E.Add(-20*time.Minute), E.Add(15*time.Minute))},
			[]firing.Firing{alert("cart", E), alert("checkout", E)}, time.Time{}},
		// A for longer than the hour a read looks before where it reads,
		// within --settle: only the overlap finds its start.
		{"a for longer than the hour before, within --settle, not yet on at the boundary", 90 * time.Minute, 2 * time.Hour,
			[]prom.Series{minutes("cart", E.Add(-80*time.Minute), E.Add(40*time.Minute))},
			[]firing.Firing{alert("cart", E.Add(15*time.Minute))}, time.Time{}},
		// Read whole, the two overlap and are one window, with both values;
		// in pieces, the one found before goes on with the other's too.
		{"a window on across the boundary, another series begun after it", 5 * time.Minute, time.Hour,
			[]prom.Series{minutes("checkout", E.Add(-30*time.Minute), E.Add(30*time.Minute)), minutes("cart", E.Add(5*time.Minute), E.Add(20*time.Minute))},
			// The first cart firing long before where the next turn reads,
			// in the window: judged again with its labels.
			[]firing.Firing{alert("cart", E.Add(-25*time.Minute)), alert("cart", E.Add(10*time.Minute)), alert("checkout", E.Add(12*time.Minute))}, time.Time{}},
		// The next turn reads on the step's grid, as a range is evaluated
		// a step at a time from its start: off it, the window would start
		// half a step late, past the lead of a firing before it.
		{"a for not a multiple of the step", 90 * time.Second, time.Hour,
			[]prom.Series{minutes("cart", E.Add(5*time.Minute), E.Add(20*time.Minute))},
			[]firing.Firing{alert("cart", E.Add(-10*time.Minute))}, time.Time{}},
		// A for longer than --settle: the overlap is --settle, and a window
		// not yet on at the boundary begins before where the next turn
		// reads, in the hour before it, with another ended there: put in as
		// new, not as the other going on.
		{"a for longer than --settle, not yet on at the boundary, another ended in the hour before", 90 * time.Minute, time.Hour,
			[]prom.Series{minutes("checkout", T.Add(40*time.Minute), T.Add(135*time.Minute)), minutes("cart", T.Add(160*time.Minute), T.Add(280*time.Minute))},
			[]firing.Firing{alert("cart", T.Add(255*time.Minute)), alert("checkout", T.Add(265*time.Minute))}, T.Add(4 * time.Hour)},
	} {
		t.Run(c.name, func(t *testing.T) {
			E := E
			if !c.e.IsZero() {
				E = c.e
			}
			end := E.Add(4 * time.Hour)
			// What a range over [lo, hi] answers: each series, while on, at
			// lo, lo+step, …, as Prometheus evaluates a range.
			slice := func(lo, hi time.Time) []prom.Series {
				var out []prom.Series
				for _, s := range c.series {
					on, off := s.Samples[0].T, s.Samples[len(s.Samples)-1].T
					x := prom.Series{Labels: s.Labels}
					for at := lo; !at.After(hi); at = at.Add(time.Minute) {
						if !at.Before(on) && !at.After(off) {
							x.Samples = append(x.Samples, prom.Sample{T: at, V: 1})
						}
					}
					if len(x.Samples) > 0 {
						out = append(out, x)
					}
				}
				return out
			}
			// As a read of the signal over [from, to] finds its windows:
			// from the hour before from.
			read := func(from, to time.Time) []incident.Incident {
				return incident.FromSignal(slice(from.Add(-time.Hour), to), time.Minute, c.for_, "errors", nil)
			}
			seen := seenOfValues([]string{"service"}, "checkout", "cart", "search")
			setup := func() *Ledger {
				l := open(t, time.UTC)
				defs := slices.Clone(signalDefs)
				defs[0].For = c.for_
				if _, err := l.SyncSignals(ctx, defs, nil, opts.Match); err != nil {
					t.Fatal(err)
				}
				// The run that missed it, with the firings.
				f := Found{From: T, To: E.Add(2 * time.Hour), Firings: c.firings, Run: Run{Kind: "test", Started: T, Finished: T, From: T, To: E.Add(2 * time.Hour)},
					Signals: []SignalRead{{Name: "errors", Criterion: "c", Since: T.Add(-time.Hour), From: T, Missed: true}}}
				if err := l.Record(ctx, f, opts); err != nil {
					t.Fatal(err)
				}
				return l
			}
			run := func(l *Ledger, from, to time.Time, r SignalRead) {
				t.Helper()
				r.Name, r.Criterion, r.Seen, r.Since = "errors", "c", seen, r.From.Add(-time.Hour)
				f := Found{From: from, To: to, Run: Run{Kind: "test", Started: from, Finished: to, From: from, To: to}, Signals: []SignalRead{r}}
				if err := l.Record(ctx, f, opts); err != nil {
					t.Fatal(err)
				}
			}
			r2, r3 := E.Add(2*time.Hour), E.Add(3*time.Hour)
			whole := setup()
			run(whole, r2, r3, SignalRead{From: T, Windows: read(T, r3)})
			run(whole, r3, end, SignalRead{From: r3, Windows: read(r3, end)})
			pieces := setup()
			resume := PieceResume(E, c.for_, c.settle, time.Minute)
			run(pieces, r2, r3, SignalRead{From: T, To: resume, Windows: read(T, E)})
			run(pieces, r3, end, SignalRead{From: resume, Windows: read(resume, end)})

			show := func(l *Ledger) string {
				var out []string
				ws, err := l.ImpactWindows(ctx, T.Add(-3*time.Hour), end)
				if err != nil {
					t.Fatal(err)
				}
				for _, w := range ws {
					out = append(out, fmt.Sprintf("window %s %s–%s %v", w.ID, w.Start.Format("15:04"), w.End.Format("15:04"), w.Labels))
				}
				rows, err := l.SignalRows(ctx)
				if err != nil {
					t.Fatal(err)
				}
				st, err := l.SeenOf(ctx, opts.Match)
				if err != nil {
					t.Fatal(err)
				}
				var list []verdict.Signal
				for _, d := range signalDefs {
					x := verdict.Signal{Name: d.Name, For: d.For, Match: d.Match, Record: d.Record, Bit: -1, Seen: st[d.Name].Seen}
					for _, r := range rows {
						if r.Name == d.Name {
							x.Bit, x.Start = r.Bit, r.Kept.From()
						}
					}
					list = append(list, x)
				}
				o := opts
				o.Signals = verdict.NewSignals(list, nil)
				rep, err := l.Report(ctx, T, end, o, true)
				if err != nil {
					t.Fatal(err)
				}
				for _, f := range rep.Firings {
					out = append(out, fmt.Sprintf("firing %s %s tied %v white %v %v no record %v", f.Labels["service"], f.Start.Format("15:04"), f.Signals, f.White, f.Incidents, f.NoRecord))
				}
				weeks, err := l.WeeksBy(ctx, 0, 1<<40)
				if err != nil {
					t.Fatal(err)
				}
				for _, w := range weeks {
					out = append(out, fmt.Sprintf("week %+v", w))
				}
				slices.Sort(out)
				return strings.Join(out, "\n")
			}
			if a, b := show(whole), show(pieces); a != b {
				t.Errorf("in pieces:\n%s\nread whole:\n%s", b, a)
			}
		})
	}
}
