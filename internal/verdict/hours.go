package verdict

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Hours say when people are at work and when they are asleep, so that a
// firing can be weighed by when it came: one that caught nothing at three in
// the morning cost more than one at three in the afternoon.
type Hours struct {
	Loc *time.Location
	// Days are the days people work, by time.Weekday; Work is the hours of
	// those days they are at work, and Sleep the hours they are asleep, every
	// day. A Sleep that ends before it starts runs past midnight.
	Days        [7]bool
	Work, Sleep Span
}

// Span is part of a day, in minutes after midnight: From up to, not
// including, To. An empty span is none of the day.
type Span struct{ From, To int }

func (s Span) has(m int) bool {
	if s.From <= s.To {
		return s.From <= m && m < s.To
	}
	return m >= s.From || m < s.To
}

func (s Span) String() string {
	if s.From == s.To {
		return "none"
	}
	hm := func(m int) string { return fmt.Sprintf("%02d:%02d", m/60, m%60) }
	return hm(s.From) + "-" + hm(s.To)
}

// The hours a firing can come in.
const (
	AtWork   = "at_work"
	OffHours = "off_hours"
	Asleep   = "asleep"
)

// DefaultHours are close to PagerDuty's: at work from 8:00 to 18:00, Monday
// to Friday, and asleep from 22:00 to 8:00. PagerDuty counts 21:00 to 22:00
// as both off hours and asleep; here each minute is one or the other.
func DefaultHours(loc *time.Location) *Hours {
	h := &Hours{Loc: loc, Work: Span{8 * 60, 18 * 60}, Sleep: Span{22 * 60, 8 * 60}}
	for d := time.Monday; d <= time.Friday; d++ {
		h.Days[d] = true
	}
	return h
}

// Of says which hours t came in. Asleep wins where it and at work overlap.
func (h *Hours) Of(t time.Time) string {
	t = t.In(h.Loc)
	m := t.Hour()*60 + t.Minute()
	switch {
	case h.Sleep.has(m):
		return Asleep
	case h.Days[t.Weekday()] && h.Work.has(m):
		return AtWork
	}
	return OffHours
}

var dayNames = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// WorkText is the working hours as --work-hours takes them.
func (h *Hours) WorkText() string {
	if h.Work.From == h.Work.To {
		return "none"
	}
	var runs []string
	for d := 1; d <= 7; d++ { // from Monday
		if !h.Days[d%7] || h.Days[(d+6)%7] && d > 1 {
			continue
		}
		e := d
		for e < 7 && h.Days[(e+1)%7] {
			e++
		}
		if e == d {
			runs = append(runs, dayNames[d%7])
		} else {
			runs = append(runs, dayNames[d%7]+"-"+dayNames[e%7])
		}
	}
	if len(runs) == 0 {
		return "none"
	}
	return strings.Join(runs, ",") + "@" + h.Work.String()
}

// SleepText is the hours asleep as --sleep-hours takes them.
func (h *Hours) SleepText() string { return h.Sleep.String() }

// ParseWork reads working hours, like mon-fri@08:00-18:00 or
// mon,wed,fri@09:00-17:30; without days, Monday to Friday. "none" is no
// working hours: every hour awake is off hours.
func (h *Hours) ParseWork(s string) error {
	if s == "none" {
		h.Days, h.Work = [7]bool{}, Span{}
		return nil
	}
	want := fmt.Errorf("%q: want days and hours, like mon-fri@08:00-18:00, or none", s)
	days, hours, ok := strings.Cut(s, "@")
	if !ok {
		days, hours = "mon-fri", s
	}
	var on [7]bool
	for _, part := range strings.Split(days, ",") {
		a, b, isRange := strings.Cut(strings.TrimSpace(part), "-")
		i, j := dayIndex(a), dayIndex(b)
		if !isRange {
			j = i
		}
		if i < 0 || j < 0 {
			return want
		}
		for d := i; ; d = (d + 1) % 7 {
			on[d] = true
			if d == j {
				break
			}
		}
	}
	sp, err := parseSpan(hours)
	if err != nil || sp.From >= sp.To {
		return fmt.Errorf("%q: want days and hours within a day, like mon-fri@08:00-18:00, or none", s)
	}
	h.Days, h.Work = on, sp
	return nil
}

// ParseSleep reads the hours asleep, like 22:00-08:00, or "none".
func (h *Hours) ParseSleep(s string) error {
	if s == "none" {
		h.Sleep = Span{}
		return nil
	}
	sp, err := parseSpan(s)
	if err != nil || sp.From == sp.To {
		return fmt.Errorf("%q: want hours like 22:00-08:00, or none", s)
	}
	h.Sleep = sp
	return nil
}

func dayIndex(s string) int {
	for i, d := range dayNames {
		if strings.EqualFold(s, d) {
			return i
		}
	}
	return -1
}

func parseSpan(s string) (Span, error) {
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return Span{}, fmt.Errorf("no -")
	}
	x, err := minutes(a)
	if err != nil {
		return Span{}, err
	}
	y, err := minutes(b)
	if err != nil {
		return Span{}, err
	}
	if x == 24*60 {
		return Span{}, fmt.Errorf("starts at 24:00")
	}
	return Span{x, y}, nil
}

func minutes(s string) (int, error) {
	hh, mm, ok := strings.Cut(strings.TrimSpace(s), ":")
	h, err1 := strconv.Atoi(hh)
	m, err2 := strconv.Atoi(mm)
	if !ok || err1 != nil || err2 != nil || len(mm) != 2 || h < 0 || m < 0 || m > 59 || h > 24 || h == 24 && m != 0 {
		return 0, fmt.Errorf("%q: want a time like 08:00", s)
	}
	return h*60 + m, nil
}

// HoursUsed are the hours a report was weighed by, for a reader.
type HoursUsed struct {
	Zone  string `json:"zone"`
	Work  string `json:"work"`
	Sleep string `json:"sleep"`
}

// Used are the hours, for a reader; nil without hours.
func (h *Hours) Used() *HoursUsed {
	if h == nil {
		return nil
	}
	return &HoursUsed{Zone: h.Loc.String(), Work: h.WorkText(), Sleep: h.SleepText()}
}

// Count adds a firing that came at t, caught something or not, to a rule's
// tally of when its firings came.
func (h *Hours) Count(r *RuleResult, t time.Time, white bool) {
	if h == nil {
		return
	}
	switch h.Of(t) {
	case OffHours:
		r.OffHours++
		if !white {
			r.OffHoursBlack++
		}
	case Asleep:
		r.Asleep++
		if !white {
			r.AsleepBlack++
		}
	}
}

// HourSpan is a stretch of time all in one kind of hours.
type HourSpan struct {
	From, To time.Time
	Of       string
}

// Spans splits [from, to) into the stretches off hours and asleep, none
// across a midnight in h.Loc; the hours at work are left out.
func (h *Hours) Spans(from, to time.Time) []HourSpan {
	lo, hi := from.Unix(), to.Unix()
	if lo >= hi {
		return nil
	}
	// The kind of hours can change only on a quarter of an hour, since every
	// zone is a whole number of quarters from UTC, or at an edge of the
	// hours.
	const q = 15 * 60
	edges := []int64{lo, hi}
	for t := (lo/q + 1) * q; t < hi; t += q {
		edges = append(edges, t)
	}
	// Each day by its date, read at noon: a zone whose clocks change at
	// midnight has no midnight that day. A wall time that comes twice, as the
	// clocks go back, is an edge both times.
	f := from.In(h.Loc)
	for i := -1; ; i++ {
		noon := time.Date(f.Year(), f.Month(), f.Day()+i, 12, 0, 0, 0, h.Loc)
		if noon.After(to.Add(24 * time.Hour)) {
			break
		}
		y, mo, d := noon.Date()
		for _, m := range []int{h.Work.From, h.Work.To, h.Sleep.From, h.Sleep.To} {
			t := time.Date(y, mo, d, 0, m, 0, 0, h.Loc)
			for _, shift := range []time.Duration{0, -time.Hour, -30 * time.Minute, 30 * time.Minute, time.Hour} {
				u := t.Add(shift)
				if lu, lt := u.In(h.Loc), t.In(h.Loc); shift != 0 && (lu.Hour() != lt.Hour() || lu.Minute() != lt.Minute()) {
					continue
				}
				if x := u.Unix(); x > lo && x < hi {
					edges = append(edges, x)
				}
			}
		}
	}
	slices.Sort(edges)
	edges = slices.Compact(edges)
	var out []HourSpan
	for i := 0; i+1 < len(edges); i++ {
		a, b := time.Unix(edges[i], 0), time.Unix(edges[i+1], 0)
		of := h.Of(a)
		if of == AtWork {
			continue
		}
		if n := len(out); n > 0 && out[n-1].To.Equal(a) && out[n-1].Of == of && sameDay(out[n-1].From, a, h.Loc) {
			out[n-1].To = b
			continue
		}
		out = append(out, HourSpan{a, b, of})
	}
	return out
}

func sameDay(a, b time.Time, loc *time.Location) bool {
	y1, m1, d1 := a.In(loc).Date()
	y2, m2, d2 := b.In(loc).Date()
	return y1 == y2 && m1 == m2 && d1 == d2
}
