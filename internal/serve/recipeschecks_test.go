package serve

import (
	"encoding/base64"
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

// tokenRecipe is an incident recipe that asks a tool's API with a token:
// the variables naming where it asks and its token's file, the rest it
// needs, an answer with nothing in it, and the Authorization it sends.
type tokenRecipe struct {
	script, api, token string
	env                []string
	empty              string
	auth               func(token string) string
}

func tokenRecipes() []tokenRecipe {
	bearer := func(t string) string { return "Bearer " + t }
	basic := func(t string) string {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte("ana@example.com:"+t))
	}
	return []tokenRecipe{
		{"pagerduty.sh", "PAGERDUTY_API", "PAGERDUTY_TOKEN_FILE", nil, `{"incidents": [], "more": false}`,
			func(t string) string { return "Token token=" + t }},
		{"incident-io.sh", "INCIDENT_IO_API", "INCIDENT_IO_TOKEN_FILE", nil, `{"incidents": [], "pagination_meta": {}}`, bearer},
		{"rootly.sh", "ROOTLY_API", "ROOTLY_TOKEN_FILE", nil, `{"data": [], "meta": {}}`, bearer},
		{"firehydrant.sh", "FIREHYDRANT_API", "FIREHYDRANT_TOKEN_FILE", nil, `{"data": [], "pagination": {}}`, bearer},
		{"grafana-irm.sh", "GRAFANA_URL", "GRAFANA_IRM_TOKEN_FILE", nil, `{"incidents": [], "cursor": {"hasMore": false}}`, bearer},
		{"jira.sh", "JIRA_URL", "JIRA_TOKEN_FILE", []string{"JIRA_EMAIL=ana@example.com", "JIRA_JQL=project = OPS"},
			`{"issues": [], "isLast": true}`, basic},
		{"compass.sh", "COMPASS_API", "COMPASS_TOKEN_FILE", []string{"COMPASS_CLOUD_ID=35273b54", "COMPASS_EMAIL=ana@example.com"},
			`{"values": [], "links": {}}`, basic},
	}
}

// TestIncidentRecipesRefuseWhereAndWhat: a recipe sends its token only
// over https, or to this machine for tests, and curl speaks no other
// scheme; an address with a user before @ is refused, and so is a name
// that only starts with this machine's. A token file holding more than a
// token on one line is refused, as a line break or a carriage return in it
// would add header lines; so is a space or a tab inside it. Each is
// refused before curl runs, and the refusal says neither the token nor the
// address. The scheme in capitals, and this machine with a port or not,
// are taken.
func TestIncidentRecipesRefuseWhereAndWhat(t *testing.T) {
	needTools(t)
	bin := t.TempDir()
	args := filepath.Join(bin, "args")
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" >>"+args+"\ncat >/dev/null\nexit 22\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := "PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")
	run := func(script string, env []string) (string, string, error) {
		os.Remove(args)
		cmd := exec.Command("sh", recipes+script)
		cmd.Env = append(append(os.Environ(), path), env...)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}
	for _, r := range tokenRecipes() {
		good := tokenFile(t, "x")
		type refused struct {
			env  []string
			says string
		}
		cases := []refused{
			{[]string{r.api + "=http://example.com", r.token + "=" + good}, r.api + " is not https"},
			{[]string{r.api + "=ftp://127.0.0.1:21", r.token + "=" + good}, r.api + " is not https"},
			{[]string{r.api + "=https://ana:s3cret@example.com", r.token + "=" + good}, r.api + " holds a user or token before @, refused"},
			{[]string{r.api + "=http://localhost:1@example.com", r.token + "=" + good}, r.api + " holds a user or token before @, refused"},
			{[]string{r.api + "=http://localhost@example.com", r.token + "=" + good}, r.api + " holds a user or token before @, refused"},
			{[]string{r.api + "=HTTP://example.com", r.token + "=" + good}, r.api + " is not https"},
			{[]string{r.api + "=http://localhost.evil.example", r.token + "=" + good}, r.api + " is not https"},
			{[]string{r.api + "=http://127.0.0.1.evil.example", r.token + "=" + good}, r.api + " is not https"},
			{[]string{r.api + "=http://127.0.0.10:1", r.token + "=" + good}, r.api + " is not https"},
			{[]string{r.api + "=http://[::1].evil.example", r.token + "=" + good}, r.api + " is not https"},
			{[]string{r.api + "=http://[::2]:1", r.token + "=" + good}, r.api + " is not https"},
			{[]string{r.api + "=http://127.0.0.1:1", r.token + "=" + tokenFile(t, "first-line\nsecond-line")}, r.token + " doesn't hold a token"},
			{[]string{r.api + "=http://127.0.0.1:1", r.token + "=" + tokenFile(t, "first-line\r\nsecond-line\r")}, r.token + " doesn't hold a token"},
			{[]string{r.api + "=http://127.0.0.1:1", r.token + "=" + tokenFile(t, "first-line\rsecond-line")}, r.token + " doesn't hold a token"},
			{[]string{r.api + "=http://127.0.0.1:1", r.token + "=" + tokenFile(t, "first-line second-line")}, r.token + " doesn't hold a token"},
			{[]string{r.api + "=http://127.0.0.1:1", r.token + "=" + tokenFile(t, "first-line\tsecond-line")}, r.token + " doesn't hold a token"},
			{[]string{r.api + "=http://127.0.0.1:1", r.token + "=" + tokenFile(t, "")}, r.token + " doesn't hold a token"},
			{[]string{r.api + "=http://127.0.0.1:1", r.token + "=" + tokenFile(t, " \t\r")}, r.token + " doesn't hold a token"},
		}
		if r.script == "jira.sh" {
			cases = append(cases,
				refused{[]string{"JIRA_URL=http://127.0.0.1:1", "JSM_API=http://example.com", r.token + "=" + good}, "JSM_API is not https"},
				refused{[]string{"JIRA_URL=http://127.0.0.1:1", "JSM_API=http://localhost.evil.example", r.token + "=" + good}, "JSM_API is not https"})
		}
		for _, c := range cases {
			out, said, err := run(r.script, append(append([]string{}, r.env...), c.env...))
			_, called := os.Stat(args)
			if err == nil || out != "" || called == nil || !strings.Contains(said, c.says) {
				t.Errorf("%s %q: %v, wrote %q, said %q, curl ran: %v", r.script, c.env, err, out, said, called == nil)
			}
			for _, secret := range []string{"s3cret", "first-line", "second-line"} {
				if strings.Contains(said, secret) {
					t.Errorf("%s %q said what the token file or the address holds: %q", r.script, c.env, said)
				}
			}
		}
		// curl is told the scheme: https for a tool, http for this machine,
		// in whatever case it's written, and is given the address as it is.
		for _, c := range []struct{ url, proto string }{
			{"https://tool.example.com", "=https"},
			{"HTTPS://tool.example.com", "=https"},
			{"hTTpS://tool.example.com", "=https"},
			{"http://127.0.0.1:1", "=http"},
			{"HTTP://127.0.0.1:1", "=http"},
			{"http://127.0.0.1", "=http"},
			{"http://127.0.0.1/", "=http"},
			{"http://localhost", "=http"},
			{"http://localhost/", "=http"},
			{"HTTP://LOCALHOST:1", "=http"},
			{"http://[::1]", "=http"},
			{"http://[::1]/", "=http"},
			{"http://[::1]:1", "=http"},
		} {
			env := append(append([]string{}, r.env...), r.api+"="+c.url, r.token+"="+good)
			_, said, _ := run(r.script, env)
			b, _ := os.ReadFile(args)
			if !strings.Contains(string(b), "-q\n") || !strings.Contains(string(b), "\n--proto\n"+c.proto+"\n") || !strings.Contains(string(b), "\n"+c.url) {
				t.Errorf("%s %s: curl's arguments %q, want --proto %s and the address; said %q", r.script, c.url, b, c.proto, said)
			}
		}
		if r.script == "jira.sh" {
			for _, jsm := range []string{"HTTPS://api.example.com", "http://[::1]", "http://localhost"} {
				_, said, _ := run(r.script, append(append([]string{}, r.env...), "JIRA_URL=http://127.0.0.1:1", "JSM_API="+jsm, r.token+"="+good))
				if _, err := os.Stat(args); err != nil {
					t.Errorf("jira.sh JSM_API=%s: curl didn't run, said %q", jsm, said)
				}
			}
		}
	}
}

// askedStandIn runs r against a stand-in for its tool, listening at
// listen, its address what where writes of the stand-in's port, and its
// token in file: the Authorization each request carried, and what the
// recipe said.
func askedStandIn(t *testing.T, r tokenRecipe, listen string, where func(port string) string, file string) ([]string, string, error) {
	t.Helper()
	l, err := net.Listen("tcp", listen)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var sent []string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		sent = append(sent, req.Header.Get("Authorization"))
		mu.Unlock()
		w.Write([]byte(r.empty))
	}))
	srv.Listener.Close()
	srv.Listener = l
	srv.Start()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	cmd := exec.Command("sh", recipes+r.script)
	cmd.Env = append(append(os.Environ(), r.env...), r.api+"="+where(port), r.token+"="+file)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	_, err = cmd.Output()
	srv.Close()
	mu.Lock()
	defer mu.Unlock()
	return sent, stderr.String(), err
}

// TestIncidentRecipesTakeThisMachineHoweverWritten: this machine is
// 127.0.0.1, [::1] or localhost, in any case, and so is the scheme, as
// curl takes it; a recipe asks the stand-in there, with its token.
func TestIncidentRecipesTakeThisMachineHoweverWritten(t *testing.T) {
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
			for _, r := range tokenRecipes() {
				sent, said, err := askedStandIn(t, r, f.listen, func(port string) string { return fmt.Sprintf(f.url, port) }, tokenFile(t, "x"))
				if err != nil || len(sent) == 0 || sent[0] != r.auth("x") {
					t.Errorf("%s %s: %v, sent %q, said %q", r.script, f.url, err, sent, said)
				}
			}
		})
	}
}

// TestIncidentRecipesTakeTheirTokens: a token as each tool writes one
// (made up here) is taken, and sent as it is; spaces, tabs and a carriage
// return after it, as an editor may leave them, are dropped.
func TestIncidentRecipesTakeTheirTokens(t *testing.T) {
	needTools(t)
	tokens := map[string]string{
		"pagerduty.sh":   "u+Ab_cD-12.ef~GH/5=",
		"incident-io.sh": "inc_Ab12-cd.34~ef+56/gh==",
		"rootly.sh":      "rootly_0a1b2c3d4e5f",
		"firehydrant.sh": "fhb-0a1b2c3d4e5f",
		"grafana-irm.sh": "glsa_Ab12Cd34Ef56_0a1b2c3d",
		// Atlassian's are its own: anything on one line with no space.
		"jira.sh":    "ATATT3xFfGF0Ab-Cd_Ef+Gh/Ij=0A1B2C3D",
		"compass.sh": "ATATT3xFfGF0Ab!Cd:Ef=0A1B2C3D",
	}
	for _, r := range tokenRecipes() {
		// tokenFile ends each with a line break: "\r" is a Windows one.
		for _, after := range []string{"", "\r", "  \t", " \t\r"} {
			token := tokens[r.script]
			sent, said, err := askedStandIn(t, r, "127.0.0.1:0", func(port string) string { return "http://127.0.0.1:" + port }, tokenFile(t, token+after))
			if err != nil || len(sent) == 0 || sent[0] != r.auth(token) {
				t.Errorf("%s, %q after the token: %v, sent %q, said %q", r.script, after, err, sent, said)
			}
		}
	}
}

// TestRootlyWaitsTenMinutesInAll: past its rate limit Rootly answers 429.
// Each is waited out as its Retry-After says, or else a minute; two
// minutes at most a wait, five waits a request and ten minutes in all a
// run, past which it stops with nothing written, though each page answers
// after its 429. sleep stands in, so the waits are counted, not waited.
func TestRootlyWaitsTenMinutesInAll(t *testing.T) {
	needTools(t)
	for _, c := range []struct {
		name string
		// answer is what the nth request is answered: a Retry-After for
		// a 429, "" for one without, "page" for the page asked.
		answer func(n int) string
		// says is what it says when it stops; "" when it doesn't.
		says, sleeps string
	}{
		{"ten minutes", func(n int) string {
			if n%2 == 0 {
				return "120"
			}
			return "page"
		}, "still says too many requests after 5 waits, 600 seconds in all", "120 120 120 120 120"},
		{"five waits", func(int) string { return "" }, "still says too many requests after 5 waits, 300 seconds in all", "60 60 60 60 60"},
		// Decimal, not octal, whatever zeros lead.
		{"leading zeros", func(n int) string {
			return []string{"08", "09", "99999999999999999999", "000", "last"}[min(n, 4)]
		}, "", "8 9 120 0"},
	} {
		var mu sync.Mutex
		asked := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			n := asked
			asked++
			mu.Unlock()
			switch a := c.answer(n); a {
			case "page":
				page, _ := strconv.Atoi(r.URL.Query().Get("page[number]"))
				fmt.Fprintf(w, `{"data": [], "meta": {"current_page": %d, "next_page": %d}}`, page, page+1)
			case "last":
				w.Write([]byte(`{"data": [], "meta": {"next_page": null}}`))
			default:
				if a != "" {
					w.Header().Set("Retry-After", a)
				}
				http.Error(w, `{"errors": [{"title": "Too many requests"}]}`, http.StatusTooManyRequests)
			}
		}))
		path, slept := fakeSleep(t)
		cmd := exec.Command("sh", recipes+"rootly.sh")
		cmd.Env = append(os.Environ(), "ROOTLY_API="+srv.URL, "ROOTLY_TOKEN_FILE="+tokenFile(t, "rootly-secret"),
			"ALERTS=no", "MAX_PAGES=10", path)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		srv.Close()
		if got := sleeps(t, slept); got != c.sleeps {
			t.Errorf("%s: slept %q, want %q (said %q)", c.name, got, c.sleeps, stderr.String())
		}
		if c.says == "" && err != nil {
			t.Errorf("%s: %v, said %q", c.name, err, stderr.String())
		}
		if c.says != "" && (err == nil || stdout.Len() != 0 || !strings.Contains(stderr.String(), c.says)) {
			t.Errorf("%s: %v, wrote %q, said %q", c.name, err, stdout.String(), stderr.String())
		}
	}
}

// padded is answer with spaces after it, past 16 MiB, its length said
// first: JSON still, which jq reads.
func padded(answer string) []byte {
	b := []byte(answer)
	for len(b) <= 16<<20 {
		b = append(b, ' ')
	}
	return b
}

// TestIncidentRecipesTakeNoAnswerOver16MiB: an answer over 16 MiB, a page
// of incidents or of an incident's alerts, stops a recipe with nothing
// written, rather than all of it read into the shell.
func TestIncidentRecipesTakeNoAnswerOver16MiB(t *testing.T) {
	needTools(t)
	run := func(name string, handler http.HandlerFunc, env func(url string) []string, script string) {
		srv := httptest.NewServer(handler)
		defer srv.Close()
		cmd := exec.Command("sh", recipes+script)
		cmd.Env = append(os.Environ(), env(srv.URL)...)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err == nil || stdout.Len() != 0 {
			t.Errorf("%s: %v, wrote %d bytes, said %.200q", name, err, stdout.Len(), stderr.String())
		}
	}
	answer := func(b []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(b)))
			w.Write(b)
		}
	}
	for _, r := range tokenRecipes() {
		run(r.script, answer(padded(r.empty)), func(u string) []string {
			return append(append([]string{}, r.env...), r.api+"="+u, r.token+"="+tokenFile(t, "x"))
		}, r.script)
	}
	// Jira Service Management's list of an incident's alerts.
	const cloud = "35273b54-3f06-40d2-880f-dd28cf8daafa"
	big := padded(`{"values": {"alertIds": []}}`)
	run("jira.sh's alerts", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rest/api/3/search/jql" {
			jiraPages(t, w, r)
			return
		}
		answer(big)(w, r)
	}, func(u string) []string {
		return []string{"JIRA_URL=" + u, "JIRA_EMAIL=ana@example.com", "JIRA_TOKEN_FILE=" + tokenFile(t, "x"),
			"JIRA_JQL=project = OPS", "JSM_API=" + u, "JSM_CLOUD_ID=" + cloud}
	}, "jira.sh")
}

// jiraPages answers Jira's search with the two pages of the recipes' test
// data.
func jiraPages(t *testing.T, w http.ResponseWriter, r *http.Request) {
	name := "jira-1.json"
	if r.URL.Query().Get("nextPageToken") != "" {
		name = "jira-2.json"
	}
	w.Write(fixture(t, name))
}

// TestJiraRefusesAPageOfAlertsAgain: Jira Service Management's incident
// API pages an incident's alerts by a startKey. One it gave before, not
// only the one just before, would go round until MAX_ALERT_REQUESTS: it
// stops the script with nothing written.
func TestJiraRefusesAPageOfAlertsAgain(t *testing.T) {
	needTools(t)
	const cloud = "35273b54-3f06-40d2-880f-dd28cf8daafa"
	var mu sync.Mutex
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/api/3/search/jql":
			jiraPages(t, w, r)
		case "/jsm/incidents/cloudId/" + cloud + "/v1/incident/10001/alert":
			key := r.URL.Query().Get("startKey")
			mu.Lock()
			keys = append(keys, key)
			mu.Unlock()
			// 2, then 3, then 2 again.
			next := map[string]string{"": "2", "2": "3", "3": "2"}[key]
			fmt.Fprintf(w, `{"values": {"alertIds": []}, "startKey": %s}`, next)
		default:
			http.Error(w, `{"errors": [{"code": "BAD_REQUEST"}]}`, http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	out, said, err := runJira(t, srv.URL, "JSM_API="+srv.URL, "JSM_CLOUD_ID="+cloud, "MAX_ALERT_REQUESTS=20")
	mu.Lock()
	defer mu.Unlock()
	if err == nil || out != "" || !strings.Contains(said, "a page of alerts it gave before") || strings.Join(keys, ",") != ",2,3" {
		t.Errorf("%v, wrote %q, said %q, asked for pages %q", err, out, said, keys)
	}
}
