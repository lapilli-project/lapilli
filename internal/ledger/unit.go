package ledger

import (
	"time"

	"lapilli.dev/internal/verdict"
)

// The record is kept by a unit: a day, as lapilli serve judges by default,
// or a week (--record-by week), as it did before 0.72, from the Monday
// (verdict.Options.RecordDay says which). A unit is named by its first day.

// unitDays is how many days a unit of o's is.
func unitDays(o verdict.Options) int64 {
	if o.RecordDay {
		return 1
	}
	return 7
}

// unitOf is the first day of the unit the day d is in.
func unitOf(o verdict.Options, d int64) int64 {
	if o.RecordDay {
		return d
	}
	return Week(d)
}

// UnitDays is how many days a unit of o's is: 1, or 7.
func UnitDays(o verdict.Options) int64 { return unitDays(o) }

// UnitOf is the first day of the unit (o's RecordDay: a day, else a week)
// the day d is in: d itself judged by day, its Monday by week.
func UnitOf(o verdict.Options, d int64) int64 { return unitOf(o, d) }

// DayOf is the day t is in, in the ledger's time zone, from its midnight to
// the next: what lapilli serve judges the record by (verdict.Options.RecordUnit
// with RecordDay).
func (l *Ledger) DayOf(t time.Time) (time.Time, time.Time) {
	d := l.day(t.Unix())
	return unix(l.midnight(d)), unix(l.midnight(d + 1))
}

// UnitBounds are the start and the end of the unit u, as o has units.
func (l *Ledger) UnitBounds(o verdict.Options, u int64) (time.Time, time.Time) {
	return unix(l.midnight(u)), unix(l.midnight(u + unitDays(o)))
}

// SpanUnits are the first days of the units [from, to) has some of: one at
// least.
func (l *Ledger) SpanUnits(o verdict.Options, from, to time.Time) []int64 {
	var out []int64
	n := unitDays(o)
	for u := unitOf(o, l.day(from.Unix())); l.midnight(u) < to.Unix(); u += n {
		out = append(out, u)
	}
	if len(out) == 0 {
		out = append(out, unitOf(o, l.day(from.Unix())))
	}
	return out
}

// UnitsOf is from the start of the first unit [from, to) has some of to the
// end of the last.
func (l *Ledger) UnitsOf(o verdict.Options, from, to time.Time) (time.Time, time.Time) {
	us := l.SpanUnits(o, from, to)
	return unix(l.midnight(us[0])), unix(l.midnight(us[len(us)-1] + unitDays(o)))
}

// UnitKept reports whether the record of the unit u is kept, by rec: the
// granularity lapilli serve judges firings by. With named signals, whether
// any firing of it would be judged.
func (l *Ledger) UnitKept(rec verdict.Record, o verdict.Options, u int64) bool {
	a, b := l.UnitBounds(o, u)
	if rec.Named() {
		return rec.JudgesAny(a, b)
	}
	return rec.Kept(a, b)
}

// unitRun is a stretch of units judged alike.
type unitRun struct {
	from, to int64 // unix seconds, [from, to)
	first    int64 // the first day of its first unit
}

// runs are the units [from, to) has some of, cut to it, those next to one
// another that rec judges alike put together: a statement for each run,
// not each unit. With named signals, alike is rec.Key's.
func (l *Ledger) runs(rec verdict.Record, o verdict.Options, from, to time.Time) []unitRun {
	var out []unitRun
	var last string
	for i, u := range l.SpanUnits(o, from, to) {
		a, b := max(from.Unix(), l.midnight(u)), min(to.Unix(), l.midnight(u+unitDays(o)))
		a0, b0 := l.UnitBounds(o, u)
		k := rec.Key(a0, b0)
		if i > 0 && k == last {
			out[len(out)-1].to = b
			continue
		}
		out = append(out, unitRun{a, b, u})
		last = k
	}
	return out
}
