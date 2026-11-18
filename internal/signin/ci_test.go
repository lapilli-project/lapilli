package signin

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"lapilli.dev/internal/signin/idptest"
)

const ciAudience = "https://lapilli-ci.example.com"

// ciClock is a time a test moves by hand.
type ciClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *ciClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *ciClock) move(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

func newCI(t *testing.T, path string) (*idptest.CI, *CI, *ciClock) {
	t.Helper()
	clk := &ciClock{at: time.Now().Truncate(time.Second)}
	idp := idptest.NewCI(t, path, clk.now)
	return idp, &CI{Issuer: idp.Issuer, Client: Client(nil), Now: clk.now}, clk
}

// The tokens of GitHub Actions, GitLab and Forgejo, as each writes its
// claims, verify, and say them as written: a GitLab's ids as strings, a
// Forgejo's repository_id as a number, both read as the decimal.
func TestCITokens(t *testing.T) {
	for _, c := range []struct {
		path, claim, want string
		claims            map[string]any
	}{
		{"", "repository_id", "123456789", idptest.GitHub("123456789", "42")},
		{"", "project_id", "42", idptest.GitLab("42")},
		{"/api/actions", "repository_id", "9007199254", idptest.Forgejo(9007199254)},
	} {
		idp, ci, _ := newCI(t, c.path)
		tok, err := ci.Verify(context.Background(), idp.Token(ciAudience, c.claims), ciAudience)
		if err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}
		if got, ok := ClaimString(tok.Claims[c.claim]); !ok || got != c.want {
			t.Errorf("%s: %s = %q %t, want %q", c.path, c.claim, got, ok, c.want)
		}
		if tok.IssuedAt.IsZero() || tok.Expiry.IsZero() {
			t.Errorf("%+v", tok)
		}
	}
}

// What a token must be, each check alone: for this audience and no other
// beside it, from this issuer, not expired, not before its nbf, issued in
// the last hour (whatever its exp) and not in the future, with an iat.
func TestCITokenRefused(t *testing.T) {
	idp, ci, _ := newCI(t, "")
	now := idp.Now()
	for name, claims := range map[string]map[string]any{
		"another audience":  {"aud": "https://other.example.com"},
		"two audiences":     {"aud": []string{ciAudience, "sts.amazonaws.com"}},
		"no audience":       {"aud": nil},
		"another issuer":    {"iss": "https://token.actions.example.com"},
		"expired":           {"exp": now.Add(-time.Minute).Unix()},
		"no exp":            {"exp": nil},
		"not yet":           {"nbf": now.Add(10 * time.Minute).Unix()},
		"over an hour old":  {"iat": now.Add(-61 * time.Minute).Unix(), "exp": now.Add(time.Hour).Unix()},
		"issued in a while": {"iat": now.Add(10 * time.Minute).Unix()},
		"no iat":            {"iat": nil},
		"iat with a point":  {"iat": float64(now.Unix()) + 0.5},
		"iat as a string":   {"iat": "1700000000"},
	} {
		_, err := ci.Verify(context.Background(), idp.Token(ciAudience, claims), ciAudience)
		if !errors.Is(err, ErrCIToken) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// An hour old exactly is taken; a second more isn't.
	if _, err := ci.Verify(context.Background(), idp.Token(ciAudience, map[string]any{"iat": now.Add(-time.Hour).Unix(), "exp": now.Add(time.Hour).Unix()}), ciAudience); err != nil {
		t.Error(err)
	}
}

// No token signed with a shared secret, or not at all, is taken, whatever
// the key set holds: one signed with HMAC by the issuer's public key, as
// a confused verifier would take it, is refused; so is alg none; and an
// RSA key under 2048 bits, or over 4096, isn't one a CI's token verifies
// by.
func TestCITokenAlgorithms(t *testing.T) {
	idp, ci, _ := newCI(t, "")
	now := idp.Now()
	claims := map[string]any{"iss": idp.Issuer, "aud": ciAudience, "iat": now.Unix(), "exp": now.Add(time.Minute).Unix(), "repository_id": "1"}
	der, _ := x509.MarshalPKIXPublicKey(&idp.Key.PublicKey)
	for _, secret := range [][]byte{pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), der, idp.Key.PublicKey.N.Bytes()} {
		hs := idptest.Sign(secret, idp.KID, jose.HS256, claims)
		if _, err := ci.Verify(context.Background(), hs, ciAudience); err == nil {
			t.Error("a token signed with HS256 by the public key was taken")
		}
	}
	b, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding.EncodeToString
	none := enc([]byte(`{"alg":"none","typ":"JWT","kid":"`+idp.KID+`"}`)) + "." + enc(b) + "."
	if _, err := ci.Verify(context.Background(), none, ciAudience); err == nil {
		t.Error("a token with alg none was taken")
	}
	for _, bits := range []int{1024, 4096 + 1024} {
		weak, err := rsa.GenerateKey(rand.Reader, bits)
		if err != nil {
			t.Fatal(err)
		}
		idp.Key = weak
		idp2 := &CI{Issuer: idp.Issuer, Client: Client(nil), Now: idp.Now}
		if _, err := idp2.Verify(context.Background(), idp.Token(ciAudience, nil), ciAudience); err == nil {
			t.Errorf("a token signed by an RSA key of %d bits was taken", bits)
		}
	}
}

// Keys are fetched from the issuer's own origin, as its discovery document
// names them, or from the URL given: one at another origin is refused, and
// nothing is fetched there; a discovery document naming another issuer is
// refused.
func TestCIKeysWhere(t *testing.T) {
	idp, ci, _ := newCI(t, "")
	var elsewhere int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere++
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &idp.Key.PublicKey, KeyID: idp.KID, Algorithm: "RS256", Use: "sig"}}})
	}))
	defer other.Close()
	idp.JWKS = other.URL + "/keys"
	_, err := ci.Verify(context.Background(), idp.Token(ciAudience, nil), ciAudience)
	if !errors.Is(err, ErrMisconfigured) || elsewhere != 0 {
		t.Errorf("keys at another origin: %v, fetched there %d times", err, elsewhere)
	}
	given := &CI{Issuer: idp.Issuer, JWKS: other.URL + "/keys", Client: Client(nil), Now: idp.Now}
	if _, err := given.Verify(context.Background(), idp.Token(ciAudience, nil), ciAudience); err != nil || elsewhere != 1 {
		t.Errorf("keys at the URL given: %v, fetched %d times", err, elsewhere)
	}
	wrong := &CI{Issuer: idp.Issuer + "/", Client: Client(nil), Now: idp.Now}
	if _, err := wrong.Verify(context.Background(), idp.Token(ciAudience, nil), ciAudience); !errors.Is(err, ErrMisconfigured) {
		t.Errorf("an issuer its document doesn't name: %v", err)
	}
}

// A token naming a key the issuer doesn't hold makes lapilli fetch the
// keys once a minute at most, however many are sent; a discovery that
// failed is asked again once a minute at most.
func TestCIFetchesSparingly(t *testing.T) {
	idp, ci, clk := newCI(t, "")
	if _, err := ci.Verify(context.Background(), idp.Token(ciAudience, nil), ciAudience); err != nil {
		t.Fatal(err)
	}
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := idp.Now().Add(time.Minute)
	made := idptest.Sign(other, "made-up", jose.RS256, map[string]any{"iss": idp.Issuer, "aud": ciAudience, "iat": now.Unix(), "exp": now.Add(10 * time.Minute).Unix()})
	clk.move(time.Minute) // the first fetch's minute over
	before := idp.KeyFetches.Load()
	for range 50 {
		if _, err := ci.Verify(context.Background(), made, ciAudience); err == nil {
			t.Fatal("a token of a key made up was taken")
		}
	}
	if n := idp.KeyFetches.Load() - before; n != 1 {
		t.Errorf("50 tokens of a key made up fetched the keys %d times, want once", n)
	}
	clk.move(time.Minute)
	ci.Verify(context.Background(), made, ciAudience)
	if n := idp.KeyFetches.Load() - before; n != 2 {
		t.Errorf("a minute on, %d fetches, want 2", n)
	}

	var asked int
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failing.Close()
	down := &CI{Issuer: failing.URL, Client: Client(nil), Now: clk.now}
	for range 20 {
		if _, err := down.Verify(context.Background(), "x.y.z", ciAudience); !errors.Is(err, ErrUnreachable) {
			t.Fatalf("an issuer out of reach: %v", err)
		}
	}
	if asked != 1 {
		t.Errorf("20 tokens of an issuer out of reach asked it %d times within the minute, want once", asked)
	}
	clk.move(time.Minute)
	down.Verify(context.Background(), "x.y.z", ciAudience)
	if asked != 2 {
		t.Errorf("a minute on, asked %d times, want 2", asked)
	}
}

// A pin is held to a claim as a string as it is, or an integer as its
// decimal, written as it was; nothing else matches one.
func TestClaimString(t *testing.T) {
	for raw, want := range map[string]string{`"123"`: "123", `123`: "123", `-4`: "-4", `"a b"`: "a b", `0`: "0"} {
		if got, ok := ClaimString(json.RawMessage(raw)); !ok || got != want {
			t.Errorf("%s: %q %t", raw, got, ok)
		}
	}
	for _, raw := range []string{`1.0`, `1e3`, `0123`, `true`, `null`, `["123"]`, `{"id":"123"}`, ``, `"unterminated`} {
		if got, ok := ClaimString(json.RawMessage(raw)); ok {
			t.Errorf("%s matched as %q", raw, got)
		}
	}
}
