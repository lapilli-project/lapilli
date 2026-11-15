package verdict

import (
	"slices"
	"time"

	"lapilli.dev/internal/incident"
)

// A period's record is kept when an incident started in it, or in the two
// weeks before its end: one written down, or a window --impact found. With
// none, a firing that caught nothing can't be told from one nobody wrote
// down, so the period's firings are counted, not judged: no rule is noisy,
// no precision is known, and no longer "for" is suggested (Report.NoRecord).

// RecordLookback is how long before a period's end an incident still keeps
// its record: two weeks, the gap lapilli serve's outcomes page asks about
// ("No incident written down in two weeks or more").
const RecordLookback = 14 * 24 * time.Hour

// RecordFrom is when the incidents that keep the record of [from, to) may
// start: from, or two weeks before to, whichever is earlier.
func RecordFrom(from, to time.Time) time.Time {
	if at := to.Add(-RecordLookback); at.Before(from) {
		return at
	}
	return from
}

// RecordKept reports whether one of starts, the times incidents started,
// falls in [RecordFrom(from, to), to).
func RecordKept(from, to time.Time, starts []time.Time) bool {
	lo := RecordFrom(from, to)
	return slices.ContainsFunc(starts, func(t time.Time) bool { return !t.Before(lo) && t.Before(to) })
}

// Starts are when incidents started.
func Starts(incs []incident.Incident) []time.Time {
	out := make([]time.Time, len(incs))
	for i, in := range incs {
		out[i] = in.Start
	}
	return out
}

// NoRecordText says, for a person, what a period with no record means, and
// how to keep one.
const NoRecordText = `No incident was written down, or found by --impact, in the period or the two weeks before it: a firing that caught nothing can't be told from one nobody wrote down, so no rule is called noisy and no longer "for" is suggested. Give incidents with --incidents or --impact; in lapilli serve, write them down, or mark a firing This was real.`
