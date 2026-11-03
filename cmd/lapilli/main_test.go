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
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
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
	case expr == "flappy":
		// Eleven six-minute bursts, then half an hour from 3h ago, through
		// the start of the incident.
		var out []prom.Series
		for k := 0; k < 11; k++ {
			from := 10*time.Hour - time.Duration(k)*20*time.Minute
			out = append(out, run(from, from-5*time.Minute)...)
		}
		out = append(out, run(3*time.Hour, 2*time.Hour+30*time.Minute)...)
		s := prom.Series{Labels: out[0].Labels}
		for _, o := range out {
			s.Samples = append(s.Samples, o.Samples...)
		}
		return []prom.Series{s}, nil
	}
	return nil, nil
}

// Rules answers as GET /api/v1/rules would, with the checkout rule loaded.
func (fakeHistory) Rules(context.Context) ([]byte, error) {
	return []byte(`{"status":"success","data":{"groups":[{"name":"shop","interval":60,"rules":[
		{"type":"alerting","name":"CheckoutErrors","query":"errors > 5","duration":300,"labels":{"severity":"page"}},
		{"type":"recording","name":"job:errors","query":"sum(errors)"}]}]}}`), nil
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
	// Proposed: the checkout rule waits two hours, longer than the errors
	// last.
	laterRules := write(t, "later.yaml", `groups:
  - name: shop
    rules:
      - alert: CheckoutErrors
        expr: errors > 5
        for: 2h
      - alert: Typo
        expr: broken(
      - alert: Quiet
        expr: never
`)
	flappyRules := write(t, "flappy.yaml", `groups:
  - name: shop
    rules:
      - alert: Flappy
        expr: flappy
        labels: {severity: page}
      - alert: Twice
        expr: never
        labels: {severity: page}
      - alert: Twice
        expr: never
        labels: {severity: ticket}
      - alert: Errors
        expr: errors > 5
        labels: {severity: page}
      - alert: Errors
        expr: errors > 5
        labels: {severity: ticket}
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
		{args: "verdict", code: 2, stderr: "one of --rules, --loaded-rules, --grafana, --alerts, --firings"},
		{args: "verdict --alerts --firings x", code: 2, stderr: "one of --rules, --loaded-rules, --grafana, --alerts, --firings"},
		{args: "verdict --alerts --incidents x", code: 2, stderr: "add --prometheus URL or --gcp-project ID"},
		{args: "verdict --firings x", code: 2, stderr: "say what went wrong"},
		{args: "verdict --firings x --incidents y --json --metrics", code: 2, stderr: "don't go together"},
		{args: "verdict --firings x --incidents y --since soon", code: 2, stderr: "want a duration"},
		{args: "verdict --firings x --incidents y --from 2026-11-01T00:00:00Z", code: 2, stderr: "--from and --to go together"},
		{args: "verdict --firings x --incidents y --nope 1", code: 2, stderr: "unknown flag --nope"},
		{args: "verdict --alerts --auth nope --prometheus http://p --incidents y", code: 2, stderr: "want one of aws, azure, google"},
		{args: "verdict --alerts --auth oci --prometheus http://p --incidents y", code: 2, stderr: "Oracle Cloud has no managed Prometheus"},
		{args: "verdict --alerts --auth azure --incidents y", code: 2, stderr: "--auth goes with --prometheus URL"},
		{args: "verdict --alerts --gcp-project p --auth aws --incidents y", code: 2, stderr: "--gcp-project is Google Cloud's"},
		{args: "verdict --alerts --prometheus http://p --auth aws --bearer-token-file t --incidents y", code: 2, stderr: "don't go together"},
		{args: "verdict --alerts --prometheus http://p --aws-region us-east-1 --incidents y", code: 2, stderr: "--aws-region goes with --auth aws"},
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
		{args: "verdict --rules " + laterRules + " --compare " + rulesFile + " --prometheus http://p --incidents " + incidents + " --tz UTC",
			code: 0, stdout: "INC-7 Checkout down  2026-11-01 21:00\n  before  CheckoutErrors +5m\n  after   ✗ nothing fired\n"},
		{args: "verdict --rules " + laterRules + " --compare " + rulesFile + " --prometheus http://p --incidents " + incidents,
			code: 0, stdout: "Rules compared: 1 changed, 0 added, 0 taken away, 2 the same"},
		{args: "verdict --rules " + laterRules + " --compare " + rulesFile + " --prometheus http://p --incidents " + incidents + " --json",
			code: 0, stdout: `"same": 2`},
		{args: "verdict --rules " + rulesFile + " --compare " + rulesFile + " --prometheus http://p --incidents " + incidents,
			code: 0, stdout: "Nothing that decides when the 3 rules fire has changed: nothing to replay."},
		{args: "verdict --rules " + flappyRules + " --prometheus http://p --incidents " + incidents,
			code: 0, stdout: "Flappy  0s → 10m  12 → 1  1          up to 10m"},
		{args: "verdict --rules " + flappyRules + " --prometheus http://p --incidents " + incidents,
			code: 0, stdout: "page      12     1         11         8%         10"},
		{args: "verdict --rules " + flappyRules + " --rule ^Errors$ --prometheus http://p --incidents " + incidents + " --tz UTC",
			code: 0, stdout: "Errors [severity=page]    page      1"},
		{args: "verdict --rules " + flappyRules + " --prometheus http://p --incidents " + incidents + " --noisy-firings 20",
			code: 0, stdout: "Flappy +0s\n"},
		{args: "verdict --rules " + flappyRules + " --prometheus http://p --incidents " + incidents + " --metrics",
			code: 0, stdout: `lapilli_rule_firings{rule="Twice [severity=ticket]",severity="ticket"} 0`},
		{args: "verdict --rules " + laterRules + " --compare " + rulesFile + " --changed-only --prometheus http://p --incidents " + incidents,
			code: 0, stdout: "  after   no changed rule fired"},
		{args: "verdict --rules " + rulesFile + " --changed-only --prometheus http://p --incidents y", code: 2, stderr: "--changed-only goes with --compare"},
		{args: "verdict --rules x --noisy-precision 120% --prometheus http://p --incidents y", code: 2, stderr: "want a share like 20% or 0.2"},
		{args: "verdict --loaded-rules --prometheus http://p --incidents " + incidents + " --tz UTC",
			code: 0, stdout: "INC-7 Checkout down  2026-11-01 21:00  CheckoutErrors +5m"},
		{args: "verdict --grafana-token-file t --alerts --prometheus http://p --incidents y", code: 2, stderr: "--grafana-token-file goes with --grafana"},
		{args: "verdict --grafana ftp://g --incidents y", code: 2, stderr: "want Grafana's URL"},
		{args: "verdict --alerts --compare x --prometheus http://p --incidents y", code: 2, stderr: "--compare goes with --rules"},
		{args: "verdict --rules x@2026-11-01T21:30:00Z --compare y --prometheus http://p --incidents z", code: 2, stderr: "give --rules without @TIME"},
		{args: "verdict --rules x --compare y --prometheus http://p --incidents z --metrics", code: 2, stderr: "--compare doesn't go with --metrics or --push"},
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
		switch {
		case strings.Contains(r.URL.Path, "revoked"):
			http.Error(w, "invalid_token", http.StatusForbidden)
		case strings.Contains(r.URL.Path, "moved"):
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		case r.URL.Path == "/elsewhere":
			t.Errorf("a redirect was followed, Referer %q", r.Header.Get("Referer"))
		case strings.HasPrefix(r.URL.Path, "/services/"):
			slack = g
		default:
			push = g
		}
	}))
	defer srv.Close()
	firings := write(t, "firings.jsonl", `{"rule":"<!channel> & co","start":"2026-11-01T21:01:00Z","end":"2026-11-01T21:30:00Z"}`+"\n")
	incidents := write(t, "incidents.yaml", "incidents:\n  - {id: INC-7, start: 2026-11-01T21:00:00Z}\n")
	hook := write(t, "hook", srv.URL+"/services/T0/B0/SECRET\n")
	revoked := write(t, "revoked", srv.URL+"/services/T0/B0/revoked-SECRET\n")
	moved := write(t, "moved", srv.URL+"/services/T0/B0/moved-SECRET\n")
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

	for file, want := range map[string]string{revoked: "HTTP 403: invalid_token", unreachable: "sending to Slack", moved: "HTTP 302"} {
		stdout.Reset()
		stderr.Reset()
		if code := run(strings.Fields(base+" --slack-webhook-file "+file), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), want) {
			t.Errorf("%s: exit %d, stderr %q, want 1 and %q", file, code, stderr.String(), want)
		}
		if strings.Contains(stderr.String(), "SECRET") {
			t.Errorf("the webhook's URL leaked into an error: %s", stderr.String())
		}
	}
	// A URL naming its group is used as it is.
	if code := run(strings.Fields(base+" --push "+srv.URL+"/metrics/job/lapilli/instance/shop"), &stdout, &stderr); code != 0 || push.path != "/metrics/job/lapilli/instance/shop" {
		t.Errorf("exit %d, pushed to %q", code, push.path)
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

func TestTellApart(t *testing.T) {
	rs := []rules.Rule{
		{Name: "Errors", Group: "api", Labels: map[string]string{"severity": "page"}},
		{Name: "Errors", Group: "api", Labels: map[string]string{"severity": "ticket"}},
		{Name: "Disk", Group: "a"},
		{Name: "Disk", Group: "b"},
		{Name: "Twin", Group: "a"},
		{Name: "Twin", Group: "a"},
		{Name: "Alone", Group: "a"},
	}
	tellApart(rs)
	var got []string
	for _, r := range rs {
		got = append(got, r.Name)
	}
	want := []string{"Errors [severity=page]", "Errors [severity=ticket]", "Disk [a]", "Disk [b]", "Twin", "Twin", "Alone"}
	if !slices.Equal(got, want) {
		t.Errorf("names %q, want %q", got, want)
	}
}

func TestGrafanaRules(t *testing.T) {
	newHistory = func(context.Context, *verdictFlags) (history, error) { return fakeHistory{}, nil }
	now = func() time.Time { return t0 }
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/provisioning/alert-rules/export":
			auth = r.Header.Get("Authorization")
			if r.URL.Query().Get("format") != "yaml" {
				http.Error(w, "want yaml", http.StatusBadRequest)
				return
			}
			io.WriteString(w, `apiVersion: 1
groups:
  - orgId: 1
    name: shop
    folder: prod
    interval: 1m
    rules:
      - uid: c1
        title: CheckoutErrors
        condition: A
        for: 5m
        data:
          - refId: A
            datasourceUid: prom
            model: {expr: errors > 5}
`)
		case "/moved/api/v1/provisioning/alert-rules/export":
			http.Redirect(w, r, "https://elsewhere.example/steal", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	token := write(t, "token", "glsa_secret\n")
	incidents := write(t, "incidents.yaml", "incidents:\n  - {id: INC-7, title: Checkout down, start: 2026-11-01T21:00:00Z, end: 2026-11-01T22:00:00Z}\n")
	var stdout, stderr bytes.Buffer
	args := []string{"verdict", "--grafana", srv.URL, "--grafana-token-file", token, "--prometheus", "http://p", "--incidents", incidents, "--tz", "UTC"}
	if code := run(args, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "INC-7 Checkout down  2026-11-01 21:00  CheckoutErrors +5m") {
		t.Fatalf("exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
	if auth != "Bearer glsa_secret" {
		t.Errorf("Authorization %q", auth)
	}
	stdout.Reset()
	stderr.Reset()
	args[2] = srv.URL + "/moved"
	if code := run(args, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "HTTP 302") || strings.Contains(stderr.String(), "glsa_secret") {
		t.Errorf("a redirect isn't followed, and the token isn't shown: exit %d, %s", code, stderr.String())
	}
}

func TestTellApartAcrossSets(t *testing.T) {
	page := map[string]string{"severity": "page"}
	was := []rules.Rule{{Name: "Errors", Expr: "x", Labels: page}, {Name: "Errors", Expr: "x", Labels: map[string]string{"severity": "ticket"}}}
	is := []rules.Rule{{Name: "Errors", Expr: "x", Labels: page}}
	tellApart(is, was)
	same, before, after := split(was, is)
	if len(same) != 1 || len(before) != 1 || len(after) != 0 || is[0].Name != "Errors [severity=page]" {
		t.Errorf("the page rule, unchanged, is the same in both: same %v, before %v, after %v", same, before, after)
	}
	// A rule whose severity changed, alone in each set, keeps its name.
	a, b := []rules.Rule{{Name: "Disk", Labels: page}}, []rules.Rule{{Name: "Disk", Labels: map[string]string{"severity": "ticket"}}}
	tellApart(a, b)
	if a[0].Name != "Disk" || b[0].Name != "Disk" {
		t.Errorf("names %q %q", a[0].Name, b[0].Name)
	}
}
