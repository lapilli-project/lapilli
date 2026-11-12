package prom

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Limiter shares one API out among everything that asks it in a run, so
// that they back off together: at most so many requests at once, half as
// many each time the API says it is asked too much (HTTP 429, or 503), one
// more again for every recoverEvery it doesn't; none while the API said to
// wait (Retry-After); and, with a rate, at most that many a second, spaced
// evenly.
type Limiter struct {
	mu sync.Mutex
	// limit is how many requests may be in flight now, inFlight how many
	// are, and queue those waiting for one to finish, first come first.
	limit, inFlight int
	queue           []chan struct{}
	// every is the time between two requests, from the rate; 0 for no rate.
	// next is when the next may go by it.
	every time.Duration
	next  time.Time
	// until is when the API said to ask again.
	until time.Time
	// cut is when limit was last halved, changed when it last changed.
	cut, changed time.Time
	now          func() time.Time
}

const (
	// maxInFlight is the most requests a Limiter lets in at once: more than
	// a run asks for at once (--parallel is 64 at most), so it doesn't hold
	// a run back until the API pushes back.
	maxInFlight = 64
	// recoverEvery is how long the API must take requests without pushing
	// back for one more to be let in at once: from one at a time to eight,
	// a little over a minute, the window of a quota by the minute.
	recoverEvery = 10 * time.Second
	// cutEvery is how soon after it was halved the limit is halved again:
	// requests in flight together that are turned away together are one
	// push back, not several.
	cutEvery = time.Second
)

// NewLimiter is a Limiter of rate requests a second at most; 0 for no rate.
func NewLimiter(rate float64) *Limiter {
	l := &Limiter{limit: maxInFlight, now: time.Now}
	if rate > 0 {
		l.every = time.Duration(float64(time.Second) / rate)
	}
	return l
}

// take waits for a request's turn: a place among those in flight, its time
// by the rate, and the end of a wait the API asked for. Once it returns nil,
// done must be called.
func (l *Limiter) take(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	if l.inFlight < l.limit && len(l.queue) == 0 {
		l.inFlight++
		at := l.turn()
		l.mu.Unlock()
		return l.sleep(ctx, at)
	}
	ch := make(chan struct{})
	l.queue = append(l.queue, ch)
	l.mu.Unlock()
	select {
	case <-ch: // let in: counted in flight by done
	case <-ctx.Done():
		l.mu.Lock()
		if i := slices.Index(l.queue, ch); i >= 0 {
			l.queue = slices.Delete(l.queue, i, i+1)
			l.mu.Unlock()
			return ctx.Err()
		}
		l.mu.Unlock()
		// Let in as the context ended: give the place back.
		l.done(0, 0)
		return ctx.Err()
	}
	l.mu.Lock()
	at := l.turn()
	l.mu.Unlock()
	return l.sleep(ctx, at)
}

// turn is when a request let in may go: now, or when the API said to ask
// again, or its time by the rate, whichever is latest. l.mu is held.
func (l *Limiter) turn() time.Time {
	at := l.now()
	if l.until.After(at) {
		at = l.until
	}
	if l.every > 0 {
		if l.next.After(at) {
			at = l.next
		}
		l.next = at.Add(l.every)
	}
	return at
}

// sleep waits until at, or gives the place back when ctx ends first.
func (l *Limiter) sleep(ctx context.Context, at time.Time) error {
	d := at.Sub(l.now())
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		l.done(0, 0)
		return ctx.Err()
	}
}

// done ends a request let in by take: status is the API's answer (0 for
// none), wait how long it said to wait before asking again (Retry-After,
// capped), if it did.
func (l *Limiter) done(status int, wait time.Duration) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	was := l.inFlight
	l.inFlight--
	switch {
	case pushedBack(status):
		if now.Sub(l.cut) >= cutEvery {
			l.limit = max(1, min(l.limit, was)/2)
			l.cut, l.changed = now, now
		}
		if wait > 0 && now.Add(wait).After(l.until) {
			l.until = now.Add(wait)
		}
	case status/100 == 2 && l.limit < maxInFlight && now.Sub(l.changed) >= recoverEvery:
		l.limit++
		l.changed = now
	}
	for l.inFlight < l.limit && len(l.queue) > 0 {
		ch := l.queue[0]
		l.queue = l.queue[1:]
		l.inFlight++
		close(ch)
	}
}

// pushedBack says an answer asks for fewer requests: too many (429), or
// the API unable to take more just now (503).
func pushedBack(status int) bool {
	return status == 429 || status == 503
}

// Counts are what the history APIs of a run were asked, as a progress line
// tells it: requests sent, those asked again after an answer that said to
// (HTTP 429 or 5xx), and answers of HTTP 429.
type Counts struct {
	Requests, Retries, TooMany atomic.Int64
}

type countsKey struct{}

// Counting is ctx with counts, which every Client asked with it adds to.
func Counting(ctx context.Context, c *Counts) context.Context {
	return context.WithValue(ctx, countsKey{}, c)
}

func countsOf(ctx context.Context) *Counts {
	c, _ := ctx.Value(countsKey{}).(*Counts)
	return c
}
