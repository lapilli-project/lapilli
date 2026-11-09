package source

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

var t0 = time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)

// held answers each expression with when it held: "errors" for 3m from
// 10:02 on the Tuesday, and from 15:00 to 15:20 on the Thursday; "slow" for
// 40m from 10:00; anything else never.
type held struct{ asked atomic.Int64 }

func (h *held) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	h.asked.Add(1)
	var spans [][2]time.Time
	tue, thu := t0.Add(24*time.Hour), t0.Add(72*time.Hour)
	switch expr {
	case "errors":
		spans = [][2]time.Time{{tue.Add(10*time.Hour + 2*time.Minute), tue.Add(10*time.Hour + 4*time.Minute)},
			{thu.Add(15 * time.Hour), thu.Add(15*time.Hour + 20*time.Minute)}}
	case "slow":
		spans = [][2]time.Time{{tue.Add(10 * time.Hour), tue.Add(10*time.Hour + 39*time.Minute)}}
	}
	s := prom.Series{Labels: map[string]string{"service": "checkout"}}
	for _, sp := range spans {
		for t := sp[0]; !t.After(sp[1]); t = t.Add(step) {
			if !t.Before(start) && !t.After(end) {
				s.Samples = append(s.Samples, prom.Sample{T: t, V: 1})
			}
		}
	}
	if len(s.Samples) == 0 {
		return nil, nil
	}
	return []prom.Series{s}, nil
}

func TestNearMisses(t *testing.T) {
	o := verdict.Options{From: t0, To: t0.Add(7 * 24 * time.Hour), Lead: 15 * time.Minute, Match: []string{"service"},
		RefireWithin: time.Hour, UnknownLen: time.Hour}
	tue := t0.Add(24 * time.Hour)
	rs := []rules.Rule{
		{Name: "Errors", Expr: "errors", For: 5 * time.Minute},
		{Name: "Slow", Expr: "slow", For: time.Hour},
		{Name: "Search", Expr: "errors", For: 5 * time.Minute, Labels: map[string]string{"service": "search"}}, // another service's
		{Name: "Instant", Expr: "never"},                                                                       // no "for" to shorten
	}
	incs := []incident.Incident{
		{ID: "INC-1", Start: tue.Add(10 * time.Hour), End: tue.Add(10*time.Hour + 30*time.Minute), Labels: incident.Labels{"service": {"checkout"}}},
		{ID: "INC-2", Start: tue.Add(20 * time.Hour), Labels: incident.Labels{"service": {"checkout"}}}, // nothing near it
	}
	h := &held{}
	versions := []replay.Version{{Rules: rs}}
	_, fs := replay.Run(context.Background(), h, versions, o.From, o.To, time.Minute, 4)
	rep := verdict.Judge(rs, fs, incs, o)
	if !rep.Incidents[0].Missed() {
		t.Fatalf("INC-1 caught: %+v", rep.Incidents[0])
	}
	h.asked.Store(0)
	nms, _, _ := NearMisses(context.Background(), h, versions, rep, incs, o, time.Minute, 4, nil)
	if len(nms) != 2 {
		t.Fatalf("%d near misses: %+v", len(nms), nms)
	}
	// Slow held for 40m of its hour: with 30m it fires at the incident's end.
	// That keeps more of its "for" than Errors' 2m of 5m, so it comes first.
	s, e := nms[0], nms[1]
	if s.Rule != "Slow" || s.NewFor != 30*time.Minute || s.Delay != 30*time.Minute || s.Held != 40*time.Minute {
		t.Errorf("Slow: %+v", s)
	}
	if e.Rule != "Errors" || e.Incident != "INC-1" || e.NewFor != 2*time.Minute || e.Delay != 4*time.Minute || e.For != 5*time.Minute {
		t.Errorf("Errors: %+v", e)
	}
	// With a "for" of 2m, Errors fires twice that week: on INC-1, and on
	// Thursday, when it fired anyway.
	if e.After == nil || e.Before.Firings != 1 || e.After.Firings != 2 || e.After.White != 1 || e.After.Black != 1 {
		t.Errorf("Errors' cost: before %+v after %+v", e.Before, e.After)
	}
	// Two rules asked about INC-1's window, and the INC-2 window for both,
	// and each near miss replayed as suggested; then the week, a day at a
	// time, once each.
	if n := h.asked.Load(); n > 4+2+2*9 {
		t.Errorf("asked %d times", n)
	}

	// An incident only a noisy rule caught is looked at too, but not for that
	// rule; and a noisy rule nearly catching one counts for nothing.
	rep.Incidents[0].CaughtBy = []verdict.Catch{{Rule: "Slow", Noisy: true}}
	for i := range rep.Rules {
		if rep.Rules[i].Rule == "Errors" {
			rep.Rules[i].Noisy = true
		}
	}
	if nms, _, _ := NearMisses(context.Background(), h, versions, rep, incs, o, time.Minute, 4, nil); len(nms) != 0 {
		t.Errorf("noisy: %+v", nms)
	}

	// Against the rules as they stood: from Tuesday 09:00, Errors was gone.
	rep.Incidents[0].CaughtBy = nil
	for i := range rep.Rules {
		rep.Rules[i].Noisy = false
	}
	later := []replay.Version{{Rules: rs}, {From: tue.Add(9 * time.Hour), Rules: rs[1:]}}
	nms, _, _ = NearMisses(context.Background(), h, later, rep, incs, o, time.Minute, 4, nil)
	if len(nms) != 1 || nms[0].Rule != "Slow" {
		t.Errorf("as they stood: %+v", nms)
	}

	// Out of time, it says what it found.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if nms, done, _ := NearMisses(ctx, h, versions, rep, incs, o, time.Minute, 4, nil); len(nms) != 0 || done {
		t.Errorf("cancelled: %+v", nms)
	}
}
