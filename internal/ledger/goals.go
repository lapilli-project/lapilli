package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Goals (docs/design/goals.md): what an operations team holds its alerting
// to, as --config declares it under goals:, kept in the ledger by the
// leader for every replica to read. A goal names one of the summary's
// numbers, for everyone, a team, or each team (team: "*"), and a bound on
// it; each week is met, missed or not known, worked out from the summary's
// rows wherever it is read. What the ledger keeps besides is what was done
// about one: its tasks (tasks.goal), and what was told.

// Goal is a goal, as the file declares it. The bound is one of AtLeast,
// AtMost and NoWorseThan, as written; the others are "". By is the slack
// of NoWorseThan, "" when left out. Critical are the severities counted as
// critical, none for the summary's default; Each is "week", or a kind of
// marked window.
type Goal struct {
	ID          int64    `json:"-"`
	Name        string   `json:"name"`
	TeamID      int64    `json:"team_id,omitempty"`
	EveryTeam   bool     `json:"every_team,omitempty"`
	Measure     string   `json:"measure"`
	AtLeast     string   `json:"at_least,omitempty"`
	AtMost      string   `json:"at_most,omitempty"`
	NoWorseThan string   `json:"no_worse_than,omitempty"`
	By          string   `json:"by,omitempty"`
	Critical    []string `json:"critical,omitempty"`
	Each        string   `json:"each,omitempty"`
	Declared    bool     `json:"declared,omitempty"`
}

// Bound is a goal's bound: at_least, at_most or no_worse_than, and the
// number it holds the measure to (0 for no_worse_than).
func (g Goal) Bound() (string, float64) {
	switch {
	case g.AtLeast != "":
		n, _ := strconv.ParseFloat(g.AtLeast, 64)
		return "at_least", n
	case g.AtMost != "":
		n, _ := strconv.ParseFloat(g.AtMost, 64)
		return "at_most", n
	}
	return "no_worse_than", 0
}

// CriticalOf is the severities a goal counts as critical: its own, or the
// summary's default.
func (g Goal) CriticalOf() []string {
	if len(g.Critical) == 0 {
		return []string{"critical"}
	}
	return g.Critical
}

// GoalMeasure is what a measure is: whether higher is better, whether it
// needs the record (an incident written down, or an impact signal read,
// to tell a firing that caught nothing from one nobody wrote down), a
// critical.* one, a share (from 0 to 1), one of the incidents a team owns,
// a median of times (any number of seconds, negative when the alert came
// after), and what marked windows count of it (#90): with each: KIND, and
// with no_worse_than.
type GoalMeasure struct {
	Higher, Record, Critical, Share, Incidents, Seconds, Each, NoWorse bool
}

// GoalMeasures are the measures a goal can name: each a field of a summary
// row (/api/v1/summary), so a goal means what the summary means.
var GoalMeasures = map[string]GoalMeasure{
	"firings":                          {Each: true},
	"precision":                        {Higher: true, Record: true, Share: true, Each: true, NoWorse: true},
	"critical.firings":                 {Critical: true},
	"critical.share_caught":            {Higher: true, Record: true, Critical: true, Share: true},
	"refired.firings":                  {Each: true},
	"refired.share":                    {Share: true},
	"asleep.firings":                   {},
	"off_hours.firings":                {},
	"asleep.nothing":                   {Record: true},
	"off_hours.nothing":                {Record: true},
	"incidents.missed":                 {Record: true, Incidents: true, Each: true},
	"impacts.missed":                   {Record: true, Incidents: true, Each: true},
	"incidents.noisy_only":             {Record: true, Incidents: true, Each: true},
	"incidents.median_lead_seconds":    {Higher: true, Record: true, Incidents: true, Seconds: true, Each: true, NoWorse: true},
	"during_deploys.precision_without": {Higher: true, Record: true, Share: true},
}

// GoalMeasureNames are the measures, in the order the design lists them.
var GoalMeasureNames = []string{"firings", "precision", "critical.firings", "critical.share_caught", "refired.firings", "refired.share",
	"asleep.firings", "off_hours.firings", "asleep.nothing", "off_hours.nothing", "incidents.missed", "impacts.missed",
	"incidents.noisy_only", "incidents.median_lead_seconds", "during_deploys.precision_without"}

// Bounds on what --config declares of goals: 50 goals, and four lists of
// severities counted as critical among them, as each is a count of its own
// over the week's rules; a list of 32 at most, 64 bytes each, as the
// summary's critical= takes them.
const (
	MaxGoals            = 50
	MaxGoalCriticals    = 4
	MaxGoalCritical     = 32
	MaxGoalSeverityLen  = 64
	goalWeek            = "week"
	goalBefore          = "before"
	goalNameMost        = 64
	GoalTasksBy         = "lapilli"
	goalMissedKeyPrefix = "goal-missed."
	goalMadeKeyPrefix   = "goal-made."
	goalTaskKeyPrefix   = "goal-task."
)

// goalName is what a goal is named by: a small alphabet, as a label's
// value is safe in /metrics, a URL and a notice. markedKind is a kind of
// marked window, as docs/design/marked.md names one: 32 bytes at most.
var (
	goalName   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	markedKind = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
)

// CheckGoalName refuses what can't name a goal.
func CheckGoalName(name string) error {
	if !goalName.MatchString(name) || len(name) > goalNameMost {
		return invalid("a goal's name is lower-case letters, digits, _ and -, from a letter or a digit, %d bytes at most", goalNameMost)
	}
	return nil
}

// goalNumber reads a goal's number, as written.
func goalNumber(key, s string) (float64, error) {
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, invalid("%s is a number, such as 0.8 or 40", key)
	}
	return n, nil
}

// CheckGoal refuses a goal that breaks a rule of the design's, and fills
// in its defaults: exactly one bound, at_least, at_most or no_worse_than;
// a share's bound from 0 to 1, a count's from 0; by only with
// no_worse_than; critical only with a critical.* measure; each, week or a
// kind of marked window, never deploy. Goals over a period (each: KIND)
// wait for marked windows (#90), which this lapilli doesn't keep: refused,
// once every other rule is kept, with that said.
func CheckGoal(g *Goal) error {
	g.Name, g.Measure, g.Each = strings.TrimSpace(g.Name), strings.TrimSpace(g.Measure), strings.TrimSpace(g.Each)
	if err := CheckGoalName(g.Name); err != nil {
		return err
	}
	m, ok := GoalMeasures[g.Measure]
	if !ok {
		return invalid("measure %q: want one of %s", g.Measure, strings.Join(GoalMeasureNames, ", "))
	}
	n := 0
	for _, b := range []string{g.AtLeast, g.AtMost, g.NoWorseThan} {
		if b != "" {
			n++
		}
	}
	if n != 1 {
		return invalid("a goal has one bound: at_least, at_most or no_worse_than")
	}
	for _, b := range []struct{ key, v string }{{"at_least", g.AtLeast}, {"at_most", g.AtMost}} {
		if b.v == "" {
			continue
		}
		x, err := goalNumber(b.key, b.v)
		switch {
		case err != nil:
			return err
		case m.Share && (x < 0 || x > 1):
			return invalid("%s of %s is a share, from 0 to 1, such as 0.8", b.key, g.Measure)
		case !m.Share && !m.Seconds && x < 0:
			return invalid("%s of %s is a count, 0 or more", b.key, g.Measure)
		}
	}
	if g.By != "" {
		if g.NoWorseThan == "" {
			return invalid("by is the slack of no_worse_than: it goes with no_worse_than alone")
		}
		x, err := goalNumber("by", g.By)
		if err != nil {
			return err
		}
		if x < 0 || m.Share && x > 1 {
			return invalid("by is how much worse is no worse, 0 or more; of a share, up to 1")
		}
	}
	if g.NoWorseThan != "" && g.NoWorseThan != goalBefore {
		return invalid("no_worse_than is before: the four whole weeks before a period")
	}
	if len(g.Critical) > 0 {
		if !m.Critical {
			return invalid("critical is the severities a critical.* measure counts: it goes with critical.firings and critical.share_caught alone")
		}
		var cs []string
		for _, c := range g.Critical {
			c = strings.TrimSpace(c)
			if c == "" || len(c) > MaxGoalSeverityLen || len(cs) == MaxGoalCritical {
				return invalid("critical is %d severities at most, each %d bytes at most", MaxGoalCritical, MaxGoalSeverityLen)
			}
			if !slices.Contains(cs, c) {
				cs = append(cs, c)
			}
		}
		g.Critical = cs
	}
	if g.Each == "" {
		g.Each = goalWeek
	}
	switch {
	case g.Each == "deploy":
		return invalid("each: deploy: deploys are counted by during_deploys, and are too many and too short to be a period")
	case g.Each != goalWeek && !markedKind.MatchString(g.Each):
		return invalid("each is week, or a kind of marked window, such as sale")
	case g.Each != goalWeek && !m.Each:
		return invalid("%s isn't counted inside a marked window: with each: KIND, a goal is of firings, precision, refired.firings, incidents.missed, impacts.missed, incidents.noisy_only or incidents.median_lead_seconds", g.Measure)
	case g.NoWorseThan != "" && !m.NoWorse:
		return invalid("no_worse_than sets %s beside the weeks before, which only precision and incidents.median_lead_seconds can be", g.Measure)
	case g.NoWorseThan != "" && g.Each == goalWeek:
		return invalid("no_worse_than holds over a period: give each, a kind of marked window")
	case g.Each != goalWeek:
		return invalid("each: %s is a goal over a kind of marked window, which this lapilli doesn't keep yet: each takes week alone for now", g.Each)
	}
	return nil
}

// criticalKey is a list of severities as one text, to tell the lists apart.
func criticalKey(cs []string) string {
	s := slices.Clone(cs)
	slices.Sort(s)
	return strings.Join(s, "\x00")
}

// CriticalLists are the different lists of severities goals count as
// critical, each once, the default among them when one needs it.
func CriticalLists(gs []Goal) [][]string {
	var out [][]string
	seen := map[string]bool{}
	for _, g := range gs {
		if !GoalMeasures[g.Measure].Critical {
			continue
		}
		cs := g.CriticalOf()
		if k := criticalKey(cs); !seen[k] {
			seen[k] = true
			out = append(out, cs)
		}
	}
	return out
}

// CriticalKey is the key a list of severities is counted under.
func CriticalKey(cs []string) string { return criticalKey(cs) }

// CheckGoals refuses what a file's goals break together: more than
// MaxGoals, a name given twice, more than MaxGoalCriticals lists of
// severities. Each is checked on its own by CheckGoal.
func CheckGoals(gs []Goal) error {
	if len(gs) > MaxGoals {
		return invalid("a config declares %d goals at most", MaxGoals)
	}
	if len(CriticalLists(gs)) > MaxGoalCriticals {
		return invalid("the goals count %d lists of severities as critical at most: give the critical.* goals fewer different lists", MaxGoalCriticals)
	}
	return nil
}

// DeclaredGoal is a goal --config declares: Team is its team's name, ""
// for everyone's, "*" for each team's own.
type DeclaredGoal struct {
	Goal
	Team string
	Line int
	Bad  error
}

// KindGoal is a goal, as a DeclaredError names it.
const KindGoal = "goal"

const goalCols = "id, name, team_id, every_team, measure, at_least, at_most, no_worse_than, slack, critical, each, declared"

func scanGoal(s scanner) (Goal, error) {
	var g Goal
	var critical string
	if err := s.Scan(&g.ID, &g.Name, &g.TeamID, &g.EveryTeam, &g.Measure, &g.AtLeast, &g.AtMost, &g.NoWorseThan, &g.By, &critical, &g.Each, &g.Declared); err != nil {
		return g, err
	}
	if err := json.Unmarshal([]byte(critical), &g.Critical); err != nil {
		return g, err
	}
	if len(g.Critical) == 0 {
		g.Critical = nil
	}
	return g, nil
}

// Goals are the goals the ledger holds, by name.
func (l *Ledger) Goals(ctx context.Context) ([]Goal, error) { return goals(ctx, l.r) }

func goals(ctx context.Context, q queryer) ([]Goal, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+goalCols+" FROM goals ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Goal
	for rows.Next() {
		g, err := scanGoal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// sameGoal says two goals are kept alike.
func sameGoal(a, b Goal) bool {
	return a.Name == b.Name && a.TeamID == b.TeamID && a.EveryTeam == b.EveryTeam && a.Measure == b.Measure && a.AtLeast == b.AtLeast &&
		a.AtMost == b.AtMost && a.NoWorseThan == b.NoWorseThan && a.By == b.By && slices.Equal(a.Critical, b.Critical) && a.Each == b.Each
}

// PruneGoalMarks lets go of what was told of goals, and what tasks were
// made for, of weeks before Monday before: of goals no longer declared
// among them. Kept until then, a goal taken out of the file for a while
// (a ConfigMap rolled back, serve started once without --config) and put
// back, or given another team, tells no week again, nor makes its task
// again. The meta keys under a prefix are those after it, up to it with
// its last byte, a dot, raised to a slash: a goal's name holds _, which
// LIKE would read as any character.
func (l *Ledger) PruneGoalMarks(ctx context.Context, before int64) (int, error) {
	n := 0
	err := l.writeAside(ctx, func(t *txn) error {
		n = 0
		for _, p := range []string{goalMissedKeyPrefix, goalMadeKeyPrefix} {
			rows, err := t.QueryContext(t.ctx, "SELECT key, value FROM meta WHERE key > ? AND key < ?", p, strings.TrimSuffix(p, ".")+"/")
			if err != nil {
				return err
			}
			var gone []string
			for rows.Next() {
				var k, v string
				if err := rows.Scan(&k, &v); err != nil {
					rows.Close()
					return err
				}
				if w, err := strconv.ParseInt(v, 10, 64); err != nil || w < before {
					gone = append(gone, k)
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for _, k := range gone {
				if _, err := t.ExecContext(t.ctx, "DELETE FROM meta WHERE key = ?", k); err != nil {
					return err
				}
			}
			n += len(gone)
		}
		return nil
	})
	return n, err
}

// declareGoals keeps the goals to the file: each matched by its name, made
// or changed; one that can't be kept (it breaks a rule, or names a team
// the file doesn't declare and the ledger doesn't hold) skipped, said, and
// kept as it was; and every goal the file no longer declares deleted,
// pruned or not, as no form in the UI takes one back. Without a file
// (left.NoFile), there are none. What was told of a goal, and which weeks
// tasks were made for, are kept by its name and each team's id, deleted or
// changed, until they are weeks old (PruneGoalMarks); its tasks keep its
// name, as text.
func (l *Ledger) declareGoals(t *txn, want []DeclaredGoal, team resolver, r *Reconciled, skip func(string, int, string, error)) error {
	have, err := goals(t.ctx, t)
	if err != nil {
		return err
	}
	byName := map[string]int{}
	for i, g := range have {
		byName[g.Name] = i
	}
	matched := make([]bool, len(have))
	seen := map[string]int{}
	type todo struct {
		g Goal
		i int // -1 for a new one
	}
	var todos []todo
	for _, dg := range want {
		g := dg.Goal
		g.ID, g.Declared = 0, true
		err := dg.Bad
		if err == nil {
			err = CheckGoal(&g)
		}
		if err == nil {
			if line, ok := seen[g.Name]; ok {
				err = invalid("a goal called %s is declared on line %d already: a goal's name is unique, across everyone's and every team's", g.Name, line)
			}
		}
		if err == nil {
			switch tn := strings.TrimSpace(dg.Team); tn {
			case "*":
				g.TeamID, g.EveryTeam = 0, true
			default:
				g.TeamID, _, err = team(dg.Team)
			}
		}
		i, there := byName[strings.TrimSpace(g.Name)]
		if err != nil {
			if there {
				matched[i] = true // kept as it was
			}
			skip(KindGoal, dg.Line, dg.Name, err)
			continue
		}
		seen[g.Name] = dg.Line
		if !there {
			i = -1
		} else {
			matched[i] = true
		}
		todos = append(todos, todo{g, i})
	}
	now := time.Now().Unix()
	for i, h := range have {
		if matched[i] {
			continue
		}
		if _, err := t.ExecContext(t.ctx, "DELETE FROM goals WHERE id = ?", h.ID); err != nil {
			return err
		}
		if err := change(t, ConfigBy, "goal "+h.Name+" deleted, as the file no longer declares it", h, nil); err != nil {
			return err
		}
		r.Deleted++
	}
	for _, td := range todos {
		g := td.g
		critical, _ := json.Marshal(orNoStrings(g.Critical))
		if td.i < 0 {
			if _, err := t.ExecContext(t.ctx, `INSERT INTO goals (name, team_id, every_team, measure, at_least, at_most, no_worse_than, slack, critical, each, declared, updated)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
				g.Name, g.TeamID, g.EveryTeam, g.Measure, g.AtLeast, g.AtMost, g.NoWorseThan, g.By, string(critical), g.Each, now); err != nil {
				return err
			}
			if err := change(t, ConfigBy, "goal "+g.Name+" kept", nil, g); err != nil {
				return err
			}
			r.Made++
			continue
		}
		h := have[td.i]
		if sameGoal(h, g) {
			continue
		}
		if _, err := t.ExecContext(t.ctx, `UPDATE goals SET team_id = ?, every_team = ?, measure = ?, at_least = ?, at_most = ?, no_worse_than = ?, slack = ?,
			critical = ?, each = ?, declared = 1, updated = ? WHERE id = ?`,
			g.TeamID, g.EveryTeam, g.Measure, g.AtLeast, g.AtMost, g.NoWorseThan, g.By, string(critical), g.Each, now, h.ID); err != nil {
			return err
		}
		if err := change(t, ConfigBy, "goal "+g.Name+" kept", h, g); err != nil {
			return err
		}
		r.Changed++
	}
	return nil
}

func orNoStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// GoalTeamKey is how a goal's team is named in what is kept of it: its
// team's id, 0 for everyone's, as a rename keeps it.
func goalTeamKey(name string, team int64) string { return name + "." + strconv.FormatInt(team, 10) }

// GoalMarks are, by a goal's name and its team's id (GoalKey), the latest
// week goal.missed was told of (told), and the latest a task was made for
// or said missed on (made): read at once, so that the leader's work over
// every team reads them once a round, not once a team.
func (l *Ledger) GoalMarks(ctx context.Context) (told, made map[string]int64, err error) {
	read := func(prefix string) (map[string]int64, error) {
		rows, err := l.r.QueryContext(ctx, "SELECT key, value FROM meta WHERE key > ? AND key < ?", prefix, strings.TrimSuffix(prefix, ".")+"/")
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := map[string]int64{}
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				return nil, err
			}
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				out[strings.TrimPrefix(k, prefix)] = n
			}
		}
		return out, rows.Err()
	}
	if told, err = read(goalMissedKeyPrefix); err != nil {
		return nil, nil, err
	}
	made, err = read(goalMadeKeyPrefix)
	return told, made, err
}

// GoalKey is what is kept of a goal for a team is keyed on: its name and
// its team's id, 0 for everyone's, as a rename keeps it.
func GoalKey(name string, team int64) string { return goalTeamKey(name, team) }

// TellGoalMissed tells goal.missed of a goal for its teams (0 for
// everyone's), of week, a Monday's day, once each, in one write: told, and
// each team's mark set to the week, as the outbox's TellOnce keeps one;
// a team marked for the week, or a later one, already is passed over
// (its caller leaves it out of told). A goal of each team's tells its
// teams missed in one notice.
func (l *Ledger) TellGoalMissed(ctx context.Context, name string, teams []int64, week int64, told []Told) error {
	return l.writeAside(ctx, func(t *txn) error {
		now := wallNow()
		for _, x := range told {
			about := map[string]any{"event": "goal.missed", "at": now.UTC()}
			maps.Copy(about, x.About)
			body, err := json.Marshal(about)
			if err != nil {
				return err
			}
			if _, err := t.ExecContext(t.ctx, t.d.pick("INSERT INTO outbox (dest, event, body, created, next_at) VALUES (?, ?, ?, ?, ?)", outboxDue),
				x.Dest, "goal.missed", string(body), now.Unix(), now.Unix()); err != nil {
				return err
			}
		}
		for _, team := range teams {
			key := goalMissedKeyPrefix + goalTeamKey(name, team)
			was, err := l.meta(t.ctx, t, key)
			if err != nil {
				return err
			}
			if n, _ := strconv.ParseInt(was, 10, 64); n >= week {
				continue
			}
			if err := setMeta(t.ctx, t, key, strconv.FormatInt(week, 10)); err != nil {
				return err
			}
		}
		return nil
	})
}

// OpenGoalTasks are the tasks open or being done about goals, by their
// goal and team (GoalKey), each the earliest: all of them, a goal and a
// team at a time each, read through the index of the tasks about goals.
func (l *Ledger) OpenGoalTasks(ctx context.Context) (map[string]int64, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT id, goal, coalesce(team_id, 0) FROM tasks WHERE goal != '' AND status IN ('open', 'doing') ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id, team int64
		var goal string
		if err := rows.Scan(&id, &goal, &team); err != nil {
			return nil, err
		}
		if _, ok := out[goalTeamKey(goal, team)]; !ok {
			out[goalTeamKey(goal, team)] = id
		}
	}
	return out, rows.Err()
}

// goalsShownKey keeps what the leader worked out of the goals last.
const goalsShownKey = "goals-shown"

// KeepGoalsShown keeps what the leader worked out of the goals after a
// round, for the replicas that follow and their /metrics: written aside,
// as no page of the ledger's changes with it, and only when it changed.
func (l *Ledger) KeepGoalsShown(ctx context.Context, b []byte) error {
	return l.writeAside(ctx, func(t *txn) error { return setMeta(t.ctx, t, goalsShownKey, string(b)) })
}

// GoalsShown is what KeepGoalsShown kept last, nil for nothing.
func (l *Ledger) GoalsShown(ctx context.Context) ([]byte, error) {
	v, err := l.meta(ctx, l.r, goalsShownKey)
	if err != nil || v == "" {
		return nil, err
	}
	return []byte(v), nil
}

// GoalTaskState is what a goal's task was made for, and what has been said
// on it since, week by week (Mondays, as days): "missed" for a week said
// missed (the one it was made for, or missed again), "met" for one said
// met, "no_longer" for one missed and said no longer missed.
type GoalTaskState struct {
	Week int64            `json:"week"`
	Said map[int64]string `json:"said,omitempty"`
}

func goalTaskKey(id int64) string { return goalTaskKeyPrefix + strconv.FormatInt(id, 10) }

// GoalTask is what a goal's task was made for, and what was said on it;
// ok false for a task made otherwise than for a week (through the API).
func (l *Ledger) GoalTask(ctx context.Context, id int64) (GoalTaskState, bool, error) {
	var st GoalTaskState
	v, err := l.meta(ctx, l.r, goalTaskKey(id))
	if err != nil || v == "" {
		return st, false, err
	}
	return st, json.Unmarshal([]byte(v), &st) == nil, nil
}

// ErrGoalTaskOpen is a task for a goal's team made while one is open.
var ErrGoalTaskOpen = errors.New("a task for this goal and team is open already")

// MakeGoalTask makes t, a task about a goal missed (t.Goal, of t.TeamID),
// for the week of Monday week, in one write with what is kept of it: the
// week is noted as the latest one was made for, so that Lapilli doesn't
// make another for it once this one is done or dropped. A task about the
// goal and team open already is refused (ErrGoalTaskOpen), with its id.
func (l *Ledger) MakeGoalTask(ctx context.Context, t Task, week int64, by string) (int64, error) {
	if t.Goal == "" {
		return 0, invalid("a goal's task names its goal")
	}
	var id int64
	err := l.write(ctx, func(tx *txn) error {
		id = 0
		var open int64
		err := tx.QueryRowContext(tx.ctx, "SELECT id FROM tasks WHERE goal = ? AND goal != '' AND coalesce(team_id, 0) = ? AND status IN ('open', 'doing') ORDER BY id LIMIT 1",
			t.Goal, t.TeamID).Scan(&open)
		switch {
		case err == nil:
			id = open
			return ErrGoalTaskOpen
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if id, err = l.putTaskIn(tx, 0, func(Task) (Task, error) { return t, nil }, by); err != nil {
			return err
		}
		st, _ := json.Marshal(GoalTaskState{Week: week, Said: map[int64]string{week: "missed"}})
		if err := setMeta(tx.ctx, tx, goalTaskKey(id), string(st)); err != nil {
			return err
		}
		key := goalMadeKeyPrefix + goalTeamKey(t.Goal, t.TeamID)
		was, err := l.meta(tx.ctx, tx, key)
		if err != nil {
			return err
		}
		if n, _ := strconv.ParseInt(was, 10, 64); n >= week {
			return nil
		}
		return setMeta(tx.ctx, tx, key, strconv.FormatInt(week, 10))
	})
	return id, err
}

// SayOnGoalTask comments on a goal's task, as by, and keeps what was said
// (st), in one write: neither without the other. A week said missed on it
// is one a task was made for: once it is done or dropped, Lapilli makes
// none for that week again, but for the next missed.
func (l *Ledger) SayOnGoalTask(ctx context.Context, id int64, body string, st GoalTaskState, by string) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	var latest int64
	for w, said := range st.Said {
		if said == "missed" {
			latest = max(latest, w)
		}
	}
	return l.write(ctx, func(t *txn) error {
		var goal string
		var team int64
		if err := t.QueryRowContext(t.ctx, "SELECT goal, coalesce(team_id, 0) FROM tasks WHERE id = ?", id).Scan(&goal, &team); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if goal != "" && latest > 0 {
			key := goalMadeKeyPrefix + goalTeamKey(goal, team)
			was, err := l.meta(t.ctx, t, key)
			if err != nil {
				return err
			}
			if n, _ := strconv.ParseInt(was, 10, 64); n < latest {
				if err := setMeta(t.ctx, t, key, strconv.FormatInt(latest, 10)); err != nil {
					return err
				}
			}
		}
		now := time.Now().Unix()
		c := Comment{On: "task", Key: strconv.FormatInt(id, 10), Body: body}
		if err := t.QueryRowContext(t.ctx, "INSERT INTO comments (on_kind, on_key, body, created, created_by, updated) VALUES (?, ?, ?, ?, ?, ?) RETURNING id",
			c.On, c.Key, c.Body, now, by, now).Scan(&c.ID); err != nil {
			return err
		}
		c.Created, c.By, c.Updated = unix(now), by, unix(now)
		if err := l.tell(t, "comment.written", by, map[string]any{"comment": c}); err != nil {
			return err
		}
		if err := change(t, by, fmt.Sprintf("comment on task %d", id), nil, nil); err != nil {
			return err
		}
		return setMeta(t.ctx, t, goalTaskKey(id), string(b))
	})
}

// ForgetGoalTask lets go of what was kept of a goal's task once it is done
// or dropped: nothing more is said on it.
func (l *Ledger) ForgetGoalTask(ctx context.Context, id int64) error {
	return l.writeAside(ctx, func(t *txn) error {
		_, err := t.ExecContext(t.ctx, "DELETE FROM meta WHERE key = ?", goalTaskKey(id))
		return err
	})
}

// GoalTasksKept are the ids of the tasks something is kept of, said on as
// their goals are missed and met.
func (l *Ledger) GoalTasksKept(ctx context.Context) ([]int64, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT key FROM meta WHERE key > ? AND key < ?", goalTaskKeyPrefix, strings.TrimSuffix(goalTaskKeyPrefix, ".")+"/")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		if n, err := strconv.ParseInt(strings.TrimPrefix(k, goalTaskKeyPrefix), 10, 64); err == nil {
			out = append(out, n)
		}
	}
	return out, rows.Err()
}
