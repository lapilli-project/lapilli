package ledger

import (
	"context"
	"slices"
	"unicode/utf8"

	"lapilli.dev/internal/verdict"
)

// MaxTurns is how many turns of a schedule are kept: the oldest go first.
const MaxTurns = 20_000

// PutTurns keeps a schedule's turns as its feed now has them, flattened and
// the earliest first: the turns kept that end after the earliest of them
// are the feed's to say, and are replaced (one that began before it ends
// there); those before it are kept. It reports whether anything changed;
// no turns change nothing, as a feed that failed or holds none.
func (l *Ledger) PutTurns(ctx context.Context, schedule string, turns []verdict.RotaTurn) (bool, error) {
	if len(turns) == 0 {
		return false, nil
	}
	for i, t := range turns {
		switch {
		case !t.To.After(t.From) || i > 0 && t.From.Before(turns[i-1].To):
			return false, invalid("turns are in order, don't overlap, and last a while")
		case t.Who == "" || !Plain(t.Who) || utf8.RuneCountInString(t.Who) > 200:
			return false, invalid("a turn names someone, 200 characters at most, that shows")
		case !sane(t.From) || !sane(t.To):
			return false, invalid("a turn's times are from 1970 to 9999")
		}
	}
	from := turns[0].From.Unix()
	was, err := l.turnsFrom(ctx, schedule, from)
	if err != nil {
		return false, err
	}
	if slices.EqualFunc(was, turns, func(a, b verdict.RotaTurn) bool {
		return a.From.Equal(b.From) && a.To.Equal(b.To) && a.Who == b.Who
	}) {
		return false, nil
	}
	err = l.write(ctx, func(t *txn) error {
		if _, err := t.ExecContext(t.ctx, "DELETE FROM on_call_turns WHERE schedule = ? AND start_at >= ?", schedule, from); err != nil {
			return err
		}
		if _, err := t.ExecContext(t.ctx, "UPDATE on_call_turns SET end_at = ? WHERE schedule = ? AND start_at < ? AND end_at > ?", from, schedule, from, from); err != nil {
			return err
		}
		st, err := t.PrepareContext(t.ctx, "INSERT INTO on_call_turns (schedule, start_at, end_at, who) VALUES (?, ?, ?, ?)")
		if err != nil {
			return err
		}
		defer st.Close()
		for _, x := range turns {
			if _, err := st.ExecContext(t.ctx, schedule, x.From.Unix(), x.To.Unix(), x.Who); err != nil {
				return err
			}
		}
		_, err = t.ExecContext(t.ctx, `DELETE FROM on_call_turns WHERE schedule = ?1 AND start_at < (
			SELECT start_at FROM on_call_turns WHERE schedule = ?1 ORDER BY start_at DESC LIMIT 1 OFFSET ?2)`, schedule, MaxTurns-1)
		return err
	})
	return err == nil, err
}

// Turns are a schedule's turns kept, the earliest first.
func (l *Ledger) Turns(ctx context.Context, schedule string) ([]verdict.RotaTurn, error) {
	return l.turnsFrom(ctx, schedule, -1<<62)
}

// turnsFrom are a schedule's turns that end after from.
func (l *Ledger) turnsFrom(ctx context.Context, schedule string, from int64) ([]verdict.RotaTurn, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT start_at, end_at, who FROM on_call_turns WHERE schedule = ? AND end_at > ? ORDER BY start_at", schedule, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []verdict.RotaTurn
	for rows.Next() {
		var a, b int64
		var t verdict.RotaTurn
		if err := rows.Scan(&a, &b, &t.Who); err != nil {
			return nil, err
		}
		t.From, t.To = unix(a), unix(b)
		out = append(out, t)
	}
	return out, rows.Err()
}
