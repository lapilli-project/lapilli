package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
)

const notifDir = "testdata/notifications"

// notifArgs is a week of firings read from a file, judged against one
// incident, with the alert manager's configuration, the labels Prometheus
// adds, a silence and the working hours: who each firing reached, worked
// out.
func notifArgs() []string {
	return []string{"verdict", "--firings", filepath.Join(notifDir, "firings.jsonl"), "--incidents", filepath.Join(notifDir, "incidents.yaml"),
		"--from", "2026-10-05T00:00:00Z", "--to", "2026-10-12T00:00:00Z", "--match", "namespace", "--tz", "UTC",
		"--alertmanager-config", filepath.Join(notifDir, "alertmanager.yml"), "--external-labels", "cluster=prod",
		"--silences", filepath.Join(notifDir, "silences.json"), "--work-hours", "mon-fri@08:00-18:00"}
}

func golden(t *testing.T, path, got string) {
	t.Helper()
	if os.Getenv("LAPILLI_GOLDEN") == "update" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s differs:\n%s\nwant\n%s", path, got, want)
	}
}

// The report with the notifications worked out, as text, --json, --csv and
// --metrics (golden, with LAPILLI_GOLDEN=update): a rule that pages, one an
// inhibition rule mutes, one a silence mutes, and one that reaches a
// receiver that sends nothing.
func TestVerdictNotifications(t *testing.T) {
	alone(t)
	now = func() time.Time { return time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC) }
	for _, out := range []struct{ file, flag string }{{"report.txt", ""}, {"report.json", "--json"}, {"report.csv", "--csv"}, {"report.prom", "--metrics"}} {
		args := notifArgs()
		if out.flag != "" {
			args = append(args, out.flag)
		}
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("%s: exit %d\n%s", out.file, code, stderr.String())
		}
		golden(t, filepath.Join(notifDir, out.file), stripRunTime(stdout.String()))
	}
}

// --redact gives each receiver, inhibition rule and silence an alias.
func TestVerdictNotificationsRedact(t *testing.T) {
	alone(t)
	var stdout, stderr bytes.Buffer
	if code := run(append(notifArgs(), "--json", "--redact"), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	for _, name := range []string{`"oncall"`, `"cart-team"`, `"slack-default"`, `"nobody"`, "api-errors-hide-disk", "b1c2", "ApiErrors", "DiskFull"} {
		if strings.Contains(stdout.String(), name) {
			t.Errorf("--redact left %s in the report", name)
		}
	}
	for _, want := range []string{`"receiver-`, `"inhibition-`, `"silence-`, `"worked_out": true`} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("--redact: no %s in\n%s", want, stdout.String())
		}
	}
}

// With the configuration given and --notifications=false, or with none, a
// report is what it was before the notifications came, byte for byte: the
// goldens of testdata/withoutresponses, written by lapilli verdict at
// origin/main (see TestVerdictWithoutResponses), are what both write for the
// same firings and incidents, as text, --json, --csv and --metrics.
func TestVerdictNotificationsOff(t *testing.T) {
	alone(t)
	dir := filepath.Join("testdata", "withoutresponses")
	for _, extra := range [][]string{
		{"--alertmanager-config", filepath.Join(notifDir, "alertmanager.yml"), "--notifications=false"},
		{"--alertmanager-config", filepath.Join(notifDir, "alertmanager.yml"), "--notifications=false", "--silences", filepath.Join(notifDir, "silences.json")},
		{},
	} {
		for _, out := range []struct{ file, flag string }{{"report.txt", ""}, {"report.json", "--json"}, {"report.csv", "--csv"}, {"report.prom", "--metrics"}} {
			args := []string{"verdict", "--firings", filepath.Join(dir, "firings.jsonl"), "--incidents", filepath.Join(dir, "incidents.yaml"),
				"--from", "2026-10-05T00:00:00Z", "--to", "2026-10-12T00:00:00Z", "--match", "namespace", "--tz", "UTC"}
			args = append(args, extra...)
			if out.flag != "" {
				args = append(args, out.flag)
			}
			var stdout, stderr bytes.Buffer
			// --silences with --notifications=false is a flag given for nothing,
			// taken, as every flag of what a run is told to leave out is.
			if code := run(args, &stdout, &stderr); code != 0 {
				t.Fatalf("%v %s: exit %d\n%s", extra, out.file, code, stderr.String())
			}
			want, err := os.ReadFile(filepath.Join(dir, out.file))
			if err != nil {
				t.Fatal(err)
			}
			if got := stripRunTime(stdout.String()); got != stripRunTime(string(want)) {
				t.Errorf("%v %s differs from what origin/main wrote:\n%s\nwant\n%s", extra, out.file, got, want)
			}
		}
	}
}

// The flags: what they take, what they go with.
func TestNotificationsFlags(t *testing.T) {
	parallel(t)
	am := filepath.Join(notifDir, "alertmanager.yml")
	base := []string{"--firings", "f.jsonl", "--incidents", "i.yaml"}
	for _, c := range []struct {
		args []string
		want string // "" for accepted
	}{
		{[]string{"--alertmanager-config", am}, ""},
		{[]string{"--alertmanager-config", am, "--notifications=false"}, ""},
		{[]string{"--alertmanager-config", am, "--notifications=false", "--silences", "s.json"}, ""},
		{[]string{"--notifications"}, "give --alertmanager-config, or --grafana"},
		{[]string{"--external-labels", "cluster=prod"}, "goes with --alertmanager-config, or --grafana"},
		{[]string{"--silences", "s.json"}, "goes with --alertmanager-config, or --grafana"},
		{[]string{"--paging-receivers", "^oncall"}, "goes with --alertmanager-config, or --grafana"},
		{[]string{"--alertmanager-config", am, "--external-labels", "cluster=prod", "--external-labels", "env=prod", "--silences", "s.json", "--paging-receivers", "^oncall"}, ""},
		{[]string{"--alertmanager-config", am, "--external-labels", "cluster"}, "want NAME=VALUE"},
		{[]string{"--alertmanager-config", am, "--external-labels", "a b=c"}, "want NAME=VALUE"},
		{[]string{"--alertmanager-config", am, "--external-labels", "a=b", "--external-labels", "a=c"}, "a is given twice"},
		{[]string{"--alertmanager-config", am, "--paging-receivers", "("}, "want a regular expression"},
		{[]string{"--grafana", "http://g", "--prometheus", "http://p", "--external-labels", "cluster=prod"}, ""},
		{[]string{"--grafana", "http://g", "--prometheus", "http://p", "--notifications=false"}, ""},
	} {
		_, err := parseVerdictFlags(append(append([]string{}, base...), c.args...))
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%v: %v", c.args, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%v: %v, want %q", c.args, err, c.want)
		}
	}
	// The content check and the replay check work out nothing.
	for _, args := range [][]string{
		{"--check-content", "--rules", "r.yaml", "--alertmanager-config", am, "--silences", "s.json"},
		{"--check-content", "--rules", "r.yaml", "--notifications"},
	} {
		if _, err := parseVerdictFlags(args); err == nil || !strings.Contains(err.Error(), "works out who a firing reached") {
			t.Errorf("%v: %v", args, err)
		}
	}
	// Silences that aren't silences, or a configuration without its route.
	var stdout, stderr bytes.Buffer
	bad := write(t, "silences.json", `{"not":"a list"}`)
	if code := run(append(notifArgs()[:len(notifArgs())-2:len(notifArgs())-2], "--silences", bad), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "--silences") {
		t.Errorf("silences: exit %d: %s", code, stderr.String())
	}
	stderr.Reset()
	noRoute := write(t, "am.yml", "receivers:\n  - name: x\n")
	args := append([]string{}, notifArgs()...)
	for i, a := range args {
		if a == filepath.Join(notifDir, "alertmanager.yml") {
			args[i] = noRoute
		}
	}
	if code := run(args, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "--alertmanager-config") {
		t.Errorf("no route: exit %d: %s", code, stderr.String())
	}
}

// routeHistory answers the two expressions the rules of the compare test
// read: pa is true on day 1 from 10:00 to 10:20, pb on day 6 from 03:00 to
// 03:30.
type routeHistory struct{}

func (routeHistory) QueryRange(_ context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	var from, to time.Time
	switch expr {
	case "pa":
		from, to = at(1, 10, 0), at(1, 10, 20)
	case "pb":
		from, to = at(6, 3, 0), at(6, 3, 30)
	default:
		return nil, nil
	}
	s := prom.Series{Labels: map[string]string{"namespace": "shop"}}
	for t := start; !t.After(end); t = t.Add(step) {
		if !t.Before(from) && !t.After(to) {
			s.Samples = append(s.Samples, prom.Sample{T: t, V: 1})
		}
	}
	return []prom.Series{s}, nil
}

const routeRulesBefore = `groups:
  - name: g
    interval: 1m
    rules:
      - alert: ApiErrors
        expr: pa
        labels: {severity: critical}
`

const routeRulesAfter = routeRulesBefore + `      - alert: NightJob
        expr: pb
        labels: {severity: critical}
`

// --compare: the pages before and after, the first line of the summary, in
// the text, the Markdown (the comment) and the summary's JSON, with PAGED for
// each rule that differs; a change that adds a rule that pages says so
// (goldens, with LAPILLI_GOLDEN=update).
func TestCompareNotifications(t *testing.T) {
	alone(t)
	newHistory = func(context.Context, *verdictFlags) (history, error) { return routeHistory{}, nil }
	now = func() time.Time { return at(8, 0, 0) }
	before, after := write(t, "before.yaml", routeRulesBefore), write(t, "after.yaml", routeRulesAfter)
	args := []string{"verdict", "--rules", after, "--compare", before, "--prometheus", "http://p", "--from", ts(sigBase), "--to", ts(sigBase.Add(7 * 24 * time.Hour)), "--tz", "UTC",
		"--alertmanager-config", filepath.Join(notifDir, "alertmanager.yml"), "--external-labels", "cluster=prod", "--work-hours", "mon-fri@08:00-18:00"}
	for _, out := range []struct{ file, flag string }{{"compare.txt", ""}, {"compare.md", "--markdown"}, {"compare.json", "--json"}} {
		a := append([]string{}, args...)
		if out.flag != "" {
			a = append(a, out.flag)
		}
		var stdout, stderr bytes.Buffer
		if code := run(a, &stdout, &stderr); code != 0 {
			t.Fatalf("%s: exit %d\n%s", out.file, code, stderr.String())
		}
		golden(t, filepath.Join(notifDir, out.file), stdout.String())
	}
	// And a budget of pages: ApiErrors sent two before, the new rule's pages
	// are over a budget of none.
	exp := write(t, "expect.yaml", "expectations:\n  - rule: NightJob\n    budget: {pages: 0}\n  - rule: ApiErrors\n    budget: {pages: 5}\n")
	var stdout, stderr bytes.Buffer
	if code := run(append(append([]string{}, args...), "--expect", exp, "--fail-expectations"), &stdout, &stderr); code != 1 ||
		!strings.Contains(stdout.String(), "at most 0 pages a week") || !strings.Contains(stdout.String(), "broken by this change") {
		t.Errorf("pages budget: exit %d\n%s\n%s", code, stdout.String(), stderr.String())
	}
	// Without the configuration, the same comparison has no pages at all.
	stdout.Reset()
	plain := append([]string{}, args[:len(args)-6]...)
	if code := run(append(plain, "--markdown"), &stdout, &stderr); code != 0 {
		t.Fatalf("plain: exit %d\n%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "Pages") || strings.Contains(stdout.String(), "notifications") || strings.Contains(stdout.String(), "Paged") {
		t.Errorf("a comparison without the configuration mentions pages:\n%s", stdout.String())
	}
}
