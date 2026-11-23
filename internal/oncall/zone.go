package oncall

import (
	"fmt"
	"strings"
	"time"
)

// zone is the zone a time is written in: UTC, an IANA zone, or one of the
// feed's own VTIMEZONEs. offset is how far east of UTC its clocks are at an
// instant, in seconds.
type zone interface {
	offset(u time.Time) (int, error)
}

type utcZone struct{}

func (utcZone) offset(time.Time) (int, error) { return 0, nil }

type ianaZone struct{ loc *time.Location }

func (z ianaZone) offset(u time.Time) (int, error) {
	_, off := u.In(z.loc).Zone()
	return off, nil
}

// maxOffset is the furthest from UTC a zone's clocks may be, in seconds:
// none is more than 14 hours.
const maxOffset = 18 * 3600

func secs(n int) time.Duration { return time.Duration(n) * time.Second }

// instant is the instant a wall clock time w (held in UTC) is in z, as RFC
// 5545 (3.3.5) reads a DTSTART: a time the clocks skip when they go
// forward is read by the offset before the gap (02:30 is 03:30 after it);
// one they show twice when they go back is the first. Go's time.Date would
// take the offset after a gap, and east of UTC the second of two.
//
// A repeat's day at a skipped time is read so too, and kept and counted.
// RFC 5545 is of two minds there: 3.3.10 says such an occurrence "MUST be
// ignored and MUST NOT be counted", and, further on, that its time "is
// interpreted in the same manner as an explicit DATE-TIME value", as
// 3.3.5 says. Lapilli departs from the first on purpose: a turn dropped
// on the night the clocks go forward would leave a hole in the rota, the
// pager with no one.
func instant(z zone, w time.Time) (time.Time, error) {
	if _, ok := z.(utcZone); ok {
		return w, nil
	}
	// The instant is within maxOffset of w, either way: the offsets in
	// effect at the two ends say whether the clocks change between.
	a, err := z.offset(w.Add(-secs(maxOffset)))
	if err != nil {
		return time.Time{}, err
	}
	b, err := z.offset(w.Add(secs(maxOffset)))
	if err != nil {
		return time.Time{}, err
	}
	ua, ub := w.Add(-secs(a)), w.Add(-secs(b))
	if a == b {
		return ua, nil
	}
	oa, err := z.offset(ua)
	if err != nil {
		return time.Time{}, err
	}
	ob, err := z.offset(ub)
	if err != nil {
		return time.Time{}, err
	}
	switch {
	case oa == a && ob == b:
		if ub.Before(ua) {
			return ub, nil
		}
		return ua, nil // shown twice: the first
	case oa == a:
		return ua, nil
	case ob == b:
		return ub, nil
	}
	return ua, nil // skipped: by the offset before
}

// wallOf is the wall clock time an instant is in z, held in UTC.
func wallOf(z zone, t time.Time) (time.Time, error) {
	off, err := z.offset(t)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC().Add(secs(off)), nil
}

// later is days on the clock after t, in z, then d: a day of a DURATION,
// or of an event of whole days, is a day on the clock, 23 or 25 hours
// when the clocks change.
func later(z zone, t time.Time, days int, d time.Duration) (time.Time, error) {
	if days == 0 {
		return t.Add(d), nil
	}
	w, err := wallOf(z, t)
	if err != nil {
		return time.Time{}, err
	}
	u, err := instant(z, w.AddDate(0, 0, days))
	return u.Add(d), err
}

// cachedZone is a TZID's zone, or why it has none, looked up once a feed.
type cachedZone struct {
	z   zone
	err error
}

// lookupZone is the zone a TZID names: an IANA name of a place, an Area
// and a City such as Europe/Berlin, which wins over a VTIMEZONE of the
// same name (Google writes both); else the feed's own VTIMEZONE (Outlook
// and Exchange write "Pacific Standard Time" with one); else, without
// one, an IANA name of another kind, or a Windows name by CLDR's table
// (windows.go). A VTIMEZONE wins over the table, and over IANA's names
// that aren't a place's, such as EST, a fixed -0500 all year, or
// Etc/GMT+5: a feed's EST whose VTIMEZONE keeps summer time means that.
func lookupZone(tz string, own map[string]*vzone) (zone, error) {
	name := strings.TrimPrefix(tz, "/")
	if place(name) {
		if l, err := LoadZone(name); err == nil {
			return ianaZone{l}, nil
		}
	}
	if v := own[tz]; v != nil {
		if v.err != nil {
			return nil, fmt.Errorf("the feed's VTIMEZONE %q isn't read: %v", clip(tz), v.err)
		}
		return v, nil
	}
	if l, err := LoadZone(name); err == nil {
		return ianaZone{l}, nil
	}
	if name, ok := windowsZones[tz]; ok {
		if l, err := LoadZone(name); err == nil {
			return ianaZone{l}, nil
		}
	}
	return nil, fmt.Errorf("a zone %q that isn't an IANA name, a Windows one, or one of the feed's VTIMEZONEs", clip(tz))
}

// place reports whether an IANA name is a place's, an Area and a City
// (America/New_York, America/Argentina/Buenos_Aires), not one of those
// kept for a fixed offset or an old rule (EST, MST7MDT, Etc/GMT+5, UTC).
func place(name string) bool {
	return strings.Contains(name, "/") && !strings.HasPrefix(name, "Etc/")
}
