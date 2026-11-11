package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
)

// lokiHistory answers LogQL as fakeHistory answers PromQL: the shop's
// errors, counted in its logs, from 3h to 2h before t0.
type lokiHistory struct{ asked *[]string }

func (l lokiHistory) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	*l.asked = append(*l.asked, expr)
	if expr != `sum by (service) (count_over_time({app="checkout"} |= "error" [5m])) > 5` {
		return nil, nil
	}
	s := prom.Series{Labels: map[string]string{"service": "checkout"}}
	for t := t0.Add(-3 * time.Hour); !t.After(t0.Add(-2 * time.Hour)); t = t.Add(step) {
		if !t.Before(start) && !t.After(end) {
			s.Samples = append(s.Samples, prom.Sample{T: t, V: 9})
		}
	}
	return []prom.Series{s}, nil
}

// TestVerdictLokiRules: a Loki ruler's rules, replayed against --loki
// beside the Prometheus rules, are judged alike; their LogQL is never
// asked of Prometheus.
func TestVerdictLokiRules(t *testing.T) {
	var asked []string
	newHistory = func(context.Context, *verdictFlags) (history, error) { return fakeHistory{}, nil }
	newLoki = func(*verdictFlags) history { return lokiHistory{&asked} }
	defer func() {
		newLoki = func(f *verdictFlags) history { return f.lokiSource().History("lapilli/" + buildVersion()) }
	}()
	now = func() time.Time { return t0 }
	promRules := write(t, "rules.yaml", "groups:\n  - name: shop\n    rules:\n      - alert: CheckoutErrors\n        expr: errors > 5\n")
	lokiRules := write(t, "loki.yaml", `groups:
  - name: logs
    rules:
      - alert: CheckoutLogErrors
        expr: sum by (service) (count_over_time({app="checkout"} |= "error" [5m])) > 5
        for: 10m
`)
	incidents := write(t, "incidents.yaml", "incidents:\n  - {id: INC-7, title: Checkout down, start: 2026-11-01T21:00:00Z, end: 2026-11-01T22:00:00Z}\n")
	var stdout, stderr bytes.Buffer
	args := []string{"verdict", "--rules", promRules, "--loki-rules", lokiRules, "--loki", "http://loki:3100", "--prometheus", "http://p",
		"--incidents", incidents, "--tz", "UTC", "--json"}
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, `"rule": "CheckoutLogErrors"`) || !strings.Contains(out, `"rule": "CheckoutErrors"`) ||
		!strings.Contains(out, `"start": "2026-11-01T21:10:00Z"`) {
		t.Errorf("both rules judged, the Loki one firing after its for:\n%s", out)
	}
	if len(asked) == 0 || strings.Contains(strings.Join(asked, "\n"), "errors > 5\n") {
		t.Errorf("Loki asked %q", asked)
	}
	// Alone, without --prometheus.
	stdout.Reset()
	if code := run([]string{"verdict", "--loki-rules", lokiRules, "--loki", "http://loki:3100", "--incidents", incidents, "--tz", "UTC"}, &stdout, &stderr); code != 0 ||
		!strings.Contains(stdout.String(), "CheckoutLogErrors") {
		t.Errorf("Loki's rules alone: exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
}

func TestLokiFlags(t *testing.T) {
	rulesFile := write(t, "loki.yaml", "groups: []\n")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--loki-rules", rulesFile, "--incidents", rulesFile}, "add --loki URL"},
		{[]string{"--loki-loaded-rules", "--incidents", rulesFile}, "add --loki URL"},
		{[]string{"--rules", rulesFile, "--prometheus", "http://p", "--loki-tenant", "a", "--incidents", rulesFile}, "go with --loki"},
		{[]string{"--loki", "loki:3100", "--loki-loaded-rules", "--incidents", rulesFile}, "want Loki's URL"},
		{[]string{"--loki", "http://l", "--loki-rules", rulesFile, "--loki-loaded-rules", "--incidents", rulesFile}, "give one"},
		{[]string{"--loki", "http://l", "--loki-rules", rulesFile, "--alerts", "--prometheus", "http://p", "--incidents", rulesFile}, "don't go with --alerts"},
		{[]string{"--loki", "http://l", "--loki-rules", rulesFile, "--rules", rulesFile, "--compare", rulesFile, "--prometheus", "http://p", "--incidents", rulesFile}, "leave out --loki-rules"},
		{[]string{"--loki", "http://l", "--loki-rules", rulesFile, "--check-replay"}, "add --prometheus"},
	} {
		if _, err := parseVerdictFlags(c.args); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want %q", c.args, err, c.want)
		}
	}
	// lapilli serve takes one directory.
	if _, err := parseFlags([]string{"--prometheus", "http://p", "--loki", "http://l", "--loki-rules", rulesFile, "--loki-rules", rulesFile},
		func(string, string) (bool, error) { return false, nil }); err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Errorf("serve, two --loki-rules: %v", err)
	}
}

// TestLokiPlainHTTP: a token sent to --loki over plain http is said, as
// --grafana's is; over https, or with no token, nothing is.
func TestLokiPlainHTTP(t *testing.T) {
	newHistory = func(context.Context, *verdictFlags) (history, error) { return fakeHistory{}, nil }
	var asked []string
	newLoki = func(*verdictFlags) history { return lokiHistory{&asked} }
	defer func() {
		newLoki = func(f *verdictFlags) history { return f.lokiSource().History("lapilli/" + buildVersion()) }
	}()
	now = func() time.Time { return t0 }
	lokiRules := write(t, "loki.yaml", "groups:\n  - name: logs\n    rules:\n      - alert: A\n        expr: sum(count_over_time({app=\"x\"}[5m])) > 5\n")
	incidents := write(t, "incidents.yaml", "incidents: []\n")
	token := write(t, "token", "secret\n")
	for url, warned := range map[string]bool{"http://loki:3100": true, "https://loki.example.com": false} {
		var stdout, stderr bytes.Buffer
		run([]string{"verdict", "--loki-rules", lokiRules, "--loki", url, "--loki-token-file", token, "--incidents", incidents}, &stdout, &stderr)
		if got := strings.Contains(stderr.String(), "--loki is plain http: the token goes unencrypted"); got != warned || strings.Contains(stderr.String(), "secret") {
			t.Errorf("%s: %q", url, stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	run([]string{"verdict", "--loki-rules", lokiRules, "--loki", "http://loki:3100", "--incidents", incidents}, &stdout, &stderr)
	if strings.Contains(stderr.String(), "plain http") {
		t.Errorf("no token, yet: %q", stderr.String())
	}
}
