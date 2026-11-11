package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"html"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// Azure Boards and Shortcut, stood in for at their documented shapes
// (docs/design/inputs.md): Azure DevOps Services REST 7.1 and the work item
// comments API 7.1-preview.4; Shortcut's REST v3. Neither is checked
// against a real account.

// adoUser is an Azure DevOps identity's id, as connectionData and a work
// item's System.CreatedBy give it.
func adoUser(author string) string {
	if author == trBot {
		return "6f1d4b2a-9c7e-4e1f-8a3b-0d2c5e7f9a10"
	}
	return fmt.Sprintf("00000000-1111-4222-8333-%012x", crc32.ChecksumIEEE([]byte(author)))
}

// scMember is a Shortcut member's id.
func scMember(author string) string {
	if author == trBot {
		return "5f0c9a3e-7b21-4d6e-9f8a-2c4b6d8e0f12"
	}
	return fmt.Sprintf("00000000-2222-4333-8444-%012x", crc32.ChecksumIEEE([]byte(author)))
}

func init() {
	trackerAPIs["azure-boards"] = trackerAPI{
		script: "azure-boards.sh",
		env: func(api, secret string) []string {
			return []string{"AZURE_DEVOPS_URL=" + api, "AZURE_DEVOPS_WEB=https://dev.example.com", "AZURE_DEVOPS_ORG=example",
				"AZURE_DEVOPS_PROJECT=alerts", "AZURE_DEVOPS_TOKEN_FILE=" + secret}
		},
		link:        func(n int) string { return fmt.Sprintf("https://dev.example.com/example/alerts/_workitems/edit/%d", n) },
		labels:      true,
		dropped:     true,
		markerLabel: true,
		serve:       adoServe,
	}
	trackerAPIs["shortcut"] = trackerAPI{
		script: "shortcut.sh",
		env: func(api, secret string) []string {
			return []string{"SHORTCUT_API=" + api + "/api/v3", "SHORTCUT_WEB=https://app.example.com", "SHORTCUT_STATE=500000011",
				"SHORTCUT_TOKEN_FILE=" + secret}
		},
		link:    func(n int) string { return fmt.Sprintf("https://app.example.com/example/story/%d", n) },
		labels:  true,
		dropped: true,
		serve:   scServe,
	}
}

func adoServe(f *fakeTracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "" || pass != "ado-secret" {
			w.WriteHeader(http.StatusUnauthorized) // Azure DevOps answers a page, not JSON
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		path := r.URL.EscapedPath()
		q := r.URL.Query()
		const items = "/example/alerts/_apis/wit/workitems"
		state := map[string]string{"open": "Active", "done": "Done", "dropped": "Removed"}
		item := func(i *fakeIssue) map[string]any {
			return map[string]any{"id": i.n, "rev": 3, "url": fmt.Sprintf("https://dev.example.com/example/_apis/wit/workItems/%d", i.n),
				"fields": map[string]any{"System.Title": i.title, "System.State": state[i.state], "System.Description": i.body,
					"System.Tags":        strings.Join(i.labels, "; "),
					"System.CreatedBy":   map[string]any{"id": adoUser(i.author), "displayName": i.author, "uniqueName": i.author + "@example.com"},
					"System.ChangedDate": i.updated.UTC().Format("2006-01-02T15:04:05.00Z")}}
		}
		if cn := commentPath(`^/example/alerts/_apis/wit/workItems/([0-9]+)/comments$`, path); cn > 0 && f.issue(cn) != nil {
			i := f.issue(cn)
			if q.Get("api-version") != "7.1-preview.4" {
				f.t.Errorf("azure-boards: comments %v", q)
			}
			if r.Method == "POST" {
				var in struct{ Text string }
				readBody(r, &in)
				if q.Get("format") != "markdown" || in.Text == "" {
					http.Error(w, `{"message":"bad comment"}`, http.StatusBadRequest)
					return
				}
				f.posted++
				f.comment(i, trBot, in.Text)
				json.NewEncoder(w).Encode(map[string]any{"commentId": 1})
				return
			}
			// Comments are HTML, as Azure Boards keeps them.
			cs := trComments(i, func(c *fakeComment) map[string]any {
				text := c.body
				if c.author != trBot {
					text = "<div>" + strings.ReplaceAll(html.EscapeString(c.body), "\n", "<br>") + "</div>"
				}
				return map[string]any{"workItemId": i.n, "commentId": c.id, "version": 1, "text": text, "isDeleted": c.system,
					"createdBy": map[string]any{"id": adoUser(c.author), "displayName": c.author}, "createdDate": c.at.UTC().Format(time.RFC3339)}
			}, true)
			json.NewEncoder(w).Encode(map[string]any{"totalCount": len(cs), "count": len(cs), "comments": cs})
			return
		}
		n, _ := strconv.Atoi(strings.TrimPrefix(path, items+"/"))
		switch {
		case r.Method == "GET" && path == "/example/_apis/connectionData":
			json.NewEncoder(w).Encode(map[string]any{"authenticatedUser": map[string]any{"id": adoUser(trBot), "providerDisplayName": "Lapilli Bot"}})
		case r.Method == "GET" && path == "/example/_apis/projects/alerts" && f.noProject:
			http.Error(w, `{"message":"TF200016: The following project does not exist: alerts."}`, http.StatusNotFound)
		case r.Method == "GET" && path == "/example/_apis/projects/alerts":
			json.NewEncoder(w).Encode(map[string]any{"id": "p-1", "name": "alerts"})
		case r.Method == "POST" && path == "/example/alerts/_apis/wit/wiql" && f.noSearch:
			http.Error(w, `{"message":"VS402337: service unavailable"}`, http.StatusServiceUnavailable)
		case r.Method == "POST" && path == "/example/alerts/_apis/wit/wiql":
			// The tag and who made them are asked for, and checked again:
			// every work item with the marker in its tags is given.
			var in struct{ Query string }
			readBody(r, &in)
			m := regexp.MustCompile(`^SELECT \[System\.Id\] FROM workitems WHERE \[System\.TeamProject\] = @project AND \[System\.Tags\] CONTAINS '(lapilli-task-[0-9]+)' AND \[System\.CreatedBy\] = @Me$`).FindStringSubmatch(in.Query)
			if m == nil || q.Get("api-version") != "7.1" {
				f.t.Errorf("azure-boards: wiql %q", in.Query)
				http.Error(w, `{"message":"TF51005: The query references a field that does not exist."}`, http.StatusBadRequest)
				return
			}
			found := []map[string]any{}
			for _, i := range f.issues {
				if slices.ContainsFunc(i.labels, func(l string) bool { return strings.HasPrefix(l, m[1]) }) {
					found = append(found, map[string]any{"id": i.n, "url": "x"})
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"queryType": "flat", "workItems": found})
		case r.Method == "GET" && path == items && q.Get("ids") != "":
			var out []map[string]any
			for _, s := range strings.Split(q.Get("ids"), ",") {
				k, _ := strconv.Atoi(s)
				if i := f.issue(k); i != nil {
					out = append(out, item(i))
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"count": len(out), "value": out})
		case r.Method == "POST" && path == items+"/%24Task":
			if r.Header.Get("Content-Type") != "application/json-patch+json" {
				http.Error(w, `{"message":"unsupported media type"}`, http.StatusUnsupportedMediaType)
				return
			}
			var ops []struct {
				Op, Path string
				Value    string
			}
			readBody(r, &ops)
			fields := map[string]string{}
			for _, op := range ops {
				fields[strings.TrimPrefix(op.Path, "/fields/")] = op.Value
			}
			title := fields["System.Title"]
			if title == "" || f.refused(title) || strings.ContainsAny(title, "\r\n") {
				http.Error(w, `{"message":"TF401320: Rule Error for field Title."}`, http.StatusBadRequest)
				return
			}
			var tags []string
			for _, t := range strings.Split(fields["System.Tags"], ";") {
				tags = append(tags, strings.TrimSpace(t))
			}
			f.creates++
			i := f.made(f.add(trBot, title, fields["System.Description"], tags))
			out := item(i)
			if f.badURL {
				out["id"] = "not-a-number"
			}
			json.NewEncoder(w).Encode(out)
		case f.gone(n):
			http.Error(w, fmt.Sprintf(`{"message":"TF401232: Work item %d does not exist, or you do not have permissions to read it."}`, n), http.StatusNotFound)
		case f.hidden(n):
			http.Error(w, `{"message":"TF401289: access denied"}`, http.StatusForbidden)
		case r.Method == "GET" && f.issue(n) != nil:
			f.read(n)
			json.NewEncoder(w).Encode(item(f.issue(n)))
		case r.Method == "PATCH" && f.noClose:
			http.Error(w, `{"message":"TF401289: access denied"}`, http.StatusForbidden)
		case r.Method == "PATCH" && f.issue(n) != nil:
			var ops []struct{ Op, Path, Value string }
			readBody(r, &ops)
			i := f.issue(n)
			if len(ops) != 1 || ops[0].Path != "/fields/System.State" {
				f.t.Errorf("azure-boards: PATCH %+v", ops)
			}
			switch ops[0].Value {
			case "Done":
				i.state = "done"
			case "Removed":
				i.state = "dropped"
			default:
				f.t.Errorf("azure-boards: to %q", ops[0].Value)
			}
			i.updated = time.Now()
			json.NewEncoder(w).Encode(item(i))
		default:
			f.t.Errorf("azure-boards: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}
}

func scServe(f *fakeTracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Shortcut-Token") != "sc-secret" {
			http.Error(w, `{"message":"Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/api/v3")
		const workflow, todo, done, later = 500000010, 500000011, 500000013, 500000014
		story := func(i *fakeIssue) map[string]any {
			labels := []map[string]any{}
			for _, l := range i.labels {
				labels = append(labels, map[string]any{"id": 1, "name": l})
			}
			st := todo
			if i.state == "done" {
				st = done
			}
			cs := trComments(i, func(c *fakeComment) map[string]any {
				return map[string]any{"id": c.id, "text": c.body, "author_id": scMember(c.author), "created_at": c.at.UTC().Format(time.RFC3339),
					"deleted": c.system}
			}, true)
			return map[string]any{"id": i.n, "app_url": fmt.Sprintf("https://app.example.com/example/story/%d/an-issue", i.n),
				"name": i.title, "description": i.body, "labels": labels, "requested_by_id": scMember(i.author),
				"archived": i.state == "dropped", "workflow_id": workflow, "workflow_state_id": st,
				"updated_at": i.updated.UTC().Format("2006-01-02T15:04:05Z"), "comments": cs}
		}
		n := commentPath(`^/stories/([0-9]+)$`, path)
		switch cn := commentPath(`^/stories/([0-9]+)/comments$`, path); {
		case cn > 0 && r.Method == "POST" && f.issue(cn) != nil:
			var in struct{ Text string }
			readBody(r, &in)
			f.posted++
			f.comment(f.issue(cn), trBot, in.Text)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == "GET" && path == "/member":
			json.NewEncoder(w).Encode(map[string]any{"id": scMember(trBot), "mention_name": trBot, "name": "Lapilli Bot",
				"workspace2": map[string]any{"url_slug": "example"}})
		case r.Method == "GET" && path == "/members":
			var out []map[string]any
			for _, who := range []string{trBot, "someone", "mallory"} {
				out = append(out, map[string]any{"id": scMember(who), "profile": map[string]any{"mention_name": who}})
			}
			json.NewEncoder(w).Encode(out)
		case r.Method == "GET" && path == "/workflows" && f.noProject:
			http.Error(w, `{"message":"Forbidden"}`, http.StatusForbidden)
		case r.Method == "GET" && path == "/workflows":
			json.NewEncoder(w).Encode([]map[string]any{{"id": workflow, "name": "Engineering", "states": []map[string]any{
				{"id": 500000012, "type": "unstarted"}, {"id": todo, "type": "unstarted"},
				{"id": done, "type": "done"}, {"id": later, "type": "done"}}}})
		case r.Method == "GET" && path == "/search/stories" && f.noSearch:
			http.Error(w, `{"message":"Too many requests"}`, http.StatusTooManyRequests)
		case r.Method == "GET" && path == "/search/stories":
			// The label lapilli and who asked for them are in the query,
			// and checked again: all those with the marker are given.
			m := regexp.MustCompile(`^label:lapilli requester:lapilli-bot description:"(lapilli-task-[0-9]+)"$`).FindStringSubmatch(r.URL.Query().Get("query"))
			if m == nil || r.URL.Query().Get("detail") != "full" {
				f.t.Errorf("shortcut: search %q", r.URL.Query().Get("query"))
				http.Error(w, `{"message":"bad query"}`, http.StatusBadRequest)
				return
			}
			data := []map[string]any{}
			for _, i := range f.search(m[1]) {
				data = append(data, story(i))
			}
			json.NewEncoder(w).Encode(map[string]any{"total": len(data), "data": data, "next": nil})
		case r.Method == "POST" && path == "/stories":
			var in struct {
				Name, Description string
				StoryType         string `json:"story_type"`
				WorkflowStateID   int    `json:"workflow_state_id"`
				Labels            []struct{ Name string }
			}
			if err := readBody(r, &in); err != nil || in.Name == "" || in.WorkflowStateID != todo || f.refused(in.Name) || strings.ContainsAny(in.Name, "\r\n") {
				http.Error(w, `{"message":"invalid story"}`, http.StatusBadRequest)
				return
			}
			var labels []string
			for _, l := range in.Labels {
				labels = append(labels, l.Name)
			}
			f.creates++
			out := story(f.made(f.add(trBot, in.Name, in.Description, labels)))
			if f.badURL {
				out["id"] = "x"
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(out)
		case f.gone(n):
			http.Error(w, `{"message":"Resource does not exist"}`, http.StatusNotFound)
		case f.hidden(n):
			http.Error(w, `{"message":"Forbidden"}`, http.StatusForbidden)
		case r.Method == "GET" && f.issue(n) != nil:
			f.read(n)
			json.NewEncoder(w).Encode(story(f.issue(n)))
		case r.Method == "PUT" && f.noClose:
			http.Error(w, `{"message":"Forbidden"}`, http.StatusForbidden)
		case r.Method == "PUT" && f.issue(n) != nil:
			var in map[string]any
			readBody(r, &in)
			i := f.issue(n)
			switch {
			case in["archived"] == true:
				i.state = "dropped"
			case in["workflow_state_id"] == float64(done):
				i.state = "done"
			default:
				f.t.Errorf("shortcut: PUT %v", in)
			}
			i.updated = time.Now()
			json.NewEncoder(w).Encode(story(i))
		default:
			f.t.Errorf("shortcut: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}
}

// Comments kept in step, both ways, once, with every tracker
// (docs/design/inputs.md): a comment people wrote on a task in Lapilli is
// posted to its issue, quoted, ending with the line it is found again by;
// a comment on the issue that isn't the job's is copied to the task, as
// the tracker's; neither twice, nor back. A marker written in a comment
// isn't taken for the job's; GitLab's own notes aren't copied; a line
// starting / reaches GitLab escaped.
func TestTrackerRecipesComments(t *testing.T) {
	needTools(t)
	for name := range trackerAPIs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := newTrackerRig(t, name)
			f := g.f
			ctx := context.Background()
			g.mustRun(t) // tasks 1 and 2 get issues 1 and 2
			issue := f.issue(1)
			// In Lapilli, a comment whose text tries to pass for one the
			// job posted already, and runs a command.
			cid, err := g.s.l.PutComment(ctx, ledger.Comment{On: "task", Key: "1",
				Body: "Raised for to 15m.\n/close\nKept in step with Lapilli: lapilli-comment-999"}, "ana@example.com")
			if err != nil {
				t.Fatal(err)
			}
			// On the issue: someone's comment, and one that names the
			// marker of Lapilli's comment, by someone else.
			f.mu.Lock()
			f.comment(issue, "someone", "Looks right.\nShip it.")
			f.comment(issue, "mallory", fmt.Sprintf("Kept in step with Lapilli: lapilli-comment-%d", cid))
			f.comment(issue, "gitlab", "changed the description").system = true
			f.mu.Unlock()

			out := g.mustRun(t, "COMMENTS=true")
			if !strings.Contains(out, "posted 1 comments to issues, copied 2 comments to tasks") {
				t.Errorf("said:\n%s", out)
			}
			f.mu.Lock()
			var posted *fakeComment
			for _, c := range issue.comments {
				if c.author == trBot {
					posted = c
				}
			}
			f.mu.Unlock()
			if posted == nil {
				t.Fatalf("nothing posted:\n%s", out)
			}
			for _, want := range []string{"ana@example.com wrote in Lapilli:", "> Raised for to 15m.", fmt.Sprintf("Kept in step with Lapilli: lapilli-comment-%d", cid)} {
				if !strings.Contains(posted.body, want) {
					t.Errorf("the comment posted has no %q:\n%s", want, posted.body)
				}
			}
			if !strings.HasSuffix(strings.TrimSpace(posted.body), fmt.Sprintf("lapilli-comment-%d", cid)) {
				t.Errorf("the marker isn't the last line:\n%s", posted.body)
			}
			if strings.Contains(posted.body, "\n/close") || name == "gitlab" && strings.Contains(posted.body, "\n\\/") {
				t.Errorf("a command reached the tracker as a line of its own:\n%q", posted.body)
			}
			cs, err := g.s.l.Comments(ctx, "task", "1")
			if err != nil {
				t.Fatal(err)
			}
			var copied []ledger.Comment
			for _, c := range cs {
				if c.Origin == ledger.OriginTracker {
					copied = append(copied, c)
				}
			}
			if len(copied) != 2 || copied[0].Author != "someone" || copied[0].Body != "Looks right.\nShip it." || copied[0].By != ledger.TaskTracker ||
				copied[1].Author != "mallory" || copied[0].Ref == "" {
				t.Fatalf("copied: %+v", copied)
			}
			// Again: nothing new either way.
			made := f.posted
			out = g.mustRun(t, "COMMENTS=true")
			if f.posted != made || !strings.Contains(out, "posted 0 comments to issues, copied 0 comments to tasks") {
				t.Errorf("again: posted %d\n%s", f.posted-made, out)
			}
			// The task's page shows a copied comment as the tracker's, and
			// no one can change it.
			_, body, _ := get(t, g.s.Handler(), "/tasks/1")
			if !strings.Contains(body, "in the tracker") {
				t.Errorf("the task's page doesn't say a comment is the tracker's")
			}
			if _, err := g.s.l.PutComment(ctx, ledger.Comment{ID: copied[0].ID, Body: "changed"}, "ana@example.com"); err == nil {
				t.Error("a person changed a comment copied in from the tracker")
			}
			// Without COMMENTS, nothing is posted or copied.
			g.s.l.PutComment(ctx, ledger.Comment{On: "task", Key: "1", Body: "Another."}, "ana@example.com")
			made = f.posted
			g.mustRun(t)
			if f.posted != made {
				t.Errorf("posted without COMMENTS")
			}
		})
	}
}

// What the tasks token may do with comments, as an attacker holding it, or
// another, would try: read a task's comments, and add one from the tracker,
// once by its id; nothing without the token or with another's; no field
// but text, author, ref and at; no text that hides, no author that
// doesn't show, no ref of another shape, no time ahead of now, nothing
// past the bounds; nothing told to the notice webhook; a person's comment
// can't be written through it.
func TestTrackerComments(t *testing.T) {
	s := trackerServer(t, func(c *Config) { c.NotifyWebhook = tokenFile(t, "https://hooks.example.com/lapilli") })
	h := s.IncidentsHandler()
	ctx := context.Background()
	path := "/api/v1/tasks/1/comments"
	good := `{"text":"Looks right.","author":"someone","ref":"1001","at":"2026-11-02T00:00:00Z"}`
	for name, tok := range map[string]string{"no token": "", "the incidents token": trIncTok, "the firings token": trFirTok} {
		if code, _ := tracker(t, h, "POST", path, tok, good); code != http.StatusUnauthorized && code != http.StatusForbidden {
			t.Errorf("%s: %d", name, code)
		}
		if code, _ := tracker(t, h, "GET", path, tok, ""); code != http.StatusUnauthorized && code != http.StatusForbidden {
			t.Errorf("GET, %s: %d", name, code)
		}
	}
	// A person's comment is told; the tracker's isn't.
	if _, err := s.l.PutComment(ctx, ledger.Comment{On: "task", Key: "1", Body: "Mine."}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	pending, _ := s.l.Unsent(ctx)
	if pending == 0 {
		t.Fatal("a person's comment wasn't told: the check below would say nothing")
	}
	if code, body := tracker(t, h, "POST", path, trTasksTok, good); code != http.StatusCreated || !strings.Contains(body, `"kept": true`) {
		t.Fatalf("a comment: %d %s", code, body)
	}
	if code, body := tracker(t, h, "POST", path, trTasksTok, good); code != http.StatusOK || !strings.Contains(body, `"kept": false`) {
		t.Errorf("the same again: %d %s", code, body)
	}
	if now, _ := s.l.Unsent(ctx); now != pending {
		t.Errorf("told: %d pending, were %d", now, pending)
	}
	for name, c := range map[string]struct {
		body string
		code int
	}{
		"another field":          {`{"text":"x","author":"a","ref":"1","at":"2026-11-02T00:00:00Z","by":"ana@example.com"}`, 400},
		"no text":                {`{"text":" ","author":"a","ref":"2","at":"2026-11-02T00:00:00Z"}`, 400},
		"text that hides":        {`{"text":"pay\u202eyap","author":"a","ref":"3","at":"2026-11-02T00:00:00Z"}`, 400},
		"an author on two lines": {`{"text":"x","author":"a\nb","ref":"4","at":"2026-11-02T00:00:00Z"}`, 400},
		"no author":              {`{"text":"x","author":"","ref":"5","at":"2026-11-02T00:00:00Z"}`, 400},
		"a ref with a slash":     {`{"text":"x","author":"a","ref":"../1","at":"2026-11-02T00:00:00Z"}`, 400},
		"a ref too long":         {`{"text":"x","author":"a","ref":"` + strings.Repeat("1", 101) + `","at":"2026-11-02T00:00:00Z"}`, 400},
		"a time ahead":           {`{"text":"x","author":"a","ref":"6","at":"2099-01-01T00:00:00Z"}`, 400},
		"text too long":          {`{"text":"` + strings.Repeat("x", 10001) + `","author":"a","ref":"7","at":"2026-11-02T00:00:00Z"}`, 400},
		"a body too large":       {`{"text":"` + strings.Repeat("x", 70000) + `","author":"a","ref":"8","at":"2026-11-02T00:00:00Z"}`, 413},
		"not JSON":               {`text=x`, 400},
	} {
		if code, body := tracker(t, h, "POST", path, trTasksTok, c.body); code != c.code {
			t.Errorf("%s: %d %s, want %d", name, code, body, c.code)
		}
	}
	if code, _ := tracker(t, h, "POST", "/api/v1/tasks/999/comments", trTasksTok, good); code != http.StatusNotFound {
		t.Errorf("a task that isn't there: %d", code)
	}
	code, body := tracker(t, h, "GET", path, trTasksTok, "")
	var got struct {
		Comments []ledger.Comment `json:"comments"`
	}
	if err := json.Unmarshal([]byte(body), &got); code != http.StatusOK || err != nil || len(got.Comments) != 2 ||
		got.Comments[1].Origin != "" || got.Comments[0].Origin != ledger.OriginTracker || got.Comments[0].Author != "someone" || got.Comments[0].By != ledger.TaskTracker {
		t.Errorf("the task's comments: %d %s", code, body)
	}
	// Bounded: so many copied comments a task.
	for i := range ledger.MaxCopied - 1 {
		if _, err := s.l.CopyComment(ctx, 2, "x", "a", fmt.Sprint("r", i), time.Now().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.l.CopyComment(ctx, 2, "x", "a", "last", time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("the last one taken: %v", err)
	}
	if _, err := s.l.CopyComment(ctx, 2, "x", "a", "one-more", time.Now().Add(-time.Hour)); err == nil {
		t.Error("more than MaxCopied taken")
	}
	if kept, err := s.l.CopyComment(ctx, 2, "x", "a", "last", time.Now().Add(-time.Hour)); err != nil || kept {
		t.Errorf("one kept already, at the bound: %v %v", kept, err)
	}
}
