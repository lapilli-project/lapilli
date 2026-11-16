package verdict

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/rules"
)

// secrets are the names a report below holds, each somewhere a reader of a
// redacted one mustn't find it: rules, groups, label values, incidents and
// their titles, teams, sources, hosts, URLs, an expression, a schedule.
var secrets = []string{
	"CheckoutApiErrors", "PaymentsQueueStuck", "LedgerDiskFull", "shop-alerts", "checkout-svc", "payments-svc",
	"db-primary-7", "INC-4711", "Checkout fell over", "team-orange", "cw-prod-eu", "prom.internal.example",
	"https://runbooks.example", "http_errors_total", "on-call=nightwatch", "nightwatch", "orders-db",
	"KafkaLagRecording", "recording-group", "198.51.100.7",
}

// redactedReport is a report with every name in secrets in it.
func redactedReport(t *testing.T) *Report {
	t.Helper()
	t0 := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	known := []rules.Rule{
		{Name: "CheckoutApiErrors", Group: "shop-alerts", Labels: map[string]string{"severity": "critical"}},
		{Name: "PaymentsQueueStuck", Group: "shop-alerts", Labels: map[string]string{"severity": "warning"}},
		{Name: "LedgerDiskFull", Group: "shop-alerts", Unsupported: rules.HistoryUnread + "Get https://prom.internal.example/api/v1/query_range?query=http_errors_total: dial tcp 198.51.100.7:9090"},
	}
	var fs []firing.Firing
	for k := range 12 {
		fs = append(fs, firing.Firing{Rule: "PaymentsQueueStuck", Labels: map[string]string{"service": "payments-svc", "instance": "db-primary-7", "severity": "warning"},
			Start: t0.Add(time.Duration(k) * 3 * time.Hour), End: t0.Add(time.Duration(k)*3*time.Hour + 10*time.Minute)})
	}
	fs = append(fs, firing.Firing{Rule: "CheckoutApiErrors", Labels: map[string]string{"service": "checkout-svc", "severity": "critical"},
		Start: t0.Add(25 * time.Hour), End: t0.Add(26 * time.Hour)})
	incs := []incident.Incident{
		{ID: "INC-4711", Title: "Checkout fell over", Start: t0.Add(25*time.Hour + 5*time.Minute), Labels: incident.Labels{"service": {"checkout-svc", "orders-db"}}},
		{ID: "INC-4712", Title: "Checkout fell over again", Start: t0.Add(50 * time.Hour), Labels: incident.Labels{"service": {"payments-svc"}}},
	}
	r := Judge(known, fs, incs, Options{From: t0, To: t0.Add(7 * 24 * time.Hour), Lead: 15 * time.Minute, RefireWithin: time.Hour,
		UnknownLen: time.Hour, Match: []string{"service"}, Hours: DefaultHours(time.UTC)})
	for i := range r.Rules {
		r.Rules[i].Team, r.Rules[i].Source = "team-orange", "cw-prod-eu"
	}
	r.Unreplayed = r.UnreplayedRules()
	r.Suggestions = []Suggestion{{Rule: "PaymentsQueueStuck", For: 0, NewFor: 10 * time.Minute, Before: r.Rules[0], After: r.Rules[0]}}
	reached := 0.8
	r.NearMisses = []NearMiss{{Incident: "INC-4712", Rule: "CheckoutApiErrors", Labels: map[string]string{"service": "checkout-svc", "instance": "db-primary-7"},
		Before: r.Rules[1], Value: &ByValue{Op: ">", Bound: 1, Of: "sum(rate(http_errors_total[5m]))", Reached: &reached, New: 0.8}}}
	clear := 3
	r.Unrecorded = []Unrecorded{{Rule: "PaymentsQueueStuck", Firings: 12, Clear: &clear, Bound: "> 5", Alerts: 1, Team: "team-orange", Scope: "team-orange",
		Top: []AlertCount{{Labels: map[string]string{"instance": "db-primary-7"}, Firings: 12, Team: "team-orange"}}}}
	r.Hours.Shifts = "on-call=nightwatch"
	r.Hours.Teams = []TeamHours{{Team: "team-orange", Zone: "UTC", Work: "mon-fri@08:00-18:00", Sleep: "22:00-08:00", Shifts: "on-call=nightwatch"}}
	return r
}

// writeAll is every way a report is written.
func writeAll(t *testing.T, r *Report) string {
	t.Helper()
	var b bytes.Buffer
	for _, write := range []func() error{
		func() error { return Text(&b, r, time.UTC) },
		func() error { return JSON(&b, r) },
		func() error { return Prometheus(&b, r, time.Now()) },
	} {
		if err := write(); err != nil {
			t.Fatal(err)
		}
	}
	return b.String()
}

// noSecrets fails for each name of secrets in out, in any letter case.
func noSecrets(t *testing.T, what, out string) {
	t.Helper()
	low := strings.ToLower(out)
	for _, s := range secrets {
		if strings.Contains(low, strings.ToLower(s)) {
			i := strings.Index(low, strings.ToLower(s))
			t.Errorf("%s: %q is there: …%s…", what, s, out[max(0, i-60):min(len(out), i+len(s)+60)])
		}
	}
}

// A redacted report's text, JSON and metrics hold none of its names, and
// every number, severity and time as they were.
func TestRedactReport(t *testing.T) {
	r := redactedReport(t)
	plain := writeAll(t, r)
	for _, s := range []string{"CheckoutApiErrors", "INC-4711", "db-primary-7", "http_errors_total", "nightwatch", "team-orange", "prom.internal.example"} {
		if !strings.Contains(plain, s) {
			t.Fatalf("the report unredacted lacks %q: a test of nothing", s)
		}
	}
	before, _ := json.Marshal(r)
	red := NewRedactor(nil).Report(r)
	if after, _ := json.Marshal(r); !bytes.Equal(before, after) {
		t.Error("redacting changed the report it was given")
	}
	noSecrets(t, "redacted", writeAll(t, red))

	// The numbers are the same, rule by rule, in the same order.
	numbers := func(rs []RuleResult) []string {
		var out []string
		for _, rr := range rs {
			rr.Rule, rr.Group, rr.Team, rr.Source, rr.Unsupported = "", "", "", "", ""
			b, _ := json.Marshal(rr)
			out = append(out, string(b))
		}
		return out
	}
	if a, b := numbers(r.Rules), numbers(red.Rules); !slices.Equal(a, b) {
		t.Errorf("numbers:\n%v\n%v", a, b)
	}
	if len(red.Firings) != len(r.Firings) || len(red.Incidents) != len(r.Incidents) || !red.From.Equal(r.From) {
		t.Fatalf("%d firings, %d incidents", len(red.Firings), len(red.Incidents))
	}
	for i, f := range red.Firings {
		if !f.Start.Equal(r.Firings[i].Start) || f.White != r.Firings[i].White || f.Labels["severity"] != r.Firings[i].Labels["severity"] {
			t.Errorf("firing %d: %+v, was %+v", i, f, r.Firings[i])
		}
	}
	// One name, one alias, wherever it is: the critical rule's, on its
	// firing, its catch, the near miss; the noisy one's, on its suggestion.
	alias := map[string]string{}
	for _, x := range red.Rules {
		alias[x.Severity] = x.Rule
	}
	i := slices.IndexFunc(red.Firings, func(j Judged) bool { return j.Labels["severity"] == "critical" })
	if i < 0 {
		t.Fatal("no critical firing")
	}
	crit := red.Firings[i]
	caught := red.Incidents[0].CaughtBy
	if len(caught) != 1 || caught[0].Rule != alias["critical"] || crit.Rule != alias["critical"] || red.NearMisses[0].Rule != alias["critical"] ||
		red.Suggestions[0].Rule != alias["warning"] || red.Unrecorded[0].Rule != alias["warning"] {
		t.Errorf("aliases differ from place to place: %v, %+v, %s", alias, caught, crit.Rule)
	}
	if red.Incidents[0].ID != "incident-01" || red.Incidents[1].ID != "incident-02" || red.Incidents[0].Title != "" ||
		!slices.Equal(crit.Incidents, []string{"incident-01"}) || red.NearMisses[0].Incident != "incident-02" {
		t.Errorf("incidents: %+v, the firing's %v", red.Incidents, crit.Incidents)
	}
	// A label's values by label, alike on firings and incidents, so what
	// --match agreed on is still seen to agree.
	if v := crit.Labels["service"]; !strings.HasPrefix(v, "service-") || !slices.Contains(red.Incidents[0].Labels["service"], v) {
		t.Errorf("service %q, the incident's %v", v, red.Incidents[0].Labels["service"])
	}
	seq := regexp.MustCompile(`^rule-0[1-3]$`)
	for _, rr := range red.Rules {
		if !seq.MatchString(rr.Rule) {
			t.Errorf("alias %q, want rule-01 to rule-03", rr.Rule)
		}
	}
	// Whether a replay failed, as the exit status counts it, is kept.
	for _, rr := range red.Rules {
		if rr.Unsupported != "" && (!rules.ReplayFailed(rr.Unsupported) || !strings.HasSuffix(rr.Unsupported, Redacted)) {
			t.Errorf("why not replayed: %q", rr.Unsupported)
		}
	}
	if len(red.Unreplayed) != 1 || !red.Unreplayed[0].Failed || red.NearMisses[0].Value.Of != Redacted || red.NearMisses[0].Value.New != 0.8 {
		t.Errorf("unreplayed %+v, near miss %+v", red.Unreplayed, red.NearMisses[0].Value)
	}
}

// A label named as a kind of alias has aliases of its own: a team label's
// value isn't taken for a team.
func TestRedactLabelPrefix(t *testing.T) {
	x := NewRedactor(nil)
	r := x.Report(&Report{Rules: []RuleResult{{Rule: "A", Team: "orange", Firings: 1}}, Firings: []Judged{{Firing: firing.Firing{Rule: "A",
		Labels: map[string]string{"team": "orange", "a b": "c", "service": "d"}}}}})
	l := r.Firings[0].Labels
	if r.Rules[0].Team != "team-01" || l["team"] != "label-team-01" || l["a b"] != "label-01" || l["service"] != "service-01" {
		t.Errorf("team %q, labels %v", r.Rules[0].Team, l)
	}
}

// --redact-key: a name's alias is a keyed hash, the same with the same key,
// another with another; never the name's unkeyed hash.
func TestRedactKeyed(t *testing.T) {
	r := redactedReport(t)
	key := []byte("0123456789abcdef0123456789abcdef")
	a, b := NewRedactor(key).Report(r), NewRedactor(key).Report(r)
	c := NewRedactor([]byte("another key, sixteen bytes or more")).Report(r)
	if a.Rules[0].Rule != b.Rules[0].Rule || a.Rules[0].Rule == c.Rules[0].Rule || !regexp.MustCompile(`^rule-[0-9a-f]{10}$`).MatchString(a.Rules[0].Rule) {
		t.Errorf("keyed aliases: %s %s %s", a.Rules[0].Rule, b.Rules[0].Rule, c.Rules[0].Rule)
	}
	noSecrets(t, "keyed", writeAll(t, a))
}

// A comparison, as --compare writes it in text, Markdown and JSON: the
// same alias for a rule on both sides; the recording rules, what reading
// the files passed over and the incidents caught differently, aliased.
func TestRedactComparison(t *testing.T) {
	before := redactedReport(t)
	after := redactedReport(t)
	after.Rules = slices.DeleteFunc(after.Rules, func(rr RuleResult) bool { return rr.Rule == "PaymentsQueueStuck" })
	after.Firings = slices.DeleteFunc(after.Firings, func(j Judged) bool { return j.Rule == "PaymentsQueueStuck" })
	c := Compare(before, after, []string{"PaymentsQueueStuck", "CheckoutApiErrors"}, 1)
	c.Recording = []RecordingChange{{Record: "KafkaLagRecording", Group: "recording-group", Change: "changed"}}
	c.PassedOver = map[string]string{"rules": "2 documents that hold no rules"}
	c.Summarize(true)
	c.Unreplayed = c.UnreplayedRules()
	// What --expect held them to, an incident outside the period among it.
	c.Expectations = &Checked{Weeks: 1, Unexpected: []string{"LedgerDiskFull"}, Results: []ExpectResult{
		{Rule: "PaymentsQueueStuck", What: "budget week", Limit: "5", Before: "12", After: "0", BeforeState: ExpectBroken, AfterState: ExpectAbsent, Removed: true},
		{Rule: "CheckoutApiErrors", What: "catches INC-9001", Limit: "within 5m", Before: "–", After: "–", BeforeState: ExpectUnknown, AfterState: ExpectUnknown},
		{Rule: "CheckoutApiErrors", What: "catches INC-4711", Limit: "within 5m", Before: "-5m", After: "-5m", BeforeState: ExpectMet, AfterState: ExpectMet},
	}}
	var plain bytes.Buffer
	CompareText(&plain, c, time.UTC)
	ExpectationsText(&plain, c.Expectations)
	CompareMarkdown(&plain, c, time.UTC)
	JSON(&plain, c)
	for _, s := range []string{"KafkaLagRecording", "PaymentsQueueStuck", "INC-9001"} {
		if !strings.Contains(plain.String(), s) {
			t.Fatalf("the comparison unredacted lacks %q", s)
		}
	}
	red := NewRedactor(nil).Comparison(c)
	var out bytes.Buffer
	for _, write := range []func() error{
		func() error { return CompareText(&out, red, time.UTC) },
		func() error { return ExpectationsText(&out, red.Expectations) },
		func() error { return CompareMarkdown(&out, red, time.UTC) },
		func() error { return JSON(&out, red) },
	} {
		if err := write(); err != nil {
			t.Fatal(err)
		}
	}
	noSecrets(t, "a comparison", out.String())
	if strings.Contains(out.String(), "INC-9001") {
		t.Error("an incident an expectation names is there")
	}
	ex := red.Expectations
	if ex == nil || len(ex.Results) != 3 || ex.Results[1].What != "catches incident-03" || ex.Results[2].What != "catches incident-01" ||
		ex.Results[0].Rule == "PaymentsQueueStuck" || len(ex.Unexpected) != 1 || !strings.HasPrefix(ex.Unexpected[0], "rule-") {
		t.Errorf("expectations %+v", ex)
	}
	if *red.Summary != *c.Summary || red.PassedOver["rules"] != "2 documents that hold no rules" {
		t.Errorf("summary %+v, was %+v", red.Summary, c.Summary)
	}
	for _, rc := range red.Rules {
		if rc.Before != nil && rc.Before.Rule != rc.Rule || rc.After != nil && rc.After.Rule != rc.Rule {
			t.Errorf("%s: before %v, after %v", rc.Rule, rc.Before, rc.After)
		}
	}
}
