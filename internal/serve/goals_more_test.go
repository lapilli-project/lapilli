package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// hooked is a goalServer telling what happens to a webhook, with
// --goal-tasks as given.
func hooked(t *testing.T, tasks bool) *Server {
	t.Helper()
	hook := filepath.Join(t.TempDir(), "hook")
	os.WriteFile(hook, []byte("https://hooks.example/x\n"), 0o600)
	return goalServer(t, func(c *Config) { c.NotifyWebhook, c.GoalTasks = hook, tasks })
}

// goalNotices are the goal.missed told to the webhook, as each says them.
func goalNotices(t *testing.T, s *Server) []goalNotice {
	t.Helper()
	due, err := s.l.Due(context.Background(), "webhook", now.Add(60*24*time.Hour), 500)
	if err != nil {
		t.Fatal(err)
	}
	var out []goalNotice
	for _, n := range due {
		if n.Event == "goal.missed" {
			var x struct {
				Goal goalNotice `json:"goal"`
			}
			if err := json.Unmarshal(n.Body, &x); err != nil {
				t.Fatal(err)
			}
			out = append(out, x.Goal)
		}
	}
	return out
}

// again works the goals out again, as a round after the ledger changed.
func again(t *testing.T, s *Server) {
	t.Helper()
	s.goalsK.Lock()
	s.goalsK.key = ""
	s.goalsK.Unlock()
	if err := s.goalsAfter(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// commentsOn are what was said on a task.
func commentsOn(t *testing.T, s *Server, id int64) []string {
	t.Helper()
	cs, err := s.l.Comments(context.Background(), "task", fmt.Sprint(id))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range cs {
		out = append(out, c.Body)
	}
	return out
}

// Without --goal-tasks, Lapilli makes no task and writes on none, not on
// one a person made with the button either; goal.missed is told all the
// same.
func TestGoalsNoTasksWithoutTheFlag(t *testing.T) {
	parallel(t)
	s := hooked(t, false)
	declare(t, s, theGoals)
	ctx := context.Background()
	id, err := s.l.MakeGoalTask(ctx, ledger.Task{Title: "Goal none-missed missed, week of 2026-10-12", Kind: "decide", Goal: "none-missed"},
		ledger.Week(s.l.Day(dayAt("2026-10-12", 0))), "ana")
	if err != nil {
		t.Fatal(err)
	}
	again(t, s)
	if ts, _ := s.l.Tasks(ctx, ledger.TaskQuery{AnyGoal: true}); len(ts) != 1 {
		t.Errorf("tasks made without --goal-tasks: %+v", ts)
	}
	if got := commentsOn(t, s, id); len(got) != 0 {
		t.Errorf("said without --goal-tasks: %q", got)
	}
	if len(goalNotices(t, s)) == 0 {
		t.Error("nothing told")
	}
}

// A run writes goalSaysARun comments at most (here 1); the runs after say
// the rest, once each.
func TestGoalCommentsBounded(t *testing.T) {
	s := hooked(t, true)
	declare(t, s, "goals:\n  - {name: none-missed, measure: incidents.missed, at_most: 0}\n")
	was := goalSaysARun
	goalSaysARun = 1
	defer func() { goalSaysARun = was }()
	// Made for the week of 10-12, since not known; then 10-19 met and
	// 10-26 missed: three to say.
	id, err := s.l.MakeGoalTask(context.Background(), ledger.Task{Title: "none-missed", Kind: "decide", Goal: "none-missed"},
		ledger.Week(s.l.Day(dayAt("2026-10-12", 0))), "ana")
	if err != nil {
		t.Fatal(err)
	}
	for round := 1; round <= 4; round++ {
		again(t, s)
		if n := len(commentsOn(t, s, id)); n != min(round, 3) {
			t.Fatalf("round %d: %d said, want %d: %q", round, n, min(round, 3), commentsOn(t, s, id))
		}
	}
	got := commentsOn(t, s, id)
	slices.Sort(got)
	if want := []string{"Met, week of 2026-10-19: 0.", "Missed again, week of 2026-10-26: 1.",
		"No longer missed, week of 2026-10-12: not known: no incident written down to judge the firings by."}; !slices.Equal(got, want) {
		t.Errorf("said %q, want %q", got, want)
	}
}

// goal.missed is told once for each goal, team and week: a leader whose
// clock is a week behind the last one's tells no earlier week of a team
// told of a later one; a goal given another team, here each team's,
// tells its team again no more, nor makes its task again; a goal of each
// team's tells its teams missed in one notice, and Slack's line names
// them.
func TestGoalsToldOnce(t *testing.T) {
	parallel(t)
	s := hooked(t, true)
	declare(t, s, "goals:\n  - {name: paged-low, team: payments, measure: critical.firings, at_most: 1}\n  - {name: quiet, team: \"*\", measure: firings, at_most: 0}\n")
	again(t, s)
	ns := goalNotices(t, s)
	if len(ns) != 2 {
		t.Fatalf("told: %+v", ns)
	}
	var quiet goalNotice
	for _, n := range ns {
		if n.Name == "quiet" {
			quiet = n
		}
	}
	if quiet.Team != "*" || len(quiet.Teams) != 2 || quiet.Teams[0].Team != "payments" || *quiet.Teams[0].Value != 2 || quiet.Teams[1].Team != "search" || quiet.Value != nil {
		t.Errorf("a goal of each team's: %+v", quiet)
	}
	if line := s.line(notice{Event: "goal.missed", Goal: &quiet}); line != "lapilli: Goal quiet missed in the week of 2026-10-26 by 2 teams: payments (2), search (2); the goal is at most 0." {
		t.Errorf("its line: %q", line)
	}
	tasks, _ := s.l.Tasks(context.Background(), ledger.TaskQuery{AnyGoal: true})
	// A week behind: the week of 10-19, missed by paged-low too.
	s.now = func() time.Time { return now.Add(-7 * 24 * time.Hour) }
	again(t, s)
	if got := goalNotices(t, s); len(got) != 2 {
		t.Errorf("a clock gone back told again: %+v", got)
	}
	s.now = func() time.Time { return now }
	// paged-low each team's now: payments told of the week already.
	declare(t, s, "goals:\n  - {name: paged-low, team: \"*\", measure: critical.firings, at_most: 1}\n  - {name: quiet, team: \"*\", measure: firings, at_most: 0}\n")
	again(t, s)
	if got := goalNotices(t, s); len(got) != 2 {
		t.Errorf("another team told the same week again: %+v", got)
	}
	if ts, _ := s.l.Tasks(context.Background(), ledger.TaskQuery{AnyGoal: true}); len(ts) != len(tasks) {
		t.Errorf("tasks made again: %d, then %d", len(tasks), len(ts))
	}
	// Taken out of the file and put back: neither told nor made again.
	declare(t, s, "teams: []\n")
	declare(t, s, "goals:\n  - {name: paged-low, team: payments, measure: critical.firings, at_most: 1}\n  - {name: quiet, team: \"*\", measure: firings, at_most: 0}\n")
	again(t, s)
	if got := goalNotices(t, s); len(got) != 2 {
		t.Errorf("put back, told again: %+v", got)
	}
	if ts, _ := s.l.Tasks(context.Background(), ledger.TaskQuery{AnyGoal: true}); len(ts) != len(tasks) {
		t.Errorf("put back, tasks made again: %d, then %d", len(tasks), len(ts))
	}
}

// The week the cursor hasn't passed the end of is open: /metrics gives it
// known 0, and neither met nor a value.
func TestGoalMetricsOpenWeek(t *testing.T) {
	parallel(t)
	s := goalServer(t, nil)
	declare(t, s, theGoals)
	// A week on, the ledger's firings still up to the last one.
	s.now = func() time.Time { return now.Add(7 * 24 * time.Hour) }
	again(t, s)
	_, metrics, _ := get(t, s.Handler(), "/metrics")
	if !strings.Contains(metrics, `lapilli_goal_known{goal="none-missed"} 0`+"\n") {
		t.Error("an open week is known")
	}
	for _, x := range []string{"lapilli_goal_met{", "lapilli_goal_value{"} {
		if strings.Contains(metrics, x) {
			t.Errorf("an open week has %s", x)
		}
	}
}

// What the leader worked out is in its status in brief, a few hundred
// bytes whatever the goals; the lines are kept in the ledger, for the
// replicas that follow, when they change, and not again when they don't.
func TestGoalsSharedInBrief(t *testing.T) {
	parallel(t)
	s := goalServer(t, nil)
	declare(t, s, theGoals)
	ctx := context.Background()
	again(t, s)
	b, _ := json.Marshal(s.ownStatus())
	var st struct {
		Goals json.RawMessage `json:"goals"`
	}
	json.Unmarshal(b, &st)
	if len(st.Goals) == 0 || len(st.Goals) > 200 || strings.Contains(string(st.Goals), "none-missed") {
		t.Errorf("the status's goals: %s", st.Goals)
	}
	kept := func() goalsShown {
		raw, err := s.l.GoalsShown(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var g goalsShown
		if err := json.Unmarshal(raw, &g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	first := kept()
	if len(first.Lines) != 7 {
		t.Errorf("kept: %+v", first)
	}
	// Worked out again later, the same: not written again.
	s.now = func() time.Time { return now.Add(time.Minute) }
	again(t, s)
	if k := kept(); !k.At.Equal(first.At) {
		t.Errorf("written again unchanged: %v, then %v", first.At, k.At)
	}
	// Changed: written.
	if _, err := s.l.SyncFile(ctx, goalIncidents[:2], s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	again(t, s)
	if k := kept(); k.At.Equal(first.At) {
		t.Error("not written once it changed")
	}
}

// A value missed is cut away from its bound, so that it never reads as
// the bound: down for at_least, up for at_most; a quotient a hair off a
// tenth of a percent is that tenth.
func TestGoalValueCut(t *testing.T) {
	parallel(t)
	f := func(v float64) *float64 { return &v }
	least, most := ledger.Goal{Measure: "precision", AtLeast: "0.8"}, ledger.Goal{Measure: "refired.share", AtMost: "0.1"}
	for _, c := range []struct {
		g    ledger.Goal
		v    float64
		want string
	}{
		{least, 0.7995, "79.9%"}, {least, 0.2, "20%"}, {least, 0.79999, "79.9%"},
		{most, 0.10004, "10.1%"}, {most, 0.35, "35%"}, {most, 0.7 / 2, "35%"},
	} {
		if got := goalMissedValue(c.g, f(c.v)); got != c.want {
			t.Errorf("%s %v: %s, want %s", c.g.Measure, c.v, got, c.want)
		}
	}
	if got := goalWeekValue(summaryGoal{Measure: "precision", AtLeast: f(0.8), Value: f(0.7995), Result: "missed"}); got != "79.9%" {
		t.Errorf("a week missed: %s", got)
	}
	if got := goalWeekValue(summaryGoal{Measure: "precision", AtLeast: f(0.8), Value: f(0.80049), Result: "met"}); got != "80%" {
		t.Errorf("a week met: %s", got)
	}
	// A bound is shown as written, unrounded: a value missed, cut away
	// from it, never reads as it.
	fine := ledger.Goal{Measure: "precision", AtLeast: "0.8004"}
	if got, v := goalTarget("en", fine), goalMissedValue(fine, f(0.8001)); got != "at least 80.04%" || v != "80%" {
		t.Errorf("a bound of four places: %s, %s", got, v)
	}
	if got := goalTarget("en", ledger.Goal{Measure: "refired.share", AtMost: "0.07"}); got != "at most 7%" {
		t.Errorf("a bound of two places: %s", got)
	}
}

// A goal met at its bound exactly is met: at_least is a floor, at_most a
// ceiling, both included.
func TestGoalAtItsBound(t *testing.T) {
	parallel(t)
	s := goalServer(t, nil)
	declare(t, s, "goals:\n  - {name: all-caught, measure: critical.share_caught, at_least: 1}\n  - {name: two-paged, measure: critical.firings, at_most: 2}\n")
	sum := summaryOfGoals(t, s)
	for _, name := range []string{"all-caught", "two-paged"} {
		if g := goalIn(t, sum, "2026-10-26", "", name); g.Result != "met" {
			t.Errorf("%s at its bound: %+v", name, g)
		}
	}
}

// A goal of each team's missed by more teams than a notice names: one
// notice, 50 named with their values, how many more counted, and every
// one of them marked told.
func TestGoalNoticeNamesFifty(t *testing.T) {
	parallel(t)
	s := hooked(t, false)
	ctx := context.Background()
	for i := range 55 {
		if _, err := s.l.PutTeam(ctx, ledger.Team{Name: fmt.Sprintf("quiet%02d", i), Rules: fmt.Sprintf("^NoRule%02d$", i)}, "ana"); err != nil {
			t.Fatal(err)
		}
	}
	// payments and search fired; the 55 others, nothing.
	declare(t, s, "goals:\n  - {name: some-firing, team: \"*\", measure: firings, at_least: 1}\n")
	again(t, s)
	ns := goalNotices(t, s)
	if len(ns) != 1 || ns[0].Team != "*" || len(ns[0].Teams) != 50 || ns[0].More != 5 || ns[0].Teams[0].Team != "quiet00" || *ns[0].Teams[0].Value != 0 {
		t.Fatalf("told: %d notices, %+v", len(ns), ns)
	}
	if line := s.line(notice{Event: "goal.missed", Goal: &ns[0]}); !strings.Contains(line, "by 55 teams: quiet00 (0), quiet01 (0)") || !strings.HasSuffix(line, "and 45 more; the goal is at least 1.") {
		t.Errorf("its line: %q", line)
	}
	told, _, err := s.l.GoalMarks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	marked := 0
	for k := range told {
		if strings.HasPrefix(k, "some-firing.") {
			marked++
		}
	}
	if marked != 55 {
		t.Errorf("%d teams marked told, want 55", marked)
	}
}

// A replica that follows shows what the ledger holds of the goals by the
// sum the leader's status says: nothing with nothing kept; what it read,
// unkept, while the ledger holds other than the sum (a write since, or one
// that failed); kept once it holds what the sum is of; and read again when
// another leader says another. The status says the sum of what was kept,
// not of what was worked out and not written.
func TestGoalsFollowed(t *testing.T) {
	parallel(t)
	s := goalServer(t, nil)
	ctx := context.Background()
	brief := func(g *goalsShown) *goalsBrief {
		return &goalsBrief{Gen: goalsSum(g), Week: g.Week, Lines: len(g.Lines)}
	}
	keep := func(g *goalsShown) {
		t.Helper()
		b, _ := json.Marshal(g)
		if err := s.l.KeepGoalsShown(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	a := &goalsShown{Week: dayAt("2026-10-26", 0), Lines: []goalShown{{Goal: "a", Result: "met"}}}
	b := &goalsShown{Week: dayAt("2026-10-26", 0), Lines: []goalShown{{Goal: "a", Result: "missed"}}}
	c := &goalsShown{Week: dayAt("2026-11-02", 0), Lines: []goalShown{{Goal: "b", Result: "met"}}}
	if g := s.goalsFollow(ctx, nil); g != nil {
		t.Errorf("no goals said: %+v", g)
	}
	if g := s.goalsFollow(ctx, brief(b)); g != nil {
		t.Errorf("nothing kept: %+v", g)
	}
	// The ledger holds a, the leader says b: a shown, not kept as b.
	keep(a)
	if g := s.goalsFollow(ctx, brief(b)); g == nil || g.Lines[0].Result != "met" || s.goalsF.shown != nil {
		t.Errorf("stale: %+v, kept %+v", g, s.goalsF.shown)
	}
	keep(b)
	if g := s.goalsFollow(ctx, brief(b)); g == nil || g.Lines[0].Result != "missed" || s.goalsF.gen != goalsSum(b) {
		t.Errorf("written: %+v", g)
	}
	// Another leader, another sum.
	keep(c)
	if g := s.goalsFollow(ctx, brief(c)); g == nil || g.Lines[0].Goal != "b" {
		t.Errorf("a leader change: %+v", g)
	}
	// Worked out, not written: the status says what was.
	s.goalsK.Lock()
	s.goalsK.shown, s.goalsK.sum, s.goalsK.kept = b, goalsSum(b), goalsSum(a)
	s.goalsK.Unlock()
	if g := s.goalsBriefNow(); g.Gen != goalsSum(a) {
		t.Errorf("the status says %s, of what wasn't written", g.Gen)
	}
}

// As a term of leading begins, what the ledger holds of the goals is
// served, and its sum said, until the first round: /metrics keeps its
// series through a failover, and a first round that works out the same
// writes nothing. As it ends, what this replica worked out is let go.
func TestGoalsTerm(t *testing.T) {
	parallel(t)
	s := goalServer(t, nil)
	declare(t, s, theGoals)
	ctx := context.Background()
	again(t, s)
	raw, _ := s.l.GoalsShown(ctx)
	var was goalsShown
	json.Unmarshal(raw, &was)
	s.goalsTerm(ctx, true)
	if s.goalsShownNow() != nil || s.goalsBriefNow() != nil {
		t.Fatal("kept past its term")
	}
	s.goalsTerm(ctx, false)
	if g := s.goalsShownNow(); g == nil || len(g.Lines) != 7 || s.goalsBriefNow().Gen != goalsSum(&was) {
		t.Fatalf("a term begun: %+v", g)
	}
	_, metrics, _ := get(t, s.Handler(), "/metrics")
	if !strings.Contains(metrics, `lapilli_goal_met{goal="none-missed"} 0`+"\n") {
		t.Error("no series before the term's first round")
	}
	s.now = func() time.Time { return now.Add(time.Minute) }
	again(t, s)
	raw, _ = s.l.GoalsShown(ctx)
	var after goalsShown
	json.Unmarshal(raw, &after)
	if !after.At.Equal(was.At) {
		t.Errorf("the first round wrote the same again: %v, then %v", was.At, after.At)
	}
}
