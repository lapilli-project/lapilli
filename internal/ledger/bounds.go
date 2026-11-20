package ledger

import (
	"time"
)

// A team as the boundary of change (docs/design/changes.md): who of a team
// approves its rules' changes, where they are written, and when nothing
// is approved or applied, declared in lapilli serve's --config beside the
// teams. Kept as the file says it, in the meta table, in the write that
// keeps the ledger to the file (Reconcile): read twice before it is kept,
// as a role is, and the same on every replica that shares the ledger. No
// table of its own: the file alone declares it, and the UI changes none
// of it.

// BoundsKey is the meta key the boundaries are kept under, for Meta: what
// lapilli serve wrote as Declared.Bounds, "" for none.
const BoundsKey = "change_bounds"

// keepBounds keeps the boundaries a file declares, in the write that keeps
// the ledger to it: noted in the activity log when they changed, as a
// role given or taken is. Without a file, there are none.
func keepBounds(t *txn, bounds string, r *Reconciled) error {
	was, err := (&Ledger{}).meta(t.ctx, t, "x."+BoundsKey)
	if err != nil || was == bounds {
		return err
	}
	if err := setMeta(t.ctx, t, "x."+BoundsKey, bounds); err != nil {
		return err
	}
	r.Changed++
	return change(t, ConfigBy, "change boundaries declared in config", nil, nil)
}

// CheckSpan checks the times of a window that isn't a quiet window, as a
// quiet window's are checked (PutWindow): a change freeze's, once or
// repeating every day, week, month or by a cron expression, by the
// calendar of loc. It is then ready to say what it covers (Covers, EndOf,
// NextAfter).
func CheckSpan(w *Window, loc *time.Location) error {
	if loc == nil {
		loc = time.UTC
	}
	err := (&Ledger{loc: loc}).checkWindow(w)
	w.loc = loc
	return err
}

// EndOf is when the repeat of a window that covers t ends, as Covers says
// it covers t: the window's end, for one that doesn't repeat. ok is false
// when it covers no repeat of t.
func (w Window) EndOf(t time.Time) (end time.Time, ok bool) {
	long := w.End.Sub(w.Start)
	if w.Every == "" {
		return w.End, !t.Before(w.Start) && !t.After(w.End)
	}
	if t.Before(w.Start) {
		return time.Time{}, false
	}
	loc := w.where()
	if w.scheduled() {
		w.resolve()
		if w.sch == nil {
			return time.Time{}, false
		}
		due := w.sch.Last(w.lastAt(t), loc)
		if due.IsZero() || due.Before(w.Start) || t.After(due.Add(long)) {
			return time.Time{}, false
		}
		return due.Add(long), true
	}
	s, at := w.Start.In(loc), t.In(loc)
	k := int((civil(at) - civil(s)) / int64(w.days()))
	for _, n := range []int{k, k - 1} {
		if n < 0 {
			continue
		}
		from := s.AddDate(0, 0, n*w.days())
		if !w.Until.IsZero() && civil(from) > civil(w.Until.In(loc)) {
			continue
		}
		if !t.Before(from) && !t.After(from.Add(long)) {
			return from.Add(long), true
		}
	}
	return time.Time{}, false
}

// NextAfter is the first repeat of a window that starts after t: its start
// and end. ok is false when none does: one that doesn't repeat and began,
// or one whose last repeat, by Until, began.
func (w Window) NextAfter(t time.Time) (start, end time.Time, ok bool) {
	long := w.End.Sub(w.Start)
	if !t.Before(w.Start) && w.Every == "" {
		return time.Time{}, time.Time{}, false
	}
	if t.Before(w.Start) {
		return w.Start, w.End, true
	}
	loc := w.where()
	var next time.Time
	if w.scheduled() {
		w.resolve()
		if w.sch == nil {
			return time.Time{}, time.Time{}, false
		}
		next = w.sch.Next(t, loc)
		if next.IsZero() || !next.After(t) || !w.lastAt(next).Equal(next) {
			return time.Time{}, time.Time{}, false // past Until's day
		}
	} else {
		s, at := w.Start.In(loc), t.In(loc)
		// The repeat of t's day, or of the period's, may start after t.
		n := max(0, int((civil(at)-civil(s))/int64(w.days()))-1)
		for ; ; n++ {
			if from := s.AddDate(0, 0, n*w.days()); from.After(t) {
				next = from
				break
			}
		}
		if !w.Until.IsZero() && civil(next.In(loc)) > civil(w.Until.In(loc)) {
			return time.Time{}, time.Time{}, false
		}
	}
	return next, next.Add(long), true
}
