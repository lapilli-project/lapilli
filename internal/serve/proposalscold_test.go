package serve

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// TestProposalsColdScale: two hundred proposals of a for, each made
// against a version of the rules of its own, a thousand rules and more
// each, read with nothing of them worked out before: applied, as the
// review found them, then waiting (approved). The proposals list of every
// state and the API's list each take under a second, and allocate less
// than budgetCold, cold; the times and what each allocated, cold and warm,
// are logged.
func TestProposalsColdScale(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	const budgetCold = 200 << 20
	for _, state := range []string{ledger.StateApplied, ledger.StateApproved} {
		s, dir := server(t, nil)
		againstVersions(t, s, dir, 200, 1200, state)
		h := s.Handler()
		ask := func(path string, cold bool) (time.Duration, uint64, int) {
			s.cache.Lock()
			s.cache.gen = ""
			s.cache.Unlock()
			if cold {
				s.outlasts.Lock()
				s.outlasts.m = nil
				s.outlasts.Unlock()
			}
			var m0, m1 runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&m0)
			began := time.Now()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			took := time.Since(began)
			runtime.ReadMemStats(&m1)
			return took, m1.TotalAlloc - m0.TotalAlloc, w.Code
		}
		for _, path := range []string{"/proposals?state=all", "/api/v1/proposals"} {
			cold, coldBytes, code := ask(path, true)
			warm, warmBytes, _ := ask(path, false)
			t.Logf("%-8s %-22s %d  cold %s, %d MB  warm %s, %d MB", state, path, code, cold.Round(time.Millisecond), coldBytes>>20, warm.Round(time.Millisecond), warmBytes>>20)
			if code != http.StatusOK {
				t.Errorf("%s: %d", path, code)
			}
			if budget := time.Duration(float64(time.Second) * slow); cold > budget {
				t.Errorf("%s, %s: %s cold, over %s", state, path, cold, budget)
			}
			if coldBytes > budgetCold*uint64(slow) {
				t.Errorf("%s, %s: %d MB cold, over %d", state, path, coldBytes>>20, budgetCold>>20)
			}
		}
	}
}
