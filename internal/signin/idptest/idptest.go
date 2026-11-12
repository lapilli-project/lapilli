// Package idptest is an OpenID Connect provider for tests: discovery, keys,
// and a token endpoint that hands out ID tokens for codes it gave, once
// each, checking PKCE and the redirect URI, as a real provider does.
package idptest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// IdP is a provider running on a local port.
type IdP struct {
	*httptest.Server
	ClientID, Secret string
	Key              *rsa.PrivateKey
	KID              string
	Now              func() time.Time
	// Mutate changes the claims of each ID token the token endpoint gives,
	// before it is signed: a wrong audience, an expired token.
	Mutate func(map[string]any)
	// KeyFetches counts the requests for keys.
	KeyFetches atomic.Int64

	mu    sync.Mutex
	codes map[string]*grant
}

type grant struct {
	claims              map[string]any
	challenge, redirect string
	used                bool
}

// New starts a provider for a client, with a secret ("" for a public
// client); it stops with the test.
func New(t testing.TB, clientID, secret string, now func() time.Time) *IdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &IdP{ClientID: clientID, Secret: secret, Key: key, KID: "k1", Now: now, codes: map[string]*grant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": p.URL, "authorization_endpoint": p.URL + "/auth", "token_endpoint": p.URL + "/token",
			"jwks_uri": p.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"},
			"response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
		})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, r *http.Request) {
		p.KeyFetches.Add(1)
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &p.Key.PublicKey, KeyID: p.KID, Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("POST /token", p.token)
	p.Server = httptest.NewServer(mux)
	t.Cleanup(p.Close)
	return p
}

// Authorize plays a person signing in at the provider: given the URL
// lapilli sent the browser to, it checks what lapilli asked for and gives
// the URL the provider sends the browser back to, with a code for an ID
// token with claims (and the nonce asked for).
func (p *IdP) Authorize(t testing.TB, authURL string, claims map[string]any) string {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	for k, want := range map[string]string{"response_type": "code", "client_id": p.ClientID, "code_challenge_method": "S256"} {
		if q.Get(k) != want {
			t.Fatalf("authorize: %s is %q, want %q (%s)", k, q.Get(k), want, authURL)
		}
	}
	for _, k := range []string{"state", "nonce", "code_challenge", "redirect_uri"} {
		if q.Get(k) == "" {
			t.Fatalf("authorize: no %s (%s)", k, authURL)
		}
	}
	c := map[string]any{"nonce": q.Get("nonce")}
	for k, v := range claims {
		c[k] = v
	}
	code := rand.Text()
	p.mu.Lock()
	p.codes[code] = &grant{claims: c, challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri")}
	p.mu.Unlock()
	back, _ := url.Parse(q.Get("redirect_uri"))
	bq := back.Query()
	bq.Set("code", code)
	bq.Set("state", q.Get("state"))
	back.RawQuery = bq.Encode()
	return back.String()
}

// token exchanges a code, once, for an ID token.
func (p *IdP) token(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	} else {
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	}
	fail := func(code string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": code})
	}
	if id != p.ClientID || secret != p.Secret {
		fail("invalid_client")
		return
	}
	p.mu.Lock()
	g := p.codes[r.PostForm.Get("code")]
	used := g != nil && g.used
	if g != nil {
		g.used = true
	}
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	switch {
	case r.PostForm.Get("grant_type") != "authorization_code", g == nil, used:
		fail("invalid_grant")
		return
	case base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge, r.PostForm.Get("redirect_uri") != g.redirect:
		fail("invalid_grant")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": p.IDToken(g.claims)})
}

// IDToken is an ID token signed with the provider's key: iss, aud, sub,
// iat and exp as for a fresh one, then the claims given, then Mutate.
func (p *IdP) IDToken(claims map[string]any) string {
	now := p.Now()
	c := map[string]any{"iss": p.URL, "aud": p.ClientID, "sub": "subject", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}
	for k, v := range claims {
		c[k] = v
	}
	if p.Mutate != nil {
		p.Mutate(c)
	}
	return Sign(p.Key, p.KID, jose.RS256, c)
}

// Sign is a compact JWS of claims, signed with a key.
func Sign(key any, kid string, alg jose.SignatureAlgorithm, claims map[string]any) string {
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", kid).WithType("JWT"))
	if err != nil {
		panic(err)
	}
	b, _ := json.Marshal(claims)
	jws, err := s.Sign(b)
	if err != nil {
		panic(err)
	}
	out, err := jws.CompactSerialize()
	if err != nil {
		panic(err)
	}
	return out
}
