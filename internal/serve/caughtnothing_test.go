package serve

import (
	"context"
	"strings"
	"testing"
)

// A rule with judged firings, none of which caught anything, and too few to
// be noisy, is "Caught nothing", a warning, not OK; one that caught
// something is OK; noisy, no record and silent are as they were.
func TestRowCaughtNothing(t *testing.T) {
	for _, c := range []struct {
		name string
		l    ruleLine
		want string
	}{
		{"7 firings, none caught", ruleLine{Firings: 7, White: 0}, "nothing"},
		{"7 firings, one caught", ruleLine{Firings: 7, White: 1}, "ok"},
		{"one firing, none caught", ruleLine{Firings: 1}, "nothing"},
		{"7 firings, 3 judged, none caught", ruleLine{Firings: 7, Unjudged: 4}, "nothing"},
		{"7 firings, none judged", ruleLine{Firings: 7, Unjudged: 7, NoRecord: true}, "unrecorded"},
		{"12 firings, none caught", ruleLine{Firings: 12, Noisy: true}, "noisy"},
		{"no firing", ruleLine{}, "silent"},
	} {
		r := rowOf(c.l, 0)
		if r.Perf != c.want {
			t.Errorf("%s: %q, want %q", c.name, r.Perf, c.want)
		}
		if r.Perf == "nothing" && r.PerfClass() != "status-warn" || r.Perf == "ok" && r.PerfClass() != "status-ok" {
			t.Errorf("%s: drawn as %q", c.name, r.PerfClass())
		}
	}
}

// The rules list and a rule's page say "Caught nothing" for a rule none of
// whose judged firings caught anything, where they said OK, and say the bar
// for noisy in effect, which the settings can change: next to the badge on
// the page, as the badge's title in the list, in each language. Pick the
// facet "Caught nothing" to list them.
func TestCaughtNothingShown(t *testing.T) {
	s := facetServer(t)
	h := s.Handler()
	all := rulesFor(t, s, "weeks=4")
	var name string
	for _, r := range all.Rows {
		if r.Perf == "nothing" {
			name = r.Name
			break
		}
	}
	if name == "" {
		t.Fatal("no rule caught nothing")
	}
	if got := rulesFor(t, s, "weeks=4&perf=nothing"); len(got.Rows) != 11 {
		t.Errorf("perf=nothing: %d rules, want 11", len(got.Rows))
	}
	_, list, _ := get(t, h, "/rules?weeks=4")
	if !strings.Contains(list, "Caught nothing") || !strings.Contains(list, `title="noisy at 10 firings or more with under 20% caught"`) {
		t.Errorf("the list: no badge or no bar")
	}
	_, page, _ := get(t, h, "/rules/"+name+"?weeks=4")
	if !strings.Contains(page, "Caught nothing") || !strings.Contains(page, "noisy at 10 firings or more with under 20% caught") || strings.Contains(page, "○</span> OK") {
		t.Errorf("the page of %s: %s", name, page)
	}
	ko := pageIn(t, h, "/rules/"+name+"?weeks=4", "", "ko")
	if !strings.Contains(ko, "정탐 없음") || !strings.Contains(ko, "알림 10건 이상에 정탐률 20% 미만이면 노이즈") {
		t.Errorf("the page of %s in Korean: %s", name, ko)
	}

	// With another bar in effect, set in the settings, the page says that
	// one.
	if err := s.l.SetSettings(context.Background(), map[string]string{"noisy-firings": "5", "noisy-precision": "0.125"}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	_, page, _ = get(t, h, "/rules/"+name+"?weeks=4")
	if !strings.Contains(page, "noisy at 5 firings or more with under 12.5% caught") {
		t.Errorf("the page of %s with another bar: %s", name, page)
	}
}
