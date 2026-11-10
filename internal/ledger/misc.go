package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"lapilli.dev/internal/verdict"
)

// Setting is a judging setting changed through the UI.
type Setting struct {
	Value   string
	Updated time.Time
	By      string
}

// Settings are the judging settings changed through the UI, by name.
func (l *Ledger) Settings(ctx context.Context) (map[string]Setting, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT key, value, updated, updated_by FROM settings")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Setting{}
	for rows.Next() {
		var k string
		var s Setting
		var at int64
		if err := rows.Scan(&k, &s.Value, &at, &s.By); err != nil {
			return nil, err
		}
		s.Updated = unix(at)
		out[k] = s
	}
	return out, rows.Err()
}

// SetSettings changes judging settings; an empty value goes back to the
// one the command line gives.
func (l *Ledger) SetSettings(ctx context.Context, values map[string]string, by string) error {
	return l.write(ctx, func(t *txn) error {
		now := time.Now().Unix()
		for k, v := range values {
			var was string
			err := t.QueryRowContext(t.ctx, "SELECT value FROM settings WHERE key = ?", k).Scan(&was)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			if was == v {
				continue
			}
			if v == "" {
				_, err = t.ExecContext(t.ctx, "DELETE FROM settings WHERE key = ?", k)
			} else {
				_, err = t.ExecContext(t.ctx, `INSERT INTO settings (key, value, updated, updated_by) VALUES (?, ?, ?, ?)
					ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated = excluded.updated, updated_by = excluded.updated_by`,
					k, v, now, by)
			}
			if err != nil {
				return err
			}
			if err := change(t, by, "setting "+k, was, v); err != nil {
				return err
			}
		}
		return nil
	})
}

// Change is a change made through the UI or API.
type Change struct {
	At            time.Time
	By, What      string
	Before, After string
}

// Changes are the latest changes, the latest first.
func (l *Ledger) Changes(ctx context.Context, limit int) ([]Change, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT at, by, what, before, after FROM changes ORDER BY at DESC, id DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var c Change
		var at int64
		if err := rows.Scan(&at, &c.By, &c.What, &c.Before, &c.After); err != nil {
			return nil, err
		}
		c.At = unix(at)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Runs are the latest runs, the latest first.
func (l *Ledger) Runs(ctx context.Context, limit int) ([]Run, error) {
	rows, err := l.r.QueryContext(ctx, `SELECT id, kind, started, finished, period_from, period_to, firings, failed, error
		FROM runs ORDER BY started DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var started, finished, from, to int64
		var failed string
		if err := rows.Scan(&r.ID, &r.Kind, &started, &finished, &from, &to, &r.Firings, &failed, &r.Error); err != nil {
			return nil, err
		}
		r.Started, r.Finished = unix(started), unix(finished)
		if from > 0 {
			r.From, r.To = unix(from), unix(to)
		}
		if err := json.Unmarshal([]byte(failed), &r.Failed); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Result is something worked out with Prometheus over a period: the
// suggestions of a week, a review of a change to the rules.
type Result struct {
	ID       int64
	Kind     string
	At       time.Time
	From, To time.Time
	body     []byte
}

// Decode reads the result into v.
func (r Result) Decode(v any) error { return ungz(r.body, v) }

// AddResult keeps a result.
func (l *Ledger) AddResult(ctx context.Context, kind string, at, from, to time.Time, v any) error {
	z, err := gz(v)
	if err != nil {
		return err
	}
	return l.write(ctx, func(t *txn) error {
		_, err := t.ExecContext(t.ctx, "INSERT INTO results (kind, at, period_from, period_to, body) VALUES (?, ?, ?, ?, ?)",
			kind, at.Unix(), from.Unix(), to.Unix(), z)
		return err
	})
}

// Results are the latest results of a kind, the latest first.
func (l *Ledger) Results(ctx context.Context, kind string, limit int) ([]Result, error) {
	return l.results(ctx, "SELECT id, kind, at, period_from, period_to, body FROM results WHERE kind = ? ORDER BY at DESC, id DESC LIMIT ?", kind, limit)
}

// ReplaceResult keeps a result in place of those of its kind kept for the
// same period's start.
func (l *Ledger) ReplaceResult(ctx context.Context, kind string, at, from, to time.Time, v any) error {
	z, err := gz(v)
	if err != nil {
		return err
	}
	return l.write(ctx, func(t *txn) error {
		if _, err := t.ExecContext(t.ctx, "DELETE FROM results WHERE kind = ? AND period_from = ?", kind, from.Unix()); err != nil {
			return err
		}
		_, err := t.ExecContext(t.ctx, "INSERT INTO results (kind, at, period_from, period_to, body) VALUES (?, ?, ?, ?, ?)",
			kind, at.Unix(), from.Unix(), to.Unix(), z)
		return err
	})
}

// ByPeriod are, of the results of a kind for periods from since on, the
// latest for each period's start, the latest period first.
func (l *Ledger) ByPeriod(ctx context.Context, kind string, since time.Time) ([]Result, error) {
	return l.results(ctx, `SELECT id, kind, at, period_from, period_to, body FROM (
			SELECT *, row_number() OVER (PARTITION BY period_from ORDER BY at DESC, id DESC) AS n
			FROM results WHERE kind = ? AND period_from >= ?)
		WHERE n = 1 ORDER BY period_from DESC`, kind, since.Unix())
}

// ForPeriod is the latest result of a kind for the period starting at from;
// ok is false when there is none.
func (l *Ledger) ForPeriod(ctx context.Context, kind string, from time.Time) (r Result, ok bool, err error) {
	rs, err := l.results(ctx, "SELECT id, kind, at, period_from, period_to, body FROM results WHERE kind = ? AND period_from = ? ORDER BY at DESC, id DESC LIMIT 1",
		kind, from.Unix())
	if err != nil || len(rs) == 0 {
		return Result{}, false, err
	}
	return rs[0], true, nil
}

func (l *Ledger) results(ctx context.Context, q string, args ...any) ([]Result, error) {
	rows, err := l.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Result
	for rows.Next() {
		var r Result
		var at, from, to int64
		if err := rows.Scan(&r.ID, &r.Kind, &at, &from, &to, &r.body); err != nil {
			return nil, err
		}
		r.At, r.From, r.To = unix(at), unix(from), unix(to)
		out = append(out, r)
	}
	return out, rows.Err()
}

// JudgeAll gives every firing in the ledger its stones again, a week at a
// time so that other writes get their turn: after a change to the
// settings that decide them. done is told how far it has got.
func (l *Ledger) JudgeAll(ctx context.Context, o verdict.Options, done func(float64)) error {
	var lo, hi sql.NullInt64
	if err := l.r.QueryRowContext(ctx, `SELECT min(t), max(t) FROM (SELECT start_at AS t FROM firings
		UNION ALL SELECT start_at FROM incidents)`).Scan(&lo, &hi); err != nil || !lo.Valid {
		return err
	}
	const week = 7 * 86400
	for at := lo.Int64; at <= hi.Int64; at += week {
		end := min(at+week-1, hi.Int64)
		if err := l.write(ctx, func(t *txn) error { return l.judge(t, at, end, o) }); err != nil {
			return err
		}
		if done != nil {
			done(float64(end-lo.Int64+1) / float64(hi.Int64-lo.Int64+1))
		}
	}
	return nil
}

// Prune drops what started before a time: firings, impact windows, runs,
// results, changes and notifications. Incidents written down are kept. It deletes a few
// thousand rows at a time, so other writes get their turn, and gives the
// space back a little at a time.
func (l *Ledger) Prune(ctx context.Context, before time.Time) error {
	at := before.Unix()
	steps := []string{
		"DELETE FROM firings WHERE id IN (SELECT id FROM firings WHERE start_at < ? LIMIT 5000)",
		"DELETE FROM catches WHERE incident_id IN (SELECT id FROM incidents WHERE origin = 'impact' AND end_at < ? LIMIT 5000)",
		"DELETE FROM incidents WHERE id IN (SELECT id FROM incidents WHERE origin = 'impact' AND end_at < ? AND id NOT IN (SELECT incident_id FROM catches) LIMIT 5000)",
		"DELETE FROM runs WHERE id IN (SELECT id FROM runs WHERE started < ? LIMIT 5000)",
		"DELETE FROM results WHERE id IN (SELECT id FROM results WHERE at < ? LIMIT 5000)",
		"DELETE FROM changes WHERE id IN (SELECT id FROM changes WHERE at < ? LIMIT 5000)",
		"DELETE FROM windows WHERE id IN (SELECT id FROM windows WHERE (every = '' AND end_at < ?1) OR (every != '' AND until_at < ?1) LIMIT 5000)",
		"DELETE FROM outbox WHERE id IN (SELECT id FROM outbox WHERE created < ?1 LIMIT 5000)",
		// Their alerts go with them.
		"DELETE FROM notifications WHERE id IN (SELECT id FROM notifications WHERE at < ?1 LIMIT 5000)",
		// A source nothing was written to since is forgotten.
		"DELETE FROM sources WHERE written < ?",
	}
	for _, q := range steps {
		arg := at
		for {
			var n int64
			err := l.write(ctx, func(t *txn) error {
				res, err := t.ExecContext(t.ctx, q, arg)
				if err != nil {
					return err
				}
				n, err = res.RowsAffected()
				t.all = n > 0 && q == steps[0] // firings
				return err
			})
			if err != nil {
				return err
			}
			if n < 5000 {
				break
			}
		}
	}
	err := l.write(ctx, func(t *txn) error {
		if _, err := t.ExecContext(t.ctx, "DELETE FROM days WHERE day < ?", l.day(at)); err != nil {
			return err
		}
		if _, err := t.ExecContext(t.ctx, "DELETE FROM weeks WHERE week < ?", Week(l.day(at))); err != nil {
			return err
		}
		// Versions replaced before then, but the one in effect then, and
		// the documents no version holds.
		if _, err := t.ExecContext(t.ctx, `DELETE FROM versions WHERE since < ? AND id != (SELECT id FROM versions
			WHERE since < ? ORDER BY since DESC, id DESC LIMIT 1)`, at, at); err != nil {
			return err
		}
		if _, err := t.ExecContext(t.ctx, `DELETE FROM docs WHERE hash NOT IN (SELECT json_extract(d.value, '$.hash')
			FROM versions v, json_each(v.docs) d)`); err != nil {
			return err
		}
		// Alerts with no firing left: their labels go too.
		if _, err := t.ExecContext(t.ctx, "DELETE FROM series WHERE id NOT IN (SELECT DISTINCT series_id FROM firings)"); err != nil {
			return err
		}
		t.recorded = true
		l.ids.Lock()
		clear(l.ids.series)
		clear(l.ids.labels)
		l.ids.Unlock()
		return nil
	})
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err = l.w.ExecContext(ctx, "PRAGMA incremental_vacuum(2000)")
	return err
}
