package oncall

// A feed, whatever it holds, is read without a panic, its turns not
// overlapping, in five seconds at most.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func FuzzFeed(f *testing.F) {
	for _, name := range []string{"google.ics", "grafana-oncall.ics", "opsgenie.ics", "pagerduty.ics"} {
		if b, err := os.ReadFile(filepath.Join("testdata", name)); err == nil {
			f.Add(b)
		}
	}
	f.Add([]byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nDTSTART;TZID=Asia/Seoul:20261101T090000\r\nDURATION:P1W\r\nRRULE:FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,TH;WKST=SU;UNTIL=20270101\r\nEXDATE;VALUE=DATE:20261110\r\nATTENDEE;CN=\"Sam; Lee\":mailto:sam@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
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
		for i := 1; i < len(fd.Turns); i++ {
			if fd.Turns[i].From.Before(fd.Turns[i-1].To) {
				t.Fatalf("turns overlap: %+v %+v", fd.Turns[i-1], fd.Turns[i])
			}
		}
	})
}
