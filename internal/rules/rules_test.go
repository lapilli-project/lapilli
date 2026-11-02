package rules

import (
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
    name: commerce
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
            model: {expr: 'x'}
          - refId: B
            datasourceUid: __expr__
            model: {type: classic_conditions}
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
	if c.Group != "prod/commerce" || c.For != 5*time.Minute || c.Labels["service"] != "checkout" {
		t.Errorf("checkout rule %+v", c)
	}
	l := got["Latency band"].Graph
	if l == nil || l.Queries["A"] != "p99" || l.Steps["C"].Threshold.Type != "outside_range" || l.Steps["C"].Threshold.B != 2 {
		t.Errorf("latency band %+v", l)
	}
	if m := got["Two signals"].Graph; m == nil || m.Steps["M"].Math == nil {
		t.Errorf("math rule %+v (%s)", m, got["Two signals"].Unsupported)
	}
	if r := got["Old style"]; r.Graph != nil || !strings.Contains(r.Unsupported, "classic_conditions") {
		t.Errorf("classic conditions: %+v", r)
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
