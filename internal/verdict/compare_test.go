package verdict

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/rules"
)

func TestCompare(t *testing.T) {
	incs := []incident.Incident{
		{ID: "INC-1", Title: "Checkout errors", Start: m(100), End: m(140)},
		{ID: "INC-2", Title: "Search slow", Start: m(2000), End: m(2020)},
	}
	silent := rules.Rule{Name: "Silent", Expr: "z"}
	// In place: CheckoutErrors catches INC-1 at +2m, Flappy fires for
	// nothing, nothing catches INC-2.
	before := Judge(
		[]rules.Rule{silent, {Name: "CheckoutErrors", Expr: "x"}, {Name: "Flappy", Expr: "y"}},
		[]firing.Firing{
			{Rule: "CheckoutErrors", Start: m(102), End: m(130)},
			{Rule: "Flappy", Start: m(10), End: m(12)},
			{Rule: "Flappy", Start: m(40), End: m(42)},
			{Rule: "Flappy", Start: m(500), End: m(502)},
		}, incs, opts())
	// Proposed: CheckoutErrors waits longer, Flappy is gone, SearchLatency
	// is new and catches INC-2.
	after := Judge(
		[]rules.Rule{silent, {Name: "CheckoutErrors", Expr: "x", For: 20 * time.Minute}, {Name: "SearchLatency", Expr: "w"}},
		[]firing.Firing{
			{Rule: "CheckoutErrors", Start: m(120), End: m(130)},
			{Rule: "SearchLatency", Start: m(2005), End: m(2015)},
		}, incs, opts())
	c := Compare(before, after, []string{"CheckoutErrors", "Flappy", "CheckoutErrors", "SearchLatency"}, 1)

	var names []string
	for _, rc := range c.Rules {
		names = append(names, rc.Rule)
	}
	if strings.Join(names, " ") != "Flappy SearchLatency CheckoutErrors" {
		t.Errorf("rules %v, want each once, those whose firings change most first", names)
	}
	if rc := c.Rules[0]; rc.Before == nil || rc.After != nil || rc.Before.Firings != 3 {
		t.Errorf("Flappy, taken away: %+v", rc)
	}
	if !c.Rules[2].Alike() {
		t.Errorf("CheckoutErrors fires once and catches once in both: %+v", c.Rules[2])
	}
	if len(c.Incidents) != 2 || c.Incidents[0].Before[0].Delay != 2*time.Minute || c.Incidents[0].After[0].Delay != 20*time.Minute ||
		len(c.Incidents[1].Before) != 0 || c.Incidents[1].After[0].Rule != "SearchLatency" {
		t.Errorf("incidents %+v", c.Incidents)
	}

	var b bytes.Buffer
	if err := CompareText(&b, c, time.UTC); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"Rules compared: 1 changed, 1 added, 1 taken away, 1 the same",
		"Firings 4 → 2; incidents with no alert 1 → 0 of 2",
		"Flappy         3 → –",
		"SearchLatency  – → 1",
		"0% → –",
		"1 changed rules keep the same stones: CheckoutErrors",
		"INC-1 Checkout errors  2026-10-26 01:40  CheckoutErrors +2m  CheckoutErrors +20m",
		"✗ nothing fired",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text lacks %q:\n%s", want, out)
		}
	}

	// Rules changed, incidents caught alike.
	same := Compare(before, before, []string{"Flappy"}, 2)
	b.Reset()
	if err := CompareText(&b, same, time.UTC); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "Every incident is caught as it was.") {
		t.Errorf("text:\n%s", b.String())
	}
}

func TestCompareUnreplayed(t *testing.T) {
	incs := []incident.Incident{{ID: "INC-1", Start: m(100), End: m(140)}}
	// A fix: Typo couldn't be replayed before, and fires twice after.
	before := Judge([]rules.Rule{{Name: "Typo", Unsupported: "query failed: bad_data"}, {Name: "Paused", Unsupported: rules.Paused}}, nil, incs, opts())
	after := Judge([]rules.Rule{{Name: "Typo", Expr: "x"}, {Name: "Paused", Unsupported: "classic_conditions expressions"}},
		[]firing.Firing{{Rule: "Typo", Start: m(101), End: m(110)}, {Rule: "Typo", Start: m(500), End: m(510)}}, incs, opts())
	var b bytes.Buffer
	if err := CompareText(&b, Compare(before, after, []string{"Typo", "Paused"}, 0), time.UTC); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"Typo  ? → 2  ? → 1",
		"Typo (before): query failed: bad_data",
		"Paused (before): paused",
		"Paused (after): classic_conditions expressions",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Paused  ") {
		t.Errorf("Paused replayed on neither side, so it should have no row:\n%s", out)
	}
}
