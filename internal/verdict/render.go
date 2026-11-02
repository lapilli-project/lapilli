package verdict

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
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
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "RULE\tFIRED\t○ CAUGHT\t● NOTHING\tPRECISION\tREFIRED")
		for _, rr := range fired {
			p, _ := rr.Precision()
			fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%.0f%%\t%d\n", Clean(rr.Rule), rr.Firings, rr.White, rr.Black, 100*p, rr.Refired)
		}
		if err := tw.Flush(); err != nil {
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
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "INCIDENT\tSTART\tCAUGHT BY")
		for _, in := range r.Incidents {
			name := Clean(in.ID)
			if in.Title != "" {
				name += " " + Clean(in.Title)
			}
			by := "✗ nothing fired"
			if !in.Missed() {
				var parts []string
				for _, c := range in.CaughtBy {
					part := fmt.Sprintf("%s %s", Clean(c.Rule), Delay(c.Delay))
					if c.Noisy {
						part += " (noisy)"
					}
					parts = append(parts, part)
				}
				by = strings.Join(parts, ", ")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", name, in.Start.In(loc).Format(stamp), by)
		}
		if err := tw.Flush(); err != nil {
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
				noisy, len(r.Incidents), NoisyFirings, 100*NoisyPrecision)
		}
	}
	return nil
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

// JSON writes the whole report.
func JSON(w io.Writer, r *Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
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
