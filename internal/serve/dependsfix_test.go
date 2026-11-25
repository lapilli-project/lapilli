package serve

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// A run whose read of the graph fails keeps why in the ledger, for the other
// replicas to show, and the span to be read once more; the incident is judged
// by what the ledger holds meanwhile. A read that goes well clears the reason.
func TestDependsRunReadFails(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s := dependsServer(t, onCalls)
	if st, _ := s.l.EdgesStatus(ctx); st.Error != "" {
		t.Fatalf("a read that went well left %+v", st)
	}
	hist := s.h.(*history)
	fails := &graphFlaky{history: hist, failing: map[string]*atomic.Int32{"calls": failAlways()}}
	s.h = fails
	asked := fails.tried("calls")
	was := s.now()
	s.now = func() time.Time { return was.Add(3 * time.Hour) }
	s.jobs(ctx, true)
	if fails.tried("calls") == asked {
		t.Fatal("the run never read the graph")
	}
	st, err := s.l.EdgesStatus(ctx)
	if err != nil || !strings.Contains(st.Error, "the history failed") || st.At.IsZero() {
		t.Errorf("the reason wasn't kept in the ledger: %+v %v", st, err)
	}
	if r, _ := s.l.EdgeRetries(ctx); len(r) == 0 {
		t.Error("the span that failed wasn't kept to be read again")
	}
	if got := viaOf(t, s); !slices.Equal(got, []string{"checkout", "payments-db"}) {
		t.Errorf("judged by what the ledger holds: caught via %v", got)
	}
	// Another replica's page says why.
	_, body := askAs(t, s.Handler(), http.MethodGet, "/settings", nil, "ana@example.com", "none")
	if !strings.Contains(body, "the history failed") {
		t.Errorf("the Settings page doesn't say why:\n%s", body)
	}
	// The history well again: read, and what was kept of the failure is gone.
	fails.failing["calls"].Store(0)
	s.now = func() time.Time { return was.Add(6 * time.Hour) }
	s.jobs(ctx, true)
	if st, _ := s.l.EdgesStatus(ctx); st.Error != "" {
		t.Errorf("after a read that went well: %+v", st)
	}
}

// A read over the last four weeks that fails isn't tried every round: again
// an hour later, then two, up to a day, "Next" on the page, within tryWithin;
// the weeks that read are kept and those that didn't are read once more.
func TestDependsAgainBacksOff(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s := dependsServer(t, DependsConfig{})
	hist := s.h.(*history)
	if err := s.l.SetSettings(ctx, map[string]string{"depends": "calls", "depends-labels": "caller,callee"}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	fails := &graphFlaky{history: hist, failing: map[string]*atomic.Int32{"calls": failAlways()}}
	s.h = fails
	at := s.now()
	read := func() int { return fails.tried("calls") }
	s.dependsAgain(ctx)
	n := read()
	if n == 0 {
		t.Fatal("not read")
	}
	st, _ := s.l.EdgesStatus(ctx)
	if st.Tries != 1 || !st.Next.Equal(at.Add(time.Hour)) {
		t.Fatalf("after one failure: %+v", st)
	}
	if k, _ := s.l.EdgesKey(ctx); k != "" {
		t.Errorf("a read that reached nothing changed the key to %q", k)
	}
	// The next round, a minute later, asks nothing.
	s.now = func() time.Time { return at.Add(time.Minute) }
	s.dependsAgain(ctx)
	if read() != n {
		t.Errorf("tried again within the hour: %d reads more", read()-n)
	}
	// An hour later, and failed again: two hours.
	s.now = func() time.Time { return at.Add(time.Hour) }
	s.dependsAgain(ctx)
	if read() == n {
		t.Fatal("not tried again after the hour")
	}
	st, _ = s.l.EdgesStatus(ctx)
	if st.Tries != 2 || !st.Next.Equal(at.Add(3*time.Hour)) {
		t.Errorf("after two failures: %+v", st)
	}
	_, body := askAs(t, s.Handler(), http.MethodGet, "/settings", nil, "ana@example.com", "none")
	if !strings.Contains(body, "Tried 2 times; tried again at") {
		t.Errorf("the page doesn't say when it is tried again:\n%s", body)
	}
	for tries, want := range map[int]time.Duration{1: time.Hour, 2: 2 * time.Hour, 3: 4 * time.Hour, 6: 24 * time.Hour, 20: 24 * time.Hour} {
		if got := dependsAgainBackoff(tries); got != want {
			t.Errorf("backoff after %d tries: %v, want %v", tries, got, want)
		}
	}
	// The history well: read, and the status gone.
	fails.failing["calls"].Store(0)
	s.now = func() time.Time { return at.Add(4 * time.Hour) }
	s.dependsAgain(ctx)
	if k, _ := s.l.EdgesKey(ctx); k == "" {
		t.Error("not read once the history was well")
	}
	if st, _ := s.l.EdgesStatus(ctx); st.Error != "" || st.Tries != 0 {
		t.Errorf("after it went well: %+v", st)
	}
}

// A read of the last four weeks whose part over the incident fails keeps the
// rest, and reads that part once more with the next run, then lets it be a
// known gap, shown on the Settings page.
func TestDependsAgainKeepsWhatRead(t *testing.T) {
	// Not in parallel: it makes the pieces a day.
	defer func(was time.Duration) { dependsAgainPiece = was }(dependsAgainPiece)
	dependsAgainPiece = 24 * time.Hour
	ctx := context.Background()
	s := dependsServer(t, DependsConfig{})
	hist := s.h.(*history)
	// Four weeks back from the cursor, a week at a time: fail the week that holds INC-1.
	over := time.Date(2026, 11, 2, 6, 10, 0, 0, time.UTC)
	fails := new(atomic.Int32)
	fails.Store(1)
	s.h = &graphFlaky{history: hist, failing: map[string]*atomic.Int32{"calls": fails}, over: over}
	if err := s.l.SetSettings(ctx, map[string]string{"depends": "calls", "depends-labels": "caller,callee"}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	s.dependsAgain(ctx)
	if k, _ := s.l.EdgesKey(ctx); k == "" {
		t.Fatal("what read wasn't kept")
	}
	if r, _ := s.l.EdgeRetries(ctx); len(r) != 1 {
		t.Fatalf("the part that failed isn't kept to be read once more: %v", r)
	}
	if st, _ := s.l.EdgesStatus(ctx); !strings.Contains(st.Error, "couldn't be read") {
		t.Errorf("status %+v", st)
	}
	// Known gaps are shown on the page.
	spans, _ := s.l.EdgeRetries(ctx)
	if err := s.l.EdgeGaveUp(ctx, spans[0][0]); err != nil {
		t.Fatal(err)
	}
	_, body := askAs(t, s.Handler(), http.MethodGet, "/settings", nil, "ana@example.com", "none")
	if !strings.Contains(body, "failed twice, and isn&#39;t read again") {
		t.Errorf("the page doesn't show the gap:\n%s", body)
	}
}

// What a round reads and keeps is of the graph its options had, whatever a
// save made since: it is read with the round's expression, and kept only
// while the ledger holds the round's key.
func TestDependsRoundKeepsItsOwnGraph(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s := dependsServer(t, onCalls)
	hist := s.h.(*history)
	o, err := s.options(ctx)
	if err != nil || o.Depends == nil {
		t.Fatalf("options %+v %v", o.Depends, err)
	}
	// An admin saves another expression in the middle of the round.
	if err := s.l.SetSettings(ctx, map[string]string{"depends": "other"}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	e := s.readEdges(ctx, o, from, from.Add(time.Hour))
	if hist.exprs["other"] != 0 {
		t.Errorf("read the expression saved since: %d times", hist.exprs["other"])
	}
	if e == nil || e.Key != o.Depends.Key {
		t.Fatalf("read %+v, want one of the round's key %q", e, o.Depends.Key)
	}
	// The ledger holds another key by the time it is kept (the leader's
	// dependsAgain ran): not kept.
	if err := s.l.ReplaceEdges(ctx, "another key", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.l.KeepEdges(ctx, e, o); err != nil {
		t.Fatal(err)
	}
	if rows, _, _ := s.l.EdgesKept(ctx); rows != 0 {
		t.Errorf("kept %d edges of another graph", rows)
	}
	// A round whose key no longer is the latest options' reads nothing.
	if _, ok := s.roundDepends("not the key"); ok {
		t.Error("a round with another key has a graph to read")
	}
}

// What the ledger's stones are judged by of the graph: nothing with none, and
// a digest that changes with what it was read with, its label, depth and
// direction, and with nothing else.
func TestDependsKey(t *testing.T) {
	var none verdict.Options
	if k := dependsKey(none); k != "" {
		t.Errorf("no graph: %q", k)
	}
	of := func(d verdict.Dependencies) string { return dependsKey(verdict.Options{Depends: &d}) }
	base := verdict.Dependencies{Key: "1h a,b #1", Label: "service", Depth: 1, Direction: "down"}
	k := of(base)
	if len(k) != 9 || k[0] != '~' || strings.Trim(k[1:], "ABCDEFGHIJKLMNOP") != "" {
		t.Fatalf("key %q: want ~ and eight capital letters A to P", k)
	}
	if of(base) != k {
		t.Error("not the same for the same")
	}
	for name, d := range map[string]verdict.Dependencies{
		"key":       {Key: "1h a,b #2", Label: "service", Depth: 1, Direction: "down"},
		"label":     {Key: "1h a,b #1", Label: "app", Depth: 1, Direction: "down"},
		"depth":     {Key: "1h a,b #1", Label: "service", Depth: 2, Direction: "down"},
		"direction": {Key: "1h a,b #1", Label: "service", Depth: 1, Direction: "both"},
	} {
		if of(d) == k {
			t.Errorf("a change of the %s isn't in the key", name)
		}
	}
	// Not what is only in the graph loaded.
	g := base
	g.Graph = nil
	if of(g) != k {
		t.Error("the loaded graph is in the key")
	}
}

// The key of what the edges were read with doesn't change with the
// expression's spaces or the order of its matchers, and does with the rest.
func TestDependsGraphKeyNormalised(t *testing.T) {
	c := func(expr, from, to string, step time.Duration) string {
		return dependsConf{Expr: expr, From: from, To: to, Step: step}.graphKey()
	}
	base := c(`sum by (caller, callee) (rate(calls{job="mesh", env="x"}[1h]))`, "caller", "callee", time.Hour)
	for _, same := range []string{
		"  sum by (caller,callee)   (rate(calls{env=\"x\",job=\"mesh\"}[1h]))\n",
		"sum by (callee, caller) (rate(calls{job=\"mesh\", env=\"x\"}[1h]))",
	} {
		if got := c(same, "caller", "callee", time.Hour); got != base {
			t.Errorf("%q has another key", same)
		}
	}
	for name, got := range map[string]string{
		"expression": c(`sum by (caller, callee) (rate(calls{job="other"}[1h]))`, "caller", "callee", time.Hour),
		"labels":     c(`sum by (caller, callee) (rate(calls{job="mesh", env="x"}[1h]))`, "callee", "caller", time.Hour),
		"step":       c(`sum by (caller, callee) (rate(calls{job="mesh", env="x"}[1h]))`, "caller", "callee", 2*time.Hour),
	} {
		if got == base {
			t.Errorf("a change of the %s has the same key", name)
		}
	}
	if k := (dependsConf{Expr: "calls"}).graphKey(); k != "" {
		t.Errorf("no labels: %q", k)
	}
}

// An expression with no labels to name its services is refused, saved or
// tried.
func TestDependsNeedsLabels(t *testing.T) {
	parallel(t)
	s := dependsServer(t, DependsConfig{})
	h := s.Handler()
	code, body := askAs(t, h, http.MethodPost, "/settings/depends", url.Values{"depends": {"calls"}}, "ana@example.com", "same-origin")
	if code != 400 || !strings.Contains(body, "--depends-labels") {
		t.Errorf("saved with no labels: %d %s", code, body)
	}
	if set, _ := s.l.Settings(context.Background()); len(set) != 0 {
		t.Errorf("saved: %v", set)
	}
	_, body = askAs(t, h, http.MethodGet, "/settings?depends-try=1&depends=calls", nil, "ana@example.com", "same-origin")
	if !strings.Contains(body, "the label of the caller") {
		t.Errorf("tried with no labels:\n%s", body)
	}
}

// A report of a period whose judgement was made with no graph, its incidents'
// windows having more edges than are loaded, says so: too_many, on the
// Settings page and the rule's page; and says it no more once it was judged
// with the graph.
func TestDependsOverSaid(t *testing.T) {
	// Not in parallel: it lowers what every judgement loads.
	ctx := context.Background()
	s := dependsServer(t, onCalls)
	h := s.Handler()
	report := func() string {
		_, body := askAs(t, h, http.MethodGet, "/api/v1/report?from=2026-11-02T00:00:00Z&to=2026-11-03T00:00:00Z", nil, "ana@example.com", "none")
		return strings.Join(strings.Fields(body), "")
	}
	if strings.Contains(report(), `"too_many":true`) {
		t.Fatal("too_many before anything was over")
	}
	defer ledger.LowerJudgeEdges(1)()
	o, err := s.options(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.l.JudgeAll(ctx, o, nil); err != nil {
		t.Fatal(err)
	}
	if got := viaOf(t, s); got != nil {
		t.Errorf("judged with no graph, caught via %v", got)
	}
	if r := report(); !strings.Contains(r, `"over":true`) || strings.Contains(r, `"too_many":true`) {
		t.Errorf("the report doesn't say it was judged with no graph: %s", report())
	}
	_, body := askAs(t, h, http.MethodGet, "/settings", nil, "ana@example.com", "none")
	if !strings.Contains(body, "made with no graph") {
		t.Errorf("the Settings page doesn't say it:\n%s", body)
	}
	if _, body := askAs(t, h, http.MethodGet, "/rules/PaymentsDBConnections", nil, "ana@example.com", "none"); !strings.Contains(body, "made with no graph") {
		t.Errorf("the rule's page doesn't say it:\n%s", body)
	}
}

// tried is how many reads of an expression a flaky history was asked for,
// while its failures last.
func (f *graphFlaky) tried(expr string) int {
	if n := f.failing[expr]; n != nil {
		return 1<<30 - int(n.Load())
	}
	return 0
}
