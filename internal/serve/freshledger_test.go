package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The overview of a server restarted on a ledger that has been run, and
// not written to since, has the generation 0 and no cache filled: the
// credit cache is made when it is first filled, whatever the generation.
func TestOverviewOnFreshLedger(t *testing.T) {
	dir := t.TempDir()
	first, _ := server(t, func(c *Config) { c.Ledger = dir; keepRecord(c) })
	raw := ""
	if err := first.look(context.Background(), &raw); err != nil {
		t.Fatal(err)
	}
	first.jobs(context.Background(), true)
	first.Close()
	s, _ := server(t, func(c *Config) { c.Ledger = dir; keepRecord(c) })
	s.ready.Store(true)
	// A SQLite ledger starts its generation at 0 again; a Postgres one
	// keeps it across the restart, so the cache's zero value can't meet a
	// generation of 0 there, and there is nothing to test.
	if g := s.l.Gen(); g != 0 {
		t.Skipf("the ledger kept its generation, %d, across the restart (Postgres)", g)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK {
		t.Errorf("overview: %d", w.Code)
	}
}
