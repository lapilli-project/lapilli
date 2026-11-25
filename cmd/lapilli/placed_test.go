package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
)

// placedHistory is a Prometheus whose group "web" runs every five minutes,
// 2m30s into them, and whose condition "down" held at one evaluation, the
// last before the period, and not at the next: a firing on when the period
// starts that ends before the group's first evaluation in it. Its rules API
// says where the group runs, by its last evaluation; rulesErr, when it is
// not nil, is what the API answers with instead.
type placedHistory struct {
	from     time.Time
	rulesErr error
	asked    *[]string
}

func (h placedHistory) QueryRange(_ context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if expr != "down" {
		return nil, nil
	}
	s := prom.Series{Labels: map[string]string{"job": "web"}}
	for t := start; !t.After(end); t = t.Add(step) {
		// Holds from 23:57:00 to 23:59:00, before the period.
		if !t.Before(h.from.Add(-3*time.Minute)) && t.Before(h.from.Add(-time.Minute)) {
			s.Samples = append(s.Samples, prom.Sample{T: t, V: 1})
		}
	}
	return []prom.Series{s}, nil
}

func (h placedHistory) Rules(context.Context) ([]byte, error) {
	if h.asked != nil {
		*h.asked = append(*h.asked, "rules")
	}
	if h.rulesErr != nil {
		return nil, h.rulesErr
	}
	// Last evaluated at 12:32:30, 2m30s into its five minutes.
	return []byte(`{"status":"success","data":{"groups":[
	  {"name":"web","file":"/etc/rules/web.yml","interval":300,"lastEvaluation":"2026-11-01T12:32:30.01Z","rules":[]},
	  {"name":"twice","file":"/etc/rules/a.yml","interval":300,"lastEvaluation":"2026-11-01T12:31:00Z","rules":[]},
	  {"name":"twice","file":"/etc/rules/b.yml","interval":300,"lastEvaluation":"2026-11-01T12:31:00Z","rules":[]}]}}`), nil
}

func TestVerdictPlacesRuleFilesByTheRulesAPI(t *testing.T) {
	alone(t)
	from := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	var asked []string
	h := placedHistory{from: from, asked: &asked}
	newHistory = func(context.Context, *verdictFlags) (history, error) { return h, nil }
	now = func() time.Time { return from.Add(time.Hour) }
	files := write(t, "rules.yaml", `groups:
  - name: web
    interval: 5m
    rules:
      - alert: WebDown
        expr: down
  - name: twice
    interval: 5m
    rules:
      - alert: TwiceDown
        expr: down
  - name: ungiven
    rules:
      - alert: UngivenDown
        expr: down
`)
	incidents := write(t, "incidents.yaml", "incidents:\n  - {id: INC-1, title: Web down, start: 2026-11-01T23:00:00Z, end: 2026-11-02T01:00:00Z}\n")
	run1 := func(h placedHistory) (string, string) {
		newHistory = func(context.Context, *verdictFlags) (history, error) { return h, nil }
		var stdout, stderr bytes.Buffer
		args := []string{"verdict", "--rules", files, "--prometheus", "http://p", "--incidents", incidents, "--tz", "UTC", "--json",
			"--from", "2026-11-02T00:00:00Z", "--to", "2026-11-02T01:00:00Z"}
		if code := run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("exit %d\n%s", code, stderr.String())
		}
		return stdout.String(), stderr.String()
	}
	firings := func(out string) map[string]int {
		var r struct {
			Rules []struct {
				Rule    string
				Firings int
			}
		}
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatal(err)
		}
		m := map[string]int{}
		for _, x := range r.Rules {
			m[x.Rule] = x.Firings
		}
		return m
	}
	out, said := run1(h)
	// WebDown, placed at 2m30s, fired at the evaluation before the period
	// and resolved at its first, 00:02:30; the others, on the grid, didn't.
	if got := firings(out); got["WebDown"] != 1 || got["TwiceDown"] != 0 || got["UngivenDown"] != 0 {
		t.Errorf("firings %v: want WebDown's from before the period, and none of the groups not placed", got)
	}
	for _, want := range []string{"1 group placed", `"twice"`, "lists 2 groups of that name", `"ungiven"`, "no group of that name"} {
		if !strings.Contains(said, want) {
			t.Errorf("stderr lacks %q:\n%s", want, said)
		}
	}
	if n := strings.Count(strings.Join(asked, ","), "rules"); n != 1 {
		t.Errorf("the rules API was asked %d times, want once", n)
	}
	// No rules API: the run goes on, says so, and places nothing.
	out, said = run1(placedHistory{from: from, rulesErr: errors.New("404 page not found")})
	if firings(out)["WebDown"] != 0 || !strings.Contains(said, "rules API couldn't be read") || !strings.Contains(said, "steps' grid") {
		t.Errorf("without the API:\n%s\n%s", out, said)
	}
}

// Where every group of the rule files gives no interval, the rules API is
// asked all the same: it says each group's interval, and where it runs.
func TestVerdictPlacesGroupsWithNoInterval(t *testing.T) {
	alone(t)
	from := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	var asked []string
	newHistory = func(context.Context, *verdictFlags) (history, error) {
		return placedHistory{from: from, asked: &asked}, nil
	}
	now = func() time.Time { return from.Add(time.Hour) }
	files := write(t, "rules.yaml", "groups:\n  - name: web\n    rules:\n      - alert: WebDown\n        expr: down\n")
	incidents := write(t, "incidents.yaml", "incidents:\n  - {id: INC-1, title: Web down, start: 2026-11-01T23:00:00Z, end: 2026-11-02T01:00:00Z}\n")
	var stdout, stderr bytes.Buffer
	args := []string{"verdict", "--rules", files, "--prometheus", "http://p", "--incidents", incidents, "--tz", "UTC", "--json",
		"--from", "2026-11-02T00:00:00Z", "--to", "2026-11-02T01:00:00Z"}
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	if len(asked) != 1 || !strings.Contains(stderr.String(), "1 group placed") {
		t.Errorf("asked %v\n%s", asked, stderr.String())
	}
}
