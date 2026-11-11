package rules

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/sse"
)

func TestBound(t *testing.T) {
	for _, c := range []struct {
		expr, value, op string
		n               float64
		ok              bool
		with            string // the expression with 0.5 for the number
	}{
		{"errors > 0.9", "errors", ">", 0.9, true, "errors > 0.5"},
		{`sum by (job) (rate(http_errors{code=~"5.."}[5m])) / sum by (job) (rate(http_requests[5m])) >= 0.05`,
			`sum by (job) (rate(http_errors{code=~"5.."}[5m])) / sum by (job) (rate(http_requests[5m]))`, ">=", 0.05, true,
			`sum by (job) (rate(http_errors{code=~"5.."}[5m])) / sum by (job) (rate(http_requests[5m])) >= 0.5`},
		{"0.9 < errors", "errors", ">", 0.9, true, "0.5 < errors"},
		{"up{job=\"a>b\"} <= 0", `up{job="a>b"}`, "<=", 0, true, `up{job="a>b"} <= 0.5`},
		{"free_bytes < 1e+09 # under a gigabyte", "free_bytes", "<", 1e9, true, "free_bytes < 0.5 # under a gigabyte"},
		{"x > -3", "x", ">", -3, true, "x > 0.5"},
		{"orders_total > 10", "orders_total", ">", 10, true, "orders_total > 0.5"},
		{"(errors > 0.9)", "errors", ">", 0.9, true, "(errors > 0.5)"},                     // a whole in brackets
		{"(errors > 0.9) * 2", "", "", 0, false, ""},                                       // part of a value
		{"errors > bool 0.9", "", "", 0, false, ""},                                        // returns every series
		{"errors > 0.9 and on() up", "errors", ">", 0.9, true, "errors > 0.5 and on() up"}, // a set operation
		{"errors > 0.9 unless maintenance", "errors", ">", 0.9, true, "errors > 0.5 unless maintenance"},
		{"errors > other", "", "", 0, false, ""}, // no number
		{"0 < errors < 1", "", "", 0, false, ""}, // two comparisons
		{"errors == 1", "", "", 0, false, ""},    // not above or below
		{"errors > Inf", "", "", 0, false, ""},
		{`absent(up{job="a"})`, "", "", 0, false, ""},
		{`errors > 0.9 or vector(0)`, "errors", ">", 0.9, true, "errors > 0.5 or vector(0)"},
		{"x > 0.9 # high\n + 1", "", "", 0, false, ""}, // the comment ends, the expression goes on
		{"0.9 # c\n + 0.1 < x", "", "", 0, false, ""},
		{"x > 0.9 # high\n# and more\n", "x", ">", 0.9, true, "x > 0.5 # high\n# and more\n"},
		{"x > 1e-310", "", "", 0, false, ""}, // too small to be a number to come near
		{"x > 0", "x", ">", 0, true, "x > 0.5"},
		{"(x > 1", "", "", 0, false, ""},
		{`x{a="b} > 1`, "", "", 0, false, ""},
	} {
		r := Rule{Name: "R", Expr: c.expr}
		bs := r.Bounds()
		ok := len(bs) == 1
		if ok != c.ok || ok && (bs[0].Op != c.op || bs[0].Value != c.n || comparisons(c.expr)[0].value != c.value) {
			t.Errorf("%s: %+v, want %v %s %v %q", c.expr, bs, c.ok, c.op, c.n, c.value)
			continue
		}
		if !ok {
			continue
		}
		if got := r.WithBound(bs[0], 0.5); got.Expr != c.with {
			t.Errorf("%s: with 0.5 %q, want %q", c.expr, got.Expr, c.with)
		}
		if v, ok := r.ValueOf(bs[0]); bs[0].Values && (!ok || v.Expr != r.WithBound(bs[0], bs[0].Furthest()).Expr) {
			t.Errorf("%s: value %v %q", c.expr, ok, v.Expr)
		}
	}
	// Not one of the rule's: the rule as it is.
	r := Rule{Name: "R", Expr: "x > 1"}
	if got := r.WithBound(Bound{Op: "<", Value: 1}, 2); got.Expr != r.Expr {
		t.Errorf("another's bound: %q", got.Expr)
	}
}

func TestBounds(t *testing.T) {
	type b struct {
		op     string
		n      float64
		of     string
		unless bool
		values bool
	}
	for _, c := range []struct {
		expr string
		want []b
		with string // the expression with the last number at 7
	}{
		{"rate(errors[5m]) > 0.05 and rate(requests[5m]) > 10",
			[]b{{">", 0.05, "rate(errors[5m])", false, true}, {">", 10, "rate(requests[5m])", false, false}},
			"rate(errors[5m]) > 0.05 and rate(requests[5m]) > 7"},
		{"errors > 0.9 unless on(job) requests < 10",
			[]b{{">", 0.9, "errors", false, true}, {"<", 10, "requests", true, false}},
			"errors > 0.9 unless on(job) requests < 7"},
		{"a > 1 or b < 2", []b{{">", 1, "a", false, false}, {"<", 2, "b", false, false}}, "a > 1 or b < 7"},
		{"(a > 1 and b > 2) unless (c < 3)",
			[]b{{">", 1, "a", false, true}, {">", 2, "b", false, false}, {"<", 3, "c", true, false}}, "(a > 1 and b > 2) unless (c < 7)"},
		{"a > 1 and b > 2 or c > 3", []b{{">", 1, "a", false, false}, {">", 2, "b", false, false}, {">", 3, "c", false, false}}, "a > 1 and b > 2 or c > 7"},
		// (x unless y > 5) and z > 1.
		{"x unless y > 5 and z > 1", []b{{">", 5, "y", true, false}, {">", 1, "z", false, false}}, "x unless y > 5 and z > 7"},
		// a unless (b unless c): c holding keeps b from keeping a quiet.
		{"a > 1 unless (b > 2 unless c > 3)", []b{{">", 1, "a", false, true}, {">", 2, "b", true, false}, {">", 3, "c", false, false}}, "a > 1 unless (b > 2 unless c > 7)"},
		{"a{job=\"x and y\"} > 1 # or so\n and b > 2", []b{{">", 1, `a{job="x and y"}`, false, true}, {">", 2, "b", false, false}}, "a{job=\"x and y\"} > 1 # or so\n and b > 7"},
		{"a > 1 AND IGNORING (instance) b > 2", []b{{">", 1, "a", false, true}, {">", 2, "b", false, false}}, "a > 1 AND IGNORING (instance) b > 7"},
		{"a > bool 1 and b > 2", []b{{">", 2, "b", false, false}}, "a > bool 1 and b > 7"},
		{"orders > 1 and ordered > 2", []b{{">", 1, "orders", false, true}, {">", 2, "ordered", false, false}}, "orders > 1 and ordered > 7"},
		{"sum by (job) (rate(a[5m]))\n  > 0.5\nand on (job)\n  sum by (job) (rate(b[5m])) > 100",
			[]b{{">", 0.5, "sum by (job) (rate(a[5m]))", false, true}, {">", 100, "sum by (job) (rate(b[5m]))", false, false}},
			"sum by (job) (rate(a[5m]))\n  > 0.5\nand on (job)\n  sum by (job) (rate(b[5m])) > 7"},
	} {
		r := Rule{Name: "R", Expr: c.expr}
		bs := r.Bounds()
		var got []b
		for i, x := range bs {
			if x.At != i {
				t.Errorf("%s: %d at %d", c.expr, i, x.At)
			}
			got = append(got, b{x.Op, x.Value, x.Of, x.Unless, x.Values})
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", c.expr, got, c.want)
			continue
		}
		if w := r.WithBound(bs[len(bs)-1], 7); w.Expr != c.with {
			t.Errorf("%s: with 7 %q, want %q", c.expr, w.Expr, c.with)
		}
	}
	// A long expression is cut short.
	long := Rule{Name: "R", Expr: "sum by (namespace, pod) (rate(container_cpu_usage_seconds_total[5m])) > 2 and up > 0.5"}
	if of := long.Bounds()[0].Of; of != "sum by (namespace, pod) (rate(container…" {
		t.Errorf("long: %q", of)
	}
}

func TestBoundGrafana(t *testing.T) {
	g := &Graph{Queries: map[string]string{"A": "latency"}, Cond: "C",
		Steps: map[string]Step{"C": {Threshold: &Threshold{Input: "A", Type: "gt", A: 0.9}}}}
	r := Rule{Name: "R", Graph: g, Digest: "d"}
	bs := r.Bounds()
	if len(bs) != 1 || bs[0] != (Bound{Op: ">", Value: 0.9, Values: true}) {
		t.Fatalf("threshold: %+v", bs)
	}
	if v, ok := r.ValueOf(bs[0]); !ok || v.Graph.Cond != "A" {
		t.Fatalf("threshold's value: %v %+v", ok, v.Graph)
	}
	w := r.WithBound(bs[0], 0.8)
	if w.Graph.Steps["C"].Threshold.A != 0.8 || g.Steps["C"].Threshold.A != 0.9 || w.Digest == r.Digest {
		t.Errorf("with 0.8: %+v, the rule's own %+v, digest %q", w.Graph.Steps["C"].Threshold, g.Steps["C"].Threshold, w.Digest)
	}
	// A number that isn't one.
	g.Steps["C"] = Step{Threshold: &Threshold{Input: "A", Type: "gt", A: math.NaN()}}
	if bs := r.Bounds(); len(bs) != 0 {
		t.Errorf("NaN is a bound: %+v", bs)
	}
	// A range has two numbers, each with the other end as it is.
	g.Steps["C"] = Step{Threshold: &Threshold{Input: "A", Type: "within_range", A: 10, B: 20}}
	bs = r.Bounds()
	if len(bs) != 2 || bs[0].Op != ">" || bs[0].Value != 10 || bs[0].Rest == nil || *bs[0].Rest != (Bound{Op: "<", Value: 20}) || bs[0].Either ||
		bs[1].Op != "<" || bs[1].Value != 20 || bs[1].At != 1 || *bs[1].Rest != (Bound{Op: ">", Value: 10}) {
		t.Fatalf("within: %+v", bs)
	}
	if !bs[0].Whole(8, 9) || bs[0].Whole(8, 21) || bs[0].Whole(10, 9) {
		t.Error("within, the lower end at 8")
	}
	if w := r.WithBound(bs[1], 24); w.Graph.Steps["C"].Threshold.B != 24 || w.Graph.Steps["C"].Threshold.A != 10 || w.Digest == r.WithBound(bs[0], 24).Digest {
		t.Errorf("within, the upper end at 24: %+v", w.Graph.Steps["C"].Threshold)
	}
	g.Steps["C"] = Step{Threshold: &Threshold{Input: "A", Type: "outside_range_included", A: 10, B: 20}}
	bs = r.Bounds()
	if len(bs) != 2 || bs[0].Op != "<=" || bs[1].Op != ">=" || !bs[0].Either || !bs[0].Whole(12, 25) || !bs[0].Whole(12, 11) || bs[0].Whole(12, 15) {
		t.Fatalf("outside: %+v", bs)
	}
	// One end of nothing: only the other.
	g.Steps["C"] = Step{Threshold: &Threshold{Input: "A", Type: "within_range", A: 0, B: 5}}
	if bs := slices.DeleteFunc(r.Bounds(), func(b Bound) bool { return b.Value == 0 }); len(bs) != 1 || bs[0].At != 1 {
		t.Errorf("from nothing: %+v", bs)
	}
	// A math expression comparing with a number.
	m, err := sse.ParseMath("80 >= $A")
	if err != nil {
		t.Fatal(err)
	}
	g.Steps["C"] = Step{Math: m}
	bs = r.Bounds()
	if len(bs) != 1 || bs[0] != (Bound{Op: "<=", Value: 80, Values: true}) {
		t.Fatalf("math: %+v", bs)
	}
	if v, ok := r.ValueOf(bs[0]); !ok || v.Graph.Steps["C"].Math == nil || len(v.Graph.Steps["C"].Math.Refs()) != 1 {
		t.Fatalf("math's value: %v", ok)
	}
	if bs := r.WithBound(bs[0], 90).Bounds(); bs[0].Value != 90 {
		t.Errorf("math with 90: %+v", bs)
	}
	if bs := r.Bounds(); bs[0].Value != 80 {
		t.Errorf("the rule's own changed: %+v", bs)
	}
}

func TestSteps(t *testing.T) {
	for _, c := range []struct {
		b           Bound
		first, last float64
		n           int
	}{
		{Bound{Op: ">", Value: 0.9}, 0.89, 0.72, 18},
		{Bound{Op: "<", Value: 100}, 110, 120, 2},
		{Bound{Op: "<", Value: 10, Unless: true}, 9, 8, 2}, // less of the exclusion
		{Bound{Op: ">", Value: 5, Unless: true}, 5.1, 6, 10},
		{Bound{Op: ">", Value: -3}, -3.1, -3.6, 6},
		{Bound{Op: ">=", Value: 0.955}, 0.95, 0.77, 19},
	} {
		s := c.b.Steps()
		if len(s) != c.n || s[0] != c.first || s[len(s)-1] != c.last || s[len(s)-1] != c.b.Furthest() {
			t.Errorf("%+v: %v", c.b, s)
		}
	}
}

func TestDurationsInRange(t *testing.T) {
	for _, doc := range []string{
		"groups: [{name: g, rules: [{alert: A, expr: x > 1, for: -5m}]}]",
		"groups: [{name: g, rules: [{alert: A, expr: x > 1, for: 99999999d}]}]",
		"groups: [{name: g, rules: [{alert: A, expr: x > 1, keep_firing_for: -1s}]}]",
		"groups: [{name: g, interval: -1m, rules: [{alert: A, expr: x > 1}]}]",
	} {
		if rs, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: read as %+v", doc, rs)
		}
	}
	// The rules API: nothing below nothing, nothing past a hundred years.
	rs, err := FromAPI([]byte(`{"status":"success","data":{"groups":[{"name":"g","interval":-60,"rules":[
		{"type":"alerting","name":"A","query":"x > 1","duration":-300,"keepFiringFor":1e300}]}]}}`))
	if err != nil || len(rs) != 1 || rs[0].For != 0 || rs[0].Interval != 0 || rs[0].KeepFiringFor != never {
		t.Errorf("from the API: %v %+v", err, rs)
	}
}

func TestBoundsEdges(t *testing.T) {
	// A comment between the operator and on(...).
	r := Rule{Name: "R", Expr: "a > 1 and # c\n on(job) b > 2"}
	if bs := r.Bounds(); len(bs) != 2 || bs[1].Of != "b" {
		t.Errorf("comment before on: %+v", bs)
	}
	// A number written up against a word keeps apart from it.
	r = Rule{Name: "R", Expr: "errors > 0.9 and+5 < delta"}
	bs := r.Bounds()
	if len(bs) != 2 {
		t.Fatalf("and+5: %+v", bs)
	}
	if got := r.WithBound(bs[1], 4).Expr; got != "errors > 0.9 and 4 < delta" {
		t.Errorf("and+5 at 4: %q", got)
	}
	// Deep, or long: read so far, and at once.
	deep := strings.Repeat("(", 20000) + "latency > 0.95" + strings.Repeat(")", 20000)
	began := time.Now()
	if bs := (Rule{Name: "R", Expr: deep}).Bounds(); len(bs) != 0 {
		t.Errorf("deep: %+v", bs)
	}
	if took := time.Since(began); took > 200*time.Millisecond {
		t.Errorf("deep took %s", took)
	}
	nested := strings.Repeat("(", 30) + "latency > 0.95" + strings.Repeat(")", 30)
	if bs := (Rule{Name: "R", Expr: nested}).Bounds(); len(bs) != 1 {
		t.Errorf("thirty deep: %+v", bs)
	}
	many := strings.Repeat("up > 1 and ", 1400) + "up > 1"
	began = time.Now()
	if bs := (Rule{Name: "R", Expr: many}).Bounds(); len(bs) != 1401 {
		t.Errorf("many: %d", len(bs))
	}
	if took := time.Since(began); took > 200*time.Millisecond {
		t.Errorf("many took %s", took)
	}
	if bs := (Rule{Name: "R", Expr: strings.Repeat("up > 1 and ", 2000) + "up > 1"}).Bounds(); len(bs) != 0 {
		t.Errorf("past 16 KiB: %d", len(bs))
	}
}

// A math expression joining comparisons with && and ||, and classic
// conditions: each number is a bound of its own, named by what it
// compares.
func TestBoundGrafanaJoined(t *testing.T) {
	m, err := sse.ParseMath("$A > 0.9 && (10 > $B || $C <= 3)")
	if err != nil {
		t.Fatal(err)
	}
	g := &Graph{Queries: map[string]string{"A": "a", "B": "b", "C": "c"}, Cond: "M", Steps: map[string]Step{"M": {Math: m}}}
	r := Rule{Name: "R", Graph: g, Digest: "d"}
	bs := r.Bounds()
	if len(bs) != 3 || bs[0] != (Bound{Op: ">", Value: 0.9, Of: "$A", Values: true}) ||
		bs[1] != (Bound{Op: "<", Value: 10, At: 1, Of: "$B", Values: true}) || bs[2].Of != "$C" || bs[2].At != 2 {
		t.Fatalf("joined: %+v", bs)
	}
	// The value of $B where the rest lets it decide: all three are read.
	v, ok := r.ValueOf(bs[1])
	if !ok || v.Graph.Steps["M"].Math == nil || !slices.Contains(v.Graph.Steps["M"].Math.Refs(), "C") || g.Steps["M"].Math != m {
		t.Fatalf("value of $B: %v %+v", ok, v.Graph)
	}
	w := r.WithBound(bs[1], 12)
	if got := sse.Format(w.Graph.Steps["M"].Math); got != "$A > 0.9 && ($B < 12 || $C <= 3)" || w.Digest == r.Digest {
		t.Errorf("with 12: %s", got)
	}
	if got := sse.Format(g.Steps["M"].Math); got != "$A > 0.9 && (10 > $B || $C <= 3)" {
		t.Errorf("the rule's own changed: %s", got)
	}

	c := &Classic{Conds: []ClassicCond{
		{Query: "A", Reducer: "avg", Type: "gt", Params: []float64{5}},
		{Query: "B", Reducer: "max", Type: "within_range", Params: []float64{1, 10}, Op: "or"},
		{Query: "A", Reducer: "last", Type: "eq", Params: []float64{3}, Op: "and"},
		{Query: "A", Reducer: "count", Type: "lte", Params: []float64{7}, Op: "and"},
		{Query: "A", Reducer: "diff", Type: "no_value", Op: "and"},
		{Query: "Q", Instant: true, Reducer: "sum", Type: "lt", Params: []float64{2}, Op: "and"},
	}}
	g = &Graph{Cond: "C", Steps: map[string]Step{"C": {Classic: c}}}
	r = Rule{Name: "R", Graph: g, Digest: "d"}
	bs = r.Bounds()
	want := []Bound{
		{Op: ">", Value: 5, Of: "avg(A)", Values: true},
		{Op: ">", Value: 1, At: 1, Of: "max(B)", Rest: &Bound{Op: "<", Value: 10}},
		{Op: "<", Value: 10, At: 2, Of: "max(B)", Rest: &Bound{Op: ">", Value: 1}},
		{Op: "<=", Value: 7, At: 4, Of: "count(A)", Values: true},
		{Op: "<", Value: 2, At: 5, Of: "Q", Values: true},
	}
	if len(bs) != len(want) {
		t.Fatalf("classic: %+v", bs)
	}
	for i := range bs {
		if bs[i].Op != want[i].Op || bs[i].Value != want[i].Value || bs[i].At != want[i].At || bs[i].Of != want[i].Of ||
			bs[i].Values != want[i].Values || (bs[i].Rest == nil) != (want[i].Rest == nil) {
			t.Errorf("bound %d: %+v, want %+v", i, bs[i], want[i])
		}
	}
	if v, ok := r.ValueOf(bs[3]); !ok || v.Graph.Steps["C"].Classic.Of != 4 || c.Of != 0 {
		t.Errorf("value of count(A): %v %+v", ok, v.Graph.Steps["C"].Classic)
	}
	w = r.WithBound(bs[2], 12)
	if p := w.Graph.Steps["C"].Classic.Conds[1].Params; p[0] != 1 || p[1] != 12 || c.Conds[1].Params[1] != 10 || w.Digest == r.Digest {
		t.Errorf("with 12: %v, the rule's own %v", p, c.Conds[1].Params)
	}
	if p := r.WithBound(bs[4], 1).Graph.Steps["C"].Classic.Conds[5].Params; p[0] != 1 {
		t.Errorf("the instant one with 1: %v", p)
	}
}
