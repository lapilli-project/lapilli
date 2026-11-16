package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/source"
	"lapilli.dev/internal/verdict"
)

// The series that say what really fired, by default.
const (
	alertsSeries        = `ALERTS{alertstate="firing"}`
	grafanaAlertsSeries = `GRAFANA_ALERTS{alertstate="firing",grafana_alertstate!="error"}`
	grafanaErrorsSeries = `GRAFANA_ALERTS{alertstate="firing",grafana_alertstate="error"}`
)

// checkSeriesFor are the series to read for rules: ALERTS for Prometheus's,
// GRAFANA_ALERTS for Grafana's.
func checkSeriesFor(f *verdictFlags, rs []rules.Rule) []string {
	if len(f.checkSeries) > 0 {
		return f.checkSeries
	}
	var out []string
	if slices.ContainsFunc(rs, func(r rules.Rule) bool { return r.Graph == nil }) {
		out = append(out, alertsSeries)
	}
	if slices.ContainsFunc(rs, func(r rules.Rule) bool { return r.Graph != nil }) {
		out = append(out, grafanaAlertsSeries)
	}
	return out
}

// runCheck replays the rules and sets them beside what really fired. Rules
// that share a name aren't told apart here: to what really fired, they are one
// alert.
func runCheck(ctx context.Context, f *verdictFlags, versions []replay.Version, stdout, stderr io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintf(stderr, "lapilli verdict: %v\n", err)
		return 1
	}
	h, err := newHistory(ctx, f)
	if err != nil {
		return fail(err)
	}
	f.askScrapes(ctx, h, stderr, versions)
	h = f.withLogs(h, stderr)
	if readsAPI(f) {
		rs, err := readRules(ctx, f, h, stderr)
		if err != nil {
			return fail(err)
		}
		versions = withRead(versions, rs)
	}
	var all []rules.Rule
	for _, v := range versions {
		all = append(all, v.Rules...)
	}
	// A cloud's own alarms keep no series of what they fired: their history,
	// from the recipes, stands in.
	series := checkSeriesFor(f, slices.DeleteFunc(slices.Clone(all), func(r rules.Rule) bool { return r.Cloud != "" }))
	// With the Grafana known, what its rules really fired is its own
	// state history, where it serves one (docs/design/grafanahistory.md).
	var gh source.GrafanaHistory
	where := ""
	if f.historyGrafana() != "" && slices.ContainsFunc(all, func(r rules.Rule) bool { return r.Graph != nil }) {
		gh = f.grafanaHistory()
		if f.checkGrafana != "" && f.grafanaTk != "" && strings.HasPrefix(f.checkGrafana, "http://") {
			fmt.Fprintln(stderr, "lapilli: --check-grafana is plain http: the token goes unencrypted")
		}
		if where, err = gh.Where(ctx); err != nil {
			return fail(fmt.Errorf("--check-replay: %w", err))
		}
		if where == "" {
			fmt.Fprintln(stderr, "lapilli verdict: this Grafana keeps its alert state history in Prometheus alone, or keeps none: what its rules really fired is read from GRAFANA_ALERTS.")
		} else {
			series = slices.DeleteFunc(series, func(s string) bool { return s == grafanaAlertsSeries })
		}
	}
	var recorded []firing.Firing
	if f.checkFirings != "" {
		file, err := os.Open(f.checkFirings)
		if err != nil {
			return fail(fmt.Errorf("--check-firings: %w", err))
		}
		recorded, err = firing.ReadJSONL(file)
		file.Close()
		if err != nil {
			return fail(fmt.Errorf("--check-firings %s: %w", f.checkFirings, err))
		}
	}
	// A rule in a slow group shows in the series for the lookback after each
	// evaluation, and then not until the next: to tell that from having
	// resolved, each is read at its own interval.
	every := map[string]time.Duration{}
	for _, r := range all {
		every[r.Name] = max(every[r.Name], r.Interval, f.step)
	}
	// What really fired first: it is what the check can't do without, and a
	// replay that runs out of time should still be set beside it.
	var real []firing.Firing
	for _, sel := range series {
		ss, err := h.QueryRange(ctx, sel, f.opts.From, f.opts.To, f.step)
		if err != nil {
			return fail(fmt.Errorf("--check-replay: reading %s: %w", sel, err))
		}
		byName := map[string][]prom.Series{}
		for _, s := range ss {
			name := s.Labels["alertname"]
			if name == "" {
				return fail(fmt.Errorf("--check-replay: %s has a series with no alertname label, to say which alert it is", sel))
			}
			byName[name] = append(byName[name], s)
		}
		for name, group := range byName {
			at := every[name]
			if at == 0 {
				at = f.step
			}
			real = append(real, firing.FromAlerts(group, at, f.opts.To)...)
		}
	}
	var grafanaErrors map[string]int
	if where != "" {
		// From a day before the period, so that what fired since before it
		// is seen as firing at its start.
		lead := f.opts.From.Add(-historyLead)
		ts, read, err := gh.Read(ctx, where, lead, f.opts.To)
		if err != nil {
			return fail(fmt.Errorf("--check-replay: %w", err))
		}
		if read.Passed > 0 {
			fmt.Fprintf(stderr, "lapilli verdict: %d changes of state in Grafana's history passed over, past what lapilli reads of one (docs/design/grafanahistory.md).\n", read.Passed)
		}
		// What was over by the period's start isn't in it, as a series
		// read from then doesn't have it.
		real = append(real, slices.DeleteFunc(firing.FromTransitions(ts, lead), func(x firing.Firing) bool {
			return !x.End.IsZero() && !x.End.After(f.opts.From)
		})...)
		grafanaErrors = historyErrors(ts, lead, f.opts.From, f.opts.To, all)
	}
	if f.ruleRE != nil {
		real = slices.DeleteFunc(real, func(x firing.Firing) bool { return !f.ruleRE.MatchString(x.Rule) })
	}
	known, fs := replay.Run(ctx, h, versions, f.opts.From, f.opts.To, f.step, f.parallel)
	if f.checkFirings != "" {
		history := verdict.AlarmHistory(known, recorded)
		if f.ruleRE != nil {
			history = slices.DeleteFunc(history, func(x firing.Firing) bool { return !f.ruleRE.MatchString(x.Rule) })
		}
		real = append(real, history...)
		// CloudWatch's history is of the alarm as a whole.
		fs = verdict.Whole(fs, verdict.WholeAlarms(known))
	} else {
		for i, r := range known {
			if verdict.ReplayedAlarm(r) {
				known[i].Unsupported = verdict.NoHistory
			}
		}
	}
	c := verdict.CheckReplay(ctx, known, fs, real, f.opts.From, f.opts.To, f.step)
	c.Series = series
	if where != "" {
		c.Series = append(c.Series, historyName(where))
		c.Errors = grafanaErrors
	}
	if slices.Contains(series, grafanaAlertsSeries) {
		// Grafana's error alerts, left out of what really fired: said where
		// they happened, as a replay can't show them.
		if ss, err := h.QueryRange(ctx, grafanaErrorsSeries, f.opts.From, f.opts.To, f.step); err == nil {
			c.Errors = errorsOf(firing.FromAlerts(ss, f.step, f.opts.To), all)
		}
	}
	if f.checkFirings != "" {
		c.Series = append(c.Series, "the history in "+f.checkFirings)
	}
	if f.json {
		err = verdict.JSON(stdout, c)
	} else {
		err = verdict.CheckText(stdout, c, f.loc)
	}
	if err != nil {
		return fail(err)
	}
	if !slices.ContainsFunc(known, func(r rules.Rule) bool { return r.Unsupported == "" }) {
		return fail(errors.New("--check-replay: no rule could be replayed, so nothing was compared"))
	}
	if len(real) == 0 {
		fmt.Fprintln(stderr, "lapilli verdict: nothing in those series for this period, so the replay has nothing to be checked against.")
		fmt.Fprintln(stderr, "Either nothing fired, or this API doesn't keep them: Prometheus and Amazon Managed Service for Prometheus keep ALERTS;")
		fmt.Fprintln(stderr, "for Grafana's rules, its own alert state history is read with --grafana or --check-grafana, or GRAFANA_ALERTS once it writes")
		fmt.Fprintln(stderr, "its history to Prometheus. Name another series with --check-series;")
		fmt.Fprintln(stderr, "for a cloud's own alarms, give their history with --check-firings.")
		return 1
	}
	if c.Unfinished || slices.ContainsFunc(known, func(r rules.Rule) bool { return r.Unsupported == replay.Unfinished }) {
		return fail(errors.New("some rules didn't finish within --timeout; raise it, or shorten the period"))
	}
	return 0
}

// historyLead is how long before a period Grafana's state history is
// read from: an alert that fired from before the period, changing state
// within a day of its start, is seen as firing at its start.
const historyLead = 24 * time.Hour

// historyName says where Grafana's state history was read from, as a
// check's series are said.
func historyName(where string) string {
	if where == source.HistoryLoki {
		return "Grafana's alert state history (Loki)"
	}
	return "Grafana's alert state history (annotations)"
}

// historyErrors counts Grafana's error alerts on over [from, to] by rule,
// of the rules given, read from lead: an alert in the Error state, which
// is left out of what really fired, as GRAFANA_ALERTS{grafana_alertstate=
// "error"} is, counted as that series' firings are.
func historyErrors(ts []firing.Transition, lead, from, to time.Time, given []rules.Rule) map[string]int {
	errs := make([]firing.Transition, len(ts))
	as := func(state string) string {
		if firing.GrafanaBase(state) == "Error" {
			return "Alerting"
		}
		return "Normal"
	}
	for i, t := range ts {
		errs[i] = firing.Transition{Rule: t.Rule, Labels: t.Labels, At: t.At, From: as(t.From), To: as(t.To)}
	}
	fs := slices.DeleteFunc(firing.FromTransitions(errs, lead), func(x firing.Firing) bool {
		return !x.Active(from, to) || !x.End.IsZero() && !x.End.After(from)
	})
	return errorsOf(fs, given)
}

// errorsOf counts error alerts by rule, of the rules given.
func errorsOf(fs []firing.Firing, given []rules.Rule) map[string]int {
	names := map[string]bool{}
	for _, r := range given {
		names[r.AlertName()] = true
	}
	var out map[string]int
	for _, x := range fs {
		if names[x.Rule] {
			if out == nil {
				out = map[string]int{}
			}
			out[x.Rule]++
		}
	}
	return out
}
