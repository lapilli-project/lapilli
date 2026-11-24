package main

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/verdict"
)

// restartHistory answers increase(restarts[30m]) compared with a number:
// each restart holds the count at 1 for 30 minutes, every two hours over
// the day before t0, in three namespaces in turn.
type restartHistory struct{}

var comparedWith = regexp.MustCompile(`>\s*([0-9.]+)\s*$`)

func (restartHistory) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	m := comparedWith.FindStringSubmatch(expr)
	if m == nil || !strings.Contains(expr, "restarts") {
		return nil, nil
	}
	if bound, _ := strconv.ParseFloat(m[1], 64); bound >= 1 {
		return nil, nil
	}
	by := map[string]*prom.Series{}
	for k := range 12 {
		ns := []string{"web", "api", "jobs"}[k%3]
		at := t0.Add(-24*time.Hour + time.Duration(k)*2*time.Hour)
		s := by[ns]
		if s == nil {
			s = &prom.Series{Labels: map[string]string{"namespace": ns}}
			by[ns] = s
		}
		for t := at; t.Before(at.Add(30 * time.Minute)); t = t.Add(step) {
			if !t.Before(start) && !t.After(end) {
				s.Samples = append(s.Samples, prom.Sample{T: t, V: 1})
			}
		}
	}
	var out []prom.Series
	for _, ns := range []string{"web", "api", "jobs"} {
		if len(by[ns].Samples) > 0 {
			out = append(out, *by[ns])
		}
	}
	return out, nil
}

// TestVerdictNoRecord: a day of restarts, and no incident written down in
// it or in the two weeks before: lapilli verdict says once that the firings
// can't be judged, calls nothing noisy and suggests no longer "for", says
// what the restart rule's firings were, and that a for of 1h is one a
// restart can't outlast.
func TestVerdictNoRecord(t *testing.T) {
	alone(t)
	newHistory = func(context.Context, *verdictFlags) (history, error) { return restartHistory{}, nil }
	now = func() time.Time { return t0 }
	rulesFile := write(t, "rules.yaml", `groups:
  - name: pods
    rules:
      - alert: Restarts
        expr: increase(restarts[30m]) > 0.5
        for: 1m
        keep_firing_for: 10m
        labels: {team: infra}
      - alert: RestartsForAnHour
        expr: increase(restarts[30m]) > 0.5
        for: 1h
        labels: {team: infra}
`)
	incidents := write(t, "incidents.yaml", `incidents:
  - {id: INC-1, title: Long ago, start: 2026-09-01T10:00:00Z, end: 2026-09-01T11:00:00Z}
`)
	args := []string{"verdict", "--rules", rulesFile, "--prometheus", "http://p", "--incidents", incidents, "--tz", "UTC", "--since", "2d"}
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout.String(), stderr.String())
	}
	out := stdout.String()
	t.Logf("\n%s", out)
	for _, want := range []string{
		verdict.NoRecordText,
		"Restarts: Fired 12 times with no incident written down to judge them by. All 12 are events its condition counts (a count that went up): these happened. Most came from: namespace=api 4, namespace=jobs 4, namespace=web 4. A longer \"for\" would hide them: write down whether they mattered, or fix what fires them.",
		"of the firings, with no incident written down to judge them by, 5 came while people slept, and 7 more off hours.",
		"RestartsForAnHour: Its for (1h) is as long as the 30m its condition counts over, or longer: one event alone can never fire it; only events that keep coming for 1h will.",
	} {
		if strings.Count(out, want) != 1 {
			t.Errorf("not said once: %q", want)
		}
	}
	if strings.Contains(out, "Noisy rules that a longer") || strings.Contains(out, "0%") {
		t.Errorf("judged:\n%s", out)
	}

	stdout.Reset()
	if code := run(append(args, "--json"), &stdout, &stderr); code != 0 {
		t.Fatalf("--json: exit %d\n%s", code, stderr.String())
	}
	var rep verdict.Report
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.NoRecord || len(rep.Unrecorded) != 1 || rep.Unrecorded[0].Clear == nil || *rep.Unrecorded[0].Clear != 12 || len(rep.Suggestions) != 0 {
		t.Errorf("--json: %+v %+v", rep.NoRecord, rep.Unrecorded)
	}
	for _, r := range rep.Rules {
		if r.Noisy || r.Rule == "Restarts" && !r.NoRecord || r.Rule == "RestartsForAnHour" && (r.Outlasts == nil || r.Outlasts.Window != 30*time.Minute) {
			t.Errorf("--json %s: %+v", r.Rule, r)
		}
	}

	// --check-content: which rules are noisy can't be said.
	stdout.Reset()
	if code := run([]string{"verdict", "--rules", rulesFile, "--prometheus", "http://p", "--incidents", incidents, "--since", "2d", "--check-content"}, &stdout, &stderr); code != 0 ||
		!strings.Contains(stdout.String(), verdict.NoRecordText) || strings.Contains(stdout.String(), "Measured over") {
		t.Errorf("--check-content: exit %d\n%s", code, stdout.String())
	}
	stdout.Reset()
	if code := run([]string{"verdict", "--rules", rulesFile, "--prometheus", "http://p", "--incidents", incidents, "--since", "2d", "--check-content", "--json"}, &stdout, &stderr); code != 0 ||
		!strings.Contains(stdout.String(), `"noisy_measured": false`) || !strings.Contains(stdout.String(), `"no_record": true`) {
		t.Errorf("--check-content --json: exit %d\n%s", code, stdout.String())
	}
}

// impactHistory is restartHistory, with an impact signal that found nothing,
// and the times it was asked over; its data, the series it reads, begins
// at begins (zero: all along), or nowhere (noData).
type impactHistory struct {
	restartHistory
	asked  *[]time.Time
	begins time.Time
	noData bool
}

func (h impactHistory) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	switch expr {
	case "impact":
		*h.asked = append(*h.asked, start)
		return nil, nil
	case "count(impact)":
		s := prom.Series{Labels: map[string]string{}}
		for at := start; !at.After(end) && !h.noData; at = at.Add(step) {
			if !at.Before(h.begins) {
				s.Samples = append(s.Samples, prom.Sample{T: at, V: 1})
			}
		}
		if len(s.Samples) == 0 {
			return nil, nil
		}
		return []prom.Series{s}, nil
	}
	return h.restartHistory.QueryRange(ctx, expr, start, end, step)
}

// With --impact read over the period, and the two weeks before it, its
// record is kept, found or not: users weren't hurt, as far as it can say, so
// the restarts are judged, and noisy; a for one restart can't outlast is
// never suggested all the same.
func TestVerdictImpactKeepsRecord(t *testing.T) {
	alone(t)
	var asked []time.Time
	newHistory = func(context.Context, *verdictFlags) (history, error) { return impactHistory{asked: &asked}, nil }
	now = func() time.Time { return t0 }
	rulesFile := write(t, "rules.yaml", `groups:
  - name: pods
    rules:
      - alert: Restarts
        expr: increase(restarts[30m]) > 0.5
        for: 1m
`)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"verdict", "--rules", rulesFile, "--prometheus", "http://p", "--impact", "impact", "--since", "2d", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	var rep verdict.Report
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.NoRecord || rep.Unjudged != 0 || !rep.Rules[0].Noisy || len(rep.Unrecorded) != 0 {
		t.Errorf("%v %d %+v", rep.NoRecord, rep.Unjudged, rep.Rules[0])
	}
	for _, s := range rep.Suggestions {
		if s.NewFor >= 30*time.Minute {
			t.Errorf("suggested %+v", s)
		}
	}
	if len(asked) == 0 || !asked[0].Equal(t0.Add(-2*24*time.Hour-verdict.RecordLookback)) {
		t.Errorf("impact asked from %v, want two weeks before the period", asked)
	}
}

// An impact signal keeps the record from where the data it reads begins,
// as lapilli serve's does, not from where it was first read: before, a
// restart that caught nothing can't be told from one nobody saw, and is
// counted, not judged; and with no data, it keeps none.
func TestVerdictImpactDataBegins(t *testing.T) {
	alone(t)
	rulesFile := write(t, "rules.yaml", `groups:
  - name: pods
    rules:
      - alert: Restarts
        expr: increase(restarts[30m]) > 0.5
        for: 1m
`)
	now = func() time.Time { return t0 }
	verdictOf := func(h impactHistory, flag string) (verdict.Report, string) {
		t.Helper()
		var asked []time.Time
		h.asked = &asked
		newHistory = func(context.Context, *verdictFlags) (history, error) { return h, nil }
		var stdout, stderr bytes.Buffer
		args := []string{"verdict", "--rules", rulesFile, "--prometheus", "http://p", "--impact", "impact", "--since", "2d", "--tz", "UTC"}
		if flag != "" {
			args = append(args, flag)
		}
		if code := run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("exit %d\n%s", code, stderr.String())
		}
		var rep verdict.Report
		if flag == "--json" {
			if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
				t.Fatal(err)
			}
		}
		return rep, stdout.String()
	}

	begins := t0.Add(-12 * time.Hour) // the restarts are from a day before t0, two hours apart
	rep, _ := verdictOf(impactHistory{begins: begins}, "--json")
	if !rep.Settings.ImpactFrom.Equal(begins) || rep.Settings.ImpactNoData {
		t.Errorf("impact_from %v, no data %v, want %v", rep.Settings.ImpactFrom, rep.Settings.ImpactNoData, begins)
	}
	var before, after int
	for _, f := range rep.Firings {
		switch {
		case f.Start.Before(begins) && f.NoRecord:
			before++
		case !f.Start.Before(begins) && !f.NoRecord:
			after++
		default:
			t.Errorf("firing at %v: no record %v, the data beginning at %v", f.Start, f.NoRecord, begins)
		}
	}
	if before == 0 || after == 0 || rep.Unjudged != before || rep.NoRecord {
		t.Errorf("%d unjudged before, %d judged after, %d unjudged, no record %v", before, after, rep.Unjudged, rep.NoRecord)
	}
	if _, text := verdictOf(impactHistory{begins: begins}, ""); !strings.Contains(text, "--impact keeps the record from 2026-11-01 12:00, where the data of every selector it reads begins") {
		t.Errorf("text:\n%s", text)
	}

	// Data all along: the record from where it was first read, and nothing
	// said of it, as before.
	rep, _ = verdictOf(impactHistory{}, "--json")
	if !rep.Settings.ImpactFrom.Equal(t0.Add(-2*24*time.Hour-verdict.RecordLookback)) || rep.Unjudged != 0 {
		t.Errorf("all along: impact_from %v, %d unjudged", rep.Settings.ImpactFrom, rep.Unjudged)
	}
	if _, text := verdictOf(impactHistory{}, ""); strings.Contains(text, "--impact keeps") {
		t.Errorf("all along, said:\n%s", text)
	}

	// Beginning before the period, after where it was first read: every
	// firing judged, and nothing said but impact_from.
	early := t0.Add(-3 * 24 * time.Hour)
	rep, _ = verdictOf(impactHistory{begins: early}, "--json")
	if !rep.Settings.ImpactFrom.Equal(early) || rep.Unjudged != 0 || rep.NoRecord {
		t.Errorf("before the period: impact_from %v, %d unjudged, no record %v", rep.Settings.ImpactFrom, rep.Unjudged, rep.NoRecord)
	}
	if _, text := verdictOf(impactHistory{begins: early}, ""); strings.Contains(text, "--impact keeps") {
		t.Errorf("before the period, said:\n%s", text)
	}

	rep, _ = verdictOf(impactHistory{noData: true}, "--json")
	if !rep.Settings.ImpactFrom.IsZero() || !rep.Settings.ImpactNoData || !rep.NoRecord || rep.Unjudged != len(rep.Firings) || len(rep.Firings) == 0 {
		t.Errorf("no data: impact_from %v, no data %v, no record %v, %d of %d unjudged",
			rep.Settings.ImpactFrom, rep.Settings.ImpactNoData, rep.NoRecord, rep.Unjudged, len(rep.Firings))
	}
	if _, text := verdictOf(impactHistory{noData: true}, "--check-content"); !strings.Contains(text, verdict.NoRecordNoDataText) {
		t.Errorf("no data, --check-content:\n%s", text)
	}
	if _, text := verdictOf(impactHistory{noData: true}, ""); !strings.Contains(text, verdict.NoRecordNoDataText) || strings.Contains(text, verdict.NoRecordText) {
		t.Errorf("no data, text:\n%s", text)
	}
}

// ongoingImpact is restartHistory, with an impact signal on from three
// hours before the period, two days before t0, to t0: users hurt all along.
type ongoingImpact struct{ restartHistory }

func (h ongoingImpact) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if expr != "impact" {
		return h.restartHistory.QueryRange(ctx, expr, start, end, step)
	}
	s := prom.Series{Labels: map[string]string{}}
	for at := start; !at.After(end); at = at.Add(step) {
		if !at.Before(t0.Add(-2*24*time.Hour-3*time.Hour)) && at.Before(t0) {
			s.Samples = append(s.Samples, prom.Sample{T: at, V: 1})
		}
	}
	return []prom.Series{s}, nil
}

// An impact window already on at the period's start starts there, as when
// the period alone was read: the restarts in it catch it, as they did,
// though the signal is read from two weeks before for the record.
func TestVerdictImpactOngoing(t *testing.T) {
	alone(t)
	newHistory = func(context.Context, *verdictFlags) (history, error) { return ongoingImpact{}, nil }
	now = func() time.Time { return t0 }
	rulesFile := write(t, "rules.yaml", `groups:
  - name: pods
    rules:
      - alert: Restarts
        expr: increase(restarts[30m]) > 0.5
        for: 1m
`)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"verdict", "--rules", rulesFile, "--prometheus", "http://p", "--impact", "impact", "--since", "2d", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	var rep verdict.Report
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.NoRecord || len(rep.Rules) != 1 || rep.Rules[0].Firings != 12 || rep.Rules[0].White != 12 || rep.Rules[0].Noisy {
		t.Errorf("%v %+v", rep.NoRecord, rep.Rules)
	}
	if len(rep.Incidents) != 1 || !rep.Incidents[0].Start.Equal(t0.Add(-2*24*time.Hour)) {
		t.Errorf("the window, from the period's start: %+v", rep.Incidents)
	}
}
