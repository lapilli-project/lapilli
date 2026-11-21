package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// headOf is, for each host, what its CI says the pull request's head
// commit is, and the job's page, as each script reads them: GitHub's and
// Gitea's event, GitLab's CI_COMMIT_SHA, Bitbucket's BITBUCKET_COMMIT,
// Azure Pipelines' SYSTEM_PULLREQUEST_SOURCECOMMITID.
func headOf(t *testing.T, kind, sha string) (env []string, link string) {
	t.Helper()
	event := filepath.Join(t.TempDir(), "event.json")
	os.WriteFile(event, fmt.Appendf(nil, `{"pull_request": {"number": 7, "head": {"sha": %q}}}`, sha), 0o600)
	switch kind {
	case "github":
		return []string{"GITHUB_EVENT_PATH=" + event, "GITHUB_SERVER_URL=https://github.example.com", "GITHUB_REPOSITORY=o/r", "GITHUB_RUN_ID=99"},
			"https://github.example.com/o/r/actions/runs/99"
	case "gitea":
		return []string{"GITEA_ACTIONS=true", "GITHUB_EVENT_PATH=" + event}, ""
	case "gitlab":
		return []string{"CI_COMMIT_SHA=" + sha, "CI_PIPELINE_ID=314", "CI_JOB_URL=https://gitlab.example.com/o/r/-/jobs/27"},
			"https://gitlab.example.com/o/r/-/jobs/27"
	case "bitbucket":
		return []string{"BITBUCKET_COMMIT=" + sha, "BITBUCKET_BUILD_NUMBER=12", "BITBUCKET_REPO_FULL_NAME=ws/repo"},
			"https://bitbucket.org/ws/repo/pipelines/results/12"
	}
	return []string{"SYSTEM_PULLREQUEST_SOURCECOMMITID=" + sha, "BUILD_BUILDID=55"}, "/org/my%20project/_build/results?buildId=55"
}

// STATUS sets a commit status on the pull request's head commit, as each
// host's API takes one, after the comment: failure when FAIL_MISSED fails
// the job, success when it passes, the description saying why. The
// commit is the one the CI says, the backtest's or a parent of it; with
// none, or another, no status is set, and the job goes on as it would.
func TestBacktestRecipeStatus(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	now := time.Now()
	incidents := fmt.Sprintf("incidents:\n  - id: INC-7\n    title: Checkout down\n    start: %s\n    end: %s\n",
		now.Add(-3*time.Hour).Format(time.RFC3339), now.Add(-2*time.Hour).Format(time.RFC3339))
	for _, h := range backtestHosts {
		t.Run(h.kind, func(t *testing.T) {
			repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m"), "incidents.yaml": incidents})
			repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
			sha := repo.git("rev-parse", "pr")
			rr := newRecipeRun(t, h, repo)
			rr.host.iterations = []string{repo.git("rev-parse", "main"), sha}
			ci, link := headOf(t, h.kind, sha)
			if h.kind == "azure" {
				link = rr.host.url + link
			}
			rr.env = append(rr.env, ci...)
			settings := []string{"PATHS=alerts", "INCIDENTS=incidents.yaml", "STATUS=true"}
			args := []string{"--prometheus", prom.URL, "--tz", "UTC"}

			// CheckoutErrors held 2h no longer catches INC-7: the job fails,
			// once the comment says which, and the status says it too.
			out, err := rr.run(append(settings, "FAIL_MISSED=true"), args...)
			if err == nil || !strings.Contains(out, "the change leaves 1 incidents a rule caught before it caught by no rule (FAIL_MISSED=true)") {
				t.Fatalf("FAIL_MISSED: %v\n%s", err, out)
			}
			if body := rr.body(); !strings.Contains(body, "**This run fails** (`--fail-missed`): 1 incidents") || !strings.Contains(body, "| `INC-7` `Checkout down` |") ||
				strings.Contains(body, "### Lapilli backtest failed") {
				t.Errorf("the comment:\n%s", body)
			}
			want := map[string][2]string{"github": {"success", "failure"}, "gitea": {"success", "failure"}, "gitlab": {"success", "failed"},
				"bitbucket": {"SUCCESSFUL", "FAILED"}, "azure": {"succeeded", "failed"}}[h.kind]
			st, n := rr.host.lastStatus()
			if n != 1 || st.SHA != sha || st.State != want[1] || st.Context != "lapilli/backtest" || st.URL != link ||
				st.Text != "the change leaves 1 incidents a rule caught before it caught by no rule (FAIL_MISSED=true)" {
				t.Errorf("the status: %d, %+v, want on %s, linking to %s", n, st, sha, link)
			}
			switch h.kind {
			case "bitbucket":
				if st.Ref != "pr" {
					t.Errorf("bitbucket: refname %q, not the pull request's source branch", st.Ref)
				}
			case "azure":
				if st.Iteration != 2 {
					t.Errorf("azure: iteration %d, not the one at the commit", st.Iteration)
				}
			case "gitlab":
				if st.Pipeline != 314 {
					t.Errorf("gitlab: pipeline_id %d, not the job's pipeline", st.Pipeline)
				}
			}

			// Not held to it: the job passes, and the status with it.
			if out, err := rr.run(settings, args...); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if st, n := rr.host.lastStatus(); n != 2 || st.State != want[0] || st.Text != "backtested: 1 changed, 0 added, 0 taken away" {
				t.Errorf("passed: %d, %+v", n, st)
			}
			// Another job's (KEY), a status of its own.
			if out, err := rr.run(append(settings, "KEY=staging"), args...); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if st, _ := rr.host.lastStatus(); st.Context != "lapilli/backtest/staging" {
				t.Errorf("KEY=staging: %+v", st)
			}
			// The host refuses it: said, and the job fails only with
			// FAIL_ON_ERROR=true.
			rr.host.set(func(h *commentHost) { h.failStatus = true })
			if out, err := rr.run(settings, args...); err != nil || !strings.Contains(out, "couldn't be set on "+sha) {
				t.Errorf("refused: %v\n%s", err, out)
			}
			if out, err := rr.run(append(settings, "FAIL_ON_ERROR=true"), args...); err == nil {
				t.Errorf("refused, FAIL_ON_ERROR=true: passed\n%s", out)
			}
			rr.host.set(func(h *commentHost) { h.failStatus = false })
			// A commit the backtest didn't read: none set on it.
			_, before := rr.host.lastStatus()
			other := strings.Repeat("ab", 20)
			if out, err := rr.run(append(settings, "STATUS_SHA="+other), args...); err != nil ||
				!strings.Contains(out, other+" isn't the commit the backtest read") {
				t.Errorf("another commit: %v\n%s", err, out)
			}
			if _, n := rr.host.lastStatus(); n != before {
				t.Errorf("another commit: a status set")
			}
		})
	}
}

// Without a commit the CI names, the status goes on HEAD when it is the
// pull request's own commit; a merge it can't tell the parents of is
// refused, and so is Azure DevOps' pull request with no iteration at the
// commit. Nothing changed, the status says so, and no comment is posted.
func TestBacktestRecipeStatusCommit(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	rr := newRecipeRun(t, backtestHosts[1], repo) // gitea, outside Actions
	settings := []string{"PATHS=alerts", "STATUS=true"}
	if out, err := rr.run(settings, "--prometheus", prom.URL); err != nil || !strings.Contains(out, "HEAD is a merge") {
		t.Errorf("a merge: %v\n%s", err, out)
	}
	if _, n := rr.host.lastStatus(); n != 0 {
		t.Errorf("a merge: a status set")
	}
	// The pull request's own commit checked out, as Gitea Actions does.
	repo.git("checkout", "--quiet", "pr")
	sha := repo.git("rev-parse", "HEAD")
	if out, err := rr.run(append(settings, "TARGET=main"), "--prometheus", prom.URL); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if st, n := rr.host.lastStatus(); n != 1 || st.SHA != sha || st.State != "success" {
		t.Errorf("HEAD: %d, %+v", n, st)
	}
	// Nothing changed: no comment, and the status says why.
	repo.git("checkout", "--quiet", "main")
	repo.pullRequest(map[string]string{"README.md": "docs\n"})
	rr.host.set(func(h *commentHost) { h.comments = nil })
	if out, err := rr.run(append(settings, "STATUS_SHA="+repo.git("rev-parse", "pr")), "--prometheus", prom.URL); err != nil ||
		len(rr.host.comments) != 0 {
		t.Errorf("nothing changed: %v, %d comments\n%s", err, len(rr.host.comments), out)
	}
	if st, n := rr.host.lastStatus(); n != 2 || st.Text != "nothing that decides when a rule fires changed" || st.State != "success" {
		t.Errorf("nothing changed: %d, %+v", n, st)
	}

	// Azure DevOps: no iteration at the commit, none set.
	az := newRecipeRun(t, backtestHosts[4], repo)
	az.host.iterations = []string{repo.git("rev-parse", "main")}
	az.env = append(az.env, "SYSTEM_PULLREQUEST_SOURCECOMMITID="+repo.git("rev-parse", "pr"))
	if out, err := az.run(settings, "--prometheus", prom.URL); err != nil || !strings.Contains(out, "no iteration of the pull request is at ") {
		t.Errorf("azure, no iteration: %v\n%s", err, out)
	}
	if _, n := az.host.lastStatus(); n != 0 {
		t.Errorf("azure, no iteration: a status set")
	}
}

// FAIL_MISSED holds a change to the incidents, with INCIDENTS, IMPACT or
// --incidents: a rule left alone that still catches one keeps it caught,
// and a rule that differs not replayed fails the job, as what it catches
// can't be judged. Wrong settings for it, and for STATUS, fail at once.
func TestBacktestRecipeFailMissed(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	now := time.Now()
	incidents := fmt.Sprintf("incidents:\n  - id: INC-7\n    start: %s\n    end: %s\n",
		now.Add(-3*time.Hour).Format(time.RFC3339), now.Add(-2*time.Hour).Format(time.RFC3339))
	also := "      - alert: CheckoutDown\n        expr: errors > 5\n        for: 1m\n"
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m") + also, "incidents.yaml": incidents})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h") + also})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	settings := []string{"PATHS=alerts", "INCIDENTS=incidents.yaml", "FAIL_MISSED=1"}
	if out, err := rr.run(settings, "--prometheus", prom.URL); err != nil ||
		!strings.Contains(rr.body(), "Every incident a rule caught before the change is caught after it too (`--fail-missed`).") {
		t.Errorf("caught by a rule left alone: %v\n%s\n%s", err, rr.body(), out)
	}
	if out, err := rr.run(settings, "--prometheus", "http://127.0.0.1:1"); err == nil ||
		!strings.Contains(out, "couldn't be replayed, so what they catch can't be judged (FAIL_MISSED=true)") {
		t.Errorf("out of reach: %v\n%s", err, out)
	}
	// A lapilli without --fail-missed: said, and the job fails.
	old := filepath.Join(t.TempDir(), "lapilli")
	os.WriteFile(old, []byte("#!/bin/sh\necho 'lapilli verdict: unknown flag --fail-missed' >&2\nexit 2\n"), 0o755)
	if out, err := rr.run(append(settings, "LAPILLI="+old), "--prometheus", prom.URL); err == nil ||
		!strings.Contains(rr.body(), "the lapilli run has no --fail-missed") {
		t.Errorf("an older lapilli: %v\n%s\n%s", err, rr.body(), out)
	}

	// A report sent to Slack too, which fails: lapilli's exit 1 isn't the
	// gate's alone, and the comment says the backtest failed.
	hook := filepath.Join(t.TempDir(), "hook")
	os.WriteFile(hook, []byte("http://127.0.0.1:1/hook\n"), 0o600)
	repo.git("checkout", "--quiet", "main")
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	if out, err := rr.run(settings, "--prometheus", prom.URL, "--slack-webhook-file", hook); err == nil ||
		!strings.Contains(rr.body(), "### Lapilli backtest failed") {
		t.Errorf("a send failed beside the gate: %v\n%s\n%s", err, rr.body(), out)
	}
	// A rule left alone whose query the history refuses: whether INC-7 is
	// missed isn't known, which fails the job held to it.
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("query") == "refused_total > 0" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"status":"error","errorType":"bad_data","error":"refused"}`)
			return
		}
		prom.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(refusing.Close)
	refused := "      - alert: Refused\n        expr: refused_total > 0\n"
	repo.git("checkout", "--quiet", "main")
	repo.write(map[string]string{"alerts/shop.yaml": shopRules("5m") + refused})
	repo.git("commit", "--quiet", "-am", "a rule refused")
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h") + refused})
	if out, err := rr.run(settings, "--prometheus", refusing.URL); err == nil ||
		!strings.Contains(out, "1 incidents a rule caught before the change can't be told caught or missed, as a rule after it couldn't be replayed (FAIL_MISSED=true)") ||
		!strings.Contains(rr.body(), "whether they are missed isn't known") {
		t.Errorf("not known: %v\n%s\n%s", err, rr.body(), out)
	}

	for _, c := range []struct {
		settings []string
		args     []string
		said     string
	}{
		{[]string{"PATHS=alerts", "FAIL_MISSED=true"}, nil, "FAIL_MISSED fails the job when the change leaves an incident caught by no rule: give the incidents, INCIDENTS or IMPACT"},
		{[]string{"PATHS=alerts", "FAIL_MISSED=yes", "INCIDENTS=incidents.yaml"}, nil, "FAIL_MISSED yes: want true or false"},
		{[]string{"PATHS=alerts", "INCIDENTS=incidents.yaml"}, []string{"--fail-missed"}, "--fail-missed: the recipe gives lapilli --fail-missed itself"},
		{[]string{"PATHS=alerts", "FAIL_MISSED=true", "LAPILLI_SERVE=https://lapilli-ci.example.com"}, nil, "FAIL_MISSED: lapilli serve's report doesn't say yet"},
		{[]string{"PATHS=alerts", "STATUS=yes"}, nil, "STATUS yes: want true or false"},
		{[]string{"PATHS=alerts", "STATUS=true", "STATUS_SHA=abc123"}, nil, "STATUS_SHA abc123: want the commit's full hash"},
		{[]string{"PATHS=alerts", "STATUS=true", "STATUS_URL=http://ci.example.com/job/1"}, nil, "STATUS_URL http://ci.example.com/job/1: want an https URL, or http to this machine, of 255 characters at most"},
		{[]string{"PATHS=alerts", "STATUS_SHA=" + strings.Repeat("ab", 20)}, nil, "STATUS_SHA is for the commit status: give STATUS=true too"},
	} {
		out, err := rr.run(c.settings, append([]string{"--prometheus", prom.URL}, c.args...)...)
		if err == nil || !strings.Contains(out, c.said) {
			t.Errorf("%v %v: %v\n%s", c.settings, c.args, err, out)
		}
	}
}

// The action's fail-missed and status reach github.sh as FAIL_MISSED and
// STATUS: the step fails, and the head commit's status says why.
func TestBacktestActionStatus(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	now := time.Now()
	incidents := fmt.Sprintf("incidents:\n  - id: INC-7\n    start: %s\n    end: %s\n",
		now.Add(-3*time.Hour).Format(time.RFC3339), now.Add(-2*time.Hour).Format(time.RFC3339))
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m"), "incidents.yaml": incidents})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	sha := repo.git("rev-parse", "pr")
	rr := newRecipeRun(t, backtestHosts[0], repo)
	ci, _ := headOf(t, "github", sha)
	out, err := runAction(t, rr, map[string]string{"version": "v0.32.0", "paths": "alerts", "incidents": "incidents.yaml", "prometheus-url": prom.URL,
		"fail-missed": "true", "status": "true"}, ci...)
	if st, n := rr.host.lastStatus(); err == nil || n != 1 || st.SHA != sha || st.State != "failure" || !strings.Contains(st.Text, "FAIL_MISSED=true") {
		t.Errorf("%v: %d, %+v\n%s", err, n, st, out)
	}
}

// A checkout of the pull request's own commit, as actions/checkout with
// ref: pull_request.head.sha makes, has TARGET HEAD^1, its default, the
// pull request's own earlier commit: of two commits, the rule change and
// another, nothing changed against it, the gate passed and a status of
// success was set, INCIDENTS read from the pull request. GitHub's branch
// the pull request goes into (GITHUB_BASE_REF) is taken instead now, and,
// with none, a gate or a status is refused. Of a merge, the status goes
// on the pull request's parent alone, never the commit it goes into.
func TestBacktestRecipeOwnParent(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	now := time.Now()
	incidents := fmt.Sprintf("incidents:\n  - id: INC-7\n    title: Checkout down\n    start: %s\n    end: %s\n",
		now.Add(-3*time.Hour).Format(time.RFC3339), now.Add(-2*time.Hour).Format(time.RFC3339))
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m"), "incidents.yaml": incidents})
	repo.git("remote", "add", "origin", repo.dir)
	repo.git("fetch", "--quiet", "origin")
	// The pull request: its first commit holds CheckoutErrors to 2h, which
	// then catches INC-7 no more; its second touches nothing that matters.
	repo.git("checkout", "--quiet", "-B", "pr")
	repo.write(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	repo.git("add", "-A")
	repo.git("commit", "--quiet", "-m", "hold CheckoutErrors longer")
	repo.write(map[string]string{"README.md": "docs\n"})
	repo.git("add", "-A")
	repo.git("commit", "--quiet", "-m", "docs")
	sha := repo.git("rev-parse", "HEAD")
	repo.git("checkout", "--quiet", "--detach", sha)
	h := backtestHosts[0] // github
	rr := newRecipeRun(t, h, repo)
	ci, _ := headOf(t, h.kind, sha)
	rr.env = append(rr.env, ci...)
	settings := []string{"PATHS=alerts", "INCIDENTS=incidents.yaml", "STATUS=true", "FAIL_MISSED=true"}
	args := []string{"--prometheus", prom.URL, "--tz", "UTC"}

	// TARGET left at HEAD^1, or given so as the action gives it, and no
	// branch the CI says: refused, never a success.
	for _, given := range [][]string{nil, {"TARGET=HEAD^1"}} {
		before := len(rr.host.statuses)
		out, err := rr.run(append(slices.Clone(settings), given...), args...)
		st, n := rr.host.lastStatus()
		if err == nil || !strings.Contains(out, "that parent is the pull request's own commit") || n > before && st.State == "success" {
			t.Errorf("%v: %v, status %+v\n%s", given, err, st, out)
		}
	}
	// Without a gate or a status, compared as before.
	if out, err := rr.run([]string{"PATHS=alerts"}, args...); err != nil || strings.Contains(out, "own commit") {
		t.Errorf("no gate: %v\n%s", err, out)
	}
	// GitHub's base branch: compared with main, INC-7 lost, a failure.
	out, err := rr.run(append(slices.Clone(settings), "GITHUB_BASE_REF=main"), args...)
	st, _ := rr.host.lastStatus()
	if err == nil || st.State != "failure" || st.SHA != sha || !strings.Contains(out, "compared with the branch it goes into, main") ||
		!strings.Contains(out, "caught by no rule") {
		t.Errorf("GITHUB_BASE_REF: %v, status %+v\n%s", err, st, out)
	}
	if out, err := rr.run(append(slices.Clone(settings), "GITHUB_BASE_REF=main;x"), args...); err == nil || !strings.Contains(out, "isn't one taken as TARGET_BRANCH") {
		t.Errorf("a branch not taken: %v\n%s", err, out)
	}

	// A merge of it into main, the status asked for on main's commit: not
	// the pull request's, none set.
	repo.git("checkout", "--quiet", "--detach", "main")
	repo.git("merge", "--quiet", "--no-ff", "--no-edit", sha)
	main := repo.git("rev-parse", "main")
	before := len(rr.host.statuses)
	out, err = rr.run(append(slices.Clone(settings), "STATUS_SHA="+main), args...)
	if _, n := rr.host.lastStatus(); n != before || !strings.Contains(out, "is the commit the pull request goes into") {
		t.Errorf("a status on the commit it goes into: %v, %d statuses\n%s", err, n-before, out)
	}
	// Its parent that is the pull request's, linking to a page on this
	// machine, which STATUS_URL takes over http as it says.
	out, err = rr.run(append(slices.Clone(settings), "STATUS_URL=http://127.0.0.1:8080/job/1"), args...)
	if st, _ := rr.host.lastStatus(); err == nil || !strings.Contains(out, "set the commit status lapilli/backtest on "+sha+": failure") || st.URL != "http://127.0.0.1:8080/job/1" {
		t.Errorf("a merge: %v, status %+v\n%s", err, st, out)
	}
}

// A pull request's head that is a merge of its own (GitHub's "Update
// branch", a merge --no-ff), checked out as it is (ref:
// pull_request.head.sha), has two parents: HEAD^1 is its own earlier
// commit, which the parents alone didn't tell from GitHub's merge. HEAD is
// the event's head, so GITHUB_BASE_REF is taken. A TARGET of HEAD's first
// parent written otherwise (HEAD~1, HEAD^, its hash) is too; and STATUS
// alone, refused, fails the job, never a success.
func TestBacktestRecipeOwnParentMerged(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	now := time.Now()
	incidents := fmt.Sprintf("incidents:\n  - id: INC-7\n    title: Checkout down\n    start: %s\n    end: %s\n",
		now.Add(-3*time.Hour).Format(time.RFC3339), now.Add(-2*time.Hour).Format(time.RFC3339))
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m"), "incidents.yaml": incidents})
	repo.git("remote", "add", "origin", repo.dir)
	repo.git("fetch", "--quiet", "origin")
	repo.git("checkout", "--quiet", "-B", "side", "main")
	repo.write(map[string]string{"NOTES.md": "x\n"})
	repo.git("add", "-A")
	repo.git("commit", "--quiet", "-m", "side")
	// The pull request: the rule change, then a merge of the side branch.
	repo.git("checkout", "--quiet", "-B", "pr", "main")
	repo.write(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	repo.git("add", "-A")
	repo.git("commit", "--quiet", "-m", "hold CheckoutErrors longer")
	repo.git("merge", "--quiet", "--no-ff", "--no-edit", "side")
	sha := repo.git("rev-parse", "HEAD")
	parent := repo.git("rev-parse", "HEAD^1")
	repo.git("checkout", "--quiet", "--detach", sha)
	h := backtestHosts[0] // github
	rr := newRecipeRun(t, h, repo)
	ci, _ := headOf(t, h.kind, sha)
	rr.env = append(rr.env, ci...)
	args := []string{"--prometheus", prom.URL, "--tz", "UTC"}
	gates := []string{"PATHS=alerts", "INCIDENTS=incidents.yaml", "STATUS=true", "FAIL_MISSED=true"}
	for _, target := range []string{"", "TARGET=HEAD^1", "TARGET=HEAD~1", "TARGET=HEAD^", "TARGET=" + parent} {
		settings := append(slices.Clone(gates), "GITHUB_BASE_REF=main")
		if target != "" {
			settings = append(settings, target)
		}
		out, err := rr.run(settings, args...)
		st, _ := rr.host.lastStatus()
		if err == nil || st.State != "failure" || st.SHA != sha || !strings.Contains(out, "compared with the branch it goes into, main") ||
			!strings.Contains(out, "caught by no rule") {
			t.Errorf("%q: %v, status %+v\n%s", target, err, st, out)
		}
	}
	// No base branch: refused, the job failed, with a gate or STATUS alone.
	for _, settings := range [][]string{gates, {"PATHS=alerts", "STATUS=true"}, {"PATHS=alerts", "STATUS=true", "TARGET=HEAD~1"}} {
		before := len(rr.host.statuses)
		out, err := rr.run(slices.Clone(settings), args...)
		st, n := rr.host.lastStatus()
		if err == nil || !strings.Contains(out, "that parent is the pull request's own commit") || n > before && st.State != "failure" {
			t.Errorf("%v: %v, status %+v\n%s", settings, err, st, out)
		}
	}
	// GitHub's own merge of it into main: HEAD^2 the event's head, HEAD^1
	// main's commit, taken as it is.
	repo.git("checkout", "--quiet", "--detach", "main")
	repo.git("merge", "--quiet", "--no-ff", "--no-edit", sha)
	out, err := rr.run(slices.Clone(gates), args...)
	if st, _ := rr.host.lastStatus(); err == nil || st.State != "failure" || st.SHA != sha || strings.Contains(out, "compared with the branch") ||
		!strings.Contains(out, "caught by no rule") {
		t.Errorf("GitHub's merge: %v, status %+v\n%s", err, st, out)
	}
}
