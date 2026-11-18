package preflight

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"lapilli.dev/internal/serve"
)

// The examples in these tests are made up: a repository 123456789, a
// project 42, an audience at lapilli-ci.example.com.

// issuerAt is a stand-in for a CI's issuer of ID tokens: its discovery
// document at /.well-known/openid-configuration and its keys at
// /.well-known/jwks, each counted as it is asked.
type issuerAt struct {
	*httptest.Server
	mu    sync.Mutex
	asked map[string]int
	// doc, when set, changes the discovery document before it is sent;
	// keys is the JWKS sent; serve, when set, answers in their place.
	doc   func(map[string]any)
	keys  []byte
	serve func(w http.ResponseWriter, r *http.Request) bool
}

func newIssuerAt(t *testing.T, keys []byte) *issuerAt {
	t.Helper()
	x := &issuerAt{asked: map[string]int{}, keys: keys}
	x.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x.mu.Lock()
		x.asked[r.Method+" "+r.URL.RequestURI()]++
		x.mu.Unlock()
		if x.serve != nil && x.serve(w, r) {
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/.well-known/openid-configuration":
			doc := map[string]any{"issuer": x.URL, "jwks_uri": x.URL + "/.well-known/jwks", "id_token_signing_alg_values_supported": []string{"RS256"}}
			if x.doc != nil {
				x.doc(doc)
			}
			json.NewEncoder(w).Encode(doc)
		case r.Method == http.MethodGet && r.URL.Path == "/.well-known/jwks":
			w.Write(x.keys)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(x.Close)
	return x
}

func (x *issuerAt) count() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	n := 0
	for _, v := range x.asked {
		n += v
	}
	return n
}

// jwks is a key set of these public keys, each with a kid of its own.
func jwks(t *testing.T, keys ...any) []byte {
	t.Helper()
	var set jose.JSONWebKeySet
	for i, k := range keys {
		set.Keys = append(set.Keys, jose.JSONWebKey{Key: k, KeyID: fmt.Sprint("k", i), Use: "sig"})
	}
	b, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rsaOf is an RSA public key of bits, made up: a modulus of that many bits
// that no one holds the primes of, which a key set reads as any other.
func rsaOf(bits int) *rsa.PublicKey {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), uint(bits-1)))
	n.SetBit(n, bits-1, 1)
	n.SetBit(n, 0, 1)
	return &rsa.PublicKey{N: n, E: 65537}
}

func ecOf(t *testing.T, c elliptic.Curve) *ecdsa.PublicKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(c, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &k.PublicKey
}

// backtestConfig is a deployment taking pull request backtests from these
// entries.
func backtestConfig(trust ...serve.Trust) Config {
	c := config()
	c.Serve.BacktestListen = ":8084"
	c.Serve.RulesPath = "rules.yaml"
	c.Serve.Trust = trust
	c.Scrubber = byFlag
	return c
}

func ghEntry(name, issuer string) serve.Trust {
	return serve.Trust{Name: name, Issuer: issuer, Audience: "https://lapilli-ci.example.com", CI: "github",
		Claims: map[string]serve.Pin{"repository_id": {Values: []string{"123456789"}}, "event_name": {Values: []string{"pull_request"}}}}
}

// Each issuer is asked once for its discovery document and once for its
// keys, through the allowlist, each counted as backtest; ok when a key it
// holds verifies a token of an algorithm it names, a problem otherwise:
// each failure as signin.CI's first check would fail, nothing fetched that
// a token's check wouldn't fetch.
func TestBacktestIssuers(t *testing.T) {
	good := jwks(t, rsaOf(2048))
	elsewhere := newIssuerAt(t, good)
	for _, x := range []struct {
		name   string
		keys   []byte
		doc    func(*issuerAt) func(map[string]any)
		serve  func(w http.ResponseWriter, r *http.Request) bool
		jwks   bool // the entry's jwks_url is elsewhere's
		state  State
		says   string
		asked  int // of the issuer's
		usable int
	}{
		{name: "ok", keys: good, state: OK, says: "1 key for RS256", asked: 2},
		{name: "two algorithms, keys of each", keys: jwks(t, rsaOf(2048), ecOf(t, elliptic.P256()), ecOf(t, elliptic.P384())), state: OK, says: "2 keys for RS256, ES256", asked: 2,
			doc: func(*issuerAt) func(map[string]any) {
				return func(d map[string]any) { d["id_token_signing_alg_values_supported"] = []string{"RS256", "ES256"} }
			}},
		{name: "keys of another algorithm", keys: jwks(t, ecOf(t, elliptic.P256())), state: Problem, says: "no key its JWKS holds verifies a token of RS256", asked: 2},
		{name: "a curve not the algorithm's", keys: jwks(t, ecOf(t, elliptic.P384())), state: Problem, says: "verifies a token of ES256", asked: 2,
			doc: func(*issuerAt) func(map[string]any) {
				return func(d map[string]any) { d["id_token_signing_alg_values_supported"] = []string{"ES256"} }
			}},
		{name: "RSA under 2048 bits", keys: jwks(t, rsaOf(1024)), state: Problem, says: "RSA under 2048 bits", asked: 2},
		{name: "RSA over 4096 bits, a CI's", keys: jwks(t, rsaOf(8192)), state: Problem, says: "no public keys", asked: 2},
		{name: "keys past 1 MB", keys: []byte(`{"keys":[],"pad":"` + strings.Repeat("x", 1<<20) + `"}`), state: Problem, says: "more than 1048576 bytes", asked: 2},
		{name: "a discovery document past 1 MB", state: Problem, says: "more than 1048576 bytes", asked: 1,
			doc: func(*issuerAt) func(map[string]any) {
				return func(d map[string]any) { d["pad"] = strings.Repeat("x", 1<<20) }
			}},
		{name: "another issuer", state: Problem, says: "its discovery document says", asked: 1,
			doc: func(*issuerAt) func(map[string]any) {
				return func(d map[string]any) { d["issuer"] = "https://token.example.com" }
			}},
		{name: "keys at another origin", state: Problem, says: "which isn't https at the issuer's own origin: give jwks_url", asked: 1,
			doc: func(*issuerAt) func(map[string]any) {
				return func(d map[string]any) { d["jwks_uri"] = elsewhere.URL + "/.well-known/jwks" }
			}},
		{name: "keys at another origin, as jwks_url says", jwks: true, state: OK, says: "1 key for RS256", asked: 1,
			doc: func(*issuerAt) func(map[string]any) {
				return func(d map[string]any) { d["jwks_uri"] = elsewhere.URL + "/.well-known/jwks" }
			}},
		{name: "a redirect", state: Problem, says: "redirects aren't followed", asked: 1,
			serve: func(w http.ResponseWriter, r *http.Request) bool {
				http.Redirect(w, r, elsewhere.URL+"/.well-known/openid-configuration", http.StatusFound)
				return true
			}},
		{name: "not found", state: Problem, says: "404 Not Found", asked: 1,
			serve: func(w http.ResponseWriter, r *http.Request) bool { http.NotFound(w, r); return true }},
	} {
		t.Run(x.name, func(t *testing.T) {
			elsewhere.mu.Lock()
			clear(elsewhere.asked)
			elsewhere.mu.Unlock()
			is := newIssuerAt(t, x.keys)
			if x.doc != nil {
				is.doc = x.doc(is)
			}
			is.serve = x.serve
			e := ghEntry("alerts", is.URL)
			if x.jwks {
				e.JWKSURL = elsewhere.URL + "/.well-known/jwks"
			}
			p := guarded(t, backtestConfig(e))
			p.backtestIssuers(context.Background())
			it := judged(p, *itemOf(p, "backtest.issuers"))
			if it.State != x.state || !strings.Contains(it.What+" "+it.Why, x.says) {
				t.Errorf("%+v, want %s saying %q", it, x.state, x.says)
			}
			a := p.al.asked()
			if is.count() != x.asked || a.Refused != 0 || a.By["backtest"] != a.Requests {
				t.Errorf("the issuer was asked %v, %+v", is.asked, a)
			}
			if want := map[bool]int{true: 1}[x.jwks]; elsewhere.count() != want {
				t.Errorf("elsewhere was asked %v", elsewhere.asked)
			}
			if a.Requests != is.count()+elsewhere.count() {
				t.Errorf("%+v counted, %d sent", a, is.count()+elsewhere.count())
			}
		})
	}
}

// The keys a set holds are read a hundred at most, as a token's check
// reads them.
func TestBacktestIssuerKeysBounded(t *testing.T) {
	var many []any
	for range 150 {
		many = append(many, rsaOf(2048))
	}
	is := newIssuerAt(t, jwks(t, many...))
	p := guarded(t, backtestConfig(ghEntry("alerts", is.URL)))
	p.backtestIssuers(context.Background())
	if it := itemOf(p, "backtest.issuers"); it.State != OK || !strings.Contains(it.What, "100 keys for RS256") {
		t.Errorf("%+v", it)
	}
}

// The entries of one issuer are asked of it once, as serve's port asks
// it once for them all; and issuers past twenty aren't asked: unknown.
func TestBacktestIssuersOnceEach(t *testing.T) {
	is := newIssuerAt(t, jwks(t, rsaOf(2048)))
	var trust []serve.Trust
	for i := range 3 {
		trust = append(trust, ghEntry(fmt.Sprint("alerts-", i), is.URL))
	}
	p := guarded(t, backtestConfig(trust...))
	p.backtestIssuers(context.Background())
	if it := itemOf(p, "backtest.issuers"); it.State != OK || is.count() != 2 || !strings.Contains(it.What, "(alerts-0, alerts-1, alerts-2)") {
		t.Errorf("asked %v: %+v", is.asked, it)
	}
	trust = nil
	var all []*issuerAt
	for i := range mostIssuers + 2 {
		x := newIssuerAt(t, jwks(t, rsaOf(2048)))
		all = append(all, x)
		trust = append(trust, ghEntry(fmt.Sprint("e", i), x.URL))
	}
	p = guarded(t, backtestConfig(trust...))
	p.backtestIssuers(context.Background())
	it := itemOf(p, "backtest.issuers")
	if it.State != Unknown || !strings.Contains(it.Why, "past 20 issuers") || all[mostIssuers].count()+all[mostIssuers+1].count() != 0 ||
		all[mostIssuers-1].count() != 2 || p.al.asked().Refused != 0 {
		t.Errorf("%+v", it)
	}
}

// Every request is asked within the preflight's time: once it is up, an
// issuer not yet asked is unknown, and sent nothing.
func TestBacktestIssuersWithinTime(t *testing.T) {
	a, b := newIssuerAt(t, jwks(t, rsaOf(2048))), newIssuerAt(t, jwks(t, rsaOf(2048)))
	p := guarded(t, backtestConfig(ghEntry("a", a.URL), ghEntry("b", b.URL)))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.backtestIssuers(ctx)
	if it := itemOf(p, "backtest.issuers"); it.State != Unknown || a.count()+b.count() != 0 {
		t.Errorf("asked %d: %+v", a.count()+b.count(), it)
	}
	// A context done that the loop doesn't see is done (timeUp, the first
	// look): the discovery document, asked under it, is never sent.
	p = guarded(t, backtestConfig(ghEntry("a", a.URL)))
	p.backtestIssuers(&timeUp{Context: ctx, n: 1})
	if it := itemOf(p, "backtest.issuers"); it.State != Unknown || a.count() != 0 {
		t.Errorf("asked %v: %+v", a.asked, it)
	}
	// A context done while one is asked: what it sends is cut, under it,
	// and unknown, the preflight's time and not the issuer's answer.
	p = guarded(t, backtestConfig(ghEntry("a", a.URL)))
	ctx, cancel = context.WithCancel(context.Background())
	a.serve = func(w http.ResponseWriter, r *http.Request) bool { cancel(); return false }
	p.backtestIssuers(ctx)
	if it := itemOf(p, "backtest.issuers"); it.State != Unknown || a.count() != 1 {
		t.Errorf("asked %v: %+v", a.asked, it)
	}
}

// The allowlist holds each issuer's discovery document and the keys at a
// jwks_url given, to their exact host, path and no query, GETs alone; the
// keys a discovery document names are held to the issuer's own origin
// before they are allowed (keysAllowed).
func TestAllowIssuers(t *testing.T) {
	c := backtestConfig(ghEntry("alerts", "https://token.example.com/actions"), serve.Trust{Name: "plain", TokenFile: "/x"})
	c.Serve.Trust = append(c.Serve.Trust, ghEntry("keys", "https://gitlab.example.com"))
	c.Serve.Trust[2].JWKSURL = "https://keys.example.com/oauth/discovery/keys"
	p := newRun(c)
	p.allowAll()
	for _, x := range []struct {
		method, url string
		ok          bool
	}{
		{"GET", "https://token.example.com/actions/.well-known/openid-configuration", true},
		{"GET", "https://gitlab.example.com/.well-known/openid-configuration", true},
		{"GET", "https://keys.example.com/oauth/discovery/keys", true},
		{"GET", "https://token.example.com/actions/.well-known/openid-configuration?x=1", false},
		{"POST", "https://token.example.com/actions/.well-known/openid-configuration", false},
		{"GET", "https://token.example.com/actions/.well-known/openid-configuration/x", false},
		{"GET", "https://token.example.com/actions/.well-known/jwks", false}, // learned, not given
		{"GET", "https://token.example.com/.well-known/openid-configuration", false},
		{"GET", "http://token.example.com/actions/.well-known/openid-configuration", false},
		{"GET", "https://token.example.com.evil.example/actions/.well-known/openid-configuration", false},
		{"GET", "https://keys.example.com/oauth/discovery/keys/x", false},
		{"GET", "https://keys.example.com/", false},
	} {
		if err := p.al.check(req(t, x.method, x.url, "")); (err == nil) != x.ok {
			t.Errorf("%s %s: %v, want allowed %v", x.method, x.url, err, x.ok)
		}
	}
	if a := p.al.asked(); a.By["backtest"] != 3 {
		t.Errorf("%+v", a)
	}
	// With the trust file refused, nothing of it is allowed.
	c.TrustErr = fmt.Errorf("refused")
	p = newRun(c)
	p.allowAll()
	if err := p.al.check(req(t, "GET", "https://gitlab.example.com/.well-known/openid-configuration", "")); err == nil {
		t.Error("allowed")
	}
	for _, x := range []struct {
		entry issuer
		at    string
		ok    bool
	}{
		{issuer{url: "https://token.example.com/actions"}, "https://token.example.com/actions/.well-known/jwks", true},
		{issuer{url: "https://token.example.com/actions"}, "https://token.example.com/other/keys", true}, // its own origin
		{issuer{url: "https://token.example.com/actions"}, "https://keys.example.com/keys", false},
		{issuer{url: "https://token.example.com/actions"}, "https://token.example.com:8443/keys", false},
		{issuer{url: "https://token.example.com/actions"}, "http://token.example.com/keys", false},
		{issuer{url: "https://gitlab.example.com", jwks: "https://keys.example.com/k"}, "https://keys.example.com/k", false}, // allowed as given
	} {
		if got := keysAllowed(x.entry, x.at); got != x.ok {
			t.Errorf("%v %s: %v", x.entry, x.at, got)
		}
	}
}

// With --offline, no issuer is asked: unknown, and nothing sent.
func TestBacktestOffline(t *testing.T) {
	is := newIssuerAt(t, jwks(t, rsaOf(2048)))
	c := backtestConfig(ghEntry("alerts", is.URL), serve.Trust{Name: "plain", TokenFile: "/etc/lapilli/backtest/token"})
	c.Offline = true
	rep := Run(context.Background(), c)
	iss, tr := reportItem(rep, "backtest.issuers"), reportItem(rep, "backtest.trust")
	if iss == nil || iss.State != Unknown || !strings.Contains(iss.Why, "--offline") || is.count() != 0 || rep.Asked.Requests+rep.Asked.Refused != 0 {
		t.Errorf("asked %d: %+v %+v", is.count(), iss, rep.Asked)
	}
	// The trust file needs no credential: said all the same, the plain
	// tokens' files unread, unknown.
	if tr == nil || tr.State != Unknown || !strings.Contains(tr.What, "alerts: github, pins event_name and repository_id") ||
		!strings.Contains(tr.What, "plain: a plain token, its file not read") || !strings.Contains(tr.Why, "plain") {
		t.Errorf("%+v", tr)
	}
	// With issuers alone, it says what serve does: it starts.
	c = backtestConfig(ghEntry("alerts", is.URL))
	c.Offline = true
	if tr := reportItem(Run(context.Background(), c), "backtest.trust"); tr == nil || tr.State != OK {
		t.Errorf("%+v", tr)
	}
}

// What serve says at start of the backtest port's flags, beside the trust
// file, preflight says: a trust file refused, or the port without rules
// to replay, is a problem, as serve stops at start.
func TestBacktestTrustRefused(t *testing.T) {
	c := backtestConfig()
	// An error that isn't serve's refusal is said by none of its words.
	c.TrustErr = fmt.Errorf("--backtest-trust trust.yaml: alerts: claims: pin repository_id")
	p := newRun(c)
	p.backtest(context.Background())
	if it := itemOf(p, "backtest.trust"); it == nil || it.State != Problem || it.Why != "it can't be used" {
		t.Errorf("%+v", it)
	}
	if it := itemOf(p, "backtest.issuers"); it == nil || it.State != Unknown {
		t.Errorf("%+v", it)
	}
	c = backtestConfig(serve.Trust{Name: "plain", TokenFile: "/x"})
	c.Serve.RulesPath = ""
	p = newRun(c)
	p.backtest(context.Background())
	if it := itemOf(p, "backtest.trust"); it == nil || it.State != Problem || it.Why != serve.BacktestRefused(c.Serve, true).Error() {
		t.Errorf("%+v", it)
	}
	// Off, nothing is said.
	c = config()
	p = newRun(c)
	p.backtest(context.Background())
	if len(p.r.Items) != 0 {
		t.Errorf("%+v", p.r.Items)
	}
}

// Nothing a trust file holds reaches the report, however it is written:
// why serve refuses it is said in Lapilli's words alone, from the refusal
// serve.ReadTrust gives (its line, its entry, its field), never serve's
// message, which quotes what it can't take; and Lapilli's words aren't
// changed by a value that is one of them (days: 30 beside a pin of "1").
func TestBacktestTrustHidden(t *testing.T) {
	file := filepath.Join(t.TempDir(), "trust.yaml")
	gh := "trust:\n  - name: alerts\n    issuer: https://token.actions.githubusercontent.com\n    audience: %s\n    claims:\n%s"
	for _, x := range []struct {
		file, says string
		hidden     []string
	}{
		{fmt.Sprintf(gh, "https://lapilli-ci.example.com", "      repository_id: [\"123456789\", 0x75bcd15]\n      event_name: pull_request\n"),
			"line 6, entry 1, claims: a claim's value that isn't a string or an integer written in decimal", []string{"0x75bcd15", "123456789"}},
		{fmt.Sprintf(gh, "\"https://github.com/example-owner   \"", "      repository_id: \"123456789\"\n      event_name: pull_request\n"),
			"line 4, entry 1: audience is GitHub's default", []string{"example-owner"}},
		{"trust:\n  - name: generic\n    issuer: https://ci.example.com\n    audience: https://lapilli-ci.example.com\n    claims:\n      sub: 0x2f3a\n      event_name: push\n",
			"claims: a claim's value", []string{"0x2f3a"}},
		{"trust:\n  - name: alerts\n    issuer: https://token.actions.githubusercontent.com\n    audience: https://lapilli-ci.example.com\n    claims: {repository_id:987650001, event_name: pull_request}\n",
			"a claim pinned to no value", []string{"987650001"}},
		{fmt.Sprintf(gh, "https://lapilli-ci.example.com", "      \"987650002\": repository_id\n      event_name: pull_request\n"),
			"a claim's name that isn't one", []string{"987650002"}},
		{"ids: &ids [\"987650003\", 0x3ade]\ntrust:\n  - name: alerts\n    issuer: https://token.actions.githubusercontent.com\n    audience: https://lapilli-ci.example.com\n" +
			"    claims:\n      repository_id: *ids\n      event_name: pull_request\n", "decimal", []string{"987650003", "0x3ade"}},
		{fmt.Sprintf(gh, "https://lapilli-ci.example.com", "      repository_id: \"123456789\"\n      repository_owner_id: 4200917\n      event_name: pull_request_target\n"),
			"event_name: want pull_request or push", []string{"123456789", "4200917", "pull_request_target"}},
		{"trust:\n  - {name: alerts, issuer: 'https://token.actions.githubusercontent.com', audience: x, claims: {repository_id: '1', event_name: push}, days: 30}\n",
			"line 2, entry 1: days is 1 to 14", []string{"30"}},
	} {
		os.WriteFile(file, []byte(x.file), 0o600)
		_, err := serve.ReadTrust(file, nil, true)
		if err == nil {
			t.Fatalf("taken: %s", x.file)
		}
		c := backtestConfig()
		c.TrustFile, c.TrustErr = file, err
		p := newRun(c)
		p.backtest(context.Background())
		p.redact()
		it := itemOf(p, "backtest.trust")
		all := it.What + " " + it.Why + " " + it.Fix
		if !strings.Contains(it.Why, x.says) {
			t.Errorf("%v\n%+v, want it to say %q", err, it, x.says)
		}
		for _, h := range x.hidden {
			if strings.Contains(all, h) {
				t.Errorf("says %q: %+v", h, it)
			}
		}
	}
	// Taken, its entries are said by their claims' names.
	os.WriteFile(file, []byte(`trust:
  - name: generic
    issuer: https://ci.example.com
    audience: https://lapilli-ci.example.com
    claims:
      sub: repo:example/alerts:pull_request
      event_name: pull_request
`), 0o600)
	ts, err := serve.ReadTrust(file, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	c := backtestConfig(ts...)
	c.TrustFile = file
	c.Offline = true
	rep := Run(context.Background(), c)
	b, _ := json.Marshal(rep)
	if it := reportItem(rep, "backtest.trust"); it.State != OK || !strings.Contains(it.What, "generic: generic, pins event_name and sub") ||
		strings.Contains(string(b), "repo:example") || strings.Contains(string(b), "lapilli-ci") {
		t.Errorf("%s", b)
	}
}

// A backtest's requests are counted as serve's port counts them before it
// is queued: the design's case, ten rules that differ over seven days at
// --max-rate 0.5 with a quarter for the lane, 160 requests, of which 112
// fit in fifteen minutes, so four days; their for alone, 80, which fits.
func TestEstimateBacktests(t *testing.T) {
	c := backtestConfig()
	c.MaxRate = 0.5
	p := newRun(c)
	p.estimate(context.Background(), &read{all: rulesOf(t, 12, 0)})
	b := p.r.Estimates.Backtests
	if b == nil || b.Rate != 0.125 || b.Fit != 112 || b.Rules != 10 || b.Days != 7 || b.Changed != 160 || b.Shared != 80 || b.ChangedDays != 4 ||
		b.SharedDays != 10 || b.Parallel != 2 || b.TimeoutSeconds != 900 {
		t.Fatalf("%+v", b)
	}
	it := itemOf(p, "estimate.backtests")
	if it.State != OK || !strings.Contains(it.What, "160 over 7 days") || !strings.Contains(it.What, ": 4 days fit") {
		t.Errorf("%+v", it)
	}
	// A share and a time of the flags', as serve takes them.
	c.Serve.BacktestShare, c.Serve.BacktestTimeout = 0.5, 30*time.Minute
	p = newRun(c)
	p.estimate(context.Background(), &read{all: rulesOf(t, 12, 0)})
	if b := p.r.Estimates.Backtests; b.Rate != 0.25 || b.Fit != 450 || b.ChangedDays != 14 {
		t.Errorf("%+v", b)
	}
	// Without --max-rate, none is refused for its time.
	c.MaxRate = 0
	p = newRun(c)
	p.estimate(context.Background(), &read{all: rulesOf(t, 12, 0)})
	if b := p.r.Estimates.Backtests; b.Rate != 0 || b.Fit != 0 || b.ChangedDays != 14 || b.Changed != 160 {
		t.Errorf("%+v", b)
	}
	// So low a rate that not a day of one rule fits: the port takes none.
	c.MaxRate, c.Serve.BacktestShare = 0.001, 0.05
	p = newRun(c)
	p.estimate(context.Background(), &read{all: rulesOf(t, 1, 0)})
	if it := itemOf(p, "estimate.backtests"); it.State != Problem || p.r.Estimates.Backtests.SharedDays != 0 {
		t.Errorf("%+v %+v", it, p.r.Estimates.Backtests)
	}
	// Off, none.
	p = newRun(config())
	p.estimate(context.Background(), &read{all: rulesOf(t, 12, 0)})
	if p.r.Estimates.Backtests != nil || itemOf(p, "estimate.backtests") != nil {
		t.Error("counted without --backtest-listen")
	}
}

// While a backtest runs, the runs have --max-rate less the lane's share
// for --prometheus: each run's requests timed at it, beside the full rate.
func TestEstimateHourlyWhileBacktest(t *testing.T) {
	c := backtestConfig()
	c.MaxRate = 0.5
	p := newRun(c)
	p.estimate(context.Background(), &read{all: rulesOf(t, 200, 0)})
	h := p.r.Estimates.Hourly
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-6 }
	if h.WhileBacktest == nil || h.WhileBacktest.Rate != 0.375 || !near(h.WhileBacktest.Seconds, float64(h.Most)/0.375) || !near(h.Seconds, float64(h.Most)/0.5) {
		t.Fatalf("%+v %+v", h, h.WhileBacktest)
	}
	it := itemOf(p, "estimate.hourly")
	if it.State != OK || !strings.Contains(it.What, "while a backtest runs the whole hour, the runs have 0.375 a second") {
		t.Errorf("%+v", it)
	}
	// A run that fits --every at the full rate and not at the runs' share:
	// unknown, never ok.
	c.MaxRate = 0.12
	p = newRun(c)
	p.estimate(context.Background(), &read{all: rulesOf(t, 200, 0)})
	if it, h := itemOf(p, "estimate.hourly"), p.r.Estimates.Hourly; h.Seconds > 3600 || h.WhileBacktest.Seconds <= 3600 || it.State != Unknown ||
		!strings.Contains(it.Why, "while backtests run back to back") {
		t.Errorf("%+v %+v", h, it)
	}
	// Without --backtest-listen, as before.
	c = config()
	c.MaxRate = 0.12
	p = newRun(c)
	p.estimate(context.Background(), &read{all: rulesOf(t, 200, 0)})
	if it, h := itemOf(p, "estimate.hourly"), p.r.Estimates.Hourly; h.WhileBacktest != nil || it.State != OK || strings.Contains(it.What, "backtest") {
		t.Errorf("%+v %+v", h, it)
	}
}

// The lane's reads are held beside the runs' at their bound: a memory
// limit the runs fit and the two don't is unknown, never ok.
func TestEstimateMemoryBacktests(t *testing.T) {
	c := backtestConfig()
	c.Serve.Parallel, c.MaxSamples = 8, 1000
	c.CgroupMemory = filepath.Join(t.TempDir(), "memory.max")
	alerts := map[string]float64{"Errors0": 100}
	rs := rulesOf(t, 3, 0)
	p := &run{c: c, r: &Report{}, alerts: alerts}
	p.memory(rs)
	m := p.r.Estimates.Memory
	if m.BacktestBytes != 2*1000*64 {
		t.Fatalf("%+v", m)
	}
	os.WriteFile(c.CgroupMemory, []byte(fmt.Sprint(m.PeakBytes+m.BacktestBytes/2)), 0o600)
	p = &run{c: c, r: &Report{}, alerts: alerts}
	p.memory(rs)
	if it := p.r.Items[0]; it.State != Unknown || !strings.Contains(it.Why, "backtest lane's reads at their bound") {
		t.Errorf("%+v", it)
	}
	os.WriteFile(c.CgroupMemory, []byte(fmt.Sprint(m.PeakBytes+m.BacktestBytes)), 0o600)
	p = &run{c: c, r: &Report{}, alerts: alerts}
	p.memory(rs)
	if it := p.r.Items[0]; it.State != OK {
		t.Errorf("%+v", it)
	}
	// Without the lane, the same limit as before.
	c.Serve.BacktestListen = ""
	os.WriteFile(c.CgroupMemory, []byte(fmt.Sprint(m.PeakBytes)), 0o600)
	p = &run{c: c, r: &Report{}, alerts: alerts}
	p.memory(rs)
	if it, m := p.r.Items[0], p.r.Estimates.Memory; it.State != OK || m.BacktestBytes != 0 || strings.Contains(it.What, "backtest") {
		t.Errorf("%+v", it)
	}
}

// The port's most days, which the estimate counts up to, are serve's: an
// entry that gives none is given them as ReadTrust reads it.
func TestLaneDaysAsServe(t *testing.T) {
	file := filepath.Join(t.TempDir(), "trust.yaml")
	os.WriteFile(file, []byte("trust:\n  - {name: plain, token_file: /etc/lapilli/backtest/token}\n"), 0o600)
	ts, err := serve.ReadTrust(file, nil, true)
	if err != nil || len(ts) != 1 || ts[0].Days != laneDays {
		t.Errorf("%v %+v, want %d days", err, ts, laneDays)
	}
}
