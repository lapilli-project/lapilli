package serve

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"lapilli.dev/internal/ledger"
)

// uncappedCurl is a directory whose curl is this machine's, given every
// argument but --max-filesize and its value: as a curl before 8.4.0 is,
// which holds to that bound no answer that doesn't say its length first.
func uncappedCurl(t *testing.T) string {
	t.Helper()
	real, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("no curl here")
	}
	dir := t.TempDir()
	script := `#!/bin/sh
n=$#
while [ "$n" -gt 0 ]; do
  a=$1
  shift
  n=$((n - 1))
  if [ "$a" = --max-filesize ]; then
    shift
    n=$((n - 1))
    continue
  fi
  set -- "$@" "$a"
done
exec '` + real + `' "$@"
`
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestRecipesHoldTheirAnswersWhateverCurl: before curl 8.4.0,
// --max-filesize held to 16 MiB only an answer that said its length
// first, and a chunked one was read whole. With a curl that holds to none,
// each recipe that reads a tool's API, a tracker's or Lapilli's tasks
// still stops past 16 MiB, writing nothing and saying so.
func TestRecipesHoldTheirAnswersWhateverCurl(t *testing.T) {
	needTools(t)
	t.Setenv("PATH", uncappedCurl(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	// No length said: Go sends an answer this long chunked.
	big := func(answer string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(padded(answer)) }))
		t.Cleanup(srv.Close)
		return srv
	}
	ran := func(name string, cmd *exec.Cmd, says ...string) {
		t.Helper()
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		said := false
		for _, s := range says {
			said = said || strings.Contains(stderr.String(), s)
		}
		if err == nil || stdout.Len() != 0 || !said {
			t.Errorf("%s: %v, wrote %d bytes, said %.300q", name, err, stdout.Len(), stderr.String())
		}
	}
	for _, r := range firingTokenRecipes(t) {
		if r.api == "" {
			continue // put.sh: serve's own answer, held by --max-filesize
		}
		ran(r.name, r.command(big(r.empty).URL, tokenFile(t, "x")), "more than 16 MiB")
	}
	for _, r := range tokenRecipes() {
		cmd := exec.Command(recipeShell(), recipes+r.script)
		cmd.Env = append(append(os.Environ(), r.env...), r.api+"="+big(r.empty).URL, r.token+"="+tokenFile(t, "x"))
		ran(r.script, cmd, "more than 16 MiB")
	}
	for name, api := range trackerAPIs {
		// Lapilli's tasks, past 16 MiB, or the tracker's answer, whichever
		// is asked first.
		srv := big(`{"tasks": []}`)
		cmd := exec.Command(recipeShell(), trackerRecipes+api.script)
		cmd.Env = append(append(trackerEnv(), api.env(srv.URL, tokenFile(t, "x"))...), "LAPILLI_URL="+srv.URL, "LAPILLI_TOKEN_FILE="+tokenFile(t, trTasksTok))
		ran(name+", Lapilli's answer", cmd, "the tracker's answer is more than 16 MiB", "Lapilli didn't give the tasks")
		// The tracker's, a task to make an issue for.
		g := newTrackerRig(t, name)
		g.put(t, ledger.Task{Title: "Made here"})
		cmd = exec.Command(recipeShell(), trackerRecipes+api.script)
		cmd.Env = append(slices.Clone(g.env), api.env(big(`{}`).URL, tokenFile(t, "x"))...)
		ran(name+", the tracker's answer", cmd, "the tracker's answer is more than 16 MiB")
	}
}

// TestRecipesTakeA2xxAlone: an answer that isn't a 2xx, a redirect among
// them, which curl doesn't follow with the token, stops a recipe with
// nothing written. google-cloud.sh took a 307 as an empty page, and the
// incident recipes that ask with -f, which fails a 4xx or a 5xx alone,
// one as a page of nothing; each exited 0.
func TestRecipesTakeA2xxAlone(t *testing.T) {
	parallel(t)
	needTools(t)
	// Sent elsewhere, with nothing in the answer.
	moved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://elsewhere.example/")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer moved.Close()
	ran := func(name string, cmd *exec.Cmd) {
		t.Helper()
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err == nil || stdout.Len() != 0 || !strings.Contains(stderr.String(), "307") {
			t.Errorf("%s: %v, wrote %q, said %.300q", name, err, stdout.String(), stderr.String())
		}
	}
	for _, r := range firingTokenRecipes(t) {
		if r.api != "" {
			ran(r.name, r.command(moved.URL, tokenFile(t, "x")))
		}
	}
	for _, r := range tokenRecipes() {
		cmd := exec.Command(recipeShell(), recipes+r.script)
		cmd.Env = append(append(os.Environ(), r.env...), r.api+"="+moved.URL, r.token+"="+tokenFile(t, "x"))
		ran(r.script, cmd)
	}
}
