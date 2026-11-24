package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The backtest recipes in a merge queue's run: GitHub's merge_group, as
// its reference says the commits it makes.

// queueRef is the ref GitHub's merge queue names a merge group's branch
// by: refs/heads/gh-readonly-queue/BRANCH/pr-N-SHA.
func queueRef(branch, pr, sha string) string {
	return "refs/heads/gh-readonly-queue/" + branch + "/pr-" + pr + "-" + sha
}

// queueEvent is a merge_group event's file, as GitHub's webhook reference
// writes one (action checks_requested; merge_group's head_sha, head_ref,
// base_sha, base_ref and head_commit), of head on base, the branch's
// commit, into branch, its ref ref, in a repository whose default branch
// is def.
func queueEvent(t *testing.T, head, base, branch, ref, def string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"action": "checks_requested",
		"merge_group": map[string]any{"head_sha": head, "head_ref": ref, "base_sha": base, "base_ref": "refs/heads/" + branch,
			"head_commit": map[string]any{"id": head, "message": "Merge group"}},
		"repository": map[string]any{"full_name": "o/r", "default_branch": def}, "sender": map[string]any{"login": "someone"}})
	event := filepath.Join(t.TempDir(), "event.json")
	os.WriteFile(event, b, 0o600)
	return event
}

// queueRun is what GitHub Actions says of a merge queue's run beside its
// event: the event's name, GITHUB_REF the merge group's ref and
// GITHUB_SHA its head, and the run's page.
func queueRun(event, ref, head string) []string {
	return []string{"GITHUB_ACTIONS=true", "GITHUB_EVENT_NAME=merge_group", "GITHUB_EVENT_PATH=" + event, "GITHUB_REF=" + ref, "GITHUB_SHA=" + head,
		"GITHUB_SERVER_URL=https://github.example.com", "GITHUB_REPOSITORY=o/r", "GITHUB_RUN_ID=99"}
}

// queueRepo is gateRepo's, with a merge queue holding two pull requests
// into main, each squashed as the queue squashes it: first, pull request 6,
// on main, changing no rule; second, pull request 7, on first, holding
// CheckoutErrors 2h, which catches INC-7 no more.
func queueRepo(t *testing.T) (repo *prRepo, first, second, bad, aside string) {
	t.Helper()
	repo, bad, aside = gateRepo(t)
	repo.git("checkout", "--quiet", "--detach", "main")
	repo.write(map[string]string{"NOTES.md": "notes\n"})
	repo.git("add", "-A")
	repo.git("commit", "--quiet", "-m", "Notes (#6)")
	first = repo.git("rev-parse", "HEAD")
	repo.write(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	repo.git("commit", "--quiet", "-am", "Hold CheckoutErrors longer (#7)")
	second = repo.git("rev-parse", "HEAD")
	return repo, first, second, bad, aside
}

// A merge queue's run is backtested as GitHub's event says it: the merge
// group's head (merge_group.head_sha) against the branch's commit it was
// made on (merge_group.base_sha), the pull requests ahead of it in the
// queue in it too; INCIDENTS that commit's. The status goes on the head,
// where a required status is read in a queue, pending first, named for
// the branch as a pull request's is; no comment is posted, the report
// going to the job's log. FAIL_MISSED holds it as a pull request's run.
func TestBacktestRecipeMergeGroup(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	repo, first, second, bad, _ := queueRepo(t)
	main := repo.git("rev-parse", "main")
	repo.git("branch", "release", "main")
	args := []string{"--prometheus", prom.URL, "--tz", "UTC"}
	gates := []string{"PATHS=alerts", "INCIDENTS=incidents.yaml", "FAIL_MISSED=true", "STATUS=true"}
	for _, c := range []struct {
		name               string
		head, branch, pr   string
		settings           []string
		state, context, in string
	}{
		{"the first, changing no rule", first, "main", "6", []string{"PR="}, "success", "lapilli/backtest", "nothing changed"},
		{"the second, PR given as the action gives it", second, "main", "7", []string{"PR="}, "failure", "lapilli/backtest", "caught by no rule"},
		{"the second, PR the ref's", second, "main", "7", []string{"PR=7", "TARGET=HEAD^1"}, "failure", "lapilli/backtest", "caught by no rule"},
		{"the second, into release", second, "release", "7", nil, "failure", "lapilli/backtest (into release)", "caught by no rule"},
	} {
		t.Run(c.name, func(t *testing.T) {
			repo.git("checkout", "--quiet", "--detach", c.head)
			rr := newRecipeRun(t, backtestHosts[0], repo)
			ref := queueRef(c.branch, c.pr, bad)
			rr.env = append(rr.env, queueRun(queueEvent(t, c.head, main, c.branch, ref, "main"), ref, c.head)...)
			out, err := rr.run(append(slices.Clone(gates), c.settings...), args...)
			if (err == nil) != (c.state == "success") || !strings.Contains(out, c.in) {
				t.Errorf("%v\n%s", err, out)
			}
			if !strings.Contains(out, "the rules of "+c.head+", beside those of "+main) {
				t.Errorf("not the merge group's head beside its base:\n%s", out)
			}
			// The report, in the job's log, as a comment would have it.
			if !strings.Contains(out, "of no pull request to comment on") || !strings.Contains(out, "  | ### Lapilli backtest") {
				t.Errorf("the report isn't in the log:\n%s", out)
			}
			if rr.host.posts != 0 || rr.host.edits != 0 || len(rr.host.comments) != 0 {
				t.Errorf("commented: %d posted, %d edited", rr.host.posts, rr.host.edits)
			}
			if n := len(rr.host.statuses); n != 2 {
				t.Fatalf("%d statuses: %+v", n, rr.host.statuses)
			}
			for i, st := range rr.host.statuses {
				want := []string{"pending", c.state}[i]
				if st.SHA != c.head || st.State != want || st.Context != c.context || st.URL != "https://github.example.com/o/r/actions/runs/99" {
					t.Errorf("status %d: %+v, want %s %s on %s", i, st, want, c.context, c.head)
				}
			}
		})
	}
	// Squashed or merged, as the queue's merge method says: a merge
	// group's head that is a merge of the pull request's head into main is
	// judged against main alike.
	merge := repo.git("commit-tree", repo.git("rev-parse", bad+"^{tree}"), "-p", main, "-p", bad, "-m", "Merge pull request #7")
	repo.git("checkout", "--quiet", "--detach", merge)
	rr := newRecipeRun(t, backtestHosts[0], repo)
	ref := queueRef("main", "7", bad)
	rr.env = append(rr.env, queueRun(queueEvent(t, merge, main, "main", ref, "main"), ref, merge)...)
	out, err := rr.run(slices.Clone(gates), args...)
	if st, n := rr.host.lastStatus(); err == nil || n != 2 || st.State != "failure" || st.SHA != merge || !strings.Contains(out, "the rules of "+merge+", beside those of "+main) {
		t.Errorf("a merge: %v, %d statuses, the last %+v\n%s", err, n, st, lastLines(out, 4))
	}
}

// The action, in a merge queue's run: the event names no pull request,
// so the number the action gives is none, and target is its default,
// HEAD^1, which is none given; the merge group's head is judged against
// its base, and the status goes on it.
func TestBacktestActionMergeGroup(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	repo, _, second, bad, _ := queueRepo(t)
	main := repo.git("rev-parse", "main")
	repo.git("checkout", "--quiet", "--detach", second)
	rr := newRecipeRun(t, backtestHosts[0], repo)
	ref := queueRef("main", "7", bad)
	env := append(queueRun(queueEvent(t, second, main, "main", ref, "main"), ref, second), "PR=")
	out, err := runAction(t, rr, map[string]string{"version": "v0.32.0", "paths": "alerts", "incidents": "incidents.yaml", "prometheus-url": prom.URL,
		"args": "--tz UTC", "fail-missed": "true", "status": "true"}, env...)
	st, n := rr.host.lastStatus()
	if err == nil || !strings.Contains(out, "the rules of "+second+", beside those of "+main) || !strings.Contains(out, "caught by no rule") ||
		n != 2 || st.SHA != second || st.State != "failure" || st.Context != "lapilli/backtest" || rr.host.posts != 0 {
		t.Errorf("%v, %d statuses, the last %+v, %d posted\n%s", err, n, st, rr.host.posts, lastLines(out, 4))
	}
}

// What a merge queue's run is refused for. Where GitHub's own says no
// merge group, or another than the queue's (no merge_group in the event,
// a GITHUB_REF that isn't the queue's ref for the branch, a GITHUB_SHA
// that isn't its head): at once, with no status set, which a branch rule
// doesn't take as passing, and nothing commented. Where the job's
// settings name another base or head (TARGET, TARGET_BRANCH, STATUS_SHA,
// PR), and where the checkout isn't the head, or the base isn't an
// ancestor of it in the clone: the job fails, gates or not, and with
// STATUS the one status is a failure on the head, with no pending before
// it, so that no earlier run's success stands there.
func TestBacktestRecipeMergeGroupRefused(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	repo, _, second, bad, aside := queueRepo(t)
	main := repo.git("rev-parse", "main")
	args := []string{"--prometheus", prom.URL, "--tz", "UTC"}
	gates := []string{"PATHS=alerts", "INCIDENTS=incidents.yaml", "FAIL_MISSED=true", "STATUS=true", "PR="}
	ref := queueRef("main", "7", bad)
	event := queueEvent(t, second, main, "main", ref, "main")
	repo.git("checkout", "--quiet", "--detach", second)

	// GitHub's own says no merge group of the queue's: refused at once.
	write := func(body string) string {
		f := filepath.Join(t.TempDir(), "event.json")
		os.WriteFile(f, []byte(body), 0o600)
		return f
	}
	for _, c := range []struct {
		name string
		env  []string
		says string
	}{
		{"no merge_group in the event", queueRun(write(`{"pull_request": {"number": 7, "head": {"sha": "`+second+`"}}, "repository": {"default_branch": "main"}}`), ref, second),
			"the event has no merge_group"},
		{"no event", append(queueRun(event, ref, second), "GITHUB_EVENT_PATH="), "can't be read"},
		{"a merge_group of no hashes", queueRun(write(`{"merge_group": {"head_sha": "HEAD", "base_sha": "main", "head_ref": "`+ref+`", "base_ref": "refs/heads/main"}}`), ref, second),
			"aren't two commits' full hashes"},
		{"a base_ref of a tag", queueRun(queueEvent(t, second, main, "main", ref, "main"), ref, second), ""},
		{"another branch's queue", queueRun(queueEvent(t, second, main, "main", queueRef("release", "7", bad), "main"), queueRef("release", "7", bad), second),
			"isn't the queue's ref for main"},
		{"GITHUB_REF a branch", queueRun(event, "refs/heads/main", second), "isn't the queue's ref for main"},
		{"GITHUB_REF a pull request's merge", queueRun(queueEvent(t, second, main, "main", "refs/pull/7/merge", "main"), "refs/pull/7/merge", second),
			"isn't the queue's ref for main"},
		{"a ref below the queue's", queueRun(queueEvent(t, second, main, "main", "refs/heads/gh-readonly-queue/main/x/pr-7-"+bad, "main"),
			"refs/heads/gh-readonly-queue/main/x/pr-7-"+bad, second), "isn't the queue's ref for main"},
		{"a ref of no number", queueRun(queueEvent(t, second, main, "main", queueRef("main", "07", bad), "main"), queueRef("main", "07", bad), second),
			"isn't the queue's ref for main"},
		{"a ref of no head", queueRun(queueEvent(t, second, main, "main", "refs/heads/gh-readonly-queue/main/pr-7", "main"),
			"refs/heads/gh-readonly-queue/main/pr-7", second), "isn't the queue's ref for main"},
		{"GITHUB_SHA another", queueRun(event, ref, bad), "isn't the merge group's head"},
	} {
		if c.says == "" {
			// A base_ref of a tag, written into the event.
			var e map[string]any
			b, _ := os.ReadFile(event)
			json.Unmarshal(b, &e)
			e["merge_group"].(map[string]any)["base_ref"] = "refs/tags/main"
			b, _ = json.Marshal(e)
			c.env, c.says = queueRun(write(string(b)), ref, second), "isn't refs/heads/BRANCH"
		}
		rr := newRecipeRun(t, backtestHosts[0], repo)
		rr.env = append(rr.env, c.env...)
		for _, settings := range [][]string{gates, {"PATHS=alerts", "PR="}} {
			out, err := rr.run(slices.Clone(settings), args...)
			if err == nil || !strings.Contains(out, "a merge queue's run (merge_group) refused, no status set: ") || !strings.Contains(out, c.says) ||
				len(rr.host.statuses) != 0 || rr.host.posts != 0 {
				t.Errorf("%s, %v: %v, %d statuses, %d posted\n%s", c.name, settings, err, len(rr.host.statuses), rr.host.posts, lastLines(out, 3))
			}
		}
	}

	// Of a merge group: the job fails, and its status is the one failure,
	// on the head, where an earlier run's success stood.
	refused := func(name string, settings []string, says, text string) {
		t.Helper()
		rr := newRecipeRun(t, backtestHosts[0], repo)
		rr.env = append(rr.env, queueRun(event, ref, second)...)
		rr.host.set(func(h *commentHost) {
			h.statuses = []hostStatus{{SHA: second, State: "success", Context: "lapilli/backtest"}}
		})
		out, err := rr.run(append(slices.Clone(gates), settings...), args...)
		st, n := rr.host.lastStatus()
		if err == nil || !strings.Contains(out, says) || n != 2 || st.SHA != second || st.State != "failure" || st.Text != text || rr.host.posts != 0 {
			t.Errorf("%s: %v, %d set, the last %+v\n%s", name, err, n-1, st, lastLines(out, 3))
		}
	}
	setting := "a setting is wrong: the job's log says which"
	refused("TARGET", []string{"TARGET=refs/remotes/origin/aside"}, "TARGET: the base is the event's", setting)
	refused("TARGET_BRANCH", []string{"TARGET_BRANCH=aside"}, "TARGET_BRANCH: the base is the event's", setting)
	refused("STATUS_SHA", []string{"STATUS_SHA=" + bad}, "STATUS_SHA: the status goes on the merge group's head", setting)
	refused("PR", []string{"PR=8"}, "PR 8 isn't the pull request the queue's ref names, 7", setting)
	// The checkout another commit: a branch by its name, the pull
	// request's head.
	repo.git("checkout", "--quiet", "--detach", bad)
	refused("the checkout the pull request's head", nil, "HEAD "+bad+" isn't the merge group's head as the CI says, "+second,
		"refused: HEAD isn't the merge group's head as the CI says")
	// Without a gate or a status, failed all the same, and nothing set.
	rr := newRecipeRun(t, backtestHosts[0], repo)
	rr.env = append(rr.env, queueRun(event, ref, second)...)
	if out, err := rr.run([]string{"PATHS=alerts", "PR="}, args...); err == nil || !strings.Contains(out, "isn't the merge group's head as the CI says") ||
		len(rr.host.statuses) != 0 || rr.host.posts != 0 {
		t.Errorf("no gate, the checkout another: %v\n%s", err, lastLines(out, 3))
	}
	// A base the head wasn't made on: aside isn't second's ancestor.
	repo.git("checkout", "--quiet", "--detach", second)
	rr = newRecipeRun(t, backtestHosts[0], repo)
	rr.env = append(rr.env, queueRun(queueEvent(t, second, aside, "main", ref, "main"), ref, second)...)
	out, err := rr.run(slices.Clone(gates), args...)
	if st, n := rr.host.lastStatus(); err == nil || !strings.Contains(out, "isn't an ancestor of its head") || n != 1 || st.State != "failure" ||
		st.SHA != second || st.Text != "refused: the merge group's base isn't an ancestor of its head in the clone" {
		t.Errorf("a base not an ancestor: %v, %d set, %+v\n%s", err, n, st, lastLines(out, 3))
	}
}

// Through lapilli serve, a merge queue's run sends the merge group's
// head as the commit and the pull request its ref names, and logs serve's
// report where a pull request's would be commented; the status goes on
// the head.
func TestBacktestServeRecipeMergeGroup(t *testing.T) {
	needRecipeTools(t)
	repo, _, second, bad, _ := queueRepo(t)
	main := repo.git("rev-parse", "main")
	repo.git("checkout", "--quiet", "--detach", second)
	srv := newServeStandIn(t)
	markdown := serveMarkdown(changedSummary)
	srv.answers(nil, serveWaitsRunsDone(markdown))
	rr := serveRun(t, backtestHosts[0], repo, srv)
	ref := queueRef("main", "7", bad)
	rr.env = append(rr.env, queueRun(queueEvent(t, second, main, "main", ref, "main"), ref, second)...)
	out, err := rr.run([]string{"PATHS=alerts", "STATUS=true", "PR="}, "--since", "14d")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	body := srv.sent()
	if body["pull_request"] != json.Number("7") || body["commit"] != second {
		t.Errorf("pull_request %v, commit %v", body["pull_request"], body["commit"])
	}
	if before, after := sentFiles(t, body["before"]), sentFiles(t, body["after"]); before["shop.yaml"] != shopRules("5m") || after["shop.yaml"] != shopRules("2h") {
		t.Errorf("before %q, after %q", before["shop.yaml"], after["shop.yaml"])
	}
	if !strings.Contains(out, "  | ### Lapilli backtest, 2026-11-04 00:00") || rr.host.posts != 0 {
		t.Errorf("serve's report isn't in the log, or was commented (%d):\n%s", rr.host.posts, out)
	}
	if st, n := rr.host.lastStatus(); n != 2 || st.SHA != second || st.State != "success" || st.Context != "lapilli/backtest" {
		t.Errorf("%d statuses, the last %+v", n, st)
	}
}
