package ledger

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
)

// excused are the starts, in loc, of the firings quiet windows excused.
func excused(t *testing.T, l *Ledger, from, to time.Time, loc *time.Location) []string {
	t.Helper()
	got, err := l.Firings(context.Background(), FiringQuery{From: from, To: to, Excused: true, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range got {
		out = append(out, f.Rule+" "+f.Start.In(loc).Format("01-02 15:04"))
	}
	slices.Sort(out)
	return out
}

// A window every month, on the first Saturday, or the 15th, or the last
// day, or by a cron expression, excuses the firings in each repeat, at its
// time of day across daylight saving, until its last day; its first repeat
// is a time it falls due, and each is shorter than the time between two.
func TestWindowByMonthAndCron(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip(err)
	}
	ctx := context.Background()
	at := func(s string) time.Time {
		t.Helper()
		v, err := time.ParseInLocation("2006-01-02 15:04", s, berlin)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	var fs []firing.Firing
	// Every day of five months at 02:30 and 06:00, across the clocks going
	// forward on 2026-03-29.
	for d := at("2026-01-01 00:00"); d.Before(at("2026-05-31 00:00")); d = d.AddDate(0, 0, 1) {
		y, m, dd := d.Date()
		fs = append(fs, firing.Firing{Rule: "Backup", Start: time.Date(y, m, dd, 2, 30, 0, 0, berlin), End: time.Date(y, m, dd, 2, 40, 0, 0, berlin)},
			firing.Firing{Rule: "Batch", Start: time.Date(y, m, dd, 6, 0, 0, 0, berlin), End: time.Date(y, m, dd, 6, 5, 0, 0, berlin)})
	}
	for _, c := range []struct {
		name string
		w    Window
		want []string
	}{
		{"the first Saturday", Window{Title: "maintenance", Start: at("2026-01-03 02:00"), End: at("2026-01-03 03:00"), Every: "month", On: "first sat",
			Rules: "^Backup$"}, []string{"Backup 01-03 02:30", "Backup 02-07 02:30", "Backup 03-07 02:30", "Backup 04-04 02:30", "Backup 05-02 02:30"}},
		{"the 15th, until April", Window{Title: "batch", Start: at("2026-01-15 05:00"), End: at("2026-01-15 07:00"), Every: "month", On: "15",
			Until: at("2026-04-01 00:00")}, []string{"Batch 01-15 06:00", "Batch 02-15 06:00", "Batch 03-15 06:00"}},
		{"the start's day", Window{Title: "batch", Start: at("2026-01-15 05:00"), End: at("2026-01-15 07:00"), Every: "month", Rules: "Batch",
			Until: at("2026-02-20 00:00")}, []string{"Batch 01-15 06:00", "Batch 02-15 06:00"}},
		{"the last day", Window{Title: "close", Start: at("2026-01-31 02:00"), End: at("2026-01-31 03:00"), Every: "month", On: "last"},
			[]string{"Backup 01-31 02:30", "Backup 02-28 02:30", "Backup 03-31 02:30", "Backup 04-30 02:30"}},
		{"cron, on Sundays in March", Window{Title: "cron", Start: at("2026-03-01 02:00"), End: at("2026-03-01 02:45"), Every: "cron", On: "0 2 * 3 sun"},
			// 2026-03-29's 02:00 is skipped: the clocks go to 03:00, and its
			// repeat starts then, so the firing at 03:30 is in it.
			[]string{"Backup 03-01 02:30", "Backup 03-08 02:30", "Backup 03-15 02:30", "Backup 03-22 02:30", "Backup 03-29 03:30"}},
	} {
		l := open(t, berlin)
		record(t, l, at("2026-01-01 00:00"), at("2026-05-31 00:00"), fs)
		id, err := l.PutWindow(ctx, c.w, "ana", opts)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if err := l.JudgePending(ctx, opts); err != nil {
			t.Fatal(err)
		}
		if got := excused(t, l, at("2026-01-01 00:00"), at("2026-06-01 00:00"), berlin); !slices.Equal(got, c.want) {
			t.Errorf("%s: excused %q, want %q", c.name, got, c.want)
		}
		w, err := l.WindowByID(ctx, id)
		if err != nil || w.Every != c.w.Every || w.On == "" {
			t.Errorf("%s: kept as %+v, %v", c.name, w, err)
		}
		// Taken away, they count again.
		if err := l.DeleteWindow(ctx, id, "ana", opts); err != nil {
			t.Fatal(err)
		}
		if err := l.JudgePending(ctx, opts); err != nil {
			t.Fatal(err)
		}
		if got := excused(t, l, at("2026-01-01 00:00"), at("2026-06-01 00:00"), berlin); len(got) != 0 {
			t.Errorf("%s: deleted, still excused %q", c.name, got)
		}
		// Kept before the runs record its repeats, it excuses the same as
		// they come.
		ahead := open(t, berlin)
		if _, err := ahead.PutWindow(ctx, c.w, "ana", opts); err != nil {
			t.Fatal(err)
		}
		record(t, ahead, at("2026-01-01 00:00"), at("2026-05-31 00:00"), fs)
		if got := excused(t, ahead, at("2026-01-01 00:00"), at("2026-06-01 00:00"), berlin); !slices.Equal(got, c.want) {
			t.Errorf("%s, kept ahead: excused %q, want %q", c.name, got, c.want)
		}
	}

	l := open(t, berlin)
	for _, c := range []struct {
		w    Window
		says string
	}{
		{Window{Title: "x", Start: at("2027-01-03 02:00"), End: at("2027-01-03 03:00"), Every: "month", On: "first sat"}, "the next is 2027-02-06 02:00"},
		{Window{Title: "x", Start: at("2027-01-30 02:00"), End: at("2027-01-30 03:00"), Every: "month"}, "the 29th to the 31st"},
		{Window{Title: "x", Start: at("2027-01-02 02:00"), End: at("2027-01-31 03:00"), Every: "month", On: "first sat"}, "shorter than how often"},
		{Window{Title: "x", Start: at("2027-01-02 00:00"), End: at("2027-01-02 05:30"), Every: "cron", On: "0 */6 * * *"}, "shorter than how often"}, // 5h, the clocks going forward
		{Window{Title: "x", Start: at("2027-01-02 02:00"), End: at("2027-01-02 03:00"), Every: "cron", On: "daily@02:00"}, "five cron fields"},
		{Window{Title: "x", Start: at("2027-01-02 02:00"), End: at("2027-01-02 03:00"), Every: "cron", On: "*/5 * * * *"}, "hourly at most"},
		{Window{Title: "x", Start: at("2027-01-02 02:00"), End: at("2027-01-02 03:00"), Every: "month", On: "fifth sat"}, "first, second"},
		{Window{Title: "x", Start: at("2027-01-02 02:00"), End: at("2027-01-02 03:00"), Every: "year"}, "every day, week or month"},
	} {
		if _, err := l.PutWindow(ctx, c.w, "ana", opts); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%+v: %v, want one saying %q", c.w, err, c.says)
		}
	}
	// A window every day keeps no schedule.
	id, err := l.PutWindow(ctx, Window{Title: "nightly", Start: at("2027-01-02 02:00"), End: at("2027-01-02 03:00"), Every: "day", On: "first sat"}, "ana", opts)
	if w, _ := l.WindowByID(ctx, id); err != nil || w.On != "" {
		t.Errorf("a daily window kept %q: %v", w.On, err)
	}
}

// A team's window excuses only the firings of rules its team owns: by a
// note, or the first team by name whose pattern or labels match. Who owns
// what changing judges it again; a team removed stops its windows.
func TestTeamWindow(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	t0 := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	search, err := l.PutTeam(ctx, Team{Name: "search", Labels: incident.Labels{"team": {"search"}}}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	pay, err := l.PutTeam(ctx, Team{Name: "payments", Rules: "^Pay"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	f := func(rule string, h int, labels map[string]string) firing.Firing {
		return firing.Firing{Rule: rule, Start: t0.Add(time.Duration(h) * time.Hour), End: t0.Add(time.Duration(h)*time.Hour + 10*time.Minute), Labels: labels}
	}
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{
		f("PayErrors", 1, nil), f("SearchSlow", 1, map[string]string{"team": "search"}), f("PayLatency", 2, map[string]string{"team": "search"}),
		f("Disk", 2, nil),
	})
	id, err := l.PutWindow(ctx, Window{Title: "deploy", Start: t0, End: t0.Add(3 * time.Hour), TeamID: pay}, "ana", opts)
	if err != nil {
		t.Fatal(err)
	}
	want := func(what string, w ...string) {
		t.Helper()
		if err := l.JudgePending(ctx, opts); err != nil {
			t.Fatal(err)
		}
		if got := excused(t, l, t0, t0.Add(24*time.Hour), time.UTC); !slices.Equal(got, w) {
			t.Errorf("%s: excused %q, want %q", what, got, w)
		}
	}
	// payments' pattern decides before search's labels: PayLatency, though
	// its alert has search's label, is payments'.
	want("payments' window", "PayErrors 11-02 01:00", "PayLatency 11-02 02:00")
	// A note gives PayLatency to search: payments' window no longer quiets it.
	if err := l.PutNote(ctx, Note{Rule: "PayLatency", TeamID: search}, "ana"); err != nil {
		t.Fatal(err)
	}
	want("a note moves PayLatency to search", "PayErrors 11-02 01:00")
	// payments' pattern changes: it owns no rule.
	if _, err := l.PutTeam(ctx, Team{ID: pay, Name: "payments", Rules: "^Billing"}, "ana"); err != nil {
		t.Fatal(err)
	}
	want("payments' pattern changed")
	if _, err := l.PutTeam(ctx, Team{ID: pay, Name: "payments", Rules: "^Pay"}, "ana"); err != nil {
		t.Fatal(err)
	}
	want("back", "PayErrors 11-02 01:00")
	// search's window: its labels, and the rule a note gives it.
	if _, err := l.PutWindow(ctx, Window{Title: "reindex", Start: t0, End: t0.Add(3 * time.Hour), TeamID: search}, "ana", opts); err != nil {
		t.Fatal(err)
	}
	want("search's too", "PayErrors 11-02 01:00", "PayLatency 11-02 02:00", "SearchSlow 11-02 01:00")
	// payments removed: its window quiets nothing, nor does a team made
	// later and given its id.
	if err := l.DeleteTeam(ctx, pay, "ana"); err != nil {
		t.Fatal(err)
	}
	want("payments removed", "PayLatency 11-02 02:00", "SearchSlow 11-02 01:00")
	w, err := l.WindowByID(ctx, id)
	if err != nil || w.TeamID != -pay {
		t.Errorf("payments' window kept team %d, want %d: %v", w.TeamID, -pay, err)
	}
	again, err := l.PutTeam(ctx, Team{Name: "zebra", Rules: "^PayErrors$"}, "ana")
	if err != nil || again != pay {
		t.Fatalf("a team made after has id %d, not %d: %v", again, pay, err)
	}
	want("a team made after, with its id", "PayLatency 11-02 02:00", "SearchSlow 11-02 01:00")
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// A team that owns by labels, and sorts first by name, doesn't take the
// alerts of a rule another team's pattern owns: the rule's name decides
// before any label, as the pages have it (a review found the other way
// round, each team's window quieting the other's rule).
func TestTeamWindowRuleOwnerFirst(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	t0 := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	pay, _ := l.PutTeam(ctx, Team{Name: "payments", Rules: "^Errors$"}, "ana")
	disc, _ := l.PutTeam(ctx, Team{Name: "discovery", Labels: incident.Labels{"service": {"search"}}}, "ana")
	var fs []firing.Firing
	for h := range 10 {
		at := t0.Add(time.Duration(h) * time.Hour)
		fs = append(fs, firing.Firing{Rule: "Errors", Start: at, End: at.Add(5 * time.Minute), Labels: map[string]string{"service": "search"}},
			firing.Firing{Rule: "Slow", Start: at.Add(time.Minute), End: at.Add(5 * time.Minute), Labels: map[string]string{"service": "search"}})
	}
	record(t, l, t0, t0.Add(24*time.Hour), fs)
	for team, want := range map[int64]int{pay: 10, disc: 10} {
		id, err := l.PutWindow(ctx, Window{Title: "w", Start: t0, End: t0.Add(12 * time.Hour), TeamID: team}, "ana", opts)
		if err != nil {
			t.Fatal(err)
		}
		got := excused(t, l, t0, t0.Add(24*time.Hour), time.UTC)
		rule := map[int64]string{pay: "Errors", disc: "Slow"}[team]
		n := 0
		for _, g := range got {
			if !strings.HasPrefix(g, rule+" ") {
				t.Errorf("team %d's window excused %s", team, g)
			}
			n++
		}
		if n != want {
			t.Errorf("team %d's window excused %d, want %d of %s", team, n, want, rule)
		}
		if err := l.DeleteWindow(ctx, id, "ana", opts); err != nil {
			t.Fatal(err)
		}
	}
}

// A team's editors change only their team's tasks, windows, reports and
// notes, before and after; the rest is an editor's.
func TestTeamScope(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	t0 := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	pay, _ := l.PutTeam(ctx, Team{Name: "payments", Rules: "^Pay"}, "ana")
	search, _ := l.PutTeam(ctx, Team{Name: "search", Rules: "^Search"}, "ana")
	mine := WithTeams(ctx, []int64{pay})
	no := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, ErrNotYourTeam) {
			t.Errorf("%s: %v, want ErrNotYourTeam", what, err)
		}
	}
	yes := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
	// Tasks.
	theirs, _ := l.PutTask(ctx, Task{Title: "t", TeamID: search}, "ana")
	everyone, _ := l.PutTask(ctx, Task{Title: "t"}, "ana")
	own, err := l.PutTask(mine, Task{Title: "t", TeamID: pay}, "bo")
	yes("a task of payments", err)
	_, err = l.PutTask(mine, Task{Title: "t", TeamID: search}, "bo")
	no("a task of search", err)
	_, err = l.PutTask(mine, Task{Title: "t"}, "bo")
	no("a task of no team", err)
	_, err = l.PutTask(mine, Task{ID: own, Title: "t", TeamID: search}, "bo")
	no("moving a task to search", err)
	_, err = l.PutTask(mine, Task{ID: theirs, Title: "t", TeamID: pay}, "bo")
	no("taking search's task", err)
	_, err = l.PatchTask(mine, everyone, []byte(`{"team_id": `+itoa(pay)+`}`), "bo")
	no("taking everyone's task by a patch", err)
	_, err = l.PatchTask(mine, own, []byte(`{"team_id": `+itoa(search)+`}`), "bo")
	no("giving a task away by a patch", err)
	_, err = l.PatchTask(mine, own, []byte(`{"status": "done"}`), "bo")
	yes("its own task done", err)
	no("deleting search's task", l.DeleteTask(mine, theirs, "bo"))
	if got, _ := l.TaskByID(ctx, theirs); got.TeamID != search {
		t.Errorf("search's task is %d's", got.TeamID)
	}
	yes("deleting its own", l.DeleteTask(mine, own, "bo"))
	// Windows.
	w := Window{Title: "deploy", Start: t0, End: t0.Add(time.Hour)}
	_, err = l.PutWindow(mine, w, "bo", opts)
	no("a window for everyone", err)
	w.TeamID = search
	_, err = l.PutWindow(mine, w, "bo", opts)
	no("a window of search", err)
	theirW, _ := l.PutWindow(ctx, w, "ana", opts)
	w.TeamID = pay
	ownW, err := l.PutWindow(mine, w, "bo", opts)
	yes("a window of payments", err)
	w.ID = theirW
	_, err = l.PutWindow(mine, w, "bo", opts)
	no("taking search's window", err)
	w.ID, w.TeamID = ownW, search
	_, err = l.PutWindow(mine, w, "bo", opts)
	no("giving its window away", err)
	no("deleting search's window", l.DeleteWindow(mine, theirW, "bo", opts))
	yes("deleting its own", l.DeleteWindow(mine, ownW, "bo", opts))
	// Reports.
	r := Scheduled{Name: "pay daily", Schedule: "daily@09:00", Dests: []string{"team:payments"}, Enabled: true}
	_, err = l.PutScheduled(mine, r, "bo", t0)
	no("a report for everyone", err)
	r.TeamID = search
	theirR, _ := l.PutScheduled(ctx, r, "ana", t0)
	r.TeamID, r.Name = pay, "pay daily 2"
	ownR, err := l.PutScheduled(mine, r, "bo", t0)
	yes("a report of payments", err)
	r.ID = theirR
	_, err = l.PutScheduled(mine, r, "bo", t0)
	no("taking search's report", err)
	no("deleting search's report", l.DeleteScheduled(mine, theirR, "bo"))
	// One an admin sent elsewhere, to Slack, stays an admin's.
	r.ID, r.TeamID, r.Name, r.Dests = 0, pay, "pay to slack", []string{"slack"}
	slackR, err := l.PutScheduled(ctx, r, "ana", t0)
	yes("an admin's report of payments to Slack", err)
	r.ID, r.Dests = slackR, []string{"team:payments"}
	_, err = l.PutScheduled(mine, r, "bo", t0)
	no("taking an admin's report to Slack", err)
	no("deleting an admin's report to Slack", l.DeleteScheduled(mine, slackR, "bo"))
	r.ID, r.Name, r.Dests = 0, "pay to search", []string{"team:search"}
	_, err = l.PutScheduled(mine, r, "bo", t0)
	no("a report of payments to search's destination", err)
	yes("deleting its own", l.DeleteScheduled(mine, ownR, "bo"))
	// A team's editors make ten of its reports at most.
	r.Dests = []string{"team:payments"}
	for i := range MaxTeamScheduled + 1 {
		r.Name = "pay " + itoa(int64(i))
		_, err = l.PutScheduled(mine, r, "bo", t0)
		if i < MaxTeamScheduled-1 && err != nil || i == MaxTeamScheduled && (err == nil || !strings.Contains(err.Error(), "at most")) {
			t.Errorf("report %d of payments: %v", i, err)
		}
	}
	// Notes keep their team.
	yes("a note on its rule", l.PutNote(mine, Note{Rule: "PayErrors", Status: "tune"}, "bo"))
	no("a note giving its rule to search", l.PutNote(mine, Note{Rule: "PayErrors", TeamID: search}, "bo"))
	yes("search's rule given to it by an editor", l.PutNote(ctx, Note{Rule: "SearchSlow", TeamID: search, Note: "x"}, "ana"))
	no("a note taking search's rule", l.PutNote(mine, Note{Rule: "SearchSlow", TeamID: pay}, "bo"))
	no("a note taking search's note away", l.PutNote(mine, Note{Rule: "SearchSlow"}, "bo"))
	// Roles are an admin's.
	_, err = l.PutGrant(mine, Grant{Who: "user:bo", TeamID: pay}, "bo")
	no("a role", err)
	// With no teams, nothing.
	_, err = l.PutTask(WithTeams(ctx, nil), Task{Title: "t", TeamID: pay}, "bo")
	no("no teams", err)
}

// Roles in the ledger are an editor's, of every team or of one, given to
// user:NAME or group:NAME: never admin or approver, everyone, or a name
// Lapilli keeps. A team removed takes its editors with it.
func TestGrants(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	pay, _ := l.PutTeam(ctx, Team{Name: "payments", Rules: "^Pay"}, "ana")
	for _, c := range []struct {
		g    Grant
		says string
	}{
		{Grant{Who: "user:bo", Role: "admin"}, "admins and approvers are named by the flags"},
		{Grant{Who: "user:bo", Role: "approver"}, "admins and approvers"},
		{Grant{Who: "bo"}, "user:NAME or group:NAME"},
		{Grant{Who: "role:bo"}, "user:NAME or group:NAME"},
		{Grant{Who: "user:"}, "user:NAME or group:NAME"},
		{Grant{Who: "user:*"}, "by the flags only"},
		{Grant{Who: "group:*"}, "by the flags only"},
		{Grant{Who: "user:anonymous"}, "keeps for itself"},
		{Grant{Who: "user:Lapilli"}, "keeps for itself"},
		{Grant{Who: "user:incidents-token"}, "keeps for itself"},
		{Grant{Who: "group:a,b"}, "no comma"},
		{Grant{Who: "user:bo\nX-Forwarded-Email: admin"}, "one line"},
		{Grant{Who: "user:" + strings.Repeat("x", 201)}, "200 characters"},
		{Grant{Who: "user:bo", TeamID: 99}, "no such team"},
		{Grant{Who: "user:bo", TeamID: -pay}, "no such team"},
	} {
		if _, err := l.PutGrant(ctx, c.g, "ana"); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%+v: %v, want one saying %q", c.g, err, c.says)
		}
	}
	if _, err := l.PutGrant(ctx, Grant{Who: " user:bo "}, "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutGrant(ctx, Grant{Who: "user:bo"}, "ana"); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("given twice: %v", err)
	}
	if _, err := l.PutGrant(ctx, Grant{Who: "group:pay-oncall", TeamID: pay}, "ana"); err != nil {
		t.Fatal(err)
	}
	// The table itself refuses another role.
	for _, row := range []string{"('user:x', 'admin')", "('everyone', 'editor')", "('user:', 'editor')", "('*', 'editor')"} {
		if _, err := l.w.ExecContext(ctx, "INSERT INTO grants (who, role, team_id, created) VALUES "+row[:len(row)-1]+", 0, 0)"); err == nil {
			t.Errorf("the table kept %s", row)
		}
	}
	gs, _ := l.Grants(ctx)
	if len(gs) != 2 || gs[0].Who != "group:pay-oncall" || gs[0].TeamID != pay || gs[1].Who != "user:bo" || gs[1].Role != "editor" || gs[1].By != "ana" {
		t.Fatalf("grants %+v", gs)
	}
	if err := l.DeleteTeam(ctx, pay, "ana"); err != nil {
		t.Fatal(err)
	}
	again, _ := l.PutTeam(ctx, Team{Name: "payments", Rules: "^Pay"}, "ana")
	if gs, _ = l.Grants(ctx); len(gs) != 1 || gs[0].Who != "user:bo" || again != pay {
		t.Fatalf("after payments was removed and made again (%d, was %d): %+v", again, pay, gs)
	}
	if err := l.DeleteGrant(ctx, gs[0].ID, "ana"); err != nil {
		t.Fatal(err)
	}
	if err := l.DeleteGrant(ctx, gs[0].ID, "ana"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted twice: %v", err)
	}
}

// Editors, limited to teams, can't fill the ledger's reports: the last ten
// are an admin's to make (a review filled all fifty, ten a team).
func TestReportsKeptForAdmins(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	var teams []int64
	for i := range 5 {
		id, err := l.PutTeam(ctx, Team{Name: "t" + itoa(int64(i)), Rules: "^T" + itoa(int64(i))}, "boss")
		if err != nil {
			t.Fatal(err)
		}
		teams = append(teams, id)
	}
	ed := WithTeams(ctx, teams)
	made := 0
	for i, team := range teams {
		for k := range MaxTeamScheduled {
			_, err := l.PutScheduled(ed, Scheduled{Name: "r" + itoa(team) + "-" + itoa(int64(k)), TeamID: team, Schedule: "0 * * * *",
				Dests: []string{"team:t" + itoa(int64(i))}, Enabled: true}, "ed", time.Now())
			if err == nil {
				made++
			} else if !strings.Contains(err.Error(), "for admins") {
				t.Fatal(err)
			}
		}
	}
	if made != MaxScheduled-AdminScheduled {
		t.Errorf("editors made %d", made)
	}
	for k := range AdminScheduled {
		if _, err := l.PutScheduled(ctx, Scheduled{Name: "admins " + itoa(int64(k)), Schedule: "daily@09:00", Dests: []string{"slack"}, Enabled: true},
			"boss", time.Now()); err != nil {
			t.Errorf("an admin's report %d: %v", k, err)
		}
	}
}
