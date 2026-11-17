package serve

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
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
	s, _ := server(t, func(c *Config) {
		c.Options.Hours = verdict.DefaultHours(time.UTC)
		if c.LedgerURLFile != "" {
			// Postgres plans each statement for its own arguments, as a
			// connection's plan cache can (whether it does depends on what
			// the statement's first five runs asked for): with the
			// statistics below, the worst it does, and what CI's did.
			c.LedgerURLFile = urlWith(t, c.LedgerURLFile, "plan_cache_mode", "force_custom_plan")
		}
	})
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
		if s.cfg.LedgerURLFile != "" && d.Equal(start.Add(10*24*time.Hour)) {
			// Postgres's statistics of the firings as autovacuum can leave
			// them, taken ten days into the backfill: the weeks since are
			// past them, and a query of a few rules' firings of one of those
			// weeks is planned by when they started, reading the whole week,
			// as it often is with any statistics (hoursOf, ledger/read.go).
			// Fifty teams' hours, a query a team and a week, took 3.4 s so
			// on CI's Postgres 18.
			ledgerExec(t, s, "ANALYZE firings")
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
	// A deploy of each service every few hours, read from the deploys
	// file all at once, the firings they meet marked (docs/design/noc.md).
	var deploys []ledger.Deploy
	for at := start; at.Before(now); at = at.Add(time.Duration(1+r.IntN(6)) * time.Hour) {
		deploys = append(deploys, ledger.Deploy{Start: at, End: at.Add(5 * time.Minute), Labels: map[string]string{"service": services[r.IntN(len(services))]}})
	}
	o.DeployAfter = 10 * time.Minute
	began = time.Now()
	if _, err := s.l.SyncDeploys(ctx, deploys, o, ledger.DeploysRead{}); err != nil {
		t.Fatal(err)
	}
	if err := s.l.JudgePending(ctx, o); err != nil {
		t.Fatal(err)
	}
	took = time.Since(began)
	t.Logf("%-32s %s", fmt.Sprintf("%d deploys, marked", len(deploys)), took.Round(time.Microsecond))
	if budget := time.Duration(float64(5*time.Second) * max(1, k) * slow); took > budget {
		t.Errorf("deploys took %s, over %s", took, budget)
	}
	// Windows every month, by cron, and a team's, each judged again over the
	// twelve weeks, to the end (docs/design/serveops.md).
	labelled, err := s.l.PutTeam(ctx, ledger.Team{Name: "billing", Labels: incident.Labels{"service": {"billing"}}}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(start.Year(), start.Month(), start.Day(), 2, 0, 0, 0, time.UTC)
	for _, w := range []ledger.Window{
		{Title: "monthly", Start: first, End: first.Add(4 * time.Hour), Every: "month", On: fmt.Sprint(min(first.Day(), 28))},
		{Title: "cron", Start: first, End: first.Add(time.Hour), Every: "cron", On: "0 2 * * *"},
		{Title: "a team's, by cron", Start: first, End: first.Add(time.Hour), Every: "cron", On: "0 2 * * *", TeamID: labelled},
		{Title: "a team's, for good", Start: start, End: start.Add(24 * time.Hour), TeamID: 1},
	} {
		if w.Every == "month" && first.Day() > 28 {
			continue
		}
		began := time.Now()
		if _, err := s.l.PutWindow(ctx, w, "ana", o); err != nil {
			t.Fatal(err)
		}
		if err := s.l.JudgePending(ctx, o); err != nil {
			t.Fatal(err)
		}
		took := time.Since(began)
		t.Logf("%-32s %s", w.Title+", judged again", took.Round(time.Microsecond))
		if budget := time.Duration(float64(10*time.Second) * max(1, k) * slow); took > budget {
			t.Errorf("%s took %s, over %s", w.Title, took, budget)
		}
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
		// A team keeping its own hours: its twelve weeks by those, afresh;
		// the pages below count by both.
		{"hours, a team's own", 500 * time.Millisecond, func() {
			teams, err := s.l.Teams(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, tm := range teams {
				if tm.Name == "team3" {
					tm.TZ = "Asia/Tokyo"
					if _, err := s.l.PutTeam(ctx, tm, "ana"); err != nil {
						t.Fatal(err)
					}
				}
			}
			if t12, err = s.tallies(ctx, weeks); err != nil {
				t.Fatal(err)
			}
		}},
		// Fifty teams, each with hours of its own, owning every rule: about
		// as slow as reading the firings once more, row by row.
		{"hours, fifty teams' own, afresh", time.Second, func() {
			for i := 0; i < 50; i++ {
				var names []string
				for j := 0; j < 8; j++ {
					names = append(names, fmt.Sprintf("Rule%03d", i*8+j))
				}
				tm := ledger.Team{Name: fmt.Sprintf("a%02d", i), Rules: "^(" + strings.Join(names, "|") + ")$",
					SleepHours: fmt.Sprintf("%02d:%02d-08:00", 20+i%4, i), TZ: "Asia/Tokyo"}
				if _, err := s.l.PutTeam(ctx, tm, "ana"); err != nil {
					t.Fatal(err)
				}
			}
			if t12, err = s.tallies(ctx, weeks); err != nil {
				t.Fatal(err)
			}
			s.hours.by = nil
		}},
		// The same teams in shifts of eight hours, round three zones.
		{"hours, fifty teams in shifts, afresh", time.Second, func() {
			teams, err := s.l.Teams(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for i, tm := range teams {
				if !strings.HasPrefix(tm.Name, "a") {
					continue
				}
				tm.Shifts = fmt.Sprintf("every=8h from=2026-10-%02dT%02d:00; tz=Asia/Tokyo; tz=Europe/Berlin; tz=America/New_York", 1+i%28, i%24)
				if _, err := s.l.PutTeam(ctx, tm, "ana"); err != nil {
					t.Fatal(err)
				}
			}
			if t12, err = s.tallies(ctx, weeks); err != nil {
				t.Fatal(err)
			}
			s.hours.by = nil
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
		"/outcomes", "/api/v1/outcomes", "/metrics", "/api/v1/report?firings=false", "/api/v1/report", "/roles", "/reports", "/reports/digest",
		"/rules?sort=refired", "/incidents?team=team3&source=written", "/search?q=rule0", "/api/v1/summary?team=*", "/api/v1/summary?from=2025-11-10&team=*",
		"/marked", "/rules?during=deploy"} {
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
	goalsScale(t, s, h)
}

// goalsScale holds the goals (docs/design/goals.md) of TestPagesScale's
// ledger by the summary's rows each reads and the heap each takes, the
// least of three runs: a team's Goals page, its thirteen weeks for the
// team and everyone; the overview's card, the last whole week; and the
// leader's work after a run, thirteen weeks for everyone and every team,
// of a goal of each team's.
func goalsScale(t *testing.T, s *Server, h http.Handler) {
	ctx := context.Background()
	declare(t, s, "goals:\n  - {name: precise, measure: precision, at_least: 0.5}\n  - {name: team3-paged, team: team3, measure: critical.firings, critical: [page], at_most: 40}\n"+
		"  - {name: no-refire, team: \"*\", measure: refired.firings, at_most: 0}\n")
	teams, err := s.l.Teams(ctx)
	if err != nil {
		t.Fatal(err)
	}
	team3 := teamOf(t, s, "team3")
	afresh := func() {
		s.cache.Lock()
		s.cache.gen = ""
		s.cache.Unlock()
		s.goalRows.Lock()
		s.goalRows.by = nil
		s.goalRows.Unlock()
		s.goalsK.Lock()
		s.goalsK.key = ""
		s.goalsK.Unlock()
	}
	for _, c := range []struct {
		name string
		rows int
		most uint64
		run  func() error
	}{
		{"/goals?team=team3", 13 * 2, 32 << 20, func() error {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/goals?team=team3", nil))
			if w.Code != 200 {
				return fmt.Errorf("%d", w.Code)
			}
			return nil
		}},
		{"the overview's Goals card", 1, 8 << 20, func() error {
			_, _, err := s.goalsOverview(ctx, "en", &team3)
			return err
		}},
		{"the leader's work after a run", 13 * (1 + len(teams)), 48 << 20, func() error { return s.goalsAfter(ctx) }},
	} {
		rows := 0
		summaryRowSeen = func() { rows++ }
		var err error
		peak := leastPeak(func() {
			afresh()
			rows = 0
			err = c.run()
		})
		summaryRowSeen = nil
		t.Logf("%-32s %4d rows, heap +%d MiB (the least of three runs)", c.name, rows, peak>>20)
		if err != nil || rows != c.rows || peak > c.most {
			t.Errorf("%s: %d rows, want %d; heap +%d MiB, most %d; %v", c.name, rows, c.rows, peak>>20, c.most>>20, err)
		}
	}
	// Goals over a period (step 3): a sale each Friday, everyone's, and a
	// launch each morning, of each team's. Besides the summary's rows, each
	// reads the tallies of its occurrences' weeks and the four before,
	// seventeen weeks at most, never the firings, and counts each
	// occurrence once for each team it is of.
	fri := s.l.Midnight(ledger.Week(s.l.Day(now)) - 7*weeks).Add(4*24*time.Hour + 18*time.Hour)
	if _, err := s.l.SyncMarked(ctx, []ledger.Marked{
		{Kind: "sale", Title: "Friday sale", Start: fri, End: fri.Add(4 * time.Hour), Every: "week"},
		{Kind: "launch", Title: "Morning launch", Start: fri.Add(-4*24*time.Hour - 9*time.Hour), End: fri.Add(-4*24*time.Hour - 7*time.Hour), Every: "day",
			Labels: map[string]string{"service": "search"}},
	}, s.cfg.Options, ledger.MarkedRead{}); err != nil {
		t.Fatal(err)
	}
	if err := s.l.JudgePending(ctx, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	declare(t, s, "goals:\n  - {name: precise, measure: precision, at_least: 0.5}\n  - {name: sale-no-worse, each: sale, measure: precision, no_worse_than: before}\n"+
		"  - {name: launch-missed, team: \"*\", each: launch, measure: incidents.missed, at_most: 0}\n")
	firings := 0
	for _, w := range []int64{ledger.Week(s.l.Day(now)) - 7*weeks} {
		ws, err := s.l.WeeksBy(ctx, w, ledger.Week(s.l.Day(now)))
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range ws {
			firings += x.Firings
		}
	}
	for _, c := range []struct {
		name      string
		rows, who int
		most      uint64
		run       func() error
	}{
		{"/goals?team=team3, over a period", 13 * 2, 2, 48 << 20, func() error {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/goals?team=team3", nil))
			if w.Code != 200 {
				return fmt.Errorf("%d", w.Code)
			}
			return nil
		}},
		{"the overview's card, over a period", 2, 1, 16 << 20, func() error {
			_, _, err := s.goalsOverview(ctx, "en", &team3)
			return err
		}},
		{"the leader's work, over a period", 13 * (1 + len(teams)), 1 + len(teams), 64 << 20, func() error { return s.goalsAfter(ctx) }},
	} {
		rows, read, weeksRead, occs, counted := 0, 0, 0, 0, 0
		summaryRowSeen = func() { rows++ }
		periodCounted = func(r, w, o, n int) { read, weeksRead, occs, counted = read+r, max(weeksRead, w), occs+o, counted+n }
		var err error
		peak := leastPeak(func() {
			afresh()
			rows, read, weeksRead, occs, counted = 0, 0, 0, 0, 0
			err = c.run()
		})
		summaryRowSeen, periodCounted = nil, nil
		t.Logf("%-32s %4d rows; over a period %d rows of %d weeks, %d occurrences counted %d times; heap +%d MiB (the least of three runs)",
			c.name, rows, read, weeksRead, occs, counted, peak>>20)
		if err != nil || rows != c.rows || peak > c.most || occs == 0 || weeksRead > 17 || counted > occs*c.who || read > firings/5 {
			t.Errorf("%s: %d rows, want %d; %d rows of %d weeks over a period, of %d firings; %d occurrences counted %d times, of %d teams; heap +%d MiB, most %d; %v",
				c.name, rows, c.rows, read, weeksRead, firings, occs, counted, c.who, peak>>20, c.most>>20, err)
		}
	}
}

// heapAfterGC is the live heap.
func heapAfterGC() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// leastPeak runs f three times and gives the least the heap rose above the
// live heap while it ran, as internal/prom's tests take it (#99): a
// sampled peak counts garbage the collector hasn't reached yet too.
func leastPeak(f func()) uint64 {
	least := ^uint64(0)
	for range 3 {
		before := heapAfterGC()
		least = min(least, max(peakDuring(f), before)-before)
	}
	return least
}

// peakDuring samples the heap every millisecond while f runs.
func peakDuring(f func()) (peak uint64) {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			peak = max(peak, m.HeapAlloc)
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	f()
	close(stop)
	<-done
	return peak
}
