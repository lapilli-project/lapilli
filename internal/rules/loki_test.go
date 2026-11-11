package rules

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
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
		if got := lokiMacros(c.expr, false, time.Second, time.Second, 5*time.Minute); got != c.range_ {
			t.Errorf("range: %s, want %s", got, c.range_)
		}
		if got := lokiMacros(c.expr, true, time.Second, time.Second, 5*time.Minute); got != c.inst_ {
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
	loki, err := FromDocs([]Doc{{Source: "a.yaml", Data: file, Logs: LokiFile, Namespace: "a.yaml"}})
	if err != nil {
		t.Fatal(err)
	}
	if loki[0].Logs != LokiRuler || loki[0].KeepFiringFor != 0 || loki[0].QueryOffset != 0 || loki[0].For != 2*time.Minute {
		t.Errorf("as Loki reads it: %+v", loki[0])
	}
	// What a Loki's API gives is as Loki holds it: were it to say a
	// keep_firing_for, it would be run.
	api, err := FromDocs([]Doc{{Source: "loki/a.yaml", Data: file, Logs: LokiRuler, Namespace: "a.yaml"}})
	if err != nil || api[0].Logs != LokiRuler || api[0].KeepFiringFor != 5*time.Minute || api[0].QueryOffset != 30*time.Second {
		t.Errorf("as Loki's API gives it: %+v %v", api[0], err)
	}
	if prom[0].Logs != "" || prom[0].KeepFiringFor != 5*time.Minute || prom[0].Key() == loki[0].Key() {
		t.Errorf("as Prometheus reads it: %+v; keys %q %q", prom[0], prom[0].Key(), loki[0].Key())
	}
	// The same text, without what Loki drops: LogQL still isn't PromQL.
	plain := []byte("groups:\n  - name: shop\n    rules:\n      - alert: Errors\n        expr: sum(count_over_time({app=\"shop\"}[5m])) > 10\n")
	a, _ := FromDocs([]Doc{{Source: "a.yaml", Data: plain}})
	b, _ := FromDocs([]Doc{{Source: "a.yaml", Data: plain, Logs: LokiFile}})
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

// FuzzLokiGrafana: a Grafana rule on Loki, read as Loki once Grafana says
// its data source is, and where a Loki ruler runs its groups: whatever
// they are given, they answer or refuse, and never panic; a reduce worked
// out by the replay asks Loki no finer than LokiLeastStep.
func FuzzLokiGrafana(f *testing.F) {
	f.Add([]byte(lokiGrafana))
	f.Add([]byte(strings.Replace(lokiGrafana, `intervalMs: 1000, queryType: range`, `step: 1ms, queryType: range`, 1)))
	f.Add([]byte(`{"status":"success","data":{"groups":[{"name":"g","file":"ns","interval":10,"lastEvaluation":"2026-11-11T19:00:04.5Z"}]}}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = Places(b)
		rs, err := Parse(b)
		if err != nil {
			return
		}
		Replaying{Kinds: map[string]string{"lk": KindLoki, "loki": KindLoki}, Places: map[Place]time.Duration{{"ns", "g"}: time.Second}}.Apply(rs)
		for _, r := range rs {
			_ = r.Key()
			if r.Graph == nil || r.Unsupported != "" {
				continue
			}
			for ref, a := range r.Graph.Asks {
				if a.Logs && a.Res < LokiLeastStep {
					t.Errorf("ask %s: a step of %s", ref, a.Res)
				}
			}
		}
	})
}

// TestLokiLeastStep: a Grafana reduce over a Loki range query asked at a
// step under a second isn't replayed, and says the step.
func TestLokiLeastStep(t *testing.T) {
	for _, step := range []string{"step: 1ms", "step: 0.001", "step: 500ms", "intervalMs: 1"} {
		file := strings.Replace(lokiGrafana, `intervalMs: 1000, queryType: range`, step+`, queryType: range`, 1)
		rs, err := Parse([]byte(file))
		if err != nil {
			t.Fatal(err)
		}
		Replaying{Kinds: map[string]string{"lk": KindLoki}}.Apply(rs)
		if !strings.Contains(rs[1].Unsupported, "query A asks Loki for a point every ") || !strings.Contains(rs[1].Unsupported, "no finer than every 1s") {
			t.Errorf("%s: %q", step, rs[1].Unsupported)
		}
	}
}

// TestLokiIntervalIsNotStep: Grafana's Loki data source (grafana/grafana pkg/tsdb/loki up to
// v12.3.0, grafana/grafana-loki-datasource pkg/loki since) puts
// $__interval and $__interval_ms in with the query's interval
// (intervalMs, 1s when not set), not its step; only $__auto is the step.
// The two differ whenever the step isn't the interval: a query's own step,
// a resolution over 1, or a window over 11,000 intervals.
//
//	expr = strings.ReplaceAll(expr, varIntervalMs, intervalMsText) // interval
//	expr = strings.ReplaceAll(expr, varInterval, intervalText)     // interval
//	expr = strings.ReplaceAll(expr, varAuto, stepText)             // range: step
func TestLokiIntervalIsNotStep(t *testing.T) {
	for _, c := range []struct {
		model map[string]any
		span  time.Duration
		want  string
	}{
		// A query's own step: Grafana asks [1s] at a 10s step.
		{map[string]any{"expr": `sum(count_over_time({app="shop"}[$__interval]))`, "queryType": "range", "intervalMs": 1000.0, "step": "10s"},
			5 * time.Minute, `sum(count_over_time({app="shop"}[1s]))`},
		// Resolution 2: step 2s, interval 1s.
		{map[string]any{"expr": `sum(count_over_time({app="shop"}[$__interval])) + $__interval_ms`, "queryType": "range", "intervalMs": 1000.0, "resolution": 2.0},
			5 * time.Minute, `sum(count_over_time({app="shop"}[1s])) + 1000`},
		// Six hours: step 1.964s, interval 1s; $__interval_ms 1000, not 1964.
		{map[string]any{"expr": `sum(count_over_time({app="shop"}[$__interval])) + $__interval_ms`, "queryType": "range", "intervalMs": 1000.0},
			6 * time.Hour, `sum(count_over_time({app="shop"}[1s])) + 1000`},
	} {
		q, err := lokiQuery("A", dataQuery{expr: c.model["expr"].(string)}, map[string]any{}, c.model, c.span)
		if err != nil {
			t.Fatal(err)
		}
		if q.expr != c.want {
			t.Errorf("%v over %s:\n got %s\nwant %s (as Grafana's Loki data source puts it in)", c.model, c.span, q.expr, c.want)
		}
	}
}

// TestLokiOwnStepTimesResolution: with a step of its own, Grafana
// multiplies it by the resolution, and doesn't raise it to the window over
// 11,000; a resolution that isn't 1 to 5 or 10 is 1:
//
//	return time.Duration(step.Nanoseconds() * resolution), nil
func TestLokiOwnStepTimesResolution(t *testing.T) {
	for _, c := range []struct {
		model map[string]any
		span  time.Duration
		want  time.Duration
	}{
		{map[string]any{"step": "15s", "resolution": 2.0}, 5 * time.Minute, 30 * time.Second},
		{map[string]any{"step": "1s"}, 24 * time.Hour, time.Second},
		{map[string]any{"intervalMs": 1000.0, "resolution": 10.0}, 5 * time.Minute, 10 * time.Second},
		{map[string]any{"intervalMs": 1000.0, "resolution": 7.0}, 5 * time.Minute, time.Second},
		{map[string]any{"intervalMs": 1000.0, "resolution": 2.5}, 5 * time.Minute, time.Second},
	} {
		if got := lokiStep(c.model, c.span); got != c.want {
			t.Errorf("%v over %s: %s, want %s", c.model, c.span, got, c.want)
		}
	}
}

// TestLokiInstantNeedsQueryType: Grafana's Loki data source reads only
// the model's queryType;
// one without it is a range query ("older queries stored in alerting that
// did not have queryType, those were range-queries"), whatever its
// instant says.
func TestLokiInstantNeedsQueryType(t *testing.T) {
	model := map[string]any{"expr": `sum(count_over_time({app="shop"}[1m]))`, "instant": true, "intervalMs": 1000.0}
	q, err := lokiQuery("A", dataQuery{expr: model["expr"].(string)}, map[string]any{}, model, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if q.window != 5*time.Minute {
		t.Errorf("no queryType, instant: true: window %s, want a range query over 5m, as Grafana asks it", q.window)
	}
}

// TestVictoriaMetricsOldPlugin: a Grafana rule on VictoriaMetrics' own data
// source plugin was
// replayed in v0.27.0, as PromQL against --prometheus; now its type has to
// be on promKinds, which has the plugin's id since v0.12.0
// (victoriametrics-metrics-datasource) but not its id before that,
// victoriametrics-datasource, which installs still run: such a rule is
// now listed, not replayed.
func TestVictoriaMetricsOldPlugin(t *testing.T) {
	doc := `apiVersion: 1
groups:
  - orgId: 1
    name: vm
    folder: VM
    interval: 1m
    rules:
      - uid: vm1
        title: VMErrors
        condition: B
        data:
          - refId: A
            relativeTimeRange: {from: 600, to: 0}
            datasourceUid: vm
            model: {datasource: {type: victoriametrics-datasource, uid: vm}, expr: 'sum(rate(errors_total[5m]))', instant: true}
          - refId: B
            datasourceUid: __expr__
            model: {type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [1]}}]}
`
	rs, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if rs[0].Unsupported != "" {
		t.Errorf("replayed in v0.27.0, now: %q", rs[0].Unsupported)
	}
	// Nor when Grafana says the type.
	Replaying{Kinds: map[string]string{"vm": "victoriametrics-datasource"}}.Apply(rs)
	if rs[0].Unsupported != "" || rs[0].Logs != "" {
		t.Errorf("read again as Grafana says: %q %q", rs[0].Unsupported, rs[0].Logs)
	}
}

// TestGrafanaLokiEval: what Grafana 13.3.1 asked a real Loki 3.7.9 for,
// through its /api/v1/eval, kept in testdata/grafana-13.3.1-loki-eval.json,
// beside how Lapilli reads the same queries. The stream counted has a line
// a second: counted over [$__interval] at a step of 10s, each point was 1,
// the interval's second, not the step's ten; over [$__auto], 10, the step;
// a step of 15s at resolution 2 gave points 30s apart; and a query with
// instant: true and no queryType was a range query, a point a second.
func TestGrafanaLokiEval(t *testing.T) {
	b, err := os.ReadFile("testdata/grafana-13.3.1-loki-eval.json")
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Cases []struct {
			Name   string
			Model  map[string]any
			FromS  float64 `json:"from_s"`
			Points int
			StepMs float64 `json:"step_ms"`
			Values []float64
		}
	}
	if err := json.Unmarshal(b, &rec); err != nil || len(rec.Cases) != 4 {
		t.Fatalf("%v %d", err, len(rec.Cases))
	}
	window := regexp.MustCompile(`\[(\d+[smh])\]`)
	for _, c := range rec.Cases {
		span := time.Duration(c.FromS) * time.Second
		q, err := lokiQuery("A", dataQuery{expr: c.Model["expr"].(string)}, map[string]any{}, c.Model, span)
		if err != nil {
			t.Fatal(err)
		}
		if q.res != time.Duration(c.StepMs)*time.Millisecond || q.window != span {
			t.Errorf("%s: asked every %s over %s; Grafana asked every %vms over %s", c.Name, q.res, q.window, c.StepMs, span)
		}
		// A line a second: what Grafana counted is the window in seconds.
		m := window.FindStringSubmatch(q.expr)
		if m == nil {
			t.Fatalf("%s: %s", c.Name, q.expr)
		}
		if d, _ := time.ParseDuration(m[1]); slices.Max(c.Values) != d.Seconds() {
			t.Errorf("%s: Lapilli asks %s; Grafana's counts were %v", c.Name, q.expr, c.Values)
		}
	}
}
