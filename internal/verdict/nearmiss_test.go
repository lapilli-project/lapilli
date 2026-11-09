package verdict

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
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
			{Incident: "INC-1", Rule: "Value", From: 0, For: 5 * time.Minute, NewFor: 5 * time.Minute, Delay: 5 * time.Minute,
				Value: &ByValue{Op: ">", Bound: 0.9, Reached: new(0.88), New: 0.87}},
			{Incident: "INC-1", Rule: "Guarded", For: 5 * time.Minute, NewFor: 5 * time.Minute, Delay: 5 * time.Minute,
				Value: &ByValue{Op: ">", Bound: 10, Of: "rate(requests[5m])", New: 9}},
			{Incident: "INC-1", Rule: "Excused", For: 5 * time.Minute, NewFor: 5 * time.Minute, Delay: 5 * time.Minute,
				Value: &ByValue{Op: "<", Bound: 10, Of: "requests", Unless: true, New: 9}},
			{Incident: "INC-1", Rule: "Escaped", For: 5 * time.Minute, NewFor: 5 * time.Minute, Delay: 5 * time.Minute,
				Value: &ByValue{Op: "<", Bound: 10, Of: "up{job=\"\x1b]8;;x\"}", New: 9}},
		},
		NearMissesCut: true}
	var b bytes.Buffer
	if err := Text(&b, r, time.UTC); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Short    3m from +2m", "5m → 2m", "Late     from +0s, fired +30m", "0 → 3", "Value    reached 0.88", "> 0.9 → 0.87", "for 5m → 2m",
		"Guarded  –", "rate(requests[5m]) > 10 → 9", "unless requests < 10 → 9", `up{job="\u{1b}]8;;x"} < 10 → 9`, "Not every incident was looked at for rules that nearly caught it"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("lacks %q:\n%s", want, b.String())
		}
	}
	if strings.Contains(b.String(), "\x1b") {
		t.Errorf("an escape, as it is:\n%q", b.String())
	}
}

func TestNearlyBy(t *testing.T) {
	o := opts()
	in := incident.Incident{ID: "INC-1", Start: m(100), End: m(130), Labels: incident.Labels{"service": {"checkout"}}}
	// values is a series evaluated each minute from m(0) to m(200), v(i)
	// at minute i.
	values := func(service string, v func(i int) float64) []prom.Series {
		s := prom.Series{Labels: map[string]string{"service": service, "__name__": "latency"}}
		for i := 0; i <= 200; i++ {
			s.Samples = append(s.Samples, prom.Sample{T: m(i), V: v(i)})
		}
		return []prom.Series{s}
	}
	during := func(lo, hi int, at, else_ float64) func(int) float64 {
		return func(i int) float64 {
			if i >= lo && i < hi {
				return at
			}
			return else_
		}
	}
	above, below := rules.Bound{Op: ">", Value: 0.9}, rules.Bound{Op: "<", Value: 100}
	for _, c := range []struct {
		name    string
		series  []prom.Series
		b       rules.Bound
		for_    time.Duration
		ok      bool
		new     float64
		reached float64
		delay   time.Duration
	}{
		// At 0.88 from the start for 10m: past 0.87 for the 5m "for", it
		// fires at +5m.
		{"close", values("checkout", during(100, 110, 0.88, 0.1)), above, 5 * time.Minute, true, 0.87, 0.88, 5 * time.Minute},
		// Exactly on a round number: one short of it.
		{"on a round number", values("checkout", during(100, 110, 0.87, 0.1)), above, 5 * time.Minute, true, 0.86, 0.87, 5 * time.Minute},
		// Close, but not for the "for": the least of each 6 evaluations.
		{"for the for", values("checkout", func(i int) float64 {
			switch {
			case i >= 100 && i < 103:
				return 0.89
			case i >= 103 && i < 110:
				return 0.8
			}
			return 0.1
		}), above, 5 * time.Minute, true, 0.79, 0.8, 5 * time.Minute},
		// Too far: 0.5 is more than a fifth short of 0.9.
		{"too far", values("checkout", during(100, 110, 0.5, 0.1)), above, 5 * time.Minute, false, 0, 0, 0},
		// Another service's value doesn't catch it.
		{"other service", values("search", during(100, 110, 0.88, 0.1)), above, 5 * time.Minute, false, 0, 0, 0},
		// Close only well before the incident.
		{"before", values("checkout", during(20, 40, 0.88, 0.1)), above, 5 * time.Minute, false, 0, 0, 0},
		// Close from long before: with 0.87 it fires then and is firing
		// already when the incident starts.
		{"on already", values("checkout", during(20, 140, 0.88, 0.1)), above, 5 * time.Minute, false, 0, 0, 0},
		// Below: at 104, short of 100, it fires at once with 110.
		{"below", values("checkout", during(110, 120, 104, 300)), below, 0, true, 110, 104, 10 * time.Minute},
		// No "for": a single evaluation is enough.
		{"one evaluation", values("checkout", during(120, 121, 0.75, 0.1)), above, 0, true, 0.74, 0.75, 20 * time.Minute},
		// A value that isn't a number breaks the stretch.
		{"not a number", values("checkout", func(i int) float64 {
			switch {
			case i == 103:
				return math.NaN()
			case i >= 100 && i < 107:
				return 0.88
			}
			return 0.1
		}), above, 5 * time.Minute, false, 0, 0, 0},
	} {
		nm, ok := NearlyBy(in, c.series, rules.Rule{Name: "R", For: c.for_}, c.b, time.Time{}, time.Minute, o)
		if ok != c.ok || ok && (nm.Value == nil || nm.Value.New != c.new || nm.Value.Reached == nil || *nm.Value.Reached != c.reached || nm.Delay != c.delay || nm.NewFor != c.for_) {
			t.Errorf("%s: %v %+v %+v, want %v, %v at %s", c.name, ok, nm, nm.Value, c.ok, c.new, c.delay)
		}
		if ok && nm.Labels["__name__"] != "" {
			t.Errorf("%s: labels %v", c.name, nm.Labels)
		}
	}
	// Held through a dip by keep_firing_for: with 0.87 it fires at m75 and
	// stays on into the incident's lead, so it catches nothing.
	dip := values("checkout", func(i int) float64 {
		switch {
		case i >= 70 && i <= 80, i >= 83 && i <= 110:
			return 0.88
		}
		return 0.1
	})
	if nm, ok := NearlyBy(in, dip, rules.Rule{For: 5 * time.Minute, KeepFiringFor: 10 * time.Minute}, above, time.Time{}, time.Minute, o); ok {
		t.Errorf("kept firing through the dip: %+v", nm)
	}
	if nm, ok := NearlyBy(in, dip, rules.Rule{For: 5 * time.Minute}, above, time.Time{}, time.Minute, o); !ok || nm.Delay != -12*time.Minute {
		t.Errorf("without keep_firing_for: %v %+v", ok, nm)
	}
	// No "for" below nothing, no number that isn't one.
	close := values("checkout", during(100, 110, 0.88, 0.1))
	if _, ok := NearlyBy(in, close, rules.Rule{For: -5 * time.Minute}, above, time.Time{}, time.Minute, o); ok {
		t.Error("a negative for")
	}
	if _, ok := NearlyBy(in, close, rules.Rule{}, rules.Bound{Op: ">", Value: math.NaN()}, time.Time{}, time.Minute, o); ok {
		t.Error("a bound that isn't a number")
	}
	// Exactly a fifth short counts.
	if nm, ok := NearlyBy(in, values("checkout", during(100, 110, 0.725, 0.1)), rules.Rule{}, above, time.Time{}, time.Minute, o); !ok || nm.Value.New != 0.72 {
		t.Errorf("a fifth short: %v %+v", ok, nm.Value)
	}
	// The rule's own labels decide too.
	if _, ok := NearlyBy(in, values("checkout", during(100, 110, 0.88, 0.1)), rules.Rule{Labels: map[string]string{"service": "search"}}, above, time.Time{}, time.Minute, o); ok {
		t.Error("the rule's own labels disagree")
	}
	// Of two label sets, the one changing the number least.
	two := append(values("checkout", during(100, 110, 0.8, 0.1)), values("checkout", during(100, 110, 0.85, 0.1))...)
	two[1].Labels = map[string]string{"service": "checkout", "pod": "b"}
	if nm, ok := NearlyBy(in, two, rules.Rule{}, above, time.Time{}, time.Minute, o); !ok || nm.Value.New != 0.84 || nm.Labels["pod"] != "b" {
		t.Errorf("two: %v %+v", ok, nm)
	}

	// A range: within 10 and 20, its lower end. At 8.5 for 10m, with 8 it
	// fires at +5m; at 8.5 but past 20 for a minute of it, the upper end
	// breaks the stretch.
	within := rules.Bound{Op: ">", Value: 10, Rest: &rules.Bound{Op: "<", Value: 20}}
	r := rules.Rule{Name: "R", For: 5 * time.Minute}
	if nm, ok := NearlyBy(in, values("checkout", during(100, 110, 8.5, 0)), r, within, time.Time{}, time.Minute, o); !ok || nm.Value.New != 8 || *nm.Value.Reached != 8.5 || nm.Delay != 5*time.Minute {
		t.Errorf("within: %v %+v", ok, nm.Value)
	}
	broken := values("checkout", func(i int) float64 {
		switch {
		case i == 103:
			return 25
		case i >= 100 && i < 108:
			return 8.5
		}
		return 0
	})
	if nm, ok := NearlyBy(in, broken, r, within, time.Time{}, time.Minute, o); ok {
		t.Errorf("within, broken by the upper end: %+v", nm.Value)
	}
	// Outside 10 and 20, its lower end: the values past 20 hold whatever it
	// is, so it is the 11s that need it at 12.
	outside := rules.Bound{Op: "<", Value: 10, Rest: &rules.Bound{Op: ">", Value: 20}, Either: true}
	swing := values("checkout", func(i int) float64 {
		switch {
		case i >= 100 && i < 110 && i%2 == 0:
			return 25
		case i >= 100 && i < 110:
			return 11
		}
		return 15
	})
	if nm, ok := NearlyBy(in, swing, r, outside, time.Time{}, time.Minute, o); !ok || nm.Value.New != 12 || *nm.Value.Reached != 11 {
		t.Errorf("outside: %v %+v", ok, nm.Value)
	}
	// Past 20 all the while, it held: no near miss.
	if nm, ok := NearlyBy(in, values("checkout", during(80, 140, 25, 15)), r, outside, time.Time{}, time.Minute, o); ok {
		t.Errorf("outside, held: %+v", nm.Value)
	}
}

func TestNearlyAt(t *testing.T) {
	o := opts()
	in := incident.Incident{ID: "INC-1", Start: m(100), End: m(130), Labels: incident.Labels{"service": {"checkout"}}}
	b := rules.Bound{Op: ">", Value: 10, Of: "requests", At: 1}
	r := rules.Rule{Name: "R", For: 5 * time.Minute}
	l := map[string]string{"service": "checkout"}
	fired := []firing.Firing{
		{Rule: "R", Labels: l, Start: m(20), End: m(30)},   // before the incident
		{Rule: "R", Labels: l, Start: m(112), End: m(120)}, // in it
		{Rule: "R", Labels: l, Start: m(105), End: m(108)}, // in it, sooner
	}
	nm, ok := NearlyAt(in, fired, r, b, 9, time.Minute, o)
	if !ok || nm.Delay != 5*time.Minute || nm.From != 0 || nm.Value.New != 9 || nm.Value.Of != "requests" || nm.Value.Reached != nil || nm.NewFor != r.For {
		t.Errorf("%v %+v %+v", ok, nm, nm.Value)
	}
	if _, ok := NearlyAt(in, fired[:1], r, b, 9, time.Minute, o); ok {
		t.Error("before the incident")
	}
}

func TestRound(t *testing.T) {
	for _, c := range []struct {
		v    float64
		b    rules.Bound
		want float64
	}{
		{0.873, rules.Bound{Op: ">", Value: 0.9}, 0.87},
		{0.87, rules.Bound{Op: ">=", Value: 0.9}, 0.87},
		{487, rules.Bound{Op: ">", Value: 500}, 480},
		{0.95, rules.Bound{Op: ">", Value: 1}, 0.9},
		{104, rules.Bound{Op: "<", Value: 100}, 110},
		{104, rules.Bound{Op: "<", Value: 99}, 105},
		{110, rules.Bound{Op: "<=", Value: 100}, 110},
		{-10.5, rules.Bound{Op: ">", Value: -10}, -11},
		{0.0123, rules.Bound{Op: ">", Value: 0.015}, 0.012},
		{0.29, rules.Bound{Op: ">=", Value: 0.3}, 0.29},
		{0.29, rules.Bound{Op: ">", Value: 0.3}, 0.28},
		{0.57, rules.Bound{Op: "<=", Value: 0.5}, 0.57},
	} {
		if got := rules.Round(c.v, c.b); got != c.want {
			t.Errorf("Round(%v, %v) = %v, want %v", c.v, c.b, got, c.want)
		}
	}
}
