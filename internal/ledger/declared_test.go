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
func reconcile(t *testing.T, l *Ledger, d Declared, left Leftover) (Reconciled, []DeclaredError) {
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
	r, skipped := reconcile(t, l, declaredOf(), LeftUndeclare)
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
	r, _ = reconcile(t, l, declaredOf(), LeftUndeclare)
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
	reconcile(t, l, d, LeftUndeclare)
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
	// Changed but for its name, it is the UI's to change.
	if _, err := l.PutTeam(ctx, Team{ID: ui, Name: "search", Rules: "^Search", Note: "ours"}, "ana"); err != nil {
		t.Errorf("a team a declared role names, changed but for its name: %v", err)
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
// and the UI may change it; kept, it stays declared; deleted, it goes,
// each noted.
func TestReconcileRemoved(t *testing.T) {
	ctx := context.Background()
	for _, left := range []Leftover{LeftUndeclare, LeftKeep, LeftDelete} {
		l := open(t, time.UTC)
		reconcile(t, l, declaredOf(), left)
		r, _ := reconcile(t, l, Declared{}, left)
		ts, _ := l.Teams(ctx)
		ws, _ := l.windows(ctx, l.r, "")
		rs, _ := l.Scheduleds(ctx)
		gs, _ := l.Grants(ctx)
		logged := strings.Join(changesBy(t, l, ConfigBy), "\n")
		switch left {
		case LeftKeep:
			if r != (Reconciled{Kept: 5}) || !ts[0].Declared || !ws[0].Declared || !rs[0].Declared || !gs[0].Declared {
				t.Errorf("kept: %+v; %+v %+v %+v %+v", r, ts, ws, rs, gs)
			}
		case LeftDelete:
			if r != (Reconciled{Deleted: 5}) || len(ts)+len(ws)+len(rs)+len(gs) != 0 {
				t.Errorf("pruned: %+v; left %+v %+v %+v %+v", r, ts, ws, rs, gs)
			}
			for _, want := range []string{"team payments deleted", "quiet window nightly backup deleted", "report payments weekly deleted", RoleTaken + "group:payments-oncall"} {
				if !strings.Contains(logged, want) {
					t.Errorf("pruned: the activity log hasn't %q:\n%s", want, logged)
				}
			}
		default:
			if r != (Reconciled{Undeclared: 5}) || len(ts) != 1 || len(ws) != 1 || len(rs) != 1 || len(gs) != 2 {
				t.Fatalf("taken out: %+v; left %+v %+v %+v %+v", r, ts, ws, rs, gs)
			}
			if ts[0].Declared || ws[0].Declared || rs[0].Declared || gs[0].Declared || gs[1].Declared || ws[0].TeamID != ts[0].ID {
				t.Errorf("still declared: %+v %+v %+v %+v", ts, ws, rs, gs)
			}
			if !strings.Contains(logged, "team payments"+undeclaredNow) || !strings.Contains(logged, "quiet window nightly backup"+undeclaredNow) {
				t.Errorf("taken out: the activity log:\n%s", logged)
			}
			if _, err := l.PutTeam(ctx, Team{ID: ts[0].ID, Name: "payments", Rules: "^Pay"}, "ana"); err != nil {
				t.Errorf("the UI changing a team no longer declared: %v", err)
			}
			if err := l.DeleteWindow(ctx, ws[0].ID, "ana", opts); err != nil {
				t.Errorf("the UI deleting a window no longer declared: %v", err)
			}
		}
	}
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
	reconcile(t, l, d, LeftDelete)
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
	r, _ := reconcile(t, l, Declared{Teams: []DeclaredTeam{{Team: Team{Name: "other", Rules: "^O"}, Line: 1}}}, LeftDelete)
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
	r, _ = reconcile(t, l, d, LeftDelete)
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
	reconcile(t, l, declaredOf(), LeftDelete)
	d := declaredOf()
	d.Teams[0].Note = "changed"
	d.Windows[0].End = d.Windows[0].Start.Add(-time.Hour) // can't be kept
	d.Grants = d.Grants[:1]                               // ana's taken away
	d.Reports[0].Bad = errors.New("there is no destination called nowhere")
	d.Windows = append(d.Windows, DeclaredWindow{Window: Window{Title: "of nobody", Start: d.Windows[0].Start, End: d.Windows[0].Start.Add(time.Hour)}, Team: "nobody", Line: 30})
	r, skipped := reconcile(t, l, d, LeftDelete)
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
	r, skipped := reconcile(t, l, Declared{Windows: []DeclaredWindow{{Window: Window{Title: "declared", Start: start, End: start.Add(time.Hour)}, Line: 1}}}, LeftUndeclare)
	if r.Made != 1 || len(skipped) != 0 {
		t.Errorf("a file beside the UI's most: %+v %v", r, skipped)
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
	r, skipped := reconcile(t, l, d, LeftUndeclare)
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
	reconcile(t, l, d, LeftUndeclare)
	ws, _ := l.windows(ctx, l.r, "ORDER BY id")
	d.Windows = d.Windows[1:]
	r, _ := reconcile(t, l, d, LeftUndeclare)
	after, _ := l.windows(ctx, l.r, "ORDER BY id")
	if r != (Reconciled{Undeclared: 1}) || len(after) != 2 || after[0].Declared || !after[1].Declared || !sameWindow(after[1], ws[1]) {
		t.Errorf("one of two taken out: %+v; %+v", r, after)
	}
	// Moved by the file to another team, a declared window is changed.
	d.Windows[0].Team = "a"
	r, _ = reconcile(t, l, d, LeftUndeclare)
	after, _ = l.windows(ctx, l.r, "ORDER BY id")
	if r != (Reconciled{Changed: 1}) || after[1].ID != ws[1].ID || after[1].TeamID != after[0].TeamID {
		t.Errorf("moved to another team: %+v; %+v", r, after)
	}
}

// A team the file renames (renamed_from) keeps its id, and what names it.
func TestReconcileRenamedFrom(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	reconcile(t, l, Declared{Teams: []DeclaredTeam{{Team: Team{Name: "payments", Rules: "^P"}, Line: 1}}}, LeftDelete)
	ts, _ := l.Teams(ctx)
	id := ts[0].ID
	if _, err := l.PutTask(ctx, Task{Title: "t", TeamID: id}, "ana"); err != nil {
		t.Fatal(err)
	}
	r, _ := reconcile(t, l, Declared{Teams: []DeclaredTeam{{Team: Team{Name: "payments-eu", Rules: "^P"}, RenamedFrom: "payments", Line: 1}}}, LeftDelete)
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
	reconcile(t, l, d, LeftUndeclare)
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
	if r, _ := reconcile(t, l, d, LeftUndeclare); r.Any() {
		t.Errorf("read again after pruning: %+v", r)
	}
}
