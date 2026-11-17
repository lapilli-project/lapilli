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
	"lapilli.dev/internal/ledger/ledgertest"
)

// undo022, undo022pg: ledgertest's, which lapilli serve's tests take a
// ledger back to 021 with too.
var (
	undo022   = ledgertest.Undo022SQLite
	undo022pg = ledgertest.Undo022Postgres
)

// A ledger at 021 with one impact signal with no name, in SQLite or, with
// LAPILLI_TEST_POSTGRES, in Postgres, migrates to 022 with its windows,
// their keys and what was decided of them, the tasks and comments on them,
// its tallies and the record it keeps, its start among it, unchanged: what
// meta kept of it is the unnamed signal's row, every alert is tied to no
// signal, and every tally is keyed by that.
func TestMigrate022(t *testing.T) {
	ctx := context.Background()
	where := place(t)
	l, err := openAt(ctx, where, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	from, to := t0, t0.Add(24*time.Hour)
	w1, w2 := impactWindow("search", t0.Add(3*time.Hour), 30*time.Minute), impactWindow("cart", t0.Add(9*time.Hour), 20*time.Minute)
	f := Found{From: from, To: to, ImpactOn: true, ImpactSince: from.Add(-time.Hour), ImpactCriterion: "5m impact", Impact: []incident.Incident{w1, w2},
		Firings: []firing.Firing{
			{Rule: "Errors", Labels: map[string]string{"service": "search"}, Start: t0.Add(3*time.Hour + 5*time.Minute), End: t0.Add(4 * time.Hour)},
			{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: t0.Add(6 * time.Hour), End: t0.Add(6*time.Hour + 10*time.Minute)}},
		Run: Run{Kind: "test", Started: to, Finished: to, From: from, To: to}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	if err := l.MarkAgain(ctx, "5m impact"); err != nil {
		t.Fatal(err)
	}
	if err := l.SetImpactData(ctx, ImpactData{Criterion: "5m impact", From: from.Add(-time.Hour), CheckedTo: to,
		Selectors: []SelectorData{{Sel: "impact", First: from.Add(-time.Hour), AtStart: true, To: to}}}); err != nil {
		t.Fatal(err)
	}
	ws, err := l.ImpactWindows(ctx, from, to)
	if err != nil || len(ws) != 2 {
		t.Fatalf("windows: %+v %v", ws, err)
	}
	if _, err := l.Decide(ctx, ws[1].ID, "dismissed", "", "ana", opts); err != nil {
		t.Fatal(err)
	}
	task, err := l.PutTask(ctx, Task{Title: "look", Kind: "decide", Incident: ws[0].ID}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutComment(ctx, Comment{On: "incident", Key: ws[0].ID, Body: "a deploy"}, "ana"); err != nil {
		t.Fatal(err)
	}
	keptBefore, err := l.ImpactKept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	read := func(l *Ledger) ([]Incident, []Tally, []Tally, *verdict0) {
		t.Helper()
		incs, err := l.Incidents(ctx, from, to, true)
		if err != nil {
			t.Fatal(err)
		}
		days, err := l.Days(ctx, l.Day(from), l.Day(to))
		if err != nil {
			t.Fatal(err)
		}
		weeks, err := l.Weeks(ctx, Week(l.Day(from)), Week(l.Day(to)))
		if err != nil {
			t.Fatal(err)
		}
		rep, err := l.Report(ctx, from, to, opts, true)
		if err != nil {
			t.Fatal(err)
		}
		return incs, days, weeks, &verdict0{rep.Rules, rep.Incidents, rep.Firings, rep.NoRecord}
	}
	incs, days, weeks, rep := read(l)
	l.Close()

	// Back to 021, as 0.53 left it.
	if strings.HasPrefix(where, "postgres") {
		c, err := pgx.Connect(ctx, where)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range append(slices.Clone(undo022pg), "UPDATE schema_version SET version = 21") {
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
		for _, q := range append(slices.Clone(undo022), "PRAGMA user_version = 21") {
			if _, err := l.w.ExecContext(ctx, q); err != nil {
				t.Fatal(q, err)
			}
		}
		l.Close()
	}

	l2, err := openAt(ctx, where, time.UTC)
	if err != nil {
		t.Fatalf("migrating to 022: %v", err)
	}
	defer l2.Close()
	if v, err := l2.meta(ctx, l2.r, "impact_since"); err != nil || v != "" {
		t.Errorf("impact_since is left in meta: %q %v", v, err)
	}
	keptAfter, err := l2.ImpactKept(ctx)
	if err != nil || !reflect.DeepEqual(keptBefore, keptAfter) || keptAfter.From().IsZero() {
		t.Errorf("what keeps the record: %+v, not %+v (%v)", keptAfter, keptBefore, err)
	}
	incs2, days2, weeks2, rep2 := read(l2)
	if !reflect.DeepEqual(incs, incs2) {
		t.Errorf("windows after migrating:\n%+v\nnot\n%+v", incs2, incs)
	}
	if !reflect.DeepEqual(days, days2) || !reflect.DeepEqual(weeks, weeks2) {
		t.Errorf("tallies after migrating: %+v %+v, not %+v %+v", days2, weeks2, days, weeks)
	}
	if !reflect.DeepEqual(rep, rep2) {
		t.Errorf("the report after migrating: %+v, not %+v", rep2, rep)
	}
	if x, err := l2.TaskByID(ctx, task); err != nil || x.Incident != ws[0].ID {
		t.Errorf("the task after migrating: %+v %v", x, err)
	}
	if cs, err := l2.Comments(ctx, "incident", ws[0].ID); err != nil || len(cs) != 1 {
		t.Errorf("the comments after migrating: %+v %v", cs, err)
	}
	var untied, other int
	l2.r.QueryRowContext(ctx, "SELECT count(*) FROM series WHERE signals != 0").Scan(&untied)
	l2.r.QueryRowContext(ctx, "SELECT count(*) FROM days WHERE signals != 0").Scan(&other)
	if untied != 0 || other != 0 {
		t.Errorf("%d alerts and %d days tied to a signal, with none named", untied, other)
	}
	// A run after it goes on as before: the window on at its start, of the
	// same criterion, goes on.
	next := Found{From: to, To: to.Add(time.Hour), ImpactOn: true, ImpactSince: to.Add(-time.Hour), ImpactCriterion: "5m impact",
		Run: Run{Kind: "test", Started: to, Finished: to, From: to, To: to.Add(time.Hour)}}
	if err := l2.Record(ctx, next, opts); err != nil {
		t.Fatal(err)
	}
	if k, err := l2.ImpactKept(ctx); err != nil || !k.Since.Equal(keptBefore.Since) || k.Criterion != "5m impact" {
		t.Errorf("after a run: %+v %v", k, err)
	}
}

// verdict0 is what of a report the migration holds unchanged.
type verdict0 struct {
	rules, incidents, firings any
	noRecord                  bool
}
