package prom

import (
	"context"
	"errors"
	"math/rand/v2"
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

// TestLimiterHalvesAndRecovers: each push back halves the requests let in
// at once, from those in flight, once a second at most; each ten seconds
// of answers without one lets one more in.
func TestLimiterHalvesAndRecovers(t *testing.T) {
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	l := NewLimiter(0)
	l.now = func() time.Time { return now }
	for range 8 {
		if err := l.take(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	l.done(429, 0)
	if limit, in, _ := l.state(); limit != 4 || in != 7 {
		t.Fatalf("limit %d, %d in flight: want half of the 8 that were", limit, in)
	}
	l.done(429, 0) // the same push back, answered to another
	l.done(503, 0)
	if limit, _, _ := l.state(); limit != 4 {
		t.Fatalf("limit %d: halved again within a second", limit)
	}
	now = now.Add(time.Second)
	l.done(503, 0)
	if limit, in, _ := l.state(); limit != 2 || in != 4 {
		t.Fatalf("limit %d, %d in flight: want 2", limit, in)
	}
	now = now.Add(time.Second)
	l.done(429, 0)
	l.done(200, 0)
	if limit, in, _ := l.state(); limit != 1 || in != 2 {
		t.Fatalf("limit %d, %d in flight: want 1, never 0", limit, in)
	}
	l.done(0, 0) // no answer: neither
	l.done(0, 0)
	if limit, in, _ := l.state(); limit != 1 || in != 0 {
		t.Fatalf("limit %d, %d in flight: want 1, none", limit, in)
	}
	// Recovering: one more each ten seconds of answers taken.
	for i, want := range []int{1, 2, 2, 3} {
		now = now.Add(5 * time.Second)
		if err := l.take(context.Background()); err != nil {
			t.Fatal(err)
		}
		l.done(200, 0)
		if limit, _, _ := l.state(); limit != want {
			t.Fatalf("after %ds of answers: limit %d, want %d", 5*(i+1), limit, want)
		}
	}
	// An error answer, not a push back, neither halves nor recovers.
	now = now.Add(time.Minute)
	if err := l.take(context.Background()); err != nil {
		t.Fatal(err)
	}
	l.done(400, 0)
	if limit, _, _ := l.state(); limit != 3 {
		t.Fatalf("limit %d after a 400, want 3", limit)
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
// after its context ends gives its place back.
func TestLimiterRateWithinContext(t *testing.T) {
	l := NewLimiter(0.1) // every 10s
	if err := l.take(context.Background()); err != nil {
		t.Fatal(err)
	}
	l.done(200, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := l.take(ctx); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
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
