package rules

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// A rule counts events when its value is how many happened over a range,
// summed or maxed or not, and one event passes its bound: its "for" then
// outlasts what one event can hold when it is as long as the range, or
// longer. A rate held for its "for" is what "for" is for; and so is a count
// one event doesn't pass.
func TestEventWindow(t *testing.T) {
	restarts := `increase(kube_pod_container_status_restarts_total{namespace=~"a|b"}[30m]) > 0.5`
	for _, c := range []struct {
		expr     string
		for_     time.Duration
		window   time.Duration // 0: not one that counts events
		outlasts bool
	}{
		{restarts, time.Minute, 30 * time.Minute, false},
		{restarts, 30 * time.Minute, 30 * time.Minute, true},
		{restarts, time.Hour, 30 * time.Minute, true},
		{`rate(x[5m]) > 0.05`, 10 * time.Minute, 0, false},
		{`sum by (ns) (increase(x[10m])) >= 1`, 15 * time.Minute, 10 * time.Minute, true},
		{`increase(x[30m]) > 5`, time.Hour, 0, false},
		{`sum(increase(x[10m])) by (ns) > 0`, 10 * time.Minute, 10 * time.Minute, true},
		{`max without (pod) (changes(up[1h:1m])) > 0`, 2 * time.Hour, time.Hour, true},
		{`resets(x[15m] offset 5m) > 0`, 5 * time.Minute, 15 * time.Minute, false},
		{`0.5 < changes(x[20m])`, 20 * time.Minute, 20 * time.Minute, true},
		// A rate of restarts above none is a restart, its rate times a
		// number too: the kube-prometheus mixin's spelling.
		{`rate(restarts[10m]) > 0`, 15 * time.Minute, 10 * time.Minute, true},
		{`rate(kube_pod_container_status_restarts_total{job="kube-state-metrics", namespace=~".*"}[10m]) * 60 * 5 > 0`, 15 * time.Minute, 10 * time.Minute, true},
		{`60 * rate(restarts[10m]) >= 0.1`, 15 * time.Minute, 10 * time.Minute, true},
		{`rate(restarts[10m]) / 2 > 0.001`, 15 * time.Minute, 0, false}, // one restart is 1/1200 a second
		{`increase(x[30m]) * 2 > 0.5`, time.Hour, 30 * time.Minute, true},
		// An amount, a gauge's change, or what holds for a scrape: no event.
		{`increase(process_cpu_seconds_total[10m]) > 0.5`, time.Hour, 0, false},
		{`increase(node_network_transmit_bytes_total[10m]) > 0.5`, time.Hour, 0, false},
		{`increase(http_request_duration_seconds_sum[10m]) > 0.5`, time.Hour, 0, false},
		{`delta(temperature[30m]) > 0.5`, time.Hour, 0, false},
		{`idelta(x[2m]) >= 0.5`, 2 * time.Minute, 0, false},
		{`irate(restarts[5m]) > 0`, 10 * time.Minute, 0, false},
		{`increase((sum(x))[30m:1m]) > 0.5`, time.Hour, 0, false},
		{`increase(x[200y10428w73000d]) > 0.5`, time.Hour, 0, false}, // past two hundred years: no range
		{`(increase(x[30m]) > 0.5)`, time.Hour, 30 * time.Minute, true},
		{`increase(x[30m]) > 0.5 and on(pod) kube_pod_status_ready == 0`, time.Hour, 30 * time.Minute, true},
		{`increase(a[10m]) > 0 and increase(b[1h]) > 0`, 30 * time.Minute, time.Hour, false},
		{`# restarts
increase(x[30m]) # in half an hour
  > 0.5`, time.Hour, 30 * time.Minute, true},
		// Not sure, so nothing said.
		{`increase(x[30m]) > 0.5 or y > 1`, time.Hour, 0, false},
		{`increase(x[30m]) > 0.5 unless y`, time.Hour, 0, false},
		{`avg(increase(x[30m])) > 0.5`, time.Hour, 0, false},
		{`increase(x[30m]) >= 0`, time.Hour, 0, false},
		{`increase(x[30m]) > -1`, time.Hour, 0, false},
		{`increase(x[30m]) > bool 0`, time.Hour, 0, false},
		{`increase(x[$__rate_interval]) > 0`, time.Hour, 0, false},
		{`increase(x[30m]) + increase(y[30m]) > 0`, time.Hour, 0, false},
		{`increase(x[30m]) < 1`, time.Hour, 0, false},
		{`count(increase(x[30m]) > 0) > 0`, time.Hour, 0, false},
		{`increase(x[30m]`, time.Hour, 0, false},
	} {
		r := Rule{Name: "R", Expr: c.expr, For: c.for_}
		w, ok := r.EventWindow()
		if ok != (c.window > 0) || w != c.window {
			t.Errorf("%s: window %s %v, want %s", c.expr, w, ok, c.window)
		}
		if _, out := r.ForOutlasts(); out != c.outlasts {
			t.Errorf("%s for %s: outlasts %v, want %v", c.expr, c.for_, out, c.outlasts)
		}
	}
	// Of Grafana's rules, Loki's, and any not replayed, nothing is said.
	for _, r := range []Rule{{Expr: "increase(x[30m]) > 0.5", For: time.Hour, Logs: "loki"}, {Expr: "increase(x[30m]) > 0.5", For: time.Hour, Graph: &Graph{}},
		{Expr: "increase(x[30m]) > 0.5", For: time.Hour, Unsupported: Paused}} {
		if _, ok := r.EventWindow(); ok {
			t.Errorf("%+v: said", r)
		}
	}
}

// Whatever an expression, EventWindow says something or nothing, and
// doesn't panic; what it says is a range one event holds for.
func FuzzEventWindow(f *testing.F) {
	for _, seed := range []string{`increase(x{a="]"}[30m]) > 0.5`, `sum by (ns) (increase(x[10m])) >= 1`, `changes(up[1h:1m] @ start()) > 0`,
		`(((increase(x[5m]))))>0`, `max(changes(x[1d] offset -5m)) by (a) > 0 and y`, `increase(x[30m] > 0.5`, `sum(sum(sum(increase(x[1m]))))>0`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, expr string) {
		r := Rule{Expr: expr, For: time.Hour}
		if w, ok := r.EventWindow(); ok && w <= 0 {
			t.Errorf("%q: window %s", expr, w)
		}
	})
}

// What EventWindow reads is bounded: an expression longer than eventMaxExpr
// isn't read, and the longest, deepest one that is costs little, so a rule
// writer can't make the content check, which asks it of every rule, slow.
func TestEventWindowBounded(t *testing.T) {
	deep := func(levels, size int) string {
		e := "increase(x{a=\"" + strings.Repeat("y", size) + "\"}[30m])"
		for range levels {
			e = "sum(" + e + ")"
		}
		return e + " > 0.5"
	}
	long := Rule{Expr: deep(31, 16<<10), For: time.Hour}
	start := time.Now()
	if _, ok := long.EventWindow(); ok || time.Since(start) > 10*time.Millisecond {
		t.Errorf("16 KB, 31 deep: %v in %s", ok, time.Since(start))
	}
	worst := Rule{Expr: deep(7, eventMaxExpr-200), For: time.Hour}
	if len(worst.Expr) > eventMaxExpr {
		t.Fatal(len(worst.Expr))
	}
	start = time.Now()
	for range 100 {
		if _, ok := worst.EventWindow(); !ok {
			t.Fatal("7 sums deep, under 4 KB: counts events")
		}
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("100 of the deepest, longest read: %s", took)
	} else {
		t.Logf("100 of the deepest, longest read: %s", took)
	}
}

// A Grafana rule counts events when its condition compares its one PromQL
// query's value, as it is or through a reduce that leaves it so, with a
// number one event passes: a restart rule written in Grafana as a query, a
// reduce (last) and a threshold (> 0.5) has the window a Prometheus rule of
// the same PromQL has, and a for one restart can't outlast. Nothing is said
// of one whose value is changed on its way, or read any other way.
func TestGrafanaEventWindow(t *testing.T) {
	const restarts = `sum by (cluster_name, namespace_name, container_name) (increase(kubernetes_io:container_restart_count{namespace_name!~"kube-.*"}[30m]))`
	query := func(ref, expr string, instant bool) string {
		return `
          - refId: ` + ref + `
            relativeTimeRange: {from: 1800, to: 0}
            datasourceUid: prom
            model: {expr: '` + expr + `', instant: ` + strconv.FormatBool(instant) + `, intervalMs: 60000, refId: ` + ref + `}`
	}
	step := func(ref, model string) string {
		return `
          - refId: ` + ref + `
            datasourceUid: __expr__
            model: {refId: ` + ref + `, ` + model + `}`
	}
	reduce := func(ref, fn, in string) string {
		return step(ref, `type: reduce, reducer: `+fn+`, expression: `+in)
	}
	threshold := func(ref, typ, n, in string) string {
		return step(ref, `type: threshold, expression: `+in+`, conditions: [{evaluator: {type: `+typ+`, params: [`+n+`]}}]`)
	}
	rule := func(for_, data string, more ...string) Rule {
		t.Helper()
		rs, err := Parse([]byte(`apiVersion: 1
groups:
  - orgId: 1
    name: pods
    folder: Ops
    interval: 1m
    rules:
      - uid: restarts
        title: Container restarted
        condition: C
        for: ` + for_ + `
        keepFiringFor: 10m` + strings.Join(more, "") + `
        data:` + data + "\n"))
		if err != nil || len(rs) != 1 || rs[0].Graph == nil || rs[0].Unsupported != "" {
			t.Fatalf("%v %+v", err, rs)
		}
		return rs[0]
	}
	shape := query("A", restarts, true) + reduce("B", "last", "A") + threshold("C", "gt", "0.5", "B")
	for _, c := range []struct {
		name, for_, data string
		more             []string
		window           time.Duration // 0: nothing said
		outlasts         bool
	}{
		{"query, reduce last, threshold > 0.5, for 1m", "1m", shape, nil, 30 * time.Minute, false},
		{"for 30m", "30m", shape, nil, 30 * time.Minute, true},
		{"for 1h", "1h", shape, nil, 30 * time.Minute, true},
		{"noDataState NoData, or OK", "1h", shape, []string{"\n        noDataState: OK"}, 30 * time.Minute, true},
		{"gte 1", "30m", query("A", restarts, true) + reduce("B", "last", "A") + threshold("C", "gte", "1", "B"), nil, 30 * time.Minute, true},
		{"max of an instant query's one value", "30m", query("A", restarts, true) + reduce("B", "max", "A") + threshold("C", "gt", "0", "B"), nil, 30 * time.Minute, true},
		{"mean of an instant query's one value", "30m", query("A", restarts, true) + reduce("B", "mean", "A") + threshold("C", "gt", "0", "B"), nil, 30 * time.Minute, true},
		{"a threshold on the instant query itself", "30m", query("A", restarts, true) + threshold("C", "gt", "0.5", "A"), nil, 30 * time.Minute, true},
		{"a reduce of a reduce", "30m", query("A", restarts, true) + reduce("B", "last", "A") + reduce("D", "max", "B") + threshold("C", "gt", "0.5", "D"), nil, 30 * time.Minute, true},
		{"math $B > 0.5", "30m", query("A", restarts, true) + reduce("B", "last", "A") + step("C", `type: math, expression: "$B > 0.5"`), nil, 30 * time.Minute, true},
		{"math 0 < ${A}", "30m", query("A", restarts, true) + step("C", `type: math, expression: "0 < ${A}"`), nil, 30 * time.Minute, true},
		{"a rate times a number", "15m", query("A", `rate(restarts_total[10m]) * 60 * 5`, true) + reduce("B", "last", "A") + threshold("C", "gt", "0", "B"), nil, 10 * time.Minute, true},
		// A range query's last value is its last step's: up to a step (1m)
		// past the range.
		{"last of a range query", "30m", query("A", restarts, false) + reduce("B", "last", "A") + threshold("C", "gt", "0.5", "B"), nil, 31 * time.Minute, false},
		{"last of a range query, for a step past", "31m", query("A", restarts, false) + reduce("B", "last", "A") + threshold("C", "gt", "0.5", "B"), nil, 31 * time.Minute, true},

		// Nothing said.
		{"mean of a range query", "1h", query("A", restarts, false) + reduce("B", "mean", "A") + threshold("C", "gt", "0.5", "B"), nil, 0, false},
		{"max of a range query", "1h", query("A", restarts, false) + reduce("B", "max", "A") + threshold("C", "gt", "0.5", "B"), nil, 0, false},
		{"a range query read as it is", "1h", query("A", restarts, false) + step("C", `type: math, expression: "$A > 0.5"`), nil, 0, false},
		{"count of an instant query", "1h", query("A", restarts, true) + reduce("B", "count", "A") + threshold("C", "gt", "0.5", "B"), nil, 0, false},
		{"sum of an instant query", "1h", query("A", restarts, true) + reduce("B", "sum", "A") + threshold("C", "gt", "0.5", "B"), nil, 0, false},
		{"a reduce that puts a number in place of NaN", "1h", query("A", restarts, true) + step("B", `type: reduce, reducer: last, expression: A, settings: {mode: replaceNN, replaceWithValue: 1}`) + threshold("C", "gt", "0.5", "B"), nil, 0, false},
		{"math that scales", "1h", query("A", restarts, true) + reduce("B", "last", "A") + step("C", `type: math, expression: "$B * 2 > 0.5"`), nil, 0, false},
		{"math of two comparisons", "1h", query("A", restarts, true) + reduce("B", "last", "A") + step("C", `type: math, expression: "$B > 0.5 && $B < 100"`), nil, 0, false},
		{"math of a reduce, not compared", "1h", query("A", restarts, true) + step("C", `type: math, expression: "$A"`), nil, 0, false},
		{"a math step between", "1h", query("A", restarts, true) + step("B", `type: math, expression: "$A * 2"`) + reduce("D", "last", "B") + threshold("C", "gt", "0.5", "D"), nil, 0, false},
		{"a classic condition", "1h", query("A", restarts, true) + step("C", `type: classic_conditions, conditions: [{evaluator: {type: gt, params: [0.5]}, operator: {type: and}, query: {params: [A]}, reducer: {type: last}}]`), nil, 0, false},
		{"two data queries", "1h", query("A", restarts, true) + query("D", `up`, true) + reduce("B", "last", "A") + threshold("C", "gt", "0.5", "B"), nil, 0, false},
		{"lt", "1h", query("A", restarts, true) + reduce("B", "last", "A") + threshold("C", "lt", "0.5", "B"), nil, 0, false},
		{"a range", "1h", query("A", restarts, true) + reduce("B", "last", "A") + step("C", `type: threshold, expression: B, conditions: [{evaluator: {type: within_range, params: [0.5, 10]}}]`), nil, 0, false},
		{"a number one event doesn't pass", "1h", query("A", restarts, true) + reduce("B", "last", "A") + threshold("C", "gt", "5", "B"), nil, 0, false},
		{"a number none passes", "1h", query("A", restarts, true) + reduce("B", "last", "A") + threshold("C", "gte", "0", "B"), nil, 0, false},
		{"a gauge's delta", "1h", query("A", `delta(temperature[30m])`, true) + reduce("B", "last", "A") + threshold("C", "gt", "0.5", "B"), nil, 0, false},
		{"fires on no data", "1h", shape, []string{"\n        noDataState: Alerting"}, 0, false},
		{"keeps its last state on no data", "1h", shape, []string{"\n        noDataState: KeepLast"}, 0, false},
	} {
		r := rule(c.for_, c.data, c.more...)
		w, ok := r.EventWindow()
		if ok != (c.window > 0) || w != c.window {
			t.Errorf("%s: window %s %v, want %s", c.name, w, ok, c.window)
		}
		if _, out := r.ForOutlasts(); out != c.outlasts {
			t.Errorf("%s: outlasts %v, want %v", c.name, out, c.outlasts)
		}
	}

	// Bounded as a Prometheus rule is: a query past eventMaxExpr isn't
	// read, nor reduces deeper than eventMaxDepth.
	long := `sum(increase(x{a="` + strings.Repeat("y", eventMaxExpr) + `"}[30m]))`
	if _, ok := rule("1h", query("A", long, true)+reduce("B", "last", "A")+threshold("C", "gt", "0.5", "B")).EventWindow(); ok {
		t.Error("a query past 4 KB: read")
	}
	deep := func(n int) string {
		data := query("A", restarts, true) + reduce("R0", "last", "A")
		for i := 1; i < n; i++ {
			data += reduce("R"+strconv.Itoa(i), "last", "R"+strconv.Itoa(i-1))
		}
		return data + threshold("C", "gt", "0.5", "R"+strconv.Itoa(n-1))
	}
	if _, ok := rule("1h", deep(eventMaxDepth)).EventWindow(); !ok {
		t.Errorf("%d reduces: not read", eventMaxDepth)
	}
	if _, ok := rule("1h", deep(eventMaxDepth+3)).EventWindow(); ok {
		t.Errorf("%d reduces: read", eventMaxDepth+3)
	}
}
