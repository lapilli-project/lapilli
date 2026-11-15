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
// and the times it was asked over.
type impactHistory struct {
	restartHistory
	asked *[]time.Time
}

func (h impactHistory) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if expr == "impact" {
		*h.asked = append(*h.asked, start)
		return nil, nil
	}
	return h.restartHistory.QueryRange(ctx, expr, start, end, step)
}

// With --impact read over the period, and the two weeks before it, its
// record is kept, found or not: users weren't hurt, as far as it can say, so
// the restarts are judged, and noisy; a for one restart can't outlast is
// never suggested all the same.
func TestVerdictImpactKeepsRecord(t *testing.T) {
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
