# Roadmap

Lapilli answers one question: which of your alerts are worth their page?
`lapilli verdict` answers it once, for a period; `lapilli serve` keeps
answering it, every hour, in a ledger with a dashboard. What follows is what
it doesn't do yet, area by area, with where it lives in the code. Each area
is a piece someone can take on and come to own; see
[MAINTAINERS.md](MAINTAINERS.md) for how owning an area works.

Some areas need what the current maintainer doesn't have: an AWS or Azure
account with a managed Prometheus, a Grafana with Loki alerts, an incident
tool. If you do, you are the person this needs.

## Replay fidelity

`internal/rules`, `internal/sse`, `internal/replay`

A replay is only as useful as it is faithful: a rule that fired differently
from how it really did is the worst kind of wrong.

- No-data and error states. Lapilli replays a rule's condition, not what
  Grafana does when a query returns nothing or fails.
- Datasources that don't speak PromQL: Loki (LogQL), SQL. Such rules are
  listed with the reason today.
- Classic conditions whose reducer has no PromQL match: `diff`,
  `percent_diff`, `no_value`.
- Metrics that arrive late, as a cloud's own metrics of its services often do.
  The live rule saw a gap and resolved; the replay sees the filled-in history
  and doesn't.
- Where Prometheus places a group within its interval. A replay can't know
  this, so a firing can start up to one interval sooner or later.

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
  Still to do: Alertmanager's event recorder (0.33.0), which sees every
  delivery, PagerDuty's and Slack's too; checking the SNS, Azure and Google
  Cloud paths against real accounts; and a cloud alarm's own notifications,
  every cloud alike (CloudWatch through SNS, Azure Monitor's other alerts,
  Google Cloud's other policies, OCI through Notifications), tied to the
  firings copied in: accepted for v0.21 in
  [docs/design/notifications.md](docs/design/notifications.md#a-clouds-own-alarms-notifications-v021).

## What to change

`internal/source`, `internal/verdict`

A noisy rule gets a longer `for` that would quiet it; a rule that nearly caught
an incident nothing caught gets the shorter `for`, or the number short of its
threshold, that would have.

- Near misses by value for the conditions still read only as a whole: a
  Grafana math expression joining comparisons with `&&` or `||`, and classic
  conditions.
- Shifts read from where on-call schedules are kept, rather than written down
  again: many on-call tools export a schedule as an iCalendar feed. A turn
  there names a person, not hours, so this goes with knowing whose hours
  those are.

## Where rules and alarms come from

`internal/source`, `internal/rules`, `internal/cloud`

- Alarms kept in a cloud's own monitoring. Those written in PromQL are
  replayed (CloudWatch PromQL alarms, Google Cloud policies with a PromQL
  condition, Azure's Prometheus rule groups), and the others' history is
  judged through [recipes/firings](recipes/firings), one script a cloud.
  `lapilli serve` keeps that history, copied in a source at a time, past
  what the cloud keeps ([docs/design/recorded.md](docs/design/recorded.md)).
  Still to do, every cloud alike (see [GOVERNANCE.md](GOVERNANCE.md)):
  - Checking a cloud alarm's replay against its history: `--check-replay`
    reads a series of what fired, and these alarms keep none. Their
    history, from the recipes, could stand in; CloudWatch's and OCI's are of
    the alarm as a whole, so the replay's firings would first be put
    together the same way.
  - Per-series history: CloudWatch keeps a contributor's moves for an alarm
    on several series (`AlarmContributorStateUpdate`), in a form its
    documentation doesn't show; OCI keeps only the alarm's.
- Rule versions from git history are built (0.21): `--rules-history DIR`,
  written by [recipes/versions](recipes/versions) with `RENDER` for rules
  rendered from a chart, and the chart's `serve.rulesHistory`
  ([docs/design/history.md](docs/design/history.md)). Still to do: the time
  a change was deployed rather than committed, from a deployment tool's
  history (Argo CD's, Flux's).

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
- Compass keeps alerts, not incidents, and doesn't say when one was closed:
  its recipe ends a closed alert at its last update. Its alert log
  (`/v1/alerts/{id}/logs`) has the close, at a request an alert.

## lapilli serve

`internal/serve`, `internal/ledger`

- **More languages.** Each is one JSON file, `internal/serve/i18n/LANG.json`,
  and a test fails until every text has a translation.
- Tasks kept in step with a tracker (0.22): a tasks token on the incidents
  port, and `recipes/trackers` for GitHub, GitLab, Jira and Linear
  ([docs/design/trackers.md](docs/design/trackers.md)). Still to do: more
  trackers, and comments kept in step.
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
- **An accessibility review** of the UI, and of the charts beyond colour.
  Their colours are already checked for colour blindness.

The performance budgets are part of the tests (`TestScale`,
`TestPagesScale`), so a change that slows the ledger fails CI.

## Packaging and trust

- **A listing on [Artifact Hub](https://artifacthub.io).** The chart is
  ready; the listing needs a repository registered there.
- **Provenance as a release asset.** Today it lives in GitHub's attestation
  store, where OpenSSF Scorecard doesn't look.
- **The [OpenSSF Best Practices](https://www.bestpractices.dev) badge.**
- **A Homebrew tap.**

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
