package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// respUIPages are the pages of respUIServer: two firings of the week before
// last week nobody answered, in last week one nobody answered and one a
// person acknowledged; a deploy, in last week too, answers a fifth firing.
const respUIPages = `responses:
  - {id: PD-4, alerts: [{rule: checkout-5xx, labels: {service: checkout, namespace: shop}}], sent: 2026-10-21T02:11:00Z, resolved: 2026-10-21T02:40:00Z, resolved_by: alert}
  - {id: PD-5, alerts: [{rule: checkout-5xx, labels: {service: checkout, namespace: shop}}], sent: 2026-10-22T02:11:00Z, resolved: 2026-10-22T02:40:00Z, resolved_by: alert}
  - {id: PD-1, alerts: [{rule: checkout-5xx, labels: {service: checkout, namespace: shop}}], sent: 2026-10-28T02:11:00Z, resolved: 2026-10-28T02:40:00Z, resolved_by: alert}
  - {id: PD-2, alerts: [{rule: checkout-5xx, labels: {service: checkout, namespace: shop}}], sent: 2026-10-29T02:11:00Z, acknowledged: 2026-10-29T02:15:00Z}
`

// respUIServer is a server whose ledger holds six firings of a rule, in the
// three weeks to now, with pages (withPages) and a deploy of what the
// third was about; noisyBy is what its --noisy-by is.
func respUIServer(t *testing.T, withPages bool, noisyBy string, change func(*Config)) *Server {
	t.Helper()
	file := ""
	if withPages {
		file = t.TempDir() + "/responses.yaml"
		if err := os.WriteFile(file, []byte(respUIPages), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := server(t, func(c *Config) {
		c.UserHeader, c.Admins, c.Editors = "X-Forwarded-Email", []string{"ana@example.com"}, []string{"bo@example.com"}
		c.Responses = file
		c.Options.NoisyBy = noisyBy
		c.Options.DeployBefore = 30 * time.Minute // a firing that began before a deploy is the deploy's too, judged again with it
		if change != nil {
			change(c)
		}
	})
	ctx := context.Background()
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.ready.Store(true)
	o, err := s.options(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var fs []firing.Firing
	for _, d := range []string{"2026-10-21", "2026-10-22", "2026-10-28", "2026-10-29", "2026-10-30", "2026-11-01"} {
		fs = append(fs, firing.Firing{Rule: "checkout-5xx", Labels: map[string]string{"service": "checkout", "namespace": "shop"},
			Start: utc(d + "T02:10:00Z"), End: utc(d + "T02:40:00Z")})
	}
	keep(t, s, utc("2026-10-12T00:00:00Z"), utc("2026-11-03T00:00:00Z"), fs)
	if withPages {
		if err := s.syncResponses(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	// A deploy of what the Friday's firing is about: a change on its target.
	if _, err := s.l.SyncDeploys(ctx, []ledger.Deploy{{Start: utc("2026-10-30T02:20:00Z"), End: utc("2026-10-30T02:25:00Z"),
		Labels: map[string]string{"service": "checkout"}}}, o, ledger.DeploysRead{}); err != nil {
		t.Fatal(err)
	}
	if err := s.l.JudgePending(ctx, o); err != nil {
		t.Fatal(err)
	}
	return s
}

// ruleOf is a rule's page for a query, worked out as the page is.
func ruleOf(t *testing.T, s *Server, name, query string) ruleView {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/rules/"+name+"?"+query, nil)
	r.SetPathValue("name", name)
	_, vr := s.withContext(r, "rule")
	v, err := s.rulePage(vr)
	if err != nil {
		t.Fatalf("%s?%s: %v", name, query, err)
	}
	return v.(ruleView)
}

func getPage(t *testing.T, s *Server, path string) string {
	t.Helper()
	code, body := askAs(t, s.Handler(), http.MethodGet, path, nil, "ana@example.com", "same-origin")
	if code != http.StatusOK {
		t.Fatalf("%s: %d", path, code)
	}
	return body
}

// With responses kept, the alert history says what was done about each
// firing, picks by it, and its form offers the choice; the filter alone, with
// no day, goes to the last day with such a firing.
func TestResponsesAlertHistory(t *testing.T) {
	parallel(t)
	s := respUIServer(t, true, "", nil)
	body := getPage(t, s, "/firings?day=2026-10-28")
	for _, want := range []string{`<th scope="col">Response</th>`, "Nobody acknowledged; resolved by its alert manager", "page <code><bdi>PD-1</bdi></code>", `<select name="response">`} {
		if !strings.Contains(body, want) {
			t.Errorf("a day with a page nobody answered lacks %q", want)
		}
	}
	if body := getPage(t, s, "/firings?day=2026-10-29"); !strings.Contains(body, "Acknowledged") || !strings.Contains(body, "PD-2") {
		t.Error("an acknowledged firing isn't said so")
	}
	body = getPage(t, s, "/firings?day=2026-10-30")
	if !strings.Contains(body, "Deployed 02:20") || !strings.Contains(body, "service=checkout") {
		t.Errorf("a deploy made after a firing began: %s", body)
	}
	// A firing with nothing known has nothing in its cell.
	if body := getPage(t, s, "/firings?day=2026-11-01"); strings.Contains(body, "Nobody acknowledged") || strings.Contains(body, "Deployed") || strings.Contains(body, "Rolled") {
		t.Error("a firing with no response says something of one")
	}
	// The filter, with a day and without.
	v := firingsOf(t, s, "day=2026-10-28&response=acknowledged")
	if len(v.Lines) != 0 || v.Response != "acknowledged" || len(v.Labels) != 1 || v.Labels[0].Group != "Response" {
		t.Errorf("one day, acknowledged: %d lines, %+v", len(v.Lines), v.Labels)
	}
	v = firingsOf(t, s, "response=unanswered")
	if v.Day != "2026-10-28" || len(v.Lines) != 1 || v.Lines[0].Response == nil || v.Lines[0].Response.Kind != verdict.ResponseUnanswered || !v.Said.Latest {
		t.Errorf("the last day with one nobody answered: %s, %d lines, %+v", v.Day, len(v.Lines), v.Said)
	}
	if v := firingsOf(t, s, "day=2026-10-30&response=changed"); len(v.Lines) != 1 {
		t.Errorf("changed: %d lines", len(v.Lines))
	}
	if v := firingsOf(t, s, "day=2026-11-01&response=none"); len(v.Lines) != 1 {
		t.Errorf("none: %d lines", len(v.Lines))
	}
	// The chip takes the filter away, and what isn't a response is no filter.
	if v := firingsOf(t, s, "day=2026-10-28&response=unanswered"); len(v.Labels) != 1 || strings.Contains(v.Labels[0].Href, "response=") {
		t.Errorf("the chip: %+v", v.Labels)
	}
	if v := firingsOf(t, s, "day=2026-10-28&response=sleeping"); v.Response != "" || len(v.Lines) != 1 {
		t.Errorf("a response that isn't one: %q, %d lines", v.Response, len(v.Lines))
	}
}

// Not shown, nothing of responses is: not the column, the form's choice,
// nor a filter, with no pages kept, and with deploys alone, which serve took
// before responses and which leave it noisy by what its firings caught.
func TestResponsesAlertHistoryNotShown(t *testing.T) {
	parallel(t)
	for name, s := range map[string]*Server{
		"nothing kept":   respUIServer(t, false, "", nil),
		"deploys alone":  respUIServer(t, false, "", func(c *Config) { c.Deploys = os.DevNull }),
		"no responses 2": respUIServer(t, false, "caught", nil),
	} {
		body := getPage(t, s, "/firings?day=2026-10-28")
		if strings.Contains(body, ">Response<") || strings.Contains(body, `name="response"`) {
			t.Errorf("%s: the page shows responses", name)
		}
		if v := firingsOf(t, s, "response=unanswered"); v.Response != "" || v.Responses || v.Said.Latest && v.Day != "2026-11-01" {
			t.Errorf("%s: a filter that has nothing to pick by was taken: %q on %s", name, v.Response, v.Day)
		}
	}
}

// A rule's page has Actionable beside Precision, as a share or a dash, the
// counts of its responses, and its latest firings picked by response.
func TestResponsesRulePage(t *testing.T) {
	parallel(t)
	s := respUIServer(t, true, "", nil)
	body := getPage(t, s, "/rules/checkout-5xx?tab=firings")
	for _, want := range []string{"<summary>Actionable", "1 changed, 1 acknowledged, 3 unanswered, 0 routine, 1 none", "1 useful, 3 not useful", `<th scope="col">Response</th>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the rule page lacks %q", want)
		}
	}
	rv := ruleOf(t, s, "checkout-5xx", "")
	if !rv.Responding || rv.Responses == nil || rv.Responses.Actionable == nil || *rv.Responses.Actionable != 0.25 {
		t.Fatalf("its responses: %+v", rv.Responses)
	}
	if got := actionableText(rv.Responses); got != "25%" {
		t.Errorf("actionable %q", got)
	}
	if len(rv.Latest) != 6 || len(rv.ResponseKinds) != 5 {
		t.Errorf("latest %d, kinds %+v", len(rv.Latest), rv.ResponseKinds)
	}
	for kind, n := range map[string]int{"unanswered": 3, "acknowledged": 1, "changed": 1, "routine": 0, "none": 1} {
		rv := ruleOf(t, s, "checkout-5xx", "tab=firings&response="+kind)
		if len(rv.Latest) != n || rv.ResponseKind != kind {
			t.Errorf("%s: %d firings listed, want %d", kind, len(rv.Latest), n)
		}
		for _, l := range rv.Latest {
			if l.Response == nil && kind != "none" || l.Response != nil && l.Response.Kind != kind && kind != "none" {
				t.Errorf("%s: a firing with %+v", kind, l.Response)
			}
		}
	}
	body = getPage(t, s, "/rules/checkout-5xx?tab=firings&response=unanswered")
	if !strings.Contains(body, `aria-current="page">Unanswered`) || !strings.Contains(body, "All its firings with this response, by day") {
		t.Error("the filter isn't marked, or has no link to the history")
	}
}

// A rule's page, with nothing kept, has no Actionable, nor a filter.
func TestResponsesRulePageNotShown(t *testing.T) {
	parallel(t)
	s := respUIServer(t, false, "", nil)
	body := getPage(t, s, "/rules/checkout-5xx?tab=firings&response=unanswered")
	for _, no := range []string{"Actionable", "Response", "response="} {
		if strings.Contains(body, no) {
			t.Errorf("a rule page with nothing kept says %q", no)
		}
	}
	rv := ruleOf(t, s, "checkout-5xx", "tab=firings&response=unanswered")
	if rv.Responding || rv.ResponseKind != "" || len(rv.Latest) != 6 {
		t.Errorf("responding %v, %q, %d firings", rv.Responding, rv.ResponseKind, len(rv.Latest))
	}
}

// The rules list has an Actionable column, a total, and an order, once
// responses show.
func TestResponsesRulesList(t *testing.T) {
	parallel(t)
	s := respUIServer(t, true, "", nil)
	rl := viewOf(t, s, "rules", "/rules?weeks=12&sort=actionable", s.rulesList).(rulesList)
	if !rl.Responding || rl.Sort != "actionable" || rl.Sum.Actionable != "25%" || len(rl.Rows) != 1 {
		t.Fatalf("responding %v, sort %q, sum %q, %d rows", rl.Responding, rl.Sort, rl.Sum.Actionable, len(rl.Rows))
	}
	cols := 11 + 1 // and the column
	if rl.Cols() < cols {
		t.Errorf("columns %d", rl.Cols())
	}
	body := getPage(t, s, "/rules?weeks=12&sort=actionable")
	for _, want := range []string{`<th scope="col" class="n">Actionable</th>`, `<td class="n">25%</td>`, "actionable, the least first"} {
		if !strings.Contains(body, want) {
			t.Errorf("the list lacks %q", want)
		}
	}
	// A rule whose share isn't known comes after the rest.
	lines := []ruleLine{{Name: "a"}, {Name: "b", Responses: &verdict.RuleResponses{Actionable: ptr(0.5)}}, {Name: "c", Responses: &verdict.RuleResponses{Actionable: ptr(0.1)}}}
	sortRules(lines, "actionable")
	if lines[0].Name != "c" || lines[1].Name != "b" || lines[2].Name != "a" {
		t.Errorf("by actionable: %v", []string{lines[0].Name, lines[1].Name, lines[2].Name})
	}
	// Not shown: no column, no order.
	s = respUIServer(t, false, "", nil)
	rl = viewOf(t, s, "rules", "/rules?weeks=12&sort=actionable", s.rulesList).(rulesList)
	if rl.Responding || rl.Sort != "" {
		t.Errorf("not shown: responding %v, sort %q", rl.Responding, rl.Sort)
	}
	if body := getPage(t, s, "/rules?weeks=12"); strings.Contains(body, "Actionable") {
		t.Error("a list with nothing kept has the column")
	}
}

// The overview has "Pages nobody answered", last week beside the week
// before, linked to the alert history picked by it, once responses show.
func TestResponsesOverviewTile(t *testing.T) {
	parallel(t)
	s := respUIServer(t, true, "", nil)
	d := viewOf(t, s, "overview", "/", s.overview).(dashboard)
	var got *tile
	for i := range d.Tiles {
		if d.Tiles[i].key == "Pages nobody answered" {
			got = &d.Tiles[i]
		}
	}
	if got == nil {
		t.Fatalf("no tile among %d", len(d.Tiles))
	}
	if got.Value != "1" || got.Of != "of 2 pages" || got.Delta != "-1 on the week before" || !got.Better || got.Href != "/firings?response=unanswered" {
		t.Errorf("the tile: %+v", got)
	}
	body := getPage(t, s, "/")
	if !strings.Contains(body, "Pages nobody answered") || !strings.Contains(body, `href="/firings?response=unanswered"`) {
		t.Error("the overview doesn't show it")
	}
	// Not shown, none.
	s = respUIServer(t, false, "", nil)
	if body := getPage(t, s, "/"); strings.Contains(body, "Pages nobody answered") {
		t.Error("an overview with nothing kept has the tile")
	}
}

// Settings has noisy-by as a choice, caught or useful, tried on the last week
// first as the thresholds are, and saved the same way.
func TestResponsesSettingsNoisyBy(t *testing.T) {
	parallel(t)
	s := respUIServer(t, true, "", nil)
	h := s.Handler()
	body := getPage(t, s, "/settings")
	if !strings.Contains(body, `<select name="noisy-by"`) || strings.Contains(body, `name="noisy-by" value=`) || !strings.Contains(body, `<option value="caught" selected>`) {
		t.Error("noisy-by isn't a choice, caught chosen")
	}
	// Tried: 3 firings in the last week, 1 useful and 1 not, so under 60% of them useful, from 2 firings.
	q := url.Values{"try": {"1"}, "noisy-by": {"useful"}, "noisy-firings": {"2"}, "noisy-precision": {"60%"}}
	code, body := askAs(t, h, http.MethodGet, "/settings?"+q.Encode(), nil, "ana@example.com", "same-origin")
	if code != http.StatusOK {
		t.Fatalf("the try: %d", code)
	}
	for _, want := range []string{"Tried on", "checkout-5xx", "<th scope=\"col\">Actionable</th>", "not noisy → noisy"} {
		if !strings.Contains(body, want) {
			t.Errorf("the try lacks %q", want)
		}
	}
	// Caught, as it is, the rule isn't among those that would change; and none of it is saved.
	if _, body := askAs(t, h, http.MethodGet, "/settings?try=1&noisy-by=caught", nil, "ana@example.com", "same-origin"); strings.Contains(body, "checkout-5xx") {
		t.Error("a try of what is in effect would change the rule")
	}
	ctx := context.Background()
	if o, _ := s.options(ctx); o.NoisyBy != "" {
		t.Errorf("a try saved %q", o.NoisyBy)
	}
	// Saved as the thresholds are, with who changed it; a value that isn't one is refused.
	if code, _ := askAs(t, h, http.MethodPost, "/settings", url.Values{"noisy-by": {"both"}}, "ana@example.com", "same-origin"); code != http.StatusBadRequest {
		t.Errorf("a value that isn't one: %d", code)
	}
	form := url.Values{"noisy-by": {"useful"}}
	if code, _ := askAs(t, h, http.MethodPost, "/settings", form, "ana@example.com", "same-origin"); code != http.StatusSeeOther {
		t.Fatalf("saved: %d", code)
	}
	if o, _ := s.options(ctx); o.NoisyBy != verdict.NoisyByUseful {
		t.Errorf("saved %q", o.NoisyBy)
	}
	set, _ := s.l.Settings(ctx)
	if c, ok := set["noisy-by"]; !ok || c.By == "" || c.Value != "useful" {
		t.Errorf("kept %+v", c)
	}
	if body := getPage(t, s, "/settings"); !strings.Contains(body, `<option value="useful" selected>`) || !strings.Contains(body, "changed by") {
		t.Error("the page doesn't say useful is chosen, and by whom")
	}
	// An editor sees it, and may not change it.
	_, body = askAs(t, h, http.MethodGet, "/settings", nil, "bo@example.com", "same-origin")
	if strings.Contains(body, `<select name="noisy-by"`) || !strings.Contains(body, "useful (by what was done about its firings)") {
		t.Error("an editor is offered the choice, or isn't told what is in effect")
	}
	// Back to the start: the command line's.
	if code, _ := askAs(t, h, http.MethodPost, "/settings", url.Values{"noisy-by": {"useful"}, "reset-noisy-by": {"1"}}, "ana@example.com", "same-origin"); code != http.StatusSeeOther {
		t.Fatalf("reset: %d", code)
	}
	if o, _ := s.options(ctx); o.NoisyBy != "" {
		t.Errorf("reset %q", o.NoisyBy)
	}
}

// Choosing useful shows responses, with nothing kept but people's answers:
// the pages have what it is found by.
func TestResponsesShownByNoisyBy(t *testing.T) {
	parallel(t)
	s := respUIServer(t, false, verdict.NoisyByUseful, nil)
	if body := getPage(t, s, "/rules?weeks=12"); !strings.Contains(body, "Actionable") {
		t.Error("rules found noisy by what was done have no Actionable")
	}
	if body := getPage(t, s, "/firings?day=2026-10-28"); !strings.Contains(body, `name="response"`) {
		t.Error("the alert history has no response to pick by")
	}
}

// Every page that shows responses finds a translation for each text it says
// of them, in each language; the pages say it in the language asked for.
func TestResponsesPagesInEachLanguage(t *testing.T) {
	parallel(t)
	s := respUIServer(t, true, "", nil)
	h := s.Handler()
	paths := []string{"/firings?day=2026-10-28", "/firings?day=2026-10-30", "/firings?day=2026-10-29&response=acknowledged", "/rules?weeks=12&sort=actionable",
		"/rules/checkout-5xx?tab=firings", "/rules/checkout-5xx?tab=firings&response=unanswered", "/", "/settings", "/settings?try=1&noisy-by=useful&noisy-firings=2&noisy-precision=60%25"}
	// What each says in English, and a word of it in another language.
	said := map[string]string{"ko": "무응답", "ja": "未応答", "es": "Sin atender"}
	for _, n := range names {
		if n.Code == "en" {
			continue
		}
		seen := false
		for _, p := range paths {
			r := httptest.NewRequest(http.MethodGet, p, nil)
			r.Header.Set("Sec-Fetch-Site", "same-origin")
			r.Header.Set("X-Forwarded-Email", "ana@example.com")
			r.Header.Set("Accept-Language", n.Code)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			body := w.Body.String()
			if w.Code != http.StatusOK || !strings.Contains(body, `<html lang="`+n.Code+`">`) {
				t.Errorf("%s %s: %d", n.Code, p, w.Code)
			}
			for _, en := range []string{"Nobody acknowledged; resolved by its alert manager", "Pages nobody answered", "Actionable", ">Response<", "Deployed 02:20", "Routine change", "Change made"} {
				if strings.Contains(body, en) {
					t.Errorf("%s %s says %q in English", n.Code, p, en)
				}
			}
			seen = seen || strings.Contains(body, said[n.Code])
		}
		if !seen {
			t.Errorf("%s: no page says %q", n.Code, said[n.Code])
		}
	}
	// The English texts the pages ask a translation of are all in each catalog,
	// as the pages rendered above found them.
	missing.Lock()
	defer missing.Unlock()
	for k := range missing.m {
		lang, msg, _ := strings.Cut(k, "\x00")
		for _, word := range []string{"Response", "Actionable", "nobody answered", "Nobody acknowledged", "Rolled", "Scaled", "Deployed", "routine", "noisy-by", "useful (by"} {
			if strings.Contains(msg, word) {
				t.Errorf("%s has no translation for %q", lang, msg)
			}
		}
	}
}
