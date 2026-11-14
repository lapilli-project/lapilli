package ledger

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// TestWindowStarts: a window's starts over four weeks are the hours at
// which Covers begins to say yes, once, every day until a day, every week,
// every month and by cron, in UTC and in a zone whose clocks change in the
// span.
func TestWindowStarts(t *testing.T) {
	ctx := context.Background()
	for _, zone := range []string{"UTC", "America/New_York"} {
		loc, _ := time.LoadLocation(zone)
		l := open(t, loc)
		from := time.Date(2026, 10, 20, 0, 0, 0, 0, loc)
		to := from.Add(28 * 24 * time.Hour)
		for _, w := range []Window{
			{Title: "once", Start: time.Date(2026, 10, 25, 3, 0, 0, 0, loc), End: time.Date(2026, 10, 25, 5, 0, 0, 0, loc)},
			{Title: "once, before", Start: time.Date(2026, 10, 1, 3, 0, 0, 0, loc), End: time.Date(2026, 10, 1, 5, 0, 0, 0, loc)},
			{Title: "daily", Start: time.Date(2026, 10, 1, 1, 0, 0, 0, loc), End: time.Date(2026, 10, 1, 2, 0, 0, 0, loc), Every: "day", Until: time.Date(2026, 11, 5, 0, 0, 0, 0, loc)},
			{Title: "weekly", Start: time.Date(2026, 10, 3, 22, 0, 0, 0, loc), End: time.Date(2026, 10, 3, 23, 0, 0, 0, loc), Every: "week"},
			{Title: "monthly", Start: time.Date(2026, 9, 30, 2, 0, 0, 0, loc), End: time.Date(2026, 9, 30, 4, 0, 0, 0, loc), Every: "month", On: "last"},
			{Title: "cron", Start: time.Date(2026, 10, 3, 2, 0, 0, 0, loc), End: time.Date(2026, 10, 3, 3, 0, 0, 0, loc), Every: "cron", On: "0 2 * * 6"},
		} {
			p, err := l.PreviewWindow(ctx, w)
			if err != nil {
				t.Fatalf("%s %s: %v", zone, w.Title, err)
			}
			var got []time.Time
			for _, f := range p.Falls(from, to, 100) {
				got = append(got, f.Start)
			}
			var want []time.Time
			for at := from; at.Before(to); at = at.Add(time.Hour) {
				if p.Covers(at) && !p.Covers(at.Add(-time.Minute)) {
					want = append(want, at)
				}
			}
			if !slices.EqualFunc(got, want, time.Time.Equal) {
				t.Errorf("%s %s: starts %v, want %v", zone, w.Title, got, want)
			}
			if most := p.Falls(from, to, 2); len(want) > 2 && len(most) != 2 {
				t.Errorf("%s %s: %d starts, at most 2", zone, w.Title, len(most))
			}
		}
	}
}

// TestWindowFallsAsCovered: the repeats a preview lists are what Covers
// covers, minute by minute, and no more: in UTC, and where the clocks skip
// midnight (America/Santiago on 2026-09-06, America/Havana on 2026-03-08),
// where a repeat set at midnight falls, by the calendar, at 23:00 the day
// before, which Covers doesn't take for it; every day, every week, by cron
// and once, at midnight and either side of it.
func TestWindowFallsAsCovered(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		zone string
		skip time.Time // a day whose midnight the clocks skip, or any
	}{{"UTC", time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)}, {"America/Santiago", time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)}, {"America/Havana", time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC)}} {
		loc, err := time.LoadLocation(c.zone)
		if err != nil {
			t.Fatal(err)
		}
		l := open(t, loc)
		y, m, d := c.skip.Date()
		at := func(days, h, min int) time.Time { return time.Date(y, m, d+days, h, min, 0, 0, loc) }
		var ws []Window
		for _, hm := range [][2]int{{0, 0}, {0, 30}, {23, 30}, {1, 0}} {
			for _, long := range []time.Duration{30 * time.Minute, time.Hour, 90 * time.Minute, 3 * time.Hour} {
				ws = append(ws,
					Window{Title: "daily", Start: at(-5, hm[0], hm[1]), End: at(-5, hm[0], hm[1]).Add(long), Every: "day"},
					Window{Title: "daily until", Start: at(-5, hm[0], hm[1]), End: at(-5, hm[0], hm[1]).Add(long), Every: "day", Until: at(0, 0, 0)},
					Window{Title: "weekly", Start: at(-7, hm[0], hm[1]), End: at(-7, hm[0], hm[1]).Add(long), Every: "week"},
					Window{Title: "once", Start: at(-1, hm[0], hm[1]), End: at(-1, hm[0], hm[1]).Add(long)},
				)
			}
		}
		ws = append(ws, Window{Title: "cron", Start: at(-3, 0, 0), End: at(-3, 1, 0), Every: "cron", On: "0 0 * * *"})
		from, to := at(-3, 0, 0), at(4, 0, 0)
		for _, w := range ws {
			p, err := l.PreviewWindow(ctx, w)
			if err != nil {
				t.Fatalf("%s %s: %v", c.zone, w.Title, err)
			}
			falls := p.Falls(from.Add(-48*time.Hour), to, 1000)
			// Each listed is covered from its start to its end.
			for _, f := range falls {
				if f.End.Before(f.Start) || !p.Covers(f.Start) || !p.Covers(f.End) {
					t.Errorf("%s %s: listed %s–%s, which Covers doesn't cover", c.zone, w.Title, f.Start.In(loc).Format("01-02 15:04"), f.End.In(loc).Format("01-02 15:04"))
				}
			}
			for x := from; x.Before(to.Add(-48 * time.Hour)); x = x.Add(time.Minute) {
				listed := slices.ContainsFunc(falls, func(f Fall) bool { return !x.Before(f.Start) && !x.After(f.End) })
				if listed != p.Covers(x) {
					t.Errorf("%s %s %s→%s: at %s, listed %v, Covers %v", c.zone, w.Title, w.Start.In(loc).Format("15:04"), w.End.In(loc).Format("15:04"),
						x.In(loc).Format("2006-01-02 15:04 MST"), listed, p.Covers(x))
					break
				}
			}
		}
	}
}

// TestPreviewWindow: a window previewed is checked as PutWindow checks it,
// a team's editor's within their teams, and nothing is written.
func TestPreviewWindow(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	start := time.Date(2026, 11, 1, 2, 0, 0, 0, time.UTC)
	pay, err := l.PutTeam(ctx, Team{Name: "payments", Rules: "^Pay"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	gen := l.Gen()
	for _, c := range []struct {
		name    string
		ctx     context.Context
		w       Window
		want    error
		invalid bool
	}{
		{"fine", ctx, Window{Title: "w", Start: start, End: start.Add(time.Hour)}, nil, false},
		{"no title", ctx, Window{Start: start, End: start.Add(time.Hour)}, nil, true},
		{"ends first", ctx, Window{Title: "w", Start: start, End: start.Add(-time.Hour)}, nil, true},
		{"a pattern that isn't", ctx, Window{Title: "w", Start: start, End: start.Add(time.Hour), Rules: "("}, nil, true},
		{"no such team", ctx, Window{Title: "w", Start: start, End: start.Add(time.Hour), TeamID: pay + 9}, nil, true},
		{"everyone's, by a team's editor", WithTeams(ctx, []int64{pay}), Window{Title: "w", Start: start, End: start.Add(time.Hour)}, ErrNotYourTeam, false},
		{"their team's", WithTeams(ctx, []int64{pay}), Window{Title: "w", Start: start, End: start.Add(time.Hour), TeamID: pay}, nil, false},
		{"one there isn't", ctx, Window{ID: 99, Title: "w", Start: start, End: start.Add(time.Hour)}, ErrNotFound, false},
	} {
		p, err := l.PreviewWindow(c.ctx, c.w)
		var inv ErrInvalid
		switch {
		case c.invalid:
			if !errors.As(err, &inv) {
				t.Errorf("%s: %v, want it refused as invalid", c.name, err)
			}
		case c.want == nil && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != nil && !errors.Is(err, c.want):
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
		if c.name == "their team's" && (p.own == nil || !p.Applies("PayErrors", nil) || p.Applies("Other", nil)) {
			t.Error("a team's window previewed doesn't apply to its team's rules alone")
		}
	}
	if l.Gen() != gen {
		t.Error("a preview wrote to the ledger")
	}
	if ws, _ := l.Windows(ctx, start.Add(-time.Hour), start.Add(time.Hour)); len(ws) != 0 {
		t.Errorf("a preview kept a window: %v", ws)
	}
}

