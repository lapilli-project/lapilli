# Firings recorded elsewhere, in the ledger

Status: accepted, not built. Under [GOVERNANCE.md](../../GOVERNANCE.md) this
is a new kind of data in the ledger, so the design came first, as a pull
request (#32); it was merged on 2026-11-09, before its week was out, by the
maintainer's decision, with no comments. The companion to
[serve.md](serve.md), about the firings a replay finds, and to
[notifications.md](notifications.md), about what the alert manager sent.

## Why

Many teams alert from their cloud's own monitoring: Amazon CloudWatch alarms,
Azure Monitor alert rules, Google Cloud Monitoring alerting policies, OCI
Monitoring alarms. Those written in PromQL Lapilli replays (CloudWatch PromQL
alarms, Google Cloud policies with a PromQL condition, Azure's Prometheus rule
groups). Most aren't: they are written in each cloud's own terms (a
CloudWatch metric math expression, a KQL query, an MQL query), against metrics
that no PromQL API serves. What Lapilli can do with those is judge what they
fired. `recipes/firings/` writes each cloud's alarm history as `lapilli
verdict --firings` takes it, and a verdict over a week is then as good as for
any rule.

`lapilli serve` doesn't take them. So a team on cloud alarms gets a weekly
verdict at most, and none of what the ledger gives: weeks beside each other,
incidents written down later, teams, hours asleep, quiet windows, tasks, the
digest, the agent's tools. And some clouds keep their history briefly
(CloudWatch and Azure Monitor 30 days, OCI 90; Google Cloud 13 months), so
last quarter can't be judged on AWS or Azure unless someone kept it.

This note proposes that `lapilli serve` keep firings recorded elsewhere,
copied in by a job, beside the firings it replays, and judge them alike.

## What it would answer

Everything the ledger answers about a rule that doesn't need its condition:
its stones by day and week, precision, refirings, what it adds beside the
other rules (replayed or recorded), the incidents it caught and how soon,
when its firings came by each team's hours, quiet windows, tasks, comments,
the digest and the team digests, `/metrics`, and MCP's reading tools.

What needs the condition stays with replayed rules: change reviews and their
outcomes, longer `for`s, near misses, `compare`. A recorded rule's page says
so, and why: Lapilli doesn't have its condition, only what it fired.

## The way in

The port tools already copy incidents into, `--incidents-listen`, takes
firings too, on a path of its own and with a token of its own
(`--firings-token-file`), so a tool that may copy incidents can't write
firings, and the reverse:

```
PUT /api/v1/firings/{source}?from=2026-11-02T00:00:00Z&to=2026-11-09T00:00:00Z
Authorization: Bearer <token>

{"rule":"checkout-5xx","labels":{"service":"checkout"},"start":"2026-11-03T02:10:00Z","end":"2026-11-03T02:40:00Z"}
{"rule":"checkout-5xx","labels":{"service":"checkout"},"start":"2026-11-08T23:50:00Z"}
```

- **A source** is a name the job picks (`cloudwatch-prod`, `azure-shared`):
  `[a-z0-9-]`, 40 characters at most. The ledger keeps at most 50.
- **The period replaces.** The source's firings that start in `[from, to)`
  are replaced by those sent, the way a run replaces the firings that start
  in its period. Sending the same again changes nothing, tells no one and
  judges nothing again, so a job can send the last day every hour, or the
  cloud's whole history every week. A firing sent with a start outside the
  period is refused, with the request.
- **Open firings.** A firing without an end is still firing. Sent again
  with an end, in a later period that includes its start, it ends.
- **The body** is what `--firings` reads, one firing a line, and so what
  every recipe writes: `recipes/firings/put.sh` sends a recipe's output.
  A period is 31 days at most, a body 16 MB and 100,000 firings at most.

The port's rules stay as they are: one write at a time, 20 a second, the
token read for each request and hashed before it is compared, no sign-in
headers believed. What a token writes is noted as `firings-token`, which
outcomes don't count as use. The UI and API on the other port can't change
recorded firings: they are the tool's, as copied incidents are.

## In the ledger

No new kind of row for a firing: a recorded firing is a row of `firings`,
judged by the same code. What is new is where its rule comes from:

- `rules` gains `source`, empty for a replayed rule and the source's name for
  a recorded one. A recorded rule is named by the source, its name and its
  labels' severity, as replayed rules sharing a name are told apart;
  `checkout-5xx [cloudwatch-prod]` when a replayed rule has the same name.
- A recorded rule has no versions. It is there from its first firing, and
  shown as recorded.
- A migration of its own adds the column and a table `sources`: each source,
  when it was last written and by whom, and the latest period sent, for the
  status page and for `lapilli_serve_source_last_timestamp_seconds`, so an
  alert can say when a job stopped copying.

CloudWatch's and OCI's history is of an alarm as a whole, so their recorded
firings carry the alarm's labels, not a series'; Azure's and Google Cloud's
carry each alert's. The ledger takes what it is sent, as `--firings` does.

Stones are given again over the period written, as for a run, and recorded
firings take part in everything stones take part in: a rule replayed and a
rule recorded catching the same incident are compared for what each adds,
noisy or not, as any two rules are.

`--keep` drops recorded firings as it drops replayed ones, and a source
nothing has been written to for `--keep` is forgotten.

## Without rules to replay

`lapilli serve` wants `--rules` and `--prometheus` today, and runs wait for
the rules. A team with only cloud alarms has neither. With
`--firings-token-file`, both become optional: without them, runs replay
nothing, and only judge again what is copied in; `--impact` still needs
`--prometheus`. The chart gets `serve.firings.tokenSecret`.

## Parity

Each cloud's alarm history becomes firings through a recipe, so all four
work the same way: CloudWatch (`DescribeAlarmHistory`), Azure Monitor
(Azure Resource Graph's `alertsmanagementresources`), Google Cloud
(`projects.alerts.list`), OCI Monitoring (`GetAlarmHistory`). So do firings
from anywhere else that writes the same lines.

## Alternatives

- **`lapilli serve` asks each cloud itself.** No job to run, but four clouds'
  credentials, APIs and paging inside `serve`, each needing a real account to
  test. It could come later as built-in sources, once the recipes have shown
  what each cloud's history really holds.
- **A file read each run**, as `--incidents FILE` is. The file would own its
  firings, so one holding the cloud's last 30 days would take older ones out
  of the ledger every run: the opposite of what a ledger is for here.
- **The alarms' own notifications**, through the port the notifications
  design proposes (SNS for CloudWatch, an action group's webhook for Azure, a
  webhook channel for Google Cloud, OCI Notifications). They come as alarms
  change, with nothing to run, but only from when they are set up and only
  where a notification is configured, and carry no history. They fit beside
  this, not instead of it.
- **Replaying cloud alarms.** Where the condition is PromQL against a
  PromQL API, Lapilli replays it already, on each cloud that has such alarms;
  the rest would need each cloud's query language reimplemented, against
  metrics only that cloud serves.

## Building it

One release: the migration, the path, the source column through the pages,
the API, `/metrics` and MCP, `recipes/firings/put.sh`, and the chart. Tested
with the recipes' fixtures sent through the port, and a budget: a week of
100,000 firings written in under 5 s, and sent again unchanged in under 1 s.
