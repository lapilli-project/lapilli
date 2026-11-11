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
	"unicode/utf8"

	"lapilli.dev/internal/ledger"
)

const trackerRecipes = "../../recipes/trackers/"

// fakeIssue is an issue in a stand-in tracker; state is open, done or
// dropped, as the recipes tell them apart. author is who wrote it (trBot,
// the recipes' token); team is Linear's, ours when empty.
type fakeIssue struct {
	n            int
	author, team string
	title, body  string
	labels       []string
	state        string
	updated      time.Time
	// gone: deleted, so the tracker no longer gives it.
	gone bool
}

// trBot is who the stand-in trackers say the recipes' token is; trTeam is
// the Linear team the recipe is for.
const (
	trBot  = "lapilli-bot"
	trTeam = "9cfb482a-81e3-4154-b5b9-2c805e70a02d"
)

// trUserID is an author as a tracker gives users ids: GitLab by number,
// Jira by account, Linear by UUID.
func trUserID(tracker, author string) any {
	n := 8
	if author == trBot {
		n = 7
	}
	switch tracker {
	case "gitlab":
		return n
	case "jira":
		return fmt.Sprintf("557058:%08d-0000-4000-8000-000000000000", n)
	case "linear":
		return fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
	}
	return author
}

// fakeTracker keeps issues for a tracker's stand-in API.
type fakeTracker struct {
	t       *testing.T
	mu      sync.Mutex
	issues  []*fakeIssue
	creates int
	// failCreate refuses to make an issue, and refuse one whose title
	// holds it; badURL makes one and gives a link that isn't the tracker's.
	failCreate, badURL bool
	refuse             string
}

// refused is whether the tracker refuses to make an issue with this title.
func (f *fakeTracker) refused(title string) bool {
	return f.failCreate || f.refuse != "" && strings.Contains(title, f.refuse)
}

// gone is whether issue n was deleted.
func (f *fakeTracker) gone(n int) bool {
	return f.issue(n) != nil && f.issue(n).gone
}

func (f *fakeTracker) add(author, title, body string, labels []string) *fakeIssue {
	i := &fakeIssue{n: len(f.issues) + 1, author: author, title: title, body: body, labels: labels, state: "open", updated: time.Now()}
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
					labels := []map[string]any{}
					for _, l := range i.labels {
						labels = append(labels, map[string]any{"name": l})
					}
					// GitHub's logins are the same in any case.
					login := i.author
					if login == trBot {
						login = "Lapilli-Bot"
					}
					out := map[string]any{"number": i.n, "html_url": fmt.Sprintf("https://github.example.com/example/alerts/issues/%d", i.n),
						"user": map[string]any{"login": login}, "labels": labels,
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
				case r.Method == "GET" && r.URL.Path == "/user":
					json.NewEncoder(w).Encode(map[string]any{"login": "lapilli-bot", "id": 7})
				case r.Method == "GET" && r.URL.Path == "/search/issues":
					// Found by words, as GitHub's search finds them; who wrote
					// them and their labels are asked for, and checked again.
					q := r.URL.Query().Get("q")
					m := regexp.MustCompile(`"([^"]+)"`).FindStringSubmatch(q)
					if !strings.Contains(q, "repo:example/alerts") || !strings.Contains(q, "in:body") || m == nil ||
						!strings.Contains(q, "author:lapilli-bot") || !strings.Contains(q, "label:lapilli") {
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
					// GitHub's labels are 50 characters at most.
					if err := readBody(r, &in); err != nil || in.Title == "" || f.refused(in.Title) ||
						slices.ContainsFunc(in.Labels, func(l string) bool { return utf8.RuneCountInString(l) > 50 }) {
						http.Error(w, `{"message":"Validation Failed"}`, http.StatusUnprocessableEntity)
						return
					}
					f.creates++
					out := issue(f.add(trBot, in.Title, in.Body, in.Labels))
					if f.badURL {
						out["html_url"] = "https://evil.example/example/alerts/issues/1"
					}
					w.WriteHeader(http.StatusCreated)
					json.NewEncoder(w).Encode(out)
				case f.gone(n):
					http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
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
						"author": map[string]any{"id": trUserID("gitlab", i.author), "username": i.author},
						"title":  i.title, "description": i.body, "labels": i.labels, "state": state,
						"updated_at": i.updated.UTC().Format("2006-01-02T15:04:05.000Z")}
				}
				n, _ := strconv.Atoi(strings.TrimPrefix(path, base+"/"))
				switch {
				case r.Method == "GET" && path == "/api/v4/user":
					json.NewEncoder(w).Encode(map[string]any{"id": trUserID("gitlab", trBot), "username": trBot})
				case r.Method == "GET" && path == base:
					// Who wrote them, and the label lapilli, are asked for
					// and checked again: all those with the marker are given.
					q := r.URL.Query()
					if q.Get("in") != "description" || q.Get("search") == "" || q.Get("author_id") != "7" || q.Get("labels") != "lapilli" {
						f.t.Errorf("gitlab: search %v", q)
					}
					out := []map[string]any{}
					for _, i := range f.search(q.Get("search")) {
						out = append(out, issue(i))
					}
					json.NewEncoder(w).Encode(out)
				case r.Method == "POST" && path == base:
					var in struct{ Title, Description, Labels string }
					if err := readBody(r, &in); err != nil || in.Title == "" || f.refused(in.Title) {
						http.Error(w, `{"message":{"title":["can't be blank"]}}`, http.StatusBadRequest)
						return
					}
					f.creates++
					out := issue(f.add(trBot, in.Title, in.Description, strings.Split(in.Labels, ",")))
					if f.badURL {
						out["web_url"] = "https://evil.example/example/alerts/-/issues/1"
					}
					w.WriteHeader(http.StatusCreated)
					json.NewEncoder(w).Encode(out)
				case f.gone(n):
					http.Error(w, `{"message":"404 Issue Not Found"}`, http.StatusNotFound)
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
				fields := func(i *fakeIssue) map[string]any {
					return map[string]any{"creator": map[string]any{"accountId": trUserID("jira", i.author)}, "labels": i.labels}
				}
				switch {
				case r.Method == "GET" && r.URL.Path == "/rest/api/3/myself":
					json.NewEncoder(w).Encode(map[string]any{"accountId": trUserID("jira", trBot)})
				case r.Method == "GET" && r.URL.Path == "/rest/api/3/search/jql":
					// Who made them, and the label lapilli, are asked for and
					// checked again: all those with the marker are given.
					m := regexp.MustCompile(`^project = OPS AND labels = "([^"]+)" AND labels = "lapilli" AND creator = currentUser\(\)$`).FindStringSubmatch(r.URL.Query().Get("jql"))
					if m == nil || r.URL.Query().Get("fields") != "creator,labels" {
						f.t.Errorf("jira: jql %q", r.URL.Query().Get("jql"))
						http.Error(w, `{"errorMessages":["bad jql"]}`, http.StatusBadRequest)
						return
					}
					out := []map[string]any{}
					for _, i := range f.issues {
						if slices.Contains(i.labels, m[1]) {
							out = append(out, map[string]any{"id": strconv.Itoa(10000 + i.n), "key": fmt.Sprintf("OPS-%d", i.n), "fields": fields(i)})
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
						in.Fields.Summary == "" || in.Fields.Description.Type != "doc" || f.refused(in.Fields.Summary) {
						http.Error(w, `{"errorMessages":[],"errors":{"summary":"You must specify a summary of the issue."}}`, http.StatusBadRequest)
						return
					}
					// A summary is one line.
					if strings.ContainsAny(in.Fields.Summary, "\r\n") {
						http.Error(w, `{"errorMessages":[],"errors":{"summary":"The summary is invalid because it contains newline characters."}}`, http.StatusBadRequest)
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
					i := f.add(trBot, in.Fields.Summary, strings.Join(lines, "\n"), in.Fields.Labels)
					key := fmt.Sprintf("OPS-%d", i.n)
					if f.badURL {
						key = "EVIL-1"
					}
					w.WriteHeader(http.StatusCreated)
					json.NewEncoder(w).Encode(map[string]any{"id": strconv.Itoa(10000 + i.n), "key": key, "self": "https://jira.example.com/rest/api/3/issue/" + key})
				case f.gone(n):
					http.Error(w, `{"errorMessages":["Issue does not exist or you do not have permission to see it."],"errors":{}}`, http.StatusNotFound)
				case r.Method == "GET" && n > 0 && f.issue(n) != nil && !strings.HasSuffix(r.URL.Path, "/transitions"):
					i := f.issue(n)
					cat, resolution := "indeterminate", any(nil)
					switch i.state {
					case "done":
						cat, resolution = "done", map[string]any{"name": "Done"}
					case "dropped":
						cat, resolution = "done", map[string]any{"name": "Won't Do"}
					}
					out := fields(i)
					out["status"] = map[string]any{"name": "Whatever", "statusCategory": map[string]any{"key": cat}}
					out["resolution"], out["updated"] = resolution, i.updated.In(seoul).Format("2006-01-02T15:04:05.000-0700")
					json.NewEncoder(w).Encode(map[string]any{"key": fmt.Sprintf("OPS-%d", i.n), "fields": out})
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
			return []string{"LINEAR_API=" + api + "/graphql", "LINEAR_WEB=https://linear.example.com", "LINEAR_TEAM=" + trTeam, "LINEAR_TOKEN_FILE=" + secret}
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
				const team = trTeam
				node := func(i *fakeIssue) map[string]any {
					t := team
					if i.team != "" {
						t = i.team
					}
					return map[string]any{"url": url(i), "description": i.body, "team": map[string]any{"id": t},
						"creator": map[string]any{"id": trUserID("linear", i.author)}}
				}
				n := 0
				if id, ok := in.Variables["id"].(string); ok {
					n, _ = strconv.Atoi(strings.TrimPrefix(id, "ENG-"))
				}
				switch q := in.Query; {
				case strings.Contains(q, "viewer { id }"):
					data(map[string]any{"viewer": map[string]any{"id": trUserID("linear", trBot)}})
				case strings.Contains(q, "issues(first: 20"):
					// Who made them, and their team, are asked for and
					// checked again: all those with the marker are given.
					if in.Variables["team"] != team || in.Variables["me"] != trUserID("linear", trBot) || !strings.Contains(q, "creator: {id: {eq: $me}}") {
						f.t.Errorf("linear: search %s %v", q, in.Variables)
					}
					nodes := []map[string]any{}
					m, _ := in.Variables["m"].(string)
					for _, i := range f.search(m) {
						nodes = append(nodes, node(i))
					}
					data(map[string]any{"issues": map[string]any{"nodes": nodes}})
				case strings.Contains(q, "issueCreate"):
					v, _ := in.Variables["in"].(map[string]any)
					title, _ := v["title"].(string)
					desc, _ := v["description"].(string)
					if v["teamId"] != team || title == "" || f.refused(title) {
						json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]any{{"message": "Argument Validation Error"}}})
						return
					}
					f.creates++
					i := f.add(trBot, title, desc, nil)
					u := url(i)
					if f.badURL {
						u = "https://evil.example/example/issue/ENG-1/x"
					}
					data(map[string]any{"issueCreate": map[string]any{"success": true, "issue": map[string]any{"url": u}}})
				case f.gone(n):
					json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]any{{"message": "Entity not found: Issue"}}})
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
					out := node(i)
					out["updatedAt"], out["state"] = i.updated.UTC().Format("2006-01-02T15:04:05.000Z"), map[string]any{"type": typ}
					data(map[string]any{"issue": out})
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

// trackerRig is a tracker's recipe set up against its stand-in and
// Lapilli's own incidents port, with trackerServer's tasks.
type trackerRig struct {
	s   *Server
	f   *fakeTracker
	api trackerAPI
	env []string
}

func newTrackerRig(t *testing.T, name string) *trackerRig {
	t.Helper()
	api := trackerAPIs[name]
	s := trackerServer(t, nil)
	lap := httptest.NewServer(s.IncidentsHandler())
	t.Cleanup(lap.Close)
	f := &fakeTracker{t: t}
	trk := httptest.NewServer(api.serve(f))
	t.Cleanup(trk.Close)
	secrets := map[string]string{"github": "gh-secret", "gitlab": "gl-secret", "jira": "jira-secret", "linear": "lin-secret"}
	env := append(trackerEnv(), api.env(trk.URL, tokenFile(t, secrets[name]))...)
	env = append(env, "LAPILLI_URL="+lap.URL, "LAPILLI_TOKEN_FILE="+tokenFile(t, trTasksTok))
	return &trackerRig{s: s, f: f, api: api, env: env}
}

// run runs the recipe once, with more settings.
func (g *trackerRig) run(more ...string) (string, error) {
	cmd := exec.Command("sh", trackerRecipes+g.api.script)
	cmd.Env = append(slices.Clone(g.env), more...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (g *trackerRig) mustRun(t *testing.T, more ...string) string {
	t.Helper()
	out, err := g.run(more...)
	if err != nil {
		t.Fatalf("%s: %v\n%s", g.api.script, err, out)
	}
	return out
}

func (g *trackerRig) task(t *testing.T, id int64) ledger.Task {
	t.Helper()
	x, err := g.s.l.TaskByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func (g *trackerRig) put(t *testing.T, task ledger.Task) int64 {
	t.Helper()
	id, err := g.s.l.PutTask(context.Background(), task, "ana@example.com")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// marked is what the recipes write to find an issue again: the line ending
// its text, and Jira's labels.
func marked(id int64) (string, []string) {
	return fmt.Sprintf("Kept in step with Lapilli: lapilli-task-%d", id), []string{"lapilli", fmt.Sprintf("lapilli-task-%d", id)}
}

// Each tracker's recipe against a stand-in for its API and Lapilli's own
// incidents port: issues made for open tasks, found again rather than made
// twice, links put back, tasks moved as their issues were, issues closed
// with CLOSE_ISSUES, and nothing written to Lapilli when the tracker fails
// or answers with a link that isn't its own.
func TestTrackerRecipes(t *testing.T) {
	needTools(t)
	for name := range trackerAPIs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := newTrackerRig(t, name)
			s, f, api := g.s, g.f, g.api
			ctx := context.Background()
			run := g.run
			mustRun := func(more ...string) string { t.Helper(); return g.mustRun(t, more...) }
			task := func(id int64) ledger.Task { t.Helper(); return g.task(t, id) }
			// An issue the job made for another task, which a search by
			// words finds for task 1.
			text12, labels12 := marked(12)
			f.add(trBot, "Another", text12, labels12)

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
			id6 := g.put(t, ledger.Task{Title: "Made before"})
			text6, labels6 := marked(id6)
			f.add(trBot, "Made before", text6, labels6)
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
			id7 := g.put(t, ledger.Task{Title: "Seven"})
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
			id8 := g.put(t, ledger.Task{Title: "Eight"})
			id9 := g.put(t, ledger.Task{Title: "Nine"})
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

// Anyone may write a task's marker in an issue, and an editor may link a
// task to any issue: only an issue the job's own account made, for that
// task (and labelled lapilli, in its team), is taken for the task's,
// followed, or closed.
func TestTrackerRecipesOnlyTheirOwn(t *testing.T) {
	needTools(t)
	for name := range trackerAPIs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := newTrackerRig(t, name)
			f, api := g.f, g.api
			g.mustRun(t) // tasks 1 and 2 get issues 1 and 2

			// Before task 6 is made, issues holding its marker: one by
			// someone else, labelled as the job labels them; one by the
			// job's account but not labelled lapilli; in Linear, one by it
			// in another team. None is taken for task 6's.
			text6, labels6 := marked(6)
			planted := f.add("mallory", "Totally unrelated", "Hi! "+text6, labels6)
			if api.labels {
				f.add(trBot, "Not labelled", text6, labels6[1:])
			}
			if name == "linear" {
				f.add(trBot, "Another team's", text6, nil).team = "0d1e9f62-5a4b-4c3d-8e2f-1a2b3c4d5e6f"
			}
			id6 := g.put(t, ledger.Task{Title: "Fix the paging rule"})
			if id6 != 6 {
				t.Fatalf("task %d, want 6", id6)
			}
			made := f.creates
			out := g.mustRun(t)
			mine := f.issues[len(f.issues)-1]
			if f.creates != made+1 || mine.author != trBot || g.task(t, id6).Link != api.link(mine.n) {
				t.Fatalf("task 6 linked to %q, %d made\n%s", g.task(t, id6).Link, f.creates-made, out)
			}
			// Its writer closing theirs doesn't close the task.
			f.set(planted.n, "dropped", time.Now().Add(time.Minute))
			g.mustRun(t)
			if x := g.task(t, id6); x.Status != "open" {
				t.Fatalf("task 6 after mallory closed her issue: %s", x.Status)
			}

			// Tasks done in Lapilli, linked by an editor to someone else's
			// issue (7), to the job's issue for task 1 (8), and in Linear
			// to the job's issue in another team holding the task's marker
			// (9); and an open task linked to someone else's issue, closed
			// (10). With CLOSE_ISSUES, none is closed, and the open task
			// isn't moved.
			past := time.Now().Add(-time.Hour)
			victim := f.add("someone", "Security review", "Owned by another team.", []string{"security"})
			victim.updated = past
			g.put(t, ledger.Task{Title: "Seven", Status: "done", Link: api.link(victim.n)})
			f.issue(1 + 1).updated = past // issue 2, task 1's
			id8 := g.put(t, ledger.Task{Title: "Eight", Status: "done", Link: api.link(2)})
			id9 := g.put(t, ledger.Task{Title: "Nine", Status: "done"})
			text9, _ := marked(id9)
			theirs := f.add(trBot, "Another team's", text9, nil)
			theirs.updated = past
			if name == "linear" {
				theirs.team = "0d1e9f62-5a4b-4c3d-8e2f-1a2b3c4d5e6f"
			}
			g.s.l.PatchTask(context.Background(), id9, []byte(fmt.Sprintf(`{"link": %q}`, api.link(theirs.n))), "ana@example.com")
			closedOne := f.add("someone", "Closed", "Done long ago.", nil)
			id10 := g.put(t, ledger.Task{Title: "Ten", Link: api.link(closedOne.n)})
			f.set(closedOne.n, "done", time.Now().Add(time.Minute))
			out = g.mustRun(t, "CLOSE_ISSUES=true")
			if victim.state != "open" || f.issue(2).state != "open" || g.task(t, id8).Status != "done" || g.task(t, id10).Status != "open" {
				t.Fatalf("someone's issue %s, task 1's %s; task 8 %s, task 10 %s\n%s", victim.state, f.issue(2).state,
					g.task(t, id8).Status, g.task(t, id10).Status, out)
			}
			if name == "linear" && theirs.state != "open" {
				t.Fatalf("another team's issue closed\n%s", out)
			}
			for _, want := range []string{"task 7's issue isn't one this job made for it", "task 8's issue isn't one", "task 10's issue isn't one"} {
				if !strings.Contains(out, want) {
					t.Errorf("didn't say %q:\n%s", want, out)
				}
			}
		})
	}
}

// One task the tracker refuses, or whose issue it no longer gives, is said
// and skipped: the rest are made, linked and moved, and the run fails at
// the end. A title on two lines, and a team's name longer than GitHub's
// labels, are made to fit.
func TestTrackerRecipesCarryOn(t *testing.T) {
	needTools(t)
	for name := range trackerAPIs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := newTrackerRig(t, name)
			f, api := g.f, g.api
			g.mustRun(t) // tasks 1 and 2 get issues 1 and 2
			team, err := g.s.l.PutTeam(context.Background(), ledger.Team{Name: strings.Repeat("Reliability ", 5) + "Team", Rules: "Checkout"}, "ana@example.com")
			if err != nil {
				t.Fatal(err)
			}
			f.refuse = "Refused"
			refused := g.put(t, ledger.Task{Title: "Refused by the tracker"})
			fits := g.put(t, ledger.Task{Title: "Line one\nline two", TeamID: team})
			f.issue(1).gone = true
			f.set(2, "done", time.Now().Add(time.Minute))
			out, err := g.run()
			if err == nil {
				t.Fatalf("the run passed:\n%s", out)
			}
			for _, want := range []string{
				fmt.Sprintf("didn't make an issue for task %d; skipped", refused),
				"didn't give task 1's issue; skipped",
				"made 1 issues, linked 1 tasks, moved 1 tasks",
				"2 failed",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("didn't say %q:\n%s", want, out)
				}
			}
			i := f.issues[len(f.issues)-1]
			if g.task(t, refused).Link != "" || g.task(t, fits).Link != api.link(i.n) || g.task(t, 2).Status != "done" {
				t.Fatalf("links %q %q; task 2 %s\n%s", g.task(t, refused).Link, g.task(t, fits).Link, g.task(t, 2).Status, out)
			}
			if name == "jira" && i.title != "Line one line two" {
				t.Errorf("summary %q", i.title)
			}
			if name == "github" && !slices.Contains(i.labels, strings.Repeat("Reliability ", 4)+"Re") {
				t.Errorf("labels %q", i.labels)
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
