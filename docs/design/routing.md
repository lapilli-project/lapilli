# Who a firing reached: the alert manager's routing, worked out

Status: proposed with its implementation. Under
[GOVERNANCE.md](../../GOVERNANCE.md) this is a new kind of number, worked
out rather than recorded, and a new kind of data in the ledger, so it is a
design; with one maintainer it is merged with the code that builds it,
sooner than a week, and this note says so. "Questions for review" at the
end are open.

## Why

Lapilli counts firings. A firing isn't a notification, and a notification
isn't a page:

- the route tree sends it to one receiver, several (`continue`), or one
  that sends nothing;
- `group_by` puts it in one notification with others, `group_wait` holds
  it, `group_interval` batches what changes, and `repeat_interval` sends it
  again while it fires;
- an inhibition rule mutes it while another alert fires, a silence while
  someone said so, a time interval at night or out of hours;
- a receiver's integrations decide whether a phone rings (PagerDuty,
  Opsgenie) or a channel gets a message (Slack, email).

So the numbers that matter most to an on-call engineer come out wrong
both ways. A rule that a critical rule always inhibits looks noisy and
wakes no one; thirty rules grouped into one notification are one page,
not thirty; a rule that stays on for eight hours with a `repeat_interval`
of an hour pages eight times, and counts as one firing.

`lapilli serve` keeps the notifications really sent, from the day the
alert manager starts copying them to it
([notifications.md](notifications.md)). Before that day, in `lapilli
verdict`, and for a change not yet made (`--compare`, a pull request's
backtest), nothing is known. notifications.md put this aside as "a model,
not a record. It could come later, checked against what this ledger
records." This is that model, and the check.

## What is built

### What it reads

`--alertmanager-config` already reads a file (and Mimir's, Cortex's or
Amazon's format) or Mimir's and Cortex's API, for the content check
([content.md](content.md), "Alertmanager's templates"); Grafana's policies
and contact points are read for the same ([serveops.md](serveops.md)).
That reader now keeps what routing needs as well:

- each route's `group_by` (with `...`), `group_wait`, `group_interval`,
  `repeat_interval`, `mute_time_intervals` and `active_time_intervals`,
  inherited down the tree as Alertmanager inherits them, with its
  defaults (30s, 5m, 4h);
- the time intervals they name (`time_intervals`, and the older top-level
  `mute_time_intervals`): times, weekdays, days of the month, months,
  years and location;
- `inhibit_rules`: `source_matchers`, `target_matchers`, `equal`, and the
  older `source_match`, `source_match_re`, `target_match` and
  `target_match_re`;
- each receiver's integrations: their kind, and whether each sends
  resolves (`send_resolved`, defaulting per kind as Alertmanager's own
  configuration defaults do).

Grafana's: a policy's grouping and timings and its mute and active timings
(`/api/v1/provisioning/mute-timings`), and each contact point's `type` and
`disableResolveMessage`.

Still no field that holds a secret or a connection's address is parsed:
only names, kinds, matchers, durations and booleans.

Two more inputs, both optional:

- `--external-labels NAME=VALUE` (repeatable): the labels Prometheus adds
  to each alert as it sends it (`global.external_labels`), which routes
  often match on (`cluster`, `env`). The chart passes its Prometheus's.
- `--silences FILE`: silences as Alertmanager's API lists them (`GET
  /api/v2/silences`, or `amtool silence query -o json`), expired ones
  included, which it keeps for `--data.retention` (five days by
  default). Only matchers, start and end are read; not the comment, not
  who made it.

### The alerts it is given

Each replayed firing is an alert: `alertname`, the rule's labels, its
series' labels and the external labels, from the evaluation it fired on to
the one it resolved on. A Grafana rule's carries `grafana_folder`, as its
routing already does. Firings of every rule in the period go in together:
a group is of many rules.

### How it is worked out

As Alertmanager's dispatcher and notification pipeline do, in their order:

1. **Routes.** Each route the alert matches, `continue` going on, the
   deepest matching child first; the root's receiver when no child
   matches.
2. **Groups.** A route's alerts are grouped by their values of its
   `group_by`. A group starts with its first alert; it flushes first after
   `group_wait`, and then every `group_interval` from its start.
3. **A flush.** At each flush, for each integration of the route's
   receiver, a notification goes out when, of the group's alerts not muted:
   one is firing that the last notification didn't have firing; or, the
   integration sending resolves, one resolved that the last had firing; or
   any are firing and `repeat_interval` has passed since the last. A group
   whose alerts have all resolved is gone after the flush that tells of
   it.
4. **Muted** at the flush, before 3: an alert an inhibition rule mutes (an
   alert firing then, not itself, matches the rule's source, and the two
   agree on each `equal` label, absent agreeing with absent); an alert a
   silence matches; and every alert of a route in one of its mute time
   intervals or outside its active ones.

A receiver with several integrations sends one notification on each. A
cluster of Alertmanagers sends once, as the model does.

What isn't worked out: an Alertmanager restart, an integration that fails
and retries, and the second each alert arrived (Prometheus sends it at the
evaluation, and the model takes that time). The flushes are on the
group's own clock, as Alertmanager's are, so a notification's time is
right to the second its group's timing allows; the check below says how
right in practice.

### Which notifications page someone

A notification on an integration of a paging kind is a **page**:
Alertmanager's `pagerduty`, `opsgenie`, `victorops` and `pushover`, and
Grafana's `pagerduty`, `opsgenie`, `victorops`, `pushover` and `oncall`.
Every other kind (Slack, email, a webhook, Teams, Discord, Telegram, SNS,
Webex, WeChat, Jira) is a **message**. A webhook may be a pager (incident.io
and Grafana IRM take alerts by webhook): `--paging-receivers REGEX` makes
every notification of the receivers it matches a page, whatever their kinds
(both commands; the chart's `pagingReceivers`). The report says which
receivers were taken as paging, and why.

### What it gives

When `--alertmanager-config` (or Grafana with its policies) is given,
`--notifications` (on by default then; `--notifications=false` keeps the
report as before):

- **Each firing**: its notifications and pages, when the first page went,
  the receivers it reached, and what muted it, if anything did (the
  inhibiting rule, a silence, a time interval), or that it reached a
  receiver that sends nothing.
- **Each rule**: NOTIFIED and PAGED counts, pages while people slept and
  out of hours (`--sleep-hours`, `--work-hours`, `--on-call` as firings
  are), **pages for nothing** (of firings that caught nothing, or, with
  [responses.md](responses.md), weren't useful), how often it was
  inhibited, and by which rule most, and how many of its firings went out
  in a notification with other rules' alerts.
- **The text report**: a NOTIFICATIONS table after the rules, and the
  "woken for nothing" line counted from pages, saying so.
- **`--compare` and the pull request backtest**: PAGED before and after,
  for each rule and in all, the first line of its summary: a change that
  would ring more phones says so before it is merged. An expectations
  budget ([expectations.md](expectations.md)) takes `pages` beside
  `firings`.
- `--json`, CSV, `--metrics` (`lapilli_rule_notifications_worked_out`,
  `lapilli_rule_pages_worked_out`), `--redact` (receivers aliased), and
  docs/numbers.md, as every number is.

Nothing in the alert manager's configuration is ever changed or proposed:
Lapilli writes alert rules alone (ROADMAP, "Not planned"). The report says
what it found ("inhibited by `NodeDown` in 92% of its firings"); what to do
about it is a person's.

### In lapilli serve: worked out beside what was sent

Each run works out the notifications of the hours it judged, and keeps
them per firing (schema, the next free number when the pull request
opens): receiver, integration kind, notifications, pages, first page,
what muted it. Silences come from the mutes the event recorder told of, as
well as `--silences`.

Where notifications were really sent and recorded, the recorded ones count,
and the worked-out ones are held to them: the Notifications page says, for
the spans that have both, how many notifications each receiver was worked
out to get and how many it got, and `/metrics` gives
`lapilli_notifications_agreement`. Where none were recorded (before the
port was connected, or a receiver that copies nothing to Lapilli), the
worked-out ones count, marked so on every page that shows them.

The rule page, the rules list, the overview's "Woken for nothing", the
summary (`paged`), goals, the digest and MCP take pages as they take
firings, saying which kind each number is.

## Checked against a real Alertmanager

The model is held to what Alertmanager really did, as the replay is held
to what Prometheus really fired ([fidelity.md](fidelity.md)):

- **The recordings** in `internal/serve/testdata/events/` (0.33.1, 0.34.2,
  0.35.0, with their configuration and the alerts sent): the model, given
  the same alerts at the same times, gives the same deliveries to each
  receiver and integration, with the same reasons.
- **An end-to-end run** (`test/e2e/routing.sh`, in CI where the other real
  servers run, and weekly): a real Prometheus and Alertmanager (the latest,
  and the oldest the docs support) with short timings, nested routes with
  `continue`, `group_by`, an inhibition rule, a mute time interval, a
  silence and a receiver of two integrations, a webhook keeping every
  notification. Lapilli replays the rules over that Prometheus's history,
  works out the notifications from the same configuration, and must agree
  on each receiver's notifications, each within one `group_interval` of
  its time.

## Cost and bounds

The routes are bounded as the content check bounds them (2,000 routes, 32
levels, 64 receivers an alert can reach). The model is worked out event by
event, not tick by tick: a group is touched at its alerts' starts and ends
and at its repeats, so a period costs its firings plus its repeats
(at most the period over `repeat_interval`, a group), not its length over
`group_interval`. 100,000 groups a period at most; past that the section
says it stopped, and why. Matchers' regular expressions are Go's, which
run in linear time.

## Attacker's look

- **The configuration** is whoever runs Lapilli's, or the ruler's API
  with its token, as today. A route with a regular expression of any
  length is compiled once, bounded by the size Lapilli reads.
- **The silences file** is whoever runs Lapilli's: its comments and
  authors aren't read, so they appear nowhere.
- **Worked-out numbers aren't records.** Every page that shows them says
  so, and where records exist they count. A wrong configuration can make a
  rule look as if it pages more or less; it changes no stone and no
  verdict, and a change to a rule still needs a person's approval.

## As built: `lapilli verdict`, `--compare` and expectations

What was decided building the first part, where the text above left it open.
`lapilli serve` and the end-to-end run come next.

- **Flags.** `--notifications` is on by itself when `--alertmanager-config` or
  `--grafana` is given, and `--notifications=false` turns it off; `--external-labels
  NAME=VALUE` (repeatable), `--silences FILE`, `--paging-receivers REGEX` need one of
  those two (or are refused). `--alertmanager-config` also goes with a verdict run now,
  not with `--check-content` alone; the configuration is read once, before anything
  is replayed, and one that can't be read fails the run. Grafana's policies are read
  because `--grafana` was given; if they can't be, the run says so on stderr and goes
  on without them. `lapilli serve` refuses the four flags until it works the
  notifications out.
- **The alerts** are the report's firings, each with `alertname` (the rule's name as
  written, before a rule was told apart from another), its labels, and for a Grafana
  rule `grafana_folder`; the end is the firing's end, or the period's end. The
  model's `Settled` and `TimerLate`, which exist only to hold it to a recording, are
  left zero.
- **JSON.** A firing has `notifications` `{notified, paged, first_page, receivers,
  no_integrations, muted: [{kind, by, source_rule}]}`; a rule has `notifications`
  `{notified, paged, paged_asleep, paged_off_hours, paged_for_nothing,
  paged_for_nothing_asleep, paged_for_nothing_off_hours, inhibited, inhibited_by,
  grouped}`; the report has `notifications` `{worked_out: true, notified, paged,
  groups, receivers: [{name, kinds, paging, why}], stopped}`. CSV columns
  `worked_out_notified`, `worked_out_paged`, `worked_out_first_page`,
  `worked_out_receivers`, `worked_out_muted`, after the others. The comparison's
  summary has `notifications` `{worked_out, paged_before, paged_after,
  notified_before, notified_after}`, left out when nothing was worked out, so a
  summary without them is what it was.
- **Counting.** A rule's `notified` and `paged` count each notification for each of
  its firings it told of, so they add up to more than the report's, which counts a
  notification once. Pages while people slept and out of hours are counted at the
  time the page went, by the hours firings are counted by. A page is for nothing when
  its firing is black, or, with responses, when its `useful` is false. The receivers
  listed are those that page and those the firings reached.
- **`--compare`.** Each side's report works out its own notifications, from that
  side's firings, so a change that adds a rule shows the groups it joins; with
  `--changed-only` only the changed rules' firings are replayed, and the pages are of
  those alone. The first line of the text, and the line under the Markdown's heading,
  is `Pages 12 → 15`, and PAGED before → after is a column of the table of rules.
- **Expectations.** `pages` is a budget beside `week`, `asleep` and `refires` (a rule's
  annotation takes `N pages`): the most a week the firings' pages come to, counted by
  the week of the firing, as `week` counts firings; not known where the notifications
  weren't worked out; in `lapilli serve`, not known (`pages_not_yet`).

Where the model departs from Alertmanager, and what the first build of it found:

- Flushes are `group_wait` after the group's first alert, then each `group_interval`
  after the previous timer, and a repeat needs `repeat_interval` to have strictly passed
  (`ts < now - repeat`).
- A route's mute and active time intervals aren't inherited by its children, as
  Alertmanager's aren't.
- A flush that the silences and inhibitions empty logs nothing.
- The notification log's expiry, a restart of Alertmanager, and the shortcut for an
  alert whose start is old aren't modelled.
- Grafana's root policy's matchers are ignored.

## Questions for review

1. Webhooks are messages unless `--paging-receivers` says otherwise.
2. Grafana-managed alerting has no inhibition rules that Lapilli knows
   of; if its API gives them, they are read.
3. An Alertmanager restart resets its groups; the model doesn't. Worth
   reading restarts from the history (`process_start_time_seconds` of the
   alert manager), or not?
4. The oldest Alertmanager the end-to-end run holds the model to.

## Sources

- Alertmanager's configuration, routes, inhibition and time intervals:
  https://prometheus.io/docs/alerting/latest/configuration/
- Alertmanager's dispatcher, inhibitor and notification pipeline:
  `dispatch/dispatch.go`, `inhibit/inhibit.go`, `notify/notify.go` and
  `config/notifiers.go` in https://github.com/prometheus/alertmanager
- Alertmanager's silences API: https://github.com/prometheus/alertmanager/blob/main/api/v2/openapi.yaml
- Grafana's notification policies, contact points and mute timings:
  https://grafana.com/docs/grafana/latest/alerting/set-up/provision-alerting-resources/http-api-provisioning/
