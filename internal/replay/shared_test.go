package replay

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
)

func TestChunks(t *testing.T) {
	day := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	got := chunks(day.Add(-90*time.Minute), day.Add(25*time.Hour), time.Minute)
	want := [][2]time.Time{
		{day.Add(-90 * time.Minute), day.Add(-time.Minute)},
		{day, day.Add(24*time.Hour - time.Minute)},
		{day.Add(24 * time.Hour), day.Add(25 * time.Hour)},
	}
	if !slices.EqualFunc(got, want, func(a, b [2]time.Time) bool { return a[0].Equal(b[0]) && a[1].Equal(b[1]) }) {
		t.Errorf("chunks on the day grid: %v, want %v", got, want)
	}
	// Off the step grid: chunks count from the start instead.
	odd := day.Add(30 * time.Second)
	if got := chunks(odd, odd.Add(30*time.Hour), time.Minute); len(got) != 2 || !got[1][0].Equal(odd.Add(24*time.Hour)) {
		t.Errorf("chunks off the grid: %v", got)
	}
}

// counting counts requests, and serves a value of 1 every minute for any
// expression but "bad".
type counting struct {
	mu    sync.Mutex
	calls map[string]int
}

func (c *counting) QueryRange(_ context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	c.mu.Lock()
	c.calls[expr]++
	c.mu.Unlock()
	if expr == "bad" {
		return nil, errors.New("bad_data")
	}
	s := prom.Series{Labels: map[string]string{"job": "api"}}
	for t := start; !t.After(end); t = t.Add(step) {
		s.Samples = append(s.Samples, prom.Sample{T: t, V: 1})
	}
	return []prom.Series{s}, nil
}

func TestShared(t *testing.T) {
	// Warning and critical on the same query, with different "for": three
	// days, so four chunks with the lookback, and the same three after.
	rs := []rules.Rule{
		{Name: "Warn", Expr: "up", For: 5 * time.Minute},
		{Name: "Crit", Expr: "up", For: 15 * time.Minute},
		{Name: "Same", Expr: "up", For: 5 * time.Minute},
		{Name: "Broken", Expr: "bad"},
		{Name: "Other", Expr: "other"},
	}
	h := &counting{calls: map[string]int{}}
	day := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	ran, fs := Run(context.Background(), h, one(rs), day, day.Add(3*24*time.Hour-time.Minute), time.Minute, 2)
	// Warn and Same ask for the same four chunks; Crit's first starts
	// earlier, so it asks for that one itself.
	if h.calls["up"] != 5 || h.calls["other"] != 3 {
		t.Errorf("requests %v, want up 5 (4 shared, 1 of Crit's own) and other 3", h.calls)
	}
	if len(fs) != 4 {
		t.Errorf("firings %+v, want one each from Warn, Crit, Same and Other", fs)
	}
	if ran[3].Unsupported == "" {
		t.Error("Broken should be marked")
	}
}

func TestSharedLetsGo(t *testing.T) {
	sh := newShared(&counting{calls: map[string]int{}})
	r := request{"up", 0, 60}
	a, b := sh.expect([]request{r}), sh.expect([]request{r, r})
	ctx := context.Background()
	a.QueryRange(ctx, "up", time.Unix(0, 0), time.Unix(60, 0), time.Minute)
	if sh.calls[r] == nil || sh.held == 0 {
		t.Fatal("an answer still wanted twice was let go")
	}
	b.QueryRange(ctx, "up", time.Unix(0, 0), time.Unix(60, 0), time.Minute)
	b.done() // the second ask never came
	if len(sh.calls) != 0 || sh.held != 0 {
		t.Errorf("calls %v, held %d: want everything let go", sh.calls, sh.held)
	}
}
