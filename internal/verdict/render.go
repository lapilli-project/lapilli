package verdict

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/width"

	"lapilli.dev/internal/incident"
)

// Text writes the verdict for a person, times in loc.
func Text(w io.Writer, r *Report, loc *time.Location) error {
	const stamp = "2006-01-02 15:04"
	written, found, missed, noisy := 0, 0, 0, 0
	for _, in := range r.Incidents {
		if in.Candidate {
			found++
		} else {
			written++
		}
		if in.Missed() {
			missed++
		}
		if in.OnlyNoisy() {
			noisy++
		}
	}
	fmt.Fprintf(w, "Lapilli  %s → %s %s\n", r.From.In(loc).Format(stamp), r.To.In(loc).Format(stamp), loc)
	fmt.Fprintf(w, "%d firings, %d incidents written down", len(r.Firings), written)
	if found > 0 {
		fmt.Fprintf(w, ", %d more found by --impact", found)
	}
	fmt.Fprintln(w)
	if len(r.RulesChanged) > 0 {
		var at []string
		for _, t := range r.RulesChanged {
			at = append(at, t.In(loc).Format(stamp))
		}
		fmt.Fprintf(w, "Rules replayed as they stood: replaced at %s\n", strings.Join(at, ", "))
	}

	var fired, silent, unsupported []RuleResult
	for _, rr := range r.Rules {
		switch {
		case rr.Unsupported != "":
			unsupported = append(unsupported, rr)
		case rr.Firings == 0:
			silent = append(silent, rr)
		default:
			fired = append(fired, rr)
		}
	}
	if len(fired) > 0 {
		fmt.Fprintln(w)
		sev := slices.ContainsFunc(fired, func(rr RuleResult) bool { return rr.Severity != "" })
		rows := [][]string{withSeverity(sev, "RULE", "SEVERITY", "FIRED", "○ CAUGHT", "● NOTHING", "PRECISION", "REFIRED")}
		for _, rr := range fired {
			p, _ := rr.Precision()
			rows = append(rows, withSeverity(sev, Clean(rr.Rule), orDash(Clean(rr.Severity)), fmt.Sprint(rr.Firings), fmt.Sprint(rr.White),
				fmt.Sprint(rr.Black), fmt.Sprintf("%.0f%%", 100*p), fmt.Sprint(rr.Refired)))
		}
		if err := table(w, rows); err != nil {
			return err
		}
	}
	if len(r.Suggestions) > 0 {
		fmt.Fprintf(w, "\nNoisy rules that a longer \"for\" would quiet, still catching every incident they caught:\n")
		rows := [][]string{{"RULE", "FOR", "FIRED", "INCIDENTS", "CAUGHT LATER BY"}}
		for _, s := range r.Suggestions {
			later := "–"
			if s.Incidents > 0 {
				later = "up to " + Duration(s.Later)
			}
			rows = append(rows, []string{Clean(s.Rule), Duration(s.For) + " → " + Duration(s.NewFor),
				fmt.Sprintf("%d → %d", s.Before.Firings, s.After.Firings), fmt.Sprint(s.Incidents), later})
		}
		if err := table(w, rows); err != nil {
			return err
		}
	}
	if len(silent) > 0 {
		fmt.Fprintf(w, "\n%d rules never fired.\n", len(silent))
	}
	if len(unsupported) > 0 {
		fmt.Fprintf(w, "\nCould not replay %d rules:\n", len(unsupported))
		for _, rr := range unsupported {
			fmt.Fprintf(w, "  %s: %s\n", Clean(rr.Rule), Clean(rr.Unsupported))
		}
	}

	if len(r.Incidents) > 0 {
		fmt.Fprintln(w)
		rows := [][]string{{"INCIDENT", "START", "CAUGHT BY"}}
		for _, in := range r.Incidents {
			rows = append(rows, []string{incidentName(in.Incident), in.Start.In(loc).Format(stamp), caughtBy(in.CaughtBy)})
		}
		if err := table(w, rows); err != nil {
			return err
		}
		if missed > 0 || noisy > 0 {
			fmt.Fprintln(w)
		}
		if missed > 0 {
			fmt.Fprintf(w, "✗ %d of %d incidents had no alert.\n", missed, len(r.Incidents))
		}
		if noisy > 0 {
			fmt.Fprintf(w, "! %d of %d incidents were caught only by noisy rules, which fire whatever happens\n"+
				"  (%d or more firings, under %.0f%% of them catching anything).\n",
				noisy, len(r.Incidents), r.NoisyAt.Firings, 100*r.NoisyAt.Precision)
		}
	}
	return nil
}

// withSeverity is a row, without its severity cell when no rule has one.
func withSeverity(sev bool, rule, severity string, rest ...string) []string {
	if sev {
		return append([]string{rule, severity}, rest...)
	}
	return append([]string{rule}, rest...)
}

func orDash(s string) string {
	if s == "" {
		return "–"
	}
	return s
}

// Duration is a duration as a rule file writes it: 5m, 1h30m, 0s.
func Duration(d time.Duration) string {
	switch {
	case d == 0:
		return "0s"
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d >= time.Hour && d%time.Minute == 0:
		return fmt.Sprintf("%dh%dm", d/time.Hour, d%time.Hour/time.Minute)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return d.String()
}

// table writes rows in columns two spaces apart. Columns are as wide as
// their text shows in a terminal, where Chinese, Japanese and Korean
// characters take two cells.
func table(w io.Writer, rows [][]string) error {
	var widths []int
	for _, row := range rows {
		for i, cell := range row[:len(row)-1] {
			if i == len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], textWidth(cell))
		}
	}
	var b strings.Builder
	for _, row := range rows {
		for i, cell := range row {
			b.WriteString(cell)
			if i < len(row)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-textWidth(cell)+2))
			}
		}
		b.WriteByte('\n')
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// textWidth is how many cells s takes in a terminal.
func textWidth(s string) int {
	n := 0
	for _, r := range s {
		switch width.LookupRune(r).Kind() {
		case width.EastAsianWide, width.EastAsianFullwidth:
			n += 2
		default:
			n++
		}
	}
	return n
}

// incidentName is an incident's id and title.
func incidentName(in incident.Incident) string {
	name := Clean(in.ID)
	if in.Title != "" {
		name += " " + Clean(in.Title)
	}
	return name
}

// caughtBy lists the rules that caught an incident, with how long each took.
func caughtBy(cs []Catch) string {
	if len(cs) == 0 {
		return "✗ nothing fired"
	}
	var parts []string
	for _, c := range cs {
		part := fmt.Sprintf("%s %s", Clean(c.Rule), Delay(c.Delay))
		if c.Noisy {
			part += " (noisy)"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

// Clean makes text from rule files, incident files and the API safe for a
// terminal: control characters and Unicode format characters (escape
// sequences, bidi overrides) are shown as \u{...} instead of acted on.
func Clean(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			fmt.Fprintf(&b, "\\u{%x}", r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Delay is a detection delay as a person reads it: +2m, -30s.
func Delay(d time.Duration) string {
	sign := "+"
	if d < 0 {
		sign, d = "-", -d
	}
	d = d.Round(time.Second)
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%s%dh%dm", sign, d/time.Hour, d%time.Hour/time.Minute)
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%s%dm", sign, d/time.Minute)
	case d >= time.Minute:
		return fmt.Sprintf("%s%dm%ds", sign, d/time.Minute, d%time.Minute/time.Second)
	default:
		return fmt.Sprintf("%s%ds", sign, d/time.Second)
	}
}

// JSON writes the whole of a report or a comparison.
func JSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// Prometheus writes the verdict as metrics, for node_exporter's textfile
// collector or Pushgateway, so the stones can be graphed week to week.
func Prometheus(w io.Writer, r *Report, now time.Time) error {
	var b strings.Builder
	family := func(name, help string, lines []string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
		for _, l := range lines {
			b.WriteString(l + "\n")
		}
	}
	var fired, caught, nothing, refired []string
	for _, rr := range r.Rules {
		if rr.Unsupported != "" {
			continue
		}
		l := fmt.Sprintf(`{rule="%s"}`, escape(rr.Rule))
		if rr.Severity != "" {
			l = fmt.Sprintf(`{rule="%s",severity="%s"}`, escape(rr.Rule), escape(rr.Severity))
		}
		fired = append(fired, fmt.Sprintf("lapilli_rule_firings%s %d", l, rr.Firings))
		caught = append(caught, fmt.Sprintf("lapilli_rule_caught%s %d", l, rr.White))
		nothing = append(nothing, fmt.Sprintf("lapilli_rule_nothing%s %d", l, rr.Black))
		refired = append(refired, fmt.Sprintf("lapilli_rule_refired%s %d", l, rr.Refired))
	}
	family("lapilli_rule_firings", "Times the rule fired in the period.", fired)
	family("lapilli_rule_caught", "Firings that caught an incident (white stones).", caught)
	family("lapilli_rule_nothing", "Firings with no incident (black stones).", nothing)
	family("lapilli_rule_refired", "Firings that came soon after the same alert resolved.", refired)

	counts := map[[2]string]int{}
	var delays []string
	for _, in := range r.Incidents {
		src := "written"
		if in.Candidate {
			src = "impact"
		}
		state := "caught"
		switch first, ok := in.First(); {
		case in.Missed():
			state = "missed"
		case !ok:
			state = "noisy_only"
		default:
			delays = append(delays, fmt.Sprintf(`lapilli_incident_detection_delay_seconds{incident="%s"} %g`,
				escape(in.ID), first.Delay.Seconds()))
		}
		counts[[2]string{state, src}]++
	}
	var incs []string
	for _, state := range []string{"caught", "noisy_only", "missed"} {
		for _, src := range []string{"written", "impact"} {
			incs = append(incs, fmt.Sprintf(`lapilli_incidents{state="%s",source="%s"} %d`, state, src, counts[[2]string{state, src}]))
		}
	}
	family("lapilli_incidents", "Incidents in the period: caught, caught only by noisy rules, or missed.", incs)
	family("lapilli_incident_detection_delay_seconds", "From an incident's start to the first alert from a rule that isn't noisy; negative when the alert came first.", delays)
	family("lapilli_last_run_timestamp_seconds", "When lapilli last ran, in Unix seconds.", []string{fmt.Sprintf("lapilli_last_run_timestamp_seconds %d", now.Unix())})
	_, err := io.WriteString(w, b.String())
	return err
}

func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}
