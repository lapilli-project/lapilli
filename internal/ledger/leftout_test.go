package ledger

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"lapilli.dev/internal/firing"
)

// hours are spans, as hours from t0.
func hours(spans [][2]time.Time) [][2]int {
	out := [][2]int{}
	for _, s := range spans {
		out = append(out, [2]int{int(s[0].Sub(t0) / time.Hour), int(s[1].Sub(t0) / time.Hour)})
	}
	return out
}

// The periods a rule was left out of its source's copies are kept by rule,
// those that meet or overlap as one, and taken out of what the copies
// cover of that rule alone: no record there, also once the copies go on
// past them.
func TestLeftOut(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	h := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Hour) }
	putFirings(t, l, GrafanaSource, h(0), h(24), []firing.Firing{{Rule: "Orders slow", Start: h(1), End: h(2)}})
	leave := func(from, to int, rules ...string) {
		t.Helper()
		if err := l.LeaveOut(ctx, GrafanaSource, rules, nil, h(from), h(to)); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := l.LeftOut(ctx, GrafanaSource); err != nil || len(out) != 0 {
		t.Errorf("none left out: %v %v", out, err)
	}
	leave(2, 4, "Orders slow", "Stock low", "Orders slow")
	leave(4, 6, "Orders slow") // meets the first
	leave(3, 5, "Orders slow") // within them
	leave(10, 12, "Orders slow")
	out, err := l.LeftOut(ctx, GrafanaSource)
	if err != nil || !reflect.DeepEqual(hours(out["Orders slow"]), [][2]int{{2, 6}, {10, 12}}) || !reflect.DeepEqual(hours(out["Stock low"]), [][2]int{{2, 4}}) || len(out) != 2 {
		t.Errorf("left out: %v %v", out, err)
	}
	// Kept already, nothing is written.
	gen := l.Gen()
	leave(2, 6, "Orders slow")
	leave(11, 12, "Orders slow", "Orders slow")
	if l.Gen() != gen {
		t.Errorf("a period kept already was written again")
	}
	for rule, want := range map[string][][2]int{
		"Orders slow":   {{0, 2}, {6, 10}, {12, 24}},
		"Stock low":     {{0, 2}, {4, 24}}, // never copied in: by the name it would be sent by
		"Payments late": {{0, 24}},
	} {
		spans, err := l.SpansOf(ctx, GrafanaSource, rule)
		if err != nil || !reflect.DeepEqual(hours(spans), want) {
			t.Errorf("%s: its copies cover %v, not %v: %v", rule, hours(spans), want, err)
		}
	}
	if from, to, err := l.CoverageOf(ctx, GrafanaSource, "Orders slow"); err != nil || !from.Equal(h(12)) || !to.Equal(h(24)) {
		t.Errorf("Orders slow covered %s to %s: %v", from, to, err)
	}
	// Left out up to where the copies reach: covered up to where that
	// began. And no further, the copies going on past it.
	leave(20, 24, "Orders slow")
	if from, to, err := l.CoverageOf(ctx, GrafanaSource, "Orders slow"); err != nil || !from.Equal(h(12)) || !to.Equal(h(20)) {
		t.Errorf("Orders slow, left out now, covered %s to %s: %v", from, to, err)
	}
	putFirings(t, l, GrafanaSource, h(24), h(48), nil)
	if spans, err := l.SpansOf(ctx, GrafanaSource, "Orders slow"); err != nil || !reflect.DeepEqual(hours(spans), [][2]int{{0, 2}, {6, 10}, {12, 20}, {24, 48}}) {
		t.Errorf("the copies gone on: %v %v", hours(spans), err)
	}
	// Another source's rule of the name isn't left out of its own.
	putFirings(t, l, "cw", h(0), h(24), []firing.Firing{{Rule: "Orders slow", Start: h(3), End: h(4)}})
	if spans, err := l.SpansOf(ctx, "cw", "Orders slow [cw]"); err != nil || !reflect.DeepEqual(hours(spans), [][2]int{{0, 24}}) {
		t.Errorf("another source's rule: %v %v", hours(spans), err)
	}
	// What isn't kept.
	for what, err := range map[string]error{
		"a name that doesn't show": l.LeaveOut(ctx, GrafanaSource, []string{"Orders​slow"}, nil, h(0), h(1)),
		"no name":                  l.LeaveOut(ctx, GrafanaSource, []string{""}, nil, h(0), h(1)),
		"a long name":              l.LeaveOut(ctx, GrafanaSource, []string{strings.Repeat("x", 513)}, nil, h(0), h(1)),
		"no source":                l.LeaveOut(ctx, "Not A Source", []string{"A"}, nil, h(0), h(1)),
		"a period backwards":       l.LeaveOut(ctx, GrafanaSource, []string{"A"}, nil, h(1), h(1)),
		"a part of a second":       l.LeaveOut(ctx, GrafanaSource, []string{"A"}, nil, h(0), h(1).Add(time.Millisecond)),
	} {
		if !errors.Is(err, ErrLeftOut) {
			t.Errorf("%s: %v", what, err)
		}
	}
	if RuleName("Orders​slow") || RuleName("") || !RuleName("Orders slow") {
		t.Errorf("RuleName")
	}
	// Dropped with the firings beside them, once over before --keep.
	if err := l.Prune(ctx, h(7)); err != nil {
		t.Fatal(err)
	}
	out, _ = l.LeftOut(ctx, GrafanaSource)
	if !reflect.DeepEqual(hours(out["Orders slow"]), [][2]int{{10, 12}, {20, 24}}) || len(out["Stock low"]) != 0 {
		t.Errorf("after pruning: %v", out)
	}
}

// A rule's periods are bounded: past as many as are kept, the two earliest
// are run together, so that nothing left out reads as copied; and so are
// the rules of a source that have any, past which no more is kept, and it
// is said.
func TestLeftOutBounds(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	h := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Hour) }
	for i := range maxLeftOut + 2 {
		if err := l.LeaveOut(ctx, GrafanaSource, []string{"Orders slow"}, nil, h(2*i), h(2*i+1)); err != nil {
			t.Fatal(err)
		}
	}
	out, err := l.LeftOut(ctx, GrafanaSource)
	ps := hours(out["Orders slow"])
	if err != nil || len(ps) != maxLeftOut || ps[0] != [2]int{0, 5} || ps[1] != [2]int{6, 7} || ps[len(ps)-1] != [2]int{2*maxLeftOut + 2, 2*maxLeftOut + 3} {
		t.Errorf("%d periods, the first %v: %v", len(ps), ps[:min(2, len(ps))], err)
	}
	defer func(n int) { maxLeftOutRules = n }(maxLeftOutRules)
	maxLeftOutRules = 3
	if err := l.LeaveOut(ctx, GrafanaSource, []string{"B", "C"}, nil, h(0), h(1)); err != nil {
		t.Fatal(err)
	}
	if err := l.LeaveOut(ctx, GrafanaSource, []string{"B", "D"}, nil, h(1), h(2)); !errors.Is(err, ErrLeftOutFull) {
		t.Errorf("a fourth rule: %v", err)
	}
	// Nothing of a write refused is kept, and those kept go on.
	if out, _ := l.LeftOut(ctx, GrafanaSource); len(out) != 3 || !reflect.DeepEqual(hours(out["B"]), [][2]int{{0, 1}}) {
		t.Errorf("after a write refused: %v", out)
	}
	if err := l.LeaveOut(ctx, GrafanaSource, []string{"B", "C"}, nil, h(1), h(2)); err != nil {
		t.Errorf("rules kept already: %v", err)
	}
	// The rules no longer the source's let go of, those whose last period
	// ended earliest first, as many as make room: B's, which ended before
	// C's.
	if err := l.LeaveOut(ctx, GrafanaSource, []string{"C"}, nil, h(2), h(3)); err != nil {
		t.Fatal(err)
	}
	if err := l.LeaveOut(ctx, GrafanaSource, []string{"D"}, []string{"Orders slow"}, h(3), h(4)); err != nil {
		t.Errorf("a fourth rule, the source's rules known: %v", err)
	}
	if out, _ := l.LeftOut(ctx, GrafanaSource); len(out) != 3 || out["B"] != nil || out["C"] == nil || out["D"] == nil {
		t.Errorf("after letting go of one: %v", out)
	}
	// None to let go of: refused.
	if err := l.LeaveOut(ctx, GrafanaSource, []string{"E"}, []string{"Orders slow", "C", "D"}, h(3), h(4)); !errors.Is(err, ErrLeftOutFull) {
		t.Errorf("a fourth rule, the others the source's: %v", err)
	}
	// Another source's are counted apart.
	if err := l.LeaveOut(ctx, "cw", []string{"D"}, nil, h(1), h(2)); err != nil {
		t.Errorf("another source's rule: %v", err)
	}
}

// What a tie says of a rule judged from Grafana's history is what the
// copies cover of that rule: not a period it was left out of them, nor
// what came before one.
func TestTiedGrafanaHistoryLeftOut(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	h := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Hour) }
	if err := l.SetFromHistory(ctx, []string{"Orders slow", "Stock low"}); err != nil {
		t.Fatal(err)
	}
	grafanaVersion(t, l, t0, "Orders slow", "Stock low")
	f := Found{From: t0, To: h(9), Rules: []Rule{{Name: "Orders slow", Unsupported: "its condition reads a mysql data source"},
		{Name: "Stock low", Unsupported: "its condition reads a mysql data source"}, {Name: "Replayed"}},
		Run: Run{Kind: "test", Started: h(9), Finished: h(9), From: t0, To: h(9)}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	putFirings(t, l, GrafanaSource, h(0), h(9), []firing.Firing{{Rule: "Orders slow", Start: h(1), End: h(2)}, {Rule: "Stock low", Start: h(1), End: h(2)}})
	// Each rule's tie, of every rule's and of its own alone (TieOf), which
	// reads its periods alone.
	ties := func(rule string, since, to int) {
		t.Helper()
		ts, err := l.Ties(ctx)
		if x := ts[rule]; err != nil || x.Source != GrafanaSource || x.Since != h(since).Unix() || x.To != h(to).Unix() {
			t.Errorf("%s: since %s to %s, not %d to %d: %v", rule, unix(x.Since).Sub(t0), unix(x.To).Sub(t0), since, to, err)
		}
		if x, ok, err := l.TieOf(ctx, rule); err != nil || !ok || x != ts[rule] {
			t.Errorf("%s alone: %+v %v %v, of all %+v", rule, x, ok, err, ts[rule])
		}
	}
	ties("Orders slow", 0, 9)
	if err := l.LeaveOut(ctx, GrafanaSource, []string{"Orders slow"}, nil, h(3), h(6)); err != nil {
		t.Fatal(err)
	}
	ties("Orders slow", 6, 9)
	ties("Stock low", 0, 9)
	// Left out up to where the copies reach.
	if err := l.LeaveOut(ctx, GrafanaSource, []string{"Orders slow"}, nil, h(6), h(9)); err != nil {
		t.Fatal(err)
	}
	ties("Orders slow", 0, 3)
	// Left out of all of them: tied, with no record.
	if err := l.LeaveOut(ctx, GrafanaSource, []string{"Orders slow"}, nil, h(0), h(3)); err != nil {
		t.Fatal(err)
	}
	if ts, err := l.Ties(ctx); err != nil || ts["Orders slow"].Source != GrafanaSource || ts["Orders slow"].Since != 0 || ts["Orders slow"].To != 0 {
		t.Errorf("left out of all the copies: %+v %v", ts["Orders slow"], err)
	}
	ties("Stock low", 0, 9)
}

// AmendFirings writes a period again without taking it as one the copies
// cover: a firing on in a span the ledger let go is ended where it is.
func TestAmendFirings(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	h := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Hour) }
	on := firing.Firing{Rule: "Orders slow", Labels: map[string]string{"service": "orders"}, Start: h(1)}
	other := firing.Firing{Rule: "Orders slow", Labels: map[string]string{"service": "search"}, Start: h(1), End: h(2)}
	putFirings(t, l, GrafanaSource, h(0), h(3), []firing.Firing{on, other})
	putFirings(t, l, GrafanaSource, h(5), h(8), nil)
	srcs, _ := l.Sources(ctx)
	on.End = h(6)
	got, err := l.AmendFirings(ctx, GrafanaSource, h(1), h(1).Add(time.Second), []firing.Firing{on, other}, GrafanaCopier, opts)
	if err != nil || got.Added != 1 || got.Removed != 1 {
		t.Fatalf("amended: %+v %v", got, err)
	}
	if open, err := l.OpenFirings(ctx, GrafanaSource, h(9)); err != nil || len(open) != 0 {
		t.Errorf("still on: %+v %v", open, err)
	}
	fs, err := l.RecordedFirings(ctx, GrafanaSource, h(0), h(9))
	if err != nil || len(fs) != 2 || !slices.ContainsFunc(fs, func(f firing.Firing) bool { return f.Labels["service"] == "orders" && f.End.Equal(h(6)) }) {
		t.Errorf("firings: %+v %v", fs, err)
	}
	spans, err := l.Spans(ctx, GrafanaSource)
	if err != nil || !reflect.DeepEqual(hours(spans), [][2]int{{0, 3}, {5, 8}}) {
		t.Errorf("the copies cover %v: %v", hours(spans), err)
	}
	if after, _ := l.Sources(ctx); !reflect.DeepEqual(after, srcs) {
		t.Errorf("the source noted: %+v, not %+v", after, srcs)
	}
}

// A ledger at 029, with Grafana's history copied in, in SQLite or, with
// LAPILLI_TEST_POSTGRES, in Postgres, migrates to 030 with its firings and
// what its copies cover unchanged and no rule left out; then a period is
// kept, and read.
func TestMigrate030(t *testing.T) {
	ctx := context.Background()
	where := place(t)
	l, err := openAt(ctx, where, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	h := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Hour) }
	putFirings(t, l, GrafanaSource, h(0), h(24), []firing.Firing{
		{Rule: "Orders slow", Labels: map[string]string{"service": "orders"}, Start: h(5), End: h(6)},
		{Rule: "Stock low", Labels: map[string]string{"service": "stock"}, Start: h(7)}})
	read := func(l *Ledger) string {
		t.Helper()
		fs, err := l.RecordedFirings(ctx, GrafanaSource, h(0), h(24))
		if err != nil {
			t.Fatal(err)
		}
		firing.Sort(fs)
		spans, err := l.Spans(ctx, GrafanaSource)
		if err != nil {
			t.Fatal(err)
		}
		rep, err := l.Report(ctx, h(0), h(24), opts, true)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%+v %v %+v", fs, hours(spans), rep.Rules)
	}
	was := read(l)
	l.Close()

	// Back to 029, as 0.68.1 left it.
	if strings.HasPrefix(where, "postgres") {
		c, err := pgx.Connect(ctx, where)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range append(slices.Concat(undo031, undo030), "UPDATE schema_version SET version = 29") {
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
		for _, q := range append(slices.Concat(undo031, undo030), "PRAGMA user_version = 29") {
			if _, err := l.w.ExecContext(ctx, q); err != nil {
				t.Fatal(q, err)
			}
		}
		l.Close()
	}

	l2, err := openAt(ctx, where, time.UTC)
	if err != nil {
		t.Fatalf("migrating to 030: %v", err)
	}
	defer l2.Close()
	if got := read(l2); got != was {
		t.Errorf("after migrating:\n%s\nnot\n%s", got, was)
	}
	if out, err := l2.LeftOut(ctx, GrafanaSource); err != nil || len(out) != 0 {
		t.Errorf("left out after migrating: %v %v", out, err)
	}
	if err := l2.LeaveOut(ctx, GrafanaSource, []string{"Orders slow"}, nil, h(8), h(12)); err != nil {
		t.Fatalf("a period after migrating: %v", err)
	}
	if spans, err := l2.SpansOf(ctx, GrafanaSource, "Orders slow"); err != nil || !reflect.DeepEqual(hours(spans), [][2]int{{0, 8}, {12, 24}}) {
		t.Errorf("after migrating, Orders slow's copies cover %v: %v", hours(spans), err)
	}
	if spans, err := l2.SpansOf(ctx, GrafanaSource, "Stock low"); err != nil || !reflect.DeepEqual(hours(spans), [][2]int{{0, 24}}) {
		t.Errorf("after migrating, Stock low's copies cover %v: %v", hours(spans), err)
	}
}
