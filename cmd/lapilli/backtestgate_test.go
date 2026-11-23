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

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " / ")
}
