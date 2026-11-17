package verdict

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/rules"
)

// afterScenario is an incident from 100m to 106m, and alerts on it: one
// whose condition held in it but whose "for" and late metrics ran past its
// end, and others that weren't its alert, or that caught it.
func afterScenario() ([]rules.Rule, []firing.Firing, []incident.Incident) {
	api := map[string]string{"service": "api"}
	known := []rules.Rule{
		{Name: "LB5xx", Expr: "x", For: 3 * time.Minute, Late: 3 * time.Minute},
		{Name: "LB5xx", Expr: "x", For: 5 * time.Minute, Late: 3 * time.Minute}, // as it stood at another time: the least is taken
		{Name: "Quick", Expr: "y"},
		{Name: "Caught", Expr: "z", For: 3 * time.Minute},
	}
	fs := []firing.Firing{
		{Rule: "LB5xx", Labels: api, Start: m(109), End: m(115)},                                 // held from 103, in it: after
		{Rule: "LB5xx", Labels: api, Start: m(113), End: m(116)},                                 // held from 107, past its end
		{Rule: "LB5xx", Labels: map[string]string{"service": "web"}, Start: m(108), End: m(110)}, // another service's
		{Rule: "Quick", Labels: api, Start: m(107), End: m(108)},                                 // no "for": held from its start, after the end
		{Rule: "Caught", Labels: api, Start: m(102), End: m(104)},                                // caught it
		{Rule: "Caught", Labels: api, Start: m(108), End: m(109)},                                // held in it, but the rule caught it already
	}
	incs := []incident.Incident{{ID: "I", Start: m(100), End: m(106), Labels: incident.Labels{"service": {"api"}}, Candidate: true}}
	return known, fs, incs
}

// TestFiredAfter: a black firing whose condition held in an incident, by
// its rule's "for" and how late its metrics arrive, and that fired once
// the incident was over, is said to have come after it: of the firing, of
// its rule and of the incident. Its stone stays black, and the incident
// missed by that rule.
func TestFiredAfter(t *testing.T) {
	known, fs, incs := afterScenario()
	only := fs[:1]
	r := Judge(known, slices.Clone(only), incs, opts())
	if in := r.Incidents[0]; !in.Missed() || len(in.FiredAfter) != 1 || in.FiredAfter[0].Rule != "LB5xx" || in.FiredAfter[0].Delay != 9*time.Minute {
		t.Errorf("incident %+v: missed, LB5xx after it, 9m from its start", in)
	}
	if j := r.Firings[0]; j.White || !slices.Equal(j.After, []string{"I"}) || !j.Held.Equal(m(103)) {
		t.Errorf("firing %+v: black, after I, held from 103m (its start less 3m for and 3m late)", j)
	}
	if rr := r.Rules[0]; rr.Black != 1 || rr.White != 0 || rr.Late != 1 {
		t.Errorf("rule %+v: one black firing, late", rr)
	}

	r = Judge(known, slices.Clone(fs[:6]), incs, opts())
	by := map[string]RuleResult{}
	for _, rr := range r.Rules {
		by[rr.Rule] = rr
	}
	if lb := by["LB5xx"]; lb.Firings != 3 || lb.Black != 3 || lb.Late != 1 {
		t.Errorf("LB5xx %+v: three black, one of them late", lb)
	}
	if q := by["Quick"]; q.Late != 0 {
		t.Errorf("Quick %+v: with no \"for\" or late metrics, it held only once the incident was over", q)
	}
	if c := by["Caught"]; c.White != 1 || c.Black != 1 || c.Late != 0 {
		t.Errorf("Caught %+v: a rule that caught the incident didn't come after it", c)
	}
	in := r.Incidents[0]
	if len(in.CaughtBy) != 1 || in.CaughtBy[0].Rule != "Caught" || len(in.FiredAfter) != 1 || in.FiredAfter[0].Rule != "LB5xx" {
		t.Errorf("incident %+v: caught by Caught, LB5xx after it", in)
	}
	for _, j := range r.Firings {
		if j.Rule == "Quick" && !j.Held.IsZero() {
			t.Errorf("Quick's firing %+v: held from its start, said as none", j)
		}
	}

	// The precision the stones give is the same as with no rules known,
	// which says nothing of after.
	plain := Judge(nil, slices.Clone(fs[:6]), incs, opts())
	for _, rr := range plain.Rules {
		got := by[rr.Rule]
		if got.Firings != rr.Firings || got.White != rr.White || got.Black != rr.Black || got.Unjudged != rr.Unjudged || got.Noisy != rr.Noisy || rr.Late != 0 {
			t.Errorf("%s: %+v with its rules known, %+v without: the stones differ", rr.Rule, got, rr)
		}
	}
	if len(plain.Incidents[0].FiredAfter) != 0 {
		t.Errorf("no rules known: %+v", plain.Incidents[0])
	}
}

// TestFiredAfterReadBack: a report's firings, read back as firings from
// elsewhere and judged with no rules known, say what the report said; a
// firing from elsewhere with no held_from came after nothing, and one that
// held from after it fired is refused.
func TestFiredAfterReadBack(t *testing.T) {
	known, fs, incs := afterScenario()
	r := Judge(known, slices.Clone(fs), incs, opts())
	var b bytes.Buffer
	if err := JSON(&b, r); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Firings []json.RawMessage `json:"firings"`
	}
	if err := json.Unmarshal(b.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, f := range doc.Firings {
		var line bytes.Buffer
		if err := json.Compact(&line, f); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line.String())
	}
	back, err := firing.ReadJSONL(strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	again := Judge(nil, back, incs, opts())
	if a, b := again.Incidents[0].FiredAfter, r.Incidents[0].FiredAfter; !slices.Equal(a, b) {
		t.Errorf("read back %+v, the report %+v", a, b)
	}
	late := func(rep *Report) (n int) {
		for _, rr := range rep.Rules {
			n += rr.Late
		}
		return n
	}
	if late(again) != 1 || late(r) != 1 {
		t.Errorf("late firings: read back %d, the report %d, want 1", late(again), late(r))
	}

	bare, err := firing.ReadJSONL(strings.NewReader(`{"rule":"LB5xx","labels":{"service":"api"},"start":"2026-10-26T01:49:00Z"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := Judge(nil, bare, incs, opts()); len(got.Incidents[0].FiredAfter) != 0 {
		t.Errorf("a firing with no held_from: %+v", got.Incidents[0])
	}
	odd, err := firing.ReadJSONL(strings.NewReader(`{"rule":"LB5xx","labels":{"service":"api"},"start":"2026-10-26T01:49:00Z","held_from":"2026-10-26T01:50:00Z"}`))
	if err != nil || !odd[0].Held.IsZero() {
		t.Errorf("held_from after start: %v, %+v; want it read as none", err, odd)
	}
}

// TestFiredAfterText: the incident a rule fired on only once it was over
// says so, by how long after its end; the rules' black firings that came
// after an incident are counted beneath the rules.
func TestFiredAfterText(t *testing.T) {
	known, fs, incs := afterScenario()
	var b bytes.Buffer
	if err := Text(&b, Judge(known, slices.Clone(fs[:4]), incs, opts()), time.UTC); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"✗ nothing in time; after its end: LB5xx +3m",
		"! 1 of the black firings came after an incident they agree with was over (LB5xx 1)",
		"✗ 1 of 1 incidents had no alert.\n  1 of them had one only once they were over (\"after its end\" above).",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	b.Reset()
	if err := Text(&b, Judge(nil, slices.Clone(fs[:4]), incs, opts()), time.UTC); err != nil {
		t.Fatal(err)
	}
	if out := b.String(); strings.Contains(out, "after its end") || strings.Contains(out, "came after an incident") {
		t.Errorf("no rules known, nothing said of after:\n%s", out)
	}
}

// TestImpactShortLateText: when rules need longer for their late metrics,
// the note on short impact says how many; with none late, it is as it was.
func TestImpactShortLateText(t *testing.T) {
	r := &Report{From: m(0), To: m(600),
		Incidents:   []IncidentResult{{Incident: incident.Incident{ID: "impact-1", Start: m(100), End: m(106), Candidate: true}}},
		ImpactShort: &ImpactShort{For: 6 * time.Minute, Rules: 2, Judged: 5, Need: 7 * time.Minute, Most: 1, Lagged: 1}}
	var b bytes.Buffer
	if err := Text(&b, r, time.UTC); err != nil {
		t.Fatal(err)
	}
	want := "--impact-for 7m counts only what they can.\n  Of them, 1 read metrics that arrive late (--late, query_offset, a Grafana query's time range): what they\n  need counts it.\n"
	if !strings.Contains(b.String(), want) {
		t.Errorf("lacks %q:\n%s", want, b.String())
	}
	r.ImpactShort.Lagged = 0
	b.Reset()
	if err := Text(&b, r, time.UTC); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "arrive late") {
		t.Errorf("none late:\n%s", b.String())
	}
}

// TestFiredAfterNoEnd: an incident written down with no end is taken to
// last --unknown-length, as when it was judged, and an alert after that
// is said by how long after it.
func TestFiredAfterNoEnd(t *testing.T) {
	known := []rules.Rule{{Name: "Slow", Expr: "x", For: 10 * time.Minute}}
	fs := []firing.Firing{{Rule: "Slow", Start: m(165), End: m(170)}} // held from 155, the hour from 100 ending at 160
	incs := []incident.Incident{{ID: "I", Start: m(100)}}
	r := Judge(known, fs, incs, opts())
	if in := r.Incidents[0]; len(in.FiredAfter) != 1 || in.FiredAfter[0].Delay != 65*time.Minute {
		t.Fatalf("%+v", in)
	}
	var b bytes.Buffer
	if err := Text(&b, r, time.UTC); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "after its end: Slow +5m") {
		t.Errorf("want 5m after the hour it is taken to last:\n%s", b.String())
	}
}
