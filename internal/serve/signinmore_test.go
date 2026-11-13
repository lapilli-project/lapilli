package serve

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/ledger/ledgertest"
	"lapilli.dev/internal/signin"
	"lapilli.dev/internal/signin/idptest"
)

// An identity provider signs in whoever it knows, a stranger with a public
// account among them: only those a role names come in, to the pages and
// the API alike, and '*' gives a role only once --viewers says who comes
// in at all.
func TestSignInOnlyThoseNamed(t *testing.T) {
	s, idp := oidcServer(t, nil)
	h := s.Handler()
	b := newBrowser(t, h)
	w := b.signIn(idp, "/rules", claimsOf("stranger@public.example"))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("signing in: %d", w.Code)
	}
	for _, p := range []string{"/", "/rules", "/incidents", "/api/v1/incidents", "/api/v1/rules", "/api/v1/me", "/settings.yaml", "/metrics"} {
		w := b.do("GET", p, map[string]string{"Accept-Language": "ko"})
		if w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "Checkout down") {
			t.Errorf("a stranger reads %s: %d", p, w.Code)
		}
	}
	page := b.do("GET", "/", map[string]string{"Accept-Language": "ko"})
	if !strings.Contains(page.Body.String(), "stranger@public.example(으)로 로그인했지만") || !strings.Contains(page.Body.String(), `action="/auth/logout"`) {
		t.Errorf("the refusal, in Korean, with a way to sign out: %s", page.Body)
	}
	// A group lets in, and so does a role given in the UI.
	g := newBrowser(t, h)
	g.signIn(idp, "/", claimsOf("someone@public.example", "ops"))
	if m := me(t, g.do("GET", "/api/v1/me", nil)); m["role"] != "admin" {
		t.Errorf("an admin by a group, from another domain: %v", m)
	}
	// '*' refused until --viewers is restricted, then everyone it lets in.
	for _, c := range []Config{
		{OIDC: &signin.OIDC{}, Admins: []string{"*"}},
		{OIDC: &signin.OIDC{}, Admins: []string{"*"}, Viewers: []string{"*"}},
		{Proxy: &signin.Proxy{}, Editors: []string{"*"}},
		{OIDC: &signin.OIDC{}, Approvers: []string{"*"}, Viewers: []string{"*@example.com"}},
		{OIDC: &signin.OIDC{}},
	} {
		if err := CheckRoles(c); err == nil && len(c.Approvers) == 0 {
			t.Errorf("%+v: taken", c)
		}
	}
	if err := CheckRoles(Config{UserHeader: "X-Forwarded-Email", Admins: []string{"*"}}); err != nil {
		t.Errorf("--user-header with '*', as before: %v", err)
	}
	if _, err := New(context.Background(), Config{OIDC: &signin.OIDC{}, Admins: []string{"*"}}, &history{}, s.log); err == nil {
		t.Error("New took '*' with no --viewers")
	}
	s2, idp2 := oidcServer(t, func(c *Config) { c.Admins, c.Editors = []string{"*"}, nil })
	in := newBrowser(t, s2.Handler())
	in.signIn(idp2, "/", claimsOf("ana@example.com"))
	if m := me(t, in.do("GET", "/api/v1/me", nil)); m["role"] != "admin" {
		t.Errorf("'*' with --viewers *@example.com: %v", m)
	}
	out := newBrowser(t, s2.Handler())
	out.signIn(idp2, "/", claimsOf("ana@public.example"))
	if w := out.do("GET", "/api/v1/me", nil); w.Code != http.StatusForbidden {
		t.Errorf("'*' let in a stranger: %d", w.Code)
	}
}

// --viewers with a proxy's plain header: only those named come in.
func TestViewersBehindAProxy(t *testing.T) {
	s, _ := server(t, func(c *Config) {
		c.UserHeader, c.Viewers, c.Editors = "X-Forwarded-Email", []string{"*@example.com"}, []string{"ed@other.example"}
	})
	h := s.Handler()
	for who, want := range map[string]int{"ana@example.com": 200, "ANA@EXAMPLE.COM": 200, "ed@other.example": 200, "eve@other.example": 403,
		"eve@example.com.other.example": 403, "": 403} {
		r := httptest.NewRequest("GET", "/api/v1/me", nil)
		if who != "" {
			r.Header.Set("X-Forwarded-Email", who)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("%q: %d, want %d", who, w.Code, want)
		}
	}
}

// Google's hd: with --oidc-hosted-domain, an account of another domain, or
// of none (gmail.com), is refused; and Google is told to offer only that
// domain's.
func TestSignInHostedDomain(t *testing.T) {
	s, idp := oidcServer(t, func(c *Config) { c.OIDC.HostedDomains = []string{"example.com"} })
	h := s.Handler()
	w := newBrowser(t, h).do("GET", "/auth/login", nil)
	if u, _ := url.Parse(w.Header().Get("Location")); u.Query().Get("hd") != "example.com" {
		t.Errorf("no hd in %s", u)
	}
	for hd, want := range map[string]int{"example.com": http.StatusSeeOther, "": http.StatusForbidden, "other.example": http.StatusForbidden} {
		c := claimsOf("ana@example.com")
		if hd != "" {
			c["hd"] = hd
		}
		if got := newBrowser(t, h).signIn(idp, "/", c).Code; got != want {
			t.Errorf("hd %q: %d, want %d", hd, got, want)
		}
	}
}

// A bearer token issued before its person signed out is refused, as their
// sessions are; one that doesn't verify says why, for a client to get
// another (RFC 6750), and the server says where tokens come from (RFC
// 9728). --oidc-bearer=false takes none.
func TestBearerRefusals(t *testing.T) {
	s, idp := oidcServer(t, nil)
	h := s.Handler()
	call := func(path, tok string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	s.now = func() time.Time { return now.Add(-time.Minute) }
	b := newBrowser(t, h)
	b.signIn(idp, "/", claimsOf("ed@example.com"))
	before := idp.IDToken(claimsOf("ed@example.com")) // issued at now, a moment before signing out below
	s.now = func() time.Time { return now.Add(time.Second) }
	if w := b.do("POST", "/auth/logout", nil); w.Code != http.StatusSeeOther {
		t.Fatalf("signing out: %d", w.Code)
	}
	w := call("/api/v1/me", before)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Header().Get("WWW-Authenticate"), `error="invalid_token"`) ||
		!strings.Contains(w.Header().Get("WWW-Authenticate"), "signed out") {
		t.Errorf("a token from before signing out: %d %v", w.Code, w.Header())
	}
	idp.Now = func() time.Time { return now.Add(2 * time.Second) }
	if w := call("/api/v1/me", idp.IDToken(claimsOf("ed@example.com"))); w.Code != 200 {
		t.Errorf("a token issued after: %d %s", w.Code, w.Body)
	}
	idp.Now = func() time.Time { return now }
	s.now = func() time.Time { return now }
	idp.Mutate = func(c map[string]any) { c["exp"] = now.Add(-time.Minute).Unix() }
	expired := idp.IDToken(claimsOf("boss@example.com", "ops"))
	idp.Mutate = nil
	for _, path := range []string{"/api/v1/me", "/mcp"} {
		w := call(path, expired)
		ch := w.Header().Get("WWW-Authenticate")
		if w.Code != http.StatusUnauthorized || !strings.Contains(ch, `error="invalid_token"`) || !strings.Contains(ch, `error_description="the token has expired"`) ||
			!strings.Contains(ch, `resource_metadata="`+extURL+`/.well-known/oauth-protected-resource"`) {
			t.Errorf("%s, expired: %d %q", path, w.Code, ch)
		}
	}
	if w := call("/api/v1/me", ""); strings.Contains(w.Header().Get("WWW-Authenticate"), "invalid_token") {
		t.Errorf("no token is no invalid one: %q", w.Header().Get("WWW-Authenticate"))
	}
	w = call("/.well-known/oauth-protected-resource", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"authorization_servers":["`+idp.URL+`"]`) || !strings.Contains(w.Body.String(), `"resource":"`+extURL+`"`) {
		t.Errorf("the resource's metadata: %d %s", w.Code, w.Body)
	}
	// A token's words never reach the log: only the reason.
	var logs bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logs, nil))
	s.in.warned.Store(false)
	idp.Mutate = func(c map[string]any) { c["aud"] = "another-client"; c["email"] = "secret-marker@example.com" }
	refused := idp.IDToken(claimsOf("x"))
	call("/api/v1/me", refused)
	idp.Mutate = nil
	sig := refused[strings.LastIndexByte(refused, '.')+1:]
	if !strings.Contains(logs.String(), "didn't verify") || strings.Contains(logs.String(), "secret-marker") || strings.Contains(logs.String(), "another-client") ||
		strings.Contains(logs.String(), sig) {
		t.Errorf("the log: %s", logs.String())
	}
	off, idp3 := oidcServer(t, func(c *Config) { c.OIDC.NoBearer = true })
	r := httptest.NewRequest("GET", "/api/v1/me", nil)
	r.Header.Set("Authorization", "Bearer "+idp3.IDToken(claimsOf("boss@example.com", "ops")))
	w = httptest.NewRecorder()
	off.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("--oidc-bearer=false took a bearer token: %d", w.Code)
	}
	if code, _, _ := get(t, off.Handler(), "/.well-known/oauth-protected-resource"); code != http.StatusNotFound {
		t.Errorf("the metadata, with no bearer tokens taken: %d", code)
	}
}

// A JWT a proxy puts in Authorization (oauth2-proxy's) is a bearer token:
// whoever holds one sends it as easily, and doesn't approve a change.
func TestProxyJWTInAuthorizationIsBearer(t *testing.T) {
	idp := idptest.New(t, "oauth2-proxy", "", func() time.Time { return now })
	s, _ := server(t, func(c *Config) {
		c.Proxy = &signin.Proxy{Kind: signin.JWKS, Header: "Authorization", JWKS: idp.URL + "/keys", Issuer: idp.URL,
			Audience: "oauth2-proxy", Claims: signin.Claims{UserClaim: "email"}}
		c.Admins, c.Changes, c.Approvers = []string{"ana@example.com"}, true, []string{"ana@example.com"}
	})
	reached := false
	approve := s.person(func(w http.ResponseWriter, r *http.Request) { reached = true })
	h := s.signedIn(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { approve(w, r) }))
	r := httptest.NewRequest("POST", "/proposals/1/approve", nil)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Authorization", "Bearer "+idp.IDToken(map[string]any{"email": "ana@example.com"}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if reached || w.Code != http.StatusForbidden {
		t.Errorf("approved with a proxy's bearer token: %d", w.Code)
	}
}

// Two tabs signing in at once each come back to their own sign-in.
func TestSignInTwoTabs(t *testing.T) {
	s, idp := oidcServer(t, nil)
	h := s.Handler()
	b := newBrowser(t, h)
	wa := b.do("GET", b.do("GET", "/rules", nil).Header().Get("Location"), nil)
	wb := b.do("GET", b.do("GET", "/incidents", nil).Header().Get("Location"), nil)
	backA := idp.Authorize(t, wa.Header().Get("Location"), claimsOf("boss@example.com", "ops"))
	backB := idp.Authorize(t, wb.Header().Get("Location"), claimsOf("boss@example.com", "ops"))
	if w := b.do("GET", backA, nil); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/rules" {
		t.Errorf("tab A: %d %v", w.Code, w.Header().Get("Location"))
	}
	if w := b.do("GET", backB, nil); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/incidents" {
		t.Errorf("tab B: %d %v", w.Code, w.Header().Get("Location"))
	}
}

// Signing out at one process that shares the ledger signs out at the
// other too, and neither writes over the other's.
func TestSignOutTwoProcesses(t *testing.T) {
	s, idp := oidcServer(t, nil)
	s.now = func() time.Time { return now.Add(-time.Minute) }
	ed := newBrowser(t, s.Handler())
	ed.signIn(idp, "/", claimsOf("ed@example.com"))
	boss := newBrowser(t, s.Handler())
	boss.signIn(idp, "/", claimsOf("boss@example.com", "ops"))
	edCopy, bossCopy := *ed.cookies["__Host-lapilli-session"], *boss.cookies["__Host-lapilli-session"]
	s.now = func() time.Time { return now }
	cfg := s.cfg
	cfg.OIDC = &signin.OIDC{Issuer: idp.URL, ClientID: "lapilli", SecretFile: s.cfg.OIDC.SecretFile, Claims: s.cfg.OIDC.Claims}
	s2, err := New(context.Background(), cfg, &history{}, s.log)
	if ledgertest.On() {
		// A Postgres ledger takes one process: the second is refused.
		var locked *ledger.LockedError
		if !errors.As(err, &locked) {
			t.Fatalf("a second process on a Postgres ledger: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	s2.now = func() time.Time { return now }
	// ed signs out at the first, boss at the second.
	if w := ed.do("POST", "/auth/logout", nil); w.Code != http.StatusSeeOther {
		t.Fatalf("ed: %d", w.Code)
	}
	boss.h = s2.Handler()
	if w := boss.do("POST", "/auth/logout", nil); w.Code != http.StatusSeeOther {
		t.Fatalf("boss: %d", w.Code)
	}
	for _, h := range []http.Handler{s.Handler(), s2.Handler()} {
		for name, ck := range map[string]http.Cookie{"ed": edCopy, "boss": bossCopy} {
			b := newBrowser(t, h)
			b.cookies[ck.Name] = &ck
			if w := b.do("GET", "/api/v1/me", nil); w.Code != http.StatusUnauthorized {
				t.Errorf("%s's copy: %d", name, w.Code)
			}
		}
	}
}

// What the identity provider says, read for what lapilli needs: a name
// with spacing that looks like nothing is refused; Microsoft Entra ID's
// groups overage is said in the log; a refusal is said to people in their
// language, and what went wrong only in the log.
func TestSignInClaimsRead(t *testing.T) {
	s, idp := oidcServer(t, nil)
	var logs bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logs, nil))
	h := s.Handler()
	for _, n := range []string{"boss@example.com ", "boss@example.com ", "boss@example.com　", "boss@​example.com"} {
		if w := newBrowser(t, h).signIn(idp, "/", claimsOf(n, "ops")); w.Code != http.StatusForbidden {
			t.Errorf("%q: %d", n, w.Code)
		}
	}
	b := newBrowser(t, h)
	b.signIn(idp, "/", map[string]any{"email": "ana@example.com", "email_verified": true,
		"_claim_names": map[string]any{"groups": "src1"}, "_claim_sources": map[string]any{"src1": map[string]any{"endpoint": "https://graph.example/x"}}})
	if !strings.Contains(logs.String(), "overage") {
		t.Errorf("the overage isn't said: %s", logs.String())
	}
	logs.Reset()
	b2 := newBrowser(t, h)
	wl := b2.do("GET", b2.do("GET", "/", nil).Header().Get("Location"), nil)
	back := idp.Authorize(t, wl.Header().Get("Location"), map[string]any{"email": "ana@example.com"})
	w := b2.do("GET", back, map[string]string{"Accept-Language": "ko"})
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "ID 제공자의 응답이") || strings.Contains(w.Body.String(), "email_verified") {
		t.Errorf("the refusal, in Korean: %d", w.Code)
	}
	if !strings.Contains(logs.String(), "email_verified") {
		t.Errorf("the reason isn't in the log: %s", logs.String())
	}
}

// An issuer given as the provider doesn't say it is a setting to put
// right, said at start as an ERROR, not an outage.
func TestSignInMisconfigured(t *testing.T) {
	idp := idptest.New(t, "lapilli", "", func() time.Time { return now })
	var logs bytes.Buffer
	s, _ := server(t, func(c *Config) {
		c.ExternalURL, c.Viewers = extURL, []string{"*@example.com"}
		c.OIDC = &signin.OIDC{Issuer: idp.URL + "/", ClientID: "lapilli", Claims: signin.Claims{UserClaim: "email", Verified: true}}
	})
	s.log = slog.New(slog.NewTextHandler(&logs, nil))
	s.discover(context.Background())
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "its issuer is") {
		t.Errorf("at start: %s", logs.String())
	}
	code, body, _ := get(t, s.Handler(), "/auth/login")
	if code == http.StatusServiceUnavailable || !strings.Contains(body, "isn&#39;t set up right") {
		t.Errorf("/auth/login: %d", code)
	}
}

// A session key is never kept in the ledger: without --session-key-file,
// one is made for the process, and sessions end when it restarts.
func TestSessionKeyNotInLedger(t *testing.T) {
	s, idp := oidcServer(t, func(c *Config) { c.SessionKey = "" })
	b := newBrowser(t, s.Handler())
	b.signIn(idp, "/", claimsOf("ed@example.com"))
	if ledgerHolds(t, s, "session-key") {
		t.Error("a session key in the ledger")
	}
	cfg := s.cfg
	cfg.OIDC = &signin.OIDC{Issuer: idp.URL, ClientID: "lapilli", SecretFile: s.cfg.OIDC.SecretFile, Claims: s.cfg.OIDC.Claims}
	s.Close()
	s2, err := New(context.Background(), cfg, &history{}, s.log)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	s2.now = func() time.Time { return now }
	b.h = s2.Handler()
	if w := b.do("GET", "/api/v1/me", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("a session across a restart, without a key file: %d", w.Code)
	}
}

// /readyz is ready once the ledger has a run recorded, not only once this
// process finished one: after a restart a Prometheus that hangs doesn't
// keep the UI out of a load balancer.
func TestReadyOnceARunIsRecorded(t *testing.T) {
	s, _ := server(t, nil)
	h := s.MetricsHandler()
	if code, _, _ := get(t, h, "/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("a new ledger: %d", code)
	}
	if err := s.l.Record(context.Background(), ledger.Found{From: now.Add(-time.Hour), To: now,
		Run: ledger.Run{Kind: "hourly", Started: now, Finished: now}}, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := get(t, h, "/readyz"); code != 200 {
		t.Errorf("a run recorded, this process's first not yet done: %d", code)
	}
}

// A proxy's plain header is held to what a claim is: a name with spacing
// that looks like nothing is no one's.
func TestPlainNamesFromHeaders(t *testing.T) {
	s, _ := server(t, func(c *Config) { c.UserHeader, c.Admins = "X-Forwarded-Email", []string{"boss@example.com"} })
	h := s.Handler()
	for n, want := range map[string]string{"boss@example.com": "admin", "boss@example.com ": "viewer", "boss@example.com　": "viewer", "boss@⁠example.com": "viewer"} {
		r := httptest.NewRequest("GET", "/api/v1/me", nil)
		r.Header.Set("X-Forwarded-Email", n)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if m := me(t, w); m["role"] != want {
			t.Errorf("%q: %v", n, m)
		}
	}
}
