# Roadmap

Lapilli answers one question: which of your alerts are worth their page?
`lapilli verdict` answers it once, for a period; `lapilli serve` keeps
answering it, every hour, in a ledger with a dashboard. What follows is what
it doesn't do yet, area by area, with where it lives in the code. Each area
is a piece someone can take on and come to own; see
[MAINTAINERS.md](MAINTAINERS.md) for how owning an area works.

Some areas need what the current maintainer doesn't have: an AWS or Azure
account with a managed Prometheus, Loki alerts in real use, an incident
tool. If you do, you are the person this needs.

## Replay fidelity

`internal/rules`, `internal/sse`, `internal/replay`

A replay is only as useful as it is faithful: a rule that fired differently
from how it really did is the worst kind of wrong.

Built (0.25), measured against a real Grafana 13.3.1 and Prometheus 3.5.0
and kept as test data ([docs/design/states.md](docs/design/states.md)):
Grafana's no data states and missing series, classic conditions with every
reducer, `query_offset`, a Grafana window ending before now, `--late`, and
groups placed where Prometheus places them, for rules read from its API.
Still to do:

- Query errors. History can't say when a query failed, so a rule's error
  states aren't replayed: Grafana's error alerts are left out of what really
  fired, and `--check-replay` says how many; `serve` keeps no count of them.
- A query's history before the day before a period: a query that has
  returned nothing for longer is taken to have no history, not no data.
- Rules written in LogQL are built (0.28): a Loki ruler's, from its files
  or its API, and Grafana's on a Loki data source, through Grafana's data
  source proxy, measured against Loki 3.7.9 and Grafana 13.3.1
  ([docs/design/loki.md](docs/design/loki.md)). Still to do: a Grafana rule
  reading Prometheus with a Loki query beside its condition, which is
  replayed in part (the Loki query could be asked); classic conditions over
  Loki; a Loki group's fraction of a second; Loki's values in the chart
  (today through `extraArgs`); and changing a Loki ruler's rules through
  its API, as Mimir's are. SQL and the other data sources stay out: a
  database keeps no history of what a query returned (the design says why);
  their rules are listed, by the type Grafana gives.
- Metrics whose lateness varies. `--late` is one delay; a gap the live rule
  saw, filled in a minute later, isn't in history at all.
- Where Prometheus places a group read from a file: the hash takes the path
  Prometheus loaded it from. `serve` could take it from the history's own
  rules API, by the group's name.
- How long before a period Grafana's alerts are worked out from: an hour, for
  `noDataState` other than `NoData`.

## What really fired

`internal/verdict`, `cmd/lapilli`

`--check-replay` sets a replay beside what really fired, once, when asked.

- The same in `lapilli serve`, kept in the ledger, so a rule starting to differ
  shows on its page: built for v0.21
  ([docs/design/fidelity.md](docs/design/fidelity.md)). Still to do: the
  threshold a rule differs at, set from what real ledgers show.
- Notifications as they were sent are built (0.20): Alertmanager's and
  Grafana's webhooks, SNS for Amazon Managed Service for Prometheus, Azure
  Monitor's action groups and Google Cloud's webhook channels, each on a path
  of its own ([docs/design/notifications.md](docs/design/notifications.md)).
  Alertmanager's event recorder, every delivery, PagerDuty's and Slack's
  too, is taken since 0.27 ([docs/design/inputs.md](docs/design/inputs.md));
  still to do: keeping its mutes, which say why a firing paged no one;
  checking the SNS, Azure and Google Cloud paths against real accounts; and a cloud alarm's own notifications,
  every cloud alike (CloudWatch through SNS, Azure Monitor's other alerts,
  Google Cloud's other policies, OCI through Notifications), tied to the
  firings copied in: accepted for v0.21 in
  [docs/design/notifications.md](docs/design/notifications.md#a-clouds-own-alarms-notifications-v021).

## What to change

`internal/source`, `internal/verdict`

A noisy rule gets a longer `for` that would quiet it; a rule that nearly caught
an incident nothing caught gets the shorter `for`, or the number short of its
threshold, that would have.

- Near misses by value for a Grafana math expression joining comparisons
  with `&&` or `||`, and for classic conditions, are built (0.25). A
  comparison under `!`, or inside arithmetic, is still read only as a whole.
- Shifts read from where on-call schedules are kept: built for 0.27
  (`--on-call`, iCalendar feeds). Still to do: monthly and yearly repeats,
  and a feed's own `VTIMEZONE`; checking each tool's feed against a real
  account.

## Where rules and alarms come from

`internal/source`, `internal/rules`, `internal/cloud`

- Alarms kept in a cloud's own monitoring. Those written in PromQL are
  replayed (CloudWatch PromQL alarms, Google Cloud policies with a PromQL
  condition, Azure's Prometheus rule groups), and the others' history is
  judged through [recipes/firings](recipes/firings), one script a cloud.
  `lapilli serve` keeps that history, copied in a source at a time, past
  what the cloud keeps ([docs/design/recorded.md](docs/design/recorded.md)).
  Still to do, every cloud alike (see [GOVERNANCE.md](GOVERNANCE.md)):
  - Checking a cloud alarm's replay against its history: built for 0.27,
    the history copied in standing in for `ALERTS`, CloudWatch's replay
    put together as the alarm as a whole.
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
- AWS and Azure are tested against SigV4 and token checks written apart from
  the SDKs, but not yet against a real workspace.
- CloudWatch PromQL alarms and Google Cloud PromQL policies are read from
  their documented shapes, and the firing recipes are tested against the
  clouds' documented answers with stand-ins for the CLIs; none of them yet
  against a real account. Two things the documentation doesn't show are
  guessed both ways: the key of the Azure CLI's skip token, and how the OCI
  CLI pages alarm history.

A contributor with an account could run `lapilli verdict --check-replay`
there, report what differs, and keep a check that does it. It also needs
finding out which of these keep a series of what really fired: `ALERTS` is
documented for Amazon Managed Service for Prometheus, and not for the others.

## Incidents in

`internal/incident`, `internal/serve` (`POST /api/v1/incidents`)

Incidents come from a YAML file, from `--impact` windows, from the UI and API,
and through the [recipes](recipes/incidents) for PagerDuty, incident.io, Jira,
GitHub issues, Grafana IRM, Rootly, FireHydrant and Compass's alerts. No tool
is built in and none is favoured.

- Recipes checked against real accounts. Today each is tested against its
  API's published schema, with a server standing in.
- Compass keeps alerts, not incidents; its recipe reads a closed alert's
  log for its close since 0.27. Compass doesn't document a close's words:
  checking them against a real account.

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
  ([docs/design/inputs.md](docs/design/inputs.md)). Still to do: edits
  followed; YouTrack, Jira Data Center; checking the recipes against real
  accounts.
- Roles kept in the ledger, and a team's own editors (built,
  [docs/design/serveops.md](docs/design/serveops.md)): admins give an
  editor's role, of every team or of one; the flags stay the floor, and
  the UI never makes an admin or an approver. Still to do: whether a team's
  editors give their team's role, or write incidents only their rules
  caught.
- Quiet windows that repeat every month or by a cron expression, and a
  team's quiet windows (built, serveops.md).
- Reports people schedule themselves (0.22): daily, weekly, monthly or by a
  cron expression, for everyone or a team, to destinations named in files,
  with the sections they pick
  ([docs/design/reports.md](docs/design/reports.md)); the weekly digest set
  among them, in the UI and the API, the flags defining it until an admin
  does (built, serveops.md).
- What an alert says is checked (0.22): summaries, descriptions, runbook
  links, templates and severities, from every source's own fields, beside
  what the ledger measured of each rule, in `serve` and
  `lapilli verdict --check-content`
  ([docs/design/content.md](docs/design/content.md)); the range check by
  each job's own scrape interval, and Grafana's notification templates
  checked beside each rule (built, serveops.md). Still to do: the same for
  Alertmanager's templates, which a ruler's rules are sent with, read from
  Alertmanager's or a managed service's configuration; and a range over a
  selector that names no job, by the jobs its metric comes from.
- An accessibility review of the UI and its charts, to WCAG 2.2 AA, is
  built (0.28): axe-core over every page in a browser, and tests that hold
  each page's structure, its contrast and its charts' numbers in tables
  ([docs/design/access.md](docs/design/access.md)). Still to do: a review
  by people who use a screen reader day to day, and the heatmap's long
  rule names, cut short.

The performance budgets are part of the tests (`TestScale`,
`TestPagesScale`), so a change that slows the ledger fails CI.

## Packaging and trust

- **A listing on [Artifact Hub](https://artifacthub.io).** The chart is
  ready; the listing needs a repository registered there.
- Provenance as a release asset is built (0.28): the attestations'
  Sigstore bundles go with each release as `lapilli-vX.Y.Z.intoto.jsonl`,
  where OpenSSF Scorecard looks, and check offline (SECURITY.md).
- **The [OpenSSF Best Practices](https://www.bestpractices.dev) badge.**
- **A Homebrew tap.** It needs a public repository of its own
  (`homebrew-tap`), which the maintainer decides on; it waits for that.

## Changing rules

[docs/design/changes.md](docs/design/changes.md): proposals made from what
Lapilli finds, each with its backtest, approved by a person, and applied as a
pull request to where the rule is kept (by a recipe, for GitHub, GitLab,
Bitbucket, Gitea and Azure DevOps alike) or, for a rule kept nowhere else,
through the API that holds it. Built in steps, every source alike within
each:

1. Proposals, the git path and Grafana: built for v0.22.
2. The managed Prometheus rulers: Mimir and Cortex, Amazon Managed Service
   for Prometheus, Azure's rule groups, through their APIs; Google Cloud's
   rule resources, Kubernetes resources, by the git path
   ([docs/design/rulers.md](docs/design/rulers.md)): built for v0.23.
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
   ([docs/design/alarms.md](docs/design/alarms.md)): built for v0.24,
   against each cloud's documented answers only. Still to do, with an
   account: that CloudWatch answers `DescribeAlarms` and `PutMetricAlarm`
   in JSON 1.0 as its model says, which `PendingPeriod`s a PromQL alarm
   takes, whether CloudFormation's tags reach an alarm it makes, and that
   IAM allows the writes on alarm ARNs by prefix; whether Azure honours
   `If-Match` for a log search alert, and whether a PromQL metric alert's
   `scopes` is the workspace `--prometheus` queries; whether a Google or
   OCI alarm off is still evaluated; that OKE's workload identity reads
   alarms.

## Not planned

- **Changes without a person's approval.** Lapilli proposes changes to
  rules and applies one only once a person has approved it
  ([docs/design/changes.md](docs/design/changes.md)); an agent proposes,
  never applies.
- **Silencing alerts, or writing anything but alert rules.** No silences,
  no metrics written, nothing changed in a cluster beyond the rules it was
  told it may change.
- **A Kubernetes controller.** `lapilli serve` reads files and APIs; it
  doesn't watch cluster resources, and changes nothing in a cluster: a rule
  resource is changed by a pull request.
- **Features for one cloud that the others can have but don't.**
- **A model of its own.** Lapilli measures; people and their agents
  decide. It speaks MCP so that an agent can ask it, and it sends nothing
  to one.
