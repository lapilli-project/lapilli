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
	// Written down the day before the week, nothing caught: every firing judged.
	away := []incident.Incident{{ID: "INC-1", Start: t0.Add(-24 * time.Hour), End: t0.Add(-23 * time.Hour), Labels: incident.Labels{"service": {"x"}}}}
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

// teamed are a report's firings with no record, each alert with the team
// whose labels it has: pod a is web's.
type teamed struct{ Judged }

func (t teamed) Alerts(ctx context.Context, rule string, most int) ([]verdict.AlertCount, int, error) {
	top, all, err := t.Judged.Alerts(ctx, rule, most)
	for i := range top {
		if top[i].Labels["pod"] == "a" {
			top[i].Team = "web"
		}
	}
	return top, all, err
}

// With no incident written down, the restart rule isn't noisy: what its
// firings were is said instead. Its condition counts restarts, so each of
// its 37 firings is one, the flicker at 0.55 too; its alerts, by the label
// that tells them apart, with the team whose labels each has.
func TestUnrecorded(t *testing.T) {
	versions, rep, _ := restartsWeek(t, nil)
	us, err := Unrecorded(context.Background(), restarts{}, versions, rep, teamed{Judged(rep.Firings)}, time.Minute)
	if err != nil || len(us) != 1 {
		t.Fatalf("%+v %v", us, err)
	}
	u := us[0]
	if u.Rule != "Restarts" || u.Firings != 37 || !u.Events || u.Clear == nil || *u.Clear != 37 || u.Bound != "" || u.Alerts != 3 {
		t.Errorf("%+v", u)
	}
	want := []verdict.AlertCount{{Labels: map[string]string{"pod": "a"}, Firings: 18, Team: "web"}, {Labels: map[string]string{"pod": "b"}, Firings: 18},
		{Labels: map[string]string{"pod": "c"}, Firings: 1}}
	if !reflect.DeepEqual(u.Top, want) {
		t.Errorf("top %+v", u.Top)
	}
	if got := u.Text(); got != `Fired 37 times with no incident written down to judge them by. All 37 are events its condition counts (a count that went up): these happened. Most came from: pod=a 18 (web), pod=b 18, pod=c 1. A longer "for" would hide them: write down whether they mattered, or fix what fires them.` {
		t.Errorf("text %q", got)
	}

	// Judged, nothing to say.
	away := []incident.Incident{{ID: "INC-1", Start: t0.Add(-24 * time.Hour), End: t0.Add(-23 * time.Hour), Labels: incident.Labels{"service": {"x"}}}}
	versions, rep, _ = restartsWeek(t, away)
	if us, err := Unrecorded(context.Background(), restarts{}, versions, rep, Judged(rep.Firings), time.Minute); err != nil || len(us) != 0 {
		t.Errorf("with a record: %+v %v", us, err)
	}

	// A level, not a count of events, with one bound that decides it: the
	// 36 at 1 went well past 0.5; the flicker at 0.55 didn't stay past 0.6.
	versions, rep, _ = restartsWeek(t, nil)
	versions[0].Rules[0].Expr = "level > 0.5"
	us, err = Unrecorded(context.Background(), restarts{}, versions, rep, Judged(rep.Firings), time.Minute)
	if err != nil || len(us) != 1 || us[0].Events || us[0].Clear == nil || *us[0].Clear != 36 || us[0].Bound != "> 0.5" {
		t.Errorf("a level: %+v %v", us, err)
	}
	if !strings.Contains(us[0].Text(), "36 of them went well past its bound (> 0.5), not a flicker at its edge") {
		t.Errorf("a level: %q", us[0].Text())
	}

	// No one bound that decides it: how many went past isn't known, and
	// isn't guessed.
	versions[0].Rules[0].Expr = "up == 1 and level > 0.5"
	us, err = Unrecorded(context.Background(), restarts{}, versions, rep, Judged(rep.Firings), time.Minute)
	if err != nil || len(us) != 1 || us[0].Clear != nil || us[0].Bound != "" || us[0].Alerts != 3 {
		t.Errorf("and: %+v %v", us, err)
	}
	if strings.Contains(us[0].Text(), "well past") || strings.Contains(us[0].Text(), "would hide") {
		t.Errorf("and: %q", us[0].Text())
	}
}

// more is an UnrecordedFrom that has more firings than are read.
type more struct{ Judged }

func (m more) Firings(ctx context.Context, rule string, most int) ([]firing.Firing, bool, error) {
	fs, _, err := m.Judged.Firings(ctx, rule, most)
	return fs, true, err
}

// A level that fired more times than are read: how many went past its
// bound isn't told from part of them.
func TestUnrecordedMoreThanRead(t *testing.T) {
	versions, rep, _ := restartsWeek(t, nil)
	versions[0].Rules[0].Expr = "level > 0.5"
	us, err := Unrecorded(context.Background(), restarts{}, versions, rep, more{Judged(rep.Firings)}, time.Minute)
	if err != nil || len(us) != 1 || us[0].Clear != nil || us[0].Alerts != 3 {
		t.Errorf("%+v %v", us, err)
	}
}

// grafanaRestarts answers the restart rule's query as a Grafana rule asks
// it, the count alone: as restarts has it where it is more than none, and
// none elsewhere, each pod's series there all along as a counter's increase
// is; with gaps, no series at all where there was no restart, which Grafana
// takes as no data.
type grafanaRestarts struct{ gaps bool }

func (g grafanaRestarts) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if strings.Contains(expr, ">") {
		return nil, nil
	}
	ss, err := restarts{}.QueryRange(ctx, expr+" > 0", start, end, step)
	if g.gaps || err != nil {
		return ss, err
	}
	out := make([]prom.Series, 0, 3)
	for _, pod := range []string{"a", "b", "c"} {
		at := map[int64]float64{}
		for _, s := range ss {
			if s.Labels["pod"] == pod {
				for _, smp := range s.Samples {
					at[smp.T.Unix()] = smp.V
				}
			}
		}
		s := prom.Series{Labels: map[string]string{"pod": pod}}
		for t := start; !t.After(end); t = t.Add(step) {
			s.Samples = append(s.Samples, prom.Sample{T: t, V: at[t.Unix()]})
		}
		out = append(out, s)
	}
	return out, nil
}

// grafanaRule is a rule as Grafana provisions it: an instant query of expr,
// a reduce (last) and a threshold (> 0.5), for 1m.
func grafanaRule(t *testing.T, expr string) rules.Rule {
	t.Helper()
	rs, err := rules.Parse([]byte(`apiVersion: 1
groups:
  - orgId: 1
    name: pods
    folder: Ops
    interval: 1m
    rules:
      - uid: restarts
        title: Restarts
        condition: C
        for: 1m
        keepFiringFor: 10m
        labels: {team: infra}
        data:
          - refId: A
            relativeTimeRange: {from: 1800, to: 0}
            datasourceUid: prom
            model: {expr: '` + expr + `', instant: true, refId: A}
          - refId: B
            datasourceUid: __expr__
            model: {type: reduce, reducer: last, expression: A, refId: B}
          - refId: C
            datasourceUid: __expr__
            model: {type: threshold, expression: B, refId: C, conditions: [{evaluator: {type: gt, params: [0.5]}}]}
`))
	if err != nil || len(rs) != 1 || rs[0].Unsupported != "" {
		t.Fatalf("%v %+v", err, rs)
	}
	return rs[0]
}

// grafanaWeek is the week of restarts, with r replayed from h, judged
// against incs.
func grafanaWeek(t *testing.T, h replay.History, r rules.Rule, incs []incident.Incident) ([]replay.Version, *verdict.Report, verdict.Options) {
	t.Helper()
	o := verdict.Options{From: t0, To: t0.Add(7 * 24 * time.Hour), Lead: 15 * time.Minute, Match: []string{"service"},
		RefireWithin: time.Hour, UnknownLen: time.Hour}
	versions := []replay.Version{{Rules: []rules.Rule{r}}}
	known, fs := replay.Run(context.Background(), h, versions, o.From, o.To, time.Minute, 2)
	rep := verdict.Judge(known, fs, incs, o)
	if len(rep.Rules) != 1 || !rep.Rules[0].Replayed || rep.Rules[0].Firings < 30 {
		t.Fatalf("the restart rule: %+v", rep.Rules)
	}
	return versions, rep, o
}

// The same restart rule written in Grafana: never suggested a for of 30m
// or more, which one restart can't outlast, though a replay with one fires
// on none; and, with no incident written down, its firings are events.
func TestGrafanaRestarts(t *testing.T) {
	r := grafanaRule(t, `sum by (pod) (increase(restarts[30m]))`)
	if w, ok := r.EventWindow(); !ok || w != 30*time.Minute {
		t.Fatalf("window %s %v", w, ok)
	}
	h := grafanaRestarts{}
	away := []incident.Incident{{ID: "INC-1", Start: t0.Add(-24 * time.Hour), End: t0.Add(-23 * time.Hour), Labels: incident.Labels{"service": {"x"}}}}
	versions, rep, o := grafanaWeek(t, h, r, away)
	if !rep.Rules[0].Noisy || rep.Rules[0].Firings != 37 {
		t.Fatalf("the restart rule: %+v", rep.Rules[0])
	}
	// What a for of 30m would do: fire on no restart, so it would look
	// better, though it hides every one.
	alts, err := replay.WithFor(context.Background(), h, r, []time.Duration{30 * time.Minute}, o.From, o.To, time.Minute)
	if at, ok := alts[30*time.Minute]; err != nil || !ok || len(at) != 0 {
		t.Fatalf("for 30m: %v %v %d", err, ok, len(at))
	}
	if got := Suggest(context.Background(), h, versions[0].Rules, rep, away, o, time.Minute, nil); len(got) != 0 {
		t.Errorf("suggested %+v", got)
	}

	versions, rep, _ = grafanaWeek(t, h, r, nil)
	us, err := Unrecorded(context.Background(), h, versions, rep, Judged(rep.Firings), time.Minute)
	if err != nil || len(us) != 1 || !us[0].Events || us[0].Firings != 37 || us[0].Clear == nil || *us[0].Clear != 37 || us[0].Bound != "" {
		t.Fatalf("%+v %v", us, err)
	}
	if !strings.Contains(us[0].Text(), "All 37 are events its condition counts (a count that went up)") {
		t.Errorf("%q", us[0].Text())
	}
}

// A Grafana rule fires on no data too, and such a firing is no event, nor
// went past any bound: with one among them, that they all are events isn't
// said; of a level, those on no data aren't counted as gone past its bound.
func TestGrafanaNoData(t *testing.T) {
	h := grafanaRestarts{gaps: true}
	r := grafanaRule(t, `sum by (pod) (increase(restarts[30m]))`)
	versions, rep, _ := grafanaWeek(t, h, r, nil)
	nodata := 0
	for _, f := range rep.Firings {
		if f.Labels[replay.StateLabel] == replay.NoDataState {
			nodata++
		}
	}
	if nodata == 0 {
		t.Fatal("no firing on no data")
	}
	us, err := Unrecorded(context.Background(), h, versions, rep, Judged(rep.Firings), time.Minute)
	if err != nil || len(us) != 1 || us[0].Events || us[0].Clear != nil {
		t.Fatalf("%+v %v", us, err)
	}
	if text := us[0].Text(); strings.Contains(text, "are events") || strings.Contains(text, "would hide") {
		t.Errorf("%q", text)
	}
	// More than are read: not known.
	us, err = Unrecorded(context.Background(), h, versions, rep, more{Judged(rep.Firings)}, time.Minute)
	if err != nil || len(us) != 1 || us[0].Events || us[0].Clear != nil {
		t.Fatalf("more: %+v %v", us, err)
	}

	// A level: of its 36 at 1, all went past 0.6; the flicker at 0.55,
	// and those on no data, didn't.
	r = grafanaRule(t, `level`)
	if _, ok := r.EventWindow(); ok {
		t.Fatal("a level counts events")
	}
	versions, rep, _ = grafanaWeek(t, h, r, nil)
	us, err = Unrecorded(context.Background(), h, versions, rep, Judged(rep.Firings), time.Minute)
	if err != nil || len(us) != 1 || us[0].Events || us[0].Clear == nil || *us[0].Clear != 36 || us[0].Firings <= 37 || us[0].Bound != "> 0.5" {
		t.Fatalf("a level: %+v %v", us, err)
	}
}
