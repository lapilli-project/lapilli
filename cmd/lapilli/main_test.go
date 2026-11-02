package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
)

var t0 = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)

// fakeHistory answers each expression with a canned series: "errors > 5"
// is true from 3h to 2h ago, "impact" from 6h ago for 10m, anything with
// "broken" fails.
type fakeHistory struct{}

func (fakeHistory) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	run := func(from, to time.Duration) []prom.Series {
		s := prom.Series{Labels: map[string]string{"service": "checkout"}}
		for t := t0.Add(-from); !t.After(t0.Add(-to)); t = t.Add(step) {
			if !t.Before(start) && !t.After(end) {
				s.Samples = append(s.Samples, prom.Sample{T: t, V: 1})
			}
		}
		return []prom.Series{s}
	}
	switch {
	case strings.Contains(expr, "broken"):
		return nil, errors.New("bad_data: parse error")
	case expr == "errors > 5":
		return run(3*time.Hour, 2*time.Hour), nil
	case expr == "slow":
		<-ctx.Done()
		return nil, ctx.Err()
	case expr == "impact":
		return run(6*time.Hour, 6*time.Hour-10*time.Minute), nil
	}
	return nil, nil
}

func write(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRun(t *testing.T) {
	newHistory = func(context.Context, *verdictFlags) (history, error) { return fakeHistory{}, nil }
	now = func() time.Time { return t0 }
	rulesFile := write(t, "rules.yaml", `groups:
  - name: shop
    rules:
      - alert: CheckoutErrors
        expr: errors > 5
        for: 5m
      - alert: Typo
        expr: broken(
      - alert: Quiet
        expr: never
`)
	incidents := write(t, "incidents.yaml", `incidents:
  - id: INC-7
    title: Checkout down
    start: 2026-11-01T21:00:00Z
    end: 2026-11-01T22:00:00Z
`)
	slowRules := write(t, "slow.yaml", `groups:
  - name: shop
    rules:
      - alert: CheckoutErrors
        expr: errors > 5
        for: 5m
      - alert: Slow
        expr: slow
`)
	// From 21:30 the checkout rule is gone.
	quietRules := write(t, "quiet.yaml", `groups:
  - name: shop
    rules:
      - alert: Quiet
        expr: never
`)
	firings := write(t, "firings.jsonl", `{"rule":"Paged","start":"2026-11-01T21:01:00Z","end":"2026-11-01T21:30:00Z"}`+"\n")

	for _, c := range []struct {
		args   string
		code   int
		stdout string
		stderr string
	}{
		{args: "", code: 2, stderr: "lapilli verdict --rules FILE"},
		{args: "version", code: 0, stdout: "lapilli "},
		{args: "help verdict", code: 0, stdout: "The incidents file:"},
		{args: "verdict -h", code: 0, stdout: "--impact PROMQL"},
		{args: "nope", code: 2, stderr: `unknown command "nope"`},
		{args: "verdict", code: 2, stderr: "one of --rules, --alerts, --firings"},
		{args: "verdict --alerts --firings x", code: 2, stderr: "one of --rules, --alerts, --firings"},
		{args: "verdict --alerts --incidents x", code: 2, stderr: "add --prometheus URL or --gcp-project ID"},
		{args: "verdict --firings x", code: 2, stderr: "say what went wrong"},
		{args: "verdict --firings x --incidents y --json --metrics", code: 2, stderr: "don't go together"},
		{args: "verdict --firings x --incidents y --since soon", code: 2, stderr: "want a duration"},
		{args: "verdict --firings x --incidents y --from 2026-11-01T00:00:00Z", code: 2, stderr: "--from and --to go together"},
		{args: "verdict --firings x --incidents y --nope 1", code: 2, stderr: "unknown flag --nope"},
		{args: "verdict --rules x@2026-10-12 --prometheus http://p --incidents y", code: 2, stderr: "want FILE, or FILE@TIME"},

		{args: "verdict --rules " + rulesFile + " --prometheus http://p --incidents " + incidents + " --tz UTC",
			code: 0, stdout: "INC-7 Checkout down  2026-11-01 21:00  CheckoutErrors +5m"},
		{args: "verdict --rules " + rulesFile + " --prometheus http://p --incidents " + incidents,
			code: 0, stdout: "Typo: query failed: bad_data: parse error"},
		{args: "verdict --rules " + rulesFile + " --prometheus http://p --incidents " + incidents + " --impact impact",
			code: 0, stdout: "✗ 1 of 2 incidents had no alert."},
		{args: "verdict --rules " + rulesFile + " --rule ^Quiet$ --prometheus http://p --incidents " + incidents,
			code: 0, stdout: "1 rules never fired."},
		{args: "verdict --rules " + slowRules + " --prometheus http://p --incidents " + incidents + " --timeout 1s --tz UTC",
			code: 1, stdout: "CheckoutErrors +5m", stderr: "1 of 2 rules didn't finish within --timeout 1s"},
		{args: "verdict --rules " + rulesFile + " --rules " + quietRules + "@2026-11-01T21:30:00Z --prometheus http://p --incidents " + incidents + " --tz UTC",
			code: 0, stdout: "replaced at 2026-11-01 21:30"},
		{args: "verdict --rules " + rulesFile + " --rules " + quietRules + "@2026-11-01T21:30:00Z --prometheus http://p --incidents " + incidents + " --json",
			code: 0, stdout: `"end": "2026-11-01T21:30:00Z"`},
		{args: "verdict --rules " + quietRules + "@2026-11-01T21:30:00Z --prometheus http://p --incidents " + incidents,
			code: 1, stderr: "give the rules from before then as --rules FILE"},
		{args: "verdict --firings " + firings + " --incidents " + incidents + " --metrics",
			code: 0, stdout: `lapilli_rule_caught{rule="Paged"} 1`},
		{args: "verdict --firings " + firings + " --incidents " + incidents + " --json",
			code: 0, stdout: `"white": true`},
	} {
		var stdout, stderr bytes.Buffer
		code := run(strings.Fields(c.args), &stdout, &stderr)
		if code != c.code {
			t.Errorf("lapilli %s: exit %d, want %d\nstdout: %s\nstderr: %s", c.args, code, c.code, stdout.String(), stderr.String())
		}
		if c.stdout != "" && !strings.Contains(stdout.String(), c.stdout) {
			t.Errorf("lapilli %s: stdout lacks %q:\n%s", c.args, c.stdout, stdout.String())
		}
		if c.stderr != "" && !strings.Contains(stderr.String(), c.stderr) {
			t.Errorf("lapilli %s: stderr lacks %q:\n%s", c.args, c.stderr, stderr.String())
		}
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"7d": 7 * 24 * time.Hour, "1d12h": 36 * time.Hour, "15m": 15 * time.Minute} {
		if got, err := parseDuration(in); err != nil || got != want {
			t.Errorf("parseDuration(%q) = %s, %v", in, got, err)
		}
	}
	if _, err := parseDuration("xd"); err == nil {
		t.Error("xd parsed")
	}
}

func TestSend(t *testing.T) {
	type got struct{ method, path, ctype, body string }
	var push, slack got
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		g := got{r.Method, r.URL.Path, r.Header.Get("Content-Type"), string(b)}
		if strings.HasPrefix(r.URL.Path, "/services/") {
			if strings.Contains(r.URL.Path, "revoked") {
				http.Error(w, "invalid_token", http.StatusForbidden)
				return
			}
			slack = g
		} else {
			push = g
		}
	}))
	defer srv.Close()
	firings := write(t, "firings.jsonl", `{"rule":"<!channel> & co","start":"2026-11-01T21:01:00Z","end":"2026-11-01T21:30:00Z"}`+"\n")
	incidents := write(t, "incidents.yaml", "incidents:\n  - {id: INC-7, start: 2026-11-01T21:00:00Z}\n")
	hook := write(t, "hook", srv.URL+"/services/T0/B0/SECRET\n")
	revoked := write(t, "revoked", srv.URL+"/services/T0/B0/revoked-SECRET\n")
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	unreachable := write(t, "unreachable", closed.URL+"/services/T0/B0/SECRET\n")
	base := "verdict --firings " + firings + " --incidents " + incidents + " --from 2026-11-01T00:00:00Z --to 2026-11-02T00:00:00Z"

	var stdout, stderr bytes.Buffer
	if code := run(strings.Fields(base+" --push "+srv.URL+" --slack-webhook-file "+hook), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if push.method != http.MethodPut || push.path != "/metrics/job/lapilli" || !strings.Contains(push.body, "lapilli_rule_caught{") {
		t.Errorf("push %+v", push)
	}
	var msg struct{ Text string }
	if err := json.Unmarshal([]byte(slack.body), &msg); err != nil || slack.ctype != "application/json" {
		t.Fatalf("slack %+v: %v", slack, err)
	}
	if !strings.HasPrefix(msg.Text, "```\nLapilli") || !strings.Contains(msg.Text, "&lt;!channel&gt; &amp; co") || strings.Contains(msg.Text, "<!channel>") {
		t.Errorf("slack text should be the report in a code block, <, > and & escaped:\n%s", msg.Text)
	}
	if !strings.Contains(stdout.String(), "INC-7") {
		t.Errorf("the report should still go to stdout:\n%s", stdout.String())
	}

	for file, want := range map[string]string{revoked: "HTTP 403: invalid_token", unreachable: "sending to Slack"} {
		stdout.Reset()
		stderr.Reset()
		if code := run(strings.Fields(base+" --slack-webhook-file "+file), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), want) {
			t.Errorf("%s: exit %d, stderr %q, want 1 and %q", file, code, stderr.String(), want)
		}
		if strings.Contains(stderr.String(), "SECRET") {
			t.Errorf("the webhook's URL leaked into an error: %s", stderr.String())
		}
	}
	for args, want := range map[string]string{
		" --push nowhere":                "want the Pushgateway's URL",
		" --slack-webhook-file /no/such": "--slack-webhook-file",
	} {
		stderr.Reset()
		if code := run(strings.Fields(base+args), &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), want) {
			t.Errorf("%s: exit %d, stderr %q", args, code, stderr.String())
		}
	}
}
