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

	"lapilli.dev/internal/cloud"
	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/report"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/source"
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
  --rules-history DIR earlier versions of the rules, one directory each, named
                      by when it took effect (2026-10-14T16:40:31Z or
                      20261014T164031Z): as each given as --rules DIR@TIME.
                      recipes/versions/git.sh writes one from git
  --loaded-rules      replay the alert rules the API at --prometheus has
                      loaded (GET /api/v1/rules): Prometheus, Thanos, Mimir,
                      vmalert, Amazon Managed Service for Prometheus
  --grafana URL       replay the alert rules this Grafana holds, exported
                      through its provisioning API
  --grafana-token-file FILE
                      a Grafana service account token, to read them with
  --ruler URL         replay the alert rules a managed Prometheus ruler
                      holds, as written, from the API that writes them:
                        Mimir     http://mimir:8080/prometheus/config/v1/rules
                        Cortex    http://cortex:9009/api/v1/rules
                        Amazon    https://aps.REGION.amazonaws.com/workspaces/ID
                                  (with --auth aws)
                        Azure     https://management.azure.com/subscriptions/SUB
                                  or .../resourceGroups/RG (with --auth azure)
                      Rules of one name in two namespaces are told apart by
                      both: "ScrapeSlow [payments/shop]"
  --ruler-token-file FILE
                      a token that may read a Mimir's or a Cortex's rules
  --ruler-tenant ID   the tenant of a Mimir or a Cortex with tenants and no
                      gateway to set it, sent as X-Scope-OrgID to --ruler
                      and --prometheus alike
  --alarms SOURCE=URL also read a cloud's own alarms through its API, beside
                      any of the above or alone (repeatable): PromQL alarms
                      are replayed, the others listed. SOURCE names them, as
                      their history is copied in (lapilli serve); the URL's
                      host says which cloud, with its credentials, whatever
                      --auth says:
                        CloudWatch  https://monitoring.REGION.amazonaws.com
                        Azure       https://management.azure.com/subscriptions/SUB
                                    or .../resourceGroups/RG
                        Google      https://monitoring.googleapis.com/v3/
                                    projects/PROJECT/alertPolicies
                        OCI         https://telemetry.REGION.oraclecloud.com/
                                    20180401/alarms?compartmentId=OCID
                      An alarm and a rule of one name are told apart by the
                      source: "HighCPU [cloudwatch-prod]"
  --alerts            read the ALERTS series Prometheus keeps for its rules
  --firings FILE      read firings recorded elsewhere, one JSON object a line:
                      {"rule":"...","labels":{...},"start":"...","end":"..."};
                      recipes/firings writes each cloud's alarm history so

Before a change to the rules:
  --compare FILE      the rules in place, to set --rules beside (repeatable):
                      both are replayed over the same period, and the rules
                      that differ and the incidents caught differently are
                      shown before → after
  --changed-only      with --compare, replay only the rules that differ:
                      much faster, but an incident shows only their catches

What the rules say:
  --check-content     check what each rule says when it fires, and how it is
                      written, instead of judging: no summary, no runbook, a
                      template that doesn't parse, no severity, the same
                      condition as another rule, as facts; with --prometheus,
                      a template label no firing over the period had, and
                      with --incidents or --impact, a noisy rule with no
                      "for", as measured; guesses (a label an aggregation
                      can't give, a rare severity, a range shorter than two
                      scrapes, absent() with no "for") apart. No history
                      needed for --rules alone. A template is parsed, never
                      run; a link is checked, never visited
  --severities LIST   the severities the team uses, comma-separated: any
                      other is a problem of fact. Without it, one fewer than
                      two rules use is a guess
  --scrape-interval DUR
                      the scrape interval the range check counts with;
                      without it, the one Prometheus's
                      /api/v1/status/config gives, when it is served

Is the replay right?
  --check-replay      replay the rules, and set them beside what really fired,
                      instead of judging: firings alike, only in the replay,
                      only in what really fired, rule by rule. No incidents
                      needed
  --check-series SEL  the series that say what really fired (repeatable; default
                      ALERTS{alertstate="firing"}, and for Grafana's rules
                      GRAFANA_ALERTS{alertstate="firing"})

What went wrong:
  --incidents FILE    incidents written down, in YAML; see below
  --impact PROMQL     an expression that returns something while users are
                      hurt, such as a 5xx ratio above 1%; its windows become
                      incidents too, so a thin incident log still works. By
                      service, they carry the services hit, for --match
  --impact-for DUR    how long impact must last in a series (default 5m)

Where the history is (needed for --rules, --alerts and --impact):
  --prometheus URL    a Prometheus-compatible API: Prometheus, Thanos, Mimir,
                      VictoriaMetrics, or a cloud's managed Prometheus
  --auth CLOUD        send the credentials that cloud's managed Prometheus
                      wants, from those the machine or pod already has:
                        aws     Amazon Managed Service for Prometheus, signed
                                with SigV4 (the workspace's query URL)
                        azure   Azure Monitor managed service for Prometheus
                                (the workspace's query endpoint)
                        google  Google Cloud Managed Service for Prometheus
  --aws-region REGION the region to sign for, when the URL doesn't say
  --gcp-project ID    the same as --prometheus with Google Cloud Managed
                      Service for Prometheus's URL for this project and
                      --auth google
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
  --noisy-firings N, --noisy-precision P
                      a rule that fired N times or more (default 10) with
                      under P of its firings catching anything (default 20%)
                      is noisy: it fires whatever happens. Its catches are
                      marked, and a longer "for" that would quiet it is
                      suggested when one keeps everything it caught
  --work-hours DAYS@HH:MM-HH:MM, --sleep-hours HH:MM-HH:MM
                      when people are at work, and asleep, in --tz (default
                      mon-fri@08:00-18:00 and 22:00-08:00), or none: the
                      firings that caught nothing are counted by when they
                      came, those that woke people first
  --shifts SHIFTS     hours that take turns instead, for a team on call in
                      shifts, or by turns across time zones: how long each
                      turn is, when the first starts, and each turn's hours,
                      those left out as above, "-" for a turn on them
                      all, like
                      "every=1w from=2026-11-02T09:00; tz=Asia/Seoul;
                      tz=Europe/Berlin sleep=23:00-07:00"

Output:
  --json              the whole report, every firing included
  --metrics           Prometheus metrics, for node_exporter's textfile
                      collector or Pushgateway
  --tz ZONE           show times in this zone (default: local)
  --push URL          also put the metrics on this Pushgateway, as job lapilli;
                      or in the group a URL ending /metrics/job/... names
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
	rulesHistory       string
	loadedRules        bool
	grafana, grafanaTk string

	// ruler is --ruler, rulerTk its token's file, and rulerTenant its
	// tenant.
	ruler, rulerTk, rulerTenant string
	// alarms are each --alarms, its source and its URL.
	alarms []alarmsFlag

	compare            []string
	changedOnly        bool
	checkReplay        bool
	checkReplaySet     bool // --check-replay given, true or false
	checkSeries        []string
	checkContent       bool
	checkContentSet    bool // --check-content given, true or false
	severities         []string
	scrapeInterval     time.Duration
	alerts             bool
	firings, incidents string
	// firingsToken is lapilli serve's --firings-token-file, given: firings
	// copied in may be all there is.
	firingsToken        bool
	impact              string
	impactFor           time.Duration
	promURL, gcpProj    string
	auth, awsRegion     string
	tokenFile           string
	since               time.Duration
	from, to            time.Time
	step                time.Duration
	opts                verdict.Options
	ruleRE              *regexp.Regexp
	json, metrics       bool
	loc                 *time.Location
	timeout             time.Duration
	parallel            int
	push, slackFile     string
	work, sleep, shifts string
}

// alarmsFlag is an --alarms SOURCE=URL.
type alarmsFlag struct{ source, url string }

// ruleFile is a file of rules, and when they took effect; zero for from
// before the period.
type ruleFile struct {
	path string
	from time.Time
}

func parseVerdictFlags(args []string) (*verdictFlags, error) {
	return parseFlags(args, nil)
}

// parseFlags reads lapilli verdict's flags; serve, when not nil, takes the
// flags only lapilli serve has, and says it is serve's.
func parseFlags(args []string, serve func(name, val string) (bool, error)) (*verdictFlags, error) {
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
		case "--changed-only":
			f.changedOnly = true
			continue
		case "--check-replay":
			f.checkReplay, f.checkReplaySet = true, true
			if hasVal {
				b, err := strconv.ParseBool(val)
				if err != nil {
					return nil, fmt.Errorf("--check-replay=%s: want true or false", val)
				}
				f.checkReplay = b
			}
			continue
		case "--check-content":
			f.checkContent, f.checkContentSet = true, true
			if hasVal {
				b, err := strconv.ParseBool(val)
				if err != nil {
					return nil, fmt.Errorf("--check-content=%s: want true or false", val)
				}
				f.checkContent = b
			}
			continue
		case "--loaded-rules":
			f.loadedRules = true
			continue
		}
		if serve != nil && (name == "--read-only" || name == "--notify-slack" || name == "--changes") {
			// On by itself; =false turns it off, as --check-content=false does.
			on := true
			if hasVal {
				b, err := strconv.ParseBool(val)
				if err != nil {
					return nil, fmt.Errorf("%s=%s: want true or false", name, val)
				}
				on = b
			}
			if _, err := serve(name, strconv.FormatBool(on)); err != nil {
				return nil, err
			}
			continue
		}
		if !hasVal {
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s needs a value", name)
			}
			i++
			val = args[i]
		}
		if serve != nil {
			if ok, err := serve(name, val); err != nil {
				return nil, err
			} else if ok {
				f.firingsToken = f.firingsToken || name == "--firings-token-file"
				continue
			}
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
		case "--rules-history":
			f.rulesHistory = val
		case "--compare":
			f.compare = append(f.compare, val)
		case "--check-series":
			f.checkSeries = append(f.checkSeries, val)
		case "--severities":
			for _, sev := range strings.Split(val, ",") {
				if sev = strings.TrimSpace(sev); sev != "" && !slices.Contains(f.severities, sev) {
					f.severities = append(f.severities, sev)
				}
			}
			if len(f.severities) == 0 {
				return nil, fmt.Errorf("--severities %q: want the severities the team uses, like critical,warning", val)
			}
		case "--scrape-interval":
			f.scrapeInterval, err = dur(time.Second)
		case "--grafana":
			if u, perr := url.Parse(val); perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return nil, fmt.Errorf("--grafana %s: want Grafana's URL, like https://grafana.example.com", val)
			}
			f.grafana = val
		case "--grafana-token-file":
			f.grafanaTk = val
		case "--ruler":
			if _, err := source.RulerKind(val); err != nil {
				return nil, err
			}
			f.ruler = strings.TrimRight(val, "/")
		case "--alarms":
			src, u, ok := strings.Cut(val, "=")
			if !ok || !ledger.ValidSource(src) {
				return nil, fmt.Errorf("--alarms %s: want SOURCE=URL, SOURCE lowercase letters, digits and dashes, 40 at most, such as cloudwatch-prod=https://monitoring.eu-west-1.amazonaws.com", val)
			}
			if _, err := source.AlarmsKind(u); err != nil {
				return nil, err
			}
			if slices.ContainsFunc(f.alarms, func(a alarmsFlag) bool { return a.source == src }) {
				return nil, fmt.Errorf("--alarms %s: the source %s is named twice", val, src)
			}
			f.alarms = append(f.alarms, alarmsFlag{src, u})
		case "--ruler-token-file":
			f.rulerTk = val
		case "--ruler-tenant":
			if val == "" || len(val) > 150 || strings.ContainsAny(val, "|/\\\r\n") || val == "." || val == ".." {
				return nil, fmt.Errorf("--ruler-tenant %q: want a tenant's ID, as Mimir and Cortex take one", val)
			}
			f.rulerTenant = val
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
		case "--auth":
			switch {
			case val == "oci" || val == "oracle":
				return nil, fmt.Errorf("--auth %s: Oracle Cloud has no managed Prometheus; give --prometheus the one you run, without --auth", val)
			case !slices.Contains(cloud.Kinds, val):
				return nil, fmt.Errorf("--auth %s: want one of %s", val, strings.Join(cloud.Kinds, ", "))
			}
			f.auth = val
		case "--aws-region":
			f.awsRegion = val
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
		case "--noisy-firings":
			f.opts.NoisyFirings, err = strconv.Atoi(val)
			if err != nil || f.opts.NoisyFirings < 1 {
				return nil, fmt.Errorf("--noisy-firings %s: want a number of firings, 1 or more", val)
			}
		case "--noisy-precision":
			p, perr := strconv.ParseFloat(strings.TrimSuffix(val, "%"), 64)
			if strings.HasSuffix(val, "%") {
				p /= 100
			}
			if perr != nil || p <= 0 || p > 1 {
				return nil, fmt.Errorf("--noisy-precision %s: want a share like 20%% or 0.2", val)
			}
			f.opts.NoisyPrecision = p
		case "--rule":
			if f.ruleRE, err = regexp.Compile(val); err != nil {
				return nil, fmt.Errorf("--rule %s: %v", val, err)
			}
			f.opts.SomeRules = true
		case "--tz":
			if f.loc, err = time.LoadLocation(val); err != nil {
				return nil, fmt.Errorf("--tz %s: %v", val, err)
			}
		case "--work-hours":
			f.work = val
		case "--sleep-hours":
			f.sleep = val
		case "--shifts":
			f.shifts = val
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
	// The hours are read once the zone is known, wherever --tz came.
	f.opts.Hours = verdict.DefaultHours(f.loc)
	if f.work != "" {
		if err := f.opts.Hours.ParseWork(f.work); err != nil {
			return nil, fmt.Errorf("--work-hours %v", err)
		}
	}
	if f.sleep != "" {
		if err := f.opts.Hours.ParseSleep(f.sleep); err != nil {
			return nil, fmt.Errorf("--sleep-hours %v", err)
		}
	}
	if f.shifts != "" {
		sh, err := verdict.ParseShifts(f.shifts, f.opts.Hours)
		if err != nil {
			return nil, fmt.Errorf("--shifts %v", err)
		}
		f.opts.Hours.Shifts = sh
	}
	if f.rulesHistory != "" {
		hs, err := rules.History(f.rulesHistory)
		switch {
		case err != nil:
			return nil, fmt.Errorf("--rules-history %v", err)
		case f.loadedRules || f.grafana != "" || f.ruler != "" || f.alerts || f.firings != "":
			return nil, errors.New("--rules-history is earlier versions of rule files: it goes with --rules, not --loaded-rules, --grafana, --ruler, --alerts or --firings")
		case len(f.compare) > 0:
			return nil, errors.New("--compare sets two sets of rules side by side over one period: leave out --rules-history")
		case serve != nil && len(f.rules) == 0:
			return nil, errors.New("--rules-history is the history of the rules --rules reads: give --rules as well")
		case serve == nil:
			// As each given as --rules DIR@TIME. The rules --rules gives
			// with no time are the rules as they are now, as serve takes
			// them: the newest version, after the directory's newest.
			newest := hs[len(hs)-1].At
			for i := range f.rules {
				if f.rules[i].from.IsZero() {
					f.rules[i].from = newest.Add(time.Second)
				}
			}
			for _, h := range hs {
				f.rules = append(f.rules, ruleFile{path: h.Path, from: h.At})
			}
		}
	}
	sources := 0
	for _, on := range []bool{len(f.rules) > 0, f.loadedRules, f.grafana != "", f.ruler != "", f.alerts, f.firings != ""} {
		if on {
			sources++
		}
	}
	needsHistory := len(f.rules) > 0 || f.loadedRules || f.grafana != "" || f.ruler != "" || len(f.alarms) > 0 || f.alerts || f.impact != ""
	rulesToo := sources > 0 || len(f.alarms) > 0
	if serve != nil {
		for _, no := range []struct {
			on   bool
			flag string
		}{{f.firings != "", "--firings"}, {len(f.compare) > 0, "--compare"}, {f.changedOnly, "--changed-only"},
			{f.json, "--json"}, {f.metrics, "--metrics"}, {f.push != "", "--push: scrape /metrics instead"},
			{!f.from.IsZero(), "--from"}, {!f.to.IsZero(), "--to"}, {len(f.rules) > 1, "--rules more than once: give a directory"},
			{slices.ContainsFunc(f.rules, func(rf ruleFile) bool { return !rf.from.IsZero() }), "--rules FILE@TIME: serve records each version itself, and takes earlier ones with --rules-history"}} {
			if no.on {
				return nil, fmt.Errorf("lapilli serve doesn't take %s", no.flag)
			}
		}
		switch {
		case sources > 1 || sources == 0 && !f.firingsToken && len(f.alarms) == 0:
			return nil, errors.New("say where the firings come from: one of --rules, --loaded-rules, --grafana, --ruler, --alerts, or --alarms; or take them copied in, with --firings-token-file")
		case f.alerts && len(f.alarms) > 0:
			return nil, errors.New("--alarms reads a cloud's alarms to replay or list beside the rules: it doesn't go with --alerts")
		case f.checkReplay && f.checkReplaySet && (f.alerts || !rulesToo):
			return nil, errors.New("--check-replay sets the rules' replay beside what really fired: give the rules, with --rules, --loaded-rules, --grafana or --ruler")
		case len(f.checkSeries) > 0 && f.checkReplaySet && !f.checkReplay:
			return nil, errors.New("--check-series goes with the replay set beside what really fired, which --check-replay=false turns off")
		case f.checkContent && f.checkContentSet && (f.alerts || !rulesToo):
			return nil, errors.New("--check-content checks what the rules say: give the rules, with --rules, --loaded-rules, --grafana or --ruler")
		case (len(f.severities) > 0 || f.scrapeInterval > 0) && f.checkContentSet && !f.checkContent:
			return nil, errors.New("--severities and --scrape-interval go with the content check, which --check-content=false turns off")
		case f.grafanaTk != "" && f.grafana == "":
			return nil, errors.New("--grafana-token-file goes with --grafana")
		case validRuler(f) != nil:
			return nil, validRuler(f)
		case f.promURL == "" && f.gcpProj == "" && rulesToo:
			return nil, errors.New("say where the history is: --prometheus URL or --gcp-project ID")
		case f.promURL == "" && f.gcpProj == "" && f.impact != "":
			return nil, errors.New("--impact reads history: add --prometheus URL or --gcp-project ID")
		}
		return f, validHistory(f)
	}
	if f.checkContent {
		return f, validContent(f, sources)
	}
	switch {
	case len(f.severities) > 0 || f.scrapeInterval > 0:
		return nil, errors.New("--severities and --scrape-interval go with --check-content")
	case sources > 1 || sources == 0 && len(f.alarms) == 0:
		return nil, errors.New("say where the firings come from: one of --rules, --loaded-rules, --grafana, --ruler, --alerts, --firings, or --alarms")
	case len(f.alarms) > 0 && (f.alerts || f.firings != ""):
		return nil, errors.New("--alarms reads a cloud's alarms to replay or list beside the rules: it doesn't go with --alerts or --firings")
	case len(f.alarms) > 0 && len(f.compare) > 0:
		return nil, errors.New("--compare sets two sets of rule files side by side: leave out --alarms")
	case f.grafanaTk != "" && f.grafana == "":
		return nil, errors.New("--grafana-token-file goes with --grafana")
	case validRuler(f) != nil:
		return nil, validRuler(f)
	case f.json && f.metrics:
		return nil, errors.New("--json and --metrics don't go together")
	case len(f.checkSeries) > 0 && !f.checkReplay:
		return nil, errors.New("--check-series goes with --check-replay")
	case f.checkReplay && (f.alerts || f.firings != ""):
		return nil, errors.New("--check-replay sets a replay beside what really fired: give the rules, with --rules, --loaded-rules, --grafana or --ruler")
	case f.checkReplay && len(f.compare) > 0:
		return nil, errors.New("--check-replay doesn't go with --compare")
	case f.checkReplay && (f.metrics || f.push != "" || f.slackFile != ""):
		return nil, errors.New("--check-replay is for you to read, or --json: it doesn't go with --metrics, --push or --slack-webhook-file")
	case f.checkReplay && (f.incidents != "" || f.impact != ""):
		return nil, errors.New("--check-replay judges nothing against incidents: leave out --incidents and --impact")
	case len(f.compare) > 0 && len(f.rules) == 0:
		return nil, errors.New("--compare goes with --rules: the rules proposed, to set beside those in place")
	case len(f.compare) > 0 && slices.ContainsFunc(f.rules, func(rf ruleFile) bool { return !rf.from.IsZero() }):
		return nil, errors.New("--compare sets two sets of rules side by side over one period: give --rules without @TIME")
	case len(f.compare) > 0 && (f.metrics || f.push != ""):
		return nil, errors.New("--compare doesn't go with --metrics or --push")
	case f.changedOnly && len(f.compare) == 0:
		return nil, errors.New("--changed-only goes with --compare")
	case validHistory(f) != nil:
		return nil, validHistory(f)
	case needsHistory && f.promURL == "" && f.gcpProj == "":
		return nil, errors.New("replays, --alerts and --impact read history: add --prometheus URL or --gcp-project ID")
	case f.from.IsZero() != f.to.IsZero():
		return nil, errors.New("--from and --to go together")
	case !f.from.IsZero() && !f.to.After(f.from):
		return nil, errors.New("--to must be after --from")
	case f.incidents == "" && f.impact == "" && !f.checkReplay:
		return nil, errors.New("say what went wrong: --incidents FILE, --impact PROMQL, or both")
	}
	return f, nil
}

// validContent checks the flags of lapilli verdict --check-content.
func validContent(f *verdictFlags, sources int) error {
	switch {
	case !f.checkContentSet || sources > 1 || sources == 0 && len(f.alarms) == 0 || f.alerts || f.firings != "":
		return errors.New("--check-content checks what the rules say: give them with one of --rules, --loaded-rules, --grafana, --ruler, or --alarms")
	case f.checkReplay || len(f.checkSeries) > 0:
		return errors.New("--check-content doesn't go with --check-replay")
	case len(f.compare) > 0 || f.changedOnly:
		return errors.New("--check-content checks the rules given: leave out --compare")
	case f.metrics || f.push != "" || f.slackFile != "":
		return errors.New("--check-content is for you to read, or --json: it doesn't go with --metrics, --push or --slack-webhook-file")
	case f.grafanaTk != "" && f.grafana == "":
		return errors.New("--grafana-token-file goes with --grafana")
	case validRuler(f) != nil:
		return validRuler(f)
	case f.loadedRules && f.promURL == "" && f.gcpProj == "":
		return errors.New("--loaded-rules reads the rules from the API: add --prometheus URL or --gcp-project ID")
	case f.impact != "" && f.promURL == "" && f.gcpProj == "":
		return errors.New("--impact reads history: add --prometheus URL or --gcp-project ID")
	case f.from.IsZero() != f.to.IsZero():
		return errors.New("--from and --to go together")
	case !f.from.IsZero() && !f.to.After(f.from):
		return errors.New("--to must be after --from")
	}
	return validHistory(f)
}

// validRuler checks the flags that read a ruler: its token for Mimir and
// Cortex, a cloud's credentials for Amazon and Azure.
func validRuler(f *verdictFlags) error {
	kind := ""
	if f.ruler != "" {
		kind, _ = source.RulerKind(f.ruler)
	}
	switch {
	case f.ruler == "" && (f.rulerTk != "" || f.rulerTenant != ""):
		return errors.New("--ruler-token-file and --ruler-tenant go with --ruler")
	case f.rulerTk != "" && kind != source.RulerConfig:
		return errors.New("--ruler-token-file is a Mimir's or a Cortex's token: Amazon and Azure are read with --auth")
	case f.rulerTenant != "" && kind != source.RulerConfig:
		return errors.New("--ruler-tenant is a Mimir's or a Cortex's tenant")
	case kind == source.RulerAmazon && f.auth != "aws":
		return errors.New("--ruler on Amazon is read with the pod's identity: add --auth aws")
	case kind == source.RulerAzure && f.auth != "azure":
		return errors.New("--ruler on Azure is read with the pod's identity: add --auth azure")
	}
	return nil
}

// validHistory checks the flags that say where the history is.
func validHistory(f *verdictFlags) error {
	switch {
	case f.promURL != "" && f.gcpProj != "":
		return errors.New("--prometheus and --gcp-project don't go together")
	case f.gcpProj != "" && f.auth != "" && f.auth != "google":
		return errors.New("--gcp-project is Google Cloud's: it goes with --auth google or none")
	case f.auth != "" && f.tokenFile != "":
		return errors.New("--auth and --bearer-token-file don't go together")
	case f.auth != "" && f.promURL == "" && f.gcpProj == "" && f.ruler == "":
		return errors.New("--auth goes with --prometheus URL")
	case f.awsRegion != "" && f.auth != "aws":
		return errors.New("--aws-region goes with --auth aws")
	}
	return nil
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
	return f.prometheus().History(ctx, "lapilli/"+buildVersion())
}

func (f *verdictFlags) prometheus() source.Prometheus {
	return source.Prometheus{URL: f.promURL, GCPProject: f.gcpProj, Auth: f.auth, AWSRegion: f.awsRegion, TokenFile: f.tokenFile, Tenant: f.rulerTenant}
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
	if f.checkReplay {
		return runCheck(ctx, f, versions, stdout, stderr)
	}
	if f.checkContent {
		return runContent(ctx, f, versions, stdout, stderr)
	}
	var incs []incident.Incident
	if f.incidents != "" {
		if incs, err = incident.Load(f.incidents); err != nil {
			return fail(err)
		}
	}
	var same, before, after, was []rules.Rule
	if len(f.compare) > 0 {
		for _, p := range f.compare {
			rs, err := loadRules(p, f.ruleRE)
			if err != nil && !errors.Is(err, rules.ErrNoneInDir) {
				return fail(err)
			}
			was = append(was, rs...)
		}
	}
	// Rules that share a name are told apart alike in every set, so a
	// rule's name doesn't hang on its neighbours in one of them.
	var sets [][]rules.Rule
	for _, v := range versions {
		sets = append(sets, v.Rules)
	}
	source.TellApart(append(sets, was)...)
	if len(f.compare) > 0 {
		var is []rules.Rule
		if len(versions) > 0 {
			is = versions[0].Rules
		}
		same, before, after = source.Split(was, is)
		if len(before)+len(after) == 0 {
			if f.json {
				err = verdict.JSON(stdout, &verdict.Comparison{From: f.opts.From, To: f.opts.To, Same: len(same),
					Rules: []verdict.RuleChange{}, Incidents: []verdict.IncidentChange{}})
			} else {
				_, err = fmt.Fprintf(stdout, "Nothing that decides when the %d rules fire has changed: nothing to replay.\n", len(same))
			}
			if err != nil {
				return fail(err)
			}
			return 0
		}
	}

	var h history
	if len(f.rules) > 0 || readsAPI(f) || f.alerts || f.impact != "" {
		if h, err = newHistory(ctx, f); err != nil {
			return fail(err)
		}
	}
	if readsAPI(f) {
		kept, err := readRules(ctx, f, h, stderr)
		if err != nil {
			return fail(err)
		}
		versions = withRead(versions, kept)
		for _, v := range versions {
			source.TellApart(v.Rules)
		}
	}

	// Impact first: it is one query, and a replay that runs out of time
	// should still be judged against it.
	if f.impact != "" {
		series, err := h.QueryRange(ctx, f.impact, f.opts.From, f.opts.To, f.step)
		if err != nil {
			return fail(fmt.Errorf("--impact: %w", err))
		}
		incs = incident.Merge(incs, incident.FromImpact(series, f.step, f.impactFor), f.opts.UnknownLen, f.opts.Match)
	}

	var known []rules.Rule
	var fs []firing.Firing
	var cmp *verdict.Comparison
	switch {
	case len(f.compare) > 0:
		// Rules alike in both are replayed once, for both; or not at all.
		if f.changedOnly {
			same = nil
		}
		ran, fired := replay.RunEach(ctx, h, [][]replay.Version{{{Rules: same}}, {{Rules: before}}, {{Rules: after}}},
			f.opts.From, f.opts.To, f.step, f.parallel)
		var changed []string
		for _, r := range slices.Concat(before, after) {
			changed = append(changed, r.Name)
		}
		o := f.opts
		o.SomeRules = o.SomeRules || f.changedOnly
		cmp = verdict.Compare(
			verdict.Judge(slices.Concat(ran[0], ran[1]), slices.Concat(fired[0], fired[1]), incs, o),
			verdict.Judge(slices.Concat(ran[0], ran[2]), slices.Concat(fired[0], fired[2]), incs, o),
			changed, len(was)-len(before))
		cmp.ChangedOnly = f.changedOnly
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
	if f.ruleRE != nil && len(versions) == 0 {
		// Replayed rules were picked when they were read.
		kept := fs[:0]
		for _, x := range fs {
			if f.ruleRE.MatchString(x.Rule) {
				kept = append(kept, x)
			}
		}
		fs = kept
	}

	reports := []*verdict.Report{}
	var rep *verdict.Report
	if cmp != nil {
		reports = append(reports, cmp.Before, cmp.After)
	} else {
		rep = verdict.Judge(known, fs, incs, f.opts)
		for _, v := range versions {
			if v.From.After(f.opts.From) && v.From.Before(f.opts.To) {
				rep.RulesChanged = append(rep.RulesChanged, v.From)
			}
		}
		// A rule taken away during the period covers nothing.
		if len(versions) > 1 {
			names := map[string]bool{}
			for _, r := range versions[len(versions)-1].Rules {
				names[r.Name] = true
			}
			rep.Credit(func(rule string) bool { return names[rule] })
		}
		// Rules replayed as one version can be replayed again with a
		// longer "for"; the metrics alone don't show suggestions.
		if len(versions) == 1 && !f.metrics {
			rep.Suggestions = source.Suggest(ctx, h, versions[0].Rules, rep, incs, f.opts, f.step, nil)
		}
		// Each incident nothing caught, against the rules as they stood then.
		if !f.metrics {
			var done bool
			var err error
			rep.NearMisses, done, err = source.NearMisses(ctx, h, versions, rep, incs, f.opts, f.step, f.parallel, nil)
			rep.NearMissesCut = !done
			if err != nil && ctx.Err() == nil {
				fmt.Fprintf(stderr, "lapilli verdict: looking for near misses: %v\n", err)
			}
		}
		reports = append(reports, rep)
	}
	text := func(w io.Writer) error {
		if cmp != nil {
			return verdict.CompareText(w, cmp, f.loc)
		}
		return verdict.Text(w, rep, f.loc)
	}
	switch {
	case f.json && cmp != nil:
		err = verdict.JSON(stdout, cmp)
	case f.json:
		err = verdict.JSON(stdout, rep)
	case f.metrics:
		err = verdict.Prometheus(stdout, rep, now())
	default:
		err = text(stdout)
	}
	if err != nil {
		return fail(err)
	}
	// Each place the report goes gets its own minute: the replay may have
	// used up --timeout, and one slow place shouldn't keep it from another.
	failed := false
	sendTo := func(send func(context.Context) error) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := send(ctx); err != nil {
			fmt.Fprintf(stderr, "lapilli verdict: %v\n", err)
			failed = true
		}
	}
	if f.push != "" {
		sendTo(func(ctx context.Context) error { return report.Push(ctx, f.push, rep, now()) })
	}
	if f.slackFile != "" {
		var b strings.Builder
		if err := text(&b); err != nil {
			return fail(err)
		}
		sendTo(func(ctx context.Context) error { return report.Slack(ctx, f.slackFile, b.String()) })
	}
	// A partial verdict is still worth printing, but not worth a 0.
	all, unfinished := map[string]bool{}, map[string]bool{}
	for _, rep := range reports {
		for _, r := range rep.Rules {
			all[r.Rule] = true
			if r.Unsupported == replay.Unfinished {
				unfinished[r.Rule] = true
			}
		}
	}
	if len(unfinished) > 0 {
		fmt.Fprintf(stderr, "lapilli verdict: %d of %d rules didn't finish within --timeout %s; raise it, or shorten the period\n",
			len(unfinished), len(all), f.timeout)
		return 1
	}
	if failed {
		return 1
	}
	return 0
}

// readRules reads the rules from --loaded-rules, --grafana or --ruler, and
// a cloud's alarms from each --alarms, whose names match --rule.
func readRules(ctx context.Context, f *verdictFlags, h history, stderr io.Writer) ([]rules.Rule, error) {
	var rs []rules.Rule
	var err error
	switch {
	case f.loadedRules:
		rs, _, err = source.Loaded(ctx, h)
	case f.ruler != "":
		var r *source.Ruler
		if r, err = newRuler(ctx, f, stderr); err == nil {
			rs, _, err = r.Read(ctx)
		}
	case f.grafana != "":
		rs, _, err = source.Grafana(ctx, f.grafana, f.grafanaTk, "lapilli/"+buildVersion(), stderr)
	}
	if err != nil {
		return nil, err
	}
	for _, at := range f.alarms {
		a, err := newAlarms(ctx, at)
		if err != nil {
			return nil, err
		}
		more, _, err := a.Read(ctx)
		if err != nil {
			return nil, err
		}
		rs = append(rs, more...)
	}
	kept := rs[:0]
	for _, r := range rs {
		if f.ruleRE == nil || f.ruleRE.MatchString(r.Name) {
			kept = append(kept, r)
		}
	}
	if len(kept) == 0 && len(f.rules) == 0 {
		return nil, errors.New("no alert rules to replay")
	}
	return kept, nil
}

// readsAPI reports whether rules are read from an API: --loaded-rules,
// --grafana, --ruler or --alarms.
func readsAPI(f *verdictFlags) bool {
	return f.loadedRules || f.grafana != "" || f.ruler != "" || len(f.alarms) > 0
}

// withRead are the versions of the rules with those read from an API: in
// place of none, or, beside --rules files, a cloud's alarms added to each.
func withRead(versions []replay.Version, read []rules.Rule) []replay.Version {
	if len(versions) == 0 {
		return []replay.Version{{Rules: read}}
	}
	out := slices.Clone(versions)
	for i := range out {
		out[i].Rules = append(slices.Clip(out[i].Rules), read...)
	}
	return out
}

// newAlarms is the reader of an --alarms, with its cloud's credentials;
// tests replace it.
var newAlarms = func(ctx context.Context, at alarmsFlag) (*source.Alarms, error) {
	a, err := source.NewAlarms(at.source, at.url, "lapilli/"+buildVersion())
	if err != nil {
		return nil, err
	}
	kind := map[string]string{source.AlarmsAWS: "aws", source.AlarmsAzure: "azure", source.AlarmsGoogle: "google", source.AlarmsOCI: "oci"}[a.Kind]
	if a.Reads.Sign, err = cloud.Request(ctx, kind, a.URL, ""); err != nil {
		return nil, fmt.Errorf("--alarms %s: %w", a.Source, err)
	}
	return a, nil
}

// newRuler is the reader of --ruler; tests replace it.
var newRuler = func(ctx context.Context, f *verdictFlags, stderr io.Writer) (*source.Ruler, error) {
	r, err := source.NewRuler(f.ruler, "lapilli/"+buildVersion())
	if err != nil {
		return nil, err
	}
	r.Warn = func(what string) { fmt.Fprintf(stderr, "lapilli verdict: %s\n", what) }
	r.Tenant, r.Reads.TokenFile = f.rulerTenant, f.rulerTk
	if r.Kind != source.RulerConfig {
		if r.Reads.Sign, err = cloud.Request(ctx, f.auth, f.ruler, ""); err != nil {
			return nil, fmt.Errorf("--ruler: %w", err)
		}
	}
	return r, nil
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
		rs, err := loadRules(rf.path, f.ruleRE)
		if err != nil && !(len(f.compare) > 0 && errors.Is(err, rules.ErrNoneInDir)) {
			return nil, err
		}
		versions[i].Rules = append(versions[i].Rules, rs...)
		n += len(rs)
	}
	slices.SortFunc(versions, func(a, b replay.Version) int { return a.From.Compare(b.From) })
	// Only the latest version from before the period matters: it is the one
	// in effect when it starts.
	if k := slices.IndexFunc(versions, func(v replay.Version) bool { return v.From.After(f.opts.From) }); k != 0 {
		if k < 0 {
			k = len(versions)
		}
		versions = versions[k-1:]
	}
	switch first := versions[0].From; {
	case n == 0 && len(f.compare) > 0:
		// All taken away: compared with nothing.
	case n == 0:
		return nil, errors.New("no alert rules to replay")
	case first.After(f.opts.From):
		return nil, fmt.Errorf("the first rules given are from %s, after the period starts: give the rules from before then as --rules FILE, without @TIME",
			first.Format(time.RFC3339))
	}
	return versions, nil
}

// loadRules reads the rules in a file or directory whose names match re, or
// all of them when re is nil.
func loadRules(path string, re *regexp.Regexp) ([]rules.Rule, error) {
	rs, err := rules.Load(path)
	if err != nil {
		return nil, err
	}
	kept := rs[:0]
	for _, r := range rs {
		if re == nil || re.MatchString(r.Name) {
			kept = append(kept, r)
		}
	}
	return kept, nil
}
