package ledger

import (
	"context"
	"reflect"
	"testing"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// ReportFirings are the report's firings, judged alike and in the same
// order, those a quiet window excused left out: the first so many, of
// some rules or all, and how many there are, whether cut or not.
func TestReportFirings(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	fs, incs := world(7, 400)
	if _, err := l.SyncFile(ctx, append([]incident.Incident{keepAll}, incs...), opts); err != nil {
		t.Fatal(err)
	}
	record(t, l, t0, t0.Add(7*24*time.Hour), fs)
	if _, err := l.PutWindow(ctx, Window{Title: "deploy", Start: t0.Add(30 * time.Hour), End: t0.Add(40 * time.Hour)}, "ana", opts); err != nil {
		t.Fatal(err)
	}
	from, to := t0, t0.Add(7*24*time.Hour)
	rep, err := l.Report(ctx, from, to, opts, true)
	if err != nil {
		t.Fatal(err)
	}
	all := rep.Firings
	excused, together := 0, 0
	for _, r := range rep.Rules {
		excused += r.Excused
	}
	for i := 1; i < len(all); i++ {
		if all[i].Start.Equal(all[i-1].Start) && all[i].Rule != all[i-1].Rule {
			together++
		}
	}
	if excused == 0 || together == 0 {
		t.Fatalf("the world has %d excused firings and %d that start with another rule's: the test shows less", excused, together)
	}
	some := func(rule string) bool { return rule == "Rule1" || rule == "Rule4" || rule == "Rule9" }
	var ofSome []verdict.Judged
	for _, j := range all {
		if some(j.Rule) {
			ofSome = append(ofSome, j)
		}
	}
	for _, c := range []struct {
		name string
		in   func(string) bool
		want []verdict.Judged
	}{{"every rule", nil, all}, {"some rules", some, ofSome}} {
		for _, most := range []int{0, 1, 7, len(c.want) - 1, len(c.want), len(c.want) + 1, 100_000} {
			got, total, err := l.ReportFirings(ctx, from, to, opts, nil, c.in, most)
			if err != nil {
				t.Fatal(err)
			}
			n := min(most, len(c.want))
			if !reflect.DeepEqual(got, c.want[:n:n]) && !(n == 0 && len(got) == 0) {
				t.Errorf("%s, %d at most: not the report's first %d\n got %+v\nwant %+v", c.name, most, n, got, c.want[:n])
			}
			if total != len(c.want) {
				t.Errorf("%s, %d at most: total %d, want %d", c.name, most, total, len(c.want))
			}
		}
	}
	// None in a span with none, and none of a rule with none.
	if got, total, err := l.ReportFirings(ctx, to, to.Add(time.Hour), opts, nil, nil, 10); err != nil || len(got) != 0 || got == nil || total != 0 {
		t.Errorf("an empty span: %v %d %v", got, total, err)
	}
	if got, total, err := l.ReportFirings(ctx, from, to, opts, nil, func(string) bool { return false }, 10); err != nil || len(got) != 0 || total != 0 {
		t.Errorf("no rule: %v %d %v", got, total, err)
	}
	// Judged by the record keeps says keeps it, as ReportFor is: a team
	// that owns no incident has none, and judges no firing that caught
	// nothing.
	keeps := func(Incident) bool { return false }
	team, err := l.ReportFor(ctx, from, to, opts, true, keeps)
	if err != nil {
		t.Fatal(err)
	}
	got, total, err := l.ReportFirings(ctx, from, to, opts, keeps, nil, 100_000)
	if err != nil || !reflect.DeepEqual(got, team.Firings) || total != len(team.Firings) {
		t.Errorf("by a team's record: %d of %d, %v", len(got), total, err)
	}
	if reflect.DeepEqual(got, all) {
		t.Error("a team's record judged as everyone's: the test shows less")
	}
}
