package source

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// valued answers a value's name with its value each step, and "name > n"
// (or <) with the steps it held: latency is 0.88 from 10:00 to 10:30 on the
// Tuesday and 0.875 from 15:00 to 15:10 on the Thursday, far 0.5 from 10:00
// on the Tuesday; 0.1 otherwise.
type valued struct {
	asked atomic.Int64
	mu    sync.Mutex
	exprs []string
}

func (h *valued) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	h.asked.Add(1)
	h.mu.Lock()
	h.exprs = append(h.exprs, expr)
	h.mu.Unlock()
	tue, thu := t0.Add(24*time.Hour), t0.Add(72*time.Hour)
	in := func(t, from time.Time, d time.Duration) bool { return !t.Before(from) && t.Before(from.Add(d)) }
	f := strings.Fields(expr)
	value := func(t time.Time) float64 {
		switch f[0] {
		case "latency":
			switch {
			case in(t, tue.Add(10*time.Hour), 30*time.Minute):
				return 0.88
			case in(t, thu.Add(15*time.Hour), 10*time.Minute):
				return 0.875
			}
		case "far":
			if in(t, tue.Add(10*time.Hour), 30*time.Minute) {
				return 0.5
			}
		case "dip":
			// 0.88 from 70m to 80m before 11:40 on the Tuesday's own clock
			// (m70–m80 of a base 100m before 10:00), a dip, and again from m83
			// to m110.
			base := tue.Add(10*time.Hour - 100*time.Minute)
			if i := int(t.Sub(base) / time.Minute); i >= 70 && i <= 80 || i >= 83 && i <= 110 {
				return 0.88
			}
		}
		return 0.1
	}
	s := prom.Series{Labels: map[string]string{"__name__": f[0], "service": "checkout"}}
	for t := start; !t.After(end); t = t.Add(step) {
		v := value(t)
		if len(f) == 3 {
			n, _ := strconv.ParseFloat(f[2], 64)
			if !(rules.Bound{Op: f[1], Value: n}).Holds(v) {
				continue
			}
		}
		s.Samples = append(s.Samples, prom.Sample{T: t, V: v})
	}
	if len(s.Samples) == 0 {
		return nil, nil
	}
	return []prom.Series{s}, nil
}

func TestNearMissesByValue(t *testing.T) {
	o := verdict.Options{From: t0, To: t0.Add(7 * 24 * time.Hour), Lead: 15 * time.Minute, Match: []string{"service"},
		RefireWithin: time.Hour, UnknownLen: time.Hour}
	tue := t0.Add(24 * time.Hour)
	rs := []rules.Rule{
		{Name: "Latency", Expr: "latency > 0.9", For: 5 * time.Minute},
		{Name: "Far", Expr: "far > 0.9", For: 5 * time.Minute}, // a long way short
		{Name: "Instant", Expr: "latency > 0.89"},              // no "for": by value all the same
	}
	incs := []incident.Incident{{ID: "INC-1", Start: tue.Add(10 * time.Hour), End: tue.Add(10*time.Hour + 30*time.Minute),
		Labels: incident.Labels{"service": {"checkout"}}}}
	h := &valued{}
	versions := []replay.Version{{Rules: rs}}
	_, fs := replay.Run(context.Background(), h, versions, o.From, o.To, time.Minute, 4)
	rep := verdict.Judge(rs, fs, incs, o)
	if !rep.Incidents[0].Missed() {
		t.Fatalf("INC-1 caught: %+v", rep.Incidents[0])
	}
	nms, done, _ := NearMisses(context.Background(), h, versions, rep, incs, o, time.Minute, 4, nil)
	if !done || len(nms) != 2 {
		t.Fatalf("%v %d near misses: %+v", done, len(nms), nms)
	}
	// Instant keeps more of its number, 0.87 of 0.89, so it comes first; at
	// once, without a "for".
	i, l := nms[0], nms[1]
	if i.Rule != "Instant" || i.Value == nil || i.Value.New != 0.87 || i.Delay != 0 {
		t.Errorf("Instant: %+v %+v", i, i.Value)
	}
	// Latency stayed at 0.88 for its 5m: with 0.87 it fires at +5m.
	if l.Rule != "Latency" || l.Value == nil || l.Value.Op != ">" || l.Value.Bound != 0.9 || *l.Value.Reached != 0.88 || l.Value.New != 0.87 || l.Value.Of != "" ||
		l.Delay != 5*time.Minute || l.NewFor != 5*time.Minute || l.Held != 0 || l.Late {
		t.Errorf("Latency: %+v %+v", l, l.Value)
	}
	// With 0.87 it fires twice that week: on INC-1, and on Thursday, for
	// nothing.
	if l.After == nil || l.Before.Firings != 0 || l.After.Firings != 2 || l.After.White != 1 || l.After.Black != 1 {
		t.Errorf("Latency's cost: before %+v after %+v", l.Before, l.After)
	}

	// As the rules stood: before Tuesday, Latency's number was 0.85 already,
	// and stays so in the cost.
	was := []rules.Rule{{Name: "Latency", Expr: "latency > 0.85", For: 5 * time.Minute}}
	later := []replay.Version{{Rules: was}, {From: tue, Rules: rs[:1]}}
	nms, _, _ = NearMisses(context.Background(), h, later, rep, incs, o, time.Minute, 4, nil)
	if len(nms) != 1 || nms[0].After == nil || nms[0].After.Firings != 2 {
		t.Errorf("as they stood: %+v", nms)
	}
}

// refusing refuses every question.
type refusing struct{}

func (refusing) QueryRange(context.Context, string, time.Time, time.Time, time.Duration) ([]prom.Series, error) {
	return nil, errors.New("unavailable")
}

func TestNearMissesByValueBounds(t *testing.T) {
	o := verdict.Options{From: t0, To: t0.Add(7 * 24 * time.Hour), Lead: 15 * time.Minute, Match: []string{"service"},
		RefireWithin: time.Hour, UnknownLen: time.Hour}
	tue := t0.Add(24 * time.Hour)
	incs := []incident.Incident{{ID: "INC-1", Start: tue.Add(10 * time.Hour), End: tue.Add(10*time.Hour + 30*time.Minute),
		Labels: incident.Labels{"service": {"checkout"}}}}
	// A number of nothing has nothing within a fifth of it: not asked.
	rs := []rules.Rule{{Name: "Zero", Expr: "latency > 0"}}
	h := &valued{}
	versions := []replay.Version{{Rules: rs}}
	rep := verdict.Judge(rs, nil, incs, o)
	if nms, done, err := NearMisses(context.Background(), h, versions, rep, incs, o, time.Minute, 4, nil); len(nms) != 0 || !done || err != nil || h.asked.Load() != 0 {
		t.Errorf("zero: %+v %v %v, asked %d", nms, done, err, h.asked.Load())
	}
	// A history that fails: not done, and why.
	rs = []rules.Rule{{Name: "Latency", Expr: "latency > 0.9", For: 5 * time.Minute}}
	rep = verdict.Judge(rs, nil, incs, o)
	if nms, done, err := NearMisses(context.Background(), refusing{}, []replay.Version{{Rules: rs}}, rep, incs, o, time.Minute, 4, nil); len(nms) != 0 || done || err == nil {
		t.Errorf("refused: %+v %v %v", nms, done, err)
	}
	// Asked as the rule is, with its number a fifth further: the history
	// returns no more than could come near.
	h = &valued{}
	if _, _, err := NearMisses(context.Background(), h, []replay.Version{{Rules: rs}}, rep, incs, o, time.Minute, 4, nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(h.exprs, "latency > 0.72") || slices.Contains(h.exprs, "latency") {
		t.Errorf("asked %v", h.exprs)
	}
}

// Kept firing through a dip by keep_firing_for, a rule with a number a little
// lower fires long before the incident, and catches nothing.
func TestNearMissesByValueKeep(t *testing.T) {
	o := verdict.Options{From: t0, To: t0.Add(7 * 24 * time.Hour), Lead: 15 * time.Minute, Match: []string{"service"},
		RefireWithin: time.Hour, UnknownLen: time.Hour}
	tue := t0.Add(24 * time.Hour)
	incs := []incident.Incident{{ID: "INC-1", Start: tue.Add(10 * time.Hour), End: tue.Add(10*time.Hour + 30*time.Minute),
		Labels: incident.Labels{"service": {"checkout"}}}}
	for _, c := range []struct {
		keep time.Duration
		ok   bool
	}{{10 * time.Minute, false}, {0, true}} {
		rs := []rules.Rule{{Name: "Dip", Expr: "dip > 0.9", For: 5 * time.Minute, KeepFiringFor: c.keep}}
		rep := verdict.Judge(rs, nil, incs, o)
		nms, _, err := NearMisses(context.Background(), &valued{}, []replay.Version{{Rules: rs}}, rep, incs, o, time.Minute, 4, nil)
		if err != nil || (len(nms) == 1) != c.ok || c.ok && nms[0].Delay != -12*time.Minute {
			t.Errorf("keep %s: %v %+v", c.keep, err, nms)
		}
	}
}

// A rule paused for part of the period has no cost to tell: that isn't a
// failure.
func TestNearMissesPausedCost(t *testing.T) {
	o := verdict.Options{From: t0, To: t0.Add(7 * 24 * time.Hour), Lead: 15 * time.Minute, Match: []string{"service"},
		RefireWithin: time.Hour, UnknownLen: time.Hour}
	tue := t0.Add(24 * time.Hour)
	incs := []incident.Incident{{ID: "INC-1", Start: tue.Add(10 * time.Hour), End: tue.Add(10*time.Hour + 30*time.Minute),
		Labels: incident.Labels{"service": {"checkout"}}}}
	on := rules.Rule{Name: "Latency", Expr: "latency > 0.9", For: 5 * time.Minute}
	paused := on
	paused.Unsupported = rules.Paused
	versions := []replay.Version{{Rules: []rules.Rule{paused}}, {From: t0.Add(time.Hour), Rules: []rules.Rule{on}}}
	rep := verdict.Judge([]rules.Rule{on}, nil, incs, o)
	nms, done, err := NearMisses(context.Background(), &valued{}, versions, rep, incs, o, time.Minute, 4, nil)
	if err != nil || !done || len(nms) != 1 || nms[0].After != nil {
		t.Errorf("%v %v %+v", err, done, nms)
	}
}

func TestLookedAt(t *testing.T) {
	rep := &verdict.Report{}
	for i := 0; i < 25; i++ {
		rep.Incidents = append(rep.Incidents, verdict.IncidentResult{Incident: incident.Incident{ID: strconv.Itoa(i)}})
	}
	rep.Incidents[0].CaughtBy = []verdict.Catch{{Rule: "R"}}
	got := LookedAt(rep)
	if len(got) != 20 || got["0"] || !got["1"] || !got["20"] || got["21"] {
		t.Errorf("%v", got)
	}
}
