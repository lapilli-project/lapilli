# Notifications as they were sent

Status: proposed, not built. Under [GOVERNANCE.md](../../GOVERNANCE.md) this
is a new kind of data in the ledger, so the design comes first, as a pull
request open for a week. The companion to [serve.md](serve.md), which is about
the firings a replay finds, and [work.md](work.md), about what people keep.

## Why

Lapilli asks which alerts are worth their page, and counts firings. A page is
a notification, and between the two sits the alert manager:

- **Grouping.** A rule that fires for forty pods in a minute may page once.
- **Waiting.** An alert that resolves within `group_wait` pages no one.
- **Repeats.** One that stays firing pages again every `repeat_interval`, which
  a firing count never shows: one firing, a page every four hours for a week.
- **Silences, inhibitions and routes.** A firing can page no one at all.

So a rule's firings can be a poor stand-in for its pages, in either direction.
The README says so plainly today. This note proposes that `lapilli serve` keep
the notifications as they were sent, beside the firings, and say how many
pages each rule cost, how many came while people slept, and how many were for
nothing.

## What it would answer

- For each rule: notifications sent, by receiver, and how many were repeats;
  pages for each firing; the notifications for firings that caught nothing.
- When they came, by the hours `--work-hours` and `--sleep-hours` give: a
  notification, unlike a firing, is a person woken.
- Firings that paged no one. Lapilli can't always say why: silenced, inhibited
  or routed nowhere look the same from the receiving end.
- What `repeat_interval` costs: the repeats of alerts that caught nothing.

It stops at receivers. Who was on call, and whether they acknowledged, is
what PagerDuty, incident.io and Grafana IRM measure already.

## Where notifications come from

Each way to run alerting sends notifications its own way. The project keeps
them at parity (GOVERNANCE: "a feature isn't built for one cloud when the
others could have it"), so this is the first question.

| Where the rules run | How notifications can reach Lapilli | Says it is a repeat | Ties a notification to an alert |
|---|---|---|---|
| Prometheus, Thanos, Mimir, VictoriaMetrics, each with an Alertmanager; on any cloud, OKE among them | Alertmanager's `webhook_config` | `notification_reason`, since Alertmanager 0.32.0 | `fingerprint`, labels, `startsAt`; `groupKey` |
| Google Cloud Managed Service for Prometheus | its managed Alertmanager (Preview), configured with the `alertmanager` Secret in `gmp-public`: the same webhook | not in the 0.27-based image its manifest pins | as above |
| Google Cloud Monitoring alerting policies with PromQL | a webhook notification channel, schema 1.2 | `incident.renotify` | `policy_name`, `condition_name`, `metric.labels`, `started_at` |
| Grafana Alerting | a webhook contact point (`version: "1"`) | no | `ruleUID`, labels, `startsAt` |
| Amazon Managed Service for Prometheus | its alert manager sends to Amazon SNS (one AWS page adds PagerDuty); SNS delivers to an HTTPS subscription | no; and `groupKey` isn't in what a template can send | labels, `startsAt`, `fingerprint`, with AWS's JSON template |
| Azure Monitor managed service for Prometheus | an action group's webhook, with the common alert schema | not documented | `essentials.alertRule`, `alertContext.labels`, `firedDateTime`, `monitorCondition` |

OCI has no managed PromQL; on OKE the rules run in a Prometheus of one's own,
with an Alertmanager, the first row. Cloud alarms (CloudWatch, Azure Monitor
metric alerts, Google Cloud alerting on its own metrics, OCI Monitoring) stay
out of scope, as in the [roadmap](../../ROADMAP.md), until Lapilli reads them.

Every source can say, at least, which rule an alert came from, its labels and
when it started, so each notification can be tied to a firing in the ledger.
Repeats can be told apart only where the source says so, or by timing: a
notification for the same group and the same alerts, with nothing new and
nothing resolved, after `repeat_interval`.

## The way in

A port of its own, `--notify-listen ADDR`, as `--incidents-listen` is: it
takes notifications, and nothing else, so a NetworkPolicy can let the alert
manager reach it and no one else. One path for each source, since each is
checked its own way:

| Path | Checked by |
|---|---|
| `/v1/alertmanager` | a bearer token (`http_config.authorization`), from `--notify-token-file`, compared in constant time |
| `/v1/grafana` | the same token in the Authorization header; or Grafana's HMAC-SHA256 signature, with a secret from a file, and, when Grafana sends a timestamp, one no older than five minutes |
| `/v1/sns` | the SNS message's signature, against a certificate from an `sns.REGION.amazonaws.com` host and no other; the subscription is confirmed once by a GET of its `SubscribeURL`, to such a host only. This is the one request the feature sends, and only with `--notify-sns` |
| `/v1/azure` | basic auth in the action group's URL, from the token file |
| `/v1/google` | basic auth, answering the first request with 401 and `WWW-Authenticate`, as Cloud Monitoring requires; its webhooks reach public endpoints only, so this one needs a public way in |

Limits, as on the incidents port: bodies up to 1 MB, one write at a time, a
rate per second, at most 1,000 alerts in one notification (Alertmanager's
`max_alerts` can keep it under that), and notifications kept for `--keep`.

## In the ledger

New tables, in a migration of their own:

- `notifications`: when it came, the source, the receiver, the group key where
  there is one, why it was sent (`first`, `new`, `resolved`, `repeat`, or
  `unknown`), and whether all its alerts were resolved.
- `notified`: one row for each alert in it: the notification, the rule, the
  series (as the ledger keeps series), the alert's start and status, and the
  firing it was tied to, if any.

Annotations aren't kept: they are often long, sometimes hold what shouldn't
be stored, and nothing here needs them. Labels are kept as the series already
are.

**Tying an alert to a firing.** The alert's rule is its `alertname` (Grafana:
`ruleUID`). Its firing is that rule's firing whose labels are all among the
alert's (the alert also carries external labels the replay doesn't), and
which started within two evaluations of the alert's `startsAt`. One that ties
to nothing is kept, and counted as such: a rule Lapilli doesn't replay, or a
replay that differs from what fired, which `--check-replay` looks into.

**Duplicates.** Alertmanager in high availability prefers sending twice to
not sending; each replica sends. A notification with the same group key,
receiver, alerts and statuses as one in the last minute is the same
notification.

Judging doesn't change. A notification is counted against the firing it was
tied to, and so against that firing's stone: notifications for black firings
are pages for nothing.

## What it shows

- A rule's page and the rules list: notifications and repeats beside firings,
  and how many came while people slept.
- A firing's notifications; a firing that paged no one says so.
- The dashboard's "woken for nothing" counts notifications once there are any,
  and says which it counts.
- `/metrics`: `lapilli_rule_notifications` by rule, receiver and reason.
- MCP's `rules` and `rule` tools carry the same.

Before the port is connected, there are no notifications to count: unlike
firings, they can't be replayed for history. Each number says from when it
counts.

## Alternatives

- **Work notifications out from the replayed firings and the alert manager's
  configuration.** It would work on history and on a change not yet made. But
  silences, inhibitions, high availability and each managed service's own
  configuration make it a model, not a record. It could come later, checked
  against what this ledger records.
- **Read Alertmanager's notification log.** It keeps the last notification for
  each group and receiver, overwritten, for `--data.retention` (120h by
  default): not a history.
- **Alertmanager's event recorder** (0.33.0, behind
  `--enable-feature=event-recorder`) records every successful delivery to any
  integration, PagerDuty and Slack too, not only webhooks. It is the fullest
  source for an Alertmanager of one's own, and could be a later path in; no
  managed service offers it, so it can't be the only one.
- **`alertmanager_notifications_total`** is counted by integration, not by
  rule.

## Building it

All the paths in one release, as the table above, each tested with payloads
taken from the sources' own documentation and a server standing in. AWS and
Azure are then checked against a real account by whoever has one, as the
roadmap's "Clouds, checked for real" asks already.

## For review

- One port for notifications, apart from the incidents port, or one port
  with two tokens.
- Whether to keep a notification that ties to no firing, or only count it.
- How long to keep notifications: `--keep`, or shorter.
- The SNS confirmation is the only outbound request the feature adds. Is
  opt-in (`--notify-sns`), to SNS hosts only, enough?

## Sources

- Alertmanager `webhook_config`, its payload and `http_config`:
  https://github.com/prometheus/alertmanager/blob/v0.34.2/docs/configuration.md
- Why a notification is sent (`notification_reason`, 0.32.0) and the dedup
  stage: https://github.com/prometheus/alertmanager/blob/v0.34.2/notify/dedup_stage.go
- High availability sends duplicates rather than miss:
  https://github.com/prometheus/alertmanager/blob/v0.34.2/docs/high_availability.md
- Event recorder (0.33.0):
  https://github.com/prometheus/alertmanager/blob/v0.34.2/eventrecorder/eventrecorderpb/eventrecorder.proto
- Grafana webhook contact point:
  https://grafana.com/docs/grafana/latest/alerting/configure-notifications/manage-contact-points/integrations/webhook-notifier/
- Google Cloud Managed Service for Prometheus, managed rule evaluation and
  Alertmanager: https://docs.cloud.google.com/stackdriver/docs/managed-prometheus/rules-managed
- Cloud Monitoring webhook payload (schema 1.2):
  https://docs.cloud.google.com/monitoring/support/notification-options
- Amazon Managed Service for Prometheus alert manager receivers and SNS:
  https://docs.aws.amazon.com/prometheus/latest/userguide/AMP-alertmanager-receiver.html,
  https://docs.aws.amazon.com/prometheus/latest/userguide/AMP-alertmanager-receiver-JSON.html
- SNS HTTPS delivery and signatures:
  https://docs.aws.amazon.com/sns/latest/dg/http-notification-json.html,
  https://docs.aws.amazon.com/sns/latest/dg/sns-verify-signature-of-message.html
- Azure managed Prometheus rule groups and the common alert schema:
  https://learn.microsoft.com/en-us/azure/azure-monitor/metrics/prometheus-rule-groups,
  https://learn.microsoft.com/en-us/azure/azure-monitor/alerts/alerts-common-schema
