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

## Where rules and alarms come from

`internal/source`, `internal/rules`, `internal/cloud`

- Alarms kept in a cloud's own monitoring: Amazon CloudWatch alarms, Azure
  Monitor alert rules, Google Cloud alerting policies, OCI Monitoring alarms.
  Lapilli reads rule files and the PromQL APIs. These four have their own
  rule shapes and their own histories of what fired. One cloud alone isn't
  enough here: the project keeps the clouds at parity (see
  [GOVERNANCE.md](GOVERNANCE.md)).
- Rule versions from git history, so a first `lapilli serve` start knows how
  the rules changed before it ran. Helm-rendered rules need the chart
  rendered at each commit.

## Clouds, checked for real

`internal/cloud`, `test/e2e`

`--auth aws|azure|google` signs requests to Amazon Managed Service for
Prometheus, Azure Monitor and Google Cloud Managed Service for Prometheus.

- The Google path has been run against a real project.
- AWS and Azure are tested against SigV4 and token checks written apart from
  the SDKs, but not yet against a real workspace.

A contributor with an account could run a replay there, report what differs,
and keep a check that does it.

## Incidents in

`internal/incident`, `internal/serve` (`POST /api/v1/incidents`)

Incidents come from a YAML file, from `--impact` windows, and from the UI and
API.

- Recipes, and where needed small adapters, for the tools teams record
  incidents in: PagerDuty, Opsgenie, incident.io, Jira, GitHub issues. They
  should go through the API, so no tool is built in and none is favoured.

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
- **Fuzzing for the rule parsers and the Grafana expression engine.** They
  read files written by others.
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
