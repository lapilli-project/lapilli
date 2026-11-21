package verdict

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/rules"
)

// A budget drafted is the most rounded up: as it is below 10, to the next
// 5 below 100, to two figures from there; never below 1.
func TestRoundUp(t *testing.T) {
	for n, want := range map[int]int{-3: 1, 0: 1, 1: 1, 7: 7, 9: 9, 10: 10, 11: 15, 15: 15, 16: 20, 47: 50, 96: 100, 99: 100, 100: 100,
		101: 110, 123: 130, 130: 130, 999: 1000, 1000: 1000, 1001: 1100, 1234: 1300, 98765: 99000} {
		if got := RoundUp(n); got != want {
			t.Errorf("RoundUp(%d) = %d, want %d", n, got, want)
		}
	}
	for d, want := range map[time.Duration]time.Duration{-5 * time.Minute: time.Minute, 0: time.Minute, 30 * time.Second: time.Minute,
		time.Minute: time.Minute, 61 * time.Second: 2 * time.Minute, 2*time.Minute + 10*time.Second: 3 * time.Minute, 47 * time.Minute: 47 * time.Minute} {
		if got := RoundUpWithin(d); got != want {
			t.Errorf("RoundUpWithin(%s) = %s, want %s", d, got, want)
		}
	}
}

// A draft holds each rule counted to what it did, rounded up, and to the
// incidents it caught; a rule that never fired to the least budget; while
// people slept only by hours that say when they sleep. The file it is
// written as is an expectations file, read back as it was, whatever the
// rules are called: their names are values the encoder writes, never
// pieced into the text, nor into a comment.
func TestDraftYAML(t *testing.T) {
	hostile := "Errors\n# not a comment: [x]: {y} *z &a !!str \"q\" 'r' `s` | > \u202e"
	d := Draft([]DraftRule{
		{Rule: "Restarts", Weeks: 12, Most: 37, Asleep: 4, Refires: 0, Sleeps: true,
			Caught: []DraftCatch{{"INC-2", 2*time.Minute + 5*time.Second}, {"INC-1", -time.Minute}, {"INC-2", time.Hour}}},
		{Rule: "Silent", Weeks: 12, Sleeps: true},
		{Rule: hostile, Weeks: 3, Most: 120, Asleep: 9, Refires: 11},
		{Rule: "New", Weeks: 0},                   // nothing counted yet
		{Rule: "Restarts", Weeks: 12, Most: 1000}, // given twice: the first holds
	})
	if len(d.Expectations) != 3 || len(d.Left) != 1 || d.Left[0] != (DraftLeftOut{"New", DraftNoWeek}) {
		t.Fatalf("%+v %+v", d.Expectations, d.Left)
	}
	b, err := d.YAML(DraftAbout{By: "lapilli serve", From: time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC),
		Weeks: DraftLedgerWeeks, Loc: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	e, err := ReadExpectations(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	got := map[string]string{}
	for _, x := range e.Expectations {
		var held []string
		for _, it := range x.Items() {
			held = append(held, it.What+" "+it.Held)
		}
		got[x.Rule] = strings.Join(held, "; ")
	}
	want := map[string]string{
		"Restarts": "budget week 40; budget asleep 4; budget refires 1; catches INC-2 within 3m; catches INC-1 within 1m",
		"Silent":   "budget week 1; budget asleep 1; budget refires 1",
		hostile:    "budget week 120; budget refires 15",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("read back:\n%v\nwant\n%v\n%s", got, want, b)
	}
	// Comments are Lapilli's words and numbers: no line of the file but
	// the hostile name's own value holds what the name does.
	// A name of more than one line is one line, quoted, its breaks
	// escaped: what it holds is never a line of its own.
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, "not a comment") && !strings.HasPrefix(strings.TrimSpace(l), `- rule: "Errors\n# not a comment`) {
			t.Errorf("a name's line: %q", l)
		}
	}
	if !strings.Contains(string(b), "Drafted by lapilli serve from its ledger's whole weeks from 2026-08-24 to 2026-11-16") ||
		!strings.Contains(string(b), "1 rules with nothing counted yet are left out") || !strings.Contains(string(b), "the most a week over 12 whole weeks: 37 fired, 4 while people slept") {
		t.Errorf("what it says of itself:\n%s", b)
	}
	// The same rules, the same draft.
	b2, _ := Draft([]DraftRule{{Rule: "Restarts", Weeks: 12, Most: 37, Asleep: 4, Sleeps: true,
		Caught: []DraftCatch{{"INC-2", 2*time.Minute + 5*time.Second}, {"INC-1", -time.Minute}, {"INC-2", time.Hour}}},
		{Rule: "Silent", Weeks: 12, Sleeps: true}, {Rule: hostile, Weeks: 3, Most: 120, Asleep: 9, Refires: 11}, {Rule: "New"}, {Rule: "Restarts", Weeks: 12, Most: 1000}}).
		YAML(DraftAbout{By: "lapilli serve", From: time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC), Weeks: DraftLedgerWeeks})
	if !bytes.Equal(b, b2) {
		t.Errorf("drafted twice, two drafts:\n%s\n%s", b, b2)
	}
	// Nothing to draft is still a file that reads.
	b, err = Draft(nil).YAML(DraftAbout{By: "lapilli verdict"})
	if err != nil {
		t.Fatal(err)
	}
	if e, err := ReadExpectations(bytes.NewReader(b)); err != nil || len(e.Expectations) != 0 {
		t.Errorf("an empty draft: %v %v\n%s", e, err, b)
	}
}

// A draft is bounded as an expectations file is: its catches, ten a rule
// and the file's in all; its rules, the file's; what is left out said.
func TestDraftBounded(t *testing.T) {
	var rs []DraftRule
	for i := range MaxExpectationEntries + 5 {
		r := DraftRule{Rule: fmt.Sprintf("R%05d", i), Weeks: 12, Most: i}
		for j := range 12 {
			r.Caught = append(r.Caught, DraftCatch{Incident: fmt.Sprintf("INC-%d-%d", i, j), Delay: time.Minute})
		}
		rs = append(rs, r)
	}
	d := Draft(rs)
	catches := 0
	for _, x := range d.Expectations {
		if len(x.Catches) > DraftCatchesARule {
			t.Fatalf("%s: %d catches", x.Rule, len(x.Catches))
		}
		catches += len(x.Catches)
	}
	if len(d.Expectations) != MaxExpectationEntries || catches != MaxExpectationCatches || len(d.Left) != 5 {
		t.Fatalf("%d rules, %d catches, %d left", len(d.Expectations), catches, len(d.Left))
	}
	b, err := d.YAML(DraftAbout{By: "lapilli verdict"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadExpectations(bytes.NewReader(b)); err != nil {
		t.Fatalf("the draft doesn't read back: %v", err)
	}
	if !strings.Contains(string(b), "5 more rules are left out") {
		t.Error("what was left out isn't said")
	}
}

// The history's draft leaves out an incident whose id spans lines, or
// holds what doesn't show: an expectations file can't name it, and the
// draft reads back.
func TestDraftLeavesOutIDsThatDontShow(t *testing.T) {
	d := Draft([]DraftRule{{Rule: "R", Weeks: 12, Most: 3, Caught: []DraftCatch{
		{Incident: "INC-1\n  - rule: Other", Delay: time.Minute}, {Incident: "INC-2\u202e", Delay: time.Minute}, {Incident: "INC-3", Delay: time.Minute}}}})
	if cs := d.Expectations[0].Catches; len(cs) != 1 || cs[0].Incident != "INC-3" {
		t.Fatalf("catches: %+v", cs)
	}
	b, err := d.YAML(DraftAbout{By: "lapilli verdict"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadExpectations(bytes.NewReader(b)); err != nil {
		t.Fatalf("the draft doesn't read back: %v\n%s", err, b)
	}
}

// From a report, a rule is counted by whole weeks back from the period's
// end, as a comparison holds budgets, and its catches the latest first;
// one that wasn't replayed is left out; with no whole week, scaled.
func TestDraftFromReport(t *testing.T) {
	to := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	from := to.Add(-14 * 24 * time.Hour)
	h := DefaultHours(time.UTC)
	fs := firingsAt("A", to.Add(-10*time.Hour), to.Add(-11*time.Hour), to.Add(-8*24*time.Hour), to.Add(-23*time.Hour-30*time.Minute))
	fs[1].Refired = true
	rep := &Report{From: from, To: to, Rules: []RuleResult{{Rule: "A", Firings: 4}, {Rule: "B"}, {Rule: "C", Unsupported: "no history"}},
		Firings: fs,
		Incidents: []IncidentResult{
			{Incident: incident.Incident{ID: "INC-1", Start: to.Add(-10 * 24 * time.Hour)}, CaughtBy: []Catch{{Rule: "A", Delay: 90 * time.Second}}},
			{Incident: incident.Incident{ID: "INC-2", Start: to.Add(-2 * 24 * time.Hour)}, CaughtBy: []Catch{{Rule: "A", Delay: -time.Minute}, {Rule: "B", Delay: time.Minute}}},
		}}
	rs := DraftFromReport(rep, func(string) *Hours { return h })
	if len(rs) != 2 {
		t.Fatalf("%+v", rs)
	}
	a := rs[0]
	// The last week: three firings (one at 00:30, while people slept),
	// one of them re-fired; the week before, one.
	if a.Rule != "A" || a.Weeks != 2 || a.Most != 3 || a.Asleep != 1 || a.Refires != 1 || !a.Sleeps {
		t.Errorf("A: %+v", a)
	}
	if len(a.Caught) != 2 || a.Caught[0].Incident != "INC-2" || a.Caught[1].Incident != "INC-1" {
		t.Errorf("A's catches, the latest first: %+v", a.Caught)
	}
	if b := rs[1]; b.Rule != "B" || b.Most != 0 || len(b.Caught) != 1 {
		t.Errorf("B: %+v", b)
	}
	// Three days: no whole week, scaled to one.
	rep.From = to.Add(-3 * 24 * time.Hour)
	rs = DraftFromReport(rep, func(string) *Hours { return nil })
	if a := rs[0]; a.Weeks != 0 || a.Scaled != 3 || a.Most != 7 || a.Sleeps {
		t.Errorf("scaled: %+v", a)
	}
}

// A rewrite is posted only when it meets every expectation it is held
// to, losing no catch; not known is never met; and only beside the rule
// as it is replayed to the end, and rewritten: a side cut short or not
// replayed catches nothing, and a catch lost there goes unseen.
func TestRewriteMeets(t *testing.T) {
	met := ExpectResult{What: "budget week", AfterState: ExpectMet}
	whole := &RuleResult{Rule: "R", Replayed: true}
	for _, c := range []struct {
		results       []ExpectResult
		lost          []string
		before, after *RuleResult
		want          bool
	}{
		{[]ExpectResult{met}, nil, whole, whole, true},
		{nil, nil, whole, whole, false}, // held to nothing
		{[]ExpectResult{met}, []string{"INC-1"}, whole, whole, false},
		{[]ExpectResult{met, {What: "catches INC-1", AfterState: ExpectBroken}}, nil, whole, whole, false},
		{[]ExpectResult{met, {What: "scenario t.yaml", AfterState: ExpectUnknown}}, nil, whole, whole, false},
		{[]ExpectResult{met}, nil, &RuleResult{Rule: "R", Unsupported: rules.Unfinished}, whole, false},
		{[]ExpectResult{met}, nil, &RuleResult{Rule: "R", Unsupported: rules.QueryFailed + "HTTP 400"}, whole, false},
		{[]ExpectResult{met}, nil, whole, &RuleResult{Rule: "R", Unsupported: rules.Unfinished}, false},
		{[]ExpectResult{met}, nil, nil, whole, false},
		{[]ExpectResult{met}, nil, whole, nil, false},
	} {
		if got := RewriteMeets(c.results, c.lost, c.before, c.after); got != c.want {
			t.Errorf("%+v %v %+v %+v: %v", c.results, c.lost, c.before, c.after, got)
		}
	}
}

// A rewrite's post says it is an agent's, backtested by Lapilli, with the
// numbers before and after; what the rule and the agent wrote can't
// format, link or close its code block, however many backticks it holds.
func TestRewriteMarkdown(t *testing.T) {
	name := "Errors`|[x](https://evil.example) @team"
	diff := "--- a\n+++ b\n-  for: 1m\n+  for: 10m ```` <script>\n+  # ‮````\n"
	got := RewriteMarkdown(Rewritten{Rule: name, From: time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC),
		Fired: [2]int{41, 8}, Caught: [2]int{3, 3}, Results: []ExpectResult{
			{What: "budget week", Limit: "10", Before: "41", BeforeState: ExpectBroken, After: "8", AfterState: ExpectMet},
			{What: "catches INC-43", Limit: "within 5m", Before: "+2m", BeforeState: ExpectMet, After: "+2m", AfterState: ExpectMet}}}, diff)
	for _, want := range []string{"#### An agent's rewrite of ``Errors`|[x](https://evil.example) @team``, backtested by Lapilli",
		"An agent wrote this rewrite, not Lapilli", "2026-08-24 00:00 UTC to 2026-11-16 00:00 UTC",
		"| at most 10 a week | 41 ✗ | 8 | met |", "Fired 41 → 8", "`````diff\n", "\\u{202e}"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in\n%s", want, got)
		}
	}
	// The fence closes only at its end: the only lines fenced as long
	// are its first and its last.
	var fences []string
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(strings.TrimLeft(l, " "), "`````") {
			fences = append(fences, l)
		}
	}
	if len(fences) != 2 || fences[0] != "`````diff" || fences[1] != "`````" {
		t.Errorf("fences: %q\n%s", fences, got)
	}
}

// A rewrite's post says whose expectations it meets: Lapilli's for the
// rule, or the ones the agent gave with it, which it may have loosened or
// left one out of, never as though they were the rule's.
func TestRewriteMarkdownSaysWhose(t *testing.T) {
	w := Rewritten{Rule: "Errors", Results: []ExpectResult{{What: "budget week", Limit: "10", Before: "41", After: "8", AfterState: ExpectMet}}}
	own := RewriteMarkdown(w, "")
	w.Given = true
	given := RewriteMarkdown(w, "")
	for _, c := range []struct {
		post, want, not string
	}{
		{own, "it meets every expectation Lapilli holds the rule to,", "the agent gave"},
		{own, "the agent's text, Lapilli's numbers", "text and expectations"},
		{given, "it meets every expectation the agent gave with it, which aren't necessarily the ones Lapilli holds the rule to,", "the rule is held to"},
		{given, "the agent's text and expectations, Lapilli's numbers", "\x00"},
	} {
		if !strings.Contains(c.post, c.want) || strings.Contains(c.post, c.not) {
			t.Errorf("want %q, not %q, in\n%s", c.want, c.not, c.post)
		}
	}
}
