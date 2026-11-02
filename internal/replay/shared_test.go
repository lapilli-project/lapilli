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

func TestRunEach(t *testing.T) {
	// The rules in place and those proposed: Warn alike in both, Crit's
	// "for" changed, Old taken away and New added. Over one day.
	same := []rules.Rule{{Name: "Warn", Expr: "up", For: 5 * time.Minute}}
	before := []rules.Rule{{Name: "Crit", Expr: "up", For: 15 * time.Minute}, {Name: "Old", Expr: "other"}}
	after := []rules.Rule{{Name: "Crit", Expr: "up", For: 30 * time.Minute}, {Name: "New", Expr: "up > 1"}}
	h := &counting{calls: map[string]int{}}
	day := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	ran, fs := RunEach(context.Background(), h, [][]Version{one(same), one(before), one(after)},
		day, day.Add(24*time.Hour-time.Minute), time.Minute, 2)
	// up: the day, asked for once for all three sets, and each rule's own
	// look back over its "for" before the day.
	if h.calls["up"] != 4 || h.calls["other"] != 1 || h.calls["up > 1"] != 1 {
		t.Errorf("requests %v, want up 4 (the day shared by all three sets, 3 look-backs), other 1, up > 1 1", h.calls)
	}
	if len(ran) != 3 || len(ran[1]) != 2 || ran[2][1].Name != "New" {
		t.Fatalf("rules by set %+v", ran)
	}
	// Each set's firings are its own: Crit fires once in before and once in
	// after, at its own "for".
	for i, want := range [][]string{{"Warn"}, {"Crit", "Old"}, {"Crit", "New"}} {
		var got []string
		for _, f := range fs[i] {
			got = append(got, f.Rule)
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("set %d fired %v, want %v", i, got, want)
		}
	}
	alone, _ := Run(context.Background(), &counting{calls: map[string]int{}}, one(after), day, day.Add(24*time.Hour-time.Minute), time.Minute, 2)
	if len(alone) != 2 {
		t.Errorf("Run alone: %+v", alone)
	}
}
