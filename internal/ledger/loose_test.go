package ledger

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// The ledger keeps whether a catch was loose, and judges as verdict.Judge
// does, with --match-strict and without: a rule whose firings lack the
// service of an incident in checkout catches it, loose, and with
// --match-strict doesn't.
func TestLooseCatchKept(t *testing.T) {
	ctx := context.Background()
	at := func(m int) time.Time { return t0.Add(2*time.Hour + time.Duration(m)*time.Minute) }
	fs := []firing.Firing{
		{Rule: "DiskFull", Labels: map[string]string{"node": "n1"}, Start: at(1), End: at(20)},
		{Rule: "CheckoutLatency", Labels: map[string]string{"service": "checkout"}, Start: at(3), End: at(9)},
	}
	in := incident.Incident{ID: "INC-7", Start: at(0), End: at(40), Labels: incident.Labels{"service": {"checkout"}}}
	for _, strict := range []bool{false, true} {
		l := open(t, time.UTC)
		o := opts
		o.Match = []string{"service"}
		o.MatchStrict = strict
		incs := []incident.Incident{keepAll, in}
		if _, err := l.SyncFile(ctx, incs, o); err != nil {
			t.Fatal(err)
		}
		to := t0.Add(7 * 24 * time.Hour)
		if err := l.Record(ctx, Found{From: t0, To: to, Firings: fs, Run: Run{Kind: "test", Started: to, Finished: to, From: t0, To: to}}, o); err != nil {
			t.Fatal(err)
		}
		o.From, o.To = t0, to
		want := verdict.Judge(nil, slices.Clone(fs), incs, o)
		got, err := l.Report(ctx, o.From, o.To, o, true)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Incidents, want.Incidents) {
			t.Errorf("strict %v: incidents\n got %+v\nwant %+v", strict, got.Incidents, want.Incidents)
		}
		kept, err := l.IncidentByID(ctx, "INC-7")
		if err != nil {
			t.Fatal(err)
		}
		var loose []string
		for _, c := range kept.CaughtBy {
			if c.Loose {
				loose = append(loose, c.Rule)
			}
		}
		if n := len(kept.CaughtBy); strict && (n != 1 || len(loose) != 0) || !strict && (n != 2 || !slices.Equal(loose, []string{"DiskFull"})) {
			t.Errorf("strict %v: caught by %+v, want DiskFull, loose, and without --match-strict alone", strict, kept.CaughtBy)
		}
		for _, c := range kept.CaughtBy {
			if c.Loose != (len(c.Lacked) > 0) || c.Loose && !slices.Equal(c.Lacked, []string{"service"}) {
				t.Errorf("strict %v: %+v, want the loose one to have lacked service", strict, c)
			}
		}
	}
}
