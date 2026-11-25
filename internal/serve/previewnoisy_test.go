package serve

import (
	"context"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// A preview (the impact criterion's, the signals') judges with the responses
// of the firings it is given when rules are found noisy by what was done
// about them: Errors' 14 firings caught nothing, but a person said 10 of them
// needed action, so it is not noisy, as in the report, and not by what they
// caught, which the preview asked before.
func TestPreviewsAskTheNoisyTestOfEverySurface(t *testing.T) {
	ctx := context.Background()
	from := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	to := from.Add(7 * 24 * time.Hour)
	s, _ := server(t, func(c *Config) {
		c.Backfill = 10 * 24 * time.Hour
		c.Impact = ""
		keepRecord(c)
		c.Options.NoisyBy = verdict.NoisyByUseful
	})
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	o, _ := s.options(ctx)
	fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: from, To: to})
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
	fs, err = s.l.Firings(ctx, ledger.FiringQuery{From: from, To: to})
	if err != nil {
		t.Fatal(err)
	}
	o.From, o.To = from, to
	wf, _ := s.l.WeekOf(from)
	_, wt := s.l.WeekOf(to.Add(-time.Nanosecond))
	around, err := s.l.RecordIncidents(ctx, verdict.RecordFrom(wf, wt), wt, o.UnknownLen)
	if err != nil {
		t.Fatal(err)
	}
	var incs []incident.Incident
	for _, in := range around {
		incs = append(incs, in.Incident)
	}
	said, err := s.l.ReplayVerdicts(ctx, from, to)
	if err != nil {
		t.Fatal(err)
	}
	o.Verdicts = said
	var plain []firing.Firing
	for _, f := range fs {
		plain = append(plain, f.Firing)
	}
	noisy := func(o verdict.Options) int {
		n := 0
		for _, r := range verdict.Judge(nil, plain, incs, o).Rules {
			if r.Noisy {
				n++
			}
		}
		return n
	}
	if n := noisy(o); n != 1 {
		t.Fatalf("without responses %d noisy, want 1 (by what they caught)", n)
	}
	rsp, err := s.previewResponses(ctx, fs, o)
	if err != nil || rsp == nil {
		t.Fatalf("%v %v", rsp, err)
	}
	o.Responder = rsp
	if n := noisy(o); n != 0 {
		t.Errorf("with responses %d noisy, want 0 (10 of 14 needed action)", n)
	}
}
