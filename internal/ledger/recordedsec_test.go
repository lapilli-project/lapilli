package ledger

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
)

// What a token can't send: a period or a firing in the future, and a name
// with a character that doesn't show, which could pass for another rule's
// or break a line where a reader sees none.
func TestRecordedRefusedMore(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	now := time.Now().Truncate(time.Hour)
	from, to := now.Add(-24*time.Hour), now.Add(24*time.Hour)
	at := now.Add(-time.Hour)
	for _, c := range []struct {
		from, to time.Time
		f        firing.Firing
		says     string
	}{
		{now.Add(2 * time.Hour), now.Add(3 * time.Hour), firing.Firing{Rule: "A", Start: now.Add(2 * time.Hour)}, "can't start in the future"},
		{from, to, firing.Firing{Rule: "A", Start: now.Add(2 * time.Hour)}, "starts in the future"},
		{from, to, firing.Firing{Rule: "A", Start: at, End: now.Add(2 * time.Hour)}, "ends in the future"},
		{from, to, firing.Firing{Rule: "A", Start: at, End: time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)}, "ends in the future"},
		{from, to, firing.Firing{Rule: "HighErrorRate​", Start: at}, "doesn't show"},
		{from, to, firing.Firing{Rule: "Disk\nTOTAL 0 incidents missed", Start: at}, "doesn't show"},
		{from, to, firing.Firing{Rule: "‮gnp.exe", Start: at}, "doesn't show"},
		{from, to, firing.Firing{Rule: "A", Start: at, Labels: map[string]string{"severity": "critical​"}}, "doesn't show"},
		{from, to, firing.Firing{Rule: "A", Start: at, Labels: map[string]string{"svc\x1b[2J": "x"}}, "doesn't show"},
	} {
		_, err := l.PutFirings(ctx, "cw", c.from, c.to, []firing.Firing{c.f}, FiringsCopier, opts)
		var inv ErrInvalid
		if !errors.As(err, &inv) || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%q from %s: %v, want one saying %q", c.f.Rule, c.f.Start, err, c.says)
		}
	}
	// Within the skew, and on still: taken.
	putFirings(t, l, "cw", from, to, []firing.Firing{{Rule: "A", Start: now.Add(30 * time.Minute)}, {Rule: "B", Start: at, End: now.Add(30 * time.Minute)}})
}

// One firing sent with a far end judges no further than a period past its
// period: otherwise each such write would judge the whole ledger again.
func TestRecordedJudgesNear(t *testing.T) {
	l := open(t, time.UTC)
	from := t0.Add(-200 * 24 * time.Hour)
	to := from.Add(24 * time.Hour)
	var furthest int64
	judging = func(lo, hi int64) { furthest = max(furthest, hi) }
	defer func() { judging = nil }()
	putFirings(t, l, "cw", from, to, []firing.Firing{{Rule: "A", Start: from.Add(time.Hour), End: from.Add(150 * 24 * time.Hour)}})
	if reach := to.Add(MaxRecordedPeriod + opts.RefireWithin); furthest == 0 || furthest > reach.Unix() {
		t.Errorf("judged to %s, want no further than %s", time.Unix(furthest, 0).UTC(), reach.UTC())
	}
	// Taken away again, as far.
	furthest = 0
	putFirings(t, l, "cw", from, to, nil)
	if reach := to.Add(MaxRecordedPeriod + opts.RefireWithin); furthest == 0 || furthest > reach.Unix() {
		t.Errorf("taking it away judged to %s, want no further than %s", time.Unix(furthest, 0).UTC(), reach.UTC())
	}
}

// A token can't add rules and alerts without end: each source has so many,
// and all sources so many rules; a write past them changes nothing.
func TestRecordedBounded(t *testing.T) {
	ctx := context.Background()
	defer func(r, a, s int) { maxSourceRules, maxRecordedRules, maxSourceSeries = r, a, s }(maxSourceRules, maxRecordedRules, maxSourceSeries)
	maxSourceRules, maxRecordedRules, maxSourceSeries = 3, 5, 4
	l := open(t, time.UTC)
	from, to := t0, t0.Add(24*time.Hour)
	at := t0.Add(time.Hour)
	fs := func(rules ...string) []firing.Firing {
		var out []firing.Firing
		for _, r := range rules {
			out = append(out, firing.Firing{Rule: r, Start: at})
		}
		return out
	}
	putFirings(t, l, "a", from, to, fs("A1", "A2", "A3"))
	refused := func(source string, f []firing.Firing, says string) {
		t.Helper()
		_, err := l.PutFirings(ctx, source, from, to, f, FiringsCopier, opts)
		var inv ErrInvalid
		if !errors.As(err, &inv) || !strings.Contains(err.Error(), says) {
			t.Errorf("%s: %v, want one saying %q", source, err, says)
		}
	}
	refused("a", fs("A1", "A2", "A3", "A4"), "3 rules at most")
	putFirings(t, l, "b", from, to, fs("B1", "B2"))
	refused("c", fs("C1"), "5 rules copied in at most")
	// Alerts: a rule and its labels each.
	var many []firing.Firing
	for _, svc := range []string{"w", "x", "y", "z", "v"} {
		many = append(many, firing.Firing{Rule: "A1", Start: at, Labels: map[string]string{"svc": svc}})
	}
	refused("a", many, "4 alerts at most")
	srcs, err := l.Sources(ctx)
	if err != nil || len(srcs) != 2 || srcs[0].Rules != 3 || srcs[1].Rules != 2 {
		t.Errorf("refused writes changed the sources: %+v %v", srcs, err)
	}
	// What is there may be written again.
	putFirings(t, l, "a", from, to, fs("A1", "A2", "A3"))
}

// A note or a task can be written for a rule before it is. A rule copied in
// under that name doesn't take it, so it can't carry the note off when the
// rule comes to be replayed.
func TestRecordedLeavesNotesWaiting(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if err := l.PutNote(ctx, Note{Rule: "CheckoutLatencyHigh", Runbook: "https://runbooks.example/checkout"}, "ana"); err != nil {
		t.Fatal(err)
	}
	one := []firing.Firing{{Rule: "CheckoutLatencyHigh", Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)}}
	putFirings(t, l, "evil", t0, t0.Add(3*time.Hour), one)
	record(t, l, t0.Add(3*time.Hour), t0.Add(6*time.Hour), []firing.Firing{{Rule: "CheckoutLatencyHigh", Start: t0.Add(4 * time.Hour)}})
	notes, err := l.Notes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if notes["CheckoutLatencyHigh"].Runbook == "" || notes["CheckoutLatencyHigh [evil]"].Runbook != "" {
		t.Errorf("the note went to %+v", notes)
	}
	table, err := l.RuleTable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range table {
		names = append(names, r.Name+" <"+r.Source+">")
	}
	slices.Sort(names)
	if strings.Join(names, ", ") != "CheckoutLatencyHigh <>, CheckoutLatencyHigh [evil] <evil>" {
		t.Errorf("rules: %q", names)
	}
}

// A source is as far as the furthest period it has written: writing an
// older period again doesn't move it back, so the digest that waits for it
// can't be held back by a source writing 1970 each hour.
func TestRecordedFurthest(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	day := 24 * time.Hour
	putFirings(t, l, "cw", t0.Add(day), t0.Add(2*day), []firing.Firing{{Rule: "A", Start: t0.Add(day + time.Hour)}})
	putFirings(t, l, "cw", t0.Add(-100*day), t0.Add(-99*day), nil)
	srcs, err := l.Sources(ctx)
	if err != nil || len(srcs) != 1 || !srcs[0].To.Equal(t0.Add(2*day)) || !srcs[0].From.Equal(t0.Add(day)) || srcs[0].Firings != 1 {
		t.Errorf("sources: %+v %v", srcs, err)
	}
}

// The queries that find a source's rule use the index of recorded rules:
// with tens of thousands of rules, a scan for each of a write's would make
// a write's cost grow with the square of them.
func TestRecordedQueriesUseTheIndex(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	for _, q := range []string{qRecordedRule, qRecordedTwin} {
		rows, err := l.r.QueryContext(ctx, "EXPLAIN QUERY PLAN "+q, "cw", "A", "")
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		if p := strings.Join(plan, "; "); !strings.Contains(p, "rules_recorded") {
			t.Errorf("%s\nplan: %s", q, p)
		}
	}
}
