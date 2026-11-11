package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"lapilli.dev/internal/content"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/source"
	"lapilli.dev/internal/verdict"
)

// contentReport is lapilli verdict --check-content's, as --json writes it.
type contentReport struct {
	From time.Time `json:"from,omitzero"`
	To   time.Time `json:"to,omitzero"`
	// Measured says whether the rules were replayed, for the labels their
	// alerts had; Noisy, whether they were judged against incidents too.
	Measured bool          `json:"measured"`
	Noisy    bool          `json:"noisy_measured"`
	Scrape   string        `json:"scrape_interval,omitempty" jsonschema:"the one the range check counted with; none when not known"`
	Rules    []contentRule `json:"rules"`
}

type contentRule struct {
	Rule     string          `json:"rule"`
	Group    string          `json:"group,omitempty"`
	Says     rules.Says      `json:"says"`
	Problems []contentOutput `json:"problems"`
	Guesses  []contentOutput `json:"guesses"`
}

type contentOutput struct {
	content.Finding
	Message string `json:"message"`
	Why     string `json:"why,omitempty"`
}

// runContent checks what the rules given say when they fire, and how they
// are written (docs/design/content.md): the facts from the rules alone;
// with --prometheus, the labels their alerts had over the period, from a
// replay; with incidents too, which were noisy.
func runContent(ctx context.Context, f *verdictFlags, versions []replay.Version, stdout, stderr io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintf(stderr, "lapilli verdict: %v\n", err)
		return 1
	}
	var h history
	var err error
	if f.promURL != "" || f.gcpProj != "" {
		if h, err = newHistory(ctx, f); err != nil {
			return fail(err)
		}
		f.askScrapes(ctx, h, versions)
	}
	if h != nil || f.loki != "" {
		h = f.withLogs(h, stderr)
	}
	if readsAPI(f) {
		rs, err := readRules(ctx, f, h, stderr)
		if err != nil {
			return fail(err)
		}
		versions = withRead(versions, rs)
	}
	// The rules as they are now: the newest version given.
	var rs []rules.Rule
	if len(versions) > 0 {
		rs = versions[len(versions)-1].Rules
	}
	source.TellApart(rs)
	o := content.Options{Severities: f.severities, Scrape: f.scrapeInterval, Failed: func(rule string, err error) {
		fmt.Fprintf(stderr, "lapilli verdict: the content check failed on %s, which has no findings: %v\n", rule, err)
	}}
	// With --grafana, what its notification templates show
	// (docs/design/serveops.md); refused, the check goes on without them.
	if f.grafana != "" {
		ts, cps, ps, err := source.GrafanaNotifications(ctx, f.grafana, f.grafanaTk, "lapilli/"+buildVersion())
		if err == nil {
			o.Notify, _, err = content.ReadNotifications(ts, cps, ps)
		}
		if err != nil {
			fmt.Fprintf(stderr, "lapilli verdict: Grafana's notification templates weren't read, and aren't checked: %v\n", err)
		}
	}
	rep := contentReport{Rules: []contentRule{}}
	cut := false
	if h != nil {
		if o.Scrape == 0 && f.scrapes != nil {
			o.Scrape, o.Jobs = f.scrapes.Global, f.scrapes.Jobs
		}
		known, fs := replay.Run(ctx, h, []replay.Version{{Rules: rs}}, f.opts.From, f.opts.To, f.step, f.parallel)
		cut = ctx.Err() != nil
		seen := map[string]map[string]bool{}
		for _, x := range fs {
			if seen[x.Rule] == nil {
				seen[x.Rule] = map[string]bool{}
			}
			for k := range x.Labels {
				seen[x.Rule][k] = true
			}
		}
		o.Seen = func(rule string) (map[string]bool, bool) {
			m, ok := seen[rule]
			return m, ok
		}
		rep.From, rep.To, rep.Measured = f.opts.From, f.opts.To, true
		if f.incidents != "" || f.impact != "" {
			var incs []incident.Incident
			if f.incidents != "" {
				if incs, err = incident.Load(f.incidents); err != nil {
					return fail(err)
				}
			}
			if f.impact != "" {
				series, err := h.QueryRange(ctx, f.impact, f.opts.From, f.opts.To, f.step)
				if err != nil {
					return fail(fmt.Errorf("--impact: %w", err))
				}
				incs = incident.Merge(incs, incident.FromImpact(series, f.step, f.impactFor), f.opts.UnknownLen, f.opts.Match)
			}
			judged := verdict.Judge(known, fs, incs, f.opts)
			noisy := map[string]bool{}
			for _, r := range judged.Rules {
				noisy[r.Rule] = r.Noisy
			}
			o.Noisy = func(rule string) bool { return noisy[rule] }
			rep.Noisy = true
		}
	} else if f.incidents != "" {
		fmt.Fprintln(stderr, "lapilli verdict: --incidents tells which rules are noisy only with a replay: add --prometheus")
	}
	if o.Scrape > 0 {
		rep.Scrape = verdict.Duration(o.Scrape)
	}
	found := content.Check(rs, o)
	for i, r := range rs {
		cr := contentRule{Rule: r.Name, Group: r.Group, Says: r.Says, Problems: []contentOutput{}, Guesses: []contentOutput{}}
		for _, x := range found[i] {
			out := contentOutput{Finding: x, Message: content.Message(x.Check), Why: content.Why(x.Check)}
			if x.Class == content.Heuristic {
				cr.Guesses = append(cr.Guesses, out)
			} else {
				cr.Problems = append(cr.Problems, out)
			}
		}
		rep.Rules = append(rep.Rules, cr)
	}
	if f.json {
		err = verdict.JSON(stdout, rep)
	} else {
		err = contentText(stdout, rep)
	}
	if err != nil {
		return fail(err)
	}
	if cut {
		fmt.Fprintln(stderr, "lapilli verdict: the replay ran out of --timeout: what was measured is only part")
		return 1
	}
	return 0
}

// contentText writes the report as tables: the problems, then the guesses,
// apart.
func contentText(w io.Writer, rep contentReport) error {
	problems, guesses, with := 0, 0, 0
	for _, r := range rep.Rules {
		problems += len(r.Problems)
		guesses += len(r.Guesses)
		if len(r.Problems) > 0 {
			with++
		}
	}
	fmt.Fprintf(w, "What %d rules say: %d problems in %d rules, %d guesses apart.\n", len(rep.Rules), problems, with, guesses)
	switch {
	case !rep.Measured:
		fmt.Fprintln(w, "Not measured: give --prometheus to replay the rules, for the labels their alerts have, and --incidents for which are noisy.")
	case !rep.Noisy:
		fmt.Fprintf(w, "Measured over %s to %s; give --incidents or --impact for which rules are noisy.\n", rep.From.Format(time.RFC3339), rep.To.Format(time.RFC3339))
	default:
		fmt.Fprintf(w, "Measured over %s to %s.\n", rep.From.Format(time.RFC3339), rep.To.Format(time.RFC3339))
	}
	if rep.Scrape == "" {
		fmt.Fprintln(w, "The range check is off: no scrape interval (--scrape-interval).")
	}
	table := func(title, head string, rows func(tw *tabwriter.Writer)) error {
		fmt.Fprintln(w)
		if title != "" {
			fmt.Fprintln(w, title)
		}
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, head)
		rows(tw)
		return tw.Flush()
	}
	if problems > 0 {
		if err := table("", "RULE\tKIND\tPROBLEM\tDETAIL", func(tw *tabwriter.Writer) {
			for _, r := range rep.Rules {
				for _, x := range r.Problems {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", printable(r.Rule), x.Class, x.Message, printable(x.Detail))
				}
			}
		}); err != nil {
			return err
		}
	}
	if guesses > 0 {
		return table("Guesses, which may be wrong:", "RULE\tGUESS\tDETAIL\tWHY", func(tw *tabwriter.Writer) {
			for _, r := range rep.Rules {
				for _, x := range r.Guesses {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", printable(r.Rule), x.Message, printable(x.Detail), x.Why)
				}
			}
		})
	}
	return nil
}

// printable is a name or detail from the rules, as a terminal is given it:
// with no control characters, or tabs to break the table.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if content.Unprintable(r) {
			return ' '
		}
		return r
	}, s)
}

// askScrapes reads, without --scrape-interval, how often Prometheus scrapes
// each job, as its /api/v1/status/config gives the global interval and
// each job's, and its targets say the job label each job's ends up with,
// where it serves them; and replays the rules read so far with it: the
// reduces Grafana's rules ask the server to work out are asked at the
// interval of the series they read (rules.Replaying.ScrapeOf).
func (f *verdictFlags) askScrapes(ctx context.Context, h history, versions []replay.Version, more ...[]rules.Rule) {
	if f.scrapeInterval > 0 || h == nil {
		return
	}
	sc, ok := h.(interface {
		StatusConfig(context.Context) (string, error)
	})
	if !ok {
		return
	}
	cfg, err := sc.StatusConfig(ctx)
	if err != nil {
		return
	}
	scrapes, ok := content.ScrapeConfig(cfg)
	if !ok || scrapes.Global <= 0 {
		return
	}
	if ta, ok := h.(interface {
		Targets(context.Context) ([]byte, error)
	}); ok {
		if b, err := ta.Targets(ctx); err == nil {
			scrapes.Targets(b)
		}
	}
	f.scrapes = &scrapes
	for _, v := range versions {
		f.replaying().Apply(v.Rules)
	}
	for _, rs := range more {
		f.replaying().Apply(rs)
	}
}
