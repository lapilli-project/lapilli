package verdict

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
)

func readExpect(t *testing.T, s string) *Expectations {
	t.Helper()
	e, err := ReadExpectations(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// An expectations file is read strictly: what it can't hold a rule to is
// an error, never passed over.
func TestReadExpectationsStrict(t *testing.T) {
	for want, s := range map[string]string{
		"field budjet not found":                "expectations:\n  - rule: A\n    budjet: {week: 3}\n",
		"give rule or group":                    "expectations:\n  - budget: {week: 3}\n",
		"give rule or group, one of them":       "expectations:\n  - rule: A\n    group: g\n    budget: {week: 3}\n",
		"nothing to hold A to":                  "expectations:\n  - rule: A\n",
		"a budget of nothing":                   "expectations:\n  - rule: A\n    budget: {}\n",
		"a budget below nothing":                "expectations:\n  - rule: A\n    budget: {week: -1}\n",
		"a catch names no incident":             "expectations:\n  - rule: A\n    catches: [{incident: ' '}]\n",
		"want a duration such as 5m":            "expectations:\n  - rule: A\n    catches: [{incident: INC-1, within: soon}]\n",
		"cannot unmarshal !!str `ten` into int": "expectations:\n  - rule: A\n    budget: {week: ten}\n",
	} {
		if _, err := ReadExpectations(strings.NewReader(s)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", s, err, want)
		}
	}
	if _, err := ReadExpectations(bytes.NewReader(bytes.Repeat([]byte("#"), MaxExpectations+1))); err == nil {
		t.Error("a file over the limit read")
	}
	if e, err := ReadExpectations(strings.NewReader("")); err != nil || len(e.Expectations) != 0 {
		t.Errorf("an empty file: %v %+v", err, e)
	}
}

// firingsAt are firings of a rule, one at each time.
func firingsAt(rule string, ts ...time.Time) []Judged {
	var out []Judged
	for _, t := range ts {
		out = append(out, Judged{Firing: firing.Firing{Rule: rule, Start: t, End: t.Add(time.Minute)}})
	}
	return out
}

// A budget is held to each whole week of the period, counted back from its
// end, the most in one shown; while people slept, by the hours given. What
// the change broke is told from what was broken before it.
func TestCheckExpectationsBudget(t *testing.T) {
	to := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	from := to.Add(-14 * 24 * time.Hour)
	at := func(daysAgo int, hour int) time.Time {
		return to.Add(-time.Duration(daysAgo) * 24 * time.Hour).Truncate(24 * time.Hour).Add(time.Duration(hour) * time.Hour)
	}
	// Before: two a week, by day. After: five the week before last, three
	// of them at 03:00, and one last week.
	before := &Report{Rules: []RuleResult{{Rule: "Restarts", Firings: 4}, {Rule: "Steady", Firings: 2}},
		Firings: append(firingsAt("Restarts", at(2, 12), at(3, 12), at(9, 12), at(10, 12)), firingsAt("Steady", at(2, 12), at(9, 12))...)}
	after := &Report{Rules: []RuleResult{{Rule: "Restarts", Firings: 6}, {Rule: "Steady", Firings: 2}, {Rule: "New", Firings: 1}},
		Firings: append(append(firingsAt("Restarts", at(8, 3), at(9, 3), at(10, 3), at(11, 12), at(12, 12), at(2, 12)), firingsAt("Steady", at(2, 12), at(9, 12))...),
			firingsAt("New", at(1, 12))...)}
	c := &Comparison{From: from, To: to, Before: before, After: after,
		Rules: []RuleChange{{Rule: "Restarts", Before: &before.Rules[0], After: &after.Rules[0]}, {Rule: "Steady", Before: &before.Rules[1], After: &after.Rules[1]},
			{Rule: "New", After: &after.Rules[2]}}}
	c.Summarize(false)
	e := readExpect(t, "expectations:\n  - rule: Restarts\n    budget: {week: 3, asleep: 1}\n  - rule: Steady\n    budget: {week: 1}\n")
	c.Expect(e, nil, DefaultHours(time.UTC))
	k := c.Expectations
	if k.Weeks != 2 {
		t.Fatalf("weeks: %d", k.Weeks)
	}
	got := map[string]ExpectResult{}
	for _, r := range k.Results {
		got[r.Rule+" "+r.What] = r
	}
	week, asleep, steady := got["Restarts budget week"], got["Restarts budget asleep"], got["Steady budget week"]
	if week.Before != "2" || week.After != "5" || !week.BrokenByChange() {
		t.Errorf("a week's: %+v", week)
	}
	if asleep.Before != "0" || asleep.After != "3" || !asleep.BrokenByChange() {
		t.Errorf("while people slept: %+v", asleep)
	}
	// One a week, held to one: met on both sides.
	if steady.Before != "1" || steady.After != "1" || steady.AfterState != ExpectMet || steady.BrokenByChange() {
		t.Errorf("steady: %+v", steady)
	}
	if k.Broken() != 2 || c.Summary.ExpectationsBroken != 2 || !c.Summary.Expected || c.Summary.Unexpected != 1 || k.Unexpected[0] != "New" {
		t.Errorf("summary: %+v %+v", c.Summary, k.Unexpected)
	}

	// Broken before too: the change isn't held to it.
	tight := readExpect(t, "expectations:\n  - rule: Restarts\n    budget: {week: 1}\n")
	c.Expect(tight, nil, DefaultHours(time.UTC))
	if r := c.Expectations.Results[0]; !r.AlreadyBroken() || r.BrokenByChange() || c.Summary.ExpectationsBroken != 0 || c.Summary.ExpectationsAlreadyBroken != 1 {
		t.Errorf("broken before too: %+v %+v", r, c.Summary)
	}
	// Raised in the change: before is held to what it was, and it says so.
	c.Expect(readExpect(t, "expectations:\n  - rule: Restarts\n    budget: {week: 9}\n"), tight, DefaultHours(time.UTC))
	if r := c.Expectations.Results[0]; !r.Changed || r.WasLimit != "1" || r.BeforeState != ExpectBroken || r.AfterState != ExpectMet {
		t.Errorf("raised: %+v", r)
	}
	// No hours to count by: while people slept isn't known, never met.
	c.Expect(e, nil, nil)
	for _, r := range c.Expectations.Results {
		if r.What == "budget asleep" && (r.AfterState != ExpectUnknown || r.BrokenByChange()) {
			t.Errorf("no hours: %+v", r)
		}
	}

	var text strings.Builder
	c.Expect(e, nil, DefaultHours(time.UTC))
	if err := ExpectationsText(&text, c.Expectations); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Expectations: 2 broken by this change, 0 broken before too, 1 met", "each of the period's 2 whole weeks",
		"at most 3 a week", "✗ broken by this change", "Rules this change adds that no expectation holds: New"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text: no %q in\n%s", want, text.String())
		}
	}
	// The broken first.
	if i, j := strings.Index(text.String(), "broken by this change\n"), strings.Index(text.String(), "  met"); i < 0 || j >= 0 && j < i {
		t.Errorf("order:\n%s", text.String())
	}
}

// A period with no whole week holds a budget to a week's share of it.
func TestCheckExpectationsShortPeriod(t *testing.T) {
	to := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	from := to.Add(-84 * time.Hour) // three and a half days
	after := &Report{Rules: []RuleResult{{Rule: "A", Firings: 2}}, Firings: firingsAt("A", to.Add(-time.Hour), to.Add(-2*time.Hour))}
	before := &Report{Rules: []RuleResult{{Rule: "A"}}}
	c := &Comparison{From: from, To: to, Before: before, After: after, Rules: []RuleChange{{Rule: "A", Before: &before.Rules[0], After: &after.Rules[0]}}}
	k := CheckExpectations(c, readExpect(t, "expectations:\n  - rule: A\n    budget: {week: 3}\n"), nil, nil)
	if r := k.Results[0]; k.Weeks != 0 || k.Scaled != 3.5 || r.After != "4" || r.Before != "0" || !r.BrokenByChange() {
		t.Errorf("scaled: %+v %+v", k, r)
	}
	if !strings.Contains(k.over(), "3.5 days") {
		t.Errorf("over: %s", k.over())
	}
}

// A catch is the incident caught by the rule, within its delay when one is
// given; not known when the period has no such incident.
func TestCheckExpectationsCatches(t *testing.T) {
	to := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	inc := incident.Incident{ID: "INC-43", Start: to.Add(-48 * time.Hour)}
	before := &Report{Rules: []RuleResult{{Rule: "Errors"}}, Incidents: []IncidentResult{{Incident: inc, CaughtBy: []Catch{{Rule: "Errors", Delay: 2 * time.Minute}}}}}
	late := &Report{Rules: []RuleResult{{Rule: "Errors"}}, Incidents: []IncidentResult{{Incident: inc, CaughtBy: []Catch{{Rule: "Errors", Delay: 9 * time.Minute}}}}}
	none := &Report{Rules: []RuleResult{{Rule: "Errors"}}, Incidents: []IncidentResult{{Incident: inc}}}
	cmp := func(after *Report) *Comparison {
		return &Comparison{From: to.Add(-7 * 24 * time.Hour), To: to, Before: before, After: after,
			Rules: []RuleChange{{Rule: "Errors", Before: &before.Rules[0], After: &after.Rules[0]}}}
	}
	e := readExpect(t, "expectations:\n  - rule: Errors\n    catches:\n      - {incident: INC-43, within: 5m}\n      - {incident: INC-99}\n")
	for name, c := range map[string]struct {
		after       *Report
		state, seen string
	}{
		"later than 5m": {late, ExpectBroken, "+9m"},
		"not caught":    {none, ExpectBroken, "✗"},
		"as before":     {before, ExpectMet, "+2m"},
	} {
		k := CheckExpectations(cmp(c.after), e, nil, nil)
		r := k.Results[0]
		if r.AfterState != c.state || r.After != c.seen || r.Before != "+2m" || r.Limit != "within 5m" {
			t.Errorf("%s: %+v", name, r)
		}
		// INC-99 isn't in the period: not known on both sides.
		if r := k.Results[1]; r.BeforeState != ExpectUnknown || r.AfterState != ExpectUnknown || r.BrokenByChange() {
			t.Errorf("%s: an incident not in the period: %+v", name, r)
		}
	}
	// A rule the change takes away catches nothing: still expected to,
	// the change breaks it.
	c := cmp(before)
	c.Rules[0].After = nil
	if r := CheckExpectations(c, e, nil, nil).Results[0]; r.AfterState != ExpectBroken || r.After != "taken away" || !r.BrokenByChange() {
		t.Errorf("taken away: %+v", r)
	}
}

// The review's cases: each would have gone wrong before its fix.
func TestCheckExpectationsReview(t *testing.T) {
	to := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	from := to.Add(-7 * 24 * time.Hour)
	eight := func(rule string) []Judged {
		var ts []time.Time
		for i := range 8 {
			ts = append(ts, to.Add(-time.Duration(i+1)*12*time.Hour))
		}
		return firingsAt(rule, ts...)
	}
	side := func(rule, group string, fs []Judged) *Report {
		return &Report{Rules: []RuleResult{{Rule: rule, Group: group, Firings: len(fs)}}, Firings: fs}
	}
	change := func(b, a *Report, rule string) *Comparison {
		rc := RuleChange{Rule: rule}
		if b != nil {
			rc.Before = &b.Rules[0]
		}
		if a != nil {
			rc.After = &a.Rules[0]
		}
		return &Comparison{From: from, To: to, Before: b, After: a, Rules: []RuleChange{rc}}
	}
	find := func(k *Checked, what string, removed bool) ExpectResult {
		t.Helper()
		for _, r := range k.Results {
			if r.What == what && r.Removed == removed {
				return r
			}
		}
		t.Fatalf("no %s (removed %v) in %+v", what, removed, k.Results)
		return ExpectResult{}
	}

	// A rule held by name and its group by group, in one file: each to its
	// own, before as after; nothing the change didn't do is said changed.
	both := readExpect(t, "expectations:\n  - rule: R\n    budget: {week: 20}\n  - group: G\n    budget: {week: 5}\n")
	c := change(side("R", "G", eight("R")), side("R", "G", eight("R")), "R")
	k := CheckExpectations(c, both, nil, nil)
	if k.Broken() != 0 || len(k.Results) != 2 || k.Results[0].Changed || k.Results[1].Changed || !k.Results[1].AlreadyBroken() {
		t.Errorf("rule and group: %+v", k.Results)
	}
	// The same file given as it was too: nothing changed.
	if k := CheckExpectations(c, both, both, nil); k.Broken() != 0 || slices.ContainsFunc(k.Results, func(r ExpectResult) bool { return r.Changed || r.Removed }) {
		t.Errorf("as it was: %+v", k.Results)
	}

	// A name told apart in the run is held by its written name.
	apart := "CheckoutErrors [severity=page]"
	c = change(side(apart, "G", nil), side(apart, "G", eight(apart)), apart)
	if k := CheckExpectations(c, readExpect(t, "expectations:\n  - rule: CheckoutErrors\n    budget: {week: 5}\n"), nil, nil); k.Broken() != 1 {
		t.Errorf("told apart: %+v", k.Results)
	}
	if !(Expectation{Rule: "CheckoutErrors"}).For(apart, "") || (Expectation{Rule: "Checkout"}).For(apart, "") {
		t.Error("For: a name told apart")
	}

	// Taken away in the change, whole or a field of it: shown, held to
	// what it was, and failing nothing.
	was := readExpect(t, "expectations:\n  - rule: R\n    budget: {week: 5, asleep: 1}\n")
	c = change(side("R", "G", nil), side("R", "G", eight("R")), "R")
	k = CheckExpectations(c, readExpect(t, "expectations:\n  - rule: Other\n    budget: {week: 5}\n"), was, DefaultHours(time.UTC))
	if r := find(k, "budget week", true); r.BrokenByChange() || r.After != "8" || r.verdictOf() != "taken away; the change would break it" ||
		!strings.Contains(r.heldTo(Clean), "(taken away in this change)") || k.Broken() != 0 {
		t.Errorf("taken away whole: %+v", k.Results)
	}
	k = CheckExpectations(c, readExpect(t, "expectations:\n  - rule: R\n    budget: {week: 50}\n"), was, DefaultHours(time.UTC))
	if r := find(k, "budget asleep", true); r.BrokenByChange() || k.Broken() != 0 {
		t.Errorf("a field taken away: %+v", k.Results)
	}
	if r := find(k, "budget week", false); r.WasLimit != "5" || r.BeforeState != ExpectMet || r.AfterState != ExpectMet {
		t.Errorf("raised beside it: %+v", r)
	}

	// New in the change, whole or a field: the side before held to it too,
	// and said new; the change failed for what it breaks, either way alike.
	for name, now := range map[string]string{
		"a new entry": "expectations:\n  - rule: R\n    budget: {week: 5}\n",
		"a new field": "expectations:\n  - rule: R\n    budget: {week: 5}\n    catches: [{incident: INC-1}]\n",
	} {
		before := readExpect(t, "expectations: []\n")
		if name == "a new field" {
			before = readExpect(t, "expectations:\n  - rule: R\n    catches: [{incident: INC-1}]\n")
		}
		k := CheckExpectations(c, readExpect(t, now), before, nil)
		if r := find(k, "budget week", false); !r.Changed || r.WasLimit != "" || r.BeforeState != ExpectMet || !r.BrokenByChange() ||
			!strings.Contains(r.heldTo(Clean), "(new in this change)") {
			t.Errorf("%s: %+v", name, r)
		}
	}
	// A catch's delay changed: the side before held to the old one, and said.
	inc := incident.Incident{ID: "INC-43", Start: to.Add(-48 * time.Hour)}
	b := &Report{Rules: []RuleResult{{Rule: "R"}}, Incidents: []IncidentResult{{Incident: inc, CaughtBy: []Catch{{Rule: "R", Delay: 9 * time.Minute}}}}}
	a := &Report{Rules: []RuleResult{{Rule: "R"}}, Incidents: b.Incidents}
	k = CheckExpectations(change(b, a, "R"), readExpect(t, "expectations:\n  - rule: R\n    catches: [{incident: INC-43, within: 30m}]\n"),
		readExpect(t, "expectations:\n  - rule: R\n    catches: [{incident: INC-43, within: 5m}]\n"), nil)
	if r := k.Results[0]; r.WasLimit != "within 5m" || r.BeforeState != ExpectBroken || r.AfterState != ExpectMet || r.Limit != "within 30m" {
		t.Errorf("within changed: %+v", r)
	}

	// A firing at the period's start is in its first week.
	c = change(side("R", "G", nil), side("R", "G", firingsAt("R", from, to.Add(-time.Hour))), "R")
	if r := CheckExpectations(c, readExpect(t, "expectations:\n  - rule: R\n    budget: {week: 1}\n"), nil, nil).Results[0]; r.After != "2" {
		t.Errorf("at from: %+v", r)
	}

	// At most so many expectations.
	big := "expectations:\n" + strings.Repeat("  - {rule: R, budget: {week: 1}}\n", MaxExpectationEntries+1)
	if _, err := ReadExpectations(strings.NewReader(big)); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("too many: %v", err)
	}
}

// The second review's cases.
func TestCheckExpectationsReviewAgain(t *testing.T) {
	to := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	from := to.Add(-7 * 24 * time.Hour)
	// One rule's entries, given apart, are one: an unchanged file says
	// nothing new or taken away.
	split := readExpect(t, "expectations:\n  - rule: R\n    budget: {week: 5}\n  - rule: R\n    catches: [{incident: INC-1}]\n")
	if len(split.Expectations) != 1 || split.Expectations[0].Budget == nil || len(split.Expectations[0].Catches) != 1 {
		t.Fatalf("merged: %+v", split.Expectations)
	}
	b := &Report{Rules: []RuleResult{{Rule: "R", Group: "G1"}}}
	a := &Report{Rules: []RuleResult{{Rule: "R", Group: "G1", Firings: 1}}, Firings: firingsAt("R", to.Add(-time.Hour))}
	c := &Comparison{From: from, To: to, Before: b, After: a, Rules: []RuleChange{{Rule: "R", Before: &b.Rules[0], After: &a.Rules[0]}}}
	if k := CheckExpectations(c, split, split, nil); slices.ContainsFunc(k.Results, func(r ExpectResult) bool { return r.Changed || r.Removed }) {
		t.Errorf("unchanged, split: %+v", k.Results)
	}
	// The same thing twice for one rule is refused: which holds isn't for
	// the order to decide.
	for _, s := range []string{
		"expectations:\n  - rule: R\n    budget: {week: 5}\n  - rule: R\n    budget: {week: 9}\n",
		"expectations:\n  - rule: R\n    catches: [{incident: INC-1, within: 5m}, {incident: INC-1, within: 30m}]\n",
	} {
		if _, err := ReadExpectations(strings.NewReader(s)); err == nil || !strings.Contains(err.Error(), "given twice") {
			t.Errorf("%q: %v", s, err)
		}
	}
	// A rule the change moves out of the group that held it: said, held to
	// what the group's expectation was, and failing nothing.
	moved := &Report{Rules: []RuleResult{{Rule: "R", Group: "G2", Firings: 40}}, Firings: firingsAt("R", to.Add(-time.Hour), to.Add(-2*time.Hour))}
	c = &Comparison{From: from, To: to, Before: b, After: moved, Rules: []RuleChange{{Rule: "R", Before: &b.Rules[0], After: &moved.Rules[0]}}}
	k := CheckExpectations(c, readExpect(t, "expectations:\n  - group: G1\n    budget: {week: 1}\n"), nil, nil)
	if len(k.Results) != 1 || !k.Results[0].Moved || !k.Results[0].Removed || k.Broken() != 0 || k.Results[0].After != "2" ||
		!strings.Contains(k.Results[0].heldTo(Clean), "no longer holds it") {
		t.Errorf("moved out of the group: %+v", k.Results)
	}
}
