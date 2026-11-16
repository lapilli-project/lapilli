package verdict

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
)

// A kind of marked window is a name a team chooses: a redacted report
// gives it an alias, in a rule's marked and a firing's alike, its counts
// as they were.
func TestRedactMarkedKinds(t *testing.T) {
	t0 := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	r := &Report{From: t0, To: t0.Add(time.Hour),
		Rules:   []RuleResult{{Rule: "Errors", Firings: 2, White: 1, Marked: map[string]MarkedCount{"black-friday": {Firings: 2, Caught: 1}}}},
		Firings: []Judged{{Firing: firing.Firing{Rule: "Errors", Start: t0}, White: true, Marked: []string{"black-friday"}}}}
	red := NewRedactor(nil).Report(r)
	var b bytes.Buffer
	if err := JSON(&b, red); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "black-friday") {
		t.Errorf("a kind's name in a redacted report:\n%s", b.String())
	}
	c, ok := red.Rules[0].Marked["kind-01"]
	if !ok || c.Firings != 2 || c.Caught != 1 || len(red.Firings[0].Marked) != 1 || red.Firings[0].Marked[0] != "kind-01" {
		t.Errorf("redacted: %+v %+v", red.Rules[0].Marked, red.Firings[0].Marked)
	}
	if r.Rules[0].Marked["black-friday"].Firings != 2 {
		t.Error("the report redacted was changed")
	}
}
