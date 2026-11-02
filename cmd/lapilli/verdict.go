package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/oauth2/google"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

const verdictUsage = `lapilli verdict judges every firing in a period against the incidents in
it: white stone when the firing caught one, black when nothing was wrong. It
counts each rule's stones, finds incidents no rule caught, and how long each
took to catch.

Usage:
  lapilli verdict [flags]

Where the firings come from (pick one):
  --rules FILE        replay these alert rules against history (repeatable):
                      Prometheus rule files, PrometheusRule resources, Grafana
                      alerting provisioning files, or ConfigMaps holding them;
                      or a directory of them, such as a mounted ConfigMap
  --rules FILE@TIME   the rules as they stood from TIME (RFC 3339) on, for a
                      period in which they changed; give every file in effect
                      from TIME with the same TIME, and the rules from before
                      the first TIME without one
  --alerts            read the ALERTS series Prometheus keeps for its rules
  --firings FILE      read firings recorded elsewhere, one JSON object a line:
                      {"rule":"...","labels":{...},"start":"...","end":"..."}

What went wrong:
  --incidents FILE    incidents written down, in YAML; see below
  --impact PROMQL     an expression that returns something while users are
                      hurt, such as a 5xx ratio above 1%; its windows become
                      incidents too, so a thin incident log still works. By
                      service, they carry the services hit, for --match
  --impact-for DUR    how long impact must last in a series (default 5m)

Where the history is (needed for --rules, --alerts and --impact):
  --prometheus URL    a Prometheus-compatible API: Prometheus, Thanos, Mimir,
                      VictoriaMetrics
  --gcp-project ID    Google Managed Service for Prometheus in this project,
                      with Application Default Credentials
  --bearer-token-file FILE
                      send this token with each query

When:
  --since DUR         the last DUR (default 7d)
  --from TIME, --to TIME
                      an RFC 3339 period instead
  --step DUR          how often rules are evaluated in a replay (default 1m)

Judging:
  --lead DUR          a firing up to this long before an incident starts
                      still catches it (default 15m)
  --match LABEL       a label a firing and an incident must agree on when
                      both have it, such as service (repeatable); an
                      incident can list several values
  --refire DUR        a firing this soon after the same alert resolved counts
                      as refired (default 1h)
  --rule REGEX        only rules whose name matches

Output:
  --json              the whole report, every firing included
  --metrics           Prometheus metrics, for node_exporter's textfile
                      collector or Pushgateway
  --tz ZONE           show times in this zone (default: local)
  --push URL          also put the metrics on this Pushgateway, as job lapilli
  --slack-webhook-file FILE
                      also post the report to the Slack incoming webhook
                      whose URL is in FILE
  --timeout DUR       give up after this long (default 10m)
  --parallel N        replay up to N rules at once (default 8)

The incidents file:

  incidents:
    - id: INC-12
      title: Checkout errors
      start: 2026-10-30T17:00:00+09:00
      end: 2026-10-30T17:40:00+09:00      # optional; 1h when missing
      labels: {service: [checkout, cart]} # optional, for --match

lapilli only reads history and files. It sends the report nowhere but to
--push and --slack-webhook-file.
`

type verdictFlags struct {
	rules              []ruleFile
	alerts             bool
	firings, incidents string
	impact             string
	impactFor          time.Duration
	promURL, gcpProj   string
	tokenFile          string
	since              time.Duration
	from, to           time.Time
	step               time.Duration
	opts               verdict.Options
	ruleRE             *regexp.Regexp
	json, metrics      bool
	loc                *time.Location
	timeout            time.Duration
	parallel           int
	push, slackFile    string
}

// ruleFile is a file of rules, and when they took effect; zero for from
// before the period.
type ruleFile struct {
	path string
	from time.Time
}

func parseVerdictFlags(args []string) (*verdictFlags, error) {
	f := &verdictFlags{
		impactFor: 5 * time.Minute, since: 7 * 24 * time.Hour, step: time.Minute,
		opts:     verdict.Options{Lead: 15 * time.Minute, RefireWithin: time.Hour, UnknownLen: time.Hour},
		loc:      time.Local,
		timeout:  10 * time.Minute,
		parallel: 8,
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := strings.Cut(a, "=")
		switch name {
		case "--alerts":
			f.alerts = true
			continue
		case "--json":
			f.json = true
			continue
		case "--metrics":
			f.metrics = true
			continue
		}
		if !hasVal {
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s needs a value", name)
			}
			i++
			val = args[i]
		}
		dur := func(min time.Duration) (time.Duration, error) {
			d, err := parseDuration(val)
			if err != nil || d < min {
				return 0, fmt.Errorf("%s %s: want a duration like 15m, 7d", name, val)
			}
			return d, nil
		}
		var err error
		switch name {
		case "--rules":
			rf := ruleFile{path: val}
			if i := strings.LastIndex(val, "@"); i >= 0 {
				if t, perr := time.Parse(time.RFC3339, val[i+1:]); perr == nil {
					rf = ruleFile{path: val[:i], from: t}
				} else if _, serr := os.Stat(val); serr != nil {
					return nil, fmt.Errorf("--rules %s: want FILE, or FILE@TIME with an RFC 3339 time like 2026-10-12T09:00:00Z", val)
				}
			}
			f.rules = append(f.rules, rf)
		case "--firings":
			f.firings = val
		case "--incidents":
			f.incidents = val
		case "--impact":
			f.impact = val
		case "--impact-for":
			f.impactFor, err = dur(time.Second)
		case "--prometheus":
			f.promURL = val
		case "--gcp-project":
			f.gcpProj = val
		case "--bearer-token-file":
			f.tokenFile = val
		case "--since":
			f.since, err = dur(time.Minute)
		case "--from", "--to":
			t, perr := time.Parse(time.RFC3339, val)
			if perr != nil {
				return nil, fmt.Errorf("%s %s: want an RFC 3339 time like 2026-11-01T00:00:00Z", name, val)
			}
			if name == "--from" {
				f.from = t
			} else {
				f.to = t
			}
		case "--step":
			f.step, err = dur(time.Second)
		case "--lead":
			f.opts.Lead, err = dur(0)
		case "--match":
			f.opts.Match = append(f.opts.Match, val)
		case "--refire":
			f.opts.RefireWithin, err = dur(0)
		case "--rule":
			if f.ruleRE, err = regexp.Compile(val); err != nil {
				return nil, fmt.Errorf("--rule %s: %v", val, err)
			}
		case "--tz":
			if f.loc, err = time.LoadLocation(val); err != nil {
				return nil, fmt.Errorf("--tz %s: %v", val, err)
			}
		case "--timeout":
			f.timeout, err = dur(time.Second)
		case "--push":
			if u, perr := url.Parse(val); perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return nil, fmt.Errorf("--push %s: want the Pushgateway's URL, like http://pushgateway:9091", val)
			}
			f.push = val
		case "--slack-webhook-file":
			if _, err := os.Stat(val); err != nil {
				return nil, fmt.Errorf("--slack-webhook-file: %v", err)
			}
			f.slackFile = val
		case "--parallel":
			f.parallel, err = strconv.Atoi(val)
			if err != nil || f.parallel < 1 || f.parallel > 64 {
				return nil, fmt.Errorf("--parallel %s: want a number from 1 to 64", val)
			}
		default:
			return nil, fmt.Errorf("unknown flag %s", a)
		}
		if err != nil {
			return nil, err
		}
	}
	sources := 0
	for _, on := range []bool{len(f.rules) > 0, f.alerts, f.firings != ""} {
		if on {
			sources++
		}
	}
	needsHistory := len(f.rules) > 0 || f.alerts || f.impact != ""
	switch {
	case sources != 1:
		return nil, errors.New("say where the firings come from: one of --rules, --alerts, --firings")
	case f.json && f.metrics:
		return nil, errors.New("--json and --metrics don't go together")
	case f.promURL != "" && f.gcpProj != "":
		return nil, errors.New("--prometheus and --gcp-project don't go together")
	case needsHistory && f.promURL == "" && f.gcpProj == "":
		return nil, errors.New("--rules, --alerts and --impact read history: add --prometheus URL or --gcp-project ID")
	case f.from.IsZero() != f.to.IsZero():
		return nil, errors.New("--from and --to go together")
	case !f.from.IsZero() && !f.to.After(f.from):
		return nil, errors.New("--to must be after --from")
	case f.incidents == "" && f.impact == "":
		return nil, errors.New("say what went wrong: --incidents FILE, --impact PROMQL, or both")
	}
	return f, nil
}

// parseDuration takes Go durations and, as Prometheus does, days: 7d, 1d12h.
func parseDuration(s string) (time.Duration, error) {
	if d, rest, ok := strings.Cut(s, "d"); ok {
		var n int
		if _, err := fmt.Sscanf(d, "%d", &n); err != nil || fmt.Sprint(n) != d {
			return 0, fmt.Errorf("bad duration %q", s)
		}
		more := time.Duration(0)
		if rest != "" {
			var err error
			if more, err = time.ParseDuration(rest); err != nil {
				return 0, err
			}
		}
		return time.Duration(n)*24*time.Hour + more, nil
	}
	return time.ParseDuration(s)
}

// history is the part of a Prometheus API lapilli uses.
type history = replay.History

// newHistory is replaced in tests.
var newHistory = func(ctx context.Context, f *verdictFlags) (history, error) {
	agent := "lapilli/" + buildVersion()
	if f.gcpProj != "" {
		ts, err := google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/monitoring.read")
		if err != nil {
			return nil, fmt.Errorf("Google credentials: %w", err)
		}
		base := "https://monitoring.googleapis.com/v1/projects/" + f.gcpProj + "/location/global/prometheus"
		return prom.New(base, agent, prom.WithToken(func(context.Context) (string, error) {
			t, err := ts.Token()
			if err != nil {
				return "", err
			}
			return t.AccessToken, nil
		})), nil
	}
	var opts []prom.Option
	if f.tokenFile != "" {
		opts = append(opts, prom.WithToken(func(context.Context) (string, error) {
			b, err := os.ReadFile(f.tokenFile)
			return strings.TrimSpace(string(b)), err
		}))
	}
	return prom.New(f.promURL, agent, opts...), nil
}

var now = time.Now

func runVerdict(args []string, stdout, stderr io.Writer) int {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Fprint(stdout, verdictUsage)
			return 0
		}
	}
	f, err := parseVerdictFlags(args)
	if err != nil {
		fmt.Fprintf(stderr, "lapilli verdict: %v\nRun \"lapilli verdict -h\" for the options.\n", err)
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "lapilli verdict: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	f.opts.From, f.opts.To = f.from, f.to
	if f.from.IsZero() {
		f.opts.To = now().Truncate(f.step)
		f.opts.From = f.opts.To.Add(-f.since)
	}

	versions, err := loadVersions(f)
	if err != nil {
		return fail(err)
	}

	var h history
	if len(f.rules) > 0 || f.alerts || f.impact != "" {
		if h, err = newHistory(ctx, f); err != nil {
			return fail(err)
		}
	}

	// Impact first: it is one query, and a replay that runs out of time
	// should still be judged against it.
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

	var known []rules.Rule
	var fs []firing.Firing
	switch {
	case len(versions) > 0:
		known, fs = replay.Run(ctx, h, versions, f.opts.From, f.opts.To, f.step, f.parallel)
	case f.alerts:
		series, err := h.QueryRange(ctx, `ALERTS{alertstate="firing"}`, f.opts.From, f.opts.To, f.step)
		if err != nil {
			return fail(fmt.Errorf("reading ALERTS: %w", err))
		}
		fs = firing.FromAlerts(series, f.step, f.opts.To)
	default:
		file, err := os.Open(f.firings)
		if err != nil {
			return fail(err)
		}
		fs, err = firing.ReadJSONL(file)
		file.Close()
		if err != nil {
			return fail(fmt.Errorf("%s: %w", f.firings, err))
		}
	}
	if f.ruleRE != nil {
		kept := fs[:0]
		for _, x := range fs {
			if f.ruleRE.MatchString(x.Rule) {
				kept = append(kept, x)
			}
		}
		fs = kept
	}

	rep := verdict.Judge(known, fs, incs, f.opts)
	for _, v := range versions {
		if v.From.After(f.opts.From) && v.From.Before(f.opts.To) {
			rep.RulesChanged = append(rep.RulesChanged, v.From)
		}
	}
	switch {
	case f.json:
		err = verdict.JSON(stdout, rep)
	case f.metrics:
		err = verdict.Prometheus(stdout, rep, now())
	default:
		err = verdict.Text(stdout, rep, f.loc)
	}
	if err != nil {
		return fail(err)
	}
	// Sending gets its own time: the replay may have used up --timeout.
	sendCtx, cancelSend := context.WithTimeout(context.Background(), time.Minute)
	defer cancelSend()
	failed := false
	if f.push != "" {
		if err := pushMetrics(sendCtx, f.push, rep, now()); err != nil {
			fmt.Fprintf(stderr, "lapilli verdict: %v\n", err)
			failed = true
		}
	}
	if f.slackFile != "" {
		var b strings.Builder
		if err := verdict.Text(&b, rep, f.loc); err != nil {
			return fail(err)
		}
		if err := postSlack(sendCtx, f.slackFile, b.String()); err != nil {
			fmt.Fprintf(stderr, "lapilli verdict: %v\n", err)
			failed = true
		}
	}
	// A partial verdict is still worth printing, but not worth a 0.
	unfinished := 0
	for _, r := range rep.Rules {
		if r.Unsupported == replay.Unfinished {
			unfinished++
		}
	}
	if unfinished > 0 {
		fmt.Fprintf(stderr, "lapilli verdict: %d of %d rules didn't finish within --timeout %s; raise it, or shorten the period\n",
			unfinished, len(rep.Rules), f.timeout)
		return 1
	}
	if failed {
		return 1
	}
	return 0
}

// loadVersions reads the --rules files, one version of the rules for each
// time they were given with, in order.
func loadVersions(f *verdictFlags) ([]replay.Version, error) {
	if len(f.rules) == 0 {
		return nil, nil
	}
	var versions []replay.Version
	n := 0
	for _, rf := range f.rules {
		i := slices.IndexFunc(versions, func(v replay.Version) bool { return v.From.Equal(rf.from) })
		if i < 0 {
			versions = append(versions, replay.Version{From: rf.from})
			i = len(versions) - 1
		}
		rs, err := rules.Load(rf.path)
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			if f.ruleRE == nil || f.ruleRE.MatchString(r.Name) {
				versions[i].Rules = append(versions[i].Rules, r)
				n++
			}
		}
	}
	slices.SortFunc(versions, func(a, b replay.Version) int { return a.From.Compare(b.From) })
	switch first := versions[0].From; {
	case n == 0:
		return nil, errors.New("no alert rules to replay")
	case first.After(f.opts.From):
		return nil, fmt.Errorf("the first rules given are from %s, after the period starts: give the rules from before then as --rules FILE, without @TIME",
			first.Format(time.RFC3339))
	}
	return versions, nil
}
