package serve

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/signin/idptest"
)

// Made up, as Bitbucket writes them: a repository, its workspace, and
// another workspace's repository.
const (
	bbRepo           = "{1a2b3c4d-2222-4333-8444-5555555555ee}"
	bbWorkspace      = "{4b1d9c2e-7a3f-4e5b-8c6d-0e1f2a3b4c5d}"
	bbOtherRepo      = "{66666666-7777-4888-8999-aaaaaaaaaaaa}"
	bbOtherWorkspace = "{0e1f2a3b-4c5d-4e6f-8a7b-9c0d1e2f3a4b}"
)

// bbServer is btServer's serve with two Bitbucket workspaces' issuers
// trusted beside the others: example's repository bbRepo, and another
// workspace's bbOtherRepo, each by its own entry.
func bbServer(t *testing.T) (b *btSet, bb, other *idptest.CI) {
	t.Helper()
	clock := func() time.Time { return now }
	bb = idptest.NewCI(t, "/2.0/workspaces/example/pipelines-config/identity/oidc", clock)
	other = idptest.NewCI(t, "/2.0/workspaces/other/pipelines-config/identity/oidc", clock)
	extra := fmt.Sprintf(`  - name: monitoring-bitbucket
    issuer: %s
    ci: bitbucket
    audience: %s
    claims:
      repositoryUuid: ["%s"]
      workspaceUuid: "%s"
  - name: other-bitbucket
    issuer: %s
    ci: bitbucket
    audience: %s
    claims:
      repositoryUuid: "%s"
      workspaceUuid: "%s"
`, bb.Issuer, btAudience, bbRepo, bbWorkspace, other.Issuer, btAudience, bbOtherRepo, bbOtherWorkspace)
	return btServer(t, nil, extra, nil), bb, other
}

// bitbucket is a Bitbucket step's token of example's workspace, with
// serve's audience beside the workspace's default, claims changed.
func bitbucket(bb *idptest.CI, change map[string]any) string {
	c := idptest.Bitbucket(bbRepo, bbWorkspace, btAudience)
	for k, v := range change {
		c[k] = v
	}
	return bb.Token(btAudience, c)
}

// A Bitbucket step's token, as Bitbucket writes it (the workspace's
// default audience beside the one the step asked for, the UUIDs in
// braces, no jti), gets a backtest; so does one naming serve's audience
// alone. The repository kept is its UUID; the pull request the job's word.
func TestBacktestBitbucket(t *testing.T) {
	parallel(t)
	b, bb, _ := bbServer(t)
	for name, token := range map[string]string{
		"beside the default": bitbucket(bb, nil),
		"its own alone":      bitbucket(bb, map[string]any{"aud": btAudience}),
	} {
		md, rep := b.done(t, token, btBody(ruleFile, changedRules, map[string]any{"days": 3, "pull_request": 7}))
		if !strings.Contains(md, "judged by lapilli serve's record") || !strings.Contains(md, "`INC-1`") || rep["judged_by"] != "lapilli serve" {
			t.Errorf("%s: %s", name, md)
		}
	}
	bs, _ := b.s.l.Backtests(context.Background(), 10)
	n := 0
	for _, x := range bs {
		if x.Trust != "monitoring-bitbucket" {
			continue
		}
		n++
		if x.Repository != bbRepo || x.PullRequest != 7 {
			t.Errorf("kept: %+v", x)
		}
	}
	if n != 2 {
		t.Errorf("%d of Bitbucket's kept", n)
	}
}

// What a Bitbucket token must be to be taken, each refused with the same
// 401, its body unread: the entry's repository and workspace, exactly;
// serve's audience, alone or beside the workspace's own default, never
// the default alone, a third, another service's or another workspace's
// beside it; signed by the workspace's own issuer, not another
// workspace's; fresh, as every CI's token is. Only a Bitbucket entry takes
// two audiences.
func TestBacktestBitbucketRefused(t *testing.T) {
	parallel(t)
	b, bb, other := bbServer(t)
	def := idptest.BitbucketAudience(bbWorkspace)
	fromOther := idptest.Bitbucket(bbRepo, bbWorkspace, btAudience)
	gh := idptest.GitHub("123456789", "42")
	gh["aud"] = []string{btAudience, def}
	for name, token := range map[string]string{
		"another repository":                   bitbucket(bb, map[string]any{"repositoryUuid": bbOtherRepo}),
		"another workspace":                    bitbucket(bb, map[string]any{"workspaceUuid": bbOtherWorkspace}),
		"the repository without braces":        bitbucket(bb, map[string]any{"repositoryUuid": strings.Trim(bbRepo, "{}")}),
		"the repository in capitals":           bitbucket(bb, map[string]any{"repositoryUuid": strings.ToUpper(bbRepo)}),
		"no workspace":                         bitbucket(bb, map[string]any{"workspaceUuid": nil}),
		"the default audience alone":           bitbucket(bb, map[string]any{"aud": def}),
		"a third audience":                     bitbucket(bb, map[string]any{"aud": []string{def, btAudience, "sts.amazonaws.com"}}),
		"another service's beside":             bitbucket(bb, map[string]any{"aud": []string{"https://other.example.com", btAudience}}),
		"another workspace's default beside":   bitbucket(bb, map[string]any{"aud": []string{idptest.BitbucketAudience(bbOtherWorkspace), btAudience}}),
		"its own twice":                        bitbucket(bb, map[string]any{"aud": []string{btAudience, btAudience}}),
		"another workspace's issuer":           other.Token(btAudience, fromOther),
		"signed by another workspace's key":    other.Token(btAudience, mapWith(fromOther, "iss", bb.Issuer)),
		"expired":                              bitbucket(bb, map[string]any{"exp": now.Add(-time.Minute).Unix()}),
		"an hour and more old":                 bitbucket(bb, map[string]any{"iat": now.Add(-61 * time.Minute).Unix(), "exp": now.Add(time.Hour).Unix()}),
		"issued in the future":                 bitbucket(bb, map[string]any{"iat": now.Add(10 * time.Minute).Unix(), "exp": now.Add(20 * time.Minute).Unix()}),
		"not valid yet":                        bitbucket(bb, map[string]any{"nbf": now.Add(10 * time.Minute).Unix()}),
		"no iat":                               bitbucket(bb, map[string]any{"iat": nil}),
		"HS256 by the public key":              hmacToken(bb, idptest.Bitbucket(bbRepo, bbWorkspace, btAudience)),
		"GitHub's, beside Bitbucket's default": b.gh.Token(btAudience, gh),
	} {
		read := &countingReader{r: bytes.NewReader(btBody(ruleFile, changedRules, nil))}
		r := httptest.NewRequest("POST", "/v1/backtests", read)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		b.h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized || strings.TrimSpace(w.Body.String()) != `{"error":"unauthorized"}` {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
		if read.n > 0 {
			t.Errorf("%s: the body was read before the token verified", name)
		}
	}
	// The other workspace's own token, of its own repository, is its
	// entry's: what was refused was the pins, not the issuer.
	ok := other.Token(btAudience, idptest.Bitbucket(bbOtherRepo, bbOtherWorkspace, btAudience))
	if code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", ok, btBody(ruleFile, changedRules, nil)); code != http.StatusAccepted {
		t.Errorf("the other workspace's own: %d %v", code, m)
	}
}

func mapWith(m map[string]any, k string, v any) map[string]any {
	out := map[string]any{}
	for kk, vv := range m {
		out[kk] = vv
	}
	out[k] = v
	return out
}

// A Bitbucket token names no pull request: two steps' backtests of the
// pull request the jobs say is 7 both wait, neither taking the other's
// place, as a GitLab merge request's don't. Each step's token is its own,
// known by what it says (no jti), and starts one backtest.
func TestBacktestBitbucketNoSupersede(t *testing.T) {
	parallel(t)
	b, bb, _ := bbServer(t)
	for i, step := range []string{"{8f2c4a1e-5b7d-4e3a-9c6f-1a2b3c4d5e6f}", "{9a3d5b2f-6c8e-4f4b-8d7a-2b3c4d5e6f70}"} {
		tok := bitbucket(bb, map[string]any{"stepUuid": step, "sub": bbRepo + ":" + step})
		code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", tok, btBody(ruleFile, changedRules, map[string]any{"pull_request": 7, "commit": fmt.Sprint("0123abc", i)}))
		if code != http.StatusAccepted || m["position"] != float64(i) {
			t.Fatalf("step %d: %d %v", i, code, m)
		}
		if code, _, n := btPostCounting(t, b, tok, btBody(ruleFile, changedRules, nil)); code != http.StatusConflict || n != 0 {
			t.Errorf("step %d's token again: %d, %d bytes read", i, code, n)
		}
	}
	bs, _ := b.s.l.Backtests(context.Background(), 10)
	waiting := 0
	for _, x := range bs {
		if x.Trust == "monitoring-bitbucket" && x.State == ledger.BacktestWaiting && x.PullRequest == 7 {
			waiting++
		}
	}
	if waiting != 2 || b.s.btPort.ended[[2]string{"monitoring-bitbucket", ledger.BacktestSuperseded}] != 0 {
		t.Errorf("%d waiting, %d superseded", waiting, b.s.btPort.ended[[2]string{"monitoring-bitbucket", ledger.BacktestSuperseded}])
	}
}
