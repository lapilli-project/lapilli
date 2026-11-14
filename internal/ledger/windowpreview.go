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
// changes, when it changes one. Nothing is written.
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
	if w.ID != 0 {
		was, err := l.WindowByID(ctx, w.ID)
		if err != nil {
			return w, err
		}
		if !mine(was.TeamID) {
			return w, ErrNotYourTeam
		}
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

// Starts are when a window's repeats start in [from, to), most at most, the
// earliest first: its own start, for one that doesn't repeat.
func (w Window) Starts(from, to time.Time, most int) []time.Time {
	var out []time.Time
	in := func(t time.Time) bool { return !t.Before(from) && t.Before(to) }
	loc := w.where()
	switch {
	case w.Every == "":
		if in(w.Start) {
			out = append(out, w.Start)
		}
	case w.scheduled():
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
				out = append(out, d)
			}
		}
	default:
		s := w.Start.In(loc)
		// The first repeat that may start in the span, by the calendar.
		n := 0
		if from.After(w.Start) {
			n = max(0, int((civil(from.In(loc))-civil(s))/int64(w.days()))-1)
		}
		for ; len(out) < most; n++ {
			d := s.AddDate(0, 0, n*w.days())
			if !d.Before(to) || !w.Until.IsZero() && civil(d) > civil(w.Until.In(loc)) {
				break
			}
			if in(d) {
				out = append(out, d)
			}
		}
	}
	return out
}
