package verdict

import (
	"bytes"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.yaml.in/yaml/v3"
)

// Drafts from the history (docs/design/expectations.md, step 3): each
// rule's expectations as it is now, counted, never written by a model, so
// that the same history gives the same draft each time. lapilli serve
// drafts from the ledger's weeks, lapilli verdict --draft-expectations from
// the period it judged: each counts as it checks budgets, and both write
// the draft with Draft and DraftYAML.

// DraftRule is what a rule did over the weeks a draft is counted from.
type DraftRule struct {
	Rule string
	// Weeks are the whole weeks it was counted over; with none, Scaled are
	// the days it was, its counts scaled to a week, as a comparison holds a
	// period of no whole week.
	Weeks  int
	Scaled float64
	// Most, Asleep and Refires are the most it fired in one week, while
	// people slept, and re-fired. Asleep is counted when Sleeps: the hours
	// it is counted by say when people sleep.
	Most, Asleep, Refires int
	Sleeps                bool
	// Caught are the incidents it caught, the latest first, each with the
	// delay of its first catch of it.
	Caught []DraftCatch
}

// DraftCatch is an incident a rule caught, and how soon.
type DraftCatch struct {
	Incident string
	Delay    time.Duration
}

// The bounds of a draft: as many rules and catches as an expectations file
// may hold, and of each rule its latest catches, so that the file it is
// written as is read back (ReadExpectations).
const (
	DraftCatchesARule = 10
)

// RoundUp is a count as a draft writes it as a budget, as it is now,
// rounded up: 1 to 9 as they are; 10 to 99 to the next multiple of 5;
// from 100, to two significant figures (123 is 130, 1,234 is 1,300); and
// never below 1, since a budget of 0 is broken by the first firing,
// however right that firing was. The same count is the same budget each
// time.
func RoundUp(n int) int {
	switch {
	case n < 1:
		return 1
	case n < 10:
		return n
	case n < 100:
		return (n + 4) / 5 * 5
	}
	step := 1
	for v := n; v >= 100; v /= 10 {
		step *= 10
	}
	return (n + step - 1) / step * step
}

// RoundUpWithin is a catch's delay as a draft holds it: rounded up to the
// whole minute, a minute at least (an alert before the incident began is
// held to come within a minute of it).
func RoundUpWithin(d time.Duration) time.Duration {
	if d <= time.Minute {
		return time.Minute
	}
	return (d + time.Minute - 1) / time.Minute * time.Minute
}

// Drafted is a draft: the expectations of each rule counted, in the
// order given, and the rules left out of it, each with why (DraftLeft).
type Drafted struct {
	Expectations []Expectation
	Left         []DraftLeftOut
	// counted are, of each rule drafted, what it was counted from, for
	// the line written beside it.
	counted map[string]DraftRule
}

// DraftLeftOut is a rule a draft left out, and why.
type DraftLeftOut struct {
	Rule string `json:"rule"`
	Why  string `json:"why"`
}

// Why a rule is left out of a draft.
const (
	DraftNoWeek    = "no_week"    // nothing counted of it yet
	DraftPastBound = "past_bound" // past the expectations a file holds
)

// Draft makes the expectations of rules as they are now: of each rule
// counted, a budget of the most it fired in a week, while people slept
// (when its hours say when they sleep) and re-fired, each rounded up
// (RoundUp); and its latest catches, DraftCatchesARule at most, each held
// within its delay rounded up (RoundUpWithin). A rule nothing was counted
// of is left out, and so is what would take the file past what an
// expectations file holds (MaxExpectationEntries, MaxExpectationCatches).
// A rule that never fired is held to the least budget, 1.
func Draft(rs []DraftRule) *Drafted {
	d := &Drafted{Expectations: []Expectation{}, counted: map[string]DraftRule{}}
	catches := 0
	seen := map[string]bool{}
	for _, r := range rs {
		if r.Rule == "" || seen[r.Rule] {
			continue
		}
		seen[r.Rule] = true
		switch {
		case r.Weeks == 0 && r.Scaled <= 0:
			d.Left = append(d.Left, DraftLeftOut{r.Rule, DraftNoWeek})
			continue
		case len(d.Expectations) == MaxExpectationEntries:
			d.Left = append(d.Left, DraftLeftOut{r.Rule, DraftPastBound})
			continue
		}
		week, refires := RoundUp(r.Most), RoundUp(r.Refires)
		x := Expectation{Rule: r.Rule, Budget: &Budget{Week: &week, Refires: &refires}}
		if r.Sleeps {
			asleep := RoundUp(r.Asleep)
			x.Budget.Asleep = &asleep
		}
		have := map[string]bool{}
		for _, c := range r.Caught {
			if len(x.Catches) == DraftCatchesARule || catches == MaxExpectationCatches {
				break
			}
			if c.Incident == "" || have[c.Incident] {
				continue
			}
			have[c.Incident] = true
			x.Catches = append(x.Catches, ExpectCatch{Incident: c.Incident, Within: Within(RoundUpWithin(c.Delay))})
			catches++
		}
		d.Expectations = append(d.Expectations, x)
		d.counted[r.Rule] = r
	}
	return d
}

// DraftAbout is what a draft was counted from, for its first lines: by
// whom (lapilli serve, lapilli verdict), over what, in the time zone its
// weeks are counted in.
type DraftAbout struct {
	By       string
	From, To time.Time
	// Weeks says how its weeks are counted: the ledger's (Monday, in the
	// zone) or whole weeks counted back from the period's end.
	Weeks string
	Loc   *time.Location
}

// Weeks a draft says it counted by.
const (
	DraftLedgerWeeks = "ledger"   // lapilli serve's: Monday, in --tz
	DraftPeriodWeeks = "backward" // lapilli verdict's: counted back from the period's end
)

// YAML is the draft as an expectations file, written by the YAML encoder
// from the expectations themselves, never pieced together from their
// text: a rule's name, whatever it holds, is a value the encoder quotes as
// it must, and reads back as it was (ReadExpectations). Its comments are
// Lapilli's own words and numbers: what it was counted from and how, and
// beside each rule what it did; never a name.
func (d *Drafted) YAML(about DraftAbout) ([]byte, error) {
	loc := about.Loc
	if loc == nil {
		loc = time.UTC
	}
	day := func(t time.Time) string { return t.In(loc).Format("2006-01-02") }
	var head []string
	switch about.Weeks {
	case DraftLedgerWeeks:
		head = append(head, fmt.Sprintf("Drafted by %s from its ledger's whole weeks from %s to %s (each from Monday, in %s),", about.By, day(about.From), day(about.To), loc))
	default:
		head = append(head, fmt.Sprintf("Drafted by %s from the period %s to %s, by whole weeks counted back from its end,", about.By, day(about.From), day(about.To)))
	}
	head = append(head,
		"as each rule is now: the most it fired in one week, while people slept and re-fired,",
		"rounded up (1 to 9 as they are, 10 to 99 to the next 5, from 100 to two figures, never",
		"below 1), and the incidents it caught, each within its delay rounded up to a minute.",
		"Counted, not written by a model: lower what is more than you want, and keep the rest.")
	for _, l := range d.Left {
		if l.Why == DraftPastBound {
			head = append(head, fmt.Sprintf("Past the %d expectations a file holds, %d more rules are left out.", MaxExpectationEntries, d.countLeft(DraftPastBound)))
			break
		}
	}
	if n := d.countLeft(DraftNoWeek); n > 0 {
		head = append(head, fmt.Sprintf("%d rules with nothing counted yet are left out.", n))
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, x := range d.Expectations {
		n, err := expectationNode(x)
		if err != nil {
			return nil, err
		}
		oneLine(n)
		if r, ok := d.counted[x.Rule]; ok {
			n.Content[1].LineComment = "# " + r.said()
		}
		seq.Content = append(seq.Content, n)
	}
	if len(seq.Content) == 0 {
		seq.Style = yaml.FlowStyle
	}
	root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: "expectations"}, seq}}
	root.HeadComment = "# " + strings.Join(head, "\n# ")
	return encodeYAML(&yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}})
}

func (d *Drafted) countLeft(why string) int {
	n := 0
	for _, l := range d.Left {
		if l.Why == why {
			n++
		}
	}
	return n
}

// said is what a rule did, beside it in a draft: numbers, never its name.
func (r DraftRule) said() string {
	over := fmt.Sprintf("over %d whole weeks", r.Weeks)
	switch {
	case r.Weeks == 1:
		over = "over 1 whole week"
	case r.Weeks == 0:
		over = fmt.Sprintf("over %s days, scaled to a week", strconv.FormatFloat(r.Scaled, 'f', 1, 64))
	}
	asleep := "while people slept not counted"
	if r.Sleeps {
		asleep = fmt.Sprintf("%d while people slept", r.Asleep)
	}
	return fmt.Sprintf("the most a week %s: %d fired, %s, %d re-fired; %d incidents caught", over, r.Most, asleep, r.Refires, len(r.Caught))
}

// oneLine quotes, in an expectation's node, a name of more than one line,
// or with a character that doesn't show: one line, its breaks and such
// characters escaped as the encoder escapes them, rather than a block
// whose lines a person could read as the file's own.
func oneLine(n *yaml.Node) {
	for i := 0; i+1 < len(n.Content); i += 2 {
		v := n.Content[i+1]
		if v.Kind == yaml.ScalarNode && strings.ContainsFunc(v.Value, func(r rune) bool { return r == '\n' || r == '\r' || !unicode.IsPrint(r) && r != ' ' }) {
			v.Style = yaml.DoubleQuotedStyle
		}
	}
}

// expectationNode is one expectation as the encoder writes it.
func expectationNode(x Expectation) (*yaml.Node, error) {
	var n yaml.Node
	if err := n.Encode(x); err != nil {
		return nil, err
	}
	return &n, nil
}

// encodeYAML writes a node as an expectations file is written: two spaces
// to a level.
func encodeYAML(n *yaml.Node) ([]byte, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// ExpectationsYAML is expectations as a file of them, written by the
// YAML encoder from the expectations themselves: what a draft kept, or a
// file with a draft taken in, as a person puts it where the expectations
// are declared.
func ExpectationsYAML(xs []Expectation) ([]byte, error) {
	if xs == nil {
		xs = []Expectation{}
	}
	return encodeYAML(&yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{mustNode(Expectations{Expectations: xs})}})
}

func mustNode(v any) *yaml.Node {
	var n yaml.Node
	if err := n.Encode(v); err != nil {
		panic(err) // expectations always encode
	}
	return &n
}

// DraftFromReport is, of a report's period, what each rule replayed did,
// as a draft is made of it: counted by whole weeks back from the period's
// end, as a comparison holds budgets (CheckExpectations), with no whole
// week the period's days, scaled; while people slept by the hours hoursOf
// gives each rule (nil, or hours with no sleep: not counted); and the
// incidents each caught, the latest first. A rule that couldn't be
// replayed is left out of it.
func DraftFromReport(rep *Report, hoursOf func(rule string) *Hours) []DraftRule {
	if rep == nil {
		return nil
	}
	k := &Checked{}
	span := rep.To.Sub(rep.From)
	k.Weeks = int(span / (7 * 24 * time.Hour))
	if k.Weeks == 0 {
		k.Scaled = span.Hours() / 24
	}
	counts := k.count(rep, rep.From, rep.To, hoursOf)
	caught := map[string][]DraftCatch{}
	incs := slices.Clone(rep.Incidents)
	slices.SortStableFunc(incs, func(a, b IncidentResult) int { return b.Start.Compare(a.Start) })
	for _, in := range incs {
		seen := map[string]bool{}
		for _, c := range in.CaughtBy {
			if seen[c.Rule] {
				continue
			}
			seen[c.Rule] = true
			caught[c.Rule] = append(caught[c.Rule], DraftCatch{Incident: in.ID, Delay: c.Delay})
		}
	}
	var out []DraftRule
	for _, r := range rep.Rules {
		if r.Unsupported != "" {
			continue
		}
		most, asleep, refired := k.most(counts, r.Rule)
		h := hoursOf(r.Rule)
		out = append(out, DraftRule{Rule: r.Rule, Weeks: k.Weeks, Scaled: k.Scaled, Most: most, Asleep: asleep, Refires: refired,
			Sleeps: h != nil && h.Sleeps(), Caught: caught[r.Rule]})
	}
	return out
}
