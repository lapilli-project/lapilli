# Design notes

Each note says what a part does, what it deliberately doesn't, what was
weighed against it, and what is left. A larger change starts as one
([GOVERNANCE.md](../../GOVERNANCE.md)). They are long; read the one for the
part you are changing. What a user needs is in [docs](../README.md), not
here.

## Judging and the replay

- [states.md](states.md): replaying what happens around a rule's condition
  (no data, missing series, `for`, a group's place in time)
- [fidelity.md](fidelity.md): the replay beside what really fired, kept
- [judging.md](judging.md): a lead per signal, episodes beside firings, a
  person's verdict on one firing, rows by label
- [signals.md](signals.md): impact signals, each named, with its own `for`,
  `match` and start
- [suggest.md](suggest.md): `lapilli impact suggest`
- [links.md](links.md): incidents judged by the alerts their tool linked
- [responses.md](responses.md): what people did when a rule fired (pages
  acknowledged or not, changes on its target), and rules found noisy by it
- [loki.md](loki.md): rules written in LogQL
- [grafanahistory.md](grafanahistory.md): what Grafana's rules really
  fired, from Grafana's own history
- [grafanajudged.md](grafanajudged.md): Grafana's rules that can't be
  replayed, judged from that history
- [history.md](history.md): rule versions from before the ledger began
- [quota.md](quota.md): sharing the history with the alerts that read it
- [content.md](content.md): what an alert says, checked
- [marked.md](marked.md): marked windows, shown and never excused
- [mutes.md](mutes.md): why a firing paged no one

## What comes in

- [inputs.md](inputs.md): deliveries, on-call schedules, deploys, a cloud
  alarm's history, trackers' comments
- [recorded.md](recorded.md): firings recorded elsewhere, in the ledger
- [notifications.md](notifications.md): notifications as they were sent

## Changing rules, and checking a change

- [changes.md](changes.md): changing rules, with a person's approval
- [rendered-changes.md](rendered-changes.md): changing rules rendered from a
  chart's template and values (proposed)
- [rulers.md](rulers.md): a managed Prometheus ruler's rules
- [alarms.md](alarms.md): a cloud's own alarms
- [backtests.md](backtests.md): pull request backtests through serve
- [expectations.md](expectations.md): what a rule was tested to do, kept
  and held to
- [goals.md](goals.md): what an operations team holds its alerting to

## `lapilli serve`

- [serve.md](serve.md): a ledger of verdicts
- [work.md](work.md): what people keep in the ledger
- [ui.md](ui.md): the web UI
- [noc.md](noc.md): what an operations team asks of its alerts
- [reports.md](reports.md): reports people schedule themselves
- [trackers.md](trackers.md): tasks kept in step with a tracker
- [signin.md](signin.md): signing in
- [serveops.md](serveops.md): serve, run by the people who use it
- [postgres.md](postgres.md): the ledger in Postgres
- [replicas.md](replicas.md): replicas on one Postgres ledger
- [preflight.md](preflight.md): `lapilli preflight`
- [access.md](access.md): more languages, an accessible UI, provenance
  beside a release

## A first look

- [demo.md](demo.md): `lapilli demo`
