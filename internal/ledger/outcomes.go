package ledger

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"slices"
	"sort"
	"time"
)

// What the ledger can tell of its own worth: a rule's stones over any
// stretch of time, to set beside what a review of a change to it said;
// what people wrote, and when; and when the record of incidents last moved.

// Span is a rule's stones over a stretch of time, and how many of the
// incidents that started in it the rule caught.
type Span struct {
	Firings int `json:"firings"` // judged: not those a quiet window excused
	White   int `json:"white"`
	Excused int `json:"excused"`
	Caught  int `json:"caught"`
}

// Black is how many firings caught nothing.
func (s Span) Black() int { return s.Firings - s.White }

// RuleSpans are a rule's stones between each of the times given, which
// are in order, and the next: whole days from the daily tallies, the days a
// time falls inside of from the firings themselves. A rule the ledger has
// never seen has none. It reads the rule's days, the firings of the days
// cut and its catches once each, however many times are given.
func (l *Ledger) RuleSpans(ctx context.Context, rule string, at []time.Time) ([]Span, error) {
	if len(at) < 2 {
		return nil, nil
	}
	out := make([]Span, len(at)-1)
	var id int64
	err := l.r.QueryRowContext(ctx, "SELECT id FROM rules WHERE name = ?", rule).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	cut := make([]int64, len(at))
	for i, t := range at {
		cut[i] = t.Unix()
	}
	lo, hi := cut[0], cut[len(cut)-1]
	// in is the span a time falls in, -1 for none.
	in := func(t int64) int {
		i := sort.Search(len(cut), func(i int) bool { return cut[i] > t }) - 1
		if i >= len(out) {
			return -1
		}
		return i
	}
	cutDays := map[int64]bool{}
	for _, c := range cut {
		if d := l.day(c); l.midnight(d) != c {
			cutDays[d] = true
		}
	}
	rows, err := l.r.QueryContext(ctx, "SELECT day, firings, white, excused FROM days WHERE rule_id = ? AND day >= ? AND day <= ?",
		id, l.day(lo), l.day(hi))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d int64
		var n, white, excused int
		if err := rows.Scan(&d, &n, &white, &excused); err != nil {
			rows.Close()
			return nil, err
		}
		// A day no time falls inside of lies in one span whole.
		if i := in(l.midnight(d)); i >= 0 && !cutDays[d] {
			out[i].Firings += n
			out[i].White += white
			out[i].Excused += excused
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The days a time falls inside of, firing by firing.
	for _, d := range slices.Sorted(maps.Keys(cutDays)) {
		if err := l.eachRow(ctx, func(s scanner) error {
			var at int64
			var white, excused bool
			if err := s.Scan(&at, &white, &excused); err != nil {
				return err
			}
			if i := in(at); i >= 0 {
				switch {
				case excused:
					out[i].Excused++
				case white:
					out[i].Firings++
					out[i].White++
				default:
					out[i].Firings++
				}
			}
			return nil
		}, "SELECT start_at, white, excused FROM firings WHERE rule_id = ? AND start_at >= ? AND start_at < ?",
			id, l.midnight(d), l.midnight(d+1)); err != nil {
			return nil, err
		}
	}
	err = l.eachRow(ctx, func(s scanner) error {
		var at int64
		if err := s.Scan(&at); err != nil {
			return err
		}
		if i := in(at); i >= 0 {
			out[i].Caught++
		}
		return nil
	}, `SELECT i.start_at FROM catches c JOIN incidents i ON i.id = c.incident_id
		WHERE c.rule_id = ? AND i.counts = 1 AND i.start_at >= ? AND i.start_at < ?`, id, lo, hi)
	return out, err
}

// Stones are every rule's firings and white stones over [from, to): whole
// days from the daily tallies, the hours at either end from the firings.
func (l *Ledger) Stones(ctx context.Context, from, to time.Time) (firings, white int, err error) {
	lo, hi := from.Unix(), to.Unix()
	if hi <= lo {
		return 0, 0, nil
	}
	d1, d2 := l.day(lo), l.day(hi)
	if l.midnight(d1) < lo {
		d1++
	}
	byFirings := "SELECT count(*) - coalesce(sum(excused), 0), coalesce(sum(white), 0) FROM firings WHERE start_at >= ? AND start_at < ?"
	parts := [][3]any{{byFirings, lo, hi}}
	if d1 < d2 {
		parts = [][3]any{
			{byFirings, lo, l.midnight(d1)},
			{"SELECT coalesce(sum(firings), 0), coalesce(sum(white), 0) FROM days WHERE day >= ? AND day < ?", d1, d2},
			{byFirings, l.midnight(d2), hi},
		}
	}
	for _, p := range parts {
		var n, w int
		if err := l.r.QueryRowContext(ctx, p[0].(string), p[1], p[2]).Scan(&n, &w); err != nil {
			return 0, 0, err
		}
		firings, white = firings+n, white+w
	}
	return firings, white, nil
}

// CountFirings is how many firings started in [from, to), excused or not:
// as many as a report or a try loads.
func (l *Ledger) CountFirings(ctx context.Context, from, to time.Time) (int, error) {
	var n int
	err := l.r.QueryRowContext(ctx, "SELECT count(*) FROM firings WHERE start_at >= ? AND start_at < ?", from.Unix(), to.Unix()).Scan(&n)
	return n, err
}

// eachRow calls f on each row a query reads.
func (l *Ledger) eachRow(ctx context.Context, f func(scanner) error, q string, args ...any) error {
	rows, err := l.r.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := f(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ChangesSince are the changes made through the UI or API since a time,
// the earliest first, most of them.
func (l *Ledger) ChangesSince(ctx context.Context, from time.Time, most int) ([]Change, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT at, by, what FROM changes WHERE at >= ? ORDER BY at, id LIMIT ?", from.Unix(), most)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var c Change
		var at int64
		if err := rows.Scan(&at, &c.By, &c.What); err != nil {
			return nil, err
		}
		c.At = unix(at)
		out = append(out, c)
	}
	return out, rows.Err()
}

// TasksSince are the tasks made or done since a time, and those still open
// or being done, the latest changed first.
func (l *Ledger) TasksSince(ctx context.Context, from time.Time) ([]Task, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT "+taskColumns+` FROM tasks
		WHERE created >= ?1 OR done_at >= ?1 OR status IN ('open', 'doing') ORDER BY updated DESC, id DESC LIMIT 5000`, from.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// LastIncident is when the latest incident written down started, at or
// before a time: from the incidents file, the UI or the API, impact windows
// confirmed among them; not one --impact found and nobody decided on. Zero
// when there is none.
func (l *Ledger) LastIncident(ctx context.Context, at time.Time) (time.Time, error) {
	var t sql.NullInt64
	err := l.r.QueryRowContext(ctx, "SELECT max(start_at) FROM incidents WHERE origin != 'impact' AND start_at <= ?", at.Unix()).Scan(&t)
	return fromNull(t), err
}

// FailedRuns are how many runs started since a time failed.
func (l *Ledger) FailedRuns(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := l.r.QueryRowContext(ctx, "SELECT count(*) FROM runs WHERE started >= ? AND error != ''", since.Unix()).Scan(&n)
	return n, err
}

// FirstRun is when the earliest run kept started; zero when there is none.
func (l *Ledger) FirstRun(ctx context.Context) (time.Time, error) {
	var t sql.NullInt64
	err := l.r.QueryRowContext(ctx, "SELECT min(started) FROM runs").Scan(&t)
	return fromNull(t), err
}
