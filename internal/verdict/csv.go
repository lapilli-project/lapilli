package verdict

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"time"
)

// CSVColumns are the columns of a report's firings as CSV, in this order.
// A column is only ever added at the end (docs/numbers.md).
var CSVColumns = []string{"rule", "severity", "team", "labels", "start", "end", "stone", "no_record", "incidents", "refired", "during_deploy", "excused"}

// CSV writes a report's firings, a row each, as CSV (docs/numbers.md):
// with excused, the firings a quiet window excused, which a report leaves
// out, among them by when they started. teamOf, when not nil, names the
// team that owns a rule.
//
// A rule's name, a severity and a team come from whoever wrote them, and a
// spreadsheet takes a cell that starts with =, +, - or @ for a formula: such
// a cell is written with ' before it (Cell). Labels and incidents are JSON,
// which starts with { or [.
func CSV(w io.Writer, r *Report, excused []Judged, teamOf func(rule string) string) error {
	severity := map[string]string{}
	for _, rr := range r.Rules {
		severity[rr.Rule] = rr.Severity
	}
	rows := slices.Clone(r.Firings)
	for _, j := range excused {
		j.Excused = true
		rows = append(rows, j)
	}
	slices.SortStableFunc(rows, func(a, b Judged) int {
		if c := a.Start.Compare(b.Start); c != 0 {
			return c
		}
		return strings.Compare(a.Rule, b.Rule)
	})
	cw := csv.NewWriter(w)
	if err := cw.Write(CSVColumns); err != nil {
		return err
	}
	for _, j := range rows {
		team := ""
		if teamOf != nil {
			team = teamOf(j.Rule)
		}
		stone := "black"
		switch {
		case j.Excused:
			stone = ""
		case j.White:
			stone = "white"
		}
		end := ""
		if !j.End.IsZero() {
			end = csvTime(j.End)
		}
		incidents := j.Incidents
		if incidents == nil {
			incidents = []string{}
		}
		labels := j.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		row := []string{Cell(j.Rule), Cell(severity[j.Rule]), Cell(team), csvJSON(labels), csvTime(j.Start), end, stone,
			csvBool(j.NoRecord && !j.Excused), csvJSON(incidents), csvBool(j.Refired), csvBool(j.DuringDeploy), csvBool(j.Excused)}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// Cell is text for a cell of CSV that a spreadsheet won't take for a
// formula: one that starts with =, +, -, @, a tab or a carriage return has
// ' put before it, as OWASP advises. Take it off to read the text as it
// was.
func Cell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// csvTime is a time in UTC, as RFC 3339 writes it.
func csvTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func csvBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// csvJSON is v as JSON on one line, a map's keys in order, < > & as they
// are.
func csvJSON(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimSuffix(b.String(), "\n")
}
