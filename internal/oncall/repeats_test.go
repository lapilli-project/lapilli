package oncall

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// occurs is what a repeat in New York gives, as RFC 5545's examples are
// written: each start's day, at most n of them, each checked to be at 09:00
// on New York's clock, EDT or EST. Every occurrence from the start on is
// kept, over twelve years.
func occurs(t *testing.T, dtstart, rrule string, n int, exdates ...string) []string {
	t.Helper()
	ny, err := LoadZone("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	resolve := func(string) (zone, error) { return ianaZone{ny}, nil }
	tz := map[string]string{"TZID": "America/New_York"}
	st, err := parseStamp(tz, dtstart, utcZone{}, resolve)
	if err != nil {
		t.Fatal(err)
	}
	e := &event{start: st, rrule: rrule}
	for _, x := range exdates {
		s, err := parseStamp(tz, x, utcZone{}, resolve)
		if err != nil {
			t.Fatal(err)
		}
		e.exdates = append(e.exdates, exdate{t: s.t})
	}
	steps := maxFeedSteps
	got, err := expand(context.Background(), e, st.t.Add(Ahead), st.t.AddDate(12, 0, 0), maxTurns, &steps)
	if err != nil {
		t.Fatalf("%s: %v", rrule, err)
	}
	var out []string
	for _, x := range got[:min(n, len(got))] {
		l := x.In(ny)
		if l.Hour() != 9 || l.Minute() != 0 {
			t.Errorf("%s: %s isn't 09:00 in New York", rrule, l)
		}
		out = append(out, l.Format("2006-01-02"))
	}
	return out
}

// days writes "1997-09-05" as year, then month-day pairs: days("1997",
// "09-05", "10-03", "1998", "01-02").
func days(parts ...string) []string {
	var out []string
	year := ""
	for _, p := range parts {
		if len(p) == 4 {
			year = p
			continue
		}
		out = append(out, year+"-"+p)
	}
	return out
}

// RFC 5545's examples of monthly and yearly repeats (3.8.5.3), each as the
// RFC gives it, and some of its daily and weekly ones, as one engine now
// expands all four: the days, and 09:00 on New York's clock across its
// changes. An invalid date (30 February) is skipped, not rolled over.
func TestRepeatsRFC5545(t *testing.T) {
	for _, c := range []struct {
		name, start, rule string
		n                 int
		exdates           []string
		want              []string
	}{
		{"monthly on the first Friday for 10 occurrences", "19970905T090000", "FREQ=MONTHLY;COUNT=10;BYDAY=1FR", 99, nil,
			days("1997", "09-05", "10-03", "11-07", "12-05", "1998", "01-02", "02-06", "03-06", "04-03", "05-01", "06-05")},
		{"monthly on the first Friday until December 24, 1997", "19970905T090000", "FREQ=MONTHLY;UNTIL=19971224T000000Z;BYDAY=1FR", 99, nil,
			days("1997", "09-05", "10-03", "11-07", "12-05")},
		{"every other month on the first and last Sunday, 10 times", "19970907T090000", "FREQ=MONTHLY;INTERVAL=2;COUNT=10;BYDAY=1SU,-1SU", 99, nil,
			days("1997", "09-07", "09-28", "11-02", "11-30", "1998", "01-04", "01-25", "03-01", "03-29", "05-03", "05-31")},
		{"monthly on the second-to-last Monday for 6 months", "19970922T090000", "FREQ=MONTHLY;COUNT=6;BYDAY=-2MO", 99, nil,
			days("1997", "09-22", "10-20", "11-17", "12-22", "1998", "01-19", "02-16")},
		{"monthly on the third-to-the-last day, forever", "19970928T090000", "FREQ=MONTHLY;BYMONTHDAY=-3", 6, nil,
			days("1997", "09-28", "10-29", "11-28", "12-29", "1998", "01-29", "02-26")},
		{"monthly on the 2nd and 15th, 10 times", "19970902T090000", "FREQ=MONTHLY;COUNT=10;BYMONTHDAY=2,15", 99, nil,
			days("1997", "09-02", "09-15", "10-02", "10-15", "11-02", "11-15", "12-02", "12-15", "1998", "01-02", "01-15")},
		{"monthly on the first and last day, 10 times", "19970930T090000", "FREQ=MONTHLY;COUNT=10;BYMONTHDAY=1,-1", 99, nil,
			days("1997", "09-30", "10-01", "10-31", "11-01", "11-30", "12-01", "12-31", "1998", "01-01", "01-31", "02-01")},
		{"every 18 months on the 10th to the 15th, 10 times", "19970910T090000", "FREQ=MONTHLY;INTERVAL=18;COUNT=10;BYMONTHDAY=10,11,12,13,14,15", 99, nil,
			days("1997", "09-10", "09-11", "09-12", "09-13", "09-14", "09-15", "1999", "03-10", "03-11", "03-12", "03-13")},
		{"every Tuesday, every other month", "19970902T090000", "FREQ=MONTHLY;INTERVAL=2;BYDAY=TU", 18, nil,
			days("1997", "09-02", "09-09", "09-16", "09-23", "09-30", "11-04", "11-11", "11-18", "11-25",
				"1998", "01-06", "01-13", "01-20", "01-27", "03-03", "03-10", "03-17", "03-24", "03-31")},
		{"yearly in June and July, 10 times", "19970610T090000", "FREQ=YEARLY;COUNT=10;BYMONTH=6,7", 99, nil,
			days("1997", "06-10", "07-10", "1998", "06-10", "07-10", "1999", "06-10", "07-10", "2000", "06-10", "07-10", "2001", "06-10", "07-10")},
		{"every other year in January, February and March, 10 times", "19970310T090000", "FREQ=YEARLY;INTERVAL=2;COUNT=10;BYMONTH=1,2,3", 99, nil,
			days("1997", "03-10", "1999", "01-10", "02-10", "03-10", "2001", "01-10", "02-10", "03-10", "2003", "01-10", "02-10", "03-10")},
		{"every 20th Monday of the year", "19970519T090000", "FREQ=YEARLY;BYDAY=20MO", 3, nil,
			days("1997", "05-19", "1998", "05-18", "1999", "05-17")},
		{"every Thursday in March", "19970313T090000", "FREQ=YEARLY;BYMONTH=3;BYDAY=TH", 11, nil,
			days("1997", "03-13", "03-20", "03-27", "1998", "03-05", "03-12", "03-19", "03-26", "1999", "03-04", "03-11", "03-18", "03-25")},
		{"every Thursday in June, July and August", "19970605T090000", "FREQ=YEARLY;BYDAY=TH;BYMONTH=6,7,8", 14, nil,
			days("1997", "06-05", "06-12", "06-19", "06-26", "07-03", "07-10", "07-17", "07-24", "07-31", "08-07", "08-14", "08-21", "08-28", "1998", "06-04")},
		{"every Friday the 13th", "19970902T090000", "FREQ=MONTHLY;BYDAY=FR;BYMONTHDAY=13", 5, []string{"19970902T090000"},
			days("1998", "02-13", "03-13", "11-13", "1999", "08-13", "2000", "10-13")},
		{"the first Saturday after the first Sunday of the month", "19970913T090000", "FREQ=MONTHLY;BYDAY=SA;BYMONTHDAY=7,8,9,10,11,12,13", 10, nil,
			days("1997", "09-13", "10-11", "11-08", "12-13", "1998", "01-10", "02-07", "03-07", "04-11", "05-09", "06-13")},
		{"every 4 years, the first Tuesday after a Monday in November", "19961105T090000", "FREQ=YEARLY;INTERVAL=4;BYMONTH=11;BYDAY=TU;BYMONTHDAY=2,3,4,5,6,7,8", 3, nil,
			days("1996", "11-05", "2000", "11-07", "2004", "11-02")},
		{"the third of Tuesday, Wednesday or Thursday, for 3 months", "19970904T090000", "FREQ=MONTHLY;COUNT=3;BYDAY=TU,WE,TH;BYSETPOS=3", 99, nil,
			days("1997", "09-04", "10-07", "11-06")},
		{"the second-to-last weekday of the month", "19970929T090000", "FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-2", 7, nil,
			days("1997", "09-29", "10-30", "11-27", "12-30", "1998", "01-29", "02-26", "03-30")},
		{"30 February ignored", "20070115T090000", "FREQ=MONTHLY;BYMONTHDAY=15,30;COUNT=5", 99, nil,
			days("2007", "01-15", "01-30", "02-15", "03-15", "03-30")},
		// Daily and weekly, through the same periods.
		{"weekly on Tuesday and Thursday for five weeks", "19970902T090000", "FREQ=WEEKLY;UNTIL=19971007T000000Z;WKST=SU;BYDAY=TU,TH", 99, nil,
			days("1997", "09-02", "09-04", "09-09", "09-11", "09-16", "09-18", "09-23", "09-25", "09-30", "10-02")},
		{"every other week on Monday, Wednesday and Friday until December 24", "19970901T090000", "FREQ=WEEKLY;INTERVAL=2;UNTIL=19971224T000000Z;WKST=SU;BYDAY=MO,WE,FR", 99, nil,
			days("1997", "09-01", "09-03", "09-05", "09-15", "09-17", "09-19", "09-29", "10-01", "10-03", "10-13", "10-15", "10-17",
				"10-27", "10-29", "10-31", "11-10", "11-12", "11-14", "11-24", "11-26", "11-28", "12-08", "12-10", "12-12", "12-22")},
		{"every 10 days, 5 times", "19970902T090000", "FREQ=DAILY;INTERVAL=10;COUNT=5", 99, nil,
			days("1997", "09-02", "09-12", "09-22", "10-02", "10-12")},
	} {
		got := occurs(t, c.start, c.rule, c.n, c.exdates...)
		if strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("%s (%s):\n got %v\nwant %v", c.name, c.rule, got, c.want)
		}
	}
	// Every day in January, for 3 years: yearly by month and day, and daily
	// limited by month, alike.
	for _, rule := range []string{"FREQ=YEARLY;UNTIL=20000131T140000Z;BYMONTH=1;BYDAY=SU,MO,TU,WE,TH,FR,SA", "FREQ=DAILY;UNTIL=20000131T140000Z;BYMONTH=1"} {
		got := occurs(t, "19980101T090000", rule, 1000)
		if len(got) != 93 || got[0] != "1998-01-01" || got[30] != "1998-01-31" || got[31] != "1999-01-01" || got[92] != "2000-01-31" {
			t.Errorf("%s: %d days, %v", rule, len(got), got)
		}
	}
}

// Monthly and yearly turns in feeds shaped as each tool writes them: an
// occurrence left out (EXDATE) and one replaced (RECURRENCE-ID) in a
// monthly repeat; "the last weekday of the month" (BYSETPOS=-1); yearly
// all-day turns; the clocks changing in Berlin under a monthly 09:00 turn.
func TestFeedsMonthlyYearly(t *testing.T) {
	for _, c := range []struct {
		file, schedule string
		want           map[string]string
		turns          int
	}{
		{"google-monthly.ics", "  - {name: release, file: x}\n", map[string]string{
			// The first Monday, 09:00 in Berlin: CET in winter, CEST in summer.
			"2026-03-02T08:30:00Z": "Jane Doe", "2026-03-02T07:30:00Z": "",
			"2026-04-06T07:30:00Z": "Jane Doe", "2026-04-06T06:30:00Z": "",
			"2026-07-06T12:00:00Z": "",                                    // left out
			"2026-08-03T12:00:00Z": "", "2026-08-04T12:00:00Z": "Sam Lee", // moved a day, to Sam
			"2026-11-02T08:30:00Z": "Jane Doe", "2026-12-07T08:30:00Z": "Jane Doe",
			// Christmas Eve, a day in UTC (the schedule's zone), each year.
			"2025-12-24T12:00:00Z": "Ali Kaya", "2026-12-24T12:00:00Z": "Ali Kaya", "2026-12-25T12:00:00Z": "",
			// The last weekday of each month, 18:00 in Berlin.
			"2026-03-31T16:30:00Z": "Cy", "2026-05-29T16:30:00Z": "Cy", "2026-10-30T17:30:00Z": "Cy", "2026-10-30T16:30:00Z": "",
		}, 10 + 1 + 2 + 10},
		{"pagerduty-monthly.ics", "  - {name: payments, file: x}\n", map[string]string{
			"2025-10-15T12:00:00Z": "Sam Lee", "2026-02-15T12:00:00Z": "Sam Lee", "2026-12-15T12:00:00Z": "Sam Lee", "2026-12-16T12:00:00Z": "",
			"2026-01-01T12:00:00Z": "Ali Kaya", "2027-01-01T12:00:00Z": "Ali Kaya", // COUNT=3: 2025, 2026 and 2027
		}, 15 + 2},
		{"opsgenie-yearly.ics", "  - {name: platform, file: x, who: '^(.+?) \\(user\\) is on call'}\n", map[string]string{
			// The last day of the month, 4 times: 30 September, 31 October,
			// 30 November and 31 December.
			"2026-09-30T12:00:00Z": "Jane Doe", "2026-10-31T12:00:00Z": "Jane Doe", "2026-11-30T12:00:00Z": "Jane Doe",
			"2026-12-31T12:00:00Z": "Jane Doe", "2026-10-30T12:00:00Z": "",
			"2025-12-25T12:00:00Z": "Ali Kaya", "2026-12-25T12:00:00Z": "Ali Kaya",
		}, 4 + 2},
	} {
		t.Run(c.file, func(t *testing.T) {
			cfg := config(t, c.schedule)
			f, err := Parse(context.Background(), feed(t, c.file), cfg.Schedules[0], cfg, now)
			if err != nil {
				t.Fatal(err)
			}
			for at, who := range c.want {
				if got := whoAt(f.Turns, at); got != who {
					t.Errorf("at %s: %q, want %q", at, got, who)
				}
			}
			if len(f.Turns) != c.turns || len(f.Notes) > 0 {
				t.Errorf("%d turns, notes %q: %+v", len(f.Turns), f.Notes, f.Turns)
			}
		})
	}
}

// What a repeat can't be, refused, the event passed over and said: what
// RFC 5545 doesn't allow, values past its bounds, and what this doesn't
// expand.
func TestRepeatsRefused(t *testing.T) {
	cfg := config(t, "  - {name: s, file: x}\n")
	for rule, says := range map[string]string{
		"FREQ=MONTHLY;BYDAY=0MO":              "by day",
		"FREQ=MONTHLY;BYDAY=54MO":             "by day",
		"FREQ=MONTHLY;BYDAY=+MO":              "by day",
		"FREQ=MONTHLY;BYDAY=1XX":              "by day",
		"FREQ=MONTHLY;BYMONTHDAY=0":           "day of the month",
		"FREQ=MONTHLY;BYMONTHDAY=32":          "day of the month",
		"FREQ=MONTHLY;BYMONTHDAY=-32":         "day of the month",
		"FREQ=YEARLY;BYMONTH=13":              "by month",
		"FREQ=YEARLY;BYMONTH=0":               "by month",
		"FREQ=MONTHLY;BYDAY=MO;BYSETPOS=0":    "BYSETPOS",
		"FREQ=MONTHLY;BYDAY=MO;BYSETPOS=367":  "BYSETPOS",
		"FREQ=MONTHLY;BYSETPOS=1":             "BYSETPOS",
		"FREQ=WEEKLY;BYDAY=1MO":               "numbered day",
		"FREQ=DAILY;BYDAY=-1FR":               "numbered day",
		"FREQ=WEEKLY;BYMONTHDAY=1":            "day of the month",
		"FREQ=YEARLY;BYWEEKNO=20;BYDAY=MO":    "BYWEEKNO",
		"FREQ=YEARLY;BYYEARDAY=1,100":         "BYYEARDAY",
		"FREQ=DAILY;BYHOUR=9,17":              "BYHOUR",
		"FREQ=HOURLY;INTERVAL=3":              "FREQ=HOURLY",
		"FREQ=MONTHLY;INTERVAL=1001":          "INTERVAL",
		"FREQ=MONTHLY;INTERVAL=0":             "INTERVAL",
		"FREQ=YEARLY;COUNT=0":                 "COUNT",
		"BYDAY=MO":                            "FREQ=",
		"FREQ=MONTHLY;BYDAY=1MO;RSCALE=CHINA": "RSCALE",
	} {
		f, err := Parse(context.Background(), []byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nDTSTART:20260105T090000Z\r\nDURATION:PT8H\r\nRRULE:"+rule+
			"\r\nSUMMARY:Sam Lee\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"), cfg.Schedules[0], cfg, now)
		if err != nil || len(f.Turns) != 0 || len(f.Held) != 1 || !strings.Contains(strings.Join(f.Notes, " "), says) {
			t.Errorf("%s: %v, %d turns, held %v, notes %q, want one saying %q", rule, err, len(f.Turns), f.Held, f.Notes, says)
		}
	}
}

// What a feed's editor could write to make a repeat cost much, bounded by
// steps, not by time: a repeat that never occurs (31 February) stops when
// its periods pass the span read, with COUNT too; a yearly repeat of every
// day there is costs a step a day looked at; the largest INTERVAL is
// refused or costs a step; a list written over and over is read once.
func TestRepeatsHostile(t *testing.T) {
	cfg := config(t, "  - {name: s, file: x}\n")
	parse := func(body string) (*Feed, error) {
		return Parse(context.Background(), []byte("BEGIN:VCALENDAR\r\n"+body+"END:VCALENDAR\r\n"), cfg.Schedules[0], cfg, now)
	}
	event := func(start, rule string) string {
		return "BEGIN:VEVENT\r\nDTSTART:" + start + "\r\nDURATION:PT1H\r\nRRULE:" + rule + "\r\nSUMMARY:Sam Lee\r\nEND:VEVENT\r\n"
	}
	for _, c := range []struct {
		start, rule string
		turns, most int // turns made, and steps taken at most
	}{
		{"19700101T000000Z", "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=31", 0, 10},
		{"19700101T000000Z", "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=30,31;COUNT=10", 0, 2 * 60},
		{"19700101T000000Z", "FREQ=MONTHLY;BYMONTH=2;BYMONTHDAY=31", 0, 2 * 20},
		{"19700101T000000Z", "FREQ=MONTHLY;BYMONTHDAY=31;BYMONTH=2;COUNT=200000", 0, 2 * 12 * 60},
		{"19700105T000000Z", "FREQ=MONTHLY;BYDAY=5MO;BYMONTH=2", 0, 2 * 20}, // a fifth Monday of February: 2027 has none
		{"20260101T000000Z", "FREQ=YEARLY;INTERVAL=1000", 1, 10},
		{"19700101T000000Z", "FREQ=MONTHLY;INTERVAL=1000", 0, 10},
		// Every day of every year, by month and day of the month and by day:
		// a step for each day looked at.
		{"20250101T000000Z", "FREQ=YEARLY;BYMONTH=1,2,3,4,5,6,7,8,9,10,11,12;BYMONTHDAY=" + strings.TrimSuffix(strings.Repeat("1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31,", 50), ","), 460, 3 * (12 + 366)},
		{"20250101T000000Z", "FREQ=YEARLY;BYDAY=" + strings.TrimSuffix(strings.Repeat("SU,MO,TU,WE,TH,FR,SA,1MO,-1MO,53SU,-53SA,", 500), ","), 460, 3 * (12 + 366)},
		// Counted from 1970: each year to the span read is stepped through,
		// and no more.
		{"19700101T000000Z", "FREQ=YEARLY;BYDAY=SU,MO,TU,WE,TH,FR,SA;COUNT=200000", 460, 58 * (12 + 366)},
	} {
		f, err := parse(event(c.start, c.rule))
		if err != nil {
			t.Errorf("%.60s: %v", c.rule, err)
			continue
		}
		if len(f.Turns) != c.turns || f.steps > c.most || len(f.Notes) > 0 {
			t.Errorf("%.60s: %d turns, %d steps (at most %d), notes %q", c.rule, len(f.Turns), f.steps, c.most, f.Notes)
		}
	}
	// 2,000 repeats counted from 1970: more steps than a feed may take, the feed refused.
	var many strings.Builder
	for i := range 2000 {
		fmt.Fprintf(&many, "BEGIN:VEVENT\r\nUID:y%d\r\nDTSTART:19700101T000000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=MONTHLY;BYMONTHDAY=1,15;COUNT=1300\r\nSUMMARY:Sam Lee\r\nEND:VEVENT\r\n", i)
	}
	if _, err := parse(many.String()); err == nil || !strings.Contains(err.Error(), "steps") {
		t.Errorf("2,000 monthly repeats counted from 1970: %v", err)
	}
	// What a monthly repeat makes counts against the turns a feed may make.
	var lots strings.Builder
	for i := range 110 {
		fmt.Fprintf(&lots, "BEGIN:VEVENT\r\nUID:m%d\r\nDTSTART:20250101T%02d%02d00Z\r\nDURATION:PT1M\r\nRRULE:FREQ=MONTHLY;BYMONTHDAY=%s\r\nSUMMARY:Sam Lee\r\nEND:VEVENT\r\n",
			i, i/60, i%60, "1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31")
	}
	if _, err := parse(lots.String()); err == nil || !strings.Contains(err.Error(), "turns") {
		t.Errorf("past the turns a feed may make, by monthly repeats: %v", err)
	}
}

// A repeat steps on the clock it is written on: a weekly 09:00 turn in an
// IANA zone stays at 09:00 when the clocks change, and a turn written at
// a time the clocks skip or show twice is read as RFC 5545 reads it (the
// offset before a gap; the first of two).
func TestRepeatsOnTheClock(t *testing.T) {
	cfg := config(t, "  - {name: s, file: x}\n")
	f, err := Parse(context.Background(), []byte("BEGIN:VCALENDAR\r\n"+
		"BEGIN:VEVENT\r\nDTSTART;TZID=America/Los_Angeles:20260202T090000\r\nDURATION:PT1H\r\nRRULE:FREQ=MONTHLY;BYDAY=1MO\r\nSUMMARY:Sam Lee\r\nEND:VEVENT\r\n"+
		"BEGIN:VEVENT\r\nDTSTART;TZID=America/Los_Angeles:20260308T023000\r\nDURATION:PT10M\r\nSUMMARY:gap\r\nEND:VEVENT\r\n"+
		"BEGIN:VEVENT\r\nDTSTART;TZID=America/Los_Angeles:20261101T013000\r\nDURATION:PT10M\r\nSUMMARY:twice\r\nEND:VEVENT\r\n"+
		"BEGIN:VEVENT\r\nDTSTART;TZID=Europe/Berlin:20261025T023000\r\nDURATION:PT10M\r\nSUMMARY:twice east\r\nEND:VEVENT\r\n"+
		"END:VCALENDAR\r\n"), cfg.Schedules[0], cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	for at, who := range map[string]string{
		"2026-03-02T17:30:00Z": "Sam Lee", "2026-04-06T16:30:00Z": "Sam Lee", "2026-11-02T17:30:00Z": "Sam Lee", "2026-11-02T16:30:00Z": "",
		"2026-03-08T10:35:00Z": "gap",        // 02:30 PST is skipped: 03:30 PDT
		"2026-11-01T08:35:00Z": "twice",      // 01:30 PDT, the first
		"2026-10-25T00:35:00Z": "twice east", // 02:30 CEST, the first
	} {
		if got := whoAt(f.Turns, at); got != who {
			t.Errorf("at %s: %q, want %q", at, got, who)
		}
	}
}
