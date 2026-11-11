package rules

import (
	"os"
	"path/filepath"
	"testing"
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
  "CompositeAlarms": [{"AlarmName": "c"}]}`} {
		f.Add([]byte(seed))
	}
	// An Azure metric alert in PromQL, as the 2026-01-01 API writes one.
	if b, err := os.ReadFile(filepath.Join("testdata", "azure-metric-alert-promql.json")); err == nil {
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
		}
	})
}

func FuzzFromAPI(f *testing.F) {
	f.Add([]byte(`{"status":"success","data":{"groups":[{"name":"g","interval":60,"rules":[{"type":"alerting","name":"A","query":"up == 0","duration":300,"labels":{"severity":"page"}}]}]}}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		FromAPI(b)
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
