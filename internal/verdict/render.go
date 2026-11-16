package verdict

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/width"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/rules"
)

// Text writes the verdict for a person, times in loc.
func Text(w io.Writer, r *Report, loc *time.Location) error {
	const stamp = "2006-01-02 15:04"
	written, found, missed, noisy, firings := 0, 0, 0, 0, 0
	for _, rr := range r.Rules {
		firings += rr.Firings
	}
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
	fmt.Fprintf(w, "Lapilli  %s → %s %s\n", r.From.In(loc).Format(stamp), r.To.In(loc).Format(stamp), Zone(loc, r.From, r.To))
	fmt.Fprintf(w, "%d firings, %d incidents written down", firings, written)
	if found > 0 {
		fmt.Fprintf(w, ", %d more found by --impact", found)
		if by := bySignal(r); by != "" {
			fmt.Fprintf(w, " (%s)", by)
		}
	}
	fmt.Fprintln(w)
	signalsKept(w, r, loc)
	if len(r.RulesChanged) > 0 {
		var at []string
		for _, t := range r.RulesChanged {
			at = append(at, t.In(loc).Format(stamp))
		}
		fmt.Fprintf(w, "Rules replayed as they stood: replaced at %s\n", strings.Join(at, ", "))
	}
	partial(w, r.Unreplayed, len(r.Rules), true)
	switch {
	case r.NoRecord && firings > 0:
		fmt.Fprintln(w, NoRecordText)
	case r.Unjudged > 0 && len(signals(r)) > 0:
		fmt.Fprintf(w, SomeUnjudgedSignalsText+"\n", r.Unjudged, firings)
	case r.Unjudged > 0:
		fmt.Fprintf(w, SomeUnjudgedText+"\n", r.Unjudged, firings)
	}

	var fired, silent, unsupported []RuleResult
	for _, rr := range r.Rules {
		switch {
		case rr.Unsupported != "":
			unsupported = append(unsupported, rr)
		case rr.Firings+rr.Excused+rr.Incidents == 0:
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
			share := "–"
			if p, ok := rr.Precision(); ok {
				share = fmt.Sprintf("%.0f%%", 100*p)
			}
			rows = append(rows, withSeverity(sev, Clean(rr.Rule), orDash(Clean(rr.Severity)), fmt.Sprint(rr.Firings), fmt.Sprint(rr.White),
				fmt.Sprint(rr.Black), share, fmt.Sprint(rr.Refired)))
		}
		if err := table(w, rows); err != nil {
			return err
		}
	}
	if len(r.Unrecorded) > 0 {
		fmt.Fprintf(w, "\nRules that fired as often as a noisy rule, with no record to judge them by:\n")
		for _, u := range r.Unrecorded {
			fmt.Fprintf(w, "  %s: %s\n", Clean(u.Rule), u.Text())
		}
	}
	outlasting(w, r.Rules)
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
	if err := outOfHours(w, r); err != nil {
		return err
	}
	if !r.SomeRules {
		if err := adds(w, r.Rules, len(unsupported)); err != nil {
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
	unseen(w, r.Rules)

	if len(r.Incidents) > 0 {
		fmt.Fprintln(w)
		rows := [][]string{{"INCIDENT", "START", "LASTED", "CAUGHT BY"}}
		for _, in := range r.Incidents {
			rows = append(rows, []string{incidentName(in.Incident), in.Start.In(loc).Format(stamp), lasted(in.Incident), caughtBy(in.CaughtBy)})
		}
		if err := table(w, rows); err != nil {
			return err
		}
		short := r.ImpactShort != nil && found > 0
		if missed > 0 || noisy > 0 || short {
			fmt.Fprintln(w)
		}
		if missed > 0 {
			fmt.Fprintf(w, "✗ %d of %d incidents had no alert.\n", missed, len(r.Incidents))
			spans(w, r.Incidents, loc)
		}
		if s := r.ImpactShort; short {
			fmt.Fprintf(w, "! --impact-for %s counts impact that lasted %s as an incident, but %d of the %d rules judged fire only once\n"+
				"  their condition held longer: their \"for\" and one evaluation, with no \"for\" an evaluation (%s for %d\n"+
				"  of them). Impact shorter than that they can't be sure to catch. --impact-for %s counts only what they can.\n",
				Duration(s.For), Duration(s.For), s.Rules, s.Judged, Duration(s.Need), s.Most, Duration(s.Need))
		}
		if noisy > 0 {
			fmt.Fprintf(w, "! %d of %d incidents were caught only by noisy rules, which fire whatever happens\n"+
				"  (%d or more firings, under %.0f%% of them catching anything).\n",
				noisy, len(r.Incidents), r.NoisyAt.Firings, 100*r.NoisyAt.Precision)
		}
		if err := nearMisses(w, r.NearMisses); err != nil {
			return err
		}
		if r.NearMissesCut {
			fmt.Fprintln(w, "Not every incident was looked at for rules that nearly caught it: the time or the bounds ran out, or a question of the history failed.")
		}
	}
	switch r.Extras {
	case "skipped":
		fmt.Fprintln(w, "\nA longer \"for\" for the noisy rules, near misses and what fired with no record weren't looked for (--extras 0).")
	case "cut":
		fmt.Fprintln(w, "\nA longer \"for\" for the noisy rules, near misses and what fired with no record are in part: the steps after the replays ran out of time (--extras, or --timeout).")
	}
	return nil
}

// signals are the named impact signals a report was judged by, none for an
// impact signal with no name.
func signals(r *Report) []Signal {
	if r.Settings == nil {
		return nil
	}
	return r.Settings.ImpactSignals
}

// bySignal is how many windows each named signal found, of those that
// found any: "errors 2, probe 1".
func bySignal(r *Report) string {
	n := map[string]int{}
	for _, in := range r.Incidents {
		if in.Candidate && in.Signal != "" {
			n[in.Signal]++
		}
	}
	var parts []string
	for _, s := range signals(r) {
		if n[s.Name] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", s.Name, n[s.Name]))
		}
	}
	return strings.Join(parts, ", ")
}

// signalsKept writes, of named impact signals, from when each keeps the
// record: where it was first read with data, a firing it could have seen
// judged from then on.
func signalsKept(w io.Writer, r *Report, loc *time.Location) {
	ss := signals(r)
	if len(ss) == 0 {
		return
	}
	var parts []string
	for _, s := range ss {
		switch {
		case !s.Record:
			parts = append(parts, s.Name+" doesn't (record: no)")
		case s.Start.IsZero():
			parts = append(parts, s.Name+" never: a selector it reads has no data")
		default:
			parts = append(parts, s.Name+" from "+s.Start.In(loc).Format("2006-01-02 15:04"))
		}
	}
	fmt.Fprintf(w, "Impact signals keep the record of the firings they could have seen: %s\n", strings.Join(parts, ", "))
}

// outlasting writes the rules replayed whose "for" one event can't
// outlast, those that never fired among them: what it means for each.
func outlasting(w io.Writer, rs []RuleResult) {
	said := false
	for _, rr := range rs {
		if rr.Outlasts == nil || rr.Unsupported != "" {
			continue
		}
		if !said {
			fmt.Fprintf(w, "\nA \"for\" one event can't outlast:\n")
			said = true
		}
		fmt.Fprintf(w, "  %s: %s\n", Clean(rr.Rule), rr.Outlasts.Text())
	}
}

// outOfHours writes when the firings that caught nothing came: those that
// woke people first.
func outOfHours(w io.Writer, r *Report) error {
	if r.Hours == nil {
		return nil
	}
	var rows []RuleResult
	asleep, off := 0, 0
	for _, rr := range r.Rules {
		asleep += rr.AsleepBlack
		off += rr.OffHoursBlack
		if rr.AsleepBlack+rr.OffHoursBlack > 0 {
			rows = append(rows, rr)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	slices.SortStableFunc(rows, func(a, b RuleResult) int {
		if a.AsleepBlack != b.AsleepBlack {
			return b.AsleepBlack - a.AsleepBlack
		}
		if a.OffHoursBlack != b.OffHoursBlack {
			return b.OffHoursBlack - a.OffHoursBlack
		}
		return strings.Compare(a.Rule, b.Rule)
	})
	switch {
	case r.Hours.Shifts != "":
		fmt.Fprintf(w, "\nOut of hours, in shifts (%s; where a turn doesn't say, at work %s, asleep %s, in %s):\n",
			Clean(r.Hours.Shifts), HoursText(r.Hours.Work), HoursText(r.Hours.Sleep), r.Hours.Zone)
	case r.Hours.Zone != "":
		fmt.Fprintf(w, "\nOut of hours, in %s (at work %s, asleep %s):\n", r.Hours.Zone, HoursText(r.Hours.Work), HoursText(r.Hours.Sleep))
	default: // only teams' own
		fmt.Fprintf(w, "\nOut of hours:\n")
	}
	for _, t := range r.Hours.Teams {
		if t.Shifts != "" {
			fmt.Fprintf(w, "%s's rules by its own shifts (%s; where a turn doesn't say, at work %s, asleep %s, in %s);\n",
				Clean(t.Team), Clean(t.Shifts), HoursText(t.Work), HoursText(t.Sleep), t.Zone)
			continue
		}
		fmt.Fprintf(w, "%s's rules by its own hours, in %s (at work %s, asleep %s);\n", Clean(t.Team), t.Zone, HoursText(t.Work), HoursText(t.Sleep))
	}
	// With no record, what caught nothing may have caught something nobody
	// wrote down.
	switch {
	case r.NoRecord:
		fmt.Fprintf(w, "of the firings, with no incident written down to judge them by, %d came while people slept, and %d more off hours.\n", asleep, off)
	case r.Unjudged > 0:
		fmt.Fprintf(w, "of the firings that caught nothing, or had no incident written down to judge them by, %d came while people slept, and %d more off hours.\n", asleep, off)
	default:
		fmt.Fprintf(w, "of the firings that caught nothing, %d came while people slept, and %d more off hours.\n", asleep, off)
	}
	sev := slices.ContainsFunc(rows, func(rr RuleResult) bool { return rr.Severity != "" })
	table := [][]string{withSeverity(sev, "RULE", "SEVERITY", "● ASLEEP", "● OFF HOURS", "● AT WORK")}
	const most = 10
	for i, rr := range rows {
		if i == most {
			break
		}
		table = append(table, withSeverity(sev, Clean(rr.Rule), orDash(Clean(rr.Severity)), fmt.Sprint(rr.AsleepBlack),
			fmt.Sprint(rr.OffHoursBlack), fmt.Sprint(atWork(rr))))
	}
	if err := writeTable(w, table); err != nil {
		return err
	}
	if len(rows) > most {
		fmt.Fprintf(w, "and %d more rules.\n", len(rows)-most)
	}
	return nil
}

// atWork is how many of a rule's firings that caught nothing came at work:
// those that came neither asleep nor off hours. The two are read apart in
// lapilli serve, so a write between them is no reason to go below nothing.
func atWork(rr RuleResult) int { return max(0, rr.Black-rr.AsleepBlack-rr.OffHoursBlack) }

// HoursText is hours as --work-hours and --sleep-hours take them, as a
// person reads them.
func HoursText(s string) string { return strings.ReplaceAll(s, "@", " ") }

// Number writes a rule's value short: in up to four figures.
func Number(v float64) string { return strconv.FormatFloat(v, 'g', 4, 64) }

// nearMisses writes the rules that nearly caught the incidents nothing
// caught, or only noisy rules did.
func nearMisses(w io.Writer, nms []NearMiss) error {
	if len(nms) == 0 {
		return nil
	}
	fmt.Fprintf(w, "\nRules that nearly caught them: their condition held, but not for their \"for\", or fired too late; or their value came within a fifth of their threshold.\n")
	rows := [][]string{{"INCIDENT", "RULE", "HELD", "CHANGE", "WOULD FIRE", "FIRED", "● NOTHING"}}
	for _, nm := range nms {
		fired, black := "–", "–"
		if nm.After != nil {
			fired = fmt.Sprintf("%d → %d", nm.Before.Firings, nm.After.Firings)
			black = fmt.Sprintf("%d → %d", nm.Before.Black, nm.After.Black)
		}
		held := fmt.Sprintf("%s from %s", Duration(nm.Held), Delay(nm.From))
		change := "for " + Duration(nm.For) + " → " + Duration(nm.NewFor)
		switch {
		case nm.Value != nil:
			held = "–"
			if nm.Value.Reached != nil {
				held = "reached " + Number(*nm.Value.Reached)
			}
			change = nm.Value.Compared() + " → " + Number(nm.Value.New)
		case nm.Late:
			held = fmt.Sprintf("from %s, fired %s", Delay(nm.From), Delay(nm.Fired))
		}
		rows = append(rows, []string{Clean(nm.Incident), Clean(nm.Rule), held, Clean(change), Delay(nm.Delay), fired, black})
	}
	return writeTable(w, rows)
}

// adds writes what each rule that caught something adds to the others.
func adds(w io.Writer, rs []RuleResult, unreplayed int) error {
	var caught []RuleResult
	var spare []string
	noisy := false
	for _, rr := range rs {
		if rr.Incidents > 0 {
			caught = append(caught, rr)
			noisy = noisy || rr.Noisy
		}
		if rr.Spare {
			spare = append(spare, Clean(rr.Rule))
		}
	}
	if len(caught) == 0 {
		return nil
	}
	slices.SortStableFunc(caught, func(a, b RuleResult) int {
		if a.Incidents != b.Incidents {
			return b.Incidents - a.Incidents
		}
		return strings.Compare(a.Rule, b.Rule)
	})
	fmt.Fprintf(w, "\nWhat each rule adds: the incidents it caught, those it caught before any other rule, those no other rule caught:\n")
	rows := [][]string{{"RULE", "INCIDENTS", "FIRST", "ALONE"}}
	for _, rr := range caught {
		rows = append(rows, []string{Clean(rr.Rule), fmt.Sprint(rr.Incidents), fmt.Sprint(rr.First), fmt.Sprint(rr.Alone)})
	}
	if err := table(w, rows); err != nil {
		return err
	}
	if noisy {
		fmt.Fprintln(w, "A noisy rule's catch doesn't count beside one by a rule that isn't noisy.")
	}
	if len(spare) > 0 {
		slices.Sort(spare)
		fmt.Fprintf(w, "Another rule caught every incident these caught, as soon, at the same severity or above: %s.\n", strings.Join(spare, ", "))
	}
	if unreplayed > 0 {
		fmt.Fprintf(w, "%d rules couldn't be replayed, so what they would have caught isn't counted here.\n", unreplayed)
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
func table(w io.Writer, rows [][]string) error { return writeTable(w, rows) }

func writeTable(w io.Writer, rows [][]string) error {
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
// lasted is how long an incident lasted, "–" when its end wasn't written
// down; of one found by --impact over several series, from the first one's
// start to the last one's end.
func lasted(in incident.Incident) string {
	if in.End.IsZero() {
		return "–"
	}
	return Duration(in.End.Sub(in.Start).Round(time.Second))
}

// spansMost and spansEach bound what spans writes: the incidents, and the
// series of each; spanLabel each label of a series, in runes.
const spansMost, spansEach, spanLabel = 20, 5, 80

// clip is s cut to n runes, an ellipsis ending it when it was longer.
func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// spans writes, of the incidents nothing caught that --impact found over
// several series, each series' own window: none may have lasted as long as
// the incident.
func spans(w io.Writer, ins []IncidentResult, loc *time.Location) {
	var lines []string
	for _, in := range ins {
		if !in.Missed() || len(in.Spans) < 2 {
			continue
		}
		// The labels the series differ by name each.
		common := maps.Clone(in.Spans[0].Labels)
		for _, sp := range in.Spans[1:] {
			for k, v := range common {
				if sp.Labels[k] != v {
					delete(common, k)
				}
			}
		}
		var each []string
		for i, sp := range in.Spans {
			if i == spansEach {
				each = append(each, fmt.Sprintf("%d more", len(in.Spans)-spansEach))
				break
			}
			var by []string
			for k, v := range sp.Labels {
				if _, same := common[k]; !same {
					by = append(by, clip(Clean(k)+"="+Clean(v), spanLabel))
				}
			}
			slices.Sort(by)
			name := strings.Join(by, ",")
			if name == "" {
				name = fmt.Sprintf("series %d", i+1) // they differ by nothing kept
			}
			each = append(each, fmt.Sprintf("%s %s–%s (%s)", name, sp.Start.In(loc).Format("15:04"),
				sp.End.In(loc).Format("15:04"), Duration(sp.End.Sub(sp.Start).Round(time.Second))))
		}
		lines = append(lines, fmt.Sprintf("  %s: %s", Clean(in.ID), strings.Join(each, "; ")))
	}
	if len(lines) == 0 {
		return
	}
	fmt.Fprintln(w, "  Of those --impact found over several series, each series' own window:")
	for i, l := range lines {
		if i == spansMost {
			fmt.Fprintf(w, "  and %d more\n", len(lines)-spansMost)
			break
		}
		fmt.Fprintln(w, l)
	}
}

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
	var fired, caught, nothing, refired, incidents, first, alone, hours []string
	for _, rr := range r.Rules {
		if rr.Unsupported != "" {
			continue
		}
		l := fmt.Sprintf(`{rule="%s"`, escape(rr.Rule))
		if rr.Severity != "" {
			l += fmt.Sprintf(`,severity="%s"`, escape(rr.Severity))
		}
		if rr.Team != "" {
			l += fmt.Sprintf(`,team="%s"`, escape(rr.Team))
		}
		l += "}"
		fired = append(fired, fmt.Sprintf("lapilli_rule_firings%s %d", l, rr.Firings))
		caught = append(caught, fmt.Sprintf("lapilli_rule_caught%s %d", l, rr.White))
		nothing = append(nothing, fmt.Sprintf("lapilli_rule_nothing%s %d", l, rr.Black))
		refired = append(refired, fmt.Sprintf("lapilli_rule_refired%s %d", l, rr.Refired))
		incidents = append(incidents, fmt.Sprintf("lapilli_rule_incidents%s %d", l, rr.Incidents))
		first = append(first, fmt.Sprintf("lapilli_rule_incidents_first%s %d", l, rr.First))
		alone = append(alone, fmt.Sprintf("lapilli_rule_incidents_alone%s %d", l, rr.Alone))
		if r.Hours != nil {
			for _, x := range []struct {
				name string
				n    int
			}{{Asleep, rr.AsleepBlack}, {OffHours, rr.OffHoursBlack}, {AtWork, atWork(rr)}} {
				hours = append(hours, fmt.Sprintf(`lapilli_rule_nothing_by_hours%s,hours="%s"} %d`, strings.TrimSuffix(l, "}"), x.name, x.n))
			}
		}
	}
	family("lapilli_rule_firings", "Times the rule fired in the period.", fired)
	family("lapilli_rule_caught", "Firings that caught an incident (white stones).", caught)
	family("lapilli_rule_nothing", "Firings with no incident (black stones).", nothing)
	family("lapilli_rule_refired", "Firings that came soon after the same alert resolved.", refired)
	family("lapilli_rule_incidents", "Incidents the rule caught.", incidents)
	if r.Hours != nil {
		family("lapilli_rule_nothing_by_hours", "Firings with no incident, by when they came: asleep, off hours or at work.", hours)
	}
	if !r.SomeRules {
		family("lapilli_rule_incidents_first", "Incidents the rule caught before any other rule; a noisy rule's catch doesn't count beside another's.", first)
		family("lapilli_rule_incidents_alone", "Incidents no other rule caught; a noisy rule's catch doesn't count beside another's.", alone)
	}

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
		if in.Signal != "" {
			counts[[2]string{state, "signal " + in.Signal}]++
		}
	}
	var incs []string
	for _, state := range []string{"caught", "noisy_only", "missed"} {
		for _, src := range []string{"written", "impact"} {
			incs = append(incs, fmt.Sprintf(`lapilli_incidents{state="%s",source="%s"} %d`, state, src, counts[[2]string{state, src}]))
		}
	}
	family("lapilli_incidents", "Incidents in the period: caught, caught only by noisy rules, or missed.", incs)
	family("lapilli_incident_detection_delay_seconds", "From an incident's start to the first alert from a rule that isn't noisy; negative when the alert came first.", delays)
	// Named impact signals: each one's windows, by state, and from when it
	// keeps the record.
	if ss := signals(r); len(ss) > 0 {
		var windows, starts []string
		for _, sg := range ss {
			for _, state := range []string{"caught", "noisy_only", "missed"} {
				windows = append(windows, fmt.Sprintf(`lapilli_impact_windows{signal="%s",state="%s"} %d`, escape(sg.Name), state, counts[[2]string{state, "signal " + sg.Name}]))
			}
			if !sg.Start.IsZero() && sg.Record {
				starts = append(starts, fmt.Sprintf(`lapilli_impact_signal_start_timestamp_seconds{signal="%s"} %d`, escape(sg.Name), sg.Start.Unix()))
			}
		}
		family("lapilli_impact_windows", "Impact windows in the period, by the named signal that found them: caught, caught only by noisy rules, or missed.", windows)
		family("lapilli_impact_signal_start_timestamp_seconds", "From when each named impact signal keeps the record of the firings it could have seen, in Unix seconds.", starts)
	}
	// Not run yet, it has no time to give: a dashboard shows none, not
	// the year 1.
	if !now.IsZero() {
		family("lapilli_last_run_timestamp_seconds", "When lapilli last ran, in Unix seconds.", []string{fmt.Sprintf("lapilli_last_run_timestamp_seconds %d", now.Unix())})
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// escape is a label's value as Prometheus's text format writes it, a
// control character (an escape sequence) and a direction mark, embedding,
// override or isolate (what rules.Shown takes out) said as Clean says it:
// a name comes from whoever writes rules, or from an API. Any other format
// character is as it is: the joiners names in Persian, Indic scripts or
// emoji are written with, which ALERTS has as they are.
func escape(s string) string {
	if strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) || rules.Turns(r) }) < 0 {
		return labelEscaper.Replace(s)
	}
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || rules.Turns(r) {
			fmt.Fprintf(&b, "\\u{%x}", r)
			continue
		}
		b.WriteRune(r)
	}
	return labelEscaper.Replace(b.String())
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// LabelValue is a label's value as Prometheus's text format writes it, for
// /metrics and --metrics alike (escape).
func LabelValue(s string) string { return escape(s) }

// UnseenText says, for a person, what Unseen's code means a replay can't
// see.
func UnseenText(code string) string {
	switch code {
	case rules.UnseenLate:
		return "it reads a cloud's own metrics, which often arrive minutes after their time: without --late, the replay reads them as filled in since"
	case rules.UnseenOther:
		return "it has a query on another data source than its condition's, which isn't replayed: whenever that returns nothing, Grafana takes the rule as having no data"
	case rules.UnseenFailed:
		return "a query outside its condition failed when it was replayed: whether that returned anything, which Grafana's no data hangs on, isn't known"
	case rules.UnseenLimit:
		return "its group has a limit, which the replay doesn't hold it to: past as many alerts at once, Prometheus drops them all"
	}
	return code
}

// unseen writes, of the rules replayed, how many can't be seen whole by a
// replay, and why: the names, for a few.
func unseen(w io.Writer, rs []RuleResult) {
	by := map[string][]string{}
	for _, rr := range rs {
		if rr.Unsupported != "" {
			continue
		}
		for _, u := range rr.Unseen {
			by[u] = append(by[u], Clean(rr.Rule))
		}
	}
	said := map[string]string{
		rules.UnseenLate:   "%d rules read a cloud's own metrics, which often arrive minutes late: without --late, they are replayed as filled in since (%s).",
		rules.UnseenOther:  "%d Grafana rules have a query on another data source than their condition's, which isn't replayed: whenever it returns nothing, Grafana takes the rule as having no data (%s).",
		rules.UnseenFailed: "%d Grafana rules had a query outside their condition fail when replayed: whether it returned anything, which Grafana's no data hangs on, isn't known (%s).",
		rules.UnseenLimit:  "%d rules are in a group with a limit, which the replay doesn't hold them to: past as many alerts at once, Prometheus drops them all (%s).",
	}
	for _, code := range slices.Sorted(maps.Keys(by)) {
		names := by[code]
		list := strings.Join(names[:min(len(names), 5)], ", ")
		if len(names) > 5 {
			list += fmt.Sprintf(" and %d more", len(names)-5)
		}
		f, ok := said[code]
		if !ok {
			f = "%d rules replayed in part, " + Clean(code) + " (%s)."
		}
		fmt.Fprintf(w, "\n"+f+"\n", len(names), list)
	}
}
