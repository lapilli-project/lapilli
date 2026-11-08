package verdict

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Shifts are hours that take turns, for a team on call in shifts, or by
// turns across time zones: a firing is weighed by the hours of whoever's
// turn it came in. Each turn lasts Every, the first from From, and they
// come round in order, before From too.
type Shifts struct {
	From  time.Time
	Every time.Duration
	// Days is Every in days, when it is a number of them: those turns change
	// at From's time of day on the clock where the shifts are kept, the
	// clocks changing or not; 0 for turns of hours.
	Days  int
	Turns []*Hours
	text  string
}

// The most turns, and the longest and shortest turn.
const (
	maxTurns = 24
	minTurn  = time.Hour
	maxTurn  = 8 * 7 * 24 * time.Hour
)

// ParseShifts reads shifts like
//
//	every=1w from=2026-11-02T09:00; tz=Asia/Seoul; tz=Europe/Berlin sleep=23:00-07:00
//
// first how long each turn is (hours, days or weeks) and when the first
// starts, on base's clock, and then each turn's hours: work=, sleep= and tz=
// as --work-hours, --sleep-hours and --tz take them, each left out base's.
func ParseShifts(s string, base *Hours) (*Shifts, error) {
	parts := strings.Split(s, ";")
	want := func(what string) error {
		return fmt.Errorf("%q: %s; want shifts like every=1w from=2026-11-02T09:00; tz=Asia/Seoul; tz=Europe/Berlin", s, what)
	}
	if len(parts) < 2 {
		return nil, want("no turns")
	}
	if len(parts)-1 > maxTurns {
		return nil, want(fmt.Sprintf("more than %d turns", maxTurns))
	}
	sh := &Shifts{}
	var head []string
	for _, f := range strings.Fields(parts[0]) {
		k, v, _ := strings.Cut(f, "=")
		switch k {
		case "every":
			d, days, err := turnLength(v)
			if err != nil {
				return nil, want(err.Error())
			}
			sh.Every, sh.Days = d, days
		case "from":
			layout := "2006-01-02T15:04"
			if !strings.Contains(v, "T") {
				layout = "2006-01-02"
			}
			t, err := time.ParseInLocation(layout, v, base.Loc)
			if err != nil {
				return nil, want(fmt.Sprintf("from %q isn't a time like 2026-11-02T09:00", v))
			}
			sh.From = t
		default:
			return nil, want(fmt.Sprintf("%q isn't every= or from=", f))
		}
		head = append(head, f)
	}
	if sh.Every == 0 || sh.From.IsZero() {
		return nil, want("every= and from= first")
	}
	texts := []string{strings.Join(head, " ")}
	for _, p := range parts[1:] {
		h := *base
		h.Shifts = nil
		var work, sleep string
		var fields []string
		for _, f := range strings.Fields(p) {
			k, v, _ := strings.Cut(f, "=")
			switch k {
			case "tz":
				loc, err := time.LoadLocation(v)
				if err != nil || v == "Local" || v == "" || len(v) > 64 {
					return nil, want(fmt.Sprintf("%q isn't a time zone", v))
				}
				h.Loc = loc
			case "work":
				work = v
			case "sleep":
				sleep = v
			default:
				return nil, want(fmt.Sprintf("%q isn't work=, sleep= or tz=", f))
			}
			fields = append(fields, f)
		}
		if work != "" {
			if err := h.ParseWork(work); err != nil {
				return nil, want(err.Error())
			}
		}
		if sleep != "" {
			if err := h.ParseSleep(sleep); err != nil {
				return nil, want(err.Error())
			}
		}
		sh.Turns = append(sh.Turns, &h)
		texts = append(texts, strings.Join(fields, " "))
	}
	sh.text = strings.Join(texts, "; ")
	return sh, nil
}

// turnLength reads a turn's length: a whole number of hours, days or
// weeks.
func turnLength(s string) (time.Duration, int, error) {
	if len(s) < 2 {
		return 0, 0, fmt.Errorf("every %q isn't a length like 8h, 1d or 1w", s)
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	unit := map[byte]time.Duration{'h': time.Hour, 'd': 24 * time.Hour, 'w': 7 * 24 * time.Hour}[s[len(s)-1]]
	if err != nil || unit == 0 || n < 1 || n > int(maxTurn/unit) {
		return 0, 0, fmt.Errorf("every %q isn't a length from 1h to 8w, like 8h, 1d or 1w", s)
	}
	d := time.Duration(n) * unit
	if unit == time.Hour {
		return d, 0, nil
	}
	return d, int(d / (24 * time.Hour)), nil
}

func (s *Shifts) String() string { return s.text }

// turn is the turn t comes in: which, and when it starts and ends.
func (s *Shifts) turn(t time.Time) (int, time.Time, time.Time) {
	// Near enough, from the seconds between, which don't run out as a
	// Duration does; then to the turn.
	k := int64(math.Floor(float64(t.Unix()-s.From.Unix()) / s.Every.Seconds()))
	at := func(k int64) time.Time { return s.From.Add(time.Duration(k) * s.Every) }
	if s.Days != 0 {
		f := s.From
		at = func(k int64) time.Time {
			return time.Date(f.Year(), f.Month(), f.Day()+int(k)*s.Days, f.Hour(), f.Minute(), 0, 0, f.Location())
		}
	}
	for at(k).After(t) {
		k--
	}
	for !at(k + 1).After(t) {
		k++
	}
	n := int64(len(s.Turns))
	return int((k%n + n) % n), at(k), at(k + 1)
}

// of is the hours of the turn t comes in.
func (s *Shifts) of(t time.Time) *Hours {
	i, _, _ := s.turn(t)
	return s.Turns[i]
}
