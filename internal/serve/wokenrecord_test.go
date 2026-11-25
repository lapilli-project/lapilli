package serve

import (
	"context"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/verdict"
)

// With no incident written down, a report's woken section says how many
// firings came while people slept, not how many woke them for nothing; with
// some firings judged and some not, it says both; with every one judged,
// "for nothing" as before.
func TestWokenNoRecord(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, _ := server(t, func(c *Config) { c.Options.Hours = verdict.DefaultHours(time.UTC) })
	s.cfg.RecordByWeek = true
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	o, err := s.options(ctx)
	if err != nil {
		t.Fatal(err)
	}
	all := func(string) bool { return true }
	from, to := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	woken := func(rep *verdict.Report) (string, map[string]any) {
		t.Helper()
		line, nums, err := s.wokenSection(ctx, rep, from, to, o, all)
		if err != nil {
			t.Fatal(err)
		}
		m, _ := nums.(map[string]any)
		return line, m
	}
	report := func() *verdict.Report {
		t.Helper()
		rep, err := s.ledgerReportFor(ctx, from, to, o, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}

	// The fixture's span has an incident on the Monday: the firings in the
	// two weeks after it judged, those before it not. Both, as the command
	// line says them.
	rep := report()
	if rep.NoRecord || rep.Unjudged == 0 {
		t.Fatalf("the fixture's record: %+v", rep)
	}
	line, nums := woken(rep)
	if line != "Firings that woke people for nothing, or came while they slept with no incident written down to judge them by: 8. By rule: Errors (8)." ||
		nums["unjudged"] != true || nums["no_record"] != false {
		t.Errorf("some judged: %q %v", line, nums)
	}

	// No record at all: how many came while people slept, and no "for
	// nothing".
	none := &verdict.Report{NoRecord: true, Rules: []verdict.RuleResult{{Rule: "Errors", Firings: 20, Unjudged: 20, AsleepBlack: 8}, {Rule: "Slow", Firings: 3, Unjudged: 3}}}
	line, nums = woken(none)
	if line != "Firings that came while people slept, with no incident written down to judge them by: 8. By rule: Errors (8)." ||
		nums["no_record"] != true || strings.Contains(line, "for nothing") {
		t.Errorf("no record: %q %v", line, nums)
	}
	s.cfg.Lang = "ko"
	if line, _ := woken(none); line != "자는 동안 울린 발화 (판정할 기록된 인시던트 없음): 8건. 규칙별: Errors (8)." || strings.Contains(line, "오탐") {
		t.Errorf("in Korean: %q", line)
	}
	s.cfg.Lang = ""
	if line, _ := woken(&verdict.Report{NoRecord: true, Rules: []verdict.RuleResult{{Rule: "Errors", Firings: 3, Unjudged: 3}}}); line != "Nothing came while people slept." {
		t.Errorf("none asleep: %q", line)
	}

	// Every firing judged: for nothing, as before.
	judged := &verdict.Report{Rules: []verdict.RuleResult{{Rule: "Errors", Firings: 20, White: 1, AsleepBlack: 7}}}
	if line, nums := woken(judged); line != "Firings that woke people for nothing: 7. By rule: Errors (7)." || nums["no_record"] != false || nums["unjudged"] != false {
		t.Errorf("judged: %q %v", line, nums)
	}
	if line, _ := woken(&verdict.Report{Rules: []verdict.RuleResult{{Rule: "Errors", Firings: 2, White: 2}}}); line != "Nothing woke people for nothing." {
		t.Errorf("none for nothing: %q", line)
	}
}

// The digest's notifications line doesn't call those for firings with no
// record "for nothing".
func TestSentLineNoRecord(t *testing.T) {
	parallel(t)
	x := &sentLine{Notifications: 9, Rules: 2, Repeats: 1, Asleep: 4, ForNothing: 7, Most: []sentFor{{"Errors", 5}, {"Slow", 2}}}
	if got := x.text("en"); got != "Notifications sent, as the alert managers sent them: 9, for 2 rules; 1 repeats, 4 while people slept, 7 for firings that caught nothing. The most for nothing: Errors (5), Slow (2)." {
		t.Errorf("with a record: %q", got)
	}
	x.NoRecord = true
	got := x.text("en")
	if got != "Notifications sent, as the alert managers sent them: 9, for 2 rules; 1 repeats, 4 while people slept, 7 for firings with no incident written down to judge them by. The most for those firings: Errors (5), Slow (2)." ||
		strings.Contains(got, "for nothing") {
		t.Errorf("no record: %q", got)
	}
	if got := x.text("ko"); strings.Contains(got, "헛알림") || !strings.Contains(got, "판정할 기록된 인시던트가 없는 발화에 대한 알림 7건") {
		t.Errorf("in Korean: %q", got)
	}
}

// A week of notifications alone, for a firing begun before it, has no
// firings to make it a week with no record; with none kept, its
// notifications still aren't known to be for nothing.
func TestWokenTileNotifiedAlone(t *testing.T) {
	parallel(t)
	woken := func(last *weekView) tile {
		t.Helper()
		ws := []*weekView{{Asleep: 3, Kept: true}, last, {}} // the week before, the last, the one going on
		for _, x := range (&Server{}).tiles("en", ws, ws[1], ws[0], "notifications, counted since 2026-10-19", "22:00–07:00", "2026-10-19",
			false, func() (int, int) { return 0, 0 }) {
			if x.key == "Woken for nothing" {
				return x
			}
		}
		t.Fatal("no tile")
		return tile{}
	}
	if x := woken(&weekView{Asleep: 6}); x.Value != "–" || x.Delta != "" || x.Better || x.Worse ||
		x.Of != "not known: no incident written down to judge their firings by; 6 notifications came while people slept that week (counted since 2026-10-19)" || x.Values != "3, –" {
		t.Errorf("none kept: %+v", x)
	}
	if x := woken(&weekView{Asleep: 6, Kept: true}); x.Value != "6" || x.Delta != "+3 on the week before" || !x.Worse ||
		x.Of != "notifications, counted since 2026-10-19" {
		t.Errorf("kept: %+v", x)
	}
	// None came while people slept: nothing to know, none kept or not.
	if x := woken(&weekView{}); x.Value != "0" {
		t.Errorf("none asleep: %+v", x)
	}
}
