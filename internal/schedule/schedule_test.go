package schedule

import (
	"strings"
	"testing"
	"time"
)

func must(t *testing.T, text string, span time.Duration) Schedule {
	t.Helper()
	s, err := Parse(text, span)
	if err != nil {
		t.Fatalf("%q: %v", text, err)
	}
	return s
}

// The presets and cron fall due when they say, and their spans are the
// calendar's before them.
func TestLastAndSpan(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	at := func(s string) time.Time {
		t.Helper()
		v, err := time.ParseInLocation("2006-01-02 15:04", s, seoul)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	now := at("2026-11-11 10:30") // a Wednesday
	for _, c := range []struct {
		text          string
		span          time.Duration
		due, from, to string
	}{
		{"daily@09:00", 0, "2026-11-11 09:00", "2026-11-10 00:00", "2026-11-11 00:00"},
		{"daily@11:00", 0, "2026-11-10 11:00", "2026-11-09 00:00", "2026-11-10 00:00"},
		{"weekly mon@09:00", 0, "2026-11-09 09:00", "2026-11-02 00:00", "2026-11-09 00:00"},
		{"weekly wed@11:00", 0, "2026-11-04 11:00", "2026-10-26 00:00", "2026-11-02 00:00"},
		{"monthly 1@09:00", 0, "2026-11-01 09:00", "2026-10-01 00:00", "2026-11-01 00:00"},
		{"monthly last@09:00", 0, "2026-10-31 09:00", "2026-09-01 00:00", "2026-10-01 00:00"},
		{"0 9 * * 1-5", 0, "2026-11-11 09:00", "2026-11-10 09:00", "2026-11-11 09:00"},
		{"0 9 * * 1", 0, "2026-11-09 09:00", "2026-11-02 09:00", "2026-11-09 09:00"},
		{"30 */6 * * *", 0, "2026-11-11 06:30", "2026-11-11 00:30", "2026-11-11 06:30"},
		{"0 8 * * mon-fri", 24 * time.Hour, "2026-11-11 08:00", "2026-11-10 08:00", "2026-11-11 08:00"},
		{"0 9 15 * 0", 0, "2026-11-08 09:00", "2026-11-01 09:00", "2026-11-08 09:00"}, // the 15th or a Sunday
	} {
		s := must(t, c.text, c.span)
		due := s.Last(now, seoul)
		from, to := s.Span(due, seoul)
		if !due.Equal(at(c.due)) || !from.Equal(at(c.from)) || !to.Equal(at(c.to)) {
			t.Errorf("%q: due %s, span %s → %s; want %s, %s → %s", c.text, due.In(seoul), from.In(seoul), to.In(seoul), c.due, c.from, c.to)
		}
	}
}

// Counted on the calendar: a time daylight saving skips falls due at the
// first instant after it, a time it repeats once, the first time; a day
// still starts on itself where the clocks skip midnight.
func TestDaylightSaving(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	s := must(t, "daily@02:30", 0)
	// 2026-03-08: 02:00 → 03:00.
	due := s.Last(time.Date(2026, 3, 8, 12, 0, 0, 0, ny), ny)
	if want := time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC); !due.Equal(want) {
		t.Errorf("a skipped time: %s, want the jump, %s", due.In(ny), want.In(ny))
	}
	// 2026-11-01: 01:30 happens twice; the first.
	r := must(t, "daily@01:30", 0)
	due = r.Last(time.Date(2026, 11, 1, 12, 0, 0, 0, ny), ny)
	if want := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC); !due.Equal(want) {
		t.Errorf("a repeated time: %s, want the first, %s", due.UTC(), want)
	}
	// Once that day: the next is the next day's, the hour repeated between.
	next := r.Last(time.Date(2026, 11, 2, 12, 0, 0, 0, ny), ny)
	if got := next.Sub(due); got != 25*time.Hour {
		t.Errorf("the day after a repeated hour: %s apart", got)
	}
	if again := r.Last(due.Add(time.Hour+time.Minute), ny); !again.Equal(due) {
		t.Errorf("due again at the second 01:30: %s", again.UTC())
	}
	sa, _ := time.LoadLocation("America/Santiago")
	d := must(t, "daily@09:00", 0)
	from, to := d.Span(d.Last(time.Date(2026, 9, 7, 12, 0, 0, 0, sa), sa), sa)
	if got := from.In(sa); got.Day() != 6 || got.Hour() != 1 || to.In(sa).Day() != 7 {
		t.Errorf("where midnight is skipped: %s → %s", from.In(sa), to.In(sa))
	}
}

// What isn't a schedule is refused, and so is anything more often than
// hourly.
func TestRefused(t *testing.T) {
	for _, c := range []struct {
		text string
		span time.Duration
		says string
	}{
		{"", 0, "a schedule is"},
		{strings.Repeat("x", 101), 0, "a schedule is"},
		{"daily@9", 0, "09:00"},
		{"daily@24:00", 0, "09:00"},
		{"weekly@mon@09:00", 0, "five fields"},
		{"weekly xyz@09:00", 0, "weekly DAY"},
		{"monthly 29@09:00", 0, "1 to 28"},
		{"daily@09:00", time.Hour, "only with a cron"},
		{"* * * * *", 0, "hourly at most"},
		{"*/5 * * * *", 0, "hourly at most"},
		{"0,30 * * * *", 0, "hourly at most"},
		{"0 9 * *", 0, "five fields"},
		{"0 25 * * *", 0, "0 to 23"},
		{"0 9 * * 8", 0, "0 to 7"},
		{"0 9 5-1 * *", 0, "goes up"},
		{"0 9 * * */0", 0, "a step"},
		{"0 9 31 2 *", 0, "every year"},
		{"0 9 29 2 *", 0, "every year"},
		{"0 9 * * *", 30 * time.Minute, "an hour at least"},
		{"0 9 * * *", 100 * 24 * time.Hour, "up to 92 days"},
	} {
		if _, err := Parse(c.text, c.span); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%q, %s: %v, want one saying %q", c.text, c.span, err, c.says)
		}
	}
	// February has its first 28 days every year.
	if _, err := Parse("0 9 1-30 2 *", 0); err != nil {
		t.Errorf("February's first days: %v", err)
	}
}
