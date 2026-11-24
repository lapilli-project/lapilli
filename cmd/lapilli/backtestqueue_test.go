package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/verdict"
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
// its base, the status goes on it, and the report in the step's summary.
func TestBacktestActionMergeGroup(t *testing.T) {
	parallel(t)
	needRecipeTools(t)
	prom := promStandIn(t)
	repo, _, second, bad, _ := queueRepo(t)
	main := repo.git("rev-parse", "main")
	repo.git("checkout", "--quiet", "--detach", second)
	rr := newRecipeRun(t, backtestHosts[0], repo)
	ref := queueRef("main", "7", bad)
	summary := filepath.Join(t.TempDir(), "step_summary")
	env := append(queueRun(queueEvent(t, second, main, "main", ref, "main"), ref, second), "PR=", "GITHUB_STEP_SUMMARY="+summary)
	out, err := runAction(t, rr, map[string]string{"version": "v0.32.0", "paths": "alerts", "incidents": "incidents.yaml", "prometheus-url": prom.URL,
		"args": "--tz UTC", "fail-missed": "true", "status": "true"}, env...)
	st, n := rr.host.lastStatus()
	if err == nil || !strings.Contains(out, "the rules of "+second+", beside those of "+main) || !strings.Contains(out, "caught by no rule") ||
		n != 2 || st.SHA != second || st.State != "failure" || st.Context != "lapilli/backtest" || rr.host.posts != 0 {
		t.Errorf("%v, %d statuses, the last %+v, %d posted\n%s", err, n, st, rr.host.posts, lastLines(out, 4))
	}
	if b, _ := os.ReadFile(summary); !strings.HasPrefix(string(b), verdict.SummaryLine) || !strings.Contains(string(b), "`CheckoutErrors`") {
		t.Errorf("the step's summary: %q", b)
	}
}

// In GitHub Actions, the report is added to the step's summary
// (GITHUB_STEP_SUMMARY), which the run's page shows: of a merge queue's
// run, where no comment is posted, the report its log has, line for line;
// of a pull request's, its comment but the hidden first line, posted or
// not (nothing changed). It is that Markdown and nothing else, made safe
// as the comment is. After what the step wrote there, a block of its own;
// past the 1 MiB GitHub takes of a step's summary, none, and the run is
// as it would be. Without GITHUB_STEP_SUMMARY, with one that isn't a
// file or is a symbolic link, or of another host's recipe, none is
// written. RENDER and RENDER_PREPARE, the pull request's, run without
// GitHub Actions' files a step writes to: the summary is the report alone.
func TestBacktestRecipeStepSummary(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	// The report's heading has its period to the minute, and the period
	// ends now unless it is given: two runs a minute apart would head their
	// reports differently. One period for every run, the default's length.
	to := time.Now().UTC().Truncate(time.Second)
	args := []string{"--prometheus", prom.URL, "--tz", "UTC",
		"--from", to.Add(-7 * 24 * time.Hour).Format(time.RFC3339), "--to", to.Format(time.RFC3339)}
	read := func(p string) string {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	// An empty file, as the runner makes one for each step.
	fresh := func(body string) string {
		t.Helper()
		f := filepath.Join(t.TempDir(), "step_summary")
		if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return f
	}
	const added = "the report added to the step's summary (GITHUB_STEP_SUMMARY), as the comment has it"

	repo, _, second, bad, _ := queueRepo(t)
	main := repo.git("rev-parse", "main")
	repo.git("checkout", "--quiet", "--detach", second)
	ref := queueRef("main", "7", bad)
	queue := func(settings ...string) (string, error) {
		t.Helper()
		rr := newRecipeRun(t, backtestHosts[0], repo)
		rr.env = append(rr.env, queueRun(queueEvent(t, second, main, "main", ref, "main"), ref, second)...)
		out, err := rr.run(append([]string{"PATHS=alerts", "INCIDENTS=incidents.yaml", "FAIL_MISSED=true", "STATUS=true", "PR="}, settings...), args...)
		if rr.host.posts != 0 {
			t.Errorf("a merge queue's run commented")
		}
		return out, err
	}
	summary := fresh("")
	out, err := queue("GITHUB_STEP_SUMMARY=" + summary)
	got := read(summary)
	if err == nil || !strings.Contains(out, added) || !strings.HasPrefix(got, verdict.SummaryLine) || !strings.Contains(got, "\n### Lapilli backtest, ") ||
		!strings.Contains(got, "`CheckoutErrors`") || strings.Contains(got, "lapilli-backtest") || !strings.HasSuffix(got, "\n") {
		t.Fatalf("%v, the summary:\n%s\n%s", err, got, lastLines(out, 5))
	}
	// The report as the job's log has it.
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if !strings.Contains(out, "  | "+line+"\n") {
			t.Errorf("a line of the summary isn't the log's: %q", line)
		}
	}
	// After what the step wrote there: a blank line, then the report.
	summary = fresh("## Written before\nby the step")
	if out, err := queue("GITHUB_STEP_SUMMARY=" + summary); err == nil || read(summary) != "## Written before\nby the step\n\n"+got {
		t.Errorf("after the step's own: %v\n%q\n%s", err, read(summary), lastLines(out, 3))
	}
	// Without GITHUB_STEP_SUMMARY, or with one that is empty or a
	// directory: none, and the run as it is.
	for _, settings := range [][]string{nil, {"GITHUB_STEP_SUMMARY="}, {"GITHUB_STEP_SUMMARY=" + t.TempDir()}} {
		out, err := queue(settings...)
		if err == nil || strings.Contains(out, added) || !strings.Contains(out, "  | ### Lapilli backtest, ") {
			t.Errorf("%v: %v\n%s", settings, err, lastLines(out, 3))
		}
		if len(settings) == 1 && strings.HasSuffix(settings[0], "=") == strings.Contains(out, "isn't a file the job can read") {
			t.Errorf("%v: said %t", settings, !strings.HasSuffix(settings[0], "="))
		}
	}

	// A symbolic link, to a file and to none: the report goes to neither.
	target := fresh("")
	link := filepath.Join(t.TempDir(), "step_summary")
	os.Symlink(target, link)
	dangling := filepath.Join(t.TempDir(), "step_summary")
	os.Symlink(filepath.Join(t.TempDir(), "made"), dangling)
	for name, f := range map[string]string{"a link to a file": link, "a link to none": dangling} {
		out, err := queue("GITHUB_STEP_SUMMARY=" + f)
		if err == nil || strings.Contains(out, added) || !strings.Contains(out, "GITHUB_STEP_SUMMARY is a symbolic link") {
			t.Errorf("%s: %v\n%s", name, err, lastLines(out, 3))
		}
	}
	if read(target) != "" {
		t.Errorf("written through the link: %q", read(target))
	}
	if made, err := os.Readlink(dangling); err != nil {
		t.Error(err)
	} else if _, err := os.Lstat(made); err == nil {
		t.Errorf("a file made where the dangling link leads: %s", made)
	}

	// 1 MiB a step: a report of a known size, with what the step wrote
	// there to the byte, is added; a byte more, and it isn't.
	fake := filepath.Join(t.TempDir(), "lapilli")
	report := verdict.SummaryLine + `{"nothing_changed":false,"changed":1,"unreplayed":0})` + "\n### Lapilli backtest, a report of a known size\n"
	os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s' '"+report+"'\n"), 0o755)
	room := 1<<20 - len("\n\n"+report)
	for _, c := range []struct {
		had   int
		added bool
	}{{room, true}, {room + 1, false}} {
		before := strings.Repeat("x", c.had)
		summary := fresh(before)
		rr := newRecipeRun(t, backtestHosts[0], repo)
		rr.env = append(rr.env, queueRun(queueEvent(t, second, main, "main", ref, "main"), ref, second)...)
		out, err := rr.run([]string{"PATHS=alerts", "PR=", "LAPILLI=" + fake, "GITHUB_STEP_SUMMARY=" + summary}, args...)
		want := before
		if c.added {
			want += "\n\n" + report
		}
		if err != nil || read(summary) != want || strings.Contains(out, added) != c.added ||
			!c.added && !strings.Contains(out, fmt.Sprintf("with the %d bytes the step wrote there, it would pass the 1 MiB GitHub takes of one", c.had)) {
			t.Errorf("%d bytes there: %v, %d bytes after\n%s", c.had, err, len(read(summary)), lastLines(out, 3))
		}
		if len(read(summary)) > 1<<20 {
			t.Errorf("%d bytes there: the summary is %d bytes, past 1 MiB", c.had, len(read(summary)))
		}
	}

	// A pull request's run: its comment, but its first line, posted.
	pr := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m"), "README.md": "rules\n"})
	pr.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	rr := newRecipeRun(t, backtestHosts[0], pr)
	summary = fresh("")
	if out, err := rr.run([]string{"PATHS=alerts", "GITHUB_STEP_SUMMARY=" + summary}, args...); err != nil || rr.host.posts != 1 || !strings.Contains(out, added) {
		t.Fatalf("%v, %d posted\n%s", err, rr.host.posts, lastLines(out, 3))
	}
	if _, body, _ := strings.Cut(rr.body(), "\n"); read(summary) != body || !strings.Contains(body, "`CheckoutErrors`") {
		t.Errorf("the summary isn't the comment:\n%q\n%q", read(summary), body)
	}
	// RENDER and RENDER_PREPARE, the pull request's, find none of the files
	// GitHub Actions has a step write to, to write a report of their own.
	files := map[string]string{}
	for _, v := range []string{"GITHUB_STEP_SUMMARY", "GITHUB_OUTPUT", "GITHUB_ENV", "GITHUB_PATH", "GITHUB_STATE"} {
		files[v] = fresh("")
	}
	render := filepath.Join(t.TempDir(), "render.sh")
	os.WriteFile(render, []byte(`for v in GITHUB_STEP_SUMMARY GITHUB_OUTPUT GITHUB_ENV GITHUB_PATH GITHUB_STATE; do
  eval "f=\${$v:-}"
  [ -z "$f" ] || { echo "$v reached the pull request's command" >&2; printf '### Lapilli backtest, all good\n`+"```"+`\n' >>"$f"; }
done
[ "$1" = prepare ] || cat alerts/shop.yaml
`), 0o644)
	rr = newRecipeRun(t, backtestHosts[0], pr)
	settings := []string{"RENDER=sh " + render, "RENDER_PREPARE=sh " + render + " prepare"}
	for v, f := range files {
		settings = append(settings, v+"="+f)
	}
	if out, err := rr.run(settings, args...); err != nil || rr.host.posts != 1 || strings.Contains(out, "reached the pull request's command") {
		t.Fatalf("RENDER: %v, %d posted\n%s", err, rr.host.posts, lastLines(out, 5))
	}
	if _, body, _ := strings.Cut(rr.body(), "\n"); read(files["GITHUB_STEP_SUMMARY"]) != body {
		t.Errorf("RENDER: the summary isn't the comment alone:\n%q\n%q", read(files["GITHUB_STEP_SUMMARY"]), body)
	}
	for v, f := range files {
		if v != "GITHUB_STEP_SUMMARY" && read(f) != "" {
			t.Errorf("RENDER wrote %s: %q", v, read(f))
		}
	}
	// ... and one that changes no rule, which posts none.
	same := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m"), "README.md": "rules\n"})
	same.pullRequest(map[string]string{"README.md": "more rules\n"})
	rr = newRecipeRun(t, backtestHosts[0], same)
	summary = fresh("")
	if out, err := rr.run([]string{"PATHS=alerts", "GITHUB_STEP_SUMMARY=" + summary}, args...); err != nil || rr.host.posts != 0 ||
		!strings.Contains(read(summary), `{"nothing_changed":true`) {
		t.Errorf("no rule changed: %v, %d posted, the summary %q\n%s", err, rr.host.posts, read(summary), lastLines(out, 3))
	}
	// Gitea's recipe, whose Actions show no step's summary: none.
	pr.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	rr = newRecipeRun(t, backtestHosts[1], pr)
	summary = fresh("")
	if out, err := rr.run([]string{"PATHS=alerts", "GITHUB_STEP_SUMMARY=" + summary}, args...); err != nil || read(summary) != "" || strings.Contains(out, added) {
		t.Errorf("gitea.sh: %v, the summary %q", err, read(summary))
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
	long := "release/" + strings.Repeat("x", 248)

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
		// 255 at most, as lapilli serve takes a branch from the queue's ref.
		{"a branch of 256", queueRun(queueEvent(t, second, main, long, queueRef(long, "7", bad), "main"), queueRef(long, "7", bad), second),
			"255 at most"},
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

	// A branch of 255 is taken.
	taken := newRecipeRun(t, backtestHosts[0], repo)
	long = long[:255]
	taken.env = append(taken.env, queueRun(queueEvent(t, second, main, long, queueRef(long, "7", bad), "main"), queueRef(long, "7", bad), second)...)
	if out, err := taken.run([]string{"PATHS=alerts", "PR="}, args...); err != nil || strings.Contains(out, "refused") {
		t.Errorf("a branch of 255: %v\n%s", err, lastLines(out, 3))
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
// report where a pull request's would be commented, and adds it to the
// step's summary; the status goes on the head. A group serve says a newer
// one took the place of is said so, the merge queue having built it again.
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
	summary := filepath.Join(t.TempDir(), "step_summary")
	out, err := rr.run([]string{"PATHS=alerts", "STATUS=true", "PR=", "GITHUB_STEP_SUMMARY=" + summary}, "--since", "14d")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if b, _ := os.ReadFile(summary); string(b) != markdown {
		t.Errorf("the step's summary isn't serve's report:\n%q\n%q", b, markdown)
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
	// Superseded: the group built again. Held to the incidents, the job
	// fails, its status a failure on this group's head.
	srv.answers([]serveAnswer{{202, nil, `{"id":"` + serveID + `","key":"` + serveKey + `","position":1,"requests":80}`}},
		[]serveAnswer{serveState(200, map[string]any{"state": "superseded"})})
	rr = serveRun(t, backtestHosts[0], repo, srv)
	rr.env = append(rr.env, queueRun(queueEvent(t, second, main, "main", ref, "main"), ref, second)...)
	summary = filepath.Join(t.TempDir(), "step_summary")
	out, err = rr.run([]string{"PATHS=alerts", "STATUS=true", "FAIL_MISSED=true", "PR=", "GITHUB_STEP_SUMMARY=" + summary}, "--since", "14d")
	b, _ := os.ReadFile(summary)
	why := "the merge queue built this pull request's group again: the newer group's backtest took this one's place in lapilli serve's queue"
	if st, _ := rr.host.lastStatus(); err == nil || st.State != "failure" || st.SHA != second || rr.host.posts != 0 {
		t.Errorf("superseded: %v, the status %+v\n%s", err, st, lastLines(out, 4))
	}
	if block := failure(t, "[//]: # (lapilli-backtest default)\n"+string(b), why); block == "" {
		t.Errorf("the summary: %q", b)
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
