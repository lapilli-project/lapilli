package ledger

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// lateSays is what a report says of the firings that came after an
// incident was over: each rule's late, each incident's fired_after, each
// firing's after and held_from.
func lateSays(rep *verdict.Report) (rs, ins, fs []string) {
	for _, r := range rep.Rules {
		if r.Late > 0 {
			rs = append(rs, fmt.Sprintf("%s %d", r.Rule, r.Late))
		}
	}
	for _, in := range rep.Incidents {
		for _, c := range in.FiredAfter {
			ins = append(ins, fmt.Sprintf("%s %s %s", in.ID, c.Rule, c.Delay))
		}
	}
	for _, j := range rep.Firings {
		// In order of the incidents' start: of two that started at once,
		// either first.
		fs = append(fs, fmt.Sprintf("%s %v %s %v held %s", j.Rule, j.Labels, j.Start, slices.Sorted(slices.Values(j.After)), j.Held))
	}
	slices.Sort(rs)
	slices.Sort(ins)
	slices.Sort(fs)
	return rs, ins, fs
}

// The ledger says a firing came after an incident was over as
// verdict.Judge says it of the same firings and incidents (docs/
// how-it-judges.md, "An alert after the incident"): with how long before
// its start each rule's condition held given (verdict.Options.Held), as
// Judge works it out from the rules, the same rules' late, incidents'
// fired_after, firings' after and held_from.
func TestLateSameAsJudge(t *testing.T) {
	ctx := context.Background()
	late := 0
	for seed := uint64(1); seed <= 5; seed++ {
		l := open(t, time.UTC)
		fs, incs := world(seed, 600)
		incs = append([]incident.Incident{keepAll}, incs...)
		if _, err := l.SyncFile(ctx, incs, opts); err != nil {
			t.Fatal(err)
		}
		record(t, l, t0, t0.Add(7*24*time.Hour), fs)
		// Each rule's for, and one with none, held only once it fired.
		var known []rules.Rule
		for i := range 12 {
			known = append(known, rules.Rule{Name: fmt.Sprintf("Rule%d", i), For: time.Duration(i%4) * 10 * time.Minute})
		}
		o := opts
		o.From, o.To = t0, t0.Add(7*24*time.Hour)
		want := verdict.Judge(known, slices.Clone(fs), incs, o)
		o.Held = verdict.HeldFor(known)
		got, err := l.Report(ctx, o.From, o.To, o, true)
		if err != nil {
			t.Fatal(err)
		}
		gr, gi, gf := lateSays(got)
		wr, wi, wf := lateSays(want)
		if !slices.Equal(gr, wr) {
			t.Errorf("seed %d rules' late\n got %q\nwant %q", seed, gr, wr)
		}
		if !slices.Equal(gi, wi) {
			t.Errorf("seed %d incidents' fired_after\n got %q\nwant %q", seed, gi, wi)
		}
		if !slices.Equal(gf, wf) {
			t.Errorf("seed %d firings' after and held_from differ", seed)
		}
		late += len(wr)
		// The firings ReportFor gives, one at a time, say the same.
		js, _, err := l.ReportFirings(ctx, o.From, o.To, o, nil, nil, len(fs))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, f := lateSays(&verdict.Report{Firings: js}); !slices.Equal(f, gf) {
			t.Errorf("seed %d: ReportFirings differs from ReportFor", seed)
		}
	}
	if late == 0 {
		t.Fatal("no firing came after an incident: the test says nothing")
	}
}

// What comes after an incident is over, and what doesn't: a black firing
// of a rule that didn't catch it, its condition held from its end or
// before, its labels agreeing. Not one of a rule that caught it, one whose
// condition held only once it was over, one about another service, nor
// one of a rule judged by nothing. A firing in the span after an incident
// before it counts for its rule; one past the span counts for none, and
// an incident is named only by the span it started in.
func TestLateOf(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	at := t0.Add(24 * time.Hour)
	in := incident.Incident{ID: "INC-A", Start: at, End: at.Add(30 * time.Minute), Labels: incident.Labels{"service": {"checkout"}}}
	before := incident.Incident{ID: "INC-B", Start: at.Add(-3 * time.Hour), End: at.Add(-2*time.Hour - 50*time.Minute), Labels: incident.Labels{"service": {"checkout"}}}
	other := incident.Incident{ID: "INC-C", Start: at.Add(2 * time.Hour), End: at.Add(2*time.Hour + 5*time.Minute), Labels: incident.Labels{"service": {"cart"}}}
	if _, err := l.SyncFile(ctx, []incident.Incident{keepAll, in, before, other}, opts); err != nil {
		t.Fatal(err)
	}
	checkout := map[string]string{"service": "checkout"}
	end := in.End
	f := func(rule string, start time.Time, labels map[string]string) firing.Firing {
		return firing.Firing{Rule: rule, Labels: labels, Start: start, End: start.Add(5 * time.Minute)}
	}
	record(t, l, t0, t0.Add(7*24*time.Hour), []firing.Firing{
		f("Late", end.Add(5*time.Minute), checkout),                                 // after INC-A, held from 5m before its end
		f("Late", before.End.Add(2*time.Minute), checkout),                          // after INC-B, before the span
		f("Caught", at.Add(10*time.Minute), checkout),                               // catches INC-A
		f("Caught", end.Add(time.Minute), checkout),                                 // after it, but its rule caught it
		f("Short", end.Add(time.Minute), checkout),                                  // held for a minute, from its end: after it
		f("Short", end.Add(3*time.Minute), checkout),                                // held from 2m after its end: after nothing
		f("Other", end.Add(5*time.Minute), map[string]string{"service": "cart"}),    // about the cart
		f("Nobody", end.Add(5*time.Minute), checkout),                               // judged by nothing
		f("Late", other.End.Add(time.Minute), map[string]string{"service": "cart"}), // after INC-C, past the span
	})
	o := opts
	o.Held = map[string]time.Duration{"Late": 10 * time.Minute, "Caught": 10 * time.Minute, "Short": time.Minute, "Other": 10 * time.Minute, "Nobody": 10 * time.Minute}
	o.Signals = verdict.NewSignals(nil, []verdict.RuleEntry{{Rules: "^Nobody$", Judged: no()}})
	if !o.NotJudged("Nobody") {
		t.Fatal("Nobody is judged")
	}
	from, to := at.Add(-time.Hour), at.Add(2*time.Hour)
	got, err := l.LateOf(ctx, from, to, o, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{"Late": 1, "Short": 1}; !mapsEqual(got.Rules, want) {
		t.Errorf("rules: %v, want %v", got.Rules, want)
	}
	fired := map[string][]string{}
	for id, cs := range got.Fired {
		for _, c := range cs {
			fired[id] = append(fired[id], fmt.Sprintf("%s %s", c.Rule, c.Delay))
		}
	}
	if len(fired) != 1 || !slices.Equal(fired["INC-A"], []string{"Short 31m0s", "Late 35m0s"}) {
		t.Errorf("fired after: %v", fired)
	}
	afters := 0
	for _, ids := range got.After {
		if !slices.Equal(ids, []string{"INC-A"}) {
			t.Errorf("after %v", ids)
		}
		afters++
	}
	if afters != 2 || got.Cut {
		t.Errorf("%d firings after INC-A, cut %v", afters, got.Cut)
	}
	// INC-C, past the span, is named by its own span; INC-B's firing is
	// counted in a span that holds it.
	if x, err := l.LateOf(ctx, other.Start, other.End.Add(time.Hour), o, false); err != nil || !slices.Equal(catchRules(x.Fired["INC-C"]), []string{"Late"}) || x.Rules["Late"] != 1 || x.After != nil {
		t.Errorf("INC-C: %+v %v", x, err)
	}
	if x, err := l.LateOf(ctx, before.Start, before.End.Add(time.Hour), o, false); err != nil || x.Rules["Late"] != 1 {
		t.Errorf("INC-B: %+v %v", x, err)
	}
	// With no rule's condition known to hold before it fired, none.
	if x, err := l.LateOf(ctx, from, to, opts, true); err != nil || len(x.Rules)+len(x.Fired)+len(x.After) != 0 {
		t.Errorf("no held: %+v %v", x, err)
	}
	// Past what one read looks at, firings against incidents, firings read
	// or reads: cut, and what was found by then given.
	for _, bound := range []*int{&lateChecks, &lateRows, &lateSpans} {
		was := *bound
		*bound = 1
		x, err := l.LateOf(ctx, t0, t0.Add(7*24*time.Hour), o, true)
		*bound = was
		if err != nil || !x.Cut || x.Rules["Late"] > 1 || x.Rules["Short"] > 1 {
			t.Errorf("cut: %+v %v", x, err)
		}
	}
	if x, err := l.LateOf(ctx, t0, t0.Add(7*24*time.Hour), o, true); err != nil || x.Cut || x.Rules["Late"] != 3 {
		t.Errorf("the week, not cut: %+v %v", x, err)
	}
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// catchRules are the rules of catches.
func catchRules(cs []verdict.Catch) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Rule)
	}
	return out
}
