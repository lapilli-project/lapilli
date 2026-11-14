package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"maps"
	"time"
)

// The outbox: what is to be told of what happened, written in the same
// write as what happened, one row for each place it goes, and sent after.

// TellTo sets where what happens is to be told: "webhook", "slack", or
// none, and then nothing is written to the outbox.
func (l *Ledger) TellTo(dests []string) {
	d := append([]string(nil), dests...)
	l.tellTo.Store(&d)
}

// tell writes, for each place it goes, that something happened.
func (l *Ledger) tell(t *txn, event, by string, about map[string]any) error {
	dests := l.tellTo.Load()
	if dests == nil || len(*dests) == 0 {
		return nil
	}
	now := time.Now()
	about["event"], about["at"] = event, now.UTC()
	if by != "" {
		about["by"] = by
	}
	body, err := json.Marshal(about)
	if err != nil {
		return err
	}
	for _, d := range *dests {
		if _, err := t.ExecContext(t.ctx, "INSERT INTO outbox (dest, event, body, created, next_at) VALUES (?, ?, ?, ?, ?)",
			d, event, string(body), now.Unix(), now.Unix()); err != nil {
			return err
		}
	}
	return nil
}

// Told is something to be told, to one place.
type Told struct {
	Dest  string
	About map[string]any
}

// TellOnce writes what is told of an event, once for each value of a key
// kept in the ledger, whatever is told and wherever it goes. A page shows none
// of it, so pages kept stay.
func (l *Ledger) TellOnce(ctx context.Context, key, value, event string, told []Told) error {
	return l.writeAside(ctx, func(t *txn) error {
		if have, err := l.meta(t.ctx, t, key); err != nil || have == value {
			return err
		}
		now := time.Now()
		for _, x := range told {
			about := map[string]any{"event": event, "at": now.UTC()}
			maps.Copy(about, x.About)
			body, err := json.Marshal(about)
			if err != nil {
				return err
			}
			if _, err := t.ExecContext(t.ctx, "INSERT INTO outbox (dest, event, body, created, next_at) VALUES (?, ?, ?, ?, ?)",
				x.Dest, event, string(body), now.Unix(), now.Unix()); err != nil {
				return err
			}
		}
		return setMeta(t.ctx, t, key, value)
	})
}

// Notice is something to be told, to one place.
type Notice struct {
	ID       int64
	Dest     string
	Event    string
	Body     json.RawMessage
	Created  time.Time
	Attempts int
	SentAt   time.Time
	Error    string
}

func (l *Ledger) notices(ctx context.Context, where string, args ...any) ([]Notice, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT id, dest, event, body, created, attempts, sent_at, error FROM outbox "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Notice
	for rows.Next() {
		var n Notice
		var body string
		var created int64
		var sent sql.NullInt64
		if err := rows.Scan(&n.ID, &n.Dest, &n.Event, &body, &created, &n.Attempts, &sent, &n.Error); err != nil {
			return nil, err
		}
		n.Body, n.Created, n.SentAt = json.RawMessage(body), unix(created), fromNull(sent)
		out = append(out, n)
	}
	return out, rows.Err()
}

// Due are the notices to send to dest now, the oldest first.
func (l *Ledger) Due(ctx context.Context, dest string, now time.Time, limit int) ([]Notice, error) {
	return l.notices(ctx, "WHERE dest = ? AND sent_at IS NULL AND next_at <= ? ORDER BY id LIMIT ?", dest, now.Unix(), limit)
}

// Unsent is how many notices are not sent, those given up on too.
func (l *Ledger) Unsent(ctx context.Context) (n int, err error) {
	err = l.r.QueryRowContext(ctx, "SELECT COUNT(*) FROM outbox WHERE sent_at IS NULL").Scan(&n)
	return
}

// Sent notes a notice sent. In a ledger replicas share, by itself, a row
// it holds claimed: it waits for no write's turn, which a write long enough
// could hold past the claim.
func (l *Ledger) Sent(ctx context.Context, id int64) error {
	if l.rep != nil {
		_, err := l.rep.side.ExecContext(ctx, "UPDATE outbox SET sent_at = ?, error = '' WHERE id = ?", time.Now().Unix(), id)
		return err
	}
	return l.writeAside(ctx, func(t *txn) error {
		_, err := t.ExecContext(t.ctx, "UPDATE outbox SET sent_at = ?, error = '' WHERE id = ?", time.Now().Unix(), id)
		return err
	})
}

// GiveUpAfter is how many times a notice is tried.
const GiveUpAfter = 10

// Failed notes a notice that couldn't be sent, to be tried again later,
// further apart each time; after GiveUpAfter tries, not again.
func (l *Ledger) Failed(ctx context.Context, id int64, why string) error {
	if len(why) > 300 {
		why = why[:300]
	}
	if l.rep != nil {
		// As Sent: by itself, the row locked while its attempts are counted.
		tx, err := l.rep.side.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var attempts int
		if err := tx.QueryRowContext(ctx, "SELECT attempts FROM outbox WHERE id = ? FOR UPDATE", id).Scan(&attempts); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE outbox SET attempts = ?, next_at = ?, error = ? WHERE id = ?", attempts+1, retryAt(attempts+1).Unix(), why, id); err != nil {
			return err
		}
		return tx.Commit()
	}
	return l.writeAside(ctx, func(t *txn) error {
		var attempts int
		if err := t.QueryRowContext(t.ctx, "SELECT attempts FROM outbox WHERE id = ?", id).Scan(&attempts); err != nil {
			return err
		}
		attempts++
		_, err := t.ExecContext(t.ctx, "UPDATE outbox SET attempts = ?, next_at = ?, error = ? WHERE id = ?", attempts, retryAt(attempts).Unix(), why, id)
		return err
	})
}

// retryAt is when a notice tried so many times is tried again: further
// apart each time; after GiveUpAfter tries, not again.
func retryAt(attempts int) time.Time {
	if attempts >= GiveUpAfter {
		return time.Unix(1<<40, 0) // given up
	}
	return time.Now().Add(min(time.Minute<<min(attempts-1, 9), 6*time.Hour))
}
