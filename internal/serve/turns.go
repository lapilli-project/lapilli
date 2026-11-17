package serve

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

// turns are how the asks of each named signal went, by kind (a run's
// read, the look at what it reads, the look for where its data begins),
// in this process: how long each took, whether it answered, and when it
// was last given all that was left of --timeout.
type turns struct {
	sync.Mutex
	// took is how long its last ask that answered took; of one that ran
	// out of time, at least what it was given.
	took map[string]time.Duration
	// turn is the round it was last given all that was left.
	turn   map[string]int64
	behind map[string]bool
	round  map[string]int64
}

// turnAsk is one signal's ask in a round: behind says its last didn't
// answer (a run's read missed, as the ledger keeps it).
type turnAsk struct {
	name   string
	behind bool
	ask    func(ctx context.Context) error
}

// errNoTurn is an ask left to a later round: its share of what is left of
// --timeout is less than it took before.
var errNoTurn = errors.New("its share of --timeout is less than it took before: left to a later round")

// inTurn asks each of asks within --timeout, so that one that hangs
// starves no other and one that fits in --timeout alone is never left
// behind for good (docs/design/signals.md, "Cost"):
//
//   - those whose last ask answered first, the quickest before, each with
//     its share of what is left, counting the one whose turn it is; one
//     that took more than its share before is left to a later round,
//     rather than run out its share for nothing;
//   - then, of those behind, the one given all that was left longest ago
//     (in asks' order, among those never given it): all that is left,
//     whatever it took before;
//   - then the others behind, each with what is still left, but one that
//     took more before.
//
// One that hangs runs out its share, falls behind, and has all that is
// left only on its turn, after every one that answered. One that needs
// more than its share is read on its turn, which comes round to each of
// those behind in turn. stop, of an ask's error, asks no more.
func (s *Server) inTurn(parent context.Context, kind string, asks []turnAsk, stop func(error) bool) []error {
	tu := &s.turns
	key := func(name string) string { return kind + "\x00" + name }
	tu.Lock()
	if tu.round == nil {
		tu.took, tu.turn, tu.behind, tu.round = map[string]time.Duration{}, map[string]int64{}, map[string]bool{}, map[string]int64{}
	}
	tu.round[kind]++
	round := tu.round[kind]
	var ahead, behind []int
	for i, a := range asks {
		if a.behind || tu.behind[key(a.name)] {
			behind = append(behind, i)
		} else {
			ahead = append(ahead, i)
		}
	}
	// By what each took, to 10ms: those alike keep asks' order.
	slices.SortStableFunc(ahead, func(x, y int) int {
		return cmp.Compare(tu.took[key(asks[x].name)]/(10*time.Millisecond), tu.took[key(asks[y].name)]/(10*time.Millisecond))
	})
	slices.SortStableFunc(behind, func(x, y int) int { return cmp.Compare(tu.turn[key(asks[x].name)], tu.turn[key(asks[y].name)]) })
	tu.Unlock()

	errs := make([]error, len(asks))
	deadline := time.Now().Add(s.cfg.Timeout)
	stopped := false
	ask := func(i int, share time.Duration, turn bool) {
		a := asks[i]
		k := key(a.name)
		tu.Lock()
		took, known := tu.took[k]
		tu.Unlock()
		switch {
		case stopped || parent.Err() != nil || share <= 0:
			errs[i] = context.DeadlineExceeded
		case known && took > share && !turn:
			// Its turn is asked whatever it took before: what hung may
			// answer now.
			errs[i] = errNoTurn
		default:
			ctx, cancel := context.WithTimeout(parent, share)
			start := time.Now()
			errs[i] = a.ask(ctx)
			took = time.Since(start)
			ranOut := ctx.Err() != nil
			cancel()
			tu.Lock()
			if errs[i] == nil {
				tu.took[k] = took
			} else if ranOut {
				tu.took[k] = max(tu.took[k], share)
			}
			tu.Unlock()
			if errs[i] != nil && stop != nil && stop(errs[i]) {
				stopped = true
			}
		}
		tu.Lock()
		tu.behind[k] = errs[i] != nil
		if turn {
			tu.turn[k] = round
		}
		tu.Unlock()
	}
	turn := 0
	if len(behind) > 0 {
		turn = 1
	}
	for n, i := range ahead {
		ask(i, time.Until(deadline)/time.Duration(len(ahead)-n+turn), false)
	}
	for n, i := range behind {
		ask(i, time.Until(deadline), n == 0)
	}
	return errs
}
