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

// MaxExpectationEntries is the most expectations a file may hold: each is
// held against every rule a change touches.
const MaxExpectationEntries = 2000

// MaxExpectationCatches is the most catches a file may hold, in all its
// expectations, as many as it may hold expectations: each is held against
// every rule its expectation holds that a change touches.
const MaxExpectationCatches = 2000

// MaxExpectResults is the most a comparison holds the rules that differ
// to: each rule to each budget and catch that holds it, before and after.
// A group's 2,000 catches held against every rule of the group a change
// touches would be millions, each kept and shown.
const MaxExpectResults = 100000

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
	if len(e.Expectations) > MaxExpectationEntries {
		return nil, fmt.Errorf("%d expectations: at most %d", len(e.Expectations), MaxExpectationEntries)
	}
	catches := 0
	for _, x := range e.Expectations {
		catches += len(x.Catches)
	}
	if catches > MaxExpectationCatches {
		return nil, fmt.Errorf("%d catches: at most %d in all", catches, MaxExpectationCatches)
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
	return e.merged()
}

// merged is the file with each rule's, and each group's, entries made one:
// what is held is then compared item by item. The same budget, or the same
// incident, given twice for one rule or group is refused: which holds isn't
// for the order to decide. Each incident is looked for among those of its
// rule or group in a set.
func (e *Expectations) merged() (*Expectations, error) {
	out := &Expectations{Expectations: []Expectation{}}
	at := map[[2]string]int{}
	caught := map[[3]string]bool{}
	for _, x := range e.Expectations {
		key := [2]string{x.Rule, x.Group}
		i, seen := at[key]
		if !seen {
			at[key] = len(out.Expectations)
			y := Expectation{Rule: x.Rule, Group: x.Group}
			out.Expectations = append(out.Expectations, y)
			i = at[key]
		}
		y := &out.Expectations[i]
		if x.Budget != nil {
			if y.Budget == nil {
				y.Budget = &Budget{}
			}
			if x.Budget.Week != nil {
				if y.Budget.Week != nil {
					return nil, fmt.Errorf("%s: a budget for a week given twice", x.name())
				}
				y.Budget.Week = x.Budget.Week
			}
			if x.Budget.Asleep != nil {
				if y.Budget.Asleep != nil {
					return nil, fmt.Errorf("%s: a budget while people slept given twice", x.name())
				}
				y.Budget.Asleep = x.Budget.Asleep
			}
		}
		for _, c := range x.Catches {
			k := [3]string{x.Rule, x.Group, c.Incident}
			if caught[k] {
				return nil, fmt.Errorf("%s: incident %s given twice", x.name(), c.Incident)
			}
			caught[k] = true
			y.Catches = append(y.Catches, c)
		}
	}
	return out, nil
}

func (x Expectation) name() string {
	if x.Rule != "" {
		return x.Rule
	}
	return "group " + x.Group
}

// For says whether it holds a rule of that name and group. A name several
// rules share is told apart in a run (CheckoutErrors [severity=page]):
// rule: CheckoutErrors holds each of them; the name told apart, one.
func (x Expectation) For(rule, group string) bool {
	if x.Rule != "" {
		return x.Rule == rule || strings.HasPrefix(rule, x.Rule+" [")
	}
	return x.Group == group
}

// same says two expectations name the same rule or group: the one is the
// other, as a change may have made it.
func (x Expectation) same(y Expectation) bool { return x.Rule == y.Rule && x.Group == y.Group }

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
	// Changed says the change changed the expectation itself
	// (--expect-before): new in it when WasLimit is empty; else from
	// WasLimit, which the side before is held to. Removed says the change
	// took it away: both sides are held to what it was, and nothing it
	// finds fails the change, as a budget raised doesn't.
	Changed  bool   `json:"changed,omitempty"`
	WasLimit string `json:"was_limit,omitempty"`
	Removed  bool   `json:"removed,omitempty"`
	// Moved says it was taken away as the rule left the group that held
	// it: the expectation is there still, and no longer holds this rule.
	Moved bool `json:"moved,omitempty"`
}

// BrokenByChange says the change broke it: broken after, and met before
// or a rule the change adds. One broken before too isn't the change's, nor
// one the change took away.
func (r ExpectResult) BrokenByChange() bool {
	return !r.Removed && r.AfterState == ExpectBroken && (r.BeforeState == ExpectMet || r.BeforeState == ExpectAbsent)
}

// AlreadyBroken says it was broken before the change, and still is.
func (r ExpectResult) AlreadyBroken() bool {
	return !r.Removed && r.AfterState == ExpectBroken && r.BeforeState == ExpectBroken
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

// item is one thing an expectation holds a rule to: a budget's limit, or a
// catch, by what it is.
type item struct {
	what   string
	limit  *int         // a budget's
	catch  *ExpectCatch // a catch's
	asleep bool
}

func (x *Expectation) items() []item {
	var out []item
	if x == nil {
		return nil
	}
	if b := x.Budget; b != nil {
		if b.Week != nil {
			out = append(out, item{what: "budget week", limit: b.Week})
		}
		if b.Asleep != nil {
			out = append(out, item{what: "budget asleep", limit: b.Asleep, asleep: true})
		}
	}
	for i := range x.Catches {
		out = append(out, item{what: "catches " + x.Catches[i].Incident, catch: &x.Catches[i]})
	}
	return out
}

func (it item) limitText() string {
	if it.catch == nil {
		return fmt.Sprint(*it.limit)
	}
	if it.catch.Within > 0 {
		return "within " + strings.TrimPrefix(Delay(time.Duration(it.catch.Within)), "+")
	}
	return "caught"
}

// weekCounts are, of one side's firings, by rule, each whole week's and
// how many came while people slept, counted once.
type weekCounts map[string][]struct{ all, asleep int }

// CheckExpectations holds the rules a comparison's change touches to what
// after says of them; with before (--expect-before), the side before to
// what was expected then, and what the change did to the expectations is
// said. hours count the firings while people slept. Past MaxExpectResults
// it stops, and says what to do.
func CheckExpectations(c *Comparison, after, before *Expectations, hours *Hours) (*Checked, error) {
	out := &Checked{Results: []ExpectResult{}}
	span := c.To.Sub(c.From)
	out.Weeks = int(span / (7 * 24 * time.Hour))
	if out.Weeks == 0 {
		out.Scaled = span.Hours() / 24
	}
	countsB, countsA := out.count(c.Before, c.From, c.To, hours), out.count(c.After, c.From, c.To, hours)
	incsB, incsA := incidentsByID(c.Before), incidentsByID(c.After)
	sleeps := hours != nil && hours.Sleeps()
	for _, rc := range c.Rules {
		// Checked as each rule's are added: one rule adds a few
		// expectations' items at most, each file's 2,000 catches at most.
		if len(out.Results) > MaxExpectResults {
			return nil, errHeldTooMuch
		}
		b, a := rc.Before, rc.After
		group := ""
		if a != nil {
			group = a.Group
		} else if b != nil {
			group = b.Group
		}
		held := false
		judge := func(it item, beforeLimit item, r ExpectResult) ExpectResult {
			if it.catch != nil {
				r.Before, r.BeforeState = catchState(c.Before, incsB, b, rc.Rule, *beforeLimit.catch)
				r.After, r.AfterState = catchState(c.After, incsA, a, rc.Rule, *it.catch)
				// A rule taken away catches nothing: the expectation that
				// it does, still in the file, is broken.
				if a == nil {
					r.After, r.AfterState = "taken away", ExpectBroken
				}
				return r
			}
			wb, sb := out.most(countsB, rc.Rule)
			wa, sa := out.most(countsA, rc.Rule)
			nb, na := wb, wa
			if it.asleep {
				nb, na = sb, sa
			}
			counted := !it.asleep || sleeps
			r.Before, r.BeforeState = budgetState(b, nb, beforeLimit.limit, counted)
			r.After, r.AfterState = budgetState(a, na, it.limit, counted)
			return r
		}
		for i := range after.Expectations {
			x := &after.Expectations[i]
			if !x.For(rc.Rule, group) {
				continue
			}
			held = true
			// What was expected of it before, by the same rule or group:
			// the same file's own, without --expect-before.
			var was *Expectation
			if before == nil {
				was = x
			} else {
				for j := range before.Expectations {
					if before.Expectations[j].same(*x) {
						was = &before.Expectations[j]
						break
					}
				}
			}
			old := map[string]item{}
			for _, it := range was.items() {
				old[it.what] = it
			}
			for _, it := range x.items() {
				r := ExpectResult{Rule: rc.Rule, What: it.what, Limit: it.limitText()}
				beforeLimit, ok := old[it.what]
				switch {
				case !ok:
					// New in the change: the side before is held to it too,
					// so a change is failed only for what it breaks.
					beforeLimit = it
					r.Changed = before != nil
				case beforeLimit.limitText() != it.limitText():
					r.Changed, r.WasLimit = true, beforeLimit.limitText()
				}
				delete(old, it.what)
				out.Results = append(out.Results, judge(it, beforeLimit, r))
			}
			// What the change took away of it: shown, held to what it was.
			for _, it := range was.items() {
				if _, gone := old[it.what]; gone && before != nil {
					r := ExpectResult{Rule: rc.Rule, What: it.what, Limit: it.limitText(), Removed: true}
					out.Results = append(out.Results, judge(it, it, r))
				}
			}
		}
		// What held the rule before and holds it no more: an expectation
		// the change took away whole, or one by group of a rule the change
		// moved out of the group. The rule's group before decides.
		bGroup := group
		if b != nil {
			bGroup = b.Group
		}
		ys := before
		if ys == nil {
			ys = after
		}
		for j := range ys.Expectations {
			y := &ys.Expectations[j]
			if !y.For(rc.Rule, bGroup) || slices.ContainsFunc(after.Expectations, func(x Expectation) bool { return x.same(*y) && x.For(rc.Rule, group) }) {
				continue
			}
			moved := slices.ContainsFunc(after.Expectations, func(x Expectation) bool { return x.same(*y) })
			for _, it := range y.items() {
				r := ExpectResult{Rule: rc.Rule, What: it.what, Limit: it.limitText(), Removed: true, Moved: moved}
				out.Results = append(out.Results, judge(it, it, r))
			}
		}
		if !held && b == nil && a != nil {
			out.Unexpected = append(out.Unexpected, rc.Rule)
		}
	}
	if len(out.Results) > MaxExpectResults {
		return nil, errHeldTooMuch
	}
	return out, nil
}

// errHeldTooMuch is what CheckExpectations says past MaxExpectResults.
var errHeldTooMuch = fmt.Errorf("the rules that differ are held to more than %d expectations, each rule to each budget and catch that holds it: "+
	"give a catch to the rule it is for (rule:, not group:), or compare fewer rules at once", MaxExpectResults)

// incidentsByID are where each incident of a report is, by its id: the
// first of an id, as a catch is looked for, each looked up once.
func incidentsByID(rep *Report) map[string]int {
	if rep == nil {
		return nil
	}
	out := make(map[string]int, len(rep.Incidents))
	for i, ir := range rep.Incidents {
		if _, ok := out[ir.ID]; !ok {
			out[ir.ID] = i
		}
	}
	return out
}

// count counts a side's firings by rule and whole week, counted back from
// to, the weeks closed at their start, so a firing at from is in the
// first; with no whole week, the period's, as one.
func (k *Checked) count(r *Report, from, to time.Time, hours *Hours) weekCounts {
	out := weekCounts{}
	if r == nil {
		return out
	}
	const week = 7 * 24 * time.Hour
	n := max(k.Weeks, 1)
	for _, f := range r.Firings {
		if f.Start.Before(from) || !f.Start.Before(to) {
			continue
		}
		i := 0
		if k.Weeks > 0 {
			i = int((to.Sub(f.Start) - 1) / week)
		}
		if i >= n {
			continue
		}
		ws := out[f.Rule]
		if ws == nil {
			ws = make([]struct{ all, asleep int }, n)
			out[f.Rule] = ws
		}
		ws[i].all++
		if hours != nil && hours.Of(f.Start) == Asleep {
			ws[i].asleep++
		}
	}
	return out
}

// most is the most a rule fired in a week, and while people slept; with
// no whole week, the period's, scaled to a week.
func (k *Checked) most(counts weekCounts, rule string) (int, int) {
	most, asleep := 0, 0
	for _, w := range counts[rule] {
		most, asleep = max(most, w.all), max(asleep, w.asleep)
	}
	if k.Weeks == 0 {
		scale := 7 / max(k.Scaled, 1.0/24)
		return int(float64(most)*scale + 0.5), int(float64(asleep)*scale + 0.5)
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
// at is where each of the report's incidents is (incidentsByID).
func catchState(rep *Report, at map[string]int, r *RuleResult, rule string, k ExpectCatch) (string, string) {
	if r == nil {
		return "–", ExpectAbsent
	}
	if rep == nil || r.Unsupported != "" {
		return "–", ExpectUnknown
	}
	i, ok := at[k.Incident]
	if !ok {
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
	case r.Moved:
		s += " (no longer holds it: the change moved it out of the group)"
	case r.Removed:
		s += " (taken away in this change)"
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
	case r.Removed && r.AfterState == ExpectBroken && r.BeforeState != ExpectBroken:
		return "taken away; the change would break it"
	case r.Removed:
		return "taken away"
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
		case r.Removed && r.AfterState == ExpectBroken:
			return 1
		case r.AlreadyBroken():
			return 2
		case r.AfterState == ExpectUnknown || r.Removed:
			return 3
		}
		return 4
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
		case r.AfterState == ExpectMet && !r.Removed:
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
