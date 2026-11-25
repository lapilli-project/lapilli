package serve

import (
	"context"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// With rules found noisy by what was done about their firings, every
// surface asks one test: Errors' 14 firings caught nothing, so by what they
// caught it is noisy, but a person said 10 of them needed action, so by what
// was done it is not, in the report, the tallies' weeks, and over weeks.
// There are no pages, changes or deploys: the Settings page's value alone.
func TestNoisyTestIsOneEverywhere(t *testing.T) {
	ctx := context.Background()
	from := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	for _, by := range []string{verdict.NoisyByCaught, verdict.NoisyByUseful} {
		s, _ := server(t, func(c *Config) { c.Backfill = 10 * 24 * time.Hour; c.Impact = ""; keepRecord(c); c.Options.NoisyBy = by })
		raw := ""
		if err := s.look(ctx, &raw); err != nil {
			t.Fatal(err)
		}
		s.jobs(ctx, true)
		o, _ := s.options(ctx)
		fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: from, To: from.Add(7 * 24 * time.Hour)})
		if err != nil || len(fs) != 14 {
			t.Fatalf("firings: %d %v", len(fs), err)
		}
		yes := "yes"
		for _, f := range fs[:10] {
			if _, err := s.l.SetVerdict(ctx, ledger.FiringKey{ID: f.ID, Series: f.Series, Start: f.Start}, ledger.VerdictChange{Action: &yes},
				ledger.VerdictBy{By: "ana", Admin: true, Now: time.Now()}, o); err != nil {
				t.Fatal(err)
			}
		}
		want := by == verdict.NoisyByCaught
		rep, err := s.l.Report(ctx, from, from.Add(7*24*time.Hour), o, false)
		if err != nil || len(rep.Rules) != 1 || rep.Rules[0].Noisy != want {
			t.Errorf("by %s, the report: %+v %v, want noisy %v", by, rep.Rules, err, want)
		}
		tl, err := s.tallies(ctx, 4)
		if err != nil {
			t.Fatal(err)
		}
		id := tl.id("Errors")
		week := ledger.Week(s.l.Day(from))
		x := tl.judged(week, id)
		if got := tl.noisy(week, id); got != want {
			t.Errorf("by %s, the week's tallies: noisy %v, want %v", by, got, want)
		}
		if got := tl.noisyOver(tl.weeks, id, x.Firings, x.White); got != want {
			t.Errorf("by %s, over weeks: noisy %v, want %v", by, got, want)
		}
	}
}
