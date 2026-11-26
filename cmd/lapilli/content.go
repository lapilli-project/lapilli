package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"lapilli.dev/internal/content"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/routing"
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
	Measured bool `json:"measured"`
	Noisy    bool `json:"noisy_measured"`
	// NoRecord says incidents were given, and none keeps the period's
	// record: which rules are noisy can't be said.
	NoRecord bool `json:"no_record,omitempty"`
	// ImpactNoData says --impact was read, and keeps no record: a selector
	// it reads had no data.
	ImpactNoData bool          `json:"impact_no_data,omitempty"`
	Scrape       string        `json:"scrape_interval,omitempty" jsonschema:"the one the range check counted with; none when not known"`
	Rules        []contentRule `json:"rules"`
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

// readImpact reads the impact signal (--impact) from two weeks before the
// period to its end, which keeps the record from where the data of every
// selector it reads begins, as a named signal's does (recordStart), none
// for a selector with no data (verdict.Options.ImpactFrom and
// ImpactNoData), and adds the windows it finds in the period to incs: from
// its values in the period alone, as they were when the period alone was
// read, so that a window already on at its start starts there, and the
// firings in it are judged against it as they were.
func (f *verdictFlags) readImpact(ctx context.Context, h history, incs []incident.Incident) ([]incident.Incident, error) {
	if f.opts.Signals.Named() {
		return f.readSignals(ctx, h, incs)
	}
	from := verdict.RecordFrom(f.opts.From, f.opts.To)
	series, err := h.QueryRange(ctx, f.impact, from, f.opts.To, f.step)
	if err != nil {
		return nil, fmt.Errorf("--impact: %w", err)
	}
	start, err := f.recordStart(ctx, h, f.impact, from)
	if err != nil {
		return nil, fmt.Errorf("--impact: looking for where the data it reads begins: %w", err)
	}
	f.opts.ImpactFrom, f.opts.ImpactNoData = start, start.IsZero()
	in := make([]prom.Series, 0, len(series))
	for _, s := range series {
		s.Samples = slices.DeleteFunc(slices.Clone(s.Samples), func(x prom.Sample) bool { return x.T.Before(f.opts.From) })
		in = append(in, s)
	}
	ws, err := incident.FromImpact(ctx, in, f.step, f.impactFor)
	if err != nil {
		return nil, fmt.Errorf("--impact: %w", err)
	}
	return incident.Merge(incs, ws, f.opts.UnknownLen, f.opts.Match), nil
}

// runContent checks what the rules given say when they fire, and how they
// are written (docs/design/content.md): the facts from the rules alone;
// with --prometheus, the labels their alerts had over the period, from a
// replay; with incidents too, which were noisy.
func runContent(ctx context.Context, f *verdictFlags, versions []replay.Version, stdout, stderr io.Writer) int {
	if f.warn == nil {
		f.warn = stderr
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "lapilli verdict: %s\n", verdict.Clean(err.Error()))
		return 1
	}
	var h history
	var err error
	if f.promURL != "" || f.gcpProj != "" {
		if h, err = newHistory(ctx, f); err != nil {
			return fail(err)
		}
		f.askScrapes(ctx, h, stderr, versions)
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
	o := content.Options{Runbooks: f.runbooks, Severities: f.severities, Scrape: f.scrapeInterval, Failed: func(rule string, err error) {
		fmt.Fprintf(stderr, "lapilli verdict: the content check failed on %s, which has no findings: %s\n", verdict.Clean(rule), verdict.Clean(err.Error()))
	}}
	// With --grafana, what its notification templates show
	// (docs/design/serveops.md); refused, the check goes on without them.
	if f.grafana != "" {
		ts, cps, ps, err := source.GrafanaNotifications(ctx, f.grafana, f.grafanaTk, "lapilli/"+buildVersion())
		if err == nil {
			o.Notify, _, err = content.ReadNotifications(ts, cps, ps)
		}
		if err != nil {
			fmt.Fprintf(stderr, "lapilli verdict: Grafana's notification templates weren't read, and aren't checked: %s\n", verdict.Clean(err.Error()))
		}
	}
	// With --alertmanager-config, what the templates Alertmanager sends the
	// other rules with show; it is for this alone, so not read is an error.
	if f.amConfig != "" {
		if o.Alertmanager, err = f.alertmanagerTemplates(ctx, stderr); err != nil {
			return fail(err)
		}
		// A Grafana that sends its alerts to an external Alertmanager has
		// its rules checked against the one given (docs/design/content.md);
		// one that can't be asked is checked as before.
		if f.grafana != "" {
			sends, err := source.GrafanaAlertmanagers(ctx, f.grafana, f.grafanaTk, "lapilli/"+buildVersion())
			if err != nil {
				fmt.Fprintf(stderr, "lapilli verdict: where Grafana sends its alerts wasn't read, so its rules are checked against its own notification templates only: %s\n", verdict.Clean(err.Error()))
			} else if sends.External() {
				o.GrafanaExternal = true
				if n := sends.Several(); n != "" {
					fmt.Fprintf(stderr, "lapilli verdict: %s\n", verdict.Clean(n))
				}
				if !sends.Internal() {
					o.Notify = nil
				}
			}
		}
	}
	rep := contentReport{Rules: []contentRule{}}
	cut := false
	if h != nil {
		if o.Scrape == 0 && f.scrapes != nil {
			o.Scrape, o.Jobs = f.scrapes.Global, f.scrapes.Jobs
			// A range over a selector that names no job: the jobs its
			// series come from, asked, one query each.
			if sels := content.ToAsk(rs, *f.scrapes); len(sels) > 0 {
				var failed, notAsked int
				o.JobsOf, failed, notAsked = source.JobsOf(ctx, h, sels, now())
				if failed+notAsked > 0 {
					fmt.Fprintf(stderr, "lapilli verdict: the jobs the series of %d selectors that name none come from weren't asked (%d failed, %d past the %d asked or the time): their ranges count as before\n",
						failed+notAsked, failed, notAsked, source.MaxAsk)
				}
			}
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
		if f.incidents != "" || f.anyImpact() {
			var incs []incident.Incident
			if f.incidents != "" {
				if incs, err = incident.Load(f.incidents); err != nil {
					return fail(err)
				}
			}
			if f.anyImpact() {
				if incs, err = f.readImpact(ctx, h, incs); err != nil {
					return fail(err)
				}
			}
			f.opts.Signals = indexRules(f.opts.Signals, rs, fs)
			judged := verdict.Judge(known, fs, incs, f.opts)
			noisy := map[string]bool{}
			for _, r := range judged.Rules {
				noisy[r.Rule] = r.Noisy
			}
			o.Noisy = func(rule string) bool { return noisy[rule] }
			// With no record, which rules are noisy wasn't measured.
			rep.Noisy, rep.NoRecord, rep.ImpactNoData = !judged.NoRecord, judged.NoRecord, f.opts.ImpactNoData
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

// alertmanagerTemplates reads --alertmanager-config: a file, and the
// template files it names, or Mimir's or Cortex's API, with the ruler's
// token and tenant. What is passed over is said on stderr.
func (f *verdictFlags) alertmanagerTemplates(ctx context.Context, stderr io.Writer) (*content.Notifications, error) {
	var cfg []byte
	var files []source.TemplateFile
	var passed []string
	var err error
	if source.AlertmanagerURL(f.amConfig) {
		var found bool
		cfg, files, found, err = source.AlertmanagerAPI(ctx, f.amConfig, f.rulerTenant, source.Creds{TokenFile: f.rulerTk}, "lapilli/"+buildVersion(), nil)
		if err == nil && !found {
			err = errors.New("the tenant has no Alertmanager configuration (HTTP 404)")
		}
	} else {
		cfg, files, passed, err = source.AlertmanagerFiles(f.amConfig)
	}
	if err != nil {
		return nil, amError(err)
	}
	in := make([]content.TemplateFile, len(files))
	for i, x := range files {
		in[i] = content.TemplateFile(x)
	}
	n, skipped, err := content.ReadAlertmanager(cfg, in)
	for _, p := range append(passed, skipped...) {
		fmt.Fprintf(stderr, "lapilli verdict: --alertmanager-config: %s\n", verdict.Clean(p))
	}
	if err != nil {
		return nil, amError(err)
	}
	return n, nil
}

// alertmanagerRouting reads the routing of the configuration a flag names
// (--alertmanager-config, or --alertmanager-config-before), as serve reads
// it: the configuration alone, the templates its templates: globs name read
// but neither parsed nor said. An error names the flag once.
func (f *verdictFlags) alertmanagerRouting(ctx context.Context, path, flag string) (*routing.Config, error) {
	var cfg []byte
	var err error
	if source.AlertmanagerURL(path) {
		var found bool
		cfg, _, found, err = source.AlertmanagerAPI(ctx, path, f.rulerTenant, source.Creds{TokenFile: f.rulerTk}, "lapilli/"+buildVersion(), nil)
		if err == nil && !found {
			err = errors.New("the tenant has no Alertmanager configuration (HTTP 404)")
		}
	} else {
		cfg, _, _, err = source.AlertmanagerFiles(path)
	}
	if err == nil {
		var c *routing.Config
		if c, err = content.ReadAlertmanagerRouting(cfg); err == nil {
			return c, nil
		}
		err = fmt.Errorf("the routing can't be read: %w", err)
	}
	return nil, fmt.Errorf("%s: %s", flag, strings.TrimPrefix(verdict.Clean(err.Error()), "--alertmanager-config: "))
}

// amError names --alertmanager-config once: the source's errors name it
// already.
func amError(err error) error {
	return fmt.Errorf("--alertmanager-config: %s", strings.TrimPrefix(err.Error(), "--alertmanager-config: "))
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
	case rep.NoRecord:
		fmt.Fprintln(w, verdict.NoRecordTextFor(rep.ImpactNoData))
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
//
// A configuration, or targets, too large to read is said on stderr: the
// range check and the intervals are off then, which --scrape-interval
// mends.
func (f *verdictFlags) askScrapes(ctx context.Context, h history, stderr io.Writer, versions []replay.Version, more ...[]rules.Rule) {
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
	if errors.Is(err, prom.ErrTooLarge) && stderr != nil {
		fmt.Fprintf(stderr, "lapilli verdict: Prometheus's configuration is larger than is read of it (%s): the range check is off; give --scrape-interval\n", verdict.Clean(err.Error()))
	}
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
		} else if errors.Is(err, prom.ErrTooLarge) && stderr != nil {
			fmt.Fprintf(stderr, "lapilli verdict: Prometheus's active targets are more than is read of them (%s): a job is known by its name\n", verdict.Clean(err.Error()))
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
