package verdict

import (
	"bytes"
	"slices"
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
		{Name: "Broken", Expr: "w"}, // as it stood at another time
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
		{ID: "INC-1", Title: "Checkout errors", Start: m(100), End: m(140), Labels: incident.Labels{"service": {"checkout", "payments"}}},
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

func TestMatchSeveral(t *testing.T) {
	fs := []firing.Firing{
		{Rule: "PaymentsSlow", Labels: map[string]string{"service": "payments"}, Start: m(105)},
		{Rule: "SearchSlow", Labels: map[string]string{"service": "search"}, Start: m(105)},
		{Rule: "DBCPU", Start: m(106)}, // no service: no constraint
	}
	incs := []incident.Incident{{ID: "I", Start: m(100), Labels: incident.Labels{"service": {"checkout", "payments"}}}}
	var got []string
	for _, c := range Judge(nil, fs, incs, opts()).Incidents[0].CaughtBy {
		got = append(got, c.Rule)
	}
	if !slices.Equal(got, []string{"PaymentsSlow", "DBCPU"}) {
		t.Errorf("caught by %v, want PaymentsSlow and DBCPU", got)
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
	if got := Clean("검색 지연 5xx"); got != "검색 지연 5xx" {
		t.Errorf("Clean changed ordinary text: %q", got)
	}
}

func TestNoisyCatch(t *testing.T) {
	var fs []firing.Firing
	for i := 0; i < 12; i++ { // fires every two hours whatever happens
		fs = append(fs, firing.Firing{Rule: "Chatty", Start: m(i * 120), End: m(i*120 + 5)})
	}
	fs = append(fs, firing.Firing{Rule: "Precise", Start: m(605), End: m(620)})
	incs := []incident.Incident{
		{ID: "A", Start: m(238)},  // Chatty fires at 240, by chance
		{ID: "B", Start: m(600)},  // Chatty at 600, Precise at 605
		{ID: "C", Start: m(3000)}, // nothing
	}
	r := Judge(nil, fs, incs, opts())
	a, b, c := r.Incidents[0], r.Incidents[1], r.Incidents[2]
	if !a.OnlyNoisy() || !a.CaughtBy[0].Noisy {
		t.Errorf("A %+v: caught only by a rule that fires whatever happens", a)
	}
	if first, ok := b.First(); b.OnlyNoisy() || !ok || first.Rule != "Precise" || first.Delay != 5*time.Minute {
		t.Errorf("B %+v: the first catch that counts is Precise at +5m", b)
	}
	if !c.Missed() || c.OnlyNoisy() {
		t.Errorf("C %+v", c)
	}
	var text, metrics bytes.Buffer
	Text(&text, r, time.UTC)
	Prometheus(&metrics, r, t0)
	for _, want := range []string{"Chatty +2m (noisy)", "! 1 of 3 incidents were caught only by noisy rules"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text lacks %q:\n%s", want, text.String())
		}
	}
	for _, want := range []string{`lapilli_incidents{state="noisy_only",source="written"} 1`, `lapilli_incident_detection_delay_seconds{incident="B"} 300`} {
		if !strings.Contains(metrics.String(), want) {
			t.Errorf("metrics lack %q:\n%s", want, metrics.String())
		}
	}
	if strings.Contains(metrics.String(), `incident="A"`) {
		t.Errorf("a noisy-only catch has no detection delay worth graphing:\n%s", metrics.String())
	}
}

// A rule its replay can't see whole is said so, with its name; a Grafana
// rule for its query errors isn't, as nearly every one fires on them.
func TestUnseenText(t *testing.T) {
	g := &rules.Graph{ExecErr: rules.StateError}
	rs := []rules.Rule{{Name: "Grafana", Graph: g}, {Name: "Cloud", Expr: "x", Cloud: "google"}, {Name: "Plain", Expr: "x"}}
	rep := Judge(rs, nil, nil, Options{})
	var b strings.Builder
	if err := Text(&b, rep, time.UTC); err != nil {
		t.Fatal(err)
	}
	if want := "1 rules read a cloud's own metrics, which often arrive minutes late: without --late, they are replayed as filled in since (Cloud)."; !strings.Contains(b.String(), want) {
		t.Errorf("want %q in\n%s", want, b.String())
	}
	if strings.Contains(b.String(), "Grafana)") {
		t.Errorf("a Grafana rule marked for its errors:\n%s", b.String())
	}
	if !slices.Equal(rep.Rules[slices.IndexFunc(rep.Rules, func(r RuleResult) bool { return r.Rule == "Cloud" })].Unseen, []string{rules.UnseenLate}) {
		t.Errorf("rules %+v", rep.Rules)
	}
	// A query on another data source, and one outside the condition that
	// failed.
	other := &rules.Graph{Other: map[string]string{"L": "loki"}}
	rs = []rules.Rule{{Name: "Logged", Graph: other}, {Name: "Failing", Graph: g, Failed: []string{"L"}}}
	b.Reset()
	if err := Text(&b, Judge(rs, nil, nil, Options{}), time.UTC); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1 Grafana rules have a query on another data source than their condition's, which isn't replayed: whenever it returns nothing, Grafana takes the rule as having no data (Logged).",
		"1 Grafana rules had a query outside their condition fail when replayed: whether it returned anything, which Grafana's no data hangs on, isn't known (Failing)."} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("want %q in\n%s", want, b.String())
		}
	}
}

// A label's value in --metrics and /metrics, as Prometheus's text format
// writes it: what doesn't show, or acts on a terminal, said as Clean says
// it, as every other output does.
func TestLabelValue(t *testing.T) {
	for in, want := range map[string]string{
		"Errors":               "Errors",
		`a "b" \c` + "\nd":     `a \"b\" \\c\\u{a}d`,
		"Up\x1b]52;c;eA==\x07": `Up\\u{1b}]52;c;eA==\\u{7}`,
		"Errors‮":              `Errors\\u{202e}`,
		"tab\there":            `tab\\u{9}here`,
		// The joiners names are written with are as they are, as ALERTS
		// has them.
		"👩‍💻 ن‌م": "👩‍💻 ن‌م",
	} {
		if got := LabelValue(in); got != want {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
	}
}
