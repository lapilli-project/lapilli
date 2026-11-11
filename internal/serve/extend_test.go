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

	// A group is named whole: "Platform Admins Lite" isn't "sre".
	if code, _ := as(t, h, http.MethodPost, "/tasks", "ops@example.com", "Platform sre Lite", task); code != http.StatusForbidden {
		t.Errorf("a group whose name has another's in it: %d", code)
	}
	// Named twice, a header is the caller's as much as the proxy's.
	r = httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(task.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Add("X-Forwarded-Email", "boss@example.com")
	r.Header.Add("X-Forwarded-Email", "someone@example.com")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("the user header twice: %d", w.Code)
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
	var keys []string
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
		if k := r.Header.Get("Idempotency-Key"); k != "" {
			keys = append(keys, k)
		}
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
	send := func(at time.Time) {
		t.Helper()
		for _, d := range s.dests() {
			if err := s.sendDue(ctx, d, at); err != nil {
				t.Fatal(err)
			}
		}
	}

	send(time.Now())
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
	send(time.Now())
	send(time.Now().Add(2 * time.Minute))
	mu.Lock()
	hooks := got["/hook"]
	mu.Unlock()
	if len(hooks) != 1 {
		t.Fatalf("the webhook, tried again: %v", hooks)
	}
	var n struct {
		ID    string
		Event string
		By    string
		URL   string
		Task  struct {
			ID        int64
			Link      string
			CreatedBy string `json:"created_by"`
		}
	}
	if err := json.Unmarshal([]byte(hooks[0]), &n); err != nil || n.Event != "task.created" || n.By != "bo@example.com" || n.ID == "" || n.ID != keys[0] ||
		n.URL != "https://lapilli.example.com/tasks/1" || n.Task.Link != "https://tracker.example/LAP-1" || n.Task.CreatedBy != "bo@example.com" {
		t.Errorf("the webhook's JSON: %s, key %q, %v", hooks[0], keys, err)
	}

	// A tracker puts the issue's link back, and only that.
	if code, body := api(t, h, "PATCH", "/api/v1/tasks/1", `{"link": "https://tracker.example/LAP-2"}`); code != 200 || !strings.Contains(body, "LAP-2") || !strings.Contains(body, "tune Errors") {
		t.Errorf("patch: %d %s", code, body)
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
	send(time.Now())
	mu.Lock()
	lines := strings.Join(got["/slack"][1:], "\n")
	mu.Unlock()
	for _, want := range []string{"담당이 \\\"ana@example.com\\\" 님이 됐어요", "진행 중(으)로 바꿨어요", "작업 \\\"1\\\" 에 댓글", "메인터넌스 윈도우 \\\"deploy\\\"", "인시던트"} {
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
		{"title": {"x"}, "start": {"2026-11-03T02:00"}, "end": {"2026-11-03T03:00"}, "every": {"year"}},
		{"title": {"x"}, "start": {"2026-11-03T02:00"}, "end": {"2026-11-03T03:00"}, "every": {"day"}, "until": {"30/11/2026"}},
		{"title": {"x"}, "start": {"2026-11-03T02:00"}, "end": {"2026-11-04T03:00"}, "every": {"day"}},
		{"title": {"x"}, "start": {"2026-11-03T02:00"}, "end": {"2026-11-03T03:00"}, "every": {"day"}, "until": {"2026-11-01"}},
	} {
		if code, _, _ := post(t, h, "/windows", bad, "same-origin"); code != http.StatusBadRequest {
			t.Errorf("%v: %d", bad, code)
		}
	}
}

// A repeating window excuses the firings in each of its repeats, and its
// page lists them all.
func TestRepeatingWindowExcuses(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, nil)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	h := s.Handler()
	// 04:00 to 12:00 each day: two search firings on the 1st, two on the
	// 2nd.
	code, body := api(t, h, "POST", "/api/v1/windows", `{"title": "reindex", "start": "2026-11-01T04:00:00Z", "end": "2026-11-01T12:00:00Z", "every": "day", "labels": {"service": ["search"]}}`)
	if code != http.StatusCreated {
		t.Fatalf("window: %d %s", code, body)
	}
	_, body, _ = get(t, h, "/windows/1")
	if n := strings.Count(body, `<td><a href="/rules/Errors">`); n != 4 {
		t.Errorf("%d firings listed, want 4:\n%s", n, body)
	}
	_, body = api(t, h, "GET", "/api/v1/report?from=2026-10-31T00:00:00Z&to=2026-11-03T12:00:00Z&firings=false", "")
	if !strings.Contains(body, `"excused": 4`) {
		t.Errorf("the report:\n%s", body)
	}
	// A new title judges nothing again.
	code, _ = api(t, h, "PUT", "/api/v1/windows/1", `{"title": "search reindex", "start": "2026-11-01T04:00:00Z", "end": "2026-11-01T12:00:00Z", "every": "day", "labels": {"service": ["search"]}}`)
	if pending, _ := s.l.Meta(ctx, "pending"); code != 200 || (pending != "" && pending != "[]" && pending != "null") {
		t.Errorf("renamed: %d, pending %q", code, pending)
	}
}

// The UI's font comes from the server itself, as a font, with its licence;
// the CSP lets fonts load from there and nowhere else.
func TestFont(t *testing.T) {
	s, _ := server(t, nil)
	h := s.Handler()
	for path, want := range map[string]string{"/static/heebo-latin.woff2": "font/woff2", "/static/heebo-latin-ext.woff2": "font/woff2", "/static/OFL-Heebo.txt": "text/plain"} {
		code, body, hd := get(t, h, path)
		if code != 200 || !strings.HasPrefix(hd.Get("Content-Type"), want) || len(body) < 1000 {
			t.Errorf("%s: %d %q, %d bytes", path, code, hd.Get("Content-Type"), len(body))
		}
	}
	// A page asks for this release's stylesheet, which can then be kept.
	_, page, _ := get(t, h, "/runs")
	if !strings.Contains(page, `href="/static/lapilli.css?v=`+staticSum+`"`) {
		t.Errorf("the page doesn't ask for this release's stylesheet:\n%s", page)
	}
	_, css, hd := get(t, h, "/static/lapilli.css?v="+staticSum)
	if !strings.Contains(hd.Get("Cache-Control"), "immutable") {
		t.Errorf("this release's stylesheet: %q", hd.Get("Cache-Control"))
	}
	if _, _, old := get(t, h, "/static/lapilli.css?v=0ld"); strings.Contains(old.Get("Cache-Control"), "immutable") {
		t.Errorf("another release's stylesheet is kept for good: %q", old.Get("Cache-Control"))
	}
	if !strings.Contains(css, `url("heebo-latin.woff2")`) || !strings.Contains(hd.Get("Content-Security-Policy"), "font-src 'self'") {
		t.Errorf("the stylesheet or the CSP doesn't load the font: %q", hd.Get("Content-Security-Policy"))
	}
}

// A page kept by a browser isn't taken for the page of another process on
// the same ledger, a new release's above all: the generation starts again.
func TestETagAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	at := func() string {
		s, _ := server(t, func(c *Config) { c.Ledger = dir })
		defer s.Close()
		_, _, hd := get(t, s.Handler(), "/runs")
		return hd.Get("ETag")
	}
	if a, b := at(), at(); a == "" || a == b {
		t.Errorf("the same ETag after a restart: %q, %q", a, b)
	}
}

// A page elsewhere that points a name of its own at the server (DNS
// rebinding) is refused, reading or writing; the names the server is
// reached by are answered.
func TestHosts(t *testing.T) {
	s, _ := server(t, func(c *Config) {
		c.ExternalURL = "https://lapilli.example.com"
		c.AllowedHosts = []string{"ui.corp.example"}
	})
	h := s.Handler()
	for host, want := range map[string]int{
		"127.0.0.1:8080": 200, "localhost:8080": 200, "[::1]:8080": 200, "10.0.3.7:8080": 200,
		"lapilli:8080": 200, "lapilli.monitoring.svc:8080": 200, "lapilli.monitoring.svc.cluster.local": 200,
		"lapilli.example.com": 200, "LAPILLI.example.com.:443": 200, "ui.corp.example": 200,
		"rebind.attacker.example:8080": 421, "lapilli.example.com.attacker.example": 421, "": 421,
	} {
		// What a refused host writes is told apart from what the others do.
		lead := "lead=0s"
		if want == 421 {
			lead = "lead=7m"
		}
		for _, req := range []*http.Request{httptest.NewRequest(http.MethodGet, "/api/v1/report?firings=false", nil),
			httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(lead))} {
			req.Host = host
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if got := w.Code; (want == 421) != (got == 421) {
				t.Errorf("%s %s to %q: %d", req.Method, req.URL.Path, host, got)
			}
		}
	}
	// Over loopback (this machine, a port-forward), names a company's or a
	// cluster's DNS answers aren't, as someone with a hand in it could
	// point them here.
	for host, want := range map[string]int{"localhost:8080": 200, "127.0.0.1:8080": 200, "lapilli.example.com": 200,
		"lapilli": 421, "lapilli.monitoring.svc": 421, "db.corp.internal": 421} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/report?firings=false", nil)
		r.Host, r.RemoteAddr = host, "127.0.0.1:50000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if (want == 421) != (w.Code == 421) {
			t.Errorf("over loopback, %q: %d", host, w.Code)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.Host = "rebind.attacker.example"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("health checks are answered whatever the name: %d", w.Code)
	}
	if set, err := s.l.Settings(context.Background()); err != nil || set["lead"].Value == "7m" {
		t.Errorf("a refused write changed the settings: %v %v", set, err)
	}
}

// Every firing of a busy month would take gigabytes: a report gives so many
// at most, and one such report is worked out at a time.
func TestHeavyReport(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, nil)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	h := s.Handler()
	was, wait := reportFirings, busyWait
	defer func() { reportFirings, busyWait = was, wait }()
	reportFirings, busyWait = 5, 50*time.Millisecond
	if code, body, _ := get(t, h, "/api/v1/report"); code != http.StatusBadRequest || !strings.Contains(body, "more than 5 at a time") {
		t.Errorf("too many firings: %d %s", code, body)
	}
	if code, _, _ := get(t, h, "/api/v1/report?firings=false"); code != 200 {
		t.Errorf("without firings: %d", code)
	}
	reportFirings = 1000
	s.heavy <- struct{}{} // another heavy request holds the turn
	if code, body, _ := get(t, h, "/api/v1/report?to=2026-11-03T10:00:00Z"); code != http.StatusServiceUnavailable || !strings.Contains(body, "busy") {
		t.Errorf("while another holds the turn: %d %s", code, body)
	}
	<-s.heavy
	if code, _, _ := get(t, h, "/api/v1/report?to=2026-11-03T11:00:00Z"); code != 200 {
		t.Errorf("once the turn is free: %d", code)
	}
}

// The metrics listener serves the metrics and the health checks, and none
// of the UI, the API or MCP: a NetworkPolicy can let Prometheus reach it
// alone.
func TestMetricsListener(t *testing.T) {
	s, _ := server(t, nil)
	h := s.MetricsHandler()
	for path, want := range map[string]int{"/metrics": 200, "/healthz": 200, "/readyz": 503,
		"/": 404, "/api/v1/report": 404, "/settings": 404, "/mcp": 404} {
		if code, _, _ := get(t, h, path); code != want {
			t.Errorf("%s: %d, want %d", path, code, want)
		}
	}
}

// "user:" names a person alone and "group:" a group alone, so one who can
// pick their own name can't take a group's role.
func TestRolePrefixes(t *testing.T) {
	s, _ := server(t, func(c *Config) {
		c.UserHeader, c.GroupsHeader = "X-Forwarded-Email", "X-Forwarded-Groups"
		c.Admins = []string{"group:sre team", "user:boss@example.com"}
		c.Editors = []string{"ops"}
	})
	h := s.Handler()
	for _, c := range []struct {
		who, groups string
		want        string
	}{
		{"sre team", "", "viewer"},                   // a person named like the group
		{"someone@example.com", "sre team", "admin"}, // in the group
		{"boss@example.com", "", "admin"},
		{"x@example.com", "boss@example.com", "viewer"}, // a group named like the person
		{"ops", "", "editor"},                           // unprefixed: either, as before
		{"x@example.com", "ops", "editor"},
	} {
		if _, body := as(t, h, http.MethodGet, "/api/v1/me", c.who, c.groups, nil); !strings.Contains(body, `"role":"`+c.want+`"`) {
			t.Errorf("%q in %q: %s, want %s", c.who, c.groups, body, c.want)
		}
	}
}
