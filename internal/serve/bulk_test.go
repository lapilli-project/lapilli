package serve

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
)

// postAs posts a form as the sign-in proxy names who, from a page on the
// site site says ("same-origin", "cross-site").
func postAs(t *testing.T, h http.Handler, path string, form url.Values, who, site string) (int, string, http.Header) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", site)
	if who != "" {
		r.Header.Set("X-Forwarded-Email", who)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.String(), w.Header()
}

// getAs asks for a page as the sign-in proxy names who.
func getAs(t *testing.T, h http.Handler, path, who string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if who != "" {
		r.Header.Set("X-Forwarded-Email", who)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

// bulkServer is rolesServer with search's rules besides payments' Errors,
// one no team owns, and pat an editor of payments alone.
func bulkServer(t *testing.T, change func(*Config)) (*Server, int64, int64) {
	t.Helper()
	ctx := context.Background()
	s, pay, search := rolesServer(t, change)
	os.WriteFile(filepath.Join(s.cfg.RulesPath, "search.yaml"), []byte("groups:\n  - name: search\n    rules:\n"+
		"      - {alert: SearchLatency, expr: latency > 1}\n      - {alert: SearchErrors, expr: errors > 1}\n      - {alert: Nobody, expr: up == 0}\n"), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutGrant(ctx, ledger.Grant{Who: "user:pat@example.com", TeamID: pay}, "boss"); err != nil {
		t.Fatal(err)
	}
	return s, pay, search
}

// worked is what the ledger keeps of what people did: each rule's note and
// its tasks, and the changes log's lines, by whom, in order, with no time
// or id that two ledgers would differ by.
func worked(t *testing.T, s *Server) (notes map[string]ledger.Note, tasks []string, log []string) {
	t.Helper()
	ctx := context.Background()
	ns, err := s.l.Notes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	notes = map[string]ledger.Note{}
	for k, n := range ns {
		n.Updated = time.Time{} // when, which two ledgers differ by
		notes[k] = n
	}
	ts, err := s.l.Tasks(ctx, ledger.TaskQuery{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range ts {
		tasks = append(tasks, fmt.Sprintf("%s|%s|%s|%d|%s|%s", x.Title, x.Kind, x.Rule, x.TeamID, x.Status, x.CreatedBy))
	}
	slices.Sort(tasks)
	cs, err := s.l.Changes(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		what := c.What
		if strings.HasPrefix(what, "task ") {
			what = "task " + strings.SplitN(what, " ", 3)[2] // "task 12 made": the id is the ledger's
		}
		log = append(log, c.By+"|"+what)
	}
	slices.Reverse(log)
	return notes, tasks, log
}

// TestBulkRules: each bulk write on the rules list does, rule by rule, what
// the same write for one rule does: the same checks, for each role and
// team, the same notes and tasks, the same lines in the changes log; and
// what it refuses, it counts.
func TestBulkRules(t *testing.T) {
	const ed, pat, boss, vi = "ed@example.com", "pat@example.com", "boss@example.com", "vi@example.com"
	picks := []string{"Errors", "SearchLatency", "Nobody"}
	base, _, _ := bulkServer(t, nil)
	for _, c := range []struct {
		who  string
		form url.Values
		// one is what the same, one rule at a time, posts: a path and a
		// form for each rule.
		one  func(rule string, pay, search int64) (string, url.Values)
		want string // the result's query, as the list is sent back with
	}{
		{ed, url.Values{"do": {"task"}}, func(rule string, _, _ int64) (string, url.Values) {
			return "/tasks", url.Values{"title": {"Tune " + rule}, "kind": {"tune"}, "rule": {rule}, "team": {"0"}}
		}, "bulk=task&done=3"},
		{pat, url.Values{"do": {"task"}}, func(rule string, _, _ int64) (string, url.Values) {
			return "/tasks", url.Values{"title": {"Tune " + rule}, "kind": {"tune"}, "rule": {rule}, "team": {"0"}}
		}, "bulk=task&done=1&theirs=2"},
		{pat, url.Values{"do": {"status"}, "status": {"retire"}}, func(rule string, _, _ int64) (string, url.Values) {
			return "/notes", url.Values{"rule": {rule}, "team": {"0"}, "status": {"retire"}}
		}, "bulk=status&done=1&theirs=2"},
		{boss, url.Values{"do": {"status"}, "status": {"keep"}}, func(rule string, _, _ int64) (string, url.Values) {
			return "/notes", url.Values{"rule": {rule}, "team": {"0"}, "status": {"keep"}}
		}, "bulk=status&done=3"},
		{pat, url.Values{"do": {"team"}, "team": {"SEARCH"}}, func(rule string, _, search int64) (string, url.Values) {
			return "/notes", url.Values{"rule": {rule}, "team": {fmt.Sprint(search)}}
		}, "bulk=team&theirs=3"},
		{ed, url.Values{"do": {"team"}, "team": {"SEARCH"}}, func(rule string, _, search int64) (string, url.Values) {
			return "/notes", url.Values{"rule": {rule}, "team": {fmt.Sprint(search)}}
		}, "bulk=team&done=3"},
	} {
		name := c.who + " " + c.form.Get("do")
		bulk, pay, search := bulkServer(t, nil)
		one, _, _ := bulkServer(t, nil)
		form := url.Values{}
		for k, v := range c.form {
			form[k] = slices.Clone(v)
		}
		if form.Get("team") == "SEARCH" {
			form.Set("team", fmt.Sprint(search))
		}
		form["rule"] = picks
		form.Set("back", "/rules?perf=noisy&sort=refired&team=payments&weeks=12")
		code, body, hd := postAs(t, bulk.Handler(), "/rules/bulk", form, c.who, "same-origin")
		if code != http.StatusSeeOther {
			t.Fatalf("%s: %d %s", name, code, body)
		}
		want := "/rules?" + c.want + "&perf=noisy&sort=refired&team=payments&weeks=12"
		if q, _ := url.ParseQuery(strings.TrimPrefix(hd.Get("Location"), "/rules?")); "/rules?"+q.Encode() != canonicalURL(t, want) {
			t.Errorf("%s: back to %q, not %q", name, hd.Get("Location"), want)
		}
		h := one.Handler()
		for _, rule := range picks {
			path, f := c.one(rule, pay, search)
			if code, _, _ := postAs(t, h, path, f, c.who, "same-origin"); code != http.StatusSeeOther && code != http.StatusForbidden {
				t.Fatalf("%s %s one at a time: %d", name, rule, code)
			}
		}
		bn, bt, bl := worked(t, bulk)
		on, ot, ol := worked(t, one)
		// Something was done, when the result says so.
		_, _, was := worked(t, base)
		if done := strings.Contains(c.want, "done="); done != (len(bl) > len(was)) {
			t.Errorf("%s: the changes log has %d lines, %d before", name, len(bl), len(was))
		}
		if !maps.Equal(bn, on) {
			t.Errorf("%s: notes\n%v\none at a time\n%v", name, bn, on)
		}
		if !slices.Equal(bt, ot) {
			t.Errorf("%s: tasks\n%v\none at a time\n%v", name, bt, ot)
		}
		if !slices.Equal(bl, ol) {
			t.Errorf("%s: the changes log\n%v\none at a time\n%v", name, bl, ol)
		}
	}
}

// canonicalURL is a URL with its query in the order url.Values encodes.
func canonicalURL(t *testing.T, u string) string {
	t.Helper()
	p, q, _ := strings.Cut(u, "?")
	v, err := url.ParseQuery(q)
	if err != nil {
		t.Fatal(err)
	}
	return p + "?" + v.Encode()
}

// TestBulkRulesRefused: a bulk write is refused whole to whoever may write
// nothing, from another site, past 100 rules, or for what it can't do; and
// it goes back only to the rules list, with what it did said once.
func TestBulkRulesRefused(t *testing.T) {
	const ed = "ed@example.com"
	s, _, _ := bulkServer(t, nil)
	h := s.Handler()
	ctx := context.Background()
	tasks := func() int {
		ts, err := s.l.Tasks(ctx, ledger.TaskQuery{Limit: 500})
		if err != nil {
			t.Fatal(err)
		}
		return len(ts)
	}
	many := make([]string, 101)
	for i := range many {
		many[i] = fmt.Sprintf("R%03d", i)
	}
	for _, c := range []struct {
		what, who, site string
		form            url.Values
		want            int
	}{
		{"a viewer", "vi@example.com", "same-origin", url.Values{"do": {"task"}, "rule": {"Errors"}}, http.StatusForbidden},
		{"no one signed in", "", "same-origin", url.Values{"do": {"task"}, "rule": {"Errors"}}, http.StatusForbidden},
		{"from another site", ed, "cross-site", url.Values{"do": {"task"}, "rule": {"Errors"}}, http.StatusForbidden},
		{"101 rules", ed, "same-origin", url.Values{"do": {"task"}, "rule": many}, http.StatusBadRequest},
		{"nothing to do", ed, "same-origin", url.Values{"do": {"delete"}, "rule": {"Errors"}}, http.StatusBadRequest},
		{"a team no one has", ed, "same-origin", url.Values{"do": {"team"}, "team": {"999"}, "rule": {"Errors"}}, http.StatusBadRequest},
		{"a status no one has", ed, "same-origin", url.Values{"do": {"status"}, "status": {"gone"}, "rule": {"Errors"}}, http.StatusBadRequest},
	} {
		if code, body, _ := postAs(t, h, "/rules/bulk", c.form, c.who, c.site); code != c.want {
			t.Errorf("%s: %d, not %d: %s", c.what, code, c.want, body)
		}
		if n := tasks(); n != 0 {
			t.Fatalf("%s: %d tasks made", c.what, n)
		}
	}
	ro, _, _ := bulkServer(t, func(c *Config) { c.ReadOnly = true })
	if code, _, _ := postAs(t, ro.Handler(), "/rules/bulk", url.Values{"do": {"task"}, "rule": {"Errors"}}, "boss@example.com", "same-origin"); code != http.StatusForbidden {
		t.Errorf("read-only: %d", code)
	}

	// A hundred, each once however often it is picked.
	hundred := append(slices.Clone(many[:100]), many[:50]...)
	if code, _, hd := postAs(t, h, "/rules/bulk", url.Values{"do": {"task"}, "rule": hundred, "back": {"/rules"}}, ed, "same-origin"); code != http.StatusSeeOther ||
		hd.Get("Location") != "/rules?bulk=task&done=100" || tasks() != 100 {
		t.Errorf("a hundred: %d to %q, %d tasks", code, hd.Get("Location"), tasks())
	}

	// Back to the rules list alone; and to it, with nothing picked.
	for back, want := range map[string]string{
		"//evil.example/rules": "/rules?bulk=status", "https://evil.example/rules?x=1": "/rules?bulk=status", "/tasks?team=payments": "/rules?bulk=status",
		"/rules/../tasks": "/rules?bulk=status", "/rules?q=err&weeks=1": "/rules?bulk=status&q=err&weeks=1", "": "/rules?bulk=status",
		"/rules?bulk=task&done=7&refused=3": "/rules?bulk=status",
	} {
		if _, _, hd := postAs(t, h, "/rules/bulk", url.Values{"do": {"status"}, "back": {back}}, ed, "same-origin"); hd.Get("Location") != want {
			t.Errorf("back %q: to %q, not %q", back, hd.Get("Location"), want)
		}
	}
}

// TestBulkResultShown: the rules list says once what a bulk write did,
// read within bounds, and its links don't carry it on.
func TestBulkResultShown(t *testing.T) {
	s, _, _ := bulkServer(t, nil)
	h := s.Handler()
	for q, want := range map[string]string{
		"bulk=task&done=3&theirs=2":            "3 done; 2 not your team&#39;s</p>",
		"bulk=status&done=1&refused=1":         "1 done; 1 refused</p>",
		"bulk=real&done=2&already=1":           "2 done; 1 had caught something already</p>",
		"bulk=task":                            "Nothing was picked: tick the ones to change first.</p>",
		"bulk=task&done=101":                   "",
		"bulk=task&done=60&theirs=60":          "",
		"bulk=task&done=-1":                    "",
		"bulk=task&done=x":                     "",
		"bulk=delete&done=3":                   "",
		"done=3":                               "",
		"bulk=task&done=3&done=%3Cscript%3E50": "3 done</p>",
	} {
		code, body := getAs(t, h, "/rules?"+q, "ed@example.com")
		if code != http.StatusOK {
			t.Fatalf("%s: %d", q, code)
		}
		said := strings.Contains(body, `role="status">`)
		if want == "" && said || want != "" && !strings.Contains(body, `<p class="note bulk-done" role="status">`+want) {
			t.Errorf("%s: says %v, want %q", q, said, want)
		}
		if strings.Contains(body, "done=") || strings.Contains(body, "bulk=") || strings.Contains(body, "done%3D") || strings.Contains(body, "bulk%3D") ||
			strings.Contains(body, `name="bulk"`) || strings.Contains(body, `name="done"`) {
			t.Errorf("%s: a link or a form carries the result on", q)
		}
	}
	// The language links, the page's own address query-escaped, too; and
	// a comment's form, which goes back to its page.
	for path, want := range map[string]string{
		"/rules?bulk=task&done=3&weeks=12":                      `href="/lang?to=ko&amp;back=%2Frules%3Fweeks%3D12"`,
		"/rules?done=3&bulk=status&refused=1&failed=2&theirs=1": `href="/lang?to=ja&amp;back=%2Frules"`,
		"/firings?day=2026-11-02&bulk=real&done=1&already=1":    `href="/lang?to=es&amp;back=%2Ffirings%3Fday%3D2026-11-02"`,
		"/incidents/INC-1?bulk=task&done=1":                     `<input type="hidden" name="back" value="/incidents/INC-1">`,
	} {
		_, body := getAs(t, h, path, "ed@example.com")
		if !strings.Contains(body, want) {
			t.Errorf("%s: no %s", path, want)
		}
		if strings.Contains(body, "done%3D") || strings.Contains(body, "bulk%3D") || strings.Contains(body, "bulk=") || strings.Contains(body, `name="bulk"`) ||
			strings.Contains(body, `name="done"`) {
			t.Errorf("%s: a link or a form carries the result on", path)
		}
	}
}

// TestBulkDisabled: whoever may not use the bulk bar sees it disabled, with
// why; a team's editor may make tasks and set dispositions, not teams.
func TestBulkDisabled(t *testing.T) {
	s, _, _ := bulkServer(t, nil)
	h := s.Handler()
	for who, want := range map[string][]string{
		"": {`<button name="do" value="task" disabled aria-describedby="bulk-why">`, `<span id="bulk-why" class="why">Read-only: no one is signed in</span>`,
			`aria-label="Errors" disabled>`},
		"vi@example.com": {`<button name="do" value="task" disabled aria-describedby="bulk-why">`, `<span id="bulk-why" class="why">Editors only</span>`},
		"pat@example.com": {`<button name="do" value="task">`, `<button class="quiet-button" name="do" value="status">`,
			`<button class="quiet-button" name="do" value="team" disabled aria-describedby="bulk-team-why">`, `<span id="bulk-team-why" class="why">Editors only</span>`},
		"ed@example.com": {`<button name="do" value="task">`, `<button class="quiet-button" name="do" value="team">`, `aria-label="Errors">`},
	} {
		code, body := getAs(t, h, "/rules?weeks=12&perf=ok&perf=noisy", who)
		if code != http.StatusOK {
			t.Fatalf("%q: %d", who, code)
		}
		for _, w := range want {
			if !strings.Contains(body, w) {
				t.Errorf("%q: no %s", who, w)
			}
		}
		if who == "ed@example.com" && strings.Contains(body, "disabled") {
			t.Errorf("an editor sees something disabled")
		}
	}
}

// TestBulkAccessibleReadOnly: the pages with bulk forms and disabled
// actions pass the HTML checks the other pages do when the reader may
// change nothing: the bar, the firings' buttons and the overview's
// suggestions disabled, with why.
func TestBulkAccessibleReadOnly(t *testing.T) {
	s, _ := a11yServer(t)
	s.cfg.ReadOnly = true
	pageA11y(t, s.Handler(), "", "/", "/rules", "/rules?weeks=12&bulk=task&done=1", "/firings?day=2026-11-02", "/incidents/INC-1")
}

// failOn makes the ledger fail to write a row of table whose column is
// value, as a database that refused it would: an error that is neither the
// reader's nor what was written.
func failOn(t *testing.T, s *Server, table, column, value string) {
	t.Helper()
	if s.cfg.LedgerURLFile != "" {
		ledgerExec(t, s, `CREATE FUNCTION fail_`+table+`() RETURNS trigger AS $$ BEGIN IF NEW.`+column+` = '`+value+`' THEN RAISE EXCEPTION 'boom'; END IF; RETURN NEW; END $$ LANGUAGE plpgsql`)
		ledgerExec(t, s, `CREATE TRIGGER fail_`+table+` BEFORE INSERT ON `+table+` FOR EACH ROW EXECUTE FUNCTION fail_`+table+`()`)
		return
	}
	ledgerExec(t, s, `CREATE TRIGGER fail_`+table+` BEFORE INSERT ON `+table+` WHEN NEW.`+column+` = '`+value+`' BEGIN SELECT RAISE(ABORT, 'boom'); END`)
}

// TestBulkPartial: what the ledger refuses for what it is, the single
// write answers 400, and a bulk write counts refused; a write that fails
// otherwise, it counts failed, and goes on: it ends on the list it came
// from, saying so, with the rest written.
func TestBulkPartial(t *testing.T) {
	s, ids := realServer(t)
	h := s.Handler()
	ctx := context.Background()
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	// A firing longer than an incident may be.
	keep(t, s, day.Add(-41*24*time.Hour), day.Add(-39*24*time.Hour), []firing.Firing{{Rule: "Long", Start: day.Add(-40 * 24 * time.Hour), End: day.Add(time.Hour),
		Labels: map[string]string{"service": "long"}}})
	long, err := s.l.Firings(ctx, ledger.FiringQuery{From: day.Add(-41 * 24 * time.Hour), To: day.Add(-39 * 24 * time.Hour), Limit: 5})
	if err != nil || len(long) != 1 {
		t.Fatalf("%d long firings: %v", len(long), err)
	}
	if code, body, _ := postAs(t, h, fmt.Sprintf("/firings/%d/real", long[0].ID), nil, "ana@example.com", "same-origin"); code != http.StatusBadRequest ||
		!strings.Contains(body, "an incident lasts 31 days at most") {
		t.Errorf("a firing of 40 days: %d %s", code, body)
	}
	f1, err := s.l.FiringByID(ctx, mustInt(t, ids[1]))
	if err != nil {
		t.Fatal(err)
	}
	failOn(t, s, "incidents", "title", "caught by "+f1.Rule)
	code, body, hd := postAs(t, h, "/firings/real", url.Values{"firing": {fmt.Sprint(long[0].ID), ids[0], ids[1]}, "back": {"/firings?day=2026-11-01"}}, "ana@example.com", "same-origin")
	if want := "/firings?bulk=real&day=2026-11-01&done=1&failed=1&refused=1"; code != http.StatusSeeOther || hd.Get("Location") != want {
		t.Errorf("firings: %d to %q, not %q: %s", code, hd.Get("Location"), want, body)
	}
	if f0, err := s.l.FiringByID(ctx, mustInt(t, ids[0])); err != nil || !f0.White {
		t.Errorf("the firing written down: %+v %v", f0, err)
	}

	b, _, _ := bulkServer(t, nil)
	failOn(t, b, "tasks", "rule", "SearchErrors")
	code, body, hd = postAs(t, b.Handler(), "/rules/bulk", url.Values{"do": {"task"}, "rule": {"Errors", "SearchErrors", "SearchLatency"}, "back": {"/rules"}},
		"ed@example.com", "same-origin")
	if want := "/rules?bulk=task&done=2&failed=1"; code != http.StatusSeeOther || hd.Get("Location") != want {
		t.Errorf("rules: %d to %q, not %q: %s", code, hd.Get("Location"), want, body)
	}
	if _, tasks, _ := worked(t, b); len(tasks) != 2 {
		t.Errorf("tasks %v", tasks)
	}
	if _, body := getAs(t, b.Handler(), "/rules?bulk=task&done=2&failed=1", "ed@example.com"); !strings.Contains(body, `role="status">2 done; 1 failed: try again</p>`) {
		t.Error("the list doesn't say one failed")
	}
}

func mustInt(t *testing.T, v string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestBulkTasksOnce: a rule picked with spaces around its name is the rule,
// once, and its task is titled by its name; a rule with a task to tune it
// still open has one already, counted so, and none is made; one done
// doesn't count.
func TestBulkTasksOnce(t *testing.T) {
	const ed = "ed@example.com"
	s, _, _ := bulkServer(t, nil)
	h := s.Handler()
	post := func(rules ...string) string {
		t.Helper()
		code, body, hd := postAs(t, h, "/rules/bulk", url.Values{"do": {"task"}, "rule": rules, "back": {"/rules"}}, ed, "same-origin")
		if code != http.StatusSeeOther {
			t.Fatalf("%d %s", code, body)
		}
		return hd.Get("Location")
	}
	if got := post(" Errors", "Errors\t", "Errors\n", "SearchLatency", " "); got != "/rules?bulk=task&done=2" {
		t.Errorf("trimmed: to %q", got)
	}
	_, tasks, _ := worked(t, s)
	if len(tasks) != 2 || !strings.HasPrefix(tasks[0], "Tune Errors|tune|Errors|") || !strings.HasPrefix(tasks[1], "Tune SearchLatency|tune|SearchLatency|") {
		t.Errorf("tasks %q", tasks)
	}
	if got := post("Errors", "SearchErrors"); got != "/rules?already=1&bulk=task&done=1" {
		t.Errorf("again: to %q", got)
	}
	if _, body := getAs(t, h, "/rules?already=1&bulk=task&done=1", ed); !strings.Contains(body, `role="status">1 done; 1 had an open task to tune them already</p>`) {
		t.Error("the list doesn't say one had a task already")
	}
	// Done, it is no task to tune it still: another is made.
	ts, err := s.l.Tasks(context.Background(), ledger.TaskQuery{Rule: "Errors"})
	if err != nil || len(ts) != 1 {
		t.Fatalf("%v %v", ts, err)
	}
	done := ts[0]
	done.Status = "done"
	if _, err := s.l.PutTask(context.Background(), done, "ed"); err != nil {
		t.Fatal(err)
	}
	if got := post("Errors"); got != "/rules?bulk=task&done=1" {
		t.Errorf("after done: to %q", got)
	}
}

// TestBulkKeepsNotes: setting the team, or what is to become of the rules
// picked, changes that alone of each rule's note: its text, its runbook and
// the rest are as they were.
func TestBulkKeepsNotes(t *testing.T) {
	const ed = "ed@example.com"
	ctx := context.Background()
	s, pay, search := bulkServer(t, nil)
	h := s.Handler()
	was := map[string]ledger.Note{
		"Errors":        {Rule: "Errors", TeamID: pay, Status: "tune", Runbook: "https://runbooks.example/errors", Note: "pages at night"},
		"SearchLatency": {Rule: "SearchLatency", Runbook: "https://runbooks.example/latency", Note: "slow on Mondays"},
	}
	for _, n := range was {
		if err := s.l.PutNote(ctx, n, "ana"); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		form url.Values
		set  func(*ledger.Note)
	}{
		{url.Values{"do": {"team"}, "team": {fmt.Sprint(search)}}, func(n *ledger.Note) { n.TeamID = search }},
		{url.Values{"do": {"status"}, "status": {"keep"}}, func(n *ledger.Note) { n.Status = "keep" }},
	} {
		c.form["rule"] = []string{"Errors", "SearchLatency"}
		if code, body, hd := postAs(t, h, "/rules/bulk", c.form, ed, "same-origin"); code != http.StatusSeeOther || !strings.Contains(hd.Get("Location"), "done=2") {
			t.Fatalf("%s: %d to %q: %s", c.form.Get("do"), code, hd.Get("Location"), body)
		}
		notes, _, _ := worked(t, s)
		for name, n := range was {
			c.set(&n)
			n.By = ed
			was[name] = n
			if got := notes[name]; got != n {
				t.Errorf("%s %s: %+v, not %+v", c.form.Get("do"), name, got, n)
			}
		}
	}
}
