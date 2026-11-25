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
		`(((increase(x[5m]))))>0`, `max(changes(x[1d] offset -5m)) by (a) > 0 and y`, `increase(x[30m] > 0.5`, `sum(sum(sum(increase(x[1m]))))>0`,
		`count_over_time({app="]"} |= "[" [5m]) > 0`, `sum by (a) (bytes_rate({a=~".+"} | json | unwrap [1m])) > 0`, `rate({a="b"}[5m] offset 1m) * 60 >= 1`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, expr string) {
		// Read as PromQL, and as a Loki ruler's LogQL.
		for _, logs := range []string{"", LokiRuler} {
			r := Rule{Expr: expr, For: time.Hour, Logs: logs}
			if w, ok := r.EventWindow(); ok && w <= 0 {
				t.Errorf("%q (%q): window %s", expr, logs, w)
			}
		}
	})
}

// What EventWindow reads is bounded: an expression longer than eventMaxExpr
// isn't read, and the longest, deepest one that is costs little, so a rule
// writer can't make the content check, which asks it of every rule, slow.
// What it costs is the bytes it reads (exprRead), counted, not timed.
func TestEventWindowBounded(t *testing.T) {
	deep := func(levels, size int) string {
		e := "increase(x{a=\"" + strings.Repeat("y", size) + "\"}[30m])"
		for range levels {
			e = "sum(" + e + ")"
		}
		return e + " > 0.5"
	}
	n := 0
	exprRead = func(k int) { n += k }
	defer func() { exprRead = nil }()
	long := Rule{Expr: deep(31, 16<<10), For: time.Hour}
	if _, ok := long.EventWindow(); ok || n != 0 {
		t.Errorf("16 KB, 31 deep: %v, %d bytes read, want none", ok, n)
	}
	worst := Rule{Expr: deep(7, eventMaxExpr-200), For: time.Hour}
	if len(worst.Expr) > eventMaxExpr {
		t.Fatal(len(worst.Expr))
	}
	n = 0
	start := time.Now()
	for range 100 {
		if _, ok := worst.EventWindow(); !ok {
			t.Fatal("7 sums deep, under 4 KB: counts events")
		}
	}
	t.Logf("100 of the deepest, longest read: %d bytes each, in %s", n/100, time.Since(start))
	if most := readMost(worst.Expr, eventMaxDepth); n/100 > most {
		t.Errorf("the deepest, longest: %d bytes read, over %d", n/100, most)
	}
}

// A Grafana rule counts events when its condition compares its one PromQL
// query's value, as it is or through a reduce that leaves it so, with a
// number one event passes: a restart rule written in Grafana as a query, a
// reduce (last) and a threshold (> 0.5) has the window a Prometheus rule of
// the same PromQL has, and a for one restart can't outlast. Nothing is said
// of one whose value is changed on its way, or read any other way.
func TestGrafanaEventWindow(t *testing.T) {
	const restarts = `sum by (cluster, namespace, container) (increase(kube_pod_container_status_restarts_total{namespace!~"kube-.*"}[30m]))`
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

		// Nothing said. A range query's points one event can hold for as
		// long again as its range: a pod's series goes on being returned
		// over the range after the pod is gone.
		{"last of a range query", "1h", query("A", restarts, false) + reduce("B", "last", "A") + threshold("C", "gt", "0.5", "B"), nil, 0, false},
		{"a reduce of the last of a range query", "1h", query("A", restarts, false) + reduce("B", "last", "A") + reduce("D", "last", "B") + threshold("C", "gt", "0.5", "D"), nil, 0, false},
		{"mean of a range query", "1h", query("A", restarts, false) + reduce("B", "mean", "A") + threshold("C", "gt", "0.5", "B"), nil, 0, false},
		{"max of a range query", "1h", query("A", restarts, false) + reduce("B", "max", "A") + threshold("C", "gt", "0.5", "B"), nil, 0, false},
		{"count of an instant query", "1h", query("A", restarts, true) + reduce("B", "count", "A") + threshold("C", "gt", "0.5", "B"), nil, 0, false},
		{"sum of an instant query", "1h", query("A", restarts, true) + reduce("B", "sum", "A") + threshold("C", "gt", "0.5", "B"), nil, 0, false},
		{"a reduce that puts a number in place of NaN", "1h", query("A", restarts, true) + step("B", `type: reduce, reducer: last, expression: A, settings: {mode: replaceNN, replaceWithValue: 1}`) + threshold("C", "gt", "0.5", "B"), nil, 0, false},
		{"a reduce of a reduce that puts a number in place of NaN", "1h", query("A", restarts, true) + reduce("B", "last", "A") + step("D", `type: reduce, reducer: last, expression: B, settings: {mode: replaceNN, replaceWithValue: 1}`) + threshold("C", "gt", "0.5", "D"), nil, 0, false},
		{"a reduce of a reduce that drops NaN", "30m", query("A", restarts, true) + reduce("B", "last", "A") + step("D", `type: reduce, reducer: last, expression: B, settings: {mode: dropNN}`) + threshold("C", "gt", "0.5", "D"), nil, 30 * time.Minute, true},
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

// A Loki ruler's rule counts events when its value is how many lines a
// stream's pipeline let through over a range (count_over_time), them a
// second (rate), their bytes (bytes_over_time) or those a second
// (bytes_rate), summed or maxed or not, and one line passes its bound: a
// line holds what it counts for that range at most, as an increase's event
// does. Nothing is said of a pipeline that unwraps a label, whose values
// are then taken, of a range before the pipeline, of a comment Loki reads
// that PromQL doesn't, or of a bound one line doesn't pass.
func TestLogEventWindow(t *testing.T) {
	errors := `count_over_time({app="shop"} |= "error" [5m]) > 0`
	for _, c := range []struct {
		expr     string
		for_     time.Duration
		window   time.Duration // 0: not one that counts events
		outlasts bool
	}{
		{errors, time.Minute, 5 * time.Minute, false},
		{errors, 5 * time.Minute, 5 * time.Minute, true},
		{`sum by (app) (count_over_time({app="shop"} |= "error" | json | level="error" [10m])) >= 1`, 10 * time.Minute, 10 * time.Minute, true},
		{`sum(count_over_time({app="shop"} |~ "panic|fatal" [10m])) by (app) > 0.5`, 15 * time.Minute, 10 * time.Minute, true},
		{`max without (pod) (count_over_time({app="shop"} [1h] offset 5m)) > 0`, 2 * time.Hour, time.Hour, true},
		{`rate({app="shop"} |= "panic" [5m]) > 0`, 10 * time.Minute, 5 * time.Minute, true},
		{`rate({app="shop"} |= "panic" [5m]) * 300 >= 1`, 10 * time.Minute, 5 * time.Minute, true},
		{`bytes_over_time({app="shop"} |= "panic" [5m]) > 0`, 5 * time.Minute, 5 * time.Minute, true},
		{`bytes_rate({app="shop"} |= "panic" [5m]) > 0`, 5 * time.Minute, 5 * time.Minute, true},
		{`0 < count_over_time({app="shop"} |= "error" [20m])`, 20 * time.Minute, 20 * time.Minute, true},
		// A pipeline that starts by keeping the lines without something.
		{`count_over_time({app="shop"} != "debug" [5m]) > 0`, 5 * time.Minute, 5 * time.Minute, true},
		{`count_over_time({app="shop"} !~ "debug|trace" |= "error" [5m]) > 0`, 5 * time.Minute, 5 * time.Minute, true},
		{`count_over_time({app="shop"} !> "<_> debug <_>" [5m]) > 0`, 5 * time.Minute, 5 * time.Minute, true},
		{`count_over_time({app="shop"} ! "debug" [5m]) > 0`, time.Hour, 0, false},
		// A bracket or a word in a string is the string's.
		{`count_over_time({app="shop"} |~ "code=[45][0-9]{2}" |= "unwrap" [5m]) > 0`, 5 * time.Minute, 5 * time.Minute, true},
		{`count_over_time({app="shop"} |= "error" [5m]) > 0 and count_over_time({app="cart"} |= "error" [1h]) > 0`, 30 * time.Minute, time.Hour, false},
		{`# errors
count_over_time({app="shop"} |= "error" [5m]) # in five minutes
  > 0`, 5 * time.Minute, 5 * time.Minute, true},

		// Not sure, so nothing said.
		{`count_over_time({app="shop"}[5m]) > 5`, time.Hour, 0, false},
		{`bytes_over_time({app="shop"}[5m]) > 100`, time.Hour, 0, false},
		{`rate({app="shop"}[5m]) > 1`, time.Hour, 0, false},
		{`count_over_time({app="shop"}[5m]) >= 0`, time.Hour, 0, false},
		{`count_over_time({app="shop"}[5m]) < 1`, time.Hour, 0, false},
		{`rate({app="shop"} | logfmt | unwrap latency [5m]) > 0`, time.Hour, 0, false},
		{`sum_over_time({app="shop"} | logfmt | unwrap bytes [5m]) > 0`, time.Hour, 0, false},
		{`count_over_time({app="shop"}[5m] |= "error") > 0`, time.Hour, 0, false},
		{`count_over_time({app="shop"} |= "error" [5m]) > 0 // errors`, time.Hour, 0, false},
		{`count_over_time({app="shop"} |= "error" /* errors */ [5m]) > 0`, time.Hour, 0, false},
		{`count_over_time({app="shop"} |= "error" [5m]) > 0 or vector(0)`, time.Hour, 0, false},
		{`count_over_time({app="shop"} |= "error" [5m]) > 0 unless count_over_time({app="shop"} |= "ok" [5m]) > 0`, time.Hour, 0, false},
		{`absent_over_time({app="shop"}[5m]) > 0`, time.Hour, 0, false},
		{`avg(count_over_time({app="shop"}[5m])) > 0`, time.Hour, 0, false},
		{`count_over_time({app="shop"}[5m]) + count_over_time({app="cart"}[5m]) > 0`, time.Hour, 0, false},
		{`count_over_time(app[5m]) > 0`, time.Hour, 0, false},
		{`count_over_time({app="shop"}[5m]) > bool 0`, time.Hour, 0, false},
		{`count_over_time({app="shop"}[5m]`, time.Hour, 0, false},
		// PromQL's counts, read as LogQL: none of LogQL's.
		{`increase(x[30m]) > 0.5`, time.Hour, 0, false},
	} {
		r := Rule{Name: "R", Expr: c.expr, For: c.for_, Logs: LokiRuler}
		w, ok := r.EventWindow()
		if ok != (c.window > 0) || w != c.window {
			t.Errorf("%s: window %s %v, want %s", c.expr, w, ok, c.window)
		}
		if _, out := r.ForOutlasts(); out != c.outlasts {
			t.Errorf("%s for %s: outlasts %v, want %v", c.expr, c.for_, out, c.outlasts)
		}
	}
	// LogQL's counts in a Prometheus rule are no PromQL: nothing said.
	if _, ok := (Rule{Expr: errors, For: time.Hour}).EventWindow(); ok {
		t.Error("count_over_time read as PromQL")
	}
}

// A Grafana rule on Loki counts events as a Loki ruler's rule does, of its
// one instant query, as it is or through a reduce that leaves its value so,
// compared with a number one line passes. A range query, as of PromQL, says
// nothing.
func TestGrafanaLogEventWindow(t *testing.T) {
	rule := func(for_, queryType, expr, cond string) Rule {
		t.Helper()
		rs, err := Parse([]byte(`apiVersion: 1
groups:
  - orgId: 1
    name: logs
    folder: Logs
    interval: 1m
    rules:
      - uid: errors
        title: Shop errors
        condition: C
        for: ` + for_ + `
        data:
          - refId: A
            relativeTimeRange: {from: 600, to: 0}
            datasourceUid: lk
            model: {datasource: {type: loki, uid: lk}, expr: '` + expr + `', intervalMs: 1000, queryType: ` + queryType + `}
          - refId: B
            datasourceUid: __expr__
            model: {type: reduce, expression: A, reducer: last}
          - refId: C
            datasourceUid: __expr__
            model: ` + cond + "\n"))
		if err != nil || len(rs) != 1 || rs[0].Graph == nil || rs[0].Unsupported != "" || rs[0].Logs == "" {
			t.Fatalf("%v %+v", err, rs)
		}
		return rs[0]
	}
	const errors = `sum by (app) (count_over_time({app="shop"} |= "error" [5m]))`
	gt := func(n, in string) string {
		return `{type: threshold, expression: ` + in + `, conditions: [{evaluator: {type: gt, params: [` + n + `]}}]}`
	}
	for _, c := range []struct {
		name               string
		for_, qt, expr, cd string
		window             time.Duration
		outlasts           bool
	}{
		{"instant, last, > 0, for 5m", "5m", "instant", errors, gt("0", "B"), 5 * time.Minute, true},
		{"for 1m", "1m", "instant", errors, gt("0", "B"), 5 * time.Minute, false},
		{"on the query itself", "10m", "instant", errors, gt("0", "A"), 5 * time.Minute, true},
		{"math", "10m", "instant", `rate({app="shop"} |= "panic" [5m])`, `{type: math, expression: "$B > 0"}`, 5 * time.Minute, true},
		{"a range query", "1h", "range", errors, gt("0", "B"), 0, false},
		{"a bound one line doesn't pass", "1h", "instant", errors, gt("10", "B"), 0, false},
		{"unwrap", "1h", "instant", `sum(rate({app="shop"} | logfmt | unwrap latency [5m]))`, gt("0", "B"), 0, false},
	} {
		r := rule(c.for_, c.qt, c.expr, c.cd)
		w, ok := r.EventWindow()
		if ok != (c.window > 0) || w != c.window {
			t.Errorf("%s: window %s %v, want %s", c.name, w, ok, c.window)
		}
		if _, out := r.ForOutlasts(); out != c.outlasts {
			t.Errorf("%s: outlasts %v, want %v", c.name, out, c.outlasts)
		}
	}
}
