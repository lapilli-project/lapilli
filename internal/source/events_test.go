package source

import (
	"context"
	"regexp"
	"strconv"
	"testing"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// restarts answers increase(restarts[30m]) compared with a number: each
// restart holds the count at 1 for 30 minutes, every two hours over the
// first three days, two alerts taking turns; and one flicker, a count of
// 0.55 for ten minutes on the fourth.
type restarts struct{}

var restartsBound = regexp.MustCompile(`>\s*([0-9.]+)\s*$`)

func (restarts) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	m := restartsBound.FindStringSubmatch(expr)
	if m == nil {
		return nil, nil
	}
	bound, _ := strconv.ParseFloat(m[1], 64)
	series := map[string]*prom.Series{}
	add := func(pod string, from, to time.Time, v float64) {
		if v <= bound {
			return
		}
		s := series[pod]
		if s == nil {
			s = &prom.Series{Labels: map[string]string{"pod": pod}}
			series[pod] = s
		}
		for t := from; t.Before(to); t = t.Add(step) {
			if !t.Before(start) && !t.After(end) {
				s.Samples = append(s.Samples, prom.Sample{T: t, V: v})
			}
		}
	}
	for k := range 36 {
		at := t0.Add(time.Duration(k) * 2 * time.Hour)
		add([]string{"a", "b"}[k%2], at, at.Add(30*time.Minute), 1)
	}
	flick := t0.Add(80 * time.Hour)
	add("c", flick, flick.Add(10*time.Minute), 0.55)
	var out []prom.Series
	for _, pod := range []string{"a", "b", "c"} {
		if s := series[pod]; s != nil && len(s.Samples) > 0 {
			out = append(out, *s)
		}
	}
	return out, nil
}

// restartsWeek is the week, judged against an incident none of its firings
// catches, or none at all, with the restart rule replayed.
func restartsWeek(t *testing.T, incs []incident.Incident) ([]replay.Version, *verdict.Report, verdict.Options) {
	t.Helper()
	o := verdict.Options{From: t0, To: t0.Add(7 * 24 * time.Hour), Lead: 15 * time.Minute, Match: []string{"service"},
		RefireWithin: time.Hour, UnknownLen: time.Hour}
	rs := []rules.Rule{{Name: "Restarts", Expr: "increase(restarts[30m]) > 0.5", For: time.Minute, KeepFiringFor: 10 * time.Minute,
		Labels: map[string]string{"team": "infra"}}}
	versions := []replay.Version{{Rules: rs}}
	known, fs := replay.Run(context.Background(), restarts{}, versions, o.From, o.To, time.Minute, 2)
	return versions, verdict.Judge(known, fs, incs, o), o
}

// A noisy rule that counts events is never suggested a for as long as the
// range it counts over: from 30m, one restart alone can never fire it, so
// it would fire less for no better reason. Shorter ones don't halve it.
func TestSuggestNotPastTheWindow(t *testing.T) {
	away := []incident.Incident{{ID: "INC-1", Start: t0.Add(6 * 24 * time.Hour), End: t0.Add(6*24*time.Hour + time.Hour), Labels: incident.Labels{"service": {"x"}}}}
	versions, rep, o := restartsWeek(t, away)
	if rep.NoRecord || !rep.Rules[0].Noisy || rep.Rules[0].Firings < 30 {
		t.Fatalf("the restart rule: %+v", rep.Rules[0])
	}
	if got := Suggest(context.Background(), restarts{}, versions[0].Rules, rep, away, o, time.Minute, nil); len(got) != 0 {
		t.Errorf("suggested %+v", got)
	}
	// With nothing written down, nothing is noisy, and nothing suggested.
	versions, rep, o = restartsWeek(t, nil)
	if !rep.NoRecord || rep.Rules[0].Noisy {
		t.Fatalf("no record: %+v", rep.Rules[0])
	}
	if got := Suggest(context.Background(), restarts{}, versions[0].Rules, rep, nil, o, time.Minute, nil); len(got) != 0 {
		t.Errorf("suggested with no record %+v", got)
	}
}
