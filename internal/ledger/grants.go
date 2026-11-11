package ledger

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"lapilli.dev/internal/incident"
)

// Roles kept in the ledger, and what a team's editors may change
// (docs/design/serveops.md). The flags give every role; an admin can give,
// in the UI, an editor's role, of every team or of one, and nothing more.

// Grant is a role an admin gave: an editor of every team (TeamID 0), or of
// one.
type Grant struct {
	ID int64 `json:"id"`
	// Who is user:NAME or group:NAME, as the sign-in proxy names them.
	Who string `json:"who"`
	// Role is editor: the only one the ledger keeps.
	Role    string    `json:"role"`
	TeamID  int64     `json:"team_id,omitempty"`
	Created time.Time `json:"created"`
	By      string    `json:"by,omitempty"`
}

// RoleGiven and RoleTaken begin what the changes say of a role given or
// taken away, with whom: only admins see those.
const (
	RoleGiven = "role given to "
	RoleTaken = "role taken from "
)

// GrantedRole is the only role a grant gives.
const GrantedRole = "editor"

// MaxGrants is how many roles the ledger keeps.
const MaxGrants = 1000

// ErrNotYourTeam is a change a team's editor may not make: to what is
// another team's, or everyone's.
var ErrNotYourTeam = errors.New("only an editor of its team can change this")

// scoped is what the writes made with a context may change: the rows of
// these teams.
type scoped struct{ teams []int64 }

type scopeKey struct{}

// WithTeams limits the writes made with ctx to what the editors of these
// teams may change: the tasks, quiet windows and reports of these teams,
// and notes keeping their team. With no teams, nothing.
func WithTeams(ctx context.Context, teams []int64) context.Context {
	return context.WithValue(ctx, scopeKey{}, scoped{slices.Clone(teams)})
}

// scopeOf is the teams the writes made with ctx are limited to; limited is
// false for writes that aren't.
func scopeOf(ctx context.Context) (teams []int64, limited bool) {
	s, ok := ctx.Value(scopeKey{}).(scoped)
	return s.teams, ok
}

// TeamsOf is the teams the writes made with ctx are limited to; limited is
// false for writes that aren't.
func TeamsOf(ctx context.Context) (teams []int64, limited bool) { return scopeOf(ctx) }

// inScope refuses a write, in a transaction limited to some teams, to a row
// of none of them: everyone's (0), or a team removed's (negative).
func inScope(t *txn, team int64) error {
	teams, limited := scopeOf(t.ctx)
	if !limited || team > 0 && slices.Contains(teams, team) {
		return nil
	}
	return ErrNotYourTeam
}

// ownership says which team owns a rule's alert, as the pages work it out:
// a note's team; or else the first team, by name, whose pattern matches the
// rule's name; or else the first whose labels the alert has. The rule's
// name decides before any label, so a team that owns by labels can't take
// a rule another team's pattern or note claims. The pages read the labels
// the rule sets; this, the alert's, which hold those and more: they agree
// but where an alert carries a team's label its rule doesn't set.
type ownership struct {
	teams []ownTeam
	notes map[string]int64
	// first is, for each rule's name, the index of the first team whose
	// pattern matches it; len(teams) for none.
	first map[string]int
}

type ownTeam struct {
	id     int64
	re     *regexp.Regexp
	labels incident.Labels
}

func (l *Ledger) ownership(ctx context.Context, q queryer) (*ownership, error) {
	o := &ownership{notes: map[string]int64{}, first: map[string]int{}}
	rows, err := q.QueryContext(ctx, "SELECT id, rules, labels FROM teams ORDER BY name")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t ownTeam
		var rules, labels string
		if err := rows.Scan(&t.id, &rules, &labels); err != nil {
			rows.Close()
			return nil, err
		}
		if rules != "" {
			t.re, _ = regexp.Compile(rules)
		}
		if t.labels, err = readLabels(labels); err != nil {
			rows.Close()
			return nil, err
		}
		o.teams = append(o.teams, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = q.QueryContext(ctx, "SELECT rule, team_id FROM notes WHERE team_id IS NOT NULL")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var rule string
		var team int64
		if err := rows.Scan(&rule, &team); err != nil {
			return nil, err
		}
		o.notes[rule] = team
	}
	return o, rows.Err()
}

// of is the team that owns a rule's alert with these labels; 0 for none.
func (o *ownership) of(rule string, labels map[string]string) int64 {
	if id, ok := o.notes[rule]; ok {
		return id
	}
	first, ok := o.first[rule]
	if !ok {
		first = slices.IndexFunc(o.teams, func(t ownTeam) bool { return t.re != nil && t.re.MatchString(rule) })
		if first < 0 {
			first = len(o.teams)
		}
		o.first[rule] = first
	}
	if first < len(o.teams) {
		return o.teams[first].id
	}
	for _, t := range o.teams {
		if len(t.labels) > 0 && hasLabels(labels, t.labels) {
			return t.id
		}
	}
	return 0
}

// hasLabels reports whether labels have, for each label named, one of its
// values.
func hasLabels(labels map[string]string, want incident.Labels) bool {
	for k, vs := range want {
		if v, ok := labels[k]; !ok || !slices.Contains(vs, v) {
			return false
		}
	}
	return true
}

// pendTeamWindows notes, to be judged again, the spans of every team's
// quiet window: who owns what changed, and with it what they quiet.
func (l *Ledger) pendTeamWindows(t *txn) error {
	ws, err := l.windows(t.ctx, t, "WHERE team_id != 0")
	if err != nil || len(ws) == 0 {
		return err
	}
	now := time.Now()
	spans := make([][2]int64, 0, len(ws))
	for _, w := range ws {
		spans = append(spans, w.span(now))
	}
	return pend(t, spans)
}

// grantName is a name a grant gives a role to: user:NAME or group:NAME.
func grantName(who string) (string, error) {
	who = strings.TrimSpace(who)
	kind, name, ok := strings.Cut(who, ":")
	name = strings.TrimSpace(name)
	switch {
	case !ok || kind != "user" && kind != "group" || name == "":
		return "", invalid("a role is given to user:NAME or group:NAME, as the sign-in proxy names them")
	case utf8.RuneCountInString(name) > 200 || strings.ContainsFunc(name, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }):
		return "", invalid("a name is one line of 200 characters at most")
	case name == "*":
		return "", invalid("everyone is given a role by the flags only")
	case kind == "group" && strings.Contains(name, ","):
		return "", invalid("a group's name has no comma: the sign-in proxy separates groups by commas")
	}
	if f := FoldName(name); kind == "user" && slices.ContainsFunc([]string{"anonymous", Copier, FiringsCopier, TaskTracker, ChangesCopier, Proposer},
		func(n string) bool { return f == FoldName(n) }) {
		return "", invalid("%s is a name Lapilli keeps for itself", name)
	}
	return kind + ":" + name, nil
}

// Grants are the roles given in the UI, by whom they are given to.
func (l *Ledger) Grants(ctx context.Context) ([]Grant, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT id, who, role, team_id, created, created_by FROM grants ORDER BY who, team_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var g Grant
		var at int64
		if err := rows.Scan(&g.ID, &g.Who, &g.Role, &g.TeamID, &at, &g.By); err != nil {
			return nil, err
		}
		g.Created = unix(at)
		out = append(out, g)
	}
	return out, rows.Err()
}

// PutGrant gives a role: an editor's, of every team or of one. Any other
// is refused, here and by the table.
func (l *Ledger) PutGrant(ctx context.Context, g Grant, by string) (int64, error) {
	if g.Role == "" {
		g.Role = GrantedRole
	}
	if g.Role != GrantedRole {
		return 0, invalid("the UI gives an editor's role, of every team or of one: admins and approvers are named by the flags")
	}
	who, err := grantName(g.Who)
	if err != nil {
		return 0, err
	}
	g.Who = who
	if g.TeamID < 0 {
		return 0, invalid("no such team")
	}
	var id int64
	err = l.write(ctx, func(t *txn) error {
		if _, limited := scopeOf(t.ctx); limited {
			return ErrNotYourTeam
		}
		if g.TeamID != 0 {
			var x int
			if err := t.QueryRowContext(t.ctx, "SELECT 1 FROM teams WHERE id = ?", g.TeamID).Scan(&x); err != nil {
				return invalid("no such team")
			}
		}
		var n int
		if err := t.QueryRowContext(t.ctx, "SELECT count(*) FROM grants").Scan(&n); err != nil {
			return err
		}
		if n >= MaxGrants {
			return invalid("the ledger keeps %d roles at most", MaxGrants)
		}
		res, err := t.ExecContext(t.ctx, "INSERT INTO grants (who, role, team_id, created, created_by) VALUES (?, ?, ?, ?, ?)",
			g.Who, g.Role, g.TeamID, time.Now().Unix(), by)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return invalid("%s has that role already", g.Who)
			}
			return err
		}
		id, _ = res.LastInsertId()
		g.ID = id
		return change(t, by, RoleGiven+g.Who, nil, g)
	})
	return id, err
}

// DeleteGrant takes a role away.
func (l *Ledger) DeleteGrant(ctx context.Context, id int64, by string) error {
	return l.write(ctx, func(t *txn) error {
		if _, limited := scopeOf(t.ctx); limited {
			return ErrNotYourTeam
		}
		var who string
		if err := t.QueryRowContext(t.ctx, "SELECT who FROM grants WHERE id = ?", id).Scan(&who); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if _, err := t.ExecContext(t.ctx, "DELETE FROM grants WHERE id = ?", id); err != nil {
			return err
		}
		return change(t, by, RoleTaken+who, nil, nil)
	})
}
