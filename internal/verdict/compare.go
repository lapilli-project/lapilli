package verdict

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
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
		switch {
		case rc.Before != nil && rc.Before.Unsupported != "", rc.After != nil && rc.After.Unsupported != "":
			broken = append(broken, rc)
		case rc.Alike():
			alike = append(alike, rc)
		default:
			shown = append(shown, rc)
		}
	}
	fmt.Fprintf(w, "Lapilli  %s → %s %s\n", c.From.In(loc).Format(stamp), c.To.In(loc).Format(stamp), loc)
	fmt.Fprintf(w, "Rules compared: %d changed, %d added, %d taken away, %d the same\n", changed, added, gone, c.Same)
	fmt.Fprintf(w, "Firings %d → %d", len(c.Before.Firings), len(c.After.Firings))
	if n := len(c.Before.Incidents); n > 0 {
		missed := func(r *Report) int {
			m := 0
			for _, in := range r.Incidents {
				if in.Missed() {
					m++
				}
			}
			return m
		}
		fmt.Fprintf(w, "; incidents with no alert %d → %d of %d", missed(c.Before), missed(c.After), n)
	}
	fmt.Fprintln(w)

	if len(shown) > 0 {
		fmt.Fprintln(w)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "RULE\tFIRED\t○ CAUGHT\t● NOTHING\tPRECISION\tREFIRED")
		for _, rc := range shown {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", Clean(rc.Rule),
				both(rc, func(r RuleResult) string { return fmt.Sprint(r.Firings) }),
				both(rc, func(r RuleResult) string { return fmt.Sprint(r.White) }),
				both(rc, func(r RuleResult) string { return fmt.Sprint(r.Black) }),
				both(rc, func(r RuleResult) string {
					if p, ok := r.Precision(); ok {
						return fmt.Sprintf("%.0f%%", 100*p)
					}
					return "–"
				}),
				both(rc, func(r RuleResult) string { return fmt.Sprint(r.Refired) }))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if len(alike) > 0 {
		var names []string
		for _, rc := range alike {
			names = append(names, Clean(rc.Rule))
		}
		fmt.Fprintf(w, "\n%d changed rules keep the same stones: %s\n", len(alike), strings.Join(names, ", "))
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
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "INCIDENT\tSTART\tBEFORE\tAFTER")
	for _, ic := range c.Incidents {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", incidentName(ic.Incident), ic.Start.In(loc).Format(stamp), caughtBy(ic.Before), caughtBy(ic.After))
	}
	return tw.Flush()
}

// both is one column of a rule's stones, before → after, with – where the
// rule isn't.
func both(rc RuleChange, f func(RuleResult) string) string {
	side := func(r *RuleResult) string {
		if r == nil {
			return "–"
		}
		return f(*r)
	}
	return side(rc.Before) + " → " + side(rc.After)
}
