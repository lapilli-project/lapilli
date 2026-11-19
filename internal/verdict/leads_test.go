package verdict

import (
	"slices"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
)

// A lead of a signal's or a rule entry's own, and labels an entry requires
// (docs/design/judging.md, item 1 and "Coincidences").

func dur(d time.Duration) *time.Duration { return &d }

// leadSignals are errors, following --lead (15m), and probe, 5m of its
// own; MemoryPressure's entry gives 1h against errors' windows, and
// requires service; Errors' entry names errors and probe, and gives none.
func leadSignals() Options {
	o := Options{Lead: 15 * time.Minute, UnknownLen: time.Hour, Match: []string{"service"}}
	o.Signals = NewSignals([]Signal{
		{Name: "errors", For: 5 * time.Minute, Record: true},
		{Name: "probe", For: 2 * time.Minute, Match: []string{"service"}, Record: true, Lead: dur(5 * time.Minute)},
	}, []RuleEntry{
		{Rules: "^MemoryPressure$", Signals: []string{"errors"}, Match: []string{"service"}, Lead: dur(time.Hour), Require: []string{"service"}},
		{Rules: "^Errors$", Signals: []string{"errors", "probe"}},
	})
	return o
}

var leadAt = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func leadWindow(signal string, labels incident.Labels) incident.Incident {
	return incident.Incident{ID: "impact-" + signal + "-1", Start: leadAt, End: leadAt.Add(30 * time.Minute), Candidate: true, Signal: signal, Labels: labels}
}

// The lead a firing is judged by: its entry's against its signals'
// windows, else the signal's, else --lead; --lead of every incident written
// down, the rule's entry whatever it says, and of a window of a signal no
// longer in effect.
func TestLeadFor(t *testing.T) {
	o := leadSignals()
	checkout := incident.Labels{"service": {"checkout"}}
	written := incident.Incident{ID: "INC-1", Start: leadAt, End: leadAt.Add(time.Hour), Labels: checkout}
	confirmed := written
	confirmed.Signal = "errors" // a confirmation of an errors window carries it
	gone := leadWindow("latency", checkout)
	for _, c := range []struct {
		rule string
		in   incident.Incident
		want time.Duration
	}{
		{"MemoryPressure", leadWindow("errors", checkout), time.Hour},
		{"MemoryPressure", confirmed, time.Hour},
		{"Errors", leadWindow("errors", checkout), 15 * time.Minute},
		{"Errors", leadWindow("probe", checkout), 5 * time.Minute},
		{"Other", leadWindow("probe", checkout), 5 * time.Minute},
		{"MemoryPressure", written, 15 * time.Minute},
		{"MemoryPressure", gone, 15 * time.Minute},
	} {
		if got := o.LeadFor(c.rule, c.in); got != c.want {
			t.Errorf("%s against %s (signal %q): %s, want %s", c.rule, c.in.ID, c.in.Signal, got, c.want)
		}
	}
	if got := o.MaxLead(); got != time.Hour {
		t.Errorf("the longest lead: %s", got)
	}
	if got := o.LongestLeadFor(leadWindow("probe", checkout)); got != 5*time.Minute {
		t.Errorf("the longest lead against probe's window: %s", got)
	}
	if got, ok := o.Reach(); !ok || got != time.Hour+5*time.Minute {
		t.Errorf("reach: %s %v, want errors' 1h and its 5m for", got, ok)
	}
	plain := Options{Lead: 15 * time.Minute}
	if plain.MaxLead() != 15*time.Minute || plain.LongestLeadFor(leadWindow("errors", checkout)) != 15*time.Minute {
		t.Error("with no named signal, every lead is --lead")
	}
	if _, ok := plain.Reach(); ok {
		t.Error("a reach with no named signal")
	}
}

// Judging keeps its shape: an entry's 1h makes white a firing that began 50
// minutes before a window, leaves black one that began two hours before
// and is still on, and catches nothing written down more than --lead
// before it. Written incidents keep --lead.
func TestLeadCatches(t *testing.T) {
	o := leadSignals()
	checkout := map[string]string{"service": "checkout"}
	errs := leadWindow("errors", incident.Labels{"service": {"checkout"}})
	early := firing.Firing{Rule: "MemoryPressure", Labels: checkout, Start: leadAt.Add(-50 * time.Minute)}
	if !Catches(early, errs, o) {
		t.Error("a cause 50m before the window, its entry's lead 1h: not caught")
	}
	if on := (firing.Firing{Rule: "MemoryPressure", Labels: checkout, Start: leadAt.Add(-2 * time.Hour)}); Catches(on, errs, o) {
		t.Error("on since two hours before: caught")
	}
	if late := (firing.Firing{Rule: "Errors", Labels: checkout, Start: leadAt.Add(-50 * time.Minute)}); Catches(late, errs, o) {
		t.Error("Errors 50m before, by errors' --lead 15m: caught")
	}
	written := incident.Incident{ID: "INC-1", Start: leadAt, End: leadAt.Add(time.Hour), Labels: incident.Labels{"service": {"checkout"}}}
	if Catches(early, written, o) {
		t.Error("an incident written down caught from the entry's 1h: written incidents keep --lead")
	}
	if !Catches(firing.Firing{Rule: "MemoryPressure", Labels: checkout, Start: leadAt.Add(-10 * time.Minute)}, written, o) {
		t.Error("an incident written down not caught within --lead")
	}
	// The probe's 5m, for every rule judged against it.
	probe := leadWindow("probe", incident.Labels{"service": {"checkout"}})
	if f := (firing.Firing{Rule: "Other", Labels: checkout, Start: leadAt.Add(-10 * time.Minute)}); Catches(f, probe, o) {
		t.Error("10m before the probe's window, its lead 5m: caught")
	}
	if f := (firing.Firing{Rule: "Other", Labels: checkout, Start: leadAt.Add(-4 * time.Minute)}); !Catches(f, probe, o) {
		t.Error("4m before the probe's window: not caught")
	}
	// A lead of 0: from the window's start on.
	zero := leadSignals()
	zero.Signals.List[1].Lead = dur(0)
	zero.Signals = NewSignals(zero.Signals.List, zero.Signals.Entries)
	if f := (firing.Firing{Rule: "Other", Labels: checkout, Start: leadAt.Add(-time.Minute)}); Catches(f, probe, zero) {
		t.Error("a minute before a window whose signal's lead is 0: caught")
	}
	if f := (firing.Firing{Rule: "Other", Labels: checkout, Start: leadAt}); !Catches(f, probe, zero) {
		t.Error("at the start of a window whose signal's lead is 0: not caught")
	}
}

// require: on an entry, opt-in: a firing of its rules without the label,
// or a window without it, agrees with none of its signals' windows, though
// the match alone would agree on a label one of them lacks; an entry
// without it agrees as ever, and a written incident is matched on the
// match in effect.
func TestRequire(t *testing.T) {
	o := leadSignals()
	node := map[string]string{"node": "n1"} // a node's memory alert: no service
	at := leadAt.Add(-5 * time.Minute)
	errs := leadWindow("errors", incident.Labels{"service": {"checkout"}})
	if Catches(firing.Firing{Rule: "MemoryPressure", Labels: node, Start: at}, errs, o) {
		t.Error("a firing with no service caught a window its entry requires service of")
	}
	if !Catches(firing.Firing{Rule: "MemoryPressure", Labels: map[string]string{"service": "checkout"}, Start: at}, errs, o) {
		t.Error("a firing that carries service not caught")
	}
	thinned := leadWindow("errors", incident.Labels{"team": {"shop"}}) // its service lost where its series met
	if Catches(firing.Firing{Rule: "MemoryPressure", Labels: map[string]string{"service": "checkout"}, Start: at}, thinned, o) {
		t.Error("a window with no service caught by a rule whose entry requires it")
	}
	if !Catches(firing.Firing{Rule: "Errors", Labels: node, Start: at}, errs, o) {
		t.Error("an entry without require: a firing with no service agrees no more")
	}
	written := incident.Incident{ID: "INC-1", Start: leadAt, End: leadAt.Add(time.Hour), Labels: incident.Labels{"service": {"checkout"}}}
	if !Catches(firing.Firing{Rule: "MemoryPressure", Labels: node, Start: at}, written, o) {
		t.Error("require held against an incident written down")
	}
	if o.Agrees("MemoryPressure", node, errs) {
		t.Error("Agrees: a firing with no service, require: [service]")
	}
}

// With no lead set anywhere and no require, judging is as it was: the
// same stones as with the options before leads.
func TestNoLeadUnchanged(t *testing.T) {
	o := leadSignals()
	for i := range o.Signals.List {
		o.Signals.List[i].Lead = nil
	}
	var es []RuleEntry
	for _, e := range o.Signals.Entries {
		e.Lead, e.Require = nil, nil
		es = append(es, e)
	}
	o.Signals = NewSignals(o.Signals.List, es)
	if o.MaxLead() != o.Lead || o.Signals.Leads() {
		t.Fatalf("leads with none set: %s", o.MaxLead())
	}
	in := leadWindow("errors", incident.Labels{"service": {"checkout"}})
	for _, d := range []time.Duration{-20 * time.Minute, -15 * time.Minute, -14 * time.Minute, 0, 30 * time.Minute, 31 * time.Minute} {
		f := firing.Firing{Rule: "MemoryPressure", Labels: map[string]string{"service": "checkout"}, Start: leadAt.Add(d)}
		want := !f.Start.Before(in.Start.Add(-o.Lead)) && !f.Start.After(in.End)
		if got := Catches(f, in, o); got != want {
			t.Errorf("%s after the start: caught %v, want %v", d, got, want)
		}
	}
	rep := Judge(nil, nil, nil, o)
	if s := rep.Settings; slices.ContainsFunc(s.ImpactSignals, func(x Signal) bool { return x.Lead != nil }) {
		t.Errorf("settings: %+v", s.ImpactSignals)
	}
}
