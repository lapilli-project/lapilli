package replay

import (
	"slices"
	"strings"
	"testing"

	"lapilli.dev/internal/rules"
)

// Queries are the expressions a rule's condition asks the history for,
// each once: a plain rule's own, a Grafana rule's queries the condition
// reads; none for one that can't be replayed.
func TestQueries(t *testing.T) {
	rs, err := rules.Parse([]byte(`apiVersion: 1
groups:
  - name: g
    rules:
      - title: Gated errors
        condition: C
        data:
          - {refId: A, datasourceUid: gmp, model: {expr: errors, instant: true}}
          - {refId: B, datasourceUid: gmp, model: {expr: volume, instant: true}}
          - {refId: M, datasourceUid: __expr__, model: {type: math, expression: '$A * ($B >= 20)'}}
          - refId: C
            datasourceUid: __expr__
            model: {type: threshold, expression: M, conditions: [{evaluator: {type: gt, params: [5]}}]}
`))
	if err != nil || len(rs) != 1 || rs[0].Graph == nil {
		t.Fatalf("%+v %v", rs, err)
	}
	if got := Queries(rs[0]); !slices.Equal(got, []Query{{Expr: "errors"}, {Expr: "volume"}}) {
		t.Errorf("a Grafana rule: %q", got)
	}
	plain := rules.Rule{Name: "R", Expr: "up == 0"}
	if got := Queries(plain); !slices.Equal(got, []Query{{Expr: "up == 0"}}) {
		t.Errorf("a plain rule: %q", got)
	}
	plain.Unsupported = "paused"
	if got := Queries(plain); got != nil {
		t.Errorf("a rule that can't be replayed: %q", got)
	}
}

// A Loki query beside a Prometheus condition is said to be a Loki's, by its
// data source's uid, as the replay routes it (asideFor): not asked of the
// history.
func TestQueriesAside(t *testing.T) {
	r := asideRules(t, mixed)
	got := Queries(r)
	var loki, prom int
	for _, q := range got {
		switch q.Logs {
		case "loki":
			loki++
			if !strings.Contains(q.Expr, "count_over_time") {
				t.Errorf("a query for Loki: %q", q.Expr)
			}
		case "":
			prom++
		default:
			t.Errorf("%+v", q)
		}
	}
	if loki != 1 || prom != 1 {
		t.Errorf("%+v: %d for Loki, %d for the history", got, loki, prom)
	}
}
