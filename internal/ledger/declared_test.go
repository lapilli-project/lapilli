package ledger

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

// reconcile keeps l to d, as left says, and fails the test on an error of
// the ledger's.
func reconcile(t *testing.T, l *Ledger, d Declared, left Leave) (Reconciled, []DeclaredError) {
	t.Helper()
	r, skipped, err := l.Reconcile(context.Background(), d, left, opts)
	if err != nil {
		t.Fatal(err)
	}
	return r, skipped
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
	r, skipped := reconcile(t, l, declaredOf(), Leave{})
	if r != (Reconciled{Made: 5}) || len(skipped) != 0 {
		t.Errorf("reconciled: %+v %v, want 5 made", r, skipped)
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
	r, _ = reconcile(t, l, declaredOf(), Leave{})
	if r.Any() {
		t.Errorf("the same file again: %+v", r)
	}
	if again := changesBy(t, l, ConfigBy); len(again) != len(logged) {
		t.Errorf("the same file again wrote %q", again[:len(again)-len(logged)])
	}
}

// What --config declares, the UI and the API change and delete none of:
// every write to it is refused in the ledger, whoever makes it; and a team
// it names isn't deleted, nor renamed.
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
	reconcile(t, l, d, Leave{})
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
	if _, err := l.PutTeam(ctx, Team{ID: ui, Name: "searching", Rules: "^Search"}, "ana"); !errors.Is(err, ErrNamedByDeclared) {
		t.Errorf("a team a declared role names, renamed: %v", err)
	}
	// Changed but for its name, or in its name's letter case alone, which
	// the file's name still finds, it is the UI's to change.
	if _, err := l.PutTeam(ctx, Team{ID: ui, Name: "Search", Rules: "^Search", Note: "ours"}, "ana"); err != nil {
		t.Errorf("a team a declared role names, changed but for its name's case: %v", err)
	}
	if r, skipped := reconcile(t, l, d, Leave{}); r.Any() || len(skipped) != 0 {
		t.Errorf("the file read again after the case changed: %+v %v", r, skipped)
	}
	if _, err := l.PutTeam(ctx, Team{ID: ui, Name: "search", Rules: "^Search", Note: "ours"}, "ana"); err != nil {
		t.Errorf("back: %v", err)
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
// and the UI may change it, but a role: taken away, noted with whom it
// gave what. Pruned, the rest is deleted. With no file at all, all of it
// is the UI's again, roles included, and nothing deleted.
func TestReconcileRemoved(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		left Leave
		want Reconciled
	}{
		{"taken out", Leave{}, Reconciled{Undeclared: 3, Deleted: 2}},
		{"pruned", Leave{Prune: true}, Reconciled{Deleted: 5}},
		{"no file", Leave{NoFile: true}, Reconciled{Undeclared: 5}},
		{"no file, pruned", Leave{NoFile: true, Prune: true}, Reconciled{Undeclared: 5}},
	} {
		l := open(t, time.UTC)
		reconcile(t, l, declaredOf(), Leave{})
		r, _ := reconcile(t, l, Declared{}, c.left)
		ts, _ := l.Teams(ctx)
		ws, _ := l.windows(ctx, l.r, "")
		rs, _ := l.Scheduleds(ctx)
		gs, _ := l.Grants(ctx)
		logged := strings.Join(changesBy(t, l, ConfigBy), "\n")
		if r != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, r, c.want)
		}
		declared := 0
		for _, x := range ts {
			declared += b2i(x.Declared)
		}
		for _, x := range ws {
			declared += b2i(x.Declared)
		}
		for _, x := range rs {
			declared += b2i(x.Declared)
		}
		for _, x := range gs {
			declared += b2i(x.Declared)
		}
		if declared != c.want.Kept || len(ts)+len(ws)+len(rs)+len(gs) != 5-c.want.Deleted {
			t.Errorf("%s: %d declared of %d left", c.name, declared, len(ts)+len(ws)+len(rs)+len(gs))
		}
		if !c.left.NoFile {
			// Taken away, noted with what it was: whom to, of which team.
			cs, _ := l.Changes(ctx, 100)
			i := slices.IndexFunc(cs, func(x Change) bool { return x.What == RoleTaken+"group:payments-oncall" })
			if i < 0 || !strings.Contains(cs[i].Before, `"team_id":1`) {
				t.Errorf("%s: the role taken isn't noted with its team: %+v", c.name, cs)
			}
		} else if strings.Contains(logged, RoleTaken) {
			t.Errorf("%s: a role taken away:\n%s", c.name, logged)
		}
		if c.want.Undeclared == 3 {
			if _, err := l.PutTeam(ctx, Team{ID: ts[0].ID, Name: "payments", Rules: "^Pay"}, "ana"); err != nil {
				t.Errorf("the UI changing a team no longer declared: %v", err)
			}
		}
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Pruned, a declared team that what the UI made names (a quiet window, a
// role) isn't deleted: it is the UI's again, said why, and what names it
// keeps its team; the file put back takes it over, by its id. One nothing
// names is deleted. A team the UI deletes has each of its roles noted as
// taken.
func TestReconcilePruneKeepsWhatIsNamed(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	d := Declared{Teams: []DeclaredTeam{{Team: Team{Name: "payments", Rules: "^P"}, Line: 1}, {Team: Team{Name: "search", Rules: "^S"}, Line: 2}}}
	reconcile(t, l, d, Leave{Prune: true})
	ts, _ := l.Teams(ctx)
	pay := ts[0].ID
	start := time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC)
	wid, err := l.PutWindow(ctx, Window{Title: "deploy", Start: start, End: start.Add(time.Hour), TeamID: pay}, "ana", opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutGrant(ctx, Grant{Who: "user:pat", TeamID: pay}, "ana"); err != nil {
		t.Fatal(err)
	}
	r, _ := reconcile(t, l, Declared{Teams: []DeclaredTeam{{Team: Team{Name: "other", Rules: "^O"}, Line: 1}}}, Leave{Prune: true})
	if r != (Reconciled{Made: 1, Undeclared: 1, Deleted: 1}) {
		t.Errorf("pruned: %+v", r)
	}
	ts, _ = l.Teams(ctx)
	if !slices.ContainsFunc(ts, func(x Team) bool { return x.ID == pay && !x.Declared }) || slices.ContainsFunc(ts, func(x Team) bool { return x.Name == "search" }) {
		t.Errorf("teams after pruning: %+v", ts)
	}
	w, _ := l.WindowByID(ctx, wid)
	gs, _ := l.Grants(ctx)
	if w.TeamID != pay || len(gs) != 1 || gs[0].TeamID != pay {
		t.Errorf("what the UI made: %+v %+v", w, gs)
	}
	if logged := strings.Join(changesBy(t, l, ConfigBy), "\n"); !strings.Contains(logged, "team payments no longer declared in config, and not deleted") {
		t.Errorf("the activity log:\n%s", logged)
	}
	// Put back: payments taken over as it is, by its id.
	r, _ = reconcile(t, l, d, Leave{Prune: true})
	ts, _ = l.Teams(ctx)
	if r != (Reconciled{Made: 1, Adopted: 1, Deleted: 1}) || !slices.ContainsFunc(ts, func(x Team) bool { return x.ID == pay && x.Declared }) {
		t.Errorf("put back: %+v %+v", r, ts)
	}
	// The UI deleting a team notes each of its roles taken.
	ops, err := l.PutTeam(ctx, Team{Name: "ops", Rules: "^Ops"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutGrant(ctx, Grant{Who: "group:sre", TeamID: ops}, "ana"); err != nil {
		t.Fatal(err)
	}
	if err := l.DeleteTeam(ctx, ops, "ana"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(changesBy(t, l, "ana"), RoleTaken+"group:sre") {
		t.Errorf("a team deleted in the UI: its role taken isn't noted: %q", changesBy(t, l, "ana"))
	}
}

// One thing the file declares that can't be kept is skipped, and said,
// and what the ledger held of it kept as it was; the rest is kept, a role
// taken out of the file among it.
func TestReconcileSkipsWhatCantBeKept(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	reconcile(t, l, declaredOf(), Leave{Prune: true})
	d := declaredOf()
	d.Teams[0].Note = "changed"
	d.Windows[0].End = d.Windows[0].Start.Add(-time.Hour) // can't be kept
	d.Grants = d.Grants[:1]                               // ana's taken away
	d.Reports[0].Bad = errors.New("there is no destination called nowhere")
	d.Windows = append(d.Windows, DeclaredWindow{Window: Window{Title: "of nobody", Start: d.Windows[0].Start, End: d.Windows[0].Start.Add(time.Hour)}, Team: "nobody", Line: 30})
	r, skipped := reconcile(t, l, d, Leave{Prune: true})
	if r != (Reconciled{Changed: 1, Deleted: 1}) {
		t.Errorf("reconciled: %+v", r)
	}
	var lines []string
	for _, s := range skipped {
		lines = append(lines, fmt.Sprintf("%s %d", s.Kind, s.Line))
	}
	if strings.Join(lines, ", ") != "window 8, window 30, report 14" || !invalidErr(skipped[0]) {
		t.Errorf("skipped: %v (%v)", lines, skipped)
	}
	ws, _ := l.windows(ctx, l.r, "")
	rs, _ := l.Scheduleds(ctx)
	gs, _ := l.Grants(ctx)
	ts, _ := l.Teams(ctx)
	if len(ws) != 1 || !ws[0].Declared || !ws[0].End.After(ws[0].Start) || len(rs) != 1 || !rs[0].Declared || len(gs) != 1 || ts[0].Note != "changed" {
		t.Errorf("after: %+v %+v %+v %+v", ws, rs, gs, ts)
	}
}

// What the file declares has its own most: quiet windows the UI made up to
// the UI's most don't keep a file from declaring one, nor declared ones the
// UI from making one.
func TestDeclaredOwnLimits(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	start := time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC)
	err := l.write(ctx, func(t *txn) error {
		for i := range MaxWindows {
			if _, err := t.ExecContext(t.ctx, "INSERT INTO windows (title, start_at, end_at, updated) VALUES (?, ?, ?, 1)", fmt.Sprint("w", i), start.Unix(), start.Unix()+60); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutWindow(ctx, Window{Title: "one more", Start: start, End: start.Add(time.Hour)}, "ana", opts); !invalidErr(err) {
		t.Errorf("the UI past its most: %v", err)
	}
	r, skipped := reconcile(t, l, Declared{Windows: []DeclaredWindow{{Window: Window{Title: "declared", Start: start, End: start.Add(time.Hour)}, Line: 1}}}, Leave{})
	if r.Made != 1 || len(skipped) != 0 {
		t.Errorf("a file beside the UI's most: %+v %v", r, skipped)
	}
	// And the other way: as many declared as a file holds, and the UI
	// still makes one.
	l = open(t, time.UTC)
	err = l.write(ctx, func(t *txn) error {
		for i := range MaxWindows {
			if _, err := t.ExecContext(t.ctx, "INSERT INTO windows (title, start_at, end_at, updated, declared) VALUES (?, ?, ?, 1, 1)", fmt.Sprint("w", i), start.Unix(), start.Unix()+60); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutWindow(ctx, Window{Title: "the UI's", Start: start, End: start.Add(time.Hour)}, "ana", opts); err != nil {
		t.Errorf("the UI beside a file's most: %v", err)
	}
}

// What the UI made is taken over only of the same team: a declared quiet
// window of another team's title is made beside it, and a report of
// another team's name is skipped. Taken over, it is counted as such and
// noted "declared in config", changed or not.
func TestReconcileAdoptsOfItsTeam(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	pay, _ := l.PutTeam(ctx, Team{Name: "Payments", Rules: "^Old"}, "ana")
	search, _ := l.PutTeam(ctx, Team{Name: "search", Rules: "^S"}, "ana")
	d := declaredOf()
	w := d.Windows[0].Window
	w.TeamID = search
	theirs, err := l.PutWindow(ctx, w, "ana", opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutScheduled(ctx, Scheduled{Name: "payments weekly", TeamID: search, Schedule: "daily@09:00", Dests: []string{"team:search"}, Enabled: true}, "ana", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutGrant(ctx, Grant{Who: "user:ana@example.com"}, "ana"); err != nil {
		t.Fatal(err)
	}
	r, skipped := reconcile(t, l, d, Leave{})
	// payments taken over and renamed, ana's role taken over; the window
	// and the other role made; the report skipped.
	if r != (Reconciled{Made: 2, Adopted: 2}) || len(skipped) != 1 || skipped[0].Kind != KindReport {
		t.Errorf("reconciled: %+v %v", r, skipped)
	}
	ws, _ := l.windows(ctx, l.r, "ORDER BY id")
	if len(ws) != 2 || ws[0].ID != theirs || ws[0].Declared || ws[0].TeamID != search || !ws[1].Declared || ws[1].TeamID != pay {
		t.Errorf("windows: %+v", ws)
	}
	rs, _ := l.Scheduleds(ctx)
	if len(rs) != 1 || rs[0].Declared || rs[0].TeamID != search {
		t.Errorf("reports: %+v", rs)
	}
	logged := changesBy(t, l, ConfigBy)
	if !slices.Contains(logged, "team payments"+declaredNow) {
		t.Errorf("the activity log: %q", logged)
	}
	ts, _ := l.Teams(ctx)
	if ts[0].ID != pay || ts[0].Name != "payments" || ts[0].Rules != "^Checkout" || !ts[0].Declared {
		t.Errorf("teams: %+v", ts)
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
	reconcile(t, l, d, Leave{})
	ws, _ := l.windows(ctx, l.r, "ORDER BY id")
	d.Windows = d.Windows[1:]
	r, _ := reconcile(t, l, d, Leave{})
	after, _ := l.windows(ctx, l.r, "ORDER BY id")
	if r != (Reconciled{Undeclared: 1}) || len(after) != 2 || after[0].Declared || !after[1].Declared || !sameWindow(after[1], ws[1]) {
		t.Errorf("one of two taken out: %+v; %+v", r, after)
	}
	// Moved by the file to another team, a declared window is changed.
	d.Windows[0].Team = "a"
	r, _ = reconcile(t, l, d, Leave{})
	after, _ = l.windows(ctx, l.r, "ORDER BY id")
	if r != (Reconciled{Changed: 1}) || after[1].ID != ws[1].ID || after[1].TeamID != after[0].TeamID {
		t.Errorf("moved to another team: %+v; %+v", r, after)
	}
}

// A team the file renames (renamed_from) keeps its id, and what names it.
func TestReconcileRenamedFrom(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	reconcile(t, l, Declared{Teams: []DeclaredTeam{{Team: Team{Name: "payments", Rules: "^P"}, Line: 1}}}, Leave{Prune: true})
	ts, _ := l.Teams(ctx)
	id := ts[0].ID
	if _, err := l.PutTask(ctx, Task{Title: "t", TeamID: id}, "ana"); err != nil {
		t.Fatal(err)
	}
	r, _ := reconcile(t, l, Declared{Teams: []DeclaredTeam{{Team: Team{Name: "payments-eu", Rules: "^P"}, RenamedFrom: "payments", Line: 1}}}, Leave{Prune: true})
	ts, _ = l.Teams(ctx)
	if r != (Reconciled{Changed: 1}) || len(ts) != 1 || ts[0].ID != id || ts[0].Name != "payments-eu" || !ts[0].Declared {
		t.Errorf("renamed: %+v %+v", r, ts)
	}
}

// A quiet window's title is one line with nothing that doesn't show: an
// export writes it in a comment, where a line break would start YAML.
func TestWindowTitlePlain(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	start := time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC)
	for _, title := range []string{"x\ngrants:\n- {user: mallory}\n#", "x\ry", "x\u0085y", "x\u2028y", "x\u2029y", "deploy\u202e", "a\u200bb"} {
		if _, err := l.PutWindow(ctx, Window{Title: title, Start: start, End: start.Add(time.Hour)}, "ana", opts); !invalidErr(err) {
			t.Errorf("%q: %v", title, err)
		}
	}
	if _, err := l.PutWindow(ctx, Window{Title: "Deploy, 배포 🚀", Start: start, End: start.Add(time.Hour)}, "ana", opts); err != nil {
		t.Errorf("a plain title: %v", err)
	}
}

// Pruning the ledger keeps a declared quiet window that ended long ago:
// the file, read again, would make it again, and tell of it.
func TestPruneKeepsDeclaredWindows(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	old := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	d := Declared{Windows: []DeclaredWindow{{Window: Window{Title: "old freeze", Start: old, End: old.Add(24 * time.Hour)}, Line: 1}}}
	reconcile(t, l, d, Leave{})
	if _, err := l.PutWindow(ctx, Window{Title: "old ui", Start: old, End: old.Add(time.Hour)}, "ana", opts); err != nil {
		t.Fatal(err)
	}
	if err := l.Prune(ctx, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	ws, _ := l.windows(ctx, l.r, "")
	if len(ws) != 1 || ws[0].Title != "old freeze" {
		t.Errorf("after pruning: %+v", ws)
	}
	if r, _ := reconcile(t, l, d, Leave{}); r.Any() {
		t.Errorf("read again after pruning: %+v", r)
	}
}

// A role whose team is misspelled keeps none of the person's roles: one of
// every team the file gave before is taken away, not kept as it was. A
// quiet window of a team that isn't there keeps no declared window of
// its title, another team's or everyone's.
func TestReconcileUnfoundTeamKeepsNothing(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	start := time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC)
	v1 := Declared{
		Teams:   []DeclaredTeam{{Team: Team{Name: "payments", Rules: "^P"}, Line: 1}},
		Grants:  []DeclaredGrant{{Who: "user:eve", Line: 2}},
		Windows: []DeclaredWindow{{Window: Window{Title: "freeze", Start: start, End: start.Add(time.Hour)}, Line: 3}},
	}
	reconcile(t, l, v1, Leave{})
	v2 := v1
	v2.Grants = []DeclaredGrant{{Who: "user:eve", Team: "paymnets", Line: 2}}
	v2.Windows = []DeclaredWindow{{Window: v1.Windows[0].Window, Team: "nobody", Line: 3}}
	_, skipped := reconcile(t, l, v2, Leave{})
	if len(skipped) != 2 {
		t.Errorf("skipped: %v", skipped)
	}
	if gs, _ := l.Grants(ctx); len(gs) != 0 {
		t.Errorf("eve's roles: %+v", gs)
	}
	if ws, _ := l.windows(ctx, l.r, ""); len(ws) != 1 || ws[0].Declared {
		t.Errorf("windows: %+v", ws)
	}
	if !slices.Contains(changesBy(t, l, ConfigBy), RoleTaken+"user:eve") {
		t.Errorf("the activity log: %q", changesBy(t, l, ConfigBy))
	}
}

// renamed_from is read after every name, whatever order the file lists
// them in: an old name another declares by its name, given twice, or a
// team besides the one of the name is skipped, said why, and what it names
// kept as it was. A rename is noted as one.
func TestReconcileRenamedFromOrder(t *testing.T) {
	ctx := context.Background()
	team := func(name, from string, line int) DeclaredTeam {
		return DeclaredTeam{Team: Team{Name: name, Rules: "^X"}, RenamedFrom: from, Line: line}
	}
	start := func(t *testing.T, names ...string) *Ledger {
		l := open(t, time.UTC)
		var d Declared
		for i, n := range names {
			d.Teams = append(d.Teams, team(n, "", i+1))
		}
		reconcile(t, l, d, Leave{})
		return l
	}
	names := func(l *Ledger) string {
		ts, _ := l.Teams(ctx)
		var out []string
		for _, x := range ts {
			out = append(out, fmt.Sprintf("%s:%d:%v", x.Name, x.ID, x.Declared))
		}
		return strings.Join(out, " ")
	}
	for _, c := range []struct {
		name   string
		have   []string
		want   []DeclaredTeam
		result string
		skips  int
	}{
		// B declared by its name, and as C's old one: in either order, C
		// isn't made from it, and B stays.
		{"C from B, B", []string{"B"}, []DeclaredTeam{team("C", "B", 1), team("B", "", 2)}, "B:1:true", 1},
		{"B, C from B", []string{"B"}, []DeclaredTeam{team("B", "", 1), team("C", "B", 2)}, "B:1:true", 1},
		// A chain: B is there, by its name, and A another team.
		{"B from A, C from B", []string{"A", "B"}, []DeclaredTeam{team("B", "A", 1), team("C", "B", 2)}, "A:1:true B:2:true", 2},
		// One old name twice: neither renames it.
		{"C and D from B", []string{"B"}, []DeclaredTeam{team("C", "B", 1), team("D", "B", 2)}, "B:1:true", 2},
		// A rename, its old name trimmed.
		{"C from B", []string{"B"}, []DeclaredTeam{team("C", " B ", 1)}, "C:1:true", 0},
	} {
		l := start(t, c.have...)
		_, skipped := reconcile(t, l, Declared{Teams: c.want}, Leave{})
		if got := names(l); got != c.result || len(skipped) != c.skips {
			t.Errorf("%s: %s, %d skipped (%v); want %s, %d", c.name, got, len(skipped), skipped, c.result, c.skips)
		}
	}
	l := start(t, "B")
	reconcile(t, l, Declared{Teams: []DeclaredTeam{team("C", "B", 1)}}, Leave{})
	if !slices.Contains(changesBy(t, l, ConfigBy), "team B renamed to C") {
		t.Errorf("the activity log: %q", changesBy(t, l, ConfigBy))
	}
}

// Past the most the UI keeps, what a file no longer declares isn't given
// back to the UI, but deleted: declared and taken out again and again, it
// doesn't pile up past what the UI may make. The UI's own most holds for
// teams too, and the most of both kinds together for every kind.
func TestDeclaredLeftPastTheUIsMost(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	start := time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC)
	err := l.write(ctx, func(t *txn) error {
		for i := range MaxWindows {
			if _, err := t.ExecContext(t.ctx, "INSERT INTO windows (title, start_at, end_at, updated) VALUES (?, ?, ?, 1)", fmt.Sprint("w", i), start.Unix(), start.Unix()+60); err != nil {
				return err
			}
		}
		for i := range MaxTeams {
			if _, err := t.ExecContext(t.ctx, "INSERT INTO teams (name, rules, updated, name_key) VALUES (?, '^X', 1, ?)", fmt.Sprint("t", i), fmt.Sprint("t", i)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutTeam(ctx, Team{Name: "one more", Rules: "^O"}, "ana"); !invalidErr(err) {
		t.Errorf("a team past the UI's most: %v", err)
	}
	d := Declared{Teams: []DeclaredTeam{{Team: Team{Name: "declared", Rules: "^D"}, Line: 1}}}
	for range 3 {
		d.Windows = d.Windows[:0]
		for i := range 10 {
			d.Windows = append(d.Windows, DeclaredWindow{Window: Window{Title: fmt.Sprint("d", i), Start: start, End: start.Add(time.Hour)}, Line: i + 2})
		}
		if r, skipped := reconcile(t, l, d, Leave{}); r.Made < 10 || len(skipped) != 0 {
			t.Fatalf("declared: %+v %v", r, skipped)
		}
		if r, _ := reconcile(t, l, Declared{Teams: []DeclaredTeam{{Team: Team{Name: "other", Rules: "^O"}, Line: 1}}}, Leave{}); r.Undeclared != 0 {
			t.Errorf("given back past the UI's most: %+v", r)
		}
	}
	var ui, all int
	l.r.QueryRowContext(ctx, "SELECT count(*) FILTER (WHERE declared = 0), count(*) FROM windows").Scan(&ui, &all)
	if ui != MaxWindows || all != MaxWindows {
		t.Errorf("windows: %d the UI's, %d in all", ui, all)
	}
	if !slices.Contains(changesBy(t, l, ConfigBy), fmt.Sprintf("quiet window d0 deleted, as the file no longer declares it and the UI keeps %d at most", MaxWindows)) {
		t.Errorf("the activity log doesn't say why")
	}
}

// Past the UI's most, what the file no longer declares is deleted, said
// why; with no file at all, given back all the same.
func TestPastTheUIsMost(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	err := l.write(ctx, func(t *txn) error {
		for i := range MaxTeams {
			if _, err := t.ExecContext(t.ctx, "INSERT INTO teams (name, rules, updated, name_key) VALUES (?, '^X', 1, ?)", fmt.Sprint("t", i), fmt.Sprint("t", i)); err != nil {
				return err
			}
		}
		for i := range MaxScheduled {
			if _, err := t.ExecContext(t.ctx, "INSERT INTO reports (name, schedule, dests, created, updated) VALUES (?, 'daily@09:00', '[\"ops\"]', 1, 1)", fmt.Sprint("r", i)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	d := Declared{Teams: []DeclaredTeam{{Team: Team{Name: "declared", Rules: "^D"}, Line: 1}},
		Reports: []DeclaredReport{{Scheduled: Scheduled{Name: "declared daily", Schedule: "daily@09:00", Dests: []string{"ops"}, Enabled: true}, Line: 2}}}
	reconcile(t, l, d, Leave{})
	if r, _ := reconcile(t, l, Declared{}, Leave{}); r != (Reconciled{Deleted: 2}) {
		t.Errorf("taken out: %+v", r)
	}
	logged := changesBy(t, l, ConfigBy)
	for _, want := range []string{fmt.Sprintf("team declared deleted, as the file no longer declares it and the UI keeps %d at most", MaxTeams),
		fmt.Sprintf("report declared daily deleted, as the file no longer declares it and the UI keeps %d at most", MaxScheduled)} {
		if !slices.Contains(logged, want) {
			t.Errorf("the activity log hasn't %q: %q", want, logged)
		}
	}
	// With no file at all, given back whatever the UI holds.
	reconcile(t, l, d, Leave{})
	if r, _ := reconcile(t, l, Declared{}, Leave{NoFile: true}); r != (Reconciled{Undeclared: 2}) {
		t.Errorf("no file: %+v", r)
	}
}

// A file that replaces what it declares, at the most of both kinds
// together, lets go of the old before it adds the new: none of the new is
// refused as full.
func TestReplacedAtTheMost(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	start := time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC)
	err := l.write(ctx, func(t *txn) error {
		for i := range MaxWindows {
			if _, err := t.ExecContext(t.ctx, "INSERT INTO windows (title, start_at, end_at, updated) VALUES (?, ?, ?, 1)", fmt.Sprint("ui", i), start.Unix(), start.Unix()+60); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	windows := func(prefix string) Declared {
		var d Declared
		for i := range MaxWindows {
			d.Windows = append(d.Windows, DeclaredWindow{Window: Window{Title: fmt.Sprint(prefix, i), Start: start, End: start.Add(time.Hour)}, Line: i + 1})
		}
		return d
	}
	if r, skipped := reconcile(t, l, windows("old"), Leave{Prune: true}); r.Made != MaxWindows || len(skipped) != 0 {
		t.Fatalf("the first: %+v %d skipped", r, len(skipped))
	}
	r, skipped := reconcile(t, l, windows("new"), Leave{Prune: true})
	if r.Made != MaxWindows || r.Deleted != MaxWindows || len(skipped) != 0 {
		t.Errorf("replaced: %+v, %d skipped (%v)", r, len(skipped), skipped[:min(len(skipped), 2)])
	}
}

// So do reports, and roles, taken away before others are given.
func TestReplacedAtTheMostReportsRoles(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	err := l.write(ctx, func(t *txn) error {
		for i := range MaxScheduled {
			if _, err := t.ExecContext(t.ctx, "INSERT INTO reports (name, schedule, dests, created, updated) VALUES (?, 'daily@09:00', '[\"ops\"]', 1, 1)", fmt.Sprint("ui", i)); err != nil {
				return err
			}
		}
		for i := range MaxGrants {
			if _, err := t.ExecContext(t.ctx, "INSERT INTO grants (who, role, created, created_by) VALUES (?, ?, 1, 'ana')", fmt.Sprint("user:ui", i), GrantedRole); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	declared := func(prefix string) Declared {
		var d Declared
		for i := range MaxScheduled {
			d.Reports = append(d.Reports, DeclaredReport{Scheduled: Scheduled{Name: fmt.Sprint(prefix, i), Schedule: "daily@09:00", Dests: []string{"ops"}, Enabled: true}, Line: i + 1})
		}
		for i := range MaxGrants {
			d.Grants = append(d.Grants, DeclaredGrant{Who: fmt.Sprint("user:", prefix, i), Line: MaxScheduled + i + 1})
		}
		return d
	}
	all := MaxScheduled + MaxGrants
	if r, skipped := reconcile(t, l, declared("old"), Leave{Prune: true}); r.Made != all || len(skipped) != 0 {
		t.Fatalf("the first: %+v %d skipped", r, len(skipped))
	}
	r, skipped := reconcile(t, l, declared("new"), Leave{Prune: true})
	if r.Made != all || r.Deleted != all || len(skipped) != 0 {
		t.Errorf("replaced: %+v, %d skipped (%v)", r, len(skipped), skipped[:min(len(skipped), 2)])
	}
}

// A role taken away keeps whom it was given to and of which team, taken
// in the UI or with its team.
func TestRoleTakenKeepsWhom(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	pay, err := l.PutTeam(ctx, Team{Name: "payments", Rules: "^P"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	g, err := l.PutGrant(ctx, Grant{Who: "user:pat", TeamID: pay}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.DeleteGrant(ctx, g, "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutGrant(ctx, Grant{Who: "group:sre", TeamID: pay}, "ana"); err != nil {
		t.Fatal(err)
	}
	if err := l.DeleteTeam(ctx, pay, "ana"); err != nil {
		t.Fatal(err)
	}
	cs, _ := l.Changes(ctx, 50)
	for _, who := range []string{"user:pat", "group:sre"} {
		i := slices.IndexFunc(cs, func(c Change) bool { return c.What == RoleTaken+who })
		if i < 0 || !strings.Contains(cs[i].Before, `"who":"`+who+`"`) || !strings.Contains(cs[i].Before, fmt.Sprintf(`"team_id":%d`, pay)) {
			t.Errorf("%s's role taken: %+v", who, cs)
		}
	}
}

// A name a team is declared by that couldn't be kept names nothing else:
// a role or a quiet window of it is skipped, said why, not given to the
// UI's team of that name. A team declared twice by its name keeps the team
// its renamed_from names, as it was.
func TestSkippedTeamNamesNothingElse(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	reconcile(t, l, Declared{Teams: []DeclaredTeam{{Team: Team{Name: "B", Rules: "^B"}, Line: 1}}}, Leave{Prune: true})
	c, err := l.PutTeam(ctx, Team{Name: "C", Rules: "^C"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	d := Declared{
		Teams:  []DeclaredTeam{{Team: Team{Name: "C", Rules: "^C"}, RenamedFrom: "B", Line: 1}},
		Grants: []DeclaredGrant{{Who: "user:mallory", Team: "C", Line: 2}},
	}
	_, skipped := reconcile(t, l, d, Leave{Prune: true})
	if len(skipped) != 2 || skipped[1].Kind != KindGrant || !strings.Contains(skipped[1].Error(), "declared on line 1, can't be kept") {
		t.Errorf("skipped: %v", skipped)
	}
	if gs, _ := l.Grants(ctx); slices.ContainsFunc(gs, func(g Grant) bool { return g.TeamID == c }) {
		t.Errorf("a role given to the UI's team C: %+v", gs)
	}
	// Declared twice by its name, the second renamed from B: B is kept.
	d = Declared{Teams: []DeclaredTeam{{Team: Team{Name: "X", Rules: "^X"}, Line: 1}, {Team: Team{Name: "x", Rules: "^X"}, RenamedFrom: "B", Line: 2}}}
	reconcile(t, l, d, Leave{Prune: true})
	ts, _ := l.Teams(ctx)
	if !slices.ContainsFunc(ts, func(x Team) bool { return x.Name == "B" && x.Declared }) {
		t.Errorf("B, which a team declared twice was renamed from: %+v", ts)
	}
}

// What no --config gave back is marked so: the next file that doesn't
// declare it again lets it go as one it no longer declares, a role taken
// away, the rest deleted when pruned and the UI's for good when not, a
// team something names given back, but what the UI changed since, which
// is the UI's. Given back, a role is noted as not declared.
func TestGivenBackLetGoByTheNextFile(t *testing.T) {
	ctx := context.Background()
	for _, prune := range []bool{false, true} {
		l := open(t, time.UTC)
		reconcile(t, l, declaredOf(), Leave{Prune: prune})
		if r, _ := reconcile(t, l, Declared{}, Leave{NoFile: true, Prune: prune}); r != (Reconciled{Undeclared: 5}) {
			t.Fatalf("no file: %+v", r)
		}
		gb, err := l.GivenBack(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := []GivenBackRow{{KindGrant, "group:payments-oncall", "payments"}, {KindGrant, "user:ana@example.com", ""},
			{KindReport, "payments weekly", "payments"}, {KindTeam, "payments", ""}, {KindWindow, "nightly backup", "payments"}}
		if !slices.Equal(gb, want) {
			t.Errorf("given back: %+v", gb)
		}
		cs, _ := l.Changes(ctx, 100)
		if i := slices.IndexFunc(cs, func(c Change) bool { return c.What == RoleGiven+"user:ana@example.com"+givenBackNow }); i < 0 || strings.Contains(cs[i].After, `"declared":true`) {
			t.Errorf("a role given back, noted: %+v", cs)
		}
		// Again with no file: as it is.
		if r, _ := reconcile(t, l, Declared{}, Leave{NoFile: true}); r.Any() {
			t.Errorf("no file again: %+v", r)
		}
		// The UI changes the report: the UI's.
		rs, _ := l.Scheduleds(ctx)
		rs[0].Schedule = "weekly tue@09:00"
		if _, err := l.PutScheduled(ctx, rs[0], "ana", time.Now()); err != nil {
			t.Fatal(err)
		}
		// A file with another team alone: payments, which the report names,
		// given back for good either way.
		d := Declared{Teams: []DeclaredTeam{{Team: Team{Name: "other", Rules: "^O"}, Line: 1}}}
		r, _ := reconcile(t, l, d, Leave{Prune: prune})
		want2 := Reconciled{Made: 1, Undeclared: 2, Deleted: 2}
		if prune {
			want2 = Reconciled{Made: 1, Undeclared: 1, Deleted: 3}
		}
		if r != want2 {
			t.Errorf("pruned %v: the next file: %+v, want %+v", prune, r, want2)
		}
		if gs, _ := l.Grants(ctx); len(gs) != 0 {
			t.Errorf("pruned %v: roles the file no longer gives: %+v", prune, gs)
		}
		ws, _ := l.windows(ctx, l.r, "")
		if len(ws) != b2i(!prune) || len(ws) == 1 && (ws[0].Declared || ws[0].GivenBack) {
			t.Errorf("pruned %v: windows: %+v", prune, ws)
		}
		if rs, _ := l.Scheduleds(ctx); len(rs) != 1 || rs[0].Declared {
			t.Errorf("pruned %v: the report the UI changed: %+v", prune, rs)
		}
		if ts, _ := l.Teams(ctx); len(ts) != 2 || ts[1].Name != "payments" || ts[1].Declared || ts[1].GivenBack {
			t.Errorf("pruned %v: the teams: %+v", prune, ts)
		}
		if gb, _ := l.GivenBack(ctx); len(gb) != 0 {
			t.Errorf("pruned %v: given back still: %+v", prune, gb)
		}
	}
}

// A quiet window that can't be kept, of a team that can't be, keeps the
// declared one of its title and team as it was: another team's window of
// the title doesn't take it.
func TestBadWindowKeepsItsOwn(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	start := time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC)
	freeze := func(team string) DeclaredWindow {
		return DeclaredWindow{Window: Window{Title: "freeze", Start: start, End: start.Add(time.Hour)}, Team: team, Line: 5}
	}
	d := Declared{Teams: []DeclaredTeam{{Team: Team{Name: "pay", Rules: "^P"}, Line: 1}, {Team: Team{Name: "search", Rules: "^S"}, Line: 2}},
		Windows: []DeclaredWindow{freeze("pay")}}
	reconcile(t, l, d, Leave{})
	ws, _ := l.windows(ctx, l.r, "")
	pay := ws[0].TeamID
	d.Teams[0].Bad = errors.New("can't be kept")
	d.Windows = []DeclaredWindow{freeze("pay"), freeze("search")}
	if _, skipped := reconcile(t, l, d, Leave{}); len(skipped) != 2 {
		t.Errorf("skipped: %v", skipped)
	}
	ws, _ = l.windows(ctx, l.r, "ORDER BY id")
	if len(ws) != 2 || ws[0].TeamID != pay || !ws[0].Declared || ws[1].TeamID == pay {
		t.Errorf("windows: %+v", ws)
	}
}

// A team that can't be kept holds its name: another of it, in another
// letter case, is skipped as a second, and the team kept as it was.
func TestBadTeamHoldsItsName(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	reconcile(t, l, Declared{Teams: []DeclaredTeam{{Team: Team{Name: "ops", Rules: "^Ops"}, Line: 1}}}, Leave{})
	d := Declared{Teams: []DeclaredTeam{{Team: Team{Name: "ops", Rules: "^Ops"}, Bad: errors.New("can't be kept"), Line: 1},
		{Team: Team{Name: "OPS", Rules: "^Other"}, Line: 2}}}
	_, skipped := reconcile(t, l, d, Leave{})
	if len(skipped) != 2 || !strings.Contains(skipped[1].Error(), "declared on line 1 already") {
		t.Errorf("skipped: %v", skipped)
	}
	if ts, _ := l.Teams(ctx); len(ts) != 1 || ts[0].Name != "ops" || ts[0].Rules != "^Ops" || !ts[0].Declared {
		t.Errorf("teams: %+v", ts)
	}
}

// What no --config gave back is the UI's for good once the UI changes it:
// a team, a quiet window or a report changed is given back no more.
func TestGivenBackClearedByTheUI(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	reconcile(t, l, declaredOf(), Leave{})
	reconcile(t, l, Declared{}, Leave{NoFile: true})
	ts, _ := l.Teams(ctx)
	ts[0].Note = "ours"
	if _, err := l.PutTeam(ctx, ts[0], "ana"); err != nil {
		t.Fatal(err)
	}
	ws, _ := l.windows(ctx, l.r, "")
	ws[0].Rules = "^Backups?"
	if _, err := l.PutWindow(ctx, ws[0], "ana", opts); err != nil {
		t.Fatal(err)
	}
	rs, _ := l.Scheduleds(ctx)
	rs[0].Enabled = false
	if _, err := l.PutScheduled(ctx, rs[0], "ana", time.Now()); err != nil {
		t.Fatal(err)
	}
	gb, _ := l.GivenBack(ctx)
	if len(gb) != 2 || gb[0].Kind != KindGrant || gb[1].Kind != KindGrant {
		t.Errorf("given back still: %+v", gb)
	}
}

// What no --config gave back that the next file declares again is
// declared again, given back no more.
func TestGivenBackDeclaredAgain(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	reconcile(t, l, declaredOf(), Leave{})
	reconcile(t, l, Declared{}, Leave{NoFile: true})
	if r, _ := reconcile(t, l, declaredOf(), Leave{Prune: true}); r != (Reconciled{Adopted: 5}) {
		t.Errorf("declared again: %+v", r)
	}
	if gb, _ := l.GivenBack(ctx); len(gb) != 0 {
		t.Errorf("given back still: %+v", gb)
	}
}
