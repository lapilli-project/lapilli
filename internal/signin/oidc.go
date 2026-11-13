package signin

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDC is an OpenID Connect provider lapilli signs people in with: the
// authorization code flow, with PKCE, state and nonce, as a confidential
// client when it has a secret and a public one when it hasn't.
type OIDC struct {
	Issuer   string
	ClientID string
	// SecretFile holds the client's secret, read for each sign-in so it can
	// be rotated; "" for a public client.
	SecretFile string
	// Redirect is the URL the provider sends people back to:
	// --external-url's /auth/callback.
	Redirect string
	Scopes   []string
	Claims   Claims
	// HostedDomains are the Google Workspace domains (the hd claim) people
	// may come from; none for any. TrustedClients are the other clients of
	// the provider whose tokens (azp) are taken as this one's.
	HostedDomains, TrustedClients []string
	// NoBearer refuses ID tokens sent as bearer tokens: only sessions.
	NoBearer bool
	Client   *http.Client
	Now      func() time.Time

	mu     sync.Mutex
	p      *provider
	tried  time.Time
	failed error
}

// provider is what discovery found.
type provider struct {
	endpoint oauth2.Endpoint
	verifier *oidc.IDTokenVerifier
}

// discoverRetry is how soon discovery is tried again after it failed.
const discoverRetry = 10 * time.Second

// ErrUnreachable is an identity provider that couldn't be asked; and
// ErrMisconfigured one whose answer won't do, as set up.
var (
	ErrUnreachable   = errors.New("the identity provider can't be reached")
	ErrMisconfigured = errors.New("the identity provider isn't set up as lapilli is")
)

// provider is the provider as discovery found it, discovered when first
// needed and, after a failure, at most once every ten seconds: a request
// from anyone can lead here. The last failure is given again meanwhile, so
// a setting that is wrong is said as one, not as an outage.
func (o *OIDC) provider(ctx context.Context) (*provider, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.p != nil {
		return o.p, nil
	}
	if !o.tried.IsZero() && o.now().Sub(o.tried) < discoverRetry {
		return nil, o.failed
	}
	o.tried = o.now()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
	defer cancel()
	p, err := o.discover(ctx)
	if err != nil {
		o.failed = err
		return nil, err
	}
	o.p = p
	return p, nil
}

// Discover asks the provider for its configuration now, as at start: nil,
// ErrUnreachable (it couldn't be asked, or answered with a server's error)
// or ErrMisconfigured (it answered, and the answer won't do).
func (o *OIDC) Discover(ctx context.Context) error {
	_, err := o.provider(ctx)
	return err
}

// discover reads the provider's configuration: its issuer must be the one
// given, to the byte, and what it names gets the code, the secret and the
// keys, so must be as safe to reach as the issuer.
func (o *OIDC) discover(ctx context.Context) (*provider, error) {
	where := strings.TrimSuffix(o.Issuer, "/") + "/.well-known/openid-configuration"
	b, err := get(ctx, o.Client, where, keysBody)
	var status statusError
	switch {
	case errors.As(err, &status) && status.code < 500:
		return nil, fmt.Errorf("%w: %v", ErrMisconfigured, err)
	case err != nil:
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	var doc struct {
		Issuer string   `json:"issuer"`
		Auth   string   `json:"authorization_endpoint"`
		Token  string   `json:"token_endpoint"`
		JWKS   string   `json:"jwks_uri"`
		Algs   []string `json:"id_token_signing_alg_values_supported"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%w: %s isn't a provider's configuration: %v", ErrMisconfigured, where, err)
	}
	if doc.Issuer != o.Issuer {
		return nil, fmt.Errorf("%w: --oidc-issuer is %q, and the provider says its issuer is %q: give exactly that", ErrMisconfigured, o.Issuer, doc.Issuer)
	}
	for _, u := range []string{doc.JWKS, doc.Auth, doc.Token} {
		if !SafeURL(u) {
			return nil, fmt.Errorf("%w: its configuration names %q, which isn't https", ErrMisconfigured, redact(u))
		}
	}
	keys := NewKeys(doc.JWKS, o.Client, doc.Algs, o.Now)
	return &provider{endpoint: oauth2.Endpoint{AuthURL: doc.Auth, TokenURL: doc.Token}, verifier: oidc.NewVerifier(o.Issuer, keys,
		&oidc.Config{ClientID: o.ClientID, SupportedSigningAlgs: keys.Algs(), Now: o.Now})}, nil
}

func (o *OIDC) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// config is the OAuth 2.0 client, with its secret as the file holds it
// now.
func (o *OIDC) config(p *provider) (*oauth2.Config, error) {
	c := &oauth2.Config{ClientID: o.ClientID, Endpoint: p.endpoint, RedirectURL: o.Redirect, Scopes: o.Scopes}
	if o.SecretFile != "" {
		b, err := os.ReadFile(o.SecretFile)
		if err != nil {
			return nil, fmt.Errorf("--oidc-client-secret-file: %v", err)
		}
		c.ClientSecret = strings.TrimSpace(string(b))
	}
	return c, nil
}

// Start is a sign-in begun: what the browser keeps until it comes back.
type Start struct {
	State, Nonce, Verifier string
}

// NewStart makes the secrets of a sign-in.
func NewStart() Start {
	return Start{State: random(32), Nonce: random(32), Verifier: oauth2.GenerateVerifier()}
}

// AuthURL is where to send the browser to sign in.
func (o *OIDC) AuthURL(ctx context.Context, st Start) (string, error) {
	p, err := o.provider(ctx)
	if err != nil {
		return "", err
	}
	c, err := o.config(p)
	if err != nil {
		return "", err
	}
	opts := []oauth2.AuthCodeOption{oidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier)}
	if len(o.HostedDomains) == 1 {
		// Google's hint: its sign-in offers only that domain's accounts.
		opts = append(opts, oauth2.SetAuthURLParam("hd", o.HostedDomains[0]))
	}
	return c.AuthCodeURL(st.State, opts...), nil
}

// Finish takes the code the provider sent back for a sign-in begun with
// st, and gives who signed in: the code is exchanged, with the verifier,
// for an ID token, whose signature, issuer, audience, expiry and nonce are
// checked.
func (o *OIDC) Finish(ctx context.Context, st Start, code string) (Identity, error) {
	p, err := o.provider(ctx)
	if err != nil {
		return Identity{}, err
	}
	c, err := o.config(p)
	if err != nil {
		return Identity{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	tok, err := c.Exchange(context.WithValue(ctx, oauth2.HTTPClient, o.Client), code, oauth2.VerifierOption(st.Verifier))
	if err != nil {
		return Identity{}, fmt.Errorf("the identity provider didn't take the code: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return Identity{}, errors.New("the identity provider sent no ID token: is openid among the scopes?")
	}
	claims, nonce, err := o.verify(ctx, p, raw)
	if err != nil {
		return Identity{}, err
	}
	if subtle.ConstantTimeCompare([]byte(nonce), []byte(st.Nonce)) != 1 {
		return Identity{}, errors.New("the ID token isn't this sign-in's (its nonce differs)")
	}
	return o.Claims.Identity(claims)
}

// Bearer gives who an ID token, sent as a bearer token by an API client or
// an agent, says signed in: the same checks but the nonce, which only the
// sign-in that asked for it knows.
func (o *OIDC) Bearer(ctx context.Context, raw string) (Identity, error) {
	p, err := o.provider(ctx)
	if err != nil {
		return Identity{}, err
	}
	claims, _, err := o.verify(ctx, p, raw)
	if err != nil {
		return Identity{}, err
	}
	id, err := o.Claims.Identity(claims)
	if err == nil && id.IssuedAt.IsZero() {
		return Identity{}, fmt.Errorf("%w: the ID token says no time it was issued (iat)", ErrNoName)
	}
	return id, err
}

// verify checks an ID token, and gives its claims and its nonce. A token
// for several audiences must have been given to this client (azp): one
// given to another, that names this one too, isn't this one's sign-in.
func (o *OIDC) verify(ctx context.Context, p *provider, raw string) (map[string]any, string, error) {
	t, err := p.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, "", fmt.Errorf("the ID token: %w", err)
	}
	var claims map[string]any
	if err := t.Claims(&claims); err != nil {
		return nil, "", fmt.Errorf("the ID token's claims: %w", err)
	}
	// Given to another client (azp), the token is that client's sign-in,
	// even naming this one too: refused, unless that client is trusted.
	// One naming several audiences must say whose it is.
	azp, _ := claims["azp"].(string)
	switch {
	case len(t.Audience) > 1 && azp == "":
		return nil, "", errors.New("the ID token names several audiences and no azp")
	case azp != "" && azp != o.ClientID && !slices.Contains(o.TrustedClients, azp):
		return nil, "", fmt.Errorf("the ID token was given to %q, not this client (--oidc-trusted-clients)", azp)
	}
	if len(o.HostedDomains) > 0 {
		hd, _ := claims["hd"].(string)
		if !slices.ContainsFunc(o.HostedDomains, func(d string) bool { return EqualFoldASCII(d, hd) }) {
			return nil, "", fmt.Errorf("the account is in the domain %q, not one of --oidc-hosted-domain", hd)
		}
	}
	if iat := t.IssuedAt; !iat.IsZero() && iat.After(o.now().Add(5*time.Minute)) {
		return nil, "", errors.New("the ID token was issued in the future")
	}
	return claims, t.Nonce, nil
}

// WithOpenID gives scopes with openid first, once.
func WithOpenID(scopes []string) []string {
	out := []string{oidc.ScopeOpenID}
	for _, s := range scopes {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}
