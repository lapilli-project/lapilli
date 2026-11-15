package rules

import (
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
