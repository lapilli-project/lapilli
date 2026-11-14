package serve

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
)

// The fourth phase of the web UI (docs/design/ui.md) beyond the
// proposals: the tasks' facet, the quiet windows' preview, the Manage
// page, and what a reader may not do, shown disabled with why.

// tasksFor is the tasks list for a query.
func tasksFor(t *testing.T, s *Server, query string) tasksView {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/tasks?"+query, nil)
	_, vr := s.withContext(r, "tasks")
	v, err := s.tasksPage(vr)
	if err != nil {
		t.Fatal(err)
	}
	return v.(tasksView)
}

// TestTasksFacet: the tasks' status is a facet, each value counted for the
// team: the open ones (open and being done) unless asked; done, dropped,
// all, and an older link's status as it was (doing, or one there is no
// such of, the open ones); the team kept in the form.
func TestTasksFacet(t *testing.T) {
	s, _ := server(t, nil)
	ctx := context.Background()
	pay, err := s.l.PutTeam(ctx, ledger.Team{Name: "payments", Rules: "^P"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	for i, st := range []string{"open", "doing", "done", "dropped", "open", "done", "done"} {
		task := ledger.Task{Title: "t", Kind: "tune", Status: st}
		if i < 4 {
			task.TeamID = pay
		}
		if _, err := s.l.PutTask(ctx, task, "ana"); err != nil {
			t.Fatal(err)
		}
	}
	count := func(v tasksView) map[string]int {
		out := map[string]int{}
		for _, o := range v.Pick.Facets[0].Options {
			out[o.Value] = o.Count
		}
		return out
	}
	for _, c := range []struct {
		q            string
		shown, total int
		counts       map[string]int
	}{
		{"", 3, 7, map[string]int{"open": 3, "done": 3, "dropped": 1}},
		{"status=all", 7, 7, nil},
		{"status=done", 3, 7, nil},
		{"status=done&status=dropped", 4, 7, nil},
		// An older link's doing, or a status there is no such of, is the
		// open ones, as it was.
		{"status=doing", 3, 7, map[string]int{"open": 3, "done": 3, "dropped": 1}},
		{"status=bogus", 3, 7, nil},
		{"status=bogus&status=done", 3, 7, nil},
		{"team=payments", 2, 4, map[string]int{"open": 2, "done": 1, "dropped": 1}},
		{"team=payments&status=all", 4, 4, nil},
	} {
		v := tasksFor(t, s, c.q)
		if len(v.Lines) != c.shown || v.Total != c.total {
			t.Errorf("%q: %d of %d, want %d of %d", c.q, len(v.Lines), v.Total, c.shown, c.total)
		}
		if got := count(v); c.counts != nil && len(got) != len(c.counts) {
			t.Errorf("%q: counts %v, want %v", c.q, got, c.counts)
		} else {
			for k, n := range c.counts {
				if got[k] != n {
					t.Errorf("%q: %s counts %d, want %d", c.q, k, got[k], n)
				}
			}
		}
	}
	v := tasksFor(t, s, "team=payments&status=done")
	if len(v.Pick.Chips) != 2 || v.Pick.Chips[1].Href != "/tasks?status=all&team=payments" || v.Pick.Clear != "/tasks?status=all&team=payments" {
		t.Errorf("chips %+v, clear %q", v.Pick.Chips, v.Pick.Clear)
	}
	_, body, _ := get(t, s.Handler(), "/tasks?team=payments")
	if !strings.Contains(body, `<form method="get" action="/tasks" class="facets pick" aria-label="Filters"><input type="hidden" name="team" value="payments"><input type="hidden" name="status" value="all">`) ||
		!strings.Contains(body, `<input type="checkbox" name="status" value="open" checked> <span>open</span> <span class="n">2</span>`) {
		t.Error("the form doesn't keep the team, or count the open ones")
	}
}

// TestFixTextsTranslated: every text the proposals' pages, the tasks'
// facet and the actions shown disabled take from the code has a
// translation: some are shown only in states the pages rendered elsewhere
// aren't in.
func TestFixTextsTranslated(t *testing.T) {
	texts := map[string]bool{}
	for st := range stateGroups {
		b := badgeOf(st)
		texts[b.Word] = true
		if b.Detail != "" {
			texts[b.Detail] = true
		}
	}
	d := time.Minute
	for _, ev := range []evidence{{}, {Edit: &rules.Edit{For: &d, From: "1", To: "2", Annotation: "summary"}}, {Edit: &rules.Edit{Annotation: "summary", Unset: true}}} {
		for _, k := range []string{ledger.KindFor, ledger.KindThreshold, ledger.KindDisable, ledger.KindDelete, ledger.KindSays, ledger.KindUndo, "other"} {
			for _, known := range []bool{false, true} {
				texts[headOf(ledger.Proposal{Kind: k, Rule: "r"}, ev, time.Minute, known).Format] = true
			}
		}
	}
	for _, r := range btTableOf(&backtest{}).Rows {
		texts[r.Label] = true
	}
	for _, f := range append(slices.Clone(proposalFacets), taskFacets...) {
		texts[f.name] = true
		for _, l := range f.labels {
			texts[l] = true
		}
	}
	for _, ls := range diffLinks(diffOpts{}, func(string, string) string { return "" }) {
		for _, l := range ls {
			texts[l.Label] = true
		}
	}
	for _, x := range []string{"Now", "As proposed", "As the change left it", "As it was before", "The version before", "This version",
		"I have read the backtest", "I have read the change and its evidence", "Approvers only", "Editors only", "Admins only",
		"Changes are off: lapilli serve was started without --changes", "For a person to make where the rule is kept: nothing to approve here",
		"You proposed this: someone else approves it.", "You approved it: it waits for another approver.", "Ask to undo it", "Drop it", "Close it",
		"Make it a task", "Approve", "Pause changes", "Resume changes", "Lapilli", "a change recipe", "No proposals yet.", "No proposal matches these filters.",
		"A rule's page proposes a change to it, with its backtest; Lapilli proposes of itself with --changes, after the weekly digest."} {
		texts[x] = true
	}
	for x := range texts {
		for lang, c := range catalogs {
			if _, ok := c[x]; !ok {
				t.Errorf("%s has no translation for %q", lang, x)
			}
		}
	}
}

// windowForm is a quiet window's form, as a browser sends it.
func windowForm(title, start, end string, more ...string) url.Values {
	f := url.Values{"title": {title}, "start": {start}, "end": {end}, "every": {""}, "on": {""}, "until": {""}, "team": {"0"}, "rules": {""}, "labels": {""}}
	for i := 0; i+1 < len(more); i += 2 {
		f.Set(more[i], more[i+1])
	}
	return f
}

// TestWindowPreview: Preview, on the same POST as saving, writes nothing,
// and shows the form as typed with what the window would do: the rules it
// applies to, the firings of the last four weeks it would have excused,
// as many as saving it excuses, and when it falls in the next four weeks.
// It is checked as saving is: the role, the team, the same site; what was
// typed comes back escaped, and a window that couldn't be kept is refused.
func TestWindowPreview(t *testing.T) {
	s, pay, search := rolesServer(t, nil)
	h := s.Handler()
	ctx := context.Background()
	from, to := now.Add(-27*24*time.Hour).In(time.UTC).Format("2006-01-02T15:04"), now.Add(-time.Minute).In(time.UTC).Format("2006-01-02T15:04")
	f := windowForm("Checkout <b>deploy</b>", from, to, "rules", "^Err", "labels", "service=search", "preview", "1")
	gen := s.l.Gen()
	code, body := as(t, h, http.MethodPost, "/windows", "ed@example.com", "", f)
	if code != http.StatusOK {
		t.Fatalf("preview: %d\n%s", code, body)
	}
	if s.l.Gen() != gen {
		t.Error("a preview wrote to the ledger")
	}
	if ws, _ := s.l.Windows(ctx, now.Add(-60*24*time.Hour), now.Add(60*24*time.Hour)); len(ws) != 0 {
		t.Errorf("a preview kept a window: %+v", ws)
	}
	for _, want := range []string{`<section class="card preview" id="preview" aria-labelledby="preview-h">`,
		`<td><a href="/rules/Errors"><bdi>Errors</bdi></a></td><td class="n">20</td>`,
		`it would have excused 20 firings that caught nothing; 0 of them are excused already.`,
		`It doesn&#39;t fall in the next four weeks.`,
		// The form as typed, escaped, and its buttons.
		`<input name="title" required maxlength="200" value="Checkout &lt;b&gt;deploy&lt;/b&gt;"`, `value="` + from + `"`, `value="^Err"`, `value="service=search"`,
		`<button name="preview" value="1" formaction="/windows#preview" class="quiet-button">Preview</button> <button>Add it</button>`} {
		if !strings.Contains(body, want) {
			t.Errorf("no %s", want)
		}
	}
	if strings.Contains(body, "<b>deploy") {
		t.Error("the title is said back raw")
	}
	// A firing that caught something isn't excused, whatever matches it:
	// of Errors' 21 over the weeks, one caught INC-1.
	if _, body := as(t, h, http.MethodPost, "/windows", "ed@example.com", "", windowForm("all", from, to, "preview", "1")); !strings.Contains(body,
		`it would have excused 20 firings that caught nothing`) {
		t.Error("a firing that caught something is counted excused")
	}
	// Saved as previewed, it excuses as many.
	f.Del("preview")
	if code, _ := as(t, h, http.MethodPost, "/windows", "ed@example.com", "", f); code != http.StatusSeeOther {
		t.Fatalf("saving: %d", code)
	}
	n, err := s.l.CountFirings(ctx, now.Add(-28*24*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	fs, _ := s.l.Firings(ctx, ledger.FiringQuery{From: now.Add(-28 * 24 * time.Hour), To: now, Excused: true, Limit: n + 1})
	if len(fs) != 20 {
		t.Errorf("saved, it excused %d, previewed 20", len(fs))
	}
	// Every day: its repeats in the next four weeks; changing one shows
	// its own page, the window as it was kept.
	daily := windowForm("Nightly", now.Add(time.Hour).In(time.UTC).Format("2006-01-02T15:04"), now.Add(2*time.Hour).In(time.UTC).Format("2006-01-02T15:04"),
		"every", "day", "id", "1", "preview", "1")
	code, body = as(t, h, http.MethodPost, "/windows", "ed@example.com", "", daily)
	if code != http.StatusOK || strings.Count(body, " → ") < 28 || !strings.Contains(body, `<input type="hidden" name="id" value="1">`) ||
		!strings.Contains(body, "<h1>Checkout &lt;b&gt;deploy&lt;/b&gt;</h1>") || !strings.Contains(body, `<button>Save</button>`) {
		t.Errorf("a daily window's preview: %d, %d repeats", code, strings.Count(body, " → "))
	}
	if w, _ := s.l.WindowByID(ctx, 1); w.Title != "Checkout <b>deploy</b>" || w.Every != "" {
		t.Errorf("a preview changed the window: %+v", w)
	}
	// Checked as saving is.
	if _, err := s.l.PutGrant(ctx, ledger.Grant{Who: "user:pat@example.com", TeamID: pay}, "boss"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		who, site string
		form      url.Values
		want      int
	}{
		{"vic@example.com", "same-origin", f, http.StatusForbidden},
		{"", "same-origin", f, http.StatusForbidden},
		{"pat@example.com", "same-origin", windowForm("w", from, to, "team", fmt.Sprint(search), "preview", "1"), http.StatusForbidden},
		{"pat@example.com", "same-origin", windowForm("w", from, to, "preview", "1"), http.StatusForbidden},
		{"pat@example.com", "same-origin", windowForm("w", from, to, "team", fmt.Sprint(pay), "id", "1", "preview", "1"), http.StatusForbidden},
		{"pat@example.com", "same-origin", windowForm("w", from, to, "team", fmt.Sprint(pay), "preview", "1"), http.StatusOK},
		{"ed@example.com", "same-origin", windowForm("w", to, from, "preview", "1"), http.StatusBadRequest},
		{"ed@example.com", "same-origin", windowForm("w", from, to, "rules", "(", "preview", "1"), http.StatusBadRequest},
	} {
		r := httptest.NewRequest(http.MethodPost, "/windows", strings.NewReader(c.form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Sec-Fetch-Site", c.site)
		if c.who != "" {
			r.Header.Set("X-Forwarded-Email", c.who)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s, %v: %d, want %d", c.who, c.form, w.Code, c.want)
		}
	}
	// From another site, refused before anything is read.
	r := httptest.NewRequest(http.MethodPost, "/windows", strings.NewReader(f.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.Header.Set("X-Forwarded-Email", "ed@example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("from another site: %d", w.Code)
	}
}

// a11yPreviews are a quiet window's preview, to add one and to change one,
// as GET pages for axe (test/a11y/axe.sh), which reads pages by their
// URLs: each answers what the form's Preview would.
func a11yPreviews(mux *http.ServeMux, s *Server) []string {
	h := s.Handler()
	for path, form := range map[string]url.Values{
		"/a11y/window-preview":      windowForm("Checkout deploy", "2026-11-02T10:00", "2026-11-02T11:00", "every", "day", "rules", "^Err", "preview", "1"),
		"/a11y/window-preview-edit": windowForm("Nightly", "2026-11-02T01:00", "2026-11-02T02:00", "id", "1", "preview", "1"),
	} {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			p := httptest.NewRequest(http.MethodPost, "/windows", strings.NewReader(form.Encode()))
			p.Header = r.Header.Clone()
			p.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			p.Header.Set("Sec-Fetch-Site", "same-origin")
			p.Host = r.Host
			h.ServeHTTP(w, p)
		})
	}
	return []string{"/a11y/window-preview", "/a11y/window-preview-edit"}
}

// TestPreviewAccessible: a quiet window's preview, to add one and to
// change one, passes the pages' checks in each language.
func TestPreviewAccessible(t *testing.T) {
	s, _ := a11yServer(t)
	mux := http.NewServeMux()
	pageA11y(t, mux, "", a11yPreviews(mux, s)...)
}

// TestManagePage: /manage has a card for each of the Manage group's pages
// and the reports, each with what it is for, Roles for admins alone; the
// sidebar's Manage links to it, with the page context; and the Manage
// pages' breadcrumbs start with it.
func TestManagePage(t *testing.T) {
	s, _, _ := rolesServer(t, nil)
	h := s.Handler()
	cards := func(who string) []string {
		_, body := as(t, h, http.MethodGet, "/manage", who, "", nil)
		var out []string
		for _, m := range regexp.MustCompile(`<li class="card">\s*<h2><a href="([^"]+)">([^<]+)</a></h2>\s*<p>([^<]+)</p>`).FindAllStringSubmatch(body, -1) {
			out = append(out, m[1]+" "+m[2])
		}
		return out
	}
	if got, want := cards("boss@example.com"), []string{"/teams Teams", "/roles Roles", "/settings Settings", "/runs Runs", "/reports Reports"}; !slices.Equal(got, want) {
		t.Errorf("an admin's cards: %q, want %q", got, want)
	}
	if got, want := cards("ed@example.com"), []string{"/teams Teams", "/settings Settings", "/runs Runs", "/reports Reports"}; !slices.Equal(got, want) {
		t.Errorf("an editor's cards: %q, want %q", got, want)
	}
	_, body := as(t, h, http.MethodGet, "/manage?team=payments", "ed@example.com", "", nil)
	if !strings.Contains(body, `<p class="nav-group" id="side-manage"><a href="/manage?team=payments" aria-current="page">Manage</a></p>`) ||
		!strings.Contains(body, `<a href="/teams?team=payments">Teams</a>`) || !strings.Contains(body, "2 teams") {
		t.Error("the sidebar's Manage doesn't link to it, current, or the cards don't carry the team")
	}
	if !strings.Contains(body, `<ol><li><span aria-current="page">Manage</span></li></ol>`) {
		t.Error("the Manage page's breadcrumb")
	}
	for _, p := range []string{"/teams", "/settings", "/runs"} {
		_, body := as(t, h, http.MethodGet, p, "boss@example.com", "", nil)
		if !strings.Contains(body, `<ol><li><a href="/manage">Manage</a></li>`) || !strings.Contains(body, `<p class="nav-group" id="side-manage"><a href="/manage">Manage</a></p>`) {
			t.Errorf("%s: its breadcrumb doesn't start with a link to Manage", p)
		}
	}
	if _, body := as(t, h, http.MethodGet, "/rules", "boss@example.com", "", nil); !strings.Contains(body, `<ol><li>See</li>`) || !strings.Contains(body, `<p class="nav-group" id="side-see">See</p>`) {
		t.Error("a group without a page of its own is a link")
	}
}

// TestOffTextsTranslated: every action a template shows disabled, with
// offOf or offTeam, is named in each language.
func TestOffTextsTranslated(t *testing.T) {
	tpls, err := files.ReadDir("templates")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range tpls {
		b, err := files.ReadFile("templates/" + f.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range regexp.MustCompile(`\boff(?:Of|Team) \$ "((?:[^"\\]|\\.)+)"`).FindAllStringSubmatch(string(b), -1) {
			n++
			for lang, c := range catalogs {
				if _, ok := c[m[1]]; !ok {
					t.Errorf("%s has no translation for %q", lang, m[1])
				}
			}
		}
	}
	if n < 15 {
		t.Errorf("only %d disabled actions found", n)
	}
}

// TestFormsDisabledWithWhy: where a page's form is for those who may change
// something, a reader who may not sees one button, disabled, naming what it
// does, and why, and no form: a quiet window's, a task's, a team's, the
// settings', a report's, an incident's, and a rule's (what to do first,
// its note, what it says, proposing a change, a task about it).
func TestFormsDisabledWithWhy(t *testing.T) {
	t.Run("read-only", func(t *testing.T) {
		s := pagesServer(t)
		s.cfg.ReadOnly = true
		h := s.Handler()
		ro := "Read-only: lapilli serve was started with --read-only"
		for _, c := range []struct {
			path   string
			offs   [][2]string // label, id
			forms  []string
			status int
		}{
			{"/windows/1", [][2]string{{"Change it", "window-why"}}, []string{`action="/windows"`, `/windows/1/delete`}, 200},
			{"/tasks/1", [][2]string{{"Change it", "task-why"}}, []string{`/tasks/1/delete`, `name="only"`}, 200},
			{"/teams?edit=1", [][2]string{{"Change it", "team-why"}}, []string{`action="/teams"`}, 200},
			{"/settings", [][2]string{{"Save, and judge again", "settings-why"}}, []string{`<form method="post" action="/settings">`}, 200},
			{"/reports/1", [][2]string{{"Change it", "report-why"}}, nil, 200},
			{"/incidents/impact-20261103T0600Z", [][2]string{{"Confirm or dismiss it", "incident-why"}, {"Add a task about this incident", "incident-task-why"}}, []string{`/decide`}, 200},
			{"/rules/Errors", [][2]string{{"Change it", "note-why"}}, []string{`action="/notes"`}, 200},
			{"/rules/Errors?tab=proposals", [][2]string{{"Propose a change", "propose-why"}}, []string{`action="/proposals"`}, 200},
			{"/rules/Errors?tab=activity", [][2]string{{"Add a task about this rule", "activity-task-why"}}, nil, 200},
		} {
			code, body, _ := get(t, h, c.path)
			if code != c.status {
				t.Errorf("%s: %d", c.path, code)
				continue
			}
			for _, o := range c.offs {
				if !strings.Contains(body, offButton(o[0], o[1], ro)) {
					t.Errorf("%s: no %q disabled with why", c.path, o[0])
				}
			}
			for _, f := range c.forms {
				if strings.Contains(body, f) {
					t.Errorf("%s: a form, %s", c.path, f)
				}
			}
		}
	})
	t.Run("roles", func(t *testing.T) {
		s, pay, search := rolesServer(t, nil)
		ctx := context.Background()
		if _, err := s.l.PutGrant(ctx, ledger.Grant{Who: "user:pat@example.com", TeamID: pay}, "boss"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.l.PutGrant(ctx, ledger.Grant{Who: "user:sam@example.com", TeamID: search}, "boss"); err != nil {
			t.Fatal(err)
		}
		mine, err := s.l.PutWindow(ctx, ledger.Window{Title: "pay's", Start: now, End: now.Add(time.Hour), TeamID: pay}, "boss", s.cfg.Options)
		if err != nil {
			t.Fatal(err)
		}
		theirs, err := s.l.PutWindow(ctx, ledger.Window{Title: "search's", Start: now, End: now.Add(time.Hour), TeamID: search}, "boss", s.cfg.Options)
		if err != nil {
			t.Fatal(err)
		}
		task, err := s.l.PutTask(ctx, ledger.Task{Title: "search's", Kind: "tune", TeamID: search}, "boss")
		if err != nil {
			t.Fatal(err)
		}
		h := s.Handler()
		for _, c := range []struct {
			who, path, label, id, why string
		}{
			{"vic@example.com", "/windows/" + itoa(mine), "Change it", "window-why", "Editors only"},
			{"pat@example.com", "/windows/" + itoa(theirs), "Change it", "window-why", "Only <bdi>search</bdi>&#39;s editors"},
			{"pat@example.com", "/tasks/" + itoa(task), "Change it", "task-why", "Only <bdi>search</bdi>&#39;s editors"},
			{"", "/tasks/" + itoa(task), "Change it", "task-why", "Read-only: no one is signed in"},
			{"ed@example.com", "/teams?edit=" + itoa(pay), "Change it", "team-why", "Admins only"},
			{"ed@example.com", "/settings", "Save, and judge again", "settings-why", "Admins only"},
			{"sam@example.com", "/rules/Errors?tab=activity", "Add a task about this rule", "activity-task-why", "Only <bdi>payments</bdi>&#39;s editors"},
			{"sam@example.com", "/rules/Errors", "Change it", "note-why", "Only <bdi>payments</bdi>&#39;s editors"},
		} {
			_, body := as(t, h, http.MethodGet, c.path, c.who, "", nil)
			if !strings.Contains(body, offButton(c.label, c.id, c.why)) {
				t.Errorf("%s as %q: no %q disabled with %q", c.path, c.who, c.label, c.why)
			}
		}
		// Whoever may, has the form, and no button disabled.
		if _, body := as(t, h, http.MethodGet, "/windows/"+itoa(mine), "pat@example.com", "", nil); !strings.Contains(body, `action="/windows"`) || strings.Contains(body, `id="window-why"`) {
			t.Error("pat's own team's window: no form")
		}
		// And the server refuses whoever may not, as before.
		if code, _ := as(t, h, http.MethodPost, "/windows", "pat@example.com", "", windowForm("x", "2026-11-01T10:00", "2026-11-01T11:00", "id", itoa(theirs), "team", itoa(search))); code != http.StatusForbidden {
			t.Errorf("pat changing search's window: %d", code)
		}
	})
}

// The pages and states phase 4 adds, among those every page test renders
// in each language and checks for accessibility; a proposal's are where a
// server has one.
func init() {
	pagePaths = append(pagePaths, "/manage", "/proposals/1?diff=split&context=all", "/proposals/1?context=all",
		"/proposals?state=all&kind=for&by=person", "/tasks?status=done&status=dropped")
}

// TestDisabledAccessible: the pages with the actions a reader may not take
// shown disabled, and the Manage page, pass the pages' checks in each
// language.
func TestDisabledAccessible(t *testing.T) {
	ro, _ := a11yServer(t)
	ro.cfg.ReadOnly = true
	pageA11y(t, ro.Handler(), "", "/incidents/impact-20261103T0600Z", "/teams?edit=1", "/rules/Errors?tab=proposals", "/rules/Errors?tab=activity", "/manage")
}
