package rules

import (
	"math"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// Grafana 13.3's own reducers (pkg/expr/classic/reduce.go): the diff
// reducers take the newest number less the oldest; one number alone, as
// the first, differs by nothing.
func TestClassicReduce(t *testing.T) {
	n, o := Reduced{150, true}, Reduced{50, true}
	for red, want := range map[string]float64{"diff": 100, "diff_abs": 100, "percent_diff": 200, "percent_diff_abs": 200} {
		if got := (ClassicCond{Reducer: red}).Reduce(n, o, false); !got.OK || got.V != want {
			t.Errorf("%s(newest 150, oldest 50) = %+v, want %v", red, got, want)
		}
	}
	down := ClassicCond{Reducer: "percent_diff"}.Reduce(Reduced{50, true}, Reduced{-100, true}, false)
	if down.V != 150 { // (50 - -100) / |-100| * 100
		t.Errorf("percent_diff over a negative oldest = %v", down.V)
	}
	if got := (ClassicCond{Reducer: "percent_diff_abs"}).Reduce(Reduced{50, true}, Reduced{-100, true}, false); got.V != 150 {
		t.Errorf("percent_diff_abs = %v", got.V)
	}
	if got := (ClassicCond{Reducer: "diff"}).Reduce(Reduced{7, true}, Reduced{7, true}, false); got.V != 0 || !got.OK {
		t.Errorf("one number alone: %+v", got)
	}
	// One number alone, 0, as Grafana's calculateDiff leaves it: 0, where
	// two numbers of 0 divide by nothing.
	for _, red := range []string{"percent_diff", "percent_diff_abs"} {
		if got := (ClassicCond{Reducer: red}).Reduce(Reduced{0, true}, Reduced{0, true}, true); got.V != 0 || !got.OK {
			t.Errorf("%s of one number, 0: %+v", red, got)
		}
		if got := (ClassicCond{Reducer: red}).Reduce(Reduced{0, true}, Reduced{0, true}, false); !math.IsNaN(got.V) {
			t.Errorf("%s of two numbers, 0: %+v", red, got)
		}
	}
	if got := (ClassicCond{Reducer: "diff"}).Reduce(Reduced{}, Reduced{7, true}, false); got.OK {
		t.Errorf("no newest number is no value: %+v", got)
	}
	if got := (ClassicCond{Reducer: "diff_abs"}).Reduce(Reduced{7, true}, Reduced{}, false); got.OK {
		t.Errorf("no oldest number found is no value: %+v", got)
	}
	if got := (ClassicCond{Reducer: "percent_diff"}).Reduce(Reduced{1, true}, Reduced{0, true}, false); !math.IsInf(got.V, 1) {
		t.Errorf("percent_diff from 0 = %v, want +Inf as Grafana divides", got.V)
	}
	if got := (ClassicCond{Reducer: "avg"}).Reduce(Reduced{3, true}, Reduced{}, false); got.V != 3 {
		t.Errorf("a reducer asked of the server is its value: %+v", got)
	}
	if got := (ClassicCond{Reducer: "diff", Instant: true}).Reduce(Reduced{3, true}, Reduced{}, false); got.V != 3 || !got.OK {
		t.Errorf("an instant query's value is compared as it is: %+v", got)
	}
}

// Grafana 13.3's evaluators (pkg/expr/classic/evaluator.go).
func TestClassicPass(t *testing.T) {
	nan := math.NaN()
	for _, c := range []struct {
		typ    string
		params []float64
		v      float64
		ok     bool
		want   bool
	}{
		{"gt", []float64{5}, 5, true, false}, {"gt", []float64{5}, 6, true, true},
		{"gte", []float64{5}, 5, true, true}, {"lt", []float64{5}, 5, true, false},
		{"lte", []float64{5}, 5, true, true}, {"eq", []float64{5}, 5, true, true},
		{"ne", []float64{5}, 5, true, false}, {"ne", []float64{5}, nan, true, true},
		{"gt", []float64{5}, nan, true, false}, {"gt", []float64{5}, 0, false, false},
		{"within_range", []float64{1, 3}, 2, true, true}, {"within_range", []float64{3, 1}, 2, true, true},
		{"within_range", []float64{1, 3}, 3, true, false}, {"within_range_included", []float64{1, 3}, 3, true, true},
		{"within_range_included", []float64{3, 1}, 1, true, true},
		{"outside_range", []float64{1, 3}, 0, true, true}, {"outside_range", []float64{3, 1}, 4, true, true},
		{"outside_range", []float64{1, 3}, 3, true, false}, {"outside_range_included", []float64{1, 3}, 3, true, true},
		{"outside_range", []float64{1, 3}, nan, true, false},
		{"no_value", nil, 0, false, true}, {"no_value", nil, nan, true, false}, {"no_value", nil, 1, true, false},
	} {
		cd := ClassicCond{Type: c.typ, Params: c.params}
		if got := cd.Pass(Reduced{c.v, c.ok}); got != c.want {
			t.Errorf("%s %v on %v (%v): %v, want %v", c.typ, c.params, c.v, c.ok, got, c.want)
		}
	}
}

// Grafana 13.3's ConditionsCmd (pkg/expr/classic/classic.go): no data comes
// first; a condition is no data when every series reduces to none, or it
// has none; logic-or stops once those before hold.
func TestClassicEval(t *testing.T) {
	gt := func(op string) ClassicCond { return ClassicCond{Type: "gt", Params: []float64{5}, Op: op} }
	some := []Reduced{{1, true}, {9, true}}
	none := []Reduced{{0, false}, {0, false}}
	for _, c := range []struct {
		name         string
		conds        []ClassicCond
		series       [][]Reduced
		firing, noDt bool
	}{
		{"any series passes", []ClassicCond{gt("")}, [][]Reduced{some}, true, false},
		{"one with none, one passing", []ClassicCond{gt("")}, [][]Reduced{{{0, false}, {9, true}}}, true, false},
		{"all none is no data", []ClassicCond{gt("")}, [][]Reduced{none}, false, true},
		{"no series is no data", []ClassicCond{gt("")}, [][]Reduced{nil}, false, true},
		{"no_value with no series holds", []ClassicCond{{Type: "no_value"}}, [][]Reduced{nil}, true, false},
		{"no_value with a value doesn't", []ClassicCond{{Type: "no_value"}}, [][]Reduced{some}, false, false},
		{"and", []ClassicCond{gt(""), gt("and")}, [][]Reduced{some, {{1, true}}}, false, false},
		{"or", []ClassicCond{gt(""), gt("or")}, [][]Reduced{{{1, true}}, some}, true, false},
		// No data comes before firing, even when it fired.
		{"or with no data", []ClassicCond{gt(""), gt("or")}, [][]Reduced{some, none}, false, true},
		// logic-or leaves out what follows once it holds, its no data too.
		{"logic-or", []ClassicCond{gt(""), gt("logic-or")}, [][]Reduced{some, none}, true, false},
		{"logic-or not yet", []ClassicCond{gt(""), gt("logic-or")}, [][]Reduced{{{1, true}}, none}, false, true},
		// In order, with no precedence: false or true and true is true.
		{"in order", []ClassicCond{gt(""), gt("or"), gt("and")}, [][]Reduced{{{1, true}}, some, some}, true, false},
		{"and with no data", []ClassicCond{gt(""), gt("and")}, [][]Reduced{some, none}, false, false},
	} {
		cl := &Classic{Conds: c.conds}
		f, nd := cl.Eval(func(i int) []Reduced { return c.series[i] })
		if f != c.firing || nd != c.noDt {
			t.Errorf("%s: firing %v no data %v, want %v %v", c.name, f, nd, c.firing, c.noDt)
		}
	}
}

func TestClassicValueOf(t *testing.T) {
	above := ClassicCond{Type: "gt", Params: []float64{100}}
	below := ClassicCond{Type: "lt", Params: []float64{5}, Op: "and"}
	cl := &Classic{Conds: []ClassicCond{above, below}}
	series := func(a, b []Reduced) func(int) []Reduced {
		return func(i int) []Reduced {
			if i == 0 {
				return a
			}
			return b
		}
	}
	// The largest of the series, where the other condition holds.
	if v := cl.ValueOf(0, series([]Reduced{{80, true}, {95, true}, {0, false}}, []Reduced{{1, true}})); v != 95 {
		t.Errorf("value = %v, want 95", v)
	}
	// Where the other doesn't hold, no number would make it.
	if v := cl.ValueOf(0, series([]Reduced{{95, true}}, []Reduced{{9, true}})); !math.IsNaN(v) {
		t.Errorf("value = %v, want NaN", v)
	}
	// The smallest, for one below.
	if v := cl.ValueOf(1, series([]Reduced{{120, true}}, []Reduced{{7, true}, {6, true}})); v != 6 {
		t.Errorf("value = %v, want 6", v)
	}
	// Holding either way: or with one that holds.
	or := &Classic{Conds: []ClassicCond{above, {Type: "lt", Params: []float64{5}, Op: "or"}}}
	if v := or.ValueOf(0, series([]Reduced{{80, true}}, []Reduced{{1, true}})); !math.IsInf(v, 1) {
		t.Errorf("value = %v, want +Inf", v)
	}
	if v := cl.ValueOf(0, series([]Reduced{{0, false}}, []Reduced{{1, true}})); !math.IsNaN(v) {
		t.Errorf("no value to compare: %v", v)
	}
}

func TestClassicParse(t *testing.T) {
	parse := func(conds string, model string) (*Graph, error) {
		var rm map[string]any
		body := "condition: C\ndata:\n  - {refId: A, datasourceUid: prom, relativeTimeRange: {from: 300, to: 60}, model: " + model + "}\n" +
			"  - refId: C\n    datasourceUid: __expr__\n    model:\n      type: classic_conditions\n      conditions:\n" + conds
		if err := yaml.Unmarshal([]byte(body), &rm); err != nil {
			t.Fatal(err)
		}
		return grafanaGraph(rm, nil, nil)
	}
	cond := func(red, ev, params, op string) string {
		return "        - {evaluator: {type: " + ev + ", params: " + params + "}, operator: {type: " + op + "}, query: {params: [A]}, reducer: {type: " + red + "}}\n"
	}
	g, err := parse(cond("count_non_null", "lte", "[3]", "and")+cond("median", "outside_range_included", "[1, 2]", "logic-or"), "{expr: x, intervalMs: 1000}")
	if err != nil {
		t.Fatal(err)
	}
	cs := g.Steps["C"].Classic.Conds
	if g.Asks[cs[0].Value].PromQL(0) != "count_over_time(((x) >= -Inf)[4m500ms:1s])" || g.Asks[cs[1].Value].PromQL(15*time.Second) != "quantile_over_time(0.5, ((x) >= -Inf)[4m7s500ms:15s])" {
		t.Errorf("asks %v", g.Asks)
	}
	// The window ends a minute before each evaluation.
	if g.Shift[cs[0].Value] != time.Minute || g.Shift["A"] != time.Minute {
		t.Errorf("shift %v", g.Shift)
	}
	inst, err := parse(cond("diff", "no_value", "[]", "and"), "{expr: x, instant: true}")
	if err != nil {
		t.Fatal(err)
	}
	if c := inst.Steps["C"].Classic.Conds[0]; !c.Instant || c.Value != "A" || c.Present != "A" || inst.Data["A"].Present != "A" {
		t.Errorf("instant %+v %+v", c, inst.Data)
	}
	for conds, want := range map[string]string{
		cond("mode", "gt", "[1]", "and"):                                  `reducer "mode"`,
		cond("avg", "between", "[1]", "and"):                              `evaluator "between"`,
		cond("avg", "within_range", "[1]", "and"):                         "needs 2 values",
		cond("avg", "gt", "[]", "and"):                                    "needs a value",
		cond("avg", "gt", "[1]", "and") + cond("avg", "gt", "[1]", "xor"): `operator "xor"`,
		cond("avg", "gt", "[a]", "and"):                                   "value a",
	} {
		if _, err := parse(conds, "{expr: x}"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", conds, err, want)
		}
	}
}

// How far apart Grafana 13.3 asks for a range query's points, as measured
// through its /api/v1/eval.
func TestResolutionOf(t *testing.T) {
	for _, c := range []struct {
		model map[string]any
		span  time.Duration
		want  time.Duration
	}{
		{map[string]any{"intervalMs": 1000}, 10 * time.Minute, time.Second},
		{map[string]any{}, 5 * time.Minute, time.Second},
		{map[string]any{"intervalMs": 1000}, time.Hour, time.Second},
		{map[string]any{"intervalMs": 1000}, 3 * time.Hour, time.Second},
		{map[string]any{"intervalMs": 1000}, 6 * time.Hour, 2 * time.Second},
		{map[string]any{"intervalMs": 1000}, 12 * time.Hour, 5 * time.Second},
		{map[string]any{"intervalMs": 1000}, 24 * time.Hour, 10 * time.Second},
		{map[string]any{"intervalMs": 1000, "maxDataPoints": 100}, 10 * time.Minute, 5 * time.Second},
		{map[string]any{"intervalMs": 15000}, 10 * time.Minute, 15 * time.Second},
		{map[string]any{"intervalMs": 500}, 10 * time.Minute, 500 * time.Millisecond},
		{map[string]any{"intervalMs": 1000, "interval": "30s"}, 4 * time.Minute, 30 * time.Second},
	} {
		if got := resolutionOf(c.model, c.span, 0); got != c.want {
			t.Errorf("%v over %s: %s, want %s", c.model, c.span, got, c.want)
		}
	}
}

func TestGrafanaStates(t *testing.T) {
	read := func(extra string) Rule {
		rs, err := Parse([]byte(`apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    rules:
      - uid: s1
        title: States
        condition: A
` + extra + `
        data:
          - {refId: A, datasourceUid: prom, model: {expr: x, instant: true}}
`))
		if err != nil || len(rs) != 1 {
			t.Fatal(rs, err)
		}
		return rs[0]
	}
	// Grafana's provisioning defaults.
	if g := read("").Graph; g.NoData != StateNoData || g.ExecErr != StateAlerting || g.MissingToResolve != 2 {
		t.Errorf("defaults %s %s %d", g.NoData, g.ExecErr, g.MissingToResolve)
	}
	g := read("        noDataState: KeepLast\n        execErrState: OK\n        missing_series_evals_to_resolve: 5").Graph
	if g.NoData != StateKeepLast || g.ExecErr != StateOK || g.MissingToResolve != 5 {
		t.Errorf("read %s %s %d", g.NoData, g.ExecErr, g.MissingToResolve)
	}
	for _, bad := range []string{"        noDataState: Pending", "        execErrState: NoData", "        missing_series_evals_to_resolve: -1"} {
		if r := read(bad); r.Graph != nil || r.Unsupported == "" {
			t.Errorf("%s: %+v", bad, r)
		}
	}
}

func TestQueryOffset(t *testing.T) {
	rs, err := Parse([]byte("groups:\n  - name: g\n    query_offset: 1m\n    rules:\n      - {alert: A, expr: up == 0}\n  - name: h\n    rules:\n      - {alert: A, expr: up == 0}\n"))
	if err != nil || len(rs) != 2 {
		t.Fatal(rs, err)
	}
	if rs[0].QueryOffset != time.Minute || rs[1].QueryOffset != 0 {
		t.Errorf("query_offset %s %s", rs[0].QueryOffset, rs[1].QueryOffset)
	}
	plain := rs[1]
	plain.Group = "g"
	if rs[0].Key() == plain.Key() || strings.Contains(plain.Key(), "offset") {
		t.Errorf("a query_offset decides when a rule fires: %s / %s", rs[0].Key(), plain.Key())
	}
	plain.Late = 5 * time.Minute
	if plain.Lag() != 5*time.Minute || rs[0].Lag() != time.Minute {
		t.Errorf("lag %s %s", plain.Lag(), rs[0].Lag())
	}
}

func TestLate(t *testing.T) {
	var ls Lateness
	for _, v := range []string{"cloud=5m", "^Disk.*=2m", "1m"} {
		l, err := ParseLate(v)
		if err != nil {
			t.Fatal(err)
		}
		ls = append(ls, l)
	}
	for _, c := range []struct {
		r    Rule
		want time.Duration
	}{
		{Rule{Name: "CPU", Cloud: "cloudwatch"}, 5 * time.Minute}, // by the cloud it came from
		{Rule{Name: "CPU", Source: "/rules/cloud/x.yaml"}, 5 * time.Minute},
		{Rule{Name: "DiskFull"}, 2 * time.Minute},
		{Rule{Name: "CPU"}, time.Minute}, // none matches: the one with no pattern
	} {
		if got := ls.Of(c.r); got != c.want {
			t.Errorf("%+v: %s, want %s", c.r, got, c.want)
		}
	}
	// The first that matches counts, a bare one after a pattern too.
	first := Lateness{{D: time.Minute}, ls[1]}
	if got := first.Of(Rule{Name: "DiskFull"}); got != 2*time.Minute {
		t.Errorf("a pattern after a bare one: %s", got)
	}
	if got := (Lateness{ls[1]}).Of(Rule{Name: "CPU"}); got != 0 {
		t.Errorf("none matching, none bare: %s", got)
	}
	rs := []Rule{{Name: "DiskFull"}, {Name: "CPU"}}
	Replaying{Late: ls, Scrape: 15 * time.Second}.Apply(rs)
	if rs[0].Late != 2*time.Minute || rs[1].Late != time.Minute || rs[0].Scrape != 15*time.Second {
		t.Errorf("applied: %+v", rs)
	}
	for _, bad := range []string{"", "0s", "-1m", "25h", "x=", "(=1m", "soon"} {
		if _, err := ParseLate(bad); err == nil {
			t.Errorf("--late %q taken", bad)
		}
	}
}

func TestUnseen(t *testing.T) {
	// Nearly every Grafana rule fires on errors: not a mark of each.
	if u := (Rule{Graph: &Graph{ExecErr: StateAlerting}}).Unseen(); len(u) != 0 {
		t.Errorf("error state: %v", u)
	}
	if u := (Rule{Expr: "x", Cloud: "google"}).Unseen(); len(u) != 1 || u[0] != UnseenLate {
		t.Errorf("a cloud's: %v", u)
	}
	if u := (Rule{Expr: "x", Cloud: "google", Late: time.Minute}).Unseen(); len(u) != 0 {
		t.Errorf("with --late: %v", u)
	}
}

// A group's place in its interval from its last evaluation, in whole
// seconds, whenever that is: past 2262 too.
func TestPhaseFarAhead(t *testing.T) {
	for at, want := range map[string]time.Duration{
		"2026-11-11T12:40:30.9Z": 30 * time.Second,
		"7000-01-01T00:00:07Z":   7 * time.Second,
	} {
		rs, err := FromAPI([]byte(`{"status":"success","data":{"groups":[{"name":"g","interval":60,"lastEvaluation":"` + at + `",
		  "rules":[{"type":"alerting","name":"A","query":"up == 0"}]}]}}`))
		if err != nil || len(rs) != 1 || rs[0].Phase != want {
			t.Errorf("%s: %+v %v", at, rs, err)
		}
	}
}
