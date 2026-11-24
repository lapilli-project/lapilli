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
// state and the API's list each allocate less than budgetCold, cold, and
// send the ledger a few statements for each proposal at most, cold (its
// version looked up, oldVersionsMost of them read) or warm, never one for
// each rule of a version (ledgerWork): counts, not times, so that a slow
// runner fails none. The
// times, what each allocated and the statements, cold and warm, are
// logged. It runs alone: what it allocates is the process's.
func TestProposalsColdScale(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	const budgetCold = 200 << 20
	for _, state := range []string{ledger.StateApplied, ledger.StateApproved} {
		s, dir := server(t, counting)
		againstVersions(t, s, dir, 200, 1200, state)
		h := s.Handler()
		ask := func(path string, cold bool) (time.Duration, uint64, int, int64) {
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
			did := ledgerDid(t, s, func() { h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil)) })
			took := time.Since(began)
			runtime.ReadMemStats(&m1)
			return took, m1.TotalAlloc - m0.TotalAlloc, w.Code, did.statements
		}
		for _, path := range []string{"/proposals?state=all", "/api/v1/proposals"} {
			cold, coldBytes, code, coldSent := ask(path, true)
			warm, warmBytes, _, warmSent := ask(path, false)
			t.Logf("%-8s %-22s %d  cold %s, %d MB, %d statements  warm %s, %d MB, %d statements", state, path, code,
				cold.Round(time.Millisecond), coldBytes>>20, coldSent, warm.Round(time.Millisecond), warmBytes>>20, warmSent)
			if code != http.StatusOK {
				t.Errorf("%s: %d", path, code)
			}
			if most := int64(4*200 + 100); coldSent > most || warmSent > most {
				t.Errorf("%s, %s: %d statements cold and %d warm, over %d", state, path, coldSent, warmSent, most)
			}
			if coldBytes > budgetCold*uint64(slow) {
				t.Errorf("%s, %s: %d MB cold, over %d", state, path, coldBytes>>20, budgetCold>>20)
			}
		}
	}
}
