package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// day is a time on a day, in UTC, as the goals' ledger is counted.
func dayAt(day string, hour int) time.Time {
	d, err := time.Parse("2006-01-02", day)
	if err != nil {
		panic(err)
	}
	return d.Add(time.Duration(hour) * time.Hour)
}

// goalIncidents are the goals' ledger's incidents: INC-A, caught by one of
// Paged's five firings in the week of 10-19; INC-B, by both of its two in
// the week of 10-26, and INC-C that week, on cart, by none.
var goalIncidents = []incident.Incident{
	{ID: "INC-A", Start: dayAt("2026-10-21", 10), End: dayAt("2026-10-21", 10).Add(30 * time.Minute), Labels: incident.Labels{"service": {"checkout"}}},
	{ID: "INC-B", Start: dayAt("2026-10-27", 10), End: dayAt("2026-10-27", 11), Labels: incident.Labels{"service": {"checkout"}}},
	{ID: "INC-C", Start: dayAt("2026-10-28", 10), End: dayAt("2026-10-28", 11), Labels: incident.Labels{"service": {"cart"}}},
}

// goalServer is a server whose ledger holds, from the week of 2026-10-05
// to an hour before now (Tuesday 2026-11-03), firings of Paged (critical,
// payments's, which owns checkout) and Noise (warning, search's, which has
// no labels), and goalIncidents:
//
//   - the week of 10-05: Paged's first firing, a week the ledger holds
//     only part of;
//   - 10-12: two of Paged's, with no incident written down near them;
//   - 10-19: five of Paged's, one catching INC-A;
//   - 10-26, the last whole week: two of Paged's, both catching INC-B;
//     INC-C, missed; Noise twice, the second re-firing;
//   - 11-02, this week: forty-one of Paged's so far.
func goalServer(t *testing.T, change func(*Config)) *Server {
	t.Helper()
	s, _ := server(t, func(c *Config) {
		c.Incidents, c.Impact = "", ""
		if change != nil {
			change(c)
		}
	})
	ctx := context.Background()
	o := s.cfg.Options
	// Each of Paged's an alert of its own, by its instance, re-firing
	// none; Noise's alike, so that its second re-fires.
	n := 0
	fire := func(rule, service string, at time.Time) firing.Firing {
		labels := map[string]string{"service": service}
		if rule == "Paged" {
			n++
			labels["instance"] = fmt.Sprint("i-", n)
		}
		return firing.Firing{Rule: rule, Labels: labels, Start: at, End: at.Add(10 * time.Minute)}
	}
	fs := []firing.Firing{fire("Paged", "checkout", dayAt("2026-10-06", 10)),
		fire("Paged", "checkout", dayAt("2026-10-13", 10)), fire("Paged", "checkout", dayAt("2026-10-14", 10))}
	for h := 0; h < 5; h++ {
		fs = append(fs, fire("Paged", "checkout", dayAt("2026-10-21", 10+h)))
	}
	fs = append(fs, fire("Paged", "checkout", dayAt("2026-10-27", 10)), fire("Paged", "checkout", dayAt("2026-10-27", 10).Add(30*time.Minute)),
		fire("Noise", "search", dayAt("2026-10-28", 12)), fire("Noise", "search", dayAt("2026-10-28", 12).Add(30*time.Minute)))
	for i := 0; i < 41; i++ {
		fs = append(fs, fire("Paged", "checkout", dayAt("2026-11-02", 0).Add(time.Duration(i)*30*time.Minute)))
	}
	if err := s.l.Record(ctx, ledger.Found{From: dayAt("2026-10-05", 0), To: now.Add(-time.Hour), Firings: fs,
		Rules: []ledger.Rule{{Name: "Paged", Severity: "critical"}, {Name: "Noise", Severity: "warning"}},
		Run:   ledger.Run{Kind: "backfill", Started: now, Finished: now}}, o); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.SyncFile(ctx, goalIncidents, o); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "payments", Rules: "^Paged$", Labels: incident.Labels{"service": {"checkout"}}}, "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "search", Rules: "^Noise$"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if err := s.l.JudgePending(ctx, o); err != nil {
		t.Fatal(err)
	}
	return s
}

// theGoals are the goals goalServer is held to, as a file declares them.
const theGoals = `goals:
  - name: critical-caught
    measure: critical.share_caught
    at_least: 0.8
  - name: none-missed
    measure: incidents.missed
    at_most: 0
  - name: paged
    team: payments
    measure: critical.firings
    critical: [critical, page]
    at_most: 40
  - name: no-refire
    team: "*"
    measure: refired.firings
    at_most: 0
  - name: every-missed
    team: "*"
    measure: incidents.missed
    at_most: 0
`

// declare keeps the server's ledger to the goals a file declares, as the
// leader does with --config.
func declare(t *testing.T, s *Server, body string) {
	t.Helper()
	d, err := ParseConfig([]byte(body), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if _, skipped, err := s.l.Reconcile(context.Background(), d, ledger.Leave{}, s.cfg.Options); err != nil || len(skipped) > 0 {
		t.Fatalf("%v %v", skipped, err)
	}
}

// summaryOfGoals is /api/v1/summary over the five weeks, every team's.
func summaryOfGoals(t *testing.T, s *Server) summary {
	t.Helper()
	code, body, _ := get(t, s.Handler(), "/api/v1/summary?from=2026-10-05&to=2026-11-02&team=*")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var sum summary
	if err := json.Unmarshal([]byte(body), &sum); err != nil {
		t.Fatal(err)
	}
	return sum
}

// goalIn is a goal of a row, by week and team.
func goalIn(t *testing.T, sum summary, week, team, name string) summaryGoal {
	t.Helper()
	for _, r := range sum.Rows {
		if r.From.Format("2006-01-02") == week && r.Team == team {
			for _, g := range r.Goals {
				if g.Name == name {
					return g
				}
			}
			t.Fatalf("no goal %s in the row of %s %q: %+v", name, week, team, r.Goals)
		}
	}
	t.Fatalf("no row of %s %q", week, team)
	return summaryGoal{}
}

// Each week of each goal, in the summary: met, missed, not known with why
// (the week the ledger's first firing fell in, a week with no record, a
// team with no labels whose goal is on incidents, a share with nothing
// to divide), and the week under way open, its count over its at_most so
// far; a goal's own list of severities counted as critical, whatever the
// summary is asked; a team's row holding its own goals and each team's,
// everyone's its own; schema 1.
func TestGoalsWeekByWeek(t *testing.T) {
	s := goalServer(t, nil)
	declare(t, s, theGoals)
	sum := summaryOfGoals(t, s)
	if sum.Schema != 1 {
		t.Errorf("schema %d", sum.Schema)
	}
	f := func(v float64) *float64 { return &v }
	for _, c := range []struct {
		week, team, goal string
		want             summaryGoal
	}{
		{"2026-10-05", "", "critical-caught", summaryGoal{Result: "unknown", Why: "not_whole"}},
		{"2026-10-12", "", "critical-caught", summaryGoal{Result: "unknown", Why: "no_record"}},
		{"2026-10-12", "", "none-missed", summaryGoal{Result: "unknown", Why: "no_record"}},
		{"2026-10-19", "", "critical-caught", summaryGoal{Result: "missed", Value: f(0.2)}},
		{"2026-10-19", "", "none-missed", summaryGoal{Result: "met", Value: f(0)}},
		{"2026-10-26", "", "critical-caught", summaryGoal{Result: "met", Value: f(1)}},
		{"2026-10-26", "", "none-missed", summaryGoal{Result: "missed", Value: f(1)}},
		{"2026-11-02", "", "none-missed", summaryGoal{Result: "open", Value: f(0)}},
		// The team's own goal, of its own severities; and each team's.
		{"2026-10-26", "payments", "paged", summaryGoal{Result: "met", Value: f(2)}},
		{"2026-11-02", "payments", "paged", summaryGoal{Result: "open", Value: f(41), Over: true}},
		{"2026-10-26", "payments", "no-refire", summaryGoal{Result: "met", Value: f(0)}},
		{"2026-10-26", "search", "no-refire", summaryGoal{Result: "missed", Value: f(1)}},
		{"2026-10-26", "payments", "every-missed", summaryGoal{Result: "met", Value: f(0)}},
		{"2026-10-26", "search", "every-missed", summaryGoal{Result: "unknown", Why: "team_owns_by_catching"}},
	} {
		g := goalIn(t, sum, c.week, c.team, c.goal)
		if g.Result != c.want.Result || g.Why != c.want.Why || g.Over != c.want.Over ||
			(g.Value == nil) != (c.want.Value == nil) || g.Value != nil && *g.Value != *c.want.Value {
			t.Errorf("%s %q %s: %+v, want %+v", c.week, c.team, c.goal, g, c.want)
		}
	}
	// A share with nothing to divide is not known: no critical firing of
	// payments's in the week of 10-12 had a record, and search fired none.
	if g := goalIn(t, sum, "2026-10-26", "", "critical-caught"); g.AtLeast == nil || *g.AtLeast != 0.8 || g.AtMost != nil {
		t.Errorf("its bound: %+v", g)
	}
	for _, r := range sum.Rows {
		var names []string
		for _, g := range r.Goals {
			names = append(names, g.Name)
		}
		want := []string{"critical-caught", "none-missed"}
		switch r.Team {
		case "payments":
			want = []string{"every-missed", "no-refire", "paged"}
		case "search":
			want = []string{"every-missed", "no-refire"}
		}
		if !slices.Equal(names, want) {
			t.Errorf("%s %q: goals %v, want %v", r.From.Format("2006-01-02"), r.Team, names, want)
		}
	}
	// The summary's own critical= changes the row's critical, not a goal's.
	_, body, _ := get(t, s.Handler(), "/api/v1/summary?from=2026-10-26&to=2026-10-26&team=payments&critical=page")
	var asked summary
	json.Unmarshal([]byte(body), &asked)
	if len(asked.Rows) != 1 || asked.Rows[0].Critical.Firings != 0 || len(asked.Rows[0].Goals) != 3 || *asked.Rows[0].Goals[2].Value != 2 {
		t.Errorf("critical=page: %+v", asked.Rows)
	}
	// With no critical firing judged, a share caught isn't known.
	declare(t, s, "goals:\n  - {name: sev1-caught, measure: critical.share_caught, critical: [sev1], at_least: 0.5}\n")
	sum = summaryOfGoals(t, s)
	if g := goalIn(t, sum, "2026-10-26", "", "sev1-caught"); g.Result != "unknown" || g.Why != "no_value" || g.Value != nil {
		t.Errorf("nothing to divide: %+v", g)
	}
}

// Without goals: nothing changes. The summary has no goals, and every
// other field of it reads the same with goals declared; /metrics has no
// goal's series, the overview no Goals card, the digest no line of them;
// the leader's work after a run writes nothing, tells nothing and makes
// no task.
func TestGoalsNothingChanges(t *testing.T) {
	s := goalServer(t, func(c *Config) {
		hook := filepath.Join(t.TempDir(), "hook")
		os.WriteFile(hook, []byte("https://hooks.example/x\n"), 0o600)
		c.NotifyWebhook, c.GoalTasks = hook, true
	})
	ctx := context.Background()
	h := s.Handler()
	_, before, _ := get(t, h, "/api/v1/summary?from=2026-10-05&to=2026-11-02&team=*")
	if strings.Contains(before, `"goals"`) {
		t.Errorf("a summary with no goals declared gives goals: %s", before)
	}
	gen := s.l.Gen()
	if err := s.goalsAfter(ctx); err != nil {
		t.Fatal(err)
	}
	if s.l.Gen() != gen || s.goalsShownNow() != nil {
		t.Error("the leader's work wrote, or kept, something with no goals")
	}
	if due, _ := s.l.Due(ctx, "webhook", now.Add(time.Hour), 100); len(due) != 0 {
		t.Errorf("told with no goals: %+v", due)
	}
	if ts, _ := s.l.Tasks(ctx, ledger.TaskQuery{}); len(ts) != 0 {
		t.Errorf("tasks with no goals: %+v", ts)
	}
	_, metrics, _ := get(t, h, "/metrics")
	_, overview, _ := get(t, h, "/")
	if strings.Contains(metrics, "lapilli_goal_") || strings.Contains(overview, `<h2>Goals</h2>`) {
		t.Error("a goal's series, or the Goals card, with no goals declared")
	}
	text, err := s.digestText(ctx, &verdict.Report{From: dayAt("2026-10-26", 0), To: dayAt("2026-11-02", 0)}, nil, dayAt("2026-10-26", 0), dayAt("2026-11-02", 0), nil)
	if err != nil || strings.Contains(text, "Goals:") {
		t.Errorf("the digest, with no goals: %q %v", text, err)
	}
	// Declared, they are added, and nothing else of the summary changes.
	declare(t, s, theGoals)
	_, after, _ := get(t, s.Handler(), "/api/v1/summary?from=2026-10-05&to=2026-11-02&team=*")
	var a, b map[string]any
	json.Unmarshal([]byte(before), &a)
	json.Unmarshal([]byte(after), &b)
	for _, r := range b["rows"].([]any) {
		if _, ok := r.(map[string]any)["goals"]; !ok {
			t.Errorf("a row without its goals: %v", r)
		}
		delete(r.(map[string]any), "goals")
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Errorf("goals changed the rest of the summary:\n%s\n%s", ja, jb)
	}
}

// After a run, the leader works out the last whole week of each goal and
// team, from one read of 13 weeks for everyone and each team a goal is
// of; /metrics reads what it worked out, missed first; goal.missed is told
// once for each goal, team and week; --goal-tasks makes a task of each
// missed, 20 a run (here 1), the rest waiting, said on Run history; and
// an open task is said on as later weeks are missed again, met, and no
// longer missed, once each, never closed, and not made again for a week
// it was made for once it is done.
func TestGoalsAfterRun(t *testing.T) {
	hook := filepath.Join(t.TempDir(), "hook")
	os.WriteFile(hook, []byte("https://hooks.example/x\n"), 0o600)
	s := goalServer(t, func(c *Config) { c.NotifyWebhook, c.GoalTasks = hook, true })
	declare(t, s, theGoals)
	ctx := context.Background()
	was := goalTasksARun
	goalTasksARun = 1
	defer func() { goalTasksARun = was }()
	// One read of the thirteen weeks, for everyone, payments and search.
	rows := 0
	summaryRowSeen = func() { rows++ }
	err := s.goalsAfter(ctx)
	summaryRowSeen = nil
	if err != nil {
		t.Fatal(err)
	}
	if rows != 13*3 {
		t.Errorf("the leader's work read %d rows, want %d", rows, 13*3)
	}
	shown := s.goalsShownNow()
	if shown == nil || !shown.Week.Equal(dayAt("2026-10-26", 0)) || len(shown.Lines) != 7 || shown.Waiting != 1 {
		t.Fatalf("worked out: %+v", shown)
	}
	if l := shown.Lines[0]; l.Result != "missed" || l.Goal != "no-refire" && l.Goal != "none-missed" {
		t.Errorf("missed first: %+v", shown.Lines)
	}
	_, metrics, _ := get(t, s.Handler(), "/metrics")
	for _, want := range []string{
		`lapilli_goal_met{goal="none-missed"} 0`, `lapilli_goal_met{goal="critical-caught"} 1`, `lapilli_goal_met{goal="no-refire",team="search"} 0`,
		`lapilli_goal_known{goal="every-missed",team="search"} 0`, `lapilli_goal_known{goal="paged",team="payments"} 1`,
		`lapilli_goal_value{goal="critical-caught"} 1`, `lapilli_goal_value{goal="paged",team="payments"} 2`,
		`lapilli_goal_weeks_met{goal="critical-caught"} 1`, `lapilli_goal_weeks_met{goal="none-missed"} 1`,
	} {
		if !strings.Contains(metrics, want+"\n") {
			t.Errorf("/metrics has no %s", want)
		}
	}
	if strings.Contains(metrics, `lapilli_goal_met{goal="every-missed",team="search"}`) || strings.Contains(metrics, `lapilli_goal_value{goal="every-missed",team="search"}`) {
		t.Error("a goal not known has a lapilli_goal_met or a value")
	}
	// Told once each: none-missed's, and no-refire's of search.
	told := func() []string {
		due, err := s.l.Due(ctx, "webhook", now.Add(30*24*time.Hour), 500)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, n := range due {
			if n.Event == "goal.missed" {
				var x struct {
					Goal goalNotice `json:"goal"`
				}
				json.Unmarshal(n.Body, &x)
				teams := ""
				for _, y := range x.Goal.Teams {
					teams += "/" + y.Team
				}
				out = append(out, x.Goal.Name+"/"+x.Goal.Team+"/"+x.Goal.From.Format("2006-01-02")+teams)
			}
		}
		slices.Sort(out)
		return out
	}
	// A goal of each team's, once, with its teams.
	if got := told(); !slices.Equal(got, []string{"no-refire/*/2026-10-26/search", "none-missed//2026-10-26"}) {
		t.Errorf("told: %v", got)
	}
	// One task a run: no-refire's first, by its name; none-missed's waits.
	tasks := func() []ledger.Task {
		ts, err := s.l.Tasks(ctx, ledger.TaskQuery{AnyGoal: true})
		if err != nil {
			t.Fatal(err)
		}
		slices.SortFunc(ts, func(a, b ledger.Task) int { return int(a.ID - b.ID) })
		return ts
	}
	ts := tasks()
	if len(ts) != 1 || ts[0].Kind != "decide" || ts[0].Goal != "no-refire" || ts[0].TeamID == 0 || ts[0].CreatedBy != ledger.GoalTasksBy ||
		ts[0].Title != "Goal no-refire of search missed, week of 2026-10-26" || !strings.Contains(ts[0].Body, "firings that re-fired was 1; the goal is at most 0") {
		t.Fatalf("made: %+v", ts)
	}
	_, runs, _ := get(t, s.Handler(), "/runs")
	if !strings.Contains(runs, "1 tasks for goals missed wait for the runs after") {
		t.Error("Run history doesn't say what waits")
	}
	// The next round: the one that waited, and nothing told again.
	if err := s.goalsAfter(ctx); err != nil {
		t.Fatal(err)
	}
	if ts = tasks(); len(ts) != 2 || ts[1].Goal != "none-missed" || ts[1].TeamID != 0 || s.goalsShownNow().Waiting != 0 {
		t.Fatalf("the next round: %+v", ts)
	}
	if got := told(); len(got) != 2 {
		t.Errorf("told again: %v", got)
	}
	none := ts[1].ID
	comments := func() []string {
		cs, err := s.l.Comments(ctx, "task", fmt.Sprint(none))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range cs {
			out = append(out, c.Body)
		}
		return out
	}
	// A week on: an incident on cart that week missed again. Said on the
	// open task, and told; no second task.
	s.now = func() time.Time { return now.Add(7 * 24 * time.Hour) }
	if _, err := s.l.SyncFile(ctx, append(slices.Clone(goalIncidents),
		incident.Incident{ID: "INC-D", Start: dayAt("2026-11-04", 5), End: dayAt("2026-11-04", 6), Labels: incident.Labels{"service": {"cart"}}}), s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	if err := s.l.Record(ctx, ledger.Found{From: now, To: now.Add(7 * 24 * time.Hour), Rules: []ledger.Rule{{Name: "Paged", Severity: "critical"}},
		Run: ledger.Run{Kind: "hourly", Started: now, Finished: now}}, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	if err := s.goalsAfter(ctx); err != nil {
		t.Fatal(err)
	}
	if got := comments(); !slices.Equal(got, []string{"Missed again, week of 2026-11-02: 1."}) {
		t.Errorf("said: %q", got)
	}
	if ts := tasks(); slices.ContainsFunc(ts, func(x ledger.Task) bool { return x.Goal == "none-missed" && x.ID != none }) {
		t.Errorf("a second task for an open one: %+v", ts)
	}
	// INC-C taken out: the week it was made for is no longer missed. Said
	// once; the task stays open.
	if _, err := s.l.SyncFile(ctx, []incident.Incident{goalIncidents[0], goalIncidents[1],
		{ID: "INC-D", Start: dayAt("2026-11-04", 5), End: dayAt("2026-11-04", 6), Labels: incident.Labels{"service": {"cart"}}}}, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.goalsAfter(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := comments(); !slices.Equal(got, []string{"Missed again, week of 2026-11-02: 1.", "No longer missed, week of 2026-10-26: 0."}) {
		t.Errorf("said: %q", got)
	}
	if x, _ := s.l.TaskByID(ctx, none); x.Status != "open" {
		t.Errorf("closed for someone: %+v", x)
	}
	// Done: the week said on it isn't made a task again; nothing more is
	// said on it.
	if _, err := s.l.PatchTask(ctx, none, []byte(`{"status": "done"}`), "ana"); err != nil {
		t.Fatal(err)
	}
	if err := s.goalsAfter(ctx); err != nil {
		t.Fatal(err)
	}
	if ts := tasks(); slices.ContainsFunc(ts, func(x ledger.Task) bool { return x.Goal == "none-missed" && x.ID != none }) {
		t.Errorf("made again for a week a task was made for: %+v", ts)
	}
	if ids, _ := s.l.GoalTasksKept(ctx); slices.Contains(ids, none) {
		t.Error("still kept of a task done")
	}
}

// The overview's Goals card, and a goal missed first in What to look at
// with Make it a task, which makes the task with one press, of the goal's
// team, titled with the goal and the week; pressed again, the task open;
// a week the goal wasn't missed refused; a team's editor makes one of
// their team's goal, not of everyone's. Then the line links to it.
func TestGoalTaskButton(t *testing.T) {
	s := goalServer(t, func(c *Config) {
		c.UserHeader, c.Admins, c.Editors = "X-Forwarded-Email", []string{"boss@example.com"}, []string{"ed@example.com"}
	})
	declare(t, s, "goals:\n  - {name: none-missed, measure: incidents.missed, at_most: 0}\n  - {name: payments-paged, team: payments, measure: critical.firings, at_most: 1}\n")
	h := s.Handler()
	last := "2026-10-26"
	asEditor := func(path string) string {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-Forwarded-Email", "ed@example.com")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Body.String()
	}
	body := asEditor("/")
	if !strings.Contains(body, "<h2>Goals</h2>") || !strings.Contains(body, "0 of 1 goals met") || !strings.Contains(body, "Goal none-missed missed last week") ||
		!strings.Contains(body, `action="/goals/task"`) || !strings.Contains(body, `name="week" value="`+last+`"`) {
		t.Fatalf("the overview: no card, or no line with its form")
	}
	if strings.Index(body, "Goal none-missed missed last week") > strings.Index(body, "Make it a task") {
		t.Error("a goal missed isn't the first line of What to look at")
	}
	as := func(who string, form url.Values) (int, string) {
		r := httptest.NewRequest(http.MethodPost, "/goals/task", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("X-Forwarded-Email", who)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Header().Get("Location") + w.Body.String()
	}
	if code, out := as("ed@example.com", url.Values{"goal": {"none-missed"}, "week": {last}, "back": {"/"}}); code != http.StatusSeeOther {
		t.Fatalf("made: %d %s", code, out)
	}
	ts, _ := s.l.Tasks(context.Background(), ledger.TaskQuery{Goal: "none-missed"})
	if len(ts) != 1 || ts[0].Kind != "decide" || ts[0].TeamID != 0 || ts[0].Title != "Goal none-missed missed, week of "+last || ts[0].CreatedBy != "ed@example.com" ||
		!strings.Contains(ts[0].Body, "/goals#goal-none-missed") {
		t.Fatalf("the task: %+v", ts)
	}
	if code, out := as("ed@example.com", url.Values{"goal": {"none-missed"}, "week": {last}}); code != http.StatusSeeOther || !strings.Contains(out, fmt.Sprintf("/tasks/%d", ts[0].ID)) {
		t.Errorf("again: %d %s", code, out)
	}
	if n, _ := s.l.Tasks(context.Background(), ledger.TaskQuery{Goal: "none-missed"}); len(n) != 1 {
		t.Errorf("pressed twice, %d tasks", len(n))
	}
	// Not missed that week, a week that isn't a Monday, a goal there isn't.
	for _, c := range []struct {
		form url.Values
		says string
	}{
		{url.Values{"goal": {"none-missed"}, "week": {"2026-10-19"}}, "wasn&#39;t missed in the week of 2026-10-19"},
		{url.Values{"goal": {"none-missed"}, "week": {"2026-10-28"}}, "want its Monday"},
		{url.Values{"goal": {"nope"}, "week": {last}}, "there is no goal called &#34;nope&#34;"},
	} {
		if code, out := as("ed@example.com", c.form); code < 400 || !strings.Contains(out, c.says) {
			t.Errorf("%v: %d %s", c.form, code, out)
		}
	}
	// A team's editor: their team's goal, not everyone's.
	if _, err := s.l.PutGrant(context.Background(), ledger.Grant{Who: "user:pat@example.com", TeamID: teamOf(t, s, "payments").ID}, "boss@example.com"); err != nil {
		t.Fatal(err)
	}
	if code, out := as("pat@example.com", url.Values{"goal": {"payments-paged"}, "week": {last}}); code != http.StatusSeeOther {
		t.Errorf("a team's editor, their team's: %d %s", code, out)
	}
	if ts, _ := s.l.Tasks(context.Background(), ledger.TaskQuery{Goal: "payments-paged"}); len(ts) != 1 || ts[0].TeamID != teamOf(t, s, "payments").ID {
		t.Errorf("of the team: %+v", ts)
	}
	if code, _ := as("pat@example.com", url.Values{"goal": {"none-missed"}, "week": {last}}); code != http.StatusForbidden {
		t.Errorf("a team's editor, everyone's: %d", code)
	}
	if body := asEditor("/"); !strings.Contains(body, fmt.Sprintf(`href="/tasks/%d">task #%d</a>`, ts[0].ID, ts[0].ID)) {
		t.Error("the line doesn't link to its task")
	}
}

// The Goals page: each goal, its team, its target, its last whole week
// and its twelve whole weeks and the week under way, in status colours,
// each with its value, and why when not known; with a team picked, its
// goals and everyone's, reading their rows alone; and in every language.
func TestGoalsPage(t *testing.T) {
	s := goalServer(t, nil)
	h := s.Handler()
	if _, body, _ := get(t, h, "/goals"); !strings.Contains(body, "No goals are declared") {
		t.Error("no goals: the page doesn't say how to declare them")
	}
	declare(t, s, theGoals)
	_, body, _ := get(t, h, "/goals")
	for _, want := range []string{`id="goal-critical-caught"`, "critical firings that caught something: at least 80%",
		`class="gw status-bad" title="week of 2026-10-19: not met, 20%"`, `title="week of 2026-10-26: met, 100%"`,
		`title="week of 2026-10-12: not known: no incident written down to judge the firings by, –"`,
		`title="week of 2026-10-05: not known: the ledger holds only part of the week, –"`,
		`class="gw status-warn" title="week of 2026-11-02: over so far, 41"`, "counted as critical: critical, page",
		"not known: the team has no labels, so it owns only the incidents its rules caught: give it labels",
		"for a team with no labels: it owns only the incidents its rules caught, so it never missed one"} {
		if !strings.Contains(body, want) {
			t.Errorf("/goals has no %s", want)
		}
	}
	rows := 0
	summaryRowSeen = func() { rows++ }
	s.cache.Lock()
	s.cache.gen = ""
	s.cache.Unlock()
	_, body, _ = get(t, h, "/goals?team=payments")
	summaryRowSeen = nil
	if rows != 13*2 || !strings.Contains(body, "<code>paged</code>") || !strings.Contains(body, "<code>none-missed</code>") || strings.Contains(body, "<bdi>search</bdi>") {
		t.Errorf("payments's page read %d rows, want 26; or shows another team's", rows)
	}
	// The overview's card reads the last whole week alone: one row.
	rows = 0
	summaryRowSeen = func() { rows++ }
	_, body, _ = get(t, h, "/?team=search")
	summaryRowSeen = nil
	if rows != 1 || !strings.Contains(body, "0 of 2 goals met") {
		t.Errorf("the card read %d rows, want 1", rows)
	}
	for _, l := range Langs() {
		r := httptest.NewRequest(http.MethodGet, "/goals", nil)
		r.Header.Set("Accept-Language", l)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Errorf("%s: %d", l, w.Code)
		}
	}
}

// The digest's line of the week's goals, digest.ready's goals, and a
// report's goals section over the span's whole weeks, for whom each is
// for.
func TestGoalsDigestAndReports(t *testing.T) {
	s := goalServer(t, nil)
	declare(t, s, theGoals)
	ctx := context.Background()
	from, to := dayAt("2026-10-26", 0), dayAt("2026-11-02", 0)
	text, err := s.digestText(ctx, &verdict.Report{From: from, To: to}, nil, from, to, nil)
	if err != nil || !strings.Contains(text, "Goals: 1 of 2 met, 1 missed, 0 not known. Goal none-missed missed in the week of 2026-10-26: incidents missed was 1; the goal is at most 0.") {
		t.Errorf("everyone's digest: %q %v", text, err)
	}
	search := teamOf(t, s, "search")
	text, _ = s.digestText(ctx, &verdict.Report{From: from, To: to}, nil, from, to, &search)
	if !strings.Contains(text, "Goals: 0 of 2 met, 1 missed, 1 not known.") {
		t.Errorf("search's digest: %q", text)
	}
	about := map[string]any{}
	if err := s.aboutGoals(ctx, about, from, &search); err != nil || len(about["goals"].([]summaryGoal)) != 2 {
		t.Errorf("digest.ready's goals: %+v %v", about, err)
	}
	line, nums, err := s.goalsSection(ctx, dayAt("2026-10-14", 0), dayAt("2026-11-03", 0), nil, time.UTC)
	if err != nil || !strings.Contains(line, "Goal critical-caught (critical firings that caught something, at least 80%): met in 1 of 2 whole weeks known, of 2. Missed in the weeks of 2026-10-19.") {
		t.Errorf("the section: %q %v", line, err)
	}
	b, _ := json.Marshal(nums)
	if !strings.Contains(string(b), `"name":"none-missed","met":1,"of":2`) {
		t.Errorf("its numbers: %s", b)
	}
	if line, _, _ := s.goalsSection(ctx, dayAt("2026-10-28", 0), dayAt("2026-11-01", 0), nil, time.UTC); !strings.Contains(line, "holds no whole week") {
		t.Errorf("a span of no whole week: %q", line)
	}
}

// goals: in --config is read strictly: what breaks a rule refuses the
// file, with its line; a goal over a period refused, saying why, until
// marked windows are kept; a goal of a team there isn't skipped and said;
// and an export gives them back as the file declares them.
func TestParseConfigGoals(t *testing.T) {
	d, err := ParseConfig([]byte("teams:\n  - name: payments\n"+theGoals), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Goals) != 5 || d.Goals[0].Line != 4 || d.Goals[0].AtLeast != "0.8" || d.Goals[2].Team != "payments" ||
		!slices.Equal(d.Goals[2].Critical, []string{"critical", "page"}) || d.Goals[3].Team != "*" || d.Goals[1].Each != "week" {
		t.Errorf("goals: %+v", d.Goals)
	}
	g := "goals:\n  - name: a\n"
	for _, c := range []struct{ body, want string }{
		{g + "    measure: precision\n    at_least: 0.8\n    at_most: 0.9\n", "line 2: a goal has one bound: at_least, at_most or no_worse_than"},
		{g + "    measure: precision\n", "line 2: a goal has one bound"},
		{g + "    measure: precision\n    at_least: 0.8\n    by: 0.05\n", "line 2: by is the slack of no_worse_than"},
		{g + "    measure: firings\n    critical: [page]\n    at_most: 4\n", "line 2: critical is the severities a critical.* measure counts"},
		{g + "    measure: firings\n    at_most: 4\n    each: deploy\n", "line 2: each: deploy: deploys are counted by during_deploys"},
		{g + "    measure: precision\n    no_worse_than: before\n    by: 0.05\n    each: sale\n", "line 2: each: sale is a goal over a kind of marked window, which this lapilli doesn't keep yet"},
		{g + "    measure: incidents.missed\n    at_most: 0\n    each: sale\n", "doesn't keep yet: each takes week alone for now"},
		{g + "    measure: precision\n    at_least: 80\n", "line 2: at_least of precision is a share, from 0 to 1"},
		{g + "    measure: precision\n    at_least: true\n", "line 4: at_least is a number"},
		{g + "    measure: precision\n    at_least: 0.8\n    colour: red\n", `line 5: unknown key "colour" in a goal`},
		{g + "    measure: noise\n    at_most: 1\n", `line 2: measure "noise": want one of`},
		{g + "    at_most: 1\n", "line 2: a goal has a measure"},
		{"goals:\n  - measure: firings\n    at_most: 1\n", "line 2: a goal has a name"},
		{"goals:\n  - {name: 2026, measure: firings, at_most: 1}\n", "line 2: name is 2026, read as a boolean or a number"},
		{"goals:\n  - {name: A, measure: firings, at_most: 1}\n", "line 2: a goal's name is lower-case letters"},
		{"goals:\n  - {name: a, measure: firings, at_most: 1}\n  - {name: a, team: \"*\", measure: firings, at_most: 2}\n", "line 3: a goal called a is declared on line 2 already"},
		{"goals:\n  - {name: a, measure: firings, at_most: 1, team: }\n", "team has nothing after it"},
		{"goals:\n" + strings.Repeat("  - {name: a, measure: firings, at_most: 1}\n", ledger.MaxGoals+1), "a config declares 50 goals at most"},
		{"goals:\n  - {name: a, measure: critical.firings, at_most: 1}\n  - {name: b, measure: critical.firings, critical: [p1], at_most: 1}\n" +
			"  - {name: c, measure: critical.firings, critical: [p2], at_most: 1}\n  - {name: d, measure: critical.firings, critical: [p3], at_most: 1}\n" +
			"  - {name: e, measure: critical.firings, critical: [p4], at_most: 1}\n", "line 6: the goals count 4 lists of severities as critical at most"},
		{"goal:\n  - {name: a}\n", `line 1: unknown key "goal": want teams, windows, reports, grants or goals`},
	} {
		_, err := ParseConfig([]byte(c.body), time.UTC)
		if err == nil || !strings.Contains(configText("en", err), c.want) {
			t.Errorf("%q: %v, want %q", c.body, err, c.want)
		}
	}
	// Through the leader: a goal of a team there isn't is skipped, said,
	// and the rest kept; the export gives back the goals as declared.
	s, _ := configServer(t, "teams:\n  - {name: payments, rules: ^Errors$}\n"+theGoals+"  - {name: ghost, team: nobody, measure: firings, at_most: 1}\n", nil)
	if gs, _ := s.l.Goals(context.Background()); len(gs) != 5 {
		t.Errorf("kept: %+v", gs)
	}
	if c := s.configShown(); c == nil || len(c.Problems) != 1 || c.Problems[0].Kind != ledger.KindGoal || !strings.Contains(c.Problems[0].Text["en"], "there is no team called nobody") {
		t.Errorf("said: %+v", c)
	}
	r := httptest.NewRequest(http.MethodGet, "/config.yaml", nil)
	r.Header.Set("X-Forwarded-Email", "boss@example.com")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	back, err := ParseConfig(w.Body.Bytes(), time.UTC)
	if err != nil {
		t.Fatalf("the export: %v\n%s", err, w.Body.String())
	}
	// By name, as the ledger holds them.
	if len(back.Goals) != 5 || back.Goals[0].Name != "critical-caught" || back.Goals[0].AtLeast != "0.8" || back.Goals[2].Name != "no-refire" ||
		back.Goals[2].Team != "*" || back.Goals[4].Team != "payments" || !slices.Equal(back.Goals[4].Critical, []string{"critical", "page"}) {
		t.Errorf("exported: %+v\n%s", back.Goals, w.Body.String())
	}
	if !regexp.MustCompile(`(?m)^    at_least: 0\.8$`).MatchString(w.Body.String()) {
		t.Errorf("a number exported as a text:\n%s", w.Body.String())
	}
}
