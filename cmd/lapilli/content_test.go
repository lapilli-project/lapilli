package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/content"
)

const sayingRules = `groups:
  - name: shop
    rules:
      - alert: CheckoutErrors
        expr: flappy
        labels: {severity: page}
        annotations: {summary: "{{ $labels.service }} on {{ $labels.pod }}", runbook_url: "https://runbooks.example/x"}
      - alert: Quiet
        expr: sum by (service) (rate(x[30s])) > 1
        for: 5m
        annotations: {summary: "{{ $labels.instance"}
      - alert: Typo
        expr: up == 0
        for: 1m
        labels: {severity: pgae}
        annotations: {summary: "{{ $labels.instance }} down", runbook_url: "runbooks/down.md"}
`

// configured is a history that serves Prometheus's configuration too.
type configured struct{ fakeHistory }

func (configured) StatusConfig(context.Context) (string, error) {
	return "global:\n  scrape_interval: 1m\n", nil
}

func TestCheckContent(t *testing.T) {
	newHistory = func(context.Context, *verdictFlags) (history, error) { return configured{}, nil }
	now = func() time.Time { return t0 }
	rulesFile := write(t, "rules.yaml", sayingRules)

	// The rules alone: facts and guesses; nothing measured.
	var stdout, stderr bytes.Buffer
	if code := run([]string{"verdict", "--rules", rulesFile, "--check-content"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"What 3 rules say: 4 problems in 2 rules, 0 guesses apart.", "Not measured", "The range check is off",
		"Quiet  fact  no runbook link", "line 1: unclosed action",
		"Typo   fact  a runbook link that isn't an http or https URL  runbooks/down.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "noisy, with no for") || strings.Contains(out, "no firing had") {
		t.Errorf("measured with no history:\n%s", out)
	}

	// Replayed, and judged against incidents: what is measured too.
	incidents := write(t, "incidents.yaml", "incidents:\n  - {id: INC-1, title: Checkout down, start: 2026-11-01T21:00:00Z, end: 2026-11-01T21:30:00Z, labels: {service: checkout}}\n")
	stdout.Reset()
	if code := run([]string{"verdict", "--rules", rulesFile, "--check-content", "--prometheus", "http://x", "--incidents", incidents,
		"--severities", "page,ticket", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var rep contentReport
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	checks := func(name string, of func(contentRule) []contentOutput) []string {
		i := slices.IndexFunc(rep.Rules, func(r contentRule) bool { return r.Rule == name })
		if i < 0 {
			t.Fatalf("no %s", name)
		}
		var out []string
		for _, x := range of(rep.Rules[i]) {
			out = append(out, x.Check+":"+x.Detail)
		}
		return out
	}
	problems := func(r contentRule) []contentOutput { return r.Problems }
	guesses := func(r contentRule) []contentOutput { return r.Guesses }
	if !rep.Measured || !rep.Noisy || rep.Scrape != "1m" {
		t.Errorf("measured %v, noisy %v, scrape %q", rep.Measured, rep.Noisy, rep.Scrape)
	}
	if got, want := checks("CheckoutErrors", problems), []string{content.LabelNeverSeen + ":pod", content.NoisyNoFor + ":"}; !slices.Equal(got, want) {
		t.Errorf("CheckoutErrors: %q, want %q", got, want)
	}
	if got, want := checks("Typo", problems), []string{content.RunbookNotURL + ":runbooks/down.md", content.UnknownSeverity + ":pgae"}; !slices.Equal(got, want) {
		t.Errorf("Typo: %q, want %q", got, want)
	}
	if got, want := checks("Quiet", guesses), []string{content.ShortRange + ":rate[30s] < 2 × 1m"}; !slices.Equal(got, want) {
		t.Errorf("Quiet: %q, want %q", got, want)
	}
	if i := slices.IndexFunc(rep.Rules, func(r contentRule) bool { return r.Rule == "Quiet" }); rep.Rules[i].Guesses[0].Why == "" ||
		rep.Rules[i].Says.Summary != "{{ $labels.instance" {
		t.Errorf("Quiet: %+v", rep.Rules[i])
	}
}

// A name from the rules can't move the terminal or the table about.
func TestContentPrintable(t *testing.T) {
	if got := printable("a\x1b[2J\tb\u202ec\n"); got != "a [2J b c " {
		t.Errorf("%q", got)
	}
}

func TestCheckContentFlags(t *testing.T) {
	rulesFile := write(t, "rules.yaml", sayingRules)
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"verdict", "--alerts", "--prometheus", "http://x", "--check-content"}, "give them with one of --rules"},
		{[]string{"verdict", "--rules", rulesFile, "--check-content", "--compare", rulesFile}, "leave out --compare"},
		{[]string{"verdict", "--rules", rulesFile, "--check-content", "--check-replay", "--prometheus", "http://x"}, "--check-replay"},
		{[]string{"verdict", "--rules", rulesFile, "--check-content", "--metrics"}, "--metrics"},
		{[]string{"verdict", "--loaded-rules", "--check-content"}, "--loaded-rules reads the rules from the API"},
		{[]string{"verdict", "--rules", rulesFile, "--check-content", "--impact", "x"}, "--impact reads history"},
		{[]string{"verdict", "--rules", rulesFile, "--prometheus", "http://x", "--incidents", "x", "--severities", "page"}, "go with --check-content"},
		{[]string{"verdict", "--rules", rulesFile, "--check-content", "--severities", ", ,"}, "--severities"},
		{[]string{"verdict", "--rules", rulesFile, "--check-content", "--scrape-interval", "0s"}, "--scrape-interval"},
		{[]string{"verdict", "--rules", rulesFile, "--check-content=maybe"}, "want true or false"},
		{[]string{"serve", "--rules", rulesFile, "--prometheus", "http://x", "--check-content=false", "--severities", "page"}, "--check-content=false turns off"},
		{[]string{"serve", "--alerts", "--prometheus", "http://x", "--check-content"}, "give the rules"},
	} {
		var errs strings.Builder
		if code := run(c.args, io.Discard, &errs); code != 2 || !strings.Contains(errs.String(), c.says) {
			t.Errorf("%v: exit %d, want 2 saying %q:\n%s", c.args, code, c.says, errs.String())
		}
	}
	// serve checks what the rules say unless told not to.
	serveFlag := func(string, string) (bool, error) { return false, nil }
	for args, want := range map[string]bool{
		"--rules " + rulesFile + " --prometheus http://x":                          true,
		"--rules " + rulesFile + " --prometheus http://x --check-content=false":    false,
		"--rules " + rulesFile + " --prometheus http://x --severities page,ticket": true,
		"--alerts --prometheus http://x":                                           false,
	} {
		f, err := parseFlags(strings.Fields(args), serveFlag)
		if err != nil {
			t.Fatalf("%s: %v", args, err)
		}
		if checkContentOn(f) != want {
			t.Errorf("%s: %v", args, !want)
		}
	}
}
