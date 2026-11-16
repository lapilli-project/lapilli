package serve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// The Goals page, the overview's Goals card and its lines in What to look
// at, the task a goal missed becomes with one press, and the goals' line in
// the digest and section in reports (docs/design/goals.md).

// goalCell is a week of a goal on a page: its status class ("" for the
// week under way, which is no result yet), its sign and value, and what it
// says in full, for those who can't see the colour.
type goalCell struct {
	Class, Sign, Value, Text string
}

// goalCellOf is a goal's week as a cell.
func goalCellOf(l string, w goalWeek, loc *time.Location) goalCell {
	c := goalCell{Value: goalWeekValue(w.summaryGoal)}
	day := w.From.In(loc).Format("2006-01-02")
	switch w.Result {
	case goalMet:
		c.Class, c.Sign = statusClass("goal", "met"), "✓"
	case goalMissed:
		c.Class, c.Sign = statusClass("goal", "missed"), "✕"
	case goalOpen:
		c.Sign = "…"
		if w.Over {
			c.Class, c.Sign = statusClass("goal", "over"), "⚠︎"
		}
	default:
		c.Class, c.Sign, c.Value = statusClass("goal", "unknown"), "?", "–"
	}
	c.Text = tr(l, "week of %s: %s, %s", day, goalResultText(l, w.summaryGoal), c.Value)
	return c
}

// goalRowView is a goal for a team on the Goals page: its last whole week
// and its twelve whole weeks and the week under way, the earliest first.
type goalRowView struct {
	ID, Name, Team, Measure, Target, Critical string
	EveryTeam                                 bool
	Last                                      goalCell
	LastWhy                                   string
	Cells                                     []goalCell
	// Task is its open task, 0 for none.
	Task int64
}

// goalHead is a week's column: its Monday, or the week under way.
type goalHead struct {
	Label   string
	Current bool
}

type goalsView struct {
	// Empty says no goal is declared; NoneFor, none of the team picked or
	// of everyone.
	Empty, NoneFor bool
	Team           teamFilter
	Weeks          []goalHead // the earliest first
	Last           time.Time
	Rows           []goalRowView
	// More are the teams left out of each team's goals, with no team
	// picked: pick one to see its own.
	More int
	// ByCatching says some team's goal on incidents isn't known for its
	// team having no labels.
	ByCatching bool
}

// goalTeamsShown is how many teams a goal of each team's (team: "*") is
// shown for on the Goals page with no team picked, those missed last week
// first.
const goalTeamsShown = 50

func (s *Server) goalsPage(r *http.Request) (any, error) {
	ctx := r.Context()
	l := lang(r)
	gs, err := s.goalsNow(ctx)
	if err != nil {
		return nil, err
	}
	o, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	var v goalsView
	var team *ledger.Team
	v.Team, team = s.teamFilter(o, r.URL.Query().Get("team"))
	if len(gs) == 0 {
		v.Empty = true
		return v, nil
	}
	this := ledger.Week(s.l.Day(s.now()))
	w1 := this - 12*7
	v.Last = s.l.Midnight(this - 7)
	for w := w1; w <= this; w += 7 {
		v.Weeks = append(v.Weeks, goalHead{s.l.Midnight(w).In(s.cfg.Loc).Format("01-02"), w == this})
	}
	// With a team picked, its goals and everyone's.
	who, every := goalsWho(gs, o, team)
	if len(who) == 0 {
		v.NoneFor = true
		return v, nil
	}
	if every || ((this-w1)/7+1)*int64(len(who)) > summaryMaxWeeks {
		// Every team's rows, or more than a year's of one, as a summary
		// of as many takes them: in the heavy turn.
		done, err := s.busy(ctx)
		if err != nil {
			return nil, err
		}
		defer done()
	}
	lines, err := s.goalTable(ctx, w1, this, who)
	if err != nil {
		return nil, err
	}
	open, err := s.l.OpenGoalTasks(ctx)
	if err != nil {
		return nil, err
	}
	// With no team picked, a goal of each team's shows its teams missed
	// last week first, goalTeamsShown of them.
	shown := map[string]int{}
	order := map[string]int{goalMissed: 0, goalUnknown: 1, goalOpen: 2, goalMet: 3}
	at := map[string]int{}
	for i, g := range gs {
		at[g.Name] = i
	}
	slices.SortStableFunc(lines, func(a, b goalLine) int {
		if d := at[a.Goal.Name] - at[b.Goal.Name]; d != 0 || !a.Goal.EveryTeam || team != nil {
			return d
		}
		wa, _ := a.week(v.Last)
		wb, _ := b.week(v.Last)
		return order[wa.Result] - order[wb.Result]
	})
	seen := map[string]bool{}
	for _, gl := range lines {
		g := gl.Goal
		if g.EveryTeam && team == nil {
			if shown[g.Name] == goalTeamsShown {
				v.More++
				continue
			}
			shown[g.Name]++
		}
		row := goalRowView{Name: g.Name, Team: gl.TeamName(), Measure: goalMeasureText(l, g.Measure), Target: goalTarget(l, g), EveryTeam: g.EveryTeam,
			Task: open[ledger.GoalKey(g.Name, teamIDOf(gl.Team))]}
		if !seen[g.Name] {
			row.ID, seen[g.Name] = "goal-"+g.Name, true
		}
		if ledger.GoalMeasures[g.Measure].Critical {
			row.Critical = strings.Join(g.CriticalOf(), ", ")
		}
		for _, w := range gl.Weeks {
			row.Cells = append(row.Cells, goalCellOf(l, w, s.cfg.Loc))
		}
		if w, ok := gl.week(v.Last); ok {
			row.Last = goalCellOf(l, w, s.cfg.Loc)
			if w.Result == goalUnknown {
				row.LastWhy = goalResultText(l, w.summaryGoal)
				v.ByCatching = v.ByCatching || w.Why == whyByCatching
			}
		}
		v.Rows = append(v.Rows, row)
	}
	return v, nil
}

// goalsCard is the overview's Goals card: the last whole week's results
// for the team in effect, its own goals and each team's, or everyone's
// with none picked; how many met of how many, and each missed.
type goalsCard struct {
	Week   time.Time
	Met    int
	Of     int
	Missed []string
	// Unknown are those not known that week.
	Unknown int
}

// goalsOverview is the overview's Goals card, nil with no goals for whom
// it is shown, and the lines it adds to What to look at: each goal missed
// last week, with the task it would make, or the one open about it.
func (s *Server) goalsOverview(ctx context.Context, l string, team *ledger.Team) (*goalsCard, []action, error) {
	gs, err := s.goalsNow(ctx)
	if err != nil || len(rowGoals(gs, team)) == 0 {
		return nil, nil, err
	}
	last := ledger.Week(s.l.Day(s.now())) - 7
	lines, err := s.goalTable(ctx, last, last, []*ledger.Team{team})
	if err != nil {
		return nil, nil, err
	}
	open, err := s.l.OpenGoalTasks(ctx)
	if err != nil {
		return nil, nil, err
	}
	c := &goalsCard{Week: s.l.Midnight(last)}
	var as []action
	for _, gl := range lines {
		w, ok := gl.week(c.Week)
		if !ok {
			continue
		}
		c.Of++
		switch w.Result {
		case goalMet:
			c.Met++
		case goalMissed:
			text := goalMissedText(l, gl.Goal, gl.TeamName(), w.From, w.Value, s.cfg.Loc)
			c.Missed = append(c.Missed, text)
			href := "/goals"
			if gl.Team != nil {
				href += "?team=" + url.QueryEscape(gl.Team.Name)
			}
			a := action{Text: tr(l, "Goal %s missed last week", gl.Goal.Name), Detail: text, Href: href + "#goal-" + gl.Goal.Name,
				Kind: "decide", Goal: gl.Goal.Name, GoalTeam: gl.TeamName(), GoalTeamID: teamIDOf(gl.Team), Week: w.From.In(s.cfg.Loc).Format("2006-01-02"),
				Task: open[ledger.GoalKey(gl.Goal.Name, teamIDOf(gl.Team))]}
			as = append(as, a)
		default:
			c.Unknown++
		}
	}
	return c, as, nil
}

// postGoalTask makes the task a goal missed in a week becomes, with one
// press: of kind decide, of the goal's team, titled with the goal and the
// week, its text the value, the target and where the goal is, linked to
// the goal. A goal whose task is open already goes to that task; a week
// the goal wasn't missed in, as an incident written down since turned it,
// is refused.
func (s *Server) postGoalTask(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	ctx := r.Context()
	name, teamName, day := r.PostFormValue("goal"), r.PostFormValue("team"), r.PostFormValue("week")
	gs, err := s.goalsNow(ctx)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	i := slices.IndexFunc(gs, func(g ledger.Goal) bool { return g.Name == name })
	if i < 0 {
		s.oops(w, r, bad("there is no goal called %q", name))
		return
	}
	g := gs[i]
	o, err := s.owners(ctx)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	var team *ledger.Team
	switch {
	case g.EveryTeam:
		t, ok := o.team(teamName)
		if !ok {
			s.oops(w, r, bad("goal %s is each team's: say which team's, of those there are", g.Name))
			return
		}
		team = &t
	case g.TeamID != 0:
		t, ok := o.byID[g.TeamID]
		if !ok {
			s.oops(w, r, ledger.ErrNotFound)
			return
		}
		team = &t
	}
	// A team's editor makes their team's goal's task alone: said before
	// anything else, as the ledger would refuse it in the write.
	if teams, limited := scoped(r); limited && (team == nil || !slices.Contains(teams, team.ID)) {
		s.oops(w, r, ledger.ErrNotYourTeam)
		return
	}
	d, err := dateDay(day)
	if err != nil || ledger.Week(d) != d {
		s.oops(w, r, bad("week %q: want its Monday, like 2026-11-09", day))
		return
	}
	lines, err := s.goalTable(ctx, d, d, []*ledger.Team{team})
	if err != nil {
		s.oops(w, r, err)
		return
	}
	var gw goalWeek
	found := false
	for _, gl := range lines {
		if gl.Goal.Name == g.Name {
			gw, found = gl.week(s.l.Midnight(d))
		}
	}
	if !found || gw.Result != goalMissed {
		s.oops(w, r, bad("goal %s wasn't missed in the week of %s, as it reads now", g.Name, day))
		return
	}
	id, err := s.l.MakeGoalTask(ctx, s.goalTaskIn(lang(r), g, team, gw), d, s.user(r))
	if errors.Is(err, ledger.ErrGoalTaskOpen) {
		back(w, r, urlContext(r.URL.Query()).Link(fmt.Sprintf("/tasks/%d", id)))
		return
	}
	to := r.PostFormValue("back")
	if to == "" {
		to = urlContext(r.URL.Query()).Link(fmt.Sprintf("/tasks/%d", id))
	}
	s.written(w, r, err, to)
}

// weekGoals are the goals of a week, from Monday week, for everyone's (team
// nil) or a team's, as the digest and what it tells say them.
func (s *Server) weekGoals(ctx context.Context, week int64, team *ledger.Team) ([]goalLine, error) {
	gs, err := s.goalsNow(ctx)
	if err != nil || len(rowGoals(gs, team)) == 0 {
		return nil, err
	}
	return s.goalTable(ctx, week, week, []*ledger.Team{team})
}

// goalsLine says, for the digest, how the week's goals came out: how many
// met, missed and not known, and each missed, with its value and target;
// "" with no goals of whom it is for.
func (s *Server) goalsLine(ctx context.Context, from time.Time, team *ledger.Team) (string, []summaryGoal, error) {
	lines, err := s.weekGoals(ctx, ledger.Week(s.l.Day(from)), team)
	if err != nil || len(lines) == 0 {
		return "", nil, err
	}
	l := s.cfg.Lang
	met, missed, unknown := 0, 0, 0
	var said []string
	var out []summaryGoal
	for _, gl := range lines {
		w, ok := gl.week(from)
		if !ok {
			continue
		}
		out = append(out, w.summaryGoal)
		switch w.Result {
		case goalMet:
			met++
		case goalMissed:
			missed++
			said = append(said, goalMissedText(l, gl.Goal, "", w.From, w.Value, s.cfg.Loc))
		default:
			unknown++
		}
	}
	line := tr(l, "Goals: %d of %d met, %d missed, %d not known.", met, len(out), missed, unknown)
	if len(said) > 0 {
		line += " " + strings.Join(said, " ")
	}
	return line, out, nil
}

// aboutGoals adds to what digest.ready tells the week's goals, of whom it
// is for (goals); with none declared of them, nothing.
func (s *Server) aboutGoals(ctx context.Context, about map[string]any, from time.Time, team *ledger.Team) error {
	_, goals, err := s.goalsLine(ctx, from, team)
	if err == nil && len(goals) > 0 {
		about["goals"] = goals
	}
	return err
}

// goalsSection: each goal over the span's whole weeks, for whom the report
// is for, and how many it met; its numbers, each week's result.
func (s *Server) goalsSection(ctx context.Context, from, to time.Time, team *ledger.Team, loc *time.Location) (string, any, error) {
	l := s.cfg.Lang
	gs, err := s.goalsNow(ctx)
	if err != nil {
		return "", nil, err
	}
	type weekNums struct {
		From time.Time `json:"from"`
		To   time.Time `json:"to"`
		summaryGoal
	}
	type goalNums struct {
		Name  string     `json:"name"`
		Team  string     `json:"team,omitempty"`
		Met   int        `json:"met"`
		Of    int        `json:"of"`
		Weeks []weekNums `json:"weeks"`
	}
	nums := []goalNums{}
	if len(rowGoals(gs, team)) == 0 {
		return tr(l, "No goals are declared for whom this report is for."), nums, nil
	}
	// The whole weeks the span holds, 53 at most, the latest.
	w1 := ledger.Week(s.l.Day(from))
	if s.l.Midnight(w1).Before(from) {
		w1 += 7
	}
	w2 := ledger.Week(s.l.Day(to)) - 7
	if s.l.Midnight(w2 + 7).After(to) {
		w2 -= 7
	}
	if w2 < w1 {
		return tr(l, "Goals: the span holds no whole week to hold them to."), nums, nil
	}
	w1 = max(w1, w2-(summaryMaxWeeks-1)*7)
	lines, err := s.goalTable(ctx, w1, w2, []*ledger.Team{team})
	if err != nil {
		return "", nil, err
	}
	var said []string
	for _, gl := range lines {
		n := goalNums{Name: gl.Goal.Name, Team: gl.TeamName(), Weeks: []weekNums{}}
		var missed []string
		for _, w := range gl.Weeks {
			n.Weeks = append(n.Weeks, weekNums{w.From.UTC(), w.To.UTC(), w.summaryGoal})
			if w.Result == goalMet || w.Result == goalMissed {
				n.Of++
			}
			switch w.Result {
			case goalMet:
				n.Met++
			case goalMissed:
				missed = append(missed, w.From.In(loc).Format("2006-01-02"))
			}
		}
		line := tr(l, "Goal %s (%s, %s): met in %d of %d whole weeks known, of %d.", gl.Goal.Name, goalMeasureText(l, gl.Goal.Measure),
			goalTarget(l, gl.Goal), n.Met, n.Of, len(gl.Weeks))
		if len(missed) > 0 {
			line += " " + tr(l, "Missed in the weeks of %s.", strings.Join(missed, ", "))
		}
		said = append(said, verdict.Clean(line))
		nums = append(nums, n)
	}
	return strings.Join(said, "\n"), nums, nil
}

// goalsCardOut is the overview's Goals card, as MCP's overview gives it.
type goalsCardOut struct {
	Week    time.Time `json:"week" jsonschema:"the Monday of the last whole week"`
	Met     int       `json:"met"`
	Of      int       `json:"of"`
	Unknown int       `json:"not_known"`
	Missed  []string  `json:"missed" jsonschema:"each goal missed, with its value and target"`
}
