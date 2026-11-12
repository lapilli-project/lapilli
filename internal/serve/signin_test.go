package serve

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"golang.org/x/net/html"

	"lapilli.dev/internal/signin"
	"lapilli.dev/internal/signin/idptest"
)

const extURL = "https://lapilli.example.com"

// oidcServer is a server that signs people in with a provider of its own:
// ops (a group) are admins, ed@example.com an editor.
func oidcServer(t *testing.T, change func(*Config)) (*Server, *idptest.IdP) {
	t.Helper()
	idp := idptest.New(t, "lapilli", "the-client-secret", func() time.Time { return now })
	secret := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secret, []byte("the-client-secret\n"), 0o600)
	key := filepath.Join(t.TempDir(), "session-key")
	os.WriteFile(key, []byte(strings.Repeat("k", 40)+"\n"), 0o600)
	s, _ := server(t, func(c *Config) {
		c.ExternalURL, c.SessionKey = extURL, key
		c.OIDC = &signin.OIDC{Issuer: idp.URL, ClientID: "lapilli", SecretFile: secret, Scopes: []string{"email", "groups"},
			Claims: signin.Claims{UserClaim: "email", GroupsClaim: "groups", Verified: true}}
		c.Admins, c.Editors = []string{"group:ops"}, []string{"user:ed@example.com"}
		c.Viewers = []string{"*@example.com"}
		if change != nil {
			change(c)
		}
	})
	return s, idp
}

// browser plays a browser: it keeps the cookies it is given, and sends
// what it posts as from the page's own site.
type browser struct {
	t       *testing.T
	h       http.Handler
	cookies map[string]*http.Cookie
}

func newBrowser(t *testing.T, h http.Handler) *browser {
	return &browser{t: t, h: h, cookies: map[string]*http.Cookie{}}
}

func (b *browser) do(method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	b.t.Helper()
	if u, err := url.Parse(target); err == nil && u.IsAbs() {
		target = u.RequestURI() // the server's own, at --external-url
	}
	r := httptest.NewRequest(method, target, nil)
	if method == http.MethodPost {
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	for _, c := range b.cookies {
		r.AddCookie(c)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	b.h.ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = &http.Cookie{Name: c.Name, Value: c.Value}
		}
	}
	return w
}

// signIn signs in as whoever claims says, through the provider, from a
// page asked for to the page again; it gives the last answer.
func (b *browser) signIn(idp *idptest.IdP, page string, claims map[string]any) *httptest.ResponseRecorder {
	b.t.Helper()
	w := b.do("GET", page, nil)
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), extURL+"/auth/login?next=") {
		b.t.Fatalf("%s, not signed in: %d %v", page, w.Code, w.Header())
	}
	w = b.do("GET", w.Header().Get("Location"), nil)
	if w.Code != http.StatusFound || !strings.HasPrefix(w.Header().Get("Location"), idp.URL+"/auth?") {
		b.t.Fatalf("/auth/login: %d %v %s", w.Code, w.Header(), w.Body)
	}
	back := idp.Authorize(b.t, w.Header().Get("Location"), claims)
	if !strings.HasPrefix(back, extURL+"/auth/callback?") {
		b.t.Fatalf("sent back to %s", back)
	}
	return b.do("GET", back, nil)
}

func claimsOf(email string, groups ...string) map[string]any {
	g := []any{}
	for _, x := range groups {
		g = append(g, x)
	}
	return map[string]any{"email": email, "email_verified": true, "groups": g}
}

func me(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	return m
}

// Signed in through the provider, a person comes back to the page they
// asked for, with the role the lists give their name or their groups.
func TestSignInFlow(t *testing.T) {
	s, idp := oidcServer(t, nil)
	h := s.Handler()
	for _, c := range []struct {
		claims map[string]any
		role   string
	}{
		{claimsOf("boss@example.com", "ops", "sre"), "admin"},
		{claimsOf("ed@example.com"), "editor"},
		{claimsOf("viewer@example.com", "sre"), "viewer"},
	} {
		b := newBrowser(t, h)
		w := b.signIn(idp, "/rules?weeks=4", c.claims)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/rules?weeks=4" {
			t.Fatalf("back from signing in: %d %v", w.Code, w.Header())
		}
		if _, ok := b.cookies["__Host-lapilli-session"]; !ok {
			t.Fatalf("no session cookie: %v", b.cookies)
		}
		if _, ok := b.cookies["__Host-lapilli-login"]; ok {
			t.Error("the sign-in's cookie is kept")
		}
		m := me(t, b.do("GET", "/api/v1/me", nil))
		if m["user"] != c.claims["email"] || m["role"] != c.role {
			t.Errorf("%v: %v", c.claims, m)
		}
		page := b.do("GET", "/rules?weeks=4", nil)
		if page.Code != 200 || !strings.Contains(page.Body.String(), c.claims["email"].(string)) || !strings.Contains(page.Body.String(), `action="/auth/logout"`) {
			t.Errorf("the page as %v: %d", c.claims["email"], page.Code)
		}
		// Pages differ by who reads them: kept apart in the cache.
		if !strings.Contains(strings.Join(page.Header().Values("Vary"), ","), "Cookie") {
			t.Errorf("Vary: %v", page.Header())
		}
	}
	// An editor writes; a viewer doesn't.
	b := newBrowser(t, h)
	b.signIn(idp, "/", claimsOf("ed@example.com"))
	if w := b.do("POST", "/api/v1/incidents", map[string]string{"Content-Type": "application/json"}); w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
		t.Errorf("an editor's write: %d %s", w.Code, w.Body)
	}
	v := newBrowser(t, h)
	v.signIn(idp, "/", claimsOf("viewer@example.com"))
	r := httptest.NewRequest("POST", "/api/v1/incidents", strings.NewReader(`{"title": "x", "start": "2026-11-02T06:00:00Z"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	for _, c := range v.cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("a viewer's write: %d %s", w.Code, w.Body)
	}
}

// What isn't signed in: a browser is sent to sign in; an API client, an
// agent and a form get 401; the health checks, the static files and the
// choice of language are anyone's.
func TestNotSignedIn(t *testing.T) {
	s, _ := oidcServer(t, nil)
	h := s.Handler()
	for path, want := range map[string]int{
		"/": http.StatusSeeOther, "/rules/Errors": http.StatusSeeOther, "/metrics": http.StatusSeeOther,
		"/api/v1/me": http.StatusUnauthorized, "/api/v1/report": http.StatusUnauthorized,
		"/healthz": 200, "/static/lapilli.css": 200, "/lang?to=ko&back=/": http.StatusSeeOther, "/auth/signed-out": 200,
	} {
		code, body, hdr := get(t, h, path)
		if code != want {
			t.Errorf("%s: %d, want %d (%s)", path, code, want, body)
		}
		if strings.HasPrefix(path, "/api/") && (hdr.Get("WWW-Authenticate") == "" || !strings.Contains(body, `"error"`)) {
			t.Errorf("%s: %v %s", path, hdr, body)
		}
	}
	code, _, hdr := get(t, h, "/rules?weeks=4")
	if loc := hdr.Get("Location"); code != http.StatusSeeOther || loc != extURL+"/auth/login?next="+url.QueryEscape("/rules?weeks=4") {
		t.Errorf("a page: %d %s", code, loc)
	}
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		r := httptest.NewRequest(m, "/api/v1/incidents/INC-1", strings.NewReader("{}"))
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d", m, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("/mcp: %d", w.Code)
	}
	// The plain header of a sign-in proxy means nothing here, on the pages
	// anyone may see too, even were it set beside the sign-in.
	s.cfg.UserHeader = "X-Forwarded-Email"
	r0 := httptest.NewRequest("GET", "/auth/signed-out", nil)
	r0.Header.Set("X-Forwarded-Email", "boss@example.com")
	w0 := httptest.NewRecorder()
	h.ServeHTTP(w0, r0)
	if strings.Contains(w0.Body.String(), "boss@example.com") {
		t.Error("the header named the reader of a page anyone may see")
	}
	s.cfg.UserHeader = ""
	r = httptest.NewRequest("GET", "/api/v1/me", nil)
	r.Header.Set("X-Forwarded-Email", "boss@example.com")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("a proxy's header: %d", w.Code)
	}
}

// Each way a sign-in can be forged, replayed or misled is refused.
func TestSignInRefusals(t *testing.T) {
	s, idp := oidcServer(t, nil)
	h := s.Handler()
	start := func() (*browser, string) {
		b := newBrowser(t, h)
		w := b.do("GET", "/auth/login?next=/rules", nil)
		return b, w.Header().Get("Location")
	}
	// The state isn't the one the cookie holds.
	b, auth := start()
	back := idp.Authorize(t, auth, claimsOf("boss@example.com", "ops"))
	u, _ := url.Parse(back)
	q := u.Query()
	q.Set("state", "x"+q.Get("state"))
	u.RawQuery = q.Encode()
	if w := b.do("GET", u.String(), nil); w.Code != http.StatusBadRequest || b.cookies["__Host-lapilli-session"] != nil {
		t.Errorf("another state: %d", w.Code)
	}
	// No cookie at all: a callback made elsewhere (login CSRF).
	_, auth = start()
	back = idp.Authorize(t, auth, claimsOf("boss@example.com", "ops"))
	if w := newBrowser(t, h).do("GET", back, nil); w.Code != http.StatusBadRequest {
		t.Errorf("no sign-in's cookie: %d", w.Code)
	}
	// The same callback twice, cookie and all.
	b, auth = start()
	back = idp.Authorize(t, auth, claimsOf("boss@example.com", "ops"))
	saved := map[string]*http.Cookie{}
	for k, v := range b.cookies {
		saved[k] = v
	}
	if w := b.do("GET", back, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("the first time: %d %s", w.Code, w.Body)
	}
	replay := newBrowser(t, h)
	replay.cookies = saved
	if w := replay.do("GET", back, nil); w.Code != http.StatusBadRequest || replay.cookies["__Host-lapilli-session"] != nil {
		t.Errorf("replayed: %d", w.Code)
	}
	// Tokens the provider shouldn't have given: another client's, expired,
	// an address not verified.
	for name, change := range map[string]func(map[string]any){
		"another audience":   func(c map[string]any) { c["aud"] = "another-client" },
		"expired":            func(c map[string]any) { c["exp"] = now.Add(-time.Minute).Unix() },
		"email not verified": func(c map[string]any) { c["email_verified"] = false },
		"another nonce":      func(c map[string]any) { c["nonce"] = "x" },
		"another issuer":     func(c map[string]any) { c["iss"] = "https://evil.example.com" },
	} {
		idp.Mutate = change
		b := newBrowser(t, h)
		w := b.signIn(idp, "/", claimsOf("boss@example.com", "ops"))
		if w.Code != http.StatusForbidden || b.cookies["__Host-lapilli-session"] != nil {
			t.Errorf("%s: %d", name, w.Code)
		}
		idp.Mutate = nil
	}
	// The provider says no: its code is shown, not the description,
	// which whoever made the link chose.
	b, auth = start()
	au, _ := url.Parse(auth)
	w := b.do("GET", "/auth/callback?state="+url.QueryEscape(au.Query().Get("state"))+"&error=access_denied&error_description=Call+0800-EVIL", nil)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "access_denied") || strings.Contains(w.Body.String(), "Call 0800") {
		t.Errorf("an error: %d %s", w.Code, w.Body)
	}
	// A cookie changed by hand is no session.
	b = newBrowser(t, h)
	b.signIn(idp, "/", claimsOf("ed@example.com"))
	ck := b.cookies["__Host-lapilli-session"]
	body, sig, _ := strings.Cut(ck.Value, ".")
	raw, _ := jsonB64(body)
	raw["u"], raw["g"] = "boss@example.com", []any{"ops"}
	ck.Value = b64JSON(raw) + "." + sig
	if w := b.do("GET", "/api/v1/me", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("a cookie changed by hand: %d %s", w.Code, w.Body)
	}
}

// After signing in, the browser goes only to a page of this server's.
func TestSignInNoOpenRedirect(t *testing.T) {
	s, idp := oidcServer(t, nil)
	h := s.Handler()
	for _, next := range []string{"//evil.example.com/x", "https://evil.example.com/", "/\\evil.example.com", "javascript:alert(1)", "/%2F%2Fevil.example.com", "\t//evil.example.com"} {
		b := newBrowser(t, h)
		w := b.do("GET", "/auth/login?next="+url.QueryEscape(next), nil)
		back := idp.Authorize(t, w.Header().Get("Location"), claimsOf("ed@example.com"))
		w = b.do("GET", back, nil)
		loc := w.Header().Get("Location")
		u, err := url.Parse(loc)
		if w.Code != http.StatusSeeOther || err != nil || u.Host != "" || u.Scheme != "" || !strings.HasPrefix(loc, "/") || strings.HasPrefix(loc, "//") || strings.HasPrefix(loc, "/\\") {
			t.Errorf("next %q: %d %q", next, w.Code, loc)
		}
	}
	// Each step checks it: the login keeps a page of this server's, and the
	// callback goes only to one, whatever the cookie says.
	w := newBrowser(t, h).do("GET", "/auth/login?next="+url.QueryEscape("//evil.example.com/x"), nil)
	lr := httptest.NewRequest("GET", "/auth/callback", nil)
	for _, c := range w.Result().Cookies() {
		lr.AddCookie(c)
	}
	lu, _ := url.Parse(w.Header().Get("Location"))
	if l, ok := s.in.cookies.Take(httptest.NewRecorder(), lr, lu.Query().Get("state")); !ok || l.Next != "/" {
		t.Errorf("the login kept %q", l.Next)
	}
	b := newBrowser(t, h)
	st := signin.NewStart()
	lw := httptest.NewRecorder()
	s.in.cookies.Begin(lw, st, "//evil.example.com/x")
	for _, c := range lw.Result().Cookies() {
		b.cookies[c.Name] = &http.Cookie{Name: c.Name, Value: c.Value}
	}
	auth, err := s.in.oidc.AuthURL(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if w := b.do("GET", idp.Authorize(t, auth, claimsOf("ed@example.com")), nil); w.Header().Get("Location") != "/" {
		t.Errorf("the callback went to %q", w.Header().Get("Location"))
	}
	// Nor is a page not signed in sent anywhere but --external-url.
	r := httptest.NewRequest("GET", "//evil.example.com/x", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if loc := w.Header().Get("Location"); w.Code == http.StatusSeeOther && !strings.HasPrefix(loc, extURL+"/") {
		t.Errorf("sent to %q", loc)
	}
}

// Signing out takes the session away, for good: a copy of its cookie is
// refused, and the person's sessions in other browsers end too, after a
// restart as well; another person's go on, and so does a session made
// after. A page elsewhere can't sign people out.
func TestSignOut(t *testing.T) {
	s, idp := oidcServer(t, nil)
	h := s.Handler()
	// Signed in a minute before signing out, in two browsers.
	s.now = func() time.Time { return now.Add(-time.Minute) }
	b := newBrowser(t, h)
	b.signIn(idp, "/", claimsOf("ed@example.com"))
	copied := *b.cookies["__Host-lapilli-session"]
	laptop := newBrowser(t, h)
	laptop.signIn(idp, "/", claimsOf("ed@example.com"))
	s.now = func() time.Time { return now }
	// From another site: refused, and still signed in.
	for _, site := range []string{"cross-site", "same-site"} {
		if w := b.do("POST", "/auth/logout", map[string]string{"Sec-Fetch-Site": site}); w.Code != http.StatusForbidden {
			t.Errorf("signing out from a %s page: %d", site, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "/auth/logout", nil)
	r.Header.Set("Origin", "https://evil.example.com")
	r.AddCookie(&copied)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("signing out from another origin: %d", w.Code)
	}
	if w := b.do("GET", "/api/v1/me", nil); w.Code != 200 {
		t.Fatalf("signed out by another site: %d", w.Code)
	}
	// Only a POST.
	if w := b.do("GET", "/auth/logout", nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /auth/logout: %d", w.Code)
	}
	w = b.do("POST", "/auth/logout", nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/auth/signed-out" || b.cookies["__Host-lapilli-session"] != nil {
		t.Fatalf("signing out: %d %v", w.Code, w.Header())
	}
	again := newBrowser(t, h)
	again.cookies["__Host-lapilli-session"] = &copied
	if w := again.do("GET", "/api/v1/me", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("a copy of the cookie after signing out: %d", w.Code)
	}
	if w := laptop.do("GET", "/api/v1/me", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("the same person in another browser: %d", w.Code)
	}
	// Signed in again a moment later: a session made after goes on.
	s.now = func() time.Time { return now.Add(time.Second) }
	fresh := newBrowser(t, h)
	fresh.signIn(idp, "/", claimsOf("ed@example.com"))
	if w := fresh.do("GET", "/api/v1/me", nil); w.Code != 200 {
		t.Errorf("signed in again: %d", w.Code)
	}
	s.now = func() time.Time { return now }
	// A session not ended, from before the restart.
	b2 := newBrowser(t, h)
	b2.signIn(idp, "/", claimsOf("boss@example.com", "ops"))
	// Another server on the same ledger: still ended.
	cfg := s.cfg
	cfg.OIDC = &signin.OIDC{Issuer: idp.URL, ClientID: "lapilli", SecretFile: s.cfg.OIDC.SecretFile, Claims: s.cfg.OIDC.Claims}
	s.Close()
	s2, err := New(context.Background(), cfg, &history{}, s.log)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	s2.now = func() time.Time { return now }
	again.h = s2.Handler()
	if w := again.do("GET", "/api/v1/me", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("a copy of the cookie after a restart: %d", w.Code)
	}
	// And a session that wasn't ended goes on, signed with the key kept.
	b2.h = s2.Handler()
	if w := b2.do("GET", "/api/v1/me", nil); w.Code != 200 {
		t.Errorf("a session across a restart: %d", w.Code)
	}
}

// With a sign-in and no roles, everyone signed in may only look: on
// loopback too, which a proxy on the same machine makes anyone's.
func TestSignInNoRoles(t *testing.T) {
	s, idp := oidcServer(t, func(c *Config) { c.Admins, c.Editors, c.Listen = nil, nil, "127.0.0.1:0" })
	if s.open.Load() {
		t.Error("open, with a sign-in, before listening")
	}
	// Bound to loopback, it stays closed.
	s.open.Store(true)
	s.listening(&netTCP127)
	if s.open.Load() {
		t.Error("open, with a sign-in, listening on loopback")
	}
	h := s.Handler()
	b := newBrowser(t, h)
	b.signIn(idp, "/", claimsOf("boss@example.com", "ops"))
	if m := me(t, b.do("GET", "/api/v1/me", nil)); m["role"] != "viewer" {
		t.Errorf("signed in with no roles: %v", m)
	}
	r := httptest.NewRequest("POST", "/api/v1/incidents", strings.NewReader(`{"title": "x", "start": "2026-11-02T06:00:00Z"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	for _, c := range b.cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "signs people in, with no roles") {
		t.Errorf("a write: %d %s", w.Code, w.Body)
	}
}

// An API client or an agent sends an ID token as a bearer token; it may do
// what its role allows, but never approve a change, which is a person's,
// in a browser.
func TestSignInBearer(t *testing.T) {
	s, idp := oidcServer(t, nil)
	h := s.Handler()
	call := func(method, path, tok, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+tok)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	tok := idp.IDToken(claimsOf("boss@example.com", "ops"))
	if m := me(t, call("GET", "/api/v1/me", tok, "")); m["role"] != "admin" || m["user"] != "boss@example.com" {
		t.Errorf("a bearer token: %v", m)
	}
	if w := call("POST", "/mcp", tok, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`); w.Code != 200 {
		t.Errorf("/mcp with a bearer token: %d %s", w.Code, w.Body)
	}
	// A page asked for with a bearer token that doesn't verify: 401, not
	// a sign-in page.
	idp.Mutate = func(c map[string]any) { c["aud"] = "another-client" }
	if w := call("GET", "/", idp.IDToken(claimsOf("boss@example.com", "ops")), ""); w.Code != http.StatusUnauthorized {
		t.Errorf("another client's token: %d", w.Code)
	}
	idp.Mutate = nil
	if w := call("GET", "/api/v1/me", "not-a-token", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("not a token: %d", w.Code)
	}
	// An approval: a person's, in a browser, never a bearer token's,
	// whatever headers it sends.
	s.cfg.Changes, s.cfg.Approvers = true, []string{"group:ops"}
	reached := false
	approve := s.person(func(w http.ResponseWriter, r *http.Request) { reached = true })
	r := httptest.NewRequest("POST", "/proposals/1/approve", nil)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	id := who{Identity: signin.Identity{User: "boss@example.com", Groups: []string{"ops"}}, by: byBearer}
	w := httptest.NewRecorder()
	approve(w, r.WithContext(context.WithValue(r.Context(), whoKey{}, id)))
	if reached || w.Code != http.StatusForbidden {
		t.Errorf("approved with a bearer token: %d", w.Code)
	}
	id.by = bySession
	approve(httptest.NewRecorder(), r.WithContext(context.WithValue(r.Context(), whoKey{}, id)))
	if !reached {
		t.Error("an approver in a browser, refused")
	}
}

// A sign-in in front that signs its header (Google's IAP here): who it
// says, with the role the lists give; nothing without it.
func TestProxyJWT(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "iap", Algorithm: "ES256", Use: "sig"}}})
	}))
	defer keys.Close()
	aud := "/projects/123456789012/global/backendServices/42"
	s, _ := server(t, func(c *Config) {
		c.Proxy = &signin.Proxy{Kind: signin.GoogleIAP, Audience: aud, KeysURL: keys.URL, Claims: signin.Claims{UserClaim: "email"}}
		c.Admins, c.Viewers = []string{"user:boss@example.com"}, []string{"*@example.com"}
	})
	h := s.Handler()
	ask := func(path string, claims map[string]any) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if claims != nil {
			c := map[string]any{"iss": "https://cloud.google.com/iap", "aud": aud, "iat": now.Unix(), "exp": now.Add(10 * time.Minute).Unix()}
			for k, v := range claims {
				c[k] = v
			}
			r.Header.Set("X-Goog-Iap-Jwt-Assertion", idptest.Sign(key, "iap", jose.ES256, c))
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if m := me(t, ask("/api/v1/me", map[string]any{"email": "boss@example.com"})); m["role"] != "admin" {
		t.Errorf("IAP's admin: %v", m)
	}
	if m := me(t, ask("/api/v1/me", map[string]any{"email": "pat@example.com"})); m["role"] != "viewer" || m["user"] != "pat@example.com" {
		t.Errorf("IAP's viewer: %v", m)
	}
	if w := ask("/", map[string]any{"email": "boss@example.com"}); w.Code != 200 || !strings.Contains(strings.Join(w.Header().Values("Vary"), ","), "X-Goog-Iap-Jwt-Assertion") {
		t.Errorf("a page: %d %v", w.Code, w.Header())
	}
	for name, w := range map[string]*httptest.ResponseRecorder{
		"no header":        ask("/", nil),
		"another backend":  ask("/api/v1/me", map[string]any{"email": "boss@example.com", "aud": "/projects/1/global/backendServices/2"}),
		"a reserved name":  ask("/api/v1/me", map[string]any{"email": "incidents-token"}),
		"a hidden char":    ask("/api/v1/me", map[string]any{"email": "boss@example.com​"}),
		"no sign-in pages": ask("/auth/login", map[string]any{"email": "boss@example.com"}),
	} {
		if w.Code != http.StatusUnauthorized && !(name == "no sign-in pages" && w.Code == http.StatusNotFound) {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	if w := ask("/healthz", nil); w.Code != 200 {
		t.Errorf("/healthz: %d", w.Code)
	}
}

// The sign-out button and the page after it, in every language: each
// text translated, the structure accessible.
func TestSignInPages(t *testing.T) {
	s, idp := oidcServer(t, nil)
	h := s.Handler()
	b := newBrowser(t, h)
	b.signIn(idp, "/", claimsOf("boss@example.com", "ops"))
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	for _, n := range names {
		for _, p := range []string{"/", "/rules", "/auth/signed-out"} {
			w := b.do("GET", p, map[string]string{"Accept-Language": n.Code})
			if w.Code != 200 {
				t.Errorf("%s %s: %d", n.Code, p, w.Code)
				continue
			}
			doc, err := html.Parse(strings.NewReader(w.Body.String()))
			if err != nil {
				t.Fatal(err)
			}
			for _, pr := range a11yProblems(doc, n.Code) {
				t.Errorf("%s %s: %s", n.Code, p, pr)
			}
			if p != "/auth/signed-out" && !strings.Contains(w.Body.String(), `action="/auth/logout"`) {
				t.Errorf("%s %s: no sign-out", n.Code, p)
			}
		}
		// The refusals, as pages.
		for _, p := range []string{"/auth/callback?state=x&code=y"} {
			b.do("GET", p, map[string]string{"Accept-Language": n.Code})
		}
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Accept-Language", n.Code)
		for _, err := range []error{errBadState, errSignInDown, errSignInBroken, errNoName, errSignInFails, errTooManyGrp, errNoOne, s.forbidden(editor),
			forbiddenSignIn{bad("the identity provider didn't sign you in (%s)", "access_denied")}} {
			s.oops(httptest.NewRecorder(), r, err)
			if msg := message(n.Code, err); n.Code != "en" && msg == err.Error() {
				t.Errorf("%s: %q isn't translated", n.Code, err)
			}
		}
	}
	missing.Lock()
	for k := range missing.m {
		lang, msg, _ := strings.Cut(k, "\x00")
		t.Errorf("%s has no translation for %q", lang, msg)
	}
	missing.Unlock()
}

// An identity provider that can't be reached is asked again at most every
// ten seconds, however many browsers come to sign in.
func TestSignInProviderDown(t *testing.T) {
	asked := 0
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer down.Close()
	s, _ := server(t, func(c *Config) {
		c.ExternalURL = extURL
		c.OIDC = &signin.OIDC{Issuer: down.URL, ClientID: "lapilli", Claims: signin.Claims{UserClaim: "email", Verified: true}}
		c.Viewers = []string{"*@example.com"}
	})
	h := s.Handler()
	for range 20 {
		if code, _, _ := get(t, h, "/auth/login"); code != http.StatusServiceUnavailable {
			t.Fatalf("%d", code)
		}
	}
	if asked != 1 {
		t.Errorf("asked %d times", asked)
	}
}

var netTCP127 = net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8080}

var b64 = base64.RawURLEncoding

func jsonB64(s string) (map[string]any, error) {
	b, err := b64.DecodeString(s)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(b, &m)
}

func b64JSON(m map[string]any) string {
	b, _ := json.Marshal(m)
	return b64.EncodeToString(b)
}
