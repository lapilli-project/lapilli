package main

import (
	"bytes"
	"encoding/csv"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"lapilli.dev/internal/verdict"
)

// numbersReport writes a report of neutral firings and incidents, as
// lapilli verdict --json writes it: a rule that re-fires (once at --refire
// to the second), a catch at --lead to the second, firings with no
// incident in the two weeks before them, one before the period still on,
// one still firing, a noisy rule whose catch is an incident's only one, a
// missed incident, an incident written in another offset, and one that
// keeps the record from before the period.
func numbersReport(t *testing.T, format string) string {
	t.Helper()
	incidents := write(t, "incidents.yaml", `incidents:
  - {id: INC-1, title: Checkout errors, start: 2026-09-25T10:00:00Z, end: 2026-09-25T11:00:00Z, labels: {service: checkout}}
  - {id: INC-2, title: Cart and checkout slow, start: 2026-10-20T09:00:00+09:00, end: 2026-10-20T10:00:00+09:00, labels: {service: [checkout, cart]}}
  - {id: INC-3, title: Search down, start: 2026-10-22T14:00:00Z, labels: {service: search}}
  - {id: INC-4, title: Payments queue stuck, start: 2026-10-25T03:00:00Z, end: 2026-10-25T03:30:00Z, labels: {service: payments}}
  - {id: INC-5, title: Checkout errors again, start: 2026-10-28T12:00:00Z, end: 2026-10-28T12:20:00Z, labels: {service: checkout}}
`)
	var fs strings.Builder
	line := func(rule, labels, start, end string) {
		fs.WriteString(`{"rule":"` + rule + `","labels":` + labels + `,"start":"` + start + `"`)
		if end != "" {
			fs.WriteString(`,"end":"` + end + `"`)
		}
		fs.WriteString("}\n")
	}
	co := `{"service":"checkout","severity":"critical"}`
	line("CheckoutErrors", co, "2026-09-30T23:50:00Z", "2026-10-01T00:10:00Z")
	line("CheckoutErrors", co, "2026-10-02T10:00:00Z", "2026-10-02T10:20:00Z")
	line("CheckoutErrors", co, "2026-10-15T08:00:00Z", "2026-10-15T08:10:00Z")
	line("CheckoutErrors", co, "2026-10-15T08:30:00Z", "2026-10-15T08:40:00Z")
	line("CheckoutErrors", co, "2026-10-15T09:40:00Z", "2026-10-15T09:50:00Z") // re-fired, at --refire to the second
	line("CheckoutErrors", co, "2026-10-19T23:50:00Z", "2026-10-20T00:30:00Z")
	line("CheckoutErrors", co, "2026-10-28T11:55:00Z", "2026-10-28T12:30:00Z")
	line("CheckoutLatency", `{"service":"checkout","severity":"warning"}`, "2026-10-28T11:45:00Z", "2026-10-28T11:50:00Z") // at --lead to the second
	ca := `{"service":"cart","severity":"warning"}`
	line("CartLatency", ca, "2026-10-20T00:20:00Z", "2026-10-20T00:40:00Z")
	line("CartLatency", ca, "2026-10-28T12:10:00Z", "2026-10-28T12:25:00Z")
	node := `{"instance":"node-1","severity":"warning"}`
	for _, h := range []string{"21T00", "21T06", "21T12", "21T18", "22T00", "22T06", "22T12", "22T18", "23T00", "23T06", "23T12", "23T18"} {
		line("NodeFlap", node, "2026-10-"+h+":00:00Z", "2026-10-"+h+":10:00Z")
	}
	line("NodeFlap", node, "2026-10-22T13:50:00Z", "2026-10-22T14:05:00Z")
	pay := `{"service":"payments","severity":"critical"}`
	line("DiskSoon", pay, "2026-10-25T05:00:00Z", "2026-10-25T05:10:00Z")
	line("DiskSoon", pay, "2026-10-31T23:00:00Z", "")
	firings := write(t, "firings.jsonl", fs.String())
	var stdout, stderr bytes.Buffer
	if code := run([]string{"verdict", "--firings", firings, "--incidents", incidents, "--from", "2026-10-01T00:00:00Z", "--to", "2026-11-01T00:00:00Z",
		"--match", "service", "--tz", "UTC", format}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	return stdout.String()
}

// recount is a pair of docs/numbers.md's jq programs: a number as the
// report (or the summary) has it, and counted again from the firings.
type recount struct {
	name, as, prog string
}

// recounts are docs/numbers.md's jq programs, each named by its first
// line: "# NAME: as the report has it", "as the summary has it" or
// "counted again".
func recounts(t *testing.T, doc string) []recount {
	t.Helper()
	b, err := os.ReadFile(doc)
	if err != nil {
		t.Fatal(err)
	}
	head := regexp.MustCompile(`^# (.+): (as the report has it|as the summary has it|counted again)$`)
	var out []recount
	for _, block := range regexp.MustCompile("(?s)```jq\n(.*?)```").FindAllStringSubmatch(string(b), -1) {
		first, _, _ := strings.Cut(block[1], "\n")
		m := head.FindStringSubmatch(first)
		if m == nil {
			t.Errorf("a jq block in %s doesn't say what it is: %q", doc, first)
			continue
		}
		out = append(out, recount{m[1], m[2], block[1]})
	}
	return out
}

// jq runs a program on a file, its output on one line, keys in order.
func jq(t *testing.T, prog, file string, args ...string) string {
	t.Helper()
	cmd := exec.Command("jq", append(append([]string{"-c", "-S"}, args...), prog, file)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("jq: %v: %s\n%s", err, stderr.String(), prog)
	}
	return strings.TrimSpace(string(out))
}

// Every number docs/numbers.md says how to count again is counted again,
// by its jq, from a report lapilli verdict wrote, and comes out as the
// report has it: the document says what the code does.
func TestNumbersRecount(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("no jq")
	}
	report := filepath.Join(t.TempDir(), "report.json")
	os.WriteFile(report, []byte(numbersReport(t, "--json")), 0o644)
	// The incident before the period, which keeps the record of the firings
	// early in it, as the document says to give it.
	earlier := []string{"--argjson", "earlier", `[{"id":"INC-1","start":"2026-09-25T10:00:00Z","end":"2026-09-25T11:00:00Z"}]`}
	rs := recounts(t, "../../docs/numbers.md")
	reported := map[string]string{}
	for _, r := range rs {
		if r.as == "as the report has it" && !strings.Contains(r.name, "(lapilli serve)") {
			reported[r.name] = jq(t, r.prog, report, earlier...)
		}
	}
	compared := 0
	for _, r := range rs {
		if r.as != "counted again" || strings.Contains(r.name, "(lapilli serve)") {
			continue
		}
		want, ok := reported[r.name]
		if !ok {
			continue // as the summary has it alone: lapilli serve's
		}
		if got := jq(t, r.prog, report, earlier...); got != want {
			t.Errorf("%s: counted again %s, the report has %s", r.name, got, want)
		}
		if want == "[]" || want == "null" || want == "{}" {
			t.Errorf("%s: the report has %s: a test of nothing", r.name, want)
		}
		compared++
	}
	if compared < 10 {
		t.Errorf("%d numbers counted again, want 10 or more: does docs/numbers.md still mark its jq?", compared)
	}
	// Without the incident before the period, the firings early in it have
	// no record: the count needs it, as the document says.
	for _, r := range rs {
		if r.name == "no record (lapilli verdict)" && r.as == "counted again" {
			if got := jq(t, r.prog, report); got == reported[r.name] {
				t.Error("no record: counted again the same without the incidents before the period")
			}
		}
	}
}

// --csv: the firings a row each, in the columns docs/numbers.md lists, and
// the same stones as --json.
func TestVerdictCSV(t *testing.T) {
	rows, err := csv.NewReader(strings.NewReader(numbersReport(t, "--csv"))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rows[0], verdict.CSVColumns) {
		t.Fatalf("columns %q", rows[0])
	}
	if len(rows) != 26 {
		t.Fatalf("%d rows, want the 25 firings and the header", len(rows))
	}
	count := map[string]int{}
	for _, row := range rows[1:] {
		count[row[6]]++
		if row[7] == "true" {
			count["no_record"]++
		}
		if row[9] == "true" {
			count["refired"]++
		}
	}
	if count["white"] != 5 || count["black"] != 20 || count["no_record"] != 3 || count["refired"] != 2 {
		t.Errorf("counts %v", count)
	}
	// The one still firing has no end; one before the period is first.
	if rows[1][4] != "2026-09-30T23:50:00Z" || rows[25][5] != "" || rows[1][3] != `{"service":"checkout","severity":"critical"}` {
		t.Errorf("rows %q ... %q", rows[1], rows[25])
	}
	for args, want := range map[string]string{
		"--firings f --incidents i --json":               "--csv doesn't go with --json",
		"--firings f --incidents i --metrics":            "--csv doesn't go with --json, --metrics",
		"--rules y --compare x --prometheus http://p":    "--csv writes a verdict's firings",
		"--rules y --check-replay --prometheus http://p": "--csv writes a verdict's firings",
		"--rules y --check-content":                      "--csv writes a verdict's firings",
		"--rules y --prometheus http://p":                "",
	} {
		cmd := "verdict"
		if want == "" {
			cmd, want = "serve", "lapilli serve doesn't take --csv"
		}
		var stdout, stderr bytes.Buffer
		code := run(append([]string{cmd, "--csv"}, strings.Fields(args)...), &stdout, &stderr)
		if code != 2 || !strings.Contains(stderr.String(), want) {
			t.Errorf("%s --csv %s: exit %d, %s", cmd, args, code, stderr.String())
		}
	}
}
