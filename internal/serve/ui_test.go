package serve

// The frame of the pages (docs/design/ui.md): the page context carried on
// every link, the reader's own team, and a main action shown disabled
// with why rather than hidden.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"lapilli.dev/internal/ledger"
)

func TestLink(t *testing.T) {
	for _, c := range []struct {
		ctx    pageCtx
		target string
		kv     []any
		want   string
	}{
		{pageCtx{}, "/rules", nil, "/rules"},
		{pageCtx{Team: "pay", TeamSet: true, Weeks: 12}, "/incidents", nil, "/incidents?team=pay&weeks=12"},
		// All teams, chosen, is carried as such.
		{pageCtx{TeamSet: true}, "/tasks", nil, "/tasks?team="},
		// The reader's own team, taken for them, isn't: the next page takes
		// it again.
		{pageCtx{Team: "pay", Mine: true}, "/tasks", nil, "/tasks"},
		// What the target or the pairs set wins; an empty value takes a
		// parameter out, but team's, which is all teams.
		{pageCtx{Team: "pay", TeamSet: true, Weeks: 4}, "/rules?weeks=12", nil, "/rules?team=pay&weeks=12"},
		{pageCtx{Team: "pay", TeamSet: true, Weeks: 4}, "/rules", []any{"weeks", 1, "sort", "", "team", ""}, "/rules?team=&weeks=1"},
		{pageCtx{Team: "a b&c", TeamSet: true}, "/rules/" + url.PathEscape("x y") + "#says", nil, "/rules/x%20y?team=a+b%26c#says"},
	} {
		if got := c.ctx.Link(c.target, c.kv...); got != c.want {
			t.Errorf("%+v Link(%q, %v) = %q, want %q", c.ctx, c.target, c.kv, got, c.want)
		}
	}
}

// TestContextCarried crawls every page with a team and a period, and finds
// both on every link to another page: those that choose another team or
// period choose it, and say so.
func TestContextCarried(t *testing.T) {
	s, paths := a11yServer(t)
	h := s.Handler()
	links := 0
	for _, p := range append(paths, "/tasks?status=done", "/reports?edit=1", "/teams?edit=1") {
		u, _ := url.Parse(p)
		q := u.Query()
		q.Set("team", "payments")
		q.Set("weeks", "4")
		u.RawQuery = q.Encode()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, u.String(), nil))
		doc, err := html.Parse(w.Body)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range allNodes(doc) {
			href := nodeAttr(n, "href")
			if n.DataAtom != atom.A || !strings.HasPrefix(href, "/") || strings.HasPrefix(href, "//") || !toPage(href) {
				continue
			}
			links++
			l, err := url.Parse(href)
			if err != nil {
				t.Errorf("%s: %q: %v", p, href, err)
				continue
			}
			text := squashed(nodeText(n))
			switch team, ok := l.Query()["team"]; {
			case !ok:
				t.Errorf("%s: %q (%q) doesn't carry the team", p, href, text)
			case team[0] != "payments" && !(team[0] == "" && text == "all"):
				t.Errorf("%s: %q (%q) carries another team", p, href, text)
			}
			switch weeks := l.Query().Get("weeks"); {
			case weeks == "":
				t.Errorf("%s: %q (%q) doesn't carry the period", p, href, text)
			case weeks != "4" && !strings.Contains(text, "week"):
				t.Errorf("%s: %q (%q) carries another period", p, href, text)
			}
		}
	}
	t.Logf("%d links crawled", links)
	if links < 500 {
		t.Errorf("only %d links crawled", links)
	}
}

// toPage reports whether a link on this server goes to a page, not a file,
// the sign-in, the API or the choice of a language.
func toPage(href string) bool {
	for _, not := range []string{"/static/", "/auth/", "/api/", "/settings.yaml", "/lang?", "/metrics", "/mcp"} {
		if strings.HasPrefix(href, not) {
			return false
		}
	}
	return true
}

// TestDefaultTeam: with no team= in the URL, a reader who edits one team,
// and only one, sees that team's; everyone else, all teams.
func TestDefaultTeam(t *testing.T) {
	s, pay, search := rolesServer(t, nil)
	ctx := context.Background()
	for _, g := range []ledger.Grant{{Who: "user:pat@example.com", TeamID: pay}, {Who: "user:two@example.com", TeamID: pay},
		{Who: "user:two@example.com", TeamID: search}, {Who: "user:every@example.com"}} {
		if _, err := s.l.PutGrant(ctx, g, "boss"); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		who, query, team string
		mine             bool
	}{
		{"pat@example.com", "", "payments", true},
		{"two@example.com", "", "", false},
		{"every@example.com", "", "", false},
		{"boss@example.com", "", "", false},
		{"ed@example.com", "", "", false},
		{"vic@example.com", "", "", false},
		{"", "", "", false},
		// All teams, chosen; another team, chosen.
		{"pat@example.com", "?team=", "", false},
		{"pat@example.com", "?team=search", "search", false},
	} {
		r := httptest.NewRequest(http.MethodGet, "/rules"+c.query, nil)
		if c.who != "" {
			r.Header.Set("X-Forwarded-Email", c.who)
		}
		if got := s.pageContext(r); got.Team != c.team || got.Mine != c.mine {
			t.Errorf("%q%s: team %q (mine %v), want %q (%v)", c.who, c.query, got.Team, got.Mine, c.team, c.mine)
		}
	}

	// The pages that show one team's show the reader's own; a link to all
	// teams says so in the URL, and the API doesn't take a team unasked.
	h := s.Handler()
	for _, tk := range []ledger.Task{{Title: "pay's", Kind: "tune", TeamID: pay}, {Title: "search's", Kind: "tune", TeamID: search}} {
		if _, err := s.l.PutTask(ctx, tk, "boss"); err != nil {
			t.Fatal(err)
		}
	}
	_, body, _ := ui(t, h, http.MethodGet, "/tasks", "pat@example.com", "", nil)
	if !strings.Contains(body, "pay&#39;s") || strings.Contains(body, "search&#39;s") || !strings.Contains(body, "payments (your team)") {
		t.Errorf("/tasks for pat isn't payments', as pat's own team:\n%s", body)
	}
	if !strings.Contains(body, `href="/tasks?status=open&amp;team="`) {
		t.Error("/tasks for pat: no link to every team's")
	}
	_, body, _ = ui(t, h, http.MethodGet, "/tasks?team=", "pat@example.com", "", nil)
	if !strings.Contains(body, "pay&#39;s") || !strings.Contains(body, "search&#39;s") || strings.Contains(body, "(your team)") {
		t.Error("/tasks?team= for pat isn't every team's")
	}
	_, body, _ = ui(t, h, http.MethodGet, "/tasks", "two@example.com", "", nil)
	if !strings.Contains(body, "search&#39;s") {
		t.Error("/tasks for two, who edits two teams, isn't every team's")
	}
	_, body, _ = ui(t, h, http.MethodGet, "/api/v1/rules", "pat@example.com", "", nil)
	var v struct{ Team struct{ Name string } }
	if err := json.Unmarshal([]byte(body), &v); err != nil || v.Team.Name != "" {
		t.Errorf("/api/v1/rules for pat took a team: %q %v", v.Team.Name, err)
	}
}

// TestActionDisabled: a page's main action is a link to its form for whoever
// may use it, and for everyone else the button disabled, with why beside
// it, tied to it.
func TestActionDisabled(t *testing.T) {
	disabled := func(label, why string) string {
		return `<button type="button" disabled aria-describedby="action-why">` + label + `</button> <span id="action-why" class="why">` + why + `</span>`
	}
	check := func(t *testing.T, h http.Handler, who, path, want string) {
		t.Helper()
		code, body, _ := ui(t, h, http.MethodGet, path, who, "", nil)
		if code != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("%s as %q: %d, no %s", path, who, code, want)
		}
	}
	t.Run("read-only", func(t *testing.T) {
		s, _ := server(t, func(c *Config) { c.ReadOnly = true })
		h := s.Handler()
		for path, label := range map[string]string{"/incidents": "Write one down", "/tasks": "Add a task", "/windows": "Add a quiet window", "/teams": "Add a team", "/reports": "Add a report"} {
			check(t, h, "", path, disabled(label, "Read-only: lapilli serve was started with --read-only"))
		}
	})
	t.Run("roles", func(t *testing.T) {
		s, pay, _ := rolesServer(t, nil)
		if _, err := s.l.PutGrant(context.Background(), ledger.Grant{Who: "user:pat@example.com", TeamID: pay}, "boss"); err != nil {
			t.Fatal(err)
		}
		h := s.Handler()
		check(t, h, "vic@example.com", "/incidents", disabled("Write one down", "Editors only"))
		check(t, h, "vic@example.com", "/tasks", disabled("Add a task", "Editors only"))
		check(t, h, "", "/incidents", disabled("Write one down", "Read-only: no one is signed in"))
		check(t, h, "ed@example.com", "/teams", disabled("Add a team", "Admins only"))
		// A team's editor writes no incident, and adds their team's tasks.
		check(t, h, "pat@example.com", "/incidents", disabled("Write one down", "Editors only"))
		check(t, h, "pat@example.com", "/tasks", `<a class="button" href="/tasks#add">Add a task</a>`)
		check(t, h, "ed@example.com", "/incidents?team=search", `<a class="button" href="/incidents?team=search#add">Write one down</a>`)
		check(t, h, "boss@example.com", "/teams?edit=1", `<a class="button" href="/teams#add">Add a team</a>`)
		check(t, h, "boss@example.com", "/reports", `<a class="button" href="/reports#add">Add a report</a>`)
		for _, path := range []string{"/incidents", "/tasks", "/teams", "/reports"} {
			if _, body, _ := ui(t, h, http.MethodGet, path, "boss@example.com", "", nil); !strings.Contains(body, `id="add"`) {
				t.Errorf("%s: no form the action goes to", path)
			}
		}
	})
	t.Run("no roles beyond this machine", func(t *testing.T) {
		s, _ := server(t, nil)
		s.open.Store(false)
		check(t, s.Handler(), "", "/windows", disabled("Add a quiet window", "Read-only: no one is given a role here"))
	})
	t.Run("nowhere to send a report", func(t *testing.T) {
		s, _ := server(t, nil)
		check(t, s.Handler(), "", "/reports", disabled("Add a report", "Nowhere to send a report yet"))
	})
}

// TestSidebar: the pages in their groups, the page the reader is on, and
// Roles only for whoever may open it.
func TestSidebar(t *testing.T) {
	s, _, _ := rolesServer(t, nil)
	h := s.Handler()
	for who, roles := range map[string]bool{"boss@example.com": true, "ed@example.com": false, "": false} {
		_, body, _ := ui(t, h, http.MethodGet, "/rules/Errors", who, "", nil)
		doc, err := html.Parse(strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		var groups, crumbs []string
		var current []string
		for _, n := range allNodes(doc) {
			switch {
			case nodeAttr(n, "class") == "nav-group" && strings.HasPrefix(nodeAttr(n, "id"), "side-"):
				groups = append(groups, nodeText(n))
			case n.DataAtom == atom.Li && nodeWithin(n, atom.Ol):
				crumbs = append(crumbs, nodeText(n))
			case n.DataAtom == atom.A && nodeAttr(n, "aria-current") != "" && nodeAttr(n.Parent.Parent.Parent, "class") == "nav-side":
				current = append(current, nodeText(n)+" "+nodeAttr(n, "aria-current"))
			}
		}
		if !slices.Equal(groups, []string{"See", "Fix", "Report", "Manage"}) {
			t.Errorf("%q: groups %q", who, groups)
		}
		if !slices.Equal(crumbs, []string{"See", "Rules", "Errors"}) || !strings.Contains(body, `<span aria-current="page">Errors</span>`) {
			t.Errorf("%q: breadcrumb %q", who, crumbs)
		}
		if !slices.Equal(current, []string{"Rules true"}) {
			t.Errorf("%q: current %q", who, current)
		}
		if got := strings.Contains(body, `href="/roles"`); got != roles {
			t.Errorf("%q: Roles in the sidebar %v, want %v", who, got, roles)
		}
	}
}
