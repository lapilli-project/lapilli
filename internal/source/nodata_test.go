package source

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// onlyQueue has the series of queue_depth, at 1, and no other.
type onlyQueue struct{}

func (onlyQueue) QueryRange(_ context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if !strings.Contains(expr, "queue_depth") {
		return nil, nil
	}
	s := prom.Series{Labels: map[string]string{}}
	for t := start; !t.After(end); t = t.Add(step) {
		s.Samples = append(s.Samples, prom.Sample{T: t, V: 1})
	}
	return []prom.Series{s}, nil
}

func TestNoDataStep(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	last := []rules.Rule{
		{Name: "Typo", Expr: "queue_dept > 100"},
		{Name: "Quiet", Expr: "queue_depth > 100"},
		{Name: "Loud", Expr: "gone_total > 1"},
	}
	versions := []replay.Version{{From: from, Rules: []rules.Rule{{Name: "Typo", Expr: "old > 1"}}}, {From: from, Rules: last}}
	rep := &verdict.Report{Rules: []verdict.RuleResult{
		{Rule: "Typo"}, {Rule: "Quiet"}, {Rule: "Loud", Firings: 2}, {Rule: "Gone"},
	}}
	if err := NoData(context.Background(), onlyQueue{}, versions, rep, from, to, time.Minute); err != nil {
		t.Fatal(err)
	}
	by := map[string][]string{}
	for _, rr := range rep.Rules {
		by[rr.Rule] = rr.NoData
	}
	if !slices.Equal(by["Typo"], []string{"queue_dept"}) {
		t.Errorf("Typo: %q, want its typo'd metric", by["Typo"])
	}
	if by["Quiet"] != nil || by["Loud"] != nil || by["Gone"] != nil {
		t.Errorf("only a silent rule that reads nothing has NoData: %v", by)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NoData(ctx, onlyQueue{}, versions, rep, from, to, time.Minute); err == nil {
		t.Error("an ended context didn't stop it")
	}
}
