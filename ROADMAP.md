# Roadmap

Lapilli answers one question: which of your alerts are worth their page?
`lapilli verdict` answers it once, for a period; `lapilli serve` keeps
answering it, every hour, in a ledger with a dashboard. What follows is what
it doesn't do yet, area by area, with where it lives in the code. Each area
is a piece someone can take on and come to own; see
[MAINTAINERS.md](MAINTAINERS.md) for how owning an area works.

What comes first: the replay and `lapilli verdict`, with the pull request
backtests built on them. Everyone starts there, and a verdict wrong there
is wrong in every page after it, so a replay that differs from what really
fired is fixed before anything is added. What is large and will have to
be built anyway is built, not put off for being large. And where Lapilli
doesn't do something by default that someone may need, it is an option: a
flag that turns it on or off, or a value they can set.

The areas sort into a core and what is built on it. The core is the replay
and the verdict on it: [A first look](#a-first-look),
[Replay fidelity](#replay-fidelity), [What really fired](#what-really-fired),
[What to change](#what-to-change),
[Where rules and alarms come from](#where-rules-and-alarms-come-from),
[Clouds, checked for real](#clouds-checked-for-real),
[Pull request backtests](#pull-request-backtests) and
[Incidents in](#incidents-in). What is built on it is
[lapilli serve](#lapilli-serve) and [Changing rules](#changing-rules).
[Packaging and trust](#packaging-and-trust) covers both.

Some areas need what the current maintainer doesn't have: an AWS or Azure
account with a managed Prometheus, Loki alerts in real use, an incident
tool. If you do, you are the person this needs.

## Toward 1.0

1.0 is when what Lapilli keeps stops changing in a minor release. While
the version is 0.x, a minor release may change a flag or the API, its
changelog entry saying how (GOVERNANCE.md); the numbers past 0.99 are
0.100 and on, so 1.0 comes when what is below is done, not by counting.

What 1.0 keeps, through every 1.x:

- What [docs/numbers.md](docs/numbers.md#what-stays-the-same) lists (the
  report's fields, the metrics and their labels, the CSV's columns, the
  summary's and `--compare`'s schemas) changes only in a major release. A
  minor release may add to it, as now, said in the changelog.
- A command's flags, and serve's API ([docs/api.md](docs/api.md)), keep
  their names and meaning. One renamed is still taken under its old name
  until the next major release, a flag saying on stderr what it is now;
  one taken away goes only in a major release.
- The chart's values keep their names the same way.
- A ledger a 1.x release wrote opens in every later 1.x, upgraded as it
  opens. What it holds that was recorded (the firings, incidents,
  notifications, notes, comments and what people set) is kept through each
  schema step; what is worked out from it may be worked out again. Going
  back to an earlier release after a schema step needs the copy kept
  before it, as now ([docs/upgrading.md](docs/upgrading.md)).

What comes before it, as it is what everyone starts from:

- **Newer servers.** The replay checked against the latest Prometheus,
  Grafana and Loki, not only the versions pinned here: their recordings
  taken with the `measure` workflow and kept in `internal/replay/testdata`
  beside the pinned ones, each real-server test passing on both
  ([Replay fidelity](#replay-fidelity)). Done for Prometheus 3.16.0,
  Grafana 13.3.2 and Loki 3.7.10 (0.72); each newer release is measured
  so as it comes.
- **Pull request backtests through serve without a gap.** A rule serve
  doesn't run backtested, not left out; and a Loki ruler's rules, Grafana's
  rules and cloud alarms compared as parsed, not by their text, as
  Prometheus's are ([Pull request backtests](#pull-request-backtests)).
- **Several replicas held by CI.** The kind e2e of three replicas
  (`e2e-replicas`) a required check ([lapilli serve](#lapilli-serve)).
- **The names looked at once more.** Every flag, API route, chart value
  and report field read through as it will be kept; what 0.x left
  misnamed, or doing two things, renamed or split in the last 0.x minor
  releases, said in the changelog with what to do; and what takes a
  renamed flag or route under its old name, which nothing does yet.

Until then minor releases go on at most twice a week, each with what was
merged since the last (GOVERNANCE.md); 1.0 is the release after the last
of these is done. What needs an account or people the maintainer doesn't
have (checking the clouds, the trackers, the incident tools and managed
Postgres for real, a review by people who use a screen reader, the
Homebrew tap) doesn't wait for 1.0 and doesn't hold it: each comes in a
1.x minor release as someone brings it.

## A first look

`cmd/lapilli`

`lapilli demo` (0.69) runs `lapilli verdict`'s own code over a sample
built in, a small shop's week, reading no Prometheus and sending nothing
([docs/design/demo.md](docs/design/demo.md)). `lapilli demo --serve`
(0.70) shows the same week in `lapilli serve`'s pages, by its own code,
read-only, on loopback alone, its clock pinned to the end of the sample's
week.

## Replay fidelity

`internal/rules`, `internal/sse`, `internal/replay`

A replay is only as useful as it is faithful: a rule that fired differently
from how it really did is the worst kind of wrong.

Built (0.25), measured against a real Grafana 13.3.1 and Prometheus 3.5.0
(and Grafana 13.3.2 and Prometheus 3.16.0, 0.72),
and kept as test data ([docs/design/states.md](docs/design/states.md)):
Grafana's no data states and missing series, classic conditions with every
reducer, `query_offset`, a Grafana window ending before now, `--late`, and
groups placed where Prometheus places them, for rules read from its API,
and for rule files by the group of the same name the rules API lists, its
hash checked against the groups' own evaluations (0.72); a placed group
replayed from its last evaluation before the period, so a firing on at the
period's start is replayed as it fired (0.72). Still to do:

- Newer Grafana, Prometheus and Loki. What Grafana's expressions mean is
  written again here (`internal/sse`), not read from Grafana. Built: the
  real-server scripts in `test/e2e` take `PROMETHEUS_IMAGE`,
  `GRAFANA_IMAGE` and `LOKI_IMAGE` (and the Thanos and VictoriaMetrics
  ones), as do the chart's CronJob and the whole service on kind
  (`run.sh`, `service.sh`) for the first two, and `upstream.yml` runs them
  weekly against each one's latest image, so a change in how a server
  evaluates is found by a failing run before a user's `--check-replay`
  finds it. The measurements kept as test data are taken again at any
  version by `test/e2e/measure.sh` and the `measure` workflow (0.72), and
  kept for the pinned versions and the latest (Prometheus 3.16.0, Grafana
  13.3.2 and Loki 3.7.10, recorded by the workflow, every real-server test
  passing on both; 0.72). Still to do: the workflow is run by hand, a run being two hours and its recordings looked at
  before they are kept; and nothing opens the pull request that moves a
  pin, by choice so far (a workflow that could would need to write to the
  repository).
- Query errors. History can't say when a query failed, so a rule's error
  states aren't replayed: Grafana's error alerts are left out of what really
  fired, and `--check-replay` says how many; `serve` keeps no count of them.
  A Grafana rule whose condition comes to a time series, which errors at
  every evaluation, is listed as not replayed, with why (0.72).
- A query's history before the day before a period: a query that has
  returned nothing for longer is taken to have no history, not no data.
- Rules written in LogQL are built (0.28): a Loki ruler's, from its files
  or its API, and Grafana's on a Loki data source, through Grafana's data
  source proxy, measured against Loki 3.7.9 and Grafana 13.3.1 (and Loki
  3.7.10 and Grafana 13.3.2, 0.72)
  ([docs/design/loki.md](docs/design/loki.md)); classic conditions over
  Loki, and a Grafana rule reading Prometheus with a Loki query beside its
  condition, replayed whole (0.72), checked against a real Grafana and Loki
  in `test/e2e/loki.sh` and in those measurements. Still to do: a condition reading Prometheus and
  Loki together, and a Prometheus query beside a Loki condition, which need
  a condition evaluated over two histories; a Loki group's fraction of a
  second, and instant queries an evaluation for a short `--check-replay`,
  which would see exactly what the ruler saw. A Loki ruler's rules are
  changed as every other rule is: those read from its API through it, as
  Mimir's are, and the chart takes Loki's values, since 0.70
  ([docs/design/rulers.md](docs/design/rulers.md)), and those of its rule
  files by the git path, a new rule to either, since 0.72. SQL and the other data sources stay out: a
  database keeps no history of what a query returned (the design says why).
  Their rules, read with `--grafana`, are judged from Grafana's own alert
  state history instead, with no backtest
  ([docs/design/grafanajudged.md](docs/design/grafanajudged.md), 0.68),
  each change taken for the rule the history says it is of, by its uid or
  id, since 0.69.2, with the severity its labels say since 0.70; still to
  do: the rules' ids read from wherever Grafana lists them once its
  provisioning API, which its main branch marks deprecated, is gone; a day
  past the reader's bounds, or of more firings than a write holds, a gap
  for every rule judged so; and those `--check-grafana`'s Grafana runs from
  files.
- Metrics whose lateness varies. `--late` is one delay; a gap the live rule
  saw, filled in a minute later, isn't in history at all.
- A ruler started within an interval of a period's start: a placed group
  is replayed from its last evaluation before the period (0.72), which a
  ruler that had just started hadn't made yet. Lapilli doesn't know when a
  ruler started.
- How long before a period Grafana's alerts are worked out from: an hour, for
  `noDataState` other than `NoData`.

## What really fired

`internal/verdict`, `cmd/lapilli`

`--check-replay` sets a replay beside what really fired, once, when asked.

- The same in `lapilli serve`, kept in the ledger, so a rule starting to differ
  shows on its page: built (0.21)
  ([docs/design/fidelity.md](docs/design/fidelity.md)). Still to do: the
  threshold a rule differs at, set from what real ledgers show.
- For Grafana's rules, what really fired is read from Grafana's own alert
  state history (0.50), in annotations or in Loki, where `GRAFANA_ALERTS`
  isn't written ([docs/design/grafanahistory.md](docs/design/grafanahistory.md)).
- Notifications as they were sent are built (0.20): Alertmanager's and
  Grafana's webhooks, SNS for Amazon Managed Service for Prometheus, Azure
  Monitor's action groups and Google Cloud's webhook channels, each on a path
  of its own ([docs/design/notifications.md](docs/design/notifications.md)).
  A cloud alarm's own notifications, every cloud alike (CloudWatch through
  SNS, Azure Monitor's other alerts, Google Cloud's other policies, OCI
  through Notifications), tied to the firings copied in, are taken since
  0.21 ([notifications.md](docs/design/notifications.md#a-clouds-own-alarms-notifications-v021)).
  Alertmanager's event recorder, every delivery, PagerDuty's and Slack's
  too, is taken since 0.27 ([docs/design/inputs.md](docs/design/inputs.md)),
  and its mutes, which say why a firing paged no one, since 0.67
  ([docs/design/mutes.md](docs/design/mutes.md)), counted in the digest
  and the reports since 0.68; its file and Kafka outputs, sent on by
  [recipes/events](recipes/events), since 0.70. Still to do: checking the
  SNS, Azure, Google Cloud and OCI paths against real accounts.

## What to change

`internal/source`, `internal/verdict`

A noisy rule gets a longer `for` that would quiet it; a rule that nearly caught
an incident nothing caught gets the shorter `for`, or the number short of its
threshold, that would have. With no incident written down, nothing is
noisy: a rule that fires as often says what its firings were (0.41).

- What a rule's firings were with no record is said of one bound, a
  range, and comparisons joined by `and`, `or` and `unless` where they
  decide the condition (0.70); a value beside them under `or`, or a
  comparison under `!` or inside arithmetic, is still left unsaid.
- A `for` one event can't outlast is known of PromQL's counts of events
  (`increase`, `changes`, `resets`, `rate` of a counter), in a Prometheus
  rule or a Grafana rule's one instant query, compared as it is or reduced
  by `last`, `max`, `min` or `mean`, and of LogQL's `count_over_time`,
  `rate`, `bytes_over_time` and `bytes_rate` (0.70). Grafana's classic
  conditions, a Grafana rule's range queries, and LogQL's `unwrap` are
  still left unsaid.
- lapilli serve judges a firing by its day's record (0.72; `--record-by-week`
  keeps the week); lapilli verdict by the record at the firing's own time.
  They still differ where an incident starts later in the firing's day, on
  the day an incident's two weeks end, and where an impact signal starts
  within a day ([docs/design/judging.md](docs/design/judging.md)). Taking
  the impact signal's data floor by the day would bring them closer still.

- Near misses by value for a Grafana math expression joining comparisons
  with `&&` or `||`, and for classic conditions, are built (0.25). A
  comparison under `!`, or inside arithmetic, is still read only as a whole.
- Shifts read from where on-call schedules are kept: built (0.27)
  (`--on-call`, iCalendar feeds); monthly and yearly repeats, a feed's own
  `VTIMEZONE` and Windows zone names (0.70); `RDATE`'s dates, days and
  periods (0.71). Still to do: checking each tool's feed against a real
  account.

## Where rules and alarms come from

`internal/source`, `internal/rules`, `internal/cloud`

- Alarms kept in a cloud's own monitoring. Those written in PromQL are
  replayed (CloudWatch PromQL alarms, Google Cloud policies with a PromQL
  condition, Azure's Prometheus rule groups), and the others' history is
  judged through [recipes/firings](recipes/firings), one script a cloud;
  Datadog's monitors and New Relic's alert conditions the same way since
  0.67, from their events, neither read nor replayed.
  `lapilli serve` keeps that history, copied in a source at a time, past
  what the cloud keeps ([docs/design/recorded.md](docs/design/recorded.md)),
  and checks a PromQL alarm's replay against it (0.27), the history copied
  in standing in for `ALERTS`, CloudWatch's replay put together as the
  alarm as a whole. Still to do, every cloud alike (see
  [GOVERNANCE.md](GOVERNANCE.md)):
  - Per-series history: CloudWatch keeps a contributor's moves for an alarm
    on several series (`AlarmContributorStateUpdate`), in a form its
    documentation doesn't show; OCI keeps only the alarm's.
- Rule versions from git history are built (0.21): `--rules-history DIR`,
  written by [recipes/versions](recipes/versions) with `RENDER` for rules
  rendered from a chart, and the chart's `serve.rulesHistory`
  ([docs/design/history.md](docs/design/history.md)); the time a change was
  deployed, from Argo CD's or Flux's history, since 0.27 (`DEPLOYS`). Still
  to do: deploys in the chart's `serve.rulesHistory`, whose image has
  neither `curl` nor `jq`.

## Clouds, checked for real

`internal/cloud`, `test/e2e`

`--auth aws|azure|google` signs requests to Amazon Managed Service for
Prometheus, CloudWatch's PromQL API, Azure Monitor and Google Cloud Managed
Service for Prometheus.

- The Google path has been run against a real project.
- Thanos 0.42.5 (a sidecar and its query) and VictoriaMetrics 1.156.0,
  with vmalert 1.156.0: the CI job `e2e-compat` runs `lapilli verdict`
  and `--check-replay` against them, beside the Prometheus they read or
  are written by, and `--loaded-rules` on vmalert's rules. Mimir 3.2.4
  runs in `e2e-changes`.
- AWS and Azure are tested against SigV4 and token checks written apart from
  the SDKs, but not yet against a real workspace.
- CloudWatch PromQL alarms and Google Cloud PromQL policies are read from
  their documented shapes, and the firing recipes are tested against the
  clouds' documented answers with stand-ins for the CLIs; none of them yet
  against a real account. Two things the documentation doesn't show are
  guessed both ways: the key of the Azure CLI's skip token, and how the OCI
  CLI pages alarm history. Datadog's and New Relic's recipes, too, are
  tested against their documentation's answers, not real accounts.

A contributor with an account could run `lapilli verdict --check-replay`
there, report what differs, and keep a check that does it. It also needs
finding out which of these keep a series of what really fired: `ALERTS` is
documented for Amazon Managed Service for Prometheus, and not for the others
(vmalert writes it, and Thanos serves its Prometheus's, in `e2e-compat`).

## Pull request backtests

`internal/verdict` (`--markdown`), `recipes/backtest`

A pull request that changes alert rules gets its backtest as a comment, on
GitHub, GitLab, Bitbucket, Azure DevOps and Gitea alike, and through an
action on GitHub. The Markdown is checked by its structure in the tests,
and as Gitea renders it against a real Gitea in CI; GitHub's rendering was
checked by hand. A job can backtest through `lapilli serve` instead, with
no metric credential, judged by serve's record, by the ID token of GitHub
Actions, GitLab CI, Forgejo Actions or Bitbucket Pipelines, or a plain
token, which Azure Pipelines keeps; Grafana-managed and Loki rules too,
on the data sources serve's own rules read (0.63)
([docs/design/backtests.md](docs/design/backtests.md)). The rules changed
can be held to what is expected of them (`--expect`, 0.47): how often each
may fire, and the incidents it must keep catching
([docs/design/expectations.md](docs/design/expectations.md)). A job can
stop a merge: it fails when the change leaves an incident a rule caught
before it caught by no rule (`FAIL_MISSED`, `--fail-missed`) or breaks an
expectation, and sets a commit status on the pull request's head commit
(`STATUS`): commit statuses on every host (Bitbucket's build statuses,
Azure DevOps' pull request statuses), not GitHub check runs, for a branch
rule to require, pending while the job runs, so that a job that dies
never leaves a success. Through serve too since 0.70, which judges the
rules left alone by its record, compared as parsed PromQL since 0.71; and
in GitHub's merge queue (`merge_group`), on the merge group's head (0.71),
its report in the step's summary and a group built again taking the place
of the one waiting (0.72). Still to do:

- Through serve, a rule left alone that serve doesn't run: an incident
  only it may catch is not known, where it could be replayed. And a rule
  serve reads from an API that leaves out what its file says (a group's
  `query_offset`, which Prometheus's rules API doesn't report); and a Loki
  ruler's, a Grafana rule and a cloud alarm, still compared by their text.
- The recipes checked against real GitHub, GitLab, Bitbucket and Azure
  DevOps accounts: how each renders the comment, and takes the status,
  pending then final (GitLab's `running` set again over a job left
  running, which its reference doesn't say). Today each is tested against
  its API's documented answers, with a server standing in; Gitea against
  a real one. GitLab's merged results pipelines are tested (0.70), and its
  merge train pipelines (0.71), as its merge request pipelines' reference
  describes them: a merge of the source head, compared with its first
  parent; another commit is refused. GitHub's merge queue is tested
  against its event as its webhook reference writes it.

## Incidents in

`internal/incident`, `internal/serve` (`POST /api/v1/incidents`)

Incidents come from a YAML file, from `--impact` windows, from the UI and API,
and through the [recipes](recipes/incidents) for PagerDuty, incident.io, Jira,
GitHub issues, Grafana IRM, Rootly, FireHydrant and Compass's alerts. No tool
is built in and none is favoured.

- Signals of user impact, named, each with its own `for`, match and
  record (0.53 in verdict, 0.55 in serve, on the Settings page since
  0.58); a lead and `require:` of a signal's or a rule entry's own, and a
  person's verdict on one firing, kept apart from Lapilli's (0.63)
  ([docs/design/signals.md](docs/design/signals.md),
  [docs/design/judging.md](docs/design/judging.md)). `lapilli impact
  suggest` (0.67) says which stacks' metrics the history has, and the
  `--impact` to give ([docs/design/suggest.md](docs/design/suggest.md)).
  `lapilli serve` says a firing came after its incident ended, as verdict
  does, since 0.70. A tenant of the history's own (`--prometheus-tenant`),
  for a Mimir or a Cortex with no gateway, since 0.71.

- Recipes checked against real accounts. Today each is tested against its
  API's published schema, with a server standing in.
- Compass keeps alerts, not incidents; its recipe reads a closed alert's
  log for its close since 0.27. Compass doesn't document a close's words:
  checking them against a real account.
- The alerts an incident's tool linked to it judge it (0.67,
  [docs/design/links.md](docs/design/links.md)): the PagerDuty, incident.io
  and Rootly recipes copy them where an alert carries its `alertname` and
  labels, FireHydrant's and Jira Service Management's since 0.68, and near
  misses are looked for by the link too (0.68). Grafana IRM's APIs don't
  say which alerts an incident came from (links.md, "Grafana IRM"): its
  linked alerts are read once they do; its incidents are read by
  `QueryIncidentPreviews` since 0.71. `--linked-only` is a setting an admin
  changes on the Settings page since 0.72.

## lapilli serve

`internal/serve`, `internal/ledger`

- More languages: Spanish and Japanese are built (0.28), beside English and
  Korean, for the UI and what `--lang` posts. They were written with a
  machine's help and want a native speaker's review. Each language is one
  JSON file, `internal/serve/i18n/LANG.json`, and a test fails until every
  text has a translation: more are welcome.
- Tasks kept in step with a tracker (0.22): a tasks token on the incidents
  port, and `recipes/trackers` for GitHub, GitLab, Jira and Linear
  ([docs/design/trackers.md](docs/design/trackers.md)); Azure Boards and
  Shortcut, and comments kept in step both ways, since 0.27
  ([docs/design/inputs.md](docs/design/inputs.md)); YouTrack and Jira Data
  Center, and comments' edits followed both ways, since 0.70. Still to do:
  deletions followed; checking the recipes against real accounts.
- Roles kept in the ledger, and a team's own editors (0.25,
  [docs/design/serveops.md](docs/design/serveops.md)): admins give an
  editor's role, of every team or of one; the flags stay the floor, and
  the UI never makes an admin or an approver. A team's editors give no
  role and write no incident, as decided: an incident judges every team's
  firings.
- Quiet windows that repeat every month or by a cron expression, and a
  team's quiet windows (0.25, serveops.md).
- Declared configuration (`--config`, 0.44): teams, windows, goals,
  reports and roles in one file, kept to as it changes; routes of who is
  told what, a team as the boundary of change, the Sources page and the
  activity log (0.64).
- Goals (0.53; over a period, 0.57) and marked windows (0.53; in the UI,
  0.57) ([docs/design/goals.md](docs/design/goals.md),
  [docs/design/marked.md](docs/design/marked.md)). Still to do: goals in
  the UI; months and quarters as `each:`; weeks in a team's own zone;
  expressions for marked windows in the UI; deploys among their kinds;
  budgets inside a kind.
- `lapilli preflight` (0.53; what each identity may write, 0.60; the
  backtest lane, 0.62) says what serve will meet before it is deployed,
  and writes nothing ([docs/design/preflight.md](docs/design/preflight.md)).
  Each rule's expression asked once, behind `--queries` (0.72). Still to
  do: the same checks in serve, on the Settings page, once a day.
- Agents over MCP (0.13): they read what the pages show, backtest a change
  they propose, keep it as a proposal, and since 0.65 draft and test
  expectations ([docs/mcp.md](docs/mcp.md)); they never approve or apply.
- Reports people schedule themselves (0.22): daily, weekly, monthly or by a
  cron expression, for everyone or a team, to destinations named in files,
  with the sections they pick
  ([docs/design/reports.md](docs/design/reports.md)); the weekly digest set
  among them, in the UI and the API, the flags defining it until an admin
  does (0.25, serveops.md).
- What an alert says is checked (0.22): summaries, descriptions, runbook
  links, templates and severities, from every source's own fields, beside
  what the ledger measured of each rule, in `serve` and
  `lapilli verdict --check-content`
  ([docs/design/content.md](docs/design/content.md)); the range check by
  each job's own scrape interval, and Grafana's notification templates
  checked beside each rule (0.25, serveops.md); Alertmanager's templates,
  which a ruler's rules are sent with, from its configuration or Mimir's
  and Cortex's, and a range over a selector that names no job by the jobs
  its metric comes from (0.70); `--alertmanager-config` in the chart and in
  `lapilli preflight` (0.71); Grafana's rules sent to an outside
  Alertmanager, checked against its configuration (0.72).
- An accessibility review of the UI and its charts, to WCAG 2.2 AA, is
  built (0.28): axe-core over every page in a browser, and tests that hold
  each page's structure, its contrast and its charts' numbers in tables
  ([docs/design/access.md](docs/design/access.md)). Still to do: a review
  by people who use a screen reader day to day.

- The ledger in Postgres (0.34, [docs/design/postgres.md](docs/design/postgres.md)):
  `--ledger-url-file`, the same tables and statements as SQLite's, and
  `lapilli ledger copy`; and several replicas on it (0.36,
  [docs/design/replicas.md](docs/design/replicas.md)): a leader on a lease
  for what runs once, the caches told of other replicas' writes, each
  write's turn across them, notices claimed one at a time, the rate limits
  shared, and the chart's `serve.replicas`. Still to do: fewer round trips
  to the database where a page or a write still asks a row at a time
  (each request now asks one more, for the others' writes); the kind e2e
  of three replicas (`e2e-replicas`), which runs on main, made a required
  check once its rolling restart has passed on main since 0.70, which
  fixed what failed it, and its setup's downloads are retried or cached;
  and checking managed services
  (RDS, Cloud SQL, Azure Database) for real, a failover among them.

The performance budgets are part of the tests (`TestScale`,
`TestPagesScale`, `TestManyAtOnceScale` and the others), as counts, not
times: the statements a page or a write sends the ledger, the rows it
writes and the tables it reads, what a read allocates, and the work a
parser or router does. So a change that makes the ledger or a reader do
more fails CI on any runner, however slow; on Postgres too, by the
statements sent, in CI's job for it. The tests that keep to their own
ledger, files and stand-ins run in parallel, serve's (0.71) and the
command's (`cmd/lapilli/parallel_test.go`, 0.72).

## Packaging and trust

- **A listing on [Artifact Hub](https://artifacthub.io).** The chart is
  ready; the listing needs a repository registered there.
- Provenance as a release asset is built (0.28): the attestations'
  Sigstore bundles go with each release as `lapilli-vX.Y.Z.intoto.jsonl`,
  where OpenSSF Scorecard looks, and check offline (SECURITY.md).
- The [OpenSSF Best Practices](https://www.bestpractices.dev/projects/16891)
  badge, passing, is built (0.60); [SECURITY.md](SECURITY.md) says how.
- **A Homebrew tap.** It needs a public repository of its own
  (`homebrew-tap`), which the maintainer decides on; it waits for that.

## Changing rules

[docs/design/changes.md](docs/design/changes.md): proposals made from what
Lapilli finds, each with its backtest, approved by a person, and applied as a
pull request to where the rule is kept (by a recipe, for GitHub, GitLab,
Bitbucket, Gitea and Azure DevOps alike) or, for a rule kept nowhere else,
through the API that holds it. Built in steps, every source alike within
each:

1. Proposals, the git path and Grafana: built (0.22).
2. The managed Prometheus rulers: Mimir and Cortex, Amazon Managed Service
   for Prometheus, Azure's rule groups, through their APIs; Google Cloud's
   rule resources, Kubernetes resources, by the git path
   ([docs/design/rulers.md](docs/design/rulers.md)): built (0.23).
   Mimir's runs against a real Mimir in CI; Amazon's and Azure's against
   their documented answers only. Still to do, with an account: what an
   Amazon namespace's `data` reads as after `UPDATE_FAILED`, whether IAM
   grants `aps:PutRuleGroupsNamespace` on one namespace's ARN, whether
   Azure's `PUT` honours `If-Match` for a rule group, and what it adds to
   one it is given.
3. A cloud's own alarms: CloudWatch, Azure Monitor, Google Cloud Monitoring
   and OCI, read through each cloud's API and written there; a `for` and a
   threshold only where the alarm is PromQL and can be backtested; one
   that isn't retired as far as off; turning off and deleting waiting
   while a rule fires, on every path
   ([docs/design/alarms.md](docs/design/alarms.md)): built (0.24),
   against each cloud's documented answers only. Still to do, with an
   account: that CloudWatch answers `DescribeAlarms` and `PutMetricAlarm`
   in JSON 1.0 as its model says, which `PendingPeriod`s a PromQL alarm
   takes, whether CloudFormation's tags reach an alarm it makes, and that
   IAM allows the writes on alarm ARNs by prefix; whether Azure honours
   `If-Match` for a log search alert, and whether a PromQL metric alert's
   `scopes` is the workspace `--prometheus` queries; whether a Google or
   OCI alarm off is still evaluated; that OKE's workload identity reads
   alarms.
4. A new rule, proposed held to its expectations: built (0.65), with a
   team as the boundary of change, its approvers, where its rules are
   written and its freezes, in `--config` (0.64); to Loki's ruler and its
   rule files too (0.72). Not offered where the design says why: Azure's
   rule groups and alert rules, OCI's alarms, a new Grafana group, a new
   rule file.

## Not planned

- **Changes without a person's approval.** Lapilli proposes changes to
  rules and applies one only once a person has approved it
  ([docs/design/changes.md](docs/design/changes.md)); an agent proposes,
  never applies.
- **Silencing alerts, or writing anything but alert rules and what they
  are held to.** No silences, no metrics written, nothing changed beyond
  the rules, and the expectations file, it was told it may change.
- **A Kubernetes controller.** `lapilli serve` reads files and APIs; it
  doesn't watch cluster resources, and changes nothing in a cluster: a rule
  resource is changed by a pull request.
- **Features for one cloud that the others can have but don't.**
- **A model of its own.** Lapilli measures; people and their agents
  decide. It speaks MCP so that an agent can ask it, and it sends nothing
  to one.
