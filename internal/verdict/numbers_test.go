package verdict

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
)

// stableFields are the report's fields docs/numbers.md promises keep their
// names, by where they are: renaming one fails here first.
var stableFields = map[string][]string{
	"":                                {"from", "to", "noisy_at", "no_record", "unjudged", "some_rules", "rules", "incidents", "firings", "settings", "responses", "notifications"},
	"noisy_at":                        {"firings", "precision"},
	"settings":                        {"lead_ns", "match", "linked_only", "refire_ns", "unknown_length_ns", "record_lookback_ns", "impact_from", "impact_no_data", "record_by_week", "impact_signals", "impact_rules", "zone", "noisy_by", "routine"},
	"settings.impact_signals[]":       {"name", "for_ns", "match", "record", "start", "lead_ns"},
	"settings.impact_rules[]":         {"rules", "labels", "signals", "match", "lead_ns", "require", "judged"},
	"rules[]":                         {"rule", "severity", "firings", "white", "black", "refired", "noisy", "unjudged", "no_record", "replayed", "unsupported", "incidents", "first", "alone", "spare", "off_hours", "off_hours_black", "asleep", "asleep_black", "excused", "during_deploys", "during_deploys_caught", "during_deploys_unjudged", "impact_rule", "marked", "episodes", "unrelated", "action_yes", "action_no", "not_judged", "responses", "notifications"},
	"rules[].notifications":           {"notified", "paged", "paged_asleep", "paged_off_hours", "paged_for_nothing", "paged_for_nothing_asleep", "paged_for_nothing_off_hours", "inhibited", "inhibited_by", "grouped"},
	"rules[].responses":               {"changed", "routine", "acknowledged", "unanswered", "none", "useful", "not_useful", "actionable"},
	"rules[].marked.*":                {"firings", "caught", "refired", "unjudged"},
	"rules[].episodes":                {"total", "white", "unjudged", "noisy"},
	"incidents[]":                     {"id", "title", "start", "end", "labels", "alerts", "candidate", "caught_by", "signal", "real", "not_known"},
	"incidents[].alerts[]":            {"rule", "labels"},
	"incidents[].caught_by[]":         {"rule", "delay_ns", "noisy", "linked"},
	"firings[]":                       {"rule", "labels", "start", "end", "white", "incidents", "no_record", "refired", "during_deploy", "signals", "marked", "episode", "day", "unrelated", "action", "not_judged", "response", "notifications"},
	"notifications":                   {"worked_out", "notified", "paged", "groups", "receivers", "stopped"},
	"notifications.receivers[]":       {"name", "kinds", "paging", "why"},
	"firings[].notifications":         {"notified", "paged", "first_page", "receivers", "no_integrations", "muted"},
	"firings[].notifications.muted[]": {"kind", "by", "source_rule"},
	"responses":                       {"pages", "pages_unmatched", "changes", "autoscalers_known"},
	"firings[].response":              {"kind", "useful", "change", "page"},
	"firings[].response.change":       {"kind", "at", "labels", "routine"},
	"firings[].response.page":         {"id", "sent", "acknowledged", "resolved", "resolved_by"},
}

// A report with every field docs/numbers.md promises set, none left out as
// empty.
func fullReport() *Report {
	t0 := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	lead, no, yes, third := 5*time.Minute, false, true, 1.0/3
	return &Report{
		From: t0, To: t0.Add(7 * 24 * time.Hour), NoisyAt: NoisyAt{Firings: 10, Precision: 0.2}, NoRecord: true, Unjudged: 1, SomeRules: true,
		Settings: &Settings{Lead: time.Minute, Match: []string{"service"}, LinkedOnly: true, Refire: time.Hour, UnknownLength: time.Hour, RecordLookback: RecordLookback,
			ImpactFrom: t0, ImpactNoData: true, ByWeek: true, // every field, though no data leaves impact_from out
			ImpactSignals: []Signal{{Name: "probe", For: 2 * time.Minute, Match: []string{"service"}, Record: true, Start: t0, Lead: &lead}},
			ImpactRules: []RuleEntry{{Rules: "^BackendMissing$", Labels: incident.Labels{"team": {"platform"}}, Signals: []string{"probe"}, Match: []string{"backend"},
				Lead: &lead, Require: []string{"backend"}, Judged: &no}}, Zone: "Asia/Seoul", NoisyBy: NoisyByUseful, Routine: 0.5},
		Rules: []RuleResult{{Rule: "ApiErrors", Severity: "critical", Firings: 3, White: 1, Black: 2, Refired: 1, Noisy: true, Unjudged: 1, NoRecord: true,
			Replayed: true, Unsupported: "x", Incidents: 1, First: 1, Alone: 1, Spare: true, OffHours: 1, OffHoursBlack: 1, Asleep: 1, AsleepBlack: 1,
			Excused: 1, DuringDeploys: 1, DeploysCaught: 1, DeploysUnjudged: 1, ImpactRule: 1,
			Marked:   map[string]MarkedCount{"sale": {Firings: 1, Caught: 1, Refired: 1, Unjudged: 1}},
			Episodes: EpisodeCount{Total: 2, White: 1, Unjudged: 1, Noisy: true}, Unrelated: 1, ActionYes: 1, ActionNo: 1, NotJudged: 1,
			Responses:     &RuleResponses{Changed: 1, Routine: 1, Acknowledged: 1, Unanswered: 1, None: 1, Useful: 1, NotUseful: 2, Actionable: &third},
			Notifications: &RuleNotifications{Notified: 2, Paged: 1, PagedAsleep: 1, PagedOffHours: 1, PagedForNothing: 1, PagedForNothingAsleep: 1, PagedForNothingOffHours: 1, Inhibited: 1, InhibitedBy: "Down", Grouped: 1}}},
		Incidents: []IncidentResult{{Incident: incident.Incident{ID: "INC-1", Title: "Errors", Start: t0, End: t0.Add(time.Hour),
			Labels: incident.Labels{"service": {"api"}}, Alerts: []incident.Alert{{Rule: "ApiErrors", Labels: map[string]string{"service": "api"}}},
			Candidate: true, Signal: "probe"}, Real: true, NotKnown: true, CaughtBy: []Catch{{Rule: "ApiErrors", Delay: time.Minute, Noisy: true, Linked: true}}}},
		Firings: []Judged{{Firing: firing.Firing{Rule: "ApiErrors", Labels: map[string]string{"service": "api"}, Start: t0, End: t0.Add(time.Hour)},
			White: true, Incidents: []string{"INC-1"}, NoRecord: true, Refired: true, DuringDeploy: true, Signals: []string{"probe"}, Marked: []string{"sale"},
			Episode: 1, Day: "2026-11-02", Unrelated: []string{"INC-2"}, Action: ActionYes, NotJudged: true,
			Response: &FiringResponse{Kind: ResponseChanged, Useful: &yes, Change: &ResponseChange{Kind: "rollout", At: t0, Labels: map[string]string{"namespace": "shop"}, Routine: true},
				Page: &ResponsePage{ID: "PD-1", Sent: t0, Acknowledged: t0, Resolved: t0, ResolvedBy: "alert"}},
			Notifications: &FiringNotifications{Notified: 2, Paged: 1, FirstPage: t0, Receivers: []string{"oncall"}, NoIntegrations: []string{"nobody"},
				Muted: []NotificationMute{{Kind: "inhibition", By: "rule-1", SourceRule: "Down"}}}}},
		Responses: &ResponseTotals{Pages: 1, PagesUnmatched: 1, Changes: 1, AutoscalersKnown: true},
		Notifications: &ReportNotifications{WorkedOut: true, Notified: 2, Paged: 1, Groups: 1, Stopped: "x",
			Receivers: []NotificationReceiver{{Name: "oncall", Kinds: []string{"pagerduty"}, Paging: true, Why: "pages by its pagerduty"}}},
	}
}

// The report's fields keep their names (docs/numbers.md, "What stays the
// same"), and the document lists each.
func TestReportFieldsStable(t *testing.T) {
	var b bytes.Buffer
	if err := JSON(&b, fullReport()); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	at := func(path string) map[string]any {
		v := any(doc)
		for _, part := range strings.Split(path, ".") {
			if part == "" {
				continue
			}
			name, list := strings.CutSuffix(part, "[]")
			if name == "*" {
				// A map by name (rules[].marked, by kind): its one value.
				for _, x := range v.(map[string]any) {
					v = x
				}
				continue
			}
			v = v.(map[string]any)[name]
			if list {
				v = v.([]any)[0]
			}
		}
		return v.(map[string]any)
	}
	numbers, err := os.ReadFile("../../docs/numbers.md")
	if err != nil {
		t.Fatal(err)
	}
	// The document's table, row by row: where, then the fields there.
	_, section, _ := strings.Cut(string(numbers), "### The report's fields")
	section, _, _ = strings.Cut(section, "\n### ")
	rows := map[string][]string{}
	name := regexp.MustCompile("`([^`]+)`")
	for _, line := range strings.Split(section, "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 4 || strings.HasPrefix(strings.TrimSpace(cells[1]), "---") {
			continue
		}
		where := strings.Trim(strings.TrimSpace(cells[1]), "`")
		if where == "the report" {
			where = ""
		}
		for _, m := range name.FindAllStringSubmatch(cells[2], -1) {
			rows[where] = append(rows[where], m[1])
		}
	}
	for where, fields := range stableFields {
		obj := at(where)
		for _, f := range fields {
			if _, ok := obj[f]; !ok {
				t.Errorf("%s: no %q in the JSON", where, f)
			}
			if !slices.Contains(rows[where], f) {
				t.Errorf("docs/numbers.md's report's fields, the row of %q, don't name %s", where, f)
			}
		}
		if len(rows[where]) != len(fields) {
			t.Errorf("docs/numbers.md's row of %q names %v, the test holds %v", where, rows[where], fields)
		}
	}
	if len(rows) != len(stableFields) {
		t.Errorf("docs/numbers.md's report's fields have %d rows, the test %d", len(rows), len(stableFields))
	}
}

// The CSV's columns keep their names and order, and the document lists
// them so.
func TestCSVColumns(t *testing.T) {
	want := []string{"rule", "severity", "team", "labels", "start", "end", "stone", "no_record", "incidents", "refired", "during_deploy", "excused", "marked", "episode", "unrelated", "action"}
	if !slices.Equal(CSVColumns, want) {
		t.Errorf("columns %q, want %q: a column is only added at the end", CSVColumns, want)
	}
	numbers, err := os.ReadFile("../../docs/numbers.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, _ := strings.Cut(string(numbers), "### The CSV's columns")
	var quoted []string
	for _, c := range want {
		quoted = append(quoted, "`"+c+"`")
	}
	if !strings.Contains(strings.Join(strings.Fields(section), " "), strings.Join(quoted, ", ")) {
		t.Errorf("docs/numbers.md doesn't list the columns in order: %s", strings.Join(quoted, ", "))
	}
	// With responses, more columns after them.
	more := []string{"response", "useful", "rule_changed", "rule_routine", "rule_acknowledged", "rule_unanswered", "rule_none", "rule_useful", "rule_not_useful", "rule_actionable"}
	if !slices.Equal(ResponseCSVColumns, more) {
		t.Errorf("response columns %q, want %q", ResponseCSVColumns, more)
	}
	quoted = nil
	for _, c := range more {
		quoted = append(quoted, "`"+c+"`")
	}
	if !strings.Contains(strings.Join(strings.Fields(section), " "), strings.Join(quoted, ", ")) {
		t.Errorf("docs/numbers.md doesn't list the columns with responses in order: %s", strings.Join(quoted, ", "))
	}
	// With the notifications worked out, five more after them.
	worked := []string{"worked_out_notified", "worked_out_paged", "worked_out_first_page", "worked_out_receivers", "worked_out_muted"}
	if !slices.Equal(NotificationCSVColumns, worked) {
		t.Errorf("notification columns %q, want %q", NotificationCSVColumns, worked)
	}
	quoted = nil
	for _, c := range worked {
		quoted = append(quoted, "`"+c+"`")
	}
	if !strings.Contains(strings.Join(strings.Fields(section), " "), strings.Join(quoted, ", ")) {
		t.Errorf("docs/numbers.md doesn't list the columns with notifications in order: %s", strings.Join(quoted, ", "))
	}
}

// A cell from a rule's name, a severity or a team that a spreadsheet would
// run is written as text, after spaces and in full-width forms too; a
// control or format character is written as \u{...}, there and in labels
// and incidents, which are JSON, whatever they hold; an excused firing has
// no stone, and re-fired for none.
func TestCSVCells(t *testing.T) {
	t0 := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	r := &Report{
		Rules: []RuleResult{{Rule: "=HYPERLINK(\"http://example.com\")", Severity: "+1"}, {Rule: "@SUM(A1)", Severity: "-2"}, {Rule: "\tTab", Severity: "\rCR"}},
		Firings: []Judged{
			{Firing: firing.Firing{Rule: "=HYPERLINK(\"http://example.com\")", Labels: map[string]string{"x": "=1+1", "y": "a,b\n\"c\"", "z": "\u202egnp.exe"}, Start: t0}},
			{Firing: firing.Firing{Rule: "@SUM(A1)", Start: t0.Add(time.Minute), End: t0.Add(2 * time.Minute)}, White: true, Incidents: []string{"-INC", "INC\u202e1"}},
			{Firing: firing.Firing{Rule: "\tTab", Start: t0.Add(3 * time.Minute)}, NoRecord: true},
		},
	}
	excused := []Judged{{Firing: firing.Firing{Rule: "@SUM(A1)", Start: t0.Add(2 * time.Minute)}, NoRecord: true, Refired: true}}
	var b bytes.Buffer
	if err := CSV(&b, r, excused, func(rule string) string { return "-team" }); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&b).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Fatalf("%d rows", len(rows))
	}
	for _, row := range rows[1:] {
		for i, cell := range row {
			if strings.ContainsAny(cell, "\t\r\n\u202e") || cell != "" && strings.ContainsRune("=+-@", rune(cell[0])) {
				t.Errorf("column %s: %q, which a spreadsheet would run or a reader misread", CSVColumns[i], cell)
			}
		}
	}
	first, excusedRow := rows[1], rows[3]
	if first[0] != `'=HYPERLINK("http://example.com")` || first[1] != "'+1" || first[2] != "'-team" ||
		first[3] != `{"x":"=1+1","y":"a,b\\u{a}\"c\"","z":"\\u{202e}gnp.exe"}` || first[6] != "black" || first[8] != "[]" {
		t.Errorf("first row %q", first)
	}
	if rows[2][8] != `["-INC","INC\\u{202e}1"]` || rows[2][6] != "white" || rows[2][5] != "2026-11-02T09:02:00Z" {
		t.Errorf("second row %q", rows[2])
	}
	if excusedRow[6] != "" || excusedRow[11] != "true" || excusedRow[7] != "false" || excusedRow[9] != "false" || excusedRow[0] != "'@SUM(A1)" {
		t.Errorf("an excused firing: %q", excusedRow)
	}
	if rows[4][7] != "true" || rows[4][0] != `\u{9}Tab` || rows[4][1] != `\u{d}CR` {
		t.Errorf("no record: %q", rows[4])
	}
	// Two labels' names Clean alone writes alike stay two keys.
	var two bytes.Buffer
	CSV(&two, &Report{Firings: []Judged{{Firing: firing.Firing{Rule: "R", Start: t0, Labels: map[string]string{"a\u202eb": "1", `a\u{202e}b`: "2"}}}}}, nil, nil)
	twoRows, _ := csv.NewReader(&two).ReadAll()
	var keys map[string]string
	if len(twoRows) != 2 || json.Unmarshal([]byte(twoRows[1][3]), &keys) != nil || len(keys) != 2 || keys[`a\u{202e}b`] != "1" || keys[`a\\u{202e}b`] != "2" {
		t.Errorf("two names written alike: %q", twoRows)
	}
	for in, want := range map[string]string{"plain-name": "plain-name", "": "", " =1+1": "' =1+1", "\u00a0@x": "'\u00a0@x", "＝SUM(1)": "'＝SUM(1)",
		"－1": "'－1", "\n=cmd": `\u{a}=cmd`, "\u202e=cmd": `\u{202e}=cmd`, "a=b": "a=b", `C:\path`: `C:\\path`, `\u{202e}=cmd`: `\\u{202e}=cmd`} {
		if got := Cell(in); got != want {
			t.Errorf("Cell(%q) = %q, want %q", in, got, want)
		}
	}
}

// verdictMetrics are the metrics docs/numbers.md lists that only lapilli
// verdict's --metrics gives for now, with named impact signals, each with
// its labels: TestMetricsStable holds lapilli serve's.
var verdictMetrics = []struct {
	name   string
	labels []string
}{
	{"lapilli_impact_windows", []string{"signal", "state"}},
	{"lapilli_impact_signal_start_timestamp_seconds", []string{"signal"}},
}

// The metrics of named signals keep their names and labels, and the
// document lists each with them.
func TestVerdictMetricsStable(t *testing.T) {
	t0 := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	r := fullReport()
	var b bytes.Buffer
	if err := Prometheus(&b, r, t0); err != nil {
		t.Fatal(err)
	}
	doc, err := os.ReadFile("../../docs/numbers.md")
	if err != nil {
		t.Fatal(err)
	}
	_, table, _ := strings.Cut(string(doc), "### The metrics")
	table, _, _ = strings.Cut(table, "\n### ")
	sample := regexp.MustCompile(`^([a-z_]+)(?:\{(.*)\})? `)
	label := regexp.MustCompile(`([a-z_]+)="`)
	for _, m := range verdictMetrics {
		row := ""
		for _, l := range strings.Split(table, "\n") {
			if strings.HasPrefix(l, "| `"+m.name+"` |") {
				row = l
			}
		}
		if row == "" {
			t.Errorf("docs/numbers.md doesn't list %s", m.name)
		}
		series := 0
		for _, line := range strings.Split(b.String(), "\n") {
			x := sample.FindStringSubmatch(line)
			if x == nil || x[1] != m.name {
				continue
			}
			series++
			var names []string
			for _, l := range label.FindAllStringSubmatch(x[2], -1) {
				names = append(names, l[1])
			}
			if !slices.Equal(names, m.labels) {
				t.Errorf("%s: labels %v, want %v", line, names, m.labels)
			}
		}
		for _, l := range m.labels {
			if !strings.Contains(row, "`"+l+"`") {
				t.Errorf("docs/numbers.md: %s's %s isn't listed", m.name, l)
			}
		}
		if series == 0 {
			t.Errorf("%s: no series", m.name)
		}
	}
	// With no named signal, neither.
	r.Settings.ImpactSignals = nil
	b.Reset()
	Prometheus(&b, r, t0)
	for _, m := range verdictMetrics {
		if strings.Contains(b.String(), m.name) {
			t.Errorf("no named signal, and %s", m.name)
		}
	}
}

// The metrics of responses keep their names and labels, and the document
// lists each with them: they are there with responses, and not without.
func TestResponseMetricsStable(t *testing.T) {
	r := fullReport()
	r.Rules[0].Unsupported = ""
	var b bytes.Buffer
	if err := Prometheus(&b, r, time.Time{}); err != nil {
		t.Fatal(err)
	}
	doc, err := os.ReadFile("../../docs/numbers.md")
	if err != nil {
		t.Fatal(err)
	}
	_, table, _ := strings.Cut(string(doc), "### The metrics")
	table, _, _ = strings.Cut(table, "\n### ")
	for name, labels := range map[string][]string{
		"lapilli_rule_responses":  {`rule="ApiErrors"`, `severity="critical"`, `response="`},
		"lapilli_rule_actionable": {`rule="ApiErrors"`, `severity="critical"`},
	} {
		line := ""
		for _, l := range strings.Split(b.String(), "\n") {
			if strings.HasPrefix(l, name+"{") {
				line = l
			}
		}
		if line == "" {
			t.Errorf("--metrics: no %s", name)
		}
		for _, l := range labels {
			if !strings.Contains(line, l) {
				t.Errorf("%s: no %s", line, l)
			}
		}
		if !strings.Contains(table, "| `"+name+"` |") {
			t.Errorf("docs/numbers.md doesn't list %s", name)
		}
	}
	r.Responses = nil
	b.Reset()
	Prometheus(&b, r, time.Time{})
	if strings.Contains(b.String(), "lapilli_rule_responses") || strings.Contains(b.String(), "lapilli_rule_actionable") {
		t.Error("metrics of responses, and no responses")
	}
}
