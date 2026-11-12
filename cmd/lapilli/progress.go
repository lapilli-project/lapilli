package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
)

// progressAfter is how long a run goes before it says how it is going, on
// stderr, and progressEvery how often it says so after: in CI, a run that
// prints nothing for minutes looks hung. Replaced in tests.
var progressAfter, progressEvery = 15 * time.Second, 30 * time.Second

// watch counts what the run asks the history and the rules it replays,
// into ctx, and, unless --progress=false, says on stderr how the run is
// going. finish stops it, says nothing more of how it was going, and says
// how often the history answered HTTP 429, if it did.
func (f *verdictFlags) watch(ctx context.Context, stderr io.Writer) (_ context.Context, finish func()) {
	var asked prom.Counts
	var replayed replay.Progress
	ctx = replay.Tracking(prom.Counting(ctx, &asked), &replayed)
	stop := func() {}
	if !f.noProgress {
		stop = showProgress(stderr, &replayed, &asked, time.Now())
	}
	return ctx, func() {
		stop()
		if n := asked.TooMany.Load(); n > 0 {
			fmt.Fprintf(stderr, "lapilli verdict: the history answered %d requests with HTTP 429, too many requests: each was asked again after a wait, fewer at once; --max-rate keeps under a quota\n", n)
		}
	}
}

// showProgress writes a line to w every progressEvery, once the run has
// gone progressAfter since start. stop ends it; nothing is written after.
func showProgress(w io.Writer, p *replay.Progress, c *prom.Counts, start time.Time) (stop func()) {
	quit, ended := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ended)
		t := time.NewTimer(progressAfter)
		defer t.Stop()
		for {
			select {
			case <-quit:
				return
			case <-t.C:
				fmt.Fprintln(w, progressLine(p, c, time.Since(start)))
				t.Reset(progressEvery)
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(quit); <-ended }) }
}

// progressLine says how a run is going: "lapilli verdict: after 2m30s, 23
// of 61 rules replayed, 412 requests to the history, 37 asked again after a
// wait, 35 answered HTTP 429".
func progressLine(p *replay.Progress, c *prom.Counts, took time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "lapilli verdict: after %s,", took.Round(time.Second))
	if total := p.Total.Load(); total > 0 {
		fmt.Fprintf(&b, " %d of %d rules replayed,", p.Done.Load(), total)
	}
	fmt.Fprintf(&b, " %d requests to the history", c.Requests.Load())
	if n := c.Retries.Load(); n > 0 {
		fmt.Fprintf(&b, ", %d asked again after a wait", n)
	}
	if n := c.TooMany.Load(); n > 0 {
		fmt.Fprintf(&b, ", %d answered HTTP 429", n)
	}
	return b.String()
}

// lockedWriter is a writer two goroutines can write to: stderr, by the run
// and by its progress line.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
