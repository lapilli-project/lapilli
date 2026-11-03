package serve

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// as asks the handler, as the sign-in proxy would for who, in groups.
func as(t *testing.T, h http.Handler, method, path, who, groups string, form url.Values) (int, string) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, path, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r.Header.Set("Sec-Fetch-Site", "same-origin")
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

// Viewers look, editors write, admins also change the settings and the
// teams; the pages offer only what the reader may do.
func TestRoles(t *testing.T) {
	s, _ := server(t, func(c *Config) {
		c.UserHeader, c.GroupsHeader = "X-Forwarded-Email", "X-Forwarded-Groups"
		c.Editors, c.Admins = []string{"ed@example.com", "sre"}, []string{"boss@example.com"}
	})
	h := s.Handler()
	task := url.Values{"title": {"Tune Errors"}}
	team := url.Values{"name": {"payments"}, "rules": {"^Errors$"}}
	settings := url.Values{"lead": {"10m"}, "match": {"service"}, "refire": {"1h"}, "noisy-firings": {"5"}, "noisy-precision": {"0.5"}}

	for _, c := range []struct {
		who, groups, path string
		form              url.Values
		want              int
	}{
		{"", "", "/tasks", task, http.StatusForbidden},
		{"someone@example.com", "dev", "/tasks", task, http.StatusForbidden},
		{"someone@example.com", "dev", "/incidents", url.Values{"title": {"x"}, "start": {"2026-11-02T10:00"}}, http.StatusForbidden},
		{"ed@example.com", "", "/tasks", task, http.StatusSeeOther},
		{"ops@example.com", "dev, sre", "/tasks", task, http.StatusSeeOther},
		{"ed@example.com", "", "/teams", team, http.StatusForbidden},
		{"ed@example.com", "", "/settings", settings, http.StatusForbidden},
		{"boss@example.com", "", "/teams", team, http.StatusSeeOther},
		{"boss@example.com", "", "/settings", settings, http.StatusSeeOther},
	} {
		if code, body := as(t, h, http.MethodPost, c.path, c.who, c.groups, c.form); code != c.want {
			t.Errorf("%s (%s) POST %s: %d, want %d\n%s", c.who, c.groups, c.path, code, c.want, body)
		}
	}

	// The API is held to the same.
	r := httptest.NewRequest(http.MethodPost, "/api/v1/teams", strings.NewReader(`{"name": "search", "rules": "^Search"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Forwarded-Email", "ed@example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "only an admin") {
		t.Errorf("an editor making a team through the API: %d %s", w.Code, w.Body)
	}

	// What the pages offer.
	_, body := as(t, h, http.MethodGet, "/tasks", "someone@example.com", "", nil)
	if strings.Contains(body, `<form method="post"`) || !strings.Contains(body, "someone@example.com") {
		t.Errorf("a viewer's tasks page offers to write, or doesn't say who:\n%s", body)
	}
	_, body = as(t, h, http.MethodGet, "/tasks", "ed@example.com", "", nil)
	if !strings.Contains(body, `<form method="post"`) {
		t.Errorf("an editor's tasks page should offer a task:\n%s", body)
	}
	_, body = as(t, h, http.MethodGet, "/settings", "ed@example.com", "", nil)
	if strings.Contains(body, `<form method="post"`) {
		t.Errorf("an editor's settings page offers to change them:\n%s", body)
	}
	_, body = as(t, h, http.MethodGet, "/settings", "boss@example.com", "", nil)
	if !strings.Contains(body, `<form method="post"`) {
		t.Errorf("an admin's settings page should offer to change them:\n%s", body)
	}

	// Who the reader is.
	_, body = as(t, h, http.MethodGet, "/api/v1/me", "ops@example.com", "dev,sre", nil)
	var me struct {
		User   string
		Groups []string
		Role   string
	}
	if err := json.Unmarshal([]byte(body), &me); err != nil || me.User != "ops@example.com" || me.Role != "editor" || len(me.Groups) != 2 {
		t.Errorf("me: %s %v", body, err)
	}

	// A refusal is in the reader's language.
	r = httptest.NewRequest(http.MethodPost, "/teams", strings.NewReader(team.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("X-Forwarded-Email", "ed@example.com")
	r.Header.Set("Accept-Language", "ko")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "관리자만") {
		t.Errorf("the refusal in Korean:\n%s", w.Body)
	}

	// "*" is everyone the proxy names, never whoever it doesn't.
	s2, _ := server(t, func(c *Config) { c.UserHeader, c.Editors = "X-Forwarded-Email", []string{"*"} })
	h2 := s2.Handler()
	if code, _ := as(t, h2, http.MethodPost, "/tasks", "anyone@example.com", "", task); code != http.StatusSeeOther {
		t.Errorf("* should let anyone signed in write: %d", code)
	}
	if code, _ := as(t, h2, http.MethodPost, "/tasks", "", "", task); code != http.StatusForbidden {
		t.Errorf("* shouldn't let the unnamed write: %d", code)
	}
}

// What happens is told to the webhook, as JSON with a link, and to Slack, as
// a line in --lang; one that fails is tried again.
func TestNotify(t *testing.T) {
	var mu sync.Mutex
	got := map[string][]string{}
	fail := 1
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/hook" && fail > 0 {
			fail--
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		got[r.URL.Path] = append(got[r.URL.Path], string(b))
	}))
	defer recv.Close()
	dir := t.TempDir()
	hook, slack := filepath.Join(dir, "hook"), filepath.Join(dir, "slack")
	os.WriteFile(hook, []byte(recv.URL+"/hook"), 0o600)
	os.WriteFile(slack, []byte(recv.URL+"/slack"), 0o600)
	s, _ := server(t, func(c *Config) {
		c.UserHeader, c.NotifyWebhook, c.Slack, c.NotifySlack = "X-Forwarded-Email", hook, slack, true
		c.ExternalURL, c.Lang = "https://lapilli.example.com/", "ko"
	})
	h := s.Handler()
	ctx := context.Background()
	if code, body := api(t, h, "POST", "/api/v1/tasks", `{"title": "<!channel> tune Errors", "rule": "Errors", "link": "https://tracker.example/LAP-1"}`); code != http.StatusCreated {
		t.Fatalf("task: %d %s", code, body)
	}

	if err := s.sendDue(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(got["/hook"]) != 0 || len(got["/slack"]) != 1 {
		t.Fatalf("first try: %v", got)
	}
	line := got["/slack"][0]
	mu.Unlock()
	var msg struct{ Text string }
	json.Unmarshal([]byte(line), &msg)
	if !strings.Contains(msg.Text, "bo@example.com 님이 작업 #1") || !strings.Contains(msg.Text, "&lt;!channel&gt;") || strings.Contains(msg.Text, "<!channel>") ||
		!strings.HasSuffix(msg.Text, " https://lapilli.example.com/tasks/1") {
		t.Errorf("the line to Slack: %q", msg.Text)
	}

	// Not again until it is due; then the webhook's turn.
	s.sendDue(ctx, time.Now())
	if err := s.sendDue(ctx, time.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	hooks := got["/hook"]
	mu.Unlock()
	if len(hooks) != 1 {
		t.Fatalf("the webhook, tried again: %v", hooks)
	}
	var n struct {
		Event string
		By    string
		URL   string
		Task  struct {
			ID   int64
			Link string
		}
	}
	if err := json.Unmarshal([]byte(hooks[0]), &n); err != nil || n.Event != "task.created" || n.By != "bo@example.com" ||
		n.URL != "https://lapilli.example.com/tasks/1" || n.Task.Link != "https://tracker.example/LAP-1" {
		t.Errorf("the webhook's JSON: %s %v", hooks[0], err)
	}

	_, body, _ := get(t, h, "/metrics")
	for _, want := range []string{`lapilli_serve_notices_total{to="webhook",result="failed"} 1`, `lapilli_serve_notices_total{to="webhook",result="sent"} 1`,
		`lapilli_serve_notices_total{to="slack",result="sent"} 1`, "lapilli_serve_notices_unsent 0"} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics want %s:\n%s", want, body)
		}
	}

	// Each kind of notice has its line.
	for _, req := range []struct{ method, path, body string }{
		{"PUT", "/api/v1/tasks/1", `{"title": "tune Errors", "assignee": "ana@example.com", "status": "doing"}`},
		{"POST", "/api/v1/comments", `{"on": "task", "key": "1", "body": "on it"}`},
		{"POST", "/api/v1/windows", `{"title": "deploy", "start": "2026-11-04T01:00:00Z", "end": "2026-11-04T02:00:00Z", "every": "week"}`},
		{"POST", "/api/v1/incidents", `{"title": "Search slow", "start": "2026-11-02T10:00:00Z"}`},
	} {
		if code, body := api(t, h, req.method, req.path, req.body); code >= 300 {
			t.Fatalf("%s %s: %d %s", req.method, req.path, code, body)
		}
	}
	if err := s.sendDue(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	lines := strings.Join(got["/slack"][1:], "\n")
	mu.Unlock()
	for _, want := range []string{"담당이 ana@example.com 님이 됐어요", "진행 중(으)로 바꿨어요", "작업 1에 댓글", "점검 시간대 \\\"deploy\\\"", "인시던트"} {
		if !strings.Contains(lines, want) {
			t.Errorf("want %q in:\n%s", want, lines)
		}
	}
}

// A quiet window that repeats, kept from the form and shown as it repeats.
func TestRepeatingWindowForm(t *testing.T) {
	s, _ := server(t, nil)
	h := s.Handler()
	form := url.Values{"title": {"nightly batch"}, "start": {"2026-11-03T02:00"}, "end": {"2026-11-03T03:00"}, "every": {"day"}, "until": {"2026-11-30"}}
	if code, body, _ := post(t, h, "/windows", form, "same-origin"); code != http.StatusSeeOther {
		t.Fatalf("window: %d %s", code, body)
	}
	_, body, _ := get(t, h, "/windows")
	if !strings.Contains(body, "every day, until 2026-11-30") {
		t.Errorf("the windows page:\n%s", body)
	}
	_, body, _ = get(t, h, "/windows/1")
	if !strings.Contains(body, `<option value="day" selected>`) || !strings.Contains(body, `value="2026-11-30"`) {
		t.Errorf("the window's form keeps how it repeats:\n%s", body)
	}
	for _, bad := range []url.Values{
		{"title": {"x"}, "start": {"2026-11-03T02:00"}, "end": {"2026-11-03T03:00"}, "every": {"month"}},
		{"title": {"x"}, "start": {"2026-11-03T02:00"}, "end": {"2026-11-03T03:00"}, "every": {"day"}, "until": {"30/11/2026"}},
		{"title": {"x"}, "start": {"2026-11-03T02:00"}, "end": {"2026-11-04T03:00"}, "every": {"day"}},
		{"title": {"x"}, "start": {"2026-11-03T02:00"}, "end": {"2026-11-03T03:00"}, "every": {"day"}, "until": {"2026-11-01"}},
	} {
		if code, _, _ := post(t, h, "/windows", bad, "same-origin"); code != http.StatusBadRequest {
			t.Errorf("%v: %d", bad, code)
		}
	}
}
