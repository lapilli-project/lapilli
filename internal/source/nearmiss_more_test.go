package source

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// spans answers an expression with 1 at each evaluation asked for that falls
// in one of its stretches of time, on whatever grid the query asks: as
// Prometheus does. An expression with "broken" in it fails.
type spans struct {
	by    map[string][][2]time.Time
	asked atomic.Int64
}

func (c *spans) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	c.asked.Add(1)
	if strings.Contains(expr, "broken") {
		return nil, errors.New("bad_data")
	}
	s := prom.Series{Labels: map[string]string{"service": "checkout"}}
	for t := start; !t.After(end); t = t.Add(step) {
		for _, sp := range c.by[expr] {
			if !t.Before(sp[0]) && !t.After(sp[1]) {
				s.Samples = append(s.Samples, prom.Sample{T: t, V: 1})
				break
			}
		}
	}
	if len(s.Samples) == 0 {
		return nil, nil
	}
	return []prom.Series{s}, nil
}

func week() verdict.Options {
	return verdict.Options{From: t0, To: t0.Add(7 * 24 * time.Hour), Lead: 15 * time.Minute, Match: []string{"service"},
		RefireWithin: time.Hour, UnknownLen: time.Hour}
}

func judged(t *testing.T, h replay.History, versions []replay.Version, incs []incident.Incident, o verdict.Options) *verdict.Report {
	t.Helper()
	known, fs := replay.Run(context.Background(), h, versions, o.From, o.To, time.Minute, 4)
	return verdict.Judge(known, fs, incs, o)
}

func checkout(id string, from, to time.Time) incident.Incident {
	return incident.Incident{ID: id, Start: from, End: to, Labels: incident.Labels{"service": {"checkout"}}}
}

var tue = t0.Add(24 * time.Hour)

func at(h, m, s int) time.Time {
	return tue.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second)
}

// An incident that starts on odd seconds: the condition is asked when the
// replay asked it, so a replay with the shorter "for" catches what it says.
func TestNearMissOnTheReplaysSteps(t *testing.T) {
	o := week()
	h := &spans{by: map[string][][2]time.Time{"c": {{at(10, 2, 10), at(10, 6, 50)}}}}
	r := rules.Rule{Name: "R", Expr: "c", For: 5 * time.Minute}
	versions := []replay.Version{{Rules: []rules.Rule{r}}}
	incs := []incident.Incident{checkout("INC-1", at(10, 0, 20), at(10, 30, 0))}
	nms, _, _ := NearMisses(context.Background(), h, versions, judged(t, h, versions, incs, o), incs, o, time.Minute, 4, nil)
	if len(nms) != 1 || nms[0].Delay%time.Minute != 40*time.Second || nms[0].After == nil || nms[0].After.Firings != 1 || nms[0].After.White != 1 {
		t.Fatalf("%+v", nms)
	}
}

// A "for" that isn't a whole number of evaluations: the longest shorter one
// in whole evaluations.
func TestNearMissOddFor(t *testing.T) {
	o := week()
	h := &spans{by: map[string][][2]time.Time{"c": {{at(10, 2, 0), at(10, 4, 0)}}}}
	versions := []replay.Version{{Rules: []rules.Rule{{Name: "R", Expr: "c", For: 150 * time.Second}}}}
	incs := []incident.Incident{checkout("INC-1", at(10, 0, 0), at(10, 30, 0))}
	nms, _, _ := NearMisses(context.Background(), h, versions, judged(t, h, versions, incs, o), incs, o, time.Minute, 4, nil)
	if len(nms) != 1 || nms[0].NewFor != 2*time.Minute || nms[0].Delay != 4*time.Minute || nms[0].After.Firings != 1 {
		t.Fatalf("%+v", nms)
	}
}

// Held for two hours from the incident's start, with a "for" of 30m: it
// fired, but only after the ten-minute incident was over.
func TestNearMissLate(t *testing.T) {
	o := week()
	h := &spans{by: map[string][][2]time.Time{"c": {{at(10, 0, 0), at(12, 0, 0)}}}}
	versions := []replay.Version{{Rules: []rules.Rule{{Name: "R", Expr: "c", For: 30 * time.Minute}}}}
	incs := []incident.Incident{checkout("INC-1", at(10, 0, 0), at(10, 10, 0))}
	nms, _, _ := NearMisses(context.Background(), h, versions, judged(t, h, versions, incs, o), incs, o, time.Minute, 4, nil)
	if len(nms) != 1 || !nms[0].Late || nms[0].Fired != 30*time.Minute || nms[0].Held != 0 || nms[0].NewFor != 10*time.Minute {
		t.Fatalf("%+v", nms)
	}
}

// A rule added on Wednesday costs what it would have from Wednesday.
func TestNearMissCostAsTheRulesStood(t *testing.T) {
	o := week()
	mon, wed, thu := t0, t0.Add(48*time.Hour), t0.Add(72*time.Hour)
	h := &spans{by: map[string][][2]time.Time{"c": {{mon.Add(10 * time.Hour), mon.Add(11 * time.Hour)}, {thu.Add(15 * time.Hour), thu.Add(15*time.Hour + 20*time.Minute)}}}}
	r := rules.Rule{Name: "R", Expr: "c", For: 30 * time.Minute}
	other := rules.Rule{Name: "Other", Expr: "never", For: time.Minute}
	versions := []replay.Version{{Rules: []rules.Rule{other}}, {From: wed, Rules: []rules.Rule{other, r}}}
	incs := []incident.Incident{checkout("INC-1", thu.Add(15*time.Hour), thu.Add(15*time.Hour+40*time.Minute))}
	nms, _, _ := NearMisses(context.Background(), h, versions, judged(t, h, versions, incs, o), incs, o, time.Minute, 4, nil)
	if len(nms) != 1 || nms[0].After == nil || nms[0].After.Firings != 1 || nms[0].After.Black != 0 {
		t.Fatalf("%+v", nms)
	}
}

// What a quiet window excuses isn't counted against the shorter "for".
func TestNearMissCostExcused(t *testing.T) {
	o := week()
	h := &spans{by: map[string][][2]time.Time{"c": {{at(10, 0, 0), at(10, 10, 0)}, {at(20, 0, 0), at(20, 10, 0)}}}}
	versions := []replay.Version{{Rules: []rules.Rule{{Name: "R", Expr: "c", For: 30 * time.Minute}}}}
	incs := []incident.Incident{checkout("INC-1", at(10, 0, 0), at(10, 30, 0))}
	rep := judged(t, h, versions, incs, o)
	evening := func(f firing.Firing) bool { return f.Start.After(at(19, 0, 0)) }
	nms, _, _ := NearMisses(context.Background(), h, versions, rep, incs, o, time.Minute, 4, evening)
	if len(nms) != 1 || nms[0].After.Firings != 1 || nms[0].After.Black != 0 || nms[0].After.Excused != 1 {
		t.Fatalf("%+v", nms[0].After)
	}
}

// An incident whose end is far off, as some tools mark one still open,
// is looked at within the period, and quickly.
func TestNearMissFarEnd(t *testing.T) {
	o := week()
	h := &spans{by: map[string][][2]time.Time{"c": {{at(10, 2, 0), at(10, 4, 0)}}}}
	r := rules.Rule{Name: "R", Expr: "c", For: 5 * time.Minute}
	incs := []incident.Incident{checkout("INC-1", at(10, 0, 0), time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC))}
	rep := judged(t, h, []replay.Version{{Rules: []rules.Rule{r}}}, incs, o)
	// A "for" of centuries is passed over, as it would reach back as far.
	long := rules.Rule{Name: "Long", Expr: "c", For: 90000 * 24 * time.Hour}
	rep.Rules = append(rep.Rules, verdict.RuleResult{Rule: "Long", Replayed: true})
	versions := []replay.Version{{Rules: []rules.Rule{r, long}}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	began := time.Now()
	nms, done, _ := NearMisses(ctx, h, versions, rep, incs, o, time.Minute, 4, nil)
	if !done || len(nms) != 1 || nms[0].Rule != "R" || time.Since(began) > 2*time.Second {
		t.Fatalf("%v %s %+v", done, time.Since(began), nms)
	}
}

// At most a few rules for each incident, and at most nearCosts replays of
// the period tried, whether they succeed or not.
func TestNearMissBounds(t *testing.T) {
	o := week()
	h := &spans{by: map[string][][2]time.Time{}}
	var rs []rules.Rule
	for i := 0; i < 12; i++ {
		expr := fmt.Sprintf("c%d", i)
		for _, hour := range []int{10, 14, 18} {
			h.by[expr] = append(h.by[expr], [2]time.Time{at(hour, 2, 0), at(hour, 3, 0)})
		}
		rs = append(rs, rules.Rule{Name: fmt.Sprintf("R%02d", i), Expr: expr, For: 5 * time.Minute})
	}
	versions := []replay.Version{{Rules: rs}}
	var incs []incident.Incident
	for _, hour := range []int{10, 14, 18} {
		incs = append(incs, checkout(fmt.Sprint("INC-", hour), at(hour, 0, 0), at(hour, 30, 0)))
	}
	rep := judged(t, h, versions, incs, o)
	// The windows about the incidents are answered; a replay of the period,
	// a day at a time, isn't.
	failing := &failLong{spans: h}
	nms, _, _ := NearMisses(context.Background(), failing, versions, rep, incs, o, time.Minute, 4, nil)
	if len(nms) != 3*nearEach {
		t.Errorf("%d near misses for three incidents, want %d", len(nms), 3*nearEach)
	}
	for _, nm := range nms {
		if nm.After != nil {
			t.Errorf("a cost from a replay that failed: %+v", nm)
		}
	}
	if n := failing.long.Load(); n > nearCosts {
		t.Errorf("tried %d replays of the period, want %d at most", n, nearCosts)
	}
}

type failLong struct {
	*spans
	long atomic.Int64
}

func (f *failLong) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if end.Sub(start) >= 12*time.Hour {
		f.long.Add(1)
		return nil, errors.New("too much")
	}
	return f.spans.QueryRange(ctx, expr, start, end, step)
}
