package schedule

import (
	"testing"
	"time"
)

// Next, Last and Prev agree with each other over five years, in zones whose
// daylight saving moves by an hour, half an hour, at midnight or not at all,
// for every kind of schedule; a month's schedule falls due once a month; and
// Gap is the shortest time between two over four years, as counting them
// one by one finds (a review's property test, kept).
func TestScheduleProperties(t *testing.T) {
	zones := []string{"UTC", "Europe/Berlin", "America/New_York", "America/Santiago", "Australia/Lord_Howe", "Pacific/Chatham", "America/Havana"}
	texts := []string{}
	for _, at := range []string{"00:00", "00:30", "02:00", "02:30", "23:30"} {
		for _, d := range []string{"1", "15", "28", "last", "first sat", "second sun", "fourth fri", "last sun", "last sat", "first sun"} {
			texts = append(texts, "monthly "+d+"@"+at)
		}
		texts = append(texts, "daily@"+at, "weekly sun@"+at)
	}
	texts = append(texts, "0 2 * * 6", "30 2 * * *", "30 */3 * * *", "30 1 * * 0", "0 0 31 * *",
		"0 3 1-7 * 0", "0 2 * 3,10,11 0", "45 1,2,3 * * *", "0 0 29 * *", "0 0 L * *")
	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
	fails := 0
	report := func(format string, args ...any) {
		fails++
		if fails < 40 {
			t.Errorf(format, args...)
		}
	}
	for _, zn := range zones {
		loc, err := time.LoadLocation(zn)
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range texts {
			s, err := Parse(text, 0)
			if err != nil {
				continue
			}
			prev := s.Next(from, loc)
			var dues []time.Time
			for !prev.IsZero() && prev.Before(to) && len(dues) < 50000 {
				dues = append(dues, prev)
				prev = s.Next(prev, loc)
			}
			if len(dues) < 2 {
				report("%s %s: %d dues", zn, text, len(dues))
				continue
			}
			minGap := time.Duration(0)
			for i, d := range dues {
				if got := s.Last(d, loc); !got.Equal(d) {
					report("%s %q: Last(due %s) = %s", zn, text, d.In(loc), got.In(loc))
				}
				if got := s.Next(d.Add(-time.Second), loc); !got.Equal(d) {
					report("%s %q: Next(due-1s %s) = %s", zn, text, d.In(loc), got.In(loc))
				}
				if i > 0 {
					a := dues[i-1]
					mid := a.Add(d.Sub(a) / 2)
					if got := s.Last(mid, loc); !got.Equal(a) {
						report("%s %q: Last(mid %s) = %s, want %s", zn, text, mid.In(loc), got.In(loc), a.In(loc))
					}
					if got := s.Prev(d, loc); !got.Equal(a) {
						report("%s %q: Prev(%s) = %s, want %s", zn, text, d.In(loc), got.In(loc), a.In(loc))
					}
					if g := d.Sub(a); minGap == 0 || g < minGap {
						minGap = g
					}
				}
			}
			// monthly: one due per month, consecutive
			if s.kind == monthly {
				for i := 1; i < len(dues); i++ {
					a, b := dues[i-1].In(loc), dues[i].In(loc)
					am := a.Year()*12 + int(a.Month())
					bm := b.Year()*12 + int(b.Month())
					if bm-am != 1 {
						report("%s %q: months skip: %s -> %s", zn, text, a, b)
					}
				}
			}
			// Gap from the first due over four years vs brute force over the same.
			g := s.Gap(dues[0], loc)
			end := dues[0].AddDate(4, 0, 1)
			bf := time.Duration(0)
			for i := 1; i < len(dues) && dues[i-1].Before(end); i++ {
				if x := dues[i].Sub(dues[i-1]); bf == 0 || x < bf {
					bf = x
				}
			}
			if g != bf {
				report("%s %q: Gap = %s, brute force = %s", zn, text, g, bf)
			}
		}
	}
}
