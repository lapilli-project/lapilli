package serve

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/verdict"
)

const paymentsRule = `groups:
  - name: payments
    rules:
      - alert: PaymentsDBConnections
        expr: payments > 1
        labels: {severity: page}
`

// dependsServer is a server with ana an admin and bo an editor, a rule on
// payments-db, and INC-1 in checkout: the graph says checkout calls
// payments-db ("calls"). started is the --depends serve is started with.
func dependsServer(t *testing.T, started DependsConfig) *Server {
	t.Helper()
	s, rules := server(t, func(c *Config) {
		c.UserHeader, c.Admins, c.Editors = "X-Forwarded-Email", []string{"ana@example.com"}, []string{"bo@example.com"}
		c.Impact = ""
		c.Depends = started
		keepRecord(c)
	})
	if err := os.WriteFile(filepath.Join(rules, "payments.yaml"), []byte(paymentsRule), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	s.ready.Store(true)
	return s
}

var onCalls = DependsConfig{Expr: "calls", From: "caller", To: "callee"}

// viaOf is the way INC-1 was caught by PaymentsDBConnections, by dependency,
// nil when it wasn't.
func viaOf(t *testing.T, s *Server) []string {
	t.Helper()
	in, err := s.l.IncidentByID(context.Background(), "INC-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range in.CaughtBy {
		if c.Rule == "PaymentsDBConnections" {
			if c.Dependency == nil {
				t.Errorf("caught, and not by dependency: %+v", c)
			}
			return c.Dependency
		}
	}
	return nil
}

// Started with --depends, each run reads the graph for its hours and keeps
// it, a rule on a service the incident's service calls catches it with its
// path, and judged again with the history away it says the same.
func TestDependsRuns(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s := dependsServer(t, onCalls)
	if got := viaOf(t, s); !slices.Equal(got, []string{"checkout", "payments-db"}) {
		t.Fatalf("caught via %v", got)
	}
	key, err := s.l.EdgesKey(ctx)
	if c, _ := s.currentDepends(ctx); err != nil || key == "" || key != c.graphKey() {
		t.Errorf("edges key %q %v, want %q", key, err, c.graphKey())
	}
	if rows, spans, err := s.l.EdgesKept(ctx); err != nil || rows == 0 || len(spans) == 0 {
		t.Errorf("kept %d edges over %v: %v", rows, spans, err)
	}
	if n := s.h.(*history).exprs["calls"]; n == 0 {
		t.Error("the graph was never read")
	}
	// The next run, with the history failing for everything: it reads the
	// graph, which fails, and the incident is judged by what the ledger holds.
	hist := s.h.(*history)
	fails := &graphFlaky{history: hist, failing: map[string]*atomic.Int32{"calls": failAlways()}}
	s.h = fails
	was := s.now()
	s.now = func() time.Time { return was.Add(3 * time.Hour) }
	s.jobs(ctx, true)
	if fails.tried("calls") == 0 {
		t.Fatal("the run never read the graph")
	}
	if got := viaOf(t, s); !slices.Equal(got, []string{"checkout", "payments-db"}) {
		t.Errorf("after a run whose read failed: caught via %v", got)
	}
	// And judged again, with the history still failing: it asks nothing.
	asked := fails.tried("calls")
	o, err := s.options(ctx)
	if err != nil || o.Depends == nil {
		t.Fatalf("options %+v %v", o.Depends, err)
	}
	if err := s.l.JudgeAll(ctx, o, nil); err != nil {
		t.Fatal(err)
	}
	if got := viaOf(t, s); !slices.Equal(got, []string{"checkout", "payments-db"}) {
		t.Errorf("judged again: caught via %v", got)
	}
	if fails.tried("calls") != asked {
		t.Errorf("judging again asked the history %d times", fails.tried("calls")-asked)
	}
}

func failAlways() *atomic.Int32 {
	n := new(atomic.Int32)
	n.Store(1 << 30)
	return n
}

// graphFlaky is a history that fails an expression while it has failures
// left, of the reads that cover over when that isn't zero.
type graphFlaky struct {
	*history
	failing map[string]*atomic.Int32
	over    time.Time
}

func (f *graphFlaky) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	covers := f.over.IsZero() || !f.over.Before(start) && !f.over.After(end)
	if n := f.failing[expr]; n != nil && covers && n.Add(-1) >= 0 {
		return nil, errors.New("the history failed")
	}
	return f.history.QueryRange(ctx, expr, start, end, step)
}

// Without --depends nothing is read or kept, and no catch is by dependency.
func TestDependsOff(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s := dependsServer(t, DependsConfig{})
	if got := viaOf(t, s); got != nil {
		t.Errorf("caught via %v", got)
	}
	if rows, spans, _ := s.l.EdgesKept(ctx); rows != 0 || len(spans) != 0 {
		t.Errorf("kept %d edges over %v", rows, spans)
	}
	if n := s.h.(*history).exprs["calls"]; n != 0 {
		t.Errorf("read %d times", n)
	}
	if o, _ := s.options(ctx); o.Depends != nil {
		t.Errorf("options %+v", o.Depends)
	}
}

// A graph a run couldn't read is read once more by the next run, and the
// incident is then caught by it; one that fails again is let go, and not
// read a third time.
func TestDependsFailedReadRetried(t *testing.T) {
	parallel(t)
	for _, failures := range []int32{1, 2} {
		ctx := context.Background()
		s, rules := server(t, func(c *Config) {
			c.Impact, c.Depends = "", onCalls
			keepRecord(c)
		})
		os.WriteFile(filepath.Join(rules, "payments.yaml"), []byte(paymentsRule), 0o644)
		fails := new(atomic.Int32)
		fails.Store(failures)
		// The first read that covers INC-1 fails, and with two failures the read
		// again too.
		s.h = &graphFlaky{history: s.h.(*history), failing: map[string]*atomic.Int32{"calls": fails}, over: time.Date(2026, 11, 2, 6, 10, 0, 0, time.UTC)}
		raw := ""
		if err := s.look(ctx, &raw); err != nil {
			t.Fatal(err)
		}
		s.jobs(ctx, true)
		if failures == 1 {
			// Read again by the run after it, which judged the span again.
			if retries, _ := s.l.EdgeRetries(ctx); len(retries) != 0 {
				t.Errorf("retries once read again: %v", retries)
			}
			if got := viaOf(t, s); !slices.Equal(got, []string{"checkout", "payments-db"}) {
				t.Errorf("caught via %v after the retry", got)
			}
			continue
		}
		if retries, _ := s.l.EdgeRetries(ctx); len(retries) != 0 {
			t.Errorf("a span that failed twice is kept: %v", retries)
		}
		if got := viaOf(t, s); got != nil {
			t.Errorf("caught via %v with the graph unread", got)
		}
		if fails.Load() != 0 {
			t.Errorf("failures left over: %d", fails.Load())
		}
	}
}

// The Settings page tries a graph on the last four weeks without saving it;
// saved, it is read over those weeks and every firing judged again; changing
// how far or which way judges again and reads nothing; off takes it away.
func TestDependsSettings(t *testing.T) {
	// Not in parallel: it counts the times judgeAll is called, which every
	// test's server calls.
	ctx := context.Background()
	s := dependsServer(t, DependsConfig{})
	h := s.Handler()
	try := "/settings?depends-try=1&depends=calls&depends-labels=caller%2Ccallee&depends-depth=1&depends-direction=down"
	if code, _ := askAs(t, h, http.MethodGet, try, nil, "bo@example.com", "same-origin"); code != http.StatusForbidden {
		t.Errorf("an editor: %d", code)
	}
	if code, _ := askAs(t, h, http.MethodGet, try, nil, "ana@example.com", "cross-site"); code != 400 {
		t.Errorf("from another site: %d", code)
	}
	code, body := askAs(t, h, http.MethodGet, try, nil, "ana@example.com", "same-origin")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	for _, want := range []string{"2 pairs of services call each other in it; 1 catches by dependency would be added.", "PaymentsDBConnections",
		"<bdi>checkout</bdi> → <bdi>payments-db</bdi>", "Nothing was saved.", `<th scope="row">Incidents missed</th>`} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q:\n%s", want, body)
		}
	}
	if set, err := s.l.Settings(ctx); err != nil || len(set) != 0 {
		t.Errorf("saved: %v %v", set, err)
	}
	if got := viaOf(t, s); got != nil {
		t.Errorf("tried, caught via %v", got)
	}
	// What doesn't read is said, with the form as typed, and read no more.
	asked := s.h.(*history).asked
	_, body = askAs(t, h, http.MethodGet, "/settings?depends-try=1&depends=sum(&depends-labels=caller%2Ccallee", nil, "ana@example.com", "same-origin")
	if !strings.Contains(body, "doesn&#39;t read as PromQL") || !strings.Contains(body, ">sum(</textarea>") || s.h.(*history).asked != asked {
		t.Errorf("not PromQL:\n%s", body)
	}
	_, body = askAs(t, h, http.MethodGet, "/settings?depends-try=1&depends=calls&depends-labels=caller", nil, "ana@example.com", "same-origin")
	if !strings.Contains(body, "want FROM,TO") {
		t.Errorf("one label:\n%s", body)
	}
	// Saved: an editor may not, nothing without labels.
	form := url.Values{"depends": {"calls"}, "depends-labels": {"caller,callee"}, "depends-depth": {"1"}, "depends-direction": {"down"}}
	if code, _ := askAs(t, h, http.MethodPost, "/settings/depends", form, "bo@example.com", "same-origin"); code != http.StatusForbidden {
		t.Errorf("an editor saved: %d", code)
	}
	if code, _ := askAs(t, h, http.MethodPost, "/settings/depends", url.Values{"depends": {"calls"}, "depends-labels": {"caller"}}, "ana@example.com", "same-origin"); code != 400 {
		t.Errorf("without both labels: %d", code)
	}
	if code, body := askAs(t, h, http.MethodPost, "/settings/depends", form, "ana@example.com", "same-origin"); code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	// Judged by it from the next round, once: until it is read, what the
	// ledger holds is of the graph before, and judging again, as a run
	// does, is judging by none.
	if o, err := s.options(ctx); err != nil || o.Depends != nil {
		t.Errorf("saved, not read yet: options %+v %v", o.Depends, err)
	} else if err := s.l.JudgeAll(ctx, o, nil); err != nil {
		t.Fatal(err)
	}
	if got := viaOf(t, s); got != nil {
		t.Errorf("saved, not read yet: caught via %v", got)
	}
	judgedAll := 0
	defer func(f func(*ledger.Ledger, context.Context, verdict.Options, func(float64)) error) { judgeAll = f }(judgeAll)
	judgeAll = func(l *ledger.Ledger, ctx context.Context, o verdict.Options, done func(float64)) error {
		judgedAll++
		return l.JudgeAll(ctx, o, done)
	}
	s.jobs(ctx, false)
	if got := viaOf(t, s); !slices.Equal(got, []string{"checkout", "payments-db"}) {
		t.Fatalf("caught via %v after it was read", got)
	}
	if judgedAll != 1 {
		t.Errorf("judged everything %d times", judgedAll)
	}
	s.jobs(ctx, false)
	if judgedAll != 1 {
		t.Errorf("judged everything again with nothing changed: %d", judgedAll)
	}
	_, body = askAs(t, h, http.MethodGet, "/settings", nil, "ana@example.com", "same-origin")
	if !strings.Contains(body, "Service graph") || !strings.Contains(body, "edges kept") {
		t.Errorf("the page doesn't show it:\n%s", body)
	}
	// Depth alone judges again, reading nothing.
	read := s.h.(*history).exprs["calls"]
	form.Set("depends-depth", "2")
	if code, body := askAs(t, h, http.MethodPost, "/settings/depends", form, "ana@example.com", "same-origin"); code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	s.jobs(ctx, false)
	if judgedAll != 2 || s.h.(*history).exprs["calls"] != read {
		t.Errorf("a new depth: judged %d times, read %d more", judgedAll, s.h.(*history).exprs["calls"]-read)
	}
	// Off: the edges go, and no catch is by dependency.
	if code, body := askAs(t, h, http.MethodPost, "/settings/depends", url.Values{"depends": {"off"}}, "ana@example.com", "same-origin"); code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	s.jobs(ctx, false)
	if rows, _, _ := s.l.EdgesKept(ctx); rows != 0 {
		t.Errorf("kept %d edges once off", rows)
	}
	if got := viaOf(t, s); got != nil {
		t.Errorf("off, caught via %v", got)
	}
	if judgedAll != 3 {
		t.Errorf("judged everything %d times", judgedAll)
	}
}

// Tries of the graph are counted with the incident criteria's, and past them
// refused before anything is read.
func TestDependsTriesLimited(t *testing.T) {
	parallel(t)
	s := dependsServer(t, DependsConfig{})
	h := s.Handler()
	for i := range impactTriesAnHour + 1 {
		path := "/settings?depends-try=1&depends-labels=caller%2Ccallee&depends=calls+or+vector%28" + strings.Repeat("1", i+1) + "%29"
		code, body := askAs(t, h, http.MethodGet, path, nil, "ana@example.com", "same-origin")
		switch {
		case i < impactTriesAnHour && code != 200:
			t.Fatalf("try %d: %d %s", i, code, body)
		case i == impactTriesAnHour && (code != 400 || !strings.Contains(body, "as many incident criteria")):
			t.Errorf("past the hour's: %d", code)
		}
	}
}

// A catch by dependency is shown with its path on the incident's page, a
// rule's page lists its catches by dependency apart, and the API, MCP and
// the report carry it; with no graph none of them say anything of it.
func TestDependsShown(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	for _, on := range []bool{true, false} {
		started := DependsConfig{}
		if on {
			started = onCalls
		}
		s := dependsServer(t, started)
		h := s.Handler()
		in := func(path string) string {
			t.Helper()
			code, body := askAs(t, h, http.MethodGet, path, nil, "ana@example.com", "none")
			if code != 200 {
				t.Fatalf("%s: %d %s", path, code, body)
			}
			return body
		}
		for path, wants := range map[string][]string{
			"/incidents/INC-1":             {"by dependency, via <bdi><code><bdi>checkout</bdi> → <bdi>payments-db</bdi></code></bdi>"},
			"/rules/PaymentsDBConnections": {"Caught by dependency", "<bdi>checkout</bdi> → <bdi>payments-db</bdi>", "caught something only by the service graph"},
			"/api/v1/incidents?weeks=4":    {`"dependency":["checkout","payments-db"]`},
			"/api/v1/report?from=2026-11-02T00:00:00Z&to=2026-11-03T00:00:00Z": {`"dependency":["checkout","payments-db"]`, `"depends":{"label":"service","depth":1,"direction":"down","edges":2}`, `"white_by_dependency":1`},
		} {
			body := in(path)
			if strings.HasPrefix(path, "/api") {
				body = strings.Join(strings.Fields(body), "")
			}
			for _, want := range wants {
				if has := strings.Contains(body, want); has != on {
					t.Errorf("graph %v, %s: has %q is %v", on, path, want, has)
				}
			}
		}
		if body := in("/rules/Errors"); strings.Contains(body, "Caught by dependency") {
			t.Errorf("graph %v: Errors caught by dependency", on)
		}
		_, inc, err := s.toolIncidents(ctx, nil, incidentsIn{})
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(inc.Incidents, func(x incidentOut) bool { return x.ID == "INC-1" })
		if i < 0 {
			t.Fatalf("MCP's incidents: %+v", inc.Incidents)
		}
		var via []string
		for _, c := range inc.Incidents[i].CaughtBy {
			if c.Rule == "PaymentsDBConnections" {
				via = c.Dependency
			}
		}
		if on != slices.Equal(via, []string{"checkout", "payments-db"}) {
			t.Errorf("graph %v: MCP says %v", on, via)
		}
		// The rules MCP lists say how many of a rule's white firings only the
		// graph made white.
		_, rs, err := s.toolRules(ctx, nil, rulesIn{})
		if err != nil {
			t.Fatal(err)
		}
		white := 0
		for _, r := range rs.Rules {
			if r.Name == "PaymentsDBConnections" {
				white = r.WhiteByDependency
			}
		}
		if (white == 1) != on {
			t.Errorf("graph %v: MCP says %d firings white by dependency", on, white)
		}
		// The Settings page tells which it is.
		body := in("/settings")
		if has := strings.Contains(body, "edges kept"); has != on {
			t.Errorf("graph %v: the Settings page says edges kept: %v", on, has)
		}
	}
}

// settings.yaml gives the graph the Settings page set as Helm values, and
// nothing of it when none was.
func TestDependsSettingsYAML(t *testing.T) {
	parallel(t)
	s := dependsServer(t, DependsConfig{})
	h := s.Handler()
	if _, body, _ := get(t, h, "/settings.yaml"); strings.Contains(body, "depends") {
		t.Errorf("settings.yaml before:\n%s", body)
	}
	form := url.Values{"depends": {"calls"}, "depends-labels": {"caller,callee"}, "depends-depth": {"2"}, "depends-direction": {"both"}}
	if code, body := askAs(t, h, http.MethodPost, "/settings/depends", form, "ana@example.com", "same-origin"); code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	_, body, _ := get(t, h, "/settings.yaml")
	for _, want := range []string{`depends: "calls"`, `dependsLabels: "caller,callee"`, "dependsDepth: 2", "dependsDirection: both"} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q:\n%s", want, body)
		}
	}
}
