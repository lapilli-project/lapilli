package verdict

import (
	"fmt"
	"strings"
	"testing"
)

// TestPartialTop: the rules whose replay failed are named at the top, ten
// at most, the rest said to be below; those no run replays aren't.
func TestPartialTop(t *testing.T) {
	r := &Report{}
	for i := range 12 {
		r.Rules = append(r.Rules, RuleResult{Rule: fmt.Sprintf("R%02d", 12-i), Unsupported: "query failed: HTTP 429: slow down"})
	}
	r.Rules = append(r.Rules, RuleResult{Rule: "cpu-high", Unsupported: "a CloudWatch metric alarm isn't PromQL"},
		RuleResult{Rule: "Late", Unsupported: "not finished before the timeout"}, RuleResult{Rule: "Fine", Replayed: true})
	us := r.UnreplayedRules()
	if len(us) != 14 || us[0].Rule != "Late" || us[1].Rule != "R01" || us[13] != (Unreplayed{Rule: "cpu-high", Reason: "a CloudWatch metric alarm isn't PromQL"}) {
		t.Fatalf("%+v", us)
	}
	var b strings.Builder
	partial(&b, us, len(r.Rules))
	want := "! 13 of 15 rules couldn't be replayed, and are left out of what follows:\n" +
		"  Late: not finished before the timeout\n  R01: query failed: HTTP 429: slow down\n"
	if !strings.HasPrefix(b.String(), want) || !strings.HasSuffix(b.String(), "  R09: query failed: HTTP 429: slow down\n  and 3 more, listed below\n") ||
		strings.Contains(b.String(), "cpu-high") {
		t.Errorf("got\n%s", b.String())
	}
	b.Reset()
	partial(&b, []Unreplayed{{Rule: "cpu-high", Reason: "a CloudWatch metric alarm isn't PromQL"}}, 2)
	if b.Len() != 0 {
		t.Errorf("a rule no run replays named at the top:\n%s", b.String())
	}
}
