package verdict

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"lapilli.dev/internal/incident"
)

// Comparison is the verdict on one period from two sets of rules: those in
// place (Before) and those proposed (After).
type Comparison struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Same is how many rules are alike in both.
	Same int `json:"same"`
	// ChangedOnly says the rules alike in both weren't replayed: the
	// reports, and the incidents' catches, are the changed rules' alone.
	ChangedOnly bool `json:"changed_only,omitempty"`
	// Rules are the rules that differ, by name, those whose firings change
	// most first.
	Rules []RuleChange `json:"rules"`
	// Incidents are those the two sets catch differently.
	Incidents []IncidentChange `json:"incidents"`
	Before    *Report          `json:"before"`
	After     *Report          `json:"after"`
}

// RuleChange is a rule's stones before and after: Before is nil for a rule
// added, After for one taken away.
type RuleChange struct {
	Rule   string      `json:"rule"`
	Before *RuleResult `json:"before"`
	After  *RuleResult `json:"after"`
}

// Alike reports whether a changed rule fires as often, and catches as
// much, as it did.
func (c RuleChange) Alike() bool {
	b, a := c.Before, c.After
	return b != nil && a != nil && b.Firings == a.Firings && b.White == a.White &&
		b.Refired == a.Refired && b.Unsupported == a.Unsupported
}

// IncidentChange is an incident the two sets of rules catch differently.
type IncidentChange struct {
	incident.Incident
	Before []Catch `json:"before"`
	After  []Catch `json:"after"`
}

// Compare sets two verdicts on the same period and incidents side by side:
// the rules named in changed, and the incidents caught differently. same is
// how many rules the two sets have alike.
func Compare(before, after *Report, changed []string, same int) *Comparison {
	c := &Comparison{From: before.From, To: before.To, Same: same,
		Rules: []RuleChange{}, Incidents: []IncidentChange{}, Before: before, After: after}
	find := func(r *Report, name string) *RuleResult {
		i := slices.IndexFunc(r.Rules, func(rr RuleResult) bool { return rr.Rule == name })
		if i < 0 {
			return nil
		}
		return &r.Rules[i]
	}
	seen := map[string]bool{}
	for _, name := range changed {
		if !seen[name] {
			seen[name] = true
			c.Rules = append(c.Rules, RuleChange{Rule: name, Before: find(before, name), After: find(after, name)})
		}
	}
	firings := func(r *RuleResult) int {
		if r == nil {
			return 0
		}
		return r.Firings
	}
	moved := func(rc RuleChange) int {
		d := firings(rc.After) - firings(rc.Before)
		return max(d, -d)
	}
	sort.SliceStable(c.Rules, func(a, b int) bool {
		if ma, mb := moved(c.Rules[a]), moved(c.Rules[b]); ma != mb {
			return ma > mb
		}
		return c.Rules[a].Rule < c.Rules[b].Rule
	})
	// Both were judged against the same incidents, which come in the same
	// order.
	for i, b := range before.Incidents {
		a := after.Incidents[i]
		if !slices.Equal(b.CaughtBy, a.CaughtBy) {
			c.Incidents = append(c.Incidents, IncidentChange{Incident: b.Incident, Before: b.CaughtBy, After: a.CaughtBy})
		}
	}
	return c
}

// CompareText writes a comparison for a person, times in loc.
func CompareText(w io.Writer, c *Comparison, loc *time.Location) error {
	const stamp = "2006-01-02 15:04"
	var changed, added, gone int
	var alike, broken []RuleChange
	var shown []RuleChange
	for _, rc := range c.Rules {
		switch {
		case rc.Before == nil:
			added++
		case rc.After == nil:
			gone++
		default:
			changed++
		}
		unreplayed := func(r *RuleResult) bool { return r != nil && r.Unsupported != "" }
		if unreplayed(rc.Before) || unreplayed(rc.After) {
			broken = append(broken, rc)
		}
		switch {
		case rc.Alike():
			alike = append(alike, rc)
		case (rc.Before == nil || unreplayed(rc.Before)) && (rc.After == nil || unreplayed(rc.After)):
			// nothing replayed on either side: only the reason to show
		default:
			shown = append(shown, rc)
		}
	}
	fmt.Fprintf(w, "Lapilli  %s → %s %s\n", c.From.In(loc).Format(stamp), c.To.In(loc).Format(stamp), loc)
	fmt.Fprintf(w, "Rules compared: %d changed, %d added, %d taken away, %d the same\n", changed, added, gone, c.Same)
	if c.ChangedOnly {
		fmt.Fprintf(w, "Firings of the changed rules %d → %d\n", len(c.Before.Firings), len(c.After.Firings))
	} else {
		fmt.Fprintf(w, "Firings %d → %d\n", len(c.Before.Firings), len(c.After.Firings))
	}
	if by := bySeverity(c.Before, c.After); by != "" {
		fmt.Fprintf(w, "By severity: %s\n", by)
	}
	if n := len(c.Before.Incidents); n > 0 && c.ChangedOnly {
		fmt.Fprintln(w, "Rules left alone weren't replayed (--changed-only): incidents show the changed rules' catches alone.")
	} else if n > 0 {
		count := func(r *Report, f func(IncidentResult) bool) int {
			m := 0
			for _, in := range r.Incidents {
				if f(in) {
					m++
				}
			}
			return m
		}
		fmt.Fprintf(w, "Of %d incidents: with no alert %d → %d", n, count(c.Before, IncidentResult.Missed), count(c.After, IncidentResult.Missed))
		if b, a := count(c.Before, IncidentResult.OnlyNoisy), count(c.After, IncidentResult.OnlyNoisy); b+a > 0 {
			fmt.Fprintf(w, ", caught only by noisy rules %d → %d", b, a)
		}
		fmt.Fprintln(w)
	}

	if len(shown) > 0 {
		fmt.Fprintln(w)
		sev := slices.ContainsFunc(shown, func(rc RuleChange) bool {
			return rc.Before != nil && rc.Before.Severity != "" || rc.After != nil && rc.After.Severity != ""
		})
		rows := [][]string{withSeverity(sev, "RULE", "SEVERITY", "FIRED", "○ CAUGHT", "● NOTHING", "PRECISION", "REFIRED")}
		for _, rc := range shown {
			rows = append(rows, withSeverity(sev, Clean(rc.Rule), severities(rc),
				both(rc, func(r RuleResult) string { return fmt.Sprint(r.Firings) }),
				both(rc, func(r RuleResult) string { return fmt.Sprint(r.White) }),
				both(rc, func(r RuleResult) string { return fmt.Sprint(r.Black) }),
				both(rc, func(r RuleResult) string {
					if p, ok := r.Precision(); ok {
						return fmt.Sprintf("%.0f%%", 100*p)
					}
					return "–"
				}),
				both(rc, func(r RuleResult) string { return fmt.Sprint(r.Refired) })))
		}
		if err := table(w, rows); err != nil {
			return err
		}
	}
	if len(alike) > 0 {
		fmt.Fprintf(w, "\n%d changed rules keep the same stones:\n", len(alike))
		for _, rc := range alike {
			fmt.Fprintf(w, "  %s\n", Clean(rc.Rule))
		}
	}
	if len(broken) > 0 {
		fmt.Fprintf(w, "\nCould not replay %d rules:\n", len(broken))
		for _, rc := range broken {
			for _, side := range []struct {
				name string
				r    *RuleResult
			}{{"before", rc.Before}, {"after", rc.After}} {
				if side.r != nil && side.r.Unsupported != "" {
					fmt.Fprintf(w, "  %s (%s): %s\n", Clean(rc.Rule), side.name, Clean(side.r.Unsupported))
				}
			}
		}
	}

	if len(c.Before.Incidents) == 0 {
		return nil
	}
	fmt.Fprintln(w)
	if len(c.Incidents) == 0 {
		fmt.Fprintln(w, "Every incident is caught as it was.")
		return nil
	}
	// Each incident over three lines: the lists of catches can be long.
	nameWidth := textWidth("INCIDENT")
	for _, ic := range c.Incidents {
		nameWidth = max(nameWidth, textWidth(incidentName(ic.Incident)))
	}
	pad := func(s string) string { return s + strings.Repeat(" ", nameWidth-textWidth(s)+2) }
	catches := func(cs []Catch) string {
		if len(cs) == 0 && c.ChangedOnly {
			return "no changed rule fired"
		}
		return caughtBy(cs)
	}
	var b strings.Builder
	b.WriteString(pad("INCIDENT") + "START\n")
	for _, ic := range c.Incidents {
		fmt.Fprintf(&b, "%s%s\n  before  %s\n  after   %s\n",
			pad(incidentName(ic.Incident)), ic.Start.In(loc).Format(stamp), catches(ic.Before), catches(ic.After))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// severities is a rule's severity before → after, or the one it keeps.
func severities(rc RuleChange) string {
	side := func(r *RuleResult) string {
		if r == nil || r.Severity == "" {
			return "–"
		}
		return Clean(r.Severity)
	}
	b, a := side(rc.Before), side(rc.After)
	if rc.Before != nil && rc.After != nil && b == a {
		return b
	}
	return b + " → " + a
}

// bySeverity counts the firings of each severity before → after, or is
// empty when no firing has a severity.
func bySeverity(before, after *Report) string {
	count := func(r *Report) map[string]int {
		m := map[string]int{}
		for _, f := range r.Firings {
			m[f.Labels["severity"]]++
		}
		return m
	}
	b, a := count(before), count(after)
	keys := map[string]bool{}
	for k := range b {
		keys[k] = true
	}
	for k := range a {
		keys[k] = true
	}
	if len(keys) == 0 || len(keys) == 1 && keys[""] {
		return ""
	}
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		name := Clean(k)
		if k == "" {
			name = "none"
		}
		parts = append(parts, fmt.Sprintf("%s %d → %d", name, b[k], a[k]))
	}
	return strings.Join(parts, ", ")
}

// both is one column of a rule's stones, before → after, with – where the
// rule isn't and ? where it couldn't be replayed.
func both(rc RuleChange, f func(RuleResult) string) string {
	side := func(r *RuleResult) string {
		switch {
		case r == nil:
			return "–"
		case r.Unsupported != "":
			return "?"
		}
		return f(*r)
	}
	return side(rc.Before) + " → " + side(rc.After)
}
