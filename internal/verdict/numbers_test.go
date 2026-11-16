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
	"":                        {"from", "to", "noisy_at", "no_record", "unjudged", "some_rules", "rules", "incidents", "firings", "settings"},
	"noisy_at":                {"firings", "precision"},
	"settings":                {"lead_ns", "match", "refire_ns", "unknown_length_ns", "record_lookback_ns", "impact_from", "record_by_week"},
	"rules[]":                 {"rule", "severity", "firings", "white", "black", "refired", "noisy", "unjudged", "no_record", "replayed", "unsupported", "incidents", "first", "alone", "spare", "off_hours", "off_hours_black", "asleep", "asleep_black", "excused", "during_deploys", "during_deploys_caught", "during_deploys_unjudged"},
	"incidents[]":             {"id", "title", "start", "end", "labels", "candidate", "caught_by"},
	"incidents[].caught_by[]": {"rule", "delay_ns", "noisy"},
	"firings[]":               {"rule", "labels", "start", "end", "white", "incidents", "no_record", "refired", "during_deploy"},
}

// A report with every field docs/numbers.md promises set, none left out as
// empty.
func fullReport() *Report {
	t0 := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	return &Report{
		From: t0, To: t0.Add(7 * 24 * time.Hour), NoisyAt: NoisyAt{Firings: 10, Precision: 0.2}, NoRecord: true, Unjudged: 1, SomeRules: true,
		Settings: &Settings{Lead: time.Minute, Match: []string{"service"}, Refire: time.Hour, UnknownLength: time.Hour, RecordLookback: RecordLookback,
			ImpactFrom: t0, ByWeek: true},
		Rules: []RuleResult{{Rule: "ApiErrors", Severity: "critical", Firings: 3, White: 1, Black: 2, Refired: 1, Noisy: true, Unjudged: 1, NoRecord: true,
			Replayed: true, Unsupported: "x", Incidents: 1, First: 1, Alone: 1, Spare: true, OffHours: 1, OffHoursBlack: 1, Asleep: 1, AsleepBlack: 1,
			Excused: 1, DuringDeploys: 1, DeploysCaught: 1, DeploysUnjudged: 1}},
		Incidents: []IncidentResult{{Incident: incident.Incident{ID: "INC-1", Title: "Errors", Start: t0, End: t0.Add(time.Hour),
			Labels: incident.Labels{"service": {"api"}}, Candidate: true}, CaughtBy: []Catch{{Rule: "ApiErrors", Delay: time.Minute, Noisy: true}}}},
		Firings: []Judged{{Firing: firing.Firing{Rule: "ApiErrors", Labels: map[string]string{"service": "api"}, Start: t0, End: t0.Add(time.Hour)},
			White: true, Incidents: []string{"INC-1"}, NoRecord: true, Refired: true, DuringDeploy: true}},
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
	want := []string{"rule", "severity", "team", "labels", "start", "end", "stone", "no_record", "incidents", "refired", "during_deploy", "excused"}
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
	for in, want := range map[string]string{"plain-name": "plain-name", "": "", " =1+1": "' =1+1", "\u00a0@x": "'\u00a0@x", "＝SUM(1)": "'＝SUM(1)",
		"－1": "'－1", "\n=cmd": `\u{a}=cmd`, "\u202e=cmd": `\u{202e}=cmd`, "a=b": "a=b"} {
		if got := Cell(in); got != want {
			t.Errorf("Cell(%q) = %q, want %q", in, got, want)
		}
	}
}
