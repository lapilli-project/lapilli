package verdict

import (
	"fmt"
	"math"
	"slices"
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
	// wall is From as written, on that clock: a time the clocks skip that
	// day is still the time turns change on the others.
	wall [5]int
	text string
}

// The most turns, the longest turn, and the years a first turn may start
// in.
const (
	maxTurns  = 24
	maxTurn   = 8 * 7 * 24 * time.Hour
	firstYear = 1970
	lastYear  = 2200
)

// A ShiftsError is shifts that aren't, and why.
type ShiftsError struct{ Text, Why string }

func (e *ShiftsError) Error() string {
	return fmt.Sprintf("%q: %s; want shifts like every=1w from=2026-11-02T09:00; tz=Asia/Seoul; tz=Europe/Berlin", e.Text, e.Why)
}

// ParseShifts reads shifts like
//
//	every=1w from=2026-11-02T09:00; tz=Asia/Seoul; tz=Europe/Berlin sleep=23:00-07:00
//
// first how long each turn is (hours, days or weeks) and when the first
// starts, on base's clock, and then each turn's hours: work=, sleep= and tz=
// as --work-hours, --sleep-hours and --tz take them, each left out base's;
// "-" for a turn on base's hours.
func ParseShifts(s string, base *Hours) (*Shifts, error) {
	want := func(format string, a ...any) error { return &ShiftsError{s, fmt.Sprintf(format, a...)} }
	parts := strings.Split(s, ";")
	if len(parts) < 2 {
		return nil, want("no turns")
	}
	if len(parts)-1 > maxTurns {
		return nil, want("more than %d turns", maxTurns)
	}
	// fields reads a part's key=value fields, each key once and with a value.
	fields := func(part string, keys ...string) (map[string]string, []string, error) {
		got := map[string]string{}
		var order []string
		for _, f := range strings.Fields(part) {
			k, v, eq := strings.Cut(f, "=")
			switch {
			case !eq || !slices.Contains(keys, k):
				names := strings.Join(keys, "=, ") + "="
				if i := strings.LastIndex(names, ", "); i >= 0 {
					names = names[:i] + " or " + names[i+2:]
				}
				return nil, nil, want("%q isn't %s", f, names)
			case v == "":
				return nil, nil, want("%q says nothing", f)
			case got[k] != "":
				return nil, nil, want("%s= twice", k)
			}
			got[k] = v
			order = append(order, f)
		}
		return got, order, nil
	}
	head, order, err := fields(parts[0], "every", "from")
	if err != nil {
		return nil, err
	}
	if head["every"] == "" || head["from"] == "" {
		return nil, want("every= and from= first")
	}
	sh := &Shifts{}
	if sh.Every, sh.Days, err = turnLength(head["every"]); err != nil {
		return nil, want("%s", err)
	}
	layout, v := "2006-01-02T15:04", head["from"]
	if !strings.Contains(v, "T") {
		layout = "2006-01-02"
	}
	w, err := time.Parse(layout, v)
	if err != nil {
		return nil, want("from %q isn't a time like 2026-11-02T09:00", v)
	}
	if w.Year() < firstYear || w.Year() > lastYear {
		return nil, want("from %q isn't from %d to %d", v, firstYear, lastYear)
	}
	sh.wall = [5]int{w.Year(), int(w.Month()), w.Day(), w.Hour(), w.Minute()}
	sh.From = time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), 0, 0, base.Loc)
	texts := []string{strings.Join(order, " ")}
	for _, p := range parts[1:] {
		h := *base
		h.Shifts = nil
		if strings.TrimSpace(p) == "-" {
			sh.Turns = append(sh.Turns, &h)
			texts = append(texts, "-")
			continue
		}
		turn, order, err := fields(p, "work", "sleep", "tz")
		if err != nil {
			return nil, err
		}
		if len(turn) == 0 {
			return nil, want("a turn says nothing; write - for one on the hours as they are")
		}
		if v, ok := turn["tz"]; ok {
			loc, err := time.LoadLocation(v)
			if err != nil || v == "Local" || len(v) > 64 {
				return nil, want("%q isn't a time zone", v)
			}
			h.Loc = loc
		}
		if v, ok := turn["work"]; ok {
			if err := h.ParseWork(v); err != nil {
				return nil, want("%s", err)
			}
		}
		if v, ok := turn["sleep"]; ok {
			if err := h.ParseSleep(v); err != nil {
				return nil, want("%s", err)
			}
		}
		sh.Turns = append(sh.Turns, &h)
		texts = append(texts, strings.Join(order, " "))
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
	// In seconds, which don't run out as a Duration does: exactly, for turns
	// of hours; within a turn, for turns of days, the clocks changing.
	secs := int64(s.Every / time.Second)
	k := int64(math.Floor(float64(t.Unix()-s.From.Unix()) / float64(secs)))
	at := func(k int64) time.Time { return time.Unix(s.From.Unix()+k*secs, 0).In(s.From.Location()) }
	if s.Days != 0 {
		w := s.wall
		at = func(k int64) time.Time {
			return time.Date(w[0], time.Month(w[1]), w[2]+int(k)*s.Days, w[3], w[4], 0, 0, s.From.Location())
		}
	}
	for i := 0; i < 4 && at(k).After(t); i++ {
		k--
	}
	for i := 0; i < 4 && !at(k+1).After(t); i++ {
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
