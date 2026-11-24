package rules

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Prometheus 3.5.0 runs the rules of rules.yml (internal/replay/testdata),
// and its rules API answers each as api.json has it: its expression printed
// again (on() as on ()), its for in seconds, its group's interval too. The
// same rule, its Whole another, has one Parsed; but for the group with a
// query_offset, which the API doesn't say: there, the rule as the API
// gives it is replayed otherwise than as its file says, and the two aren't
// one.
func TestParsedRecordedPrometheus(t *testing.T) {
	file, err := Load("../replay/testdata/prometheus-3.5.0/rules.yml")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile("../replay/testdata/prometheus-3.5.0/api.json")
	if err != nil {
		t.Fatal(err)
	}
	api, err := FromAPI(b)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Rule{}
	for _, r := range api {
		byName[r.Name] = r
	}
	if len(file) != 6 || len(api) != 6 {
		t.Fatalf("%d rules in the file, %d in the answer", len(file), len(api))
	}
	for _, f := range file {
		a, ok := byName[f.Name]
		if !ok {
			t.Fatalf("%s isn't in the answer", f.Name)
		}
		fp, fok := f.Parsed()
		ap, aok := a.Parsed()
		if !fok || !aok {
			t.Fatalf("%s: not read: %q %q", f.Name, f.Expr, a.Expr)
		}
		if f.Whole() == a.Whole() {
			t.Errorf("%s: its file and the API write it alike: %q", f.Name, f.Expr)
		}
		if want := f.Name != "QueryOffset"; (fp == ap) != want {
			t.Errorf("%s: one %t, want %t:\n%s\n%s", f.Name, fp == ap, want, fp, ap)
		}
	}
	if byName["SpikeE"].For != time.Minute {
		t.Errorf("SpikeE's for read from the API: %v", byName["SpikeE"].For)
	}
}

// vmalert's rules API gives a rule's query as its file has it, and its
// group's interval in seconds (app/vmalert/rule/web.go, v1.156.0): one with
// the file, as its text is too but for the interval.
func TestParsedRecordedVmalert(t *testing.T) {
	api, err := FromAPI([]byte(`{"status":"success","data":{"groups":[
	  {"name":"shop","rules":[{"state":"inactive","name":"ErrorsHigh","query":"e2e_errors > 3","duration":120,"keep_firing_for":300,
	    "labels":{"severity":"page"},"lastError":"","evaluationTime":0.001,"lastEvaluation":"2026-11-20T10:04:23.417Z","health":"ok",
	    "type":"alerting","datasourceType":"prometheus","lastSamples":0,"id":"1","group_id":"2","group_name":"shop","file":"/etc/vmalert/alerts.yaml","debug":false,"max_updates_entries":0}],
	   "interval":60,"lastEvaluation":"2026-11-20T10:04:23.417Z","type":"prometheus","id":"2","file":"/etc/vmalert/alerts.yaml","concurrency":1,"states":{"ok":1}}]}}`))
	if err != nil || len(api) != 1 {
		t.Fatal(api, err)
	}
	file, err := Parse([]byte("groups:\n  - name: shop\n    interval: 1m\n    rules:\n      - alert: ErrorsHigh\n        expr: e2e_errors > 3\n" +
		"        for: 2m\n        keep_firing_for: 5m\n        labels: {severity: page}\n"))
	if err != nil || len(file) != 1 {
		t.Fatal(file, err)
	}
	fp, fok := file[0].Parsed()
	ap, aok := api[0].Parsed()
	if !fok || !aok || fp != ap {
		t.Errorf("not one:\n%s\n%s", fp, ap)
	}
}

// Each pair is one expression as a rule file writes it and as Prometheus
// 3.5.0's promql/parser prints it again (Expr.String, which its rules API,
// Mimir's and Thanos' answer with), or another writing of the same: one
// Parsed.
func TestParsedExprAlike(t *testing.T) {
	for _, c := range [][2]string{
		// As Prometheus 3.5.0 prints them.
		{`vector(1) and on() (vector(time() % 180) < 40)`, `vector(1) and on () (vector(time() % 180) < 40)`},
		{`sum(rate(http_requests_total{job="api",code=~"5.."}[5m])) by (service, job) / sum(rate(http_requests_total{job="api"}[300s])) by (job,service) > 0.05`,
			`sum by (service, job) (rate(http_requests_total{code=~"5..",job="api"}[5m])) / sum by (job, service) (rate(http_requests_total{job="api"}[5m])) > 0.05`},
		{"up{job='node'} == 0 # down\n", `up{job="node"} == 0`},
		{`SUM(x) BY (a) > 1e3`, `sum by (a) (x) > 1000`},
		{`x AND ON(b) y`, `x and on (b) y`},
		{`x > 1e-7`, `x > 0.0000001`},
		{`x > .5`, `x > 0.5`},
		{`x > 5.`, `x > 5`},
		{`x > 1E3`, `x > 1000`},
		{`x > +5`, `x > 5`},
		{`- 5`, `-5`},
		{`x > Inf`, `x > +Inf`},
		{`x @ 100 offset 5m`, `x @ 100.000 offset 5m`},
		{`x offset 5m @ 100`, `x @ 100.000 offset 5m`},
		{`x[5m] offset 1h @ 100`, `x[5m] @ 100.000 offset 1h`},
		{`x offset 0s`, `x`},
		{`rate(x[90m])`, `rate(x[1h30m])`},
		{`a * on(x) group_left b`, `a * on (x) group_left () b`},
		{`a * ignoring() b`, `a * b`},
		{"label_replace(up, \"a\", `$1`, 'b', \"(.*)\")", `label_replace(up, "a", "$1", "b", "(.*)")`},
		{`x{}`, `x`},
		{`sum by () (x)`, `sum(x)`},
		{`x{a="b",}`, `x{a="b"}`},
		{`sum(x) by (a,)`, `sum by (a) (x)`},
		{`x{a='it\'s'}`, `x{a="it's"}`},
		{"x{a=`a\\b`}", `x{a="a\\b"}`},
		{`x{a="\x41"}`, `x{a="A"}`},
		{`x{a="\u00e9"}`, `x{a="é"}`},
		{`quantile(0.9, x) by (a)`, `quantile by (a) (0.9, x)`},
		{`sum(x) without (a)`, `sum without (a) (x)`},
		{`x > bool on(a) group_right(c) y`, `x > bool on (a) group_right (c) y`},
		{`max(x) by (le) `, `max by (le) (x)`},
		{`x{a!~"b", c!="d"}`, `x{a!~"b",c!="d"}`},
		{"histogram_quantile(0.99,\n  sum by (le) (rate(x_bucket[5m]))\n)", `histogram_quantile(0.99, sum by (le) (rate(x_bucket[5m])))`},
		{"errors > 5 # five\r\n", `errors > 5`},
		// The same, written otherwise.
		{`sum without (b, a) (x)`, `sum without (a, b) (x)`},
		{`x and on(b, a) y`, `x and on (a, b) y`},
		{`x * on(a) group_left(c, b) y`, `x * on (a) group_left (b, c) y`},
		{`{__name__="x", job="a"}`, `x{job="a"}`},
		{`(x)`, `x`},
		{`((x + y))`, `x + y`},
		{`(sum(x)) > 1`, `sum(x) > 1`},
		{`topk((3), (http_requests))`, `topk(3, http_requests)`},
		{`(x offset 5m) ^ 2`, `x offset 5m ^ 2`},
		{`-(x)`, `-x`},
		{`-(5)`, `-5`},
		{`x[1h:5m0s]`, `x[1h:5m]`},
		{`x offset 1h0m`, `x offset 60m`},
		{`rate(x[1000ms])`, `rate(x[1s])`},
	} {
		a, aok := parsedExpr(c[0])
		b, bok := parsedExpr(c[1])
		if !aok || !bok || a != b {
			t.Errorf("%q and %q aren't one:\n%t %s\n%t %s", c[0], c[1], aok, a, bok, b)
		}
	}
}

// Each pair may fire otherwise: never one Parsed.
func TestParsedExprUnalike(t *testing.T) {
	for _, c := range [][2]string{
		{`errors > 5`, `errors > 6`},
		{`errors > 5`, `errors >= 5`},
		{`errors > 5`, `errors < 5`},
		{`errors == 5`, `errors != 5`},
		{`x > 1e3`, `x > 1e4`},
		{`x > 5`, `x > -5`},
		{`x - y`, `y - x`},
		{`x > bool 1`, `x > 1`},
		{`errors{job="a"} > 5`, `errors{job="b"} > 5`},
		{`errors{job="a"}`, `errors{job=~"a"}`},
		{`errors{job="a"}`, `errors{job!="a"}`},
		{`errors{job="a"}`, `errors{job="a",env="b"}`},
		{`errors{job="a"}`, `errors{job="a",job="a"}`},
		{`errors{job="a"}`, `errors{job="a "}`},
		{`errors{job="a"}`, `errors{Job="a"}`},
		{`X`, `x`},
		{`sum by (a) (x)`, `sum without (a) (x)`},
		{`sum by (a) (x)`, `sum by (a, b) (x)`},
		{`sum by (a, a) (x)`, `sum by (a) (x)`},
		{`sum by (a) (x)`, `sum by (A) (x)`},
		{`sum(x)`, `sum without () (x)`},
		{`sum(x)`, `avg(x)`},
		{`x and on (a) y`, `x and ignoring (a) y`},
		{`x and on () y`, `x and y`},
		{`x * on (a) group_left y`, `x * on (a) group_right y`},
		{`x * on (a) group_left y`, `x * on (a) y`},
		{`x and y`, `x or y`},
		{`x and y`, `x unless y`},
		{`a or b and c`, `(a or b) and c`},
		{`-2 ^ 2`, `(-2) ^ 2`},
		{`rate(x[5m])`, `rate(x[10m])`},
		{`rate(x[5m])`, `irate(x[5m])`},
		{`rate(x[5m])`, `RATE(x[5m])`},
		{`x[1h:5m]`, `x[1h:]`},
		{`x[1h:5m]`, `x[1h:1m]`},
		{`x offset 5m`, `x offset -5m`},
		{`x offset 5m`, `x`},
		{`x @ 5`, `x @ 5.0004`},
		{`x @ start()`, `x @ end()`},
		{`topk(5, x)`, `topk(x, 5)`},
		{`label_replace(x, "a", "$1", "b", "(.*)")`, `label_replace(x, "a", "$1", "b", "(.+)")`},
		{`absent(x)`, `absent(x) or vector(0)`},
	} {
		a, aok := parsedExpr(c[0])
		b, bok := parsedExpr(c[1])
		if !aok || !bok {
			t.Errorf("%q or %q not read: %t %t", c[0], c[1], aok, bok)
			continue
		}
		if a == b {
			t.Errorf("%q and %q are one: %s", c[0], c[1], a)
		}
	}
}

// What may be the same but isn't known to be is two: a hexadecimal or a
// leading 0 (010, which Prometheus reads as 8 and MetricsQL may not), a
// label's or a metric's name in quotes beside one without, and ignoring ()
// with group_left, which Prometheus 3.5.0 prints as nothing at all, and
// so as another rule (a * b).
func TestParsedExprNotKnownAlike(t *testing.T) {
	for _, c := range [][2]string{
		{`x > 0x10`, `x > 16`},
		{`x > 010`, `x > 8`},
		{`x > 010`, `x > 10`},
		{`{"x"}`, `{__name__="x"}`},
		{`x{"job"="a"}`, `x{job="a"}`},
		{`sum by ("job") (x)`, `sum by (job) (x)`},
		{`a * ignoring() group_left b`, `a * b`},
		{`rate((x[5m]))`, `rate(x[5m])`},
		{`(x)[5m:]`, `x[5m:]`},
		{`+x`, `x`},
	} {
		a, aok := parsedExpr(c[0])
		b, bok := parsedExpr(c[1])
		if aok && bok && a == b {
			t.Errorf("%q and %q are one: %s", c[0], c[1], a)
		}
	}
}

// What isn't read so has no Parsed: Prometheus refuses it, or it reads it
// as only some histories do (Prometheus 3's durations as numbers and numbers
// as durations), or MetricsQL reads it otherwise (or in braces, WITH, a
// backtick's string ending in \, a name in quotes with a \, 1d-1y), or it is
// past 16 KiB or 32 levels of brackets.
func TestParsedExprRefused(t *testing.T) {
	for _, expr := range []string{
		``,
		`  # nothing`,
		`x >`,
		`(x`,
		`x)`,
		`x{a="b"`,
		`x{a=b}`,
		`x{a:b="c"}`,
		`x{a="b" c="d"}`,
		`x{,}`,
		`x > 1_000`,
		`x > 1e400`,
		`x > 1e`,
		`x > 0x1e+5`,
		`x > 5m`,
		`predict_linear(x[1h], 50m)`,
		`rate(x[300])`,
		`x offset 300`,
		`x @ 100s`,
		`x offset 1d-1y`,
		`x offset +5m`,
		`f(x,)`,
		`foo:bar(x)`,
		`sum(x) by (a) by (b)`,
		`sum by (a) (x) without (b)`,
		`x{a="b" or c="d"}`,
		`WITH (y = x) y`,
		"x{a=`a\\`}",
		`x{"a\x41"="b"}`,
		`x{__name__="y"}`,
		`x{"y"}`,
		`sum{a="b"}`,
		`x and by`,
		`x = 5`,
		`x =~ "a"`,
		"x{a=\"a\nb\"}",
		`x{a="` + "\uFFFD" + `"}`,
		`x{a="\q"}`,
		`x{a='a"}`,
		`métrique > 1`,
		`x[1h:0m5m:]`,
		`x[5m # c` + "\n]",
		// Prometheus ends a comment at \r, MetricsQL doesn't: vmalert runs
		// this as errors > 5, Prometheus as errors > 5 or latency > 1.
		"errors > 5 #\ror latency > 1",
		"errors > 5 # five\r",
		strings.Repeat("(", 33) + "x" + strings.Repeat(")", 33),
		strings.Repeat("abs(", 32) + "x" + strings.Repeat(")", 32),
		"x > " + strings.Repeat("1 + ", 4<<10) + "1",
	} {
		if got, ok := parsedExpr(expr); ok {
			t.Errorf("%q read: %s", expr, got)
		}
	}
	// 32 levels are read.
	if _, ok := parsedExpr(strings.Repeat("(", 31) + "x" + strings.Repeat(")", 31)); !ok {
		t.Error("31 parentheses deep, not read")
	}
}

// A rule's Parsed: its for, keep_firing_for, interval and the rest by value
// as in Whole, its group's labels with its own as one map, whichever wrote
// them apart; and none for a rule whose expression isn't PromQL Lapilli
// reads so.
func TestRuleParsed(t *testing.T) {
	base := Rule{Name: "Errors", Group: "shop", Expr: "errors > 5", For: 5 * time.Minute, Interval: time.Minute,
		Labels: map[string]string{"severity": "page", "team": "shop"}}
	one := func(r Rule) string {
		t.Helper()
		s, ok := r.Parsed()
		if !ok {
			t.Fatalf("%+v: no Parsed", r)
		}
		return s
	}
	want := one(base)
	apart := base
	apart.Labels, apart.GroupLabels = map[string]string{"severity": "page"}, map[string]string{"team": "shop", "severity": "ticket"}
	reprinted := base
	reprinted.Expr, reprinted.Source = "errors  >  5.0 # five", "api"
	for name, r := range map[string]Rule{"its group's labels apart": apart, "its expression written otherwise": reprinted} {
		if got := one(r); got != want {
			t.Errorf("%s: another:\n%s\n%s", name, got, want)
		}
	}
	for name, change := range map[string]func(*Rule){
		"another expression":      func(r *Rule) { r.Expr = "errors > 6" },
		"another for":             func(r *Rule) { r.For = 10 * time.Minute },
		"another keep_firing_for": func(r *Rule) { r.KeepFiringFor = time.Minute },
		"another interval":        func(r *Rule) { r.Interval = 30 * time.Second },
		"a query_offset":          func(r *Rule) { r.QueryOffset = time.Minute },
		"another label":           func(r *Rule) { r.Labels = map[string]string{"severity": "ticket", "team": "shop"} },
		"a label fewer":           func(r *Rule) { r.Labels = map[string]string{"severity": "page"} },
		"another group":           func(r *Rule) { r.Group = "cart" },
		"a limit":                 func(r *Rule) { r.Limit = 10 },
		"another name":            func(r *Rule) { r.Name = "Errors2" },
	} {
		r := base
		r.Labels = map[string]string{"severity": "page", "team": "shop"}
		change(&r)
		if got := one(r); got == want {
			t.Errorf("%s: the same Parsed", name)
		}
	}
	// Labels whose Key is one, of a name Prometheus 3 takes: two.
	eq, in := base, base
	eq.Labels, in.Labels = map[string]string{"a=b": "c"}, map[string]string{"a": "b=c"}
	if eq.Key() != in.Key() || one(eq) == one(in) {
		t.Errorf("a=b=c: Key %t, Parsed %t", eq.Key() == in.Key(), one(eq) == one(in))
	}
	for name, r := range map[string]Rule{
		"a Grafana rule":   {Name: "G", Graph: &Graph{}},
		"a Loki rule":      {Name: "L", Expr: `sum(count_over_time({app="a"}[1m])) > 1`, Logs: LokiRuler},
		"a cloud's alarm":  {Name: "C", Expr: "errors > 5", Cloud: "cloudwatch"},
		"not read":         {Name: "N", Expr: "x > 1_000"},
		"no expression":    {Name: "E"},
		"MetricsQL's WITH": {Name: "W", Expr: "WITH (y = x) y > 1"},
	} {
		if s, ok := r.Parsed(); ok {
			t.Errorf("%s: %s", name, s)
		}
	}
}

// Whatever is given, it is read to an end or refused: never a panic.
func FuzzParsedExpr(f *testing.F) {
	for _, s := range []string{`sum by (a) (rate(x{b="c"}[5m] offset 1h @ 100)) > bool on (a) group_left (d) y`,
		"x{a=`b`} # c\n", `-(-(1)) ^ 2`, `x[1h:5m]`, `topk((3), (x))`, `{"x", "a"="b"}`, `0x1e+5`, `1e+3`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, expr string) {
		a, aok := parsedExpr(expr)
		b, bok := parsedExpr(expr)
		if a != b || aok != bok {
			t.Errorf("%q read twice otherwise", expr)
		}
	})
}
