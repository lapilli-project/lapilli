package signin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	// 4096 bits, the most a CI's key is taken at, verifies.
	big, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		t.Fatal(err)
	}
	idp.Key = big
	fresh := &CI{Issuer: idp.Issuer, Client: Client(nil), Now: idp.Now}
	if _, err := fresh.Verify(context.Background(), idp.Token(ciAudience, nil), ciAudience); err != nil {
		t.Errorf("a token signed by an RSA key of 4096 bits: %v", err)
	}
	// Under MinRSABits, or past 4096 (which go-jose doesn't take either).
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

// samePayload is raw with the last character of its signature changed in
// the bits base64url carries past the last byte: another string, which a
// lax decoder reads as the same token.
func samePayload(t *testing.T, raw string) string {
	t.Helper()
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	i := strings.LastIndexByte(raw, '.')
	sig := raw[i+1:]
	last := strings.IndexByte(alphabet, sig[len(sig)-1])
	// 256 bytes of RS256 are 342 characters: the last carries 2 bits of
	// the signature, and 4 passed over.
	other := raw[:len(raw)-1] + string(alphabet[last^1])
	if other == raw {
		t.Fatal("no variant")
	}
	return other
}

// A token written another way, in the bits a decoder passes over, verifies
// as the same token, with the same payload: what a token is known by, once
// it has no jti, is its payload, not how it was written.
func TestCITokenPayload(t *testing.T) {
	idp, ci, _ := newCI(t, "/api/actions")
	raw := idp.Token(ciAudience, map[string]any{"jti": nil, "repository_id": 7})
	a, err := ci.Verify(context.Background(), raw, ciAudience)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ci.Verify(context.Background(), samePayload(t, raw), ciAudience)
	if err != nil {
		t.Skipf("this go-jose refuses the variant: %v", err)
	}
	if !bytes.Equal(a.Payload, b.Payload) || len(a.Payload) == 0 || a.Claims["jti"] != nil {
		t.Errorf("payloads %q and %q", a.Payload, b.Payload)
	}
}

// One request asks the issuer for its discovery document; another, while
// it is being asked, doesn't wait on it, and asks nothing.
func TestCIDiscoveryAlone(t *testing.T) {
	var asked atomic.Int64
	hold, held := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if asked.Add(1) == 1 {
			close(held)
			<-hold
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	clk := &ciClock{at: time.Now()}
	ci := &CI{Issuer: srv.URL, Client: Client(nil), Now: clk.now}
	first := make(chan error)
	go func() {
		_, err := ci.Verify(context.Background(), "x.y.z", ciAudience)
		first <- err
	}()
	<-held
	_, err := ci.Verify(context.Background(), "x.y.z", ciAudience)
	if !errors.Is(err, ErrUnreachable) || !strings.Contains(err.Error(), "being asked") {
		t.Errorf("while being asked: %v", err)
	}
	close(hold)
	if err := <-first; !errors.Is(err, ErrUnreachable) {
		t.Errorf("the first: %v", err)
	}
	if n := asked.Load(); n != 1 {
		t.Errorf("asked %d times", n)
	}
}

// DiscoverKeys reads the issuer's discovery document as a token's first
// check does, under its caller's context, keeping nothing; Usable fetches
// the keys under it too, and counts those that verify a token of an
// algorithm the document names. A context already done sends nothing.
func TestCIDiscoverKeys(t *testing.T) {
	idp, ci, _ := newCI(t, "/api/actions")
	keys, err := ci.DiscoverKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if keys.URL() != idp.Issuer+"/.well-known/jwks" || strings.Join(keys.Algs(), ",") != "RS256" || idp.Discoveries.Load() != 1 || idp.KeyFetches.Load() != 0 {
		t.Fatalf("%s %v, %d discoveries, %d key fetches", keys.URL(), keys.Algs(), idp.Discoveries.Load(), idp.KeyFetches.Load())
	}
	if n, err := keys.Usable(context.Background()); n != 1 || err != nil || idp.KeyFetches.Load() != 1 {
		t.Errorf("%d usable, %v, %d fetches", n, err, idp.KeyFetches.Load())
	}
	// Kept nothing: asked again, asked again.
	if _, err := ci.DiscoverKeys(context.Background()); err != nil || idp.Discoveries.Load() != 2 {
		t.Errorf("%v, %d discoveries", err, idp.Discoveries.Load())
	}
	done, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ci.DiscoverKeys(done); err == nil || idp.Discoveries.Load() != 2 {
		t.Errorf("a context done: %v, %d discoveries", err, idp.Discoveries.Load())
	}
	if _, err := keys.Usable(done); err == nil || idp.KeyFetches.Load() != 1 {
		t.Errorf("a context done: %v, %d key fetches", err, idp.KeyFetches.Load())
	}
	// The keys the algorithms named verify, by type and curve.
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	for _, x := range []struct {
		algs []string
		want int
	}{{[]string{"RS256"}, 1}, {[]string{"PS512"}, 1}, {[]string{"ES256"}, 0}, {[]string{"EdDSA"}, 0}, {nil, 1}} {
		k := NewKeys("", nil, x.algs, nil)
		n := 0
		for _, a := range k.algs {
			if verifies(jose.JSONWebKey{Key: &rsaKey.PublicKey}, a) {
				n = 1
			}
		}
		if n != x.want {
			t.Errorf("%v: %d", x.algs, n)
		}
	}
}

// NewKeys takes each algorithm once, however many times a discovery
// document names it.
func TestKeysAlgorithmsOnce(t *testing.T) {
	k := NewKeys("", nil, []string{"RS256", "RS256", "ES256", "HS256", "RS256", "ES256"}, nil)
	if got := strings.Join(k.Algs(), ","); got != "RS256,ES256" {
		t.Errorf("%s", got)
	}
}

// DiscoverKeys skips what keysOf holds a token from anyone to (once a
// minute after a failure, one ask at a time): no code but lapilli
// preflight's calls it, so no path a request to serve takes does.
func TestDiscoverKeysPreflightAlone(t *testing.T) {
	var files []string
	for _, root := range []string{"../../internal", "../../cmd"} {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
				files = append(files, p)
			}
			return nil
		})
	}
	read := 0
	for _, f := range files {
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		read++
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "DiscoverKeys" &&
				!strings.HasPrefix(filepath.ToSlash(f), "../../internal/preflight/") && !strings.HasPrefix(filepath.ToSlash(f), "../../internal/signin/") {
				t.Errorf("%s calls DiscoverKeys: serve's paths call Discover and VerifyToken", f)
			}
			return true
		})
	}
	if read < 50 {
		t.Fatalf("%d files read", read)
	}
}

// A fetch that got no answer is said naming no host but the one asked: a
// redirect refused without where it led (the Location, another host, its
// query), a certificate without the names it is for or the authority that
// signed it; in what serve logs, and lapilli preflight prints.
func TestGetSaysNoOtherHost(t *testing.T) {
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://sso.corp-a.example/login?client=secret-client-77", http.StatusFound)
	}))
	defer redirect.Close()
	_, err := get(context.Background(), Client(nil), redirect.URL+"/.well-known/openid-configuration", keysBody)
	if err == nil || strings.Contains(err.Error(), "corp-a") || strings.Contains(err.Error(), "secret-client") ||
		!strings.Contains(err.Error(), "/.well-known/openid-configuration: it redirected, and redirects aren't followed") {
		t.Errorf("a redirect: %v", err)
	}
	ci := &CI{Issuer: redirect.URL, Client: Client(nil)}
	if _, err := ci.DiscoverKeys(context.Background()); err == nil || strings.Contains(err.Error(), "corp-a") || !errors.Is(err, ErrUnreachable) {
		t.Errorf("discovery redirected: %v", err)
	}
	// TLS: a certificate of an authority not trusted, and one trusted but
	// for other names than the host asked (localhost: the test's certificate
	// is for example.com and 127.0.0.1).
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer tlsSrv.Close()
	_, err = get(context.Background(), Client(nil), tlsSrv.URL+"/keys", keysBody)
	if err == nil || strings.Contains(err.Error(), "x509") || !strings.Contains(err.Error(), "of an authority this machine doesn't trust") {
		t.Errorf("an authority not trusted: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(tlsSrv.Certificate())
	trusting := Client(&http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}})
	_, port, _ := strings.Cut(strings.TrimPrefix(tlsSrv.URL, "https://"), ":")
	_, err = get(context.Background(), trusting, "https://localhost:"+port+"/keys", keysBody)
	names := strings.Join(tlsSrv.Certificate().DNSNames, " ")
	if err == nil || names == "" || strings.Contains(err.Error(), tlsSrv.Certificate().DNSNames[0]) || !strings.Contains(err.Error(), "isn't for the host asked") {
		t.Errorf("a certificate for %s: %v", names, err)
	}
	// One that doesn't resolve names the host asked alone, as it is.
	if _, err := get(context.Background(), Client(nil), "http://nowhere.invalid/keys", keysBody); err == nil || !strings.Contains(err.Error(), "nowhere.invalid") {
		t.Errorf("a name that doesn't resolve: %v", err)
	}
}
