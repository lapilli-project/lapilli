package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// periodWindows are the marked windows periodServer keeps over
// goalServer's firings:
//
//   - a flash sale, each Wednesday 10:00 to 10:30 from 10-07 to 10-21:
//     on 10-07, in the week the ledger holds only part of; on 10-14 over
//     one of Paged's two firings with no record; on 10-21 over the one of
//     Paged's five that caught INC-A;
//   - a weekend sale, 10-24, over no firing, in a week whose record is
//     kept;
//   - the autumn sale, 10-27 to 10-29, over the week of 10-26's firings:
//     Paged's two catching INC-B, Noise's two catching nothing, and INC-C,
//     on cart, missed;
//   - a launch, 10-28 12:00 to 12:45, over Noise's two;
//   - this week's sale, 11-02 to 11-05, on now, over Paged's forty-one.
func periodWindows() []ledger.Marked {
	return []ledger.Marked{
		{Kind: "sale", Title: "Flash sale", Start: dayAt("2026-10-07", 10), End: dayAt("2026-10-07", 10).Add(30 * time.Minute), Every: "week",
			Until: dayAt("2026-10-22", 0)},
		{Kind: "sale", Title: "Weekend sale", Start: dayAt("2026-10-24", 0), End: dayAt("2026-10-25", 0)},
		{Kind: "sale", Title: "Autumn sale", Start: dayAt("2026-10-27", 0), End: dayAt("2026-10-29", 0)},
		{Kind: "launch", Title: "Search, new ranking", Start: dayAt("2026-10-28", 12), End: dayAt("2026-10-28", 12).Add(45 * time.Minute)},
		{Kind: "sale", Title: "Week sale", Start: dayAt("2026-11-02", 0), End: dayAt("2026-11-05", 0)},
	}
}

// thePeriodGoals are goals over a period, beside a goal of each week.
const thePeriodGoals = `goals:
  - {name: none-missed, measure: incidents.missed, at_most: 0}
  - {name: sale-no-worse, each: sale, measure: precision, no_worse_than: before, by: 0.05}
  - {name: sale-none-missed, each: sale, measure: incidents.missed, at_most: 0}
  - {name: sale-firings, each: sale, measure: firings, at_most: 40}
  - {name: sale-lead, each: sale, measure: incidents.median_lead_seconds, no_worse_than: before}
  - {name: launch-no-worse, each: launch, measure: precision, no_worse_than: before, by: 0.05}
  - {name: sale-each-missed, team: "*", each: sale, measure: incidents.missed, at_most: 0}
`

// periodServer is goalServer with periodWindows kept and its firings
// marked, held to thePeriodGoals.
func periodServer(t *testing.T, change func(*Config)) *Server {
	t.Helper()
	s := goalServer(t, change)
	ctx := context.Background()
	for _, m := range periodWindows() {
		if _, err := s.l.PutMarked(ctx, m, "ana", s.cfg.Options); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.l.JudgePending(ctx, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	declare(t, s, thePeriodGoals)
	return s
}

// periodIn is a goal over a period of a row, by week, team, goal and the
// title of the occurrence's window.
func periodIn(t *testing.T, sum summary, week, team, name, title string) summaryGoal {
	t.Helper()
	for _, r := range sum.Rows {
		if r.From.Format("2006-01-02") != week || r.Team != team {
			continue
		}
		for _, g := range r.Goals {
			if g.Name == name && g.Title == title {
				return g
			}
		}
		t.Fatalf("no goal %s of %s in the row of %s %q", name, title, week, team)
	}
	t.Fatalf("no row of %s %q", week, team)
	return summaryGoal{}
}

// Each occurrence of a goal over a period, in the summary's row of the
// week it ended in, the one on now in this week's: its value inside it, as
// the Marked windows page counts it; of no_worse_than, beside the four
// whole weeks before, outside its kind, met when no worse than them by
// more than by; at_least and at_most inside alone; not known, with why,
// in the order a week's are tried (not_whole, team_owns_by_catching,
// no_record, no_value), and no_before last, with no value; open while it
// is on, over its at_most so far; and the occurrence's from, to, title
// and key. schema stays 1.
func TestGoalsOverPeriod(t *testing.T) {
	s := periodServer(t, nil)
	sum := summaryOfGoals(t, s)
	if sum.Schema != 1 {
		t.Errorf("schema %d", sum.Schema)
	}
	f := func(v float64) *float64 { return &v }
	for _, c := range []struct {
		week, team, goal, title string
		want                    summaryGoal
	}{
		// The flash sale: a week the ledger holds only part of, before a
		// week with no record (of incidents, a team with no labels, before
		// it); its firing with no record before a value with nothing to
		// count; then nothing in the weeks before to set it beside.
		{"2026-10-05", "", "sale-none-missed", "Flash sale", summaryGoal{Result: "unknown", Why: "not_whole"}},
		{"2026-10-12", "", "sale-none-missed", "Flash sale", summaryGoal{Result: "unknown", Why: "no_record"}},
		{"2026-10-12", "", "sale-no-worse", "Flash sale", summaryGoal{Result: "unknown", Why: "no_record"}},
		{"2026-10-12", "search", "sale-each-missed", "Flash sale", summaryGoal{Result: "unknown", Why: "team_owns_by_catching"}},
		{"2026-10-12", "", "sale-firings", "Flash sale", summaryGoal{Result: "met", Value: f(1)}},
		{"2026-10-19", "", "sale-no-worse", "Flash sale", summaryGoal{Result: "unknown", Why: "no_before"}},
		{"2026-10-19", "", "sale-none-missed", "Flash sale", summaryGoal{Result: "met", Value: f(0)}},
		{"2026-10-19", "payments", "sale-each-missed", "Flash sale", summaryGoal{Result: "met", Value: f(0)}},
		// The weekend sale: no firing, in a week whose record is kept.
		{"2026-10-19", "", "sale-no-worse", "Weekend sale", summaryGoal{Result: "unknown", Why: "no_value"}},
		{"2026-10-19", "", "sale-lead", "Weekend sale", summaryGoal{Result: "unknown", Why: "no_value"}},
		{"2026-10-19", "", "sale-none-missed", "Weekend sale", summaryGoal{Result: "met", Value: f(0)}},
		// The autumn sale: two of four caught something, beside none of
		// the four the weeks before (the flash sale's firing, of its kind,
		// left out); INC-C, missed (not payments's: it is on cart); and
		// INC-A, the weeks before's one incident, during a sale, so no
		// lead to set its own beside.
		{"2026-10-26", "", "sale-no-worse", "Autumn sale", summaryGoal{Result: "met", Value: f(0.5), Before: f(0)}},
		{"2026-10-26", "", "sale-none-missed", "Autumn sale", summaryGoal{Result: "missed", Value: f(1)}},
		{"2026-10-26", "payments", "sale-each-missed", "Autumn sale", summaryGoal{Result: "met", Value: f(0)}},
		{"2026-10-26", "", "sale-firings", "Autumn sale", summaryGoal{Result: "met", Value: f(4)}},
		{"2026-10-26", "", "sale-lead", "Autumn sale", summaryGoal{Result: "unknown", Why: "no_before"}},
		// The launch: none of Noise's two caught anything, beside one of
		// five the weeks before (the flash sale's firing in them, of
		// another kind, counted): worse by more than 0.05.
		{"2026-10-26", "", "launch-no-worse", "Search, new ranking", summaryGoal{Result: "missed", Value: f(0), Before: f(0.2)}},
		// This week's sale, on now: so far, over its at_most already.
		{"2026-11-02", "", "sale-firings", "Week sale", summaryGoal{Result: "open", Value: f(41), Over: true}},
		{"2026-11-02", "search", "sale-each-missed", "Week sale", summaryGoal{Result: "open", Value: f(0)}},
	} {
		g := periodIn(t, sum, c.week, c.team, c.goal, c.title)
		same := func(a, b *float64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
		if g.Result != c.want.Result || g.Why != c.want.Why || g.Over != c.want.Over || !same(g.Value, c.want.Value) ||
			c.want.Before != nil && !same(g.Before, c.want.Before) {
			t.Errorf("%s %q %s %s: %+v, want %+v", c.week, c.team, c.goal, c.title, g, c.want)
		}
	}
	// What says which occurrence, and of no_worse_than its bound.
	g := periodIn(t, sum, "2026-10-26", "", "sale-no-worse", "Autumn sale")
	if g.Each != "sale" || g.NoWorseThan != "before" || g.By == nil || *g.By != 0.05 || g.AtLeast != nil || g.AtMost != nil ||
		g.Start == nil || !g.Start.Equal(dayAt("2026-10-27", 0)) || g.End == nil || !g.End.Equal(dayAt("2026-10-29", 0)) || !strings.HasSuffix(g.Occurrence, "@2026-10-27T00:00Z") {
		t.Errorf("the autumn sale's: %+v", g)
	}
	// Each occurrence of a repeating window is a period of its own.
	n := 0
	for _, r := range sum.Rows {
		for _, g := range r.Goals {
			if r.Team == "" && g.Name == "sale-firings" && g.Title == "Flash sale" {
				n++
			}
		}
	}
	if n != 3 {
		t.Errorf("the flash sale's occurrences: %d, want 3", n)
	}
	// In JSON: from and to, and the four weeks before's value.
	_, body, _ := get(t, s.Handler(), "/api/v1/summary?from=2026-10-26&to=2026-10-26")
	var compact bytes.Buffer
	json.Compact(&compact, []byte(body))
	body = compact.String()
	if !strings.Contains(body, `"name":"sale-no-worse","measure":"precision","value":0.5,"result":"met","each":"sale","no_worse_than":"before","by":0.05,"before":0,"occurrence":"`) ||
		!strings.Contains(body, `"title":"Autumn sale","from":"2026-10-27T00:00:00Z","to":"2026-10-29T00:00:00Z"}`) {
		t.Errorf("the summary's JSON: %s", body)
	}
}

// Weekly goals beside goals over a period read as they do alone: every
// row's goals of each week, and every other field of the summary, the
// same as with the weekly goal declared alone.
func TestGoalsPeriodLeaveWeeksAlone(t *testing.T) {
	s := periodServer(t, nil)
	_, both, _ := get(t, s.Handler(), "/api/v1/summary?from=2026-10-05&to=2026-11-02&team=*")
	declare(t, s, "goals:\n  - {name: none-missed, measure: incidents.missed, at_most: 0}\n")
	_, alone, _ := get(t, s.Handler(), "/api/v1/summary?from=2026-10-05&to=2026-11-02&team=*")
	var a, b map[string]any
	json.Unmarshal([]byte(both), &a)
	json.Unmarshal([]byte(alone), &b)
	periods := 0
	for _, r := range a["rows"].([]any) {
		row := r.(map[string]any)
		gs, _ := row["goals"].([]any)
		var weekly []any
		for _, g := range gs {
			if _, ok := g.(map[string]any)["each"]; ok {
				periods++
				continue
			}
			weekly = append(weekly, g)
		}
		if weekly == nil {
			delete(row, "goals")
		} else {
			row["goals"] = weekly
		}
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if periods == 0 || string(ja) != string(jb) {
		t.Errorf("goals over a period changed the rest of the summary (%d of them):\n%s\n%s", periods, ja, jb)
	}
}

// no_worse_than, for each measure that takes it: higher is better for
// precision and the median lead, so inside is no worse than before when it
// is at least before less by; and at_least and at_most hold inside alone.
func TestGoalPeriodNoWorseDirection(t *testing.T) {
	m := ledger.Marked{ID: 1, Kind: "sale", Title: "Sale"}
	o := markedOcc{ledger.Occurrence{Start: dayAt("2026-10-27", 0), End: dayAt("2026-10-29", 0)}, &m}
	when := occWhen{whole: true}
	share := func(caught, judged int) markedCount {
		return markedCount{Firings: judged, Caught: caught, judged: judged}
	}
	lead := func(seconds ...int) markedCount {
		var c markedCount
		for _, x := range seconds {
			c.Incidents.Total++
			c.Incidents.Caught++
			c.Incidents.leads = append(c.Incidents.leads, time.Duration(x)*time.Second)
		}
		return c
	}
	for _, c := range []struct {
		goal       ledger.Goal
		in, before markedCount
		want       string
	}{
		{ledger.Goal{Measure: "precision", NoWorseThan: "before"}, share(9, 10), share(8, 10), goalMet},
		{ledger.Goal{Measure: "precision", NoWorseThan: "before"}, share(8, 10), share(8, 10), goalMet},
		{ledger.Goal{Measure: "precision", NoWorseThan: "before"}, share(7, 10), share(8, 10), goalMissed},
		{ledger.Goal{Measure: "precision", NoWorseThan: "before", By: "0.1"}, share(7, 10), share(8, 10), goalMet},
		{ledger.Goal{Measure: "precision", NoWorseThan: "before", By: "0.05"}, share(15, 20), share(8, 10), goalMet},
		{ledger.Goal{Measure: "precision", NoWorseThan: "before", By: "0.05"}, share(7, 10), share(8, 10), goalMissed},
		{ledger.Goal{Measure: "incidents.median_lead_seconds", NoWorseThan: "before"}, lead(300), lead(120), goalMet},
		{ledger.Goal{Measure: "incidents.median_lead_seconds", NoWorseThan: "before"}, lead(60), lead(120), goalMissed},
		{ledger.Goal{Measure: "incidents.median_lead_seconds", NoWorseThan: "before", By: "60"}, lead(60), lead(120), goalMet},
		{ledger.Goal{Measure: "incidents.median_lead_seconds", NoWorseThan: "before", By: "59"}, lead(60), lead(120), goalMissed},
		{ledger.Goal{Measure: "incidents.median_lead_seconds", NoWorseThan: "before"}, lead(-60), lead(-120), goalMet},
		// Inside alone: the weeks before don't count.
		{ledger.Goal{Measure: "precision", AtLeast: "0.8"}, share(8, 10), share(1, 10), goalMet},
		{ledger.Goal{Measure: "precision", AtLeast: "0.8"}, share(7, 10), share(10, 10), goalMissed},
		{ledger.Goal{Measure: "incidents.median_lead_seconds", AtLeast: "0"}, lead(-30), lead(600), goalMissed},
	} {
		g := c.goal
		g.Name, g.Each = "g", "sale"
		if err := ledger.CheckGoal(&g); err != nil {
			t.Fatal(err)
		}
		if got := periodOn(g, false, o, c.in, c.before, when); got.Result != c.want {
			t.Errorf("%s %s by %q: inside %v, before %v: %s, want %s", g.Measure, g.NoWorseThan+g.AtLeast, g.By, got.Value, got.Before, got.Result, c.want)
		}
	}
}

// The four whole weeks before an occurrence's first week, outside its
// kind: not its own week, not a fifth week back, and not three; of the
// rest of its kind's firings and incidents, none.
func TestGoalPeriodBeforeWindow(t *testing.T) {
	s := periodServer(t, nil)
	ctx := context.Background()
	sum := summaryOfGoals(t, s)
	// The launch: its own week's other firings (Paged's two, both caught
	// something) aren't before it; the flash sale's, of another kind, are.
	if g := periodIn(t, sum, "2026-10-26", "", "launch-no-worse", "Search, new ranking"); g.Before == nil || *g.Before != 0.2 {
		t.Errorf("the launch's weeks before: %+v", g)
	}
	// The autumn sale: the flash sale's firing that caught INC-A is of its
	// kind, and INC-A during it, so neither is before it.
	if g := periodIn(t, sum, "2026-10-26", "", "sale-no-worse", "Autumn sale"); g.Before == nil || *g.Before != 0 {
		t.Errorf("the autumn sale's weeks before: %+v", g)
	}
	// A promotion three weeks on, in the week of 11-23, with no record:
	// the four weeks before are those of 10-26 (two of four caught
	// something), 11-02 (none of forty-one), 11-09 and 11-16 (none fired);
	// 10-19's, a fifth week back (one of five), aren't. Said beside it,
	// though it isn't known.
	later := now.Add(21 * 24 * time.Hour)
	s.now = func() time.Time { return later }
	if err := s.l.Record(ctx, ledger.Found{From: now, To: later, Rules: []ledger.Rule{{Name: "Paged", Severity: "critical"}},
		Run: ledger.Run{Kind: "hourly", Started: later, Finished: later}}, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutMarked(ctx, ledger.Marked{Kind: "promo", Title: "Promotion", Start: dayAt("2026-11-23", 0), End: dayAt("2026-11-23", 6)}, "ana", s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	declare(t, s, "goals:\n  - {name: promo-no-worse, each: promo, measure: precision, no_worse_than: before}\n")
	_, body, _ := get(t, s.Handler(), "/api/v1/summary?from=2026-11-23&to=2026-11-23")
	var later1 summary
	json.Unmarshal([]byte(body), &later1)
	if g := periodIn(t, later1, "2026-11-23", "", "promo-no-worse", "Promotion"); g.Before == nil || *g.Before != 2.0/45 || g.Result != goalUnknown || g.Why != whyNoRecord {
		t.Errorf("the promotion's weeks before: %+v", g)
	}
}

// After a run, the leader works out each goal over a period's latest
// occurrence that ended, for /metrics; reads its occurrences' days and
// the four weeks before the first's once; tells goal.missed once for each
// goal, team and occurrence that ended since the last whole week began;
// with --goal-tasks, makes a task of each missed, said on as later
// occurrences are missed again, met, and no longer missed, and not made
// again for an occurrence it was made for once it is done.
func TestGoalsPeriodAfterRun(t *testing.T) {
	hook := filepath.Join(t.TempDir(), "hook")
	os.WriteFile(hook, []byte("https://hooks.example/x\n"), 0o600)
	s := periodServer(t, func(c *Config) { c.NotifyWebhook, c.GoalTasks = hook, true })
	ctx := context.Background()
	var reads [][2]int64
	periodRead = func(d1, d2 int64) { reads = append(reads, [2]int64{d1, d2}) }
	err := s.goalsAfter(ctx)
	periodRead = nil
	if err != nil {
		t.Fatal(err)
	}
	// Once, from the flash sale's first day to this week's sale's last.
	if len(reads) != 1 || reads[0] != [2]int64{s.l.Day(dayAt("2026-10-07", 10)), s.l.Day(dayAt("2026-11-05", 0))} {
		t.Errorf("read %v", reads)
	}
	shown := s.goalsShownNow()
	byGoal := map[string]goalShown{}
	for _, l := range shown.Lines {
		byGoal[l.Goal+"/"+l.Team] = l
	}
	for k, want := range map[string]goalShown{
		"sale-none-missed/": {Result: "missed", Met: 2}, "launch-no-worse/": {Result: "missed"}, "sale-no-worse/": {Result: "met", Met: 1},
		"sale-each-missed/payments": {Result: "met", Met: 3}, "sale-each-missed/search": {Result: "unknown"}, "none-missed/": {Result: "missed", Met: 1},
	} {
		if got := byGoal[k]; got.Result != want.Result || got.Met != want.Met {
			t.Errorf("%s: %+v, want %+v", k, got, want)
		}
	}
	_, metrics, _ := get(t, s.Handler(), "/metrics")
	for _, want := range []string{`lapilli_goal_met{goal="sale-none-missed"} 0`, `lapilli_goal_met{goal="sale-no-worse"} 1`, `lapilli_goal_value{goal="sale-no-worse"} 0.5`,
		`lapilli_goal_known{goal="sale-lead"} 0`, `lapilli_goal_weeks_met{goal="sale-each-missed",team="payments"} 3`, `lapilli_goal_value{goal="launch-no-worse"} 0`} {
		if !strings.Contains(metrics, want+"\n") {
			t.Errorf("/metrics has no %s", want)
		}
	}
	// Told: the autumn sale's and the launch's, ended since 10-26; not the
	// flash sale's, which ended before.
	told := func() []string {
		due, err := s.l.Due(ctx, "webhook", now.Add(30*24*time.Hour), 500)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, n := range due {
			if n.Event != "goal.missed" {
				continue
			}
			var x struct {
				Goal goalNotice `json:"goal"`
			}
			json.Unmarshal(n.Body, &x)
			if x.Goal.Each != "" {
				out = append(out, x.Goal.Name+"/"+x.Goal.Title+"/"+x.Goal.From.Format("2006-01-02"))
			}
		}
		slices.Sort(out)
		return out
	}
	if got := told(); !slices.Equal(got, []string{"launch-no-worse/Search, new ranking/2026-10-28", "sale-none-missed/Autumn sale/2026-10-27"}) {
		t.Errorf("told: %v", got)
	}
	tasks := func(goal string) []ledger.Task {
		ts, err := s.l.Tasks(ctx, ledger.TaskQuery{Goal: goal})
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}
	ts := tasks("sale-none-missed")
	if len(ts) != 1 || ts[0].Title != "Goal sale-none-missed missed during sale Autumn sale, 2026-10-27" ||
		!strings.Contains(ts[0].Body, "Goal sale-none-missed missed during sale Autumn sale, 2026-10-27 00:00 → 2026-10-29 00:00: incidents missed was 1; the goal is at most 0.") {
		t.Fatalf("made: %+v", ts)
	}
	if ts := tasks("launch-no-worse"); len(ts) != 1 || !strings.Contains(ts[0].Body, "precision was 0%; the goal is no worse than the four weeks before, 20%, by 5%.") {
		t.Errorf("made: %+v", ts)
	}
	task := ts[0].ID
	// Again: nothing told again, no second task.
	again(t, s)
	if got := told(); len(got) != 2 || len(tasks("sale-none-missed")) != 1 {
		t.Errorf("again: told %v", got)
	}
	// A week on, a cart incident during this week's sale, missed: said on
	// the open task, and told; no second task.
	s.now = func() time.Time { return now.Add(7 * 24 * time.Hour) }
	if _, err := s.l.SyncFile(ctx, append(slices.Clone(goalIncidents),
		incident.Incident{ID: "INC-D", Start: dayAt("2026-11-04", 5), End: dayAt("2026-11-04", 6), Labels: incident.Labels{"service": {"cart"}}}), s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	if err := s.l.Record(ctx, ledger.Found{From: now, To: now.Add(7 * 24 * time.Hour), Rules: []ledger.Rule{{Name: "Paged", Severity: "critical"}},
		Run: ledger.Run{Kind: "hourly", Started: now, Finished: now}}, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	again(t, s)
	if got := commentsOn(t, s, task); !slices.Equal(got, []string{"Missed again, sale Week sale, 2026-11-02 00:00 → 2026-11-05 00:00: 1."}) {
		t.Errorf("said: %q", got)
	}
	if got := told(); !slices.Contains(got, "sale-none-missed/Week sale/2026-11-02") || len(tasks("sale-none-missed")) != 1 {
		t.Errorf("a week on: told %v", got)
	}
	// INC-C taken out: the autumn sale is no longer missed. Said once; the
	// task stays open.
	if _, err := s.l.SyncFile(ctx, []incident.Incident{goalIncidents[0], goalIncidents[1],
		{ID: "INC-D", Start: dayAt("2026-11-04", 5), End: dayAt("2026-11-04", 6), Labels: incident.Labels{"service": {"cart"}}}}, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	again(t, s)
	again(t, s)
	if got := commentsOn(t, s, task); !slices.Equal(got, []string{"Missed again, sale Week sale, 2026-11-02 00:00 → 2026-11-05 00:00: 1.",
		"No longer missed, sale Autumn sale, 2026-10-27 00:00 → 2026-10-29 00:00: 0."}) {
		t.Errorf("said: %q", got)
	}
	if x, _ := s.l.TaskByID(ctx, task); x.Status != "open" {
		t.Errorf("closed for someone: %+v", x)
	}
	// Done: the occurrences said missed on it aren't made a task again.
	if _, err := s.l.PatchTask(ctx, task, []byte(`{"status": "done"}`), "ana"); err != nil {
		t.Fatal(err)
	}
	again(t, s)
	if ts := tasks("sale-none-missed"); len(ts) != 1 {
		t.Errorf("made again for an occurrence a task was made for: %+v", ts)
	}
	// What is kept of them is of their own, an occurrence at a time: no
	// week's mark, and the weekly goal's none of an occurrence's.
	toldW, madeW, _ := s.l.GoalMarks(ctx)
	toldP, madeP, _ := s.l.PeriodGoalMarks(ctx)
	autumnKey := ledger.GoalOccKey("sale-none-missed", 0, heldOf(t, s, "sale-none-missed", "Autumn sale"))
	weekKey := ledger.GoalOccKey("sale-none-missed", 0, heldOf(t, s, "sale-none-missed", "Week sale"))
	if _, ok := toldW["sale-none-missed.0"]; ok || madeW["sale-none-missed.0"] != 0 || toldP[autumnKey] != dayAt("2026-10-29", 0).Unix() ||
		toldP[weekKey] != dayAt("2026-11-05", 0).Unix() || madeP[autumnKey] != dayAt("2026-10-29", 0).Unix() || madeP[weekKey] != dayAt("2026-11-05", 0).Unix() {
		t.Errorf("marks: %v %v %v %v", toldW, madeW, toldP, madeP)
	}
	for k := range toldP {
		if strings.HasPrefix(k, "none-missed.") {
			t.Errorf("a weekly goal's mark among the occurrences': %s", k)
		}
	}
	// Slack's line of an occurrence's notice.
	n := notice{Goal: &goalNotice{Name: "launch-no-worse", Measure: "precision", Each: "launch", Title: "Search, new ranking", From: dayAt("2026-10-28", 12),
		To: dayAt("2026-10-28", 12).Add(45 * time.Minute), Value: ptr(0.0), Before: ptr(0.2), NoWorseThan: "before", By: ptr(0.05)}}
	if got := s.line(n); got != "lapilli: Goal launch-no-worse missed during launch Search, new ranking, 2026-10-28 12:00 → 2026-10-28 12:45: precision was 0%; the goal is no worse than the four weeks before, 20%, by 5%." {
		t.Errorf("Slack: %q", got)
	}
	n.Goal.Team, n.Goal.Teams, n.Goal.Value, n.Goal.Before = "*", []goalNoticeTeam{{"payments", ptr(0.1)}}, nil, nil
	if got := s.line(n); got != "lapilli: Goal launch-no-worse missed during launch Search, new ranking, 2026-10-28 12:00 → 2026-10-28 12:45 by 1 teams: payments (10%); the goal is no worse than before, by 5%." {
		t.Errorf("Slack, of each team's: %q", got)
	}
}

func ptr(v float64) *float64 { return &v }

// heldOf is what is kept of everyone's goal over a period of an occurrence
// is keyed by, of the window titled title, as the leader works it out.
func heldOf(t *testing.T, s *Server, goal, title string) string {
	t.Helper()
	this := ledger.Week(s.l.Day(s.now()))
	lines, err := s.goalTable(context.Background(), this-12*7, this, []*ledger.Team{nil})
	if err != nil {
		t.Fatal(err)
	}
	for _, gl := range lines {
		for _, w := range gl.Occs {
			if gl.Goal.Name == goal && w.Title == title && w.held != "" {
				return w.held
			}
		}
	}
	t.Fatalf("no occurrence of %s titled %s", goal, title)
	return ""
}

// /metrics gives maxGoalSeries lines of each family at most, those missed
// first, goals over a period among them.
func TestGoalsPeriodMetricsBound(t *testing.T) {
	s := periodServer(t, nil)
	was := maxGoalSeries
	maxGoalSeries = 3
	defer func() { maxGoalSeries = was }()
	if err := s.goalsAfter(context.Background()); err != nil {
		t.Fatal(err)
	}
	shown := s.goalsShownNow()
	if len(shown.Lines) != 3 || shown.More != 5 {
		t.Fatalf("lines %d, more %d: %+v", len(shown.Lines), shown.More, shown.Lines)
	}
	var missed []string
	for _, l := range shown.Lines {
		if l.Result != goalMissed {
			t.Errorf("not missed first: %+v", shown.Lines)
		}
		missed = append(missed, l.Goal)
	}
	if !slices.Equal(missed, []string{"launch-no-worse", "none-missed", "sale-none-missed"}) {
		t.Errorf("missed: %v", missed)
	}
	_, metrics, _ := get(t, s.Handler(), "/metrics")
	if n := strings.Count(metrics, "\nlapilli_goal_known{"); n != 3 {
		t.Errorf("%d series of lapilli_goal_known", n)
	}
}

// The Goals page shows a goal over a period occurrence by occurrence, the
// overview's card counts those that ended since the last whole week began,
// and What to look at makes the task of one missed with one press, of its
// occurrence: pressed again, the task open; an occurrence not missed, a
// weekly goal's and a key that isn't one refused. MCP's overview says them.
func TestGoalsPeriodPages(t *testing.T) {
	s := periodServer(t, func(c *Config) {
		c.UserHeader, c.Admins, c.Editors = "X-Forwarded-Email", []string{"boss@example.com"}, []string{"ed@example.com"}
	})
	h := s.Handler()
	_, body, _ := get(t, h, "/goals")
	for _, want := range []string{`<h2 id="goal-periods">Occurrence by occurrence</h2>`, `<tr id="goal-sale-no-worse">`,
		"each sale: precision: no worse than before, by 5%",
		`title="sale Autumn sale, 2026-10-27 00:00 → 2026-10-29 00:00: met, 50%; the four weeks before 0%"`,
		`title="sale Flash sale, 2026-10-07 10:00 → 2026-10-07 10:30: not known: the ledger holds only part of the week it started in, –"`,
		`class="gw status-warn" title="sale Week sale, 2026-11-02 00:00 → 2026-11-05 00:00: over so far, 41"`,
		`title="sale Flash sale, 2026-10-21 10:00 → 2026-10-21 10:30: not known: nothing to set it beside in the four weeks before, –; the four weeks before –"`,
		`<th scope="col" colspan="6">Occurrences, the earliest first</th>`} {
		if !strings.Contains(body, want) {
			t.Errorf("/goals has no %s", want)
		}
	}
	if _, body, _ := get(t, h, "/goals?team=payments"); !strings.Contains(body, "<bdi>payments</bdi>") || strings.Contains(body, "<bdi>search</bdi>") ||
		!strings.Contains(body, `<tr id="goal-sale-each-missed">`) {
		t.Error("payments's Goals page")
	}
	// The overview's card: of everyone's, five ended since 10-26, two met.
	asEditor := func(path string) string {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-Forwarded-Email", "ed@example.com")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Body.String()
	}
	body = asEditor("/")
	if !strings.Contains(body, "2 of 5 occurrences met</strong>, 1 not known · of the goals over a period, those that ended since 2026-10-26") ||
		!strings.Contains(body, "Goal sale-none-missed missed during sale Autumn sale</bdi>") ||
		!strings.Contains(body, `<input type="hidden" name="occurrence" value="3@2026-10-27T00:00Z">`) {
		t.Fatalf("the overview: no card, or no line with its form")
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
	autumn := "3@2026-10-27T00:00Z"
	if code, out := as("ed@example.com", url.Values{"goal": {"sale-none-missed"}, "occurrence": {autumn}, "back": {"/"}}); code != http.StatusSeeOther {
		t.Fatalf("made: %d %s", code, out)
	}
	ts, _ := s.l.Tasks(context.Background(), ledger.TaskQuery{Goal: "sale-none-missed"})
	if len(ts) != 1 || ts[0].Title != "Goal sale-none-missed missed during sale Autumn sale, 2026-10-27" || ts[0].CreatedBy != "ed@example.com" {
		t.Fatalf("the task: %+v", ts)
	}
	if st, ok, _ := s.l.GoalTask(context.Background(), ts[0].ID); !ok || !st.Period || st.Week != dayAt("2026-10-29", 0).Unix() {
		t.Errorf("kept of it: %+v", st)
	}
	if code, out := as("ed@example.com", url.Values{"goal": {"sale-none-missed"}, "occurrence": {autumn}}); code != http.StatusSeeOther || !strings.Contains(out, fmt.Sprintf("/tasks/%d", ts[0].ID)) {
		t.Errorf("again: %d %s", code, out)
	}
	for _, c := range []struct {
		form url.Values
		says string
	}{
		{url.Values{"goal": {"sale-none-missed"}, "occurrence": {"1@2026-10-21T10:00Z"}}, "wasn&#39;t missed during 1@2026-10-21T10:00Z"},
		{url.Values{"goal": {"sale-none-missed"}, "occurrence": {"4@2026-10-28T12:00Z"}}, "want an occurrence of sale"},
		{url.Values{"goal": {"sale-none-missed"}, "occurrence": {"3@2026-10-27"}}, "want an occurrence of sale"},
		{url.Values{"goal": {"sale-none-missed"}, "week": {"2026-10-26"}}, "want an occurrence of sale"},
		{url.Values{"goal": {"none-missed"}, "occurrence": {autumn}}, "goal none-missed is of each week"},
	} {
		if code, out := as("ed@example.com", c.form); code < 400 || !strings.Contains(out, c.says) {
			t.Errorf("%v: %d %s", c.form, code, out)
		}
	}
	if body := asEditor("/"); !strings.Contains(body, fmt.Sprintf(`href="/tasks/%d">task #%d</a>`, ts[0].ID, ts[0].ID)) {
		t.Error("the line doesn't link to its task")
	}
	// MCP's overview: the card's occurrences.
	ts2 := httptest.NewServer(h)
	defer ts2.Close()
	cs := connect(t, ts2.URL, "viewer@example.com")
	var out struct {
		Goals goalsCardOut `json:"goals"`
	}
	if failed, text := call(t, cs, "overview", nil, &out); failed || out.Goals.OccurrencesOf != 5 || out.Goals.OccurrencesMet != 2 || out.Goals.OccurrencesUnknown != 1 ||
		len(out.Goals.Missed) != 3 {
		t.Errorf("MCP's overview: %+v %s", out.Goals, text)
	}
	// A team with goals over a period alone: the card says its occurrences.
	declare(t, s, "goals:\n  - {name: sale-each-missed, team: \"*\", each: sale, measure: incidents.missed, at_most: 0}\n")
	if body := asEditor("/?team=payments"); !strings.Contains(body, "1 of 1 occurrences met") || strings.Contains(body, "No goal was worked out for the week of") {
		i := strings.Index(body, "<h2>Goals</h2>")
		t.Errorf("payments's card, of goals over a period alone: %s", body[i:i+600])
	}
	// Every text in every language.
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	declare(t, s, thePeriodGoals)
	for _, l := range Langs() {
		for _, p := range []string{"/goals", "/goals?team=search", "/"} {
			r := httptest.NewRequest(http.MethodGet, p, nil)
			r.Header.Set("Accept-Language", l)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 200 {
				t.Errorf("%s %s: %d", l, p, w.Code)
			}
		}
	}
	missing.Lock()
	for k := range missing.m {
		t.Errorf("no translation: %q", k)
	}
	missing.Unlock()
}

// The digest's line and digest.ready's goals of the week of 10-26: its
// weekly goals and the occurrences that ended in it; and a report's goals
// section, over the occurrences that ended in its span, whole weeks or
// not.
func TestGoalsPeriodDigestAndReports(t *testing.T) {
	s := periodServer(t, nil)
	ctx := context.Background()
	from, to := dayAt("2026-10-26", 0), dayAt("2026-11-02", 0)
	text, err := s.digestText(ctx, &verdict.Report{From: from, To: to}, nil, from, to, nil)
	if err != nil || !strings.Contains(text, "Goals: 2 of 6 met, 3 missed, 1 not known.") ||
		!strings.Contains(text, "Goal sale-none-missed missed during sale Autumn sale, 2026-10-27 00:00 → 2026-10-29 00:00: incidents missed was 1; the goal is at most 0.") {
		t.Errorf("the digest: %q %v", text, err)
	}
	about := map[string]any{}
	if err := s.aboutGoals(ctx, about, from, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(about["goals"])
	if !strings.Contains(string(b), `"title":"Autumn sale","from":"2026-10-27T00:00:00Z","to":"2026-10-29T00:00:00Z"`) {
		t.Errorf("digest.ready's goals: %s", b)
	}
	// From Wednesday 10-21 to Wednesday 10-28: the flash sale of 10-21 and
	// the weekend sale end in it, the autumn sale doesn't.
	line, nums, err := s.goalsSection(ctx, dayAt("2026-10-21", 0), dayAt("2026-10-28", 0), nil, time.UTC)
	if err != nil || !strings.Contains(line, "Goal sale-none-missed (incidents missed, at most 0), each sale: met in 2 of 2 occurrences known that ended in the span, of 2.") ||
		!strings.Contains(line, "Goals: the span holds no whole week to hold them to.") {
		t.Errorf("the section: %q %v", line, err)
	}
	line, nums, err = s.goalsSection(ctx, dayAt("2026-10-19", 0), dayAt("2026-11-02", 0), nil, time.UTC)
	if err != nil || !strings.Contains(line, "Goal sale-none-missed (incidents missed, at most 0), each sale: met in 2 of 3 occurrences known that ended in the span, of 3. Missed during sale Autumn sale, 2026-10-27 00:00 → 2026-10-29 00:00.") ||
		!strings.Contains(line, "Goal none-missed (incidents missed, at most 0): met in 1 of 2 whole weeks known, of 2.") {
		t.Errorf("the section: %q %v", line, err)
	}
	b, _ = json.Marshal(nums)
	if !strings.Contains(string(b), `"name":"sale-none-missed","each":"sale","met":2,"of":3,"weeks":[],"occurrences":[`) {
		t.Errorf("its numbers: %s", b)
	}
}

// goals: in --config takes a goal over a period, and the export gives it
// back as declared.
func TestParseConfigPeriodGoals(t *testing.T) {
	s, _ := configServer(t, "teams:\n  - {name: payments, rules: ^Errors$}\n"+thePeriodGoals, nil)
	gs, err := s.l.Goals(context.Background())
	if err != nil || len(gs) != 7 {
		t.Fatalf("kept: %+v %v", gs, err)
	}
	r := httptest.NewRequest(http.MethodGet, "/config.yaml", nil)
	r.Header.Set("X-Forwarded-Email", "boss@example.com")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	back, err := ParseConfig(w.Body.Bytes(), time.UTC)
	if err != nil {
		t.Fatalf("the export: %v\n%s", err, w.Body.String())
	}
	i := slices.IndexFunc(back.Goals, func(g ledger.DeclaredGoal) bool { return g.Name == "sale-no-worse" })
	if i < 0 || back.Goals[i].Each != "sale" || back.Goals[i].NoWorseThan != "before" || back.Goals[i].By != "0.05" {
		t.Errorf("exported: %+v\n%s", back.Goals, w.Body.String())
	}
}

// The Goals page shows a goal over a period's latest goalOccsShown
// occurrences, and says how many earlier ones it leaves out: a daily
// stand-up over three weeks is twenty-one of them.
func TestGoalsPeriodPageBounded(t *testing.T) {
	s := goalServer(t, nil)
	ctx := context.Background()
	if _, err := s.l.PutMarked(ctx, ledger.Marked{Kind: "standup", Title: "Stand-up", Start: dayAt("2026-10-12", 9), End: dayAt("2026-10-12", 9).Add(15 * time.Minute),
		Every: "day", Until: dayAt("2026-11-01", 0)}, "ana", s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	if err := s.l.JudgePending(ctx, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	declare(t, s, "goals:\n  - {name: standup-quiet, each: standup, measure: firings, at_most: 0}\n")
	_, body, _ := get(t, s.Handler(), "/goals")
	if n := strings.Count(body, `title="standup Stand-up, `); n != goalOccsShown || !strings.Contains(body, "8 earlier occurrences left out") ||
		!strings.Contains(body, `title="standup Stand-up, 2026-11-01 09:00 → 2026-11-01 09:15: met, 0"`) || strings.Contains(body, `title="standup Stand-up, 2026-10-19 09:00`) ||
		strings.Contains(body, `<h2 id="goal-weeks">`) {
		t.Errorf("%d cells, or no word of those left out:\n%s", n, body)
	}
}

// An occurrence's goal reads alike whatever span of weeks it is read in:
// the four weeks before it leave out the incidents during any occurrence
// of its kind, those of windows the read's own weeks don't meet among
// them. A sale's median lead beside an earlier sale's, read over thirteen
// weeks (the Goals page, /metrics, the leader) and over its own week (the
// digest, a report, Make it a task), and the Marked windows page of the
// sale alone, which lists one window.
func TestGoalsPeriodBeforeAnySpan(t *testing.T) {
	s, _ := server(t, func(c *Config) { c.Incidents, c.Impact = "", "" })
	ctx := context.Background()
	o := s.cfg.Options
	fire := func(at time.Time) firing.Firing {
		return firing.Firing{Rule: "Sharp", Labels: map[string]string{"service": "checkout"}, Start: at, End: at.Add(10 * time.Minute)}
	}
	if err := s.l.Record(ctx, ledger.Found{From: dayAt("2026-10-05", 0), To: now.Add(-time.Hour),
		Firings: []firing.Firing{fire(dayAt("2026-10-06", 3)), fire(dayAt("2026-10-07", 10).Add(-5 * time.Minute)), fire(dayAt("2026-10-21", 10).Add(-time.Minute))},
		Rules:   []ledger.Rule{{Name: "Sharp", Severity: "critical"}}, Run: ledger.Run{Kind: "backfill", Started: now, Finished: now}}, o); err != nil {
		t.Fatal(err)
	}
	// INC-1 during the early sale, caught five minutes ahead; INC-2 during
	// the main sale, a minute ahead.
	if _, err := s.l.SyncFile(ctx, []incident.Incident{
		{ID: "INC-1", Start: dayAt("2026-10-07", 10), End: dayAt("2026-10-07", 11), Labels: incident.Labels{"service": {"checkout"}}},
		{ID: "INC-2", Start: dayAt("2026-10-21", 10), End: dayAt("2026-10-21", 11), Labels: incident.Labels{"service": {"checkout"}}},
	}, o); err != nil {
		t.Fatal(err)
	}
	var main ledger.Marked
	for _, m := range []ledger.Marked{
		{Kind: "sale", Title: "Early sale", Start: dayAt("2026-10-07", 9), End: dayAt("2026-10-07", 11)},
		{Kind: "sale", Title: "Main sale", Start: dayAt("2026-10-21", 9), End: dayAt("2026-10-21", 11)},
	} {
		id, err := s.l.PutMarked(ctx, m, "ana", o)
		if err != nil {
			t.Fatal(err)
		}
		main, main.ID = m, id
	}
	if err := s.l.JudgePending(ctx, o); err != nil {
		t.Fatal(err)
	}
	declare(t, s, "goals:\n  - {name: sale-lead, each: sale, measure: incidents.median_lead_seconds, no_worse_than: before}\n")
	read := func(from, to string) summaryGoal {
		_, body, _ := get(t, s.Handler(), "/api/v1/summary?from="+from+"&to="+to)
		var sum summary
		if err := json.Unmarshal([]byte(body), &sum); err != nil {
			t.Fatal(err)
		}
		return periodIn(t, sum, "2026-10-19", "", "sale-lead", "Main sale")
	}
	for _, g := range []summaryGoal{read("2026-08-10", "2026-11-02"), read("2026-10-19", "2026-10-19")} {
		if g.Result != goalUnknown || g.Why != whyNoBefore || g.Before != nil {
			t.Errorf("the main sale's lead, beside weeks whose one incident was during a sale: %+v", g)
		}
	}
	if line, _, err := s.goalsLine(ctx, dayAt("2026-10-19", 0), nil); err != nil || line != "Goals: 0 of 1 met, 0 missed, 1 not known." {
		t.Errorf("the digest of the sale's week: %q %v", line, err)
	}
	// The Marked windows page of the main sale alone, and of the weeks
	// that don't reach back to the early sale.
	m, err := s.l.MarkedByID(ctx, main.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ms := range [][]ledger.Marked{{m}} {
		lines, _, err := s.markedLines(ctx, ms, dayAt("2026-10-20", 0), dayAt("2026-10-22", 0), nil, "en")
		if err != nil || len(lines) != 1 || lines[0].Before.Incidents.Total != 0 || lines[0].Inside.Incidents.Total != 1 {
			t.Errorf("the main sale's page: %+v %v", lines, err)
		}
	}
	_, body, _ := get(t, s.Handler(), fmt.Sprintf("/marked/%d", main.ID))
	if !strings.Contains(body, "Main sale") {
		t.Error("the main sale's page")
	}
}

// An occurrence met when the leader first finds it ended, and missed once
// an incident during it is written down a day later, is told then, once,
// though a later occurrence was told of already; and said on the goal's
// open task, missed again.
func TestGoalsPeriodToldWhenMissedLate(t *testing.T) {
	hook := filepath.Join(t.TempDir(), "hook")
	os.WriteFile(hook, []byte("https://hooks.example/x\n"), 0o600)
	s := periodServer(t, func(c *Config) { c.NotifyWebhook, c.GoalTasks = hook, true })
	ctx := context.Background()
	if _, err := s.l.PutMarked(ctx, ledger.Marked{Kind: "sale", Title: "Monday sale", Start: dayAt("2026-10-26", 9), End: dayAt("2026-10-26", 10)}, "ana", s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	if err := s.l.JudgePending(ctx, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	told := func() []string {
		var out []string
		for _, n := range goalNotices(t, s) {
			if n.Name == "sale-none-missed" {
				out = append(out, n.Title)
			}
		}
		slices.Sort(out)
		return out
	}
	again(t, s)
	if got := told(); !slices.Equal(got, []string{"Autumn sale"}) {
		t.Fatalf("first: %v", got)
	}
	// INC-M, on cart, during the Monday sale, written down a day later.
	if _, err := s.l.SyncFile(ctx, append(slices.Clone(goalIncidents),
		incident.Incident{ID: "INC-M", Start: dayAt("2026-10-26", 9).Add(30 * time.Minute), End: dayAt("2026-10-26", 10), Labels: incident.Labels{"service": {"cart"}}}), s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	again(t, s)
	again(t, s)
	if got := told(); !slices.Equal(got, []string{"Autumn sale", "Monday sale"}) {
		t.Errorf("told: %v", got)
	}
	ts, _ := s.l.Tasks(ctx, ledger.TaskQuery{Goal: "sale-none-missed"})
	if len(ts) != 1 || !slices.Equal(commentsOn(t, s, ts[0].ID), []string{"Missed again, sale Monday sale, 2026-10-26 09:00 → 2026-10-26 10:00: 1."}) {
		t.Errorf("said: %+v %q", ts, commentsOn(t, s, ts[0].ID))
	}
}

// Two occurrences of a kind ending at the same moment are two, told and
// said on apart: the autumn sale, missed, which the task is made for; a
// cart sale with it, missed too, told and said missed again; and a search
// sale with them, met, of which nothing is said that isn't so.
func TestGoalsPeriodSameEnd(t *testing.T) {
	hook := filepath.Join(t.TempDir(), "hook")
	os.WriteFile(hook, []byte("https://hooks.example/x\n"), 0o600)
	s := periodServer(t, func(c *Config) { c.NotifyWebhook, c.GoalTasks = hook, true })
	ctx := context.Background()
	for _, m := range []ledger.Marked{
		{Kind: "sale", Title: "Search sale", Start: dayAt("2026-10-28", 0), End: dayAt("2026-10-29", 0), Labels: map[string]string{"service": "search"}},
		{Kind: "sale", Title: "Cart sale", Start: dayAt("2026-10-28", 0), End: dayAt("2026-10-29", 0), Labels: map[string]string{"service": "cart"}},
	} {
		if _, err := s.l.PutMarked(ctx, m, "ana", s.cfg.Options); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.l.JudgePending(ctx, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	sum := summaryOfGoals(t, s)
	for title, want := range map[string]string{"Autumn sale": goalMissed, "Cart sale": goalMissed, "Search sale": goalMet} {
		if g := periodIn(t, sum, "2026-10-26", "", "sale-none-missed", title); g.Result != want || !g.End.Equal(dayAt("2026-10-29", 0)) {
			t.Errorf("%s: %+v", title, g)
		}
	}
	again(t, s)
	again(t, s)
	var told []string
	for _, n := range goalNotices(t, s) {
		if n.Name == "sale-none-missed" {
			told = append(told, n.Title)
		}
	}
	slices.Sort(told)
	if !slices.Equal(told, []string{"Autumn sale", "Cart sale"}) {
		t.Errorf("told: %v", told)
	}
	ts, _ := s.l.Tasks(ctx, ledger.TaskQuery{Goal: "sale-none-missed"})
	if len(ts) != 1 || ts[0].Title != "Goal sale-none-missed missed during sale Autumn sale, 2026-10-27" ||
		!slices.Equal(commentsOn(t, s, ts[0].ID), []string{"Missed again, sale Cart sale, 2026-10-28 00:00 → 2026-10-29 00:00: 1."}) {
		t.Errorf("the task: %+v, said %q", ts, commentsOn(t, s, ts[0].ID))
	}
}

// goal.missed of goals over a period is told goalToldARun times a run at
// most, the earliest ended first, the rest by the runs after; and what is
// kept of what was told, a key an occurrence, is let go 13 weeks after it
// ended, so that over many occurrences there are as many keys as the
// occurrences told of in 13 weeks.
func TestGoalsPeriodToldBounded(t *testing.T) {
	hook := filepath.Join(t.TempDir(), "hook")
	os.WriteFile(hook, []byte("https://hooks.example/x\n"), 0o600)
	s := goalServer(t, func(c *Config) { c.NotifyWebhook = hook })
	ctx := context.Background()
	// A stand-up each day from 10-12, over no firing: each missed.
	if _, err := s.l.PutMarked(ctx, ledger.Marked{Kind: "standup", Title: "Stand-up", Start: dayAt("2026-10-12", 9), End: dayAt("2026-10-12", 9).Add(15 * time.Minute),
		Every: "day"}, "ana", s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	if err := s.l.JudgePending(ctx, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	declare(t, s, "goals:\n  - {name: standup-busy, each: standup, measure: firings, at_least: 1}\n")
	was := goalToldARun
	goalToldARun = 3
	defer func() { goalToldARun = was }()
	count := func() int {
		n := 0
		for _, x := range goalNotices(t, s) {
			if x.Name == "standup-busy" {
				n++
			}
		}
		return n
	}
	// Ended since 10-26: the stand-ups of 10-26 to 11-03, but 11-02's,
	// over one of Paged's firings: eight.
	ends := func() []string {
		var out []string
		for _, x := range goalNotices(t, s) {
			out = append(out, x.To.Format("01-02"))
		}
		slices.Sort(out)
		return out
	}
	for i, want := range []int{3, 6, 8, 8} {
		again(t, s)
		if n := count(); n != want {
			t.Fatalf("round %d: told %d, want %d", i+1, n, want)
		}
		if got := ends(); i == 0 && !slices.Equal(got, []string{"10-26", "10-27", "10-28"}) {
			t.Errorf("the first round told %v, not the earliest", got)
		}
	}
	if got := ends(); len(got) != 8 || got[0] != "10-26" || got[7] != "11-03" || slices.Contains(got, "11-02") {
		t.Errorf("told: %v", got)
	}
	// Fifteen weeks on, a run a week: a key for each told, none older than
	// thirteen weeks.
	goalToldARun = 20
	at := now
	for range 15 {
		next := at.Add(7 * 24 * time.Hour)
		s.now = func() time.Time { return next }
		if err := s.l.Record(ctx, ledger.Found{From: at, To: next, Rules: []ledger.Rule{{Name: "Paged", Severity: "critical"}},
			Run: ledger.Run{Kind: "hourly", Started: next, Finished: next}}, s.cfg.Options); err != nil {
			t.Fatal(err)
		}
		at = next
		again(t, s)
	}
	told, _, err := s.l.PeriodGoalMarks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	oldest := s.l.Midnight(ledger.Week(s.l.Day(at)) - 13*7).Unix()
	for k, end := range told {
		if end < oldest {
			t.Errorf("%s ended %s, kept past 13 weeks", k, time.Unix(end, 0).UTC())
		}
	}
	if n := count(); n < 15*7 || len(told) > 14*7 {
		t.Errorf("told %d, %d kept", n, len(told))
	}
}

// With no team picked, a goal of each team's over a period shows its
// teams missed first, then those not known, a team none of whose
// occurrences ended among them, and met last.
func TestGoalsPeriodPageSort(t *testing.T) {
	s := periodServer(t, nil)
	ctx := context.Background()
	id, err := s.l.PutTeam(ctx, ledger.Team{Name: "web-cart", Rules: "^Cart$", Labels: incident.Labels{"service": {"cart"}}}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	// A promotion of web-cart's alone, during INC-C, on cart, missed.
	if _, err := s.l.PutMarked(ctx, ledger.Marked{Kind: "promo", Title: "Cart promotion", Start: dayAt("2026-10-28", 9), End: dayAt("2026-10-28", 11), TeamID: id}, "ana", s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	if err := s.l.JudgePending(ctx, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	declare(t, s, "goals:\n  - {name: promo-missed, team: \"*\", each: promo, measure: incidents.missed, at_most: 0}\n")
	_, body, _ := get(t, s.Handler(), "/goals")
	cart, pay, search := strings.Index(body, "<bdi>web-cart</bdi>"), strings.Index(body, "<bdi>payments</bdi>"), strings.Index(body, "<bdi>search</bdi>")
	if cart < 0 || pay < 0 || search < 0 || cart > pay || cart > search || !strings.Contains(body, "None ended in the weeks shown.") {
		t.Errorf("web-cart, missed, at %d; payments at %d, search at %d, with none ended", cart, pay, search)
	}
}

// A slack reads unsigned, a lead's as a time: by 1m, not by +1m.
func TestGoalPeriodSlackText(t *testing.T) {
	g := ledger.Goal{Name: "g", Measure: "incidents.median_lead_seconds", NoWorseThan: "before", By: "60", Each: "sale"}
	before := 300.0
	if got := goalTarget("en", g); got != "no worse than before, by 1m" {
		t.Errorf("%q", got)
	}
	if got := periodTarget("en", g, &before); got != "no worse than the four weeks before, +5m, by 1m" {
		t.Errorf("%q", got)
	}
	g = ledger.Goal{Name: "g", Measure: "precision", NoWorseThan: "before", By: "0.05", Each: "sale"}
	if got := goalTarget("en", g); got != "no worse than before, by 5%" {
		t.Errorf("%q", got)
	}
}

// A window an expression (--mark) finds again is kept as a row anew, with
// another id, and so another key (id@start), but is the same occurrence:
// what was told of it, the task made for it and what was said on it hold.
// Read again three times, a window made in the UI between the reads, its
// miss is told once, said on its task once (as made), and, the task done,
// no task is made for it again.
func TestGoalsPeriodExpressionReadAgain(t *testing.T) {
	hook := filepath.Join(t.TempDir(), "hook")
	os.WriteFile(hook, []byte("https://hooks.example/x\n"), 0o600)
	s := goalServer(t, func(c *Config) { c.NotifyWebhook, c.GoalTasks = hook, true })
	ctx := context.Background()
	o := s.cfg.Options
	// Over INC-C, on cart, missed.
	w := ledger.Marked{Kind: "flash", Start: dayAt("2026-10-28", 9).Add(30 * time.Minute), End: dayAt("2026-10-28", 10).Add(30 * time.Minute)}
	declare(t, s, "goals:\n  - {name: flash-missed, each: flash, measure: incidents.missed, at_most: 0}\n")
	var keys []string
	read := func(since time.Time) {
		t.Helper()
		if _, err := s.l.KeepMarks(ctx, []ledger.MarkRead{{Kind: "flash", Key: "k", Since: since, To: since.Add(2 * time.Hour), Step: time.Minute,
			Windows: []ledger.Marked{w}}}, o); err != nil {
			t.Fatal(err)
		}
		if err := s.l.JudgePending(ctx, o); err != nil {
			t.Fatal(err)
		}
		again(t, s)
		g := periodIn(t, summaryOfGoals(t, s), "2026-10-26", "", "flash-missed", "flash")
		if g.Result != goalMissed {
			t.Fatalf("the flash window: %+v", g)
		}
		keys = append(keys, g.Occurrence)
	}
	for i, since := range []time.Time{dayAt("2026-10-28", 9), dayAt("2026-10-28", 9).Add(15 * time.Minute), dayAt("2026-10-28", 9).Add(20 * time.Minute)} {
		read(since)
		if i == 0 {
			if _, err := s.l.PutMarked(ctx, ledger.Marked{Kind: "launch", Title: "Launch", Start: dayAt("2026-10-30", 9), End: dayAt("2026-10-30", 10)}, "ana", o); err != nil {
				t.Fatal(err)
			}
		}
	}
	if keys[0] == keys[len(keys)-1] {
		t.Fatalf("the window kept with its id: %v", keys)
	}
	told := 0
	for _, n := range goalNotices(t, s) {
		if n.Name == "flash-missed" {
			told++
		}
	}
	ts, _ := s.l.Tasks(ctx, ledger.TaskQuery{Goal: "flash-missed"})
	if told != 1 || len(ts) != 1 || len(commentsOn(t, s, ts[0].ID)) != 0 {
		t.Fatalf("keys %v: told %d times, %d tasks, said %q", keys, told, len(ts), commentsOn(t, s, ts[0].ID))
	}
	if _, err := s.l.PatchTask(ctx, ts[0].ID, []byte(`{"status": "done"}`), "ana"); err != nil {
		t.Fatal(err)
	}
	read(dayAt("2026-10-28", 9).Add(25 * time.Minute))
	if ts, _ := s.l.Tasks(ctx, ledger.TaskQuery{Goal: "flash-missed"}); len(ts) != 1 {
		t.Errorf("made again, read again: %+v", ts)
	}
}

// An occurrence is held by what it is, not by its window's row: a window
// renamed is the same occurrence, and its miss isn't told again; one whose
// end a person changes is another, and is told once more.
func TestGoalsPeriodHeldByContent(t *testing.T) {
	hook := filepath.Join(t.TempDir(), "hook")
	os.WriteFile(hook, []byte("https://hooks.example/x\n"), 0o600)
	s := periodServer(t, func(c *Config) { c.NotifyWebhook = hook })
	ctx := context.Background()
	told := func() int {
		n := 0
		for _, x := range goalNotices(t, s) {
			if x.Name == "sale-none-missed" {
				n++
			}
		}
		return n
	}
	again(t, s)
	if n := told(); n != 1 {
		t.Fatalf("told %d", n)
	}
	ms, err := s.l.MarkedMeeting(ctx, dayAt("2026-10-27", 0), dayAt("2026-10-28", 0))
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(ms, func(m ledger.Marked) bool { return m.Title == "Autumn sale" })
	autumn := ms[i]
	for _, c := range []struct {
		change func(*ledger.Marked)
		told   int
	}{
		{func(m *ledger.Marked) { m.Title = "Autumn sale, renamed" }, 1},
		{func(m *ledger.Marked) { m.End = m.End.Add(time.Hour) }, 2},
	} {
		c.change(&autumn)
		if _, err := s.l.PutMarked(ctx, autumn, "ana", s.cfg.Options); err != nil {
			t.Fatal(err)
		}
		if err := s.l.JudgePending(ctx, s.cfg.Options); err != nil {
			t.Fatal(err)
		}
		again(t, s)
		if n := told(); n != c.told {
			t.Errorf("%+v: told %d, want %d", autumn, n, c.told)
		}
	}
}
