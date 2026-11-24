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

// TestNoDataVersions: with several versions of the rules, each is asked over
// the span it was in effect, and a rule has NoData only when every span had
// none; rules sharing a name all need to have none.
func TestNoDataVersions(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(4 * time.Hour)
	at := func(h int) time.Time { return from.Add(time.Duration(h) * time.Hour) }
	run := func(versions []replay.Version) map[string][]string {
		t.Helper()
		rep := &verdict.Report{From: from, To: to, Rules: []verdict.RuleResult{{Rule: "Slow"}, {Rule: "Late"}}}
		if err := NoData(context.Background(), onlyQueue{}, versions, rep, from, to, time.Minute); err != nil {
			t.Fatal(err)
		}
		by := map[string][]string{}
		for _, rr := range rep.Rules {
			by[rr.Rule] = rr.NoData
		}
		return by
	}
	typo := func(name, metric string) rules.Rule { return rules.Rule{Name: name, Expr: metric + " > 1"} }

	// Fixed in the second version: it read nothing only until then.
	by := run([]replay.Version{
		{From: from, Rules: []rules.Rule{typo("Slow", "queue_dept")}},
		{From: at(2), Rules: []rules.Rule{typo("Slow", "queue_depth")}},
	})
	if by["Slow"] != nil {
		t.Errorf("a version that read data: %q", by["Slow"])
	}
	// Both versions read nothing: the last one's expression.
	by = run([]replay.Version{
		{From: from, Rules: []rules.Rule{typo("Slow", "old_metric")}},
		{From: at(2), Rules: []rules.Rule{typo("Slow", "queue_dept"), typo("Late", "queue_dept")}},
	})
	if !slices.Equal(by["Slow"], []string{"queue_dept"}) {
		t.Errorf("every span without data: %q", by["Slow"])
	}
	// Late is only in the second version.
	if !slices.Equal(by["Late"], []string{"queue_dept"}) {
		t.Errorf("a rule of the last version only: %q", by["Late"])
	}
	// Two rules of one name: one reads data, so not NoData; both none, one
	// expression each, deduplicated.
	by = run([]replay.Version{{From: from, Rules: []rules.Rule{typo("Slow", "queue_dept"), typo("Slow", "queue_depth")}}})
	if by["Slow"] != nil {
		t.Errorf("one of two rules read data: %q", by["Slow"])
	}
	by = run([]replay.Version{{From: from, Rules: []rules.Rule{typo("Slow", "queue_dept"), typo("Slow", "queue_dept"), typo("Slow", "gone_total")}}})
	if !slices.Equal(by["Slow"], []string{"queue_dept", "gone_total"}) {
		t.Errorf("two rules without data: %q", by["Slow"])
	}
}
