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

// ErrNamedByDeclared is a team deleted that something --config declares
// names: the file would name a team that is gone.
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

// MaxDeclaredTeams is how many teams --config declares at most; quiet
// windows, reports and roles are bounded as the ledger keeps them
// (MaxWindows, MaxScheduled, MaxGrants).
const MaxDeclaredTeams = 1000

// DeclaredTeam is a team --config declares, and the line it starts on.
type DeclaredTeam struct {
	Team
	Line int
}

// DeclaredWindow is a quiet window --config declares: Team is its team's
// name, "" for everyone's.
type DeclaredWindow struct {
	Window
	Team string
	Line int
}

// DeclaredReport is a report --config declares: Team is its team's name,
// "" for everyone's.
type DeclaredReport struct {
	Scheduled
	Team string
	Line int
}

// DeclaredGrant is a role --config declares: an editor's, given to Who
// (user:NAME or group:NAME), of Team ("" for every team).
type DeclaredGrant struct {
	Who, Team string
	Line      int
}

// Declared is what --config declares.
type Declared struct {
	Teams   []DeclaredTeam
	Windows []DeclaredWindow
	Reports []DeclaredReport
	Grants  []DeclaredGrant
}

// DeclaredError is what is wrong with something --config declares: the
// line it starts on, its name, and why.
type DeclaredError struct {
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

func declErr(line int, name string, err error) error {
	if err == nil {
		return nil
	}
	return DeclaredError{line, name, err}
}

// Reconciled is what Reconcile did: what it made, changed, took over from
// the UI (declared as it was), gave back to the UI and deleted.
type Reconciled struct {
	Made, Changed, Adopted, Undeclared, Deleted int
}

// Any says it did anything.
func (r Reconciled) Any() bool { return r != Reconciled{} }

// Reconcile makes the ledger hold what d declares, in one write: all of it
// or, when anything in it can't be kept, none (a DeclaredError, the ledger
// as it was). Each team is matched by its name, in any letter case; each
// report by its name; each role by whom it is given to and its team; each
// quiet window by its title, those alike first, then those of its team,
// then in order, the declared before the UI's. One the ledger holds from
// the UI already is taken over, as the file says it. What is declared and
// the file no longer names is the UI's again, or deleted with prune. Each
// change is noted in the activity log as ConfigBy's; a file that says what
// the ledger holds writes nothing. The quiet windows it changes are judged
// again with o.
//
// The checks are those of the UI's writes (PutTeam, PutWindow,
// PutScheduled, PutGrant) and their limits: what the server checks besides
// (a report's destinations, a team's on-call shifts) its caller checks.
func (l *Ledger) Reconcile(ctx context.Context, d Declared, prune bool, o verdict.Options) (Reconciled, error) {
	if len(d.Teams) > MaxDeclaredTeams {
		return Reconciled{}, invalid("a config declares %d teams at most", MaxDeclaredTeams)
	}
	for i := range d.Teams {
		if err := checkTeam(&d.Teams[i].Team); err != nil {
			return Reconciled{}, declErr(d.Teams[i].Line, d.Teams[i].Name, err)
		}
	}
	var r Reconciled
	var spans [][2]int64
	err := l.write(ctx, func(t *txn) error {
		r, spans = Reconciled{}, nil
		if err := l.declareTeams(t, d.Teams, prune, &r); err != nil {
			return err
		}
		team, err := l.teamResolver(t)
		if err != nil {
			return err
		}
		if spans, err = l.declareWindows(t, d.Windows, team, prune, &r); err != nil {
			return err
		}
		if err := l.declareReports(t, d.Reports, team, prune, &r); err != nil {
			return err
		}
		if err := l.declareGrants(t, d.Grants, team, prune, &r); err != nil {
			return err
		}
		if !r.Any() {
			// Nothing to keep: no write, and no page read again.
			return errUnchanged
		}
		return pend(t, spans)
	})
	if errors.Is(err, errUnchanged) {
		return Reconciled{}, nil
	}
	if err != nil {
		return Reconciled{}, err
	}
	if len(spans) > 0 {
		l.judgeSoon(ctx, o)
	}
	return r, nil
}

// errUnchanged takes back a write that changed nothing.
var errUnchanged = errors.New("unchanged")

// sameTeam says two teams are kept alike.
func sameTeam(a, b Team) bool {
	return a.Name == b.Name && a.Rules == b.Rules && labelsText(a.Labels) == labelsText(b.Labels) && a.Note == b.Note &&
		a.WorkHours == b.WorkHours && a.SleepHours == b.SleepHours && a.TZ == b.TZ && a.Shifts == b.Shifts
}

// setDeclared marks a row of table declared or not, and notes it.
func setDeclared(t *txn, table string, id int64, declared bool, what string) error {
	if _, err := t.ExecContext(t.ctx, "UPDATE "+table+" SET declared = ? WHERE id = ?", declared, id); err != nil {
		return err
	}
	return change(t, ConfigBy, what, nil, nil)
}

// The activity log's words for a row taken over from the UI, and for one
// the file no longer names, given back.
const (
	declaredNow   = " declared in config"
	undeclaredNow = " no longer declared in config: the UI's to change"
)

func (l *Ledger) declareTeams(t *txn, want []DeclaredTeam, prune bool, r *Reconciled) error {
	have, err := teams(t.ctx, t)
	if err != nil {
		return err
	}
	exact, folds := map[string]int{}, map[string][]int{}
	for i, x := range have {
		exact[x.Name] = i
		folds[FoldName(x.Name)] = append(folds[FoldName(x.Name)], i)
	}
	matched := make([]bool, len(have))
	seen := map[string]int{}
	for _, dt := range want {
		f := FoldName(dt.Name)
		if line, ok := seen[f]; ok {
			return declErr(dt.Line, dt.Name, invalid("a team called %s is declared on line %d already: a team's name is unique in any letter case", dt.Name, line))
		}
		seen[f] = dt.Line
		i, ok := exact[dt.Name]
		if !ok {
			switch c := folds[f]; len(c) {
			case 0:
			case 1:
				i, ok = c[0], true
			default:
				var names []string
				for _, j := range c {
					names = append(names, have[j].Name)
				}
				return declErr(dt.Line, dt.Name, invalid("the teams %s differ only in letter case: rename all but one in the UI, then declare it", strings.Join(names, ", ")))
			}
		}
		if !ok {
			if _, err := l.putTeam(t, 0, dt.Team, ConfigBy, true); err != nil {
				return declErr(dt.Line, dt.Name, err)
			}
			r.Made++
			continue
		}
		matched[i] = true
		x := have[i]
		switch {
		case !sameTeam(x, dt.Team):
			if _, err := l.putTeam(t, x.ID, dt.Team, ConfigBy, true); err != nil {
				return declErr(dt.Line, dt.Name, err)
			}
			r.Changed++
		case !x.Declared:
			if err := setDeclared(t, "teams", x.ID, true, "team "+x.Name+declaredNow); err != nil {
				return err
			}
			r.Adopted++
		}
	}
	for i, x := range have {
		if matched[i] || !x.Declared {
			continue
		}
		if prune {
			// Its roles go with it, as when the UI deletes a team: those
			// declared, each noted, as a role the file no longer names.
			if err := pruneTeamGrants(t, x.ID, r); err != nil {
				return err
			}
			if err := l.deleteTeam(t, x.ID, ConfigBy); err != nil {
				return err
			}
			r.Deleted++
			continue
		}
		if err := setDeclared(t, "teams", x.ID, false, "team "+x.Name+undeclaredNow); err != nil {
			return err
		}
		r.Undeclared++
	}
	return nil
}

// pruneTeamGrants deletes the declared roles of a team pruned, each noted.
func pruneTeamGrants(t *txn, team int64, r *Reconciled) error {
	rows, err := t.QueryContext(t.ctx, "SELECT id, who FROM grants WHERE team_id = ? AND declared = 1 ORDER BY id", team)
	if err != nil {
		return err
	}
	type grant struct {
		id  int64
		who string
	}
	var gs []grant
	for rows.Next() {
		var g grant
		if err := rows.Scan(&g.id, &g.who); err != nil {
			rows.Close()
			return err
		}
		gs = append(gs, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, g := range gs {
		if _, err := t.ExecContext(t.ctx, "DELETE FROM grants WHERE id = ?", g.id); err != nil {
			return err
		}
		if err := change(t, ConfigBy, RoleTaken+g.who, nil, nil); err != nil {
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

func (l *Ledger) declareWindows(t *txn, want []DeclaredWindow, team func(string) (int64, error), prune bool, r *Reconciled) ([][2]int64, error) {
	ws := make([]Window, len(want))
	for i, dw := range want {
		w := dw.Window
		id, err := team(dw.Team)
		if err != nil {
			return nil, declErr(dw.Line, dw.Title, err)
		}
		w.ID, w.TeamID = 0, id
		if err := l.checkWindow(&w); err != nil {
			return nil, declErr(dw.Line, dw.Title, err)
		}
		w.loc = l.loc
		ws[i] = w
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
	// Those alike, then those of the same team, then in order: a window
	// taken out of a file of several with one title leaves the others as
	// they were.
	for stage := range 3 {
		for wi, w := range ws {
			if pair[wi] >= 0 {
				continue
			}
			for _, hi := range byTitle[w.Title] {
				if matched[hi] {
					continue
				}
				h := have[hi]
				if stage == 2 || stage == 1 && h.TeamID == w.TeamID || stage == 0 && sameWindow(h, w) {
					pair[wi], matched[hi] = hi, true
					break
				}
			}
		}
	}
	now := time.Now()
	var spans [][2]int64
	for wi, w := range ws {
		dw := want[wi]
		if pair[wi] < 0 {
			var n int
			if err := t.QueryRowContext(t.ctx, "SELECT count(*) FROM windows").Scan(&n); err != nil {
				return nil, err
			}
			if n >= MaxWindows {
				return nil, declErr(dw.Line, dw.Title, invalid("the ledger keeps %d quiet windows at most: delete those no longer needed", MaxWindows))
			}
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
		switch {
		case !sameWindow(h, w):
			if err := updateWindow(t, h.ID, w, ConfigBy, true); err != nil {
				return nil, err
			}
			w.ID = h.ID
			if err := change(t, ConfigBy, "quiet window "+w.Title+" kept", nil, w); err != nil {
				return nil, err
			}
			if !sameStones(h, w) {
				spans = append(spans, h.span(now), w.span(now))
			}
			r.Changed++
		case !h.Declared:
			if err := setDeclared(t, "windows", h.ID, true, "quiet window "+h.Title+declaredNow); err != nil {
				return nil, err
			}
			r.Adopted++
		}
	}
	for i, h := range have {
		if matched[i] || !h.Declared {
			continue
		}
		if prune {
			if _, err := t.ExecContext(t.ctx, "DELETE FROM windows WHERE id = ?", h.ID); err != nil {
				return nil, err
			}
			if err := change(t, ConfigBy, "quiet window "+h.Title+" deleted", nil, nil); err != nil {
				return nil, err
			}
			spans = append(spans, h.span(now))
			r.Deleted++
			continue
		}
		if err := setDeclared(t, "windows", h.ID, false, "quiet window "+h.Title+undeclaredNow); err != nil {
			return nil, err
		}
		r.Undeclared++
	}
	return spans, nil
}

// sameReport says two reports are kept alike.
func sameReport(a, b Scheduled) bool {
	return a.Name == b.Name && a.TeamID == b.TeamID && a.Schedule == b.Schedule && a.Span == b.Span && a.Zone == b.Zone &&
		slices.Equal(a.Sections, b.Sections) && slices.Equal(a.Dests, b.Dests) && a.Enabled == b.Enabled
}

func (l *Ledger) declareReports(t *txn, want []DeclaredReport, team func(string) (int64, error), prune bool, r *Reconciled) error {
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
		id, err := team(dr.Team)
		if err != nil {
			return declErr(dr.Line, dr.Name, err)
		}
		x.ID, x.TeamID = 0, id
		if err := x.check(); err != nil {
			return declErr(dr.Line, dr.Name, err)
		}
		if line, ok := seen[x.Name]; ok {
			return declErr(dr.Line, x.Name, invalid("a report called %s is declared on line %d already", x.Name, line))
		}
		seen[x.Name] = dr.Line
		span, _ := x.span()
		sections, _ := json.Marshal(x.Sections)
		dests, _ := json.Marshal(x.Dests)
		i, ok := byName[x.Name]
		if !ok {
			var n int
			if err := t.QueryRowContext(t.ctx, "SELECT count(*) FROM reports").Scan(&n); err != nil {
				return err
			}
			if n >= MaxScheduled {
				return declErr(dr.Line, x.Name, invalid("the ledger keeps %d reports at most", MaxScheduled))
			}
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
		switch {
		case !sameReport(h, x):
			if _, err := t.ExecContext(t.ctx, `UPDATE reports SET team_id = ?, schedule = ?, span = ?, zone = ?, sections = ?, dests = ?, enabled = ?,
				updated = ?, updated_by = ?, declared = 1 WHERE id = ?`,
				x.TeamID, x.Schedule, int64(span/time.Second), x.Zone, string(sections), string(dests), x.Enabled, now.Unix(), ConfigBy, h.ID); err != nil {
				return err
			}
			x.ID = h.ID
			if err := change(t, ConfigBy, "report "+x.Name+" kept", nil, x); err != nil {
				return err
			}
			r.Changed++
		case !h.Declared:
			if err := setDeclared(t, "reports", h.ID, true, "report "+h.Name+declaredNow); err != nil {
				return err
			}
			r.Adopted++
		}
	}
	for i, h := range have {
		if matched[i] || !h.Declared {
			continue
		}
		if prune {
			for _, q := range []string{"DELETE FROM reports WHERE id = ?", "DELETE FROM meta WHERE key = ?"} {
				arg := any(h.ID)
				if strings.Contains(q, "meta") {
					arg = ReportSentKey(h.ID)
				}
				if _, err := t.ExecContext(t.ctx, q, arg); err != nil {
					return err
				}
			}
			if err := change(t, ConfigBy, "report "+h.Name+" deleted", nil, nil); err != nil {
				return err
			}
			r.Deleted++
			continue
		}
		if err := setDeclared(t, "reports", h.ID, false, "report "+h.Name+undeclaredNow); err != nil {
			return err
		}
		r.Undeclared++
	}
	return nil
}

func (l *Ledger) declareGrants(t *txn, want []DeclaredGrant, team func(string) (int64, error), prune bool, r *Reconciled) error {
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
		if err != nil {
			return declErr(dg.Line, dg.Who, err)
		}
		id, err := team(dg.Team)
		if err != nil {
			return declErr(dg.Line, who, err)
		}
		k := key{who, id}
		if line, ok := seen[k]; ok {
			return declErr(dg.Line, who, invalid("this role is declared on line %d already", line))
		}
		seen[k] = dg.Line
		i, ok := byKey[k]
		if !ok {
			var n int
			if err := t.QueryRowContext(t.ctx, "SELECT count(*) FROM grants").Scan(&n); err != nil {
				return err
			}
			if n >= MaxGrants {
				return declErr(dg.Line, who, invalid("the ledger keeps %d roles at most", MaxGrants))
			}
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
			if err := setDeclared(t, "grants", have[i].ID, true, RoleGiven+who+declaredNow); err != nil {
				return err
			}
			r.Adopted++
		}
	}
	for i, g := range have {
		if matched[i] || !g.Declared {
			continue
		}
		if prune {
			if _, err := t.ExecContext(t.ctx, "DELETE FROM grants WHERE id = ?", g.ID); err != nil {
				return err
			}
			if err := change(t, ConfigBy, RoleTaken+g.Who, nil, nil); err != nil {
				return err
			}
			r.Deleted++
			continue
		}
		// Said as a role given, which only admins read.
		if err := setDeclared(t, "grants", g.ID, false, RoleGiven+g.Who+undeclaredNow); err != nil {
			return err
		}
		r.Undeclared++
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
