package ledger

import (
	"context"
	"database/sql"
	"time"
)

// OutboxStanding is how the outbox stands, as the pages say it: when a
// notice was last sent, to any of the places asked of; how many notices
// made since a time failed their last try and wait to be tried again, of
// the latest FailedCounted waiting a place has (More, that it has more
// waiting than that, tried or not); and how many made since then were given
// up on, after GiveUpAfter tries, of the latest FailedCounted given up
// (GivenUpMore, that all of those were made since then).
type OutboxStanding struct {
	LastSent    time.Time
	Failed      int
	More        bool
	GivenUp     int
	GivenUpMore bool
}

// FailedCounted is how many of a place's waiting notices, and of its given
// up, Standing looks through at most: a receiver down for days, with --keep
// 0, leaves hundreds of thousands, and the pages that say it are worked out
// for each request.
const FailedCounted = 1000

// givenUpAt is when a notice given up on is next tried: never (Failed).
const givenUpAt = int64(1) << 40

// standingWaiting and standingGivenUp are a place's unsent notices, those
// waiting to be tried and those given up on, FailedCounted and one more of
// each at most: each read by the outbox's index on its place, when each was
// sent and when it is next tried (outbox_due), a range of it read backward
// from its end, in the index's order, so that what is read is bounded
// however many there are, on SQLite and on Postgres alike (ordered by
// sent_at too, which a backward scan of the index gives; by next_at alone,
// Postgres read and sorted every unsent notice of the place). Whether each
// was made since a time, and failed, is looked at in those rows alone.
// Given up are the latest made first as the index keeps rows of one key:
// by rowid on SQLite, by their place in the table on Postgres.
const (
	standingWaiting = `SELECT COUNT(*), COUNT(CASE WHEN error != '' AND created >= ? THEN 1 END) FROM
	(SELECT error, created FROM outbox WHERE dest = ? AND sent_at IS NULL AND next_at < ? ORDER BY sent_at DESC, next_at DESC LIMIT ?) w`
	standingGivenUp = `SELECT COUNT(CASE WHEN created >= ? THEN 1 END) FROM
	(SELECT created FROM outbox WHERE dest = ? AND sent_at IS NULL AND next_at >= ? ORDER BY sent_at DESC, next_at DESC LIMIT ?) g`
)

// Standing is how the outbox stands for the places dests: for each, its
// last notice sent, one read by the outbox's index; its waiting notices
// made since a time that failed; and those given up on made since then
// (standingWaiting, standingGivenUp).
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
		var waiting, failed, givenUp int
		if err := l.r.QueryRowContext(ctx, standingWaiting, since.Unix(), d, givenUpAt, FailedCounted+1).Scan(&waiting, &failed); err != nil {
			return out, err
		}
		if err := l.r.QueryRowContext(ctx, standingGivenUp, since.Unix(), d, givenUpAt, FailedCounted+1).Scan(&givenUp); err != nil {
			return out, err
		}
		out.Failed += min(failed, FailedCounted)
		out.More = out.More || waiting > FailedCounted
		out.GivenUp += min(givenUp, FailedCounted)
		out.GivenUpMore = out.GivenUpMore || givenUp > FailedCounted
	}
	return out, nil
}
