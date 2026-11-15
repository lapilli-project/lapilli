package ledger

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/incident"
)

// declaredOf is a config of a team (payments), a quiet window of its, a
// report of its and a role, as --config declares them.
func declaredOf() Declared {
	start := time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC)
	return Declared{
		Teams: []DeclaredTeam{{Team: Team{Name: "payments", Rules: "^Checkout", Labels: incident.Labels{"team": {"payments"}}}, Line: 2}},
		Windows: []DeclaredWindow{{Window: Window{Title: "nightly backup", Start: start, End: start.Add(time.Hour), Every: "day", Rules: "^Backup"},
			Team: "payments", Line: 8}},
		Reports: []DeclaredReport{{Scheduled: Scheduled{Name: "payments weekly", Schedule: "weekly mon@09:00", Dests: []string{"team:payments"}, Enabled: true},
			Team: "payments", Line: 14}},
		Grants: []DeclaredGrant{{Who: "group:payments-oncall", Team: "payments", Line: 20}, {Who: "user:ana@example.com", Line: 22}},
	}
}

// changesBy are the activity log's lines by who.
func changesBy(t *testing.T, l *Ledger, by string) []string {
	t.Helper()
	cs, err := l.Changes(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range cs {
		if c.By == by {
			out = append(out, c.What)
		}
	}
	return out
}

// What --config declares is made in one write, marked declared, each noted
// as the config's in the activity log; the same file again writes
// nothing.
func TestReconcileMakes(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	r, err := l.Reconcile(ctx, declaredOf(), false, opts)
	if err != nil {
		t.Fatal(err)
	}
	if r != (Reconciled{Made: 5}) {
		t.Errorf("reconciled: %+v, want 5 made", r)
	}
	ts, _ := l.Teams(ctx)
	if len(ts) != 1 || !ts[0].Declared || ts[0].By != ConfigBy {
		t.Fatalf("teams: %+v", ts)
	}
	ws, _ := l.windows(ctx, l.r, "")
	if len(ws) != 1 || !ws[0].Declared || ws[0].TeamID != ts[0].ID {
		t.Errorf("windows: %+v", ws)
	}
	rs, _ := l.Scheduleds(ctx)
	if len(rs) != 1 || !rs[0].Declared || rs[0].TeamID != ts[0].ID || rs[0].Zone != "ledger" {
		t.Errorf("reports: %+v", rs)
	}
	gs, _ := l.Grants(ctx)
	if len(gs) != 2 || !gs[0].Declared || !gs[1].Declared {
		t.Errorf("grants: %+v", gs)
	}
	logged := changesBy(t, l, ConfigBy)
	if len(logged) != 5 {
		t.Errorf("the activity log: %q", logged)
	}
	r, err = l.Reconcile(ctx, declaredOf(), false, opts)
	if err != nil || r.Any() {
		t.Errorf("the same file again: %+v %v", r, err)
	}
	if again := changesBy(t, l, ConfigBy); len(again) != len(logged) {
		t.Errorf("the same file again wrote %q", again[:len(again)-len(logged)])
	}
}

// What --config declares, the UI and the API change and delete none of:
// every write to it is refused in the ledger, whoever makes it; and a team
// it names isn't deleted.
func TestDeclaredRefused(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	// A team of the UI's, which a declared role names.
	ui, err := l.PutTeam(ctx, Team{Name: "search", Rules: "^Search"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	d := declaredOf()
	d.Grants = append(d.Grants, DeclaredGrant{Who: "user:bo@example.com", Team: "search", Line: 24})
	if _, err := l.Reconcile(ctx, d, false, opts); err != nil {
		t.Fatal(err)
	}
	ts, _ := l.Teams(ctx)
	var pay Team
	for _, x := range ts {
		if x.Name == "payments" {
			pay = x
		}
	}
	ws, _ := l.windows(ctx, l.r, "")
	rs, _ := l.Scheduleds(ctx)
	gs, _ := l.Grants(ctx)
	w := ws[0]
	w.Title = "changed"
	r := rs[0]
	r.Enabled = false
	for what, err := range map[string]error{
		"team changed": func() error {
			_, err := l.PutTeam(ctx, Team{ID: pay.ID, Name: "payments", Rules: "^Pay"}, "ana")
			return err
		}(),
		"team deleted":   l.DeleteTeam(ctx, pay.ID, "ana"),
		"window changed": func() error { _, err := l.PutWindow(ctx, w, "ana", opts); return err }(),
		"window deleted": l.DeleteWindow(ctx, w.ID, "ana", opts),
		"report changed": func() error { _, err := l.PutScheduled(ctx, r, "ana", time.Now()); return err }(),
		"report deleted": l.DeleteScheduled(ctx, r.ID, "ana"),
		"role taken":     l.DeleteGrant(ctx, gs[0].ID, "ana"),
	} {
		if !errors.Is(err, ErrDeclared) {
			t.Errorf("%s: %v, want ErrDeclared", what, err)
		}
	}
	if err := l.DeleteTeam(ctx, ui, "ana"); !errors.Is(err, ErrNamedByDeclared) {
		t.Errorf("a team a declared role names, deleted: %v", err)
	}
	// As they were.
	if again, _ := l.Teams(ctx); len(again) != 2 {
		t.Errorf("teams: %+v", again)
	}
	if again, _ := l.windows(ctx, l.r, ""); len(again) != 1 || again[0].Title != "nightly backup" {
		t.Errorf("windows: %+v", again)
	}
	if again, _ := l.Scheduleds(ctx); len(again) != 1 || !again[0].Enabled {
		t.Errorf("reports: %+v", again)
	}
	if again, _ := l.Grants(ctx); len(again) != 3 {
		t.Errorf("grants: %+v", again)
	}
}

// Taken out of the file, what was declared is the UI's again, as it was,
// and the UI may change it; with prune, it is deleted. Each is noted.
func TestReconcileRemoved(t *testing.T) {
	ctx := context.Background()
	for _, prune := range []bool{false, true} {
		l := open(t, time.UTC)
		if _, err := l.Reconcile(ctx, declaredOf(), prune, opts); err != nil {
			t.Fatal(err)
		}
		r, err := l.Reconcile(ctx, Declared{}, prune, opts)
		if err != nil {
			t.Fatal(err)
		}
		ts, _ := l.Teams(ctx)
		ws, _ := l.windows(ctx, l.r, "")
		rs, _ := l.Scheduleds(ctx)
		gs, _ := l.Grants(ctx)
		logged := strings.Join(changesBy(t, l, ConfigBy), "\n")
		if prune {
			if r != (Reconciled{Deleted: 5}) || len(ts)+len(ws)+len(rs)+len(gs) != 0 {
				t.Errorf("pruned: %+v; left %+v %+v %+v %+v", r, ts, ws, rs, gs)
			}
			for _, want := range []string{"team payments deleted", "quiet window nightly backup deleted", "report payments weekly deleted", RoleTaken + "group:payments-oncall"} {
				if !strings.Contains(logged, want) {
					t.Errorf("pruned: the activity log hasn't %q:\n%s", want, logged)
				}
			}
			continue
		}
		if r != (Reconciled{Undeclared: 5}) || len(ts) != 1 || len(ws) != 1 || len(rs) != 1 || len(gs) != 2 {
			t.Fatalf("taken out: %+v; left %+v %+v %+v %+v", r, ts, ws, rs, gs)
		}
		if ts[0].Declared || ws[0].Declared || rs[0].Declared || gs[0].Declared || gs[1].Declared || ws[0].TeamID != ts[0].ID {
			t.Errorf("still declared: %+v %+v %+v %+v", ts, ws, rs, gs)
		}
		if !strings.Contains(logged, "team payments"+undeclaredNow) || !strings.Contains(logged, "quiet window nightly backup"+undeclaredNow) {
			t.Errorf("taken out: the activity log:\n%s", logged)
		}
		// The UI's again: changed and deleted there.
		if _, err := l.PutTeam(ctx, Team{ID: ts[0].ID, Name: "payments", Rules: "^Pay"}, "ana"); err != nil {
			t.Errorf("the UI changing a team no longer declared: %v", err)
		}
		if err := l.DeleteWindow(ctx, ws[0].ID, "ana", opts); err != nil {
			t.Errorf("the UI deleting a window no longer declared: %v", err)
		}
	}
}

// A file with anything in it that can't be kept changes nothing: the
// ledger keeps what it held, and the error says the line and why, as the
// UI's checks say it.
func TestReconcileAllOrNothing(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if _, err := l.Reconcile(ctx, declaredOf(), false, opts); err != nil {
		t.Fatal(err)
	}
	before := len(changesBy(t, l, ConfigBy))
	for name, change := range map[string]func(*Declared){
		"a window ending before it starts": func(d *Declared) { d.Windows[0].End = d.Windows[0].Start.Add(-time.Hour) },
		"a team named twice": func(d *Declared) {
			d.Teams = append(d.Teams, DeclaredTeam{Team: Team{Name: "PAYMENTS", Rules: "^X"}, Line: 30})
		},
		"a window of no team": func(d *Declared) { d.Windows[0].Team = "nobody" },
		"a report's section":  func(d *Declared) { d.Reports[0].Sections = []string{"gossip"} },
		"a role to everyone":  func(d *Declared) { d.Grants[1].Who = "user:*" },
		"a team owning nothing, and a new team before it": func(d *Declared) {
			d.Teams = append([]DeclaredTeam{{Team: Team{Name: "new", Rules: "^New"}, Line: 1}}, d.Teams...)
			d.Teams[1].Rules, d.Teams[1].Labels = "", nil
		},
	} {
		d := declaredOf()
		d.Teams[0].Note = "changed" // a change the error undoes with the rest
		change(&d)
		_, err := l.Reconcile(ctx, d, true, opts)
		var de DeclaredError
		if !errors.As(err, &de) || de.Line == 0 || !invalidErr(err) {
			t.Errorf("%s: %v, want a DeclaredError of an ErrInvalid", name, err)
		}
		ts, _ := l.Teams(ctx)
		if len(ts) != 1 || ts[0].Note != "" {
			t.Errorf("%s: teams after: %+v", name, ts)
		}
		if after := len(changesBy(t, l, ConfigBy)); after != before {
			t.Errorf("%s: %d changes noted", name, after-before)
		}
	}
}

// What the UI made already, named in the file, is taken over: changed to
// what the file says, or, where it says the same, declared as it is. A
// team is matched in any letter case.
func TestReconcileAdopts(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	d := declaredOf()
	pay, err := l.PutTeam(ctx, Team{Name: "Payments", Rules: "^Old"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	w := d.Windows[0].Window
	w.TeamID = pay
	wid, err := l.PutWindow(ctx, w, "ana", opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutGrant(ctx, Grant{Who: "user:ana@example.com"}, "ana"); err != nil {
		t.Fatal(err)
	}
	r, err := l.Reconcile(ctx, d, false, opts)
	if err != nil {
		t.Fatal(err)
	}
	// The team renamed payments, with its pattern; the window and the
	// role as they were; the report and the other role made.
	if r != (Reconciled{Made: 2, Changed: 1, Adopted: 2}) {
		t.Errorf("reconciled: %+v", r)
	}
	ts, _ := l.Teams(ctx)
	if len(ts) != 1 || ts[0].ID != pay || ts[0].Name != "payments" || ts[0].Rules != "^Checkout" || !ts[0].Declared {
		t.Errorf("teams: %+v", ts)
	}
	ws, _ := l.windows(ctx, l.r, "")
	if len(ws) != 1 || ws[0].ID != wid || !ws[0].Declared || ws[0].By != "ana" {
		t.Errorf("windows: %+v", ws)
	}
}

// Quiet windows are matched by title: one taken out of a file holding two
// of one title leaves the other as it was, whichever was first.
func TestReconcileWindowsByTitle(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	start := time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC)
	d := Declared{
		Teams: []DeclaredTeam{{Team: Team{Name: "a", Rules: "^A"}, Line: 1}, {Team: Team{Name: "b", Rules: "^B"}, Line: 2}},
		Windows: []DeclaredWindow{
			{Window: Window{Title: "deploy", Start: start, End: start.Add(time.Hour)}, Team: "a", Line: 3},
			{Window: Window{Title: "deploy", Start: start, End: start.Add(2 * time.Hour)}, Team: "b", Line: 4},
		},
	}
	if _, err := l.Reconcile(ctx, d, false, opts); err != nil {
		t.Fatal(err)
	}
	ws, _ := l.windows(ctx, l.r, "ORDER BY id")
	d.Windows = d.Windows[1:]
	r, err := l.Reconcile(ctx, d, false, opts)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := l.windows(ctx, l.r, "ORDER BY id")
	if r != (Reconciled{Undeclared: 1}) || len(after) != 2 || after[0].Declared || !after[1].Declared || !sameWindow(after[1], ws[1]) {
		t.Errorf("one of two taken out: %+v; %+v", r, after)
	}
}
