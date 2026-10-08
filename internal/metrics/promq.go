package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

// PromqUsage is what `promq` prints when it is given nothing to ask.
const PromqUsage = "usage: promq '<PromQL>' [--range 30m] [--step 15s] [--at <Unix seconds or RFC 3339>]"

// maxSeries bounds what one query prints: an agent that asks for everything should get an answer it can read.
const maxSeries = 60

// Promq is the metrics helper an agent is given: one PromQL query against a Prometheus-compatible
// endpoint, printed one series per line. With --range it asks for the window ending now, and prints
// each point with the time the endpoint stamped it — which, from a frozen store, is the incident's own
// time, the one in the pod logs beside it.
//
// With --at it asks about an instant that is named: the query is evaluated there, and a range ends
// there. That is how a time read in a pod log is asked about; and it is how the same question is put
// to a Prometheus and to the case frozen from it (test/replay-diff), since "now" is never the same
// instant twice. Asked of a frozen store about its freeze, it is the question promq asks unasked.
func Promq(w io.Writer, client *http.Client, baseURL string, args []string, now time.Time) error {
	if len(args) == 0 {
		return errors.New(PromqUsage)
	}
	// What follows the query is a flag and its value, and a flag that is not one of promq's is said to
	// be none. It used to be dropped, and `promq 'up' --time 19:24` answered about now; and a query in
	// the wrong place — `promq --range 5m 'up'` — was asked as the query `--range`, which is PromQL.
	opt := map[string]string{}
	for i := 1; i < len(args); i += 2 {
		if !promqFlags[args[i]] || i+1 == len(args) {
			return fmt.Errorf("%q is not something promq takes after the query, or has nothing after it\n%s", args[i], PromqUsage)
		}
		opt[args[i]] = args[i+1]
	}
	if promqFlags[args[0]] {
		return fmt.Errorf("the query comes first, and %q is a flag\n%s", args[0], PromqUsage)
	}
	named := false
	if at, given := opt["--at"]; given {
		when, err := parseTime(at)
		if err != nil || when.Before(earliestNamed) { // 0930 is a number, and half past nine it is not
			return fmt.Errorf("--at %q: not a time: give Unix seconds or RFC 3339", at)
		}
		now, named = when, true
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	base := strings.TrimRight(baseURL, "/")
	path, params := "/api/v1/query", url.Values{"query": {args[0]}}
	stamp := func(t time.Time) string { return strconv.FormatFloat(float64(t.UnixMilli())/1000, 'f', 3, 64) }
	if named { // otherwise the endpoint's own now, which for a frozen store is the freeze, to the millisecond
		params.Set("time", stamp(now))
	}
	if r, ok := opt["--range"]; ok {
		window, err := model.ParseDuration(r)
		if err != nil {
			return fmt.Errorf("--range %q: %w", r, err)
		}
		step := opt["--step"]
		if step == "" {
			step = "15s"
		}
		path = "/api/v1/query_range"
		params.Del("time")
		params.Set("start", stamp(now.Add(-time.Duration(window))))
		params.Set("end", stamp(now))
		params.Set("step", step)
	}
	resp, err := client.Get(base + path + "?" + params.Encode())
	if err != nil {
		return fmt.Errorf("query failed: %w", err)
	}
	defer resp.Body.Close()
	var body struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("query failed: %s: %w", resp.Status, err)
	}
	if body.Status != "success" {
		return fmt.Errorf("query failed: %s", body.Error)
	}
	return render(w, body.Data.ResultType, body.Data.Result, maxSeries, orderOf(args[0]))
}

var promqFlags = map[string]bool{"--range": true, "--step": true, "--at": true}

// earliestNamed is the earliest instant --at takes: a bare number below it is a mistake for a clock time.
var earliestNamed = time.Unix(1_000_000_000, 0)

// ordering is how a query orders its own result.
type ordering int

const (
	byNothing ordering = iota // the query does not say: printed by label
	byValue                   // sort, sort_desc, topk, bottomk, outermost: as it came, equals by label
	asItCame                  // ordered somewhere inside, or by a label: as it came
)

// everyFunction reads a query with what a Prometheus may have switched on, experimental functions
// among them: `sort_by_label` is one, and a query that uses it has to be read as ordered.
var everyFunction = parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

var saysAnOrder = regexp.MustCompile(`\b(sort|sort_desc|sort_by_label|sort_by_label_desc|topk|bottomk)\b`)

// orderOf reads a query for the order its result comes in.
//
// Only the outermost thing a query does decides. `sort_desc(x)` and `topk(3, x)` come by value, and
// equal values are then put by label, which does not disturb them. Something done to each series of
// an ordered vector keeps the order and not the values it was ordered by — `round(sort_desc(x))`,
// `timestamp(sort_desc(x))`, `sort_desc(x) * 100` — so it is printed as it came and nothing is put
// right among equals, which would be to sort it over again by the wrong thing. And an aggregation
// over an ordered vector, `sum by (client) (topk(5, x))`, has thrown the order away: its groups are
// printed by label like any other's. A query that does not parse is read as text, and one that
// names an order anywhere is left as it came.
func orderOf(query string) ordering {
	expr, err := everyFunction.ParseExpr(query)
	if err != nil {
		if saysAnOrder.MatchString(query) {
			return asItCame
		}
		return byNothing
	}
	var of func(parser.Expr) ordering
	of = func(e parser.Expr) ordering {
		kept := func(inner ...parser.Expr) ordering { // each series of something ordered: still in that order
			for _, x := range inner {
				if x != nil && x.Type() == parser.ValueTypeVector && of(x) != byNothing {
					return asItCame
				}
			}
			return byNothing
		}
		switch n := e.(type) {
		case *parser.ParenExpr:
			return of(n.Expr)
		case *parser.StepInvariantExpr:
			return of(n.Expr)
		case *parser.Call:
			switch n.Func.Name {
			case "sort", "sort_desc":
				return byValue
			case "sort_by_label", "sort_by_label_desc":
				return asItCame
			}
			return kept(n.Args...)
		case *parser.AggregateExpr:
			if n.Op == parser.TOPK || n.Op == parser.BOTTOMK {
				return byValue
			}
			return byNothing
		case *parser.UnaryExpr:
			return kept(n.Expr)
		case *parser.BinaryExpr:
			return kept(n.LHS, n.RHS)
		}
		return byNothing
	}
	return of(expr)
}

// number prints a value to ten significant digits, in plain notation wherever that is readable. Six
// digits with an exponent, which this used to print, turns two counters of 1234567 and 1234580 into the
// same "1.23457e+06", and their difference is what an agent was asked to quantify.
func number(v any) string {
	f, err := strconv.ParseFloat(fmt.Sprint(v), 64)
	if err != nil {
		return fmt.Sprint(v)
	}
	rounded, _ := strconv.ParseFloat(strconv.FormatFloat(f, 'g', 10, 64), 64)
	if abs := math.Abs(rounded); abs != 0 && (abs < 1e-6 || abs >= 1e15) || math.IsInf(rounded, 0) || math.IsNaN(rounded) {
		return strconv.FormatFloat(rounded, 'g', -1, 64)
	}
	return strconv.FormatFloat(rounded, 'f', -1, 64)
}

// render prints a query result the way promq does: one series per line, labels sorted. limit bounds
// the series printed; zero prints all of them.
//
// The series of an instant vector are printed in the order of their labels, unless the query orders
// them itself. PromQL leaves that order open, and an engine returns the series as its store listed
// them: a Prometheus as its head created them — the order an application first exposed them in — or
// by label where a query reaches one of its blocks, and a case frozen before it kept its Prometheus's
// order, by label. Printed as they came, the same query showed one list from one store and another
// from the next, and where there are more series than are printed, not the same series. Where the
// query sorts by value, series of equal value are put in the order of their labels for the same
// reason; which of them `topk` keeps when it has to choose among equals is the engine's, and is not
// something a printer can put right (a case keeps its series in the Prometheus's order for that:
// Store.Add). orderOf says which of the three a query is.
func render(w io.Writer, resultType string, raw json.RawMessage, limit int, order ordering) error {
	clock := func(v any) string {
		f, _ := strconv.ParseFloat(fmt.Sprint(v), 64)
		return FromSeconds(f).UTC().Format("15:04:05")
	}
	switch resultType {
	case "scalar", "string":
		var point [2]any
		if err := json.Unmarshal(raw, &point); err != nil {
			return err
		}
		fmt.Fprintln(w, number(point[1]))
		return nil
	}
	var result []struct {
		Metric map[string]string `json:"metric"`
		Value  *[2]any           `json:"value"`
		Values [][2]any          `json:"values"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	if len(result) == 0 {
		fmt.Fprintln(w, "(empty result)")
		return nil
	}
	if resultType == "vector" {
		byLabels := func(i, j int) bool {
			return labels.Compare(labels.FromMap(result[i].Metric), labels.FromMap(result[j].Metric)) < 0
		}
		switch order {
		case byNothing:
			sort.SliceStable(result, byLabels)
		case byValue:
			for from := 0; from < len(result); { // within a run of one value, and the runs stay where they are
				to := from + 1
				for to < len(result) && result[to].Value != nil && result[from].Value != nil && fmt.Sprint(result[to].Value[1]) == fmt.Sprint(result[from].Value[1]) {
					to++
				}
				run := result[from:to]
				sort.SliceStable(run, func(i, j int) bool {
					return labels.Compare(labels.FromMap(run[i].Metric), labels.FromMap(run[j].Metric)) < 0
				})
				from = to
			}
		}
	}
	for i, s := range result {
		if limit > 0 && i == limit {
			fmt.Fprintf(w, "(%d more series not shown; narrow the query)\n", len(result)-limit)
			break
		}
		names := make([]string, 0, len(s.Metric))
		for k := range s.Metric {
			if k != model.MetricNameLabel {
				names = append(names, k)
			}
		}
		sort.Strings(names)
		pairs := make([]string, len(names))
		for j, k := range names {
			pairs[j] = k + "=" + s.Metric[k]
		}
		fmt.Fprintf(w, "%s{%s}", s.Metric[model.MetricNameLabel], strings.Join(pairs, ","))
		if s.Value != nil {
			fmt.Fprintf(w, " %s", number(s.Value[1]))
		}
		for _, p := range s.Values {
			fmt.Fprintf(w, " %s=%s", clock(p[0]), number(p[1]))
		}
		fmt.Fprintln(w)
	}
	return nil
}

// Text evaluates an instant query at the freeze and returns what promq would print for it, every
// series included. It is how a case is checked for evidence that lives in its metrics: the pattern in
// the answer key is written against this text.
func (a *API) Text(ctx context.Context, query string) (string, error) {
	q, err := a.engine.NewInstantQuery(ctx, a.store, nil, query, a.freeze)
	if err != nil {
		return "", err
	}
	defer q.Close()
	res := q.Exec(ctx)
	if res.Err != nil {
		return "", res.Err
	}
	raw, err := json.Marshal(encode(res.Value))
	if err != nil {
		return "", err
	}
	var out strings.Builder
	err = render(&out, string(res.Value.Type()), raw, 0, orderOf(query))
	return out.String(), err
}
