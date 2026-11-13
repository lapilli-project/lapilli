package prom

import (
	"context"
	"errors"
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func (l *Limiter) state() (limit, inFlight, queued int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limit, l.inFlight, len(l.queue)
}

// TestLimiterHalvesAndRecovers: a Limiter lets 64 in at once; each push
// back halves those in flight lately, or the limit, if fewer, once a second
// at most, down to one; each ten seconds without one lets one more in, time
// without a request included.
func TestLimiterHalvesAndRecovers(t *testing.T) {
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	l := NewLimiter(0)
	l.now = func() time.Time { return now }
	if limit, _, _ := l.state(); limit != 64 {
		t.Fatalf("limit %d at first, want 64", limit)
	}
	take := func(n int) {
		t.Helper()
		for range n {
			if err := l.take(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Eight at once, the default --parallel, one answered 429: half of the
	// eight, not of the limit, 32, which would hold none of them back.
	take(8)
	l.done(429, 0)
	if limit, in, _ := l.state(); limit != 4 || in != 7 {
		t.Fatalf("limit %d, %d in flight: want half of the 8 in flight", limit, in)
	}
	l.done(429, 0) // the same push back, answered to another
	l.done(503, 0)
	if limit, _, _ := l.state(); limit != 4 {
		t.Fatalf("limit %d: halved again within a second", limit)
	}
	for _, want := range []int{2, 1, 1, 1, 1} {
		now = now.Add(time.Second)
		l.done(503, 0)
		if limit, _, _ := l.state(); limit != want {
			t.Fatalf("limit %d, want %d, never 0", limit, want)
		}
	}
	if limit, in, _ := l.state(); limit != 1 || in != 0 {
		t.Fatalf("limit %d, %d in flight: want 1, none", limit, in)
	}
	// Recovering: one more each ten seconds, asked or not.
	for i, want := range []int{1, 2, 2, 3} {
		now = now.Add(5 * time.Second)
		take(1)
		l.done(200, 0)
		if limit, _, _ := l.state(); limit != want {
			t.Fatalf("after %ds: limit %d, want %d", 5*(i+1), limit, want)
		}
	}
	now = now.Add(25 * time.Second)
	if limit, _, _ := l.state(); limit != 3 {
		t.Fatalf("limit %d: changed without a request", limit)
	}
	take(1)
	if limit, _, _ := l.state(); limit != 5 {
		t.Fatalf("limit %d after 25s more, want 5", limit)
	}
	// An error answer, not a push back, neither halves nor recovers.
	l.done(400, 0)
	if limit, _, _ := l.state(); limit != 5 {
		t.Fatalf("limit %d after a 400, want 5", limit)
	}
	// An hour later, as lapilli serve's next run is: all of them again.
	now = now.Add(time.Hour)
	take(1)
	l.done(200, 0)
	if limit, _, _ := l.state(); limit != 64 {
		t.Errorf("an hour after a push back: limit %d, want 64", limit)
	}
	// Then one asked alone, answered 503: one at a time, as the API was
	// asked one, and back to eight at once in seventy seconds.
	take(1)
	l.done(503, 0)
	if limit, _, _ := l.state(); limit != 1 {
		t.Errorf("limit %d after a 503 to one asked alone, want 1", limit)
	}
	now = now.Add(70 * time.Second)
	take(1)
	if limit, _, _ := l.state(); limit != 8 {
		t.Errorf("limit %d seventy seconds after, want 8", limit)
	}
}

// TestPauseNotShortened: a wait one answer asks for isn't cut short by a
// shorter one asked for after it.
func TestPauseNotShortened(t *testing.T) {
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	l := NewLimiter(0)
	l.now = func() time.Time { return now }
	for range 2 {
		if err := l.take(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	l.done(429, 5*time.Second)
	l.done(503, time.Second)
	if l.until != now.Add(5*time.Second) {
		t.Errorf("waiting until %s from now, want 5s", l.until.Sub(now))
	}
}

// TestPauseLookedAtAgain: requests let in, sleeping for their turn by the
// rate or for the end of a wait the API asked for, keep to a wait another
// answer asks for meanwhile.
func TestPauseLookedAtAgain(t *testing.T) {
	l := NewLimiter(10) // every 100ms
	if err := l.take(context.Background()); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	went := make(chan time.Duration, 4)
	for range 4 {
		go func() {
			if err := l.take(context.Background()); err != nil {
				t.Error(err)
			}
			went <- time.Since(start)
			l.done(200, 0)
		}()
	}
	for _, in, _ := l.state(); in != 5; _, in, _ = l.state() {
		time.Sleep(time.Millisecond)
	}
	l.done(429, 600*time.Millisecond) // the first, answered 429
	// Meanwhile, the wait made longer.
	time.Sleep(300 * time.Millisecond)
	l.mu.Lock()
	l.inFlight++
	l.mu.Unlock()
	l.done(429, 900*time.Millisecond)
	for range 4 {
		if d := <-went; d < 1150*time.Millisecond {
			t.Errorf("a request went %s after the start, within the wait asked for", d)
		}
	}
}

// TestCancelledTakesNoTurn: a request whose context ends before its turn by
// the rate takes none, so the next isn't held for turns nobody used.
func TestCancelledTakesNoTurn(t *testing.T) {
	l := NewLimiter(2) // every 500ms
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.take(ctx); err == nil {
				l.done(200, 0)
			}
		}()
	}
	wg.Wait()
	start := time.Now()
	if err := l.take(context.Background()); err != nil {
		t.Fatal(err)
	}
	l.done(200, 0)
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the next request waited %s, for the turns of requests given up", took)
	}
}

// TestLetInAsGivenUp: a request let in just as its context ends gives its
// place back.
func TestLetInAsGivenUp(t *testing.T) {
	for range 2000 {
		l := NewLimiter(0)
		l.limit, l.changed = 1, time.Now()
		if err := l.take(context.Background()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		got := make(chan error)
		go func() { got <- l.take(ctx) }()
		for _, _, q := l.state(); q != 1; _, _, q = l.state() {
			runtime.Gosched()
		}
		go cancel()
		l.done(200, 0)
		if err := <-got; err == nil {
			l.done(200, 0)
		}
		if _, in, q := l.state(); in != 0 || q != 0 {
			t.Fatalf("%d in flight, %d waiting, after all gave back", in, q)
		}
	}
}

// TestPaceShared: Limiters sharing a Pace keep to its rate together, and
// each to its own waits: one told to wait doesn't hold another.
func TestPaceShared(t *testing.T) {
	p := NewPace(10) // every 100ms
	a, b := NewLimiterPaced(p), NewLimiterPaced(p)
	if err := a.take(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.done(429, time.Hour)
	if limit, _, _ := b.state(); limit != 64 {
		t.Errorf("b's limit %d, after a's push back", limit)
	}
	start := time.Now()
	for range 3 {
		if err := b.take(context.Background()); err != nil {
			t.Fatal(err)
		}
		b.done(200, 0)
	}
	if took := time.Since(start); took < 150*time.Millisecond || took > 10*time.Second {
		t.Errorf("b asked 3 in %s: want its turns by the rate, not a's wait", took)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := a.take(ctx); !errors.Is(err, ErrPastDeadline) || err.Error() != waitPast {
		t.Errorf("a, told to wait an hour: %v", err)
	}
}

// TestTinyRate: a rate too small for a Duration's nanoseconds is the
// smallest taken, not one that overflows.
func TestTinyRate(t *testing.T) {
	for _, rate := range []float64{1e-10, 1e-300, MinRate} {
		if p := NewPace(rate); p.every != 1000*time.Second {
			t.Errorf("rate %g: every %s, want 1000s", rate, p.every)
		}
	}
	if NewPace(0) != nil || NewPace(-1) != nil {
		t.Error("a rate of 0 or less is a pace")
	}
}

// TestLimiterQueues: a request waits for one in flight to finish, first
// come first; one whose context ends while waiting leaves the queue.
func TestLimiterQueues(t *testing.T) {
	l := NewLimiter(0)
	l.limit, l.changed = 1, time.Now()
	if err := l.take(context.Background()); err != nil {
		t.Fatal(err)
	}
	var order []int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.take(context.Background()); err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			l.done(200, 0)
		}()
		for _, _, q := l.state(); q != i+1; _, _, q = l.state() {
			time.Sleep(time.Millisecond)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	gave := make(chan error)
	go func() { gave <- l.take(ctx) }()
	for _, _, q := l.state(); q != 4; _, _, q = l.state() {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-gave; !errors.Is(err, context.Canceled) {
		t.Fatalf("%v, want the context's end", err)
	}
	if _, in, q := l.state(); in != 1 || q != 3 {
		t.Fatalf("%d in flight, %d waiting: want the one given up gone", in, q)
	}
	l.done(200, 0)
	wg.Wait()
	if len(order) != 3 || order[0] != 0 || order[1] != 1 || order[2] != 2 {
		t.Errorf("let in %v, want first come first", order)
	}
	if _, in, q := l.state(); in != 0 || q != 0 {
		t.Errorf("%d in flight, %d waiting at the end", in, q)
	}
}

// TestLimiterRate: at a rate, requests go evenly spaced, however many ask
// at once.
func TestLimiterRate(t *testing.T) {
	l := NewLimiter(20) // every 50ms
	start := time.Now()
	var wg sync.WaitGroup
	var times [6]time.Duration
	for i := range times {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.take(context.Background()); err != nil {
				t.Error(err)
				return
			}
			times[i] = time.Since(start)
			l.done(200, 0)
		}()
	}
	wg.Wait()
	last := time.Duration(0)
	for _, d := range times {
		last = max(last, d)
	}
	if last < 240*time.Millisecond {
		t.Errorf("6 requests at 20 a second all went within %s, want 250ms at least", last)
	}
}

// TestLimiterRateWithinContext: a request whose turn by the rate comes
// after its context's deadline is given up at once; one whose context ends
// while it waits for its turn gives its place back.
func TestLimiterRateWithinContext(t *testing.T) {
	l := NewLimiter(0.1) // every 10s
	if err := l.take(context.Background()); err != nil {
		t.Fatal(err)
	}
	l.done(200, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	// The rate's turn, not a wait the API asked for.
	if err := l.take(ctx); !errors.Is(err, ErrPastDeadline) || err.Error() != turnPast || time.Since(start) > 2*time.Second {
		t.Errorf("%v after %s", err, time.Since(start))
	}
	ctx, cancel = context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start = time.Now()
	if err := l.take(ctx); !errors.Is(err, context.Canceled) || time.Since(start) > 5*time.Second {
		t.Errorf("%v after %s", err, time.Since(start))
	}
	if _, in, _ := l.state(); in != 0 {
		t.Errorf("%d in flight", in)
	}
}

// TestLimiterNeverPastLimit: however many ask, and whatever they are
// answered, no more are in flight than the limit, and every place is given
// back.
func TestLimiterNeverPastLimit(t *testing.T) {
	l := NewLimiter(0)
	l.limit, l.changed = 3, time.Now()
	var in, most atomic.Int64
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(rand.IntN(50))*time.Millisecond)
			defer cancel()
			if i%7 == 0 {
				ctx = context.Background()
			}
			if err := l.take(ctx); err != nil {
				return
			}
			n := in.Add(1)
			limit, _, _ := l.state()
			if n > int64(maxInFlight) || n > int64(max(limit, 3)) {
				t.Errorf("%d in flight, limit %d", n, limit)
			}
			for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
			}
			time.Sleep(time.Millisecond)
			in.Add(-1)
			l.done([]int{200, 200, 429, 503, 500, 0}[i%6], 0)
		}()
	}
	wg.Wait()
	if _, n, q := l.state(); n != 0 || q != 0 {
		t.Errorf("%d in flight, %d waiting at the end", n, q)
	}
	if most.Load() > 3 {
		t.Errorf("%d in flight at once, limit 3", most.Load())
	}
}

// TestCapShared: Limiters within one cap, as a Grafana's data sources are,
// have no more in flight together than it lets in, each its own pauses and
// halving kept, and every place given back.
func TestCapShared(t *testing.T) {
	most := NewCap(3)
	a, b := NewLimiter(0).Within(most), NewLimiter(0).Within(most)
	for range 3 {
		if err := a.take(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := b.take(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("b let in past the cap: %v", err)
	}
	if _, in, _ := b.state(); in != 0 {
		t.Fatalf("b holds %d places, given up", in)
	}
	// a is pushed back: its limit halves, b's and the cap's don't.
	a.done(429, time.Hour)
	if limit, _, _ := most.state(); limit != 3 {
		t.Errorf("the cap is %d after a push back, want 3", limit)
	}
	if limit, _, _ := b.state(); limit != 64 {
		t.Errorf("b's limit %d after a's push back", limit)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.take(ctx); err != nil {
		t.Fatalf("b, in the place a gave back: %v", err)
	}
	if _, in, _ := most.state(); in != 3 {
		t.Errorf("%d in the cap, want 3", in)
	}
	a.done(200, 0)
	a.done(200, 0)
	b.done(200, 0)
	if _, in, _ := most.state(); in != 0 {
		t.Errorf("%d left in the cap", in)
	}
}

// TestCapNotHeldPaused: a request held by its own Limiter's wait, which the
// API asked for, holds no place in the cap: another data source's asks.
func TestCapNotHeldPaused(t *testing.T) {
	most := NewCap(1)
	a, b := NewLimiter(0).Within(most), NewLimiter(0).Within(most)
	a.mu.Lock()
	a.until = time.Now().Add(time.Hour)
	a.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.take(ctx) // sleeps out the hour, till the test ends
	for wait := time.Now().Add(5 * time.Second); time.Now().Before(wait); time.Sleep(time.Millisecond) {
		if _, in, _ := a.state(); in == 1 {
			break
		}
	}
	time.Sleep(20 * time.Millisecond)
	bctx, bcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer bcancel()
	if err := b.take(bctx); err != nil {
		t.Fatalf("b, beside a held by its wait: %v", err)
	}
	b.done(200, 0)
}

// TestPeakLately: what is halved is the most in flight since the limit
// last changed: those let in from the queue count, and those of before the
// limit recovered don't.
func TestPeakLately(t *testing.T) {
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	move := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	l := NewLimiter(0)
	l.now = clock
	// Eight at once, one pushed back: four.
	for range 8 {
		if err := l.take(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	l.done(429, 0)
	for range 7 {
		l.done(200, 0)
	}
	// Twenty seconds later, the limit back to six, one asked alone is
	// pushed back: one at a time, not half of the eight of before.
	move(20 * time.Second)
	if err := l.take(context.Background()); err != nil {
		t.Fatal(err)
	}
	move(time.Second)
	l.done(503, 0)
	if limit, _, _ := l.state(); limit != 1 {
		t.Fatalf("limit %d, want 1: halved from what was in flight before it recovered", limit)
	}
	// Four waiting for the one place; thirty seconds on, the limit is four,
	// and the four let in from the queue are in flight at once.
	if err := l.take(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if err := l.take(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	for _, _, q := l.state(); q != 4 && ctx.Err() == nil; _, _, q = l.state() {
		time.Sleep(time.Millisecond)
	}
	move(30 * time.Second)
	l.done(200, 0)
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	move(time.Second)
	l.done(429, 0)
	if limit, in, _ := l.state(); limit != 2 || in != 3 {
		t.Errorf("limit %d, %d in flight: want half the four let in from the queue", limit, in)
	}
	for range 3 {
		l.done(200, 0)
	}
}
