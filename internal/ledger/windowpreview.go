package ledger

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"slices"
	"time"
)

// A quiet window looked at before it is kept (docs/design/ui.md): checked
// as PutWindow checks it, and ready to say which firings it would excuse
// and when it falls due, with nothing written.

// PreviewWindow is a quiet window as PutWindow would keep it, checked as
// it is: its title, times, schedule, pattern and labels; that its team is
// there, and, for a team's editor (WithTeams), theirs, as is the window it
// changes, when it changes one; and the limits on how many the ledger and
// a team keep, a new one's or one moved to another team. Nothing is
// written.
func (l *Ledger) PreviewWindow(ctx context.Context, w Window) (Window, error) {
	if err := l.checkWindow(&w); err != nil {
		return w, err
	}
	teams, limited := scopeOf(ctx)
	mine := func(team int64) bool { return !limited || team > 0 && slices.Contains(teams, team) }
	if !mine(w.TeamID) {
		return w, ErrNotYourTeam
	}
	if w.TeamID != 0 {
		var x int
		if err := l.r.QueryRowContext(ctx, "SELECT 1 FROM teams WHERE id = ?", w.TeamID).Scan(&x); errors.Is(err, sql.ErrNoRows) {
			return w, invalid("no such team")
		} else if err != nil {
			return w, err
		}
	}
	// The limits on how many there are, as saving asks them: a preview
	// that passes is a window that saving keeps.
	if w.ID != 0 {
		was, err := l.WindowByID(ctx, w.ID)
		if err != nil {
			return w, err
		}
		if !mine(was.TeamID) {
			return w, ErrNotYourTeam
		}
		if err := movedLeft(ctx, l.r, was.TeamID, w.TeamID); err != nil {
			return w, err
		}
	} else if err := windowsLeft(ctx, l.r, w.TeamID); err != nil {
		return w, err
	}
	w.loc, w.ruled = l.loc, map[string]bool{}
	if w.Rules != "" {
		w.re = regexp.MustCompile(w.Rules) // checked by checkWindow
	}
	w.resolve()
	if w.TeamID > 0 {
		own, err := l.ownership(ctx, l.r)
		if err != nil {
			return w, err
		}
		w.own = own
	}
	return w, nil
}

// Fall is a repeat of a quiet window, as Covers covers it: from Start to
// End, both covered.
type Fall struct{ Start, End time.Time }

// Falls are a window's repeats that Covers starts to cover in [from, to),
// most at most, the earliest first: its own span, for one that doesn't
// repeat. Each is what Covers says of it, worked out by its own rule, not
// by the calendar alone: a repeat every day or week is covered by Covers
// only from the start of the day it falls on, and where the clocks skip
// midnight (America/Santiago, America/Havana) a repeat set at midnight
// starts, by the calendar, the hour before, on the day before, which
// Covers doesn't take for it. A repeat Covers never covers isn't one.
func (w Window) Falls(from, to time.Time, most int) []Fall {
	var out []Fall
	in := func(t time.Time) bool { return !t.Before(from) && t.Before(to) }
	loc := w.where()
	long := w.End.Sub(w.Start)
	switch {
	case w.Every == "":
		if in(w.Start) {
			out = append(out, Fall{w.Start, w.End})
		}
	case w.scheduled():
		// Covers takes the last time the schedule fell due by a time, and
		// each repeat is shorter than the shortest time between two.
		w.resolve()
		if w.sch == nil {
			return nil
		}
		last := w.lastAt(to)
		for d := w.sch.Last(from, loc); len(out) < most; d = w.sch.Next(d, loc) {
			if d.IsZero() || d.After(last) || !d.Before(to) {
				break
			}
			if !d.Before(w.Start) && in(d) {
				out = append(out, Fall{d, d.Add(long)})
			}
		}
	default:
		s := w.Start.In(loc)
		days := int64(w.days())
		// The first repeat that may start in the span, by the calendar.
		n := int64(0)
		if from.After(w.Start) {
			n = max(0, (civil(from.In(loc))-civil(s))/days-1)
		}
		for ; len(out) < most; n++ {
			d := s.AddDate(0, 0, int(n*days))
			if !w.Until.IsZero() && civil(d) > civil(w.Until.In(loc)) {
				break
			}
			// Covers takes a time for repeat n when it falls in the n-th
			// period of days from the window's first, or the next: from the
			// first of its days on. A repeat is shorter than its period
			// (checkWindow), so it ends before the next is over.
			f := Fall{d, d.Add(long)}
			if lo := dayStart(civil(s)+n*days, loc); f.Start.Before(lo) {
				f.Start = lo
			}
			if !f.Start.Before(to) {
				break
			}
			if f.Start.After(f.End) || f.Start.Before(w.Start) {
				continue
			}
			if in(f.Start) {
				out = append(out, f)
			}
		}
	}
	return out
}

// dayStart is when a day, as civil counts it, starts in loc: its midnight,
// or, where the clocks skip midnight, the first time of the day, as the
// ledger's Midnight has it.
func dayStart(day int64, loc *time.Location) time.Time {
	y, m, d := time.Unix(day*86400, 0).UTC().Date()
	t := time.Date(y, m, d, 0, 0, 0, 0, loc)
	for civil(t.In(loc)) < day {
		t = t.Add(time.Hour)
	}
	return t
}
