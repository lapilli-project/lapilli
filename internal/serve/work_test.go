package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

func api(t *testing.T, h http.Handler, method, path, body string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Forwarded-Email", "bo@example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

// Teams, notes, quiet windows, tasks and comments, through the pages and
// the API: made, shown, changed and taken away.
func TestWork(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, func(c *Config) { c.UserHeader = "X-Forwarded-Email" })
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	h := s.Handler()

	// A team that owns checkout's incidents and the rules named Errors.
	if code, body, _ := post(t, h, "/teams", url.Values{"name": {"payments"}, "rules": {"^Errors$"}, "labels": {"service=checkout"}}, "same-origin"); code != http.StatusSeeOther {
		t.Fatalf("team: %d %s", code, body)
	}
	_, body, _ := get(t, h, "/teams")
	if !strings.Contains(body, "payments") {
		t.Errorf("teams page:\n%s", body)
	}
	_, body, _ = get(t, h, "/rules?team=payments")
	if !strings.Contains(body, `href="/rules/Errors"`) {
		t.Errorf("payments' rules should list Errors:\n%s", body)
	}
	if code, _, _ := get(t, h, "/?team=payments"); code != 200 {
		t.Errorf("the overview for a team: %d", code)
	}

	// A note on the rule.
	if code, body, _ := post(t, h, "/notes", url.Values{"rule": {"Errors"}, "status": {"tune"}, "runbook": {"https://runbooks.example/errors"}, "note": {"pages on every deploy"}}, "same-origin"); code != http.StatusSeeOther {
		t.Fatalf("note: %d %s", code, body)
	}
	_, body, _ = get(t, h, "/rules/Errors")
	if !strings.Contains(body, "pages on every deploy") || !strings.Contains(body, "https://runbooks.example/errors") {
		t.Errorf("the rule's note:\n%s", body)
	}
	if code, _, _ := post(t, h, "/notes", url.Values{"rule": {"Errors"}, "runbook": {"javascript:alert(1)"}}, "same-origin"); code != 400 {
		t.Errorf("a runbook that isn't a web page: %d", code)
	}

	// A quiet window over the search firings 50h to 30h ago excuses them.
	report := func() verdict.RuleResult {
		_, body := api(t, h, "GET", "/api/v1/report?from=2026-10-31T00:00:00Z&to=2026-11-03T12:00:00Z&firings=false", "")
		var rep verdict.Report
		json.Unmarshal([]byte(body), &rep)
		return rep.Rules[0]
	}
	before := report()
	code, wbody := api(t, h, "POST", "/api/v1/windows", `{"title": "search migration", "start": "2026-11-01T10:00:00Z", "end": "2026-11-02T06:00:00Z", "labels": {"service": ["search"]}}`)
	if code != http.StatusCreated {
		t.Fatalf("window: %d %s", code, wbody)
	}
	after := report()
	if after.Excused == 0 || after.Firings != before.Firings-after.Excused || after.White != before.White {
		t.Errorf("before %+v, after %+v", before, after)
	}
	var win ledger.Window
	json.Unmarshal([]byte(wbody), &win)
	_, body, _ = get(t, h, "/windows/"+itoa(win.ID))
	if !strings.Contains(body, "search migration") || !strings.Contains(body, "service=search") {
		t.Errorf("the window's page:\n%s", body)
	}
	if code, _ := api(t, h, "DELETE", "/api/v1/windows/"+itoa(win.ID), ""); code != http.StatusNoContent {
		t.Errorf("delete window: %d", code)
	}
	if again := report(); again.Excused != 0 || again.Firings != before.Firings {
		t.Errorf("after the window went: %+v", again)
	}

	// A task from the overview's suggestion, then done.
	code, _, hd := post(t, h, "/tasks", url.Values{"title": {"Tune Errors"}, "kind": {"tune"}, "rule": {"Errors"}, "back": {"/"}}, "same-origin")
	if code != http.StatusSeeOther || hd.Get("Location") != "/" {
		t.Fatalf("task: %d %v", code, hd)
	}
	_, body, _ = get(t, h, "/")
	if !strings.Contains(body, `href="/tasks/1"`) {
		t.Errorf("the overview should link the task:\n%s", body)
	}
	if code, body, _ := post(t, h, "/tasks", url.Values{"id": {"1"}, "only": {"status"}, "status": {"done"}}, "same-origin"); code != http.StatusSeeOther {
		t.Fatalf("done: %d %s", code, body)
	}
	_, tbody := api(t, h, "GET", "/api/v1/tasks/1", "")
	if !strings.Contains(tbody, `"status": "done"`) || !strings.Contains(tbody, `"rule": "Errors"`) || !strings.Contains(tbody, `"by": "ana@example.com"`) {
		t.Errorf("the task kept its rule, and who closed it: %s", tbody)
	}
	if code, body := api(t, h, "POST", "/api/v1/tasks", `{"title": "x", "kind": "nonsense"}`); code != 400 {
		t.Errorf("a bad kind: %d %s", code, body)
	}

	// Comments: only their author changes them.
	if code, _, _ := post(t, h, "/comments", url.Values{"on": {"rule"}, "key": {"Errors"}, "body": {"raised for to 10m"}, "back": {"/rules/Errors"}}, "same-origin"); code != http.StatusSeeOther {
		t.Fatalf("comment: %d", code)
	}
	_, body, _ = get(t, h, "/rules/Errors")
	if !strings.Contains(body, "raised for to 10m") {
		t.Errorf("the comment on the rule's page:\n%s", body)
	}
	if code, _ := api(t, h, "PUT", "/api/v1/comments/1", `{"body": "not mine"}`); code != http.StatusForbidden {
		t.Errorf("someone else's comment: %d", code)
	}
	// Never back to another site after a form.
	if _, _, hd := post(t, h, "/comments", url.Values{"on": {"rule"}, "key": {"Errors"}, "body": {"x"}, "back": {"//evil.example/"}}, "same-origin"); hd.Get("Location") != "/" {
		t.Errorf("back to %q", hd.Get("Location"))
	}
	if code, _ := api(t, h, "DELETE", "/api/v1/teams/1", ""); code != http.StatusNoContent {
		t.Errorf("delete team: %d", code)
	}
	for _, p := range []string{"/tasks", "/tasks?status=all", "/tasks/1", "/windows", "/teams"} {
		if code, body, _ := get(t, h, p); code != 200 {
			t.Errorf("%s: %d\n%s", p, code, body)
		}
	}
}

func itoa(n int64) string { return jsonInt(n) }

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }
