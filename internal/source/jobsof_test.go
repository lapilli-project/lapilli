package source

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
)

// jobsHistory answers count by (job) of a selector with the jobs it has,
// and fails the one it is told to.
type jobsHistory struct {
	jobs  map[string][]string
	fail  string
	asked []string
}

func (h *jobsHistory) QueryRange(_ context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	h.asked = append(h.asked, expr)
	if expr == JobsQuery(h.fail) {
		return nil, errors.New("refused")
	}
	var out []prom.Series
	for sel, js := range h.jobs {
		if expr != JobsQuery(sel) {
			continue
		}
		for _, j := range js {
			out = append(out, prom.Series{Labels: map[string]string{"job": j}, Samples: []prom.Sample{{T: end, V: 1}}})
		}
	}
	return out, nil
}

// Each selector asked once, its jobs sorted and each once; one that fails
// or gives none not among them; past MaxAsk, or the time, not asked.
func TestJobsOf(t *testing.T) {
	h := &jobsHistory{jobs: map[string][]string{"a": {"web", "batch", "web"}, "b": {}}, fail: "c"}
	at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	got, failed, notAsked := JobsOf(context.Background(), h, []string{"a", "b", "c"}, at)
	if len(got) != 1 || !slices.Equal(got["a"], []string{"batch", "web"}) || failed != 1 || notAsked != 0 || len(h.asked) != 3 ||
		h.asked[0] != `count by (job) (a)` {
		t.Errorf("%v %d %d %q", got, failed, notAsked, h.asked)
	}
	many := make([]string, MaxAsk+5)
	for i := range many {
		many[i] = "s"
	}
	if _, _, notAsked := JobsOf(context.Background(), &jobsHistory{}, many, at); notAsked != 5 {
		t.Errorf("past MaxAsk: %d not asked", notAsked)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, notAsked := JobsOf(ctx, &jobsHistory{}, []string{"a", "b"}, at); notAsked != 2 {
		t.Errorf("the time out: %d not asked", notAsked)
	}
}
