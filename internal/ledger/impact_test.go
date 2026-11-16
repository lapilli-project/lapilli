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
// from its own start alone, as the weeks before weren't read with it, and
// marks the weeks before as waiting to be read again with it; a window on
// at its start is its own, not the one before's going on.
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
	if err != nil || !k.Since.Equal(at.Add(-time.Hour)) || k.Criterion != "5m b > 1" || k.Again != againDue {
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
	// again.
	fs, err := l.Firings(ctx, FiringQuery{From: start, To: t0})
	if err != nil || len(fs) != 1 || !fs[0].White {
		t.Errorf("the firing: %+v %v", fs, err)
	}
}

// When each source last wrote through the incidents port is kept, the
// latest of each, MaxReceived sources at most, the oldest forgotten first.
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
	got, err := l.Received(ctx)
	if err != nil || len(got) != MaxReceived {
		t.Fatalf("%d kept: %v", len(got), err)
	}
	if _, ok := got["S01"]; ok {
		t.Errorf("the oldest kept: %v", got)
	}
	if at := got["S10"]; !at.Equal(t0.Add(10 * time.Minute)) {
		t.Errorf("S10: %s", at)
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
