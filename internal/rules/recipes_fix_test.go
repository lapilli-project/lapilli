package rules

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// commitAt commits all of repo's files, dated at.
func commitAt(t *testing.T, repo string, at time.Time, msg string) {
	t.Helper()
	git(t, repo, at.UTC().Format(time.RFC3339), "add", "-A")
	git(t, repo, at.UTC().Format(time.RFC3339), "commit", "-qm", msg)
}

func headOf(t *testing.T, repo string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repo
	b, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// newRepo is a clone with the rules made on 1 October, and the files given
// beside them.
func newRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "2026-10-01T10:00:00Z", "init", "-q", "-b", "main")
	files["alerts/a.yaml"] = rulesFor("2m")
	for name, body := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(repo, name)), 0o755)
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commitAt(t, repo, time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC), "rules")
	return repo
}

// A commit is read as it is, whatever its .gitattributes say: a file
// marked export-ignore is read, one marked export-subst read unchanged,
// and a filter isn't run, not even one a git configuration outside the
// clone (an image's own, turning Git LFS on) makes required.
func TestVersionsRecipeReadsTheCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := newRepo(t, map[string]string{
		"alerts/noisy.yaml": "groups: [{name: g, rules: [{alert: Noisy, expr: vector(1)}]}]\n",
		"alerts/subst.yaml": "groups: [{name: g, rules: [{alert: Subst, expr: up == $Format:%ct$}]}]\n",
		"alerts/lfs.yaml":   "groups: [{name: g, rules: [{alert: Big, expr: up == 0}]}]\n",
		".gitattributes":    "alerts/noisy.yaml export-ignore\nalerts/subst.yaml export-subst\nalerts/lfs.yaml filter=boom\n",
	})
	dir := t.TempDir()
	conf, attrs := filepath.Join(dir, "gitconfig"), filepath.Join(dir, "attributes")
	os.WriteFile(attrs, []byte("alerts/a.yaml export-ignore\n"), 0o644)
	os.WriteFile(conf, []byte("[filter \"boom\"]\n\tsmudge = false\n\trequired = true\n[core]\n\tattributesFile = "+attrs+"\n"), 0o644)
	for _, render := range []string{"", `for f in alerts/*.yaml; do echo ---; cat "$f"; done`} {
		out := filepath.Join(t.TempDir(), "history")
		said, err := runRecipe(t, "REPO="+repo, "PATHS=alerts/", "SINCE=2026-09-01", "OUT="+out, "RENDER="+render, "GIT_CONFIG_GLOBAL="+conf)
		if err != nil {
			t.Fatalf("RENDER %q: %v: %s", render, err, said)
		}
		hs, _ := History(out)
		rs, _, err := Read(hs[0].Path)
		var got []string
		for _, r := range rs {
			got = append(got, r.Name+" "+r.Expr)
		}
		if err != nil || strings.Join(got, ", ") != "A a > 1, Big up == 0, Noisy vector(1), Subst up == $Format:%ct$" {
			t.Errorf("RENDER %q: %q %v", render, got, err)
		}
	}
}

// With RENDER, a symbolic link anywhere in the commit is refused, and one
// RENDER_PREPARE leaves; without, one among PATHS. Each is a commit that
// can't be read: skipped with UNRENDERED=skip, the reason said.
func TestVersionsRecipeLinksAnywhere(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := newRepo(t, map[string]string{"lib/files/README": "x"})
	os.Symlink("/etc/hosts", filepath.Join(repo, "lib", "files", "note"))
	os.WriteFile(filepath.Join(repo, "alerts", "a.yaml"), []byte(rulesFor("5m")), 0o644)
	commitAt(t, repo, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), "a link outside PATHS")
	base := []string{"REPO=" + repo, "PATHS=alerts/", "SINCE=2026-09-01", "UNRENDERED=skip"}
	out := filepath.Join(t.TempDir(), "rendered")
	said, err := runRecipe(t, append(base, "OUT="+out, "RENDER=cat alerts/a.yaml")...)
	if err != nil || !strings.Contains(said, "can't be rendered: it holds a symbolic link, lib/files/note. Passed over") {
		t.Errorf("with RENDER: %v: %s", err, said)
	}
	if hs, _ := History(out); len(hs) != 1 {
		t.Errorf("with RENDER: %v", hs)
	}
	out = filepath.Join(t.TempDir(), "prepared")
	said, err = runRecipe(t, append(base, "OUT="+out, "RENDER=cat alerts/a.yaml", "RENDER_PREPARE=ln -s /etc/hosts alerts/x")...)
	if !strings.Contains(said, "RENDER_PREPARE left a symbolic link, alerts/x") {
		t.Errorf("RENDER_PREPARE's link: %v: %s", err, said)
	}
	out = filepath.Join(t.TempDir(), "plain")
	if said, err := runRecipe(t, append(base, "OUT="+out)...); err != nil || strings.Contains(said, "Passed over") {
		t.Errorf("without RENDER, a link outside PATHS: %v: %s", err, said)
	}
}

// A commit more files or bytes than MAX_TREE_FILES and MAX_TREE_BYTES, as
// git says before anything is written, can't be read: skipped, saying
// why. So is one git can't read at all.
func TestVersionsRecipeTooBigOrUnreadable(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := newRepo(t, map[string]string{"docs/a": strings.Repeat("x", 4000), "docs/b": "y"})
	os.WriteFile(filepath.Join(repo, "alerts", "a.yaml"), []byte(rulesFor("5m")), 0o644)
	commitAt(t, repo, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), "five")
	for _, c := range []struct {
		env  []string
		says string
	}{
		{[]string{"RENDER=cat alerts/a.yaml", "MAX_TREE_FILES=2"}, "its files are 3 files of"},
		{[]string{"RENDER=cat alerts/a.yaml", "MAX_TREE_BYTES=3000"}, "more than MAX_TREE_FILES (20000) or MAX_TREE_BYTES (3000)"},
		{[]string{"MAX_TREE_BYTES=50"}, "can't be read: the files PATHS names are 1 files of"},
		{[]string{"MAX_TREE_FILES=x"}, "MAX_TREE_FILES x: want a number"},
	} {
		out := filepath.Join(t.TempDir(), "history")
		said, err := runRecipe(t, append([]string{"REPO=" + repo, "PATHS=alerts/", "SINCE=2026-09-01", "OUT=" + out}, c.env...)...)
		if err == nil || !strings.Contains(said, c.says) {
			t.Errorf("%v: %v, said %q, want %q", c.env, err, said, c.says)
		}
	}
	// A blob gone from the clone: git can't read the commit.
	cmd := exec.Command("git", "rev-parse", "HEAD:alerts/a.yaml")
	cmd.Dir = repo
	blob, _ := cmd.Output()
	b := strings.TrimSpace(string(blob))
	os.Remove(filepath.Join(repo, ".git", "objects", b[:2], b[2:]))
	out := filepath.Join(t.TempDir(), "history")
	said, err := runRecipe(t, "REPO="+repo, "PATHS=alerts/", "SINCE=2026-09-01", "OUT="+out, "RENDER=cat alerts/a.yaml", "UNRENDERED=skip")
	if err != nil || !strings.Contains(said, "can't be rendered: git couldn't read it") {
		t.Errorf("a blob gone: %v: %s", err, said)
	}
}

// Following, nothing a committer does stops git.sh: a commit dated after
// now is named now, and of more than MAX new commits the newest MAX are
// written. Not following, both still stop it.
func TestVersionsRecipeFollowingNeverStops(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := newRepo(t, map[string]string{})
	first := headOf(t, repo)
	for i, at := range []time.Time{time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC),
		time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)} {
		os.WriteFile(filepath.Join(repo, "alerts", "a.yaml"), []byte(rulesFor(strconv.Itoa(i+3)+"m")), 0o644)
		commitAt(t, repo, at, "commit "+strconv.Itoa(i))
	}
	out := filepath.Join(t.TempDir(), "history")
	said, err := runRecipe(t, "REPO="+repo, "PATHS=alerts/", "SINCE=2026-10-01T10:00:00Z", "AFTER="+first, "MAX=2", "OUT="+out)
	if err != nil || !strings.Contains(said, "3 commits changed PATHS since") || !strings.Contains(said, "it is named now") {
		t.Fatalf("%v: %s", err, said)
	}
	hs, err := History(out)
	if err != nil || len(hs) != 2 || forOf(t, hs[0].Path) != 4*time.Minute || forOf(t, hs[1].Path) != 5*time.Minute ||
		hs[1].At.After(time.Now()) || time.Since(hs[1].At) > time.Minute {
		t.Errorf("the newest two, the last named now: %v %v", hs, err)
	}
	for _, env := range [][]string{{"MAX=2"}, {}} {
		said, err := runRecipe(t, append([]string{"REPO=" + repo, "PATHS=alerts/", "SINCE=2026-09-01", "OUT=" + filepath.Join(t.TempDir(), "h")}, env...)...)
		if err == nil || !strings.Contains(said, "more than MAX") && !strings.Contains(said, "dated after now") {
			t.Errorf("not following, %v: %v: %s", env, err, said)
		}
	}
}

// Following, a commit dated back is named no earlier than when the clone's
// branch first held it, by the branch's reflog (what the fetch writes),
// less LATE: a commit dated within that keeps its own time.
func TestVersionsRecipeFetchedBound(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := newRepo(t, map[string]string{})
	first := headOf(t, repo)
	fetched := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	// Two commits made aside, one dated three days back, one ten minutes
	// before the fetch, then fetched onto the branch.
	git(t, repo, "2026-10-01T10:00:00Z", "checkout", "-q", "-b", "aside")
	os.WriteFile(filepath.Join(repo, "alerts", "a.yaml"), []byte(rulesFor("7m")), 0o644)
	commitAt(t, repo, fetched.Add(-72*time.Hour), "dated back")
	os.WriteFile(filepath.Join(repo, "alerts", "a.yaml"), []byte(rulesFor("8m")), 0o644)
	commitAt(t, repo, fetched.Add(-4*time.Minute), "honest")
	git(t, repo, fetched.Format(time.RFC3339), "update-ref", "-m", "fetch", "refs/heads/main", "aside")
	git(t, repo, fetched.Format(time.RFC3339), "checkout", "-q", "main")
	out := filepath.Join(t.TempDir(), "history")
	said, err := runRecipe(t, "REPO="+repo, "PATHS=alerts/", "SINCE=2026-10-01T10:00:00Z", "AFTER="+first, "LATE=360", "OUT="+out)
	if err != nil || !strings.Contains(said, "before the branch held it") {
		t.Fatalf("%v: %s", err, said)
	}
	hs, err := History(out)
	if err != nil || len(hs) != 2 {
		t.Fatalf("%v %v", hs, err)
	}
	if !hs[0].At.Equal(fetched.Add(-6*time.Minute)) || !hs[1].At.Equal(fetched.Add(-4*time.Minute)) {
		t.Errorf("named %v and %v, want %v (fetched, less LATE) and %v (its own)", hs[0].At, hs[1].At, fetched.Add(-6*time.Minute), fetched.Add(-4*time.Minute))
	}
}

// follow.sh takes away the versions older than KEEP seconds after each
// look, but the newest of them, in effect then.
func TestFollowRecipeKeep(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := newRepo(t, map[string]string{})
	at := time.Now().Add(-10 * 24 * time.Hour)
	for i := range 4 {
		os.WriteFile(filepath.Join(repo, "alerts", "a.yaml"), []byte(rulesFor(strconv.Itoa(i+3)+"m")), 0o644)
		commitAt(t, repo, at.Add(time.Duration(i)*3*24*time.Hour), "commit "+strconv.Itoa(i))
	}
	hist := filepath.Join(t.TempDir(), "history")
	said, err := runFollow(t, time.Minute, "REPO="+repo, "PATHS=alerts/", "HISTORY="+hist, "EVERY=0", "SINCE=2026-09-01", "KEEP="+strconv.Itoa(5*24*3600))
	if err != nil {
		t.Fatalf("%v: %s", err, said)
	}
	hs, err := History(hist)
	// Made 10, 7, 4 and 1 days ago: the one from 7 days ago was in effect 5
	// days ago; those before it go.
	if err != nil || len(hs) != 3 || forOf(t, hs[0].Path) != 4*time.Minute {
		t.Errorf("%v %v", hs, err)
	}
}

// follow.sh never stops on a commit either when it writes a history
// afresh: one that can't be rendered is passed over there too. Its own git
// runs with no configuration but the clone's, as git.sh's does.
func TestFollowRecipeFollowing(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := newRepo(t, map[string]string{})
	os.WriteFile(filepath.Join(repo, "alerts", "a.yaml"), []byte(rulesFor("5m")), 0o644)
	os.WriteFile(filepath.Join(repo, "alerts", "BROKEN"), []byte("x"), 0o644)
	commitAt(t, repo, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), "broken")
	bad := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(bad, []byte("[core\nthis isn't git's\n"), 0o644)
	hist := filepath.Join(t.TempDir(), "history")
	said, err := runFollow(t, time.Minute, "REPO="+repo, "PATHS=alerts/", "HISTORY="+hist, "EVERY=0", "SINCE=2026-09-01",
		"RENDER=[ ! -e alerts/BROKEN ] && cat alerts/a.yaml", "UNRENDERED=fail", "GIT_CONFIG_GLOBAL="+bad)
	if err != nil || !strings.Contains(said, "can't be rendered: RENDER failed. Passed over") {
		t.Fatalf("%v: %s", err, said)
	}
	if hs, err := History(hist); err != nil || len(hs) != 1 {
		t.Errorf("%v %v", hs, err)
	}
}
