# Notifications as they were sent

A rule's firings can be a poor stand-in for its pages. The alert manager
groups a rule's alerts into one page, waits out the ones that resolve
quickly, pages again every `repeat_interval` while one stays firing, and
silences, inhibits or routes others to no one. So `lapilli serve` can keep
the notifications as they were sent, beside the firings, and say how many
each rule sent, how many were repeats, how many came while people slept, and
how many were for firings that caught nothing.

With `--notify-listen ADDR` it opens a port that takes notifications, and
nothing else, each source on a path of its own:

| Where the rules run | Send to | Proved by |
|---|---|---|
| Prometheus, Thanos, Mimir, VictoriaMetrics, with an Alertmanager (on any cloud, OKE among them); Google Cloud Managed Service for Prometheus's managed Alertmanager | `/v1/alertmanager`, a `webhook_config` | the token in `--notify-token-file`, as `http_config.authorization`'s bearer token |
| An Alertmanager of 0.33.0 or later, its event recorder (`--enable-feature=event-recorder`): every delivery, PagerDuty's, Slack's and email's too, to `RECEIVER/INTEGRATION`; alerts a silence or an inhibition muted aren't counted as delivered, and are kept as mutes ([below](#why-a-firing-paged-no-one)) | `/v1/alertmanager/events`, an `event_recorder` `webhook_outputs` entry (`batch: true` is fine). Drop a webhook receiver that sends to Lapilli, or name it in `?skip=NAME`: the recorder tells of its deliveries too, and each would count twice | as above |
| Grafana Alerting | `/v1/grafana`, a webhook contact point | the token as its Authorization header, or its HMAC signature with the secret in `--notify-grafana-secret-file` (timestamp header `X-Grafana-Alerting-Timestamp`, five minutes at most) |
| Amazon Managed Service for Prometheus | `/v1/sns`, an HTTPS subscription to the SNS topic its alert manager sends to, with AWS's JSON template for SNS | SNS's signature, from a topic `--notify-sns` names; the subscription is confirmed by fetching its `SubscribeURL`, from an SNS host only |
| Azure Monitor managed service for Prometheus | `/v1/azure`, an action group's webhook with the common alert schema | the token as the password in the URL: `https://lapilli:TOKEN@host/v1/azure` |
| Google Cloud Monitoring policies with a PromQL condition | `/v1/google`, a webhook channel with basic auth | the token as the password, sent once asked with a 401. Its webhooks reach public endpoints only |

A cloud's own alarms, those not written in PromQL whose firings are copied
in ([Firings recorded elsewhere](serve.md#firings-recorded-elsewhere)), are told of
on the same paths, every cloud alike:

| A cloud's alarm | Send to | Proved by |
|---|---|---|
| Amazon CloudWatch, a metric, composite or log alarm, or one on several series (each contributor's message) | `/v1/sns`, the SNS topic its alarm actions send to, directly or through EventBridge's "CloudWatch Alarm State Change" | as above; keep SNS's raw message delivery off |
| Azure Monitor, a metric, log search or activity log alert | `/v1/azure`, an action group's webhook with the common alert schema | as above |
| Google Cloud Monitoring, a policy on a threshold, an absence or a log match | `/v1/google`, a webhook channel with basic auth | as above |
| OCI Monitoring, an alarm | `/v1/oci/SOURCE`, an HTTPS (Custom URL) subscription to the alarm's Notifications topic | the token as the password in the subscription's URL: `https://lapilli:TOKEN@host/v1/oci/oci-prod`, answered 401 without it. Its subscriptions reach public endpoints only |

The URL says which source the alarm's firings are copied in as:
`?source=cloudwatch-prod`, or the path for OCI, whose subscriptions take no
query. An alert is then tied to that source's rule with the name the alarm
sent, and its severity, though the ledger names the rule apart
(`disk-full [cloudwatch-prod]`); an OCI alarm's, to the rule whose firings
carry its OCID as `alarm_id`, as `recipes/firings/oci.jq` labels them,
whatever its message's title, and whenever its firing is copied in: the
name is worked out when read. A message of an alarm resolving, or of OCI's
repeating, doesn't say when the alarm began: it takes the start of the last
notification of its alarm (or contributor, or stream) with the same labels,
of the same kind of message, over the last month, and with none, is counted
and tied to no firing. An Azure resource and its group are compared without
their case. CloudWatch sends on a change of
state only, so it has no repeats; a move between `OK` and
`INSUFFICIENT_DATA` is of no firing, and isn't kept. OCI's `REPEAT` is a
repeat, and `FIRING_TO_OK` and `RESET` resolve.

OCI Notifications signs its messages, but Oracle doesn't document what is
signed or where its certificates come from, so lapilli takes the token
instead. A new subscription is confirmed from the URL it is sent with,
only from a request with the token, only over https to a Notifications API
host (`notification.REGION.oraclecloud.com`, in any of Oracle's realms),
and only to an address on the internet, not one on this machine or a private
network (as for SNS's certificates; through a proxy, when the environment
names one), following no redirect, after the answer, each URL once; any other is left
in the log, by its host, for someone to confirm with `oci ons subscription
confirm`. Notifications gives an endpoint five seconds, so `/v1/oci` waits
two for its turn, then answers 503, which Notifications tries again. A
message tried again is kept once, by its alarm and its evaluation.

```yaml
# Alertmanager
receivers:
  - name: oncall
    webhook_configs:
      - url: http://lapilli:8083/v1/alertmanager
        max_alerts: 1000
        http_config:
          authorization:
            credentials_file: /etc/alertmanager/lapilli-token
```

A notification's alert is tied to its rule's firing whose labels are all
among the alert's (which carries external labels the replay doesn't), and
which started within two evaluations of it; one that ties to none is kept and
counted as such. That is worked out when read, since a notification comes
before the run that finds its firing. Alertmanager says why it sent each
notification since 0.32.0, and Google Cloud says which are repeats; for the
others, a notification with the same alerts as the last one in its group,
nothing new and nothing resolved, is a repeat. One with the same group,
receiver and alerts as another within a minute is the same one, sent by
another replica. Notifications don't change the stones.

Each rule's page then says how many notifications it sent over twelve weeks,
how many were repeats, how many came while people slept, how many were for
firings that caught nothing, and to which receivers; each firing, how many
told of it, or that it paged no one, or why ([below](#why-a-firing-paged-no-one)). The dashboard's "Woken for nothing"
counts notifications instead of firings, and says so. With no incident
written down to judge them by, it shows no number, as precision doesn't,
and says how many came while people slept; the report's and the digest's
lines say the same, not "for nothing". `/metrics` has
`lapilli_rule_notifications{rule, receiver, reason}` over seven days; MCP's
`rules` and `rule` carry the same, and the weekly digest, to Slack and the
webhook, a line of it: how many were sent, repeats, those while people
slept, those for nothing, and the rules that sent the most for nothing,
and, once mutes are kept, how many firings paged no one and why
([below](#why-a-firing-paged-no-one)); a team's digest, for its rules.
Notifications can't be replayed for history, so each number counts from
the first that came. In the chart: `serve.notificationsAPI`.

The port takes 1 MB and 1,000 alerts a notification at most (Alertmanager's
`max_alerts` keeps it under), 20 a second from each source, one write at a
time, and 200,000 alerts a day (UTC) from each source, about 200 MB, so
one sending more can't keep another out; past that it answers that source
429 until the next day, and says so in the log once. Its
notifications are kept for `--keep`. It doesn't check the host name, as the
UI's port does: every request carries its own proof.

- A path the token proves is refused before its body is read. SNS's and
  Grafana's signatures are of the body, so a body from anyone is read only
  when its headers say it could prove itself (SNS's message type and a
  topic in `--notify-sns`, which must be the body's; Grafana's signature,
  with a secret to check it by), 64 such bodies at most at once, each
  within ten seconds; past that the port answers 503, which SNS sends
  again.
- A rule's name is 512 characters at most, a receiver's 200, and neither
  may hold a character that doesn't show (a control character, a
  zero-width space, a mark that turns text right to left); a group key
  longer than 1,000 characters, or with such a character, is kept as its
  hash; an alert carries 64 labels and 4 KB of them at most. A notification that
  doesn't keep to this is refused with 400, which an alert manager doesn't
  send again; one of a batch (the event recorder's) is passed over and the
  rest kept. Each is counted in
  `lapilli_serve_notifications_refused_total{source}`, and why is logged.
  `lapilli_rule_notifications` counts only the ledger's rules,
  and a rule's ten busiest receivers, the rest as `receiver="other"`.
- SNS: a message signed more than an hour ago (SNS gives up sooner) or
  ahead of the clock isn't taken. Its certificate must be on the SNS host
  of its topic's region, spelled as SNS spells it. A certificate that has
  verified a message is kept; when one can't be fetched, or too many have
  been lately, the port answers 503 rather than 403, so SNS sends the
  message again rather than drop it. A URL with no certificate behind it
  is fetched once and doesn't count against those: someone who knows a
  topic taken can still make lapilli ask SNS's host for certificates, one
  at a time, and if that host slowed lapilli down, a certificate SNS had
  just begun to sign with would wait for it. The same message again, by its
  MessageId within the hour, is answered as kept without a turn. A topic
  not in `--notify-sns` and a signature that doesn't hold get the same
  answer. Lapilli confirms a subscription with a GET of its `SubscribeURL`,
  which leaves `AuthenticateOnUnsubscribe` off: someone who sees the
  subscription's `UnsubscribeURL` can unsubscribe it, so keep the port's
  logs, and any proxy's, to those who run it.
- Grafana: set the contact point's timestamp header. With it, a signature
  holds five minutes either side of its time; the same signature again in
  that time is answered as kept. Without it, a signature holds as long as
  the secret does, and someone who saw a request could send it again
  later.

Lapilli signs what it posts to its own webhook in the same shape, an
HMAC-SHA256 of a timestamp and the body, with `--notify-webhook-secret-file`,
for the receiver to check: [Signed posts to the
webhook](serve.md#signed-posts-to-the-webhook).

## Why a firing paged no one

Alertmanager's event recorder also says when a silence or an inhibition
kept an alert from a notification: `silenceMutedAlert`,
`inhibitionMutedAlert`, and, in 0.35, a notification's `mutedAlerts`.
`/v1/alertmanager/events` keeps them as mutes: when, the alert's name and
labels (bounded as a notification's alert's), whether a silence, an
inhibition, or either (a notification doesn't say which), and the
silence's ID or the inhibiting alert's fingerprint. Nothing else of a
silence is kept: not its comment, nor who made it. The recorder tells of a
mute every few seconds, again after a failure, and from each replica;
each is kept once an hour, from the first to the last, and kept for
`--keep`.

A firing no notification told of then says what muted it, on the rule's
page and the day's firings: "muted by a silence" with its ID, "inhibited
by another alert", or "muted by a silence or an inhibition"; MCP's `rule`
gives each latest firing `muted`. A mute is tied to a firing as a
notification's alert is: its rule's, with all of the firing's labels,
told of from two evaluations before the firing started to two after it
ended, as read in the hours about its start and its end. A firing that
paged no one with nothing muting it was routed nowhere, or never reached
the alert manager.

The weekly digest, a team's, and a report's `notifications` section then
say, beside the notifications sent, how many of the span's firings paged
no one: muted by a silence, inhibited by another alert, muted by one or
the other, or with no mute told of, as their pages say it.
`digest.ready` and `report.ready` carry it as
`notifications.paged_no_one`: `silence`, `inhibition`, `either`,
`nothing`, and `partial` when there were too many mutes to look through
for every firing. It looks at 200,000 rows of mutes at most
([design/mutes.md](design/mutes.md#bounds)).

Mutes come at the events' rate (200 a second, bursts of 1,000), not the
notifications', so a storm of them can't keep a delivery out; 1,000 rows
of a rule's hour and 200,000 rows a day (UTC) at most, past which they are
passed over (until the next hour, or day) and a notification with them is
still kept. An event that can't
be read as the recorder's (a silence with no ID, a mute of an alert with no
`alertname`) is refused, that event alone, and counted in
`lapilli_serve_notifications_refused_total`. Someone with the token can
post mutes that never were: they claim only that a firing was muted, and
change no stone or count ([design/mutes.md](design/mutes.md)).

Annotations aren't kept. Who was on call, and whether they acknowledged, is
for PagerDuty, incident.io or Grafana IRM. Each path is tested against its
source's documented payloads, a cloud's own alarms' among them, with
stand-ins for SNS and for OCI's confirmation; none yet against a cloud's
real account. Grafana's runs against a real Grafana 13.3.1 in CI
(`test/e2e/service.sh`): its alert rule's notifications, signed, each kept
and tied to the firing Lapilli replayed.
