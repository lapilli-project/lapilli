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
)

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
	s, _ := server(t, nil)
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
	h := s.Handler()
	for _, path := range []string{"/", "/rules", "/rules?weeks=12", "/rules/Rule007", "/incidents", "/incidents?weeks=12",
		"/firings?day=2026-11-02", "/metrics", "/api/v1/report?firings=false", "/api/v1/report"} {
		s.cache.Lock()
		s.cache.gen = "" // worked out afresh
		s.cache.Unlock()
		began := time.Now()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		took := time.Since(began)
		t.Logf("%-32s %4d %8d bytes  %s", path, w.Code, w.Body.Len(), took.Round(time.Microsecond))
		budget := time.Duration(float64(time.Second) * max(1, k))
		if path == "/api/v1/report" {
			budget *= 4 // every firing of the week, as JSON
		}
		if w.Code != 200 || took > budget {
			t.Errorf("%s: %d in %s, over %s", path, w.Code, took, budget)
		}
	}
}
