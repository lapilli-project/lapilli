# Commands

Each command's options, as `lapilli COMMAND -h` prints them. The pages linked
under each say what the options are for; this page is all of them in one
place, to look one up. These pages follow the main branch: `lapilli version`
says which version you have, and its own `-h` is the one to go by.

## `lapilli`

```text
lapilli judges your alert rules against what actually went wrong.

Each firing gets a stone, the way Roman juries voted: white when it caught an
incident, black when nothing was wrong. Count a rule's stones over a week and
you know whether it earns its page; an incident with no stone is one your
alerts missed.

Usage:
  lapilli verdict --rules FILE --prometheus URL --incidents FILE
                                     replay rules against history
  lapilli verdict --alerts --prometheus URL --incidents FILE
                                     judge what Prometheus fired
  lapilli serve --rules DIR --prometheus URL
                                     keep judging, every hour, into a
                                     ledger with a web UI
  lapilli preflight [serve's flags]  what serve will meet before it is
                                     deployed: reads, writes nothing
  lapilli ledger copy --ledger DIR --to-url-file FILE
                                     copy a ledger into Postgres
  lapilli version

Run "lapilli verdict -h", "lapilli serve -h", "lapilli preflight -h" or
"lapilli ledger -h" for the options.
```

## `lapilli verdict`

What it's for: [Use `lapilli verdict`](verdict.md), [How it judges](how-it-judges.md).

```text
lapilli verdict judges every firing in a period against the incidents in
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
                      incident can list several values. A firing of an
                      alert the incident's tool linked to it (alerts: in
                      the incidents file) catches it whatever they say
  --linked-only       an incident that lists the alerts its tool linked to
                      it is caught by those alone, not by any other firing
                      at the time; one that lists none is caught as ever
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
      alerts:                             # optional: those its tool linked
        - {rule: CheckoutErrors, labels: {service: checkout}}

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
```

## `lapilli serve`

What it's for: [Keep judging](serve.md).

```text
lapilli serve keeps judging: every hour it replays the rules over the hours
since it last did, records the firings, the incidents and every version of
the rules in a ledger, and shows the stones in a web UI, a JSON API and
Prometheus metrics. Incidents can be written down in the UI, and judged
against at once.

Usage:
  lapilli serve [flags]

Where the firings come from (pick one), what went wrong, where the history
is, and how to judge: as for lapilli verdict (lapilli verdict -h), except:
  --rules PATH        one file or directory, read again every --watch: each
                      change is recorded as a new version, from when it was
                      seen, and replayed as such
  --on-call FILE      as for lapilli verdict, each feed read again every so
                      often (every: in the file), its turns kept in the
                      ledger past what the feed holds
  --rules-select SEL  as for lapilli verdict, for --rules and --rules-history:
                      what it and the documents that hold no rules leave
                      out of a file isn't kept in the ledger with it
  --rules-history DIR earlier versions of those rules, one directory each,
                      named by when it took effect (2026-10-14T16:40:31Z or
                      20261014T164031Z), so a first start's backfill replays
                      each day with that day's rules; a ledger with versions
                      of its own passes them over. recipes/versions/git.sh
                      writes one from git. Of each version, only its
                      documents of rules are kept in the ledger.
                      Without --rules, it is the rules, followed: its
                      newest version is in effect, read again every
                      --watch, and each version added to it later, as
                      recipes/versions/follow.sh adds them, is recorded
                      from its own time, the hours since replayed again
  --loki-rules PATH   one file or directory of Loki ruler rules, read again
                      every --watch, beside the rest
  --incidents FILE    read again each run; optional, as incidents can be
                      written down in the UI
  --deploys FILE      when services and namespaces were deployed, read again
                      each run: a list of deploys, each the labels that say
                      what was deployed (service, namespace) and either a
                      history recipes/versions/argocd.sh or flux.sh wrote
                      (REVISION TIME a line, beside the deploys file or
                      below it) or a start and an end. A firing that starts
                      during one, from --deploy-before before its start to
                      --deploy-after after its end, and agrees with it on
                      every label both have, one at least, is marked as
                      during a deploy: judged as ever, and shown apart, so
                      precision can be read with those firings and without
                      (docs/design/noc.md). Read again only when it or a
                      history changed
  --deploy-before DUR, --deploy-after DUR
                      how long before a deploy's start, and after its end, a
                      firing still counts as during it (default 0 and 10m;
                      a day at most each)
  --marked FILE       marked windows, read again each run: spans of a kind a
                      team names (a sale, a launch, a campaign), each a kind,
                      a title, a start and an end, repeating or not, of some
                      labels or a team's. A firing that starts during one
                      and agrees with it is marked with its kind: judged as
                      ever, never excused, and every count read inside the
                      kind and outside (docs/design/marked.md)
  --mark KIND=PROMQL  an expression that returns something while a window of
                      KIND is on, such as shop_sale_active == 1, read each
                      run as --impact is; its windows mark every firing.
                      Repeatable, eight at most
  --mark-match KIND=LABEL,...
                      the labels of each series of KIND's expression its
                      windows keep: they mark the firings that agree with
                      them (none by default)
  --runbook-base URL  an https URL a runbook that is a path in the rules'
                      repository (docs/runbooks/disk.md#full) is a link
                      under, such as
                      https://git.example.com/org/monitoring/blob/main/:
                      the rule's page links to it
  --lead, --match, --refire, --noisy-firings, --noisy-precision
                      the defaults; the UI can change them
  --linked-only       an incident that lists the alerts its tool linked to
                      it is caught by those alone; changed at a start,
                      every firing is judged again with it

The ledger:
  --ledger DIR        where the ledger is kept (default lapilli-ledger)
  --ledger-url-file FILE
                      keep the ledger in Postgres instead: the file holds
                      its connection string, as pgx reads it
                      (postgres://USER:PASSWORD@HOST:5432/DB?sslmode=verify-full),
                      read at start and said nowhere. Several serves on
                      one Postgres ledger are replicas: one leads, and runs
                      what is run once; each serves (docs/design/replicas.md).
                      "lapilli ledger copy" moves a SQLite ledger there
                      (docs/design/postgres.md)
  --every DUR         run this often (default 1h)
  --settle DUR        and replay the last DUR before the last run's end
                      again, for points that arrived late (default 1h)
  --backfill DUR      on first start, replay this far back (default 28d, at
                      most 3650d)
  --keep DUR          drop firings older than this (default 400d; 0 keeps
                      everything)
  --keep-activity DUR drop the activity log's changes older than this, never
                      sooner than --keep (default 800d, or --keep when
                      longer; 0 keeps them all, as with --keep 0)
  --watch DUR         read the rules this often (default 1m)
  --alarms-every DUR  read each --alarms this often (default 10m), so the
                      clouds' request quotas hold
  --digest DAY@HH:MM  once a week, judge the week before, work out longer
                      "for"s for noisy rules, and shorter ones that would
                      have caught the incidents nothing caught, and post the
                      report to --slack-webhook-file (default mon@09:00, in
                      --tz). Once an admin sets the digest on the Reports
                      page or at /api/v1/reports/digest, that is used
                      instead, and the flags only name where it can go
  --tz ZONE           the zone days and weeks are counted, and times shown,
                      in (default: local)
  --timeout DUR       give up a run after this long (default 50m)

Sharing the history with the alerts, so that Lapilli never takes their
quota (docs/design/quota.md). /metrics counts what this process asks of
each history API (lapilli_serve_history_*). Off by default:
  --yield-on-429      when a history API (--prometheus, --loki, Grafana's
                      data source proxy) answers a run's request with HTTP
                      429, ask it nothing more in the run, not even again
                      after a wait. The run stops there, and so does what
                      reads the history after it that round (reviews of
                      changes, the digest and what follows it, backtests
                      again, changes carried out, the replay set beside what
                      really fired), for a later round. The rules it didn't
                      replay are replayed first by a later run, from where
                      they were missed, and the runs go no further than a
                      week past the oldest of them, so none is given up; the
                      run history says why of each. Without it, a 429 is
                      asked again, as --max-rate says
  --yield-for DUR     and ask that API nothing for DUR after the 429, a day
                      at most (default 0: the next run asks again). While
                      --prometheus is left alone, runs wait, held in the run
                      history. What was yielded to is this process's: a new
                      leader asks afresh
  --guard EXPR        a PromQL expression asked of --prometheus before each
                      run and between the days of a catch-up, such as how
                      often the alerts' own evaluations fail
                      (rate(prometheus_rule_evaluation_failures_total[5m]) >
                      0): while it returns anything, or can't be read, the
                      run waits for the next --every, and so does what reads
                      the history after it, held in the run history. One the
                      history refuses as written fails the run, each time
  --catch-up-hours HH:MM-HH:MM
                      catch up only in these hours, in --tz, such as
                      01:00-06:00, at least --every long (twice is better): a
                      run more than a day behind where the ledger's firings
                      are up to (the first start's --backfill, one after
                      being down a day or more; not one replaying again from
                      when a version of the rules took effect) waits for
                      them, held. Runs are recorded in order, so the hourly
                      runs resume once it caught up; a run less behind runs
                      at any hour. Hours a change of clocks skips (02:00-03:00
                      on the night it does) don't come that day
  --catch-up-rate N   while catching up (more than a day behind), ask each
                      history API N requests a second at most, besides
                      --max-rate
  --check-replay=false
                      don't set the replay beside what really fired. It is
                      on with rules to replay: each day the runs won't
                      replay again is set beside ALERTS (or GRAFANA_ALERTS),
                      and a rule whose replay differs says so
  --check-series SEL  the series that say what really fired (repeatable;
                      default ALERTS and GRAFANA_ALERTS, as the rules need).
                      Without it, with --grafana or --check-grafana, what
                      Grafana's rules really fired is read from Grafana's
                      own alert state history instead, where it keeps one it
                      serves: in annotations, its default, from Grafana 12.4
                      on, or in Loki; GRAFANA_ALERTS first, while it holds
                      anything, where Grafana may keep it too. A day it can't
                      be read for, Grafana's rules aren't checked, and the
                      check goes on
  --check-grafana URL the Grafana that runs the Grafana rules --rules or
                      --rules-history gives, whose alert state history says
                      what they really fired, read with --grafana-token-file
  --check-differs N/RATIO
                      a rule differs when, over a week, it had N firings
                      either way and fewer than RATIO of them were alike
                      (default 5/0.9)
  --check-content=false
                      don't check what the rules say when they fire. It is
                      on with rules to read: each rule's page says what it
                      says and what to fix in it, the rules list marks those
                      with a problem of fact, and /metrics has
                      lapilli_rule_content_problems. With --grafana,
                      Grafana's notification templates, contact points and
                      policies are read once an hour too, with the same
                      token, and checked beside each rule
  --severities LIST, --scrape-interval DUR
                      as for lapilli verdict --check-content; without
                      --scrape-interval, Prometheus's /api/v1/status/config
                      and /api/v1/targets are asked once a day, for the
                      global interval and each job's: the range check
                      counts with them, and a replay asks Grafana's reduces
                      that don't count points at the shortest interval the
                      series they read are scraped at, or, where they
                      aren't served, at Grafana's own resolution (1s)

The web UI and API:
  --listen ADDR       where to listen (default 127.0.0.1:8080). Without a
                      sign-in (below), keep it inside, or behind a proxy
                      that signs people in. Listening beyond this machine
                      (:8080, a pod's address) with no roles, everyone may
                      only look
  --metrics-listen ADDR
                      also serve /metrics, /healthz and /readyz here, alone,
                      so a NetworkPolicy can let Prometheus reach only them
  --incidents-listen ADDR
                      also take incidents here, and nothing else, from a job
                      that holds the token in --incidents-token-file: PUT or
                      DELETE /api/v1/incidents/ID, POST /api/v1/incidents.
                      A NetworkPolicy can let that job reach this port alone,
                      and not the one whose headers name people
  --incidents-token-file FILE
                      the file holding that token, 16 characters at least;
                      read for each request, so it can be rotated
  --firings-token-file FILE
                      also take firings recorded elsewhere, such as a
                      cloud's own alarm history, on --incidents-listen,
                      from a job that holds this token: PUT
                      /api/v1/firings/SOURCE?from=&to= with the firings
                      that start in [from, to), one JSON object a line as
                      --firings reads them (recipes/firings/put.sh sends a
                      recipe's), in place of those the ledger has from
                      SOURCE then. With it, --rules (or the like) and
                      --prometheus are optional: without them nothing is
                      replayed, and what is copied in is judged as it comes
  --source-quiet DUR  tell, once, of a source nothing has been copied in from
                      for this long, to --notify-webhook-file and, with
                      --notify-slack, Slack: the job copying it may have
                      stopped (default 26h; give a weekly job's 8d)
  --tasks-token-file FILE
                      also let a job keep tasks in step with a tracker, on
                      --incidents-listen, with this token: GET
                      /api/v1/tasks?linked=false (or true)&status=open,doing
                      &after=ID, and PATCH /api/v1/tasks/ID with a task's
                      link (an https URL, set once) and status, nothing else
                      (recipes/trackers)
  --allowed-hosts LIST
                      host names to answer to, comma-separated, besides an
                      address, localhost, a name without dots or under
                      .svc, .cluster.local or .internal, and --external-url's:
                      the name a proxy passes on, if it is another. Any
                      other name is refused, against DNS rebinding; * for
                      any
  --user-header NAME  the header such a proxy names the person in, such as
                      X-Forwarded-Email; changes are recorded under it. Set it
                      only behind a proxy that sets it and strips it from
                      callers
  --read-only         take no changes through the UI or API
  --config FILE       a YAML file declaring teams, quiet windows (windows),
                      reports and roles (grants), as the UI's forms make
                      them, which the ledger is kept holding: at start, and
                      whenever its content changes, read again every 15s.
                      What it declares the UI shows and doesn't change; GET
                      /config.yaml gives what the ledger holds in its form.
                      A file that can't be used changes nothing, a thing it
                      declares that can't be kept is kept as it was, and the
                      Settings and Run history pages say why
                      (docs/design/serveops.md). Under notify:, routes:
                      who is told what, by kind, team and labels, and a
                      default (docs/design/notifications.md)
  --config-prune      delete what --config no longer declares, once the file
                      is read the same twice in a row, but a team something
                      the UI made names; without it, that is the UI's again.
                      A goal (goals:) the file no longer declares is deleted
                      either way
  --goal-tasks        make a task, of kind decide, for each goal --config
                      declares that a whole week missed, or an occurrence
                      of a goal over a period (each: KIND), 20 a run at
                      most; one open for a goal and team is commented on
                      as weeks (occurrences) are missed again, met, or no
                      longer missed (docs/design/goals.md). Off unless
                      turned on
  --expect FILE       an expectations file, as lapilli verdict --expect
                      takes, that the rules are held to after deploy: after
                      each run, each rule's budgets and catches are checked
                      over the week so far and the twelve whole weeks
                      before, shown on its page, the overview and the
                      digest, and told once a rule, an expectation and a
                      week when broken. Read again every 15s, as --config;
                      a file that can't be read changes nothing. --config's
                      expectations: and a rule's annotation
                      lapilli.dev/budget hold rules too
                      (docs/design/expectations.md)
  --expect-tasks      make a task, of kind tune, for the rule's team, of
                      each expectation broken, once a rule, an expectation
                      and a week, 20 a run at most, none while one is open.
                      Off unless turned on
  --promtool PATH     run the scenarios the expectations hold rules to
                      (scenarios:, promtool's own test files, read in the
                      directory of the file that declares them) with
                      promtool at PATH, against each rule's group as the
                      rules in effect have it, again when either changes:
                      5 a run, 30s each, with no environment and its
                      rule_files the group alone. Only rules in
                      Prometheus's format. Without it, each scenario is not
                      known. Scenarios a pull request's backtest sends
                      (--backtest-listen) are never run
  --promtool-memory SIZE
                      the memory one run of --promtool may take, as 256Mi
                      or 1Gi, 64Mi at least (default 256Mi): on Linux,
                      promtool's RLIMIT_DATA, past which the run stops and
                      the scenario is not known; promtool runs in serve's
                      container, so leave room for it under its memory
                      limit. Not set on macOS, where promtool runs without
                      a cap
  --team-labels LIST  the labels whose values the Teams page suggests as
                      teams, when the rules carry values no team owns by,
                      comma-separated (default team,owner,squad)
  --everyone-admin    with no roles (--editors, --admins), let everyone who
                      reaches the UI and API change anything, as an admin,
                      though it listens beyond this machine. Without roles,
                      and without it, only a server listening on loopback
                      (127.0.0.1, localhost) takes changes; beyond it, every
                      pod or machine that shares its network could
  --viewers LIST      who may look, and with a sign-in of lapilli's own who
                      may come in at all, beside those the other lists name:
                      people, group:NAME, *@DOMAIN (everyone whose name ends
                      in @DOMAIN), or * for everyone signed in. With it,
                      everyone else is refused, 403
  --editors LIST      with this or --admins, only those named may change
                      anything, and the rest may only look: a comma-separated
                      list of people (as --user-header names them) and groups
                      (as --groups-header lists them), *@DOMAIN, or * for
                      everyone signed in (with a sign-in of lapilli's own,
                      only once --viewers names who may come in). Editors write incidents, tasks, comments,
                      quiet windows and notes on rules. user:NAME is a
                      person and group:NAME a group, alone: use them where
                      someone could pick a name that is a group's
  --admins LIST       as --editors, who may also change the settings, the
                      teams, the reports and the weekly digest, and give an
                      editor's role, of every team or of one, on the Roles
                      page or at /api/v1/roles: never an admin's or an
                      approver's, which stay these flags'
  --groups-header NAME
                      the header such a proxy lists the person's groups in,
                      separated by commas, such as X-Forwarded-Groups. With
                      roles, nothing but the proxy should reach lapilli
                      serve, and it should strip both headers from callers

Signing people in, without a proxy of plain headers (one of these, or
--user-header; docs/design/signin.md). Every page and API call then needs
someone signed in, but /healthz, /readyz and /static/, and only those a
role names come in at all: --viewers, --editors, --admins, --approvers,
or a role given in the UI. An identity provider signs in whoever it
knows, which for a public one (Google's for any account, GitLab.com,
GitHub through Dex, a multi-tenant Entra ID app) is anyone:
  --oidc-issuer URL   sign people in with this OpenID Connect provider, its
                      configuration discovered from the URL (https, or http
                      to this machine or a name inside a cluster): the
                      authorization code flow, with PKCE, state and nonce.
                      A browser is sent to sign in, and keeps a session in a
                      signed cookie; an API client or an agent sends an ID
                      token the provider gave this client as a bearer
                      token. Needs --external-url (https, or http to this
                      machine), whose /auth/callback the provider sends
                      people back to
  --oidc-client-id ID the client lapilli is registered as
  --oidc-client-secret-file FILE
                      the file holding the client's secret, read for each
                      sign-in; without it, a public client (PKCE alone)
  --oidc-scopes LIST  the scopes asked for, comma-separated (default
                      email,profile, and openid always); add groups where
                      the provider lists them only when asked
  --oidc-hosted-domain LIST
                      Google Workspace domains people must come from (the
                      hd claim), comma-separated
  --oidc-trusted-clients LIST
                      the provider's other clients whose ID tokens (azp)
                      are taken as this one's, comma-separated; others
                      are refused
  --oidc-bearer=false take no ID token as a bearer token: sessions alone
  --user-claim NAME   the claim that names the person (default email; with
                      --oidc-issuer, email_verified must be true beside it)
  --groups-claim NAME the claim that lists their groups, for --editors,
                      --admins and --approvers (default groups; empty for
                      none)
  --session-max-age DUR
                      how long a session lasts, and the oldest a bearer
                      token is taken at, from its iat, whatever its own exp
                      (default 12h; 5m to 30d)
  --session-key-file FILE
                      a key of 32 characters or more that session cookies
                      are signed with, read at start; without it, one made
                      for the process, so sessions end when it restarts.
                      Whoever holds the key can sign in as anyone; a new
                      one signs everyone out
  --proxy-jwt KIND    take who each request comes from from the JWT a
                      sign-in in front signed, checked against its keys:
                      google-iap (Google Cloud's Identity-Aware Proxy,
                      x-goog-iap-jwt-assertion), aws-alb (an AWS Application
                      Load Balancer's sign-in, x-amzn-oidc-data), or jwks
                      (any other: oauth2-proxy, an Azure App Service's
                      sign-in, an access proxy). A request without it, or
                      with one that doesn't verify, gets 401
  --proxy-jwt-audience AUD
                      google-iap: the backend service's,
                      /projects/NUMBER/global/backendServices/ID; jwks: the
                      aud the JWTs carry
  --proxy-jwt-alb-arn ARN
                      aws-alb: the load balancer whose signature is taken
  --proxy-jwt-issuer URL
                      jwks: the iss the JWTs carry; aws-alb: the identity
                      provider's, to check it too
  --proxy-jwt-jwks URL
                      jwks: where the keys are
  --proxy-jwt-header NAME
                      jwks: the header the JWT is in; Authorization takes it
                      as a bearer token

Telling of what happens (tasks made, assigned, done or overdue; incidents
written down; quiet windows kept; comments):
  --notify-webhook-file FILE
                      the file holding a URL to POST each, as JSON, to; for
                      a tracker, a chat or anything else that takes a
                      webhook. Sent apart from the writes, and tried again,
                      further apart, for about eight hours when it fails
  --notify-webhook-secret-file FILE
                      the file holding a key, 32 bytes at least (a line break
                      at its end isn't of it), to sign each post to
                      --notify-webhook-file with: X-Lapilli-Timestamp, the
                      Unix seconds it is sent at, and X-Lapilli-Signature,
                      sha256= and the HMAC-SHA256 of the timestamp, a dot
                      and the body, in hex. Read for each post, so it can be
                      rotated
  --notify-slack      also post each, as a line, to --slack-webhook-file
  --external-url URL  where people reach the UI, such as
                      https://lapilli.example.com; what is told links to it
  --lang en|es|ja|ko  the language of what is posted to Slack (default en)
  --team-digest TEAM=FILE
                      also post each week's digest for one team, its own
                      rules and incidents, to the Slack-style incoming webhook
                      whose URL is in FILE (repeatable). TEAM is a team's name
                      in the ledger. --notify-webhook-file gets the digest as
                      digest.ready, for everyone and for each team
  --report-dest NAME=FILE
                      a destination the reports people schedule in the UI can
                      go to: the Slack-style incoming webhook whose URL is in
                      FILE (repeatable). NAME is lowercase letters, digits and
                      . _ -. Reports can also go to slack (--slack-webhook-
                      file), webhook (--notify-webhook-file, as
                      report.ready) and team:TEAM (each --team-digest)
  --webhook-dest NAME=FILE
                      a destination routes in --config's notify: can send
                      to: the webhook whose URL is in FILE, posted JSON as
                      --notify-webhook-file is (repeatable). NAME is
                      lowercase letters, digits and . _ -, none that
                      --report-dest gives. It takes only what a route
                      sends it; needs --config
  --webhook-dest-secret NAME=FILE
                      the file holding a key, 32 bytes at least, to sign
                      each post to --webhook-dest NAME with, as
                      --notify-webhook-secret-file signs the webhook's
                      (repeatable)

Notifications as they were sent, kept beside the firings: how many each rule
sent, how many repeats, how many woke people, how many for nothing:
  --notify-listen ADDR
                      take notifications here, and nothing else, each source
                      on a path of its own: POST /v1/alertmanager (a webhook
                      with the token as its bearer token),
                      /v1/alertmanager/events (Alertmanager's event
                      recorder, 0.33.0 on: every delivery, PagerDuty's and
                      Slack's too, the same token; ?skip=NAME passes over a
                      receiver that is Lapilli's own webhook), /v1/grafana (a
                      webhook contact point, with the token or an HMAC
                      signature), /v1/sns (Amazon SNS, for Amazon Managed
                      Service for Prometheus), /v1/azure (an action group's
                      webhook, common alert schema, the token as the URL's
                      password), /v1/google (a Cloud Monitoring webhook
                      channel, with basic auth). ?receiver=NAME names the
                      receiver where the notification doesn't
  --notify-token-file FILE
                      the file holding that token, 16 characters at least;
                      read for each request, so it can be rotated
  --notify-grafana-secret-file FILE
                      the file holding Grafana's HMAC secret, 16 characters
                      at least. Set Grafana's timestamp header to
                      X-Grafana-Alerting-Timestamp
  --notify-sns ARNS   the SNS topics taken, by ARN, comma-separated. A
                      subscription to one is confirmed by fetching its
                      SubscribeURL, from an SNS host only: the one request
                      the port makes

Changing rules, with a person's approval (off by default: Lapilli reads,
and proposes; docs/design/changes.md):
  --changes           carry out the changes a person approved: each checked
                      again (the rule read again, the backtest run again),
                      then handed to a recipe that opens a pull request where
                      the rule is kept (recipes/changes/), or, for a rule kept
                      only in Grafana, a ruler or a cloud's own alarms,
                      applied through its API with what it replaces kept for
                      undo. Turning a rule off, or deleting it, waits while
                      it fires. Lapilli also
                      proposes, after each week's digest: longer fors for
                      noisy rules, thresholds and fors for near misses,
                      retiring spare rules and those that never fired. Needs
                      --user-header and --admins (without roles, whoever may
                      change anything is an admin)
  --approvers LIST    who may approve a change, as --editors lists people
                      and groups (default: the admins). Approving is a form
                      on the proposal's page, in a browser: never the API,
                      MCP or a token. No one approves their own
                      proposal, or one their agent made
  --approvals N       how many people approve each change: 1 (default) or
                      2. An undo of an applied change is the exception: the
                      one approver who asks for it approves it, ticking the
                      box that they read the change, as an approval does
  --changes-token-file FILE
                      the change recipes' token, on --incidents-listen: GET
                      /api/v1/changes and PATCH /api/v1/changes/ID, what
                      became of a pull request, and nothing else
  --changes-grafana-token-file FILE
                      a Grafana service account's token that may write the
                      alert rules Lapilli may change, for --grafana. An
                      Editor's changes a for or what a rule says; turning a
                      rule off takes an Admin's, as Grafana keeps a pause
                      from an Editor (answering that it took it)
  --changes-in LIST   the namespaces of --ruler Lapilli may change rules in,
                      comma-separated, each a name or a prefix ending in *
                      (team-*); on Azure, rule groups, RG/NAME, or NAME when
                      --ruler names one resource group, in any letter case.
                      And a cloud's alarms, SOURCE:NAME, an --alarms source
                      and an alarm in it: CloudWatch's alarm name, Azure's
                      RG/NAME (in any letter case), Google Cloud's policy's
                      display name, OCI's display name (cloudwatch-prod:
                      checkout-*). Without it nothing in a ruler or a
                      cloud is changed. Name none that a tool keeps
                      (mimirtool rules sync, Terraform, CloudFormation,
                      Config Connector, Alloy's mimir.rules.kubernetes): it
                      would undo the change, or fight it. A ruler's token,
                      and Google Cloud's and OCI's permissions, reach more
                      than these: this is Lapilli's own check
  --changes-managed KEY=VALUE
                      a tag that means another tool keeps an Amazon
                      namespace, an Azure rule group or a cloud's alarm,
                      KEY=* for any value (repeatable), an OCI defined tag
                      as NAMESPACE.KEY=VALUE: one carrying it is for a
                      person to change. CloudFormation's
                      aws:cloudformation:stack-id, and Google Cloud's
                      managed-by-cnrm=true and goog-terraform-provisioned=true,
                      are known without it
  --changes-ruler-token-file FILE
                      a token that may write a Mimir's or a Cortex's rules
                      (rules:write), for --ruler; without it, writes are sent
                      with no token, for a ruler reached only in the cluster
  --changes-aws-role ARN
                      a role the pod's identity assumes to write Amazon's
                      rule groups namespaces, allowed
                      aps:PutRuleGroupsNamespace and
                      aps:DescribeRuleGroupsNamespace on the namespaces
                      named, and CloudWatch's alarms (see the chart's
                      README); without it, the pod's identity writes
  --changes-azure-client-id ID
                      the client ID of a user-assigned identity to write
                      Azure's rule groups and alert rules as, federated
                      with the same service account and given a role that
                      reads and writes those named; without it, the pod's
                      identity writes
  --changes-google-service-account EMAIL
                      a service account the pod's identity impersonates
                      (IAM Credentials' generateAccessToken) to write Google
                      Cloud's alerting policies, given a role that reads,
                      updates, deletes and creates them; without it, the
                      pod's identity writes
  --changes-oci-config FILE
                      an OCI config file, and the API key its key_file
                      names, of a user to write OCI's alarms as, in a group
                      allowed to update them in the compartments named;
                      without it, the pod's identity writes
  --changes-limits D,C
                      at most D deletions a day and C changes an hour
                      (default 5,20); a change past them waits
  --changes-paused FILE
                      while this file is there, nothing is carried out or
                      handed out; an admin can also pause changes in the UI
  --proposals-expire DURATION
                      close a proposal no one approved that waited longer
                      since it was made, such as 30d, as expired, so its rule
                      takes another; 0, the default, keeps it waiting

Agents, over MCP at /mcp:
  --mcp-compare editors|admins
                      who an agent must act for to replay a change to the
                      rules it proposes (the compare tool). Off by default:
                      it runs PromQL the agent writes, with lapilli's access
                      to Prometheus, and, for a rule kept in Loki (--loki),
                      LogQL the agent writes, sent to Loki, or through
                      Grafana's data source proxy, with lapilli's token. It
                      needs roles (--editors or --admins). The tools that
                      read are everyone's
  --mcp-propose editors|admins
                      who an agent must act for to propose a change to a
                      rule (the propose tool), kept as an agent's for a
                      person to approve. An agent never approves or applies

Pull request backtests, from CI jobs (off by default; docs/serve.md, "Pull
request backtests"):
  --backtest-listen ADDR
                      take pull request backtests here, and nothing else:
                      POST /v1/backtests from a CI job, with the rules before
                      and after (recipes/backtest's serve mode), replayed by
                      the replica that leads, one at a time, with this
                      serve's access to the history and judged by its
                      record; GET /v1/backtests/ID, with the key given, says
                      where it is and, once done, gives the comment lapilli
                      verdict --compare --changed-only --markdown writes;
                      DELETE cancels it. Prometheus-format rules only, 50
                      that differ at most, each selector of one metric by its
                      name. It runs PromQL the pull request writes, with this
                      serve's access: give that access what the rules need
                      and no more. Needs --backtest-trust
  --backtest-trust FILE
                      who may ask, read at start: a YAML file of entries
                      under trust:, each a CI's issuer of ID tokens (issuer,
                      audience, the claims each token must hold: the
                      repository's id, as one or a list, and the event, for
                      GitHub Actions, GitLab CI and Forgejo Actions) or a
                      plain token (token_file, 32 characters at least, read
                      for each request), with days (14) and hourly (30). One
                      that can't be used stops serve from starting
  --backtest-share F  of --max-rate, the share the backtests ask the history
                      at, besides the runs' own pace, so that together they
                      never pass it (default 0.25; 0.05 to 0.75). Without
                      --max-rate, a quarter of --parallel at once, one at
                      least
  --backtest-queue N  backtests waiting at most, 3 of an entry and one of a
                      pull request, a newer commit's taking the place of the
                      one waiting (default 20; 1 to 1000)
  --backtest-timeout DUR
                      one backtest's time, from its start (default 15m; 1m
                      to 2h): one that couldn't finish in it at the lane's
                      rate is refused before it is queued, saying what
                      period would fit

Pages: / (overview), /rules, /incidents, /firings, /settings, /runs, /teams,
/windows, /tasks, /proposals, /outcomes. API: /api/v1/report?from=&to=
(&format=csv: the firings a row each, docs/numbers.md; &redact=true: every
name an alias, as lapilli verdict --redact gives),
/api/v1/incidents, /api/v1/rules, /api/v1/runs, /api/v1/settings,
/api/v1/teams, /api/v1/notes, /api/v1/windows, /api/v1/tasks,
/api/v1/comments, /api/v1/proposals, /api/v1/me, /api/v1/outcomes,
/api/v1/summary?from=&to=&team= (each week, for everyone and each team:
critical firings and the share that caught something, re-fires, off hours,
during deploys, incidents and impact windows caught and missed, the median
lead; docs/design/noc.md). MCP: /mcp. Also /metrics, /healthz, /readyz.
```

## `lapilli preflight`

What it's for: [Before it is deployed](serve.md#before-it-is-deployed).

```text
lapilli preflight says what lapilli serve will meet before it is deployed,
given serve's own flags: whether it reads the rules and how many it can
replay, whether it reaches the history and signs in to it, what its tokens,
files and ledger hold, and how many requests, how long, how many nights and
how much memory its runs will take. It writes nothing, replays nothing, and
sends no request but the reads it makes, which an allowlist holds it to
(docs/design/preflight.md). What the clouds' credential chains do of their
own is outside it: Google's metadata server (and whether it is there, off
Google Cloud), the Azure CLI (which may write its token cache), OCI's
instance and resource principals and OKE's workload identity (its auth
service's POST among them), and the processes AWS's credential_process and
Google's executable credentials start.

Usage:
  lapilli preflight [serve's flags] [--json] [--fail-unknown] [--offline]

Every flag of lapilli serve (lapilli serve -h), read as serve reads it: one
serve refuses, it refuses, and exits 2. Its own:
  --json              the report as JSON: its items' ids and fields stay the
                      same between releases (docs/numbers.md)
  --fail-unknown      exit 1 for an item it can't say, as for a problem
  --offline           ask nothing: the items that need a request are
                      unknown; the flags, the rule files, the other files
                      and the estimates of requests and time are worked out.
                      The files of secrets and --ledger-url-file aren't
                      read. For CI, against what a pull request changes
  --preflight-timeout DUR
                      the whole of it (default 2m); --timeout stays serve's,
                      a run's, read for the estimates

Each item is ok, a problem or unknown, with why, and for a problem what to
change. It exits 0 with no problem, 1 with one (or, with --fail-unknown, an
unknown), 2 when the flags are wrong. Run it where serve runs, with serve's
service account and volume, for the items that need its credentials (the
history, Grafana's token, the clouds, the ledger): never in CI with the
production credentials. With --changes, what each write token or identity
may write is asked of an API that says so without writing: Grafana's list
of the token's actions, IAM's policy simulator (the role needs
iam:SimulatePrincipalPolicy on its own ARN, or the item is unknown), Cloud
Resource Manager's testIamPermissions, Azure's permissions, deny
assignments and locks (Microsoft.Authorization's permissions, denyAssignments
and locks read), and OCI's policies (the user needs to inspect groups,
users, compartments and policies in the tenancy; a policy with a condition
is never a grant). With --backtest-listen, --backtest-trust is read as
serve reads it at start, and said as serve would take it, by its entries'
names, CIs and the claims they pin, or as serve would refuse it; each
issuer it names is asked once for its discovery document and its keys, as
a token's first check asks them; and the estimates time the hourly runs at
--max-rate less --backtest-share too, as while a backtest runs, and say how
many requests, and how many days of ten of the rules changed, a backtest
fits in --backtest-timeout. What it prints is redacted as a pull request's
backtest comment is: the URLs given as their flag's name, their hosts and
users, addresses, tenants, a --gcp-project; a secret by its file's path and
length; what a trust file pins, its audiences, and an issuer of no public
CI's or a jwks_url (said as the file's issuer N), never.
```

## `lapilli ledger`

What it's for: [The ledger in Postgres](serve.md#the-ledger-in-postgres).

```text
lapilli ledger copy copies a ledger into another, empty one: a SQLite
ledger into Postgres, to keep it there from then on, or one back. Stop
lapilli serve first: a Postgres ledger serve holds is refused, and what a
SQLite ledger is written after the copy reads it is left out.

Usage:
  lapilli ledger copy --ledger DIR --to-url-file FILE
  lapilli ledger copy --ledger-url-file FILE --to DIR

  --ledger DIR        the SQLite ledger to copy, as lapilli serve --ledger
                      keeps it (DIR/ledger.db)
  --ledger-url-file FILE
                      or the Postgres ledger to copy: the file holds its
                      connection string, as for lapilli serve
  --to DIR            copy into a SQLite ledger there, made if it isn't
  --to-url-file FILE  or into the Postgres database the file's connection
                      string names, in its schema (search_path)

The ledger copied into must be empty: new, or only opened. Every table is
copied in one transaction, so a copy that fails leaves it empty; ids are
kept. Then both are read back, table by table, and a copy that differs
fails. The ledger copied from is brought to this lapilli's schema first, as
lapilli serve would, and its days and weeks stay counted in its time zone:
serve counts them again if --tz says another.
```
