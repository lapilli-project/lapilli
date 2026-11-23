package ledger

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
)

// undo020 takes back what 020 made, for the tests of the migrations
// before it.
var undo020 = []string{
	"DROP INDEX tasks_goal",
	"ALTER TABLE tasks DROP COLUMN goal",
	"DROP TABLE goals",
}

// A ledger at 019 holding a team, firings, an incident and tasks, in
// SQLite or, with LAPILLI_TEST_POSTGRES, in Postgres, migrates to 020: no
// goal is declared, every task is kept as it was, of no goal, the firings
// are judged the same, and a goal can then be declared, and its task made.
func TestMigrate020(t *testing.T) {
	ctx := context.Background()
	where := place(t)
	l, err := openAt(ctx, where, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	team, err := l.PutTeam(ctx, Team{Name: "payments", Rules: "^Errors$"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{
		{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: t0.Add(2 * time.Hour), End: t0.Add(2*time.Hour + 10*time.Minute)},
		{Rule: "Errors", Labels: map[string]string{"service": "search"}, Start: t0.Add(5 * time.Hour), End: t0.Add(5*time.Hour + 10*time.Minute)}})
	if _, err := l.SyncFile(ctx, []incident.Incident{{ID: "INC-1", Start: t0.Add(2 * time.Hour), End: t0.Add(3 * time.Hour),
		Labels: incident.Labels{"service": {"checkout"}}}}, opts); err != nil {
		t.Fatal(err)
	}
	id, err := l.PutTask(ctx, Task{Title: "tune it", Kind: "tune", Rule: "Errors", TeamID: team}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	before, err := l.Report(ctx, t0, t0.Add(24*time.Hour), opts, true)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	// Back to 019.
	if strings.HasPrefix(where, "postgres") {
		c, err := pgx.Connect(ctx, where)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range append(slices.Concat(undo030, undo029, undo028, undo027, undo026, undo025, undo024, undo023, undo022pg, undo021, undo020), "UPDATE schema_version SET version = 19") {
			if _, err := c.Exec(ctx, q); err != nil {
				t.Fatal(q, err)
			}
		}
		c.Close(ctx)
	} else {
		l, err := Open(ctx, where, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range append(slices.Concat(undo030, undo029, undo028, undo027, undo026, undo025, undo024, undo023, undo022, undo021, undo020), "PRAGMA user_version = 19") {
			if _, err := l.w.ExecContext(ctx, q); err != nil {
				t.Fatal(q, err)
			}
		}
		l.Close()
	}
	l2, err := openAt(ctx, where, time.UTC)
	if err != nil {
		t.Fatalf("migrating to 020: %v", err)
	}
	defer l2.Close()
	gs, err := l2.Goals(ctx)
	if err != nil || len(gs) != 0 {
		t.Errorf("goals after migrating: %+v %v", gs, err)
	}
	x, err := l2.TaskByID(ctx, id)
	if err != nil || x.Title != "tune it" || x.Goal != "" || x.TeamID != team {
		t.Errorf("the task after migrating: %+v %v", x, err)
	}
	// Its episodes counted, as the leader counts them after an upgrade
	// (schema 025).
	if _, err := l2.CountEpisodes(ctx, opts, nil); err != nil {
		t.Fatal(err)
	}
	after, err := l2.Report(ctx, t0, t0.Add(24*time.Hour), opts, true)
	if err != nil || !reflect.DeepEqual(before.Rules, after.Rules) || len(after.Firings) != 2 {
		t.Errorf("the firings after migrating: %+v %v", after, err)
	}
	if _, skipped, err := l2.Reconcile(ctx, Declared{Goals: []DeclaredGoal{{Goal: Goal{Name: "none-missed", Measure: "incidents.missed", AtMost: "0"},
		Team: "payments", Line: 1}}}, Leave{}, opts); err != nil || len(skipped) > 0 {
		t.Fatal(skipped, err)
	}
	if gs, _ := l2.Goals(ctx); len(gs) != 1 || gs[0].TeamID != team {
		t.Errorf("declared after migrating: %+v", gs)
	}
	if _, err := l2.MakeGoalTask(ctx, Task{Title: "Goal none-missed missed", Kind: "decide", TeamID: team, Goal: "none-missed"}, Week(l2.Day(t0)), "ana"); err != nil {
		t.Error(err)
	}
	var v int
	if strings.HasPrefix(where, "postgres") {
		l2.r.QueryRowContext(ctx, "SELECT version FROM schema_version").Scan(&v)
	} else {
		l2.w.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	}
	if want := schemaVersion(t); v != want || want < 20 {
		t.Errorf("migrated to %d, want %d", v, want)
	}
}
