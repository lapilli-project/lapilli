package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	"time"
	"unicode/utf16"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/rules"
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
		"alerts/.draft.yaml": "a draft, sent: hidden, read when it holds rules\n", "rules/extra.yaml": "groups: []\n", "expect.yaml": "expectations: []\n", "README.md": "x\n"})
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
			draft := "a draft, sent: hidden, read when it holds rules\n"
			wantBefore := map[string]string{"shop.yaml": shopRules("5m"), "team/api.yaml": shopRules("10m"), ".draft.yaml": draft, "extra.yaml": "groups: []\n"}
			wantAfter := map[string]string{"shop.yaml": shopRules("2h"), "team/api.yaml": shopRules("10m"), ".draft.yaml": draft, "extra.yaml": "groups: []\n"}
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
				"sending lapilli serve 4 files of the pull request's commit, and 4 of the commit it goes into", "LAPILLI passed over"} {
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

// LAPILLI_ID_TOKEN, the CI's ID token (GitLab's id_tokens:, Bitbucket's
// BITBUCKET_STEP_OIDC_TOKEN as the step gives it), is sent as the bearer
// token, kept out of what the recipe runs: neither curl's command line nor
// its environment has it, nor RENDER's, under either name.
func TestBacktestServeRecipeIDToken(t *testing.T) {
	needRecipeTools(t)
	for _, h := range []backtestHost{backtestHosts[2], backtestHosts[3]} {
		t.Run(h.kind, func(t *testing.T) {
			repo := newPRRepo(t, map[string]string{"chart/out.yaml": shopRules("5m")})
			repo.pullRequest(map[string]string{"chart/out.yaml": shopRules("2h")})
			srv := newServeStandIn(t)
			srv.token = "eyJhbGciOiJSUzI1NiJ9.eyJpc3MiOiJodHRwczovL2dpdGxhYi5leGFtcGxlLmNvbSJ9.c2lnbmF0dXJl"
			srv.answers(nil, []serveAnswer{serveDone(serveMarkdown(changedSummary))})
			rr := newRecipeRun(t, h, repo)
			var argv, envs string
			rr.env, argv, envs = curlShim(t, rr.env)
			seen := filepath.Join(t.TempDir(), "render-saw")
			env := []string{"LAPILLI_SERVE=" + srv.url, "LAPILLI_ID_TOKEN=" + srv.token, "SERVE_POLL=0",
				"RENDER=printf '%s' \"${LAPILLI_ID_TOKEN:-none}${BITBUCKET_STEP_OIDC_TOKEN:-none}\" >" + seen + "; cat chart/out.yaml"}
			if h.kind == "bitbucket" {
				env = append(env, "BITBUCKET_STEP_OIDC_TOKEN="+srv.token, "PR=7")
			}
			out, err := rr.run(env)
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if body := srv.sent(); !maps.Equal(sentFiles(t, body["after"]), map[string]string{"rules.yaml": shopRules("2h")}) ||
				!maps.Equal(sentFiles(t, body["before"]), map[string]string{"rules.yaml": shopRules("5m")}) {
				t.Errorf("what RENDER wrote isn't sent as rules.yaml: %v", body)
			}
			if b, _ := os.ReadFile(seen); string(b) != "nonenone" {
				t.Errorf("RENDER saw the ID token: %q", b)
			}
			args, _ := os.ReadFile(argv)
			e, _ := os.ReadFile(envs)
			if strings.Contains(string(args), srv.token) || strings.Contains(string(e), srv.token) {
				t.Error("the ID token on curl's command line, or in its environment")
			}
			if !strings.Contains(rr.body(), "| `CheckoutErrors` | `page` | 1 → 0 |") {
				t.Errorf("the comment:\n%s", rr.body())
			}
		})
	}
}

// A Bitbucket step's token that names its workspace's audience alone, from
// a step whose oidc: asks for none of serve's, isn't sent: serve would
// refuse it, and whatever trusts the workspace's audience takes it. Its
// payload is read, not verified, as base64url without padding, its aud a
// string or a list; capitals are the same. One with serve's audience
// beside the workspace's is sent.
func TestBacktestServeRecipeBitbucketDefaultAudience(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	def := "ari:cloud:bitbucket::workspace/4b1d9c2e-7a3f-4e5b-8c6d-0e1f2a3b4c5d"
	// jwt is a token of aud whose payload, encoded, has both of
	// base64url's own characters and no padding.
	jwt := func(aud any) string {
		for i := range 12 {
			for j := range 12 {
				x := strings.Repeat("?", i) + strings.Repeat("~", j)
				b, _ := json.Marshal(map[string]any{"iss": "https://api.bitbucket.org/2.0/workspaces/example/pipelines-config/identity/oidc", "aud": aud, "x": x})
				if p := base64.RawURLEncoding.EncodeToString(b); strings.Contains(p, "-") && strings.Contains(p, "_") && len(p)%4 != 0 {
					return "eyJhbGciOiJSUzI1NiJ9." + p + ".c2lnbmF0dXJl"
				}
			}
		}
		t.Fatal("no payload with - and _ and no padding")
		return ""
	}
	srv := newServeStandIn(t)
	for name, aud := range map[string]any{
		"a string":        def,
		"a list":          []string{def},
		"two workspaces'": []string{def, "ari:cloud:bitbucket::workspace/0e1f2a3b-4c5d-4e6f-8a7b-9c0d1e2f3a4b"},
		"in capitals":     strings.ToUpper(def),
	} {
		rr := serveRun(t, backtestHosts[3], repo, srv)
		out, err := rr.run([]string{"PATHS=alerts", "LAPILLI_SERVE_TOKEN_FILE=", "LAPILLI_ID_TOKEN=" + jwt(aud)})
		if err == nil || !strings.Contains(out, "LAPILLI_ID_TOKEN names Bitbucket's workspace audience alone") {
			t.Errorf("%s: %v\n%s", name, err, out)
		}
	}
	if posts, gets, _ := srv.counts(); posts+gets != 0 {
		t.Errorf("sent: %d POSTs, %d GETs", posts, gets)
	}
	srv.token = jwt([]string{def, "https://lapilli-ci.example.com"})
	srv.answers(nil, []serveAnswer{serveDone(serveMarkdown(changedSummary))})
	rr := serveRun(t, backtestHosts[3], repo, srv)
	if out, err := rr.run([]string{"PATHS=alerts", "LAPILLI_SERVE_TOKEN_FILE=", "LAPILLI_ID_TOKEN=" + srv.token}); err != nil {
		t.Fatalf("beside serve's: %v\n%s", err, out)
	}
	if posts, _, _ := srv.counts(); posts != 1 {
		t.Errorf("beside serve's: %d POSTs", posts)
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
// audience URL-encoded, right before the request, once for each. In GitHub
// Actions, github.sh masks it first thing, on stdout, as core.getIDToken
// does (a % as %25, as the runner reads a command); gitea.sh doesn't, as
// Forgejo documents no ::add-mask::, though its runner says GITHUB_ACTIONS
// too.
func TestBacktestServeRecipeAsksForIDToken(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	audience := "https://lapilli-ci.example.com/?team=a&b=c"
	for _, c := range []struct {
		h       backtestHost
		actions bool
		mask    string
	}{
		{backtestHosts[0], true, "::add-mask::eyJ.actions%25id-token.github\n"},
		{backtestHosts[0], false, ""},
		{backtestHosts[1], true, ""},
	} {
		t.Run(fmt.Sprintf("%s, in Actions %v", c.h.kind, c.actions), func(t *testing.T) {
			srv := newServeStandIn(t)
			srv.token = "eyJ.actions%id-token." + c.h.kind
			srv.answers(nil, []serveAnswer{serveDone(serveMarkdown(changedSummary))})
			oidc, asked := actionsIDTokens(t, audience, srv.token)
			rr := newRecipeRun(t, c.h, repo)
			var argv string
			rr.env, argv, _ = curlShim(t, rr.env)
			if c.actions {
				rr.env = append(rr.env, "GITHUB_ACTIONS=true", "FORGEJO_ACTIONS=true")
			}
			stdout, stderr, err := rr.runSplit([]string{"PATHS=alerts", "LAPILLI_SERVE=" + srv.url, "LAPILLI_SERVE_AUDIENCE=" + audience, "SERVE_AUDIENCE_ANY=1", "SERVE_POLL=0",
				"ACTIONS_ID_TOKEN_REQUEST_URL=" + oidc.URL + "/idtoken?api-version=2.0", "ACTIONS_ID_TOKEN_REQUEST_TOKEN=request-token-0123456789"})
			if err != nil || *asked != 1 || !strings.Contains(rr.body(), "`CheckoutErrors`") {
				t.Fatalf("%v, asked %d\n%s%s", err, *asked, stdout, stderr)
			}
			if args, _ := os.ReadFile(argv); strings.Contains(string(args), "request-token-0123456789") || strings.Contains(string(args), srv.token) {
				t.Errorf("a token on curl's command line:\n%s", args)
			}
			if c.mask != "" && !strings.HasPrefix(stdout, c.mask) || c.mask == "" && strings.Contains(stdout+stderr, "add-mask") {
				t.Errorf("stdout %q, want it to begin %q", stdout, c.mask)
			}
			if strings.Contains(stdout+stderr, srv.token) {
				t.Errorf("the token in the job's log:\n%s%s", stdout, stderr)
			}
		})
	}
}

// runSplit is run, what the recipe wrote on stdout apart from its stderr:
// GitHub Actions reads ::add-mask:: on stdout.
func (rr *recipeRun) runSplit(settings []string, args ...string) (string, string, error) {
	rr.t.Helper()
	script, err := filepath.Abs(backtestRecipes + rr.h.script)
	if err != nil {
		rr.t.Fatal(err)
	}
	shell := os.Getenv("RECIPE_SH")
	if shell == "" {
		shell = "sh"
	}
	cmd := exec.Command(shell, append([]string{script}, args...)...)
	cmd.Dir = rr.repo.dir
	cmd.Env = append(append([]string{}, rr.env...), settings...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	rr.host.set(func(*commentHost) {}) // as run orders them
	err = cmd.Run()
	rr.host.set(func(*commentHost) {})
	return stdout.String(), stderr.String(), err
}

// LAPILLI_SERVE_AUDIENCE, the audience the recipe asks Actions for an ID
// token for, isn't a cloud's, nor the CI's own default, which other
// services take a token for, and serve, or whoever is on the way to it,
// could use there; the job's log says when it isn't LAPILLI_SERVE. The
// action's audience is it.
func TestBacktestServeRecipeAudience(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	srv := newServeStandIn(t)
	rr := newRecipeRun(t, backtestHosts[0], repo)
	ask := []string{"PATHS=alerts", "LAPILLI_SERVE=" + srv.url, "SERVE_POLL=0",
		"ACTIONS_ID_TOKEN_REQUEST_URL=https://token.example.com/idtoken?api-version=2.0", "ACTIONS_ID_TOKEN_REQUEST_TOKEN=request-token-0123456789"}
	for _, aud := range []string{"sts.amazonaws.com", "https://sts.amazonaws.com", "sts.us-east-1.amazonaws.com", "STS.AmazonAWS.com",
		"api://AzureADTokenExchange", "API://azureadtokenexchange", "api://AzureADTokenExchangeUSGov",
		"//iam.googleapis.com/projects/123456789/locations/global/workloadIdentityPools/ci/providers/github",
		"https://iam.googleapis.com/projects/123456789/locations/global/workloadIdentityPools/ci/providers/github",
		"https://sts.googleapis.com", "https://sts.googleapis.com/v1/token", "sigstore", "https://github.com", "https://github.com/",
		"https://GitHub.com/example"} {
		out, err := rr.run(append(slices.Clone(ask), "LAPILLI_SERVE_AUDIENCE="+aud))
		if err == nil || !strings.Contains(out, "LAPILLI_SERVE_AUDIENCE "+aud+": a cloud's audience, or the CI's own default") {
			t.Errorf("%s: %v\n%s", aud, err, out)
		}
	}
	if posts, _, _ := srv.counts(); posts != 0 || len(rr.host.comments) != 0 {
		t.Errorf("%d POSTs, %d comments", posts, len(rr.host.comments))
	}
	// Another than serve's own URL is refused, those a list of others'
	// misses among them, unless SERVE_AUDIENCE_ANY=1.
	for _, aud := range []string{"https://lapilli-ci.example.com", "sts.amazonaws.com.cn", "pypi", "npm:registry.npmjs.org", "https://github.example.com/example"} {
		out, err := rr.run(append(slices.Clone(ask), "LAPILLI_SERVE_AUDIENCE="+aud))
		if err == nil || !strings.Contains(out, "SERVE_AUDIENCE_ANY=1 for a trust file that names another") {
			t.Errorf("%s: %v\n%s", aud, err, out)
		}
	}
	// Serve's own, and, with SERVE_AUDIENCE_ANY=1, one that isn't its
	// URL, said.
	for _, c := range []struct {
		aud  string
		said bool
	}{{srv.url + "/", false}, {srv.url, false}, {"https://lapilli-ci.example.com", true}, {"https://github.community/x", true}} {
		srv.token = "eyJ.for." + c.aud
		srv.answers(nil, []serveAnswer{serveDone(serveMarkdown(changedSummary))})
		oidc, _ := actionsIDTokens(t, c.aud, srv.token)
		rr.host.set(func(h *commentHost) { h.comments = nil })
		settings := append(slices.Clone(ask[:3]), "LAPILLI_SERVE_AUDIENCE="+c.aud, "ACTIONS_ID_TOKEN_REQUEST_URL="+oidc.URL+"/idtoken?api-version=2.0",
			"ACTIONS_ID_TOKEN_REQUEST_TOKEN=request-token-0123456789")
		if c.said {
			settings = append(settings, "SERVE_AUDIENCE_ANY=1")
		}
		out, err := rr.run(settings)
		said := strings.Contains(out, "LAPILLI_SERVE_AUDIENCE "+c.aud+" isn't LAPILLI_SERVE, "+srv.url+", where the token goes")
		if err != nil || said != c.said || !strings.Contains(rr.body(), "`CheckoutErrors`") {
			t.Errorf("%s: %v, said %v\n%s", c.aud, err, said, out)
		}
	}
	// The action's audience.
	out, err := runAction(t, rr, map[string]string{"paths": "alerts", "serve-url": srv.url, "audience": "api://AzureADTokenExchange"},
		"ACTIONS_ID_TOKEN_REQUEST_URL=https://token.example.com/idtoken?api-version=2.0", "ACTIONS_ID_TOKEN_REQUEST_TOKEN=request-token-0123456789")
	if err == nil || !strings.Contains(out, "LAPILLI_SERVE_AUDIENCE api://AzureADTokenExchange: a cloud's audience") {
		t.Errorf("the action's audience: %v\n%s", err, out)
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
// again: the POST, and the GET. SERVE_RETRY_MIN, for tests, lowers the
// least it waits from 5 seconds to Retry-After's 1.
func TestBacktestServeRecipeRetryAfter(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	srv := newServeStandIn(t)
	rr := serveRun(t, backtestHosts[0], repo, srv)
	rr.env = append(rr.env, "SERVE_RETRY_MIN=1")
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

// pageEveryone is a rule no expectation holds, which pages at once.
const pageEveryone = `groups:
  - name: sneaky
    rules:
      - alert: PageEveryone
        expr: vector(1)
        labels: {severity: page}
`

// A rule file whose name, or a directory's in its path, begins with a dot
// is one Prometheus's rule_files glob loads (alerts/*.yaml reads
// alerts/.page.yaml and alerts/..page.yaml, alerts/*/*.yaml
// alerts/.d/page.yaml and alerts/..d/page.yaml), and yaml.v3 reads one in
// UTF-16 with its BOM: backtested as any other, its rule held to the
// expectations, and the job failing as for a file without a dot, in
// either mode. In serve mode each is sent, but one in UTF-16, which is
// trouble, as any rule file that isn't UTF-8 text; a hidden file that
// isn't text and has no BOM, a .DS_Store, isn't sent, and isn't trouble.
func TestBacktestRecipeHiddenRuleFile(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	base := map[string]string{"alerts/shop.yaml": shopRules("5m"), "expect.yaml": "expectations:\n  - rule: CheckoutErrors\n    budget: {week: 5}\n"}
	gates := []string{"PATHS=alerts", "EXPECT=expect.yaml", "EXPECT_REQUIRED=true", "FAIL_ON_EXPECTATIONS=true", "FAIL_ON_ERROR=true"}
	for path, content := range map[string]string{"alerts/page.yaml": pageEveryone, "alerts/.page.yaml": pageEveryone, "alerts/..page.yaml": pageEveryone,
		"alerts/.d/page.yaml": pageEveryone, "alerts/..d/page.yaml": pageEveryone, "alerts/.u16.yaml": utf16LE(pageEveryone)} {
		repo := newPRRepo(t, base)
		repo.pullRequest(map[string]string{path: content})
		rr := newRecipeRun(t, backtestHosts[0], repo)
		out, err := rr.run(gates, "--prometheus", prom.URL, "--tz", "UTC")
		if err == nil || rr.host.posts == 0 || !strings.Contains(rr.body(), "PageEveryone") {
			t.Errorf("%s, local: the job passed, or said nothing of PageEveryone (%v, %d posts)\n%s", path, err, rr.host.posts, out)
		}
	}
	repo := newPRRepo(t, base)
	repo.pullRequest(map[string]string{"alerts/.page.yaml": pageEveryone, "alerts/..page.yaml": pageEveryone, "alerts/.d/page.yaml": pageEveryone,
		"alerts/.DS_Store": "\x00\x00\x00\x01Bud1\x00\xff\xfe", "alerts/..2026_11_20/shop.yaml": shopRules("1h")})
	srv := newServeStandIn(t)
	srv.answers(nil, []serveAnswer{serveDone(serveMarkdown(`{"nothing_changed":false,"changed":0,"added":1,"removed":0,"same":2,"alike":0,"unreplayed":0,"expected":true,"expectations_broken":0,"unexpected":1}`))})
	sr := serveRun(t, backtestHosts[0], repo, srv)
	out, _ := sr.run(gates, "--since", "7d")
	after := sentFiles(t, srv.sent()["after"])
	if want := []string{"..2026_11_20/shop.yaml", "..page.yaml", ".d/page.yaml", ".page.yaml", "shop.yaml"}; !slices.Equal(slices.Sorted(maps.Keys(after)), want) || after["..page.yaml"] != pageEveryone {
		t.Errorf("sent %v, want %v\n%s", slices.Sorted(maps.Keys(after)), want, out)
	}
	if strings.Contains(out, "not UTF-8") {
		t.Errorf("a hidden file that isn't text was trouble:\n%s", out)
	}
	// One in UTF-16, hidden: trouble, nothing sent.
	repo = newPRRepo(t, base)
	repo.pullRequest(map[string]string{"alerts/.u16.yaml": utf16LE(strings.ReplaceAll(pageEveryone, "PageEveryone", "PageInUTF16"))})
	srv = newServeStandIn(t)
	srv.answers(nil, nil)
	sr = serveRun(t, backtestHosts[0], repo, srv)
	out, err := sr.run(gates, "--since", "7d")
	if posts, _, _ := srv.counts(); err == nil || posts != 0 || !strings.Contains(out, "not UTF-8 text, in the pull request's commit: .u16.yaml") {
		t.Errorf("a hidden rule file in UTF-16, serve mode: %v, %d POSTs\n%s", err, posts, out)
	}
	failure(t, sr.body(), "a rule file in the pull request's commit isn't UTF-8 text, which lapilli serve takes alone: not sent (the job's log says which)")
}

// utf16LE is s in UTF-16LE with its BOM, which Prometheus's rulefmt
// (yaml.v3) reads as it reads UTF-8.
func utf16LE(s string) string {
	u := utf16.Encode(append([]rune{0xFEFF}, []rune(s)...))
	b := make([]byte, 0, 2*len(u))
	for _, c := range u {
		b = append(b, byte(c), byte(c>>8))
	}
	return string(b)
}

// LAPILLI_SERVE is https, or plain http to this machine or to a Service in
// a cluster, NAME.NAMESPACE.svc and NAME.NAMESPACE.svc.cluster.local: a
// runner in the cluster reaches the chart's Service in plain http, through
// no proxy, whatever http_proxy says, where the token would be in the
// clear.
func TestBacktestServeRecipeClusterURL(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	srv := newServeStandIn(t)
	u, _ := url.Parse(srv.url)
	// A proxy that takes nothing: a request that went to it would fail. The
	// stand-in host's API, at 127.0.0.1, is passed by.
	proxy := []string{"http_proxy=http://127.0.0.1:9", "HTTP_PROXY=http://127.0.0.1:9", "all_proxy=http://127.0.0.1:9", "ALL_PROXY=http://127.0.0.1:9",
		"no_proxy=127.0.0.1", "NO_PROXY=127.0.0.1"}
	for _, name := range []string{"lapilli-backtest.monitoring.svc", "Lapilli-Backtest.Monitoring.svc.cluster.local", "localhost"} {
		t.Run(name, func(t *testing.T) {
			srv.answers(nil, []serveAnswer{serveDone(serveMarkdown(changedSummary))})
			rr := serveRun(t, backtestHosts[0], repo, srv)
			// The name, as the cluster's DNS would answer it: here.
			var argv string
			rr.env, argv, _ = curlShim(t, rr.env, "--connect-to", strings.ToLower(name)+":8084:"+u.Host)
			rr.env = append(append(rr.env, proxy...), "LAPILLI_SERVE=http://"+name+":8084/")
			out, err := rr.run([]string{"PATHS=alerts"})
			if posts, _, _ := srv.counts(); err != nil || posts != 1 || !strings.Contains(rr.body(), "`CheckoutErrors`") {
				t.Errorf("%v, %d POSTs\n%s\n%s", err, posts, rr.body(), out)
			}
			if args, _ := os.ReadFile(argv); !strings.Contains(string(args), "\n--noproxy\n*\n") {
				t.Errorf("no --noproxy '*' for plain http:\n%s", args)
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
		{nil, []string{"--grafana", "https://grafana.example.com"}, "--grafana: lapilli serve backtests the rules read from PATHS, LOKI_PATHS or RENDER against its own history"},
		{nil, []string{"--loki-tenant", "a"}, "--loki-tenant: lapilli serve backtests the rules read from PATHS, LOKI_PATHS or RENDER against its own history"},
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
		// A name of one label is looked up under the resolver's search
		// domains; NAME.NAMESPACE.svc's labels are DNS labels, a letter
		// first.
		{[]string{"LAPILLI_SERVE=http://lapilli-backtest:8084"}, nil, "want lapilli serve's backtest port"},
		{[]string{"LAPILLI_SERVE=http://lapilli-backtest"}, nil, "want lapilli serve's backtest port"},
		{[]string{"LAPILLI_SERVE=http://lapilli-backtest.monitoring.svc.example.com"}, nil, "want lapilli serve's backtest port"},
		{[]string{"LAPILLI_SERVE=http://a.lapilli-backtest.monitoring.svc"}, nil, "want lapilli serve's backtest port"},
		{[]string{"LAPILLI_SERVE=http://1backtest.monitoring.svc"}, nil, "want lapilli serve's backtest port"},
		{[]string{"LAPILLI_SERVE=http://backtest.monitoring-.svc"}, nil, "want lapilli serve's backtest port"},
		{[]string{"LAPILLI_SERVE=http://backtest..svc"}, nil, "want lapilli serve's backtest port"},
		{[]string{"LAPILLI_SERVE=http://" + strings.Repeat("a", 64) + ".monitoring.svc"}, nil, "want lapilli serve's backtest port"},
		{[]string{"LAPILLI_SERVE=http://backtest.monitoring.svc:80x"}, nil, "want lapilli serve's backtest port"},
		{[]string{"LAPILLI_SERVE=http://backtest.monitoring.svc:123456"}, nil, "want lapilli serve's backtest port"},
		{[]string{"LAPILLI_SERVE=http://localhost.:8084"}, nil, "want lapilli serve's backtest port"},
		{[]string{"LAPILLI_SERVE=http://127.0.0.2:8084"}, nil, "want lapilli serve's backtest port"},
		{[]string{"LAPILLI_SERVE=http://cluster.local"}, nil, "want lapilli serve's backtest port"},
		{[]string{"LAPILLI_SERVE=http://10.0.0.8:8084"}, nil, "want lapilli serve's backtest port"},
		{[]string{"LAPILLI_SERVE=http://[fd00::1]:8084"}, nil, "want lapilli serve's backtest port"},
		{[]string{"LAPILLI_SERVE=http://ci@127.0.0.1:8084"}, nil, "want lapilli serve's backtest port"},
		{[]string{"LAPILLI_SERVE=https://lapilli-ci.example.com/?x=1"}, nil, "want lapilli serve's backtest port"},
		{[]string{"LAPILLI_SERVE=ftp://lapilli-ci.example.com"}, nil, "want lapilli serve's backtest port"},
		{[]string{"SERVE_WAIT=ten"}, nil, "SERVE_WAIT ten: want how long to wait for lapilli serve"},
		{[]string{"SERVE_WAIT=2d"}, nil, "SERVE_WAIT 2d: want how long"},
		{[]string{"SERVE_POLL=61"}, nil, "SERVE_POLL 61: want how many seconds apart"},
		{[]string{"SERVE_POLL=x"}, nil, "SERVE_POLL x: want how many seconds apart"},
		{[]string{"SERVE_RETRY_MIN=6"}, nil, "SERVE_RETRY_MIN 6: want the least seconds"},
		{[]string{"SERVE_RETRY_MIN=-1"}, nil, "SERVE_RETRY_MIN -1: want the least seconds"},
		// Serve mode's alone, without it.
		{[]string{"LAPILLI_SERVE=", "SERVE_WAIT=30m"}, nil, "SERVE_WAIT is for a backtest through lapilli serve: give LAPILLI_SERVE"},
		{[]string{"LAPILLI_SERVE=", "SERVE_POLL=", "SERVE_RETRY_MIN=0"}, nil, "SERVE_RETRY_MIN is for a backtest through lapilli serve"},
		{[]string{"LAPILLI_SERVE=", "SERVE_POLL=", "LAPILLI_SERVE_TOKEN_FILE=" + tok}, nil, "LAPILLI_SERVE_TOKEN_FILE is for a backtest through lapilli serve"},
		{[]string{"LAPILLI_SERVE=", "SERVE_POLL=", "LAPILLI_SERVE_TOKEN_FILE=", "LAPILLI_ID_TOKEN=eyJ.a.b"}, nil, "LAPILLI_ID_TOKEN is for a backtest through lapilli serve"},
		{[]string{"LAPILLI_SERVE=", "SERVE_POLL=", "LAPILLI_SERVE_TOKEN_FILE=", "LAPILLI_SERVE_AUDIENCE=https://lapilli-ci.example.com"}, nil,
			"LAPILLI_SERVE_AUDIENCE is for a backtest through lapilli serve"},
		{[]string{"LAPILLI_SERVE=", "SERVE_POLL=", "LAPILLI_SERVE_TOKEN_FILE=", "LOKI_PATHS=loki"}, nil, "LOKI_PATHS is for a backtest through lapilli serve"},
		// A Loki ruler's files: in the repository, none of PATHS, not with
		// RENDER.
		{[]string{"LOKI_PATHS=../loki"}, nil, "LOKI_PATHS: ../loki isn't a path in the repository"},
		{[]string{"LOKI_PATHS=alerts/"}, nil, "LOKI_PATHS: alerts/ is in PATHS too"},
		{[]string{"PATHS=", "RENDER=cat alerts/shop.yaml", "LOKI_PATHS=loki"}, nil, "LOKI_PATHS goes with PATHS"},
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

// LOKI_PATHS's files are sent beside PATHS's, each marked a Loki ruler's,
// as --loki-rules would read them; a path of it in neither commit is said.
func TestBacktestServeRecipeLokiPaths(t *testing.T) {
	needRecipeTools(t)
	logs := "groups:\n  - name: logs\n    rules:\n      - alert: LogErrors\n        expr: sum(count_over_time({app=\"x\"}[5m])) > %d\n"
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m"), "loki/logs.yaml": fmt.Sprintf(logs, 1)})
	repo.pullRequest(map[string]string{"loki/logs.yaml": fmt.Sprintf(logs, 2)})
	srv := newServeStandIn(t)
	srv.answers(nil, serveWaitsRunsDone(serveMarkdown(changedSummary)))
	rr := serveRun(t, backtestHosts[0], repo, srv)
	if out, err := rr.run([]string{"PATHS=alerts", "LOKI_PATHS=loki"}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	body := srv.sent()
	for side, want := range map[string]string{"before": fmt.Sprintf(logs, 1), "after": fmt.Sprintf(logs, 2)} {
		var lokis []string
		list, _ := body[side].([]any)
		for _, f := range list {
			m, _ := f.(map[string]any)
			switch {
			case m["loki"] == true && m["path"] == "logs.yaml" && m["content"] == want:
				lokis = append(lokis, "logs.yaml")
			case m["loki"] != nil:
				t.Errorf("%s: %v", side, m)
			}
		}
		if len(lokis) != 1 || len(list) != 2 {
			t.Errorf("%s: %v", side, list)
		}
	}
	if out, err := serveRun(t, backtestHosts[0], repo, srv).run([]string{"PATHS=alerts", "LOKI_PATHS=lokki"}); err == nil || !strings.Contains(out, "LOKI_PATHS: lokki: in neither commit") {
		t.Errorf("%v\n%s", err, out)
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
	// The stand-in's URL isn't the audience its trust would name: taken
	// only with audience-any.
	if out, err := runAction(t, rr, map[string]string{"paths": "alerts", "serve-url": srv.url, "audience": audience}, idTokens...); err == nil ||
		!strings.Contains(out, "SERVE_AUDIENCE_ANY=1 for a trust file that names another") {
		t.Errorf("an audience not serve-url's, no audience-any: %v\n%s", err, out)
	}
	out, err := runAction(t, rr, map[string]string{"paths": "alerts", "serve-url": srv.url, "audience": audience, "audience-any": "true", "since": "14d", "args": "--tz UTC"}, idTokens...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if *asked != 1 || srv.sent()["days"] != json.Number("14") || srv.sent()["tz"] != "UTC" || !strings.Contains(rr.body(), "`CheckoutErrors`") {
		t.Errorf("asked %d, sent %v\n%s\n%s", *asked, srv.sent(), rr.body(), out)
	}
	// Masked, as core.getIDToken masks it.
	if !strings.Contains(out, "::add-mask::"+srv.token+"\n") || strings.Count(out, srv.token) != 1 {
		t.Errorf("the ID token isn't masked first:\n%s", out)
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
		{map[string]string{"paths": "alerts", "audience": audience}, idTokens, "action.yml: audience, audience-any and serve-wait are for serve-url: give it too"},
		{map[string]string{"paths": "alerts", "serve-wait": "10m"}, idTokens, "action.yml: audience, audience-any and serve-wait are for serve-url"},
		{map[string]string{"paths": "alerts", "serve-url": srv.url, "audience": audience, "prometheus-url": "http://prometheus:9090"}, idTokens,
			"action.yml: prometheus-url: lapilli serve decides where the history is"},
		{map[string]string{"paths": "alerts", "serve-url": srv.url, "audience": audience, "incidents": "incidents.yaml"}, idTokens, "INCIDENTS: lapilli serve judges"},
		{map[string]string{"paths": "alerts", "serve-url": srv.url, "audience": audience, "audience-any": "true", "serve-wait": "a while"}, idTokens, "SERVE_WAIT a while: want how long"},
		{map[string]string{"paths": "alerts", "serve-url": srv.url, "audience": audience, "audience-any": "maybe"}, idTokens, "audience-any: want true or false"},
		// Without id-token: write, GitHub gives the job no way to ask.
		{map[string]string{"paths": "alerts", "serve-url": srv.url, "audience": audience, "audience-any": "true"}, []string{"LAPILLI=" + fake},
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

// leftOut is the line the recipe puts in place of lines of serve's it
// doesn't post, one for each run of them.
const leftOut = "(what lapilli serve wrote here isn't Markdown lapilli writes: left out, the job's log has it)"

// realReports are comparisons as CompareMarkdown writes them, as lapilli
// serve does, judged by its record: rules that differ, are added and taken
// away, that couldn't be replayed, fire alike (<details>), incidents
// caught differently, expectations, recording rules, what was passed over,
// names that would be a mention, a link, HTML or a quick action were they
// not code spans, a report cut short, one with nothing changed.
func realReports(t *testing.T) map[string]string {
	t.Helper()
	t0 := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	m := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }
	opts := verdict.Options{From: t0, To: t0.Add(7 * 24 * time.Hour), Lead: 15 * time.Minute, Match: []string{"service"},
		RefireWithin: time.Hour, UnknownLen: time.Hour}
	page, ticket := map[string]string{"severity": "page"}, map[string]string{"severity": "ticket"}
	incs := []incident.Incident{{ID: "INC-1", Title: "Checkout errors", Start: m(100), End: m(140)},
		{ID: "INC-2", Title: "Search slow", Start: m(2000), End: m(2020)}}
	judged := func() *verdict.Comparison {
		before := verdict.Judge(
			[]rules.Rule{{Name: "CheckoutErrors", Expr: "x", Labels: page}, {Name: "Flappy", Expr: "y", Labels: ticket},
				{Name: "Typo", Unsupported: "query failed: bad_data"}, {Name: "Same", Expr: "s"}},
			[]firing.Firing{{Rule: "CheckoutErrors", Labels: page, Start: m(102), End: m(130)}, {Rule: "Flappy", Labels: ticket, Start: m(10), End: m(12)},
				{Rule: "Flappy", Labels: ticket, Start: m(40), End: m(42)}, {Rule: "Flappy", Labels: ticket, Start: m(500), End: m(502)}}, incs, opts)
		after := verdict.Judge(
			[]rules.Rule{{Name: "CheckoutErrors", Expr: "x", For: 20 * time.Minute, Labels: page}, {Name: "SearchLatency", Expr: "w"},
				{Name: "Typo", Expr: "z"}, {Name: "Same", Expr: "s2"}},
			[]firing.Firing{{Rule: "CheckoutErrors", Labels: page, Start: m(120), End: m(130)}, {Rule: "SearchLatency", Start: m(2005), End: m(2015)}}, incs, opts)
		c := verdict.Compare(before, after, []string{"CheckoutErrors", "Flappy", "SearchLatency", "Typo", "Same"}, 40)
		c.JudgedBy = verdict.JudgedByServe
		return c
	}
	write := func(c *verdict.Comparison, incidents bool, loc *time.Location) string {
		t.Helper()
		if c.Summary == nil {
			c.Summarize(incidents)
		}
		var b bytes.Buffer
		if err := verdict.CompareMarkdown(&b, c, loc); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	zone := func(name string) *time.Location {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Skipf("no zone database: %v", err)
		}
		return loc
	}
	out := map[string]string{}
	out["judged"] = write(judged(), true, zone("Asia/Seoul"))

	// Counted, not judged, in a zone whose clocks change in the period;
	// what reading the rules passed over; recording rules that differ.
	c := judged()
	for _, r := range []*verdict.Report{c.Before, c.After} {
		r.Incidents = nil
	}
	c.Incidents = nil
	c.From, c.To = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	c.PassedOver = map[string]string{"rules": "1 document that holds no rules", "compare": "2 documents of rules the selector doesn't match"}
	c.Recording = []verdict.RecordingChange{{Record: "shop:errors:rate5m", Group: "shop", Change: "changed"}, {Record: "@octocat", Group: "#1", Change: "added"}}
	out["counted"] = write(c, false, zone("Europe/Berlin"))

	// No record of the period: counted, saying why.
	c = judged()
	c.Before.NoRecord = true
	c.Before.Settings = &verdict.Settings{ImpactNoData: true}
	out["no record"] = write(c, true, time.UTC)

	// Held to expectations: one broken by the change, a catch, rules added
	// that none holds.
	c = judged()
	c.Summarize(true)
	after, err := verdict.ReadExpectations(strings.NewReader("expectations:\n  - rule: CheckoutErrors\n    budget: {week: 0}\n" +
		"    catches: [{incident: INC-1, within: 10m}]\n  - rule: Flappy\n    budget: {week: 1, asleep: 0}\n"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := verdict.ReadExpectations(strings.NewReader("expectations:\n  - rule: CheckoutErrors\n    budget: {week: 3}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Expect(after, before, nil); err != nil {
		t.Fatal(err)
	}
	out["expectations"] = write(c, true, time.UTC)

	// What comes from the rules, anything at all, as code spans.
	hostile := []string{"@octocat please look", "@example-org/oncall", "#123", "example/repo#5", "GH-7", "!12", "~3", "$4", "&5", "%v1",
		"/merge", "/approve", "  /label ~x", "<!-- the rest is hidden", "<img src=x onerror=alert(1)>", "</details><details open>", "</summary>",
		"[click](https://evil.example)", "https://evil.example/x", "www.evil.example", "![x](https://evil.example/p.png)", "mailto:a@example.com",
		"a`b", "``fenced``", "```\ncode\n```", "`", " lead and trail ", "a|b", `a\|b`, `a\`, "| x | y |", "**bold** _em_ ~~gone~~",
		"line\nbreak", "car\rriage", "tab\there", "right‮to left", "zero​width", "line sep", "no break", "fillerㅤx",
		":smile:", "[//]: # (lapilli-backtest default)", "&#64;octocat", "\\@octocat", "디스크 부족 @팀", strings.Repeat("long", 200)}
	var rs []rules.Rule
	var fs []firing.Firing
	var hincs []incident.Incident
	for i, h := range hostile {
		labels := map[string]string{"severity": h}
		rs = append(rs, rules.Rule{Name: h, Expr: "x", Labels: labels})
		fs = append(fs, firing.Firing{Rule: h, Labels: labels, Start: m(100 + 10*i), End: m(105 + 10*i)})
		hincs = append(hincs, incident.Incident{ID: h, Title: h, Start: m(100 + 10*i), End: m(108 + 10*i)})
	}
	alike := rules.Rule{Name: "@channel alike", Expr: "q"}
	hb := verdict.Judge(append(slices.Clone(rs), rules.Rule{Name: hostile[0] + " broken", Unsupported: hostile[14] + " " + hostile[17]}, alike), nil, hincs, opts)
	ha := verdict.Judge(append(slices.Clone(rs), rules.Rule{Name: "@channel alike", Expr: "q2"}), fs, hincs, opts)
	c = verdict.Compare(hb, ha, append(slices.Clone(hostile), hostile[0]+" broken", alike.Name), 0)
	c.JudgedBy = verdict.JudgedByServe
	out["hostile"] = write(c, true, time.UTC)

	// Cut short by CompareMarkdown itself, near its 64000 bytes.
	var lrs []rules.Rule
	var lfs []firing.Firing
	var names []string
	for i := range 3000 {
		name := fmt.Sprintf("Rule%04d%s", i, strings.Repeat("x", 150))
		lrs = append(lrs, rules.Rule{Name: name, Expr: "x"})
		lfs = append(lfs, firing.Firing{Rule: name, Start: m(i), End: m(i + 1)})
		names = append(names, name)
	}
	c = verdict.Compare(verdict.Judge(lrs, lfs[2000:], nil, opts), verdict.Judge(lrs, lfs, nil, opts), names, 0)
	c.JudgedBy = verdict.JudgedByServe
	out["cut short"] = write(c, false, time.UTC)
	if n := len(out["cut short"]); n < verdict.MarkdownLimit-1000 || !strings.Contains(out["cut short"], "**Cut short**") {
		t.Fatalf("a report cut short of %d bytes", n)
	}

	none := &verdict.Comparison{From: t0, To: m(60), Same: 42, Rules: []verdict.RuleChange{}, Incidents: []verdict.IncidentChange{}}
	out["nothing changed"] = write(none, false, time.UTC)
	return out
}

// A report CompareMarkdown wrote is posted as serve wrote it, byte for
// byte, whatever came from the rules: each a code span.
func TestBacktestServeRecipeReportAsWritten(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	srv := newServeStandIn(t)
	rr := serveRun(t, backtestHosts[2], repo, srv)
	reports := realReports(t)
	for _, name := range slices.Sorted(maps.Keys(reports)) {
		md := reports[name]
		t.Run(name, func(t *testing.T) {
			srv.answers(nil, []serveAnswer{serveDone(md)})
			// A comment to edit, so that nothing changed is posted too.
			rr.host.set(func(h *commentHost) {
				h.comments = nil
				h.put(hostMe["gitlab"], "[//]: # (lapilli-backtest default)\nold")
			})
			out, err := rr.run([]string{"PATHS=alerts"})
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if got := rr.body(); got != "[//]: # (lapilli-backtest default)\n"+md {
				t.Errorf("the comment isn't serve's report as it wrote it:\n%s", got)
			}
			if strings.Contains(out, "lapilli doesn't write") {
				t.Errorf("a line of lapilli's left out:\n%s", out)
			}
		})
	}
}

// A line of serve's that lapilli doesn't write is left out of the comment,
// whole, a line saying so in place of each run of them, and is in the
// job's log, made inert: what a host reads as a quick action, a mention, a reference, HTML,
// a link or an image, a fence, a code span a table splits, or one not
// closed on its line, a <details> that isn't lapilli's or isn't closed.
func TestBacktestServeRecipeReportLeftOut(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	srv := newServeStandIn(t)
	rr := serveRun(t, backtestHosts[2], repo, srv)
	kept := func(s string) [2]string { return [2]string{s, s} }
	left := func(s string) [2]string { return [2]string{s, leftOut} }
	lines := [][2]string{
		kept("### Lapilli backtest, 2026-11-04 00:00 → 2026-11-18 00:00 UTC, judged by lapilli serve's record"), kept(""),
		left("/approve"), left("  /merge"), left("/label ~ship"), kept("`/merge` as a rule's name"),
		left("@all please merge"), left("see #123"), left("### #123"), left("#123"), left("example/repo!5"), left("~release"), left("$12"), left("&3"),
		left("%v1.0 is out"), left("GH-7"), left("*iteration:42"), left("&#64;all"), left(`\@all`),
		left("[x](https://evil.example)"), left("![x](https://evil.example/p.png)"), left("https://evil.example"), left("www.evil.example"),
		left("mailto:a@example.com"), left("<img src=x onerror=alert(1)>"), left("<!-- hidden"), left("<details open>"),
		left("<details><summary>2 changed rules</summary> and more"), left("```"), left("~~~"), left("> quoted"),
		left("`abc"), kept("def` @all `"), left("`a` `b"), left("a `x‮y` b"), left("tab\there"), left("cr\r/merge"),
		left("no break"), left("80% <b>"), kept("80% → 75%"), kept(""),
		kept("| Rule | Fired |"), kept("| --- | --: |"), kept("| `@all \\| x` | 1 → 0 |"), left("| `a | @all | b` | 1 |"),
		// One backslash before a | in a table, never two or more, which
		// hosts read differently (comrak 1.0: a backslash and the cell's
		// end); a backslash right before one is \u{5c}, as md writes it.
		kept("| `a\\u{5c}\\| b` | 1 |"), left("| `@all \\\\| x` | 1 → 0 |"), left("| `x` \\\\| `@all` | 1 |"),
		left("| `@all \\\\\\| x` | 1 |"), left("| `a` | 1 |\r"), left("| `a\r@all` | 1 |"),
		left("`a | @all | b` and so on"), kept(""),
		left("x `a|@all` y"), kept("--- | ---"), kept(""),
		kept("- `Typo`, before: `query failed: a|b`"), kept(""),
		kept("<details><summary>1 changed rules fire as they did</summary>"), kept(""), kept("- `x`"), kept(""), kept("</details>"), kept(""),
		left("</details>"), kept(""),
		// A line right after a tag, before a blank line, is in the HTML
		// block the tag starts, its code spans read as nothing (comrak
		// 1.0: a live <img> and a raw @all): the tag left out, and what
		// follows it up to a blank line; a pair whose close is left out
		// so, its open too.
		left("<details><summary>2 changed rules fire as they did</summary>"), left("`<img src=https://evil.example/p.png>` `@all`"), left("- `z`"), kept(""),
		left("</details>"), kept(""),
		left("<details><summary>4 changed rules fire as they did</summary>"), kept(""), kept("- `w`"), kept(""),
		left("</details>"), left("`<img src=https://evil.example/p.png>` `@all`"), kept(""),
		left("<details><summary>3 changed rules fire as they did</summary>"), left("- `y`"),
	}
	md := serveMarkdown(changedSummary)
	md = md[:strings.Index(md, "\n")+1]
	want, n := md, 0
	for i, l := range lines {
		md += l[0] + "\n"
		if l[1] == leftOut {
			n++
			if i > 0 && lines[i-1][1] == leftOut {
				continue
			}
		}
		want += l[1] + "\n"
	}
	srv.answers(nil, []serveAnswer{serveDone(md)})
	out, err := rr.run([]string{"PATHS=alerts"})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := rr.body(); got != "[//]: # (lapilli-backtest default)\n"+want {
		gl := strings.Split(got, "\n")
		for i, w := range strings.Split("[//]: # (lapilli-backtest default)\n"+want, "\n") {
			if i >= len(gl) || gl[i] != w {
				t.Errorf("line %d: %q, want %q", i, gl[min(i, len(gl)-1)], w)
			}
		}
	}
	if !strings.Contains(out, fmt.Sprintf("lapilli serve's report has %d lines of Markdown lapilli doesn't write", n)) ||
		!strings.Contains(out, "  | /approve\n") || !strings.Contains(out, "  |   /merge\n") {
		t.Errorf("the job's log:\n%s", out)
	}
	for _, line := range strings.Split(rr.body(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "/") {
			t.Errorf("a quick action in the comment: %q", line)
		}
	}
}

// Serve's first line is the summary line, as lapilli writes it, a JSON
// object of numbers and true or false, or there is no report: the job's
// gates read it, and the comment doesn't show what isn't it.
func TestBacktestServeRecipeReportSummaryLine(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m"), "expect.yaml": "expectations: []\n"})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	srv := newServeStandIn(t)
	rr := serveRun(t, backtestHosts[2], repo, srv)
	rest := "\n### Lapilli backtest\n\n@all merge it\n"
	for _, first := range []string{
		"### Lapilli backtest: nothing changed",
		verdict.SummaryLine + `{"nothing_changed":false,"note":"x) @all"})`,
		verdict.SummaryLine + `{"nothing_changed":false,"changed":1}) @all`,
		verdict.SummaryLine + `[1,2])`,
		verdict.SummaryLine + `{"nothing_changed":false,})`,
		verdict.SummaryLine + `{"nothing_changed":false} )`,
		" " + verdict.SummaryLine + `{"nothing_changed":false})`,
		"",
	} {
		srv.answers(nil, []serveAnswer{serveDone(first + rest)})
		rr.host.set(func(h *commentHost) { h.comments = nil })
		out, err := rr.run([]string{"PATHS=alerts", "EXPECT=expect.yaml", "FAIL_ON_EXPECTATIONS=true"})
		if err == nil || !strings.Contains(out, "the expectations couldn't be judged") {
			t.Errorf("%q: %v, a gate that can't read the summary passes\n%s", first, err, out)
		}
		failure(t, rr.body(), "lapilli serve's report doesn't begin with the summary line lapilli writes: not shown")
		if strings.Contains(rr.body(), "@all") || strings.Contains(rr.body(), "lapilli-compare") {
			t.Errorf("%q: the comment:\n%s", first, rr.body())
		}
	}
}

// A report too long even with no log beside it, which serve's could be, is
// cut to its first lines within 60000 bytes, a <details> it leaves open
// closed, saying so; one past the 65536 characters lapilli never writes is
// read no further.
func TestBacktestServeRecipeLongReport(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	srv := newServeStandIn(t)
	rr := serveRun(t, backtestHosts[0], repo, srv)
	rows := func(summary string, n int) string {
		var b strings.Builder
		b.WriteString(serveMarkdown(summary) + "\n<details><summary>" + strconv.Itoa(n) + " changed rules fire as they did</summary>\n\n")
		for i := range n {
			fmt.Fprintf(&b, "- `Rule%04d%s`\n", i, strings.Repeat("x", 40))
		}
		b.WriteString("\n</details>\n")
		return b.String()
	}
	// Just past 65000 bytes, as lapilli never writes; then far past it.
	near, far := rows(changedSummary, 1224), rows(changedSummary, 3000)
	if len(near) < 65000 || len(near) > 65536 || len(far) < 100000 {
		t.Fatalf("%d and %d bytes", len(near), len(far))
	}
	for _, c := range []struct {
		name, md      string
		problem, long bool
	}{{"long", near, false, false}, {"past what lapilli writes", far, false, true},
		{"long, and a problem", rows(`{"nothing_changed":false,"changed":1,"unreplayed":1}`, 1222), true, false}} {
		t.Run(c.name, func(t *testing.T) {
			srv.answers(nil, []serveAnswer{serveDone(c.md)})
			rr.host.set(func(h *commentHost) { h.comments = nil })
			settings := []string{"PATHS=alerts"}
			if c.problem {
				settings = append(settings, "FAIL_ON_ERROR=true")
			}
			out, _ := rr.run(settings)
			body := rr.body()
			if len(body) > 60000 || !strings.Contains(body, "\n**Cut short** by the recipe, to keep the comment under 60000 bytes: the rest of the report isn't shown.\n") ||
				!strings.HasPrefix(body, "[//]: # (lapilli-backtest default)\n"+verdict.SummaryLine) {
				t.Errorf("%d bytes:\n%s\n%s", len(body), body[max(0, len(body)-400):], out)
			}
			if strings.Count(body, "<details>") != strings.Count(body, "</details>") || !c.long && !strings.Contains(body, "<details><summary>") {
				t.Errorf("a <details> left open: %d, %d", strings.Count(body, "<details>"), strings.Count(body, "</details>"))
			}
			if c.long != strings.Contains(out, "lapilli serve's report is longer than lapilli writes one (64000 bytes): read to its first 65536 characters") {
				t.Errorf("past 65536 characters %v:\n%s", c.long, out)
			}
			if c.problem {
				failure(t, body, "1 of the rules that differ couldn't be replayed (FAIL_ON_ERROR=true)")
			}
		})
	}
}

// An expectations file of no bytes is given, as --expect takes it: serve
// is sent "expect": "", and "expect_before" when the commit it goes into
// has one; a field is left out only when there is no file.
func TestBacktestServeRecipeEmptyExpect(t *testing.T) {
	needRecipeTools(t)
	srv := newServeStandIn(t)
	empty := func(r *prRepo) {
		if err := os.WriteFile(filepath.Join(r.dir, "expect.yaml"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		r.git("add", "expect.yaml")
		r.git("commit", "--quiet", "-m", "no expectations")
	}
	for _, c := range []struct {
		name         string
		base, head   bool // an empty file, or none
		want, before any
	}{
		{"both", true, true, "", ""},
		{"the pull request's", false, true, "", nil},
		{"the base's, taken away", true, false, "expectations: []\n", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
			if c.base {
				empty(repo)
			}
			repo.git("checkout", "--quiet", "-B", "pr")
			repo.write(map[string]string{"alerts/shop.yaml": shopRules("2h")})
			repo.git("add", "-A")
			repo.git("commit", "--quiet", "-m", "change the rules")
			if c.head && !c.base {
				empty(repo)
			}
			if !c.head && c.base {
				repo.git("rm", "--quiet", "expect.yaml")
				repo.git("commit", "--quiet", "-m", "no file")
			}
			repo.git("checkout", "--quiet", "--detach", "main")
			repo.git("merge", "--quiet", "--no-ff", "--no-edit", "pr")
			srv.answers(nil, []serveAnswer{serveDone(serveMarkdown(changedSummary))})
			rr := serveRun(t, backtestHosts[0], repo, srv)
			out, err := rr.run([]string{"PATHS=alerts", "EXPECT=expect.yaml"})
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			body := srv.sent()
			for field, want := range map[string]any{"expect": c.want, "expect_before": c.before} {
				got, ok := body[field]
				if want == nil && ok || want != nil && (!ok || got != want) {
					t.Errorf("%s %q (sent: %v), want %q", field, got, ok, want)
				}
			}
		})
	}
}

// A Retry-After of 0, or none, is waited 5 seconds at least, or
// SERVE_POLL if longer: serve asked to wait isn't asked in a loop.
func TestBacktestServeRecipeRetryAfterZero(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	srv := newServeStandIn(t)
	rr := serveRun(t, backtestHosts[0], repo, srv)
	busy := serveAnswer{429, map[string]string{"Retry-After": "0"}, `{"error":"try again"}`}
	// Taking it: 5 seconds is past SERVE_WAIT, so it isn't asked again.
	srv.answers([]serveAnswer{busy}, nil)
	out, err := rr.run([]string{"PATHS=alerts", "SERVE_WAIT=3s"})
	if posts, _, _ := srv.counts(); err != nil || posts != 1 {
		t.Errorf("%v: %d POSTs\n%s", err, posts, out)
	}
	failure(t, rr.body(), "lapilli serve had no room for the backtest within SERVE_WAIT (3s)")
	// Reading it: asked once, then waited to SERVE_WAIT, and cancelled.
	srv.answers([]serveAnswer{{202, nil, `{"id":"` + serveID + `","key":"` + serveKey + `","position":0,"requests":1}`}}, []serveAnswer{busy})
	rr.host.set(func(h *commentHost) { h.comments = nil })
	out, err = rr.run([]string{"PATHS=alerts", "SERVE_WAIT=2s"})
	if _, gets, deletes := srv.counts(); err != nil || gets > 2 || deletes != 1 {
		t.Errorf("%v: %d GETs, %d DELETEs\n%s", err, gets, deletes, out)
	}
	failure(t, rr.body(), "the backtest didn't come back within SERVE_WAIT (2s): cancelled in lapilli serve")
	// Lowered for tests as far as they go, SERVE_RETRY_MIN and SERVE_POLL
	// of 0, a Retry-After of 0 is still waited a second: a few POSTs
	// within SERVE_WAIT, not a loop of them.
	srv.answers([]serveAnswer{busy}, nil)
	rr.host.set(func(h *commentHost) { h.comments = nil })
	out, err = rr.run([]string{"PATHS=alerts", "SERVE_WAIT=2s", "SERVE_RETRY_MIN=0", "SERVE_POLL=0"})
	if posts, _, _ := srv.counts(); err != nil || posts > 3 {
		t.Errorf("SERVE_RETRY_MIN and SERVE_POLL of 0: %v: %d POSTs\n%s", err, posts, out)
	}
	failure(t, rr.body(), "lapilli serve had no room for the backtest within SERVE_WAIT (2s)")
}
