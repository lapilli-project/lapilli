package verdict

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/rules"
)

func TestCheckReplay(t *testing.T) {
	from := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	at := func(min int, sec ...int) time.Time {
		s := 0
		if len(sec) > 0 {
			s = sec[0]
		}
		return from.Add(time.Duration(min)*time.Minute + time.Duration(s)*time.Second)
	}
	f := func(rule string, start time.Time, labels ...string) firing.Firing {
		l := map[string]string{}
		for i := 0; i+1 < len(labels); i += 2 {
			l[labels[i]] = labels[i+1]
		}
		return firing.Firing{Rule: rule, Labels: l, Start: start, End: start.Add(5 * time.Minute)}
	}
	known := []rules.Rule{
		{Name: "Same"}, {Name: "Late", Interval: 5 * time.Minute}, {Name: "Flaps"}, {Name: "Ghost"}, {Name: "Missed"},
		{Name: "Labels"}, {Name: "Template"}, {Name: "Quiet"}, {Name: "Before"}, {Name: "Edge"}, {Name: "EdgeAlone"},
		{Name: "NotReplayed", Unsupported: "uses Loki"},
	}
	replayed := []firing.Firing{
		f("Same", at(10), "svc", "a"), f("Same", at(60), "svc", "a"),
		f("Late", at(30), "svc", "a"), // nine minutes off is within its two evaluations of five
		f("Flaps", at(100), "svc", "a"),
		f("Ghost", at(200)),
		f("Labels", at(300), "svc", "a"),
		f("Template", at(400), "summary", "{{ $value }} errors"),
		{Rule: "Before", Labels: map[string]string{"svc": "a"}, Start: from.Add(-time.Hour), End: from.Add(time.Hour)},
		f("Edge", to.Add(-30*time.Second)),
		f("EdgeAlone", to.Add(-30*time.Second)), // can't tell: its match may be just past the end
	}
	real := []firing.Firing{
		f("Same", at(10, 30), "svc", "a", "cluster", "one"), f("Same", at(60, 30), "svc", "a", "cluster", "one"),
		f("Late", at(39), "svc", "a"),
		f("Flaps", at(100), "svc", "a"), f("Flaps", at(120), "svc", "a"), f("Flaps", at(140), "svc", "a"),
		f("Missed", at(500)),
		f("Labels", at(300), "svc", "b"), // the same time, another service
		f("Template", at(400), "summary", "12 errors"),
		f("Before", from, "svc", "a"),
		f("NotReplayed", at(5)),
		f("Watchdog", at(0)),
		f("Edge", to.Add(-10*time.Second)),
	}
	c := CheckReplay(known, replayed, real, from, to, time.Minute)
	got := map[string]RuleCheck{}
	for _, r := range c.Rules {
		got[r.Rule] = r
	}
	for _, tc := range []struct {
		rule                  string
		replayed, real, alike int
	}{
		{"Same", 2, 2, 2}, {"Late", 1, 1, 1}, {"Flaps", 1, 3, 1}, {"Ghost", 1, 0, 0}, {"Missed", 0, 1, 0},
		{"Labels", 1, 1, 0}, {"Template", 1, 1, 1}, {"Before", 1, 1, 1}, {"Edge", 1, 1, 1},
	} {
		r := got[tc.rule]
		if r.Replayed != tc.replayed || r.Real != tc.real || r.Alike != tc.alike {
			t.Errorf("%s: replayed %d, real %d, alike %d; want %d, %d, %d", tc.rule, r.Replayed, r.Real, r.Alike, tc.replayed, tc.real, tc.alike)
		}
	}
	if got["Same"].OffBy != 30*time.Second {
		t.Errorf("off by %s", got["Same"].OffBy)
	}
	if r := got["NotReplayed"]; r.Unsupported == "" || r.Real != 1 {
		t.Errorf("not replayed: %+v", r)
	}
	if _, ok := got["EdgeAlone"]; ok {
		t.Error("a firing too near the end to tell was counted")
	}
	if c.Silent != 2 || len(c.Elsewhere) != 1 || c.Elsewhere[0] != "Watchdog" {
		t.Errorf("silent %d, elsewhere %v", c.Silent, c.Elsewhere)
	}
	if c.Rules[0].Rule != "Flaps" && c.Rules[0].Rule != "Labels" {
		t.Errorf("those differing most should come first: %+v", c.Rules[0])
	}
	// The rule not replayed adds nothing to the totals.
	if c.Replayed != 9 || c.Real != 11 || c.Alike != 7 {
		t.Errorf("totals: %d %d %d", c.Replayed, c.Real, c.Alike)
	}

	c.Series = []string{`ALERTS{alertstate="firing"}`}
	var b bytes.Buffer
	if err := CheckText(&b, c, time.UTC); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Replay beside ALERTS", "7 firings alike, 2 only in the replay, 4 only in what really fired: 54% alike.",
		"5 rules fired alike, 4 differ, 2 fired neither way.", "ONLY REPLAYED", "Could not replay 1 rules", "NotReplayed: uses Loki",
		"Really fired, but no rule given has the name: Watchdog."} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("no %q in:\n%s", want, b.String())
		}
	}
}

// A firing is matched once, to the nearest: two real ones can't both be
// counted as alike with one replayed.
func TestCheckMatchesOnce(t *testing.T) {
	from := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	f := func(sec int) firing.Firing {
		return firing.Firing{Rule: "R", Start: from.Add(time.Hour + time.Duration(sec)*time.Second)}
	}
	c := CheckReplay([]rules.Rule{{Name: "R"}}, []firing.Firing{f(0)}, []firing.Firing{f(-100), f(10), f(90)}, from, from.Add(24*time.Hour), time.Minute)
	r := c.Rules[0]
	if r.Alike != 1 || r.Real != 3 || r.OffBy != 10*time.Second {
		t.Errorf("%+v", r)
	}
}

// Rules sharing a name are one alert to what really fired.
func TestCheckSharedName(t *testing.T) {
	from := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	f := func(sev string) firing.Firing {
		return firing.Firing{Rule: "R", Labels: map[string]string{"severity": sev}, Start: from.Add(time.Hour)}
	}
	c := CheckReplay([]rules.Rule{{Name: "R", Labels: map[string]string{"severity": "page"}}, {Name: "R", Labels: map[string]string{"severity": "ticket"}}},
		[]firing.Firing{f("page"), f("ticket")}, []firing.Firing{f("ticket"), f("page")}, from, from.Add(24*time.Hour), time.Minute)
	if len(c.Rules) != 1 || c.Rules[0].Alike != 2 {
		t.Errorf("%+v", c.Rules)
	}
}
