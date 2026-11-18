package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/ledger/ledgertest"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/signin/idptest"
	"lapilli.dev/internal/verdict"
)

const btAudience = "https://lapilli-ci.example.com"

// btSet is a serve with the backtest port on, its trust file naming three
// fake issuers (a GitHub's, a GitLab's, a Forgejo's) and a plain token.
type btSet struct {
	s          *Server
	h          http.Handler
	gh, gl, fj *idptest.CI
	plain      string
	log        *lockedBuffer
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// btTrust is a trust file for the three issuers and the plain token's file.
func btTrust(gh, gl, fj *idptest.CI, plainFile, extra string) string {
	return fmt.Sprintf(`trust:
  - name: alerts
    issuer: %s
    ci: github
    audience: %s
    claims:
      repository_id: ["111", "123456789"]
      event_name: pull_request
  - name: monitoring-gitlab
    issuer: %s
    ci: gitlab
    audience: %s
    claims:
      project_id: "42"
      pipeline_source: merge_request_event
  - name: forgejo
    issuer: %s
    audience: %s
    days: 3
    claims:
      repository_id: 9007199254
      event_name: pull_request
  - name: plain
    token_file: %s
%s`, gh.Issuer, btAudience, gl.Issuer, btAudience, fj.Issuer, btAudience, plainFile, extra)
}

// btServer is serve with the port on, the rules read and a run recorded,
// asking h of the history (the tests' own when nil).
func btServer(t *testing.T, h replay.History, extra string, more func(*Config)) *btSet {
	t.Helper()
	clock := func() time.Time { return now }
	b := &btSet{gh: idptest.NewCI(t, "", clock), gl: idptest.NewCI(t, "", clock), fj: idptest.NewCI(t, "/api/actions", clock),
		plain: "plain-token-of-thirty-two-characters-or-more", log: &lockedBuffer{}}
	dir := t.TempDir()
	plainFile := filepath.Join(dir, "plain-token")
	os.WriteFile(plainFile, []byte(b.plain+"\n"), 0o600)
	trustFile := filepath.Join(dir, "trust.yaml")
	os.WriteFile(trustFile, []byte(btTrust(b.gh, b.gl, b.fj, plainFile, extra)), 0o600)
	trust, err := ReadTrust(trustFile, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	rulesDir := filepath.Join(dir, "rules")
	os.MkdirAll(rulesDir, 0o755)
	os.WriteFile(filepath.Join(rulesDir, "shop.yaml"), []byte(ruleFile), 0o644)
	incidents := filepath.Join(dir, "incidents.yaml")
	os.WriteFile(incidents, []byte("incidents:\n  - {id: INC-1, title: Checkout down, start: 2026-11-02T06:01:00Z, end: 2026-11-02T06:40:00Z, labels: {service: checkout}}\n"), 0o644)
	cfg := Config{
		RulesPath: rulesDir, Incidents: incidents,
		Options: verdict.Options{Lead: 15 * time.Minute, RefireWithin: time.Hour, UnknownLen: time.Hour, Match: []string{"service"}},
		Step:    time.Minute, Loc: time.UTC, Ledger: filepath.Join(dir, "ledger"), LedgerURLFile: ledgertest.File(t),
		Every: time.Hour, Settle: time.Hour, Backfill: 3 * 24 * time.Hour, Watch: time.Minute,
		Digest: Weekly{time.Monday, 9, 0}, Timeout: time.Minute, Parallel: 4, Listen: "127.0.0.1:0", Version: "test",
		BacktestListen: "127.0.0.1:0", Trust: trust, AllowedHosts: []string{"example.com"},
	}
	if more != nil {
		more(&cfg)
	}
	if h == nil {
		h = &history{}
	}
	s, err := New(context.Background(), cfg, h, slog.New(slog.NewTextHandler(b.log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.now = clock
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	b.s, b.h = s, s.BacktestHandler()
	return b
}

// github is a GitHub Actions job's token for pull request 42 of the
// repository 123456789, with claims changed.
func (b *btSet) github(change map[string]any) string {
	c := idptest.GitHub("123456789", "42")
	for k, v := range change {
		c[k] = v
	}
	return b.gh.Token(btAudience, c)
}

// changed are rules before and after a change of Errors's for, and a rule
// added.
const changedRules = `groups:
  - name: shop
    rules:
      - alert: Errors
        expr: errors > 5
        for: 10m
        labels: {severity: page}
      - alert: MoreErrors
        expr: errors > 1
        for: 5m
`

func btBody(before, after string, more map[string]any) []byte {
	m := map[string]any{"before": []map[string]string{{"path": "alerts/shop.yaml", "content": before}},
		"after": []map[string]string{{"path": "alerts/shop.yaml", "content": after}}, "commit": "0123abcd"}
	for k, v := range more {
		m[k] = v
	}
	out, _ := json.Marshal(m)
	return out
}

// btAsk sends a request to the port and gives its status and JSON.
func btAsk(t *testing.T, h http.Handler, method, path, token string, body []byte) (int, map[string]any, http.Header) {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var m map[string]any
	json.Unmarshal(w.Body.Bytes(), &m)
	return w.Code, m, w.Header()
}

// laneOnce runs the backtest waiting first, as the leader's lane does.
func laneOnce(t *testing.T, s *Server) ledger.Backtest {
	t.Helper()
	ctx := replay.Tracking(prom.Counting(context.Background(), &s.btPort.asked), &s.btPort.progress)
	b, body, ok, err := s.l.StartBacktest(ctx, s.now())
	if err != nil || !ok {
		t.Fatalf("nothing to run: %t %v", ok, err)
	}
	s.runBacktest(ctx, b, body)
	got, _, _ := s.l.Backtest(ctx, b.ID)
	return got
}

// done is a backtest asked for, run, and read back: its Markdown and JSON.
func (b *btSet) done(t *testing.T, token string, body []byte) (string, map[string]any) {
	t.Helper()
	code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", token, body)
	if code != http.StatusAccepted {
		t.Fatalf("POST: %d %v", code, m)
	}
	id, key := m["id"].(string), m["key"].(string)
	if got := laneOnce(t, b.s); got.State != ledger.BacktestDone {
		t.Fatalf("ran: %+v", got)
	}
	code, res, _ := btAsk(t, b.h, "GET", "/v1/backtests/"+id, key, nil)
	if code != http.StatusOK || res["state"] != "done" {
		t.Fatalf("GET: %d %v", code, res)
	}
	md, _ := res["markdown"].(string)
	rep, _ := res["report"].(map[string]any)
	return md, rep
}

// A job of each CI, its token as each writes its claims, gets a backtest:
// replayed with serve's access, judged by serve's record (INC-1 is the
// ledger's, not the job's), and the comment lapilli verdict --compare
// --changed-only --markdown writes, saying so, with its summary line.
func TestBacktestThreeCIs(t *testing.T) {
	b := btServer(t, nil, "", nil)
	for name, token := range map[string]string{
		"github":  b.github(nil),
		"gitlab":  b.gl.Token(btAudience, idptest.GitLab("42")),
		"forgejo": b.fj.Token(btAudience, idptest.Forgejo(9007199254)),
		"plain":   b.plain,
	} {
		md, rep := b.done(t, token, btBody(ruleFile, changedRules, map[string]any{"days": 3}))
		if !strings.HasPrefix(md, verdict.SummaryLine) || !strings.Contains(md, "judged by lapilli serve's record") {
			t.Errorf("%s: %s", name, md)
		}
		if !strings.Contains(md, "`Errors`") || !strings.Contains(md, "`MoreErrors`") || !strings.Contains(md, "`INC-1`") {
			t.Errorf("%s: the rules, or the ledger's incident, aren't said: %s", name, md)
		}
		sum, _ := rep["summary"].(map[string]any)
		if sum == nil || sum["incidents_given"] != true || sum["changed"] != float64(1) || sum["added"] != float64(1) || rep["judged_by"] != "lapilli serve" {
			t.Errorf("%s: %v", name, rep)
		}
	}
	bs, _ := b.s.l.Backtests(context.Background(), 10)
	repos := map[string]string{}
	for _, x := range bs {
		repos[x.Trust] = x.Repository
		if want := map[string]int64{"alerts": 42, "forgejo": 9}[x.Trust]; x.PullRequest != want {
			t.Errorf("%s: pull request %d, want %d, from its ref or none", x.Trust, x.PullRequest, want)
		}
	}
	if repos["alerts"] != "example/alerts" || repos["monitoring-gitlab"] != "monitoring/alerts" || repos["forgejo"] != "monitoring/alerts" || repos["plain"] != "plain" {
		t.Errorf("repositories kept: %v", repos)
	}
}

// What a token must be to be taken, each refused with the same 401 and no
// word of why: a pin exact (no prefix, a list only of the repository's
// id), its type a string or an integer; the audience the entry's; not a
// fork's event; not signed with HMAC by the issuer's public key; an issuer
// no entry names asks nothing of anyone. The body of a request refused is
// never read.
func TestBacktestTokensRefused(t *testing.T) {
	b := btServer(t, nil, "", nil)
	// A token of an issuer no entry names asks nothing of anyone: before
	// any token, each issuer still unasked.
	asked := func() int64 {
		var n int64
		for _, ci := range []*idptest.CI{b.gh, b.gl, b.fj} {
			n += ci.Discoveries.Load() + ci.KeyFetches.Load()
		}
		return n
	}
	for range 20 {
		btAsk(t, b.h, "POST", "/v1/backtests", signedAs(b.gh, "https://token.actions.example.com"), nil)
	}
	if n := asked(); n != 0 {
		t.Errorf("20 tokens of an issuer no entry names asked the issuers %d times", n)
	}
	fork := idptest.GitLab("77") // a fork's merge request: its project's id
	for name, token := range map[string]string{
		"none":                      "",
		"another repository":        b.github(map[string]any{"repository_id": "999"}),
		"a prefix of the pin":       b.github(map[string]any{"repository_id": "12345678"}),
		"the pin's prefix":          b.github(map[string]any{"repository_id": "1234567890"}),
		"the pin as a list":         b.github(map[string]any{"repository_id": []string{"123456789"}}),
		"the pin as an object":      b.github(map[string]any{"repository_id": map[string]string{"id": "123456789"}}),
		"pull_request_target":       b.github(map[string]any{"event_name": "pull_request_target"}),
		"workflow_run":              b.github(map[string]any{"event_name": "workflow_run"}),
		"no event":                  b.github(map[string]any{"event_name": nil}),
		"a cloud's audience":        b.gh.Token("sts.amazonaws.com", idptest.GitHub("123456789", "42")),
		"the owner's URL":           b.gh.Token("https://github.com/example", idptest.GitHub("123456789", "42")),
		"a fork's merge request":    b.gl.Token(btAudience, fork),
		"a GitLab push of no MR":    b.gl.Token(btAudience, map[string]any{"project_id": "42", "pipeline_source": "web"}),
		"an hour and more old":      b.github(map[string]any{"iat": now.Add(-61 * time.Minute).Unix(), "exp": now.Add(time.Hour).Unix()}),
		"HS256 by the public key":   hmacToken(b.gh, idptest.GitHub("123456789", "42")),
		"another issuer":            signedAs(b.gh, "https://token.actions.example.com"),
		"a plain token almost":      b.plain[:len(b.plain)-1],
		"the plain token, and more": b.plain + "x",
		"a Forgejo id with a point": b.fj.Token(btAudience, map[string]any{"repository_id": 9007199254.5, "event_name": "pull_request"}),
	} {
		read := &countingReader{r: bytes.NewReader(btBody(ruleFile, changedRules, nil))}
		r := httptest.NewRequest("POST", "/v1/backtests", read)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		b.h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != "Bearer" || strings.TrimSpace(w.Body.String()) != `{"error":"unauthorized"}` {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
		if read.n > 0 {
			t.Errorf("%s: the body was read before the token verified", name)
		}
	}
	// Each issuer was asked once for its keys, by all the tokens of it.
	for _, ci := range []*idptest.CI{b.gh, b.gl, b.fj} {
		if ci.Discoveries.Load() != 1 {
			t.Errorf("%s discovered %d times", ci.Issuer, ci.Discoveries.Load())
		}
	}
	// A request with an Origin, as a page would send, is refused whatever
	// its token.
	r := httptest.NewRequest("POST", "/v1/backtests", bytes.NewReader(btBody(ruleFile, changedRules, nil)))
	r.Header.Set("Authorization", "Bearer "+b.github(nil))
	r.Header.Set("Origin", "https://example.com")
	w := httptest.NewRecorder()
	b.h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("with an Origin: %d", w.Code)
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// hmacToken is a token signed with HS256 by the issuer's public key, as a
// verifier that takes the key set's key as a secret would take it.
func hmacToken(ci *idptest.CI, claims map[string]any) string {
	c := map[string]any{"iss": ci.Issuer, "aud": btAudience, "iat": now.Unix(), "exp": now.Add(time.Minute).Unix()}
	for k, v := range claims {
		c[k] = v
	}
	return idptest.Sign(ci.Key.PublicKey.N.Bytes(), ci.KID, "HS256", c)
}

func signedAs(ci *idptest.CI, iss string) string {
	c := idptest.GitHub("123456789", "42")
	c["iss"] = iss
	return ci.Token(btAudience, c)
}

// A token starts one backtest: the same body again, as a job asked twice,
// is the same backtest, answered 200 without its key, which was given
// once; another body is 409. A plain token's each start one.
func TestBacktestTokenOnce(t *testing.T) {
	b := btServer(t, nil, "", nil)
	tok := b.github(nil)
	body := btBody(ruleFile, changedRules, nil)
	code, first, _ := btAsk(t, b.h, "POST", "/v1/backtests", tok, body)
	if code != http.StatusAccepted || first["key"] == nil {
		t.Fatalf("%d %v", code, first)
	}
	code, again, _ := btAsk(t, b.h, "POST", "/v1/backtests", tok, body)
	if code != http.StatusOK || again["id"] != first["id"] || again["key"] != nil {
		t.Errorf("the same body again: %d %v", code, again)
	}
	code, other, _ := btAsk(t, b.h, "POST", "/v1/backtests", tok, btBody(ruleFile, changedRules, map[string]any{"days": 2}))
	if code != http.StatusConflict {
		t.Errorf("another body: %d %v", code, other)
	}
	for range 2 {
		if code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", b.plain, btBody(ruleFile, changedRules, map[string]any{"pull_request": 5})); code != http.StatusAccepted {
			t.Errorf("a plain token: %d %v", code, m)
		}
	}
}

// What a request may be: read strictly, 4 MiB at most; the period, the
// zone and the numbers within their bounds; Prometheus-format rules, 50
// that differ at most, a for of 48h and a group interval of an hour at
// most; each selector one metric's, by its name. Each is a 400 with what
// is wrong, the request's own words; none is queued.
func TestBacktestRequestsRefused(t *testing.T) {
	b := btServer(t, nil, "", nil)
	many := "groups:\n  - name: many\n    rules:\n"
	for i := range 51 {
		many += fmt.Sprintf("      - {alert: R%d, expr: errors > %d}\n", i, i)
	}
	grafana := "apiVersion: 1\ngroups:\n  - orgId: 1\n    name: g\n    folder: f\n    interval: 1m\n    rules:\n      - uid: u1\n        title: G\n        condition: A\n        data:\n          - refId: A\n            datasourceUid: prom\n            model: {expr: 'up == 0'}\n"
	for name, c := range map[string]struct {
		body []byte
		code int
		says string
	}{
		"past 4 MiB":         {btBody(ruleFile, strings.Repeat("#", 4<<20), nil), 413, "4 MiB"},
		"a field unknown":    {btBody(ruleFile, changedRules, map[string]any{"prometheus": "http://elsewhere"}), 400, "unknown field"},
		"two objects":        {append(btBody(ruleFile, changedRules, nil), []byte(`{}`)...), 400, "one JSON object"},
		"15 days":            {btBody(ruleFile, changedRules, map[string]any{"days": 15}), 400, "1 to 14"},
		"Local":              {btBody(ruleFile, changedRules, map[string]any{"tz": "Local"}), 400, "zone"},
		"a zone made up":     {btBody(ruleFile, changedRules, map[string]any{"tz": "Mars/Olympus"}), 400, "zone"},
		"a long zone":        {btBody(ruleFile, changedRules, map[string]any{"tz": strings.Repeat("A", 65)}), 400, "zone"},
		"a commit":           {btBody(ruleFile, changedRules, map[string]any{"commit": "main; rm -rf"}), 400, "commit"},
		"another PR":         {btBody(ruleFile, changedRules, map[string]any{"pull_request": 41}), 400, "ref"},
		"a Grafana rule":     {btBody(ruleFile, grafana, nil), 400, "Prometheus's format"},
		"51 rules":           {btBody(ruleFile, many, nil), 400, "50 rules"},
		"for 49h":            {btBody(ruleFile, strings.Replace(changedRules, "for: 10m", "for: 49h", 1), nil), 400, "48h"},
		"interval 2h":        {btBody(ruleFile, strings.Replace(changedRules, "- name: shop\n", "- name: shop\n    interval: 2h\n", 1), nil), 400, "interval"},
		"no metric":          {btBody(ruleFile, strings.Replace(changedRules, "errors > 1", `'{job="api"} > 1'`, 1), nil), 400, `{job="api"}`},
		"__name__ =~":        {btBody(ruleFile, strings.Replace(changedRules, "errors > 1", `'{__name__=~"secret_.+"} > 41'`, 1), nil), 400, "names no metric"},
		"a subquery of 1s":   {btBody(ruleFile, strings.Replace(changedRules, "errors > 1", `max_over_time(errors[30d:1s]) > 1`, 1), nil), 400, "a range past"},
		"offset 5y":          {btBody(ruleFile, strings.Replace(changedRules, "errors > 1", `errors offset 5y > 1`, 1), nil), 400, "offset 5y"},
		"@ 0":                {btBody(ruleFile, strings.Replace(changedRules, "errors > 1", `errors @ 0 > 1`, 1), nil), 400, "no @"},
		"expect_before only": {btBody(ruleFile, changedRules, map[string]any{"expect_before": "expectations: []"}), 400, "expect_before goes with expect"},
		"expect broken":      {btBody(ruleFile, changedRules, map[string]any{"expect": "expectations: [{rule: Errors, nope: 1}]"}), 400, "expect"},
		"a path control":     {[]byte(`{"before":[],"after":[{"path":"a\u0007b.yaml","content":"groups: []"}]}`), 400, "path"},
		"nothing":            {[]byte(`{}`), 400, "before, after"},
	} {
		code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", b.github(nil), c.body)
		why, _ := m["error"].(string)
		if code != c.code || !strings.Contains(why, c.says) {
			t.Errorf("%s: %d %q, want %d %q", name, code, why, c.code, c.says)
		}
	}
	// The forgejo entry backtests 3 days at most.
	if code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", b.fj.Token(btAudience, idptest.Forgejo(9007199254)), btBody(ruleFile, changedRules, map[string]any{"days": 4})); code != 400 || !strings.Contains(m["error"].(string), "3 at most") {
		t.Errorf("past the entry's days: %d %v", code, m)
	}
	if n, _ := b.s.l.BacktestsWaiting(context.Background()); len(n) != 0 {
		t.Errorf("queued: %v", n)
	}
}

// A backtest that couldn't finish within --backtest-timeout at the lane's
// rate is refused before it is queued, saying how many requests it needs
// and how many days would fit.
func TestBacktestPlanRefused(t *testing.T) {
	b := btServer(t, nil, "", func(c *Config) { c.BacktestTimeout = time.Minute })
	b.s.btPort.rate = 0.05 // 3 requests a minute
	code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", b.github(nil), btBody(ruleFile, changedRules, map[string]any{"days": 7}))
	why, _ := m["error"].(string)
	if code != 400 || !strings.Contains(why, "requests of the history") || !strings.Contains(why, "--backtest-timeout 1m") {
		t.Errorf("%d %q", code, why)
	}
	b.s.btPort.rate = 0.25 // 15 a minute: a day or two fits
	code, m, _ = btAsk(t, b.h, "POST", "/v1/backtests", b.github(map[string]any{"jti": "other"}), btBody(ruleFile, changedRules, map[string]any{"days": 7}))
	if why, _ = m["error"].(string); code != 400 || !strings.Contains(why, "days would fit") {
		t.Errorf("%d %q", code, why)
	}
}

// The queue: 3 of an entry, --backtest-queue in all, one of a pull
// request (a newer commit's supersedes the one waiting), each answered
// 429 with Retry-After past them; an entry's hourly, from its bucket.
func TestBacktestQueueBounds(t *testing.T) {
	b := btServer(t, nil, "", func(c *Config) { c.BacktestQueue = 5 })
	post := func(token string, pr int) (int, map[string]any, http.Header) {
		return btAsk(t, b.h, "POST", "/v1/backtests", token, btBody(ruleFile, changedRules, map[string]any{"pull_request": pr}))
	}
	for pr := 1; pr <= 3; pr++ {
		if code, m, _ := post(b.plain, pr); code != 202 || m["position"] != float64(pr-1) {
			t.Fatalf("%d: %d %v", pr, code, m)
		}
	}
	code, _, hd := post(b.plain, 4)
	if code != 429 || hd.Get("Retry-After") == "" {
		t.Errorf("a fourth of the entry's: %d %v", code, hd)
	}
	// A plain token's pull request is the job's word: it supersedes no
	// backtest waiting, the entry's third.
	if code, _, _ := post(b.plain, 2); code != 429 || b.s.btPort.ended[[2]string{"plain", ledger.BacktestSuperseded}] != 0 {
		t.Errorf("a plain token's pull request 2 again: %d", code)
	}
	// A newer commit of GitHub's pull request 50, by its token's ref,
	// takes the place of the one waiting.
	for i := range 2 {
		tok := b.github(map[string]any{"jti": fmt.Sprint("s", i), "ref": "refs/pull/50/merge"})
		if code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", tok, btBody(ruleFile, changedRules, nil)); code != 202 || m["position"] != float64(3) {
			t.Errorf("pull request 50, the %dth: %d %v", i+1, code, m)
		}
	}
	if n := b.s.btPort.ended[[2]string{"alerts", ledger.BacktestSuperseded}]; n != 1 {
		t.Errorf("superseded counted %d", n)
	}
	// Past --backtest-queue in all: 4 wait, the fifth fits.
	for i, want := range []int{202, 429} {
		tok := b.github(map[string]any{"jti": fmt.Sprint("q", i), "ref": fmt.Sprintf("refs/pull/%d/merge", 60+i)})
		code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", tok, btBody(ruleFile, changedRules, nil))
		if code != want {
			t.Errorf("%d in the queue: %d %v, want %d", i+4, code, m, want)
		}
	}
	// An entry's hourly, from its bucket: two an hour, the third 429.
	b2 := btServer(t, nil, "", nil)
	t2 := b2.s.trustNamed("monitoring-gitlab")
	t2.hourly, t2.Hourly = &limiter{rate: 2.0 / 3600, burst: 2}, 2
	for i, want := range []int{202, 202, 429} {
		tok := b2.gl.Token(btAudience, idptest.GitLab("42"))
		code, m, hd := btAsk(t, b2.h, "POST", "/v1/backtests", tok, btBody(ruleFile, changedRules, map[string]any{"pull_request": i + 1}))
		if code != want || want == 429 && hd.Get("Retry-After") != "1800" {
			t.Errorf("the %dth in the hour: %d %v %v", i+1, code, m, hd)
		}
	}
	// Past the hour, the body isn't read.
	read := &countingReader{r: bytes.NewReader(btBody(ruleFile, changedRules, nil))}
	r := httptest.NewRequest("POST", "/v1/backtests", read)
	r.Header.Set("Authorization", "Bearer "+b2.gl.Token(btAudience, idptest.GitLab("42")))
	w := httptest.NewRecorder()
	b2.h.ServeHTTP(w, r)
	if w.Code != 429 || read.n != 0 {
		t.Errorf("past the hour: %d, %d bytes read", w.Code, read.n)
	}
	// What is refused before it is queued gives its hour back.
	b3 := btServer(t, nil, "", nil)
	t3 := b3.s.trustNamed("plain")
	t3.hourly, t3.Hourly = &limiter{rate: 1.0 / 3600, burst: 1}, 1
	for range 3 {
		if code, _, _ := btAsk(t, b3.h, "POST", "/v1/backtests", b3.plain, btBody(ruleFile, changedRules, map[string]any{"days": 15})); code != 400 {
			t.Errorf("a bad request: %d", code)
		}
	}
	if code, m, _ := btAsk(t, b3.h, "POST", "/v1/backtests", b3.plain, btBody(ruleFile, changedRules, nil)); code != 202 {
		t.Errorf("after three refused, the hour's one: %d %v", code, m)
	}
	// As many bodies being read as the port reads at once: refused, the
	// body unread.
	for range cap(b3.s.btPort.reading) {
		b3.s.btPort.reading <- struct{}{}
	}
	read = &countingReader{r: bytes.NewReader(btBody(ruleFile, changedRules, nil))}
	r = httptest.NewRequest("POST", "/v1/backtests", read)
	r.Header.Set("Authorization", "Bearer "+b3.github(nil))
	w = httptest.NewRecorder()
	b3.h.ServeHTTP(w, r)
	if w.Code != 429 || read.n != 0 {
		t.Errorf("the reads full: %d, %d bytes read", w.Code, read.n)
	}
}

// The lane's share of --max-rate: its own Limiter, paced at the share and
// by the runs' pace too; the runs' Limiter as it was. Without --max-rate,
// a quarter of --parallel at once, one at least.
func TestBacktestLaneShare(t *testing.T) {
	runs := prom.NewLimiter(2)
	c := prom.New("http://127.0.0.1:1", "test", prom.WithLimiter(runs))
	b := btServer(t, c, "", func(cfg *Config) { cfg.BacktestShare = 0.25 })
	lane, ok := b.s.btPort.lane.(*prom.Client)
	if !ok {
		t.Fatalf("the lane asks %T", b.s.btPort.lane)
	}
	l := lane.Limiter()
	if l == runs || c.Limiter() != runs || l.Pace().Rate() != 0.5 || runs.Pace().Reserved() != 0.25 || b.s.btPort.rate != 0.5 {
		t.Errorf("the lane's Limiter: own %g; the runs' reserving %g; rate %g", l.Pace().Rate(), runs.Pace().Reserved(), b.s.btPort.rate)
	}
	none := btServer(t, prom.New("http://127.0.0.1:1", "test"), "", func(cfg *Config) { cfg.Parallel = 2 })
	if none.s.btPort.rate != 0 || none.s.btPort.parallel != 1 {
		t.Errorf("no --max-rate: rate %g, %d at once", none.s.btPort.rate, none.s.btPort.parallel)
	}
}

// leakyHistory answers as the tests' history does, and for three rules
// as a pull request could write them to read what serve's access reads:
// an alert's labels carrying a series' values, the history's refusal
// echoing a label's value, and an error that says where the history is.
type leakyHistory struct{ history }

const leak = "SECRET-7c1e"

func (h *leakyHistory) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	switch {
	case strings.HasPrefix(expr, "leaky_metric"):
		var s prom.Series
		s.Labels = map[string]string{"service": "checkout", "secret": leak, "severity": leak + "-sev", "instance": "db." + leak + ".internal"}
		for t := now.Add(-30 * time.Hour); t.Before(now.Add(-29 * time.Hour)); t = t.Add(step) {
			if !t.Before(start) && !t.After(end) {
				s.Samples = append(s.Samples, prom.Sample{T: t, V: 1})
			}
		}
		return []prom.Series{s}, nil
	case strings.HasPrefix(expr, "refused_metric"):
		return nil, &prom.APIError{Status: 400, Msg: `bad_data: found duplicate series for the match group {instance="` + leak + `"}`}
	case strings.HasPrefix(expr, "unread_metric"):
		return nil, fmt.Errorf("Post \"http://prometheus." + leak + ".svc:9090/api/v1/query_range\": dial tcp 10.0.0.7:9090: connection refused")
	}
	return h.history.QueryRange(ctx, expr, start, end, step)
}

// Nothing of what serve's access reads, beyond counts, reaches the job, the
// log, /metrics or the Run history: no series' label (an alert's labels,
// its severity among them), no word the history said, nothing of where it
// is; nor a token, a key, or a rule's name outside the comment.
func TestBacktestNoLeaks(t *testing.T) {
	b := btServer(t, &leakyHistory{}, "", nil)
	after := ruleFile + `      - alert: LeakyRule
        expr: leaky_metric > 0
      - alert: RefusedRule
        expr: refused_metric > 0
      - alert: UnreadRule
        expr: unread_metric > 0
`
	tok := b.github(nil)
	code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", tok, btBody(ruleFile, after, nil))
	if code != 202 {
		t.Fatal(code, m)
	}
	key := m["key"].(string)
	laneOnce(t, b.s)
	r := httptest.NewRequest("GET", "/v1/backtests/"+m["id"].(string), nil)
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	b.h.ServeHTTP(w, r)
	got := w.Body.String()
	if !strings.Contains(got, `"state":"done"`) || !strings.Contains(got, "LeakyRule") {
		t.Fatalf("%s", got)
	}
	_, metrics, _ := get(t, b.s.Handler(), "/metrics")
	_, runs, _ := get(t, b.s.Handler(), "/runs")
	for where, text := range map[string]string{"the result": got, "the log": b.log.String(), "/metrics": metrics, "the Run history": runs} {
		for _, bad := range []string{leak, "10.0.0.7", "duplicate series", "dial tcp"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s holds %q", where, bad)
			}
		}
	}
	for where, text := range map[string]string{"the log": b.log.String(), "/metrics": metrics, "the Run history": runs} {
		for _, bad := range []string{tok, key, "LeakyRule", "RefusedRule", "UnreadRule"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s holds %q", where, bad)
			}
		}
	}
	for _, want := range []string{"query failed: the history refused it as written", unreadByServe} {
		if !strings.Contains(got, want) {
			t.Errorf("the result doesn't say %q", want)
		}
	}
	// The JSON holds no firing, no incident of a side, no setting it was
	// judged by: the counts, the names the request wrote, and the
	// incidents caught differently by their ids alone.
	var res struct {
		Markdown string
		Report   map[string]any
	}
	json.Unmarshal([]byte(got), &res)
	for _, side := range []string{"before", "after"} {
		rep := res.Report[side].(map[string]any)
		for _, k := range []string{"firings", "incidents", "settings", "noisy_at", "hours", "impact_short"} {
			if _, ok := rep[k]; ok {
				t.Errorf("the report's %s has %s", side, k)
			}
		}
	}
	incs, _ := res.Report["incidents"].([]any)
	if len(incs) == 0 {
		t.Fatal("no incident caught differently")
	}
	for _, x := range incs {
		for k := range x.(map[string]any) {
			if k != "id" && k != "start" && k != "before" && k != "after" {
				t.Errorf("an incident caught differently has %s", k)
			}
		}
	}
	if strings.Contains(res.Markdown, "Checkout down") || strings.Contains(got, "Checkout down") {
		t.Error("an incident's title is in the result, the entry not asking for it")
	}
}

// A plain token another token file of serve's comes to hold, after start,
// is taken no longer, until each has its own again.
func TestBacktestPlainTokenShared(t *testing.T) {
	dir := t.TempDir()
	incidents := filepath.Join(dir, "incidents-token")
	os.WriteFile(incidents, []byte(strings.Repeat("i", 32)), 0o600)
	b := btServer(t, nil, "", func(c *Config) { c.IncidentsListen, c.IncidentsToken = "127.0.0.1:0", incidents })
	if code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", b.plain, btBody(ruleFile, changedRules, nil)); code != 202 {
		t.Fatal(code, m)
	}
	os.WriteFile(incidents, []byte(b.plain), 0o600)
	if code, _, _ := btAsk(t, b.h, "POST", "/v1/backtests", b.plain, btBody(ruleFile, changedRules, map[string]any{"pull_request": 3})); code != 401 {
		t.Errorf("a plain token the incidents token file holds too: %d", code)
	}
}

// A job reads its backtest with its key alone: an unknown id and a wrong
// key are the same 404. It cancels one waiting, which ends so, and one
// running, which stops at the lane's next look.
func TestBacktestReadAndCancel(t *testing.T) {
	was := backtestPoll
	backtestPoll = 10 * time.Millisecond
	t.Cleanup(func() { backtestPoll = was })
	b := btServer(t, &blockingHistory{asked: make(chan struct{})}, "", nil)
	_, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", b.plain, btBody(ruleFile, changedRules, map[string]any{"pull_request": 1}))
	id, key := m["id"].(string), m["key"].(string)
	c1, wrong, _ := btAsk(t, b.h, "GET", "/v1/backtests/"+id, key+"x", nil)
	c2, unknown, _ := btAsk(t, b.h, "GET", "/v1/backtests/"+strings.Repeat("0", 32), key, nil)
	c3, waiting, _ := btAsk(t, b.h, "GET", "/v1/backtests/"+id, key, nil)
	if c1 != 404 || c2 != 404 || fmt.Sprint(wrong) != fmt.Sprint(unknown) || c3 != 202 || waiting["state"] != "waiting" || waiting["position"] != float64(0) {
		t.Errorf("%d %v / %d %v / %d %v", c1, wrong, c2, unknown, c3, waiting)
	}
	if code, _, _ := btAsk(t, b.h, "DELETE", "/v1/backtests/"+id, key, nil); code != 204 {
		t.Errorf("DELETE: %d", code)
	}
	if _, m, _ := btAsk(t, b.h, "GET", "/v1/backtests/"+id, key, nil); m["state"] != "cancelled" {
		t.Errorf("cancelled waiting: %v", m)
	}
	// One running, cancelled: the lane stops it.
	_, m, _ = btAsk(t, b.h, "POST", "/v1/backtests", b.plain, btBody(ruleFile, changedRules, map[string]any{"pull_request": 2}))
	id, key = m["id"].(string), m["key"].(string)
	ran := make(chan ledger.Backtest)
	go func() {
		ctx := replay.Tracking(prom.Counting(context.Background(), &b.s.btPort.asked), &b.s.btPort.progress)
		bt, body, _, _ := b.s.l.StartBacktest(ctx, b.s.now())
		b.s.runBacktest(ctx, bt, body)
		got, _, _ := b.s.l.Backtest(ctx, bt.ID)
		ran <- got
	}()
	h := b.s.h.(*blockingHistory)
	<-h.asked
	if code, _, _ := btAsk(t, b.h, "DELETE", "/v1/backtests/"+id, key, nil); code != 204 {
		t.Errorf("DELETE running: %d", code)
	}
	if got := <-ran; got.State != ledger.BacktestCancelled {
		t.Errorf("cancelled running: %+v", got)
	}
}

// blockingHistory answers once asked by the lane only when its context
// ends, as a slow history does; it says when it is asked.
type blockingHistory struct {
	history
	once  sync.Once
	asked chan struct{}
}

func (h *blockingHistory) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if prom.CountsOf(ctx) == nil || expr != "errors > 1" {
		return h.history.QueryRange(ctx, expr, start, end, step)
	}
	h.once.Do(func() { close(h.asked) })
	<-ctx.Done()
	return nil, ctx.Err()
}

// /metrics counts the backtests by entry and how they ended, and those
// waiting; the Run history lists each, counts and the repository.
func TestBacktestShown(t *testing.T) {
	b := btServer(t, nil, "", nil)
	b.done(t, b.github(nil), btBody(ruleFile, changedRules, nil))
	btAsk(t, b.h, "POST", "/v1/backtests", b.github(map[string]any{"jti": "2"}), btBody(ruleFile, changedRules, map[string]any{"days": 15}))
	btAsk(t, b.h, "POST", "/v1/backtests", b.plain, btBody(ruleFile, changedRules, nil))
	_, metrics, _ := get(t, b.s.Handler(), "/metrics")
	for _, want := range []string{
		`lapilli_serve_backtests_total{trust="alerts",result="done"} 1`,
		`lapilli_serve_backtests_total{trust="alerts",result="refused"} 1`,
		`lapilli_serve_backtests_waiting{trust="plain"} 1`,
		`lapilli_serve_backtests_waiting{trust="alerts"} 0`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
	_, runs, _ := get(t, b.s.Handler(), "/runs")
	if !strings.Contains(runs, "Pull request backtests") || !strings.Contains(runs, "example/alerts") || strings.Count(runs, "<td>alerts</td>") != 1 {
		t.Errorf("the Run history: %s", runs)
	}
}

// Without --backtest-listen, nothing of the port: no section on the Run
// history, no series in /metrics, no lane; with it and nothing asked, the
// Run history is the page without it, but for its section.
func TestBacktestOffUnchanged(t *testing.T) {
	off, _ := server(t, nil)
	if off.btPort != nil {
		t.Fatal("a backtest port without --backtest-listen")
	}
	ctx := context.Background()
	raw := ""
	off.look(ctx, &raw)
	off.jobs(ctx, true)
	_, metrics, _ := get(t, off.Handler(), "/metrics")
	_, runs, _ := get(t, off.Handler(), "/runs")
	if strings.Contains(metrics, "backtests") || strings.Contains(runs, "Pull request backtests") || strings.Contains(runs, `id="backtests"`) {
		t.Error("the port shows without --backtest-listen")
	}
	on := btServer(t, nil, "", nil)
	_, onRuns, _ := get(t, on.s.Handler(), "/runs")
	start := strings.Index(onRuns, `<h2 id="backtests">`)
	end := strings.Index(onRuns[start:], "</table></div>")
	if start < 0 || end < 0 {
		t.Fatalf("no section: %s", onRuns)
	}
	if !strings.Contains(onRuns[start:start+end], "None yet.") {
		t.Error("an empty queue isn't said")
	}
	// The same server without the port, its section taken out of the
	// page with it: the pages are the same to the byte.
	alike := btServer(t, nil, "", func(c *Config) { c.BacktestListen, c.Trust = "", nil })
	if alike.s.btPort != nil {
		t.Fatal("a port without --backtest-listen")
	}
	_, offRuns, _ := get(t, alike.s.Handler(), "/runs")
	section := onRuns[start : start+end+len("</table></div>")+1]
	if strings.Replace(onRuns, section, "", 1) != offRuns {
		t.Errorf("the Run history differs but for the section:\n%s\n----\n%s", strings.Replace(onRuns, section, "", 1), offRuns)
	}
	_, onMetrics, _ := get(t, on.s.Handler(), "/metrics")
	_, offMetrics, _ := get(t, alike.s.Handler(), "/metrics")
	var kept []string
	for _, line := range strings.SplitAfter(onMetrics, "\n") {
		if !strings.Contains(line, "lapilli_serve_backtests_") {
			kept = append(kept, line)
		}
	}
	if strings.Join(kept, "") != offMetrics {
		t.Error("/metrics differs but for the backtests' series")
	}
}

// A backtest of a large rule set, rules named in Korean and Cyrillic,
// as teams write them, goes through and is said as written.
func TestBacktestOrdinary(t *testing.T) {
	b := btServer(t, nil, "", nil)
	var same strings.Builder
	same.WriteString("groups:\n  - name: 결제\n    rules:\n")
	for i := range 1500 {
		fmt.Fprintf(&same, "      - {alert: 결제실패_%d, expr: errors > %d, for: 5m}\n", i, 1000+i)
	}
	before := ruleFile + "      - alert: ошибка\n        expr: errors > 5\n        for: 1m\n"
	after := ruleFile + "      - alert: ошибка\n        expr: errors > 5\n        for: 20m\n      - alert: 결제실패\n        expr: errors > 1\n"
	body, _ := json.Marshal(map[string]any{
		"before": []map[string]string{{"path": "alerts/shop.yaml", "content": before}, {"path": "alerts/결제.yaml", "content": same.String()}},
		"after":  []map[string]string{{"path": "alerts/shop.yaml", "content": after}, {"path": "alerts/결제.yaml", "content": same.String()}},
		"tz":     "Asia/Seoul",
	})
	md, rep := b.done(t, b.gl.Token(btAudience, idptest.GitLab("42")), body)
	if !strings.Contains(md, "`ошибка`") || !strings.Contains(md, "`결제실패`") || !strings.Contains(md, "Asia/Seoul") {
		t.Errorf("%s", md)
	}
	if sum := rep["summary"].(map[string]any); sum["same"] != float64(1501) || sum["changed"] != float64(1) || sum["added"] != float64(1) {
		t.Errorf("%v", sum)
	}
}

// Replicas sharing a Postgres ledger share the queue and each entry's
// hour: a backtest asked of one is run by another's lane and read through
// the one asked, and an entry's hourly counts what every replica took.
func TestBacktestReplicas(t *testing.T) {
	shared := sharedLedger(t)
	a := btServer(t, nil, "", func(c *Config) { c.LedgerURLFile = shared })
	b := btServer(t, nil, "", func(c *Config) { c.LedgerURLFile = shared })
	for _, x := range []*btSet{a, b} {
		h := x.s.trustNamed("plain").hourly
		h.mu.Lock()
		h.rate, h.burst = 2.0/3600, 2
		h.mu.Unlock()
	}
	var asked []map[string]any
	for i, x := range []*btSet{a, b, a} {
		code, m, _ := btAsk(t, x.h, "POST", "/v1/backtests", x.plain, btBody(ruleFile, changedRules, map[string]any{"pull_request": i + 1}))
		if want := []int{202, 202, 429}[i]; code != want {
			t.Fatalf("the %dth, of replica %d: %d %v, want %d", i+1, map[*btSet]int{a: 1, b: 2}[x], code, m, want)
		}
		asked = append(asked, m)
	}
	laneOnce(t, a.s)
	if got := laneOnce(t, a.s); got.ID != asked[1]["id"] || got.State != ledger.BacktestDone {
		t.Fatalf("the one asked of b, run by a: %+v", got)
	}
	if code, m, _ := btAsk(t, b.h, "GET", "/v1/backtests/"+asked[1]["id"].(string), asked[1]["key"].(string), nil); code != 200 || m["state"] != "done" {
		t.Errorf("read through b: %d %v", code, m)
	}
}

// A pull request that changes nothing that decides when a rule fires,
// expectations sent beside it, is answered as lapilli verdict answers it:
// nothing changed, nothing asked of the history.
func TestBacktestNothingChanged(t *testing.T) {
	h := &history{}
	b := btServer(t, h, "", nil)
	same := strings.Replace(ruleFile, "labels: {severity: page}", "labels: {severity: page}\n        annotations: {summary: still the same}", 1)
	h.Lock()
	before := h.asked
	h.Unlock()
	md, rep := b.done(t, b.plain, btBody(ruleFile, same, map[string]any{"expect": ""}))
	// As lapilli verdict's summary says it then: nothing held.
	if sum := rep["summary"].(map[string]any); !strings.Contains(md, verdict.NothingChangedHeading) || sum["nothing_changed"] != true || sum["expected"] != false {
		t.Errorf("%s", md)
	}
	h.Lock()
	defer h.Unlock()
	if n := h.asked - before; n != 0 {
		t.Errorf("asked the history %d times for nothing that changed", n)
	}
}

// An expectations file of no bytes is given, as --expect takes one: held
// to it, the summary says it was.
func TestBacktestEmptyExpect(t *testing.T) {
	b := btServer(t, nil, "", nil)
	_, rep := b.done(t, b.plain, btBody(ruleFile, changedRules, map[string]any{"expect": ""}))
	if sum := rep["summary"].(map[string]any); sum["expected"] != true {
		t.Errorf("an empty expect: %v", sum)
	}
	_, rep = b.done(t, b.plain, btBody(ruleFile, changedRules, nil))
	if sum := rep["summary"].(map[string]any); sum["expected"] != false {
		t.Errorf("no expect: %v", sum)
	}
}

// An entry that asks for the incidents' titles (incident_titles) gets them
// in its comments; others, ids alone.
func TestBacktestIncidentTitles(t *testing.T) {
	b := btServer(t, nil, "", nil)
	b.s.trustNamed("plain").IncidentTitles = true
	md, rep := b.done(t, b.plain, btBody(ruleFile, changedRules, nil))
	if !strings.Contains(md, "`Checkout down`") || !strings.Contains(fmt.Sprint(rep["incidents"]), "Checkout down") {
		t.Errorf("titles asked for: %s", md)
	}
	md, _ = b.done(t, b.github(nil), btBody(ruleFile, changedRules, nil))
	if strings.Contains(md, "Checkout down") || !strings.Contains(md, "`INC-1`") {
		t.Errorf("titles not asked for: %s", md)
	}
}

// A backtest that makes the lane panic ends failed, and the lane goes on.
func TestBacktestPanicFails(t *testing.T) {
	b := btServer(t, nil, "", nil)
	laneHook = func(ledger.Backtest) { panic("made to") }
	t.Cleanup(func() { laneHook = nil })
	code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", b.plain, btBody(ruleFile, changedRules, nil))
	if code != 202 {
		t.Fatal(code, m)
	}
	if got := laneOnce(t, b.s); got.State != ledger.BacktestFailed || got.Error != "lapilli serve couldn't run it" {
		t.Errorf("%+v", got)
	}
	if strings.Contains(b.log.String(), "made to") {
		t.Error("the log holds what the panic said")
	}
}

// The leader's rounds let go of backtests ended a day ago, the port off.
func TestBacktestPrunedPortOff(t *testing.T) {
	s, _ := server(t, nil)
	ctx := context.Background()
	if _, _, _, err := s.l.AskBacktest(ctx, ledger.BacktestAsk{Backtest: ledger.Backtest{ID: strings.Repeat("a", 32), Trust: "gone", KeyHash: "k"},
		Body: []byte("{}"), Queue: 1, PerTrust: 1}, now.Add(-50*time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.l.StartBacktest(ctx, now.Add(-49*time.Hour))
	s.l.EndBacktest(ctx, strings.Repeat("a", 32), ledger.BacktestDone, "", 0, nil, now.Add(-48*time.Hour))
	s.jobs(ctx, false)
	if _, ok, _ := s.l.Backtest(ctx, strings.Repeat("a", 32)); ok {
		t.Error("a backtest ended two days ago is kept, the port off")
	}
}

// Guesses don't keep a job from reading its backtest: a wrong key or an id
// that isn't is limited by the address it comes from, and comes to 429;
// the job's own read, from the same address, is still answered, and is
// limited by its backtest alone.
func TestBacktestReadsNotStarved(t *testing.T) {
	b := btServer(t, nil, "", nil)
	_, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", b.plain, btBody(ruleFile, changedRules, nil))
	id, key := m["id"].(string), m["key"].(string)
	codes := map[int]int{}
	for i := range 150 {
		c, _, _ := btAsk(t, b.h, "GET", fmt.Sprintf("/v1/backtests/%032x", i), key, nil)
		codes[c]++
	}
	if codes[404] == 0 || codes[429] == 0 {
		t.Errorf("150 guesses: %v", codes)
	}
	if c, m, _ := btAsk(t, b.h, "GET", "/v1/backtests/"+id, key, nil); c != 202 || m["state"] != "waiting" {
		t.Errorf("the job's own read after them: %d %v", c, m)
	}
	reads := map[int]int{}
	for range 30 {
		c, _, _ := btAsk(t, b.h, "GET", "/v1/backtests/"+id, key, nil)
		reads[c]++
	}
	if reads[202] == 0 || reads[429] == 0 {
		t.Errorf("30 reads of one backtest at once: %v", reads)
	}
}

// What the lane asks of the history is in lapilli_serve_history_*'s totals,
// beside the runs' and people's.
func TestBacktestLaneInHistoryMetrics(t *testing.T) {
	var asked atomic.Int64
	prometheus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	defer prometheus.Close()
	b := btServer(t, prom.New(prometheus.URL, "test"), "", nil)
	count := func() int64 {
		_, metrics, _ := get(t, b.s.Handler(), "/metrics")
		var n int64
		for _, line := range strings.Split(metrics, "\n") {
			if strings.HasPrefix(line, `lapilli_serve_history_requests_total{api="prometheus",answer="2xx"} `) {
				fmt.Sscan(strings.Fields(line)[1], &n)
			}
		}
		return n
	}
	before, sent := count(), asked.Load()
	b.done(t, b.plain, btBody(ruleFile, changedRules, map[string]any{"days": 2}))
	lane := b.s.btPort.asked.Requests.Load()
	if lane == 0 || count()-before != lane || asked.Load()-sent != lane {
		t.Errorf("the lane asked %d (the history saw %d more); the totals grew %d", lane, asked.Load()-sent, count()-before)
	}
}

// variant is raw with its signature's last character changed in the bits
// base64url carries past the last byte: another string, the same token.
func variant(raw string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, raw[len(raw)-1])
	return raw[:len(raw)-1] + string(alphabet[last^1])
}

// A token without jti, as Forgejo's are, starts one backtest however it is
// written: the same token with a bit flipped past its signature's last
// byte, and another body, is 409.
func TestBacktestTokenWithoutJTI(t *testing.T) {
	b := btServer(t, nil, "", nil)
	claims := idptest.Forgejo(9007199254)
	claims["jti"] = nil
	tok := b.fj.Token(btAudience, claims)
	if code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", tok, btBody(ruleFile, changedRules, map[string]any{"days": 3})); code != 202 {
		t.Fatal(code, m)
	}
	other := variant(tok)
	if other == tok {
		t.Fatal("no variant")
	}
	if code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", other, btBody(ruleFile, changedRules, map[string]any{"days": 2})); code != 409 {
		t.Errorf("the same token written another way, another body: %d %v", code, m)
	}
}

// The lane is busy while it runs a backtest, and keeps its share then.
func TestBacktestLaneBusy(t *testing.T) {
	b := btServer(t, nil, "", nil)
	var during bool
	laneHook = func(ledger.Backtest) { during = b.s.btPort.busy.Load() }
	t.Cleanup(func() { laneHook = nil })
	b.done(t, b.plain, btBody(ruleFile, changedRules, nil))
	if !during || b.s.btPort.busy.Load() {
		t.Errorf("busy while it ran %t, after %t", during, b.s.btPort.busy.Load())
	}
}
