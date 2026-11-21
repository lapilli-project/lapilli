package verdict

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
)

// linkedWorld is an incident in checkout whose tool linked an alert of
// CartErrors about cart to it, and the firings around it.
func linkedWorld() ([]firing.Firing, []incident.Incident) {
	fs := []firing.Firing{
		// The alert the tool linked, about another service: --match would
		// keep it from catching.
		{Rule: "CartErrors", Labels: map[string]string{"service": "cart", "severity": "page"}, Start: m(101), End: m(120)},
		// Agreeing on service, not linked: caught by the time alone.
		{Rule: "CheckoutLatency", Labels: map[string]string{"service": "checkout"}, Start: m(103), End: m(110)},
		// Of the rule linked, another service's: not linked, and
		// disagreeing.
		{Rule: "CartErrors", Labels: map[string]string{"service": "search"}, Start: m(102), End: m(104)},
		// Linked, but before the lead: it was on anyway.
		{Rule: "CartErrors", Labels: map[string]string{"service": "cart", "severity": "page"}, Start: m(60), End: m(70)},
		// A rule told apart from another of its name, of the name the
		// tool knows.
		{Rule: "DiskFull [severity=critical]", Labels: map[string]string{"severity": "critical", "node": "n1"}, Start: m(105), End: m(130)},
	}
	incs := []incident.Incident{
		{ID: "INC-7", Start: m(100), End: m(140), Labels: incident.Labels{"service": {"checkout"}},
			Alerts: []incident.Alert{{Rule: "CartErrors", Labels: map[string]string{"service": "cart"}}, {Rule: "DiskFull"}}},
		// No alerts listed: judged as ever, with --linked-only too.
		{ID: "INC-8", Start: m(1000), End: m(1030), Labels: incident.Labels{"service": {"checkout"}}},
	}
	fs = append(fs, firing.Firing{Rule: "CheckoutLatency", Labels: map[string]string{"service": "checkout"}, Start: m(1001), End: m(1010)})
	return fs, incs
}

func catchesOf(in IncidentResult) map[string]Catch {
	out := map[string]Catch{}
	for _, c := range in.CaughtBy {
		out[c.Rule] = c
	}
	return out
}

// A firing of an alert the incident lists catches it, labels disagreeing on
// --match or not, marked linked; a firing of its rule with other labels
// doesn't, nor one before the lead; a rule told apart is linked by its
// name as written. The rest catch by the time and labels as before.
func TestLinkedAlertsCatch(t *testing.T) {
	fs, incs := linkedWorld()
	r := Judge(nil, fs, incs, opts())
	got := catchesOf(r.Incidents[0])
	if c, ok := got["CartErrors"]; !ok || !c.Linked || c.Delay != time.Minute {
		t.Errorf("CartErrors: %+v %v, want caught by the alert linked, +1m, linked", c, ok)
	}
	if c, ok := got["DiskFull [severity=critical]"]; !ok || !c.Linked {
		t.Errorf("DiskFull [severity=critical]: %+v %v, want linked by the name its alerts carry", c, ok)
	}
	if c, ok := got["CheckoutLatency"]; !ok || c.Linked {
		t.Errorf("CheckoutLatency: %+v %v, want caught by the time, not linked", c, ok)
	}
	if len(got) != 3 {
		t.Errorf("INC-7 caught by %+v, want three rules", r.Incidents[0].CaughtBy)
	}
	for _, f := range r.Firings {
		if f.Rule == "CartErrors" && f.Labels["service"] == "search" && f.White {
			t.Errorf("CartErrors about search caught %v: its labels aren't the alert's", f.Incidents)
		}
		if f.Rule == "CartErrors" && f.Start.Equal(m(60)) && f.White {
			t.Errorf("CartErrors before the lead caught %v", f.Incidents)
		}
	}
	if c := catchesOf(r.Incidents[1]); len(c) != 1 || c["CheckoutLatency"].Linked {
		t.Errorf("INC-8 caught by %+v, want CheckoutLatency by the time", r.Incidents[1].CaughtBy)
	}

	var b bytes.Buffer
	if err := Text(&b, r, time.UTC); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "CartErrors +1m (linked)") || strings.Contains(b.String(), "CheckoutLatency +3m (linked)") {
		t.Errorf("the text doesn't mark the linked catch alone:\n%s", b.String())
	}
	b.Reset()
	if err := JSON(&b, r); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Settings  map[string]any `json:"settings"`
		Incidents []struct {
			Alerts   []incident.Alert `json:"alerts"`
			CaughtBy []map[string]any `json:"caught_by"`
		} `json:"incidents"`
	}
	if err := json.Unmarshal(b.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Settings["linked_only"]; ok || len(doc.Incidents[0].Alerts) != 2 {
		t.Errorf("--json: settings %v, alerts %+v", doc.Settings, doc.Incidents[0].Alerts)
	}
	n := 0
	for _, c := range doc.Incidents[0].CaughtBy {
		if c["linked"] == true {
			n++
		}
	}
	if n != 2 {
		t.Errorf("--json: %d catches linked, want 2: %v", n, doc.Incidents[0].CaughtBy)
	}
}

// With --linked-only, an incident that lists alerts is caught by them
// alone, and one that lists none as before; the report says it was judged
// so.
func TestLinkedOnly(t *testing.T) {
	fs, incs := linkedWorld()
	o := opts()
	o.LinkedOnly = true
	r := Judge(nil, fs, incs, o)
	got := catchesOf(r.Incidents[0])
	if _, ok := got["CheckoutLatency"]; ok || len(got) != 2 || !got["CartErrors"].Linked {
		t.Errorf("INC-7 caught by %+v, want the two linked alone", r.Incidents[0].CaughtBy)
	}
	if c := catchesOf(r.Incidents[1]); len(c) != 1 {
		t.Errorf("INC-8, which lists no alerts, caught by %+v, want CheckoutLatency", r.Incidents[1].CaughtBy)
	}
	if !r.Settings.LinkedOnly {
		t.Error("settings don't say --linked-only")
	}
	// A firing that came after its end, its condition having held in
	// it: of a linked alert, the incident's own alert too late, labels or
	// not; of another, with --linked-only, not the incident's.
	end := m(140)
	late := []firing.Firing{
		{Rule: "CartErrors", Labels: map[string]string{"service": "cart"}, Start: end.Add(5 * time.Minute), Held: m(130)},
		{Rule: "CheckoutLatency", Labels: map[string]string{"service": "checkout"}, Start: end.Add(5 * time.Minute), Held: m(130)},
	}
	r = Judge(nil, late, incs[:1], o)
	if fa := r.Incidents[0].FiredAfter; len(fa) != 1 || fa[0].Rule != "CartErrors" {
		t.Errorf("fired after INC-7: %+v, want the linked CartErrors alone", fa)
	}
}
