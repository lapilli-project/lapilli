package rules

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const ruleFile = `# Checkout's alerts.
groups:
  - name: checkout
    interval: 30s
    rules:
      # Errors, paged.
      - alert: CheckoutErrors
        expr: rate(http_errors_total{job="checkout"}[5m]) > 0.05 # five percent
        for: 5m # long enough
        labels:
          severity: page
      - alert: CheckoutSlow
        expr: |
          histogram_quantile(0.99, rate(http_seconds_bucket[5m]))
            > 2
        labels:
          severity: ticket

      # Nobody looks at this one.
      - alert: CheckoutQueue
        expr: queue_depth > 100
        for: '10m'
  - name: search
    rules:
      - alert: SearchDown
        expr: up{job="search"} == 0
`

func dur(d time.Duration) *time.Duration { return &d }

// same checks a document after an edit reads as before, but for the rule
// changed, which want says.
func same(t *testing.T, before, after []byte, name string, want func(r *Rule) bool) {
	t.Helper()
	a, err := Parse(before)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse(after)
	if err != nil {
		t.Fatalf("the edited document doesn't parse: %v\n%s", err, after)
	}
	keys := map[string]int{}
	for _, r := range a {
		if r.Name != name {
			keys[r.Key()]++
		}
	}
	var changed *Rule
	for i, r := range b {
		if r.Name == name {
			changed = &b[i]
			continue
		}
		keys[r.Key()]--
	}
	for k, n := range keys {
		if n != 0 {
			t.Errorf("a rule not changed reads otherwise: %s\n%s", k, after)
		}
	}
	if !want(changed) {
		t.Errorf("%s reads %+v\n%s", name, changed, after)
	}
}

func TestEditFor(t *testing.T) {
	out, err := EditDoc([]byte(ruleFile), Edit{Name: "CheckoutErrors", For: dur(15 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "        for: 15m # long enough\n") {
		t.Errorf("the comment after the value goes:\n%s", out)
	}
	if got := Diff("alerts.yaml", []byte(ruleFile), out); !strings.Contains(got, "-        for: 5m # long enough\n+        for: 15m # long enough\n") ||
		strings.Count(got, "\n-") != 1 || strings.Count(got, "\n+") != 2 { // +++ and the line
		t.Errorf("diff:\n%s", got)
	}
	same(t, []byte(ruleFile), out, "CheckoutErrors", func(r *Rule) bool { return r != nil && r.For == 15*time.Minute })

	// Quoted, it stays a duration.
	out, err = EditDoc([]byte(ruleFile), Edit{Name: "CheckoutQueue", For: dur(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	same(t, []byte(ruleFile), out, "CheckoutQueue", func(r *Rule) bool { return r != nil && r.For == time.Hour })
}

func TestEditAddsFor(t *testing.T) {
	// After a block, at the rule's own indent.
	out, err := EditDoc([]byte(ruleFile), Edit{Name: "CheckoutSlow", For: dur(10 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "          severity: ticket\n        for: 10m\n\n      # Nobody looks") {
		t.Errorf("for added where:\n%s", out)
	}
	same(t, []byte(ruleFile), out, "CheckoutSlow", func(r *Rule) bool { return r != nil && r.For == 10*time.Minute })
	// The last rule of a file with no newline at its end.
	in := strings.TrimSuffix(ruleFile, "\n")
	out, err = EditDoc([]byte(in), Edit{Name: "SearchDown", For: dur(2 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	same(t, []byte(in), out, "SearchDown", func(r *Rule) bool { return r != nil && r.For == 2*time.Minute })
}

func TestEditThreshold(t *testing.T) {
	out, err := EditDoc([]byte(ruleFile), Edit{Name: "CheckoutQueue", From: "100", To: "80"})
	if err != nil {
		t.Fatal(err)
	}
	same(t, []byte(ruleFile), out, "CheckoutQueue", func(r *Rule) bool { return r != nil && r.Expr == "queue_depth > 80" })
	// In a block, on its own line.
	out, err = EditDoc([]byte(ruleFile), Edit{Name: "CheckoutSlow", From: "> 2", To: "> 1.5"})
	if err != nil {
		t.Fatal(err)
	}
	same(t, []byte(ruleFile), out, "CheckoutSlow", func(r *Rule) bool { return r != nil && strings.Contains(r.Expr, "> 1.5") })
	// Written twice in the expression, or not at all: which isn't a guess.
	for _, from := range []string{"5", "0.07"} {
		if _, err := EditDoc([]byte(ruleFile), Edit{Name: "CheckoutErrors", From: from, To: "1"}); !errors.Is(err, ErrNotEditable) {
			t.Errorf("%s: %v, want ErrNotEditable", from, err)
		}
	}
	// What is written after the expression, in a comment, isn't in it.
	if _, err := EditDoc([]byte(ruleFile), Edit{Name: "CheckoutErrors", From: "long", To: "short"}); !errors.Is(err, ErrNotEditable) {
		t.Errorf("a word of the next key's comment: %v", err)
	}
}

func TestEditDelete(t *testing.T) {
	out, err := EditDoc([]byte(ruleFile), Edit{Name: "CheckoutErrors", Delete: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "Errors, paged") || !strings.Contains(string(out), "# Nobody looks at this one.\n      - alert: CheckoutQueue") {
		t.Errorf("its comment should go with it, the next one's stay:\n%s", out)
	}
	same(t, []byte(ruleFile), out, "CheckoutErrors", func(r *Rule) bool { return r == nil })
	// The last of a group, with a blank line and a comment before it.
	out, err = EditDoc([]byte(ruleFile), Edit{Name: "CheckoutQueue", Delete: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "Nobody looks") || !strings.Contains(string(out), "  - name: search") {
		t.Errorf("deleting the last of a group:\n%s", out)
	}
	same(t, []byte(ruleFile), out, "CheckoutQueue", func(r *Rule) bool { return r == nil })
	// A group's only rule: the group goes.
	out, err = EditDoc([]byte(ruleFile), Edit{Name: "SearchDown", Delete: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "search") {
		t.Errorf("the group should go:\n%s", out)
	}
	same(t, []byte(ruleFile), out, "SearchDown", func(r *Rule) bool { return r == nil })
	// The file's only rule: a file of no rules isn't for one change.
	only := "groups:\n  - name: x\n    rules:\n      - alert: Only\n        expr: up == 0\n"
	if _, err := EditDoc([]byte(only), Edit{Name: "Only", Delete: true}); !errors.Is(err, ErrNotEditable) {
		t.Errorf("the only rule: %v", err)
	}
}

func TestEditWhichRule(t *testing.T) {
	two := `groups:
  - name: a
    rules:
      - alert: Down
        expr: up{job="a"} == 0
  - name: b
    rules:
      - alert: Down
        expr: up{job="b"} == 0
`
	if _, err := EditDoc([]byte(two), Edit{Name: "Down", For: dur(time.Minute)}); !errors.Is(err, ErrNotEditable) {
		t.Errorf("two rules alike and no group: %v", err)
	}
	out, err := EditDoc([]byte(two), Edit{Name: "Down", Group: "b", For: dur(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(out), "up{job=\"b\"} == 0\n        for: 1m\n") {
		t.Errorf("group b's:\n%s", out)
	}
	if _, err := EditDoc([]byte(two), Edit{Name: "Missing", For: dur(time.Minute)}); !errors.Is(err, ErrNotEditable) {
		t.Errorf("no such rule: %v", err)
	}
	// Written inline, it isn't edited line by line.
	flow := `groups: [{name: a, rules: [{alert: Down, expr: up == 0}, {alert: Up, expr: up == 1}]}]`
	if _, err := EditDoc([]byte(flow), Edit{Name: "Down", For: dur(time.Minute)}); !errors.Is(err, ErrNotEditable) {
		t.Errorf("flow style: %v", err)
	}
}

func TestEditPrometheusRuleAndConfigMap(t *testing.T) {
	pr := `apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: checkout
spec:
  groups:
    - name: checkout
      rules:
        - alert: CheckoutErrors
          expr: errors > 5
          for: 5m
        - alert: Other
          expr: other > 1
`
	out, err := EditDoc([]byte(pr), Edit{Name: "CheckoutErrors", For: dur(20 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	same(t, []byte(pr), out, "CheckoutErrors", func(r *Rule) bool { return r != nil && r.For == 20*time.Minute })

	cm := `apiVersion: v1
kind: ConfigMap
metadata:
  name: rules
data:
  other.yaml: |
    groups:
      - name: other
        rules:
          - alert: Elsewhere
            expr: x > 1
  alerts.yaml: |
    # kept
    groups:
      - name: checkout
        rules:
          - alert: CheckoutErrors
            expr: errors > 5
            for: 5m
          - alert: Other
            expr: other > 1

  extra: plain
`
	out, err = EditDoc([]byte(cm), Edit{Name: "CheckoutErrors", For: dur(20 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "    # kept\n") || !strings.Contains(string(out), "            for: 20m\n") || !strings.Contains(string(out), "\n\n  extra: plain\n") {
		t.Errorf("the ConfigMap:\n%s", out)
	}
	same(t, []byte(cm), out, "CheckoutErrors", func(r *Rule) bool { return r != nil && r.For == 20*time.Minute })
	// A rule held in a quoted string isn't edited.
	quoted := "kind: ConfigMap\ndata:\n  a.yaml: \"groups:\\n- name: a\\n  rules:\\n  - alert: Q\\n    expr: up == 0\\n  - alert: R\\n    expr: up == 1\\n\"\n"
	if _, err := EditDoc([]byte(quoted), Edit{Name: "Q", For: dur(time.Minute)}); !errors.Is(err, ErrNotEditable) {
		t.Errorf("quoted: %v", err)
	}
}

func TestEditGrafana(t *testing.T) {
	g := `apiVersion: 1
groups:
  - orgId: 1
    name: api
    folder: Platform
    interval: 1m
    rules:
      - uid: abc123
        title: API errors
        condition: C
        data:
          - refId: A
            relativeTimeRange: {from: 600, to: 0}
            datasourceUid: prom
            model: {expr: 'rate(errors[5m])', refId: A}
          - refId: C
            datasourceUid: __expr__
            model: {type: threshold, expression: A, refId: C, conditions: [{evaluator: {type: gt, params: [1]}}]}
        for: 5m
      - uid: def456
        title: API slow
        condition: C
        data:
          - refId: A
            relativeTimeRange: {from: 600, to: 0}
            datasourceUid: prom
            model: {expr: 'latency', refId: A}
          - refId: C
            datasourceUid: __expr__
            model: {type: threshold, expression: A, refId: C, conditions: [{evaluator: {type: gt, params: [2]}}]}
        for: 1m
        isPaused: false
`
	out, err := EditDoc([]byte(g), Edit{Name: "API errors", Group: "Platform/api", UID: "abc123", Pause: true})
	if err != nil {
		t.Fatal(err)
	}
	same(t, []byte(g), out, "API errors", func(r *Rule) bool { return r != nil && r.Unsupported == Paused })
	out, err = EditDoc([]byte(g), Edit{Name: "API slow", UID: "def456", Pause: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "        isPaused: true\n") {
		t.Errorf("isPaused set:\n%s", out)
	}
	out, err = EditDoc([]byte(g), Edit{Name: "API slow", UID: "def456", For: dur(10 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	same(t, []byte(g), out, "API slow", func(r *Rule) bool { return r != nil && r.For == 10*time.Minute })
	// A Prometheus rule has no pause.
	if _, err := EditDoc([]byte(ruleFile), Edit{Name: "CheckoutErrors", Pause: true}); !errors.Is(err, ErrNotEditable) {
		t.Errorf("pausing a Prometheus rule: %v", err)
	}
}

func TestDiffNothing(t *testing.T) {
	if d := Diff("a", []byte("x\n"), []byte("x\n")); d != "" {
		t.Errorf("diff of the same: %q", d)
	}
}
