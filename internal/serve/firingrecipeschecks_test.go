package serve

import (
	"cmp"
	"fmt"
	"net"
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

// firingRecipe is a firing recipe that sends a token with curl: one that
// asks a tool's API (api names where), or put.sh, which writes to serve at
// its first argument. token names its token's file; env is the rest it
// needs; empty an answer with nothing in it; header the header the token
// goes in, and auth what it holds for a token.
type firingRecipe struct {
	name, script, api, token string
	env                      []string
	empty                    string
	header                   string
	auth                     func(token string) string
}

// source is the source put.sh writes in these tests.
const putSource = "cloudwatch-shop"

func firingTokenRecipes(t *testing.T) []firingRecipe {
	t.Helper()
	bearer := func(t string) string { return "Bearer " + t }
	plain := func(t string) string { return t }
	since := "SINCE=2026-11-01T00:00:00Z"
	return []firingRecipe{
		{"datadog.sh, a token", "datadog.sh", "DATADOG_API", "DATADOG_TOKEN_FILE", []string{since}, `{"data": []}`, "Authorization", bearer},
		{"datadog.sh, an application key", "datadog.sh", "DATADOG_API", "DATADOG_APP_KEY_FILE",
			[]string{since, "DATADOG_TOKEN_FILE=", "DATADOG_API_KEY_FILE=" + tokenFile(t, "0a1b2c3d")}, `{"data": []}`, "DD-APPLICATION-KEY", plain},
		{"newrelic.sh", "newrelic.sh", "NEW_RELIC_API", "NEW_RELIC_API_KEY_FILE", []string{since, "NEW_RELIC_ACCOUNT_ID=1234567"},
			`{"data": {"actor": {"account": {"nrql": {"results": []}}}}}`, "API-Key", plain},
		{"google-cloud.sh", "google-cloud.sh", "GOOGLE_MONITORING_API", "GOOGLE_TOKEN_FILE", []string{since, "GOOGLE_CLOUD_PROJECT=shop"},
			`{}`, "Authorization", bearer},
		{"put.sh", "put.sh", "", "LAPILLI_TOKEN_FILE", []string{since, "UNTIL=2026-11-02T00:00:00Z"},
			`{"source": "` + putSource + `", "firings": 0, "added": 0, "removed": 0}`, "Authorization", bearer},
	}
}

// command is r run with its address url and its token in file: put.sh's
// address is its first argument, the others' a variable. RECIPE_SH names
// the shell, as the backtest recipes' tests take it: sh unless given.
func (r firingRecipe) command(url, file string, env ...string) *exec.Cmd {
	args := []string{firingRecipes + r.script}
	vars := append(append(os.Environ(), env...), r.env...)
	if r.api == "" {
		args = append(args, url, putSource)
	} else {
		vars = append(vars, r.api+"="+url)
	}
	cmd := exec.Command(cmp.Or(os.Getenv("RECIPE_SH"), "sh"), args...)
	cmd.Env = append(vars, r.token+"="+file)
	cmd.Stdin = strings.NewReader("")
	return cmd
}

// TestFiringRecipesRefuseWhereAndWhat: a firing recipe sends its token
// only over https, or to this machine for tests, and curl speaks no other
// scheme; put.sh, which writes to serve's port inside a cluster, takes
// http and https and no other. An address with a user before @ is
// refused, and so is a name that only starts with this machine's. A token
// file holding more than a token on one line is refused, as a line break
// or a carriage return in it would add header lines; so is a space or a
// tab inside it, but a space in serve's own, which serve takes. Each is
// refused before curl runs, and the refusal says neither the token nor
// the address. The scheme in capitals, this machine with a port or not,
// and an https port and path of the deployment's own are taken.
func TestFiringRecipesRefuseWhereAndWhat(t *testing.T) {
	parallel(t)
	needTools(t)
	bin := t.TempDir()
	args := filepath.Join(bin, "args")
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" >>"+args+"\ncat >/dev/null\nexit 22\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := "PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")
	run := func(r firingRecipe, url, file string) (string, string, error) {
		os.Remove(args)
		cmd := r.command(url, file, path)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}
	for _, r := range firingTokenRecipes(t) {
		good := tokenFile(t, "x")
		where, local := r.api, "http://127.0.0.1:1"
		type refused struct{ url, file, says string }
		var cases []refused
		if r.api == "" {
			where = "the URL"
			for _, u := range []string{"ftp://127.0.0.1:21", "file:///etc/hosts", "lapilli:8082", "http://", "https://", "-K/etc/hosts"} {
				cases = append(cases, refused{u, good, "the URL is not http or https"})
			}
		} else {
			for _, u := range []string{"http://example.com", "ftp://127.0.0.1:21", "HTTP://example.com", "http://localhost.evil.example",
				"http://127.0.0.1.evil.example", "http://127.0.0.10:1", "http://[::1].evil.example", "http://[::2]:1"} {
				cases = append(cases, refused{u, good, r.api + " is not https"})
			}
		}
		for _, u := range []string{"https://ana:s3cret@example.com", "http://localhost:1@example.com", "http://localhost@example.com"} {
			cases = append(cases, refused{u, good, where + " holds a user or "})
		}
		holds := []string{"first-line\nsecond-line", "first-line\r\nsecond-line\r", "first-line\rsecond-line", "first-line\tsecond-line",
			"first-line\x1bsecond-line", "", " \t\r"}
		if r.api != "" {
			holds = append(holds, "first-line second-line")
		}
		for _, held := range holds {
			cases = append(cases, refused{local, tokenFile(t, held), r.token + " doesn't hold a "})
		}
		for _, c := range cases {
			out, said, err := run(r, c.url, c.file)
			_, called := os.Stat(args)
			if err == nil || out != "" || called == nil || !strings.Contains(said, c.says) {
				t.Errorf("%s %q: %v, wrote %q, said %q, curl ran: %v", r.name, c.url, err, out, said, called == nil)
			}
			for _, secret := range []string{"s3cret", "first-line", "second-line", "example.com"} {
				if strings.Contains(said, secret) {
					t.Errorf("%s %q said what the token file or the address holds: %q", r.name, c.url, said)
				}
			}
		}
		// curl is told the scheme, in whatever case it's written, and is
		// given the address as it is, and the most it reads of an answer.
		type taken struct{ url, proto string }
		takes := []taken{
			{"https://tool.example.com", "=https"},
			{"HTTPS://tool.example.com", "=https"},
			{"hTTpS://tool.example.com", "=https"},
			{"https://tool.example.com:8443/base/path", "=https"},
			{"http://127.0.0.1:1", "=http"},
			{"HTTP://127.0.0.1:1", "=http"},
			{"http://127.0.0.1", "=http"},
			{"http://localhost", "=http"},
			{"HTTP://LOCALHOST:1", "=http"},
			{"http://[::1]", "=http"},
			{"http://[::1]:1", "=http"},
		}
		if r.api == "" {
			// serve's port, a Service in the cluster.
			takes = append(takes, taken{"http://lapilli:8082", "=http"}, taken{"HTTP://lapilli.monitoring.svc:8082", "=http"})
		}
		for _, c := range takes {
			_, said, _ := run(r, c.url, good)
			b, _ := os.ReadFile(args)
			if !strings.Contains(string(b), "-q\n") || !strings.Contains(string(b), "\n--proto\n"+c.proto+"\n") || !strings.Contains(string(b), "\n"+c.url) ||
				!strings.Contains(string(b), "\n--max-filesize\n16777216\n") {
				t.Errorf("%s %s: curl's arguments %q, want --proto %s, --max-filesize and the address; said %q", r.name, c.url, b, c.proto, said)
			}
		}
	}
}

// askedFiringStandIn runs r against a stand-in for what it asks, listening
// at listen, its address what where writes of the stand-in's port, and its
// token in file: what each request carried in the token's header, what the
// recipe wrote, and what it said. The stand-in answers answer.
func askedFiringStandIn(t *testing.T, r firingRecipe, listen string, where func(port string) string, file string, answer []byte) ([]string, string, string, error) {
	t.Helper()
	l, err := net.Listen("tcp", listen)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var sent []string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		sent = append(sent, req.Header.Get(r.header))
		mu.Unlock()
		w.Header().Set("Content-Length", strconv.Itoa(len(answer)))
		w.Write(answer)
	}))
	srv.Listener.Close()
	srv.Listener = l
	srv.Start()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	cmd := r.command(where(port), file)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	srv.Close()
	mu.Lock()
	defer mu.Unlock()
	return sent, stdout.String(), stderr.String(), err
}

// TestFiringRecipesTakeThisMachineHoweverWritten: this machine is
// 127.0.0.1, [::1] or localhost, in any case, and so is the scheme, as
// curl takes it; a recipe asks the stand-in there, with its token.
func TestFiringRecipesTakeThisMachineHoweverWritten(t *testing.T) {
	parallel(t)
	needTools(t)
	for _, f := range []struct{ name, listen, url string }{
		{"HTTP", "127.0.0.1:0", "HTTP://127.0.0.1:%s"},
		{"LocalHost", "127.0.0.1:0", "Http://LocalHost:%s"},
		{"[::1]", "[::1]:0", "http://[::1]:%s"},
		{"[::1] in HTTP", "[::1]:0", "HTTP://[::1]:%s"},
	} {
		t.Run(f.name, func(t *testing.T) {
			l, err := net.Listen("tcp", f.listen)
			if err != nil {
				t.Skipf("nothing listens at %s here: %v", f.listen, err)
			}
			l.Close()
			for _, r := range firingTokenRecipes(t) {
				sent, _, said, err := askedFiringStandIn(t, r, f.listen, func(port string) string { return fmt.Sprintf(f.url, port) }, tokenFile(t, "x"), []byte(r.empty))
				if err != nil || len(sent) == 0 || sent[0] != r.auth("x") {
					t.Errorf("%s %s: %v, sent %q, said %q", r.name, f.url, err, sent, said)
				}
			}
		})
	}
}

// TestFiringRecipesTakeTheirTokens: a token as each tool writes one (made
// up here) is taken, and sent as it is; spaces, tabs and a carriage return
// after it, as an editor may leave them, are dropped. serve's own, which
// put.sh sends, is of any characters, a space among them.
func TestFiringRecipesTakeTheirTokens(t *testing.T) {
	parallel(t)
	needTools(t)
	tokens := map[string][]string{
		"datadog.sh, a token":            {"ddpat_Ab12-cd.34~ef+56/gh=="},
		"datadog.sh, an application key": {"0a1b2c3d4e5f60718293a4b5c6d7e8f901234567"},
		"newrelic.sh":                    {"NRAK-0A1B2C3D4E5F6G7H8I9J"},
		"google-cloud.sh":                {"ya29.a0Ab_cD-12.ef~GH+5/6="},
		"put.sh":                         {"u+Ab_cD-12.ef~GH/5=0a1b", "s3rve!tok:en#of,its;own*", "a pass phrase of its own"},
	}
	for _, r := range firingTokenRecipes(t) {
		for _, token := range tokens[r.name] {
			// tokenFile ends each with a line break: "\r" is a Windows one.
			for _, after := range []string{"", "\r", "  \t", " \t\r"} {
				sent, _, said, err := askedFiringStandIn(t, r, "127.0.0.1:0", func(port string) string { return "http://127.0.0.1:" + port },
					tokenFile(t, token+after), []byte(r.empty))
				if err != nil || len(sent) == 0 || sent[0] != r.auth(token) {
					t.Errorf("%s, %q after the token: %v, sent %q, said %q", r.name, after, err, sent, said)
				}
			}
		}
	}
}

// TestFiringRecipesTakeNoAnswerOver16MiB: an answer over 16 MiB stops a
// recipe with nothing written, rather than all of it read; put.sh, of
// serve's answer, fails.
func TestFiringRecipesTakeNoAnswerOver16MiB(t *testing.T) {
	parallel(t)
	needTools(t)
	for _, r := range firingTokenRecipes(t) {
		_, out, said, err := askedFiringStandIn(t, r, "127.0.0.1:0", func(port string) string { return "http://127.0.0.1:" + port },
			tokenFile(t, "x"), padded(r.empty))
		if err == nil || out != "" {
			t.Errorf("%s: %v, wrote %d bytes, said %.200q", r.name, err, len(out), said)
		}
	}
}

// TestGoogleCloudRecipeHoldsGcloudsToken: without GOOGLE_TOKEN_FILE the
// token is what gcloud auth print-access-token writes, held as a file's
// is: its line's end dropped, and more than one line refused before the
// API is asked, rather than sent as a header of its own.
func TestGoogleCloudRecipeHoldsGcloudsToken(t *testing.T) {
	parallel(t)
	needTools(t)
	var mu sync.Mutex
	var sent []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sent = append(sent, r.Header.Clone())
		mu.Unlock()
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	for _, c := range []struct {
		name, gcloud string
		taken        bool
	}{
		{"a token", `printf 'ya29.a0Ab_cD-12\n'`, true},
		{"a token, a Windows line's end", `printf 'ya29.a0Ab_cD-12\r\n'`, true},
		{"two lines", `printf 'ya29.a0Ab_cD-12\nX-Goog-User-Project: other\n'`, false},
		{"a carriage return inside", `printf 'ya29.a0Ab_cD-12\rX-Goog-User-Project: other\n'`, false},
		{"nothing", `printf '\n'`, false},
	} {
		mu.Lock()
		sent = nil
		mu.Unlock()
		cmd := exec.Command(cmp.Or(os.Getenv("RECIPE_SH"), "sh"), firingRecipes+"google-cloud.sh")
		cmd.Env = append(os.Environ(), "PATH="+fakeCLI(t, "gcloud", c.gcloud), "GOOGLE_TOKEN_FILE=", "GOOGLE_MONITORING_API="+srv.URL,
			"GOOGLE_CLOUD_PROJECT=shop", "SINCE=2026-11-01T00:00:00Z")
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		mu.Lock()
		asked := sent
		mu.Unlock()
		if c.taken {
			if err != nil || len(asked) != 1 || asked[0].Get("Authorization") != "Bearer ya29.a0Ab_cD-12" {
				t.Errorf("%s: %v, asked %d times, said %q", c.name, err, len(asked), stderr.String())
			}
			continue
		}
		if err == nil || len(asked) != 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "gcloud auth print-access-token didn't write a token") ||
			strings.Contains(stderr.String(), "ya29") || strings.Contains(stderr.String(), "other") {
			t.Errorf("%s: %v, asked %d times, wrote %q, said %q", c.name, err, len(asked), stdout.String(), stderr.String())
		}
	}
}
