package replay

import (
	"slices"
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
	if got := Queries(rs[0]); !slices.Equal(got, []string{"errors", "volume"}) {
		t.Errorf("a Grafana rule: %q", got)
	}
	plain := rules.Rule{Name: "R", Expr: "up == 0"}
	if got := Queries(plain); !slices.Equal(got, []string{"up == 0"}) {
		t.Errorf("a plain rule: %q", got)
	}
	plain.Unsupported = "paused"
	if got := Queries(plain); got != nil {
		t.Errorf("a rule that can't be replayed: %q", got)
	}
}
