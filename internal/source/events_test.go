package source

import (
	"context"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
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

// firedOf gives a rule's firings, as a report judged them.
func firedOf(rep *verdict.Report) func(string) ([]firing.Firing, error) {
	return func(rule string) ([]firing.Firing, error) {
		var out []firing.Firing
		for _, j := range rep.Firings {
			if j.Rule == rule {
				out = append(out, j.Firing)
			}
		}
		return out, nil
	}
}

// With no incident written down, the restart rule isn't noisy: what its
// firings were is said instead. Each restart held the count at 1, well past
// the bound of 0.5, so 36 of its 37 firings happened; the flicker at 0.55
// didn't go past 0.6. Its alerts, by the label that tells them apart, with
// the team whose labels each has.
func TestUnrecorded(t *testing.T) {
	versions, rep, _ := restartsWeek(t, nil)
	teamOf := func(l map[string]string) string {
		if l["pod"] == "a" {
			return "web"
		}
		return ""
	}
	us, err := Unrecorded(context.Background(), restarts{}, versions, rep, firedOf(rep), teamOf, time.Minute)
	if err != nil || len(us) != 1 {
		t.Fatalf("%+v %v", us, err)
	}
	u := us[0]
	if u.Rule != "Restarts" || u.Firings != 37 || u.Clear == nil || *u.Clear != 36 || u.Bound != "> 0.5" || u.Alerts != 3 {
		t.Errorf("%+v", u)
	}
	want := []verdict.AlertCount{{Labels: map[string]string{"pod": "a"}, Firings: 18, Team: "web"}, {Labels: map[string]string{"pod": "b"}, Firings: 18},
		{Labels: map[string]string{"pod": "c"}, Firings: 1}}
	if !reflect.DeepEqual(u.Top, want) {
		t.Errorf("top %+v", u.Top)
	}
	if got := u.Text(); got != `Fired 37 times with no incident written down to judge them by. 36 of them went well past its bound (> 0.5), not a flicker at its edge: these happened. Most came from: pod=a 18 (web), pod=b 18, pod=c 1. A longer "for" would hide them: write down whether they mattered, or fix what fires them.` {
		t.Errorf("text %q", got)
	}

	// Judged, nothing to say.
	away := []incident.Incident{{ID: "INC-1", Start: t0.Add(6 * 24 * time.Hour), End: t0.Add(6*24*time.Hour + time.Hour), Labels: incident.Labels{"service": {"x"}}}}
	versions, rep, _ = restartsWeek(t, away)
	if us, err := Unrecorded(context.Background(), restarts{}, versions, rep, firedOf(rep), nil, time.Minute); err != nil || len(us) != 0 {
		t.Errorf("with a record: %+v %v", us, err)
	}

	// A condition with no one bound that decides it: how many went past it
	// isn't known, and isn't guessed.
	versions, rep, _ = restartsWeek(t, nil)
	versions[0].Rules[0].Expr = "up == 1 and increase(restarts[30m]) > 0.5"
	us, err = Unrecorded(context.Background(), restarts{}, versions, rep, firedOf(rep), nil, time.Minute)
	if err != nil || len(us) != 1 || us[0].Clear != nil || us[0].Bound != "" || us[0].Alerts != 3 {
		t.Errorf("and: %+v %v", us, err)
	}
	if strings.Contains(us[0].Text(), "well past") || strings.Contains(us[0].Text(), "would hide") {
		t.Errorf("and: %q", us[0].Text())
	}
}
