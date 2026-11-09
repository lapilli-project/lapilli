package rules

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func TestPrometheusFile(t *testing.T) {
	rs, err := Parse([]byte(`
groups:
  - name: api
    rules:
      - record: job:errors:rate5m
        expr: sum(rate(errors[5m]))
      - alert: HighErrors
        expr: job:errors:rate5m > 5
        for: 10m
        labels: {severity: page, service: api}
      - alert: Down
        expr: up == 0
        for: 1d
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 {
		t.Fatalf("got %d rules, want the 2 alerts and no recording rule: %+v", len(rs), rs)
	}
	if r := rs[0]; r.Name != "HighErrors" || r.Group != "api" || r.Expr != "job:errors:rate5m > 5" || r.For != 10*time.Minute || r.Labels["service"] != "api" {
		t.Errorf("rule %+v", r)
	}
	if rs[1].For != 24*time.Hour {
		t.Errorf("for 1d = %s", rs[1].For)
	}
}

func TestPrometheusRuleResource(t *testing.T) {
	rs, err := Parse([]byte(`
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata: {name: x}
spec:
  groups:
    - name: g
      rules:
        - alert: A
          expr: vector(1)
`))
	if err != nil || len(rs) != 1 || rs[0].Name != "A" {
		t.Errorf("%+v %v", rs, err)
	}
}

const grafanaFile = `
apiVersion: 1
groups:
  - orgId: 1
    name: shop
    folder: prod
    interval: 1m
    rules:
      - uid: a1
        title: Checkout 5xx
        condition: C
        for: 5m
        labels: {service: checkout}
        data:
          - refId: A
            relativeTimeRange: {from: 600, to: 0}
            datasourceUid: gmp
            model: {expr: 'sum(rate(http_errors[5m]))', refId: A}
          - refId: B
            datasourceUid: __expr__
            model: {type: reduce, expression: A, reducer: last, refId: B}
          - refId: C
            datasourceUid: __expr__
            model:
              type: threshold
              expression: B
              conditions: [{evaluator: {type: gt, params: [5]}}]
      - uid: a2
        title: Latency band
        condition: C
        data:
          - refId: A
            relativeTimeRange: {from: 300, to: 0}
            datasourceUid: gmp
            model: {expr: 'p99', instant: true}
          - refId: C
            datasourceUid: __expr__
            model:
              type: threshold
              expression: A
              conditions: [{evaluator: {type: outside_range, params: [0.1, 2]}}]
      - uid: a3
        title: Two signals
        condition: M
        data:
          - refId: A
            relativeTimeRange: {from: 300, to: 0}
            datasourceUid: gmp
            model: {expr: 'errors', instant: true}
          - refId: B
            relativeTimeRange: {from: 300, to: 0}
            datasourceUid: gmp
            model: {expr: 'saturation', instant: true}
          - refId: M
            datasourceUid: __expr__
            model: {type: math, expression: '$A > 5 && ${B} > 0.9'}
      - uid: a4
        title: Old style
        condition: B
        data:
          - refId: A
            datasourceUid: gmp
            relativeTimeRange: {from: 600, to: 0}
            model: {expr: 'x'}
          - refId: B
            datasourceUid: __expr__
            model:
              type: classic_conditions
              conditions:
                - evaluator: {params: [3], type: gt}
                  operator: {type: and}
                  query: {params: [A]}
                  reducer: {type: avg}
                - evaluator: {params: [10, 1], type: within_range}
                  operator: {type: or}
                  query: {params: [A]}
                  reducer: {type: max}
      - uid: a5
        title: Paused
        isPaused: true
        condition: A
        data:
          - refId: A
            datasourceUid: gmp
            model: {expr: 'x'}
`

func TestGrafanaProvisioning(t *testing.T) {
	rs, err := Parse([]byte(grafanaFile))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Rule{}
	for _, r := range rs {
		got[r.Name] = r
	}
	c := got["Checkout 5xx"]
	if c.Unsupported != "" || c.Graph == nil {
		t.Fatalf("checkout: %+v", c)
	}
	if q := c.Graph.Queries["B"]; q != "last_over_time((sum(rate(http_errors[5m])))[10m:])" {
		t.Errorf("reduce over a 10m range query folded to %q", q)
	}
	if th := c.Graph.Steps["C"].Threshold; c.Graph.Cond != "C" || th == nil || th.Input != "B" || th.Type != "gt" || th.A != 5 {
		t.Errorf("threshold %+v cond %s", th, c.Graph.Cond)
	}
	if c.Group != "prod/shop" || c.For != 5*time.Minute || c.Labels["service"] != "checkout" {
		t.Errorf("checkout rule %+v", c)
	}
	l := got["Latency band"].Graph
	if l == nil || l.Queries["A"] != "p99" || l.Steps["C"].Threshold.Type != "outside_range" || l.Steps["C"].Threshold.B != 2 {
		t.Errorf("latency band %+v", l)
	}
	if m := got["Two signals"].Graph; m == nil || m.Steps["M"].Math == nil {
		t.Errorf("math rule %+v (%s)", m, got["Two signals"].Unsupported)
	}
	want := "(count((avg_over_time((x)[10m:])) > 3)) or (count(((max_over_time((x)[10m:])) > 1) < 10))"
	if r := got["Old style"]; r.Graph == nil || r.Graph.Queries["B"] != want || r.Graph.Cond != "B" {
		t.Errorf("classic conditions: %+v\nwant B = %s", r, want)
	}
	odd, _ := Parse([]byte(strings.Replace(grafanaFile, "reducer: {type: avg}", "reducer: {type: diff}", 1)))
	for _, r := range odd {
		if r.Name == "Old style" && !strings.Contains(r.Unsupported, `reducer "diff"`) {
			t.Errorf("a reducer PromQL can't do: %+v", r)
		}
	}
	if r := got["Paused"]; r.Unsupported != "paused" {
		t.Errorf("paused: %+v", r)
	}
	if c.UID != "a1" || c.Digest == "" || c.Digest == got["Latency band"].Digest {
		t.Errorf("uid %q, digest %q: want a1 and a digest of its own", c.UID, c.Digest)
	}
	again, _ := Parse([]byte(grafanaFile))
	edited, _ := Parse([]byte(strings.Replace(grafanaFile, "params: [5]", "params: [6]", 1)))
	if again[0].Digest != c.Digest || edited[0].Digest == c.Digest {
		t.Errorf("digest should follow the rule as written: %s, again %s, edited %s", c.Digest, again[0].Digest, edited[0].Digest)
	}
}

func TestThreshold(t *testing.T) {
	for _, c := range []struct {
		th   Threshold
		v    float64
		want bool
	}{
		{Threshold{Type: "gt", A: 5}, 5, false}, {Threshold{Type: "gte", A: 5}, 5, true},
		{Threshold{Type: "within_range", A: 1, B: 3}, 3, false}, {Threshold{Type: "within_range_included", A: 1, B: 3}, 3, true},
		{Threshold{Type: "outside_range", A: 1, B: 3}, 0.5, true}, {Threshold{Type: "outside_range", A: 1, B: 3}, 2, false},
	} {
		if got := c.th.Pass(c.v); got != c.want {
			t.Errorf("%+v.Pass(%v) = %v", c.th, c.v, got)
		}
	}
}

func TestGraphErrors(t *testing.T) {
	for body, want := range map[string]string{
		"condition: Z\ndata: [{refId: A, datasourceUid: gmp, model: {expr: x}}]":                                                                                  "condition \"Z\"",
		"condition: B\ndata: [{refId: A, datasourceUid: gmp, model: {expr: x}}, {refId: B, datasourceUid: __expr__, model: {type: math, expression: '$A > $Q'}}]": "\"Q\"",
		"condition: B\ndata: [{refId: B, datasourceUid: __expr__, model: {type: reduce, expression: A, reducer: last}}]":                                          "doesn't have",
		"condition: B\ndata: [{refId: A, datasourceUid: gmp, model: {}}, {refId: B, datasourceUid: __expr__, model: {type: math, expression: '$A'}}]":             "no PromQL",
	} {
		var rm map[string]any
		if err := yaml.Unmarshal([]byte(body), &rm); err != nil {
			t.Fatal(err)
		}
		if _, err := grafanaGraph(rm); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want %q", body, err, want)
		}
	}
}

func TestConfigMap(t *testing.T) {
	cm := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: grafana-alert-rules}\ndata:\n  README: just text\n  rules.yaml: |\n" +
		indent(grafanaFile, "    ")
	rs, err := Parse([]byte(cm))
	if err != nil || len(rs) != 5 {
		t.Errorf("%d rules, %v", len(rs), err)
	}
}

func TestDirectory(t *testing.T) {
	// A ConfigMap mounted in a pod: keys are links into ..data, which links
	// to the current version.
	dir := t.TempDir()
	v := filepath.Join(dir, "..2026_11_02_00_00_00.1")
	os.Mkdir(v, 0o700)
	os.WriteFile(filepath.Join(v, "alerts.yaml"), []byte("groups:\n  - name: g\n    rules:\n      - alert: Down\n        expr: up == 0\n"), 0o600)
	os.WriteFile(filepath.Join(v, "dashboard.json"), []byte(`{"title": "not rules"}`), 0o600)
	os.Symlink(filepath.Base(v), filepath.Join(dir, "..data"))
	for _, k := range []string{"alerts.yaml", "dashboard.json"} {
		os.Symlink(filepath.Join("..data", k), filepath.Join(dir, k))
	}
	rs, err := Load(dir)
	if err != nil || len(rs) != 1 || rs[0].Name != "Down" || rs[0].Source != filepath.Join(dir, "alerts.yaml") {
		t.Fatalf("%+v %v: want Down, once, from alerts.yaml", rs, err)
	}
	empty := t.TempDir()
	os.WriteFile(filepath.Join(empty, "x.yaml"), []byte("kind: Deployment\n"), 0o600)
	if _, err := Load(empty); err == nil || !strings.Contains(err.Error(), "no file in it holds alert rules") {
		t.Errorf("a directory without rules: %v", err)
	}
	broken := t.TempDir()
	os.WriteFile(filepath.Join(broken, "x.yaml"), []byte("groups: [\n"), 0o600)
	if _, err := Load(broken); err == nil {
		t.Error("a file that isn't YAML should fail, not be passed over")
	}
}

func TestNotRules(t *testing.T) {
	if _, err := Parse([]byte("kind: Deployment\n")); err == nil || !strings.Contains(err.Error(), "no alert rules") {
		t.Errorf("err = %v", err)
	}
}

func indent(s, pre string) string {
	lines := strings.Split(strings.TrimLeft(s, "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pre + l
		}
	}
	return strings.Join(lines, "\n")
}

func TestKey(t *testing.T) {
	r := Rule{Name: "Errors", Group: "api", Source: "a/rules.yaml", Expr: "errors > 5", For: 5 * time.Minute, Labels: map[string]string{"severity": "page"}}
	moved := r
	moved.Source = "b/rules.yaml"
	if r.Key() != moved.Key() {
		t.Error("the same rule from another file should have the same key")
	}
	for name, change := range map[string]func(*Rule){
		"expr":        func(r *Rule) { r.Expr = "errors > 10" },
		"for":         func(r *Rule) { r.For = 10 * time.Minute },
		"labels":      func(r *Rule) { r.Labels = map[string]string{"severity": "ticket"} },
		"group":       func(r *Rule) { r.Group = "web" },
		"digest":      func(r *Rule) { r.Digest = "x" },
		"unsupported": func(r *Rule) { r.Unsupported = Paused },
	} {
		c := r
		change(&c)
		if c.Key() == r.Key() {
			t.Errorf("a rule with another %s has the same key", name)
		}
	}
}

func TestIntervalAndKeepFiringFor(t *testing.T) {
	rs, err := Parse([]byte(`groups:
  - name: slow
    interval: 5m
    rules:
      - alert: Flapping
        expr: up == 0
        keep_firing_for: 10m
`))
	if err != nil {
		t.Fatal(err)
	}
	if r := rs[0]; r.Interval != 5*time.Minute || r.KeepFiringFor != 10*time.Minute {
		t.Errorf("rule %+v", r)
	}
	other := rs[0]
	other.KeepFiringFor = 0
	if other.Key() == rs[0].Key() {
		t.Error("keep_firing_for changes when a rule resolves: it belongs in the key")
	}
}

func TestCloudFormats(t *testing.T) {
	gmp := `apiVersion: monitoring.googleapis.com/v1
kind: Rules
metadata: {name: shop, namespace: shop}
spec:
  groups:
    - name: shop
      interval: 30s
      rules:
        - alert: CheckoutErrors
          expr: errors > 5
          for: 5m
        - record: job:errors
          expr: sum(errors)
`
	azureGroup := `{
  "type": "Microsoft.AlertsManagement/prometheusRuleGroups",
  "name": "shop-alerts",
  "properties": {
    "interval": "PT1M",
    "rules": [
      {"alert": "CheckoutErrors", "expression": "errors > 5", "for": "PT5M", "severity": 2,
       "resolveConfiguration": {"autoResolved": true, "timeToResolve": "PT10M"}},
      {"alert": "Off", "expression": "up == 0", "enabled": false},
      {"record": "job:errors", "expression": "sum(errors)"}
    ]
  }
}`
	data := base64.StdEncoding.EncodeToString([]byte("groups:\n  - name: shop\n    rules:\n      - alert: CheckoutErrors\n        expr: errors > 5\n"))
	amp := `{"ruleGroupsNamespace": {"name": "shop", "data": "` + data + `", "status": {"statusCode": "ACTIVE"}}}`
	for _, c := range []struct {
		name, doc string
		check     func([]Rule) bool
	}{
		{"GMP Rules", gmp, func(rs []Rule) bool {
			return len(rs) == 1 && rs[0].Interval == 30*time.Second && rs[0].For == 5*time.Minute
		}},
		{"GMP GlobalRules", strings.Replace(gmp, "kind: Rules", "kind: GlobalRules", 1), func(rs []Rule) bool { return len(rs) == 1 }},
		{"kubectl get -o yaml", "apiVersion: v1\nkind: List\nitems:\n" + item(gmp) + item(strings.Replace(gmp, "kind: Rules", "kind: ClusterRules", 1)),
			func(rs []Rule) bool { return len(rs) == 2 }},
		{"Azure rule group", azureGroup, func(rs []Rule) bool {
			r := rs[0]
			return len(rs) == 2 && r.For == 5*time.Minute && r.Interval == time.Minute && r.KeepFiringFor == 10*time.Minute &&
				r.Labels["severity"] == "Sev2" && r.Group == "shop-alerts" && rs[1].Unsupported == Disabled
		}},
		{"az ... list", "[" + azureGroup + "]", func(rs []Rule) bool { return len(rs) == 2 }},
		{"ARM template", `{"$schema": "x", "resources": [` + azureGroup + `]}`, func(rs []Rule) bool { return len(rs) == 2 }},
		{"AMP rule groups namespace", amp, func(rs []Rule) bool { return len(rs) == 1 && rs[0].Expr == "errors > 5" }},
	} {
		rs, err := Parse([]byte(c.doc))
		if err != nil || !c.check(rs) {
			t.Errorf("%s: %+v, %v", c.name, rs, err)
		}
	}
}

// googlePolicy is an alerting policy as the Cloud Monitoring API writes
// one, with a PromQL condition.
const googlePolicyJSON = `{
  "name": "projects/shop/alertPolicies/123",
  "displayName": "shop/CheckoutErrors",
  "severity": "CRITICAL",
  "enabled": true,
  "combiner": "OR",
  "conditions": [{
    "name": "projects/shop/alertPolicies/123/conditions/456",
    "displayName": "errors",
    "conditionPrometheusQueryLanguage": {
      "query": "sum by (service) (rate(errors_total[5m])) > 5",
      "duration": "300s",
      "evaluationInterval": "60s",
      "labels": {"team": "shop"},
      "ruleGroup": "shop",
      "alertRule": "CheckoutErrors"
    }
  }]
}`

func TestGooglePolicies(t *testing.T) {
	threshold := `{"displayName": "CPU", "enabled": true, "conditions": [{"displayName": "cpu high",
  "conditionThreshold": {"filter": "metric.type=\"compute.googleapis.com/instance/cpu/utilization\"", "comparison": "COMPARISON_GT", "thresholdValue": 0.9}}]}`
	bare := `{"displayName": "Down", "conditions": [{"conditionPrometheusQueryLanguage": {"query": "up == 0"}}]}`
	for _, c := range []struct {
		name, doc string
		check     func([]Rule) bool
	}{
		{"a policy", googlePolicyJSON, func(rs []Rule) bool {
			r := rs[0]
			// Closed 270s after the condition last held: 210s after the
			// evaluation that would have resolved it.
			return len(rs) == 1 && r.Name == "CheckoutErrors" && r.Group == "shop" && r.For == 5*time.Minute &&
				r.Interval == time.Minute && r.KeepFiringFor == 210*time.Second && r.Labels["severity"] == "critical" &&
				r.Labels["team"] == "shop" && r.Unsupported == "" && strings.HasPrefix(r.Expr, "sum by (service)")
		}},
		{"the defaults", bare, func(rs []Rule) bool {
			r := rs[0]
			return len(rs) == 1 && r.Name == "Down" && r.Group == "Down" && r.For == 0 && r.Interval == 30*time.Second &&
				r.KeepFiringFor == 240*time.Second && r.Labels["severity"] == ""
		}},
		{"a threshold condition", threshold, func(rs []Rule) bool {
			return len(rs) == 1 && rs[0].Name == "CPU: cpu high" && strings.Contains(rs[0].Unsupported, "conditionThreshold isn't PromQL")
		}},
		{"turned off", strings.Replace(googlePolicyJSON, `"enabled": true`, `"enabled": false`, 1), func(rs []Rule) bool {
			return len(rs) == 1 && rs[0].Unsupported == Disabled
		}},
		{"the API's list", `{"alertPolicies": [` + googlePolicyJSON + `, ` + threshold + `], "nextPageToken": ""}`, func(rs []Rule) bool {
			return len(rs) == 2 && rs[0].Unsupported == "" && rs[1].Unsupported != ""
		}},
		{"gcloud's YAML, a document each", "displayName: Down\nconditions:\n- conditionPrometheusQueryLanguage:\n    query: up == 0\n    duration: 600s\n---\n" +
			"displayName: Slow\nseverity: WARNING\nconditions:\n- conditionPrometheusQueryLanguage:\n    query: latency > 1\n",
			func(rs []Rule) bool {
				return len(rs) == 2 && rs[0].For == 10*time.Minute && rs[1].Labels["severity"] == "warning"
			}},
		{"gcloud's JSON", "[" + googlePolicyJSON + "]", func(rs []Rule) bool { return len(rs) == 1 }},
	} {
		rs, err := Parse([]byte(c.doc))
		if err != nil || !c.check(rs) {
			t.Errorf("%s: %+v, %v", c.name, rs, err)
		}
	}
}

// TestCloudWatchAlarms reads aws cloudwatch describe-alarms: a PromQL
// alarm is replayed, the others are listed with why.
func TestCloudWatchAlarms(t *testing.T) {
	doc := `{
  "MetricAlarms": [
    {"AlarmName": "checkout-5xx", "AlarmArn": "arn:aws:cloudwatch:ap-northeast-2:1:alarm:checkout-5xx", "ActionsEnabled": true,
     "StateValue": "OK", "EvaluationInterval": 60,
     "EvaluationCriteria": {"PromQLCriteria": {"Query": "sum by (service) (rate(http_server_errors[5m])) > 1", "PendingPeriod": 300, "RecoveryPeriod": 600}}},
    {"AlarmName": "cpu-high", "Namespace": "AWS/EC2", "MetricName": "CPUUtilization", "Statistic": "Average", "Period": 300,
     "EvaluationPeriods": 3, "Threshold": 90, "ComparisonOperator": "GreaterThanThreshold"}
  ],
  "CompositeAlarms": [{"AlarmName": "shop-down", "AlarmRule": "ALARM(checkout-5xx) AND ALARM(cpu-high)"}]
}`
	rs, err := Parse([]byte(doc))
	if err != nil || len(rs) != 3 {
		t.Fatalf("%+v, %v", rs, err)
	}
	if r := rs[0]; r.Name != "checkout-5xx" || r.For != 5*time.Minute || r.KeepFiringFor != 10*time.Minute || r.Interval != time.Minute ||
		r.Unsupported != "" || !strings.HasPrefix(r.Expr, "sum by (service)") {
		t.Errorf("PromQL alarm: %+v", r)
	}
	if !strings.Contains(rs[1].Unsupported, "isn't PromQL") || !strings.Contains(rs[2].Unsupported, "composite") {
		t.Errorf("the others: %+v", rs[1:])
	}
	for _, bad := range []string{`"soon"`, `"NaN"`, `-60`, `1e300`} {
		if _, err := Parse([]byte(`{"MetricAlarms": [{"AlarmName": "x", "EvaluationInterval": ` + bad + `, "EvaluationCriteria": {"PromQLCriteria": {"Query": "up"}}}]}`)); err == nil {
			t.Errorf("an interval of %s should fail", bad)
		}
	}
	// Rule groups win over what only looks like a policy.
	rs, err = Parse([]byte("groups:\n  - name: g\n    rules:\n      - alert: A\n        expr: up == 0\nconditions:\n  - conditionThreshold: {}\n"))
	if err != nil || len(rs) != 1 || rs[0].Name != "A" {
		t.Errorf("groups beside conditions: %+v, %v", rs, err)
	}
}

func TestISODuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"PT5M": 5 * time.Minute, "P1DT2H": 26 * time.Hour, "PT1M30S": 90 * time.Second, "5m": 5 * time.Minute, "": 0} {
		if got, err := isoDuration(in); err != nil || got != want {
			t.Errorf("isoDuration(%q) = %s, %v; want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{"P1M", "PT", "P1Y", "PT5X"} {
		if _, err := isoDuration(bad); err == nil {
			t.Errorf("isoDuration(%q) should fail", bad)
		}
	}
}

func TestFromAPI(t *testing.T) {
	rs, err := FromAPI([]byte(`{"status":"success","data":{"groups":[{"name":"g","interval":60,"rules":[
		{"type":"alerting","name":"High","query":" x > 1 ","duration":600,"keepFiringFor":300,"labels":{"severity":"page"}},
		{"type":"recording","name":"r","query":"sum(x)"}]}]}}`))
	if err != nil || len(rs) != 1 {
		t.Fatalf("%+v, %v", rs, err)
	}
	if r := rs[0]; r.Expr != "x > 1" || r.For != 10*time.Minute || r.KeepFiringFor != 5*time.Minute || r.Interval != time.Minute || r.Labels["severity"] != "page" {
		t.Errorf("rule %+v", r)
	}
	if _, err := FromAPI([]byte(`{"status":"error","error":"forbidden"}`)); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("an error answer: %v", err)
	}
}

func TestDirectoryBelow(t *testing.T) {
	dir := t.TempDir()
	for p, body := range map[string]string{
		"team-a/rules.yaml":   "groups:\n  - name: a\n    rules:\n      - alert: A\n        expr: up == 0\n",
		"team-b/x/rules.yaml": "groups:\n  - name: b\n    rules:\n      - alert: B\n        expr: up == 0\n",
		".git/rules.yaml":     "groups:\n  - name: hidden\n    rules:\n      - alert: Hidden\n        expr: up == 0\n",
		"README.md":           "# not rules\n",
	} {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rs, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range rs {
		names = append(names, r.Name)
	}
	if strings.Join(names, " ") != "A B" {
		t.Errorf("rules %v, want A and B from below, not the hidden one", names)
	}
}

// item makes a YAML document one item of a list.
func item(doc string) string {
	lines := strings.Split(strings.TrimRight(doc, "\n"), "\n")
	for i, l := range lines {
		if i == 0 {
			lines[i] = "  - " + l
		} else {
			lines[i] = "    " + l
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

func TestDirectoryLinkAndTemplates(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	os.MkdirAll(filepath.Join(real, "chart", "templates"), 0o755)
	os.WriteFile(filepath.Join(real, "alerts.yaml"), []byte("groups:\n  - name: g\n    rules:\n      - alert: Down\n        expr: up == 0\n"), 0o600)
	// A Helm template below the top isn't YAML until rendered.
	os.WriteFile(filepath.Join(real, "chart", "templates", "rules.yaml"), []byte("groups:\n{{- range .Values.x }}\n  - {{ . }}: [\n{{- end }}\n"), 0o600)
	link := filepath.Join(dir, "link")
	os.Symlink(real, link)
	rs, err := Load(link)
	if err != nil || len(rs) != 1 || rs[0].Source != filepath.Join(link, "alerts.yaml") {
		t.Fatalf("%+v %v: want Down, read through the link", rs, err)
	}
}

func TestScopedCloudRules(t *testing.T) {
	rs, err := Parse([]byte(`apiVersion: monitoring.googleapis.com/v1
kind: Rules
metadata: {name: shop, namespace: shop}
spec:
  groups:
    - name: shop
      rules:
        - alert: Errors
          expr: sum(rate(errors[5m])) > 5
`))
	if err != nil || len(rs) != 1 || rs[0].Expr != `sum(rate(errors{namespace="shop"}[5m])) > 5` {
		t.Errorf("GMP Rules scoped to their namespace: %+v %v", rs, err)
	}
	rs, err = Parse([]byte(`{"type": "Microsoft.AlertsManagement/prometheusRuleGroups", "name": "g",
  "properties": {"clusterName": "aks-1", "rules": [{"alert": "Errors", "expression": "errors > 5",
    "resolveConfiguration": {"autoResolved": false}}]}}`))
	if err != nil || len(rs) != 1 || rs[0].Expr != `errors{cluster="aks-1"} > 5` || rs[0].KeepFiringFor != never {
		t.Errorf("Azure, scoped to its cluster, resolved by hand: %+v %v", rs, err)
	}
}

func TestGrafanaKeepAndCount(t *testing.T) {
	rs, err := Parse([]byte(`apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    interval: 1m
    rules:
      - uid: k1
        title: Kept
        condition: B
        keepFiringFor: 5m
        data:
          - refId: A
            datasourceUid: prom
            relativeTimeRange: {from: 600, to: 0}
            model: {expr: errors, intervalMs: 30000}
          - refId: B
            datasourceUid: __expr__
            model:
              type: classic_conditions
              conditions:
                - evaluator: {params: [20], type: gt}
                  operator: {type: and}
                  query: {params: [A]}
                  reducer: {type: count}
`))
	if err != nil || len(rs) != 1 {
		t.Fatalf("%+v %v", rs, err)
	}
	if r := rs[0]; r.KeepFiringFor != 5*time.Minute || r.Graph.Queries["B"] != "count((count_over_time((errors)[10m:30s])) > 20)" {
		t.Errorf("keepFiringFor %s, B = %s", r.KeepFiringFor, r.Graph.Queries["B"])
	}
}

// A Grafana expression that refers back to itself can't be replayed, and
// says so, rather than recursing until the stack runs out.
func TestGrafanaCycle(t *testing.T) {
	for _, math := range []string{`$A + $B`, `$C * 2`} {
		rs, err := Parse([]byte(`
apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    rules:
      - uid: c1
        title: Loop
        condition: B
        data:
          - refId: A
            datasourceUid: prom
            model: {expr: 'vector(1)', instant: true}
          - refId: B
            datasourceUid: __expr__
            model: {type: math, expression: '` + math + `'}
          - refId: C
            datasourceUid: __expr__
            model: {type: math, expression: '$B + 1'}
`))
		if err != nil || len(rs) != 1 || !strings.Contains(rs[0].Unsupported, "refers back to itself") {
			t.Errorf("%s: %+v %v", math, rs, err)
		}
	}
}
