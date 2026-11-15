package ledger

import (
	"context"
	"database/sql"
	"time"
)

// OutboxStanding is how the outbox stands, as the pages say it: when a
// notice was last sent, to any of the places asked of; how many of the
// unsent ones were made since a time and failed their last try (those given
// up on among them); and More, that a place has more unsent than
// FailedCounted, which were not looked through.
type OutboxStanding struct {
	LastSent time.Time
	Failed   int
	More     bool
}

// FailedCounted is how many unsent notices of a place Standing looks
// through at most: a receiver down for days, with --keep 0, leaves hundreds
// of thousands, and the pages that say it are worked out for each request.
const FailedCounted = 1000

// standingUnsent are a place's unsent notices, FailedCounted and one more at
// most, the latest to be tried first: read by the outbox's index on its
// place, when each was sent and when it is next tried (outbox_due), a range
// of it read from its end, so that what is read is bounded however many
// wait; whether each was made since a time and failed is looked at in those
// rows alone.
const standingUnsent = `SELECT COUNT(*), COUNT(CASE WHEN error != '' AND created >= ? THEN 1 END) FROM
	(SELECT error, created FROM outbox WHERE dest = ? AND sent_at IS NULL ORDER BY next_at DESC LIMIT ?) u`

// Standing is how the outbox stands for the places dests: for each, its
// last notice sent, one read by the outbox's index, and its unsent notices
// made since a time that failed, of FailedCounted of them at most
// (standingUnsent).
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
		var unsent, failed int
		if err := l.r.QueryRowContext(ctx, standingUnsent, since.Unix(), d, FailedCounted+1).Scan(&unsent, &failed); err != nil {
			return out, err
		}
		out.Failed += min(failed, FailedCounted)
		out.More = out.More || unsent > FailedCounted
	}
	return out, nil
}
