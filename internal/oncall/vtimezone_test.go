package oncall

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"lapilli.dev/internal/verdict"
)

// pacific and wEurope are the VTIMEZONEs Outlook and Exchange write for
// their zones: from 1601, a yearly rule each.
const pacific = "BEGIN:VTIMEZONE\r\nTZID:Pacific Standard Time\r\n" +
	"BEGIN:STANDARD\r\nDTSTART:16010101T020000\r\nTZOFFSETFROM:-0700\r\nTZOFFSETTO:-0800\r\nRRULE:FREQ=YEARLY;INTERVAL=1;BYDAY=1SU;BYMONTH=11\r\nEND:STANDARD\r\n" +
	"BEGIN:DAYLIGHT\r\nDTSTART:16010101T020000\r\nTZOFFSETFROM:-0800\r\nTZOFFSETTO:-0700\r\nRRULE:FREQ=YEARLY;INTERVAL=1;BYDAY=2SU;BYMONTH=3\r\nEND:DAYLIGHT\r\n" +
	"END:VTIMEZONE\r\n"

const wEurope = "BEGIN:VTIMEZONE\r\nTZID:W. Europe Standard Time\r\n" +
	"BEGIN:STANDARD\r\nDTSTART:16010101T030000\r\nTZOFFSETFROM:+0200\r\nTZOFFSETTO:+0100\r\nRRULE:FREQ=YEARLY;INTERVAL=1;BYDAY=-1SU;BYMONTH=10\r\nEND:STANDARD\r\n" +
	"BEGIN:DAYLIGHT\r\nDTSTART:16010101T020000\r\nTZOFFSETFROM:+0100\r\nTZOFFSETTO:+0200\r\nRRULE:FREQ=YEARLY;INTERVAL=1;BYDAY=-1SU;BYMONTH=3\r\nEND:DAYLIGHT\r\n" +
	"END:VTIMEZONE\r\n"

func parseFeed(t *testing.T, body string) *Feed {
	t.Helper()
	cfg := config(t, "  - {name: s, file: x}\n")
	f, err := Parse(context.Background(), []byte("BEGIN:VCALENDAR\r\n"+body+"END:VCALENDAR\r\n"), cfg.Schedules[0], cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// single is an event of one turn, from start for d, in the zone tz names.
func single(summary, tz, start, d string) string {
	return "BEGIN:VEVENT\r\nDTSTART;TZID=" + tz + ":" + start + "\r\nDURATION:" + d + "\r\nSUMMARY:" + summary + "\r\nEND:VEVENT\r\n"
}

// An Outlook or Exchange feed, its zones Windows names each given as a
// VTIMEZONE from 1601: read as the same feed written with IANA names is.
// A weekly 09:00 turn in Pacific Standard Time is 09:00 on the clock all
// year (17:00Z in winter, 16:00Z in summer), a monthly one in W. Europe
// Standard Time too, and a turn at 01:30 on the night the clocks go back
// is the first 01:30. The same feed with no VTIMEZONE is read by CLDR's
// table, alike.
func TestOutlookVTimezone(t *testing.T) {
	cfg := config(t, "  - {name: payments, file: x}\n")
	s := cfg.Schedules[0]
	read := func(b []byte) *Feed {
		t.Helper()
		f, err := Parse(context.Background(), b, s, cfg, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(f.Notes) > 0 {
			t.Errorf("notes: %q", f.Notes)
		}
		return f
	}
	own := read(feed(t, "outlook.ics"))
	for at, who := range map[string]string{
		"2026-03-02T17:30:00Z": "Jane Doe", "2026-03-02T16:30:00Z": "", // PST
		"2026-03-09T16:30:00Z": "Jane Doe", "2026-03-09T15:30:00Z": "", // PDT, the day after the clocks went forward
		"2026-04-06T18:00:00Z": "", // left out
		"2026-10-26T16:30:00Z": "Jane Doe", "2026-11-02T16:30:00Z": "", "2026-11-02T17:30:00Z": "Jane Doe",
		"2026-12-21T17:30:00Z": "Jane Doe", "2026-12-28T17:30:00Z": "", // UNTIL 21 December, 17:00Z
		"2026-03-27T17:30:00Z": "Ali Kaya", "2026-03-27T16:30:00Z": "", // the last Friday, 18:00 CET
		"2026-09-25T16:30:00Z": "Ali Kaya", "2026-09-25T15:30:00Z": "", // 18:00 CEST
		"2026-10-30T17:30:00Z": "Ali Kaya", "2026-10-30T16:30:00Z": "",
		// Sam over Ali's weekend, 01:30 PDT to 03:00 PST.
		"2026-11-01T08:15:00Z": "Ali Kaya", "2026-11-01T08:45:00Z": "Sam Lee", "2026-11-01T10:45:00Z": "Sam Lee", "2026-11-01T11:15:00Z": "Ali Kaya",
	} {
		if got := whoAt(own.Turns, at); got != who {
			t.Errorf("at %s: %q, want %q", at, got, who)
		}
	}
	if len(own.Turns) != 46+12+1+1 { // Ali's weekend in two
		t.Errorf("%d turns", len(own.Turns))
	}
	// The same, by IANA names, and by the Windows names alone.
	plain := feed(t, "outlook-no-vtimezone.ics")
	iana := strings.NewReplacer("TZID=Pacific Standard Time", "TZID=America/Los_Angeles", "TZID=W. Europe Standard Time", "TZID=Europe/Berlin").Replace(string(plain))
	for name, b := range map[string][]byte{"IANA names": []byte(iana), "no VTIMEZONE": plain} {
		if got := read(b).Turns; !slices.EqualFunc(got, own.Turns, func(a, b verdict.RotaTurn) bool { return a.From.Equal(b.From) && a.To.Equal(b.To) && a.Who == b.Who }) {
			t.Errorf("%s: %d turns, not the VTIMEZONEs' %d:\n%+v", name, len(got), len(own.Turns), got)
		}
	}
}

// A time the clocks skip, or show twice, in a feed's own zone is read as
// RFC 5545 (3.3.5) reads one, and as the IANA zone is: 02:30 on the night
// the clocks go forward by the offset before (03:30 after); 02:30 or 01:30
// on the night they go back, the first.
func TestVTimezoneGapAndOverlap(t *testing.T) {
	for _, c := range []struct{ tz, iana, at, want string }{
		{"Pacific Standard Time", "America/Los_Angeles", "20260308T023000", "2026-03-08T10:35:00Z"},
		{"Pacific Standard Time", "America/Los_Angeles", "20261101T013000", "2026-11-01T08:35:00Z"},
		{"W. Europe Standard Time", "Europe/Berlin", "20260329T023000", "2026-03-29T01:35:00Z"},
		{"W. Europe Standard Time", "Europe/Berlin", "20261025T023000", "2026-10-25T00:35:00Z"},
	} {
		for _, tz := range []string{c.tz, c.iana} {
			f := parseFeed(t, pacific+wEurope+single("Sam Lee", tz, c.at, "PT10M"))
			if whoAt(f.Turns, c.want) != "Sam Lee" || len(f.Turns) != 1 {
				t.Errorf("%s %s: %+v, want a turn at %s", tz, c.at, f.Turns, c.want)
			}
		}
	}
}

// A VTIMEZONE may come after the events that name it.
func TestVTimezoneAfterEvents(t *testing.T) {
	f := parseFeed(t, single("Sam Lee", "Pacific Standard Time", "20260706T090000", "PT1H")+pacific)
	if len(f.Turns) != 1 || whoAt(f.Turns, "2026-07-06T16:30:00Z") != "Sam Lee" {
		t.Errorf("%+v %q", f.Turns, f.Notes)
	}
}

// Which zone a TZID is: an IANA name, though the feed gives a VTIMEZONE
// of its own for it (Google writes both); else the feed's VTIMEZONE,
// though the name is a Windows one too; else the Windows name by CLDR's
// table. A VTIMEZONE of STANDARD alone is an offset that doesn't change.
// Outlook's older names, which hold commas, are matched as written. A zone
// none of these is, is said.
func TestZoneLookup(t *testing.T) {
	wrongNewYork := "BEGIN:VTIMEZONE\r\nTZID:America/New_York\r\nBEGIN:STANDARD\r\nDTSTART:19700101T000000\r\nTZOFFSETFROM:+0500\r\nTZOFFSETTO:+0500\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\n"
	ninePacific := "BEGIN:VTIMEZONE\r\nTZID:Pacific Standard Time\r\nBEGIN:STANDARD\r\nDTSTART:16010101T000000\r\nTZOFFSETFROM:+0900\r\nTZOFFSETTO:+0900\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\n"
	korea := "BEGIN:VTIMEZONE\r\nTZID:Korea Standard Time\r\nBEGIN:STANDARD\r\nDTSTART:16010101T000000\r\nTZOFFSETFROM:+0900\r\nTZOFFSETTO:+0900\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\n"
	older := "BEGIN:VTIMEZONE\r\nTZID:(UTC+01:00) Amsterdam\\, Berlin\\, Bern\\, Rome\\, Stockholm\\, Vienna\r\n" +
		"BEGIN:STANDARD\r\nDTSTART:16010101T030000\r\nTZOFFSETFROM:+0200\r\nTZOFFSETTO:+0100\r\nRRULE:FREQ=YEARLY;BYDAY=-1SU;BYMONTH=10\r\nEND:STANDARD\r\n" +
		"BEGIN:DAYLIGHT\r\nDTSTART:16010101T020000\r\nTZOFFSETFROM:+0100\r\nTZOFFSETTO:+0200\r\nRRULE:FREQ=YEARLY;BYDAY=-1SU;BYMONTH=3\r\nEND:DAYLIGHT\r\nEND:VTIMEZONE\r\n"
	for _, c := range []struct{ name, zones, tz, want string }{
		{"IANA over the feed's", wrongNewYork, "America/New_York", "2026-07-06T13:30:00Z"},
		{"the feed's over the table", ninePacific, "Pacific Standard Time", "2026-07-06T00:30:00Z"},
		{"the table, with no VTIMEZONE", "", "Pacific Standard Time", "2026-07-06T16:30:00Z"},
		{"the table, a zone east", "", "Korea Standard Time", "2026-07-06T00:30:00Z"},
		{"STANDARD alone", korea, "Korea Standard Time", "2026-07-06T00:30:00Z"},
		{"an older name, quoted", older, `"(UTC+01:00) Amsterdam, Berlin, Bern, Rome, Stockholm, Vienna"`, "2026-07-06T07:30:00Z"},
	} {
		f := parseFeed(t, c.zones+single("Sam Lee", c.tz, "20260706T090000", "PT1H"))
		if len(f.Turns) != 1 || whoAt(f.Turns, c.want) != "Sam Lee" || len(f.Notes) > 0 {
			t.Errorf("%s: %+v %q, want a turn at %s", c.name, f.Turns, f.Notes, c.want)
		}
	}
	f := parseFeed(t, single("Sam Lee", "Customized Time Zone", "20260706T090000", "PT1H"))
	if len(f.Turns) != 0 || !strings.Contains(strings.Join(f.Notes, " "), `"Customized Time Zone" that isn't an IANA name`) {
		t.Errorf("a zone none of them is: %+v %q", f.Turns, f.Notes)
	}
}

// A zone from 1601 is brought to the year asked without a step a year: the
// steps a feed's zones take are a few for each year a time is read in.
func TestVTimezoneFrom1601(t *testing.T) {
	f := parseFeed(t, pacific+wEurope+single("Sam Lee", "Pacific Standard Time", "20261102T090000", "PT1H")+single("Ali Kaya", "W. Europe Standard Time", "20261102T090000", "PT1H"))
	if len(f.Turns) != 2 || f.steps == 0 || f.steps > 100 {
		t.Errorf("%d turns, %d steps: a year a step from 1601 would be over 800", len(f.Turns), f.steps)
	}
	// Dates left out in every year from 1970 to 2200: each year worked out
	// once.
	var ex strings.Builder
	for y := 1970; y <= 2200; y++ {
		fmt.Fprintf(&ex, "EXDATE;TZID=Pacific Standard Time:%d0105T090000\r\n", y)
	}
	f = parseFeed(t, pacific+"BEGIN:VEVENT\r\nDTSTART;TZID=Pacific Standard Time:20260105T090000\r\nDURATION:PT1H\r\nRRULE:FREQ=MONTHLY;BYDAY=1MO\r\n"+ex.String()+"SUMMARY:Sam Lee\r\nEND:VEVENT\r\n")
	if len(f.Turns) != 11 || f.steps > 231*30+200 {
		t.Errorf("231 years of dates left out: %d turns, %d steps", len(f.Turns), f.steps)
	}
}

// What a feed's editor could write in a VTIMEZONE, bounded: more of them
// than a feed may hold refuses the feed; one that can't be read passes the
// events in it over, said (as a zone not known is: when they start isn't
// known, so nothing is held from then): too many parts or RDATEs, offsets past any
// zone's or not offsets, a part without its offsets, an onset in UTC, an
// offset changing more often than a zone's, a rule with COUNT making too
// many onsets. A part whose rule never holds a day, and blocks nested
// deep, cost little.
func TestVTimezoneHostile(t *testing.T) {
	zoneOf := func(parts string) string {
		return "BEGIN:VTIMEZONE\r\nTZID:Acme Time\r\n" + parts + "END:VTIMEZONE\r\n"
	}
	part := func(kind, start, from, to, more string) string {
		return "BEGIN:" + kind + "\r\nDTSTART:" + start + "\r\nTZOFFSETFROM:" + from + "\r\nTZOFFSETTO:" + to + "\r\n" + more + "END:" + kind + "\r\n"
	}
	event := single("Sam Lee", "Acme Time", "20261102T090000", "PT1H")

	var flood strings.Builder
	for i := range maxVTimezones + 1 {
		fmt.Fprintf(&flood, "BEGIN:VTIMEZONE\r\nTZID:z%d\r\n%sEND:VTIMEZONE\r\n", i, part("STANDARD", "19700101T000000", "+0100", "+0100", ""))
	}
	cfg := config(t, "  - {name: s, file: x}\n")
	if _, err := Parse(context.Background(), []byte("BEGIN:VCALENDAR\r\n"+flood.String()+event+"END:VCALENDAR\r\n"), cfg.Schedules[0], cfg, now); err == nil || !strings.Contains(err.Error(), "VTIMEZONEs") {
		t.Errorf("%d VTIMEZONEs: %v", maxVTimezones+1, err)
	}

	var parts, rdates strings.Builder
	for i := range maxObservances + 1 {
		parts.WriteString(part("STANDARD", fmt.Sprintf("%04d0101T000000", 1900+i), "+0100", "+0100", ""))
	}
	for i := range maxRdates + 1 {
		if i > 0 {
			rdates.WriteByte(',')
		}
		fmt.Fprintf(&rdates, "%04d0101T000000", 1000+i)
	}
	daily := part("STANDARD", "19700101T000000", "+0200", "+0100", "RRULE:FREQ=DAILY\r\n") + part("DAYLIGHT", "19700101T120000", "+0100", "+0200", "RRULE:FREQ=DAILY\r\n")
	for name, c := range map[string]struct{ zone, says string }{
		"too many parts":    {zoneOf(parts.String()), "more than 64"},
		"too many RDATEs":   {zoneOf(part("STANDARD", "19700101T000000", "+0100", "+0100", "RDATE:"+rdates.String()+"\r\n")), "more than 1000 RDATEs"},
		"24 hours":          {zoneOf(part("STANDARD", "19700101T000000", "+0100", "+2400", "")), "+2400"},
		"past 18 hours":     {zoneOf(part("STANDARD", "19700101T000000", "+0100", "-1801", "")), "more than any zone's"},
		"no minutes":        {zoneOf(part("STANDARD", "19700101T000000", "+0100", "+09", "")), "+09"},
		"no sign":           {zoneOf(part("STANDARD", "19700101T000000", "+0100", "0900", "")), "0900"},
		"not digits":        {zoneOf(part("STANDARD", "19700101T000000", "+0100", "+09a0", "")), "+09a0"},
		"no offsets":        {zoneOf("BEGIN:STANDARD\r\nDTSTART:19700101T000000\r\nEND:STANDARD\r\n"), "without its DTSTART"},
		"no parts":          {zoneOf(""), "no STANDARD or DAYLIGHT"},
		"an onset in UTC":   {zoneOf(part("STANDARD", "19700101T000000Z", "+0100", "+0100", "")), "a time on the clock"},
		"changing daily":    {zoneOf(daily), "changes more than 16 times"},
		"counting too many": {zoneOf(part("STANDARD", "19700101T000000", "+0100", "+0100", "RRULE:FREQ=DAILY;COUNT=5000\r\n")), "more than 2000"},
		"a bad rule":        {zoneOf(part("STANDARD", "19700101T000000", "+0100", "+0100", "RRULE:FREQ=YEARLY;BYDAY=0SU\r\n")), "by day"},
		"periods":           {zoneOf(part("STANDARD", "19700101T000000", "+0100", "+0100", "RDATE;VALUE=PERIOD:19800101T000000/PT1H\r\n")), "periods"},
	} {
		f := parseFeed(t, c.zone+event)
		if len(f.Turns) != 0 || !strings.Contains(strings.Join(f.Notes, " "), c.says) {
			t.Errorf("%s: %+v, notes %q, want one saying %q", name, f.Turns, f.Notes, c.says)
		}
		if f.steps > 3*maxOnsets {
			t.Errorf("%s: %d steps", name, f.steps)
		}
	}
	// A zone's name longer than any is refused, and not kept.
	long := strings.Repeat("Z", maxTZID+1)
	f := parseFeed(t, strings.Replace(zoneOf(part("STANDARD", "19700101T000000", "+0100", "+0100", "")), "Acme Time", long, 1)+single("Sam Lee", long, "20261102T090000", "PT1H"))
	if len(f.Turns) != 0 || !strings.Contains(strings.Join(f.Notes, " "), "over 256 bytes") {
		t.Errorf("a zone's name too long: %+v %q", f.Turns, f.Notes)
	}
	// The furthest offsets a zone may have are read.
	f = parseFeed(t, zoneOf(part("STANDARD", "19700101T000000", "+1800", "-1800", ""))+event)
	if whoAt(f.Turns, "2026-11-03T03:30:00Z") != "Sam Lee" {
		t.Errorf("-1800: %+v %q", f.Turns, f.Notes)
	}
	// 64 parts whose rules never hold a day (30 February): their DTSTARTs
	// are the onsets, looked back for a few periods each.
	var never strings.Builder
	for i := range maxObservances {
		never.WriteString(part("STANDARD", fmt.Sprintf("%04d0101T000000", 1601+i), "+0100", "+0300", "RRULE:FREQ=MONTHLY;BYMONTH=2;BYMONTHDAY=30\r\n"))
	}
	f = parseFeed(t, zoneOf(never.String())+event)
	if whoAt(f.Turns, "2026-11-02T06:30:00Z") != "Sam Lee" || f.steps > 64*(3*12+lookBack)*3 {
		t.Errorf("rules that never hold a day: %+v %q, %d steps", f.Turns, f.Notes, f.steps)
	}
	// Blocks nested deep in a VTIMEZONE, never closed: counted, not
	// followed; the zone is never kept.
	f = parseFeed(t, "BEGIN:VTIMEZONE\r\nTZID:Acme Time\r\n"+strings.Repeat("BEGIN:X-NEST\r\n", 100_000)+part("STANDARD", "19700101T000000", "+0100", "+0100", "")+event)
	if len(f.Turns) != 0 || !strings.Contains(strings.Join(f.Notes, " "), "isn't an IANA name") || f.steps != 0 {
		t.Errorf("nested blocks: %+v %q, %d steps", f.Turns, f.Notes, f.steps)
	}
	// A VTIMEZONE's onsets in every one of its parts, the flood of steps
	// they could cost bounded by the feed's: 64 parts of yearly rules by
	// every day, the zone refused at its first year.
	var everyDay strings.Builder
	for i := range maxObservances {
		everyDay.WriteString(part("STANDARD", "19700101T000000", "+0100", fmt.Sprintf("+%02d00", i%12), "RRULE:FREQ=YEARLY;BYDAY=SU,MO,TU,WE,TH,FR,SA\r\n"))
	}
	f = parseFeed(t, zoneOf(everyDay.String())+event)
	if len(f.Turns) != 0 || !strings.Contains(strings.Join(f.Notes, " "), "changes more than 16") || f.steps > 2*(12+366) {
		t.Errorf("64 parts by every day: %+v %q, %d steps", f.Turns, f.Notes, f.steps)
	}
}
