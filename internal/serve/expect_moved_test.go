package serve

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/scenario/scenariotest"
)

// serve's checks after deploy, a follow-up of 0.65.0's review: a catch's
// or a scenario's row moving to another week keeps what was told of it
// and its task.

// openTasks are the tasks the ledger has, by id.
func openTasks(t *testing.T, s *Server) []ledger.Task {
	t.Helper()
	ts, err := s.l.Tasks(context.Background(), ledger.TaskQuery{})
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// Guard: a scenario broken in one week, its test file edited a week on
// and failing still, is found in the new week, its row that week's: told
// once, and made a task once, the task kept on its row (the tab's link),
// not a second made while the first is open.
func TestScenarioMovedWeekKeepsItsTask(t *testing.T) {
	expect := "expectations:\n  - rule: Errors\n    scenarios: [tests/errors.yaml]\n"
	s, _, dir, count := scenarioServer(t, expect, map[string]string{"tests/errors.yaml": scenariotest.Test("fail")}, true)
	s.cfg.ExpectTasks = true
	checkAgain(t, s)
	was := checksOf(t, s, "Errors")["scenario tests/errors.yaml"]
	if was.Result != ledger.ExpectBroken || was.Task == 0 || len(openTasks(t, s)) != 1 || len(expectNotices(t, s)) != 1 || count() != 1 {
		t.Fatalf("first: %+v, %d runs", was, count())
	}
	s.now = func() time.Time { return now.Add(7 * 24 * time.Hour) }
	os.WriteFile(filepath.Join(dir, "tests", "errors.yaml"), []byte(strings.Replace(scenariotest.Test("fail"), "evaluation_interval: 1m", "evaluation_interval: 2m", 1)), 0o600)
	checkAgain(t, s)
	c := checksOf(t, s, "Errors")["scenario tests/errors.yaml"]
	if c.Week == was.Week || c.Result != ledger.ExpectBroken || count() != 2 {
		t.Fatalf("a week on, edited: %+v, %d runs", c, count())
	}
	if c.Task != was.Task {
		t.Errorf("its task, %d, isn't on its row: %d", was.Task, c.Task)
	}
	if ts := openTasks(t, s); len(ts) != 1 {
		t.Errorf("a second task while the first is open: %+v", ts)
	}
	if ns := expectNotices(t, s); len(ns) != 1 {
		t.Errorf("told again: %+v", ns)
	}
	// Met, then broken again: told again, the task still open kept.
	os.WriteFile(filepath.Join(dir, "tests", "errors.yaml"), []byte(scenariotest.Test("pass")), 0o600)
	checkAgain(t, s)
	s.now = func() time.Time { return now.Add(14 * 24 * time.Hour) }
	os.WriteFile(filepath.Join(dir, "tests", "errors.yaml"), []byte(scenariotest.Test("fail")), 0o600)
	checkAgain(t, s)
	if c := checksOf(t, s, "Errors")["scenario tests/errors.yaml"]; c.Result != ledger.ExpectBroken || c.Task != was.Task || len(openTasks(t, s)) != 1 ||
		len(expectNotices(t, s)) != 2 {
		t.Errorf("met, then broken again: %+v, %d told, %d tasks", c, len(expectNotices(t, s)), len(openTasks(t, s)))
	}
}

// Guard: a catch broken, its incident's start written down otherwise, in
// another week, is of that week now: told once, a task made once.
func TestCatchMovedWeekToldOnce(t *testing.T) {
	ctx := context.Background()
	s := expectServer(t, "expectations:\n  - rule: Paged\n    catches: [{incident: INC-C}]\n", true)
	checkAgain(t, s)
	was := checksOf(t, s, "Paged")["catches INC-C"]
	if was.Result != ledger.ExpectBroken || was.Task == 0 || len(expectNotices(t, s)) != 1 || len(openTasks(t, s)) != 1 {
		t.Fatalf("first: %+v", was)
	}
	incs := slices.Clone(goalIncidents)
	incs[2].Start, incs[2].End = dayAt("2026-10-21", 15), dayAt("2026-10-21", 16)
	if _, err := s.l.SyncFile(ctx, incs, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	if err := s.l.JudgePending(ctx, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	checkAgain(t, s)
	c := checksOf(t, s, "Paged")["catches INC-C"]
	if c.Week == was.Week || c.Result != ledger.ExpectBroken {
		t.Fatalf("moved: %+v", c)
	}
	if ns := expectNotices(t, s); len(ns) != 1 {
		t.Errorf("told again: %+v", ns)
	}
	if ts := openTasks(t, s); len(ts) != 1 || c.Task != was.Task {
		t.Errorf("tasks: %+v, its row's %d", ts, c.Task)
	}
}
