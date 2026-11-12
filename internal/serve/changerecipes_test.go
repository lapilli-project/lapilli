package serve

import (
	"context"
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
	"strings"
	"sync"
	"testing"

	"lapilli.dev/internal/ledger"
)

// The change recipes (recipes/changes/), each against a stand-in for its
// host's API, a local git repository, and lapilli serve itself.

const changeRecipes = "../../recipes/changes/"

// gitEnv is git with none of this machine's own settings: no signing, no
// hooks, no credentials.
func gitEnv(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	empty := filepath.Join(home, "gitconfig")
	os.WriteFile(empty, nil, 0o600)
	return append(os.Environ(), "HOME="+home, "GIT_CONFIG_GLOBAL="+empty, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
}

func gitIn(t *testing.T, env []string, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = dir, env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// remote is a bare repository holding files, on main.
func remote(t *testing.T, env []string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	bare := filepath.Join(dir, "remote.git")
	gitIn(t, env, dir, "init", "--quiet", "--bare", "--initial-branch=main", bare)
	work := filepath.Join(dir, "work")
	gitIn(t, env, dir, "clone", "--quiet", bare, work)
	gitIn(t, env, work, "checkout", "--quiet", "-b", "main")
	for name, body := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o755)
		os.WriteFile(filepath.Join(work, name), []byte(body), 0o644)
	}
	gitIn(t, env, work, "add", ".")
	gitIn(t, env, work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--quiet", "-m", "rules")
	gitIn(t, env, work, "push", "--quiet", "origin", "HEAD:main")
	return bare
}

// prHost is a stand-in for one host's pull request API.
type prHost struct {
	t      *testing.T
	kind   string
	url    string
	auth   string
	mu     sync.Mutex
	opened []map[string]any
	merged bool
	// listed are the open pull requests listed, as the host gives them;
	// the list's filters are left to the recipe to apply again.
	listed []any
	// anonymous says the token can't say whose it is.
	anonymous bool
	// exists says one is open from the branch into the base already: a new
	// one is refused, as each host refuses it.
	exists bool
}

// prExists is each host's refusal of a pull request from a branch with
// one open into the same base already.
var prExists = map[string]struct {
	code int
	body string
}{
	"github":    {http.StatusUnprocessableEntity, `{"message":"Validation Failed","errors":[{"resource":"PullRequest","code":"custom","message":"A pull request already exists for o:lapilli/proposal-1."}]}`},
	"gitea":     {http.StatusConflict, `{"message":"pull request already exists for these targets [id: 8, issue_id: 8, head_repo_id: 1, base_repo_id: 1, head_branch: lapilli/proposal-1, base_branch: main]"}`},
	"gitlab":    {http.StatusConflict, `{"message":["Another open merge request already exists for this source branch: !8"]}`},
	"bitbucket": {http.StatusBadRequest, `{"type":"error","error":{"message":"There is already an open pull request from lapilli/proposal-1 to main"}}`},
	"azure":     {http.StatusConflict, `{"message":"TF401179: An active pull request for the source and target branch already exists.","typeKey":"GitPullRequestExistsException"}`},
}

// The account the stand-ins' token is, as each host names it.
const (
	botLogin = "lapilli-bot"
	botID    = 42
	botUUID  = "{0b0b0b0b-0000-4000-8000-000000000042}"
	botAzure = "6c1f3b52-0000-4000-8000-000000000042"
)

// prListed is an open pull request from branch, as a host lists it: the
// recipe's own, or one from a fork, into another branch, or opened by
// someone else.
func prListed(kind, url string, n int, branch, which string) any {
	repo, base, mine := "o/r", "main", true
	switch which {
	case "fork":
		repo = "mallory/r"
	case "another base":
		base = "release"
	case "someone else":
		mine = false
	}
	link := fmt.Sprintf("%s/o/r/pull/%d", url, n)
	switch kind {
	case "github", "gitea":
		login := map[bool]string{true: botLogin, false: "mallory"}[mine]
		return map[string]any{"number": n, "html_url": link, "head": map[string]any{"ref": branch, "repo": map[string]any{"full_name": repo}},
			"base": map[string]any{"ref": base}, "user": map[string]any{"login": login}}
	case "gitlab":
		source := map[bool]int{true: 1, false: 9}[repo == "o/r"]
		return map[string]any{"iid": n, "web_url": link, "source_branch": branch, "target_branch": base, "source_project_id": source,
			"target_project_id": 1, "author": map[string]any{"id": map[bool]int{true: botID, false: 7}[mine]}}
	case "bitbucket":
		if repo == "o/r" {
			repo = "ws/repo"
		}
		return map[string]any{"id": n, "links": map[string]any{"html": map[string]any{"href": link}},
			"source":      map[string]any{"branch": map[string]any{"name": branch}, "repository": map[string]any{"full_name": repo}},
			"destination": map[string]any{"branch": map[string]any{"name": base}}, "author": map[string]any{"uuid": map[bool]string{true: botUUID, false: "{mallory}"}[mine]}}
	default: // azure
		pr := map[string]any{"pullRequestId": n, "sourceRefName": "refs/heads/" + branch, "targetRefName": "refs/heads/" + base,
			"createdBy": map[string]any{"id": map[bool]string{true: botAzure, false: "mallory"}[mine]}}
		if repo != "o/r" {
			pr["forkSource"] = map[string]any{"name": "refs/heads/" + branch, "repository": map[string]any{"name": "r"}}
		}
		return pr
	}
}

func (h *prHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	var list, one, create string
	switch h.kind {
	case "github", "gitea":
		list, one, create = "/repos/o/r/pulls", "/repos/o/r/pulls/7", "/repos/o/r/pulls"
	case "gitlab":
		list, one, create = "/projects/o%2Fr/merge_requests", "/projects/o%2Fr/merge_requests/7", "/projects/o%2Fr/merge_requests"
	case "bitbucket":
		list, one, create = "/repositories/ws/repo/pullrequests", "/repositories/ws/repo/pullrequests/7", "/repositories/ws/repo/pullrequests"
	case "azure":
		list, one, create = "/org/my%20project/_apis/git/repositories/rules/pullrequests", "/org/my%20project/_apis/git/repositories/rules/pullrequests/7",
			"/org/my%20project/_apis/git/repositories/rules/pullrequests"
	}
	me := map[string]string{"github": "/user", "gitea": "/user", "gitlab": "/user", "bitbucket": "/user", "azure": "/org/_apis/connectionData"}[h.kind]
	switch {
	case r.Method == http.MethodGet && path == me:
		if h.anonymous {
			http.Error(w, "Resource not accessible by integration", http.StatusForbidden)
			return
		}
		switch h.kind {
		case "github", "gitea":
			reply(map[string]any{"login": botLogin})
		case "gitlab":
			reply(map[string]any{"id": botID, "username": "project_1_bot"})
		case "bitbucket":
			reply(map[string]any{"uuid": botUUID})
		case "azure":
			reply(map[string]any{"authenticatedUser": map[string]any{"id": botAzure}})
		}
	case r.Method == http.MethodPost && path == create && h.exists:
		e := prExists[h.kind]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(e.code)
		io.WriteString(w, e.body)
	case r.Method == http.MethodPost && path == create:
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		h.opened = append(h.opened, in)
		switch h.kind {
		case "github", "gitea":
			reply(map[string]any{"number": 7, "html_url": h.url + "/o/r/pull/7"})
		case "gitlab":
			reply(map[string]any{"iid": 7, "web_url": h.url + "/o/r/-/merge_requests/7"})
		case "bitbucket":
			reply(map[string]any{"id": 7, "links": map[string]any{"html": map[string]any{"href": h.url + "/ws/repo/pull-requests/7"}}})
		case "azure":
			reply(map[string]any{"pullRequestId": 7, "url": "https://elsewhere.example/api"})
		}
	case r.Method == http.MethodGet && path == list:
		listed := h.listed
		if listed == nil {
			listed = []any{}
		}
		switch h.kind {
		case "bitbucket":
			reply(map[string]any{"values": listed})
		case "azure":
			reply(map[string]any{"value": listed})
		default:
			reply(listed)
		}
	case r.Method == http.MethodGet && (path == one || strings.TrimLeft(strings.TrimPrefix(path, strings.TrimSuffix(one, "7")), "0123456789") == "" &&
		strings.HasPrefix(path, strings.TrimSuffix(one, "7"))):
		switch h.kind {
		case "github", "gitea":
			reply(map[string]any{"merged": h.merged, "state": map[bool]string{true: "closed", false: "open"}[h.merged]})
		case "gitlab":
			reply(map[string]any{"state": map[bool]string{true: "merged", false: "opened"}[h.merged]})
		case "bitbucket":
			reply(map[string]any{"state": map[bool]string{true: "MERGED", false: "OPEN"}[h.merged]})
		case "azure":
			reply(map[string]any{"status": map[bool]string{true: "completed", false: "active"}[h.merged]})
		}
	default:
		h.t.Errorf("%s: %s %s", h.kind, r.Method, r.URL)
		http.NotFound(w, r)
	}
}

// recipeHost is a change recipe, and what it is run with.
type recipeHost struct {
	kind, script, auth string
	env                func(url, tok string) []string
	link               func(url string) string
}

var recipeHosts = []recipeHost{
	{"github", "github.sh", "Bearer host-token-0123456789",
		func(u, tok string) []string {
			return []string{"GITHUB_API=" + u, "GITHUB_REPO=o/r", "GITHUB_TOKEN_FILE=" + tok}
		},
		func(u string) string { return u + "/o/r/pull/7" }},
	{"gitea", "gitea.sh", "token host-token-0123456789",
		func(u, tok string) []string {
			return []string{"GITEA_API=" + u, "GITEA_REPO=o/r", "GITEA_TOKEN_FILE=" + tok}
		},
		func(u string) string { return u + "/o/r/pull/7" }},
	{"gitlab", "gitlab.sh", "host-token-0123456789",
		func(u, tok string) []string {
			return []string{"GITLAB_API=" + u, "GITLAB_PROJECT=o/r", "GITLAB_TOKEN_FILE=" + tok}
		},
		func(u string) string { return u + "/o/r/-/merge_requests/7" }},
	{"bitbucket", "bitbucket.sh", "Bearer host-token-0123456789",
		func(u, tok string) []string {
			return []string{"BITBUCKET_API=" + u, "BITBUCKET_REPO=ws/repo", "BITBUCKET_TOKEN_FILE=" + tok}
		},
		func(u string) string { return u + "/ws/repo/pull-requests/7" }},
	{"azure", "azure-devops.sh", "Basic " + base64.StdEncoding.EncodeToString([]byte(":host-token-0123456789")),
		func(u, tok string) []string {
			return []string{"AZURE_DEVOPS_ORG_URL=" + u + "/org", "AZURE_DEVOPS_PROJECT=my project", "AZURE_DEVOPS_REPO=rules", "AZURE_DEVOPS_TOKEN_FILE=" + tok}
		},
		func(u string) string { return u + "/org/my%20project/_git/rules/pullrequest/7" }},
}

func TestChangeRecipes(t *testing.T) {
	needTools(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	for _, c := range recipeHosts {
		t.Run(c.kind, func(t *testing.T) {
			ctx := context.Background()
			s, _ := changesServer(t, nil)
			h := s.Handler()
			id := proposeFor(t, h, "15m")
			approve(t, h, id, ana)
			s.jobs(ctx, false)
			lapilli := httptest.NewServer(s.IncidentsHandler())
			defer lapilli.Close()
			host := &prHost{t: t, kind: c.kind, auth: c.auth}
			hs := httptest.NewServer(host)
			defer hs.Close()
			host.url = hs.URL
			env := gitEnv(t)
			bare := remote(t, env, map[string]string{"alerts/shop.yaml": ruleFile, "README.md": "rules\n"})
			run := func() string {
				cmd := exec.Command("sh", changeRecipes+c.script)
				cmd.Env = append(append(env, "LAPILLI_URL="+lapilli.URL, "LAPILLI_TOKEN_FILE="+tokenFile(t, changesToken), "CLONE="+bare, "RULES_PATHS=alerts"),
					c.env(hs.URL, tokenFile(t, "host-token-0123456789"))...)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("%s: %v\n%s", c.script, err, out)
				}
				return string(out)
			}
			run()
			p := state(t, s, id)
			if p.State != ledger.StateOpened || p.Link != c.link(hs.URL) {
				t.Fatalf("opened: %s %q, want %q", p.State, p.Link, c.link(hs.URL))
			}
			if len(host.opened) != 1 {
				t.Fatalf("pull requests opened: %v", host.opened)
			}
			pr, _ := json.Marshal(host.opened[0])
			for _, want := range []string{"lapilli/" + s.ledgerID + "/proposal-" + itoa(id), "Lapilli: a new for, Errors", "Backtest from"} {
				if !strings.Contains(string(pr), want) {
					t.Errorf("the pull request has no %q: %s", want, pr)
				}
			}
			if got := gitIn(t, env, bare, "show", "lapilli/"+s.ledgerID+"/proposal-"+itoa(id)+":alerts/shop.yaml"); got != p.Document {
				t.Errorf("the branch's file:\n%s\nwant:\n%s", got, p.Document)
			}
			if got := gitIn(t, env, bare, "show", "main:alerts/shop.yaml"); got != ruleFile {
				t.Errorf("main changed:\n%s", got)
			}
			// Run again: open still; merged, Lapilli is told.
			run()
			if p := state(t, s, id); p.State != ledger.StateOpened || len(host.opened) != 1 {
				t.Errorf("run again: %s, %d pull requests", p.State, len(host.opened))
			}
			host.merged = true
			run()
			if p := state(t, s, id); p.State != ledger.StateMerged {
				t.Errorf("merged: %s", p.State)
			}
		})
	}
}

// A document no file in the clone is: Lapilli is told the change failed,
// and why; nothing is pushed.
func TestChangeRecipeNoFile(t *testing.T) {
	needTools(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	ctx := context.Background()
	s, _ := changesServer(t, nil)
	h := s.Handler()
	id := proposeFor(t, h, "15m")
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	lapilli := httptest.NewServer(s.IncidentsHandler())
	defer lapilli.Close()
	hs := httptest.NewServer(&prHost{t: t, kind: "github", auth: "Bearer host-token-0123456789"})
	defer hs.Close()
	env := gitEnv(t)
	// Rendered from a chart: the file in git isn't what Lapilli read.
	bare := remote(t, env, map[string]string{"charts/rules/templates/shop.yaml": "{{ .Values.rules | toYaml }}\n"})
	cmd := exec.Command("sh", changeRecipes+"github.sh")
	cmd.Env = append(env, "LAPILLI_URL="+lapilli.URL, "LAPILLI_TOKEN_FILE="+tokenFile(t, changesToken), "CLONE="+bare, "RULES_PATHS=charts/rules",
		"GITHUB_API="+hs.URL, "GITHUB_REPO=o/r", "GITHUB_TOKEN_FILE="+tokenFile(t, "host-token-0123456789"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if p := state(t, s, id); p.State != ledger.StateFailed || !strings.Contains(p.Reason, "no file in the repository") {
		t.Errorf("no file: %+v", p)
	}
	if out := gitIn(t, env, bare, "branch", "--list", "lapilli/*"); strings.TrimSpace(out) != "" {
		t.Errorf("pushed: %s", out)
	}
	// Credentials in CLONE, where ps would show them, are refused.
	cmd = exec.Command("sh", changeRecipes+"github.sh")
	cmd.Env = append(env, "LAPILLI_URL="+lapilli.URL, "LAPILLI_TOKEN_FILE="+tokenFile(t, changesToken), "CLONE=https://x:secret@git.example/o/r.git", "RULES_PATHS=alerts",
		"GITHUB_REPO=o/r", "GITHUB_TOKEN_FILE="+tokenFile(t, "host-token-0123456789"))
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "credentials") {
		t.Errorf("credentials in CLONE: %v %s", err, out)
	}
}

// A recipe takes as its own only a pull request from the repository
// itself, into BASE, opened by the account its token is: one from a fork
// with Lapilli's branch name, into another branch, or opened by someone
// else takes over nothing; and none, when the host can't say who the token
// is.
func TestChangeRecipeTakesOnlyItsOwn(t *testing.T) {
	needTools(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	for _, c := range recipeHosts {
		for _, which := range []string{"others", "its own", "no one"} {
			t.Run(c.kind+", "+which, func(t *testing.T) {
				ctx := context.Background()
				s, _ := changesServer(t, nil)
				h := s.Handler()
				id := proposeFor(t, h, "15m")
				approve(t, h, id, ana)
				s.jobs(ctx, false)
				lapilli := httptest.NewServer(s.IncidentsHandler())
				defer lapilli.Close()
				host := &prHost{t: t, kind: c.kind, auth: c.auth}
				hs := httptest.NewServer(host)
				defer hs.Close()
				host.url = hs.URL
				branch := "lapilli/" + s.ledgerID + "/proposal-" + itoa(id)
				host.listed = []any{prListed(c.kind, hs.URL, 99, branch, "fork"), prListed(c.kind, hs.URL, 98, branch, "another base"),
					prListed(c.kind, hs.URL, 97, branch, "someone else")}
				want := c.link(hs.URL)
				switch which {
				case "its own":
					host.listed = append(host.listed, prListed(c.kind, hs.URL, 8, branch, "its own"))
					want = hs.URL + "/o/r/pull/8"
					if c.kind == "azure" {
						want = hs.URL + "/org/my%20project/_git/rules/pullrequest/8"
					}
				case "no one":
					host.listed = append(host.listed, prListed(c.kind, hs.URL, 8, branch, "its own"))
					host.anonymous = true
				}
				env := gitEnv(t)
				bare := remote(t, env, map[string]string{"alerts/shop.yaml": ruleFile})
				cmd := exec.Command("sh", changeRecipes+c.script)
				cmd.Env = append(append(env, "LAPILLI_URL="+lapilli.URL, "LAPILLI_TOKEN_FILE="+tokenFile(t, changesToken), "CLONE="+bare, "RULES_PATHS=alerts"),
					c.env(hs.URL, tokenFile(t, "host-token-0123456789"))...)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("%s: %v\n%s", c.script, err, out)
				}
				p := state(t, s, id)
				created := map[string]int{"others": 1, "its own": 0, "no one": 1}[which]
				if p.State != ledger.StateOpened || p.Link != want || len(host.opened) != created {
					t.Errorf("%s: %s %q, want %q; %d opened, want %d", which, p.State, p.Link, want, len(host.opened), created)
				}
			})
		}
	}
}

// The host refuses another pull request from Lapilli's branch, as one is
// open from it already: a run that stopped after opening it, with a token
// the host can't say whose it is, or one someone else opened. The one open
// from the branch in the repository itself, into BASE, is the change's,
// whoever opened it; one from a fork, or into another branch, never is.
func TestChangeRecipeOpenAlready(t *testing.T) {
	needTools(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	for _, c := range recipeHosts {
		for _, anonymous := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, anonymous %v", c.kind, anonymous), func(t *testing.T) {
				ctx := context.Background()
				s, _ := changesServer(t, nil)
				h := s.Handler()
				id := proposeFor(t, h, "15m")
				approve(t, h, id, ana)
				s.jobs(ctx, false)
				lapilli := httptest.NewServer(s.IncidentsHandler())
				defer lapilli.Close()
				host := &prHost{t: t, kind: c.kind, auth: c.auth, anonymous: anonymous, exists: true}
				hs := httptest.NewServer(host)
				defer hs.Close()
				host.url = hs.URL
				branch := "lapilli/" + s.ledgerID + "/proposal-" + itoa(id)
				// Its own, from the run that stopped; or someone else's.
				n, which := 8, "its own"
				if !anonymous {
					n, which = 97, "someone else"
				}
				host.listed = []any{prListed(c.kind, hs.URL, 99, branch, "fork"), prListed(c.kind, hs.URL, 98, branch, "another base"),
					prListed(c.kind, hs.URL, n, branch, which)}
				want := fmt.Sprintf("%s/o/r/pull/%d", hs.URL, n)
				if c.kind == "azure" {
					want = fmt.Sprintf("%s/org/my%%20project/_git/rules/pullrequest/%d", hs.URL, n)
				}
				env := gitEnv(t)
				bare := remote(t, env, map[string]string{"alerts/shop.yaml": ruleFile})
				for range 2 {
					cmd := exec.Command("sh", changeRecipes+c.script)
					cmd.Env = append(append(env, "LAPILLI_URL="+lapilli.URL, "LAPILLI_TOKEN_FILE="+tokenFile(t, changesToken), "CLONE="+bare, "RULES_PATHS=alerts"),
						c.env(hs.URL, tokenFile(t, "host-token-0123456789"))...)
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("%s: %v\n%s", c.script, err, out)
					}
				}
				if p := state(t, s, id); p.State != ledger.StateOpened || p.Link != want || len(host.opened) != 0 {
					t.Errorf("%s: %s %q, want %q; %d opened", which, p.State, p.Link, want, len(host.opened))
				}
			})
		}
	}
}

// A change is written only to a file under RULES_PATHS, never to one a CI
// system runs: a Lapilli someone took over, sending a change with the hash
// of a workflow, would otherwise have the recipe push that workflow to the
// repository, which a push runs with its secrets before anyone reviews it.
func TestChangeRecipeOnlyRulesPaths(t *testing.T) {
	needTools(t)
	workflow := "on: [push]\njobs:\n  check:\n    runs-on: ubuntu-latest\n    steps:\n      - run: promtool check rules alerts/*.yaml\n"
	script := "#!/bin/sh\nmake test\n"
	files := map[string]string{"alerts/shop.yaml": ruleFile, ".github/workflows/ci.yml": workflow, "scripts/test.sh": script,
		"alerts/.gitlab-ci.yml": workflow + "# here too\n"}
	hash := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	for _, c := range []struct{ name, paths, doc, refused string }{
		{"a workflow", "alerts", workflow, "under RULES_PATHS"},
		{"a script", "alerts", script, "under RULES_PATHS"},
		{"a workflow under RULES_PATHS", ". .github alerts", workflow, "RULES_PATHS .:"},
		{"a workflow where it is named", ".github alerts", workflow, "RULES_PATHS .github: it is what a CI system runs"},
		{"GitLab's CI under RULES_PATHS", "alerts", workflow + "# here too\n", "is not one a change is written to"},
		{"a path out of the repository", "../elsewhere", ruleFile, "RULES_PATHS ../elsewhere:"},
		{"an absolute path", "/etc", ruleFile, "RULES_PATHS /etc:"},
		{"none", " ", ruleFile, "RULES_PATHS: want"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var told []string
			lapilli := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Header.Get("Authorization") != "Bearer "+changesToken:
					http.Error(w, "no", http.StatusUnauthorized)
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/changes":
					json.NewEncoder(w).Encode(map[string]any{"changes": []map[string]any{{
						"id": 1, "rule": "Errors", "kind": "for", "state": "ready", "source": "/rules/shop.yaml",
						"doc_hash": hash(c.doc), "document": "on: [push]\njobs: {steal: {runs-on: x, steps: [{run: env}]}}\n",
						"title": "Lapilli: a new for, Errors", "body": "Backtest",
					}}})
				case r.Method == http.MethodPatch:
					b, _ := io.ReadAll(r.Body)
					mu.Lock()
					told = append(told, string(b))
					mu.Unlock()
					io.WriteString(w, "{}")
				default:
					http.NotFound(w, r)
				}
			}))
			defer lapilli.Close()
			h := recipeHosts[0]
			host := &prHost{t: t, kind: h.kind, auth: h.auth}
			hs := httptest.NewServer(host)
			defer hs.Close()
			host.url = hs.URL
			env := gitEnv(t)
			bare := remote(t, env, files)
			cmd := exec.Command("sh", changeRecipes+h.script)
			cmd.Env = append(append(env, "LAPILLI_URL="+lapilli.URL, "LAPILLI_TOKEN_FILE="+tokenFile(t, changesToken), "CLONE="+bare,
				"RULES_PATHS="+c.paths), h.env(hs.URL, tokenFile(t, "host-token-0123456789"))...)
			out, _ := cmd.CombinedOutput()
			mu.Lock()
			said := string(out) + strings.Join(told, "")
			mu.Unlock()
			if !strings.Contains(said, c.refused) {
				t.Errorf("want %q said:\n%s", c.refused, said)
			}
			if b := gitIn(t, env, bare, "branch", "--list", "lapilli/*"); strings.TrimSpace(b) != "" || len(host.opened) != 0 {
				t.Errorf("pushed %q, opened %v", b, host.opened)
			}
		})
	}
}
