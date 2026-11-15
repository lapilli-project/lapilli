package ledger

import (
	"context"
	"database/sql"
	"errors"
)

// LastFailed is the latest run of a kind that failed, and whether there is
// one: read by the runs' index on when each started, from the latest back,
// as the runs are kept (--keep).
func (l *Ledger) LastFailed(ctx context.Context, kind string) (Run, bool, error) {
	var r Run
	var started, finished, from, to int64
	err := l.r.QueryRowContext(ctx, `SELECT id, kind, started, finished, period_from, period_to, error FROM runs
		WHERE kind = ? AND error != '' ORDER BY started DESC, id DESC LIMIT 1`, kind).Scan(&r.ID, &r.Kind, &started, &finished, &from, &to, &r.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, err
	}
	r.Started, r.Finished = unix(started), unix(finished)
	if from > 0 {
		r.From, r.To = unix(from), unix(to)
	}
	return r, true, nil
}
