package serve

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/oncall"
	"lapilli.dev/internal/verdict"
)

// onCallFeed is a feed of one turn of Jane Doe's, whose hours are Seoul's,
// asleep 23:00-07:00, over 1 and 2 November (UTC).
const onCallFeed = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nDTSTART:20261101T000000Z\r\nDTEND:20261103T000000Z\r\n" +
	"ATTENDEE:jane@example.com\r\nSUMMARY:On Call - Jane Doe - Payments\r\nUID:a\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

// A team on call by a schedule's turns has its rules' firings counted by
// the hours of whoever's turn they came in: Jane, in Seoul, was at work at
// 06:00 UTC and asleep from 14:00 to 22:00 UTC; by the server's hours, in
// UTC, it is the other way round. The turns are kept in the ledger, and a
// feed that later looks back less, or fails, takes none away.
func TestOnCallTeam(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	feedFile := filepath.Join(dir, "payments.ics")
	os.WriteFile(feedFile, []byte(onCallFeed), 0o600)
	cfgFile := filepath.Join(dir, "on-call.yaml")
	os.WriteFile(cfgFile, []byte("schedules:\n  - {name: payments-oncall-team, file: "+feedFile+"}\n"+
		"people:\n  - {name: Jane Doe, also: [jane@example.com], tz: Asia/Seoul, sleep: '23:00-07:00'}\n"), 0o600)
	oc, err := oncall.Load(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := server(t, func(c *Config) {
		c.OnCall = oc
		c.Options.Hours = verdict.DefaultHours(time.UTC)
	})
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	asleep := func() int {
		t.Helper()
		o, err := s.options(ctx)
		if err != nil {
			t.Fatal(err)
		}
		rep, err := s.ledgerReport(ctx, now.Add(-7*24*time.Hour), now, o, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rep.Rules {
			if r.Rule == "Errors" {
				return r.Asleep
			}
		}
		t.Fatal("no Errors")
		return 0
	}
	if n := asleep(); n != 9 {
		t.Fatalf("by the server's hours: %d asleep, want 9", n)
	}
	// A team on call by the schedule: refused before it is known, then
	// kept.
	h := s.Handler()
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "payments", Rules: "^Errors$", Shifts: "on-call=payments-oncall-team"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if err := s.checkShifts("on-call=nowhere"); err == nil {
		t.Error("shifts naming a schedule --on-call doesn't were taken")
	}
	s.readFeeds(ctx)
	turns, err := s.l.Turns(ctx, "payments-oncall-team")
	if err != nil || len(turns) != 1 || turns[0].Who != "Jane Doe" {
		t.Fatalf("turns kept: %+v %v", turns, err)
	}
	if n := asleep(); n != 8 {
		t.Errorf("by Jane's hours: %d asleep, want 8", n)
	}
	// The week's hours kept for the pages are counted again once the turns
	// change: 2 November, 00:00 to 06:00 UTC, is Jane's day in Seoul, and
	// asleep by the server's hours, someone else's turn.
	weekAsleep := func() int {
		t.Helper()
		own, err := s.owners(ctx)
		if err != nil {
			t.Fatal(err)
		}
		hh := own.schedules(s.cfg.Options.Hours).of("Errors")
		by, err := s.weekHours(ctx, s.l.Day(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)), ledger.HoursKey(hh), hh, nil)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, x := range by {
			n += x.Asleep
		}
		return n
	}
	if n := weekAsleep(); n != 0 {
		t.Errorf("the week by Jane's hours: %d asleep, want 0", n)
	}
	os.WriteFile(feedFile, []byte(strings.ReplaceAll(onCallFeed, "jane@example.com", "someone@example.com")), 0o600)
	s.readFeeds(ctx)
	if n := weekAsleep(); n != 7 {
		t.Errorf("the week by someone's turn, the server's hours: %d asleep, want 7", n)
	}
	os.WriteFile(feedFile, []byte(onCallFeed), 0o600)
	s.readFeeds(ctx)
	_, body, _ := get(t, h, "/runs")
	if !strings.Contains(body, "payments-oncall-team") || !strings.Contains(body, "On-call schedules") {
		t.Errorf("the runs page doesn't show the schedule")
	}
	_, body, _ = get(t, h, "/teams")
	if !strings.Contains(body, "on call by the schedule payments-oncall-team") {
		t.Errorf("the teams page doesn't say the team's hours")
	}
	// The feed now looks back less, then fails: the turns kept stay.
	os.WriteFile(feedFile, []byte(strings.ReplaceAll(onCallFeed, "20261101T000000Z", "20261102T120000Z")), 0o600)
	s.readFeeds(ctx)
	if turns, _ := s.l.Turns(ctx, "payments-oncall-team"); len(turns) != 2 || !turns[0].From.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) ||
		!turns[0].To.Equal(time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("turns after a shorter feed: %+v", turns)
	}
	os.WriteFile(feedFile, []byte("<html>sign in</html>"), 0o600)
	s.readFeeds(ctx)
	if turns, _ := s.l.Turns(ctx, "payments-oncall-team"); len(turns) != 2 {
		t.Errorf("a failed read took turns away: %+v", turns)
	}
	if n := asleep(); n != 8 {
		t.Errorf("after a failed read: %d asleep, want 8", n)
	}
	_, body, _ = get(t, h, "/runs")
	if !strings.Contains(body, "The last read failed") {
		t.Errorf("the runs page doesn't say the read failed")
	}
	// Teams through the API: a schedule --on-call doesn't name is refused.
	code, body, _ := ui(t, h, http.MethodPost, "/teams", "", "same-origin", map[string][]string{"name": {"x"}, "shifts": {"on-call=nowhere"}})
	if code != http.StatusBadRequest || !strings.Contains(body, "no schedule") {
		t.Errorf("a team on call by a schedule --on-call doesn't name: %d %s", code, zzMsg(body))
	}
}
