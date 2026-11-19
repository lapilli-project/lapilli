package ledger

import (
	"context"
	"fmt"
	"reflect"
	"runtime"
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
	// n series of services named prefix-000 on, on from from to to.
	services := func(prefix string, n int, from, to time.Time) []prom.Series {
		var out []prom.Series
		for i := range n {
			out = append(out, minutes(fmt.Sprintf("%s-%03d", prefix, i), from, to))
		}
		return out
	}
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
		// 40 services long before the boundary, 40 more after it, one on
		// from the first to the last: one window of 81 values of service,
		// which it leaves out read whole; in pieces, the next turn's read
		// (from the hour before where it reads) doesn't see the first 40,
		// and the window it finds, of 41, goes on from the one found
		// before, of 41: 81 when they are one, and left out.
		{"a window across the boundary of more than 64 values of a label, in two pieces", 5 * time.Minute, time.Hour,
			append(append(services("a", 40, E.Add(-100*time.Minute), E.Add(-70*time.Minute)), minutes("bridge", E.Add(-72*time.Minute), E.Add(10*time.Minute))),
				services("b", 40, E.Add(5*time.Minute), E.Add(20*time.Minute))...),
			[]firing.Firing{alert("a-000", E.Add(-95*time.Minute)), alert("b-039", E.Add(10*time.Minute))}, time.Time{}},
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
				ws, err := incident.FromSignal(context.Background(), slice(from.Add(-time.Hour), to), time.Minute, c.for_, "errors", nil)
				if err != nil {
					t.Fatal(err)
				}
				return ws
			}
			seen := seenOfValues([]string{"service"}, "checkout", "cart", "search")
			setup := func() *Ledger {
				l := open(t, time.UTC)
				defs := slices.Clone(signalDefs)
				defs[0].For = c.for_
				if _, err := l.SyncSignals(ctx, defs, nil, opts.Match, opts.Lead); err != nil {
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
			// What a window going on with other labels pended, judged again
			// a week a write, as serve does after each round.
			for _, l := range []*Ledger{whole, pieces} {
				if err := l.JudgePending(ctx, opts); err != nil {
					t.Fatal(err)
				}
			}

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

// longOn records, for signal errors, one window from t0 with labels and a
// cart firing two days in, its day tallied, the window then on ten days
// as many runs left it; it returns the ledger, the firing's day and when
// the window's ten days end.
func longOn(t *testing.T, labels incident.Labels) (*Ledger, int64, time.Time) {
	t.Helper()
	ctx := context.Background()
	l := open(t, time.UTC)
	if _, err := l.SyncSignals(ctx, signalDefs, nil, opts.Match, opts.Lead); err != nil {
		t.Fatal(err)
	}
	win := incident.Incident{ID: "impact-errors-1", Start: t0, End: t0.Add(time.Hour), Signal: "errors", Candidate: true, Labels: labels}
	run := func(from, to time.Time, fs []firing.Firing, ws []incident.Incident) {
		t.Helper()
		f := Found{From: from, To: to, Firings: fs, Run: Run{Kind: "test", Started: from, Finished: to, From: from, To: to},
			Signals: []SignalRead{{Name: "errors", Criterion: "c", Since: from.Add(-time.Hour), From: from, Windows: ws, Seen: seenOfValues([]string{"service"}, "cart", "checkout")}}}
		if err := l.Record(ctx, f, opts); err != nil {
			t.Fatal(err)
		}
	}
	run(t0, t0.Add(time.Hour), nil, []incident.Incident{win})
	at := t0.Add(48 * time.Hour)
	// On since, as the runs between left it.
	if _, err := l.w.ExecContext(ctx, "UPDATE incidents SET end_at = ? WHERE origin = 'impact' AND signal = 'errors'", at.Unix()); err != nil {
		t.Fatal(err)
	}
	on := win
	on.Start, on.End = at.Add(-time.Hour), at.Add(time.Hour)
	run(at, at.Add(time.Hour), []firing.Firing{{Rule: "Errors", Labels: map[string]string{"service": "cart"}, Start: at.Add(time.Minute), End: at.Add(5 * time.Minute)}},
		[]incident.Incident{on})
	until := t0.Add(10 * 24 * time.Hour)
	if _, err := l.w.ExecContext(ctx, "UPDATE incidents SET end_at = ? WHERE origin = 'impact' AND signal = 'errors'", until.Unix()); err != nil {
		t.Fatal(err)
	}
	return l, l.Day(at), until
}

// hourly is the run of the hour from from, the window found again from the
// hour before with labels.
func hourly(t *testing.T, l *Ledger, from time.Time, labels incident.Labels) {
	t.Helper()
	w := incident.Incident{ID: "impact-errors-1", Start: from.Add(-time.Hour), End: from.Add(time.Hour), Signal: "errors", Candidate: true, Labels: labels}
	f := Found{From: from, To: from.Add(time.Hour), Run: Run{Kind: "test", Started: from, Finished: from, From: from, To: from.Add(time.Hour)},
		Signals: []SignalRead{{Name: "errors", Criterion: "c", Since: from.Add(-time.Hour), From: from, Windows: []incident.Incident{w}, Seen: seenOfValues([]string{"service"}, "cart", "checkout")}}}
	if err := l.Record(context.Background(), f, opts); err != nil {
		t.Fatal(err)
	}
}

// dayAndPending are a day's firings tallied, and what is pending.
func dayAndPending(t *testing.T, l *Ledger, day int64) (int, string) {
	t.Helper()
	var n int
	var pending string
	if err := l.r.QueryRowContext(context.Background(), "SELECT coalesce(sum(firings), 0) FROM days WHERE day = ?", day).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if err := l.r.QueryRowContext(context.Background(), "SELECT coalesce((SELECT value FROM meta WHERE key = 'pending'), '')").Scan(&pending); err != nil {
		t.Fatal(err)
	}
	return n, pending
}

// A named signal's window on for days, found again by each hourly run
// with its labels the same: only its end later, which changes no stone
// before the run's from. A run judges from its from, touching no day
// before it, and pends nothing.
func TestWindowGoingOnJudgedFromFrom(t *testing.T) {
	l, day, until := longOn(t, incident.Labels{"service": {"cart"}})
	for i := range 2 {
		if _, err := l.w.ExecContext(context.Background(), "UPDATE days SET firings = 999 WHERE day = ?", day); err != nil {
			t.Fatal(err)
		}
		hourly(t, l, until.Add(time.Duration(i)*time.Hour), incident.Labels{"service": {"cart"}})
		if n, pending := dayAndPending(t, l, day); n != 999 || pending != "" && pending != "[]" && pending != "null" {
			t.Errorf("hourly run %d: the day eight days back judged again (%d firings, 999 untouched), pending %q", i+1, n, pending)
		}
	}
}

// A named signal's window on for days that goes on with other labels
// (another series it overlaps): its firings before the run's from are
// pended, not judged in the run's write, and, judged again a week a write,
// their stones are as the window read whole with both values leaves them.
func TestWindowGoingOnWithOtherLabelsPended(t *testing.T) {
	ctx := context.Background()
	l, day, until := longOn(t, incident.Labels{"service": {"checkout"}})
	if _, err := l.w.ExecContext(ctx, "UPDATE days SET firings = 999 WHERE day = ?", day); err != nil {
		t.Fatal(err)
	}
	hourly(t, l, until, incident.Labels{"service": {"cart", "checkout"}})
	if n, pending := dayAndPending(t, l, day); n != 999 || pending == "" || pending == "[]" {
		t.Fatalf("the day eight days back: %d firings (999 untouched, as not judged in the run's write), pending %q", n, pending)
	}
	if err := l.JudgePending(ctx, opts); err != nil {
		t.Fatal(err)
	}
	whole, _, _ := longOn(t, incident.Labels{"service": {"cart", "checkout"}})
	hourly(t, whole, until, incident.Labels{"service": {"cart", "checkout"}})
	stones := func(l *Ledger) string {
		rep, err := l.Report(ctx, t0, until.Add(time.Hour), opts, true)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, f := range rep.Firings {
			out = append(out, fmt.Sprintf("%s %s white %v %v", f.Labels["service"], f.Start.Format("01-02 15:04"), f.White, f.Incidents))
		}
		weeks, err := l.WeeksBy(ctx, 0, 1<<40)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(out, "; ") + fmt.Sprint(weeks)
	}
	if a, b := stones(whole), stones(l); a != b || !strings.Contains(a, "white true") {
		t.Errorf("judged again:\n%s\nread whole:\n%s", b, a)
	}
}

// A named signal's window with more than 64 values of a label leaves the
// label out whether its values came at once or a piece at a time, and keeps
// one of 64 alike: a label left out constrains nothing, so the window is
// caught by more firings, never by fewer.
func TestWindowValuesKeptAlike(t *testing.T) {
	for _, n := range []int{64, 100} {
		var all []string
		for i := range n {
			all = append(all, fmt.Sprintf("svc-%03d", n-1-i))
		}
		half := n / 2
		labels := func(vs []string) incident.Labels {
			return incident.Labels{"service": slices.Clone(vs), "job": {"api"}}
		}
		// The same values, each label's in order: within the bounds a
		// window's are kept in the order they came.
		sorted := func(l incident.Labels) incident.Labels {
			for k := range l {
				slices.Sort(l[k])
			}
			return l
		}
		whole := sorted(keptWindowLabels(labels(all)))
		pieces := sorted(keptWindowLabels(mergeLabels(keptWindowLabels(labels(all[:half])), keptWindowLabels(labels(all[half:])))))
		if !reflect.DeepEqual(whole, pieces) || !slices.Equal(whole["job"], []string{"api"}) {
			t.Errorf("%d values: at once %v\na piece at a time %v", n, whole, pieces)
		}
		if _, kept := whole["service"]; kept != (n <= 64) {
			t.Errorf("%d values: service kept %v", n, kept)
		}
	}
}

// A window's label of 100,000 values, each first in order as it comes (the
// most an insertion can move), is left out, each looked for among those
// kept until then: never all of them held, nor written out to be
// measured. All of them were, each looked for among all before it.
func TestWindowValuesKeptOfMany(t *testing.T) {
	const n = 100000
	vs := make([]string, n)
	for i := range vs {
		vs[i] = fmt.Sprintf("path-%06d", n-1-i)
	}
	l := incident.Labels{"path": vs, "service": {"checkout"}}
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	got := keptWindowLabels(l)
	runtime.ReadMemStats(&m1)
	if want := (incident.Labels{"service": {"checkout"}}); !reflect.DeepEqual(got, want) {
		t.Errorf("%v, want %v", got, want)
	}
	if a := m1.TotalAlloc - m0.TotalAlloc; a > 256<<10 {
		t.Errorf("%d KiB allocated for 64 values kept of %d", a>>10, n)
	}
}
