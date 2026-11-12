package verdict

import (
	"fmt"
	"io"
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
	// Summary is the comparison in numbers, for a tool to read. lapilli
	// verdict --compare gives it; lapilli serve keeps none.
	Summary *Summary `json:"summary,omitempty"`
}

// Summary is a comparison in numbers, for a tool to read: lapilli verdict
// --compare --json gives it as "summary", and --markdown on its first
// line. Every field is always there, and its meaning doesn't change
// (README, "Pull request backtests in CI").
type Summary struct {
	// NothingChanged says nothing that decides when a rule fires differs
	// between the two sets of rules: nothing was replayed.
	NothingChanged bool `json:"nothing_changed"`
	// Changed, Added and Removed are the rules that differ: in both sets,
	// only in the rules proposed (--rules), only in those in place
	// (--compare). Same are those alike in both.
	Changed int `json:"changed"`
	Added   int `json:"added"`
	Removed int `json:"removed"`
	Same    int `json:"same"`
	// Alike are the rules that differ but fire as they did, replayed on
	// both sides: as often, catching as much.
	Alike int `json:"alike"`
	// Unreplayed are the rules that differ that couldn't be replayed, on
	// one side or both.
	Unreplayed int `json:"unreplayed"`
	// FiringsBefore and FiringsAfter are the firings on each side: of the
	// rules that differ alone, with --changed-only.
	FiringsBefore int `json:"firings_before"`
	FiringsAfter  int `json:"firings_after"`
	// IncidentsGiven says the firings were judged against incidents,
	// --incidents or --impact. Without them nothing was judged: no firing
	// caught anything, each counts as a black stone, and Incidents is 0.
	IncidentsGiven bool `json:"incidents_given"`
	// Incidents are the incidents in the period, and IncidentsChanged
	// those the two sets catch differently.
	Incidents        int `json:"incidents"`
	IncidentsChanged int `json:"incidents_changed"`
}

// Summarize sets c.Summary from the rest of c; incidents says whether
// incidents were given to judge against.
func (c *Comparison) Summarize(incidents bool) {
	p := c.parts()
	s := &Summary{Changed: p.changed, Added: p.added, Removed: p.gone, Same: c.Same, Alike: len(p.alike),
		Unreplayed: len(p.broken), IncidentsGiven: incidents, IncidentsChanged: len(c.Incidents)}
	s.NothingChanged = s.Changed+s.Added+s.Removed == 0
	if c.Before != nil {
		s.FiringsBefore, s.Incidents = len(c.Before.Firings), len(c.Before.Incidents)
	}
	if c.After != nil {
		s.FiringsAfter = len(c.After.Firings)
	}
	c.Summary = s
}

// judged says the firings were judged against incidents: always, unless
// the summary says none were given.
func (c *Comparison) judged() bool { return c.Summary == nil || c.Summary.IncidentsGiven }

// compareParts is a comparison's rules that differ, sorted for showing.
type compareParts struct {
	changed, added, gone int
	// alike fire as they did; broken couldn't be replayed on one side or
	// both; shown are the rest, and those replayed on one side of broken.
	alike, broken, shown []RuleChange
}

func (c *Comparison) parts() compareParts {
	var p compareParts
	unreplayed := func(r *RuleResult) bool { return r != nil && r.Unsupported != "" }
	for _, rc := range c.Rules {
		switch {
		case rc.Before == nil:
			p.added++
		case rc.After == nil:
			p.gone++
		default:
			p.changed++
		}
		if unreplayed(rc.Before) || unreplayed(rc.After) {
			p.broken = append(p.broken, rc)
		}
		switch {
		case rc.Alike() && !unreplayed(rc.Before) && !unreplayed(rc.After):
			// Replayed alike on both sides: a rule replayed on neither
			// can't be said to fire as it did.
			p.alike = append(p.alike, rc)
		case (rc.Before == nil || unreplayed(rc.Before)) && (rc.After == nil || unreplayed(rc.After)):
			// nothing replayed on either side: only the reason to show
		default:
			p.shown = append(p.shown, rc)
		}
	}
	return p
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

// CompareText writes a comparison for a person, times in loc. Without
// incidents to judge against (see Summary), the firings are counted, not
// judged: no catches, no precision, nothing of incidents.
func CompareText(w io.Writer, c *Comparison, loc *time.Location) error {
	const stamp = "2006-01-02 15:04"
	p := c.parts()
	alike, broken, shown := p.alike, p.broken, p.shown
	judged := c.judged()
	fmt.Fprintf(w, "Lapilli  %s → %s %s\n", c.From.In(loc).Format(stamp), c.To.In(loc).Format(stamp), Zone(loc, c.From, c.To))
	fmt.Fprintf(w, "Rules compared: %d changed, %d added, %d taken away, %d the same\n", p.changed, p.added, p.gone, c.Same)
	if c.ChangedOnly {
		fmt.Fprintf(w, "Firings of the changed rules %d → %d\n", len(c.Before.Firings), len(c.After.Firings))
	} else {
		fmt.Fprintf(w, "Firings %d → %d\n", len(c.Before.Firings), len(c.After.Firings))
	}
	if by := bySeverity(c.Before, c.After); by != "" {
		fmt.Fprintf(w, "By severity: %s\n", by)
	}
	if n := len(c.Before.Incidents); n == 0 || !judged {
		// nothing judged: nothing to say of incidents
	} else if c.ChangedOnly {
		fmt.Fprintln(w, "Rules left alone weren't replayed (--changed-only): incidents show the changed rules' catches alone.")
	} else {
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
		rows := [][]string{judgedColumns(judged, withSeverity(sev, "RULE", "SEVERITY", "FIRED", "○ CAUGHT", "● NOTHING", "PRECISION", "REFIRED"))}
		for _, rc := range shown {
			rows = append(rows, judgedColumns(judged, withSeverity(sev, Clean(rc.Rule), severities(rc), stones(rc)...)))
		}
		if err := table(w, rows); err != nil {
			return err
		}
	}
	if len(alike) > 0 {
		keep := "keep the same stones"
		if !judged {
			keep = "fire as they did"
		}
		fmt.Fprintf(w, "\n%d changed rules %s:\n", len(alike), keep)
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

	if len(c.Before.Incidents) == 0 || !judged {
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

// stones are a changed rule's cells after its severity, before → after:
// fired, caught, nothing, precision, refired.
func stones(rc RuleChange) []string {
	return []string{
		both(rc, func(r RuleResult) string { return fmt.Sprint(r.Firings) }),
		both(rc, func(r RuleResult) string { return fmt.Sprint(r.White) }),
		both(rc, func(r RuleResult) string { return fmt.Sprint(r.Black) }),
		both(rc, func(r RuleResult) string {
			if p, ok := r.Precision(); ok {
				return fmt.Sprintf("%.0f%%", 100*p)
			}
			return "–"
		}),
		both(rc, func(r RuleResult) string { return fmt.Sprint(r.Refired) }),
	}
}

// judgedColumns is a row of a table of stones as it is shown: without
// its caught, nothing and precision when nothing was judged, which
// would say nothing. The row ends fired, caught, nothing, precision,
// refired.
func judgedColumns(judged bool, row []string) []string {
	if judged {
		return row
	}
	n := len(row)
	return append(slices.Clone(row[:n-4]), row[n-1])
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
	var parts []string
	for _, s := range severityCounts(before, after) {
		name := Clean(s.name)
		if s.name == "" {
			name = "none"
		}
		parts = append(parts, fmt.Sprintf("%s %d → %d", name, s.before, s.after))
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
