package serve

import (
	"encoding/base64"
	"fmt"
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
// scheme; an address with a user before @ is refused. A token file holding
// more than a token on one line is refused, as a line break or a carriage
// return in it would add header lines. Each is refused before curl runs,
// and the refusal says neither the token nor the address.
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
			{[]string{r.api + "=http://127.0.0.1:1", r.token + "=" + tokenFile(t, "first-line\nsecond-line")}, r.token + " doesn't hold a token"},
			{[]string{r.api + "=http://127.0.0.1:1", r.token + "=" + tokenFile(t, "first-line\r")}, r.token + " doesn't hold a token"},
			{[]string{r.api + "=http://127.0.0.1:1", r.token + "=" + tokenFile(t, "")}, r.token + " doesn't hold a token"},
		}
		if r.script == "jira.sh" {
			cases = append(cases, refused{[]string{"JIRA_URL=http://127.0.0.1:1", "JSM_API=http://example.com", r.token + "=" + good}, "JSM_API is not https"})
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
		// curl is told the scheme: https for a tool, http for this machine.
		for _, c := range []struct{ url, proto string }{{"https://tool.example.com", "=https"}, {"http://127.0.0.1:1", "=http"}} {
			env := append(append([]string{}, r.env...), r.api+"="+c.url, r.token+"="+good)
			run(r.script, env)
			b, _ := os.ReadFile(args)
			if !strings.Contains(string(b), "-q\n") || !strings.Contains(string(b), "\n--proto\n"+c.proto+"\n") {
				t.Errorf("%s %s: curl's arguments %q, want --proto %s", r.script, c.url, b, c.proto)
			}
		}
	}
}

// TestIncidentRecipesTakeTheirTokens: a token as each tool writes one
// (made up here) is taken, and sent as it is.
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
		var mu sync.Mutex
		var sent []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			mu.Lock()
			sent = append(sent, req.Header.Get("Authorization"))
			mu.Unlock()
			w.Write([]byte(r.empty))
		}))
		cmd := exec.Command("sh", recipes+r.script)
		cmd.Env = append(append(os.Environ(), r.env...), r.api+"="+srv.URL, r.token+"="+tokenFile(t, tokens[r.script]))
		var stderr strings.Builder
		cmd.Stderr = &stderr
		_, err := cmd.Output()
		srv.Close()
		mu.Lock()
		if err != nil || len(sent) == 0 || sent[0] != r.auth(tokens[r.script]) {
			t.Errorf("%s: %v, sent %q, said %q", r.script, err, sent, stderr.String())
		}
		mu.Unlock()
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
