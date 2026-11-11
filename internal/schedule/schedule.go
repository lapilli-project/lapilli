// Package schedule reads when a report is sent, or a quiet window repeats: a
// preset, daily, weekly or monthly at a time of day, or a cron expression of
// five fields; and says when it last fell due, and what span that covers
// (docs/design/reports.md, docs/design/serveops.md).
//
// Times are counted on the calendar of a zone, as a repeating quiet window
// is: a time daylight saving skips falls due at the instant the clocks move
// forward past it, and a time it repeats, once, the first time.
package schedule

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Error is a schedule that can't be read. Format and Args are kept apart,
// so the message can be shown in the reader's language.
type Error struct {
	Format string
	Args   []any
}

func (e Error) Error() string { return fmt.Sprintf(e.Format, e.Args...) }

func bad(format string, args ...any) error { return Error{format, args} }

// maxText is how long a schedule's text may be.
const maxText = 100

// lookBack is how far back, in days, a cron expression's last time is
// looked for: a year and a day, as one is refused that doesn't fall due
// every year.
const lookBack = 366

type kind int

const (
	daily kind = iota
	weekly
	monthly
	cron
)

// Schedule is when a report falls due.
type Schedule struct {
	text         string
	kind         kind
	hour, minute int
	weekday      time.Weekday
	monthDay     int // 1 to 28, or 0 for the month's last day
	// nth, for a month's weekday, is which of them: 1 to 4, or -1 for the
	// last; 0 for a day of the month.
	nth int
	c   *cronExpr
	// span is the span a cron expression's report covers, when given; zero
	// for the time since the one before.
	span time.Duration
}

// String is the schedule as it was given.
func (s Schedule) String() string { return s.text }

// Cron reports whether the schedule is a cron expression, whose span may be
// given.
func (s Schedule) Cron() bool { return s.kind == cron }

var weekdays = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

var months = []string{"", "jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}

// ordinals are which of a month's weekdays: the first to the fourth, or the
// last (-1).
var ordinals = map[string]int{"first": 1, "second": 2, "third": 3, "fourth": 4, "last": -1}

// Parse reads a schedule: daily@09:00, weekly mon@09:00, monthly 1@09:00
// (a day from 1 to 28, or last, or a weekday of the month: first sat, last
// fri), or five cron fields (minute hour
// day-of-month month day-of-week), at most hourly: a cron expression's
// minute is one minute. span, for a cron expression only, is the span its
// report covers; zero for the time since the one before.
func Parse(text string, span time.Duration) (Schedule, error) {
	text = strings.TrimSpace(text)
	s := Schedule{text: text}
	if text == "" || len(text) > maxText {
		return s, bad("a schedule is daily@HH:MM, weekly DAY@HH:MM, monthly D@HH:MM, or five cron fields, %d characters at most", maxText)
	}
	low := strings.ToLower(text)
	clock := func(v string) error {
		h, m, ok := strings.Cut(v, ":")
		hh, err1 := strconv.Atoi(h)
		mm, err2 := strconv.Atoi(m)
		if !ok || err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 || len(h) > 2 || len(m) != 2 {
			return bad("%q: want a time like 09:00", v)
		}
		s.hour, s.minute = hh, mm
		return nil
	}
	preset := func() error {
		if span != 0 {
			return bad("a preset's span is its calendar's: give a span only with a cron expression")
		}
		return nil
	}
	switch {
	case strings.HasPrefix(low, "daily@"):
		s.kind = daily
		if err := clock(strings.TrimPrefix(low, "daily@")); err != nil {
			return s, err
		}
		return s, preset()
	case strings.HasPrefix(low, "weekly "):
		s.kind = weekly
		day, at, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(low, "weekly ")), "@")
		i := slices.Index(weekdays, day)
		if !ok || i < 0 {
			return s, bad("%q: want weekly DAY@HH:MM, such as weekly mon@09:00", text)
		}
		s.weekday = time.Weekday(i)
		if err := clock(at); err != nil {
			return s, err
		}
		return s, preset()
	case strings.HasPrefix(low, "monthly "):
		s.kind = monthly
		day, at, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(low, "monthly ")), "@")
		if !ok {
			return s, bad("%q: want monthly D@HH:MM, such as monthly 1@09:00 or monthly last@09:00", text)
		}
		if f := strings.Fields(day); len(f) == 2 {
			n, ok := ordinals[f[0]]
			i := slices.Index(weekdays, f[1])
			if !ok || i < 0 {
				return s, bad("%q: a weekday of the month is first, second, third, fourth or last, and a day, such as first sat", day)
			}
			s.nth, s.weekday = n, time.Weekday(i)
		} else if day != "last" {
			n, err := strconv.Atoi(day)
			if err != nil || n < 1 || n > 28 {
				return s, bad("%q: a day of the month is 1 to 28, or last", day)
			}
			s.monthDay = n
		}
		if err := clock(at); err != nil {
			return s, err
		}
		return s, preset()
	}
	c, err := parseCron(low)
	if err != nil {
		return s, err
	}
	s.kind, s.c = cron, c
	if span < 0 || span > 92*24*time.Hour || span != span.Truncate(time.Minute) {
		return s, bad("a span is whole minutes, up to 92 days")
	}
	if span > 0 && span < time.Hour {
		return s, bad("a span is an hour at least")
	}
	s.span = span
	if !s.c.yearly() {
		return s, bad("%q doesn't fall due every year: give days each month it names has", text)
	}
	return s, nil
}

// at is a time of day in loc: one daylight saving skips is the instant the
// clocks move forward past it; one it repeats, the first.
func at(y int, m time.Month, d, hour, minute int, loc *time.Location) time.Time {
	t := time.Date(y, m, d, hour, minute, 0, 0, loc)
	want := time.Date(y, m, d, hour, minute, 0, 0, time.UTC)
	if got := wall(t); !got.Equal(want) {
		// Skipped. Go gives a time on one side of the jump or the other, as
		// the zone is east or west of UTC: the jump is where t's zone
		// starts, or where it ends.
		start, end := t.ZoneBounds()
		switch {
		case got.After(want) && !start.IsZero():
			return start
		case got.Before(want) && !end.IsZero():
			return end
		}
		return t
	}
	// Repeated: the same time of day, earlier, in the zone before t's.
	start, _ := t.ZoneBounds()
	if start.IsZero() {
		return t
	}
	_, off := t.Zone()
	if _, was := start.Add(-time.Second).Zone(); was > off {
		first := t.Add(-time.Duration(was-off) * time.Second)
		if first.Before(start) && wall(first.In(loc)).Equal(want) {
			return first
		}
	}
	return t
}

// wall is a time's date and time of day, to the minute, as if in UTC.
func wall(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
}

// Last is the latest time the schedule fell due at or before t, in loc.
func (s Schedule) Last(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	y, m, d := t.Date()
	switch s.kind {
	case daily:
		due := at(y, m, d, s.hour, s.minute, loc)
		if due.After(t) {
			due = at(y, m, d-1, s.hour, s.minute, loc)
		}
		return due
	case weekly:
		back := (int(t.Weekday()) - int(s.weekday) + 7) % 7
		due := at(y, m, d-back, s.hour, s.minute, loc)
		if due.After(t) {
			due = at(y, m, d-back-7, s.hour, s.minute, loc)
		}
		return due
	case monthly:
		due := at(y, m, s.dayOf(y, m), s.hour, s.minute, loc)
		if due.After(t) {
			py, pm, _ := time.Date(y, m-1, 1, 0, 0, 0, 0, time.UTC).Date()
			due = at(py, pm, s.dayOf(py, pm), s.hour, s.minute, loc)
		}
		return due
	}
	due, _ := s.c.last(t, loc)
	return due
}

// dayOf is the day of a month the schedule falls on.
func (s Schedule) dayOf(y int, m time.Month) int {
	last := time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
	switch {
	case s.nth > 0:
		first := time.Date(y, m, 1, 12, 0, 0, 0, time.UTC).Weekday()
		return 1 + (int(s.weekday)-int(first)+7)%7 + 7*(s.nth-1)
	case s.nth < 0:
		end := time.Date(y, m, last, 12, 0, 0, 0, time.UTC).Weekday()
		return last - (int(end)-int(s.weekday)+7)%7
	case s.monthDay > 0:
		return s.monthDay
	}
	return last
}

// midnight is when a day starts in loc: its first instant, an hour on where
// the clocks skip midnight.
func midnight(y int, m time.Month, d int, loc *time.Location) time.Time {
	return at(y, m, d, 0, 0, loc)
}

// Span is the span the report that fell due at due covers: for a preset,
// the calendar's before it (the day, the week from Monday, the month); for a
// cron expression, its span before it, or the time since the one before.
func (s Schedule) Span(due time.Time, loc *time.Location) (from, to time.Time) {
	t := due.In(loc)
	y, m, d := t.Date()
	switch s.kind {
	case daily:
		return midnight(y, m, d-1, loc), midnight(y, m, d, loc)
	case weekly:
		back := (int(t.Weekday()) + 6) % 7 // to Monday
		return midnight(y, m, d-back-7, loc), midnight(y, m, d-back, loc)
	case monthly:
		return midnight(y, m-1, 1, loc), midnight(y, m, 1, loc)
	}
	if s.span > 0 {
		return due.Add(-s.span), due
	}
	before, ok := s.c.last(due.Add(-time.Minute), loc)
	if !ok {
		before = due.Add(-24 * time.Hour)
	}
	return before, due
}

// Prev is the time the schedule fell due before due; zero when it isn't
// found, which for a schedule Parse takes it always is.
func (s Schedule) Prev(due time.Time, loc *time.Location) time.Time {
	return s.Last(due.Add(-time.Minute), loc)
}

// Next is the first time the schedule falls due after t, in loc; zero when
// it isn't found within a year and a day, which for a schedule Parse takes
// it always is.
func (s Schedule) Next(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	y, m, d := t.Date()
	switch s.kind {
	case daily:
		for n := 0; n <= 2; n++ {
			if due := at(y, m, d+n, s.hour, s.minute, loc); due.After(t) {
				return due
			}
		}
	case weekly:
		ahead := (int(s.weekday) - int(t.Weekday()) + 7) % 7
		for _, n := range []int{ahead, ahead + 7} {
			if due := at(y, m, d+n, s.hour, s.minute, loc); due.After(t) {
				return due
			}
		}
	case monthly:
		for n := 0; n <= 2; n++ {
			ny, nm, _ := time.Date(y, m+time.Month(n), 1, 0, 0, 0, 0, time.UTC).Date()
			if due := at(ny, nm, s.dayOf(ny, nm), s.hour, s.minute, loc); due.After(t) {
				return due
			}
		}
	case cron:
		due, _ := s.c.next(t, loc)
		return due
	}
	return time.Time{}
}

// gapYears is how many years Gap looks over: a leap year's cycle, so that
// a February a year after from doesn't hide a shorter one later.
const gapYears = 4

// Gap is the shortest time between two times the schedule falls due, over
// the four years after from, in loc: daylight saving and February can make
// it shorter than the calendar of one year says. Zero when it falls due
// once or never.
func (s Schedule) Gap(from time.Time, loc *time.Location) time.Duration {
	var gap time.Duration
	end := from.AddDate(gapYears, 0, 1)
	prev := s.Next(from.Add(-time.Second), loc)
	for n := 0; !prev.IsZero() && prev.Before(end) && n < gapYears*9000; n++ {
		next := s.Next(prev, loc)
		if next.IsZero() {
			break
		}
		if d := next.Sub(prev); gap == 0 || d < gap {
			gap = d
		}
		prev = next
	}
	return gap
}

// Before is the span before the one that fell due at due, for a report to
// set beside it: for a preset, the calendar's before (the day, the week, the
// month before); for a cron expression, the span of the time it fell due
// before. With a span given, which may be longer than the time between two,
// it is a span as long, just before, so the two never overlap.
func (s Schedule) Before(due time.Time, loc *time.Location) (from, to time.Time) {
	if prev := s.Prev(due, loc); !prev.IsZero() && (s.kind != cron || s.span == 0) {
		return s.Span(prev, loc)
	}
	from, to = s.Span(due, loc)
	return from.Add(-to.Sub(from)), from
}

// cronExpr is five cron fields, as sets.
type cronExpr struct {
	minute, hour, dom, month, dow []bool
	// domAny and dowAny say a field was *: as cron has it, when both day
	// fields are given, a day is due when either is.
	domAny, dowAny bool
}

func parseCron(text string) (*cronExpr, error) {
	f := strings.Fields(text)
	if len(f) != 5 {
		return nil, bad("%q: a cron expression is five fields: minute hour day-of-month month day-of-week", text)
	}
	c := &cronExpr{}
	var err error
	if c.minute, err = field("minute", f[0], 0, 59, nil); err != nil {
		return nil, err
	}
	if n := count(c.minute); n != 1 {
		return nil, bad("a report is sent hourly at most: give the minute as one minute, such as 0")
	}
	if c.hour, err = field("hour", f[1], 0, 23, nil); err != nil {
		return nil, err
	}
	if c.dom, err = field("day of month", f[2], 1, 31, nil); err != nil {
		return nil, err
	}
	if c.month, err = field("month", f[3], 1, 12, months); err != nil {
		return nil, err
	}
	if c.dow, err = field("day of week", f[4], 0, 7, weekdays); err != nil {
		return nil, err
	}
	if c.dow[7] {
		c.dow[0] = true // 7 is Sunday too
	}
	c.domAny, c.dowAny = f[2] == "*", f[4] == "*"
	return c, nil
}

func count(set []bool) int {
	n := 0
	for _, b := range set {
		if b {
			n++
		}
	}
	return n
}

// field reads one cron field, named name: *, N, N-M, with /STEP, in lists;
// names for months and days of the week.
func field(name, v string, lo, hi int, names []string) ([]bool, error) {
	set := make([]bool, hi+1)
	num := func(x string) (int, error) {
		if i := slices.Index(names, x); names != nil && i >= 0 {
			return i, nil
		}
		n, err := strconv.Atoi(x)
		if err != nil || n < lo || n > hi {
			return 0, bad("%s: %q: want %d to %d", name, x, lo, hi)
		}
		return n, nil
	}
	parts := strings.Split(v, ",")
	if len(parts) > 60 {
		return nil, bad("%s: 60 parts at most", name)
	}
	for _, part := range parts {
		rng, stepText, stepped := strings.Cut(part, "/")
		step := 1
		if stepped {
			n, err := strconv.Atoi(stepText)
			if err != nil || n < 1 || n > hi {
				return nil, bad("%s: %q: a step is 1 to %d", name, stepText, hi)
			}
			step = n
		}
		from, to := lo, hi
		switch {
		case rng == "*":
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err error
			if from, err = num(a); err != nil {
				return nil, err
			}
			if to, err = num(b); err != nil {
				return nil, err
			}
			if to < from {
				return nil, bad("%s: %q: a range goes up", name, rng)
			}
		default:
			n, err := num(rng)
			if err != nil {
				return nil, err
			}
			from, to = n, n
			if stepped {
				to = hi
			}
		}
		for i := from; i <= to; i += step {
			set[i] = true
		}
	}
	return set, nil
}

// shortest is how many days each month has at fewest.
var shortest = []int{0, 31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}

// yearly reports whether the expression falls due every year: on a day of
// the week, or on a day of the month a month it names always has.
func (c *cronExpr) yearly() bool {
	for m := 1; m <= 12; m++ {
		if !c.month[m] {
			continue
		}
		if !c.dowAny && count(c.dow) > 0 {
			return true
		}
		for d := 1; d <= shortest[m]; d++ {
			if c.dom[d] {
				return true
			}
		}
	}
	return false
}

// day reports whether a day is one the expression falls due on.
func (c *cronExpr) day(t time.Time) bool {
	if !c.month[int(t.Month())] {
		return false
	}
	dom, dow := c.dom[t.Day()], c.dow[int(t.Weekday())]
	switch {
	case c.domAny && c.dowAny:
		return true
	case c.domAny:
		return dow
	case c.dowAny:
		return dom
	}
	return dom || dow
}

// next is the first time after t the expression falls due at, in loc,
// looking ahead lookBack days.
func (c *cronExpr) next(t time.Time, loc *time.Location) (time.Time, bool) {
	t = t.In(loc)
	y, m, d := t.Date()
	minute := slices.Index(c.minute, true)
	for ahead := 0; ahead <= lookBack; ahead++ {
		noon := time.Date(y, m, d+ahead, 12, 0, 0, 0, loc)
		if !c.day(noon) {
			continue
		}
		dy, dm, dd := noon.Date()
		for h := 0; h < 24; h++ {
			if !c.hour[h] {
				continue
			}
			if due := at(dy, dm, dd, h, minute, loc); due.After(t) {
				return due, true
			}
		}
	}
	return time.Time{}, false
}

// last is the latest time at or before t the expression falls due at, in
// loc, looking back lookBack days.
func (c *cronExpr) last(t time.Time, loc *time.Location) (time.Time, bool) {
	t = t.In(loc)
	y, m, d := t.Date()
	minute := slices.Index(c.minute, true)
	for back := 0; back <= lookBack; back++ {
		noon := time.Date(y, m, d-back, 12, 0, 0, 0, loc)
		if !c.day(noon) {
			continue
		}
		dy, dm, dd := noon.Date()
		for h := 23; h >= 0; h-- {
			if !c.hour[h] {
				continue
			}
			due := at(dy, dm, dd, h, minute, loc)
			if !due.After(t) {
				return due, true
			}
		}
	}
	return time.Time{}, false
}
