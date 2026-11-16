package verdict

import (
	"bytes"
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
