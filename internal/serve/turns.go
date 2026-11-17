package serve

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// turns are how the asks of each named signal went, by kind (a run's
// read, the look at what it reads, the look for where its data begins),
// in this process.
type turns struct {
	sync.Mutex
	// took is how long its last ask that answered took (one piece's, of a
	// read catching up); of one that ran out of time, at least what it was
	// given, and on its turn twice that, up to --timeout.
	took map[string]time.Duration
	// turn is the round it last had its turn: all that was left, after
	// those that answered last time.
	turn   map[string]int64
	behind map[string]bool
	// outs is how many of its turns in a row it ran out of time on; due,
	// the round it is asked again from, after them.
	outs  map[string]int
	due   map[string]int64
	round map[string]int64
}

// maxTurnWait is how many rounds one that runs out of time on its turns
// waits at most to be asked again: a day of hourly runs, as a read again
// that fails waits.
const maxTurnWait = 24

// turnAsk is one signal's ask in a round: behind says its last didn't
// answer (a run's read missed, as the ledger keeps it). took, set by ask,
// is what it took of one piece, when it asks in pieces.
type turnAsk struct {
	name   string
	behind bool
	ask    func(ctx context.Context) error
	took   time.Duration
}

// errNoTurn is an ask left to a later round: its share of what is left of
// --timeout is less than it took before.
var errNoTurn = errors.New("its share of --timeout is less than it took before: left to a later round")

// errWaiting is an ask that ran out of time on its last turns, asked
// again from a later round.
type errWaiting int64

func (e errWaiting) Error() string {
	return fmt.Sprintf("it ran out of --timeout on its last turns: asked again in %d rounds", int64(e))
}

// inTurn asks each of asks within --timeout, so that one that hangs, or
// any number of them, starves no other, and one whose ask fits in
// --timeout alone is asked whole on its turn (docs/design/signals.md,
// "Cost"):
//
//   - those whose last ask answered first, the quickest before, each with
//     its share of what is left once the one whose turn it is has what it
//     took before (or, unknown, a share of its own); one that took more
//     than its share before is left to a later round, rather than run out
//     its share for nothing;
//   - then those behind: those that didn't run out on their last turn
//     before those that did, each group the one longest without a turn
//     first. The first has its turn: all that is left. The others have
//     what is still left.
//
// What is left at the end goes to those left to a later round for what
// they took before. One that runs out of time on its turn is asked again a
// round later,
// then two, four, up to maxTurnWait, and as before once it answers; until
// it waits maxTurnWait, what is kept for its turn is only what the others
// leave. stop, of an ask's error, asks no more.
func (s *Server) inTurn(parent context.Context, kind string, asks []turnAsk, stop func(error) bool) []error {
	tu := &s.turns
	key := func(name string) string { return kind + "\x00" + name }
	errs := make([]error, len(asks))
	tu.Lock()
	if tu.round == nil {
		tu.took, tu.turn, tu.behind, tu.outs, tu.due, tu.round =
			map[string]time.Duration{}, map[string]int64{}, map[string]bool{}, map[string]int{}, map[string]int64{}, map[string]int64{}
	}
	tu.round[kind]++
	round := tu.round[kind]
	var ahead, fresh, hung []int
	for i, a := range asks {
		k := key(a.name)
		switch {
		case !a.behind && !tu.behind[k]:
			ahead = append(ahead, i)
		case tu.outs[k] == 0:
			fresh = append(fresh, i)
		case round < tu.due[k]:
			errs[i] = errWaiting(tu.due[k] - round)
		default:
			hung = append(hung, i)
		}
	}
	// By what each took, to 10ms: those alike keep asks' order.
	slices.SortStableFunc(ahead, func(x, y int) int {
		return cmp.Compare(tu.took[key(asks[x].name)]/(10*time.Millisecond), tu.took[key(asks[y].name)]/(10*time.Millisecond))
	})
	byTurn := func(x, y int) int { return cmp.Compare(tu.turn[key(asks[x].name)], tu.turn[key(asks[y].name)]) }
	slices.SortStableFunc(fresh, byTurn)
	slices.SortStableFunc(hung, byTurn)
	behind := append(fresh, hung...)
	// What the one whose turn it is took before is kept for it, before
	// the shares of those that answered: of one that didn't run out on its
	// last turn; of one that did, only once it waits the longest between
	// turns, so that one that hangs takes no share of the others' but once
	// in maxTurnWait rounds, and one that hung and answers again, needing
	// more than they leave, is read then.
	var reserve time.Duration
	reserved, turnless := false, false
	if len(behind) > 0 {
		k := key(asks[behind[0]].name)
		if tu.outs[k] == 0 || 1<<min(tu.outs[k]-1, 16) >= maxTurnWait {
			reserve, reserved = tu.took[k]
			reserve = min(reserve, s.cfg.Timeout)
		} else {
			turnless = true
		}
	}
	tu.Unlock()

	deadline := time.Now().Add(s.cfg.Timeout)
	stopped := false
	ask := func(i int, share time.Duration, turn, spare bool) {
		a := &asks[i]
		k := key(a.name)
		tu.Lock()
		took, known := tu.took[k]
		tu.Unlock()
		ranOut := false
		switch {
		case stopped || parent.Err() != nil || share <= 0:
			errs[i] = context.DeadlineExceeded
		case known && took > share && !turn && !spare:
			// Its turn, and what is left at the end, are asked whatever it
			// took before: what hung may answer now.
			errs[i] = errNoTurn
		default:
			ctx, cancel := context.WithTimeout(parent, share)
			start := time.Now()
			a.took = 0
			errs[i] = a.ask(ctx)
			took = time.Since(start)
			if a.took > 0 {
				took = a.took
			}
			ranOut = errs[i] != nil && ctx.Err() != nil
			cancel()
			tu.Lock()
			switch {
			case errs[i] == nil:
				tu.took[k] = took
			case ranOut && turn:
				// All that was left wasn't enough: twice as much kept for
				// its next turn, up to --timeout.
				tu.took[k] = min(max(tu.took[k], 2*share), s.cfg.Timeout)
			case ranOut:
				tu.took[k] = max(tu.took[k], share)
			}
			tu.Unlock()
			if errs[i] != nil && stop != nil && stop(errs[i]) {
				stopped = true
			}
		}
		tu.Lock()
		defer tu.Unlock()
		tu.behind[k] = errs[i] != nil
		switch {
		case errs[i] == nil:
			tu.outs[k], tu.due[k] = 0, 0
		case turn && ranOut && (tu.outs[k] > 0 || share >= s.cfg.Timeout-s.cfg.Timeout/20):
			// It ran out with all of --timeout, or near it: it doesn't fit,
			// or hangs; or ran out again, having before. Run out with less,
			// first, what is kept for its next turn was doubled above.
			tu.outs[k]++
			tu.due[k] = round + int64(min(1<<min(tu.outs[k]-1, 16), maxTurnWait))
		}
		if turn {
			tu.turn[k] = round
		}
	}
	for n, i := range ahead {
		left := time.Until(deadline)
		var share time.Duration
		switch {
		case len(behind) == 0 || turnless:
			share = left / time.Duration(len(ahead)-n)
		case reserved:
			share = (left - reserve) / time.Duration(len(ahead)-n)
		default:
			share = left / time.Duration(len(ahead)-n+1)
		}
		ask(i, share, false, false)
	}
	for n, i := range behind {
		ask(i, time.Until(deadline), n == 0, false)
	}
	// What is left, which nothing else would use, to those left to a later
	// round for what they took before, the one longest without a turn
	// first: one that took long once, and answers at once now, isn't left
	// for good to rounds it hangs in.
	var skipped []int
	for i, err := range errs {
		if errors.Is(err, errNoTurn) {
			skipped = append(skipped, i)
		}
	}
	tu.Lock()
	slices.SortStableFunc(skipped, byTurn)
	tu.Unlock()
	for _, i := range skipped {
		errs[i] = nil
		ask(i, time.Until(deadline), false, true)
	}
	return errs
}

// rename moves what this process keeps of a signal's turns to its new
// name, as the ledger renames it in place (ledger.ChangeSignals): the
// same signal, which takes as long as it did.
func (tu *turns) rename(from, to string) {
	tu.Lock()
	defer tu.Unlock()
	for _, kind := range []string{"read", "seen", "data"} {
		a, b := kind+"\x00"+from, kind+"\x00"+to
		if v, ok := tu.took[a]; ok {
			tu.took[b] = v
			delete(tu.took, a)
		}
		if v, ok := tu.turn[a]; ok {
			tu.turn[b] = v
			delete(tu.turn, a)
		}
		if v, ok := tu.behind[a]; ok {
			tu.behind[b] = v
			delete(tu.behind, a)
		}
		if v, ok := tu.outs[a]; ok {
			tu.outs[b] = v
			delete(tu.outs, a)
		}
		if v, ok := tu.due[a]; ok {
			tu.due[b] = v
			delete(tu.due, a)
		}
		if v, ok := tu.round[a]; ok {
			tu.round[b] = v
			delete(tu.round, a)
		}
	}
}
