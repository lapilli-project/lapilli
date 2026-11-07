package rules

import (
	"testing"

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
		{"(errors > 0.9)", "", "", 0, false, ""},           // inside brackets
		{"errors > bool 0.9", "", "", 0, false, ""},        // returns every series
		{"errors > 0.9 and on() up", "", "", 0, false, ""}, // a set operation
		{"errors > 0.9 unless maintenance", "", "", 0, false, ""},
		{"errors > other", "", "", 0, false, ""}, // no number
		{"0 < errors < 1", "", "", 0, false, ""}, // two comparisons
		{"errors == 1", "", "", 0, false, ""},    // not above or below
		{"errors > Inf", "", "", 0, false, ""},
		{`absent(up{job="a"})`, "", "", 0, false, ""},
		{`errors > 0.9 or vector(0)`, "", "", 0, false, ""},
	} {
		r := Rule{Name: "R", Expr: c.expr}
		b, v, ok := r.Bound()
		if ok != c.ok || ok && (b.Op != c.op || b.Value != c.n || v.Expr != c.value) {
			t.Errorf("%s: %v %+v %q, want %v %s %v %q", c.expr, ok, b, v.Expr, c.ok, c.op, c.n, c.value)
			continue
		}
		if got := r.WithBound(0.5); ok && got.Expr != c.with || !ok && got.Expr != c.expr {
			t.Errorf("%s: with 0.5 %q, want %q", c.expr, got.Expr, c.with)
		}
	}
}

func TestBoundGrafana(t *testing.T) {
	g := &Graph{Queries: map[string]string{"A": "latency"}, Cond: "C",
		Steps: map[string]Step{"C": {Threshold: &Threshold{Input: "A", Type: "gt", A: 0.9}}}}
	r := Rule{Name: "R", Graph: g, Digest: "d"}
	b, v, ok := r.Bound()
	if !ok || b != (Bound{">", 0.9}) || v.Graph.Cond != "A" {
		t.Fatalf("threshold: %v %+v %+v", ok, b, v.Graph)
	}
	w := r.WithBound(0.8)
	if w.Graph.Steps["C"].Threshold.A != 0.8 || g.Steps["C"].Threshold.A != 0.9 || w.Digest == r.Digest {
		t.Errorf("with 0.8: %+v, the rule's own %+v, digest %q", w.Graph.Steps["C"].Threshold, g.Steps["C"].Threshold, w.Digest)
	}
	// A range has two numbers.
	g.Steps["C"] = Step{Threshold: &Threshold{Input: "A", Type: "within_range", A: 1, B: 2}}
	if _, _, ok := r.Bound(); ok {
		t.Error("a range has a bound")
	}
	// A math expression comparing with a number.
	m, err := sse.ParseMath("80 >= $A")
	if err != nil {
		t.Fatal(err)
	}
	g.Steps["C"] = Step{Math: m}
	b, v, ok = r.Bound()
	if !ok || b != (Bound{"<=", 80}) || v.Graph.Steps["C"].Math == nil || len(v.Graph.Steps["C"].Math.Refs()) != 1 {
		t.Fatalf("math: %v %+v", ok, b)
	}
	if b, _, _ := r.WithBound(90).Bound(); b != (Bound{"<=", 90}) {
		t.Errorf("math with 90: %+v", b)
	}
	if b, _, _ := r.Bound(); b.Value != 80 {
		t.Errorf("the rule's own changed: %+v", b)
	}
}
