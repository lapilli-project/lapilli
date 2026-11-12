package ledger

import (
	"context"
	"strconv"
	"time"
)

// A version of the rules can be recorded after the hours it was in effect
// were replayed: lapilli serve following a history of the rules learns of
// a commit when it is fetched, minutes or hours after its time. Those hours
// are replayed again, and their days set beside what really fired again.

// Again notes that the hours from at on are to be replayed again, and the
// days from at's on checked again. Hours not replayed yet need nothing; an
// earlier time noted already stands.
func (l *Ledger) Again(ctx context.Context, at time.Time) error {
	return l.write(ctx, func(t *txn) error {
		cursor, err := l.meta(t.ctx, t, "cursor")
		if err != nil {
			return err
		}
		if n, _ := strconv.ParseInt(cursor, 10, 64); cursor == "" || n <= at.Unix() {
			return nil
		}
		was, err := l.meta(t.ctx, t, "again")
		if err != nil {
			return err
		}
		if n, err := strconv.ParseInt(was, 10, 64); was == "" || err != nil || n > at.Unix() {
			if err := setMeta(t.ctx, t, "again", strconv.FormatInt(at.Unix(), 10)); err != nil {
				return err
			}
		}
		checked, err := l.meta(t.ctx, t, "checked_to")
		if err != nil {
			return err
		}
		midnight := l.midnight(l.day(at.Unix()))
		if n, _ := strconv.ParseInt(checked, 10, 64); checked != "" && n > midnight {
			return setMeta(t.ctx, t, "checked_to", strconv.FormatInt(midnight, 10))
		}
		return nil
	})
}

// AgainFrom is where the next run replays from again; zero for nowhere.
func (l *Ledger) AgainFrom(ctx context.Context) (time.Time, error) {
	v, err := l.meta(ctx, l.r, "again")
	if err != nil || v == "" {
		return time.Time{}, err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return unix(n), err
}

// AgainDone forgets where to replay from again, once a run has from there,
// unless an earlier time was noted since.
func (l *Ledger) AgainDone(ctx context.Context, at time.Time) error {
	return l.write(ctx, func(t *txn) error {
		_, err := t.ExecContext(t.ctx, "DELETE FROM meta WHERE key = 'again' AND value = ?", strconv.FormatInt(at.Unix(), 10))
		return err
	})
}
