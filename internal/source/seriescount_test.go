package source

import (
	"context"
	"math"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
)

// countHistory answers count(SEL) with the values given, a step apart,
// and keeps what it was asked.
type countHistory struct {
	values []float64
	asked  []string
	steps  []time.Duration
}

func (c *countHistory) QueryRange(_ context.Context, expr string, start, _ time.Time, step time.Duration) ([]prom.Series, error) {
	c.asked, c.steps = append(c.asked, expr), append(c.steps, step)
	s := prom.Series{}
	for i, v := range c.values {
		s.Samples = append(s.Samples, prom.Sample{T: start.Add(time.Duration(i) * step), V: v})
	}
	return []prom.Series{s}, nil
}

// The most series at once, from one count at an hour's step, or coarser
// over a long span; what isn't a count passed over; an empty span not
// asked.
func TestSeriesCount(t *testing.T) {
	lo := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	h := &countHistory{values: []float64{3, 12, math.NaN(), math.Inf(1), 1e300, 7}}
	n, err := SeriesCount(context.Background(), h, `up{job="api"}`, lo, lo.Add(7*24*time.Hour))
	if err != nil || n != 12 {
		t.Fatalf("got %d, %v; want 12", n, err)
	}
	if len(h.asked) != 1 || h.asked[0] != `count(up{job="api"})` || h.steps[0] != time.Hour {
		t.Errorf("asked %q at %v", h.asked, h.steps)
	}
	if _, err := SeriesCount(context.Background(), h, "up", lo, lo.Add(90*24*time.Hour)); err != nil || h.steps[1] != 130*time.Minute {
		t.Errorf("90 days: asked at %v, %v", h.steps, err)
	}
	if n, err := SeriesCount(context.Background(), h, "up", lo, lo); err != nil || n != 0 || len(h.asked) != 2 {
		t.Errorf("an empty span: %d, %v, asked %d", n, err, len(h.asked))
	}
}
