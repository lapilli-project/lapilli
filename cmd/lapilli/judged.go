package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/source"
	"lapilli.dev/internal/verdict"
)

// judgeFromHistory gives the rules --grafana holds that can't be replayed
// (rules.Rule.FromHistory) their firings over the period from Grafana's
// own alert state history, read with --grafana-token-file from a day
// before it, as --check-replay reads it (docs/design/grafanajudged.md),
// and the names of the rules judged so. Where the history can't be read,
// or holds no change from before the period, they are listed, with why,
// as before, and it says so on stderr; so is a rule the history doesn't
// tell apart.
func (f *verdictFlags) judgeFromHistory(ctx context.Context, known []rules.Rule, stderr io.Writer) ([]firing.Firing, map[string]bool) {
	if f.grafana == "" || f.noGrafanaHistory {
		return nil, nil
	}
	var rs []rules.Rule
	for _, r := range known {
		if r.FromHistory() && r.Source == f.grafana {
			rs = append(rs, r)
		}
	}
	if len(rs) == 0 {
		return nil, nil
	}
	not := func(why string) ([]firing.Firing, map[string]bool) {
		fmt.Fprintf(stderr, "lapilli verdict: the %d Grafana rules Lapilli can't replay aren't judged from Grafana's alert state history, and are listed: %s\n",
			len(rs), verdict.Clean(why))
		return nil, nil
	}
	gh := f.grafanaHistory()
	gh.URL = f.grafana // --check-series or not
	hw, err := gh.Where(ctx)
	switch {
	case err != nil:
		return not(err.Error())
	case hw.Serves == "":
		return not("this Grafana keeps it where Lapilli can't read it (in Prometheus alone, none, or annotations before Grafana 12.4)")
	}
	reaches, err := gh.Reaches(ctx, hw.Serves, f.opts.From)
	switch {
	case err != nil:
		return not(err.Error())
	case !reaches:
		return not("it holds no change of state from before the period, as one keeping none that far back holds none: shorten it (--since)")
	}
	all := f.grafanaRules
	if len(all) == 0 {
		all = known
	}
	j, err := gh.Judge(ctx, hw.Serves, rs, all, nil, f.opts.From.Add(-source.JudgedLead), f.opts.To)
	if err != nil {
		return not(err.Error())
	}
	if j.Passed > 0 {
		fmt.Fprintf(stderr, "lapilli verdict: %d changes of state in Grafana's history passed over, past what lapilli reads of one (docs/design/grafanahistory.md).\n", j.Passed)
	}
	for _, c := range []struct {
		names []string
		why   string
	}{{j.Unsure, "which passed over changes of them, or doesn't tell their alerts apart"},
		{j.Shared, "which gives their name to another of Grafana's rules too, and doesn't say which fired"}} {
		if len(c.names) == 0 {
			continue
		}
		names := c.names[:min(len(c.names), 10)]
		more := ""
		if n := len(c.names) - len(names); n > 0 {
			more = fmt.Sprintf(" and %d more", n)
		}
		fmt.Fprintf(stderr, "lapilli verdict: %d Grafana rules aren't judged from its alert state history, %s: %s%s\n",
			len(c.names), c.why, verdict.Clean(strings.Join(names, ", ")), more)
	}
	judged := map[string]bool{}
	for _, r := range rs {
		if !slices.Contains(j.Unsure, r.Name) && !slices.Contains(j.Shared, r.Name) {
			judged[r.Name] = true
		}
	}
	// What was over by the period's start isn't in it.
	fs := slices.DeleteFunc(j.Firings, func(x firing.Firing) bool { return !x.End.IsZero() && !x.End.After(f.opts.From) })
	return fs, judged
}
