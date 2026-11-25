package rules

import (
	"fmt"
	"strings"
	"testing"
)

// TestSeriesCondition: a condition that comes to a time series is listed, as
// Grafana 13.3.1 evaluates it as an error at every evaluation its queries
// return something (seen running test/e2e/loki.sh: "invalid format of
// evaluation results for the alert definition B: looks like time series
// data, only reduced data can be alerted on"; pkg/services/ngalert/eval,
// evaluateExecutionResult). Nothing that Grafana runs fine is: a reduce or a
// classic condition makes one number of a series, an instant query is a
// number, and so is a math of numbers.
func TestSeriesCondition(t *testing.T) {
	doc := func(query, steps string) string {
		return `apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    interval: 1m
    rules:
      - uid: s1
        title: S
        condition: C
        data:
` + query + steps
	}
	prom := func(extra string) string {
		return fmt.Sprintf("          - {refId: A, datasourceUid: prom, relativeTimeRange: {from: 600, to: 0}, model: {expr: 'sum(rate(x[5m]))'%s}}\n", extra)
	}
	loki := func(qt string) string {
		return fmt.Sprintf("          - {refId: A, datasourceUid: lk, relativeTimeRange: {from: 600, to: 0}, model: {datasource: {type: loki, uid: lk}, expr: 'sum(count_over_time({app=\"x\"}[1m]))', queryType: %s}}\n", qt)
	}
	step := func(ref, model string) string {
		return "          - {refId: " + ref + ", datasourceUid: __expr__, model: " + model + "}\n"
	}
	threshold := func(in string) string {
		return step("C", "{type: threshold, expression: "+in+", conditions: [{evaluator: {type: gt, params: [1]}}]}")
	}
	reduce := step("B", "{type: reduce, expression: A, reducer: max}")
	classic := step("C", "{type: classic_conditions, conditions: [{evaluator: {type: gt, params: [1]}, operator: {type: and}, query: {params: [A]}, reducer: {type: max}}]}")
	const why = "looks like time series data"
	for name, c := range map[string]struct {
		doc    string
		listed bool
	}{
		"threshold on a Prometheus range query":    {doc(prom(""), threshold("A")), true},
		"threshold on a Loki range query":          {doc(loki("range"), threshold("A")), true},
		"a Loki query without a queryType":         {doc(loki("''"), threshold("A")), true},
		"math on a range query":                    {doc(prom(""), step("C", `{type: math, expression: "$A > 5"}`)), true},
		"a threshold over math over a range query": {doc(prom(""), step("M", `{type: math, expression: "$A * 2"}`)+threshold("M")), true},
		"a condition that is the range query":      {strings.Replace(doc(prom(""), ""), "condition: C", "condition: A", 1), true},
		"threshold on an instant query":            {doc(prom(", instant: true"), threshold("A")), false},
		"threshold on a Loki instant query":        {doc(loki("instant"), threshold("A")), false},
		"reduce, threshold":                        {doc(prom(""), reduce+threshold("B")), false},
		"reduce, math":                             {doc(prom(""), reduce+step("C", `{type: math, expression: "$B > 5"}`)), false},
		"a Loki reduce":                            {doc(loki("range"), reduce+threshold("B")), false},
		"classic conditions on a range query":      {doc(prom(""), classic), false},
		"classic conditions on a Loki range query": {doc(loki("range"), classic), false},
		"math of a reduce and a range's reduce":    {doc(prom(""), reduce+step("D", "{type: reduce, expression: A, reducer: min}")+step("C", `{type: math, expression: "$B - $D > 1"}`)), false},
	} {
		rs, err := Parse([]byte(c.doc))
		if err != nil || len(rs) != 1 {
			t.Fatalf("%s: %v %v", name, err, rs)
		}
		Replaying{Kinds: map[string]string{"lk": KindLoki, "prom": "prometheus"}}.Apply(rs)
		if got := strings.Contains(rs[0].Unsupported, why); got != c.listed || !c.listed && rs[0].Unsupported != "" {
			t.Errorf("%s: %q, listed %v want %v", name, rs[0].Unsupported, got, c.listed)
		}
	}
}
