package rules

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mkdirs(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(root, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// A history is the directories named by a time, the earliest first, in
// either form; a mounted volume's own entries are passed over.
func TestHistory(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "2026-10-14T16:40:31Z", "20261001T100000Z", "2026-10-20T09:00:00+09:00", ".git", "..data")
	hs, err := History(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []Then{
		{time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC), filepath.Join(dir, "20261001T100000Z")},
		{time.Date(2026, 10, 14, 16, 40, 31, 0, time.UTC), filepath.Join(dir, "2026-10-14T16:40:31Z")},
		{time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC), filepath.Join(dir, "2026-10-20T09:00:00+09:00")},
	}
	if len(hs) != len(want) {
		t.Fatalf("got %v", hs)
	}
	for i := range want {
		if !hs[i].At.Equal(want[i].At) || hs[i].Path != want[i].Path {
			t.Errorf("version %d: %v, want %v", i, hs[i], want[i])
		}
	}
}

func TestHistoryRefused(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(dir string)
		says  string
	}{
		{"not a time", func(dir string) { mkdirs(t, dir, "20261001T100000Z", "last-week") }, "last-week: a version is named by when it took effect"},
		{"a file", func(dir string) {
			os.WriteFile(filepath.Join(dir, "20261001T100000Z"), []byte("x"), 0o644)
		}, "20261001T100000Z: a version is a directory"},
		{"two names, one time", func(dir string) { mkdirs(t, dir, "20261001T100000Z", "2026-10-01T10:00:00Z") }, "named for one time"},
		{"nothing", func(string) {}, "holds no versions"},
	} {
		dir := t.TempDir()
		c.setup(dir)
		if _, err := History(dir); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v, want one saying %q", c.name, err, c.says)
		}
	}
	if _, err := History(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing directory was taken")
	}
}

// git runs git in dir with fixed dates, as a test's commits need.
func git(t *testing.T, dir, when string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+when, "GIT_COMMITTER_DATE="+when, "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func rulesFor(forDur string) string {
	return "groups:\n  - name: g\n    rules:\n      - {alert: A, expr: a > 1, for: " + forDur + "}\n"
}

// recipeRepo is a clone with history: the rules made on 1 October, their
// "for" raised on 20 October, something else changed on 21 October, and a
// branch raising it again merged on 25 October.
func recipeRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "2026-10-01T10:00:00Z", "init", "-q", "-b", "main")
	write := func(name, body string) {
		os.MkdirAll(filepath.Dir(filepath.Join(repo, name)), 0o755)
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("alerts/a.yaml", rulesFor("2m"))
	git(t, repo, "2026-10-01T10:00:00Z", "add", "-A")
	git(t, repo, "2026-10-01T10:00:00Z", "commit", "-qm", "one")
	write("alerts/a.yaml", rulesFor("15m"))
	git(t, repo, "2026-10-20T08:30:00Z", "commit", "-qam", "two")
	write("other.txt", "x")
	git(t, repo, "2026-10-21T00:00:00Z", "add", "-A")
	git(t, repo, "2026-10-21T00:00:00Z", "commit", "-qm", "three")
	git(t, repo, "2026-10-22T00:00:00Z", "checkout", "-qb", "side")
	write("alerts/a.yaml", rulesFor("30m"))
	git(t, repo, "2026-10-22T00:00:00Z", "commit", "-qam", "side")
	git(t, repo, "2026-10-25T12:00:00Z", "checkout", "-q", "main")
	git(t, repo, "2026-10-25T12:00:00Z", "merge", "-q", "--no-ff", "side", "-m", "merge")
	return repo
}

// runRecipe runs recipes/versions/git.sh with env, and gives what it said.
func runRecipe(t *testing.T, env ...string) (string, error) {
	t.Helper()
	return runRecipeIn(t, "", env...)
}

// runRecipeIn runs it in dir.
func runRecipeIn(t *testing.T, dir string, env ...string) (string, error) {
	t.Helper()
	script, err := filepath.Abs("../../recipes/versions/git.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), append([]string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}, env...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	return stderr.String(), err
}

// forOf is the "for" of the rule in a version.
func forOf(t *testing.T, path string) time.Duration {
	t.Helper()
	rs, _, err := Read(path)
	if err != nil || len(rs) != 1 {
		t.Fatalf("%s: %v %v", path, rs, err)
	}
	return rs[0].For
}

// recipes/versions/git.sh writes the versions git keeps as --rules-history
// reads them: as they stood at SINCE, then each commit on the branch that
// changed them, a merge at the merge's time; with RENDER, what it writes;
// from a shallow clone, only today's.
func TestVersionsRecipe(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := recipeRepo(t)
	out := filepath.Join(t.TempDir(), "history")
	if said, err := runRecipe(t, "REPO="+repo, "PATHS=alerts/", "SINCE=2026-10-10", "OUT="+out); err != nil {
		t.Fatalf("%v: %s", err, said)
	}
	hs, err := History(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		at   time.Time
		for_ time.Duration
	}{
		{time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC), 2 * time.Minute},
		{time.Date(2026, 10, 20, 8, 30, 0, 0, time.UTC), 15 * time.Minute},
		{time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC), 30 * time.Minute},
	}
	if len(hs) != len(want) {
		t.Fatalf("versions %v", hs)
	}
	for i, w := range want {
		if !hs[i].At.Equal(w.at) || forOf(t, hs[i].Path) != w.for_ {
			t.Errorf("version %d: %v, for %s; want %s, for %s", i, hs[i], forOf(t, hs[i].Path), w.at, w.for_)
		}
	}
	// It changed nothing in the clone.
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = repo
	if st, _ := cmd.Output(); len(st) != 0 {
		t.Errorf("the clone changed: %s", st)
	}

	// RENDER writes rules.yaml from each commit's files.
	rendered := filepath.Join(t.TempDir(), "rendered")
	if said, err := runRecipe(t, "REPO="+repo, "PATHS=alerts/", "SINCE=2026-10-10", "OUT="+rendered, "RENDER=cat alerts/a.yaml"); err != nil {
		t.Fatalf("%v: %s", err, said)
	}
	rhs, err := History(rendered)
	if err != nil || len(rhs) != 3 {
		t.Fatalf("rendered %v %v", rhs, err)
	}
	if _, err := os.Stat(filepath.Join(rhs[1].Path, "rules.yaml")); err != nil || forOf(t, rhs[1].Path) != 15*time.Minute {
		t.Errorf("rendered version: %v", err)
	}

	// A shallow clone has no history: today's rules only, and it says so.
	shallow := filepath.Join(t.TempDir(), "shallow")
	git(t, repo, "2026-10-26T00:00:00Z", "clone", "-q", "--depth", "1", "file://"+repo, shallow)
	only := filepath.Join(t.TempDir(), "only")
	said, err := runRecipe(t, "REPO="+shallow, "PATHS=alerts/", "SINCE=2026-10-10", "OUT="+only)
	if err != nil || !strings.Contains(said, "shallow clone") {
		t.Fatalf("%v: %s", err, said)
	}
	if ohs, err := History(only); err != nil || len(ohs) != 1 || forOf(t, ohs[0].Path) != 30*time.Minute {
		t.Errorf("from a shallow clone: %v %v", ohs, err)
	}
}

// What the recipe can't do, it refuses, and writes nothing.
func TestVersionsRecipeRefused(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := recipeRepo(t)
	full := t.TempDir()
	os.WriteFile(filepath.Join(full, "x"), []byte("x"), 0o644)
	for _, c := range []struct {
		name string
		env  []string
		says string
	}{
		{"a SINCE that isn't a date", []string{"SINCE=last week"}, "want a date"},
		{"a SINCE with more on another line", []string{"SINCE=2026-10-10\n--all"}, "want a date"},
		{"a path out of the clone", []string{"PATHS=../secrets"}, "isn't a path in the clone"},
		{"an option for a path", []string{"PATHS=--output=/tmp/x"}, "isn't a path in the clone"},
		{"an OUT that holds something", []string{"OUT=" + full}, "holds something"},
		{"more commits than MAX", []string{"MAX=1"}, "more than MAX"},
		{"a RENDER that fails", []string{"RENDER=exit 3"}, "RENDER failed"},
		{"not a clone", []string{"REPO=" + t.TempDir()}, "isn't a git clone"},
	} {
		out := filepath.Join(t.TempDir(), "history")
		env := append([]string{"REPO=" + repo, "PATHS=alerts/", "SINCE=2026-10-10", "OUT=" + out}, c.env...)
		said, err := runRecipe(t, env...)
		if err == nil || !strings.Contains(said, c.says) {
			t.Errorf("%s: %v, said %q, want %q", c.name, err, said, c.says)
		}
		if !strings.HasPrefix(c.name, "an OUT") {
			if _, err := os.Stat(out); err == nil {
				t.Errorf("%s: wrote %s", c.name, out)
			}
		}
	}
}

// The chart's init container runs the recipe as it is here.
func TestChartHasTheRecipe(t *testing.T) {
	recipe, err := os.ReadFile("../../recipes/versions/git.sh")
	if err != nil {
		t.Fatal(err)
	}
	chart, err := os.ReadFile("../../charts/lapilli/files/versions-git.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(recipe, chart) {
		t.Error("charts/lapilli/files/versions-git.sh differs from recipes/versions/git.sh: copy it")
	}
}

// A commit's time is what its committer's clock said: one dated back is
// still named after the version before it, so versions keep the branch's
// order. A version holding a symbolic link is refused, as is RENDER's tree
// with one; RENDER runs without the token the clone was made with.
func TestVersionsRecipeOrderLinksToken(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := recipeRepo(t)
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Committed after the merge, dated before it.
	write("alerts/a.yaml", rulesFor("45m"))
	git(t, repo, "2026-10-24T00:00:00Z", "commit", "-qam", "dated back")
	out := filepath.Join(t.TempDir(), "history")
	if said, err := runRecipe(t, "REPO="+repo, "PATHS=alerts/", "SINCE=2026-10-10", "OUT="+out); err != nil {
		t.Fatalf("%v: %s", err, said)
	}
	hs, err := History(out)
	if err != nil || len(hs) != 4 {
		t.Fatalf("versions %v %v", hs, err)
	}
	if last := hs[3]; !last.At.Equal(time.Date(2026, 10, 25, 12, 0, 1, 0, time.UTC)) || forOf(t, last.Path) != 45*time.Minute {
		t.Errorf("the commit dated back: %v, for %s; want a second after the merge, for 45m", last, forOf(t, last.Path))
	}

	// RENDER doesn't see the token.
	token := filepath.Join(t.TempDir(), "token")
	os.WriteFile(token, []byte("s3cret"), 0o600)
	rendered := filepath.Join(t.TempDir(), "rendered")
	if said, err := runRecipe(t, "REPO="+repo, "PATHS=alerts/", "SINCE=2026-10-26", "OUT="+rendered, "TOKEN_FILE="+token,
		`RENDER=test -z "${TOKEN_FILE:-}" && cat alerts/a.yaml`); err != nil {
		t.Errorf("RENDER saw TOKEN_FILE: %v: %s", err, said)
	}

	// A link among PATHS, to a file outside: refused, nothing written.
	if err := os.Symlink("/etc/hosts", filepath.Join(repo, "alerts", "hosts.yaml")); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "2026-10-27T00:00:00Z", "add", "-A")
	git(t, repo, "2026-10-27T00:00:00Z", "commit", "-qm", "a link")
	for _, render := range []string{"", "cat alerts/a.yaml"} {
		linked := filepath.Join(t.TempDir(), "linked")
		said, err := runRecipe(t, "REPO="+repo, "PATHS=alerts/", "SINCE=2026-10-26", "OUT="+linked, "RENDER="+render)
		if err == nil || !strings.Contains(said, "symbolic link") {
			t.Errorf("RENDER %q: a link was taken: %v: %s", render, err, said)
		}
		if _, err := os.Stat(linked); err == nil {
			t.Errorf("RENDER %q: wrote %s", render, linked)
		}
	}
}

// A version, or a file in one, that is a link to somewhere outside it is
// refused: a link within a version is its own business.
func TestHistoryLinks(t *testing.T) {
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "rules.yaml"), []byte(rulesFor("2m")), 0o644)
	for name, setup := range map[string]func(dir string){
		"a version that is a link": func(dir string) {
			os.Symlink(outside, filepath.Join(dir, "20261001T100000Z"))
		},
		"a file that is a link outside": func(dir string) {
			v := filepath.Join(dir, "20261001T100000Z")
			os.MkdirAll(v, 0o755)
			os.Symlink(filepath.Join(outside, "rules.yaml"), filepath.Join(v, "rules.yaml"))
		},
		"a version that links to a hidden one with a link outside": func(dir string) {
			hidden := filepath.Join(dir, ".data", "v")
			os.MkdirAll(hidden, 0o755)
			os.Symlink(filepath.Join(outside, "rules.yaml"), filepath.Join(hidden, "rules.yaml"))
			os.Symlink(hidden, filepath.Join(dir, "20261001T100000Z"))
		},
	} {
		dir := t.TempDir()
		setup(dir)
		if _, err := History(dir); err == nil || !strings.Contains(err.Error(), "link") {
			t.Errorf("%s: %v", name, err)
		}
	}
	dir := t.TempDir()
	v := filepath.Join(dir, "20261001T100000Z")
	os.MkdirAll(v, 0o755)
	os.WriteFile(filepath.Join(v, "a.yaml"), []byte(rulesFor("2m")), 0o644)
	os.Symlink("a.yaml", filepath.Join(v, "b.yaml"))
	if _, err := History(dir); err != nil {
		t.Errorf("a link within its version: %v", err)
	}
	if _, err := History(t.TempDir()); !errors.Is(err, ErrNoVersions) {
		t.Errorf("an empty directory: %v", err)
	}
}

// A commit dated after now stops the recipe, rather than naming every later
// version after it; with RENDER, a link outside PATHS is the repository's
// own business.
func TestVersionsRecipeFutureAndOtherLinks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := recipeRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("AGENTS.md", filepath.Join(repo, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	// A change to the rules too, so the commit with the link is a version.
	if err := os.WriteFile(filepath.Join(repo, "alerts", "a.yaml"), []byte(rulesFor("40m")), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "2026-10-27T00:00:00Z", "add", "-A")
	git(t, repo, "2026-10-27T00:00:00Z", "commit", "-qm", "a link elsewhere")
	rendered := filepath.Join(t.TempDir(), "rendered")
	if said, err := runRecipe(t, "REPO="+repo, "PATHS=alerts/", "SINCE=2026-10-10", "OUT="+rendered, "RENDER=cat alerts/a.yaml"); err != nil {
		t.Errorf("a link outside PATHS refused RENDER: %v: %s", err, said)
	}
	if err := os.WriteFile(filepath.Join(repo, "alerts", "a.yaml"), []byte(rulesFor("50m")), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "2099-01-01T00:00:00Z", "commit", "-qam", "from a clock gone wrong")
	out := filepath.Join(t.TempDir(), "history")
	said, err := runRecipe(t, "REPO="+repo, "PATHS=alerts/", "SINCE=2026-10-10", "OUT="+out)
	if err == nil || !strings.Contains(said, "dated after now") {
		t.Errorf("a commit from the future: %v: %s", err, said)
	}
	if _, err := os.Stat(out); err == nil {
		t.Errorf("wrote %s", out)
	}
}
