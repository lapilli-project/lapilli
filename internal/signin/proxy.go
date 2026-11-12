package signin

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// The kinds of signed header a sign-in in front can send.
const (
	// GoogleIAP is Google Cloud's Identity-Aware Proxy:
	// x-goog-iap-jwt-assertion, ES256, for the backend service's audience.
	GoogleIAP = "google-iap"
	// AWSALB is an AWS Application Load Balancer's sign-in:
	// x-amzn-oidc-data, ES256, signed by the load balancer named.
	AWSALB = "aws-alb"
	// JWKS is any other: a header holding a JWT signed with the keys at a
	// JWKS URL, by an issuer, for an audience (oauth2-proxy's, an Azure App
	// Service's or an access proxy's).
	JWKS = "jwks"
)

// ProxyKinds are the kinds, in the order they are offered.
var ProxyKinds = []string{GoogleIAP, AWSALB, JWKS}

// Proxy is a sign-in in front of lapilli whose header, a JWT it signed,
// says who each request comes from.
type Proxy struct {
	Kind     string
	Header   string
	Audience string // Google IAP's and JWKS's
	ALBArn   string // the AWS load balancer that signs
	Issuer   string // JWKS's, and optionally the AWS load balancer's provider
	JWKS     string
	Claims   Claims
	Client   *http.Client
	Now      func() time.Time

	// KeysURL is where IAP's keys are: set by NewProxy, by tests to a
	// stand-in. A load balancer's are where its ARN says, alone.
	KeysURL string

	verifier *oidc.IDTokenVerifier
	alb      *albKeys
}

// iapIssuer and iapKeys are Google's, for its Identity-Aware Proxy.
const (
	iapIssuer = "https://cloud.google.com/iap"
	iapKeys   = "https://www.gstatic.com/iap/verify/public_key-jwk"
)

// albARN is an Application Load Balancer's ARN: partition, region.
var albARN = regexp.MustCompile(`^arn:(aws|aws-cn|aws-us-gov):elasticloadbalancing:([a-z]{2}(?:-[a-z]+)+-\d):\d{12}:loadbalancer/app/[A-Za-z0-9-]{1,32}/[0-9a-f]{16}$`)

// CheckProxy says what's wrong with a proxy's settings, if anything.
func CheckProxy(p Proxy) error {
	switch p.Kind {
	case GoogleIAP:
		if !strings.HasPrefix(p.Audience, "/projects/") {
			return errors.New("--proxy-jwt google-iap needs --proxy-jwt-audience, the backend service's, like /projects/123456789012/global/backendServices/1234567890123456789")
		}
	case AWSALB:
		if !albARN.MatchString(p.ALBArn) {
			return errors.New("--proxy-jwt aws-alb needs --proxy-jwt-alb-arn, the load balancer's ARN, like arn:aws:elasticloadbalancing:eu-west-1:123456789012:loadbalancer/app/lapilli/0123456789abcdef")
		}
	case JWKS:
		switch {
		case p.Header == "":
			return errors.New("--proxy-jwt jwks needs --proxy-jwt-header, the header the JWT is in")
		case !SafeURL(p.JWKS):
			return errors.New("--proxy-jwt jwks needs --proxy-jwt-jwks, the https URL of the keys it is signed with")
		case p.Issuer == "":
			return errors.New("--proxy-jwt jwks needs --proxy-jwt-issuer, the iss its JWTs have")
		case p.Audience == "":
			return errors.New("--proxy-jwt jwks needs --proxy-jwt-audience, the aud its JWTs have")
		}
	default:
		return fmt.Errorf("--proxy-jwt %s: want %s", p.Kind, strings.Join(ProxyKinds, ", "))
	}
	return nil
}

// NewProxy readies a proxy's verifier.
func NewProxy(p Proxy) (*Proxy, error) {
	if err := CheckProxy(p); err != nil {
		return nil, err
	}
	if p.Now == nil {
		p.Now = time.Now
	}
	switch p.Kind {
	case GoogleIAP:
		p.Header, p.Issuer = "X-Goog-Iap-Jwt-Assertion", iapIssuer
		if p.KeysURL == "" {
			p.KeysURL = iapKeys
		}
		keys := NewKeys(p.KeysURL, p.Client, []string{"ES256"}, p.Now)
		p.verifier = oidc.NewVerifier(p.Issuer, keys, &oidc.Config{ClientID: p.Audience, SupportedSigningAlgs: keys.Algs(), Now: p.Now})
	case JWKS:
		keys := NewKeys(p.JWKS, p.Client, nil, p.Now)
		p.verifier = oidc.NewVerifier(p.Issuer, keys, &oidc.Config{ClientID: p.Audience, SupportedSigningAlgs: keys.Algs(), Now: p.Now})
	case AWSALB:
		p.Header = "X-Amzn-Oidc-Data"
		m := albARN.FindStringSubmatch(p.ALBArn)
		p.alb = &albKeys{partition: m[1], region: m[2], client: p.Client, now: p.Now,
			keys: map[string]*ecdsa.PublicKey{}, failed: map[string]time.Time{}, tokens: albBurst}
	}
	p.Header = http.CanonicalHeaderKey(p.Header)
	return &p, nil
}

// ErrNoHeader is a request without the signed header: one that didn't come
// through the sign-in in front.
var ErrNoHeader = errors.New("no signed identity from the sign-in in front")

// Identify is who a request comes from, as its signed header says.
func (p *Proxy) Identify(r *http.Request) (Identity, error) {
	vs := r.Header.Values(p.Header)
	switch {
	case len(vs) == 0:
		return Identity{}, ErrNoHeader
	case len(vs) > 1:
		return Identity{}, fmt.Errorf("%s twice: one may be the caller's", p.Header)
	}
	raw := strings.TrimSpace(vs[0])
	if p.Header == "Authorization" {
		var ok bool
		if raw, ok = cutBearer(raw); !ok {
			return Identity{}, ErrNoHeader
		}
	}
	var claims map[string]any
	var err error
	if p.Kind == AWSALB {
		claims, err = p.albClaims(r.Context(), raw)
	} else {
		claims, err = p.jwtClaims(r.Context(), raw)
	}
	if err != nil {
		return Identity{}, err
	}
	return p.Claims.Identity(claims)
}

// cutBearer is a bearer token from an Authorization header.
func cutBearer(h string) (string, bool) {
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(tok) == "" {
		return "", false
	}
	return strings.TrimSpace(tok), true
}

// CutBearer is a bearer token from an Authorization header.
func CutBearer(h string) (string, bool) { return cutBearer(h) }

// jwtClaims checks a JWT: signature, issuer, audience, expiry, and that it
// wasn't issued in the future.
func (p *Proxy) jwtClaims(ctx context.Context, raw string) (map[string]any, error) {
	t, err := p.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, err
	}
	if !t.IssuedAt.IsZero() && t.IssuedAt.After(p.Now().Add(time.Minute)) {
		return nil, errors.New("issued in the future")
	}
	var claims map[string]any
	if err := t.Claims(&claims); err != nil {
		return nil, err
	}
	return claims, nil
}

// albHeader is the header of an AWS load balancer's JWT, which says who
// signed it and until when.
type albHeader struct {
	Alg    string `json:"alg"`
	Kid    string `json:"kid"`
	Signer string `json:"signer"`
	Iss    string `json:"iss"`
	Client string `json:"client"`
	Exp    int64  `json:"exp"`
}

// albKid is the form of a load balancer's key id, which becomes part of the
// URL its key is fetched from.
var albKid = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)

// segment decodes a part of a JWT. A load balancer pads its parts with =,
// which JWTs otherwise don't; the signature covers them as sent.
func segment(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// albClaims checks an AWS load balancer's JWT: ES256, signed by the load
// balancer named, not expired, signed with its key; and gives the user's
// claims it carries. It is checked by hand: the load balancer pads the
// parts, which JWT libraries refuse, and the signature is over them as
// sent.
func (p *Proxy) albClaims(ctx context.Context, raw string) (map[string]any, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("not a JWT")
	}
	hb, err := segment(parts[0])
	if err != nil {
		return nil, errors.New("not a JWT")
	}
	var h albHeader
	if err := json.Unmarshal(hb, &h); err != nil {
		return nil, errors.New("not a JWT")
	}
	now := p.Now().Unix()
	switch {
	case h.Alg != "ES256":
		return nil, fmt.Errorf("signed with %q, not ES256", h.Alg)
	case h.Signer != p.ALBArn:
		return nil, errors.New("signed by another load balancer")
	case p.Issuer != "" && h.Iss != p.Issuer:
		return nil, fmt.Errorf("from the identity provider %q, not %q", h.Iss, p.Issuer)
	case h.Exp == 0 || h.Exp <= now:
		return nil, errors.New("expired")
	case !albKid.MatchString(h.Kid):
		return nil, errors.New("a key id that isn't one")
	}
	sig, err := segment(parts[2])
	if err != nil || len(sig) != 64 {
		return nil, errors.New("a signature that isn't ES256's")
	}
	key, err := p.alb.key(ctx, h.Kid)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(key, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return nil, errors.New("the load balancer's key doesn't verify the signature")
	}
	pb, err := segment(parts[1])
	if err != nil {
		return nil, errors.New("not a JWT")
	}
	var claims map[string]any
	if err := json.Unmarshal(pb, &claims); err != nil {
		return nil, errors.New("not a JWT")
	}
	if exp, ok := claims["exp"].(float64); ok && int64(exp) <= now {
		return nil, errors.New("expired")
	}
	return claims, nil
}

// albKeyURL is where a load balancer's public key is, by its partition,
// its region and the key's id, from AWS's fixed hosts alone: nothing a
// token says can steer it elsewhere. ok is false for what isn't one.
func albKeyURL(partition, region, kid string) (u string, ok bool) {
	if !albKid.MatchString(kid) || !albRegion.MatchString(region) {
		return "", false
	}
	switch partition {
	case "aws":
		return "https://public-keys.auth.elb." + region + ".amazonaws.com/" + kid, true
	case "aws-cn":
		return "https://public-keys.auth.elb." + region + ".amazonaws.com.cn/" + kid, true
	case "aws-us-gov":
		return "https://s3-" + region + ".amazonaws.com/aws-elb-public-keys-prod-" + region + "/" + kid, true
	}
	return "", false
}

// albRegion is an AWS region's name.
var albRegion = regexp.MustCompile(`^[a-z]{2}(?:-[a-z]+)+-\d$`)

// albKeys are a load balancer's public keys, by id, each fetched once.
// Keys not held are fetched from a budget of their own, a few at once and
// one each ten seconds after; a key the load balancer doesn't have is
// remembered for an hour, and costs nothing again. Someone sending tokens
// with keys made up, slower than the budget fills, can't keep the load
// balancer's own new key from being fetched.
type albKeys struct {
	partition, region string
	client            *http.Client
	now               func() time.Time

	mu     sync.Mutex
	keys   map[string]*ecdsa.PublicKey
	order  []string
	failed map[string]time.Time
	tokens float64
	last   time.Time
}

const (
	// albBurst keys not held may be fetched at once, and one more each
	// albEvery.
	albBurst = 6
	albEvery = 10 * time.Second
	// albKept is how many keys are kept, and albFailedKept how many ids
	// that weren't keys, each for albFailedFor.
	albKept       = 64
	albFailedKept = 4096
	albFailedFor  = time.Hour
)

// key is the public key a load balancer signed with.
func (a *albKeys) key(ctx context.Context, kid string) (*ecdsa.PublicKey, error) {
	u, ok := albKeyURL(a.partition, a.region, kid)
	if !ok {
		return nil, errors.New("a key id that isn't one")
	}
	now := a.now()
	a.mu.Lock()
	if k, ok := a.keys[kid]; ok {
		a.mu.Unlock()
		return k, nil
	}
	if at, ok := a.failed[kid]; ok && now.Sub(at) < albFailedFor {
		a.mu.Unlock()
		return nil, errors.New("a key id the load balancer doesn't have")
	}
	if !a.last.IsZero() {
		a.tokens = min(albBurst, a.tokens+now.Sub(a.last).Seconds()/albEvery.Seconds())
	}
	a.last = now
	if a.tokens < 1 {
		a.mu.Unlock()
		return nil, errors.New("keys not held, fetched too often lately: try again in a few seconds")
	}
	a.tokens--
	a.mu.Unlock()
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
	defer cancel()
	k, err := a.fetch(fctx, u)
	a.mu.Lock()
	defer a.mu.Unlock()
	var status statusError
	switch {
	case errors.As(err, &status) && status.code < 500, errors.Is(err, errNotAKey):
		// Not one of the load balancer's: remembered, so it costs nothing
		// again. One that couldn't be fetched (a timeout, the host's
		// error) is tried again.
		for id, at := range a.failed {
			if now.Sub(at) >= albFailedFor {
				delete(a.failed, id)
			}
		}
		if len(a.failed) < albFailedKept {
			a.failed[kid] = now
		}
		return nil, fmt.Errorf("the load balancer's key: %w", err)
	case err != nil:
		return nil, fmt.Errorf("the load balancer's key: %w", err)
	}
	if held, ok := a.keys[kid]; ok {
		return held, nil
	}
	if len(a.order) == albKept {
		delete(a.keys, a.order[0])
		a.order = a.order[1:]
	}
	a.keys[kid], a.order = k, append(a.order, kid)
	return k, nil
}

// errNotAKey is an answer that isn't a P-256 public key.
var errNotAKey = errors.New("not a P-256 public key in PEM")

// fetch reads a key.
func (a *albKeys) fetch(ctx context.Context, u string) (*ecdsa.PublicKey, error) {
	b, err := get(ctx, a.client, u, 16<<10)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errNotAKey
	}
	pub, err := x509.ParsePKIXPublicKey(blk.Bytes)
	k, ok := pub.(*ecdsa.PublicKey)
	if err != nil || !ok || k.Curve != elliptic.P256() {
		return nil, errNotAKey
	}
	return k, nil
}
