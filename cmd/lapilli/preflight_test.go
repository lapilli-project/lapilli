package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/user"
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

// pfGrafanaAdmin is the part of Grafana OSS 13.3.1's answer to GET
// /api/access-control/user/permissions for an Admin service account's
// token that alert rules' provisioning reads, as measured
// (docs/design/preflight.md, "As built (step 2)").
const pfGrafanaAdmin = `{"alert.provisioning:read":[""],"alert.provisioning:write":[""],"alert.provisioning.provenance:write":[""],
"alert.rules:read":["folders:*"],"alert.rules:write":["folders:*"],"alert.rules:create":["folders:*"],"alert.rules:delete":["folders:*"],
"folders:read":["folders:*","folders:uid:general"],"datasources:query":["datasources:*"]}`

// simulatedAnswer is IAM's answer to SimulatePrincipalPolicy, as its query
// API gives one (API_SimulatePrincipalPolicy.html): each action asked, on
// each resource asked, decided so.
func simulatedAnswer(form url.Values, decision string) string {
	var b strings.Builder
	b.WriteString(`<SimulatePrincipalPolicyResponse xmlns="https://iam.amazonaws.com/doc/2010-05-08/"><SimulatePrincipalPolicyResult>` +
		`<IsTruncated>false</IsTruncated><EvaluationResults>`)
	for i := 1; form.Get(fmt.Sprintf("ActionNames.member.%d", i)) != ""; i++ {
		fmt.Fprintf(&b, `<member><EvalActionName>%s</EvalActionName><EvalResourceName>*</EvalResourceName><EvalDecision>%s</EvalDecision><ResourceSpecificResults>`,
			form.Get(fmt.Sprintf("ActionNames.member.%d", i)), decision)
		for j := 1; form.Get(fmt.Sprintf("ResourceArns.member.%d", j)) != ""; j++ {
			fmt.Fprintf(&b, `<member><EvalResourceName>%s</EvalResourceName><EvalResourceDecision>%s</EvalResourceDecision></member>`,
				form.Get(fmt.Sprintf("ResourceArns.member.%d", j)), decision)
		}
		b.WriteString(`</ResourceSpecificResults><MissingContextValues/></member>`)
	}
	b.WriteString(`</EvaluationResults></SimulatePrincipalPolicyResult><ResponseMetadata><RequestId>r2</RequestId></ResponseMetadata></SimulatePrincipalPolicyResponse>`)
	return b.String()
}

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
	alone(t)
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
		case r.URL.Path == "/api/access-control/user/permissions" && r.Header.Get("Authorization") == "Bearer glsa_example_editor_token":
			// As Grafana OSS 13.3.1 lists an Admin service account's.
			fmt.Fprint(w, pfGrafanaAdmin)
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
	iam := newFake(t, "IAM", func(w http.ResponseWriter, r *http.Request, form url.Values) bool {
		if r.Method != "POST" || r.URL.Path != "/" || form.Get("Action") != "SimulatePrincipalPolicy" ||
			form.Get("PolicySourceArn") != "arn:aws:iam::123456789012:role/lapilli-rules" {
			return false
		}
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, simulatedAnswer(form, "allowed"))
		return true
	})
	cw := alarmtest.NewCloudWatch("eu-west-1")
	cw.Account = "123456789012" // the role's: serve writes no alarm of another account
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
	http.DefaultTransport = &routed{next: before, hosts: map[string]http.Handler{"sts.eu-west-1.amazonaws.com": sts, "monitoring.eu-west-1.amazonaws.com": cloudwatch,
		"iam.amazonaws.com": iam}}
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
		"--changes", "--changes-aws-role", "arn:aws:iam::123456789012:role/lapilli-rules", "--changes-in", "cw:Queue*",
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
		"history.intervals", "grafana.notifications", "grafana.write", "changes.cloud", "changes.aws", "inputs.files", "secrets.files", "signin", "access",
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
		if it["state"] != "ok" {
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
	sent := prometheus.count() + grafana.count() + loki.count() + sts.count() + cloudwatch.count() + iam.count() + int(idp.KeyFetches.Load()) + 1
	if int(asked["requests"].(float64)) != sent || asked["refused"].(float64) != 0 {
		t.Errorf("asked %v; the stand-ins were sent %d", asked, sent)
	}
	if sts.count() != 1 || prometheus.count() != 5 || loki.count() != 1 || idp.KeyFetches.Load() != 1 || iam.count() != 1 {
		t.Errorf("STS %d, Prometheus %d %v, Loki %d, keys %d, IAM %d", sts.count(), prometheus.count(), prometheus.asked, loki.count(), idp.KeyFetches.Load(), iam.count())
	}
	if by := asked["by"].(map[string]any); by["iam:aws"] != 1.0 {
		t.Errorf("asked by %v", by)
	}
	// What the stand-ins were sent is reads, and the simulator's POST.
	for _, f := range []*fake{grafana, iam} {
		for k := range f.asked {
			if !strings.HasPrefix(k, "GET ") && k != "POST /" {
				t.Errorf("%s was sent %s", f.name, k)
			}
		}
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

// --backfill is ten years at most, for serve and for lapilli preflight,
// which counts it a day at a time; ten years is taken.
func TestBackfillAtMost(t *testing.T) {
	// Alone, not parallel: lapilli preflight, run here, sets the process's egress guard.
	rules := write(t, "rules.yaml", "groups:\n  - name: g\n    rules:\n      - alert: Down\n        expr: up == 0\n")
	// Its --ledger is a file, so that a serve that took the flag stops
	// at once, and makes no ledger.
	for _, args := range [][]string{{"serve"}, {"preflight", "--offline"}} {
		var out, errOut bytes.Buffer
		code := run(append(args, "--rules", rules, "--prometheus", "http://prometheus:9090", "--backfill", "3651d", "--keep", "0", "--ledger", rules), &out, &errOut)
		if code != 2 || !strings.Contains(errOut.String(), "--backfill 3651d: at most 3650d") {
			t.Errorf("%s: exit %d\n%s%s", args[0], code, out.String(), errOut.String())
		}
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"preflight", "--rules", rules, "--prometheus", "http://prometheus:9090", "--offline", "--json", "--backfill", "3650d", "--keep", "0"}, &out, &errOut); code != 0 ||
		!strings.Contains(out.String(), `"estimate.backfill"`) {
		t.Errorf("3650d: exit %d\n%s%s", code, out.String(), errOut.String())
	}
}

// Its own flags are taken out before serve's reader, which refuses what
// serve refuses, and it exits 2 then; -h says its options.
func TestPreflightFlags(t *testing.T) {
	// Alone, not parallel: lapilli preflight, run here, sets the process's egress guard.
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
	alone(t)
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
	alone(t)
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
	// Alone, not parallel: lapilli preflight, run here, sets the process's egress guard.
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
	alone(t)
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
	parallel(t)
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

// An IPv6 address written without brackets, as x509 and Postgres write
// one, is an address too, as is one with a zone, or an IPv4 one inside
// it; and --gcp-project is taken out of a service account's email and a
// workload identity pool, as Google names them. A time, a MAC address, a
// "::" in other text, or a word that only holds the project, isn't.
func TestScrubberAddressesAndProject(t *testing.T) {
	parallel(t)
	f := &verdictFlags{promURL: "https://prom.example.invalid", gcpProj: "sample-proj-123"}
	scrub := f.scrubber()
	for in, want := range map[string]string{
		"x509: certificate is valid for 10.1.2.3, fd12:3456:789a::17, not prom.example.invalid": "x509: certificate is valid for [address], [address], not [--prometheus]",
		`no pg_hba.conf entry for host "2001:db8:42::5", user "u"`:                              `no pg_hba.conf entry for host "[address]", user "u"`,
		"dial tcp [2001:db8::9]:5432: connect: connection refused":                              "dial tcp [address]: connect: connection refused",
		"dial tcp 10.1.2.3:5432: refused":                                                       "dial tcp [address]: refused",
		"from 2001:0db8:0000:0000:0000:ff00:0042:8329 and fe80::1%eth0.":                        "from [address] and [address].",
		"mapped ::ffff:10.1.2.3, loopback ::1, at host:fd12::17: refused":                       "mapped [address], loopback [address], at host:[address]: refused",
		"denied on projects/-/serviceAccounts/writer@sample-proj-123.iam.gserviceaccount.com":   "denied on projects/-/serviceAccounts/writer@[--gcp-project].iam.gserviceaccount.com",
		"pool sample-proj-123.svc.id.goog, projects/sample-proj-123/topics/x":                   "pool [--gcp-project].svc.id.goog, projects/[--gcp-project]/topics/x",
		// What isn't an address, nor the project.
		"at 12:30:45, 2026-11-16T12:00:00Z, in 01:00-06:00, after 1h30m, 5m:": "at 12:30:45, 2026-11-16T12:00:00Z, in 01:00-06:00, after 1h30m, 5m:",
		"a :: b, std::string, Foo::Bar, key:: and ::":                         "a :: b, std::string, Foo::Bar, key:: and ::",
		"mac aa:bb:cc:dd:ee:ff, fingerprint AB:CD:EF:01:23:45:67:89:AB:CD":    "mac aa:bb:cc:dd:ee:ff, fingerprint AB:CD:EF:01:23:45:67:89:AB:CD",
		"sample-proj-1234, xsample-proj-123, sample-proj-123x.iam":            "sample-proj-1234, xsample-proj-123, sample-proj-123x.iam",
	} {
		if got := scrub(in); got != want {
			t.Errorf("%q:\n got %q\nwant %q", in, got, want)
		}
	}
	// A host is still a whole word: not the first label of another.
	f = &verdictFlags{promURL: "http://prom:9090"}
	if got := f.scrubber()("prom.example and prom"); got != "prom.example and [--prometheus]" {
		t.Errorf("%q", got)
	}
}

// An IPv6 address in brackets with a zone, as Go writes one dialing it
// ([fe80::1%en0]:5432) and a URL holds one (%25), is an address with its
// port. A project is taken out as the first label of the names Google
// gives under it, and of no other domain: a project named monitoring
// leaves monitoring.googleapis.com as it is.
func TestScrubberZoneAndGoogleNames(t *testing.T) {
	parallel(t)
	f := &verdictFlags{promURL: "https://prom.example.invalid", gcpProj: "monitoring"}
	scrub := f.scrubber()
	for in, want := range map[string]string{
		"dial tcp [fe80::1%en0]:5432: connect: connection refused":               "dial tcp [address]: connect: connection refused",
		"dial tcp [fe80::1%25en0]:5432: x":                                       "dial tcp [address]: x",
		"at [fd12::17%eth0-1], then [2001:db8::9]":                               "at [address], then [address]",
		"monitoring.googleapis.com said 403 for project monitoring":              "monitoring.googleapis.com said 403 for project [--gcp-project]",
		"writer@monitoring.iam.gserviceaccount.com may not":                      "writer@[--gcp-project].iam.gserviceaccount.com may not",
		"pool monitoring.svc.id.goog, at monitoring.example.com":                 "pool [--gcp-project].svc.id.goog, at monitoring.example.com",
		"monitoring.appspot.com, monitoring.firebaseapp.com, monitoring.web.app": "[--gcp-project].appspot.com, [--gcp-project].firebaseapp.com, [--gcp-project].web.app",
		"https://monitoring.example.com/x and monitoring-2.iam":                  "https://monitoring.example.com/x and monitoring-2.iam",
	} {
		if got := scrub(in); got != want {
			t.Errorf("%q:\n got %q\nwant %q", in, got, want)
		}
	}
}

// refusingPostgres answers every connection as a Postgres whose
// pg_hba.conf doesn't let it in does: a FATAL that names the client's
// address, and the user and the database the client asked for, the
// user's name for a database it didn't. Its address.
func refusingPostgres(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	packet := func(c net.Conn) ([]byte, bool) {
		var n int32
		if binary.Read(c, binary.BigEndian, &n) != nil || n < 8 || n > 1<<16 {
			return nil, false
		}
		b := make([]byte, n-4)
		_, err := io.ReadFull(c, b)
		return b, err == nil
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				b, ok := packet(c)
				if ok && binary.BigEndian.Uint32(b[:4]) == 80877103 { // SSLRequest: no TLS here
					c.Write([]byte{'N'})
					b, ok = packet(c)
				}
				if !ok {
					return
				}
				params := map[string]string{}
				kv := strings.Split(string(b[4:]), "\x00")
				for i := 0; i+1 < len(kv); i += 2 {
					params[kv[i]] = kv[i+1]
				}
				db := cmp.Or(params["database"], params["user"])
				msg := fmt.Sprintf(`no pg_hba.conf entry for host "10.20.30.40", user "%s", database "%s", no encryption`, params["user"], db)
				body := "SFATAL\x00VFATAL\x00C28000\x00M" + msg + "\x00\x00"
				var out bytes.Buffer
				out.WriteByte('E')
				binary.Write(&out, binary.BigEndian, int32(len(body)+4))
				out.WriteString(body)
				c.Write(out.Bytes())
			}(c)
		}
	}()
	return ln.Addr().String()
}

// A Postgres that refuses the connection names the user and the database
// in what it answers: the report says neither, as it says no host, nor
// the password. Named in the string, or taken from PGUSER and PGDATABASE,
// or the user this process runs as.
func TestPreflightPostgresNames(t *testing.T) {
	// Alone, not parallel: t.Setenv.
	addr := refusingPostgres(t)
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		conn, pgUser, pgDatabase string
		hidden                   []string
	}{
		"in the string": {conn: "postgres://ledger_reader_x:pw-0123456789@" + addr + "/ledger_db_x?sslmode=disable&connect_timeout=5",
			hidden: []string{"ledger_reader_x", "ledger_db_x", "pw-0123456789"}},
		"from the environment": {conn: "postgres://" + addr + "/?sslmode=disable&connect_timeout=5", pgUser: "env_reader_x", pgDatabase: "env_db_x",
			hidden: []string{"env_reader_x", "env_db_x"}},
		// Names that are words of the report's own (its item ledger.postgres,
		// lapilli): taken out of what the server said, and of nothing else.
		"words of its own": {conn: "postgres://" + addr + "/?sslmode=disable&connect_timeout=5", pgUser: "lapilli", pgDatabase: "postgres",
			hidden: []string{`"lapilli"`, `"postgres"`}},
		// The user's name may be a word the report says otherwise ("user"):
		// as the server quotes it.
		"this process's user": {conn: "postgres://" + addr + "/?sslmode=disable&connect_timeout=5",
			hidden: []string{`"` + me.Username + `"`}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("PGUSER", c.pgUser)
			t.Setenv("PGDATABASE", c.pgDatabase)
			t.Setenv("PGSERVICE", "")
			f := write(t, "ledger-url", c.conn)
			var out bytes.Buffer
			code := runPreflight([]string{"--ledger-url-file", f, "--alerts", "--prometheus", "http://prom.invalid:9090", "--preflight-timeout", "20s"}, &out, io.Discard)
			text := out.String()
			if flat := strings.Join(strings.Fields(text), " "); code != 1 || !strings.Contains(flat, "problem ledger.postgres") ||
				!strings.Contains(flat, `user "[--ledger-url-file's user]"`) || !strings.Contains(flat, "no pg_hba.conf entry") {
				t.Fatalf("exit %d:\n%s", code, text)
			}
			for _, s := range append(c.hidden, "127.0.0.1", "10.20.30.40") {
				if strings.Contains(text, s) {
					t.Errorf("the report says %q:\n%s", s, text)
				}
			}
		})
	}
}

// What the role serve writes as may write is asked of IAM's policy
// simulator. Without iam:SimulatePrincipalPolicy, IAM refuses, naming the
// role and its account: the item is unknown, never a problem, and says
// what would make it known; the report says neither the account nor the
// role, as the real scrubber has them.
func TestPreflightWritesRedacted(t *testing.T) {
	alone(t)
	was := now
	now = time.Now
	t.Cleanup(func() { now = was })
	sts := newFake(t, "STS", func(w http.ResponseWriter, r *http.Request, form url.Values) bool {
		if form.Get("Action") != "AssumeRole" {
			return false
		}
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, stsAnswer)
		return true
	})
	iam := newFake(t, "IAM", func(w http.ResponseWriter, r *http.Request, form url.Values) bool {
		if form.Get("Action") != "SimulatePrincipalPolicy" {
			return false
		}
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `<ErrorResponse xmlns="https://iam.amazonaws.com/doc/2010-05-08/"><Error><Type>Sender</Type><Code>AccessDenied</Code>`+
			`<Message>User: arn:aws:sts::123456789012:assumed-role/lapilli-rules/lapilli is not authorized to perform: iam:SimulatePrincipalPolicy `+
			`on resource: role lapilli-rules because no identity-based policy allows the iam:SimulatePrincipalPolicy action</Message></Error></ErrorResponse>`)
		return true
	})
	cloudwatch := newFake(t, "CloudWatch", func(w http.ResponseWriter, r *http.Request, _ url.Values) bool {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"__type":"AccessDenied","message":"no"}`)
		return true
	})
	before := http.DefaultTransport
	http.DefaultTransport = &routed{next: before, hosts: map[string]http.Handler{"sts.eu-west-1.amazonaws.com": sts, "iam.amazonaws.com": iam,
		"monitoring.eu-west-1.amazonaws.com": cloudwatch}}
	t.Cleanup(func() { http.DefaultTransport = before })
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
	rules := write(t, "rules.yaml", "groups:\n  - name: g\n    rules:\n      - alert: Down\n        expr: up == 0\n")
	var out bytes.Buffer
	run([]string{"preflight", "--json", "--rules", rules, "--prometheus", "http://127.0.0.1:1", "--alarms", "cw=https://monitoring.eu-west-1.amazonaws.com",
		"--user-header", "X-Email", "--admins", "ops@example.com", "--changes", "--changes-aws-role", "arn:aws:iam::123456789012:role/lapilli-rules",
		"--changes-in", "cw:Checkout", "--check-content=false", "--check-replay=false"}, &out, io.Discard)
	var rep map[string]any
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	it := pfItems(rep)["changes.aws"]
	if it == nil || it["state"] != "unknown" || !strings.Contains(it["fix"].(string), "iam:SimulatePrincipalPolicy on its own ARN") ||
		!strings.Contains(it["why"].(string), "is not authorized to perform: iam:SimulatePrincipalPolicy") {
		t.Errorf("%v", it)
	}
	if iam.count() != 1 {
		t.Errorf("IAM asked %d times", iam.count())
	}
	for _, hidden := range []string{"123456789012", "lapilli-rules"} {
		if strings.Contains(out.String(), hidden) {
			t.Errorf("the report says %q:\n%s", hidden, out.String())
		}
	}
}
