package serve

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/prom"
)

// flaky is a history that fails while down is set, and answers nothing
// after, counting its questions.
type flaky struct {
	down  atomic.Bool
	asked atomic.Int64
}

func (f *flaky) QueryRange(ctx context.Context, _ string, _, _ time.Time, _ time.Duration) ([]prom.Series, error) {
	f.asked.Add(1)
	if f.down.Load() {
		return nil, errors.New("history down")
	}
	return nil, nil
}

// A run that can't read the changes keeps the span it couldn't; the next run
// reads it once more with the firings the ledger has in it, and lets it go
// whether that worked or not.
func TestRolloutsReadAgain(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, func(c *Config) { c.Rollouts = true })
	h := &flaky{}
	s.h = h
	o, err := s.options(ctx)
	if err != nil {
		t.Fatal(err)
	}
	from, to := utc("2026-11-03T00:00:00Z"), utc("2026-11-03T06:00:00Z")
	fs := []firing.Firing{{Rule: "R", Labels: map[string]string{"namespace": "shop", "name": "a"}, Start: utc("2026-11-03T02:00:00Z"), End: utc("2026-11-03T02:30:00Z")}}
	if _, err := s.l.PutFirings(ctx, "cw", utc("2026-11-02T00:00:00Z"), utc("2026-11-09T00:00:00Z"), fs, "job", o); err != nil {
		t.Fatal(err)
	}
	h.down.Store(true)
	if r := s.readRollouts(ctx, from, to, fs); r != nil {
		t.Fatalf("a failed read gave %+v", r)
	}
	got, err := s.l.RolloutRetries(ctx)
	if err != nil || len(got) != 1 || !got[0][0].Equal(from) || !got[0][1].Equal(to) {
		t.Fatalf("the span kept: %v %v", got, err)
	}
	// Failing again: tried once, let go, and not tried a third time.
	asked := h.asked.Load()
	s.retryRollouts(ctx, o)
	if h.asked.Load() == asked {
		t.Error("the history wasn't asked again")
	}
	if got, _ := s.l.RolloutRetries(ctx); len(got) != 0 {
		t.Errorf("a span that failed twice is kept: %v", got)
	}
	asked = h.asked.Load()
	s.retryRollouts(ctx, o)
	if h.asked.Load() != asked {
		t.Error("a span let go was tried again")
	}
	// Read again with the history up: the span goes, its coverage kept.
	if err := s.l.RolloutFailed(ctx, from, to); err != nil {
		t.Fatal(err)
	}
	h.down.Store(false)
	s.retryRollouts(ctx, o)
	if got, _ := s.l.RolloutRetries(ctx); len(got) != 0 {
		t.Errorf("a span read again is kept: %v", got)
	}
	// Spans that overlap are one.
	for _, sp := range [][2]time.Time{{from, to}, {to.Add(-time.Hour), to.Add(time.Hour)}, {to.Add(48 * time.Hour), to.Add(49 * time.Hour)}} {
		if err := s.l.RolloutFailed(ctx, sp[0], sp[1]); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := s.l.RolloutRetries(ctx); len(got) != 2 || !got[0][1].Equal(to.Add(time.Hour)) {
		t.Errorf("overlapping spans: %v", got)
	}
}
