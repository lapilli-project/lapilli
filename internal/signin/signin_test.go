package signin

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"lapilli.dev/internal/signin/idptest"
)

var t0 = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)

// clock is a time a test moves on.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newIdP(t *testing.T, c *clock) (*idptest.IdP, *OIDC) {
	t.Helper()
	idp := idptest.New(t, "lapilli", "", c.now)
	return idp, &OIDC{Issuer: idp.URL, ClientID: "lapilli", Redirect: "http://localhost:8080/auth/callback",
		Scopes: WithOpenID(nil), Claims: Claims{UserClaim: "email", GroupsClaim: "groups", Verified: true}, Client: Client(nil), Now: c.now}
}

// The whole flow against a provider: the code exchanged with the PKCE
// verifier, the ID token checked, the nonce this sign-in's.
func TestOIDCFinish(t *testing.T) {
	c := &clock{t0}
	idp, o := newIdP(t, c)
	ctx := context.Background()
	st := NewStart()
	auth, err := o.AuthURL(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	back := idp.Authorize(t, auth, map[string]any{"email": "ana@example.com", "email_verified": true, "groups": []any{"sre", " ", strings.Repeat("g", 201)}})
	code := query(t, back, "code")
	id, err := o.Finish(ctx, st, code)
	if err != nil {
		t.Fatal(err)
	}
	if id.User != "ana@example.com" || len(id.Groups) != 1 || id.Groups[0] != "sre" {
		t.Errorf("got %+v", id)
	}
	// The code is used: the provider refuses it a second time.
	if _, err := o.Finish(ctx, st, code); err == nil {
		t.Error("a code taken twice")
	}
	// Another sign-in's nonce, verifier, or a code it didn't give.
	other := NewStart()
	back = idp.Authorize(t, auth, map[string]any{"email": "ana@example.com", "email_verified": true})
	if _, err := o.Finish(ctx, Start{State: st.State, Nonce: other.Nonce, Verifier: st.Verifier}, query(t, back, "code")); err == nil || !strings.Contains(err.Error(), "nonce") {
		t.Errorf("another nonce: %v", err)
	}
	back = idp.Authorize(t, auth, map[string]any{"email": "ana@example.com", "email_verified": true})
	if _, err := o.Finish(ctx, Start{State: st.State, Nonce: st.Nonce, Verifier: other.Verifier}, query(t, back, "code")); err == nil {
		t.Error("another verifier")
	}
	if _, err := o.Finish(ctx, st, "made-up"); err == nil {
		t.Error("a code made up")
	}
}

// Each claim that makes a token someone else's, or no one's, is refused.
func TestOIDCRefusals(t *testing.T) {
	c := &clock{t0}
	idp, o := newIdP(t, c)
	ctx := context.Background()
	good := map[string]any{"email": "ana@example.com", "email_verified": true}
	if _, err := o.Bearer(ctx, idp.IDToken(good)); err != nil {
		t.Fatalf("a good token: %v", err)
	}
	for name, change := range map[string]func(map[string]any){
		"another audience":     func(c map[string]any) { c["aud"] = "other" },
		"another issuer":       func(c map[string]any) { c["iss"] = "https://evil.example" },
		"expired":              func(c map[string]any) { c["exp"] = t0.Add(-time.Second).Unix() },
		"not yet":              func(c map[string]any) { c["nbf"] = t0.Add(time.Hour).Unix() },
		"issued in the future": func(c map[string]any) { c["iat"] = t0.Add(time.Hour).Unix() },
		"given to another":     func(c map[string]any) { c["aud"] = []any{"lapilli", "other"}; c["azp"] = "other" },
		"no azp, two audiences": func(c map[string]any) {
			c["aud"] = []any{"lapilli", "other"}
		},
		"email not verified":    func(c map[string]any) { c["email_verified"] = false },
		"email unsaid":          func(c map[string]any) { delete(c, "email_verified") },
		"no email":              func(c map[string]any) { delete(c, "email") },
		"email not a string":    func(c map[string]any) { c["email"] = []any{"ana@example.com"} },
		"no time it was issued": func(c map[string]any) { delete(c, "iat") },
	} {
		claims := map[string]any{}
		for k, v := range good {
			claims[k] = v
		}
		idp.Mutate = change
		if id, err := o.Bearer(ctx, idp.IDToken(claims)); err == nil {
			t.Errorf("%s: taken, as %+v", name, id)
		}
		idp.Mutate = nil
	}
	// Signed with another key, or not at all, or with the key as an HMAC
	// secret.
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if _, err := o.Bearer(ctx, idptest.Sign(other, idp.KID, jose.ES256, good)); err == nil {
		t.Error("another key")
	}
	pub, _ := x509.MarshalPKIXPublicKey(&idp.Key.PublicKey)
	if _, err := o.Bearer(ctx, idptest.Sign(pub, idp.KID, jose.HS256, good)); err == nil {
		t.Error("HS256 with the public key as its secret")
	}
	parts := strings.Split(idp.IDToken(good), ".")
	none := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + parts[1] + "."
	if _, err := o.Bearer(ctx, none); err == nil {
		t.Error("alg none")
	}
	// A token whose claims were changed after signing.
	body, _ := json.Marshal(map[string]any{"iss": idp.URL, "aud": "lapilli", "exp": t0.Add(time.Hour).Unix(), "email": "boss@example.com", "email_verified": true})
	if _, err := o.Bearer(ctx, parts[0]+"."+base64.RawURLEncoding.EncodeToString(body)+"."+parts[2]); err == nil {
		t.Error("claims changed after signing")
	}
	// With another claim for the name, email_verified isn't asked.
	o.Claims.UserClaim = "preferred_username"
	if id, err := o.Bearer(ctx, idp.IDToken(map[string]any{"preferred_username": "ana"})); err != nil || id.User != "ana" {
		t.Errorf("preferred_username: %+v %v", id, err)
	}
}

// A token naming a key not held fetches the keys again, at most once a
// minute; the keys held are used meanwhile.
func TestKeysFetchedSparingly(t *testing.T) {
	c := &clock{t0}
	idp, o := newIdP(t, c)
	ctx := context.Background()
	good := map[string]any{"email": "ana@example.com", "email_verified": true}
	if _, err := o.Bearer(ctx, idp.IDToken(good)); err != nil {
		t.Fatal(err)
	}
	if n := idp.KeyFetches.Load(); n != 1 {
		t.Fatalf("%d fetches for the first token", n)
	}
	other := mustRSA(t)
	for i := range 50 {
		if _, err := o.Bearer(ctx, idptest.Sign(other, "made-up-"+string(rune('a'+i%26)), jose.RS256, good)); err == nil {
			t.Fatal("a key made up")
		}
	}
	// Fetched a moment ago: not again for a minute.
	if n := idp.KeyFetches.Load(); n != 1 {
		t.Errorf("%d fetches for 50 tokens with keys made up, want 1", n)
	}
	if _, err := o.Bearer(ctx, idp.IDToken(good)); err != nil {
		t.Errorf("the key held, meanwhile: %v", err)
	}
	c.t = c.t.Add(keysRetry)
	if _, err := o.Bearer(ctx, idptest.Sign(other, "made-up-again", jose.RS256, good)); err == nil {
		t.Fatal("a key made up")
	}
	if n := idp.KeyFetches.Load(); n != 2 {
		t.Errorf("%d fetches a minute later, want 2", n)
	}
	for range 10 {
		o.Bearer(ctx, idptest.Sign(other, "made-up-again", jose.RS256, good))
	}
	if n := idp.KeyFetches.Load(); n != 2 {
		t.Errorf("%d fetches, want 2 still", n)
	}
}

// A JWKS with a shared secret, or a key for encryption, gives no key.
func TestKeysPublicOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"keys":[{"kty":"oct","kid":"s","k":"c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0c2U"}]}`))
	}))
	defer srv.Close()
	k := NewKeys(srv.URL, Client(nil), nil, time.Now)
	tok := idptest.Sign([]byte("secretsecretsecretsecretsecretse"), "s", jose.HS256, map[string]any{"sub": "x"})
	if _, err := k.VerifySignature(context.Background(), tok); err == nil {
		t.Error("an HMAC key taken")
	}
	if keys, err := k.fetch(context.Background()); err == nil {
		t.Errorf("an HMAC key fetched: %v", keys)
	}
	// A public key for encryption, not signatures.
	rsaKey := mustRSA(t).(*rsa.PrivateKey)
	enc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &rsaKey.PublicKey, KeyID: "e", Algorithm: "RSA-OAEP", Use: "enc"}}})
	}))
	defer enc.Close()
	if keys, err := NewKeys(enc.URL, Client(nil), nil, time.Now).fetch(context.Background()); err == nil {
		t.Errorf("a key for encryption fetched: %v", keys)
	}
	// Redirects aren't followed, to keys that would verify or any, and a
	// body too big isn't read.
	idp := idptest.New(t, "x", "", time.Now)
	moved := httptest.NewServer(http.RedirectHandler(idp.URL+"/keys", http.StatusFound))
	defer moved.Close()
	if _, err := NewKeys(moved.URL, Client(nil), nil, time.Now).fetch(context.Background()); err == nil {
		t.Error("a redirect followed")
	}
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"keys":[` + strings.Repeat(" ", keysBody) + `]}`))
	}))
	defer big.Close()
	if _, err := NewKeys(big.URL, Client(nil), nil, time.Now).fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("a body too big: %v", err)
	}
}

func TestSafeURL(t *testing.T) {
	for u, want := range map[string]bool{
		"https://idp.example.com": true, "http://127.0.0.1:5556/dex": true, "http://localhost/x": true,
		"http://dex.dex.svc.cluster.local:5556/dex": true, "http://dex:5556": true, "http://[::1]:5556": true,
		"http://idp.example.com": false, "http://10.0.0.1": false, "http://[fe80::1]": false, "ftp://idp.example.com": false,
		"https://user:pw@idp.example.com": false, "": false,
	} {
		if SafeURL(u) != want {
			t.Errorf("%q: %v, want %v", u, !want, want)
		}
	}
}

func cookies(t *testing.T, secure bool, c *clock) *Cookies {
	t.Helper()
	k, err := NewCookies([]byte(strings.Repeat("k", 32)), secure, time.Hour, c.now)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// A session cookie holds only what lapilli signed, for as long as
// sessions last.
func TestSessionCookie(t *testing.T) {
	c := &clock{t0}
	k := cookies(t, true, c)
	if _, err := NewCookies([]byte("short"), true, time.Hour, c.now); err == nil {
		t.Error("a short key")
	}
	w := httptest.NewRecorder()
	s, err := k.Issue(w, Identity{User: "ana@example.com", Groups: []string{"sre"}})
	if err != nil {
		t.Fatal(err)
	}
	set := w.Result().Cookies()[0]
	if set.Name != "__Host-lapilli-session" || !set.HttpOnly || !set.Secure || set.SameSite != http.SameSiteLaxMode || set.Path != "/" || set.MaxAge != 3600 {
		t.Errorf("the cookie: %+v", set)
	}
	read := func(v string) (Session, bool) {
		r := httptest.NewRequest("GET", "/", nil)
		r.AddCookie(&http.Cookie{Name: set.Name, Value: v})
		return k.Read(r)
	}
	if got, ok := read(set.Value); !ok || got.User != "ana@example.com" || got.ID != s.ID || got.Groups[0] != "sre" {
		t.Fatalf("read back: %+v %v", got, ok)
	}
	// Tampered with: another name, the signature changed, unsigned.
	body, sig, _ := strings.Cut(set.Value, ".")
	forged, _ := json.Marshal(Session{ID: "x", User: "boss@example.com", At: t0.Unix(), Until: t0.Add(time.Hour).Unix()})
	for name, v := range map[string]string{
		"another name":     base64.RawURLEncoding.EncodeToString(forged) + "." + sig,
		"another sig":      body + "." + base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		"no sig":           body,
		"empty":            "",
		"signed elsewhere": sealWith(t, strings.Repeat("x", 32), set.Name, forged),
	} {
		if got, ok := read(v); ok {
			t.Errorf("%s: read as %+v", name, got)
		}
	}
	// A sign-in's cookie, signed by lapilli too, isn't a session.
	lw := httptest.NewRecorder()
	k.Begin(lw, NewStart(), "/")
	if got, ok := read(lw.Result().Cookies()[0].Value); ok {
		t.Errorf("a sign-in's cookie read as a session: %+v", got)
	}
	// Expired, and older than sessions last now.
	c.t = t0.Add(time.Hour)
	if _, ok := read(set.Value); ok {
		t.Error("expired")
	}
	k.MaxAge = 2 * time.Hour
	if _, ok := read(set.Value); ok {
		t.Error("expired, though sessions last longer now")
	}
	k.MaxAge = time.Hour
	c.t = t0.Add(30 * time.Minute)
	k.MaxAge = 20 * time.Minute
	if _, ok := read(set.Value); ok {
		t.Error("older than sessions last now")
	}
	// Over http, no __Host- prefix and no Secure.
	plain := cookies(t, false, c)
	w = httptest.NewRecorder()
	plain.Issue(w, Identity{User: "ana"})
	if ck := w.Result().Cookies()[0]; ck.Name != "lapilli-session" || ck.Secure {
		t.Errorf("over http: %+v", ck)
	}
}

func sealWith(t *testing.T, key, name string, body []byte) string {
	k, _ := NewCookies([]byte(key), true, time.Hour, time.Now)
	b := base64.RawURLEncoding.EncodeToString(body)
	return b + "." + base64.RawURLEncoding.EncodeToString(mac(k.session, name, b))
}

// A sign-in under way is taken once, and not after ten minutes.
func TestLoginCookie(t *testing.T) {
	c := &clock{t0}
	k := cookies(t, false, c)
	w := httptest.NewRecorder()
	st := NewStart()
	k.Begin(w, st, "/rules")
	ck := w.Result().Cookies()[0]
	take := func() (Login, bool, *httptest.ResponseRecorder) {
		r := httptest.NewRequest("GET", "/auth/callback", nil)
		r.AddCookie(ck)
		w := httptest.NewRecorder()
		l, ok := k.Take(w, r, st.State)
		return l, ok, w
	}
	l, ok, tw := take()
	if !ok || l.State != st.State || l.Next != "/rules" {
		t.Fatalf("taken: %+v %v", l, ok)
	}
	if gone := tw.Result().Cookies(); len(gone) != 1 || gone[0].MaxAge >= 0 {
		t.Errorf("not taken away: %+v", gone)
	}
	c.t = t0.Add(loginFor)
	if _, ok, _ := take(); ok {
		t.Error("taken after ten minutes")
	}
}

func TestClaims(t *testing.T) {
	c := Claims{UserClaim: "email", GroupsClaim: "groups", Verified: true}
	for name, tc := range map[string]struct {
		claims map[string]any
		ok     bool
		groups int
	}{
		"verified":              {map[string]any{"email": "a@x", "email_verified": true, "groups": []any{"a", "b", 3}}, true, 2},
		"verified as a string":  {map[string]any{"email": "a@x", "email_verified": "true", "groups": "one"}, true, 1},
		"unverified":            {map[string]any{"email": "a@x", "email_verified": false}, false, 0},
		"blank":                 {map[string]any{"email": "  ", "email_verified": true}, false, 0},
		"too long":              {map[string]any{"email": strings.Repeat("a", 201), "email_verified": true}, false, 0},
		"groups not a list":     {map[string]any{"email": "a@x", "email_verified": true, "groups": map[string]any{}}, true, 0},
		"a thousand groups max": {map[string]any{"email": "a@x", "email_verified": true, "groups": manyGroups(1500)}, true, maxGroups},
	} {
		id, err := c.Identity(tc.claims)
		if (err == nil) != tc.ok || len(id.Groups) != tc.groups {
			t.Errorf("%s: %+v %v", name, id, err)
		}
	}
	// A proxy's claims: email_verified isn't asked.
	if id, err := (Claims{UserClaim: "email"}).Identity(map[string]any{"email": "a@x"}); err != nil || id.User != "a@x" {
		t.Errorf("a proxy's: %+v %v", id, err)
	}
}

func manyGroups(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = "g" + strings.Repeat("x", i%5) + string(rune('a'+i%26))
	}
	return out
}

func query(t *testing.T, u, key string) string {
	t.Helper()
	r := httptest.NewRequest("GET", u, nil)
	return r.URL.Query().Get(key)
}

// iapServer serves an ES256 key as Google serves IAP's.
func iapServer(t *testing.T) (*ecdsa.PrivateKey, *httptest.Server, *atomic.Int64) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "iap1", Algorithm: "ES256", Use: "sig"}}})
	}))
	t.Cleanup(srv.Close)
	return key, srv, &n
}

const iapAud = "/projects/123456789012/global/backendServices/1234567890123456789"

func TestGoogleIAP(t *testing.T) {
	c := &clock{t0}
	key, srv, _ := iapServer(t)
	p, err := NewProxy(Proxy{Kind: GoogleIAP, Audience: iapAud, KeysURL: srv.URL, Client: Client(nil), Now: c.now, Claims: Claims{UserClaim: "email"}})
	if err != nil {
		t.Fatal(err)
	}
	claims := func(change func(map[string]any)) map[string]any {
		m := map[string]any{"iss": "https://cloud.google.com/iap", "aud": iapAud, "email": "ana@example.com", "sub": "accounts.google.com:1",
			"iat": t0.Unix(), "exp": t0.Add(10 * time.Minute).Unix()}
		if change != nil {
			change(m)
		}
		return m
	}
	ask := func(vals ...string) (Identity, error) {
		r := httptest.NewRequest("GET", "/", nil)
		for _, v := range vals {
			r.Header.Add("x-goog-iap-jwt-assertion", v)
		}
		return p.Identify(r)
	}
	if id, err := ask(idptest.Sign(key, "iap1", jose.ES256, claims(nil))); err != nil || id.User != "ana@example.com" {
		t.Fatalf("IAP's own: %+v %v", id, err)
	}
	if _, err := ask(); !errors.Is(err, ErrNoHeader) {
		t.Errorf("no header: %v", err)
	}
	good := idptest.Sign(key, "iap1", jose.ES256, claims(nil))
	if _, err := ask(good, good); err == nil {
		t.Error("the header twice")
	}
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	for name, tok := range map[string]string{
		"another backend": idptest.Sign(key, "iap1", jose.ES256, claims(func(m map[string]any) { m["aud"] = "/projects/1/global/backendServices/2" })),
		"another issuer":  idptest.Sign(key, "iap1", jose.ES256, claims(func(m map[string]any) { m["iss"] = "https://accounts.google.com" })),
		"expired":         idptest.Sign(key, "iap1", jose.ES256, claims(func(m map[string]any) { m["exp"] = t0.Add(-time.Minute).Unix() })),
		"from the future": idptest.Sign(key, "iap1", jose.ES256, claims(func(m map[string]any) { m["iat"] = t0.Add(time.Hour).Unix() })),
		"another key":     idptest.Sign(other, "iap1", jose.ES256, claims(nil)),
		"RS256":           idptest.Sign(mustRSA(t), "iap1", jose.RS256, claims(nil)),
		"no email":        idptest.Sign(key, "iap1", jose.ES256, claims(func(m map[string]any) { delete(m, "email") })),
		"not a JWT":       "ana@example.com",
	} {
		if id, err := ask(tok); err == nil {
			t.Errorf("%s: taken as %+v", name, id)
		}
	}
}

// Any sign-in in front whose JWT the keys at a URL verify: oauth2-proxy's
// ID token as a bearer token, here.
func TestJWKSProxy(t *testing.T) {
	c := &clock{t0}
	idp := idptest.New(t, "proxy-client", "", c.now)
	p, err := NewProxy(Proxy{Kind: JWKS, Header: "Authorization", JWKS: idp.URL + "/keys", Issuer: idp.URL, Audience: "proxy-client",
		Client: Client(nil), Now: c.now, Claims: Claims{UserClaim: "email", GroupsClaim: "groups"}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+idp.IDToken(map[string]any{"email": "ana@example.com", "groups": []any{"sre"}}))
	if id, err := p.Identify(r); err != nil || id.User != "ana@example.com" || id.Groups[0] != "sre" {
		t.Fatalf("%+v %v", id, err)
	}
	r.Header.Set("Authorization", "Basic "+idp.IDToken(map[string]any{"email": "ana@example.com"}))
	if _, err := p.Identify(r); err == nil {
		t.Error("not a bearer token")
	}
	idp.Mutate = func(m map[string]any) { m["aud"] = "another-client" }
	r.Header.Set("Authorization", "Bearer "+idp.IDToken(map[string]any{"email": "ana@example.com"}))
	if _, err := p.Identify(r); err == nil {
		t.Error("another audience")
	}
	for _, bad := range []Proxy{
		{Kind: JWKS, JWKS: idp.URL + "/keys", Issuer: idp.URL, Audience: "a"},
		{Kind: JWKS, Header: "X-Jwt", JWKS: "http://idp.example.com/keys", Issuer: idp.URL, Audience: "a"},
		{Kind: JWKS, Header: "X-Jwt", JWKS: idp.URL + "/keys", Audience: "a"},
		{Kind: JWKS, Header: "X-Jwt", JWKS: idp.URL + "/keys", Issuer: idp.URL},
		{Kind: GoogleIAP},
		{Kind: AWSALB, ALBArn: "arn:aws:elasticloadbalancing:eu-west-1:123456789012:loadbalancer/net/x/0123456789abcdef"},
		{Kind: "other"},
	} {
		if _, err := NewProxy(bad); err == nil {
			t.Errorf("%+v: taken", bad)
		}
	}
}

func mustRSA(t *testing.T) any {
	t.Helper()
	idp := idptest.New(t, "x", "", time.Now)
	return idp.Key
}

const albArn = "arn:aws:elasticloadbalancing:eu-west-1:123456789012:loadbalancer/app/lapilli/0123456789abcdef"

// albToken is a JWT as an AWS load balancer signs one: ES256, the signer
// and expiry in its header, and its parts padded with =.
func albToken(key *ecdsa.PrivateKey, kid string, header, claims map[string]any) string {
	h := map[string]any{"alg": "ES256", "kid": kid, "signer": albArn, "iss": "https://idp.example.com", "client": "c", "exp": t0.Add(time.Minute).Unix()}
	for k, v := range header {
		h[k] = v
	}
	hb, _ := json.Marshal(h)
	cb, _ := json.Marshal(claims)
	in := base64.URLEncoding.EncodeToString(hb) + "." + base64.URLEncoding.EncodeToString(cb)
	sum := sha256.Sum256([]byte(in))
	r, s, _ := ecdsa.Sign(rand.Reader, key, sum[:])
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return in + "." + base64.URLEncoding.EncodeToString(sig)
}

// toServer sends every request to a test server, as AWS's key host would
// be reached: the request's own host and path are kept for the test to see.
type toServer struct{ srv *httptest.Server }

func (t toServer) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.Header.Set("X-Asked-Host", r.URL.Host)
	r2.URL.Scheme, r2.URL.Host = "http", strings.TrimPrefix(t.srv.URL, "http://")
	return http.DefaultTransport.RoundTrip(r2)
}

// albServer serves keys as AWS does for eu-west-1, by id; asked is each
// path asked for, with its host.
func albServer(t *testing.T, keys map[string]*ecdsa.PrivateKey) (*http.Client, *[]string) {
	var asked []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.Header.Get("X-Asked-Host")+r.URL.Path)
		mu.Unlock()
		k, ok := keys[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok || r.Header.Get("X-Asked-Host") != "public-keys.auth.elb.eu-west-1.amazonaws.com" {
			http.NotFound(w, r)
			return
		}
		der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
		pem.Encode(w, &pem.Block{Type: "PUBLIC KEY", Bytes: der})
	}))
	t.Cleanup(srv.Close)
	return Client(toServer{srv}), &asked
}

func TestAWSALB(t *testing.T) {
	c := &clock{t0}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	client, asked := albServer(t, map[string]*ecdsa.PrivateKey{"kid-1": key})
	p, err := NewProxy(Proxy{Kind: AWSALB, ALBArn: albArn, Issuer: "https://idp.example.com", Client: client, Now: c.now,
		Claims: Claims{UserClaim: "email", GroupsClaim: "groups"}})
	if err != nil {
		t.Fatal(err)
	}
	ask := func(tok string) (Identity, error) {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("X-Amzn-Oidc-Data", tok)
		return p.Identify(r)
	}
	user := map[string]any{"sub": "1", "email": "ana@example.com", "groups": []any{"sre"}, "exp": t0.Add(time.Minute).Unix()}
	good := albToken(key, "kid-1", nil, user)
	if !strings.Contains(good, "=") {
		t.Fatal("the test's token should be padded, as a load balancer's are")
	}
	if id, err := ask(good); err != nil || id.User != "ana@example.com" || id.Groups[0] != "sre" {
		t.Fatalf("the load balancer's own: %+v %v", id, err)
	}
	// The key is fetched once, from the region's own host.
	ask(good)
	if len(*asked) != 1 || (*asked)[0] != "public-keys.auth.elb.eu-west-1.amazonaws.com/kid-1" {
		t.Errorf("keys fetched %v", *asked)
	}
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	parts := strings.Split(good, ".")
	forged, _ := json.Marshal(map[string]any{"email": "boss@example.com"})
	for name, tok := range map[string]string{
		"another load balancer": albToken(key, "kid-1", map[string]any{"signer": strings.Replace(albArn, "lapilli", "other", 1)}, user),
		"another provider":      albToken(key, "kid-1", map[string]any{"iss": "https://evil.example"}, user),
		"expired":               albToken(key, "kid-1", map[string]any{"exp": t0.Unix()}, user),
		"no expiry":             albToken(key, "kid-1", map[string]any{"exp": 0}, user),
		"claims expired":        albToken(key, "kid-1", nil, map[string]any{"email": "ana@example.com", "exp": t0.Unix()}),
		"RS256":                 albToken(key, "kid-1", map[string]any{"alg": "RS256"}, user),
		"another key":           albToken(other, "kid-1", nil, user),
		"claims changed":        parts[0] + "." + base64.URLEncoding.EncodeToString(forged) + "." + parts[2],
		"two parts":             parts[0] + "." + parts[1],
		"email not verified":    albToken(key, "kid-1", nil, map[string]any{"email": "boss@example.com", "email_verified": false, "exp": t0.Add(time.Minute).Unix()}),
	} {
		if id, err := ask(tok); err == nil {
			t.Errorf("%s: taken as %+v", name, id)
		}
	}
	// A key id that would leave the keys' path is never fetched, though the
	// budget allows it.
	before := len(*asked)
	for _, kid := range []string{"../../evil", "a/b", "x?y=1", "a b", "", "x@evil.example"} {
		if _, err := ask(albToken(key, kid, nil, user)); err == nil {
			t.Errorf("kid %q taken", kid)
		}
	}
	if len(*asked) != before {
		t.Errorf("fetched %v for key ids that aren't", (*asked)[before:])
	}
	// The ARN names the partition and region the keys are fetched from;
	// nothing else does.
	for _, c := range []struct{ partition, region, want string }{
		{"aws", "eu-west-1", "https://public-keys.auth.elb.eu-west-1.amazonaws.com/k"},
		{"aws-cn", "cn-north-1", "https://public-keys.auth.elb.cn-north-1.amazonaws.com.cn/k"},
		{"aws-us-gov", "us-gov-west-1", "https://s3-us-gov-west-1.amazonaws.com/aws-elb-public-keys-prod-us-gov-west-1/k"},
		{"aws", "evil.example/x", ""}, {"other", "eu-west-1", ""},
	} {
		if got, _ := albKeyURL(c.partition, c.region, "k"); got != c.want {
			t.Errorf("%s %s: %q", c.partition, c.region, got)
		}
	}
}

// Keys not held come from a budget, a few at once and one each ten seconds
// after; a key id the load balancer doesn't have is remembered and costs
// nothing again. A made-up key each ten seconds can't keep the load
// balancer's own new key from being fetched.
func TestAWSALBKeyBudget(t *testing.T) {
	c := &clock{t0}
	legit, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	client, asked := albServer(t, map[string]*ecdsa.PrivateKey{"kid-legit": legit})
	p, err := NewProxy(Proxy{Kind: AWSALB, ALBArn: albArn, Client: client, Now: c.now, Claims: Claims{UserClaim: "email"}})
	if err != nil {
		t.Fatal(err)
	}
	attacker, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ask := func(key *ecdsa.PrivateKey, kid string) error {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("X-Amzn-Oidc-Data", albToken(key, kid, map[string]any{"exp": c.t.Add(time.Minute).Unix()},
			map[string]any{"email": "ana@example.com", "exp": c.t.Add(time.Minute).Unix()}))
		_, err := p.Identify(r)
		return err
	}
	refused := 0
	for i := range 60 {
		ask(attacker, fmt.Sprintf("made-up-%d", i))
		c.t = c.t.Add(time.Second)
		if ask(legit, "kid-legit") != nil {
			refused++
		}
		c.t = c.t.Add(9 * time.Second)
	}
	if refused != 0 {
		t.Errorf("%d sign-ins through the load balancer refused while keys were made up", refused)
	}
	// One made-up key, sent again and again: fetched once.
	before := len(*asked)
	for range 20 {
		ask(attacker, "made-up-again")
		c.t = c.t.Add(time.Minute)
	}
	if n := len(*asked) - before; n != 1 {
		t.Errorf("a key id that isn't one fetched %d times", n)
	}
	// A burst of made-up keys spends the budget, which fills again.
	for i := range 20 {
		ask(attacker, fmt.Sprintf("burst-%d", i))
	}
	if n := len(*asked) - before - 1; n != albBurst {
		t.Errorf("%d fetches in a burst, want %d", n, albBurst)
	}
}

// What a sign-in in front says about the email holds for every kind: one
// it says isn't verified is refused.
func TestProxyUnverifiedEmail(t *testing.T) {
	c := &clock{t0}
	p, err := NewProxy(Proxy{Kind: JWKS, Header: "X-Jwt", JWKS: "https://idp.example.com/keys", Issuer: "x", Audience: "y", Claims: Claims{UserClaim: "email"}, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	for v, ok := range map[any]bool{false: false, "false": false, true: true} {
		_, err := p.Claims.Identity(map[string]any{"email": "boss@example.com", "email_verified": v})
		if (err == nil) != ok {
			t.Errorf("email_verified %v: %v", v, err)
		}
	}
	if _, err := p.Claims.Identity(map[string]any{"email": "boss@example.com"}); err != nil {
		t.Errorf("a proxy that doesn't say (IAP): %v", err)
	}
}

// Names and groups that show as another's are refused: spaces around them,
// spacing that isn't the ASCII space, characters that don't show.
func TestPlainNames(t *testing.T) {
	for n, want := range map[string]bool{
		"ana@example.com": true, "Ana Lopez": true, "아나": true,
		"ana@example.com ": false, " ana@example.com": false, "ana@example.com\u00a0": false, "ana@example.com\u3000": false,
		"ana\u2028": false, "ana\tx": false, "": false, "\xff": false,
		// Joiners, as Persian names and emoji are written with: lapilli
		// serve's ledger.Hidden tells them from those that hide.
		"\u0645\u0647\u062f\u06cc\u200c\u0632\u0627\u062f\u0647": true,
	} {
		if Plain(n) != want {
			t.Errorf("%q: %v", n, !want)
		}
	}
	id, err := (Claims{UserClaim: "email", GroupsClaim: "groups"}).Identity(map[string]any{"email": "a@x", "groups": []any{"ops", "ops\u00a0", " ops", "o\u00a0ps"}})
	if err != nil || len(id.Groups) != 1 {
		t.Errorf("groups: %+v %v", id, err)
	}
	if !Plain("Ana Lopez") || Plain("Ana\u00a0Lopez") || Plain("Ana\u3000Lopez") || Plain("Ana\u2009Lopez") {
		t.Error("spacing inside a name")
	}
	for _, n := range []string{"ana@example.com\u00a0", "ana\u00a0@example.com", "ana@example.com\u3000"} {
		if id, err := (Claims{UserClaim: "email"}).Identity(map[string]any{"email": n}); err == nil {
			t.Errorf("%q taken as %+v", n, id)
		}
	}
}

// A token given to another client is refused, even naming this one alone,
// unless that client is trusted.
func TestOIDCOtherClient(t *testing.T) {
	c := &clock{t0}
	idp, o := newIdP(t, c)
	ctx := context.Background()
	idp.Mutate = func(m map[string]any) { m["azp"] = "cli" }
	tok := idp.IDToken(map[string]any{"email": "ana@example.com", "email_verified": true})
	if _, err := o.Bearer(ctx, tok); err == nil {
		t.Error("another client's token taken")
	}
	o.TrustedClients = []string{"cli"}
	if _, err := o.Bearer(ctx, tok); err != nil {
		t.Errorf("a trusted client's: %v", err)
	}
	idp.Mutate = func(m map[string]any) { m["azp"] = "lapilli" }
	if _, err := o.Bearer(ctx, idp.IDToken(map[string]any{"email": "ana@example.com", "email_verified": true})); err != nil {
		t.Errorf("this client's own azp: %v", err)
	}
}

// An issuer given otherwise than the provider says, or a provider that
// answers with a client's error, is a setting to put right; a server's
// error or no answer, an outage.
func TestOIDCDiscoveryErrors(t *testing.T) {
	c := &clock{t0}
	idp, o := newIdP(t, c)
	ctx := context.Background()
	o.Issuer = idp.URL + "/"
	if err := o.Discover(ctx); !errors.Is(err, ErrMisconfigured) {
		t.Errorf("a trailing slash: %v", err)
	}
	for code, want := range map[int]error{404: ErrMisconfigured, 503: ErrUnreachable} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
		o2 := &OIDC{Issuer: srv.URL, ClientID: "x", Client: Client(nil), Now: c.now}
		if err := o2.Discover(ctx); !errors.Is(err, want) {
			t.Errorf("%d: %v", code, err)
		}
		srv.Close()
	}
	o3 := &OIDC{Issuer: "http://127.0.0.1:1", ClientID: "x", Client: Client(nil), Now: c.now}
	if err := o3.Discover(ctx); !errors.Is(err, ErrUnreachable) {
		t.Errorf("no answer: %v", err)
	}
	// Asked again within ten seconds: the same error, no new request.
	if err := o3.Discover(ctx); !errors.Is(err, ErrUnreachable) {
		t.Errorf("again: %v", err)
	}
}

// Keys held for six hours are fetched again, in the background: the
// request whose key is held doesn't wait, and a key the provider took away
// stops being taken.
func TestKeysRefreshedInBackground(t *testing.T) {
	c := &clock{t0}
	idp, o := newIdP(t, c)
	ctx := context.Background()
	good := map[string]any{"email": "ana@example.com", "email_verified": true}
	if _, err := o.Bearer(ctx, idp.IDToken(good)); err != nil {
		t.Fatal(err)
	}
	c.t = t0.Add(keysMaxAge)
	if _, err := o.Bearer(ctx, idp.IDToken(good)); err != nil {
		t.Fatalf("a key held, old: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for idp.KeyFetches.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := idp.KeyFetches.Load(); n != 2 {
		t.Errorf("%d fetches after six hours, want 2", n)
	}
}
