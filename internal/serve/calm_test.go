package serve

// Plain controls and fewer words (docs/design/ui.md, bundle 59): the mark,
// the controls' one shape, what the overview shows first, a rule's numbers
// alone in their cells, a limit of the record said in a line.

import (
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// TestBrandMark: the frame's name is the logo's micro-mark beside the word,
// its image unnamed and sized; the tab's icon is the mark at 16 and 32 px;
// the two circles drawn for it before are gone.
func TestBrandMark(t *testing.T) {
	parallel(t)
	s, _ := server(t, nil)
	h := s.Handler()
	_, body, _ := get(t, h, "/")
	for _, want := range []string{
		`<link rel="icon" href="/static/mark-32.png?v=` + staticSum + `" sizes="32x32" type="image/png">`,
		`<link rel="icon" href="/static/mark-16.png?v=` + staticSum + `" sizes="16x16" type="image/png">`,
		`<a class="brand" href="/"><img src="/static/mark-48.png?v=` + staticSum + `" alt="" width="24" height="24">Lapilli</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the frame lacks %s", want)
		}
	}
	if strings.Contains(body, "icon.svg") {
		t.Error("the frame still names icon.svg")
	}
	for _, name := range []string{"mark-16.png", "mark-32.png", "mark-48.png"} {
		if code, _, hd := get(t, h, "/static/"+name+"?v="+staticSum); code != http.StatusOK || hd.Get("Content-Type") != "image/png" {
			t.Errorf("%s: %d %s", name, code, hd.Get("Content-Type"))
		}
	}
	if code, _, _ := get(t, h, "/static/icon.svg"); code != http.StatusNotFound {
		t.Errorf("icon.svg: %d, want gone", code)
	}
}

// TestControlsOneShape: a field, a select and a button have one corner and
// one height, and nothing to press is a pill; a tab is underlined by a
// bar, not filled; no card has a shadow under it.
func TestControlsOneShape(t *testing.T) {
	parallel(t)
	b, err := files.ReadFile("static/lapilli.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	rule := func(sel string) string {
		i := strings.Index(css, "\n"+sel+" {")
		if i < 0 {
			t.Fatalf("no rule for %s", sel)
		}
		j := strings.Index(css[i:], "}")
		return css[i : i+j]
	}
	for _, sel := range []string{"button", "a.button", "input, select, textarea", ".nav-side a, .nav-menu a", "details.menu > summary"} {
		r := rule(sel)
		if !strings.Contains(r, "border-radius: var(--control-r)") || strings.Contains(r, "9999px") {
			t.Errorf("%s: %s", sel, r)
		}
	}
	for _, sel := range []string{"button", "a.button", "details.menu > summary"} {
		if !strings.Contains(rule(sel), "min-height: var(--control-h)") {
			t.Errorf("%s isn't a control's height", sel)
		}
	}
	for _, sel := range []string{".tabs a[aria-current]", "nav.ruletabs a[aria-current]"} {
		if r := rule(sel); !strings.Contains(r, "box-shadow: inset 0 -2px var(--accent)") || strings.Contains(r, "background") {
			t.Errorf("%s: %s", sel, r)
		}
	}
	for _, sel := range []string{".tile", ".card", "dl.panel > div", "figure.strip"} {
		if strings.Contains(rule(sel), "box-shadow") {
			t.Errorf("%s has a shadow", sel)
		}
	}
	if strings.Contains(rule(".bar"), "border:") || strings.Contains(rule(".bar"), "background") {
		t.Errorf("the bar is drawn as a card: %s", rule(".bar"))
	}
}

// TestOverviewLookFirst: what to look at is under the tiles, before the
// charts; no link on the overview has an arrow after it.
func TestOverviewLookFirst(t *testing.T) {
	parallel(t)
	s := pagesServer(t)
	_, body, _ := get(t, s.Handler(), "/")
	tiles, look, charts := strings.Index(body, `<section class="tiles`), strings.Index(body, `id="look-h"`), strings.Index(body, "Firings by week")
	if tiles < 0 || look < tiles || charts < look {
		t.Errorf("the tiles at %d, what to look at at %d, the charts at %d", tiles, look, charts)
	}
	if regexp.MustCompile(`→\s*(</span>\s*)?</a>`).MatchString(body) {
		t.Error("a link on the overview ends in an arrow")
	}
	if strings.Contains(body, "Each firing gets a stone") || strings.Contains(body, "The share of firings that caught something.") {
		t.Error("the overview says again what its legends and headers say")
	}
}

// TestRuleNumbersAlone: a rule's row holds its numbers alone, and what
// they come with is a line under it, as wide as the row, in tbody and
// tfoot, with the columns there are; with firings not judged, they have a
// column of their own, not a note under ● Nothing, and the firings
// asleep among those that caught nothing aren't said to be all judged.
func TestRuleNumbersAlone(t *testing.T) {
	parallel(t)
	notes := regexp.MustCompile(`<tr class="more">(?:<td class="pick"></td>)?<td colspan="(\d+)"><p class="row-notes">`)
	cellNotes := regexp.MustCompile(`<td class="n">[^<]*<br>`)
	// spans checks each line of notes spans every column but the pick's,
	// as the header has them, and says how many lines there are.
	spans := func(t *testing.T, body string) int {
		t.Helper()
		head := body[strings.Index(body, `<table class="rules-table">`):]
		head = head[:strings.Index(head, "</thead>")]
		cols := strings.Count(head, "<th ")
		if strings.Contains(head, `class="pick"`) {
			cols-- // the line starts after the column to pick a rule by
		}
		ms := notes.FindAllStringSubmatch(body, -1)
		for _, m := range ms {
			if m[1] != strconv.Itoa(cols) {
				t.Errorf("a line of notes spans %s columns of %d", m[1], cols)
			}
		}
		if cellNotes.MatchString(body) {
			t.Error("a number's cell holds its notes")
		}
		return len(ms)
	}
	t.Run("judged", func(t *testing.T) {
		s := pagesServer(t)
		s.cfg.RecordByWeek = true
		_, body, _ := get(t, s.Handler(), "/rules?weeks=12")
		if spans(t, body) == 0 || !strings.Contains(body, "re-fired: ") {
			t.Fatal("no line of notes under a row")
		}
		if !strings.Contains(body, `<p class="row-notes"><span class="vh"><bdi>`) {
			t.Error("a line of notes doesn't name its rule to a screen reader")
		}
		if strings.Contains(body, `<th scope="col" class="n">? Not judged</th>`) {
			t.Error("a column of firings not judged, with none")
		}
	})
	t.Run("not judged", func(t *testing.T) {
		ctx := context.Background()
		s, _ := server(t, func(c *Config) { c.Options.Hours = verdict.DefaultHours(time.UTC) })
		s.cfg.RecordByWeek = true
		raw := ""
		if err := s.look(ctx, &raw); err != nil {
			t.Fatal(err)
		}
		s.jobs(ctx, true)
		_, body, _ := get(t, s.Handler(), "/rules?weeks=12")
		if !strings.Contains(body, `<th scope="col" class="n">? Not judged</th>`) {
			t.Error("firings not judged have no column")
		}
		if strings.Contains(body, " not judged</span>") {
			t.Error("firings not judged are a note under ● Nothing")
		}
		spans(t, body)
		if !strings.Contains(body, "caught nothing or not judged: ") {
			t.Error("the firings asleep of a rule with some not judged are said to have caught nothing")
		}
	})
}

// TestNoRecordInALine: a limit of the record is said in a line, once, with
// where to write incidents down; its why is behind "Why".
func TestNoRecordInALine(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, _ := server(t, nil)
	s.cfg.RecordByWeek = true
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	why := `<details class="why"><summary>Why</summary><p>A firing is judged by the incidents written down around it: those of the day or week it fell in, and of the two weeks before.`
	for path, line := range map[string]string{
		"/":               "No incident written down for the week of ",
		"/rules?weeks=12": "1 of the 12 weeks shown had no incident written down for them or the two weeks before: their firings are counted, not judged.",
	} {
		_, body, _ := get(t, s.Handler(), path)
		if strings.Count(body, line) != 1 || strings.Count(body, why) != 1 || !strings.Contains(body, `href="/incidents`) {
			t.Errorf("%s: the line %d times, why %d times", path, strings.Count(body, line), strings.Count(body, why))
		}
		if strings.Contains(body, "can&#39;t be told from one nobody wrote down, so") {
			t.Errorf("%s says why in the line", path)
		}
	}
	// A rule's page, in a ledger with no incident at all.
	none := filepath.Join(t.TempDir(), "incidents.yaml")
	if err := os.WriteFile(none, []byte("incidents: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bare, _ := server(t, func(c *Config) { c.Incidents, c.Impact = none, "" })
	bare.cfg.RecordByWeek = true
	if err := bare.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	bare.jobs(ctx, true)
	_, body, _ := get(t, bare.Handler(), "/rules/Errors")
	if line := "No incident written down for the weeks it fired or the two weeks before: its precision isn&#39;t known, and it isn&#39;t called noisy."; strings.Count(body, line) != 1 || strings.Count(body, why) != 1 {
		t.Errorf("a rule's page: the line %d times, why %d times", strings.Count(body, line), strings.Count(body, why))
	}
}

// TestPrecisionChartNone: with no week to draw a precision for, the chart
// is a line saying so, with the way to the table, not an empty grid; with
// one, the chart as it was.
func TestPrecisionChartNone(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	none := filepath.Join(t.TempDir(), "incidents.yaml")
	if err := os.WriteFile(none, []byte("incidents: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, _ := server(t, func(c *Config) { c.Incidents, c.Impact = none, "" })
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	_, body, _ := get(t, s.Handler(), "/")
	if !strings.Contains(body, `<p class="chart-none"><span class="status-unknown"><span aria-hidden="true">?</span> No week has judged firings, so there is no precision to draw.</span> <a href="#by-week">By week</a></p>`) ||
		strings.Contains(body, "Precision by week; the same numbers are in the table below") {
		t.Error("no precision to draw, and the chart is drawn empty")
	}
	ps := pagesServer(t)
	_, body, _ = get(t, ps.Handler(), "/")
	if strings.Contains(body, `class="chart-none"`) || !strings.Contains(body, "Precision by week; the same numbers are in the table below") {
		t.Error("a precision to draw, and no chart")
	}
	// No firing at all isn't no record: said in no status's colour.
	if _, err := ps.l.PutTeam(ctx, ledger.Team{Name: "quiet", Rules: "^NothingIsCalledThis$"}, "ana"); err != nil {
		t.Fatal(err)
	}
	_, body, _ = get(t, ps.Handler(), "/?team=quiet")
	if !strings.Contains(body, `<p class="chart-none">No firing in these weeks, so there is no precision to draw. <a href="#by-week">By week</a></p>`) {
		t.Error("no firing, and it is said as none judged")
	}
}

// TestRefiredToldNotJudged: more re-fires than the week before are told,
// not called worse, nor fewer better: one may be an alert that really
// went off again.
func TestRefiredToldNotJudged(t *testing.T) {
	parallel(t)
	ws := []*weekView{{Refired: 1, Kept: true}, {Refired: 5, Kept: true}, {}}
	for _, x := range (&Server{}).tiles("en", ws, ws[1], ws[0], "", "", "", false, func() (int, int) { return 0, 0 }) {
		if x.key == "Refired" {
			if x.Delta != "+4 on the week before" || x.Worse || x.Better {
				t.Errorf("refired: %+v", x)
			}
			return
		}
	}
	t.Fatal("no Refired tile")
}

// TestRuleColumnRoom: a rule's label chip breaks after its "=", not inside
// a word, and the line of notes under a row doesn't widen the columns it
// spans (docs/design/ui.md, "The table fits a laptop's screen").
func TestRuleColumnRoom(t *testing.T) {
	parallel(t)
	s := pagesServer(t)
	_, body, _ := get(t, s.Handler(), "/rules?weeks=12")
	if !regexp.MustCompile(`<a class="chip label" href="[^"]*"><span class="vh">Only those with the label </span><bdi>[^<=]+=<wbr>[^<]+</bdi></a>`).MatchString(body) {
		t.Error("a label chip doesn't break after its =")
	}
	b, err := files.ReadFile("static/lapilli.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	i := strings.Index(css, "\np.row-notes {")
	if i < 0 {
		t.Fatal("no rule for p.row-notes")
	}
	if r := css[i : i+strings.Index(css[i:], "}")]; !strings.Contains(r, "width: 0;") || !strings.Contains(r, "min-width: 100%") {
		t.Errorf("the notes widen the columns: %s", r)
	}
}

// TestTeamSaidHow: the rules list says, under a rule's team, how it was
// found, as the rule's page does: by its note, its name or its labels
// (docs/design/ui.md, "Why a rule is a team's").
func TestTeamSaidHow(t *testing.T) {
	parallel(t)
	s := pagesServer(t)
	_, body, _ := get(t, s.Handler(), "/rules?weeks=12")
	// Errors is payments' by its note, which comes before the team's
	// pattern, as owners finds it.
	if !strings.Contains(body, `<td class="name"><bdi>payments</bdi><span class="team-by">by its note</span></td>`) {
		t.Error("a team given by a rule's note isn't said so")
	}
	if strings.Contains(body, `<td class="name">–<span class="team-by">`) {
		t.Error("a rule of no team is said how its team was found")
	}
}

// TestNarrowRulesTable: narrower than 1200 CSS px the rules list leaves
// out its 12 weeks' column, which each rule's page has, so that the rule's
// column has room; the column is marked in the head, each row and the sum.
func TestNarrowRulesTable(t *testing.T) {
	parallel(t)
	b, err := files.ReadFile("static/lapilli.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	i := strings.Index(css, "@media (max-width: 1199.98px) {\n  .rules-main table .wk12 { display: none; }")
	if i < 0 {
		t.Fatal("the 12 weeks' column isn't left out below 1200 px")
	}
	s := pagesServer(t)
	_, body, _ := get(t, s.Handler(), "/rules?weeks=12")
	rows := strings.Count(body, `<td class="wk12"><svg class="spark"`)
	if !strings.Contains(body, `<th scope="col" class="wk12">12 weeks</th>`) || rows == 0 || !strings.Contains(body, `<td></td><td class="wk12"></td></tr>`) {
		t.Errorf("the 12 weeks' column isn't marked: %d rows", rows)
	}
}

// TestLookSaysWhatToCheck: What to look at's lines say what to check, not
// ask it, in every language (docs/design/ui.md, "Times, and Korean").
func TestLookSaysWhatToCheck(t *testing.T) {
	parallel(t)
	first, _ := firstUseServer(t)
	// A rule another catches each incident of as soon: the spare rule's
	// line (TestSpareOnlyOfWhatCanFire's ledger).
	spare, rules := server(t, func(c *Config) {
		c.Backfill = 28 * 24 * time.Hour
		c.Options.NoisyFirings = 1000
	})
	os.WriteFile(filepath.Join(rules, "more.yaml"), []byte("groups:\n  - name: more\n    rules:\n      - {alert: Errors2, expr: errors > 1, for: 2m, labels: {severity: page}}\n"), 0o644)
	ctx := context.Background()
	raw := ""
	if err := spare.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	spare.jobs(ctx, true)
	for _, in := range []string{"I-1", "I-2"} {
		if code, body := api(t, spare.Handler(), "PUT", "/api/v1/incidents/"+in, `{"title": "x", "start": "2026-11-02T06:01:00Z", "labels": {"service": ["checkout"]}}`); code != http.StatusOK {
			t.Fatalf("incident: %d %s", code, body)
		}
	}
	line := regexp.MustCompile(`(?s)<span class="look-what">(.*?)</span></span>`)
	tags := regexp.MustCompile(`<[^>]*>`)
	for name, s := range map[string]*Server{"first use": first, "a spare rule": spare} {
		for _, n := range names {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("Accept-Language", n.Code)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			body := w.Body.String()
			ms := line.FindAllStringSubmatch(body, -1)
			if len(ms) == 0 {
				t.Fatalf("%s, %s: nothing to look at", name, n.Code)
			}
			if name == "a spare rule" && !strings.Contains(body, `href="/rules/Errors2`) {
				t.Fatalf("%s: no line for the spare rule", n.Code)
			}
			for _, m := range ms {
				// The words read, not a link's query.
				if words := tags.ReplaceAllString(m[1], ""); strings.ContainsAny(words, "?？¿") {
					t.Errorf("%s, %s: asks: %s", name, n.Code, words)
				}
			}
		}
	}
}

// TestImpactTitleSaid: an impact window of the unnamed signal is "user
// impact" on the pages, its flag left out; the API gives its title as it
// was kept.
func TestImpactTitleSaid(t *testing.T) {
	parallel(t)
	s := pagesServer(t)
	_, page, _ := get(t, s.Handler(), "/incidents")
	if !strings.Contains(page, "user impact") || strings.Contains(page, "(--impact)") {
		t.Error("the incidents page names the flag")
	}
	_, api, _ := get(t, s.Handler(), "/api/v1/incidents")
	if !strings.Contains(api, "user impact (--impact)") {
		t.Error("the API changed the title kept")
	}
	// Searched for by the title as kept, it is found still.
	for _, q := range []string{"--impact", "user impact"} {
		if _, found, _ := get(t, s.Handler(), "/search?q="+url.QueryEscape(q)); !strings.Contains(found, `href="/incidents/impact-`) {
			t.Errorf("searched for %q: no impact window", q)
		}
	}
}

// TestConfirmKeepsTitle: an impact window confirmed with the title its form
// was filled with, the page's words in the reader's language, is written
// down with the window's own; one written in its place is kept as written.
func TestConfirmKeepsTitle(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, _ := server(t, func(c *Config) {
		c.UserHeader, c.Admins, c.Editors = "X-Forwarded-Email", []string{"ana@example.com"}, []string{"bo@example.com"}
		c.Impact = "impact > 1"
	})
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	s.ready.Store(true)
	ws, err := s.l.ImpactWindows(ctx, now.Add(-30*24*time.Hour), now)
	if err != nil || len(ws) < 2 {
		t.Fatalf("windows: %v %v", ws, err)
	}
	confirm := func(id, lang, title string) {
		t.Helper()
		form := url.Values{"state": {"confirmed"}, "title": {title}}
		r := httptest.NewRequest(http.MethodPost, "/incidents/"+id+"/decide", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("X-Forwarded-Email", "bo@example.com")
		r.Header.Set("Accept-Language", lang)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("confirm %s: %d %s", id, w.Code, w.Body.String())
		}
	}
	confirm(ws[0].ID, "ko", tr("ko", "user impact"))
	confirm(ws[1].ID, "en", "Checkout down")
	all, err := s.l.Incidents(ctx, now.Add(-30*24*time.Hour), now, true)
	if err != nil {
		t.Fatal(err)
	}
	titles := map[string]bool{}
	for _, in := range all {
		if in.Origin != "impact" {
			titles[in.Title] = true
		}
	}
	if !titles[impactTitle] || !titles["Checkout down"] || titles[tr("ko", "user impact")] {
		t.Errorf("written down as %v", titles)
	}
	// The incident confirmed is shown as the window was, and saved from
	// its form unchanged but for a label, keeps its title.
	var made ledger.Incident
	for _, in := range all {
		if in.Origin != "impact" && in.Title == impactTitle {
			made = in
		}
	}
	for _, lang := range []string{"en", "ko"} {
		r := httptest.NewRequest(http.MethodGet, "/incidents/"+made.ID, nil)
		r.Header.Set("Accept-Language", lang)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if body := w.Body.String(); w.Code != http.StatusOK || strings.Contains(body, "(--impact)") || !strings.Contains(body, tr(lang, "user impact")) {
			t.Errorf("%s: %s, %d, says the flag or not its title", made.ID, lang, w.Code)
		}
	}
	form := url.Values{"id": {made.ID}, "title": {tr("ko", "user impact")}, "start": {made.Start.UTC().Format("2006-01-02T15:04")}, "labels": {"service=search"}}
	r := httptest.NewRequest(http.MethodPost, "/incidents", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("X-Forwarded-Email", "bo@example.com")
	r.Header.Set("Accept-Language", "ko")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("save %s: %d %s", made.ID, w.Code, w.Body.String())
	}
	if in, err := s.l.IncidentByID(ctx, made.ID); err != nil || in.Title != impactTitle || in.Labels["service"][0] != "search" {
		t.Errorf("saved as %+v %v", in, err)
	}
}

// TestFlagsAsCode: a flag's name in a page's words is code, in every
// language, on every page the tests serve and on a first-time user's; in
// the pages' font two hyphens read as one dash (docs/design/ui.md).
func TestFlagsAsCode(t *testing.T) {
	parallel(t)
	first, firstPaths := firstUseServer(t)
	// With people's verdicts, one to look at again on the Cleanup page.
	pages := pagesServer(t)
	peopleSay(t, pages)
	skip := regexp.MustCompile(`(?s)<code[^>]*>.*?</code>|<pre[^>]*>.*?</pre>|<textarea[^>]*>.*?</textarea>|<title>.*?</title>|<style[^>]*>.*?</style>|<[^>]*>`)
	flag := regexp.MustCompile(`(^|[^\w-])--[a-z][a-z0-9-]*`)
	seen := map[string]bool{}
	for _, c := range []struct {
		s     *Server
		paths []string
	}{{pages, append(slices.Clone(pagePaths), "/orphans")}, {first, firstPaths}} {
		for _, n := range names {
			for _, p := range c.paths {
				r := httptest.NewRequest(http.MethodGet, p, nil)
				r.Header.Set("Accept-Language", n.Code)
				w := httptest.NewRecorder()
				c.s.Handler().ServeHTTP(w, r)
				text := skip.ReplaceAllString(w.Body.String(), "\x00")
				for _, part := range strings.Split(text, "\x00") {
					if m := flag.FindString(part); m != "" {
						key := n.Code + " " + strings.TrimSpace(part)
						if !seen[key] {
							seen[key] = true
							t.Errorf("%s %s: a flag not as code: %q", n.Code, p, strings.TrimSpace(part))
						}
					}
				}
			}
		}
	}
}

// plainMarks is the <code> a page adds around a flag's name (codeFlags);
// a value's <bdi> is left, to be checked as it was.
var plainMarks = regexp.MustCompile(`<code>(--[a-z][a-z0-9-]*)</code>`)

// containsPlain is strings.Contains of a page and the words it should say,
// a flag's name plain in each (plainMarks).
func containsPlain(s, sub string) bool {
	plain := func(x string) string {
		return plainMarks.ReplaceAllString(x, "$1")
	}
	return strings.Contains(plain(s), plain(sub))
}

// TestTbdiWithoutArguments: tbdi with no arguments gives the translation
// as t does, its verbs as they are, its flags as code: a disabled
// button's why of a team not found isn't Sprintf's %!s(MISSING).
func TestTbdiWithoutArguments(t *testing.T) {
	parallel(t)
	if got := string(trBDI("en", "Only %s's editors")); got != "Only %s&#39;s editors" {
		t.Errorf("no arguments: %q", got)
	}
	if got := string(trBDI("en", "Read-only: lapilli serve was started with --read-only")); got != "Read-only: lapilli serve was started with <code>--read-only</code>" {
		t.Errorf("a flag: %q", got)
	}
	if got := string(trBDI("en", "Those --approvers names: %s.", "a@example.com")); got != "Those <code>--approvers</code> names: <bdi>a@example.com</bdi>." {
		t.Errorf("a flag and an argument: %q", got)
	}
}

// TestRefusalFlagsAsCode: a refusal or a failure said on a page, already
// translated, sets each flag's name in it as code, as a translation's
// are (bundle 67); and a flag's name in a URL's path or query stays text.
func TestRefusalFlagsAsCode(t *testing.T) {
	parallel(t)
	s, _ := changesServer(t, func(c *Config) { c.Changes = false })
	h := s.Handler()
	id := proposeFor(t, h, "15m")
	code, body, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/approve", ana, "same-origin", url.Values{})
	if code != http.StatusBadRequest || !strings.Contains(body, "<p>changes are off: lapilli serve was started without <code>--changes</code></p>") {
		t.Errorf("the refusal: %d %.300s", code, body[max(0, strings.Index(body, "<h1>")):])
	}
	for in, want := range map[string]string{
		"read within --timeout and --max-samples": "read within <code>--timeout</code> and <code>--max-samples</code>",
		"<b>--keep</b>":                         "&lt;b&gt;<code>--keep</code>&lt;/b&gt;",
		"at https://x.example/a/--b?c=--d, --e": "at https://x.example/a/--b?c=--d, <code>--e</code>",
	} {
		if got := string(flagsAsCode(in)); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// TestSentencesSetFlagsAsCode: a page's sentence naming a flag is said
// with tbdi, which sets the flag as code, not with t, whose text the
// template escapes as it is: --config read as "–config" in the pages'
// font in eight sentences of 0.64's routes, boundaries and activity log
// (bundle 67).
func TestSentencesSetFlagsAsCode(t *testing.T) {
	parallel(t)
	said := regexp.MustCompile(`\{\{-?\s*t "((?:[^"\\]|\\.)*)"`)
	pages, err := fs.Glob(files, "templates/*.html")
	if err != nil || len(pages) == 0 {
		t.Fatalf("templates: %v %v", pages, err)
	}
	checked := 0
	for _, f := range pages {
		b, err := fs.ReadFile(files, f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range said.FindAllStringSubmatch(string(b), -1) {
			checked++
			if flagName.MatchString(m[1]) {
				t.Errorf("%s: %q names a flag: say it with tbdi", f, m[1])
			}
		}
	}
	if checked < 100 {
		t.Fatalf("read only %d sentences said with t", checked)
	}
}
