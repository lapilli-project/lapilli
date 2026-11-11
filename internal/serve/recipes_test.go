package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/incident"
)

const recipes = "../../recipes/incidents/"

// needTools skips a test where what the recipes use isn't installed.
func needTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"sh", "curl", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
}

// fixture answers with a file from the recipes' test data, after checking
// the request carries what the API asks for.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(recipes + "testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// run runs a recipe with an environment, and parses what it writes the way
// --incidents does.
func runRecipe(t *testing.T, script string, env ...string) []incident.Incident {
	t.Helper()
	cmd := exec.Command("sh", recipes+script)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", script, err, out)
	}
	file := filepath.Join(t.TempDir(), "incidents.yaml")
	os.WriteFile(file, out, 0o600)
	incs, err := incident.Load(file)
	if err != nil {
		t.Fatalf("what %s wrote doesn't load: %v\n%s", script, err, out)
	}
	return incs
}

func tokenFile(t *testing.T, token string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "token")
	os.WriteFile(file, []byte(token+"\n"), 0o600)
	return file
}

func wantIncidents(t *testing.T, got []incident.Incident, want ...incident.Incident) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("incidents: %+v, want %+v", got, want)
	}
	for i, w := range want {
		g := got[i]
		a, _ := json.Marshal(g.Labels)
		b, _ := json.Marshal(w.Labels)
		if g.ID != w.ID || g.Title != w.Title || !g.Start.Equal(w.Start) || !g.End.Equal(w.End) || string(a) != string(b) {
			t.Errorf("incident %d: %+v, want %+v", i, g, w)
		}
	}
}

func utc(s string) time.Time {
	x, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return x
}

func TestPagerDutyRecipe(t *testing.T) {
	needTools(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Token token=pd-secret" || !strings.Contains(r.Header.Get("Accept"), "pagerduty+json;version=2") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if got := r.URL.Query().Get("since"); got != "2026-10-01" {
			t.Errorf("since = %q", got)
		}
		name := "pagerduty-1.json"
		if r.URL.Query().Get("offset") == "100" {
			name = "pagerduty-2.json"
		}
		w.Write(fixture(t, name))
	}))
	defer srv.Close()
	got := runRecipe(t, "pagerduty.sh", "PAGERDUTY_API="+srv.URL, "PAGERDUTY_TOKEN_FILE="+tokenFile(t, "pd-secret"), "SINCE=2026-10-01")
	wantIncidents(t, got,
		incident.Incident{ID: "PD-101", Title: "Checkout errors", Start: utc("2026-10-28T03:01:00Z"), End: utc("2026-10-28T03:40:00Z"), Labels: incident.Labels{"service": {"checkout"}}},
		incident.Incident{ID: "PD-103", Title: "Search timeouts", Start: utc("2026-10-30T09:15:00Z"), Labels: incident.Labels{"service": {"search"}}})
}

func TestIncidentIORecipe(t *testing.T) {
	needTools(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer iio-secret" || r.URL.Path != "/v2/incidents" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		name := "incident-io-1.json"
		if r.URL.Query().Get("after") != "" {
			name = "incident-io-2.json"
		}
		w.Write(fixture(t, name))
	}))
	defer srv.Close()
	got := runRecipe(t, "incident-io.sh", "INCIDENT_IO_API="+srv.URL, "INCIDENT_IO_TOKEN_FILE="+tokenFile(t, "iio-secret"))
	wantIncidents(t, got,
		incident.Incident{ID: "INC-41", Title: "Checkout errors", Start: utc("2026-10-28T03:01:00.801578Z"), End: utc("2026-10-28T03:40:00Z"), Labels: incident.Labels{"service": {"checkout", "cart"}}},
		incident.Incident{ID: "INC-43", Title: "Search timeouts", Start: utc("2026-10-30T09:15:00Z")})
}

func TestJiraRecipe(t *testing.T) {
	needTools(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "ana@example.com" || pass != "jira-secret" || r.URL.Path != "/rest/api/3/search/jql" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if got := r.URL.Query().Get("jql"); got != "project = OPS AND created >= -30d" {
			t.Errorf("jql = %q", got)
		}
		name := "jira-1.json"
		if tok := r.URL.Query().Get("nextPageToken"); tok != "" {
			if tok != "a token/with odd=characters" {
				t.Errorf("nextPageToken = %q", tok)
			}
			name = "jira-2.json"
		}
		w.Write(fixture(t, name))
	}))
	defer srv.Close()
	got := runRecipe(t, "jira.sh", "JIRA_URL="+srv.URL, "JIRA_EMAIL=ana@example.com", "JIRA_TOKEN_FILE="+tokenFile(t, "jira-secret"),
		"JIRA_JQL=project = OPS AND created >= -30d")
	wantIncidents(t, got,
		incident.Incident{ID: "OPS-41", Title: "Checkout errors", Start: utc("2026-10-28T12:01:00+09:00"), End: utc("2026-10-28T12:40:00+09:00"), Labels: incident.Labels{"service": {"checkout", "cart"}}},
		incident.Incident{ID: "OPS-42", Title: "Search timeouts", Start: utc("2026-10-30T09:15:00-05:00")})
}

func TestGitHubRecipe(t *testing.T) {
	needTools(t)
	cmd := exec.Command("jq", "-f", recipes+"github.jq", recipes+"testdata/github.json")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	file := filepath.Join(t.TempDir(), "incidents.yaml")
	os.WriteFile(file, out, 0o600)
	got, err := incident.Load(file)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	wantIncidents(t, got,
		incident.Incident{ID: "GH-41", Title: "Checkout errors", Start: utc("2026-10-28T03:01:00Z"), End: utc("2026-10-28T03:40:00Z"), Labels: incident.Labels{"service": {"checkout"}}},
		incident.Incident{ID: "GH-42", Title: "Search timeouts", Start: utc("2026-10-30T09:15:00Z")})
}

// From the tool to the ledger: a recipe's output, through put.sh, into the
// port that takes incidents. Done again, it changes nothing.
func TestPutRecipe(t *testing.T) {
	needTools(t)
	token := "a-token-long-enough"
	file := tokenFile(t, token)
	s, _ := server(t, func(c *Config) { c.IncidentsListen, c.IncidentsToken = "127.0.0.1:0", file })
	srv := httptest.NewServer(s.IncidentsHandler())
	defer srv.Close()
	put := func(body, tok string) (string, error) {
		cmd := exec.Command("sh", recipes+"put.sh", srv.URL)
		cmd.Env = append(os.Environ(), "LAPILLI_TOKEN_FILE="+tokenFile(t, tok))
		cmd.Stdin = strings.NewReader(body)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	body := `{"incidents": [
	  {"id": "PD-101", "title": "Checkout errors", "start": "2026-10-28T03:01:00Z", "end": "2026-10-28T03:40:00Z", "labels": {"service": ["checkout"]}},
	  {"id": "OPS-41", "title": "A zone written otherwise", "start": "2026-10-28T12:01:00+09:00"},
	  {"id": "PD-102", "title": "Too long", "start": "2026-08-01T00:00:00Z", "end": "2026-10-28T00:00:00Z"},
	  {"id": "PD-103", "title": "After one refused", "start": "2026-10-30T09:15:00Z"}
	]}`
	out, err := put(body, token)
	if err == nil || !strings.Contains(out, `"PD-102" wasn't written (400)`) {
		t.Fatalf("one refused should be told of, and fail the script: %v\n%s", err, out)
	}
	ctx := context.Background()
	for _, id := range []string{"PD-101", "OPS-41", "PD-103"} {
		in, err := s.l.IncidentByID(ctx, id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if in.By != tokenUser || in.Origin != "copy" {
			t.Errorf("%s: by %q, origin %q", id, in.By, in.Origin)
		}
	}
	changes := func() int {
		cs, err := s.l.ChangesSince(ctx, time.Unix(0, 0), 1000)
		if err != nil {
			t.Fatal(err)
		}
		return len(cs)
	}
	before, gen := changes(), s.l.Gen()
	if before != 3 {
		t.Errorf("changes = %d, want 3", before)
	}
	// The same again: nothing changes, and no page is thrown away.
	if out, err := put(strings.Replace(body, `{"id": "PD-102", "title": "Too long", "start": "2026-08-01T00:00:00Z", "end": "2026-10-28T00:00:00Z"},`, "", 1), token); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if changes() != before || s.l.Gen() != gen {
		t.Errorf("written again as it was: %d → %d changes, generation %d → %d", before, changes(), gen, s.l.Gen())
	}
	// Changed: written. The wrong token writes nothing, and says so.
	if out, err := put(`{"incidents": [{"id": "PD-103", "title": "After one refused", "start": "2026-10-30T09:15:00Z", "end": "2026-10-30T10:00:00Z"}]}`, token); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if in, _ := s.l.IncidentByID(ctx, "PD-103"); in.End.IsZero() {
		t.Error("the change wasn't written")
	}
	if out, err := put(`{"incidents": [{"id": "PD-104", "start": "2026-10-30T09:15:00Z"}]}`, "not the token at all"); err == nil {
		t.Errorf("the wrong token: %s", out)
	}
	if _, err := s.l.IncidentByID(ctx, "PD-104"); err == nil {
		t.Error("the wrong token wrote an incident")
	}
}

// The port takes incidents, from the token's holder, and nothing else; it
// believes no header, and the UI's port doesn't take the token.
func TestIncidentsPort(t *testing.T) {
	const token = "a-token-long-enough"
	file := tokenFile(t, token)
	s, _ := server(t, func(c *Config) {
		c.IncidentsListen, c.IncidentsToken = "127.0.0.1:0", file
		c.UserHeader, c.Editors = "X-Forwarded-Email", []string{"ana@example.com"}
	})
	port, ui := s.IncidentsHandler(), s.Handler()
	send := func(h http.Handler, method, path, tok string, hdr map[string]string, body string) int {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	inc := `{"title": "x", "start": "2026-10-30T09:15:00Z"}`
	if c := send(port, "PUT", "/api/v1/incidents/A-1", "", nil, inc); c != http.StatusUnauthorized {
		t.Errorf("no token: %d", c)
	}
	if c := send(port, "PUT", "/api/v1/incidents/A-1", "wrong", nil, inc); c != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", c)
	}
	if c := send(port, "PUT", "/api/v1/incidents/A-1", "", map[string]string{"X-Forwarded-Email": "ana@example.com"}, inc); c != http.StatusUnauthorized {
		t.Errorf("a named editor with no token: %d", c)
	}
	if c := send(port, "PUT", "/api/v1/incidents/A-1", token, map[string]string{"X-Forwarded-Email": "ana@example.com"}, inc); c != http.StatusOK {
		t.Errorf("the token: %d", c)
	}
	ctx := context.Background()
	if in, err := s.l.IncidentByID(ctx, "A-1"); err != nil || in.By != tokenUser {
		t.Errorf("noted as %q, not whom a header named: %v", in.By, err)
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/v1/incidents"}, {"GET", "/api/v1/incidents/A-1"}, {"POST", "/api/v1/tasks"}, {"POST", "/api/v1/teams"},
		{"POST", "/settings"}, {"GET", "/"}, {"GET", "/metrics"}, {"POST", "/mcp"},
	} {
		if code := send(port, c.method, c.path, token, nil, "{}"); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s on the incidents port: %d", c.method, c.path, code)
		}
	}
	if c := send(port, "DELETE", "/api/v1/incidents/A-1", token, nil, ""); c != http.StatusNoContent {
		t.Errorf("delete: %d", c)
	}
	// The UI's port: the token is no one, and may only look.
	if c := send(ui, "PUT", "/api/v1/incidents/A-2", token, nil, inc); c != http.StatusForbidden {
		t.Errorf("the token on the UI's port: %d", c)
	}
	// A rotated token is picked up, and the old one goes.
	os.WriteFile(file, []byte("another-token-long-enough\n"), 0o600)
	if c := send(port, "PUT", "/api/v1/incidents/A-3", token, nil, inc); c != http.StatusUnauthorized {
		t.Errorf("the old token: %d", c)
	}
	if c := send(port, "PUT", "/api/v1/incidents/A-3", "another-token-long-enough", nil, inc); c != http.StatusOK {
		t.Errorf("the new token: %d", c)
	}
	// An empty file is no token, whatever is sent.
	os.WriteFile(file, nil, 0o600)
	if c := send(port, "PUT", "/api/v1/incidents/A-4", " ", nil, inc); c != http.StatusUnauthorized {
		t.Errorf("an empty token file: %d", c)
	}
	// Writes made with it aren't anyone's use of Lapilli.
	outs, err := s.outcomes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if outs.Use.Edits != 0 {
		t.Errorf("edits with the token counted as use: %d", outs.Use.Edits)
	}
	// Read-only refuses them.
	s.cfg.ReadOnly = true
	os.WriteFile(file, []byte(token), 0o600)
	if c := send(port, "PUT", "/api/v1/incidents/A-5", token, nil, inc); c != http.StatusForbidden {
		t.Errorf("read-only: %d", c)
	}
}

// A script that fails says so, writes nothing, and put.sh takes nothing that
// isn't a list of incidents: a green job with nothing copied is the failure to
// fear.
func TestRecipesFailLoudly(t *testing.T) {
	needTools(t)
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		if pages > 1 {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		w.Write(fixture(t, "pagerduty-1.json"))
	}))
	defer srv.Close()
	cmd := exec.Command("sh", recipes+"pagerduty.sh")
	cmd.Env = append(os.Environ(), "PAGERDUTY_API="+srv.URL, "PAGERDUTY_TOKEN_FILE="+tokenFile(t, "x"))
	var stdout strings.Builder
	cmd.Stdout = &stdout
	if err := cmd.Run(); err == nil || stdout.Len() != 0 {
		t.Errorf("page 2 failing: %v, wrote %q", err, stdout.String())
	}

	file := tokenFile(t, "a-token-long-enough")
	s, _ := server(t, func(c *Config) { c.IncidentsListen, c.IncidentsToken = "127.0.0.1:0", file })
	port := httptest.NewServer(s.IncidentsHandler())
	defer port.Close()
	for _, in := range []string{"", "not json", `{"nothing": []}`, `{"incidents": "no"}`, "incidents:\n  - {id: A, start: 2026-10-30T09:15:00Z}\n",
		`{"incidents": [{"title": "no id", "start": "2026-10-30T09:15:00Z"}]}`} {
		cmd := exec.Command("sh", recipes+"put.sh", port.URL)
		cmd.Env = append(os.Environ(), "LAPILLI_TOKEN_FILE="+file)
		cmd.Stdin = strings.NewReader(in)
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Errorf("put.sh took %q: %s", in, out)
		}
	}
	if _, err := s.l.IncidentByID(context.Background(), "null"); err == nil {
		t.Error("an incident with no id was written as null")
	}
	// None is fine: there is nothing to copy.
	cmd = exec.Command("sh", recipes+"put.sh", port.URL)
	cmd.Env = append(os.Environ(), "LAPILLI_TOKEN_FILE="+file)
	cmd.Stdin = strings.NewReader(`{"incidents": []}`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("an empty list: %v: %s", err, out)
	}
}

// What goes in a URL is encoded: a zone's + would otherwise reach the tool as a
// space.
func TestRecipeEncodesTimes(t *testing.T) {
	needTools(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("since"); got != "2026-10-01T00:00:00+09:00" {
			t.Errorf("since = %q", got)
		}
		w.Write([]byte(`{"incidents": [], "more": false}`))
	}))
	defer srv.Close()
	runRecipe(t, "pagerduty.sh", "PAGERDUTY_API="+srv.URL, "PAGERDUTY_TOKEN_FILE="+tokenFile(t, "x"), "SINCE=2026-10-01T00:00:00+09:00")
}

// What the token copies in is the tool's, and what people write is theirs:
// neither changes the other's, so the token can't quietly move or take away
// an incident a person wrote, and a vendor id that happens to be L-1 can't
// write over one.
func TestTokenKeepsToItsOwn(t *testing.T) {
	const token = "a-token-long-enough"
	file := tokenFile(t, token)
	s, _ := server(t, func(c *Config) {
		c.IncidentsListen, c.IncidentsToken = "127.0.0.1:0", file
		c.UserHeader, c.Editors = "X-Forwarded-Email", []string{"ana@example.com"}
	})
	port, ui := s.IncidentsHandler(), s.Handler()
	send := func(h http.Handler, method, path string, hdr map[string]string, body string) int {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	ana := map[string]string{"X-Forwarded-Email": "ana@example.com"}
	bearer := map[string]string{"Authorization": "Bearer " + token}
	inc := `{"title": "x", "start": "2026-10-30T09:15:00Z"}`
	if c := send(ui, "POST", "/api/v1/incidents", ana, inc); c != http.StatusCreated {
		t.Fatalf("ana: %d", c)
	}
	for _, c := range []struct {
		h            http.Handler
		method, path string
		hdr          map[string]string
		want         int
	}{
		{port, "PUT", "/api/v1/incidents/L-1", bearer, http.StatusConflict},
		{port, "DELETE", "/api/v1/incidents/L-1", bearer, http.StatusConflict},
		{port, "PUT", "/api/v1/incidents/PD-1", bearer, http.StatusOK},
		{ui, "PUT", "/api/v1/incidents/PD-1", ana, http.StatusConflict},
		{ui, "DELETE", "/api/v1/incidents/PD-1", ana, http.StatusConflict},
	} {
		if got := send(c.h, c.method, c.path, c.hdr, inc); got != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, got, c.want)
		}
	}
	ctx := context.Background()
	if in, err := s.l.IncidentByID(ctx, "L-1"); err != nil || in.By != "ana@example.com" {
		t.Errorf("ana's incident: %+v %v", in, err)
	}
	// A person can't take the name the token's writes are noted under.
	if c := send(ui, "PUT", "/api/v1/incidents/PD-1", map[string]string{"X-Forwarded-Email": tokenUser}, inc); c != http.StatusForbidden {
		t.Errorf("named as the token: %d", c)
	}
	// The page says where it came from, in each language.
	for _, l := range []string{"en", "ko"} {
		r := httptest.NewRequest("GET", "/incidents/PD-1", nil)
		r.Header.Set("Accept-Language", l)
		w := httptest.NewRecorder()
		ui.ServeHTTP(w, r)
		want := map[string]string{"en": "Copied in from the tool", "ko": "도구에서 가져왔어요"}[l]
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: no %q on the page", l, want)
		}
	}
}

// A job needs a few writes a second; more is told to slow down, and put.sh
// waits and tries again.
func TestTokenRate(t *testing.T) {
	const token = "a-token-long-enough"
	file := tokenFile(t, token)
	s, _ := server(t, func(c *Config) { c.IncidentsListen, c.IncidentsToken = "127.0.0.1:0", file })
	port := s.IncidentsHandler()
	codes := map[int]int{}
	for i := range 60 {
		r := httptest.NewRequest("PUT", fmt.Sprintf("/api/v1/incidents/R-%d", i), strings.NewReader(`{"start": "2026-10-30T09:15:00Z"}`))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		port.ServeHTTP(w, r)
		codes[w.Code]++
		if w.Code == http.StatusTooManyRequests && w.Header().Get("Retry-After") == "" {
			t.Error("no Retry-After")
		}
	}
	if codes[http.StatusTooManyRequests] == 0 || codes[http.StatusOK] < 40 {
		t.Errorf("codes: %v", codes)
	}
	// A token rotated to one too short is no token.
	os.WriteFile(file, []byte("abc"), 0o600)
	r := httptest.NewRequest("PUT", "/api/v1/incidents/S-1", strings.NewReader(`{"start": "2026-10-30T09:15:00Z"}`))
	r.Header.Set("Authorization", "Bearer abc")
	w := httptest.NewRecorder()
	port.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("a short token: %d", w.Code)
	}
}

// put.sh: told to slow down, it tries again; sent elsewhere, it says it
// wrote nothing.
func TestPutRetriesAndRedirects(t *testing.T) {
	needTools(t)
	tries := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "moved"):
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		case tries == 0:
			tries++
			w.Header().Set("Retry-After", "1")
			http.Error(w, "slow down", http.StatusTooManyRequests)
		default:
			tries++
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	put := func(body string) (string, error) {
		cmd := exec.Command("sh", recipes+"put.sh", srv.URL)
		cmd.Env = append(os.Environ(), "LAPILLI_TOKEN_FILE="+tokenFile(t, "x"))
		cmd.Stdin = strings.NewReader(body)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := put(`{"incidents": [{"id": "A-1", "start": "2026-10-30T09:15:00Z"}]}`); err != nil || tries != 2 {
		t.Errorf("a 429: %v, %d tries: %s", err, tries, out)
	}
	out, err := put(`{"incidents": [{"id": "moved", "start": "2026-10-30T09:15:00Z"}]}`)
	if err == nil || !strings.Contains(out, "(302)") {
		t.Errorf("a redirect: %v: %s", err, out)
	}
	// An id from elsewhere is shown as JSON: nothing in it acts on a terminal.
	out, _ = put("{\"incidents\": [{\"id\": \"\\u001b]0;x\\u0007moved\", \"start\": \"2026-10-30T09:15:00Z\"}]}")
	if strings.ContainsAny(out, "\x1b\x07") || !strings.Contains(out, `\u001b`) {
		t.Errorf("an odd id: %q", out)
	}
}

// A tool that says there is always more is stopped, and nothing is written.
func TestRecipePageCap(t *testing.T) {
	needTools(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"incidents": [], "more": true, "pagination_meta": {"after": "same"}}`))
	}))
	defer srv.Close()
	for _, c := range []struct{ script, api, tok string }{
		{"pagerduty.sh", "PAGERDUTY_API", "PAGERDUTY_TOKEN_FILE"},
		{"incident-io.sh", "INCIDENT_IO_API", "INCIDENT_IO_TOKEN_FILE"},
	} {
		cmd := exec.Command("sh", recipes+c.script)
		cmd.Env = append(os.Environ(), c.api+"="+srv.URL, c.tok+"="+tokenFile(t, "x"), "MAX_PAGES=3")
		var stdout strings.Builder
		cmd.Stdout = &stdout
		if err := cmd.Run(); err == nil || stdout.Len() != 0 {
			t.Errorf("%s: %v, wrote %q", c.script, err, stdout.String())
		}
	}
}

// One token write at a time; and what is wrong with the incidents file stays
// on the status, and in the metrics, until the file reads cleanly.
func TestTokenOneAtATimeAndPassedOver(t *testing.T) {
	const token = "a-token-long-enough"
	file := tokenFile(t, token)
	s, _ := server(t, func(c *Config) { c.IncidentsListen, c.IncidentsToken = "127.0.0.1:0", file })
	port := s.IncidentsHandler()
	s.tokenBusy <- struct{}{}
	r := httptest.NewRequest("PUT", "/api/v1/incidents/A-1", strings.NewReader(`{"start": "2026-10-30T09:15:00Z"}`))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	port.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("a second at once: %d", w.Code)
	}
	<-s.tokenBusy

	ctx := context.Background()
	h := s.Handler()
	if code, body := api(t, h, "PUT", "/api/v1/incidents/INC-2", `{"title": "a person's", "start": "2026-11-02T06:01:00Z"}`); code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	os.WriteFile(s.cfg.Incidents, []byte("incidents:\n  - {id: INC-1, start: 2026-11-02T06:01:00Z}\n  - {id: INC-2, start: 2026-11-02T06:01:00Z}\n"), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	if st := s.statusView("en"); !strings.Contains(st.Err, "INC-2") {
		t.Errorf("the status after a run: %+v", st)
	}
	_, body, _ := get(t, h, "/metrics")
	if !strings.Contains(body, "lapilli_serve_incidents_passed_over 1") {
		t.Errorf("no passed-over count in the metrics")
	}
	os.WriteFile(s.cfg.Incidents, []byte("incidents:\n  - {id: INC-1, start: 2026-11-02T06:01:00Z}\n"), 0o644)
	s.jobs(ctx, true)
	if st := s.statusView("en"); st.Err != "" {
		t.Errorf("read cleanly, the status still says: %+v", st)
	}
	_, body, _ = get(t, h, "/metrics")
	if !strings.Contains(body, "lapilli_serve_incidents_passed_over 0") {
		t.Errorf("the passed-over count wasn't cleared")
	}
}

// A MAX_PAGES that isn't a number is refused, not taken for no cap.
func TestRecipeMaxPagesIsANumber(t *testing.T) {
	needTools(t)
	cmd := exec.Command("sh", recipes+"pagerduty.sh")
	cmd.Env = append(os.Environ(), "PAGERDUTY_API=http://127.0.0.1:1", "PAGERDUTY_TOKEN_FILE="+tokenFile(t, "x"), "MAX_PAGES=1k")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "MAX_PAGES is a number") {
		t.Errorf("%v: %s", err, out)
	}
}

func TestCompassRecipe(t *testing.T) {
	needTools(t)
	var logs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "ana@example.com" || pass != "compass-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		// The log of the closed alert that counts, a page at a time.
		if r.URL.Path == "/alerts/e0caa0ce-d52f-4500-81b9-d592d06970b6/logs" {
			logs = append(logs, r.URL.RawQuery)
			name := "compass-logs-1.json"
			if q.Get("after") == "1761620460958_1761620460958234593" {
				name = "compass-logs-2.json"
			}
			w.Write(fixture(t, name))
			return
		}
		if r.URL.Path != "/alerts" {
			t.Errorf("asked for %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if q.Get("from") != "1790812800" || q.Get("size") != "100" || q.Get("sort") != "createdAt" || q.Get("order") != "asc" || q.Get("query") != "tag: incident" {
			t.Errorf("query = %v", q)
		}
		name := "compass-1.json"
		if q.Get("offset") == "100" {
			name = "compass-2.json"
		}
		w.Write(fixture(t, name))
	}))
	defer srv.Close()
	got := runRecipe(t, "compass.sh", "COMPASS_API="+srv.URL, "COMPASS_CLOUD_ID=35273b54-3f06-40d2-880f-dd28cf8daafa", "COMPASS_EMAIL=ana@example.com",
		"COMPASS_TOKEN_FILE="+tokenFile(t, "compass-secret"), "SINCE=2026-10-01T09:00:00+09:00", "COMPASS_QUERY=tag: incident")
	// Closed when its log says, the latest close on either page, not at
	// its last update (03:40).
	wantIncidents(t, got,
		incident.Incident{ID: "CP-1041", Title: "Checkout errors", Start: utc("2026-10-28T03:01:00.958Z"), End: utc("2026-10-28T03:35:12Z"), Labels: incident.Labels{"service": {"checkout"}}},
		incident.Incident{ID: "CP-1043", Title: "Search timeouts", Start: utc("2026-10-30T09:15:00+09:00")})
	if len(logs) != 2 || logs[0] != "size=100" || logs[1] != "size=100&after=1761620460958_1761620460958234593" {
		t.Errorf("the log read as %q", logs)
	}
}

// Compass's log, read as an attacker might answer it: a next page is built
// from the alert's id and a cursor of digits, never followed as given; an
// alert id that isn't one stops the run; past MAX_LOGS, an alert ends at
// its last update.
func TestCompassLogsBounded(t *testing.T) {
	needTools(t)
	alert := func(id string) string {
		return `{"values": [{"id": "` + id + `", "tinyId": "7", "createdAt": "2026-10-28T03:00:00Z", "updatedAt": "2026-10-28T04:00:00Z",
			"message": "x", "status": "closed", "priority": "P1"}], "links": {}}`
	}
	var asked []string
	var listing string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.RequestURI())
		if r.URL.Path == "/alerts" {
			w.Write([]byte(listing))
			return
		}
		// Each page says there is another, at a cursor that isn't one, or
		// on another host.
		w.Write([]byte(`{"values": [{"logTime": "2026-10-28T03:30:00Z", "logType": "close", "log": "x"}],
			"links": {"next": "https://elsewhere.example/v1/alerts/a/logs?after=1;touch%20/tmp/x"}}`))
	}))
	defer srv.Close()
	run := func(env ...string) (string, string, error) {
		cmd := exec.Command("sh", recipes+"compass.sh")
		cmd.Env = append(os.Environ(), append([]string{"COMPASS_API=" + srv.URL, "COMPASS_CLOUD_ID=35273b54", "COMPASS_EMAIL=a@example.com",
			"COMPASS_TOKEN_FILE=" + tokenFile(t, "x")}, env...)...)
		var out, errb strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		return out.String(), errb.String(), err
	}

	listing = alert("a-1")
	out, stderr, err := run()
	if err != nil || !strings.Contains(out, "end: \"2026-10-28T03:30:00Z\"") && !strings.Contains(out, "2026-10-28T03:30:00Z") {
		t.Errorf("%v: %s %s", err, out, stderr)
	}
	for _, a := range asked {
		if strings.Contains(a, "touch") || strings.Contains(a, "elsewhere") || strings.Count(a, "after=") > 0 {
			t.Errorf("followed a next page as given: %s", a)
		}
	}
	if !strings.Contains(stderr, "without a cursor") {
		t.Errorf("a next page without a cursor isn't said: %s", stderr)
	}

	asked = nil
	listing = alert("../../alerts")
	if out, _, err := run(); err == nil || out != "" {
		t.Errorf("an alert id that isn't one: %v, wrote %q", err, out)
	}
	for _, a := range asked {
		if strings.Contains(a, "logs") {
			t.Errorf("read the log of an alert id that isn't one: %s", a)
		}
	}

	asked = nil
	listing = alert("a-2")
	out, stderr, err = run("MAX_LOGS=0")
	if err != nil || !strings.Contains(out, "2026-10-28T04:00:00Z") || !strings.Contains(stderr, "past MAX_LOGS") {
		t.Errorf("MAX_LOGS=0: %v: %s %s", err, out, stderr)
	}
	for _, a := range asked {
		if strings.Contains(a, "logs") {
			t.Errorf("read a log past MAX_LOGS: %s", a)
		}
	}
	if _, _, err := run("MAX_LOGS=5x"); err == nil {
		t.Errorf("MAX_LOGS that isn't a number taken")
	}
}

func TestGrafanaIRMRecipe(t *testing.T) {
	needTools(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer glsa_secret" || r.Method != http.MethodPost ||
			r.URL.Path != "/api/plugins/grafana-irm-app/resources/api/v1/IncidentsService.QueryIncidents" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req struct {
			Query struct {
				Limit          int    `json:"limit"`
				OrderDirection string `json:"orderDirection"`
				DateFrom       string `json:"dateFrom"`
			} `json:"query"`
			Cursor *struct {
				NextValue string `json:"nextValue"`
			} `json:"cursor"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.Query.Limit != 100 || req.Query.OrderDirection != "ASC" || req.Query.DateFrom != "2026-10-01T00:00:00Z" {
			t.Errorf("query = %+v", req.Query)
		}
		name := "grafana-irm-1.json"
		if req.Cursor != nil {
			if req.Cursor.NextValue != "123e4567-e89b-12d3-a456-426614174000" {
				t.Errorf("cursor = %+v", req.Cursor)
			}
			name = "grafana-irm-2.json"
		}
		w.Write(fixture(t, name))
	}))
	defer srv.Close()
	got := runRecipe(t, "grafana-irm.sh", "GRAFANA_URL="+srv.URL+"/", "GRAFANA_IRM_TOKEN_FILE="+tokenFile(t, "glsa_secret"), "SINCE=2026-10-01")
	wantIncidents(t, got,
		incident.Incident{ID: "IRM-41", Title: "Checkout errors", Start: utc("2026-10-28T03:01:00Z"), End: utc("2026-10-28T03:40:00Z"), Labels: incident.Labels{"service": {"checkout"}}},
		incident.Incident{ID: "IRM-43", Title: "Search timeouts", Start: utc("2026-10-30T09:15:00Z"), Labels: incident.Labels{"service": {"search", "cart"}}},
		incident.Incident{ID: "IRM-44", Title: "Batch stuck", Start: utc("2026-10-31T02:38:00Z"), End: utc("2026-10-31T03:10:20.574137Z")})
}

func TestRootlyRecipe(t *testing.T) {
	needTools(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer rootly-secret" || r.URL.Path != "/v1/incidents" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		if q.Get("filter[created_at][gte]") != "2026-10-01" || q.Get("page[size]") != "100" {
			t.Errorf("query = %v", q)
		}
		name := "rootly-1.json"
		if q.Get("page[number]") == "2" {
			name = "rootly-2.json"
		}
		w.Write(fixture(t, name))
	}))
	defer srv.Close()
	got := runRecipe(t, "rootly.sh", "ROOTLY_API="+srv.URL, "ROOTLY_TOKEN_FILE="+tokenFile(t, "rootly-secret"), "SINCE=2026-10-01")
	wantIncidents(t, got,
		incident.Incident{ID: "RL-41", Title: "Checkout errors", Start: utc("2026-10-28T03:01:00Z"), End: utc("2026-10-28T03:40:00Z"), Labels: incident.Labels{"service": {"checkout", "cart"}}},
		incident.Incident{ID: "RL-43", Title: "Search timeouts", Start: utc("2026-10-30T09:15:00Z"), Labels: incident.Labels{"service": {"search"}}})
}

func TestFireHydrantRecipe(t *testing.T) {
	needTools(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fhb-secret" || r.URL.Path != "/v1/incidents" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		if q.Get("start_date") != "2026-10-01" || q.Get("per_page") != "200" {
			t.Errorf("query = %v", q)
		}
		name := "firehydrant-1.json"
		if q.Get("page") == "2" {
			name = "firehydrant-2.json"
		}
		w.Write(fixture(t, name))
	}))
	defer srv.Close()
	got := runRecipe(t, "firehydrant.sh", "FIREHYDRANT_API="+srv.URL, "FIREHYDRANT_TOKEN_FILE="+tokenFile(t, "fhb-secret"), "SINCE=2026-10-01")
	wantIncidents(t, got,
		incident.Incident{ID: "FH-41", Title: "Checkout errors", Start: utc("2026-10-28T03:01:00Z"), End: utc("2026-10-28T03:40:00Z"), Labels: incident.Labels{"service": {"checkout"}}},
		incident.Incident{ID: "FH-43", Title: "Search timeouts", Start: utc("2026-10-30T09:15:00Z"), End: utc("2026-10-30T09:50:00Z")})
}

// The newer recipes, alike: a SINCE that isn't a time is refused before any
// request; a page that fails, a tool that always has more, and Grafana
// IRM's 200 that says it failed, each stop the script with nothing written.
func TestNewRecipesFailLoudly(t *testing.T) {
	needTools(t)
	type recipe struct {
		script string
		env    func(url string) []string
		// more answers a page that says there is a next one, the same
		// next one each time.
		more string
	}
	newer := []recipe{
		{"compass.sh", func(u string) []string {
			return []string{"COMPASS_API=" + u, "COMPASS_CLOUD_ID=35273b54", "COMPASS_EMAIL=a@example.com", "COMPASS_TOKEN_FILE=" + tokenFile(t, "x")}
		}, `{"values": [], "links": {"next": "/v1/alerts?offset=100&size=100"}}`},
		{"grafana-irm.sh", func(u string) []string {
			return []string{"GRAFANA_URL=" + u, "GRAFANA_IRM_TOKEN_FILE=" + tokenFile(t, "x")}
		}, `{"incidents": [], "cursor": {"hasMore": true, "nextValue": "same"}, "error": ""}`},
		{"rootly.sh", func(u string) []string {
			return []string{"ROOTLY_API=" + u, "ROOTLY_TOKEN_FILE=" + tokenFile(t, "x")}
		}, `{"data": [], "links": {}, "meta": {"current_page": 1, "next_page": 2, "prev_page": null, "total_count": 0, "total_pages": 9}}`},
		{"firehydrant.sh", func(u string) []string {
			return []string{"FIREHYDRANT_API=" + u, "FIREHYDRANT_TOKEN_FILE=" + tokenFile(t, "x")}
		}, `{"data": [], "pagination": {"next": 2}}`},
	}
	run := func(script string, env []string) (string, error) {
		cmd := exec.Command("sh", recipes+script)
		cmd.Env = append(os.Environ(), env...)
		var stdout strings.Builder
		cmd.Stdout = &stdout
		err := cmd.Run()
		return stdout.String(), err
	}
	for _, c := range newer {
		asked := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			asked++
			http.Error(w, "down", http.StatusInternalServerError)
		}))
		for _, since := range []string{"yesterday", "2026-10-01; x", "2026-10-01\n2026-10-02", "2026-10-01T00:00:00"} {
			if out, err := run(c.script, append(c.env(srv.URL), "SINCE="+since)); err == nil || out != "" {
				t.Errorf("%s: SINCE %q: %v, wrote %q", c.script, since, err, out)
			}
		}
		if asked != 0 {
			t.Errorf("%s: %d requests with a SINCE that isn't a time", c.script, asked)
		}
		srv.Close()

		pages := 0
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			pages++
			if pages > 1 {
				http.Error(w, "down", http.StatusInternalServerError)
				return
			}
			w.Write([]byte(c.more))
		}))
		if out, err := run(c.script, c.env(srv.URL)); err == nil || out != "" {
			t.Errorf("%s: page 2 failing: %v, wrote %q", c.script, err, out)
		}
		srv.Close()

		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(c.more)) }))
		if out, err := run(c.script, append(c.env(srv.URL), "MAX_PAGES=3")); err == nil || out != "" {
			t.Errorf("%s: always more: %v, wrote %q", c.script, err, out)
		}
		srv.Close()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"incidents": [], "cursor": {"hasMore": false}, "error": "\u001b]0;x\u0007permission denied"}`))
	}))
	defer srv.Close()
	cmd := exec.Command("sh", recipes+"grafana-irm.sh")
	cmd.Env = append(os.Environ(), "GRAFANA_URL="+srv.URL, "GRAFANA_IRM_TOKEN_FILE="+tokenFile(t, "x"))
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err == nil || stdout.Len() != 0 || !strings.Contains(stderr.String(), "permission denied") || strings.ContainsAny(stderr.String(), "\x1b\x07") {
		t.Errorf("Grafana IRM's error: %v, wrote %q, said %q", err, stdout.String(), stderr.String())
	}
}
