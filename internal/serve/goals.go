package serve

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// Goals (docs/design/goals.md): what an operations team holds its alerting
// to, declared in --config under goals:, each a bound on one of the
// summary's numbers for everyone, a team, or each team. Each whole week is
// met, missed or not known, worked out from the summary's rows wherever it
// is read, so that an incident written down later turns a week's goal as it
// turns the week's stones. After each round the leader works out the last
// whole week and the eleven before it, for what is told (goal.missed), the
// tasks made and said on, and /metrics, which reads what it worked out.

// Results of a goal's week.
const (
	goalMet     = "met"
	goalMissed  = "missed"
	goalUnknown = "unknown"
	goalOpen    = "open"
)

// goalNoLonger is a week said missed on a goal's task, and said no longer
// missed since.
const goalNoLonger = "no_longer"

// Why a whole week's goal isn't known.
const (
	whyNoRecord   = "no_record"
	whyNoValue    = "no_value"
	whyNotWhole   = "not_whole"
	whyByCatching = "team_owns_by_catching"
)

// summaryGoal is a goal in a summary row: its bound, the row's value of
// its measure, and the week's result: met, missed, unknown (with why) or
// open, for a row that is partial. Over says, of an open week, that a
// count is past its at_most already, as more firings can only add to it.
type summaryGoal struct {
	Name     string   `json:"name"`
	Measure  string   `json:"measure"`
	Critical []string `json:"critical,omitempty"`
	AtLeast  *float64 `json:"at_least,omitempty"`
	AtMost   *float64 `json:"at_most,omitempty"`
	Value    *float64 `json:"value"`
	Result   string   `json:"result"`
	Why      string   `json:"why,omitempty"`
	Over     bool     `json:"over,omitempty"`
}

// rowGoals are the goals a row is checked against: everyone's, in a row
// for everyone; a team's own and each team's (team: "*"), in a team's.
// Goals over a period aren't a week's.
func rowGoals(gs []ledger.Goal, team *ledger.Team) []ledger.Goal {
	var out []ledger.Goal
	for _, g := range gs {
		switch {
		case g.Each != "week":
		case team == nil && g.TeamID == 0 && !g.EveryTeam:
			out = append(out, g)
		case team != nil && (g.EveryTeam || g.TeamID == team.ID):
			out = append(out, g)
		}
	}
	return out
}

// goalValue is a row's value of a goal's measure: nil when the summary has
// none (a share with nothing to divide, a median with nothing caught, the
// hours not given).
func goalValue(g ledger.Goal, row *summaryRow) *float64 {
	n := func(v int) *float64 {
		f := float64(v)
		return &f
	}
	hours := func(h *summaryHours, nothing bool) *float64 {
		if h == nil {
			return nil
		}
		if nothing {
			return n(h.Nothing)
		}
		return n(h.Firings)
	}
	crit := row.crit[ledger.CriticalKey(g.CriticalOf())]
	switch g.Measure {
	case "firings":
		return n(row.Firings)
	case "precision":
		return row.Precision
	case "critical.firings":
		return n(crit.Firings)
	case "critical.share_caught":
		return crit.Share
	case "refired.firings":
		return n(row.Refired.Firings)
	case "refired.share":
		return row.Refired.Share
	case "asleep.firings":
		return hours(row.Asleep, false)
	case "off_hours.firings":
		return hours(row.OffHours, false)
	case "asleep.nothing":
		return hours(row.Asleep, true)
	case "off_hours.nothing":
		return hours(row.OffHours, true)
	case "incidents.missed":
		return n(row.Incidents.Missed)
	case "impacts.missed":
		return n(row.Impacts.Missed)
	case "incidents.noisy_only":
		return n(row.Incidents.NoisyOnly)
	case "incidents.median_lead_seconds":
		if row.Incidents.MedianLead == nil {
			return nil
		}
		f := float64(*row.Incidents.MedianLead)
		return &f
	case "during_deploys.precision_without":
		return row.DuringDeploys.PrecisionWithout
	}
	return nil
}

// goalOn works a goal out for a row. A week under way is open, with its
// value so far. A whole week is not known, with why, before it is met or
// missed: the week isn't whole in the ledger (the one its first firing
// fell in); a goal on incidents of a team with no labels, which owns only
// what its rules caught and so has never missed one; a measure that needs
// the record in a week with none; a value that isn't there. A value not
// known isn't given: it would read as one.
func goalOn(g ledger.Goal, row *summaryRow) summaryGoal {
	m := ledger.GoalMeasures[g.Measure]
	out := summaryGoal{Name: g.Name, Measure: g.Measure, Critical: g.Critical}
	kind, target := g.Bound()
	switch kind {
	case "at_least":
		out.AtLeast = &target
	case "at_most":
		out.AtMost = &target
	}
	v := goalValue(g, row)
	out.Value = v
	unknown := func(why string) {
		out.Result, out.Why, out.Value = goalUnknown, why, nil
	}
	switch {
	case row.Partial:
		out.Result = goalOpen
		out.Over = kind == "at_most" && !m.Share && !m.Seconds && !m.Higher && v != nil && *v > target
	case !row.whole:
		unknown(whyNotWhole)
	case m.Incidents && row.byCatching:
		unknown(whyByCatching)
	case m.Record && row.NoRecord:
		unknown(whyNoRecord)
	case v == nil:
		unknown(whyNoValue)
	case kind == "at_least" && *v >= target, kind == "at_most" && *v <= target:
		out.Result = goalMet
	default:
		out.Result = goalMissed
	}
	return out
}

// goalsCache keeps the goals the ledger holds until it changes: every
// summary reads them.
type goalsCache struct {
	sync.Mutex
	gen int64
	ok  bool
	gs  []ledger.Goal
}

// goalsNow are the goals the ledger holds, as of now.
func (s *Server) goalsNow(ctx context.Context) ([]ledger.Goal, error) {
	gen := s.l.Gen()
	s.goalsC.Lock()
	if s.goalsC.ok && s.goalsC.gen == gen {
		gs := s.goalsC.gs
		s.goalsC.Unlock()
		return gs, nil
	}
	s.goalsC.Unlock()
	gs, err := s.l.Goals(ctx)
	if err != nil {
		return nil, err
	}
	s.goalsC.Lock()
	s.goalsC.gen, s.goalsC.ok, s.goalsC.gs = gen, true, gs
	s.goalsC.Unlock()
	return gs, nil
}

// goalLine is a goal for a team (Team nil for everyone's), week by week,
// the earliest first.
type goalLine struct {
	Goal  ledger.Goal
	Team  *ledger.Team
	Weeks []goalWeek
}

// goalWeek is a goal's week.
type goalWeek struct {
	From, To time.Time
	summaryGoal
}

// TeamName is the line's team's name, "" for everyone's.
func (g goalLine) TeamName() string {
	if g.Team == nil {
		return ""
	}
	return g.Team.Name
}

// week is the line's week from Monday from, ok false when it hasn't it.
func (g goalLine) week(from time.Time) (goalWeek, bool) {
	for _, w := range g.Weeks {
		if w.From.Equal(from) {
			return w, true
		}
	}
	return goalWeek{}, false
}

// goalLines are the goals of the summary's rows, a line for each goal and
// team, in the order of the goals, everyone's first, then each team's.
func goalLines(sum *summary, gs []ledger.Goal, who []*ledger.Team) []goalLine {
	type key struct {
		goal string
		team string
	}
	at := map[key]int{}
	var out []goalLine
	for _, g := range gs {
		for _, t := range who {
			if len(rowGoals([]ledger.Goal{g}, t)) == 0 {
				continue
			}
			k := key{g.Name, ""}
			if t != nil {
				k.team = t.Name
			}
			if _, ok := at[k]; !ok {
				at[k] = len(out)
				out = append(out, goalLine{Goal: g, Team: t})
			}
		}
	}
	for _, row := range sum.Rows {
		for _, sg := range row.Goals {
			i, ok := at[key{sg.Name, row.Team}]
			if !ok {
				continue
			}
			out[i].Weeks = append(out[i].Weeks, goalWeek{From: row.From, To: row.To, summaryGoal: sg})
		}
	}
	return out
}

// goalsWho are whom the goals are of: everyone, when a goal is
// everyone's; each team a goal names; every team, for a goal of each
// team's. of keeps those of one team alone (and everyone's), nil for all.
func goalsWho(gs []ledger.Goal, own *owners, of *ledger.Team) (who []*ledger.Team, every bool) {
	everyone := false
	named := map[int64]bool{}
	for _, g := range gs {
		switch {
		case g.Each != "week":
		case g.EveryTeam:
			every = true
		case g.TeamID == 0:
			everyone = true
		default:
			named[g.TeamID] = true
		}
	}
	if everyone {
		who = append(who, nil)
	}
	for i := range own.teams {
		t := &own.teams[i]
		switch {
		case of != nil && t.ID != of.ID:
		case every, named[t.ID]:
			who = append(who, t)
		}
	}
	return who, every && of == nil
}

// goalTable are the goals' weeks from Monday w1 to Monday w2, for whom:
// each goal for everyone and each team it is of, as read now. Kept until
// the ledger changes: the pages that show goals read the same weeks.
func (s *Server) goalTable(ctx context.Context, w1, w2 int64, who []*ledger.Team) ([]goalLine, error) {
	gs, err := s.goalsNow(ctx)
	if err != nil || len(gs) == 0 || len(who) == 0 {
		return nil, err
	}
	var ids []string
	for _, t := range who {
		if t == nil {
			ids = append(ids, "0")
		} else {
			ids = append(ids, strconv.FormatInt(t.ID, 10))
		}
	}
	k := fmt.Sprint(w1, "/", w2, "/", strings.Join(ids, ","))
	gen := s.l.Gen()
	s.goalRows.Lock()
	if s.goalRows.gen == gen {
		if ls, ok := s.goalRows.by[k]; ok {
			s.goalRows.Unlock()
			return ls, nil
		}
	} else {
		s.goalRows.gen, s.goalRows.by = gen, nil
	}
	s.goalRows.Unlock()
	sum, err := s.summaryOf(ctx, w1, w2, who, []string{"critical"})
	if err != nil {
		return nil, err
	}
	ls := goalLines(sum, gs, who)
	s.goalRows.Lock()
	if s.goalRows.gen == gen {
		if s.goalRows.by == nil || len(s.goalRows.by) >= goalTablesKept {
			s.goalRows.by = map[string][]goalLine{}
		}
		s.goalRows.by[k] = ls
	}
	s.goalRows.Unlock()
	return ls, nil
}

// goalTablesKept is how many goal tables are kept at once, of one
// generation of the ledger.
const goalTablesKept = 16

// goalRowsCache keeps goal tables, of one generation of the ledger.
type goalRowsCache struct {
	sync.Mutex
	gen int64
	by  map[string][]goalLine
}

// The texts of a goal, as the pages, the digest and what is told say it.

// goalMeasureNames are each measure as a person reads it.
var goalMeasureNames = map[string]string{
	"firings":                          "firings",
	"precision":                        "precision",
	"critical.firings":                 "critical firings",
	"critical.share_caught":            "critical firings that caught something",
	"refired.firings":                  "firings that re-fired",
	"refired.share":                    "the share of firings that re-fired",
	"asleep.firings":                   "firings while people slept",
	"off_hours.firings":                "firings off hours",
	"asleep.nothing":                   "firings while people slept that caught nothing",
	"off_hours.nothing":                "firings off hours that caught nothing",
	"incidents.missed":                 "incidents missed",
	"impacts.missed":                   "impact windows missed",
	"incidents.noisy_only":             "incidents caught only by noisy rules",
	"incidents.median_lead_seconds":    "the median lead",
	"during_deploys.precision_without": "precision without the firings during deploys",
}

// goalWhys are why a week's goal isn't known, as a person reads it.
var goalWhys = map[string]string{
	whyNoRecord:   "not known: no incident written down to judge the firings by",
	whyNoValue:    "not known: nothing to count it of that week",
	whyNotWhole:   "not known: the ledger holds only part of the week",
	whyByCatching: "not known: the team has no labels, so it owns only the incidents its rules caught: give it labels",
}

// goalMeasureText is a measure as a person reads it, in l.
func goalMeasureText(l, measure string) string { return tr(l, goalMeasureNames[measure]) }

// goalNumber is a goal's number as a page shows it: a share in percent, to
// a tenth where a whole one would hide a miss; a lead as a time; a count as
// it is.
func goalNumber(measure string, v float64) string { return goalNumberTo(measure, v, 0) }

// goalNumberTo is goalNumber, a share cut to its tenth of a percent toward
// dir: down (-1), up (+1), or to the nearest (0). A value missed is cut
// away from its bound, down for at_least and up for at_most, so that it
// never reads as the bound it missed: 0.7995 of at_least 0.8 is 79.9%, not
// 80%. A share a hair off a tenth, as a quotient is, is that tenth.
func goalNumberTo(measure string, v float64, dir int) string {
	m := ledger.GoalMeasures[measure]
	switch {
	case m.Share:
		r := 1000 * v
		if math.Abs(r-math.Round(r)) < 1e-6 {
			r = math.Round(r)
		}
		switch {
		case dir < 0:
			r = math.Floor(r)
		case dir > 0:
			r = math.Ceil(r)
		default:
			r = math.Round(r)
		}
		return strconv.FormatFloat(r/10, 'f', -1, 64) + "%"
	case m.Seconds:
		return verdict.Delay(time.Duration(v) * time.Second)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// goalValueText is a week's value, "–" for none.
func goalValueText(measure string, v *float64) string {
	if v == nil {
		return "–"
	}
	return goalNumber(measure, *v)
}

// missedTo is the way a value missed is cut: away from the bound.
func missedTo(kind string) int {
	if kind == "at_most" {
		return +1
	}
	return -1
}

// goalWeekValue is a week's value as a page shows it, cut away from its
// bound when the week missed it.
func goalWeekValue(w summaryGoal) string {
	if w.Value == nil {
		return "–"
	}
	if w.Result != goalMissed {
		return goalNumber(w.Measure, *w.Value)
	}
	kind := "at_least"
	if w.AtMost != nil {
		kind = "at_most"
	}
	return goalNumberTo(w.Measure, *w.Value, missedTo(kind))
}

// goalMissedValue is a value a goal missed, cut away from its bound.
func goalMissedValue(g ledger.Goal, v *float64) string {
	if v == nil {
		return "–"
	}
	kind, _ := g.Bound()
	return goalNumberTo(g.Measure, *v, missedTo(kind))
}

// goalTarget is a goal's bound, as a person reads it: a share as the
// file wrote it, in percent, unrounded, so that a value missed, cut away
// from it, never reads as it.
func goalTarget(l string, g ledger.Goal) string {
	kind, n := g.Bound()
	text := goalNumber(g.Measure, n)
	if ledger.GoalMeasures[g.Measure].Share {
		text = strconv.FormatFloat(math.Round(n*1e12)/1e10, 'f', -1, 64) + "%"
	}
	switch kind {
	case "at_least":
		return tr(l, "at least %s", text)
	case "at_most":
		return tr(l, "at most %s", text)
	}
	return tr(l, "no worse than before")
}

// goalResultText is a week's result, as a person reads it.
func goalResultText(l string, w summaryGoal) string {
	switch w.Result {
	case goalMet:
		return tr(l, "met")
	case goalMissed:
		return tr(l, "not met")
	case goalOpen:
		if w.Over {
			return tr(l, "over so far")
		}
		return tr(l, "so far")
	}
	if why, ok := goalWhys[w.Why]; ok {
		return tr(l, why)
	}
	return tr(l, "not known")
}

// goalMissedText says a goal was missed in a week: its value and its
// target, of a team's or everyone's.
func goalMissedText(l string, g ledger.Goal, team string, week time.Time, v *float64, loc *time.Location) string {
	day := week.In(loc).Format("2006-01-02")
	if team != "" {
		return tr(l, "Goal %s of %s missed in the week of %s: %s was %s; the goal is %s.", g.Name, verdict.Clean(team), day,
			goalMeasureText(l, g.Measure), goalMissedValue(g, v), goalTarget(l, g))
	}
	return tr(l, "Goal %s missed in the week of %s: %s was %s; the goal is %s.", g.Name, day,
		goalMeasureText(l, g.Measure), goalMissedValue(g, v), goalTarget(l, g))
}

// goalTeamsNamed is how many teams a notice of a goal of each team's
// names, those missed first in the order of the teams; the rest are
// counted.
var goalTeamsNamed = 50

// goalMissedTeamsText says a goal of each team's was missed in a week by
// teams: how many, the first ten of them with their values, and its target.
func goalMissedTeamsText(l string, g ledger.Goal, week time.Time, teams []goalNoticeTeam, more int, loc *time.Location) string {
	var named []string
	for _, x := range teams[:min(len(teams), 10)] {
		named = append(named, tr(l, "%s (%s)", verdict.Clean(x.Team), goalMissedValue(g, x.Value)))
	}
	list := strings.Join(named, ", ")
	if n := len(teams) + more - len(named); n > 0 {
		list += " " + tr(l, "and %d more", n)
	}
	return tr(l, "Goal %s missed in the week of %s by %d teams: %s; the goal is %s.", g.Name, week.In(loc).Format("2006-01-02"), len(teams)+more,
		list, goalTarget(l, g))
}

// The leader's work after a round (docs/design/goals.md, "When it is
// worked out").

// goalsShown is what the leader worked out of the goals after its last
// round, for /metrics, the Run history page and the replicas that follow,
// which read it and work nothing out: each goal's last whole week for
// everyone and each team it is of, missed first, maxGoalSeries at most
// (More are left out); how many of its whole weeks it met, of the twelve
// before this one; and the tasks for goals missed that wait for a later
// run.
type goalsShown struct {
	At      time.Time   `json:"at"`
	Week    time.Time   `json:"week"`
	Lines   []goalShown `json:"lines,omitempty"`
	More    int         `json:"more,omitempty"`
	Waiting int         `json:"tasks_waiting,omitempty"`
}

// goalsBrief is what the leader's status says of what it worked out of the
// goals: which (Gen, a sum of it), the week, how many lines, and the tasks
// that wait. The rest, up to maxGoalSeries lines, is kept in the ledger
// when it changes (KeepGoalsShown), not in the status the leader writes
// every few seconds; a replica that follows reads it from there when Gen
// moves.
type goalsBrief struct {
	Gen     string    `json:"gen"`
	Week    time.Time `json:"week"`
	Lines   int       `json:"lines"`
	Waiting int       `json:"tasks_waiting,omitempty"`
}

// goalShown is a goal for a team, as the leader last worked it out.
type goalShown struct {
	Goal   string   `json:"goal"`
	Team   string   `json:"team,omitempty"`
	Result string   `json:"result"`
	Value  *float64 `json:"value,omitempty"`
	Met    int      `json:"weeks_met"`
}

// maxGoalSeries is how many goals for a team /metrics gives each family a
// series for at most, those missed first, as the incident series are
// bounded.
const maxGoalSeries = 500

// goalTasksARun is how many tasks for goals missed a run makes at most
// (--goal-tasks), those found open already as it makes them counted; the
// rest are made by the runs after. goalSaysARun is how many comments on
// goals' tasks it writes at most. Variables, for tests.
var (
	goalTasksARun = 20
	goalSaysARun  = 20
)

// goalsKept is what the leader worked out last, and of what: the ledger's
// generation and the week, so that a round that changed neither works
// nothing out again; the sum of it, and the sum last kept in the ledger;
// and the week the marks of weeks long past were last let go.
type goalsKept struct {
	sync.Mutex
	key, sum, kept string
	shown          *goalsShown
	pruned         int64
}

// goalsShownNow is what this replica worked out last, as the leader, nil
// for nothing.
func (s *Server) goalsShownNow() *goalsShown {
	s.goalsK.Lock()
	defer s.goalsK.Unlock()
	return s.goalsK.shown
}

// goalsBriefNow is what this replica's status says of the goals it worked
// out, nil for nothing: as Gen, the sum of what the ledger holds of them,
// as last written, or read as its term began; not of what it worked out
// since and failed to write, which a follower would read the ledger's
// older lines under.
func (s *Server) goalsBriefNow() *goalsBrief {
	s.goalsK.Lock()
	defer s.goalsK.Unlock()
	g := s.goalsK.shown
	if g == nil {
		return nil
	}
	return &goalsBrief{Gen: s.goalsK.kept, Week: g.Week, Lines: len(g.Lines), Waiting: g.Waiting}
}

// goalsTerm readies what is kept of the goals as a term of leading
// begins, or ends (lost): whatever this replica worked out in an earlier
// term is let go, as another may have led since. Beginning, what the
// ledger holds is served, and its sum said, until its first round works
// them out again: /metrics keeps its series through a failover, and a
// round that works out the same writes nothing.
func (s *Server) goalsTerm(ctx context.Context, lost bool) {
	s.goalsK.Lock()
	s.goalsK.key, s.goalsK.sum, s.goalsK.kept, s.goalsK.shown = "", "", "", nil
	s.goalsK.Unlock()
	if lost {
		return
	}
	gs, err := s.goalsNow(ctx)
	if err != nil || !slices.ContainsFunc(gs, func(g ledger.Goal) bool { return g.Each == "week" }) {
		return
	}
	raw, err := s.l.GoalsShown(ctx)
	var g goalsShown
	if err != nil || raw == nil || json.Unmarshal(raw, &g) != nil {
		return
	}
	sum := goalsSum(&g)
	s.goalsK.Lock()
	if s.goalsK.shown == nil {
		s.goalsK.sum, s.goalsK.kept, s.goalsK.shown = sum, sum, &g
	}
	s.goalsK.Unlock()
}

// goalsFollowed is what a replica that follows read of the leader's goals
// last, and of which Gen.
type goalsFollowed struct {
	sync.Mutex
	gen   string
	shown *goalsShown
}

// goalsRead is what the leader worked out of the goals after its last
// round: this replica's own, leading; else read from the ledger, once for
// each Gen the leader's status says, and kept.
func (s *Server) goalsRead(ctx context.Context) *goalsShown {
	if !s.follows() {
		return s.goalsShownNow()
	}
	return s.goalsFollow(ctx, s.shownStatus().Goals)
}

// goalsFollow is what a replica that follows shows of the goals, by what
// the leader's status says of them (b): read from the ledger, and kept
// under b's Gen only when what it read is what Gen is the sum of. The
// ledger holding other than the leader said (a write since, or a status
// not yet said again), it is shown as read, and read again next time.
func (s *Server) goalsFollow(ctx context.Context, b *goalsBrief) *goalsShown {
	if b == nil {
		return nil
	}
	s.goalsF.Lock()
	defer s.goalsF.Unlock()
	if s.goalsF.shown != nil && s.goalsF.gen == b.Gen {
		return s.goalsF.shown
	}
	raw, err := s.l.GoalsShown(ctx)
	if err != nil {
		return s.goalsF.shown // as last read, until the ledger answers
	}
	var g goalsShown
	if raw == nil || json.Unmarshal(raw, &g) != nil {
		return nil
	}
	if goalsSum(&g) == b.Gen {
		s.goalsF.gen, s.goalsF.shown = b.Gen, &g
	}
	return &g
}

// goalsSum is a sum of what was worked out of the goals, but when.
func goalsSum(g *goalsShown) string {
	x := *g
	x.At = time.Time{}
	b, _ := json.Marshal(x)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

// goalsAfter works the goals out after a round, the leader's: the last
// whole week and the eleven before it, and the week under way, for
// everyone and each team a goal is of, from one read of their tallies and
// incidents (summaryOf), 13 weeks at most. Then it tells goal.missed once
// for each goal, team and week found whole and missed, makes tasks for
// those missed (--goal-tasks), 20 a run at most, and says on each goal's
// open task what became of the weeks since it was made.
func (s *Server) goalsAfter(ctx context.Context) error {
	gs, err := s.goalsNow(ctx)
	if err != nil {
		return err
	}
	var weekGoals []ledger.Goal
	for _, g := range gs {
		if g.Each == "week" {
			weekGoals = append(weekGoals, g)
		}
	}
	if len(weekGoals) == 0 {
		s.goalsK.Lock()
		s.goalsK.key, s.goalsK.shown = "", nil
		s.goalsK.Unlock()
		return nil
	}
	this := ledger.Week(s.l.Day(s.now()))
	last := this - 7
	k := fmt.Sprint(s.l.Gen(), "/", this)
	s.goalsK.Lock()
	same := s.goalsK.key == k
	s.goalsK.Unlock()
	if same {
		return nil
	}
	own, err := s.owners(ctx)
	if err != nil {
		return err
	}
	who, _ := goalsWho(weekGoals, own, nil)
	sum, err := s.summaryOf(ctx, this-12*7, this, who, []string{"critical"})
	if err != nil {
		return err
	}
	lines := goalLines(sum, weekGoals, who)
	lastFrom, thisFrom := s.l.Midnight(last), s.l.Midnight(this)
	shown := &goalsShown{At: s.now(), Week: lastFrom}
	for _, gl := range lines {
		x := goalShown{Goal: gl.Goal.Name, Team: gl.TeamName(), Result: goalOpen}
		for _, w := range gl.Weeks {
			if w.From.Before(thisFrom) && w.Result == goalMet {
				x.Met++
			}
		}
		if w, ok := gl.week(lastFrom); ok {
			x.Result, x.Value = w.Result, w.Value
		}
		shown.Lines = append(shown.Lines, x)
	}
	order := map[string]int{goalMissed: 0, goalUnknown: 1, goalOpen: 2, goalMet: 3}
	slices.SortStableFunc(shown.Lines, func(a, b goalShown) int {
		return cmp.Or(cmp.Compare(order[a.Result], order[b.Result]), strings.Compare(a.Goal, b.Goal), strings.Compare(a.Team, b.Team))
	})
	if n := len(shown.Lines); n > maxGoalSeries {
		shown.More, shown.Lines = n-maxGoalSeries, shown.Lines[:maxGoalSeries]
	}
	errs := []error{s.tellGoals(ctx, lines, last)}
	waiting, err := s.goalTasks(ctx, lines, last)
	shown.Waiting = waiting
	errs = append(errs, err, s.sayOnGoalTasks(ctx, lines, this))
	// What was told and made for of weeks past any worked out, once a
	// week: of goals no longer declared among them.
	s.goalsK.Lock()
	prune := s.goalsK.pruned != this
	s.goalsK.Unlock()
	if prune {
		if _, err := s.l.PruneGoalMarks(ctx, this-13*7); err != nil {
			errs = append(errs, err)
		} else {
			s.goalsK.Lock()
			s.goalsK.pruned = this
			s.goalsK.Unlock()
		}
	}
	// Kept in the ledger, for the replicas that follow, only when it
	// changed.
	gen := goalsSum(shown)
	s.goalsK.Lock()
	kept := s.goalsK.kept
	s.goalsK.Unlock()
	if gen != kept {
		b, _ := json.Marshal(shown)
		if err := s.l.KeepGoalsShown(ctx, b); err != nil {
			errs = append(errs, err)
		} else {
			kept = gen
		}
	}
	s.goalsK.Lock()
	// Kept as of the ledger before what this wrote: a task made, or a
	// comment, is a change, and the next round looks again, finding none
	// to make. One that failed is tried again then too.
	s.goalsK.key, s.goalsK.shown, s.goalsK.sum, s.goalsK.kept = k, shown, gen, kept
	if err := errors.Join(errs...); err != nil {
		s.goalsK.key = ""
	}
	s.goalsK.Unlock()
	return errors.Join(errs...)
}

// teamIDOf is a line's team's id, 0 for everyone's.
func teamIDOf(t *ledger.Team) int64 {
	if t == nil {
		return 0
	}
	return t.ID
}

// goalNotice is what goal.missed tells of a goal missed: of everyone's or a
// team's (Team), its value; of each team's (Team "*"), the teams that
// missed it, goalTeamsNamed of them with their values, and how many more.
type goalNotice struct {
	Name     string           `json:"name"`
	Team     string           `json:"team,omitempty"`
	Measure  string           `json:"measure"`
	Critical []string         `json:"critical,omitempty"`
	AtLeast  *float64         `json:"at_least,omitempty"`
	AtMost   *float64         `json:"at_most,omitempty"`
	Value    *float64         `json:"value"`
	Teams    []goalNoticeTeam `json:"teams,omitempty"`
	More     int              `json:"more,omitempty"`
	From     time.Time        `json:"from"`
	To       time.Time        `json:"to"`
}

// goalNoticeTeam is a team that missed a goal of each team's, and its
// value.
type goalNoticeTeam struct {
	Team  string   `json:"team"`
	Value *float64 `json:"value"`
}

// tellGoals tells goal.missed for each goal whose last whole week was
// missed, once for each team: to where what happens is told, the webhook
// and Slack. A goal of each team's is one notice a week, of the teams that
// missed it and weren't told of, however many: a team marked told for the
// week, or a later one, as by a leader whose clock was ahead, is passed
// over.
func (s *Server) tellGoals(ctx context.Context, lines []goalLine, last int64) error {
	dests := s.dests()
	if len(dests) == 0 {
		return nil
	}
	told0, _, err := s.l.GoalMarks(ctx)
	if err != nil {
		return err
	}
	type notice struct {
		about goalNotice
		teams []int64
	}
	from := s.l.Midnight(last)
	var order []*notice
	byGoal := map[string]*notice{}
	for _, gl := range lines {
		w, ok := gl.week(from)
		if !ok || w.Result != goalMissed {
			continue
		}
		team := teamIDOf(gl.Team)
		if told0[ledger.GoalKey(gl.Goal.Name, team)] >= last {
			continue
		}
		n := byGoal[gl.Goal.Name]
		if n == nil {
			n = &notice{about: goalNotice{Name: gl.Goal.Name, Team: gl.TeamName(), Measure: gl.Goal.Measure, Critical: gl.Goal.Critical,
				AtLeast: w.AtLeast, AtMost: w.AtMost, Value: w.Value, From: w.From.UTC(), To: w.To.UTC()}}
			if gl.Goal.EveryTeam {
				n.about.Team, n.about.Value = "*", nil
			}
			byGoal[gl.Goal.Name] = n
			order = append(order, n)
		}
		n.teams = append(n.teams, team)
		if gl.Goal.EveryTeam {
			if len(n.about.Teams) < goalTeamsNamed {
				n.about.Teams = append(n.about.Teams, goalNoticeTeam{gl.TeamName(), w.Value})
			} else {
				n.about.More++
			}
		}
	}
	var errs []error
	for _, n := range order {
		told := make([]ledger.Told, 0, len(dests))
		for _, d := range dests {
			told = append(told, ledger.Told{Dest: d, About: map[string]any{"goal": n.about}})
		}
		errs = append(errs, s.l.TellGoalMissed(ctx, n.about.Name, n.teams, last, told))
	}
	return errors.Join(errs...)
}

// goalTaskIn is the task a goal missed in a week becomes, in a language:
// of kind decide, of the goal's team, titled with the goal and the week,
// its text the value, the target and where the goal is.
func (s *Server) goalTaskIn(l string, g ledger.Goal, team *ledger.Team, w goalWeek) ledger.Task {
	day := w.From.In(s.cfg.Loc).Format("2006-01-02")
	title := tr(l, "Goal %s missed, week of %s", g.Name, day)
	link := "/goals"
	if team != nil {
		title = tr(l, "Goal %s of %s missed, week of %s", g.Name, team.Name, day)
		link += "?team=" + url.QueryEscape(team.Name)
	}
	link += "#goal-" + g.Name
	if s.cfg.ExternalURL != "" {
		link = strings.TrimRight(s.cfg.ExternalURL, "/") + link
	}
	body := goalMissedText(l, g, "", w.From, w.Value, s.cfg.Loc) + "\n\n" +
		tr(l, "Tune the rules, write the incidents down, or change the goal in values. The goal: %s", link)
	return ledger.Task{Title: title, Kind: "decide", TeamID: teamIDOf(team), Goal: g.Name, Body: body}
}

// goalTasks makes, with --goal-tasks, a task for each goal and team whose
// last whole week was missed and has none open: one it made a task for
// that week before, since done or dropped, isn't made again; 20 a run at
// most, those over waiting for the runs after. It returns how many wait.
func (s *Server) goalTasks(ctx context.Context, lines []goalLine, last int64) (int, error) {
	if !s.cfg.GoalTasks {
		return 0, nil
	}
	_, was, err := s.l.GoalMarks(ctx)
	if err != nil {
		return 0, err
	}
	open, err := s.l.OpenGoalTasks(ctx)
	if err != nil {
		return 0, err
	}
	from := s.l.Midnight(last)
	made, waiting := 0, 0
	var errs []error
	for _, gl := range lines {
		w, ok := gl.week(from)
		if !ok || w.Result != goalMissed {
			continue
		}
		team := teamIDOf(gl.Team)
		if was[ledger.GoalKey(gl.Goal.Name, team)] >= last || open[ledger.GoalKey(gl.Goal.Name, team)] != 0 {
			continue
		}
		if made == goalTasksARun {
			waiting++
			continue
		}
		// One opened since the tasks were read is found by the write, which
		// makes none: counted all the same, as a write the turn was taken for.
		made++
		if _, err := s.l.MakeGoalTask(ctx, s.goalTaskIn(s.cfg.Lang, gl.Goal, gl.Team, w), last, ledger.GoalTasksBy); err != nil && !errors.Is(err, ledger.ErrGoalTaskOpen) {
			errs = append(errs, err)
		}
	}
	return waiting, errors.Join(errs...)
}

// sayOnGoalTasks says, with --goal-tasks, on each goal's task still open,
// whoever made it, what became of the weeks since the one it was made for,
// once each: another missed ("missed again"), one met, and one missed that
// no longer is (an incident written down later), never closing it;
// goalSaysARun a run at most, the rest said by the runs after. What is kept
// of one done or dropped is let go. Without --goal-tasks, Lapilli writes
// on no task, as it makes none.
func (s *Server) sayOnGoalTasks(ctx context.Context, lines []goalLine, this int64) error {
	if !s.cfg.GoalTasks {
		return nil
	}
	ids, err := s.l.GoalTasksKept(ctx)
	if err != nil || len(ids) == 0 {
		return err
	}
	type key struct {
		goal string
		team int64
	}
	byKey := map[key]goalLine{}
	for _, gl := range lines {
		byKey[key{gl.Goal.Name, teamIDOf(gl.Team)}] = gl
	}
	l := s.cfg.Lang
	thisFrom := s.l.Midnight(this)
	said := 0
	var errs []error
	for _, id := range ids {
		t, err := s.l.TaskByID(ctx, id)
		if errors.Is(err, ledger.ErrNotFound) || err == nil && t.Status != "open" && t.Status != "doing" {
			errs = append(errs, s.l.ForgetGoalTask(ctx, id))
			continue
		} else if err != nil {
			errs = append(errs, err)
			continue
		}
		st, ok, err := s.l.GoalTask(ctx, id)
		if err != nil || !ok {
			errs = append(errs, err)
			continue
		}
		gl, ok := byKey[key{t.Goal, t.TeamID}]
		if !ok {
			continue // the goal is no longer declared, or no longer the team's
		}
		if st.Said == nil {
			st.Said = map[int64]string{}
		}
		for w := range st.Said {
			if w < this-12*7 {
				delete(st.Said, w) // long past every week worked out
			}
		}
		for _, w := range gl.Weeks {
			if !w.From.Before(thisFrom) || w.Result == goalOpen || w.Why == whyNotWhole {
				continue
			}
			week := ledger.Week(s.l.Day(w.From))
			if week < st.Week || said == goalSaysARun {
				continue
			}
			day := w.From.In(s.cfg.Loc).Format("2006-01-02")
			var body, now string
			switch was := st.Said[week]; {
			case w.Result == goalMissed:
				// Once a week: the one it was made for is said already.
				if week == st.Week || was == goalMissed || was == goalNoLonger {
					continue
				}
				body, now = tr(l, "Missed again, week of %s: %s.", day, goalMissedValue(gl.Goal, w.Value)), goalMissed
			case was == goalMissed && w.Result == goalMet:
				body, now = tr(l, "No longer missed, week of %s: %s.", day, goalValueText(gl.Goal.Measure, w.Value)), goalNoLonger
			case was == goalMissed:
				body, now = tr(l, "No longer missed, week of %s: %s.", day, goalResultText(l, w.summaryGoal)), goalNoLonger
			case w.Result == goalMet && was == "":
				body, now = tr(l, "Met, week of %s: %s.", day, goalValueText(gl.Goal.Measure, w.Value)), goalMet
			default:
				continue
			}
			st.Said[week] = now
			if err := s.l.SayOnGoalTask(ctx, id, body, st, ledger.GoalTasksBy); err != nil {
				errs = append(errs, err)
				break
			}
			said++
		}
	}
	return errors.Join(errs...)
}

// goalMetrics writes the goals' series, as the leader worked them out after
// its last round: a scrape works nothing out, and never waits for the
// heavy turn. Everyone's goals have no team label, a team's have it.
func (s *Server) goalMetrics(ctx context.Context, w io.Writer) {
	g := s.goalsRead(ctx)
	if g == nil || len(g.Lines) == 0 {
		return
	}
	labels := func(x goalShown) string {
		l := fmt.Sprintf(`goal="%s"`, labelValue(x.Goal))
		if x.Team != "" {
			l += fmt.Sprintf(`,team="%s"`, labelValue(x.Team))
		}
		return l
	}
	var b strings.Builder
	b.WriteString("# HELP lapilli_goal_known Whether each goal's last whole week is known, met or missed, for everyone (no team label) and each team it is of: 1 or 0. Not known is a week with no record for a goal that needs one, a share with nothing to divide, or a week the ledger doesn't hold whole (docs/design/goals.md).\n# TYPE lapilli_goal_known gauge\n")
	for _, x := range g.Lines {
		known := 0
		if x.Result == goalMet || x.Result == goalMissed {
			known = 1
		}
		fmt.Fprintf(&b, "lapilli_goal_known{%s} %d\n", labels(x), known)
	}
	b.WriteString("# HELP lapilli_goal_met Whether each goal was met in the last whole week: 1, or 0 when missed; no series when not known (lapilli_goal_known).\n# TYPE lapilli_goal_met gauge\n")
	for _, x := range g.Lines {
		switch x.Result {
		case goalMet:
			fmt.Fprintf(&b, "lapilli_goal_met{%s} 1\n", labels(x))
		case goalMissed:
			fmt.Fprintf(&b, "lapilli_goal_met{%s} 0\n", labels(x))
		}
	}
	b.WriteString("# HELP lapilli_goal_value Each goal's measure over the last whole week, when it is known: a share from 0 to 1, a count, or seconds.\n# TYPE lapilli_goal_value gauge\n")
	for _, x := range g.Lines {
		if x.Value != nil && (x.Result == goalMet || x.Result == goalMissed) {
			fmt.Fprintf(&b, "lapilli_goal_value{%s} %g\n", labels(x), *x.Value)
		}
	}
	b.WriteString("# HELP lapilli_goal_weeks_met Whole weeks, of the twelve before this one, in which each goal was met: beside lapilli_weeks_counted.\n# TYPE lapilli_goal_weeks_met gauge\n")
	for _, x := range g.Lines {
		fmt.Fprintf(&b, "lapilli_goal_weeks_met{%s} %d\n", labels(x), x.Met)
	}
	io.WriteString(w, b.String())
}
