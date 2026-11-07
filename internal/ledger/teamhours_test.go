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
