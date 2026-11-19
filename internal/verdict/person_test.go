package verdict

import (
	"slices"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/rules"
)

// personOpts judge a period of three days around ept's day, on service.
func personOpts(vs ...Verdict) Options {
	o := episodeOpts
	o.From, o.To, o.Verdicts = ept(0, 0).Add(-24*time.Hour), ept(0, 0).Add(3*24*time.Hour), vs
	return o
}

func svcPod(s, pod string) map[string]string { return map[string]string{"service": s, "pod": pod} }

func incOn(id, svc string, start, end time.Time) incident.Incident {
	return incident.Incident{ID: id, Start: start, End: end, Labels: incident.Labels{"service": {svc}}}
}

// firingOf is the report's firing of rule that started at start.
func firingOf(t *testing.T, rep *Report, rule string, start time.Time) Judged {
	t.Helper()
	for _, j := range rep.Firings {
		if j.Rule == rule && j.Start.Equal(start) {
			return j
		}
	}
	t.Fatalf("no firing of %s at %s", rule, start)
	return Judged{}
}

func ruleOf(rep *Report, rule string) RuleResult {
	for _, r := range rep.Rules {
		if r.Rule == rule {
			return r
		}
	}
	return RuleResult{}
}

// A firing a person said is unrelated to what it caught is black, and the
// incident is caught by the other firings alone, or missed; one it catches
// and wasn't said to be unrelated to keeps it white.
func TestUnrelatedTurnsOneFiringBlack(t *testing.T) {
	disk := fired("Disk", svcPod("checkout", "db"), ept(14, 5), ept(14, 20))
	errs := fired("Errors", svcPod("checkout", "a"), ept(14, 12), ept(14, 30))
	inc := incOn("INC-12", "checkout", ept(14, 10), ept(14, 40))
	said := Verdict{Rule: "Disk", Labels: disk.Labels, Start: disk.Start, End: disk.End, Unrelated: []string{"INC-12"}, Action: ActionNo, At: ept(15, 0)}

	// A week before, of another service: it keeps the record.
	kept := incOn("INC-0", "cart", ept(0, 0).Add(-7*24*time.Hour), ept(1, 0).Add(-7*24*time.Hour))
	rep := Judge(nil, []firing.Firing{disk, errs}, []incident.Incident{kept, inc}, personOpts(said))
	d := firingOf(t, rep, "Disk", disk.Start)
	if d.White || !slices.Equal(d.Unrelated, []string{"INC-12"}) || len(d.Incidents) != 0 || d.Action != ActionNo || d.NoRecord {
		t.Errorf("the disk's firing: %+v", d)
	}
	if r := ruleOf(rep, "Disk"); r.White != 0 || r.Black != 1 || r.Unrelated != 1 || r.ActionNo != 1 || r.Incidents != 0 {
		t.Errorf("the disk's rule: %+v", r)
	}
	if c := rep.Incidents[0].CaughtBy; len(c) != 1 || c[0].Rule != "Errors" {
		t.Errorf("INC-12 caught by %+v, want Errors alone", c)
	}
	// Errors said unrelated too: the incident is missed.
	rep = Judge(nil, []firing.Firing{disk, errs}, []incident.Incident{inc}, personOpts(said,
		Verdict{Rule: "Errors", Labels: errs.Labels, Start: errs.Start, End: errs.End, Unrelated: []string{"INC-12"}, At: ept(15, 0)}))
	if !rep.Incidents[0].Missed() {
		t.Errorf("INC-12 should be missed: %+v", rep.Incidents[0].CaughtBy)
	}
	// Another incident it catches, which it wasn't said to be unrelated
	// to: white by that one.
	other := incOn("INC-15", "checkout", ept(14, 0), ept(14, 50))
	rep = Judge(nil, []firing.Firing{disk}, []incident.Incident{inc, other}, personOpts(said))
	d = firingOf(t, rep, "Disk", disk.Start)
	if !d.White || !slices.Equal(d.Incidents, []string{"INC-15"}) || !slices.Equal(d.Unrelated, []string{"INC-12"}) {
		t.Errorf("the disk's firing with INC-15 too: %+v", d)
	}
	if r := ruleOf(rep, "Disk"); r.Unrelated != 0 || r.White != 1 {
		t.Errorf("a firing white by another incident isn't counted unrelated: %+v", r)
	}
}

// A replay finds each verdict's firing again by the rule of a move: its
// own alert's that starts at its start; one on at it; the first that
// starts within the span it was said of; another alert's of the same
// match values after its own alert's; and none past its span.
func TestVerdictsFoundAgain(t *testing.T) {
	inc := incOn("INC-1", "checkout", ept(14, 0), ept(15, 0))
	said := Verdict{Rule: "Disk", Labels: svcPod("checkout", "a"), Start: ept(14, 5), End: ept(14, 20), Unrelated: []string{"INC-1"}, At: ept(16, 0)}
	for _, c := range []struct {
		name  string
		fs    []firing.Firing
		black time.Time // the firing the verdict should keep black; zero for none
	}{
		{"the same", []firing.Firing{fired("Disk", svcPod("checkout", "a"), ept(14, 5), ept(14, 20))}, ept(14, 5)},
		{"a step later: within its span", []firing.Firing{fired("Disk", svcPod("checkout", "a"), ept(14, 7), ept(14, 20))}, ept(14, 7)},
		{"a longer for's: within its span", []firing.Firing{fired("Disk", svcPod("checkout", "a"), ept(14, 15), ept(14, 20))}, ept(14, 15)},
		{"a shorter for's: on at its start", []firing.Firing{fired("Disk", svcPod("checkout", "a"), ept(14, 1), ept(14, 20))}, ept(14, 1)},
		{"another pod's, its own gone", []firing.Firing{fired("Disk", svcPod("checkout", "b"), ept(14, 6), ept(14, 20))}, ept(14, 6)},
		{"past its span: none", []firing.Firing{fired("Disk", svcPod("checkout", "a"), ept(14, 21), ept(14, 40))}, time.Time{}},
		{"another service's: none", []firing.Firing{fired("Disk", svcPod("cart", "a"), ept(14, 5), ept(14, 20))}, time.Time{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ins := []incident.Incident{inc, incOn("INC-2", "cart", ept(14, 0), ept(15, 0))}
			rep := Judge(nil, c.fs, ins, personOpts(said))
			for _, j := range rep.Firings {
				if want := j.Start.Equal(c.black); j.White == want {
					t.Errorf("%s at %s: white %v", j.Rule, j.Start.Format("15:04"), j.White)
				}
			}
		})
	}
	// Its own alert's before another's: a pod's verdict stays on that pod,
	// whichever started first.
	fs := []firing.Firing{fired("Disk", svcPod("checkout", "b"), ept(14, 2), ept(14, 30)), fired("Disk", svcPod("checkout", "a"), ept(14, 9), ept(14, 30))}
	rep := Judge(nil, fs, []incident.Incident{inc}, personOpts(said))
	if a, b := firingOf(t, rep, "Disk", ept(14, 9)), firingOf(t, rep, "Disk", ept(14, 2)); a.White || !b.White {
		t.Errorf("pod a's firing should be black, pod b's white: %v %v", a.White, b.White)
	}
	// Said while its firing was on: it reaches its rule's for and --refire,
	// no further.
	on := said
	on.End = time.Time{}
	known := []rules.Rule{{Name: "Disk", For: 5 * time.Minute}}
	o := personOpts(on)
	o.RefireWithin = 10 * time.Minute
	for _, c := range []struct {
		at    time.Time
		black bool
	}{{ept(14, 20), true}, {ept(14, 21), false}} {
		rep := Judge(known, []firing.Firing{fired("Disk", svcPod("checkout", "a"), c.at, ept(14, 50))}, []incident.Incident{inc}, o)
		if j := firingOf(t, rep, "Disk", c.at); j.White == c.black {
			t.Errorf("said while on, a firing at %s: white %v", c.at.Format("15:04"), j.White)
		}
	}
}

// A replay that merges two firings, each said unrelated to an incident of
// its own, keeps both incidents unrelated, whichever verdict holds it; the
// one said of that very alert holds its answer, else the latest said.
func TestVerdictsMerged(t *testing.T) {
	a, b := svcPod("checkout", "a"), svcPod("checkout", "b")
	ins := []incident.Incident{incOn("INC-1", "checkout", ept(14, 0), ept(14, 30)), incOn("INC-2", "checkout", ept(14, 10), ept(14, 40))}
	va := Verdict{Rule: "Disk", Labels: a, Start: ept(14, 5), End: ept(14, 8), Unrelated: []string{"INC-1"}, Action: ActionNo, At: ept(16, 0)}
	vb := Verdict{Rule: "Disk", Labels: a, Start: ept(14, 12), End: ept(14, 20), Unrelated: []string{"INC-2"}, Action: ActionYes, At: ept(17, 0)}
	merged := fired("Disk", a, ept(14, 5), ept(14, 20))
	rep := Judge(nil, []firing.Firing{merged}, ins, personOpts(va, vb))
	j := firingOf(t, rep, "Disk", merged.Start)
	if j.White || !slices.Equal(j.Unrelated, []string{"INC-1", "INC-2"}) || j.Action != ActionNo {
		t.Errorf("merged: %+v; want black, unrelated to both, the answer of the verdict of its start", j)
	}
	// Of other alerts: the latest said holds it.
	vb.Labels, vb.Start, va.Labels = b, merged.Start, b
	vb.End, va.End = ept(14, 20), ept(14, 20)
	at, on := Place([]Verdict{va, vb}, []firing.Firing{merged}, []string{"service"}, nil)
	if at[0] != 0 || at[1] != 0 || on[0].Action != ActionYes || on[0].Holds != 1 || !slices.Equal(on[0].Others, []int{0}) {
		t.Errorf("of other alerts: %v %+v", at, on[0])
	}
}

// An entry that says judged: false judges its rules by nothing: never
// white, never black, in no precision, never noisy, and catching nothing,
// written incidents included; and needs no named signal.
func TestJudgedFalse(t *testing.T) {
	no := false
	o := personOpts()
	o.Signals = NewSignals(nil, []RuleEntry{{Rules: "^Watchdog$", Judged: &no}})
	var fs []firing.Firing
	for h := range 12 {
		fs = append(fs, fired("Watchdog", svcPod("checkout", "a"), ept(h, 0), ept(h, 30)))
	}
	fs = append(fs, fired("Errors", svcPod("checkout", "a"), ept(10, 5), ept(10, 20)))
	inc := incOn("INC-1", "checkout", ept(10, 0), ept(10, 40))
	rep := Judge(nil, fs, []incident.Incident{inc}, o)
	w := ruleOf(rep, "Watchdog")
	if w.White != 0 || w.NotJudged != 12 || w.Unjudged != 12 || !w.NoRecord || w.Noisy || w.Incidents != 0 {
		t.Errorf("Watchdog: %+v", w)
	}
	if p, ok := w.Precision(); ok {
		t.Errorf("Watchdog has a precision: %v", p)
	}
	if j := firingOf(t, rep, "Watchdog", ept(10, 0)); j.White || !j.NotJudged || !j.NoRecord {
		t.Errorf("Watchdog's firing during INC-1: %+v", j)
	}
	if c := rep.Incidents[0].CaughtBy; len(c) != 1 || c[0].Rule != "Errors" {
		t.Errorf("INC-1 caught by %+v, want Errors alone", c)
	}
	if e := ruleOf(rep, "Errors"); e.White != 1 || e.NotJudged != 0 {
		t.Errorf("Errors: %+v", e)
	}
	// With no entry saying so, nothing changes.
	o.Signals = NewSignals(nil, []RuleEntry{{Rules: "^Watchdog$", Signals: []string{"probe"}}})
	if w := ruleOf(Judge(nil, fs, []incident.Incident{inc}, o), "Watchdog"); w.NotJudged != 0 || w.White != 1 {
		t.Errorf("Watchdog with an entry naming a signal: %+v", w)
	}
}

// With no verdict and no entry judged by nothing, a report is as it was.
func TestNoVerdictsNoChange(t *testing.T) {
	fs := []firing.Firing{fired("Disk", svcPod("checkout", "a"), ept(14, 5), ept(14, 20)), fired("Disk", svcPod("checkout", "a"), ept(18, 5), ept(18, 20))}
	ins := []incident.Incident{incOn("INC-1", "checkout", ept(14, 0), ept(14, 30))}
	rep := Judge(nil, fs, ins, personOpts())
	for _, j := range rep.Firings {
		if len(j.Unrelated) > 0 || j.Action != "" || j.NotJudged {
			t.Errorf("%+v", j)
		}
	}
	if r := ruleOf(rep, "Disk"); r.White != 1 || r.Black != 1 || r.Unrelated+r.ActionYes+r.ActionNo+r.NotJudged != 0 {
		t.Errorf("%+v", r)
	}
}
