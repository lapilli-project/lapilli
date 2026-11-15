package replay

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
)

// series is a history whose every query answers n series, each holding
// over the whole range.
type series int

func (n series) QueryRange(_ context.Context, _ string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	out := make([]prom.Series, n)
	for i := range out {
		out[i].Labels = map[string]string{"instance": strconv.Itoa(i)}
		for t := start; !t.After(end); t = t.Add(step) {
			out[i].Samples = append(out[i].Samples, prom.Sample{T: t, V: 1})
		}
	}
	return out, nil
}

// TestReplayWide: a replayed rule's firings each carry a copy of its labels
// with their series' own, and a rule whose labels on each of its series
// would take past what a range's samples are held to (--max-samples) isn't
// replayed, saying why: 9,999 labels on 1,000 series made 1.3 GB, its
// labels from whoever writes rules. One of a dozen labels on the same
// series is replayed.
func TestReplayWide(t *testing.T) {
	ctx := context.Background()
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(10 * time.Minute)
	labels := func(n int) map[string]string {
		l := map[string]string{}
		for i := range n {
			l[fmt.Sprintf("l%04d", i)] = "v"
		}
		return l
	}
	wide := rules.Rule{Name: "Wide", Group: "g", Expr: "up", Labels: labels(9999)}
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	ran, fs := Run(ctx, series(1000), []Version{{Rules: []rules.Rule{wide}}}, from, to, time.Minute, 1)
	runtime.ReadMemStats(&b)
	t.Logf("9,999 labels on 1,000 series: %d MB allocated: %s", (b.TotalAlloc-a.TotalAlloc)>>20, ran[0].Unsupported)
	if len(fs) != 0 || !strings.HasPrefix(ran[0].Unsupported, rules.QueryFailed) || !strings.Contains(ran[0].Unsupported, "9999 labels") {
		t.Errorf("replayed: %d firings, %q", len(fs), ran[0].Unsupported)
	}
	if got := b.TotalAlloc - a.TotalAlloc; got > 256<<20 {
		t.Errorf("%d MB allocated", got>>20)
	}
	if _, err := Held(ctx, series(1000), wide, from, to, time.Minute); err == nil || !strings.Contains(err.Error(), "--max-samples") {
		t.Errorf("held: %v", err)
	}
	if _, _, err := Fired(ctx, series(1000), wide, from, to, time.Minute); err == nil {
		t.Error("fired: no error")
	}

	real := rules.Rule{Name: "Real", Group: "g", Expr: "up", Labels: labels(12)}
	ran, fs = Run(ctx, series(1000), []Version{{Rules: []rules.Rule{real}}}, from, to, time.Minute, 1)
	if ran[0].Unsupported != "" || len(fs) != 1000 {
		t.Errorf("a dozen labels: %d firings, %q", len(fs), ran[0].Unsupported)
	}
}
