package verdict

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

// An agent's rewrite of a rule, backtested against the rule's
// expectations (docs/design/expectations.md, step 3): when it meets every
// one, losing no incident the rule catches, the text a person or the agent
// may post on the pull request. Lapilli writes nothing there itself.

// Rewritten is a rewrite's backtest, as its post says it: the rule, the
// period replayed, its firings and catches as it is and rewritten, and
// each expectation held. Given says whose expectations they were: the
// agent's, given with the rewrite (which it may have loosened, or left one
// out of), not the ones the rule is held to.
type Rewritten struct {
	Rule          string
	From, To      time.Time
	Fired, Caught [2]int
	Results       []ExpectResult
	Given         bool
}

// RewriteMeets says whether a rewrite may be posted: the rule as it is
// (before) and rewritten (after) each replayed to the end; held to one
// expectation at least, it meets each after (met, never not known); and
// lost names no incident the rule catches as it is that the rewrite
// doesn't. A rewrite that meets the budget by losing a catch is never
// shown, nor one beside a rule whose replay was cut short or couldn't be
// made: what it catches isn't known, and so neither is what the rewrite
// loses.
func RewriteMeets(results []ExpectResult, lost []string, before, after *RuleResult) bool {
	if len(results) == 0 || len(lost) > 0 || before == nil || after == nil || before.Unsupported != "" || after.Unsupported != "" {
		return false
	}
	for _, r := range results {
		if r.AfterState != ExpectMet {
			return false
		}
	}
	return true
}

// RewriteMarkdown is the post of a rewrite that meets the expectations it
// was held to: marked as an agent's, backtested by Lapilli, saying whose
// expectations they were (Lapilli's for the rule, or the agent's own),
// with the numbers before and after, and the change as a diff. What comes
// from the rule or the agent is in code spans and a fenced block a
// backtick longer than any run of them in it, so it formats, links and
// mentions nothing.
func RewriteMarkdown(w Rewritten, diff string) string {
	var b strings.Builder
	b.WriteString("#### An agent's rewrite of " + md(w.Rule, 200, false) + ", backtested by Lapilli\n\n")
	held, text := "every expectation Lapilli holds the rule to", "the agent's text"
	if w.Given {
		held, text = "every expectation the agent gave with it, which aren't necessarily the ones Lapilli holds the rule to", "the agent's text and expectations"
	}
	fmt.Fprintf(&b, "An agent wrote this rewrite, not Lapilli, and not a person. Lapilli replayed it against the metrics' history from %s to %s, beside the rule as it is: "+
		"it meets %s, and loses no incident the rule catches. Review it as you would any change.\n\n",
		w.From.UTC().Format("2006-01-02 15:04 UTC"), w.To.UTC().Format("2006-01-02 15:04 UTC"), held)
	b.WriteString(mdRow([]string{"Held to", "As it is", "Rewritten", ""}) + mdRow([]string{"---", "--:", "--:", "---"}))
	code := func(s string) string { return md(s, 100, true) }
	for _, r := range w.Results {
		was := r.Before
		if r.BeforeState == ExpectBroken {
			was += " ✗"
		}
		b.WriteString(mdRow([]string{r.heldTo(code), was, r.After, "met"}))
	}
	fmt.Fprintf(&b, "\nFired %d → %d over the period; firings that caught an incident %d → %d.\n\n", w.Fired[0], w.Fired[1], w.Caught[0], w.Caught[1])
	if diff != "" {
		b.WriteString(fenced(diff, "diff"))
	}
	b.WriteString("\n<sub>Posted from Lapilli's test_expectation: " + text + ", Lapilli's numbers.</sub>\n")
	return b.String()
}

// fenced is text in a fenced code block: its characters that don't show,
// or move the text around them, shown as \u{...}; the fence a backtick
// longer than the longest run of them in it, three at least.
func fenced(s, lang string) string {
	var b strings.Builder
	run, longest := 0, 0
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteRune(r)
			run = 0
			continue
		case unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp):
			fmt.Fprintf(&b, "\\u{%x}", r)
			run = 0
			continue
		case r == '`':
			run++
			longest = max(longest, run)
		default:
			run = 0
		}
		b.WriteRune(r)
	}
	fence := strings.Repeat("`", max(3, longest+1))
	body := strings.TrimSuffix(b.String(), "\n")
	return fence + lang + "\n" + body + "\n" + fence + "\n"
}
