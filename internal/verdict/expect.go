package verdict

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Expectations are what rules are held to, written beside them
// (docs/design/expectations.md): how often each may fire in a week, how
// often while people slept, and the incidents it must keep catching.
type Expectations struct {
	Expectations []Expectation `yaml:"expectations" json:"expectations"`
}

// Expectation is one rule's, or a group's rules', by name.
type Expectation struct {
	Rule    string        `yaml:"rule,omitempty" json:"rule,omitempty"`
	Group   string        `yaml:"group,omitempty" json:"group,omitempty"`
	Budget  *Budget       `yaml:"budget,omitempty" json:"budget,omitempty"`
	Catches []ExpectCatch `yaml:"catches,omitempty" json:"catches,omitempty"`
}

// Budget is how often a rule may fire in each whole week of a period, and
// of those, how often while people slept, by the hours it is counted by.
type Budget struct {
	Week   *int `yaml:"week,omitempty" json:"week,omitempty"`
	Asleep *int `yaml:"asleep,omitempty" json:"asleep,omitempty"`
}

// ExpectCatch is an incident a rule must keep catching, within a delay of
// its start when Within isn't zero.
type ExpectCatch struct {
	Incident string `yaml:"incident" json:"incident"`
	Within   Within `yaml:"within,omitempty" json:"within,omitempty"`
}

// Within is a delay, a time.Duration written as "5m" in YAML and JSON.
type Within time.Duration

func (d *Within) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil || v < 0 {
		return fmt.Errorf("line %d: within %q: want a duration such as 5m", n.Line, s)
	}
	*d = Within(v)
	return nil
}

func (d Within) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

// MaxExpectations is the most bytes an expectations file is read to, as
// serve's declared configuration is.
const MaxExpectations = 1 << 20

// ReadExpectations reads an expectations file strictly: a key it doesn't
// know, a value of another kind, an expectation naming no rule or group,
// or a budget below nothing is an error, with its line.
func ReadExpectations(r io.Reader) (*Expectations, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxExpectations+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxExpectations {
		return nil, fmt.Errorf("more than %d bytes", MaxExpectations)
	}
	var e Expectations
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&e); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	for i, x := range e.Expectations {
		at := fmt.Sprintf("expectation %d", i+1)
		switch {
		case (x.Rule == "") == (x.Group == ""):
			return nil, fmt.Errorf("%s: give rule or group, one of them", at)
		case x.Budget == nil && len(x.Catches) == 0:
			return nil, fmt.Errorf("%s: nothing to hold %s to: give a budget or catches", at, x.name())
		case x.Budget != nil && x.Budget.Week == nil && x.Budget.Asleep == nil:
			return nil, fmt.Errorf("%s: a budget of nothing: give week or asleep", at)
		case x.Budget != nil && (x.Budget.Week != nil && *x.Budget.Week < 0 || x.Budget.Asleep != nil && *x.Budget.Asleep < 0):
			return nil, fmt.Errorf("%s: a budget below nothing", at)
		}
		for _, c := range x.Catches {
			if strings.TrimSpace(c.Incident) == "" {
				return nil, fmt.Errorf("%s: a catch names no incident", at)
			}
		}
	}
	return &e, nil
}

func (x Expectation) name() string {
	if x.Rule != "" {
		return x.Rule
	}
	return "group " + x.Group
}

// For says whether it holds a rule of that name and group.
func (x Expectation) For(rule, group string) bool {
	if x.Rule != "" {
		return x.Rule == rule
	}
	return x.Group == group
}

// Expect states, of an expectation on one side of a change.
const (
	ExpectMet     = "met"
	ExpectBroken  = "broken"
	ExpectUnknown = "not known"
	ExpectAbsent  = "no rule" // the rule isn't on that side
)

// ExpectResult is one expectation of one rule, before and after a change.
type ExpectResult struct {
	Rule string `json:"rule"`
	// What is held: "budget week", "budget asleep" or "catches INC-43".
	What  string `json:"what"`
	Limit string `json:"limit"`
	// Before and After are what decided it on each side: the most in a
	// week, or the catch's delay; State, met, broken, not known or no rule.
	Before      string `json:"before"`
	After       string `json:"after"`
	BeforeState string `json:"before_state"`
	AfterState  string `json:"after_state"`
	// Changed says the expectation itself changed in the change, from
	// WasLimit (--expect-before).
	Changed  bool   `json:"changed,omitempty"`
	WasLimit string `json:"was_limit,omitempty"`
}

// BrokenByChange says the change broke it: broken after, and met before
// or a rule the change adds. One broken before too isn't the change's.
func (r ExpectResult) BrokenByChange() bool {
	return r.AfterState == ExpectBroken && (r.BeforeState == ExpectMet || r.BeforeState == ExpectAbsent)
}

// AlreadyBroken says it was broken before the change, and still is.
func (r ExpectResult) AlreadyBroken() bool {
	return r.AfterState == ExpectBroken && r.BeforeState == ExpectBroken
}

// Checked are a comparison's expectations: of the rules the change
// touches, each held to what it says; and the rules it adds with none.
type Checked struct {
	Results []ExpectResult `json:"results"`
	// Weeks are the whole weeks the budgets are held over, counted back
	// from the period's end; Scaled, the period's days when it has none,
	// a budget held to a week's share of them.
	Weeks  int     `json:"weeks"`
	Scaled float64 `json:"scaled_days,omitempty"`
	// Unexpected are the rules the change adds that no expectation holds.
	Unexpected []string `json:"unexpected,omitempty"`
}

// Broken counts the expectations the change broke.
func (c *Checked) Broken() int {
	if c == nil {
		return 0
	}
	n := 0
	for _, r := range c.Results {
		if r.BrokenByChange() {
			n++
		}
	}
	return n
}

// CheckExpectations holds the rules a comparison's change touches to what
// after says of them (before, to what before says, when it is given: the
// expectations as they were). hours count the firings while people slept.
func CheckExpectations(c *Comparison, after, before *Expectations, hours *Hours) *Checked {
	if before == nil {
		before = after
	}
	out := &Checked{Results: []ExpectResult{}}
	span := c.To.Sub(c.From)
	out.Weeks = int(span / (7 * 24 * time.Hour))
	if out.Weeks == 0 {
		out.Scaled = span.Hours() / 24
	}
	for _, rc := range c.Rules {
		var b, a *RuleResult = rc.Before, rc.After
		group := ""
		if a != nil {
			group = a.Group
		} else if b != nil {
			group = b.Group
		}
		bGroup := group
		if b != nil {
			bGroup = b.Group
		}
		held := false
		for _, x := range after.Expectations {
			if !x.For(rc.Rule, group) {
				continue
			}
			held = true
			was := matching(before, rc.Rule, bGroup)
			out.Results = append(out.Results, check(c, rc.Rule, x, was, b, a, hours, out)...)
		}
		if !held && b == nil && a != nil {
			out.Unexpected = append(out.Unexpected, rc.Rule)
		}
	}
	return out
}

// matching is the first expectation of a set that holds the rule.
func matching(set *Expectations, rule, group string) *Expectation {
	for i, x := range set.Expectations {
		if x.For(rule, group) {
			return &set.Expectations[i]
		}
	}
	return nil
}

func check(c *Comparison, rule string, x Expectation, was *Expectation, b, a *RuleResult, hours *Hours, out *Checked) []ExpectResult {
	var rs []ExpectResult
	if x.Budget != nil {
		weekB, asleepB := mostInAWeek(c.Before, rule, c.From, c.To, hours, out)
		weekA, asleepA := mostInAWeek(c.After, rule, c.From, c.To, hours, out)
		limits := []struct {
			what       string
			limit, old *int
			before     int
			after      int
			counted    bool
		}{
			{"budget week", x.Budget.Week, oldLimit(was, func(b *Budget) *int { return b.Week }), weekB, weekA, true},
			{"budget asleep", x.Budget.Asleep, oldLimit(was, func(b *Budget) *int { return b.Asleep }), asleepB, asleepA, hours != nil && hours.Sleeps()},
		}
		for _, l := range limits {
			if l.limit == nil {
				continue
			}
			r := ExpectResult{Rule: rule, What: l.what, Limit: fmt.Sprint(*l.limit)}
			// The side before is held to what was expected then.
			beforeLimit := l.limit
			if was != nil {
				beforeLimit = l.old
			}
			r.Before, r.BeforeState = budgetState(b, l.before, beforeLimit, l.counted)
			r.After, r.AfterState = budgetState(a, l.after, l.limit, l.counted)
			if beforeLimit == nil || *beforeLimit != *l.limit {
				r.Changed = true
				if beforeLimit != nil {
					r.WasLimit = fmt.Sprint(*beforeLimit)
				}
			}
			rs = append(rs, r)
		}
	}
	for _, k := range x.Catches {
		r := ExpectResult{Rule: rule, What: "catches " + k.Incident, Limit: "caught"}
		if k.Within > 0 {
			r.Limit = "within " + strings.TrimPrefix(Delay(time.Duration(k.Within)), "+")
		}
		r.Before, r.BeforeState = catchState(c.Before, b, rule, k)
		r.After, r.AfterState = catchState(c.After, a, rule, k)
		// A rule taken away catches nothing: the expectation that it
		// does, still in the file, is broken, unless the change takes it
		// away too.
		if a == nil {
			r.After, r.AfterState = "taken away", ExpectBroken
		}
		if was != nil && !slices.ContainsFunc(was.Catches, func(o ExpectCatch) bool { return o == k }) {
			r.Changed = true
		}
		rs = append(rs, r)
	}
	return rs
}

func oldLimit(was *Expectation, f func(*Budget) *int) *int {
	if was == nil || was.Budget == nil {
		return nil
	}
	return f(was.Budget)
}

// mostInAWeek is the most a rule fired in a whole week of [from, to),
// counted back from to, and the most while people slept; with no whole
// week, the period's, scaled to a week.
func mostInAWeek(r *Report, rule string, from, to time.Time, hours *Hours, out *Checked) (int, int) {
	if r == nil {
		return 0, 0
	}
	const week = 7 * 24 * time.Hour
	type n struct{ all, asleep int }
	if out.Weeks == 0 {
		var t n
		for _, f := range r.Firings {
			if f.Rule == rule && !f.Start.Before(from) && f.Start.Before(to) {
				t.all++
				if hours != nil && hours.Of(f.Start) == Asleep {
					t.asleep++
				}
			}
		}
		scale := 7 / max(out.Scaled, 1.0/24)
		return int(float64(t.all)*scale + 0.5), int(float64(t.asleep)*scale + 0.5)
	}
	weeks := make([]n, out.Weeks)
	for _, f := range r.Firings {
		if f.Rule != rule || !f.Start.Before(to) {
			continue
		}
		i := int(to.Sub(f.Start) / week)
		if i >= out.Weeks || f.Start.Before(from) {
			continue
		}
		weeks[i].all++
		if hours != nil && hours.Of(f.Start) == Asleep {
			weeks[i].asleep++
		}
	}
	most, asleep := 0, 0
	for _, w := range weeks {
		most, asleep = max(most, w.all), max(asleep, w.asleep)
	}
	return most, asleep
}

func budgetState(r *RuleResult, n int, limit *int, counted bool) (string, string) {
	switch {
	case r == nil:
		return "–", ExpectAbsent
	case limit == nil:
		return fmt.Sprint(n), ExpectUnknown
	case !counted:
		return "–", ExpectUnknown
	case r.Unsupported != "":
		return "–", ExpectUnknown
	case n > *limit:
		return fmt.Sprint(n), ExpectBroken
	}
	return fmt.Sprint(n), ExpectMet
}

// catchState is whether a rule caught an incident, as a report judged it:
// not known when the incident isn't in the period's or no record keeps it.
func catchState(rep *Report, r *RuleResult, rule string, k ExpectCatch) (string, string) {
	if r == nil {
		return "–", ExpectAbsent
	}
	if rep == nil || r.Unsupported != "" {
		return "–", ExpectUnknown
	}
	i := slices.IndexFunc(rep.Incidents, func(ir IncidentResult) bool { return ir.ID == k.Incident })
	if i < 0 {
		return "–", ExpectUnknown
	}
	for _, c := range rep.Incidents[i].CaughtBy {
		if c.Rule == rule {
			if k.Within > 0 && c.Delay > time.Duration(k.Within) {
				return Delay(c.Delay), ExpectBroken
			}
			return Delay(c.Delay), ExpectMet
		}
	}
	return "✗", ExpectBroken
}

// heldTo says what an expectation holds a rule to, for a person.
func (r ExpectResult) heldTo(name func(string) string) string {
	var s string
	switch {
	case r.What == "budget week":
		s = "at most " + r.Limit + " a week"
	case r.What == "budget asleep":
		s = "at most " + r.Limit + " a week while people slept"
	case strings.HasPrefix(r.What, "catches "):
		s = "catches " + name(strings.TrimPrefix(r.What, "catches ")) + ", " + r.Limit
	default:
		s = r.What + " " + r.Limit
	}
	switch {
	case r.Changed && r.WasLimit != "":
		s += " (was " + r.WasLimit + ", in this change)"
	case r.Changed:
		s += " (new in this change)"
	}
	return s
}

// verdictOf says what became of an expectation, for a person.
func (r ExpectResult) verdictOf() string {
	switch {
	case r.BrokenByChange():
		return "✗ broken by this change"
	case r.AlreadyBroken():
		return "broken before too"
	case r.BeforeState == ExpectBroken && r.AfterState == ExpectMet:
		return "met now"
	case r.AfterState == ExpectMet:
		return "met"
	case r.AfterState == ExpectAbsent:
		return "rule taken away"
	}
	return "not known"
}

// ordered are the results, what the change broke first.
func (k *Checked) ordered() []ExpectResult {
	rank := func(r ExpectResult) int {
		switch {
		case r.BrokenByChange():
			return 0
		case r.AlreadyBroken():
			return 1
		case r.AfterState == ExpectUnknown:
			return 2
		}
		return 3
	}
	out := slices.Clone(k.Results)
	slices.SortStableFunc(out, func(a, b ExpectResult) int { return rank(a) - rank(b) })
	return out
}

// counts says how many the change broke, were broken already, and were met.
func (k *Checked) counts() string {
	broken, already, met := 0, 0, 0
	for _, r := range k.Results {
		switch {
		case r.BrokenByChange():
			broken++
		case r.AlreadyBroken():
			already++
		case r.AfterState == ExpectMet:
			met++
		}
	}
	return fmt.Sprintf("%d broken by this change, %d broken before too, %d met", broken, already, met)
}

// over says what period the budgets are held over.
func (k *Checked) over() string {
	if k.Weeks == 0 {
		return fmt.Sprintf("Budgets are a week's: the period is %.1f days, its firings scaled to a week.", k.Scaled)
	}
	if k.Weeks == 1 {
		return "Budgets are held to the period's last whole week."
	}
	return fmt.Sprintf("Budgets are held to each of the period's %d whole weeks, counted back from its end: the most in one is shown.", k.Weeks)
}

// ExpectationsText writes what a change does to the expectations of the
// rules it touches, after a comparison's text.
func ExpectationsText(w io.Writer, k *Checked) error {
	if k == nil {
		return nil
	}
	fmt.Fprintf(w, "\nExpectations: %s\n", k.counts())
	if len(k.Results) > 0 {
		fmt.Fprintln(w, k.over())
		rows := [][]string{{"RULE", "HELD TO", "BEFORE", "AFTER", ""}}
		for _, r := range k.ordered() {
			rows = append(rows, []string{Clean(r.Rule), r.heldTo(Clean), r.Before, r.After, r.verdictOf()})
		}
		if err := writeTable(w, rows); err != nil {
			return err
		}
	}
	if len(k.Unexpected) > 0 {
		names := make([]string, 0, len(k.Unexpected))
		for _, n := range k.Unexpected {
			names = append(names, Clean(n))
		}
		fmt.Fprintf(w, "Rules this change adds that no expectation holds: %s\n", strings.Join(names, ", "))
	}
	return nil
}

// expectationsPart is the comparison's expectations, for a pull request.
func expectationsPart(k *Checked) mdPart {
	head := "#### Expectations\n\n" + k.counts() + "."
	if len(k.Results) > 0 {
		head += " " + k.over() + "\n\n" + mdRow([]string{"Rule", "Held to", "Before", "After", ""}) + mdRow([]string{"---", "---", "--:", "--:", "---"})
	} else {
		head += "\n"
	}
	part := mdPart{head: head, foot: "\n", noun: "expectations"}
	code := func(s string) string { return md(s, 100, true) }
	for _, r := range k.ordered() {
		v := r.verdictOf()
		if r.BrokenByChange() {
			v = "**" + v + "**"
		}
		part.rows = append(part.rows, mdRow([]string{md(r.Rule, 200, true), r.heldTo(code), r.Before, r.After, v}))
	}
	if len(k.Unexpected) > 0 {
		names := make([]string, 0, len(k.Unexpected))
		for _, n := range k.Unexpected[:min(len(k.Unexpected), 20)] {
			names = append(names, md(n, 200, false))
		}
		more := ""
		if n := len(k.Unexpected) - 20; n > 0 {
			more = fmt.Sprintf(" and %d more", n)
		}
		part.foot = "\nRules this change adds that no expectation holds: " + strings.Join(names, ", ") + more + ".\n\n"
	}
	return part
}
