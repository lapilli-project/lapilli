package main

import (
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"lapilli.dev/internal/cloud"
	"lapilli.dev/internal/content"
	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/oncall"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/report"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/scenario"
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
  --rules-select SEL  of the Kubernetes objects in the files --rules,
                      --rules-history and --compare read, keep those whose
                      labels match SEL, a label selector as kubectl -l takes
                      it: grafana_alert=1, team in (shop,pay), !draft. A file
                      may be a chart rendered whole, with helm template or
                      kustomize build: its documents that hold no rules are
                      passed over, and counted, with or without it. A rule
                      file that isn't an object has no labels, and is kept
  --loaded-rules      replay the alert rules the API at --prometheus has
                      loaded (GET /api/v1/rules): Prometheus, Thanos, Mimir,
                      vmalert, Amazon Managed Service for Prometheus
  --grafana URL       replay the alert rules this Grafana holds, exported
                      through its provisioning API; the scrape interval
                      each data source says it has is read too, which
                      Grafana puts $__rate_interval in with
  --grafana-token-file FILE
                      a Grafana service account token, to read them with,
                      and, for --check-replay, its alert state history: a
                      Viewer's reads both
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
  --loki-rules PATH   also replay Loki ruler rule files, or a directory of
                      them, against --loki (repeatable): Prometheus's rule
                      format with LogQL, beside any of the above or alone
  --loki-loaded-rules also replay the rules the Loki at --loki holds, as
                      written, from its ruler's API, each group where it
                      runs (GET /loki/api/v1/rules, /prometheus/api/v1/rules)
  --alerts            read the ALERTS series Prometheus keeps for its rules
  --firings FILE      read firings recorded elsewhere, one JSON object a line:
                      {"rule":"...","labels":{...},"start":"...","end":"..."};
                      recipes/firings writes each cloud's alarm history so

Before a change to the rules:
  --compare FILE      the rules in place, to set --rules beside (repeatable):
                      both are replayed over the same period, and the rules
                      that differ and the incidents caught differently are
                      shown before → after. Without --incidents or --impact,
                      the firings are counted, not judged
  --changed-only      with --compare, replay only the rules that differ:
                      much faster, but an incident shows only their catches
  --markdown          with --compare, the report as Markdown, to post on a
                      pull request or a merge request: under 64,000
                      characters, and what comes from the rules written so
                      that it can't format, link or mention anything. Its
                      first line, and --json's "summary", say whether
                      anything changed, and how much (see
                      lapilli.dev/docs/pr-backtests)
  --expect FILE       with --compare, hold the rules that differ to what
                      FILE expects of them: a budget of firings a week and
                      while people slept, each whole week of the period,
                      and incidents they must keep catching
                      (docs/design/expectations.md). The report says, of
                      each, before → after, and what the change broke
  --expect-before FILE
                      the expectations as they were, for the rules in
                      place: a budget the change raised is held to its old
                      value before it, and said
  --fail-expectations exit 1 when the change breaks an expectation that the
                      rules in place meet (one broken before too fails
                      nothing)
  --expect-required   exit 1 when the change adds a rule no expectation
                      holds
  --promtool PATH     with --expect, run each scenario (scenarios:,
                      promtool's own test files, read in the directory of
                      the expectations file that names them) of a rule the
                      change touches, or whose test file it edits (as
                      --expect-before's directory had it), before and
                      after, with promtool at PATH, against the rule's
                      group as each side has it: met, broken, or not known
                      (couldn't be run, and why).
                      Without it, each scenario is not known. Only rules in
                      Prometheus's format; promtool runs with no
                      environment, 30s a scenario, its rule_files the
                      group alone
  --promtool-memory SIZE
                      the memory one run of --promtool may take, as 256Mi
                      or 1Gi, 64Mi at least (default 256Mi): on Linux,
                      promtool's RLIMIT_DATA, past which the run stops and
                      the scenario is not known. Not set on macOS, where
                      promtool runs without a cap
  --draft-expectations FILE
                      write FILE, an expectations file drafted from the
                      period judged, as each rule is now: the most it fired
                      in a whole week of it, while people slept and
                      re-fired, rounded up, and the incidents it caught
                      (docs/design/expectations.md). Counted, never
                      written by a model: the same period gives the same
                      draft. Without --incidents or --impact, budgets alone

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
                      needed for --rules alone. With --grafana, also the
                      notification templates each Grafana rule's contact
                      point is written with: an annotation shown the rule
                      hasn't, a label named its alerts never had. A
                      template is parsed, never run; a link is checked,
                      never visited
  --severities LIST   the severities the team uses, comma-separated: any
                      other is a problem of fact. Without it, one fewer than
                      two rules use is a guess
  --runbook-annotations LIST
                      the annotations a rule's runbook is read from, the
                      first it has, comma-separated (default
                      runbook_url,runbook,playbook_url,playbook). A runbook
                      is an http or https URL, or a path in the repository
                      the rules are kept in, with an anchor or not
                      (docs/runbooks/disk.md#full)
  --scrape-interval DUR
                      the scrape interval the range check counts with;
                      without it, those Prometheus's /api/v1/status/config
                      gives, when it is served: a range over a selector
                      naming its job counts with the job's own, by its job
                      label, as /api/v1/targets says; any other, the global
                      interval. A replay asks the server for Grafana's
                      reduces that don't count points at this resolution;
                      without it, at the shortest interval the series they
                      read are scraped at, as those say, or, where they
                      aren't served, at Grafana's own (1s). A Grafana data
                      source --grafana can't say the scrape interval of is
                      taken as having this one, for $__rate_interval

Is the replay right?
  --check-replay      replay the rules, and set them beside what really fired,
                      instead of judging: firings alike, only in the replay,
                      only in what really fired, rule by rule. No incidents
                      needed
  --check-series SEL  the series that say what really fired (repeatable; default
                      ALERTS{alertstate="firing"}, and for Grafana's rules
                      GRAFANA_ALERTS{alertstate="firing",
                      grafana_alertstate!="error"}: a query that failed
                      can't be replayed, so its error alerts are left out).
                      Without it, what Grafana's rules really fired is read
                      from Grafana's own alert state history instead, when
                      --grafana or --check-grafana says which Grafana and it
                      keeps one it serves: in annotations, its default, from
                      Grafana 12.4 on, or in Loki; and GRAFANA_ALERTS first,
                      while it holds anything, where Grafana may keep it
                      too. Read from a day before the period; where it can't
                      be read, Grafana's rules aren't checked, and say why
  --check-grafana URL the Grafana that runs the Grafana rules --rules gives,
                      whose alert state history says what they really fired,
                      read with --grafana-token-file
  --check-firings FILE
                      what a cloud's own alarms really fired, as
                      recipes/firings writes it, for the PromQL alarms
                      replayed (CloudWatch, Google Cloud, Azure), which keep
                      no series: each tied by its alarm_id, or its name

What went wrong (with --compare, if you have them):
  --incidents FILE    incidents written down, in YAML; see below
  --impact PROMQL     an expression that returns something while users are
                      hurt, such as a 5xx ratio above 1%; its windows become
                      incidents too, so a thin incident log still works. By
                      service, they carry the services hit, for --match.
                      It keeps the record from where the data it reads
                      begins, none for a selector with no data
  --impact NAME=PROMQL
                      one of several signals of user impact, each named
                      (repeatable, 8 at most): a probe failing, the 5xx
                      share. Each signal's windows are its own, never
                      merged with another's, and each keeps the record of
                      the firings it could have seen from where the data it
                      reads begins. A name is a lowercase letter, then
                      letters, digits, - and _; named and unnamed don't go
                      together (docs/design/signals.md)
  --impact-for DUR    how long impact must last in a series (default 5m);
                      NAME=DUR for one signal's
  --impact-match NAME=LABELS
                      the labels, comma-separated, a firing must agree with
                      a signal's windows on, in place of --match; NAME= for
                      none: any firing in time catches them
  --impact-record NAME=no
                      reading the signal over a time doesn't keep its record:
                      it says users weren't hurt in one way, not that nothing
                      was. Its windows still keep the record near them
  --impact-lead NAME=DUR
                      how long before one of the signal's windows starts a
                      firing still catches it, in place of --lead, from 0 to
                      1d: a probe that fails as users are hurt, 5m. Incidents
                      written down keep --lead
  --impact-file FILE  the named signals in a file, and which rules are judged
                      against which (see below), in place of the flags above

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
  --loki URL          a Loki's API, which rules written in LogQL are replayed
                      against: --loki-rules, --loki-loaded-rules, and
                      Grafana's rules on Loki read from files. Grafana's
                      read with --grafana are asked through Grafana's data
                      source proxy, as Grafana asks them
  --loki-token-file FILE
                      send this token to --loki
  --loki-tenant ID    the tenant of a Loki with tenants and no gateway to set
                      it, sent as X-Scope-OrgID to --loki

When:
  --since DUR         the last DUR (default 7d)
  --from TIME, --to TIME
                      an RFC 3339 period instead
  --step DUR          how often rules are evaluated in a replay (default 1m)
  --late DUR          replay as if metrics arrived DUR after their time, as a
                      cloud's own metrics often do: each evaluation reads the
                      history as it stood DUR before it, unless a Prometheus
                      group's query_offset, or a Grafana query's window end,
                      reads earlier still. PATTERN=DUR for only the rules
                      whose name, or the file or cloud they came from,
                      matches (repeatable; the first that matches counts)

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
  --on-call FILE      on-call schedules, read from the iCalendar feeds on-call
                      tools export, and the people in them with their hours:
                      --shifts on-call=NAME then takes turns by the schedule
                      NAME, each by its person's hours (see
                      lapilli.dev/docs/how-it-judges)
  --shifts SHIFTS     hours that take turns instead, for a team on call in
                      shifts, or by turns across time zones: how long each
                      turn is, when the first starts, and each turn's hours,
                      those left out as above, "-" for a turn on them
                      all, like
                      "every=1w from=2026-11-02T09:00; tz=Asia/Seoul;
                      tz=Europe/Berlin sleep=23:00-07:00"

Output:
  --json              the whole report, every firing included
  --csv               the report's firings, a row each, as CSV: rule,
                      severity, labels, start, end, stone, the incidents
                      it caught, re-fired. A rule's name or severity that
                      starts with =, +, - or @ is written with ' before
                      it, so a spreadsheet doesn't run it. docs/numbers.md
                      defines each column and number
  --metrics           Prometheus metrics, for node_exporter's textfile
                      collector or Pushgateway
  --redact            give every name an alias, to show the report outside:
                      rules rule-01 on, label values by label (service-01),
                      incidents incident-01 on by when they started, their
                      titles left out, teams, groups and sources; why a rule
                      couldn't be replayed and the expressions a near miss
                      quotes, and time zones, said as [redacted]. Numbers,
                      severities, durations and times, in UTC, stay. The
                      aliases are this output's own, numbered in an order
                      no name decides. stderr isn't redacted
                      The text, --json, --csv, --markdown and --metrics,
                      and what --push and --slack-webhook-file send
  --redact-key FILE   --redact with aliases that hold across outputs: each a
                      keyed hash (HMAC-SHA256) of the name, with the key in
                      FILE, a random one in hex, 32 digits or more (openssl
                      rand -hex 32), never shown. Without the key a short
                      name can't be found by trying names
  --tz ZONE           show times in this zone, an IANA name such as
                      Asia/Seoul or UTC (default: the machine's own, as TZ
                      or /etc/localtime says); the header names it, with
                      its offset
  --push URL          also put the metrics on this Pushgateway, as job lapilli;
                      or in the group a URL ending /metrics/job/... names
  --slack-webhook-file FILE
                      also post the report to the Slack incoming webhook
                      whose URL is in FILE
  --timeout DUR       give up after this long (default 10m)
  --extras DUR        let the steps after the replays take this long at most:
                      a longer "for" for the noisy rules, near misses and
                      what fired with no record, each a replay or questions
                      of the history again, as long again as the replays at
                      times; 0 skips them (default: what --timeout leaves)
  --parallel N        replay up to N rules at once against each history
                      API, --prometheus, --loki and each of Grafana's data
                      sources, and ask a Grafana N requests at once at most
                      for all its data sources (default 8)
  --max-rate N        ask each API the history is read from (--prometheus,
                      --loki, a Grafana's data sources together) N requests
                      a second at most, spaced evenly, from 0.001: 0.5 for a
                      quota of 30 a minute. Without it, an API that answers
                      HTTP 429 is asked fewer at once, and the request asked
                      again after the wait it says (Retry-After), or after
                      waits of up to three minutes in all
  --max-samples N     read the answers to one read of a rule's query to N
                      samples in all at most (default 10000000, some 600 MB
                      at the peak, 64 bytes a sample): past it, the rule
                      isn't replayed, as one whose query failed. A replay
                      reads a day at a time, on days from midnight UTC: a day
                      at a minute holds 1,440 samples a series firing all
                      through it, so the default is some 6,900 such series;
                      a longer --step holds fewer
  --progress=false    don't say on stderr how a run is going: past 15s, every
                      30s, the rules replayed, the requests, those asked
                      again and those answered HTTP 429. A run the history
                      answered HTTP 429 still says so at its end
  --fail-unreplayed   exit 1 when a rule's replay failed because the history
                      couldn't be read: not reached, not let in (HTTP 401,
                      403), turned away to the end (429, 5xx); or when a
                      rule's labels on each of its series are more than
                      --max-samples lets a replay hold, which a rule's
                      writer could do to keep it out unseen. The report is
                      of the others. Not for a query the history refused as
                      written (400, 422), which another run is refused too,
                      nor for a Grafana rule replayed whose query outside
                      its condition failed. A run --timeout cut short exits
                      1 either way

The incidents file:

  incidents:
    - id: INC-12
      title: Checkout errors
      start: 2026-10-30T17:00:00+09:00
      end: 2026-10-30T17:40:00+09:00      # optional; 1h when missing
      labels: {service: [checkout, cart]} # optional, for --match

The impact file:

  signals:
    - {name: errors, expr: "...", for: 5m, match: [service, team]}
    - {name: probe, expr: "probe_success == 0", for: 2m, match: [service]}
    - {name: latency, expr: "...", for: 10m}
    - {name: payments, expr: "...", for: 1m, record: false}
  rules:                          # optional: a rule's first entry decides
    - rules: Slow$|Latency        # rules whose names match, or else
      labels: {kind: latency}     # whose labels have these values,
      signals: [latency]          # judged against these signals' windows alone
    - rules: ^BackendMissing$
      signals: [errors, probe]
      match: [backend]            # and matched on these labels alone
    - rules: ^(MemoryPressure|PodRestarting)$
      signals: [errors]           # causes, which fire before the errors:
      lead: 1h                    # caught from 1h before a window, in place
      require: [service]          # of each signal's lead (lead: on a signal
                                  # too), and only when both carry service
    - rules: ^Watchdog$
      judged: false               # judged by nothing: counted, never white
                                  # nor black (entries all so need no signals)

lapilli only reads history and files. It sends the report nowhere but to
--push and --slack-webhook-file.
`

type verdictFlags struct {
	// warn is where what a run says on its way goes, stderr: nil for
	// nowhere.
	warn         io.Writer
	rules        []ruleFile
	rulesHistory string
	rulesSelect  rules.Selector
	picked       rules.Picked // what reading the rule files passed over
	// recorded are, with --compare, the recording rules the files read
	// held; passedOver what each set's files passed over, as said.
	recorded []rules.Recording
	// noneSelected is what --rules-select keeping no rule of --rules
	// said, nil when it kept some or none was asked.
	noneSelected       error
	passedOver         map[string]string
	loadedRules        bool
	grafana, grafanaTk string

	// ruler is --ruler, rulerTk its token's file, and rulerTenant its
	// tenant.
	ruler, rulerTk, rulerTenant string
	// alarms are each --alarms, its source and its URL.
	alarms []alarmsFlag

	compare     []string
	changedOnly bool
	// expect and expectBefore are --expect and --expect-before;
	// failExpect, --fail-expectations; expectRequired, --expect-required.
	expect, expectBefore       string
	failExpect, expectRequired bool
	// promtool is --promtool, resolved (scenario.Resolve); promtoolMemory,
	// --promtool-memory, the memory one run may take, as serve's.
	promtool          string
	promtoolMemory    uint64
	promtoolMemorySet bool
	// draft is --draft-expectations: the file a draft from the history
	// is written to.
	draft          string
	checkReplay    bool
	checkReplaySet bool // --check-replay given, true or false
	checkSeries    []string
	checkFirings   string
	// checkGrafana is --check-grafana: the Grafana that runs the Grafana
	// rules --rules gives, whose alert state history says what they
	// really fired.
	checkGrafana       string
	checkContent       bool
	checkContentSet    bool // --check-content given, true or false
	severities         []string
	runbooks           []string
	scrapeInterval     time.Duration
	alerts             bool
	firings, incidents string
	// firingsToken is lapilli serve's --firings-token-file, given: firings
	// copied in may be all there is.
	firingsToken bool
	// impact is --impact with no name; signals, each --impact NAME=PROMQL,
	// or --impact-file's (docs/design/signals.md), and impactSets what the
	// flags said of each by name, until all are read.
	impact           string
	impactFor        time.Duration
	signals          []impactSignal
	impactSets       map[string]*impactSet
	impactFile       string
	promURL, gcpProj string
	auth, awsRegion  string
	tokenFile        string
	since            time.Duration
	from, to         time.Time
	step             time.Duration
	opts             verdict.Options
	ruleRE           *regexp.Regexp
	late             rules.Lateness
	json, metrics    bool
	csv              bool
	markdown         bool
	loc              *time.Location
	timeout          time.Duration
	// extras is how long the steps after the replays may take, set by
	// --extras (extrasSet); 0 skips them.
	extras              time.Duration
	extrasSet           bool
	parallel            int
	push, slackFile     string
	work, sleep, shifts string

	// dataSources are the scrape intervals Grafana's data sources say
	// they have, as read with the rules from --grafana, and kinds their
	// types; scrapes, without --scrape-interval, those Prometheus's
	// configuration says.
	dataSources rules.DataSources
	kinds       map[string]string
	scrapes     *content.Scrapes
	// onCall is --on-call's file, as read.
	onCall *oncall.Config

	// loki is --loki, with its token's file and tenant; lokiRules each
	// --loki-rules, and lokiLoaded --loki-loaded-rules; places where the
	// Loki's ruler runs each group, as read with its rules.
	loki, lokiTk, lokiTenant string
	lokiRules                []string
	lokiLoaded               bool
	places                   map[rules.Place]time.Duration

	// maxRate is --max-rate, 0 for none; maxSamples --max-samples, 0 for
	// prom.DefaultMaxSamples; noProgress --progress=false, and
	// progressSet whether --progress was given; failUnreplayed
	// --fail-unreplayed.
	maxRate                 float64
	maxSamples              int
	noProgress, progressSet bool
	failUnreplayed          bool
	// lokiLimit is the Limiter every client of --loki shares, made once
	// (lokiSource).
	lokiLimit *prom.Limiter
	// grafanaPace and grafanaCap are the Grafana's --max-rate and
	// --parallel, which its data sources and its state history are asked
	// by together, made once (grafanaLimits).
	grafanaPace *prom.Pace
	grafanaCap  *prom.Limiter

	// redact is --redact, and redactKey --redact-key's key, read.
	redact    bool
	redactKey []byte
}

// alarmsFlag is an --alarms SOURCE=URL.
type alarmsFlag struct{ source, url string }

// ruleFile is a file of rules, and when they took effect; zero for from
// before the period.
type ruleFile struct {
	path string
	from time.Time
}

// annotationName is a name an annotation can have: a label's, as
// Prometheus and Grafana allow them.
var annotationName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,127}$`)

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
		// As serve's: a quarter of the chart's memory limit.
		promtoolMemory: defaultPromtoolMemory,
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
		case "--csv":
			f.csv = true
			continue
		case "--markdown":
			f.markdown = true
			continue
		case "--redact":
			f.redact = true
			continue
		case "--metrics":
			f.metrics = true
			continue
		case "--changed-only":
			f.changedOnly = true
			continue
		case "--fail-expectations":
			f.failExpect = true
			continue
		case "--expect-required":
			f.expectRequired = true
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
		case "--loki-loaded-rules":
			f.lokiLoaded = true
			continue
		case "--fail-unreplayed":
			f.failUnreplayed = true
			continue
		case "--progress":
			f.progressSet = true
			if hasVal {
				b, err := strconv.ParseBool(val)
				if err != nil {
					return nil, fmt.Errorf("--progress=%s: want true or false", val)
				}
				f.noProgress = !b
			}
			continue
		}
		if serve != nil && (name == "--read-only" || name == "--notify-slack" || name == "--changes" || name == "--everyone-admin" || name == "--oidc-bearer" ||
			name == "--config-prune" || name == "--goal-tasks" || name == "--expect-tasks" || name == "--yield-on-429") {
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
		case "--rules-select":
			if f.rulesSelect, err = rules.ParseSelector(val); err != nil {
				return nil, fmt.Errorf("--rules-select %v", err)
			}
		case "--compare":
			f.compare = append(f.compare, val)
		case "--expect":
			f.expect = val
		case "--expect-before":
			f.expectBefore = val
		case "--draft-expectations":
			f.draft = val
		case "--promtool":
			if f.promtool, err = scenario.Resolve(val); err != nil {
				return nil, fmt.Errorf("--promtool %s: %w", val, err)
			}
		case "--promtool-memory":
			if f.promtoolMemory, err = memorySize(val); err != nil {
				return nil, fmt.Errorf("--promtool-memory %s: %v", val, err)
			}
			f.promtoolMemorySet = true
		case "--check-series":
			f.checkSeries = append(f.checkSeries, val)
		case "--check-firings":
			f.checkFirings = val
		case "--check-grafana":
			if u, perr := url.Parse(val); perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
				return nil, fmt.Errorf("--check-grafana %s: want Grafana's URL, like https://grafana.example.com", val)
			}
			f.checkGrafana = strings.TrimRight(val, "/")
		case "--severities":
			for _, sev := range strings.Split(val, ",") {
				if sev = strings.TrimSpace(sev); sev != "" && !slices.Contains(f.severities, sev) {
					f.severities = append(f.severities, sev)
				}
			}
			if len(f.severities) == 0 {
				return nil, fmt.Errorf("--severities %q: want the severities the team uses, like critical,warning", val)
			}
		case "--runbook-annotations":
			for _, a := range strings.Split(val, ",") {
				if a = strings.TrimSpace(a); a != "" && !slices.Contains(f.runbooks, a) {
					if !annotationName.MatchString(a) {
						return nil, fmt.Errorf("--runbook-annotations %s: %q isn't an annotation's name", val, a)
					}
					f.runbooks = append(f.runbooks, a)
				}
			}
			if len(f.runbooks) == 0 {
				return nil, fmt.Errorf("--runbook-annotations %q: want the annotations a runbook is in, like runbook_url,playbook", val)
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
		case "--loki":
			if u, perr := url.Parse(val); perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
				return nil, fmt.Errorf("--loki %s: want Loki's URL, like http://loki:3100", val)
			}
			f.loki = strings.TrimRight(val, "/")
		case "--loki-token-file":
			f.lokiTk = val
		case "--loki-tenant":
			if val == "" || len(val) > 150 || strings.ContainsAny(val, "|/\\\r\n") || val == "." || val == ".." {
				return nil, fmt.Errorf("--loki-tenant %q: want a tenant's ID, as Loki takes one", val)
			}
			f.lokiTenant = val
		case "--loki-rules":
			f.lokiRules = append(f.lokiRules, val)
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
		case "--redact-key":
			key, rerr := readRedactKey(val)
			if rerr != nil {
				return nil, rerr
			}
			f.redact, f.redactKey = true, key
		case "--incidents":
			f.incidents = val
		case "--impact", "--impact-for", "--impact-match", "--impact-record", "--impact-lead", "--impact-file":
			_, err = f.impactFlag(name, val)
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
			// Any name, as ever: one that isn't a label's name, as PromQL
			// names one (an OpenTelemetry name with dots), is never put in
			// a query; a named signal takes it to have seen every value of
			// it (source.SeenFrom).
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
		case "--late":
			l, lerr := rules.ParseLate(val)
			if lerr != nil {
				return nil, lerr
			}
			f.late = append(f.late, l)
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
		case "--on-call":
			c, err := oncall.Load(val)
			if err != nil {
				return nil, fmt.Errorf("--on-call %s: %v", val, err)
			}
			f.onCall = c
		case "--timeout":
			f.timeout, err = dur(time.Second)
		case "--extras":
			f.extrasSet = true
			if val != "0" {
				f.extras, err = dur(time.Second)
			}
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
		case "--max-rate":
			f.maxRate, err = strconv.ParseFloat(val, 64)
			if err != nil || !(f.maxRate >= prom.MinRate) || f.maxRate > 10000 {
				return nil, fmt.Errorf("--max-rate %s: want requests a second, from %g, like 5 or 0.5", val, prom.MinRate)
			}
		case "--max-samples":
			f.maxSamples, err = strconv.Atoi(val)
			if err != nil || f.maxSamples < 1000 || f.maxSamples > 1000000000 {
				return nil, fmt.Errorf("--max-samples %s: want a number of samples from 1000 to 1000000000, like 40000000", val)
			}
		default:
			return nil, fmt.Errorf("unknown flag %s", a)
		}
		if err != nil {
			return nil, err
		}
	}
	// The named impact signals, once --step and --match are known, wherever
	// they came.
	if err := f.signalsRead(); err != nil {
		return nil, err
	}
	// The hours are read once the zone is known, wherever --tz came; and
	// the days episodes are parted by.
	f.opts.Hours = verdict.DefaultHours(f.loc)
	f.opts.Zone = f.loc
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
	if name, ok := verdict.OnCallShifts(f.shifts); ok && (f.onCall == nil || f.onCall.Schedule(name) == nil) {
		return nil, fmt.Errorf("--shifts on-call=%s: no schedule %s in --on-call", name, name)
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
	if !f.rulesSelect.Empty() && len(f.rules) == 0 && f.rulesHistory == "" && len(f.compare) == 0 {
		return nil, errors.New("--rules-select picks among the documents of rule files: give --rules or --rules-history")
	}
	// serve given --rules-history without --rules follows it: the history
	// is the rules.
	following := serve != nil && f.rulesHistory != "" && len(f.rules) == 0
	sources := 0
	for _, on := range []bool{len(f.rules) > 0 || following, f.loadedRules, f.grafana != "", f.ruler != "", f.alerts, f.firings != ""} {
		if on {
			sources++
		}
	}
	needsHistory := len(f.rules) > 0 || following || f.loadedRules || f.grafana != "" || f.ruler != "" || len(f.alarms) > 0 || f.alerts || f.anyImpact() ||
		f.checkReplay && f.checkReplaySet
	lokiToo := len(f.lokiRules) > 0 || f.lokiLoaded
	rulesToo := sources > 0 || len(f.alarms) > 0 || lokiToo
	if err := validLoki(f, serve != nil); err != nil {
		return nil, err
	}
	if serve == nil && f.extrasSet && len(f.compare) > 0 {
		return nil, errors.New("--extras: --compare has no steps after the replays to bound")
	}
	if serve != nil {
		for _, no := range []struct {
			on   bool
			flag string
		}{{f.firings != "", "--firings"}, {len(f.compare) > 0, "--compare"}, {f.changedOnly, "--changed-only"},
			{f.expect != "" || f.expectBefore != "" || f.failExpect || f.expectRequired, "--expect, --expect-before, --fail-expectations or --expect-required: they hold a change to the rules, which --compare backtests"},
			{f.json, "--json"}, {f.csv, "--csv: ask /api/v1/report?format=csv"}, {f.redact, "--redact: ask /api/v1/report?redact=true"}, {f.markdown, "--markdown"}, {f.metrics, "--metrics"}, {f.checkFirings != "", "--check-firings: it reads what a cloud's alarms fired from the firings copied in, with --firings-token-file"}, {f.push != "", "--push: scrape /metrics instead"},
			{!f.from.IsZero(), "--from"}, {!f.to.IsZero(), "--to"}, {len(f.rules) > 1, "--rules more than once: give a directory"},
			{slices.ContainsFunc(f.rules, func(rf ruleFile) bool { return !rf.from.IsZero() }), "--rules FILE@TIME: serve records each version itself, and takes earlier ones with --rules-history"},
			{f.progressSet, "--progress"}, {f.failUnreplayed, "--fail-unreplayed"},
			{f.draft != "", "--draft-expectations: ask GET /api/v1/expectations/draft"},
			{f.extrasSet, "--extras: each run's steps after the replays take what --timeout leaves"}} {
			if no.on {
				return nil, fmt.Errorf("lapilli serve doesn't take %s", no.flag)
			}
		}
		switch {
		case sources > 1 || sources == 0 && !f.firingsToken && len(f.alarms) == 0 && !lokiToo:
			return nil, errors.New("say where the firings come from: one of --rules, --loaded-rules, --grafana, --ruler, --alerts, --alarms or --loki-rules; or take them copied in, with --firings-token-file")
		case f.alerts && lokiToo:
			return nil, errors.New("--loki-rules and --loki-loaded-rules read rules to replay beside the others: they don't go with --alerts")
		case f.alerts && len(f.alarms) > 0:
			return nil, errors.New("--alarms reads a cloud's alarms to replay or list beside the rules: it doesn't go with --alerts")
		case f.checkReplay && f.checkReplaySet && (f.alerts || !rulesToo):
			return nil, errors.New("--check-replay sets the rules' replay beside what really fired: give the rules, with --rules, --loaded-rules, --grafana or --ruler")
		case len(f.checkSeries) > 0 && f.checkReplaySet && !f.checkReplay:
			return nil, errors.New("--check-series goes with the replay set beside what really fired, which --check-replay=false turns off")
		case validCheckGrafana(f, f.checkReplaySet && !f.checkReplay) != nil:
			return nil, validCheckGrafana(f, f.checkReplaySet && !f.checkReplay)
		case f.checkContent && f.checkContentSet && (f.alerts || !rulesToo):
			return nil, errors.New("--check-content checks what the rules say: give the rules, with --rules, --loaded-rules, --grafana or --ruler")
		case len(f.severities) > 0 && f.checkContentSet && !f.checkContent:
			return nil, errors.New("--severities goes with the content check, which --check-content=false turns off")
		case f.grafanaTk != "" && f.grafana == "" && f.checkGrafana == "":
			return nil, errors.New("--grafana-token-file goes with --grafana or --check-grafana")
		case validRuler(f) != nil:
			return nil, validRuler(f)
		case f.promURL == "" && f.gcpProj == "" && rulesToo:
			return nil, errors.New("say where the history is: --prometheus URL or --gcp-project ID")
		case f.promURL == "" && f.gcpProj == "" && f.anyImpact():
			return nil, errors.New("--impact reads history: add --prometheus URL or --gcp-project ID")
		}
		return f, validHistory(f)
	}
	if f.checkContent {
		return f, validContent(f, sources)
	}
	switch {
	case len(f.severities) > 0:
		return nil, errors.New("--severities goes with --check-content")
	case sources > 1 || sources == 0 && len(f.alarms) == 0 && !lokiToo:
		return nil, errors.New("say where the firings come from: one of --rules, --loaded-rules, --grafana, --ruler, --alerts, --firings, --alarms or --loki-rules")
	case lokiToo && (f.alerts || f.firings != ""):
		return nil, errors.New("--loki-rules and --loki-loaded-rules read rules to replay beside the others: they don't go with --alerts or --firings")
	case lokiToo && len(f.compare) > 0:
		return nil, errors.New("--compare sets two sets of rule files side by side: leave out --loki-rules and --loki-loaded-rules")
	case len(f.alarms) > 0 && (f.alerts || f.firings != ""):
		return nil, errors.New("--alarms reads a cloud's alarms to replay or list beside the rules: it doesn't go with --alerts or --firings")
	case len(f.alarms) > 0 && len(f.compare) > 0:
		return nil, errors.New("--compare sets two sets of rule files side by side: leave out --alarms")
	case f.grafanaTk != "" && f.grafana == "" && f.checkGrafana == "":
		return nil, errors.New("--grafana-token-file goes with --grafana or --check-grafana")
	case validRuler(f) != nil:
		return nil, validRuler(f)
	case f.json && f.metrics:
		return nil, errors.New("--json and --metrics don't go together")
	case f.csv && (f.json || f.metrics || f.markdown):
		return nil, errors.New("--csv doesn't go with --json, --metrics or --markdown: give one")
	case f.csv && (len(f.compare) > 0 || f.checkReplay):
		return nil, errors.New("--csv writes a verdict's firings: it doesn't go with --compare or --check-replay")
	case len(f.checkSeries) > 0 && !f.checkReplay:
		return nil, errors.New("--check-series goes with --check-replay")
	case f.checkFirings != "" && !f.checkReplay:
		return nil, errors.New("--check-firings goes with --check-replay: what a cloud's alarms really fired, to set their replay beside")
	case validCheckGrafana(f, !f.checkReplay) != nil:
		return nil, validCheckGrafana(f, !f.checkReplay)
	case f.checkReplay && (f.alerts || f.firings != ""):
		return nil, errors.New("--check-replay sets a replay beside what really fired: give the rules, with --rules, --loaded-rules, --grafana or --ruler")
	case f.checkReplay && len(f.compare) > 0:
		return nil, errors.New("--check-replay doesn't go with --compare")
	case f.checkReplay && f.redact:
		return nil, errors.New("--redact gives a verdict's names aliases: it doesn't go with --check-replay")
	case f.checkReplay && (f.metrics || f.push != "" || f.slackFile != ""):
		return nil, errors.New("--check-replay is for you to read, or --json: it doesn't go with --metrics, --push or --slack-webhook-file")
	case f.checkReplay && (f.incidents != "" || f.anyImpact()):
		return nil, errors.New("--check-replay judges nothing against incidents: leave out --incidents and --impact")
	case len(f.compare) > 0 && len(f.rules) == 0:
		return nil, errors.New("--compare goes with --rules: the rules proposed, to set beside those in place")
	case len(f.compare) > 0 && slices.ContainsFunc(f.rules, func(rf ruleFile) bool { return !rf.from.IsZero() }):
		return nil, errors.New("--compare sets two sets of rules side by side over one period: give --rules without @TIME")
	case len(f.compare) > 0 && (f.metrics || f.push != ""):
		return nil, errors.New("--compare doesn't go with --metrics or --push")
	case f.changedOnly && len(f.compare) == 0:
		return nil, errors.New("--changed-only goes with --compare")
	case f.expect != "" && len(f.compare) == 0:
		return nil, errors.New("--expect holds a change to the rules to what they are expected to do: it goes with --compare")
	case f.expect == "" && (f.expectBefore != "" || f.failExpect || f.expectRequired || f.promtool != ""):
		return nil, errors.New("--expect-before, --fail-expectations, --expect-required and --promtool go with --expect")
	case f.promtoolMemorySet && f.promtool == "":
		return nil, errors.New("--promtool-memory is the memory --promtool's runs may take: give --promtool")
	case f.draft != "" && (len(f.compare) > 0 || f.checkReplay):
		return nil, errors.New("--draft-expectations drafts what the rules judged are held to: it doesn't go with --compare or --check-replay")
	case f.draft != "" && f.redact:
		return nil, errors.New("--draft-expectations names each rule, which --redact gives an alias: give one")
	case f.markdown && len(f.compare) == 0:
		return nil, errors.New("--markdown writes --compare's report, for a pull request: it goes with --compare")
	case f.markdown && f.json:
		return nil, errors.New("--json and --markdown don't go together")
	case validHistory(f) != nil:
		return nil, validHistory(f)
	case needsHistory && f.promURL == "" && f.gcpProj == "":
		return nil, errors.New("replays, --alerts, --impact and --check-replay read history: add --prometheus URL or --gcp-project ID")
	case f.from.IsZero() != f.to.IsZero():
		return nil, errors.New("--from and --to go together")
	case !f.from.IsZero() && !f.to.After(f.from):
		return nil, errors.New("--to must be after --from")
	case f.incidents == "" && !f.anyImpact() && !f.checkReplay && len(f.compare) == 0 && f.draft == "":
		// --compare counts the firings on each side even with nothing to
		// judge them against: what a change to the rules does to how often
		// they fire is worth knowing alone.
		return nil, errors.New("say what went wrong: --incidents FILE, --impact PROMQL, or both")
	}
	return f, nil
}

// validCheckGrafana checks --check-grafana: the Grafana running the
// Grafana rules in files, whose state history is read; off says the check
// is.
func validCheckGrafana(f *verdictFlags, off bool) error {
	switch {
	case f.checkGrafana == "":
		return nil
	case off:
		return errors.New("--check-grafana goes with --check-replay: what Grafana's rules really fired, to set their replay beside")
	case f.grafana != "":
		return errors.New("--check-grafana is for Grafana's rules read from files: with --grafana, that Grafana's own state history is read")
	case len(f.checkSeries) > 0:
		return errors.New("--check-grafana reads what Grafana's rules really fired from its state history, and --check-series from series: give one")
	case len(f.rules) == 0 && f.rulesHistory == "":
		return errors.New("--check-grafana is for Grafana's rules read from files, with --rules or --rules-history")
	}
	return nil
}

// validLoki checks the flags of a Loki: its rules, read with it.
func validLoki(f *verdictFlags, serve bool) error {
	switch {
	case f.loki == "" && (f.lokiTk != "" || f.lokiTenant != ""):
		return errors.New("--loki-token-file and --loki-tenant go with --loki")
	case f.lokiLoaded && f.loki == "":
		return errors.New("--loki-loaded-rules reads the rules from the Loki: add --loki URL")
	case len(f.lokiRules) > 0 && f.loki == "":
		return errors.New("--loki-rules are replayed against a Loki: add --loki URL")
	case len(f.lokiRules) > 0 && f.lokiLoaded:
		return errors.New("--loki-rules and --loki-loaded-rules are two ways to the same rules: give one")
	case serve && len(f.lokiRules) > 1:
		return errors.New("lapilli serve doesn't take --loki-rules more than once: give a directory")
	}
	return nil
}

// validContent checks the flags of lapilli verdict --check-content.
func validContent(f *verdictFlags, sources int) error {
	lokiToo := len(f.lokiRules) > 0 || f.lokiLoaded
	switch {
	case !f.checkContentSet || sources > 1 || sources == 0 && len(f.alarms) == 0 && !lokiToo || f.alerts || f.firings != "":
		return errors.New("--check-content checks what the rules say: give them with one of --rules, --loaded-rules, --grafana, --ruler, --alarms or --loki-rules")
	case f.checkReplay || len(f.checkSeries) > 0 || f.checkGrafana != "":
		return errors.New("--check-content doesn't go with --check-replay")
	case len(f.compare) > 0 || f.changedOnly:
		return errors.New("--check-content checks the rules given: leave out --compare")
	case f.markdown:
		return errors.New("--markdown writes --compare's report, for a pull request: it doesn't go with --check-content")
	case f.csv:
		return errors.New("--csv writes a verdict's firings: it doesn't go with --check-content")
	case f.redact:
		return errors.New("--redact gives a verdict's names aliases: it doesn't go with --check-content")
	case f.metrics || f.push != "" || f.slackFile != "":
		return errors.New("--check-content is for you to read, or --json: it doesn't go with --metrics, --push or --slack-webhook-file")
	case f.grafanaTk != "" && f.grafana == "":
		return errors.New("--grafana-token-file goes with --grafana")
	case validRuler(f) != nil:
		return validRuler(f)
	case f.loadedRules && f.promURL == "" && f.gcpProj == "":
		return errors.New("--loaded-rules reads the rules from the API: add --prometheus URL or --gcp-project ID")
	case f.anyImpact() && f.promURL == "" && f.gcpProj == "":
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
	return source.Prometheus{URL: f.promURL, GCPProject: f.gcpProj, Auth: f.auth, AWSRegion: f.awsRegion, TokenFile: f.tokenFile, Tenant: f.rulerTenant,
		MaxRate: f.maxRate, MaxSamples: f.maxSamples}
}

var now = time.Now

func runVerdict(args []string, stdout, stderr io.Writer) int {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Fprint(stdout, verdictUsage)
			return 0
		}
	}
	// What an error says can hold what a rule file or a flag does, a line
	// break among it: shown as \u{...}, never a line of its own, which a
	// CI system could read as a command.
	f, err := parseVerdictFlags(args)
	if err != nil {
		fmt.Fprintf(stderr, "lapilli verdict: %s\nRun \"lapilli verdict -h\" for the options.\n", verdict.Clean(err.Error()))
		return 2
	}
	stderr = &lockedWriter{w: stderr} // the progress line writes to it too
	f.warn = stderr
	// The report is redacted; what is said on the way, here, names what
	// it reads, for whoever runs it.
	if f.redact {
		fmt.Fprintln(stderr, "lapilli verdict: --redact: the report is redacted; what is said here, on stderr, isn't")
	}
	// Times in a redacted report are in UTC: a zone says where it was made.
	shownLoc := f.loc
	if f.redact {
		shownLoc = time.UTC
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "lapilli verdict: %s\n", verdict.Clean(err.Error()))
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	ctx, finish := f.watch(ctx, stderr)
	defer finish()

	f.opts.From, f.opts.To = f.from, f.to
	if f.from.IsZero() {
		f.opts.To = now().Truncate(f.step)
		f.opts.From = f.opts.To.Add(-f.since)
	}

	versions, err := loadVersions(f)
	if err != nil {
		return fail(err)
	}
	// What the rules are expected to do, read before anything is replayed:
	// a file that can't be read fails the run at once.
	var expAfter, expBefore *verdict.Expectations
	for _, e := range []struct {
		flag, path string
		into       **verdict.Expectations
	}{{"--expect", f.expect, &expAfter}, {"--expect-before", f.expectBefore, &expBefore}} {
		if e.path == "" {
			continue
		}
		file, err := os.Open(e.path)
		if err != nil {
			return fail(fmt.Errorf("%s: %w", e.flag, err))
		}
		*e.into, err = verdict.ReadExpectations(file)
		file.Close()
		if err != nil {
			return fail(fmt.Errorf("%s %s: %w", e.flag, e.path, err))
		}
	}
	if f.promtool != "" && f.promtoolMemory > 0 && !scenario.CanCap {
		// Said once, as serve logs it.
		fmt.Fprintf(stderr, "lapilli verdict: promtool runs without a memory cap here (%s): --promtool-memory is set through RLIMIT_DATA, on Linux alone\n", runtime.GOOS)
	}
	var recordedIs []rules.Recording
	if len(f.compare) > 0 {
		f.sayPicked(stderr, "--rules")
		recordedIs, f.recorded = f.recorded, nil
	} else {
		f.sayPicked(stderr, "")
	}
	// On-call schedules, read once: their turns, each person's hours.
	if f.onCall != nil {
		for _, sc := range f.onCall.Schedules {
			feed, err := f.onCall.Fetch(ctx, sc, nil, "lapilli/"+version, now())
			if err != nil {
				return fail(fmt.Errorf("--on-call: schedule %s: %w", sc.Name, err))
			}
			for _, n := range feed.Notes {
				fmt.Fprintf(stderr, "lapilli verdict: --on-call: schedule %s: passed over %s\n", verdict.Clean(sc.Name), verdict.Clean(n))
			}
			verdict.OnCallNamed(sc.Name).Set(verdict.NewRota(feed.Turns, f.onCall.HoursOf()))
		}
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
		var noneSelected error
		for _, p := range f.compare {
			rs, err := loadRules(f, p)
			// None there, or none the selector keeps: a side with no
			// rules, as lapilli serve's backtests take it.
			if err != nil && !errors.Is(err, rules.ErrNoneInDir) && !errors.Is(err, rules.ErrNoneSelected) {
				return fail(err)
			}
			if errors.Is(err, rules.ErrNoneSelected) && noneSelected == nil {
				noneSelected = err
			}
			f.replaying().Apply(rs)
			was = append(was, rs...)
		}
		// No rule left on either side, and the selector kept nothing of a
		// side it was given documents of: a selector that matches nothing
		// (a label's name mistyped), said as ever, not taken for two sets
		// alike, the other side empty or not (a pull request's first
		// rules).
		held := len(was)
		for _, v := range versions {
			held += len(v.Rules)
		}
		if held == 0 && (noneSelected != nil || f.noneSelected != nil) {
			return fail(fmt.Errorf("--rules-select %s keeps no document of rules of --rules or of --compare: %w", f.rulesSelect, cmp.Or(noneSelected, f.noneSelected)))
		}
		f.sayPicked(stderr, "--compare")
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
		recs := recordingChanges(f.recorded, recordedIs)
		if len(before)+len(after) == 0 && len(recs) == 0 {
			c := &verdict.Comparison{From: f.opts.From, To: f.opts.To, Same: len(same), ChangedOnly: f.changedOnly,
				Rules: []verdict.RuleChange{}, Incidents: []verdict.IncidentChange{}, PassedOver: f.passedOver}
			c.Summarize(f.incidents != "" || f.anyImpact())
			// Nothing to replay, but a scenario's test file the change
			// edited: its scenarios run before and after, the rules alike,
			// and held as any are; else the expectations are left out, as
			// they were.
			if expAfter != nil && f.promtool != "" {
				sides, err := f.scenarioSides(ctx, was, is, recordedIs)
				if err != nil {
					return fail(err)
				}
				defer sides.Close()
				c.Scenarios, c.Alike = sides, verdict.AlikeRules(same)
				if err := c.Expect(expAfter, expBefore, f.opts.Hours); err != nil {
					return fail(fmt.Errorf("--expect %s: %w", f.expect, err))
				}
				if len(c.Expectations.Results) == 0 {
					c.Expectations = nil
					c.Summarize(f.incidents != "" || f.anyImpact())
				}
			}
			checked := c.Expectations
			if f.redact {
				c = verdict.NewRedactor(f.redactKey).Comparison(c)
			}
			switch {
			case f.json:
				err = verdict.JSON(stdout, c)
			case f.markdown:
				err = verdict.CompareMarkdown(stdout, c, shownLoc)
			default:
				if _, err = fmt.Fprintf(stdout, "Nothing that decides when the %d rules fire has changed: nothing to replay.\n", len(same)); err == nil && c.Expectations != nil {
					err = verdict.ExpectationsText(stdout, c.Expectations)
				}
			}
			if err != nil {
				return fail(err)
			}
			if n := checked.Broken(); f.failExpect && n > 0 {
				fmt.Fprintf(stderr, "lapilli verdict: the change breaks %d expectations the rules in place meet (--fail-expectations)\n", n)
				return 1
			}
			return 0
		}
	}

	var h history
	if len(f.rules) > 0 || readsAPI(f) || f.alerts || f.anyImpact() {
		if f.promURL != "" || f.gcpProj != "" {
			if h, err = newHistory(ctx, f); err != nil {
				return fail(err)
			}
			f.askScrapes(ctx, h, stderr, versions, was, same, before, after)
		}
		h = f.withLogs(h, stderr)
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

	// Impact first: a query, and one a selector for where its data
	// begins, and a replay that runs out of time should still be judged
	// against it.
	if f.anyImpact() {
		var err error
		if incs, err = f.readImpact(ctx, h, incs); err != nil {
			return fail(err)
		}
	}

	var known []rules.Rule
	var fs []firing.Firing
	var cmp *verdict.Comparison
	switch {
	case len(f.compare) > 0:
		// Rules alike in both are replayed once, for both; or not at all.
		alike := same
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
		// Each side's rule entries are found by its own rules' labels.
		ob, oa := o, o
		ob.Signals = indexRules(o.Signals, slices.Concat(same, before), slices.Concat(fired[0], fired[1]))
		oa.Signals = indexRules(o.Signals, slices.Concat(same, after), slices.Concat(fired[0], fired[2]))
		cmp = verdict.Compare(
			verdict.Judge(slices.Concat(ran[0], ran[1]), slices.Concat(fired[0], fired[1]), incs, ob),
			verdict.Judge(slices.Concat(ran[0], ran[2]), slices.Concat(fired[0], fired[2]), incs, oa),
			changed, len(was)-len(before))
		cmp.ChangedOnly = f.changedOnly
		cmp.LabelRules(before, after)
		cmp.Recording = recordingChanges(f.recorded, recordedIs)
		cmp.PassedOver = f.passedOver
		cmp.Summarize(f.incidents != "" || f.anyImpact())
		if expAfter != nil {
			if f.promtool != "" {
				var is []rules.Rule
				if len(versions) > 0 {
					is = versions[0].Rules
				}
				sides, err := f.scenarioSides(ctx, was, is, recordedIs)
				if err != nil {
					return fail(err)
				}
				defer sides.Close()
				// The rules alike too, held to the scenarios whose test
				// files the change edited.
				cmp.Scenarios, cmp.Alike = sides, verdict.AlikeRules(alike)
			}
			if err := cmp.Expect(expAfter, expBefore, f.opts.Hours); err != nil {
				return fail(fmt.Errorf("--expect %s: %w", f.expect, err))
			}
		}
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
	// The rules are replayed: what asks the history from here on (near
	// misses, suggestions) replays no more of them.
	ctx = replay.Tracking(ctx, nil)
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
		// Each rule's entry, found once, by the rules' labels or its
		// firings'.
		f.opts.Signals = indexSignals(f.opts.Signals, versions, fs)
		// Replayed as several versions, a rule is known as the last: when
		// its firings' conditions held is told by every one of them.
		if len(versions) > 1 {
			for _, v := range versions {
				f.opts.Versions = append(f.opts.Versions, v.Rules...)
			}
		}
		rep = verdict.Judge(known, fs, incs, f.opts)
		for _, v := range versions {
			if v.From.After(f.opts.From) && v.From.Before(f.opts.To) {
				rep.RulesChanged = append(rep.RulesChanged, v.From)
			}
		}
		// Impact shorter than a rule can fire on reads as missed by it: said,
		// not judged otherwise.
		if f.anyImpact() && len(versions) > 0 {
			replayed := map[string]bool{}
			for _, rr := range rep.Rules {
				replayed[rr.Rule] = rr.Replayed && rr.Unsupported == ""
			}
			for r, ok := range replayed {
				replayed[r] = ok && f.judgedAgainst(r)
			}
			rep.ImpactShort = impactShort(versions[len(versions)-1].Rules, replayed, f.shortestFor(), f.step)
			if s, ok := f.shortestSignal(); ok && rep.ImpactShort != nil {
				rep.ImpactShort.Signal = s.Name
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
		// longer "for"; the metrics and the CSV alone don't show
		// suggestions, near misses or what unrecorded firings were. They
		// take time of their own, as long again as the replays at times:
		// --extras bounds it, or skips them.
		brief := f.metrics || f.csv
		ectx, cancelExtras := ctx, context.CancelFunc(func() {})
		if f.extrasSet && f.extras > 0 {
			ectx, cancelExtras = context.WithTimeout(ctx, f.extras)
		}
		if f.extrasSet && f.extras == 0 {
			if !brief {
				rep.Extras = "skipped"
			}
			brief = true
		}
		// Cut is what a step says it didn't finish, as time ran out: not
		// the time read after it, which may run out after the last one.
		cut := false
		if len(versions) == 1 && !brief {
			var done bool
			rep.Suggestions, done = source.Suggest(ectx, h, versions[0].Rules, rep, incs, f.opts, f.step, f.parallel, nil)
			cut = cut || !done && ectx.Err() != nil
		}
		// Each incident nothing caught, against the rules as they stood then.
		if !brief {
			var done bool
			var err error
			rep.NearMisses, done, err = source.NearMisses(ectx, h, versions, rep, incs, f.opts, f.step, f.parallel, nil)
			rep.NearMissesCut = !done
			cut = cut || !done && ectx.Err() != nil
			if err != nil && ectx.Err() == nil {
				fmt.Fprintf(stderr, "lapilli verdict: looking for near misses: %s\n", verdict.Clean(err.Error()))
			}
		}
		// With no record, what the firings of the rules that fired most were:
		// how many went well past their bound, and from which alerts.
		if !brief && rep.Unjudged > 0 && len(versions) > 0 {
			var err error
			rep.Unrecorded, err = source.Unrecorded(ectx, h, versions, rep, source.Judged(rep.Firings), f.step)
			cut = cut || err != nil && ectx.Err() != nil
			if err != nil && ectx.Err() == nil {
				fmt.Fprintf(stderr, "lapilli verdict: looking at the firings with no record to judge them by: %s\n", verdict.Clean(err.Error()))
			}
		}
		if cut {
			rep.Extras = "cut"
			if ctx.Err() == nil {
				fmt.Fprintf(stderr, "lapilli verdict: the steps after the replays stopped at --extras %s: a longer \"for\", near misses and what fired with no record are in part\n", f.extras)
			}
		}
		cancelExtras()
		reports = append(reports, rep)
	}
	if cmp != nil {
		cmp.Unreplayed = cmp.UnreplayedRules()
	} else {
		rep.Unreplayed = rep.UnreplayedRules()
	}
	if f.markdown {
		f.forComment(cmp, stderr)
	}
	// What is shown and sent: with --redact, a copy with every name given
	// its alias; the reports as they are decide the exit status below.
	shown, shownCmp := rep, cmp
	if f.redact {
		x := verdict.NewRedactor(f.redactKey)
		if cmp != nil {
			shownCmp = x.Comparison(cmp)
		} else {
			shown = x.Report(rep)
		}
	}
	text := func(w io.Writer) error {
		if shownCmp != nil {
			if err := verdict.CompareText(w, shownCmp, shownLoc); err != nil {
				return err
			}
			return verdict.ExpectationsText(w, shownCmp.Expectations)
		}
		return verdict.Text(w, shown, shownLoc)
	}
	switch {
	case f.json && cmp != nil:
		err = verdict.JSON(stdout, shownCmp)
	case f.markdown:
		err = verdict.CompareMarkdown(stdout, shownCmp, shownLoc)
	case f.json:
		err = verdict.JSON(stdout, shown)
	case f.csv:
		err = verdict.CSV(stdout, shown, nil, nil)
	case f.metrics:
		err = verdict.Prometheus(stdout, shown, now())
	default:
		err = text(stdout)
	}
	if err != nil {
		return fail(err)
	}
	// A draft of the expectations, from the period judged: of the rules
	// as the report has them, counted as --compare holds budgets.
	if f.draft != "" && rep != nil {
		d := verdict.Draft(verdict.DraftFromReport(rep, func(string) *verdict.Hours { return f.opts.Hours }))
		b, err := d.YAML(verdict.DraftAbout{By: "lapilli verdict", From: rep.From, To: rep.To, Weeks: verdict.DraftPeriodWeeks, Loc: f.loc})
		if err == nil {
			err = os.WriteFile(f.draft, b, 0o644)
		}
		if err != nil {
			return fail(fmt.Errorf("--draft-expectations %s: %w", f.draft, err))
		}
		fmt.Fprintf(stderr, "lapilli verdict: --draft-expectations: %d rules drafted in %s, %d left out\n", len(d.Expectations), f.draft, len(d.Left))
	}
	// Each place the report goes gets its own minute: the replay may have
	// used up --timeout, and one slow place shouldn't keep it from another.
	failed := false
	sendTo := func(send func(context.Context) error) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := send(ctx); err != nil {
			fmt.Fprintf(stderr, "lapilli verdict: %s\n", verdict.Clean(err.Error()))
			failed = true
		}
	}
	if f.push != "" {
		sendTo(func(ctx context.Context) error { return report.Push(ctx, f.push, shown, now()) })
	}
	if f.slackFile != "" {
		var b strings.Builder
		if err := text(&b); err != nil {
			return fail(err)
		}
		sendTo(func(ctx context.Context) error { return report.Slack(ctx, f.slackFile, b.String()) })
	}
	// A partial verdict is still worth printing, but not worth a 0.
	all, unfinished, dropped := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, rep := range reports {
		for _, r := range rep.Rules {
			all[r.Rule] = true
			if r.Unsupported == replay.Unfinished {
				unfinished[r.Rule] = true
			}
			if rules.Unreplayable(r.Unsupported) {
				dropped[r.Rule] = true
			}
		}
	}
	if len(unfinished) > 0 {
		fmt.Fprintf(stderr, "lapilli verdict: %d of %d rules didn't finish within --timeout %s; raise it, or shorten the period\n",
			len(unfinished), len(all), f.timeout)
		return 1
	}
	if f.failUnreplayed && len(dropped) > 0 {
		fmt.Fprintf(stderr, "lapilli verdict: %d of %d rules couldn't be replayed, as the history couldn't be read or their labels on their series were too many (--fail-unreplayed)\n", len(dropped), len(all))
		return 1
	}
	if cmp != nil && cmp.Expectations != nil {
		if n := cmp.Expectations.Broken(); f.failExpect && n > 0 {
			fmt.Fprintf(stderr, "lapilli verdict: the change breaks %d expectations the rules in place meet (--fail-expectations)\n", n)
			return 1
		}
		if n := len(cmp.Expectations.Unexpected); f.expectRequired && n > 0 {
			fmt.Fprintf(stderr, "lapilli verdict: the change adds %d rules no expectation holds (--expect-required)\n", n)
			return 1
		}
	}
	if failed {
		return 1
	}
	return 0
}

// scenarioSides runs the scenarios of --expect and --expect-before with
// --promtool (docs/design/expectations.md, step 4): each side's rules,
// as replayed, and its recording rules, by group; each test file read in
// the directory of the expectations file that names it, and nowhere else.
func (f *verdictFlags) scenarioSides(ctx context.Context, was, is []rules.Rule, recordedIs []rules.Recording) (*scenario.Sides, error) {
	s := &scenario.Sides{Ctx: ctx, Promtool: scenario.Promtool{Path: f.promtool, Memory: f.promtoolMemory},
		Rules: [2][]rules.Rule{was, is}, Recorded: [2][]rules.Recording{f.recorded, recordedIs}}
	var err error
	if s.After, err = os.OpenRoot(filepath.Dir(f.expect)); err != nil {
		return nil, fmt.Errorf("--expect %s: %w", f.expect, err)
	}
	if f.expectBefore != "" {
		if s.Before, err = os.OpenRoot(filepath.Dir(f.expectBefore)); err != nil {
			s.Close()
			return nil, fmt.Errorf("--expect-before %s: %w", f.expectBefore, err)
		}
	}
	return s, nil
}

// forComment readies the reasons rules couldn't be replayed for
// --markdown, a comment on a pull request, which more people read than the
// job's log. A query that failed as the history couldn't be read
// (rules.HistoryUnread: not reached, not let in, turned away, answered by
// a proxy's page), which is all but the history refusing the query as
// written (prom.Refused, the same line --fail-unreplayed draws), says only
// that, its whole reason going to the job's log. The rest has where the
// history is taken out of it: the URLs given, the user names in them and
// their hosts, each as a whole word, any IP address with its port, the
// tenants, and a Google Cloud project.
func (f *verdictFlags) forComment(c *verdict.Comparison, stderr io.Writer) {
	verdict.ForComment(c, UnreadHistory, f.scrubber(), func(rule, side, why string) {
		fmt.Fprintf(stderr, "lapilli verdict: %s (%s) couldn't be replayed: %s\n", verdict.Clean(rule), side, verdict.Clean(why))
	})
}

// scrubber takes where the history is out of a text: the URLs given, the
// user names in them and their hosts, each as a whole word, any IP
// address with its port, an IPv6 one written without brackets among them,
// the tenants, and a Google Cloud project, a whole word or the first label
// of a name Google gives under it (googleNames); more, those of more flags
// (lapilli preflight's).
func (f *verdictFlags) scrubber(more ...[2]string) func(string) string {
	type hid struct {
		val, as string
		google  bool // the first label of a name Google gives under a project too
	}
	var hide []hid
	given := [][2]string{{"--prometheus", f.promURL}, {"--loki", f.loki}, {"--grafana", f.grafana},
		{"--ruler", f.ruler}, {"--gcp-project", f.gcpProj}, {"--ruler-tenant", f.rulerTenant}, {"--loki-tenant", f.lokiTenant}}
	for _, w := range append(given, more...) {
		flag, val := w[0], w[1]
		if val == "" {
			continue
		}
		hide = append(hide, hid{val, "[" + flag + "]", flag == "--gcp-project"})
		if u, err := url.Parse(val); err == nil && u.Host != "" {
			hide = append(hide, hid{u.Host, "[" + flag + "]", false}, hid{u.Hostname(), "[" + flag + "]", false})
			if u.User != nil && u.User.Username() != "" {
				hide = append(hide, hid{u.User.Username(), "[user]", false})
			}
		}
	}
	slices.SortStableFunc(hide, func(a, b hid) int { return len(b.val) - len(a.val) })
	return func(s string) string {
		s = userinfo.ReplaceAllString(s, "$1")
		for _, h := range hide {
			s = replaceName(s, h.val, h.as, h.google)
		}
		for _, h := range hide {
			s = replaceCut(s, h.val, h.as)
		}
		return address.ReplaceAllString(addresses6(s, "[address]"), "[address]")
	}
}

// addresses6 replaces in s each IPv6 address written without brackets, as
// x509 ("valid for fd12:3456:789a::17") and Postgres (host "2001:db8::5")
// write one, with with: a run of hex digits and colons, a dotted IPv4 at
// its end or a zone or not, that no letter, digit or underscore touches,
// that reads as an IPv6 address (net/netip) once a colon or a dot that
// ends it, as a sentence's, is left out. A time (12:30:45) or a MAC
// address doesn't read as one, nor does "::" alone, which holds no digit;
// one in brackets is address's, with its port.
func addresses6(s, with string) string {
	word := func(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }
	reads := func(x string) bool {
		a, err := netip.ParseAddr(x)
		return err == nil && a.Is6() && strings.ContainsAny(x, "0123456789abcdefABCDEF")
	}
	var b strings.Builder
	last := 0
	for _, m := range address6.FindAllStringIndex(s, -1) {
		i, j := m[0], m[1]
		after, _ := utf8.DecodeRuneInString(s[j:])
		if j < len(s) && word(after) {
			continue
		}
		// A colon that only joins it to what is before (host:fd12::17).
		if strings.HasPrefix(s[i:j], ":") && !strings.HasPrefix(s[i:j], "::") {
			i++
		}
		before, _ := utf8.DecodeLastRuneInString(s[:i])
		if i > 0 && (word(before) || before == '[') {
			continue
		}
		for j > i && !reads(s[i:j]) && strings.ContainsAny(s[j-1:j], ".:") {
			j--
		}
		if !reads(s[i:j]) {
			continue
		}
		b.WriteString(s[last:i] + with)
		last = j
	}
	b.WriteString(s[last:])
	return b.String()
}

// replaceCut replaces in s what is left of old where a text was cut in it,
// before an ellipsis, as a server's answer is cut before it is said: the
// start of old, three characters at least, as the start of a word.
func replaceCut(s, old, with string) string {
	const mark = "…"
	var b strings.Builder
	for {
		i := strings.Index(s, mark)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		head := s[:i]
		for k := min(len(old)-1, len(head)); k >= 3; k-- {
			if !strings.HasSuffix(head, old[:k]) {
				continue
			}
			before, _ := utf8.DecodeLastRuneInString(head[:len(head)-k])
			if len(head) == k || !(before == '.' || before == '-' || before == '_' || unicode.IsLetter(before) || unicode.IsDigit(before)) {
				head = head[:len(head)-k] + with
				break
			}
		}
		b.WriteString(head + mark)
		s = s[i+len(mark):]
	}
}

// UnreadHistory is, in --markdown, why a rule whose query the history
// didn't answer couldn't be replayed: rules.HistoryUnread, without why.
const UnreadHistory = verdict.UnreadHistory

var (
	// userinfo is a URL's user and password.
	userinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/@\s"']*@`)
	// address is an IP address, with its port if it has one; in brackets,
	// with its zone if it has one, as Go writes one ([fe80::1%en0]:5432)
	// and a URL ([fe80::1%25en0]).
	address = regexp.MustCompile(`\[[0-9A-Fa-f:.]*:[0-9A-Fa-f:.]*(%[^\]\s]*)?\](:[0-9]+)?|\b([0-9]{1,3}\.){3}[0-9]{1,3}(:[0-9]+)?\b`)
	// address6 is what may be an IPv6 address without brackets: addresses6
	// says which is one.
	address6 = regexp.MustCompile(`[0-9A-Fa-f:.]*:[0-9A-Fa-f:.]*(%[0-9A-Za-z_.-]*[0-9A-Za-z_-])?`)
)

// googleNames are what follows a Google Cloud project in the names Google
// gives under it, the project their first label: a service account's
// email (writer@PROJECT.iam.gserviceaccount.com), a workload identity
// pool (PROJECT.svc.id.goog), and an App Engine or Firebase app's host
// (PROJECT.appspot.com, PROJECT.firebaseapp.com, PROJECT.web.app).
var googleNames = []string{".iam.gserviceaccount.com", ".svc.id.goog", ".appspot.com", ".firebaseapp.com", ".web.app"}

// replaceWord replaces in s each old that is a whole word, with with: no
// letter, digit, dot, dash or underscore before it, and none but a dot
// after it, a full stop, which no letter, digit, dash or underscore
// follows. A host "prom" isn't taken out of "prometheus" or "prom.example",
// but is out of "failed at prom.": as recipes/backtest/lib.sh takes it out.
func replaceWord(s, old, with string) string { return replaceName(s, old, with, false) }

// replaceName is replaceWord, and, with google, old as the first label of
// a name Google gives under a Cloud project as well (googleNames). As the
// first label of any other domain it is left: a project named monitoring
// isn't taken out of monitoring.googleapis.com.
func replaceName(s, old, with string, google bool) string {
	if old == "" {
		return s
	}
	alnum := func(r rune) bool { return r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }
	var b strings.Builder
	for {
		i := strings.Index(s, old)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		before, _ := utf8.DecodeLastRuneInString(s[:i])
		rest := s[i+len(old):]
		after, n := utf8.DecodeRuneInString(rest)
		next, _ := utf8.DecodeRuneInString(rest[n:])
		under := google && slices.ContainsFunc(googleNames, func(name string) bool {
			return len(rest) >= len(name) && strings.EqualFold(rest[:len(name)], name)
		})
		whole := (i == 0 || !(before == '.' || alnum(before))) && (rest == "" || under || !(alnum(after) || after == '.' && alnum(next)))
		b.WriteString(s[:i])
		if whole {
			b.WriteString(with)
		} else {
			b.WriteString(old)
		}
		s = s[i+len(old):]
	}
}

// replaying is how the rules are replayed: --late and --scrape-interval,
// the scrape intervals and types Grafana's data sources say they have, and
// where a Loki's ruler runs its groups.
func (f *verdictFlags) replaying() rules.Replaying {
	p := rules.Replaying{Late: f.late, Scrape: f.scrapeInterval, DataSources: f.dataSources, Kinds: f.kinds, Places: f.places,
		NoLoki: f.loki == "", NoGrafanaLoki: f.loki == "" && f.grafana == ""}
	if f.scrapes != nil {
		p.ScrapeOf = f.scrapes.Shortest
	}
	return p
}

// newLoki is --loki's history; replaced in tests.
var newLoki = func(f *verdictFlags) history {
	return f.lokiSource().History("lapilli/" + buildVersion())
}

// newGrafanaLoki is a Grafana Loki data source's history, through
// --grafana's data source proxy, asked by limit, its own; replaced in
// tests.
var newGrafanaLoki = func(f *verdictFlags, uid string, limit *prom.Limiter) history {
	return source.GrafanaLoki(f.grafana, f.grafanaTk, "lapilli/"+buildVersion(), uid, limit, prom.WithMaxSamples(f.maxSamples))
}

// lokiSource is --loki, its every client sharing one Limiter: its
// queries', and its ruler's, read once by lapilli verdict and each minute
// by lapilli serve.
func (f *verdictFlags) lokiSource() source.Loki {
	if f.lokiLimit == nil {
		f.lokiLimit = prom.NewLimiter(f.maxRate)
	}
	return source.Loki{URL: f.loki, TokenFile: f.lokiTk, Tenant: f.lokiTenant, MaxRate: f.maxRate, MaxSamples: f.maxSamples, Limit: f.lokiLimit}
}

// withLogs is h with the Lokis rules written in LogQL are replayed
// against beside it (docs/design/loki.md). A token sent to --loki over
// plain http is said to warn, as --grafana's is.
func (f *verdictFlags) withLogs(h history, warn io.Writer) history {
	if h == nil {
		h = noPrometheus{}
	}
	return replay.WithLogs(h, f.logs(warn).For)
}

// logs are the Lokis rules written in LogQL are replayed against.
func (f *verdictFlags) logs(warn io.Writer) *source.Logs {
	l := &source.Logs{}
	if f.loki != "" {
		l.Loki = newLoki(f)
		if f.lokiTk != "" && strings.HasPrefix(strings.ToLower(f.loki), "http://") && warn != nil {
			fmt.Fprintln(warn, "lapilli: --loki is plain http: the token goes unencrypted")
		}
	}
	if f.grafana != "" {
		// A Limiter for each data source, as each pushes back of its own
		// through Grafana's proxy, which passes on what it answers; one
		// Pace for all, as --max-rate is of the Grafana; and --parallel
		// at once for all, as one Grafana answers them all, and holds
		// each answer while it does. Logs.For makes one history a data
		// source.
		pace, most := f.grafanaLimits()
		l.Grafana = func(uid string) replay.History {
			return newGrafanaLoki(f, uid, prom.NewLimiterPaced(pace).Within(most))
		}
	}
	return l
}

// grafanaLimits are the Grafana's Pace, of --max-rate, and its cap, of
// --parallel, which everything it is asked shares: its data sources' and
// its state history's.
func (f *verdictFlags) grafanaLimits() (*prom.Pace, *prom.Limiter) {
	if f.grafanaCap == nil {
		f.grafanaPace, f.grafanaCap = prom.NewPace(f.maxRate), prom.NewCap(f.parallel)
	}
	return f.grafanaPace, f.grafanaCap
}

// historyGrafana is the Grafana whose state history says what Grafana's
// rules really fired: --check-grafana's, or --grafana's; "" for none, or
// with --check-series, which names series instead.
func (f *verdictFlags) historyGrafana() string {
	if len(f.checkSeries) > 0 {
		return ""
	}
	return cmp.Or(f.checkGrafana, f.grafana)
}

// grafanaHistory reads the state history of historyGrafana, asked by a
// Limiter of its own, within the Grafana's pace and cap.
func (f *verdictFlags) grafanaHistory() source.GrafanaHistory {
	pace, most := f.grafanaLimits()
	return source.GrafanaHistory{URL: f.historyGrafana(), TokenFile: f.grafanaTk, Agent: "lapilli/" + buildVersion(),
		Limit: prom.NewLimiterPaced(pace).Within(most)}
}

// noPrometheus is the history when there is no --prometheus: only rules
// written in LogQL are replayed, against a Loki.
type noPrometheus struct{}

func (noPrometheus) QueryRange(context.Context, string, time.Time, time.Time, time.Duration) ([]prom.Series, error) {
	return nil, errors.New("there is no --prometheus to ask")
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
		a.Warn = warnLine(stderr)
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
	// A Loki's rules, beside the rest.
	switch {
	case f.lokiLoaded:
		more, _, places, err := source.LokiLoaded(ctx, f.lokiSource(), "lapilli/"+buildVersion(), warnLine(stderr))
		if err != nil {
			return nil, err
		}
		f.places = places
		kept = appendMatching(kept, more, f.ruleRE)
	case len(f.lokiRules) > 0:
		for _, p := range f.lokiRules {
			more, _, notes, err := source.LokiFiles(p)
			if err != nil {
				return nil, err
			}
			for _, n := range notes {
				warnLine(stderr)(n)
			}
			kept = appendMatching(kept, more, f.ruleRE)
		}
	}
	if len(kept) == 0 && len(f.rules) == 0 {
		return nil, errors.New("no alert rules to replay")
	}
	if f.grafana != "" {
		f.dataSources, f.kinds = grafanaDataSources(ctx, f, kept, stderr)
	}
	f.replaying().Apply(kept)
	return kept, nil
}

// appendMatching adds to rs the rules of more whose names match re.
func appendMatching(rs, more []rules.Rule, re *regexp.Regexp) []rules.Rule {
	for _, r := range more {
		if re == nil || re.MatchString(r.Name) {
			rs = append(rs, r)
		}
	}
	return rs
}

// grafanaDataSources asks --grafana for each data source the rules'
// queries name: its own scrape interval, which $__rate_interval is put in
// with, and its type, which says whether its queries are PromQL or LogQL.
// What can't be read is said: its $__rate_interval is put in as
// --scrape-interval says, or as Grafana's 15s, and its queries are taken
// as PromQL, unless they say otherwise.
func grafanaDataSources(ctx context.Context, f *verdictFlags, rs []rules.Rule, stderr io.Writer) (rules.DataSources, map[string]string) {
	var uids []string
	for _, r := range rs {
		for _, u := range r.GrafanaUIDs() {
			if !slices.Contains(uids, u) {
				uids = append(uids, u)
			}
		}
	}
	if len(uids) == 0 {
		return nil, nil
	}
	slices.Sort(uids)
	ds, kinds, missed, err := source.GrafanaDataSources(ctx, f.grafana, f.grafanaTk, "lapilli/"+buildVersion(), uids)
	if err != nil {
		missed = []string{err.Error()}
	}
	if len(missed) > 0 {
		with := "15s, Grafana's own"
		if f.scrapeInterval > 0 {
			with = f.scrapeInterval.String() + ", --scrape-interval's"
		}
		fmt.Fprintf(stderr, "lapilli verdict: Grafana's data sources %s weren't read: their $__rate_interval is put in with a scrape interval of %s, and their queries are taken as PromQL\n",
			verdict.Clean(strings.Join(missed, ", ")), with)
	}
	return ds, kinds
}

// readsAPI reports whether rules are read from an API, or beside the
// files: --loaded-rules, --grafana, --ruler, --alarms, or a Loki's.
func readsAPI(f *verdictFlags) bool {
	return f.loadedRules || f.grafana != "" || f.ruler != "" || len(f.alarms) > 0 || len(f.lokiRules) > 0 || f.lokiLoaded
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

// warnLine is a reader's Warn, and what it notes: each on a line of its
// own, shown as it reads (verdict.Clean), as it names what a cloud, a
// ruler or a Loki answered, an alarm's ID or a group's name, which may
// hold an escape sequence or a line break.
func warnLine(w io.Writer) func(string) {
	return func(what string) { fmt.Fprintf(w, "lapilli verdict: %s\n", verdict.Clean(what)) }
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
	r.Warn = warnLine(stderr)
	r.Tenant, r.Reads.TokenFile = f.rulerTenant, f.rulerTk
	if r.Kind != source.RulerConfig {
		if r.Reads.Sign, err = cloud.Request(ctx, f.auth, f.ruler, ""); err != nil {
			return nil, fmt.Errorf("--ruler: %w", err)
		}
	}
	return r, nil
}

// sayPicked says what reading the rule files passed over, if anything, and
// forgets it. side is the flag that named them, when there are two sets
// of rules to tell apart: --rules or --compare.
func (f *verdictFlags) sayPicked(stderr io.Writer, side string) {
	if said := f.picked.String(); said != "" {
		files := "the rule files read"
		if side != "" {
			files = "the rule files " + side + " read"
			if f.passedOver == nil {
				f.passedOver = map[string]string{}
			}
			f.passedOver[strings.TrimPrefix(side, "--")] = said
		}
		fmt.Fprintf(stderr, "lapilli verdict: %s held %s: passed over\n", files, said)
	}
	f.picked = rules.Picked{}
}

// recordingChanges are the recording rules that differ between was and
// is (verdict.RecordingChanges).
func recordingChanges(was, is []rules.Recording) []verdict.RecordingChange {
	return verdict.RecordingChanges(was, is)
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
		rs, err := loadRules(f, rf.path)
		if err != nil && !(len(f.compare) > 0 && (errors.Is(err, rules.ErrNoneInDir) || errors.Is(err, rules.ErrNoneSelected))) {
			return nil, err
		}
		if errors.Is(err, rules.ErrNoneSelected) && f.noneSelected == nil {
			f.noneSelected = err
		}
		f.replaying().Apply(rs)
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

// loadRules reads the rules in a file or directory that --rules-select
// keeps, whose names match --rule, or all of them; what it passed over is
// added to f.picked.
func loadRules(f *verdictFlags, path string) ([]rules.Rule, error) {
	pick := rules.Picking{Select: f.rulesSelect}
	if len(f.compare) > 0 {
		// Set beside each other, what each set records is compared too.
		pick.Recordings = &f.recorded
	}
	rs, _, picked, err := rules.ReadPicked(path, pick)
	f.picked.Add(picked)
	if errors.Is(err, rules.ErrNoneSelected) {
		err = fmt.Errorf("--rules-select %w", err)
	}
	if err != nil {
		return nil, err
	}
	kept := rs[:0]
	for _, r := range rs {
		if f.ruleRE == nil || f.ruleRE.MatchString(r.Name) {
			kept = append(kept, r)
		}
	}
	return kept, nil
}

// impactShort is, of rs, the rules replayed whose "for" and one evaluation
// are longer than impactFor: impact that short, counted as an incident,
// they can't be sure to catch. A rule with no "for" fires on its first
// evaluation in the impact, so it needs as long as its evaluations are apart.
// A rule whose metrics arrive late (--late, its query_offset, or a Grafana
// query's time range ending before now: its Slowest) sees the impact that
// much later, and needs that much longer. Nil when there are none.
func impactShort(rs []rules.Rule, replayed map[string]bool, impactFor, step time.Duration) *verdict.ImpactShort {
	need := map[time.Duration]int{}
	out := verdict.ImpactShort{For: impactFor}
	seen := map[string]bool{}
	for _, r := range rs {
		if !replayed[r.Name] || seen[r.Name] {
			continue
		}
		seen[r.Name] = true
		out.Judged++
		if d := max(r.For, 0) + replay.Every(r, step) + r.Slowest(); d > impactFor {
			out.Rules++
			need[d]++
			if r.Slowest() > 0 {
				out.Lagged++
			}
		}
	}
	if out.Rules == 0 {
		return nil
	}
	for d, n := range need {
		if n > out.Most || (n == out.Most && d < out.Need) {
			out.Need, out.Most = d, n
		}
	}
	return &out
}

// readRedactKey reads --redact-key's file: a random key, written in hex,
// 32 digits or more (16 bytes), as openssl rand -hex 32 writes one. Not a
// passphrase: an alias is 40 bits of a keyed hash, and a key a person
// thought of could be found by trying keys against names. What the file
// holds is never said.
func readRedactKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("--redact-key: %v", err)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(key) < verdict.RedactKeyMin {
		return nil, fmt.Errorf("--redact-key %s: want a random key, %d hex digits or more, as openssl rand -hex 32 writes one", path, 2*verdict.RedactKeyMin)
	}
	return key, nil
}
