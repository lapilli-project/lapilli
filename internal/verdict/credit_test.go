package verdict

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
)

func caught(id string, cs ...Catch) IncidentResult {
	return IncidentResult{Incident: incident.Incident{ID: id}, CaughtBy: cs}
}

func TestCredit(t *testing.T) {
	m := func(n int) time.Duration { return time.Duration(n) * time.Minute }
	for _, tc := range []struct {
		name  string
		rules []RuleResult
		incs  []IncidentResult
		want  map[string][4]any // incidents, first, alone, spare
	}{{
		name:  "alone and first",
		rules: []RuleResult{{Rule: "a"}, {Rule: "b"}},
		incs: []IncidentResult{
			caught("1", Catch{Rule: "a", Delay: m(1)}),
			caught("2", Catch{Rule: "a", Delay: m(1)}, Catch{Rule: "b", Delay: m(5)}),
		},
		want: map[string][4]any{"a": {2, 2, 1, false}, "b": {1, 0, 0, false}},
	}, {
		name:  "always second is spare, once it has been twice",
		rules: []RuleResult{{Rule: "a"}, {Rule: "b"}},
		incs: []IncidentResult{
			caught("1", Catch{Rule: "a", Delay: m(1)}, Catch{Rule: "b", Delay: m(5)}),
			caught("2", Catch{Rule: "a", Delay: m(2)}, Catch{Rule: "b", Delay: m(3)}),
		},
		want: map[string][4]any{"a": {2, 2, 0, false}, "b": {2, 0, 0, true}},
	}, {
		name:  "only once is too little to call it spare",
		rules: []RuleResult{{Rule: "a"}, {Rule: "b"}},
		incs:  []IncidentResult{caught("1", Catch{Rule: "a", Delay: m(1)}, Catch{Rule: "b", Delay: m(5)})},
		want:  map[string][4]any{"a": {1, 1, 0, false}, "b": {1, 0, 0, false}},
	}, {
		name:  "a tie: neither is first, and either could go",
		rules: []RuleResult{{Rule: "a"}, {Rule: "b"}},
		incs: []IncidentResult{
			caught("1", Catch{Rule: "a", Delay: m(1)}, Catch{Rule: "b", Delay: m(1)}),
			caught("2", Catch{Rule: "a", Delay: m(1)}, Catch{Rule: "b", Delay: m(1)}),
		},
		want: map[string][4]any{"a": {2, 0, 0, true}, "b": {2, 0, 0, true}},
	}, {
		name:  "a noisy rule's catch takes nothing from one that isn't noisy, and covers nothing",
		rules: []RuleResult{{Rule: "good"}, {Rule: "noisy", Noisy: true}},
		incs: []IncidentResult{
			caught("1", Catch{Rule: "noisy", Delay: m(-5), Noisy: true}, Catch{Rule: "good", Delay: m(1)}),
			caught("2", Catch{Rule: "noisy", Delay: m(-5), Noisy: true}, Catch{Rule: "good", Delay: m(1)}),
		},
		want: map[string][4]any{"good": {2, 2, 2, false}, "noisy": {2, 0, 0, false}},
	}, {
		name:  "a noisy rule another covers is spare",
		rules: []RuleResult{{Rule: "good"}, {Rule: "noisy", Noisy: true}},
		incs: []IncidentResult{
			caught("1", Catch{Rule: "good", Delay: m(1)}, Catch{Rule: "noisy", Delay: m(5), Noisy: true}),
			caught("2", Catch{Rule: "good", Delay: m(1)}, Catch{Rule: "noisy", Delay: m(5), Noisy: true}),
		},
		want: map[string][4]any{"good": {2, 2, 2, false}, "noisy": {2, 0, 0, true}},
	}, {
		name:  "only noisy rules caught it: they count among themselves",
		rules: []RuleResult{{Rule: "x", Noisy: true}, {Rule: "y", Noisy: true}},
		incs: []IncidentResult{
			caught("1", Catch{Rule: "x", Delay: m(1), Noisy: true}),
			caught("2", Catch{Rule: "x", Delay: m(1), Noisy: true}, Catch{Rule: "y", Delay: m(5), Noisy: true}),
		},
		want: map[string][4]any{"x": {2, 2, 1, false}, "y": {1, 0, 0, false}},
	}, {
		name:  "a warning doesn't cover what pages",
		rules: []RuleResult{{Rule: "pages", Severity: "critical"}, {Rule: "warns", Severity: "warning"}},
		incs: []IncidentResult{
			caught("1", Catch{Rule: "warns", Delay: m(1)}, Catch{Rule: "pages", Delay: m(5)}),
			caught("2", Catch{Rule: "warns", Delay: m(1)}, Catch{Rule: "pages", Delay: m(5)}),
		},
		want: map[string][4]any{"pages": {2, 0, 0, false}, "warns": {2, 2, 0, false}},
	}, {
		name:  "what pages covers a warning",
		rules: []RuleResult{{Rule: "pages", Severity: "critical"}, {Rule: "warns", Severity: "warning"}},
		incs: []IncidentResult{
			caught("1", Catch{Rule: "pages", Delay: m(1)}, Catch{Rule: "warns", Delay: m(5)}),
			caught("2", Catch{Rule: "pages", Delay: m(1)}, Catch{Rule: "warns", Delay: m(5)}),
		},
		want: map[string][4]any{"pages": {2, 2, 0, false}, "warns": {2, 0, 0, true}},
	}, {
		name:  "severities with no known order cover only their own",
		rules: []RuleResult{{Rule: "a", Severity: "p1"}, {Rule: "b", Severity: "p2"}, {Rule: "c", Severity: "p2"}},
		incs: []IncidentResult{
			caught("1", Catch{Rule: "a", Delay: m(1)}, Catch{Rule: "b", Delay: m(2)}, Catch{Rule: "c", Delay: m(3)}),
			caught("2", Catch{Rule: "a", Delay: m(1)}, Catch{Rule: "b", Delay: m(2)}, Catch{Rule: "c", Delay: m(3)}),
		},
		want: map[string][4]any{"a": {2, 2, 0, false}, "b": {2, 0, 0, false}, "c": {2, 0, 0, true}},
	}, {
		name:  "a catch by a rule not in the report is passed over",
		rules: []RuleResult{{Rule: "a"}},
		incs: []IncidentResult{
			caught("1", Catch{Rule: "gone", Delay: m(1)}, Catch{Rule: "a", Delay: m(5)}),
			caught("2", Catch{Rule: "gone", Delay: m(1)}, Catch{Rule: "a", Delay: m(5)}),
		},
		want: map[string][4]any{"a": {2, 0, 0, true}},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			Credit(tc.rules, tc.incs)
			for _, r := range tc.rules {
				got := [4]any{r.Incidents, r.First, r.Alone, r.Spare}
				if got != tc.want[r.Rule] {
					t.Errorf("%s: incidents, first, alone, spare = %v, want %v", r.Rule, got, tc.want[r.Rule])
				}
			}
		})
	}
}

// A report gives credit, and tells what it found.
func TestJudgeCredit(t *testing.T) {
	from := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	o := Options{From: from, To: from.Add(7 * 24 * time.Hour), Lead: 15 * time.Minute, RefireWithin: time.Hour, UnknownLen: time.Hour}
	at := func(h, m int) time.Time { return from.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	f := func(rule string, start time.Time) firing.Firing {
		return firing.Firing{Rule: rule, Start: start, End: start.Add(5 * time.Minute)}
	}
	rep := Judge(nil, []firing.Firing{
		f("Fast", at(1, 1)), f("Slow", at(1, 6)),
		f("Fast", at(5, 1)), f("Slow", at(5, 6)),
		f("Lonely", at(9, 1)),
	}, []incident.Incident{
		{ID: "A", Start: at(1, 0)}, {ID: "B", Start: at(5, 0)}, {ID: "C", Start: at(9, 0)},
	}, o)
	var b bytes.Buffer
	if err := Text(&b, rep, time.UTC); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"INCIDENTS  FIRST  ALONE", "Fast    2          2      0", "Lonely  1          1      1", "Slow    2          0      0",
		"Another rule caught every incident these caught, as soon, at the same severity or above: Slow."} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("no %q in:\n%s", want, b.String())
		}
	}
	b.Reset()
	if err := Prometheus(&b, rep, from); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`lapilli_rule_incidents{rule="Fast"} 2`, `lapilli_rule_incidents_first{rule="Fast"} 2`, `lapilli_rule_incidents_alone{rule="Lonely"} 1`} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("no %q in:\n%s", want, b.String())
		}
	}
}

// What a rule that wasn't replayed would have caught can't be known.
func TestCreditSaysWhatItCantKnow(t *testing.T) {
	from := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	rep := &Report{From: from, To: from.Add(time.Hour),
		Rules:     []RuleResult{{Rule: "a", Firings: 1, White: 1, Incidents: 1, First: 1, Alone: 1}, {Rule: "b", Unsupported: "uses Loki"}},
		Incidents: []IncidentResult{caught("1", Catch{Rule: "a"})}}
	var b bytes.Buffer
	if err := Text(&b, rep, time.UTC); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "1 rules couldn't be replayed, so what they would have caught isn't counted here.") {
		t.Errorf("no caveat in:\n%s", b.String())
	}
}
