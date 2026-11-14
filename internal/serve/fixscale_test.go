package serve

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
)

// TestChangePagesScale: at two thousand rules, four weeks of their firings
// and a thousand proposals, the proposals list, a proposal's page in each
// way it is shown, the tasks and the Manage page, worked out afresh, and a
// quiet window's preview, each take under a second.
func TestChangePagesScale(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	ctx := context.Background()
	s, rules := server(t, nil)
	const n = 2000
	var b strings.Builder
	b.WriteString("groups:\n")
	for g := 0; g < 20; g++ {
		fmt.Fprintf(&b, "  - name: g%d\n    rules:\n", g)
		for i := g; i < n; i += 20 {
			fmt.Fprintf(&b, "      - {alert: Rule%04d, expr: up == %d, for: 2m, labels: {severity: %s}}\n", i, i, []string{"critical", "warning", "info"}[i%3])
		}
	}
	os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte(b.String()), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	o := s.cfg.Options
	r := rand.New(rand.NewPCG(11, 12))
	start := now.Add(-28 * 24 * time.Hour)
	for d := start; d.Before(now); d = d.Add(24 * time.Hour) {
		var fs []firing.Firing
		for i := 0; i < 3000; i++ {
			at := d.Add(time.Duration(r.IntN(24*60)) * time.Minute)
			fs = append(fs, firing.Firing{Rule: fmt.Sprintf("Rule%04d", r.IntN(n)), Labels: map[string]string{"service": "s"}, Start: at, End: at.Add(time.Minute)})
		}
		if err := s.l.Record(ctx, ledger.Found{From: d, To: d.Add(24*time.Hour - time.Second), Firings: fs, Run: ledger.Run{Kind: "backfill", Started: d, Finished: d}}, o); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		if _, err := s.l.PutTeam(ctx, ledger.Team{Name: fmt.Sprintf("team%d", i), Rules: fmt.Sprintf("^Rule%d", i)}, "ana"); err != nil {
			t.Fatal(err)
		}
	}
	// A thousand proposals, a few hundred of them moved on, the first one
	// with a long rule, through every state.
	states := []string{ledger.StateApproved, ledger.StateReady, ledger.StateOpened, ledger.StateApplied, ledger.StateDropped, ledger.StateRefused}
	long := strings.Repeat("line\n", 300)
	for i := 0; i < 1000; i++ {
		p := ledger.Proposal{Rule: fmt.Sprintf("Rule%04d", i), Kind: ledger.KindFor, Path: ledger.PathGit, MadeBy: fmt.Sprint("p", i%80, "@example.com"),
			Before: "for: 2m\n", After: "for: 15m\n", Diff: "-for: 2m\n+for: 15m\n"}
		if i == 0 {
			p.Before, p.After = long+"for: 2m\n"+long, long+"for: 15m\n"+long
		}
		id, err := s.l.Propose(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if i%3 == 0 {
			if _, err := s.l.Move(ctx, id, []string{ledger.StateProposed}, states[i%len(states)], "ana", ledger.Step{Link: "https://git.example/pull/1"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	h := s.Handler()
	for _, path := range []string{"/proposals", "/proposals?state=all", "/proposals?state=all&kind=for&by=person&team=team3", "/proposals/1",
		"/proposals/1?diff=split&context=all", "/proposals/2", "/proposals/4", "/tasks", "/tasks?status=all", "/manage"} {
		s.cache.Lock()
		s.cache.gen = ""
		s.cache.Unlock()
		began := time.Now()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		took := time.Since(began)
		t.Logf("%-60s %d %8d bytes  %s", path, w.Code, w.Body.Len(), took.Round(time.Microsecond))
		if w.Code != http.StatusOK {
			t.Errorf("%s: %d", path, w.Code)
		}
		if budget := time.Duration(float64(time.Second) * slow); took > budget {
			t.Errorf("%s took %s, over %s", path, took, budget)
		}
	}
	// A quiet window's preview, every day over the four weeks, and once
	// over most of them: every firing read, and matched.
	for name, f := range map[string]url.Values{
		"preview, every day":  windowForm("nightly", "2026-10-07T01:00", "2026-10-07T05:00", "every", "day", "rules", "^Rule1", "preview", "1"),
		"preview, once, long": windowForm("freeze", "2026-10-07T01:00", "2026-11-03T05:00", "preview", "1"),
		"preview, by cron":    windowForm("weekly", "2026-10-10T02:00", "2026-10-10T03:00", "every", "cron", "on", "0 2 * * 6", "preview", "1"),
	} {
		began := time.Now()
		req := httptest.NewRequest(http.MethodPost, "/windows", strings.NewReader(f.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		took := time.Since(began)
		t.Logf("%-60s %d %8d bytes  %s", name, w.Code, w.Body.Len(), took.Round(time.Microsecond))
		if w.Code != http.StatusOK {
			t.Errorf("%s: %d\n%s", name, w.Code, w.Body.String())
		}
		if budget := time.Duration(float64(time.Second) * slow); took > budget {
			t.Errorf("%s took %s, over %s", name, took, budget)
		}
	}
}
