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
  <img alt="The lapilli serve dashboard: last week's precision, firings, incidents missed, time to catch and noisy rules, then firings and precision by week" src="{{ '/serve-dashboard.png' | relative_url }}" width="1240" height="980">
</picture>
</div>

It works from what you already have: the rules, the history in Prometheus,
and whatever record of incidents you keep, even a thin one. It only reads.

## Once: `lapilli verdict`

Replay a week, a month, or a change you are about to merge, and get the
stones, the noisy rules with a longer `for` that would quiet them, and the
incidents nothing caught.

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

Noisy rules that a longer "for" would quiet, keeping every incident they caught:
RULE                FOR       FIRED     ○ CAUGHT
ExportJob5xx        5m → 15m  118 → 31  0 → 0
CheckoutLatencyP99  5m → 10m  14 → 6    2 → 2

38 rules never fired.

INCIDENT                         START             CAUGHT BY
INC-41 Checkout errors           2026-10-28 12:01  DBPrimaryCPU +2m, CheckoutLatencyP99 +6m (noisy)
INC-42 Search timeouts           2026-10-30 09:15  ReadReplicaLag -3m
INC-43 Batch stuck               2026-10-31 02:40  ✗ nothing fired
impact-1 user impact (--impact)  2026-11-01 21:27  ✗ nothing fired

✗ 2 of 4 incidents had no alert.
```

`--compare` replays the rules in place beside those proposed, over the same
period, and shows each rule's stones and each incident's catches before →
after. Run it in CI on a pull request that touches the rules. The report also
comes as JSON and as Prometheus metrics, and goes to Slack and a Pushgateway.

## Every hour: `lapilli serve`

`lapilli serve` keeps a ledger: every firing, incident and version of the
rules, replayed hourly with the rules as they stood. It shows the stones in
a web UI where teams act on them.

<div class="grid">
  <div><h3>A dashboard</h3><p>Last week's precision, firings, incidents missed and noisy rules beside the week before; twelve weeks by week; where the noise is, rule by week; what to look at first.</p></div>
  <div><h3>Every change replayed</h3><p>A new version of the rules is replayed over the week before it, so you see what the change would have done.</p></div>
  <div><h3>Incidents written down</h3><p>Write an incident, or say a black stone was real, and the whole ledger is judged again at once, without asking Prometheus.</p></div>
  <div><h3>Quiet windows</h3><p>A deploy, maintenance, a nightly batch: firings in a window that caught nothing are excused, once or every day or week.</p></div>
  <div><h3>Teams, tasks, comments</h3><p>Teams own rules and incidents; a suggestion becomes a task with a press, with an assignee, a due date and a link to your tracker.</p></div>
  <div><h3>Notices</h3><p>What happens goes to a webhook as JSON, and to Slack, through an outbox written with the change.</p></div>
  <div><h3>Roles</h3><p>Viewers, editors and admins, from the headers your sign-in proxy sets; the UI has no sign-in of its own.</p></div>
  <div><h3>English and Korean</h3><p>One file of translations per language; a test fails if a text has none.</p></div>
</div>

The ledger is one SQLite file. Over two million firings an hourly run takes
0.18 s and twelve weeks by rule 2 ms, and these budgets are part of the
tests. `/metrics` gives the stones to Prometheus, and the Helm chart a Grafana
dashboard of them. The UI runs no scripts.

## How it judges

- **Firings** come from replaying your alert rules against history. The
  rules can be Prometheus rule files, `PrometheusRule`, Grafana alerting
  provisioning, a cloud's managed rule groups, or ConfigMaps holding them,
  each version while it was in effect. They can also be the rules a
  Prometheus or a Grafana has loaded. Firings can also come from the
  `ALERTS` series, or from a file. Grafana rules are evaluated the way
  Grafana does.
- **Incidents** come from a YAML file, from the UI and the API, and from
  windows where a signal of user impact holds (`--impact`). A thin incident
  log still works.
- **A firing catches an incident** when it starts shortly before or during it
  and agrees on the labels you name. One already firing before caught
  nothing: it was on anyway.

## Works where your alerts are

Any Prometheus-compatible API works: Prometheus, Thanos, Mimir,
VictoriaMetrics, or the managed Prometheus of AWS, Azure or Google Cloud,
with the credentials the machine or pod already has. Oracle Cloud has no
managed Prometheus; on OKE, use the one you run there. Integrations are
generic: a webhook, not a particular tracker.

## Built to be trusted

- **It only reads.** It never changes a rule, an alert or a cluster.
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
