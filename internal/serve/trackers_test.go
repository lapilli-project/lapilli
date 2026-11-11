package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

const (
	trIncTok   = "an-incidents-token-long-enough"
	trFirTok   = "a-firings-token-long-enough"
	trTasksTok = "a-tasks-token-long-enough"
)

// trackerServer is a server whose incidents port takes all three tokens,
// with tasks: 1 open (team payments), 2 doing, 3 done, 4 open and linked,
// 5 dropped and linked.
func trackerServer(t *testing.T, change func(*Config)) *Server {
	t.Helper()
	s, _ := server(t, func(c *Config) {
		c.IncidentsListen = "127.0.0.1:0"
		c.IncidentsToken, c.FiringsToken, c.TasksToken = tokenFile(t, trIncTok), tokenFile(t, trFirTok), tokenFile(t, trTasksTok)
		c.UserHeader, c.Editors = "X-Forwarded-Email", []string{"ana@example.com"}
		c.ExternalURL = "https://lapilli.example.com/"
		if change != nil {
			change(c)
		}
	})
	ctx := context.Background()
	team, err := s.l.PutTeam(ctx, ledger.Team{Name: "payments", Rules: "Errors"}, "ana@example.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range []ledger.Task{
		{Title: "Tune Errors", Kind: "tune", Rule: "Errors", TeamID: team, Assignee: "bo", Body: "Too loud at night."},
		{Title: "Write a rule for checkout", Kind: "write", Status: "doing"},
		{Title: "Retire Old", Kind: "retire", Status: "done"},
		{Title: "Decide about INC-1", Kind: "decide", Incident: "INC-1", Link: "https://tracker.example.com/issues/4"},
		{Title: "Drop it", Status: "dropped", Link: "https://tracker.example.com/issues/5"},
	} {
		if _, err := s.l.PutTask(ctx, task, "ana@example.com"); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// tracker sends a request to a handler, with a token when given, and the
// header a sign-in proxy would set.
func tracker(t *testing.T, h http.Handler, method, path, tok, body string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Forwarded-Email", "ana@example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

type trackerPageOut struct {
	Tasks []struct {
		ID       int64     `json:"id"`
		Status   string    `json:"status"`
		Link     string    `json:"link"`
		Team     string    `json:"team"`
		URL      string    `json:"url"`
		By       string    `json:"by"`
		StatusBy string    `json:"status_by"`
		StatusAt time.Time `json:"status_at"`
	} `json:"tasks"`
	Next int64 `json:"next"`
}

func trackerIDs(t *testing.T, body string) ([]int64, trackerPageOut) {
	t.Helper()
	var p trackerPageOut
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	ids := []int64{}
	for _, x := range p.Tasks {
		ids = append(ids, x.ID)
	}
	return ids, p
}

// The tasks token reads tasks a page at a time, linked or not, at the
// statuses asked for.
func TestTrackerTasks(t *testing.T) {
	s := trackerServer(t, nil)
	port := s.IncidentsHandler()
	for _, c := range []struct {
		path string
		want []int64
		next int64
	}{
		{"/api/v1/tasks", []int64{1, 2}, 0},
		{"/api/v1/tasks?linked=false&status=open", []int64{1}, 0},
		{"/api/v1/tasks?linked=false&status=open,doing,done,dropped", []int64{1, 2, 3}, 0},
		{"/api/v1/tasks?linked=true", []int64{4}, 0},
		{"/api/v1/tasks?linked=true&status=open,doing,done,dropped", []int64{4, 5}, 0},
		{"/api/v1/tasks?limit=1", []int64{1}, 1},
		{"/api/v1/tasks?limit=1&after=1", []int64{2}, 2},
		{"/api/v1/tasks?limit=1&after=2", []int64{}, 0},
		{"/api/v1/tasks?linked=true&status=done,dropped&updated_after=2026-01-01T00:00:00Z", []int64{5}, 0},
		{"/api/v1/tasks?linked=true&status=done,dropped&updated_after=" + time.Now().Add(time.Hour).UTC().Format(time.RFC3339), []int64{}, 0},
	} {
		code, body := tracker(t, port, "GET", c.path, trTasksTok, "")
		if code != http.StatusOK {
			t.Errorf("%s: %d %s", c.path, code, body)
			continue
		}
		ids, p := trackerIDs(t, body)
		if jsonOf(ids) != jsonOf(c.want) || p.Next != c.next {
			t.Errorf("%s: %v next %d, want %v next %d", c.path, ids, p.Next, c.want, c.next)
		}
	}
	// What an issue is made from: the team's name, and the task's page.
	_, body := tracker(t, port, "GET", "/api/v1/tasks?status=open", trTasksTok, "")
	if _, p := trackerIDs(t, body); len(p.Tasks) != 1 || p.Tasks[0].Team != "payments" || p.Tasks[0].URL != "https://lapilli.example.com/tasks/1" {
		t.Errorf("task 1: %s", body)
	}
	// Who set each task's status, and when: the job for 4, whose status it
	// set; whoever made 3, though the job wrote its link since.
	if code, body := tracker(t, port, "PATCH", "/api/v1/tasks/3", trTasksTok, `{"link": "https://tracker.example.com/issues/3"}`); code != http.StatusOK {
		t.Fatalf("link: %d %s", code, body)
	}
	if code, body := tracker(t, port, "PATCH", "/api/v1/tasks/4", trTasksTok, `{"status": "doing"}`); code != http.StatusOK {
		t.Fatalf("status: %d %s", code, body)
	}
	_, body = tracker(t, port, "GET", "/api/v1/tasks?linked=true&status=doing,done,dropped", trTasksTok, "")
	if ids, p := trackerIDs(t, body); jsonOf(ids) != "[3,4,5]" || p.Tasks[0].By != ledger.TaskTracker || p.Tasks[0].StatusBy != "ana@example.com" ||
		p.Tasks[1].StatusBy != ledger.TaskTracker || p.Tasks[2].StatusBy != "ana@example.com" || p.Tasks[0].StatusAt.IsZero() {
		t.Errorf("who set the status: %s", body)
	}
	for _, path := range []string{
		"/api/v1/tasks?linked=maybe",
		"/api/v1/tasks?status=closed",
		"/api/v1/tasks?status=open,",
		"/api/v1/tasks?after=-1",
		"/api/v1/tasks?after=x",
		"/api/v1/tasks?limit=0",
		"/api/v1/tasks?limit=201",
		"/api/v1/tasks?updated_after=yesterday",
		"/api/v1/tasks?updated_after=1700000000",
		"/api/v1/tasks?updated_after=2026-11-02",
	} {
		if code, body := tracker(t, port, "GET", path, trTasksTok, ""); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", path, code, body)
		}
	}
	// One task, or comments, aren't for the job to read.
	for _, path := range []string{"/api/v1/tasks/1", "/api/v1/teams", "/api/v1/comments/1"} {
		if code, _ := tracker(t, port, "GET", path, trTasksTok, ""); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
			t.Errorf("%s on the incidents port: %d", path, code)
		}
	}
}

func jsonOf(v any) string { b, _ := json.Marshal(v); return string(b) }

// The tasks token sets a task's link, once, and its status, and nothing
// else; what it does is noted as the job, and isn't people's use.
func TestTrackerPatch(t *testing.T) {
	hook := tokenFile(t, "https://hooks.example.com/lapilli")
	s := trackerServer(t, func(c *Config) { c.NotifyWebhook = hook })
	s.now = time.Now
	ctx := context.Background()
	port := s.IncidentsHandler()
	unsent, _ := s.l.Unsent(ctx)
	if unsent == 0 {
		t.Fatal("people's tasks aren't told: the test would show nothing")
	}
	before, err := s.workOutcomes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.Use.Edits == 0 {
		t.Fatal("people's edits aren't counted: the test would show nothing")
	}
	for _, c := range []struct {
		path, body string
		want       int
	}{
		{"/api/v1/tasks/1", `{"title": "Retitled"}`, http.StatusBadRequest},
		{"/api/v1/tasks/1", `{"link": "https://tracker.example.com/issues/1", "assignee": "eve"}`, http.StatusBadRequest},
		{"/api/v1/tasks/1", `{}`, http.StatusBadRequest},
		{"/api/v1/tasks/1", `not json`, http.StatusBadRequest},
		{"/api/v1/tasks/1", `{"link": 5}`, http.StatusBadRequest},
		{"/api/v1/tasks/1", `{"link": "http://tracker.example.com/issues/1"}`, http.StatusBadRequest},
		{"/api/v1/tasks/1", `{"link": "javascript:alert(1)"}`, http.StatusBadRequest},
		{"/api/v1/tasks/1", `{"link": "https://tracker.example.com@evil.example/issues/1"}`, http.StatusBadRequest},
		{"/api/v1/tasks/1", `{"link": "https:///issues/1"}`, http.StatusBadRequest},
		{"/api/v1/tasks/1", `{"link": "https://tracker.example.com/` + strings.Repeat("a", 2049-len("https://tracker.example.com/")) + `"}`, http.StatusBadRequest},
		{"/api/v1/tasks/1", `{"status": "closed"}`, http.StatusBadRequest},
		{"/api/v1/tasks/4", `{"link": "https://tracker.example.com/issues/40"}`, http.StatusBadRequest},
		{"/api/v1/tasks/99", `{"status": "done"}`, http.StatusNotFound},
		{"/api/v1/tasks/x", `{"status": "done"}`, http.StatusNotFound},
		{"/api/v1/tasks/1", `{"link": "https://tracker.example.com/` + strings.Repeat("a", 20000) + `"}`, http.StatusRequestEntityTooLarge},
	} {
		if code, body := tracker(t, port, "PATCH", c.path, trTasksTok, c.body); code != c.want {
			t.Errorf("PATCH %s %.80s: %d %s, want %d", c.path, c.body, code, body, c.want)
		}
	}
	if task, _ := s.l.TaskByID(ctx, 1); task.Title != "Tune Errors" || task.Link != "" || task.Status != "open" || task.Assignee != "bo" {
		t.Fatalf("refused writes changed task 1: %+v", task)
	}
	cs, _ := s.l.ChangesSince(ctx, time.Unix(0, 0), 100)
	made := len(cs)
	long := "https://tracker.example.com/" + strings.Repeat("a", 2048-len("https://tracker.example.com/"))
	for _, c := range []struct{ path, body, link, status string }{
		{"/api/v1/tasks/1", `{"link": "` + long + `"}`, long, "open"},
		{"/api/v1/tasks/2", `{"link": "https://tracker.example.com/issues/2", "status": "done"}`, "https://tracker.example.com/issues/2", "done"},
		// The same link again changes nothing; the status still can.
		{"/api/v1/tasks/4", `{"link": "https://tracker.example.com/issues/4", "status": "dropped"}`, "https://tracker.example.com/issues/4", "dropped"},
		{"/api/v1/tasks/5", `{"status": "open"}`, "https://tracker.example.com/issues/5", "open"},
	} {
		code, body := tracker(t, port, "PATCH", c.path, trTasksTok, c.body)
		var got ledger.Task
		json.Unmarshal([]byte(body), &got)
		if code != http.StatusOK || got.Link != c.link || got.Status != c.status || got.By != ledger.TaskTracker {
			t.Errorf("PATCH %s: %d %.200s", c.path, code, body)
		}
	}
	// What is so already isn't written again.
	if code, _ := tracker(t, port, "PATCH", "/api/v1/tasks/4", trTasksTok, `{"link": "https://tracker.example.com/issues/4", "status": "dropped"}`); code != http.StatusOK {
		t.Errorf("the same again: %d", code)
	}
	cs, _ = s.l.ChangesSince(ctx, time.Unix(0, 0), 100)
	if len(cs) != made+4 {
		t.Errorf("%d changes, want %d", len(cs), made+4)
	}
	for _, c := range cs[made:] {
		if c.By != ledger.TaskTracker {
			t.Errorf("change by %q, want %q, whatever a header says", c.By, ledger.TaskTracker)
		}
	}
	// Nor told: the tracker has told people.
	if n, _ := s.l.Unsent(ctx); n != unsent {
		t.Errorf("the job's writes were told: %d notices, were %d", n, unsent)
	}
	after, err := s.workOutcomes(ctx)
	if err != nil || after.Use.Edits != before.Use.Edits {
		t.Errorf("the job's writes counted as use: %d edits, were %d (%v)", after.Use.Edits, before.Use.Edits, err)
	}
	// The link is shown as a link that gives the tracker nothing of
	// Lapilli's page.
	for _, path := range []string{"/tasks/2", "/tasks?status=all"} {
		if _, page, _ := get(t, s.Handler(), path); !strings.Contains(page, `<a href="https://tracker.example.com/issues/2" rel="noopener noreferrer"`) {
			t.Errorf("%s doesn't show the link with rel=\"noopener noreferrer\"", path)
		}
	}
	// On the UI's port, the job's name is no one's: a header naming it
	// isn't taken.
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-Email", ledger.TaskTracker)
	if got := s.user(r); got != "anonymous" {
		t.Errorf("a header naming the job is %q", got)
	}
}

// The tasks token is taken on the incidents port alone, for its paths
// alone; the others aren't taken on its paths; each has its own rate and
// turn; and a token held in two files isn't taken.
func TestTrackerToken(t *testing.T) {
	s := trackerServer(t, nil)
	port, ui := s.IncidentsHandler(), s.Handler()
	week := "?from=2026-11-02T00:00:00Z&to=2026-11-09T00:00:00Z"
	for _, c := range []struct {
		h                       http.Handler
		method, path, tok, body string
		want                    int
	}{
		{port, "GET", "/api/v1/tasks", "", "", http.StatusUnauthorized},
		{port, "GET", "/api/v1/tasks", "a-tasks-token-long-enougH", "", http.StatusUnauthorized},
		{port, "GET", "/api/v1/tasks", trIncTok, "", http.StatusUnauthorized},
		{port, "GET", "/api/v1/tasks", trFirTok, "", http.StatusUnauthorized},
		{port, "PATCH", "/api/v1/tasks/1", trIncTok, `{"status": "done"}`, http.StatusUnauthorized},
		{port, "PATCH", "/api/v1/tasks/1", trFirTok, `{"status": "done"}`, http.StatusUnauthorized},
		{port, "PUT", "/api/v1/incidents/A-1", trTasksTok, `{"start": "2026-11-03T02:00:00Z"}`, http.StatusUnauthorized},
		{port, "PUT", "/api/v1/firings/cw" + week, trTasksTok, `{"rule":"a","start":"2026-11-03T02:10:00Z"}` + "\n", http.StatusUnauthorized},
		{port, "POST", "/api/v1/tasks", trTasksTok, `{"title": "x"}`, http.StatusMethodNotAllowed},
		{port, "DELETE", "/api/v1/tasks/1", trTasksTok, "", http.StatusMethodNotAllowed},
	} {
		if code, body := tracker(t, c.h, c.method, c.path, c.tok, c.body); code != c.want {
			t.Errorf("%s %s with %q: %d %s, want %d", c.method, c.path, c.tok, code, body, c.want)
		}
	}
	// The UI's port doesn't take the token as anyone: the request is as
	// anonymous as it would be without it.
	r := httptest.NewRequest("PATCH", "/api/v1/tasks/1", strings.NewReader(`{"status": "done"}`))
	r.Header.Set("Authorization", "Bearer "+trTasksTok)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	ui.ServeHTTP(w, r)
	if w.Code == http.StatusOK {
		t.Errorf("the token on the UI's port wrote: %d %s", w.Code, w.Body)
	}
	// Its own turn: incidents being written don't hold it up; its own
	// write does.
	s.tokenBusy <- struct{}{}
	s.firingsBusy <- struct{}{}
	if code, body := tracker(t, port, "GET", "/api/v1/tasks", trTasksTok, ""); code != http.StatusOK {
		t.Errorf("tasks while incidents and firings are written: %d %s", code, body)
	}
	<-s.tokenBusy
	<-s.firingsBusy
	s.tasksBusy <- struct{}{}
	if code, _ := tracker(t, port, "GET", "/api/v1/tasks", trTasksTok, ""); code != http.StatusTooManyRequests {
		t.Errorf("tasks while tasks are written: %d", code)
	}
	if code, body := tracker(t, port, "PUT", "/api/v1/incidents/A-1", trIncTok, `{"start": "2026-11-03T02:00:00Z"}`); code != http.StatusOK {
		t.Errorf("incidents while tasks are written: %d %s", code, body)
	}
	<-s.tasksBusy
	// The same token in two files, as the server runs: neither is taken.
	for _, c := range []struct{ file, method, path, body string }{
		{s.cfg.IncidentsToken, "PUT", "/api/v1/incidents/A-2", `{"start": "2026-11-03T02:00:00Z"}`},
		{s.cfg.FiringsToken, "PUT", "/api/v1/firings/cw" + week, `{"rule":"a","start":"2026-11-03T02:10:00Z"}` + "\n"},
	} {
		keep, _ := os.ReadFile(c.file)
		os.WriteFile(c.file, []byte(trTasksTok+"\n"), 0o600)
		if code, _ := tracker(t, port, "GET", "/api/v1/tasks", trTasksTok, ""); code != http.StatusUnauthorized {
			t.Errorf("tasks with the token %s holds too: %d", c.path, code)
		}
		if code, _ := tracker(t, port, c.method, c.path, trTasksTok, c.body); code != http.StatusUnauthorized {
			t.Errorf("%s with the tasks token in its file: %d", c.path, code)
		}
		os.WriteFile(c.file, keep, 0o600)
	}
	if code, _ := tracker(t, port, "GET", "/api/v1/tasks", trTasksTok, ""); code != http.StatusOK {
		t.Errorf("tasks once the tokens differ again: %d", code)
	}
	// Read-only takes nothing.
	s.cfg.ReadOnly = true
	if code, _ := tracker(t, port, "GET", "/api/v1/tasks", trTasksTok, ""); code != http.StatusForbidden {
		t.Errorf("read-only: %d", code)
	}
}

// The tasks token's rate is its own: used up, the incidents token is still
// taken.
func TestTrackerRate(t *testing.T) {
	s := trackerServer(t, nil)
	s.tasksRate = &limiter{rate: 0.001, burst: 2}
	port := s.IncidentsHandler()
	for i, want := range []int{http.StatusOK, http.StatusOK, http.StatusTooManyRequests} {
		if code, _ := tracker(t, port, "GET", "/api/v1/tasks", trTasksTok, ""); code != want {
			t.Errorf("request %d: %d, want %d", i, code, want)
		}
	}
	if code, body := tracker(t, port, "PUT", "/api/v1/incidents/A-1", trIncTok, `{"start": "2026-11-03T02:00:00Z"}`); code != http.StatusOK {
		t.Errorf("incidents once the tasks token's rate is used: %d %s", code, body)
	}
}

// The notifications token sits in webhook URLs: the tasks token holding the
// same isn't taken, and the notifications token holding the tasks token's
// isn't either.
func TestTrackerTokenNotTheNotifyToken(t *testing.T) {
	s := trackerServer(t, func(c *Config) {
		c.NotifyListen, c.NotifyToken = "127.0.0.1:0", tokenFile(t, trTasksTok)
	})
	port := s.IncidentsHandler()
	if code, _ := tracker(t, port, "GET", "/api/v1/tasks", trTasksTok, ""); code != http.StatusUnauthorized {
		t.Errorf("tasks with the notifications token: %d", code)
	}
	if !s.reused(trTasksTok) {
		t.Errorf("the notifications token holding the tasks token's is taken")
	}
	os.WriteFile(s.cfg.NotifyToken, []byte("another-token-long-enough-for-it"), 0o600)
	if code, _ := tracker(t, port, "GET", "/api/v1/tasks", trTasksTok, ""); code != http.StatusOK {
		t.Errorf("tasks once the tokens differ: %d", code)
	}
}

// Without the tasks token, the incidents port has no tasks paths.
func TestTrackerPathsOff(t *testing.T) {
	s := trackerServer(t, func(c *Config) { c.TasksToken = "" })
	if code, _ := tracker(t, s.IncidentsHandler(), "GET", "/api/v1/tasks", trTasksTok, ""); code != http.StatusNotFound {
		t.Errorf("tasks paths without the token: %d", code)
	}
}
