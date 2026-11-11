package rules

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"
)

// A dataQuery is a Grafana rule's data source query, as the replay asks it.
type dataQuery struct {
	expr string
	// window is a range query's: from less to; 0 for an instant query.
	window time.Duration
	// res is how far apart Grafana asks for its points.
	res time.Duration
	// to is how long before the evaluation its range ends.
	to time.Duration
}

// atRes is the most of Grafana's points a reduce is asked for at its
// resolution when its result doesn't depend on how many there are: ten
// minutes at Grafana's 1s.
const atRes = 600

// subquery is the range a reduce over the query reads, as PromQL: at
// Grafana's resolution, as Grafana reads it, when the reduce counts or
// sums points, or the window holds no more than atRes of them; otherwise
// at the server's own, to keep what the server works out to a bound. At
// Grafana's, the range reaches half a point further back than the window,
// so it holds the window's points with both ends, as Grafana's range query
// does, whether the server's subqueries leave out their start (Prometheus
// 3) or not (Prometheus 2).
func (q dataQuery) subquery(counts bool) string {
	if !counts && q.window > atRes*q.res {
		return "[" + promDuration(q.window) + ":]"
	}
	return "[" + promDuration(q.window+q.res/2) + ":" + promDuration(q.res) + "]"
}

// resolutionOf is how far apart Grafana asks a Prometheus data source for a
// range query's points over a range: the query's interval, else its
// intervalMs, 1s when it has neither; or more, for a long range, to keep
// within its maxDataPoints (43200 when not set) and Prometheus's 11,000
// points, each rounded as Grafana rounds them. Checked against Grafana
// 13.3: 1s for 10m, 2s for 6h, 10s for a day.
func resolutionOf(model map[string]any, span time.Duration) time.Duration {
	least := time.Second
	if ms, ok := toFloat(model["intervalMs"]); ok && ms >= 1 && ms <= 1e9 {
		least = time.Duration(ms) * time.Millisecond
	}
	if s := str(model["interval"]); s != "" && s != "0s" {
		if d, err := duration(s); err == nil && d > 0 {
			least = d
		}
	}
	maxDP := 43200.0
	if n, ok := toFloat(model["maxDataPoints"]); ok && n >= 1 {
		maxDP = n
	}
	res := max(least, roundInterval(time.Duration(float64(span)/maxDP)))
	return max(res, roundInterval(span/11000))
}

// roundInterval rounds a duration to the one Grafana would use.
func roundInterval(d time.Duration) time.Duration {
	ms := float64(d) / float64(time.Millisecond)
	for _, r := range []struct {
		upTo float64
		to   time.Duration
	}{
		{15, 10 * time.Millisecond}, {35, 20 * time.Millisecond}, {75, 50 * time.Millisecond},
		{150, 100 * time.Millisecond}, {350, 200 * time.Millisecond}, {750, 500 * time.Millisecond},
		{1500, time.Second}, {3500, 2 * time.Second}, {7500, 5 * time.Second}, {12500, 10 * time.Second},
		{17500, 15 * time.Second}, {25000, 20 * time.Second}, {45000, 30 * time.Second}, {90000, time.Minute},
		{210000, 2 * time.Minute}, {450000, 5 * time.Minute}, {750000, 10 * time.Minute}, {1050000, 15 * time.Minute},
		{1500000, 20 * time.Minute}, {2700000, 30 * time.Minute}, {5400000, time.Hour}, {9000000, 2 * time.Hour},
		{16200000, 3 * time.Hour}, {32400000, 6 * time.Hour}, {86400000, 12 * time.Hour},
		{604800000, 24 * time.Hour}, {1814400000, 7 * 24 * time.Hour}, {3628800000, 30 * 24 * time.Hour},
	} {
		if ms <= r.upTo {
			return r.to
		}
	}
	return 365 * 24 * time.Hour
}

// Classic is a Grafana rule's classic conditions: each reduces every series
// of its query to one value and compares it, and holds when any series
// passes; they join in order, each with its own and, or or logic-or, with
// no precedence. Like Grafana's, the result has no labels: one alert.
type Classic struct {
	Conds []ClassicCond
	// Of, when not 0, says the step gives, in place of the outcome, the
	// value condition Of-1 compares, as ValueOf has it: for near misses
	// by value.
	Of int
}

// ClassicCond is one classic condition.
type ClassicCond struct {
	Query string // the refId of the query it reduces
	// Reducer is avg, sum, min, max, count, last, median, diff, diff_abs,
	// percent_diff, percent_diff_abs or count_non_null; Grafana passes it
	// over for an instant query, whose value is compared as it is.
	Reducer string
	// Type is the evaluator: gt, lt, gte, lte, eq, ne, within_range,
	// outside_range, within_range_included, outside_range_included, or
	// no_value; Params are its numbers.
	Type   string
	Params []float64
	// Op joins it to those before it: and, or, or logic-or, which leaves
	// the rest out once they hold. The first's is passed over.
	Op      string
	Instant bool
	// Present, Value and Oldest are refs of the graph's Queries or Asks:
	// a series for each the query returned; each one's reduced value (none
	// for one with no number to reduce); and, for the diff reducers, the
	// newest number in Value, each one's number at the window's start.
	Present, Value, Oldest string
	// Expr, Window and Res are the query, its window and Grafana's
	// resolution, to ask for a window's points when a series has none at
	// its start, for the diff reducers.
	Expr        string
	Window, Res time.Duration
}

// classicEvaluators are how many numbers each evaluator takes.
var classicEvaluators = map[string]int{
	"gt": 1, "lt": 1, "gte": 1, "lte": 1, "eq": 1, "ne": 1,
	"within_range": 2, "outside_range": 2, "within_range_included": 2, "outside_range_included": 2,
	"no_value": 0,
}

var classicReducers = []string{"avg", "sum", "min", "max", "count", "last", "median",
	"diff", "diff_abs", "percent_diff", "percent_diff_abs", "count_non_null"}

// Diff reports whether the reducer compares a window's newest number with
// its oldest.
func (c ClassicCond) Diff() bool {
	switch c.Reducer {
	case "diff", "diff_abs", "percent_diff", "percent_diff_abs":
		return true
	}
	return false
}

// classic reads Grafana's classic conditions, and adds to g what the replay
// asks for each: the query, for an instant one; for a range query, each
// series' reduction over the window, at the server's resolution, or at
// Grafana's for a reducer that counts or sums points, with numbers that
// aren't (NaN) left out where Grafana leaves them out.
func classic(ref string, m map[string]any, queries map[string]dataQuery, g *Graph) (*Classic, error) {
	c := &Classic{}
	for i, x := range list(m["conditions"]) {
		cm, _ := x.(map[string]any)
		qm, _ := cm["query"].(map[string]any)
		params := list(qm["params"])
		if len(params) == 0 || str(params[0]) == "" {
			return nil, errors.New("classic condition without a query")
		}
		cd := ClassicCond{Query: str(params[0])}
		q, ok := queries[cd.Query]
		if !ok {
			return nil, fmt.Errorf("classic condition on %q, which the rule doesn't have", cd.Query)
		}
		rm, _ := cm["reducer"].(map[string]any)
		if cd.Reducer = str(rm["type"]); !slices.Contains(classicReducers, cd.Reducer) {
			return nil, fmt.Errorf("classic condition reducer %q", cd.Reducer)
		}
		em, _ := cm["evaluator"].(map[string]any)
		cd.Type = str(em["type"])
		need, ok := classicEvaluators[cd.Type]
		if !ok {
			return nil, fmt.Errorf("classic condition evaluator %q", cd.Type)
		}
		for _, p := range list(em["params"]) {
			f, ok := toFloat(p)
			if !ok {
				return nil, fmt.Errorf("classic condition value %v", p)
			}
			cd.Params = append(cd.Params, f)
		}
		switch {
		case need == 1 && len(cd.Params) == 0:
			return nil, fmt.Errorf("classic condition %s needs a value", cd.Type)
		case need == 2 && len(cd.Params) != 2:
			return nil, fmt.Errorf("classic condition %s needs 2 values", cd.Type)
		}
		om, _ := cm["operator"].(map[string]any)
		cd.Op = str(om["type"])
		if i > 0 && cd.Op != "and" && cd.Op != "or" && cd.Op != "logic-or" {
			return nil, fmt.Errorf("classic condition operator %q", cd.Op)
		}
		cd.Expr, cd.Window, cd.Res = q.expr, q.window, q.res
		ask := func(kind, expr string) string {
			k := ref + "\x00" + strconv.Itoa(i) + "\x00" + kind
			g.Asks[k] = expr
			g.Shift[k] = q.to
			return k
		}
		if q.window == 0 {
			cd.Instant = true
			cd.Present, cd.Value = cd.Query, cd.Query
			c.Conds = append(c.Conds, cd)
			continue
		}
		// Numbers only: NaN fails every comparison.
		num := "((" + q.expr + ") >= -Inf)"
		switch cd.Reducer {
		case "count":
			cd.Value = ask("count", "count_over_time(("+q.expr+")"+q.subquery(true)+")")
			cd.Present = cd.Value
		case "count_non_null":
			cd.Value = ask("value", "count_over_time("+num+q.subquery(true)+")")
		case "sum":
			cd.Value = ask("value", "sum_over_time("+num+q.subquery(true)+")")
		case "median":
			cd.Value = ask("value", "quantile_over_time(0.5, "+num+q.subquery(false)+")")
		case "avg", "min", "max", "last":
			cd.Value = ask("value", cd.Reducer+"_over_time("+num+q.subquery(false)+")")
		default: // the diff reducers: the newest number, and the oldest
			cd.Value = ask("value", "last_over_time("+num+q.subquery(false)+")")
			cd.Oldest = ask("oldest", "last_over_time("+num+"["+promDuration(q.res/2)+":"+promDuration(q.res)+"] offset "+promDuration(q.window)+")")
		}
		if cd.Present == "" {
			cd.Present = ask("present", "count_over_time(("+q.expr+")"+q.subquery(false)+")")
		}
		c.Conds = append(c.Conds, cd)
	}
	if len(c.Conds) == 0 {
		return nil, errors.New("classic conditions without a condition")
	}
	return c, nil
}

// asks are the refs the conditions read.
func (c *Classic) asks() []string {
	var out []string
	for _, cd := range c.Conds {
		for _, r := range []string{cd.Present, cd.Value, cd.Oldest} {
			if r != "" && !slices.Contains(out, r) {
				out = append(out, r)
			}
		}
	}
	return out
}

// A Reduced is a series' value after its condition's reducer: OK is false
// for none, which Grafana's null is.
type Reduced struct {
	V  float64
	OK bool
}

// Reduce is a series' reduced value from what was asked for it: its value,
// and, for the diff reducers, the oldest number in the window. One point
// alone in a window differs from itself by nothing, as in Grafana.
func (c ClassicCond) Reduce(value, oldest Reduced) Reduced {
	if !c.Diff() || c.Instant || !value.OK {
		return value
	}
	if !oldest.OK {
		return Reduced{}
	}
	n, o := value.V, oldest.V
	switch c.Reducer {
	case "diff":
		return Reduced{n - o, true}
	case "diff_abs":
		return Reduced{math.Abs(n - o), true}
	case "percent_diff":
		return Reduced{(n - o) / math.Abs(o) * 100, true}
	}
	return Reduced{math.Abs((n - o) / o * 100), true}
}

// Pass is whether a reduced value meets the condition's evaluator, as
// Grafana's: none meets no_value alone, and NaN meets no comparison.
func (c ClassicCond) Pass(r Reduced) bool {
	if c.Type == "no_value" {
		return !r.OK
	}
	if !r.OK || len(c.Params) == 0 {
		return false
	}
	v, a := r.V, c.Params[0]
	switch c.Type {
	case "gt":
		return v > a
	case "lt":
		return v < a
	case "gte":
		return v >= a
	case "lte":
		return v <= a
	case "eq":
		return v == a
	case "ne":
		return v != a
	}
	if len(c.Params) < 2 {
		return false
	}
	b := c.Params[1]
	switch c.Type {
	case "within_range":
		return a < v && b > v || b < v && a > v
	case "outside_range":
		return b < v && a < v || b > v && a > v
	case "within_range_included":
		return a <= v && b >= v || b <= v && a >= v
	case "outside_range_included":
		return b <= v && a <= v || b >= v && a >= v
	}
	return false
}

// Eval is what the conditions come to at one evaluation, each with the
// reduced values of the series its query returned: firing, or not; and
// noData when Grafana would say there was no data, which comes first. A
// condition with no series at all has no data, unless it is no_value,
// which then holds. A condition is no data when all its series reduce to
// none; with the rest it joins as firing does, by its operator; logic-or
// leaves the rest out once those before hold, their no data too.
func (c *Classic) Eval(series func(i int) []Reduced) (firing, noData bool) {
	for i, cd := range c.Conds {
		if firing && cd.Op == "logic-or" {
			break
		}
		f, nd := cd.eval(series(i))
		if i == 0 {
			firing, noData = f, nd
			continue
		}
		if cd.Op == "or" || cd.Op == "logic-or" {
			firing, noData = firing || f, noData || nd
		} else {
			firing, noData = firing && f, noData && nd
		}
	}
	return firing && !noData, noData
}

func (cd ClassicCond) eval(rs []Reduced) (firing, noData bool) {
	if len(rs) == 0 && cd.Type == "no_value" {
		return true, false
	}
	none := 0
	for _, r := range rs {
		switch {
		case cd.Pass(r):
			firing = true
		case !r.OK:
			none++
		}
	}
	return firing, none == len(rs)
}

// Above reports whether the condition's evaluator holds above its number,
// and Below below it; a range, eq, ne and no_value are neither.
func (cd ClassicCond) Above() bool { return cd.Type == "gt" || cd.Type == "gte" }

// Below reports whether the condition's evaluator holds below its number.
func (cd ClassicCond) Below() bool { return cd.Type == "lt" || cd.Type == "lte" }

// ValueOf is, for condition at, which compares above or below one number,
// the value it compares as far as the conditions' outcome turns on it: of
// the series, the largest reduced value for one above, the smallest for
// one below, where the outcome holds with that condition holding and not
// without; +Inf (above) or -Inf (below) where it holds either way; NaN
// where it holds neither way, or the condition has no number to compare.
func (c *Classic) ValueOf(at int, series func(i int) []Reduced) float64 {
	cd := c.Conds[at]
	var best float64
	found := false
	for _, r := range series(at) {
		if !r.OK || math.IsNaN(r.V) {
			continue
		}
		if !found || cd.Above() && r.V > best || cd.Below() && r.V < best {
			best, found = r.V, true
		}
	}
	if !found {
		return math.NaN()
	}
	forced := func(holds bool) bool {
		f, _ := c.Eval(func(i int) []Reduced {
			if i != at {
				return series(i)
			}
			// A value that passes, or one that doesn't.
			v := best
			if holds != cd.Pass(Reduced{best, true}) {
				v = math.NaN()
				if holds {
					v = math.Inf(1)
					if cd.Below() {
						v = math.Inf(-1)
					}
				}
			}
			return []Reduced{{v, true}}
		})
		return f
	}
	yes, no := forced(true), forced(false)
	switch {
	case yes && no:
		if cd.Below() {
			return math.Inf(-1)
		}
		return math.Inf(1)
	case yes:
		return best
	}
	return math.NaN()
}
