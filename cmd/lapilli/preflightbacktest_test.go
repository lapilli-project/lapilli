package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/serve"
	"lapilli.dev/internal/signin/idptest"
	"lapilli.dev/internal/verdict"
)

// The examples are made up: repositories 123456789 and 987654321, a
// project 42, a backtest port at lapilli-ci.example.com.

// lapilli preflight with the backtest port's flags, against stand-in
// issuers of GitHub's and Forgejo's kind and a Prometheus, beside a ledger
// of this release's schema: the trust file is said as serve takes it, by
// its entries' names, CIs and the claims they pin; each issuer is asked
// once for its discovery document and once for its keys, counted as
// backtest, none refused; the lane's share is in the estimates; and the
// report names no repository's id, sub, audience or plain token, nor the
// token's length.
func TestPreflightBacktests(t *testing.T) {
	was := now
	now = time.Now
	t.Cleanup(func() { now = was })
	prometheus := newFake(t, "Prometheus", func(w http.ResponseWriter, r *http.Request, f url.Values) bool {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v1/query_range" && f.Get("query") == "vector(1)":
			matrix(w, fmt.Sprintf(`{"metric":{},"values":[[%s,"1"]]}`, f.Get("start")))
		case r.Method == "POST" && r.URL.Path == "/api/v1/query_range" && strings.Contains(f.Get("query"), "ALERTS"):
			matrix(w, fmt.Sprintf(`{"metric":{"alertname":"Errors0"},"values":[[%s,"60"]]}`, f.Get("start")))
		default:
			return false
		}
		return true
	})
	srv := httptest.NewServer(prometheus)
	t.Cleanup(srv.Close)
	github := idptest.NewCI(t, "", time.Now)
	forgejo := idptest.NewCI(t, "/api/actions", time.Now)
	dir := t.TempDir()
	put := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	plainToken := "plain-backtest-token-0123456789abcdefXYZ"
	trust := put("trust.yaml", `trust:
  - name: alerts
    issuer: `+github.Issuer+`
    ci: github
    audience: https://lapilli-ci.example.com
    claims:
      repository_id: ["123456789", "987654321"]
      repository_owner_id: "4200917"
      event_name: pull_request
  - name: alerts-push
    issuer: `+github.Issuer+`
    ci: github
    audience: https://lapilli-ci.example.com
    claims:
      repository_id: "123456789"
      event_name: push
  - name: monitoring
    issuer: `+forgejo.Issuer+`
    audience: https://lapilli-ci.example.com
    claims:
      repository_id: "42"
      event_name: pull_request
  - name: azure
    token_file: `+put("backtest-token", plainToken+"\n")+`
`)
	var rules strings.Builder
	rules.WriteString("groups:\n  - name: shop\n    interval: 1m\n    rules:\n")
	for i := range 12 {
		fmt.Fprintf(&rules, "      - alert: Errors%d\n        expr: rate(errors_total{job=\"svc%d\"}[5m]) > 1\n        for: 5m\n", i, i)
	}
	ledgerDir := filepath.Join(dir, "ledger")
	os.MkdirAll(ledgerDir, 0o750)
	l, err := ledger.Open(context.Background(), filepath.Join(ledgerDir, "ledger.db"), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	args := []string{"--rules", put("rules.yaml", rules.String()), "--prometheus", srv.URL, "--max-rate", "0.5", "--ledger", ledgerDir,
		"--backtest-listen", ":8084", "--backtest-trust", trust, "--check-content=false"}
	rep, code, out := pfReport(t, args...)
	items := pfItems(rep)
	for id, it := range items {
		if it["state"] != "ok" {
			t.Errorf("%s: %v", id, it)
		}
	}
	if code != 0 {
		t.Errorf("exit %d", code)
	}
	tr := items["backtest.trust"]["what"].(string)
	for _, said := range []string{"serve takes its 4 entries", "alerts: github, pins event_name, repository_id (any of 2) and repository_owner_id",
		"monitoring: forgejo, pins event_name and repository_id", "azure: a plain token, its file read, long enough and no other token's"} {
		if !strings.Contains(tr, said) {
			t.Errorf("backtest.trust doesn't say %q: %s", said, tr)
		}
	}
	if iss := items["backtest.issuers"]["what"].(string); !strings.Contains(iss, "(alerts, alerts-push), 1 key for RS256") || !strings.Contains(iss, "(monitoring), 1 key for RS256") {
		t.Errorf("backtest.issuers: %s", iss)
	}
	if !strings.Contains(items["ledger.sqlite"]["what"].(string), "schema 025, this release's") {
		t.Errorf("%v", items["ledger.sqlite"])
	}
	if !strings.Contains(items["access"]["what"].(string), "--backtest-listen") || items["estimate.backtests"] == nil {
		t.Errorf("%v %v", items["access"], items["estimate.backtests"])
	}
	asked := rep["asked"].(map[string]any)
	by := asked["by"].(map[string]any)
	sent := prometheus.count() + int(github.Discoveries.Load()+github.KeyFetches.Load()+forgejo.Discoveries.Load()+forgejo.KeyFetches.Load())
	if int(asked["requests"].(float64)) != sent || asked["refused"].(float64) != 0 || by["backtest"] != 4.0 {
		t.Errorf("asked %v; the stand-ins were sent %d", asked, sent)
	}
	for _, x := range []*idptest.CI{github, forgejo} {
		if x.Discoveries.Load() != 1 || x.KeyFetches.Load() != 1 {
			t.Errorf("%s: %d discoveries, %d key fetches", x.Issuer, x.Discoveries.Load(), x.KeyFetches.Load())
		}
	}
	est := rep["estimates"].(map[string]any)
	if b := est["backtests"].(map[string]any); b["fit"] != 112.0 || b["changed"] != 160.0 || b["changed_days"] != 4.0 {
		t.Errorf("%v", b)
	}
	if w := est["hourly"].(map[string]any)["while_backtest"].(map[string]any); w["rate"] != 0.375 {
		t.Errorf("%v", w)
	}
	// Issuers of no public CI's are said as the trust file's issuer N, and
	// hidden wherever the report would say them; nor are what the entries
	// pin, the audience and the plain token said, nor how long the token is.
	if iss := items["backtest.issuers"]["what"].(string); !strings.Contains(iss, "[--backtest-trust's issuer 1] (alerts, alerts-push)") ||
		!strings.Contains(iss, "[--backtest-trust's issuer 2] (monitoring)") || strings.Contains(out, "/api/actions") {
		t.Errorf("the issuers are said as they are:\n%s", out)
	}
	for _, hidden := range []string{"123456789", "987654321", "4200917", "lapilli-ci.example.com", plainToken} {
		if strings.Contains(out, hidden) {
			t.Errorf("the report says %q:\n%s", hidden, out)
		}
	}
	// Nor how long the token is, in any unit: the report's other numbers
	// (a time taken, the free space) may be 40 all the same.
	if length := regexp.MustCompile(fmt.Sprintf(`\b%d (characters|bytes|chars)\b`, len(plainToken))); length.MatchString(out) {
		t.Errorf("the report says the token's length:\n%s", out)
	}

	// --offline: the trust file is read all the same, no issuer is asked,
	// and the plain token's file isn't read.
	before := github.Discoveries.Load() + forgejo.Discoveries.Load()
	rep, code, _ = pfReport(t, append(args, "--offline")...)
	items = pfItems(rep)
	if items["backtest.trust"]["state"] != "unknown" || items["backtest.issuers"]["state"] != "unknown" || code != 0 ||
		github.Discoveries.Load()+forgejo.Discoveries.Load() != before || rep["asked"].(map[string]any)["requests"] != 0.0 {
		t.Errorf("exit %d: %v %v %v", code, items["backtest.trust"], items["backtest.issuers"], rep["asked"])
	}
}

// What lapilli preflight says of a trust file is what serve does with it
// at start, read by the same reader (serve.ReadTrust, as readServe reads
// the flags): serve refuses it, and stops, exactly when preflight says it
// is a problem, saying the same refusal (serve.TrustRefused) in Lapilli's
// words; serve takes it exactly when preflight says so. And the port's flags that serve.New refuses beside
// the file (rules to replay) are said alike.
func TestPreflightTrustAsServe(t *testing.T) {
	dir := t.TempDir()
	put := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	rules := put("rules.yaml", "groups:\n  - name: g\n    rules:\n      - alert: Down\n        expr: up == 0\n")
	token := put("token", strings.Repeat("t", 40))
	incidentsToken := put("incidents-token", strings.Repeat("t", 40))
	gh := func(claims string) string {
		return "trust:\n  - name: alerts\n    issuer: https://token.actions.githubusercontent.com\n    audience: https://lapilli-ci.example.com\n    claims:\n" + claims
	}
	for _, x := range []struct {
		name, file string
		more       []string
	}{
		{"github", gh("      repository_id: \"123456789\"\n      event_name: pull_request\n"), nil},
		{"plain", "trust:\n  - {name: plain, token_file: " + token + "}\n", nil},
		{"a missing file", "", nil},
		{"an unknown key", "trust:\n  - {name: plain, token_file: " + token + ", hourly: 5, weekly: 5}\n", nil},
		{"two documents", "trust:\n  - {name: plain, token_file: " + token + "}\n---\ntrust: []\n", nil},
		{"no entries", "trust: []\n", nil},
		{"no audience", "trust:\n  - name: alerts\n    issuer: https://token.actions.githubusercontent.com\n    claims: {repository_id: \"123456789\", event_name: push}\n", nil},
		{"no repository's id", gh("      event_name: pull_request\n"), nil},
		{"no event", gh("      repository_id: \"123456789\"\n"), nil},
		{"a fork's event", gh("      repository_id: \"123456789\"\n      event_name: pull_request_target\n"), nil},
		{"a list of events", gh("      repository_id: \"123456789\"\n      event_name: [pull_request, push]\n"), nil},
		{"a generic issuer without sub", "trust:\n  - name: ci\n    issuer: https://ci.example.com\n    audience: https://lapilli-ci.example.com\n    claims: {event_name: push, job: x}\n", nil},
		{"an issuer of plain http", "trust:\n  - name: ci\n    issuer: http://ci.example.com\n    audience: https://lapilli-ci.example.com\n    claims: {sub: x, event_name: push}\n", nil},
		{"a short token", "trust:\n  - {name: plain, token_file: " + put("short", "short-token") + "}\n", nil},
		{"a token's file not there", "trust:\n  - {name: plain, token_file: " + filepath.Join(dir, "none") + "}\n", nil},
		{"a token another file holds", "trust:\n  - {name: plain, token_file: " + token + "}\n", []string{"--incidents-listen", ":9091", "--incidents-token-file", incidentsToken}},
		{"a token two entries hold", "trust:\n  - {name: a, token_file: " + token + "}\n  - {name: b, token_file: " + token + "}\n", nil},
	} {
		file := filepath.Join(dir, "missing.yaml")
		if x.file != "" {
			file = put(strings.ReplaceAll(x.name, " ", "-")+".yaml", x.file)
		}
		args := append([]string{"--rules", rules, "--prometheus", "http://prometheus.example.com", "--ledger", t.TempDir(), "--check-content=false",
			"--check-replay=false", "--backtest-listen", ":8084", "--backtest-trust", file}, x.more...)
		// As serve reads its flags, and stops at.
		_, serveErr := readServe(args, false)
		var out, errOut bytes.Buffer
		code := run(append([]string{"serve"}, append(args, "--ledger", rules)...), &out, &errOut)
		if (serveErr != nil) != (code == 2) || serveErr != nil && !strings.Contains(errOut.String(), verdict.Clean(serveErr.Error())) {
			t.Errorf("%s: serve exits %d, %v:\n%s", x.name, code, serveErr, errOut.String())
		}
		// Said in Lapilli's words, from serve's own refusal: where, and why.
		said := func(err error) string {
			var tr *serve.TrustRefused
			if !errors.As(err, &tr) {
				return "not serve's refusal: " + err.Error()
			}
			return verdict.Clean(tr.Said())
		}
		rep, code, _ := pfReport(t, args...)
		it := pfItems(rep)["backtest.trust"]
		switch {
		case it == nil:
			t.Errorf("%s: no backtest.trust", x.name)
		case serveErr != nil && (it["state"] != "problem" || it["why"] != said(serveErr) || code != 1):
			t.Errorf("%s: serve refuses it, %v; preflight exits %d: %v", x.name, serveErr, code, it)
		case serveErr == nil && it["state"] != "ok":
			t.Errorf("%s: serve takes it; preflight says %v", x.name, it)
		}
		// --offline, the token files aren't read, as serve's reader reads
		// them with the secrets as given: what it refuses then, preflight
		// says; what it can't know is unknown, never ok.
		_, offErr := readServe(args, true)
		rep, code, _ = pfReport(t, append(args, "--offline")...)
		it = pfItems(rep)["backtest.trust"]
		switch {
		case offErr != nil && (it["state"] != "problem" || it["why"] != said(offErr) || code != 1):
			t.Errorf("%s --offline: %v; preflight exits %d: %v", x.name, offErr, code, it)
		case offErr == nil && it["state"] == "problem":
			t.Errorf("%s --offline: taken; preflight says %v", x.name, it)
		case serveErr != nil && it["state"] == "ok":
			t.Errorf("%s --offline: serve refuses it, %v; preflight says %v", x.name, serveErr, it)
		}
	}
	// Read with the secrets as given, as --offline reads them: the same
	// reader, its token files unread.
	file := put("offline.yaml", "trust:\n  - {name: plain, token_file: "+filepath.Join(dir, "none")+"}\n")
	if _, err := serve.ReadTrust(file, nil, true); err != nil {
		t.Fatal(err)
	}
	rep, _, _ := pfReport(t, "--rules", rules, "--prometheus", "http://prometheus.example.com", "--offline", "--backtest-listen", ":8084", "--backtest-trust", file)
	if it := pfItems(rep)["backtest.trust"]; it["state"] != "unknown" {
		t.Errorf("%v", it)
	}
	// Firings copied in alone, with no rules: serve.New refuses the port.
	firings := put("firings-token", strings.Repeat("f", 40))
	trust := put("plain.yaml", "trust:\n  - {name: plain, token_file: "+token+"}\n")
	args := []string{"--firings-token-file", firings, "--incidents-listen", ":9091", "--prometheus", "http://prometheus.example.com", "--offline",
		"--backtest-listen", ":8084", "--backtest-trust", trust}
	st, err := readServe(args[:len(args)-5], true)
	if err != nil {
		t.Fatal(err)
	}
	st.cfg.BacktestListen, st.cfg.Trust = ":8084", []serve.Trust{{Name: "plain", TokenFile: token}}
	want := serve.BacktestRefused(st.cfg, true)
	rep, code, _ := pfReport(t, args...)
	if it := pfItems(rep)["backtest.trust"]; want == nil || it["state"] != "problem" || it["why"] != want.Error() || code != 1 {
		t.Errorf("exit %d: %v, want %v", code, it, want)
	}
}

// --offline with rules read from an API (--ruler, --grafana): the rules
// aren't read, so what a backtest of them asks is unknown, never ok.
func TestPreflightBacktestsRulesUnread(t *testing.T) {
	trust := write(t, "trust.yaml", "trust:\n  - {name: plain, token_file: /etc/lapilli/backtest/token}\n")
	for _, src := range [][]string{{"--ruler", "http://mimir.example.com/prometheus/config/v1/rules"},
		{"--grafana", "https://grafana.example.com", "--grafana-token-file", "/etc/lapilli/grafana-token"}} {
		rep, _, _ := pfReport(t, append(src, "--offline", "--prometheus", "https://prometheus.example.com", "--max-rate", "0.5",
			"--backtest-listen", ":8084", "--backtest-trust", trust)...)
		items := pfItems(rep)
		if items["rules.read"]["state"] != "unknown" || items["estimate.backtests"]["state"] != "unknown" ||
			!strings.Contains(items["estimate.backtests"]["why"].(string), "couldn't all be read") {
			t.Errorf("%s: %v %v", src[0], items["rules.read"], items["estimate.backtests"])
		}
	}
}

// An enterprise's issuer under GitHub's (GitHub Enterprise Cloud's,
// .../SLUG) is hidden by its slug, not by GitHub's host, which the public
// issuer beside it is on: that one is said as it is, with the scrubber
// the command has.
func TestPreflightBacktestsEnterpriseIssuer(t *testing.T) {
	was := now
	now = time.Now
	t.Cleanup(func() { now = was })
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keys := fmt.Sprintf(`{"keys":[{"kty":"RSA","kid":"k1","use":"sig","n":%q,"e":"AQAB"}]}`, base64.RawURLEncoding.EncodeToString(key.N.Bytes()))
	github := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "https://token.actions.githubusercontent.com" + strings.TrimSuffix(r.URL.Path, "/.well-known/openid-configuration")
		switch {
		case strings.HasSuffix(r.URL.Path, "/.well-known/openid-configuration"):
			fmt.Fprintf(w, `{"issuer":%q,"jwks_uri":%q,"id_token_signing_alg_values_supported":["RS256"]}`, strings.TrimSuffix(base, "/"), strings.TrimSuffix(base, "/")+"/.well-known/jwks")
		case strings.HasSuffix(r.URL.Path, "/.well-known/jwks"):
			fmt.Fprint(w, keys)
		default:
			http.NotFound(w, r)
		}
	})
	before := http.DefaultTransport
	http.DefaultTransport = &routed{next: before, hosts: map[string]http.Handler{"token.actions.githubusercontent.com": github}}
	t.Cleanup(func() { http.DefaultTransport = before })
	rules := write(t, "rules.yaml", "groups:\n  - name: g\n    rules:\n      - alert: Down\n        expr: up == 0\n")
	trust := write(t, "trust.yaml", `trust:
  - name: alerts
    issuer: https://token.actions.githubusercontent.com
    audience: https://lapilli-ci.example.com
    claims: {repository_id: "123456789", event_name: pull_request}
  - name: enterprise
    issuer: https://token.actions.githubusercontent.com/example-enterprise-slug
    audience: https://lapilli-ci.example.com
    claims: {repository_id: "987654321", event_name: pull_request}
`)
	rep, _, out := pfReport(t, "--rules", rules, "--prometheus", "http://127.0.0.1:1", "--ledger", t.TempDir(), "--check-content=false", "--check-replay=false",
		"--backtest-listen", ":8084", "--backtest-trust", trust)
	iss := pfItems(rep)["backtest.issuers"]
	what, _ := iss["what"].(string)
	if iss["state"] != "ok" || !strings.Contains(what, "https://token.actions.githubusercontent.com (alerts), 1 key for RS256") ||
		!strings.Contains(what, "[--backtest-trust's issuer 2] (enterprise), 1 key for RS256") {
		t.Errorf("%v", iss)
	}
	for _, hidden := range []string{"example-enterprise-slug", "https://[--backtest-trust"} {
		if strings.Contains(out, hidden) {
			t.Errorf("the report says %q:\n%s", hidden, out)
		}
	}
}
