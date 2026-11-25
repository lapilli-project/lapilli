package rules

import (
	"math"
	"strings"
	"testing"
	"time"
)

// TestClassicReduceAsGrafana holds ClassicReduce to Grafana's own tests of its
// classic reducers (pkg/expr/classic/reduce_test.go, Grafana 13.3.1:
// TestReducer, TestDiffReducer, TestDiffAbsReducer, TestPercentDiffReducer,
// TestPercentDiffAbsReducer), a null point being a NaN here, and to what
// reduce.go does where those tests say nothing.
func TestClassicReduceAsGrafana(t *testing.T) {
	n := math.NaN()
	inf := math.Inf(1)
	none := math.NaN() // the expected value of a series that reduces to none
	for _, c := range []struct {
		name string
		fn   string
		in   []float64
		want float64 // none for none
	}{
		// reduce_test.go, TestReducer
		{"sum", "sum", []float64{1, 2, 3}, 6},
		{"min", "min", []float64{3, 2, 1}, 1},
		{"min of NaNs only", "min", []float64{n, n, n}, none},
		{"max", "max", []float64{1, 2, 3}, 3},
		{"count", "count", []float64{1, 2, 3000}, 3},
		{"last", "last", []float64{1, 2, 3000}, 3000},
		{"median odd", "median", []float64{1, 2, 3000}, 2},
		{"median even", "median", []float64{1, 2, 4, 3000}, 3},
		{"median one", "median", []float64{1}, 1},
		{"median ignores null", "median", []float64{n, n, n, 1, 2, 3}, 2},
		{"avg", "avg", []float64{1, 2, 3}, 2},
		{"avg of nulls only", "avg", []float64{n}, none},
		{"avg ignores nulls", "avg", []float64{3, n, n, 3}, 3},
		{"count_non_null mixed", "count_non_null", []float64{n, n, 3, 4}, 2},
		{"count_non_null none", "count_non_null", []float64{n, n}, none},
		// TestDiffReducer, TestDiffAbsReducer, TestPercentDiffReducer,
		// TestPercentDiffAbsReducer
		{"diff of one point", "diff", []float64{30}, 0},
		{"diff of one negative point", "diff", []float64{-30}, 0},
		{"diff two", "diff", []float64{30, 40}, 10},
		{"diff two down", "diff", []float64{30, 20}, -10},
		{"diff across zero", "diff", []float64{30, -40}, -70},
		{"diff three", "diff", []float64{30, 40, 50}, 20},
		{"diff nulls only", "diff", []float64{n, n}, none},
		{"diff_abs of one", "diff_abs", []float64{-30}, 0},
		{"diff_abs down", "diff_abs", []float64{30, 20}, 10},
		{"diff_abs across zero", "diff_abs", []float64{-30, 40}, 70},
		{"diff_abs nulls only", "diff_abs", []float64{n, n}, none},
		{"percent_diff of one", "percent_diff", []float64{30}, 0},
		{"percent_diff up", "percent_diff", []float64{30, 40}, 33.33333333333333},
		{"percent_diff down", "percent_diff", []float64{30, 20}, -33.33333333333333},
		{"percent_diff negatives", "percent_diff", []float64{-30, -40}, -33.33333333333333},
		{"percent_diff negatives up", "percent_diff", []float64{-30, -10}, 66.66666666666666},
		{"percent_diff across zero", "percent_diff", []float64{30, -40}, -233.33333333333334},
		{"percent_diff three", "percent_diff", []float64{30, 40, 50}, 66.66666666666666},
		{"percent_diff nulls only", "percent_diff", []float64{n, n}, none},
		{"percent_diff_abs down", "percent_diff_abs", []float64{30, 20}, 33.33333333333333},
		{"percent_diff_abs negatives", "percent_diff_abs", []float64{-30, -40}, 33.33333333333333},
		{"percent_diff_abs across zero", "percent_diff_abs", []float64{-30, 40}, 233.33333333333334},
		{"percent_diff_abs nulls only", "percent_diff_abs", []float64{n, n}, none},
		// reduce.go where those say nothing.
		{"no points at all", "avg", nil, none},
		{"count of NaN, which is a point", "count", []float64{n, n}, 2},
		{"avg of the numbers, NaN left out of the divisor", "avg", []float64{n, 4, n, 8}, 6},
		{"sum of the numbers", "sum", []float64{n, 4, n, 8}, 12},
		{"last is the newest number", "last", []float64{4, 8, n, n}, 8},
		{"last of NaNs only", "last", []float64{n, n}, none},
		{"max starts from -MaxFloat64", "max", []float64{math.Inf(-1)}, -math.MaxFloat64},
		{"min starts from MaxFloat64", "min", []float64{inf}, math.MaxFloat64},
		{"min of numbers past NaN", "min", []float64{n, 5, n, 2}, 2},
		{"median of NaNs only", "median", []float64{n}, none},
		{"count_non_null counts numbers only", "count_non_null", []float64{1, n, 2, inf}, 3},
		// calculateDiff: the newest number against the oldest number; one
		// that is the first point alone is 0; a newest number that isn't the
		// first point, with NaN before it, is compared with itself.
		{"diff skips leading NaN", "diff", []float64{n, 10, 15}, 5},
		{"diff skips trailing NaN", "diff", []float64{10, 15, n}, 5},
		{"diff of the first point alone, NaN after", "diff", []float64{7, n, n}, 0},
		{"diff of a number alone after NaN", "diff", []float64{n, 7}, 0},
		{"percent_diff of 0 alone after NaN is NaN", "percent_diff", []float64{n, 0}, n},
		{"percent_diff of 0 alone first is 0", "percent_diff", []float64{0, n}, 0},
		{"percent_diff from 0 is Inf", "percent_diff", []float64{0, 5}, inf},
		{"percent_diff_abs of 0 to 0", "percent_diff_abs", []float64{0, 0}, n},
		{"present counts points", ClassicPresent, []float64{n, n, 1}, 3},
	} {
		got, ok := ClassicReduce(c.fn, c.in)
		if math.IsNaN(c.want) && c.name != "percent_diff of 0 alone after NaN is NaN" && c.name != "percent_diff_abs of 0 to 0" {
			if ok {
				t.Errorf("%s: %g, want none", c.name, got)
			}
			continue
		}
		if !ok || got != c.want && !(math.IsNaN(got) && math.IsNaN(c.want)) {
			t.Errorf("%s: %g (%v), want %g", c.name, got, ok, c.want)
		}
	}
}

// TestClassicLokiRead: a classic condition on a Loki query reads, each
// condition with two asks of the one range query, its reducer's and which
// series there are; none of Prometheus's subqueries.
func TestClassicLokiRead(t *testing.T) {
	doc := strings.Replace(strings.Replace(lokiGrafana, `model: {type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [10]}}]}`,
		`model: {type: classic_conditions, conditions: [{evaluator: {type: gt, params: [10]}, operator: {type: and}, query: {params: [A]}, reducer: {type: percent_diff}}]}`, 1),
		`queryType: instant}`, `queryType: range}`, 1)
	rs, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	Replaying{Kinds: map[string]string{"lk": KindLoki}}.Apply(rs)
	if rs[0].Unsupported != "" || rs[0].Logs != "lk" {
		t.Fatalf("%q %q", rs[0].Unsupported, rs[0].Logs)
	}
	g := rs[0].Graph
	var cd ClassicCond
	for _, st := range g.Steps {
		if st.Classic != nil {
			cd = st.Classic.Conds[0]
		}
	}
	v, p := g.Asks[cd.Value], g.Asks[cd.Present]
	if !cd.Reduced || cd.Diff() || cd.Oldest != "" || cd.Points != "" || !v.Logs || !v.Classic || v.Fn != "percent_diff" || !p.Logs || p.Fn != ClassicPresent || v.Expr != p.Expr || v.Res != time.Second {
		t.Errorf("%+v value %+v present %+v", cd, v, p)
	}
	// The data source query returned something when a series has a point.
	if g.Data["A"].Present != cd.Present {
		t.Errorf("present %q, want %q", g.Data["A"].Present, cd.Present)
	}
	// Reduced as Grafana's: the value as it is, the diff worked out.
	if got := cd.Reduce(Reduced{V: 12, OK: true}, Reduced{}, false); got != (Reduced{12, true}) {
		t.Errorf("reduce %+v", got)
	}
	// A step finer than the replay asks isn't read.
	fine := strings.Replace(doc, "intervalMs: 1000", "intervalMs: 5", 1)
	rs, _ = Parse([]byte(fine))
	Replaying{Kinds: map[string]string{"lk": KindLoki}}.Apply(rs)
	if !strings.Contains(rs[0].Unsupported, "a replay asks no finer than") {
		t.Errorf("a step of 5ms: %q", rs[0].Unsupported)
	}
}
