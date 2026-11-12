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
		"Firings 4 → 2\nOf 2 incidents: with no alert 1 → 0\n",
		"Flappy         3 → –",
		"SearchLatency  – → 1",
		"0% → –",
		"1 changed rules keep the same stones:\n  CheckoutErrors\n",
		"INCIDENT               START\nINC-1 Checkout errors  2026-10-26 01:40\n  before  CheckoutErrors +2m\n  after   CheckoutErrors +20m\n",
		"INC-2 Search slow      2026-10-27 09:20\n  before  ✗ nothing fired\n  after   SearchLatency +5m\n",
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

	// Not replayed on either side, for the same reason: it differs, and
	// isn't said to fire as it did.
	same := Judge([]rules.Rule{{Name: "Down", Unsupported: "query failed: connection refused"}}, nil, incs, opts())
	c := Compare(same, same, []string{"Down"}, 0)
	c.Summarize(true)
	b.Reset()
	if err := CompareText(&b, c, time.UTC); err != nil {
		t.Fatal(err)
	}
	if out := b.String(); strings.Contains(out, "keep the same stones") || !strings.Contains(out, "Down (before): query failed") ||
		c.Summary.Alike != 0 || c.Summary.Unreplayed != 1 {
		t.Errorf("summary %+v:\n%s", c.Summary, out)
	}
}

func TestTableWidth(t *testing.T) {
	var b bytes.Buffer
	if err := table(&b, [][]string{{"RULE", "FIRED"}, {"디스크 부족", "3"}, {"Disk", "12"}}); err != nil {
		t.Fatal(err)
	}
	// Hangul takes two cells a character: the name is 11 cells wide.
	want := "RULE         FIRED\n디스크 부족  3\nDisk         12\n"
	if b.String() != want {
		t.Errorf("table:\n%s\nwant:\n%s", b.String(), want)
	}
}

func TestCompareSeverity(t *testing.T) {
	incs := []incident.Incident{{ID: "INC-1", Start: m(100), End: m(140)}}
	page := map[string]string{"severity": "page"}
	ticket := map[string]string{"severity": "ticket"}
	// Errors moved from page to ticket; its firings stay.
	before := Judge([]rules.Rule{{Name: "Errors", Expr: "x", Labels: page}},
		[]firing.Firing{{Rule: "Errors", Labels: page, Start: m(10), End: m(12)}, {Rule: "Errors", Labels: page, Start: m(102), End: m(110)}}, incs, opts())
	after := Judge([]rules.Rule{{Name: "Errors", Expr: "y", Labels: ticket}},
		[]firing.Firing{{Rule: "Errors", Labels: ticket, Start: m(10), End: m(12)}, {Rule: "Errors", Labels: ticket, Start: m(102), End: m(110)}}, incs, opts())
	c := Compare(before, after, []string{"Errors"}, 0)
	c.ChangedOnly = true
	var b bytes.Buffer
	if err := CompareText(&b, c, time.UTC); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"Firings of the changed rules 2 → 2\nBy severity: page 2 → 0, ticket 0 → 2\n",
		"incidents show the changed rules' catches alone",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text lacks %q:\n%s", want, out)
		}
	}
	// Same stones, so no row: the severity shows in the summary.
	if !c.Rules[0].Alike() {
		t.Errorf("Errors keeps its stones: %+v", c.Rules[0])
	}
	if got := severities(RuleChange{Before: &before.Rules[0], After: &after.Rules[0]}); got != "page → ticket" {
		t.Errorf("severities %q", got)
	}
}

func TestNoisyAt(t *testing.T) {
	o := opts()
	o.NoisyFirings, o.NoisyPrecision = 3, 0.5
	r := Judge(nil, []firing.Firing{{Rule: "A", Start: m(1)}, {Rule: "A", Start: m(200)}, {Rule: "A", Start: m(400)}}, nil, o)
	if !r.Rules[0].Noisy || r.NoisyAt.Firings != 3 {
		t.Errorf("3 firings, none catching, at 3 and 50%%: %+v %+v", r.Rules[0], r.NoisyAt)
	}
	if r := Judge(nil, []firing.Firing{{Rule: "A", Start: m(1)}}, nil, opts()); r.Rules[0].Noisy || r.NoisyAt.Firings != NoisyFirings {
		t.Errorf("defaults: %+v %+v", r.Rules[0], r.NoisyAt)
	}
}
