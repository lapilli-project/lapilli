package source

import (
	"context"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/response"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// blips answers any query with an alert that is on in short blips of two
// minutes, every three hours from the third, and in longer stretches of
// forty minutes: a rule that flaps, and a rule that fires for what lasts.
type blips struct{ blips, longs int }

func (b blips) spans() (short, long []time.Time) {
	for k := range b.blips {
		short = append(short, t0.Add(3*time.Hour+time.Duration(k)*3*time.Hour))
	}
	for k := range b.longs {
		long = append(long, t0.Add(110*time.Hour+time.Duration(k)*8*time.Hour))
	}
	return short, long
}

func (b blips) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	short, long := b.spans()
	s := prom.Series{Labels: map[string]string{"pod": "p"}}
	in := func(t time.Time) bool {
		for _, a := range short {
			if !t.Before(a) && t.Before(a.Add(2*time.Minute)) {
				return true
			}
		}
		for _, a := range long {
			if !t.Before(a) && t.Before(a.Add(40*time.Minute)) {
				return true
			}
		}
		return false
	}
	for t := start; !t.After(end); t = t.Add(step) {
		if in(t) {
			s.Samples = append(s.Samples, prom.Sample{T: t, V: 1})
		}
	}
	return []prom.Series{s}, nil
}

// answers is a Responder that says what was done about each firing by how
// long it lasted: a stretch of forty minutes had a rollout ten minutes
// after it began; a blip nobody answered, but for one that was rolled out
// a minute after it began.
type answers struct{ special time.Time }

func (a answers) Respond(f firing.Firing, within time.Duration, now time.Time, white, judged bool, answer string) verdict.FiringResponse {
	yes, no := true, false
	switch {
	case f.End.Sub(f.Start) >= 30*time.Minute:
		return verdict.FiringResponse{Kind: verdict.ResponseChanged, Useful: &yes,
			Change: &verdict.ResponseChange{Kind: "rollout", At: f.Start.Add(10 * time.Minute)}}
	case f.Start.Equal(a.special):
		return verdict.FiringResponse{Kind: verdict.ResponseChanged, Useful: &yes,
			Change: &verdict.ResponseChange{Kind: "rollout", At: f.Start.Add(time.Minute)}}
	}
	return verdict.FiringResponse{Kind: verdict.ResponseUnanswered, Useful: &no}
}

func (answers) Totals() verdict.ResponseTotals { return verdict.ResponseTotals{} }

// With --noisy-by useful, the longer "for" suggested keeps every useful
// firing, as with caught it keeps the incidents caught; one that would drop
// a useful blip isn't suggested, though it keeps every incident.
func TestSuggestKeepsUsefulFirings(t *testing.T) {
	h := blips{blips: 30, longs: 4}
	// An incident, in the week, the first stretch catches; and one before
	// it, so the record is kept all week.
	incs := []incident.Incident{
		{ID: "INC-0", Start: t0.Add(-24 * time.Hour), End: t0.Add(-23 * time.Hour)},
		{ID: "INC-1", Start: t0.Add(110*time.Hour + 5*time.Minute), End: t0.Add(112 * time.Hour)},
	}
	rs := []rules.Rule{{Name: "Flaps", Expr: "blip > 0", For: 0}}
	versions := []replay.Version{{Rules: rs}}
	o := verdict.Options{From: t0, To: t0.Add(7 * 24 * time.Hour), Lead: 15 * time.Minute, RefireWithin: time.Hour, UnknownLen: time.Hour}
	known, fs := replay.Run(context.Background(), h, versions, o.From, o.To, time.Minute, 1)

	// By stones: 34 firings, one white, noisy; a longer for keeps INC-1.
	rep := verdict.Judge(known, fs, incs, o)
	if !rep.Rules[0].Noisy || rep.Rules[0].Firings != 34 {
		t.Fatalf("by stones: %+v", rep.Rules[0])
	}
	got, _ := Suggest(context.Background(), h, rs, rep, incs, o, time.Minute, 1, nil)
	if len(got) != 1 || got[0].NewFor != 5*time.Minute {
		t.Fatalf("by stones, suggested %+v, want a for of 5m", got)
	}

	// By what was done: the blips are unanswered, and the stretches rolled
	// out, so 4 of 34 are useful: noisy, and the longer for keeps the
	// stretches' firings, started before their rollouts.
	short, _ := h.spans()
	ou := o
	ou.Responder, ou.NoisyBy, ou.ResponseStep = answers{special: short[7]}, verdict.NoisyByUseful, time.Minute
	_, fs = replay.Run(context.Background(), h, versions, o.From, o.To, time.Minute, 1)
	// A blip that had a rollout is one more useful: 5 of 34, still noisy,
	// and no longer for keeps it.
	rep = verdict.Judge(known, fs, incs, ou)
	r := rep.Rules[0]
	if !r.Noisy || r.Responses == nil || r.Responses.Useful != 5 || r.Responses.Unanswered != 29 || r.Responses.Changed != 5 {
		t.Fatalf("by what was done: noisy %v, %+v", r.Noisy, r.Responses)
	}
	if got, _ := Suggest(context.Background(), h, rs, rep, incs, ou, time.Minute, 1, nil); len(got) != 0 {
		t.Errorf("suggested %+v, which drops the blip that was rolled out", got)
	}
	// With no blip rolled out, the longer for keeps every useful firing.
	ou.Responder = answers{}
	rep = verdict.Judge(known, fs, incs, ou)
	got, _ = Suggest(context.Background(), h, rs, rep, incs, ou, time.Minute, 1, nil)
	if len(got) != 1 || got[0].NewFor != 5*time.Minute {
		t.Errorf("suggested %+v, want a for of 5m", got)
	}
	// The 5m for leaves the four stretches, and drops the blips.
	for _, s := range got {
		if s.After.Firings != 4 {
			t.Errorf("after: %+v", s.After)
		}
	}
}

// The Responder of lapilli verdict hands internal/response's answer to
// Judge: a firing's response, whether it was useful, and the pages no firing
// had.
func TestResponsesJudge(t *testing.T) {
	start := t0.Add(3 * time.Hour)
	pages, err := response.Parse([]byte(`responses:
  - id: PD-1
    alerts: [{rule: Flaps, labels: {pod: p}}]
    sent: `+start.Add(time.Minute).Format(time.RFC3339)+`
    resolved: `+start.Add(2*time.Minute).Format(time.RFC3339)+`
    resolved_by: alert
  - id: PD-2
    alerts: [{rule: Gone, labels: {pod: p}}]
    sent: `+start.Format(time.RFC3339)+`
`), "responses.yaml")
	if err != nil {
		t.Fatal(err)
	}
	rr := &Responses{Evidence: response.Evidence{Pages: response.NewIndex(pages), Changes: response.NewChangeSet(t0, t0.Add(24*time.Hour)), Routine: 0.5}}
	fr := rr.Respond(firing.Firing{Rule: "Flaps", Labels: map[string]string{"pod": "p"}, Start: start, End: start.Add(2 * time.Minute)}, 2*time.Minute, t0.Add(24*time.Hour), false, true, "")
	if fr.Kind != verdict.ResponseUnanswered || fr.Useful == nil || *fr.Useful || fr.Page == nil || fr.Page.ID != "PD-1" || fr.Page.ResolvedBy != "alert" {
		t.Errorf("an unanswered page: %+v", fr)
	}
	if tot := rr.Totals(); tot.Pages != 2 || tot.PagesUnmatched != 1 {
		t.Errorf("totals %+v", tot)
	}
}
