package serve

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestBigPageStreamed: a page bigger than the cache keeps goes to the
// response as it is written, not held whole besides, and isn't kept; one
// the cache keeps is kept.
func TestBigPageStreamed(t *testing.T) {
	s, _ := server(t, nil)
	big := bytes.Repeat([]byte("x"), cacheOne/4)
	for _, c := range []struct {
		parts int
		kept  bool
	}{{5, false}, {2, true}} {
		calls := 0
		var out *httptest.ResponseRecorder
		h := s.cached(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			for range c.parts {
				w.Write(big)
			}
			if streamed := out.Body.Len() > 0; streamed == c.kept {
				t.Errorf("%d parts: written to the response before the page is done %v", c.parts, streamed)
			}
		})
		for i := range 2 {
			out = httptest.NewRecorder()
			h(out, httptest.NewRequest(http.MethodGet, "/big?"+strings.Repeat("p", c.parts), nil))
			if out.Code != http.StatusOK || out.Body.Len() != c.parts*len(big) || out.Header().Get("Content-Type") != "text/html; charset=utf-8" {
				t.Errorf("%d parts, %d: %d, %d bytes, %q", c.parts, i, out.Code, out.Body.Len(), out.Header().Get("Content-Type"))
			}
		}
		if want := map[bool]int{true: 1, false: 2}[c.kept]; calls != want {
			t.Errorf("%d parts: worked out %d times, not %d", c.parts, calls, want)
		}
	}
}

// TestPagesTakeTurns: pages are worked out a few at a time; one that
// waits too long for its turn is answered 503, to try again after a
// while; a page kept already, the API and the health checks don't wait.
func TestPagesTakeTurns(t *testing.T) {
	s, _ := server(t, nil)
	h := s.Handler()
	if code, _, _ := get(t, h, "/runs"); code != http.StatusOK { // kept
		t.Fatalf("%d", code)
	}
	was := renderWait
	defer func() { renderWait = was }()
	renderWait = 50 * time.Millisecond
	for range pageRenders {
		s.renders <- struct{}{}
	}
	began := time.Now()
	code, body, hd := get(t, h, "/rules")
	if code != http.StatusServiceUnavailable || hd.Get("Retry-After") == "" || !strings.Contains(body, "busy with many pages at once") || time.Since(began) < renderWait {
		t.Errorf("all turns taken: %d, Retry-After %q after %s", code, hd.Get("Retry-After"), time.Since(began))
	}
	for _, path := range []string{"/runs", "/api/v1/rules", "/healthz", "/api/v1/runs"} {
		if code, _, _ := get(t, h, path); code != http.StatusOK {
			t.Errorf("%s: %d", path, code)
		}
	}
	<-s.renders
	if code, _, _ := get(t, h, "/rules"); code != http.StatusOK {
		t.Errorf("a turn given back: %d", code)
	}
	for range pageRenders - 1 {
		<-s.renders
	}
	if len(s.renders) != 0 {
		t.Errorf("%d turns not given back", len(s.renders))
	}
}
