package verdict

import (
	"slices"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/rules"
)

// With named signals, a firing comes after an incident it could have
// caught (Catches, by Agrees), and no other: a rule entry that names only
// signal a keeps its rule from signal b's windows, so a firing of it is
// neither white within b's window nor late after it; without the entry,
// the same firings catch b's window, and come after it, as the design
// lets a rule with no entry catch any signal's window.
func TestFiredAfterBySignals(t *testing.T) {
	api := map[string]string{"service": "api"}
	known := []rules.Rule{{Name: "LB5xx", Expr: "x", For: 3 * time.Minute, Late: 3 * time.Minute}}
	within := firing.Firing{Rule: "LB5xx", Labels: api, Start: m(104), End: m(105)} // in b's window
	after := firing.Firing{Rule: "LB5xx", Labels: api, Start: m(109), End: m(115)}  // held from 103, in it: after its end
	incs := []incident.Incident{{ID: "impact-b-1", Signal: "b", Start: m(100), End: m(106), Labels: incident.Labels{"service": {"api"}}, Candidate: true}}
	for _, c := range []struct {
		name    string
		entries []RuleEntry
		catches bool
	}{{"an entry naming a alone", []RuleEntry{{Rules: "^LB5xx$", Signals: []string{"a"}}}, false}, {"no entry", nil, true}} {
		t.Run(c.name, func(t *testing.T) {
			o := opts()
			o.Signals = NewSignals([]Signal{{Name: "a", Record: true}, {Name: "b", Record: true, Bit: 1}}, c.entries)
			r := Judge(known, []firing.Firing{within}, incs, o)
			if j := r.Firings[0]; j.White != c.catches || Catches(within, incs[0], o) != c.catches {
				t.Errorf("within b's window: white %v; want %v", j.White, c.catches)
			}
			r = Judge(known, []firing.Firing{after}, incs, o)
			j := r.Firings[0]
			if late := slices.Equal(j.After, []string{"impact-b-1"}); late != c.catches || (r.Rules[0].Late == 1) != c.catches || (len(r.Incidents[0].FiredAfter) == 1) != c.catches {
				t.Errorf("after b's window: after %v, rule late %d, fired after %v; want late %v, as it would catch it",
					j.After, r.Rules[0].Late, r.Incidents[0].FiredAfter, c.catches)
			}
		})
	}
}
