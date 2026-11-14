package ledger

import (
	"context"
	"time"
)

// Quarter is how long the span FiringStarts counts the firings in is: a
// quarter of an hour, which every zone's offset from UTC is a whole number
// of, so that each falls in one hour of the clock, wherever it is read.
const Quarter = 15 * time.Minute

// StartCount is the firings that started in a quarter of an hour from
// Start: Firings of them, White those that caught something, and Excused,
// of the rest, those a quiet window excused. What the firings of a day are
// counted by, an hour at a time.
type StartCount struct {
	Start                   time.Time
	Firings, White, Excused int
}

// FiringStarts counts the firings q picks by the quarter of an hour, in
// UTC, each started in, the latest first: by q's span, rule and what it is
// (Black, Excused, Refired, Deploy), not by Before or Limit. The database
// counts them: a row a quarter, 96 a day, however many fired.
func (l *Ledger) FiringStarts(ctx context.Context, q FiringQuery) ([]StartCount, error) {
	const quarter = int64(Quarter / time.Second)
	sq := `SELECT f.start_at - f.start_at % ? AS q, count(*), count(CASE WHEN f.white <> 0 THEN 1 END),
		count(CASE WHEN f.white = 0 AND f.excused <> 0 THEN 1 END)
		FROM firings f WHERE f.start_at >= ? AND f.start_at < ?`
	args := []any{quarter, q.From.Unix(), q.To.Unix()}
	if q.Rule != "" {
		sq += " AND f.rule_id = (SELECT id FROM rules WHERE name = ?)"
		args = append(args, q.Rule)
	}
	if q.Black {
		sq += " AND f.white = 0 AND f.excused = 0"
	}
	if q.Excused {
		sq += " AND f.excused = 1"
	}
	if q.Refired {
		sq += " AND f.refired = 1 AND f.excused = 0"
	}
	if q.Deploy {
		sq += " AND f.deploy = 1"
	}
	rows, err := l.r.QueryContext(ctx, sq+" GROUP BY q ORDER BY q DESC", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StartCount
	for rows.Next() {
		var x StartCount
		var at int64
		if err := rows.Scan(&at, &x.Firings, &x.White, &x.Excused); err != nil {
			return nil, err
		}
		x.Start = unix(at)
		out = append(out, x)
	}
	return out, rows.Err()
}
