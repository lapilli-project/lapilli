package verdict

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// MarkdownLimit is the most bytes CompareMarkdown writes: under the 65,536
// characters GitHub takes in a comment, the least of the hosts a pull
// request is kept on, with room for a line or two of a tool's own.
const MarkdownLimit = 64000

// SummaryLine starts CompareMarkdown's first line, which is the summary as
// JSON: [//]: # (lapilli-compare {"nothing_changed":false,...}). A link
// reference definition, which every host's Markdown reads and shows
// nothing of; HTML comments some show as text.
const SummaryLine = "[//]: # (lapilli-compare "

// NothingChangedHeading is the heading CompareMarkdown writes when nothing
// that decides when a rule fires changed: the whole comment is it, the
// summary line above it, and one line below.
const NothingChangedHeading = "### Lapilli backtest: nothing changed"

// CompareMarkdown writes a comparison as Markdown, ready to post as a
// comment on a pull request or a merge request, times in loc: the summary
// line, a heading with the period and its zone, the totals, the rules that
// differ as a table, those that couldn't be replayed, those that fire as
// they did folded in <details>, and the incidents caught differently. At
// most MarkdownLimit bytes: what doesn't fit is left out, and a note says
// how much.
//
// Rule names, labels, reasons and incidents come from whoever wrote the
// rules, in a pull request someone outside may have opened. Each is
// written as a code span (md), in which no host reads Markdown or HTML,
// mentions (@user, @org/team), references (#123) or links: none can ping
// anyone, link anywhere, break a table, or hide what follows.
func CompareMarkdown(w io.Writer, c *Comparison, loc *time.Location) error {
	const stamp = "2006-01-02 15:04"
	s := c.Summary
	if s == nil {
		cc := *c
		cc.Summarize(true)
		s = cc.Summary
	}
	line, err := json.Marshal(s)
	if err != nil {
		return err
	}
	var parts []mdPart
	text := func(s string) { parts = append(parts, mdPart{head: s}) }
	text(SummaryLine + string(line) + ")\n")
	if s.NothingChanged || c.Before == nil || c.After == nil {
		text(NothingChangedHeading + "\n\n")
		text(fmt.Sprintf("Nothing that decides when the %d rules fire has changed: nothing to replay.\n", c.Same))
		return writeParts(w, parts, MarkdownLimit)
	}
	p := c.parts()
	judged := c.judged()
	text(fmt.Sprintf("### Lapilli backtest, %s → %s %s\n\n", c.From.In(loc).Format(stamp), c.To.In(loc).Format(stamp), Zone(loc, c.From, c.To)))
	text(fmt.Sprintf("Rules compared: **%d changed, %d added, %d taken away**, %d the same.", p.changed, p.added, p.gone, c.Same))
	if !judged {
		text(" No incidents were given: the firings are counted, not judged.")
	}
	text("\n\n")

	// The totals, before and after.
	totals := mdPart{head: "| | Before | After |\n|---|--:|--:|\n", foot: "\n", noun: "totals"}
	firings := "Firings"
	if c.ChangedOnly {
		firings = "Firings of the changed rules"
	}
	totals.rows = append(totals.rows, fmt.Sprintf("| %s | %d | %d |\n", firings, len(c.Before.Firings), len(c.After.Firings)))
	for _, sev := range severityCounts(c.Before, c.After) {
		name := "no severity"
		if sev.name != "" {
			name = "Severity " + md(sev.name, 60, true)
		}
		totals.rows = append(totals.rows, fmt.Sprintf("| %s | %d | %d |\n", name, sev.before, sev.after))
	}
	n := len(c.Before.Incidents)
	if judged && n > 0 && !c.ChangedOnly {
		count := func(r *Report, f func(IncidentResult) bool) int {
			m := 0
			for _, in := range r.Incidents {
				if f(in) {
					m++
				}
			}
			return m
		}
		totals.rows = append(totals.rows, fmt.Sprintf("| Incidents with no alert, of %d | %d | %d |\n", n,
			count(c.Before, IncidentResult.Missed), count(c.After, IncidentResult.Missed)))
		if b, a := count(c.Before, IncidentResult.OnlyNoisy), count(c.After, IncidentResult.OnlyNoisy); b+a > 0 {
			totals.rows = append(totals.rows, fmt.Sprintf("| Incidents caught only by noisy rules | %d | %d |\n", b, a))
		}
	}
	parts = append(parts, totals)

	if len(p.shown) > 0 {
		sev := slices.ContainsFunc(p.shown, func(rc RuleChange) bool {
			return rc.Before != nil && rc.Before.Severity != "" || rc.After != nil && rc.After.Severity != ""
		})
		head := judgedColumns(judged, withSeverity(sev, "Rule", "Severity", "Fired", "○ Caught", "● Nothing", "Precision", "Refired"))
		align := []string{"---"}
		if sev {
			align = append(align, "---")
		}
		for range head[len(align):] {
			align = append(align, "--:")
		}
		rules := mdPart{head: "#### Rules that differ\n\n" + mdRow(head) + mdRow(align), foot: "\n", noun: "rules that differ"}
		for _, rc := range p.shown {
			rules.rows = append(rules.rows, mdRow(judgedColumns(judged, withSeverity(sev, md(rc.Rule, 200, true), mdSeverities(rc), stones(rc)...))))
		}
		parts = append(parts, rules)
	}
	if len(c.Recording) > 0 {
		recs := mdPart{head: fmt.Sprintf("#### Recording rules that differ\n\n%s\n\n", RecordingNote), foot: "\n", noun: "recording rules that differ"}
		for _, r := range c.Recording {
			recs.rows = append(recs.rows, fmt.Sprintf("- %s in %s: %s\n", md(r.Record, 200, false), md(r.Group, 100, false), r.Change))
		}
		parts = append(parts, recs)
	}
	if n := p.limited(); n > 0 {
		text(fmt.Sprintf("%d of the rules that differ are in a group with a limit, which the replay doesn't hold them to: past it, Prometheus drops all of a rule's alerts.\n\n", n))
	}
	if len(p.broken) > 0 {
		// A row a rule, its reasons on each side in it: a note that leaves
		// rows out counts rules.
		broken := mdPart{head: fmt.Sprintf("#### Could not replay %d rules\n\n", len(p.broken)), foot: "\n", noun: "rules that couldn't be replayed"}
		for _, rc := range p.broken {
			var why []string
			for _, side := range []struct {
				name string
				r    *RuleResult
			}{{"before", rc.Before}, {"after", rc.After}} {
				if side.r != nil && side.r.Unsupported != "" {
					why = append(why, side.name+": "+md(side.r.Unsupported, 300, false))
				}
			}
			broken.rows = append(broken.rows, fmt.Sprintf("- %s, %s\n", md(rc.Rule, 200, false), strings.Join(why, "; ")))
		}
		parts = append(parts, broken)
	}
	if len(p.alike) > 0 {
		keep := "keep the same stones"
		if !judged {
			keep = "fire as they did"
		}
		alike := mdPart{head: fmt.Sprintf("<details><summary>%d changed rules %s</summary>\n\n", len(p.alike), keep),
			foot: "\n</details>\n\n", noun: "changed rules that " + keep}
		for _, rc := range p.alike {
			alike.rows = append(alike.rows, "- "+md(rc.Rule, 200, false)+"\n")
		}
		parts = append(parts, alike)
	}

	if judged && n > 0 {
		switch {
		case len(c.Incidents) == 0:
			text("Every incident is caught as it was.\n\n")
		default:
			catches := func(cs []Catch) string {
				if len(cs) == 0 && c.ChangedOnly {
					return "no changed rule fired"
				}
				if len(cs) == 0 {
					return "✗ nothing fired"
				}
				var out []string
				for _, k := range cs {
					one := md(k.Rule, 200, true) + " " + Delay(k.Delay)
					if k.Noisy {
						one += " (noisy)"
					}
					out = append(out, one)
				}
				return strings.Join(out, ", ")
			}
			incs := mdPart{head: "#### Incidents caught differently\n\n" + mdRow([]string{"Incident", "Start", "Before", "After"}) +
				mdRow([]string{"---", "---", "---", "---"}), foot: "\n", noun: "incidents"}
			for _, ic := range c.Incidents {
				name := md(ic.ID, 100, true)
				if ic.Title != "" {
					name += " " + md(ic.Title, 200, true)
				}
				incs.rows = append(incs.rows, mdRow([]string{name, ic.Start.In(loc).Format(stamp), catches(ic.Before), catches(ic.After)}))
			}
			parts = append(parts, incs)
		}
		if c.ChangedOnly {
			text("Rules left alone weren't replayed (`--changed-only`): incidents show the changed rules' catches alone.\n")
		}
	}
	if said := passedOver(c.PassedOver); said != "" {
		text("\nPassed over, reading the rules: " + said + ".\n")
	}
	return writeParts(w, parts, MarkdownLimit)
}

// passedOver says what reading each set's files passed over, as
// Comparison.PassedOver holds it: Lapilli's own words and counts.
func passedOver(by map[string]string) string {
	var said []string
	for _, side := range []struct{ key, name string }{{"rules", "the rules proposed (--rules)"}, {"compare", "the rules in place (--compare)"}} {
		if s := by[side.key]; s != "" {
			said = append(said, side.name+" held "+s)
		}
	}
	return strings.Join(said, "; ")
}

// mdPart is a part of a comment: a head, rows, and a foot written after
// the rows; without rows, its head alone. A part whose head, first row and
// foot don't fit is left out whole; a row that doesn't fit is left out,
// and those after it, counted as noun.
type mdPart struct {
	head, foot string
	rows       []string
	noun       string
}

// writeParts writes parts, at most limit bytes of them: what is left out
// for want of room is said last.
func writeParts(w io.Writer, parts []mdPart, limit int) error {
	// Room for the note that says what was left out.
	const note = 400
	room := limit - note
	var b strings.Builder
	type cut struct {
		noun string
		n    int
	}
	var cuts []cut
	leave := func(noun string, n int) {
		if n == 0 {
			return
		}
		if i := slices.IndexFunc(cuts, func(c cut) bool { return c.noun == noun }); i >= 0 {
			cuts[i].n += n
			return
		}
		cuts = append(cuts, cut{noun, n})
	}
	for _, p := range parts {
		if len(p.rows) == 0 {
			if b.Len()+len(p.head)+len(p.foot) <= room {
				b.WriteString(p.head + p.foot)
			}
			continue
		}
		if b.Len()+len(p.head)+len(p.rows[0])+len(p.foot) > room {
			leave(p.noun, len(p.rows))
			continue
		}
		b.WriteString(p.head)
		i := 0
		for ; i < len(p.rows) && b.Len()+len(p.rows[i])+len(p.foot) <= room; i++ {
			b.WriteString(p.rows[i])
		}
		leave(p.noun, len(p.rows)-i)
		b.WriteString(p.foot)
	}
	if len(cuts) > 0 {
		var said []string
		for _, c := range cuts {
			said = append(said, fmt.Sprintf("%d %s", c.n, c.noun))
		}
		fmt.Fprintf(&b, "\n**Cut short** to keep this under %d characters: %s not shown. `lapilli verdict --compare --json` gives them all.\n",
			limit, strings.Join(said, ", "))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// mdRow is a row of a Markdown table, its cells as they are: each made
// with md, a number, or text of Lapilli's own.
func mdRow(cells []string) string { return "| " + strings.Join(cells, " | ") + " |\n" }

// md is text from a rule file, an incident file or a server, as a Markdown
// code span: inside one, no host reads Markdown, HTML, mentions,
// references, emoji or links, so the text is shown as it is and does
// nothing. Characters that don't show, or move the text around them
// (control and format characters, line and paragraph separators, spaces
// other than the space, Hangul fillers), are shown as \u{...}, as Clean
// does for a terminal; it is cut to most characters; the fence is one backtick
// longer than the longest run of them in it. In a table, a | is escaped as
// GitHub's tables take it (\|), which every host's tables read as a | in
// the cell, not the cell's end.
func md(s string, most int, table bool) string {
	// Cut before anything is made of it: a name of megabytes is read no
	// further than it is shown.
	cut := false
	if i, n := 0, 0; utf8.RuneCountInString(s) > most {
		for i = range s {
			if n == most {
				break
			}
			n++
		}
		s, cut = s[:i], true
	}
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) || unicode.Is(unicode.Zs, r) && r != ' ' ||
			r == 0x115f || r == 0x1160 || r == 0x3164 || r == 0xffa0 || r == 0x2800 {
			fmt.Fprintf(&b, "\\u{%x}", r)
			continue
		}
		b.WriteRune(r)
	}
	s = b.String()
	if r := []rune(s); len(r) > most {
		// What doesn't show, shown as \u{...}, is longer.
		s, cut = string(r[:most]), true
	}
	if cut {
		s += "…"
	}
	if s == "" {
		return "–"
	}
	if table {
		s = strings.ReplaceAll(s, "|", `\|`)
	}
	run, longest := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	// A space each side, which the span takes off again, when it starts or
	// ends with a backtick or a space.
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") || strings.HasPrefix(s, " ") || strings.HasSuffix(s, " ") {
		s = " " + s + " "
	}
	return fence + s + fence
}

// mdSeverities is a rule's severity before → after, or the one it keeps,
// each a code span.
func mdSeverities(rc RuleChange) string {
	side := func(r *RuleResult) string {
		if r == nil || r.Severity == "" {
			return "–"
		}
		return md(r.Severity, 60, true)
	}
	b, a := side(rc.Before), side(rc.After)
	if rc.Before != nil && rc.After != nil && b == a {
		return b
	}
	return b + " → " + a
}

// severityCount is how many firings had a severity before and after.
type severityCount struct {
	name          string
	before, after int
}

// severityCounts counts the firings of each severity before and after, in
// the order of their names; none when no firing has a severity.
func severityCounts(before, after *Report) []severityCount {
	count := func(r *Report) map[string]int {
		m := map[string]int{}
		for _, f := range r.Firings {
			m[f.Labels["severity"]]++
		}
		return m
	}
	b, a := count(before), count(after)
	var names []string
	for _, m := range []map[string]int{b, a} {
		for k := range m {
			if !slices.Contains(names, k) {
				names = append(names, k)
			}
		}
	}
	if len(names) == 0 || len(names) == 1 && names[0] == "" {
		return nil
	}
	slices.Sort(names)
	var out []severityCount
	for _, k := range names {
		out = append(out, severityCount{k, b[k], a[k]})
	}
	return out
}
