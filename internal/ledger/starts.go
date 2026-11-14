package ledger

import (
	"context"
	"time"
)

// FiringStart is when a firing started, and its stones: what the firings
// of a day are counted by, an hour at a time.
type FiringStart struct {
	Start          time.Time
	White, Excused bool
}

// FiringStarts are when each firing q picks started, with its stones, the
// latest first: by q's span, rule and what it is (Black, Excused, Refired,
// Deploy), not by Before or Limit. Its labels aren't read.
func (l *Ledger) FiringStarts(ctx context.Context, q FiringQuery) ([]FiringStart, error) {
	sq := `SELECT f.start_at, f.white, f.excused FROM firings f WHERE f.start_at >= ? AND f.start_at < ?`
	args := []any{q.From.Unix(), q.To.Unix()}
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
	rows, err := l.r.QueryContext(ctx, sq+" ORDER BY f.start_at DESC, f.id DESC", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FiringStart
	for rows.Next() {
		var x FiringStart
		var at int64
		if err := rows.Scan(&at, &x.White, &x.Excused); err != nil {
			return nil, err
		}
		x.Start = unix(at)
		out = append(out, x)
	}
	return out, rows.Err()
}
