package ledger

import (
	"context"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
)

// A week with no incident in it, or in the week before, has no record: its
// rules are counted, not judged, as verdict.Judge has it. One written down
// ten days before its end keeps it.
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
	if !rep.NoRecord || len(rep.Rules) != 1 || rep.Rules[0].Noisy || !rep.Rules[0].NoRecord {
		t.Fatalf("no incident: %v %+v", rep.NoRecord, rep.Rules)
	}
	if kept, err := l.RecordKept(ctx, t0, week); err != nil || kept {
		t.Errorf("kept %v %v", kept, err)
	}

	in := incident.Incident{ID: "INC-1", Start: week.Add(-10 * 24 * time.Hour), End: week.Add(-10*24*time.Hour + time.Hour)}
	if _, err := l.SyncFile(ctx, []incident.Incident{in}, opts); err != nil {
		t.Fatal(err)
	}
	if rep, err = l.Report(ctx, t0, week, opts, false); err != nil {
		t.Fatal(err)
	}
	if rep.NoRecord || !rep.Rules[0].Noisy || rep.Rules[0].NoRecord {
		t.Errorf("one ten days before its end: judged, and noisy: %v %+v", rep.NoRecord, rep.Rules)
	}
	starts, err := l.IncidentStarts(ctx, t0.Add(-14*24*time.Hour), week)
	if err != nil || len(starts) != 1 || !starts[0].Equal(in.Start) {
		t.Errorf("starts %v %v", starts, err)
	}
}
