package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/verdict"
)

// Declared configuration (docs/design/serveops.md): the teams, quiet
// windows, reports and roles that lapilli serve's --config declares, kept
// in the ledger as the UI's are, marked declared. The UI, the API and MCP
// show them and change none of them (ErrDeclared): the file alone does,
// when Reconcile makes the ledger hold what it says. One the file no
// longer names is the UI's again, or, pruned, deleted.

// ConfigBy is who the changes Reconcile makes are noted as, in the
// activity log: the file, not a person. No one signs in as it.
const ConfigBy = "config"

// ErrDeclared is a change to something --config declares, made elsewhere
// than in the file.
var ErrDeclared = errors.New("declared in config — change it in values")

// ErrNamedByDeclared is a team deleted or renamed that something --config
// declares names: the file would name a team that isn't there.
var ErrNamedByDeclared = errors.New("a quiet window, a report, a role or a goal declared in config names this team: change them in values first")

// isDeclared refuses, in a write, a change to a row of table that --config
// declares. A row that isn't there is the caller's to say.
func isDeclared(t *txn, table string, id int64) error {
	var d bool
	if err := t.QueryRowContext(t.ctx, "SELECT declared FROM "+table+" WHERE id = ?", id).Scan(&d); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if d {
		return ErrDeclared
	}
	return nil
}

// MaxDeclaredTeams is how many teams --config declares at most. Quiet
// windows, reports and roles are bounded as the UI's are (MaxWindows,
// MaxScheduled, MaxGrants), apart from them: what a file declares isn't
// counted against what the UI may make, nor the UI's against it, so
// neither crowds the other out.
const MaxDeclaredTeams = 1000

// DeclaredTeam is a team --config declares, and the line it starts on.
// RenamedFrom, when set, is the name the file gave it before: the team of
// that name is renamed, and what names it (windows, tasks, notes, roles)
// follows it. Bad is why its caller found it can't be kept; it is then
// kept as it was.
type DeclaredTeam struct {
	Team
	RenamedFrom string
	Line        int
	Bad         error
}

// DeclaredWindow is a quiet window --config declares: Team is its team's
// name, "" for everyone's.
type DeclaredWindow struct {
	Window
	Team string
	Line int
	Bad  error
}

// DeclaredReport is a report --config declares: Team is its team's name,
// "" for everyone's.
type DeclaredReport struct {
	Scheduled
	Team string
	Line int
	Bad  error
}

// DeclaredGrant is a role --config declares: an editor's, given to Who
// (user:NAME or group:NAME), of Team ("" for every team).
type DeclaredGrant struct {
	Who, Team string
	Line      int
	Bad       error
}

// Declared is what --config declares.
type Declared struct {
	Teams   []DeclaredTeam
	Windows []DeclaredWindow
	Reports []DeclaredReport
	Grants  []DeclaredGrant
	// Goals are its goals (docs/design/goals.md), which only a file
	// declares: one it no longer declares is deleted.
	Goals []DeclaredGoal
}

// Kinds of what --config declares, as a DeclaredError names them.
const (
	KindTeam   = "team"
	KindWindow = "window"
	KindReport = "report"
	KindGrant  = "grant"
)

// DeclaredError is what is wrong with something --config declares: its
// kind, the line it starts on, its name, and why.
type DeclaredError struct {
	Kind string
	Line int
	Name string
	Err  error
}

func (e DeclaredError) Error() string {
	if e.Name == "" {
		return fmt.Sprintf("line %d: %v", e.Line, e.Err)
	}
	return fmt.Sprintf("line %d (%s): %v", e.Line, e.Name, e.Err)
}

func (e DeclaredError) Unwrap() error { return e.Err }

// Leave says what becomes of what is declared that the file no longer
// names. Its caller keeps the ledger to a file only once it has read the
// same file twice in a row, so that one read half written changes
// nothing: what the file no longer names is let go at once.
type Leave struct {
	// Prune deletes it (--config-prune); a team something still names is
	// given back to the UI instead.
	Prune bool
	// NoFile says there is no file at all (no --config): everything
	// declared, roles included, is given back to the UI, whatever the UI
	// holds, and nothing is deleted; each is marked given back (GivenBack),
	// for the next file. A file that isn't given says nothing of who may
	// change what; a chart rolled back to before --config takes no one's
	// role away, and one rolled forward again takes away what its file no
	// longer gives.
	NoFile bool
}

// fate is what becomes of one thing the file no longer names.
type fate int

const (
	// fateStay leaves it as it is: given back already, with no file.
	fateStay fate = iota
	// fateGiveBack makes it the UI's.
	fateGiveBack
	// fateMark makes it the UI's, marked given back (GivenBack).
	fateMark
	fateDelete
)

// of is the fate of a team, a quiet window or a report: givenBack says a
// leader without --config gave it back to the UI, marked; atMost says the
// UI holds its own most already, so one declared isn't given back.
func (lv Leave) of(givenBack, atMost bool) fate {
	switch {
	case lv.NoFile && givenBack:
		return fateStay
	case lv.NoFile:
		return fateMark
	case lv.Prune, atMost && !givenBack:
		return fateDelete
	}
	return fateGiveBack
}

// role is the fate of a role: taken away, as the file no longer gives it,
// one given back without a file among them.
func (lv Leave) role(givenBack bool) fate {
	switch {
	case lv.NoFile && givenBack:
		return fateStay
	case lv.NoFile:
		return fateMark
	}
	return fateDelete
}

// Reconciled is what Reconcile did: what it made, changed, took over from
// the UI (declared, as it was or as the file says it), gave back to the
// UI, deleted, and kept declared though the file no longer names it (a
// team something names, past the teams the UI keeps).
type Reconciled struct {
	Made, Changed, Adopted, Undeclared, Deleted, Kept int
}

// Any says it wrote anything.
func (r Reconciled) Any() bool { return r.Made+r.Changed+r.Adopted+r.Undeclared+r.Deleted > 0 }

// errUnchanged takes back a write that changed nothing.
var errUnchanged = errors.New("unchanged")

// Reconcile makes the ledger hold what d declares, in one write. A thing
// it declares that can't be kept (its own check refuses it, its team
// isn't there, its caller found it Bad) is skipped, and said, and what
// the ledger held of it is kept as it was; the rest is kept, and what the
// file no longer names is given back or deleted as left says, always,
// before anything is added, so that a file that replaces what it declares
// isn't refused as full. A role is taken away (RoleTaken), not given back:
// declared, it was the file's to give; with no file at all, it is given
// back. What a leader without --config gave back, marked, the file
// doesn't declare again is let go as if it were declared.
//
// Each team is matched by its name, or else in any letter case when one
// team alone has it, or by RenamedFrom; each report by its name; each role
// by whom it is given to and its team; each quiet window by its title,
// those alike first, then the declared of its team, then of another, then
// the UI's of its team. What the UI made is taken over only of the same
// team: a report the UI made for another team under its name is skipped, a
// quiet window made beside it.
// Each change is noted in the activity log as ConfigBy's; a file that says
// what the ledger holds writes nothing. The quiet windows it changes are
// judged again with o.
//
// The checks are those of the UI's writes (PutTeam, PutWindow,
// PutScheduled, PutGrant), the costly ones made before the write; what the
// server checks besides (a report's destinations, a team's on-call shifts)
// its caller does, as Bad.
func (l *Ledger) Reconcile(ctx context.Context, d Declared, left Leave, o verdict.Options) (Reconciled, []DeclaredError, error) {
	switch {
	case len(d.Teams) > MaxDeclaredTeams:
		return Reconciled{}, nil, invalid("a config declares %d teams at most", MaxDeclaredTeams)
	case len(d.Windows) > MaxWindows:
		return Reconciled{}, nil, invalid("a config declares %d quiet windows at most", MaxWindows)
	case len(d.Reports) > MaxScheduled:
		return Reconciled{}, nil, invalid("a config declares %d reports at most", MaxScheduled)
	case len(d.Grants) > MaxGrants:
		return Reconciled{}, nil, invalid("a config declares %d roles at most", MaxGrants)
	case len(d.Goals) > MaxGoals:
		return Reconciled{}, nil, invalid("a config declares %d goals at most", MaxGoals)
	}
	// The checks of each that don't read the ledger, before the write.
	d.Teams, d.Windows = slices.Clone(d.Teams), slices.Clone(d.Windows)
	for i := range d.Teams {
		if d.Teams[i].Bad == nil {
			d.Teams[i].Bad = checkTeam(&d.Teams[i].Team)
		}
	}
	for i := range d.Windows {
		if w := &d.Windows[i]; w.Bad == nil {
			w.ID, w.TeamID = 0, 0
			w.Bad = l.checkWindow(&w.Window)
		}
	}
	var r Reconciled
	var skipped []DeclaredError
	var spans [][2]int64
	err := l.write(ctx, func(t *txn) error {
		r, skipped, spans = Reconciled{}, nil, nil
		skip := func(kind string, line int, name string, err error) {
			skipped = append(skipped, DeclaredError{kind, line, name, err})
		}
		leftTeams, deferred, refused, err := l.declareTeams(t, d.Teams, &r, skip)
		if err != nil {
			return err
		}
		team, err := l.teamResolver(t, refused)
		if err != nil {
			return err
		}
		if spans, err = l.declareWindows(t, d.Windows, team, left, &r, skip); err != nil {
			return err
		}
		if err := l.declareReports(t, d.Reports, team, left, &r, skip); err != nil {
			return err
		}
		if err := l.declareGrants(t, d.Grants, team, left, &r, skip); err != nil {
			return err
		}
		if err := l.declareGoals(t, d.Goals, team, &r, skip); err != nil {
			return err
		}
		// The teams last: what names one the file no longer does is known.
		if err := l.leaveTeams(t, leftTeams, left, &r, skip); err != nil {
			return err
		}
		// Teams a full ledger couldn't make before those were let go: what
		// names them is tried again as the ledger changes (its caller's).
		for _, dt := range deferred {
			if err := fullOf(t, "teams"); err != nil {
				skip(KindTeam, dt.Line, dt.Name, err)
				continue
			}
			if _, err := l.putTeam(t, 0, dt.Team, ConfigBy, true, "team "+dt.Name+" kept"); err != nil {
				if invalidErr(err) {
					skip(KindTeam, dt.Line, dt.Name, err)
					continue
				}
				return err
			}
			r.Made++
		}
		if !r.Any() {
			// Nothing to keep: no write, and no page read again.
			return errUnchanged
		}
		return pend(t, spans)
	})
	if errors.Is(err, errUnchanged) {
		return r, skipped, nil
	}
	if err != nil {
		return Reconciled{}, nil, err
	}
	if len(spans) > 0 {
		l.judgeSoon(ctx, o)
	}
	return r, skipped, nil
}

// sameTeam says two teams are kept alike.
func sameTeam(a, b Team) bool {
	return a.Name == b.Name && a.Rules == b.Rules && labelsText(a.Labels) == labelsText(b.Labels) && a.Note == b.Note &&
		a.WorkHours == b.WorkHours && a.SleepHours == b.SleepHours && a.TZ == b.TZ && a.Shifts == b.Shifts
}

// setDeclared marks a row of table declared or not, given back no more,
// and notes it.
func setDeclared(t *txn, table string, id int64, declared bool, what string, after any) error {
	if _, err := t.ExecContext(t.ctx, "UPDATE "+table+" SET declared = ?, given_back = 0 WHERE id = ?", declared, id); err != nil {
		return err
	}
	return change(t, ConfigBy, what, nil, after)
}

// markGivenBack gives a row of table back to the UI, as no --config does,
// marked given back for the next file, and notes it.
func markGivenBack(t *txn, table string, id int64, what string, after any) error {
	if _, err := t.ExecContext(t.ctx, "UPDATE "+table+" SET declared = 0, given_back = 1 WHERE id = ?", id); err != nil {
		return err
	}
	return change(t, ConfigBy, what, nil, after)
}

// givenBackNow is the activity log's words for a row given back without
// --config.
const givenBackNow = " given back to the UI, as serve runs without --config"

// The activity log's words for a row taken over from the UI, and for one
// the file no longer names, given back.
const (
	declaredNow   = " declared in config"
	undeclaredNow = " no longer declared in config: the UI's to change"
)

// invalidErr says err is the ledger's refusal of what is written, not a
// failure of the ledger's: an item skipped.
func invalidErr(err error) bool {
	var inv ErrInvalid
	return errors.As(err, &inv)
}

// declareTeams makes, changes and takes over the teams the file declares,
// and gives back the declared teams it no longer names, unless something
// it couldn't keep is of that name: those are left to leaveTeams.
//
// Each is matched by its name first, every one of them, then those no
// team has the name of by their renamed_from, so that the order they come
// in changes nothing. What can't be told apart is skipped, said why, and
// the teams it names kept as they were: a name and a renamed_from that
// are two teams, one old name given twice, an old name another declares
// by its name.
//
// It returns the declared teams the file no longer names (leftover), the
// new ones a full ledger couldn't make before those are let go (deferred),
// and, by folded name, the names of those skipped (refused), which nothing
// the file declares is taken to name.
func (l *Ledger) declareTeams(t *txn, want []DeclaredTeam, r *Reconciled, skip func(string, int, string, error)) (leftover, deferred []DeclaredTeam, refused map[string]DeclaredError, err error) {
	have, err := teams(t.ctx, t)
	if err != nil {
		return nil, nil, nil, err
	}
	refused = map[string]DeclaredError{}
	skipTeam := func(dt DeclaredTeam, err error) {
		skip(KindTeam, dt.Line, dt.Name, err)
		if f := FoldName(strings.TrimSpace(dt.Name)); f != "" {
			if _, ok := refused[f]; !ok {
				refused[f] = DeclaredError{KindTeam, dt.Line, dt.Name, err}
			}
		}
	}
	exact, folds := map[string]int{}, map[string][]int{}
	for i, x := range have {
		exact[x.Name] = i
		folds[FoldName(x.Name)] = append(folds[FoldName(x.Name)], i)
	}
	// find is the team of a name, or, in any letter case, the one alone
	// that has it: -1 for none; ambiguous, when more than one does. Read
	// of the teams as the write found them, before it changes any.
	find := func(name string) (i int, ambiguous []string) {
		if i, ok := exact[name]; ok {
			return i, nil
		}
		switch c := folds[FoldName(name)]; len(c) {
		case 0:
			return -1, nil
		case 1:
			return c[0], nil
		default:
			for _, j := range c {
				ambiguous = append(ambiguous, have[j].Name)
			}
			return -1, ambiguous
		}
	}
	matched := make([]bool, len(have))
	keep := func(names ...string) {
		for _, n := range names {
			if i, _ := find(n); n != "" && i >= 0 {
				matched[i] = true
			}
		}
	}
	want = slices.Clone(want)
	ok := make([]bool, len(want))
	seen := map[string]int{}
	for wi := range want {
		dt := &want[wi]
		dt.RenamedFrom = strings.TrimSpace(dt.RenamedFrom)
		if line, dup := seen[FoldName(dt.Name)]; dup {
			keep(dt.Name, dt.RenamedFrom)
			skip(KindTeam, dt.Line, dt.Name, invalid("a team called %s is declared on line %d already: a team's name is unique in any letter case", dt.Name, line))
			continue
		}
		// One that can't be kept holds its name all the same: another of
		// it, in another letter case, is a second.
		seen[FoldName(dt.Name)] = dt.Line
		if dt.Bad != nil {
			keep(dt.Name, dt.RenamedFrom) // kept as they were
			skipTeam(*dt, dt.Bad)
			continue
		}
		ok[wi] = true
	}
	// An old name given twice renames neither.
	olds := map[string][]int{}
	for wi, dt := range want {
		if ok[wi] && dt.RenamedFrom != "" {
			olds[FoldName(dt.RenamedFrom)] = append(olds[FoldName(dt.RenamedFrom)], wi)
		}
	}
	for _, wis := range olds {
		if len(wis) < 2 {
			continue
		}
		for _, wi := range wis {
			other := want[wis[0]].Line
			if wi == wis[0] {
				other = want[wis[1]].Line
			}
			ok[wi] = false
			keep(want[wi].Name, want[wi].RenamedFrom)
			skipTeam(want[wi], invalid("renamed_from %s is given on line %d too: a team is renamed once", want[wi].RenamedFrom, other))
		}
	}
	// By name.
	target := make([]int, len(want))
	for wi, dt := range want {
		target[wi] = -1
		if !ok[wi] {
			continue
		}
		i, ambiguous := find(dt.Name)
		j := -1
		if dt.RenamedFrom != "" {
			j, _ = find(dt.RenamedFrom)
		}
		switch {
		case i >= 0 && j >= 0 && i != j:
			ok[wi] = false
			keep(dt.Name, dt.RenamedFrom)
			skipTeam(dt, invalid("%s and renamed_from %s are two teams: take out renamed_from, or rename or delete the team called %s", dt.Name, dt.RenamedFrom, have[i].Name))
		case i >= 0:
			target[wi], matched[i] = i, true
		case ambiguous != nil && dt.RenamedFrom == "":
			ok[wi] = false
			skipTeam(dt, invalid("the teams %s differ only in letter case: name the one to declare as it is named, or give its name as renamed_from", strings.Join(ambiguous, ", ")))
		}
	}
	// By renamed_from, those no team has the name of.
	for wi, dt := range want {
		if !ok[wi] || target[wi] >= 0 || dt.RenamedFrom == "" {
			continue
		}
		j, ambiguous := find(dt.RenamedFrom)
		switch {
		case ambiguous != nil:
			ok[wi] = false
			skipTeam(dt, invalid("the teams %s differ only in letter case: give renamed_from as the one to rename is named", strings.Join(ambiguous, ", ")))
		case j < 0:
			// No team of either name: a new one.
		case matched[j]:
			ok[wi] = false
			skipTeam(dt, invalid("renamed_from %s is a team the file declares by its name, or keeps as it was: take out renamed_from", dt.RenamedFrom))
		default:
			target[wi], matched[j] = j, true
		}
	}
	for wi, dt := range want {
		if !ok[wi] {
			continue
		}
		i := target[wi]
		if i < 0 {
			if fullOf(t, "teams") != nil {
				// Made once the teams the file no longer names are let go.
				deferred = append(deferred, dt)
				continue
			}
			if _, err := l.putTeam(t, 0, dt.Team, ConfigBy, true, "team "+dt.Name+" kept"); err != nil {
				if invalidErr(err) {
					skipTeam(dt, err)
					continue
				}
				return nil, nil, nil, err
			}
			r.Made++
			continue
		}
		x := have[i]
		renamed := x.Name != dt.Name
		what, n := "team "+dt.Name+" kept", &r.Changed
		if renamed {
			what = "team " + x.Name + " renamed to " + dt.Name
		}
		if !x.Declared {
			n = &r.Adopted
			if !renamed {
				what = "team " + dt.Name + declaredNow
			}
		}
		switch {
		case !sameTeam(x, dt.Team):
			if _, err := l.putTeam(t, x.ID, dt.Team, ConfigBy, true, what); err != nil {
				if invalidErr(err) {
					skipTeam(dt, err)
					continue
				}
				return nil, nil, nil, err
			}
			if renamed && !x.Declared {
				if err := change(t, ConfigBy, "team "+dt.Name+declaredNow, nil, nil); err != nil {
					return nil, nil, nil, err
				}
			}
			*n++
		case !x.Declared:
			if err := setDeclared(t, "teams", x.ID, true, what, nil); err != nil {
				return nil, nil, nil, err
			}
			*n++
		}
	}
	for i, x := range have {
		if !matched[i] && (x.Declared || x.GivenBack) {
			leftover = append(leftover, DeclaredTeam{Team: x})
		}
	}
	return leftover, deferred, refused, nil
}

// leaveTeams gives back or deletes the declared teams the file no longer
// names, and those given back without --config, as left says. One
// something still names (a quiet window, a report, a role, a task, a
// rule's note), the UI's or declared, isn't deleted, but given back to the
// UI, and said why: deleted, it would leave them no one's, and the file,
// put back, would make another team in its place, which they wouldn't
// name. Past the teams the UI keeps (MaxTeams), a declared one isn't given
// back but deleted, or, something naming it, kept declared and said. With
// no file at all, every one is given back, whatever the UI holds.
func (l *Ledger) leaveTeams(t *txn, leftover []DeclaredTeam, left Leave, r *Reconciled, skip func(string, int, string, error)) error {
	ui, _, err := originCounts(t, "teams")
	if err != nil {
		return err
	}
	for _, dt := range leftover {
		x := dt.Team
		switch left.of(x.GivenBack, ui >= MaxTeams) {
		case fateStay:
			continue
		case fateMark:
			if err := markGivenBack(t, "teams", x.ID, "team "+x.Name+givenBackNow, nil); err != nil {
				return err
			}
			ui++
			r.Undeclared++
			continue
		case fateGiveBack:
			if err := setDeclared(t, "teams", x.ID, false, "team "+x.Name+undeclaredNow, nil); err != nil {
				return err
			}
			if !x.GivenBack {
				ui++
			}
			r.Undeclared++
			continue
		}
		var windows, reports, roles, tasks, notes, goals int
		if err := t.QueryRowContext(t.ctx, `SELECT (SELECT count(*) FROM windows WHERE team_id = ?), (SELECT count(*) FROM reports WHERE team_id = ?),
			(SELECT count(*) FROM grants WHERE team_id = ?), (SELECT count(*) FROM tasks WHERE team_id = ?), (SELECT count(*) FROM notes WHERE team_id = ?),
			(SELECT count(*) FROM goals WHERE team_id = ? AND every_team = 0)`,
			x.ID, x.ID, x.ID, x.ID, x.ID, x.ID).Scan(&windows, &reports, &roles, &tasks, &notes, &goals); err != nil {
			return err
		}
		named := windows+reports+roles+tasks+notes+goals > 0
		switch {
		case named && ui >= MaxTeams && !x.GivenBack:
			skip(KindTeam, 0, x.Name, invalid("kept declared, though the file no longer names it: the UI keeps %d teams at most, and what it owns names it", MaxTeams))
			r.Kept++
		case named:
			counts := map[string]int{"quiet windows": windows, "reports": reports, "roles": roles, "tasks": tasks, "notes": notes}
			if goals > 0 {
				counts["goals"] = goals
			}
			if err := setDeclared(t, "teams", x.ID, false, "team "+x.Name+" no longer declared in config, and not deleted: what it owns names it, so it is the UI's to change", counts); err != nil {
				return err
			}
			if !x.GivenBack {
				ui++
			}
			r.Undeclared++
		default:
			what := "team " + x.Name + " deleted"
			if !left.Prune {
				what = fmt.Sprintf("team %s deleted, as the file no longer declares it and the UI keeps %d at most", x.Name, MaxTeams)
			}
			if err := l.deleteTeamAs(t, x.ID, ConfigBy, what); err != nil {
				return err
			}
			r.Deleted++
		}
	}
	return nil
}

// The most of each kind the ledger keeps made in the UI (uiMost), and of
// both kinds together (allMost): what a file declares has its own most,
// as many again, but what it gives back to the UI doesn't make the UI's
// grow past its own.
var (
	uiMost  = map[string]int{"teams": MaxTeams, "windows": MaxWindows, "reports": MaxScheduled, "grants": MaxGrants}
	allMost = map[string]int{"teams": MaxTeams + MaxDeclaredTeams, "windows": 2 * MaxWindows, "reports": 2 * MaxScheduled, "grants": 2 * MaxGrants}
)

// MaxTeams is how many teams the ledger keeps made in the UI.
const MaxTeams = 1000

// originCounts are, in a write, how many rows of a table the UI made, and
// how many there are.
func originCounts(t *txn, table string) (ui, all int, err error) {
	err = t.QueryRowContext(t.ctx, "SELECT count(*) FILTER (WHERE declared = 0), count(*) FROM "+table).Scan(&ui, &all)
	return ui, all, err
}

// full refuses one more row of a table that holds all already.
func full(table string, all int) error {
	if all < allMost[table] {
		return nil
	}
	switch table {
	case "teams":
		return invalid("the ledger keeps %d teams at most, declared and made in the UI together", allMost[table])
	case "windows":
		return invalid("the ledger keeps %d quiet windows at most, declared and made in the UI together", allMost[table])
	case "reports":
		return invalid("the ledger keeps %d reports at most, declared and made in the UI together", allMost[table])
	}
	return invalid("the ledger keeps %d roles at most, declared and made in the UI together", allMost[table])
}

// fullOf refuses, in a write, one more row of a table past what the ledger
// keeps of both kinds together.
func fullOf(t *txn, table string) error {
	_, all, err := originCounts(t, table)
	if err != nil {
		return err
	}
	return full(table, all)
}

// teamResolver finds a team by the name --config gives it, as the ledger
// holds them in a write: by its name, or else in any letter case, when one
// team alone has it; "" is everyone's (0). A name the file declares a team
// by that was skipped (refused) is refused: what names it would otherwise
// be taken to name another team of the name, the UI's. known says id is
// the team of the name as the ledger holds it, refused or not, so that
// what was declared of it can be kept as it was.
func (l *Ledger) teamResolver(t *txn, refused map[string]DeclaredError) (func(name string) (id int64, known bool, err error), error) {
	have, err := teams(t.ctx, t)
	if err != nil {
		return nil, err
	}
	exact, folds := map[string]int64{}, map[string][]Team{}
	for _, x := range have {
		exact[x.Name] = x.ID
		folds[FoldName(x.Name)] = append(folds[FoldName(x.Name)], x)
	}
	return func(name string) (int64, bool, error) {
		if name == "" {
			return 0, true, nil // left out: everyone's, or every team
		}
		if name = strings.TrimSpace(name); name == "" {
			return 0, false, invalid("team has nothing after it: leave the key out for every team")
		}
		id, known := exact[name]
		if !known {
			if c := folds[FoldName(name)]; len(c) == 1 {
				id, known = c[0].ID, true
			}
		}
		if e, ok := refused[FoldName(name)]; ok {
			return id, known, invalid("the team %s, declared on line %d, can't be kept: what names it is kept as it was", e.Name, e.Line)
		}
		switch c := folds[FoldName(name)]; {
		case known:
			return id, true, nil
		case len(c) == 0:
			return 0, false, invalid("there is no team called %s, in the config or the ledger", name)
		}
		return 0, false, invalid("more than one team is called %s in some letter case: name it as one of them is", name)
	}, nil
}

// sameWindow says two quiet windows are kept alike.
func sameWindow(a, b Window) bool {
	return a.Title == b.Title && sameStones(a, b)
}

// resolver is a teamResolver's.
type resolver = func(name string) (id int64, known bool, err error)

func (l *Ledger) declareWindows(t *txn, want []DeclaredWindow, team resolver, left Leave, r *Reconciled,
	skip func(string, int, string, error)) ([][2]int64, error) {
	ws := make([]Window, len(want))
	bad := make([]error, len(want))
	known := make([]bool, len(want)) // its team found, refused or not
	for i, dw := range want {
		w := dw.Window
		id, k, err := team(dw.Team)
		if k {
			w.TeamID, known[i] = id, true
		}
		w.loc = l.loc
		ws[i], bad[i] = w, errors.Join(dw.Bad, err)
	}
	have, err := l.windows(t.ctx, t, "ORDER BY id")
	if err != nil {
		return nil, err
	}
	// Those of each title, the file's first (declared, or given back
	// without --config), each by its id.
	byTitle := map[string][]int{}
	for _, file := range []bool{true, false} {
		for i, h := range have {
			if (h.Declared || h.GivenBack) == file {
				byTitle[h.Title] = append(byTitle[h.Title], i)
			}
		}
	}
	matched := make([]bool, len(have))
	pair := make([]int, len(ws))
	for i := range pair {
		pair[i] = -1
	}
	// match pairs each window that can be kept with one the ledger holds
	// of its title, as stage says: one alike (0); one of the file's of the
	// same team (1), then of another, moved by the file (2); then, of the
	// same team alone, one the UI made, taken over (3). A window taken out
	// of a file of several with one title leaves the others as they were,
	// and what the file declares is kept in the rows it was before any of
	// the UI's is taken.
	match := func(stage int) {
		for wi, w := range ws {
			if pair[wi] >= 0 || bad[wi] != nil {
				continue
			}
			for _, hi := range byTitle[w.Title] {
				if matched[hi] {
					continue
				}
				h := have[hi]
				file := h.Declared || h.GivenBack
				if stage == 0 && sameWindow(h, w) || stage == 1 && file && h.TeamID == w.TeamID || stage == 2 && file ||
					stage == 3 && h.TeamID == w.TeamID {
					pair[wi], matched[hi] = hi, true
					break
				}
			}
		}
	}
	// Those alike first: a window the file keeps as it was keeps its row,
	// which one of its title that can't be kept doesn't take.
	match(0)
	// One that can't be kept keeps one of the file's of its title and its
	// team as it was, before any other is matched, which would take it for
	// its own; of a team that isn't there, none: kept, another team's
	// window, or everyone's, would go on quieting what the file no longer
	// says to.
	for wi, w := range ws {
		if bad[wi] == nil {
			continue
		}
		skip(KindWindow, want[wi].Line, want[wi].Title, bad[wi])
		if !known[wi] {
			continue
		}
		for _, hi := range byTitle[strings.TrimSpace(want[wi].Title)] {
			if h := have[hi]; !matched[hi] && (h.Declared || h.GivenBack) && h.TeamID == w.TeamID {
				matched[hi] = true
				break
			}
		}
	}
	for stage := 1; stage < 4; stage++ {
		match(stage)
	}
	now := time.Now()
	var spans [][2]int64
	ui, all, err := originCounts(t, "windows")
	if err != nil {
		return nil, err
	}
	// What the file no longer declares first, so that one that replaces
	// its windows isn't refused as full.
	for i, h := range have {
		if matched[i] || !h.Declared && !h.GivenBack {
			continue
		}
		switch left.of(h.GivenBack, ui >= MaxWindows) {
		case fateStay:
		case fateMark:
			if err := markGivenBack(t, "windows", h.ID, "quiet window "+h.Title+givenBackNow, nil); err != nil {
				return nil, err
			}
			ui++
			r.Undeclared++
		case fateDelete:
			what := "quiet window " + h.Title + " deleted"
			if !left.Prune {
				what = fmt.Sprintf("quiet window %s deleted, as the file no longer declares it and the UI keeps %d at most", h.Title, MaxWindows)
			}
			if _, err := t.ExecContext(t.ctx, "DELETE FROM windows WHERE id = ?", h.ID); err != nil {
				return nil, err
			}
			if err := change(t, ConfigBy, what, nil, nil); err != nil {
				return nil, err
			}
			spans = append(spans, h.span(now))
			all--
			r.Deleted++
		default:
			if err := setDeclared(t, "windows", h.ID, false, "quiet window "+h.Title+undeclaredNow, nil); err != nil {
				return nil, err
			}
			if !h.GivenBack {
				ui++
			}
			r.Undeclared++
		}
	}
	for wi, w := range ws {
		if bad[wi] != nil {
			continue
		}
		if pair[wi] < 0 {
			if err := full("windows", all); err != nil {
				skip(KindWindow, want[wi].Line, w.Title, err)
				continue
			}
			all++
			if _, err := l.insertWindow(t, w, ConfigBy, true); err != nil {
				return nil, err
			}
			if err := change(t, ConfigBy, "quiet window "+w.Title+" kept", nil, w); err != nil {
				return nil, err
			}
			spans = append(spans, w.span(now))
			r.Made++
			continue
		}
		h := have[pair[wi]]
		what, n := "quiet window "+w.Title+" kept", &r.Changed
		if !h.Declared {
			what, n = "quiet window "+w.Title+declaredNow, &r.Adopted
		}
		switch {
		case !sameWindow(h, w):
			if err := updateWindow(t, h.ID, w, ConfigBy, true); err != nil {
				return nil, err
			}
			w.ID = h.ID
			if err := change(t, ConfigBy, what, nil, w); err != nil {
				return nil, err
			}
			if !sameStones(h, w) {
				spans = append(spans, h.span(now), w.span(now))
			}
			*n++
		case !h.Declared:
			if err := setDeclared(t, "windows", h.ID, true, what, nil); err != nil {
				return nil, err
			}
			*n++
		}
	}
	return spans, nil
}

// sameReport says two reports are kept alike.
func sameReport(a, b Scheduled) bool {
	return a.Name == b.Name && a.TeamID == b.TeamID && a.Schedule == b.Schedule && a.Span == b.Span && a.Zone == b.Zone &&
		slices.Equal(a.Sections, b.Sections) && slices.Equal(a.Dests, b.Dests) && a.Enabled == b.Enabled
}

func (l *Ledger) declareReports(t *txn, want []DeclaredReport, team resolver, left Leave, r *Reconciled,
	skip func(string, int, string, error)) error {
	rows, err := t.QueryContext(t.ctx, "SELECT "+reportCols+" FROM reports ORDER BY id")
	if err != nil {
		return err
	}
	var have []Scheduled
	for rows.Next() {
		x, err := scanScheduled(rows)
		if err != nil {
			rows.Close()
			return err
		}
		have = append(have, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	byName := map[string]int{}
	for i, x := range have {
		byName[x.Name] = i
	}
	matched := make([]bool, len(have))
	seen := map[string]int{}
	now := time.Now()
	// Each read and checked, and matched, before anything is written.
	type todo struct {
		dr DeclaredReport
		x  Scheduled
		i  int // -1 for a new one
	}
	var todos []todo
	for _, dr := range want {
		x := dr.Scheduled
		x.ID = 0
		err := dr.Bad
		if err == nil {
			x.TeamID, _, err = team(dr.Team)
		}
		if err == nil {
			err = x.check()
		}
		if err == nil {
			if line, ok := seen[x.Name]; ok {
				err = invalid("a report called %s is declared on line %d already", x.Name, line)
			}
		}
		i, there := byName[strings.TrimSpace(x.Name)]
		if err == nil && there && !have[i].Declared && !have[i].GivenBack && have[i].TeamID != x.TeamID {
			// The UI's, of another team: not taken over.
			err = invalid("a report called %s is the UI's, for another team: rename one of them", x.Name)
			there = false
		}
		if err != nil {
			if there && (have[i].Declared || have[i].GivenBack) {
				matched[i] = true // kept as it was
			}
			skip(KindReport, dr.Line, dr.Name, err)
			continue
		}
		seen[x.Name] = dr.Line
		if !there {
			i = -1
		} else {
			matched[i] = true
		}
		todos = append(todos, todo{dr, x, i})
	}
	ui, all, err := originCounts(t, "reports")
	if err != nil {
		return err
	}
	// What the file no longer declares first, so that one that replaces
	// its reports isn't refused as full.
	for i, h := range have {
		if matched[i] || !h.Declared && !h.GivenBack {
			continue
		}
		switch left.of(h.GivenBack, ui >= MaxScheduled) {
		case fateStay:
		case fateMark:
			if err := markGivenBack(t, "reports", h.ID, "report "+h.Name+givenBackNow, nil); err != nil {
				return err
			}
			ui++
			r.Undeclared++
		case fateDelete:
			what := "report " + h.Name + " deleted"
			if !left.Prune {
				what = fmt.Sprintf("report %s deleted, as the file no longer declares it and the UI keeps %d at most", h.Name, MaxScheduled)
			}
			if _, err := t.ExecContext(t.ctx, "DELETE FROM reports WHERE id = ?", h.ID); err != nil {
				return err
			}
			if _, err := t.ExecContext(t.ctx, "DELETE FROM meta WHERE key = ?", ReportSentKey(h.ID)); err != nil {
				return err
			}
			if err := change(t, ConfigBy, what, nil, nil); err != nil {
				return err
			}
			all--
			r.Deleted++
		default:
			if err := setDeclared(t, "reports", h.ID, false, "report "+h.Name+undeclaredNow, nil); err != nil {
				return err
			}
			if !h.GivenBack {
				ui++
			}
			r.Undeclared++
		}
	}
	for _, td := range todos {
		x := td.x
		span, _ := x.span()
		sections, _ := json.Marshal(x.Sections)
		dests, _ := json.Marshal(x.Dests)
		if td.i < 0 {
			if err := full("reports", all); err != nil {
				skip(KindReport, td.dr.Line, td.dr.Name, err)
				continue
			}
			all++
			if _, err := t.ExecContext(t.ctx, `INSERT INTO reports (name, team_id, schedule, span, zone, sections, dests, enabled, created, created_by, updated, updated_by, declared)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
				x.Name, x.TeamID, x.Schedule, int64(span/time.Second), x.Zone, string(sections), string(dests), x.Enabled, now.Unix(), ConfigBy, now.Unix(), ConfigBy); err != nil {
				return err
			}
			if err := change(t, ConfigBy, "report "+x.Name+" kept", nil, x); err != nil {
				return err
			}
			r.Made++
			continue
		}
		h := have[td.i]
		what, n := "report "+x.Name+" kept", &r.Changed
		if !h.Declared {
			what, n = "report "+x.Name+declaredNow, &r.Adopted
		}
		switch {
		case !sameReport(h, x):
			if _, err := t.ExecContext(t.ctx, `UPDATE reports SET team_id = ?, schedule = ?, span = ?, zone = ?, sections = ?, dests = ?, enabled = ?,
				updated = ?, updated_by = ?, declared = 1, given_back = 0 WHERE id = ?`,
				x.TeamID, x.Schedule, int64(span/time.Second), x.Zone, string(sections), string(dests), x.Enabled, now.Unix(), ConfigBy, h.ID); err != nil {
				return err
			}
			x.ID = h.ID
			if err := change(t, ConfigBy, what, nil, x); err != nil {
				return err
			}
			*n++
		case !h.Declared:
			if err := setDeclared(t, "reports", h.ID, true, what, nil); err != nil {
				return err
			}
			*n++
		}
	}
	return nil
}

func (l *Ledger) declareGrants(t *txn, want []DeclaredGrant, team resolver, left Leave, r *Reconciled,
	skip func(string, int, string, error)) error {
	have, err := grants(t.ctx, t)
	if err != nil {
		return err
	}
	type key struct {
		who  string
		team int64
	}
	byKey := map[key]int{}
	for i, g := range have {
		byKey[key{g.Who, g.TeamID}] = i
	}
	matched := make([]bool, len(have))
	seen := map[key]int{}
	var adds []DeclaredGrant
	var addKeys []key
	for _, dg := range want {
		who, err := grantName(dg.Who)
		var id int64
		known := false
		if err == nil {
			var terr error
			id, known, terr = team(dg.Team)
			err = terr
		}
		// Whom to and of which team, both known, or it is no role there is:
		// a team misspelled keeps none of the person's roles, every team's
		// among them, but takes them away as the file no longer gives them.
		k := key{who, id}
		i, there := byKey[k]
		there = there && who != "" && known
		if err == nil && dg.Bad != nil {
			err = dg.Bad
		}
		if err == nil {
			if line, ok := seen[k]; ok {
				err = invalid("this role is declared on line %d already", line)
			}
		}
		if err != nil {
			if there && (have[i].Declared || have[i].GivenBack) {
				matched[i] = true // kept as it was
			}
			skip(KindGrant, dg.Line, dg.Who, err)
			continue
		}
		seen[k] = dg.Line
		if there {
			matched[i] = true
			if !have[i].Declared {
				if err := setDeclared(t, "grants", have[i].ID, true, RoleGiven+who+declaredNow, nil); err != nil {
					return err
				}
				r.Adopted++
			}
			continue
		}
		adds, addKeys = append(adds, dg), append(addKeys, k)
	}
	_, all, err := originCounts(t, "grants")
	if err != nil {
		return err
	}
	// A role the file no longer gives first: taken away, pruned or not, the
	// UI's taken over and one given back without a file included
	// (declared, it was the file's to give, as Argo CD's RBAC policy is);
	// with no file at all, given back, marked.
	for i, g := range have {
		if matched[i] || !g.Declared && !g.GivenBack {
			continue
		}
		switch left.role(g.GivenBack) {
		case fateStay:
		case fateDelete:
			if _, err := t.ExecContext(t.ctx, "DELETE FROM grants WHERE id = ?", g.ID); err != nil {
				return err
			}
			if err := change(t, ConfigBy, RoleTaken+g.Who, g, nil); err != nil {
				return err
			}
			all--
			r.Deleted++
		default:
			// Said as a role given, which only admins read, as it is now.
			g.Declared, g.GivenBack = false, true
			if err := markGivenBack(t, "grants", g.ID, RoleGiven+g.Who+givenBackNow, g); err != nil {
				return err
			}
			r.Undeclared++
		}
	}
	for n, dg := range adds {
		k := addKeys[n]
		if err := full("grants", all); err != nil {
			skip(KindGrant, dg.Line, dg.Who, err)
			continue
		}
		g := Grant{Who: k.who, Role: GrantedRole, TeamID: k.team, By: ConfigBy, Declared: true}
		if err := t.QueryRowContext(t.ctx, "INSERT INTO grants (who, role, team_id, created, created_by, declared) VALUES (?, ?, ?, ?, ?, 1) RETURNING id",
			g.Who, g.Role, g.TeamID, time.Now().Unix(), ConfigBy).Scan(&g.ID); err != nil {
			return err
		}
		if err := change(t, ConfigBy, RoleGiven+g.Who, nil, g); err != nil {
			return err
		}
		all++
		r.Made++
	}
	return nil
}

// Declaring is what the ledger holds that --config can declare, as an
// export of it gives it (GET /config.yaml): every team, quiet window,
// report and role, declared or made in the UI, and every goal.
type Declaring struct {
	Teams   []Team
	Windows []Window
	Reports []Scheduled
	Grants  []Grant
	Goals   []Goal
}

// AllDeclarable is every team, quiet window (the earliest made first),
// report and role the ledger holds, read as of one moment.
func (l *Ledger) AllDeclarable(ctx context.Context) (Declaring, error) {
	var d Declaring
	tx, err := l.r.BeginTx(ctx, nil)
	if err != nil {
		return d, err
	}
	defer tx.Rollback()
	if d.Teams, err = teams(ctx, tx); err != nil {
		return d, err
	}
	if d.Windows, err = l.windows(ctx, tx, "ORDER BY id"); err != nil {
		return d, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+reportCols+" FROM reports ORDER BY name")
	if err != nil {
		return d, err
	}
	for rows.Next() {
		x, err := scanScheduled(rows)
		if err != nil {
			rows.Close()
			return d, err
		}
		d.Reports = append(d.Reports, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return d, err
	}
	if d.Grants, err = grants(ctx, tx); err != nil {
		return d, err
	}
	d.Goals, err = goals(ctx, tx)
	return d, err
}

// GivenBackRow is one a file declared that a leader without --config gave
// back to the UI, marked (GivenBack): its row's id, its kind (KindTeam,
// KindWindow, KindReport, KindGrant), its name or title, a role's holder,
// and the name of its team, "" for everyone's.
type GivenBackRow struct {
	ID               int64
	Kind, Name, Team string
}

// GivenBack lists what is marked given back, by kind and name: the next
// file that doesn't declare one again lets it go (a role taken away, the
// rest the UI's for good, or deleted with --config-prune), unless the UI
// changes it first, or keeps it (KeepGivenBack). The mark doesn't expire.
func (l *Ledger) GivenBack(ctx context.Context) ([]GivenBackRow, error) {
	return givenBack(ctx, l.r)
}

func givenBack(ctx context.Context, q queryer) ([]GivenBackRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, k, name, team FROM (
		SELECT id, CAST(? AS TEXT) AS k, name, '' AS team FROM teams WHERE given_back = 1
		UNION ALL SELECT w.id, CAST(? AS TEXT), w.title, COALESCE(t.name, '') FROM windows w LEFT JOIN teams t ON t.id = w.team_id WHERE w.given_back = 1
		UNION ALL SELECT r.id, CAST(? AS TEXT), r.name, COALESCE(t.name, '') FROM reports r LEFT JOIN teams t ON t.id = r.team_id WHERE r.given_back = 1
		UNION ALL SELECT g.id, CAST(? AS TEXT), g.who, COALESCE(t.name, '') FROM grants g LEFT JOIN teams t ON t.id = g.team_id WHERE g.given_back = 1
	) x ORDER BY k, name, team, id`, KindTeam, KindWindow, KindReport, KindGrant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GivenBackRow
	for rows.Next() {
		var g GivenBackRow
		if err := rows.Scan(&g.ID, &g.Kind, &g.Name, &g.Team); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// givenBackTables are the tables of each kind of what is given back.
var givenBackTables = map[string]string{KindTeam: "teams", KindWindow: "windows", KindReport: "reports", KindGrant: "grants"}

// KeepGivenBack makes what no --config gave back the UI's for good, as the
// UI changing it does, so that no file takes it away: the row of kind of
// id, or, kind "", every one. Each is noted as by's. It returns how many;
// ErrNotFound when none of them is given back.
func (l *Ledger) KeepGivenBack(ctx context.Context, kind string, id int64, by string) (int, error) {
	if _, ok := givenBackTables[kind]; !ok && kind != "" {
		return 0, invalid("%q is a team, a quiet window, a report or a role", kind)
	}
	var n int
	err := l.write(ctx, func(t *txn) error {
		n = 0
		gb, err := givenBack(t.ctx, t)
		if err != nil {
			return err
		}
		for _, g := range gb {
			if kind != "" && (g.Kind != kind || g.ID != id) {
				continue
			}
			if _, err := t.ExecContext(t.ctx, "UPDATE "+givenBackTables[g.Kind]+" SET given_back = 0 WHERE id = ?", g.ID); err != nil {
				return err
			}
			what := map[string]string{KindTeam: "team ", KindWindow: "quiet window ", KindReport: "report ", KindGrant: RoleGiven}[g.Kind] + g.Name
			if g.Team != "" {
				what += " (team " + g.Team + ")"
			}
			if err := change(t, by, what+keptAsTheUIs, nil, nil); err != nil {
				return err
			}
			n++
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
	return n, err
}

// keptAsTheUIs is the activity log's words for one given back without
// --config that an admin keeps.
const keptAsTheUIs = " kept as the UI's: given back without --config, it is no file's to take away"
