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
  shows on its page.
- Notifications as they were sent, from Alertmanager's webhook (Prometheus,
  Mimir, Google Cloud's managed Alertmanager) and Grafana's. Lapilli counts
  firings; these would say which paged, whom, and how often. Amazon Managed
  Service for Prometheus sends to SNS and PagerDuty, and Azure Monitor to action
  groups, so each needs a way in of its own. The design is accepted:
  [docs/design/notifications.md](docs/design/notifications.md).

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
  Still to do, every cloud alike (see [GOVERNANCE.md](GOVERNANCE.md)):
  - `lapilli serve` taking firings recorded elsewhere, so the ledger keeps
    more than the cloud does (30 days on AWS and Azure). It is a new kind of
    data in the ledger, so a design comes first.
  - Checking a cloud alarm's replay against its history: `--check-replay`
    reads a series of what fired, and these alarms keep none. Their
    history, from the recipes, could stand in; CloudWatch's and OCI's are of
    the alarm as a whole, so the replay's firings would first be put
    together the same way.
  - Per-series history: CloudWatch keeps a contributor's moves for an alarm
    on several series (`AlarmContributorStateUpdate`), in a form its
    documentation doesn't show; OCI keeps only the alarm's.
- Rule versions from git history, so a first `lapilli serve` start knows how
  the rules changed before it ran. Helm-rendered rules need the chart
  rendered at each commit.

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
and through the [recipes](recipes/incidents) for PagerDuty, incident.io, Jira
and GitHub issues. No tool is built in and none is favoured.

- Recipes checked against real accounts. Today each is tested against its
  API's published schema, with a server standing in.
- More recipes: Compass, where Opsgenie's users are moving, as Opsgenie's
  support ends on 5 April 2027; Grafana IRM, Rootly, FireHydrant.

## lapilli serve

`internal/serve`, `internal/ledger`

- **More languages.** Each is one JSON file, `internal/serve/i18n/LANG.json`,
  and a test fails until every text has a translation.
- **Tracker recipes.** Turn the webhook into an issue in GitHub, GitLab, Jira
  or Linear, and put its link back with `PATCH /api/v1/tasks/{id}`.
- **Roles kept in the ledger.** Today the sign-in proxy names who is who.
  Teams could have their own editors.
- **Quiet windows that repeat by month, or by a cron expression.**
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

## Not planned

- **Writing anywhere.** Lapilli only reads: it never changes a rule,
  silences an alert or writes to Prometheus, Grafana or a cluster.
  Suggestions are reports, and tasks for people.
- **A Kubernetes controller.** `lapilli serve` reads files and APIs; it
  doesn't watch or change cluster resources.
- **Features for one cloud that the others can have but don't.**
- **A model of its own.** Lapilli measures; people and their agents
  decide. It speaks MCP so that an agent can ask it, and it sends nothing
  to one.
