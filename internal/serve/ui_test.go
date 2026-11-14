package serve

// The frame of the pages (docs/design/ui.md): the page context carried on
// every link, the reader's own team, and a main action shown disabled
// with why rather than hidden.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
// both on every link to another page, and in every form that is sent as a
// GET: those that choose another team or period choose it, and say so. A
// page that finds nothing carries neither.
func TestContextCarried(t *testing.T) {
	s, paths := a11yServer(t)
	h := s.Handler()
	links, forms := 0, 0
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
			if n.DataAtom == atom.Form && w.Code == http.StatusOK {
				get := strings.EqualFold(nodeAttr(n, "method"), "get")
				names := map[string]bool{}
				for _, m := range allNodes(n) {
					get = get || m.DataAtom == atom.Button && strings.EqualFold(nodeAttr(m, "formmethod"), "get")
					if m.DataAtom == atom.Input || m.DataAtom == atom.Select {
						names[nodeAttr(m, "name")] = true
					}
				}
				if get {
					forms++
					if !names["team"] || !names["weeks"] {
						t.Errorf("%s: the form to %s doesn't carry the team and period", p, nodeAttr(n, "action"))
					}
				}
			}
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
			if w.Code != http.StatusOK {
				if l.Query().Has("team") || l.Query().Has("weeks") {
					t.Errorf("%s, %d: %q (%q) carries the context", p, w.Code, href, text)
				}
				continue
			}
			switch team, ok := l.Query()["team"]; {
			case !ok:
				t.Errorf("%s: %q (%q) doesn't carry the team", p, href, text)
			case team[0] != "payments" && !(team[0] == "" && (text == "all" || strings.HasPrefix(text, "Remove filter: Team:") || text == "The same filters, in all teams" ||
				text == "Add a task about this rule" || text == "Add a task about this incident")):
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
	t.Logf("%d links and %d forms crawled", links, forms)
	if links < 500 || forms < 80 {
		t.Errorf("only %d links and %d forms crawled", links, forms)
	}

	// A task added from a page goes to the task's page, with the context.
	code, body, _ := get(t, h, "/tasks?team=payments&weeks=12")
	if code != http.StatusOK || !strings.Contains(body, `<form method="post" action="/tasks?team=payments&amp;weeks=12" class="grid">`) {
		t.Fatal("/tasks?team=payments&weeks=12: the form to add a task doesn't carry the context")
	}
	code, _, hd := post(t, h, "/tasks?team=payments&weeks=12", url.Values{"title": {"x"}, "kind": {"tune"}, "team": {"0"}}, "same-origin")
	loc := hd.Get("Location")
	if code != http.StatusSeeOther || !regexp.MustCompile(`^/tasks/[0-9]+\?team=payments&weeks=12$`).MatchString(loc) {
		t.Fatalf("a task added: %d to %q", code, loc)
	}
	// On the task's page, a change of status and a delete come back with
	// the context too.
	id := strings.TrimPrefix(strings.SplitN(loc, "?", 2)[0], "/tasks/")
	_, body, _ = get(t, h, loc)
	for _, want := range []string{`<form method="post" action="/tasks?team=payments&amp;weeks=12"><input type="hidden" name="id" value="` + id + `"><input type="hidden" name="only" value="status">`,
		`<form method="post" action="/tasks/` + id + `/delete?team=payments&amp;weeks=12">`} {
		if !strings.Contains(body, want) {
			t.Errorf("%s: no %s", loc, want)
		}
	}
	code, _, hd = post(t, h, "/tasks?team=payments&weeks=12", url.Values{"id": {id}, "only": {"status"}, "status": {"done"}}, "same-origin")
	if l := hd.Get("Location"); code != http.StatusSeeOther || l != "/tasks/"+id+"?team=payments&weeks=12" {
		t.Errorf("a task's status changed: %d to %q", code, l)
	}
	code, _, hd = post(t, h, "/tasks/"+id+"/delete?team=payments&weeks=12", url.Values{}, "same-origin")
	if l := hd.Get("Location"); code != http.StatusSeeOther || l != "/tasks?team=payments&weeks=12" {
		t.Errorf("a task deleted: %d to %q", code, l)
	}
}

// TestLongSearchAnyScript: a search of 500 characters is a search, in any
// script, though one of Hangul is 4.5 KB in a URL; past 500, the page
// says it is too long; neither is refused.
func TestLongSearchAnyScript(t *testing.T) {
	h := facetServer(t).Handler()
	for _, ch := range []string{"a", "가", "検"} {
		for _, path := range []string{"/rules", "/search"} {
			code, body, _ := get(t, h, path+"?team=&q="+url.QueryEscape(strings.Repeat(ch, maxSearch)))
			if code != http.StatusOK || strings.Contains(body, "The search is too long") {
				t.Errorf("%s, %d × %q: %d", path, maxSearch, ch, code)
			}
			code, body, _ = get(t, h, path+"?team=&q="+url.QueryEscape(strings.Repeat(ch, maxSearch+1)))
			if code != http.StatusOK || !strings.Contains(body, "The search is too long") {
				t.Errorf("%s, %d × %q: %d", path, maxSearch+1, ch, code)
			}
		}
	}
}

// TestShortNext: where a reader goes back to after signing in is kept to
// what the sign-in's cookie holds: past it, the page alone, else the
// overview.
func TestShortNext(t *testing.T) {
	long := strings.Repeat("a", maxNext)
	for _, c := range []struct{ in, want string }{
		{"/rules?perf=noisy", "/rules?perf=noisy"},
		{"/rules?q=" + long, "/rules"},
		{"/rules/" + long, "/"},
		{"/" + long[1:], "/" + long[1:]},
	} {
		if got := shortNext(c.in); got != c.want {
			t.Errorf("shortNext(%.40q…) = %.40q, want %.40q", c.in, got, c.want)
		}
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

// TestTaskAboutItsOwner: the task a rule's page adds is its owning team's,
// or no team's, whatever team the page is looked at for; an incident's is
// no team's.
func TestTaskAboutItsOwner(t *testing.T) {
	s := pagesServer(t)
	h := s.Handler()
	link := regexp.MustCompile(`<a href="([^"]*)">Add a task about this (?:rule|incident)</a>`)
	for path, want := range map[string]string{
		"/rules/Errors?team=":            "payments",
		"/rules/Errors?team=payments":    "payments",
		"/rules/Flapping?team=payments":  "",
		"/incidents/INC-1?team=payments": "",
	} {
		_, body, _ := get(t, h, path)
		m := link.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("%s: no link to add a task", path)
		}
		u, _ := url.Parse(html.UnescapeString(m[1]))
		if team, ok := u.Query()["team"]; !ok || team[0] != want {
			t.Errorf("%s: %s, want team=%s", path, m[1], want)
		}
		// The form it opens has that team picked, or none.
		_, form, _ := get(t, h, u.String())
		got := ""
		if m := regexp.MustCompile(`<option value="[0-9]+" selected>([^<]*)</option>`).FindStringSubmatch(form[strings.Index(form, `id="add"`):]); m != nil {
			got = m[1]
		}
		if got != want {
			t.Errorf("%s: the form picks %q, want %q", path, got, want)
		}
	}
}

// TestErrorPagesBare: a page that refuses, or finds nothing, or says one
// has signed out, is the frame alone: no count of what waits, nothing of
// the team or period the URL names, and not the reader's own team. A
// stranger reads nothing from it, not even whether a team is there.
func TestErrorPagesBare(t *testing.T) {
	s, pay, _ := rolesServer(t, func(c *Config) { c.Viewers = []string{"*@example.com"}; c.Version = "v9.9.9" })
	ctx := context.Background()
	// A run that failed, its error naming a host inside: the pages say so
	// to those let in, and a bare page to no one.
	s.fail("reading the rules", errors.New(`Get "http://prometheus.inside.svc:9090/api/v1/rules": refused`))
	if _, err := s.l.PutGrant(ctx, ledger.Grant{Who: "user:pat@example.com", TeamID: pay}, "boss"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutTask(ctx, ledger.Task{Title: "t", Kind: "tune", TeamID: pay}, "boss"); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	if _, body, _ := ui(t, h, http.MethodGet, "/", "ed@example.com", "", nil); !strings.Contains(body, `class="count"`) ||
		!strings.Contains(body, "prometheus.inside.svc") || !strings.Contains(body, "v9.9.9") {
		t.Fatal("no count, failed run or version on the overview")
	}
	bare := func(t *testing.T, what, body string) {
		t.Helper()
		if strings.Contains(body, `class="count"`) || strings.Contains(body, "your team") {
			t.Errorf("%s: a count, or the reader's own team", what)
		}
		if strings.Contains(body, "prometheus.inside.svc") || strings.Contains(body, "v9.9.9") {
			t.Errorf("%s: how the runs go, or the version", what)
		}
		doc, err := html.Parse(strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range allNodes(doc) {
			if href := nodeAttr(n, "href"); n.DataAtom == atom.A && strings.HasPrefix(href, "/") && toPage(href) {
				if l, _ := url.Parse(href); l.Query().Has("team") || l.Query().Has("weeks") {
					t.Errorf("%s: %q carries the context", what, href)
				}
			}
		}
	}
	bodies := map[string]string{}
	for _, c := range []struct {
		who, path string
		code      int
	}{
		{"eve@other.example", "/?team=payments&weeks=12", http.StatusForbidden},
		{"eve@other.example", "/?team=nope&weeks=12", http.StatusForbidden},
		{"ed@example.com", "/rules/Nothing?team=payments&weeks=12", http.StatusNotFound},
		{"pat@example.com", "/rules/Nothing", http.StatusNotFound},
	} {
		code, body, _ := ui(t, h, http.MethodGet, c.path, c.who, "", nil)
		if code != c.code {
			t.Errorf("%s as %s: %d, want %d", c.path, c.who, code, c.code)
		}
		bare(t, c.path+" as "+c.who, body)
		bodies[c.path] = body
	}
	if strings.ReplaceAll(bodies["/?team=payments&weeks=12"], "payments", "nope") != bodies["/?team=nope&weeks=12"] {
		t.Error("a stranger refused reads a team that is there apart from one that isn't")
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/signed-out?team=payments&weeks=12", nil)
	r.Header.Set("X-Forwarded-Email", "pat@example.com")
	s.render(w, r, "signedout", nil)
	bare(t, "signed out", w.Body.String())
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
	if !strings.Contains(body, `<select name="team"><option value="">All teams</option>`) {
		t.Error("/tasks for pat: the bar offers no way to every team's")
	}
	// The bar alone picks the team: no second picker on the page.
	if strings.Contains(body, "Team: <a href=") {
		t.Error("/tasks for pat: team links beside the bar's picker")
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
		check(t, h, "pat@example.com", "/tasks", `<a class="button" href="#add">Add a task</a>`)
		check(t, h, "ed@example.com", "/incidents?team=search&source=written&weeks=12", `<a class="button" href="#add">Write one down</a>`)
		check(t, h, "boss@example.com", "/teams?edit=1", `<a class="button" href="#add">Add a team</a>`)
		check(t, h, "boss@example.com", "/reports", `<a class="button" href="#add">Add a report</a>`)
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
	t.Run("no team to make a report for", func(t *testing.T) {
		// An editor makes a team's report, and there is no team: the
		// report the form would send is refused.
		dest := filepath.Join(t.TempDir(), "ops")
		os.WriteFile(dest, []byte("http://127.0.0.1:1/x"), 0o600)
		s, _ := server(t, func(c *Config) {
			c.UserHeader, c.Admins, c.Editors = "X-Forwarded-Email", []string{"boss@example.com"}, []string{"ed@example.com"}
			c.ReportDests = map[string]string{"ops": dest}
		})
		h := s.Handler()
		check(t, h, "ed@example.com", "/reports", disabled("Add a report", "No team yet: an editor makes a team&#39;s report"))
		check(t, h, "boss@example.com", "/reports", `<a class="button" href="#add">Add a report</a>`)
		if code, _, _ := ui(t, h, http.MethodPost, "/reports", "ed@example.com", "same-origin",
			url.Values{"name": {"r"}, "schedule": {"daily@09:00"}, "dests": {"ops"}, "sections": {"summary"}}); code != http.StatusForbidden {
			t.Errorf("an editor's report, with no team: %d", code)
		}
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
		if !slices.Equal(crumbs, []string{"See", "Rules", "Errors"}) || !strings.Contains(body, `<span aria-current="page"><bdi>Errors</bdi></span>`) {
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

// Every text a template gives t, mainAction or emptyOf as it is written,
// and every text of the sidebar, the breadcrumbs and the actions' reasons,
// has a translation: some are shown only in states the pages rendered in
// TestPagesInEachLanguage aren't in.
func TestFrameTextsTranslated(t *testing.T) {
	texts := map[string]bool{}
	tpls, err := files.ReadDir("templates")
	if err != nil {
		t.Fatal(err)
	}
	lit := regexp.MustCompile(`(?:\bt|mainAction \$|emptyOf "\w+") "((?:[^"\\]|\\.)+)"`)
	for _, f := range tpls {
		b, err := files.ReadFile("templates/" + f.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range lit.FindAllStringSubmatch(string(b), -1) {
			texts[m[1]] = true
		}
		for _, m := range regexp.MustCompile(`emptyOf "\w+" "(?:[^"]*)" "((?:[^"\\]|\\.)+)"`).FindAllStringSubmatch(string(b), -1) {
			texts[m[1]] = true
		}
	}
	for _, g := range navLayout {
		texts[g.name] = true
		for _, it := range g.items {
			texts[it.name] = true
		}
	}
	for _, c := range crumbsOf {
		texts[c.Group], texts[c.Name] = true, true
	}
	for _, x := range []string{"Team and period", "Team", "Period", "Admins only", "Editors only", "%d open", "%d to approve", "Nowhere to send a report yet", "No team yet: an editor makes a team's report",
		"Read-only: lapilli serve was started with --read-only", "Read-only: no one is given a role here", "Read-only: no one is signed in"} {
		texts[x] = true
	}
	delete(texts, "")
	for x := range texts {
		for lang, c := range catalogs {
			if _, ok := c[x]; !ok {
				t.Errorf("%s has no translation for %q", lang, x)
			}
		}
	}
	if len(texts) < 300 {
		t.Errorf("only %d texts found", len(texts))
	}
}

// TestBarNamed: the bar's form is named for what it chooses: the team, the
// period, or both.
func TestBarNamed(t *testing.T) {
	s := pagesServer(t)
	alone, _ := server(t, nil)
	for _, c := range []struct {
		h          http.Handler
		path, want string
	}{
		{s.Handler(), "/", "Team"}, {s.Handler(), "/tasks", "Team"}, {s.Handler(), "/rules", "Team and period"},
		{s.Handler(), "/incidents", "Team and period"}, {alone.Handler(), "/rules", "Period"},
	} {
		if _, body, _ := get(t, c.h, c.path); !strings.Contains(body, `class="ctx" aria-label="`+c.want+`">`) {
			t.Errorf("%s: the bar's form isn't %q", c.path, c.want)
		}
	}
}
