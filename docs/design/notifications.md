# Notifications as they were sent

Status: built, for v0.20.0. Under [GOVERNANCE.md](../../GOVERNANCE.md) this
is a new kind of data in the ledger, so the design came first, as a pull
request (#28); it was merged on 2026-11-09, before its week was out, by the
maintainer's decision, with no comments. Where the build differs, it says so
in "As built". A cloud's own alarms' notifications are proposed for v0.21,
in the last section. The companion to
[serve.md](serve.md), which is about the firings a replay finds, and
[work.md](work.md), about what people keep.

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

## As built

Built for v0.20.0 as above, the questions for review settled: a port of its
own; a notification that ties to no firing kept and counted; notifications
kept for `--keep`; the SNS confirmation opt-in. Where it differs:

- **The tie isn't stored.** `notified` keeps each alert's rule, labels, start
  and status; the firing it tells of is worked out when read. A run replaces
  the firings it finds, and their ids, and a notification comes before the
  run that finds its firing.
- **Two evaluations** are twice the longest of the rule's group interval,
  `--step` and a minute (Prometheus's default).
- **Grafana's webhook has no `ruleUID`.** The rule is the alert's
  `alertname`, its title, as Lapilli names Grafana's rules.
- **SNS takes only the topics `--notify-sns` names.** A signature proves that
  SNS sent a message, not whose topic it came from: anyone can make a topic
  and subscribe the port to it. China's SNS hosts (`amazonaws.com.cn`) are
  taken too. A message SNS sends again, with the same id, is kept once.
- **Google Cloud and Azure.** A Google Cloud incident's rule is its PromQL
  condition's `alertRule`, or the policy's display name, as Lapilli names the
  policy; its labels, the policy's user labels, the resource's and the
  metric's, and its severity when it is critical, error or warning. An Azure
  alert's labels are its context's, with the essentials' severity where they
  have none; its group, its `alertId`.
- **A receiver** is the one the notification names, or `?receiver=` in the
  URL for those that name none (Azure, Google Cloud).
- **What is counted.** Every notification for a rule, resolved ones too, is
  one; asleep, by the hours of the rule's team. A firing says it paged no one
  once a notification told of an alert of its rule that started before it.
- **The port doesn't check the host name** as the UI's port does: every
  request carries its own proof, and Google Cloud's webhooks come by a
  public name.
- **SNS, bounded.** A certificate comes only from a host named by a region
  (`sns.us-east-1.amazonaws.com`, `sns.cn-north-1.amazonaws.com.cn`,
  `sns.eusc-de-east-1.amazonaws.eu`), at SNS's own path for one
  (`/SimpleNotificationService-HEX.pem`, nothing added): the S3 bucket
  named `sns` is at `sns.s3.amazonaws.com`, and anyone may own it. A
  message is checked before anything says who sent it, so certificates are
  fetched a few a minute, one at a time, and a URL that gave none isn't
  tried again for ten minutes; 32 are kept, the one used longest ago making
  way for a new one.
- **Paged no one, from when the port began.** A rule's firings are taken as
  unpaged from the later of its earliest alert a source told of and that
  source's first notification: an alert on before a source was connected
  doesn't make the firings before it look unpaged.
- **Firings recorded elsewhere.** An Azure alert that isn't a Prometheus
  rule's carries the labels `recipes/firings/azure-monitor.jq` gives its
  firing (severity, resource, resource group, the first condition's
  dimensions), so the two tie. An alert ties by its rule's name: a
  recorded rule named apart in the ledger (`disk-full [azure-other]`)
  isn't tied to, since what the pages count a notification against is the
  alert's rule.
- **The notifications token is its own**: one that is also the incidents or
  the firings token isn't taken, as it sits in Azure's and Google Cloud's
  webhook URLs.

## A cloud's own alarms' notifications (proposed for v0.21)

Status: proposed, for v0.21. Under [GOVERNANCE.md](../../GOVERNANCE.md) a
change to what the ledger keeps is designed first, as a pull request open for
at least a week. Everything above stays as built; this adds to it.

### Why

Since v0.20.0, a cloud's alarms that aren't written in PromQL are judged
from the history the cloud keeps, copied in a source at a time
([recorded.md](recorded.md)). What they sent isn't kept. So a CloudWatch
alarm on a hundred instances shows as one firing, whatever it cost, and the
rules list can't say which of a cloud's alarms woke people for nothing. The
roadmap lists this as still to do, every cloud alike.

Each cloud sends its alarms' notifications to a place an HTTPS endpoint can
subscribe to: CloudWatch to an SNS topic, Azure Monitor to an action group,
Google Cloud to a notification channel, OCI Monitoring to a Notifications
topic. Three of the four already reach the port, on paths built for managed
Prometheus. This section says what each sends for its own alarms, how each
is tied to the firing copied in for it, and how OCI's way in proves itself.

### Per cloud

| Cloud | Way in | The alarm, and its labels | When it started | Repeats and resolutions |
|---|---|---|---|---|
| AWS CloudWatch | `/v1/sns`, as today: the SNS envelope is checked as it is now | `AlarmName`; `Trigger.Dimensions` (metric alarms), `AlarmContributorAttributes` (an alarm on several series, one notification for each contributor) | `StateChangeTime` of a move into `ALARM` | No repeats: CloudWatch sends on a change of state. A move out of `ALARM` (to `OK` or `INSUFFICIENT_DATA`) resolves |
| Azure Monitor | `/v1/azure`, as today | `essentials.alertRule`; severity, the target resource and its group, the first condition's dimensions | `firedDateTime`, in every notification | No repeats for a stateful alert. `monitorCondition: Resolved` resolves, and still carries `firedDateTime` |
| Google Cloud | `/v1/google`, as today | `incident.policy_name`, the policy's display name; user labels, the resource's and the metric's, severity | `incident.started_at`, in every notification | `renotify` is a repeat; `state: closed` resolves |
| OCI | `/v1/oci/SOURCE`, new | the alarm's OCID (`alarmMetaData[].id`), its severity and namespace; a split message's dimensions | `timestamp` of an `OK_TO_FIRING` message | `REPEAT` is a repeat; `FIRING_TO_OK` and `RESET` resolve |

**CloudWatch.** An SNS message for an alarm carries the alarm as JSON in
`Message`. The schema differs for a metric alarm, a composite alarm and a log
alarm, and an alarm on several series (Metrics Insights with `GROUP BY`,
PromQL, log alarms) sends one message for each contributor, with
`AlarmContributorId` and `AlarmContributorAttributes`. The labels are
`Trigger.Dimensions` and, for a contributor, its attributes: a superset of
what [cloudwatch.jq](../../recipes/firings/cloudwatch.jq) gives the firing,
which is the alarm's dimensions for a metric alarm and none for an alarm on
several series or a composite alarm, so the read-time tie in
`internal/ledger` holds. A composite alarm's message has no dimensions, as
its firing has no labels. A PromQL alarm's contributor carries attributes of
its series (AWS's example shows one); where they are all its labels, the
message ties to the firing a replay found for it, and where they are fewer,
it is counted and tied to none. Alarm tags (`AlarmTags`, when the alarm
propagates them) aren't labels: the recipe doesn't give them, and they
aren't needed to tie.

The path tells an alarm's message from an alert manager's by its shape, both
having been signed by SNS for a topic `--notify-sns` names: `AlarmArn` and
`NewStateValue` are an alarm's; `version` and `alerts` are AWS's JSON
template for Amazon Managed Service for Prometheus. A CloudWatch alarm
routed through EventBridge to SNS is taken too: `detail-type: CloudWatch
Alarm State Change`, with `detail.alarmName`, `detail.state.value` and
`detail.state.timestamp`. SNS's raw message delivery must stay off: the
signature is of the envelope. `StateChangeTime` is written as
`2025-12-01T13:42:04.919+0000`, with no colon in the zone.

**Azure Monitor.** `parseAzure` already reads an alert that isn't a
Prometheus rule's, with the labels
[azure-monitor.jq](../../recipes/firings/azure-monitor.jq) gives its firing.
Two things are left. The resource is read from `configurationItems`, which
for a log search alert lists what the query found, not the alert's target;
the recipe reads `targetResourceName`. The resource becomes the last part
of `alertTargetIDs[0]`, as the group already is. And Azure writes resource
names in either case (the documented example names one resource group
`pipelinealertrg` in `alertTargetIDs` and `PipeLineAlertRG` in
`originAlertId`), so both the recipe and the parser write the resource and
its group in lower case. An
activity log, service health or resource health alert is stateless: one
notification, fired and done, as its firing is.

**Google Cloud.** `parseGoogle` already names an incident as the recipe
names its firing (the policy's display name, or a PromQL condition's
`alertRule`) and gives it the same labels, in the same order of
precedence: user labels, then the resource's, then the metric's. A policy on
a threshold, an absence or a log match sends the same schema 1.2. What is
left is tests with those conditions' documented payloads.

**OCI.** Monitoring sends an alarm's messages to a Notifications topic; an
HTTPS (Custom URL) subscription posts them as JSON. A message names its
alarm by OCID, not by display name; its `title` is the display name only
when the alarm sets no notification subject. So
[oci.jq](../../recipes/firings/oci.jq) adds the alarm's OCID to its firing's
labels as `alarm_id`, and the path ties a message's alert to the source's
rule whose alerts carry that `alarm_id`. A firing copied in before the recipe
did this has no `alarm_id`; for it, the title is tried as the rule's name. A
grouped message (`Grouped messages across metric streams`) is one alert for
the alarm, as the recipe's firing is; a split message (`Split messages per
metric stream`) is one alert for its stream, with its dimensions as labels.
Severity is lower-cased, as the recipe writes it.

### Tied to the source it was copied in from

As built, an alert ties by its rule's name, so a rule copied in and named
apart (`disk-full [azure-other]`) isn't tied to. A notification of a cloud's
alarm can now say which source its firings are copied in as: `?source=NAME`
in the URL SNS, the action group or the channel posts to, or the path
segment for OCI, whose subscriptions take no query. Its alerts are then tied
to that source's rule that has the name the alarm sent (`rules.sent`) and
the alert's severity, whatever name the ledger gives it, and to firings by
labels and start as before. Without `?source=`, or with no such rule (an
alarm replayed rather than copied in), the alert's name ties as today. The
ledger keeps that source with each notification, in a new column of
`notifications` beside the one that names the path, in a migration of its
own, so a later tie reads it.

A source in a URL isn't signed by SNS. A signed message posted again to
another URL is still the same message, by its `MessageId`, and is kept once,
under the source that brought it first.

### When a message doesn't say when it started

CloudWatch's move out of `ALARM`, and OCI's `REPEAT`, `FIRING_TO_OK` and
`RESET`, don't carry when the alarm began. Such an alert takes the start of
the last notification of its group (CloudWatch: `AlarmArn` and the
contributor; OCI: `dedupeKey`) the ledger has, as the reason a notification
was sent is already worked out from the one before it. With none, because
the port began later or that message was lost, it is kept and counted, and
tied to no firing.

### OCI's way in

- **Proof.** A Notifications message carries `X-OCI-NS-Signature`,
  `X-OCI-NS-SignatureVersion` and `X-OCI-NS-SigningCertURL`, but Oracle
  doesn't document what is signed, how, or which hosts a certificate comes
  from. Lapilli doesn't guess: a signature it can't check proves nothing.
  It takes basic authentication instead, which an HTTPS subscription
  supports, with the notifications token as the password in the URL
  (`https://lapilli:TOKEN@host/v1/oci/oci-prod`). A request without it is
  answered 401 with `WWW-Authenticate: Basic`, as Oracle requires of an
  endpoint and as `/v1/google` does already. The header is checked before
  the body is read. If Oracle documents the signature, checking it can follow,
  bounded as SNS's is.
- **Confirmation.** A new subscription is confirmed with
  `X-OCI-NS-ConfirmationURL` (or `ConfirmationURL` in the body), valid for
  three days. Lapilli takes it only from a request that carries the token,
  only over https, only from a Notifications API host
  (`notification.REGION.REALM-DOMAIN`, as Oracle lists that API's endpoint,
  `oraclecloud.com` in the commercial realm; Oracle shows the confirmation
  URL itself only as a placeholder), following no redirect, with
  ten seconds and 64 KB at most, one at a time. Anything else is logged,
  with the subscription's OCID, for someone to confirm with
  `oci ons subscription confirm`. `X-OCI-NS-UnsubscribeURL` comes with every
  message and is never logged.
- **Five seconds.** Notifications gives an endpoint five seconds to answer,
  TLS included. The path waits at most two seconds for its turn at the
  ledger, then answers 503, and confirms after answering. Notifications
  tries again at once, then after 1, 2, 4, 8, 16 and 32 minutes, for two
  hours by default.
- **Duplicates.** Oracle doesn't say whether a message tried again keeps its
  `X-OCI-NS-MessageId`. Where it does, the message is kept once by it, as
  SNS's are by `MessageId`; either way, a second message with the same
  `dedupeKey` and `timestampEpochMillis`, which Oracle suggests for telling
  copies apart, is the same one.
- **Bounds**, as the other paths since v0.20.1: names that show, a rule's
  512 characters and a receiver's 200, 64 labels and 4 KB of them, and a
  rate and a day's alerts of its own. The
  receiver is the topic's name (`X-OCI-NS-TopicName`).
- **What an attacker could do.** Someone with the notifications token,
  which sits in OCI's, Azure's and Google Cloud's subscription URLs, can
  post notifications that never were, within those bounds. Without it,
  nothing: no body is read and no URL fetched.

### What is counted

Nothing new is shown: a cloud's alarm gets what an alert manager's rule got
in v0.20.0, on its page, the rules list, the dashboard, `/metrics` and MCP.
Its notifications are counted as sent, its repeats where the cloud repeats,
and its notifications for firings that caught nothing. A firing paged no
one only from when its source first sent a notification, and only for an
alarm a notification has told of: an alarm whose actions go somewhere
Lapilli doesn't hear (an email, PagerDuty directly) has no notifications
here, which doesn't mean it paged no one.

### Building it

The CloudWatch and EventBridge shapes on `/v1/sns`, the Azure resource from
the alert's target, `/v1/oci/SOURCE`, `?source=`, the start of a message
that doesn't carry it, and `oci.jq`'s `alarm_id`. Each tested with payloads
from the clouds' documentation and a server standing in, as v0.20.0's were;
none against a real account until someone with one checks them.

### For review

- `?source=` in the URL, or a flag that maps topics, action groups and
  channels to sources.
- One notifications token for every token path, or one for each: OCI's,
  Azure's and Google Cloud's sit in subscription URLs that more people can
  read than an Alertmanager's configuration.
- Confirming OCI subscriptions from the port, or only logging them.
- Whether `INSUFFICIENT_DATA` from `OK`, which pages people about missing
  data and is of no firing, should be counted at all.
- A grouped OCI message as one alert, or one for each stream it lists (up
  to a hundred).

### Sources

- CloudWatch alarm notifications to SNS, by alarm type:
  https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/Notify_Users_Alarm_Changes.html
- Contributor-level notifications for alarms on several series:
  https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/alarm-actions.html
- CloudWatch alarm state changes in EventBridge:
  https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/cloudwatch-and-eventbridge.html
- SNS raw message delivery, and its retries:
  https://docs.aws.amazon.com/sns/latest/dg/sns-large-payload-raw-message-delivery.html,
  https://docs.aws.amazon.com/sns/latest/dg/sns-message-delivery-retries.html
- Azure Monitor's common alert schema:
  https://learn.microsoft.com/en-us/azure/azure-monitor/alerts/alerts-common-schema
- Cloud Monitoring's webhook payload, schema 1.2:
  https://docs.cloud.google.com/monitoring/support/notification-options
- OCI Notifications: headers, retries, HTTPS subscriptions, confirmation:
  https://docs.oracle.com/en-us/iaas/Content/Notification/Concepts/notificationoverview.htm,
  https://docs.oracle.com/iaas/Content/Notification/Tasks/create-subscription-https.htm,
  https://docs.oracle.com/en-us/iaas/Content/Notification/Tasks/confirm-subscription.htm,
  https://docs.oracle.com/en-us/iaas/Content/Notification/Tasks/get-confirm-subscription.htm,
  https://docs.oracle.com/en-us/iaas/Content/Notification/Concepts/notificationbestpractices.htm
- The Notifications API's endpoint, by region and realm:
  https://docs.oracle.com/iaas/Content/General/Concepts/dedicatedregions.htm
- OCI alarm messages, their types and examples:
  https://docs.oracle.com/en-us/iaas/Content/Monitoring/alarm-message-format.htm,
  https://docs.oracle.com/en-us/iaas/Content/Monitoring/Concepts/monitoringoverview.htm,
  https://docs.oracle.com/en-us/iaas/Content/Monitoring/alarm-message-examples.htm
