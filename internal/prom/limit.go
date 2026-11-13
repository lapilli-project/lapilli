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
// many as were in flight lately each time the API says it is asked too
// much (HTTP 429, or 503, or a connection dropped), one more again for
// every recoverEvery it doesn't; none while the API said to wait
// (Retry-After); and, with a Pace, at most so many a second, spaced
// evenly. Within a parent (Within), a request takes a place in the
// parent's too.
type Limiter struct {
	mu sync.Mutex
	// limit is how many requests may be in flight now, most how many at
	// most, inFlight how many are, and queue those waiting for one to
	// finish, first come first. peak is the most in flight at once since
	// limit last changed.
	limit, most, inFlight, peak int
	queue                       []chan struct{}
	// until is when the API said to ask again.
	until time.Time
	// cut is when limit was last halved, changed when it last changed.
	cut, changed time.Time
	// pace spaces the requests out, nil for no rate; several Limiters may
	// share one.
	pace *Pace
	// parent, when set, holds a place for each request in flight too.
	parent *Limiter
	// fair, for a cap (NewCap), shares its places among its children.
	fair *fair
	now  func() time.Time
}

// fair is how a cap shares its places among the Limiters within it, the
// data sources behind one Grafana: one that holds a place already doesn't
// take the last one free while another holds none and asked lately (or
// came to be lately: within reserveFor), and a place given back goes to
// the one waiting that holds fewest, first come first among equals. A data
// source that answers slowly, as a broken or hostile one behind the
// Grafana can, holding each place as long as it likes, keeps the others
// out no longer than it takes another to have one; and one asked once and
// then no more keeps nothing back after reserveFor.
type fair struct {
	held    map[*Limiter]int
	asked   map[*Limiter]time.Time
	waiting []fairWait
	// wake is when places are shared again (share) for one waiting while
	// the last place is kept, as that ends then, not only when a place is
	// given back; zero for none.
	wake time.Time
}

// reserveFor is how long the last place is kept for a data source after it
// last asked, or came to be.
const reserveFor = 10 * time.Second

// afterFunc runs f after d, as time.AfterFunc does. Replaced in tests.
var afterFunc = func(d time.Duration, f func()) { time.AfterFunc(d, f) }

type fairWait struct {
	kid *Limiter
	ch  chan struct{}
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
	return &Limiter{limit: maxInFlight, most: maxInFlight, pace: p, now: func() time.Time { return clock() }}
}

// NewCap is a Limiter of n requests in flight at most, and no rate, which
// no answer changes: a parent (Within) for the Limiters of the data sources
// behind one Grafana, so that together they ask it no more at once than a
// run's --parallel, while each is held and halved by its own answers. It
// shares its places fairly among them (fair).
func NewCap(n int) *Limiter {
	n = max(n, 1)
	return &Limiter{limit: n, most: n, fair: &fair{held: map[*Limiter]int{}, asked: map[*Limiter]time.Time{}}, now: func() time.Time { return clock() }}
}

// Within makes each request l lets in take a place in parent too, once it
// has its turn in l: one held by l's wait, or by its rate, holds none of
// parent's. It returns l.
func (l *Limiter) Within(parent *Limiter) *Limiter {
	l.parent = parent
	if parent.fair != nil {
		parent.mu.Lock()
		parent.fair.held[l] += 0
		parent.fair.asked[l] = parent.now()
		parent.mu.Unlock()
	}
	return l
}

// placeFor waits for a place in a cap for a request of kid's.
func (l *Limiter) placeFor(ctx context.Context, kid *Limiter) error {
	if l.fair == nil {
		return l.place(ctx, nil)
	}
	ch := make(chan struct{})
	l.mu.Lock()
	l.fair.asked[kid] = l.now()
	l.fair.waiting = append(l.fair.waiting, fairWait{kid, ch})
	l.share()
	l.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
	}
	l.mu.Lock()
	if i := slices.IndexFunc(l.fair.waiting, func(w fairWait) bool { return w.ch == ch }); i >= 0 {
		l.fair.waiting = slices.Delete(l.fair.waiting, i, i+1)
		l.mu.Unlock()
		return ctx.Err()
	}
	l.mu.Unlock()
	l.releaseFor(kid) // let in as it gave up: the place given back
	return ctx.Err()
}

// releaseFor gives back a cap's place of a request of kid's.
func (l *Limiter) releaseFor(kid *Limiter) {
	if l.fair == nil {
		l.release(0, 0)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inFlight--
	l.fair.held[kid]--
	l.share()
}

// share lets in those waiting for a cap's places, as fair says. l.mu is
// held.
func (l *Limiter) share() {
	now := l.now()
	for l.inFlight < l.limit {
		// The last place is kept for another holding none, that asked
		// lately, while there is one: until the last of them asked
		// reserveFor ago.
		var until time.Time
		if l.limit-l.inFlight == 1 {
			for kid, held := range l.fair.held {
				if end := l.fair.asked[kid].Add(reserveFor); held == 0 && end.After(now) && end.After(until) {
					until = end
				}
			}
		}
		keep := !until.IsZero()
		best := -1
		for i, w := range l.fair.waiting {
			held := l.fair.held[w.kid]
			if held > 0 && keep {
				continue
			}
			if best < 0 || held < l.fair.held[l.fair.waiting[best].kid] {
				best = i
			}
		}
		if best < 0 {
			if keep && len(l.fair.waiting) > 0 {
				l.wakeAt(now, until)
			}
			return
		}
		w := l.fair.waiting[best]
		l.fair.waiting = slices.Delete(l.fair.waiting, best, best+1)
		l.inFlight++
		l.fair.held[w.kid]++
		l.peak = max(l.peak, l.inFlight)
		close(w.ch)
	}
}

// wakeAt shares places again at until, for those waiting while the last
// place is kept till then: none may give one back meanwhile, as a slow
// data source's requests hold theirs. l.mu is held.
func (l *Limiter) wakeAt(now, until time.Time) {
	if !l.fair.wake.IsZero() && !until.Before(l.fair.wake) {
		return // one is set, as soon
	}
	l.fair.wake = until
	afterFunc(until.Sub(now), func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.fair.wake.Equal(until) {
			l.fair.wake = time.Time{}
		}
		l.share()
	})
}

// take waits for a request's turn: a place among those in flight, the end
// of a wait the API asked for, and its time by the pace; then, within a
// parent, a place in the parent's. Each is looked at again on waking, as
// another answer may have said to wait longer meanwhile; a turn by the pace
// is taken only as the request goes, so one whose context ends first takes
// none. A request asked with a budget (retried) is pushed back from the
// first wait the API asked for, if not before. A wait isn't shortened: one
// whose turn comes past its budget is given up then (errNoTurn), and one
// whose turn comes past its context's deadline, which ends first, is given
// up then too (ErrPastDeadline). A request asked as one lapilli does
// without (bestEffort) doesn't wait out the API's wait at all (errPaused).
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
		at, paced := l.until, false
		if at.After(now) && b != nil && b.from.IsZero() {
			b.from = now // pushed back from here
		}
		if !at.After(now) {
			var ok bool
			if at, ok = l.pace.take(now); ok {
				l.mu.Unlock()
				return l.inParent(ctx)
			}
			paced = true
		}
		l.mu.Unlock()
		pastBudget := b != nil && !b.from.IsZero() && at.After(b.by())
		switch {
		case !paced && ctx.Value(noPauseKey{}) != nil:
			l.release(0, 0)
			return errPaused
		case hasDL && at.After(dl) && !(pastBudget && b.by().Before(dl)):
			// The run ends before its turn would come.
			l.release(0, 0)
			if paced {
				return &pastDeadline{why: turnPast}
			}
			return &pastDeadline{why: waitPast}
		case pastBudget:
			l.release(0, 0) // its turn comes too late
			return errNoTurn
		}
		if err := sleep(ctx, at.Sub(now)); err != nil {
			l.release(0, 0) // the place given back
			return err
		}
	}
}

// inParent takes a place in l's parent for a request l let in, or gives
// l's back when it can't have one.
func (l *Limiter) inParent(ctx context.Context) error {
	if l.parent == nil {
		return nil
	}
	if err := l.parent.placeFor(ctx, l); err != nil {
		l.release(0, 0)
		return err
	}
	return nil
}

// place waits for a place among those in flight: within b's time, when it
// has been pushed back.
func (l *Limiter) place(ctx context.Context, b *budget) error {
	l.mu.Lock()
	l.recover(l.now())
	if l.inFlight < l.limit && len(l.queue) == 0 {
		l.inFlight++
		l.peak = max(l.peak, l.inFlight)
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
	l.release(0, 0)
	return err
}

// budget is how long a request asked again (retried) is asked again for:
// maxWaited from when it was first pushed back, by an answer or by a wait
// for its turn the API asked for; from is zero until then.
type budget struct{ from time.Time }

func (b *budget) by() time.Time { return b.from.Add(maxWaited) }

type budgetKey struct{}

// errNoTurn is a request pushed back whose turn wouldn't come within its
// budget: as an answer that turned it away would, it fails the request
// for the run (retried gives that answer instead, if it had one).
var errNoTurn = errors.New("the API kept pushing back: no turn came within the five minutes a request is asked again")

// errPaused is a request lapilli does without (bestEffort), not asked
// while the API said to wait.
var errPaused = errors.New("not asked: the API said to wait")

type noPauseKey struct{}

// ErrPastDeadline is in the error of a request given up because a wait
// it was to make, the API's or its turn by the rate, would have ended past
// its context's deadline, before which it is given up instead: the run
// would have run out of time first. A replay takes it as one the deadline
// cut short.
var ErrPastDeadline = errors.New("past the deadline")

// pastDeadline is a request given up for ErrPastDeadline: why says which
// wait, last is the answer before it, if any.
type pastDeadline struct {
	why  string
	last error
}

// What would have waited past the deadline.
const (
	waitPast  = "the API said to wait past the deadline"
	againPast = "the wait to ask again would end past the deadline"
	turnPast  = "its turn by the rate (--max-rate) would come past the deadline"
)

func (e *pastDeadline) Error() string {
	if e.last == nil {
		return e.why
	}
	return e.why + ", after " + e.last.Error()
}

func (e *pastDeadline) Is(target error) bool { return target == ErrPastDeadline }
func (e *pastDeadline) Unwrap() error        { return e.last }

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
	if l.limit >= l.most {
		return
	}
	n := now.Sub(l.changed) / recoverEvery
	if n <= 0 {
		return
	}
	l.limit = int(min(int64(l.most), int64(l.limit)+int64(n)))
	l.changed = l.changed.Add(n * recoverEvery)
	l.peak = l.inFlight
}

// done ends a request let in by take: status is the API's answer (0 for
// none, or for one that shouldn't change how it is asked), wait how long it
// said to wait before asking again (Retry-After, capped), if it did.
func (l *Limiter) done(status int, wait time.Duration) {
	if l == nil {
		return
	}
	l.release(status, wait)
	if l.parent != nil {
		l.parent.releaseFor(l)
	}
}

// release gives back l's place of a request, as done says.
func (l *Limiter) release(status int, wait time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.inFlight--
	l.recover(now)
	if pushedBack(status) {
		// Half as many as were in flight lately, or as the limit, if fewer:
		// 8 at once, the default --parallel, are 4 after one push back,
		// not a limit of 32 that holds none of them back.
		if now.Sub(l.cut) >= cutEvery {
			l.limit = max(1, min(l.limit, l.peak)/2)
			l.cut, l.changed, l.peak = now, now, l.inFlight
		}
		if wait > 0 && now.Add(wait).After(l.until) {
			l.until = now.Add(wait)
		}
	}
	for l.inFlight < l.limit && len(l.queue) > 0 {
		ch := l.queue[0]
		l.queue = l.queue[1:]
		l.inFlight++
		l.peak = max(l.peak, l.inFlight)
		close(ch)
	}
}

// pushedBack says an answer asks for fewer requests: too many (429), or
// the API unable to take more just now (503); do takes a connection the
// API dropped as a 503.
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
