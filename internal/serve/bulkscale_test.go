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
// hundred, each within its budget.
func TestManyAtOnceScale(t *testing.T) {
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
			fmt.Fprintf(&b, "      - {alert: Rule%04d, expr: up == %d, labels: {severity: %s}}\n", i, i, []string{"critical", "warning", "info"}[i%3])
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
	budget := time.Duration(float64(time.Second) * slow)
	for _, path := range []string{"/rules", "/rules?weeks=12", "/rules?week=2026-10-26", "/rules?week=2026-10-26&perf=noisy&team=team3",
		"/firings?day=2026-11-02", "/firings?day=2026-11-02&label=service%3Ds3", "/firings?day=2026-11-02&label=service%3Ds3&label=pod%3D7",
		"/", "/?team=team2", "/?marks=none", "/incidents?week=2026-10-26&state=missed", "/incidents/T-1"} {
		s.cache.Lock()
		s.cache.gen = ""
		s.cache.Unlock()
		began := time.Now()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		took := time.Since(began)
		t.Logf("%-70s %d %8d bytes  %s", path, w.Code, w.Body.Len(), took.Round(time.Microsecond))
		if w.Code != http.StatusOK {
			t.Errorf("%s: %d", path, w.Code)
		}
		if took > budget {
			t.Errorf("%s took %s, over %s", path, took, budget)
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
		ids = append(ids, fmt.Sprint(f.ID))
	}
	for _, c := range []struct {
		path string
		form url.Values
	}{
		{"/rules/bulk", url.Values{"do": {"task"}, "rule": picks, "back": {"/rules"}}},
		{"/rules/bulk", url.Values{"do": {"status"}, "status": {"tune"}, "rule": picks, "back": {"/rules"}}},
		{"/firings/real", url.Values{"firing": ids, "back": {"/firings?day=2026-11-01"}}},
	} {
		began := time.Now()
		code, body, hd := post(t, h, c.path, c.form, "same-origin")
		took := time.Since(began)
		t.Logf("POST %-65s %d %s  %s", c.path+" "+c.form.Get("do"), code, hd.Get("Location"), took.Round(time.Microsecond))
		if code != http.StatusSeeOther || !strings.Contains(hd.Get("Location"), "done=") {
			t.Errorf("%s: %d %s", c.path, code, body)
		}
		if took > 10*budget {
			t.Errorf("%s took %s, over %s", c.path, took, 10*budget)
		}
	}
}
