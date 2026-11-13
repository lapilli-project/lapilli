package verdict

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"lapilli.dev/internal/rules"
)

// TestPartialTop: the rules whose replay failed are named at the top, ten
// at most, the rest said to be below; those no run replays, and those whose
// query was refused as written, aren't.
func TestPartialTop(t *testing.T) {
	r := &Report{}
	for i := range 12 {
		r.Rules = append(r.Rules, RuleResult{Rule: fmt.Sprintf("R%02d", 12-i), Unsupported: rules.HistoryUnread + "HTTP 429: slow down"})
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
		"  Late: not finished before the timeout\n  R01: query failed: the history couldn't be read: HTTP 429: slow down\n"
	if !strings.HasPrefix(b.String(), want) || !strings.HasSuffix(b.String(), "  R09: query failed: the history couldn't be read: HTTP 429: slow down\n  and 3 more, listed below\n") ||
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
	r := &Report{Rules: []RuleResult{{Rule: "Zed", Unsupported: rules.HistoryUnread + "HTTP 503: down"},
		{Rule: "Alarm", Unsupported: "a CloudWatch metric alarm isn't PromQL"}}}
	if us := r.UnreplayedRules(); len(us) != 2 || us[0].Rule != "Zed" || !us[0].Failed || us[1].Failed {
		t.Errorf("%+v", us)
	}
}

// TestPartialReasonCut: a long reason is cut at the top, so that it doesn't
// push the rest out of a Slack message: to 400 bytes, "…" included, once
// what doesn't show is shown as \u{...}, at a rune.
func TestPartialReasonCut(t *testing.T) {
	for _, reason := range []string{rules.HistoryUnread + strings.Repeat("é", 30000), rules.HistoryUnread + strings.Repeat("\x1b", 1000) + "é"} {
		var b strings.Builder
		partial(&b, []Unreplayed{{Rule: "A", Reason: reason, Failed: true}}, 1, true)
		_, line, _ := strings.Cut(b.String(), "\n  A: ")
		line, _, _ = strings.Cut(line, "\n")
		if len(line) > maxReason || !strings.HasSuffix(line, "…") || !utf8.ValidString(line) {
			t.Errorf("%d bytes: %.200q", len(line), line)
		}
	}
	if got := cut("a"+strings.Repeat("é", 10), 7); got != "aé…" {
		t.Errorf("%q", got)
	}
}

// TestPartialCompare: in a comparison, a rule is counted once, whichever
// sides failed, and named once, a line a side, ten rules at most, the rest
// counted as rules; and the foot lists only changed rules, so the rest at
// the top aren't said to be below.
func TestPartialCompare(t *testing.T) {
	failed := rules.HistoryUnread + "HTTP 429: slow down"
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
	if !strings.Contains(out, "! 13 rules couldn't be replayed") || !strings.Contains(out, "  and 3 more, all in --json\n") ||
		!strings.Contains(out, "  Changed (after): ") || !strings.Contains(out, "  S08: ") || strings.Contains(out, "  S09: ") ||
		strings.Contains(out, "listed below") {
		t.Errorf("got\n%s", out)
	}
}

// TestTextFromLedger: a report read back from lapilli serve's ledger, whose
// numbers keep what a rule fired before its last replay failed, doesn't say
// the rule is left out of them.
func TestTextFromLedger(t *testing.T) {
	r := &Report{Rules: []RuleResult{{Rule: "A", Replayed: true, Firings: 3}, {Rule: "B", Firings: 5, Unsupported: rules.HistoryUnread + "HTTP 503: down"}}}
	var b strings.Builder
	if err := Text(&b, r, time.UTC); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "left out of what follows") || !strings.Contains(b.String(), "Could not replay 1 rules:\n  B: "+rules.HistoryUnread) {
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

// TestUnreplayedAgrees: in a comparison, the rules of the unreplayed list
// that have a side are the rules that differ that couldn't be replayed,
// each counted once, as the summary's unreplayed counts them and the
// Markdown says them, at its top and in its list; one alike in both, which
// has no side, is in the list alone.
func TestUnreplayedAgrees(t *testing.T) {
	unread := rules.HistoryUnread + "HTTP 503: down"
	before := Judge([]rules.Rule{{Name: "Both", Unsupported: unread}, {Name: "OneSide", Expr: "x"}, {Name: "Gone", Unsupported: unread},
		{Name: "Same", Unsupported: unread}, {Name: "Typo", Unsupported: "query failed: HTTP 400: bad_data"}}, nil, nil, opts())
	after := Judge([]rules.Rule{{Name: "Both", Unsupported: unread}, {Name: "OneSide", Unsupported: unread}, {Name: "New", Unsupported: rules.Paused},
		{Name: "Same", Unsupported: unread}, {Name: "Typo", Expr: "y"}}, nil, nil, opts())
	c := Compare(before, after, []string{"Both", "OneSide", "Gone", "New", "Typo"}, 1)
	c.Summarize(false)
	us := c.UnreplayedRules()
	sided := map[string]bool{}
	for _, u := range us {
		if u.Side != "" {
			sided[u.Rule] = true
		} else if u.Rule != "Same" {
			t.Errorf("%s without a side", u.Rule)
		}
	}
	if len(sided) != 5 || c.Summary.Unreplayed != len(sided) || len(us) != 7 {
		t.Errorf("summary %d, %d rules with a side in %+v", c.Summary.Unreplayed, len(sided), us)
	}
	var b strings.Builder
	if err := CompareMarkdown(&b, c, time.UTC); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"**5 of the rules that differ couldn't be replayed**", "#### Could not replay 5 rules\n"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("no %q in\n%s", want, b.String())
		}
	}
}
