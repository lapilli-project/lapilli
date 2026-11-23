package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// gateRepo is a repository whose main holds CheckoutErrors for 5m, which
// catches INC-7, with origin itself; and a pull request's change, bad,
// that holds it 2h, which catches INC-7 no more (FAIL_MISSED fails); and a
// commit of its author's, aside, a branch of its own off main, that holds
// the same change already.
func gateRepo(t *testing.T) (repo *prRepo, bad, aside string) {
	t.Helper()
	now := time.Now()
	incidents := fmt.Sprintf("incidents:\n  - id: INC-7\n    title: Checkout down\n    start: %s\n    end: %s\n",
		now.Add(-3*time.Hour).Format(time.RFC3339), now.Add(-2*time.Hour).Format(time.RFC3339))
	repo = newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m"), "incidents.yaml": incidents})
	repo.git("remote", "add", "origin", repo.dir)
	repo.git("fetch", "--quiet", "origin")
	repo.git("checkout", "--quiet", "-B", "pr", "main")
	repo.write(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	repo.git("add", "-A")
	repo.git("commit", "--quiet", "-m", "hold CheckoutErrors longer")
	bad = repo.git("rev-parse", "HEAD")
	repo.git("checkout", "--quiet", "-B", "aside", "main")
	repo.write(map[string]string{"alerts/shop.yaml": shopRules("2h"), "NOTES.md": "x\n"})
	repo.git("add", "-A")
	repo.git("commit", "--quiet", "-m", "the same, aside")
	aside = repo.git("rev-parse", "HEAD")
	return repo, bad, aside
}

// A TARGET given by a short name was whatever git resolves the name to,
// and git takes a tag first (refs/tags/NAME before refs/remotes/NAME):
// whoever could push a tag named origin/main, at a commit of theirs that
// holds the change already, had the gate compare against it, pass
// FAIL_MISSED, and set a success on the event's head. A branch's name is
// the branch in full now, and one a tag has too is refused; a full hash,
// or a full ref, is taken as it is, a tag of its name or not.
func TestBacktestRecipeTargetNameIsATag(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	repo, _, aside := gateRepo(t)
	// The pull request's head: a child of the author's commit that holds
	// the change, so that commit is the last the two share.
	repo.git("checkout", "--quiet", "-B", "pr2", aside)
	repo.write(map[string]string{"NOTES.md": "y\n"})
	repo.git("add", "-A")
	repo.git("commit", "--quiet", "-m", "more")
	head := repo.git("rev-parse", "HEAD")
	main := repo.git("rev-parse", "main")
	repo.git("checkout", "--quiet", "--detach", "main")
	repo.git("merge", "--quiet", "--no-ff", "--no-edit", head)
	merge := repo.git("rev-parse", "HEAD")
	rr := newRecipeRun(t, backtestHosts[0], repo)
	ci, _ := headOf(t, "github", head)
	rr.env = append(append(rr.env, ci...), "GITHUB_SHA="+merge, "GITHUB_REF=refs/pull/7/merge", "GITHUB_BASE_REF=main")
	args := []string{"--prometheus", prom.URL, "--tz", "UTC"}
	gates := []string{"PATHS=alerts", "INCIDENTS=incidents.yaml", "FAIL_MISSED=true", "STATUS=true"}
	lost := func(target string) {
		t.Helper()
		out, err := rr.run(append(slices.Clone(gates), "TARGET="+target), args...)
		if st, _ := rr.host.lastStatus(); err == nil || st.State != "failure" || st.SHA != head || !strings.Contains(out, "caught by no rule") {
			t.Errorf("TARGET=%s: %v, %+v\n%s", target, err, st, lastLines(out, 4))
		}
	}
	// As the job means it, no tag: the remote's branch, INC-7 lost.
	lost("origin/main")
	// Tags named as the branch and as main's short hash, at the author's
	// commit.
	repo.git("tag", "origin/main", aside)
	repo.git("tag", main[:12], aside)
	for _, target := range []string{"origin/main", "origin/main~0", main[:12]} {
		rr.host.set(func(h *commentHost) {
			h.statuses = []hostStatus{{SHA: head, State: "success", Context: "lapilli/backtest"}}
		})
		out, err := rr.run(append(slices.Clone(gates), "TARGET="+target), args...)
		st, n := rr.host.lastStatus()
		if err == nil || !strings.Contains(out, "TARGET "+target+" names a tag too") || n != 2 || st.State != "failure" || st.SHA != head ||
			st.Text != "refused: TARGET names a tag too, which git would take for it" {
			t.Errorf("TARGET=%s, a tag of its name: %v, %d statuses set, the last %+v\n%s", target, err, n-1, st, lastLines(out, 4))
		}
	}
	rr.host.set(func(h *commentHost) { h.statuses = nil })
	// Without a gate or a status: the comment says why, and the job goes on.
	if out, err := rr.run([]string{"PATHS=alerts", "TARGET=origin/main"}, args...); err != nil || !strings.Contains(rr.body(), "names a tag too") {
		t.Errorf("no gate: %v\n%s\n%s", err, lastLines(out, 3), rr.body())
	}
	// In full, or by its hash: the branch the job means, a tag of its name
	// or not.
	lost("refs/remotes/origin/main")
	lost(main)
	lost(strings.ToUpper(main))
}

// eventOf is a pull_request event's file, as GitHub or Gitea writes it:
// the pull request 7, of head sha into branch into, in a repository whose
// default branch is def ("" for an event that doesn't say).
func eventOf(t *testing.T, sha, into, def string) string {
	t.Helper()
	event := filepath.Join(t.TempDir(), "event.json")
	body := fmt.Sprintf(`{"pull_request": {"number": 7, "head": {"sha": %q}, "base": {"ref": %q}}`, sha, into)
	if def != "" {
		body += fmt.Sprintf(`, "repository": {"default_branch": %q}`, def)
	}
	os.WriteFile(event, []byte(body+"}"), 0o600)
	return event
}

// lastOf is the status of name context set last on sha, and whether one was.
func lastOf(h *commentHost, sha, context string) (hostStatus, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.statuses) - 1; i >= 0; i-- {
		if st := h.statuses[i]; st.SHA == sha && st.Context == context {
			return st, true
		}
	}
	return hostStatus{}, false
}

// A status is a commit's and its name's, not a pull request's: the same
// head opened again as a second pull request, into a branch its author
// chose that holds the change already, was judged against that branch,
// and its success took the place of the failure the pull request into the
// default branch left, the status that branch's rule requires. Where the
// CI says the default branch (GitHub's and Gitea's event, GitLab's
// CI_DEFAULT_BRANCH), a pull request into another branch sets
// lapilli/backtest (into BRANCH), and the default's stays as its own pull
// request left it; one into the default, or where the CI doesn't say, is
// named as before. A branch a status's name can't hold sets none.
func TestBacktestRecipeSecondPullRequest(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	repo, bad, aside := gateRepo(t)
	args := []string{"--prometheus", prom.URL, "--tz", "UTC"}
	gates := []string{"PATHS=alerts", "INCIDENTS=incidents.yaml", "FAIL_MISSED=true", "STATUS=true"}
	main := repo.git("rev-parse", "main")
	t.Run("github", func(t *testing.T) {
		rr := newRecipeRun(t, backtestHosts[0], repo)
		for _, into := range []struct{ branch, tip, context, state string }{
			{"main", main, "lapilli/backtest", "failure"},
			{"aside", aside, "lapilli/backtest (into aside)", "success"},
		} {
			merge := repo.git("commit-tree", repo.git("rev-parse", bad+"^{tree}"), "-p", into.tip, "-p", bad, "-m", "Merge")
			repo.git("checkout", "--quiet", "--detach", merge)
			env := []string{"GITHUB_EVENT_PATH=" + eventOf(t, bad, into.branch, "main"), "GITHUB_BASE_REF=" + into.branch, "GITHUB_SHA=" + merge, "GITHUB_REF=refs/pull/7/merge"}
			out, err := rr.run(append(slices.Clone(gates), env...), args...)
			if st, ok := lastOf(rr.host, bad, into.context); !ok || st.State != into.state || (err == nil) != (into.state == "success") || st.SHA != bad {
				t.Errorf("into %s: %v, %s %+v\n%s", into.branch, err, into.context, st, lastLines(out, 3))
			}
		}
		if st, ok := lastOf(rr.host, bad, "lapilli/backtest"); !ok || st.State != "failure" {
			t.Errorf("the default branch's status after the second pull request: %+v", st)
		}
		// The event that doesn't say the default branch: named as before.
		repo.git("checkout", "--quiet", "--detach", bad)
		out, _ := rr.run(append(slices.Clone(gates), "GITHUB_EVENT_PATH="+eventOf(t, bad, "aside", ""), "GITHUB_BASE_REF=aside"), args...)
		if st, _ := rr.host.lastStatus(); st.Context != "lapilli/backtest" || st.SHA != bad {
			t.Errorf("no default branch said: %+v\n%s", st, lastLines(out, 3))
		}
		// A branch a status's name can't hold: none set.
		before := len(rr.host.statuses)
		out, _ = rr.run(append(slices.Clone(gates), "GITHUB_EVENT_PATH="+eventOf(t, bad, "aside(x)", "main"), "GITHUB_BASE_REF=aside(x)"), args...)
		if _, n := rr.host.lastStatus(); n != before || !strings.Contains(out, "a status's name can't hold it") {
			t.Errorf("a branch the name can't hold: %d set\n%s", n-before, lastLines(out, 3))
		}
	})
	// Gitea's and GitLab's are compared from where the head and the branch
	// parted, main's commit here: the name is what tells the two apart.
	t.Run("gitea", func(t *testing.T) {
		rr := newRecipeRun(t, backtestHosts[1], repo)
		repo.git("checkout", "--quiet", "--detach", bad)
		for _, into := range []struct{ branch, context, state string }{
			{"main", "lapilli/backtest", "failure"},
			{"aside", "lapilli/backtest (into aside)", "failure"},
		} {
			env := []string{"GITEA_ACTIONS=true", "GITHUB_EVENT_PATH=" + eventOf(t, bad, into.branch, "main"), "GITHUB_BASE_REF=" + into.branch}
			out, err := rr.run(append(slices.Clone(gates), env...), args...)
			if st, ok := lastOf(rr.host, bad, into.context); !ok || st.State != into.state || (err == nil) != (into.state == "success") || st.SHA != bad {
				t.Errorf("into %s: %v, %s %+v\n%s", into.branch, err, into.context, st, lastLines(out, 3))
			}
		}
		if st, ok := lastOf(rr.host, bad, "lapilli/backtest"); !ok || st.State != "failure" {
			t.Errorf("the default branch's status after the second pull request: %+v", st)
		}
	})
	t.Run("gitlab", func(t *testing.T) {
		rr := newRecipeRun(t, backtestHosts[2], repo)
		repo.git("checkout", "--quiet", "--detach", bad)
		for _, into := range []struct{ branch, context, state string }{
			{"main", "lapilli/backtest", "failed"},
			{"aside", "lapilli/backtest (into aside)", "failed"},
		} {
			env := []string{"CI_COMMIT_SHA=" + bad, "CI_MERGE_REQUEST_IID=7", "CI_DEFAULT_BRANCH=main", "CI_MERGE_REQUEST_TARGET_BRANCH_NAME=" + into.branch}
			out, err := rr.run(append(slices.Clone(gates), env...), args...)
			if st, ok := lastOf(rr.host, bad, into.context); !ok || st.State != into.state || (err == nil) != (into.state == "success") || st.SHA != bad {
				t.Errorf("into %s: %v, %s %+v\n%s", into.branch, err, into.context, st, lastLines(out, 3))
			}
		}
		if st, ok := lastOf(rr.host, bad, "lapilli/backtest"); !ok || st.State != "failed" {
			t.Errorf("the default branch's status after the second merge request: %+v", st)
		}
	})
}

// In an event of no pull request (push, workflow_run, a branch's
// pipeline), the CI says the default branch but not the one the pull
// request goes into: the status kept the default's name whatever
// TARGET_BRANCH, so a job judging the same head into a branch its author
// chose, holding the change already, set lapilli/backtest success over the
// failure the job into main left. TARGET_BRANCH is the branch now, named
// as the CI's would be; a TARGET, or none, names no branch, and no status
// is set.
func TestBacktestRecipeNoPullRequestStatusName(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	repo, _, aside := gateRepo(t)
	// The head: a child of the author's commit that holds the change, so
	// it parts from main at main, and from aside at aside.
	repo.git("checkout", "--quiet", "-B", "pr2", aside)
	repo.write(map[string]string{"NOTES.md": "y\n"})
	repo.git("add", "-A")
	repo.git("commit", "--quiet", "-m", "more")
	head := repo.git("rev-parse", "HEAD")
	repo.git("fetch", "--quiet", "origin")
	repo.git("checkout", "--quiet", "--detach", head)
	event := filepath.Join(t.TempDir(), "event.json")
	os.WriteFile(event, []byte(`{"workflow_run": {"head_sha": "`+head+`"}, "repository": {"default_branch": "main"}}`), 0o600)
	args := []string{"--prometheus", prom.URL, "--tz", "UTC"}
	gates := []string{"PATHS=alerts", "INCIDENTS=incidents.yaml", "FAIL_MISSED=true", "STATUS=true", "STATUS_SHA=" + head}
	for _, c := range []struct {
		host int
		ci   []string
		fail string
	}{
		{0, []string{"GITHUB_ACTIONS=true", "GITHUB_EVENT_NAME=workflow_run", "GITHUB_EVENT_PATH=" + event}, "failure"},
		{1, []string{"GITEA_ACTIONS=true", "GITHUB_EVENT_NAME=push", "GITHUB_EVENT_PATH=" + event}, "failure"},
		{2, []string{"CI_COMMIT_SHA=" + head, "CI_DEFAULT_BRANCH=main"}, "failed"},
	} {
		rr := newRecipeRun(t, backtestHosts[c.host], repo)
		rr.env = append(rr.env, c.ci...)
		for _, into := range []struct{ branch, context, state string }{
			{"main", "lapilli/backtest", c.fail},
			{"aside", "lapilli/backtest (into aside)", "success"},
		} {
			out, err := rr.run(append(slices.Clone(gates), "TARGET_BRANCH="+into.branch), args...)
			if st, ok := lastOf(rr.host, head, into.context); !ok || st.State != into.state || (err == nil) != (into.state == "success") {
				t.Errorf("%s, TARGET_BRANCH=%s: %v, %s %+v\n%s", rr.h.kind, into.branch, err, into.context, st, lastLines(out, 3))
			}
		}
		if st, ok := lastOf(rr.host, head, "lapilli/backtest"); !ok || st.State != c.fail {
			t.Errorf("%s: the default branch's status after the job into aside: %+v", rr.h.kind, st)
		}
		// A TARGET, a branch's in full or not, names no branch: none set.
		before := len(rr.host.statuses)
		out, _ := rr.run(append(slices.Clone(gates), "TARGET=refs/remotes/origin/aside"), args...)
		if _, n := rr.host.lastStatus(); n != before || !strings.Contains(out, "TARGET_BRANCH names none: no status set") {
			t.Errorf("%s, TARGET alone: %d set\n%s", rr.h.kind, n-before, lastLines(out, 3))
		}
	}
}

// TARGET=HEAD^1, lib.sh's default, written out: the scripts of the CIs
// that say the base set their TARGET_BRANCH default only without TARGET,
// while lib.sh takes a literal HEAD^1 as none given. A head that is the
// author's own merge, its first parent a commit of theirs holding the
// change, had that parent for the base: FAIL_MISSED passed, and the status
// was a success on it. HEAD^1 written out is none given now, in each
// script; and Gitea's Actions, which makes no merge in any event, never
// takes HEAD a merge for one.
func TestBacktestRecipeHeadParentWrittenOut(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	repo, bad, aside := gateRepo(t)
	// The author's merge: first parent their own commit, second the change.
	merge := repo.git("commit-tree", repo.git("rev-parse", bad+"^{tree}"), "-p", aside, "-p", bad, "-m", "Merge")
	repo.git("checkout", "--quiet", "--detach", merge)
	args := []string{"--prometheus", prom.URL, "--tz", "UTC"}
	gates := []string{"PATHS=alerts", "INCIDENTS=incidents.yaml", "FAIL_MISSED=true", "STATUS=true"}
	for _, c := range []struct {
		host int
		ci   []string
	}{
		{1, []string{"GITEA_ACTIONS=true", "GITHUB_EVENT_NAME=pull_request", "GITHUB_BASE_REF=main", "GITHUB_EVENT_PATH=" + eventOf(t, merge, "main", "main")}},
		{2, []string{"CI_COMMIT_SHA=" + merge, "CI_MERGE_REQUEST_IID=7", "CI_DEFAULT_BRANCH=main", "CI_MERGE_REQUEST_TARGET_BRANCH_NAME=main"}},
		{3, []string{"BITBUCKET_COMMIT=" + merge, "BITBUCKET_PR_DESTINATION_BRANCH=main"}},
		{4, []string{"SYSTEM_PULLREQUEST_SOURCECOMMITID=" + merge, "SYSTEM_PULLREQUEST_TARGETBRANCH=refs/heads/main"}},
	} {
		rr := newRecipeRun(t, backtestHosts[c.host], repo)
		rr.env = append(rr.env, c.ci...)
		rr.host.set(func(h *commentHost) { h.iterations = []string{merge} })
		for _, target := range []string{"TARGET=", "TARGET=HEAD^1"} {
			out, err := rr.run(append(slices.Clone(gates), target), args...)
			st, _ := rr.host.lastStatus()
			if err == nil || !strings.Contains(out, "caught by no rule") || !slices.Contains([]string{"failure", "failed", "FAILED"}, st.State) {
				t.Errorf("%s, %q: %v, the last status %+v\n%s", rr.h.kind, target, err, st, lastLines(out, 3))
			}
		}
	}
	// Gitea's Actions with the TARGET_BRANCH default left out (an event of
	// no base, the job giving TARGET=HEAD^1): HEAD a merge is its author's,
	// refused with a gate.
	rr := newRecipeRun(t, backtestHosts[1], repo)
	rr.env = append(rr.env, "GITEA_ACTIONS=true", "GITHUB_EVENT_NAME=pull_request", "GITHUB_EVENT_PATH="+eventOf(t, merge, "main", "main"))
	out, err := rr.run(append(slices.Clone(gates), "TARGET=HEAD^1"), args...)
	if st, _ := rr.host.lastStatus(); err == nil || !strings.Contains(out, "Gitea's and Forgejo's in none") || st.State != "failure" {
		t.Errorf("gitea, no base: %v, the last status %+v\n%s", err, st, lastLines(out, 3))
	}
}

// An event of no pull request (push, workflow_run), the job giving PR: in
// GitHub Actions, HEAD a merge was still taken for the CI's, and its first
// parent for the base, though GitHub made no merge: a merge of the change
// into a commit its author chose, holding the change already, passed
// FAIL_MISSED and got a success, where the same change as a plain commit
// was refused. GitHub's Actions says its merge (GITHUB_SHA of
// refs/pull/N/merge), so one it doesn't say isn't taken: refused with a
// gate or STATUS, unless TARGET or TARGET_BRANCH says the base.
func TestBacktestRecipeNoEventHead(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	repo, bad, _ := gateRepo(t)
	repo.git("checkout", "--quiet", "aside")
	repo.git("merge", "--quiet", "--no-ff", "--no-edit", bad)
	own := repo.git("rev-parse", "HEAD")
	args := []string{"--prometheus", prom.URL, "--tz", "UTC"}
	gates := []string{"PATHS=alerts", "INCIDENTS=incidents.yaml", "FAIL_MISSED=true", "STATUS=true", "STATUS_SHA=" + own}
	for _, ev := range [][]string{
		{"GITHUB_ACTIONS=true", "GITHUB_EVENT_NAME=push", "GITHUB_REF=refs/heads/pr", "GITHUB_SHA=" + own},
		{"GITHUB_ACTIONS=true", "GITHUB_EVENT_NAME=workflow_run", "GITHUB_REF=refs/heads/main", "GITHUB_SHA=" + repo.git("rev-parse", "main")},
	} {
		rr := newRecipeRun(t, backtestHosts[0], repo)
		event := filepath.Join(t.TempDir(), "event.json")
		os.WriteFile(event, []byte(`{"workflow_run": {"head_sha": "`+own+`"}}`), 0o600)
		rr.env = append(append(rr.env, ev...), "GITHUB_EVENT_PATH="+event)
		repo.git("checkout", "--quiet", "--detach", own)
		for _, settings := range [][]string{gates, {"PATHS=alerts", "STATUS=true", "STATUS_SHA=" + own}} {
			rr.host.set(func(h *commentHost) {
				h.statuses = []hostStatus{{SHA: own, State: "success", Context: "lapilli/backtest"}}
			})
			out, err := rr.run(slices.Clone(settings), args...)
			st, n := rr.host.lastStatus()
			if err == nil || !strings.Contains(out, "but the CI made no merge of a pull request (GitHub's Actions makes one in a pull request's event alone") || n != 2 || st.State != "failure" ||
				st.Text != "refused: TARGET is HEAD's first parent, and the CI made no merge of a pull request" {
				t.Errorf("%s, %v: %v, %d set, the last %+v\n%s", ev[1], settings, err, n-1, st, lastLines(out, 3))
			}
		}
		// The branch it goes into, given: judged against it, INC-7 lost.
		out, err := rr.run(append(slices.Clone(gates), "TARGET_BRANCH=main"), args...)
		if st, _ := rr.host.lastStatus(); err == nil || st.State != "failure" || !strings.Contains(out, "caught by no rule") {
			t.Errorf("%s, TARGET_BRANCH: %v, %+v\n%s", ev[1], err, st, lastLines(out, 3))
		}
		// Without a gate or a status, the comment, as before.
		if out, err := rr.run([]string{"PATHS=alerts"}, args...); err != nil {
			t.Errorf("%s, no gate: %v\n%s", ev[1], err, lastLines(out, 3))
		}
	}
	// Outside GitHub Actions, HEAD a merge is the CI's, as before.
	rr := newRecipeRun(t, backtestHosts[0], repo)
	if out, err := rr.run(slices.Clone(gates), args...); strings.Contains(out, "made no merge") {
		t.Errorf("outside Actions: %v\n%s", err, lastLines(out, 3))
	}
}

// GITHUB_SHA is GitHub's merge, but of another head than the event's: a
// merge of an earlier push, which an event between the two may say beside
// the later head. HEAD was the CI's merge, so not refused; its second
// parent not the event's head, the base was GITHUB_BASE_REF, the head's
// status couldn't be set, and the job passed on what the earlier head
// held. It is refused now, its failure on the event's head.
func TestBacktestRecipeMergeOfAnotherHead(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	repo, bad, _ := gateRepo(t)
	repo.git("checkout", "--quiet", "-B", "earlier", "main")
	repo.write(map[string]string{"NOTES.md": "nothing\n"})
	repo.git("add", "-A")
	repo.git("commit", "--quiet", "-m", "notes")
	earlier := repo.git("rev-parse", "HEAD")
	stale := repo.git("commit-tree", repo.git("rev-parse", earlier+"^{tree}"), "-p", "main", "-p", earlier, "-m", "Merge")
	repo.git("checkout", "--quiet", "--detach", stale)
	rr := newRecipeRun(t, backtestHosts[0], repo)
	ci, _ := headOf(t, "github", bad)
	rr.env = append(append(rr.env, ci...), "GITHUB_SHA="+stale, "GITHUB_REF=refs/pull/7/merge", "GITHUB_BASE_REF=main")
	args := []string{"--prometheus", prom.URL, "--tz", "UTC"}
	for _, settings := range [][]string{{"PATHS=alerts", "INCIDENTS=incidents.yaml", "FAIL_MISSED=true", "STATUS=true"}, {"PATHS=alerts", "STATUS=true"}} {
		rr.host.set(func(h *commentHost) {
			h.statuses = []hostStatus{{SHA: bad, State: "success", Context: "lapilli/backtest"}}
		})
		out, err := rr.run(settings, args...)
		st, n := rr.host.lastStatus()
		if err == nil || !strings.Contains(out, "HEAD "+stale+" is the CI's merge of "+earlier+", not of the pull request's head as the CI says, "+bad) ||
			n != 2 || st.SHA != bad || st.State != "failure" || st.Text != "refused: HEAD is the CI's merge of another head than the pull request's as the CI says" {
			t.Errorf("%v: %v, %d set, the last %+v\n%s", settings, err, n-1, st, lastLines(out, 3))
		}
	}
}

// A merge queue's run (merge_group) is said plainly to be one not taken
// yet, whatever PR says, rather than as a pull request's number missing.
func TestBacktestRecipeMergeGroup(t *testing.T) {
	needRecipeTools(t)
	repo, bad, _ := gateRepo(t)
	squash := repo.git("commit-tree", repo.git("rev-parse", bad+"^{tree}"), "-p", "main", "-m", "squashed (#7)")
	repo.git("checkout", "--quiet", "--detach", squash)
	rr := newRecipeRun(t, backtestHosts[0], repo)
	rr.env = append(rr.env, "GITHUB_ACTIONS=true", "GITHUB_EVENT_NAME=merge_group", "GITHUB_SHA="+squash)
	for _, pr := range []string{"PR=", "PR=7"} {
		out, err := rr.run([]string{"PATHS=alerts", "STATUS=true", "FAIL_MISSED=true", "INCIDENTS=incidents.yaml", pr})
		if err == nil || !strings.Contains(out, "a merge queue's run (merge_group) isn't taken yet") {
			t.Errorf("%s: %v\n%s", pr, err, out)
		}
	}
}

// What an earlier run left on the commit, success or the pending of one
// killed, stood where a run with STATUS ended before it set the status:
// refused where the CI says no head and STATUS_SHA isn't given; PATHS in
// neither commit; a setting found wrong; GitHub's merge in a clone of
// depth 1, whose parents the clone hasn't. Each sets the one failure now,
// on the head the CI says, or HEAD where it says none.
func TestBacktestRecipeEndsWithAFailure(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	repo, bad, _ := gateRepo(t)
	args := []string{"--prometheus", prom.URL, "--tz", "UTC"}
	repo.git("checkout", "--quiet", "--detach", bad)
	merge := repo.git("commit-tree", repo.git("rev-parse", bad+"^{tree}"), "-p", "main", "-p", bad, "-m", "Merge")
	repo.git("branch", "--force", "merged", merge)
	shallow := *repo
	shallow.dir = filepath.Join(t.TempDir(), "shallow")
	repo.git("clone", "--quiet", "--depth", "1", "--branch", "merged", "file://"+repo.dir, shallow.dir)
	for _, c := range []struct {
		name     string
		repo     *prRepo
		ci       bool
		settings []string
		says     string
	}{
		{"refused, the CI saying no head, no STATUS_SHA", repo, false, []string{"PATHS=alerts", "STATUS=true"}, "that parent is the pull request's own commit"},
		{"PATHS in neither commit", repo, true, []string{"PATHS=nowhere", "STATUS=true", "GITHUB_BASE_REF=main"}, "PATHS: nowhere: in neither commit"},
		{"a setting wrong", repo, true, []string{"PATHS=alerts", "STATUS=true", "TARGET_BRANCH=-x"}, "TARGET_BRANCH -x: want a branch's name"},
		{"a setting wrong, the CI saying no head", repo, false, []string{"PATHS=alerts", "STATUS=true", "MAX_TREE_FILES=many"}, "MAX_TREE_FILES many: want a number"},
		{"GitHub's merge at depth 1", &shallow, true, append([]string{"PATHS=alerts", "STATUS=true", "INCIDENTS=incidents.yaml", "FAIL_MISSED=true", "GITHUB_BASE_REF=main"}, githubMerge(merge)...),
			"share no commit in the clone"},
	} {
		rr := newRecipeRun(t, backtestHosts[0], c.repo)
		if c.ci {
			ci, _ := headOf(t, "github", bad)
			rr.env = append(rr.env, ci...)
		}
		for _, left := range []string{"success", "pending"} {
			rr.host.set(func(h *commentHost) { h.statuses = []hostStatus{{SHA: bad, State: left, Context: "lapilli/backtest"}} })
			out, err := rr.run(slices.Clone(c.settings), args...)
			st, n := rr.host.lastStatus()
			if err == nil || !strings.Contains(out, c.says) || n != 2 || st.SHA != bad || st.State != "failure" {
				t.Errorf("%s, an earlier %s: %v, %d set, the commit's status now %+v\n%s", c.name, left, err, n-1, st, lastLines(out, 3))
			}
		}
	}
	// Before the status's own settings are read, none can be set.
	rr := newRecipeRun(t, backtestHosts[0], repo)
	rr.host.set(func(h *commentHost) { h.statuses = nil })
	if out, err := rr.run([]string{"PATHS=alerts", "STATUS=true", "STATUS_SHA=x"}, args...); err == nil || len(rr.host.statuses) != 0 {
		t.Errorf("STATUS_SHA wrong: %v, %+v\n%s", err, rr.host.statuses, out)
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " / ")
}
