package ledger

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
)

// A team's editors keep MaxTeamWindows of its quiet windows at most, and
// leave the ledger's last EditorWindows to editors and admins; nobody keeps
// more than MaxWindows. Each window is looked at for the firings it may
// excuse every time stones are given, so the role that may change the
// least can't make judging slow for everyone.
func TestWindowsLimited(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	pay, err := l.PutTeam(ctx, Team{Name: "payments", Rules: "^Pay"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	search, err := l.PutTeam(ctx, Team{Name: "search", Rules: "^Search"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cron := func(team int64) Window {
		return Window{Title: "new year", Start: start, End: start.Add(time.Hour), Every: "cron", On: "0 0 1 1 *", TeamID: team}
	}
	payer := WithTeams(ctx, []int64{pay})
	for i := range MaxTeamWindows {
		if _, err := l.PutWindow(payer, cron(pay), "payer", opts); err != nil {
			t.Fatalf("a team's window %d: %v", i, err)
		}
	}
	var inv ErrInvalid
	if _, err := l.PutWindow(payer, cron(pay), "payer", opts); !errors.As(err, &inv) || !strings.Contains(err.Error(), "of its quiet windows at most") {
		t.Errorf("a team's editors past %d of its windows: %v", MaxTeamWindows, err)
	}
	// An editor of every team, or an admin, keeps more of a team's.
	if _, err := l.PutWindow(ctx, cron(pay), "ed", opts); err != nil {
		t.Errorf("an editor, past a team's own limit: %v", err)
	}
	// The ledger's last windows are for editors and admins.
	for n := MaxTeamWindows + 1; n < MaxWindows-EditorWindows; n++ {
		if _, err := l.PutWindow(ctx, cron(0), "ed", opts); err != nil {
			t.Fatal(n, err)
		}
	}
	if _, err := l.PutWindow(WithTeams(ctx, []int64{search}), cron(search), "searcher", opts); !errors.As(err, &inv) || !strings.Contains(err.Error(), "for editors and admins") {
		t.Errorf("a team's editors, in the ledger's last %d: %v", EditorWindows, err)
	}
	for n := MaxWindows - EditorWindows; n < MaxWindows; n++ {
		if _, err := l.PutWindow(ctx, cron(search), "ed", opts); err != nil {
			t.Fatal(n, err)
		}
	}
	if _, err := l.PutWindow(ctx, cron(0), "ed", opts); !errors.As(err, &inv) || !strings.Contains(err.Error(), "quiet windows at most") {
		t.Errorf("past %d windows: %v", MaxWindows, err)
	}
	// A window changed is no new one.
	ws, err := l.Windows(ctx, start, start.AddDate(1, 0, 0))
	if err != nil || len(ws) != MaxWindows {
		t.Fatalf("%d windows kept (%v)", len(ws), err)
	}
	w := ws[0]
	w.Title = "new year, renamed"
	if _, err := l.PutWindow(ctx, w, "ed", opts); err != nil {
		t.Errorf("changing a window at the limit: %v", err)
	}
}

// Excusing says what Excuses says of each firing, whether it started in
// the span or not: windows of every kind, of everyone's or a team's, with
// a pattern, labels or neither.
func TestExcusingAsExcuses(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	pay, err := l.PutTeam(ctx, Team{Name: "payments", Rules: "^Pay"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutTeam(ctx, Team{Name: "edge", Labels: map[string][]string{"team": {"edge"}}}, "ana"); err != nil {
		t.Fatal(err)
	}
	at := func(s string) time.Time {
		x, err := time.Parse("2006-01-02 15:04", s)
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	for _, w := range []Window{
		{Title: "once", Start: at("2026-11-03 01:00"), End: at("2026-11-03 05:00")},
		{Title: "nightly", Start: at("2026-10-01 02:00"), End: at("2026-10-01 03:00"), Every: "day", Rules: "^Pay"},
		{Title: "weekly", Start: at("2026-10-04 10:00"), End: at("2026-10-04 12:00"), Every: "week", Until: at("2026-11-08 00:00"), TeamID: pay},
		{Title: "monthly", Start: at("2026-10-01 00:00"), End: at("2026-10-02 00:00"), Every: "month", On: "1", Labels: map[string][]string{"service": {"checkout"}}},
		{Title: "first saturday", Start: at("2026-10-03 22:00"), End: at("2026-10-04 02:00"), Every: "month", On: "first sat", TeamID: pay},
		{Title: "new year", Start: at("2026-01-01 00:00"), End: at("2026-01-01 06:00"), Every: "cron", On: "0 0 1 1 *"},
		{Title: "hourly", Start: at("2026-10-01 00:00"), End: at("2026-10-01 00:10"), Every: "cron", On: "0 * * * *", Rules: "Search|Pay"},
		{Title: "until", Start: at("2026-10-05 08:00"), End: at("2026-10-05 09:00"), Every: "cron", On: "0 8 * * mon", Until: at("2026-11-02 00:00")},
	} {
		if _, err := l.PutWindow(ctx, w, "ana", opts); err != nil {
			t.Fatal(w.Title, err)
		}
	}
	r := rand.New(rand.NewPCG(7, 8))
	rules := []string{"PayErrors", "PayLatency", "SearchDown", "EdgeDown", "Disk"}
	for _, span := range [][2]time.Time{
		{at("2026-11-02 00:00"), at("2026-11-09 00:00")},
		{at("2026-11-03 00:00"), at("2026-11-03 06:00")},
		{at("2026-10-01 00:00"), at("2026-10-01 01:00")},
		{at("2026-12-31 00:00"), at("2027-01-02 00:00")},
	} {
		from, to := span[0], span[1]
		ws, err := l.Windows(ctx, from.Add(-maxWindow), to)
		if err != nil {
			t.Fatal(err)
		}
		excuses := Excusing(ws, from, to)
		for range 3000 {
			// Mostly in the span, some before and after it.
			start := from.Add(time.Duration(r.Int64N(int64(to.Sub(from)+4*24*time.Hour))) - 2*24*time.Hour).Truncate(time.Minute)
			f := firing.Firing{Rule: rules[r.IntN(len(rules))], Start: start, End: start.Add(5 * time.Minute),
				Labels: map[string]string{"service": []string{"checkout", "cart"}[r.IntN(2)], "team": []string{"edge", "core"}[r.IntN(2)]}}
			want := slices.ContainsFunc(ws, func(w Window) bool { return w.Excuses(f) })
			if got := excuses(f); got != want {
				t.Fatalf("span %s to %s, %s %v at %s: excused %v, Excuses says %v", from, to, f.Rule, f.Labels, f.Start, got, want)
			}
		}
	}
}

// TestWindowsScale holds judging to a budget beside as many quiet windows
// as the ledger keeps: a team's editors' twenty, falling due once a year,
// covering none of a week's firings, and an editor's, the rest, half the
// same, half of another's rules, a minute each night. Set beside
// every firing that caught nothing, each schedule looked back over up to a
// year, they took 106 s to judge 20,000 firings again; worked out once a
// span, for a rule's own and its team's, they take next to nothing more.
func TestWindowsScale(t *testing.T) {
	if race {
		t.Skip("the race detector slows SQLite many times over")
	}
	ctx := context.Background()
	l := open(t, time.UTC)
	pay, err := l.PutTeam(ctx, Team{Name: "payments", Rules: "^Pay"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	newYear := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	payer := WithTeams(ctx, []int64{pay})
	for range MaxTeamWindows {
		if _, err := l.PutWindow(payer, Window{Title: "new year", Start: newYear, End: newYear.Add(time.Hour), Every: "cron", On: "0 0 1 1 *", TeamID: pay}, "payer", opts); err != nil {
			t.Fatal(err)
		}
	}
	for i := MaxTeamWindows; i < MaxWindows; i++ {
		w := Window{Title: "yearly", Start: newYear, End: newYear.Add(time.Hour), Every: "cron", On: "0 0 1 1 *"}
		if i%2 == 0 {
			w = Window{Title: "a minute a night", Start: newYear.Add(3 * time.Hour), End: newYear.Add(3*time.Hour + time.Minute), Every: "cron", On: "0 3 * * *", Rules: "^Other"}
		}
		if _, err := l.PutWindow(ctx, w, "ed", opts); err != nil {
			t.Fatal(err)
		}
	}
	r := rand.New(rand.NewPCG(1, 2))
	var fs []firing.Firing
	for i := range 10_000 {
		start := t0.Add(time.Duration(r.Int64N(int64(7 * 24 * time.Hour)))).Truncate(time.Second)
		fs = append(fs, firing.Firing{Rule: fmt.Sprintf("%s%02d", []string{"Pay", "Search"}[i%2], r.IntN(40)),
			Labels: map[string]string{"service": "checkout", "instance": fmt.Sprint(i)}, Start: start, End: start.Add(3 * time.Minute)})
	}
	record(t, l, t0, t0.Add(7*24*time.Hour), fs)
	began := time.Now()
	if err := l.JudgeAll(ctx, opts, nil); err != nil {
		t.Fatal(err)
	}
	took := time.Since(began)
	t.Logf("judging 10,000 firings again beside %d quiet windows: %s", MaxWindows, took.Round(time.Millisecond))
	if budget := time.Duration(float64(2*time.Second) * slow); took > budget {
		t.Errorf("judging 10,000 firings again beside %d quiet windows took %s, over %s", MaxWindows, took, budget)
	}
}

// Excusing says what Excuses says across a change of the clocks, in a
// zone with daylight saving: repeats on a time the clocks skip, or repeat,
// worked out once a span, are those Covers finds.
func TestExcusingAsExcusesAcrossDST(t *testing.T) {
	ctx := context.Background()
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip(err)
	}
	l := open(t, berlin)
	at := func(s string) time.Time {
		x, err := time.ParseInLocation("2006-01-02 15:04", s, berlin)
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	for _, w := range []Window{
		{Title: "skipped", Start: at("2026-03-01 02:30"), End: at("2026-03-01 03:15"), Every: "cron", On: "30 2 * * *"},
		{Title: "repeated", Start: at("2026-10-01 02:30"), End: at("2026-10-01 02:50"), Every: "cron", On: "30 2 * * *"},
		{Title: "sundays", Start: at("2026-03-01 01:00"), End: at("2026-03-01 04:00"), Every: "cron", On: "0 1 * * sun", Until: at("2026-10-25 00:00")},
		{Title: "last sunday", Start: at("2026-03-29 00:00"), End: at("2026-03-29 06:00"), Every: "month", On: "last sun"},
		{Title: "hourly", Start: at("2026-03-01 00:00"), End: at("2026-03-01 00:05"), Every: "cron", On: "0 * * * *"},
		{Title: "yearly", Start: at("2026-03-29 04:00"), End: at("2026-03-29 05:00"), Every: "cron", On: "0 4 29 3 *"},
	} {
		if _, err := l.PutWindow(ctx, w, "ana", opts); err != nil {
			t.Fatal(w.Title, err)
		}
	}
	r := rand.New(rand.NewPCG(9, 10))
	for _, span := range [][2]time.Time{
		{at("2026-03-27 00:00"), at("2026-04-01 00:00")},
		{at("2026-03-29 01:00"), at("2026-03-29 04:00")},
		{at("2026-10-23 00:00"), at("2026-10-28 00:00")},
		{at("2026-10-25 02:00"), at("2026-10-25 03:00")},
	} {
		from, to := span[0], span[1]
		ws, err := l.Windows(ctx, from.Add(-maxWindow), to)
		if err != nil {
			t.Fatal(err)
		}
		excuses := Excusing(ws, from, to)
		for range 4000 {
			start := from.Add(time.Duration(r.Int64N(int64(to.Sub(from)+4*time.Hour))) - 2*time.Hour).Truncate(time.Minute)
			f := firing.Firing{Rule: "Disk", Start: start, End: start.Add(time.Minute)}
			want := slices.ContainsFunc(ws, func(w Window) bool { return w.Excuses(f) })
			if got := excuses(f); got != want {
				t.Fatalf("span %s to %s, a firing at %s: excused %v, Excuses says %v", from, to, f.Start, got, want)
			}
		}
	}
}

// TestWindowsCoverScale holds to a budget a day's 10,000 firings of a
// team, recorded and judged in the write that records them, beside quiet
// windows that do meet the day: each falling due once a year, in the
// day's last hour, after the firings, so each firing's Covers looked back a
// year for each window. 500 took 25.9 s; worked out once a span, the
// repeats a firing is set beside by a search, they take next to nothing.
func TestWindowsCoverScale(t *testing.T) {
	if race {
		t.Skip("the race detector slows SQLite many times over")
	}
	budget := map[int]time.Duration{0: 2 * time.Second, MaxTeamWindows: 2 * time.Second, MaxWindows: 3 * time.Second}
	for _, n := range []struct{ team, all int }{{0, 0}, {MaxTeamWindows, MaxTeamWindows}, {MaxTeamWindows, MaxWindows}} {
		ctx := context.Background()
		l := open(t, time.UTC)
		pay, err := l.PutTeam(ctx, Team{Name: "payments", Rules: "^Pay"}, "ana")
		if err != nil {
			t.Fatal(err)
		}
		payer := WithTeams(ctx, []int64{pay})
		end := t0.Add(7*24*time.Hour - time.Hour)
		on := fmt.Sprintf("%d %d %d %d *", end.Minute(), end.Hour(), end.Day(), int(end.Month()))
		first := end.AddDate(-1, 0, 0)
		for i := range n.all {
			c, team := payer, pay
			if i >= n.team {
				c, team = ctx, 0
			}
			if _, err := l.PutWindow(c, Window{Title: "yearly", Start: first, End: first.Add(time.Minute), Every: "cron", On: on, TeamID: team, Rules: "^Pay"}, "x", opts); err != nil {
				t.Fatal(err)
			}
		}
		r := rand.New(rand.NewPCG(1, 2))
		var fs []firing.Firing
		for i := range 10_000 {
			start := t0.Add(6*24*time.Hour + time.Duration(r.Int64N(int64(22*time.Hour)))).Truncate(time.Second)
			fs = append(fs, firing.Firing{Rule: fmt.Sprintf("Pay%02d", r.IntN(40)),
				Labels: map[string]string{"service": "checkout", "instance": fmt.Sprint(i)}, Start: start, End: start.Add(3 * time.Minute)})
		}
		began := time.Now()
		record(t, l, t0.Add(6*24*time.Hour), t0.Add(7*24*time.Hour), fs)
		took := time.Since(began)
		t.Logf("recording a day's 10,000 firings beside %d windows (%d a team's): %s", n.all, n.team, took.Round(time.Millisecond))
		if max := time.Duration(float64(budget[n.all]) * slow); took > max {
			t.Errorf("recording a day's 10,000 firings beside %d windows took %s, over %s", n.all, took, max)
		}
	}
}

// A team's editors can't move windows from one of their teams to another
// past the second's twenty: an editor of two teams made twenty in the
// first, moved them, and made twenty more, as many times as they liked.
func TestTeamWindowsMovedLimited(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	pay, err := l.PutTeam(ctx, Team{Name: "payments", Rules: "^Pay"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	search, err := l.PutTeam(ctx, Team{Name: "search", Rules: "^Search"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	both := WithTeams(ctx, []int64{pay, search})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	w := Window{Title: "new year", Start: start, End: start.Add(time.Hour), Every: "cron", On: "0 0 1 1 *"}
	var ids []int64
	for range MaxTeamWindows {
		w.TeamID = search
		if _, err := l.PutWindow(both, w, "both", opts); err != nil {
			t.Fatal(err)
		}
		w.TeamID = pay
		id, err := l.PutWindow(both, w, "both", opts)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	moved := w
	moved.ID, moved.TeamID = ids[0], search
	var inv ErrInvalid
	if _, err := l.PutWindow(both, moved, "both", opts); !errors.As(err, &inv) || !strings.Contains(err.Error(), "of its quiet windows at most") {
		t.Errorf("a window moved past a team's %d: %v", MaxTeamWindows, err)
	}
	// Within the team, a window changes as it may.
	moved.TeamID, moved.Title = pay, "renamed"
	if _, err := l.PutWindow(both, moved, "both", opts); err != nil {
		t.Errorf("a window kept in its team: %v", err)
	}
	// An editor moves it.
	moved.TeamID = search
	if _, err := l.PutWindow(ctx, moved, "ed", opts); err != nil {
		t.Errorf("an editor moving a window: %v", err)
	}
}
