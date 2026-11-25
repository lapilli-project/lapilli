package ledger

import (
	"context"
	"fmt"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/graph"
	"lapilli.dev/internal/incident"
)

// fillerGraph is the graph of edgesWorld at the hour of each incident, with
// filler pairs in every hour of a week: what a hull from the first incident
// to the last would load, and the incidents' own windows don't.
func fillerGraph(hours, perHour int, at ...time.Time) *graph.Graph {
	b := graph.NewBuilder()
	for h := range hours {
		for p := range perHour {
			b.Add(t0.Add(time.Duration(h)*time.Hour), fmt.Sprintf("filler-%d", p), fmt.Sprintf("filler-%d", p+1))
		}
	}
	for _, a := range at {
		b.Add(a, "checkout", "payments-db")
	}
	return b.Graph()
}

// Two incidents days apart load the edges of their own windows, not of the
// hours between them: a week of the graph that would be too big as a whole
// is judged by it, the first time and again. When the windows alone are too
// big, the judgement is made with none, says so (EdgesOver), and a judgement
// that loads it takes that away.
func TestJudgeLoadsIncidentWindows(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	at := func(h, m int) time.Time { return t0.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	in1 := incident.Incident{ID: "INC-1", Start: at(10, 5), End: at(10, 50), Labels: incident.Labels{"service": {"checkout"}}}
	in2 := incident.Incident{ID: "INC-2", Start: at(130, 5), End: at(130, 50), Labels: incident.Labels{"service": {"checkout"}}}
	fs := []firing.Firing{
		{Rule: "PaymentsDBConnections", Labels: map[string]string{"service": "payments-db"}, Start: at(10, 20), End: at(10, 40)},
		{Rule: "PaymentsDBConnections", Labels: map[string]string{"service": "payments-db"}, Start: at(130, 20), End: at(130, 40)},
	}
	// 168 hours of 10 filler pairs and the edge each incident asks for:
	// 1,680 and more edges, where each window has three hours of them.
	g := fillerGraph(168, 10, at(10, 0), at(130, 0))
	was := maxJudgeEdges
	maxJudgeEdges = 100
	t.Cleanup(func() { maxJudgeEdges = was })
	o := depended(opts, nil)
	if _, err := l.SyncFile(ctx, []incident.Incident{in1, in2}, o); err != nil {
		t.Fatal(err)
	}
	to := t0.Add(7 * 24 * time.Hour)
	if err := l.Record(ctx, Found{From: t0, To: to, Firings: fs, Edges: &EdgesRead{Graph: g, From: t0, To: to},
		Run: Run{Kind: "test", Started: to, Finished: to, From: t0, To: to}}, o); err != nil {
		t.Fatal(err)
	}
	via := func(when string, want bool) {
		t.Helper()
		for _, id := range []string{"INC-1", "INC-2"} {
			in, err := l.IncidentByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			got := false
			for _, c := range in.CaughtBy {
				got = got || c.Dependency != nil
			}
			if got != want {
				t.Errorf("%s: %s caught by dependency %v, want %v: %+v", when, id, got, want, in.CaughtBy)
			}
		}
	}
	via("first", true)
	if over, err := l.EdgesOver(ctx, t0, to); err != nil || over {
		t.Errorf("over after the first judgement: %v %v", over, err)
	}
	if err := l.JudgeAll(ctx, o, nil); err != nil {
		t.Fatal(err)
	}
	via("again", true)
	// A cap the windows pass: judged with no graph, and said.
	maxJudgeEdges = 30
	if err := l.JudgeAll(ctx, o, nil); err != nil {
		t.Fatal(err)
	}
	via("over the cap", false)
	if over, err := l.EdgesOver(ctx, t0, to); err != nil || !over {
		t.Errorf("not over after a judgement past the cap: %v %v", over, err)
	}
	if over, _ := l.EdgesOver(ctx, to.Add(time.Hour), to.Add(2*time.Hour)); over {
		t.Error("over for a period the judgement didn't cover")
	}
	// A judgement that covers it with the graph loaded takes it away.
	maxJudgeEdges = 100
	if err := l.JudgeAll(ctx, o, nil); err != nil {
		t.Fatal(err)
	}
	via("under it again", true)
	if over, _ := l.EdgesOver(ctx, t0, to); over {
		t.Error("still over after a judgement that loaded the graph")
	}
}

// What the ledger keeps is bounded in a day and in all: a read past either
// doesn't fit (EdgesFit), and edges are written many to a statement.
func TestEdgesFitAndBatches(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	wasDay, wasAll := maxEdgesPerDay, maxEdgesKept
	t.Cleanup(func() { maxEdgesPerDay, maxEdgesKept = wasDay, wasAll })
	// 2,500 edges, over three statements of 1,000.
	g := fillerGraph(1, 2500)
	to := t0.Add(2 * time.Hour)
	if err := l.KeepEdges(ctx, &EdgesRead{Graph: g, From: t0, To: to}, opts); err != nil {
		t.Fatal(err)
	}
	if rows, _, _ := l.EdgesKept(ctx); rows != 2500 {
		t.Fatalf("kept %d, want 2500", rows)
	}
	// Kept again, nothing is added.
	if err := l.KeepEdges(ctx, &EdgesRead{Graph: g, From: t0, To: to}, opts); err != nil {
		t.Fatal(err)
	}
	if rows, _, _ := l.EdgesKept(ctx); rows != 2500 {
		t.Errorf("kept again: %d, want 2500", rows)
	}
	maxEdgesPerDay = 2000
	if err := l.EdgesFit(ctx, &EdgesRead{Graph: g}, false); err != ErrEdgesPerDay {
		t.Errorf("a day over its bound: %v", err)
	}
	maxEdgesPerDay = 10_000
	maxEdgesKept = 4000
	if err := l.EdgesFit(ctx, &EdgesRead{Graph: g}, false); err != ErrEdgesKept {
		t.Errorf("past what is kept in all: %v", err)
	}
	if err := l.EdgesFit(ctx, &EdgesRead{Graph: g}, true); err != nil {
		t.Errorf("replacing what is kept: %v", err)
	}
	maxEdgesKept = 6000
	if err := l.EdgesFit(ctx, &EdgesRead{Graph: g}, false); err != nil {
		t.Errorf("under both: %v", err)
	}
}

// A read that ends in the middle of an hour keeps no span for it: the next
// run reads it. A span failed twice is a gap, which a read over it takes away.
func TestEdgeSpansPartialHourAndGaps(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	_, _, g := edgesWorld()
	if err := l.KeepEdges(ctx, &EdgesRead{Graph: g, From: t0, To: t0.Add(3*time.Hour + 20*time.Minute)}, opts); err != nil {
		t.Fatal(err)
	}
	_, spans, _ := l.EdgesKept(ctx)
	if len(spans) != 1 || !spans[0][1].Equal(t0.Add(3*time.Hour)) {
		t.Fatalf("spans %v, want one ending at the last whole hour", spans)
	}
	// Failed, tried again and failed: a gap.
	h := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Hour) }
	if err := l.EdgeFailed(ctx, h(10), h(12)); err != nil {
		t.Fatal(err)
	}
	if err := l.EdgeGaveUp(ctx, h(10)); err != nil {
		t.Fatal(err)
	}
	if r, _ := l.EdgeRetries(ctx); len(r) != 0 {
		t.Errorf("a gap is retried: %v", r)
	}
	if gaps, _ := l.EdgeGaps(ctx); len(gaps) != 1 || !gaps[0][0].Equal(h(10)) {
		t.Errorf("gaps %v", gaps)
	}
	if err := l.KeepEdges(ctx, &EdgesRead{Graph: g, From: h(9), To: h(13)}, opts); err != nil {
		t.Fatal(err)
	}
	if gaps, _ := l.EdgeGaps(ctx); len(gaps) != 0 {
		t.Errorf("a read over a gap leaves it: %v", gaps)
	}
}

// EdgesIn is counted once for a generation of the ledger and span, and
// counted again once the ledger changes.
func TestEdgesInCounted(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	_, _, g := edgesWorld()
	if err := l.KeepEdges(ctx, &EdgesRead{Graph: g, From: t0, To: t0.Add(5 * time.Hour)}, opts); err != nil {
		t.Fatal(err)
	}
	a, b := t0, t0.Add(5*time.Hour)
	n, err := l.EdgesIn(ctx, a, b)
	if err != nil || n != g.Edges() {
		t.Fatalf("%d %v, want %d", n, err, g.Edges())
	}
	if n2, _ := l.EdgesIn(ctx, a, b); n2 != n {
		t.Errorf("again: %d", n2)
	}
	more := graph.NewBuilder()
	more.Add(t0.Add(time.Hour), "a", "b")
	if err := l.KeepEdges(ctx, &EdgesRead{Graph: more.Graph(), From: t0, To: t0.Add(5 * time.Hour)}, opts); err != nil {
		t.Fatal(err)
	}
	if n3, _ := l.EdgesIn(ctx, a, b); n3 != n+1 {
		t.Errorf("after a new pair: %d, want %d", n3, n+1)
	}
}

// Prune lets go of edges five thousand at a time, and a span begins where
// the edges kept do.
func TestEdgesPrunedInBatches(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	g := fillerGraph(3, 6000)
	if err := l.KeepEdges(ctx, &EdgesRead{Graph: g, From: t0, To: t0.Add(10 * time.Hour)}, opts); err != nil {
		t.Fatal(err)
	}
	if rows, _, _ := l.EdgesKept(ctx); rows != 18000 {
		t.Fatalf("kept %d", rows)
	}
	if err := l.Prune(ctx, t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	rows, spans, _ := l.EdgesKept(ctx)
	if rows != 6000 || len(spans) != 1 || !spans[0][0].Equal(t0.Add(2*time.Hour)) {
		t.Errorf("after pruning: %d edges over %v, want 6000 over a span from the third hour", rows, spans)
	}
}

// How reading the graph went is kept in the ledger, and gone once it went
// well.
func TestEdgesStatusKept(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if st, err := l.EdgesStatus(ctx); err != nil || st != (EdgesStatus{}) {
		t.Fatalf("status of a fresh ledger: %+v %v", st, err)
	}
	want := EdgesStatus{Key: "k", Error: "the history failed", At: t0, Tries: 2, Next: t0.Add(2 * time.Hour)}
	if err := l.SetEdgesStatus(ctx, want); err != nil {
		t.Fatal(err)
	}
	if st, _ := l.EdgesStatus(ctx); st != want {
		t.Errorf("status %+v, want %+v", st, want)
	}
	if err := l.SetEdgesStatus(ctx, EdgesStatus{}); err != nil {
		t.Fatal(err)
	}
	if st, _ := l.EdgesStatus(ctx); st != (EdgesStatus{}) {
		t.Errorf("after a read that went well: %+v", st)
	}
}
