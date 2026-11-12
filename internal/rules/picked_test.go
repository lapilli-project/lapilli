package rules

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// rendered is a chart as helm template writes it: Grafana's rules in
// ConfigMaps labelled for its sidecar, a Prometheus rule file in another
// ConfigMap, a PrometheusRule, and what else a chart holds.
const rendered = `---
# Source: monitoring/templates/serviceaccount.yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: shop
---
# Source: monitoring/templates/secret.yaml
apiVersion: v1
kind: Secret
metadata:
  name: shop
stringData:
  password: hunter2
---
# Source: monitoring/templates/empty.yaml
---
# Source: monitoring/templates/grafana-alerts.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: shop-grafana-alerts
  labels:
    grafana_alert: "1"
    team: shop
data:
  shop.yaml: |
    apiVersion: 1
    groups:
      - orgId: 1
        name: shop
        folder: prod
        interval: 1m
        rules:
          - uid: g1
            title: Checkout errors
            condition: C
            for: 5m
            data:
              - refId: A
                relativeTimeRange: {from: 600, to: 0}
                datasourceUid: prom
                model: {expr: 'sum(rate(http_errors_total[5m]))'}
              - refId: C
                datasourceUid: __expr__
                model:
                  type: threshold
                  expression: A
                  conditions: [{evaluator: {type: gt, params: [5]}}]
---
# Source: monitoring/templates/prometheus-rules.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: shop-prometheus-rules
  labels:
    team: shop
data:
  rules.yaml: |
    groups:
      - name: shop
        rules:
          - alert: ShopDown
            expr: up{job="shop"} == 0
            for: 5m
---
# Source: monitoring/templates/nginx.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: shop-nginx
  labels:
    grafana_alert: "1"
data:
  nginx.conf: |
    server { listen 80; }
---
# Source: monitoring/templates/deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: shop
spec:
  template:
    spec:
      containers:
        - name: shop
          image: shop:1
          args:
            - |
              groups: not rules
              ---
              a block of text with a marker that isn't at the start of a line
---
# Source: monitoring/templates/prometheusrule.yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: shop-latency
  labels:
    grafana_alert: "1"
spec:
  groups:
    - name: latency
      rules:
        - alert: ShopSlow
          expr: histogram_quantile(0.99, rate(http_seconds_bucket[5m])) > 2
`

func names(rs []Rule) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Name)
	}
	slices.Sort(out)
	return out
}

// A rendered chart is read for its rules alone: the documents of other
// kinds are passed over and counted, and, trimmed, aren't kept with the
// rules (the Secret's password among them). The documents kept read again
// as the same rules; each is kept as it was written, its comments too.
func TestReadPickedRendered(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.yaml")
	if err := os.WriteFile(path, []byte(rendered), 0o644); err != nil {
		t.Fatal(err)
	}
	rs, docs, picked, err := ReadPicked(path, Picking{Trim: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(rs); !slices.Equal(got, []string{"Checkout errors", "ShopDown", "ShopSlow"}) {
		t.Errorf("rules %v", got)
	}
	// The ServiceAccount, the Secret, nginx's ConfigMap and the Deployment;
	// the template that rendered nothing isn't a document.
	if picked != (Picked{Other: 4}) || picked.String() != "4 documents that hold no rules" {
		t.Errorf("picked %+v %q", picked, picked)
	}
	if len(docs) != 1 || bytes.Contains(docs[0].Data, []byte("hunter2")) || bytes.Contains(docs[0].Data, []byte("kind: Deployment")) {
		t.Errorf("kept %s", docs[0].Data)
	}
	if !bytes.Contains(docs[0].Data, []byte("# Source: monitoring/templates/prometheusrule.yaml\napiVersion")) {
		t.Errorf("not kept as written: %s", docs[0].Data)
	}
	again, err := FromDocs(docs)
	if err != nil || !slices.Equal(names(again), names(rs)) {
		t.Errorf("read again: %v %v", names(again), err)
	}

	// Untrimmed, the file is kept as it is: a file of rules is changed
	// through git as it is (docs/design/changes.md).
	_, docs, picked, err = ReadPicked(path, Picking{})
	if err != nil || picked.Other != 4 || !bytes.Equal(docs[0].Data, []byte(rendered)) {
		t.Errorf("untrimmed: %+v %v", picked, err)
	}
	if _, docs, err := Read(path); err != nil || !bytes.Equal(docs[0].Data, []byte(rendered)) {
		t.Errorf("Read: %v", err)
	}
}

// A selector keeps the labelled objects among the documents of rules, and
// counts those it leaves out apart from those that hold no rules. What it
// leaves out is never kept, trimmed or not, so reading the documents again
// leaves it out too. A rule file that isn't an object has no labels, and is
// kept.
func TestReadPickedSelector(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.yaml")
	plain := filepath.Join(dir, "plain.yaml")
	os.WriteFile(path, []byte(rendered), 0o644)
	os.WriteFile(plain, []byte("groups:\n  - name: g\n    rules:\n      - alert: Plain\n        expr: up == 0\n"), 0o644)
	for _, c := range []struct {
		sel        string
		want       []string
		unselected int
	}{
		{"grafana_alert=1", []string{"Checkout errors", "Plain", "ShopSlow"}, 1},
		{"grafana_alert==1,team=shop", []string{"Checkout errors", "Plain"}, 2},
		{"team in (shop, pay)", []string{"Checkout errors", "Plain", "ShopDown"}, 1},
		{"grafana_alert notin (1)", []string{"Plain", "ShopDown"}, 2},
		{"!grafana_alert", []string{"Plain", "ShopDown"}, 2},
		{"team,grafana_alert!=2", []string{"Checkout errors", "Plain", "ShopDown"}, 1},
	} {
		sel, err := ParseSelector(c.sel)
		if err != nil {
			t.Fatalf("%s: %v", c.sel, err)
		}
		for _, trim := range []bool{false, true} {
			rs, docs, picked, err := ReadPicked(dir, Picking{Select: sel, Trim: trim})
			if err != nil {
				t.Fatalf("%s: %v", c.sel, err)
			}
			if got := names(rs); !slices.Equal(got, c.want) {
				t.Errorf("%s, trim %v: %v, want %v", c.sel, trim, got, c.want)
			}
			if picked.Unselected != c.unselected || picked.Other != 4 {
				t.Errorf("%s: picked %+v", c.sel, picked)
			}
			again, err := FromDocs(docs)
			if err != nil || !slices.Equal(names(again), c.want) {
				t.Errorf("%s, trim %v: read again %v %v", c.sel, trim, names(again), err)
			}
		}
	}
	// Nothing the selector matches: said, with how many it left out.
	sel, _ := ParseSelector("team=pay")
	_, _, _, err := ReadPicked(path, Picking{Select: sel})
	if !errors.Is(err, ErrNoneSelected) || !strings.Contains(err.Error(), "3 documents of rules have other labels") {
		t.Errorf("none matched: %v", err)
	}
	if _, _, _, err := ReadPicked(dir, Picking{Select: sel}); err != nil {
		t.Errorf("a directory with a rule file that isn't an object: %v", err)
	}
	os.Remove(plain)
	if _, _, _, err := ReadPicked(dir, Picking{Select: sel}); !errors.Is(err, ErrNoneSelected) {
		t.Errorf("a directory none of whose rules match: %v", err)
	}
}

// kubectl get -o yaml writes a List: its items are picked one by one, and
// a List some of whose items were left out is written afresh with the
// others.
func TestPickList(t *testing.T) {
	list := `apiVersion: v1
kind: List
items:
  - apiVersion: monitoring.coreos.com/v1
    kind: PrometheusRule
    metadata: {name: a, labels: {grafana_alert: "1"}}
    spec: {groups: [{name: a, rules: [{alert: A, expr: up == 0}]}]}
  - apiVersion: monitoring.coreos.com/v1
    kind: PrometheusRule
    metadata: {name: b}
    spec: {groups: [{name: b, rules: [{alert: B, expr: up == 0}]}]}
  - apiVersion: v1
    kind: Service
    metadata: {name: s, labels: {grafana_alert: "1"}}
`
	sel, _ := ParseSelector("grafana_alert=1")
	kept, picked, err := Pick([]byte(list), sel)
	if err != nil || picked != (Picked{Other: 1, Unselected: 1}) {
		t.Fatalf("%+v %v", picked, err)
	}
	rs, err := Parse(kept)
	if err != nil || !slices.Equal(names(rs), []string{"A"}) {
		t.Errorf("%v %v\n%s", names(rs), err, kept)
	}
	// All kept: as written.
	kept, picked, err = Pick([]byte(list), Selector{})
	if err != nil || picked != (Picked{Other: 1}) {
		t.Fatalf("%+v %v", picked, err)
	}
	if rs, _ := Parse(kept); !slices.Equal(names(rs), []string{"A", "B"}) {
		t.Errorf("%v\n%s", names(rs), kept)
	}
}

// A stream is cut where YAML cuts it: a line starting with --- or ...,
// even with more on it; one that YAML would read otherwise, such as with a
// directive before it, is decoded whole and what is kept written afresh.
func TestPickCuts(t *testing.T) {
	for _, c := range []struct{ name, in, raw string }{
		{"a marker with more on its line", "--- # Source: a\nkind: Service\nmetadata: {name: s}\n--- {groups: [{name: g, rules: [{alert: A, expr: up}]}]}\n", "--- {groups: [{name: g,"},
		{"an end of a document", "kind: Service\nmetadata: {name: s}\n...\ngroups: [{name: g, rules: [{alert: A, expr: up}]}]\n", "groups: [{name: g,"},
		{"a directive", "%YAML 1.1\n---\nkind: Service\nmetadata: {name: s}\n---\ngroups: [{name: g, rules: [{alert: A, expr: up}]}]\n", "groups:\n"},
		{"a byte order mark", "\xef\xbb\xbfkind: Service\nmetadata: {name: s}\n---\ngroups: [{name: g, rules: [{alert: A, expr: up}]}]\n", "groups: [{name: g,"},
		{"no newline at the end", "kind: Service\nmetadata: {name: s}\n---\ngroups: [{name: g, rules: [{alert: A, expr: up}]}]", "groups: [{name: g,"},
	} {
		kept, picked, err := Pick([]byte(c.in), Selector{})
		if err != nil || picked.Other != 1 {
			t.Errorf("%s: %+v %v", c.name, picked, err)
			continue
		}
		if rs, err := Parse(kept); err != nil || !slices.Equal(names(rs), []string{"A"}) || bytes.Contains(kept, []byte("Service")) {
			t.Errorf("%s: %v %v\n%s", c.name, names(rs), err, kept)
		}
		// As written, but for the directive's, written afresh.
		if !bytes.Contains(kept, []byte(c.raw)) {
			t.Errorf("%s: kept %q, want %q in it", c.name, kept, c.raw)
		}
	}
	if _, _, err := Pick([]byte("a: [\n"), Selector{}); err == nil {
		t.Error("YAML that doesn't parse was taken")
	}
}

// A selector is read as kubectl -l reads one; what isn't one is refused.
func TestParseSelector(t *testing.T) {
	for _, s := range []string{"", "a=b", "a==b", "a!=b", "a", "!a", "a in (b,c)", "a notin (b, c)", "a=b, c in (d), !e",
		"app.kubernetes.io/part-of=shop", "a=", "x_y.z-1=v_1.2-3"} {
		if _, err := ParseSelector(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	for _, s := range []string{"a=b=c", "=b", "a in (b", "a in b)", "a in ((b))", "a,,b", "-a=b", "a=-b", "a b", "Example.com/a=b",
		"a=" + strings.Repeat("v", 64), strings.Repeat("a=b,", 1100) + "a=b"} {
		if _, err := ParseSelector(s); err == nil {
			t.Errorf("%q was taken", s)
		}
	}
	sel, _ := ParseSelector("a=1,b!=2,c in (x,y),d notin (z),e,!f")
	for _, c := range []struct {
		labels map[string]string
		want   bool
	}{
		{map[string]string{"a": "1", "c": "x", "e": ""}, true},
		{map[string]string{"a": "1", "b": "3", "c": "y", "d": "w", "e": "1"}, true},
		{map[string]string{"a": "2", "c": "x", "e": ""}, false},
		{map[string]string{"a": "1", "b": "2", "c": "x", "e": ""}, false},
		{map[string]string{"a": "1", "e": ""}, false},
		{map[string]string{"a": "1", "c": "x", "d": "z", "e": ""}, false},
		{map[string]string{"a": "1", "c": "x"}, false},
		{map[string]string{"a": "1", "c": "x", "e": "", "f": ""}, false},
	} {
		if got := sel.Matches(c.labels); got != c.want {
			t.Errorf("%v: %v, want %v", c.labels, got, c.want)
		}
	}
	if !(Selector{}).Matches(nil) || !(Selector{}).Empty() || sel.String() != "a=1,b!=2,c in (x,y),d notin (z),e,!f" {
		t.Error("the empty selector, or String")
	}
}

const labelledCM = `apiVersion: v1
kind: ConfigMap
metadata:
  name: a
  labels: {grafana_alert: "1"}
data:
  a.yaml: |
    groups: [{name: g, rules: [{alert: A, expr: up == 0, for: 1m}]}]
`

// A kind's own list, as the API writes it (ConfigMapList,
// PrometheusRuleList), is read as kubectl's List is; so are streams with
// CRLF line ends or a byte order mark, and a List in JSON some of whose
// items are left out.
func TestPickListKinds(t *testing.T) {
	indent := func(s string) string {
		return "- " + strings.ReplaceAll(strings.TrimSuffix(s, "\n"), "\n", "\n  ") + "\n"
	}
	for name, c := range map[string]struct {
		in, sel string
		rules   []string
	}{
		"ConfigMapList":                    {"apiVersion: v1\nkind: ConfigMapList\nmetadata: {resourceVersion: \"1\"}\nitems:\n" + indent(labelledCM), "", []string{"A"}},
		"PrometheusRuleList":               {"apiVersion: monitoring.coreos.com/v1\nkind: PrometheusRuleList\nitems:\n- apiVersion: monitoring.coreos.com/v1\n  kind: PrometheusRule\n  metadata: {name: p}\n  spec: {groups: [{name: g, rules: [{alert: P, expr: up == 0}]}]}\n", "", []string{"P"}},
		"CRLF":                             {strings.ReplaceAll("---\n# Source: x\n"+labelledCM+"---\napiVersion: v1\nkind: Service\nmetadata: {name: s}\n", "\n", "\r\n"), "grafana_alert=1", []string{"A"}},
		"a byte order mark":                {"\xef\xbb\xbf" + labelledCM + "---\napiVersion: v1\nkind: Service\nmetadata: {name: s}\n", "grafana_alert=1", []string{"A"}},
		"a List in JSON, an item left out": {`{"apiVersion":"v1","kind":"List","items":[{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"a","labels":{"grafana_alert":"1"}},"data":{"a.yaml":"groups: [{name: g, rules: [{alert: A, expr: up == 0}]}]"}},{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"b"},"data":{"b.yaml":"groups: [{name: h, rules: [{alert: B, expr: up == 0}]}]"}}]}`, "grafana_alert=1", []string{"A"}},
	} {
		sel, _ := ParseSelector(c.sel)
		kept, _, err := Pick([]byte(c.in), sel)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		rs, err := Parse(kept)
		if err != nil || !slices.Equal(names(rs), c.rules) {
			t.Errorf("%s: %v %v\n%s", name, names(rs), err, kept)
		}
	}
}

// A Kubernetes object of a kind rules aren't kept in holds none, whatever
// else it has: a Secret with a top-level groups (the API server drops the
// field and keeps the Secret) isn't kept, selected or not, nor read as
// rules from a file given whole.
func TestPickOtherKinds(t *testing.T) {
	in := "apiVersion: v1\nkind: Secret\nmetadata: {name: s, labels: {grafana_alert: \"1\"}}\nstringData: {password: hunter2}\ngroups: [{name: g, rules: [{alert: S, expr: up == 0}]}]\n---\n" + labelledCM
	for _, s := range []string{"", "grafana_alert=1"} {
		sel, _ := ParseSelector(s)
		kept, picked, err := Pick([]byte(in), sel)
		if err != nil || bytes.Contains(kept, []byte("hunter2")) || picked.Other != 1 {
			t.Errorf("selector %q: %+v %v\n%s", s, picked, err, kept)
		}
	}
	if rs, err := Parse([]byte(in)); err != nil || !slices.Equal(names(rs), []string{"A"}) {
		t.Errorf("read whole: %v %v", names(rs), err)
	}
}

// A ConfigMap whose rule file doesn't read is said apart from what holds
// no rules: dropped when it holds nothing else that reads, kept when it
// does.
func TestPickBroken(t *testing.T) {
	broken := strings.Replace(labelledCM, "for: 1m", "for: 1x", 1)
	_, picked, err := Pick([]byte(broken+"---\napiVersion: v1\nkind: Service\nmetadata: {name: s}\n"), Selector{})
	if err != nil || picked != (Picked{Other: 1, Broken: 1}) ||
		picked.String() != "1 document that holds no rules, and 1 ConfigMap with a rule file that doesn't read" {
		t.Errorf("%+v %q %v", picked, picked, err)
	}
	both := strings.Replace(labelledCM, "\ndata:\n", "\ndata:\n  b.yaml: |\n    groups: [{name: h, rules: [{alert: B, expr: up == 0, for: 2x}]}]\n", 1)
	kept, picked, err := Pick([]byte(both), Selector{})
	if rs, _ := Parse(kept); err != nil || picked != (Picked{Broken: 1}) || !slices.Equal(names(rs), []string{"A"}) {
		t.Errorf("one of two broken: %+v %v %v", picked, names(rs), err)
	}
	// What isn't YAML, or isn't rules, isn't broken rules.
	other := strings.Replace(labelledCM, "\ndata:\n", "\ndata:\n  nginx.conf: |\n    server { listen: [80; }\n  dash.json: '{\"panels\": []}'\n", 1)
	if _, picked, err := Pick([]byte(other), Selector{}); err != nil || picked.Broken != 0 {
		t.Errorf("other keys: %+v %v", picked, err)
	}
}

// A file of rules is read MaxFile at most: more is refused, saying so.
func TestReadPickedTooBig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.yaml")
	body := []byte(labelledCM + "# " + strings.Repeat("x", MaxFile-len(labelledCM)-3) + "\n")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ReadPicked(path, Picking{}); err != nil {
		t.Errorf("MaxFile: %v", err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ReadPicked(path, Picking{}); err == nil || !strings.Contains(err.Error(), "more than 16 MiB") {
		t.Errorf("a byte more: %v", err)
	}
}

// Selectors kubectl takes, read alike: > and < on whole numbers; and an
// empty value in a set refused, though kubectl takes some.
func TestParseSelectorParity(t *testing.T) {
	for _, s := range []string{"a in (x,)", "a in (x,,)", "a in ()", "a in (,x)", "a in (x,,y)", "a notin (x,)", "a>x", "a>1.5", "a>-1", "a>", "a<", ">1"} {
		if _, err := ParseSelector(s); err == nil {
			t.Errorf("%q was taken", s)
		}
	}
	for _, c := range []struct {
		sel    string
		labels map[string]string
		want   bool
	}{
		{"a>1", map[string]string{"a": "2"}, true},
		{"a>1", map[string]string{"a": "10"}, true},
		{"a>1", map[string]string{"a": "1"}, false},
		{"a>1", map[string]string{"a": "x"}, false},
		{"a>1", map[string]string{}, false},
		{"a > 1", map[string]string{"a": "2"}, true},
		{"a<1", map[string]string{"a": "0"}, true},
		{"a<1", map[string]string{"a": "1"}, false},
		{"a<01", map[string]string{"a": "0"}, true},
		{"a<1", map[string]string{}, false},
		{"a<1,b", map[string]string{"a": "0"}, false},
	} {
		sel, err := ParseSelector(c.sel)
		if err != nil {
			t.Errorf("%q: %v", c.sel, err)
			continue
		}
		if got := sel.Matches(c.labels); got != c.want {
			t.Errorf("%q on %v: %v, want %v", c.sel, c.labels, got, c.want)
		}
	}
}
