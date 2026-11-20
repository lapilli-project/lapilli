package serve

import (
	"context"
	"encoding/json"
	"fmt"
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

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// theExpectations hold goalServer's rules: Paged to four firings a week
// and to catching INC-A within five minutes, and INC-Z, which isn't
// written down; Noise to no re-fire.
const theExpectations = `expectations:
  - rule: Paged
    budget: {week: 4}
    catches:
      - {incident: INC-A, within: 5m}
      - {incident: INC-Z}
  - rule: Noise
    budget: {refires: 0}
`

// expectServer is a goalServer held to body by --expect, read as the
// leader reads it twice in a row, telling a webhook, with --expect-tasks
// as given.
func expectServer(t *testing.T, body string, tasks bool) *Server {
	t.Helper()
	dir := t.TempDir()
	hook, file := filepath.Join(dir, "hook"), filepath.Join(dir, "expect.yaml")
	os.WriteFile(hook, []byte("https://hooks.example/x\n"), 0o600)
	os.WriteFile(file, []byte(body), 0o600)
	s := goalServer(t, func(c *Config) { c.NotifyWebhook, c.Expect, c.ExpectTasks = hook, file, tasks })
	expectReadTwice(t, s)
	return s
}

// expectReadTwice reads --expect as a term begins and again configEvery after:
// kept to, as the leader keeps it.
func expectReadTwice(t *testing.T, s *Server) {
	t.Helper()
	at := time.Now()
	s.wall = func() time.Time { return at }
	s.expect.newTerm()
	s.readExpect()
	at = at.Add(configEvery)
	s.readExpect()
	s.keptConfig(nil)
}

// checkAgain checks the expectations again, as a round after the ledger
// changed.
func checkAgain(t *testing.T, s *Server) {
	t.Helper()
	s.expect.Lock()
	s.expect.key = ""
	s.expect.Unlock()
	if err := s.expectAfter(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// checksOf are the current checks of a rule, by what and week.
func checksOf(t *testing.T, s *Server, rule string) map[string]ledger.ExpectCheck {
	t.Helper()
	cs, err := s.l.ExpectChecks(context.Background(), ledger.ExpectQuery{Rule: rule, HasRule: true, Current: true})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]ledger.ExpectCheck{}
	for _, c := range cs {
		k := c.What
		if !c.Weekless() {
			k += " " + s.mondayOf(c.Week)
		}
		out[k] = c
	}
	return out
}

// expectNotices are the expectation.broken told to the webhook.
func expectNotices(t *testing.T, s *Server) []expectNotice {
	t.Helper()
	due, err := s.l.Due(context.Background(), "webhook", time.Now().Add(time.Hour), 500)
	if err != nil {
		t.Fatal(err)
	}
	var out []expectNotice
	for _, n := range due {
		if n.Event == "expectation.broken" {
			var x struct {
				Expectation expectNotice `json:"expectation"`
			}
			if err := json.Unmarshal(n.Body, &x); err != nil {
				t.Fatal(err)
			}
			out = append(out, x.Expectation)
		}
	}
	return out
}

// After a round, each budget is held to each of the thirteen weeks: over
// it is broken, the week so far too as soon as it is over; under it, met,
// so far in the week under way, and not known in a week the ledger holds
// part of. A catch is judged against the incident written down, and not
// known when it isn't.
func TestExpectationsChecked(t *testing.T) {
	s := expectServer(t, theExpectations, false)
	checkAgain(t, s)
	paged := checksOf(t, s, "Paged")
	for k, want := range map[string]struct {
		result string
		n      int
	}{
		"budget week 2026-10-05": {ledger.ExpectUnknown, 1}, // its first firing's week, held in part
		"budget week 2026-10-12": {ledger.ExpectMet, 2},
		"budget week 2026-10-19": {ledger.ExpectBroken, 5},
		"budget week 2026-10-26": {ledger.ExpectMet, 2},
		"budget week 2026-11-02": {ledger.ExpectBroken, 41}, // so far, and over already
		"budget week 2026-09-28": {ledger.ExpectUnknown, 0}, // before the ledger began
	} {
		c, ok := paged[k]
		if !ok || c.Result != want.result || c.N == nil || *c.N != want.n {
			t.Errorf("%s: %+v, want %s %d", k, c, want.result, want.n)
		}
	}
	if n := len(paged); n != 13+2 {
		t.Errorf("%d checks of Paged, want 13 weeks and 2 catches: %v", n, slices.Sorted(mapsKeys(paged)))
	}
	if c := paged["catches INC-A"]; c.Result != ledger.ExpectMet || c.Week != ledger.Week(s.l.Day(dayAt("2026-10-21", 0))) || c.Held != "within 5m" {
		t.Errorf("INC-A: %+v", c)
	}
	if c := paged["catches INC-Z"]; c.Result != ledger.ExpectUnknown || c.Why != whyNoIncident || c.Week != 0 {
		t.Errorf("INC-Z: %+v", c)
	}
	noise := checksOf(t, s, "Noise")
	if c := noise["budget refires 2026-10-26"]; c.Result != ledger.ExpectBroken || *c.N != 1 || c.Source != expectFromFile || c.Selector != "rule: Noise" {
		t.Errorf("Noise's re-fires: %+v", c)
	}

	// Told once each: the week so far over budget, and the last whole
	// week's re-fires; not the week of 10-19, older news.
	ns := expectNotices(t, s)
	if len(ns) != 2 {
		t.Fatalf("told: %+v", ns)
	}
	slices.SortFunc(ns, func(a, b expectNotice) int { return strings.Compare(a.Rule, b.Rule) })
	if ns[0].Rule != "Noise" || ns[0].What != "budget refires" || ns[0].Team != "search" || ns[0].SoFar ||
		ns[1].Rule != "Paged" || !ns[1].SoFar || *ns[1].N != 41 || ns[1].Team != "payments" || ns[1].From != dayAt("2026-11-02", 0) {
		t.Errorf("told: %+v", ns)
	}
	checkAgain(t, s)
	if ns := expectNotices(t, s); len(ns) != 2 {
		t.Errorf("told again: %+v", ns)
	}
	// Slack's line says it.
	if got := s.line(notice{Event: "expectation.broken", Expectation: &ns[1]}); got != "lapilli: Paged has fired 41 times so far in the week of 2026-11-02, held to at most 4" {
		t.Errorf("Slack: %q", got)
	}
}

// With --expect-tasks, Lapilli makes a task of each expectation broken
// for the rule's team: once a rule, an expectation and a week, none while
// one is open; without it, none.
func TestExpectationTasks(t *testing.T) {
	ctx := context.Background()
	s := expectServer(t, theExpectations, false)
	checkAgain(t, s)
	if ts, _ := s.l.Tasks(ctx, ledger.TaskQuery{}); len(ts) != 0 {
		t.Errorf("tasks without --expect-tasks: %+v", ts)
	}
	s = expectServer(t, theExpectations, true)
	checkAgain(t, s)
	ts, err := s.l.Tasks(ctx, ledger.TaskQuery{})
	if err != nil || len(ts) != 2 {
		t.Fatalf("%+v %v", ts, err)
	}
	slices.SortFunc(ts, func(a, b ledger.Task) int { return strings.Compare(a.Rule, b.Rule) })
	if ts[0].Rule != "Noise" || ts[0].Kind != "tune" || ts[0].By != ledger.ExpectTasksBy || !strings.Contains(ts[0].Title, "Noise over its budget, week of 2026-10-26") ||
		ts[1].Rule != "Paged" || ts[1].TeamID == 0 || !strings.Contains(ts[1].Body, "/rules/Paged?tab=expectations") {
		t.Errorf("tasks: %+v", ts)
	}
	// Done, and the round again in the same week: none made again.
	for _, x := range ts {
		if _, err := s.l.PatchTask(ctx, x.ID, []byte(`{"status": "done"}`), "ana"); err != nil {
			t.Fatal(err)
		}
	}
	checkAgain(t, s)
	if ts, _ := s.l.Tasks(ctx, ledger.TaskQuery{}); len(ts) != 2 {
		t.Errorf("made again in the week: %d", len(ts))
	}
}

// expectation.broken goes where the routes send it, as goal.missed does:
// Paged's, payments's, to payments's route alone; Noise's, search's, which
// no route takes, to the default; each told once (by the rule's labels,
// TestRouteMatch). With --config and its
// destinations but no routes, as before routes: to the webhook and Slack,
// and with neither, nowhere, nothing marked told.
func TestExpectationBrokenRouted(t *testing.T) {
	routed := func(t *testing.T, config string, hooks bool) *Server {
		t.Helper()
		dir := t.TempDir()
		url, file, cfg := filepath.Join(dir, "hook"), filepath.Join(dir, "expect.yaml"), filepath.Join(dir, "config.yaml")
		os.WriteFile(url, []byte("https://hooks.example/x\n"), 0o600)
		os.WriteFile(file, []byte(theExpectations), 0o600)
		writeConfig(t, cfg, config)
		s := goalServer(t, func(c *Config) {
			c.Expect, c.Config = file, cfg
			c.WebhookDests = map[string]string{"payments-hook": url}
			if hooks {
				c.NotifyWebhook, c.Slack, c.NotifySlack = url, url, true
			}
		})
		s.startRoutes(context.Background())
		expectReadTwice(t, s)
		checkAgain(t, s)
		return s
	}
	broken := func(t *testing.T, s *Server, d string) []string {
		t.Helper()
		var out []string
		for _, n := range outbox(t, s)[d] {
			var x struct {
				Expectation expectNotice `json:"expectation"`
			}
			if n.Event == "expectation.broken" && json.Unmarshal(n.Body, &x) == nil {
				out = append(out, x.Expectation.Rule)
			}
		}
		slices.Sort(out)
		return out
	}
	s := routed(t, "notify:\n  routes:\n    - kinds: [expectation.broken]\n      teams: [payments]\n      to: [payments-hook]\n  default: [slack]\n", true)
	for d, want := range map[string][]string{"route:payments-hook": {"Paged"}, "slack": {"Noise"}, "webhook": nil} {
		if got := broken(t, s, d); !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", d, got, want)
		}
	}
	checkAgain(t, s)
	if got := broken(t, s, "route:payments-hook"); len(got) != 1 {
		t.Errorf("told again: %v", got)
	}
	// No routes: the webhook and Slack, as before routes.
	s = routed(t, "teams: []\n", true)
	if got := broken(t, s, "webhook"); !slices.Equal(got, []string{"Noise", "Paged"}) || len(broken(t, s, "route:payments-hook")) != 0 {
		t.Errorf("without routes: %v", got)
	}
	// Neither: nowhere, and still to tell.
	s = routed(t, "teams: []\n", false)
	if got := outbox(t, s); len(got) != 0 {
		t.Errorf("written: %v", slices.Sorted(mapsKeys(got)))
	}
	if c := checksOf(t, s, "Noise")["budget refires 2026-10-26"]; c.Result != ledger.ExpectBroken || c.Told != 0 {
		t.Errorf("marked told with nowhere to tell it: %+v", c)
	}
}

// --config's expectations hold rules as the file's do, one list: the same
// budget given in both is refused, and what was held stays. A rule's
// annotation holds it too, but where the file gives the same budget.
func TestExpectationSources(t *testing.T) {
	s := expectServer(t, theExpectations, false)
	cfg, err := ParseConfig([]byte("expectations:\n  - rule: Noise\n    catches: [{incident: INC-B}]\n"), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	s.keptConfig(cfg.Expectations)
	checkAgain(t, s)
	if c := checksOf(t, s, "Noise")["catches INC-B"]; c.Source != expectFromConfig || c.Result != ledger.ExpectBroken || c.Value != "✗" {
		t.Errorf("--config's: %+v", c)
	}
	// Both give Noise re-fires: refused, the last that could be held kept.
	cfg, _ = ParseConfig([]byte("expectations:\n  - rule: Noise\n    budget: {refires: 5}\n"), time.UTC)
	s.keptConfig(cfg.Expectations)
	checkAgain(t, s)
	if c := checksOf(t, s, "Noise")["budget refires 2026-10-26"]; c.Held != "0" {
		t.Errorf("held to: %+v", c)
	}
	s.expect.Lock()
	bad := s.expect.bad
	s.expect.Unlock()
	if !strings.Contains(bad, "Noise: a budget of re-fires given twice") {
		t.Errorf("not said: %q", bad)
	}

	// An annotation, read; one the file's budget is given by, passed
	// over and said; one that can't be read, said without its text.
	x := &expectRule{name: "Paged", budget: &verdict.Budget{Week: intPtr(9), Asleep: intPtr(1)}}
	e, _ := verdict.ReadExpectations(strings.NewReader(theExpectations))
	var whys []string
	for _, it := range heldBy(x, e, map[string]string{}) {
		if it.selector == annotationSelector {
			whys = append(whys, it.What+":"+it.why)
		}
	}
	if !slices.Equal(whys, []string{"budget week:" + whyAnnotationTwo, "budget asleep:"}) {
		t.Errorf("annotation: %q", whys)
	}
	x = &expectRule{name: "Other", budgetBad: true}
	if hs := heldBy(x, e, nil); len(hs) != 1 || hs[0].why != whyAnnotationBad {
		t.Errorf("unread: %+v", hs)
	}
}

// --expect is kept to once read the same twice in a row, configEvery
// apart; one that can't be read changes nothing, and is said.
func TestExpectFileReadTwice(t *testing.T) {
	s := expectServer(t, theExpectations, false)
	at := time.Now()
	s.wall = func() time.Time { return at }
	os.WriteFile(s.cfg.Expect, []byte("expectations:\n  - rule: Paged\n    budjet: {week: 1}\n"), 0o600)
	s.readExpect()
	e, _, _, ok := s.expectations()
	if !ok || len(e.Expectations) != 2 {
		t.Errorf("a bad file changed what is held: %+v", e)
	}
	if err := s.expectAfter(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := s.statusView("en", true); !strings.Contains(st.Err, "budjet") {
		t.Errorf("not said: %+v", st.Err)
	}
	// Its path, as every file serve was started with, to an admin alone.
	if st := s.statusView("en", false); strings.Contains(st.Err, s.cfg.Expect) || !strings.Contains(st.Err, "--expect") {
		t.Errorf("to a viewer: %+v", st.Err)
	}
	os.WriteFile(s.cfg.Expect, []byte("expectations:\n  - rule: Paged\n    budget: {week: 1}\n"), 0o600)
	s.readExpect()
	if e, _, _, _ := s.expectations(); len(e.Expectations) != 2 {
		t.Error("kept to after one read")
	}
	at = at.Add(configEvery)
	s.readExpect()
	if e, _, _, _ := s.expectations(); len(e.Expectations) != 1 {
		t.Errorf("not kept to after two: %+v", e)
	}
}

func mapsKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func intPtr(n int) *int { return &n }

// A catch of a rule changed since its incident is judged again on the rule
// as it is, by a replay, once for each version of it: here a threshold no
// series reaches no longer catches INC-1, which the version before did.
// labels: holds the rules whose labels its selector matches.
func TestExpectationCatchJudgedAgain(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "expect.yaml")
	os.WriteFile(file, []byte("expectations:\n  - rule: Errors\n    catches: [{incident: INC-1, within: 10m}]\n  - labels: severity=page\n    budget: {week: 100}\n"), 0o600)
	s, rulesDir := server(t, func(c *Config) { keepRecord(c); c.Expect = file })
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	expectReadTwice(t, s)
	checkAgain(t, s)
	errs := checksOf(t, s, "Errors")
	if c := errs["catches INC-1"]; c.Result != ledger.ExpectMet || c.Version != "" {
		t.Fatalf("as the ledger judged it: %+v", c)
	}
	if c := errs["budget week 2026-11-02"]; c.Selector != "labels: severity=page" || c.Result != ledger.ExpectOpen {
		t.Errorf("by labels: %+v", c)
	}
	os.WriteFile(filepath.Join(rulesDir, "shop.yaml"), []byte(strings.Replace(ruleFile, "errors > 5", "errors > 50", 1)), 0o644)
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	checkAgain(t, s)
	c := checksOf(t, s, "Errors")["catches INC-1"]
	if c.Result != ledger.ExpectBroken || c.Value != "✗" || c.Version == "" {
		t.Fatalf("judged again: %+v", c)
	}
	h := s.h.(*history)
	h.Lock()
	asked := h.exprs["errors > 50"]
	h.Unlock()
	checkAgain(t, s)
	h.Lock()
	again := h.exprs["errors > 50"]
	h.Unlock()
	if asked == 0 || again != asked {
		t.Errorf("replayed %d, then %d: once a version", asked, again)
	}
}

// The rule's tab shows each expectation with its weeks; the overview says
// each broken, made a task with one press, which then links to it; the
// rules list picks the rules held to none, and those broken; the API, MCP
// and /metrics give the checks; the digest counts the week's.
func TestExpectationPages(t *testing.T) {
	ctx := context.Background()
	s := expectServer(t, "expectations:\n  - rule: Paged\n    budget: {week: 4}\n    catches: [{incident: INC-A, within: 5m}]\n", false)
	checkAgain(t, s)
	h := s.Handler()
	code, body, _ := get(t, h, "/rules/Paged?tab=expectations")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	for _, want := range []string{"at most 4 a week", "Held by rule: Paged, in the expectations file.", "catches INC-A within 5m", `action="/expectations/task"`,
		"week of 2026-10-19: broken, 5", `aria-current="page">Expectations <span class="count">2</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the tab: no %q", want)
		}
	}
	_, body, _ = get(t, h, "/rules/Noise?tab=expectations")
	if !strings.Contains(body, "This rule is held to no expectation.") {
		t.Error("a rule held to nothing says so")
	}
	_, body, _ = get(t, h, "/")
	if !strings.Contains(body, "Paged has fired 41 times so far in the week of 2026-11-02, held to at most 4") || !strings.Contains(body, `action="/expectations/task"`) {
		t.Errorf("the overview: %s", body)
	}
	this := ledger.Week(s.l.Day(now))
	paged := checksOf(t, s, "Paged")["budget week 2026-11-02"]
	form := url.Values{"rule": {"Paged"}, "expect": {paged.Expect}, "week": {strconv.FormatInt(this, 10)}}
	if code, body, _ := post(t, h, "/expectations/task", form, "same-origin"); code != 303 {
		t.Fatalf("make it a task: %d %s", code, body)
	}
	ts, _ := s.l.Tasks(ctx, ledger.TaskQuery{Rule: "Paged"})
	if len(ts) != 1 || ts[0].Kind != "tune" || !strings.Contains(ts[0].Title, "Paged over its budget, week of 2026-11-02") {
		t.Fatalf("task: %+v", ts)
	}
	// Again: to the task open, none made.
	if code, _, hd := post(t, h, "/expectations/task", form, "same-origin"); code != 303 || !strings.HasSuffix(hd.Get("Location"), fmt.Sprintf("/tasks/%d", ts[0].ID)) {
		t.Errorf("again: %d %v", code, hd)
	}
	// Of a week not broken: refused.
	form.Set("week", strconv.FormatInt(this-7, 10))
	if code, _, _ := post(t, h, "/expectations/task", form, "same-origin"); code != 400 {
		t.Errorf("a week met: %d", code)
	}
	_, body, _ = get(t, h, "/")
	if !strings.Contains(body, fmt.Sprintf(`href="/tasks/%d"`, ts[0].ID)) {
		t.Error("the overview doesn't link to its task")
	}

	_, body, _ = get(t, h, "/rules?expect=none&weeks=12")
	if !strings.Contains(body, `href="/rules/Noise`) || strings.Contains(body, `href="/rules/Paged`) {
		t.Error("held to none: Noise alone")
	}
	_, body, _ = get(t, h, "/rules?expect=broken&weeks=12")
	if !strings.Contains(body, `href="/rules/Paged`) || !strings.Contains(body, "Expectation broken") {
		t.Error("broken: Paged, with its badge")
	}
	code, body, _ = get(t, h, "/api/v1/expectations?rule=Paged")
	var api struct {
		Checks []expectCheckOut `json:"checks"`
	}
	if err := json.Unmarshal([]byte(body), &api); code != 200 || err != nil || len(api.Checks) != 14 {
		t.Errorf("API: %d %v %d", code, err, len(api.Checks))
	}
	_, out, err := s.toolRule(ctx, nil, ruleIn{Name: "Paged"})
	if err != nil || len(out.Expectations) != 14 {
		t.Errorf("MCP: %d %v", len(out.Expectations), err)
	}
	_, body, _ = get(t, h, "/metrics")
	for _, want := range []string{"lapilli_expectations_held 2\n", "lapilli_expectations_broken 1\n", `lapilli_rule_expectations_broken{rule="Paged"} 1`} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics: no %q", want)
		}
	}
	d, err := s.expectDigestOf(ctx, this-7, nil)
	if err != nil || d == nil || d.Met != 1 || d.Broken != 0 {
		t.Errorf("the digest's week of 10-26: %+v %v", d, err)
	}
	d, _ = s.expectDigestOf(ctx, this-14, nil)
	if d == nil || d.Broken != 1 || !strings.Contains(d.text("en"), "Expectations: 1 broken, 1 met, 0 not known. Paged fired 5 times in the week of 2026-10-19, held to at most 4.") {
		t.Errorf("the digest's week of 10-19: %+v %q", d, d.text("en"))
	}
}

// The expectations' pages find a translation for every text they show, in
// each language.
func TestExpectationPagesInEachLanguage(t *testing.T) {
	s := expectServer(t, theExpectations+"  - group: nothing\n    budget: {asleep: 1}\n", false)
	checkAgain(t, s)
	h := s.Handler()
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	for _, n := range names {
		for _, p := range []string{"/", "/rules/Paged?tab=expectations", "/rules/Noise?tab=expectations", "/rules?expect=broken&weeks=12", "/rules?expect=none"} {
			r := httptest.NewRequest(http.MethodGet, p, nil)
			r.Header.Set("Sec-Fetch-Site", "none")
			r.Header.Set("Accept-Language", n.Code)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 200 {
				t.Errorf("%s %s: %d", n.Code, p, w.Code)
			}
		}
	}
	for _, c := range []ledger.ExpectCheck{{What: "budget asleep", Week: 1, Source: "file"}, {What: "budget refires", Week: 1, Source: "config"}, {What: "catches X", Value: "+9m", Held: "within 5m", Source: "annotation"}} {
		for _, n := range names {
			s.expectSentence(n.Code, c, 1)
			s.expectSentence(n.Code, c, 0)
			s.expectTaskIn(n.Code, c, 0, 1)
			for _, why := range expectWhyText {
				tr(n.Code, why)
			}
		}
	}
	missing.Lock()
	defer missing.Unlock()
	for k := range missing.m {
		lang, msg, _ := strings.Cut(k, "\x00")
		t.Errorf("%s has no translation for %q", lang, msg)
	}
}

// A rule's annotation lapilli.dev/budget holds it, with no file: its
// budget checked as a file's is, from where it is declared; one that can't
// be read is said on its tab as not known, without what it holds.
func TestExpectationAnnotations(t *testing.T) {
	ctx := context.Background()
	s, rulesDir := server(t, keepRecord)
	os.WriteFile(filepath.Join(rulesDir, "shop.yaml"), []byte(`groups:
  - name: shop
    rules:
      - alert: Errors
        expr: errors > 5
        for: 2m
        labels: {severity: page}
        annotations: {lapilli.dev/budget: "0/week, 1 refires"}
      - alert: Busy
        expr: busy > 1
        annotations: {lapilli.dev/budget: "<b>ten</b>/week"}
`), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	expectReadTwice(t, s)
	checkAgain(t, s)
	errs := checksOf(t, s, "Errors")
	if c := errs["budget week 2026-11-02"]; c.Result != ledger.ExpectBroken || c.Source != expectFromAnnotation || c.Selector != annotationSelector || c.Held != "0" {
		t.Errorf("Errors' annotation: %+v", c)
	}
	if _, ok := errs["budget refires 2026-11-02"]; !ok {
		t.Errorf("Errors' re-fires: %v", slices.Sorted(mapsKeys(errs)))
	}
	if c := checksOf(t, s, "Busy")["budget 2026-11-02"]; c.Result != ledger.ExpectUnknown || c.Why != whyAnnotationBad {
		t.Errorf("Busy's: %+v", c)
	}
	_, body, _ := get(t, s.Handler(), "/rules/Busy?tab=expectations")
	if !strings.Contains(body, "its annotation lapilli.dev/budget can&#39;t be read") || strings.Contains(body, "ten</b>") || strings.Contains(body, "&lt;b&gt;ten") {
		t.Errorf("Busy's tab: %s", body)
	}
}
