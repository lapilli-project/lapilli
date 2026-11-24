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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"lapilli.dev/internal/rules"
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
		hidden string // what the document names that the item never says
		asked  int    // of the issuer's
		usable int
	}{
		{name: "ok", keys: good, state: OK, says: "1 key for RS256", asked: 2},
		{name: "two algorithms, keys of each", keys: jwks(t, rsaOf(2048), ecOf(t, elliptic.P256()), ecOf(t, elliptic.P384())), state: OK, says: "2 keys for ES256, RS256", asked: 2,
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
		{name: "another issuer", state: Problem, says: "names another issuer than the one given, exactly", asked: 1, hidden: "ACME-SLUG",
			doc: func(x *issuerAt) func(map[string]any) {
				return func(d map[string]any) { d["issuer"] = x.URL + "/ACME-SLUG/" }
			}},
		{name: "keys at another origin", state: Problem, says: "its jwks_uri isn't https at the issuer's own origin: give jwks_url", asked: 1, hidden: "keys.internal.example",
			doc: func(*issuerAt) func(map[string]any) {
				return func(d map[string]any) { d["jwks_uri"] = "https://keys.internal.example/jwks" }
			}},
		{name: "keys at another origin of one's own", state: Problem, says: "its jwks_uri isn't https at the issuer's own origin", asked: 1,
			doc: func(*issuerAt) func(map[string]any) {
				return func(d map[string]any) { d["jwks_uri"] = elsewhere.URL + "/.well-known/jwks" }
			}},
		{name: "keys at another origin, as jwks_url says", jwks: true, state: OK, says: "1 key for RS256", asked: 1,
			doc: func(*issuerAt) func(map[string]any) {
				return func(d map[string]any) { d["jwks_uri"] = elsewhere.URL + "/.well-known/jwks" }
			}},
		{name: "a redirect", state: Problem, says: "/.well-known/openid-configuration: it redirected, and redirects aren't followed", asked: 1,
			hidden: "sso.corp-a.example",
			serve: func(w http.ResponseWriter, r *http.Request) bool {
				http.Redirect(w, r, "https://sso.corp-a.example/login?client=secret-client-77", http.StatusFound)
				return true
			}},
		{name: "a redirect of the keys", state: Problem, says: "/.well-known/jwks: it redirected, and redirects aren't followed", asked: 2,
			hidden: "secret-client-77",
			serve: func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/.well-known/jwks" {
					return false
				}
				http.Redirect(w, r, "https://sso.corp-a.example/login?client=secret-client-77", http.StatusFound)
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
			if it.State != x.state || !strings.Contains(it.What+" "+it.Why, x.says) || x.hidden != "" && strings.Contains(it.What+it.Why, x.hidden) {
				t.Errorf("%+v, want %s saying %q, never %q", it, x.state, x.says, x.hidden)
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

// An issuer is said as it is when it is a public CI's, GitHub's, and
// otherwise as the trust file's issuer N, its jwks_url never: a GitHub
// Enterprise Server's host and the keys' are hidden wherever the report
// would say them, in what a server answered too.
func TestBacktestIssuersSaid(t *testing.T) {
	keys := jwks(t, rsaOf(2048))
	public := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{"issuer": "https://token.actions.githubusercontent.com",
				"jwks_uri": "https://token.actions.githubusercontent.com/.well-known/jwks", "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/.well-known/jwks":
			w.Write(keys)
		}
	})
	ghes := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"issuer": "https://github.example.com/_services/token", "id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	gone := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no keys.example.com/jwks here", http.StatusNotFound)
	})
	h := standIns(t, map[string]http.Handler{"token.actions.githubusercontent.com": public, "github.example.com": ghes, "keys.example.com": gone,
		"ghe2.example.com": gone})
	server := ghEntry("server", "https://github.example.com/_services/token")
	server.JWKSURL = "https://keys.example.com/jwks"
	other := ghEntry("other", "https://ghe2.example.com/_services/token")
	c := backtestConfig(ghEntry("alerts", "https://token.actions.githubusercontent.com"), server, other)
	rep := Run(context.Background(), c)
	it := reportItem(rep, "backtest.issuers")
	b, _ := json.Marshal(rep)
	if it == nil || it.State != Problem || !strings.Contains(it.What, "https://token.actions.githubusercontent.com (alerts), 1 key for RS256") ||
		!strings.Contains(it.Why, "[--backtest-trust's issuer 2] (server): [--backtest-trust's jwks_url 1]: 404") ||
		!strings.Contains(it.Why, "[--backtest-trust's issuer 3] (other): ") ||
		len(slices.DeleteFunc(h.all(), func(s string) bool { return strings.Contains(s, "prometheus") })) != 5 {
		t.Errorf("sent %v: %+v", h.all(), it)
	}
	// What a server answered names the issuer it was asked at: hidden.
	for _, hidden := range []string{"github.example.com", "keys.example.com", "ghe2.example.com"} {
		if strings.Contains(string(b), hidden) {
			t.Errorf("the report says %q: %s", hidden, b)
		}
	}
	// Named by number in Lapilli's own words, before anything is hidden.
	p := guarded(t, c)
	p.backtestIssuers(context.Background())
	if it := itemOf(p, "backtest.issuers"); !strings.Contains(it.Why, "[--backtest-trust's issuer 2] (server): ") ||
		!strings.Contains(it.Why, "[--backtest-trust's issuer 3] (other): ") || strings.Contains(it.Why, "_services/token (") {
		t.Errorf("%+v", it)
	}
}

// A public CI's issuer keeps the detail of what its discovery document
// names: it is the same for everyone.
func TestBacktestIssuerPublicDetail(t *testing.T) {
	standIns(t, map[string]http.Handler{"gitlab.com": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"issuer": "https://gitlab.com/", "jwks_uri": "https://gitlab.com/oauth/discovery/keys"})
	})})
	e := ghEntry("monitoring", "https://gitlab.com")
	e.CI = "gitlab"
	p := guarded(t, backtestConfig(e))
	p.backtestIssuers(context.Background())
	if it := itemOf(p, "backtest.issuers"); it.State != Problem || !strings.Contains(it.Why, `its discovery document says "https://gitlab.com/"`) {
		t.Errorf("%+v", it)
	}
}

// A discovery document that names an algorithm many times names it once:
// a key set's algorithms are each once (signin.NewKeys), and the item is
// short whatever the document says.
func TestBacktestIssuerAlgorithmsOnce(t *testing.T) {
	is := newIssuerAt(t, jwks(t, rsaOf(2048)))
	is.doc = func(d map[string]any) {
		algs := make([]string, 120000)
		for i := range algs {
			algs[i] = "RS256"
		}
		d["id_token_signing_alg_values_supported"] = algs
	}
	p := guarded(t, backtestConfig(ghEntry("alerts", is.URL)))
	p.backtestIssuers(context.Background())
	if it := itemOf(p, "backtest.issuers"); it.State != OK || len(it.What) > 200 || !strings.HasSuffix(it.What, "1 key for RS256") {
		t.Errorf("%d bytes: %.300s", len(it.What), it.What)
	}
	// Every algorithm of a key pair named: four said, and how many more.
	is.doc = func(d map[string]any) {
		d["id_token_signing_alg_values_supported"] = []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA"}
	}
	p = guarded(t, backtestConfig(ghEntry("alerts", is.URL)))
	p.backtestIssuers(context.Background())
	if it := itemOf(p, "backtest.issuers"); it.State != OK || !strings.HasSuffix(it.What, "and 6 more") {
		t.Errorf("%s", it.What)
	}
}

// Issuers are asked four at a time: eight that each wait for the others
// are asked four at once, never more, and the eight answered.
func TestBacktestIssuersAtOnce(t *testing.T) {
	var mu sync.Mutex
	in, most := 0, 0
	release := make(chan struct{})
	var trust []serve.Trust
	var all []*issuerAt
	for i := range 8 {
		x := newIssuerAt(t, jwks(t, rsaOf(2048)))
		x.serve = func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path != "/.well-known/openid-configuration" {
				return false
			}
			mu.Lock()
			in++
			most = max(most, in)
			if in == 4 {
				select {
				case <-release:
				default:
					close(release)
				}
			}
			mu.Unlock()
			// Until four are asked at once; a client that gives up first
			// ends it.
			select {
			case <-release:
			case <-r.Context().Done():
			}
			mu.Lock()
			in--
			mu.Unlock()
			return false
		}
		all = append(all, x)
		trust = append(trust, ghEntry(fmt.Sprint("e", i), x.URL))
	}
	p := guarded(t, backtestConfig(trust...))
	p.backtestIssuers(context.Background())
	it := itemOf(p, "backtest.issuers")
	if most != 4 || it.State != OK || strings.Count(it.What, "1 key for RS256") != 8 {
		t.Errorf("%d at once: %+v", most, it)
	}
	for i, x := range all {
		if x.count() != 2 {
			t.Errorf("e%d asked %v", i, x.asked)
		}
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
			"event_name: want pull_request, push or merge_group", []string{"123456789", "4200917", "pull_request_target"}},
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

// A Bitbucket entry is read as serve reads it (serve.ReadTrust, the CI
// known from the workspace's issuer) and said by its claims' names; its
// issuer, which names the workspace, is the trust file's issuer N, and
// neither the workspace nor a UUID it pins is said. One that pins no
// workspace is refused, as serve refuses it.
func TestBacktestTrustBitbucket(t *testing.T) {
	file := filepath.Join(t.TempDir(), "trust.yaml")
	entry := `trust:
  - name: monitoring-bitbucket
    issuer: https://api.bitbucket.org/2.0/workspaces/example-987650020/pipelines-config/identity/oidc
    audience: https://lapilli-ci.example.com
    claims:
      repositoryUuid: ["{1a2b3c4d-2222-4333-8444-987650021aaa}", "{1a2b3c4d-2222-4333-8444-987650022bbb}"]
`
	os.WriteFile(file, []byte(entry+"      workspaceUuid: \"{4b1d9c2e-7a3f-4e5b-8c6d-987650023ccc}\"\n"), 0o600)
	ts, err := serve.ReadTrust(file, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	c := backtestConfig(ts...)
	c.TrustFile = file
	c.Offline = true
	rep := Run(context.Background(), c)
	b, _ := json.Marshal(rep)
	if it := reportItem(rep, "backtest.trust"); it.State != OK || !strings.Contains(it.What, "monitoring-bitbucket: bitbucket, pins repositoryUuid (any of 2) and workspaceUuid") ||
		strings.Contains(string(b), "98765002") {
		t.Errorf("%s", b)
	}
	os.WriteFile(file, []byte(entry), 0o600)
	_, err = serve.ReadTrust(file, nil, true)
	c = backtestConfig()
	c.TrustFile, c.TrustErr = file, err
	p := newRun(c)
	p.backtest(context.Background())
	p.redact()
	if it := itemOf(p, "backtest.trust"); it == nil || it.State != Problem || !strings.Contains(it.Why, "entry 1: claims: pin workspaceUuid beside repositoryUuid") ||
		strings.Contains(it.What+it.Why+it.Fix, "98765002") {
		t.Errorf("%v: %+v", err, it)
	}
}

// A backtest's requests are counted as serve's port counts them before it
// is queued (serve.BacktestRequests, which fits calls): the design's case,
// ten rules that differ over seven days at --max-rate 0.5 with a quarter
// for the lane, 160 requests, of which 112 fit in fifteen minutes, so four
// days, and seven of the rules for the seven; their for alone, 5m to 10m,
// 90, as each rule's first read starts where its for says, 9 days.
func TestEstimateBacktests(t *testing.T) {
	c := backtestConfig()
	c.MaxRate = 0.5
	p := newRun(c)
	p.estimate(context.Background(), &read{all: rulesOf(t, 12, 0)})
	b := p.r.Estimates.Backtests
	if b == nil || b.Rate != 0.125 || b.Fit != 112 || b.Rules != 10 || b.Days != 7 || b.Changed != 160 || b.Shared != 90 || b.ChangedDays != 4 ||
		b.SharedDays != 9 || b.Parallel != 2 || b.TimeoutSeconds != 900 || b.WeekRules != 7 || b.DayRules != 10 {
		t.Fatalf("%+v", b)
	}
	it := itemOf(p, "estimate.backtests")
	if it.State != OK || !strings.Contains(it.What, "160 over 7 days") || !strings.Contains(it.What, ": 4 days fit") ||
		!strings.Contains(it.What, "5 minutes longer, 90: 9 days fit") || !strings.Contains(it.What, "7 fit over 7 days, 10 over a day") {
		t.Errorf("%+v", it)
	}
	// As serve counts it: the same planner, for the same rules and now.
	rs := changedRules(rulesOf(t, 12, 0))
	if got := serve.BacktestRequests(p.planned(), rs, forBumped(rs), 7, p.now, c.Serve.Settle, c.Serve.Step); got != b.Shared {
		t.Errorf("serve counts %d, preflight %d", got, b.Shared)
	}
	// A share and a time of the flags', as serve takes them.
	c.Serve.BacktestShare, c.Serve.BacktestTimeout = 0.5, 30*time.Minute
	p = newRun(c)
	p.estimate(context.Background(), &read{all: rulesOf(t, 12, 0)})
	if b := p.r.Estimates.Backtests; b.Rate != 0.25 || b.Fit != 450 || b.ChangedDays != 14 || b.WeekRules != 0 {
		t.Errorf("%+v", b)
	}
	// Without --max-rate, none is refused for its time.
	c.MaxRate = 0
	p = newRun(c)
	p.estimate(context.Background(), &read{all: rulesOf(t, 12, 0)})
	if b := p.r.Estimates.Backtests; b.Rate != 0 || b.Fit != 0 || b.ChangedDays != 14 || b.Changed != 160 {
		t.Errorf("%+v", b)
	}
	// So low a rate that ten rules don't fit a day, and one rule's for over
	// a day does: not a problem, and how many fit is said.
	c = backtestConfig()
	c.MaxRate = 0.05
	p = newRun(c)
	p.estimate(context.Background(), &read{all: rulesOf(t, 12, 0)})
	if it, b := itemOf(p, "estimate.backtests"), p.r.Estimates.Backtests; it.State != OK || b.Fit != 11 || b.SharedDays != 0 || b.WeekRules != 0 || b.DayRules != 2 ||
		!strings.Contains(it.What, "none fit over 7 days, 2 over a day") {
		t.Errorf("%+v %+v", it, b)
	}
	// So low that one rule's for over a day doesn't fit: the port takes
	// none.
	c.MaxRate, c.Serve.BacktestShare = 0.001, 0.05
	p = newRun(c)
	p.estimate(context.Background(), &read{all: rulesOf(t, 12, 0)})
	if it := itemOf(p, "estimate.backtests"); it.State != Problem || !strings.Contains(it.Why, "over a day asks 2 requests at least") {
		t.Errorf("%+v %+v", it, p.r.Estimates.Backtests)
	}
	// Two requests fit, which a rule added alone asks over a day: not a
	// problem, though its for changed (3) doesn't fit.
	c = backtestConfig()
	c.MaxRate, c.Serve.BacktestTimeout = 0.16, time.Minute
	p = newRun(c)
	p.estimate(context.Background(), &read{all: rulesOf(t, 12, 0)})
	if it, b := itemOf(p, "estimate.backtests"), p.r.Estimates.Backtests; b.Fit != 2 || it.State != OK {
		t.Errorf("%+v %+v", it, b)
	}
	// The rules not all read: what a backtest of them asks is unknown.
	c = backtestConfig()
	c.MaxRate = 0.5
	p = newRun(c)
	p.estimate(context.Background(), &read{all: rulesOf(t, 12, 0), failed: true})
	if it := itemOf(p, "estimate.backtests"); it.State != Unknown || !strings.Contains(it.Why, "couldn't all be read") || p.r.Estimates.Backtests.Changed != 0 {
		t.Errorf("%+v", it)
	}
	// Off, none.
	p = newRun(config())
	p.estimate(context.Background(), &read{all: rulesOf(t, 12, 0)})
	if p.r.Estimates.Backtests != nil || itemOf(p, "estimate.backtests") != nil {
		t.Error("counted without --backtest-listen")
	}
}

// Each rule is planned with its own for: one that reaches back past
// midnight UTC asks a day more. At 02:30 UTC, with --settle 1h, ten rules
// for 2h start their reads at 23:30 the day before: 9 days a side, 180.
func TestEstimateBacktestsOwnFor(t *testing.T) {
	var b strings.Builder
	b.WriteString("groups:\n  - name: shop\n    interval: 1m\n    rules:\n")
	for i := range 10 {
		fmt.Fprintf(&b, "      - alert: Slow%d\n        expr: latency_seconds{job=\"svc%d\"} > 2\n        for: 2h\n", i, i)
	}
	rs, err := rules.Parse([]byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	c := backtestConfig()
	c.MaxRate = 0.5
	p := newRun(c)
	p.now = time.Date(2026, 11, 16, 2, 30, 0, 0, time.UTC)
	p.estimate(context.Background(), &read{all: rs})
	if got := p.r.Estimates.Backtests; got.Changed != 180 {
		t.Errorf("%+v", got)
	}
	p = newRun(c)
	p.estimate(context.Background(), &read{all: rulesOf(t, 12, 0)})
	if got := p.r.Estimates.Backtests; got.Changed != 160 {
		t.Errorf("for 5m: %+v", got)
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

// A Loki's requests, and a Grafana data source's, are timed at the runs'
// rate less the lane's share too, as a backtest of the rules in effect may
// ask them; the estimate's backtest changes Grafana and Loki rules
// replayed as well, each of queries of its own.
func TestEstimateBacktestsLokiGrafana(t *testing.T) {
	var b strings.Builder
	b.WriteString("groups:\n  - name: logs\n    interval: 1m\n    rules:\n")
	for i := range 200 {
		fmt.Fprintf(&b, "      - alert: LogErrors%d\n        expr: sum(count_over_time({app=\"svc%d\"} |= \"error\" [5m])) > 1\n", i, i)
	}
	rs, err := rules.Parse([]byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	for i := range rs {
		rs[i].Logs = rules.LokiRuler
	}
	c := backtestConfig()
	c.MaxRate, c.Serve.Loki = 0.5, "http://loki:3100"
	p := newRun(c)
	p.estimate(context.Background(), &read{all: rs})
	h := p.r.Estimates.Hourly
	if h == nil || h.By["loki"] == 0 || h.WhileBacktest == nil || math.Abs(h.WhileBacktest.Seconds-float64(h.Most)/0.375) > 1e-6 {
		t.Errorf("%+v %+v", h, h.WhileBacktest)
	}
	if bt := p.r.Estimates.Backtests; bt == nil || bt.Rules != 10 || bt.Changed == 0 {
		t.Errorf("%+v", bt)
	}
	grafana, err := rules.Parse([]byte(`apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    interval: 1m
    rules:
      - uid: a
        title: A
        condition: A
        data:
          - {refId: A, datasourceUid: prom, model: {expr: 'up == 0'}}
      - uid: b
        title: B
        condition: A
        data:
          - {refId: A, datasourceUid: prom, model: {expr: 'up == 0'}}
      - uid: c
        title: C
        condition: A
        data:
          - {refId: A, datasourceUid: prom, model: {expr: 'errors > 1'}}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := changedRules(slices.Concat(grafana, rs[:2], rulesOf(t, 1, 0))); len(got) != 5 || got[0].Name != "A" || got[1].Name != "C" {
		t.Errorf("%d: %v", len(got), got)
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
