package serve

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRenamedRoutesTable: the table kept is a valid one.
func TestRenamedRoutesTable(t *testing.T) {
	parallel(t)
	if err := checkRenames(renamedRoutes); err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	for _, bad := range []routeRename{
		{"GET /a", "POST /b", since},
		{"/a", "/b", since},
		{"GET /a/{x}", "GET /b/{y}", since},
		{"GET /a", "GET /b", time.Time{}},
	} {
		if checkRenames([]routeRename{bad}) == nil {
			t.Errorf("%v: taken", bad)
		}
	}
}

// TestRenamedRoutes: a route under its old name is the new route: its
// handler, its roles and its checks, with Deprecation (RFC 9745: a date as
// @seconds) and a Link to the new path; the new path says neither; a method
// the old route doesn't have is no route.
func TestRenamedRoutes(t *testing.T) {
	parallel(t)
	s, _, _ := bulkServer(t, nil)
	since := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	s.routeRenames = []routeRename{
		{"GET /api/v1/whoami", "GET /api/v1/me", since},
		{"POST /settings/keep-given-back", "POST /settings/given-back", since},
		{"GET /api/v1/old/{id}/x", "GET /api/v1/proposals/{id}", since},
	}
	h := s.Handler()
	do := func(method, path, who string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(""))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		if who != "" {
			r.Header.Set("X-Forwarded-Email", who)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	// The same handler: the same answer, whoever asks.
	for _, who := range []string{"", "ed@example.com", "vi@example.com"} {
		old, now := do("GET", "/api/v1/whoami", who), do("GET", "/api/v1/me", who)
		if old.Code != http.StatusOK || old.Code != now.Code || old.Body.String() != now.Body.String() {
			t.Errorf("%q: old %d %s, new %d %s", who, old.Code, old.Body, now.Code, now.Body)
		}
		if got := old.Header().Get("Deprecation"); got != "@1796083200" {
			t.Errorf("%q: Deprecation %q", who, got)
		}
		if got := old.Header().Get("Link"); got != `</api/v1/me>; rel="successor-version"` {
			t.Errorf("%q: Link %q", who, got)
		}
		if now.Header().Get("Deprecation") != "" || now.Header().Get("Link") != "" {
			t.Errorf("%q: the new route says it is deprecated", who)
		}
	}
	// The new route's role: an admin's only. Not an editor, not a viewer, not
	// no one, by the old name either.
	for _, who := range []string{"", "vi@example.com", "ed@example.com"} {
		old, now := do("POST", "/settings/keep-given-back", who), do("POST", "/settings/given-back", who)
		if old.Code != now.Code {
			t.Errorf("%q: old %d, new %d", who, old.Code, now.Code)
		}
		if old.Code != http.StatusForbidden && who != "" && who != "ed@example.com" {
			t.Errorf("%q: %d", who, old.Code)
		}
	}
	if w := do("POST", "/settings/keep-given-back", "vi@example.com"); w.Code != http.StatusForbidden {
		t.Errorf("a viewer through the old name: %d", w.Code)
	}
	// A wildcard goes to the new path, and the query with it.
	if w := do("GET", "/api/v1/old/7/x?a=b", ""); w.Header().Get("Link") != `</api/v1/proposals/7?a=b>; rel="successor-version"` {
		t.Errorf("Link %q", w.Header().Get("Link"))
	}
	// Not the old route's method, nor a longer path: nothing is taken.
	for _, c := range []struct{ method, path string }{{"POST", "/api/v1/whoami"}, {"GET", "/api/v1/whoami/x"}, {"GET", "/api/v1/old//x"}} {
		if w := do(c.method, c.path, "ed@example.com"); w.Header().Get("Deprecation") != "" {
			t.Errorf("%s %s: taken", c.method, c.path)
		}
	}
}

// TestRenamedRoutesOtherPort: a port without the new route doesn't take the
// old name.
func TestRenamedRoutesOtherPort(t *testing.T) {
	parallel(t)
	s, _, _ := bulkServer(t, nil)
	s.routeRenames = []routeRename{{"GET /api/v1/whoami", "GET /api/v1/me", time.Unix(1, 0)}}
	r := httptest.NewRequest("GET", "/api/v1/whoami", nil)
	w := httptest.NewRecorder()
	s.MetricsHandler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound || w.Header().Get("Deprecation") != "" {
		t.Errorf("%d, Deprecation %q", w.Code, w.Header().Get("Deprecation"))
	}
}
