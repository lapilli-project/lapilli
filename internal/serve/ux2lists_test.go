package serve

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
)

// TestRulesSeverityAgrees: a rule whose severity is in its definition, and
// not in the ledger, has it in the list, as its page says it; and the
// column is there for it.
func TestRulesSeverityAgrees(t *testing.T) {
	parallel(t)
	s := glanceServer(t)
	v := rulesFor(t, s, "weeks=12")
	var found bool
	for _, r := range v.Rows {
		if r.Name == "Errors" {
			found = true
			if r.Severity != "page" {
				t.Errorf("the list's severity of Errors is %q, its definition's is page", r.Severity)
			}
		}
	}
	if !found {
		t.Fatalf("Errors isn't in the list: %q", rowNames(v.Rows))
	}
	if !v.SevCol {
		t.Error("a rule with a severity, and no Severity column")
	}
	_, body, _ := get(t, s.Handler(), "/rules?weeks=12")
	if !strings.Contains(body, `<th scope="col">Severity</th>`) || !strings.Contains(body, `<td class="name"><bdi>page</bdi></td>`) {
		t.Error("the list doesn't show the severity of its definition")
	}
	_, page, _ := get(t, s.Handler(), "/rules/Errors")
	if !strings.Contains(page, `<span class="k">Severity</span> <bdi>page</bdi>`) {
		t.Error("the rule's page doesn't say the severity")
	}
}

// TestRulesNoSeverityNoColumn: with no rule shown having a severity, the
// table has no Severity column, in the head, the rows or the sum.
func TestRulesNoSeverityNoColumn(t *testing.T) {
	parallel(t)
	if got := (rulesList{SevCol: false}).Cols(); got != 10 {
		t.Errorf("a list without severity has %d columns after the one to pick by, want 10", got)
	}
	if got := (rulesList{SevCol: true}).Cols(); got != 11 {
		t.Errorf("a list with severity has %d columns after the one to pick by, want 11", got)
	}
	// A rule with no severity in its definition or the ledger.
	ctx := context.Background()
	s, rules := server(t, func(c *Config) { c.Backfill, c.CheckReplay = time.Hour, true })
	s.h = &alertsHistory{}
	os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte("groups:\n  - name: shop\n    rules:\n      - {alert: Plain, expr: errors > 5, for: 2m}\n"), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	day := s.l.Midnight(s.l.Day(now) - 3)
	if err := s.l.Record(ctx, ledger.Found{From: day, To: day.Add(24*time.Hour - time.Second),
		Firings: []firing.Firing{{Rule: "Plain", Labels: map[string]string{"service": "search"}, Start: day.Add(3 * time.Hour), End: day.Add(3*time.Hour + 5*time.Minute)}},
		Run:     ledger.Run{Kind: "backfill", Started: day, Finished: day, From: day, To: day.Add(24 * time.Hour)}}, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	v := rulesFor(t, s, "weeks=12")
	if len(v.Rows) == 0 || v.SevCol {
		t.Fatalf("%d rows, column %v: want rows and no column", len(v.Rows), v.SevCol)
	}
	_, body, _ := get(t, s.Handler(), "/rules?weeks=12")
	if strings.Contains(body, `<th scope="col">Severity</th>`) {
		t.Error("a list of rules with no severity has the column")
	}
	if !strings.Contains(body, `<legend>Severity</legend>`) {
		t.Error("the severity's facet is gone")
	}
}

// TestRulesBadgesQuiet: how a rule did is the loud badge; the others,
// what its backtest can't be trusted for, its disposition and what is under
// way, are drawn quiet, with the same signs and words.
func TestRulesBadgesQuiet(t *testing.T) {
	parallel(t)
	s := pagesServer(t)
	_, body, _ := get(t, s.Handler(), "/rules?weeks=12")
	re := regexp.MustCompile(`<(?:span|a) class="badge ([^"]*)"`)
	var perf, quiet int
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		classes := strings.Fields(m[1])
		isQuiet := false
		for _, c := range classes {
			isQuiet = isQuiet || c == "q"
		}
		switch {
		case strings.HasPrefix(m[1], "perf-"):
			perf++
			if isQuiet {
				t.Errorf("the performance badge is quiet: %s", m[1])
			}
		default:
			if strings.Contains(m[1], "trust") || strings.Contains(m[1], "pill") || strings.Contains(m[1], "prog") || strings.Contains(m[1], "check") || strings.Contains(m[1], "expect") {
				quiet++
				if !isQuiet {
					t.Errorf("a badge of another axis is loud: %s", m[1])
				}
			}
		}
	}
	if perf == 0 || quiet == 0 {
		t.Errorf("%d performance badges, %d others: want some of each", perf, quiet)
	}
	for _, want := range []string{`<span class="badge q pill tune status-info"><span aria-hidden="true">±</span> tune</span>`, `<span aria-hidden="true">⇣</span> Recorded only`} {
		if !strings.Contains(body, want) {
			t.Errorf("a quiet badge lost its sign or words: no %s", want)
		}
	}
	css, err := files.ReadFile("static/lapilli.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), ".rules-main .badge.q {") {
		t.Error("no quiet badge in the style sheet")
	}
}

// TestTeamSaidInWords: the team cell says where the team came from in
// words, and the team's pattern in the title when it is from the pattern.
func TestTeamSaidInWords(t *testing.T) {
	parallel(t)
	s := pagesServer(t)
	_, body, _ := get(t, s.Handler(), "/rules?weeks=12")
	if strings.Contains(body, ">by its") {
		t.Error("the team cell still says \"by its\"")
	}
	re := regexp.MustCompile(`<span class="team-by"( title="[^"]*")?>from its ([^<]*)</span>`)
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		seen[m[2]] = true
		if m[2] == "name&#39;s pattern" && m[1] == "" {
			t.Error("a team from a pattern has no title with the pattern")
		}
		if m[2] != "name&#39;s pattern" && m[1] != "" {
			t.Errorf("a team from its %s has a title", m[2])
		}
	}
	if !seen["note"] {
		t.Errorf("no team from its note: %v", seen)
	}
}

// TestIncidentsOnePeriodControl: the bar chooses the period, keeping what
// the page was asked for, and the lede has no second set of links; the
// form to write one down is folded, open when asked for.
func TestIncidentsOnePeriodControl(t *testing.T) {
	parallel(t)
	s := pagesServer(t)
	h := s.Handler()
	_, body, _ := get(t, h, "/incidents?source=written&state=caught&add=1")
	if n := strings.Count(body, ">12 weeks</a>"); n != 1 {
		t.Errorf("the period is chosen with %d sets of links, want the bar's", n)
	}
	for _, w := range []string{"1", "4", "12"} {
		re := regexp.MustCompile(`<a [^>]*href="(/incidents\?[^"]*weeks=` + w + `[^"]*)"`)
		m := re.FindStringSubmatch(body)
		if m == nil {
			t.Errorf("the bar has no link to %s weeks", w)
			continue
		}
		for _, want := range []string{"source=written", "state=caught"} {
			if !strings.Contains(m[1], want) {
				t.Errorf("the bar's link to %s weeks, %s, drops %s", w, m[1], want)
			}
		}
		if strings.Contains(m[1], "add=") {
			t.Errorf("the bar's link to %s weeks carries the form open: %s", w, m[1])
		}
	}
	if !regexp.MustCompile(`<details id="add" class="add" open>`).MatchString(body) {
		t.Error("asked with add=1, the form isn't open")
	}
	_, body, _ = get(t, h, "/incidents?source=written")
	if !regexp.MustCompile(`<details id="add" class="add"><summary>Write one down</summary>`).MatchString(body) {
		t.Error("the form to write one down isn't folded by default")
	}
	m := regexp.MustCompile(`<a class="button" href="([^"]*)">Write one down</a>`).FindStringSubmatch(body)
	if m == nil || !strings.Contains(m[1], "add=1") || !strings.Contains(m[1], "source=written") || !strings.HasSuffix(m[1], "#add") {
		t.Errorf("the main action isn't a link to the form asked open, keeping the rest: %v", m)
	}
	// A reader who may not write sees the action disabled, with why.
	ro, _ := server(t, func(c *Config) { c.ReadOnly = true })
	_, body, _ = get(t, ro.Handler(), "/incidents")
	if !strings.Contains(body, `<button type="button" disabled aria-describedby="action-why">Write one down</button>`) || strings.Contains(body, `id="add"`) {
		t.Error("a read-only reader's action isn't the disabled one, or the form is there")
	}
}

// TestProposalsFiltersFolded: the filters are in a details, closed unless a
// filter but the default is in effect, the chips of those in effect under it.
func TestProposalsFiltersFolded(t *testing.T) {
	parallel(t)
	_, h, _ := withProposal(t, nil)
	_, body, _ := get(t, h, "/proposals")
	if !strings.Contains(body, `<details class="pick-menu"><summary>Filters</summary>`) {
		t.Error("the proposals' filters aren't folded by default")
	}
	_, body, _ = get(t, h, "/proposals?kind=for")
	if !strings.Contains(body, `<details class="pick-menu" open><summary>Filters <span class="quiet">(1)</span></summary>`) {
		t.Error("with a filter in effect, the filters aren't open")
	}
	if !strings.Contains(body, `<ul class="chips"`) {
		t.Error("with a filter in effect, no chips say so")
	}
	if !strings.Contains(body, `class="opt zero"`) && !strings.Contains(body, `class="opt"`) {
		t.Error("the filters' values are gone")
	}
}

// TestForIsTheKeyword: the change of a "for" is written with the word
// quoted, as the keyword it is, in the kinds, the bulk options and the
// proposal's heading.
func TestForIsTheKeyword(t *testing.T) {
	parallel(t)
	if got := kindText("for"); got != `a new "for"` {
		t.Errorf("a for's kind is %q", got)
	}
	_, h, _ := withProposal(t, nil)
	_, body, _ := get(t, h, "/proposals")
	if !strings.Contains(body, `a new &#34;for&#34;`) || strings.Contains(body, "a new for") {
		t.Error("the kind of a new for isn't quoted on the proposals list")
	}
	for _, lang := range []string{"ko", "ja", "es"} {
		for _, k := range []string{`a new "for"`, `A new "for"`, `the "for" suggested for it`} {
			if catalogs[lang][k] == "" {
				t.Errorf("%s has no translation for %q", lang, k)
			}
		}
		if strings.Contains(catalogs[lang][`a new "for"`], "\"for\"") == false {
			t.Errorf("%s writes the kind without the quoted keyword: %q", lang, catalogs[lang][`a new "for"`])
		}
	}
}

// TestManageHasSources: the Administration page has a card for Sources, in
// the sidebar's order, between Settings and Run history.
func TestManageHasSources(t *testing.T) {
	parallel(t)
	s := pagesServer(t)
	code, body, _ := get(t, s.Handler(), "/manage")
	if code != http.StatusOK {
		t.Fatalf("/manage: %d", code)
	}
	var hrefs []string
	for _, m := range regexp.MustCompile(`<li class="card">\s*<h2><a href="([^"]+)">`).FindAllStringSubmatch(body, -1) {
		hrefs = append(hrefs, m[1])
	}
	at := func(h string) int {
		for i, x := range hrefs {
			if strings.HasPrefix(x, h) {
				return i
			}
		}
		return -1
	}
	if at("/sources") < 0 || at("/sources") != at("/settings")+1 || at("/runs") != at("/sources")+1 {
		t.Errorf("the cards are %v: Sources isn't between Settings and Run history", hrefs)
	}
}
