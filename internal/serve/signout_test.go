package serve

// Signing out, and the sign-in's cookies, held to what SECURITY.md and
// docs/design/signin.md promise against what anyone the identity provider
// signs in can send before a role is asked (0.34.1).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/signin"
)

// signOuts is how often the ledger's record of who signed out was
// written.
func signOuts(t *testing.T, s *Server) int64 {
	t.Helper()
	n, err := s.l.MetaCount(t.Context(), metaSignedOut)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Signing out ends "every bearer token issued before" (docs/design/
// signin.md). The record of who signed out is kept as long as a session
// lasts (--session-max-age, 12h by default), and pruned when it is merged
// again (anyone else signing out) or read at a start. An ID token lasts as
// long as the identity provider says (Dex gives 24h), so a bearer token is
// taken only while a session made when it was issued would be: one copied
// before its person signed out isn't taken again once the record is
// pruned.
func TestBearerTokenOlderThanASession(t *testing.T) {
	s, idp := oidcServer(t, nil) // --session-max-age: the default, 12h
	h := s.Handler()
	call := func(h http.Handler, tok string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/v1/me", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	// boss (an admin, by group ops) signs in, and holds an ID token as an
	// API client would; its lifetime is the provider's: 24h, Dex's default.
	s.now = func() time.Time { return now.Add(-time.Minute) }
	b := newBrowser(t, h)
	b.signIn(idp, "/", claimsOf("boss@example.com", "ops"))
	idp.Mutate = func(c map[string]any) { c["exp"] = now.Add(24 * time.Hour).Unix() }
	stolen := idp.IDToken(claimsOf("boss@example.com", "ops"))
	idp.Mutate = nil

	// The token leaks (a CI log, a laptop); boss signs out to end it.
	s.now = func() time.Time { return now.Add(time.Second) }
	if w := b.do("POST", "/auth/logout", nil); w.Code != http.StatusSeeOther {
		t.Fatalf("signing out: %d", w.Code)
	}
	if w := call(h, stolen); w.Code != http.StatusUnauthorized {
		t.Fatalf("right after signing out, the token from before: %d, want 401", w.Code)
	}

	later := now.Add(12*time.Hour + time.Minute)
	// In the process still running, once anyone else signs out:
	// the record is merged, and boss's part pruned.
	s.now = func() time.Time { return later }
	idp.Now = func() time.Time { return later }
	other := newBrowser(t, h)
	other.signIn(idp, "/", claimsOf("ana@example.com"))
	if other.cookies["__Host-lapilli-session"] == nil {
		t.Fatal("ana isn't signed in")
	}
	if w := other.do("POST", "/auth/logout", nil); w.Code != http.StatusSeeOther {
		t.Fatalf("ana signing out: %d", w.Code)
	}
	if w := call(h, stolen); w.Code != http.StatusUnauthorized {
		t.Errorf("a bearer token issued before its person signed out, %v after, once anyone else signs out: %d", later.Sub(now), w.Code)
	}
	// A token issued since is taken.
	if w := call(h, idp.IDToken(claimsOf("boss@example.com", "ops"))); w.Code != http.StatusOK {
		t.Errorf("a token issued now: %d", w.Code)
	}
	// 12h and a minute later, still within the token's own 24h: lapilli
	// starts again (an upgrade), on the same ledger, and reads the record,
	// pruned.
	cfg := s.cfg
	cfg.OIDC = &signin.OIDC{Issuer: idp.URL, ClientID: "lapilli", SecretFile: s.cfg.OIDC.SecretFile, Claims: s.cfg.OIDC.Claims}
	s.Close() // one process writes a ledger at a time (Postgres)
	s2, err := New(context.Background(), cfg, &history{}, s.log)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	s2.now = func() time.Time { return later }
	if w := call(s2.Handler(), stolen); w.Code != http.StatusUnauthorized || !strings.Contains(w.Header().Get("WWW-Authenticate"), "--session-max-age") {
		t.Errorf("after a restart %v after signing out, a bearer token issued before: %d %q, want 401 for its age", later.Sub(now), w.Code, w.Header().Get("WWW-Authenticate"))
	}

	// The same, sooner, with a shorter --session-max-age: 5m, the least
	// allowed, against a 1h token (Google's, Entra ID's, Okta's).
	s3, idp3 := oidcServer(t, func(c *Config) { c.SessionMaxAge = 5 * time.Minute })
	h3 := s3.Handler()
	s3.now = func() time.Time { return now.Add(-time.Minute) }
	b3 := newBrowser(t, h3)
	b3.signIn(idp3, "/", claimsOf("boss@example.com", "ops"))
	stolen3 := idp3.IDToken(claimsOf("boss@example.com", "ops")) // exp: now + 1h
	s3.now = func() time.Time { return now.Add(4 * time.Minute) }
	if w := call(h3, stolen3); w.Code != http.StatusOK {
		t.Fatalf("5m: a token of 4m: %d", w.Code)
	}
	s3.now = func() time.Time { return now.Add(time.Second) }
	b3.do("POST", "/auth/logout", nil)
	if w := call(h3, stolen3); w.Code != http.StatusUnauthorized {
		t.Fatalf("5m: right after signing out: %d", w.Code)
	}
	s3.now = func() time.Time { return now.Add(6 * time.Minute) }
	ed3 := newBrowser(t, h3)
	s3.now = func() time.Time { return now.Add(5 * time.Minute) }
	ed3.signIn(idp3, "/", claimsOf("ed@example.com"))
	s3.now = func() time.Time { return now.Add(6 * time.Minute) }
	ed3.do("POST", "/auth/logout", nil)
	if w := call(h3, stolen3); w.Code == http.StatusOK {
		t.Errorf("--session-max-age 5m: a 1h bearer token from before signing out is taken again 6m after it (role %v)", me(t, w)["role"])
	}
}

// POST /auth/logout is on an open path: it needs a session cookie, not a
// role, and whoever the identity provider signs in gets one (the callback
// issues it; letIn is asked of each later request). For a provider people
// sign up to (Cognito or Keycloak with self-registration, Auth0, a public
// Google client), sign-ins under names of one's own are a script. Who
// signed out is kept only for people a role lets in, and only once a
// session: a stranger's sign-outs, or one cookie signing out again and
// again, write nothing, and can't push an admin's sign-out out of the
// record.
func TestSignOutByStrangers(t *testing.T) {
	s, idp := oidcServer(t, nil)
	h := s.Handler()
	s.now = func() time.Time { return now.Add(-time.Minute) }
	boss := newBrowser(t, h)
	boss.signIn(idp, "/", claimsOf("boss@example.com", "ops"))
	copied := *boss.cookies["__Host-lapilli-session"]
	ed := newBrowser(t, h)
	ed.signIn(idp, "/", claimsOf("ed@example.com"))
	edCopy := *ed.cookies["__Host-lapilli-session"]
	s.now = func() time.Time { return now }
	if w := boss.do("POST", "/auth/logout", nil); w.Code != http.StatusSeeOther {
		t.Fatalf("signing out: %d", w.Code)
	}
	if n := signOuts(t, s); n != 1 {
		t.Fatalf("boss's sign-out: %d writes, want 1", n)
	}
	thief := newBrowser(t, h)
	thief.cookies[copied.Name] = &copied
	if w := thief.do("GET", "/api/v1/me", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("the copy right after signing out: %d, want 401", w.Code)
	}

	logout := func(c *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/auth/logout", nil)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.AddCookie(c)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	// boss's copy, signed out again and again, a moment apart: the cookie
	// is taken away, and nothing written.
	for i := range 20 {
		s.now = func() time.Time { return now.Add(time.Duration(i+1) * time.Millisecond) }
		if w := logout(&copied); w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Set-Cookie"), "Max-Age=0") {
			t.Fatalf("a session signed out of, signing out again: %d %v", w.Code, w.Header())
		}
	}
	if n := signOuts(t, s); n != 1 {
		t.Errorf("a session already signed out of, signed out 20 times more: %d writes, want none", n-1)
	}

	// A stranger no role names signs in through the provider, and is let in
	// nowhere: their session cookie is good for /auth/logout, which signs
	// them out as it should, and writes nothing.
	stranger := newBrowser(t, h)
	stranger.signIn(idp, "/", claimsOf("stranger@public.example"))
	if w := stranger.do("GET", "/api/v1/me", nil); w.Code != http.StatusForbidden {
		t.Fatalf("a stranger: %d, want 403 (let in nowhere)", w.Code)
	}
	strangerCookie := *stranger.cookies["__Host-lapilli-session"]
	for i := range 200 {
		s.now = func() time.Time { return now.Add(time.Duration(i+1) * time.Millisecond) }
		if w := logout(&strangerCookie); w.Code != http.StatusSeeOther {
			t.Fatalf("a stranger signing out: %d", w.Code)
		}
	}
	// More people the provider signs in, none with a role, sign out once
	// each. Their cookies are what callback would give each (Issue),
	// skipping the trips to the stand-in provider.
	for i := range 2 * s.in.out.max {
		rec := httptest.NewRecorder()
		s.now = func() time.Time { return now.Add(time.Second + time.Duration(i)*time.Millisecond) }
		if _, err := s.in.cookies.Issue(rec, signin.Identity{User: fmt.Sprintf("signup-%05d@public.example", i)}); err != nil {
			t.Fatal(err)
		}
		if w := logout(rec.Result().Cookies()[0]); w.Code != http.StatusSeeOther {
			t.Fatalf("sign-out %d: %d", i, w.Code)
		}
	}
	if n := signOuts(t, s); n != 1 {
		t.Errorf("sign-outs of people no role lets in: %d writes, want none", n-1)
	}
	if raw, _ := s.l.Meta(t.Context(), metaSignedOut); strings.Contains(raw, "public.example") {
		t.Errorf("the record holds people no role lets in: %.200s", raw)
	}
	if w := thief.do("GET", "/api/v1/me", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("after %d sign-outs by people no role lets in, a copy of an admin's cookie from before they signed out: %d, want 401",
			2*s.in.out.max, w.Code)
	}
	edMe := newBrowser(t, h)
	edMe.cookies[edCopy.Name] = &edCopy
	if w := edMe.do("GET", "/api/v1/me", nil); w.Code != http.StatusOK {
		t.Errorf("ed, who never signed out: %d", w.Code)
	}
}

// The record is bounded by people. Full, it doesn't forget whoever signed
// out first: everyone's sessions and tokens made before then are ended,
// failing closed, and those made after go on.
func TestSignOutRecordFull(t *testing.T) {
	s, idp := oidcServer(t, nil)
	s.in.out.max = 5
	h := s.Handler()
	s.now = func() time.Time { return now.Add(-time.Minute) }
	boss := newBrowser(t, h)
	boss.signIn(idp, "/", claimsOf("boss@example.com", "ops"))
	copied := *boss.cookies["__Host-lapilli-session"]
	ed := newBrowser(t, h)
	ed.signIn(idp, "/", claimsOf("ed@example.com"))
	s.now = func() time.Time { return now }
	boss.do("POST", "/auth/logout", nil)
	// Then more people a role lets in than the record holds sign out, each
	// a moment later: boss's is the oldest, let go of.
	for i := range s.in.out.max {
		at := now.Add(time.Duration(i+1) * time.Second)
		s.now, idp.Now = func() time.Time { return at }, func() time.Time { return at }
		p := newBrowser(t, h)
		p.signIn(idp, "/", claimsOf(fmt.Sprintf("viewer-%d@example.com", i)))
		if w := p.do("POST", "/auth/logout", nil); w.Code != http.StatusSeeOther {
			t.Fatalf("sign-out %d: %d", i, w.Code)
		}
	}
	raw, _ := s.l.Meta(t.Context(), metaSignedOut)
	if strings.Contains(raw, "boss@") {
		t.Fatalf("the record holds boss still: %s", raw)
	}
	thief := newBrowser(t, h)
	thief.cookies[copied.Name] = &copied
	if w := thief.do("GET", "/api/v1/me", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("the record full, boss let go of: a copy of boss's cookie from before signing out is taken again: %d", w.Code)
	}
	// ed, signed in before boss signed out, is signed out with everyone of
	// then; ed signing in again goes on.
	if w := ed.do("GET", "/api/v1/me", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("a session from before everyone's time: %d, want 401", w.Code)
	}
	fresh := newBrowser(t, h)
	fresh.signIn(idp, "/", claimsOf("ed@example.com"))
	if w := fresh.do("GET", "/api/v1/me", nil); w.Code != http.StatusOK {
		t.Errorf("signed in again after: %d", w.Code)
	}
	// Started again on the same ledger, it reads it so too.
	cfg := s.cfg
	cfg.OIDC = &signin.OIDC{Issuer: idp.URL, ClientID: "lapilli", SecretFile: s.cfg.OIDC.SecretFile, Claims: s.cfg.OIDC.Claims}
	s.Close() // one process writes a ledger at a time (Postgres)
	s2, err := New(context.Background(), cfg, &history{}, s.log)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	s2.now = s.now
	if w := newBrowser(t, s2.Handler()); true {
		w.cookies[copied.Name] = &copied
		if got := w.do("GET", "/api/v1/me", nil); got.Code != http.StatusUnauthorized {
			t.Errorf("started again: a copy of boss's cookie: %d", got.Code)
		}
	}
}

// A request reads the record of who signed out only when the ledger counts
// a change to it: the record is up to 10,000 people, and every request of
// anyone signed in asks it.
func TestSignedOutReadOnChange(t *testing.T) {
	s, idp := oidcServer(t, nil)
	h := s.Handler()
	s.now = func() time.Time { return now.Add(-time.Minute) }
	ed := newBrowser(t, h)
	ed.signIn(idp, "/", claimsOf("ed@example.com"))
	if w := ed.do("GET", "/api/v1/me", nil); w.Code != http.StatusOK {
		t.Fatalf("ed: %d", w.Code)
	}
	// Written as a process that doesn't count would: not read again.
	s.now = func() time.Time { return now }
	if err := s.l.SetMeta(t.Context(), metaSignedOut, fmt.Sprintf(`{"ed@example.com":%d}`, now.UnixMilli())); err != nil {
		t.Fatal(err)
	}
	if w := ed.do("GET", "/api/v1/me", nil); w.Code != http.StatusOK {
		t.Errorf("the record read again on a request though its count didn't move: %d", w.Code)
	}
	// Counted, it is.
	if err := s.l.UpdateMetaCounted(t.Context(), metaSignedOut, func(old string) (string, bool) { return old, true }); err != nil {
		t.Fatal(err)
	}
	if w := ed.do("GET", "/api/v1/me", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("the record's count moved, and isn't read: %d", w.Code)
	}
}

// Each GET /auth/login, which any page can send a browser to (a top-level
// navigation, a window it opened and points there again and again), leaves
// a cookie of its own, lapilli-login-HASH, for ten minutes. Browsers keep
// 180 cookies a site (Chrome's kDomainMaxCookies, Firefox's
// network.cookie.maxPerHost) and, past it, drop the least recently used,
// the session cookie among them: a page elsewhere could sign people out.
// The login takes away those the browser holds but the newest few.
func TestLoginCookiesPruned(t *testing.T) {
	s, idp := oidcServer(t, nil)
	h := s.Handler()
	b := newBrowser(t, h)
	b.signIn(idp, "/", claimsOf("ed@example.com"))
	if b.cookies["__Host-lapilli-session"] == nil {
		t.Fatal("not signed in")
	}
	var last []*httptest.ResponseRecorder
	for i := range 200 {
		at := now.Add(time.Duration(i) * time.Second)
		s.now = func() time.Time { return at }
		w := b.do("GET", "/auth/login?next=/", nil)
		if w.Code != http.StatusFound {
			t.Fatalf("/auth/login: %d", w.Code)
		}
		last = append(last, w)
	}
	logins := 0
	for name := range b.cookies {
		if strings.HasPrefix(name, "__Host-lapilli-login-") {
			logins++
		}
	}
	if logins > keepLogins {
		t.Errorf("200 visits to /auth/login, as a page elsewhere can send a browser on, leave %d login cookies in the browser beside the session, want %d at most", logins, keepLogins)
	}
	if b.cookies["__Host-lapilli-session"] == nil {
		t.Error("the session's cookie was taken away")
	}
	// The newest sign-ins, as tabs signing in at once, each come back.
	for _, w := range last[len(last)-keepLogins:] {
		back := idp.Authorize(t, w.Header().Get("Location"), claimsOf("ed@example.com"))
		if got := b.do("GET", back, nil); got.Code != http.StatusSeeOther {
			t.Errorf("one of the newest sign-ins coming back: %d", got.Code)
		}
	}
}

// --read-only takes no changes, but signing out must work: who signed out
// is kept in the ledger for people a role lets in, and for no one else.
// With --oidc-issuer only those a role names come in at all.
func TestSignOutReadOnly(t *testing.T) {
	s, idp := oidcServer(t, func(c *Config) { c.ReadOnly = true; c.Editors = nil; c.Admins = nil })
	h := s.Handler()
	b := newBrowser(t, h)
	b.signIn(idp, "/", claimsOf("stranger@public.example"))
	if w := b.do("GET", "/api/v1/me", nil); w.Code != http.StatusForbidden {
		t.Fatalf("a stranger: %d, want 403", w.Code)
	}
	if w := b.do("POST", "/incidents", nil); w.Code == http.StatusSeeOther || w.Code == http.StatusOK {
		t.Fatalf("a stranger writing an incident under --read-only: %d", w.Code)
	}
	before, _ := s.l.Meta(t.Context(), metaSignedOut)
	if w := b.do("POST", "/auth/logout", nil); w.Code != http.StatusSeeOther {
		t.Fatalf("signing out: %d", w.Code)
	}
	after, _ := s.l.Meta(t.Context(), metaSignedOut)
	if after != before {
		t.Errorf("--read-only, and a person no role lets in: POST /auth/logout wrote the ledger (meta x.%s: %q -> %q)", metaSignedOut, before, after)
	}
	// A viewer signs out: kept, and a copy of the cookie refused.
	s.now = func() time.Time { return now.Add(-time.Minute) }
	ana := newBrowser(t, h)
	ana.signIn(idp, "/", claimsOf("ana@example.com"))
	copied := *ana.cookies["__Host-lapilli-session"]
	s.now = func() time.Time { return now }
	if w := ana.do("POST", "/auth/logout", nil); w.Code != http.StatusSeeOther {
		t.Fatalf("ana signing out: %d", w.Code)
	}
	thief := newBrowser(t, h)
	thief.cookies[copied.Name] = &copied
	if w := thief.do("GET", "/api/v1/me", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("--read-only: a copy of a viewer's cookie after signing out: %d", w.Code)
	}
}

// A group is taken as a name is: one holding a character that doesn't
// show (a zero-width space, a direction mark) is dropped, from a token and
// from --groups-header, and gives no role (docs/design/signin.md).
func TestGroupsThatDontShow(t *testing.T) {
	s, idp := oidcServer(t, nil)
	h := s.Handler()
	for _, g := range []string{"ops​", "o​ps", "‎ops", "ops­"} {
		b := newBrowser(t, h)
		b.signIn(idp, "/", claimsOf("ana@example.com", g, "sre"))
		w := b.do("GET", "/api/v1/me", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%q: %d", g, w.Code)
		}
		m := me(t, w)
		if m["role"] != "viewer" || strings.Contains(fmt.Sprint(m["groups"]), "ops") {
			t.Errorf("a group %q: %v", g, m)
		}
	}
	// The admins' own group still makes an admin.
	b := newBrowser(t, h)
	b.signIn(idp, "/", claimsOf("ana@example.com", "ops"))
	if m := me(t, b.do("GET", "/api/v1/me", nil)); m["role"] != "admin" {
		t.Errorf("ops: %v", m)
	}

	p, _ := server(t, func(c *Config) {
		c.UserHeader, c.GroupsHeader = "X-Forwarded-Email", "X-Forwarded-Groups"
		c.Admins = []string{"group:ops"}
	})
	for groups, want := range map[string]string{"ops": "admin", "dev, ops": "admin", "ops​": "viewer", "dev, ⁦ops⁩": "viewer"} {
		r := httptest.NewRequest("GET", "/api/v1/me", nil)
		r.Header.Set("X-Forwarded-Email", "ana@example.com")
		r.Header.Set("X-Forwarded-Groups", groups)
		w := httptest.NewRecorder()
		p.Handler().ServeHTTP(w, r)
		if m := me(t, w); m["role"] != want {
			t.Errorf("--groups-header %q: %v, want %s", groups, m["role"], want)
		}
	}
}

// *@DOMAIN is a domain in any case of its ASCII letters, and no more:
// Unicode's folding takes the Kelvin sign (U+212A) for a k and a long s
// (U+017F) for an s.
func TestDomainFoldsASCIIOnly(t *testing.T) {
	s, _ := server(t, func(c *Config) {
		c.UserHeader, c.Viewers, c.Editors = "X-Forwarded-Email", []string{"*@kiosk.example"}, []string{"ed@other.example"}
	})
	h := s.Handler()
	for who, want := range map[string]int{"ana@kiosk.example": 200, "ana@KIOSK.EXAMPLE": 200, "ana@Kiosk.example": 403, "ana@kioſk.example": 403} {
		r := httptest.NewRequest("GET", "/api/v1/me", nil)
		r.Header.Set("X-Forwarded-Email", who)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("%q: %d, want %d", who, w.Code, want)
		}
	}
}
