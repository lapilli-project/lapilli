package rules

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// chartRepo is a clone whose rules a chart renders: chart/rules.yaml, with
// a dependency, chart/dep.yaml, that the first commit (1 October) doesn't
// vendor and the next two (20 and 25 October) do.
func chartRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "2026-10-01T10:00:00Z", "init", "-q", "-b", "main")
	write := func(name, body string) {
		os.MkdirAll(filepath.Dir(filepath.Join(repo, name)), 0o755)
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("chart/rules.yaml", rulesFor("2m"))
	git(t, repo, "2026-10-01T10:00:00Z", "add", "-A")
	git(t, repo, "2026-10-01T10:00:00Z", "commit", "-qm", "one")
	write("chart/rules.yaml", rulesFor("15m"))
	write("chart/dep.yaml", "# the dependency\n")
	git(t, repo, "2026-10-20T08:30:00Z", "add", "-A")
	git(t, repo, "2026-10-20T08:30:00Z", "commit", "-qm", "two")
	write("chart/rules.yaml", rulesFor("30m"))
	git(t, repo, "2026-10-25T12:00:00Z", "commit", "-qam", "three")
	return repo
}

// render is RENDER for chartRepo: the rules, after the dependency, which
// fails without it, and how many commits RENDER_PREPARE has seen, by a
// file in HOME, which is kept from one commit to the next.
const render = `cat chart/dep.yaml chart/rules.yaml && echo "# prepared $(cat "$HOME/seen" 2>/dev/null | wc -l | tr -d ' ')"`

// A commit that can't be rendered, such as one that doesn't vendor a
// chart's dependency, stops the recipe and says which, unless UNRENDERED
// is skip, which passes it over and says so; RENDER_PREPARE gives each
// commit what it lacks (here as the newest commit vendors it), in a HOME
// kept from commit to commit and without the token, its output not taken
// as rules. Each version holds the commit it is.
func TestVersionsRecipeRendered(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := chartRepo(t)
	// REPO as given, relative to where the recipe runs: RENDER_PREPARE
	// has it whole.
	base := []string{"REPO=" + filepath.Base(repo), "PATHS=chart/", "SINCE=2026-10-10", "RENDER=" + render}

	out := filepath.Join(t.TempDir(), "history")
	said, err := runRecipeIn(t, filepath.Dir(repo), append(base, "OUT="+out)...)
	if err == nil || !strings.Contains(said, "can't be rendered: RENDER failed. Nothing is written; UNRENDERED=skip passes such a commit over") {
		t.Errorf("unrendered: %v: %s", err, said)
	}
	if _, err := os.Stat(out); err == nil {
		t.Errorf("wrote %s", out)
	}

	said, err = runRecipeIn(t, filepath.Dir(repo), append(base, "OUT="+out, "UNRENDERED=skip")...)
	if err != nil || !strings.Contains(said, "(20261001T100000Z) can't be rendered: RENDER failed. Passed over: the version before stays in effect") ||
		!strings.Contains(said, "1 commit that couldn't be rendered was passed over") {
		t.Fatalf("skip: %v: %s", err, said)
	}
	if hs, err := History(out); err != nil || len(hs) != 2 || !hs[0].At.Equal(time.Date(2026, 10, 20, 8, 30, 0, 0, time.UTC)) {
		t.Errorf("skip: %v %v", hs, err)
	}

	token := filepath.Join(t.TempDir(), "token")
	os.WriteFile(token, []byte("s3cret"), 0o600)
	prepared := filepath.Join(t.TempDir(), "prepared")
	home := t.TempDir()
	defer func() {
		if _, err := os.Stat(filepath.Join(home, "seen")); err == nil {
			t.Error("RENDER_PREPARE ran with the job's HOME, not one of its own")
		}
	}()
	said, err = runRecipeIn(t, filepath.Dir(repo), append(base, "OUT="+prepared, "TOKEN_FILE="+token, "HOME="+home,
		`RENDER_PREPARE=test -z "${TOKEN_FILE:-}" && echo seen >>"$HOME/seen" && echo preparing && { [ -f chart/dep.yaml ] || git -C "$REPO" show HEAD:chart/dep.yaml >chart/dep.yaml; }`)...)
	if err != nil || !strings.Contains(said, "preparing") {
		t.Fatalf("RENDER_PREPARE: %v: %s", err, said)
	}
	hs, err := History(prepared)
	if err != nil || len(hs) != 3 {
		t.Fatalf("RENDER_PREPARE: %v %v", hs, err)
	}
	for i, want := range []time.Duration{2 * time.Minute, 15 * time.Minute, 30 * time.Minute} {
		if got := forOf(t, hs[i].Path); got != want {
			t.Errorf("version %d: for %s, want %s", i, got, want)
		}
		b, _ := os.ReadFile(filepath.Join(hs[i].Path, "rules.yaml"))
		if strings.Contains(string(b), "preparing") || !strings.Contains(string(b), "# prepared "+string(rune('1'+i))) {
			t.Errorf("version %d: %s", i, b)
		}
		commit, _ := os.ReadFile(filepath.Join(hs[i].Path, ".commit"))
		cmd := exec.Command("git", "log", "-1", "--format=%H", "--before="+hs[i].At.Format(time.RFC3339))
		cmd.Dir = repo
		if want, _ := cmd.Output(); len(commit) != 41 || string(commit) != string(want) {
			t.Errorf("version %d: .commit %q, want %q", i, commit, want)
		}
	}
	// So with the files as they are, without RENDER.
	plain := filepath.Join(t.TempDir(), "plain")
	if said, err := runRecipe(t, "REPO="+repo, "PATHS=chart/", "SINCE=2026-10-10", "OUT="+plain); err != nil {
		t.Fatalf("%v: %s", err, said)
	} else if hs, _ := History(plain); len(hs) != 3 {
		t.Errorf("without RENDER: %v", hs)
	} else if b, err := os.ReadFile(filepath.Join(hs[2].Path, ".commit")); err != nil || len(b) != 41 {
		t.Errorf("without RENDER, .commit: %q %v", b, err)
	}

	for _, c := range []struct {
		env  []string
		says string
	}{
		{[]string{"RENDER_PREPARE=exit 4", "UNRENDERED=fail"}, "can't be rendered: RENDER_PREPARE failed"},
		{[]string{"RENDER=true"}, "can't be rendered: RENDER wrote nothing"},
		{[]string{"UNRENDERED=maybe"}, "want fail or skip"},
		{[]string{"RENDER=", "RENDER_PREPARE=true"}, "give RENDER too"},
	} {
		o := filepath.Join(t.TempDir(), "history")
		said, err := runRecipeIn(t, filepath.Dir(repo), append(append(base, "OUT="+o), c.env...)...)
		if err == nil || !strings.Contains(said, c.says) {
			t.Errorf("%v: %v, said %q, want %q", c.env, err, said, c.says)
		}
		if _, err := os.Stat(o); err == nil {
			t.Errorf("%v: wrote %s", c.env, o)
		}
	}
}

// With AFTER, only the commits after it are written, each named after
// SINCE, the time of the version it is: what follow.sh adds to a history.
// None is nothing to write, and no failure. A branch rewritten since goes
// on from where the two parted.
func TestVersionsRecipeAfter(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := recipeRepo(t)
	head := func() string {
		cmd := exec.Command("git", "rev-parse", "HEAD")
		cmd.Dir = repo
		b, _ := cmd.Output()
		return strings.TrimSpace(string(b))
	}
	merged := head() // the version of 25 October, 12:00
	write := func(forDur string) {
		os.WriteFile(filepath.Join(repo, "alerts", "a.yaml"), []byte(rulesFor(forDur)), 0o644)
	}
	write("45m")
	git(t, repo, "2026-10-26T09:00:00Z", "commit", "-qam", "four")
	write("50m")
	git(t, repo, "2026-10-25T11:00:00Z", "commit", "-qam", "dated back")
	os.WriteFile(filepath.Join(repo, "other.txt"), []byte("y"), 0o644)
	git(t, repo, "2026-10-27T00:00:00Z", "commit", "-qam", "not the rules")

	out := filepath.Join(t.TempDir(), "new")
	said, err := runRecipe(t, "REPO="+repo, "PATHS=alerts/", "SINCE=2026-10-25T12:00:00Z", "AFTER="+merged[:12], "OUT="+out)
	if err != nil {
		t.Fatalf("%v: %s", err, said)
	}
	hs, err := History(out)
	if err != nil || len(hs) != 2 {
		t.Fatalf("%v %v", hs, err)
	}
	if !hs[0].At.Equal(time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)) || forOf(t, hs[0].Path) != 45*time.Minute ||
		!hs[1].At.Equal(time.Date(2026, 10, 26, 9, 0, 1, 0, time.UTC)) || forOf(t, hs[1].Path) != 50*time.Minute {
		t.Errorf("versions %v", hs)
	}
	last, _ := os.ReadFile(filepath.Join(hs[1].Path, ".commit"))

	// Nothing since: said, and nothing written, without failing.
	none := filepath.Join(t.TempDir(), "none")
	said, err = runRecipe(t, "REPO="+repo, "PATHS=alerts/", "SINCE=2026-10-26T09:00:01Z", "AFTER="+strings.TrimSpace(string(last)), "OUT="+none)
	if err != nil || !strings.Contains(said, "nothing new on the branch") {
		t.Errorf("nothing new: %v: %s", err, said)
	}
	if _, err := os.Stat(none); err == nil {
		t.Errorf("wrote %s", none)
	}

	// The branch rewritten: the commits since where they parted, named
	// after the newest version.
	git(t, repo, "2026-10-28T00:00:00Z", "reset", "-q", "--hard", merged)
	write("55m")
	git(t, repo, "2026-10-20T00:00:00Z", "commit", "-qam", "rewritten, dated back")
	re := filepath.Join(t.TempDir(), "rewritten")
	said, err = runRecipe(t, "REPO="+repo, "PATHS=alerts/", "SINCE=2026-10-26T09:00:01Z", "AFTER="+strings.TrimSpace(string(last)), "OUT="+re)
	if err != nil || !strings.Contains(said, "it was rewritten") {
		t.Fatalf("rewritten: %v: %s", err, said)
	}
	if hs, err := History(re); err != nil || len(hs) != 1 || forOf(t, hs[0].Path) != 55*time.Minute ||
		!hs[0].At.Equal(time.Date(2026, 10, 26, 9, 0, 2, 0, time.UTC)) {
		t.Errorf("rewritten: %v %v", hs, err)
	}

	// A commit of another history altogether.
	git(t, repo, "2026-10-28T00:00:00Z", "checkout", "-q", "--orphan", "other")
	git(t, repo, "2026-10-28T00:00:00Z", "commit", "-qm", "elsewhere")
	other := head()
	git(t, repo, "2026-10-28T00:00:00Z", "checkout", "-qf", "main")
	for _, c := range []struct {
		after, more, says string
	}{
		{other, "", "shares no history with the branch"},
		{"--all", "", "isn't a commit in the clone"},
		{"HEAD", "", "isn't a commit in the clone"},
		{"main", "", "isn't a commit in the clone"},
		{"refs/heads/main", "", "isn't a commit in the clone"},
		{merged[:6], "", "isn't a commit in the clone"},
		{merged + "\nHEAD", "", "isn't a commit in the clone"},
		{strings.Repeat("0", 40), "", "isn't a commit in the clone"},
		{merged, "DEPLOYS=/dev/null", "give one"},
	} {
		o := filepath.Join(t.TempDir(), "history")
		env := []string{"REPO=" + repo, "PATHS=alerts/", "SINCE=2026-10-25", "AFTER=" + c.after, "OUT=" + o}
		if c.more != "" {
			env = append(env, c.more)
		}
		said, err := runRecipe(t, env...)
		if err == nil || !strings.Contains(said, c.says) {
			t.Errorf("AFTER %q: %v, said %q, want %q", c.after, err, said, c.says)
		}
	}
}

// runFollow runs recipes/versions/follow.sh with env, and gives what it
// said.
func runFollow(t *testing.T, timeout time.Duration, env ...string) (string, error) {
	t.Helper()
	script, err := filepath.Abs("../../recipes/versions/follow.sh")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", script)
	cmd.Env = append(os.Environ(), append([]string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}, env...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	return stderr.String(), err
}

// follow.sh keeps a history up to date: written whole from SINCE when it
// has no version, then at each look the commits since the newest added,
// named after it, and nothing left half written. A look that fails leaves
// the history as it was; a branch that hasn't moved isn't looked at again.
func TestFollowRecipe(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := recipeRepo(t)
	hist := filepath.Join(t.TempDir(), "history")
	base := []string{"REPO=" + repo, "PATHS=alerts/", "HISTORY=" + hist, "EVERY=0"}
	said, err := runFollow(t, time.Minute, base...)
	if err == nil || !strings.Contains(said, "give SINCE") {
		t.Errorf("no version and no SINCE: %v: %s", err, said)
	}
	base = append(base, "SINCE=2026-10-10")
	if said, err := runFollow(t, time.Minute, base...); err != nil || !strings.Contains(said, "3 versions added") {
		t.Fatalf("%v: %s", err, said)
	}
	// Dated back, on the newest version's day, then a day later.
	os.WriteFile(filepath.Join(repo, "alerts", "a.yaml"), []byte(rulesFor("50m")), 0o644)
	git(t, repo, "2026-10-25T11:00:00Z", "commit", "-qam", "dated back")
	os.WriteFile(filepath.Join(repo, "alerts", "a.yaml"), []byte(rulesFor("45m")), 0o644)
	git(t, repo, "2026-10-26T09:00:00Z", "commit", "-qam", "four")
	// What a look cut short left goes.
	os.MkdirAll(filepath.Join(hist, ".follow.cut", "v", "20261030T000000Z"), 0o755)
	if said, err := runFollow(t, time.Minute, base...); err != nil || !strings.Contains(said, "2 versions added") {
		t.Fatalf("%v: %s", err, said)
	}
	hs, err := History(hist)
	if err != nil || len(hs) != 5 || !hs[3].At.Equal(time.Date(2026, 10, 25, 12, 0, 1, 0, time.UTC)) || forOf(t, hs[3].Path) != 50*time.Minute ||
		!hs[4].At.Equal(time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)) || forOf(t, hs[4].Path) != 45*time.Minute {
		t.Fatalf("%v %v", hs, err)
	}
	if left, _ := filepath.Glob(filepath.Join(hist, ".follow*")); len(left) != 0 {
		t.Errorf("left %v", left)
	}
	if said, err := runFollow(t, time.Minute, base...); err != nil || strings.Contains(said, "added") {
		t.Errorf("nothing new: %v: %s", err, said)
	}

	// A commit that can't be rendered is passed over, saying why: following
	// doesn't stop on it, and the next look doesn't render it again.
	os.WriteFile(filepath.Join(repo, "alerts", "a.yaml"), []byte(rulesFor("55m")), 0o644)
	git(t, repo, "2026-10-27T00:00:00Z", "commit", "-qam", "five")
	tries := filepath.Join(t.TempDir(), "tries")
	failing := "RENDER=echo x >>" + tries + "; exit 3"
	if said, err := runFollow(t, time.Minute, append(base, failing)...); err != nil || !strings.Contains(said, "can't be rendered: RENDER failed. Passed over") {
		t.Errorf("a commit that can't be rendered: %v: %s", err, said)
	}
	if again, _ := History(hist); len(again) != 5 {
		t.Errorf("a commit that can't be rendered left %d versions", len(again))
	}
	if left, _ := filepath.Glob(filepath.Join(hist, ".follow*")); len(left) != 0 {
		t.Errorf("a commit that can't be rendered left %v", left)
	}
	if said, err := runFollow(t, time.Minute, append(base, failing)...); err != nil {
		t.Errorf("the next look: %v: %s", err, said)
	}
	if b, _ := os.ReadFile(tries); strings.Count(string(b), "x") != 1 {
		t.Errorf("rendered %d times, want once", strings.Count(string(b), "x"))
	}
	// A newest version that names no commit: written otherwise.
	os.Remove(filepath.Join(hs[4].Path, ".commit"))
	if said, err := runFollow(t, time.Minute, base...); err == nil || !strings.Contains(said, "names no commit") {
		t.Errorf("no .commit: %v: %s", err, said)
	}

	// Looking every second, a branch that hasn't moved is looked at once.
	calls := filepath.Join(t.TempDir(), "calls")
	fake := filepath.Join(t.TempDir(), "git.sh")
	os.WriteFile(fake, []byte("echo called >>"+calls+"\n"), 0o755)
	runFollow(t, 2500*time.Millisecond, "REPO="+repo, "PATHS=alerts/", "HISTORY="+t.TempDir(), "SINCE=2026-10-10", "EVERY=1", "GIT_SH="+fake)
	if b, _ := os.ReadFile(calls); strings.Count(string(b), "called") != 1 {
		t.Errorf("git.sh run %d times for a branch that didn't move", strings.Count(string(b), "called"))
	}
}

// The chart's containers run the recipes as they are here.
func TestChartHasFollow(t *testing.T) {
	recipe, err := os.ReadFile("../../recipes/versions/follow.sh")
	if err != nil {
		t.Fatal(err)
	}
	chart, err := os.ReadFile("../../charts/lapilli/files/versions-follow.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(recipe, chart) {
		t.Error("charts/lapilli/files/versions-follow.sh differs from recipes/versions/follow.sh: copy it")
	}
}
