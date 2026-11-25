package replay

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
)

// Classic conditions over Loki are reduced by the replay, in Go, as
// Grafana's classic reducers do (rules.ClassicReduce); over Prometheus they
// are worked out by its subqueries. These tests hold the two to each other,
// and the Go one to Grafana's code (docs/design/loki.md, section 2).

// pointsOf is what each series of a stand-in server has, one point a
// minute from an hour before t0 to an hour after, by series (an app) and
// minute from t0: ok false leaves the point out, NaN is a point that isn't a
// number.
type pointsOf func(app string, minute int) (v float64, ok bool)

// serverSeries are the raw series: one for each app.
func serverSeries(f pointsOf, apps ...string) []prom.Series {
	var out []prom.Series
	for _, app := range apps {
		s := prom.Series{Labels: map[string]string{"app": app}}
		for m := -180; m <= 120; m++ {
			if v, ok := f(app, m); ok {
				s.Samples = append(s.Samples, prom.Sample{T: t0.Add(time.Duration(m) * time.Minute), V: v})
			}
		}
		out = append(out, s)
	}
	return out
}

// inRange are a series' samples from start to end, both in.
func inRange(s prom.Series, start, end time.Time) []prom.Sample {
	var out []prom.Sample
	for _, x := range s.Samples {
		if !x.T.Before(start) && !x.T.After(end) {
			out = append(out, x)
		}
	}
	return out
}

// rawLoki is a Loki holding the series: it answers any range with each
// series' points in it, on the grid of whole minutes the points are on.
type rawLoki struct {
	series []prom.Series
	mu     sync.Mutex
	asked  []string
}

func (l *rawLoki) QueryRange(_ context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	l.mu.Lock()
	l.asked = append(l.asked, fmt.Sprintf("%s %s", expr, step))
	l.mu.Unlock()
	var out []prom.Series
	for _, s := range l.series {
		if smp := inRange(s, start, end); len(smp) > 0 {
			out = append(out, prom.Series{Labels: s.Labels, Samples: smp})
		}
	}
	return out, nil
}

// promStandIn is a Prometheus holding the same series, answering what
// the replay asks of it for a classic condition as the subqueries of
// Ask.PromQL are defined (count_over_time, sum_over_time, avg_over_time,
// min_over_time, max_over_time, quantile_over_time(0.5) over a window of
// points with both ends in, `>= -Inf` dropping NaN, the newest number, the
// number at the window's start) and nothing else.
type promStandIn struct {
	series []prom.Series
	asks   map[string]rules.Ask // by the PromQL the replay asks
}

func (p promStandIn) QueryRange(_ context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	var out []prom.Series
	switch a, isAsk := p.asks[expr]; {
	case expr == "((x) >= -Inf)":
		for _, s := range p.series {
			var smp []prom.Sample
			for _, x := range inRange(s, start, end) {
				if !math.IsNaN(x.V) {
					smp = append(smp, x)
				}
			}
			out = append(out, prom.Series{Labels: s.Labels, Samples: smp})
		}
	case expr == "x":
		for _, s := range p.series {
			out = append(out, prom.Series{Labels: s.Labels, Samples: inRange(s, start, end)})
		}
	case isAsk:
		for _, s := range p.series {
			res := prom.Series{Labels: s.Labels}
			for at := start; !at.After(end); at = at.Add(step) {
				if v, ok := p.reduce(a, inRange(s, at.Add(-a.Window), at), at, s); ok {
					res.Samples = append(res.Samples, prom.Sample{T: at, V: v})
				}
			}
			if len(res.Samples) > 0 {
				out = append(out, res)
			}
		}
	default:
		return nil, fmt.Errorf("unknown expression %s", expr)
	}
	return out, nil
}

// reduce is the subquery's answer over a window's samples.
func (p promStandIn) reduce(a rules.Ask, window []prom.Sample, at time.Time, s prom.Series) (float64, bool) {
	var vs []float64
	for _, x := range window {
		if !a.Numbers || !math.IsNaN(x.V) {
			vs = append(vs, x.V)
		}
	}
	if a.Fn == "oldest" {
		for _, x := range inRange(s, at.Add(-a.Window), at.Add(-a.Window)) {
			if !math.IsNaN(x.V) {
				return x.V, true
			}
		}
		return 0, false
	}
	if len(vs) == 0 {
		return 0, false
	}
	switch a.Fn {
	case "count_over_time":
		return float64(len(vs)), true
	case "last":
		return vs[len(vs)-1], true
	case "quantile_over_time":
		slices.Sort(vs)
		if len(vs)%2 == 1 {
			return vs[len(vs)/2], true
		}
		return (vs[len(vs)/2-1] + vs[len(vs)/2]) / 2, true
	}
	acc := vs[0]
	for _, v := range vs[1:] {
		switch a.Fn {
		case "max_over_time":
			acc = max(acc, v)
		case "min_over_time":
			acc = min(acc, v)
		default:
			acc += v
		}
	}
	if a.Fn == "avg_over_time" {
		acc /= float64(len(vs))
	}
	return acc, true
}

// classicDoc is a Grafana rule with classic conditions over the query on
// data source ds, a range query over three minutes; conds are its
// conditions, as `reducer evaluator params operator`.
func classicDoc(ds string, conds ...[4]string) string {
	var cs []string
	for _, c := range conds {
		op := ""
		if c[3] != "" {
			op = ", operator: {type: " + c[3] + "}"
		} else {
			op = ", operator: {type: and}"
		}
		ev := strings.Fields(c[1])
		params := ""
		if len(ev) > 1 {
			params = ", params: [" + strings.Join(ev[1:], ", ") + "]"
		}
		cs = append(cs, fmt.Sprintf("{evaluator: {type: %s%s}%s, query: {params: [A]}, reducer: {type: %s}}", ev[0], params, op, c[0]))
	}
	model := `{expr: x, intervalMs: 60000}`
	if ds == "lk" {
		model = `{datasource: {type: loki, uid: lk}, expr: 'sum by (app) (count_over_time({job="x"}[1m]))', queryType: range, intervalMs: 60000}`
	}
	return `apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    interval: 1m
    rules:
      - uid: c1
        title: Classic
        condition: C
        noDataState: OK
        data:
          - {refId: A, datasourceUid: ` + ds + `, relativeTimeRange: {from: 180, to: 0}, model: ` + model + `}
          - refId: C
            datasourceUid: __expr__
            model:
              type: classic_conditions
              conditions:
                - ` + strings.Join(cs, "\n                - ") + `
`
}

// classicRun replays the document over the series, in Loki or in the
// Prometheus stand-in, and says its firings.
func classicRun(t *testing.T, loki bool, doc string, series []prom.Series) (string, []firing.Firing) {
	t.Helper()
	rs, err := rules.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	var h History
	if loki {
		rules.Replaying{Kinds: map[string]string{"lk": rules.KindLoki}}.Apply(rs)
		l := &rawLoki{series: series}
		h = WithLogs(noPrometheus{}, func(r rules.Rule) (History, error) { return l, nil })
	} else {
		asks := map[string]rules.Ask{}
		for _, a := range rs[0].Graph.Asks {
			asks[a.PromQL(0)] = a
		}
		h = promStandIn{series: series, asks: asks}
	}
	if rs[0].Unsupported != "" || (rs[0].Logs != "") != loki {
		t.Fatalf("unsupported %q, logs %q", rs[0].Unsupported, rs[0].Logs)
	}
	ran, fs := Run(context.Background(), h, one(rs), t0, t0.Add(40*time.Minute), time.Minute, 1)
	if ran[0].Unsupported != "" {
		t.Fatalf("%s", ran[0].Unsupported)
	}
	return spans(fs), fs
}

// sample is one app's points: a value drifting up and down, NaN at some
// minutes, none at others, a shorter run of its own.
func sample(app string, m int) (float64, bool) {
	switch app {
	case "a":
		switch {
		case m%11 == 3, m%13 == 5:
			return math.NaN(), true
		case m%17 == 7, m%19 == 0 && m > 0:
			return 0, false
		}
		return float64(((m*7)%23)-3) + float64(m%5)/4, true
	default:
		if m < 12 || m > 35 && m < 38 {
			return 0, false
		}
		if m%9 == 4 {
			return math.NaN(), true
		}
		return float64((m*3)%11) - 2, true
	}
}

// TestClassicLokiAgreesWithPrometheus: the same points, as the replay reads
// them over Loki (reduced in Go) and as a Prometheus answers its
// subqueries, give the same states, for each reducer, each evaluator, and
// the operators joining conditions.
func TestClassicLokiAgreesWithPrometheus(t *testing.T) {
	series := serverSeries(sample, "a", "b")
	reducers := []string{"avg", "min", "max", "sum", "count", "last", "median", "diff", "diff_abs", "percent_diff", "percent_diff_abs", "count_non_null"}
	evaluators := []string{"gt 6", "lt 1", "outside_range 0 8", "within_range_included 2 5", "eq 4", "ne 4", "gte 3", "lte -1", "no_value"}
	distinct := map[string]bool{}
	check := func(name string, conds ...[4]string) {
		t.Helper()
		loki, _ := classicRun(t, true, classicDoc("lk", conds...), series)
		prm, _ := classicRun(t, false, classicDoc("prom", conds...), series)
		if loki != prm {
			t.Errorf("%s: Loki %s, Prometheus %s", name, loki, prm)
		}
		distinct[loki] = true
	}
	for _, r := range reducers {
		for _, e := range evaluators {
			check(r+" "+e, [4]string{r, e, "", ""})
		}
	}
	// Joined: and, or, logic-or, with no precedence (Grafana folds left to
	// right), and no data decided by the operator.
	for _, op := range []string{"and", "or", "logic-or"} {
		check("max "+op+" min", [4]string{"max", "gt 8", "", ""}, [4]string{"min", "lt 0", "", op})
		check("count "+op+" last "+op+" avg", [4]string{"count", "lt 4", "", ""}, [4]string{"last", "gt 5", "", op}, [4]string{"avg", "lt 1", "", "or"})
		check("no_value "+op+" diff", [4]string{"median", "no_value", "", ""}, [4]string{"diff", "gt 3", "", op})
	}
	if len(distinct) < 15 {
		t.Errorf("only %d different outcomes over all the cases: the data says too little", len(distinct))
	}
}

// TestClassicLokiAsksOnce: a classic condition over Loki reads one range of
// its query for its reducer and for whether a series is there.
func TestClassicLokiAsksOnce(t *testing.T) {
	rs, err := rules.Parse([]byte(classicDoc("lk", [4]string{"avg", "gt 6", "", ""})))
	if err != nil {
		t.Fatal(err)
	}
	rules.Replaying{Kinds: map[string]string{"lk": rules.KindLoki}}.Apply(rs)
	l := &rawLoki{series: serverSeries(sample, "a")}
	h := WithLogs(noPrometheus{}, func(rules.Rule) (History, error) { return l, nil })
	Run(context.Background(), h, one(rs), t0, t0.Add(10*time.Minute), time.Minute, 1)
	if len(l.asked) == 0 {
		t.Fatal("Loki wasn't asked")
	}
	if got := Plan(h, one(rs), t0, t0.Add(10*time.Minute), time.Minute); got.By["grafana/lk"] == 0 || got.Requests > len(l.asked) {
		t.Errorf("the plan %+v, the run asked %d", got, len(l.asked))
	}
	for _, a := range l.asked {
		if !strings.HasSuffix(a, " 1m0s") {
			t.Errorf("asked %q, at Grafana's step", a)
		}
	}
}

// TestClassicLokiNoData: over Loki, as Grafana's classic conditions have it
// (pkg/expr/classic/classic.go, executeCond): a series whose points are all
// NaN reduces to none, and a condition whose every series does is no data,
// which comes before firing; no_value holds for it; NaN fails every
// comparison but ne.
func TestClassicLokiNoData(t *testing.T) {
	// Numbers, but for minutes 10 to 19, which are NaN.
	f := func(_ string, m int) (float64, bool) {
		if m >= 10 && m < 20 {
			return math.NaN(), true
		}
		return 1, true
	}
	series := serverSeries(f, "a")
	run := func(noData, reducer, evaluator string) string {
		doc := strings.Replace(classicDoc("lk", [4]string{reducer, evaluator, "", ""}), "noDataState: OK", "noDataState: "+noData, 1)
		got, _ := classicRun(t, true, doc, series)
		return got
	}
	// avg over a window of 3 minutes is none from minute 13 (10 to 13 all
	// NaN) to 19, and a number again at 20 (its window holds minute 20):
	// no data, which, as NoData, is an alert of its own for those.
	if got, want := run("NoData", "avg", "gt 0"), "0-13[] 13-20[grafana_alertstate=nodata] 20-on[]"; got != want {
		t.Errorf("avg, NoData: %s, want %s", got, want)
	}
	// As OK, none of it fires but the firing.
	if got, want := run("OK", "avg", "gt 0"), "0-13[] 20-on[]"; got != want {
		t.Errorf("avg, OK: %s, want %s", got, want)
	}
	// count counts NaN points: never none, and never fires gt 4 (four
	// points a window).
	if got := run("NoData", "count", "gt 3"); got != "0-on[]" {
		t.Errorf("count: %s", got)
	}
	// no_value holds where the series reduces to none.
	if got, want := run("OK", "avg", "no_value"), "13-20[]"; got != want {
		t.Errorf("no_value: %s, want %s", got, want)
	}
	// ne holds for NaN as a number; last of NaNs is none: no data.
	if got, want := run("OK", "last", "no_value"), "13-20[]"; got != want {
		t.Errorf("last, no_value: %s, want %s", got, want)
	}
	// diff of a window whose newest number is its first point alone is 0.
	if got := run("OK", "diff", "gt 0"); got != "" {
		t.Errorf("diff of 1s: %s", got)
	}
}
