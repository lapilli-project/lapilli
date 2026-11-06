package serve

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// slow stretches the budgets under the race detector, which slows reading
// SQLite many times over (race_test.go).
var slow = 1.0

// TestPagesScale fills a ledger with twelve weeks of a busy set of rules
// and holds each page, worked out afresh, to a budget. LAPILLI_SCALE
// multiplies the firings (default 1: about 200,000).
func TestPagesScale(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	k := 1.0
	if v := os.Getenv("LAPILLI_SCALE"); v != "" {
		k, _ = strconv.ParseFloat(v, 64)
	}
	ctx := context.Background()
	// Counted by the hours, as the command line does by default.
	s, _ := server(t, func(c *Config) { c.Options.Hours = verdict.DefaultHours(time.UTC) })
	o := s.cfg.Options
	r := rand.New(rand.NewPCG(3, 4))
	start := now.Add(-weeks * 7 * 24 * time.Hour)
	services := []string{"checkout", "search", "cart", "auth", "billing", "media"}
	var incs []incident.Incident
	for i := 0; i < weeks*40; i++ {
		at := start.Add(time.Duration(r.IntN(weeks*7*24*60)) * time.Minute)
		incs = append(incs, incident.Incident{ID: fmt.Sprintf("INC-%d", i), Start: at, End: at.Add(45 * time.Minute),
			Labels: incident.Labels{"service": {services[r.IntN(len(services))]}}})
	}
	if _, err := s.l.SyncFile(ctx, incs, o); err != nil {
		t.Fatal(err)
	}
	for d := start; d.Before(now); d = d.Add(24 * time.Hour) {
		var fs []firing.Firing
		for i := 0; i < int(2400*k); i++ {
			at := d.Add(time.Duration(r.IntN(24*60)) * time.Minute)
			fs = append(fs, firing.Firing{Rule: fmt.Sprintf("Rule%03d", r.IntN(400)),
				Labels: map[string]string{"service": services[r.IntN(len(services))], "severity": "page"},
				Start:  at, End: at.Add(time.Duration(1+r.IntN(60)) * time.Minute)})
		}
		if err := s.l.Record(ctx, ledger.Found{From: d, To: d.Add(24*time.Hour - time.Second), Firings: fs,
			Run: ledger.Run{Kind: "backfill", Started: d, Finished: d}}, o); err != nil {
			t.Fatal(err)
		}
	}
	// What people keep: teams, quiet windows, tasks and comments.
	for i := 0; i < 5; i++ {
		if _, err := s.l.PutTeam(ctx, ledger.Team{Name: fmt.Sprintf("team%d", i), Rules: fmt.Sprintf("^Rule%d", i)}, "ana"); err != nil {
			t.Fatal(err)
		}
	}
	began := time.Now()
	for i := 0; i < 20; i++ {
		at := start.Add(time.Duration(r.IntN(weeks*7*24)) * time.Hour)
		if _, err := s.l.PutWindow(ctx, ledger.Window{Title: fmt.Sprint("deploy ", i), Start: at, End: at.Add(time.Duration(1+r.IntN(24)) * time.Hour),
			Labels: incident.Labels{"service": {services[r.IntN(len(services))]}}}, "ana", o); err != nil {
			t.Fatal(err)
		}
	}
	took := time.Since(began) / 20
	t.Logf("%-32s %s", "a quiet window, judged again", took.Round(time.Microsecond))
	if budget := time.Duration(float64(2*time.Second) * max(1, k) * slow); took > budget {
		t.Errorf("a quiet window took %s, over %s", took, budget)
	}
	for i := 0; i < 300; i++ {
		id, err := s.l.PutTask(ctx, ledger.Task{Title: fmt.Sprint("task ", i), Kind: "tune", Rule: fmt.Sprintf("Rule%03d", r.IntN(400)),
			TeamID: int64(1 + r.IntN(5)), Status: ledger.TaskStatuses[r.IntN(4)]}, "ana")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.l.PutComment(ctx, ledger.Comment{On: "task", Key: fmt.Sprint(id), Body: "noted"}, "ana"); err != nil {
			t.Fatal(err)
		}
	}
	// Thirty reviewed changes to the rules, five rules each, to measure.
	for i := 0; i < 30; i++ {
		at := start.Add(time.Duration(r.IntN(weeks*7*24)) * time.Hour)
		var rcs []verdict.RuleChange
		for j := 0; j < 5; j++ {
			rcs = append(rcs, verdict.RuleChange{Rule: fmt.Sprintf("Rule%03d", r.IntN(400)),
				Before: &verdict.RuleResult{Firings: 40 + r.IntN(40)}, After: &verdict.RuleResult{Firings: r.IntN(40)}})
		}
		if err := s.l.AddResult(ctx, "review", at, at.Add(-7*24*time.Hour), at, verdict.Comparison{Rules: rcs}); err != nil {
			t.Fatal(err)
		}
	}
	// When the firings came, all twelve weeks afresh, as after a restart;
	// then after a run, which changes the last week or two.
	t12, err := s.tallies(ctx, weeks)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		budget time.Duration
		before func()
	}{
		{"hours, twelve weeks afresh", 500 * time.Millisecond, func() { s.hours.by = nil }},
		{"hours, after a run", 100 * time.Millisecond, func() {
			last := now.Truncate(time.Hour)
			if err := s.l.Record(ctx, ledger.Found{From: last.Add(-time.Hour), To: last, Run: ledger.Run{Kind: "hourly", Started: last, Finished: last}}, o); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		c.before()
		began := time.Now()
		if _, err := s.byHours(ctx, t12); err != nil {
			t.Fatal(err)
		}
		took := time.Since(began)
		t.Logf("%-32s %s", c.name, took.Round(time.Microsecond))
		if budget := time.Duration(float64(c.budget) * max(1, k) * slow); took > budget {
			t.Errorf("%s took %s, over %s", c.name, took, budget)
		}
	}
	h := s.Handler()
	for _, path := range []string{"/", "/?team=team3", "/rules", "/rules?weeks=12", "/rules?team=team3", "/rules/Rule007", "/incidents", "/incidents?weeks=12",
		"/firings?day=2026-11-02", "/tasks", "/tasks?status=all", "/tasks/7", "/windows", "/windows/3", "/teams",
		"/outcomes", "/api/v1/outcomes", "/metrics", "/api/v1/report?firings=false", "/api/v1/report"} {
		s.cache.Lock()
		s.cache.gen = "" // worked out afresh
		s.cache.Unlock()
		s.outs.Lock()
		s.outs.v = nil
		s.outs.Unlock()

		began := time.Now()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		took := time.Since(began)
		t.Logf("%-32s %4d %8d bytes  %s", path, w.Code, w.Body.Len(), took.Round(time.Microsecond))
		budget := time.Duration(float64(time.Second) * max(1, k) * slow)
		if path == "/api/v1/report" {
			budget *= 4 // every firing of the week, as JSON
		}
		if w.Code != 200 || took > budget {
			t.Errorf("%s: %d in %s, over %s", path, w.Code, took, budget)
		}
	}
}
