package verdict

import (
	"slices"
	"time"

	"lapilli.dev/internal/incident"
)

// A firing is judged when the record was kept at its time: an incident
// firings are judged against (written down, or a window --impact found) was
// on at some time in the two weeks before it, the firing caught one, or an
// impact signal was read over its time (--impact): what was read, found or
// not, says whether users were hurt. An incident with no end is taken to
// last Options.UnknownLen, as it is when a firing catches it. A firing with
// no record caught nothing written down, which can't be told from one
// nobody wrote down: it is counted, among the firings, and not judged: it
// is in no precision and makes no rule noisy (RuleResult.Unjudged), and a
// rule none of whose firings is judged has no precision at all
// (RuleResult.NoRecord).
//
// lapilli verdict judges each firing so. lapilli serve keeps its stones by
// week, and judges by week (Options.RecordWeek): a week's firings are
// judged when the record of the week is kept, an incident on in it or in
// the two weeks before it, so that what it replays is judged as what it
// keeps is.

// RecordLookback is how long before a firing an incident still keeps its
// record: two weeks, the gap lapilli serve's outcomes page asks about ("No
// incident written down in two weeks or more").
const RecordLookback = 14 * 24 * time.Hour

// RecordFrom is from when the incidents that keep the record of any time in
// [from, to) are: two weeks before from. --impact is read from then.
func RecordFrom(from, to time.Time) time.Time { return from.Add(-RecordLookback) }

// A Record is what keeps the record: when each incident firings are judged
// against was on, and from when an impact signal was read, all of it; and,
// in lapilli serve, the weeks it is judged by (Options.RecordWeek).
type Record struct {
	// starts are the incidents' starts, the earliest first; ends, the
	// latest end of any incident up to each.
	starts, ends []time.Time
	impactFrom   time.Time
	week         func(time.Time) (time.Time, time.Time)
}

// RecordOf is the record incidents keep, by o: one with no end lasting
// o.UnknownLen, an impact signal read from o.ImpactFrom on (zero for none),
// and judged by o.RecordWeek's weeks, when it has them.
func RecordOf(incs []incident.Incident, o Options) Record {
	sorted := slices.Clone(incs)
	slices.SortFunc(sorted, func(a, b incident.Incident) int { return a.Start.Compare(b.Start) })
	r := Record{impactFrom: o.ImpactFrom, week: o.RecordWeek}
	var end time.Time
	for _, in := range sorted {
		if e := in.EndOr(o.UnknownLen); e.After(end) {
			end = e
		}
		r.starts = append(r.starts, in.Start)
		r.ends = append(r.ends, end)
	}
	return r
}

// on reports whether an incident that started before at was on at lo or
// later.
func (r Record) on(lo, at time.Time, before bool) bool {
	i, _ := slices.BinarySearchFunc(r.starts, at, time.Time.Compare)
	if !before { // started at at, too
		for i < len(r.starts) && r.starts[i].Equal(at) {
			i++
		}
	}
	return i > 0 && !r.ends[i-1].Before(lo)
}

// Kept reports whether the record of [from, to) is kept, for any time in
// it: an incident on at some time from two weeks before from to to, or the
// impact signal read over all of it.
func (r Record) Kept(from, to time.Time) bool {
	if !r.impactFrom.IsZero() && !r.impactFrom.After(from) {
		return true
	}
	return r.on(RecordFrom(from, to), to, true)
}

// At reports whether the record was kept at t: an incident on at some time
// in the two weeks before it, or the impact signal read over it.
func (r Record) At(t time.Time) bool {
	if !r.impactFrom.IsZero() && !r.impactFrom.After(t) {
		return true
	}
	return r.on(t.Add(-RecordLookback), t, false)
}

// Judges reports whether a firing that started at t is judged by the
// record: kept at its time, or, judged by week, over its week.
func (r Record) Judges(t time.Time) bool {
	if r.week == nil {
		return r.At(t)
	}
	from, to := r.week(t)
	return r.Kept(from, to)
}

// JudgesAny reports whether any firing in [from, to) would be: the record
// kept at some time in it, or, judged by week, over one of its weeks.
func (r Record) JudgesAny(from, to time.Time) bool {
	if r.week == nil {
		return r.Kept(from, to)
	}
	for at, n := from, 0; n == 0 || at.Before(to); n++ {
		a, b := r.week(at)
		if r.Kept(a, b) {
			return true
		}
		if !b.After(at) || n > 1e5 {
			return false // a week that doesn't end: none further
		}
		at = b
	}
	return false
}

// NoRecordText says, for a person, what a period with no record means, and
// how to keep one.
const NoRecordText = `No incident was written down, or found by --impact, in the period or the two weeks before it: a firing that caught nothing can't be told from one nobody wrote down, so no rule is called noisy and no longer "for" is suggested. Give incidents with --incidents, or read users' impact over the period with --impact; in lapilli serve, write them down, or mark a firing This was real.`

// SomeUnjudgedText says, for a person, how many of a period's firings had
// no record at their time.
const SomeUnjudgedText = `%d of the %d firings came with no incident written down in the two weeks before them, and caught none: they are counted, not judged, and are in no rule's precision.`
