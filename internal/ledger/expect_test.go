package ledger

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/incident"
)

// checkOf is a budget's check of Restarts in a week.
func checkOf(rule string, week int64, result string, n int) ExpectCheck {
	return ExpectCheck{Rule: rule, Expect: ExpectHash("rule: "+rule, "budget week", "10"), Week: week, What: "budget week", Held: "10",
		Selector: "rule: " + rule, Source: "file", Result: result, N: &n, Value: "x"}
}

// What a round finds is kept, in SQLite or, with LAPILLI_TEST_POSTGRES,
// in Postgres: written only where it changed, kept not current once no
// longer held, a budget's week let go once out of the weeks checked, and a
// catch in one row, of its incident's week. A rule's name longer than a
// btree holds is kept and found again.
func TestKeepExpectChecks(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	w := Week(l.Day(t0))
	long := strings.Repeat("ルール", 1200) // 10,800 bytes
	found := []ExpectCheck{checkOf("Restarts", w-7, ExpectBroken, 26), checkOf("Restarts", w, ExpectOpen, 3), checkOf(long, w, ExpectOpen, 1)}
	catch := ExpectCheck{Rule: "Restarts", Expect: ExpectHash("rule: Restarts", "catches INC-43", "within 5m"), What: "catches INC-43", Held: "within 5m",
		Selector: "rule: Restarts", Source: "config", Result: ExpectUnknown, Why: "no_incident", Value: "–"}
	found = append(found, catch)
	read := func(q ExpectQuery) []ExpectCheck {
		t.Helper()
		cs, err := l.ExpectChecks(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		return cs
	}
	keep := func(found []ExpectCheck, this int64) int {
		t.Helper()
		gen := l.Gen()
		left, err := l.KeepExpectChecks(ctx, found, read(ExpectQuery{}), this-12*7, this, t0)
		if err != nil {
			t.Fatal(err)
		}
		if left != 0 {
			t.Fatalf("left %d", left)
		}
		return int(l.Gen() - gen)
	}
	if keep(found, w) == 0 {
		t.Fatal("nothing written")
	}
	cs := read(ExpectQuery{Current: true})
	if len(cs) != 4 {
		t.Fatalf("kept %+v", cs)
	}
	if got := read(ExpectQuery{Rule: long, HasRule: true}); len(got) != 1 || got[0].Rule != long || *got[0].N != 1 {
		t.Errorf("a long name: %+v", got)
	}
	if got := read(ExpectQuery{Broken: true, From: w - 7}); len(got) != 1 || got[0].Week != w-7 || *got[0].N != 26 {
		t.Errorf("broken: %+v", got)
	}
	// The same again writes nothing: the ledger's generation stays.
	if keep(found, w) != 0 {
		t.Error("written again with nothing changed")
	}

	// Told of the broken week, then found met: its mark goes, as of a
	// week before this one; told again once broken again.
	k := found[0].Key()
	if err := l.TellExpectBroken(ctx, []ExpectKey{k}, w, [][]Told{{{Dest: "webhook", About: map[string]any{"rule": "Restarts"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := l.TellExpectBroken(ctx, []ExpectKey{k}, w, [][]Told{{{Dest: "webhook"}}}); err != nil {
		t.Fatal(err)
	}
	if due, _ := l.Due(ctx, "webhook", time.Now().Add(time.Hour), 10); len(due) != 1 || due[0].Event != "expectation.broken" {
		t.Errorf("told twice in a week, or not at all: %+v", due)
	}
	if got := read(ExpectQuery{Broken: true, From: w - 7}); got[0].Told != w {
		t.Errorf("told: %+v", got[0])
	}
	met := checkOf("Restarts", w-7, ExpectMet, 9)
	keep([]ExpectCheck{met, found[1], found[2], found[3]}, w+7)
	if got := read(ExpectQuery{Rule: "Restarts", HasRule: true}); got[len(got)-2].Told != 0 {
		t.Errorf("met, still told: %+v", got)
	}

	// No longer held: kept, not current. The incident written down: the
	// catch's row is of its week, the other let go.
	catch.Week, catch.Result, catch.Why, catch.Value, catch.At = w, ExpectMet, "", "+2m", t0.Add(time.Hour)
	keep([]ExpectCheck{met, catch}, w+7)
	all := read(ExpectQuery{})
	var catches, current int
	for _, c := range all {
		if c.Catch() {
			catches++
			if c.Week != w || !c.At.Equal(t0.Add(time.Hour)) {
				t.Errorf("the catch: %+v", c)
			}
		}
		if c.Current {
			current++
		}
	}
	if catches != 1 || current != 2 || len(all) != 4 {
		t.Errorf("after: %+v", all)
	}
	// Thirteen weeks on, the budget's weeks are let go.
	keep([]ExpectCheck{catch}, w+14*7)
	if got := read(ExpectQuery{}); len(got) != 1 || !got[0].Catch() {
		t.Errorf("thirteen weeks on: %+v", got)
	}

	// MaxExpectWrites at a time: the rest the next round.
	was := MaxExpectWrites
	MaxExpectWrites = 2
	defer func() { MaxExpectWrites = was }()
	many := []ExpectCheck{catch, checkOf("A", w, ExpectOpen, 1), checkOf("B", w, ExpectOpen, 1), checkOf("C", w, ExpectOpen, 1)}
	left, err := l.KeepExpectChecks(ctx, many, read(ExpectQuery{}), w-12*7, w, t0)
	if err != nil || left != 1 {
		t.Fatalf("left %d %v", left, err)
	}
	if left, err := l.KeepExpectChecks(ctx, many, read(ExpectQuery{}), w-12*7, w, t0); err != nil || left != 0 || len(read(ExpectQuery{})) != 4 {
		t.Errorf("the rest: %d %v %+v", left, err, read(ExpectQuery{}))
	}
}

// A task is made of a row broken, once while one made of the rule's
// expectation, of any week, is open; never of one met.
func TestMakeExpectTask(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	w := Week(l.Day(t0))
	found := []ExpectCheck{checkOf("Restarts", w-7, ExpectBroken, 26), checkOf("Restarts", w, ExpectBroken, 12), checkOf("Calm", w, ExpectMet, 1)}
	if _, err := l.KeepExpectChecks(ctx, found, nil, w-12*7, w, t0); err != nil {
		t.Fatal(err)
	}
	id, err := l.MakeExpectTask(ctx, Task{Title: "Restarts fired 26 times", Kind: "tune", Rule: "Restarts"}, found[0].Key(), w, "ana")
	if err != nil || id == 0 {
		t.Fatal(id, err)
	}
	if again, err := l.MakeExpectTask(ctx, Task{Title: "again", Kind: "tune", Rule: "Restarts"}, found[1].Key(), w, "ana"); !errors.Is(err, ErrExpectTaskOpen) || again != id {
		t.Errorf("another while one is open: %d %v", again, err)
	}
	if _, err := l.MakeExpectTask(ctx, Task{Title: "calm", Kind: "tune", Rule: "Calm"}, found[2].Key(), w, "ana"); !errors.Is(err, ErrExpectNotBroken) {
		t.Errorf("of a row met: %v", err)
	}
	open, err := l.OpenExpectTasks(ctx)
	if err != nil || open[found[0].Key()] != id || len(open) != 1 {
		t.Errorf("open: %v %v", open, err)
	}
	if _, err := l.PatchTask(ctx, id, []byte(`{"status": "done"}`), "ana"); err != nil {
		t.Fatal(err)
	}
	if id2, err := l.MakeExpectTask(ctx, Task{Title: "this week", Kind: "tune", Rule: "Restarts"}, found[1].Key(), w, "ana"); err != nil || id2 == id {
		t.Errorf("once the first is done: %d %v", id2, err)
	}
}

// The incidents a catch names are found by their ids, counted ones alone,
// with what caught them.
func TestIncidentsByKey(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	incs := []incident.Incident{{ID: "INC-1", Start: t0, End: t0.Add(time.Hour)}, {ID: "INC-2", Start: t0.Add(2 * time.Hour), End: t0.Add(3 * time.Hour)}}
	if _, err := l.SyncFile(ctx, incs, opts); err != nil {
		t.Fatal(err)
	}
	got, err := l.IncidentsByKey(ctx, []string{"INC-2", "INC-9", "INC-1"})
	if err != nil || len(got) != 2 || !got["INC-1"].Start.Equal(t0) || got["INC-2"].ID != "INC-2" {
		t.Errorf("%+v %v", got, err)
	}
}

// The new queries read along an index, in either database: a rule's
// checks, those broken, an incident by its id.
func TestExpectQueriesIndexed(t *testing.T) {
	l := open(t, time.UTC)
	for _, c := range []struct{ q, table string }{
		{"SELECT " + expectCols + " FROM expectation_checks WHERE " + l.d.pick("rule = ?1", "md5(rule) = md5(?1) AND rule = ?1"), "expectation_checks"},
		{"SELECT " + expectCols + " FROM expectation_checks WHERE result = 'broken' AND (week >= ?1 OR what LIKE 'catches %' OR what LIKE 'scenario %')", "expectation_checks"},
		{"SELECT i.id FROM incidents i WHERE " + l.d.pick("i.key IN (?1, ?2)", "md5(i.key) IN (md5(?1), md5(?2)) AND i.key IN (?1, ?2)"), "incidents"},
	} {
		args := []any{"Restarts"}
		if strings.Contains(c.q, "?2") {
			args = []any{"INC-1", "INC-2"}
		} else if strings.Contains(c.q, "week >=") {
			args = []any{int64(20000)}
		}
		if p := plan(t, l, c.q, args...); scansAll(l, p, c.table) {
			t.Errorf("%s reads %s whole: %s", c.q, c.table, p)
		}
	}
}

// A scenario keeps one row, as a catch does: of the week what it found was
// found in, read as broken whatever that week, with what promtool said;
// found otherwise in a later week, its row is that week's, the other let
// go. In SQLite or, with LAPILLI_TEST_POSTGRES, in Postgres.
func TestKeepExpectScenario(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	w := Week(l.Day(t0))
	sc := ExpectCheck{Rule: "Restarts", Expect: ExpectHash("rule: Restarts", "scenario tests/restarts.yaml", "passes"), Week: w - 21,
		What: "scenario tests/restarts.yaml", Held: "passes", Selector: "rule: Restarts", Source: "file", Result: ExpectBroken, Value: "fails",
		Version: "abc", Detail: "    alertname: Restarts, time: 10m,\n        got:[]"}
	keep := func(found []ExpectCheck, this int64) []ExpectCheck {
		t.Helper()
		old, err := l.ExpectChecks(ctx, ExpectQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := l.KeepExpectChecks(ctx, found, old, this-12*7, this, t0); err != nil {
			t.Fatal(err)
		}
		all, err := l.ExpectChecks(ctx, ExpectQuery{})
		if err != nil {
			t.Fatal(err)
		}
		return all
	}
	// Twelve weeks on, still held: not let go as a budget's week would be.
	if all := keep([]ExpectCheck{sc}, w); len(all) != 1 || all[0].Week != w-21 || all[0].Detail != sc.Detail || !all[0].Scenario() || !all[0].Weekless() {
		t.Fatalf("%+v", all)
	}
	if got, err := l.ExpectChecks(ctx, ExpectQuery{Broken: true, Current: true, From: w - 7}); err != nil || len(got) != 1 {
		t.Errorf("broken, of a week before From: %+v %v", got, err)
	}
	// Run again on a new version, in this week: its row is this week's.
	again := sc
	again.Week, again.Result, again.Value, again.Version, again.Detail = w, ExpectMet, "passes", "def", ""
	if all := keep([]ExpectCheck{again}, w); len(all) != 1 || all[0].Week != w || all[0].Result != ExpectMet || all[0].Detail != "" {
		t.Errorf("run again: %+v", all)
	}
	// No longer held: kept, not current, then let go 13 weeks on.
	if all := keep(nil, w); len(all) != 1 || all[0].Current {
		t.Errorf("no longer held: %+v", all)
	}
	old, _ := l.ExpectChecks(ctx, ExpectQuery{})
	if _, err := l.KeepExpectChecks(ctx, nil, old, w, w+14*7, t0.Add(14*7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if all, _ := l.ExpectChecks(ctx, ExpectQuery{}); len(all) != 0 {
		t.Errorf("thirteen weeks on: %+v", all)
	}
}
