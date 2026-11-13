package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/verdict"
)

// The backtest recipes (recipes/backtest/), each against a stand-in for its
// host's API, a git repository with a pull request merged as a host checks
// one out, a stand-in Prometheus, and lapilli itself: this test binary,
// run as lapilli.

const backtestRecipes = "../../recipes/backtest/"

func TestMain(m *testing.M) {
	// Run by a recipe, as lapilli.
	if os.Getenv("LAPILLI_TEST_AS_CLI") == "1" {
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// needRecipeTools skips a test without what the recipes run with.
func needRecipeTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"sh", "git", "curl", "jq", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
}

// promStandIn is a Prometheus that has errors > 5 from three hours ago to
// two, and nothing else.
func promStandIn(t *testing.T) *httptest.Server {
	t.Helper()
	now := time.Now()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			http.NotFound(w, r)
			return
		}
		r.ParseForm()
		start, _ := strconv.ParseInt(r.Form.Get("start"), 10, 64)
		end, _ := strconv.ParseInt(r.Form.Get("end"), 10, 64)
		step, _ := strconv.ParseFloat(r.Form.Get("step"), 64)
		result := []any{}
		if r.Form.Get("query") == "errors > 5" && step > 0 {
			var values [][]any
			from, to := now.Add(-3*time.Hour).Unix(), now.Add(-2*time.Hour).Unix()
			for at := start; at <= end; at += int64(step) {
				if at >= from && at <= to {
					values = append(values, []any{at, "6"})
				}
			}
			if len(values) > 0 {
				result = append(result, map[string]any{"metric": map[string]string{"service": "checkout"}, "values": values})
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": result}})
	}))
	t.Cleanup(s.Close)
	return s
}

// shopRules is a rule file, CheckoutErrors waiting forDur.
func shopRules(forDur string) string {
	return `groups:
  - name: shop
    rules:
      - alert: CheckoutErrors
        expr: errors > 5
        for: ` + forDur + `
        labels: {severity: page}
      - alert: Quiet
        expr: never
`
}

// prRepo is a git repository holding base on main, and a pull request
// changing it to head, checked out as GitHub checks one out: a merge
// commit whose first parent is main. head maps a path to its text, "" to
// take it away.
type prRepo struct {
	t   *testing.T
	dir string
	env []string
	// home is the HOME the repository's git, and a recipe, run with: the
	// test's own, never the machine's.
	home string
}

// withoutCI is env without what a CI sets and the recipes read: run in a
// CI, a recipe under test would otherwise find the CI's own pull request
// (GITHUB_EVENT_PATH, CI_MERGE_REQUEST_IID, ...) and its settings.
func withoutCI(env []string) []string {
	var kept []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		ci := false
		for _, p := range []string{"GITHUB_", "GITEA_", "FORGEJO_", "GITLAB_", "CI_", "BITBUCKET_", "SYSTEM_", "BUILD_", "AZURE_DEVOPS_", "LAPILLI", "RENDER", "MAX_TREE_"} {
			if strings.HasPrefix(k, p) {
				ci = true
			}
		}
		switch k {
		case "CI", "TF_BUILD", "PR", "PATHS", "INCIDENTS", "KEY", "REPO", "TARGET", "TARGET_BRANCH", "FAIL_ON_ERROR":
			ci = true
		}
		if !ci {
			kept = append(kept, kv)
		}
	}
	return kept
}

func newPRRepo(t *testing.T, base map[string]string) *prRepo {
	t.Helper()
	home := t.TempDir()
	empty := filepath.Join(home, "gitconfig")
	os.WriteFile(empty, nil, 0o600)
	r := &prRepo{t: t, dir: t.TempDir(), home: home, env: append(withoutCI(os.Environ()), "HOME="+home, "GIT_CONFIG_GLOBAL="+empty, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")}
	r.git("init", "--quiet", "--initial-branch=main")
	r.write(base)
	r.git("add", "-A")
	r.git("commit", "--quiet", "-m", "rules")
	return r
}

func (r *prRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = r.dir, r.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *prRepo) write(files map[string]string) {
	r.t.Helper()
	for name, body := range files {
		p := filepath.Join(r.dir, name)
		if body == "" {
			os.RemoveAll(p)
			continue
		}
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
}

// pullRequest commits files on a branch from main, and checks out main
// merged with it, as a host checks a pull request out.
func (r *prRepo) pullRequest(files map[string]string) {
	r.t.Helper()
	r.git("checkout", "--quiet", "main")
	r.git("checkout", "--quiet", "-B", "pr")
	r.write(files)
	r.git("add", "-A")
	r.git("commit", "--quiet", "--allow-empty", "-m", "change the rules")
	r.git("checkout", "--quiet", "--detach", "main")
	r.git("merge", "--quiet", "--no-ff", "--no-edit", "pr")
}

// hostComment is a comment a stand-in host keeps: one deleted (Bitbucket's,
// Azure DevOps' thread), or GitLab's own (a system note), never the
// recipe's.
type hostComment struct {
	ID, Thread      int
	Author          string
	Body            string
	Deleted, System bool
}

// commentHost is a stand-in for one host's API of pull request comments.
type commentHost struct {
	t        *testing.T
	kind     string
	auth     string
	mu       sync.Mutex
	url      string
	comments []hostComment
	next     int
	posts    int
	edits    int
	// anonymous says the token can't say whose it is, as no repository
	// access token on Bitbucket can.
	anonymous bool
	// failPost refuses a new comment.
	failPost bool
	// closed says Azure DevOps' last thread was posted closed.
	closed bool
}

// The account the stand-ins' token is, as each host names it.
var hostMe = map[string]string{"github": "lapilli-bot", "gitea": "lapilli-bot", "gitlab": "42",
	"bitbucket": "{0b0b0b0b-0000-4000-8000-000000000042}", "azure": "6c1f3b52-0000-4000-8000-000000000042"}

// add keeps a comment as if posted, by author.
func (h *commentHost) add(author, body string) int {
	h.next++
	h.comments = append(h.comments, hostComment{ID: h.next, Thread: 1000 + h.next, Author: author, Body: body})
	return h.next
}

// mine are the comments the token's account posted.
func (h *commentHost) mine() []hostComment {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []hostComment
	for _, c := range h.comments {
		if c.Author == hostMe[h.kind] {
			out = append(out, c)
		}
	}
	return out
}

func (h *commentHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	header := "Authorization"
	if h.kind == "gitlab" {
		header = "PRIVATE-TOKEN"
	}
	if r.Header.Get(header) != h.auth {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	path := r.URL.EscapedPath()
	reply := func(v any) { json.NewEncoder(w).Encode(v) }
	var me, list, one string
	switch h.kind {
	case "github", "gitea":
		me, list, one = "/user", "/repos/o/r/issues/7/comments", "/repos/o/r/issues/comments/"
	case "gitlab":
		me, list, one = "/user", "/projects/o%2Fr/merge_requests/7/notes", "/projects/o%2Fr/merge_requests/7/notes/"
	case "bitbucket":
		me, list, one = "/user", "/repositories/ws/repo/pullrequests/7/comments", "/repositories/ws/repo/pullrequests/7/comments/"
	case "azure":
		me, list, one = "/org/_apis/connectionData", "/org/my%20project/_apis/git/repositories/rules/pullRequests/7/threads",
			"/org/my%20project/_apis/git/repositories/rules/pullRequests/7/threads/"
	}
	// A page of what is listed, as the host pages it, and whether there
	// is another. Gitea 28 takes no page for an issue's comments, and gives
	// them all; Azure DevOps all of a pull request's threads.
	page := func() ([]hostComment, bool) {
		size, n := len(h.comments), 1
		switch h.kind {
		case "github", "gitlab":
			size, _ = strconv.Atoi(r.URL.Query().Get("per_page"))
		case "bitbucket":
			size, _ = strconv.Atoi(r.URL.Query().Get("pagelen"))
		}
		if h.kind != "azure" && h.kind != "gitea" {
			n, _ = strconv.Atoi(r.URL.Query().Get("page"))
		}
		if h.kind == "gitea" && (r.URL.Query().Has("page") || r.URL.Query().Has("limit")) {
			h.t.Errorf("gitea: paged, which Gitea ignores: %s", r.URL)
		}
		if size < 1 || n < 1 {
			if len(h.comments) == 0 {
				return nil, false
			}
			h.t.Errorf("%s: listed without paging: %s", h.kind, r.URL)
			return nil, false
		}
		from, to := min((n-1)*size, len(h.comments)), min(n*size, len(h.comments))
		return h.comments[from:to], to < len(h.comments)
	}
	shape := func(c hostComment) any {
		switch h.kind {
		case "github", "gitea":
			return map[string]any{"id": c.ID, "body": c.Body, "user": map[string]any{"login": c.Author}}
		case "gitlab":
			id, _ := strconv.Atoi(c.Author)
			return map[string]any{"id": c.ID, "body": c.Body, "author": map[string]any{"id": id}, "system": c.System}
		case "bitbucket":
			return map[string]any{"id": c.ID, "content": map[string]any{"raw": c.Body}, "user": map[string]any{"uuid": c.Author}, "deleted": c.Deleted}
		}
		return map[string]any{"id": c.Thread, "isDeleted": c.Deleted,
			"comments": []any{map[string]any{"id": 1, "content": c.Body, "author": map[string]any{"id": c.Author}}}}
	}
	body := func() string {
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		switch h.kind {
		case "bitbucket":
			c, _ := in["content"].(map[string]any)
			s, _ := c["raw"].(string)
			return s
		case "azure":
			if s, ok := in["content"].(string); ok {
				return s
			}
			cs, _ := in["comments"].([]any)
			if len(cs) == 1 {
				c, _ := cs[0].(map[string]any)
				s, _ := c["content"].(string)
				return s
			}
			return ""
		}
		s, _ := in["body"].(string)
		return s
	}
	switch {
	case r.Method == http.MethodGet && path == me:
		if h.anonymous || h.kind == "bitbucket" {
			http.Error(w, "Resource not accessible by integration", http.StatusForbidden)
			return
		}
		switch h.kind {
		case "github", "gitea":
			reply(map[string]any{"login": hostMe[h.kind]})
		case "gitlab":
			reply(map[string]any{"id": 42})
		case "bitbucket":
			reply(map[string]any{"uuid": hostMe[h.kind]})
		case "azure":
			reply(map[string]any{"authenticatedUser": map[string]any{"id": hostMe[h.kind]}})
		}
	case r.Method == http.MethodGet && path == list:
		var shown []any
		these, more := page()
		for _, c := range these {
			shown = append(shown, shape(c))
		}
		if shown == nil {
			shown = []any{}
		}
		switch h.kind {
		case "bitbucket":
			v := map[string]any{"values": shown}
			if more {
				// Followed, it would lead elsewhere: the recipe asks by page.
				v["next"] = "https://elsewhere.example/next"
			}
			reply(v)
		case "azure":
			reply(map[string]any{"value": shown})
		default:
			reply(shown)
		}
	case r.Method == http.MethodPost && path == list && h.failPost:
		http.Error(w, "nope", http.StatusInternalServerError)
	case r.Method == http.MethodPost && path == list:
		h.posts++
		if h.kind == "azure" {
			var in map[string]any
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &in)
			h.closed = in["status"] == "closed"
			r.Body = io.NopCloser(strings.NewReader(string(b)))
		}
		id := h.add(hostMe[h.kind], body())
		reply(map[string]any{"id": id, "user": map[string]any{"uuid": hostMe[h.kind]}})
	case (r.Method == http.MethodPatch || r.Method == http.MethodPut) && strings.HasPrefix(path, one):
		rest := strings.TrimPrefix(path, one)
		for i, c := range h.comments {
			want := strconv.Itoa(c.ID)
			if h.kind == "azure" {
				want = strconv.Itoa(c.Thread) + "/comments/1"
			}
			if rest == want {
				if c.Author != hostMe[h.kind] {
					h.t.Errorf("%s: edited %s's comment", h.kind, c.Author)
				}
				h.edits++
				h.comments[i].Body = body()
				reply(map[string]any{"id": c.ID})
				return
			}
		}
		http.NotFound(w, r)
	default:
		h.t.Errorf("%s: %s %s", h.kind, r.Method, r.URL)
		http.NotFound(w, r)
	}
}

// backtestHost is a backtest recipe, and what it is run with.
type backtestHost struct {
	kind, script, auth string
	env                func(url, tok string) []string
}

var backtestHosts = []backtestHost{
	{"github", "github.sh", "Bearer host-token-0123456789",
		func(u, tok string) []string {
			return []string{"GITHUB_API=" + u, "GITHUB_REPO=o/r", "GITHUB_TOKEN_FILE=" + tok}
		}},
	{"gitea", "gitea.sh", "token host-token-0123456789",
		func(u, tok string) []string {
			return []string{"GITEA_API=" + u, "GITEA_REPO=o/r", "GITEA_TOKEN_FILE=" + tok}
		}},
	{"gitlab", "gitlab.sh", "host-token-0123456789",
		func(u, tok string) []string {
			return []string{"GITLAB_API=" + u, "GITLAB_PROJECT=o/r", "GITLAB_TOKEN_FILE=" + tok}
		}},
	{"bitbucket", "bitbucket.sh", "Bearer host-token-0123456789",
		func(u, tok string) []string {
			return []string{"BITBUCKET_API=" + u, "BITBUCKET_REPO=ws/repo", "BITBUCKET_TOKEN_FILE=" + tok, "BITBUCKET_USER_UUID=" + hostMe["bitbucket"]}
		}},
	{"azure", "azure-devops.sh", "Basic " + base64.StdEncoding.EncodeToString([]byte(":host-token-0123456789")),
		func(u, tok string) []string {
			return []string{"AZURE_DEVOPS_ORG_URL=" + u + "/org", "AZURE_DEVOPS_PROJECT=my project", "AZURE_DEVOPS_REPO=rules",
				"AZURE_DEVOPS_TOKEN_FILE=" + tok}
		}},
}

// recipeRun is a backtest recipe run against a stand-in host.
type recipeRun struct {
	t    *testing.T
	h    backtestHost
	host *commentHost
	repo *prRepo
	env  []string
}

func newRecipeRun(t *testing.T, h backtestHost, repo *prRepo) *recipeRun {
	t.Helper()
	host := &commentHost{t: t, kind: h.kind, auth: h.auth}
	hs := httptest.NewServer(host)
	t.Cleanup(hs.Close)
	host.url = hs.URL
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("host-token-0123456789\n"), 0o600)
	// A .curlrc that sends everything nowhere, in the test's own HOME: the
	// recipe's curl reads none.
	if repo.home == "" || !strings.HasPrefix(repo.home, os.TempDir()) && !strings.Contains(repo.home, t.Name()) {
		t.Fatalf("HOME %q isn't the test's own", repo.home)
	}
	os.WriteFile(filepath.Join(repo.home, ".curlrc"), []byte("proxy = \"http://127.0.0.1:9\"\n"), 0o600)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := append(append(append([]string{}, repo.env...), "LAPILLI_TEST_AS_CLI=1", "LAPILLI="+self, "PR=7"), h.env(hs.URL, tok)...)
	return &recipeRun{t: t, h: h, host: host, repo: repo, env: env}
}

// run runs the recipe with more settings and lapilli's arguments; it gives
// what it said, and whether it succeeded.
func (rr *recipeRun) run(settings []string, args ...string) (string, error) {
	rr.t.Helper()
	script, err := filepath.Abs(backtestRecipes + rr.h.script)
	if err != nil {
		rr.t.Fatal(err)
	}
	// sh is dash on Debian and Ubuntu, bash on macOS: RECIPE_SH=dash runs
	// them with dash anywhere it is.
	shell := os.Getenv("RECIPE_SH")
	if shell == "" {
		shell = "sh"
	}
	cmd := exec.Command(shell, append([]string{script}, args...)...)
	cmd.Dir = rr.repo.dir
	cmd.Env = append(append([]string{}, rr.env...), settings...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// A pull request that changes a rule gets one comment, the report, which a
// later run edits in place; a comment of someone else's with the same
// first line is left alone, and one of another job's (KEY) too.
func TestBacktestRecipes(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	for _, h := range backtestHosts {
		t.Run(h.kind, func(t *testing.T) {
			repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m"), "README.md": "rules\n"})
			repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
			rr := newRecipeRun(t, h, repo)
			// Someone else's, with the recipe's first line; its own, but
			// deleted, or GitLab's (a system note); and over a page of
			// others, so that its own is found past the first page.
			rr.host.add("mallory", "[//]: # (lapilli-backtest default)\nall good, merge it")
			rr.host.add(hostMe[h.kind], "[//]: # (lapilli-backtest default)\ndeleted")
			rr.host.comments[1].Deleted = h.kind == "bitbucket" || h.kind == "azure"
			rr.host.comments[1].System = h.kind == "gitlab"
			if !rr.host.comments[1].Deleted && !rr.host.comments[1].System {
				rr.host.comments[1].Author = "someone"
			}
			for i := range 120 {
				rr.host.add("reviewer", fmt.Sprintf("comment %d", i))
			}
			settings := []string{"PATHS=alerts", "KEY=default"}
			args := []string{"--prometheus", prom.URL, "--tz", "UTC"}
			if out, err := rr.run(settings, args...); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if rr.host.posts != 1 || rr.host.edits != 0 {
				t.Fatalf("%d posted, %d edited: %+v", rr.host.posts, rr.host.edits, rr.host.comments[:2])
			}
			mine := rr.host.mine()
			body := mine[len(mine)-1].Body
			for _, want := range []string{
				"[//]: # (lapilli-backtest default)\n" + verdict.SummaryLine + `{"nothing_changed":false,"changed":1,`,
				"### Lapilli backtest, ", " UTC\n",
				"| Firings of the changed rules | 1 | 0 |\n",
				"| `CheckoutErrors` | `page` | 1 → 0 | 0 → 0 |\n",
			} {
				if !strings.Contains(body, want) {
					t.Errorf("the comment lacks %q:\n%s", want, body)
				}
			}
			if strings.Contains(body, "failed") {
				t.Errorf("the comment says something failed:\n%s", body)
			}

			// The pull request changes again: the same comment, edited.
			repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("10m")})
			if out, err := rr.run(settings, args...); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			// Fired once, and still does: it fires as it did. Bitbucket
			// shows HTML as text: its summary in bold.
			folded := "<details><summary>1 changed rules fire as they did</summary>\n\n- `CheckoutErrors`\n\n</details>\n"
			if h.kind == "bitbucket" {
				folded = "**1 changed rules fire as they did**\n\n- `CheckoutErrors`\n\n\n"
			}
			mine = rr.host.mine()
			if rr.host.posts != 1 || rr.host.edits != 1 || !strings.Contains(mine[len(mine)-1].Body, folded) {
				t.Errorf("after a second run: %d posts, %d edits, %+v", rr.host.posts, rr.host.edits, mine)
			}
			if rr.host.comments[0].Body != "[//]: # (lapilli-backtest default)\nall good, merge it" {
				t.Errorf("someone else's comment changed: %q", rr.host.comments[0].Body)
			}

			// Another job's comment is its own.
			if out, err := rr.run([]string{"PATHS=alerts", "KEY=staging"}, args...); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if mine = rr.host.mine(); rr.host.posts != 2 || !strings.HasPrefix(mine[len(mine)-1].Body, "[//]: # (lapilli-backtest staging)\n") {
				t.Errorf("KEY=staging: %+v", mine)
			}
		})
	}
}

// Nothing that decides when a rule fires changed: no comment, unless there
// is one already, which then says so.
func TestBacktestRecipeNothingChanged(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	// A summary changed: nothing that decides when it fires.
	repo.pullRequest(map[string]string{"alerts/shop.yaml": strings.Replace(shopRules("5m"), "labels: {severity: page}",
		"labels: {severity: page}\n        annotations: {summary: Checkout fails}", 1), "README.md": "docs\n"})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	out, err := rr.run([]string{"PATHS=alerts"}, "--prometheus", prom.URL)
	if err != nil || len(rr.host.comments) != 0 || !strings.Contains(out, "none posted") {
		t.Fatalf("nothing changed: %v, %d comments\n%s", err, len(rr.host.comments), out)
	}
	// A comment from when it did change: it says nothing changes now.
	id := rr.host.add(hostMe["github"], "[//]: # (lapilli-backtest default)\nan old report")
	if out, err := rr.run([]string{"PATHS=alerts"}, "--prometheus", prom.URL); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if c := rr.host.comments[0]; c.ID != id || rr.host.edits != 1 ||
		!strings.Contains(c.Body, "\n"+verdict.NothingChangedHeading+"\n\nNothing that decides when the 2 rules fire has changed") {
		t.Errorf("the comment: %+v", c)
	}
}

// Rules a command renders, from a chart say: rendered in each commit, what
// isn't rules passed over, --rules-select given through.
func TestBacktestRecipeRender(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	chart := func(forDur string) string {
		return "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: shop}\n---\n" +
			"apiVersion: v1\nkind: ConfigMap\nmetadata: {name: rules, labels: {grafana_alert: \"1\"}}\ndata:\n  shop.yaml: |\n" +
			"    " + strings.ReplaceAll(strings.TrimSuffix(shopRules(forDur), "\n"), "\n", "\n    ") + "\n"
	}
	repo := newPRRepo(t, map[string]string{"chart/values.yaml": "for: 5m\n", "chart/render.sh": "#!/bin/sh\ncat chart/out.yaml\n", "chart/out.yaml": chart("5m")})
	repo.pullRequest(map[string]string{"chart/out.yaml": chart("2h")})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	out, err := rr.run([]string{"RENDER=sh chart/render.sh", "RENDER_PREPARE=test -f chart/values.yaml"}, "--prometheus", prom.URL, "--rules-select", "grafana_alert=1")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if mine := rr.host.mine(); len(mine) != 1 || !strings.Contains(mine[0].Body, "| `CheckoutErrors` | `page` | 1 → 0 |") {
		t.Errorf("comments: %+v\n%s", mine, out)
	}
	if !strings.Contains(out, "the rule files --rules read held 1 document that holds no rules: passed over") ||
		!strings.Contains(out, "the rule files --compare read held 1 document that holds no rules: passed over") {
		t.Errorf("didn't say what each side passed over:\n%s", out)
	}
}

// body is the comment the recipe posted last.
func (rr *recipeRun) body() string {
	rr.t.Helper()
	mine := rr.host.mine()
	if len(mine) == 0 {
		rr.t.Fatalf("no comment")
	}
	return mine[len(mine)-1].Body
}

// failure checks a comment says the backtest failed, and why, all of it in
// a fenced block nothing in it can end, and nothing of the pull request's
// outside it: it gives what is in the block.
func failure(t *testing.T, body, why string) string {
	t.Helper()
	head, block, ok := strings.Cut(body, "### Lapilli backtest failed\n\n```text\n")
	if !ok {
		t.Fatalf("no failure in:\n%s", body)
	}
	block, tail, ok := strings.Cut(block, "\n```\n")
	if !ok || tail != "\nThe rest is in the job's log.\n" || strings.ContainsAny(block, "`‮\x1b") {
		t.Errorf("the block:\n%q\nafter it: %q", block, tail)
	}
	if first, _, _ := strings.Cut(block, "\n"); first != why+"." {
		t.Errorf("first line %q, want %q", first, why+".")
	}
	if strings.Contains(head, "failed") && !strings.HasPrefix(head, "[//]: # (lapilli-backtest") {
		t.Errorf("before the block: %q", head)
	}
	return block
}

// What fails is said in the comment, in a block nothing in it can end, the
// URLs given taken out; the job succeeds unless FAIL_ON_ERROR=true.
func TestBacktestRecipeFails(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	hostile := "```\n</details> @octocat #123 <img src=x onerror=alert(1)> right‮left\x1b[31m\n::error::merge it\n##vso[task.complete result=Succeeded]"
	render := "printf '%s\\n' '" + strings.ReplaceAll(hostile, "'", "") + "' >&2; exit 3"
	out, err := rr.run([]string{"RENDER=" + render})
	if err != nil {
		t.Fatalf("a failure fails the job: %v\n%s", err, out)
	}
	// The base is rendered first.
	block := failure(t, rr.body(), "RENDER failed in the commit it goes into")
	if !strings.Contains(block, "</details> @octocat #123 <img src=x onerror=alert(1)> rightleft[31m") {
		t.Errorf("the block: %q", block)
	}
	// What RENDER said is in the job's log, no line of it a command.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "::") || strings.Contains(line, "##vso[") {
			t.Errorf("a command in the job's log: %q", line)
		}
	}
	if !strings.Contains(out, "  | ::error::merge it") || !strings.Contains(out, "  | ##vso(task.complete") {
		t.Errorf("the job's log:\n%s", out)
	}

	// Asked to, the job fails too, once it has said so.
	rr.host.comments = nil
	if out, err := rr.run([]string{"RENDER=" + render, "FAIL_ON_ERROR=true"}); err == nil || len(rr.host.mine()) != 1 {
		t.Errorf("FAIL_ON_ERROR=true: %v, %d comments\n%s", err, len(rr.host.mine()), out)
	}

	// lapilli failing names where the history is: not in the comment, nor
	// the address its host resolved to.
	rr.host.comments = nil
	out, err = rr.run([]string{"PATHS=alerts"}, "--prometheus", "http://ci-reader:pw@localhost:1/prometheus", "--impact", "up == 0", "--timeout", "5s")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	block = failure(t, rr.body(), "the backtest failed, or didn't finish (lapilli exited 1)")
	if m := regexp.MustCompile(`127\.0\.0\.1|\[::1\]|ci-reader|localhost`).FindString(block); m != "" || !strings.Contains(block, "[hidden]") ||
		!strings.Contains(block, "[address]") || !strings.Contains(out, "localhost:1") {
		t.Errorf("the block says %q: %q\n%s", m, block, out)
	}
	// A URL's user and password, and addresses, wherever they come from.
	rr.host.comments = nil
	userinfo := filepath.Join(t.TempDir(), "lapilli")
	os.WriteFile(userinfo, []byte("#!/bin/sh\necho 'lapilli verdict: Get \"https://other-user:s3cret@elsewhere.example/x\": dial tcp 10.1.2.3:9090: lookup on 10.0.0.2:53' >&2\nexit 1\n"), 0o755)
	rr.run([]string{"PATHS=alerts", "LAPILLI=" + userinfo}, "--prometheus", "http://127.0.0.1:1")
	if block := failure(t, rr.body(), "the backtest failed, or didn't finish (lapilli exited 1)"); strings.Contains(block, "other-user") ||
		strings.Contains(block, "10.") || !strings.Contains(block, `"https://elsewhere.example/x": dial tcp [address]: lookup on [address]`) {
		t.Errorf("the block: %q", block)
	}

	// A lapilli from before --markdown says so.
	rr.host.comments = nil
	old := filepath.Join(t.TempDir(), "lapilli")
	os.WriteFile(old, []byte("#!/bin/sh\necho 'lapilli verdict: --markdown needs a value' >&2\nexit 2\n"), 0o755)
	if out, err := rr.run([]string{"PATHS=alerts", "LAPILLI=" + old}, "--prometheus", "http://127.0.0.1:1"); err != nil {
		t.Errorf("an old lapilli: %v\n%s", err, out)
	}
	failure(t, rr.body(), "the lapilli LAPILLI names has no --markdown: give a later release")

	// A report from a run that failed is no reason to say nothing: one that
	// says nothing changed, then exits 1, is posted, failure and all.
	rr.host.comments = nil
	lying := filepath.Join(t.TempDir(), "lapilli")
	os.WriteFile(lying, []byte("#!/bin/sh\nprintf '%s\\n' '"+verdict.SummaryLine+`{"nothing_changed":true,"unreplayed":0}`+")' '"+verdict.NothingChangedHeading+"'\nexit 1\n"), 0o755)
	if out, err := rr.run([]string{"PATHS=alerts", "LAPILLI=" + lying}, "--prometheus", "http://127.0.0.1:1"); err != nil || len(rr.host.mine()) != 1 {
		t.Errorf("a report of a run that failed: %v %+v\n%s", err, rr.host.mine(), out)
	}

	// The comment can't be posted: the job fails only when asked to.
	rr.host.comments, rr.host.failPost = nil, true
	if out, err := rr.run([]string{"RENDER=" + render}); err != nil || !strings.Contains(out, "the comment couldn't be posted") {
		t.Errorf("not posted: %v\n%s", err, out)
	}
	if out, err := rr.run([]string{"RENDER=" + render, "FAIL_ON_ERROR=true"}); err == nil {
		t.Errorf("not posted, FAIL_ON_ERROR=true: the job succeeded\n%s", out)
	}
	rr.host.failPost = false

	// Asked to fail when it does, a backtest of rules that couldn't be
	// replayed, with the history out of reach, fails.
	rr.host.comments = nil
	out, err = rr.run([]string{"PATHS=alerts", "FAIL_ON_ERROR=true"}, "--prometheus", "http://127.0.0.1:1")
	if err == nil {
		t.Errorf("FAIL_ON_ERROR=true, nothing replayed, and the job succeeded:\n%s", out)
	}
	if body := rr.body(); !strings.Contains(body, "| Firings of the changed rules |") {
		t.Errorf("the report goes with the failure:\n%s", body)
	}
	failure(t, rr.body(), "1 of the rules that differ couldn't be replayed (FAIL_ON_ERROR=true)")
	if out, err := rr.run([]string{"PATHS=alerts"}, "--prometheus", "http://127.0.0.1:1"); err != nil || strings.Contains(rr.body(), "### Lapilli backtest failed") {
		t.Errorf("not asked to fail: %v\n%s\n%s", err, rr.body(), out)
	}

	// A link where the rules are read is never followed, and its name,
	// the pull request's, is only in the job's log.
	rr.host.comments = nil
	dir := filepath.Join(repo.dir, "alerts", "x @octocat @example-org", "team example-org", "repo#1 #2")
	os.MkdirAll(dir, 0o755)
	if err := os.Symlink("/etc/hosts", filepath.Join(dir, `www.evil.example <img src="https:\evil.example\q.png"> **ALL CLEAR**`)); err != nil {
		t.Fatal(err)
	}
	repo.git("add", "-A")
	repo.git("commit", "--quiet", "--amend", "--no-edit")
	out, err = rr.run([]string{"PATHS=alerts"}, "--prometheus", "http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	body := rr.body()
	failure(t, body, "the pull request's commit holds a symbolic link, which could lead to any file the job can read: not read (the job's log says where)")
	for _, bad := range []string{"octocat", "example-org", "#2", "www.", "<img", "ALL CLEAR"} {
		if strings.Contains(body, bad) {
			t.Errorf("the link's name in the comment: %s\n%s", bad, body)
		}
	}
	if !strings.Contains(out, "a symbolic link in the pull request's commit: alerts/x @octocat") {
		t.Errorf("the job's log doesn't say where:\n%s", out)
	}
	os.RemoveAll(filepath.Join(repo.dir, "alerts", "x @octocat @example-org"))
	repo.git("add", "-A")
	repo.git("commit", "--quiet", "--amend", "--no-edit")

	// RENDER_PREPARE leaving a link, and a commit too big to read.
	rr.host.comments = nil
	rr.run([]string{"RENDER=cat alerts/shop.yaml", "RENDER_PREPARE=ln -s /etc/hosts alerts/hosts.yaml"})
	failure(t, rr.body(), "RENDER_PREPARE left a symbolic link in the commit it goes into: not read (the job's log says where)")
	rr.host.comments = nil
	rr.run([]string{"PATHS=alerts", "MAX_TREE_FILES=0"}, "--prometheus", "http://127.0.0.1:1")
	failure(t, rr.body(), fmt.Sprintf("the commit it goes into is too big to read: 1 files of %d bytes, more than MAX_TREE_FILES (0) or MAX_TREE_BYTES (268435456)",
		len(shopRules("5m"))))
}

// Settings that are wrong fail the job at once, before anything runs.
func TestBacktestRecipeSettings(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m"), "incidents.yaml": "incidents: []\n"})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	pin := "LAPILLI_SHA256=" + strings.Repeat("a", 64)
	inClone := filepath.Join(repo.dir, "incidents.yaml")
	for _, c := range []struct {
		settings []string
		args     []string
		want     string
	}{
		{[]string{"PR="}, nil, "want the pull request's number"},
		{[]string{"PR=7;"}, nil, "want the pull request's number"},
		{nil, nil, "say where the rules are"},
		{[]string{"PATHS=alerts", "RENDER=cat x"}, nil, "leave out PATHS"},
		{[]string{"PATHS=../etc"}, nil, "isn't a path in the repository"},
		{[]string{"PATHS=-x"}, nil, "isn't a path in the repository"},
		{[]string{"PATHS=alerts", "RENDER_PREPARE=true"}, nil, "give RENDER too"},
		{[]string{"PATHS=alerts", "KEY=a b"}, nil, "KEY a b"},
		{[]string{"PATHS=alerts", "TARGET=--upload-pack=x"}, nil, "TARGET --upload-pack=x"},
		{[]string{"PATHS=alerts", "FAIL_ON_ERROR=yes"}, nil, "want true or false"},
		{[]string{"PATHS=alerts"}, []string{"--json"}, "the recipe gives lapilli the rules"},
		{[]string{"PATHS=alerts"}, []string{"--compare", "x"}, "the recipe gives lapilli the rules"},
		{[]string{"PATHS=alerts", "LAPILLI=", "LAPILLI_VERSION=latest"}, nil, "want the Lapilli release to install"},
		{[]string{"PATHS=alerts", "LAPILLI=", "LAPILLI_VERSION=v1.2.3", "LAPILLI_SHA256=abc"}, nil, "64 hex digits"},
		{[]string{"PATHS=alerts", "GITHUB_REPO=o/r/x"}, nil, "want OWNER/REPO"},
		// Over plain http, or anywhere but here with a user in it, an
		// archive and its SHA256SUMS could both be anyone's.
		{[]string{"PATHS=alerts", "LAPILLI=", "LAPILLI_VERSION=v1.2.3", pin, "LAPILLI_RELEASES=http://mirror.example/releases"}, nil, "want an https URL"},
		{[]string{"PATHS=alerts", "LAPILLI=", "LAPILLI_VERSION=v1.2.3", pin, "LAPILLI_RELEASES=http://localhost:@mirror.invalid/releases"}, nil, "want an https URL"},
		{[]string{"PATHS=alerts", "LAPILLI=", "LAPILLI_VERSION=v1.2.3", pin, "LAPILLI_RELEASES=http://127.0.0.1:x@mirror.invalid/releases"}, nil, "want an https URL"},
		{[]string{"PATHS=alerts", "LAPILLI=", "LAPILLI_VERSION=v1.2.3", pin, "LAPILLI_RELEASES=http://localhost.mirror.invalid/releases"}, nil, "want an https URL"},
		{[]string{"PATHS=alerts", "LAPILLI=", "LAPILLI_VERSION=v1.2.3", "LAPILLI_RELEASES=https://mirror.example/releases"}, nil, "give it"},
		// A token goes to the host in the clear only on this machine.
		{[]string{"PATHS=alerts", "GITHUB_API=http://github.example/api/v3"}, nil, "want an https URL"},
		// A file of the checkout's is the pull request's.
		{[]string{"PATHS=alerts"}, []string{"--incidents", "incidents.yaml"}, "a file in the checkout is the pull request's"},
		{[]string{"PATHS=alerts"}, []string{"--incidents=" + inClone}, "a file in the checkout is the pull request's"},
		{[]string{"PATHS=alerts"}, []string{"--bearer-token-file", "./alerts/../incidents.yaml"}, "a file in the checkout is the pull request's"},
		{[]string{"PATHS=alerts", "INCIDENTS=/etc/passwd"}, nil, "want a file in the repository"},
		// PATHS that neither commit has would say nothing changed.
		{[]string{"PATHS=alert"}, []string{"--prometheus", "http://127.0.0.1:1"}, "none of PATHS is in either commit"},
		{[]string{"PATHS=alerts", "BITBUCKET_USER_UUID=x"}, nil, ""},
	} {
		if len(c.settings) > 0 && c.settings[len(c.settings)-1] == "BITBUCKET_USER_UUID=x" {
			continue
		}
		out, err := rr.run(c.settings, c.args...)
		if err == nil || !strings.Contains(out, c.want) {
			t.Errorf("%v %v: %v, want it to fail saying %q\n%s", c.settings, c.args, err, c.want, out)
		}
	}
	if len(rr.host.comments) != 0 {
		t.Errorf("commented: %+v", rr.host.comments)
	}
	bb := newRecipeRun(t, backtestHosts[3], repo)
	for _, uuid := range []string{"x", "{}", "{not-hex!}"} {
		if out, err := bb.run([]string{"PATHS=alerts", "BITBUCKET_USER_UUID=" + uuid}); err == nil || !strings.Contains(out, "want {UUID}") {
			t.Errorf("BITBUCKET_USER_UUID=%s: %v\n%s", uuid, err, out)
		}
	}
}

// A release is installed only as its SHA256SUMS, and LAPILLI_SHA256 when
// given, say: an archive that isn't is never unpacked or run, nor one
// redirected to plain http.
func TestBacktestRecipeInstall(t *testing.T) {
	needRecipeTools(t)
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("releases are for Linux and macOS")
	}
	prom := promStandIn(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ran := filepath.Join(t.TempDir(), "ran")
	// The release's lapilli: this test binary, as lapilli, saying it ran.
	name := "lapilli-v9.9.9-" + runtime.GOOS + "-" + runtime.GOARCH
	script := fmt.Sprintf("#!/bin/sh\ntouch %q\nLAPILLI_TEST_AS_CLI=1 exec %q \"$@\"\n", ran, self)
	var archive strings.Builder
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: name + "/lapilli", Mode: 0o755, Size: int64(len(script)), Typeflag: tar.TypeReg})
	io.WriteString(tw, script)
	tw.Close()
	gz.Close()
	sum := sha256.Sum256([]byte(archive.String()))
	good := hex.EncodeToString(sum[:])
	goodSums := good + "  " + name + ".tar.gz\n"
	var mu sync.Mutex
	sums := goodSums
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, archive.String())
	}))
	defer plain.Close()
	releases := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		s := sums
		mu.Unlock()
		switch r.URL.Path {
		case "/v9.9.9/" + name + ".tar.gz":
			io.WriteString(w, archive.String())
		case "/v9.9.9/SHA256SUMS":
			io.WriteString(w, s)
		case "/v9.9.7/SHA256SUMS":
			io.WriteString(w, strings.ReplaceAll(s, "v9.9.9", "v9.9.7"))
		case "/v9.9.7/" + strings.Replace(name, "v9.9.9", "v9.9.7", 1) + ".tar.gz":
			// Elsewhere, over plain http.
			http.Redirect(w, r, plain.URL+"/archive.tar.gz", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer releases.Close()
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	install := []string{"PATHS=alerts", "LAPILLI=", "LAPILLI_VERSION=v9.9.9", "LAPILLI_RELEASES=" + releases.URL, "LAPILLI_SHA256=" + good}
	out, err := rr.run(install, "--prometheus", prom.URL)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(ran); err != nil || len(rr.host.mine()) != 1 || !strings.Contains(rr.host.mine()[0].Body, "`CheckoutErrors`") ||
		!strings.Contains(out, "installed Lapilli v9.9.9 ("+name+".tar.gz, sha256 "+good+")") {
		t.Fatalf("installed: %v %+v\n%s", err, rr.host.mine(), out)
	}

	zeros := strings.Repeat("0", 64)
	for _, c := range []struct {
		name, sums string
		settings   []string
		want       string
	}{
		{"another file", zeros + "  " + name + ".tar.gz\n", []string{"LAPILLI_SHA256=" + zeros}, name + ".tar.gz isn't the file SHA256SUMS names"},
		{"no sum", zeros + "  lapilli-v9.9.9-plan9-mips.tar.gz\n", nil, "Lapilli v9.9.9's SHA256SUMS has no sum for " + name + ".tar.gz"},
		{"pinned otherwise", goodSums, []string{"LAPILLI_SHA256=" + strings.Repeat("a", 64)}, "LAPILLI_SHA256 isn't the sum"},
		{"no such release", goodSums, []string{"LAPILLI_VERSION=v9.9.8"}, "Lapilli v9.9.8's " + strings.Replace(name, "v9.9.9", "v9.9.8", 1) + ".tar.gz couldn't be downloaded"},
		{"redirected to plain http", goodSums, []string{"LAPILLI_VERSION=v9.9.7"}, "Lapilli v9.9.7's " + strings.Replace(name, "v9.9.9", "v9.9.7", 1) + ".tar.gz couldn't be downloaded"},
	} {
		os.Remove(ran)
		rr.host.comments = nil
		mu.Lock()
		sums = c.sums
		mu.Unlock()
		out, err := rr.run(append(append([]string{}, install...), c.settings...), "--prometheus", prom.URL)
		if err != nil {
			t.Errorf("%s: %v\n%s", c.name, err, out)
		}
		if _, err := os.Stat(ran); err == nil {
			t.Errorf("%s: ran what it downloaded", c.name)
		}
		if mine := rr.host.mine(); len(mine) != 1 || !strings.Contains(mine[0].Body, c.want) {
			t.Errorf("%s: %+v\n%s", c.name, mine, out)
		}
	}
}

// action.yml runs github.sh with its inputs, as GitHub Actions runs a
// composite action: each input reaches the script as a variable, never in
// its text; args are split at spaces and never expanded; the token is in a
// file only while the step runs.
func TestBacktestAction(t *testing.T) {
	needRecipeTools(t)
	b, err := os.ReadFile(backtestRecipes + "action.yml")
	if err != nil {
		t.Fatal(err)
	}
	var action struct {
		Inputs map[string]struct {
			Required bool   `yaml:"required"`
			Default  string `yaml:"default"`
		} `yaml:"inputs"`
		Runs struct {
			Using string `yaml:"using"`
			Steps []struct {
				Shell string            `yaml:"shell"`
				Env   map[string]string `yaml:"env"`
				Run   string            `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"runs"`
	}
	if err := yaml.Unmarshal(b, &action); err != nil {
		t.Fatal(err)
	}
	if action.Runs.Using != "composite" || len(action.Runs.Steps) != 1 || action.Runs.Steps[0].Shell != "sh" {
		t.Fatalf("runs: %+v", action.Runs)
	}
	step := action.Runs.Steps[0]
	if strings.Contains(step.Run, "${{") {
		t.Errorf("an expression in the script's text, where an input could write shell: %s", step.Run)
	}
	prom := promStandIn(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	temp := t.TempDir()
	pwned := filepath.Join(temp, "pwned")
	run := func(inputs map[string]string) string {
		t.Helper()
		env := []string{"GITHUB_ACTION_PATH=" + must(filepath.Abs(backtestRecipes)), "RUNNER_TEMP=" + temp, "GITHUB_ACTIONS=true",
			"GITHUB_API_URL=" + rr.host.url, "GITHUB_REPOSITORY=o/r", "LAPILLI_TEST_AS_CLI=1", "LAPILLI=" + must(os.Executable())}
		for name, val := range step.Env {
			switch {
			case val == "${{ github.event.pull_request.number }}":
				val = "7"
			case strings.HasPrefix(val, "${{ inputs.") && strings.HasSuffix(val, " }}"):
				in := strings.TrimSuffix(strings.TrimPrefix(val, "${{ inputs."), " }}")
				def, ok := action.Inputs[in]
				if !ok {
					t.Fatalf("%s: no input %s", name, in)
				}
				val = def.Default
				if v, ok := inputs[in]; ok {
					val = v
				} else if def.Required {
					t.Fatalf("input %s is required", in)
				}
				if val == "${{ github.token }}" {
					val = "host-token-0123456789"
				}
			default:
				t.Fatalf("%s: %s", name, val)
			}
			env = append(env, name+"="+val)
		}
		cmd := exec.Command("sh", "-c", step.Run)
		cmd.Dir = repo.dir
		cmd.Env = append(append([]string{}, repo.env...), env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if left, _ := os.ReadDir(temp); len(left) != 0 {
			t.Errorf("left in RUNNER_TEMP: %v", left)
		}
		return string(out)
	}
	out := run(map[string]string{"version": "v0.32.0", "paths": "alerts", "prometheus-url": prom.URL, "since": "2d", "args": "--tz  UTC"})
	mine := rr.host.mine()
	if len(mine) != 1 || !strings.Contains(mine[0].Body, "| `CheckoutErrors` | `page` | 1 → 0 |") || !strings.Contains(mine[0].Body, " UTC\n") {
		t.Fatalf("comments: %+v\n%s", mine, out)
	}
	// What args holds is lapilli's arguments, never the shell's.
	out = run(map[string]string{"version": "v0.32.0", "paths": "alerts", "prometheus-url": prom.URL, "args": "--tz $(touch${IFS}" + pwned + ") *"})
	if _, err := os.Stat(pwned); err == nil {
		t.Error("args ran a command")
	}
	if mine = rr.host.mine(); len(mine) != 1 || rr.host.edits != 1 || !strings.Contains(mine[0].Body, "lapilli refused its arguments") ||
		!strings.Contains(mine[0].Body, "--tz $(touch${IFS}") {
		t.Errorf("comments: %+v\n%s", mine, out)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// The summary is a contract with the tools that read it: the README names
// each of its fields, and the form of a comparison with nothing changed.
func TestSummaryDocumented(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := string(b)
	_, section, _ := strings.Cut(readme, "## Pull request backtests in CI")
	section, _, _ = strings.Cut(section, "\n## ")
	ty := reflect.TypeOf(verdict.Summary{})
	for i := range ty.NumField() {
		tag := strings.Split(ty.Field(i).Tag.Get("json"), ",")
		if len(tag) != 1 || tag[0] == "" {
			t.Errorf("%s: %q, want a name and nothing else (no omitempty: every field is always there)", ty.Field(i).Name, tag)
			continue
		}
		if !strings.Contains(section, "`"+tag[0]+"`") {
			t.Errorf("the README's \"Pull request backtests in CI\" doesn't name %s", tag[0])
		}
	}
	if !strings.Contains(section, "  "+verdict.NothingChangedHeading+"\n\n  Nothing that decides when the 42 rules fire has changed: nothing to replay.\n") {
		t.Error("the README doesn't show the form of nothing changed")
	}
	if !strings.Contains(section, verdict.SummaryLine) {
		t.Error("the README doesn't show the summary line")
	}
}

// A pull request that changes only what an alert reads (a recording rule),
// or caps a group (limit), changes when alerts fire: it gets its comment.
func TestBacktestRecipeRecordingAndLimit(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	base := `groups:
  - name: shop
    rules:
      - record: shop:errors
        expr: errors
      - alert: CheckoutErrors
        expr: shop:errors > 5
        for: 5m
        labels: {severity: page}
`
	for name, head := range map[string]string{
		"recording rule zeroed": strings.Replace(base, "expr: errors\n", "expr: errors * 0\n", 1),
		"group limit":           strings.Replace(base, "  - name: shop\n", "  - name: shop\n    limit: 1\n", 1),
	} {
		repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": base})
		repo.pullRequest(map[string]string{"alerts/shop.yaml": head})
		rr := newRecipeRun(t, backtestHosts[0], repo)
		out, err := rr.run([]string{"PATHS=alerts"}, "--prometheus", prom.URL)
		if err != nil || len(rr.host.mine()) != 1 || !strings.Contains(rr.body(), "#### Recording rules that differ") {
			t.Errorf("%s: %v %+v\n%s", name, err, rr.host.mine(), out)
		}
	}
}

// Where the history is stays out of the comment: the user in its URL, the
// address its host resolved to, its port.
func TestBacktestRecipeHidesWhere(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	out, err := rr.run([]string{"PATHS=alerts"}, "--prometheus", "http://ci-reader:pw@localhost:1/prometheus", "--timeout", "5s")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	body := rr.body()
	if m := regexp.MustCompile(`dial tcp|127\.0\.0\.1|\[::1\]|ci-reader|localhost`).FindAllString(body, -1); len(m) > 0 || !strings.Contains(body, UnreadHistory) {
		t.Errorf("the comment says where the history is: %q\n%s", m, body)
	}
	if !strings.Contains(out, "couldn't be replayed: query failed: Post") {
		t.Errorf("the job's log doesn't say why:\n%s", out)
	}
}

// The incidents a pull request is judged by are those of the commit it
// goes into: one that takes out the incident its change misses is judged
// by it still.
func TestBacktestRecipeIncidents(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	now := time.Now()
	incidents := fmt.Sprintf("incidents:\n  - id: INC-7\n    title: Checkout down\n    start: %s\n    end: %s\n",
		now.Add(-3*time.Hour).Format(time.RFC3339), now.Add(-2*time.Hour).Format(time.RFC3339))
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m"), "incidents.yaml": incidents})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h"), "incidents.yaml": "incidents: []\n"})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	out, err := rr.run([]string{"PATHS=alerts", "INCIDENTS=incidents.yaml"}, "--prometheus", prom.URL, "--tz", "UTC")
	if err != nil || !strings.Contains(rr.body(), "| `INC-7` `Checkout down` |") {
		t.Errorf("%v\n%s\n%s", err, rr.body(), out)
	}
	// Not a file there, or a link: said.
	rr.host.comments = nil
	repo.git("checkout", "--quiet", "main")
	os.Remove(filepath.Join(repo.dir, "incidents.yaml"))
	os.Symlink("/dev/zero", filepath.Join(repo.dir, "incidents.yaml"))
	repo.git("add", "-A")
	repo.git("commit", "--quiet", "-m", "a link")
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	rr.run([]string{"PATHS=alerts", "INCIDENTS=incidents.yaml"}, "--prometheus", prom.URL)
	failure(t, rr.body(), "INCIDENTS incidents.yaml isn't a file in the commit the pull request goes into")
}

// RENDER runs in each commit's files, the base's first, each with a HOME
// of its own, and without the variables CIs keep tokens in.
func TestBacktestRecipeRenderAlone(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	repo := newPRRepo(t, map[string]string{"chart/out.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"chart/out.yaml": shopRules("2h")})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	homes := filepath.Join(t.TempDir(), "homes")
	render := `printf '%s %s\n' "$(basename "$HOME")" "${CI_JOB_TOKEN:-}${GITHUB_TOKEN_FILE:-}${SYSTEM_ACCESSTOKEN:-}${ACTIONS_ID_TOKEN_REQUEST_TOKEN:-}" >>` +
		homes + `; [ "$HOME" != "$JOB_HOME" ] && cat chart/out.yaml`
	out, err := rr.run([]string{"RENDER=" + render, "JOB_HOME=" + rr.home(), "CI_JOB_TOKEN=job", "SYSTEM_ACCESSTOKEN=azure", "ACTIONS_ID_TOKEN_REQUEST_TOKEN=oidc"},
		"--prometheus", prom.URL)
	if err != nil || strings.Contains(rr.body(), "failed") {
		t.Fatalf("%v\n%s\n%s", err, rr.body(), out)
	}
	if b, _ := os.ReadFile(homes); string(b) != "base-home \nhead-home \n" {
		t.Errorf("RENDER ran with: %q", b)
	}
	// A chart the pull request adds renders nothing on the base, as RENDER
	// says it should: no rules there.
	added := newPRRepo(t, map[string]string{"README.md": "x\n"})
	added.pullRequest(map[string]string{"chart/out.yaml": shopRules("5m")})
	rr = newRecipeRun(t, backtestHosts[0], added)
	out, err = rr.run([]string{"RENDER=[ ! -f chart/out.yaml ] || cat chart/out.yaml"}, "--prometheus", prom.URL)
	if err != nil || strings.Contains(rr.body(), "### Lapilli backtest failed") || !strings.Contains(rr.body(), "**0 changed, 2 added, 0 taken away**") {
		t.Errorf("a chart added: %v\n%s\n%s", err, rr.body(), out)
	}
}

// home is the HOME the recipe runs with: the test's own.
func (rr *recipeRun) home() string { return rr.repo.home }

// Rules a pull request asks git to leave out of an archive are read all
// the same: the commit's own .gitattributes don't count.
func TestBacktestRecipeGitattributes(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h"), ".gitattributes": "alerts/** export-ignore\n"})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	out, err := rr.run([]string{"PATHS=alerts"}, "--prometheus", prom.URL)
	if err != nil || !strings.Contains(rr.body(), "| `CheckoutErrors` | `page` | 1 → 0") {
		t.Errorf("%v\n%s\n%s", err, rr.body(), out)
	}
}

// Each host's own ways: a GitHub login in any letter case; Bitbucket's
// repository access token, which can't say who it is; GitLab's project
// from the merge request's; Azure DevOps' target branch fetched, and its
// thread closed.
func TestBacktestRecipeHosts(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	args := []string{"--prometheus", prom.URL}

	gh := newRecipeRun(t, backtestHosts[0], repo)
	gh.host.add("lapilli-bot", "[//]: # (lapilli-backtest default)\nold")
	if out, err := gh.run([]string{"PATHS=alerts", "GITHUB_LOGIN=Lapilli-Bot"}, args...); err != nil || gh.host.edits != 1 || gh.host.posts != 0 {
		t.Errorf("github, a login's case: %v, %d edits, %d posts\n%s", err, gh.host.edits, gh.host.posts, out)
	}

	bb := newRecipeRun(t, backtestHosts[3], repo)
	bb.env = slices.DeleteFunc(bb.env, func(e string) bool { return strings.HasPrefix(e, "BITBUCKET_USER_UUID=") })
	out, err := bb.run([]string{"PATHS=alerts"}, args...)
	if err != nil || bb.host.posts != 1 || !strings.Contains(out, "posted as "+hostMe["bitbucket"]+": set BITBUCKET_USER_UUID to it") {
		t.Errorf("bitbucket, no account: %v, %d posts\n%s", err, bb.host.posts, out)
	}

	gl := newRecipeRun(t, backtestHosts[2], repo)
	gl.env = slices.DeleteFunc(gl.env, func(e string) bool { return strings.HasPrefix(e, "GITLAB_PROJECT=") })
	if out, err := gl.run([]string{"PATHS=alerts", "CI_MERGE_REQUEST_PROJECT_ID=o/r", "CI_PROJECT_ID=fork/r"}, args...); err != nil || gl.host.posts != 1 {
		t.Errorf("gitlab, a fork's pipeline: %v, %d posts\n%s", err, gl.host.posts, out)
	}

	// Azure Pipelines: the pull request's own commit checked out, its
	// second, the branch it goes into fetched from origin: not HEAD^1,
	// which is the pull request's first.
	repo.git("remote", "add", "origin", repo.dir)
	repo.git("checkout", "--quiet", "pr")
	repo.write(map[string]string{"README.md": "more\n"})
	repo.git("add", "-A")
	repo.git("commit", "--quiet", "-m", "more")
	repo.git("checkout", "--quiet", "--detach", "pr")
	az := newRecipeRun(t, backtestHosts[4], repo)
	out, err = az.run([]string{"PATHS=alerts", "SYSTEM_PULLREQUEST_TARGETBRANCH=refs/heads/main"}, args...)
	if err != nil || az.host.posts != 1 || !strings.Contains(az.body(), "| `CheckoutErrors` | `page` | 1 → 0") || !az.host.closed {
		t.Errorf("azure: %v, %d posts, closed %v\n%s\n%s", err, az.host.posts, az.host.closed, az.body(), out)
	}
}

// A comment holds a report as long as Lapilli writes one and what went
// wrong, under the 65,536 characters GitHub takes.
func TestBacktestRecipeLongComment(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	long := filepath.Join(t.TempDir(), "lapilli")
	os.WriteFile(long, []byte("#!/bin/sh\nprintf '%s\\n' '"+verdict.SummaryLine+`{"nothing_changed":false,"unreplayed":0}`+")'\n"+
		"head -c 63800 /dev/zero | tr '\\000' x\nprintf '\\n'\n"+
		"i=0; while [ $i -lt 200 ]; do echo 'lapilli verdict: localhost localhost localhost localhost localhost localhost'; i=$((i+1)); done >&2\nexit 1\n"), 0o755)
	if out, err := rr.run([]string{"PATHS=alerts", "LAPILLI=" + long}, "--prometheus", "http://localhost:1"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if body := rr.body(); len(body) > 65000 || !strings.Contains(body, "### Lapilli backtest failed") {
		t.Errorf("%d bytes", len(body))
	}
}
