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
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
)

// TestManyAtOnceScale: at two thousand rules and twelve weeks of firings,
// three thousand a day, the rules list with its bulk form and over a week,
// the firings of a day by the hour and by a label, the overview with its
// marks, an incident's page with its timeline, and bulk writes of a
// hundred, each within its budget; the rules list with each rule's labels
// as chips, six a rule; and a proposal for twenty rules, each read again
// and backtested. The budgets are what the ledger does, not how long it
// takes (ledgerWork), so that a slow runner fails none: a page sends a
// few statements, not one for each rule or firing, and a write of a
// hundred a few for each of them, writing no more rows than the firings
// kept. The time each took is logged.
func TestManyAtOnceScale(t *testing.T) {
	parallel(t)
	if testing.Short() {
		t.Skip("short")
	}
	ctx := context.Background()
	s, rules := server(t, counting)
	const n = 2000
	var b strings.Builder
	b.WriteString("groups:\n")
	for g := 0; g < 20; g++ {
		fmt.Fprintf(&b, "  - name: g%d\n    rules:\n", g)
		for i := g; i < n; i += 20 {
			fmt.Fprintf(&b, "      - {alert: Rule%04d, expr: up == %d, for: 2m, labels: {severity: %s, tier: t%d, service: s%d, owner: o%d, env: prod, region: r%d}}\n",
				i, i, []string{"critical", "warning", "info"}[i%3], i%4, i%7, i%11, i%3)
		}
	}
	os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte(b.String()), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	o := s.cfg.Options
	r := rand.New(rand.NewPCG(9, 10))
	start := now.Add(-weeks * 7 * 24 * time.Hour)
	for d := start; d.Before(now); d = d.Add(24 * time.Hour) {
		var fs []firing.Firing
		for i := 0; i < 3000; i++ {
			at := d.Add(time.Duration(r.IntN(24*60)) * time.Minute)
			fs = append(fs, firing.Firing{Rule: fmt.Sprintf("Rule%04d", r.IntN(n*9/10)), Labels: map[string]string{"service": fmt.Sprint("s", i%7), "pod": fmt.Sprint(i)},
				Start: at, End: at.Add(time.Minute)})
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
	if _, err := s.l.Put(ctx, incident.Incident{ID: "T-1", Start: time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC), End: time.Date(2026, 10, 28, 11, 0, 0, 0, time.UTC),
		Labels: incident.Labels{"service": {"s3"}}}, "ana", o); err != nil {
		t.Fatal(err)
	}
	for k := 0; k < 20; k++ {
		at := time.Date(2026, 10, 5, 2, 0, 0, 0, time.UTC).Add(time.Duration(k) * 7 * time.Hour)
		if _, err := s.l.PutWindow(ctx, ledger.Window{Title: fmt.Sprint("w", k), Start: at, End: at.Add(time.Hour), Every: "week"}, "ana", o); err != nil {
			t.Fatal(err)
		}
	}
	h := s.Handler()
	// Once before the budgets, its answer thrown away: what a process does
	// only the first time it answers is not what a page costs (the hours,
	// read once and kept). Each page below is still worked out afresh.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/rules", nil))
	// A page reads the tallies, the rules and what people keep a statement
	// or a few each: a tenth of the rules is past any of them (104 for the
	// overview on SQLite), and one for each rule, or each firing, far past.
	const kept = weeks * 7 * 3000
	pageMost := int64(n / 10)
	for _, path := range []string{"/rules", "/rules?weeks=12", "/rules?week=2026-10-26", "/rules?week=2026-10-26&perf=noisy&team=team3",
		"/firings?day=2026-11-02", "/firings?day=2026-11-02&label=service%3Ds3", "/firings?day=2026-11-02&label=service%3Ds3&label=pod%3D7",
		"/", "/?team=team2", "/?marks=none", "/incidents?week=2026-10-26&state=missed", "/incidents/T-1"} {
		s.cache.Lock()
		s.cache.gen = ""
		s.cache.Unlock()
		began := time.Now()
		w := httptest.NewRecorder()
		did := ledgerDid(t, s, func() { h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil)) })
		took := time.Since(began)
		t.Logf("%-70s %d %8d bytes  %d statements %d rows  %s", path, w.Code, w.Body.Len(), did.statements, did.rows, took.Round(time.Microsecond))
		if w.Code != http.StatusOK {
			t.Errorf("%s: %d", path, w.Code)
		}
		if did.statements > pageMost {
			t.Errorf("%s: %d statements, over %d: one for each rule or firing?", path, did.statements, pageMost)
		}
	}
	// A hundred at once, each its own write.
	var picks []string
	for i := 0; i < 100; i++ {
		picks = append(picks, fmt.Sprintf("Rule%04d", i*17))
	}
	day, err := s.l.Firings(ctx, ledger.FiringQuery{From: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), Black: true, Limit: 100})
	if err != nil || len(day) != 100 {
		t.Fatalf("%d firings: %v", len(day), err)
	}
	var ids []string
	for _, f := range day {
		ids = append(ids, pickRef(f))
	}
	for _, c := range []struct {
		path string
		form url.Values
	}{
		{"/rules/bulk", url.Values{"do": {"task"}, "rule": picks, "back": {"/rules"}}},
		{"/rules/bulk", url.Values{"do": {"status"}, "status": {"tune"}, "rule": picks, "back": {"/rules"}}},
		{"/firings/real", url.Values{"firing": ids, "back": {"/firings?day=2026-11-01"}}},
		{"/rules/bulk", url.Values{"do": {"propose"}, "propose": {"for"}, "for": {"15m"}, "rule": picks[:maxPropose], "back": {"/rules"}}},
	} {
		began := time.Now()
		var code int
		var body string
		var hd http.Header
		did := ledgerDid(t, s, func() { code, body, hd = post(t, h, c.path, c.form, "same-origin") })
		took := time.Since(began)
		t.Logf("POST %-65s %d %s  %d statements %d rows  %s", c.path+" "+c.form.Get("do"), code, hd.Get("Location"), did.statements, did.rows, took.Round(time.Microsecond))
		if code != http.StatusSeeOther || !strings.Contains(hd.Get("Location"), "done=") {
			t.Errorf("%s: %d %s", c.path, code, body)
		}
		// A few dozen statements for each of the hundred at most (on
		// SQLite, 10 for each status set, 13 for each firing "This was
		// real"), and the rows of judging again what they changed: fewer
		// than the firings kept. Every firing written for each would be
		// past both.
		if most := int64(30 * len(picks)); did.statements > most {
			t.Errorf("POST %s %s: %d statements, over %d", c.path, c.form.Get("do"), did.statements, most)
		}
		if did.rows > kept {
			t.Errorf("POST %s %s: %d rows written, over the %d firings kept", c.path, c.form.Get("do"), did.rows, kept)
		}
	}
}
