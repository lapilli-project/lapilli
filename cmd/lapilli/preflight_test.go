package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/alarmtest"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/ledger/ledgertest"
	"lapilli.dev/internal/signin/idptest"
)

// fake is a stand-in API that counts what it is sent, and fails the test
// on a request it doesn't serve.
type fake struct {
	t     *testing.T
	name  string
	mu    sync.Mutex
	asked map[string]int // "METHOD /path" to how many
	serve func(w http.ResponseWriter, r *http.Request, form url.Values) bool
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var form url.Values
	if r.Method == http.MethodPost && strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		b, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(b))
		r.Body = io.NopCloser(bytes.NewReader(b))
	}
	f.mu.Lock()
	f.asked[r.Method+" "+r.URL.Path]++
	f.mu.Unlock()
	if !f.serve(w, r, form) {
		f.t.Errorf("%s was sent %s %s?%s %v, which it doesn't serve", f.name, r.Method, r.URL.Path, r.URL.RawQuery, form)
		http.NotFound(w, r)
	}
}

func (f *fake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, v := range f.asked {
		n += v
	}
	return n
}

func newFake(t *testing.T, name string, serve func(w http.ResponseWriter, r *http.Request, form url.Values) bool) *fake {
	return &fake{t: t, name: name, asked: map[string]int{}, serve: serve}
}

// routed sends a request for a host it names to a stand-in, whatever its
// scheme, keeping the host it was for; any other as before. It stands in
// for http.DefaultTransport, which a cloud's SDK's client sends through.
type routed struct {
	hosts map[string]http.Handler
	next  http.RoundTripper
}

func (r *routed) RoundTrip(req *http.Request) (*http.Response, error) {
	if h, ok := r.hosts[req.URL.Host]; ok {
		return (&alarmtest.Transport{H: h}).RoundTrip(req)
	}
	return r.next.RoundTrip(req)
}

func matrix(w http.ResponseWriter, series ...string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[%s]}}`, strings.Join(series, ","))
}

// pfGrafanaExport is a Grafana's two rules: one of PromQL, on a data source
// of Prometheus's kind, one of LogQL, on Loki's.
const pfGrafanaExport = `apiVersion: 1
groups:
  - orgId: 1
    name: shop
    folder: shop
    interval: 1m
    rules:
      - uid: u1
        title: CheckoutErrors
        condition: B
        for: 5m
        data:
          - {refId: A, datasourceUid: prom, model: {expr: 'sum(rate(checkout_errors_total[5m]))', instant: true}}
          - refId: B
            datasourceUid: __expr__
            model: {type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [1]}}]}
      - uid: u2
        title: CheckoutLogErrors
        condition: B
        data:
          - {refId: A, datasourceUid: logs, model: {expr: 'sum(count_over_time({app="checkout"} |= "error" [5m]))', instant: true}}
          - refId: B
            datasourceUid: __expr__
            model: {type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [10]}}]}
`

// stsAnswer is STS's answer to AssumeRole.
const stsAnswer = `<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleResult><Credentials>
<AccessKeyId>ASIAEXAMPLEEXAMPLE</AccessKeyId><SecretAccessKey>example-secret</SecretAccessKey><SessionToken>example-session</SessionToken>
<Expiration>2036-01-01T00:00:00Z</Expiration></Credentials><AssumedRoleUser><Arn>arn:aws:sts::123456789012:assumed-role/lapilli-rules/lapilli</Arn>
<AssumedRoleId>AROAEXAMPLE:lapilli</AssumedRoleId></AssumedRoleUser></AssumeRoleResult><ResponseMetadata><RequestId>r1</RequestId></ResponseMetadata></AssumeRoleResponse>`

// pfReport runs lapilli preflight --json with args.
func pfReport(t *testing.T, args ...string) (map[string]any, int, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(append([]string{"preflight", "--json"}, args...), &out, &errOut)
	var rep map[string]any
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("exit %d: %v\n%s\n%s", code, err, out.String(), errOut.String())
	}
	return rep, code, out.String()
}

// pfItems are a report's items by id.
func pfItems(rep map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, x := range rep["items"].([]any) {
		it := x.(map[string]any)
		out[it["id"].(string)] = it
	}
	return out
}

// files are the names in a directory, and the bytes of each.
func dirFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range es {
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		out[e.Name()] = string(b)
	}
	return out
}

// lapilli preflight with serve's flags, against stand-ins for a
// Prometheus, a Grafana, a Loki, an OpenID Connect provider, AWS's STS and
// CloudWatch, beside a ledger (SQLite, or with LAPILLI_TEST_POSTGRES,
// Postgres): every request it sends is one a stand-in serves and the
// report counts, none is refused or off its list, and nothing is written.
func TestPreflightAgainstFakes(t *testing.T) {
	was := now
	now = time.Now
	t.Cleanup(func() { now = was })
	prometheus := newFake(t, "Prometheus", func(w http.ResponseWriter, r *http.Request, form url.Values) bool {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v1/query_range" && form.Get("query") == "vector(1)":
			matrix(w, fmt.Sprintf(`{"metric":{},"values":[[%s,"1"]]}`, form.Get("start")))
		case r.Method == "POST" && r.URL.Path == "/api/v1/query_range" && strings.Contains(form.Get("query"), `count_over_time(ALERTS{alertstate="firing"}[1d])`):
			matrix(w, fmt.Sprintf(`{"metric":{"alertname":"Errors"},"values":[[%s,"120"]]}`, form.Get("start")))
		case r.Method == "POST" && r.URL.Path == "/api/v1/query_range" && strings.Contains(form.Get("query"), "GRAFANA_ALERTS"):
			matrix(w)
		case r.Method == "GET" && r.URL.Path == "/api/v1/status/config":
			fmt.Fprint(w, `{"status":"success","data":{"yaml":"global:\n  scrape_interval: 30s\n"}}`)
		case r.Method == "GET" && r.URL.Path == "/api/v1/targets":
			fmt.Fprint(w, `{"status":"success","data":{"activeTargets":[]}}`)
		default:
			return false
		}
		return true
	})
	grafana := newFake(t, "Grafana", func(w http.ResponseWriter, r *http.Request, _ url.Values) bool {
		switch {
		case r.Method != "GET":
			return false
		case r.URL.Path == "/api/v1/provisioning/alert-rules/export":
			fmt.Fprint(w, pfGrafanaExport)
		case r.URL.Path == "/api/datasources/uid/prom":
			fmt.Fprint(w, `{"type":"prometheus","jsonData":{"timeInterval":"30s"}}`)
		case r.URL.Path == "/api/datasources/uid/logs":
			fmt.Fprint(w, `{"type":"loki"}`)
		case r.URL.Path == "/api/datasources/proxy/uid/logs/loki/api/v1/labels":
			fmt.Fprint(w, `{"status":"success","data":["app"]}`)
		case r.URL.Path == "/api/v1/provisioning/templates", r.URL.Path == "/api/v1/provisioning/contact-points":
			fmt.Fprint(w, `[]`)
		case r.URL.Path == "/api/v1/provisioning/policies":
			fmt.Fprint(w, `{"receiver":"default"}`)
		default:
			return false
		}
		return true
	})
	loki := newFake(t, "Loki", func(w http.ResponseWriter, r *http.Request, _ url.Values) bool {
		if r.Method == "GET" && r.URL.Path == "/loki/api/v1/labels" {
			fmt.Fprint(w, `{"status":"success","data":["app"]}`)
			return true
		}
		return false
	})
	sts := newFake(t, "STS", func(w http.ResponseWriter, r *http.Request, form url.Values) bool {
		if r.Method == "POST" && form.Get("Action") == "AssumeRole" && form.Get("RoleArn") == "arn:aws:iam::123456789012:role/lapilli-rules" {
			w.Header().Set("Content-Type", "text/xml")
			fmt.Fprint(w, stsAnswer)
			return true
		}
		return false
	})
	cw := alarmtest.NewCloudWatch("eu-west-1")
	cw.Add(map[string]any{"AlarmName": "QueueDepth", "Namespace": "AWS/SQS", "MetricName": "ApproximateNumberOfMessagesVisible",
		"Statistic": "Average", "Period": 60, "EvaluationPeriods": 5, "Threshold": 1000, "ComparisonOperator": "GreaterThanThreshold"}, "OK", nil)
	cloudwatch := newFake(t, "CloudWatch", func(w http.ResponseWriter, r *http.Request, _ url.Values) bool {
		if r.Header.Get("X-Amz-Target") != "GraniteServiceVersion20100801.DescribeAlarms" {
			return false
		}
		cw.ServeHTTP(w, r)
		return true
	})
	servers := map[string]*httptest.Server{}
	for name, f := range map[string]*fake{"prometheus": prometheus, "grafana": grafana, "loki": loki} {
		s := httptest.NewServer(f)
		t.Cleanup(s.Close)
		servers[name] = s
	}
	idp := idptest.New(t, "lapilli", "", time.Now)
	before := http.DefaultTransport
	http.DefaultTransport = &routed{next: before, hosts: map[string]http.Handler{"sts.eu-west-1.amazonaws.com": sts, "monitoring.eu-west-1.amazonaws.com": cloudwatch}}
	t.Cleanup(func() { http.DefaultTransport = before })
	// AWS's credentials from the environment alone: nothing of this
	// machine's is read.
	dir := t.TempDir()
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLEEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "example-secret")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "none"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	for _, k := range []string{"AWS_PROFILE", "AWS_SESSION_TOKEN", "AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_STS", "AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"} {
		t.Setenv(k, "")
	}

	ctx := context.Background()
	args := []string{"--grafana", servers["grafana"].URL, "--grafana-token-file", write(t, "grafana-token", "glsa_example_viewer_token"),
		"--prometheus", servers["prometheus"].URL, "--loki", servers["loki"].URL,
		"--alarms", "cw=https://monitoring.eu-west-1.amazonaws.com",
		"--oidc-issuer", idp.URL, "--oidc-client-id", "lapilli", "--external-url", "https://lapilli.example.com", "--admins", "ops@example.com",
		"--changes", "--changes-aws-role", "arn:aws:iam::123456789012:role/lapilli-rules",
		"--changes-grafana-token-file", write(t, "grafana-writes", "glsa_example_editor_token"),
		"--incidents", write(t, "incidents.yaml", "incidents: []\n"), "--config", write(t, "config.yaml", "teams: []\n"),
		"--incidents-listen", "127.0.0.1:9091", "--incidents-token-file", write(t, "incidents-token", "incidents-token-0123456789"),
		"--firings-token-file", write(t, "firings-token", "firings-token-0123456789"),
		"--slack-webhook-file", write(t, "slack", "https://hooks.slack.example/services/T0/B0/x\n"),
		"--impact", `sum(rate(checkout_errors_total[5m])) > 1`, "--guard", `rate(prometheus_rule_evaluation_failures_total[5m]) > 0`,
		"--max-rate", "5", "--catch-up-hours", "01:00-06:00", "--catch-up-rate", "2"}
	var counted func() string
	var file string
	ledgerDir := filepath.Join(dir, "ledger")
	if u := ledgertest.URL(t); u != "" {
		l, err := ledger.OpenPostgres(ctx, u, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		l.Close()
		args = append(args, "--ledger-url-file", write(t, "ledger-url", u))
		counted = func() string { return ledgertest.Digest(t, u) }
	} else {
		os.MkdirAll(ledgerDir, 0o750)
		file = filepath.Join(ledgerDir, "ledger.db")
		l, err := ledger.Open(ctx, file, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		l.Close()
		args = append(args, "--ledger", ledgerDir)
	}
	var wrote string
	var bytesBefore map[string]string
	if counted != nil {
		wrote = counted()
	} else {
		bytesBefore = dirFiles(t, ledgerDir)
	}

	rep, code, out := pfReport(t, args...)
	items := pfItems(rep)
	for _, id := range []string{"rules.read", "rules.replayable", "expressions", "history.reach", "history.loki", "history.grafana", "history.alerts",
		"history.intervals", "grafana.notifications", "grafana.write", "changes.cloud", "inputs.files", "secrets.files", "signin", "access",
		"estimate.hourly", "estimate.backfill", "estimate.nights", "estimate.beside", "estimate.memory"} {
		if items[id] == nil {
			t.Errorf("no %s", id)
		}
	}
	if counted != nil {
		if items["ledger.postgres"]["state"] != "ok" {
			t.Errorf("%v", items["ledger.postgres"])
		}
	} else if items["ledger.sqlite"]["state"] != "ok" {
		t.Errorf("%v", items["ledger.sqlite"])
	}
	for id, it := range items {
		want := "ok"
		if id == "grafana.write" || id == "changes.cloud" {
			want = "unknown"
		}
		if it["state"] != want {
			t.Errorf("%s: %v", id, it)
		}
	}
	if code != 0 {
		t.Errorf("exit %d", code)
	}
	if !strings.Contains(items["changes.cloud"]["what"].(string), "--changes-aws-role") {
		t.Errorf("%v", items["changes.cloud"])
	}
	if !strings.Contains(items["rules.read"]["what"].(string), "3 alert rules") {
		t.Errorf("%v", items["rules.read"])
	}

	// Every request a stand-in served is one the report counted, and none
	// was refused.
	asked := rep["asked"].(map[string]any)
	sent := prometheus.count() + grafana.count() + loki.count() + sts.count() + cloudwatch.count() + int(idp.KeyFetches.Load()) + 1
	if int(asked["requests"].(float64)) != sent || asked["refused"].(float64) != 0 {
		t.Errorf("asked %v; the stand-ins were sent %d", asked, sent)
	}
	if sts.count() != 1 || prometheus.count() != 5 || loki.count() != 1 || idp.KeyFetches.Load() != 1 {
		t.Errorf("STS %d, Prometheus %d %v, Loki %d, keys %d", sts.count(), prometheus.count(), prometheus.asked, loki.count(), idp.KeyFetches.Load())
	}
	if len(cw.Errs) > 0 {
		t.Errorf("CloudWatch: %v", cw.Errs)
	}
	// Nothing written to the ledger.
	if counted != nil {
		if n := counted(); n != wrote {
			t.Errorf("rows of the ledger changed:\n%s\n%s", wrote, n)
		}
	} else {
		after := dirFiles(t, ledgerDir)
		if !slices.Equal(slices.Sorted(maps.Keys(bytesBefore)), slices.Sorted(maps.Keys(after))) {
			t.Errorf("the ledger's directory held %v, then %v", slices.Sorted(maps.Keys(bytesBefore)), slices.Sorted(maps.Keys(after)))
		}
		for name, b := range bytesBefore {
			if after[name] != b {
				t.Errorf("%s changed", name)
			}
		}
	}
	// The secrets are said by their paths and lengths, never their text;
	// the stand-ins' addresses never.
	for _, secret := range []string{"glsa_example_viewer_token", "incidents-token-0123456789", "hooks.slack.example", "example-secret", "127.0.0.1"} {
		if strings.Contains(out, secret) {
			t.Errorf("the report says %q", secret)
		}
	}
}

// Its own flags are taken out before serve's reader, which refuses what
// serve refuses, and it exits 2 then; -h says its options.
func TestPreflightFlags(t *testing.T) {
	rules := write(t, "rules.yaml", "groups:\n  - name: g\n    rules:\n      - alert: Down\n        expr: up == 0\n")
	for _, x := range []struct {
		args []string
		code int
		says string
	}{
		{[]string{"--rules", rules}, 2, "say where the history is"},
		{[]string{"--rules", rules, "--prometheus", "http://prometheus:9090", "--from", "2026-11-01T00:00:00Z"}, 2, "lapilli serve doesn't take --from"},
		{[]string{"--rules", rules, "--prometheus", "http://prometheus:9090", "--offline", "--preflight-timeout", "soon"}, 2, "--preflight-timeout soon"},
		{[]string{"--rules", rules, "--prometheus", "http://prometheus:9090", "--offline=maybe"}, 2, "--offline=maybe"},
		{[]string{"--rules", rules, "--prometheus", "http://prometheus:9090", "--offline", "--json", "--fail-unknown"}, 1, `"version": 1`},
		{[]string{"--rules", rules, "--prometheus", "http://prometheus:9090", "--offline", "--json", "--preflight-timeout=30s"}, 0, `"history.reach"`},
		{[]string{"-h"}, 0, "--fail-unknown"},
	} {
		var out, errOut bytes.Buffer
		code := run(append([]string{"preflight"}, x.args...), &out, &errOut)
		if code != x.code || !strings.Contains(out.String()+errOut.String(), x.says) {
			t.Errorf("%v: exit %d, want %d saying %q:\n%s%s", x.args, code, x.code, x.says, out.String(), errOut.String())
		}
	}
	var out bytes.Buffer
	run([]string{"help", "preflight"}, &out, io.Discard)
	if !strings.Contains(out.String(), "lapilli preflight [serve's flags]") {
		t.Errorf("help preflight: %s", out.String())
	}
	out.Reset()
	run(nil, io.Discard, &out)
	if !strings.Contains(out.String(), "lapilli preflight") {
		t.Errorf("usage: %s", out.String())
	}
}

// What a server answers, in a problem's why, is redacted as a pull
// request's comment is: the URL given as its flag, its host and user, an
// address, what acts on a terminal; and cut.
func TestPreflightRedacted(t *testing.T) {
	was := now
	now = time.Now
	t.Cleanup(func() { now = was })
	answer := newFake(t, "Prometheus", func(w http.ResponseWriter, r *http.Request, _ url.Values) bool {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, "ops@prom.internal.example from 10.1.2.3:4567 may not read team-a\x1b[31m %s", strings.Repeat("x", 500))
		return true
	})
	before := http.DefaultTransport
	http.DefaultTransport = &routed{next: before, hosts: map[string]http.Handler{"prom.internal.example:9090": answer}}
	t.Cleanup(func() { http.DefaultTransport = before })
	rules := write(t, "rules.yaml", "groups:\n  - name: g\n    rules:\n      - alert: Down\n        expr: up == 0\n")
	var out bytes.Buffer
	code := run([]string{"preflight", "--rules", rules, "--prometheus", "http://ops@prom.internal.example:9090",
		"--ledger", t.TempDir(), "--check-content=false", "--check-replay=false"}, &out, io.Discard)
	text := out.String()
	if code != 1 || !strings.Contains(text, "problem  history.reach") || !strings.Contains(text, "[--prometheus]") {
		t.Fatalf("exit %d:\n%s", code, text)
	}
	for _, hidden := range []string{"prom.internal.example", "10.1.2.3", "ops@", "\x1b", strings.Repeat("x", 300)} {
		if strings.Contains(text, hidden) {
			t.Errorf("the report says %q:\n%s", hidden, text)
		}
	}
}

// Every source of rules serve reads is read through the allowlist with
// nothing refused: a refusal is a bug of lapilli's, never the setup's.
func TestPreflightEverySource(t *testing.T) {
	was := now
	now = time.Now
	t.Cleanup(func() { now = was })
	api := newFake(t, "the APIs", func(w http.ResponseWriter, r *http.Request, form url.Values) bool {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v1/query_range":
			matrix(w, fmt.Sprintf(`{"metric":{},"values":[[%s,"1"]]}`, form.Get("start")))
		case r.Method == "GET" && r.URL.Path == "/api/v1/rules":
			fmt.Fprint(w, `{"status":"success","data":{"groups":[{"name":"g","file":"f","interval":60,"rules":[{"type":"alerting","name":"Down","query":"up == 0","duration":0,"labels":{},"annotations":{}}]}]}}`)
		case r.Method == "GET" && r.URL.Path == "/prometheus/config/v1/rules":
			fmt.Fprint(w, "shop:\n  - name: g\n    rules:\n      - alert: Down\n        expr: up == 0\n")
		case r.Method == "GET" && r.URL.Path == "/loki/api/v1/labels":
			fmt.Fprint(w, `{"status":"success","data":["app"]}`)
		case r.Method == "GET" && r.URL.Path == "/loki/api/v1/rules":
			fmt.Fprint(w, "shop:\n  - name: logs\n    rules:\n      - alert: LogErrors\n        expr: sum(count_over_time({app=\"checkout\"} |= \"error\" [5m])) > 10\n")
		case r.Method == "GET" && r.URL.Path == "/prometheus/api/v1/rules":
			fmt.Fprint(w, `{"status":"success","data":{"groups":[]}}`)
		case r.Method == "GET" && r.URL.Path == "/api/v1/provisioning/alert-rules/export":
			fmt.Fprint(w, pfGrafanaExport)
		case r.Method == "GET" && r.URL.Path == "/api/datasources/uid/prom":
			fmt.Fprint(w, `{"type":"prometheus"}`)
		case r.Method == "GET" && r.URL.Path == "/api/datasources/uid/logs":
			fmt.Fprint(w, `{"type":"loki"}`)
		case r.Method == "GET" && r.URL.Path == "/api/datasources/proxy/uid/logs/loki/api/v1/labels":
			fmt.Fprint(w, `{"status":"success","data":["app"]}`)
		default:
			return false
		}
		return true
	})
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	cw := alarmtest.NewCloudWatch("eu-west-1")
	cw.Add(map[string]any{"AlarmName": "QueueDepth", "Namespace": "AWS/SQS", "MetricName": "ApproximateNumberOfMessagesVisible",
		"Statistic": "Average", "Period": 60, "EvaluationPeriods": 5, "Threshold": 1000, "ComparisonOperator": "GreaterThanThreshold"}, "OK", nil)
	before := http.DefaultTransport
	http.DefaultTransport = &routed{next: before, hosts: map[string]http.Handler{"monitoring.eu-west-1.amazonaws.com": cw}}
	t.Cleanup(func() { http.DefaultTransport = before })
	dir := t.TempDir()
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLEEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "example-secret")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "none"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	rules := write(t, "rules.yaml", "groups:\n  - name: g\n    rules:\n      - alert: Down\n        expr: up == 0\n")
	history := t.TempDir()
	os.MkdirAll(filepath.Join(history, "2026-10-01T00:00:00Z"), 0o750)
	os.WriteFile(filepath.Join(history, "2026-10-01T00:00:00Z", "rules.yaml"), []byte("groups:\n  - name: g\n    rules:\n      - alert: Down\n        expr: up == 0\n"), 0o600)
	lokiRules := write(t, "loki.yaml", "groups:\n  - name: logs\n    rules:\n      - alert: LogErrors\n        expr: sum(count_over_time({app=\"checkout\"} |= \"error\" [5m])) > 10\n")
	for _, args := range [][]string{
		{"--rules", rules, "--prometheus", srv.URL},
		{"--rules-history", history, "--prometheus", srv.URL},
		{"--loaded-rules", "--prometheus", srv.URL},
		{"--grafana", srv.URL, "--prometheus", srv.URL},
		{"--ruler", srv.URL + "/prometheus/config/v1/rules", "--prometheus", srv.URL},
		{"--loki-rules", lokiRules, "--loki", srv.URL, "--prometheus", srv.URL},
		{"--loki-loaded-rules", "--loki", srv.URL, "--prometheus", srv.URL},
		{"--alarms", "cw=https://monitoring.eu-west-1.amazonaws.com", "--rules", rules, "--prometheus", srv.URL},
		{"--firings-token-file", write(t, "firings", "firings-token-0123456789"), "--incidents-listen", "127.0.0.1:9091"},
	} {
		rep, code, out := pfReport(t, append(args, "--ledger", t.TempDir(), "--check-content=false", "--check-replay=false")...)
		asked := rep["asked"].(map[string]any)
		if asked["refused"].(float64) != 0 || code != 0 {
			t.Errorf("%s: exit %d, asked %v:\n%s", args[0], code, asked, out)
		}
		if it := pfItems(rep)["rules.read"]; it != nil && it["state"] != "ok" || it == nil && args[0] != "--firings-token-file" {
			t.Errorf("%s: %v", args[0], it)
		}
	}
	if len(cw.Errs) > 0 {
		t.Errorf("CloudWatch: %v", cw.Errs)
	}
}

// --offline reads no secret's file and no --ledger-url-file, which CI
// doesn't hold: it says so, and goes on.
func TestPreflightOfflineSecrets(t *testing.T) {
	rules := write(t, "rules.yaml", "groups:\n  - name: g\n    rules:\n      - alert: Down\n        expr: up == 0\n")
	gone := filepath.Join(t.TempDir(), "not-here")
	args := []string{"--rules", rules, "--prometheus", "http://prometheus:9090", "--ledger-url-file", gone,
		"--incidents-listen", ":9091", "--incidents-token-file", gone, "--slack-webhook-file", gone + "-slack"}
	var errOut bytes.Buffer
	if code := run(append([]string{"preflight"}, args...), io.Discard, &errOut); code != 2 {
		t.Errorf("not --offline: exit %d, %s", code, errOut.String())
	}
	rep, code, _ := pfReport(t, append(args, "--offline")...)
	items := pfItems(rep)
	if code != 0 || items["secrets.files"]["state"] != "unknown" || items["ledger.postgres"]["state"] != "unknown" {
		t.Errorf("exit %d: %v %v", code, items["secrets.files"], items["ledger.postgres"])
	}
}

// What a server answered is redacted whole before it is cut: a host cut
// in two at 200 bytes is redacted all the same. And so is what serve's
// files name: a cloud's alarms, the sign-in, the ledger's server, an
// on-call feed's host.
func TestPreflightRedactedEverywhere(t *testing.T) {
	was := now
	now = time.Now
	t.Cleanup(func() { now = was })
	long := "internal-k8s-monitoring-prometheus-0123456789abcdef.eu-west-1.elb.amazonaws.example.com" // 87 characters
	refuse := func(name, body string) *fake {
		return newFake(t, name, func(w http.ResponseWriter, r *http.Request, _ url.Values) bool {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, body)
			return true
		})
	}
	before := http.DefaultTransport
	http.DefaultTransport = &routed{next: before, hosts: map[string]http.Handler{
		long + ":9090":                       refuse("Prometheus", strings.Repeat("x", 100)+" at "+long),
		"monitoring.eu-west-1.amazonaws.com": refuse("CloudWatch", `{"__type":"AccessDenied","message":"not at monitoring.eu-west-1.amazonaws.com"}`),
		"idp.internal.example":               refuse("the identity provider", "idp.internal.example refuses"),
		"calendar.internal.example":          refuse("the feed", "no"),
	}}
	t.Cleanup(func() { http.DefaultTransport = before })
	dir := t.TempDir()
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLEEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "example-secret")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "none"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	rules := write(t, "rules.yaml", "groups:\n  - name: g\n    rules:\n      - alert: Down\n        expr: up == 0\n")
	feed := write(t, "feed-url", "https://calendar.internal.example/feeds/0123456789abcdef.ics\n")
	onCall := write(t, "on-call.yaml", "schedules:\n  - {name: primary, url_file: "+feed+"}\n")
	var out bytes.Buffer
	run([]string{"preflight", "--rules", rules, "--prometheus", "http://" + long + ":9090", "--alarms", "cw=https://monitoring.eu-west-1.amazonaws.com",
		"--oidc-issuer", "https://idp.internal.example", "--oidc-client-id", "lapilli", "--external-url", "https://lapilli.example.com", "--viewers", "*@example.com",
		"--ledger-url-file", write(t, "ledger-url", "postgres://lapilli@localhost:1/lapilli?connect_timeout=2\n"), "--on-call", onCall,
		"--check-content=false", "--check-replay=false"}, &out, io.Discard)
	text := out.String()
	for _, why := range []string{"history.reach", "rules.read", "signin", "ledger.postgres", "inputs.files"} {
		if !strings.Contains(text, "problem  "+why) {
			t.Errorf("no problem %s:\n%s", why, text)
		}
	}
	for _, hidden := range []string{long[:30], long[30:60], long[57:], "monitoring.eu-west-1", "idp.internal.example", "localhost", "calendar.internal.example", "0123456789abcdef"} {
		if strings.Contains(text, hidden) {
			t.Errorf("the report says %q:\n%s", hidden, text)
		}
	}
}

// What is left of a value to redact where a server's answer was cut, its
// start before an ellipsis, is redacted too; a word that only begins as
// it does isn't.
func TestScrubberCut(t *testing.T) {
	f := &verdictFlags{promURL: "http://prometheus-0123456789.monitoring.example.com:9090"}
	scrub := f.scrubber()
	for in, want := range map[string]string{
		"refused at prometheus-01234…":         "refused at [--prometheus]…",
		"refused at prometheus-0123456789.mo…": "refused at [--prometheus]…",
		"the pro… was cut":                     "the [--prometheus]… was cut",
		"see myprometheus-0123…":               "see myprometheus-0123…",
		"pr…":                                  "pr…",
	} {
		if got := scrub(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
