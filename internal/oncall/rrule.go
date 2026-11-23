package oncall

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Repeats (RFC 5545, 3.3.10 and 3.8.5.3). A repeat is worked out on the
// clock it is written in, a wall clock time held in UTC as a stand-in:
// each of its periods (a day, a week, a month or a year, every INTERVAL of
// them) gives its days at DTSTART's time of day, and only then is each made
// an instant in its zone (zone.go). A weekly 09:00 turn so stays at 09:00
// when the clocks change, in an IANA zone and in a feed's own VTIMEZONE
// alike.

type freq int

const (
	daily freq = iota + 1
	weekly
	monthly
	yearly
)

var freqs = map[string]freq{"DAILY": daily, "WEEKLY": weekly, "MONTHLY": monthly, "YEARLY": yearly}

var freqNames = map[freq]string{daily: "daily", weekly: "weekly", monthly: "monthly", yearly: "yearly"}

var weekdays = []string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"}

// rule is an RRULE, read. Its lists are kept as bits, so a value written
// many times costs only its reading, and a period's days are worked out in
// the same few steps whatever the lists hold.
type rule struct {
	freq     freq
	interval int
	count    int    // -1 for no COUNT
	until    string // UNTIL as written: read in the start's zone, once that is known
	wkst     time.Weekday

	// BYDAY: a weekday alone (each of them in the period), or numbered in
	// the month or the year: nth[d][0] has bit n for +n, nth[d][1] bit n
	// for -n, n from 1 to 53.
	byDay   bool
	plain   [7]bool
	nth     [7][2]uint64
	ordinal bool
	// BYMONTHDAY: [0] has bit n for n, [1] bit n for -n, from 1 to 31.
	byMonthDay bool
	monthDay   [2]uint32
	// BYMONTH: bit m.
	months uint16
	// BYSETPOS: [0] has bit n for n, [1] bit n for -n, from 1 to 366.
	bySetPos bool
	setPos   [2][6]uint64
}

// parseRule reads an RRULE: FREQ=DAILY, WEEKLY, MONTHLY or YEARLY, with
// INTERVAL, COUNT, UNTIL, BYDAY (numbered, as 1MO or -1FR, for monthly and
// yearly only), BYMONTHDAY, BYMONTH, BYSETPOS and WKST, each where RFC
// 5545's table of BYxxx parts allows it. Anything else is refused, and the
// event passed over: BYWEEKNO, BYYEARDAY, BYHOUR, BYMINUTE, BYSECOND, and
// repeats more often than daily.
func parseRule(s string) (*rule, error) {
	r := &rule{interval: 1, count: -1, wkst: time.Monday}
	f := ""
	for part := range strings.SplitSeq(s, ";") {
		if k, v, _ := strings.Cut(part, "="); strings.EqualFold(strings.TrimSpace(k), "FREQ") {
			f = strings.ToUpper(strings.TrimSpace(v))
		}
	}
	if r.freq = freqs[f]; r.freq == 0 {
		return nil, fmt.Errorf("a repeat FREQ=%s, which this doesn't expand: only DAILY, WEEKLY, MONTHLY and YEARLY", clip(f))
	}
	for part := range strings.SplitSeq(s, ";") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		k, v = strings.ToUpper(strings.TrimSpace(k)), strings.TrimSpace(v)
		switch k {
		case "FREQ":
		case "INTERVAL":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 1000 {
				return nil, fmt.Errorf("a repeat's INTERVAL %q", clip(v))
			}
			r.interval = n
		case "COUNT":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > maxSteps {
				return nil, fmt.Errorf("a repeat's COUNT %q", clip(v))
			}
			r.count = n
		case "UNTIL":
			r.until = v
		case "WKST":
			i := index(weekdays, strings.ToUpper(v))
			if i < 0 {
				return nil, fmt.Errorf("a repeat's week starting %q", clip(v))
			}
			r.wkst = time.Weekday(i)
		case "BYDAY":
			r.byDay = true
			for d := range strings.SplitSeq(v, ",") {
				n, wd, ok := weekdayNum(d)
				if !ok {
					return nil, fmt.Errorf("a repeat by day %q, which this doesn't expand: a weekday, with a number from 1 to 53 or -53 to -1 or none", clip(d))
				}
				switch {
				case n == 0:
					r.plain[wd] = true
				case n > 0:
					r.nth[wd][0] |= 1 << n
					r.ordinal = true
				default:
					r.nth[wd][1] |= 1 << -n
					r.ordinal = true
				}
			}
		case "BYMONTHDAY":
			r.byMonthDay = true
			for d := range strings.SplitSeq(v, ",") {
				n, ok := signed(d, 31)
				if !ok {
					return nil, fmt.Errorf("a repeat by day of the month %q: want 1 to 31, or -31 to -1", clip(d))
				}
				if n > 0 {
					r.monthDay[0] |= 1 << n
				} else {
					r.monthDay[1] |= 1 << -n
				}
			}
		case "BYMONTH":
			for m := range strings.SplitSeq(v, ",") {
				n, err := strconv.Atoi(strings.TrimSpace(m))
				if err != nil || n < 1 || n > 12 {
					return nil, fmt.Errorf("a repeat by month %q: want 1 to 12", clip(m))
				}
				r.months |= 1 << n
			}
		case "BYSETPOS":
			r.bySetPos = true
			for p := range strings.SplitSeq(v, ",") {
				n, ok := signed(p, 366)
				if !ok {
					return nil, fmt.Errorf("a repeat by BYSETPOS %q: want 1 to 366, or -366 to -1", clip(p))
				}
				side := 0
				if n < 0 {
					side, n = 1, -n
				}
				r.setPos[side][n/64] |= 1 << (n % 64)
			}
		default:
			return nil, fmt.Errorf("a repeat by %s, which this doesn't expand", clip(k))
		}
	}
	// What RFC 5545 says MUST NOT be.
	switch {
	case r.ordinal && (r.freq == daily || r.freq == weekly):
		return nil, fmt.Errorf("a %s repeat by a numbered day, such as 1MO, which RFC 5545 allows only monthly and yearly", freqNames[r.freq])
	case r.byMonthDay && r.freq == weekly:
		return nil, fmt.Errorf("a weekly repeat by day of the month, which RFC 5545 doesn't allow")
	case r.bySetPos && !r.byDay && !r.byMonthDay && r.months == 0:
		return nil, fmt.Errorf("a repeat by BYSETPOS with no other BY part for it to pick from")
	}
	return r, nil
}

func index(list []string, s string) int {
	for i, x := range list {
		if x == s {
			return i
		}
	}
	return -1
}

// weekdayNum reads a BYDAY value: [+|-][n]WD, n from 1 to 53; 0 for none.
func weekdayNum(s string) (int, int, bool) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if len(s) < 2 {
		return 0, 0, false
	}
	wd := index(weekdays, s[len(s)-2:])
	if wd < 0 {
		return 0, 0, false
	}
	if s = s[:len(s)-2]; s == "" {
		return 0, wd, true
	}
	n, ok := signed(s, 53)
	return n, wd, ok
}

// signed reads [+|-]n, n from 1 to most.
func signed(s string, most int) (int, bool) {
	s = strings.TrimSpace(s)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")
	if s == "" || len(s) > 3 || strings.TrimLeft(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > most {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

func daysIn(y int, m time.Month) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// begin is period p's first day, at midnight: the start's day, its week
// (as WKST begins weeks), its month or its year, then every INTERVAL.
func (r *rule) begin(start time.Time, p int) time.Time {
	y, m, d := start.Date()
	switch r.freq {
	case daily:
		return time.Date(y, m, d+p*r.interval, 0, 0, 0, 0, time.UTC)
	case weekly:
		back := (int(start.Weekday()) - int(r.wkst) + 7) % 7
		return time.Date(y, m, d-back+7*p*r.interval, 0, 0, 0, 0, time.UTC)
	case monthly:
		return time.Date(y, m+time.Month(p*r.interval), 1, 0, 0, 0, 0, time.UTC)
	}
	return time.Date(y+p*r.interval, 1, 1, 0, 0, 0, 0, time.UTC)
}

// periodOf is the period w is in, worked out, not stepped to: a rule from
// 1601, as Outlook writes a zone's, costs nothing to bring near now. 0
// for a time before the first.
func (r *rule) periodOf(start, w time.Time) int {
	b := r.begin(start, 0)
	if !w.After(b) {
		return 0
	}
	switch r.freq {
	case daily:
		return int((w.Unix()-b.Unix())/86400) / r.interval
	case weekly:
		return int((w.Unix()-b.Unix())/(7*86400)) / r.interval
	case monthly:
		return ((w.Year()-b.Year())*12 + int(w.Month()) - int(b.Month())) / r.interval
	}
	return (w.Year() - b.Year()) / r.interval
}

// days gives the days of period p of a repeat that starts at start, in
// order, at its time of day, appended to buf[:0]: what the BYxxx parts
// make of the period (RFC 5545's table: BYMONTH limits but for yearly,
// where it expands; BYMONTHDAY expands monthly and yearly; BYDAY expands
// weekly, and monthly and yearly when BYMONTHDAY isn't given, numbered in
// the month, or for yearly without BYMONTH in the year), then BYSETPOS.
// Days a month doesn't have (30 February, the 31st of a month of 30) are
// skipped, not rolled over. Days before the start are still in it. work
// is the steps working them out took: one, or one a month a yearly
// repeat looks at; each day it gives is a step more, as its caller takes
// it.
func (r *rule) days(start time.Time, p int, buf []time.Time) (out []time.Time, work int) {
	b := r.begin(start, p)
	out = buf[:0]
	at := func(y int, m time.Month, d int) time.Time {
		return time.Date(y, m, d, start.Hour(), start.Minute(), start.Second(), start.Nanosecond(), time.UTC)
	}
	work = 1
	switch r.freq {
	case daily:
		y, m, d := b.Date()
		if r.monthOK(m) && (!r.byMonthDay || r.monthDays(y, m)&(1<<d) != 0) && (!r.byDay || r.plain[b.Weekday()]) {
			out = append(out, at(y, m, d))
		}
	case weekly:
		if !r.byDay {
			// The start's weekday, every INTERVAL weeks.
			d := b.AddDate(0, 0, (int(start.Weekday())-int(r.wkst)+7)%7)
			if r.monthOK(d.Month()) {
				out = append(out, at(d.Date()))
			}
			break
		}
		for i := range 7 {
			d := b.AddDate(0, 0, i)
			if r.plain[d.Weekday()] && r.monthOK(d.Month()) {
				out = append(out, at(d.Date()))
			}
		}
	case monthly:
		y, m, _ := b.Date()
		if r.monthOK(m) {
			out = r.inMonth(out, r.monthMask(y, m, start.Day()), y, m, at)
		}
	case yearly:
		y := b.Year()
		// BYDAY without BYMONTH numbers its days in the year.
		var inYear [13]uint32
		numbered := r.byDay && r.months == 0
		if numbered {
			inYear = r.byDayInYear(y)
		}
		work = 0
		for m := time.January; m <= time.December; m++ {
			switch {
			case r.months != 0 && r.months&(1<<m) == 0:
				continue
			case r.months == 0 && !r.byDay && !r.byMonthDay && m != start.Month():
				continue // the start's day of the year
			}
			work++
			mask := inYear[m]
			switch {
			case numbered && r.byMonthDay:
				mask &= r.monthDays(y, m)
			case !numbered:
				mask = r.monthMask(y, m, start.Day())
			}
			out = r.inMonth(out, mask, y, m, at)
		}
		work = max(work, 1)
	}
	if r.bySetPos {
		n := len(out)
		kept := out[:0]
		for i, t := range out {
			if bit6(r.setPos[0], i+1) || bit6(r.setPos[1], n-i) {
				kept = append(kept, t)
			}
		}
		out = kept
	}
	return out, work
}

func bit6(set [6]uint64, n int) bool {
	return n >= 1 && n < 6*64 && set[n/64]&(1<<(n%64)) != 0
}

func (r *rule) monthOK(m time.Month) bool { return r.months == 0 || r.months&(1<<m) != 0 }

// monthMask is the days of a month a monthly repeat holds, or a yearly
// one numbering its days in the month: by BYMONTHDAY, BYDAY limiting it;
// by BYDAY; else the start's day of the month, if the month has it.
func (r *rule) monthMask(y int, m time.Month, day int) uint32 {
	switch {
	case r.byMonthDay && r.byDay:
		return r.monthDays(y, m) & r.byDayInMonth(y, m)
	case r.byMonthDay:
		return r.monthDays(y, m)
	case r.byDay:
		return r.byDayInMonth(y, m)
	case day <= daysIn(y, m):
		return 1 << day
	}
	return 0
}

// monthDays is BYMONTHDAY in a month, bit d for its day d: -1 its last.
func (r *rule) monthDays(y int, m time.Month) uint32 {
	n := daysIn(y, m)
	mask := r.monthDay[0] & uint32(uint64(1)<<(n+1)-2)
	for k := 1; k <= n; k++ {
		if r.monthDay[1]&(1<<k) != 0 {
			mask |= 1 << (n - k + 1)
		}
	}
	return mask
}

// byDayInMonth is BYDAY in a month: each weekday named alone, and the nth
// of a weekday from the month's start or, for -n, from its end.
func (r *rule) byDayInMonth(y int, m time.Month) uint32 {
	n := daysIn(y, m)
	wd1 := int(time.Date(y, m, 1, 0, 0, 0, 0, time.UTC).Weekday())
	var mask uint32
	for d := range 7 {
		if !r.plain[d] && r.nth[d] == [2]uint64{} {
			continue
		}
		first := 1 + (d-wd1+7)%7
		count := (n-first)/7 + 1
		for j := 1; j <= count; j++ {
			if r.plain[d] || r.nth[d][0]&(1<<j) != 0 || r.nth[d][1]&(1<<(count-j+1)) != 0 {
				mask |= 1 << (first + 7*(j-1))
			}
		}
	}
	return mask
}

// byDayInYear is BYDAY in a year, by month: each weekday named alone, and
// the nth of a weekday from the year's start or, for -n, from its end (the
// 20th Monday of the year, RFC 5545's example).
func (r *rule) byDayInYear(y int) [13]uint32 {
	var out [13]uint32
	jan1 := time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)
	days := time.Date(y, 12, 31, 0, 0, 0, 0, time.UTC).YearDay()
	wd1 := int(jan1.Weekday())
	for d := range 7 {
		if !r.plain[d] && r.nth[d] == [2]uint64{} {
			continue
		}
		first := (d - wd1 + 7) % 7
		count := (days-1-first)/7 + 1
		for j := 1; j <= count; j++ {
			if r.plain[d] || r.nth[d][0]&(1<<j) != 0 || r.nth[d][1]&(1<<(count-j+1)) != 0 {
				day := jan1.AddDate(0, 0, first+7*(j-1))
				out[day.Month()] |= 1 << day.Day()
			}
		}
	}
	return out
}

// inMonth appends the days of mask, in order.
func (r *rule) inMonth(out []time.Time, mask uint32, y int, m time.Month, at func(int, time.Month, int) time.Time) []time.Time {
	for d := 1; d <= 31 && mask>>d != 0; d++ {
		if mask&(1<<d) != 0 {
			out = append(out, at(y, m, d))
		}
	}
	return out
}
