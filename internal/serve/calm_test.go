package serve

// Plain controls and fewer words (docs/design/ui.md, bundle 59): the mark,
// the controls' one shape, what the overview shows first, a rule's numbers
// alone in their cells, a limit of the record said in a line.

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/verdict"
)

// TestBrandMark: the frame's name is the logo's micro-mark beside the word,
// its image unnamed and sized; the tab's icon is the mark at 16 and 32 px;
// the two circles drawn for it before are gone.
func TestBrandMark(t *testing.T) {
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
// they come with is a line under it, as wide as the row; with firings not
// judged, they have a column of their own, not a note under ● Nothing.
func TestRuleNumbersAlone(t *testing.T) {
	notes := regexp.MustCompile(`<tr class="more"><td class="pick"></td><td colspan="(\d+)"><p class="row-notes">`)
	cellNotes := regexp.MustCompile(`<td class="n">[^<]*<br>`)
	t.Run("judged", func(t *testing.T) {
		s := pagesServer(t)
		_, body, _ := get(t, s.Handler(), "/rules?weeks=12")
		if cellNotes.MatchString(body) {
			t.Error("a number's cell holds its notes")
		}
		m := notes.FindStringSubmatch(body)
		if m == nil || !strings.Contains(body, "re-fired: ") {
			t.Fatal("no line of notes under a row")
		}
		v := rulesFor(t, s, "weeks=12")
		if want := (rulesList{rulesView: rulesView{Deploys: v.Deploys}, Sum: v.Sum}).Cols(); m[1] != strconv.Itoa(want) {
			t.Errorf("the notes span %s columns, want %d", m[1], want)
		}
	})
	t.Run("not judged", func(t *testing.T) {
		ctx := context.Background()
		s, _ := server(t, func(c *Config) { c.Options.Hours = verdict.DefaultHours(time.UTC) })
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
	})
}

// TestNoRecordInALine: a limit of the record is said in a line, once, with
// where to write incidents down; its why is behind "Why".
func TestNoRecordInALine(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, nil)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	why := `<details class="why"><summary>Why</summary><p>A firing is judged by the incidents written down in its week and the two weeks before.`
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
}

// TestPrecisionChartNone: with no week to draw a precision for, the chart
// is a line saying so, with the way to the table, not an empty grid; with
// one, the chart as it was.
func TestPrecisionChartNone(t *testing.T) {
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
	_, body, _ = get(t, pagesServer(t).Handler(), "/")
	if strings.Contains(body, `class="chart-none"`) || !strings.Contains(body, "Precision by week; the same numbers are in the table below") {
		t.Error("a precision to draw, and no chart")
	}
}

// TestRefiredToldNotJudged: more re-fires than the week before are told,
// not called worse, nor fewer better: one may be an alert that really
// went off again.
func TestRefiredToldNotJudged(t *testing.T) {
	ws := []*weekView{{Refired: 1, Kept: true}, {Refired: 5, Kept: true}, {}}
	for _, x := range (&Server{}).tiles("en", ws, ws[1], ws[0], "", "", false, func() (int, int) { return 0, 0 }) {
		if x.key == "Refired" {
			if x.Delta != "+4 on the week before" || x.Worse || x.Better {
				t.Errorf("refired: %+v", x)
			}
			return
		}
	}
	t.Fatal("no Refired tile")
}
