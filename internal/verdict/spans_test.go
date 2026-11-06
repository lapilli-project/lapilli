package verdict

import (
	"testing"
	"time"
)

// Every minute, and half minute, of periods across changes of clock, in
// zones with odd offsets and clocks that change at midnight: the span it is
// in says what Of does.
func TestSpansAgreeWithOf(t *testing.T) {
	zones := []string{"UTC", "Asia/Seoul", "America/New_York", "Europe/London", "Australia/Lord_Howe", "Asia/Kathmandu",
		"Pacific/Chatham", "America/Santiago", "Asia/Beirut", "America/Havana", "Asia/Tehran", "America/St_Johns", "Africa/Cairo"}
	configs := []struct{ work, sleep string }{
		{"", ""},
		{"mon-fri@09:10-17:20", "23:10-06:50"},
		{"none", "none"},
		{"sat-mon@00:00-24:00", "01:10-02:40"},
		{"mon-fri@08:00-24:00", "00:00-07:00"},
		{"none", "22:00-24:00"},
	}
	periods := [][2]string{
		{"2026-03-06T13:07:31Z", "2026-03-16T02:11:00Z"}, // US spring forward
		{"2026-10-28T00:00:00Z", "2026-11-04T05:00:00Z"}, // US fall back
		{"2026-03-26T00:00:00Z", "2026-04-07T00:00:00Z"}, // EU, Lord Howe
		{"2026-09-01T00:00:00Z", "2026-09-10T00:00:00Z"}, // Chile spring
		{"2026-04-01T00:00:00Z", "2026-04-08T00:00:00Z"}, // Chile fall
		{"2026-04-20T00:00:00Z", "2026-05-02T00:00:00Z"}, // Egypt
		{"2026-10-20T00:00:00Z", "2026-11-02T00:00:00Z"}, // Cuba fall
	}
	for _, z := range zones {
		loc, err := time.LoadLocation(z)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range configs {
			h := DefaultHours(loc)
			if c.work != "" {
				if err := h.ParseWork(c.work); err != nil {
					t.Fatal(err)
				}
			}
			if c.sleep != "" {
				if err := h.ParseSleep(c.sleep); err != nil {
					t.Fatal(err)
				}
			}
			for _, p := range periods {
				from, _ := time.Parse(time.RFC3339, p[0])
				to, _ := time.Parse(time.RFC3339, p[1])
				sp := h.Spans(from, to)
				// spans in order, disjoint, not across midnight
				for i, s := range sp {
					if !s.From.Before(s.To) {
						t.Errorf("%s %v: empty span %v", z, c, s)
					}
					if i > 0 && s.From.Before(sp[i-1].To) {
						t.Errorf("%s %v: overlap", z, c)
					}
					a, b := s.From.In(loc), s.To.Add(-time.Second).In(loc)
					if a.YearDay() != b.YearDay() {
						t.Errorf("%s %v: span across a day: %s - %s", z, c, a, b)
					}
				}
				j := 0
				bad := 0
				for at := from; at.Before(to); at = at.Add(time.Minute) {
					// sample at each minute; also at minute+30s
					for _, tt := range []time.Time{at, at.Add(30 * time.Second)} {
						if !tt.Before(to) {
							continue
						}
						for j < len(sp) && !tt.Before(sp[j].To) {
							j++
						}
						got := AtWork
						if j < len(sp) && !tt.Before(sp[j].From) {
							got = sp[j].Of
						}
						if want := h.Of(tt); got != want {
							if bad < 3 {
								t.Errorf("%s %v %s: %s (%s): span says %s, Of says %s", z, c, p[0], tt.UTC(), tt.In(loc).Format("Mon 15:04:05 MST"), got, want)
							}
							bad++
						}
					}
				}
				if bad > 3 {
					t.Errorf("%s %v %s: %d more", z, c, p[0], bad-3)
				}
			}
		}
	}
}
