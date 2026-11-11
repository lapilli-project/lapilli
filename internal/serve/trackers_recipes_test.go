package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

const trackerRecipes = "../../recipes/trackers/"

// fakeIssue is an issue in a stand-in tracker; state is open, done or
// dropped, as the recipes tell them apart.
type fakeIssue struct {
	n           int
	title, body string
	labels      []string
	state       string
	updated     time.Time
}

// fakeTracker keeps issues for a tracker's stand-in API.
type fakeTracker struct {
	t       *testing.T
	mu      sync.Mutex
	issues  []*fakeIssue
	creates int
	// failCreate refuses to make an issue; badURL makes one and gives a
	// link that isn't the tracker's.
	failCreate, badURL bool
}

func (f *fakeTracker) add(title, body string, labels []string) *fakeIssue {
	i := &fakeIssue{n: len(f.issues) + 1, title: title, body: body, labels: labels, state: "open", updated: time.Now()}
	f.issues = append(f.issues, i)
	return i
}

func (f *fakeTracker) issue(n int) *fakeIssue {
	if n < 1 || n > len(f.issues) {
		return nil
	}
	return f.issues[n-1]
}

// search is what a tracker's search finds: more than the marker, as a
// search by words does (lapilli-task-1 finds lapilli-task-12 too).
func (f *fakeTracker) search(m string) []*fakeIssue {
	var out []*fakeIssue
	for _, i := range f.issues {
		if strings.Contains(i.body, m) {
			out = append(out, i)
		}
	}
	return out
}

func (f *fakeTracker) set(n int, state string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issues[n-1].state, f.issues[n-1].updated = state, at
}

// trackerAPI is a tracker's stand-in: its recipe, how it is set up, its
// API, and where its issues' pages are.
type trackerAPI struct {
	script string
	env    func(api, secret string) []string
	serve  func(f *fakeTracker) http.HandlerFunc
	link   func(n int) string
	// labels: the tracker takes labels by name. dropped: it tells an
	// issue closed as not to be done from one done.
	labels, dropped bool
}

func readBody(r *http.Request, v any) error {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

var trackerAPIs = map[string]trackerAPI{
	"github": {
		script: "github.sh",
		env: func(api, secret string) []string {
			return []string{"GITHUB_API=" + api, "GITHUB_WEB=https://github.example.com", "GITHUB_REPO=example/alerts", "GITHUB_TOKEN_FILE=" + secret}
		},
		link:    func(n int) string { return fmt.Sprintf("https://github.example.com/example/alerts/issues/%d", n) },
		labels:  true,
		dropped: true,
		serve: func(f *fakeTracker) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer gh-secret" || r.Header.Get("X-GitHub-Api-Version") == "" {
					http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
					return
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				issue := func(i *fakeIssue) map[string]any {
					out := map[string]any{"number": i.n, "html_url": fmt.Sprintf("https://github.example.com/example/alerts/issues/%d", i.n),
						"title": i.title, "body": i.body, "state": "open", "state_reason": nil, "updated_at": i.updated.UTC().Format(time.RFC3339)}
					switch i.state {
					case "done":
						out["state"], out["state_reason"] = "closed", "completed"
					case "dropped":
						out["state"], out["state_reason"] = "closed", "not_planned"
					}
					return out
				}
				n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/repos/example/alerts/issues/"))
				switch {
				case r.Method == "GET" && r.URL.Path == "/search/issues":
					q := r.URL.Query().Get("q")
					m := regexp.MustCompile(`"([^"]+)"`).FindStringSubmatch(q)
					if !strings.Contains(q, "repo:example/alerts") || !strings.Contains(q, "in:body") || m == nil {
						f.t.Errorf("github: search %q", q)
						http.Error(w, `{"message":"Validation Failed"}`, http.StatusUnprocessableEntity)
						return
					}
					items := []map[string]any{}
					for _, i := range f.search(m[1]) {
						items = append(items, issue(i))
					}
					json.NewEncoder(w).Encode(map[string]any{"total_count": len(items), "incomplete_results": false, "items": items})
				case r.Method == "POST" && r.URL.Path == "/repos/example/alerts/issues":
					var in struct {
						Title, Body string
						Labels      []string
					}
					if err := readBody(r, &in); err != nil || in.Title == "" || f.failCreate {
						http.Error(w, `{"message":"Validation Failed"}`, http.StatusUnprocessableEntity)
						return
					}
					f.creates++
					out := issue(f.add(in.Title, in.Body, in.Labels))
					if f.badURL {
						out["html_url"] = "https://evil.example/example/alerts/issues/1"
					}
					w.WriteHeader(http.StatusCreated)
					json.NewEncoder(w).Encode(out)
				case r.Method == "GET" && f.issue(n) != nil:
					json.NewEncoder(w).Encode(issue(f.issue(n)))
				case r.Method == "PATCH" && f.issue(n) != nil:
					var in struct{ State, StateReason string }
					var raw map[string]string
					readBody(r, &raw)
					in.State, in.StateReason = raw["state"], raw["state_reason"]
					if in.State != "closed" {
						f.t.Errorf("github: PATCH %v", raw)
					}
					i := f.issue(n)
					i.state, i.updated = "done", time.Now()
					if in.StateReason == "not_planned" {
						i.state = "dropped"
					}
					json.NewEncoder(w).Encode(issue(i))
				default:
					f.t.Errorf("github: %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}
		},
	},
	"gitlab": {
		script: "gitlab.sh",
		env: func(api, secret string) []string {
			return []string{"GITLAB_URL=" + api, "GITLAB_WEB=https://gitlab.example.com", "GITLAB_PROJECT=example/alerts", "GITLAB_TOKEN_FILE=" + secret}
		},
		link:   func(n int) string { return fmt.Sprintf("https://gitlab.example.com/example/alerts/-/issues/%d", n) },
		labels: true,
		serve: func(f *fakeTracker) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("PRIVATE-TOKEN") != "gl-secret" {
					http.Error(w, `{"message":"401 Unauthorized"}`, http.StatusUnauthorized)
					return
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				const base = "/api/v4/projects/example%2Falerts/issues"
				path := r.URL.EscapedPath()
				issue := func(i *fakeIssue) map[string]any {
					state := "opened"
					if i.state != "open" {
						state = "closed"
					}
					return map[string]any{"iid": i.n, "web_url": fmt.Sprintf("https://gitlab.example.com/example/alerts/-/issues/%d", i.n),
						"title": i.title, "description": i.body, "labels": i.labels, "state": state,
						"updated_at": i.updated.UTC().Format("2006-01-02T15:04:05.000Z")}
				}
				n, _ := strconv.Atoi(strings.TrimPrefix(path, base+"/"))
				switch {
				case r.Method == "GET" && path == base:
					q := r.URL.Query()
					if q.Get("in") != "description" || q.Get("search") == "" {
						f.t.Errorf("gitlab: search %v", q)
					}
					out := []map[string]any{}
					for _, i := range f.search(q.Get("search")) {
						out = append(out, issue(i))
					}
					json.NewEncoder(w).Encode(out)
				case r.Method == "POST" && path == base:
					var in struct{ Title, Description, Labels string }
					if err := readBody(r, &in); err != nil || in.Title == "" || f.failCreate {
						http.Error(w, `{"message":{"title":["can't be blank"]}}`, http.StatusBadRequest)
						return
					}
					f.creates++
					out := issue(f.add(in.Title, in.Description, strings.Split(in.Labels, ",")))
					if f.badURL {
						out["web_url"] = "https://evil.example/example/alerts/-/issues/1"
					}
					w.WriteHeader(http.StatusCreated)
					json.NewEncoder(w).Encode(out)
				case r.Method == "GET" && f.issue(n) != nil:
					json.NewEncoder(w).Encode(issue(f.issue(n)))
				case r.Method == "PUT" && f.issue(n) != nil:
					var in struct {
						StateEvent string `json:"state_event"`
					}
					readBody(r, &in)
					if in.StateEvent != "close" {
						f.t.Errorf("gitlab: PUT %+v", in)
					}
					i := f.issue(n)
					i.state, i.updated = "done", time.Now()
					json.NewEncoder(w).Encode(issue(i))
				default:
					f.t.Errorf("gitlab: %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}
		},
	},
	"jira": {
		script: "jira.sh",
		env: func(api, secret string) []string {
			return []string{"JIRA_URL=" + api, "JIRA_WEB=https://jira.example.com", "JIRA_EMAIL=ana@example.com", "JIRA_TOKEN_FILE=" + secret, "JIRA_PROJECT=OPS"}
		},
		link:    func(n int) string { return fmt.Sprintf("https://jira.example.com/browse/OPS-%d", n) },
		labels:  true,
		dropped: true,
		serve: func(f *fakeTracker) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				if user, pass, ok := r.BasicAuth(); !ok || user != "ana@example.com" || pass != "jira-secret" {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				seoul := time.FixedZone("KST", 9*3600)
				var n int
				if m := regexp.MustCompile(`^/rest/api/3/issue/OPS-([0-9]+)(/transitions)?$`).FindStringSubmatch(r.URL.Path); m != nil {
					n, _ = strconv.Atoi(m[1])
				}
				switch {
				case r.Method == "GET" && r.URL.Path == "/rest/api/3/search/jql":
					m := regexp.MustCompile(`^project = OPS AND labels = "([^"]+)"$`).FindStringSubmatch(r.URL.Query().Get("jql"))
					if m == nil {
						f.t.Errorf("jira: jql %q", r.URL.Query().Get("jql"))
						http.Error(w, `{"errorMessages":["bad jql"]}`, http.StatusBadRequest)
						return
					}
					out := []map[string]any{}
					for _, i := range f.issues {
						if slices.Contains(i.labels, m[1]) {
							out = append(out, map[string]any{"id": strconv.Itoa(10000 + i.n), "key": fmt.Sprintf("OPS-%d", i.n)})
						}
					}
					json.NewEncoder(w).Encode(map[string]any{"issues": out, "isLast": true})
				case r.Method == "POST" && r.URL.Path == "/rest/api/3/issue":
					var in struct {
						Fields struct {
							Project   struct{ Key string }
							Issuetype struct{ Name string }
							Summary   string
							Labels    []string
							// The description in Atlassian Document Format.
							Description struct {
								Type    string
								Content []struct {
									Type    string
									Content []struct{ Type, Text string }
								}
							}
						}
					}
					if err := readBody(r, &in); err != nil || in.Fields.Project.Key != "OPS" || in.Fields.Issuetype.Name != "Task" ||
						in.Fields.Summary == "" || in.Fields.Description.Type != "doc" || f.failCreate {
						http.Error(w, `{"errorMessages":[],"errors":{"summary":"You must specify a summary of the issue."}}`, http.StatusBadRequest)
						return
					}
					var lines []string
					for _, p := range in.Fields.Description.Content {
						line := ""
						for _, c := range p.Content {
							line += c.Text
						}
						lines = append(lines, line)
					}
					f.creates++
					i := f.add(in.Fields.Summary, strings.Join(lines, "\n"), in.Fields.Labels)
					key := fmt.Sprintf("OPS-%d", i.n)
					if f.badURL {
						key = "EVIL-1"
					}
					w.WriteHeader(http.StatusCreated)
					json.NewEncoder(w).Encode(map[string]any{"id": strconv.Itoa(10000 + i.n), "key": key, "self": "https://jira.example.com/rest/api/3/issue/" + key})
				case r.Method == "GET" && n > 0 && f.issue(n) != nil && !strings.HasSuffix(r.URL.Path, "/transitions"):
					i := f.issue(n)
					cat, resolution := "indeterminate", any(nil)
					switch i.state {
					case "done":
						cat, resolution = "done", map[string]any{"name": "Done"}
					case "dropped":
						cat, resolution = "done", map[string]any{"name": "Won't Do"}
					}
					json.NewEncoder(w).Encode(map[string]any{"key": fmt.Sprintf("OPS-%d", i.n), "fields": map[string]any{
						"status": map[string]any{"name": "Whatever", "statusCategory": map[string]any{"key": cat}}, "resolution": resolution,
						"updated": i.updated.In(seoul).Format("2006-01-02T15:04:05.000-0700")}})
				case r.Method == "GET" && n > 0 && f.issue(n) != nil:
					json.NewEncoder(w).Encode(map[string]any{"transitions": []map[string]any{
						{"id": "11", "name": "Start", "to": map[string]any{"statusCategory": map[string]any{"key": "indeterminate"}}},
						{"id": "31", "name": "Finish", "to": map[string]any{"statusCategory": map[string]any{"key": "done"}}},
					}})
				case r.Method == "POST" && n > 0 && f.issue(n) != nil:
					var in struct{ Transition struct{ ID string } }
					readBody(r, &in)
					if in.Transition.ID != "31" {
						f.t.Errorf("jira: transition %+v", in)
					}
					i := f.issue(n)
					i.state, i.updated = "done", time.Now()
					w.WriteHeader(http.StatusNoContent)
				default:
					f.t.Errorf("jira: %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}
		},
	},
	"linear": {
		script: "linear.sh",
		env: func(api, secret string) []string {
			return []string{"LINEAR_API=" + api + "/graphql", "LINEAR_WEB=https://linear.example.com", "LINEAR_TEAM=9cfb482a-81e3-4154-b5b9-2c805e70a02d", "LINEAR_TOKEN_FILE=" + secret}
		},
		link:    func(n int) string { return fmt.Sprintf("https://linear.example.com/example/issue/ENG-%d/an-issue", n) },
		dropped: true,
		serve: func(f *fakeTracker) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "lin-secret" || r.Method != "POST" || r.URL.Path != "/graphql" {
					http.Error(w, `{"errors":[{"message":"Authentication required"}]}`, http.StatusUnauthorized)
					return
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				var in struct {
					Query     string
					Variables map[string]any
				}
				if err := readBody(r, &in); err != nil {
					http.Error(w, "bad", http.StatusBadRequest)
					return
				}
				data := func(v any) { json.NewEncoder(w).Encode(map[string]any{"data": v}) }
				url := func(i *fakeIssue) string {
					return fmt.Sprintf("https://linear.example.com/example/issue/ENG-%d/an-issue", i.n)
				}
				const team = "9cfb482a-81e3-4154-b5b9-2c805e70a02d"
				n := 0
				if id, ok := in.Variables["id"].(string); ok {
					n, _ = strconv.Atoi(strings.TrimPrefix(id, "ENG-"))
				}
				switch q := in.Query; {
				case strings.Contains(q, "issues(first: 20"):
					if in.Variables["team"] != team {
						f.t.Errorf("linear: team %v", in.Variables)
					}
					nodes := []map[string]any{}
					m, _ := in.Variables["m"].(string)
					for _, i := range f.search(m) {
						nodes = append(nodes, map[string]any{"url": url(i), "description": i.body})
					}
					data(map[string]any{"issues": map[string]any{"nodes": nodes}})
				case strings.Contains(q, "issueCreate"):
					v, _ := in.Variables["in"].(map[string]any)
					title, _ := v["title"].(string)
					desc, _ := v["description"].(string)
					if v["teamId"] != team || title == "" || f.failCreate {
						json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]any{{"message": "Argument Validation Error"}}})
						return
					}
					f.creates++
					i := f.add(title, desc, nil)
					u := url(i)
					if f.badURL {
						u = "https://evil.example/example/issue/ENG-1/x"
					}
					data(map[string]any{"issueCreate": map[string]any{"success": true, "issue": map[string]any{"url": u}}})
				case strings.Contains(q, "issueUpdate") && f.issue(n) != nil:
					i := f.issue(n)
					switch in.Variables["state"] {
					case "s-done":
						i.state = "done"
					case "s-canceled":
						i.state = "dropped"
					default:
						f.t.Errorf("linear: update %v", in.Variables)
					}
					i.updated = time.Now()
					data(map[string]any{"issueUpdate": map[string]any{"success": true}})
				case strings.Contains(q, "issue(id:") && f.issue(n) != nil:
					i := f.issue(n)
					typ := map[string]string{"open": "started", "done": "completed", "dropped": "canceled"}[i.state]
					data(map[string]any{"issue": map[string]any{"updatedAt": i.updated.UTC().Format("2006-01-02T15:04:05.000Z"), "state": map[string]any{"type": typ}}})
				case strings.Contains(q, "team(id:"):
					data(map[string]any{"team": map[string]any{"states": map[string]any{"nodes": []map[string]any{
						{"id": "s-backlog", "type": "backlog", "position": 0},
						{"id": "s-done-late", "type": "completed", "position": 5},
						{"id": "s-done", "type": "completed", "position": 3},
						{"id": "s-canceled", "type": "canceled", "position": 4},
					}}}})
				default:
					f.t.Errorf("linear: %s %v", in.Query, in.Variables)
					json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]any{{"message": "Not found"}}})
				}
			}
		},
	},
}

// trackerEnv is the environment without what the recipes read, so a
// developer's own settings don't change what is tested.
func trackerEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "LAPILLI_") || strings.HasPrefix(name, "GITHUB_") || strings.HasPrefix(name, "GITLAB_") ||
			strings.HasPrefix(name, "JIRA_") || strings.HasPrefix(name, "LINEAR_") ||
			slices.Contains([]string{"CLOSE_ISSUES", "MAX_ISSUES", "MAX_PAGES", "SYNC_DAYS"}, name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Each tracker's recipe against a stand-in for its API and Lapilli's own
// incidents port: issues made for open tasks, found again rather than made
// twice, links put back, tasks moved as their issues were, issues closed
// with CLOSE_ISSUES, and nothing written to Lapilli when the tracker fails
// or answers with a link that isn't its own.
func TestTrackerRecipes(t *testing.T) {
	needTools(t)
	for name, api := range trackerAPIs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := trackerServer(t, nil)
			ctx := context.Background()
			lap := httptest.NewServer(s.IncidentsHandler())
			defer lap.Close()
			f := &fakeTracker{t: t}
			trk := httptest.NewServer(api.serve(f))
			defer trk.Close()
			secrets := map[string]string{"github": "gh-secret", "gitlab": "gl-secret", "jira": "jira-secret", "linear": "lin-secret"}
			env := append(trackerEnv(), api.env(trk.URL, tokenFile(t, secrets[name]))...)
			env = append(env, "LAPILLI_URL="+lap.URL, "LAPILLI_TOKEN_FILE="+tokenFile(t, trTasksTok))
			run := func(more ...string) (string, error) {
				t.Helper()
				cmd := exec.Command("sh", trackerRecipes+api.script)
				cmd.Env = append(slices.Clone(env), more...)
				out, err := cmd.CombinedOutput()
				return string(out), err
			}
			mustRun := func(more ...string) string {
				t.Helper()
				out, err := run(more...)
				if err != nil {
					t.Fatalf("%s: %v\n%s", api.script, err, out)
				}
				return out
			}
			task := func(id int64) ledger.Task {
				t.Helper()
				x, err := s.l.TaskByID(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				return x
			}
			// An issue whose text has another task's marker in it, which a
			// search by words finds for task 1.
			f.add("Another", "Kept in step with Lapilli: lapilli-task-12", []string{"lapilli-task-12"})

			// Lapilli's token wrong: nothing is made in the tracker.
			if out, err := run("LAPILLI_TOKEN_FILE=" + tokenFile(t, "not-the-tasks-token-at-all")); err == nil || f.creates != 0 {
				t.Fatalf("with the wrong token: %v, %d made\n%s", err, f.creates, out)
			}

			// Tasks 1 and 2 are open without an issue: each gets one, and
			// its link. 3 is done; 4 and 5 are another tracker's.
			out := mustRun()
			if f.creates != 2 || task(1).Link != api.link(2) || task(2).Link != api.link(3) || task(3).Link != "" {
				t.Fatalf("made %d; links %q %q %q\n%s", f.creates, task(1).Link, task(2).Link, task(3).Link, out)
			}
			if !strings.Contains(out, "made 2 issues, linked 2 tasks") {
				t.Errorf("said:\n%s", out)
			}
			i := f.issue(2)
			for _, want := range []string{"Too loud at night.", "Rule: Errors", "Team: payments", "Assignee: bo", "In Lapilli: https://lapilli.example.com/tasks/1", "lapilli-task-1"} {
				if !strings.Contains(i.body, want) {
					t.Errorf("the issue's text has no %q:\n%s", want, i.body)
				}
			}
			if i.title != "Tune Errors" {
				t.Errorf("title %q", i.title)
			}
			if api.labels && (!slices.Contains(i.labels, "lapilli") || !slices.Contains(i.labels, "tune") || !slices.Contains(i.labels, "payments")) {
				t.Errorf("labels %q", i.labels)
			}
			if x := task(1); x.By != ledger.TaskTracker || x.Status != "open" {
				t.Errorf("task 1 after linking: %+v", x)
			}

			// Run again: nothing more is made. A task whose issue was made
			// before its link could be written gets that issue's link.
			id6, _ := s.l.PutTask(ctx, ledger.Task{Title: "Made before"}, "ana@example.com")
			f.add("Made before", "Kept in step with Lapilli: lapilli-task-6", []string{"lapilli-task-6"})
			out = mustRun()
			if f.creates != 2 || task(id6).Link != api.link(4) {
				t.Fatalf("again: made %d, task 6 %q\n%s", f.creates, task(id6).Link, out)
			}

			// Issues closed in the tracker close their tasks: done, or
			// dropped as the issue was (GitLab closes issues one way).
			later := time.Now().Add(time.Minute)
			f.set(2, "done", later)
			f.set(3, "dropped", later)
			mustRun()
			dropped := "dropped"
			if !api.dropped {
				dropped = "done"
			}
			if a, b := task(1), task(2); a.Status != "done" || b.Status != dropped || a.By != ledger.TaskTracker {
				t.Fatalf("after closing: %s %s", a.Status, b.Status)
			}
			// Reopened: open again.
			f.set(2, "open", later.Add(time.Minute))
			mustRun()
			if x := task(1); x.Status != "open" {
				t.Fatalf("after reopening: %s", x.Status)
			}
			// A task reopened in Lapilli after its issue closed stays open.
			s.l.PatchTask(ctx, 2, []byte(`{"status":"open"}`), "ana@example.com")
			f.set(3, "dropped", time.Now().Add(-time.Hour))
			mustRun()
			if x := task(2); x.Status != "open" {
				t.Fatalf("a task reopened in Lapilli: %s", x.Status)
			}

			// Done in Lapilli by someone: the issue is closed only with
			// CLOSE_ISSUES, and only while no one has changed it since.
			s.l.PatchTask(ctx, 1, []byte(`{"status":"done"}`), "ana@example.com")
			f.set(2, "open", time.Now().Add(-time.Hour))
			mustRun()
			if f.issue(2).state != "open" || task(1).Status != "done" {
				t.Fatalf("closed without CLOSE_ISSUES: issue %s, task %s", f.issue(2).state, task(1).Status)
			}
			f.set(2, "open", time.Now().Add(time.Hour))
			mustRun("CLOSE_ISSUES=true")
			if f.issue(2).state != "open" || task(1).Status != "done" {
				t.Fatalf("closed an issue changed since: issue %s, task %s", f.issue(2).state, task(1).Status)
			}
			f.set(2, "open", time.Now().Add(-time.Hour))
			out = mustRun("CLOSE_ISSUES=true")
			if f.issue(2).state != "done" || task(1).Status != "done" || !strings.Contains(out, "closed 1 issues") {
				t.Fatalf("with CLOSE_ISSUES: issue %s, task %s\n%s", f.issue(2).state, task(1).Status, out)
			}

			// The tracker refuses to make an issue: the run fails, and
			// nothing is written to Lapilli.
			id7, _ := s.l.PutTask(ctx, ledger.Task{Title: "Seven"}, "ana@example.com")
			cs, _ := s.l.ChangesSince(ctx, time.Unix(0, 0), 1000)
			f.failCreate = true
			if out, err := run(); err == nil {
				t.Fatalf("the tracker refused, and the run passed:\n%s", out)
			}
			// The tracker gives a link that isn't one of its issues: the
			// same.
			f.failCreate, f.badURL = false, true
			out, err := run()
			if err == nil || !strings.Contains(out, "isn't one of its own") {
				t.Fatalf("a link not the tracker's: %v\n%s", err, out)
			}
			if after, _ := s.l.ChangesSince(ctx, time.Unix(0, 0), 1000); len(after) != len(cs) || task(id7).Link != "" {
				t.Fatalf("written to Lapilli though the tracker failed: %d changes, were %d; link %q", len(after), len(cs), task(id7).Link)
			}
			f.badURL = false
			// MAX_ISSUES: so many a run, the rest the next. Task 7's issue
			// was made by the run that got a bad link, and is found again.
			id8, _ := s.l.PutTask(ctx, ledger.Task{Title: "Eight"}, "ana@example.com")
			id9, _ := s.l.PutTask(ctx, ledger.Task{Title: "Nine"}, "ana@example.com")
			made := f.creates
			mustRun("MAX_ISSUES=1")
			if f.creates != made+1 || task(id7).Link == "" || task(id8).Link == "" || task(id9).Link != "" {
				t.Fatalf("MAX_ISSUES=1: made %d; links %q %q %q", f.creates-made, task(id7).Link, task(id8).Link, task(id9).Link)
			}
			mustRun("MAX_ISSUES=1")
			if task(id9).Link == "" {
				t.Fatalf("the next run didn't make the rest")
			}
			// Settings checked before anything is asked.
			if out, err := run("MAX_ISSUES=1;id"); err == nil || !strings.Contains(out, "MAX_ISSUES is a number") {
				t.Errorf("MAX_ISSUES not a number: %v\n%s", err, out)
			}
			if out, err := run("LAPILLI_URL=file:///etc/passwd"); err == nil {
				t.Errorf("LAPILLI_URL not http: %v\n%s", err, out)
			}
		})
	}
}

// The tracker's settings are checked before they reach a request.
func TestTrackerRecipeSettings(t *testing.T) {
	needTools(t)
	lapilli := []string{"LAPILLI_URL=http://127.0.0.1:1", "LAPILLI_TOKEN_FILE=" + tokenFile(t, trTasksTok)}
	secret := tokenFile(t, "a-secret")
	for _, c := range []struct {
		script string
		env    []string
		says   string
	}{
		{"github.sh", []string{"GITHUB_REPO=example/alerts/../x", "GITHUB_TOKEN_FILE=" + secret}, "GITHUB_REPO is OWNER/NAME"},
		{"github.sh", []string{"GITHUB_REPO=example/a?b", "GITHUB_TOKEN_FILE=" + secret}, "GITHUB_REPO is OWNER/NAME"},
		{"gitlab.sh", []string{"GITLAB_PROJECT=example/a b", "GITLAB_TOKEN_FILE=" + secret}, "GITLAB_PROJECT is a project path"},
		{"jira.sh", []string{"JIRA_URL=http://127.0.0.1:1", "JIRA_EMAIL=a@example.com", "JIRA_TOKEN_FILE=" + secret, "JIRA_PROJECT=ops\" OR 1=1"}, "JIRA_PROJECT is a project key"},
		{"linear.sh", []string{"LINEAR_TEAM=ENG", "LINEAR_TOKEN_FILE=" + secret}, "LINEAR_TEAM is a team id"},
	} {
		cmd := exec.Command("sh", trackerRecipes+c.script)
		cmd.Env = append(append(trackerEnv(), lapilli...), c.env...)
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), c.says) {
			t.Errorf("%s %v: %v\n%s", c.script, c.env, err, out)
		}
	}
}

// What Lapilli answers is checked too before it reaches a request: a task's
// id that isn't a number, a page that doesn't move on, more pages than
// MAX_PAGES. The tracker isn't asked anything.
func TestTrackerRecipeDistrustsLapilli(t *testing.T) {
	needTools(t)
	trk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the tracker was asked: %s %s", r.Method, r.URL)
		http.NotFound(w, r)
	}))
	defer trk.Close()
	for _, c := range []struct {
		page func(after string) string
		says string
	}{
		{func(string) string { return `{"tasks":[{"id":"1/../../x","title":"x","status":"open"}]}` }, "a task's id is a number"},
		{func(string) string { return `{"tasks":[],"next":"5;id"}` }, "the next page is a number"},
		{func(string) string { return `{"tasks":[],"next":0}` }, "the same page again"},
		{func(after string) string {
			n, _ := strconv.Atoi(after)
			return fmt.Sprintf(`{"tasks":[],"next":%d}`, n+1)
		}, "more than 3 pages"},
		{func(string) string { return `{"error":"no"}` }, "isn't a page of tasks"},
	} {
		lap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" {
				t.Errorf("Lapilli written: %s %s", r.Method, r.URL)
			}
			io.WriteString(w, c.page(r.URL.Query().Get("after")))
		}))
		cmd := exec.Command("sh", trackerRecipes+"github.sh")
		cmd.Env = append(trackerEnv(), "LAPILLI_URL="+lap.URL, "LAPILLI_TOKEN_FILE="+tokenFile(t, trTasksTok), "MAX_PAGES=3",
			"GITHUB_API="+trk.URL, "GITHUB_REPO=example/alerts", "GITHUB_TOKEN_FILE="+tokenFile(t, "gh-secret"))
		out, err := cmd.CombinedOutput()
		lap.Close()
		if err == nil || !strings.Contains(string(out), c.says) {
			t.Errorf("want a failure saying %q: %v\n%s", c.says, err, out)
		}
	}
}

// The chart runs the recipes from its own copies: they must be the same.
func TestTrackerRecipesInChart(t *testing.T) {
	for _, name := range []string{"lib.sh", "github.sh", "github.jq", "gitlab.sh", "gitlab.jq", "jira.sh", "jira.jq", "linear.sh", "linear.jq"} {
		a, err := os.ReadFile(trackerRecipes + name)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile("../../charts/lapilli/files/trackers/" + name)
		if err != nil || string(a) != string(b) {
			t.Errorf("charts/lapilli/files/trackers/%s differs from recipes/trackers/%s: copy it (%v)", name, name, err)
		}
	}
}
