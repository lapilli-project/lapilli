package rules

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Adding a rule (docs/design/expectations.md, step 5): read strictly, and
// written by Lapilli into the place given, nothing else changed.

func TestParseAdded(t *testing.T) {
	a, err := ParseAdded([]byte("alert: CheckoutSlow\nexpr: latency > 2\nfor: 10m\nlabels:\n  severity: page\nannotations:\n  summary: Checkout is slow.\n"))
	if err != nil || a.Alert != "CheckoutSlow" || a.Expr != "latency > 2" || a.For != 10*time.Minute || a.Labels["severity"] != "page" ||
		a.Annotations["summary"] != "Checkout is slow." {
		t.Fatalf("%+v %v", a, err)
	}
	for _, bad := range []string{
		"alert: X\nexpr: up\nrecord: y\n",                             // a recording rule
		"alert: X\nexpr: up\nexpr: down\n",                            // twice
		"alert: X\nexpr: up\nnotes: hi\n",                             // a key it doesn't have
		"alert: X\nexpr: up\nlabels: {team: 7}\n",                     // a number, unquoted
		"alert: X\nexpr: up\nfor: 72h\n",                              // past 48h
		"alert: X\nexpr: up\nfor: soon\n",                             // not a duration
		"alert: 9lives\nexpr: up\n",                                   // a name a metric can't have
		"alert: \"X; rm -rf /\"\nexpr: up\n",                          // nor this one
		"alert: X\nexpr: up\nlabels: {__name__: x}\n",                 // Prometheus's own
		"alert: X\nexpr: up\nlabels: {alertname: y}\n",                // the rule's name
		"alert: X\nexpr: \"up\\u202e\"\n",                             // a character that doesn't show
		"alert: X\nexpr: up\nlabels: &a {team: x}\nannotations: *a\n", // an anchor and an alias
		"alert: X\nexpr: !!str up\n",                                  // a tag
		"alert: X\nexpr: up\n---\nalert: Y\nexpr: up\n",               // two documents
		"- alert: X\n  expr: up\n",                                    // a list
		"groups:\n- name: g\n  rules: []\n",                           // a file of rules
		"alert: X\n",                                                  // no expr
	} {
		if _, err := ParseAdded([]byte(bad)); err == nil {
			t.Errorf("read %q", bad)
		}
	}
}

func added(t *testing.T) Added {
	t.Helper()
	a, err := ParseAdded([]byte("alert: CheckoutSlow\nexpr: |-\n  latency\n    > 2\nfor: 10m\nlabels: {severity: page}\nannotations:\n  summary: \"Slow: {{ $labels.service }}\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAddRule(t *testing.T) {
	a := added(t)
	file := "# shop's rules\ngroups:\n  - name: shop\n    rules:\n      - alert: Errors # noisy\n        expr: errors > 5\n        for: 2m\n  - name: other\n    rules:\n      - alert: Down\n        expr: up == 0\n# the end\n"
	out, err := AddRule([]byte(file), "shop", false, 0, a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "# shop's rules\ngroups:\n  - name: shop\n    rules:\n      - alert: Errors # noisy\n        expr: errors > 5\n        for: 2m\n      - alert: \"CheckoutSlow\"\n") ||
		!strings.HasSuffix(string(out), "  - name: other\n    rules:\n      - alert: Down\n        expr: up == 0\n# the end\n") {
		t.Errorf("added:\n%s", out)
	}
	rs, err := Parse(out)
	if err != nil || len(rs) != 3 || rs[1].Name != "CheckoutSlow" || rs[1].Group != "shop" || rs[1].Expr != "latency\n  > 2" || rs[1].For != 10*time.Minute {
		t.Fatalf("read back: %+v %v", rs, err)
	}
	// A new group, after the last.
	out, err = AddRule([]byte(file), "slow", true, 30*time.Second, a)
	if err != nil {
		t.Fatal(err)
	}
	rs, _ = Parse(out)
	if len(rs) != 3 || rs[2].Group != "slow" || rs[2].Interval != 30*time.Second || !strings.HasPrefix(string(out), file[:len(file)-len("# the end\n")]) {
		t.Errorf("a new group:\n%s", out)
	}
	// Refused: a group not there, a new one of a name that is, one written
	// inline.
	for _, x := range []struct {
		file, group string
		new         bool
	}{{file, "nope", false}, {file, "shop", true}, {"groups:\n  - name: shop\n    rules: [{alert: E, expr: up}]\n", "shop", false},
		{"groups: [{name: shop, rules: []}]\n", "slow", true}} {
		if _, err := AddRule([]byte(x.file), x.group, x.new, 0, a); !errors.Is(err, ErrNoPlace) {
			t.Errorf("%q %q: %v", x.file, x.group, err)
		}
	}
	// A Grafana provisioning file holds Grafana's rules, not these.
	grafana := "apiVersion: 1\ngroups:\n  - orgId: 1\n    name: shop\n    folder: Shop\n    interval: 1m\n    rules:\n      - uid: a\n        title: A\n        condition: A\n        data: []\n"
	if _, err := AddRule([]byte(grafana), "Shop/shop", false, 0, a); !errors.Is(err, ErrNoPlace) {
		t.Errorf("a Grafana file: %v", err)
	}
}

func TestAddRuleKubernetes(t *testing.T) {
	a := added(t)
	pr := "apiVersion: monitoring.coreos.com/v1\nkind: PrometheusRule\nmetadata:\n  name: shop\nspec:\n  groups:\n  - name: shop\n    rules:\n    - alert: Errors\n      expr: errors > 5\n---\nkind: ServiceMonitor\nmetadata:\n  name: shop\n"
	out, err := AddRule([]byte(pr), "shop", false, 0, a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(out), "---\nkind: ServiceMonitor\nmetadata:\n  name: shop\n") || !strings.Contains(string(out), "    - alert: \"CheckoutSlow\"\n") {
		t.Errorf("a PrometheusRule:\n%s", out)
	}
	cm := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: rules\ndata:\n  shop.yaml: |\n    groups:\n      - name: shop\n        rules:\n          - alert: Errors\n            expr: errors > 5\n  other.yaml: |\n    groups:\n      - name: other\n        rules:\n          - alert: Down\n            expr: up == 0\n"
	out, err = AddRule([]byte(cm), "shop", false, 0, a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "          - alert: \"CheckoutSlow\"\n") || !strings.HasSuffix(string(out), "  other.yaml: |\n    groups:\n      - name: other\n        rules:\n          - alert: Down\n            expr: up == 0\n") {
		t.Errorf("a ConfigMap:\n%s", out)
	}
	// The group in two entries: which is a guess.
	two := strings.Replace(cm, "name: other", "name: shop", 1)
	if _, err := AddRule([]byte(two), "shop", false, 0, a); !errors.Is(err, ErrNoPlace) {
		t.Errorf("two entries: %v", err)
	}
}

func TestAddsRule(t *testing.T) {
	a := added(t)
	unit := []byte("groups:\n- name: shop\n  rules:\n  - alert: Errors\n    expr: errors > 5\n")
	out, err := AddRule(unit, "shop", false, 0, a)
	if err != nil || !AddsRule(unit, out, "shop", 0, a) {
		t.Fatalf("%s %v", out, err)
	}
	// One more than the rule: not only it.
	more, _ := AddRule(out, "shop", false, 0, Added{Alert: "Other", Expr: "up"})
	if AddsRule(unit, more, "shop", 0, a) {
		t.Error("two rules taken for one")
	}
	// A group made, from none.
	made, err := AddRule([]byte("groups:\n- name: x\n  rules:\n  - alert: Y\n    expr: up\n"), "slow", true, 0, a)
	if err != nil {
		t.Fatal(err)
	}
	_ = made
	alone := []byte("groups:\n- name: \"slow\"\n  rules:\n  - alert: \"CheckoutSlow\"\n    expr: |-\n      latency\n        > 2\n    for: 10m\n    labels:\n      severity: \"page\"\n    annotations:\n      summary: \"Slow: {{ $labels.service }}\"\n")
	if !AddsRule(nil, alone, "slow", 0, a) {
		t.Error("a group made of the rule alone")
	}
}

// A file of two PrometheusRules, each holding groups: a rule is added to
// either's group, the other document as it was; a new group fits neither
// (which would take it isn't the file's to say), and isn't offered. One
// PrometheusRule beside a document of another kind takes a new group.
func TestAddRuleTwoDocuments(t *testing.T) {
	a := added(t)
	two := "apiVersion: monitoring.coreos.com/v1\nkind: PrometheusRule\nmetadata:\n  name: a\nspec:\n  groups:\n  - name: one\n    rules:\n    - alert: A\n      expr: a > 1\n---\n" +
		"apiVersion: monitoring.coreos.com/v1\nkind: PrometheusRule\nmetadata:\n  name: b\nspec:\n  groups:\n  - name: two\n    rules:\n    - alert: B\n      expr: b > 1\n"
	first, second, _ := strings.Cut(two, "---\n")
	for _, g := range []string{"one", "two"} {
		out, err := AddRule([]byte(two), g, false, 0, a)
		if err != nil {
			t.Fatalf("group %s: %v", g, err)
		}
		rs, err := FromDocs([]Doc{{Source: "two.yaml", Data: out}})
		if err != nil || len(rs) != 3 {
			t.Fatalf("group %s read back: %+v %v", g, rs, err)
		}
		for _, r := range rs {
			if r.Name == "CheckoutSlow" && r.Group != g {
				t.Errorf("added to %s, not %s", r.Group, g)
			}
		}
		// The other document, line for line as it was.
		if g == "one" && !strings.HasSuffix(string(out), "---\n"+second) || g == "two" && !strings.HasPrefix(string(out), first+"---\n") {
			t.Errorf("group %s: the other document changed:\n%s", g, out)
		}
	}
	if _, err := AddRule([]byte(two), "three", true, 0, a); !errors.Is(err, ErrNoPlace) {
		t.Errorf("a new group in two PrometheusRules: %v", err)
	}
	pr := "apiVersion: monitoring.coreos.com/v1\nkind: PrometheusRule\nmetadata:\n  name: shop\nspec:\n  groups:\n  - name: shop\n    rules:\n    - alert: Errors\n      expr: errors > 5\n---\nkind: ServiceMonitor\nmetadata:\n  name: shop\n"
	cm := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: rules\ndata:\n  shop.yaml: |\n    groups:\n      - name: shop\n        rules:\n          - alert: Errors\n            expr: errors > 5\n"
	for file, want := range map[string]bool{
		two: false, pr: true, cm: true, "groups:\n  - name: shop\n    rules:\n      - alert: E\n        expr: up\n": true,
		strings.Replace(cm, "  shop.yaml: |", "  other.yaml: |\n    groups:\n      - name: other\n        rules:\n          - alert: D\n            expr: up\n  shop.yaml: |", 1): false,
		"kind: ServiceMonitor\nmetadata:\n  name: shop\n": false,
	} {
		if got := NewGroupFits([]byte(file)); got != want {
			t.Errorf("a new group fits %v, not %v:\n%s", got, want, file)
		}
		if _, err := AddRule([]byte(file), "slow", true, 0, a); (err == nil) != want {
			t.Errorf("a new group added: %v, offered %v:\n%s", err, want, file)
		}
	}
}

// An added rule's labels are plain text: Prometheus and Grafana expand a
// label's template at each evaluation, and {{ $value }} makes each one a
// new alert, which the backtest, keeping the label as written, wouldn't
// count. Its annotations may be templates.
func TestAddedLabelsPlain(t *testing.T) {
	for _, l := range []string{"'{{ $value }}'", "'{{ $labels.service }}'", "'x{{ humanize 1 }}'", "'{{'"} {
		if _, err := ParseAdded([]byte("alert: X\nexpr: up\nlabels:\n  value: " + l + "\n")); err == nil || !strings.Contains(err.Error(), "template") {
			t.Errorf("a label %s: %v", l, err)
		}
	}
	a, err := ParseAdded([]byte("alert: X\nexpr: up\nlabels:\n  severity: page\n  note: '{ not a template }'\nannotations:\n  summary: '{{ $value }} on {{ $labels.service }}'\n"))
	if err != nil || a.Annotations["summary"] != "{{ $value }} on {{ $labels.service }}" {
		t.Errorf("plain labels and a templated annotation: %+v %v", a, err)
	}
}
