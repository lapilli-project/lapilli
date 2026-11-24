package verdict

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/rules"
)

// TestNotKnown: an incident nothing caught is not known missed when a rule
// couldn't be replayed, which may have caught it; one a rule judged from
// its history couldn't have been missed by doesn't make it so.
func TestNotKnown(t *testing.T) {
	incs := []incident.Incident{{ID: "INC-1", Start: m(100), End: m(130)}}
	failed := rules.Rule{Name: "HighErrors", Unsupported: rules.HistoryUnread + "connection refused"}
	fine := rules.Rule{Name: "CheckoutSlow", Expr: "up == 0"}

	r := Judge([]rules.Rule{failed, fine}, nil, incs, opts())
	if in := r.Incidents[0]; !in.Missed() || !in.NotKnown {
		t.Errorf("a rule couldn't be replayed: %+v", in)
	}
	r = Judge([]rules.Rule{fine}, nil, incs, opts())
	if in := r.Incidents[0]; !in.Missed() || in.NotKnown {
		t.Errorf("every rule replayed: %+v", in)
	}
	r = Judge([]rules.Rule{failed}, nil, []incident.Incident{{ID: "INC-1", Start: m(100)}}, opts())
	for i := range r.Rules {
		r.Rules[i].Source = GrafanaSource
	}
	r.MarkNotKnown()
	if in := r.Incidents[0]; !in.Missed() || in.NotKnown {
		t.Errorf("a rule judged from its history: %+v", in)
	}
}

// TestNotKnownText: the table says "? not known" for such an incident, and
// the summary says what it can't tell, by whether any rule was replayed.
func TestNotKnownText(t *testing.T) {
	inc := func(id string, known bool) IncidentResult {
		return IncidentResult{Incident: incident.Incident{ID: id, Start: t0, End: t0.Add(time.Hour)}, NotKnown: known}
	}
	text := func(r *Report) string {
		t.Helper()
		var b bytes.Buffer
		if err := Text(&b, r, time.UTC); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	broken := RuleResult{Rule: "HighErrors", Unsupported: rules.HistoryUnread + "connection refused"}
	out := text(&Report{From: t0, To: t0.Add(time.Hour), Rules: []RuleResult{broken, {Rule: "NoData", Unsupported: "uses Loki"}},
		Incidents: []IncidentResult{inc("INC-1", true), inc("INC-2", true)}})
	for _, want := range []string{"? not known", "? 2 of 2 incidents can't be judged: no rule was replayed.\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "✗") {
		t.Errorf("a miss said that isn't known:\n%s", out)
	}
	out = text(&Report{From: t0, To: t0.Add(time.Hour), Rules: []RuleResult{broken, {Rule: "CheckoutSlow", Replayed: true}},
		Incidents: []IncidentResult{inc("INC-1", true)}})
	want := "? 1 of 1 incident had no alert from the 1 rule replayed: the 1 that couldn't be replayed may have caught it.\n"
	if !strings.Contains(out, want) {
		t.Errorf("no %q in:\n%s", want, out)
	}
	out = text(&Report{From: t0, To: t0.Add(time.Hour), Rules: []RuleResult{{Rule: "CheckoutSlow", Replayed: true}},
		Incidents: []IncidentResult{inc("INC-1", false)}})
	if !strings.Contains(out, "✗ 1 of 1 incident had no alert.\n") || strings.Contains(out, "?") {
		t.Errorf("a known miss:\n%s", out)
	}
}

// TestNotKnownMetric: the missed incidents among them not known are
// counted, and lapilli_incidents says what it did.
func TestNotKnownMetric(t *testing.T) {
	r := &Report{From: t0, To: t0.Add(time.Hour), Rules: []RuleResult{{Rule: "HighErrors", Unsupported: "uses Loki"}},
		Incidents: []IncidentResult{{Incident: incident.Incident{ID: "INC-1", Start: t0}, NotKnown: true}, {Incident: incident.Incident{ID: "INC-2", Start: t0}}}}
	var b bytes.Buffer
	if err := Prometheus(&b, r, t0); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`lapilli_incidents{state="missed",source="written"} 2`, `lapilli_incidents_not_known{source="written"} 1`, `lapilli_incidents_not_known{source="impact"} 0`} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("no %q in:\n%s", want, b.String())
		}
	}
}

// TestPartialFoot: the foot lists what the top didn't name, as "more", and
// nothing when the top named every rule.
func TestPartialFoot(t *testing.T) {
	reason := rules.HistoryUnread + "connection refused"
	r := &Report{Rules: []RuleResult{{Rule: "HighErrors", Unsupported: reason}, {Rule: "NoData", Unsupported: reason}, {Rule: "Alarm", Unsupported: "a CloudWatch metric alarm isn't PromQL"}}}
	r.Unreplayed = r.UnreplayedRules()
	var b strings.Builder
	if err := Text(&b, r, time.UTC); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "  HighErrors, NoData: query failed: the history couldn't be read: connection refused\n") ||
		!strings.Contains(out, "\nCould not replay 1 more rule:\n  Alarm: a CloudWatch") || strings.Count(out, "HighErrors") != 1 {
		t.Errorf("got\n%s", out)
	}
	r.Rules = r.Rules[:2]
	r.Unreplayed = r.UnreplayedRules()
	b.Reset()
	if err := Text(&b, r, time.UTC); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "Could not replay") || strings.Count(b.String(), "HighErrors") != 1 {
		t.Errorf("a list twice:\n%s", b.String())
	}
}

// TestHoursZoneLocal: the machine's own zone is named as the header names
// it, not as Go does.
func TestHoursZoneLocal(t *testing.T) {
	r := &Report{From: t0, To: t0.Add(time.Hour), Hours: &HoursUsed{Zone: "Local", Work: "mon-fri@09:00-18:00", Sleep: "22:00-08:00"},
		Rules: []RuleResult{{Rule: "HighErrors", Replayed: true, Firings: 1, Black: 1, AsleepBlack: 1}}}
	var b strings.Builder
	if err := Text(&b, r, time.UTC); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "in Local") || !strings.Contains(b.String(), "Out of hours, in "+Zone(time.Local, r.From, r.To)+" (") {
		t.Errorf("got\n%s", b.String())
	}
}

// TestNotKnownCantFire: a rule that fires nothing, replayed or not, doesn't
// make an incident not known; a rule whose replay failed still does.
func TestNotKnownCantFire(t *testing.T) {
	incs := []incident.Incident{{ID: "INC-1", Start: m(100), End: m(130)}}
	fine := rules.Rule{Name: "CheckoutSlow", Expr: "up == 0"}
	for _, why := range []string{rules.Disabled, rules.Paused, rules.NoQuery} {
		r := Judge([]rules.Rule{{Name: "Old", Unsupported: why}, fine}, nil, incs, opts())
		if in := r.Incidents[0]; !in.Missed() || in.NotKnown {
			t.Errorf("%s: %+v", why, in)
		}
		var b bytes.Buffer
		if err := Text(&b, r, time.UTC); err != nil {
			t.Fatal(err)
		}
		if out := b.String(); !strings.Contains(out, "✗ 1 of 1 incident had no alert.") || strings.Contains(out, "?") {
			t.Errorf("%s:\n%s", why, out)
		}
	}
	r := Judge([]rules.Rule{{Name: "Old", Unsupported: rules.Disabled}, {Name: "Broken", Unsupported: "uses Loki"}, fine}, nil, incs, opts())
	if in := r.Incidents[0]; !in.NotKnown {
		t.Errorf("a failed replay beside a disabled rule: %+v", in)
	}
	var b bytes.Buffer
	if err := Text(&b, r, time.UTC); err != nil {
		t.Fatal(err)
	}
	if want := "the 1 that couldn't be replayed"; !strings.Contains(b.String(), want) {
		t.Errorf("no %q in:\n%s", want, b.String())
	}
}

// TestNotKnownGrammar: the "?" line says "it" for one incident and "them"
// for more, and the late line follows the "✗" line, counting known misses.
func TestNotKnownGrammar(t *testing.T) {
	end := t0.Add(time.Hour)
	inc := func(id string, known bool, late bool) IncidentResult {
		in := IncidentResult{Incident: incident.Incident{ID: id, Start: t0, End: end}, NotKnown: known}
		if late {
			in.FiredAfter = []Catch{{Rule: "CheckoutSlow", Delay: 2 * time.Hour}}
		}
		return in
	}
	text := func(ins ...IncidentResult) string {
		t.Helper()
		r := &Report{From: t0, To: end, Rules: []RuleResult{{Rule: "HighErrors", Unsupported: "uses Loki"}, {Rule: "CheckoutSlow", Replayed: true}, {Rule: "Cart", Replayed: true}}, Incidents: ins}
		var b bytes.Buffer
		if err := Text(&b, r, time.UTC); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	out := text(inc("INC-1", true, true), inc("INC-2", true, false))
	if want := "from the 2 rules replayed: the 1 that couldn't be replayed may have caught them.\n"; !strings.Contains(out, want) {
		t.Errorf("no %q in:\n%s", want, out)
	}
	if strings.Contains(out, "only once they were over") {
		t.Errorf("a late line for incidents not known:\n%s", out)
	}
	out = text(inc("INC-1", false, true), inc("INC-2", true, true))
	i, j := strings.Index(out, "✗ 1 of 2 incidents had no alert.\n  1 of them had one only once"), strings.Index(out, "? 1 of 2")
	if i < 0 || j < i {
		t.Errorf("the late line isn't right after the ✗ line, before the ?:\n%s", out)
	}
	if strings.Count(out, "only once they were over") != 1 {
		t.Errorf("late line count:\n%s", out)
	}
}
