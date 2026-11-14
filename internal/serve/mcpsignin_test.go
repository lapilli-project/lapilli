package serve

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/signin"
	"lapilli.dev/internal/signin/idptest"
)

// agentHeaders adds agentHeaders to each request an agent sends: a bearer token, or
// the signed header a sign-in in front sets.
type agentHeaders map[string]string

func (h agentHeaders) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h {
		r.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func connectWith(t *testing.T, url string, h agentHeaders) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: url + "/mcp",
		HTTPClient: &http.Client{Transport: h}, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// signedInWay is a sign-in of lapilli's own, or in front, for a test: the
// agentHeaders an agent acting for someone sends, and a person's request in a
// browser, as that person.
type signedInWay struct {
	handler func(http.Handler)
	agent   func(who string) agentHeaders
	browser func(t *testing.T, h http.Handler, who string) func(method, path string, form url.Values) *httptest.ResponseRecorder
}

// With a sign-in of lapilli's own or in front, an agent's proposal is the
// person's whose token it holds: kept under their name, as an agent's, so
// that they can't approve it, another approver can, and each person has
// their own twenty an hour. It was kept as "anonymous"'s, which the person
// could approve alone, and every agent shared one limit.
func TestAgentProposesSignedIn(t *testing.T) {
	oidc := func(session bool) func(t *testing.T) (signedInWay, func(*Config)) {
		return func(t *testing.T) (signedInWay, func(*Config)) {
			idp := idptest.New(t, "lapilli", "the-client-secret", func() time.Time { return now })
			secret := filepath.Join(t.TempDir(), "secret")
			os.WriteFile(secret, []byte("the-client-secret\n"), 0o600)
			key := filepath.Join(t.TempDir(), "session-key")
			os.WriteFile(key, []byte(strings.Repeat("k", 40)+"\n"), 0o600)
			var h http.Handler
			w := signedInWay{
				agent: func(who string) agentHeaders {
					if !session {
						return agentHeaders{"Authorization": "Bearer " + idp.IDToken(claimsOf(who))}
					}
					// A session's cookie, as a browser signed in has it.
					b := newBrowser(t, h)
					b.signIn(idp, "/", claimsOf(who))
					var cs []string
					for _, c := range b.cookies {
						cs = append(cs, c.Name+"="+c.Value)
					}
					return agentHeaders{"Cookie": strings.Join(cs, "; ")}
				},
				browser: func(t *testing.T, hh http.Handler, who string) func(string, string, url.Values) *httptest.ResponseRecorder {
					b := newBrowser(t, hh)
					b.signIn(idp, "/", claimsOf(who))
					return func(method, path string, form url.Values) *httptest.ResponseRecorder {
						return b.form(method, path, form)
					}
				},
			}
			w.handler = func(hh http.Handler) { h = hh }
			return w, func(c *Config) {
				c.ExternalURL, c.SessionKey = extURL, key
				c.OIDC = &signin.OIDC{Issuer: idp.URL, ClientID: "lapilli", SecretFile: secret, Scopes: []string{"email", "groups"},
					Claims: signin.Claims{UserClaim: "email", GroupsClaim: "groups", Verified: true}}
			}
		}
	}
	for name, way := range map[string]func(t *testing.T) (signedInWay, func(*Config)){
		"oidc bearer":  oidc(false),
		"oidc session": oidc(true),
		"proxy jwt": func(t *testing.T) (signedInWay, func(*Config)) {
			key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "iap", Algorithm: "ES256", Use: "sig"}}})
			}))
			t.Cleanup(keys.Close)
			aud := "/projects/123456789012/global/backendServices/42"
			iap := func(who string) string {
				return idptest.Sign(key, "iap", jose.ES256, map[string]any{"iss": "https://cloud.google.com/iap", "aud": aud,
					"iat": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(), "email": who})
			}
			w := signedInWay{
				agent: func(who string) agentHeaders { return agentHeaders{"X-Goog-Iap-Jwt-Assertion": iap(who)} },
				browser: func(t *testing.T, h http.Handler, who string) func(string, string, url.Values) *httptest.ResponseRecorder {
					return func(method, path string, form url.Values) *httptest.ResponseRecorder {
						var r *http.Request
						if form != nil {
							r = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
							r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
						} else {
							r = httptest.NewRequest(method, path, nil)
						}
						r.Header.Set("Sec-Fetch-Site", "same-origin")
						r.Header.Set("X-Goog-Iap-Jwt-Assertion", iap(who))
						rec := httptest.NewRecorder()
						h.ServeHTTP(rec, r)
						return rec
					}
				},
			}
			return w, func(c *Config) {
				c.Proxy = &signin.Proxy{Kind: signin.GoogleIAP, Audience: aud, KeysURL: keys.URL, Claims: signin.Claims{UserClaim: "email"}}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			w, sign := way(t)
			s, _ := changesServer(t, func(c *Config) {
				c.UserHeader = ""
				c.Viewers = []string{"*@example.com"}
				c.MCPPropose = "editors"
				sign(c)
			})
			h := s.Handler()
			if w.handler != nil {
				w.handler(h)
			}
			ts := httptest.NewServer(h)
			defer ts.Close()
			// cy, an approver, lets an agent propose with their token.
			var out proposeOut
			if failed, text := call(t, connectWith(t, ts.URL, w.agent(cy)), "propose", map[string]any{"rule": "Errors", "kind": "for", "for": "15m",
				"reason": "approve this"}, &out); failed {
				t.Fatalf("propose: %s", text)
			}
			p := state(t, s, out.ID)
			if !p.Agent || p.MadeBy != cy {
				t.Fatalf("an agent's proposal for %s kept as made by %q (agent %v)", cy, p.MadeBy, p.Agent)
			}
			// cy can't approve it, by the page or by a form made up.
			asCy := w.browser(t, h, cy)
			page := asCy(http.MethodGet, "/proposals/"+itoa(out.ID), nil)
			if page.Code != 200 || formKey.MatchString(page.Body.String()) || !strings.Contains(page.Body.String(), "Your agent proposed this: someone else approves it.") {
				t.Errorf("the person an agent acted for is offered its approval: %d\n%s", page.Code, page.Body)
			}
			cyKey := s.formKey(p, cy, "approve")
			if rec := asCy(http.MethodPost, "/proposals/"+itoa(out.ID)+"/approve", url.Values{"key": {cyKey}, "read": {"1"}}); rec.Code < 400 {
				t.Errorf("the person an agent acted for approved its proposal: %d", rec.Code)
			}
			if p := state(t, s, out.ID); p.State != ledger.StateProposed || len(p.Approvals) != 0 {
				t.Fatalf("approved by whom its agent acted for: %+v", p)
			}
			// ana, another approver, can.
			asAna := w.browser(t, h, ana)
			page = asAna(http.MethodGet, "/proposals/"+itoa(out.ID), nil)
			m := formKey.FindStringSubmatch(page.Body.String())
			if m == nil || m[2] != "approve" {
				t.Fatalf("another approver isn't offered its approval: %d\n%s", page.Code, page.Body)
			}
			if rec := asAna(http.MethodPost, "/proposals/"+itoa(out.ID)+"/approve", url.Values{"key": {m[3]}, "read": {"1"}}); rec.Code != http.StatusSeeOther {
				t.Fatalf("another approver approving: %d %s", rec.Code, rec.Body)
			}
			if p := state(t, s, out.ID); p.State != ledger.StateApproved || len(p.Approvals) != 1 || p.Approvals[0].By != ana {
				t.Errorf("approved by another: %+v", p)
			}
			// Each person has twenty an hour of their own: bo's twenty
			// don't stop cy's agent, and stop bo's.
			for i := range ledger.ProposalsAnHour {
				if _, err := s.l.Propose(ctx, ledger.Proposal{Rule: fmt.Sprintf("Old%d", i), Kind: ledger.KindFor, Path: ledger.PathNone,
					MadeBy: bo, Agent: true, Before: "x", After: "y"}); err != nil {
					t.Fatal(err)
				}
			}
			s.l.Move(ctx, out.ID, []string{ledger.StateApproved}, ledger.StateDropped, ana, ledger.Step{})
			if failed, text := call(t, connectWith(t, ts.URL, w.agent(bo)), "propose", map[string]any{"rule": "Errors", "kind": "for", "for": "10m"}, &out); !failed ||
				!strings.Contains(text, "in the last hour") {
				t.Errorf("bo's agent past bo's twenty an hour: %v %s", failed, text)
			}
			if failed, text := call(t, connectWith(t, ts.URL, w.agent(cy)), "propose", map[string]any{"rule": "Errors", "kind": "for", "for": "10m"}, &out); failed {
				t.Errorf("cy's agent stopped by bo's twenty: %s", text)
			} else if p := state(t, s, out.ID); p.MadeBy != cy {
				t.Errorf("made by %q", p.MadeBy)
			}
		})
	}
}

// form posts a form, or gets a page, as the browser, from the page's own
// site.
func (b *browser) form(method, target string, form url.Values) *httptest.ResponseRecorder {
	b.t.Helper()
	var r *http.Request
	if form != nil {
		r = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	if method == http.MethodPost {
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	for _, c := range b.cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	b.h.ServeHTTP(w, r)
	return w
}

// An agent's proposal is never anyone's but the person's it acts for: a
// tool called without the request's say of who is refused, and so, with a
// sign-in, is one for no one known. Without roles, on loopback, where
// everyone is an admin and no one is named, an agent proposes as anyone
// there does, "anonymous".
func TestAgentProposesAsNoOne(t *testing.T) {
	ctx := context.Background()
	s, _ := changesServer(t, func(c *Config) { c.MCPPropose = "editors" })
	if _, _, err := s.toolPropose(ctx, &mcp.CallToolRequest{}, proposeIn{Rule: "Errors", Kind: "for", For: "15m"}); err == nil {
		t.Error("proposed for no one known")
	}
	in, _ := oidcServer(t, func(c *Config) { c.MCPPropose = "editors" })
	if _, _, err := in.toolPropose(context.WithValue(ctx, agentFor{}, "anonymous"), &mcp.CallToolRequest{}, proposeIn{Rule: "Errors", Kind: "for", For: "15m"}); err == nil ||
		!strings.Contains(err.Error(), "isn't known") {
		t.Errorf("proposed as anonymous with a sign-in: %v", err)
	}
	for _, x := range []*Server{s, in} {
		if ps, _ := x.l.Proposals(ctx, ledger.ProposalQuery{}); len(ps) != 0 {
			t.Errorf("kept: %+v", ps)
		}
	}
	open, _ := server(t, func(c *Config) { c.MCPPropose = "editors" })
	raw := ""
	if err := open.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	open.jobs(ctx, true)
	ts := httptest.NewServer(open.Handler())
	defer ts.Close()
	var out proposeOut
	if failed, text := call(t, connectWith(t, ts.URL, nil), "propose", map[string]any{"rule": "Errors", "kind": "for", "for": "15m"}, &out); failed {
		t.Fatalf("without roles: %s", text)
	}
	if p := state(t, open, out.ID); p.MadeBy != "anonymous" || !p.Agent {
		t.Errorf("without roles: %+v", p)
	}
}
