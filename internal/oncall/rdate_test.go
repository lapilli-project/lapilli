package oncall

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// RDATE in a feed shaped as Google Calendar writes it (testdata/README.md):
// dates added to a weekly repeat, in its zone, one of them left out by
// EXDATE and one the repeat has already, kept once; days added to an
// all-day event with no RRULE; periods, each with its own end or length;
// and a Windows zone's name, read as DTSTART's is.
func TestFeedRdates(t *testing.T) {
	cfg := config(t, "  - {name: release, file: x}\n")
	f, err := Parse(context.Background(), feed(t, "google-rdate.ics"), cfg.Schedules[0], cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	for at, who := range map[string]string{
		// Mondays, 09:00 to 17:00 in Berlin, CEST then CET.
		"2026-10-05T07:30:00Z": "Jane Doe", "2026-10-12T14:30:00Z": "Jane Doe", "2026-10-12T15:30:00Z": "",
		"2026-11-02T08:30:00Z": "Jane Doe", "2026-11-02T07:30:00Z": "", "2026-11-30T15:30:00Z": "Jane Doe", "2026-12-07T08:30:00Z": "",
		// A Wednesday added, and one added and left out.
		"2026-10-14T07:30:00Z": "Jane Doe", "2026-10-14T15:30:00Z": "", "2026-10-21T10:00:00Z": "",
		// Days, in the schedule's zone (UTC): the 3rd of October, and
		// Christmas, its two days one turn.
		"2026-10-03T12:00:00Z": "Ali Kaya", "2026-12-25T00:30:00Z": "Ali Kaya", "2026-12-26T23:30:00Z": "Ali Kaya", "2026-12-27T00:30:00Z": "",
		// Periods: to 02:00 the next day, and three hours.
		"2026-11-01T21:30:00Z": "Sam Lee", "2026-11-09T01:30:00Z": "Sam Lee", "2026-11-09T02:30:00Z": "",
		"2026-11-15T22:30:00Z": "Sam Lee", "2026-11-15T23:30:00Z": "",
		// W. Europe Standard Time is Berlin's.
		"2026-11-05T07:30:00Z": "Cy", "2026-11-12T14:30:00Z": "Cy", "2026-11-12T15:30:00Z": "",
	} {
		if got := whoAt(f.Turns, at); got != who {
			t.Errorf("at %s: %q, want %q", at, got, who)
		}
	}
	if len(f.Turns) != 10+2+3+2 || len(f.Notes) > 0 || len(f.Held) > 0 {
		t.Errorf("%d turns, notes %q, held %v: %+v", len(f.Turns), f.Notes, f.Held, f.Turns)
	}
}

// RFC 5545's examples of RDATE (3.8.5.2), each as the RFC writes it (the
// days' with the comma its fold drops): a date in UTC and the same
// instant in New York, kept once; periods, by their end and by their
// length; and days.
func TestRdatesRFC5545(t *testing.T) {
	cfg := config(t, "  - {name: s, file: x}\n")
	at := time.Date(1997, 3, 1, 0, 0, 0, 0, time.UTC) // the span read: Jan 1996 to April 1997
	f, err := Parse(context.Background(), []byte("BEGIN:VCALENDAR\r\n"+
		"BEGIN:VEVENT\r\nUID:a\r\nDTSTART:19970301T123000Z\r\nDURATION:PT1H\r\nRDATE:19970414T123000Z\r\nRDATE;TZID=America/New_York:19970414T083000\r\nSUMMARY:Jane Doe\r\nEND:VEVENT\r\n"+
		"BEGIN:VEVENT\r\nUID:b\r\nDTSTART:19960401T020000Z\r\nDTEND:19960401T030000Z\r\nRDATE;VALUE=PERIOD:19960403T020000Z/19960403T040000Z,\r\n 19960404T010000Z/PT3H\r\nSUMMARY:Sam Lee\r\nEND:VEVENT\r\n"+
		"BEGIN:VEVENT\r\nUID:c\r\nDTSTART;VALUE=DATE:19961225\r\nRDATE;VALUE=DATE:19970101,19970120,19970217,19970421,\r\n 19970526,19970704,19970901,19971014,19971128,19971129,19971225\r\nSUMMARY:Ali Kaya\r\nEND:VEVENT\r\n"+
		"END:VCALENDAR\r\n"), cfg.Schedules[0], cfg, at)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range f.Turns {
		got = append(got, x.From.UTC().Format("2006-01-02T15:04")+"/"+x.To.Sub(x.From).String()+" "+x.Who)
	}
	want := []string{
		"1996-04-01T02:00/1h0m0s Sam Lee", "1996-04-03T02:00/2h0m0s Sam Lee", "1996-04-04T01:00/3h0m0s Sam Lee",
		"1996-12-25T00:00/24h0m0s Ali Kaya", "1997-01-01T00:00/24h0m0s Ali Kaya", "1997-01-20T00:00/24h0m0s Ali Kaya", "1997-02-17T00:00/24h0m0s Ali Kaya",
		"1997-03-01T12:30/1h0m0s Jane Doe", "1997-04-14T12:30/1h0m0s Jane Doe", "1997-04-21T00:00/24h0m0s Ali Kaya",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") || len(f.Notes) > 0 {
		t.Errorf("got\n%s\nwant\n%s\nnotes %q", strings.Join(got, "\n"), strings.Join(want, "\n"), f.Notes)
	}
}

// EXDATE leaves out DTSTART of an event with RDATE and no RRULE, as it
// does an RDATE's date; an occurrence an RDATE adds is replaced by
// RECURRENCE-ID as a repeat's is.
func TestRdatesExdateAndRecurrenceID(t *testing.T) {
	cfg := config(t, "  - {name: s, file: x}\n")
	f, err := Parse(context.Background(), []byte("BEGIN:VCALENDAR\r\n"+
		"BEGIN:VEVENT\r\nUID:u\r\nDTSTART:20261105T080000Z\r\nDURATION:PT8H\r\nRDATE:20261112T080000Z,20261119T080000Z,20261126T080000Z\r\n"+
		"EXDATE:20261105T080000Z,20261126T080000Z\r\nSUMMARY:Sam Lee\r\nEND:VEVENT\r\n"+
		"BEGIN:VEVENT\r\nUID:u\r\nRECURRENCE-ID:20261119T080000Z\r\nDTSTART:20261120T080000Z\r\nDURATION:PT8H\r\nSUMMARY:Jane Doe\r\nEND:VEVENT\r\n"+
		"END:VCALENDAR\r\n"), cfg.Schedules[0], cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range f.Turns {
		got = append(got, x.From.UTC().Format("01-02T15")+" "+x.Who)
	}
	if want := "11-12T08 Sam Lee 11-20T08 Jane Doe"; strings.Join(got, " ") != want || len(f.Notes) > 0 {
		t.Errorf("%v, want %s; notes %q", got, want, f.Notes)
	}
}

// What an RDATE can't be is refused, saying why, and its event passed
// over, the turns kept of whom it names held: dates of another kind than
// DTSTART's, a period of days, one that doesn't end after it starts or
// is longer than a year, and values that aren't dates.
func TestRdatesRefused(t *testing.T) {
	cfg := config(t, "  - {name: s, file: x}\n")
	for _, c := range []struct{ start, rdate, says string }{
		{"DTSTART:20261105T080000Z", "RDATE;VALUE=DATE:20261112", "doesn't say when in the day"},
		{"DTSTART:20261105T080000Z", "RDATE:20261112", "doesn't say when in the day"},
		{"DTSTART;VALUE=DATE:20261105", "RDATE:20261112T080000Z", "of times beside a DTSTART of a day"},
		{"DTSTART;VALUE=DATE:20261105", "RDATE;VALUE=PERIOD:20261112T080000Z/PT1H", "of times beside a DTSTART of a day"},
		{"DTSTART:20261105T080000Z", "RDATE;VALUE=PERIOD:20261112/20261113", "starts on a day"},
		{"DTSTART:20261105T080000Z", "RDATE;VALUE=PERIOD:20261112T080000Z/20261113", "ends on a day"},
		{"DTSTART:20261105T080000Z", "RDATE;VALUE=PERIOD:20261112T080000Z/20261112T070000Z", "doesn't end after it starts"},
		{"DTSTART:20261105T080000Z", "RDATE;VALUE=PERIOD:20261112T080000Z/PT0S", "doesn't end after it starts"},
		{"DTSTART:20261105T080000Z", "RDATE;VALUE=PERIOD:20261112T080000Z/20271114T080000Z", "longer than a year"},
		{"DTSTART:20261105T080000Z", "RDATE;VALUE=PERIOD:20261112T080000Z/P400D", "isn't a duration"},
		{"DTSTART:20261105T080000Z", "RDATE;VALUE=PERIOD:20261112T080000Z/-PT1H", "isn't a time"},
		{"DTSTART:20261105T080000Z", "RDATE;VALUE=PERIOD:20261112T080000Z", "isn't a start, a /"},
		{"DTSTART:20261105T080000Z", "RDATE:2026-11-12T08:00:00Z", "isn't a time"},
		{"DTSTART:20261105T080000Z", "RDATE:22261112T080000Z", "a time in 2226"},
		{"DTSTART;TZID=Europe/Berlin:20261105T080000", "RDATE;TZID=Nowhere/Else:20261112T080000", "isn't an IANA name"},
	} {
		f, err := Parse(context.Background(), []byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:r\r\n"+c.start+"\r\nDURATION:PT8H\r\n"+c.rdate+
			"\r\nSUMMARY:Sam Lee\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"), cfg.Schedules[0], cfg, now)
		if err != nil || len(f.Turns) != 0 || len(f.Held) != 1 || !f.Held[0].To.IsZero() || !strings.Contains(strings.Join(f.Notes, " "), c.says) {
			t.Errorf("%s %s: %v, %d turns, held %v, notes %q, want one saying %q", c.start, c.rdate, err, len(f.Turns), f.Held, f.Notes, c.says)
		}
	}
}

// rdateLines is n RDATE dates of value, on lines of 3,000.
func rdateLines(n int, value string) string {
	var b strings.Builder
	for n > 0 {
		k := min(n, 3000)
		b.WriteString("RDATE:" + strings.TrimSuffix(strings.Repeat(value+",", k), ",") + "\r\n")
		n -= k
	}
	return b.String()
}

// Each date an RDATE adds is a step, of the feed's (maxFeedSteps), which
// its repeats and zones take from too, and of its event's (maxSteps, the
// event then passed over); those in the span read are turns, and a feed
// keeps maxTurns. Dates outside the span aren't kept.
func TestRdatesBounded(t *testing.T) {
	cfg := config(t, "  - {name: s, file: x}\n")
	parse := func(body string) (*Feed, error) {
		data := []byte("BEGIN:VCALENDAR\r\n" + body + "END:VCALENDAR\r\n")
		if len(data) > MaxFeed {
			t.Fatalf("%d bytes, over MaxFeed", len(data))
		}
		return Parse(context.Background(), data, cfg.Schedules[0], cfg, now)
	}
	event := func(uid string, rdates string) string {
		return "BEGIN:VEVENT\r\nUID:" + uid + "\r\nDTSTART:20261105T080000Z\r\nDURATION:PT1H\r\n" + rdates + "SUMMARY:Sam Lee\r\nEND:VEVENT\r\n"
	}
	// A thousand dates long before the span: a step each, none kept.
	f, err := parse(event("a", rdateLines(1000, "19800101T000000Z")))
	if err != nil || f.steps != 1000 || len(f.Turns) != 1 {
		t.Errorf("a thousand dates: %v, %d steps, %d turns", err, f.steps, len(f.Turns))
	}
	// Past maxSteps in one event: it is passed over, the rest read.
	f, err = parse(event("b", rdateLines(maxSteps+1, "19800101T000000Z")) + event("c", ""))
	if err != nil || len(f.Turns) != 1 || len(f.Held) != 1 || !strings.Contains(strings.Join(f.Notes, " "), fmt.Sprintf("more than %d dates", maxSteps)) {
		t.Errorf("past maxSteps: %v, %d turns, held %v, notes %q", err, len(f.Turns), f.Held, f.Notes)
	}
	// Past maxTurns in the span: the feed is refused, as a repeat's are.
	if _, err := parse(event("d", rdateLines(maxTurns+1, "20261112T080000Z"))); err == nil || !strings.Contains(err.Error(), "turns") {
		t.Errorf("past maxTurns: %v", err)
	}
	// The feed's steps, shared: repeats that take most of them read alone,
	// and with an RDATE's dates beside them pass them.
	const rule = "FREQ=YEARLY;BYDAY=SU,MO,TU,WE,TH,FR,SA;BYSETPOS=-366"
	repeat := func(uid int) string {
		return fmt.Sprintf("BEGIN:VEVENT\r\nUID:p%d\r\nDTSTART:20200101T000000Z\r\nDURATION:PT1H\r\nRRULE:%s\r\nSUMMARY:Sam Lee\r\nEND:VEVENT\r\n", uid, rule)
	}
	f, err = parse(repeat(0))
	if err != nil || f.steps == 0 {
		t.Fatalf("one repeat: %v, %+v", err, f)
	}
	var repeats strings.Builder
	n := (maxFeedSteps - 100_000) / f.steps
	for i := range n {
		repeats.WriteString(repeat(i))
	}
	if f, err := parse(repeats.String()); err != nil || f.steps > maxFeedSteps-100_000 {
		t.Fatalf("%d repeats alone: %v", n, err)
	}
	if _, err := parse(repeats.String() + event("e", rdateLines(150_000, "19800101T000000Z"))); err == nil || !strings.Contains(err.Error(), "steps") {
		t.Errorf("%d repeats and 150,000 dates: %v, want the feed's steps passed", n, err)
	}
}
