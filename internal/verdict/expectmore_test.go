package verdict

import (
	"strings"
	"testing"
	"time"
)

// Labels are a selector as --rules-select takes one, matched against the
// labels a rule's alerts carry; one that selects every rule, or doesn't
// read, is refused; re-fires are a budget of their own.
func TestReadExpectationsLabelsAndRefires(t *testing.T) {
	for want, s := range map[string]string{
		"give rule, group or labels, one of them":                  "expectations:\n  - rule: A\n    labels: severity=page\n    budget: {week: 3}\n",
		"labels: \"=page\": \"\" isn't a label's name":             "expectations:\n  - labels: \"=page\"\n    budget: {week: 3}\n",
		"it selects every rule":                                    "expectations:\n  - labels: \" \"\n    budget: {week: 3}\n",
		"a budget below nothing":                                   "expectations:\n  - labels: severity=page\n    budget: {refires: -1}\n",
		"a budget of nothing: give week, asleep, refires or pages": "expectations:\n  - labels: severity=page\n    budget: {}\n",
		"labels severity=page: a budget of re-fires given twice": "expectations:\n  - labels: severity=page\n    budget: {refires: 1}\n" +
			"  - labels: \"severity=page\"\n    budget: {refires: 2}\n",
	} {
		if _, err := ReadExpectations(strings.NewReader(s)); err == nil || !strings.Contains(err.Error(), strings.TrimPrefix(want, "labels: ")) {
			t.Errorf("%q: %v, want %q", s, err, want)
		}
	}
	e := readExpect(t, "expectations:\n  - labels: severity in (page, critical), !silenced\n    budget: {refires: 2}\n")
	x := e.Expectations[0]
	if x.Selector() != "labels: severity in (page, critical), !silenced" {
		t.Errorf("selector: %s", x.Selector())
	}
	for _, c := range []struct {
		labels map[string]string
		want   bool
	}{
		{map[string]string{"severity": "page"}, true},
		{map[string]string{"severity": "warning"}, false},
		{map[string]string{"severity": "page", "silenced": "yes"}, false},
		{nil, false}, // labels not known: held by no selector
	} {
		if got := x.Holds("A", "g", c.labels); got != c.want {
			t.Errorf("%v: %v", c.labels, got)
		}
	}
	if x.For("A", "g") {
		t.Error("held by labels with none known")
	}
}

// A change is held to an expectation by labels by the labels each side's
// rule carries: a rule that takes severity=page on is held to it after,
// and was held to nothing before. Re-fires are counted by week as firings
// are.
func TestCheckExpectationsByLabelsAndRefires(t *testing.T) {
	to := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	from := to.Add(-7 * 24 * time.Hour)
	fs := firingsAt("R", to.Add(-time.Hour), to.Add(-2*time.Hour), to.Add(-3*time.Hour))
	fs[1].Refired, fs[2].Refired = true, true
	before := &Report{Rules: []RuleResult{{Rule: "R", Firings: 3}}, Firings: fs}
	after := &Report{Rules: []RuleResult{{Rule: "R", Firings: 3}}, Firings: fs}
	c := &Comparison{From: from, To: to, Before: before, After: after, Rules: []RuleChange{{Rule: "R", Before: &before.Rules[0], After: &after.Rules[0],
		BeforeLabels: map[string]string{"severity": "warning"}, AfterLabels: map[string]string{"severity": "page"}}}}
	e := readExpect(t, "expectations:\n  - labels: severity=page\n    budget: {refires: 1}\n")
	k := check(t, c, e, nil, nil)
	if len(k.Results) != 1 {
		t.Fatalf("%+v", k.Results)
	}
	r := k.Results[0]
	if r.What != "budget refires" || r.After != "2" || r.AfterState != ExpectBroken || r.Before != "2" {
		t.Errorf("re-fires: %+v", r)
	}
	if got := r.heldTo(Clean); got != "at most 1 re-fires a week" {
		t.Errorf("held to: %s", got)
	}
	// Held to it after only, a rule held to nothing before: the selector
	// no longer holding it before, the change is what breaks it.
	if !r.BrokenByChange() && r.BeforeState != ExpectBroken {
		t.Errorf("by labels: %+v", r)
	}
}

// A budget an annotation writes is read strictly: each of what it counts
// once, whole numbers, short; what is wrong is said without its text.
func TestParseBudget(t *testing.T) {
	b, err := ParseBudget("10/week, 2 asleep, 3 re-fires")
	if err != nil || *b.Week != 10 || *b.Asleep != 2 || *b.Refires != 3 {
		t.Fatalf("%+v %v", b, err)
	}
	if b, err := ParseBudget("4 refires"); err != nil || b.Week != nil || *b.Refires != 4 {
		t.Errorf("refires alone: %+v %v", b, err)
	}
	for _, s := range []string{"", "10", "ten/week", "10/week, 3/week", "10 week", "-1/week", "+1/week", "10/day", "1/week, 1 asleep, 1 refires, 1 asleep",
		"10/week; 2 asleep", strings.Repeat("1", 101), "1e3/week", "2000000/week", "<b>1</b>/week"} {
		_, err := ParseBudget(s)
		if err == nil {
			t.Errorf("%q read", s)
			continue
		}
		if len(s) > 3 && strings.Contains(err.Error(), s) {
			t.Errorf("%q: says its text: %v", s, err)
		}
	}
}
