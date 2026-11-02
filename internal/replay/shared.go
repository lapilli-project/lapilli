package replay

import (
	"context"
	"sync"
	"time"

	"lapilli.dev/internal/prom"
)

// request is one query over one chunk of the period.
type request struct {
	expr       string
	start, end int64
}

// maxHeld is how many samples shared keeps for rules still to ask for them,
// about 300 MB; an answer that would go over it isn't kept, and is asked
// for again.
const maxHeld = 10_000_000

// shared lets rules that need the same query over the same day share one
// request. It is told beforehand who will ask for what, and keeps an
// answer only until the last of them has it.
type shared struct {
	h     History
	mu    sync.Mutex
	calls map[request]*call
	held  int
}

type call struct {
	wanted int           // asks still to come
	done   chan struct{} // closed once series and err are set; nil until asked
	series []prom.Series
	err    error
	kept   int // samples held for later asks
}

func newShared(h History) *shared {
	return &shared{h: h, calls: map[request]*call{}}
}

// expect says that one run will ask for these.
func (s *shared) expect(reqs []request) *asker {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := &asker{s: s, left: map[request]int{}}
	for _, r := range reqs {
		c := s.calls[r]
		if c == nil {
			c = &call{}
			s.calls[r] = c
		}
		c.wanted++
		a.left[r]++
	}
	return a
}

// asker is one run's way to the shared answers.
type asker struct {
	s    *shared
	mu   sync.Mutex
	left map[request]int // expected and not yet asked for
}

func (a *asker) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	r := request{expr, start.Unix(), end.Unix()}
	a.mu.Lock()
	planned := a.left[r] > 0
	if planned {
		a.left[r]--
	}
	a.mu.Unlock()
	if !planned {
		return a.s.h.QueryRange(ctx, expr, start, end, step)
	}
	return a.s.get(ctx, r, step)
}

// done gives up the asks the run expected but didn't make, as when it
// stopped at a query that failed.
func (a *asker) done() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for r, n := range a.left {
		for range n {
			a.s.mu.Lock()
			if c := a.s.calls[r]; c != nil {
				a.s.drop(r, c)
			}
			a.s.mu.Unlock()
		}
	}
	a.left = nil
}

// drop counts off one ask; the last lets the answer go. s.mu is held.
func (s *shared) drop(r request, c *call) {
	if c.wanted--; c.wanted == 0 {
		delete(s.calls, r)
		s.held -= c.kept
	}
}

func (s *shared) get(ctx context.Context, r request, step time.Duration) ([]prom.Series, error) {
	s.mu.Lock()
	c := s.calls[r]
	if c == nil { // let go to keep memory down: ask again
		s.mu.Unlock()
		return s.h.QueryRange(ctx, r.expr, time.Unix(r.start, 0), time.Unix(r.end, 0), step)
	}
	first := c.done == nil
	if first {
		c.done = make(chan struct{})
	}
	s.drop(r, c)
	s.mu.Unlock()
	if !first {
		select {
		case <-c.done:
			return c.series, c.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c.series, c.err = s.h.QueryRange(ctx, r.expr, time.Unix(r.start, 0), time.Unix(r.end, 0), step)
	close(c.done)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls[r] == c { // others will ask for it later
		n := 0
		for _, x := range c.series {
			n += len(x.Samples)
		}
		if s.held+n > maxHeld {
			delete(s.calls, r)
		} else {
			c.kept = n
			s.held += n
		}
	}
	return c.series, c.err
}
