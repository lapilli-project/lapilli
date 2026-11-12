package signin

import (
	"context"
	"crypto/subtle"
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
	Client   *http.Client
	Now      func() time.Time

	mu    sync.Mutex
	p     *provider
	tried time.Time
}

// provider is what discovery found.
type provider struct {
	endpoint oauth2.Endpoint
	verifier *oidc.IDTokenVerifier
}

// discoverRetry is how soon discovery is tried again after it failed.
const discoverRetry = 10 * time.Second

// ErrUnreachable is an identity provider that couldn't be asked.
var ErrUnreachable = errors.New("the identity provider can't be reached")

// provider is the provider as discovery found it, discovered when first
// needed and, after a failure, at most once every ten seconds: a request
// from anyone can lead here.
func (o *OIDC) provider(ctx context.Context) (*provider, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.p != nil {
		return o.p, nil
	}
	if !o.tried.IsZero() && o.now().Sub(o.tried) < discoverRetry {
		return nil, ErrUnreachable
	}
	o.tried = o.now()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
	defer cancel()
	p, err := oidc.NewProvider(oidc.ClientContext(ctx, o.Client), o.Issuer)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	var doc struct {
		JWKS string   `json:"jwks_uri"`
		Algs []string `json:"id_token_signing_alg_values_supported"`
	}
	if err := p.Claims(&doc); err != nil {
		return nil, fmt.Errorf("%w: its configuration: %v", ErrUnreachable, err)
	}
	ep := p.Endpoint()
	// What the configuration names gets the code, the secret and the
	// keys: as safe to reach as the issuer.
	for _, u := range []string{doc.JWKS, ep.AuthURL, ep.TokenURL} {
		if !SafeURL(u) {
			return nil, fmt.Errorf("%w: its configuration names %q, which isn't https", ErrUnreachable, redact(u))
		}
	}
	keys := NewKeys(doc.JWKS, o.Client, doc.Algs, o.Now)
	o.p = &provider{endpoint: ep, verifier: oidc.NewVerifier(o.Issuer, keys,
		&oidc.Config{ClientID: o.ClientID, SupportedSigningAlgs: keys.Algs(), Now: o.Now})}
	return o.p, nil
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
	return c.AuthCodeURL(st.State, oidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier)), nil
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
	return o.Claims.Identity(claims)
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
	if len(t.Audience) > 1 {
		if azp, _ := claims["azp"].(string); azp != o.ClientID {
			return nil, "", fmt.Errorf("the ID token was given to %q, not this client", azp)
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
