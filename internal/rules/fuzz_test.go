package rules

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The rule parsers read files people keep, APIs, and over MCP what an agent
// sends: whatever they are given, they answer or refuse, and never panic
// nor recurse without end.

func FuzzParse(f *testing.F) {
	for _, seed := range []string{grafanaFile, `groups:
  - name: g
    interval: 1m
    rules:
      - alert: A
        expr: sum by (service) (rate(errors[5m])) > 1
        for: 5m
        keep_firing_for: 10m
        labels: {severity: page}
`, `apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata: {name: r, namespace: shop}
spec:
  groups:
    - name: g
      rules:
        - alert: B
          expr: up == 0
`, `apiVersion: monitoring.googleapis.com/v1
kind: Rules
metadata: {name: r, namespace: shop}
spec:
  groups:
    - name: g
      rules:
        - alert: C
          expr: errors{job="a"} > 1
`, googlePolicyJSON, `{"MetricAlarms": [{"AlarmName": "a", "EvaluationInterval": 60,
  "EvaluationCriteria": {"PromQLCriteria": {"Query": "up == 0", "PendingPeriod": 300, "RecoveryPeriod": 60}}}],
  "CompositeAlarms": [{"AlarmName": "c"}]}`, `apiVersion: 1
groups:
  - {orgId: 1, name: g, folder: f, interval: 10s, rules: [{uid: c, title: C, condition: C, noDataState: KeepLast,
      execErrState: OK, missing_series_evals_to_resolve: 3, data: [
        {refId: A, datasourceUid: p, relativeTimeRange: {from: 300, to: 60}, model: {expr: x, intervalMs: 500, maxDataPoints: 10}},
        {refId: B, datasourceUid: p, model: {expr: y, instant: true}},
        {refId: M, datasourceUid: __expr__, model: {type: math, expression: '$B > 1 && ($B < 9 || $B >= 3)'}},
        {refId: C, datasourceUid: __expr__, model: {type: classic_conditions, conditions: [
          {evaluator: {type: outside_range_included, params: [1, 2]}, operator: {type: and}, query: {params: [A]}, reducer: {type: percent_diff}},
          {evaluator: {type: lte, params: [7]}, operator: {type: logic-or}, query: {params: [B]}, reducer: {type: count_non_null}}]}}]}]}
`} {
		f.Add([]byte(seed))
	}
	// An Azure metric alert in PromQL, as the 2026-01-01 API writes one.
	if b, err := os.ReadFile(filepath.Join("testdata", "azure-metric-alert-promql.json")); err == nil {
		f.Add(b)
	}
	// Grafana's own export: $$ and its macros.
	if b, err := os.ReadFile(filepath.Join("testdata", "grafana-13.3.1-export.yaml")); err == nil {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		rs, err := Parse(b)
		if err != nil {
			return
		}
		for _, r := range rs {
			_ = r.Key()
			if r.For < 0 || r.KeepFiringFor < 0 || r.Interval < 0 {
				t.Fatalf("a negative duration: %+v", r)
			}
			if r.Graph != nil {
				for _, st := range r.Graph.Steps {
					_ = st.Uses()
				}
			}
			// Near misses read each number, the values it compares, and the
			// rule with another in its place.
			for _, bd := range r.Bounds() {
				r.ValueOf(bd)
				w := r.WithBound(bd, bd.Value+1)
				if len(w.Bounds()) != len(r.Bounds()) {
					t.Fatalf("a number changed changes the numbers: %+v", bd)
				}
			}
			_ = r.Unseen()
		}
	})
}

func FuzzFromAPI(f *testing.F) {
	f.Add([]byte(`{"status":"success","data":{"groups":[{"name":"g","interval":60,"lastEvaluation":"2026-11-11T12:40:30.1Z","rules":[{"type":"alerting","name":"A","query":"up == 0","duration":300,"labels":{"severity":"page"}}]}]}}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		rs, _ := FromAPI(b)
		for _, r := range rs {
			if r.Phase < 0 || r.Phase >= max(r.Interval, time.Second) {
				t.Fatalf("placed outside its interval: %+v", r)
			}
		}
	})
}

func FuzzScope(f *testing.F) {
	for _, e := range []string{`errors > 5`, `sum by (service) (rate(http{code=~"5.."}[5m])) / ignoring(code) group_left sum(x)`,
		`label_replace(up, "a", "$1", "b", "(.*)") # comment`, "`raw` + \"q\\\"\" + x"} {
		f.Add(e, "namespace", "shop")
	}
	f.Fuzz(func(t *testing.T, expr, k, v string) {
		Scope(expr, map[string]string{k: v})
	})
}

// A rendered chart's stream comes from a repository: picking its rules out
// of it answers or refuses, and what it keeps holds the same rules.
func FuzzPick(f *testing.F) {
	f.Add([]byte(rendered), "grafana_alert=1")
	f.Add([]byte("%YAML 1.1\n---\nkind: Service\nmetadata: {name: s}\n...\ngroups: [{name: g, rules: [{alert: A, expr: up}]}]\n"), "")
	f.Add([]byte("kind: List\nitems: [{kind: Service, metadata: {}}, {groups: [{name: g, rules: [{alert: A, expr: up}]}]}]\n"), "a in (b)")
	f.Add([]byte("apiVersion: v1\nkind: Secret\nmetadata: {name: s, labels: {a: \"1\"}}\ngroups: [{name: g, rules: [{alert: A, expr: up}]}]\n"), "a>0")
	f.Add([]byte("kind: ConfigMapList\nitems: [{kind: ConfigMap, metadata: {name: x}, data: {a: \"groups: [{name: g, rules: [{alert: A, expr: up, for: 1x}]}]\"}}]\n"), "")
	f.Fuzz(func(t *testing.T, b []byte, s string) {
		sel, err := ParseSelector(s)
		if err != nil {
			return
		}
		kept, picked, err := Pick(b, sel)
		if err != nil || picked.Other < 0 || picked.Unselected < 0 || picked.Broken < 0 {
			return
		}
		// What is kept is documents of rules alone, those selected: no
		// other object (a Secret) is left in it.
		docs, err := decodeAll(kept)
		if err != nil {
			t.Fatalf("kept doesn't read: %v\n%q", err, kept)
		}
		var items []any
		for _, d := range docs {
			if l, ok := d.([]any); ok {
				items = append(items, l...)
			} else if d != nil {
				items = append(items, d)
			}
		}
		for len(items) > 0 {
			m, _ := items[0].(map[string]any)
			items = items[1:]
			if m != nil && isList(m) {
				items = append(items, list(m["items"])...)
				continue
			}
			if _, ok, _ := parseDoc(m); !ok {
				t.Fatalf("kept a document that holds no rules: %v\n%q\n%q", m, b, kept)
			}
			if !sel.Empty() && isObject(m) && !sel.Matches(objectLabels(m)) {
				t.Fatalf("kept an object the selector doesn't match: %v", m)
			}
		}
		all, err := Parse(b)
		if err != nil || !sel.Empty() {
			return
		}
		got, err := Parse(kept)
		if len(all) > 0 && (err != nil || len(got) != len(all)) {
			t.Fatalf("kept %d rules of %d: %v\n%q\n%q", len(got), len(all), err, b, kept)
		}
	})
}
