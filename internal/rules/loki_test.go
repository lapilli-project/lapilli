package rules

import (
	"strings"
	"testing"
	"time"
)

// TestLokiStep: how far apart Grafana 13.3.1's Loki data source asked a
// range query's points, measured through its /api/v1/eval.
func TestLokiStep(t *testing.T) {
	for _, c := range []struct {
		model map[string]any
		span  time.Duration
		want  time.Duration
	}{
		{map[string]any{"intervalMs": 1000.0, "maxDataPoints": 43200.0}, 10 * time.Minute, time.Second},
		{map[string]any{"intervalMs": 1000.0, "maxDataPoints": 43200.0}, 6 * time.Hour, 1964 * time.Millisecond},
		{map[string]any{"intervalMs": 1000.0, "maxDataPoints": 100.0}, 10 * time.Minute, time.Second},
		{map[string]any{"intervalMs": 5000.0}, 10 * time.Minute, 5 * time.Second},
		{map[string]any{"intervalMs": 1000.0, "resolution": 2.0}, 10 * time.Minute, 2 * time.Second},
		{map[string]any{"step": "15s"}, 5 * time.Minute, 15 * time.Second},
		{map[string]any{"step": "7"}, 5 * time.Minute, 7 * time.Second},
		{map[string]any{}, time.Minute, time.Second},
	} {
		if got := lokiStep(c.model, c.span); got != c.want {
			t.Errorf("%v over %s: %s, want %s", c.model, c.span, got, c.want)
		}
	}
}

// TestLokiMacros: as Grafana 13.3.1 put them in over five minutes at 1s,
// in what Loki logged it was asked.
func TestLokiMacros(t *testing.T) {
	for _, c := range []struct {
		expr          string
		instant       bool
		range_, inst_ string
	}{
		{`sum(count_over_time({app="shop"}[$__auto]))`, false, `sum(count_over_time({app="shop"}[1s]))`, `sum(count_over_time({app="shop"}[300s]))`},
		{`sum(count_over_time({app="shop"}[$__interval])) + $__interval_ms`, false,
			`sum(count_over_time({app="shop"}[1s])) + 1000`, `sum(count_over_time({app="shop"}[1s])) + 1000`},
		{`sum(count_over_time({app="shop"}[${__range}])) + $__range_s + $__range_ms`, false,
			`sum(count_over_time({app="shop"}[300s])) + 300 + 300000`, `sum(count_over_time({app="shop"}[300s])) + 300 + 300000`},
	} {
		if got := lokiMacros(c.expr, false, time.Second, 5*time.Minute); got != c.range_ {
			t.Errorf("range: %s, want %s", got, c.range_)
		}
		if got := lokiMacros(c.expr, true, time.Second, 5*time.Minute); got != c.inst_ {
			t.Errorf("instant: %s, want %s", got, c.inst_)
		}
	}
}

const lokiGrafana = `apiVersion: 1
groups:
  - orgId: 1
    name: logs
    folder: Logs
    interval: 10s
    rules:
      - uid: inst
        title: Instant
        condition: B
        data:
          - refId: A
            queryType: instant
            relativeTimeRange: {from: 600, to: 0}
            datasourceUid: lk
            model: {datasource: {type: loki, uid: lk}, expr: 'sum(count_over_time({app="shop"} |= "error" [5m]))', intervalMs: 1000, queryType: instant}
          - refId: B
            datasourceUid: __expr__
            model: {type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [10]}}]}
      - uid: range
        title: Range
        condition: C
        data:
          - refId: A
            relativeTimeRange: {from: 120, to: 30}
            datasourceUid: lk
            model: {expr: 'sum by (app) (rate({app=~".+"}[$__auto]))', intervalMs: 1000, queryType: range}
          - refId: B
            datasourceUid: __expr__
            model: {type: reduce, expression: A, reducer: max}
          - refId: C
            datasourceUid: __expr__
            model: {type: threshold, expression: B, conditions: [{evaluator: {type: gt, params: [1]}}]}
`

// TestGrafanaOnLoki: a Grafana rule whose condition reads a Loki data
// source is LogQL, asked of that Loki: known from the model, as Grafana's
// editor writes it, or from the type Grafana says the data source is.
// Instant and range queries, with Loki's macros; a reduce over a range
// worked out by the replay.
func TestGrafanaOnLoki(t *testing.T) {
	rs, err := Parse([]byte(lokiGrafana))
	if err != nil {
		t.Fatal(err)
	}
	inst, rng := rs[0], rs[1]
	if inst.Unsupported != "" || inst.Logs != "lk" || inst.Graph.Queries["A"] != `sum(count_over_time({app="shop"} |= "error" [5m]))` {
		t.Errorf("instant, from the model: %q %q %v", inst.Unsupported, inst.Logs, inst.Graph.Queries)
	}
	// The model doesn't say: PromQL, until Grafana says otherwise.
	if rng.Logs != "" || rng.Unsupported != "" {
		t.Fatalf("range, unknown: %q %q", rng.Logs, rng.Unsupported)
	}
	Replaying{Kinds: map[string]string{"lk": KindLoki}}.Apply(rs)
	rng = rs[1]
	a := rng.Graph.Asks["B"]
	if rng.Logs != "lk" || !a.Logs || a.Fn != "max" || a.Window != 90*time.Second || a.Res != time.Second ||
		a.Expr != `sum by (app) (rate({app=~".+"}[1s]))` || rng.Graph.Shift["B"] != 30*time.Second || rng.Graph.Data["A"].Present != "B" {
		t.Errorf("range, read again as Loki: %q %+v %v %v", rng.Logs, a, rng.Graph.Shift, rng.Graph.Data)
	}
	if expr, ok := rng.Graph.Expr("B", 0); !ok || expr != a.Expr {
		t.Errorf("the ask's expression: %q", expr)
	}
	// Grafana's data source proxy or --loki, neither: listed.
	rs, _ = Parse([]byte(lokiGrafana))
	Replaying{NoGrafanaLoki: true}.Apply(rs)
	if !strings.Contains(rs[0].Unsupported, "reads Loki") {
		t.Errorf("no way to Loki: %q", rs[0].Unsupported)
	}
}

// TestGrafanaNotReplayed: what a Grafana rule on Loki, or on another data
// source, that isn't replayed says.
func TestGrafanaNotReplayed(t *testing.T) {
	swap := func(old, new string) string { return strings.Replace(lokiGrafana, old, new, 1) }
	for name, c := range map[string]struct {
		file  string
		kinds map[string]string
		want  string
	}{
		"a log query": {swap(`'sum(count_over_time({app="shop"} |= "error" [5m]))'`, `'{app="shop"} |= "error"'`), nil,
			"query A is a log query"},
		"SQL": {lokiGrafana, map[string]string{"lk": "mysql"}, "its condition reads a mysql data source: only PromQL and LogQL are replayed"},
		"classic conditions": {swap(`model: {type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [10]}}]}`,
			`model: {type: classic_conditions, conditions: [{evaluator: {type: gt, params: [10]}, operator: {type: and}, query: {params: [A]}, reducer: {type: avg}}]}`), nil,
			"classic conditions over a Loki query"},
		"Loki and Prometheus": {swap(`          - refId: B
            datasourceUid: __expr__
            model: {type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [10]}}]}`, `          - refId: P
            datasourceUid: prom
            model: {expr: up}
          - refId: B
            datasourceUid: __expr__
            model: {type: math, expression: '$A > 10 && $P > 0'}`), nil, "Prometheus and Loki together"},
	} {
		rs, err := Parse([]byte(c.file))
		if err != nil {
			t.Fatal(name, err)
		}
		Replaying{Kinds: c.kinds}.Apply(rs)
		if !strings.Contains(rs[0].Unsupported, c.want) {
			t.Errorf("%s: %q, want %q", name, rs[0].Unsupported, c.want)
		}
	}
	// A SQL query, which has no expr, read again once Grafana says what
	// its data source is.
	sql := strings.Replace(lokiGrafana, `model: {datasource: {type: loki, uid: lk}, expr: 'sum(count_over_time({app="shop"} |= "error" [5m]))', intervalMs: 1000, queryType: instant}`,
		`model: {rawSql: 'SELECT count(*) FROM failed_orders'}`, 1)
	rs, err := Parse([]byte(sql))
	if err != nil || !strings.Contains(rs[0].Unsupported, "no PromQL expr") || strings.Join(rs[0].GrafanaUIDs(), ",") != "lk" {
		t.Fatalf("%v %q %v", err, rs[0].Unsupported, rs[0].GrafanaUIDs())
	}
	Replaying{Kinds: map[string]string{"lk": "mysql"}}.Apply(rs)
	if !strings.Contains(rs[0].Unsupported, "reads a mysql data source") {
		t.Errorf("SQL, read again: %q", rs[0].Unsupported)
	}
	// A type is a plugin's id: anything else said of one isn't shown.
	rs, _ = Parse([]byte(strings.Replace(lokiGrafana, "type: loki, uid: lk", "type: '<b>x</b>', uid: lk", 1)))
	if rs[0].Logs != "" || strings.Contains(rs[0].Unsupported, "<b>") {
		t.Errorf("a type that isn't a plugin's id: %q %q", rs[0].Logs, rs[0].Unsupported)
	}
}

// TestLokiRulerRules: a Loki ruler's rule is LogQL, kept apart from the
// same text in Prometheus; read as Loki 3.7.9 reads it, without the
// keep_firing_for and query_offset it drops; placed where the ruler's API
// says its group runs.
func TestLokiRulerRules(t *testing.T) {
	file := []byte(`groups:
  - name: shop
    interval: 1m
    query_offset: 30s
    rules:
      - alert: Errors
        expr: sum(count_over_time({app="shop"}[5m])) > 10
        for: 2m
        keep_firing_for: 5m
`)
	prom, err := FromDocs([]Doc{{Source: "a.yaml", Data: file}})
	if err != nil {
		t.Fatal(err)
	}
	loki, err := FromDocs([]Doc{{Source: "a.yaml", Data: file, Logs: LokiRuler, Namespace: "a.yaml"}})
	if err != nil {
		t.Fatal(err)
	}
	if loki[0].Logs != LokiRuler || loki[0].KeepFiringFor != 0 || loki[0].QueryOffset != 0 || loki[0].For != 2*time.Minute {
		t.Errorf("as Loki reads it: %+v", loki[0])
	}
	if prom[0].Logs != "" || prom[0].KeepFiringFor != 5*time.Minute || prom[0].Key() == loki[0].Key() {
		t.Errorf("as Prometheus reads it: %+v; keys %q %q", prom[0], prom[0].Key(), loki[0].Key())
	}
	// The same text, without what Loki drops: LogQL still isn't PromQL.
	plain := []byte("groups:\n  - name: shop\n    rules:\n      - alert: Errors\n        expr: sum(count_over_time({app=\"shop\"}[5m])) > 10\n")
	a, _ := FromDocs([]Doc{{Source: "a.yaml", Data: plain}})
	b, _ := FromDocs([]Doc{{Source: "a.yaml", Data: plain, Logs: LokiRuler}})
	if a[0].Key() == b[0].Key() {
		t.Errorf("a Loki ruler's rule and Prometheus's of the same text share a key: %q", a[0].Key())
	}
	Replaying{NoLoki: true}.Apply(loki)
	if !strings.Contains(loki[0].Unsupported, "give --loki") {
		t.Errorf("no --loki: %q", loki[0].Unsupported)
	}
	loki[0].Unsupported = ""
	Replaying{Places: map[Place]time.Duration{{"a.yaml", "shop"}: 11 * time.Second, {"b.yaml", "shop"}: 3 * time.Second}}.Apply(loki)
	if loki[0].Phase != 11*time.Second {
		t.Errorf("placed at %s, want 11s", loki[0].Phase)
	}
}

// TestPlaces: where a Loki ruler runs each group, as its
// /prometheus/api/v1/rules says, by the namespace it gives as the file.
func TestPlaces(t *testing.T) {
	got, err := Places([]byte(`{"status":"success","data":{"groups":[
		{"name":"shop","file":"shop.yaml","interval":10,"lastEvaluation":"2026-11-11T19:13:54.56508092Z","rules":[]},
		{"name":"api","file":"shop.yaml","interval":60,"lastEvaluation":"2026-11-11T19:13:11.640845929Z","rules":[]},
		{"name":"new","file":"shop.yaml","interval":60,"lastEvaluation":"0001-01-01T00:00:00Z","rules":[]}]}}`))
	if err != nil || len(got) != 2 || got[Place{"shop.yaml", "shop"}] != 4*time.Second || got[Place{"shop.yaml", "api"}] != 11*time.Second {
		t.Errorf("%v %v", got, err)
	}
	if _, err := Places([]byte(`{"status":"error"}`)); err == nil {
		t.Error("an error answer should be an error")
	}
}
