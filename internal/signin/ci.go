package signin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// CI is a CI provider's issuer of ID tokens, with which a job proves which
// repository it runs for (docs/design/backtests.md): GitHub Actions', a
// GitLab's, a Forgejo's, a Bitbucket workspace's. A token is checked as
// Bearer checks one, by the issuer's keys, found through its discovery
// document, never a URL a token names: its signature by an algorithm of a
// key pair (Asymmetric), its issuer to the byte, its audience, exp and nbf;
// and, besides, that it names one audience alone (For), or one beside the
// one its CI always adds (ForBeside: Bitbucket's), and was issued (iat)
// within the last hour, whatever its exp says. What the token says beyond
// that (its repository, its event) is for its caller to hold to what it
// trusts.
type CI struct {
	Issuer string
	// JWKS, when set, is where the keys are, in place of the URL the
	// discovery document names: for an issuer whose keys are elsewhere
	// than at its own origin.
	JWKS   string
	Client *http.Client
	Now    func() time.Time

	mu     sync.Mutex
	keys   *Keys
	tried  time.Time
	failed error
	// asking says the discovery document is being asked for: one at a
	// time, and no one waits for it.
	asking bool
}

const (
	// ciMaxAge is how old a CI's token may be, from its iat: a token
	// stolen from a job's log is worth that long at most.
	ciMaxAge = time.Hour
	// ciLeeway is how far in the future a token may say it was issued,
	// for the clocks of the CI and of lapilli: Bearer's.
	ciLeeway = 5 * time.Minute
	// ciDiscoverRetry is how soon discovery is tried again after it
	// failed: a token of an issuer trusted, sent by anyone, can't make
	// lapilli ask it more often than once a minute.
	ciDiscoverRetry = time.Minute
	// ciRSAMax is the most bits of an RSA key of a CI's taken; the fewest
	// are MinRSABits, as of every identity provider's.
	ciRSAMax = 4096
)

// CIToken is what a CI's ID token verified says: its claims, each as the
// JSON it was written as, and when it was issued and expires.
type CIToken struct {
	Claims   map[string]json.RawMessage
	Audience []string
	IssuedAt time.Time
	Expiry   time.Time
	// Payload is the claims as they were signed, decoded: one token
	// written in several ways (base64url's bits past the last byte, which
	// a decoder passes over) has one payload.
	Payload []byte
}

// ErrCIToken is a token that isn't one a CI's issuer gave for this
// audience, or no longer is: why is in what wraps it, for the log.
var ErrCIToken = errors.New("not an ID token taken here")

// Verify checks raw, a CI's ID token, for audience, and gives what it
// says. An error wraps ErrUnreachable or ErrMisconfigured when the issuer
// couldn't be asked or answered as it shouldn't, ErrCIToken otherwise.
func (c *CI) Verify(ctx context.Context, raw, audience string) (CIToken, error) {
	t, err := c.VerifyToken(ctx, raw)
	if err != nil {
		return t, err
	}
	if !t.For(audience) {
		return CIToken{}, fmt.Errorf("%w: its audience isn't the one given", ErrCIToken)
	}
	return t, nil
}

// For reports whether the token was made for audience alone.
func (t CIToken) For(audience string) bool {
	return len(t.Audience) == 1 && t.Audience[0] == audience
}

// ForBeside reports whether the token was made for audience alone, or for
// audience and beside, the one its CI adds to every token whatever the job
// asks (Bitbucket's default, its workspace's), and no other: never for a
// third service, nor for beside alone.
func (t CIToken) ForBeside(audience, beside string) bool {
	switch {
	case t.For(audience):
		return true
	case beside == "" || beside == audience || len(t.Audience) != 2:
		return false
	}
	return t.Audience[0] == audience && t.Audience[1] == beside || t.Audience[0] == beside && t.Audience[1] == audience
}

// VerifyToken checks raw as Verify does, but for its audience, which it
// gives for its caller to hold to its own (For, ForBeside): one check of
// the signature for every entry of the issuer's. A token naming more than
// two audiences is refused here: made for several services, any of which
// could send it on.
func (c *CI) VerifyToken(ctx context.Context, raw string) (CIToken, error) {
	keys, err := c.keysOf(ctx)
	if err != nil {
		return CIToken{}, err
	}
	v := oidc.NewVerifier(c.Issuer, keys, &oidc.Config{SkipClientIDCheck: true, SupportedSigningAlgs: keys.Algs(), Now: c.Now})
	t, err := v.Verify(ctx, raw)
	if err != nil {
		return CIToken{}, fmt.Errorf("%w: %v", ErrCIToken, err)
	}
	if len(t.Audience) == 0 || len(t.Audience) > 2 {
		// Made for several services, any of which could send it on; two
		// only as For and ForBeside take them.
		return CIToken{}, fmt.Errorf("%w: it names %d audiences, not one", ErrCIToken, len(t.Audience))
	}
	var payload json.RawMessage
	if err := t.Claims(&payload); err != nil {
		return CIToken{}, fmt.Errorf("%w: its claims: %v", ErrCIToken, err)
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(payload, &claims); err != nil {
		return CIToken{}, fmt.Errorf("%w: its claims: %v", ErrCIToken, err)
	}
	iat, ok := ClaimString(claims["iat"])
	var at time.Time
	if ok {
		var n int64
		if _, err := fmt.Sscan(iat, &n); err == nil {
			at = time.Unix(n, 0)
		}
	}
	now := c.now()
	switch {
	case at.IsZero():
		return CIToken{}, fmt.Errorf("%w: it says no time it was issued (iat)", ErrCIToken)
	case at.After(now.Add(ciLeeway)):
		return CIToken{}, fmt.Errorf("%w: it was issued in the future", ErrCIToken)
	case now.Sub(at) > ciMaxAge:
		return CIToken{}, fmt.Errorf("%w: it was issued more than an hour ago", ErrCIToken)
	}
	return CIToken{Claims: claims, Audience: t.Audience, IssuedAt: at, Expiry: t.Expiry, Payload: payload}, nil
}

// Discover asks the issuer for its discovery document now, as at start.
func (c *CI) Discover(ctx context.Context) error {
	_, err := c.keysOf(ctx)
	return err
}

// DiscoverKeys reads the issuer's discovery document once, as a token's
// first check does (discover), within ctx, and gives its keys, not yet
// fetched, keeping nothing. It is lapilli preflight's alone, which asks
// each issuer once, in its own time: it skips what keysOf holds a request
// from anyone to, discovery asked once a minute at most after a failure,
// and one ask at a time with none waiting on it. serve's paths, which a
// token from anyone leads to, call Discover and VerifyToken, never this
// (TestDiscoverKeysPreflightAlone reads the source).
func (c *CI) DiscoverKeys(ctx context.Context) (*Keys, error) {
	return c.discover(ctx)
}

func (c *CI) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// keysOf are the issuer's keys, as its discovery document names them:
// asked when first needed, and after a failure once a minute at most, the
// failure given meanwhile.
func (c *CI) keysOf(ctx context.Context) (*Keys, error) {
	c.mu.Lock()
	switch {
	case c.keys != nil:
		k := c.keys
		c.mu.Unlock()
		return k, nil
	case c.asking:
		// Asked by another request: none waits on it, the lock not held
		// across the fetch.
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: its discovery document is being asked for: try again in a moment", ErrUnreachable)
	case !c.tried.IsZero() && c.now().Sub(c.tried) < ciDiscoverRetry:
		err := c.failed
		c.mu.Unlock()
		return nil, err
	}
	c.tried, c.asking = c.now(), true
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
	defer cancel()
	k, err := c.discover(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asking = false
	if err != nil {
		c.failed = err
		return nil, err
	}
	c.keys = k
	return k, nil
}

// discover reads the issuer's discovery document: its issuer must be the
// one given, to the byte, and its keys at the issuer's own origin, unless
// JWKS says where they are.
func (c *CI) discover(ctx context.Context) (*Keys, error) {
	where := strings.TrimSuffix(c.Issuer, "/") + "/.well-known/openid-configuration"
	b, err := get(ctx, c.Client, where, keysBody)
	var status statusError
	switch {
	case errors.As(err, &status) && status.code < 500:
		return nil, fmt.Errorf("%w: %v", ErrMisconfigured, err)
	case err != nil:
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	var doc struct {
		Issuer string   `json:"issuer"`
		JWKS   string   `json:"jwks_uri"`
		Algs   []string `json:"id_token_signing_alg_values_supported"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%w: %s isn't an issuer's discovery document: %v", ErrMisconfigured, redact(where), err)
	}
	if doc.Issuer != c.Issuer {
		return nil, &discoveryError{kind: ErrOtherIssuer,
			err: fmt.Errorf("%w: the issuer is %q, and its discovery document says %q: give exactly that", ErrMisconfigured, c.Issuer, doc.Issuer)}
	}
	jwks := c.JWKS
	if jwks == "" {
		if !SafeURL(doc.JWKS) || !SameOrigin(doc.JWKS, c.Issuer) {
			return nil, &discoveryError{kind: ErrKeysElsewhere,
				err: fmt.Errorf("%w: its discovery document names keys at %q, which isn't https at the issuer's own origin: give jwks_url", ErrMisconfigured, redact(doc.JWKS))}
		}
		jwks = doc.JWKS
	}
	k := NewKeys(jwks, c.Client, doc.Algs, c.Now)
	k.maxRSA = ciRSAMax
	return k, nil
}

// The two ways a discovery document answers otherwise than its issuer as
// given, each said in Lapilli's words alone where what the document names
// is not to be printed (lapilli preflight, of an issuer no public CI's):
// it names another issuer, or keys elsewhere than at the issuer's origin.
var (
	ErrOtherIssuer   = errors.New("the discovery document names another issuer than the one given, exactly")
	ErrKeysElsewhere = errors.New("its jwks_uri isn't https at the issuer's own origin: give jwks_url")
)

// discoveryError is a discovery document that names another issuer, or
// keys elsewhere (kind), its message serve's, which quotes what it names.
type discoveryError struct {
	kind, err error
}

func (e *discoveryError) Error() string   { return e.err.Error() }
func (e *discoveryError) Unwrap() []error { return []error{e.kind, e.err} }

// SameOrigin reports whether two URLs have the same scheme, host and port.
func SameOrigin(a, b string) bool {
	x, err := url.Parse(a)
	if err != nil {
		return false
	}
	y, err := url.Parse(b)
	if err != nil {
		return false
	}
	return x.Scheme != "" && strings.EqualFold(x.Scheme, y.Scheme) && x.Host != "" && strings.EqualFold(x.Host, y.Host)
}

// integer is a JSON number written as an integer, as CIs write ids.
var integer = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

// ClaimString is a claim's value as a pin is held to it: a string as it
// is; a number written as an integer as its decimal, as written; anything
// else (a number with a point or an exponent, true, null, a list, an
// object) none, so that no pin matches it.
func ClaimString(raw json.RawMessage) (string, bool) {
	t := strings.TrimSpace(string(raw))
	switch {
	case t == "":
		return "", false
	case t[0] == '"':
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", false
		}
		return s, true
	case integer.MatchString(t):
		return t, true
	}
	return "", false
}
