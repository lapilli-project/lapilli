package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
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
	f.askScrapes(ctx, h, versions)
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
		fmt.Fprintln(stderr, "Grafana keeps GRAFANA_ALERTS once its alert state history is written to Prometheus. Name another series with --check-series;")
		fmt.Fprintln(stderr, "for a cloud's own alarms, give their history with --check-firings.")
		return 1
	}
	if c.Unfinished || slices.ContainsFunc(known, func(r rules.Rule) bool { return r.Unsupported == replay.Unfinished }) {
		return fail(errors.New("some rules didn't finish within --timeout; raise it, or shorten the period"))
	}
	return 0
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
