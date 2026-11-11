package ledger

import (
	"context"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/schedule"
)

// brute reports whether t is in any repeat of w: each time its schedule
// falls due from its start, on or before Until's day, lasting End-Start.
func brute(w Window, sch schedule.Schedule, loc *time.Location, t time.Time) bool {
	d := w.End.Sub(w.Start)
	for due := w.Start; !due.IsZero() && !due.After(t); due = sch.Next(due, loc) {
		if !w.Until.IsZero() && civil(due.In(loc)) > civil(w.Until.In(loc)) {
			break
		}
		if !t.Before(due) && !t.After(due.Add(d)) {
			return true
		}
	}
	return false
}

// A window every month or by cron covers what its repeats do, counted one
// by one, years after its first: one whose repeats would meet in a later
// February is refused, the gap measured over four years; and a firing in
// the last day of a long repeat that starts late on its Until's day is
// excused as the runs record it (a review found both).
func TestCoversLikeBruteForce(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		zone, start string
		dur         time.Duration
		every, on   string
		until       string
		probes      []string
		refused     string
	}{
		// From 2027-03-15 the year ahead has 2028's leap February (29d-1h
		// between two); 2029's has 28d-1h, so 28d22h would overlap.
		{"America/New_York", "2027-03-15 02:00", 28*24*time.Hour + 22*time.Hour, "month", "15", "", nil, "shorter than how often"},
		{"UTC", "2027-03-31 00:00", 28*24*time.Hour + 12*time.Hour, "month", "last", "", nil, "shorter than how often"},
		{"America/New_York", "2027-03-15 02:00", 27 * 24 * time.Hour, "month", "15", "2029-12-31",
			[]string{"2029-03-14 12:00", "2029-03-15 12:00", "2029-02-15 02:00", "2029-03-13 23:30", "2029-04-11 02:30"}, ""},
		// A yearly repeat of 31 days, starting late on its Until's day.
		{"UTC", "2024-01-01 23:00", 31 * 24 * time.Hour, "cron", "0 23 1 1 *", "2025-01-01",
			[]string{"2025-02-01 22:00", "2025-02-01 00:30", "2025-01-31 23:30", "2025-02-02 00:30", "2026-01-02 00:00"}, ""},
		{"Europe/Berlin", "2026-01-03 02:00", 25 * 24 * time.Hour, "month", "first sat", "",
			[]string{"2027-02-27 01:00", "2027-03-06 01:59", "2027-03-28 02:30", "2028-03-04 02:30", "2030-03-01 02:00"}, ""},
	} {
		loc, _ := time.LoadLocation(c.zone)
		at := func(s string) time.Time {
			t.Helper()
			for _, layout := range []string{"2006-01-02 15:04", "2006-01-02"} {
				if v, err := time.ParseInLocation(layout, s, loc); err == nil {
					return v
				}
			}
			t.Fatalf("%q", s)
			return time.Time{}
		}
		w := Window{Title: "w", Start: at(c.start), End: at(c.start).Add(c.dur), Every: c.every, On: c.on}
		if c.until != "" {
			w.Until = at(c.until)
		}
		l := open(t, loc)
		id, err := l.PutWindow(ctx, w, "ana", opts)
		if c.refused != "" {
			if err == nil || !strings.Contains(err.Error(), c.refused) {
				t.Errorf("%s every %s %q for %s: %v, want refused", c.zone, c.every, c.on, c.dur, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s %q: %v", c.zone, c.on, err)
		}
		kept, _ := l.WindowByID(ctx, id)
		sch, _ := kept.schedule(loc)
		for _, s := range c.probes {
			p := at(s)
			want := brute(kept, sch, loc, p)
			if got := kept.Covers(p); got != want {
				t.Errorf("%s %q: Covers(%s) = %v, a repeat covers it: %v", c.zone, c.on, s, got, want)
			}
			// And as the runs record a firing then, the window kept before.
			ahead := open(t, loc)
			if _, err := ahead.PutWindow(ctx, w, "ana", opts); err != nil {
				t.Fatal(err)
			}
			record(t, ahead, p.Add(-time.Hour), p.Add(time.Hour), []firing.Firing{{Rule: "R", Start: p, End: p.Add(5 * time.Minute)}})
			if got := len(excused(t, ahead, p.Add(-time.Hour), p.Add(time.Hour), loc)) == 1; got != want {
				t.Errorf("%s %q: a firing at %s recorded: excused %v, want %v", c.zone, c.on, s, got, want)
			}
		}
	}
}

// Beyond four years, a cron's repeats can come closer than the gap the
// window was kept with: "the 31st of January, or a Friday in January" from
// 2027 is two days apart at least until 2032, when the 31st is a Saturday.
// A 36-hour window until 2032-01-30 still covers the Friday's repeat into
// the 31st: its last repeat is looked up at the end of Until's day, not at
// t (a verification found it missed).
func TestCoversPastTheGap(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	start := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	w := Window{Title: "w", Start: start, End: start.Add(36 * time.Hour), Every: "cron", On: "0 0 31 1 5", Until: time.Date(2032, 1, 30, 0, 0, 0, 0, time.UTC)}
	id, err := l.PutWindow(ctx, w, "ana", opts)
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	kept, _ := l.WindowByID(ctx, id)
	sch, _ := kept.schedule(time.UTC)
	for _, p := range []time.Time{time.Date(2032, 1, 31, 6, 0, 0, 0, time.UTC), time.Date(2032, 1, 31, 13, 0, 0, 0, time.UTC),
		time.Date(2031, 2, 1, 6, 0, 0, 0, time.UTC), time.Date(2032, 2, 2, 0, 0, 0, 0, time.UTC)} {
		if got, want := kept.Covers(p), brute(kept, sch, time.UTC, p); got != want {
			t.Errorf("Covers(%s) = %v, a repeat covers it: %v", p, got, want)
		}
	}
}

// A repeat that starts late on Until's day, a day of 25 hours (Berlin's
// clocks going back), and lasts 31 days: the windows a short span in its
// last hour meets include it (a verification found the margin a day).
func TestWindowsMeetingLongDay(t *testing.T) {
	ctx := context.Background()
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip(err)
	}
	l := open(t, berlin)
	start := time.Date(2024, 10, 26, 23, 30, 0, 0, berlin)
	w := Window{Title: "w", Start: start, End: start.Add(31 * 24 * time.Hour), Every: "cron", On: "30 23 26 10 *", Until: time.Date(2025, 10, 26, 0, 0, 0, 0, berlin)}
	id, err := l.PutWindow(ctx, w, "ana", opts)
	if err != nil {
		t.Fatal(err)
	}
	kept, _ := l.WindowByID(ctx, id)
	sch, _ := kept.schedule(berlin)
	p := time.Date(2025, 11, 26, 22, 15, 0, 0, time.UTC) // 23:15 CET
	if !brute(kept, sch, berlin, p) || !kept.Covers(p) {
		t.Fatalf("Covers %v, brute %v", kept.Covers(p), brute(kept, sch, berlin, p))
	}
	if ws, err := l.Windows(ctx, p.Add(-5*time.Minute), p.Add(5*time.Minute)); err != nil || len(ws) != 1 {
		t.Errorf("the windows meeting ten minutes in its last hour: %d %v", len(ws), err)
	}
	record(t, l, p.Add(-10*time.Minute), p.Add(10*time.Minute), []firing.Firing{{Rule: "R", Start: p, End: p.Add(5 * time.Minute)}})
	if got := excused(t, l, p.Add(-time.Hour), p.Add(time.Hour), berlin); len(got) != 1 {
		t.Errorf("a firing then: excused %q", got)
	}
}
