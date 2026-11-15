package ledger

import (
	"context"
	"database/sql"
	"time"
)

// OutboxStanding is how the outbox stands, as the pages say it: when a
// notice was last sent, to any of the places asked of, and how many made
// since a time weren't sent, their last try having failed (those given up
// on among them).
type OutboxStanding struct {
	LastSent time.Time
	Failed   int
}

// Standing is how the outbox stands for the places dests: two reads a
// place, by the outbox's index on its place and when each was sent, however
// many notices it keeps.
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
		var n int
		if err := l.r.QueryRowContext(ctx, "SELECT COUNT(*) FROM outbox WHERE dest = ? AND sent_at IS NULL AND error != '' AND created >= ?", d, since.Unix()).Scan(&n); err != nil {
			return out, err
		}
		out.Failed += n
	}
	return out, nil
}
