package prom

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
)

// Yield is lapilli serve's yielding to the history APIs it shares with the
// alerts (--yield-on-429). Once a kind of API (APIs) answers a request
// asked with the Yield (Yielding) with HTTP 429, too many requests, the
// requests asked with it ask that API nothing more, and not again after a
// wait: one waiting for its turn gives up at once, one not yet asked isn't,
// and one already sent is let finish. Each fails with a *Yielded. The API
// is asked again from the next Begin once For has gone by.
type Yield struct {
	// For is how long after a 429 the API is asked nothing, at least until
	// the next Begin. Now tells the time of a 429: time.Now when nil.
	For time.Duration
	Now func() time.Time

	mu   sync.Mutex
	apis [nAPIs]yielding
}

// yielding is one kind of API's: at is when it answered 429, zero while it
// isn't yielded to; stop ends at that 429, ending the waits for a turn.
type yielding struct {
	at     time.Time
	stop   context.Context
	cancel context.CancelFunc
}

// Yielded is the error of a request not asked, or not asked again, because
// its run yielded to the API (Yield).
type Yielded struct {
	// API is the kind of API, one of APIs.
	API string
}

func (e *Yielded) Error() string {
	return "the " + e.API + " API answered HTTP 429, too many requests: lapilli yielded to it, and asks it again in a later run " + yieldedMark
}

// yieldedMark ends what a *Yielded says.
const yieldedMark = "(--yield-on-429)"

// IsYieldedReason reports whether a reason a rule couldn't be replayed, as
// a ledger keeps it, is a *Yielded's.
func IsYieldedReason(reason string) bool { return strings.Contains(reason, yieldedMark) }

// IsYielded reports whether err is a request's that its run yielded.
func IsYielded(err error) bool {
	var y *Yielded
	return errors.As(err, &y)
}

type yieldKey struct{}

// Yielding is ctx with y: its requests yield as y says.
func Yielding(ctx context.Context, y *Yield) context.Context {
	return context.WithValue(ctx, yieldKey{}, y)
}

func yieldOf(ctx context.Context) *Yield {
	y, _ := ctx.Value(yieldKey{}).(*Yield)
	return y
}

// Begin starts a run: the APIs yielded to whose For has gone by are asked
// again.
func (y *Yield) Begin(now time.Time) {
	y.mu.Lock()
	defer y.mu.Unlock()
	for i := range y.apis {
		a := &y.apis[i]
		if !a.at.IsZero() && !now.Before(a.at.Add(y.For)) {
			a.at, a.stop, a.cancel = time.Time{}, nil, nil
		}
	}
}

// Yielded are the APIs yielded to now, in the order of APIs.
func (y *Yield) Yielded() []string {
	y.mu.Lock()
	defer y.mu.Unlock()
	var out []string
	for i, a := range y.apis {
		if !a.at.IsZero() {
			out = append(out, APIs[i])
		}
	}
	return out
}

// Until is when api, yielded to, is asked again at the earliest: at the
// first Begin from then on. Zero when it isn't yielded to.
func (y *Yield) Until(api string) time.Time {
	i := slices.Index(APIs, api)
	if i < 0 {
		return time.Time{}
	}
	y.mu.Lock()
	defer y.mu.Unlock()
	if y.apis[i].at.IsZero() {
		return time.Time{}
	}
	return y.apis[i].at.Add(y.For)
}

// turn is the context a request to api waits for its turn in: ctx, ended
// as well once api is yielded to; and done, to call once it has its turn.
// err is a *Yielded when api is yielded to already.
func (y *Yield) turn(ctx context.Context, api string) (_ context.Context, done func(), err error) {
	i := slices.Index(APIs, api)
	if y == nil || i < 0 {
		return ctx, func() {}, nil
	}
	y.mu.Lock()
	a := &y.apis[i]
	if !a.at.IsZero() {
		y.mu.Unlock()
		return ctx, func() {}, &Yielded{API: api}
	}
	if a.stop == nil {
		a.stop, a.cancel = context.WithCancel(context.Background())
	}
	stop := a.stop
	y.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	unhook := context.AfterFunc(stop, cancel)
	return ctx, func() { unhook(); cancel() }, nil
}

// yield yields to api, which answered 429: it reports whether it wasn't
// yielded to already.
func (y *Yield) yield(api string) bool {
	i := slices.Index(APIs, api)
	if i < 0 {
		return false
	}
	now := time.Now()
	if y.Now != nil {
		now = y.Now()
	}
	y.mu.Lock()
	defer y.mu.Unlock()
	a := &y.apis[i]
	if !a.at.IsZero() {
		return false
	}
	a.at = now
	if a.cancel != nil {
		a.cancel()
	}
	return true
}

// held is err, or, when ctx's Yield has yielded to api, a *Yielded: a
// request that gave up its wait for a turn as the API was yielded to.
func held(ctx context.Context, api string, err error) error {
	if y := yieldOf(ctx); y != nil && slices.Contains(y.Yielded(), api) {
		return &Yielded{API: api}
	}
	return err
}

// Paces are a Pace for each kind of API (Pacing).
type Paces [nAPIs]*Pace

// NewPaces are Paces of rate requests a second at most each; nil for a
// rate of 0 or less, which is none.
func NewPaces(rate float64) *Paces {
	if !(rate > 0) {
		return nil
	}
	var p Paces
	for i := range p {
		p[i] = NewPace(rate)
	}
	return &p
}

type pacesKey struct{}

// Pacing is ctx whose requests ask each kind of API (APIs) at p's rate at
// most, spaced evenly, besides any Limiter's rate: lapilli serve's
// catch-up (--catch-up-rate), one p for all its runs. ctx itself for p
// nil.
func Pacing(ctx context.Context, p *Paces) context.Context {
	if p == nil {
		return ctx
	}
	return context.WithValue(ctx, pacesKey{}, p)
}

// catchUpPast is a request given up as its turn by the catch-up's rate
// would come past its deadline.
const catchUpPast = "its turn by the catch-up's rate (--catch-up-rate) would come past the deadline"

// paced waits for a request's turn to api by ctx's Pacing, if it has one;
// one whose turn would come past ctx's deadline is given up then
// (ErrPastDeadline), as the run would end first.
func paced(ctx context.Context, api string) error {
	p, _ := ctx.Value(pacesKey{}).(*Paces)
	i := slices.Index(APIs, api)
	if p == nil || i < 0 {
		return nil
	}
	dl, hasDL := ctx.Deadline()
	for {
		now := clock()
		at, ok := p[i].take(now)
		if ok {
			return nil
		}
		if hasDL && at.After(dl) {
			return &pastDeadline{why: catchUpPast}
		}
		if err := sleep(ctx, at.Sub(now)); err != nil {
			return err
		}
		countWait(ctx, at.Sub(now), true)
	}
}
