package verdict

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestPartialTop: the rules whose replay failed are named at the top, ten
// at most, the rest said to be below; those no run replays, and those whose
// query was refused as written, aren't.
func TestPartialTop(t *testing.T) {
	r := &Report{}
	for i := range 12 {
		r.Rules = append(r.Rules, RuleResult{Rule: fmt.Sprintf("R%02d", 12-i), Unsupported: "query failed for now: HTTP 429: slow down"})
	}
	r.Rules = append(r.Rules, RuleResult{Rule: "cpu-high", Unsupported: "a CloudWatch metric alarm isn't PromQL"},
		RuleResult{Rule: "Typo", Unsupported: "query failed: HTTP 400: bad_data: parse error"},
		RuleResult{Rule: "Late", Unsupported: "not finished before the timeout"}, RuleResult{Rule: "Fine", Replayed: true})
	us := r.UnreplayedRules()
	if len(us) != 15 || us[0].Rule != "Late" || us[1].Rule != "R01" || us[13] != (Unreplayed{Rule: "Typo", Reason: "query failed: HTTP 400: bad_data: parse error"}) ||
		us[14] != (Unreplayed{Rule: "cpu-high", Reason: "a CloudWatch metric alarm isn't PromQL"}) {
		t.Fatalf("%+v", us)
	}
	var b strings.Builder
	partial(&b, us, len(r.Rules), true)
	want := "! 13 of 16 rules couldn't be replayed, and are left out of what follows:\n" +
		"  Late: not finished before the timeout\n  R01: query failed for now: HTTP 429: slow down\n"
	if !strings.HasPrefix(b.String(), want) || !strings.HasSuffix(b.String(), "  R09: query failed for now: HTTP 429: slow down\n  and 3 more, listed below\n") ||
		strings.Contains(b.String(), "cpu-high") || strings.Contains(b.String(), "Typo") {
		t.Errorf("got\n%s", b.String())
	}
	b.Reset()
	partial(&b, []Unreplayed{{Rule: "cpu-high", Reason: "a CloudWatch metric alarm isn't PromQL"}}, 2, true)
	if b.Len() != 0 {
		t.Errorf("a rule no run replays named at the top:\n%s", b.String())
	}
}

// TestFailedFirst: the rules whose replay failed come first, whatever
// their names.
func TestFailedFirst(t *testing.T) {
	r := &Report{Rules: []RuleResult{{Rule: "Zed", Unsupported: "query failed for now: HTTP 503: down"},
		{Rule: "Alarm", Unsupported: "a CloudWatch metric alarm isn't PromQL"}}}
	if us := r.UnreplayedRules(); len(us) != 2 || us[0].Rule != "Zed" || !us[0].Failed || us[1].Failed {
		t.Errorf("%+v", us)
	}
}

// TestPartialReasonCut: a long reason is cut at the top, so that it doesn't
// push the rest out of a Slack message.
func TestPartialReasonCut(t *testing.T) {
	var b strings.Builder
	partial(&b, []Unreplayed{{Rule: "A", Reason: "query failed for now: " + strings.Repeat("é", 30000), Failed: true}}, 1, true)
	if b.Len() > 600 || !strings.Contains(b.String(), "é…\n") {
		t.Errorf("%d bytes: %.200s", b.Len(), b.String())
	}
}

// TestPartialCompare: in a comparison, a rule is counted once, whichever
// sides failed; and the foot lists only changed rules, so the rest at the
// top aren't said to be below.
func TestPartialCompare(t *testing.T) {
	failed := "query failed for now: HTTP 429: slow down"
	var same []RuleResult
	for i := range 12 {
		same = append(same, RuleResult{Rule: fmt.Sprintf("S%02d", i), Unsupported: failed})
	}
	before := &Report{Rules: append([]RuleResult{{Rule: "Changed", Unsupported: failed}}, same...)}
	after := &Report{Rules: append([]RuleResult{{Rule: "Changed", Unsupported: failed, Firings: 1}}, same...)}
	c := Compare(before, after, []string{"Changed"}, 12)
	c.Unreplayed = c.UnreplayedRules()
	var b strings.Builder
	if err := CompareText(&b, c, time.UTC); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "! 13 rules couldn't be replayed") || !strings.Contains(out, "  and 4 more, all in --json\n") ||
		strings.Contains(out, "listed below") {
		t.Errorf("got\n%s", out)
	}
}

// TestTextFromLedger: a report read back from lapilli serve's ledger, whose
// numbers keep what a rule fired before its last replay failed, doesn't say
// the rule is left out of them.
func TestTextFromLedger(t *testing.T) {
	r := &Report{Rules: []RuleResult{{Rule: "A", Replayed: true, Firings: 3}, {Rule: "B", Firings: 5, Unsupported: "query failed for now: HTTP 503: down"}}}
	var b strings.Builder
	if err := Text(&b, r, time.UTC); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "left out of what follows") || !strings.Contains(b.String(), "Could not replay 1 rules:\n  B: query failed for now") {
		t.Errorf("got\n%s", b.String())
	}
	r.Unreplayed = r.UnreplayedRules()
	b.Reset()
	if err := Text(&b, r, time.UTC); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "! 1 of 2 rules couldn't be replayed") {
		t.Errorf("a run's own report:\n%s", b.String())
	}
}
