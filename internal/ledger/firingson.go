package ledger

import (
	"context"
	"database/sql"
	"time"
)

// FiringsOn are the firings that were on at some time in [from, to):
// started before to, and not ended by from, those started more than back
// before from left out; the earliest first, limit of them at most. What an
// incident's timeline draws: those that caught it, which started in it or
// just before, come before any that started after them.
func (l *Ledger) FiringsOn(ctx context.Context, from, to time.Time, back time.Duration, limit int) ([]Firing, error) {
	rows, err := l.r.QueryContext(ctx, `SELECT f.id, r.name, s.id, s.labels, f.start_at, f.end_at, f.white, f.refired, f.excused, f.deploy
		FROM firings f JOIN series s ON s.id = f.series_id JOIN rules r ON r.id = f.rule_id
		WHERE f.start_at >= ? AND f.start_at < ? AND (f.end_at IS NULL OR f.end_at > ?)
		ORDER BY f.start_at, f.id LIMIT ?`, from.Add(-back).Unix(), to.Unix(), from.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Firing
	for rows.Next() {
		var f Firing
		var sid, start int64
		var end sql.NullInt64
		var labels string
		if err := rows.Scan(&f.ID, &f.Rule, &sid, &labels, &start, &end, &f.White, &f.Refired, &f.Excused, &f.Deploy); err != nil {
			return nil, err
		}
		if f.Labels, err = l.labels(sid, labels); err != nil {
			return nil, err
		}
		f.Start, f.End = unix(start), fromNull(end)
		out = append(out, f)
	}
	return out, rows.Err()
}
