package verdict

import (
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

func TestShiftsParse(t *testing.T) {
	base := DefaultHours(time.UTC)
	for _, s := range []string{
		"every=1w from=2026-11-02T09:00; tz=Asia/Seoul; tz=Europe/Berlin sleep=23:00-07:00",
		"every=8h from=2026-11-02; tz=Asia/Seoul; tz=Europe/Berlin; tz=America/New_York",
		"every=4d from=2026-11-02T07:00; work=mon-sun@07:00-19:00; work=none",
		"every=1w from=2026-11-02T06:00; work=mon-fri@06:00-14:00; work=mon-fri@14:00-22:00 sleep=00:00-08:00; work=mon-fri@22:00-06:00 sleep=08:00-15:00",
		"from=2026-11-02 every=2w; -; tz=Europe/Berlin", // - is a turn on the hours as they are
	} {
		sh, err := ParseShifts(s, base)
		if err != nil {
			t.Errorf("%s: %v", s, err)
			continue
		}
		if got := strings.Join(strings.Fields(sh.String()), " "); got != strings.Join(strings.Fields(s), " ") {
			t.Errorf("%s: written %q", s, sh.String())
		}
	}
	for s, why := range map[string]string{
		"every=1w from=2026-11-02":                                  "no turns",
		"tz=Asia/Seoul; tz=Europe/Berlin":                           "isn't every= or from=",
		"every=1w; tz=Asia/Seoul":                                   "every= and from= first",
		"every=0h from=2026-11-02; tz=UTC":                          "from 1h to 8w",
		"every=9w from=2026-11-02; tz=UTC":                          "from 1h to 8w",
		"every=30m from=2026-11-02; tz=UTC":                         "like 8h",
		"every=1w from=yesterday; tz=UTC":                           "isn't a time",
		"every=1w from=2026-11-02; tz=Mars/Olympus":                 "isn't a time zone",
		"every=1w from=2026-11-02; tz=Local":                        "isn't a time zone",
		"every=1w from=2026-11-02; work=someday@08:00-18:00":        "want days and hours",
		"every=1w from=2026-11-02; sleep=25:00-08:00":               "want hours",
		"every=1w from=2026-11-02; shift=night":                     "isn't work=, sleep= or tz=",
		"every=1w at=2026-11-02; tz=UTC":                            "isn't every= or from=",
		"every=1w from=2026-11-02" + strings.Repeat("; tz=UTC", 25): "more than 24 turns",
		"every=1w from=2026-11-02; tz=UTC;":                         "a turn says nothing",
		"every=1w from=2026-11-02; ; tz=UTC":                        "a turn says nothing",
		"every=1w from=2026-11-02; tz=UTC tz=Asia/Seoul":            "tz= twice",
		"every=1w every=2h from=2026-11-02; tz=UTC":                 "every= twice",
		"every=1w from=2026-11-02; work=":                           "says nothing",
		"every=1w from=2026-11-02; tz":                              "isn't work=, sleep= or tz=",
		"every=8h from=1026-11-02T09:00; tz=UTC":                    "isn't from 1970 to 2200",
		"every=8h from=2201-01-01; tz=UTC":                          "isn't from 1970 to 2200",
	} {
		if _, err := ParseShifts(s, base); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%s: %v, want %q", s, err, why)
		}
	}
}

func TestShiftsOf(t *testing.T) {
	at := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	// On call a week at a time, in Seoul and then in Berlin, the pager
	// changing hands on Mondays at 09:00 UTC.
	h := DefaultHours(time.UTC)
	sh, err := ParseShifts("every=1w from=2026-11-02T09:00; tz=Asia/Seoul; tz=Europe/Berlin", h)
	if err != nil {
		t.Fatal(err)
	}
	h.Shifts = sh
	for when, want := range map[string]string{
		"2026-11-03T02:00:00Z": AtWork,   // Seoul's turn: 11:00 there
		"2026-11-03T15:00:00Z": Asleep,   // 00:00 in Seoul
		"2026-11-10T02:00:00Z": Asleep,   // Berlin's turn: 03:00 there
		"2026-11-10T09:00:00Z": AtWork,   // 10:00 in Berlin
		"2026-11-02T08:59:00Z": AtWork,   // still Berlin's, from before: 09:59 there
		"2026-11-16T09:00:00Z": OffHours, // Seoul's again: 18:00 there
		"2026-10-27T02:00:00Z": Asleep,   // the turn before: Berlin's
		"2026-10-20T02:00:00Z": AtWork,   // and before that, Seoul's
		"2026-11-07T02:00:00Z": OffHours, // a Saturday in Seoul
	} {
		if got := h.Of(at(when)); got != want {
			t.Errorf("%s: %s, want %s", when, got, want)
		}
	}

	// A night shift, from Monday to Friday night: the mornings after are
	// worked; asleep in the day.
	n := DefaultHours(time.UTC)
	if err := n.ParseWork("mon-fri@22:00-06:00"); err != nil {
		t.Fatal(err)
	}
	if err := n.ParseSleep("08:00-15:00"); err != nil {
		t.Fatal(err)
	}
	for when, want := range map[string]string{
		"2026-11-03T03:00:00Z": AtWork,   // Tuesday morning: Monday's night
		"2026-11-07T03:00:00Z": AtWork,   // Saturday morning: Friday's night
		"2026-11-08T03:00:00Z": OffHours, // Sunday morning
		"2026-11-02T03:00:00Z": OffHours, // Monday morning: Sunday wasn't worked
		"2026-11-02T22:30:00Z": AtWork,
		"2026-11-03T10:00:00Z": Asleep,
		"2026-11-03T07:00:00Z": OffHours,
	} {
		if got := n.Of(at(when)); got != want {
			t.Errorf("night %s: %s, want %s", when, got, want)
		}
	}

	// Turns of days change at the same time on the clock, the clocks
	// changing or not: in Berlin they go forward on 29 March.
	berlin, _ := time.LoadLocation("Europe/Berlin")
	b := DefaultHours(berlin)
	sh, err = ParseShifts("every=1d from=2026-03-28T09:00; work=none; work=mon-sun@00:00-24:00", b)
	if err != nil {
		t.Fatal(err)
	}
	b.Shifts = sh
	b.Sleep = Span{}
	for _, tu := range sh.Turns {
		tu.Sleep = Span{}
	}
	for when, want := range map[string]string{
		"2026-03-29T06:30:00Z": OffHours, // 08:30 in Berlin, summer time: the first turn still
		"2026-03-29T07:30:00Z": AtWork,   // 09:30: the second
		"2026-03-30T06:59:00Z": AtWork,   // 08:59
		"2026-03-30T07:00:00Z": OffHours, // 09:00
	} {
		if got := b.Of(at(when)); got != want {
			t.Errorf("berlin %s: %s, want %s", when, got, want)
		}
	}
}

// Spans with shifts say what Of says, minute by minute, and none crosses a
// midnight where the shifts are kept.
func TestShiftsSpans(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	for _, s := range []string{
		"every=8h from=2026-11-02; tz=Asia/Seoul; tz=Europe/Berlin; tz=America/New_York",
		"every=1w from=2026-10-25T09:00; tz=Asia/Seoul; tz=Europe/Berlin sleep=23:00-07:00",
		"every=3d from=2026-10-30T07:00; work=mon-sun@07:00-19:00; work=none; work=mon-fri@22:00-06:00 sleep=08:00-15:00",
	} {
		h := DefaultHours(seoul)
		sh, err := ParseShifts(s, h)
		if err != nil {
			t.Fatal(err)
		}
		h.Shifts = sh
		from := time.Date(2026, 10, 26, 0, 0, 0, 0, seoul)
		to := from.AddDate(0, 0, 14)
		sp := h.Spans(from, to)
		for i, x := range sp {
			if !x.From.Before(x.To) || i > 0 && x.From.Before(sp[i-1].To) || !sameDay(x.From, x.To.Add(-time.Second), seoul) {
				t.Errorf("%s: span %d %v", s, i, x)
			}
		}
		r := rand.New(rand.NewPCG(1, 2))
		for range 2000 {
			tm := from.Add(time.Duration(r.Int64N(int64(to.Sub(from)/time.Minute))) * time.Minute)
			want := h.Of(tm)
			got := AtWork
			for _, x := range sp {
				if !tm.Before(x.From) && tm.Before(x.To) {
					got = x.Of
				}
			}
			if got != want {
				t.Errorf("%s: at %s spans say %s, Of %s", s, tm, got, want)
				break
			}
		}
	}
}

func TestShiftsUsed(t *testing.T) {
	h := DefaultHours(time.UTC)
	sh, err := ParseShifts("every=1w  from=2026-11-02T09:00;tz=Asia/Seoul ; tz=Europe/Berlin", h)
	if err != nil {
		t.Fatal(err)
	}
	h.Shifts = sh
	if u := h.Used(); u.Shifts != "every=1w from=2026-11-02T09:00; tz=Asia/Seoul; tz=Europe/Berlin" || u.Zone != "UTC" {
		t.Errorf("%+v", u)
	}
	if !h.Sleeps() {
		t.Error("asleep in shifts")
	}
}

// Turns far from the first one come round as near ones do, at once; and a
// first turn written at a time the clocks skip keeps that time on other
// days.
func TestShiftsFar(t *testing.T) {
	h := DefaultHours(time.UTC)
	sh, err := ParseShifts("every=1h from=2200-12-31T23:00; tz=Asia/Tokyo; tz=Europe/Berlin; tz=America/New_York", h)
	if err != nil {
		t.Fatal(err)
	}
	for _, when := range []time.Time{time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(1970, 1, 1, 0, 30, 0, 0, time.UTC), time.Date(9999, 12, 31, 23, 0, 0, 0, time.UTC)} {
		began := time.Now()
		i, from, to := sh.turn(when)
		if time.Since(began) > 10*time.Millisecond || from.After(when) || !to.After(when) || to.Sub(from) != time.Hour || i < 0 || i > 2 {
			t.Errorf("%s: turn %d from %s to %s", when, i, from, to)
		}
	}
	berlin, _ := time.LoadLocation("Europe/Berlin")
	sh, err = ParseShifts("every=1d from=2026-03-29T02:30; -; work=none", DefaultHours(berlin))
	if err != nil {
		t.Fatal(err)
	}
	_, from, _ := sh.turn(time.Date(2026, 3, 27, 12, 0, 0, 0, berlin))
	if from.Hour() != 2 || from.Minute() != 30 {
		t.Errorf("turns change at %s", from.Format("15:04"))
	}
}
