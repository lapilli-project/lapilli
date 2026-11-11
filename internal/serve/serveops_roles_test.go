package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lapilli.dev/internal/ledger"
)

// callAs asks the API as the sign-in proxy names who, in groups, from a page
// on the same site unless site says otherwise.
func callAs(t *testing.T, h http.Handler, method, path, who, groups, body, site string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if site != "" {
		r.Header.Set("Sec-Fetch-Site", site)
	}
	if who != "" {
		r.Header.Set("X-Forwarded-Email", who)
	}
	if groups != "" {
		r.Header.Set("X-Forwarded-Groups", groups)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

// rolesServer is a server with roles on: boss an admin, ed an editor, and
// two teams, payments owning Errors and search owning Search*, each with a
// destination of its own; the first run done.
func rolesServer(t *testing.T, change func(*Config)) (s *Server, pay, search int64) {
	t.Helper()
	dir := t.TempDir()
	hook := filepath.Join(dir, "hook")
	os.WriteFile(hook, []byte("https://hooks.example/x\n"), 0o600)
	s, _ = server(t, func(c *Config) {
		c.UserHeader, c.GroupsHeader = "X-Forwarded-Email", "X-Forwarded-Groups"
		c.Admins, c.Editors = []string{"boss@example.com"}, []string{"ed@example.com"}
		c.TeamDigests = map[string]string{"payments": hook, "search": hook}
		c.Slack = hook
		if change != nil {
			change(c)
		}
	})
	ctx := context.Background()
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	var err error
	if pay, err = s.l.PutTeam(ctx, ledger.Team{Name: "payments", Rules: "^Errors$"}, "boss"); err != nil {
		t.Fatal(err)
	}
	if search, err = s.l.PutTeam(ctx, ledger.Team{Name: "search", Rules: "^Search"}, "boss"); err != nil {
		t.Fatal(err)
	}
	return s, pay, search
}

func id(t *testing.T, body string) int64 {
	t.Helper()
	var x struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &x); err != nil || x.ID == 0 {
		t.Fatalf("no id in %s: %v", body, err)
	}
	return x.ID
}

// An admin gives a team's editor's role; that editor changes the team's
// tasks, quiet windows, reports, notes on its rules and comments on what is
// its, and nothing of another team's or of everyone's; nor anything an
// editor or an admin may.
func TestTeamEditors(t *testing.T) {
	s, pay, search := rolesServer(t, nil)
	h := s.Handler()
	const boss, pat = "boss@example.com", "pat@example.com"
	n := func(v int64) string { return itoa(v) }
	code, body := callAs(t, h, "POST", "/api/v1/roles", boss, "", `{"who": "user:pat@example.com", "team_id": `+n(pay)+`}`, "")
	if code != http.StatusCreated {
		t.Fatalf("giving pat payments: %d %s", code, body)
	}
	grant := id(t, body)
	if code, body := callAs(t, h, "POST", "/api/v1/roles", boss, "", `{"who": "group:search-oncall", "team_id": `+n(search)+`}`, ""); code != http.StatusCreated {
		t.Fatalf("giving search-oncall search: %d %s", code, body)
	}
	_, body = callAs(t, h, "GET", "/api/v1/me", pat, "", "", "")
	if !strings.Contains(body, `"role":"viewer"`) || !strings.Contains(body, `"teams":["payments"]`) {
		t.Errorf("me: %s", body)
	}
	// What is everyone's, or search's, made by an editor.
	_, body = callAs(t, h, "POST", "/api/v1/tasks", "ed@example.com", "", `{"title": "search's", "team_id": `+n(search)+`}`, "")
	theirTask := id(t, body)
	_, body = callAs(t, h, "POST", "/api/v1/windows", "ed@example.com", "", `{"title": "search's", "start": "2026-11-02T01:00:00Z", "end": "2026-11-02T02:00:00Z", "team_id": `+n(search)+`}`, "")
	theirWindow := id(t, body)
	_, body = callAs(t, h, "POST", "/api/v1/windows", "ed@example.com", "", `{"title": "everyone's", "start": "2026-11-02T01:00:00Z", "end": "2026-11-02T02:00:00Z"}`, "")
	everyoneWindow := id(t, body)

	_, body = callAs(t, h, "POST", "/api/v1/tasks", pat, "", `{"title": "mine", "team_id": `+n(pay)+`}`, "")
	ownTask := id(t, body)
	_, body = callAs(t, h, "POST", "/api/v1/windows", pat, "", `{"title": "deploy", "start": "2026-11-02T01:00:00Z", "end": "2026-11-02T02:00:00Z", "team_id": `+n(pay)+`}`, "")
	ownWindow := id(t, body)

	for _, c := range []struct {
		what, method, path, body string
		want                     int
	}{
		{"a task of payments", "POST", "/api/v1/tasks", `{"title": "t", "team_id": ` + n(pay) + `}`, http.StatusCreated},
		{"a task of search", "POST", "/api/v1/tasks", `{"title": "t", "team_id": ` + n(search) + `}`, http.StatusForbidden},
		{"a task of no team", "POST", "/api/v1/tasks", `{"title": "t"}`, http.StatusForbidden},
		{"search's task", "PUT", "/api/v1/tasks/" + n(theirTask), `{"title": "t", "team_id": ` + n(pay) + `}`, http.StatusForbidden},
		{"search's task patched", "PATCH", "/api/v1/tasks/" + n(theirTask), `{"status": "done"}`, http.StatusForbidden},
		{"its task given away", "PATCH", "/api/v1/tasks/" + n(ownTask), `{"team_id": ` + n(search) + `}`, http.StatusForbidden},
		{"search's task deleted", "DELETE", "/api/v1/tasks/" + n(theirTask), "", http.StatusForbidden},
		{"its task done", "PATCH", "/api/v1/tasks/" + n(ownTask), `{"status": "done"}`, http.StatusOK},
		{"a window for everyone", "POST", "/api/v1/windows", `{"title": "w", "start": "2026-11-02T01:00:00Z", "end": "2026-11-02T02:00:00Z"}`, http.StatusForbidden},
		{"a window of search", "POST", "/api/v1/windows", `{"title": "w", "start": "2026-11-02T01:00:00Z", "end": "2026-11-02T02:00:00Z", "team_id": ` + n(search) + `}`, http.StatusForbidden},
		{"everyone's window taken", "PUT", "/api/v1/windows/" + n(everyoneWindow), `{"title": "w", "start": "2026-11-02T01:00:00Z", "end": "2026-11-02T02:00:00Z", "team_id": ` + n(pay) + `}`, http.StatusForbidden},
		{"search's window deleted", "DELETE", "/api/v1/windows/" + n(theirWindow), "", http.StatusForbidden},
		{"its window given away", "PUT", "/api/v1/windows/" + n(ownWindow), `{"title": "w", "start": "2026-11-02T01:00:00Z", "end": "2026-11-02T02:00:00Z", "team_id": ` + n(search) + `}`, http.StatusForbidden},
		{"a note on its rule", "PUT", "/api/v1/notes", `{"rule": "Errors", "status": "tune"}`, http.StatusOK},
		{"its rule given to search", "PUT", "/api/v1/notes", `{"rule": "Errors", "team_id": ` + n(search) + `}`, http.StatusForbidden},
		{"a note on a rule no team owns", "PUT", "/api/v1/notes", `{"rule": "Nobody", "status": "tune"}`, http.StatusForbidden},
		{"a comment on its rule", "POST", "/api/v1/comments", `{"on": "rule", "key": "Errors", "body": "c"}`, http.StatusCreated},
		{"a comment on search's task", "POST", "/api/v1/comments", `{"on": "task", "key": "` + n(theirTask) + `", "body": "c"}`, http.StatusForbidden},
		{"a comment on its task", "POST", "/api/v1/comments", `{"on": "task", "key": "` + n(ownTask) + `", "body": "c"}`, http.StatusCreated},
		{"a report of payments", "POST", "/api/v1/reports", `{"name": "pay daily", "team_id": ` + n(pay) + `, "schedule": "daily@09:00", "dests": ["team:payments"]}`, http.StatusCreated},
		{"a report of payments to Slack", "POST", "/api/v1/reports", `{"name": "pay slack", "team_id": ` + n(pay) + `, "schedule": "daily@09:00", "dests": ["slack"]}`, http.StatusForbidden},
		{"a report of payments to search's", "POST", "/api/v1/reports", `{"name": "pay x", "team_id": ` + n(pay) + `, "schedule": "daily@09:00", "dests": ["team:search"]}`, http.StatusForbidden},
		{"a report for everyone", "POST", "/api/v1/reports", `{"name": "all", "schedule": "daily@09:00", "dests": ["team:payments"]}`, http.StatusForbidden},
		{"a report of search", "POST", "/api/v1/reports", `{"name": "s", "team_id": ` + n(search) + `, "schedule": "daily@09:00", "dests": ["team:search"]}`, http.StatusForbidden},
		{"the digest", "PUT", "/api/v1/reports/digest", `{"schedule": "weekly tue@09:00", "dests": ["team:payments"]}`, http.StatusForbidden},
		{"an incident", "POST", "/api/v1/incidents", `{"title": "x", "start": "2026-11-02T10:00:00Z"}`, http.StatusForbidden},
		{"a team", "POST", "/api/v1/teams", `{"name": "x", "rules": "^X"}`, http.StatusForbidden},
		{"a role", "POST", "/api/v1/roles", `{"who": "user:pat@example.com"}`, http.StatusForbidden},
		{"the roles", "GET", "/api/v1/roles", "", http.StatusForbidden},
	} {
		if code, body := callAs(t, h, c.method, c.path, pat, "", c.body, ""); code != c.want {
			t.Errorf("pat, %s: %d, want %d\n%s", c.what, code, c.want, body)
		}
	}
	if code, _ := as(t, h, http.MethodPost, "/proposals", pat, "", url.Values{"rule": {"Errors"}, "kind": {"for"}, "for": {"10m"}}); code != http.StatusForbidden {
		t.Errorf("pat, a proposal: %d", code)
	}
	if code, _ := as(t, h, http.MethodPost, "/proposals/1/drop", pat, "", url.Values{}); code == http.StatusSeeOther {
		t.Errorf("pat dropped a proposal")
	}
	// Nothing of search's moved.
	if task, _ := s.l.TaskByID(context.Background(), theirTask); task.TeamID != search || task.Status != "open" {
		t.Errorf("search's task: %+v", task)
	}
	// A group's editors: search's, not payments'.
	if code, body := callAs(t, h, "POST", "/api/v1/windows", "sam@example.com", "dev, search-oncall", `{"title": "w", "start": "2026-11-02T01:00:00Z", "end": "2026-11-02T02:00:00Z", "team_id": `+n(search)+`}`, ""); code != http.StatusCreated {
		t.Errorf("search-oncall, a window of search: %d %s", code, body)
	}
	if code, _ := callAs(t, h, "POST", "/api/v1/windows", "sam@example.com", "search-oncall", `{"title": "w", "start": "2026-11-02T01:00:00Z", "end": "2026-11-02T02:00:00Z", "team_id": `+n(pay)+`}`, ""); code != http.StatusForbidden {
		t.Errorf("search-oncall, a window of payments: %d", code)
	}
	// A group named "search-oncall" in a person's name is no group.
	if code, _ := callAs(t, h, "POST", "/api/v1/windows", "search-oncall", "", `{"title": "w", "start": "2026-11-02T01:00:00Z", "end": "2026-11-02T02:00:00Z", "team_id": `+n(search)+`}`, ""); code != http.StatusForbidden {
		t.Errorf("a person named as the group: %d", code)
	}
	// Named twice, the header may be the caller's: no one.
	r := httptest.NewRequest("POST", "/api/v1/tasks", strings.NewReader(`{"title": "t", "team_id": `+n(pay)+`}`))
	r.Header.Add("X-Forwarded-Email", pat)
	r.Header.Add("X-Forwarded-Email", pat)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("pat named twice: %d", w.Code)
	}

	// The pages offer pat what pat may: payments alone, never everyone.
	_, page := asPage(t, h, "/windows", pat)
	if !strings.Contains(page, `action="/windows"`) || !strings.Contains(page, `<option value="`+n(pay)+`">payments</option>`) ||
		strings.Contains(page, `>search</option>`) || strings.Contains(page, `<option value="0">`) {
		t.Errorf("the windows page, for pat:\n%s", page)
	}
	if _, page := asPage(t, h, "/windows/"+n(theirWindow), pat); strings.Contains(page, `/delete"`) {
		t.Errorf("search's window offers pat a change:\n%s", page)
	}
	if _, page := asPage(t, h, "/windows/"+n(ownWindow), pat); !strings.Contains(page, `/windows/`+n(ownWindow)+`/delete"`) {
		t.Errorf("its own window offers pat no change:\n%s", page)
	}
	if _, page := asPage(t, h, "/windows", "viewer@example.com"); strings.Contains(page, `action="/windows"`) {
		t.Errorf("a viewer is offered a window:\n%s", page)
	}
	if code, _ := asPage(t, h, "/roles", pat); code != http.StatusForbidden {
		t.Errorf("pat sees the roles: %d", code)
	}
	if code, page := asPage(t, h, "/roles", boss); code != http.StatusOK || !strings.Contains(page, "user:pat@example.com") || !strings.Contains(page, "ed@example.com") {
		t.Errorf("the roles, for boss: %d\n%s", code, page)
	}

	// Taken away, the next request is a viewer's.
	if code, body := callAs(t, h, "DELETE", "/api/v1/roles/"+n(grant), boss, "", "", ""); code != http.StatusNoContent {
		t.Fatalf("taking pat's role: %d %s", code, body)
	}
	if code, _ := callAs(t, h, "POST", "/api/v1/tasks", pat, "", `{"title": "t", "team_id": `+n(pay)+`}`, ""); code != http.StatusForbidden {
		t.Errorf("pat, after: %d", code)
	}
	// search removed: its editors go with it, and a team given its id later
	// has none.
	if code, _ := callAs(t, h, "DELETE", "/api/v1/teams/"+n(search), boss, "", "", ""); code != http.StatusNoContent {
		t.Fatal(code)
	}
	again, _ := s.l.PutTeam(context.Background(), ledger.Team{Name: "search", Rules: "^Search"}, "boss")
	if code, _ := callAs(t, h, "POST", "/api/v1/windows", "sam@example.com", "search-oncall", `{"title": "w", "start": "2026-11-02T01:00:00Z", "end": "2026-11-02T02:00:00Z", "team_id": `+n(again)+`}`, ""); code != http.StatusForbidden {
		t.Errorf("search-oncall, after search was removed and made again (%d): %d", again, code)
	}
}

func asPage(t *testing.T, h http.Handler, path, who string) (int, string) {
	t.Helper()
	return as(t, h, http.MethodGet, path, who, "", nil)
}

// Only an admin gives roles, and only an editor's; from the page, on this
// site; with roles on. An editor of every team given in the UI is an editor.
func TestGivingRoles(t *testing.T) {
	s, pay, _ := rolesServer(t, nil)
	h := s.Handler()
	const boss = "boss@example.com"
	for _, c := range []struct {
		what, who, body string
		want            int
	}{
		{"an admin's role", boss, `{"who": "user:x@example.com", "role": "admin"}`, http.StatusBadRequest},
		{"an approver's role", boss, `{"who": "user:x@example.com", "role": "approver"}`, http.StatusBadRequest},
		{"everyone", boss, `{"who": "group:*"}`, http.StatusBadRequest},
		{"a bare name", boss, `{"who": "x@example.com"}`, http.StatusBadRequest},
		{"by an editor", "ed@example.com", `{"who": "user:x@example.com"}`, http.StatusForbidden},
		{"by no one named", "", `{"who": "user:x@example.com"}`, http.StatusForbidden},
		{"an editor of every team", boss, `{"who": "user:eve@example.com"}`, http.StatusCreated},
	} {
		if code, body := callAs(t, h, "POST", "/api/v1/roles", c.who, "", c.body, ""); code != c.want {
			t.Errorf("%s: %d, want %d\n%s", c.what, code, c.want, body)
		}
	}
	// From a page elsewhere: refused before anything is read.
	if code, _ := callAs(t, h, "POST", "/api/v1/roles", boss, "", `{"who": "user:mallory@example.com"}`, "cross-site"); code != http.StatusForbidden {
		t.Errorf("from another site: %d", code)
	}
	if code, _ := as(t, h, http.MethodPost, "/roles", boss, "", url.Values{"kind": {"user"}, "name": {"zed@example.com"}, "team": {itoa(pay)}}); code != http.StatusSeeOther {
		t.Errorf("the form: %d", code)
	}
	gs, _ := s.l.Grants(context.Background())
	if len(gs) != 2 || gs[0].Who != "user:eve@example.com" || gs[1].Who != "user:zed@example.com" || gs[1].TeamID != pay || gs[1].By != boss {
		t.Errorf("grants %+v", gs)
	}
	if code, body := callAs(t, h, "POST", "/api/v1/incidents", "eve@example.com", "", `{"title": "x", "start": "2026-11-02T10:00:00Z"}`, ""); code != http.StatusCreated {
		t.Errorf("eve, an editor given in the UI, writing an incident: %d %s", code, body)
	}
	if code, _ := callAs(t, h, "POST", "/api/v1/teams", "eve@example.com", "", `{"name": "x", "rules": "^X"}`, ""); code != http.StatusForbidden {
		t.Errorf("eve making a team: %d", code)
	}
	if _, body := callAs(t, h, "GET", "/api/v1/me", "eve@example.com", "", "", ""); !strings.Contains(body, `"role":"editor"`) {
		t.Errorf("eve: %s", body)
	}
}

// Roles in the ledger need --admins, and --read-only gives no one anything,
// granted or not.
func TestRolesOff(t *testing.T) {
	s, pay, _ := rolesServer(t, func(c *Config) { c.Admins = nil })
	h := s.Handler()
	if code, body := callAs(t, h, "POST", "/api/v1/roles", "ed@example.com", "", `{"who": "user:pat@example.com"}`, ""); code != http.StatusForbidden {
		t.Errorf("no admins, an editor giving a role: %d %s", code, body)
	}
	// Kept by a ledger that had admins once: not read.
	if _, err := s.l.PutGrant(context.Background(), ledger.Grant{Who: "user:pat@example.com", TeamID: pay}, "boss"); err != nil {
		t.Fatal(err)
	}
	if code, _ := callAs(t, h, "POST", "/api/v1/tasks", "pat@example.com", "", `{"title": "t", "team_id": `+itoa(pay)+`}`, ""); code != http.StatusForbidden {
		t.Errorf("a grant read without --admins: %d", code)
	}

	ro, pay2, _ := rolesServer(t, func(c *Config) { c.ReadOnly = true })
	if _, err := ro.l.PutGrant(context.Background(), ledger.Grant{Who: "user:pat@example.com"}, "boss"); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"pat@example.com", "boss@example.com"} {
		if code, _ := callAs(t, ro.Handler(), "POST", "/api/v1/tasks", who, "", `{"title": "t", "team_id": `+itoa(pay2)+`}`, ""); code != http.StatusForbidden {
			t.Errorf("--read-only, %s: %d", who, code)
		}
	}
}

// A window every month, or by a cron expression, kept from the form and
// shown as it repeats; its first repeat refused when it isn't one of the
// schedule's times, saying the next.
func TestWindowByScheduleForm(t *testing.T) {
	s, _ := server(t, nil)
	h := s.Handler()
	for _, c := range []struct {
		form url.Values
		want string
	}{
		{url.Values{"title": {"maintenance"}, "start": {"2026-11-07T02:00"}, "end": {"2026-11-07T04:00"}, "every": {"month"}, "on": {"First  Sat"}}, "every month <code>first sat</code>"},
		{url.Values{"title": {"close"}, "start": {"2026-10-31T22:00"}, "end": {"2026-10-31T23:00"}, "every": {"month"}, "on": {"last"}, "until": {"2027-03-31"}}, "every month <code>last</code>, until 2027-03-31"},
		{url.Values{"title": {"batch"}, "start": {"2026-11-15T01:00"}, "end": {"2026-11-15T02:00"}, "every": {"month"}}, "every month <code>15</code>"},
		{url.Values{"title": {"reindex"}, "start": {"2026-11-07T03:00"}, "end": {"2026-11-07T03:30"}, "every": {"cron"}, "on": {"0 3 * * sat,sun"}}, "by a cron expression <code>0 3 * * sat,sun</code>"},
	} {
		if code, body, _ := post(t, h, "/windows", c.form, "same-origin"); code != http.StatusSeeOther {
			t.Fatalf("%v: %d %s", c.form, code, body)
		}
		if _, body, _ := get(t, h, "/windows"); !strings.Contains(body, c.want) {
			t.Errorf("the windows page has no %q:\n%s", c.want, body)
		}
	}
	if _, body, _ := get(t, h, "/windows/1"); !strings.Contains(body, `<option value="month" selected>`) || !strings.Contains(body, `value="first sat"`) {
		t.Errorf("the window's form keeps how it repeats:\n%s", body)
	}
	code, body, _ := post(t, h, "/windows", url.Values{"title": {"x"}, "start": {"2026-11-08T02:00"}, "end": {"2026-11-08T04:00"}, "every": {"month"}, "on": {"first sat"}}, "same-origin")
	if code != http.StatusBadRequest || !strings.Contains(body, "the next is 2026-12-05 02:00") {
		t.Errorf("not a first Saturday: %d %s", code, body)
	}
	code, body = api(t, h, "POST", "/api/v1/windows", `{"title": "x", "start": "2026-11-07T02:00:00Z", "end": "2026-11-07T04:00:00Z", "every": "cron", "on": "0 2 * * 6"}`)
	if code != http.StatusCreated || !strings.Contains(body, `"on": "0 2 * * 6"`) {
		t.Errorf("by the API: %d %s", code, body)
	}
}
