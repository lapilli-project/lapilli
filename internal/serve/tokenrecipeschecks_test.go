package serve

import (
	"cmp"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The recipes that send serve's own token: the incidents' put.sh, and the
// tracker recipes, which send a tracker's too. Each holds its requests as
// the firings' put.sh does (firingrecipeschecks_test.go).

// oneIncident is what the incidents' put.sh is given on stdin.
const oneIncident = `{"incidents": [{"id": "INC-7", "title": "Checkout down", "start": "2026-11-02T03:00:00Z", "end": "2026-11-02T03:30:00Z"}]}`

// refusedTokens are what a token file may not hold: more than one line,
// or a control character inside, as either would add a header's line or
// reach a terminal; or nothing.
var refusedTokens = []string{"first-line\nsecond-line", "first-line\r\nsecond-line\r", "first-line\rsecond-line", "first-line\tsecond-line",
	"first-line\x1bsecond-line", "", " \t\r"}

// recordingCurl puts a curl first on PATH that writes each call's
// arguments, a line each, to the file it gives, answers Lapilli's first
// page of tasks with one open task and its others with none, to -o's file
// or stdout, and fails any other request: PATH for a recipe's
// environment, and that file.
func recordingCurl(t *testing.T) (path, args string) {
	t.Helper()
	bin := t.TempDir()
	args = filepath.Join(bin, "args")
	script := `#!/bin/sh
printf '%s\n' "$@" >>` + args + `
cat >/dev/null
out=/dev/null
last=
while [ $# -gt 0 ]; do
  case $1 in -o) out=$2 ;; esac
  last=$1
  shift
done
[ "$out" != - ] || out=/dev/stdout
case $last in
  */api/v1/tasks\?linked=false*) printf '{"tasks": [{"id": 1, "title": "t", "kind": "tune", "status": "open"}]}' >"$out" ;;
  */api/v1/tasks\?*) printf '{"tasks": []}' >"$out" ;;
  *) exit 22 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return "PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), args
}

// recipeShell is the shell the recipes run with here: sh, or RECIPE_SH.
func recipeShell() string { return cmp.Or(os.Getenv("RECIPE_SH"), "sh") }

// TestIncidentPutRefusesWhereAndWhat: the incidents' put.sh sends serve's
// token to http or https, as serve's port in a cluster is reached, and
// curl speaks that scheme alone; an address of another scheme, or with a
// user before @, is refused, and so is a token file of more than one line
// or with a control character in it, each before curl runs, saying
// neither the token nor the address. A token with a space in it, which
// serve takes, is sent as it is, what an editor leaves after it dropped.
// An answer over 16 MiB fails the write.
func TestIncidentPutRefusesWhereAndWhat(t *testing.T) {
	needTools(t)
	path, args := recordingCurl(t)
	run := func(url, file string, env ...string) (string, error) {
		os.Remove(args)
		cmd := exec.Command(recipeShell(), recipes+"put.sh", url)
		cmd.Env = append(append(os.Environ(), env...), "LAPILLI_TOKEN_FILE="+file)
		cmd.Stdin = strings.NewReader(oneIncident)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	good := tokenFile(t, "x")
	type refused struct{ url, file, says string }
	var cases []refused
	for _, u := range []string{"ftp://127.0.0.1:21", "file:///etc/hosts", "lapilli:8082", "http://", "https://", "-K/etc/hosts"} {
		cases = append(cases, refused{u, good, "the URL is not http or https"})
	}
	for _, u := range []string{"https://ana:s3cret@example.com", "http://localhost:1@example.com"} {
		cases = append(cases, refused{u, good, "the URL holds a user or token before @, refused"})
	}
	for _, held := range refusedTokens {
		cases = append(cases, refused{"http://127.0.0.1:1", tokenFile(t, held), "LAPILLI_TOKEN_FILE doesn't hold a token"})
	}
	for _, c := range cases {
		said, err := run(c.url, c.file, path)
		_, called := os.Stat(args)
		if err == nil || called == nil || !strings.Contains(said, c.says) {
			t.Errorf("%q: %v, said %q, curl ran: %v", c.url, err, said, called == nil)
		}
		for _, secret := range []string{"s3cret", "first-line", "second-line", "example.com"} {
			if strings.Contains(said, secret) {
				t.Errorf("%q said what the token file or the address holds: %q", c.url, said)
			}
		}
	}
	for _, c := range []struct{ url, proto string }{
		{"http://lapilli:8082", "=http"},
		{"HTTP://lapilli.monitoring.svc:8082", "=http"},
		{"https://lapilli.example.com:8443/base", "=https"},
		{"HTTPS://lapilli.example.com", "=https"},
	} {
		said, _ := run(c.url, good, path)
		b, _ := os.ReadFile(args)
		if !strings.Contains(string(b), "-q\n") || !strings.Contains(string(b), "\n--proto\n"+c.proto+"\n") || !strings.Contains(string(b), "\n"+c.url+"/api/v1/incidents/INC-7") ||
			!strings.Contains(string(b), "\n--max-filesize\n16777216\n") {
			t.Errorf("%s: curl's arguments %q, want --proto %s, --max-filesize and the address; said %q", c.url, b, c.proto, said)
		}
	}

	// serve's own: sent as it is, a space in it too.
	var mu sync.Mutex
	var sent []string
	answer := []byte(`{}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sent = append(sent, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Length", strconv.Itoa(len(answer)))
		w.Write(answer)
	}))
	defer srv.Close()
	for _, token := range []string{"u+Ab_cD-12.ef~GH/5=0a1b", "s3rve!tok:en#of,its;own*", "a pass phrase of its own"} {
		for _, after := range []string{"", "\r", "  \t", " \t\r"} {
			mu.Lock()
			sent = nil
			mu.Unlock()
			said, err := run(srv.URL, tokenFile(t, token+after))
			mu.Lock()
			got := strings.Join(sent, "|")
			mu.Unlock()
			if err != nil || got != "Bearer "+token {
				t.Errorf("%q, %q after it: %v, sent %q, said %q", token, after, err, got, said)
			}
		}
	}
	answer = padded(`{}`)
	if said, err := run(srv.URL, good); err == nil || !strings.Contains(said, "wasn't written (failed)") {
		t.Errorf("an answer over 16 MiB: %v, said %.200q", err, said)
	}
}

// trackerRecipe is a tracker recipe: the variables naming its tracker's
// API and its token's file, what follows the address in the variable, the
// rest it needs, the header its token goes in and what it holds for a
// token; line, that its token is of any characters but a control one.
type trackerRecipe struct {
	script, api, token, path string
	env                      []string
	header                   string
	auth                     func(token string) string
	line                     bool
	made                     string
}

func trackerTokenRecipes() []trackerRecipe {
	plain := func(t string) string { return t }
	basic := func(user string) func(string) string {
		return func(t string) string { return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+t)) }
	}
	return []trackerRecipe{
		{"github.sh", "GITHUB_API", "GITHUB_TOKEN_FILE", "", []string{"GITHUB_REPO=example/alerts"}, "Authorization",
			func(t string) string { return "Bearer " + t }, false, "ghx_Ab12-cd.34~ef+56/gh=="},
		{"gitlab.sh", "GITLAB_URL", "GITLAB_TOKEN_FILE", "", []string{"GITLAB_PROJECT=example/alerts"}, "PRIVATE-TOKEN", plain, false, "glx-Ab12_cd.34Ef56"},
		{"jira.sh", "JIRA_URL", "JIRA_TOKEN_FILE", "", []string{"JIRA_EMAIL=ana@example.com", "JIRA_PROJECT=OPS"}, "Authorization",
			basic("ana@example.com"), true, "ATX3xFfGF0Ab!Cd:Ef=0A1B2C3D"},
		{"linear.sh", "LINEAR_API", "LINEAR_TOKEN_FILE", "/graphql", []string{"LINEAR_TEAM=9cfb482a-81e3-4154-b5b9-2c805e70a02d"}, "Authorization",
			plain, true, "Bearer lin_x_Ab12Cd34Ef56"},
		{"azure-boards.sh", "AZURE_DEVOPS_URL", "AZURE_DEVOPS_TOKEN_FILE", "", []string{"AZURE_DEVOPS_ORG=example", "AZURE_DEVOPS_PROJECT=alerts"},
			"Authorization", basic(""), true, "Ab12Cd34Ef56Gh78Ij90"},
		{"shortcut.sh", "SHORTCUT_API", "SHORTCUT_TOKEN_FILE", "", []string{"SHORTCUT_STATE=500000001"}, "Shortcut-Token", plain, false,
			"0a1b2c3d-4e5f-6071-8293-a4b5c6d7e8f9"},
		{"youtrack.sh", "YOUTRACK_URL", "YOUTRACK_TOKEN_FILE", "", []string{"YOUTRACK_PROJECT=OPS"}, "Authorization",
			func(t string) string { return "Bearer " + t }, true, "perm:YW5h.NDMtMQ==.Ab12Cd34Ef56"},
		{"jira-dc.sh", "JIRA_URL", "JIRA_TOKEN_FILE", "", []string{"JIRA_PROJECT=OPS"}, "Authorization",
			func(t string) string { return "Bearer " + t }, false, "MDk4NzY1NDMyMTpBYjEy+Q/z"},
	}
}

// command is r run with Lapilli at lapilli, its tracker's API at api, and
// the tokens in the files named.
func (r trackerRecipe) command(lapilli, lapilliToken, api, token string, env ...string) *exec.Cmd {
	cmd := exec.Command(recipeShell(), trackerRecipes+r.script)
	cmd.Env = append(append(append(trackerEnv(), env...), r.env...), "LAPILLI_URL="+lapilli, "LAPILLI_TOKEN_FILE="+lapilliToken,
		r.api+"="+api+r.path, r.token+"="+token)
	return cmd
}

// TestTrackerRecipesRefuseWhereAndWhat: a tracker recipe sends its
// tracker's token only over https, or to this machine for tests, and
// Lapilli's to http or https, as serve's port in a cluster is reached;
// curl speaks each address's scheme alone. An address with a user before
// @ is refused, and so is a token file of more than one line or with a
// control character in it, a space too where the token goes in a header
// as a tracker writes one. Each is refused before curl runs, saying
// neither the token nor the address.
func TestTrackerRecipesRefuseWhereAndWhat(t *testing.T) {
	needTools(t)
	path, args := recordingCurl(t)
	good := tokenFile(t, "x")
	const lapilli = "http://lapilli:8082"
	run := func(cmd *exec.Cmd) (string, error) {
		os.Remove(args)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	said := func(what, out string, err error, says string) {
		t.Helper()
		_, called := os.Stat(args)
		if err == nil || called == nil || !strings.Contains(out, says) {
			t.Errorf("%s: %v, said %q, want %q; curl ran: %v", what, err, out, says, called == nil)
		}
		for _, secret := range []string{"s3cret", "first-line", "second-line", "example.com"} {
			if strings.Contains(out, secret) {
				t.Errorf("%s said what the token file or the address holds: %q", what, out)
			}
		}
	}
	for i, r := range trackerTokenRecipes() {
		for _, u := range []string{"http://example.com", "ftp://127.0.0.1:21", "HTTP://example.com", "http://localhost.evil.example",
			"http://127.0.0.1.evil.example", "http://127.0.0.10:1", "http://[::2]:1"} {
			out, err := run(r.command(lapilli, good, u, good, path))
			said(r.script+" "+u, out, err, r.api+" is not https")
		}
		for _, u := range []string{"https://ana:s3cret@example.com", "http://localhost:1@example.com"} {
			out, err := run(r.command(lapilli, good, u, good, path))
			said(r.script+" "+u, out, err, r.api+" holds a user or token before @, refused")
		}
		holds := refusedTokens
		if !r.line {
			holds = append(append([]string{}, holds...), "first-line second-line")
		}
		for _, held := range holds {
			out, err := run(r.command(lapilli, good, "http://127.0.0.1:1", tokenFile(t, held), path))
			said(r.script+" "+strconv.Quote(held), out, err, r.token+" doesn't hold a token")
		}
		// Lapilli's side is the library's, the same for every tracker: of
		// the first alone.
		if i == 0 {
			for _, u := range []string{"ftp://127.0.0.1:21", "lapilli:8082", "http://", "file:///etc/hosts"} {
				out, err := run(r.command(u, good, "https://tracker.example", good, path))
				said("LAPILLI_URL "+u, out, err, "LAPILLI_URL is where --incidents-listen is")
			}
			out, err := run(r.command("http://ana:s3cret@lapilli:8082", good, "https://tracker.example", good, path))
			said("LAPILLI_URL with a user", out, err, "LAPILLI_URL holds a user or token before @, refused")
			for _, held := range refusedTokens {
				out, err := run(r.command(lapilli, tokenFile(t, held), "https://tracker.example", good, path))
				said("LAPILLI_TOKEN_FILE "+strconv.Quote(held), out, err, "LAPILLI_TOKEN_FILE doesn't hold a token")
			}
		}
		// curl is told each address's scheme, and the most it reads of an
		// answer: Lapilli's, then the tracker's, asked who its token is.
		for _, c := range []struct{ lapilli, lproto, api, proto string }{
			{"http://lapilli:8082", "=http", "https://tracker.example", "=https"},
			{"HTTPS://lapilli.example:8443", "=https", "HTTPS://tracker.example:8443/base", "=https"},
			{"https://lapilli.example", "=https", "http://127.0.0.1:1", "=http"},
			{"http://lapilli:8082", "=http", "HTTP://LocalHost:1", "=http"},
			{"http://lapilli:8082", "=http", "http://[::1]:1", "=http"},
		} {
			out, _ := run(r.command(c.lapilli, good, c.api, good, path))
			b, _ := os.ReadFile(args)
			calls := strings.Split(string(b), "-q\n")
			var toLapilli, toTracker string
			for _, call := range calls {
				switch {
				case strings.Contains(call, "\n"+c.lapilli+"/api/v1/tasks?"):
					toLapilli = call
				case strings.Contains(call, "\n"+c.api):
					toTracker = call
				}
			}
			if !strings.Contains(toLapilli, "--proto\n"+c.lproto+"\n") || !strings.Contains(toLapilli, "\n--max-filesize\n16777216\n") {
				t.Errorf("%s, Lapilli at %s: curl's arguments %q, want --proto %s and --max-filesize; said %q", r.script, c.lapilli, toLapilli, c.lproto, out)
			}
			if !strings.Contains(toTracker, "--proto\n"+c.proto+"\n") || !strings.Contains(toTracker, "\n--max-filesize\n16777216\n") {
				t.Errorf("%s, its tracker at %s: curl's arguments %q, want --proto %s and --max-filesize; said %q", r.script, c.api, toTracker, c.proto, out)
			}
		}
	}
}

// trackerStandIns are Lapilli, giving one open task with no issue, and a
// tracker, which answers what tracker says it does: each keeps what the
// requests it took carried in the token's header.
type trackerStandIns struct {
	lapilli, tracker *httptest.Server
	mu               sync.Mutex
	toLapilli        []string
	toTracker        []string
}

func newTrackerStandIns(t *testing.T, r trackerRecipe, page []byte, tracker http.HandlerFunc) *trackerStandIns {
	t.Helper()
	x := &trackerStandIns{}
	x.lapilli = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		x.mu.Lock()
		x.toLapilli = append(x.toLapilli, req.Header.Get("Authorization"))
		x.mu.Unlock()
		b := []byte(`{"tasks": []}`)
		if req.URL.Query().Get("linked") == "false" {
			b = page
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Write(b)
	}))
	x.tracker = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		x.mu.Lock()
		x.toTracker = append(x.toTracker, req.Header.Get(r.header))
		x.mu.Unlock()
		tracker(w, req)
	}))
	t.Cleanup(x.lapilli.Close)
	t.Cleanup(x.tracker.Close)
	return x
}

// oneTask is Lapilli's page of one open task with no issue.
const oneTask = `{"tasks": [{"id": 1, "title": "t", "kind": "tune", "status": "open"}]}`

// TestTrackerRecipesTakeTheirTokens: a token as each tracker writes one
// (made up here) is taken and sent as it is, and so is Lapilli's, a space
// in it too; spaces, tabs and a carriage return after either, as an editor
// may leave them, are dropped.
func TestTrackerRecipesTakeTheirTokens(t *testing.T) {
	needTools(t)
	const tasks = "a tasks token of its own"
	for _, r := range trackerTokenRecipes() {
		for _, after := range []string{"", "\r", " \t\r"} {
			// Refused, not failing: curl tries a 500 again, a 403 never.
			x := newTrackerStandIns(t, r, []byte(oneTask), func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, `{"message": "no"}`, http.StatusForbidden)
			})
			out, _ := r.command(x.lapilli.URL, tokenFile(t, tasks+after), x.tracker.URL, tokenFile(t, r.made+after)).CombinedOutput()
			x.mu.Lock()
			if len(x.toLapilli) == 0 || x.toLapilli[0] != "Bearer "+tasks || len(x.toTracker) == 0 || x.toTracker[0] != r.auth(r.made) {
				t.Errorf("%s, %q after the tokens: sent Lapilli %q and its tracker %q, said %q", r.script, after, x.toLapilli, x.toTracker, out)
			}
			x.mu.Unlock()
		}
	}
}

// TestTrackerRecipesTakeNoAnswerOver16MiB: an answer over 16 MiB, Lapilli's
// or the tracker's, stops a recipe, rather than all of it read.
func TestTrackerRecipesTakeNoAnswerOver16MiB(t *testing.T) {
	needTools(t)
	big := padded(`{}`)
	for _, r := range trackerTokenRecipes() {
		x := newTrackerStandIns(t, r, padded(`{"tasks": []}`), func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{}`)) })
		out, err := r.command(x.lapilli.URL, tokenFile(t, "x"), x.tracker.URL, tokenFile(t, "x")).CombinedOutput()
		if err == nil || !strings.Contains(string(out), "Lapilli didn't give the tasks") {
			t.Errorf("%s, Lapilli's answer: %v, said %.300q", r.script, err, out)
		}
		x = newTrackerStandIns(t, r, []byte(oneTask), func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(big)))
			w.Write(big)
		})
		out, err = r.command(x.lapilli.URL, tokenFile(t, "x"), x.tracker.URL, tokenFile(t, "x")).CombinedOutput()
		if err == nil || !strings.Contains(string(out), "the tracker answered 000") {
			t.Errorf("%s, its tracker's answer: %v, said %.300q", r.script, err, out)
		}
	}
}
