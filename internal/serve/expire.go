package serve

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// Proposals that expire (docs/design/changes.md): with --proposals-expire,
// one nobody approves doesn't hold its rule for good.

// expireProposals closes, as expired, the proposals that waited for
// approval longer than --proposals-expire since they were made, that no one
// approved, a hundred a round at most: the leader's, each in a write of its
// own that moves it only while it still waits, noted in its history and
// the activity log as Lapilli's. Its rule is free for another. One being
// backtested again on this replica is left; one backtested on another,
// closed meanwhile, has its backtest refused as moved. Nothing without
// --proposals-expire.
func (s *Server) expireProposals(ctx context.Context) error {
	d := s.cfg.ProposalsExpire
	if d <= 0 {
		return nil
	}
	ids, err := s.l.WaitingSince(ctx, s.wall().Add(-d), 100)
	if err != nil {
		return err
	}
	ids = slices.DeleteFunc(ids, s.backtesting.has)
	why := fmt.Sprintf("it waited for approval longer than --proposals-expire, %s", daysText(d))
	for _, id := range ids {
		if !s.still(ctx) {
			return nil
		}
		_, err := s.l.Move(ctx, id, []string{ledger.StateProposed}, ledger.StateExpired, ledger.Proposer, ledger.Step{Reason: why})
		if err != nil && !errors.Is(err, ledger.ErrMoved) {
			return err
		}
	}
	return nil
}

// daysText is a duration as --proposals-expire is given one: 30d, or 36h.
func daysText(d time.Duration) string {
	if d >= 24*time.Hour && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	return verdict.Duration(d)
}

// inFlight are the proposals something is being done to on this replica,
// each counted as often as it is.
type inFlight struct {
	sync.Mutex
	n map[int64]int
}

// add counts id in, and gives what counts it out.
func (f *inFlight) add(id int64) func() {
	f.Lock()
	if f.n == nil {
		f.n = map[int64]int{}
	}
	f.n[id]++
	f.Unlock()
	return func() {
		f.Lock()
		if f.n[id]--; f.n[id] <= 0 {
			delete(f.n, id)
		}
		f.Unlock()
	}
}

// has says id is in.
func (f *inFlight) has(id int64) bool {
	f.Lock()
	defer f.Unlock()
	return f.n[id] > 0
}
