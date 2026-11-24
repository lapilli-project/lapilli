package replay

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
)

// byMetric answers each question that names a metric of its map with that
// series' value at every step, and any other with no series.
type byMetric map[string]float64

func (m byMetric) QueryRange(_ context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	for name, v := range m {
		if !strings.Contains(expr, name) {
			continue
		}
		s := prom.Series{Labels: map[string]string{}}
		for t := start; !t.After(end); t = t.Add(step) {
			s.Samples = append(s.Samples, prom.Sample{T: t, V: v})
		}
		return []prom.Series{s}, nil
	}
	return nil, nil
}

func TestNoData(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	h := byMetric{"queue_depth": 3}
	for _, c := range []struct {
		name string
		r    rules.Rule
		want []string
	}{
		{"typo", rules.Rule{Name: "A", Expr: "queue_dept > 100"}, []string{"queue_dept"}},
		{"there, below the bound", rules.Rule{Name: "B", Expr: "queue_depth > 100"}, nil},
		{"the value it returns, with a filter beside it", rules.Rule{Name: "C", Expr: "missing_total > 1 and queue_depth > 100"}, []string{"missing_total"}},
		{"a value beside another under or isn't the one it returns", rules.Rule{Name: "D", Expr: "gone_total > 1 or gone_total > 5"}, nil},
		{"logs", rules.Rule{Name: "E", Logs: rules.LokiRuler, Expr: "sum(rate({a=\"b\"}[5m])) > 1"}, nil},
		{"graph", rules.Rule{Name: "F", Expr: "x", Graph: &rules.Graph{}}, nil},
		{"unsupported", rules.Rule{Name: "G", Expr: "nothing > 1", Unsupported: "no"}, nil},
	} {
		got, err := NoData(context.Background(), h, c.r, from, to, time.Minute)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: NoData %q, want %q", c.name, got, c.want)
		}
	}
}
