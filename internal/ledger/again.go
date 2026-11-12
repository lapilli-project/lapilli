package ledger

import (
	"context"
	"errors"
	"strconv"
	"time"
)

// A version of the rules can be recorded after the hours it was in effect
// were replayed: lapilli serve following a history of the rules learns of
// a commit when it is fetched, minutes or hours after its time. Those hours
// are replayed again, and their days set beside what really fired again.
//
// Following and the runs are apart (the watcher and the worker): a version
// can be recorded while a run, or a check, that began before it is under
// way. Each note bumps a sequence; a run forgets the note only if it is the
// one the run began with, and a check keeps what it found only if nothing
// was noted since it began, so what they did with the rules before isn't
// taken for what was asked since.

// ErrAgain is a check that began before a version of the rules was
// recorded under it: nothing it found is kept.
var ErrAgain = errors.New("a version of the rules was recorded since the check began")

// Again notes that the hours from at on are to be replayed again, and the
// days from at's on checked again, the checks kept for them from there gone.
// An earlier time noted already stands.
func (l *Ledger) Again(ctx context.Context, at time.Time) error {
	return l.write(ctx, func(t *txn) error {
		seq, err := l.metaInt(t, "again_seq")
		if err != nil {
			return err
		}
		if err := setMeta(t.ctx, t, "again_seq", strconv.FormatInt(seq+1, 10)); err != nil {
			return err
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
		checked, err := l.metaInt(t, "checked_to")
		if err != nil {
			return err
		}
		day := l.day(at.Unix())
		midnight := l.midnight(day)
		if checked <= midnight {
			return nil
		}
		// What the check kept from that day on was of the rules before.
		for _, q := range []string{"DELETE FROM checked WHERE day >= ?", "DELETE FROM check_elsewhere WHERE day >= ?"} {
			if _, err := t.ExecContext(t.ctx, q, day); err != nil {
				return err
			}
		}
		if _, err := t.ExecContext(t.ctx, "DELETE FROM check_misses WHERE start_at >= ?", midnight); err != nil {
			return err
		}
		return setMeta(t.ctx, t, "checked_to", strconv.FormatInt(midnight, 10))
	})
}

// metaInt reads a number kept in meta, 0 for none.
func (l *Ledger) metaInt(t *txn, key string) (int64, error) {
	v, err := l.meta(t.ctx, t, key)
	if err != nil || v == "" {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

// AgainFrom is where the next run replays from again, zero for nowhere,
// and the sequence of the notes so far: what AgainDone and the checks are
// given.
func (l *Ledger) AgainFrom(ctx context.Context) (time.Time, int64, error) {
	var at time.Time
	v, err := l.meta(ctx, l.r, "again")
	if err != nil {
		return at, 0, err
	}
	if v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return at, 0, err
		}
		at = unix(n)
	}
	s, err := l.meta(ctx, l.r, "again_seq")
	if err != nil || s == "" {
		return at, 0, err
	}
	seq, err := strconv.ParseInt(s, 10, 64)
	return at, seq, err
}

// AgainDone forgets where to replay from again, once a run has from there,
// unless anything was noted since the run read it (seq).
func (l *Ledger) AgainDone(ctx context.Context, seq int64) error {
	return l.write(ctx, func(t *txn) error {
		now, err := l.metaInt(t, "again_seq")
		if err != nil || now != seq {
			return err
		}
		_, err = t.ExecContext(t.ctx, "DELETE FROM meta WHERE key = 'again'")
		return err
	})
}

// sameAgain refuses a check's write when a version was recorded since it
// began.
func (l *Ledger) sameAgain(t *txn, seq int64) error {
	now, err := l.metaInt(t, "again_seq")
	if err != nil {
		return err
	}
	if now != seq {
		return ErrAgain
	}
	return nil
}
