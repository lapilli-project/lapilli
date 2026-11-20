package serve

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"lapilli.dev/internal/ledger"
)

// theirExpectations is the expectations file a team keeps in git, which
// lapilli serve's --expect is made from.
const theirExpectations = "# Kept by the payments team.\nexpectations:\n  - rule: Errors\n    budget: {week: 50}\n"

// draftRecipeServer is serve with changes on, the change recipes' token,
// and --expect read from a file alike the one in git; a draft of an
// agent's for Errors, accepted by an approver by the git path.
func draftRecipeServer(t *testing.T) (*Server, ledger.ExpectDraft) {
	t.Helper()
	return draftRecipeServerOf(t, theirExpectations)
}

// draftRecipeServerOf is draftRecipeServer with --expect reading served.
func draftRecipeServerOf(t *testing.T, served string) (*Server, ledger.ExpectDraft) {
	t.Helper()
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "expectations.yaml")
	os.WriteFile(file, []byte(served), 0o600)
	s, _ := changesServer(t, func(c *Config) { c.Expect = file })
	expectReadTwice(t, s)
	target, hash := s.draftTarget()
	id, err := s.l.KeepDraft(ctx, ledger.ExpectDraft{Rule: "Errors", Origin: ledger.DraftAgent, MadeBy: "bo@example.com", Agent: true,
		Text: "expectations:\n  - rule: Errors\n    budget:\n      week: 10\n", Target: target, DocHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.l.DraftByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	doc, h, whole, err := s.takenIn(d)
	if err != nil || !whole || !s.gitPath(d) {
		t.Fatalf("taken in: %v %v %v", err, whole, s.gitPath(d))
	}
	if d, err = s.l.AcceptDraft(ctx, id, ana, doc, h); err != nil {
		t.Fatal(err)
	}
	return s, d
}

// runDraftRecipe runs github.sh against lapilli serve's incidents port, the
// stand-in host and a repository, with the environment given.
func runDraftRecipe(t *testing.T, s *Server, host *prHost, bare string, env []string, more ...string) (string, error) {
	t.Helper()
	lapilli := httptest.NewServer(s.IncidentsHandler())
	defer lapilli.Close()
	hs := httptest.NewServer(host)
	defer hs.Close()
	host.url = hs.URL
	// sh is dash on Debian and Ubuntu, bash on macOS: RECIPE_SH=dash runs
	// the recipe as Debian would.
	cmd := exec.Command(cmp.Or(os.Getenv("RECIPE_SH"), "sh"), changeRecipes+"github.sh")
	cmd.Env = append(append(env, "LAPILLI_URL="+lapilli.URL, "LAPILLI_TOKEN_FILE="+tokenFile(t, changesToken), "CLONE="+bare, "RULES_PATHS=alerts",
		"GITHUB_API="+hs.URL, "GITHUB_REPO=o/r", "GITHUB_TOKEN_FILE="+tokenFile(t, "host-token-0123456789")), more...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// A draft of expectations accepted goes the git path as a rule's change
// does: the recipe writes EXPECT_FILE, and nothing else, while it is the
// file lapilli serve read, pushes a branch of Lapilli's own, opens a pull
// request, and tells Lapilli; merged, it says so. Without EXPECT_FILE it
// waits.
func TestChangeRecipeDraft(t *testing.T) {
	needTools(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	ctx := context.Background()
	s, d := draftRecipeServer(t)
	env := gitEnv(t)
	bare := remote(t, env, map[string]string{"alerts/shop.yaml": ruleFile, "alerts/expectations.yaml": theirExpectations})
	host := &prHost{t: t, kind: "github", auth: "Bearer host-token-0123456789"}
	out, err := runDraftRecipe(t, s, host, bare, env)
	if err != nil || !strings.Contains(out, "1 drafts of expectations accepted in Lapilli wait: give EXPECT_FILE") {
		t.Fatalf("without EXPECT_FILE: %v\n%s", err, out)
	}
	if got, _ := s.l.DraftByID(ctx, d.ID); got.PR != ledger.DraftReady || len(host.opened) != 0 {
		t.Fatalf("without EXPECT_FILE, handed out: %+v", got)
	}
	if out, err := runDraftRecipe(t, s, host, bare, env, "EXPECT_FILE=alerts/expectations.yaml"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got, _ := s.l.DraftByID(ctx, d.ID)
	if got.PR != ledger.DraftOpened || got.Link == "" || len(host.opened) != 1 {
		t.Fatalf("opened: %+v, %d pull requests", got, len(host.opened))
	}
	branch := "lapilli/" + s.ledgerID + "/expectations-" + strconv.FormatInt(d.ID, 10)
	if file := gitIn(t, env, bare, "show", branch+":alerts/expectations.yaml"); file != d.Document || !strings.Contains(file, "# Kept by the payments team.") ||
		!strings.Contains(file, "week: 10") || strings.Contains(file, "week: 50") {
		t.Errorf("the branch's file:\n%s", file)
	}
	if files := gitIn(t, env, bare, "diff", "--name-only", "main", branch); strings.TrimSpace(files) != "alerts/expectations.yaml" {
		t.Errorf("the branch changes %q", files)
	}
	pr, _ := json.Marshal(host.opened[0])
	for _, want := range []string{"Lapilli: expectations of Errors", "accepted by `" + ana + "`"} {
		if !strings.Contains(string(pr), want) {
			t.Errorf("the pull request has no %q: %s", want, pr)
		}
	}
	host.merged = true
	if out, err := runDraftRecipe(t, s, host, bare, env, "EXPECT_FILE=alerts/expectations.yaml"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got, _ := s.l.DraftByID(ctx, d.ID); got.PR != ledger.DraftMerged {
		t.Errorf("merged: %+v", got)
	}
}

// What a recipe is never made to write, whatever Lapilli says: a file
// that isn't EXPECT_FILE as Lapilli read it (changed since), an
// EXPECT_FILE that isn't an expectations file, what a CI system runs, or
// a document that isn't an expectations file. Each is told failed, and
// nothing is pushed.
func TestChangeRecipeDraftRefuses(t *testing.T) {
	needTools(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	ctx := context.Background()
	env := gitEnv(t)
	host := &prHost{t: t, kind: "github", auth: "Bearer host-token-0123456789"}
	for name, c := range map[string]struct {
		files  map[string]string
		expect string
		tamper string // the document, as a Lapilli someone took over hands it
		want   string
	}{
		"changed since": {map[string]string{"alerts/expectations.yaml": theirExpectations + "  - rule: Other\n    budget: {week: 1}\n"}, "alerts/expectations.yaml", "",
			"is not the expectations file lapilli serve read"},
		"not an expectations file": {map[string]string{"alerts/expectations.yaml": ruleFile}, "alerts/expectations.yaml", "",
			"EXPECT_FILE is not a file a draft is written to"},
		"a workflow handed over": {map[string]string{"alerts/expectations.yaml": theirExpectations}, "alerts/expectations.yaml",
			"on: push\njobs:\n  x:\n    runs-on: ubuntu-latest\n    steps: [{run: curl evil.example}]\n", "is not an expectations file"},
		"a second document": {map[string]string{"alerts/expectations.yaml": theirExpectations}, "alerts/expectations.yaml",
			theirExpectations + "---\napiVersion: v1\nkind: Secret\n", "is not an expectations file"},
	} {
		t.Run(name, func(t *testing.T) {
			s, d := draftRecipeServer(t)
			if c.tamper != "" {
				ledgerExec(t, s, "UPDATE expectation_drafts SET document = ? WHERE id = ?", c.tamper, d.ID)
			}
			bare := remote(t, env, c.files)
			if out, err := runDraftRecipe(t, s, host, bare, env, "EXPECT_FILE="+c.expect); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if got, _ := s.l.DraftByID(ctx, d.ID); got.PR != ledger.DraftFailed || !strings.Contains(got.Why, c.want) {
				t.Errorf("told: %+v", got)
			}
			if out := gitIn(t, env, bare, "branch", "--list", "lapilli/*"); strings.TrimSpace(out) != "" {
				t.Errorf("pushed: %s", out)
			}
		})
	}
	// What a CI system runs is refused as EXPECT_FILE, before anything.
	s, _ := draftRecipeServer(t)
	bare := remote(t, env, map[string]string{".github/workflows/x.yml": theirExpectations})
	if out, err := runDraftRecipe(t, s, host, bare, env, "EXPECT_FILE=.github/workflows/x.yml"); err == nil || !strings.Contains(out, "it is what a CI system runs") {
		t.Errorf("a workflow as EXPECT_FILE: %v\n%s", err, out)
	}
}

// A chart's ConfigMap gives lapilli serve the expectations file with a
// newline more or less at its end than the file in git it is made from (a
// |- block takes the last one off): the recipe takes the file in git as
// the one serve read, a newline at its end aside, and nothing else.
func TestChangeRecipeDraftTrailingNewline(t *testing.T) {
	needTools(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	ctx := context.Background()
	env := gitEnv(t)
	host := &prHost{t: t, kind: "github", auth: "Bearer host-token-0123456789"}
	bare := strings.TrimSuffix(theirExpectations, "\n")
	for name, c := range map[string]struct {
		served, inGit string
		opened        bool
	}{
		"served without its last newline": {bare, theirExpectations, true},
		"served with one more":            {theirExpectations + "\n", theirExpectations, true},
		"in git without it":               {theirExpectations, bare, true},
		"two more in git":                 {bare, theirExpectations + "\n", false},
		"a space before it in git":        {theirExpectations, bare + " \n", false},
		"a carriage return in git":        {theirExpectations, bare + "\r\n", false},
		"a character after it in git":     {theirExpectations, theirExpectations + "#", false},
	} {
		t.Run(name, func(t *testing.T) {
			s, d := draftRecipeServerOf(t, c.served)
			repo := remote(t, env, map[string]string{"alerts/shop.yaml": ruleFile, "alerts/expectations.yaml": c.inGit})
			if out, err := runDraftRecipe(t, s, host, repo, env, "EXPECT_FILE=alerts/expectations.yaml"); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			got, _ := s.l.DraftByID(ctx, d.ID)
			branch := "lapilli/" + s.ledgerID + "/expectations-" + strconv.FormatInt(d.ID, 10)
			if !c.opened {
				if got.PR != ledger.DraftFailed || !strings.Contains(got.Why, "is not the expectations file lapilli serve read") {
					t.Errorf("told: %+v", got)
				}
				if out := gitIn(t, env, repo, "branch", "--list", "lapilli/*"); strings.TrimSpace(out) != "" {
					t.Errorf("pushed: %s", out)
				}
				return
			}
			if got.PR != ledger.DraftOpened {
				t.Fatalf("not opened: %+v", got)
			}
			if file := gitIn(t, env, repo, "show", branch+":alerts/expectations.yaml"); file != d.Document || !strings.Contains(file, "week: 10") {
				t.Errorf("the branch's file:\n%q", file)
			}
		})
	}
}
