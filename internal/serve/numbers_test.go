package serve

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// stableMetrics are the metrics docs/numbers.md promises keep their names,
// each with the labels its series have: those always there, then those
// there when they have a value.
var stableMetrics = []struct {
	name             string
	labels, optional []string
}{
	{"lapilli_rule_firings", []string{"rule"}, []string{"severity", "team"}},
	{"lapilli_rule_caught", []string{"rule"}, []string{"severity", "team"}},
	{"lapilli_rule_nothing", []string{"rule"}, []string{"severity", "team"}},
	{"lapilli_rule_refired", []string{"rule"}, []string{"severity", "team"}},
	{"lapilli_rule_incidents", []string{"rule"}, []string{"severity", "team"}},
	{"lapilli_rule_incidents_first", []string{"rule"}, []string{"severity", "team"}},
	{"lapilli_rule_incidents_alone", []string{"rule"}, []string{"severity", "team"}},
	{"lapilli_rule_nothing_by_hours", []string{"rule", "hours"}, []string{"severity", "team"}},
	{"lapilli_incidents", []string{"state", "source"}, nil},
	{"lapilli_incident_detection_delay_seconds", []string{"incident"}, nil},
	{"lapilli_last_run_timestamp_seconds", nil, nil},
	{"lapilli_rule_episodes", []string{"rule"}, []string{"severity", "team"}},
	{"lapilli_rule_episodes_caught", []string{"rule"}, []string{"severity", "team"}},
	{"lapilli_rule_firings_during_deploys", []string{"rule"}, []string{"severity", "team"}},
	{"lapilli_rule_caught_during_deploys", []string{"rule"}, []string{"severity", "team"}},
	{"lapilli_rule_refired_weeks", []string{"rule"}, []string{"severity", "team"}},
	{"lapilli_refired_weeks", nil, []string{"team"}},
	{"lapilli_weeks_counted", nil, nil},
	{"lapilli_incident_state", []string{"incident", "source", "state"}, []string{"signal"}},
	{"lapilli_incident_lead_seconds", []string{"incident", "source"}, []string{"signal"}},
	{"lapilli_incident_team", []string{"incident"}, []string{"team"}},
	{"lapilli_rule_notifications", []string{"rule", "receiver", "reason"}, nil},
	{"lapilli_serve_cursor_timestamp_seconds", nil, nil},
	{"lapilli_serve_run_duration_seconds", nil, nil},
	{"lapilli_serve_runs_total", []string{"kind", "result"}, nil},
	{"lapilli_serve_firings_avoided", nil, nil},
	{"lapilli_serve_nothing_avoided", nil, nil},
	{"lapilli_serve_rule_changes", []string{"outcome"}, nil},
	{"lapilli_serve_incident_last_timestamp_seconds", nil, nil},
	{"lapilli_serve_edits", nil, nil},
	{"lapilli_serve_source_last_timestamp_seconds", []string{"source"}, nil},
	{"lapilli_serve_source_firings", []string{"source"}, nil},
	{"lapilli_serve_source_quiet_seconds", nil, nil},
	{"lapilli_serve_notifications_since_timestamp_seconds", nil, nil},
	{"lapilli_serve_notifications_last_timestamp_seconds", []string{"source"}, nil},
	{"lapilli_serve_deploys_last_timestamp_seconds", nil, nil},
	{"lapilli_serve_deploys_read_timestamp_seconds", nil, nil},
	{"lapilli_serve_incidents_passed_over", nil, nil},
	{"lapilli_goal_met", []string{"goal"}, []string{"team"}},
	{"lapilli_goal_known", []string{"goal"}, []string{"team"}},
	{"lapilli_goal_value", []string{"goal"}, []string{"team"}},
	{"lapilli_goal_weeks_met", []string{"goal"}, []string{"team"}},
	{"lapilli_rule_firings_marked", []string{"rule", "kind"}, []string{"severity", "team"}},
	{"lapilli_rule_caught_marked", []string{"rule", "kind"}, []string{"severity", "team"}},
	{"lapilli_marked_on", []string{"kind"}, nil},
	{"lapilli_serve_marked_read_timestamp_seconds", nil, nil},
}

// The metrics docs/numbers.md lists keep their names and their labels: a
// rename, or a label renamed or added, fails here; and the document lists
// each with its labels.
func TestMetricsStable(t *testing.T) {
	parallel(t)
	s := nocServer(t)
	// A goal of everyone's and one of each team's, as the leader worked
	// them out after its last round (docs/design/goals.md).
	declare(t, s, "goals:\n  - {name: fired-at-most, measure: firings, at_most: 1000}\n  - {name: none-refired, team: \"*\", measure: refired.firings, at_most: 0}\n")
	if err := s.goalsAfter(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, body, _ := get(t, s.Handler(), "/metrics")
	doc, err := os.ReadFile("../../docs/numbers.md")
	if err != nil {
		t.Fatal(err)
	}
	_, table, _ := strings.Cut(string(doc), "### The metrics")
	table, _, _ = strings.Cut(table, "\n### ")
	sample := regexp.MustCompile(`^([a-z_]+)(?:\{(.*)\})? `)
	label := regexp.MustCompile(`([a-z_]+)="`)
	seen := map[string]int{}
	for _, m := range stableMetrics {
		if !strings.Contains(body, "# TYPE "+m.name+" ") {
			t.Errorf("/metrics: no %s", m.name)
		}
		row := ""
		for _, l := range strings.Split(table, "\n") {
			if strings.HasPrefix(l, "| `"+m.name+"` |") {
				row = l
			}
		}
		if row == "" {
			t.Errorf("docs/numbers.md doesn't list %s", m.name)
		}
		for _, l := range m.labels {
			if !strings.Contains(row, "`"+l+"`") || strings.Contains(row, "[`"+l+"`]") {
				t.Errorf("docs/numbers.md: %s's %s isn't listed as always there", m.name, l)
			}
		}
		for _, l := range m.optional {
			if !strings.Contains(row, "[`"+l+"`]") {
				t.Errorf("docs/numbers.md: %s's %s isn't listed as there when it has a value", m.name, l)
			}
		}
	}
	for _, line := range strings.Split(body, "\n") {
		x := sample.FindStringSubmatch(line)
		if x == nil {
			continue
		}
		i := slices.IndexFunc(stableMetrics, func(m struct {
			name             string
			labels, optional []string
		}) bool {
			return m.name == x[1]
		})
		if i < 0 {
			continue
		}
		m := stableMetrics[i]
		seen[m.name]++
		var names []string
		for _, l := range label.FindAllStringSubmatch(x[2], -1) {
			names = append(names, l[1])
		}
		for _, want := range m.labels {
			if !slices.Contains(names, want) {
				t.Errorf("%s: no label %s", line, want)
			}
		}
		for _, n := range names {
			if !slices.Contains(m.labels, n) && !slices.Contains(m.optional, n) {
				t.Errorf("%s: a label %s docs/numbers.md doesn't list", line, n)
			}
		}
	}
	for _, m := range stableMetrics {
		if seen[m.name] == 0 {
			t.Errorf("%s: no series, so its labels went unchecked", m.name)
		}
	}
}

// /api/v1/report?format=csv: the firings a row each, those a quiet window
// excused among them, each rule's team; under the report's bounds.
func TestReportCSV(t *testing.T) {
	s := nocServer(t)
	ctx := context.Background()
	// A quiet window over Noise's first firing of the day before nocBase.
	at := nocBase.Add(-20 * time.Hour)
	if _, err := s.l.PutWindow(ctx, ledger.Window{Title: "load test", Start: at.Add(-time.Minute), End: at.Add(10 * time.Minute), Rules: "^Noise$"}, "ana", s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	if err := s.l.JudgePending(ctx, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	span := "from=" + nocBase.Add(-15*24*time.Hour).UTC().Format(time.RFC3339) + "&to=" + now.Add(time.Hour).UTC().Format(time.RFC3339)
	code, body, hdr := get(t, h, "/api/v1/report?format=csv&"+span)
	if code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "text/csv") {
		t.Fatalf("%d %s\n%s", code, hdr.Get("Content-Type"), body)
	}
	rows, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	// The server reads a deploys file, which feeds what was done about the
	// firings, so their columns follow the others (docs/design/responses.md).
	if !slices.Equal(rows[0], slices.Concat(verdict.CSVColumns, verdict.ResponseCSVColumns)) {
		t.Fatalf("columns %q", rows[0])
	}
	count := map[string]int{}
	for _, row := range rows[1:] {
		count[row[0]+" "+row[6]]++
		if row[2] == "search" {
			count["search's"]++
		}
		if row[10] == "true" {
			count["during deploy"]++
		}
		if row[11] == "true" {
			count["excused"]++
			if row[6] != "" || row[0] != "Noise" {
				t.Errorf("an excused firing: %q", row)
			}
		}
		if row[9] == "true" {
			count["refired"]++
		}
	}
	if count["excused"] != 1 || count["Flap white"] == 0 || count["search's"] != 8 || count["during deploy"] != 1 || count["refired"] != 7 {
		t.Errorf("counts %v", count)
	}
	// The JSON leaves the excused firing out: one firing fewer.
	_, js, _ := get(t, h, "/api/v1/report?"+span)
	var rep verdict.Report
	if err := json.Unmarshal([]byte(js), &rep); err != nil || len(rep.Firings) != len(rows)-2 {
		t.Errorf("the JSON has %d firings, the CSV %d rows: %v", len(rep.Firings), len(rows)-1, err)
	}
	// Refused: a format it doesn't know, the CSV without the firings, and
	// longer than 31 days.
	for _, q := range []string{"format=xml", "format=csv&firings=false",
		"format=csv&from=2026-09-01T00:00:00Z&to=2026-10-15T00:00:00Z"} {
		if code, _, _ := get(t, h, "/api/v1/report?"+q); code != 400 {
			t.Errorf("%s: %d", q, code)
		}
	}
	// One heavy report at a time: another holding the turn, this one waits
	// and is refused, as the JSON is.
	s.heavy <- struct{}{}
	defer func() { <-s.heavy }()
	old := busyWait
	busyWait = 0
	defer func() { busyWait = old }()
	if code, _, _ := get(t, h, "/api/v1/report?format=csv&"+span); code != 503 {
		t.Errorf("with the heavy turn taken: %d", code)
	}
}

// docs/numbers.md's jq, run on lapilli serve's report, as on lapilli
// verdict's (cmd/lapilli's TestNumbersRecount); and the summary's numbers
// counted again from the report of the same week.
func TestNumbersRecountServe(t *testing.T) {
	parallel(t)
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("no jq")
	}
	s := nocServer(t)
	h := s.Handler()
	dir := t.TempDir()
	fetch := func(name, path string) string {
		code, body, _ := get(t, h, path)
		if code != 200 {
			t.Fatalf("%s: %d %s", path, code, body)
		}
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o644)
		return p
	}
	all := fetch("all.json", "/api/v1/report?from="+nocBase.Add(-15*24*time.Hour).UTC().Format(time.RFC3339)+"&to="+now.Add(time.Hour).UTC().Format(time.RFC3339))
	w := ledger.Week(s.l.Day(nocBase))
	from, to := s.l.Midnight(w), s.l.Midnight(w+7)
	week := fetch("week.json", "/api/v1/report?from="+from.UTC().Format(time.RFC3339)+"&to="+to.UTC().Format(time.RFC3339))
	day := from.In(s.cfg.Loc).Format("2006-01-02")
	summary := fetch("summary.json", "/api/v1/summary?from="+day+"&to="+day)

	rs := numbersRecounts(t)
	compared := 0
	for _, again := range rs {
		if again.as != "counted again" || strings.Contains(again.name, "(lapilli verdict)") {
			continue
		}
		for _, has := range rs {
			if has.name != again.name {
				continue
			}
			var got, want string
			switch has.as {
			case "as the report has it":
				got, want = runJQ(t, again.prog, all), runJQ(t, has.prog, all)
			case "as the summary has it":
				got, want = runJQ(t, again.prog, week), runJQ(t, has.prog, summary)
			default:
				continue
			}
			if got != want {
				t.Errorf("%s: counted again %s, %s %s", again.name, got, has.as, want)
			}
			// The fixture's marked windows mark firings: a count of none
			// would test nothing.
			if strings.HasPrefix(again.name, "marked") && (want == "[]" || want == "{}") {
				t.Errorf("%s: %s %s: a test of nothing", again.name, has.as, want)
			}
			compared++
		}
	}
	if compared < 14 {
		t.Errorf("%d numbers counted again, want 14 or more: does docs/numbers.md still mark its jq?", compared)
	}
}

type numbersRecount struct{ name, as, prog string }

// numbersRecounts are docs/numbers.md's jq programs, each named by its
// first line.
func numbersRecounts(t *testing.T) []numbersRecount {
	t.Helper()
	b, err := os.ReadFile("../../docs/numbers.md")
	if err != nil {
		t.Fatal(err)
	}
	head := regexp.MustCompile(`^# (.+): (as the report has it|as the summary has it|counted again)$`)
	var out []numbersRecount
	for _, block := range regexp.MustCompile("(?s)```jq\n(.*?)```").FindAllStringSubmatch(string(b), -1) {
		first, _, _ := strings.Cut(block[1], "\n")
		if m := head.FindStringSubmatch(first); m != nil {
			out = append(out, numbersRecount{m[1], m[2], block[1]})
		}
	}
	return out
}

func runJQ(t *testing.T, prog, file string) string {
	t.Helper()
	cmd := exec.Command("jq", "-c", "-S", prog, file)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("jq: %v: %s\n%s", err, stderr.String(), prog)
	}
	return strings.TrimSpace(string(out))
}
