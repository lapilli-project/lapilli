package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// sigBase is the Monday the signals' tests' period starts.
var sigBase = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)

// at is a time of the signals' tests: day d of the period (0 its Monday,
// negative before it), at hh:mm.
func at(d, hh, mm int) time.Time {
	return sigBase.Add(time.Duration(d)*24*time.Hour + time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute)
}

// on is a series that returns something from from to to, both included.
type on struct {
	labels   map[string]string
	from, to time.Time
}

// signalHistory answers each expression it knows with its series, at the
// step asked, within the span asked; count() of a selector, with a sample
// at each step from where that selector's data begins (begins; all along
// for one it doesn't name); and anything else with an error, so that a
// question nobody expected fails the test.
type signalHistory struct {
	series map[string][]on
	// begins is where each selector's data begins.
	begins map[string]time.Time
	// counted are, by selector, the label sets of its series, all along,
	// as count by (LABELS) (SELECTOR) answers: none for one not named.
	counted map[string][]map[string]string
	asked   *[]string
}

func (h signalHistory) QueryRange(_ context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if h.asked != nil {
		*h.asked = append(*h.asked, expr)
	}
	if rest, ok := strings.CutPrefix(expr, "count by ("); ok {
		by, sel, _ := strings.Cut(rest, ") (")
		sel = strings.TrimSuffix(sel, ")")
		var out []prom.Series
		for _, l := range h.counted[sel] {
			kept := map[string]string{}
			for _, k := range strings.Split(by, ", ") {
				if v, ok := l[k]; ok {
					kept[k] = v
				}
			}
			out = append(out, prom.Series{Labels: kept, Samples: []prom.Sample{{T: start, V: 1}}})
		}
		return out, nil
	}
	if sel, ok := strings.CutPrefix(expr, "count("); ok {
		sel = strings.TrimSuffix(sel, ")")
		b := h.begins[sel]
		s := prom.Series{Labels: map[string]string{}}
		for t := start; !t.After(end); t = t.Add(step) {
			if !t.Before(b) {
				s.Samples = append(s.Samples, prom.Sample{T: t, V: 1})
			}
		}
		if len(s.Samples) == 0 {
			return nil, nil
		}
		return []prom.Series{s}, nil
	}
	ons, ok := h.series[expr]
	if !ok {
		return nil, fmt.Errorf("no expression %q here", expr)
	}
	var out []prom.Series
	for _, o := range ons {
		s := prom.Series{Labels: o.labels}
		for t := start.Truncate(step); !t.After(end); t = t.Add(step) {
			if !t.Before(start) && !t.Before(o.from) && !t.After(o.to) {
				s.Samples = append(s.Samples, prom.Sample{T: t, V: 1})
			}
		}
		if len(s.Samples) > 0 {
			out = append(out, s)
		}
	}
	return out, nil
}

func lbl(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

// firingsFile writes firings, one JSON object a line: rule, labels as
// JSON, start and end (empty: still firing).
func firingsFile(t *testing.T, fs [][4]string) string {
	t.Helper()
	var b strings.Builder
	for _, f := range fs {
		b.WriteString(`{"rule":"` + f[0] + `","labels":` + f[1] + `,"start":"` + f[2] + `"`)
		if f[3] != "" {
			b.WriteString(`,"end":"` + f[3] + `"`)
		}
		b.WriteString("}\n")
	}
	return write(t, "firings.jsonl", b.String())
}

func ts(t time.Time) string { return t.Format(time.RFC3339) }

// unnamedImpact is a week of firings judged against incidents written down
// and one impact signal without a name, as lapilli verdict has judged them
// since --impact came: windows of series that overlap made one, one too
// short to count, one before the period, one that is an incident written
// down.
func unnamedImpact(t *testing.T) (signalHistory, []string) {
	t.Helper()
	h := signalHistory{series: map[string][]on{"impact": {
		{lbl("service", "checkout", "team", "shop"), at(1, 10, 0), at(1, 10, 30)},
		{lbl("service", "cart", "team", "shop"), at(1, 10, 20), at(1, 11, 0)},
		{lbl("instance", "probe-a", "job", "blackbox"), at(3, 14, 0), at(3, 14, 3)},
		{lbl("service", "search", "team", "find"), at(4, 3, 0), at(4, 3, 20)},
		{lbl("service", "payments", "team", "pay"), at(5, 20, 0), at(5, 20, 15)},
		{lbl("service", "checkout", "team", "shop"), at(-10, 10, 0), at(-10, 10, 30)},
	}}}
	incidents := write(t, "incidents.yaml", `incidents:
  - {id: INC-1, title: Search down, start: `+ts(at(4, 3, 5))+`, end: `+ts(at(4, 3, 30))+`, labels: {service: search}}
  - {id: INC-2, title: Checkout errors, start: `+ts(at(2, 8, 0))+`, end: `+ts(at(2, 8, 30))+`, labels: {service: checkout}}
`)
	co, ca := `{"service":"checkout","team":"shop","severity":"critical"}`, `{"service":"cart","team":"shop","severity":"warning"}`
	firings := firingsFile(t, [][4]string{
		{"ApiErrors", co, ts(at(1, 9, 50)), ts(at(1, 10, 40))},
		{"ApiErrors", co, ts(at(2, 8, 10)), ts(at(2, 8, 20))},
		{"ApiErrors", co, ts(at(3, 12, 0)), ts(at(3, 12, 5))},
		{"CartSlow", ca, ts(at(1, 10, 30)), ts(at(1, 10, 50))},
		{"CartSlow", ca, ts(at(4, 5, 0)), ts(at(4, 5, 10))},
		{"SearchDown", `{"service":"search","team":"find","severity":"critical"}`, ts(at(4, 3, 10)), ts(at(4, 3, 30))},
		{"PayQueue", `{"service":"payments","team":"pay"}`, ts(at(5, 21, 0)), ts(at(5, 21, 5))},
		{"BackendMissing", `{"backend":"checkout","team":"platform","severity":"warning"}`, ts(at(1, 10, 5)), ts(at(1, 10, 20))},
	})
	return h, []string{"verdict", "--firings", firings, "--incidents", incidents, "--prometheus", "http://p", "--impact", "impact",
		"--from", ts(sigBase), "--to", ts(sigBase.Add(7 * 24 * time.Hour)), "--match", "service", "--match", "team", "--tz", "UTC"}
}

// A single --impact with no name, whose data is there all along, judges
// as it did before signals had names: the report, the text, the metrics
// and the CSV are, byte for byte,
// what lapilli verdict wrote before (testdata/unnamed-impact, written by
// the release before named signals; LAPILLI_GOLDEN=update writes them
// again), but for the CSV's marked column, added at its end since
// (docs/design/marked.md), [] in lapilli verdict.
func TestUnnamedImpactUnchanged(t *testing.T) {
	h, args := unnamedImpact(t)
	var asked []string
	h.asked = &asked
	newHistory = func(context.Context, *verdictFlags) (history, error) { return h, nil }
	now = func() time.Time { return sigBase.Add(8 * 24 * time.Hour) }
	for _, out := range []struct{ file, flag string }{{"report.json", "--json"}, {"report.txt", ""}, {"report.prom", "--metrics"}, {"report.csv", "--csv"}} {
		a := args
		if out.flag != "" {
			a = append(append([]string{}, args...), out.flag)
		}
		var stdout, stderr bytes.Buffer
		if code := run(a, &stdout, &stderr); code != 0 {
			t.Fatalf("%s: exit %d\n%s", out.file, code, stderr.String())
		}
		path := filepath.Join("testdata", "unnamed-impact", out.file)
		if os.Getenv("LAPILLI_GOLDEN") == "update" {
			os.MkdirAll(filepath.Dir(path), 0o755)
			if err := os.WriteFile(path, stdout.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := stdout.String(); got != string(want) {
			t.Errorf("%s differs from what an unnamed --impact gave before:\n%s\nwant\n%s", out.file, got, want)
		}
	}
	// One question of the signal a run, and one of where its data begins,
	// all along here: the record from where it was first read, as before.
	for i, q := range asked {
		if want := []string{"impact", "count(impact)"}[i%2]; q != want {
			t.Errorf("asked %q, want %q", q, want)
		}
	}
	if len(asked) != 8 {
		t.Errorf("asked %d times, want twice a run", len(asked))
	}
}

// The design's signals (docs/design/signals.md): errors, by service, team
// and backend; probe, matched on service alone, whose data begins on the
// Thursday; latency, which finds nothing; payments, matched on no labels,
// which doesn't keep the record. And three rule entries.
const (
	errorsExpr   = `sum by (service, team, backend) (rate(http_requests_total{code=~"5.."}[5m])) / sum by (service, team, backend) (rate(http_requests_total[5m])) > 0.05`
	probeExpr    = `probe_success == 0`
	latencyExpr  = `histogram_quantile(0.99, sum by (le, service, team) (rate(http_request_duration_seconds_bucket[5m]))) > 1`
	paymentsExpr = `sum(rate(payments_succeeded_total[5m])) == 0 and sum(rate(payments_tried_total[5m])) > 0`
)

// namedSignals is a week judged against named signals: the history, the
// impact file, and the flags of a run of lapilli verdict but its signals.
func namedSignals(t *testing.T) (signalHistory, string, []string) {
	t.Helper()
	h := signalHistory{
		series: map[string][]on{
			errorsExpr: {
				{lbl("service", "checkout", "team", "shop", "backend", "checkout-v2"), at(1, 10, 0), at(1, 10, 30)},
				{lbl("service", "search", "team", "find", "backend", "search-v1"), at(4, 3, 0), at(4, 3, 20)},
				{lbl("service", "checkout", "team", "shop", "backend", "checkout-v2"), at(4, 9, 0), at(4, 9, 20)},
				// In the two weeks before the period alone: no window, but cart
				// is a value it returned.
				{lbl("service", "cart", "team", "shop", "backend", "cart-v1"), at(-5, 10, 0), at(-5, 10, 20)},
			},
			probeExpr: {
				{lbl("service", "checkout", "team", "probers", "instance", "https://checkout.example", "job", "blackbox"), at(4, 9, 2), at(4, 9, 6)},
			},
			latencyExpr:  {},
			paymentsExpr: {{lbl(), at(6, 20, 0), at(6, 20, 3)}},
		},
		begins: map[string]time.Time{"probe_success": at(3, 7, 0)},
	}
	file := write(t, "impact.yaml", `signals:
  - {name: errors, expr: '`+errorsExpr+`', for: 5m}
  - {name: probe, expr: "`+probeExpr+`", for: 2m, match: [service]}
  - {name: latency, expr: "`+latencyExpr+`", for: 10m}
  - {name: payments, expr: "`+paymentsExpr+`", for: 1m, match: [], record: false}
rules:
  - rules: ^QueueLatency$
    signals: [payments]
  - rules: Latency$
    labels: {kind: latency}
    signals: [latency]
  - rules: ^BackendMissing$
    signals: [errors, probe]
    match: [backend]
`)
	incidents := write(t, "incidents.yaml", `incidents:
  - {id: INC-1, title: Edge errors, start: `+ts(at(6, 1, 0))+`, end: `+ts(at(6, 1, 30))+`, labels: {service: web}}
`)
	co := `{"service":"checkout","team":"shop","severity":"critical"}`
	firings := firingsFile(t, [][4]string{
		{"ApiErrors", co, ts(at(0, 12, 0)), ts(at(0, 12, 5))},                                    // tied to probe too: waits for it
		{"SearchSlow", `{"service":"search","team":"find"}`, ts(at(0, 13, 0)), ts(at(0, 13, 5))}, // errors alone: judged
		{"CartSlow", `{"service":"cart","team":"shop"}`, ts(at(0, 14, 0)), ts(at(0, 14, 5))},     // errors returned cart before the period
		{"NightPage", `{"service":"web","team":"edge"}`, ts(at(1, 0, 0)), ts(at(1, 0, 5))},       // no signal had web: waits for them all
		{"ApiErrors", co, ts(at(1, 9, 55)), ts(at(1, 10, 40))},
		{"BackendMissing", `{"backend":"checkout-v2","team":"platform"}`, ts(at(4, 8, 50)), ts(at(4, 9, 30))},
		{"CheckoutLatency", `{"service":"checkout","team":"shop","kind":"latency"}`, ts(at(4, 9, 1)), ts(at(4, 9, 20))},
		// Its entry by the labels all its firings have: latency's alone.
		{"CheckoutP99", `{"service":"checkout","team":"shop","kind":"latency","pod":"a"}`, ts(at(4, 9, 4)), ts(at(4, 9, 20))},
		{"CheckoutP99", `{"service":"checkout","team":"shop","kind":"latency","pod":"b"}`, ts(at(5, 9, 4)), ts(at(5, 9, 20))},
		{"ApiErrors", co, ts(at(4, 9, 3)), ts(at(4, 9, 30))},
		{"QueueLatency", `{"service":"payments","team":"pay"}`, ts(at(0, 5, 0)), ts(at(0, 5, 5))}, // payments alone: no record
		{"QueueLatency", `{"service":"payments","team":"pay"}`, ts(at(6, 2, 0)), ts(at(6, 2, 5))}, // after INC-1
		{"PayQueue", `{"service":"payments","team":"pay"}`, ts(at(6, 20, 1)), ts(at(6, 20, 10))},
	})
	return h, file, []string{"verdict", "--firings", firings, "--incidents", incidents, "--prometheus", "http://p",
		"--from", ts(sigBase), "--to", ts(sigBase.Add(7 * 24 * time.Hour)), "--match", "service", "--match", "team", "--tz", "UTC"}
}

func runSignals(t *testing.T, h signalHistory, args ...string) string {
	t.Helper()
	newHistory = func(context.Context, *verdictFlags) (history, error) { return h, nil }
	now = func() time.Time { return sigBase.Add(8 * 24 * time.Hour) }
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	return stdout.String()
}

// Named signals judged as the design says: each signal's windows its own,
// never merged with another's at the same time, matched on its own labels;
// a rule entry keeping its rules from other signals' windows, and matching
// them on its labels; each firing judged by the signals that could have
// seen it, from each one's start, the probe's where its data begins.
func TestNamedSignals(t *testing.T) {
	h, file, args := namedSignals(t)
	var asked []string
	h.asked = &asked
	out := runSignals(t, h, append(args, "--impact-file", file, "--json")...)
	var rep verdict.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	ids := map[string]verdict.IncidentResult{}
	for _, in := range rep.Incidents {
		ids[in.ID] = in
	}
	caught := func(id string) []string {
		var by []string
		for _, c := range ids[id].CaughtBy {
			by = append(by, c.Rule)
		}
		return by
	}
	for id, want := range map[string][]string{
		"impact-errors-1": {"ApiErrors"}, "impact-errors-2": nil, "impact-errors-3": {"BackendMissing", "ApiErrors"},
		"impact-probe-1": {"BackendMissing", "ApiErrors"}, "impact-payments-1": {"PayQueue"}, "INC-1": nil,
	} {
		if _, ok := ids[id]; !ok {
			t.Errorf("no incident %s: %v", id, slices.Collect(maps.Keys(ids)))
		}
		if got := caught(id); !slices.Equal(got, want) {
			t.Errorf("%s caught by %v, want %v", id, got, want)
		}
	}
	if len(rep.Incidents) != 6 || ids["impact-probe-1"].Signal != "probe" || ids["INC-1"].Signal != "" {
		t.Errorf("%d incidents: %+v", len(rep.Incidents), rep.Incidents)
	}
	judged := map[string][]bool{}
	for _, f := range rep.Firings {
		judged[f.Rule] = append(judged[f.Rule], !f.NoRecord)
	}
	for rule, want := range map[string][]bool{
		"ApiErrors": {false, true, true}, "SearchSlow": {true}, "CartSlow": {true}, "NightPage": {false}, "BackendMissing": {true},
		"CheckoutLatency": {true}, "CheckoutP99": {true, true}, "QueueLatency": {false, true}, "PayQueue": {true},
	} {
		if !slices.Equal(judged[rule], want) {
			t.Errorf("%s judged %v, want %v", rule, judged[rule], want)
		}
	}
	if rep.Unjudged != 3 || rep.NoRecord {
		t.Errorf("%d unjudged, no record %v", rep.Unjudged, rep.NoRecord)
	}
	starts := map[string]time.Time{}
	for _, s := range rep.Settings.ImpactSignals {
		starts[s.Name] = s.Start
	}
	readFrom := sigBase.Add(-verdict.RecordLookback)
	if !starts["errors"].Equal(readFrom) || !starts["probe"].Equal(at(3, 7, 0)) || !starts["latency"].Equal(readFrom) || !rep.Settings.ImpactFrom.IsZero() {
		t.Errorf("starts %v, impact_from %v", starts, rep.Settings.ImpactFrom)
	}
	entries := map[string]int{}
	for _, r := range rep.Rules {
		entries[r.Rule] = r.ImpactRule
	}
	if entries["QueueLatency"] != 1 || entries["CheckoutLatency"] != 2 || entries["CheckoutP99"] != 2 || entries["BackendMissing"] != 3 || entries["ApiErrors"] != 0 || entries["PayQueue"] != 0 {
		t.Errorf("entries %v", entries)
	}
	if len(rep.Settings.ImpactRules) != 3 || rep.Settings.ImpactRules[2].Match[0] != "backend" {
		t.Errorf("rule entries %+v", rep.Settings.ImpactRules)
	}
	// Each signal read once, and where each selector's data begins once.
	count := map[string]int{}
	for _, q := range asked {
		count[q]++
	}
	// And the errors' 5xx selector's stand-in: the requests with a code.
	for _, q := range []string{errorsExpr, probeExpr, latencyExpr, paymentsExpr, "count(probe_success)", `count(http_requests_total{code=~"5.."})`, `count(http_requests_total{code!=""})`} {
		if count[q] != 1 {
			t.Errorf("asked %q %d times", q, count[q])
		}
	}
	// What each reads, by its match labels, one request a selector, the
	// 5xx one through its stand-in: none of payments', matched on none.
	for _, q := range []string{`count by (service, team) (http_requests_total{code!=""})`, "count by (service, team) (http_requests_total)",
		"count by (service) (probe_success)", "count by (service, team) (http_request_duration_seconds_bucket)"} {
		if count[q] != 1 {
			t.Errorf("asked %q %d times", q, count[q])
		}
	}
	if len(asked) != 15 || count[`count by (service, team) (http_requests_total{code=~"5.."})`] != 0 {
		t.Errorf("%d questions: %q", len(asked), asked)
	}

	// The text says each signal's windows, and from when each keeps the
	// record; the metrics, each signal's windows by state, and its start.
	text := runSignals(t, h, append(args, "--impact-file", file)...)
	for _, want := range []string{"5 more found by --impact (errors 3, probe 1, payments 1)",
		"Impact signals keep the record of the firings they could have seen: errors from 2026-09-21 00:00, probe from 2026-10-08 07:00, latency from 2026-09-21 00:00, payments doesn't (record: no)",
		"impact-probe-1 user impact (probe)", fmt.Sprintf(verdict.SomeUnjudgedSignalsText, 3, 13)} {
		if !strings.Contains(text, want) {
			t.Errorf("the text has no %q\n%s", want, text)
		}
	}
	metrics := runSignals(t, h, append(args, "--impact-file", file, "--metrics")...)
	for _, want := range []string{
		`lapilli_impact_windows{signal="errors",state="caught"} 2`, `lapilli_impact_windows{signal="errors",state="missed"} 1`,
		`lapilli_impact_windows{signal="probe",state="caught"} 1`, `lapilli_impact_windows{signal="latency",state="missed"} 0`,
		`lapilli_impact_windows{signal="payments",state="caught"} 1`,
		fmt.Sprintf(`lapilli_impact_signal_start_timestamp_seconds{signal="probe"} %d`, at(3, 7, 0).Unix()),
		`lapilli_incidents{state="caught",source="impact"} 4`, `lapilli_incidents{state="missed",source="written"} 1`,
	} {
		if !strings.Contains(metrics, want+"\n") {
			t.Errorf("--metrics has no %s", want)
		}
	}
	if strings.Contains(metrics, `lapilli_impact_signal_start_timestamp_seconds{signal="payments"}`) {
		t.Error("payments keeps no record, and has a start")
	}
}

// The flags say what the file says, but the rule entries: the same signals
// judge the same.
func TestNamedSignalsFlags(t *testing.T) {
	h, _, args := namedSignals(t)
	file := write(t, "impact.yaml", `impact:
  signals:
    - {name: errors, expr: '`+errorsExpr+`'}
    - {name: probe, expr: "`+probeExpr+`", for: 2m, match: [service]}
    - {name: payments, expr: "`+paymentsExpr+`", for: 1m, match: [], record: false}
`)
	fromFile := runSignals(t, h, append(args, "--impact-file", file, "--json")...)
	fromFlags := runSignals(t, h, append(args, "--impact", "errors="+errorsExpr, "--impact", "probe="+probeExpr, "--impact-for", "probe=2m",
		"--impact-match", "probe=service", "--impact", "payments="+paymentsExpr, "--impact-for", "payments=1m", "--impact-match", "payments=",
		"--impact-record", "payments=no", "--json")...)
	if fromFile != fromFlags {
		t.Errorf("the flags judged otherwise than the file:\n%s\nfile:\n%s", fromFlags, fromFile)
	}
	if !strings.Contains(fromFlags, `"impact_signals"`) || strings.Contains(fromFlags, `"impact_rules"`) {
		t.Errorf("settings:\n%s", fromFlags)
	}
}

// What the flags and the file are refused for.
func TestSignalFlagsRefused(t *testing.T) {
	big := write(t, "big.yaml", "signals:\n  - {name: a, expr: x}\n#"+strings.Repeat("x", 1<<20)+"\n")
	file := func(body string) string { return write(t, "impact.yaml", body) }
	nine := "signals:\n"
	for i := range 9 {
		nine += fmt.Sprintf("  - {name: s%d, expr: up}\n", i)
	}
	base := []string{"verdict", "--firings", write(t, "f.jsonl", ""), "--prometheus", "http://p"}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--impact", "a", "--impact", "b"}, "--impact is given twice without a name"},
		{[]string{"--impact", "errors=up", "--impact", "up == 0"}, "a signal with no name and named ones don't go together"},
		{[]string{"--impact", "errors=up", "--impact", "errors=down"}, "the signal errors is named twice"},
		{[]string{"--impact", "Errors=up"}, "a signal's name is a lowercase letter"},
		{[]string{"--impact", strings.Repeat("a", 33) + "=up"}, "32 bytes at most"},
		{[]string{"--impact", "errors="}, "no expression after the name"},
		{[]string{"--impact", "errors=up", "--impact-match", "probe=service"}, "name probe, which no --impact NAME=PROMQL gives"},
		{[]string{"--impact", "errors=up", "--impact-for", "errors=10s"}, "want from --step (1m) to 1d"},
		{[]string{"--impact", "errors=up", "--impact-for", "errors=2d"}, "want from --step (1m) to 1d"},
		{[]string{"--impact", "errors=up", "--impact-record", "errors=maybe"}, "want NAME=yes or NAME=no"},
		{[]string{"--impact", "errors=up", "--impact-match", "errors=service,9team"}, `"9team" isn't a label's name`},
		{[]string{"--impact", "errors=up", "--impact-match", "errors=a", "--impact-match", "errors=b"}, "given twice"},
		{[]string{"--impact", "errors=sum(rate(x[5m]"}, "--impact errors: the impact expression doesn't read as PromQL"},
		{[]string{"--impact-match", "service"}, "want NAME=..."},
		{[]string{"--impact-file", file("signals: [{name: a, expr: up, colour: red}]")}, "field colour not found"},
		{[]string{"--impact-file", file("signals: [{name: a, expr: up}, {name: a, expr: down}]")}, "a is named twice"},
		{[]string{"--impact-file", file("signals: [{name: a, expr: up}]\nrules: [{rules: x, signals: [b]}]")}, `the signal "b" isn't one the file declares`},
		{[]string{"--impact-file", file("signals: [{name: a, expr: up}]\nrules: [{signals: [a]}]")}, "names no rule"},
		{[]string{"--impact-file", file("signals: [{name: a, expr: up}]\nrules: [{rules: '(ab|cd){600}', signals: [a]}]")}, "too complex"},
		{[]string{"--impact-file", file("signals: [{name: a, expr: up}]\nrules: [{rules: '" + strings.Repeat("x", 201) + "', signals: [a]}]")}, "200 bytes at most"},
		{[]string{"--impact-file", file(nine)}, "9 signals: 8 at most"},
		{[]string{"--impact-file", big}, "more than 1 MiB"},
		{[]string{"--impact-file", file("rules: []")}, "no signals"},
		{[]string{"--impact-file", file("signals: [{name: a, expr: up}]\n---\nsignals: [{name: b, expr: up}]\n")}, "more than one YAML document"},
		{[]string{"--impact-file", file("signals: [{name: a, expr: up}]\n---\n")}, "more than one YAML document"},
		{[]string{"--impact-file", file("signals: [{name: a, expr: up}]"), "--impact-file", file("signals: [{name: b, expr: up}]")}, "--impact-file is given twice"},
		{[]string{"--impact-file", file("signals: [{name: a, expr: up}]"), "--impact", "b=up"}, "--impact-file gives the impact signals"},
		{[]string{"--impact-file", file("signals: [{name: a, expr: up}]"), "--impact-record", "a=no"}, "leave out --impact-for NAME=DUR"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(append(slices.Clone(base), c.args...), &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), c.want) {
			t.Errorf("%q: exit %d, want 2 and %q\n%s", c.args, code, c.want, stderr.String())
		}
	}
	// No PromQL starts NAME=: these are expressions with no name.
	for _, expr := range []string{"up==0", "up>=1", "up =~ 1", `{__name__="up"}`, "rate(x[5m]) > 1"} {
		if name, e, err := splitSignal(expr); err != nil || name != "" || e != expr {
			t.Errorf("%s: %q %q %v", expr, name, e, err)
		}
	}
	if name, e, err := splitSignal("errors=rate(x[5m]) > 1"); err != nil || name != "errors" || e != "rate(x[5m]) > 1" {
		t.Errorf("errors=...: %q %q %v", name, e, err)
	}
	// lapilli serve takes named signals as lapilli verdict does
	// (docs/design/signals.md, step 2), and refuses what it refuses: named
	// and unnamed ones together, two with no name, a flag naming none.
	serving := func(args ...string) error {
		_, err := parseFlags(append([]string{"--rules", "x.yaml", "--prometheus", "http://p"}, args...), func(string, string) (bool, error) { return false, nil })
		return err
	}
	for _, args := range [][]string{{"--impact", "errors=up"}, {"--impact-file", file("signals: [{name: a, expr: up}]")}, {"--impact", "a=up", "--impact-for", "a=2m"}} {
		if err := serving(args...); err != nil {
			t.Errorf("serve %q: %v", args, err)
		}
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--impact", "up", "--impact", "errors=up"}, "a signal with no name and named ones don't go together"},
		{[]string{"--impact", "up", "--impact", "down"}, "--impact is given twice without a name"},
		{[]string{"--impact", "up", "--impact-for", "a=2m"}, "which no --impact NAME=PROMQL gives"},
	} {
		if err := serving(c.args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("serve %q: %v, want %q", c.args, err, c.want)
		}
	}
}

// docs/numbers.md's numbers, counted again from a report judged by named
// signals and rule entries, come out as the report has them: a window
// matched on its signal's labels, a rule kept from other signals' windows
// and matched on its entry's, a firing judged by its signals' starts and
// their windows.
func TestNumbersRecountSignals(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("no jq")
	}
	h, file, args := namedSignals(t)
	report := filepath.Join(t.TempDir(), "report.json")
	os.WriteFile(report, []byte(runSignals(t, h, append(args, "--impact-file", file, "--json")...)), 0o644)
	rs := recounts(t, "../../docs/numbers.md")
	reported := map[string]string{}
	for _, r := range rs {
		if r.as == "as the report has it" && !strings.Contains(r.name, "(lapilli serve)") {
			reported[r.name] = jq(t, r.prog, report)
		}
	}
	compared := 0
	for _, r := range rs {
		want, ok := reported[r.name]
		if r.as != "counted again" || !ok {
			continue
		}
		if got := jq(t, r.prog, report); got != want {
			t.Errorf("%s: counted again %s, the report has %s", r.name, got, want)
		}
		compared++
	}
	if compared < 10 {
		t.Errorf("%d numbers counted again", compared)
	}
	// What the blocks would count without the signals' own match, the
	// entries or the ties: not what the report has.
	for _, c := range []struct{ name, old, new string }{
		{"incidents caught", "$sig[$in.signal].match // $s.match", "$s.match"},
		{"incidents caught", "$entry[$f.rule] as $e", "null as $e"},
		{"no record (lapilli verdict)", "(.signals // [])", "[]"},
	} {
		var prog string
		for _, r := range rs {
			if r.name == c.name && r.as == "counted again" {
				prog = r.prog
			}
		}
		plain := strings.Replace(prog, c.old, c.new, 1)
		if prog == plain {
			t.Fatalf("%s: the block has no %s", c.name, c.old)
		}
		if jq(t, plain, report) == reported[c.name] {
			t.Errorf("%s, with %s for %s: the same, a test of nothing", c.name, c.new, c.old)
		}
	}
}

// --compare finds each side's rule entries by its own rules' labels: a
// change that gives a rule a label an entry names keeps it from the
// windows of the signals the entry doesn't name.
func TestCompareSignals(t *testing.T) {
	newHistory = func(context.Context, *verdictFlags) (history, error) { return fakeHistory{}, nil }
	now = func() time.Time { return t0 }
	base := `groups:
  - name: shop
    rules:
      - alert: CheckoutErrors
        expr: errors > 5
        for: 5m
`
	inPlace := write(t, "base.yaml", base)
	labelled := write(t, "labelled.yaml", strings.Replace(base, "  - name: shop\n", "  - name: shop\n    labels: {kind: latency}\n", 1))
	file := write(t, "impact.yaml", `signals:
  - {name: errors, expr: "errors > 5"}
  - {name: slow, expr: impact}
rules:
  - {labels: {kind: latency}, signals: [slow]}
`)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"verdict", "--rules", labelled, "--compare", inPlace, "--prometheus", "http://p", "--impact-file", file, "--since", "12h", "--json"},
		&stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	var c verdict.Comparison
	if err := json.Unmarshal(stdout.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	caught := func(r *verdict.Report) (n int) {
		for _, in := range r.Incidents {
			if in.Signal == "errors" && !in.Missed() {
				n++
			}
		}
		return n
	}
	if caught(c.Before) != 1 || caught(c.After) != 0 || c.After.Rules[0].ImpactRule != 1 || c.Before.Rules[0].ImpactRule != 0 {
		t.Errorf("before %d caught, after %d; entries %d, %d", caught(c.Before), caught(c.After), c.Before.Rules[0].ImpactRule, c.After.Rules[0].ImpactRule)
	}
}

// --redact gives each signal's name an alias, in the report, the text and
// the metrics, says a rule entry's pattern is redacted, and gives its
// labels' values aliases: a signal is named for what it measures.
func TestRedactSignals(t *testing.T) {
	h, file, args := namedSignals(t)
	for _, flag := range []string{"--json", "--metrics", ""} {
		a := append(slices.Clone(args), "--impact-file", file, "--redact")
		if flag != "" {
			a = append(a, flag)
		}
		out := runSignals(t, h, a...)
		for _, name := range []string{"errors", "probe", "latency", "payments", "Latency$", "BackendMissing", `"latency"`} {
			if strings.Contains(out, name) {
				t.Errorf("%s: %q is there", flag, name)
			}
		}
		if !strings.Contains(out, "signal-") {
			t.Errorf("%s: no signal's alias\n%s", flag, out)
		}
	}
}

// Impact as short as a named signal's "for" is said of that signal, by
// name, and of the rules judged against its windows alone: --impact-for
// with no name sets no named signal's.
func TestImpactShortSignal(t *testing.T) {
	newHistory = func(context.Context, *verdictFlags) (history, error) { return fakeHistory{}, nil }
	now = func() time.Time { return t0 }
	rulesFile := write(t, "rules.yaml", `groups:
  - name: shop
    rules:
      - alert: CheckoutErrors
        expr: errors > 5
        for: 5m
      - alert: Flappy
        expr: flappy
        for: 5m
`)
	file := write(t, "impact.yaml", `signals:
  - {name: errors, expr: "errors > 5", for: 10m}
  - {name: probe, expr: impact, for: 2m}
rules:
  - {rules: ^Flappy$, signals: [errors]}
`)
	args := []string{"verdict", "--rules", rulesFile, "--prometheus", "http://p", "--impact-file", file, "--since", "12h", "--tz", "UTC"}
	var stdout, stderr bytes.Buffer
	if code := run(append(slices.Clone(args), "--json"), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	var rep verdict.Report
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if s := rep.ImpactShort; s == nil || s.Signal != "probe" || s.For != 2*time.Minute || s.Rules != 1 || s.Judged != 1 || s.Need != 6*time.Minute {
		t.Fatalf("impact short %+v", rep.ImpactShort)
	}
	stdout.Reset()
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	for _, want := range []string{"! --impact-for probe=2m counts impact that lasted 2m as an incident, but 1 of the 1 rules judged",
		"--impact-for probe=6m counts only what they can."} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("no %q in\n%s", want, stdout.String())
		}
	}
}

// A rule's entry by labels is found by the newest version of the rules
// that has it; a rule with none, by the labels all its firings have.
func TestIndexSignals(t *testing.T) {
	sig := verdict.NewSignals([]verdict.Signal{{Name: "latency", Record: true}},
		[]verdict.RuleEntry{{Labels: incident.Labels{"kind": {"latency"}}, Signals: []string{"latency"}}})
	versions := []replay.Version{
		{Rules: []rules.Rule{{Name: "Slow", Labels: map[string]string{"kind": "queue"}}, {Name: "Gone", Labels: map[string]string{"kind": "latency"}}}},
		{From: sigBase, Rules: []rules.Rule{{Name: "Slow", Labels: map[string]string{"kind": "latency"}}}},
	}
	fs := []firing.Firing{
		{Rule: "Recorded", Labels: map[string]string{"kind": "latency", "pod": "a"}},
		{Rule: "Recorded", Labels: map[string]string{"kind": "latency", "pod": "b"}},
		{Rule: "Mixed", Labels: map[string]string{"kind": "latency"}},
		{Rule: "Mixed", Labels: map[string]string{"kind": "queue"}},
		{Rule: "Half", Labels: map[string]string{"kind": "latency"}},
		{Rule: "Half", Labels: map[string]string{}},
	}
	s := indexSignals(sig, versions, fs)
	for rule, want := range map[string]int{"Slow": 1, "Gone": 1, "Recorded": 1, "Mixed": 0, "Half": 0} {
		if got := s.EntryNumber(rule); got != want {
			t.Errorf("%s's entry %d, want %d", rule, got, want)
		}
	}
}

// An --impact with no name keeps the record by the one rule a named
// signal does: from the latest of where its selectors' data begins; a
// selector with no data that only narrows one with data (the 5xx, of a
// service that served none) from where its stand-in's does, the series
// with the label it narrows by; not one an absent() reads.
func TestUnnamedImpactSelectors(t *testing.T) {
	const ratio = `sum(rate(http_requests_total{code=~"5.."}[5m])) / sum(rate(http_requests_total[5m])) > 0.05`
	// Everyone's 5xx with a route: everyone's 5xx's stand-in for the route's,
	// with data only once a 5xx came.
	const payFive, allFive, allFiveRouted = `x{code=~"5..",route="/pay"}`, `x{code=~"5.."}`, `x{code=~"5..",route!=""}`
	const payAndAll = `sum(rate(x{code=~"5..",route="/pay"}[5m])) / sum(rate(x{route="/pay"}[5m])) > 0.1 or sum(rate(x{code=~"5.."}[5m])) / sum(rate(x[5m])) > 0.05`
	const payAlone = `sum(rate(x{code=~"5..",route="/pay"}[5m])) / sum(rate(x{route="/pay"}[5m])) > 0.1`
	never := sigBase.Add(365 * 24 * time.Hour)
	readFrom := sigBase.Add(-verdict.RecordLookback)
	for _, c := range []struct {
		name, expr string
		begins     map[string]time.Time
		from       time.Time // zero: no data
	}{
		{"no 5xx", ratio, map[string]time.Time{`http_requests_total{code=~"5.."}`: never}, readFrom},
		{"and a probe from Thursday", ratio + ` or probe_success == 0`, map[string]time.Time{`http_requests_total{code=~"5.."}`: never, "probe_success": at(3, 7, 0)}, at(3, 7, 0)},
		{"requests from Tuesday", ratio, map[string]time.Time{`http_requests_total{code=~"5.."}`: never, "http_requests_total": at(1, 0, 0)}, at(1, 0, 0)},
		{"5xx alone, none", `sum(rate(http_requests_total{code=~"5.."}[5m])) > 1`, map[string]time.Time{`http_requests_total{code=~"5.."}`: never}, time.Time{}},
		// The code label from Wednesday on (a relabelling): nothing could
		// be matched before.
		{"the code label later", ratio, map[string]time.Time{`http_requests_total{code=~"5.."}`: never, `http_requests_total{code!=""}`: at(2, 0, 0)}, at(2, 0, 0)},
		{"absent", `absent(up{job="new"}) and on() count(up) > 0`, map[string]time.Time{`up{job="new"}`: never}, time.Time{}},
		// A route's 5xx and everyone's, with none anywhere: kept from the
		// first, though everyone's 5xx stand-in for the route's has none.
		{"a route's and everyone's, no 5xx", payAndAll, map[string]time.Time{payFive: never, allFive: never, allFiveRouted: never}, readFrom},
		// Everyone's first 5xx on Wednesday, of another route: kept from
		// the first, as everyone's requests were read with their codes.
		{"a route's and everyone's, a 5xx Wednesday", payAndAll, map[string]time.Time{payFive: never, allFive: at(2, 0, 0), allFiveRouted: at(2, 0, 0)}, readFrom},
		// The route from Tuesday on, read beside everyone's requests: kept
		// from the first, as everyone's were read.
		{"a route from Tuesday, beside everyone's", payAndAll, map[string]time.Time{payFive: never, allFive: never, allFiveRouted: never, `x{route="/pay"}`: at(1, 0, 0), `x{route="/pay",code!=""}`: at(1, 0, 0)}, readFrom},
		// Read alone: from Tuesday, where the route's requests begin.
		{"a route alone from Tuesday", payAlone, map[string]time.Time{payFive: never, `x{route="/pay"}`: at(1, 0, 0), `x{route="/pay",code!=""}`: at(1, 0, 0)}, at(1, 0, 0)},
	} {
		h := signalHistory{series: map[string][]on{c.expr: {}}, begins: c.begins}
		_, args := unnamedImpact(t)
		for i, a := range args {
			if a == "--impact" {
				args[i+1] = c.expr
			}
		}
		newHistory = func(context.Context, *verdictFlags) (history, error) { return h, nil }
		now = func() time.Time { return sigBase.Add(8 * 24 * time.Hour) }
		var stdout, stderr bytes.Buffer
		if code := run(append(args, "--json"), &stdout, &stderr); code != 0 {
			t.Fatalf("%s: exit %d\n%s", c.name, code, stderr.String())
		}
		var rep verdict.Report
		if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
			t.Fatal(err)
		}
		if !rep.Settings.ImpactFrom.Equal(c.from) || rep.Settings.ImpactNoData != c.from.IsZero() {
			t.Errorf("%s: impact_from %v, no data %v; want %v", c.name, rep.Settings.ImpactFrom, rep.Settings.ImpactNoData, c.from)
		}
	}
}

// What a signal reads ties a firing to it, as what it returned does
// (docs/design/signals.md, "The record, signal by signal"): errors reads
// web's requests, which it never returned, as nothing hurt web, so a firing
// of web is tied to it, and judged from its start, not held back to the
// latest start of all; a selector that only narrows another is counted
// through its stand-in, so a 5xx it never saw ties nothing apart.
func TestNamedSignalsSeenFromReads(t *testing.T) {
	night := func(h signalHistory, file string, args []string) verdict.Judged {
		t.Helper()
		var rep verdict.Report
		if err := json.Unmarshal([]byte(runSignals(t, h, append(args, "--impact-file", file, "--json")...)), &rep); err != nil {
			t.Fatal(err)
		}
		for _, f := range rep.Firings {
			if f.Rule == "NightPage" {
				return f
			}
		}
		t.Fatal("no NightPage firing")
		return verdict.Judged{}
	}
	h, file, args := namedSignals(t)
	// Payments, matched on no label, ties every firing, and keeps no
	// record: by it alone, web falls back to every signal that does.
	all := []string{"errors", "probe", "latency", "payments"}
	if f := night(h, file, args); !f.NoRecord || !slices.Equal(f.Signals, all) {
		t.Fatalf("returned alone: no record %v, tied to %v; want waiting for them all", f.NoRecord, f.Signals)
	}
	h.counted = map[string][]map[string]string{"http_requests_total": {{"service": "web", "team": "edge"}}}
	if f := night(h, file, args); f.NoRecord || !slices.Equal(f.Signals, []string{"errors", "payments"}) {
		t.Errorf("read: no record %v, tied to %v; want judged, by errors", f.NoRecord, f.Signals)
	}
	// Counted through the 5xx selector alone, web would be a service that
	// had a 5xx: the stand-in is what is counted, not it.
	h.counted = map[string][]map[string]string{`http_requests_total{code=~"5.."}`: {{"service": "web", "team": "edge"}}}
	if f := night(h, file, args); !slices.Equal(f.Signals, all) {
		t.Errorf("by the 5xx selector: tied to %v; it isn't counted", f.Signals)
	}
}
