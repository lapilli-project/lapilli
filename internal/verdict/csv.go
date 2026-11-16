package verdict

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// CSVColumns are the columns of a report's firings as CSV, in this order.
// A column is only ever added at the end (docs/numbers.md).
var CSVColumns = []string{"rule", "severity", "team", "labels", "start", "end", "stone", "no_record", "incidents", "refired", "during_deploy", "excused", "marked"}

// CSV writes a report's firings, a row each, as CSV (docs/numbers.md):
// with excused, the firings a quiet window excused, which a report leaves
// out, among them by when they started. teamOf, when not nil, names the
// team that owns a rule.
//
// A rule's name, a severity, a team, labels and incident ids come from
// whoever wrote them: each is written as Cell writes it, in a cell of its
// own or in the JSON of labels and incidents, whose cell starts with { or
// [.
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
		incidents := []string{}
		for _, id := range j.Incidents {
			incidents = append(incidents, cellText(id))
		}
		labels := map[string]string{}
		for k, v := range j.Labels {
			labels[cellText(k)] = cellText(v)
		}
		// An excused firing is in none of the numbers: it re-fired for none.
		refired := j.Refired && !j.Excused
		// The kinds it was marked with: none for an excused firing, which
		// no window marks.
		marked := []string{}
		if !j.Excused {
			marked = append(marked, j.Marked...)
		}
		row := []string{Cell(j.Rule), Cell(severity[j.Rule]), Cell(team), csvJSON(labels), csvTime(j.Start), end, stone,
			csvBool(j.NoRecord && !j.Excused), csvJSON(incidents), csvBool(refired), csvBool(j.DuringDeploy), csvBool(j.Excused), csvJSON(marked)}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// Cell is text for a cell of CSV as a person reads it and a spreadsheet
// won't run: control and format characters (a line break, a tab, a bidi
// override) written as \u{...}, as Clean writes them for a terminal, and
// a backslash as \\, so no two texts are written alike (cellText); and
// text whose first character past any spaces is =, +, - or @, or one of
// their full-width forms, with ' put before it, as OWASP advises. Take it
// off to read the text as it was.
func Cell(s string) string {
	s = cellText(s)
	if t := strings.TrimLeftFunc(s, unicode.IsSpace); t != "" {
		if r, _ := utf8.DecodeRuneInString(t); strings.ContainsRune("=+-@＝＋－＠", r) {
			return "'" + s
		}
	}
	return s
}

// cellText is s as Clean writes it, a backslash written \\ first: Clean
// alone writes U+202E and the text \u{202e} alike, and two labels' names
// written alike would be one key of the JSON, a value lost.
func cellText(s string) string { return Clean(strings.ReplaceAll(s, `\`, `\\`)) }

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
