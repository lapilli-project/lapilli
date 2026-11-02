// Package rules reads alert rules in a form that can be replayed against
// history: PromQL for a Prometheus rule, queries and expressions for a
// Grafana rule.
//
// It reads Prometheus rule files, PrometheusRule resources, Grafana alerting
// provisioning files, and ConfigMaps that hold any of them.
package rules

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/sse"
)

// Rule is one alert rule.
type Rule struct {
	Name   string
	Group  string
	Source string // the file it came from
	// Expr, for a Prometheus rule, returns a series while the condition
	// holds.
	Expr string
	// Graph, for a Grafana rule, is its queries and expressions.
	Graph  *Graph
	For    time.Duration
	Labels map[string]string
	// UID is a Grafana rule's uid, which stays the same when it is edited.
	UID string
	// Digest, for a Grafana rule, changes when anything in the rule does;
	// Grafana starts a rule's alerts afresh when it changes.
	Digest string
	// Unsupported says why the rule can't be replayed.
	Unsupported string
}

// Paused is the Unsupported of a rule paused in Grafana, which fires
// nothing.
const Paused = "paused"

// errNoRules is a document with no alert rules in it.
var errNoRules = errors.New("no alert rules: want a Prometheus rule file, a PrometheusRule, a Grafana alerting provisioning file, or a ConfigMap holding one")

// Load reads every alert rule in a file, or in the files of a directory,
// such as a ConfigMap mounted in a pod. In a directory, hidden files and
// files without alert rules are passed over.
func Load(path string) ([]Rule, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return load(path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	var out []Rule
	for _, e := range entries {
		p := filepath.Join(path, e.Name())
		// A mounted ConfigMap holds its keys as links into ..data.
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if st, err := os.Stat(p); err != nil || !st.Mode().IsRegular() {
			continue
		}
		rs, err := load(p)
		if errors.Is(err, errNoRules) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, rs...)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no file in it holds alert rules", path)
	}
	return out, nil
}

func load(path string) ([]Rule, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rs, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for i := range rs {
		rs[i].Source = path
	}
	return rs, nil
}

// Parse reads every alert rule in a document, which may hold several YAML
// documents.
func Parse(b []byte) ([]Rule, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	var out []Rule
	found := false
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		rs, ok, err := parseDoc(doc)
		if err != nil {
			return nil, err
		}
		found = found || ok
		out = append(out, rs...)
	}
	if !found {
		return nil, errNoRules
	}
	return out, nil
}

func parseDoc(doc map[string]any) ([]Rule, bool, error) {
	switch {
	case doc == nil:
		return nil, false, nil
	case doc["kind"] == "ConfigMap":
		var out []Rule
		found := false
		data, _ := doc["data"].(map[string]any)
		for _, k := range sortedKeys(data) {
			s, _ := data[k].(string)
			rs, err := Parse([]byte(s))
			if err != nil {
				continue // not every key holds rules
			}
			found = true
			out = append(out, rs...)
		}
		return out, found, nil
	case doc["kind"] == "PrometheusRule":
		spec, _ := doc["spec"].(map[string]any)
		rs, err := prometheus(spec)
		return rs, true, err
	case doc["apiVersion"] != nil && hasGrafanaRules(doc):
		rs, err := grafana(doc)
		return rs, true, err
	case doc["groups"] != nil:
		rs, err := prometheus(doc)
		return rs, true, err
	}
	return nil, false, nil
}

// prometheus reads groups of Prometheus rules; recording rules are skipped.
func prometheus(doc map[string]any) ([]Rule, error) {
	var out []Rule
	for _, g := range list(doc["groups"]) {
		gm, _ := g.(map[string]any)
		group := str(gm["name"])
		for _, r := range list(gm["rules"]) {
			rm, _ := r.(map[string]any)
			name := str(rm["alert"])
			if name == "" {
				continue
			}
			rule := Rule{Name: name, Group: group, Expr: strings.TrimSpace(str(rm["expr"])), Labels: labels(rm["labels"])}
			var err error
			if rule.For, err = duration(rm["for"]); err != nil {
				return nil, fmt.Errorf("rule %s: %w", name, err)
			}
			out = append(out, rule)
		}
	}
	return out, nil
}

func hasGrafanaRules(doc map[string]any) bool {
	for _, g := range list(doc["groups"]) {
		gm, _ := g.(map[string]any)
		for _, r := range list(gm["rules"]) {
			if rm, _ := r.(map[string]any); rm["data"] != nil && rm["condition"] != nil {
				return true
			}
		}
	}
	return false
}

// grafana reads a Grafana alerting provisioning file.
func grafana(doc map[string]any) ([]Rule, error) {
	var out []Rule
	for _, g := range list(doc["groups"]) {
		gm, _ := g.(map[string]any)
		group := str(gm["name"])
		if f := str(gm["folder"]); f != "" {
			group = f + "/" + group
		}
		for _, r := range list(gm["rules"]) {
			rm, _ := r.(map[string]any)
			rule := Rule{
				Name: str(rm["title"]), Group: group, Labels: labels(rm["labels"]), UID: str(rm["uid"]),
				// fmt prints maps in key order, so this is the same for the same rule.
				Digest: fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprint(rm)))),
			}
			if rule.Name == "" {
				rule.Name = str(rm["uid"])
			}
			var err error
			if rule.For, err = duration(rm["for"]); err != nil {
				return nil, fmt.Errorf("rule %s: %w", rule.Name, err)
			}
			if paused, _ := rm["isPaused"].(bool); paused {
				rule.Unsupported = Paused
			} else if rule.Graph, err = grafanaGraph(rm); err != nil {
				rule.Unsupported = err.Error()
			}
			out = append(out, rule)
		}
	}
	return out, nil
}

// Graph is a Grafana rule's queries and expressions, to be evaluated
// together at each step the way Grafana evaluates them.
type Graph struct {
	// Queries maps a refId to PromQL giving that node's value at each
	// step; a reduce over a range query is folded into its PromQL.
	Queries map[string]string
	Steps   map[string]Step // refId to a threshold or math expression
	Cond    string          // the refId whose result decides firing
}

// Step is a threshold, a math expression, or a reduce over an expression.
type Step struct {
	Threshold *Threshold
	Math      sse.Expr
	Reduce    *Reduce
}

// Reduce collapses an expression's values over a trailing window into one
// per series. A reduce straight over a query is folded into its PromQL
// instead.
type Reduce struct {
	Input string
	Fn    string // last, mean, max, min, sum, count
	// Window is the widest range query under Input; 0 when they are all
	// instant, and the reduce passes each value through.
	Window time.Duration
}

// Uses lists the refs a step reads.
func (s Step) Uses() []string {
	switch {
	case s.Threshold != nil:
		return []string{s.Threshold.Input}
	case s.Reduce != nil:
		return []string{s.Reduce.Input}
	case s.Math != nil:
		return s.Math.Refs()
	}
	return nil
}

// Threshold compares its input with one or two values.
type Threshold struct {
	Input string
	Type  string // gt, lt, gte, lte, within_range, outside_range, and the _included forms
	A, B  float64
}

// Pass reports whether v meets the threshold.
func (t Threshold) Pass(v float64) bool {
	switch t.Type {
	case "gt":
		return v > t.A
	case "lt":
		return v < t.A
	case "gte":
		return v >= t.A
	case "lte":
		return v <= t.A
	case "within_range":
		return v > t.A && v < t.B
	case "within_range_included":
		return v >= t.A && v <= t.B
	case "outside_range":
		return v < t.A || v > t.B
	case "outside_range_included":
		return v <= t.A || v >= t.B
	}
	return false
}

var reducers = map[string]string{
	"last": "last_over_time", "mean": "avg_over_time", "max": "max_over_time",
	"min": "min_over_time", "sum": "sum_over_time", "count": "count_over_time",
}

var thresholdTypes = map[string]int{ // how many values each takes
	"gt": 1, "lt": 1, "gte": 1, "lte": 1,
	"within_range": 2, "outside_range": 2, "within_range_included": 2, "outside_range_included": 2,
}

// grafanaGraph reads a rule's queries and expressions. It takes the shapes
// alert rules use: PromQL queries, reduce, threshold, and math.
func grafanaGraph(rm map[string]any) (*Graph, error) {
	type query struct {
		expr   string
		window time.Duration // a range query's window; 0 for an instant query
	}
	queries := map[string]query{}
	exprs := map[string]map[string]any{}
	for _, d := range list(rm["data"]) {
		dm, _ := d.(map[string]any)
		model, _ := dm["model"].(map[string]any)
		ref := str(dm["refId"])
		if str(dm["datasourceUid"]) == "__expr__" {
			exprs[ref] = model
			continue
		}
		q := query{expr: strings.TrimSpace(str(model["expr"]))}
		if q.expr == "" {
			return nil, fmt.Errorf("query %s has no PromQL expr", ref)
		}
		if rt, ok := dm["relativeTimeRange"].(map[string]any); ok && !truthy(model["instant"]) {
			from, _ := toFloat(rt["from"])
			to, _ := toFloat(rt["to"])
			q.window = time.Duration(from-to) * time.Second
		}
		queries[ref] = q
	}
	g := &Graph{Queries: map[string]string{}, Steps: map[string]Step{}, Cond: str(rm["condition"])}
	for ref, q := range queries {
		g.Queries[ref] = q.expr
	}
	for ref, m := range exprs {
		in := strings.TrimPrefix(strings.Trim(str(m["expression"]), "${}"), "$")
		switch typ := str(m["type"]); typ {
		case "reduce":
			fn, ok := reducers[str(m["reducer"])]
			if !ok {
				return nil, fmt.Errorf("reducer %q", str(m["reducer"]))
			}
			q, ok := queries[in]
			if !ok {
				g.Steps[ref] = Step{Reduce: &Reduce{Input: in, Fn: str(m["reducer"])}}
				continue
			}
			if q.window > 0 {
				g.Queries[ref] = fmt.Sprintf("%s((%s)[%s:])", fn, q.expr, promDuration(q.window))
			} else {
				g.Queries[ref] = q.expr
			}
		case "threshold":
			conds := list(m["conditions"])
			if len(conds) != 1 {
				return nil, fmt.Errorf("threshold %s with %d conditions", ref, len(conds))
			}
			cm, _ := conds[0].(map[string]any)
			ev, _ := cm["evaluator"].(map[string]any)
			t := &Threshold{Input: in, Type: str(ev["type"])}
			n, ok := thresholdTypes[t.Type]
			if !ok {
				return nil, fmt.Errorf("threshold type %q", t.Type)
			}
			p := list(ev["params"])
			if len(p) < n {
				return nil, fmt.Errorf("threshold %s needs %d values", ref, n)
			}
			t.A, _ = toFloat(p[0])
			if n == 2 {
				t.B, _ = toFloat(p[1])
			}
			g.Steps[ref] = Step{Threshold: t}
		case "math":
			e, err := sse.ParseMath(str(m["expression"]))
			if err != nil {
				return nil, err
			}
			g.Steps[ref] = Step{Math: e}
		case "":
			return nil, fmt.Errorf("expression %s has no type", ref)
		default:
			return nil, fmt.Errorf("%s expressions", typ)
		}
	}
	known := func(ref string) bool {
		_, q := g.Queries[ref]
		_, s := g.Steps[ref]
		return q || s
	}
	if !known(g.Cond) {
		return nil, fmt.Errorf("condition %q is not one of the rule's queries", g.Cond)
	}
	for ref, st := range g.Steps {
		for _, u := range st.Uses() {
			if !known(u) {
				return nil, fmt.Errorf("%s refers to %q, which the rule doesn't have", ref, u)
			}
		}
	}
	// A reduce over an expression reduces over the widest range query
	// beneath it.
	for _, st := range g.Steps {
		if st.Reduce == nil {
			continue
		}
		seen := map[string]bool{}
		var walk func(string)
		walk = func(r string) {
			if seen[r] {
				return
			}
			seen[r] = true
			if q, ok := queries[r]; ok {
				st.Reduce.Window = max(st.Reduce.Window, q.window)
			}
			if s, ok := g.Steps[r]; ok {
				for _, u := range s.Uses() {
					walk(u)
				}
			}
		}
		walk(st.Reduce.Input)
	}
	return g, nil
}

func promDuration(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", d/time.Hour)
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return fmt.Sprintf("%ds", d/time.Second)
}

// duration reads a "for" value: Prometheus durations such as 5m or 1h30m,
// or a number of seconds.
func duration(v any) (time.Duration, error) {
	switch x := v.(type) {
	case nil:
		return 0, nil
	case int:
		return time.Duration(x) * time.Second, nil
	case string:
		if x == "" || x == "0" {
			return 0, nil
		}
		if d, err := time.ParseDuration(x); err == nil {
			return d, nil
		}
		// Prometheus also takes days and weeks.
		if n, err := strconv.Atoi(strings.TrimSuffix(x, "d")); err == nil && strings.HasSuffix(x, "d") {
			return time.Duration(n) * 24 * time.Hour, nil
		}
		return 0, fmt.Errorf("unreadable duration %q", x)
	}
	return 0, fmt.Errorf("unreadable duration %v", v)
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		return fmt.Sprint(x)
	}
}

func labels(v any) map[string]string {
	m, _ := v.(map[string]any)
	out := map[string]string{}
	for k, x := range m {
		out[k] = str(x)
	}
	return out
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}

func truthy(v any) bool {
	b, _ := v.(bool)
	return b
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
