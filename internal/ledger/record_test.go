package ledger

import (
	"context"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// A week with no incident on in it, or in the two weeks before it, has no
// record: its rules are counted, not judged, as verdict.Judge has it. One
// written down ten days before keeps it, and an impact signal read over it;
// one started twenty days before with no end doesn't, as it is taken to
// last as long as a firing catches it for (UnknownLen).
func TestReportNoRecord(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	week := t0.Add(7 * 24 * time.Hour)
	var fs []firing.Firing
	for i := range 12 {
		at := t0.Add(time.Duration(i) * 6 * time.Hour)
		fs = append(fs, firing.Firing{Rule: "Restarts", Labels: map[string]string{"service": "a"}, Start: at, End: at.Add(10 * time.Minute)})
	}
	record(t, l, t0, week, fs)
	rep, err := l.Report(ctx, t0, week, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.NoRecord || rep.Unjudged != 12 || len(rep.Rules) != 1 || rep.Rules[0].Noisy || !rep.Rules[0].NoRecord || rep.Rules[0].Unjudged != 12 {
		t.Fatalf("no incident: %v %+v", rep.NoRecord, rep.Rules)
	}
	if kept, err := l.RecordKept(ctx, t0, week, opts); err != nil || kept {
		t.Errorf("kept %v %v", kept, err)
	}
	// Read over it, the impact signal keeps it, found or not.
	o := opts
	o.ImpactFrom = t0.Add(-14 * 24 * time.Hour)
	if rep, err := l.Report(ctx, t0, week, o, false); err != nil || rep.NoRecord || !rep.Rules[0].Noisy {
		t.Errorf("impact read: %v %+v", err, rep)
	}

	for name, c := range map[string]struct {
		in   incident.Incident
		kept bool
	}{
		"twenty days before, with no end":   {incident.Incident{ID: "INC-1", Start: t0.Add(-20 * 24 * time.Hour)}, false},
		"fourteen days before, with no end": {incident.Incident{ID: "INC-1", Start: t0.Add(-14*24*time.Hour - 30*time.Minute)}, true},
		"ten days before":                   {incident.Incident{ID: "INC-1", Start: t0.Add(-10 * 24 * time.Hour), End: t0.Add(-10*24*time.Hour + time.Hour)}, true},
	} {
		if _, err := l.SyncFile(ctx, []incident.Incident{c.in}, opts); err != nil {
			t.Fatal(err)
		}
		if rep, err = l.Report(ctx, t0, week, opts, false); err != nil {
			t.Fatal(err)
		}
		if judged := !rep.NoRecord && rep.Unjudged == 0 && rep.Rules[0].Noisy && !rep.Rules[0].NoRecord; judged != c.kept {
			t.Errorf("%s: judged %v: %v %+v", name, judged, rep.NoRecord, rep.Rules)
		}
		if kept, err := l.RecordKept(ctx, t0, week, opts); err != nil || kept != c.kept {
			t.Errorf("%s: kept %v %v", name, kept, err)
		}
		if incs, err := l.RecordIncidents(ctx, verdict.RecordFrom(t0, week), week, opts.UnknownLen); err != nil || (len(incs) == 1) != c.kept {
			t.Errorf("%s: read %+v %v", name, incs, err)
		}
	}
	// What keeps it is a team's only when keeps says so.
	if rep, err = l.ReportFor(ctx, t0, week, opts, false, func(Incident) bool { return false }); err != nil || !rep.NoRecord {
		t.Errorf("kept by none: %v %+v", err, rep)
	}
}

// Over two weeks, an incident in the second judges the second's firings,
// not the first's: each week by its own record, the firings all counted.
func TestReportJudgedByWeek(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	var fs []firing.Firing
	for d := range 14 {
		at := t0.Add(time.Duration(d)*24*time.Hour + 12*time.Hour)
		fs = append(fs, firing.Firing{Rule: "Daily", Labels: map[string]string{"service": "a"}, Start: at, End: at.Add(time.Minute)})
	}
	two := t0.Add(14 * 24 * time.Hour)
	record(t, l, t0, two, fs)
	in := incident.Incident{ID: "INC-1", Start: t0.Add(8 * 24 * time.Hour), End: t0.Add(8*24*time.Hour + time.Hour), Labels: incident.Labels{"service": {"b"}}}
	if _, err := l.SyncFile(ctx, []incident.Incident{in}, opts); err != nil {
		t.Fatal(err)
	}
	rep, err := l.Report(ctx, t0, two, opts, true)
	if err != nil {
		t.Fatal(err)
	}
	r := rep.Rules[0]
	if rep.NoRecord || r.Firings != 14 || r.Unjudged != 7 || r.Judged() != 7 || r.Noisy {
		t.Errorf("%+v", r)
	}
	for _, j := range rep.Firings {
		if want := j.Start.Before(t0.Add(7 * 24 * time.Hour)); j.NoRecord != want {
			t.Errorf("%s: no record %v", j.Start, j.NoRecord)
		}
	}
}

// The impact signal's first read is kept, and an earlier one moves it back.
func TestImpactSince(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if at, err := l.ImpactSince(ctx); err != nil || !at.IsZero() {
		t.Fatalf("%s %v", at, err)
	}
	for _, from := range []time.Time{t0, t0.Add(-time.Hour), t0.Add(time.Hour)} {
		f := Found{From: from, To: from.Add(time.Hour), ImpactOn: true, ImpactSince: from, Run: Run{Kind: "test", Started: from, Finished: from, From: from, To: from.Add(time.Hour)}}
		if err := l.Record(ctx, f, opts); err != nil {
			t.Fatal(err)
		}
	}
	if at, err := l.ImpactSince(ctx); err != nil || !at.Equal(t0.Add(-time.Hour)) {
		t.Errorf("%s %v", at, err)
	}
	run := func(from time.Time, on bool, impact ...incident.Incident) {
		t.Helper()
		f := Found{From: from, To: from.Add(time.Hour), ImpactOn: on, Impact: impact, Run: Run{Kind: "test", Started: from, Finished: from, From: from, To: from.Add(time.Hour)}}
		if on {
			f.ImpactSince = from
		}
		if err := l.Record(ctx, f, opts); err != nil {
			t.Fatal(err)
		}
	}
	// A run that doesn't read it ends it: what it read before no longer
	// keeps a record, as it wasn't read all along since.
	run(t0.Add(2*time.Hour), false)
	if at, err := l.ImpactSince(ctx); err != nil || !at.IsZero() {
		t.Errorf("a run without it: %s %v", at, err)
	}
	// Read again, from that run on; its windows from before aren't taken.
	run(t0.Add(3*time.Hour), true)
	if at, err := l.ImpactSince(ctx); err != nil || !at.Equal(t0.Add(3*time.Hour)) {
		t.Errorf("read again: %s %v", at, err)
	}
}

// A ledger an earlier lapilli wrote, with impact windows and no
// impact_since: the first run that reads the signal keeps its own start,
// not the earliest window's, as the runs between may not have read it.
func TestImpactSinceUpgraded(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	first := t0.Add(-20 * 24 * time.Hour)
	f := Found{From: first, To: first.Add(time.Hour), ImpactOn: true, ImpactSince: first,
		Impact: []incident.Incident{{ID: "impact-1", Start: first.Add(10 * time.Minute), End: first.Add(40 * time.Minute)}},
		Run:    Run{Kind: "test", Started: first, Finished: first, From: first, To: first.Add(time.Hour)}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	// As an earlier lapilli left it: the windows, no impact_since.
	if _, err := l.w.ExecContext(ctx, "DELETE FROM meta WHERE key = 'impact_since'"); err != nil {
		t.Fatal(err)
	}
	f = Found{From: t0, To: t0.Add(time.Hour), ImpactOn: true, ImpactSince: t0, Run: Run{Kind: "test", Started: t0, Finished: t0, From: t0, To: t0.Add(time.Hour)}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	if at, err := l.ImpactSince(ctx); err != nil || !at.Equal(t0) {
		t.Errorf("%s %v", at, err)
	}
}

// A report over part of a week judges it by the whole week's record, as
// WeekKept does: an incident on October 20, two weeks and a bit before the
// week of November 2, keeps that week's, so the firings Thursday to Sunday
// are judged over [Nov 5, Nov 9) as over [Nov 2, Nov 9); and one on Nov 5
// keeps [Nov 2, Nov 4)'s, of the same week.
func TestReportPartOfAWeek(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	day := 24 * time.Hour
	mon := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	var fs []firing.Firing
	for i := range 12 {
		at := mon.Add(3*day + time.Duration(i)*7*time.Hour)
		fs = append(fs, firing.Firing{Rule: "Restarts", Labels: map[string]string{"service": "a"}, Start: at, End: at.Add(10 * time.Minute)})
	}
	record(t, l, mon.Add(-21*day), mon.Add(7*day), fs)
	oct20 := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	if _, err := l.SyncFile(ctx, []incident.Incident{{ID: "INC-1", Start: oct20, End: oct20.Add(time.Hour)}}, opts); err != nil {
		t.Fatal(err)
	}
	for _, from := range []time.Time{mon, mon.Add(3 * day)} {
		rep, err := l.Report(ctx, from, mon.Add(7*day), opts, false)
		if err != nil {
			t.Fatal(err)
		}
		if rep.NoRecord || rep.Unjudged != 0 || len(rep.Rules) != 1 || !rep.Rules[0].Noisy {
			t.Errorf("from %s: %v %d %+v", from.Format("Jan 2"), rep.NoRecord, rep.Unjudged, rep.Rules)
		}
		if kept, err := l.RecordKept(ctx, from, mon.Add(7*day), opts); err != nil || !kept {
			t.Errorf("from %s: kept %v %v", from.Format("Jan 2"), kept, err)
		}
	}
	rec, err := l.RecordOf(ctx, mon.Add(3*day), mon.Add(7*day), opts, nil)
	if err != nil || !l.WeekKept(rec, Week(l.day(mon.Unix()))) {
		t.Errorf("WeekKept: %v", err)
	}
	// Taken away, one on Thursday keeps Monday to Wednesday's too.
	if _, err := l.SyncFile(ctx, []incident.Incident{{ID: "INC-2", Start: mon.Add(3 * day), End: mon.Add(3*day + time.Hour), Labels: incident.Labels{"service": {"b"}}}}, opts); err != nil {
		t.Fatal(err)
	}
	if kept, err := l.RecordKept(ctx, mon, mon.Add(2*day), opts); err != nil || !kept {
		t.Errorf("[Nov 2, Nov 4): kept %v %v", kept, err)
	}
	// With no incident in the weeks of the span, none kept: no record.
	if _, err := l.SyncFile(ctx, nil, opts); err != nil {
		t.Fatal(err)
	}
	rep, err := l.Report(ctx, mon.Add(3*day), mon.Add(7*day), opts, false)
	if err != nil || !rep.NoRecord || rep.Unjudged != 12 {
		t.Errorf("none: %v %+v", err, rep)
	}
}
