package verdict

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/rules"
)

var t0 = time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)

func m(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }

func opts() Options {
	return Options{From: t0, To: t0.Add(7 * 24 * time.Hour), Lead: 15 * time.Minute, Match: []string{"service"},
		RefireWithin: time.Hour, UnknownLen: time.Hour}
}

func scenario() *Report {
	known := []rules.Rule{
		{Name: "CheckoutErrors", Expr: "x"}, {Name: "Flappy", Expr: "y"}, {Name: "Silent", Expr: "z"},
		{Name: "Broken", Unsupported: "classic_conditions expressions"},
	}
	fs := []firing.Firing{
		{Rule: "CheckoutErrors", Labels: map[string]string{"service": "checkout"}, Start: m(102), End: m(130)},
		{Rule: "Flappy", Start: m(10), End: m(12)},
		{Rule: "Flappy", Start: m(40), End: m(42)},                                     // back within the hour: refired
		{Rule: "Flappy", Start: m(500), End: m(502)},                                   // much later: not refired
		{Rule: "Flappy", Labels: map[string]string{"service": "search"}, Start: m(98)}, // other service: no catch
		{Rule: "AlwaysOn", Start: t0.Add(-time.Hour)},                                  // on before the incident: no catch
	}
	incs := []incident.Incident{
		{ID: "INC-1", Title: "Checkout errors", Start: m(100), End: m(140), Labels: map[string]string{"service": "checkout"}},
		{ID: "impact-1", Start: m(2000), End: m(2020), Candidate: true},
		{ID: "INC-0", Start: t0.Add(-48 * time.Hour)}, // before the period
	}
	return Judge(known, fs, incs, opts())
}

func TestJudge(t *testing.T) {
	r := scenario()
	by := map[string]RuleResult{}
	for _, rr := range r.Rules {
		by[rr.Rule] = rr
	}
	if c := by["CheckoutErrors"]; c.White != 1 || c.Black != 0 {
		t.Errorf("CheckoutErrors %+v", c)
	}
	if f := by["Flappy"]; f.Firings != 4 || f.White != 0 || f.Black != 4 || f.Refired != 1 {
		t.Errorf("Flappy %+v", f)
	}
	if a := by["AlwaysOn"]; a.White != 0 {
		t.Errorf("AlwaysOn %+v: a firing already on before the incident caught nothing", a)
	}
	if s := by["Silent"]; s.Firings != 0 || !s.Replayed {
		t.Errorf("Silent %+v", s)
	}
	if b := by["Broken"]; b.Unsupported == "" || b.Replayed {
		t.Errorf("Broken %+v", b)
	}
	if len(r.Incidents) != 2 {
		t.Fatalf("incidents %+v: want the two in the period", r.Incidents)
	}
	if in := r.Incidents[0]; in.Missed() || in.CaughtBy[0].Rule != "CheckoutErrors" || in.CaughtBy[0].Delay != 2*time.Minute {
		t.Errorf("INC-1 %+v", in)
	}
	if !r.Incidents[1].Missed() {
		t.Errorf("impact-1 should be missed: %+v", r.Incidents[1])
	}
	if r.Rules[0].Rule != "Flappy" {
		t.Errorf("first rule %s, want the noisiest", r.Rules[0].Rule)
	}
}

func TestLead(t *testing.T) {
	fs := []firing.Firing{{Rule: "Early", Start: m(90)}}
	incs := []incident.Incident{{ID: "I", Start: m(100)}}
	r := Judge(nil, fs, incs, opts())
	if r.Incidents[0].Missed() || r.Incidents[0].CaughtBy[0].Delay != -10*time.Minute {
		t.Errorf("%+v: a firing 10m before, within the 15m lead, catches it", r.Incidents[0])
	}
}

func TestText(t *testing.T) {
	var b bytes.Buffer
	if err := Text(&b, scenario(), time.UTC); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"6 firings, 1 incidents written down, 1 more found by --impact",
		"RULE",
		"1 rules never fired.",
		"Broken: classic_conditions expressions",
		"INC-1 Checkout errors",
		"CheckoutErrors +2m",
		"✗ nothing fired",
		"✗ 1 of 2 incidents had no alert.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text lacks %q:\n%s", want, out)
		}
	}
}

func TestPrometheus(t *testing.T) {
	var b bytes.Buffer
	if err := Prometheus(&b, scenario(), t0); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`lapilli_rule_nothing{rule="Flappy"} 4`,
		`lapilli_rule_caught{rule="CheckoutErrors"} 1`,
		`lapilli_incidents{state="missed",source="impact"} 1`,
		`lapilli_incident_detection_delay_seconds{incident="INC-1"} 120`,
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("metrics lack %q:\n%s", want, b.String())
		}
	}
	if strings.Contains(b.String(), `rule="Broken"`) {
		t.Error("an unreplayable rule made it into the metrics")
	}
}

func TestDelay(t *testing.T) {
	for d, want := range map[time.Duration]string{
		2 * time.Minute: "+2m", -30 * time.Second: "-30s", 90 * time.Second: "+1m30s", 125 * time.Minute: "+2h5m",
	} {
		if got := Delay(d); got != want {
			t.Errorf("Delay(%s) = %s, want %s", d, got, want)
		}
	}
}

func TestClean(t *testing.T) {
	if got := Clean("ok\x1b[2Jrule\u202e"); got != `ok\u{1b}[2Jrule\u{202e}` {
		t.Errorf("Clean = %q", got)
	}
	if got := Clean("주문 결제 5xx"); got != "주문 결제 5xx" {
		t.Errorf("Clean changed ordinary text: %q", got)
	}
}
