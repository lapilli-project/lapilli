package verdict

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/rules"
)

// comparison is TestCompare's: a rule slowed, one taken away, one added,
// two incidents caught differently.
func comparison(t *testing.T) *Comparison {
	t.Helper()
	incs := []incident.Incident{
		{ID: "INC-1", Title: "Checkout errors", Start: m(100), End: m(140)},
		{ID: "INC-2", Title: "Search slow", Start: m(2000), End: m(2020)},
	}
	page, ticket := map[string]string{"severity": "page"}, map[string]string{"severity": "ticket"}
	before := Judge(
		[]rules.Rule{{Name: "CheckoutErrors", Expr: "x", Labels: page}, {Name: "Flappy", Expr: "y", Labels: ticket}, {Name: "Typo", Unsupported: "query failed: bad_data"}},
		[]firing.Firing{
			{Rule: "CheckoutErrors", Labels: page, Start: m(102), End: m(130)},
			{Rule: "Flappy", Labels: ticket, Start: m(10), End: m(12)},
			{Rule: "Flappy", Labels: ticket, Start: m(40), End: m(42)},
			{Rule: "Flappy", Labels: ticket, Start: m(500), End: m(502)},
		}, incs, opts())
	after := Judge(
		[]rules.Rule{{Name: "CheckoutErrors", Expr: "x", For: 20 * time.Minute, Labels: page}, {Name: "SearchLatency", Expr: "w"}, {Name: "Typo", Expr: "z"}},
		[]firing.Firing{
			{Rule: "CheckoutErrors", Labels: page, Start: m(120), End: m(130)},
			{Rule: "SearchLatency", Start: m(2005), End: m(2015)},
		}, incs, opts())
	return Compare(before, after, []string{"CheckoutErrors", "Flappy", "SearchLatency", "Typo"}, 40)
}

func TestCompareMarkdown(t *testing.T) {
	seoul, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Skip("no zone database here")
	}
	c := comparison(t)
	c.Summarize(true)
	var b bytes.Buffer
	if err := CompareMarkdown(&b, c, seoul); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	first, _, _ := strings.Cut(out, "\n")
	var s Summary
	if !strings.HasPrefix(first, SummaryLine) || !strings.HasSuffix(first, ")") ||
		json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(first, SummaryLine), ")")), &s) != nil {
		t.Fatalf("first line %q", first)
	}
	if s != *c.Summary || s.NothingChanged || s.Changed != 2 || s.Added != 1 || s.Removed != 1 || s.Same != 40 || s.Unreplayed != 1 ||
		s.FiringsBefore != 4 || s.FiringsAfter != 2 || !s.IncidentsGiven || s.Incidents != 2 || s.IncidentsChanged != 2 || s.Alike != 1 {
		t.Errorf("summary %+v", s)
	}
	for _, want := range []string{
		"### Lapilli backtest, 2026-10-26 09:00 → 2026-11-02 09:00 Asia/Seoul (+09:00)\n",
		"Rules compared: **2 changed, 1 added, 1 taken away**, 40 the same.",
		"| | Before | After |\n|---|--:|--:|\n| Firings | 4 | 2 |\n| no severity | 0 | 1 |\n| Severity `page` | 1 | 1 |\n" +
			"| Severity `ticket` | 3 | 0 |\n| Incidents with no alert, of 2 | 1 | 0 |\n",
		"#### Rules that differ\n\n| Rule | Severity | Fired | ○ Caught | ● Nothing | Precision | Refired |\n| --- | --- | --: | --: | --: | --: | --: |\n",
		"| `Flappy` | `ticket` → – | 3 → – | 0 → – | 3 → – | 0% → – | 1 → – |\n",
		"| `SearchLatency` | – → – | – → 1 | – → 1 | – → 0 | – → 100% | – → 0 |\n",
		"| `Typo` | – | ? → 0 | ? → 0 | ? → 0 | ? → – | ? → 0 |\n",
		"#### Could not replay 1 rules\n\n- `Typo`, before: `query failed: bad_data`\n",
		"<details><summary>1 changed rules keep the same stones</summary>\n\n- `CheckoutErrors`\n\n</details>\n",
		"#### Incidents caught differently\n\n| Incident | Start | Before | After |\n| --- | --- | --- | --- |\n" +
			"| `INC-1` `Checkout errors` | 2026-10-26 10:40 | `CheckoutErrors` +2m | `CheckoutErrors` +20m |\n" +
			"| `INC-2` `Search slow` | 2026-10-27 18:20 | ✗ nothing fired | `SearchLatency` +5m |\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown lacks %q:\n%s", want, out)
		}
	}
	checkMarkdown(t, out, nil)

	// Without incidents: counted, not judged.
	c = comparison(t)
	for _, r := range []*Report{c.Before, c.After} {
		r.Incidents = nil
	}
	c.Incidents = nil
	c.Summarize(false)
	b.Reset()
	if err := CompareMarkdown(&b, c, time.UTC); err != nil {
		t.Fatal(err)
	}
	out = b.String()
	for _, want := range []string{
		"### Lapilli backtest, 2026-10-26 00:00 → 2026-11-02 00:00 UTC\n",
		"No incidents were given: the firings are counted, not judged.",
		"| Rule | Severity | Fired | Refired |\n| --- | --- | --: | --: |\n",
		"| `Flappy` | `ticket` → – | 3 → – | 1 → – |\n",
		"<details><summary>1 changed rules fire as they did</summary>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("without incidents, markdown lacks %q:\n%s", want, out)
		}
	}
	for _, not := range []string{"Caught", "Nothing", "Precision", "ncidents with", "Incidents caught", "stones"} {
		if strings.Contains(out, not) {
			t.Errorf("without incidents, markdown has %q:\n%s", not, out)
		}
	}
	if !strings.Contains(out, `"incidents_given":false,"incidents":0`) {
		t.Errorf("summary line: %s", out)
	}

	// Nothing changed: short, and always the same.
	none := &Comparison{From: t0, To: m(60), Same: 42, Rules: []RuleChange{}, Incidents: []IncidentChange{}}
	none.Summarize(false)
	b.Reset()
	if err := CompareMarkdown(&b, none, time.UTC); err != nil {
		t.Fatal(err)
	}
	want := SummaryLine + `{"nothing_changed":true,"changed":0,"added":0,"removed":0,"same":42,"alike":0,"unreplayed":0,` +
		`"firings_before":0,"firings_after":0,"incidents_given":false,"incidents":0,"incidents_changed":0})` + "\n" +
		NothingChangedHeading + "\n\nNothing that decides when the 42 rules fire has changed: nothing to replay.\n"
	if b.String() != want {
		t.Errorf("nothing changed:\n%s\nwant:\n%s", b.String(), want)
	}
}

// What comes from the rules can't format, link, mention, reference, break a
// table or hide anything: each is a code span, and outside them is only
// Lapilli's own text.
func TestCompareMarkdownHostile(t *testing.T) {
	hostile := []string{
		"@octocat please look", "@example-org/oncall", "#123", "example/repo#5", "GH-7", "!12", "~3",
		"<!-- the rest is hidden", "<img src=x onerror=alert(1)>", "</details><details open>", "</summary>",
		"[click](https://evil.example)", "https://evil.example/x", "www.evil.example", "![x](https://evil.example/p.png)",
		"a`b", "``fenced``", "```\ncode\n```", "`", " lead and trail ", "a|b", `a\|b`, `a\`, "| x | y |",
		"**bold** _em_ ~~gone~~", "line\nbreak", "car\rriage", "tab\there", "right\u202eto left", "zero\u200bwidth",
		"line\u2028sep", "no\u00a0break", "filler\u3164x", ":smile: :+1:", "[//]: # (lapilli-backtest default)",
		"$`x`$", "&lt;b&gt;", "&#64;octocat", "\\@octocat", "디스크 부족 @팀", strings.Repeat("long", 200),
	}
	var rs []rules.Rule
	var fs []firing.Firing
	var incs []incident.Incident
	var names []string
	for i, h := range hostile {
		labels := map[string]string{"severity": h}
		rs = append(rs, rules.Rule{Name: h, Expr: "x", Labels: labels})
		fs = append(fs, firing.Firing{Rule: h, Labels: labels, Start: m(100 + 10*i), End: m(105 + 10*i)})
		incs = append(incs, incident.Incident{ID: h, Title: h, Start: m(100 + 10*i), End: m(108 + 10*i)})
		names = append(names, h)
	}
	before := Judge(append(rs, rules.Rule{Name: hostile[0] + " broken", Unsupported: hostile[5] + " " + hostile[6]}), nil, incs, opts())
	after := Judge(rs, fs, incs, opts())
	c := Compare(before, after, append(names, hostile[0]+" broken"), 0)
	c.Summarize(true)
	var b bytes.Buffer
	if err := CompareMarkdown(&b, c, time.UTC); err != nil {
		t.Fatal(err)
	}
	checkMarkdown(t, b.String(), hostile)
}

// The longest comment is cut short within MarkdownLimit, its tables and
// <details> whole, saying what was left out.
func TestCompareMarkdownLimit(t *testing.T) {
	var rs []rules.Rule
	var fs []firing.Firing
	var names []string
	for i := range 3000 {
		name := fmt.Sprintf("Rule%04d%s", i, strings.Repeat("x", 150))
		rs = append(rs, rules.Rule{Name: name, Expr: "x"})
		fs = append(fs, firing.Firing{Rule: name, Start: m(i), End: m(i + 1)})
		names = append(names, name)
	}
	// The first 2000 differ; the rest fire alike.
	c := Compare(Judge(rs, fs[2000:], nil, opts()), Judge(rs, fs, nil, opts()), names, 0)
	c.Summarize(false)
	var b bytes.Buffer
	if err := CompareMarkdown(&b, c, time.UTC); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if len(out) > MarkdownLimit || len(out) < MarkdownLimit-1000 {
		t.Errorf("%d bytes, want at most %d and near it", len(out), MarkdownLimit)
	}
	if !strings.Contains(out, "**Cut short** to keep this under 64000 characters: ") ||
		!strings.Contains(out, "rules that differ, 1000 changed rules that fire as they did not shown.") {
		t.Errorf("no note of what was left out:\n%s", out[len(out)-600:])
	}
	if strings.Contains(out, "<details>") {
		t.Errorf("a <details> with no room for its rows")
	}
	checkMarkdown(t, out, nil)
}

// checkMarkdown checks Markdown CompareMarkdown wrote: each table row has
// as many cells as its table's head; and once the code spans are taken
// out, as CommonMark reads them, what is left is Lapilli's own: none of
// hostile, and none of what starts a mention, a reference, a link, HTML
// or an emphasis, but for the headings, the summary line and <details>.
func checkMarkdown(t *testing.T, out string, hostile []string) {
	t.Helper()
	cells := 0
	for i, line := range strings.Split(out, "\n") {
		if i == 0 {
			if !strings.HasPrefix(line, SummaryLine) || strings.ContainsAny(strings.TrimPrefix(line, SummaryLine), "()[]`<@") && !strings.HasSuffix(line, "})") {
				t.Errorf("first line %q", line)
			}
			continue
		}
		if strings.HasPrefix(line, "|") {
			n := strings.Count(line, "|") - strings.Count(line, `\|`)
			if cells == 0 {
				cells = n
			} else if n != cells {
				t.Errorf("a row of %d cells in a table of %d: %q", n-1, cells-1, line)
			}
		} else {
			cells = 0
		}
		outside := outsideCode(line)
		for _, h := range hostile {
			if len(strings.TrimSpace(h)) > 3 && strings.Contains(outside, strings.TrimSpace(h)) {
				t.Errorf("%q outside a code span: %q", h, line)
			}
		}
		outside = strings.NewReplacer("<details>", "", "</details>", "", "<summary>", "", "</summary>", "", "**", "").Replace(outside)
		if strings.HasPrefix(outside, "### Lapilli backtest, ") {
			// The period's zone, an IANA name: Asia/Seoul.
			outside = strings.ReplaceAll(outside, "/", "")
		}
		outside = strings.TrimLeft(outside, "#")
		if !lapillisOwn.MatchString(outside) {
			t.Errorf("outside the code spans, more than Lapilli's own: %q of %q", outside, line)
		}
	}
}

// lapillisOwn is what Lapilli writes outside code spans, but for the
// headings' #, the bold's ** and <details>: letters, digits, spaces and
// the punctuation of its tables, times and counts. Not @, #, <, [, `, *,
// _, ~, !, &, \ or /, which start a mention, a reference, HTML, a link,
// code, an emphasis, an image, an entity, an escape or a URL.
var lapillisOwn = regexp.MustCompile(`^[\p{L}\p{N} |:,.'+%?()\-→–✗○●]*$`)

// outsideCode is a line with its code spans taken out, as CommonMark reads
// them: a run of n backticks opens one, the next run of exactly n closes
// it, and a run that nothing closes is backticks.
func outsideCode(line string) string {
	var b strings.Builder
	for i := 0; i < len(line); {
		if line[i] != '`' {
			b.WriteByte(line[i])
			i++
			continue
		}
		n := 0
		for i+n < len(line) && line[i+n] == '`' {
			n++
		}
		closed := -1
		for j := i + n; j < len(line); {
			if line[j] != '`' {
				j++
				continue
			}
			k := 0
			for j+k < len(line) && line[j+k] == '`' {
				k++
			}
			if k == n {
				closed = j + k
				break
			}
			j += k
		}
		if closed < 0 {
			b.WriteString(line[i : i+n])
			i += n
			continue
		}
		b.WriteString(" ")
		i = closed
	}
	return b.String()
}
