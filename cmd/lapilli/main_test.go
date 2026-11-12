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
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/verdict"
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
	case expr == `ALERTS{alertstate="firing"}`:
		// CheckoutErrors, as it really fired, 5m after it went pending; and
		// an alert from a rule not in the files.
		out := run(3*time.Hour-5*time.Minute, 2*time.Hour)
		out[0].Labels = map[string]string{"alertname": "CheckoutErrors", "alertstate": "firing", "service": "checkout", "cluster": "one"}
		other := run(time.Hour, 50*time.Minute)
		other[0].Labels = map[string]string{"alertname": "Watchdog", "alertstate": "firing"}
		return append(out, other...), nil
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
	// A cloud's own alarms written in PromQL, as its CLI lists them.
	googlePolicy := write(t, "policies.json", `[{"displayName": "shop/CheckoutErrors", "severity": "CRITICAL", "enabled": true,
  "conditions": [{"conditionPrometheusQueryLanguage": {"query": "errors > 5", "duration": "300s", "evaluationInterval": "60s",
    "ruleGroup": "shop", "alertRule": "CheckoutErrors"}}]}]`)
	cloudWatch := write(t, "alarms.json", `{"MetricAlarms": [{"AlarmName": "CheckoutErrors", "EvaluationInterval": 60,
  "EvaluationCriteria": {"PromQLCriteria": {"Query": "errors > 5", "PendingPeriod": 300, "RecoveryPeriod": 600}}},
  {"AlarmName": "cpu-high", "Namespace": "AWS/EC2", "MetricName": "CPUUtilization", "Threshold": 90}]}`)

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
		{args: "verdict", code: 2, stderr: "one of --rules, --loaded-rules, --grafana, --ruler, --alerts, --firings"},
		{args: "verdict --alerts --firings x", code: 2, stderr: "one of --rules, --loaded-rules, --grafana, --ruler, --alerts, --firings"},
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
		{args: "verdict --rules " + googlePolicy + " --prometheus http://p --incidents " + incidents + " --tz UTC",
			code: 0, stdout: "INC-7 Checkout down  2026-11-01 21:00  CheckoutErrors +5m"},
		// Closed by the evaluation at least 270s after the errors last held.
		{args: "verdict --rules " + googlePolicy + " --prometheus http://p --incidents " + incidents + " --json",
			code: 0, stdout: `"end": "2026-11-01T22:05:00Z"`},
		{args: "verdict --rules " + cloudWatch + " --prometheus http://p --incidents " + incidents + " --tz UTC",
			code: 0, stdout: "INC-7 Checkout down  2026-11-01 21:00  CheckoutErrors +5m"},
		// Back to OK after ten minutes of not breaching; the metric alarm
		// can't be replayed.
		{args: "verdict --rules " + cloudWatch + " --prometheus http://p --incidents " + incidents + " --json",
			code: 0, stdout: `"end": "2026-11-01T22:11:00Z"`},
		{args: "verdict --rules " + cloudWatch + " --prometheus http://p --incidents " + incidents,
			code: 0, stdout: "cpu-high: a CloudWatch metric alarm isn't PromQL"},
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
		// Two hours of "for" misses the hour the errors last: an hour would
		// have caught it, at its end.
		{args: "verdict --rules " + laterRules + " --prometheus http://p --incidents " + incidents + " --tz UTC",
			code: 0, stdout: "INC-7     CheckoutErrors  1h1m from +0s  for 2h → 1h  +1h0m       0 → 1  0 → 0"},
		{args: "verdict --rules " + laterRules + " --prometheus http://p --incidents " + incidents + " --json",
			code: 0, stdout: `"new_for_ns": 3600000000000`},
		// The bursts come on a Sunday afternoon.
		{args: "verdict --rules " + flappyRules + " --prometheus http://p --incidents " + incidents + " --tz UTC",
			code: 0, stdout: "of the firings that caught nothing, 0 came while people slept, and 11 more off hours."},
		{args: "verdict --rules " + flappyRules + " --prometheus http://p --incidents " + incidents + " --tz UTC --sleep-hours 14:00-16:00",
			code: 0, stdout: "Flappy  page      6         5            0"},
		{args: "verdict --rules " + flappyRules + " --prometheus http://p --incidents " + incidents + " --tz UTC --metrics",
			code: 0, stdout: `lapilli_rule_nothing_by_hours{rule="Flappy",severity="page",hours="off_hours"} 11`},
		{args: "verdict --rules " + flappyRules + " --prometheus http://p --incidents " + incidents + " --tz UTC --work-hours none --sleep-hours none",
			code: 0, stdout: "0 came while people slept, and 11 more off hours."},
		{args: "verdict --rules x --prometheus http://p --incidents y --work-hours mon-fri@18:00-18:00", code: 2, stderr: "want days and hours"},
		// In shifts: one turn, asleep all day.
		{args: "verdict --rules " + flappyRules + " --prometheus http://p --incidents " + incidents + " --tz UTC --shifts every=1w;sleep=00:00-24:00", code: 2,
			stderr: "--shifts"},
		{args: "verdict --rules " + flappyRules + " --prometheus http://p --incidents " + incidents + " --tz UTC --shifts every=1w,from=2026-01-05;sleep=00:00-24:00", code: 2,
			stderr: "--shifts"},
		{args: "verdict --rules " + flappyRules + " --prometheus http://p --incidents " + incidents + " --tz UTC --shifts from=2026-01-05;sleep=00:00-24:00;tz=Asia/Seoul", code: 2,
			stderr: "every= and from= first"},
		{args: "verdict --rules x --prometheus http://p --incidents y --sleep-hours 25:00-08:00", code: 2, stderr: "--sleep-hours"},
		{args: "verdict --rules x --prometheus http://p --incidents y --work-hours someday@08:00-18:00", code: 2, stderr: "want days and hours"},
		{args: "verdict --rules " + laterRules + " --compare " + rulesFile + " --changed-only --prometheus http://p --incidents " + incidents,
			code: 0, stdout: "  after   no changed rule fired"},
		{args: "verdict --rules " + rulesFile + " --changed-only --prometheus http://p --incidents y", code: 2, stderr: "--changed-only goes with --compare"},
		// Compared without incidents: the firings counted, not judged.
		{args: "verdict --rules " + laterRules + " --compare " + rulesFile + " --prometheus http://p --tz UTC",
			code: 0, stdout: "Lapilli  2026-10-26 00:00 → 2026-11-02 00:00 UTC\nRules compared: 1 changed, 0 added, 0 taken away, 2 the same\nFiring" +
				"s 1 → 0\n\nRULE            FIRED  REFIRED\nCheckoutErrors  1 → 0  0 → 0\n"},
		{args: "verdict --rules " + laterRules + " --compare " + rulesFile + " --prometheus http://p --json",
			code: 0, stdout: `"incidents_given": false,`},
		{args: "verdict --rules " + laterRules + " --compare " + rulesFile + " --prometheus http://p --incidents " + incidents + " --json",
			code: 0, stdout: `"incidents_given": true,`},
		{args: "verdict --rules " + laterRules + " --compare " + rulesFile + " --changed-only --prometheus http://p --markdown --tz Asia/Seoul",
			code: 0, stdout: "### Lapilli backtest, 2026-10-26 09:00 → 2026-11-02 09:00 Asia/Seoul (+09:00)\n\nRules compared: **1 changed, 0 added, 0 taken away**, 2 the same. " +
				"No incidents were given: the firings are counted, not judged.\n\n| | Before | After |\n|---|--:|--:|\n| Firings of the changed rules | 1 | 0 |\n\n" +
				"#### Rules that differ\n\n| Rule | Fired | Refired |\n| --- | --: | --: |\n| `CheckoutErrors` | 1 → 0 | 0 → 0 |\n"},
		{args: "verdict --rules " + laterRules + " --compare " + rulesFile + " --prometheus http://p --markdown --incidents " + incidents,
			code: 0, stdout: "| Incidents with no alert, of 1 | 0 | 1 |\n"},
		{args: "verdict --rules " + rulesFile + " --compare " + rulesFile + " --prometheus http://p --markdown",
			code: 0, stdout: `"nothing_changed":true,"changed":0,"added":0,"removed":0,"same":3,`},
		{args: "verdict --rules " + rulesFile + " --compare " + rulesFile + " --prometheus http://p --markdown",
			code: 0, stdout: ")\n### Lapilli backtest: nothing changed\n\nNothing that decides when the 3 rules fire has changed: nothing to replay.\n"},
		{args: "verdict --rules " + rulesFile + " --compare " + rulesFile + " --prometheus http://p --json",
			code: 0, stdout: `"nothing_changed": true,`},
		{args: "verdict --rules " + rulesFile + " --prometheus http://p --markdown --incidents y", code: 2, stderr: "--markdown writes --compare's report"},
		{args: "verdict --rules x --compare y --prometheus http://p --markdown --json", code: 2, stderr: "--json and --markdown don't go together"},
		{args: "verdict --rules x --prometheus http://p", code: 2, stderr: "say what went wrong"},
		{args: "verdict --rules " + rulesFile + " --prometheus http://p --incidents " + incidents + " --tz Asia/Seoul",
			code: 0, stdout: "Lapilli  2026-10-26 09:00 → 2026-11-02 09:00 Asia/Seoul (+09:00)\n"},
		{args: "serve --rules " + rulesFile + " --prometheus http://p --markdown", code: 2, stderr: "lapilli serve doesn't take --markdown"},
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

	// In shifts, one turn of them asleep all day: every burst woke people.
	var stdout, stderr bytes.Buffer
	if code := run([]string{"verdict", "--rules", flappyRules, "--prometheus", "http://p", "--incidents", incidents, "--tz", "UTC",
		"--shifts", "every=1w from=2026-01-05; sleep=00:00-24:00"}, &stdout, &stderr); code != 0 ||
		!strings.Contains(stdout.String(), "Out of hours, in shifts (every=1w from=2026-01-05; sleep=00:00-24:00; where a turn doesn't say, at work mon-fri 08:00-18:00, asleep 22:00-08:00, in UTC):") ||
		!strings.Contains(stdout.String(), "11 came while people slept, and 0 more off hours.") {
		t.Errorf("shifts: exit %d\n%s\n%s", code, stdout.String(), stderr.String())
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

// lapilli serve refuses roles without a sign-in, and telling Slack without
// a Slack, before it starts.
func TestServeFlags(t *testing.T) {
	hook := write(t, "hook", "https://hooks.example/x")
	for _, args := range [][]string{
		{"--editors", "ana@example.com"},
		{"--admins=*"},
		{"--groups-header", "X-Forwarded-Groups"},
		{"--notify-slack"},
		{"--notify-webhook-file", hook + ".missing"},
		{"--external-url", "lapilli.example.com"},
		{"--lang", "fr"},
		{"--mcp-compare", "everyone"},
		{"--mcp-compare", "editors"}, // without roles, everyone's
		// --everyone-admin is for no roles, and lets everyone write.
		{"--everyone-admin", "--user-header", "X-Forwarded-Email", "--editors", "ana@example.com"},
		{"--everyone-admin", "--user-header", "X-Forwarded-Email", "--admins", "ana@example.com"},
		{"--everyone-admin", "--read-only"},
		{"--everyone-admin=maybe"},
	} {
		var out, errs strings.Builder
		if code := run(append([]string{"serve", "--prometheus", "http://127.0.0.1:1", "--rules", hook}, args...), &out, &errs); code != 2 {
			t.Errorf("%v: %d, want 2\n%s", args, code, errs.String())
		}
	}
}

func TestCheckReplay(t *testing.T) {
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
	var stdout, stderr bytes.Buffer
	code := run([]string{"verdict", "--rules", rulesFile, "--prometheus", "http://x", "--check-replay", "--tz", "UTC"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{`Replay beside ALERTS{alertstate="firing"}`, "1 firings alike, 0 only in the replay, 0 only in what really fired: 100% alike.",
		"1 rules fired alike, 0 differ, 1 fired neither way.", "Typo", "Really fired, but no rule given has the name: Watchdog."} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}

	stdout.Reset()
	if code := run([]string{"verdict", "--rules", rulesFile, "--prometheus", "http://x", "--check-replay", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("--json: exit %d", code)
	}
	var c struct {
		Alike  int      `json:"alike"`
		Series []string `json:"series"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &c); err != nil || c.Alike != 1 || len(c.Series) != 1 {
		t.Errorf("--json: %v %s", err, stdout.String())
	}

	// Nothing to check against isn't a pass.
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"verdict", "--rules", rulesFile, "--prometheus", "http://x", "--check-replay", "--check-series", "nothing_here"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "nothing to be checked against") {
		t.Errorf("exit %d: %s", code, stderr.String())
	}

	// Nothing compared isn't a pass either.
	broken := write(t, "broken.yaml", "groups:\n  - name: g\n    rules:\n      - {alert: Typo, expr: 'broken('}\n")
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"verdict", "--rules", broken, "--prometheus", "http://x", "--check-replay"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "no rule could be replayed") {
		t.Errorf("exit %d: %s", code, stderr.String())
	}
	// A series that doesn't say which alert it is can't be used.
	stderr.Reset()
	code = run([]string{"verdict", "--rules", rulesFile, "--prometheus", "http://x", "--check-replay", "--check-series", "errors > 5"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "no alertname") {
		t.Errorf("exit %d: %s", code, stderr.String())
	}

	for _, bad := range [][]string{
		{"verdict", "--alerts", "--prometheus", "http://x", "--check-replay"},
		{"verdict", "--rules", rulesFile, "--prometheus", "http://x", "--check-replay", "--incidents", "x"},
		{"verdict", "--rules", rulesFile, "--prometheus", "http://x", "--check-replay", "--metrics"},
		{"verdict", "--rules", rulesFile, "--prometheus", "http://x", "--check-replay", "--compare", rulesFile},
		{"verdict", "--rules", rulesFile, "--prometheus", "http://x", "--incidents", "x", "--check-series", "y"},
		{"serve", "--alerts", "--prometheus", "http://x", "--check-replay"},
		{"serve", "--rules", rulesFile, "--prometheus", "http://x", "--check-replay=false", "--check-series", "x"},
		{"serve", "--rules", rulesFile, "--prometheus", "http://x", "--check-replay=maybe"},
		{"serve", "--rules", rulesFile, "--prometheus", "http://x", "--check-differs", "5"},
	} {
		if code := run(bad, io.Discard, io.Discard); code != 2 {
			t.Errorf("%v: exit %d", bad, code)
		}
	}
}

func TestServeNewFlags(t *testing.T) {
	rulesFile := write(t, "rules.yaml", "groups:\n  - name: g\n    rules:\n      - {alert: A, expr: a}\n")
	short := write(t, "short", "too short")
	good := write(t, "good", "a-token-long-enough")
	hook := write(t, "hook", "https://hooks.example.com/x")
	for _, bad := range [][]string{
		{"--incidents-listen", ":8082"},
		{"--incidents-token-file", good},
		{"--incidents-token-file", short, "--incidents-listen", ":8082"},
		{"--incidents-token-file", "/no/such/file", "--incidents-listen", ":8082"},
		{"--incidents-token-file", good, "--incidents-listen", ":8082", "--read-only"},
		{"--team-digest", "payments"},
		{"--team-digest", "=" + hook},
		{"--team-digest", "payments=/no/such/file"},
		{"--report-dest", "ops"},
		{"--report-dest", "ops=/no/such/file"},
		{"--report-dest", "Ops=" + hook},
		{"--report-dest", "https://hooks.example.com/x=" + hook},
		{"--report-dest", "slack=" + hook},
		{"--report-dest", "webhook=" + hook},
		{"--report-dest", "team:payments=" + hook},
	} {
		args := append([]string{"serve", "--rules", rulesFile, "--prometheus", "http://x"}, bad...)
		if code := run(args, io.Discard, io.Discard); code != 2 {
			t.Errorf("%v: exit %d", bad, code)
		}
	}
}

// --firings-token-file takes firings copied in on --incidents-listen, with
// a token of its own; with it, rules and history are optional.
func TestServeFiringsFlags(t *testing.T) {
	rulesFile := write(t, "rules.yaml", "groups:\n  - name: g\n    rules:\n      - {alert: A, expr: a}\n")
	good := write(t, "good", "a-token-long-enough")
	other := write(t, "other", "another-token-long-enough")
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"--firings-token-file", good}, "go with --incidents-listen"},
		{[]string{"--firings-token-file", write(t, "short", "short"), "--incidents-listen", ":8082"}, "16 characters"},
		{[]string{"--firings-token-file", good, "--incidents-token-file", good, "--incidents-listen", ":8082"}, "the same token"},
		{[]string{"--firings-token-file", good, "--incidents-token-file", write(t, "same", "a-token-long-enough\n"), "--incidents-listen", ":8082"}, "the same token"},
		{[]string{"--firings-token-file", good, "--incidents-listen", ":8082", "--notify-token-file", write(t, "same-notify", "a-token-long-enough\n"), "--notify-listen", ":8083"}, "--notify-token-file holds the same token"},
		{[]string{"--rules", rulesFile, "--prometheus", "http://x", "--incidents-token-file", good, "--incidents-listen", ":8082", "--notify-token-file", good, "--notify-listen", ":8083"}, "--notify-token-file holds the same token"},
		{[]string{"--firings-token-file", good, "--incidents-listen", ":8082", "--read-only"}, "--read-only"},
		{[]string{"--incidents-listen", ":8082"}, "--firings-token-file"},
		// Nothing to judge but what is copied in, and no way in for it.
		{[]string{"--prometheus", "http://x"}, "--firings-token-file"},
		{[]string{"--firings-token-file", good, "--incidents-listen", ":8082", "--impact", "x"}, "--impact reads history"},
		{[]string{"--firings-token-file", good, "--incidents-listen", ":8082", "--rules", rulesFile}, "where the history is"},
		{[]string{"--firings-token-file", good, "--incidents-listen", ":8082", "--firings", rulesFile}, "doesn't take --firings"},
	} {
		var errs strings.Builder
		if code := run(append([]string{"serve"}, c.args...), io.Discard, &errs); code != 2 || !strings.Contains(errs.String(), c.says) {
			t.Errorf("%v: exit %d, want 2 saying %q:\n%s", c.args, code, c.says, errs.String())
		}
	}
	// Both tokens, each its own: taken. Firings alone: taken, with no
	// history to read.
	for _, args := range [][]string{
		{"--rules", rulesFile, "--prometheus", "http://x", "--firings-token-file", good, "--incidents-token-file", other},
		{"--firings-token-file", good},
	} {
		f, err := parseFlags(args, func(name, val string) (bool, error) {
			return name == "--firings-token-file" || name == "--incidents-token-file", nil
		})
		if err != nil || !f.firingsToken {
			t.Errorf("%v: %v", args, err)
		}
	}
}

// --tasks-token-file lets a job keep tasks in step with a tracker, on
// --incidents-listen, with a token no other file holds.
func TestServeTasksFlags(t *testing.T) {
	rulesFile := write(t, "rules.yaml", "groups:\n  - name: g\n    rules:\n      - {alert: A, expr: a}\n")
	good := write(t, "good", "a-token-long-enough")
	same := write(t, "same", "a-token-long-enough\n")
	other := write(t, "other", "another-token-long-enough")
	base := []string{"--rules", rulesFile, "--prometheus", "http://x"}
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"--tasks-token-file", good}, "go with --incidents-listen"},
		{[]string{"--tasks-token-file", write(t, "short", "short"), "--incidents-listen", ":8082"}, "16 characters"},
		{[]string{"--tasks-token-file", good, "--incidents-token-file", same, "--incidents-listen", ":8082"}, "--tasks-token-file holds the same token"},
		{[]string{"--tasks-token-file", good, "--firings-token-file", same, "--incidents-token-file", other, "--incidents-listen", ":8082"}, "--tasks-token-file holds the same token"},
		{[]string{"--tasks-token-file", good, "--incidents-listen", ":8082", "--notify-token-file", same, "--notify-listen", ":8083"}, "--tasks-token-file holds the same token"},
		{[]string{"--tasks-token-file", good, "--incidents-listen", ":8082", "--read-only"}, "--read-only"},
		{[]string{"--incidents-listen", ":8082"}, "--tasks-token-file"},
	} {
		var errs strings.Builder
		args := append(append([]string{"serve"}, base...), c.args...)
		if code := run(args, io.Discard, &errs); code != 2 || !strings.Contains(errs.String(), c.says) {
			t.Errorf("%v: exit %d, want 2 saying %q:\n%s", c.args, code, c.says, errs.String())
		}
	}
}

// serve sets the replay beside what really fired unless told not to.
func TestServeCheckReplayOn(t *testing.T) {
	rulesFile := write(t, "rules.yaml", "groups:\n  - name: g\n    rules:\n      - {alert: A, expr: a}\n")
	takes := func(name, val string) (bool, error) { return false, nil }
	for args, want := range map[string]bool{"": true, "--check-replay": true, "--check-replay=true": true, "--check-replay=false": false} {
		all := []string{"--rules", rulesFile, "--prometheus", "http://x"}
		if args != "" {
			all = append(all, args)
		}
		f, err := parseFlags(all, takes)
		if err != nil {
			t.Fatal(err)
		}
		if got := checkReplayOn(f); got != want {
			t.Errorf("%q: on %v, want %v", args, got, want)
		}
	}
}

// What --markdown writes for a pull request doesn't say where the history
// is: a failed query's reason names the URL it asked, and its host.
func TestHideWhere(t *testing.T) {
	f := &verdictFlags{promURL: "https://prometheus.internal.example:9090/prom", gcpProj: "shop-prod-123456", loki: "http://p"}
	reason := `query failed: Post "https://prometheus.internal.example:9090/prom/api/v1/query_range": dial tcp: lookup prometheus.internal.example: no such host`
	before := &verdict.Report{Rules: []verdict.RuleResult{{Rule: "A", Unsupported: reason}, {Rule: "B", Unsupported: "projects/shop-prod-123456 denied, p failed"}}}
	after := &verdict.Report{Rules: []verdict.RuleResult{{Rule: "A", Unsupported: replay.Unfinished}}}
	f.hideWhere(&verdict.Comparison{Before: before, After: after})
	if got := before.Rules[0].Unsupported; got != `query failed: Post "[--prometheus]/api/v1/query_range": dial tcp: lookup [--prometheus]: no such host` {
		t.Errorf("hidden: %s", got)
	}
	// A host of a letter isn't taken out of every word it is in.
	if got := before.Rules[1].Unsupported; got != "projects/[--gcp-project] denied, p failed" {
		t.Errorf("hidden: %s", got)
	}
	if after.Rules[0].Unsupported != replay.Unfinished {
		t.Errorf("unfinished: %s", after.Rules[0].Unsupported)
	}
	f.hideWhere(&verdict.Comparison{}) // nothing compared: nothing to hide
}
