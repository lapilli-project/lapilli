package prom

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Limiter shares one API out among everything that asks it in a run, so
// that they back off together: at most so many requests at once, half as
// many each time the API says it is asked too much (HTTP 429, or 503), one
// more again for every recoverEvery it doesn't; none while the API said to
// wait (Retry-After); and, with a Pace, at most so many a second, spaced
// evenly.
type Limiter struct {
	mu sync.Mutex
	// limit is how many requests may be in flight now, inFlight how many
	// are, and queue those waiting for one to finish, first come first.
	limit, inFlight int
	queue           []chan struct{}
	// until is when the API said to ask again.
	until time.Time
	// cut is when limit was last halved, changed when it last changed.
	cut, changed time.Time
	// pace spaces the requests out, nil for no rate; several Limiters may
	// share one.
	pace *Pace
	now  func() time.Time
}

// Pace spaces requests evenly: at most so many a second, among all the
// Limiters given it. The data sources behind one Grafana each have a
// Limiter, as each pushes back of its own, and share one Pace, as
// --max-rate is of the Grafana.
type Pace struct {
	mu sync.Mutex
	// every is the time between two requests; next is when the next may
	// go.
	every time.Duration
	next  time.Time
}

const (
	// maxInFlight is the most requests a Limiter lets in at once: more than
	// a run asks for at once (--parallel is 64 at most), so it doesn't hold
	// a run back until the API pushes back.
	maxInFlight = 64
	// recoverEvery is how long the API must go without pushing back for one
	// more to be let in at once: from one at a time to eight, a little over
	// a minute, the window of a quota by the minute. Time without a request
	// counts: lapilli serve, back an hour later, asks as many at once as it
	// did before it was pushed back.
	recoverEvery = 10 * time.Second
	// cutEvery is how soon after it was halved the limit is halved again:
	// requests in flight together that are turned away together are one
	// push back, not several.
	cutEvery = time.Second
	// MinRate is the fewest requests a second a Pace takes: one every 1,000
	// seconds. Fewer would be past what a Duration holds.
	MinRate = 0.001
)

// NewPace is a Pace of rate requests a second at most, nil for a rate of 0
// or less, which is no rate; a rate under MinRate is MinRate.
func NewPace(rate float64) *Pace {
	if !(rate > 0) {
		return nil
	}
	return &Pace{every: time.Duration(float64(time.Second) / max(rate, MinRate))}
}

// NewLimiter is a Limiter of rate requests a second at most; 0 for no rate.
func NewLimiter(rate float64) *Limiter {
	return NewLimiterPaced(NewPace(rate))
}

// NewLimiterPaced is a Limiter spaced by p, which other Limiters may share;
// nil for no rate.
func NewLimiterPaced(p *Pace) *Limiter {
	return &Limiter{limit: maxInFlight, pace: p, now: func() time.Time { return clock() }}
}

// take waits for a request's turn: a place among those in flight, the end
// of a wait the API asked for, and its time by the pace. Each is looked at
// again on waking, as another answer may have said to wait longer
// meanwhile; a turn by the pace is taken only as the request goes, so one
// whose context ends first takes none. A request asked with a budget
// (retried) is pushed back from the first wait the API asked for, if not
// before. One whose turn can't come within its budget, or before its
// context's deadline, is given up then (errNoTurn): a wait isn't shortened.
// Once it returns nil, done must be called.
func (l *Limiter) take(ctx context.Context) error {
	if l == nil {
		return nil
	}
	b, _ := ctx.Value(budgetKey{}).(*budget)
	dl, hasDL := ctx.Deadline()
	if err := l.place(ctx, b); err != nil {
		return err
	}
	for {
		l.mu.Lock()
		now := l.now()
		at := l.until
		if at.After(now) && b != nil && b.from.IsZero() {
			b.from = now // pushed back from here
		}
		if !at.After(now) {
			var ok bool
			if at, ok = l.pace.take(now); ok {
				l.mu.Unlock()
				return nil
			}
		}
		l.mu.Unlock()
		if b != nil && !b.from.IsZero() && at.After(b.by()) || hasDL && at.After(dl) {
			l.done(0, 0) // its turn comes too late
			return errNoTurn
		}
		if err := sleep(ctx, at.Sub(now)); err != nil {
			l.done(0, 0) // the place given back
			return err
		}
	}
}

// place waits for a place among those in flight: within b's time, when it
// has been pushed back.
func (l *Limiter) place(ctx context.Context, b *budget) error {
	l.mu.Lock()
	l.recover(l.now())
	if l.inFlight < l.limit && len(l.queue) == 0 {
		l.inFlight++
		l.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	l.queue = append(l.queue, ch)
	var by <-chan time.Time
	if b != nil && !b.from.IsZero() {
		t := time.NewTimer(b.by().Sub(l.now()))
		defer t.Stop()
		by = t.C
	}
	l.mu.Unlock()
	var err error
	select {
	case <-ch: // let in: counted in flight by done
		return nil
	case <-ctx.Done():
		err = ctx.Err()
	case <-by:
		err = errNoTurn
	}
	l.mu.Lock()
	if i := slices.Index(l.queue, ch); i >= 0 {
		l.queue = slices.Delete(l.queue, i, i+1)
		l.mu.Unlock()
		return err
	}
	l.mu.Unlock()
	// Let in as it gave up: give the place back.
	l.done(0, 0)
	return err
}

// budget is how long a request asked again (retried) is asked again for:
// maxWaited from when it was first pushed back, by an answer or by a wait
// for its turn the API asked for; from is zero until then.
type budget struct{ from time.Time }

func (b *budget) by() time.Time { return b.from.Add(maxWaited) }

type budgetKey struct{}

// errNoTurn is a request whose turn wouldn't come within its budget, or
// before its context's deadline.
var errNoTurn = errors.New("the API said to wait past the time the request had")

// take takes the pace's next turn if it has come by now; otherwise it says
// when it comes.
func (p *Pace) take(now time.Time) (time.Time, bool) {
	if p == nil {
		return now, true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.next.After(now) {
		return p.next, false
	}
	p.next = now.Add(p.every)
	return now, true
}

// recover lets one more in at once for each recoverEvery since the limit
// last changed. l.mu is held.
func (l *Limiter) recover(now time.Time) {
	if l.limit >= maxInFlight {
		return
	}
	n := now.Sub(l.changed) / recoverEvery
	if n <= 0 {
		return
	}
	l.limit = int(min(int64(maxInFlight), int64(l.limit)+int64(n)))
	l.changed = l.changed.Add(n * recoverEvery)
}

// done ends a request let in by take: status is the API's answer (0 for
// none, or for one that shouldn't change how it is asked), wait how long it
// said to wait before asking again (Retry-After, capped), if it did.
func (l *Limiter) done(status int, wait time.Duration) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.inFlight--
	l.recover(now)
	if pushedBack(status) {
		// Half as many: the limit, not those in flight, so that one answer
		// to a request asked alone, a configuration read or a slow query,
		// doesn't leave the rest of the run one at a time.
		if now.Sub(l.cut) >= cutEvery {
			l.limit = max(1, l.limit/2)
			l.cut, l.changed = now, now
		}
		if wait > 0 && now.Add(wait).After(l.until) {
			l.until = now.Add(wait)
		}
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

// clock and sleep are what a Limiter and a request asked again tell the
// time and wait by; replaced in tests. sleep ends early, with ctx's error,
// when ctx ends.
var (
	clock = time.Now
	sleep = func(ctx context.Context, d time.Duration) error {
		if d <= 0 {
			return ctx.Err()
		}
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
)
