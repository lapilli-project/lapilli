package verdict

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/rules"
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
	// Recording are the recording rules that differ. A replay reads what
	// a recording rule recorded, from the history: the alerts that read
	// one are replayed against what it recorded, not what the change
	// would record.
	Recording []RecordingChange `json:"recording,omitempty"`
	// PassedOver says what reading each set's files passed over: a
	// rendered chart's Deployments, documents a selector didn't keep. By
	// "rules" and "compare", the flags that named them.
	PassedOver map[string]string `json:"passed_over,omitempty"`
	// Summary is the comparison in numbers, for a tool to read. lapilli
	// verdict --compare gives it; lapilli serve keeps none.
	Summary *Summary `json:"summary,omitempty"`
	// Unreplayed are the rules that couldn't be replayed, as
	// UnreplayedRules gives them, set by the run that replayed them: in
	// lapilli verdict's --json, and named at the top of its text. Those
	// with a side are the rules that differ Summary.Unreplayed counts, each
	// once.
	Unreplayed []Unreplayed `json:"unreplayed,omitempty"`
	// Expectations are the rules the change touches held to what they are
	// expected to do (--expect): set by Expect.
	Expectations *Checked `json:"expectations,omitempty"`
	// Scenarios runs the scenarios the expectations hold the rules to
	// (Expect): NoPromtool when nil, each not known. lapilli serve's
	// backtests set NotThroughServe: serve runs none it is sent.
	Scenarios ScenarioRunner `json:"-"`
	// Alike are the rules alike on both sides, which Expect holds to the
	// scenarios whose test files the change edited, when Scenarios reads
	// each side's (TestEditor), and to nothing else.
	Alike []AlikeRule `json:"-"`
	// Missed are the incidents a rule caught before the change that no
	// rule catches after it, as HoldMissed found them (--fail-missed): nil
	// when it didn't look.
	Missed []IncidentChange `json:"missed,omitempty"`
	// JudgedBy names whose record judged the firings, when it isn't the
	// incidents the comparison was given: lapilli serve's, for a pull
	// request backtested through it (JudgedByServe). CompareMarkdown's
	// heading says so.
	JudgedBy string `json:"judged_by,omitempty"`
}

// JudgedByServe is Comparison.JudgedBy of a backtest lapilli serve ran: its
// incidents, impact windows and weeks judged the firings.
const JudgedByServe = "lapilli serve"

// Expect holds the rules the change touches to after's expectations, and
// the side before to before's when it is given; it counts on the summary
// what the change broke. Held to more than MaxExpectResults, it holds
// nothing, and says why.
func (c *Comparison) Expect(after, before *Expectations, hours *Hours) error {
	return c.ExpectBy(after, before, func(string) *Hours { return hours })
}

// ExpectBy is Expect, each rule's firings counted while people slept by
// the hours hoursOf gives it (CheckExpectationsBy).
func (c *Comparison) ExpectBy(after, before *Expectations, hoursOf func(rule string) *Hours) error {
	k, err := CheckExpectationsBy(c, after, before, hoursOf)
	if err != nil {
		return err
	}
	c.Expectations = k
	if c.Summary != nil {
		c.Summary.Expected = true
		c.Summary.ExpectationsBroken = c.Expectations.Broken()
		c.Summary.ExpectationsAlreadyBroken, c.Summary.ScenariosUnrun = 0, 0
		for _, r := range c.Expectations.Results {
			if r.AlreadyBroken() {
				c.Summary.ExpectationsAlreadyBroken++
			}
		}
		c.Summary.Unexpected = len(c.Expectations.Unexpected)
		for _, r := range c.Expectations.Results {
			if r.Unrun {
				c.Summary.ScenariosUnrun++
			}
		}
	}
	return nil
}

// RecordingChange is a recording rule that differs between the two sets:
// by what it records and its group, "changed", "added" or "removed".
type RecordingChange struct {
	Record string `json:"record"`
	Group  string `json:"group"`
	Change string `json:"change"`
}

// Summary is a comparison in numbers, for a tool to read: lapilli verdict
// --compare --json gives it as "summary", and --markdown on its first
// line. Every field is always there, and its meaning doesn't change
// (docs/pr-backtests.md).
type Summary struct {
	// NothingChanged says nothing that decides when a rule fires differs
	// between the two sets of rules, a recording rule included: nothing
	// was replayed.
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
	// one side or both, whatever why: those of Comparison.Unreplayed with a
	// side, each once.
	Unreplayed int `json:"unreplayed"`
	// RecordingChanged are the recording rules that differ (Recording):
	// what the alerts that read them see changes, which no replay shows.
	RecordingChanged int `json:"recording_changed"`
	// FiringsBefore and FiringsAfter are the firings on each side: of the
	// rules that differ alone, with --changed-only.
	FiringsBefore int `json:"firings_before"`
	FiringsAfter  int `json:"firings_after"`
	// IncidentsGiven says incidents were given to judge against,
	// --incidents or --impact. Without them nothing was judged: no firing
	// caught anything, each counts as a black stone, and Incidents is 0.
	// The firings were judged only when it is true and NoRecord false.
	IncidentsGiven bool `json:"incidents_given"`
	// Incidents are the incidents in the period, and IncidentsChanged
	// those the two sets catch differently.
	Incidents        int `json:"incidents"`
	IncidentsChanged int `json:"incidents_changed"`
	// NoRecord says incidents were given, and none keeps the period's
	// record (Report.NoRecord): nothing was judged, as without them.
	NoRecord bool `json:"no_record"`
	// Expected says the rules were held to expectations (--expect).
	// ExpectationsBroken are those the change broke: met before, or of a
	// rule it adds, and broken after; ExpectationsAlreadyBroken, those
	// broken on both sides, which aren't the change's. Unexpected are the
	// rules it adds that no expectation holds.
	Expected                  bool `json:"expected"`
	ExpectationsBroken        int  `json:"expectations_broken"`
	ExpectationsAlreadyBroken int  `json:"expectations_already_broken"`
	Unexpected                int  `json:"unexpected"`
	// ScenariosUnrun are the scenarios of the rules that differ that
	// couldn't be run after the change, promtool given (--promtool) and the
	// rule one it runs: the test file unreadable, a time out, a crash. They
	// can't be judged, as a rule that couldn't be replayed can't.
	ScenariosUnrun int `json:"scenarios_unrun"`
	// MissedChecked says the run looked for the incidents a rule caught
	// before the change that no rule catches after it (--fail-missed);
	// IncidentsMissed are those it found, 0 when it didn't look.
	// MissedNotKnown are those it couldn't tell caught or missed: caught
	// by no rule replayed after the change, while a rule after it
	// couldn't be replayed, which may catch them. They fail nothing.
	MissedChecked   bool `json:"missed_checked"`
	IncidentsMissed int  `json:"incidents_missed"`
	MissedNotKnown  int  `json:"missed_not_known"`
	// MissedLimited are those of MissedNotKnown left so because the
	// replay of the rules left alone reached its limit, which lapilli serve
	// puts on a backtest's replay of them (HoldMissedBy): fewer days or
	// rules settle them. 0 when none were.
	MissedLimited int `json:"missed_limited"`
}

// Summarize sets c.Summary from the rest of c; incidents says whether
// incidents were given to judge against.
func (c *Comparison) Summarize(incidents bool) {
	p := c.parts()
	s := &Summary{Changed: p.changed, Added: p.added, Removed: p.gone, Same: c.Same, Alike: len(p.alike),
		Unreplayed: len(p.broken), RecordingChanged: len(c.Recording), IncidentsGiven: incidents, IncidentsChanged: len(c.Incidents)}
	s.NothingChanged = s.Changed+s.Added+s.Removed+s.RecordingChanged == 0
	s.NoRecord = incidents && c.Before != nil && c.Before.NoRecord
	if c.Before != nil {
		s.FiringsBefore, s.Incidents = len(c.Before.Firings), len(c.Before.Incidents)
	}
	if c.After != nil {
		s.FiringsAfter = len(c.After.Firings)
	}
	c.Summary = s
}

// Lost reports whether an incident a rule replayed caught before the
// change is caught by none replayed after it. With ChangedOnly, the rules
// left alone weren't replayed, and one of them may still catch it: see
// HoldMissed.
func (c *Comparison) Lost() bool {
	return slices.ContainsFunc(c.Incidents, func(ic IncidentChange) bool { return len(ic.Before) > 0 && len(ic.After) == 0 })
}

// HoldMissed sets Missed, and the summary's count of them: the incidents a
// rule caught before the change that no rule catches after it. alike is
// the verdict of the rules left alone over the same period and incidents,
// when they weren't replayed with the others (ChangedOnly), or nil: an
// incident one of them catches is caught on both sides, and isn't missed.
// When a rule after the change couldn't be replayed (the history unread,
// a query refused, out of time), whether it would catch one isn't known:
// such an incident is counted as not known (MissedNotKnown), not missed.
func (c *Comparison) HoldMissed(alike *Report) {
	broken := anyUnreplayed(alike)
	c.holdMissed(func(i int, in incident.Incident) (bool, bool) {
		if alike == nil {
			return false, true
		}
		same := func(x IncidentResult) bool { return x.ID == in.ID && x.Start.Equal(in.Start) }
		if i < len(alike.Incidents) && same(alike.Incidents[i]) {
			return len(alike.Incidents[i].CaughtBy) > 0, !broken
		}
		for _, x := range alike.Incidents {
			if same(x) && len(x.CaughtBy) > 0 {
				return true, true
			}
		}
		return false, !broken
	})
}

// LeftAlone says of an incident whether a rule the change leaves alone
// catches it (caught), and whether that is known at all (known): a rule
// left alone that couldn't be replayed, or whose record doesn't hold the
// incident's time, may catch it.
type LeftAlone func(in incident.Incident) (caught, known bool)

// HoldMissedBy is HoldMissed, with what the rules left alone catch said
// by leftAlone, asked only of an incident a rule that differs caught
// before the change and none catches after it: lapilli serve's backtests
// say it by serve's record of those rules, not by replaying them. An
// incident leftAlone doesn't know is counted as not known, as one a rule
// after the change that couldn't be replayed may catch. nil is no rule
// left alone.
func (c *Comparison) HoldMissedBy(leftAlone LeftAlone) {
	c.holdMissed(func(_ int, in incident.Incident) (bool, bool) {
		if leftAlone == nil {
			return false, true
		}
		return leftAlone(in)
	})
}

// AfterUnreplayed reports whether a rule replayed after the change couldn't
// be replayed: an incident none of them catches is then not known, whatever
// the rules left alone say.
func (c *Comparison) AfterUnreplayed() bool { return anyUnreplayed(c.After) }

// LostIncidents are the incidents a rule that differs caught before the
// change and none of them catches after it, in order: those HoldMissedBy
// asks leftAlone of.
func (c *Comparison) LostIncidents() []incident.Incident {
	var out []incident.Incident
	if c.Before != nil && c.After != nil && len(c.Before.Incidents) == len(c.After.Incidents) {
		for i, b := range c.Before.Incidents {
			if len(b.CaughtBy) > 0 && len(c.After.Incidents[i].CaughtBy) == 0 {
				out = append(out, b.Incident)
			}
		}
	}
	return out
}

// anyUnreplayed reports whether a rule r judged couldn't be replayed.
func anyUnreplayed(r *Report) bool {
	return r != nil && slices.ContainsFunc(r.Rules, func(rr RuleResult) bool { return rr.Unsupported != "" })
}

// holdMissed sets Missed and the summary's counts, leftAlone saying of the
// i-th incident whether a rule left alone catches it, and whether that is
// known.
func (c *Comparison) holdMissed(leftAlone func(i int, in incident.Incident) (caught, known bool)) {
	c.Missed = []IncidentChange{}
	notKnown := 0
	// After the change: the rules replayed after it, and the rules left
	// alone, as leftAlone says them.
	unsure := anyUnreplayed(c.After)
	// Both sides were judged against the same incidents, which come in the
	// same order.
	if c.Before != nil && c.After != nil && len(c.Before.Incidents) == len(c.After.Incidents) {
		for i, b := range c.Before.Incidents {
			if len(b.CaughtBy) == 0 || len(c.After.Incidents[i].CaughtBy) > 0 {
				continue
			}
			caught, known := leftAlone(i, b.Incident)
			if caught {
				continue
			}
			if unsure || !known {
				notKnown++
				continue
			}
			c.Missed = append(c.Missed, IncidentChange{Incident: b.Incident, Before: b.CaughtBy, After: []Catch{}})
		}
	}
	if c.Summary != nil {
		c.Summary.MissedChecked, c.Summary.IncidentsMissed, c.Summary.MissedNotKnown = true, len(c.Missed), notKnown
	}
}

// MissedNotKnownText says how many incidents HoldMissed couldn't tell
// caught or missed, and why, or nothing. Of a backtest lapilli serve ran,
// a rule left alone may be why too: its record doesn't hold one of them
// over an incident's time.
func (c *Comparison) MissedNotKnownText() string {
	if c.Summary == nil || c.Summary.MissedNotKnown == 0 {
		return ""
	}
	if c.JudgedBy == JudgedByServe {
		text := fmt.Sprintf("%d incidents a rule caught before the change are caught by no rule after it that lapilli serve can tell, but a rule after it couldn't be replayed, "+
			"or serve's record doesn't settle a rule left alone over them and it couldn't be replayed: whether they are missed isn't known.", c.Summary.MissedNotKnown)
		if n := c.Summary.MissedLimited; n > 0 {
			text += fmt.Sprintf(" %d of them were left so because the replay of the rules left alone reached the limit lapilli serve puts on it: fewer rules or days would settle them.", n)
		}
		return text
	}
	return fmt.Sprintf("%d incidents a rule caught before the change are caught by no rule replayed after it, but a rule after it couldn't be replayed: "+
		"whether they are missed isn't known, and they fail nothing (--fail-unreplayed fails a run whose history couldn't be read).", c.Summary.MissedNotKnown)
}

// MissedText writes, for a person, what HoldMissed found (--fail-missed),
// times in loc: nothing when it didn't look, or nothing changed.
func MissedText(w io.Writer, c *Comparison, loc *time.Location) error {
	if c.Summary == nil || !c.Summary.MissedChecked || c.Summary.NothingChanged {
		return nil
	}
	if s := c.MissedNotKnownText(); s != "" {
		fmt.Fprintf(w, "\n%s\n", s)
	}
	if len(c.Missed) == 0 {
		if c.Summary.MissedNotKnown > 0 {
			return nil
		}
		_, err := fmt.Fprintln(w, "\nEvery incident a rule caught before the change is caught after it too (--fail-missed).")
		return err
	}
	fmt.Fprintf(w, "\nThe run fails (--fail-missed): %d incidents a rule caught before the change are caught by no rule after it%s.\n", len(c.Missed), c.leftAloneToo())
	rows := [][]string{{"INCIDENT", "START", "CAUGHT BEFORE BY"}}
	for _, ic := range c.Missed {
		rows = append(rows, []string{incidentName(ic.Incident), ic.Start.In(loc).Format("2006-01-02 15:04"), caughtBy(IncidentResult{CaughtBy: ic.Before}, 0, c.matchOf())})
	}
	return table(w, rows)
}

// leftAloneToo says, of the incidents Missed holds, that no rule left
// alone catches them either, when those weren't replayed with the others
// (ChangedOnly) but were for them (HoldMissed), or, of a backtest lapilli
// serve ran, by serve's record of them, its own rules beside the pull
// request's (HoldMissedBy).
func (c *Comparison) leftAloneToo() string {
	if c.JudgedBy == JudgedByServe {
		return ", the rules left alone among them, as lapilli serve's record has them"
	}
	if c.ChangedOnly && c.Same > 0 {
		return ", the rules left alone among them"
	}
	return ""
}

// judged says the firings were judged against incidents: always, unless
// the summary says none were given, or no incident keeps the period's
// record (Report.NoRecord).
func (c *Comparison) judged() bool {
	return (c.Summary == nil || c.Summary.IncidentsGiven) && !c.noRecord()
}

// noRecord says incidents were given, but none keeps the period's record:
// the firings are counted, not judged, as without them.
func (c *Comparison) noRecord() bool {
	return (c.Summary == nil || c.Summary.IncidentsGiven) && c.Before != nil && c.Before.NoRecord
}

// noRecordText is NoRecordCompared, and why when --impact had no data.
func (c *Comparison) noRecordText() string {
	if c.Before != nil && c.Before.Settings != nil && c.Before.Settings.ImpactNoData {
		return NoRecordCompared + " --impact keeps no record: a selector it reads has no data; give one whose metrics have data over the period."
	}
	return NoRecordCompared
}

// NoRecordCompared says a comparison of a period with no record counts its
// firings and doesn't judge them.
const NoRecordCompared = "No incident was written down, or found by --impact, in the period or the two weeks before it: the firings are counted, not judged."

// RecordingNote says what a comparison can't of a recording rule that
// differs.
const RecordingNote = "The alerts that read them are replayed against what was recorded, not what the change would record."

// limited is how many of the rules that differ are in a group with a
// limit, on either side.
func (p compareParts) limited() int {
	n := 0
	has := func(r *RuleResult) bool { return r != nil && slices.Contains(r.Unseen, rules.UnseenLimit) }
	for _, rc := range p.all {
		if has(rc.Before) || has(rc.After) {
			n++
		}
	}
	return n
}

// compareParts is a comparison's rules that differ, sorted for showing.
type compareParts struct {
	changed, added, gone int
	// alike fire as they did; broken couldn't be replayed on one side or
	// both; shown are the rest, and those replayed on one side of broken.
	// all are every one.
	alike, broken, shown, all []RuleChange
}

func (c *Comparison) parts() compareParts {
	var p compareParts
	unreplayed := func(r *RuleResult) bool { return r != nil && r.Unsupported != "" }
	p.all = c.Rules
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
	// BeforeLabels and AfterLabels are the labels its alerts carry on each
	// side, its group's among them (rules.Rule.AlertLabels), as LabelRules
	// sets them: what an expectation by labels holds a rule by. Not shown.
	BeforeLabels map[string]string `json:"-"`
	AfterLabels  map[string]string `json:"-"`
}

func cmpOr(m, or map[string]string) map[string]string {
	if m == nil {
		return or
	}
	return m
}

// LabelRules gives the rules that differ the labels their alerts carry on
// each side, from the rules each side replayed: what an expectation by
// labels (labels: severity=page) holds a rule by.
func (c *Comparison) LabelRules(before, after []rules.Rule) {
	of := func(rs []rules.Rule) map[string]map[string]string {
		out := make(map[string]map[string]string, len(rs))
		for _, r := range rs {
			if _, ok := out[r.Name]; !ok {
				// None is known to be none: a selector such as !team holds it.
				out[r.Name] = cmpOr(r.AlertLabels(), map[string]string{})
			}
		}
		return out
	}
	b, a := of(before), of(after)
	for i := range c.Rules {
		rc := &c.Rules[i]
		if rc.Before != nil {
			rc.BeforeLabels = b[rc.Rule]
		}
		if rc.After != nil {
			rc.AfterLabels = a[rc.Rule]
		}
	}
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
		if !slices.EqualFunc(b.CaughtBy, a.CaughtBy, func(x, y Catch) bool {
			return x.Rule == y.Rule && x.Delay == y.Delay && x.Noisy == y.Noisy && x.Linked == y.Linked && x.Loose == y.Loose && slices.Equal(x.Lacked, y.Lacked)
		}) {
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
	if len(c.Recording) > 0 {
		var names []string
		for _, r := range c.Recording[:min(len(c.Recording), 10)] {
			names = append(names, Clean(r.Record)+" ("+r.Change+")")
		}
		if len(c.Recording) > 10 {
			names = append(names, fmt.Sprintf("and %d more", len(c.Recording)-10))
		}
		fmt.Fprintf(w, "Recording rules that differ: %s.\n%s\n", strings.Join(names, ", "), RecordingNote)
	}
	if n := p.limited(); n > 0 {
		fmt.Fprintf(w, "%d of the rules that differ are in a group with a limit, which the replay doesn't hold them to: past it, Prometheus drops all of a rule's alerts.\n", n)
	}
	if c.noRecord() {
		fmt.Fprintln(w, c.noRecordText())
	}
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
		fmt.Fprintf(w, "Of %s: with no alert %d → %d", plural(n, "incident", "incidents"), count(c.Before, IncidentResult.Missed), count(c.After, IncidentResult.Missed))
		if b, a := count(c.Before, IncidentResult.OnlyNoisy), count(c.After, IncidentResult.OnlyNoisy); b+a > 0 {
			fmt.Fprintf(w, ", caught only by noisy rules %d → %d", b, a)
		}
		fmt.Fprintln(w)
	}
	partial(w, c.Unreplayed, 0, false)

	if len(shown) > 0 {
		fmt.Fprintln(w)
		sev := slices.ContainsFunc(shown, func(rc RuleChange) bool {
			return rc.Before != nil && rc.Before.Severity != "" || rc.After != nil && rc.After.Severity != ""
		})
		// With responses, ACTIONABLE beside precision, before REFIRED: it
		// needs no incident to judge by, so it shows when nothing was judged.
		resp := c.Before.hasResponses() || c.After.hasResponses()
		withActionable := func(row []string, cell string) []string {
			if !resp {
				return row
			}
			return slices.Insert(row, len(row)-1, cell)
		}
		rows := [][]string{withActionable(judgedColumns(judged, withSeverity(sev, "RULE", "SEVERITY", "FIRED", "○ CAUGHT", "● NOTHING", "PRECISION", "REFIRED")), "ACTIONABLE")}
		for _, rc := range shown {
			rows = append(rows, withActionable(judgedColumns(judged, withSeverity(sev, Clean(rc.Rule), severities(rc), stones(rc)...)), actionables(rc)))
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
		fmt.Fprintf(w, "\nCould not replay %s:\n", plural(len(broken), "rule", "rules"))
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
		return caughtBy(IncidentResult{CaughtBy: cs}, 0, c.matchOf())
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

// actionables is a changed rule's actionable share, before → after.
func actionables(rc RuleChange) string {
	return both(rc, func(r RuleResult) string {
		if p, ok := r.Actionable(); ok {
			return fmt.Sprintf("%.0f%%", 100*p)
		}
		return "–"
	})
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

// matchOf is the labels the comparison's reports were matched on.
func (c *Comparison) matchOf() []string {
	if c.After != nil && c.After.Settings != nil {
		return c.After.Settings.Match
	}
	if c.Before != nil {
		return matchOf(c.Before.Settings)
	}
	return nil
}
