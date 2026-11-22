package main

import (
	"archive/tar"
	"bufio"
	"cmp"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
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
	"sync/atomic"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/scenario/scenariotest"
	"lapilli.dev/internal/verdict"
)

// The backtest recipes (recipes/backtest/), each against a stand-in for its
// host's API, a git repository with a pull request merged as a host checks
// one out, a stand-in Prometheus, and lapilli itself: this test binary,
// run as lapilli.

const backtestRecipes = "../../recipes/backtest/"

func TestMain(m *testing.M) {
	// Run as promtool, through scenariotest.Install's link: a stand-in.
	scenariotest.Fake()
	// Run by a recipe, as lapilli: its arguments, as it was given them,
	// written down when asked.
	if f := os.Getenv("LAPILLI_TEST_ARGS"); f != "" && os.Getenv("LAPILLI_TEST_AS_CLI") == "1" {
		b, _ := json.Marshal(os.Args[1:])
		os.WriteFile(f, b, 0o600)
	}
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
	return promAnswering(t, map[string]string{"errors > 5": "checkout"})
}

// promAnswering is a Prometheus that has each query in answers from three
// hours ago to two, one series whose service is the one it maps to, and
// nothing else: a query not written exactly so has nothing.
func promAnswering(t *testing.T, answers map[string]string) *httptest.Server {
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
		if service, ok := answers[r.Form.Get("query")]; ok && step > 0 {
			var values [][]any
			from, to := now.Add(-3*time.Hour).Unix(), now.Add(-2*time.Hour).Unix()
			for at := start; at <= end; at += int64(step) {
				if at >= from && at <= to {
					values = append(values, []any{at, "6"})
				}
			}
			if len(values) > 0 {
				result = append(result, map[string]any{"metric": map[string]string{"service": service}, "values": values})
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
		for _, p := range []string{"GITHUB_", "GITEA_", "FORGEJO_", "GITLAB_", "CI_", "BITBUCKET_", "SYSTEM_", "BUILD_", "AZURE_DEVOPS_", "LAPILLI", "RENDER", "MAX_TREE_",
			"ACTIONS_", "SERVE_"} {
			if strings.HasPrefix(k, p) {
				ci = true
			}
		}
		switch k {
		case "CI", "TF_BUILD", "PR", "PATHS", "INCIDENTS", "KEY", "REPO", "TARGET", "TARGET_BRANCH", "FAIL_ON_ERROR", "IMPACT", "IMPACT_FOR", "MATCH",
			"FAIL_MISSED", "STATUS", "STATUS_SHA", "STATUS_URL":
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

// pullRequestTree is pullRequest written without the work tree, which a
// file system that folds case can't hold two names of: files beside
// main's, in a commit of their own, and HEAD a merge of it into main.
func (r *prRepo) pullRequestTree(files map[string]string) {
	r.t.Helper()
	idx, dir := filepath.Join(r.t.TempDir(), "index"), r.t.TempDir()
	env := r.env
	r.env = append(slices.Clone(env), "GIT_INDEX_FILE="+idx)
	defer func() { r.env = env }()
	r.git("read-tree", "main")
	for name, body := range files {
		f := filepath.Join(dir, "blob")
		os.WriteFile(f, []byte(body), 0o644)
		r.git("update-index", "--add", "--cacheinfo", "100644,"+r.git("hash-object", "-w", f)+","+name)
	}
	tree := r.git("write-tree")
	pr := r.git("commit-tree", tree, "-p", "main", "-m", "change the rules")
	r.git("update-ref", "--no-deref", "HEAD", r.git("commit-tree", tree, "-p", "main", "-p", pr, "-m", "merge"))
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
	// huge answers a listing with 64 MiB, its length unsaid, as long as
	// it is read; sent is how much of it was.
	huge bool
	sent atomic.Int64
	// cut answers a listing with the start of it, then hangs up.
	cut bool
	// statuses are the commit statuses set, as each host's API took them;
	// failStatus refuses one. iterations are, on Azure DevOps, the source
	// commit of each of the pull request's iterations, the first 1.
	statuses   []hostStatus
	failStatus bool
	iterations []string
	// pending is called, h.mu held, as a status is taken whose state is
	// the host's word for pending (pendingState).
	pending func()
}

// pendingState is each host's word for a status of a job under way, as its
// reference has it: GitHub's, Gitea's and Azure DevOps' pending, GitLab's
// running, Bitbucket's INPROGRESS.
var pendingState = map[string]string{"github": "pending", "gitea": "pending", "gitlab": "running", "bitbucket": "INPROGRESS", "azure": "pending"}

// hostStatus is a commit status a stand-in host took, in each host's
// words: State as it was sent, Context its name (Azure DevOps'
// genre/name), and what else the host's API reference names.
type hostStatus struct {
	SHA, State, Context, Text, URL string
	// Ref is Bitbucket's refname; Iteration Azure DevOps' iterationId;
	// Pipeline GitLab's pipeline_id.
	Ref                 string
	Iteration, Pipeline int
}

// The account the stand-ins' token is, as each host names it.
var hostMe = map[string]string{"github": "lapilli-bot", "gitea": "lapilli-bot", "gitlab": "42",
	"bitbucket": "{0b0b0b0b-0000-4000-8000-000000000042}", "azure": "6c1f3b52-0000-4000-8000-000000000042"}

// add keeps a comment as if posted, by author.
func (h *commentHost) add(author, body string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.put(author, body)
}

// put is add, h.mu held.
func (h *commentHost) put(author, body string) int {
	h.next++
	h.comments = append(h.comments, hostComment{ID: h.next, Thread: 1000 + h.next, Author: author, Body: body})
	return h.next
}

// set changes h as change says, h.mu held: between runs, as the host's
// handler reads it, the run before's connections maybe still open.
func (h *commentHost) set(change func(*commentHost)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	change(h)
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
	// Commit statuses, as each host's reference has them: GitHub's and
	// Gitea's POST /repos/{owner}/{repo}/statuses/{sha}, GitLab's POST
	// /projects/:id/statuses/:sha, Bitbucket's POST .../commit/{commit}/
	// statuses/build, each 201; Azure DevOps' POST .../pullRequests/{id}/
	// statuses, 200, on an iteration GET .../iterations lists.
	statusPath := map[string]string{"github": "/repos/o/r/statuses/", "gitea": "/repos/o/r/statuses/", "gitlab": "/projects/o%2Fr/statuses/",
		"bitbucket": "/repositories/ws/repo/commit/", "azure": "/org/my%20project/_apis/git/repositories/rules/pullRequests/7/statuses"}[h.kind]
	if r.Method == http.MethodPost && strings.HasPrefix(path, statusPath) {
		h.status(w, r, strings.TrimPrefix(path, statusPath))
		return
	}
	switch {
	case r.Method == http.MethodGet && h.kind == "bitbucket" && path == "/repositories/ws/repo/pullrequests/7":
		reply(map[string]any{"id": 7, "source": map[string]any{"branch": map[string]any{"name": "pr"}, "commit": map[string]any{"hash": "0123456789ab"}}})
	case r.Method == http.MethodGet && h.kind == "azure" && path == "/org/my%20project/_apis/git/repositories/rules/pullRequests/7/iterations":
		var its []any
		for i, c := range h.iterations {
			its = append(its, map[string]any{"id": i + 1, "sourceRefCommit": map[string]any{"commitId": c}})
		}
		reply(map[string]any{"value": its, "count": len(its)})
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
	case r.Method == http.MethodGet && path == list && h.cut:
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `[{"id": 1, "bo`)
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	case r.Method == http.MethodGet && path == list && h.huge:
		streamed(w, 64<<20, &h.sent)
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
		id := h.put(hostMe[h.kind], body())
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

// status takes a commit status, h.mu held, as h's host does: rest is
// what follows the path statuses are set at; a status the host's
// reference wouldn't take is an error of the test's.
func (h *commentHost) status(w http.ResponseWriter, r *http.Request, rest string) {
	var in map[string]any
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		h.t.Errorf("%s: a status that isn't JSON: %v", h.kind, err)
	}
	str := func(k string) string { s, _ := in[k].(string); return s }
	num := func(k string) int { n, _ := in[k].(float64); return int(n) }
	st := hostStatus{SHA: rest, State: str("state"), Context: str("context"), Text: str("description"), URL: str("target_url")}
	states := []string{"pending", "success", "failure"}
	known := []string{"state", "context", "description", "target_url"}
	code := http.StatusCreated
	switch h.kind {
	case "gitlab":
		st.Context, st.Pipeline = str("name"), num("pipeline_id")
		states, known = []string{"running", "success", "failed"}, []string{"state", "name", "description", "target_url", "pipeline_id"}
		if len(st.Text) > 255 || len(st.URL) > 255 {
			http.Error(w, `{"message":"must be 255 characters or fewer"}`, http.StatusBadRequest)
			return
		}
	case "bitbucket":
		sha, ok := strings.CutSuffix(rest, "/statuses/build")
		st.SHA, st.Context, st.URL, st.Ref = sha, str("name"), str("url"), str("refname")
		states, known = []string{"INPROGRESS", "SUCCESSFUL", "FAILED"}, []string{"key", "state", "name", "description", "url", "refname"}
		if !ok || str("key") != strings.ReplaceAll(st.Context, "/", "-") {
			h.t.Errorf("bitbucket: a build status at %s, key %q", rest, str("key"))
		}
	case "azure":
		c, _ := in["context"].(map[string]any)
		genre, _ := c["genre"].(string)
		name, _ := c["name"].(string)
		st.SHA, st.Context, st.URL, st.Iteration = "", genre+"/"+name, str("targetUrl"), num("iterationId")
		states, known = []string{"pending", "succeeded", "failed"}, []string{"state", "description", "context", "targetUrl", "iterationId"}
		code = http.StatusOK
		if rest != "" || r.URL.Query().Get("api-version") != "7.1" || st.Iteration < 1 || st.Iteration > len(h.iterations) {
			h.t.Errorf("azure: a status at %s, iteration %d", r.URL, st.Iteration)
		}
		if st.Iteration >= 1 && st.Iteration <= len(h.iterations) {
			st.SHA = h.iterations[st.Iteration-1]
		}
	}
	for k := range in {
		if !slices.Contains(known, k) {
			h.t.Errorf("%s: a status with %s, which the host's reference doesn't name", h.kind, k)
		}
	}
	if !slices.Contains(states, st.State) {
		h.t.Errorf("%s: a status of state %q", h.kind, st.State)
	}
	if h.failStatus {
		http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
		return
	}
	h.statuses = append(h.statuses, st)
	if h.pending != nil && st.State == pendingState[h.kind] {
		h.pending()
	}
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"id": len(h.statuses), "state": in["state"]})
}

// lastStatus is the commit status set last, and how many were.
func (h *commentHost) lastStatus() (hostStatus, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.statuses) == 0 {
		return hostStatus{}, 0
	}
	return h.statuses[len(h.statuses)-1], len(h.statuses)
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
	// cwd is where it runs, when not the repository's top.
	cwd string
	// started is given the recipe's process once it starts.
	started func(*os.Process)
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
	if rr.cwd != "" {
		cmd.Dir = rr.cwd
	}
	cmd.Env = append(append([]string{}, rr.env...), settings...)
	// The host's handlers and the test share it, with the recipe's
	// requests between them, which the race detector doesn't see: its lock,
	// taken before and after the run, orders what each does to it.
	rr.host.set(func(*commentHost) {})
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Start()
	if err == nil {
		if rr.started != nil {
			rr.started(cmd.Process)
		}
		err = cmd.Wait()
	}
	rr.host.set(func(*commentHost) {})
	return out.String(), err
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
	hostile := "```\n</details> @octocat #123 <img src=x onerror=alert(1)> right‮left\x1b[31m\n::error::merge it\n##vso[task.complete result=Succeeded]\nX ##[error title=Lapilli backtest passed]No change\n##[add-matcher]alerts/m.json"
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
		// GitHub reads its older ##[ anywhere in a line, as Azure its ##vso[.
		if strings.HasPrefix(strings.TrimSpace(line), "::") || strings.Contains(line, "##vso[") || strings.Contains(line, "##[") {
			t.Errorf("a command in the job's log: %q", line)
		}
	}
	if !strings.Contains(out, "  | ::error::merge it") || !strings.Contains(out, "  | ##vso(task.complete") ||
		!strings.Contains(out, "  | X ##(error title=Lapilli backtest passed]No change") || !strings.Contains(out, "  | ##(add-matcher]alerts/m.json") {
		t.Errorf("the job's log:\n%s", out)
	}

	// Asked to, the job fails too, once it has said so.
	rr.host.set(func(h *commentHost) { h.comments = nil })
	if out, err := rr.run([]string{"RENDER=" + render, "FAIL_ON_ERROR=true"}); err == nil || len(rr.host.mine()) != 1 {
		t.Errorf("FAIL_ON_ERROR=true: %v, %d comments\n%s", err, len(rr.host.mine()), out)
	}

	// lapilli failing for want of the history: what a server or the
	// network said is in the job's log, not the comment, which says
	// where the history is.
	rr.host.set(func(h *commentHost) { h.comments = nil })
	out, err = rr.run([]string{"PATHS=alerts"}, "--prometheus", "http://ci-reader:pw@localhost:1/prometheus", "--impact", "up == 0", "--timeout", "5s")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	block = failure(t, rr.body(), "the backtest failed, or didn't finish (lapilli exited 1)")
	if m := regexp.MustCompile(`127\.0\.0\.1|\[::1\]|ci-reader|localhost|query failed|dial tcp`).FindString(block); m != "" || !strings.Contains(out, "localhost:1") {
		t.Errorf("the block says %q: %q\n%s", m, block, out)
	}
	// Of what lapilli said, the rest goes in, where the history is taken
	// out: the URLs given, a URL's user and password, any address.
	rr.host.set(func(h *commentHost) { h.comments = nil })
	userinfo := filepath.Join(t.TempDir(), "lapilli")
	os.WriteFile(userinfo, []byte("#!/bin/sh\necho 'lapilli verdict: --on-call: schedule Ops at https://other-user:s3cret@elsewhere.example/x (10.1.2.3:9090, [fd00::1]:443) read from prom.corp.example.' >&2\nexit 1\n"), 0o755)
	rr.run([]string{"PATHS=alerts", "LAPILLI=" + userinfo}, "--prometheus", "http://prom.corp.example:9090")
	if block := failure(t, rr.body(), "the backtest failed, or didn't finish (lapilli exited 1)"); strings.Contains(block, "other-user") || strings.Contains(block, "corp") ||
		strings.Contains(block, "10.") || !strings.Contains(block, `https://elsewhere.example/x ([address], [address]) read from [hidden].`) {
		t.Errorf("the block: %q", block)
	}

	// A lapilli from before --markdown says so.
	rr.host.set(func(h *commentHost) { h.comments = nil })
	old := filepath.Join(t.TempDir(), "lapilli")
	os.WriteFile(old, []byte("#!/bin/sh\necho 'lapilli verdict: --markdown needs a value' >&2\nexit 2\n"), 0o755)
	if out, err := rr.run([]string{"PATHS=alerts", "LAPILLI=" + old}, "--prometheus", "http://127.0.0.1:1"); err != nil {
		t.Errorf("an old lapilli: %v\n%s", err, out)
	}
	failure(t, rr.body(), "the lapilli LAPILLI names has no --markdown: give a later release")

	// A report from a run that failed is no reason to say nothing: one that
	// says nothing changed, then exits 1, is posted, failure and all.
	rr.host.set(func(h *commentHost) { h.comments = nil })
	lying := filepath.Join(t.TempDir(), "lapilli")
	os.WriteFile(lying, []byte("#!/bin/sh\nprintf '%s\\n' '"+verdict.SummaryLine+`{"nothing_changed":true,"unreplayed":0}`+")' '"+verdict.NothingChangedHeading+"'\nexit 1\n"), 0o755)
	if out, err := rr.run([]string{"PATHS=alerts", "LAPILLI=" + lying}, "--prometheus", "http://127.0.0.1:1"); err != nil || len(rr.host.mine()) != 1 {
		t.Errorf("a report of a run that failed: %v %+v\n%s", err, rr.host.mine(), out)
	}

	// The comment can't be posted: the job fails only when asked to.
	rr.host.set(func(h *commentHost) { h.comments, h.failPost = nil, true })
	if out, err := rr.run([]string{"RENDER=" + render}); err != nil || !strings.Contains(out, "the comment couldn't be posted") {
		t.Errorf("not posted: %v\n%s", err, out)
	}
	if out, err := rr.run([]string{"RENDER=" + render, "FAIL_ON_ERROR=true"}); err == nil {
		t.Errorf("not posted, FAIL_ON_ERROR=true: the job succeeded\n%s", out)
	}
	rr.host.set(func(h *commentHost) { h.failPost = false })

	// Asked to fail when it does, a backtest of rules that couldn't be
	// replayed, with the history out of reach, fails.
	rr.host.set(func(h *commentHost) { h.comments = nil })
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
	rr.host.set(func(h *commentHost) { h.comments = nil })
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
	rr.host.set(func(h *commentHost) { h.comments = nil })
	rr.run([]string{"RENDER=cat alerts/shop.yaml", "RENDER_PREPARE=ln -s /etc/hosts alerts/hosts.yaml"})
	failure(t, rr.body(), "RENDER_PREPARE left a symbolic link in the commit it goes into: not read (the job's log says where)")
	rr.host.set(func(h *commentHost) { h.comments = nil })
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
		// The recipe fails the job itself, on every rule that differs that
		// couldn't be replayed.
		{[]string{"PATHS=alerts"}, []string{"--fail-unreplayed"}, "with FAIL_ON_ERROR=true"},
		{[]string{"PATHS=alerts"}, []string{"--fail-unreplayed=true"}, "with FAIL_ON_ERROR=true"},
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
		{[]string{"PATHS=alert"}, []string{"--prometheus", "http://127.0.0.1:1"}, "PATHS: alert: in neither commit"},
		// One of them, beside one that is there: its rules would never be
		// compared.
		{[]string{"PATHS=alerts mroe"}, []string{"--prometheus", "http://127.0.0.1:1"}, "PATHS: mroe: in neither commit"},
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
		rr.host.set(func(h *commentHost) { h.comments = nil })
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

// backtestAction is action.yml as it is read.
type backtestAction struct {
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

func readBacktestAction(t *testing.T) backtestAction {
	t.Helper()
	b, err := os.ReadFile(backtestRecipes + "action.yml")
	if err != nil {
		t.Fatal(err)
	}
	var action backtestAction
	if err := yaml.Unmarshal(b, &action); err != nil {
		t.Fatal(err)
	}
	if action.Runs.Using != "composite" || len(action.Runs.Steps) != 1 || action.Runs.Steps[0].Shell != "sh" {
		t.Fatalf("runs: %+v", action.Runs)
	}
	return action
}

// runAction runs action.yml's step for rr's pull request, as GitHub
// Actions runs a composite action: each input, given or its default,
// reaching the script as a variable; env adds to the job's variables, or
// takes the place of one. It gives what the step said, and whether it
// succeeded.
func runAction(t *testing.T, rr *recipeRun, inputs map[string]string, env ...string) (string, error) {
	t.Helper()
	action := readBacktestAction(t)
	step := action.Runs.Steps[0]
	temp := t.TempDir()
	vars := []string{"GITHUB_ACTION_PATH=" + must(filepath.Abs(backtestRecipes)), "RUNNER_TEMP=" + temp, "GITHUB_ACTIONS=true",
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
		vars = append(vars, name+"="+val)
	}
	for in := range inputs {
		if _, ok := action.Inputs[in]; !ok {
			t.Fatalf("no input %s", in)
		}
	}
	cmd := exec.Command("sh", "-c", step.Run)
	cmd.Dir = rr.repo.dir
	cmd.Env = append(append(append([]string{}, rr.repo.env...), vars...), env...)
	rr.host.set(func(*commentHost) {}) // as rr.run orders them
	out, err := cmd.CombinedOutput()
	rr.host.set(func(*commentHost) {})
	if left, _ := os.ReadDir(temp); len(left) != 0 {
		t.Errorf("left in RUNNER_TEMP: %v", left)
	}
	return string(out), err
}

// action.yml runs github.sh with its inputs, as GitHub Actions runs a
// composite action: each input reaches the script as a variable, never in
// its text; args are split at spaces and never expanded; the token is in a
// file only while the step runs.
func TestBacktestAction(t *testing.T) {
	needRecipeTools(t)
	step := readBacktestAction(t).Runs.Steps[0]
	if strings.Contains(step.Run, "${{") {
		t.Errorf("an expression in the script's text, where an input could write shell: %s", step.Run)
	}
	prom := promStandIn(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	pwned := filepath.Join(t.TempDir(), "pwned")
	run := func(inputs map[string]string) string {
		t.Helper()
		out, err := runAction(t, rr, inputs)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return out
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

// The summary is a contract with the tools that read it: the docs' page
// on pull request backtests names each of its fields, and the form of a
// comparison with nothing changed.
func TestSummaryDocumented(t *testing.T) {
	b, err := os.ReadFile("../../docs/pr-backtests.md")
	if err != nil {
		t.Fatal(err)
	}
	section := string(b)
	ty := reflect.TypeOf(verdict.Summary{})
	for i := range ty.NumField() {
		tag := strings.Split(ty.Field(i).Tag.Get("json"), ",")
		if len(tag) != 1 || tag[0] == "" {
			t.Errorf("%s: %q, want a name and nothing else (no omitempty: every field is always there)", ty.Field(i).Name, tag)
			continue
		}
		if !strings.Contains(section, "`"+tag[0]+"`") {
			t.Errorf("docs/pr-backtests.md doesn't name %s", tag[0])
		}
	}
	if !strings.Contains(section, "  "+verdict.NothingChangedHeading+"\n\n  Nothing that decides when the 42 rules fire has changed: nothing to replay.\n") {
		t.Error("docs/pr-backtests.md doesn't show the form of nothing changed")
	}
	if !strings.Contains(section, verdict.SummaryLine) {
		t.Error("docs/pr-backtests.md doesn't show the summary line")
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
	if !strings.Contains(out, "couldn't be replayed: "+rules.HistoryUnread+"Post") {
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
	rr.host.set(func(h *commentHost) { h.comments = nil })
	repo.git("checkout", "--quiet", "main")
	os.Remove(filepath.Join(repo.dir, "incidents.yaml"))
	os.Symlink("/dev/zero", filepath.Join(repo.dir, "incidents.yaml"))
	repo.git("add", "-A")
	repo.git("commit", "--quiet", "-m", "a link")
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	rr.run([]string{"PATHS=alerts", "INCIDENTS=incidents.yaml"}, "--prometheus", prom.URL)
	failure(t, rr.body(), "INCIDENTS incidents.yaml isn't a file in the commit the pull request goes into")
}

// INCIDENTS is read from TARGET itself, the commit the pull request goes
// into as it is now, not from where the pull request branched from its
// branch, which its author picks: a pull request branched from before an
// incident was recorded, its own branch checked out (Gitea's, Forgejo's,
// Bitbucket's, GitLab's without merged results), is judged by it all the
// same. Its rules, and the expectations of the rules in place, are still
// those of where it branched: a budget the target lowered since, which the
// pull request didn't touch, isn't said changed in it, through lapilli
// serve too.
func TestBacktestRecipeIncidentsFromTarget(t *testing.T) {
	needRecipeTools(t)
	loose := "expectations:\n  - rule: CheckoutErrors\n    budget: {week: 50}\n"
	tight := "expectations:\n  - rule: CheckoutErrors\n    budget: {week: 5}\n"
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m"), "incidents.yaml": "incidents: []\n", "expect.yaml": loose})
	old := repo.git("rev-parse", "HEAD")
	repo.write(map[string]string{"incidents.yaml": "incidents:\n  - {id: INC-9, start: 2026-11-10T00:00:00Z, end: 2026-11-10T01:00:00Z}\n", "expect.yaml": tight})
	repo.git("commit", "--quiet", "-am", "record INC-9, and a tighter budget")
	repo.git("checkout", "--quiet", "-b", "pr", old)
	repo.write(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	repo.git("commit", "--quiet", "-am", "slower")
	fake, given := argsLapilli(t)
	rr := newRecipeRun(t, backtestHosts[0], repo)
	out, err := rr.run([]string{"PATHS=alerts", "INCIDENTS=incidents.yaml", "EXPECT=expect.yaml", "TARGET=main", "LAPILLI=" + fake}, "--prometheus", "http://127.0.0.1:9")
	incidents, before, compared := given()
	if err != nil || !strings.Contains(incidents, "INC-9") || before != loose || compared != shopRules("5m") {
		t.Errorf("%v: given incidents %q, expectations of the rules in place %q, rules compared against %q\n%s", err, incidents, before, compared, out)
	}
	// Judged by lapilli: the budget the pull request didn't touch isn't
	// said changed in it.
	prom := promStandIn(t)
	rr.host.set(func(h *commentHost) { h.comments = nil })
	if out, err := rr.run([]string{"PATHS=alerts", "EXPECT=expect.yaml", "TARGET=main"}, "--prometheus", prom.URL, "--tz", "UTC"); err != nil ||
		strings.Contains(rr.body(), "in this change") || !strings.Contains(rr.body(), "#### Expectations") {
		t.Errorf("the budget, not touched: %v\n%s\n%s", err, rr.body(), out)
	}
	// Through lapilli serve, the expectations alike.
	srv := newServeStandIn(t)
	srv.answers(nil, []serveAnswer{serveDone(serveMarkdown(changedSummary))})
	sr := serveRun(t, backtestHosts[0], repo, srv)
	if out, err := sr.run([]string{"PATHS=alerts", "EXPECT=expect.yaml", "TARGET=main"}); err != nil || srv.sent()["expect_before"] != loose {
		t.Errorf("through serve: %v, expect_before %q\n%s", err, srv.sent()["expect_before"], out)
	}
}

// argsLapilli is a lapilli that keeps what a recipe gave it, read by
// given: the incidents, the expectations of the rules in place and the
// rules compared against's shop.yaml.
func argsLapilli(t *testing.T) (string, func() (string, string, string)) {
	t.Helper()
	dir := t.TempDir()
	fake := filepath.Join(t.TempDir(), "lapilli")
	os.WriteFile(fake, []byte("#!/bin/sh\nwhile [ $# -gt 0 ]; do\n  case $1 in\n    --incidents) cp \"$2\" '"+dir+"/incidents' ;;\n"+
		"    --expect-before) cp \"$2\" '"+dir+"/expect-before' ;;\n    --compare) rm -rf '"+dir+"/compare'; cp -R \"$2\" '"+dir+"/compare' ;;\n  esac\n  shift\ndone\n"+
		"echo '[//]: # (lapilli-compare {\"nothing_changed\":false,\"changed\":1,\"unreplayed\":0})'\n"), 0o755)
	return fake, func() (string, string, string) {
		read := func(p ...string) string {
			b, _ := os.ReadFile(filepath.Join(append([]string{dir}, p...)...))
			return string(b)
		}
		return read("incidents"), read("expect-before"), read("compare", "shop.yaml")
	}
}

// gitlab.sh in a merge request pipeline without merged results takes the
// target branch (CI_MERGE_REQUEST_TARGET_BRANCH_NAME), fetched from origin
// as the job's clone lets it, for TARGET, not the merge base
// (CI_MERGE_REQUEST_DIFF_BASE_SHA): INCIDENTS is the branch's as it is,
// the rules compared from where the two parted. With merged results, the
// target branch's commit. A branch's name it doesn't take leaves the merge
// base, saying so.
func TestBacktestRecipeGitLabTarget(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m"), "incidents.yaml": "incidents: []\n"})
	old := repo.git("rev-parse", "HEAD")
	repo.write(map[string]string{"incidents.yaml": "incidents:\n  - {id: INC-9, start: 2026-11-10T00:00:00Z, end: 2026-11-10T01:00:00Z}\n"})
	repo.git("commit", "--quiet", "-am", "record INC-9")
	tip := repo.git("rev-parse", "HEAD")
	repo.git("checkout", "--quiet", "-b", "pr", old)
	repo.write(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	repo.git("commit", "--quiet", "-am", "slower")
	repo.git("remote", "add", "origin", repo.dir)
	fake, given := argsLapilli(t)
	gitlab := backtestHosts[2]
	for _, c := range []struct {
		name     string
		ci       []string
		incident bool
		said     string
	}{
		{"no merged results", []string{"CI_MERGE_REQUEST_TARGET_BRANCH_NAME=main", "CI_MERGE_REQUEST_DIFF_BASE_SHA=" + old}, true, ""},
		{"merged results", []string{"CI_MERGE_REQUEST_TARGET_BRANCH_SHA=" + tip, "CI_MERGE_REQUEST_TARGET_BRANCH_NAME=main", "CI_MERGE_REQUEST_DIFF_BASE_SHA=" + old}, true, ""},
		{"a name not taken", []string{"CI_MERGE_REQUEST_TARGET_BRANCH_NAME=main;x", "CI_MERGE_REQUEST_DIFF_BASE_SHA=" + old}, false,
			"gitlab.sh: the target branch's name isn't one taken as TARGET_BRANCH: compared with the merge base, INCIDENTS read from it"},
	} {
		rr := newRecipeRun(t, gitlab, repo)
		out, err := rr.run(append([]string{"PATHS=alerts", "INCIDENTS=incidents.yaml", "LAPILLI=" + fake}, c.ci...), "--prometheus", "http://127.0.0.1:9")
		incidents, _, compared := given()
		if err != nil || strings.Contains(incidents, "INC-9") != c.incident || compared != shopRules("5m") || !strings.Contains(out, c.said) {
			t.Errorf("%s: %v: given incidents %q, rules compared against %q\n%s", c.name, err, incidents, compared, out)
		}
	}
}

// Two paths under PATHS that differ only in case, two files or a file and
// a directory, are one on a file system that folds case (a macOS or
// Windows runner's): one's rules would never be backtested, and the gates
// would pass. Such a commit isn't read, on any runner, the pair said in
// the job's log and not the comment; through lapilli serve too, nothing
// sent. A directory named in two cases, each of its own files, is read.
func TestBacktestRecipeCaseFolded(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	page := "groups:\n  - name: sneaky\n    rules:\n      - alert: PageEveryone\n        expr: vector(1)\n        labels: {severity: page}\n"
	// Refused where the file system folds case (a macOS or Windows
	// runner's); elsewhere both paths are written and read.
	foldsAt := t.TempDir()
	os.WriteFile(filepath.Join(foldsAt, "case-probe"), nil, 0o600)
	_, err := os.Stat(filepath.Join(foldsAt, "CASE-PROBE"))
	folds := err == nil
	for name, c := range map[string]struct{ path, pair string }{
		"two files":            {"alerts/Shop.yaml", "alerts/Shop.yaml and alerts/shop.yaml"},
		"a file, a directory":  {"alerts/SHOP.yaml/page.yaml", "alerts/SHOP.yaml and alerts/shop.yaml"},
		"below, in LOKI_PATHS": {"logs/team/Page.yaml", "logs/team/Page.yaml and logs/team/page.yaml"},
	} {
		t.Run(name, func(t *testing.T) {
			logs := "groups:\n  - name: logs\n    rules:\n      - alert: LogErrors\n        expr: sum(count_over_time({app=\"x\"}[5m])) > 5\n"
			repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m"), "logs/team/page.yaml": logs})
			repo.pullRequestTree(map[string]string{c.path: page})
			why := "the pull request's commit holds two paths that differ only in case, which a file system that folds case (macOS's, Windows') writes as one: not read (the job's log says which)"
			if !folds {
				if strings.HasPrefix(c.path, "alerts/") {
					rr := newRecipeRun(t, backtestHosts[0], repo)
					out, _ := rr.run([]string{"PATHS=alerts", "FAIL_ON_ERROR=true"}, "--prometheus", prom.URL, "--tz", "UTC")
					if strings.Contains(out, "differ only in case") || strings.Contains(rr.body(), "differ only in case") || !strings.Contains(rr.body(), "PageEveryone") {
						t.Errorf("a file system that doesn't fold case: both read\n%s\n%s", rr.body(), out)
					}
				}
				srv := newServeStandIn(t)
				srv.answers(nil, []serveAnswer{serveDone(serveMarkdown(`{"nothing_changed":false,"changed":0,"added":1,"removed":0,"same":1,"alike":0,"unreplayed":0,"expected":true,"expectations_broken":0,"unexpected":0}`))})
				sr := serveRun(t, backtestHosts[0], repo, srv)
				out, _ := sr.run([]string{"PATHS=alerts", "LOKI_PATHS=logs"})
				if posts, _, _ := srv.counts(); posts != 1 || strings.Contains(out, "differ only in case") {
					t.Errorf("through serve, a file system that doesn't fold case: %d POSTs\n%s", posts, out)
				}
				return
			}
			if strings.HasPrefix(c.path, "alerts/") {
				rr := newRecipeRun(t, backtestHosts[0], repo)
				out, err := rr.run([]string{"PATHS=alerts", "FAIL_ON_ERROR=true"}, "--prometheus", prom.URL, "--tz", "UTC")
				block := failure(t, rr.body(), why)
				if err == nil || !strings.Contains(out, "two paths that differ only in case, in the pull request's commit: "+c.pair) ||
					strings.Contains(rr.body(), "SHOP") || strings.Contains(rr.body(), "Shop") {
					t.Errorf("%v\n%s\n%s\n%s", err, block, rr.body(), out)
				}
			}
			srv := newServeStandIn(t)
			sr := serveRun(t, backtestHosts[0], repo, srv)
			out, _ := sr.run([]string{"PATHS=alerts", "LOKI_PATHS=logs"})
			failure(t, sr.body(), why)
			if posts, _, _ := srv.counts(); posts != 0 || !strings.Contains(out, c.pair) {
				t.Errorf("through serve: %d POSTs\n%s", posts, out)
			}
		})
	}
	// A letter past ASCII in another case, which a file system that folds
	// case takes as one too: seen by the files written, fewer than the
	// commit holds, where it does.
	probe := t.TempDir()
	os.WriteFile(filepath.Join(probe, "\u00e9"), nil, 0o600)
	if _, err := os.Stat(filepath.Join(probe, "\u00c9")); err == nil {
		repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m"), "alerts/\u00e9.yaml": shopRules("10m")})
		repo.pullRequestTree(map[string]string{"alerts/\u00c9.yaml": page})
		rr := newRecipeRun(t, backtestHosts[0], repo)
		out, err := rr.run([]string{"PATHS=alerts", "FAIL_ON_ERROR=true"}, "--prometheus", prom.URL, "--tz", "UTC")
		failure(t, rr.body(), "the pull request's commit holds files whose names this machine's file system takes as one, as one that folds case does (macOS's, Windows'): not read (the job's log says how many)")
		if err == nil || !strings.Contains(out, "the pull request's commit: 3 files, 2 of them once written here") {
			t.Errorf("past ASCII: %v\n%s", err, out)
		}
	}
	// A directory in two cases, a file of its own in each: read.
	repo := newPRRepo(t, map[string]string{"alerts/team/shop.yaml": shopRules("5m")})
	repo.pullRequestTree(map[string]string{"alerts/TEAM/page.yaml": page})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	if out, err := rr.run([]string{"PATHS=alerts", "FAIL_ON_ERROR=true"}, "--prometheus", prom.URL, "--tz", "UTC"); err != nil || strings.Contains(rr.body(), "differ only in case") {
		t.Errorf("a directory in two cases: %v\n%s\n%s", err, rr.body(), out)
	}
}

// The files written are counted without reading their names: a decoy
// whose name holds a line's end, two lines to find, makes up for no file
// lost to a pair the check by case can't see (\u00c9.yaml over
// \u00e9.yaml, quoted by ls-tree), where the file system folds case. Alone,
// on any file system, such a file is read.
func TestBacktestRecipeCaseFoldNewline(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequestTree(map[string]string{"alerts/shop.yaml": shopRules("2h"), "alerts/x\ny.yaml": "groups: []\n"})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	if out, err := rr.run([]string{"PATHS=alerts", "FAIL_ON_ERROR=true"}, "--prometheus", prom.URL, "--tz", "UTC"); err != nil ||
		strings.Contains(rr.body(), "takes as one") || strings.Contains(rr.body(), "### Lapilli backtest failed") {
		t.Errorf("a name with a line's end, alone: %v\n%s\n%s", err, rr.body(), out)
	}
	probe := t.TempDir()
	os.WriteFile(filepath.Join(probe, "\u00e9"), nil, 0o600)
	if _, err := os.Stat(filepath.Join(probe, "\u00c9")); err != nil {
		return // a file system that doesn't fold case writes both
	}
	page := "groups:\n  - name: sneaky\n    rules:\n      - alert: PageEveryone\n        expr: vector(1)\n        labels: {severity: page}\n"
	for name, decoy := range map[string]bool{"no decoy": false, "a decoy": true} {
		repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m"), "alerts/\u00e9.yaml": shopRules("10m")})
		files := map[string]string{"alerts/\u00c9.yaml": page}
		if decoy {
			files["alerts/x\ny.yaml"] = "groups: []\n"
		}
		repo.pullRequestTree(files)
		rr := newRecipeRun(t, backtestHosts[0], repo)
		out, err := rr.run([]string{"PATHS=alerts", "FAIL_ON_ERROR=true"}, "--prometheus", prom.URL, "--tz", "UTC")
		if err == nil || !strings.Contains(rr.body(), "takes as one") {
			t.Errorf("%s: read (%v), \u00c9.yaml's PageEveryone never backtested\n%s\n%s", name, err, rr.body(), out)
		}
	}
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
	render := `printf '%s %s\n' "$(basename "$HOME")" "${CI_JOB_TOKEN:-}${GITHUB_TOKEN_FILE:-}${SYSTEM_ACCESSTOKEN:-}${ACTIONS_ID_TOKEN_REQUEST_TOKEN:-}${CI_REPOSITORY_URL:-}${CI_DEPENDENCY_PROXY_PASSWORD:-}" >>` +
		homes + `; [ "$HOME" != "$JOB_HOME" ] && cat chart/out.yaml`
	out, err := rr.run([]string{"RENDER=" + render, "JOB_HOME=" + rr.home(), "CI_JOB_TOKEN=job", "SYSTEM_ACCESSTOKEN=azure", "ACTIONS_ID_TOKEN_REQUEST_TOKEN=oidc",
		"CI_REPOSITORY_URL=https://gitlab-ci-token:job@gitlab.example/o/r.git", "CI_DEPENDENCY_PROXY_PASSWORD=job"},
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

// A history that refuses the job, with a reason that says who the job is
// and where it reads from: the reason is in the job's log, never the
// comment, whether the job is asked to fail or lapilli doesn't finish.
func TestBacktestRecipeRefusedReason(t *testing.T) {
	needRecipeTools(t)
	reason := `{"status":"error","errorType":"forbidden","error":"User: arn:aws:sts::123456789012:assumed-role/ci-metrics-reader/i-0abc is not authorized; via internal-gw.corp.example.net"}`
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, reason)
	}))
	defer prom.Close()
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	leaks := regexp.MustCompile(`123456789012|ci-metrics-reader|internal-gw|corp\.example`)
	out, err := rr.run([]string{"PATHS=alerts", "FAIL_ON_ERROR=true"}, "--prometheus", prom.URL)
	if err == nil {
		t.Errorf("FAIL_ON_ERROR=true, nothing replayed: the job succeeded")
	}
	block := failure(t, rr.body(), "1 of the rules that differ couldn't be replayed (FAIL_ON_ERROR=true)")
	if m := leaks.FindString(rr.body()); m != "" || strings.TrimSpace(block) != "1 of the rules that differ couldn't be replayed (FAIL_ON_ERROR=true)." {
		t.Errorf("the comment says %q:\n%s", m, rr.body())
	}
	if !strings.Contains(out, "123456789012") {
		t.Errorf("the job's log doesn't say why:\n%s", out)
	}
	// lapilli exits 1 having said why rules couldn't be replayed: the
	// same.
	rr.host.set(func(h *commentHost) { h.comments = nil })
	failing := filepath.Join(t.TempDir(), "lapilli")
	os.WriteFile(failing, []byte("#!/bin/sh\necho 'lapilli verdict: CheckoutErrors (after) couldn'\"'\"'t be replayed: query failed: HTTP 403: User: arn:aws:sts::123456789012:assumed-role/ci-metrics-reader' >&2\n"+
		"echo 'lapilli verdict: the replay did not finish within --timeout' >&2\nexit 1\n"), 0o755)
	rr.run([]string{"PATHS=alerts", "LAPILLI=" + failing}, "--prometheus", prom.URL)
	block = failure(t, rr.body(), "the backtest failed, or didn't finish (lapilli exited 1)")
	if m := leaks.FindString(block); m != "" || !strings.Contains(block, "the replay did not finish within --timeout") {
		t.Errorf("the block says %q: %q", m, block)
	}
}

// lapilli's stderr reaches the job's log as it is written, each line after
// a "| ", so a long backtest's progress lines show the job isn't hung;
// none of them reaches the comment, though what else lapilli said does.
func TestBacktestRecipeProgress(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	dir := t.TempDir()
	seen, fake := filepath.Join(dir, "seen"), filepath.Join(dir, "lapilli")
	os.WriteFile(fake, []byte("#!/bin/sh\n"+
		"echo 'lapilli verdict: after 30s, 3 of 10 replays of the rules before and after done, 41 requests to the history, 5 answered HTTP 429' >&2\n"+
		"i=0\nwhile [ ! -e '"+seen+"' ] && [ $i -lt 300 ]; do sleep 0.1; i=$((i + 1)); done\n"+
		"[ -e '"+seen+"' ] || echo 'lapilli verdict: NOT LIVE: the job log had nothing yet' >&2\n"+
		"echo 'lapilli verdict: after 1m0s, 9 of 10 replays of the rules before and after done, 90 requests to the history' >&2\n"+
		"echo 'lapilli verdict: 1 of 10 rules didn'\"'\"'t finish within --timeout 1m; raise it, or shorten the period' >&2\nexit 1\n"), 0o755)
	script, _ := filepath.Abs(backtestRecipes + rr.h.script)
	shell := cmp.Or(os.Getenv("RECIPE_SH"), "sh")
	cmd := exec.Command(shell, script, "--prometheus", "http://127.0.0.1:1")
	cmd.Dir = repo.dir
	cmd.Env = append(append([]string{}, rr.env...), "PATHS=alerts", "LAPILLI="+fake)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	sc := bufio.NewScanner(stderr)
	for sc.Scan() {
		log.WriteString(sc.Text() + "\n")
		if strings.HasPrefix(sc.Text(), "  | lapilli verdict: after 30s,") {
			os.WriteFile(seen, nil, 0o600)
		}
	}
	cmd.Wait()
	if !strings.Contains(log.String(), "  | lapilli verdict: after 1m0s, 9 of 10") || strings.Contains(log.String(), "NOT LIVE") {
		t.Errorf("the job's log:\n%s", log.String())
	}
	block := failure(t, rr.body(), "the backtest failed, or didn't finish (lapilli exited 1)")
	if strings.Contains(rr.body(), "after 30s") || strings.Contains(rr.body(), "replays of the rules") || !strings.Contains(block, "1 of 10 rules didn't finish within --timeout 1m") {
		t.Errorf("the comment:\n%s", rr.body())
	}
}

// The branch a pull request goes into is taken from the clone when the
// checkout fetched it, as a full clone does: a fetch needs credentials the
// checkout may have dropped. When the clone hasn't it, it is fetched.
func TestBacktestRecipeTargetInClone(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	// The pull request's own commit checked out; origin unreachable, as
	// without the checkout's credentials; its main as the checkout left it.
	repo.git("checkout", "--quiet", "--detach", "pr")
	repo.git("remote", "add", "origin", filepath.Join(t.TempDir(), "gone.git"))
	repo.git("update-ref", "refs/remotes/origin/main", "main")
	rr := newRecipeRun(t, backtestHosts[1], repo)
	out, err := rr.run([]string{"PATHS=alerts", "TARGET_BRANCH=main"}, "--prometheus", prom.URL)
	if err != nil || strings.Contains(rr.body(), "### Lapilli backtest failed") || !strings.Contains(rr.body(), "| `CheckoutErrors` | `page` | 1 → 0") {
		t.Errorf("%v\n%s\n%s", err, rr.body(), out)
	}
	// Not in the clone, and origin out of reach: said.
	rr.host.set(func(h *commentHost) { h.comments = nil })
	repo.git("update-ref", "-d", "refs/remotes/origin/main")
	rr.run([]string{"PATHS=alerts", "TARGET_BRANCH=main"}, "--prometheus", prom.URL)
	failure(t, rr.body(), "the branch main isn't in the clone, and couldn't be fetched from origin: check out every branch, or let the job fetch (the recipes' README says how, CI by CI)")
}

// A file in the checkout is the pull request's, whichever of its
// directories REPO is, or the job runs from.
func TestBacktestRecipeCheckoutFromBelow(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"monitoring/alerts/shop.yaml": shopRules("5m"), "incidents.yaml": "incidents: []\n"})
	repo.pullRequest(map[string]string{"monitoring/alerts/shop.yaml": shopRules("2h")})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	for name, c := range map[string]struct {
		cwd      string
		settings []string
		file     string
	}{
		"REPO a directory of it":  {"", []string{"PATHS=monitoring/alerts", "REPO=monitoring"}, "incidents.yaml"},
		"run from a directory":    {filepath.Join(repo.dir, "monitoring"), []string{"PATHS=monitoring/alerts"}, "../incidents.yaml"},
		"its path from elsewhere": {"", []string{"PATHS=monitoring/alerts", "REPO=monitoring"}, filepath.Join(repo.dir, "incidents.yaml")},
	} {
		rr.cwd = c.cwd
		out, err := rr.run(c.settings, "--prometheus", "http://127.0.0.1:1", "--incidents", c.file)
		if err == nil || !strings.Contains(out, "a file in the checkout is the pull request's") {
			t.Errorf("%s: %v\n%s", name, err, out)
		}
	}
	if len(rr.host.comments) != 0 {
		t.Errorf("commented: %+v", rr.host.comments)
	}
}

// What is said in the comment is taken out first, then cut to its last
// characters: a host the cut would split is hidden whole.
func TestBacktestRecipeScrubThenCut(t *testing.T) {
	needRecipeTools(t)
	host := "prometheus.internal.example.com"
	// One line whose last 4,000 bytes start ten bytes into the host.
	head := "error reading " + host
	tail := " " + strings.Repeat("z", 4000-(len(host)-10)-2) + "\n"
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m"), "render.log": head + tail})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	if out, err := rr.run([]string{"RENDER=cat render.log >&2; exit 1"}, "--prometheus", "http://"+host+":9090"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	block := failure(t, rr.body(), "RENDER failed in the commit it goes into")
	if strings.Contains(block, "example") || strings.Contains(block, "internal") || !strings.Contains(block, "zzzz") {
		t.Errorf("part of the host: %q", block[:min(len(block), 200)])
	}
	if n := len([]rune(strings.TrimSuffix(strings.SplitN(block, "\n", 2)[1], "\n"))); n > 4000 {
		t.Errorf("%d characters of the log, more than 4,000", n)
	}
}

// INCIDENTS is read up to 8 MiB.
func TestBacktestRecipeIncidentsCap(t *testing.T) {
	needRecipeTools(t)
	big := "incidents: []\n#" + strings.Repeat("x", 8<<20) + "\n"
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m"), "incidents.yaml": big})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	rr.run([]string{"PATHS=alerts", "INCIDENTS=incidents.yaml"}, "--prometheus", "http://127.0.0.1:1")
	failure(t, rr.body(), "INCIDENTS incidents.yaml is more than 8 MiB")
}

// What the recipe says in the job's log of the pull request's, a link's
// path, is no CI's command: Azure's ##vso[ is read anywhere in a line.
func TestBacktestRecipeSaysNoCommand(t *testing.T) {
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	os.Symlink("/etc/hosts", filepath.Join(repo.dir, "alerts", "##vso[task.setvariable variable=OK]yes"))
	repo.git("add", "-A")
	repo.git("commit", "--quiet", "--amend", "--no-edit")
	rr := newRecipeRun(t, backtestHosts[4], repo)
	out, _ := rr.run([]string{"PATHS=alerts"}, "--prometheus", "http://127.0.0.1:1")
	if strings.Contains(out, "##vso[") || !strings.Contains(out, "a symbolic link in the pull request's commit: alerts/##vso(task.setvariable") {
		t.Errorf("the job's log:\n%s", out)
	}
}

// streamed writes up to n bytes, their length unsaid, for as long as they
// are read, adding to sent as it goes.
func streamed(w http.ResponseWriter, n int64, sent *atomic.Int64) {
	chunk := []byte(strings.Repeat(" ", 64<<10))
	for done := int64(0); done < n; done += int64(len(chunk)) {
		if _, err := w.Write(chunk); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		sent.Add(int64(len(chunk)))
	}
}

// curl before 8.4.0 caps only an answer that says its length: one that
// doesn't is cut at the cap all the same, the host's and a release's, and
// read no further than it.
func TestBacktestRecipeCapsAnswers(t *testing.T) {
	needRecipeTools(t)
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("releases are for Linux and macOS")
	}
	// A curl that ignores --max-filesize, as an old one does here.
	real, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("no curl")
	}
	shim := t.TempDir()
	os.WriteFile(filepath.Join(shim, "curl"), []byte(`#!/bin/sh
n=$#
while [ "$n" -gt 0 ]; do
  a=$1; shift; n=$((n - 1))
  if [ "$a" = --max-filesize ]; then shift; n=$((n - 1)); continue; fi
  set -- "$@" "$a"
done
exec '`+real+`' "$@"
`), 0o755)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	for i, e := range rr.env {
		if strings.HasPrefix(e, "PATH=") {
			rr.env[i] = "PATH=" + shim + string(os.PathListSeparator) + strings.TrimPrefix(e, "PATH=")
		}
	}
	rr.host.set(func(h *commentHost) { h.huge = true })
	out, _ := rr.run([]string{"PATHS=alerts"}, "--prometheus", "http://127.0.0.1:1")
	if !strings.Contains(out, "didn't list the pull request's comments (failed)") {
		t.Errorf("a listing of more than 16 MiB:\n%s", out)
	}
	// What the sockets and pipes between hold aside, read no further.
	if sent := rr.host.sent.Load(); sent > 40<<20 {
		t.Errorf("%d MiB of a listing read, past 16 MiB", sent>>20)
	}
	rr.host.set(func(h *commentHost) { h.huge = false })
	// An answer cut short is no answer, whatever its status said.
	rr.host.set(func(h *commentHost) { h.cut = true })
	out, _ = rr.run([]string{"PATHS=alerts"}, "--prometheus", "http://127.0.0.1:1")
	if !strings.Contains(out, "didn't list the pull request's comments (failed)") {
		t.Errorf("a listing cut short:\n%s", out)
	}
	rr.host.set(func(h *commentHost) { h.cut = false })
	var sums atomic.Int64
	releases := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/SHA256SUMS") {
			streamed(w, 64<<20, &sums)
			return
		}
		io.WriteString(w, "an archive") // its sum, never reached
	}))
	defer releases.Close()
	rr.run([]string{"PATHS=alerts", "LAPILLI=", "LAPILLI_VERSION=v9.9.9", "LAPILLI_RELEASES=" + releases.URL, "LAPILLI_SHA256=" + strings.Repeat("a", 64)},
		"--prometheus", "http://127.0.0.1:1")
	if body := rr.body(); !strings.Contains(body, "couldn't be downloaded") {
		t.Errorf("a SHA256SUMS of more than 64 KiB:\n%s", body)
	}
	if sent := sums.Load(); sent > 32<<20 {
		t.Errorf("%d MiB of a SHA256SUMS read, past 64 KiB", sent>>20)
	}
}

// EXPECT holds the rules that differ to the expectations, the pull
// request's file for the rules it proposes and the base's for those in
// place; the comment says what the change broke, and FAIL_ON_EXPECTATIONS
// fails the job for it, once the comment is posted.
func TestBacktestRecipeExpectations(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	// The rule in place, held 2h, never fires; the pull request's, 5m, once.
	none := "expectations:\n  - rule: CheckoutErrors\n    budget: {week: 0}\n"
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("2h"), "expect.yaml": none})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("5m")})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	out, err := rr.run([]string{"PATHS=alerts", "EXPECT=expect.yaml", "FAIL_ON_EXPECTATIONS=true"}, "--prometheus", prom.URL, "--tz", "UTC")
	if err == nil || !strings.Contains(rr.body(), "#### Expectations") || !strings.Contains(rr.body(), "**✗ broken by this change**") ||
		!strings.Contains(out, "the change breaks 1 expectations the rules in place meet (FAIL_ON_EXPECTATIONS=true)") ||
		strings.Contains(rr.body(), "### Lapilli backtest failed") {
		t.Errorf("held to it: %v\n%s\n%s", err, rr.body(), out)
	}
	// Not asked to fail: the comment says it, the job passes.
	rr.host.set(func(h *commentHost) { h.comments = nil })
	if out, err := rr.run([]string{"PATHS=alerts", "EXPECT=expect.yaml"}, "--prometheus", prom.URL, "--tz", "UTC"); err != nil ||
		!strings.Contains(rr.body(), "**✗ broken by this change**") {
		t.Errorf("not asked to fail: %v\n%s\n%s", err, rr.body(), out)
	}
	// The pull request raises the budget with the rule: met, and the
	// comment says it was 0.
	repo.git("checkout", "--quiet", "main")
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("5m"), "expect.yaml": strings.Replace(none, "week: 0", "week: 5", 1)})
	rr.host.set(func(h *commentHost) { h.comments = nil })
	if out, err := rr.run([]string{"PATHS=alerts", "EXPECT=expect.yaml", "FAIL_ON_EXPECTATIONS=true"}, "--prometheus", prom.URL, "--tz", "UTC"); err != nil ||
		!strings.Contains(rr.body(), "(was 0, in this change)") {
		t.Errorf("raised: %v\n%s\n%s", err, rr.body(), out)
	}
	// A comment that can't be posted doesn't pass a broken expectation.
	repo.git("checkout", "--quiet", "main")
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("5m")})
	rr.host.set(func(h *commentHost) { h.comments, h.failPost = nil, true })
	if out, err := rr.run([]string{"PATHS=alerts", "EXPECT=expect.yaml", "FAIL_ON_EXPECTATIONS=true"}, "--prometheus", prom.URL, "--tz", "UTC"); err == nil ||
		!strings.Contains(out, "the change breaks 1 expectations") {
		t.Errorf("not posted: %v\n%s", err, out)
	}
	rr.host.set(func(h *commentHost) { h.failPost = false })
	// Taken away in the pull request: as if it held none, what it held is
	// shown taken away, failing nothing, as a budget raised doesn't.
	repo.git("checkout", "--quiet", "main")
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("5m"), "expect.yaml": ""})
	rr.host.set(func(h *commentHost) { h.comments = nil })
	if out, err := rr.run([]string{"PATHS=alerts", "EXPECT=expect.yaml", "FAIL_ON_EXPECTATIONS=true"}, "--prometheus", prom.URL, "--tz", "UTC"); err != nil ||
		!strings.Contains(rr.body(), "(taken away in this change)") || !strings.Contains(rr.body(), "taken away; the change would break it") {
		t.Errorf("taken away: %v\n%s\n%s", err, rr.body(), out)
	}
	// Unreadable as YAML, held to it: not judged, and the job fails.
	repo.git("checkout", "--quiet", "main")
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("5m"), "expect.yaml": "expectations: [\n"})
	rr.host.set(func(h *commentHost) { h.comments = nil })
	if out, err := rr.run([]string{"PATHS=alerts", "EXPECT=expect.yaml", "FAIL_ON_EXPECTATIONS=true"}, "--prometheus", prom.URL); err == nil ||
		!strings.Contains(out, "the expectations couldn't be judged") {
		t.Errorf("unreadable, held to it: %v\n%s", err, out)
	}
	// The history out of reach: no rule that differs replayed, so none held:
	// not known isn't met, and the job fails. Not held to them, it passes.
	repo.git("checkout", "--quiet", "main")
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("5m")})
	rr.host.set(func(h *commentHost) { h.comments = nil })
	if out, err := rr.run([]string{"PATHS=alerts", "EXPECT=expect.yaml", "FAIL_ON_EXPECTATIONS=true"}, "--prometheus", "http://127.0.0.1:1"); err == nil ||
		!strings.Contains(out, "couldn't be replayed, so their expectations can't be judged") {
		t.Errorf("out of reach, held to it: %v\n%s", err, out)
	}
	rr.host.set(func(h *commentHost) { h.comments = nil })
	if out, err := rr.run([]string{"PATHS=alerts", "EXPECT=expect.yaml"}, "--prometheus", "http://127.0.0.1:1"); err != nil {
		t.Errorf("out of reach, not held: %v\n%s", err, out)
	}
	// In neither commit: said.
	repo.git("checkout", "--quiet", "main")
	repo.git("rm", "--quiet", "expect.yaml")
	repo.git("commit", "--quiet", "-m", "no expectations")
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("5m")})
	rr.host.set(func(h *commentHost) { h.comments = nil })
	rr.run([]string{"PATHS=alerts", "EXPECT=expect.yaml"}, "--prometheus", prom.URL)
	failure(t, rr.body(), "EXPECT expect.yaml is in neither the pull request's commit nor the commit it goes into")

	// Wrong settings fail at once.
	for _, c := range []struct {
		settings []string
		args     []string
		said     string
	}{
		{[]string{"PATHS=alerts", "FAIL_ON_EXPECTATIONS=true"}, nil, "FAIL_ON_EXPECTATIONS and EXPECT_REQUIRED hold the rules to EXPECT"},
		{[]string{"PATHS=alerts", "EXPECT=../expect.yaml"}, nil, "EXPECT ../expect.yaml: want a file in the repository"},
		{[]string{"PATHS=alerts", "EXPECT=alerts/"}, nil, "EXPECT alerts/: want a file in the repository"},
		{[]string{"PATHS=alerts", "EXPECT=expect.yaml", "FAIL_ON_EXPECTATIONS=yes"}, nil, "FAIL_ON_EXPECTATIONS yes: want true or false"},
		{[]string{"PATHS=alerts"}, []string{"--expect", "x.yaml"}, "--expect: the recipe reads the expectations from both commits itself"},
	} {
		out, err := rr.run(c.settings, append([]string{"--prometheus", prom.URL}, c.args...)...)
		if err == nil || !strings.Contains(out, c.said) {
			t.Errorf("%v %v: %v\n%s", c.settings, c.args, err, out)
		}
	}
}

// argsGiven is what a recipe gave lapilli, as LAPILLI_TEST_ARGS wrote it.
func argsGiven(t *testing.T, file string) []string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("lapilli didn't run: %v", err)
	}
	var args []string
	if err := json.Unmarshal(b, &args); err != nil {
		t.Fatal(err)
	}
	return args
}

// flagValues are the values flag is given among args, in order.
func flagValues(args []string, flag string) []string {
	var vals []string
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			vals = append(vals, args[i+1])
		}
	}
	return vals
}

// hostileImpact is an impact expression holding what a shell would split
// or expand, or a parser take for a flag: a leading -, spaces, a line
// break, both quotes, $, *, and commands, which touch pwned.
func hostileImpact(pwned string) string {
	return `-sum by (service) (rate(http_requests_total{code=~"5..",path!~'/health.*'}[5m])) * 100` + "\n" +
		`  / sum by (service) (rate(http_requests_total[5m])) < -$THRESHOLD $(touch ` + pwned + `) ` + "`touch " + pwned + "`"
}

// checkImpactArgs checks lapilli was given impact as one argument, as it
// is, IMPACT_FOR's 10m, and the labels service and cluster to match on.
func checkImpactArgs(t *testing.T, args []string, impact string) {
	t.Helper()
	if got := flagValues(args, "--impact"); len(got) != 1 || got[0] != impact {
		t.Errorf("--impact %q, want only %q", got, impact)
	}
	if got := flagValues(args, "--impact-for"); !slices.Equal(got, []string{"10m"}) {
		t.Errorf("--impact-for %q", got)
	}
	if got := flagValues(args, "--match"); !slices.Equal(got, []string{"service", "cluster"}) {
		t.Errorf("--match %q, want service then cluster", got)
	}
}

// IMPACT gives the times users were hurt as incidents the rules that
// differ are judged by, and MATCH the labels a firing and an incident must
// agree on, on every host: the expression reaches lapilli as one argument,
// as it is, whatever it holds, and the history as it is written.
func TestBacktestRecipeImpact(t *testing.T) {
	needRecipeTools(t)
	pwned := filepath.Join(t.TempDir(), "pwned")
	impact := hostileImpact(pwned)
	// The history has the expression only as it is written.
	prom := promAnswering(t, map[string]string{"errors > 5": "checkout", impact: "checkout"})
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	argsFile := filepath.Join(t.TempDir(), "args")
	for _, h := range backtestHosts {
		t.Run(h.kind, func(t *testing.T) {
			rr := newRecipeRun(t, h, repo)
			os.Remove(argsFile)
			out, err := rr.run([]string{"PATHS=alerts", "LAPILLI_TEST_ARGS=" + argsFile, "IMPACT=" + impact, "IMPACT_FOR=10m", "MATCH= service , cluster"},
				"--prometheus", prom.URL, "--tz", "UTC")
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			checkImpactArgs(t, argsGiven(t, argsFile), impact)
			// The rule in place, waiting 5m, caught the hour of impact; the
			// pull request's, waiting 2h, misses it.
			if body := rr.body(); !strings.Contains(body, "| `CheckoutErrors` | `page` | 1 → 0 | 1 → 0 | 0 → 0 |") ||
				!regexp.MustCompile("\n\\| `impact-1` `user impact \\(--impact\\)` \\| [0-9-]+ [0-9:]+ \\| `CheckoutErrors` \\+5m \\| no changed rule fired \\|\n").MatchString(body) {
				t.Errorf("the comment:\n%s\n%s", body, out)
			}
		})
	}
	if _, err := os.Stat(pwned); err == nil {
		t.Error("IMPACT ran a command")
	}

	// Users hurt on another service: caught by a firing of checkout's, but
	// for MATCH, which keeps one service's firing from catching another's.
	other := promAnswering(t, map[string]string{"errors > 5": "checkout", "search_errors > 0": "search"})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	if out, err := rr.run([]string{"PATHS=alerts", "IMPACT=search_errors > 0"}, "--prometheus", other.URL); err != nil ||
		!strings.Contains(rr.body(), "| `CheckoutErrors` | `page` | 1 → 0 | 1 → 0 | 0 → 0 |") || !strings.Contains(rr.body(), "#### Incidents caught differently") {
		t.Errorf("without MATCH: %v\n%s\n%s", err, rr.body(), out)
	}
	if out, err := rr.run([]string{"PATHS=alerts", "IMPACT=search_errors > 0", "MATCH=service"}, "--prometheus", other.URL); err != nil ||
		!strings.Contains(rr.body(), "| `CheckoutErrors` | `page` | 1 → 0 | 0 → 0 | 1 → 0 |") || !strings.Contains(rr.body(), "Every incident is caught as it was.") {
		t.Errorf("MATCH=service: %v\n%s\n%s", err, rr.body(), out)
	}
	// --match among the arguments adds to MATCH, and --impact there gives
	// MATCH its incidents.
	os.Remove(argsFile)
	if out, err := rr.run([]string{"PATHS=alerts", "MATCH=service", "LAPILLI_TEST_ARGS=" + argsFile}, "--prometheus", other.URL,
		"--impact", "search_errors > 0", "--match", "cluster"); err != nil {
		t.Errorf("--impact and --match among the arguments: %v\n%s", err, out)
	} else if got := flagValues(argsGiven(t, argsFile), "--match"); !slices.Equal(got, []string{"service", "cluster"}) {
		t.Errorf("--match %q", got)
	}

	// Wrong settings fail at once.
	rr.host.set(func(h *commentHost) { h.comments = nil })
	for _, c := range []struct {
		settings []string
		args     []string
		said     string
	}{
		{[]string{"IMPACT_FOR=10m"}, nil, "IMPACT_FOR is how long IMPACT must last: give IMPACT too"},
		{[]string{"IMPACT=up == 0", "IMPACT_FOR=ten"}, nil, "IMPACT_FOR ten: want a duration"},
		{[]string{"IMPACT=up == 0", "IMPACT_FOR=-5m"}, nil, "IMPACT_FOR -5m: want a duration"},
		{[]string{"IMPACT= \t "}, nil, "IMPACT: want a PromQL expression"},
		{[]string{"MATCH=service"}, nil, "MATCH is what a firing and an incident must agree on: give IMPACT or INCIDENTS too"},
		{[]string{"IMPACT=up == 0", "MATCH=service,,cluster"}, nil, "want label names"},
		{[]string{"IMPACT=up == 0", "MATCH=service,"}, nil, "want label names"},
		{[]string{"IMPACT=up == 0", "MATCH=ser vice"}, nil, "want label names"},
		{[]string{"IMPACT=up == 0", "MATCH=service;touch"}, nil, "want label names"},
		{[]string{"IMPACT=up == 0"}, []string{"--impact", "down == 1"}, "--impact: IMPACT says already when users were hurt: give one of them"},
		{[]string{"IMPACT=up == 0"}, []string{"--impact=down"}, "--impact=down: IMPACT says already"},
		{[]string{"IMPACT=up == 0", "IMPACT_FOR=5m"}, []string{"--impact-for", "10m"}, "--impact-for: IMPACT_FOR says already how long impact must last"},
	} {
		out, err := rr.run(append([]string{"PATHS=alerts"}, c.settings...), append([]string{"--prometheus", other.URL}, c.args...)...)
		if err == nil || !strings.Contains(out, c.said) {
			t.Errorf("%v %v: %v, want it to fail saying %q\n%s", c.settings, c.args, err, c.said, out)
		}
	}
	if len(rr.host.comments) != 0 {
		t.Errorf("commented: %+v", rr.host.comments)
	}
}

// IMPACT_FOR is taken as lapilli's --impact-for takes it, a second or
// more: one the recipe passes, lapilli doesn't refuse, and one lapilli
// would refuse fails at once. Sub-second units and a sign are refused,
// though lapilli takes 1500ms and +5m.
func TestBacktestRecipeImpactFor(t *testing.T) {
	needRecipeTools(t)
	prom := promAnswering(t, map[string]string{"errors > 5": "checkout", "search_errors > 0": "search"})
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	refusedOnly := []string{"1500ms", "+5m"}
	for _, v := range []string{"10m", "1.5h", "1d", "1d12h", "90s", "1s", "0d5m", "5.h", ".5h", "1m30s",
		"5", "0", "10ms", "0.5s", "0m", "07d", "1.5d", "1d1d", "12h1d", "-5m", "5x", "m", "1500ms", "+5m"} {
		d, err := parseDuration(v)
		takes := err == nil && d >= time.Second
		rr.host.set(func(h *commentHost) { h.comments = nil })
		out, rerr := rr.run([]string{"PATHS=alerts", "IMPACT=search_errors > 0", "IMPACT_FOR=" + v}, "--prometheus", prom.URL)
		refused := rerr != nil && strings.Contains(out, "IMPACT_FOR "+v+": want a duration of a second or more")
		switch {
		case slices.Contains(refusedOnly, v):
			if !takes || !refused {
				t.Errorf("%s: lapilli takes it %v, the recipe refuses it %v\n%s", v, takes, refused, out)
			}
		case takes:
			if rerr != nil || len(rr.host.mine()) != 1 || strings.Contains(rr.body(), "### Lapilli backtest failed") {
				t.Errorf("%s, which lapilli takes: %v\n%s", v, rerr, out)
			}
		case !refused:
			t.Errorf("%s, which lapilli refuses, passed: %v\n%s", v, rerr, out)
		}
	}
}

// The action's impact, impact-for and match reach lapilli as IMPACT,
// IMPACT_FOR and MATCH do: the expression one argument, as it is, which
// args, split at spaces, couldn't give.
func TestBacktestActionImpact(t *testing.T) {
	needRecipeTools(t)
	pwned := filepath.Join(t.TempDir(), "pwned")
	impact := hostileImpact(pwned)
	prom := promAnswering(t, map[string]string{"errors > 5": "checkout", impact: "checkout"})
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	argsFile := filepath.Join(t.TempDir(), "args")
	out, err := runAction(t, rr, map[string]string{"paths": "alerts", "prometheus-url": prom.URL, "args": "--tz UTC",
		"impact": impact, "impact-for": "10m", "match": "service,cluster"}, "LAPILLI_TEST_ARGS="+argsFile)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	checkImpactArgs(t, argsGiven(t, argsFile), impact)
	if body := rr.body(); !strings.Contains(body, "#### Incidents caught differently") {
		t.Errorf("the comment:\n%s\n%s", body, out)
	}
	if _, err := os.Stat(pwned); err == nil {
		t.Error("impact ran a command")
	}
}

// Without version, the action installs the release it is at: the first
// release its own CHANGELOG.md names, read offline, so that moving the uses
// line moves the binary. A sha256 pinned is one release's: once the uses
// line moves on, nothing is installed, and the comment says so.
func TestBacktestActionVersion(t *testing.T) {
	needRecipeTools(t)
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("releases are for Linux and macOS")
	}
	// This repository's own: the release its CHANGELOG.md names first.
	b, err := os.ReadFile("../../CHANGELOG.md")
	if err != nil {
		t.Fatal(err)
	}
	first := regexp.MustCompile(`(?m)^## \[([0-9]+\.[0-9]+\.[0-9]+)\]`).FindSubmatch(b)
	if first == nil {
		t.Fatal("CHANGELOG.md names no release")
	}
	prom := promStandIn(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	inputs := map[string]string{"paths": "alerts", "prometheus-url": prom.URL}
	out, err := runAction(t, rr, inputs)
	if err != nil || !strings.Contains(out, "action.yml: Lapilli v"+string(first[1])+", the release this action is at\n") {
		t.Errorf("this repository's: %v\n%s", err, out)
	}

	// Releases v9.9.8 and v9.9.9: this test binary, as lapilli, saying it
	// ran.
	self := must(os.Executable())
	ran := filepath.Join(t.TempDir(), "ran")
	files, sums := map[string]string{}, map[string]string{}
	for _, v := range []string{"v9.9.8", "v9.9.9"} {
		name := "lapilli-" + v + "-" + runtime.GOOS + "-" + runtime.GOARCH
		script := fmt.Sprintf("#!/bin/sh\ntouch %q\nLAPILLI_TEST_AS_CLI=1 exec %q \"$@\"\n", ran, self)
		var archive strings.Builder
		gz := gzip.NewWriter(&archive)
		tw := tar.NewWriter(gz)
		tw.WriteHeader(&tar.Header{Name: name + "/lapilli", Mode: 0o755, Size: int64(len(script)), Typeflag: tar.TypeReg})
		io.WriteString(tw, script)
		tw.Close()
		gz.Close()
		sum := sha256.Sum256([]byte(archive.String()))
		sums[v] = hex.EncodeToString(sum[:])
		files["/"+v+"/"+name+".tar.gz"] = archive.String()
		files["/"+v+"/SHA256SUMS"] = sums[v] + "  " + name + ".tar.gz\n"
	}
	releases := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, f)
	}))
	defer releases.Close()

	// The action's files as GitHub fetches them for the uses line: the
	// repository's tree at a commit.
	tree := t.TempDir()
	dir := filepath.Join(tree, "recipes", "backtest")
	os.MkdirAll(dir, 0o755)
	for _, f := range []string{"action.yml", "lib.sh", "github.sh"} {
		os.WriteFile(filepath.Join(dir, f), must(os.ReadFile(backtestRecipes+f)), 0o644)
	}
	at := func(gomod, changelog string) {
		os.WriteFile(filepath.Join(tree, "go.mod"), []byte(gomod), 0o644)
		os.WriteFile(filepath.Join(tree, "CHANGELOG.md"), []byte(changelog), 0o644)
	}
	const lapilliMod = "module lapilli.dev\n\ngo 1.26\n"
	env := []string{"GITHUB_ACTION_PATH=" + dir, "LAPILLI=", "LAPILLI_RELEASES=" + releases.URL}
	with := func(more ...string) map[string]string {
		in := maps.Clone(inputs)
		for i := 0; i+1 < len(more); i += 2 {
			in[more[i]] = more[i+1]
		}
		return in
	}
	installs := func(what, version string, out string, err error) {
		t.Helper()
		_, rerr := os.Stat(ran)
		if err != nil || rerr != nil || !strings.Contains(out, "installed Lapilli "+version+" (") || !strings.Contains(rr.body(), "`CheckoutErrors`") {
			t.Errorf("%s: %v, ran: %v\n%s\n%s", what, err, rerr, rr.body(), out)
		}
		os.Remove(ran)
	}

	// A commit between two releases: the one before it.
	at(lapilliMod, "# Changelog\n\n## [Unreleased]\n\n### Added\n\n- **Something.** See [the notes](docs/x.md).\n\n## [9.9.8] - 2026-10-01\n\n## [9.9.7] - 2026-09-01\n")
	out, err = runAction(t, rr, with("sha256", sums["v9.9.8"]), env...)
	if !strings.Contains(out, "action.yml: Lapilli v9.9.8, the release this action is at\n") {
		t.Errorf("between releases:\n%s", out)
	}
	installs("between releases", "v9.9.8", out, err)

	// The uses line moved to v9.9.9's commit, the sha256 left as it was:
	// nothing is installed, the comment says why, and the job fails when
	// asked to.
	at(lapilliMod, "# Changelog\n\n## [Unreleased]\n\n## [9.9.9] - 2026-11-01\n\n## [9.9.8] - 2026-10-01\n")
	for _, fail := range []string{"false", "true"} {
		out, err = runAction(t, rr, with("sha256", sums["v9.9.8"], "fail-on-error", fail), env...)
		if _, rerr := os.Stat(ran); rerr == nil || (err != nil) != (fail == "true") ||
			!strings.Contains(rr.body(), "LAPILLI_SHA256 isn't the sum Lapilli v9.9.9's SHA256SUMS gives lapilli-v9.9.9-"+runtime.GOOS+"-"+runtime.GOARCH+".tar.gz ("+sums["v9.9.9"]+"): not run") {
			t.Errorf("a sha256 pinned before, fail-on-error %s: %v\n%s\n%s", fail, err, rr.body(), out)
		}
	}
	// Changed with it: installed.
	out, err = runAction(t, rr, with("sha256", sums["v9.9.9"]), env...)
	installs("the sha256 changed too", "v9.9.9", out, err)
	// A version given is installed, saying it isn't the action's.
	out, err = runAction(t, rr, with("version", "v9.9.8", "sha256", sums["v9.9.8"]), env...)
	if !strings.Contains(out, "action.yml: version isn't v9.9.9, the release this action is at") {
		t.Errorf("a version given:\n%s", out)
	}
	installs("a version given", "v9.9.8", out, err)

	// Files that aren't Lapilli's, a copy of the action in another
	// repository say, or a CHANGELOG.md with no release: version is needed.
	rr.host.set(func(h *commentHost) { h.comments = nil })
	for what, c := range map[string][2]string{
		"another module": {"module example.com/monitoring\n", "## [Unreleased]\n\n## [2.3.0] - 2026-11-01\n"},
		"no go.mod":      {"", "## [9.9.9] - 2026-11-01\n"},
		"no release":     {lapilliMod, "# Changelog\n\n## [Unreleased]\n\n## [9.9.9-rc.1]\n"},
	} {
		at(c[0], c[1])
		out, err := runAction(t, rr, with("sha256", sums["v9.9.9"]), env...)
		if err == nil || !strings.Contains(out, "action.yml: give version, the Lapilli release to install") {
			t.Errorf("%s: %v\n%s", what, err, out)
		}
	}
	if _, err := os.Stat(ran); err == nil || len(rr.host.comments) != 0 {
		t.Errorf("ran %v, commented %+v", err, rr.host.comments)
	}
}

// The release workflow refuses a version CHANGELOG.md doesn't name first:
// the action installs the release it is at as CHANGELOG.md says.
func TestReleaseChangelogFirst(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	b, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatal(err)
	}
	var run string
	for _, s := range wf.Jobs["build"].Steps {
		if s.Name == "Release notes from CHANGELOG.md" {
			run = s.Run
		}
	}
	if run == "" {
		t.Fatal("no step Release notes from CHANGELOG.md in build")
	}
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "dist"), 0o755)
	os.WriteFile(filepath.Join(dir, "CHANGELOG.md"), []byte("# Changelog\n\n## [Unreleased]\n\n## [9.9.9] - 2026-11-01\n\nNew.\n\n## [9.9.8] - 2026-10-01\n\nOld.\n"), 0o644)
	for version, want := range map[string]string{"v9.9.9": "", "v9.9.8": "CHANGELOG.md names [9.9.9] first, not [9.9.8]", "v9.9.6": "no section [9.9.6]"} {
		cmd := exec.Command("bash", "-c", run)
		cmd.Dir, cmd.Env = dir, append(os.Environ(), "VERSION="+version)
		out, err := cmd.CombinedOutput()
		if (err == nil) != (want == "") || !strings.Contains(string(out), want) {
			t.Errorf("%s: %v\n%s", version, err, out)
		}
	}
}
