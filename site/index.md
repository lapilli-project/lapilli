---
layout: default
title: Which of your alerts are worth their page?
description: "Lapilli judges every firing against what actually went wrong: a white stone when it caught an incident, a black one when nothing was wrong. Once with lapilli verdict, or every hour with lapilli serve."
---

# Which of your alerts are worth their page?

<p class="lede">Lapilli judges every firing against what actually went wrong: a
white stone when the alert caught an incident, a black stone when nothing was
wrong. Count a rule's stones and you know whether it earns its place; an
incident with no stone is one your alerts missed.</p>

<div class="cta">
  <a class="primary" href="#install">Install</a>
  <a href="https://github.com/lapilli-project/lapilli#readme">Read the docs</a>
  <a href="#help-build-it">Help build it</a>
</div>

<div class="shot">
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="{{ '/serve-dashboard-dark.png' | relative_url }}">
  <img alt="The lapilli serve overview, with its pages in a sidebar: last week's precision, firings, incidents missed, time to catch, noisy rules, firings that woke people for nothing, re-fires and firings during deploys, each beside the week before, then firings by week" src="{{ '/serve-dashboard.png' | relative_url }}" width="1240" height="980">
</picture>
</div>

It works from what you already have: the rules, the history in Prometheus,
and whatever record of incidents you keep, even a thin one. It reads by
default, and changes only what a person approves.

## Once: `lapilli verdict`

Replay a week, a month, or a change you are about to merge, and get the
stones, the firings that woke people for nothing, the noisy rules with a longer
`for` that would quiet them, the incidents nothing caught, and the rules that
nearly caught them.

```
$ lapilli verdict --rules alert-rules.yaml --prometheus http://prometheus:9090 \
    --incidents incidents.yaml --impact 'sum(rate(http_5xx[5m])) / sum(rate(http_total[5m])) > 0.01'
Lapilli  2026-10-26 00:00 → 2026-11-02 00:00 Asia/Seoul
148 firings, 3 incidents written down, 1 more found by --impact

RULE                SEVERITY  FIRED  ○ CAUGHT  ● NOTHING  PRECISION  REFIRED
ExportJob5xx        critical  118    0         118        0%         41
CheckoutLatencyP99  warning   14     2         12         14%        3
ReadReplicaLag      warning   9      1         8          11%        0
NodeNotReady        critical  4      0         4          0%         0
DBPrimaryCPU        critical  3      2         1          67%        0

Out of hours, in Asia/Seoul (at work mon-fri 08:00-18:00, asleep 22:00-08:00):
of the firings that caught nothing, 57 came while people slept, and 37 more off hours.
RULE                SEVERITY  ● ASLEEP  ● OFF HOURS  ● AT WORK
ExportJob5xx        critical  52        31           35
NodeNotReady        critical  3         0            1
ReadReplicaLag      warning   2         1            5
CheckoutLatencyP99  warning   0         4            8
DBPrimaryCPU        critical  0         1            0

Noisy rules that a longer "for" would quiet, still catching every incident they caught:
RULE                FOR       FIRED     INCIDENTS  CAUGHT LATER BY
ExportJob5xx        5m → 15m  118 → 31  0          –
CheckoutLatencyP99  5m → 10m  14 → 6    1          up to 4m

What each rule adds: the incidents it caught, those it caught before any other rule, those no other rule caught:
RULE                INCIDENTS  FIRST  ALONE
CheckoutLatencyP99  1          0      0
DBPrimaryCPU        1          1      1
ReadReplicaLag      1          1      1
A noisy rule's catch doesn't count beside one by a rule that isn't noisy.

38 rules never fired.

INCIDENT                         START             CAUGHT BY
INC-41 Checkout errors           2026-10-28 12:01  DBPrimaryCPU +2m, CheckoutLatencyP99 +6m (noisy)
INC-42 Search timeouts           2026-10-30 09:15  ReadReplicaLag -3m
INC-43 Batch stuck               2026-10-31 02:40  ✗ nothing fired
impact-1 user impact (--impact)  2026-11-01 21:27  ✗ nothing fired

✗ 2 of 4 incidents had no alert.

Rules that nearly caught them: their condition held, but not for their "for", or fired too late; or their value came within a fifth of their threshold.
INCIDENT  RULE             HELD          CHANGE         WOULD FIRE  FIRED  ● NOTHING
INC-43    BatchQueueDepth  20m from -5m  for 30m → 19m  +14m        0 → 3  0 → 2
impact-1  CheckoutLatency  reached 0.86  > 0.9 → 0.85   +6m         0 → 2  0 → 1
```

`--compare` replays the rules in place beside those proposed, over the same
period, and shows each rule's stones and each incident's catches before →
after; without incidents, how often each fires. Run it in CI on a pull
request that touches the rules: a script for GitHub, GitLab, Bitbucket, Azure
DevOps and Gitea, and an action for GitHub Actions, comment the report on
the pull request as Markdown, and keep that one comment up to date. The
report also comes as JSON and as Prometheus metrics, and goes to Slack and a
Pushgateway.

`--check-replay` sets a replay beside what really fired, rule by rule, so you
can check the replay on your own rules before you believe a verdict.

## Every hour: `lapilli serve`

`lapilli serve` keeps a ledger: every firing, incident and version of the
rules, replayed hourly with the rules as they stood. It shows the stones in
a web UI where teams act on them.

<div class="grid">
  <div><h3>A dashboard</h3><p>Last week's precision, firings, incidents missed, noisy rules and the firings that woke people for nothing beside the week before; twelve weeks by week; where the noise is, rule by week; what to look at first.</p></div>
  <div><h3>Every change replayed</h3><p>A new version of the rules is replayed over the week before it, so you see what the change would have done.</p></div>
  <div><h3>Incidents written down</h3><p>Write an incident, or say a black stone was real, and the whole ledger is judged again at once, without asking Prometheus.</p></div>
  <div><h3>Quiet windows</h3><p>A deploy, maintenance, a nightly batch: firings in a window that caught nothing are excused, once or every day or week.</p></div>
  <div><h3>Teams, tasks, comments</h3><p>Teams own rules and incidents; a suggestion becomes a task with a press, with an assignee, a due date and a link to your tracker.</p></div>
  <div><h3>Incidents from where they are kept</h3><p>Scripts copy them from PagerDuty, incident.io, Jira and GitHub issues, to a port that takes incidents with a token and does nothing else. Written again unchanged, nothing happens.</p></div>
  <div><h3>A cloud's alarms, kept</h3><p>A script reads CloudWatch, Azure Monitor, Cloud Monitoring or OCI alarm history and copies it in, with a token of its own, so the ledger keeps more than the cloud does; with no rules of its own, lapilli serve runs on that alone. Read through the cloud's API too, an alarm and its history are one rule.</p></div>
  <div><h3>Pages, not only firings</h3><p>Notifications as Alertmanager, Grafana, SNS, Azure Monitor and Cloud Monitoring sent them, on a port of their own: each rule's pages, repeats and pages while people slept, beside its firings.</p></div>
  <div><h3>Notices and digests</h3><p>What happens goes to a webhook as JSON, and to Slack, through an outbox written with the change. So does each week's digest, and each team can have its own.</p></div>
  <div><h3>Roles</h3><p>Viewers, editors and admins, signed in by Lapilli with any OpenID Connect provider, or by the sign-in in front of it: a cloud's identity-aware proxy, a load balancer's, oauth2-proxy.</p></div>
  <div><h3>Outcomes</h3><p>Each change to the rules, its prediction beside what came of it; whether incidents still reach it; whether it is used. From the ledger, sent nowhere.</p></div>
  <div><h3>Changes, approved</h3><p>A longer for, a nearer threshold, a spare rule retired, a runbook link a person wrote: each proposed with its backtest, approved by a person on its page, checked again, and opened as a pull request on GitHub, GitLab, Bitbucket, Gitea or Azure DevOps by a script that holds the repository's credentials, or applied through Grafana's API, a Mimir, Cortex, Amazon or Azure ruler's, or a cloud's own alarms' (CloudWatch, Azure Monitor, Cloud Monitoring, OCI), where it is let change. Off until turned on; a stop switch and limits; nothing turned off while it fires.</p></div>
  <div><h3>Agents, over MCP</h3><p>An agent asks what the pages show, replays a change it proposes to the rules before anyone makes it, and, when allowed, keeps it as a proposal for a person to approve. It never approves or applies one.</p></div>
  <div><h3>English, Spanish, Japanese and Korean</h3><p>One file of translations per language; a test fails if a text has none.</p></div>
</div>

The ledger is one SQLite file. Over two million firings an hourly run takes
0.18 s and twelve weeks by rule 2 ms, and these budgets are part of the
tests. `/metrics` gives the stones to Prometheus, and the Helm chart a Grafana
dashboard of them. The UI runs no scripts.

## How it judges

- **Firings** come from replaying your alert rules against history. The
  rules can be Prometheus rule files, `PrometheusRule`, Grafana alerting
  provisioning, a cloud's managed rule groups, its alarms written in PromQL
  (CloudWatch PromQL alarms, Google Cloud alerting policies), or ConfigMaps
  holding them, each version while it was in effect. They can also be the
  rules a Prometheus or a Grafana has loaded. Firings can also come from the
  `ALERTS` series, or from a file or a job copying them in, such as a
  cloud's alarm history. Grafana rules are evaluated the way Grafana does.
- **Incidents** come from a YAML file, from the UI and the API, and from
  windows where a signal of user impact holds (`--impact`). A thin incident
  log still works.
- **A firing catches an incident** when it starts shortly before or during it
  and agrees on the labels you name. One already firing before caught
  nothing: it was on anyway.

## Works where your alerts are

Any Prometheus-compatible API works: Prometheus, Thanos, Mimir,
VictoriaMetrics, the managed Prometheus of AWS, Azure or Google Cloud, or
CloudWatch's PromQL API, with the credentials the machine or pod already has.
Oracle Cloud has no managed Prometheus; on OKE, use the one you run there.
Alarms in a cloud's own terms (CloudWatch, Azure Monitor, Cloud Monitoring,
OCI Monitoring) are judged from their history, with a small script for each
cloud, and `lapilli serve` keeps that history past what the cloud keeps.
Integrations are generic: a webhook, not a particular tracker.

## Built to be trusted

- **It changes only what a person approves.** Out of the box it reads.
  Turned on, it makes the changes to rules it proposes, each once a person
  has approved it in the UI: as a pull request to where the rule is kept,
  or through Grafana's, a ruler's or a cloud's API with what it replaces
  kept for undo. It never silences an alert, never turns one off while it
  fires, and an agent can only propose.
- **Secrets stay put.** Tokens and webhook URLs are read from files and
  never shown, and nothing is sent anywhere it wasn't told to go.
- **Checked releases.** Every release is checked with govulncheck before it
  is published and again every day, and carries build provenance:
  `gh attestation verify` works on each binary, the chart and the image.
- **Out in the open.** Reports go through private GitHub security
  advisories, and the code is scanned by CodeQL and OpenSSF Scorecard.

## Help build it

Lapilli has one maintainer and is looking for more, from more than one
organisation. The open questions are good ones. How faithfully can a replay
reproduce what fired? What makes a fair verdict from a thin incident log?
Several areas need what the current maintainer doesn't have: an AWS or Azure
workspace, Loki alerts, an incident tool.

- [Roadmap](https://github.com/lapilli-project/lapilli/blob/main/ROADMAP.md):
  what it doesn't do yet, area by area, with where each lives in the code
- [Maintainers](https://github.com/lapilli-project/lapilli/blob/main/MAINTAINERS.md):
  how contributing in an area leads to maintaining it
- [Governance](https://github.com/lapilli-project/lapilli/blob/main/GOVERNANCE.md):
  the principles, and how decisions are made
- [Contributing](https://github.com/lapilli-project/lapilli/blob/main/CONTRIBUTING.md):
  build, test, send a change

The most useful report there is: a rule that replays differently from how it
really fired.

## Install

```sh
go install lapilli.dev/cmd/lapilli@latest
```

or a binary from the
[releases](https://github.com/lapilli-project/lapilli/releases), the image
`ghcr.io/lapilli-project/lapilli`, or the Helm chart
`oci://ghcr.io/lapilli-project/charts/lapilli`. The chart runs it once a
week as a CronJob, or with `mode: serve` every hour.

## The name

> *mos erat antiquus niveis atrisque lapillis,*<br>
> *his damnare reos, illis absolvere culpa*
>
> It was the old custom to judge with white and black pebbles: the black to
> condemn the accused, the white to acquit them.
>
> — Ovid, *Metamorphoses* XV.41–42

*Lapilli* are small stones. Each firing is a vote; once a week the urn is
emptied and the stones are counted.
