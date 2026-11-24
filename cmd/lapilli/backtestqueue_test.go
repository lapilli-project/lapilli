package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The backtest recipes in a merge queue's run: GitHub's merge_group and
// GitLab's merge trains, each as its reference says the commits it makes.

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
	parallel(t)
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
	parallel(t)
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
	parallel(t)
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
		// A line break after a field's first part: never read as that part.
		{"a base_ref of two lines", queueRun(write(`{"merge_group": {"head_sha": "`+second+`", "base_sha": "`+main+`", "head_ref": "`+ref+`", "base_ref": "refs/heads/main\nx"}}`), ref, second),
			"isn't refs/heads/BRANCH"},
		{"a head_sha of two lines", queueRun(write(`{"merge_group": {"head_sha": "`+second+`\n`+bad+`", "base_sha": "`+main+`", "head_ref": "`+ref+`", "base_ref": "refs/heads/main"}}`), ref, second),
			"aren't two commits' full hashes"},
		// Nor one with a line break at its end, which $ in jq matches before
		// and the shell's $(...) would take off.
		{"a head_sha and a line break", queueRun(write(`{"merge_group": {"head_sha": "`+second+`\n", "base_sha": "`+main+`", "head_ref": "`+ref+`", "base_ref": "refs/heads/main"}}`), ref, second),
			"aren't two commits' full hashes"},
		{"a base_ref and a line break", queueRun(write(`{"merge_group": {"head_sha": "`+second+`", "base_sha": "`+main+`", "head_ref": "`+ref+`", "base_ref": "refs/heads/main\n"}}`), ref, second),
			"isn't refs/heads/BRANCH"},
		{"a head_ref and a line break", queueRun(write(`{"merge_group": {"head_sha": "`+second+`", "base_sha": "`+main+`", "head_ref": "`+ref+`\n", "base_ref": "refs/heads/main"}}`), ref, second),
			"isn't the queue's ref for main"},
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
	parallel(t)
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

// gitlab.sh in a merge train's pipeline, as GitLab's merge request
// pipelines' reference says it: the job checks out a merge commit
// (CI_COMMIT_SHA) of the source branch's head
// (CI_MERGE_REQUEST_SOURCE_BRANCH_SHA), whose first parent is the commit
// the pipeline is based on, the target branch with the merge requests
// ahead in the train. The change is that merge, compared against that
// first parent, INCIDENTS its, never CI_MERGE_REQUEST_TARGET_BRANCH_SHA,
// which the reference says of merged results pipelines alone; the status
// goes on the source branch's head, as in a merged results pipeline. A
// checkout of another commit, or a train's commit that isn't such a
// merge, is refused, gates or not: its base is a commit GitLab doesn't say.
func TestBacktestRecipeGitLabMergeTrain(t *testing.T) {
	parallel(t)
	needRecipeTools(t)
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m"), "incidents.yaml": "incidents: []\n"})
	old := repo.git("rev-parse", "HEAD")
	// The target branch moves on.
	repo.write(map[string]string{"README.md": "x\n"})
	repo.git("add", "-A")
	repo.git("commit", "--quiet", "-m", "the target moves on")
	tip := repo.git("rev-parse", "HEAD")
	// The merge request ahead in the train, which adds a rule file and an
	// incident, merged on the target's head as the train merges it.
	repo.git("checkout", "--quiet", "-b", "ahead", old)
	repo.write(map[string]string{"alerts/api.yaml": "# the one ahead's\ngroups: []\n",
		"incidents.yaml": "incidents:\n  - {id: INC-9, start: 2026-11-10T00:00:00Z, end: 2026-11-10T01:00:00Z}\n"})
	repo.git("add", "-A")
	repo.git("commit", "--quiet", "-m", "ahead")
	repo.git("checkout", "--quiet", "--detach", tip)
	repo.git("merge", "--quiet", "--no-ff", "--no-edit", "ahead")
	car1 := repo.git("rev-parse", "HEAD")
	// This merge request, its source branch parted from the target before
	// it moved, merged on the one ahead.
	repo.git("checkout", "--quiet", "-b", "pr", old)
	repo.write(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	repo.git("commit", "--quiet", "-am", "slower")
	source := repo.git("rev-parse", "HEAD")
	repo.git("checkout", "--quiet", "--detach", car1)
	repo.git("merge", "--quiet", "--no-ff", "--no-edit", "pr")
	car2 := repo.git("rev-parse", "HEAD")
	if parents := repo.git("rev-list", "--parents", "-n", "1", car2); parents != car2+" "+car1+" "+source {
		t.Fatalf("the merge's parents: %s", parents)
	}
	repo.git("remote", "add", "origin", repo.dir)

	kept := t.TempDir()
	fake := filepath.Join(t.TempDir(), "lapilli")
	os.WriteFile(fake, []byte("#!/bin/sh\nwhile [ $# -gt 0 ]; do\n  case $1 in\n"+
		"    --rules) rm -rf '"+kept+"/rules'; cp -R \"$2\" '"+kept+"/rules' ;;\n"+
		"    --compare) rm -rf '"+kept+"/compare'; cp -R \"$2\" '"+kept+"/compare' ;;\n"+
		"    --incidents) cp \"$2\" '"+kept+"/incidents' ;;\n  esac\n  shift\ndone\n"+
		"echo '[//]: # (lapilli-compare {\"nothing_changed\":false,\"changed\":1,\"unreplayed\":0})'\n"), 0o755)
	given := func(p ...string) string {
		b, _ := os.ReadFile(filepath.Join(append([]string{kept}, p...)...))
		return string(b)
	}
	// CI_MERGE_REQUEST_TARGET_BRANCH_SHA set, to the target's head: taken,
	// the change would hold what the one ahead changed.
	job := []string{"CI_MERGE_REQUEST_IID=7", "CI_PIPELINE_SOURCE=merge_request_event", "CI_MERGE_REQUEST_EVENT_TYPE=merge_train",
		"CI_MERGE_REQUEST_TARGET_BRANCH_NAME=main", "CI_MERGE_REQUEST_TARGET_BRANCH_SHA=" + tip, "CI_MERGE_REQUEST_DIFF_BASE_SHA=" + old,
		"CI_MERGE_REQUEST_SOURCE_BRANCH_SHA=" + source, "CI_COMMIT_SHA=" + car2, "CI_DEFAULT_BRANCH=main", "CI_PIPELINE_ID=314",
		"CI_JOB_URL=https://gitlab.example.com/o/r/-/jobs/27"}
	settings := []string{"PATHS=alerts", "INCIDENTS=incidents.yaml", "STATUS=true", "FAIL_ON_ERROR=true", "LAPILLI=" + fake}
	repo.git("checkout", "--quiet", "--detach", car2)
	rr := newRecipeRun(t, backtestHosts[2], repo)
	out, err := rr.run(append(slices.Clone(settings), job...), "--prometheus", "http://127.0.0.1:9")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "the rules of "+car2+", beside those of "+car1) {
		t.Errorf("not the train's merge beside its first parent:\n%s", out)
	}
	if given("rules", "shop.yaml") != shopRules("2h") || given("compare", "shop.yaml") != shopRules("5m") {
		t.Errorf("shop.yaml: %q beside %q", given("rules", "shop.yaml"), given("compare", "shop.yaml"))
	}
	// What the one ahead changed is on both sides: never this merge
	// request's.
	if a, b := given("rules", "api.yaml"), given("compare", "api.yaml"); a == "" || a != b {
		t.Errorf("api.yaml: %q beside %q", a, b)
	}
	if !strings.Contains(given("incidents"), "INC-9") {
		t.Errorf("the incidents aren't the first parent's: %q", given("incidents"))
	}
	if n := len(rr.host.statuses); n != 2 {
		t.Fatalf("%d statuses: %+v", n, rr.host.statuses)
	}
	for _, st := range rr.host.statuses {
		if st.SHA != source || st.Pipeline != 0 || st.Context != "lapilli/backtest" || st.URL != "https://gitlab.example.com/o/r/-/jobs/27" {
			t.Errorf("a status %+v, want on %s, no pipeline_id", st, source)
		}
	}
	if st, _ := rr.host.lastStatus(); st.State != "success" {
		t.Errorf("the status: %+v", st)
	}

	// Refused, gates or not: the checkout the source branch's head; a
	// train's commit that isn't a merge (squashed on the one ahead); a
	// merge of another head.
	squash := repo.git("commit-tree", repo.git("rev-parse", car2+"^{tree}"), "-p", car1, "-m", "squashed")
	other := repo.git("commit-tree", repo.git("rev-parse", car2+"^{tree}"), "-p", car1, "-p", repo.git("rev-parse", "ahead"), "-m", "another's")
	for _, c := range []struct {
		name, checkout, commit string
	}{
		{"the source branch's head", source, car2},
		{"a squash", squash, squash},
		{"a merge of another head", other, other},
	} {
		repo.git("checkout", "--quiet", "--detach", c.checkout)
		for _, gated := range []bool{true, false} {
			rr := newRecipeRun(t, backtestHosts[2], repo)
			s := []string{"PATHS=alerts", "LAPILLI=" + fake}
			if gated {
				s = slices.Clone(settings)
			}
			out, err := rr.run(append(append(s, job...), "CI_COMMIT_SHA="+c.commit), "--prometheus", "http://127.0.0.1:9")
			if err == nil || !strings.Contains(out, "the base, its first parent, is said of that merge alone") && !strings.Contains(out, "not of the pull request's head as the CI says") {
				t.Errorf("%s, gated %t: %v\n%s", c.name, gated, err, lastLines(out, 3))
			}
			st, n := rr.host.lastStatus()
			if gated && (n != 1 || st.State != "failed" || st.SHA != source) || !gated && n != 0 {
				t.Errorf("%s, gated %t: %d statuses, the last %+v", c.name, gated, n, st)
			}
		}
	}
	// A train's commits GitLab didn't say: refused at once.
	rr = newRecipeRun(t, backtestHosts[2], repo)
	out, err = rr.run(append(append(slices.Clone(settings), job...), "CI_MERGE_REQUEST_SOURCE_BRANCH_SHA="), "--prometheus", "http://127.0.0.1:9")
	if err == nil || !strings.Contains(out, "a merge train's pipeline whose CI_COMMIT_SHA and CI_MERGE_REQUEST_SOURCE_BRANCH_SHA aren't two commits' full hashes") {
		t.Errorf("no source head: %v\n%s", err, lastLines(out, 3))
	}
}
