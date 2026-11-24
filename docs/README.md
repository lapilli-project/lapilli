# Lapilli docs

How Lapilli judges alert rules, and how to run it: once over a period with
`lapilli verdict`, or every hour with `lapilli serve`. The same pages are at
[lapilli.dev/docs](https://lapilli.dev/docs/) and in the
[wiki](https://github.com/lapilli-project/lapilli/wiki), a copy of
docs/ written again on each change; the [README](../README.md) says what
Lapilli is, and how to install it.

## Start here

- [Getting started](getting-started.md): a first report on sample data
  with `lapilli demo`; then install it, write down an incident or two, and
  read a first report on your own alerts; then keep judging with
  `lapilli serve`
- [Words](glossary.md): stones, firings, incidents, the ledger, and the
  rest of the words these pages use
- [Questions](faq.md): what it needs, what it writes, and what it sends
  anywhere
- [What it reads, keeps and sends](data.md): what each command reads, what
  `serve` keeps and for how long, who can reach it, what it sends where, and
  what it can change

## Judging a period

- [How it judges](how-it-judges.md): where firings and incidents come from,
  when a firing catches an incident, noisy rules and what to change, what a
  rule adds, when firings came, near misses, rules that changed
- [Use `lapilli verdict`](verdict.md): the commands, the reports it writes,
  the data sources it reads and how fast, and when it fails
- [The numbers](numbers.md): what each number is, and how to count it again
  from the report, without Lapilli
- [A cloud's own alarms](cloud-alarms.md): judging what CloudWatch, Azure
  Monitor, Google Cloud and OCI alarms fired, and Datadog's monitors and
  New Relic's alert conditions
- [Rules on logs: Loki](loki.md): LogQL rules replayed against Loki
- [Is the replay right?](check-replay.md): `--check-replay`, the replay
  beside what really fired
- [What an alert says](check-content.md): `--check-content`, what each rule
  says when it fires, and how it is written

## Before a change

- [Before a change is merged](compare.md): `--compare`, the rules proposed
  beside the rules in place, and what a rule is expected to do (`--expect`)
- [Pull request backtests in CI](pr-backtests.md): the backtest as a comment
  on each pull request that changes alert rules, and a commit status that
  can stop the merge

## Incidents

- [Incidents kept elsewhere](incidents.md): the incident log copied from
  where it is kept, and the alerts its tool linked to each
- [Incidents nobody wrote down](impact.md): `--impact`, incidents found in a
  signal of user impact, and `lapilli impact suggest`, which says which
  signal your history has

## Reports

- [Every week](every-week.md): the Helm chart's weekly report, to Slack and a
  Pushgateway
- [Showing a report outside](redact.md): `--redact`, a report with every name
  given an alias

## Every hour: `lapilli serve`

- [Keep judging](serve.md): the ledger and its dashboard, teams, tasks and
  goals; checking it before it is deployed, the ledger in Postgres, several
  replicas, firings recorded elsewhere, tasks kept in step with a tracker
- [Notifications as they were sent](notifications.md): the notifications
  kept beside the firings, and why a firing paged no one
- [Changing rules](changing-rules.md): proposals, approval, and the change
  made where the rule is kept
- [Ask it from an agent](mcp.md): MCP at `/mcp`

## Reference

- [Commands](commands.md): every option of `lapilli verdict`, `serve`,
  `preflight`, `ledger`, `impact` and `demo`, as `-h` prints it
- [The HTTP API](api.md): every endpoint `lapilli serve` answers on, and the
  role each needs
- [Metrics](metrics.md): every metric `lapilli serve` exposes at `/metrics`
- [Upgrading](upgrading.md): which version changed the ledger, and going
  back

## And

- [When something goes wrong](troubleshooting.md): what an error or an odd
  number means, and what to do
- [What it doesn't do yet](limits.md): the queries, states and timings a
  replay can't reproduce yet, and what it reads only in part
- The Helm chart: [charts/lapilli](../charts/lapilli/README.md)
- Scripts that copy in incidents, firings, tasks and earlier versions of the
  rules, open a pull request for an approved change, and run backtests in
  CI: [recipes](../recipes)
- How each part was designed: [design](design)
