package ledger

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
)

// impactRun records a run of [from, from+1h] that read the impact signal
// with crit, finding windows.
func impactRun(t *testing.T, l *Ledger, from time.Time, crit string, windows ...incident.Incident) {
	t.Helper()
	f := Found{From: from, To: from.Add(time.Hour), ImpactOn: crit != "", ImpactSince: from.Add(-time.Hour), ImpactCriterion: crit, Impact: windows,
		Run: Run{Kind: "test", Started: from, Finished: from, From: from, To: from.Add(time.Hour)}}
	if crit == "" {
		f.ImpactSince = time.Time{}
	}
	if err := l.Record(context.Background(), f, opts); err != nil {
		t.Fatal(err)
	}
}

func impactWindow(id string, start time.Time, d time.Duration) incident.Incident {
	return incident.Incident{ID: id, Title: "user impact (--impact)", Start: start, End: start.Add(d), Candidate: true}
}

// A run that reads another criterion than the runs before keeps the record
// as it was, every week since read with the criterion in effect then, and
// marks the weeks before as waiting to be read again with it; a window on
// at its start is its own, not the one before's going on. Turned off, the
// record ends; turned on again, the weeks before wait to be read again.
func TestImpactCriterionChanged(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	impactRun(t, l, t0, "5m a > 1", impactWindow("w", t0.Add(30*time.Minute), 30*time.Minute))
	impactRun(t, l, t0.Add(time.Hour), "5m a > 1")
	k, err := l.ImpactKept(ctx)
	if err != nil || !k.Since.Equal(t0.Add(-time.Hour)) || k.Criterion != "5m a > 1" || k.Again != "" {
		t.Fatalf("read with one: %+v %v", k, err)
	}
	// Read with another from 02:00, which finds a window on since 01:50.
	at := t0.Add(2 * time.Hour)
	impactRun(t, l, at, "5m b > 1", impactWindow("w", at.Add(-10*time.Minute), 20*time.Minute))
	k, err = l.ImpactKept(ctx)
	if err != nil || !k.Since.Equal(t0.Add(-time.Hour)) || k.Criterion != "5m b > 1" || k.Again != againDue {
		t.Errorf("read with another: %+v %v", k, err)
	}
	ws, err := l.ImpactWindows(ctx, t0, at.Add(time.Hour))
	if err != nil || len(ws) != 2 || !ws[0].End.Equal(t0.Add(time.Hour)) || !ws[1].Start.Equal(at.Add(-10*time.Minute)) {
		t.Errorf("the windows: %+v %v", ws, err)
	}
	// One not read ends it, the criterion forgotten.
	impactRun(t, l, at.Add(time.Hour), "")
	if k, err = l.ImpactKept(ctx); err != nil || !k.Since.IsZero() || k.Criterion != "" || k.Again != "" {
		t.Errorf("not read: %+v %v", k, err)
	}
	// Read again, from its own start, the weeks before it wait.
	impactRun(t, l, at.Add(2*time.Hour), "5m b > 1")
	if k, err = l.ImpactKept(ctx); err != nil || !k.Since.Equal(at.Add(time.Hour)) || k.Again != againDue {
		t.Errorf("turned on again: %+v %v", k, err)
	}
}

// Read with another criterion, a window found before that is on past the
// run's start ends there; one the new criterion finds starting in the same
// minute as one kept is kept too, by a key of its own.
func TestImpactChangeEndsTheOneBefore(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	from := t0.Add(time.Hour)
	f := Found{From: from, To: from.Add(2 * time.Hour), ImpactOn: true, ImpactSince: from.Add(-time.Hour), ImpactCriterion: "a",
		Impact: []incident.Incident{impactWindow("w", from.Add(50*time.Minute), 40*time.Minute)},
		Run:    Run{Kind: "test", Started: from, Finished: from, From: from, To: from.Add(2 * time.Hour)}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	// Replayed again from 02:00, with another criterion, which finds a
	// window from 01:50 too.
	again := from.Add(time.Hour)
	f = Found{From: again, To: again.Add(time.Hour), ImpactOn: true, ImpactSince: again.Add(-time.Hour), ImpactCriterion: "b",
		Impact: []incident.Incident{impactWindow("v", from.Add(50*time.Minute), 20*time.Minute)},
		Run:    Run{Kind: "test", Started: again, Finished: again, From: again, To: again.Add(time.Hour)}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	ws, err := l.ImpactWindows(ctx, t0, again.Add(time.Hour))
	if err != nil || len(ws) != 2 {
		t.Fatalf("%+v %v", ws, err)
	}
	if !ws[0].End.Equal(again) || ws[0].ID == ws[1].ID || !ws[1].End.Equal(from.Add(70*time.Minute)) {
		t.Errorf("the one before ends at %s; keys %s %s; the new one ends at %s", ws[0].End, ws[0].ID, ws[1].ID, ws[1].End)
	}
}

// Read again over [From, To] with a criterion, the windows found before
// from From on give way to those it found, keeping what was decided about
// them; one on at From ends there; the stones are given again; and the
// record is kept by it from From. Read past by the runs, it is refused.
func TestReadAgain(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	week := 7 * 24 * time.Hour
	start := t0.Add(-4 * week)
	// The runs read "old" from four weeks before: a window before the
	// read again, one on at its start, one after it, dismissed.
	f := Found{From: start, To: t0, ImpactOn: true, ImpactSince: start, ImpactCriterion: "old",
		Impact: []incident.Incident{impactWindow("a", start.Add(time.Hour), time.Hour), impactWindow("b", start.Add(2*week-time.Hour), 3*time.Hour),
			impactWindow("c", start.Add(3*week), time.Hour)},
		Firings: []firing.Firing{{Rule: "R", Start: start.Add(3*week + 130*time.Minute), End: start.Add(3*week + 140*time.Minute)}},
		Run:     Run{Kind: "test", Started: t0, Finished: t0, From: start, To: t0}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Decide(ctx, "impact-20261026T0000Z", "dismissed", "", "ana", opts); err != nil {
		t.Fatal(err)
	}
	from := start.Add(2 * week)
	r := ImpactRead{From: from, To: t0, Criterion: "new", Windows: []incident.Incident{
		impactWindow("x", start.Add(3*week+5*time.Minute), time.Hour), // over c: dismissed too
		impactWindow("y", start.Add(3*week+2*time.Hour), time.Hour),
	}}
	if err := l.ReadAgain(ctx, ImpactRead{From: from, To: t0.Add(-time.Hour), Criterion: "new"}, opts); !errors.Is(err, ErrRunsMoved) {
		t.Errorf("read to before the cursor: %v", err)
	}
	if err := l.ReadAgain(ctx, r, opts); err != nil {
		t.Fatal(err)
	}
	ws, err := l.ImpactWindows(ctx, start, t0)
	if err != nil {
		t.Fatal(err)
	}
	type got struct {
		start, end time.Time
		state      string
	}
	want := []got{{start.Add(time.Hour), start.Add(2 * time.Hour), ""}, {start.Add(2*week - time.Hour), from, ""},
		{start.Add(3*week + 5*time.Minute), start.Add(3*week + 65*time.Minute), "dismissed"}, {start.Add(3*week + 2*time.Hour), start.Add(3*week + 3*time.Hour), ""}}
	if len(ws) != len(want) {
		t.Fatalf("%d windows, not %d: %+v", len(ws), len(want), ws)
	}
	for i, w := range ws {
		if g := (got{w.Start, w.End, w.State}); g != want[i] {
			t.Errorf("window %d: %+v, not %+v", i, g, want[i])
		}
	}
	k, err := l.ImpactKept(ctx)
	if err != nil || !k.Since.Equal(from) || k.Criterion != "new" || !k.AgainDone("new") {
		t.Errorf("kept %+v %v", k, err)
	}
	// The firing in y caught nothing before, and catches y now: judged
	// again, as the span was pended.
	if err := l.JudgePending(ctx, opts); err != nil {
		t.Fatal(err)
	}
	fs, err := l.Firings(ctx, FiringQuery{From: start, To: t0})
	if err != nil || len(fs) != 1 || !fs[0].White {
		t.Errorf("the firing: %+v %v", fs, err)
	}
}

// When each source last wrote through the incidents port is kept, the
// latest of each, MaxReceived sources at most: past them, a source it
// doesn't know is kept among the others, so a flood of ids of no source
// doesn't push out those it knows.
func TestNoteReceived(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	for i := range MaxReceived + 2 {
		if err := l.NoteReceived(ctx, fmt.Sprintf("S%02d", i), t0.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.NoteReceived(ctx, "S10", t0.Add(-time.Hour)); err != nil { // older than it has: kept as it was
		t.Fatal(err)
	}
	if err := l.NoteReceived(ctx, "S00", t0.Add(2*time.Hour)); err != nil { // known: kept apart
		t.Fatal(err)
	}
	got, err := l.Received(ctx)
	if err != nil || len(got) != MaxReceived {
		t.Fatalf("%d kept: %v", len(got), err)
	}
	if !got["S00"].Equal(t0.Add(2*time.Hour)) || !got["S10"].Equal(t0.Add(10*time.Minute)) {
		t.Errorf("S00 %s, S10 %s", got["S00"], got["S10"])
	}
	if _, ok := got[fmt.Sprintf("S%02d", MaxReceived)]; ok || !got[OtherSources].Equal(t0.Add(time.Duration(MaxReceived+1)*time.Minute)) {
		t.Errorf("past the most: %v", got)
	}
}

// What was decided about a window found before holds for one found now
// that it covers half of or more, a window on from before the span among
// them; not for one it covers less of; of two, the one that covers most of
// it, confirmed before dismissed on a tie.
func TestImpactDecisions(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		old   []incident.Incident
		state map[string]string // of the old, by id
		from  time.Duration     // from t0, where the run reads again
		found incident.Incident
		want  string
	}{
		{"on from before the span", []incident.Incident{impactWindow("a", t0.Add(30*time.Minute), 2*time.Hour)}, map[string]string{"a": "dismissed"},
			time.Hour, impactWindow("x", t0.Add(70*time.Minute), 40*time.Minute), "dismissed"},
		{"a short one under a long one", []incident.Incident{impactWindow("a", t0.Add(80*time.Minute), 5*time.Minute)}, map[string]string{"a": "dismissed"},
			time.Hour, impactWindow("x", t0.Add(70*time.Minute), 3*time.Hour), ""},
		{"the one that covers most", []incident.Incident{impactWindow("a", t0.Add(70*time.Minute), 30*time.Minute), impactWindow("b", t0.Add(100*time.Minute), 60*time.Minute)},
			map[string]string{"a": "confirmed", "b": "dismissed"}, time.Hour, impactWindow("x", t0.Add(70*time.Minute), 80*time.Minute), "dismissed"},
		{"confirmed on a tie", []incident.Incident{impactWindow("a", t0.Add(70*time.Minute), 30*time.Minute), impactWindow("b", t0.Add(100*time.Minute), 30*time.Minute)},
			map[string]string{"a": "dismissed", "b": "confirmed"}, time.Hour, impactWindow("x", t0.Add(70*time.Minute), 60*time.Minute), "confirmed"},
	} {
		l := open(t, time.UTC)
		f := Found{From: t0, To: t0.Add(4 * time.Hour), ImpactOn: true, ImpactSince: t0, ImpactCriterion: "a", Impact: c.old,
			Run: Run{Kind: "test", Started: t0, Finished: t0, From: t0, To: t0.Add(4 * time.Hour)}}
		if err := l.Record(ctx, f, opts); err != nil {
			t.Fatal(err)
		}
		for _, in := range c.old {
			if st := c.state[in.ID]; st != "" {
				if _, err := l.Decide(ctx, "impact-"+in.Start.Format("20060102T1504Z"), st, "", "ana", opts); err != nil {
					t.Fatal(err)
				}
			}
		}
		from := t0.Add(c.from)
		if err := l.write(ctx, func(tx *txn) error {
			_, err := l.impact(tx, Found{From: from, ImpactSince: from, Impact: []incident.Incident{c.found}}, true)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		got, err := l.IncidentByID(ctx, "impact-"+c.found.Start.Format("20060102T1504Z"))
		if err != nil || got.State != c.want {
			t.Errorf("%s: %q, not %q (%v)", c.name, got.State, c.want, err)
		}
	}
}

// The tasks and comments on a window found before move to the window found
// now that covers most of it; those on one nothing covers now stay on its
// key, which the Cleanup page lists.
func TestImpactTasksMove(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	key := func(at time.Time) string { return "impact-" + at.Format("20060102T1504Z") }
	a, b := t0.Add(70*time.Minute), t0.Add(3*time.Hour)
	f := Found{From: t0, To: t0.Add(4 * time.Hour), ImpactOn: true, ImpactSince: t0, ImpactCriterion: "a",
		Impact: []incident.Incident{impactWindow("a", a, 30*time.Minute), impactWindow("b", b, 10*time.Minute)},
		Run:    Run{Kind: "test", Started: t0, Finished: t0, From: t0, To: t0.Add(4 * time.Hour)}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	var tasks []int64
	for _, k := range []string{key(a), key(b)} {
		id, err := l.PutTask(ctx, Task{Title: "look", Kind: "decide", Incident: k}, "ana")
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, id)
		if _, err := l.PutComment(ctx, Comment{On: "incident", Key: k, Body: "seen"}, "ana"); err != nil {
			t.Fatal(err)
		}
	}
	// Read again: a window from five minutes earlier over a, none over b.
	moved := a.Add(-5 * time.Minute)
	if err := l.write(ctx, func(tx *txn) error {
		_, err := l.impact(tx, Found{From: t0.Add(time.Hour), ImpactSince: t0.Add(time.Hour), Impact: []incident.Incident{impactWindow("x", moved, 30*time.Minute)}}, true)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	task, err := l.TaskByID(ctx, tasks[0])
	if err != nil || task.Incident != key(moved) {
		t.Errorf("a's task: %+v %v", task, err)
	}
	if cs, err := l.Comments(ctx, "incident", key(moved)); err != nil || len(cs) != 1 {
		t.Errorf("a's comments: %+v %v", cs, err)
	}
	orphans, err := l.OrphanedImpact(ctx)
	if err != nil || len(orphans) != 1 || orphans[0].Key != key(b) || len(orphans[0].Tasks) != 1 || orphans[0].Tasks[0] != tasks[1] || orphans[0].Comments != 1 {
		t.Errorf("orphans: %+v %v", orphans, err)
	}
}

// Read again, the stones of the span are given again a week at a time,
// each in a write of its own, not all of it in one.
func TestReadAgainJudgedByWeek(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	start := t0.Add(-5 * 7 * 24 * time.Hour)
	record(t, l, start, t0, []firing.Firing{{Rule: "R", Start: start.Add(time.Hour), End: start.Add(2 * time.Hour)}})
	var spans [][2]int64
	judging = func(lo, hi int64) { spans = append(spans, [2]int64{lo, hi}) }
	defer func() { judging = nil }()
	if err := l.ReadAgain(ctx, ImpactRead{From: start, To: t0, Criterion: "c"}, opts); err != nil {
		t.Fatal(err)
	}
	if err := l.JudgePending(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if len(spans) < 5 {
		t.Fatalf("judged in %d spans: %v", len(spans), spans)
	}
	for _, s := range spans {
		if s[1]-s[0] >= 7*86400 {
			t.Errorf("a span of %s judged at once", time.Duration(s[1]-s[0])*time.Second)
		}
	}
}

// Where the data of the criterion's selectors begins moves the record's
// start forward to the first whole week after it, never back; with a
// selector that has none at all, the signal keeps no record.
func TestImpactData(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	start := t0.Add(-3 * 7 * 24 * time.Hour) // a Monday, three weeks before
	impactRun(t, l, start, "c")
	first := start.Add(7*24*time.Hour + 30*time.Hour) // the Tuesday of the week after
	d := ImpactData{Criterion: "c", From: start, CheckedTo: t0, Selectors: []SelectorData{{Sel: "a", First: start, AtStart: true}, {Sel: "b", First: first}}}
	if err := l.SetImpactData(ctx, d); err != nil {
		t.Fatal(err)
	}
	k, err := l.ImpactKept(ctx)
	if want := start.Add(14 * 24 * time.Hour); err != nil || !k.Since.Equal(want) || !k.From().Equal(want) || k.Data.Late() != "b" {
		t.Fatalf("moved to %s, from %s, late %q, not %s (%v)", k.Since, k.From(), k.Data.Late(), want, err)
	}
	// Found earlier after all: kept where it is.
	d.Selectors[1].First = start.Add(time.Hour)
	if err := l.SetImpactData(ctx, d); err != nil {
		t.Fatal(err)
	}
	if k2, _ := l.ImpactKept(ctx); !k2.Since.Equal(k.Since) {
		t.Errorf("moved back to %s", k2.Since)
	}
	// A selector with no data: no record.
	d.Selectors[1].First = time.Time{}
	if err := l.SetImpactData(ctx, d); err != nil {
		t.Fatal(err)
	}
	if k, _ := l.ImpactKept(ctx); !k.From().IsZero() || !k.Data.NoData {
		t.Errorf("no data: from %s", k.From())
	}
	// Of another criterion than the one read: nothing kept.
	if err := l.SetImpactData(ctx, ImpactData{Criterion: "other", Selectors: []SelectorData{{Sel: "x"}}}); err != nil {
		t.Fatal(err)
	}
	if k, _ := l.ImpactKept(ctx); k.Data.Criterion != "c" {
		t.Errorf("kept another's: %+v", k.Data)
	}
}

// The incidents since a time, by where they came from: the latest written
// in Lapilli, whenever it started; those copied in that started or changed
// since.
func TestIncidentSources(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	since := t0.Add(-28 * 24 * time.Hour)
	in := func(id string, at time.Time) incident.Incident {
		return incident.Incident{ID: id, Start: at, End: at.Add(time.Hour)}
	}
	for _, x := range []struct {
		in incident.Incident
		by string
	}{
		{in("L-1", since.Add(-time.Hour)), "ana"}, {in("L-2", t0), "ana"},
		{in("PD-1", t0), Copier}, {in("PD-2", since.Add(-24*time.Hour)), Copier},
	} {
		if _, err := l.Put(ctx, x.in, x.by, opts); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.SyncFile(ctx, []incident.Incident{in("F-1", t0), in("F-2", since.Add(-time.Hour))}, opts); err != nil {
		t.Fatal(err)
	}
	impactRun(t, l, t0, "c", impactWindow("w", t0.Add(10*time.Minute), 10*time.Minute))
	got, err := l.IncidentSources(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	// PD-2 started before, and was written now: changed since.
	if got.Written != 1 || got.LastWritten == nil || got.LastWritten.ID != "L-2" || got.File != 1 || got.Impact != 1 || len(got.Copied) != 2 {
		t.Errorf("%+v", got)
	}
}
