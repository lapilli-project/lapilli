package ledger

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// vAt is a time on 2026-11-02 (t0, a Monday), hours and minutes in UTC.
func vAt(h, m int) time.Time {
	return t0.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
}

// vNow is when the verdicts are said: three days after the firings.
var vNow = t0.Add(3 * 24 * time.Hour)

// verdictLedger is a ledger with, on 2026-11-02, INC-12 on checkout from
// 14:10 to 14:40; a disk alert on checkout's database at 14:05 and an
// errors alert on checkout at 14:12, both catching it; and a search alert
// at 18:00 that caught nothing. The team shop owns Disk; the team find,
// Search. It returns the firings by rule.
func verdictLedger(t *testing.T) (*Ledger, map[string]Firing) {
	t.Helper()
	ctx := context.Background()
	l := open(t, time.UTC)
	if _, err := l.Put(ctx, incident.Incident{ID: "INC-12", Title: "checkout 5xx", Start: vAt(14, 10), End: vAt(14, 40),
		Labels: incident.Labels{"service": {"checkout"}}}, "ana", opts); err != nil {
		t.Fatal(err)
	}
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{
		{Rule: "Disk", Labels: map[string]string{"service": "checkout", "instance": "db"}, Start: vAt(14, 5), End: vAt(14, 20)},
		{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: vAt(14, 12), End: vAt(14, 30)},
		{Rule: "Search", Labels: map[string]string{"service": "search"}, Start: vAt(18, 0), End: vAt(18, 10)},
	})
	for _, team := range []Team{{Name: "shop", Rules: "^Disk$"}, {Name: "find", Rules: "^Search$"}} {
		if _, err := l.PutTeam(ctx, team, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	return l, firingsByRule(t, l)
}

func firingsByRule(t *testing.T, l *Ledger) map[string]Firing {
	t.Helper()
	fs, err := l.Firings(context.Background(), FiringQuery{From: t0, To: t0.Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Firing{}
	for _, f := range fs {
		out[f.Rule] = f
	}
	return out
}

func keyOfFiring(f Firing) FiringKey { return FiringKey{ID: f.ID, Series: f.Series, Start: f.Start} }

func yes() *bool            { b := true; return &b }
func no() *bool             { b := false; return &b }
func text(s string) *string { return &s }
func editorBy() VerdictBy   { return VerdictBy{By: "ed", Now: vNow} }
func adminBy() VerdictBy    { return VerdictBy{By: "root", Admin: true, Now: vNow} }
func teamCtx(l *Ledger, t *testing.T, name string) context.Context {
	t.Helper()
	teams, err := l.Teams(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range teams {
		if x.Name == name {
			return WithTeams(context.Background(), []int64{x.ID})
		}
	}
	t.Fatalf("no team %s", name)
	return nil
}

// "Unrelated" turns that one firing black: the incident is caught by the
// others alone, the report and the tallies say so, and the change is noted
// with its before and after. Undone, it is white again.
func TestVerdictUnrelated(t *testing.T) {
	ctx := context.Background()
	l, fs := verdictLedger(t)
	v, err := l.SetVerdict(ctx, keyOfFiring(fs["Disk"]), VerdictChange{Unrelated: yes(), UnrelatedNote: text("a log rotation")}, editorBy(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Unrelated) != 1 || v.Unrelated[0].Key != "INC-12" || !v.Unrelated[0].Start.Equal(vAt(14, 10)) || v.UnrelatedBy != "ed" || v.UnrelatedNote != "a log rotation" {
		t.Errorf("the verdict kept: %+v", v)
	}
	fs = firingsByRule(t, l)
	if d := fs["Disk"]; d.White || !d.SaidUnrelated() || !slices.Equal(d.Unrelated, []string{"INC-12"}) {
		t.Errorf("the disk's firing: %+v", d)
	}
	if !fs["Errors"].White {
		t.Error("the errors' firing should stay white")
	}
	in, err := l.IncidentByID(ctx, "INC-12")
	if err != nil || len(in.CaughtBy) != 1 || in.CaughtBy[0].Rule != "Errors" {
		t.Errorf("INC-12 caught by %+v (%v), want Errors alone", in.CaughtBy, err)
	}
	o := opts
	rep, err := l.Report(ctx, t0, t0.Add(7*24*time.Hour), o, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rep.Rules {
		if r.Rule == "Disk" && (r.Unrelated != 1 || r.White != 0 || r.Incidents != 0) {
			t.Errorf("Disk in the report: %+v", r)
		}
	}
	for _, j := range rep.Firings {
		if j.Rule == "Disk" && (j.White || !slices.Equal(j.Unrelated, []string{"INC-12"}) || len(j.Incidents) != 0) {
			t.Errorf("Disk's firing in the report: %+v", j)
		}
	}
	weeks, err := l.WeeksBy(ctx, Week(l.Day(t0)), Week(l.Day(t0)))
	if err != nil {
		t.Fatal(err)
	}
	var unrelated int
	for _, w := range weeks {
		unrelated += w.Unrelated
	}
	if unrelated != 1 {
		t.Errorf("the week's tallies say %d unrelated, want 1", unrelated)
	}
	// Noted at the write's time (vNow), by its writer.
	cs, err := l.Changes(ctx, 10)
	i := slices.IndexFunc(cs, func(c Change) bool { return strings.HasPrefix(c.What, "verdict on Disk ") })
	if err != nil || i < 0 || !strings.Contains(cs[i].After, "INC-12") || cs[i].Before != "" || cs[i].By != "ed" || !cs[i].At.Equal(vNow) {
		t.Errorf("the change noted: %+v %v", cs, err)
	}
	// Undone, white again.
	if err := l.DeleteVerdict(ctx, keyOfFiring(fs["Disk"]), "unrelated", editorBy(), opts); err != nil {
		t.Fatal(err)
	}
	if d := firingsByRule(t, l)["Disk"]; !d.White || d.SaidUnrelated() || len(d.Unrelated) != 0 {
		t.Errorf("undone: %+v", d)
	}
	if err := l.DeleteVerdict(ctx, keyOfFiring(fs["Disk"]), "", editorBy(), opts); !errors.Is(err, ErrNoVerdict) || !errors.Is(err, ErrNotFound) {
		t.Errorf("undoing nothing: %v", err)
	}
}

// Each part keeps its own who and when; "needed action" changes no
// stone, and is counted.
func TestVerdictParts(t *testing.T) {
	ctx := context.Background()
	l, fs := verdictLedger(t)
	if _, err := l.SetVerdict(ctx, keyOfFiring(fs["Disk"]), VerdictChange{Unrelated: yes()}, VerdictBy{By: "ana", Now: vNow}, opts); err != nil {
		t.Fatal(err)
	}
	v, err := l.SetVerdict(ctx, keyOfFiring(fs["Disk"]), VerdictChange{Action: text("no"), ActionNote: text("healed itself")}, VerdictBy{By: "bo", Now: vNow}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if v.UnrelatedBy != "ana" || v.ActionBy != "bo" || v.Action != "no" || v.ActionNote != "healed itself" {
		t.Errorf("each part's who: %+v", v)
	}
	if _, err := l.SetVerdict(ctx, keyOfFiring(fs["Search"]), VerdictChange{Action: text("yes")}, editorBy(), opts); err != nil {
		t.Fatal(err)
	}
	fs2 := firingsByRule(t, l)
	if s := fs2["Search"]; s.White || s.Action() != verdict.ActionYes {
		t.Errorf("Search: %+v", s)
	}
	weeks, _ := l.WeeksBy(ctx, Week(l.Day(t0)), Week(l.Day(t0)))
	var yesN, noN int
	for _, w := range weeks {
		yesN, noN = yesN+w.ActionYes, noN+w.ActionNo
	}
	if yesN != 1 || noN != 1 {
		t.Errorf("answers %d yes, %d no; want 1 and 1", yesN, noN)
	}
	// A note is one line, 500 characters at most; an answer yes, no or
	// none.
	for _, c := range []VerdictChange{{ActionNote: text("two\nlines")}, {ActionNote: text(strings.Repeat("x", 501))}, {Action: text("maybe")}, {}} {
		if _, err := l.SetVerdict(ctx, keyOfFiring(fs["Search"]), c, editorBy(), opts); !errors.As(err, new(ErrInvalid)) {
			t.Errorf("%+v: %v, want refused", c, err)
		}
	}
	if _, err := l.SetVerdict(ctx, keyOfFiring(fs["Search"]), VerdictChange{ActionNote: text(strings.Repeat("가", 500))}, editorBy(), opts); err != nil {
		t.Errorf("500 characters, not bytes: %v", err)
	}
}

// A team's editor answers "needed action" of their team's rules alone, and
// leaves "unrelated" as it is: set, changed or cleared, refused; their
// undoing of both undoes theirs alone.
func TestVerdictTeamEditor(t *testing.T) {
	l, fs := verdictLedger(t)
	shop := teamCtx(l, t, "shop")
	disk := keyOfFiring(fs["Disk"])
	if _, err := l.SetVerdict(shop, disk, VerdictChange{Unrelated: yes()}, editorBy(), opts); !errors.Is(err, ErrUnrelatedRole) {
		t.Errorf("a team's editor setting unrelated: %v", err)
	}
	if _, err := l.SetVerdict(shop, disk, VerdictChange{Action: text("yes"), ActionNote: text("restarted it")}, editorBy(), opts); err != nil {
		t.Errorf("a team's editor answering: %v", err)
	}
	if _, err := l.SetVerdict(shop, keyOfFiring(fs["Search"]), VerdictChange{Action: text("yes")}, editorBy(), opts); !errors.Is(err, ErrNotYourTeam) {
		t.Errorf("another team's rule: %v", err)
	}
	// An editor of every team says it is unrelated.
	if _, err := l.SetVerdict(context.Background(), disk, VerdictChange{Unrelated: yes(), UnrelatedNote: text("rotation")}, editorBy(), opts); err != nil {
		t.Fatal(err)
	}
	for _, c := range []VerdictChange{{Unrelated: no()}, {Unrelated: no(), Action: text("no")}, {UnrelatedNote: text("mine")}} {
		if _, err := l.SetVerdict(shop, disk, c, editorBy(), opts); !errors.Is(err, ErrUnrelatedRole) {
			t.Errorf("a team's editor changing unrelated %+v: %v", c, err)
		}
	}
	// Given as stored, it stands: the rest is written.
	if _, err := l.SetVerdict(shop, disk, VerdictChange{Unrelated: yes(), Action: text("no")}, editorBy(), opts); err != nil {
		t.Errorf("unrelated as stored: %v", err)
	}
	if err := l.DeleteVerdict(shop, disk, "unrelated", editorBy(), opts); !errors.Is(err, ErrUnrelatedRole) {
		t.Errorf("a team's editor undoing unrelated: %v", err)
	}
	if err := l.DeleteVerdict(shop, disk, "", editorBy(), opts); err != nil {
		t.Fatal(err)
	}
	vs, err := l.VerdictsIn(context.Background(), t0, t0.Add(24*time.Hour), nil)
	if err != nil || len(vs) != 1 || len(vs[0].Unrelated) != 1 || vs[0].UnrelatedNote != "rotation" || vs[0].Action != "" {
		t.Errorf("after a team's editor undid both: %+v %v", vs, err)
	}
	if d := firingsByRule(t, l)["Disk"]; d.White {
		t.Error("the disk's firing should stay black")
	}
}

// An editor says something of the last 14 days' firings alone, and presses
// "This was real" on them alone; an admin of any firing kept.
func TestVerdictFourteenDays(t *testing.T) {
	ctx := context.Background()
	l, fs := verdictLedger(t)
	late := VerdictBy{By: "ed", Now: vAt(14, 5).Add(VerdictAge + time.Second)}
	edge := VerdictBy{By: "ed", Now: vAt(14, 5).Add(VerdictAge)}
	if _, err := l.SetVerdict(ctx, keyOfFiring(fs["Disk"]), VerdictChange{Action: text("yes")}, late, opts); !errors.Is(err, ErrTooOld) {
		t.Errorf("an editor, past 14 days: %v", err)
	}
	if _, err := l.SetVerdict(ctx, keyOfFiring(fs["Disk"]), VerdictChange{Action: text("yes")}, edge, opts); err != nil {
		t.Errorf("an editor, at 14 days: %v", err)
	}
	late.Admin = true
	if _, err := l.SetVerdict(ctx, keyOfFiring(fs["Disk"]), VerdictChange{Unrelated: yes()}, late, opts); err != nil {
		t.Errorf("an admin, past 14 days: %v", err)
	}
	late.Admin = false
	if err := l.DeleteVerdict(ctx, keyOfFiring(fs["Disk"]), "", late, opts); !errors.Is(err, ErrTooOld) {
		t.Errorf("an editor undoing, past 14 days: %v", err)
	}
	if _, err := l.SetVerdicts(ctx, []FiringKey{keyOfFiring(fs["Search"]), keyOfFiring(fs["Disk"])}, "no", nil, late, opts); !errors.Is(err, ErrTooOld) {
		t.Errorf("an editor in bulk, past 14 days: %v", err)
	}
	search := keyOfFiring(fs["Search"])
	if _, _, err := l.Real(ctx, search, VerdictBy{By: "ed", Now: vAt(18, 0).Add(VerdictAge + time.Second)}, opts); !errors.Is(err, ErrTooOld) {
		t.Errorf("This was real by an editor, past 14 days: %v", err)
	}
	if made, _, err := l.Real(ctx, search, VerdictBy{By: "root", Admin: true, Now: vAt(18, 0).Add(VerdictAge + time.Second)}, opts); err != nil || made == "" {
		t.Errorf("This was real by an admin, past 14 days: %q %v", made, err)
	}
}

// Thirty verdict writes an hour a person, of every kind (a form's, a bulk
// one, an undoing): past them, nothing is written, and when to try again
// is said. Another person has theirs, and the writer theirs again an hour
// on.
func TestVerdictsAnHour(t *testing.T) {
	ctx := context.Background()
	l, fs := verdictLedger(t)
	w := editorBy()
	search, errs := keyOfFiring(fs["Search"]), keyOfFiring(fs["Errors"])
	writes := 0
	for i := 0; writes < VerdictsAnHour; i++ {
		answer := []string{"yes", "no"}[i%2]
		var err error
		switch i % 3 {
		case 0:
			_, err = l.SetVerdict(ctx, search, VerdictChange{Action: &answer}, w, opts)
		case 1:
			_, err = l.SetVerdicts(ctx, []FiringKey{search, errs}, answer, nil, w, opts)
		case 2:
			err = l.DeleteVerdict(ctx, search, "action", w, opts)
		}
		if err != nil {
			t.Fatalf("write %d: %v", writes+1, err)
		}
		writes++
	}
	cs, _ := l.Changes(ctx, 1000)
	before := len(cs)
	var many TooManyVerdicts
	for _, write := range []func() error{
		func() error {
			_, err := l.SetVerdict(ctx, search, VerdictChange{Action: text("no"), ActionNote: text("x")}, w, opts)
			return err
		},
		func() error {
			_, err := l.SetVerdicts(ctx, []FiringKey{search, errs}, "yes", text("y"), w, opts)
			return err
		},
		func() error { return l.DeleteVerdict(ctx, errs, "", w, opts) },
		func() error {
			_, err := l.SetVerdict(ctx, keyOfFiring(fs["Disk"]), VerdictChange{Unrelated: yes()}, w, opts)
			return err
		},
	} {
		if err := write(); !errors.As(err, &many) || !errors.Is(err, ErrTooManyVerdicts) || many.Retry <= 0 || many.Retry > time.Hour {
			t.Errorf("the 31st write: %v (%+v)", err, many)
		}
	}
	if cs, _ := l.Changes(ctx, 1000); len(cs) != before {
		t.Errorf("%d changes after refusals, %d before: nothing should be written", len(cs), before)
	}
	if d := firingsByRule(t, l)["Disk"]; !d.White {
		t.Error("the disk's firing turned black past the hour's writes")
	}
	// Folded as names are: ED is ed.
	if _, err := l.SetVerdict(ctx, search, VerdictChange{Action: text("no"), ActionNote: text("z")}, VerdictBy{By: "ED", Now: vNow}, opts); !errors.Is(err, ErrTooManyVerdicts) {
		t.Errorf("ED: %v", err)
	}
	if _, err := l.SetVerdict(ctx, search, VerdictChange{Action: text("no"), ActionNote: text("other")}, VerdictBy{By: "ana", Now: vNow}, opts); err != nil {
		t.Errorf("another person: %v", err)
	}
	// An hour on, the writer has theirs again.
	if _, err := l.w.ExecContext(ctx, "UPDATE changes SET at = at - 3601"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.SetVerdict(ctx, search, VerdictChange{Action: text("yes"), ActionNote: text("later")}, w, opts); err != nil {
		t.Errorf("an hour later: %v", err)
	}
}

// The reads a page or a call makes of verdicts take what they give from
// the ledger, no more: the first most of a period, counted apart; a page's
// firings' own, by alert; the latest moved, most at most, and how many of
// them all started before a time.
func TestVerdictReadsBounded(t *testing.T) {
	ctx := context.Background()
	l, fs := verdictLedger(t)
	if _, err := l.SetVerdict(ctx, keyOfFiring(fs["Disk"]), VerdictChange{Unrelated: yes()}, editorBy(), opts); err != nil {
		t.Fatal(err)
	}
	if _, err := l.SetVerdict(ctx, keyOfFiring(fs["Search"]), VerdictChange{Action: text("yes")}, editorBy(), opts); err != nil {
		t.Fatal(err)
	}
	if n, err := l.CountVerdicts(ctx, t0, t0.Add(24*time.Hour)); err != nil || n != 2 {
		t.Errorf("counted %d %v", n, err)
	}
	if vs, err := l.VerdictsFirst(ctx, t0, t0.Add(24*time.Hour), 1); err != nil || len(vs) != 1 || vs[0].Rule != "Disk" {
		t.Errorf("the first: %+v %v", vs, err)
	}
	if got, err := l.VerdictsOf(ctx, []Firing{fs["Search"]}); err != nil || len(got) != 1 || got[fs["Search"].ID].Action != "yes" {
		t.Errorf("of Search's firing: %+v %v", got, err)
	}
	if _, err := l.w.ExecContext(ctx, "UPDATE verdicts SET moved = ?", movedGone); err != nil {
		t.Fatal(err)
	}
	moved, old, err := l.MovedVerdicts(ctx, 1, vNow)
	if err != nil || len(moved) != 1 || moved[0].Rule != "Search" || old != 2 {
		t.Errorf("moved: %+v, %d before, %v", moved, old, err)
	}
}

// A note is its writer's: a part whose note another rewrites is theirs,
// and an answer changed with no note given leaves none of the one before.
func TestVerdictNotesCredited(t *testing.T) {
	ctx := context.Background()
	l, fs := verdictLedger(t)
	disk := keyOfFiring(fs["Disk"])
	shop := teamCtx(l, t, "shop")
	pat := VerdictBy{By: "pat", Now: vNow.Add(time.Minute)}
	if _, err := l.SetVerdict(ctx, disk, VerdictChange{Unrelated: yes(), UnrelatedNote: text("a log rotation"), Action: text("yes"), ActionNote: text("paged")}, adminBy(), opts); err != nil {
		t.Fatal(err)
	}
	// An editor rewrites the admin's note on "unrelated".
	v, err := l.SetVerdict(ctx, disk, VerdictChange{UnrelatedNote: text("a cron job")}, editorBy(), opts)
	if err != nil || v.UnrelatedBy != "ed" || v.UnrelatedNote != "a cron job" || len(v.Unrelated) != 1 {
		t.Errorf("an editor's note on unrelated: %+v %v", v, err)
	}
	// A team's editor flips the admin's yes to no, giving no note.
	v, err = l.SetVerdict(shop, disk, VerdictChange{Action: text("no")}, pat, opts)
	if err != nil || v.Action != "no" || v.ActionBy != "pat" || v.ActionNote != "" || !v.ActionAt.Equal(pat.Now) {
		t.Errorf("a team's editor's no: %+v %v", v, err)
	}
	// The admin's note on it, then a team's editor's in its place.
	if v, err = l.SetVerdict(ctx, disk, VerdictChange{ActionNote: text("it healed")}, adminBy(), opts); err != nil || v.ActionBy != "root" || v.ActionNote != "it healed" {
		t.Errorf("the admin's note on needed action: %+v %v", v, err)
	}
	if v, err = l.SetVerdict(shop, disk, VerdictChange{ActionNote: text("restarted it")}, pat, opts); err != nil || v.ActionBy != "pat" || v.ActionNote != "restarted it" {
		t.Errorf("a team's editor's note on needed action: %+v %v", v, err)
	}
	// The same answer again, with no note: nothing changes.
	if v, err = l.SetVerdict(ctx, disk, VerdictChange{Action: text("no")}, adminBy(), opts); err != nil || v.ActionBy != "pat" || v.ActionNote != "restarted it" {
		t.Errorf("the same answer again: %+v %v", v, err)
	}
}

// What people said, or undid, since a time, of a period's firings: an
// undoing that keeps no verdict counts as a saying does. SaidSince (a
// proposal's backtest gone stale) is "unrelated" set or undone of the
// rules asked; VerdictsSince (a goal's "no longer missed") every firing
// whose verdict changed, in bulk too.
func TestVerdictsSinceUndone(t *testing.T) {
	ctx := context.Background()
	l, fs := verdictLedger(t)
	at := func(h time.Duration) VerdictBy { return VerdictBy{By: "ed", Now: vNow.Add(h)} }
	day, week := t0.Add(24*time.Hour), t0.Add(7*24*time.Hour)
	said := func(since time.Time, rule string) bool {
		t.Helper()
		ok, err := l.SaidSince(ctx, t0, day, since, func(r string) bool { return r == rule })
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	count := func(since, from, to time.Time) int {
		t.Helper()
		n, err := l.VerdictsSince(ctx, since, from, to)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := l.SetVerdict(ctx, keyOfFiring(fs["Disk"]), VerdictChange{Unrelated: yes()}, at(0), opts); err != nil {
		t.Fatal(err)
	}
	if !said(vNow.Add(-time.Second), "Disk") || said(vNow, "Disk") || said(vNow.Add(-time.Second), "Search") {
		t.Error("said unrelated")
	}
	if err := l.DeleteVerdict(ctx, keyOfFiring(fs["Disk"]), "unrelated", at(time.Hour), opts); err != nil {
		t.Fatal(err)
	}
	if !said(vNow.Add(30*time.Minute), "Disk") {
		t.Error("undone after the backtest: not stale")
	}
	if _, err := l.SetVerdict(ctx, keyOfFiring(fs["Search"]), VerdictChange{Action: text("yes")}, at(2*time.Hour), opts); err != nil {
		t.Fatal(err)
	}
	if said(vNow.Add(90*time.Minute), "Search") {
		t.Error("needed action, which changes no stone, made it stale")
	}
	if n := count(vNow.Add(30*time.Minute), t0, week); n != 2 {
		t.Errorf("since the undoing: %d verdicts, want 2", n)
	}
	if _, err := l.SetVerdicts(ctx, []FiringKey{keyOfFiring(fs["Disk"]), keyOfFiring(fs["Errors"])}, "no", nil, at(3*time.Hour), opts); err != nil {
		t.Fatal(err)
	}
	if n := count(vNow.Add(150*time.Minute), t0, week); n != 2 {
		t.Errorf("since the bulk answer: %d verdicts, want 2", n)
	}
	if n := count(vNow.Add(-time.Hour), t0, week); n != 3 {
		t.Errorf("since before them all: %d firings, want 3", n)
	}
	if n := count(vNow.Add(-time.Hour), week, week.Add(7*24*time.Hour)); n != 0 {
		t.Errorf("of another week: %d", n)
	}
}

// A form's firing is held to the series and start the page read it with:
// another alert's, or another start's, or an id gone, is refused, nothing
// written. "This was real" alike.
func TestVerdictFiringChanged(t *testing.T) {
	ctx := context.Background()
	l, fs := verdictLedger(t)
	d := keyOfFiring(fs["Disk"])
	for _, k := range []FiringKey{
		{ID: d.ID, Series: fs["Errors"].Series, Start: d.Start},
		{ID: d.ID, Series: d.Series, Start: d.Start.Add(time.Minute)},
		{ID: d.ID + 1000, Series: d.Series, Start: d.Start},
		{ID: fs["Errors"].ID, Series: d.Series, Start: d.Start},
	} {
		if _, err := l.SetVerdict(ctx, k, VerdictChange{Unrelated: yes()}, editorBy(), opts); !errors.Is(err, ErrFiringChanged) {
			t.Errorf("%+v: %v", k, err)
		}
		if _, err := l.SetVerdicts(ctx, []FiringKey{k}, "yes", nil, editorBy(), opts); !errors.Is(err, ErrFiringChanged) {
			t.Errorf("bulk %+v: %v", k, err)
		}
		if _, _, err := l.Real(ctx, k, editorBy(), opts); !errors.Is(err, ErrFiringChanged) {
			t.Errorf("This was real %+v: %v", k, err)
		}
	}
	if vs, _ := l.VerdictsIn(ctx, t0, t0.Add(24*time.Hour), nil); len(vs) != 0 {
		t.Errorf("written: %+v", vs)
	}
	// By its rule, labels and start (the API): one not there is not found.
	if _, err := l.SetVerdict(ctx, FiringKey{Rule: "Disk", Labels: fs["Disk"].Labels, Start: d.Start.Add(time.Second)}, VerdictChange{Action: text("yes")}, editorBy(), opts); !errors.Is(err, ErrNotFound) {
		t.Errorf("no such firing: %v", err)
	}
	if _, err := l.SetVerdict(ctx, FiringKey{Rule: "Disk", Labels: fs["Disk"].Labels, Start: d.Start}, VerdictChange{Action: text("yes")}, editorBy(), opts); err != nil {
		t.Errorf("by rule, labels and start: %v", err)
	}
}

// "Unrelated" and "This was real" are exclusive; and "unrelated" needs a
// catch.
func TestVerdictRealExclusive(t *testing.T) {
	ctx := context.Background()
	l, fs := verdictLedger(t)
	if _, err := l.SetVerdict(ctx, keyOfFiring(fs["Disk"]), VerdictChange{Unrelated: yes()}, editorBy(), opts); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Real(ctx, keyOfFiring(fs["Disk"]), editorBy(), opts); !errors.Is(err, ErrSaidUnrelated) {
		t.Errorf("This was real on a firing said unrelated: %v", err)
	}
	if _, err := l.SetVerdict(ctx, keyOfFiring(fs["Search"]), VerdictChange{Unrelated: yes()}, editorBy(), opts); !errors.Is(err, ErrCaughtNothing) {
		t.Errorf("unrelated on a firing that caught nothing: %v", err)
	}
	made, _, err := l.Real(ctx, keyOfFiring(fs["Search"]), editorBy(), opts)
	if err != nil || made == "" {
		t.Fatalf("This was real: %q %v", made, err)
	}
	if in, err := l.IncidentByID(ctx, made); err != nil || !in.Real || in.RealSeries() != fs["Search"].Series {
		t.Errorf("the incident written: %+v %v", in, err)
	}
	if s := firingsByRule(t, l)["Search"]; !s.White || !s.RealOnly() {
		t.Errorf("Search after This was real: %+v", s)
	}
	if _, err := l.SetVerdict(ctx, keyOfFiring(fs["Search"]), VerdictChange{Unrelated: yes()}, editorBy(), opts); !errors.Is(err, ErrRealFiring) {
		t.Errorf("unrelated on a firing This was real wrote from: %v", err)
	}
	weeks, _ := l.WeeksBy(ctx, Week(l.Day(t0)), Week(l.Day(t0)))
	var real int
	for _, w := range weeks {
		real += w.RealWhite
	}
	if real != 1 {
		t.Errorf("real_white %d, want 1", real)
	}
}

// As Lapilli judged it, a firing is white when it catches any incident but
// one "This was real" wrote, said unrelated to it or not; bit 1 is a
// firing white so and black by people's word, bit 8 one white by people's
// word alone. Disk catches INC-A, said unrelated to it, and R, which "This
// was real" wrote afterwards: white both ways, neither bit. Cpu catches R
// alone, said unrelated to it: black both ways, neither bit. Probe, which
// R was written from, is white by it alone: bit 8. The tallies count so,
// and the report agrees with verdict.Judge given the same verdicts.
func TestVerdictsAsLapilliJudged(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	a := incident.Incident{ID: "INC-A", Title: "checkout 5xx", Start: vAt(14, 10), End: vAt(14, 40), Labels: incident.Labels{"service": {"checkout"}}}
	if _, err := l.Put(ctx, a, "ana", opts); err != nil {
		t.Fatal(err)
	}
	fs := []firing.Firing{
		{Rule: "Disk", Labels: map[string]string{"service": "checkout", "instance": "db"}, Start: vAt(14, 5), End: vAt(14, 20)},
		{Rule: "Probe", Labels: map[string]string{"service": "checkout"}, Start: vAt(13, 0), End: vAt(14, 6)},
		{Rule: "Cpu", Labels: map[string]string{"service": "checkout", "instance": "cpu"}, Start: vAt(13, 30), End: vAt(13, 40)},
	}
	record(t, l, t0, t0.Add(24*time.Hour), fs)
	by := firingsByRule(t, l)
	if !by["Disk"].White || by["Probe"].White || by["Cpu"].White {
		t.Fatalf("before: %+v", by)
	}
	if _, err := l.SetVerdict(ctx, keyOfFiring(by["Disk"]), VerdictChange{Unrelated: yes()}, editorBy(), opts); err != nil {
		t.Fatal(err)
	}
	made, _, err := l.Real(ctx, keyOfFiring(by["Probe"]), editorBy(), opts)
	if err != nil || made == "" {
		t.Fatalf("This was real: %q %v", made, err)
	}
	by = firingsByRule(t, l)
	if _, err := l.SetVerdict(ctx, keyOfFiring(by["Cpu"]), VerdictChange{Unrelated: yes()}, editorBy(), opts); err != nil {
		t.Fatal(err)
	}
	by = firingsByRule(t, l)
	for rule, want := range map[string][3]bool{"Disk": {true, false, false}, "Cpu": {false, false, false}, "Probe": {true, false, true}} {
		f := by[rule]
		if f.White != want[0] || f.SaidUnrelated() != want[1] || f.RealOnly() != want[2] {
			t.Errorf("%s: white %t, bit 1 %t, bit 8 %t; want %v (%+v)", rule, f.White, f.SaidUnrelated(), f.RealOnly(), want, f)
		}
	}
	weeks, err := l.WeeksBy(ctx, Week(l.Day(t0)), Week(l.Day(t0)))
	if err != nil {
		t.Fatal(err)
	}
	var white, unrelated, real int
	for _, w := range weeks {
		white, unrelated, real = white+w.White, unrelated+w.Unrelated, real+w.RealWhite
	}
	// As Lapilli judged it, Disk alone is white.
	if unrelated != 0 || real != 1 || white+unrelated-real != 1 {
		t.Errorf("the week: white %d, unrelated %d, real_white %d", white, unrelated, real)
	}
	r, err := l.IncidentByID(ctx, made)
	if err != nil || !r.Real {
		t.Fatalf("%s: %+v %v", made, r, err)
	}
	o := opts
	o.From, o.To = t0, t0.Add(7*24*time.Hour)
	if o.Verdicts, err = l.ReplayVerdicts(ctx, o.From, o.To); err != nil {
		t.Fatal(err)
	}
	want := verdict.Judge(nil, slices.Clone(fs), []incident.Incident{a, r.Incident}, o)
	rep, err := l.Report(ctx, o.From, o.To, o, true)
	if err != nil {
		t.Fatal(err)
	}
	if g, w := norm(rep.Rules), norm(want.Rules); !slices.EqualFunc(g, w, func(a, b verdict.RuleResult) bool {
		return a.Rule == b.Rule && a.Firings == b.Firings && a.White == b.White && a.Unrelated == b.Unrelated && a.Unrelated == 0
	}) {
		t.Errorf("rules\n got %+v\nwant %+v", g, w)
	}
}

// A bulk answer is one write, one change and one count, all or nothing.
func TestVerdictsBulk(t *testing.T) {
	ctx := context.Background()
	l, fs := verdictLedger(t)
	ks := []FiringKey{keyOfFiring(fs["Disk"]), keyOfFiring(fs["Errors"]), keyOfFiring(fs["Search"])}
	cs, _ := l.Changes(ctx, 100)
	n, err := l.SetVerdicts(ctx, ks, "yes", text("paged"), editorBy(), opts)
	if err != nil || n != 3 {
		t.Fatalf("%d %v", n, err)
	}
	after, _ := l.Changes(ctx, 100)
	if len(after) != len(cs)+1 || !slices.ContainsFunc(after, func(c Change) bool {
		return strings.HasPrefix(c.What, "verdicts: needed action yes, on 3 firings of 3 rules") && c.At.Equal(vNow)
	}) {
		t.Errorf("changes: %+v", after)
	}
	// One of another team's for a team's editor refuses it all.
	shop := teamCtx(l, t, "shop")
	if _, err := l.SetVerdicts(shop, ks, "no", nil, editorBy(), opts); !errors.Is(err, ErrNotYourTeam) {
		t.Errorf("a team's editor, another team's among them: %v", err)
	}
	for _, f := range firingsByRule(t, l) {
		if f.Action() != verdict.ActionYes {
			t.Errorf("%s: %q, all or nothing", f.Rule, f.Action())
		}
	}
	if _, err := l.SetVerdicts(ctx, slices.Repeat(ks[:1], MaxBulkVerdicts+1), "no", nil, editorBy(), opts); !errors.As(err, new(ErrInvalid)) {
		t.Errorf("past %d firings: %v", MaxBulkVerdicts, err)
	}
}

// A verdict follows its firing through a run that starts it a step later,
// and is marked moved; a window confirmed takes the window's place in it;
// an incident it now catches turns it white, marked; and one whose span
// changed marks it.
func TestVerdictsMove(t *testing.T) {
	ctx := context.Background()
	l, fs := verdictLedger(t)
	if _, err := l.SetVerdict(ctx, keyOfFiring(fs["Disk"]), VerdictChange{Unrelated: yes(), Action: text("no")}, editorBy(), opts); err != nil {
		t.Fatal(err)
	}
	// Run again, the disk's firing starts a minute later.
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{
		{Rule: "Disk", Labels: map[string]string{"service": "checkout", "instance": "db"}, Start: vAt(14, 6), End: vAt(14, 20)},
		{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: vAt(14, 12), End: vAt(14, 30)},
		{Rule: "Search", Labels: map[string]string{"service": "search"}, Start: vAt(18, 0), End: vAt(18, 10)},
	})
	vs, err := l.VerdictsIn(ctx, t0, t0.Add(24*time.Hour), nil)
	if err != nil || len(vs) != 1 || !vs[0].Start.Equal(vAt(14, 6)) || vs[0].Moved != movedStart || vs[0].Firing == 0 {
		t.Fatalf("after the run: %+v %v", vs, err)
	}
	if d := firingsByRule(t, l)["Disk"]; d.White || !d.SaidUnrelated() || d.Action() != verdict.ActionNo {
		t.Errorf("the disk's firing a minute later: %+v", d)
	}
	// Said again: not marked any more.
	fs = firingsByRule(t, l)
	if v, err := l.SetVerdict(ctx, keyOfFiring(fs["Disk"]), VerdictChange{Unrelated: yes()}, editorBy(), opts); err != nil || v.Moved != "" {
		t.Errorf("said again: %+v %v", v, err)
	}
	// INC-12's span changed: marked.
	if _, err := l.Put(ctx, incident.Incident{ID: "INC-12", Title: "checkout 5xx", Start: vAt(14, 0), End: vAt(14, 40),
		Labels: incident.Labels{"service": {"checkout"}}}, "ana", opts); err != nil {
		t.Fatal(err)
	}
	vs, _ = l.VerdictsIn(ctx, t0, t0.Add(24*time.Hour), nil)
	if len(vs) != 1 || vs[0].Moved != movedSpan+" INC-12" {
		t.Errorf("INC-12's span changed: %+v", vs)
	}
	if format, args := MovedText(vs[0].Moved); format == "" || len(args) != 1 || args[0] != "INC-12" {
		t.Errorf("said as %q %v", format, args)
	}
	// Another incident it catches, which it wasn't said to be unrelated
	// to: white by it.
	if _, err := l.SetVerdict(ctx, keyOfFiring(firingsByRule(t, l)["Disk"]), VerdictChange{Unrelated: yes()}, editorBy(), opts); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Put(ctx, incident.Incident{ID: "INC-15", Title: "db", Start: vAt(14, 3), End: vAt(14, 50),
		Labels: incident.Labels{"service": {"checkout"}}}, "ana", opts); err != nil {
		t.Fatal(err)
	}
	vs, _ = l.VerdictsIn(ctx, t0, t0.Add(24*time.Hour), nil)
	if d := firingsByRule(t, l)["Disk"]; !d.White || len(vs) != 1 || vs[0].Moved != movedCatches+" INC-15" {
		t.Errorf("INC-15 caught: %+v, %+v", d, vs)
	}
	// Its firing gone in a run: on no firing, marked so.
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{
		{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: vAt(14, 12), End: vAt(14, 30)},
	})
	vs, _ = l.VerdictsIn(ctx, t0, t0.Add(24*time.Hour), nil)
	if len(vs) != 1 || vs[0].Moved != movedGone || vs[0].Firing != 0 {
		t.Errorf("its firing gone: %+v", vs)
	}
	moved, old, err := l.MovedVerdicts(ctx, 10, vNow.Add(-VerdictAge))
	if err != nil || len(moved) != 1 || old != 0 {
		t.Errorf("moved: %+v %d %v", moved, old, err)
	}
}

// Two firings of one alert, each said unrelated, one answered: a run that
// merges them leaves the later one's verdict held by the earlier's firing;
// a run that parts them again gives it its own firing back, not held, its
// answer counted, and the earlier firing no longer has its word. A replay
// judged in memory agrees at each step.
func TestVerdictHeldThenOwnAgain(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if _, err := l.Put(ctx, incident.Incident{ID: "INC-12", Title: "checkout 5xx", Start: vAt(14, 10), End: vAt(14, 40),
		Labels: incident.Labels{"service": {"checkout"}}}, "ana", opts); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Put(ctx, incident.Incident{ID: "INC-13", Title: "checkout db", Start: vAt(14, 15), End: vAt(14, 40),
		Labels: incident.Labels{"service": {"checkout"}}}, "ana", opts); err != nil {
		t.Fatal(err)
	}
	lab := map[string]string{"service": "checkout", "instance": "db"}
	two := []firing.Firing{
		{Rule: "Disk", Labels: lab, Start: vAt(14, 5), End: vAt(14, 8)},
		{Rule: "Disk", Labels: lab, Start: vAt(14, 12), End: vAt(14, 20)},
	}
	one := []firing.Firing{{Rule: "Disk", Labels: lab, Start: vAt(14, 5), End: vAt(14, 20)}}
	record(t, l, t0, t0.Add(24*time.Hour), two)
	fs, err := l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(24 * time.Hour)})
	if err != nil || len(fs) != 2 {
		t.Fatalf("%+v %v", fs, err)
	}
	for _, f := range fs {
		c := VerdictChange{Unrelated: yes()}
		if f.Start.Equal(vAt(14, 12)) {
			c.Action = text("no")
		}
		if _, err := l.SetVerdict(ctx, keyOfFiring(f), c, editorBy(), opts); err != nil {
			t.Fatal(err)
		}
	}
	agrees := func(step string) {
		t.Helper()
		o := opts
		o.From, o.To = t0, t0.Add(7*24*time.Hour)
		var err error
		if o.Verdicts, err = l.ReplayVerdicts(ctx, o.From, o.To); err != nil {
			t.Fatal(err)
		}
		incs, err := l.Incidents(ctx, o.From, o.To, true)
		if err != nil {
			t.Fatal(err)
		}
		var in []incident.Incident
		for _, x := range incs {
			in = append(in, x.Incident)
		}
		got, err := l.Firings(ctx, FiringQuery{From: o.From, To: o.To})
		if err != nil {
			t.Fatal(err)
		}
		var replayed []firing.Firing
		for _, f := range got {
			replayed = append(replayed, f.Firing)
		}
		want := verdict.Judge(nil, replayed, in, o)
		rep, err := l.Report(ctx, o.From, o.To, o, true)
		if err != nil {
			t.Fatal(err)
		}
		stones := func(js []verdict.Judged) []string {
			var out []string
			for _, j := range js {
				out = append(out, j.Start.Format("15:04")+" "+strings.Join(j.Incidents, ",")+"|"+strings.Join(j.Unrelated, ",")+"|"+j.Action)
			}
			slices.Sort(out)
			return out
		}
		if g, w := stones(rep.Firings), stones(want.Firings); !slices.Equal(g, w) {
			t.Errorf("%s: the ledger's stones %v, a replay's %v", step, g, w)
		}
	}
	record(t, l, t0, t0.Add(24*time.Hour), one)
	vs, _ := l.VerdictsIn(ctx, t0, t0.Add(24*time.Hour), nil)
	if len(vs) != 2 || !strings.HasPrefix(vs[1].Moved, movedHeld+" ") {
		t.Fatalf("merged: %+v", vs)
	}
	agrees("merged")
	record(t, l, t0, t0.Add(24*time.Hour), two)
	vs, _ = l.VerdictsIn(ctx, t0, t0.Add(24*time.Hour), nil)
	if len(vs) != 2 || vs[0].Moved != "" || vs[1].Moved != "" || vs[1].Firing == 0 {
		t.Errorf("parted again: %+v", vs)
	}
	fs, _ = l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(24 * time.Hour)})
	for _, f := range fs {
		action := ""
		if f.Start.Equal(vAt(14, 12)) {
			action = "no"
		}
		if f.White || !f.SaidUnrelated() || f.Action() != action {
			t.Errorf("parted again, %s: white %t, unrelated %v, action %q", f.Start.Format("15:04"), f.White, f.Unrelated, f.Action())
		}
	}
	agrees("parted again")
}

// A change of one verdict gives back the firing it was noted of, whatever
// its rule's name and labels hold: braces, quotes, " at ".
func TestVerdictOnRoundTrip(t *testing.T) {
	for _, f := range []Firing{
		{Firing: firing.Firing{Rule: "Disk", Labels: map[string]string{"service": "checkout"}, Start: vAt(14, 5)}},
		{Firing: firing.Firing{Rule: "A {b}", Start: vAt(1, 0)}},
		{Firing: firing.Firing{Rule: `X {"a":"b"}`, Labels: map[string]string{"a": " {}", "b": "at 2026-11-02T14:05:00Z"}, Start: vAt(2, 0)}},
		{Firing: firing.Firing{Rule: "high at night", Labels: map[string]string{"x": `"} {"`}, Start: vAt(23, 59)}},
	} {
		for _, what := range []string{"verdict on " + firingName(f), "verdict on " + firingName(f) + verdictUndone, "verdict on " + firingName(f) + verdictTakenAway} {
			rule, alert, start, ok := verdictOn(what)
			if !ok || rule != f.Rule || alert != alertName(f.Rule, f.Labels) || !start.Equal(f.Start) {
				t.Errorf("%q: %q %q %s %t", what, rule, alert, start, ok)
			}
		}
	}
	for _, what := range []string{"verdicts: needed action yes, on 3 firings of 3 rules", "verdict on Disk at 2026-11-02T14:05:00Z", "team shop kept"} {
		if _, _, _, ok := verdictOn(what); ok {
			t.Errorf("%q read as one verdict's", what)
		}
	}
}

// Writing "unrelated", or undoing it, takes away the verdicts another held
// of that firing: each is noted in the changes as taken away, with what it
// said, by the writer; SaidSince sees it.
func TestVerdictHeldTakenAwayNoted(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if _, err := l.Put(ctx, incident.Incident{ID: "INC-12", Title: "checkout 5xx", Start: vAt(14, 10), End: vAt(14, 40),
		Labels: incident.Labels{"service": {"checkout"}}}, "ana", opts); err != nil {
		t.Fatal(err)
	}
	lab := map[string]string{"service": "checkout", "instance": "db"}
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{
		{Rule: "Disk", Labels: lab, Start: vAt(14, 5), End: vAt(14, 8)},
		{Rule: "Disk", Labels: lab, Start: vAt(14, 12), End: vAt(14, 20)},
	})
	fs, _ := l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(24 * time.Hour)})
	for _, f := range fs {
		if _, err := l.SetVerdict(ctx, keyOfFiring(f), VerdictChange{Unrelated: yes(), UnrelatedNote: text("rotation")}, editorBy(), opts); err != nil {
			t.Fatal(err)
		}
	}
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{{Rule: "Disk", Labels: lab, Start: vAt(14, 5), End: vAt(14, 20)}})
	merged := firingsByRule(t, l)["Disk"]
	undo := VerdictBy{By: "root", Admin: true, Now: vNow.Add(time.Hour)}
	if err := l.DeleteVerdict(ctx, keyOfFiring(merged), "unrelated", undo, opts); err != nil {
		t.Fatal(err)
	}
	if vs, _ := l.VerdictsIn(ctx, t0, t0.Add(24*time.Hour), nil); len(vs) != 0 {
		t.Errorf("left: %+v", vs)
	}
	cs, err := l.Changes(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(cs, func(c Change) bool { return strings.HasSuffix(c.What, "at 2026-11-02T14:12:00Z taken away") })
	if i < 0 || cs[i].By != "root" || !strings.Contains(cs[i].Before, `"unrelated":["INC-12"]`) || !strings.Contains(cs[i].Before, "rotation") || cs[i].After != "" {
		t.Errorf("the changes: %+v", cs)
	}
	if ok, err := l.SaidSince(ctx, vAt(14, 10), vAt(14, 15), vNow.Add(30*time.Minute), nil); err != nil || !ok {
		t.Errorf("the one taken away, since: %t %v", ok, err)
	}
}

// A window confirmed: the incident it writes takes the window's place in
// the verdicts naming it, which stay as they were, unmarked.
func TestVerdictWindowConfirmed(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	o := opts
	window := incident.Incident{ID: "impact-20261102T1410Z", Start: vAt(14, 10), End: vAt(14, 40), Labels: incident.Labels{"service": {"checkout"}}, Candidate: true}
	f := Found{From: t0, To: t0.Add(24 * time.Hour), Firings: []firing.Firing{
		{Rule: "Disk", Labels: map[string]string{"service": "checkout"}, Start: vAt(14, 5), End: vAt(14, 20)},
	}, Impact: []incident.Incident{window}, ImpactOn: true, ImpactSince: t0, ImpactCriterion: "a",
		Run: Run{Kind: "test", Started: t0.Add(24 * time.Hour), Finished: t0.Add(24 * time.Hour), From: t0, To: t0.Add(24 * time.Hour)}}
	if err := l.Record(ctx, f, o); err != nil {
		t.Fatal(err)
	}
	incs, err := l.Incidents(ctx, t0, t0.Add(24*time.Hour), true)
	if err != nil || len(incs) != 1 {
		t.Fatalf("windows: %+v %v", incs, err)
	}
	key := incs[0].ID
	d := firingsByRule(t, l)["Disk"]
	if _, err := l.SetVerdict(ctx, keyOfFiring(d), VerdictChange{Unrelated: yes()}, editorBy(), o); err != nil {
		t.Fatal(err)
	}
	made, err := l.Decide(ctx, key, "confirmed", "", "ana", o)
	if err != nil {
		t.Fatal(err)
	}
	vs, _ := l.VerdictsIn(ctx, t0, t0.Add(24*time.Hour), nil)
	if len(vs) != 1 || !slices.Equal(vs[0].Keys(), []string{made}) || vs[0].Moved != "" {
		t.Errorf("after confirming: %+v", vs)
	}
	if d := firingsByRule(t, l)["Disk"]; d.White || !slices.Equal(d.Unrelated, []string{made}) {
		t.Errorf("the disk's firing after confirming: %+v", d)
	}
}

// --keep prunes a verdict with its firing, and one on no firing as old.
func TestVerdictsPruned(t *testing.T) {
	ctx := context.Background()
	l, fs := verdictLedger(t)
	if _, err := l.SetVerdict(ctx, keyOfFiring(fs["Search"]), VerdictChange{Action: text("yes")}, editorBy(), opts); err != nil {
		t.Fatal(err)
	}
	if err := l.Prune(ctx, vAt(17, 0)); err != nil {
		t.Fatal(err)
	}
	if vs, _ := l.VerdictsIn(ctx, t0, t0.Add(24*time.Hour), nil); len(vs) != 1 {
		t.Errorf("kept: %+v", vs)
	}
	if err := l.Prune(ctx, vAt(19, 0)); err != nil {
		t.Fatal(err)
	}
	if vs, _ := l.VerdictsIn(ctx, t0, t0.Add(24*time.Hour), nil); len(vs) != 0 {
		t.Errorf("pruned: %+v", vs)
	}
}

// An entry that says judged: false keeps its rules from every catch,
// written incidents included, and the report counts their firings not
// judged.
func TestNotJudgedInLedger(t *testing.T) {
	ctx := context.Background()
	l, _ := verdictLedger(t)
	o := opts
	o.Signals = verdict.NewSignals(nil, []verdict.RuleEntry{{Rules: "^Disk$", Judged: no()}})
	if err := l.JudgeAll(ctx, o, nil); err != nil {
		t.Fatal(err)
	}
	rep, err := l.Report(ctx, t0, t0.Add(7*24*time.Hour), o, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rep.Rules {
		if r.Rule == "Disk" && (r.White != 0 || r.NotJudged != 1 || r.Unjudged != 1 || !r.NoRecord) {
			t.Errorf("Disk: %+v", r)
		}
		if r.Rule == "Errors" && (r.White != 1 || r.NotJudged != 0) {
			t.Errorf("Errors: %+v", r)
		}
	}
	for _, j := range rep.Firings {
		if j.Rule == "Disk" && (!j.NotJudged || !j.NoRecord || j.White) {
			t.Errorf("Disk's firing: %+v", j)
		}
	}
	rec, err := l.RecordOf(ctx, t0, t0.Add(7*24*time.Hour), o, nil)
	if err != nil {
		t.Fatal(err)
	}
	if l.FiringJudged(rec, o, "Disk", vAt(14, 5), 0) || !l.FiringJudged(rec, o, "Search", vAt(18, 0), 0) {
		t.Error("FiringJudged: Disk is judged by nothing, Search by the week's record")
	}
}

// With people's verdicts on a third of the white firings and some black
// ones, the ledger's stones are verdict.Judge's given the same verdicts
// (ReplayVerdicts): what serve keeps and what it replays agree.
func TestVerdictsSameAsJudge(t *testing.T) {
	ctx := context.Background()
	for seed := uint64(1); seed <= 3; seed++ {
		l := open(t, time.UTC)
		fs, incs := world(seed, 300)
		incs = append([]incident.Incident{keepAll}, incs...)
		if _, err := l.SyncFile(ctx, incs, opts); err != nil {
			t.Fatal(err)
		}
		record(t, l, t0, t0.Add(7*24*time.Hour), fs)
		got, err := l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(7 * 24 * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		w := VerdictBy{By: "root", Admin: true, Now: t0.Add(8 * 24 * time.Hour)}
		said := 0
		for i, f := range got {
			c := VerdictChange{}
			if f.White && i%3 == 0 {
				c.Unrelated = yes()
			}
			if i%4 == 0 {
				c.Action = text([]string{"yes", "no"}[i%8/4])
			}
			if c.Unrelated == nil && c.Action == nil {
				continue
			}
			if _, err := l.SetVerdict(ctx, keyOfFiring(f), c, w, opts); err != nil {
				if errors.Is(err, ErrTooManyVerdicts) {
					if _, err := l.w.ExecContext(ctx, "UPDATE changes SET at = at - 3601"); err != nil {
						t.Fatal(err)
					}
					if _, err = l.SetVerdict(ctx, keyOfFiring(f), c, w, opts); err == nil {
						said++
						continue
					}
				}
				t.Fatalf("seed %d, firing %d: %v", seed, i, err)
			}
			said++
		}
		if said < 20 {
			t.Fatalf("seed %d: %d verdicts said", seed, said)
		}
		o := opts
		o.From, o.To = t0, t0.Add(7*24*time.Hour)
		if o.Verdicts, err = l.ReplayVerdicts(ctx, o.From, o.To); err != nil {
			t.Fatal(err)
		}
		want := verdict.Judge(nil, slices.Clone(fs), incs, o)
		rep, err := l.Report(ctx, o.From, o.To, o, true)
		if err != nil {
			t.Fatal(err)
		}
		if g, w := norm(rep.Rules), norm(want.Rules); !slices.EqualFunc(g, w, func(a, b verdict.RuleResult) bool {
			return a.Rule == b.Rule && a.Firings == b.Firings && a.White == b.White && a.Unrelated == b.Unrelated &&
				a.ActionYes == b.ActionYes && a.ActionNo == b.ActionNo && a.Incidents == b.Incidents && a.Unjudged == b.Unjudged
		}) {
			t.Errorf("seed %d rules\n got %+v\nwant %+v", seed, g, w)
		}
		stones := func(js []verdict.Judged) []string {
			var out []string
			for _, j := range js {
				out = append(out, j.Rule+" "+j.Start.String()+" "+firingLabelsText(j.Labels)+" "+strings.Join(j.Incidents, ",")+"|"+strings.Join(j.Unrelated, ",")+"|"+j.Action)
			}
			slices.Sort(out)
			return out
		}
		if g, w := stones(rep.Firings), stones(want.Firings); !slices.Equal(g, w) {
			t.Errorf("seed %d stones differ:\n got %v\nwant %v", seed, g, w)
		}
		for i, in := range rep.Incidents {
			if !slices.EqualFunc(in.CaughtBy, want.Incidents[i].CaughtBy, func(a, b verdict.Catch) bool { return a.Rule == b.Rule && a.Delay == b.Delay }) {
				t.Errorf("seed %d %s caught by %+v, want %+v", seed, in.ID, in.CaughtBy, want.Incidents[i].CaughtBy)
			}
		}
	}
}
