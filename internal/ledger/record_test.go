package ledger

import (
	"context"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
)

// A week with no incident on in it, or in the two weeks before it, has no
// record: its rules are counted, not judged, as verdict.Judge has it. One
// written down ten days before keeps it; so does one started twenty days
// before and still on; and an impact signal read over it.
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

	for name, in := range map[string]incident.Incident{
		"ten days before":              {ID: "INC-1", Start: t0.Add(-10 * 24 * time.Hour), End: t0.Add(-10*24*time.Hour + time.Hour)},
		"twenty days before, still on": {ID: "INC-1", Start: t0.Add(-20 * 24 * time.Hour)},
	} {
		if _, err := l.SyncFile(ctx, []incident.Incident{in}, opts); err != nil {
			t.Fatal(err)
		}
		if rep, err = l.Report(ctx, t0, week, opts, false); err != nil {
			t.Fatal(err)
		}
		if rep.NoRecord || rep.Unjudged != 0 || !rep.Rules[0].Noisy || rep.Rules[0].NoRecord {
			t.Errorf("%s: judged, and noisy: %v %+v", name, rep.NoRecord, rep.Rules)
		}
		if kept, err := l.RecordKept(ctx, t0, week, opts); err != nil || !kept {
			t.Errorf("%s: kept %v %v", name, kept, err)
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
}
