package verdict

import (
	"slices"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
)

// A window of a signal not in effect, taken away, dropped by a start or set
// aside past eight (docs/design/judging.md, item 5): it keeps no rule's
// record, and only the rules with no entry catch it, on the labels it was
// found with; the incident a confirmation of one wrote keeps everyone's
// record, and is caught as its window was.

// goneOpts are signalOpts with probe taken away: errors and payments in
// effect, and the entries given, indexed by the rules' labels.
func goneOpts(entries ...RuleEntry) Options {
	o := signalOpts()
	list := slices.DeleteFunc(slices.Clone(o.Signals.List), func(x Signal) bool { return x.Name == "probe" })
	o.Signals = NewSignals(list, entries).Index(map[string]map[string]string{"Errors": {}, "Other": {}, "Probed": {}})
	return o
}

// probeWindow is a window probe found, matched on service alone, as it
// was: team is no constraint.
func probeWindow(from, to float64) incident.Incident {
	return window("probe", []string{"service"}, from, to, incident.Labels{"service": {"checkout"}, "team": {"probers"}})
}

// confirmed is the incident a confirmation of in wrote: its signal and
// match, and no candidate.
func confirmed(in incident.Incident) incident.Incident {
	in.ID, in.Candidate = "L-1", false
	return in
}

func TestNotInEffect(t *testing.T) {
	o := goneOpts()
	probe := probeWindow(10, 10.5)
	errs := window("errors", nil, 10, 10.5, incident.Labels{"service": {"checkout"}})
	written := incident.Incident{ID: "INC-1", Start: sigAt(10), End: sigAt(11), Labels: incident.Labels{"service": {"checkout"}}}
	unnamed := incident.Incident{ID: "impact-1", Candidate: true, Start: sigAt(10), End: sigAt(11), Labels: incident.Labels{"service": {"checkout"}}}
	for _, c := range []struct {
		in   incident.Incident
		o    Options
		want bool
	}{
		{probe, o, true},
		{probe, signalOpts(), false}, // probe in effect
		{probe, Options{}, true},     // no named signal in effect
		{confirmed(probe), o, false}, // written down
		{errs, o, false},
		{written, o, false},
		{unnamed, o, false},
		{unnamed, Options{}, false},
	} {
		if got := c.o.NotInEffect(c.in); got != c.want {
			t.Errorf("%s of %q, named signals %v: %v, want %v", c.in.ID, c.in.Signal, c.o.Signals.Named(), got, c.want)
		}
	}
	if !o.InEffect("errors") || o.InEffect("probe") || (Options{}).InEffect("errors") {
		t.Error("InEffect")
	}
}

// The record: a window of a signal not in effect keeps no rule's, with
// named signals in effect or none, where a window of one in effect keeps
// its own firings' and its confirmation everyone's.
func TestNotInEffectKeepsNoRecord(t *testing.T) {
	// A firing tied by its entry to payments alone, which keeps no record:
	// judged by what keeps everyone's alone.
	o := goneOpts(RuleEntry{Rules: "^Errors$", Signals: []string{"payments"}})
	fs := []firing.Firing{fire("Errors", 20, "service", "checkout", "team", "shop")}
	probe := probeWindow(10, 10.5)
	if rep := Judge(nil, fs, []incident.Incident{probe}, o); !rep.Firings[0].NoRecord || rep.Firings[0].White || !rep.Rules[0].NoRecord {
		t.Errorf("by a window of probe, taken away: no record %v, white %v", rep.Firings[0].NoRecord, rep.Firings[0].White)
	}
	if rep := Judge(nil, fs, []incident.Incident{confirmed(probe)}, o); rep.Firings[0].NoRecord || rep.Firings[0].White {
		t.Errorf("by its confirmation: no record %v, white %v; want judged, black", rep.Firings[0].NoRecord, rep.Firings[0].White)
	}
	// With no named signal in effect: the same, beside the windows of the
	// one with no name, which keep everyone's record as ever.
	plain := Options{From: sigT0, To: sigT0.Add(7 * 24 * time.Hour), Lead: 15 * time.Minute, UnknownLen: time.Hour, Match: []string{"service"}}
	if rep := Judge(nil, fs, []incident.Incident{probe}, plain); !rep.NoRecord || !rep.Firings[0].NoRecord {
		t.Errorf("no named signal, a window of probe: no record %v", rep.NoRecord)
	}
	unnamed := incident.Incident{ID: "impact-1", Candidate: true, Start: sigAt(5), End: sigAt(6), Labels: incident.Labels{"service": {"cart"}}}
	if rep := Judge(nil, fs, []incident.Incident{probe, unnamed}, plain); rep.NoRecord || rep.Firings[0].NoRecord {
		t.Errorf("no named signal, a window of the one with no name: no record %v", rep.NoRecord)
	}
	// Judged by week, as serve judges: the record of no week.
	weekly := plain
	weekly.RecordUnit = func(t time.Time) (time.Time, time.Time) { return sigT0, sigT0.Add(7 * 24 * time.Hour) }
	if rec := RecordOf([]incident.Incident{probe}, weekly); rec.Judges(sigAt(20)) || rec.JudgesAny(sigT0, sigT0.Add(7*24*time.Hour)) {
		t.Error("by week, a window of probe keeps the record")
	}
	named := o
	named.RecordUnit = weekly.RecordUnit
	if rec := RecordOf([]incident.Incident{probe}, named); rec.JudgesTied(sigAt(20), nil) || rec.JudgesTied(sigAt(20), []int{1}) {
		t.Error("by week, with named signals, a window of probe keeps the record")
	}
}

// The catches: a rule with no entry catches it, on the labels it was found
// with; a rule with an entry doesn't, whatever signals the entry names,
// probe among them, nor with no named signal in effect, the entries read
// for it all the same. Its confirmation is caught as it was.
func TestNotInEffectCaughtByRulesWithNoEntry(t *testing.T) {
	entries := []RuleEntry{
		{Rules: "^Errors$", Signals: []string{"errors"}},
		{Rules: "^Probed$", Signals: []string{"probe"}}, // names no signal in effect
	}
	o := goneOpts(entries...)
	probe := probeWindow(10, 10.5)
	for _, c := range []struct {
		f    firing.Firing
		want bool
	}{
		{fire("Other", 10, "service", "checkout", "team", "shop"), true}, // on service alone
		{fire("Other", 10, "service", "cart", "team", "shop"), false},
		{fire("Other", 9.8, "service", "checkout"), true}, // within --lead before it
		{fire("Errors", 10, "service", "checkout", "team", "shop"), false},
		{fire("Probed", 10, "service", "checkout", "team", "shop"), false},
	} {
		if got := Catches(c.f, probe, o); got != c.want {
			t.Errorf("%s %v catches probe's window: %v, want %v", c.f.Rule, c.f.Labels, got, c.want)
		}
		if got := Catches(c.f, confirmed(probe), o); got != c.want {
			t.Errorf("%s %v catches probe's window's confirmation: %v, want %v", c.f.Rule, c.f.Labels, got, c.want)
		}
	}
	// No named signal in effect, the entries kept: read for it.
	entriesOnly := Options{Lead: 15 * time.Minute, UnknownLen: time.Hour, Match: []string{"service", "team"}}
	entriesOnly.Signals = NewSignals(nil, entries).Index(map[string]map[string]string{"Errors": {}, "Other": {}, "Probed": {}})
	if entriesOnly.Signals.Named() {
		t.Fatal("entries alone are named signals")
	}
	if !Catches(fire("Other", 10, "service", "checkout", "team", "shop"), probe, entriesOnly) || Catches(fire("Probed", 10, "service", "checkout"), probe, entriesOnly) ||
		Catches(fire("Errors", 10, "service", "checkout"), probe, entriesOnly) {
		t.Error("with entries and no named signal in effect, a rule with an entry catches probe's window, or one with none doesn't")
	}
	// Written down, with no signal: everyone's, as ever.
	written := incident.Incident{ID: "INC-1", Start: sigAt(10), End: sigAt(11), Labels: incident.Labels{"service": {"checkout"}, "team": {"shop"}}}
	if !Catches(fire("Probed", 10, "service", "checkout", "team", "shop"), written, entriesOnly) || !Catches(fire("Errors", 10, "service", "checkout", "team", "shop"), written, o) {
		t.Error("a rule with an entry catches no incident written down")
	}
	// --lead, whatever lead probe or an entry had.
	led := goneOpts(RuleEntry{Rules: "^Probed$", Signals: []string{"probe"}, Lead: dur(time.Hour)})
	if a, b := led.LeadFor("Other", probe), led.LeadFor("Probed", probe); a != 15*time.Minute || b != 15*time.Minute {
		t.Errorf("the leads against probe's window: %s, %s", a, b)
	}
	// Each rule's entry looked for once, as for a window of a signal in
	// effect: judging looks for none again.
	looked := o.Signals.Looked()
	Judge(nil, []firing.Firing{fire("Other", 10, "service", "checkout"), fire("Errors", 10, "service", "checkout")}, []incident.Incident{probe}, o)
	if n := o.Signals.Looked(); n != looked {
		t.Errorf("judging looked for %d entries more", n-looked)
	}
}

// A rule whose entry names no signal in effect is tied to none: no record
// by reading, and no window keeps it; an incident written down does.
func TestEntryNamingNoSignalInEffect(t *testing.T) {
	o := goneOpts(RuleEntry{Rules: "^Probed$", Signals: []string{"probe"}})
	if ties := o.Signals.Ties("Probed", map[string]string{"service": "checkout"}, o.Match); len(ties) != 0 {
		t.Errorf("tied to %v", ties)
	}
	fs := []firing.Firing{fire("Probed", 80, "service", "checkout", "team", "shop")}
	errs := window("errors", nil, 70, 70.5, incident.Labels{"service": {"checkout"}, "team": {"shop"}})
	if rep := Judge(nil, fs, []incident.Incident{probeWindow(70, 70.5), errs}, o); !rep.Firings[0].NoRecord {
		t.Error("judged by the windows of a signal not in effect, or of one its entry doesn't name")
	}
	written := incident.Incident{ID: "INC-1", Start: sigAt(60), End: sigAt(61), Labels: incident.Labels{"service": {"cart"}}}
	if rep := Judge(nil, fs, []incident.Incident{written}, o); rep.Firings[0].NoRecord || rep.Firings[0].White {
		t.Error("not judged by an incident written down")
	}
}
