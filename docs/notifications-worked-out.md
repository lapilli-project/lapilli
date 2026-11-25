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
| `--alertmanager-config-before FILE` | with `--compare`, the configuration as it was before the change, which the before side is worked out with (the one `--alertmanager-config` names otherwise), so a change to the routing shows in the pages. A file, not a URL; a pull request's backtest recipe passes the base branch's with `ALERTMANAGER_CONFIG` |
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

A notification on an integration of a paging kind is on a **paging
integration**: Alertmanager's `pagerduty`, `opsgenie`, `victorops`, `incidentio`
and `pushover`, and Grafana's `pagerduty`, `opsgenie`, `victorops`, `pushover` and
`oncall`. Every other kind (Slack, email, a webhook, Teams, Discord, Telegram,
SNS, Webex, WeChat, Jira) is a **message**. A webhook may well be a pager
(Grafana IRM takes alerts by webhook), and Lapilli can't tell;
`--paging-receivers` says so. (incident.io is Alertmanager's own integration,
`incidentio_configs`, and a paging kind.) The report names the receivers it took
as paging, and why: by the kind of an integration, by `--paging-receivers`, or
both.

Not every notification on a paging integration rings a phone. Alertmanager sends
the hash of a group's key as PagerDuty's `dedup_key`, Opsgenie's `alias` and
VictorOps' `entity_id`, and incident.io the key itself, so a later notification
of the same group updates the incident that is open. On those kinds a **page** is
the notification that opens one: the group's first, or its first after all its
alerts resolved (reason `first`). A repeat, or a notification of new alerts in the
group, is on a paging integration and not a page. Pushover, and every receiver
`--paging-receivers` matches, ring for each notification. A report shows both:
`PAGED` (and `paged`) and the notifications on a paging integration
(`on_paging`). If a receiver doesn't send resolves (`send_resolved: false`), the
incident stays open and the next firing updates it, which still counts as a page
here: the count is then the most.

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

## A Grafana rule that names its contact point

In Grafana, an alert rule can say where it goes itself ("Configure notification
settings" on the rule, `notification_settings` in the provisioning API and in
Terraform): a contact point, and optionally its own `group_by`, `group_wait`,
`group_interval`, `repeat_interval` and mute and active timings. Grafana's
documentation says of it: "All notifications for this alert rule are sent to
this contact point automatically and notification policies aren't used"
([Create a Grafana-managed alert rule](https://grafana.com/docs/grafana/latest/alerting/alerting-rules/create-grafana-managed-rule/),
Configure notifications).

Lapilli routes such a rule's alerts that way, in `lapilli verdict` and in
`lapilli serve`: they go to that contact point alone, never through the policy
tree, grouped by the rule's `group_by` (by `grafana_folder` and `alertname`
when it gives none) and with its timings, those it leaves out being the root
policy's, as Grafana's own generated route inherits them. Rules with the
same settings share a group. A rule that names a contact point Grafana's
contact points don't have, or a timing it can't read, is routed by the
policies. What it names and sets is read from the rule, never from the
notification policies.

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
- A `repeat_interval` that is a multiple of `group_interval` (Alertmanager's
  defaults, 4h and 5m, are) puts the repeat on the edge: Alertmanager repeats at
  the flush exactly that long after the last notification or at the next, which a
  few milliseconds decide (against real Alertmanagers 0.27.0 and 0.35.1, 17 of 28
  repeats were at the first and 11 at the next, at random). The model takes the
  next, so a long-lived alert may be worked out one notification fewer, a
  `group_interval` late, than it really got.
- A cluster of Alertmanagers sends once. An integration that fails and retries
  sends once. The second each alert arrived, which Prometheus sets by its
  evaluation, is the time of its evaluation.
- Grafana: the root policy's matchers are ignored. A rule that names its own
  contact point (`notification_settings`) is sent there directly, with its own
  grouping and timings, past the policies, as Grafana does (below).
- Grafana-managed alerting has no inhibition rules that Lapilli reads.
- `lapilli verdict` leaves two of the model's inputs that exist to hold it to a
  recording (when Alertmanager's pipeline first let a flush through, and how late
  its timers run) at zero.
- A firing on since before the period began (its start is the period's) is taken
  as notified at the period's start: its group's first flush isn't a first
  notification, and a repeat is due a `repeat_interval` after the start. Whether
  it really was, and when, isn't known.
- `alertname` is always the rule's name, as Prometheus sets it. A rule's label
  that is a template (`{{ $labels.x }}`) is taken as written: Lapilli has no
  expander for the labels of a replayed series, so a route that matches on what
  the template makes isn't followed, and a `group_by` on it groups by the text.
- Bounds, against a configuration that reaches `lapilli serve` from a tenant or a
  user: a run spends at most 20,000,000 work units (a notification is 50, a flush
  1 and each alert it holds, each alert put in a notification 1) and stops when
  it has, or its context ends; more than 100,000 groups, or more than 20,000
  silences (an error), stops it too. A run that stopped says why
  (`notifications.stopped`, and `known: false`): what it counted is of part of the
  period, and every surface says **not known** instead of a number, a budget of
  pages is neither held nor broken, and the report has no `*_worked_out` metrics.
- An Alertmanager configuration with a `group_wait` over 0 and under a second, or
  a `group_interval` or `repeat_interval` under a second, is refused: Alertmanager
  would accept it, and the model works out a flush each interval. A Grafana's
  (notification policies and a rule's `notification_settings` accept `1ns`) is
  read as one second, with a warning in the report (`notifications.warnings`).
- A configuration has at most 5,000 time intervals and 20,000 ranges, weekdays,
  days, months and years in all, a matcher's name and value are 4 KB at most, a
  time interval's or inhibition rule's name 1,024 bytes, and a regular expression
  compiles to 10,000 instructions at most (500,000 in all, for a configuration and
  for a silences file); more is an error that says so.
- `lapilli serve` works them out for each run's hours, from the ledger's
  firings ([below](#in-lapilli-serve)); the departures above hold for it too.

## How it is checked

The model is held to what Alertmanager really did, as the replay is held to
what Prometheus really fired ([Is the replay right?](check-replay.md)), in two
ways:

- A test over the recordings of Alertmanager 0.33.1, 0.34.2 and 0.35.0 (their
  configuration, the alerts sent, and the notifications each receiver really
  got): given the same alerts at the same times, the model gives the same
  notifications to each receiver, with the same reasons.
- A run against real servers, in CI and weekly
  ([test/e2e/routing.sh](../test/e2e/routing.sh), CI's `e2e-routing`): a real
  Prometheus sends the alerts of rules that fire and resolve at known times to
  two real Alertmanagers, the newest release and 0.27.0 (the docs name no
  oldest the model is held to; this is the oldest tried), with `group_wait` 5s,
  `group_interval` 15s and `repeat_interval` 50s, nested routes with `continue`,
  `group_by` (two rules in one group), an inhibition rule, a mute time interval,
  a silence made through the API, a receiver with two integrations and one that
  sends no resolves, every receiver a webhook of one keeper that keeps what it
  gets and when. `lapilli verdict --alertmanager-config --silences` replays the
  same rules over that Prometheus's history, and each integration's
  notifications as worked out must be the ones it got: the same number, each
  within 4s of its time (a `group_interval` is 15s, so a model a flush off
  fails), the same status (firing or resolved) and the same alerts, so an alert
  muted by an inhibition rule, a silence or a time interval is muted in both. The
  report's own notified and paged counts are held to them too, and so is each
  firing's own list of notifications (`notifications.sent`), paired with the
  real ones. It prints each notification of both sides,
  and fails on any difference.

## In lapilli serve

`lapilli serve` works the notifications out as well, beside the ones it is told
were sent. Given `--alertmanager-config` (a file, or Mimir's or Cortex's API) or
`--grafana`, with the same `--external-labels`, `--silences` and
`--paging-receivers` ([the chart's `notifications` values](../charts/lapilli/README.md)),
it reads the routing configuration once an hour (a configuration that can't be
read leaves the last one in use, and the log says why once) and, after each run
records its firings, works out the notifications of the hours the run judged:

- **From what is in the ledger**, so judging an hour again works the same out
  again without asking Prometheus anything. All rules' firings go in together,
  as a group is of many rules, with a lead-in before the hours: the longest
  `repeat_interval` of the routes, at least an hour and at most a day, so that a
  group that began before them, and an alert that inhibits one in them, is
  there. A firing still on is taken in whole, from its start, so a long firing's
  repeats are counted in full; one that began before the lead-in and whose group
  began with an alert that ended before it has the group's timer as if it began
  with it. At most 300,000 firings go in at once; past that the Notifications page
  says it stopped.
- **Silences** are `--silences` (read again each run) and the mutes the
  event recorder told of ([Notifications as they were sent](notifications.md)):
  an alert a silence muted is a silence of its labels from ten seconds before
  the first mute of an hour to ten seconds after the last, which is how long it
  is known to have held. Inhibition is worked out, not taken from the mutes.
- **Kept for each firing**: the notifications that told of it and the pages
  among them, when the first page went and when each did and to which receiver, the
  receivers and integration kinds it reached and which of them send nothing, and
  what muted it. Each notification is kept too, once for each integration. A
  firing the ledger forgets takes its numbers with it.

**Held to what was sent.** Where notifications were really recorded, the
recorded ones count, and the ones worked out are compared with them. A recorded
page is a recorded notification to a receiver the configuration says pages: for an
event the alert manager's recorder posts, it names the integration, so it is
that kind's; for a webhook port, the receiver's, which pages when any of its
integrations does or `--paging-receivers` matches. A page worked out is counted
only where its receiver has no recorded notification from the hour of its page
on. So a number of pages is recorded where there are records and worked out
where there are none, and every page and API that shows it says which:
"recorded", "worked out" or "recorded and worked out". As in `lapilli verdict`, a
rule's pages count a notification for each firing of it that the notification told of
(for a recorded page, once for the rule), so the rules' pages can add up to more than the
notifications sent: the Notifications page and `lapilli_notifications_agreement` count
each notification once.

The Notifications page, in the sidebar when the notifications are worked out,
shows for each receiver the notifications worked out and those recorded, over the
hours that have both, counted from the hour a receiver's first was recorded,
and for those with none recorded, what was worked out. `lapilli_notifications_agreement`
is, over the receivers that have both, the sum of the smaller of the two counts
over the sum of the larger: 1 is the same number for every receiver. It is not
a measure of timing, only of counts, and counts a notification on each of a
receiver's integrations, as the recorder does. Alertmanager's own delivery takes
seconds the model doesn't know of, and its first flush after a start waits for its
cluster to settle: on the recordings in `internal/serve/testdata/events` it is
0.75 or more, which the tests hold it to.

**Where it shows**, each saying whether what it counts is recorded or worked out:
the rule page (a Pages section and the Human cost panel), the rules list, the
overview's "Woken for nothing" (pages for nothing while people slept, not every
notification), `/api/v1/summary` (`paged` on each row), goals (`pages`,
`pages.asleep`, `pages.nothing`), expectations (a budget of `N pages`, counted by
the pages worked out of the firings that started in each week, and not known for a
week the hours worked out don't cover), the weekly digest and the reports
that carry it, MCP's rule and firing tools (`pages`, `worked_out`), a firing's
line ("worked out", never a bare number) and `/api/v1/report` in the fields
`lapilli verdict` gives (`notifications` on the report, each rule and each
firing, `worked_out: true`; with `format=csv` its `worked_out_*` columns). A page
for nothing is one for a firing that caught nothing, or, with
[responses](responses.md), one that wasn't useful: of the pages worked out; a page
recorded is for nothing when its firing caught nothing.

Past what a run covers (before `--backfill`, before the first start with the
configuration, or for hours judged while the configuration couldn't be read),
nothing is worked out and the pages say "not known", never none.
