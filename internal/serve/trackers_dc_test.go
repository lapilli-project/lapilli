package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// YouTrack and Jira Data Center, stood in for at their documented shapes
// (recipes/trackers/README.md names the pages): YouTrack's REST API, which
// gives only the attributes fields= names, its times in milliseconds; Jira
// Data Center's REST API v2, with a personal access token, users by key,
// and an issue's text and comments as wiki markup. Neither is checked
// against a real account.

// ytUser is a YouTrack user's database id.
func ytUser(author string) string {
	if author == trBot {
		return "1-7"
	}
	return fmt.Sprintf("1-%d", 100+crc32.ChecksumIEEE([]byte(author))%10000)
}

// jdcUser is a Jira Data Center user's key: JIRAUSER and a number, as Jira
// 8.4 and later give new accounts.
func jdcUser(author string) string {
	if author == trBot {
		return "JIRAUSER10007"
	}
	return fmt.Sprintf("JIRAUSER%d", 20000+crc32.ChecksumIEEE([]byte(author))%10000)
}

func init() {
	trackerAPIs["youtrack"] = trackerAPI{
		script: "youtrack.sh",
		env: func(api, secret string) []string {
			return []string{"YOUTRACK_URL=" + api, "YOUTRACK_WEB=https://example.youtrack.cloud", "YOUTRACK_PROJECT=OPS", "YOUTRACK_TOKEN_FILE=" + secret}
		},
		link:    func(n int) string { return fmt.Sprintf("https://example.youtrack.cloud/issue/OPS-%d", n) },
		dropped: true,
		serve:   ytServe,
	}
	trackerAPIs["jira-dc"] = trackerAPI{
		script: "jira-dc.sh",
		env: func(api, secret string) []string {
			return []string{"JIRA_URL=" + api, "JIRA_WEB=https://jira.example.com", "JIRA_TOKEN_FILE=" + secret, "JIRA_PROJECT=OPS"}
		},
		link:        func(n int) string { return fmt.Sprintf("https://jira.example.com/browse/OPS-%d", n) },
		labels:      true,
		dropped:     true,
		markerLabel: true,
		serve:       jdcServe,
	}
	trackerSecrets["youtrack"] = "perm-bGFwaWxsaQ==.NDMtMQ==.yt0secret"
	trackerSecrets["jira-dc"] = "MDk4NzY1NDMyMTpqaXJhLWRjLXNlY3JldA=="
}

// ytFields is v with only the attributes fields= names at its top, as
// YouTrack answers, and its $type.
func ytFields(v map[string]any, fields string) map[string]any {
	out := map[string]any{}
	if t, ok := v["$type"]; ok {
		out["$type"] = t
	}
	depth, from := 0, 0
	name := func(s string) {
		if i := strings.IndexByte(s, '('); i >= 0 {
			s = s[:i]
		}
		if x, ok := v[s]; ok {
			out[s] = x
		}
	}
	for i := 0; i < len(fields); i++ {
		switch fields[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				name(fields[from:i])
				from = i + 1
			}
		}
	}
	name(fields[from:])
	return out
}

func ytServe(f *fakeTracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+trackerSecrets["youtrack"] {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]any{"error": "Unauthorized", "error_description": "You are not logged in."})
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		q := r.URL.Query()
		fields := q.Get("fields")
		answer := func(code int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			json.NewEncoder(w).Encode(v)
		}
		fail := func(code int, e, desc string) {
			answer(code, map[string]any{"error": e, "error_description": desc})
		}
		issue := func(i *fakeIssue) map[string]any {
			state := map[string]string{"open": "In Progress", "done": "Fixed", "dropped": "Won't fix"}[i.state]
			project := "OPS"
			if i.team != "" {
				project = "OTHER"
			}
			out := map[string]any{"$type": "Issue", "id": fmt.Sprintf("2-%d", i.n), "idReadable": fmt.Sprintf("%s-%d", project, i.n),
				"reporter": map[string]any{"$type": "User", "id": ytUser(i.author)}, "project": map[string]any{"$type": "Project", "shortName": project},
				"summary": i.title, "description": i.body, "updated": i.updated.UnixMilli(), "resolved": nil,
				"customFields": []map[string]any{
					{"$type": "SingleEnumIssueCustomField", "name": "Priority", "value": map[string]any{"$type": "EnumBundleElement", "name": "Normal"}},
					{"$type": "StateIssueCustomField", "name": "State", "value": map[string]any{"$type": "StateBundleElement", "name": state}},
				}}
			if i.state != "open" {
				out["resolved"] = i.stateAt().UnixMilli()
			}
			return out
		}
		n := commentPath(`^/api/issues/OPS-([0-9]+)$`, r.URL.Path)
		if m := regexp.MustCompile(`^/api/issues/OPS-([0-9]+)/comments/4-([0-9]+)$`).FindStringSubmatch(r.URL.Path); m != nil && r.Method == "POST" {
			in, _ := strconv.Atoi(m[1])
			cn, _ := strconv.Atoi(m[2])
			var body struct{ Text string }
			if err := readBody(r, &body); err != nil || body.Text == "" {
				fail(http.StatusBadRequest, "bad_request", "text is required")
				return
			}
			if f.issue(in) == nil || !f.editComment(f.issue(in), cn, body.Text) {
				fail(http.StatusNotFound, "Not Found", "Entity not found")
				return
			}
			answer(http.StatusOK, ytFields(map[string]any{"$type": "IssueComment", "id": fmt.Sprintf("4-%d", cn)}, fields))
			return
		}
		if cn := commentPath(`^/api/issues/OPS-([0-9]+)/comments$`, r.URL.Path); cn > 0 && f.issue(cn) != nil {
			i := f.issue(cn)
			if r.Method == "POST" {
				var in struct{ Text string }
				if err := readBody(r, &in); err != nil || in.Text == "" {
					fail(http.StatusBadRequest, "bad_request", "text is required")
					return
				}
				f.posted++
				c := f.comment(i, trBot, in.Text)
				answer(http.StatusOK, ytFields(map[string]any{"$type": "IssueComment", "id": fmt.Sprintf("4-%d", c.id), "text": c.body}, fields))
				return
			}
			if !strings.Contains(fields, "visibility") || !strings.Contains(fields, "deleted") {
				f.t.Errorf("youtrack: comments %v", q)
			}
			// Deleted comments are kept, as deleted, the tracker's notes
			// among them here.
			cs := trComments(i, func(c *fakeComment) map[string]any {
				vis := map[string]any{"$type": "UnlimitedVisibility"}
				if c.restricted {
					vis = map[string]any{"$type": "LimitedVisibility", "permittedGroups": []map[string]any{{"id": "3-2", "name": "Admins"}}, "permittedUsers": []any{}}
				}
				updated := any(nil)
				if !c.edited.IsZero() {
					updated = c.edited.UnixMilli()
				}
				return ytFields(map[string]any{"$type": "IssueComment", "id": fmt.Sprintf("4-%d", c.id), "text": c.body, "created": c.at.UnixMilli(),
					"updated": updated, "deleted": c.system, "visibility": vis,
					"author": map[string]any{"$type": "User", "id": ytUser(c.author), "login": strings.ToLower(c.author), "fullName": c.author}}, fields)
			}, true)
			top, _ := strconv.Atoi(q.Get("$top"))
			skip, _ := strconv.Atoi(q.Get("$skip"))
			if top < 1 || top > 100 || skip%top != 0 {
				f.t.Errorf("youtrack: comments $top %q $skip %q", q.Get("$top"), q.Get("$skip"))
				top = 42
			}
			answer(http.StatusOK, pageOf(cs, skip/top+1, top))
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/users/me":
			answer(http.StatusOK, ytFields(map[string]any{"$type": "Me", "id": ytUser(trBot), "login": trBot, "fullName": "Lapilli Bot"}, fields))
		case r.Method == "GET" && r.URL.Path == "/api/admin/projects":
			// Paged, as the projects the token reads: the one asked for
			// isn't on the first page.
			all := []map[string]any{}
			for k := range 120 {
				all = append(all, map[string]any{"$type": "Project", "id": fmt.Sprintf("0-%d", 100+k), "shortName": fmt.Sprintf("P%d", k)})
			}
			if !f.noProject {
				all = append(all, map[string]any{"$type": "Project", "id": "0-7", "shortName": "OPS"})
			}
			top, _ := strconv.Atoi(q.Get("$top"))
			skip, _ := strconv.Atoi(q.Get("$skip"))
			if top != 100 || skip%100 != 0 {
				f.t.Errorf("youtrack: projects %v", q)
			}
			page := []map[string]any{}
			for _, p := range pageOf(all, skip/100+1, 100) {
				page = append(page, ytFields(p, fields))
			}
			answer(http.StatusOK, page)
		case r.Method == "GET" && r.URL.Path == "/api/issues" && f.noSearch:
			fail(http.StatusServiceUnavailable, "service_unavailable", "Search is not available.")
		case r.Method == "GET" && r.URL.Path == "/api/issues":
			// The project, who reported them and the marker are asked for,
			// and checked again: all those with the marker are given.
			m := regexp.MustCompile(`^project: \{OPS\} reporter: me "(lapilli-task-[0-9]+)"$`).FindStringSubmatch(q.Get("query"))
			if m == nil || q.Get("$top") != "20" {
				f.t.Errorf("youtrack: search %v", q)
				fail(http.StatusBadRequest, "bad_request", "Can't parse the query.")
				return
			}
			out := []map[string]any{}
			for _, i := range f.search(m[1]) {
				out = append(out, ytFields(issue(i), fields))
			}
			answer(http.StatusOK, out)
		case r.Method == "POST" && r.URL.Path == "/api/issues":
			var in struct {
				Project              struct{ ID string }
				Summary, Description string
			}
			if err := readBody(r, &in); err != nil || in.Project.ID != "0-7" || in.Summary == "" || f.refused(in.Summary) {
				fail(http.StatusBadRequest, "bad_request", "Summary is required.")
				return
			}
			if strings.ContainsAny(in.Summary, "\r\n") {
				fail(http.StatusBadRequest, "bad_request", "A summary is one line.")
				return
			}
			f.creates++
			out := ytFields(issue(f.made(f.add(trBot, in.Summary, in.Description, nil))), fields)
			if f.badURL {
				out["idReadable"] = "EVIL-1"
			}
			answer(http.StatusOK, out)
		case f.gone(n):
			fail(http.StatusNotFound, "Not Found", fmt.Sprintf("Entity with id OPS-%d not found", n))
		case f.hidden(n):
			fail(http.StatusForbidden, "Forbidden", "You have no permission to read the issue.")
		case r.Method == "GET" && f.issue(n) != nil:
			f.read(n)
			answer(http.StatusOK, ytFields(issue(f.issue(n)), fields))
		case r.Method == "POST" && f.noClose:
			fail(http.StatusForbidden, "Forbidden", "You have no permission to update the issue.")
		case r.Method == "POST" && f.issue(n) != nil:
			var in struct {
				CustomFields []struct {
					Name  string
					Type  string `json:"$type"`
					Value struct {
						Name string
						Type string `json:"$type"`
					}
				}
			}
			readBody(r, &in)
			if len(in.CustomFields) != 1 || in.CustomFields[0].Name != "State" || in.CustomFields[0].Type != "StateIssueCustomField" {
				f.t.Errorf("youtrack: update %+v", in)
			}
			i := f.issue(n)
			switch in.CustomFields[0].Value.Name {
			case "Fixed":
				i.state = "done"
			case "Won't fix":
				i.state = "dropped"
			default:
				f.t.Errorf("youtrack: to %q", in.CustomFields[0].Value.Name)
			}
			i.updated, i.changed = time.Now(), time.Now()
			answer(http.StatusOK, ytFields(issue(i), fields))
		default:
			f.t.Errorf("youtrack: %s %s", r.Method, r.URL)
			fail(http.StatusNotFound, "Not Found", "Not found")
		}
	}
}

func jdcServe(f *fakeTracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+trackerSecrets["jira-dc"] {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		seoul := time.FixedZone("KST", 9*3600)
		stamp := func(t time.Time) string { return t.In(seoul).Format("2006-01-02T15:04:05.000-0700") }
		user := func(author string) map[string]any {
			return map[string]any{"self": "https://jira.example.com/rest/api/2/user?username=" + author, "key": jdcUser(author),
				"name": strings.ToLower(author), "displayName": author, "active": true}
		}
		var n int
		if m := regexp.MustCompile(`^/rest/api/2/issue/OPS-([0-9]+)(/transitions)?$`).FindStringSubmatch(r.URL.Path); m != nil {
			n, _ = strconv.Atoi(m[1])
		}
		fields := func(i *fakeIssue) map[string]any {
			return map[string]any{"creator": user(i.author), "labels": i.labels}
		}
		if m := regexp.MustCompile(`^/rest/api/2/issue/OPS-([0-9]+)/comment/([0-9]+)$`).FindStringSubmatch(r.URL.Path); m != nil && r.Method == "PUT" {
			in, _ := strconv.Atoi(m[1])
			cn, _ := strconv.Atoi(m[2])
			var body struct{ Body any }
			readBody(r, &body)
			text, ok := body.Body.(string)
			if !ok || text == "" {
				http.Error(w, `{"errorMessages":[],"errors":{"comment":"Comment body can not be empty!"}}`, http.StatusBadRequest)
				return
			}
			if f.issue(in) == nil || !f.editComment(f.issue(in), cn, text) {
				http.Error(w, `{"errorMessages":["Can not find a comment for the id."]}`, http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": strconv.Itoa(cn), "body": text})
			return
		}
		if cn := commentPath(`^/rest/api/2/issue/OPS-([0-9]+)/comment$`, r.URL.Path); cn > 0 && f.issue(cn) != nil {
			i := f.issue(cn)
			if r.Method == "POST" {
				// A comment's body is wiki markup, a string.
				var in struct{ Body any }
				readBody(r, &in)
				text, ok := in.Body.(string)
				if !ok || text == "" {
					http.Error(w, `{"errorMessages":[],"errors":{"comment":"Comment body can not be empty!"}}`, http.StatusBadRequest)
					return
				}
				f.posted++
				c := f.comment(i, trBot, text)
				w.WriteHeader(http.StatusCreated)
				json.NewEncoder(w).Encode(map[string]any{"id": strconv.Itoa(c.id), "body": c.body, "author": user(trBot)})
				return
			}
			cs := trComments(i, func(c *fakeComment) map[string]any {
				out := map[string]any{"self": fmt.Sprintf("https://jira.example.com/rest/api/2/issue/%d/comment/%d", 10000+i.n, c.id),
					"id": strconv.Itoa(c.id), "author": user(c.author), "updateAuthor": user(c.author), "body": c.body,
					"created": stamp(c.at), "updated": stamp(c.editedAt())}
				if c.restricted {
					out["visibility"] = map[string]any{"type": "role", "value": "Administrators"}
				}
				return out
			}, false)
			if r.URL.Query().Get("orderBy") != "created" {
				f.t.Errorf("jira-dc: comments %v", r.URL.Query())
			}
			start, _ := strconv.Atoi(r.URL.Query().Get("startAt"))
			json.NewEncoder(w).Encode(map[string]any{"comments": pageOf(cs, start/100+1, 100), "startAt": start, "maxResults": 100, "total": len(cs)})
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/rest/api/2/myself":
			json.NewEncoder(w).Encode(user(trBot))
		case r.Method == "GET" && r.URL.Path == "/rest/api/2/project/OPS" && f.noProject:
			http.Error(w, `{"errorMessages":["No project could be found with key 'OPS'."],"errors":{}}`, http.StatusNotFound)
		case r.Method == "GET" && r.URL.Path == "/rest/api/2/project/OPS":
			json.NewEncoder(w).Encode(map[string]any{"id": "10000", "key": "OPS", "name": "Operations"})
		case r.Method == "GET" && r.URL.Path == "/rest/api/2/search" && f.noSearch:
			http.Error(w, `{"errorMessages":["Rate limit exceeded."]}`, http.StatusServiceUnavailable)
		case r.Method == "GET" && r.URL.Path == "/rest/api/2/search":
			// Who made them, and the label lapilli, are asked for and
			// checked again: all those with the marker are given.
			m := regexp.MustCompile(`^project = OPS AND labels = "([^"]+)" AND labels = "lapilli" AND creator = currentUser\(\)$`).FindStringSubmatch(r.URL.Query().Get("jql"))
			if m == nil || r.URL.Query().Get("fields") != "creator,labels" {
				f.t.Errorf("jira-dc: jql %q", r.URL.Query().Get("jql"))
				http.Error(w, `{"errorMessages":["bad jql"]}`, http.StatusBadRequest)
				return
			}
			out := []map[string]any{}
			for _, i := range f.issues {
				if slices.Contains(i.labels, m[1]) {
					out = append(out, map[string]any{"id": strconv.Itoa(10000 + i.n), "key": fmt.Sprintf("OPS-%d", i.n), "fields": fields(i)})
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"startAt": 0, "maxResults": 20, "total": len(out), "issues": out})
		case r.Method == "POST" && r.URL.Path == "/rest/api/2/issue":
			var in struct {
				Fields struct {
					Project   struct{ Key string }
					Issuetype struct{ Name string }
					Summary   string
					Labels    []string
					// Wiki markup, a string; an object is refused.
					Description any
				}
			}
			err := readBody(r, &in)
			desc, ok := in.Fields.Description.(string)
			if err != nil || !ok || in.Fields.Project.Key != "OPS" || in.Fields.Issuetype.Name != "Task" || in.Fields.Summary == "" || f.refused(in.Fields.Summary) {
				http.Error(w, `{"errorMessages":[],"errors":{"summary":"You must specify a summary of the issue."}}`, http.StatusBadRequest)
				return
			}
			if strings.ContainsAny(in.Fields.Summary, "\r\n") {
				http.Error(w, `{"errorMessages":[],"errors":{"summary":"The summary is invalid because it contains newline characters."}}`, http.StatusBadRequest)
				return
			}
			f.creates++
			i := f.made(f.add(trBot, in.Fields.Summary, desc, in.Fields.Labels))
			key := fmt.Sprintf("OPS-%d", i.n)
			if f.badURL {
				key = "EVIL-1"
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": strconv.Itoa(10000 + i.n), "key": key, "self": "https://jira.example.com/rest/api/2/issue/" + key})
		case f.gone(n):
			http.Error(w, `{"errorMessages":["Issue Does Not Exist"],"errors":{}}`, http.StatusNotFound)
		case f.hidden(n):
			http.Error(w, `{"errorMessages":["You do not have the permission to see the specified issue."],"errors":{}}`, http.StatusForbidden)
		case r.Method == "GET" && n > 0 && f.issue(n) != nil && !strings.HasSuffix(r.URL.Path, "/transitions"):
			f.read(n)
			i := f.issue(n)
			cat, resolution := "indeterminate", any(nil)
			switch i.state {
			case "done":
				cat, resolution = "done", map[string]any{"name": "Fixed"}
			case "dropped":
				cat, resolution = "done", map[string]any{"name": "Won't Fix"}
			}
			out := fields(i)
			out["status"] = map[string]any{"name": "Whatever", "statusCategory": map[string]any{"key": cat}}
			out["resolution"], out["updated"] = resolution, stamp(i.updated)
			// No statuscategorychangedate: read by resolutiondate, as a
			// Jira without the field is.
			if resolution != nil {
				out["resolutiondate"] = stamp(i.stateAt())
			}
			json.NewEncoder(w).Encode(map[string]any{"key": fmt.Sprintf("OPS-%d", i.n), "fields": out})
		case r.Method == "GET" && n > 0 && f.issue(n) != nil:
			json.NewEncoder(w).Encode(map[string]any{"expand": "transitions", "transitions": []map[string]any{
				{"id": "11", "name": "Start", "to": map[string]any{"statusCategory": map[string]any{"key": "indeterminate"}}},
				{"id": "31", "name": "Finish", "to": map[string]any{"statusCategory": map[string]any{"key": "done"}}},
			}})
		case r.Method == "POST" && n > 0 && f.noClose:
			http.Error(w, `{"errorMessages":["You do not have permission to transition this issue."],"errors":{}}`, http.StatusForbidden)
		case r.Method == "POST" && n > 0 && f.issue(n) != nil:
			var in struct{ Transition struct{ ID string } }
			readBody(r, &in)
			if in.Transition.ID != "31" {
				f.t.Errorf("jira-dc: transition %+v", in)
			}
			i := f.issue(n)
			i.state, i.updated, i.changed = "done", time.Now(), time.Now()
			w.WriteHeader(http.StatusNoContent)
		default:
			f.t.Errorf("jira-dc: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}
}

// Jira Data Center reads an issue's text and its comments as wiki markup:
// the task's text and its names go in a {noformat} block, which shows them
// as written (a PromQL selector's braces aren't a macro, [~name] mentions
// no one, !URL! shows no image), and a {noformat} in them, in any case,
// can't end it; a line of the task's text like the link back doesn't move
// the rest out of it. A comment posted from Lapilli is quoted in a block
// too, its first line (who wrote it) and its last (the marker) outside.
func TestTrackerRecipeJiraDCWikiText(t *testing.T) {
	parallel(t)
	needTools(t)
	g := newTrackerRig(t, "jira-dc")
	g.mustRun(t) // tasks 1 and 2 get issues 1 and 2
	evil := "Errors [~admin] !https://evil.example/i.png! {noformat}{html}<b>x</b>{html}"
	id := g.put(t, ledger.Task{Title: "Wiki", Rule: evil, Assignee: "{NoFormat}[~ceo]",
		Body: "rate(errors{job=\"api\"}[5m]) > 1\nIn Lapilli: https://evil.example\n\n{noformat}\n[~admin]"})
	g.mustRun(t)
	i := g.f.last()
	if g.task(t, id).Link != g.api.link(i.n) {
		t.Fatalf("task %d: %q", id, g.task(t, id).Link)
	}
	// The block is the text up to the link back: nothing of the task's
	// after its first line, nor any name, is outside it.
	open, rest, ok := strings.Cut(i.body, "\n")
	inside, after, closed := strings.Cut(rest, "\n{noformat}\n")
	if !ok || open != "{noformat}" || !closed || after != "In Lapilli: https://lapilli.example.com/tasks/"+strconv.FormatInt(id, 10)+"\n\nKept in step with Lapilli: lapilli-task-"+strconv.FormatInt(id, 10) {
		t.Fatalf("the issue's text isn't one block, then the link and the marker:\n%s", i.body)
	}
	if strings.Contains(strings.ToLower(inside), "{noformat") {
		t.Errorf("a {noformat} inside the block can end it:\n%s", inside)
	}
	for _, want := range []string{"rate(errors{job=\"api\"}[5m]) > 1", "[~admin]", "!https://evil.example/i.png!", "In Lapilli: https://evil.example", "[~ceo]"} {
		if !strings.Contains(inside, want) {
			t.Errorf("the block doesn't hold %q as written:\n%s", want, inside)
		}
	}
	// A comment: in a block, between who wrote it and its marker.
	cid, err := g.s.l.PutComment(context.Background(), ledger.Comment{On: "task", Key: "1",
		Body: "Raised to {job=\"api\"}.\n{noformat}\n[~admin] see !https://evil.example/i.png!"}, "ana@example.com")
	if err != nil {
		t.Fatal(err)
	}
	g.mustRun(t, "COMMENTS=true")
	g.f.mu.Lock()
	cs := g.f.issue(1).comments
	g.f.mu.Unlock()
	if len(cs) != 1 {
		t.Fatalf("posted %d comments", len(cs))
	}
	lines := strings.Split(cs[0].body, "\n")
	body := strings.Join(lines[3:len(lines)-3], "\n")
	if len(lines) < 7 || lines[0] != "Written in Lapilli:" || lines[2] != "{noformat}" || lines[len(lines)-3] != "{noformat}" ||
		lines[len(lines)-1] != fmt.Sprintf("Kept in step with Lapilli: lapilli-comment-%d", cid) || strings.Contains(strings.ToLower(body), "{noformat") ||
		!strings.Contains(body, "> Raised to {job=\"api\"}.") || !strings.Contains(body, "> [~admin] see") {
		t.Errorf("the comment posted:\n%s", cs[0].body)
	}
	// Found again by its marker, not posted twice.
	made := g.f.posts()
	g.mustRun(t, "COMMENTS=true")
	if g.f.posts() != made {
		t.Errorf("posted again")
	}
}

// YouTrack's and Jira Data Center's 429, and a 503 on a read, are waited
// out as long as Retry-After says, and asked again: the run makes its
// issues, each once.
func TestTrackerRecipeDCRetryAfter(t *testing.T) {
	parallel(t)
	needTools(t)
	for _, c := range []struct {
		name, path string
		code       int
	}{{"youtrack", "/api/issues", http.StatusTooManyRequests}, {"jira-dc", "/rest/api/2/search", http.StatusTooManyRequests},
		{"youtrack", "/api/users/me", http.StatusServiceUnavailable}, {"jira-dc", "/rest/api/2/myself", http.StatusServiceUnavailable}} {
		t.Run(c.name+" "+strconv.Itoa(c.code), func(t *testing.T) {
			t.Parallel()
			var refused atomic.Int32
			g := retryRig(t, c.name, func(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
				if r.Method == http.MethodGet && r.URL.Path == c.path && refused.Add(1) <= 2 {
					w.Header().Set("Retry-After", "1")
					http.Error(w, `{"error":"Too Many Requests"}`, c.code)
					return
				}
				next(w, r)
			}, passed)
			out, err := runTracker(g)
			if err != nil || refused.Load() < 3 || g.f.created() != 2 || g.task(t, 1).Link == "" {
				t.Errorf("%v; refused %d, made %d\n%s", err, refused.Load(), g.f.created(), out)
			}
		})
	}
}
