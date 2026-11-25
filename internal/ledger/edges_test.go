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
	"lapilli.dev/internal/graph"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger/ledgertest"
	"lapilli.dev/internal/verdict"
)

// edgesWorld is checkout calling payments-db, an incident in checkout, and
// a firing on payments-db that came before it.
func edgesWorld() (fs []firing.Firing, in incident.Incident, g *graph.Graph) {
	at := func(m int) time.Time { return t0.Add(2*time.Hour + time.Duration(m)*time.Minute) }
	fs = []firing.Firing{
		{Rule: "PaymentsDBConnections", Labels: map[string]string{"service": "payments-db"}, Start: at(-5), End: at(30)},
		{Rule: "CheckoutErrors", Labels: map[string]string{"service": "checkout"}, Start: at(2), End: at(20)},
		{Rule: "SearchSlow", Labels: map[string]string{"service": "search"}, Start: at(1), End: at(20)},
	}
	in = incident.Incident{ID: "INC-9", Start: at(0), End: at(40), Labels: incident.Labels{"service": {"checkout"}}}
	b := graph.NewBuilder()
	b.Add(at(0), "checkout", "payments-db")
	b.Add(at(0), "search", "checkout")
	return fs, in, b.Graph()
}

func depended(o verdict.Options, g *graph.Graph) verdict.Options {
	o.Match = []string{"service"}
	o.Depends = &verdict.Dependencies{Graph: g, Label: "service", Depth: 1, Direction: verdict.DependsDown}
	return o
}

// The ledger keeps the graph a run reads, judges by it as verdict.Judge
// does, writes the path a rule caught an incident by, and, judged again
// with the history away, says the same.
func TestEdgesKeptAndJudgedAgain(t *testing.T) {
	ctx := context.Background()
	fs, in, g := edgesWorld()
	l := open(t, time.UTC)
	o := depended(opts, nil) // the ledger gives the graph it holds
	incs := []incident.Incident{keepAll, in}
	if _, err := l.SyncFile(ctx, incs, o); err != nil {
		t.Fatal(err)
	}
	to := t0.Add(7 * 24 * time.Hour)
	f := Found{From: t0, To: to, Firings: fs, Edges: &EdgesRead{Graph: g, From: t0, To: to},
		Run: Run{Kind: "test", Started: to, Finished: to, From: t0, To: to}}
	if err := l.Record(ctx, f, o); err != nil {
		t.Fatal(err)
	}
	if rows, spans, err := l.EdgesKept(ctx); err != nil || rows != int64(g.Rows()) || len(spans) != 1 {
		t.Fatalf("kept %d edges over %v, want %d over one span: %v", rows, spans, g.Rows(), err)
	}
	want := verdict.Judge(nil, slices.Clone(fs), incs, func() verdict.Options { w := depended(opts, g); w.From, w.To = t0, to; return w }())
	check := func(when string) {
		t.Helper()
		kept, err := l.IncidentByID(ctx, "INC-9")
		if err != nil {
			t.Fatal(err)
		}
		var via []string
		for _, c := range kept.CaughtBy {
			if c.Dependency != nil {
				if c.Rule != "PaymentsDBConnections" {
					t.Errorf("%s: %s caught by dependency", when, c.Rule)
				}
				via = c.Dependency
			}
		}
		if !slices.Equal(via, []string{"checkout", "payments-db"}) {
			t.Errorf("%s: dependency %v, want checkout > payments-db; caught by %+v", when, via, kept.CaughtBy)
		}
		o.From, o.To = t0, to
		got, err := l.Report(ctx, o.From, o.To, o, true)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Incidents, want.Incidents) {
			t.Errorf("%s: incidents\n got %+v\nwant %+v", when, got.Incidents, want.Incidents)
		}
	}
	check("first")
	if err := l.JudgeAll(ctx, o, nil); err != nil {
		t.Fatal(err)
	}
	check("again")
	// Without the graph, no catch is by dependency.
	if err := l.JudgeAll(ctx, opts, nil); err != nil {
		t.Fatal(err)
	}
	kept, _ := l.IncidentByID(ctx, "INC-9")
	for _, c := range kept.CaughtBy {
		if c.Dependency != nil {
			t.Errorf("judged with no graph: %s caught by dependency %v", c.Rule, c.Dependency)
		}
	}
}

// A graph that couldn't be read is kept as a span to read once, and one
// read over a span before is made one with it.
func TestEdgeRetries(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	h := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Hour) }
	for _, s := range [][2]int{{0, 2}, {2, 4}, {8, 9}} {
		if err := l.EdgeFailed(ctx, h(s[0]), h(s[1])); err != nil {
			t.Fatal(err)
		}
	}
	got, err := l.EdgeRetries(ctx)
	if err != nil || len(got) != 2 || !got[0][0].Equal(h(0)) || !got[0][1].Equal(h(4)) {
		t.Fatalf("retries (touching spans are one): %v %v", got, err)
	}
	_, _, g := edgesWorld()
	if err := l.KeepEdges(ctx, &EdgesRead{Graph: g, From: h(0), To: h(4)}, opts); err != nil {
		t.Fatal(err)
	}
	if err := l.EdgeRetried(ctx, got[0][0]); err != nil {
		t.Fatal(err)
	}
	if got, _ = l.EdgeRetries(ctx); len(got) != 1 || !got[0][0].Equal(h(8)) {
		t.Errorf("retries after one was read again: %v", got)
	}
	if rows, spans, _ := l.EdgesKept(ctx); rows != int64(g.Rows()) || len(spans) != 1 {
		t.Errorf("kept %d over %v", rows, spans)
	}
	// A graph with too many edges in an hour is kept as none, over no span.
	over := graph.NewBuilder()
	over.Over()
	if err := l.KeepEdges(ctx, &EdgesRead{Graph: over.Graph(), From: h(10), To: h(12)}, opts); err != nil {
		t.Fatal(err)
	}
	if _, spans, _ := l.EdgesKept(ctx); len(spans) != 1 {
		t.Errorf("a graph past the bound kept a span: %v", spans)
	}
}

// A new key drops what the ledger held of the graph, and what is past the
// firings' retention goes with them.
func TestEdgesResetAndPruned(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	_, _, g := edgesWorld()
	if err := l.KeepEdges(ctx, &EdgesRead{Graph: g, From: t0, To: t0.Add(5 * time.Hour)}, opts); err != nil {
		t.Fatal(err)
	}
	if err := l.Prune(ctx, t0.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if rows, spans, _ := l.EdgesKept(ctx); rows != 0 || len(spans) != 0 {
		t.Errorf("after pruning: %d edges, spans %v", rows, spans)
	}
	if err := l.KeepEdges(ctx, &EdgesRead{Graph: g, From: t0, To: t0.Add(5 * time.Hour)}, opts); err != nil {
		t.Fatal(err)
	}
	if err := l.ReplaceEdges(ctx, "k1", nil); err != nil {
		t.Fatal(err)
	}
	if k, _ := l.EdgesKey(ctx); k != "k1" {
		t.Errorf("key %q", k)
	}
	if rows, spans, _ := l.EdgesKept(ctx); rows != 0 || len(spans) != 0 {
		t.Errorf("after a reset: %d edges, spans %v", rows, spans)
	}
}

// A ledger migrated from 035 keeps its catches, with no dependency, and
// keeps edges after.
func TestMigrate036(t *testing.T) {
	ctx := context.Background()
	where := place(t)
	l, err := openAt(ctx, where, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	fs, in, _ := edgesWorld()
	o := depended(opts, nil)
	if _, err := l.SyncFile(ctx, []incident.Incident{keepAll, in}, o); err != nil {
		t.Fatal(err)
	}
	to := t0.Add(7 * 24 * time.Hour)
	if err := l.Record(ctx, Found{From: t0, To: to, Firings: fs, Run: Run{Kind: "test", Started: to, Finished: to, From: t0, To: to}}, o); err != nil {
		t.Fatal(err)
	}
	l.Close()
	back := ledgertest.Undo036
	if strings.HasPrefix(where, "postgres") {
		c, err := pgx.Connect(ctx, where)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range append(slices.Clone(back), "UPDATE schema_version SET version = 35") {
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
		for _, q := range append(slices.Clone(back), "PRAGMA user_version = 35") {
			if _, err := l.w.ExecContext(ctx, q); err != nil {
				t.Fatal(q, err)
			}
		}
		l.Close()
	}
	l2, err := openAt(ctx, where, time.UTC)
	if err != nil {
		t.Fatalf("migrating to 036: %v", err)
	}
	defer l2.Close()
	kept, err := l2.IncidentByID(ctx, "INC-9")
	if err != nil || len(kept.CaughtBy) == 0 {
		t.Fatalf("catches after migrating: %+v %v", kept.CaughtBy, err)
	}
	for _, c := range kept.CaughtBy {
		if c.Dependency != nil {
			t.Errorf("%s has a dependency after migrating: %v", c.Rule, c.Dependency)
		}
	}
	_, _, g := edgesWorld()
	if err := l2.KeepEdges(ctx, &EdgesRead{Graph: g, From: t0, To: to}, o); err != nil {
		t.Fatal(err)
	}
	if rows, _, _ := l2.EdgesKept(ctx); rows != int64(g.Rows()) {
		t.Errorf("edges after migrating: %d", rows)
	}
}
