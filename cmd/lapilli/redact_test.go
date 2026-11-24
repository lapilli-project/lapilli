package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"lapilli.dev/internal/verdict"
)

// lapilli verdict --redact: the text, --json and --metrics name no rule,
// service, host or incident; the numbers stay; --redact-key's key is read
// from its file and never said.
func TestVerdictRedact(t *testing.T) {
	parallel(t)
	incidents := write(t, "incidents.yaml", `incidents:
  - {id: OPS-31337, title: Basket service melted, start: 2026-10-20T00:00:00Z, end: 2026-10-20T01:00:00Z, labels: {service: basket-api}}
`)
	firings := write(t, "firings.jsonl", `{"rule":"BasketLatencyHigh","labels":{"service":"basket-api","instance":"basket-host-3.corp.example","severity":"critical"},"start":"2026-10-19T23:55:00Z","end":"2026-10-20T00:30:00Z"}
{"rule":"NightlyBackupLate","labels":{"job":"backup-runner","severity":"warning"},"start":"2026-10-21T02:00:00Z","end":"2026-10-21T02:10:00Z"}
`)
	secrets := []string{"BasketLatencyHigh", "NightlyBackupLate", "basket-api", "basket-host-3", "backup-runner", "OPS-31337", "Basket service melted"}
	base := []string{"verdict", "--firings", firings, "--incidents", incidents, "--from", "2026-10-19T00:00:00Z", "--to", "2026-10-26T00:00:00Z", "--tz", "UTC"}
	run1 := func(more ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := run(append(append([]string{}, base...), more...), &stdout, &stderr); code != 0 {
			t.Fatalf("%v: exit %d: %s", more, code, stderr.String())
		}
		return stdout.String()
	}
	if plain := run1(); !strings.Contains(plain, "BasketLatencyHigh") || !strings.Contains(plain, "OPS-31337") {
		t.Fatalf("unredacted:\n%s", plain)
	}
	for _, more := range [][]string{{"--redact"}, {"--redact", "--json"}, {"--redact", "--metrics"}, {"--redact", "--csv"}} {
		out := run1(more...)
		for _, s := range secrets {
			if strings.Contains(out, s) {
				t.Errorf("%v: %q is there:\n%s", more, s, out)
			}
		}
		if more[len(more)-1] == "--redact" && (!strings.Contains(out, "incident-01") || !strings.Contains(out, "critical")) {
			t.Errorf("text:\n%s", out)
		}
	}
	// In UTC, whatever --tz says, and stderr says it isn't redacted.
	var stdout, stderr bytes.Buffer
	seoul := append(append([]string{}, base...), "--tz", "Asia/Seoul", "--redact")
	if code := run(seoul, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "2026-10-26 00:00 UTC") || strings.Contains(stdout.String(), "Seoul") ||
		strings.Contains(stdout.String(), "+09:00") || !strings.Contains(stderr.String(), "on stderr, isn't") {
		t.Errorf("--tz Asia/Seoul --redact: exit %d\n%s\n%s", code, stdout.String(), stderr.String())
	}
	var rep verdict.Report
	if err := json.Unmarshal([]byte(run1("--redact", "--json")), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Firings) != 2 || !rep.Firings[0].White || rep.Firings[0].Labels["instance"] != "instance-01" || rep.Firings[0].Labels["severity"] != "critical" {
		t.Errorf("firings %+v", rep.Firings)
	}

	key := write(t, "key", "4f1c8e2a9b7d3f60c5e1a8b2d9f4c7e03a6b1d8e5f2c9a7b4e1d6f3a8c5b2e90\n")
	a, b := run1("--redact-key", key, "--json"), run1("--redact-key", key, "--json")
	if a != b || strings.Contains(a, "BasketLatencyHigh") || !strings.Contains(a, `"rule": "rule-`) {
		t.Errorf("--redact-key: not the same twice, or not redacted:\n%s", a)
	}
	short := write(t, "short", "0a1b2c3d4e5f6a7b8c9d\n")
	phrase := write(t, "phrase", "correct s3cr3t horse battery staple\n")
	for args, want := range map[string]string{
		"--redact-key " + short:                       "want a random key, 32 hex digits or more",
		"--redact-key " + phrase:                      "want a random key, 32 hex digits or more",
		"--redact-key /no/such/key":                   "--redact-key:",
		"--redact --check-content --rules " + firings: "--redact gives a verdict's names aliases",
	} {
		var stdout, stderr bytes.Buffer
		argv := strings.Fields(args)
		if !strings.Contains(args, "--rules") {
			argv = append(append([]string{}, base...), argv...)
		} else {
			argv = append([]string{"verdict"}, argv...)
		}
		if code := run(argv, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), want) || strings.Contains(stderr.String(), "s3cr3t") ||
			strings.Contains(stderr.String(), "0a1b2c3d") {
			t.Errorf("%s: exit %d, %s", args, code, stderr.String())
		}
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"serve", "--redact", "--rules", firings, "--prometheus", "http://p"}, &stdout, &stderr); code != 2 ||
		!strings.Contains(stderr.String(), "lapilli serve doesn't take --redact") {
		t.Errorf("serve --redact: exit %d, %s", code, stderr.String())
	}
}
