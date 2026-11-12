// Package signin says who a request to lapilli serve comes from, when serve
// signs people in itself (OpenID Connect) or takes them from a sign-in in
// front that signs what it says (a cloud's identity-aware proxy, a load
// balancer's sign-in, oauth2-proxy and the like): it verifies ID tokens and
// signed headers, and keeps sessions in signed cookies
// (docs/design/signin.md).
//
// Everything here reads what comes from outside before anyone is known, so
// it bounds what that can make lapilli do: keys are fetched from the URLs
// lapilli was given, never from a URL a token names, at most once a minute
// for a key it hasn't seen, with a time limit, no redirects and a limit on
// what is read.
package signin

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// Asymmetric are the signing algorithms taken: those of a key pair. A
// token signed with a shared secret (HS256), or not at all, is refused
// whatever the key set holds.
var Asymmetric = []jose.SignatureAlgorithm{jose.RS256, jose.RS384, jose.RS512, jose.PS256, jose.PS384, jose.PS512,
	jose.ES256, jose.ES384, jose.ES512, jose.EdDSA}

const (
	// keysRetry is how soon keys are fetched again for a token whose key
	// isn't held: a token with a key made up can't make lapilli fetch
	// them more often.
	keysRetry = time.Minute
	// keysFirstRetry is the same while no key is held at all, as when the
	// identity provider couldn't be reached at start.
	keysFirstRetry = 10 * time.Second
	// keysMaxAge is how long keys are used before they are fetched again,
	// so a key the provider took away stops being taken.
	keysMaxAge = 6 * time.Hour
	// keysBody is how much of an answer with keys is read.
	keysBody = 1 << 20
	// maxKeys is how many keys a set may hold.
	maxKeys = 100
	// fetchTimeout is how long a fetch of keys or of the provider's
	// configuration may take.
	fetchTimeout = 10 * time.Second
)

// Client is an HTTP client for the identity provider: a time limit, and no
// redirects, so nothing is fetched from anywhere but the URL given.
func Client(rt http.RoundTripper) *http.Client {
	return &http.Client{Transport: rt, Timeout: fetchTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("redirects aren't followed")
	}}
}

// Keys are the public keys at a JWKS URL, fetched when first needed and
// again when a token names a key not held, at most once a minute.
type Keys struct {
	url    string
	client *http.Client
	algs   []jose.SignatureAlgorithm
	now    func() time.Time

	mu    sync.Mutex
	keys  []jose.JSONWebKey
	at    time.Time // when keys were fetched
	tried time.Time // when they were last asked for
	// fetching is held while they are fetched: one fetch at a time.
	fetching sync.Mutex
}

// NewKeys are the keys at a JWKS URL, for tokens signed with one of algs
// (Asymmetric when none).
func NewKeys(jwksURL string, client *http.Client, algs []string, now func() time.Time) *Keys {
	k := &Keys{url: jwksURL, client: client, now: now}
	for _, a := range algs {
		if slices.Contains(Asymmetric, jose.SignatureAlgorithm(a)) {
			k.algs = append(k.algs, jose.SignatureAlgorithm(a))
		}
	}
	if len(k.algs) == 0 {
		k.algs = Asymmetric
	}
	if k.now == nil {
		k.now = time.Now
	}
	return k
}

// Algs are the algorithms taken, as names.
func (k *Keys) Algs() []string {
	out := make([]string, len(k.algs))
	for i, a := range k.algs {
		out[i] = string(a)
	}
	return out
}

// VerifySignature checks a compact JWS against the keys and gives its
// payload. It is go-oidc's KeySet.
func (k *Keys) VerifySignature(ctx context.Context, raw string) ([]byte, error) {
	jws, err := jose.ParseSigned(raw, k.algs)
	if err != nil {
		return nil, fmt.Errorf("not a token signed with %v: %v", k.algs, err)
	}
	if len(jws.Signatures) != 1 {
		return nil, errors.New("want one signature")
	}
	kid := jws.Signatures[0].Header.KeyID
	keys, err := k.keysFor(ctx, kid)
	if err != nil {
		return nil, err
	}
	for i := range keys {
		if kid != "" && keys[i].KeyID != kid {
			continue
		}
		if p, err := jws.Verify(&keys[i]); err == nil {
			return p, nil
		}
	}
	return nil, errors.New("no key of the identity provider's verifies the signature")
}

// held reports whether a key is among those held: any, for a token that
// names none.
func held(keys []jose.JSONWebKey, kid string) bool {
	if len(keys) == 0 {
		return false
	}
	return kid == "" || slices.ContainsFunc(keys, func(k jose.JSONWebKey) bool { return k.KeyID == kid })
}

// keysFor are the keys to try for a token naming kid: those held, fetched
// again first when kid isn't among them, or they are old, and the last
// fetch wasn't too lately.
func (k *Keys) keysFor(ctx context.Context, kid string) ([]jose.JSONWebKey, error) {
	now := k.now()
	k.mu.Lock()
	keys, at, tried := k.keys, k.at, k.tried
	k.mu.Unlock()
	if held(keys, kid) && now.Sub(at) < keysMaxAge {
		return keys, nil
	}
	retry := keysRetry
	if len(keys) == 0 {
		retry = keysFirstRetry
	}
	if !tried.IsZero() && now.Sub(tried) < retry {
		if len(keys) == 0 {
			return nil, errors.New("the identity provider's keys couldn't be fetched: try again in a minute")
		}
		return keys, nil
	}
	k.fetching.Lock()
	defer k.fetching.Unlock()
	k.mu.Lock()
	if k.tried != tried { // fetched meanwhile, by another request
		keys = k.keys
		k.mu.Unlock()
		return keys, nil
	}
	k.tried = now
	k.mu.Unlock()
	// Not the request's context: a client that goes away doesn't cut short
	// a fetch others wait for.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
	defer cancel()
	fresh, err := k.fetch(fctx)
	if err != nil {
		if len(keys) > 0 {
			return keys, nil // keep using those held
		}
		return nil, fmt.Errorf("the identity provider's keys: %w", err)
	}
	k.mu.Lock()
	k.keys, k.at = fresh, now
	k.mu.Unlock()
	return fresh, nil
}

// fetch reads the keys at the URL: public keys of a key pair alone.
func (k *Keys) fetch(ctx context.Context) ([]jose.JSONWebKey, error) {
	b, err := get(ctx, k.client, k.url, keysBody)
	if err != nil {
		return nil, err
	}
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(b, &set); err != nil {
		return nil, fmt.Errorf("%s: not a JWKS: %v", k.url, err)
	}
	var out []jose.JSONWebKey
	for _, raw := range set.Keys {
		var key jose.JSONWebKey
		if json.Unmarshal(raw, &key) != nil || !key.Valid() || !key.IsPublic() || key.Use == "enc" {
			continue // a key of a kind not known, or not for signatures
		}
		switch key.Key.(type) {
		case *rsa.PublicKey, *ecdsa.PublicKey, ed25519.PublicKey:
		default:
			continue // a shared secret, never
		}
		out = append(out, key)
		if len(out) == maxKeys {
			break
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no public keys", k.url)
	}
	return out, nil
}

// get fetches a URL, reading at most limit bytes of a 200's body.
func get(ctx context.Context, c *http.Client, u string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", redact(u), resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s: more than %d bytes", redact(u), limit)
	}
	return b, nil
}

// redact is a URL without its query, for an error.
func redact(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return "the URL"
	}
	p.RawQuery, p.User = "", nil
	return p.String()
}

// SafeURL reports whether an identity provider's URL is one keys, tokens
// and secrets may go to: https, or http to this machine or to a name
// inside a cluster (no dots, or under .svc or .cluster.local), where no
// one between can change what it says.
func SafeURL(u string) bool {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" || p.User != nil {
		return false
	}
	switch p.Scheme {
	case "https":
		return true
	case "http":
		return Inside(p.Hostname())
	}
	return false
}

// Inside reports whether a host is this machine's, or a name inside a
// cluster.
func Inside(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	switch {
	case host == "":
		return false
	case host == "localhost", strings.HasSuffix(host, ".localhost"):
		return true
	case !strings.Contains(host, "."):
		return true
	}
	return strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".cluster.local")
}
