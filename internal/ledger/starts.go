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
	// Rule and Signals, of a count by them (FiringStartsBy): the rule's
	// name and the signals its firings' alerts are tied to.
	Rule    string
	Signals int64
}

// FiringStarts counts the firings q picks by the quarter of an hour, in
// UTC, each started in, the latest first: by q's span, rule and what it is
// (Black, Excused, Refired, Deploy, Kind), not by Before or Limit. The database
// counts them: a row a quarter, 96 a day, however many fired.
func (l *Ledger) FiringStarts(ctx context.Context, q FiringQuery) ([]StartCount, error) {
	return l.firingStarts(ctx, q, false)
}

// FiringStartsBy are FiringStarts, apart by rule and by the signals the
// firings' alerts are tied to: what judges a firing with named signals
// (FiringJudged). A row a quarter for each rule and set of signals that
// fired in it.
func (l *Ledger) FiringStartsBy(ctx context.Context, q FiringQuery) ([]StartCount, error) {
	return l.firingStarts(ctx, q, true)
}

func (l *Ledger) firingStarts(ctx context.Context, q FiringQuery, by bool) ([]StartCount, error) {
	const quarter = int64(Quarter / time.Second)
	sel, from, group := "", "firings f", ""
	if by {
		sel, from, group = ", r.name, s.signals", "firings f JOIN series s ON s.id = f.series_id JOIN rules r ON r.id = f.rule_id", ", r.name, s.signals"
	}
	sq := `SELECT f.start_at - f.start_at % ? AS q, count(*), count(CASE WHEN f.white <> 0 THEN 1 END),
		count(CASE WHEN f.white = 0 AND f.excused <> 0 THEN 1 END)` + sel + `
		FROM ` + from + ` WHERE f.start_at >= ? AND f.start_at < ?`
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
	if q.Kind != "" {
		sq += " AND f.marked != 0 AND (f.marked & (" + kindBit + ")) != 0"
		args = append(args, q.Kind)
	}
	sq, args = byResponse(sq, args, q)
	rows, err := l.r.QueryContext(ctx, sq+" GROUP BY q"+group+" ORDER BY q DESC"+group, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StartCount
	for rows.Next() {
		var x StartCount
		var at int64
		dest := []any{&at, &x.Firings, &x.White, &x.Excused}
		if by {
			dest = append(dest, &x.Rule, &x.Signals)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		x.Start = unix(at)
		out = append(out, x)
	}
	return out, rows.Err()
}
