package verdict

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
)

var sigT0 = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)

// sigAt is hour h of the period that starts at sigT0.
func sigAt(h float64) time.Time { return sigT0.Add(time.Duration(h * float64(time.Hour))) }

// window is a named signal's window, as incident.FromSignal makes one.
func window(signal string, match []string, from, to float64, labels incident.Labels) incident.Incident {
	return incident.Incident{ID: "impact-" + signal + fmt.Sprint(from), Signal: signal, Match: match, Candidate: true,
		Start: sigAt(from), End: sigAt(to), Labels: labels}
}

func fire(rule string, at float64, kv ...string) firing.Firing {
	l := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		l[kv[i]] = kv[i+1]
	}
	return firing.Firing{Rule: rule, Labels: l, Start: sigAt(at), End: sigAt(at + 0.25)}
}

// seen is what a signal returned: each series' labels.
func seen(labels []string, series ...map[string]string) *Seen {
	s := NewSeen(labels)
	for _, l := range series {
		s.Add(l)
	}
	return s
}

// signalOpts are three signals of the design's: errors, by service and
// team, as --match; probe, matched on service alone, whose data begins
// on day 3; payments, on no labels, which doesn't keep the record.
func signalOpts(entries ...RuleEntry) Options {
	match := []string{"service", "team"}
	list := []Signal{
		{Name: "errors", For: 5 * time.Minute, Record: true, Start: sigT0.Add(-14 * 24 * time.Hour),
			Seen: seen(match, map[string]string{"service": "search", "team": "find"}, map[string]string{"service": "checkout", "team": "shop"})},
		{Name: "probe", For: 2 * time.Minute, Match: []string{"service"}, Record: true, Start: sigAt(72),
			Seen: seen([]string{"service"}, map[string]string{"service": "checkout", "instance": "https://checkout.example"})},
		{Name: "payments", For: time.Minute, Match: []string{}, Record: false, Start: sigT0.Add(-14 * 24 * time.Hour), Seen: seen(nil, map[string]string{})},
	}
	return Options{From: sigT0, To: sigT0.Add(7 * 24 * time.Hour), Lead: 15 * time.Minute, UnknownLen: time.Hour, RefireWithin: time.Hour,
		Match: match, Signals: NewSignals(list, entries)}
}

// A window is matched on its signal's labels: a probe's, on service alone,
// whatever team the firing has; one matched on none, by any firing in its
// time. A window of the match in effect, and an incident written down, by
// --match, as before.
func TestSignalOwnMatch(t *testing.T) {
	o := signalOpts()
	probe := window("probe", []string{"service"}, 10, 10.5, incident.Labels{"service": {"checkout"}, "team": {"probers"}})
	pay := window("payments", []string{}, 20, 20.5, nil)
	errs := window("errors", nil, 30, 30.5, incident.Labels{"service": {"checkout"}, "team": {"shop"}})
	written := incident.Incident{ID: "INC-1", Start: sigAt(40), End: sigAt(41), Labels: incident.Labels{"service": {"checkout"}, "team": {"shop"}}}
	for _, c := range []struct {
		f    firing.Firing
		in   incident.Incident
		want bool
	}{
		{fire("ApiErrors", 10, "service", "checkout", "team", "shop"), probe, true},
		{fire("ApiErrors", 10, "service", "cart", "team", "shop"), probe, false},
		{fire("Anything", 20, "service", "search", "team", "find"), pay, true},
		{fire("ApiErrors", 30, "service", "checkout", "team", "platform"), errs, false},
		{fire("ApiErrors", 30, "service", "checkout", "team", "shop"), errs, true},
		{fire("ApiErrors", 40, "service", "checkout", "team", "platform"), written, false},
	} {
		if got := Catches(c.f, c.in, o); got != c.want {
			t.Errorf("%s %v catches %s: %v, want %v", c.f.Rule, c.f.Labels, c.in.ID, got, c.want)
		}
	}
	// The same probe window, with --match: team is no constraint left out.
	plain := o
	plain.Signals = nil
	probe.Match = nil
	if Catches(fire("ApiErrors", 10, "service", "checkout", "team", "shop"), probe, plain) {
		t.Error("with --match service,team, a probe window's team catches no shop firing")
	}
}

// A rule entry keeps its rules from other signals' windows, neither caught
// nor missed, and matches them on its own labels; a written incident is
// everyone's, by --match, whatever the entries say.
func TestRuleEntries(t *testing.T) {
	o := signalOpts(
		RuleEntry{Rules: "Slow$|Latency", Signals: []string{"errors"}, Match: []string{"service"}},
		RuleEntry{Rules: "^BackendMissing$", Signals: []string{"errors", "probe"}, Match: []string{"backend"}},
		RuleEntry{Labels: incident.Labels{"kind": {"latency"}}, Signals: []string{"probe"}},
		RuleEntry{Labels: incident.Labels{"kind": {"queue"}, "tier": {"front"}}, Signals: []string{"errors"}},
	)
	o.Signals = o.Signals.Index(map[string]map[string]string{
		"CheckoutSlow": {"kind": "latency"}, "BackendMissing": {}, "ApiErrors": {}, "QueueLatency": {"kind": "queue"}, "Tagged": {"kind": "latency"},
		"QueueDepth": {"kind": "queue"}, "FrontQueue": {"kind": "queue", "tier": "front"},
	})
	errs := window("errors", nil, 10, 10.5, incident.Labels{"service": {"checkout"}, "team": {"shop"}, "backend": {"checkout-v2"}})
	probe := window("probe", []string{"service"}, 20, 20.5, incident.Labels{"service": {"checkout"}})
	// A probe's window, its team the probers': matched on service alone, as
	// the probe is, by a rule whose entry gives no match of its own.
	probers := window("probe", []string{"service"}, 25, 25.5, incident.Labels{"service": {"checkout"}, "team": {"probers"}})
	written := incident.Incident{ID: "INC-1", Start: sigAt(30), End: sigAt(31), Labels: incident.Labels{"service": {"checkout"}, "team": {"shop"}}}
	for _, c := range []struct {
		f    firing.Firing
		in   incident.Incident
		want bool
	}{
		// The name first: CheckoutSlow's labels would give it the third.
		{fire("CheckoutSlow", 10, "service", "checkout", "team", "platform"), errs, true},
		{fire("CheckoutSlow", 20, "service", "checkout"), probe, false},
		// By labels, the rule's: the third entry, probe's alone.
		{fire("Tagged", 10, "service", "checkout", "team", "shop"), errs, false},
		{fire("Tagged", 20, "service", "checkout"), probe, true},
		{fire("Tagged", 25, "service", "checkout", "team", "shop", "kind", "latency"), probers, true},
		{fire("BackendMissing", 10, "backend", "checkout-v2", "team", "platform"), errs, true},
		{fire("BackendMissing", 10, "backend", "cart-v1", "team", "shop"), errs, false},
		{fire("BackendMissing", 20, "backend", "checkout-v2"), probe, true},
		{fire("ApiErrors", 10, "service", "checkout", "team", "platform"), errs, false},
		{fire("BackendMissing", 30, "service", "checkout", "team", "platform"), written, false},
		{fire("CheckoutSlow", 30, "service", "checkout", "team", "shop"), written, true},
	} {
		if got := Catches(c.f, c.in, o); got != c.want {
			t.Errorf("%s %v catches %s: %v, want %v", c.f.Rule, c.f.Labels, c.in.ID, got, c.want)
		}
	}
	// By labels, every label of the entry's: one of two is none.
	for rule, want := range map[string]int{"CheckoutSlow": 1, "QueueLatency": 1, "BackendMissing": 2, "Tagged": 3, "ApiErrors": 0, "NeverSeen": 0,
		"QueueDepth": 0, "FrontQueue": 4} {
		if got := o.Signals.EntryNumber(rule); got != want {
			t.Errorf("%s's entry: %d, want %d", rule, got, want)
		}
	}
	// A window every rule is kept from is missed; one that kept a rule's
	// firing from it isn't black for it.
	rep := Judge(nil, []firing.Firing{fire("CheckoutSlow", 20, "service", "checkout")}, []incident.Incident{probe}, o)
	if !rep.Incidents[0].Missed() || rep.Rules[0].ImpactRule != 1 {
		t.Errorf("%+v %+v", rep.Incidents[0], rep.Rules[0])
	}
}

// Each rule's entry is looked for once, when the rules are indexed: judging
// a thousand firings of them looks for none again.
func TestRuleEntriesLookedOnce(t *testing.T) {
	o := signalOpts(RuleEntry{Rules: "^Slow", Signals: []string{"probe"}}, RuleEntry{Labels: incident.Labels{"team": {"platform"}}, Signals: []string{"errors"}})
	rules := map[string]map[string]string{}
	for i := range 50 {
		rules[fmt.Sprintf("Rule%d", i)] = map[string]string{"team": "shop"}
	}
	o.Signals = o.Signals.Index(rules)
	if n := o.Signals.Looked(); n != 50 {
		t.Fatalf("indexing 50 rules looked for %d entries", n)
	}
	var fs []firing.Firing
	for i := range 1000 {
		fs = append(fs, fire(fmt.Sprintf("Rule%d", i%50), float64(i%150), "service", "checkout", "team", "shop"))
	}
	var incs []incident.Incident
	for h := 0; h < 150; h += 3 {
		incs = append(incs, window("errors", nil, float64(h), float64(h)+0.5, incident.Labels{"service": {"checkout"}}))
	}
	Judge(nil, fs, incs, o)
	if n := o.Signals.Looked(); n != 50 {
		t.Errorf("judging looked for %d entries more", n-50)
	}
}

// The signals tied to a firing: an entry's, alone; else each whose series
// had its value of each label it is matched on, a label the firing hasn't,
// or a series hadn't, tying it; and, none of those keeping the record,
// every signal that does.
func TestTies(t *testing.T) {
	o := signalOpts(RuleEntry{Rules: "^Pay", Signals: []string{"payments"}})
	o.Signals = o.Signals.Index(map[string]map[string]string{"PayQueue": {}})
	names := func(f firing.Firing) []string { return o.Signals.Names(o.Signals.Ties(f.Rule, f.Labels, o.Match)) }
	for _, c := range []struct {
		f    firing.Firing
		want []string
	}{
		{fire("ApiErrors", 1, "service", "search", "team", "find"), []string{"errors", "payments"}},
		{fire("ApiErrors", 1, "service", "checkout", "team", "shop"), []string{"errors", "probe", "payments"}},
		{fire("ApiErrors", 1, "service", "checkout", "team", "pay"), []string{"probe", "payments"}},
		{fire("NodeDown", 1, "instance", "node-1"), []string{"errors", "probe", "payments"}},
		// Tied to payments alone, which doesn't keep the record: it waits
		// for every one that does.
		{fire("ApiErrors", 1, "service", "web", "team", "edge"), []string{"errors", "probe", "payments"}},
		{fire("PayQueue", 1, "service", "checkout", "team", "shop"), []string{"payments"}},
	} {
		if got := names(c.f); !slices.Equal(got, c.want) {
			t.Errorf("%s %v tied to %v, want %v", c.f.Rule, c.f.Labels, got, c.want)
		}
	}
	// payments matched on --match, and its series without labels: any value
	// ties, as any firing agrees with its windows. Matched on service, with
	// values none had: none ties; it falls back to every one that keeps the
	// record.
	o.Signals.List[2].Match = nil
	o.Signals.List[2].Seen = seen(o.Match, map[string]string{})
	if got := names(fire("ApiErrors", 1, "service", "cart", "team", "shop")); !slices.Equal(got, []string{"errors", "probe", "payments"}) {
		t.Errorf("a payments signal with no labels: tied to %v", got)
	}
	o.Signals.List[2].Record = true
	if got := names(fire("ApiErrors", 1, "service", "cart", "team", "shop")); !slices.Equal(got, []string{"payments"}) {
		t.Errorf("a payments signal with no labels, that keeps the record: tied to %v", got)
	}
	o.Signals.List[2].Record = false
	// A signal matched on a label --match hasn't: tied by its own labels.
	region := Signal{Name: "region", Match: []string{"region"}, Record: true, Start: sigT0, Seen: seen([]string{"region"}, map[string]string{"region": "eu"})}
	rs := NewSignals([]Signal{region, o.Signals.List[0]}, nil)
	if got := rs.Names(rs.Ties("ApiErrors", map[string]string{"service": "search", "team": "find", "region": "us"}, o.Match)); !slices.Equal(got, []string{"errors"}) {
		t.Errorf("a firing in a region the signal never returned: tied to %v", got)
	}
	o.Signals.List[2].Seen = seen(o.Match, map[string]string{"service": "payments", "team": "pay"})
	if got := names(fire("ApiErrors", 1, "service", "cart", "team", "shop")); !slices.Equal(got, []string{"errors", "probe"}) {
		t.Errorf("none tying it: %v, want the two that keep the record", got)
	}
}

// What a signal returned is bounded: past 1,000 values a label, or 256 KB
// a signal, it is taken to have returned every value of that label.
func TestSeenBounds(t *testing.T) {
	s := NewSeen([]string{"service", "pod"})
	for i := range MaxSeenValues {
		s.Add(map[string]string{"service": fmt.Sprintf("svc-%d", i), "pod": "p"})
	}
	if s.Ties(map[string]string{"service": "svc-new"}, []string{"service"}) {
		t.Error("1,000 values: a new one ties nothing yet")
	}
	s.Add(map[string]string{"service": "svc-1001", "pod": "p"})
	if !s.Ties(map[string]string{"service": "svc-new"}, []string{"service"}) || s.Bytes() > 100 {
		t.Errorf("past 1,000 values every value ties, and the values are let go: %d bytes kept", s.Bytes())
	}
	b := NewSeen([]string{"pod"})
	long := strings.Repeat("x", 1000)
	for i := 0; i < 300; i++ {
		b.Add(map[string]string{"pod": fmt.Sprintf("%s-%d", long, i)})
	}
	if b.Bytes() > MaxSeenBytes || !b.Ties(map[string]string{"pod": "new"}, []string{"pod"}) {
		t.Errorf("%d bytes: over 256 KB every value ties", b.Bytes())
	}
}

// A firing is judged when the signals tied to it that keep the record had
// all started, or a window of one of them, or an incident written down,
// was on near it; a window keeps only the record of the firings tied to
// its signal. A rule an entry ties to a signal that doesn't keep the
// record has none by reading it.
func TestRecordBySignal(t *testing.T) {
	o := signalOpts(RuleEntry{Rules: "Latency$", Signals: []string{"payments"}})
	o.Signals = o.Signals.Index(map[string]map[string]string{"CheckoutLatency": {}, "ApiErrors": {}})
	fs := []firing.Firing{
		fire("ApiErrors", 10, "service", "search", "team", "find"),      // errors alone: judged
		fire("ApiErrors", 11, "service", "checkout", "team", "shop"),    // and probe, which starts at 72h
		fire("ApiErrors", 80, "service", "checkout", "team", "shop"),    // after it starts
		fire("CheckoutLatency", 12, "service", "checkout", "team", "x"), // payments alone, which keeps none
		fire("CheckoutLatency", 50, "service", "search", "team", "x"),   // after an errors window: not its
		fire("CheckoutLatency", 110, "service", "checkout", "team", "x"),
	}
	incs := []incident.Incident{
		window("errors", nil, 45, 45.5, incident.Labels{"service": {"search"}}),
		window("payments", []string{}, 100, 100.5, nil),
	}
	rep := Judge(nil, fs, incs, o)
	got := map[string][]bool{}
	for _, j := range rep.Firings {
		got[j.Rule] = append(got[j.Rule], !j.NoRecord)
	}
	if want := []bool{true, false, true}; !slices.Equal(got["ApiErrors"], want) {
		t.Errorf("ApiErrors judged %v, want %v", got["ApiErrors"], want)
	}
	if want := []bool{false, false, true}; !slices.Equal(got["CheckoutLatency"], want) {
		t.Errorf("CheckoutLatency judged %v, want %v", got["CheckoutLatency"], want)
	}
	if !slices.Equal(rep.Firings[1].Signals, []string{"errors", "probe", "payments"}) || !slices.Equal(rep.Firings[3].Signals, []string{"payments"}) {
		t.Errorf("the firings' signals: %v, %v", rep.Firings[1].Signals, rep.Firings[3].Signals)
	}
	if rep.NoRecord || rep.Unjudged != 3 {
		t.Errorf("no record %v, %d unjudged", rep.NoRecord, rep.Unjudged)
	}
	// An incident written down keeps everyone's record.
	written := []incident.Incident{{ID: "INC-1", Start: sigAt(5), End: sigAt(6), Labels: incident.Labels{"service": {"cart"}}}}
	if rep := Judge(nil, fs[3:4], written, o); rep.Unjudged != 0 {
		t.Errorf("after an incident written down: %d unjudged", rep.Unjudged)
	}
	// Judged by week, as serve does: a signal's start by the week's start.
	weekly := o
	weekly.RecordWeek = func(t time.Time) (time.Time, time.Time) {
		w := sigT0.Add(t.Sub(sigT0).Truncate(7 * 24 * time.Hour))
		return w, w.Add(7 * 24 * time.Hour)
	}
	rec := RecordOf(nil, weekly)
	if rec.JudgesTied(sigAt(80), []int{1}) {
		t.Error("by week, probe hadn't started by the week's start")
	}
	if !rec.JudgesTied(sigAt(80+7*24), []int{1}) {
		t.Error("by week, the next week is judged")
	}
}

// With named signals, a period whose signals that keep the record never
// started, and no incident near it, has no record; one that starts within
// it has.
func TestNoRecordBySignal(t *testing.T) {
	o := signalOpts()
	for i := range o.Signals.List {
		o.Signals.List[i].Start = time.Time{}
	}
	fs := []firing.Firing{fire("ApiErrors", 10, "service", "search", "team", "find")}
	if rep := Judge(nil, fs, nil, o); !rep.NoRecord || rep.Unjudged != 1 {
		t.Errorf("never started: no record %v, %d unjudged", rep.NoRecord, rep.Unjudged)
	}
	// payments read all along keeps no record: still none.
	o.Signals.List[2].Start = sigT0.Add(-14 * 24 * time.Hour)
	if rep := Judge(nil, fs, nil, o); !rep.NoRecord {
		t.Error("a signal that doesn't keep the record, started: the period has a record")
	}
	o.Signals.List[1].Start = sigAt(150)
	if rep := Judge(nil, fs, nil, o); rep.NoRecord || rep.Unjudged != 1 {
		t.Errorf("probe starts in the period: no record %v, %d unjudged", rep.NoRecord, rep.Unjudged)
	}
}

// The report says what judged it: each signal's name, for, match (null for
// the match in effect, [] for none), record and start, and the entries; a
// rule its entry, a firing its signals, an incident its signal. With no
// named signal, none of it.
func TestSignalSettings(t *testing.T) {
	o := signalOpts(RuleEntry{Rules: "^BackendMissing$", Signals: []string{"errors"}, Match: []string{"backend"}})
	o.Signals = o.Signals.Index(map[string]map[string]string{"BackendMissing": {}})
	rep := Judge(nil, []firing.Firing{fire("BackendMissing", 10, "backend", "b")}, []incident.Incident{window("errors", nil, 10, 10.5, nil)}, o)
	var b bytes.Buffer
	if err := JSON(&b, rep); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"impact_signals": [`, `"name": "probe",`, `"for_ns": 120000000000,`, `"match": [
          "service"
        ],`, `"match": null,`, `"match": [],`, `"record": false`, `"start": "2026-10-08T00:00:00Z"`,
		`"impact_rules": [`, `"rules": "^BackendMissing$",`, `"impact_rule": 1`, `"signals": [
        "errors"
      ]`, `"signal": "errors"`} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("the report has no %s", want)
		}
	}
	plain := o
	plain.Signals = nil
	b.Reset()
	JSON(&b, Judge(nil, []firing.Firing{fire("BackendMissing", 10, "backend", "b")}, nil, plain))
	for _, no := range []string{"impact_signals", "impact_rules", "impact_rule", `"signals"`, `"signal"`} {
		if strings.Contains(b.String(), no) {
			t.Errorf("no named signal, and the report has %s", no)
		}
	}
	var back Report
	if err := json.Unmarshal(b.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
}
