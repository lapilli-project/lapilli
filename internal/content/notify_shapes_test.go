package content

import (
	"fmt"
	"testing"

	"lapilli.dev/internal/rules"
)

// graphRule is a Grafana rule whose condition, C, is over the queries and
// expressions given.
func graphRule(t *testing.T, data string) rules.Rule {
	t.Helper()
	doc := `apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: shop
    interval: 1m
    rules:
      - uid: u1
        title: ShopErrors
        condition: C
        data:
` + data + `
        for: 1m
        annotations: {summary: errors, runbook_url: 'https://r.example/x'}
        labels: {severity: page}
`
	rs, err := rules.FromDocs([]rules.Doc{{Source: "grafana", Data: []byte(doc)}})
	if err != nil || len(rs) != 1 {
		t.Fatalf("%v %d", err, len(rs))
	}
	return rs[0]
}

// queryA is a Prometheus query, instant or not, sum by (job).
const queryA = `          - refId: A
            datasourceUid: prom
            relativeTimeRange: {from: 600, to: 0}
            model: {refId: A, expr: 'sum by (job) (rate(http_errors_total[5m]))', instant: %s}
`

// The heuristic on notification templates reads a Grafana rule's one
// query back from its condition, through the shapes Grafana's rules take:
// a threshold, a reduce (folded into the query, for a range query), math.
// Two queries: nothing guessed (a verification found the reduce missed).
func TestNotifyHeuristicShapes(t *testing.T) {
	tmpl := `[{"name": "c", "template": "{{ define \"c.msg\" }}on {{ .CommonLabels.cluster }} for {{ .CommonLabels.job }}{{ end }}"}]`
	cps := `[{"name": "root", "settings": {"message": "{{ template \"c.msg\" . }}"}}]`
	n, _, err := ReadNotifications([]byte(tmpl), []byte(cps), []byte(`{"receiver": "root"}`))
	if err != nil {
		t.Fatal(err)
	}
	thr := func(in string) string {
		return fmt.Sprintf(`          - refId: C
            datasourceUid: __expr__
            model: {refId: C, type: threshold, expression: %s, conditions: [{evaluator: {type: gt, params: [1]}}]}
`, in)
	}
	reduce := func(fn string) string {
		return `          - refId: B
            datasourceUid: __expr__
            model: {refId: B, type: reduce, expression: A, reducer: ` + fn + `}
`
	}
	want := NotifyLabelNotGiven + `:"c.msg": cluster`
	for name, c := range map[string]struct{ data, want string }{
		"A instant -> C threshold":                  {fmt.Sprintf(queryA, "true") + thr("A"), want},
		"A instant -> B reduce last -> C threshold": {fmt.Sprintf(queryA, "true") + reduce("last") + thr("B"), want},
		"A range -> B reduce mean -> C threshold":   {fmt.Sprintf(queryA, "false") + reduce("mean") + thr("B"), want},
		"A range -> B reduce max -> C threshold":    {fmt.Sprintf(queryA, "false") + reduce("max") + thr("B"), want},
		"A instant -> B math $A*2 -> C threshold": {fmt.Sprintf(queryA, "true") + `          - refId: B
            datasourceUid: __expr__
            model: {refId: B, type: math, expression: '$A * 2'}
` + thr("B"), want},
		"A and D -> B math $A+$D -> C threshold": {fmt.Sprintf(queryA, "true") + `          - refId: D
            datasourceUid: prom
            relativeTimeRange: {from: 600, to: 0}
            model: {refId: D, expr: 'sum by (job) (up)', instant: true}
          - refId: B
            datasourceUid: __expr__
            model: {refId: B, type: math, expression: '$A + $D'}
` + thr("B"), ""},
	} {
		r := graphRule(t, c.data)
		var got string
		for _, f := range checks(Check([]rules.Rule{r}, Options{Notify: n}), 0) {
			if f != NoSeverity {
				got += f
			}
		}
		if got != c.want {
			t.Errorf("%s: %q, want %q (its query %q)", name, got, c.want, promQL(r))
		}
	}
}
