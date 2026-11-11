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

func TestEditThresholdTellsApart(t *testing.T) {
	doc := `groups:
  - name: a
    rules:
      - alert: Errors
        expr: rate(errors_total[5m]) > 5
        annotations:
          summary: more than 5 a second
`
	rs, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	r := rs[0]
	b := r.Bounds()[0]
	out, err := EditThreshold([]byte(doc), r, Edit{Name: r.Name, Group: r.Group}, b, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "expr: rate(errors_total[5m]) > 3\n") || !strings.Contains(string(out), "more than 5 a second") {
		t.Errorf("the threshold, not the range or the annotation:\n%s", out)
	}
}

// What is checked: an edit that took something else with it, or changed
// another rule, isn't given back.
func TestCheckRefusesMore(t *testing.T) {
	e := Edit{Name: "CheckoutErrors", For: dur(15 * time.Minute)}
	good, err := EditDoc([]byte(ruleFile), e)
	if err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"a label gone":         strings.Replace(string(good), "          severity: page\n", "", 1),
		"another rule changed": strings.Replace(string(good), "queue_depth > 100", "queue_depth > 99", 1),
		"nothing changed":      ruleFile,
		"not YAML":             string(good) + "\n  - : : [",
	} {
		if err := Check([]byte(ruleFile), []byte(bad), e); !errors.Is(err, ErrNotEditable) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := Check([]byte(ruleFile), good, e); err != nil {
		t.Errorf("the edit itself: %v", err)
	}
}

// An edit the lines can't make as asked, such as of a value an anchor
// shares, isn't given back: what is written is read back first.
func TestEditReadBack(t *testing.T) {
	doc := `groups:
  - name: a
    rules:
      - alert: A
        expr: up == 0
        for: &wait 5m
      - alert: B
        expr: up == 1
        for: *wait
`
	if out, err := EditDoc([]byte(doc), Edit{Name: "A", For: dur(15 * time.Minute)}); !errors.Is(err, ErrNotEditable) {
		t.Errorf("an anchored for: %v\n%s", err, out)
	}
}

// changedLines are the lines of after that aren't lines of before, and how
// many of before's are gone: an edit of one annotation touches one line.
func changedLines(before, after string) (added []string, gone int) {
	b, a := strings.SplitAfter(before, "\n"), strings.SplitAfter(after, "\n")
	left := map[string]int{}
	for _, l := range b {
		left[l]++
	}
	for _, l := range a {
		if left[l] > 0 {
			left[l]--
			continue
		}
		added = append(added, l)
	}
	for _, n := range left {
		gone += n
	}
	return added, gone
}

func TestEditAnnotation(t *testing.T) {
	doc := `groups:
  - name: checkout
    rules:
      - alert: Errors
        expr: errors > 5 # page
        for: 5m
        labels:
          severity: page
        annotations:
          summary: Errors   # short
          description: |
            Checkout is failing.
            See the dashboard.
          runbook_url: http://wiki/old
      - alert: Bare
        expr: up == 0
`
	for name, c := range map[string]struct {
		e           Edit
		added, gone int
		says        func(Says) bool
	}{
		"set a summary": {Edit{Name: "Errors", Annotation: "summary", Value: "Checkout errors on {{ $labels.instance }}"}, 1, 1,
			func(s Says) bool {
				return s.Summary == "Checkout errors on {{ $labels.instance }}" && s.Description != ""
			}},
		"set a block's": {Edit{Name: "Errors", Annotation: "description", Value: "Two\nlines"}, 1, 3,
			func(s Says) bool { return s.Description == "Two\nlines" && s.Runbook == "http://wiki/old" }},
		"set the last": {Edit{Name: "Errors", Annotation: "runbook_url", Value: "https://wiki/checkout"}, 1, 1,
			func(s Says) bool { return s.Runbook == "https://wiki/checkout" }},
		"add one": {Edit{Name: "Errors", Annotation: "runbook", Value: "https://wiki/x"}, 1, 0,
			func(s Says) bool { return s.Runbook == "http://wiki/old" }},
		"take one out": {Edit{Name: "Errors", Annotation: "description", Unset: true}, 0, 3,
			func(s Says) bool { return s.Description == "" && s.Summary == "Errors" }},
		"annotations made": {Edit{Name: "Bare", Annotation: "summary", Value: "Down"}, 2, 0,
			func(s Says) bool { return s.Summary == "Down" }},
		// What looks like more YAML is one value.
		"a value that looks like keys": {Edit{Name: "Errors", Annotation: "summary", Value: "x\"\n  severity: none\nfor: 0s #"}, 1, 1,
			func(s Says) bool { return s.Summary == "x\"\n  severity: none\nfor: 0s #" }},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := EditDoc([]byte(doc), c.e)
			if err != nil {
				t.Fatal(err)
			}
			added, gone := changedLines(doc, string(out))
			if len(added) != c.added || gone != c.gone {
				t.Errorf("%d lines added, %d gone, want %d and %d:\n%s", len(added), gone, c.added, c.gone, out)
			}
			// When it fires is as it was, to the byte: only what it says moved.
			a, _ := Parse([]byte(doc))
			b, err := Parse(out)
			if err != nil || len(b) != len(a) {
				t.Fatalf("reads as %d rules: %v\n%s", len(b), err, out)
			}
			for i := range a {
				if a[i].Key() != b[i].Key() {
					t.Errorf("%s fires otherwise: %s", a[i].Name, b[i].Key())
				}
				if a[i].Name == c.e.Name && !c.says(b[i].Says) {
					t.Errorf("says %+v", b[i].Says)
				}
				if a[i].Name != c.e.Name && a[i].Says != b[i].Says {
					t.Errorf("%s says otherwise", a[i].Name)
				}
			}
		})
	}
	for name, e := range map[string]Edit{
		"nothing to take out": {Name: "Bare", Annotation: "summary", Unset: true},
		"not there":           {Name: "Errors", Annotation: "runbook", Unset: true},
		"a key that isn't":    {Name: "Errors", Annotation: "summary: x\n  y", Value: "z"},
		"a carriage return":   {Name: "Errors", Annotation: "summary", Value: "a\rb"},
	} {
		if _, err := EditDoc([]byte(doc), e); !errors.Is(err, ErrNotEditable) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The only annotation taken out: annotations go.
	one := "groups:\n  - name: a\n    rules:\n      - alert: A\n        expr: up == 0\n        annotations:\n          summary: A\n      - alert: B\n        expr: up == 1\n"
	out, err := EditDoc([]byte(one), Edit{Name: "A", Annotation: "summary", Unset: true})
	if err != nil || strings.Contains(string(out), "annotations") {
		t.Errorf("the only one: %v\n%s", err, out)
	}
	// Written inline, they aren't edited.
	inline := "groups:\n  - name: a\n    rules:\n      - alert: A\n        expr: up == 0\n        annotations: {summary: A}\n"
	if _, err := EditDoc([]byte(inline), Edit{Name: "A", Annotation: "summary", Value: "B"}); !errors.Is(err, ErrNotEditable) {
		t.Errorf("inline annotations: %v", err)
	}
}

// A Grafana provisioning file's rule, and a ConfigMap's.
func TestEditAnnotationElsewhere(t *testing.T) {
	g := `apiVersion: 1
groups:
  - orgId: 1
    name: api
    folder: Platform
    interval: 1m
    rules:
      - uid: abc123
        title: API errors
        condition: A
        data:
          - refId: A
            datasourceUid: prom
            model: {expr: 'errors > 1', refId: A}
        for: 5m
        annotations:
          __dashboardUid__: d1
          summary: errors
      - uid: def456
        title: Other
        condition: A
        data:
          - refId: A
            datasourceUid: prom
            model: {expr: 'other > 1', refId: A}
`
	out, err := EditDoc([]byte(g), Edit{Name: "API errors", UID: "abc123", Annotation: "runbook_url", Value: "https://wiki/api"})
	if err != nil {
		t.Fatal(err)
	}
	if added, gone := changedLines(g, string(out)); len(added) != 1 || gone != 0 || !strings.Contains(string(out), "__dashboardUid__: d1") {
		t.Errorf("added %v, %d gone:\n%s", added, gone, out)
	}
	cm := "kind: ConfigMap\ndata:\n  a.yaml: |\n    groups:\n      - name: a\n        rules:\n          - alert: A\n            expr: up == 0\n          - alert: B\n            expr: up == 1\n"
	out, err = EditDoc([]byte(cm), Edit{Name: "B", Annotation: "summary", Value: "B is up"})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := Parse(out)
	if err != nil || len(rs) != 2 || rs[1].Says.Summary != "B is up" || rs[0].Says.Summary != "" {
		t.Errorf("the ConfigMap's: %v %+v\n%s", err, rs, out)
	}
}
