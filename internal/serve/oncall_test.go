package serve

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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

// What the reviews found of serve's on-call turns, each a test: a person
// added to the file later, by a name a feed's turns had (an attendee's
// email), is found in the turns kept before (M11); a turn removed at the
// head of a feed (Grafana OnCall keeps it, cancelled) is removed from those
// kept (L5); an event from long ago, or one running from then, doesn't take
// the turns kept before the span read away (L3); a name the ledger won't
// keep passes that event over, not the read (L2).
func TestOnCallKept(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	feedFile := filepath.Join(dir, "storage.ics")
	cfgFile := filepath.Join(dir, "on-call.yaml")
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	load := func(people string) *oncall.Config {
		t.Helper()
		write(cfgFile, "schedules:\n  - {name: storage-oncall-kept, file: "+feedFile+"}\npeople:\n"+people)
		c, err := oncall.Load(cfgFile)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	event := func(from, to, extra string) string {
		return "BEGIN:VEVENT\r\nDTSTART:" + from + "\r\nDTEND:" + to + "\r\n" + extra + "END:VEVENT\r\n"
	}
	cal := func(events ...string) string {
		return "BEGIN:VCALENDAR\r\n" + strings.Join(events, "") + "END:VCALENDAR\r\n"
	}
	s, _ := server(t, func(c *Config) { c.OnCall = load("  - {name: Sam Lee, tz: Asia/Seoul}\n") })
	s.now = func() time.Time { return now }
	turns := func() []verdict.RotaTurn {
		t.Helper()
		ts, err := s.l.Turns(ctx, "storage-oncall-kept")
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}

	// Alice's turns by her email, before she is in the file.
	write(feedFile, cal(
		event("20261101T000000Z", "20261102T000000Z", "ATTENDEE:mailto:alice@example.com\r\nSUMMARY:On Call\r\n"),
		event("20261102T000000Z", "20261103T000000Z", "SUMMARY:Sam Lee\r\n")))
	s.readFeeds(ctx)
	if ts := turns(); len(ts) != 2 || ts[0].Who != "alice@example.com" {
		t.Fatalf("turns: %+v", ts)
	}
	key := verdict.OnCallNamed("storage-oncall-kept").Rota()
	if key == nil || key.Known("Alice Doe") {
		t.Fatal("Alice known before she is in the file")
	}
	// She is added, under also: the turns kept are hers.
	s.cfg.OnCall = load("  - {name: Sam Lee, tz: Asia/Seoul}\n  - {name: Alice Doe, also: [alice@example.com], tz: Europe/Berlin}\n")
	if err := s.onCallKept(ctx); err != nil {
		t.Fatal(err)
	}
	r := verdict.OnCallNamed("storage-oncall-kept").Rota()
	if r.Turns()[0].Who != "Alice Doe" || !r.Known("Alice Doe") {
		t.Errorf("Alice's turns kept before she was in the file: %+v", r.Turns())
	}

	// Her turn removed at the head of the feed, cancelled as Grafana
	// OnCall keeps it.
	write(feedFile, cal(
		event("20261101T000000Z", "20261101T000000Z", "STATUS:CANCELLED\r\nSUMMARY:alice@example.com\r\n"),
		event("20261102T000000Z", "20261103T000000Z", "SUMMARY:Sam Lee\r\n")))
	s.readFeeds(ctx)
	if ts := turns(); len(ts) != 1 || ts[0].Who != "Sam Lee" {
		t.Errorf("a turn removed at the head of the feed stays: %+v", ts)
	}

	// Long ago: a turn kept from then stays, though the feed now has an
	// event from 1970, and one running from 1970 into the span.
	old := time.Date(2025, 8, 1, 0, 0, 0, 0, time.UTC) // before now less 400 days
	if _, err := s.l.PutTurns(ctx, "storage-oncall-kept", old, []verdict.RotaTurn{{From: old, To: old.Add(24 * time.Hour), Who: "Sam Lee"}}); err != nil {
		t.Fatal(err)
	}
	write(feedFile, cal(
		event("19700101T000000Z", "19700102T000000Z", "SUMMARY:Sam Lee\r\n"),
		event("19700101T000000Z", "20261101T000000Z", "SUMMARY:Sam Lee\r\n"),
		event("20261102T000000Z", "20261103T000000Z", "SUMMARY:Sam Lee\r\n"),
		event("20261103T000000Z", "20261104T000000Z", "SUMMARY:Ja\u200dne\r\n")))
	s.readFeeds(ctx)
	ts := turns()
	if len(ts) == 0 || !ts[0].From.Equal(old) {
		t.Errorf("the turn kept from long ago was taken away: %+v", ts)
	}
	if st := s.onCallRead("storage-oncall-kept"); st.Err != "" {
		t.Errorf("a name that hides failed the read: %s", st.Err)
	}
}

// What the verification found of serve's on-call turns: an event passed
// over (here for its 1,001st EXDATE) keeps the turns kept of it, where
// the feed's other turns aren't, rather than taking them away (F3); a
// summary longer than the ledger keeps names is cut to fit, not a failed
// read (F2).
func TestOnCallHeld(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	feedFile := filepath.Join(dir, "payments.ics")
	cfgFile := filepath.Join(dir, "on-call.yaml")
	if err := os.WriteFile(cfgFile, []byte("schedules:\n  - {name: payments-oncall-held, file: "+feedFile+"}\npeople:\n  - {name: Sam Lee, tz: Asia/Seoul}\n  - {name: Ali Kaya, tz: Europe/Istanbul}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oc, err := oncall.Load(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := server(t, func(c *Config) { c.OnCall = oc })
	s.now = func() time.Time { return now }
	put := func(events ...string) {
		t.Helper()
		if err := os.WriteFile(feedFile, []byte("BEGIN:VCALENDAR\r\n"+strings.Join(events, "")+"END:VCALENDAR\r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	event := func(from, to, extra string) string {
		return "BEGIN:VEVENT\r\nDTSTART:" + from + "\r\nDTEND:" + to + "\r\n" + extra + "END:VEVENT\r\n"
	}
	turns := func() []string {
		t.Helper()
		ts, err := s.l.Turns(ctx, "payments-oncall-held")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, x := range ts {
			out = append(out, x.From.UTC().Format("01-02T15")+" "+x.Who)
		}
		return out
	}
	ali := event("20261025T000000Z", "20261025T010000Z", "UID:z\r\nRRULE:FREQ=DAILY;COUNT=10\r\nSUMMARY:Ali Kaya\r\n")
	put(ali)
	s.readFeeds(ctx)
	if got := turns(); len(got) != 10 {
		t.Fatalf("Ali's turns: %v", got)
	}
	var ex strings.Builder
	for i := range 1001 {
		fmt.Fprintf(&ex, "EXDATE:%sT000000Z\r\n", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format("20060102"))
	}
	put(event("20261025T000000Z", "20261025T010000Z", "UID:z\r\nRRULE:FREQ=DAILY;COUNT=10\r\n"+ex.String()+"SUMMARY:Ali Kaya\r\n"),
		event("20261101T000000Z", "20261101T020000Z", "SUMMARY:Sam Lee\r\n"))
	s.readFeeds(ctx)
	want := []string{"10-25T00 Ali Kaya", "10-26T00 Ali Kaya", "10-27T00 Ali Kaya", "10-28T00 Ali Kaya", "10-29T00 Ali Kaya", "10-30T00 Ali Kaya",
		"10-31T00 Ali Kaya", "11-01T00 Sam Lee", "11-02T00 Ali Kaya", "11-03T00 Ali Kaya"}
	st := s.onCallRead("payments-oncall-held")
	if got := turns(); !slices.Equal(got, want) || st.Err != "" || !strings.Contains(strings.Join(st.Notes, "\n"), "more than 1000") {
		t.Errorf("an event passed over: turns %v, want %v; err %q, notes %q", got, want, st.Err, st.Notes)
	}

	// A name past what the ledger keeps, beside another turn: cut, read.
	put(ali, event("20261105T000000Z", "20261106T000000Z", "SUMMARY:"+strings.Repeat("y", 230)+"\r\n"))
	s.readFeeds(ctx)
	st = s.onCallRead("payments-oncall-held")
	got := turns()
	if st.Err != "" || len(got) != 11 || !strings.HasSuffix(got[10], "…") || len(strings.TrimPrefix(got[10], "11-05T00 ")) > ledger.MaxWho {
		t.Errorf("a long name: err %q, turns %v", st.Err, got)
	}
}
