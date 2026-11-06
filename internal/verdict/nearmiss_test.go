package verdict

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
)

func TestNearly(t *testing.T) {
	o := opts()
	in := incident.Incident{ID: "INC-1", Start: m(100), End: m(130), Labels: incident.Labels{"service": {"checkout"}}}
	held := func(from, to int, service string) firing.Firing {
		return firing.Firing{Rule: "R", Labels: map[string]string{"service": service}, Start: m(from), End: m(to)}
	}
	for _, c := range []struct {
		name   string
		held   []firing.Firing
		for_   time.Duration
		ok     bool
		newFor time.Duration
		delay  time.Duration
	}{
		// Held from +2m for 3m: with a "for" of 2m it fires at +4m, the last
		// evaluation that held.
		{"short", []firing.Firing{held(102, 105, "checkout")}, 5 * time.Minute, true, 2 * time.Minute, 4 * time.Minute},
		// Held long, but from just before the end: it fired too late. With a
		// "for" of 2m it fires at the incident's end.
		{"late", []firing.Firing{held(128, 160, "checkout")}, 5 * time.Minute, true, 2 * time.Minute, 30 * time.Minute},
		// Another service's alert doesn't catch it.
		{"other service", []firing.Firing{held(102, 105, "search")}, 5 * time.Minute, false, 0, 0},
		// Held only well before the incident: firing then catches nothing.
		{"before", []firing.Firing{held(60, 70, "checkout")}, 30 * time.Minute, false, 0, 0},
		// Held from before the lead, into it: a "for" long enough to wait for
		// the lead catches it.
		{"into the lead", []firing.Firing{held(80, 90, "checkout")}, 30 * time.Minute, true, 9 * time.Minute, -11 * time.Minute},
		// Of two stretches, the one that keeps more of the "for".
		{"best of two", []firing.Firing{held(101, 103, "checkout"), held(110, 120, "checkout")}, 15 * time.Minute, true, 9 * time.Minute, 19 * time.Minute},
		// A single evaluation: a "for" of nothing would have fired.
		{"one evaluation", []firing.Firing{held(110, 111, "checkout")}, 5 * time.Minute, true, 0, 10 * time.Minute},
	} {
		nm, ok := Nearly(in, c.held, c.for_, time.Minute, o)
		if ok != c.ok || ok && (nm.NewFor != c.newFor || nm.Delay != c.delay) {
			t.Errorf("%s: %v %+v, want %v, for %s at %s", c.name, ok, nm, c.ok, c.newFor, c.delay)
		}
	}
	// On a group evaluated every 5m, the "for" is in whole evaluations.
	nm, ok := Nearly(in, []firing.Firing{held(100, 115, "checkout")}, 15*time.Minute, 5*time.Minute, o)
	if !ok || nm.NewFor != 10*time.Minute || nm.Delay != 10*time.Minute || nm.Held != 15*time.Minute {
		t.Errorf("every 5m: %v %+v", ok, nm)
	}
}

func TestNearMissesText(t *testing.T) {
	r := &Report{From: t0, To: t0.Add(24 * time.Hour), Incidents: []IncidentResult{{Incident: incident.Incident{ID: "INC-1", Start: m(100)}, CaughtBy: []Catch{}}},
		NearMisses: []NearMiss{
			{Incident: "INC-1", Rule: "Short", From: 2 * time.Minute, Held: 3 * time.Minute, For: 5 * time.Minute, NewFor: 2 * time.Minute, Delay: 4 * time.Minute},
			{Incident: "INC-1", Rule: "Late", From: 0, Late: true, Fired: 30 * time.Minute, For: 30 * time.Minute, NewFor: 10 * time.Minute, Delay: 10 * time.Minute,
				After: &RuleResult{Firings: 3, Black: 2}},
		},
		NearMissesCut: true}
	var b bytes.Buffer
	if err := Text(&b, r, time.UTC); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Short  3m from +2m", "5m → 2m", "Late   from +0s, fired +30m", "0 → 3", "The time ran out before every incident was looked at"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("lacks %q:\n%s", want, b.String())
		}
	}
}
