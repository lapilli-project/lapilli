package verdict

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/rules"
)

// Expectations are what rules are held to, written beside them
// (docs/design/expectations.md): how often each may fire in a week, how
// often while people slept, the incidents it must keep catching, and the
// scenarios, promtool's own test files, it must pass.
type Expectations struct {
	Expectations []Expectation `yaml:"expectations" json:"expectations"`
}

// Expectation is one rule's, or a group's rules', by name; or the rules
// whose labels a selector matches, as --rules-select takes one (labels:
// severity=page, team in (payments,cart)).
type Expectation struct {
	Rule    string        `yaml:"rule,omitempty" json:"rule,omitempty"`
	Group   string        `yaml:"group,omitempty" json:"group,omitempty"`
	Labels  string        `yaml:"labels,omitempty" json:"labels,omitempty"`
	Budget  *Budget       `yaml:"budget,omitempty" json:"budget,omitempty"`
	Catches []ExpectCatch `yaml:"catches,omitempty" json:"catches,omitempty"`
	// Scenarios are promtool test files the rule must pass, each a path
	// relative to the expectations file, inside its directory
	// (ScenarioPath), run against the rule's group as Lapilli has it.
	Scenarios []string `yaml:"scenarios,omitempty" json:"scenarios,omitempty"`
	// sel is Labels, read.
	sel rules.Selector
}

// Budget is how often a rule may fire in each whole week of a period, and
// of those, how often while people slept, by the hours it is counted by,
// and how often a firing came again within --refire of the same alert
// resolving.
type Budget struct {
	Week    *int `yaml:"week,omitempty" json:"week,omitempty"`
	Asleep  *int `yaml:"asleep,omitempty" json:"asleep,omitempty"`
	Refires *int `yaml:"refires,omitempty" json:"refires,omitempty"`
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

// MarshalText writes it as a rule file writes a duration: 5m, 1h30m.
func (d Within) MarshalText() ([]byte, error) { return []byte(Duration(time.Duration(d))), nil }

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

// MaxExpectationScenarios is the most scenarios a file may hold, in all
// its expectations: each is a run of promtool, on each side of a change.
const MaxExpectationScenarios = 200

// maxScenarioPath is the longest a scenario's path may be, in bytes.
const maxScenarioPath = 512

// ScenarioPath says whether a scenario's path is one an expectations file
// may name: relative to the file, inside its directory, written as it is
// (no ., .. or empty part, no leading or doubled /, no \), 512 bytes at
// most, of characters that show. What it leads to is read inside that
// directory alone (scenario.ReadTest), never run.
func ScenarioPath(p string) error {
	bad := p == "" || len(p) > maxScenarioPath || !utf8.ValidString(p) || strings.ContainsFunc(p, func(r rune) bool {
		return r == '\\' || unicode.IsControl(r) || rules.Turns(r)
	})
	for _, part := range strings.Split(p, "/") {
		bad = bad || part == "" || part == "." || part == ".."
	}
	if bad {
		return fmt.Errorf("scenario %q: want a path relative to the expectations file, inside its directory, such as tests/restarts.yaml", Clean(p))
	}
	return nil
}

// MaxExpectResults is the most a comparison holds the rules that differ
// to: each rule to each budget and catch that holds it, before and after.
// A group's 2,000 catches held against every rule of the group a change
// touches would be millions, each kept and shown.
const MaxExpectResults = 100000

// ReadExpectations reads an expectations file strictly: a key it doesn't
// know, a value of another kind, an expectation naming no rule, group or
// labels, or a budget below nothing is an error, with its line.
func ReadExpectations(r io.Reader) (*Expectations, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxExpectations+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxExpectations {
		return nil, fmt.Errorf("more than %d bytes", MaxExpectations)
	}
	// Read first as the rules are, in time linear in it, its values and
	// what its aliases expand to bounded: a file from a pull request,
	// which lapilli serve's backtests read too.
	if _, err := rules.DecodeYAML(context.Background(), b); err != nil {
		return nil, err
	}
	var e Expectations
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&e); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return NewExpectations(e.Expectations)
}

// NewExpectations checks expectations read, from a file or from where else
// they are declared (lapilli serve's --config), as ReadExpectations does:
// each names one of a rule, a group and labels, and holds it to something;
// the same budget or incident given twice for one of them is refused. Each
// rule's, each group's and each selector's entries are made one.
func NewExpectations(list []Expectation) (*Expectations, error) {
	if len(list) > MaxExpectationEntries {
		return nil, fmt.Errorf("%d expectations: at most %d", len(list), MaxExpectationEntries)
	}
	catches, scenarios := 0, 0
	for _, x := range list {
		catches += len(x.Catches)
		scenarios += len(x.Scenarios)
	}
	if catches > MaxExpectationCatches {
		return nil, fmt.Errorf("%d catches: at most %d in all", catches, MaxExpectationCatches)
	}
	if scenarios > MaxExpectationScenarios {
		return nil, fmt.Errorf("%d scenarios: at most %d in all", scenarios, MaxExpectationScenarios)
	}
	e := &Expectations{Expectations: slices.Clone(list)}
	for i := range e.Expectations {
		x := &e.Expectations[i]
		at := fmt.Sprintf("expectation %d", i+1)
		named := 0
		for _, v := range []string{x.Rule, x.Group, x.Labels} {
			if v != "" {
				named++
			}
		}
		b := x.Budget
		switch {
		case named != 1:
			return nil, fmt.Errorf("%s: give rule, group or labels, one of them", at)
		case b == nil && len(x.Catches) == 0 && len(x.Scenarios) == 0:
			return nil, fmt.Errorf("%s: nothing to hold %s to: give a budget, catches or scenarios", at, x.name())
		case b != nil && b.Week == nil && b.Asleep == nil && b.Refires == nil:
			return nil, fmt.Errorf("%s: a budget of nothing: give week, asleep or refires", at)
		case b != nil && (below(b.Week) || below(b.Asleep) || below(b.Refires)):
			return nil, fmt.Errorf("%s: a budget below nothing", at)
		}
		if x.Labels != "" {
			sel, err := rules.ParseSelector(x.Labels)
			if err == nil && sel.Empty() {
				err = errors.New("it selects every rule: give a rule, a group, or a label to select by")
			}
			if err != nil {
				return nil, fmt.Errorf("%s: labels: %w", at, err)
			}
			// As written, its spaces aside: one selector is one entry.
			x.Labels, x.sel = sel.String(), sel
		}
		for _, c := range x.Catches {
			if strings.TrimSpace(c.Incident) == "" {
				return nil, fmt.Errorf("%s: a catch names no incident", at)
			}
		}
		for _, p := range x.Scenarios {
			if err := ScenarioPath(p); err != nil {
				return nil, fmt.Errorf("%s: %w", at, err)
			}
		}
	}
	return e.merged()
}

func below(n *int) bool { return n != nil && *n < 0 }

// merged is the file with each rule's, and each group's, entries made one:
// what is held is then compared item by item. The same budget, or the same
// incident, given twice for one rule or group is refused: which holds isn't
// for the order to decide. Each incident is looked for among those of its
// rule or group in a set.
func (e *Expectations) merged() (*Expectations, error) {
	out := &Expectations{Expectations: []Expectation{}}
	at := map[[3]string]int{}
	caught, scenarios := map[[4]string]bool{}, map[[4]string]bool{}
	for _, x := range e.Expectations {
		key := [3]string{x.Rule, x.Group, x.Labels}
		i, seen := at[key]
		if !seen {
			at[key] = len(out.Expectations)
			y := Expectation{Rule: x.Rule, Group: x.Group, Labels: x.Labels, sel: x.sel}
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
			if x.Budget.Refires != nil {
				if y.Budget.Refires != nil {
					return nil, fmt.Errorf("%s: a budget of re-fires given twice", x.name())
				}
				y.Budget.Refires = x.Budget.Refires
			}
		}
		for _, c := range x.Catches {
			k := [4]string{x.Rule, x.Group, x.Labels, c.Incident}
			if caught[k] {
				return nil, fmt.Errorf("%s: incident %s given twice", x.name(), c.Incident)
			}
			caught[k] = true
			y.Catches = append(y.Catches, c)
		}
		for _, p := range x.Scenarios {
			k := [4]string{x.Rule, x.Group, x.Labels, p}
			if scenarios[k] {
				return nil, fmt.Errorf("%s: scenario %s given twice", x.name(), Clean(p))
			}
			scenarios[k] = true
			y.Scenarios = append(y.Scenarios, p)
		}
	}
	return out, nil
}

func (x Expectation) name() string {
	switch {
	case x.Rule != "":
		return x.Rule
	case x.Group != "":
		return "group " + x.Group
	}
	return "labels " + x.Labels
}

// Selector is what it names its rules by, as written: rule: NAME,
// group: NAME or labels: SELECTOR.
func (x Expectation) Selector() string {
	switch {
	case x.Rule != "":
		return "rule: " + x.Rule
	case x.Group != "":
		return "group: " + x.Group
	}
	return "labels: " + x.Labels
}

// For says whether it holds a rule of that name and group, its labels
// unknown: an expectation by labels holds none of them. A name several
// rules share is told apart in a run (CheckoutErrors [severity=page]):
// rule: CheckoutErrors holds each of them; the name told apart, one.
func (x Expectation) For(rule, group string) bool { return x.Holds(rule, group, nil) }

// Holds says whether it holds a rule of that name, group and labels (the
// labels its alerts carry, its group's among them): by labels, when every
// requirement of its selector holds of them.
func (x Expectation) Holds(rule, group string, labels map[string]string) bool {
	switch {
	case x.Rule != "":
		return x.Rule == rule || strings.HasPrefix(rule, x.Rule+" [")
	case x.Group != "":
		return x.Group == group
	}
	return labels != nil && x.sel.Matches(labels)
}

// same says two expectations name the same rule, group or labels: the one
// is the other, as a change may have made it.
func (x Expectation) same(y Expectation) bool {
	return x.Rule == y.Rule && x.Group == y.Group && x.Labels == y.Labels
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
	// Of a scenario: BeforeWhy and AfterWhy, why it is not known on each
	// side, in Lapilli's words; Said, what promtool said of it failing
	// after the change, or why its test file can't be read: the test
	// file's and the rules' own words. Edited says the change edited its
	// test file; Unrun, that it couldn't be run after the change, promtool
	// given and the rule one promtool runs (its test file unread, a time
	// out, a crash), so it can't be judged.
	BeforeWhy string `json:"before_why,omitempty"`
	AfterWhy  string `json:"after_why,omitempty"`
	Said      string `json:"said,omitempty"`
	Edited    bool   `json:"edited,omitempty"`
	Unrun     bool   `json:"unrun,omitempty"`
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

// item is one thing an expectation holds a rule to: a budget's limit, a
// catch or a scenario, by what it is. fromBefore says it was read from the
// expectations before the change (--expect-before), whose directory a
// scenario's path is read in.
type item struct {
	what           string
	limit          *int         // a budget's
	catch          *ExpectCatch // a catch's
	scenario       string       // a scenario's path
	asleep, refire bool
	fromBefore     bool
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
		if b.Refires != nil {
			out = append(out, item{what: "budget refires", limit: b.Refires, refire: true})
		}
	}
	for i := range x.Catches {
		out = append(out, item{what: "catches " + x.Catches[i].Incident, catch: &x.Catches[i]})
	}
	for _, p := range x.Scenarios {
		out = append(out, item{what: ScenarioWhat + p, scenario: p})
	}
	return out
}

// itemsFrom are its items, read from the expectations before the change
// when fromBefore says so.
func (x *Expectation) itemsFrom(fromBefore bool) []item {
	out := x.items()
	for i := range out {
		out[i].fromBefore = fromBefore
	}
	return out
}

// ScenarioWhat begins what a scenario holds a rule to: "scenario
// tests/restarts.yaml".
const ScenarioWhat = "scenario "

// ScenarioHeld is what a scenario holds a rule to: passing it.
const ScenarioHeld = "passes"

// ExpectItem is one thing an expectation holds a rule to, as a person
// reads it: What, "budget week", "budget asleep", "budget refires",
// "catches INC-43" or "scenario tests/restarts.yaml"; Held, its bound
// ("10", "within 5m", "caught", "passes"). Limit is a budget's, Catch a
// catch's, Scenario a scenario's path.
type ExpectItem struct {
	What, Held string
	Limit      *int
	Catch      *ExpectCatch
	Scenario   string
}

// Asleep and Refires say what a budget counts: firings while people
// slept, re-fires; neither, every firing.
func (it ExpectItem) Asleep() bool  { return it.What == "budget asleep" }
func (it ExpectItem) Refires() bool { return it.What == "budget refires" }

// Items are what it holds a rule to, its budget's first.
func (x *Expectation) Items() []ExpectItem {
	var out []ExpectItem
	for _, it := range x.items() {
		out = append(out, ExpectItem{What: it.what, Held: it.limitText(), Limit: it.limit, Catch: it.catch, Scenario: it.scenario})
	}
	return out
}

// BudgetItems are what a budget holds a rule to, as Items gives them.
func BudgetItems(b *Budget) []ExpectItem {
	x := Expectation{Budget: b}
	return x.Items()
}

func (it item) limitText() string {
	if it.scenario != "" {
		return ScenarioHeld
	}
	if it.catch == nil {
		return fmt.Sprint(*it.limit)
	}
	if it.catch.Within > 0 {
		return "within " + strings.TrimPrefix(Delay(time.Duration(it.catch.Within)), "+")
	}
	return "caught"
}

// weekCounts are, of one side's firings, by rule, each whole week's, how
// many came while people slept and how many re-fired, counted once.
type weekCounts map[string][]struct{ all, asleep, refired int }

// ScenarioRunner runs the scenarios expectations hold rules to: a promtool
// test file against a rule's group, on one side of a change.
type ScenarioRunner interface {
	// RunScenario runs path, as the expectations name it (fromBefore says
	// those before the change, --expect-before, whose directory it is read
	// in), against the group of rule, as side ("before" or "after") has it.
	RunScenario(side string, fromBefore bool, rule, path string) ScenarioResult
}

// ScenarioResult is what running a scenario came to: State met, broken or
// not known; of not known, Why, in Lapilli's words; Said, what promtool
// said of it failing, or why its test file can't be read, in the file's
// and the rules' own words; Test, a hash of the test file run, "" when it
// wasn't read; Unrun, that it couldn't be run, promtool given and the rule
// one it runs (Comparison.Summary's scenarios_unrun).
type ScenarioResult struct {
	State, Why, Said, Test string
	Unrun                  bool
}

// ScenariosNotRun runs no scenario: each is not known, for why. Never met.
type ScenariosNotRun string

func (why ScenariosNotRun) RunScenario(string, bool, string, string) ScenarioResult {
	return ScenarioResult{State: ExpectUnknown, Why: string(why)}
}

// Why a scenario wasn't run: lapilli verdict wasn't given promtool, or a
// pull request's backtest ran through lapilli serve, which runs none it is
// sent.
const (
	NoPromtool      ScenariosNotRun = "not run: no --promtool"
	NotThroughServe ScenariosNotRun = "not run through lapilli serve"
)

// CheckExpectations holds the rules a comparison's change touches to what
// after says of them; with before (--expect-before), the side before to
// what was expected then, and what the change did to the expectations is
// said. hours count the firings while people slept. Past MaxExpectResults
// it stops, and says what to do.
func CheckExpectations(c *Comparison, after, before *Expectations, hours *Hours) (*Checked, error) {
	return CheckExpectationsBy(c, after, before, func(string) *Hours { return hours })
}

// CheckExpectationsBy is CheckExpectations, each rule's firings counted
// while people slept by the hours hoursOf gives it: lapilli serve's, its
// team's own where it has them. nil for none.
func CheckExpectationsBy(c *Comparison, after, before *Expectations, hoursOf func(rule string) *Hours) (*Checked, error) {
	out := &Checked{Results: []ExpectResult{}}
	span := c.To.Sub(c.From)
	out.Weeks = int(span / (7 * 24 * time.Hour))
	if out.Weeks == 0 {
		out.Scaled = span.Hours() / 24
	}
	countsB, countsA := out.count(c.Before, c.From, c.To, hoursOf), out.count(c.After, c.From, c.To, hoursOf)
	incsB, incsA := incidentsByID(c.Before), incidentsByID(c.After)
	var run ScenarioRunner = NoPromtool
	if c.Scenarios != nil {
		run = c.Scenarios
	}
	for _, rc := range c.Rules {
		h := hoursOf(rc.Rule)
		sleeps := h != nil && h.Sleeps()
		// Checked as each rule's are added: one rule adds a few
		// expectations' items at most, each file's 2,000 catches at most.
		if len(out.Results) > MaxExpectResults {
			return nil, errHeldTooMuch
		}
		b, a := rc.Before, rc.After
		group, labels := "", rc.AfterLabels
		if a != nil {
			group = a.Group
		} else if b != nil {
			group, labels = b.Group, rc.BeforeLabels
		}
		held := false
		judge := func(it item, beforeLimit item, r ExpectResult) ExpectResult {
			if it.scenario != "" {
				var sb, sa ScenarioResult
				r.Before, r.BeforeState = "–", ExpectAbsent
				if b != nil {
					sb = run.RunScenario("before", beforeLimit.fromBefore, rc.Rule, beforeLimit.scenario)
					r.Before, r.BeforeState, r.BeforeWhy = scenarioShown(sb)
				}
				r.After, r.AfterState = "taken away", ExpectBroken
				if a != nil {
					sa = run.RunScenario("after", it.fromBefore, rc.Rule, it.scenario)
					r.After, r.AfterState, r.AfterWhy = scenarioShown(sa)
					r.Said, r.Unrun = sa.Said, sa.Unrun && !r.Removed
				}
				r.Edited = sb.Test != "" && sa.Test != "" && sb.Test != sa.Test
				return r
			}
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
			wb, sb, rb := out.most(countsB, rc.Rule)
			wa, sa, ra := out.most(countsA, rc.Rule)
			nb, na := wb, wa
			switch {
			case it.asleep:
				nb, na = sb, sa
			case it.refire:
				nb, na = rb, ra
			}
			counted := !it.asleep || sleeps
			r.Before, r.BeforeState = budgetState(b, nb, beforeLimit.limit, counted)
			r.After, r.AfterState = budgetState(a, na, it.limit, counted)
			return r
		}
		for i := range after.Expectations {
			x := &after.Expectations[i]
			if !x.Holds(rc.Rule, group, labels) {
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
			for _, it := range was.itemsFrom(before != nil) {
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
			for _, it := range was.itemsFrom(before != nil) {
				if _, gone := old[it.what]; gone && before != nil {
					r := ExpectResult{Rule: rc.Rule, What: it.what, Limit: it.limitText(), Removed: true}
					out.Results = append(out.Results, judge(it, it, r))
				}
			}
		}
		// What held the rule before and holds it no more: an expectation
		// the change took away whole, or one by group of a rule the change
		// moved out of the group. The rule's group before decides.
		bGroup, bLabels := group, labels
		if b != nil {
			bGroup, bLabels = b.Group, rc.BeforeLabels
		}
		ys := before
		if ys == nil {
			ys = after
		}
		for j := range ys.Expectations {
			y := &ys.Expectations[j]
			if !y.Holds(rc.Rule, bGroup, bLabels) || slices.ContainsFunc(after.Expectations, func(x Expectation) bool { return x.same(*y) && x.Holds(rc.Rule, group, labels) }) {
				continue
			}
			moved := slices.ContainsFunc(after.Expectations, func(x Expectation) bool { return x.same(*y) })
			for _, it := range y.itemsFrom(before != nil) {
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

// scenarioShown is a scenario's result as a side shows it: passes, fails,
// or not known and why.
func scenarioShown(x ScenarioResult) (shown, state, why string) {
	switch x.State {
	case ExpectMet:
		return "passes", ExpectMet, ""
	case ExpectBroken:
		return "fails", ExpectBroken, ""
	}
	return "–", ExpectUnknown, x.Why
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
func (k *Checked) count(r *Report, from, to time.Time, hoursOf func(string) *Hours) weekCounts {
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
			ws = make([]struct{ all, asleep, refired int }, n)
			out[f.Rule] = ws
		}
		ws[i].all++
		if h := hoursOf(f.Rule); h != nil && h.Of(f.Start) == Asleep {
			ws[i].asleep++
		}
		if f.Refired {
			ws[i].refired++
		}
	}
	return out
}

// most is the most a rule fired in a week, while people slept, and
// re-fired; with no whole week, the period's, scaled to a week.
func (k *Checked) most(counts weekCounts, rule string) (int, int, int) {
	most, asleep, refired := 0, 0, 0
	for _, w := range counts[rule] {
		most, asleep, refired = max(most, w.all), max(asleep, w.asleep), max(refired, w.refired)
	}
	if k.Weeks == 0 {
		scale := 7 / max(k.Scaled, 1.0/24)
		return int(float64(most)*scale + 0.5), int(float64(asleep)*scale + 0.5), int(float64(refired)*scale + 0.5)
	}
	return most, asleep, refired
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
	return CaughtWithin(rep.Incidents[i].CaughtBy, rule, k)
}

// CaughtWithin is whether rule is among the rules that caught an incident,
// within the catch's delay of its start: its delay, met or broken; ✗,
// broken, when it isn't among them.
func CaughtWithin(by []Catch, rule string, k ExpectCatch) (string, string) {
	for _, c := range by {
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
	case r.What == "budget refires":
		s = "at most " + r.Limit + " re-fires a week"
	case strings.HasPrefix(r.What, "catches "):
		s = "catches " + name(strings.TrimPrefix(r.What, "catches ")) + ", " + r.Limit
	case strings.HasPrefix(r.What, ScenarioWhat):
		s = "passes the scenario " + name(strings.TrimPrefix(r.What, ScenarioWhat))
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
	case r.Edited:
		s += " (its test file edited in this change)"
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
	case r.AfterWhy != "":
		return "not known: " + r.AfterWhy
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
	for _, r := range k.ordered() {
		if r.Said == "" {
			continue
		}
		fmt.Fprintf(w, "promtool said, of %s and %s after the change:\n", Clean(r.Rule), Clean(strings.TrimPrefix(r.What, ScenarioWhat)))
		for _, l := range strings.Split(r.Said, "\n") {
			fmt.Fprintf(w, "    %s\n", Clean(l))
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
	// What promtool said of each scenario failing: its lines, each a code
	// span, after the table.
	var said []string
	for _, r := range k.ordered() {
		if r.Said == "" {
			continue
		}
		var lines []string
		for i, l := range strings.Split(r.Said, "\n") {
			if i == maxSaidLines {
				lines = append(lines, "…")
				break
			}
			lines = append(lines, md(strings.TrimSpace(l), 200, false))
		}
		said = append(said, "- "+md(r.Rule, 200, false)+", "+md(strings.TrimPrefix(r.What, ScenarioWhat), 100, false)+": "+strings.Join(lines, " ")+"\n")
	}
	var after []string
	if len(said) > 0 {
		after = append(after, "promtool said, of the scenarios after the change:\n\n"+strings.Join(said, ""))
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
		after = append(after, "Rules this change adds that no expectation holds: "+strings.Join(names, ", ")+more+".\n")
	}
	for _, x := range after {
		part.foot += x + "\n"
	}
	return part
}

// maxSaidLines is how many of the lines promtool said of a scenario
// failing a pull request's comment shows.
const maxSaidLines = 8

// BudgetAnnotation is the annotation a rule may carry its budget in, for
// teams that keep everything in the rule: "10/week, 2 asleep, 3 refires".
const BudgetAnnotation = "lapilli.dev/budget"

// maxBudgetText is the longest an annotation's budget is read, in bytes;
// maxBudget, the most a budget's number may be.
const (
	maxBudgetText = 100
	maxBudget     = 1000000
)

// errBudgetText is what ParseBudget says of a budget it can't read. A
// rule's annotation is its author's: what is wrong is said without its
// text.
var errBudgetText = errors.New(`want a budget such as "10/week, 2 asleep, 3 refires": a whole number and what it counts, each once`)

// ParseBudget reads a budget as an annotation writes it: parts separated
// by commas, each a whole number and what it counts, N/week, N asleep and
// N refires (or re-fires), each once, one at least.
func ParseBudget(s string) (*Budget, error) {
	if len(s) > maxBudgetText {
		return nil, fmt.Errorf("more than %d bytes: %w", maxBudgetText, errBudgetText)
	}
	b := &Budget{}
	parts := strings.Split(s, ",")
	if len(parts) > 3 {
		return nil, errBudgetText
	}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		num, what := p, ""
		if i := strings.IndexAny(p, "/ "); i > 0 {
			num, what = p[:i], strings.TrimSpace(strings.TrimPrefix(p[i:], "/"))
		}
		n, err := strconv.Atoi(num)
		if err != nil || n < 0 || n > maxBudget || strings.HasPrefix(num, "+") {
			return nil, errBudgetText
		}
		var at **int
		switch what {
		case "week":
			if !strings.Contains(p, "/") {
				return nil, errBudgetText
			}
			at = &b.Week
		case "asleep":
			at = &b.Asleep
		case "refires", "re-fires":
			at = &b.Refires
		default:
			return nil, errBudgetText
		}
		if *at != nil {
			return nil, errBudgetText
		}
		*at = &n
	}
	return b, nil
}
