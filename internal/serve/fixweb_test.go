package serve

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
// all, and an older link's status as it was; the team kept in the form.
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
		{"status=doing", 1, 7, map[string]int{"open": 3, "done": 3, "dropped": 1, "doing": 1}},
		{"status=bogus", 7, 7, nil},
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
			texts[headOf(ledger.Proposal{Kind: k, Rule: "r"}, ev).Format] = true
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
