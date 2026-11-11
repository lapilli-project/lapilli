package rules

import (
	"bytes"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func needDeployTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"git", "jq", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
}

// rev is a commit of the repo, by its message.
func rev(t *testing.T, repo, msg string) string {
	t.Helper()
	cmd := exec.Command("git", "log", "--format=%H", "--grep=^"+msg+"$", "-n", "1", "main")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil || len(bytes.TrimSpace(out)) != 40 {
		t.Fatalf("commit %q: %v %s", msg, err, out)
	}
	return string(bytes.TrimSpace(out))
}

// deployFixture is a deploy history from recipes/versions/testdata, with
// the repo's own commits in place of REV_ONE, REV_THREE and REV_MERGE.
func deployFixture(t *testing.T, repo, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("../../recipes/versions/testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	merge := rev(t, repo, "merge")
	return []byte(strings.NewReplacer("REV_ONE", rev(t, repo, "one"), "REV_THREE", rev(t, repo, "three"),
		"REV_MERGE12", merge[:12], "REV_MERGE", merge).Replace(string(b)))
}

// runDeploys runs argocd.sh or flux.sh with stdin and env, and gives what
// it wrote and said.
func runDeploys(t *testing.T, script string, stdin []byte, env []string, args ...string) (string, string, error) {
	t.Helper()
	path, err := filepath.Abs("../../recipes/versions/" + script)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", append([]string{path}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	return out.String(), errb.String(), err
}

// argocd.sh and flux.sh write when each revision was deployed, from each
// tool's documented shapes; git.sh with DEPLOYS names each version by the
// first deploy holding it: the rules raised to 15m on 20 October went out
// with the deploy of 21 October 09:00, the merge of 25 October on 26
// October; the rules from before SINCE, which the earliest deploy known
// already held, keep their commit's time.
func TestVersionsRecipeDeploys(t *testing.T) {
	needDeployTools(t)
	repo := recipeRepo(t)
	one, three, merge := rev(t, repo, "one"), rev(t, repo, "three"), rev(t, repo, "merge")
	want := one + " 2026-10-01T12:00:00Z\n" + three + " 2026-10-21T09:00:00Z\n" + merge + " 2026-10-26T08:00:00Z\n"
	for _, c := range []struct {
		script, file string
		want         string
	}{
		{"argocd.sh", "argocd-application.json", want},
		{"flux.sh", "flux-kustomization.json", want},
		{"flux.sh", "flux-kustomization-2.6.json", merge + " 2026-10-26T08:00:00Z\n"},
		{"flux.sh", "flux-helmrelease.json", merge[:12] + " 2026-10-26T08:00:00Z\n"},
	} {
		args := []string{}
		if c.script == "argocd.sh" {
			args = append(args, "-")
		}
		got, said, err := runDeploys(t, c.script, deployFixture(t, repo, c.file), nil, args...)
		if err != nil || got != c.want {
			t.Errorf("%s %s: %v %s\n%s\nwant\n%s", c.script, c.file, err, said, got, c.want)
		}
	}

	deploys := filepath.Join(t.TempDir(), "deploys.txt")
	os.WriteFile(deploys, []byte(want), 0o644)
	out := filepath.Join(t.TempDir(), "history")
	said, err := runRecipe(t, "REPO="+repo, "PATHS=alerts/", "SINCE=2026-10-10", "OUT="+out, "DEPLOYS="+deploys)
	if err != nil {
		t.Fatalf("%v: %s", err, said)
	}
	hs, err := History(out)
	if err != nil {
		t.Fatal(err)
	}
	wantAt := []struct {
		at   time.Time
		for_ time.Duration
	}{
		{time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC), 2 * time.Minute},
		{time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC), 15 * time.Minute},
		{time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC), 30 * time.Minute},
	}
	if len(hs) != len(wantAt) {
		t.Fatalf("versions %v: %s", hs, said)
	}
	for i, w := range wantAt {
		if !hs[i].At.Equal(w.at) || forOf(t, hs[i].Path) != w.for_ {
			t.Errorf("version %d: %v, for %s; want %s, for %s", i, hs[i], forOf(t, hs[i].Path), w.at, w.for_)
		}
	}
	if !strings.Contains(said, "1 commits were deployed before the earliest deploy known") {
		t.Errorf("didn't say which keep their commit's time: %s", said)
	}

	// The merge not deployed yet: passed over, and said; a deploy of a
	// revision the clone doesn't have, passed over, and said.
	os.WriteFile(deploys, []byte(one+" 2026-10-01T12:00:00Z\n"+three+" 2026-10-21T09:00:00Z\n0123456789abcdef0123 2026-10-22T00:00:00Z\n"), 0o644)
	out2 := filepath.Join(t.TempDir(), "history")
	said, err = runRecipe(t, "REPO="+repo, "PATHS=alerts/", "SINCE=2026-10-10", "OUT="+out2, "DEPLOYS="+deploys)
	if err != nil {
		t.Fatalf("%v: %s", err, said)
	}
	if hs, err := History(out2); err != nil || len(hs) != 2 || !hs[1].At.Equal(wantAt[1].at) {
		t.Errorf("with the merge not deployed: %v %v", hs, err)
	}
	if !strings.Contains(said, "1 commits that changed PATHS aren't in a deploy yet") || !strings.Contains(said, "1 deploys of revisions this clone doesn't have") {
		t.Errorf("didn't say what was passed over: %s", said)
	}

	// Several commits in one deploy are one version, the last of them.
	os.WriteFile(deploys, []byte(one+" 2026-10-01T12:00:00Z\n"+merge+" 2026-10-26T08:00:00Z\n"), 0o644)
	out3 := filepath.Join(t.TempDir(), "history")
	if said, err := runRecipe(t, "REPO="+repo, "PATHS=alerts/", "SINCE=2026-10-10", "OUT="+out3, "DEPLOYS="+deploys); err != nil {
		t.Fatalf("%v: %s", err, said)
	}
	if hs, err := History(out3); err != nil || len(hs) != 2 || !hs[1].At.Equal(wantAt[2].at) || forOf(t, hs[1].Path) != 30*time.Minute {
		t.Errorf("two commits in one deploy: %v %v", hs, err)
	}

	// DEPLOYS_KEEP: what was written before is kept, each revision once,
	// at its earliest.
	kept := filepath.Join(t.TempDir(), "kept.txt")
	os.WriteFile(kept, []byte(one+" 2026-09-30T00:00:00Z\n"+"abcdef0123 2026-09-01T00:00:00Z\n"), 0o644)
	got, said, err := runDeploys(t, "argocd.sh", deployFixture(t, repo, "argocd-application.json"), []string{"DEPLOYS_KEEP=" + kept}, "-")
	wantKept := "abcdef0123 2026-09-01T00:00:00Z\n" + one + " 2026-09-30T00:00:00Z\n" + three + " 2026-10-21T09:00:00Z\n" + merge + " 2026-10-26T08:00:00Z\n"
	if err != nil || got != wantKept {
		t.Errorf("DEPLOYS_KEEP: %v %s\n%s\nwant\n%s", err, said, got, wantKept)
	}
}

// What the deploy recipes and git.sh refuse, as an attacker writing a
// deploy history might try: a revision that is an option or a path, a time
// git would guess at, a deploy dated ahead, an answer that isn't the
// tool's; Argo CD only over https, its names checked, its token never on
// a command line, no redirect followed.
func TestVersionsRecipeDeploysRefused(t *testing.T) {
	needDeployTools(t)
	repo := recipeRepo(t)
	for name, line := range map[string]string{
		"an option":           "--output=/tmp/x 2026-10-01T12:00:00Z",
		"a path":              "../../etc 2026-10-01T12:00:00Z",
		"a name git guesses":  "HEAD 2026-10-01T12:00:00Z",
		"a time git guesses":  rev(t, repo, "one") + " yesterday",
		"a time with a zone":  rev(t, repo, "one") + " 2026-10-01T12:00:00+09:00",
		"shell":               rev(t, repo, "one") + " $(touch x)",
		"ahead of now":        rev(t, repo, "merge") + " 2099-01-01T00:00:00Z",
		"a second line in it": rev(t, repo, "one") + " 2026-10-01T12:00:00Z extra",
	} {
		deploys := filepath.Join(t.TempDir(), "deploys.txt")
		os.WriteFile(deploys, []byte(line+"\n"), 0o644)
		out := filepath.Join(t.TempDir(), "history")
		said, err := runRecipe(t, "REPO="+repo, "PATHS=alerts/", "SINCE=2026-10-10", "OUT="+out, "DEPLOYS="+deploys)
		if err == nil {
			t.Errorf("%s: taken: %s", name, said)
		}
		if _, err := os.Stat(out); err == nil {
			t.Errorf("%s: wrote OUT", name)
		}
	}

	// What isn't the tool's is refused, and a revision or time that isn't
	// one passed over.
	if _, said, err := runDeploys(t, "flux.sh", []byte(`{"kind":"Deployment"}`), nil); err == nil || !strings.Contains(said, "not a Flux") {
		t.Errorf("a Deployment: %v %s", err, said)
	}
	if _, said, err := runDeploys(t, "argocd.sh", []byte(`<html>sign in</html>`), nil, "-"); err == nil || !strings.Contains(said, "not an Argo CD Application") {
		t.Errorf("a sign-in page: %v %s", err, said)
	}
	odd := `{"kind":"Application","status":{"history":[{"revision":"--upload-pack=x","deployedAt":"2026-10-01T12:00:00Z"},
		{"revision":"abcdef0","deployedAt":"$(id)"},{"revision":"1.4.0","deployedAt":"2026-10-01T12:00:00Z"}]}}`
	if got, _, err := runDeploys(t, "argocd.sh", []byte(odd), nil, "-"); err != nil || got != "" {
		t.Errorf("revisions and times that aren't: %v %q", err, got)
	}
	big := append([]byte(`{"kind":"Kustomization","x":"`), bytes.Repeat([]byte("a"), 4<<20)...)
	if _, said, err := runDeploys(t, "flux.sh", append(big, `"}`...), nil); err == nil || !strings.Contains(said, "over 4 MB") {
		t.Errorf("over 4 MB: %v %s", err, said)
	}

	// Argo CD's API: the token in a header, from stdin; https only; names
	// checked before a request; a redirect not followed.
	var asked []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.RequestURI())
		if r.Header.Get("Authorization") != "Bearer argo-token" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/v1/applications/moved" {
			http.Redirect(w, r, "https://elsewhere.example/api/v1/applications/alerts", http.StatusFound)
			return
		}
		b := deployFixture(t, repo, "argocd-application.json")
		w.Write(bytes.Replace(b, []byte(`"metadata"`), []byte(`"spec2": {}, "metadata"`), 1)) // Argo CD's API leaves out kind
	}))
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o644)
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("argo-token\n"), 0o600)
	env := func(more ...string) []string {
		return append([]string{"CURL_CA_BUNDLE=" + ca, "SSL_CERT_FILE=" + ca, "ARGOCD_URL=" + srv.URL, "ARGOCD_TOKEN_FILE=" + tok, "ARGOCD_APP=alerts"}, more...)
	}
	got, said, err := runDeploys(t, "argocd.sh", nil, env("ARGOCD_APP_NAMESPACE=monitoring", "ARGOCD_PROJECT=platform"))
	if err != nil || strings.Count(got, "\n") != 3 {
		t.Fatalf("from Argo CD's API: %v %s %q", err, said, got)
	}
	if len(asked) != 1 || asked[0] != "/api/v1/applications/alerts?appNamespace=monitoring&project=platform" {
		t.Errorf("asked %q", asked)
	}
	asked = nil
	for name, e := range map[string][]string{
		"http":                {"ARGOCD_URL=" + strings.Replace(srv.URL, "https:", "http:", 1)},
		"a name with a path":  {"ARGOCD_APP=alerts/../x"},
		"a name with a query": {"ARGOCD_APP=alerts?x=1"},
		"a namespace":         {"ARGOCD_APP_NAMESPACE=Mon itoring"},
		"a user in the URL":   {"ARGOCD_URL=https://user@" + strings.TrimPrefix(srv.URL, "https://")},
	} {
		if _, said, err := runDeploys(t, "argocd.sh", nil, env(e...)); err == nil || name == "http" && !strings.Contains(said, "must be https") {
			t.Errorf("%s: %v: %s", name, err, said)
		}
	}
	if len(asked) != 0 {
		t.Errorf("asked before refusing: %q", asked)
	}
	if _, said, err := runDeploys(t, "argocd.sh", nil, env("ARGOCD_APP=moved")); err == nil || len(asked) != 1 {
		t.Errorf("a redirect: %v %s, asked %q", err, said, asked)
	}
	// The token never on a command line: it is read from the file.
	b, _ := os.ReadFile("../../recipes/versions/argocd.sh")
	if bytes.Contains(b, []byte("-H \"Authorization")) || !bytes.Contains(b, []byte("-H @-")) {
		t.Error("argocd.sh gives the token on a command line")
	}
}
