package oncall

// A feed, whatever it holds, is read without a panic, its turns not
// overlapping, in five seconds at most, and in no more steps than a feed
// may take.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func FuzzFeed(f *testing.F) {
	for _, name := range []string{"google.ics", "grafana-oncall.ics", "opsgenie.ics", "pagerduty.ics",
		"google-monthly.ics", "outlook.ics", "outlook-no-vtimezone.ics", "pagerduty-monthly.ics", "opsgenie-yearly.ics"} {
		if b, err := os.ReadFile(filepath.Join("testdata", name)); err == nil {
			f.Add(b)
		}
	}
	f.Add([]byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nDTSTART;TZID=Asia/Seoul:20261101T090000\r\nDURATION:P1W\r\nRRULE:FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,TH;WKST=SU;UNTIL=20270101\r\nEXDATE;VALUE=DATE:20261110\r\nATTENDEE;CN=\"Sam; Lee\":mailto:sam@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
	// A VTIMEZONE after the event naming it, with a rule, an RDATE and
	// offsets with seconds; a yearly repeat by numbered day and BYSETPOS.
	f.Add([]byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nDTSTART;TZID=Acme Time:20260105T090000\r\nDURATION:PT8H\r\nRRULE:FREQ=YEARLY;INTERVAL=2;BYMONTH=1,7;BYDAY=1MO,-1FR;BYSETPOS=1,-1;COUNT=6\r\nEXDATE;TZID=Acme Time:20260105T090000\r\nSUMMARY:Sam Lee\r\nEND:VEVENT\r\n" +
		"BEGIN:VTIMEZONE\r\nTZID:Acme Time\r\nBEGIN:STANDARD\r\nDTSTART:16010101T030000\r\nTZOFFSETFROM:+023030\r\nTZOFFSETTO:+0130\r\nRRULE:FREQ=YEARLY;BYDAY=-1SU;BYMONTH=10\r\nEND:STANDARD\r\n" +
		"BEGIN:DAYLIGHT\r\nDTSTART:19800101T020000\r\nTZOFFSETFROM:+0130\r\nTZOFFSETTO:+023030\r\nRDATE:19800330T020000,19810329T020000\r\nRRULE:FREQ=YEARLY;BYMONTHDAY=-1;BYMONTH=3;UNTIL=20300101T000000Z\r\nEND:DAYLIGHT\r\nEND:VTIMEZONE\r\nEND:VCALENDAR\r\n"))
	dir := f.TempDir()
	p := filepath.Join(dir, "on-call.yaml")
	if err := os.WriteFile(p, []byte("schedules:\n  - {name: s, file: x, who: '^(.*) on call$'}\n"+people), 0o600); err != nil {
		f.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		f.Fatal(err)
	}
	s := cfg.Schedules[0]
	f.Fuzz(func(t *testing.T, data []byte) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		fd, err := Parse(ctx, data, s, cfg, now)
		if err != nil {
			return
		}
		if fd.steps > maxFeedSteps {
			t.Fatalf("%d steps", fd.steps)
		}
		for i := 1; i < len(fd.Turns); i++ {
			if fd.Turns[i].From.Before(fd.Turns[i-1].To) {
				t.Fatalf("turns overlap: %+v %+v", fd.Turns[i-1], fd.Turns[i])
			}
		}
	})
}
