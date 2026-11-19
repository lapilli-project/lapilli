package ledger

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/incident"
)

// Each rule of a goal's: exactly one bound; a share's from 0 to 1, a
// count's from 0; by only with no_worse_than; critical only with a
// critical.* measure; each: deploy refused; a goal over a period of the
// measures marked windows count inside an occurrence, and no_worse_than of
// precision and the median lead alone, over a period alone.
func TestCheckGoal(t *testing.T) {
	for _, c := range []struct {
		g    Goal
		want string
	}{
		{Goal{Name: "a", Measure: "precision", AtLeast: "0.8"}, ""},
		{Goal{Name: "paged", Measure: "critical.firings", Critical: []string{"critical", "page", "page"}, AtMost: "40"}, ""},
		{Goal{Name: "lead", Measure: "incidents.median_lead_seconds", AtLeast: "-60"}, ""},
		{Goal{Name: "A", Measure: "precision", AtLeast: "0.8"}, "a goal's name is lower-case"},
		{Goal{Name: "-a", Measure: "precision", AtLeast: "0.8"}, "a goal's name is lower-case"},
		{Goal{Name: strings.Repeat("a", 65), Measure: "precision", AtLeast: "0.8"}, "64 bytes at most"},
		{Goal{Name: "a", Measure: "noise", AtLeast: "0.8"}, `measure "noise": want one of firings, precision`},
		{Goal{Name: "a", Measure: "precision"}, "a goal has one bound"},
		{Goal{Name: "a", Measure: "precision", AtLeast: "0.8", AtMost: "0.9"}, "a goal has one bound"},
		{Goal{Name: "a", Measure: "precision", AtLeast: "0.8", NoWorseThan: "before"}, "a goal has one bound"},
		{Goal{Name: "a", Measure: "precision", AtLeast: "80"}, "is a share, from 0 to 1"},
		{Goal{Name: "a", Measure: "firings", AtMost: "-1"}, "is a count, 0 or more"},
		{Goal{Name: "a", Measure: "firings", AtMost: "many"}, "at_most is a number"},
		{Goal{Name: "a", Measure: "firings", AtMost: "NaN"}, "at_most is a number"},
		{Goal{Name: "a", Measure: "precision", AtLeast: "0.8", By: "0.05"}, "by is the slack of no_worse_than"},
		{Goal{Name: "a", Measure: "firings", Critical: []string{"page"}, AtMost: "40"}, "critical is the severities a critical.* measure counts"},
		{Goal{Name: "a", Measure: "critical.firings", Critical: []string{strings.Repeat("x", 65)}, AtMost: "40"}, "critical is 32 severities at most"},
		{Goal{Name: "a", Measure: "precision", AtLeast: "0.8", Each: "deploy"}, "each: deploy: deploys are counted by during_deploys"},
		{Goal{Name: "a", Measure: "refired.share", AtMost: "0.1", Each: "sale"}, "refired.share isn't counted inside a marked window"},
		{Goal{Name: "a", Measure: "firings", NoWorseThan: "before", Each: "sale"}, "no_worse_than sets firings beside the weeks before"},
		{Goal{Name: "a", Measure: "precision", NoWorseThan: "before"}, "no_worse_than holds over a period: give each"},
		{Goal{Name: "a", Measure: "precision", NoWorseThan: "after", Each: "sale"}, "no_worse_than is before"},
		{Goal{Name: "a", Measure: "precision", NoWorseThan: "before", By: "0.05", Each: "sale"}, ""},
		{Goal{Name: "a", Measure: "incidents.median_lead_seconds", NoWorseThan: "before", By: "60", Each: "launch"}, ""},
		{Goal{Name: "a", Measure: "incidents.missed", AtMost: "0", Each: "sale"}, ""},
		{Goal{Name: "a", Measure: "precision", AtLeast: "0.5", Each: "sale"}, ""},
		{Goal{Name: "a", Measure: "precision", NoWorseThan: "before", By: "1.5", Each: "sale"}, "by is how much worse is no worse"},
		{Goal{Name: "a", Measure: "precision", NoWorseThan: "before", By: "-1", Each: "sale"}, "by is how much worse is no worse"},
		{Goal{Name: "a", Measure: "precision", AtLeast: "0.8", Each: "Sale!"}, "each is week, or a kind of marked window"},
		{Goal{Name: "a", Measure: "precision", AtLeast: "0.8", Each: "1sale"}, "each is week, or a kind of marked window"},
		{Goal{Name: "a", Measure: "precision", AtLeast: "0.8", Each: strings.Repeat("s", 33)}, "each is week, or a kind of marked window"},
	} {
		g := c.g
		err := CheckGoal(&g)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%+v: %v", c.g, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%+v: %v, want %q", c.g, err, c.want)
		}
	}
	// The measures with each: KIND, and those with no_worse_than, as the
	// design's table of measures says them: every other refused, saying
	// which are.
	withEach := map[string]bool{"firings": true, "precision": true, "refired.firings": true, "incidents.missed": true, "impacts.missed": true,
		"incidents.noisy_only": true, "incidents.median_lead_seconds": true, "episodes.total": true, "episodes.precision": true}
	withNoWorse := map[string]bool{"precision": true, "incidents.median_lead_seconds": true, "episodes.precision": true}
	for _, m := range GoalMeasureNames {
		g := Goal{Name: "a", Measure: m, AtMost: "0", Each: "sale"}
		if err := CheckGoal(&g); withEach[m] != (err == nil) {
			t.Errorf("each: sale, %s, at_most: %v", m, err)
		} else if err != nil && !strings.Contains(err.Error(), "with each: KIND, a goal is of firings, precision, refired.firings, incidents.missed, impacts.missed, incidents.noisy_only, incidents.median_lead_seconds, episodes.total or episodes.precision") {
			t.Errorf("each: sale, %s: %v", m, err)
		}
		if g := (Goal{Name: "a", Measure: m, AtMost: "0"}); CheckGoal(&g) != nil {
			t.Errorf("each week, %s: refused", m)
		}
		g = Goal{Name: "a", Measure: m, NoWorseThan: "before", Each: "sale"}
		if err := CheckGoal(&g); withNoWorse[m] != (err == nil) {
			t.Errorf("each: sale, %s, no_worse_than: %v", m, err)
		}
		g = Goal{Name: "a", Measure: m, NoWorseThan: "before"}
		if err := CheckGoal(&g); err == nil {
			t.Errorf("each week, %s, no_worse_than: taken", m)
		}
	}
	g := Goal{Name: "paged", Measure: "critical.firings", Critical: []string{" page ", "page", "critical"}, AtMost: "40"}
	if err := CheckGoal(&g); err != nil || !slices.Equal(g.Critical, []string{"page", "critical"}) || g.Each != "week" {
		t.Errorf("filled in: %+v %v", g, err)
	}
	// Four lists of severities at most, the default among them.
	var gs []Goal
	for _, cs := range [][]string{nil, {"page"}, {"page", "critical"}, {"critical", "page"}, {"sev1"}} {
		gs = append(gs, Goal{Name: "g" + strings.Join(cs, ""), Measure: "critical.firings", Critical: cs, AtMost: "1"})
	}
	if err := CheckGoals(gs[:4]); err != nil {
		t.Errorf("three lists, one given in two orders: %v", err)
	}
	if err := CheckGoals(append(gs, Goal{Name: "x", Measure: "critical.firings", Critical: []string{"sev2"}, AtMost: "1"})); err == nil || !strings.Contains(err.Error(), "lists of severities") {
		t.Errorf("five lists: %v", err)
	}
	if err := CheckGoals(make([]Goal, MaxGoals+1)); err == nil || !strings.Contains(err.Error(), "50 goals at most") {
		t.Errorf("51 goals: %v", err)
	}
}

// The goals the file declares are kept to it by name: made, changed, and
// deleted once the file no longer declares them, pruned or not, and with
// no file at all. One naming a team there isn't is skipped, said, and kept
// as it was; a name given twice is refused; a team a goal names isn't
// deleted, nor given up by the file, while the goal names it.
func TestReconcileGoals(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	d := declaredOf()
	d.Goals = []DeclaredGoal{
		{Goal: Goal{Name: "critical-caught", Measure: "critical.share_caught", AtLeast: "0.8"}, Line: 30},
		{Goal: Goal{Name: "no-refire", Measure: "refired.firings", AtMost: "0"}, Team: "*", Line: 33},
		{Goal: Goal{Name: "paged", Measure: "critical.firings", Critical: []string{"critical", "page"}, AtMost: "40"}, Team: "payments", Line: 37},
	}
	r, skipped := reconcile(t, l, d, Leave{})
	if r.Made != 8 || len(skipped) != 0 {
		t.Fatalf("made %+v %v", r, skipped)
	}
	gs, err := l.Goals(ctx)
	if err != nil || len(gs) != 3 {
		t.Fatalf("goals: %+v %v", gs, err)
	}
	pay := teamNamed(t, l, "payments")
	if gs[0].Name != "critical-caught" || gs[0].TeamID != 0 || gs[0].EveryTeam || !gs[0].Declared || gs[0].Each != "week" ||
		gs[1].Name != "no-refire" || !gs[1].EveryTeam || gs[1].TeamID != 0 ||
		gs[2].Name != "paged" || gs[2].TeamID != pay.ID || !slices.Equal(gs[2].Critical, []string{"critical", "page"}) {
		t.Errorf("kept: %+v", gs)
	}
	if !slices.Contains(changesBy(t, l, ConfigBy), "goal paged kept") {
		t.Errorf("activity: %v", changesBy(t, l, ConfigBy))
	}
	// The same file again writes nothing.
	if r, _ := reconcile(t, l, d, Leave{}); r.Any() {
		t.Errorf("the same again: %+v", r)
	}
	// A team the UI made, named by a goal the file declares: the UI
	// deletes it no more.
	ui, err := l.PutTeam(ctx, Team{Name: "search", Rules: "^Search"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	d.Goals = append(d.Goals, DeclaredGoal{Goal: Goal{Name: "search-quiet", Measure: "firings", AtMost: "10"}, Team: "search", Line: 41})
	reconcile(t, l, d, Leave{})
	if err := l.DeleteTeam(ctx, ui, "ana"); !errors.Is(err, ErrNamedByDeclared) {
		t.Errorf("deleting a team of the UI's a goal names: %v", err)
	}
	d.Goals = d.Goals[:3]
	reconcile(t, l, d, Leave{})
	if err := l.DeleteTeam(ctx, ui, "ana"); err != nil {
		t.Errorf("deleting it once no goal names it: %v", err)
	}
	// Changed, one taken out, one of a team there isn't (kept as it was).
	d.Goals = []DeclaredGoal{
		{Goal: Goal{Name: "critical-caught", Measure: "critical.share_caught", AtLeast: "0.9"}, Line: 30},
		{Goal: Goal{Name: "paged", Measure: "critical.firings", AtMost: "30"}, Team: "search", Line: 37},
	}
	r, skipped = reconcile(t, l, d, Leave{})
	if r.Changed != 1 || r.Deleted != 1 || len(skipped) != 1 || skipped[0].Kind != KindGoal || skipped[0].Line != 37 ||
		!strings.Contains(skipped[0].Error(), "there is no team called search") {
		t.Errorf("changed %+v %v", r, skipped)
	}
	gs, _ = l.Goals(ctx)
	if len(gs) != 2 || gs[0].AtLeast != "0.9" || gs[1].Name != "paged" || gs[1].AtMost != "40" || gs[1].TeamID != pay.ID {
		t.Errorf("after the change: %+v", gs)
	}
	// Given twice, in a Declared made otherwise than by the file: refused.
	d.Goals = append(d.Goals, DeclaredGoal{Goal: Goal{Name: "critical-caught", Measure: "precision", AtLeast: "0.5"}, Line: 40})
	if _, skipped = reconcile(t, l, d, Leave{}); len(skipped) != 2 || !strings.Contains(skipped[1].Error(), "declared on line 30 already") {
		t.Errorf("twice: %v", skipped)
	}
	// Taken out of the file, unpruned: deleted all the same.
	d.Goals = nil
	if r, _ := reconcile(t, l, d, Leave{}); r.Deleted != 2 {
		t.Errorf("taken out: %+v", r)
	}
	if gs, _ := l.Goals(ctx); len(gs) != 0 {
		t.Errorf("left: %+v", gs)
	}
	// Without a file, none.
	d.Goals = []DeclaredGoal{{Goal: Goal{Name: "a", Measure: "firings", AtMost: "100"}, Line: 1}}
	reconcile(t, l, d, Leave{})
	if r, _ := reconcile(t, l, Declared{}, Leave{NoFile: true}); r.Deleted < 1 {
		t.Errorf("no file: %+v", r)
	}
	if gs, _ := l.Goals(ctx); len(gs) != 0 {
		t.Errorf("no file left: %+v", gs)
	}
}

// teamNamed is the team of a name.
func teamNamed(t *testing.T, l *Ledger, name string) Team {
	t.Helper()
	ts, err := l.Teams(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range ts {
		if x.Name == name {
			return x
		}
	}
	t.Fatalf("no team %s", name)
	return Team{}
}

// A goal's task: one open for a goal and team at most; its goal set as it
// is made and kept through an edit; what is said on it kept with what was
// said, in one write; the week it was made for noted, so that one done
// isn't made again for the same week; and what was told of a goal kept
// though the file no longer declares it, or gives it another team, until
// it is weeks old.
func TestGoalTasks(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	tid, err := l.PutTeam(ctx, Team{Name: "payments", Labels: incident.Labels{"team": {"payments"}}}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	week := Week(l.Day(time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)))
	task := Task{Title: "Goal paged of payments missed", Kind: "decide", TeamID: tid, Goal: "paged"}
	id, err := l.MakeGoalTask(ctx, task, week, GoalTasksBy)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := l.MakeGoalTask(ctx, task, week+7, "ana"); !errors.Is(err, ErrGoalTaskOpen) || again != id {
		t.Errorf("a second open: %d %v", again, err)
	}
	// Another team's, or everyone's, is another.
	if _, err := l.MakeGoalTask(ctx, Task{Title: "everyone's", Kind: "decide", Goal: "paged"}, week, "ana"); err != nil {
		t.Errorf("everyone's: %v", err)
	}
	marks := func() (told, made map[string]int64) {
		t.Helper()
		told, made, err := l.GoalMarks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return told, made
	}
	if _, made := marks(); made[GoalKey("paged", tid)] != week || made[GoalKey("paged", 0)] != week {
		t.Errorf("made for: %v", made)
	}
	if open, err := l.OpenGoalTasks(ctx); err != nil || len(open) != 2 || open[GoalKey("paged", tid)] != id {
		t.Errorf("open: %v %v", open, err)
	}
	st, ok, err := l.GoalTask(ctx, id)
	if err != nil || !ok || st.Week != week || st.Said[week] != "missed" {
		t.Errorf("kept: %+v %v %v", st, ok, err)
	}
	// An edit doesn't change what it is about; a patch can't either.
	x, _ := l.TaskByID(ctx, id)
	x.Goal, x.Title = "", "renamed"
	if _, err := l.PutTask(ctx, x, "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PatchTask(ctx, id, []byte(`{"goal": "other"}`), "ana"); err != nil {
		t.Fatal(err)
	}
	if x, _ := l.TaskByID(ctx, id); x.Goal != "paged" || x.Title != "renamed" {
		t.Errorf("edited: %+v", x)
	}
	// A goal an edit gives, not a goal's name, is no refusal: it is the
	// task's own goal that is kept, and checked.
	if _, err := l.PatchTask(ctx, id, []byte(`{"goal": "Not A Name", "title": "patched"}`), "ana"); err != nil {
		t.Errorf("a patch giving a goal: %v", err)
	}
	if _, err := l.PutTask(ctx, Task{Title: "bad", Goal: "Not A Name"}, "ana"); err == nil {
		t.Error("a task's goal that is no goal's name")
	}
	st.Said[week+7] = "missed"
	if err := l.SayOnGoalTask(ctx, id, "Missed again, week of 2026-11-16: 41.", st, GoalTasksBy); err != nil {
		t.Fatal(err)
	}
	cs, _ := l.Comments(ctx, "task", "1")
	if len(cs) != 1 || cs[0].By != GoalTasksBy || !strings.HasPrefix(cs[0].Body, "Missed again") {
		t.Errorf("said: %+v", cs)
	}
	if st, _, _ := l.GoalTask(ctx, id); st.Said[week+7] != "missed" {
		t.Errorf("what was said, kept: %+v", st)
	}
	if _, made := marks(); made[GoalKey("paged", tid)] != week+7 {
		t.Errorf("a week said missed is one a task was made for: %v", made)
	}
	open, err := l.Tasks(ctx, TaskQuery{Status: []string{"open"}, Goal: "paged"})
	if err != nil || len(open) != 2 {
		t.Errorf("of the goal: %+v %v", open, err)
	}
	if ids, err := l.GoalTasksKept(ctx); err != nil || len(ids) != 2 {
		t.Errorf("kept of: %v %v", ids, err)
	}
	if err := l.ForgetGoalTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := l.GoalTask(ctx, id); ok {
		t.Error("forgotten, still kept")
	}
	// Told in one write for its teams, each marked; a team marked for a
	// later week keeps it.
	other, err := l.PutTeam(ctx, Team{Name: "search", Rules: "^Search"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	l.TellTo([]string{"webhook"})
	defer l.TellTo(nil)
	if err := l.TellGoalMissed(ctx, "paged", []int64{tid, other}, week+7, []Told{{Dest: "webhook", About: map[string]any{"goal": "paged"}}}); err != nil {
		t.Fatal(err)
	}
	if err := l.TellGoalMissed(ctx, "paged", []int64{tid}, week, nil); err != nil {
		t.Fatal(err)
	}
	if told, _ := marks(); told[GoalKey("paged", tid)] != week+7 || told[GoalKey("paged", other)] != week+7 {
		t.Errorf("told: %v", told)
	}
	if due, _ := l.Due(ctx, "webhook", time.Now().Add(time.Hour), 10); len(due) != 1 || due[0].Event != "goal.missed" {
		t.Errorf("told once: %+v", due)
	}
	// Taken out of the file, put back with another team: what was told and
	// made for is kept, by each team's id.
	d := Declared{Teams: []DeclaredTeam{{Team: Team{Name: "payments", Labels: incident.Labels{"team": {"payments"}}}, Line: 1}},
		Goals: []DeclaredGoal{{Goal: Goal{Name: "paged", Measure: "critical.firings", AtMost: "40"}, Team: "payments", Line: 3}}}
	reconcile(t, l, d, Leave{})
	d.Goals[0].Team = "*"
	reconcile(t, l, d, Leave{})
	d.Goals = nil
	reconcile(t, l, d, Leave{})
	if told, made := marks(); told[GoalKey("paged", tid)] != week+7 || made[GoalKey("paged", tid)] != week+7 {
		t.Errorf("of a goal taken out: %v %v", told, made)
	}
	if x, _ := l.TaskByID(ctx, id); x.Goal != "paged" || x.Title != "patched" {
		t.Errorf("its tasks keep its name: %+v", x)
	}
	// Weeks old, let go.
	if n, err := l.PruneGoalMarks(ctx, week+7); err != nil || n != 1 {
		t.Errorf("pruned %d: %v", n, err)
	}
	if told, made := marks(); len(told) != 2 || made[GoalKey("paged", 0)] != 0 || made[GoalKey("paged", tid)] != week+7 {
		t.Errorf("after pruning: %v %v", told, made)
	}
	if n, _ := l.PruneGoalMarks(ctx, week+14); n != 3 {
		t.Errorf("pruned %d, want 3", n)
	}
}
