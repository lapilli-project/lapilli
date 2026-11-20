package verdict

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Scenarios are paths relative to the expectations file, inside its
// directory, each named once for one rule, 200 in all; nothing in the file
// names promtool, or what it is run with.
func TestReadExpectationsScenarios(t *testing.T) {
	e := readExpect(t, "expectations:\n  - rule: A\n    scenarios: [tests/a.yaml, a.test.yaml]\n  - rule: A\n    scenarios: [more/a.yaml]\n  - group: shop\n    scenarios: [shop.yaml]\n")
	if len(e.Expectations) != 2 || len(e.Expectations[0].Scenarios) != 3 || e.Expectations[1].Scenarios[0] != "shop.yaml" {
		t.Fatalf("%+v", e.Expectations)
	}
	items := e.Expectations[0].Items()
	if len(items) != 3 || items[0].What != "scenario tests/a.yaml" || items[0].Held != "passes" || items[0].Scenario != "tests/a.yaml" {
		t.Errorf("%+v", items)
	}
	many := "expectations:\n  - rule: A\n    scenarios:\n" + strings.Repeat("      - x.yaml\n", 0)
	for i := range MaxExpectationScenarios + 1 {
		many += fmt.Sprintf("      - t%d.yaml\n", i)
	}
	for want, s := range map[string]string{
		"want a path relative to the expectations file": "expectations:\n  - rule: A\n    scenarios: [../secret.yaml]\n",
		"scenario \"/etc/passwd\"":                      "expectations:\n  - rule: A\n    scenarios: [/etc/passwd]\n",
		"scenario \"a//b.yaml\"":                        "expectations:\n  - rule: A\n    scenarios: [a//b.yaml]\n",
		"scenario \"./a.yaml\"":                         "expectations:\n  - rule: A\n    scenarios: [./a.yaml]\n",
		"scenario \"tests/\"":                           "expectations:\n  - rule: A\n    scenarios: [tests/]\n",
		"scenario \"a\\\\b.yaml\"":                      "expectations:\n  - rule: A\n    scenarios: ['a\\b.yaml']\n",
		"scenario \"\"":                                 "expectations:\n  - rule: A\n    scenarios: ['']\n",
		"A: scenario t.yaml given twice":                "expectations:\n  - rule: A\n    scenarios: [t.yaml]\n  - rule: A\n    scenarios: [t.yaml]\n",
		"201 scenarios: at most 200 in all":             many,
		"field promtool not found":                      "expectations:\n  - rule: A\n    scenarios: [t.yaml]\n    promtool: /bin/sh\n",
		"field args not found":                          "expectations:\n  - rule: A\n    scenarios: [t.yaml]\n    args: [--debug]\n",
		"cannot unmarshal":                              "expectations:\n  - rule: A\n    scenarios: {file: t.yaml}\n",
	} {
		if _, err := ReadExpectations(strings.NewReader(s)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", s, err, want)
		}
	}
	if err := ScenarioPath(strings.Repeat("a", 513)); err == nil {
		t.Error("a path past 512 bytes")
	}
}

// runner is a ScenarioRunner that answers from a table, by side, rule and
// path, and notes what it was asked.
type runner struct {
	results map[string]ScenarioResult
	asked   []string
}

func (r *runner) RunScenario(side string, fromBefore bool, rule, path string) ScenarioResult {
	k := fmt.Sprintf("%s %t %s %s", side, fromBefore, rule, path)
	r.asked = append(r.asked, k)
	if x, ok := r.results[k]; ok {
		return x
	}
	return ScenarioResult{State: ExpectUnknown, Why: "not in the table"}
}

// A scenario is run on each side the rule is on: met, broken or not
// known, as budgets and catches are. One broken after and passed before is
// the change's; broken on both, already; not known never met nor broken.
// One that couldn't be run after is counted apart, on the summary: it
// can't be judged. Without a runner, not known: no --promtool.
func TestCheckExpectationsScenarios(t *testing.T) {
	to := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	from := to.Add(-7 * 24 * time.Hour)
	rep := func(rules ...string) *Report {
		r := &Report{}
		for _, n := range rules {
			r.Rules = append(r.Rules, RuleResult{Rule: n})
		}
		return r
	}
	before, after := rep("A", "B", "C", "D", "Gone"), rep("A", "B", "C", "D", "New")
	c := &Comparison{From: from, To: to, Before: before, After: after}
	for _, n := range []string{"A", "B", "C", "D"} {
		i := len(c.Rules)
		c.Rules = append(c.Rules, RuleChange{Rule: n, Before: &before.Rules[i], After: &after.Rules[i]})
	}
	c.Rules = append(c.Rules, RuleChange{Rule: "Gone", Before: &before.Rules[4]}, RuleChange{Rule: "New", After: &after.Rules[4]})
	c.Summarize(false)
	e := readExpect(t, `expectations:
  - rule: A
    scenarios: [a.yaml]
  - rule: B
    scenarios: [b.yaml]
  - rule: C
    scenarios: [c.yaml]
  - rule: D
    scenarios: [d.yaml]
  - rule: Gone
    scenarios: [gone.yaml]
  - rule: New
    scenarios: [new.yaml]
`)
	// Without a runner: each not known, never met.
	if err := c.Expect(e, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, r := range c.Expectations.Results {
		if r.Rule != "Gone" && (r.AfterState != ExpectUnknown || r.AfterWhy != string(NoPromtool) || r.verdictOf() != "not known: not run: no --promtool") {
			t.Errorf("without promtool: %+v", r)
		}
	}
	// Taken away, the rule fails its scenario; but not known before, it
	// isn't the change's to have broken.
	if s := c.Summary; s.ExpectationsBroken != 0 || s.ScenariosUnrun != 0 {
		t.Errorf("without promtool: %+v", s)
	}

	pass, fail := ScenarioResult{State: ExpectMet, Test: "t1"}, ScenarioResult{State: ExpectBroken, Said: "    alertname: A, time: 10m,\n        got:[]", Test: "t1"}
	unrun := ScenarioResult{State: ExpectUnknown, Why: "promtool took longer than 30s", Unrun: true, Test: "t1"}
	run := &runner{results: map[string]ScenarioResult{
		"before false A a.yaml": pass, "after false A a.yaml": fail,
		"before false B b.yaml": fail, "after false B b.yaml": fail,
		"before false C c.yaml": pass, "after false C c.yaml": unrun,
		"before false D d.yaml": fail, "after false D d.yaml": {State: ExpectMet, Test: "t2"},
		"before false Gone gone.yaml": pass,
		"after false New new.yaml":    fail,
	}}
	c.Scenarios = run
	if err := c.Expect(e, nil, nil); err != nil {
		t.Fatal(err)
	}
	got := map[string]ExpectResult{}
	for _, r := range c.Expectations.Results {
		got[r.Rule] = r
	}
	for rule, want := range map[string]struct {
		before, after, verdict string
	}{
		"A":    {"passes", "fails", "✗ broken by this change"},
		"B":    {"fails", "fails", "broken before too"},
		"C":    {"passes", "–", "not known: promtool took longer than 30s"},
		"D":    {"fails", "passes", "met now"},
		"Gone": {"passes", "taken away", "✗ broken by this change"},
		"New":  {"–", "fails", "✗ broken by this change"},
	} {
		r := got[rule]
		if r.Before != want.before || r.After != want.after || r.verdictOf() != want.verdict {
			t.Errorf("%s: %+v, verdict %q", rule, r, r.verdictOf())
		}
	}
	if !got["C"].Unrun || got["A"].Unrun || got["A"].Said == "" || !got["D"].Edited || got["A"].Edited {
		t.Errorf("unrun, said, edited: %+v", got)
	}
	if s := c.Summary; s.ExpectationsBroken != 3 || s.ExpectationsAlreadyBroken != 1 || s.ScenariosUnrun != 1 {
		t.Errorf("summary: %+v", s)
	}
	if got["D"].heldTo(Clean) != "passes the scenario d.yaml (its test file edited in this change)" {
		t.Errorf("held to: %s", got["D"].heldTo(Clean))
	}

	// The text and the Markdown say what promtool said of A after, its
	// lines as code; the JSON, each field.
	var text bytes.Buffer
	if err := ExpectationsText(&text, c.Expectations); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "promtool said, of A and a.yaml after the change:\n        alertname: A, time: 10m,\n") {
		t.Errorf("text:\n%s", text.String())
	}
	part := expectationsPart(c.Expectations)
	md := part.head + strings.Join(part.rows, "") + part.foot
	for _, want := range []string{
		"| `A` | passes the scenario `a.yaml` | passes | fails | **✗ broken by this change** |",
		"| `C` | passes the scenario `c.yaml` | passes | – | not known: promtool took longer than 30s |",
		"promtool said, of the scenarios after the change:\n\n- `A`, `a.yaml`: `alertname: A, time: 10m,` `got:[]`\n",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("Markdown: no %q\n%s", want, md)
		}
	}
	b, err := json.Marshal(c.Expectations)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"after_why":"promtool took longer than 30s"`, `"unrun":true`, `"edited":true`, `"said":"    alertname: A`} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("JSON: no %s\n%s", want, b)
		}
	}

	// Through serve: not run, said so.
	c.Scenarios = NotThroughServe
	if err := c.Expect(e, nil, nil); err != nil {
		t.Fatal(err)
	}
	if r := c.Expectations.Results[0]; r.AfterWhy != "not run through lapilli serve" || c.Summary.ScenariosUnrun != 0 {
		t.Errorf("through serve: %+v", r)
	}
}

// With --expect-before, a scenario the change keeps is run before from the
// file before, after from the file after; one it adds, on both sides from
// the file after; one it takes away, on both from the file before.
func TestCheckExpectationsScenariosBefore(t *testing.T) {
	to := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	before, after := &Report{Rules: []RuleResult{{Rule: "A"}}}, &Report{Rules: []RuleResult{{Rule: "A"}}}
	c := &Comparison{From: to.Add(-7 * 24 * time.Hour), To: to, Before: before, After: after,
		Rules: []RuleChange{{Rule: "A", Before: &before.Rules[0], After: &after.Rules[0]}}}
	run := &runner{}
	c.Scenarios = run
	was := readExpect(t, "expectations:\n  - rule: A\n    scenarios: [kept.yaml, gone.yaml]\n")
	is := readExpect(t, "expectations:\n  - rule: A\n    scenarios: [kept.yaml, new.yaml]\n")
	if err := c.Expect(is, was, nil); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"before true A kept.yaml": true, "after false A kept.yaml": true,
		"before false A new.yaml": true, "after false A new.yaml": true,
		"before true A gone.yaml": true, "after true A gone.yaml": true,
	}
	if len(run.asked) != len(want) {
		t.Errorf("asked %q", run.asked)
	}
	for _, k := range run.asked {
		if !want[k] {
			t.Errorf("asked %q", k)
		}
	}
}

// A scenario run before the change and not known after it, the change
// having moved its rule out of what promtool runs (out of Prometheus's
// format, or its definition out of reach), can't be judged: counted as
// unrun, so that FAIL_ON_EXPECTATIONS fails closed. Not known on both
// sides (no --promtool, through serve) counts nothing, as before.
func TestCheckExpectationsScenariosMovedOut(t *testing.T) {
	to := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	before, after := &Report{Rules: []RuleResult{{Rule: "A"}, {Rule: "B"}}}, &Report{Rules: []RuleResult{{Rule: "A"}, {Rule: "B"}}}
	c := &Comparison{From: to.Add(-7 * 24 * time.Hour), To: to, Before: before, After: after,
		Rules: []RuleChange{{Rule: "A", Before: &before.Rules[0], After: &after.Rules[0]}, {Rule: "B", Before: &before.Rules[1], After: &after.Rules[1]}}}
	c.Summarize(false)
	notProm := ScenarioResult{State: ExpectUnknown, Why: "not a rule in Prometheus's format, which promtool runs"}
	c.Scenarios = &runner{results: map[string]ScenarioResult{
		"before false A a.yaml": {State: ExpectMet, Test: "t1"}, "after false A a.yaml": notProm,
		"before false B b.yaml": notProm, "after false B b.yaml": notProm,
	}}
	e := readExpect(t, "expectations:\n  - rule: A\n    scenarios: [a.yaml]\n  - rule: B\n    scenarios: [b.yaml]\n")
	if err := c.Expect(e, nil, nil); err != nil {
		t.Fatal(err)
	}
	got := map[string]ExpectResult{}
	for _, r := range c.Expectations.Results {
		got[r.Rule] = r
	}
	if !got["A"].Unrun || got["B"].Unrun || c.Summary.ScenariosUnrun != 1 {
		t.Errorf("A run before and not after: unrun %v; B not known on both: unrun %v; summary %d", got["A"].Unrun, got["B"].Unrun, c.Summary.ScenariosUnrun)
	}
}
