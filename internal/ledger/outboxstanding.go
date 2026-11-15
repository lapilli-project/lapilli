package ledger

import (
	"context"
	"database/sql"
	"time"
)

// OutboxStanding is how the outbox stands, as the pages say it: when a
// notice was last sent, to any of the places asked of, and how many made
// since a time weren't sent, their last try having failed (those given up
// on among them): FailedCounted at most, More when there were more.
type OutboxStanding struct {
	LastSent time.Time
	Failed   int
	More     bool
}

// FailedCounted is how many failed notices Standing counts at most: the
// count reads the unsent notices of a place, and a receiver down for days,
// with --keep 0, leaves hundreds of thousands, which the pages that say it
// read on each request.
const FailedCounted = 1000

// Standing is how the outbox stands for the places dests: for each, its
// last notice sent, one read by the outbox's index on its place and when
// each was sent, and its failed notices made since a time, FailedCounted at
// most, read by the same index as far as that.
func (l *Ledger) Standing(ctx context.Context, dests []string, since time.Time) (OutboxStanding, error) {
	var out OutboxStanding
	for _, d := range dests {
		var sent sql.NullInt64
		if err := l.r.QueryRowContext(ctx, "SELECT MAX(sent_at) FROM outbox WHERE dest = ?", d).Scan(&sent); err != nil {
			return out, err
		}
		if at := fromNull(sent); at.After(out.LastSent) {
			out.LastSent = at
		}
		if out.More {
			continue
		}
		var n int
		if err := l.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT 1 FROM outbox
			WHERE dest = ? AND sent_at IS NULL AND error != '' AND created >= ? LIMIT ?) f`, d, since.Unix(), FailedCounted+1-out.Failed).Scan(&n); err != nil {
			return out, err
		}
		if out.Failed += n; out.Failed > FailedCounted {
			out.Failed, out.More = FailedCounted, true
		}
	}
	return out, nil
}
