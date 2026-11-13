package ledger

import (
	"context"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/verdict"
)

// A team's own hours are kept, refused when they aren't hours, and take what
// they don't set from the server's.
func TestTeamHours(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	id, err := l.PutTeam(ctx, Team{Name: "search", Rules: "^Search", WorkHours: " mon-thu@09:00-17:00 ", TZ: "America/New_York"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Team{{TZ: "Nowhere/Here"}, {TZ: "/etc/localtime"}, {WorkHours: "9-5"}, {SleepHours: "22:00-22:00"}} {
		bad.Name, bad.Rules = "bad", "x"
		if _, err := l.PutTeam(ctx, bad, "ana"); err == nil || !strings.Contains(err.Error(), "want") {
			t.Errorf("%+v kept: %v", bad, err)
		}
	}
	teams, err := l.Teams(ctx)
	if err != nil || len(teams) != 1 || teams[0].ID != id {
		t.Fatalf("teams: %+v %v", teams, err)
	}
	tm := teams[0]
	if tm.WorkHours != "mon-thu@09:00-17:00" || tm.SleepHours != "" || tm.TZ != "America/New_York" {
		t.Errorf("kept: %+v", tm)
	}
	server := verdict.DefaultHours(time.UTC)
	if err := server.ParseSleep("23:00-07:00"); err != nil {
		t.Fatal(err)
	}
	h, err := tm.Hours(server)
	if err != nil {
		t.Fatal(err)
	}
	if u := h.Used(); u.Zone != "America/New_York" || u.Work != "mon-thu@09:00-17:00" || u.Sleep != "23:00-07:00" {
		t.Errorf("hours: %+v", u)
	}
	if u := server.Used(); u.Zone != "UTC" || u.Work != "mon-fri@08:00-18:00" {
		t.Errorf("the server's changed: %+v", u)
	}
	if h, err := (Team{Name: "x"}).Hours(server); h != nil || err != nil {
		t.Errorf("a team without hours: %v %v", h, err)
	}
	if h, err := (Team{SleepHours: "none"}).Hours(nil); err != nil || h.Sleep.From != h.Sleep.To || h.Loc != time.UTC {
		t.Errorf("none, without the server's: %+v %v", h, err)
	}
}

// CountHours counts each rule by its own hours, reading the firings once
// for each of the hours, and starts each rule's count afresh.
func TestCountHours(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	// 03:00 UTC on the Monday: asleep in UTC, at work in Tokyo (12:00).
	at := t0.Add(3 * time.Hour)
	svc := map[string]string{"service": "search"}
	record(t, l, t0, t0.Add(6*time.Hour), []firing.Firing{
		{Rule: "Here", Labels: svc, Start: at, End: at.Add(time.Minute)},
		{Rule: "Tokyo", Labels: svc, Start: at, End: at.Add(time.Minute)},
	})
	utc := verdict.DefaultHours(time.UTC)
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	jst := verdict.DefaultHours(tokyo)
	rules := []verdict.RuleResult{{Rule: "Here", Asleep: 9}, {Rule: "Tokyo", AsleepBlack: 9}, {Rule: "None", OffHours: 9}}
	err := l.CountHours(ctx, rules, t0, t0.Add(24*time.Hour), func(rule string) *verdict.Hours {
		return map[string]*verdict.Hours{"Here": utc, "Tokyo": jst}[rule]
	})
	if err != nil {
		t.Fatal(err)
	}
	if r := rules[0]; r.Asleep != 1 || r.AsleepBlack != 1 || r.OffHours != 0 {
		t.Errorf("Here: %+v", r)
	}
	if r := rules[1]; r.Asleep != 0 || r.AsleepBlack != 0 || r.OffHours != 0 {
		t.Errorf("Tokyo: %+v", r)
	}
	if r := rules[2]; r.OffHours != 0 {
		t.Errorf("a rule by no hours: %+v", r)
	}
}

// A team's shifts are kept and refused as its hours are, take the rest of
// the team's hours from it, and count its rules by whose turn it is.
func TestTeamShifts(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	const shifts = "every=1w from=2026-11-02T09:00; tz=Asia/Seoul; tz=Europe/Berlin sleep=23:00-07:00"
	if _, err := l.PutTeam(ctx, Team{Name: "search", Rules: "^Search", Shifts: " " + shifts + " "}, "ana"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Team{{Shifts: "every=1w"}, {Shifts: "every=1w from=2026-11-02; tz=Nowhere"}, {Shifts: strings.Repeat("x", 1001)}} {
		bad.Name, bad.Rules = "bad", "x"
		if _, err := l.PutTeam(ctx, bad, "ana"); err == nil || !strings.Contains(err.Error(), "shifts") {
			t.Errorf("%.40s kept: %v", bad.Shifts, err)
		}
	}
	// Saying why.
	if _, err := l.PutTeam(ctx, Team{Name: "bad", Rules: "x", Shifts: "every=1w from=2026-11-02; tz=Nowhere"}, "ana"); err == nil || !strings.Contains(err.Error(), `"Nowhere" isn't a time zone`) {
		t.Errorf("why: %v", err)
	}
	teams, err := l.Teams(ctx)
	if err != nil || len(teams) != 1 || teams[0].Shifts != shifts {
		t.Fatalf("teams: %+v %v", teams, err)
	}
	// The server's shifts give way to a team's own hours.
	server := verdict.DefaultHours(time.UTC)
	ss, err := verdict.ParseShifts("every=1d from=2026-11-02; sleep=none; tz=Asia/Tokyo", server)
	if err != nil {
		t.Fatal(err)
	}
	server.Shifts = ss
	h, err := teams[0].Hours(server)
	if err != nil || h.Shifts == nil || h.Used().Shifts != shifts || len(h.Shifts.Turns) != 2 {
		t.Fatalf("hours: %+v %v", h, err)
	}
	if h, err := (Team{TZ: "Asia/Seoul"}).Hours(server); err != nil || h.Shifts != nil {
		t.Errorf("a team's zone, the server's shifts: %+v %v", h, err)
	}
	if HoursKey(h) == HoursKey(&verdict.Hours{Loc: h.Loc, Days: h.Days, Work: h.Work, Sleep: h.Sleep}) {
		t.Error("shifts aren't in the key")
	}

	// 03:00 UTC on Monday 2 November, before the first turn: Berlin's, at
	// 04:00 there, asleep. A week later, Seoul's, at 12:00: at work.
	mon := time.Date(2026, 11, 2, 3, 0, 0, 0, time.UTC)
	svc := map[string]string{"service": "search"}
	var fs []firing.Firing
	for _, r := range []string{"SearchA", "SearchB"} {
		for _, at := range []time.Time{mon, mon.AddDate(0, 0, 7)} {
			fs = append(fs, firing.Firing{Rule: r, Labels: svc, Start: at, End: at.Add(time.Minute)})
		}
	}
	record(t, l, mon.Add(-3*time.Hour), mon.AddDate(0, 0, 8), fs)
	rules := []verdict.RuleResult{{Rule: "SearchA"}, {Rule: "SearchB"}, {Rule: "Other"}}
	if err := l.CountHours(ctx, rules, mon.Add(-3*time.Hour), mon.AddDate(0, 0, 8), func(rule string) *verdict.Hours {
		if strings.HasPrefix(rule, "Search") {
			return h
		}
		return verdict.DefaultHours(time.UTC)
	}); err != nil {
		t.Fatal(err)
	}
	for _, r := range rules[:2] {
		if r.Asleep != 1 || r.AsleepBlack != 1 || r.OffHours != 0 {
			t.Errorf("%s: %+v", r.Rule, r)
		}
	}
}

// Hours that change often are counted row by row, alike.
func TestByHoursShortShifts(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	svc := map[string]string{"service": "search"}
	var fs []firing.Firing
	for i := 0; i < 7*24*4; i++ {
		at := t0.Add(time.Duration(i)*15*time.Minute + 7*time.Minute)
		fs = append(fs, firing.Firing{Rule: "Search", Labels: svc, Start: at, End: at.Add(time.Minute)})
	}
	record(t, l, t0, t0.AddDate(0, 0, 7), fs)
	h := verdict.DefaultHours(time.UTC)
	sh, err := verdict.ParseShifts("every=1h from=2026-11-02; tz=Asia/Tokyo; tz=Europe/Berlin; tz=America/New_York", h)
	if err != nil {
		t.Fatal(err)
	}
	h.Shifts = sh
	if n := len(h.Spans(t0, t0.AddDate(0, 0, 7))); n <= 6*8 {
		t.Fatalf("only %d spans: not row by row", n)
	}
	by, err := l.RuleHours(ctx, t0, t0.AddDate(0, 0, 7), h)
	if err != nil {
		t.Fatal(err)
	}
	var want HourTally
	for _, f := range fs {
		switch h.Of(f.Start) {
		case verdict.Asleep:
			want.Asleep++
		case verdict.OffHours:
			want.OffHours++
		}
	}
	want.AsleepBlack, want.OffHoursBlack = want.Asleep, want.OffHours
	var got HourTally
	for _, x := range by {
		got = x
	}
	if got != want || want.Asleep == 0 || want.OffHours == 0 {
		t.Errorf("%+v, want %+v", got, want)
	}
}

// A rule's firings are counted by the hours they came in, excused ones
// aside, read by their rule's own (ByHoursOf) as by every rule's.
func TestHoursOfExcused(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	// 03:00 UTC on the Monday, asleep in UTC; a quiet window over the first.
	at := t0.Add(3 * time.Hour)
	if _, err := l.PutWindow(ctx, Window{Title: "upgrade", Start: at.Add(-time.Minute), End: at.Add(10 * time.Minute)}, "ana", opts); err != nil {
		t.Fatal(err)
	}
	svc := map[string]string{"service": "search"}
	record(t, l, t0, t0.Add(6*time.Hour), []firing.Firing{
		{Rule: "Night", Labels: svc, Start: at.Add(time.Minute), End: at.Add(2 * time.Minute)},
		{Rule: "Night", Labels: svc, Start: at.Add(30 * time.Minute), End: at.Add(31 * time.Minute)},
	})
	var id int64
	if err := l.r.QueryRowContext(ctx, "SELECT id FROM rules WHERE name = 'Night'").Scan(&id); err != nil {
		t.Fatal(err)
	}
	h := verdict.DefaultHours(time.UTC)
	if err := h.ParseSleep("23:00-07:00"); err != nil {
		t.Fatal(err)
	}
	day := l.day(t0.Unix())
	of, err := l.ByHoursOf(ctx, day, day+7, h, []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	all, err := l.ByHours(ctx, day, day+7, h)
	if err != nil {
		t.Fatal(err)
	}
	week := Week(day)
	if got := of[week][id]; got.Asleep != 1 {
		t.Errorf("by its own: %+v, want the one not excused", got)
	}
	if got := all[week][id]; got.Asleep != 1 {
		t.Errorf("by every rule's: %+v, want the one not excused", got)
	}
}
