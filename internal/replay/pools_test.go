package replay

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
)

// held answers nothing until its context ends, as a history waiting out
// the Retry-After its API keeps saying does.
type held struct{ asked *atomic.Int64 }

func (h held) QueryRange(ctx context.Context, _ string, _, _ time.Time, _ time.Duration) ([]prom.Series, error) {
	h.asked.Add(1)
	<-ctx.Done()
	return nil, ctx.Err()
}

// heeding is a history that, as a real one does, answers nothing once its
// context has ended.
type heeding struct{ History }

func (h heeding) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return h.History.QueryRange(ctx, expr, start, end, step)
}

// TestPoolsApart: rules replayed against a history that holds them don't
// hold those of another history: each has its own --parallel.
func TestPoolsApart(t *testing.T) {
	var lokiAsked atomic.Int64
	promH := heeding{&points{name: "prometheus", v: func(t time.Time) (float64, bool) { return 1, t.Minute()%10 < 5 }}}
	h := WithLogs(promH, func(rules.Rule) (History, error) { return held{&lokiAsked}, nil })
	expr := `sum(count_over_time({app="shop"}[5m])) > 0`
	// The Loki rules ask the most, so go first.
	rs := []rules.Rule{{Name: "P1", Expr: "a > 0"}, {Name: "P2", Expr: "b > 0"},
		{Name: "L1", Expr: expr, For: 48 * time.Hour, Logs: rules.LokiRuler}, {Name: "L2", Expr: expr + " * 2", For: 48 * time.Hour, Logs: rules.LokiRuler}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var p Progress
	ran, _ := Run(Tracking(ctx, &p), h, []Version{{Rules: rs}}, t0, t0.Add(time.Hour), time.Minute, 1)
	got := map[string]string{}
	for _, r := range ran {
		got[r.Name] = r.Unsupported
	}
	if got["P1"] != "" || got["P2"] != "" || got["L1"] != Unfinished || got["L2"] != Unfinished || lokiAsked.Load() == 0 {
		t.Errorf("%q; Loki asked %d times", got, lokiAsked.Load())
	}
	if p.Total.Load() != 4 || p.Done.Load() != 4 {
		t.Errorf("progress: %d of %d", p.Done.Load(), p.Total.Load())
	}
}

// pastDeadline is a history whose every query is given up as a wait would
// end past the run's deadline, as prom gives it up.
type pastDeadline struct{}

func (pastDeadline) QueryRange(context.Context, string, time.Time, time.Time, time.Duration) ([]prom.Series, error) {
	return nil, fmt.Errorf("the API said to wait %w, after HTTP 429: slow down", prom.ErrPastDeadline)
}

// TestPastDeadlineUnfinished: a query given up as its wait would end past
// the deadline leaves its rule unfinished, as the deadline itself would,
// whatever the API answered before: lapilli verdict exits 1, saying to
// raise --timeout, and lapilli serve's MCP compare says so too.
func TestPastDeadlineUnfinished(t *testing.T) {
	ran, _ := Run(context.Background(), pastDeadline{}, []Version{{Rules: []rules.Rule{{Name: "A", Expr: "a > 0"}}}}, t0, t0.Add(time.Hour), time.Minute, 1)
	if ran[0].Unsupported != Unfinished {
		t.Errorf("%q", ran[0].Unsupported)
	}
}
