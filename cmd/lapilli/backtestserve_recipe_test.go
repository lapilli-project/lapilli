package main

import (
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"lapilli.dev/internal/verdict"
)

// The backtest recipes through lapilli serve (LAPILLI_SERVE,
// docs/design/backtests.md): the rules of both commits sent to a stand-in
// for serve's backtest port, nothing installed or run, the report read
// back and posted as lapilli verdict's would be.

const (
	serveID  = "0123456789abcdef0123456789abcdef"
	serveKey = "k3y_Of-The-Backtest-0123456789abcdefghijklm"
	// servePlain is the plain token the stand-in takes, as a file holds it.
	servePlain = "plain-token-0123456789abcdefghijklmnopqrstuvwxyz"
)

// serveAnswer is one answer of the stand-in's.
type serveAnswer struct {
	code   int
	header map[string]string
	body   string
}

// serveStandIn is lapilli serve's backtest port, as far as a recipe sees
// it: POST /v1/backtests, taking token alone, and GET and DELETE
// /v1/backtests/ID, taking the key alone. It answers each in turn as post
// and get say, the last again once they run out, and keeps what it was
// sent.
type serveStandIn struct {
	t     *testing.T
	url   string
	token string

	mu                   sync.Mutex
	post, get            []serveAnswer
	posts, gets, deletes int
	body                 map[string]any
}

func newServeStandIn(t *testing.T) *serveStandIn {
	t.Helper()
	s := &serveStandIn{t: t, token: servePlain,
		post: []serveAnswer{{202, nil, `{"id":"` + serveID + `","key":"` + serveKey + `","position":1,"requests":160}`}}}
	hs := httptest.NewServer(s)
	t.Cleanup(hs.Close)
	s.url = hs.URL
	return s
}

// answers sets what the stand-in answers next, between runs.
func (s *serveStandIn) answers(post, get []serveAnswer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if post != nil {
		s.post = post
	}
	s.get = get
	s.posts, s.gets, s.deletes, s.body = 0, 0, 0, nil
}

// counts are the requests it took: POSTs, GETs and DELETEs.
func (s *serveStandIn) counts() (int, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.posts, s.gets, s.deletes
}

func (s *serveStandIn) sent() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body
}

func (s *serveStandIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Header.Get("Origin") != "" {
		s.t.Errorf("a request with an Origin: %s", r.Header.Get("Origin"))
	}
	reply := func(as []serveAnswer, n int) {
		a := as[min(n, len(as)-1)]
		for k, v := range a.header {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(a.code)
		io.WriteString(w, a.body)
	}
	id, ok := strings.CutPrefix(r.URL.Path, "/v1/backtests/")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/backtests":
		if r.Header.Get("Authorization") != "Bearer "+s.token {
			s.t.Errorf("POST with %q, not the job's token as a bearer token", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Content-Type") != "application/json" {
			s.t.Errorf("POST of %q", r.Header.Get("Content-Type"))
		}
		b, _ := io.ReadAll(r.Body)
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.UseNumber()
		var body map[string]any
		if err := dec.Decode(&body); err != nil {
			s.t.Errorf("a body that isn't JSON: %v\n%s", err, b)
		}
		for k := range body {
			if !slices.Contains([]string{"before", "after", "expect", "expect_before", "rules_select", "days", "tz", "pull_request", "commit"}, k) {
				s.t.Errorf("a field serve doesn't take: %s", k)
			}
		}
		s.body = body
		s.posts++
		reply(s.post, s.posts-1)
	case ok && id == serveID && (r.Method == http.MethodGet || r.Method == http.MethodDelete):
		if r.Header.Get("Authorization") != "Bearer "+serveKey {
			http.Error(w, `{"error":"no such backtest"}`, http.StatusNotFound)
			return
		}
		if r.Method == http.MethodDelete {
			s.deletes++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s.gets++
		if len(s.get) == 0 {
			s.t.Errorf("GET, with nothing to answer")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		reply(s.get, s.gets-1)
	default:
		s.t.Errorf("%s %s", r.Method, r.URL)
		http.NotFound(w, r)
	}
}

// serveMarkdown is the Markdown serve gives back, its summary line first.
func serveMarkdown(summary string) string {
	return verdict.SummaryLine + summary + ")\n### Lapilli backtest, 2026-11-04 00:00 → 2026-11-18 00:00 UTC (+00:00), judged by lapilli serve's record\n\n" +
		"| Rule | Severity | Fired |\n| --- | --- | --: |\n| `CheckoutErrors` | `page` | 1 → 0 |\n"
}

const changedSummary = `{"nothing_changed":false,"changed":1,"added":0,"removed":0,"same":1,"alike":0,"unreplayed":0}`

func serveDone(markdown string) serveAnswer {
	b, _ := json.Marshal(map[string]any{"state": "done", "markdown": markdown, "report": map[string]any{"summary": map[string]any{}}})
	return serveAnswer{200, nil, string(b)}
}

func serveState(code int, v map[string]any) serveAnswer {
	b, _ := json.Marshal(v)
	return serveAnswer{code, nil, string(b)}
}

// The answers of a backtest that waits, runs, then is done.
func serveWaitsRunsDone(markdown string) []serveAnswer {
	return []serveAnswer{
		serveState(202, map[string]any{"state": "waiting", "position": 1}),
		serveState(202, map[string]any{"state": "waiting", "position": 0}),
		serveState(202, map[string]any{"state": "running", "requests": 160, "asked": 40}),
		serveDone(markdown),
	}
}

// curlShim puts a curl before the real one that writes down its arguments
// and environment, a line each, and adds flags after -q, which it keeps
// first: what a recipe gives curl on a command line, ps shows.
func curlShim(t *testing.T, env []string, flags ...string) ([]string, string, string) {
	t.Helper()
	real, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("no curl")
	}
	dir := t.TempDir()
	argv, envs := filepath.Join(dir, "argv"), filepath.Join(dir, "env")
	more := ""
	for _, f := range flags {
		more += " '" + f + "'"
	}
	os.WriteFile(filepath.Join(dir, "curl"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" >>'"+argv+"'\nenv >>'"+envs+"'\n"+
		"first=$1\nshift\nexec '"+real+"' \"$first\""+more+" \"$@\"\n"), 0o755)
	out := slices.Clone(env)
	for i, e := range out {
		if strings.HasPrefix(e, "PATH=") {
			out[i] = "PATH=" + dir + string(os.PathListSeparator) + strings.TrimPrefix(e, "PATH=")
		}
	}
	return out, argv, envs
}

// serveRepo is a pull request that changes a rule and the expectations:
// alerts/ holds a file, one in a directory below, and one whose name begins
// with a dot; rules/extra.yaml is a file PATHS names.
func serveRepo(t *testing.T) *prRepo {
	t.Helper()
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m"), "alerts/team/api.yaml": shopRules("10m"),
		"alerts/.draft.yaml": "not sent\n", "rules/extra.yaml": "groups: []\n", "expect.yaml": "expectations: []\n", "README.md": "x\n"})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h"),
		"expect.yaml": "expectations:\n  - rule: CheckoutErrors\n    budget: {week: 5}\n"})
	return repo
}

// sentFiles are a side of a body, by path.
func sentFiles(t *testing.T, side any) map[string]string {
	t.Helper()
	list, ok := side.([]any)
	if !ok {
		t.Fatalf("a side that isn't a list: %v", side)
	}
	out := map[string]string{}
	for _, f := range list {
		m, _ := f.(map[string]any)
		p, _ := m["path"].(string)
		c, _ := m["content"].(string)
		out[p] = c
	}
	return out
}

// serveRun is a recipe run through the stand-in, with the job's plain
// token in a file, asking it with no pause between.
func serveRun(t *testing.T, h backtestHost, repo *prRepo, srv *serveStandIn) *recipeRun {
	t.Helper()
	rr := newRecipeRun(t, h, repo)
	tok := filepath.Join(t.TempDir(), "serve-token")
	os.WriteFile(tok, []byte(servePlain+"\n"), 0o600)
	rr.env = append(rr.env, "LAPILLI_SERVE="+srv.url, "LAPILLI_SERVE_TOKEN_FILE="+tok, "SERVE_POLL=0")
	return rr
}

// On every host, the recipe sends serve both commits' files as --rules
// reads them, named under what PATHS names, the expectations, the days of
// --since, --tz, --rules-select, the pull request's number and its commit;
// the token and the key as bearer tokens, on no command line; it waits,
// saying where the backtest is, and posts the Markdown serve gave as the
// comment. Nothing is installed or run.
func TestBacktestServeRecipe(t *testing.T) {
	needRecipeTools(t)
	repo := serveRepo(t)
	head := repo.git("rev-parse", "HEAD")
	ran := filepath.Join(t.TempDir(), "ran")
	fake := filepath.Join(t.TempDir(), "lapilli")
	os.WriteFile(fake, []byte("#!/bin/sh\ntouch '"+ran+"'\nexit 1\n"), 0o755)
	markdown := serveMarkdown(changedSummary)
	for _, h := range backtestHosts {
		t.Run(h.kind, func(t *testing.T) {
			srv := newServeStandIn(t)
			srv.answers(nil, serveWaitsRunsDone(markdown))
			rr := serveRun(t, h, repo, srv)
			var argv, envs string
			rr.env, argv, envs = curlShim(t, rr.env)
			out, err := rr.run([]string{"PATHS=alerts rules/extra.yaml", "EXPECT=expect.yaml", "LAPILLI=" + fake},
				"--since", "14d", "--tz", "UTC", "--rules-select=grafana_alert!=1")
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if posts, gets, deletes := srv.counts(); posts != 1 || gets != 4 || deletes != 0 {
				t.Errorf("%d POSTs, %d GETs, %d DELETEs", posts, gets, deletes)
			}
			body := srv.sent()
			before, after := sentFiles(t, body["before"]), sentFiles(t, body["after"])
			wantBefore := map[string]string{"shop.yaml": shopRules("5m"), "team/api.yaml": shopRules("10m"), "extra.yaml": "groups: []\n"}
			wantAfter := map[string]string{"shop.yaml": shopRules("2h"), "team/api.yaml": shopRules("10m"), "extra.yaml": "groups: []\n"}
			if !maps.Equal(before, wantBefore) || !maps.Equal(after, wantAfter) {
				t.Errorf("before %v\nafter %v", slices.Sorted(maps.Keys(before)), slices.Sorted(maps.Keys(after)))
			}
			if body["expect"] != "expectations:\n  - rule: CheckoutErrors\n    budget: {week: 5}\n" || body["expect_before"] != "expectations: []\n" {
				t.Errorf("expect %q, expect_before %q", body["expect"], body["expect_before"])
			}
			if body["days"] != json.Number("14") || body["tz"] != "UTC" || body["rules_select"] != "grafana_alert!=1" ||
				body["pull_request"] != json.Number("7") || body["commit"] != head {
				t.Errorf("days %v, tz %v, rules_select %v, pull_request %v, commit %v", body["days"], body["tz"], body["rules_select"],
					body["pull_request"], body["commit"])
			}
			// The comment is serve's Markdown, under the marker.
			if got := rr.body(); got != "[//]: # (lapilli-backtest default)\n"+markdown {
				t.Errorf("the comment:\n%s", got)
			}
			// Where it is when that changes (and every 30 seconds).
			for _, want := range []string{"waiting in lapilli serve's queue, 1 before it",
				"running in lapilli serve: 40 of its 160 requests to the history asked", "lapilli serve took the backtest (" + serveID + "): 160 requests",
				"sending lapilli serve 3 files of the pull request's commit, and 3 of the commit it goes into", "LAPILLI passed over"} {
				if !strings.Contains(out, want) {
					t.Errorf("the job's log doesn't say %q:\n%s", want, out)
				}
			}
			if _, err := os.Stat(ran); err == nil {
				t.Error("lapilli ran")
			}
			// Neither the token nor the key on a command line, nor the
			// host's token.
			args, _ := os.ReadFile(argv)
			for _, secret := range []string{servePlain, serveKey, "host-token-0123456789"} {
				if strings.Contains(string(args), secret) {
					t.Errorf("%s on curl's command line:\n%s", secret, args)
				}
			}
			e, _ := os.ReadFile(envs)
			if strings.Contains(string(e), servePlain) || strings.Contains(string(e), serveKey) {
				t.Errorf("a token in curl's environment")
			}
		})
	}
}

// LAPILLI_ID_TOKEN, the CI's ID token (GitLab's id_tokens:), is sent as
// the bearer token, kept out of what the recipe runs: neither curl's
// command line nor its environment has it, nor RENDER's.
func TestBacktestServeRecipeIDToken(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"chart/out.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"chart/out.yaml": shopRules("2h")})
	srv := newServeStandIn(t)
	srv.token = "eyJhbGciOiJSUzI1NiJ9.eyJpc3MiOiJodHRwczovL2dpdGxhYi5leGFtcGxlLmNvbSJ9.c2lnbmF0dXJl"
	srv.answers(nil, []serveAnswer{serveDone(serveMarkdown(changedSummary))})
	rr := newRecipeRun(t, backtestHosts[2], repo)
	var argv, envs string
	rr.env, argv, envs = curlShim(t, rr.env)
	seen := filepath.Join(t.TempDir(), "render-saw")
	out, err := rr.run([]string{"LAPILLI_SERVE=" + srv.url, "LAPILLI_ID_TOKEN=" + srv.token, "SERVE_POLL=0",
		"RENDER=printf '%s' \"${LAPILLI_ID_TOKEN:-none}\" >" + seen + "; cat chart/out.yaml"})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if body := srv.sent(); !maps.Equal(sentFiles(t, body["after"]), map[string]string{"rules.yaml": shopRules("2h")}) ||
		!maps.Equal(sentFiles(t, body["before"]), map[string]string{"rules.yaml": shopRules("5m")}) {
		t.Errorf("what RENDER wrote isn't sent as rules.yaml: %v", body)
	}
	if b, _ := os.ReadFile(seen); string(b) != "none" {
		t.Errorf("RENDER saw LAPILLI_ID_TOKEN: %q", b)
	}
	args, _ := os.ReadFile(argv)
	e, _ := os.ReadFile(envs)
	if strings.Contains(string(args), srv.token) || strings.Contains(string(e), srv.token) {
		t.Error("the ID token on curl's command line, or in its environment")
	}
	if !strings.Contains(rr.body(), "| `CheckoutErrors` | `page` | 1 → 0 |") {
		t.Errorf("the comment:\n%s", rr.body())
	}
}

// actionsIDTokens is a stand-in for GitHub's (and Forgejo's) Actions
// token service: it gives token for the audience asked, to the request's
// token alone, as core.getIDToken asks it.
func actionsIDTokens(t *testing.T, audience, token string) (*httptest.Server, *int) {
	t.Helper()
	var mu sync.Mutex
	asked := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		asked++
		if r.Header.Get("Authorization") != "Bearer request-token-0123456789" || r.URL.Path != "/idtoken" ||
			r.URL.Query().Get("api-version") != "2.0" || r.URL.Query().Get("audience") != audience {
			t.Errorf("asked for an ID token with %q at %s", r.Header.Get("Authorization"), r.URL)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"count": 1, "value": token})
	}))
	t.Cleanup(s.Close)
	return s, &asked
}

// github.sh, and gitea.sh in Forgejo Actions, ask Actions for the job's ID
// token for LAPILLI_SERVE_AUDIENCE, its request's token on stdin and the
// audience URL-encoded, right before the request, once for each.
func TestBacktestServeRecipeAsksForIDToken(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	audience := "https://lapilli-ci.example.com/?team=a&b=c"
	for _, h := range []backtestHost{backtestHosts[0], backtestHosts[1]} {
		t.Run(h.kind, func(t *testing.T) {
			srv := newServeStandIn(t)
			srv.token = "eyJ.actions-id-token." + h.kind
			srv.answers(nil, []serveAnswer{serveDone(serveMarkdown(changedSummary))})
			oidc, asked := actionsIDTokens(t, audience, srv.token)
			rr := newRecipeRun(t, h, repo)
			var argv string
			rr.env, argv, _ = curlShim(t, rr.env)
			out, err := rr.run([]string{"PATHS=alerts", "LAPILLI_SERVE=" + srv.url, "LAPILLI_SERVE_AUDIENCE=" + audience, "SERVE_POLL=0",
				"ACTIONS_ID_TOKEN_REQUEST_URL=" + oidc.URL + "/idtoken?api-version=2.0", "ACTIONS_ID_TOKEN_REQUEST_TOKEN=request-token-0123456789"})
			if err != nil || *asked != 1 || !strings.Contains(rr.body(), "`CheckoutErrors`") {
				t.Fatalf("%v, asked %d\n%s", err, *asked, out)
			}
			if args, _ := os.ReadFile(argv); strings.Contains(string(args), "request-token-0123456789") || strings.Contains(string(args), srv.token) {
				t.Errorf("a token on curl's command line:\n%s", args)
			}
		})
	}
}

// What can go wrong through serve is said in the comment, in serve's words
// where it has them, and the job succeeds unless FAIL_ON_ERROR=true.
func TestBacktestServeRecipeFails(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	srv := newServeStandIn(t)
	rr := serveRun(t, backtestHosts[0], repo, srv)
	accepted := []serveAnswer{{202, nil, `{"id":"` + serveID + `","key":"` + serveKey + `","position":0,"requests":80}`}}
	timeout := "it didn't finish within --backtest-timeout 15m: give fewer days, or fewer rules"
	for _, c := range []struct {
		name      string
		post, get []serveAnswer
		why       string
		said      string
		gets      int
	}{
		{"failed", accepted, []serveAnswer{serveState(202, map[string]any{"state": "running", "requests": 80, "asked": 80}),
			serveState(200, map[string]any{"state": "failed", "error": timeout})}, "lapilli serve couldn't backtest the rules", timeout, 2},
		{"superseded", accepted, []serveAnswer{serveState(200, map[string]any{"state": "superseded"})},
			"a newer commit's backtest took this one's place in lapilli serve's queue", "", 1},
		{"cancelled", accepted, []serveAnswer{serveState(200, map[string]any{"state": "cancelled"})}, "the backtest was cancelled in lapilli serve", "", 1},
		{"refused", []serveAnswer{serveState(400, map[string]any{"error": "it needs 160 requests of the history, which at the lane's 0.125 a second take 21m, past --backtest-timeout 15m: 4 days would fit"})},
			nil, "lapilli serve refused the backtest", "it needs 160 requests of the history", 0},
		{"unauthorized", []serveAnswer{{401, map[string]string{"WWW-Authenticate": "Bearer"}, `{"error":"unauthorized"}`}}, nil,
			"lapilli serve didn't take the job's token: the entry of its --backtest-trust for this repository says what it takes (its log says why)", "", 0},
		{"asked twice", []serveAnswer{serveState(200, map[string]any{"id": serveID, "state": "waiting", "position": 0})}, nil,
			"lapilli serve had this request already, from this token, and gave the key to read it by to the first: a job asks once", "", 0},
		{"token used", []serveAnswer{serveState(409, map[string]any{"error": "this token started another backtest: a job asks once, with a token of its own"})}, nil,
			"the job's token started another backtest in lapilli serve: a job asks once, with a token of its own", "this token started another backtest", 0},
		{"gone", accepted, []serveAnswer{{404, nil, `{"error":"no such backtest"}`}}, "lapilli serve has no such backtest: kept a day after it ends, or another serve's", "", 1},
		{"no report", accepted, []serveAnswer{serveState(200, map[string]any{"state": "done"})}, "lapilli serve's answer holds no report", "", 1},
		{"no key", []serveAnswer{serveState(202, map[string]any{"id": serveID, "position": 0})}, nil,
			"lapilli serve took the backtest, but gave no id and key to read it by", "", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv.answers(c.post, c.get)
			rr.host.set(func(h *commentHost) { h.comments = nil })
			out, err := rr.run([]string{"PATHS=alerts"})
			if err != nil {
				t.Fatalf("a failure fails the job: %v\n%s", err, out)
			}
			block := failure(t, rr.body(), c.why)
			if c.said != "" && (!strings.Contains(block, c.said) || !strings.Contains(out, "  | "+c.said)) {
				t.Errorf("serve's words aren't in the comment and the log:\n%s\n%s", block, out)
			}
			if _, gets, _ := srv.counts(); gets != c.gets {
				t.Errorf("%d GETs, want %d", gets, c.gets)
			}
			// Asked to, the job fails too, once it has said so.
			srv.answers(c.post, c.get)
			rr.host.set(func(h *commentHost) { h.comments = nil })
			if out, err := rr.run([]string{"PATHS=alerts", "FAIL_ON_ERROR=true"}); err == nil || len(rr.host.mine()) != 1 {
				t.Errorf("FAIL_ON_ERROR=true: %v\n%s", err, out)
			}
		})
	}

	// Out of reach.
	rr.host.set(func(h *commentHost) { h.comments = nil })
	out, err := rr.run([]string{"PATHS=alerts", "LAPILLI_SERVE=http://127.0.0.1:1"})
	if err != nil || !strings.Contains(out, "  | curl: (7) ") {
		t.Errorf("out of reach: %v\n%s", err, out)
	}
	if block := failure(t, rr.body(), "lapilli serve couldn't be reached (the job's log says why)"); strings.Contains(block, "127.0.0.1") {
		t.Errorf("where serve is, in the comment: %q", block)
	}
}

// Asked to wait (429, 503), the recipe waits as Retry-After says, and asks
// again: the POST, and the GET.
func TestBacktestServeRecipeRetryAfter(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	srv := newServeStandIn(t)
	rr := serveRun(t, backtestHosts[0], repo, srv)
	srv.answers([]serveAnswer{
		{429, map[string]string{"Retry-After": "1"}, `{"error":"as many backtests wait as the queue takes: try again in a minute"}`},
		{503, map[string]string{"Retry-After": "1"}, `{"error":"the rules in effect haven't been read yet: try again in a minute"}`},
		{202, nil, `{"id":"` + serveID + `","key":"` + serveKey + `","position":0,"requests":1}`},
	}, []serveAnswer{{429, map[string]string{"Retry-After": "1"}, `{"error":"too many reads: try again in a second"}`}, serveDone(serveMarkdown(changedSummary))})
	out, err := rr.run([]string{"PATHS=alerts"})
	posts, gets, _ := srv.counts()
	if err != nil || posts != 3 || gets != 2 || strings.Contains(rr.body(), "failed") {
		t.Fatalf("%v: %d POSTs, %d GETs\n%s\n%s", err, posts, gets, rr.body(), out)
	}
	if !strings.Contains(out, "lapilli serve can't take the backtest yet (429): asking again in 1s") ||
		!strings.Contains(out, "  | as many backtests wait as the queue takes") {
		t.Errorf("the job's log:\n%s", out)
	}
	// A wait past SERVE_WAIT isn't waited.
	srv.answers([]serveAnswer{{429, map[string]string{"Retry-After": "3600"}, `{"error":"this entry starts 30 backtests an hour at most"}`}}, nil)
	rr.host.set(func(h *commentHost) { h.comments = nil })
	rr.run([]string{"PATHS=alerts", "SERVE_WAIT=1m"})
	if posts, _, _ := srv.counts(); posts != 1 || !strings.Contains(failure(t, rr.body(), "lapilli serve had no room for the backtest within SERVE_WAIT (1m)"),
		"this entry starts 30 backtests an hour at most") {
		t.Errorf("%d POSTs:\n%s", posts, rr.body())
	}
}

// A backtest that doesn't come back within SERVE_WAIT is cancelled
// (DELETE), and said.
func TestBacktestServeRecipeWait(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	srv := newServeStandIn(t)
	rr := serveRun(t, backtestHosts[0], repo, srv)
	srv.answers(nil, []serveAnswer{serveState(202, map[string]any{"state": "waiting", "position": 3})})
	out, err := rr.run([]string{"PATHS=alerts", "SERVE_WAIT=1s", "SERVE_POLL=1"})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	failure(t, rr.body(), "the backtest didn't come back within SERVE_WAIT (1s): cancelled in lapilli serve")
	if _, gets, deletes := srv.counts(); gets < 1 || deletes != 1 || !strings.Contains(out, "cancelled the backtest in lapilli serve") {
		t.Errorf("%d GETs, %d DELETEs\n%s", gets, deletes, out)
	}
	// A state the recipe doesn't know: cancelled too.
	srv.answers(nil, []serveAnswer{serveState(202, map[string]any{"state": "##vso[task.complete result=Succeeded]"})})
	rr.host.set(func(h *commentHost) { h.comments = nil })
	out, _ = rr.run([]string{"PATHS=alerts"})
	failure(t, rr.body(), "lapilli serve said the backtest is in a state the recipe doesn't know: cancelled")
	if _, _, deletes := srv.counts(); deletes != 1 || strings.Contains(out, "##vso[") {
		t.Errorf("%d DELETEs\n%s", deletes, out)
	}
}

// What serve says reaches the job's log made inert, no line of it a CI's
// command, and the comment only in the fenced block, nothing in it able to
// end the block or hide what is shown.
func TestBacktestServeRecipeInert(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	srv := newServeStandIn(t)
	rr := serveRun(t, backtestHosts[4], repo, srv)
	hostile := "##[error title=Lapilli backtest passed]No change ##vso[task.complete result=Succeeded]\x1b[31m red\u202e left\n" +
		"::error::merge it\n```\n</details> @octocat #123"
	srv.answers(nil, []serveAnswer{serveState(200, map[string]any{"state": "failed", "error": hostile})})
	out, err := rr.run([]string{"PATHS=alerts"})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "::") || strings.Contains(line, "##vso[") || strings.Contains(line, "##[") || strings.Contains(line, "\x1b") {
			t.Errorf("a command in the job's log: %q", line)
		}
	}
	if !strings.Contains(out, "  | ##(error title=Lapilli backtest passed]No change ##vso(task.complete") || !strings.Contains(out, "  | ::error::merge it") {
		t.Errorf("the job's log:\n%s", out)
	}
	block := failure(t, rr.body(), "lapilli serve couldn't backtest the rules")
	if !strings.Contains(block, "</details> @octocat #123") {
		t.Errorf("the block: %q", block)
	}
	// A note beside a report, too.
	b, _ := json.Marshal(map[string]any{"state": "done", "markdown": serveMarkdown(changedSummary), "report": nil, "note": "##vso[task.setvariable variable=OK]yes\x1b[0m"})
	srv.answers(nil, []serveAnswer{{200, nil, string(b)}})
	out, err = rr.run([]string{"PATHS=alerts"})
	if err != nil || strings.Contains(out, "##vso[") || !strings.Contains(out, "lapilli serve: ##vso(task.setvariable") {
		t.Errorf("a note: %v\n%s", err, out)
	}
}

// Held to the expectations, the job fails from the summary line serve's
// report has, as from lapilli's: a change that breaks one, or adds a rule
// none holds, and a backtest that couldn't judge them.
func TestBacktestServeRecipeExpectations(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m"), "expect.yaml": "expectations: []\n"})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	srv := newServeStandIn(t)
	rr := serveRun(t, backtestHosts[0], repo, srv)
	summary := func(broken, unexpected, unreplayed int) string {
		return serveMarkdown(`{"nothing_changed":false,"changed":1,"unreplayed":` + strconv.Itoa(unreplayed) + `,"expected":true,"expectations_broken":` +
			strconv.Itoa(broken) + `,"expectations_already_broken":0,"unexpected":` + strconv.Itoa(unexpected) + `}`)
	}
	for _, c := range []struct {
		name     string
		get      serveAnswer
		settings []string
		fails    string
	}{
		{"broken", serveDone(summary(1, 0, 0)), []string{"FAIL_ON_EXPECTATIONS=true"}, "the change breaks 1 expectations the rules in place meet (FAIL_ON_EXPECTATIONS=true)"},
		{"met", serveDone(summary(0, 0, 0)), []string{"FAIL_ON_EXPECTATIONS=true"}, ""},
		{"broken, not held", serveDone(summary(1, 0, 0)), nil, ""},
		{"a rule none holds", serveDone(summary(0, 2, 0)), []string{"EXPECT_REQUIRED=true"}, "the change adds 2 rules no expectation holds (EXPECT_REQUIRED=true)"},
		{"not replayed", serveDone(summary(0, 0, 1)), []string{"FAIL_ON_EXPECTATIONS=true"}, "1 of the rules that differ couldn't be replayed, so their expectations can't be judged"},
		{"failed", serveState(200, map[string]any{"state": "failed", "error": "lapilli serve couldn't read its record: try again"}), []string{"FAIL_ON_EXPECTATIONS=true"},
			"the expectations couldn't be judged"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv.answers(nil, []serveAnswer{c.get})
			rr.host.set(func(h *commentHost) { h.comments = nil })
			out, err := rr.run(append([]string{"PATHS=alerts", "EXPECT=expect.yaml"}, c.settings...))
			if (err != nil) != (c.fails != "") || !strings.Contains(out, c.fails) || len(rr.host.mine()) != 1 {
				t.Errorf("%v, want it to fail saying %q\n%s", err, c.fails, out)
			}
			if c.name != "failed" && strings.Contains(rr.body(), "### Lapilli backtest failed") {
				t.Errorf("a report held to its expectations is no failure of the backtest:\n%s", rr.body())
			}
		})
	}
	// Nothing changed, and no comment yet: none posted.
	srv.answers(nil, []serveAnswer{serveDone(verdict.SummaryLine + `{"nothing_changed":true,"unreplayed":0}` + ")\n" + verdict.NothingChangedHeading + "\n")})
	rr.host.set(func(h *commentHost) { h.comments = nil })
	if out, err := rr.run([]string{"PATHS=alerts", "EXPECT=expect.yaml", "FAIL_ON_EXPECTATIONS=true"}); err != nil || len(rr.host.comments) != 0 ||
		!strings.Contains(out, "none posted") {
		t.Errorf("nothing changed: %v, %d comments\n%s", err, len(rr.host.comments), out)
	}
}

// What can't be sent is said before anything is: a rule file that isn't
// UTF-8 text, a request past 4 MiB, two files of one name from two of
// PATHS.
func TestBacktestServeRecipeRequest(t *testing.T) {
	needRecipeTools(t)
	srv := newServeStandIn(t)
	for _, c := range []struct {
		name       string
		base, head map[string]string
		paths      string
		why        string
		log        string
	}{
		{"not text", map[string]string{"alerts/shop.yaml": shopRules("5m")}, map[string]string{"alerts/team/latin1.yaml": "# caf\xe9\ngroups: []\n"}, "alerts",
			"a rule file in the pull request's commit isn't UTF-8 text, which lapilli serve takes alone: not sent (the job's log says which)",
			"not UTF-8 text, in the pull request's commit: team/latin1.yaml"},
		{"too big", map[string]string{"alerts/shop.yaml": shopRules("5m")}, map[string]string{"alerts/big.yaml": "# " + strings.Repeat("x", 5<<20) + "\n"}, "alerts",
			"the rules and expectations are 6 MiB as lapilli serve's request, past the 4 MiB it takes: backtest fewer files", ""},
		{"one name twice", map[string]string{"alerts/prod/api.yaml": shopRules("5m"), "alerts/stage/api.yaml": shopRules("5m")},
			map[string]string{"alerts/prod/api.yaml": shopRules("2h")}, "alerts/prod alerts/stage",
			"two of PATHS hold a file of the same name in the commit it goes into, which lapilli serve reads as one directory's: name them apart (the job's log says which)",
			"two of PATHS hold api.yaml, in the commit it goes into"},
	} {
		t.Run(c.name, func(t *testing.T) {
			repo := newPRRepo(t, c.base)
			repo.pullRequest(c.head)
			rr := serveRun(t, backtestHosts[0], repo, srv)
			srv.answers(nil, nil)
			out, err := rr.run([]string{"PATHS=" + c.paths})
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			failure(t, rr.body(), c.why)
			if posts, _, _ := srv.counts(); posts != 0 || !strings.Contains(out, c.log) {
				t.Errorf("%d POSTs\n%s", posts, out)
			}
		})
	}
}

// LAPILLI_SERVE is https, or plain http to this machine or to a name inside
// a cluster, as serve takes an identity provider's URL: a runner in the
// cluster reaches the chart's Service in plain http.
func TestBacktestServeRecipeClusterURL(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	srv := newServeStandIn(t)
	u, _ := url.Parse(srv.url)
	for _, name := range []string{"lapilli-backtest", "lapilli-backtest.monitoring.svc", "Lapilli-Backtest.Monitoring.svc.cluster.local"} {
		t.Run(name, func(t *testing.T) {
			srv.answers(nil, []serveAnswer{serveDone(serveMarkdown(changedSummary))})
			rr := serveRun(t, backtestHosts[0], repo, srv)
			// The name, as the cluster's DNS would answer it: here.
			rr.env, _, _ = curlShim(t, rr.env, "--connect-to", strings.ToLower(name)+":8084:"+u.Host)
			rr.env = append(rr.env, "LAPILLI_SERVE=http://"+name+":8084/")
			out, err := rr.run([]string{"PATHS=alerts"})
			if posts, _, _ := srv.counts(); err != nil || posts != 1 || !strings.Contains(rr.body(), "`CheckoutErrors`") {
				t.Errorf("%v, %d POSTs\n%s\n%s", err, posts, rr.body(), out)
			}
		})
	}
}

// Through serve, what serve decides, and what isn't sent, is refused at
// once, saying why; so is what serve mode alone reads, without it.
func TestBacktestServeRecipeSettings(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m"), "incidents.yaml": "incidents: []\n"})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	srv := newServeStandIn(t)
	rr := serveRun(t, backtestHosts[0], repo, srv)
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte(servePlain), 0o600)
	history := "lapilli serve decides where the history is, and how it is asked"
	judging := "lapilli serve judges the firings, by its record and its own settings"
	for _, c := range []struct {
		settings []string
		args     []string
		want     string
	}{
		{[]string{"INCIDENTS=incidents.yaml"}, nil, "INCIDENTS: lapilli serve judges the firings by the incidents and impact windows it keeps"},
		{[]string{"IMPACT=up == 0"}, nil, "IMPACT: lapilli serve judges"},
		{[]string{"IMPACT_FOR=5m"}, nil, "IMPACT_FOR: lapilli serve judges"},
		{[]string{"MATCH=service"}, nil, "MATCH: lapilli serve judges"},
		{nil, []string{"--prometheus", "http://prometheus:9090"}, "--prometheus: " + history},
		{nil, []string{"--gcp-project=example"}, "--gcp-project: " + history},
		{nil, []string{"--auth", "google"}, "--auth: " + history},
		{nil, []string{"--step", "30s"}, "--step: " + history},
		{nil, []string{"--late=2m"}, "--late: " + history},
		{nil, []string{"--scrape-interval", "15s"}, "--scrape-interval: " + history},
		{nil, []string{"--lead", "5m"}, "--lead: " + judging},
		{nil, []string{"--match=service"}, "--match: " + judging},
		{nil, []string{"--refire", "2h"}, "--refire: " + judging},
		{nil, []string{"--sleep-hours", "23:00-07:00"}, "--sleep-hours: " + judging},
		{nil, []string{"--shifts", "every=1w"}, "--shifts: " + judging},
		{nil, []string{"--impact", "up == 0"}, "--impact: " + judging},
		{nil, []string{"--impact-for=10m"}, "--impact-for: " + judging},
		{nil, []string{"--incidents", "/tmp/incidents.yaml"}, "--incidents: " + judging},
		{nil, []string{"--from", "2026-11-01T00:00:00Z"}, "--from: lapilli serve replays only what has settled"},
		{nil, []string{"--to=2026-11-01T00:00:00Z"}, "--to: lapilli serve replays only what has settled"},
		{nil, []string{"--grafana", "https://grafana.example.com"}, "--grafana: lapilli serve backtests rules in Prometheus's format"},
		{nil, []string{"--loki-tenant", "a"}, "--loki-tenant: lapilli serve backtests rules in Prometheus's format"},
		{nil, []string{"--ruler=https://ruler.example.com"}, "--ruler: lapilli serve backtests"},
		{nil, []string{"--alarms", "cloudwatch"}, "--alarms: lapilli serve backtests"},
		{nil, []string{"--max-rate", "2"}, "--max-rate: through lapilli serve, the recipe sends it --since, --tz and --rules-select alone"},
		{nil, []string{"--json"}, "--json: the recipe gives lapilli the rules"},
		{nil, []string{"--expect", "x.yaml"}, "--expect: the recipe reads the expectations from both commits itself"},
		{nil, []string{"--since", "15d"}, "--since 15d: through lapilli serve, whole days, 1d to 14d"},
		{nil, []string{"--since=36h"}, "--since 36h: through lapilli serve, whole days"},
		{nil, []string{"--since", "0d"}, "--since 0d: through lapilli serve"},
		{nil, []string{"--tz"}, "--tz: want a value"},
		// The token: one, and one that can be read.
		{[]string{"LAPILLI_SERVE_TOKEN_FILE="}, nil, "LAPILLI_SERVE: give the token lapilli serve takes"},
		{[]string{"LAPILLI_ID_TOKEN=eyJ.a.b"}, nil, "give lapilli serve one token"},
		{[]string{"LAPILLI_SERVE_TOKEN_FILE=", "LAPILLI_ID_TOKEN=eyJ a b"}, nil, "LAPILLI_ID_TOKEN: want an ID token"},
		{[]string{"LAPILLI_SERVE_TOKEN_FILE=/nonexistent/token"}, nil, "LAPILLI_SERVE_TOKEN_FILE /nonexistent/token can't be read"},
		{[]string{"LAPILLI_SERVE_TOKEN_FILE=", "LAPILLI_SERVE_AUDIENCE=https://lapilli-ci.example.com"}, nil, "the job can't ask for an ID token"},
		{[]string{"LAPILLI_SERVE_AUDIENCE=https://lapilli-ci.example.com"}, nil, "give lapilli serve one token"},
		// Where serve is: the token goes there.
		{[]string{"LAPILLI_SERVE=http://lapilli.example.com"}, nil, "want lapilli serve's backtest port"},
		{[]string{"LAPILLI_SERVE=http://lapilli-backtest.monitoring:8084"}, nil, "want lapilli serve's backtest port"},
		{[]string{"LAPILLI_SERVE=http://10.0.0.8:8084"}, nil, "want lapilli serve's backtest port"},
		{[]string{"LAPILLI_SERVE=http://[fd00::1]:8084"}, nil, "want lapilli serve's backtest port"},
		{[]string{"LAPILLI_SERVE=http://ci@127.0.0.1:8084"}, nil, "want lapilli serve's backtest port"},
		{[]string{"LAPILLI_SERVE=https://lapilli-ci.example.com/?x=1"}, nil, "want lapilli serve's backtest port"},
		{[]string{"LAPILLI_SERVE=ftp://lapilli-ci.example.com"}, nil, "want lapilli serve's backtest port"},
		{[]string{"SERVE_WAIT=ten"}, nil, "SERVE_WAIT ten: want how long to wait for lapilli serve"},
		{[]string{"SERVE_WAIT=2d"}, nil, "SERVE_WAIT 2d: want how long"},
		{[]string{"SERVE_POLL=61"}, nil, "SERVE_POLL 61: want how many seconds apart"},
		{[]string{"SERVE_POLL=x"}, nil, "SERVE_POLL x: want how many seconds apart"},
		// Serve mode's alone, without it.
		{[]string{"LAPILLI_SERVE=", "SERVE_WAIT=30m"}, nil, "SERVE_WAIT is for a backtest through lapilli serve: give LAPILLI_SERVE"},
		{[]string{"LAPILLI_SERVE=", "SERVE_POLL=", "LAPILLI_SERVE_TOKEN_FILE=" + tok}, nil, "LAPILLI_SERVE_TOKEN_FILE is for a backtest through lapilli serve"},
		{[]string{"LAPILLI_SERVE=", "SERVE_POLL=", "LAPILLI_SERVE_TOKEN_FILE=", "LAPILLI_ID_TOKEN=eyJ.a.b"}, nil, "LAPILLI_ID_TOKEN is for a backtest through lapilli serve"},
		{[]string{"LAPILLI_SERVE=", "SERVE_POLL=", "LAPILLI_SERVE_TOKEN_FILE=", "LAPILLI_SERVE_AUDIENCE=https://lapilli-ci.example.com"}, nil,
			"LAPILLI_SERVE_AUDIENCE is for a backtest through lapilli serve"},
	} {
		out, err := rr.run(append([]string{"PATHS=alerts"}, c.settings...), c.args...)
		if err == nil || !strings.Contains(out, c.want) {
			t.Errorf("%v %v: %v, want it to fail saying %q\n%s", c.settings, c.args, err, c.want, out)
		}
	}
	if posts, gets, _ := srv.counts(); posts+gets != 0 || len(rr.host.comments) != 0 {
		t.Errorf("%d POSTs, %d GETs, commented %+v", posts, gets, rr.host.comments)
	}
	// A script that asks its CI for no ID token.
	gl := serveRun(t, backtestHosts[2], repo, srv)
	if out, err := gl.run([]string{"PATHS=alerts", "LAPILLI_SERVE_TOKEN_FILE=", "LAPILLI_SERVE_AUDIENCE=https://lapilli-ci.example.com"}); err == nil ||
		!strings.Contains(out, "LAPILLI_SERVE_AUDIENCE: gitlab.sh asks its CI for no ID token") {
		t.Errorf("gitlab.sh: %v\n%s", err, out)
	}
}

// action.yml with serve-url installs nothing: github.sh asks GitHub for
// the job's ID token for audience, as core.getIDToken(audience) does, and
// sends the rules to serve; without serve-url, the action is as it was.
func TestBacktestActionServe(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	audience := "https://lapilli-ci.example.com"
	srv := newServeStandIn(t)
	srv.token = "eyJ.the-action-s.id-token"
	srv.answers(nil, serveWaitsRunsDone(serveMarkdown(changedSummary)))
	oidc, asked := actionsIDTokens(t, audience, srv.token)
	rr := newRecipeRun(t, backtestHosts[0], repo)
	ran := filepath.Join(t.TempDir(), "ran")
	fake := filepath.Join(t.TempDir(), "lapilli")
	os.WriteFile(fake, []byte("#!/bin/sh\ntouch '"+ran+"'\nexit 1\n"), 0o755)
	idTokens := []string{"ACTIONS_ID_TOKEN_REQUEST_URL=" + oidc.URL + "/idtoken?api-version=2.0", "ACTIONS_ID_TOKEN_REQUEST_TOKEN=request-token-0123456789",
		"LAPILLI=" + fake, "SERVE_POLL=0"}
	out, err := runAction(t, rr, map[string]string{"paths": "alerts", "serve-url": srv.url, "audience": audience, "since": "14d", "args": "--tz UTC"}, idTokens...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if *asked != 1 || srv.sent()["days"] != json.Number("14") || srv.sent()["tz"] != "UTC" || !strings.Contains(rr.body(), "`CheckoutErrors`") {
		t.Errorf("asked %d, sent %v\n%s\n%s", *asked, srv.sent(), rr.body(), out)
	}
	if _, err := os.Stat(ran); err == nil || strings.Contains(out, "the release this action is at") || strings.Contains(out, "installed Lapilli") {
		t.Errorf("installed or ran lapilli:\n%s", out)
	}
	for _, c := range []struct {
		inputs map[string]string
		env    []string
		want   string
	}{
		{map[string]string{"paths": "alerts", "serve-url": srv.url}, idTokens, "action.yml: serve-url needs audience"},
		{map[string]string{"paths": "alerts", "audience": audience}, idTokens, "action.yml: audience and serve-wait are for serve-url: give it too"},
		{map[string]string{"paths": "alerts", "serve-wait": "10m"}, idTokens, "action.yml: audience and serve-wait are for serve-url"},
		{map[string]string{"paths": "alerts", "serve-url": srv.url, "audience": audience, "prometheus-url": "http://prometheus:9090"}, idTokens,
			"action.yml: prometheus-url: lapilli serve decides where the history is"},
		{map[string]string{"paths": "alerts", "serve-url": srv.url, "audience": audience, "incidents": "incidents.yaml"}, idTokens, "INCIDENTS: lapilli serve judges"},
		{map[string]string{"paths": "alerts", "serve-url": srv.url, "audience": audience, "serve-wait": "a while"}, idTokens, "SERVE_WAIT a while: want how long"},
		// Without id-token: write, GitHub gives the job no way to ask.
		{map[string]string{"paths": "alerts", "serve-url": srv.url, "audience": audience}, []string{"LAPILLI=" + fake},
			"give it permissions: id-token: write (GitHub Actions)"},
	} {
		if out, err := runAction(t, rr, c.inputs, c.env...); err == nil || !strings.Contains(out, c.want) {
			t.Errorf("%v: %v, want it to fail saying %q\n%s", c.inputs, err, c.want, out)
		}
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("lapilli ran")
	}
}
