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
			got := p.Starts(from, to, 100)
			var want []time.Time
			for at := from; at.Before(to); at = at.Add(time.Hour) {
				if p.Covers(at) && !p.Covers(at.Add(-time.Minute)) {
					want = append(want, at)
				}
			}
			if !slices.EqualFunc(got, want, time.Time.Equal) {
				t.Errorf("%s %s: starts %v, want %v", zone, w.Title, got, want)
			}
			if most := p.Starts(from, to, 2); len(want) > 2 && len(most) != 2 {
				t.Errorf("%s %s: %d starts, at most 2", zone, w.Title, len(most))
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
