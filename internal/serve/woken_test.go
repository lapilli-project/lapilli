package serve

import (
	"net/http"
	"testing"
)

// TestWokenWalkedOnce: the notifications kept are walked for those that
// woke people for nothing once for a generation of the ledger, however
// many pages count them.
func TestWokenWalkedOnce(t *testing.T) {
	s := drillServer(t, true)
	h := s.Handler()
	before := wokenWalks.Load()
	for _, path := range []string{"/rules?week=2026-10-26", "/rules", "/rules?team=payments", "/", "/?team=search"} {
		s.cache.Lock()
		s.cache.gen = ""
		s.cache.Unlock()
		if code, _, _ := get(t, h, path); code != http.StatusOK {
			t.Fatalf("%s: %d", path, code)
		}
	}
	if n := wokenWalks.Load() - before; n != 1 {
		t.Errorf("walked %d times", n)
	}
}
