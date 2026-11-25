# Who a firing reached, worked out

A firing isn't a notification, and a notification isn't a page. The route
tree sends it to one receiver, several, or one that sends nothing; grouping
puts it in a notification with others and holds it; an inhibition rule, a
silence or a time interval mutes it; and only some receivers ring a phone.
So a rule that a critical rule always inhibits looks noisy and wakes no one,
thirty rules grouped into one notification are one page, not thirty, and a
rule that stays on for eight hours with a `repeat_interval` of an hour pages
eight times and counts as one firing.

Given the alert manager's configuration, `lapilli verdict` works this out
over the firings of the period: for each firing, which receivers it reached,
in which notifications, which of them page someone, and what muted it.
`lapilli verdict --compare`, and so a pull request's backtest, says how many
pages a change would add or take away, before it is merged.

**These numbers are worked out, not recorded.** They are a model of what
Alertmanager or Grafana would have done with the alerts, from the
configuration you give it. [Notifications as they were sent](notifications.md)
is the record: `lapilli serve` keeps the notifications an alert manager really
sent. Every place that shows the worked-out numbers says so.

## What it reads

| Flag | |
|---|---|
| `--alertmanager-config FILE\|URL` | the alert manager's configuration, as `--check-content` reads it: `alertmanager.yml`, a file of Mimir's, Cortex's or Amazon's format, or Mimir's or Cortex's API |
| `--grafana URL` | with Grafana's notification policies, contact points and mute timings, read from its provisioning API with `--grafana-token-file` |
| `--external-labels NAME=VALUE` | a label Prometheus adds to each alert as it sends it (`global.external_labels`), which routes often match on, such as `cluster=prod`; repeatable |
| `--silences FILE` | the silences Alertmanager held, as it lists them (`GET /api/v2/silences`, or `amtool silence query -o json`), expired ones too; their matchers and times are read, not the comments or who made them |
| `--paging-receivers REGEX` | every notification to a receiver whose name matches is a page, whatever it sends with: a webhook to a paging tool, say. Not anchored |
| `--notifications=false` | leave it all out: the report is as it was |

It is on when the configuration is given, with `--alertmanager-config`, or
with `--grafana`, whose policies are then read too (if they can't be, Lapilli
says so on stderr and goes on without). Without one, nothing here applies, and
every number and field is what it was.

From the configuration it keeps only what routing needs: each route's matchers,
`continue`, `group_by`, `group_wait`, `group_interval`, `repeat_interval`,
`mute_time_intervals` and `active_time_intervals`; the named time intervals;
`inhibit_rules`; and each receiver's integrations, their kinds and whether
each sends resolves. No field that holds a secret or a connection's address is
read. A configuration Alertmanager wouldn't load (a route naming a receiver
that isn't there, a matcher that doesn't parse) is an error.

## The alerts it is given

Each firing of the period is an alert: `alertname` (the rule's name as
written), the rule's labels, the series' labels and the external labels, from
the evaluation it fired on to the one it resolved on, or the period's end if it
still fires. A Grafana rule's carries `grafana_folder`. The firings of every
rule go in together, because a group is of many rules.

## An example

A week of firings of four rules, with a configuration that sends critical
alerts to `oncall` (PagerDuty and a Slack channel), the cart's to `cart-team`
(a webhook) and everything else to a channel, with an inhibition rule that has
`ApiErrors` hide `DiskFull` in the same namespace, and a silence over the cart's
maintenance:

```sh
lapilli verdict --firings firings.jsonl --incidents incidents.yaml \
  --alertmanager-config alertmanager.yml --external-labels cluster=prod \
  --silences silences.json --work-hours mon-fri@08:00-18:00 --tz UTC
```

```
RULE        FIRED  NOTIFIED  PAGED  PAGED ASLEEP  PAGED OFF HOURS  PAGED FOR NOTHING  INHIBITED         GROUPED
ApiErrors   3      6         3      1             0                2                  –                 0
QueueDepth  2      1         0      0             0                0                  –                 0
DiskFull    2      2         0      0             0                0                  50% by ApiErrors  0
Latency     1      0         0      0             0                0                  –                 0
Worked out from the alert manager's configuration, not recorded: ...
Taken as paging: oncall (pages by its pagerduty).
Reached, and sends nothing: nobody.
```

`ApiErrors` fired three times and paged three times, one of them while people
slept; two of the pages were of firings that caught nothing. `DiskFull` fired
twice and half of its firings were held back by `ApiErrors`. `QueueDepth`'s
first firing was silenced. `Latency` reached a receiver that sends nothing, so
it paged no one however often it fires.

## Which notifications page someone

A notification on an integration of a paging kind is a **page**:
Alertmanager's `pagerduty`, `opsgenie`, `victorops` and `pushover`, and Grafana's
`pagerduty`, `opsgenie`, `victorops`, `pushover` and `oncall`. Every other kind
(Slack, email, a webhook, Teams, Discord, Telegram, SNS, Webex, WeChat, Jira)
is a **message**. A webhook may well be a pager (incident.io and Grafana IRM
take alerts by webhook), and Lapilli can't tell; `--paging-receivers` says so.
The report names the receivers it took as paging, and why: by the kind of an
integration, by `--paging-receivers`, or both.

## What it gives

- **Each firing**: the notifications that told of it, the pages among them, when
  the first page went, the receivers it reached, those of them with no
  integrations, and what muted it, if anything did (the inhibition rule and the
  rule whose firing inhibited it, a silence, a time interval).
- **Each rule**: `NOTIFIED` and `PAGED`, which count each notification for each
  of the rule's firings it told of (a notification of three of its firings
  counts three, so the rules' counts can add up to more than the report's);
  pages while people slept and out of hours, by the hours firings are counted by
  (`--work-hours`, `--sleep-hours`, `--shifts`, `--on-call`); **pages for
  nothing**, the pages of firings that caught nothing, or, with
  [responses](responses.md) read, that weren't useful; how many firings an
  inhibition rule muted, and the rule whose firing muted most of them; and how
  many firings went out in a notification with other rules' alerts.
- **The text report** has a NOTIFICATIONS table after the rules, and the
  out-of-hours section counts the pages that went for nothing, not the firings
  that caught nothing, saying so.
- **`--json`** has `notifications` on the report (`worked_out`, the totals,
  `receivers` with `kinds`, `paging` and `why`, and `stopped`), on each rule and
  on each firing; `--csv` has the `worked_out_*` columns at the end; `--metrics`
  has `lapilli_rule_notifications_worked_out` and `lapilli_rule_pages_worked_out`;
  `--redact` gives receivers, inhibition rules, silences and time intervals an
  alias. See [The numbers](numbers.md#notifications-worked-out).
- **`--compare`**, and the pull request comment it writes, starts its summary
  with the pages before and after, `Pages 12 → 15`, and shows `PAGED` for each
  rule that differs, before and after. A change that would ring more phones says
  so before it is merged. With `--changed-only` the pages are of the changed
  rules' firings alone, which leaves out the groups they would have shared with
  the rules left alone.
- **An expectation** may hold a rule to a `pages` budget beside `firings`:
  `budget: {pages: 3}`, or `lapilli.dev/budget: "3 pages"`
  ([Before a change is merged](compare.md)). It is counted as a week's pages
  of the rule's firings and fails as a firings budget does, and is not known
  where the notifications weren't worked out.

The report doesn't change a thing in the alert manager's configuration, and
Lapilli never proposes a change to it: it writes alert rules alone. It says
what it found, `inhibited by ApiErrors in 92% of its firings`; what to do about
it is a person's.

## What isn't modelled

The model plays the alerts through the route tree, the groups, the inhibition
rules, the silences and the time intervals as Alertmanager's dispatcher and
notification pipeline do, with these departures:

- Flushes are `group_wait` after the group's first alert, then each
  `group_interval` after the one before; a notification repeats when `repeat_interval`
  has strictly passed. A restart of Alertmanager, which resets its groups, isn't
  worked out, nor the notification log's expiry, nor Alertmanager's shortcut for
  an alert whose start is old.
- A mute or active time interval isn't inherited by a route's children, as in
  Alertmanager. A flush that every silence or inhibition empties logs nothing.
- A cluster of Alertmanagers sends once. An integration that fails and retries
  sends once. The second each alert arrived, which Prometheus sets by its
  evaluation, is the time of its evaluation.
- Grafana: the root policy's matchers are ignored, and a rule that names its own
  contact point (`notification_settings`) isn't sent there directly: it is
  routed by the policies like the others.
- Grafana-managed alerting has no inhibition rules that Lapilli reads.
- `lapilli verdict` leaves two of the model's inputs that exist to hold it to a
  recording (when Alertmanager's pipeline first let a flush through, and how late
  its timers run) at zero.
- A configuration of more than 100,000 groups in a period stops at the first
  100,000, and the report says so (`notifications.stopped`).
- `lapilli serve` doesn't work these out yet: it keeps the notifications that were
  sent, where it is told of them.

## How it is checked

The model is held to what Alertmanager really did, as the replay is held to
what Prometheus really fired ([Is the replay right?](check-replay.md)). Today
that is a test over the recordings of Alertmanager 0.33.1, 0.34.2 and 0.35.0
(their configuration, the alerts sent, and the notifications each receiver
really got): given the same alerts at the same times, the model gives the same
notifications to each receiver, with the same reasons. A run against a real
Prometheus and Alertmanager, in CI and weekly, comes next.
