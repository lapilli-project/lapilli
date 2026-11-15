package serve

import (
	"context"
	"errors"
	"fmt"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// Proposals that expire (docs/design/changes.md): with --proposals-expire,
// one nobody approves doesn't hold its rule for good.

// expireProposals closes, as expired, the proposals that waited for
// approval longer than --proposals-expire, a hundred a round at most: the
// leader's, each in a write of its own that moves it only while it still
// waits, noted in its history and the activity log as Lapilli's. Its rule
// is free for another. One being backtested again waits all the same; the
// backtest, done, finds it closed (backtestStale backtests only one that
// waits). Nothing without --proposals-expire.
func (s *Server) expireProposals(ctx context.Context) error {
	d := s.cfg.ProposalsExpire
	if d <= 0 {
		return nil
	}
	ids, err := s.l.WaitingSince(ctx, s.wall().Add(-d), 100)
	if err != nil {
		return err
	}
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
