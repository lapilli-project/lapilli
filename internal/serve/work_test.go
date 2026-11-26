package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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
	parallel(t)
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
	if !strings.Contains(body, `href="/rules/Errors?team=payments&amp;weeks=4"`) {
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
	_, body, _ = get(t, h, "/rules/Errors?tab=activity")
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

// A team or a comment written through the API is answered as it was
// kept, as the API reads it back: they were answered as they were sent,
// a team's name untrimmed, with no time and no author (updated
// 0001-01-01), where a quiet window or a task was answered as kept.
func TestWrittenAsKept(t *testing.T) {
	parallel(t)
	s, _ := server(t, func(c *Config) { c.UserHeader = "X-Forwarded-Email" })
	raw := ""
	if err := s.look(context.Background(), &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(context.Background(), true)
	h := s.Handler()
	for _, c := range []struct{ path, body string }{
		{"/api/v1/teams", `{"name": " shop ", "rules": "^Shop"}`},
		{"/api/v1/comments", `{"on": "rule", "key": "Errors", "body": "pages on every deploy"}`},
	} {
		code, written := api(t, h, "POST", c.path, c.body)
		if code != http.StatusCreated {
			t.Fatalf("%s: %d %s", c.path, code, written)
		}
		var w struct {
			ID      int64     `json:"id"`
			Name    string    `json:"name"`
			By      string    `json:"by"`
			Updated time.Time `json:"updated"`
		}
		if err := json.Unmarshal([]byte(written), &w); err != nil {
			t.Fatal(err)
		}
		_, read := api(t, h, "GET", c.path+"/"+itoa(w.ID), "")
		if written != read || w.By != "bo@example.com" || w.Updated.IsZero() || strings.Contains(c.path, "teams") && w.Name != "shop" {
			t.Errorf("%s: answered\n%s\nread back\n%s", c.path, written, read)
		}
	}
}

func itoa(n int64) string { return jsonInt(n) }

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }

// What an independent review found, held to.
func TestWorkReview(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, _ := server(t, func(c *Config) { c.UserHeader = "X-Forwarded-Email" })
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	h := s.Handler()
	as := func(who, path string) string {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-Forwarded-Email", who)
		r.Header.Set("Accept-Language", "ko")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Body.String()
	}

	// A page shows each reader what they can change, cached or not.
	if _, err := s.l.PutComment(ctx, ledger.Comment{On: "rule", Key: "Errors", Body: "mine"}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(as("ana@example.com", "/rules/Errors?tab=activity"), "/comments/1/delete") {
		t.Error("ana should be offered her comment to change")
	}
	if strings.Contains(as("bo@example.com", "/rules/Errors?tab=activity"), "/comments/1/delete") {
		t.Error("bo was offered ana's comment, from the cache")
	}

	// Anonymous, in the reader's language.
	if _, err := s.l.PutComment(ctx, ledger.Comment{On: "rule", Key: "Errors", Body: "theirs"}, "anonymous"); err != nil {
		t.Fatal(err)
	}
	if body := as("bo@example.com", "/rules/Errors?tab=activity"); !strings.Contains(body, "익명") || strings.Contains(body, "<strong>anonymous</strong>") {
		t.Error("anonymous should be shown in Korean")
	}

	// A task made from a team's overview is that team's.
	id, err := s.l.PutTeam(ctx, ledger.Team{Name: "payments", Rules: "^Errors$"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	post(t, h, "/tasks", url.Values{"title": {"Tune Errors"}, "kind": {"tune"}, "rule": {"Errors"}, "back": {"/?team=payments"}}, "same-origin")
	if ts, _ := s.l.Tasks(ctx, ledger.TaskQuery{TeamID: id}); len(ts) != 1 {
		t.Errorf("payments' tasks: %+v", ts)
	}

	// PUT changes; it doesn't make.
	for _, p := range []string{"/api/v1/teams/abc", "/api/v1/teams/0", "/api/v1/tasks/0", "/api/v1/windows/x", "/api/v1/comments/0"} {
		if code, body := api(t, h, "PUT", p, `{"name": "x", "rules": "x", "title": "x", "body": "x"}`); code != http.StatusNotFound {
			t.Errorf("PUT %s: %d %s", p, code, body)
		}
	}
	// What a POST says it made can be read.
	code, body := api(t, h, "POST", "/api/v1/windows", `{"title": "w", "start": "2026-11-01T10:00:00Z", "end": "2026-11-01T11:00:00Z"}`)
	if code != http.StatusCreated {
		t.Fatalf("window: %d %s", code, body)
	}
	if code, _ := api(t, h, "GET", "/api/v1/windows/1", ""); code != 200 {
		t.Errorf("GET the window made: %d", code)
	}

	// A pattern that would take long to match is refused, in Korean too.
	r := httptest.NewRequest(http.MethodPost, "/teams", strings.NewReader(url.Values{"name": {"slow"}, "rules": {strings.Repeat(`(?:\w*\pL*){1000}`, 10) + "!"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Accept-Language", "ko")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "너무 복잡해서") {
		t.Errorf("a slow pattern: %d", w.Code)
	}
	// A Korean title of 150 characters fits the 200 a title takes.
	if _, err := s.l.PutTask(ctx, ledger.Task{Title: strings.Repeat("가", 150)}, "ana"); err != nil {
		t.Errorf("a Korean title: %v", err)
	}
}

// A window's repeat day is repeat_on on the API, and on, its old name, is
// still taken, in a window's body and a marked window's, and in the marked
// file; both given is refused, naming both. What the API gives back has
// repeat_on, and on beside it.
func TestRepeatOnNames(t *testing.T) {
	parallel(t)
	s, _ := server(t, func(c *Config) { c.UserHeader = "X-Forwarded-Email" })
	h := s.Handler()
	window := func(day string) string {
		return `{"title": "patch", "start": "2026-11-07T02:00:00Z", "end": "2026-11-07T03:00:00Z", "every": "month", ` + day + `}`
	}
	marked := func(day string) string {
		return `{"kind": "sale", "title": "sale", "start": "2026-11-07T02:00:00Z", "end": "2026-11-07T03:00:00Z", "every": "month", ` + day + `}`
	}
	for _, c := range []struct{ path, kind string }{{"/api/v1/windows", "window"}, {"/api/v1/marked", "marked"}} {
		body := window
		if c.kind == "marked" {
			body = marked
		}
		for _, day := range []string{`"repeat_on": "first sat"`, `"on": "first sat"`} {
			code, out := api(t, h, "POST", c.path, body(day))
			if code != http.StatusCreated || !strings.Contains(out, `"repeat_on": "first sat"`) || !strings.Contains(out, `"on": "first sat"`) {
				t.Fatalf("%s %s: %d %s", c.path, day, code, out)
			}
			var made struct{ ID int64 }
			if err := json.Unmarshal([]byte(out), &made); err != nil {
				t.Fatal(err)
			}
			// The same day, kept as On whichever name it came by.
			code, out = api(t, h, "GET", c.path+"/"+itoa(made.ID), "")
			if code != http.StatusOK || !strings.Contains(out, `"repeat_on": "first sat"`) || !strings.Contains(out, `"on": "first sat"`) {
				t.Errorf("%s/%d: %d %s", c.path, made.ID, code, out)
			}
		}
		code, out := api(t, h, "GET", c.path+"?from=2026-11-01T00:00:00Z&to=2026-12-31T00:00:00Z", "")
		if code != http.StatusOK || strings.Count(out, `"repeat_on": "first sat"`) != 2 || strings.Count(out, `"on": "first sat"`) != 2 {
			t.Errorf("%s: %d %s", c.path, code, out)
		}
		code, out = api(t, h, "POST", c.path, body(`"on": "first sat", "repeat_on": "last"`))
		if code != http.StatusBadRequest || !strings.Contains(out, "on and repeat_on") {
			t.Errorf("%s with both: %d %s", c.path, code, out)
		}
	}
	// Created with one, changed with the other.
	code, out := api(t, h, "PUT", "/api/v1/windows/1", strings.Replace(window(`"repeat_on": "first sat"`), "patch", "patched", 1))
	if code != http.StatusOK || !strings.Contains(out, `"patched"`) || !strings.Contains(out, `"on": "first sat"`) || !strings.Contains(out, `"repeat_on": "first sat"`) {
		t.Errorf("PUT: %d %s", code, out)
	}
	// The marked file.
	file := "marked:\n  - kind: sale\n    title: s\n    start: 2026-11-07T02:00:00Z\n    end: 2026-11-07T03:00:00Z\n    every: month\n    %s\n"
	for _, day := range []string{"repeat_on: first sat", "on: first sat"} {
		es, err := readMarkedFile([]byte(fmt.Sprintf(file, day)), "m.yaml")
		if err != nil || len(es) != 1 {
			t.Fatalf("%s: %v", day, err)
		}
		ms, err := s.markedOf(context.Background(), es, "m.yaml")
		if err != nil || len(ms) != 1 || ms[0].On != "first sat" || ms[0].RepeatOn != "" {
			t.Errorf("%s: %+v %v", day, ms, err)
		}
	}
	es, err := readMarkedFile([]byte(fmt.Sprintf(file, "on: first sat\n    repeat_on: last")), "m.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.markedOf(context.Background(), es, "m.yaml"); err == nil || !strings.Contains(err.Error(), "on and repeat_on") || !strings.Contains(err.Error(), "window 1") {
		t.Errorf("both: %v", err)
	}
}
