package routing

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A TimeInterval is one of Alertmanager's time intervals as
// timeinterval.TimeInterval has it: it contains an instant when every part
// it sets does. A part left out (nil, not an empty list) is any. A named
// time interval is a list of these, and contains an instant when any does.
type TimeInterval struct {
	// Times are ranges of minutes of the day, [Start, End).
	Times []TimeRange
	// Weekdays are ranges of days of the week, 0 for Sunday; a range that
	// begins after it ends wraps (friday:monday).
	Weekdays []Range
	// DaysOfMonth are ranges of days of the month; a negative day counts
	// from the month's end (-1 is its last).
	DaysOfMonth []Range
	// Months are ranges of months, 1 to 12; Years, ranges of years.
	Months, Years []Range
	// Location is the time zone the parts above are read in; nil is UTC.
	Location *time.Location
}

// A TimeRange is a range of minutes of the day, from Start up to End.
type TimeRange struct{ Start, End int }

// A Range is a range of numbers, both ends included.
type Range struct{ Begin, End int }

var timeOfDay = regexp.MustCompile(`^((([01][0-9])|(2[0-3])):[0-5][0-9])$|(^24:00$)`)

// zones caches time.LoadLocation by name: reading a zone's rules off the disk
// for each time interval that names it is what a configuration of thousands
// of them would make the reader do. A name that doesn't load isn't kept, and
// the cache holds at most maxZones names.
var zones struct {
	sync.Mutex
	by map[string]*time.Location
}

const maxZones = 1024

func loadLocation(name string) (*time.Location, error) {
	zones.Lock()
	loc, ok := zones.by[name]
	zones.Unlock()
	if ok {
		return loc, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, err
	}
	zones.Lock()
	if zones.by == nil {
		zones.by = map[string]*time.Location{}
	}
	if len(zones.by) < maxZones {
		zones.by[name] = loc
	}
	zones.Unlock()
	return loc, nil
}

// Parts is how many ranges, weekdays, days, months and years the time
// interval lists, and one for itself.
func (ti TimeInterval) Parts() int {
	return 1 + len(ti.Times) + len(ti.Weekdays) + len(ti.DaysOfMonth) + len(ti.Months) + len(ti.Years)
}

// maxIntervalParts bounds the ranges one time interval may list.
const maxIntervalParts = 1000

// ParseTimeInterval reads a time interval as Alertmanager's configuration
// and Grafana's mute-timings API write it, decoded from YAML or JSON into
// maps, lists and scalars: times ([{start_time: "09:00", end_time:
// "17:00"}]), weekdays (["monday:friday", "sunday"], by name), days_of_month
// (["1:15", "-1"]), months (["1:3", "december"]), years (["2025:2026"]) and
// location ("Asia/Seoul"). The errors are Alertmanager's: it won't load a
// configuration with one.
func ParseTimeInterval(v any) (TimeInterval, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return TimeInterval{}, errors.New("a time interval isn't a map")
	}
	var ti TimeInterval
	for k, val := range m {
		if val == nil {
			continue
		}
		list, isList := val.([]any)
		if k != "location" && !isList {
			return TimeInterval{}, fmt.Errorf("time interval's %s isn't a list", k)
		}
		if len(list) > maxIntervalParts {
			return TimeInterval{}, fmt.Errorf("time interval's %s has more than %d parts", k, maxIntervalParts)
		}
		switch k {
		case "times":
			ti.Times = []TimeRange{}
			for _, x := range list {
				xm, ok := x.(map[string]any)
				if !ok {
					return TimeInterval{}, errors.New("a time range isn't a map")
				}
				s, e := scalar(xm["start_time"]), scalar(xm["end_time"])
				if s == "" || e == "" {
					return TimeInterval{}, errors.New("both start and end times must be provided")
				}
				sm, err := minuteOfDay(s)
				if err != nil {
					return TimeInterval{}, err
				}
				em, err := minuteOfDay(e)
				if err != nil {
					return TimeInterval{}, err
				}
				if sm >= em {
					return TimeInterval{}, errors.New("start time cannot be equal or greater than end time")
				}
				ti.Times = append(ti.Times, TimeRange{sm, em})
			}
		case "weekdays":
			ti.Weekdays = []Range{}
			for _, x := range list {
				r, err := parseRange(scalar(x), weekdayNumber)
				if err != nil {
					return TimeInterval{}, err
				}
				if r.Begin < 0 || r.Begin > 6 || r.End < 0 || r.End > 6 {
					return TimeInterval{}, fmt.Errorf("%s is not a valid day of the week: out of range", scalar(x))
				}
				ti.Weekdays = append(ti.Weekdays, r)
			}
		case "days_of_month":
			ti.DaysOfMonth = []Range{}
			for _, x := range list {
				r, err := parseRange(scalar(x), plainNumber)
				if err != nil {
					return TimeInterval{}, err
				}
				if r.Begin == 0 || r.Begin < -31 || r.Begin > 31 || r.End == 0 || r.End < -31 || r.End > 31 {
					return TimeInterval{}, fmt.Errorf("%s is not a valid day of the month: out of range", scalar(x))
				}
				if r.Begin < 0 && r.End > 0 {
					return TimeInterval{}, errors.New("end day must be negative if start day is negative")
				}
				b, e := r.Begin, r.End
				if b < 0 {
					b += 28
				}
				if e < 0 {
					e += 28
				}
				if b > e {
					return TimeInterval{}, fmt.Errorf("end day %d is always before start day %d", r.End, r.Begin)
				}
				ti.DaysOfMonth = append(ti.DaysOfMonth, r)
			}
		case "months":
			ti.Months = []Range{}
			for _, x := range list {
				r, err := parseRange(scalar(x), monthNumber)
				if err != nil {
					return TimeInterval{}, err
				}
				if r.Begin < 1 || r.Begin > 12 || r.End < 1 || r.End > 12 {
					return TimeInterval{}, fmt.Errorf("%s is not a valid month: out of range", scalar(x))
				}
				if r.Begin > r.End {
					return TimeInterval{}, fmt.Errorf("end month %d is before start month %d", r.End, r.Begin)
				}
				ti.Months = append(ti.Months, r)
			}
		case "years":
			ti.Years = []Range{}
			for _, x := range list {
				r, err := parseRange(scalar(x), plainNumber)
				if err != nil {
					return TimeInterval{}, err
				}
				if r.Begin > r.End {
					return TimeInterval{}, fmt.Errorf("end year %d is before start year %d", r.End, r.Begin)
				}
				ti.Years = append(ti.Years, r)
			}
		case "location":
			name := scalar(val)
			if len(name) > MaxName {
				return TimeInterval{}, fmt.Errorf("a time zone's name is %d bytes at most", MaxName)
			}
			loc, err := loadLocation(name)
			if err != nil {
				return TimeInterval{}, err
			}
			ti.Location = loc
		default:
			// Alertmanager's configuration is strict: a field it doesn't
			// know is an error there. Here it is passed over.
		}
	}
	return ti, nil
}

// ParseTimeIntervals reads a named time interval's list of time intervals.
func ParseTimeIntervals(v any) ([]TimeInterval, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, errors.New("time_intervals isn't a list")
	}
	if len(list) > maxIntervalParts {
		return nil, fmt.Errorf("more than %d time intervals under one name", maxIntervalParts)
	}
	out := make([]TimeInterval, 0, len(list))
	for _, x := range list {
		ti, err := ParseTimeInterval(x)
		if err != nil {
			return nil, err
		}
		out = append(out, ti)
	}
	return out, nil
}

// scalar is a decoded YAML or JSON scalar as the text it was written as.
func scalar(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case uint64:
		return strconv.FormatUint(v, 10)
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
	return ""
}

func minuteOfDay(s string) (int, error) {
	if !timeOfDay.MatchString(s) {
		return 0, fmt.Errorf("couldn't parse timestamp %s, invalid format", s)
	}
	h, _ := strconv.Atoi(s[:2])
	m, _ := strconv.Atoi(s[3:])
	return h*60 + m, nil
}

var weekdays = map[string]int{"sunday": 0, "monday": 1, "tuesday": 2, "wednesday": 3, "thursday": 4, "friday": 5, "saturday": 6}

var monthNames = map[string]int{"january": 1, "february": 2, "march": 3, "april": 4, "may": 5, "june": 6,
	"july": 7, "august": 8, "september": 9, "october": 10, "november": 11, "december": 12}

func weekdayNumber(s string) (int, error) {
	n, ok := weekdays[s]
	if !ok {
		return 0, fmt.Errorf("%s is not a valid weekday", s)
	}
	return n, nil
}

func monthNumber(s string) (int, error) {
	if n, ok := monthNames[s]; ok {
		return n, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s is not a valid month", s)
	}
	return n, nil
}

func plainNumber(s string) (int, error) { return strconv.Atoi(s) }

// parseRange reads "a:b", or a alone, as Alertmanager does
// (stringableRangeFromString); a weekday or a month may be a name.
func parseRange(s string, member func(string) (int, error)) (Range, error) {
	s = strings.ToLower(s)
	if s == "" {
		return Range{}, errors.New("an empty range")
	}
	if strings.Contains(s, ":") {
		parts := strings.Split(s, ":")
		if len(parts) != 2 {
			return Range{}, fmt.Errorf("couldn't parse range %s, invalid format", s)
		}
		b, err := member(parts[0])
		if err != nil {
			return Range{}, err
		}
		e, err := member(parts[1])
		if err != nil {
			return Range{}, err
		}
		return Range{b, e}, nil
	}
	n, err := member(s)
	if err != nil {
		return Range{}, err
	}
	return Range{n, n}, nil
}

// ContainsTime reports whether an instant is in the time interval
// (timeinterval.TimeInterval.ContainsTime).
func (ti TimeInterval) ContainsTime(t time.Time) bool {
	if ti.Location != nil {
		t = t.In(ti.Location)
	} else {
		t = t.UTC()
	}
	if ti.Times != nil {
		in := false
		min := t.Hour()*60 + t.Minute()
		for _, r := range ti.Times {
			if min >= r.Start && min < r.End {
				in = true
				break
			}
		}
		if !in {
			return false
		}
	}
	if ti.DaysOfMonth != nil {
		in := false
		dim := time.Date(t.Year(), t.Month()+1, 0, 12, 0, 0, 0, t.Location()).Day()
		for _, r := range ti.DaysOfMonth {
			begin, end := r.Begin, r.End
			if begin < 0 {
				begin = dim + begin + 1
			}
			if end < 0 {
				end = dim + end + 1
			}
			if begin > dim {
				continue
			}
			// The clamp keeps the range within the month, as Alertmanager's
			// does: a day past its end counts as the last, one before its
			// start as the first.
			begin, end = clamp(begin, -dim, dim), clamp(end, -dim, dim)
			if t.Day() >= begin && t.Day() <= end {
				in = true
				break
			}
		}
		if !in {
			return false
		}
	}
	if ti.Months != nil {
		in := false
		for _, r := range ti.Months {
			if t.Month() >= time.Month(r.Begin) && t.Month() <= time.Month(r.End) {
				in = true
				break
			}
		}
		if !in {
			return false
		}
	}
	if ti.Weekdays != nil {
		in := false
		for _, r := range ti.Weekdays {
			if r.Begin <= r.End {
				if t.Weekday() >= time.Weekday(r.Begin) && t.Weekday() <= time.Weekday(r.End) {
					in = true
					break
				}
			} else if t.Weekday() >= time.Weekday(r.Begin) || t.Weekday() <= time.Weekday(r.End) {
				in = true
				break
			}
		}
		if !in {
			return false
		}
	}
	if ti.Years != nil {
		in := false
		for _, r := range ti.Years {
			if t.Year() >= r.Begin && t.Year() <= r.End {
				in = true
				break
			}
		}
		if !in {
			return false
		}
	}
	return true
}

func clamp(n, lo, hi int) int {
	if n <= lo {
		return lo
	}
	if n >= hi {
		return hi
	}
	return n
}

// containsAny reports whether any of the time intervals contains t.
func containsAny(tis []TimeInterval, t time.Time) bool {
	for _, ti := range tis {
		if ti.ContainsTime(t) {
			return true
		}
	}
	return false
}

// MaxLookAhead is how far nextChange looks for a time interval to change:
// no further than a year and a bit, however long the period.
const MaxLookAhead = 400 * 24 * time.Hour

// nextChange is the first instant after t at which whether the time
// intervals (the list a name stands for) contain it differs from whether
// they contain t, looked for as far as maxLook (and MaxLookAhead at most);
// the zero time if there is none within that. A part of an interval changes
// on a minute of a local day, so those are the only instants it looks at.
func nextChange(tis []TimeInterval, t time.Time, maxLook time.Duration) time.Time {
	at, _, _ := nextChangeWork(tis, t, maxLook, 0)
	return at
}

// nextChangeWork is nextChange, and the work it took (the time intervals it
// asked about, over the instants it asked of); with a limit over zero, it
// gives up when the work is more, and says so (the zero time, and true).
func nextChangeWork(tis []TimeInterval, t time.Time, maxLook time.Duration, limit int) (time.Time, int, bool) {
	was := containsAny(tis, t)
	work := len(tis)
	if maxLook > MaxLookAhead {
		maxLook = MaxLookAhead
	}
	end := t.Add(maxLook)
	// The minutes of the day each time zone's parts can change at, by the
	// zone's name: two pointers to one zone are one.
	minutes := map[string]map[int]bool{}
	var zones []*time.Location
	for _, ti := range tis {
		loc := ti.Location
		if loc == nil {
			loc = time.UTC
		}
		key := loc.String()
		if minutes[key] == nil {
			minutes[key] = map[int]bool{0: true}
			zones = append(zones, loc)
		}
		for _, r := range ti.Times {
			minutes[key][r.Start] = true
			minutes[key][r.End%1440] = true
		}
	}
	var best time.Time
	for _, loc := range zones {
		ms := make([]int, 0, len(minutes[loc.String()]))
		for m := range minutes[loc.String()] {
			ms = append(ms, m)
		}
		sort.Ints(ms)
		day := t.In(loc)
		for d := 0; ; d++ {
			y, mo, dd := day.Year(), day.Month(), day.Day()+d
			if midnight := time.Date(y, mo, dd, 0, 0, 0, 0, loc); !midnight.Before(end) || !best.IsZero() && midnight.After(best) {
				break
			}
			found := false
			for _, m := range ms {
				at := time.Date(y, mo, dd, m/60, m%60, 0, 0, loc)
				if !at.After(t) || !at.Before(end) {
					continue
				}
				if work += len(tis); limit > 0 && work > limit {
					return time.Time{}, work, true
				}
				if containsAny(tis, at) == was {
					continue
				}
				if best.IsZero() || at.Before(best) {
					best = at
				}
				found = true
				break
			}
			if found {
				break
			}
		}
	}
	return best, work, false
}
