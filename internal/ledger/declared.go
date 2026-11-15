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
var ErrNamedByDeclared = errors.New("a quiet window, a report or a role declared in config names this team: change them in values first")

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

// Leftover is what becomes of what is declared that the file no longer
// names.
type Leftover int

const (
	// LeftUndeclare gives it back to the UI, as it was.
	LeftUndeclare Leftover = iota
	// LeftKeep keeps it declared, as it is: a deletion not yet sure, as
	// the file that leaves it out hasn't been read twice.
	LeftKeep
	// LeftDelete deletes it (--config-prune): but a team something still
	// names, which is given back to the UI instead.
	LeftDelete
)

// Reconciled is what Reconcile did: what it made, changed, took over from
// the UI (declared, as it was or as the file says it), gave back to the
// UI, deleted, and kept declared though the file no longer names it
// (LeftKeep).
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
// file no longer names is given back, kept or deleted as left says, always.
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
func (l *Ledger) Reconcile(ctx context.Context, d Declared, left Leftover, o verdict.Options) (Reconciled, []DeclaredError, error) {
	switch {
	case len(d.Teams) > MaxDeclaredTeams:
		return Reconciled{}, nil, invalid("a config declares %d teams at most", MaxDeclaredTeams)
	case len(d.Windows) > MaxWindows:
		return Reconciled{}, nil, invalid("a config declares %d quiet windows at most", MaxWindows)
	case len(d.Reports) > MaxScheduled:
		return Reconciled{}, nil, invalid("a config declares %d reports at most", MaxScheduled)
	case len(d.Grants) > MaxGrants:
		return Reconciled{}, nil, invalid("a config declares %d roles at most", MaxGrants)
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
		leftTeams, err := l.declareTeams(t, d.Teams, &r, skip)
		if err != nil {
			return err
		}
		team, err := l.teamResolver(t)
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
		// The teams last: what names one the file no longer does is known.
		if err := l.leaveTeams(t, leftTeams, left, &r); err != nil {
			return err
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

// setDeclared marks a row of table declared or not, and notes it.
func setDeclared(t *txn, table string, id int64, declared bool, what string, after any) error {
	if _, err := t.ExecContext(t.ctx, "UPDATE "+table+" SET declared = ? WHERE id = ?", declared, id); err != nil {
		return err
	}
	return change(t, ConfigBy, what, nil, after)
}

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
func (l *Ledger) declareTeams(t *txn, want []DeclaredTeam, r *Reconciled, skip func(string, int, string, error)) ([]Team, error) {
	have, err := teams(t.ctx, t)
	if err != nil {
		return nil, err
	}
	exact, folds := map[string]int{}, map[string][]int{}
	for i, x := range have {
		exact[x.Name] = i
		folds[FoldName(x.Name)] = append(folds[FoldName(x.Name)], i)
	}
	// find is the team of a name, or, in any letter case, the one alone
	// that has it: -1 for none; ambiguous, when more than one does.
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
	seen := map[string]int{}
	for _, dt := range want {
		f := FoldName(dt.Name)
		if dt.Bad != nil {
			// Kept as it was.
			for _, n := range []string{dt.Name, dt.RenamedFrom} {
				if i, _ := find(n); n != "" && i >= 0 {
					matched[i] = true
				}
			}
			skip(KindTeam, dt.Line, dt.Name, dt.Bad)
			continue
		}
		if line, ok := seen[f]; ok {
			skip(KindTeam, dt.Line, dt.Name, invalid("a team called %s is declared on line %d already: a team's name is unique in any letter case", dt.Name, line))
			continue
		}
		seen[f] = dt.Line
		i, ambiguous := find(dt.Name)
		if i < 0 && ambiguous == nil && dt.RenamedFrom != "" {
			if i, ambiguous = find(dt.RenamedFrom); i >= 0 && matched[i] {
				i = -1
			}
		}
		if ambiguous != nil {
			skip(KindTeam, dt.Line, dt.Name, invalid("the teams %s differ only in letter case: rename all but one in the UI, then declare it", strings.Join(ambiguous, ", ")))
			continue
		}
		if i < 0 {
			if _, err := l.putTeam(t, 0, dt.Team, ConfigBy, true, "team "+dt.Name+" kept"); err != nil {
				if invalidErr(err) {
					skip(KindTeam, dt.Line, dt.Name, err)
					continue
				}
				return nil, err
			}
			r.Made++
			continue
		}
		matched[i] = true
		x := have[i]
		what, n := "team "+dt.Name+" kept", &r.Changed
		if !x.Declared {
			what, n = "team "+dt.Name+declaredNow, &r.Adopted
		}
		switch {
		case !sameTeam(x, dt.Team):
			if _, err := l.putTeam(t, x.ID, dt.Team, ConfigBy, true, what); err != nil {
				if invalidErr(err) {
					skip(KindTeam, dt.Line, dt.Name, err)
					continue
				}
				return nil, err
			}
			*n++
		case !x.Declared:
			if err := setDeclared(t, "teams", x.ID, true, what, nil); err != nil {
				return nil, err
			}
			*n++
		}
	}
	var leftover []Team
	for i, x := range have {
		if !matched[i] && x.Declared {
			leftover = append(leftover, x)
		}
	}
	return leftover, nil
}

// leaveTeams gives back, keeps or deletes the declared teams the file no
// longer names, as left says. One something still names (a quiet window,
// a report, a role, a task, a rule's note), the UI's or declared, isn't
// deleted, but given back to the UI, and said why: deleted, it would leave
// them no one's, and the file, put back, would make another team in its
// place, which they wouldn't name.
func (l *Ledger) leaveTeams(t *txn, leftover []Team, left Leftover, r *Reconciled) error {
	for _, x := range leftover {
		switch left {
		case LeftKeep:
			r.Kept++
			continue
		case LeftUndeclare:
			if err := setDeclared(t, "teams", x.ID, false, "team "+x.Name+undeclaredNow, nil); err != nil {
				return err
			}
			r.Undeclared++
			continue
		}
		var windows, reports, roles, tasks, notes int
		if err := t.QueryRowContext(t.ctx, `SELECT (SELECT count(*) FROM windows WHERE team_id = ?), (SELECT count(*) FROM reports WHERE team_id = ?),
			(SELECT count(*) FROM grants WHERE team_id = ?), (SELECT count(*) FROM tasks WHERE team_id = ?), (SELECT count(*) FROM notes WHERE team_id = ?)`,
			x.ID, x.ID, x.ID, x.ID, x.ID).Scan(&windows, &reports, &roles, &tasks, &notes); err != nil {
			return err
		}
		if windows+reports+roles+tasks+notes > 0 {
			named := map[string]int{"quiet windows": windows, "reports": reports, "roles": roles, "tasks": tasks, "notes": notes}
			if err := setDeclared(t, "teams", x.ID, false, "team "+x.Name+" no longer declared in config, and not deleted: what it owns names it, so it is the UI's to change", named); err != nil {
				return err
			}
			r.Undeclared++
			continue
		}
		if err := l.deleteTeam(t, x.ID, ConfigBy); err != nil {
			return err
		}
		r.Deleted++
	}
	return nil
}

// teamResolver finds a team by the name --config gives it, as the ledger
// holds them in a write: by its name, or else in any letter case, when one
// team alone has it; "" is everyone's (0).
func (l *Ledger) teamResolver(t *txn) (func(name string) (int64, error), error) {
	have, err := teams(t.ctx, t)
	if err != nil {
		return nil, err
	}
	exact, folds := map[string]int64{}, map[string][]Team{}
	for _, x := range have {
		exact[x.Name] = x.ID
		folds[FoldName(x.Name)] = append(folds[FoldName(x.Name)], x)
	}
	return func(name string) (int64, error) {
		name = strings.TrimSpace(name)
		if name == "" {
			return 0, nil
		}
		if id, ok := exact[name]; ok {
			return id, nil
		}
		switch c := folds[FoldName(name)]; len(c) {
		case 0:
			return 0, invalid("there is no team called %s, in the config or the ledger", name)
		case 1:
			return c[0].ID, nil
		}
		return 0, invalid("more than one team is called %s in some letter case: name it as one of them is", name)
	}, nil
}

// sameWindow says two quiet windows are kept alike.
func sameWindow(a, b Window) bool {
	return a.Title == b.Title && sameStones(a, b)
}

func (l *Ledger) declareWindows(t *txn, want []DeclaredWindow, team func(string) (int64, error), left Leftover, r *Reconciled,
	skip func(string, int, string, error)) ([][2]int64, error) {
	ws := make([]Window, len(want))
	bad := make([]error, len(want))
	known := make([]bool, len(want)) // its team found
	for i, dw := range want {
		w := dw.Window
		id, err := team(dw.Team)
		if err == nil {
			w.TeamID, known[i] = id, true
		}
		w.loc = l.loc
		ws[i], bad[i] = w, errors.Join(dw.Bad, err)
	}
	have, err := l.windows(t.ctx, t, "ORDER BY id")
	if err != nil {
		return nil, err
	}
	// Those of each title, the declared first, each by its id.
	byTitle := map[string][]int{}
	for _, declared := range []bool{true, false} {
		for i, h := range have {
			if h.Declared == declared {
				byTitle[h.Title] = append(byTitle[h.Title], i)
			}
		}
	}
	matched := make([]bool, len(have))
	pair := make([]int, len(ws))
	for i := range pair {
		pair[i] = -1
	}
	// Those alike; then a declared one of the same team, then of another
	// (moved by the file); then, of the same team alone, one the UI made,
	// taken over. A window taken out of a file of several with one title
	// leaves the others as they were, and what the file declares is kept
	// in the rows it was before any of the UI's is taken.
	for stage := range 4 {
		for wi, w := range ws {
			if pair[wi] >= 0 || bad[wi] != nil {
				continue
			}
			for _, hi := range byTitle[w.Title] {
				if matched[hi] {
					continue
				}
				h := have[hi]
				if stage == 0 && sameWindow(h, w) || stage == 1 && h.Declared && h.TeamID == w.TeamID || stage == 2 && h.Declared ||
					stage == 3 && h.TeamID == w.TeamID {
					pair[wi], matched[hi] = hi, true
					break
				}
			}
		}
	}
	// One that can't be kept keeps a declared one of its title as it was,
	// of its team when that is known.
	for wi, w := range ws {
		if bad[wi] == nil {
			continue
		}
		skip(KindWindow, want[wi].Line, want[wi].Title, bad[wi])
		best := -1
		for _, hi := range byTitle[strings.TrimSpace(want[wi].Title)] {
			if h := have[hi]; !matched[hi] && h.Declared && (best < 0 || known[wi] && h.TeamID == w.TeamID && have[best].TeamID != w.TeamID) {
				best = hi
			}
		}
		if best >= 0 {
			matched[best] = true
		}
	}
	now := time.Now()
	var spans [][2]int64
	for wi, w := range ws {
		if bad[wi] != nil {
			continue
		}
		if pair[wi] < 0 {
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
	for i, h := range have {
		if matched[i] || !h.Declared {
			continue
		}
		switch left {
		case LeftKeep:
			r.Kept++
		case LeftDelete:
			if _, err := t.ExecContext(t.ctx, "DELETE FROM windows WHERE id = ?", h.ID); err != nil {
				return nil, err
			}
			if err := change(t, ConfigBy, "quiet window "+h.Title+" deleted", nil, nil); err != nil {
				return nil, err
			}
			spans = append(spans, h.span(now))
			r.Deleted++
		default:
			if err := setDeclared(t, "windows", h.ID, false, "quiet window "+h.Title+undeclaredNow, nil); err != nil {
				return nil, err
			}
			r.Undeclared++
		}
	}
	return spans, nil
}

// sameReport says two reports are kept alike.
func sameReport(a, b Scheduled) bool {
	return a.Name == b.Name && a.TeamID == b.TeamID && a.Schedule == b.Schedule && a.Span == b.Span && a.Zone == b.Zone &&
		slices.Equal(a.Sections, b.Sections) && slices.Equal(a.Dests, b.Dests) && a.Enabled == b.Enabled
}

func (l *Ledger) declareReports(t *txn, want []DeclaredReport, team func(string) (int64, error), left Leftover, r *Reconciled,
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
	for _, dr := range want {
		x := dr.Scheduled
		x.ID = 0
		err := dr.Bad
		if err == nil {
			x.TeamID, err = team(dr.Team)
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
		if err == nil && there && !have[i].Declared && have[i].TeamID != x.TeamID {
			// The UI's, of another team: not taken over.
			err = invalid("a report called %s is the UI's, for another team: rename one of them", x.Name)
			there = false
		}
		if err != nil {
			if there && have[i].Declared {
				matched[i] = true // kept as it was
			}
			skip(KindReport, dr.Line, dr.Name, err)
			continue
		}
		seen[x.Name] = dr.Line
		span, _ := x.span()
		sections, _ := json.Marshal(x.Sections)
		dests, _ := json.Marshal(x.Dests)
		if !there {
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
		matched[i] = true
		h := have[i]
		what, n := "report "+x.Name+" kept", &r.Changed
		if !h.Declared {
			what, n = "report "+x.Name+declaredNow, &r.Adopted
		}
		switch {
		case !sameReport(h, x):
			if _, err := t.ExecContext(t.ctx, `UPDATE reports SET team_id = ?, schedule = ?, span = ?, zone = ?, sections = ?, dests = ?, enabled = ?,
				updated = ?, updated_by = ?, declared = 1 WHERE id = ?`,
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
	for i, h := range have {
		if matched[i] || !h.Declared {
			continue
		}
		switch left {
		case LeftKeep:
			r.Kept++
		case LeftDelete:
			if _, err := t.ExecContext(t.ctx, "DELETE FROM reports WHERE id = ?", h.ID); err != nil {
				return err
			}
			if _, err := t.ExecContext(t.ctx, "DELETE FROM meta WHERE key = ?", ReportSentKey(h.ID)); err != nil {
				return err
			}
			if err := change(t, ConfigBy, "report "+h.Name+" deleted", nil, nil); err != nil {
				return err
			}
			r.Deleted++
		default:
			if err := setDeclared(t, "reports", h.ID, false, "report "+h.Name+undeclaredNow, nil); err != nil {
				return err
			}
			r.Undeclared++
		}
	}
	return nil
}

func (l *Ledger) declareGrants(t *txn, want []DeclaredGrant, team func(string) (int64, error), left Leftover, r *Reconciled,
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
	for _, dg := range want {
		who, err := grantName(dg.Who)
		var id int64
		if err == nil {
			id, err = team(dg.Team)
		}
		k := key{who, id}
		i, there := byKey[k]
		if err == nil && dg.Bad != nil {
			err = dg.Bad
		}
		if err == nil {
			if line, ok := seen[k]; ok {
				err = invalid("this role is declared on line %d already", line)
			}
		}
		if err != nil {
			if there && have[i].Declared && who != "" {
				matched[i] = true // kept as it was
			}
			skip(KindGrant, dg.Line, dg.Who, err)
			continue
		}
		seen[k] = dg.Line
		if !there {
			g := Grant{Who: who, Role: GrantedRole, TeamID: id, By: ConfigBy, Declared: true}
			if err := t.QueryRowContext(t.ctx, "INSERT INTO grants (who, role, team_id, created, created_by, declared) VALUES (?, ?, ?, ?, ?, 1) RETURNING id",
				g.Who, g.Role, g.TeamID, time.Now().Unix(), ConfigBy).Scan(&g.ID); err != nil {
				return err
			}
			if err := change(t, ConfigBy, RoleGiven+g.Who, nil, g); err != nil {
				return err
			}
			r.Made++
			continue
		}
		matched[i] = true
		if !have[i].Declared {
			if err := setDeclared(t, "grants", have[i].ID, true, RoleGiven+who+declaredNow, nil); err != nil {
				return err
			}
			r.Adopted++
		}
	}
	for i, g := range have {
		if matched[i] || !g.Declared {
			continue
		}
		switch left {
		case LeftKeep:
			r.Kept++
		case LeftDelete:
			if _, err := t.ExecContext(t.ctx, "DELETE FROM grants WHERE id = ?", g.ID); err != nil {
				return err
			}
			if err := change(t, ConfigBy, RoleTaken+g.Who, nil, nil); err != nil {
				return err
			}
			r.Deleted++
		default:
			// Said as a role given, which only admins read.
			if err := setDeclared(t, "grants", g.ID, false, RoleGiven+g.Who+undeclaredNow, nil); err != nil {
				return err
			}
			r.Undeclared++
		}
	}
	return nil
}

// Declaring is what the ledger holds that --config can declare, as an
// export of it gives it (GET /config.yaml): every team, quiet window,
// report and role, declared or made in the UI.
type Declaring struct {
	Teams   []Team
	Windows []Window
	Reports []Scheduled
	Grants  []Grant
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
	d.Grants, err = grants(ctx, tx)
	return d, err
}
