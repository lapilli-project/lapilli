# Who a firing reached: the alert manager's routing, worked out

Status: proposed with its implementation; the part for `lapilli verdict` and the part for
`lapilli serve` are built ("As built"). Under
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
of an hour sends eight notifications, which on a Slack channel are eight
messages and on PagerDuty one page (the repeats update the open incident),
and counts as one firing.

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

A notification on an integration of a paging kind is on a **paging
integration**: Alertmanager's `pagerduty`, `opsgenie`, `victorops`,
`incidentio` and `pushover`, and Grafana's `pagerduty`, `opsgenie`,
`victorops`, `pushover` and `oncall`. Whether it is a **page** depends on
the tool: Alertmanager sends the hash of the group's key as PagerDuty's
`dedup_key` (`incident_key` on the older API), Opsgenie's `alias` and
VictorOps' `entity_id`, and incident.io the key itself, so a later
notification of the group updates the incident that is open instead of
opening another. On those kinds a page is the notification that opens it:
reason `first`, which is the group's first, or its first after all its
alerts resolved. The repeats and the notifications of new alerts in the group
are on a paging integration and not pages. Pushover rings for each
notification, and so does a receiver `--paging-receivers` matches, whatever
its kinds. Both numbers are shown: `paged`, and `on_paging` for
the notifications on paging integrations. (A group whose resolve was never
sent, `send_resolved: false`, leaves the incident open, and the next
firing updates it: counted as a page here, so the count is the most.)
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
The end-to-end run is `test/e2e/routing.sh`; `lapilli serve` is the next section.

- **Flags.** `--notifications` is on by itself when `--alertmanager-config` or
  `--grafana` is given, and `--notifications=false` turns it off; `--external-labels
  NAME=VALUE` (repeatable), `--silences FILE`, `--paging-receivers REGEX` need one of
  those two (or are refused). `--alertmanager-config` also goes with a verdict run now,
  not with `--check-content` alone; the configuration is read once, before anything
  is replayed, and one that can't be read fails the run. Grafana's policies are read
  because `--grafana` was given; if they can't be, the run says so on stderr and goes
  on without them. `lapilli serve` takes the four flags too, as built below.
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
  weren't worked out; in `lapilli serve`, as built below (not known, `pages_not_yet`, only where
  it isn't given the configuration).

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

## As built: `lapilli serve`, and Grafana's simplified routing

What was decided building the second part, in `lapilli serve`
([notifications-worked-out.md](../notifications-worked-out.md#in-lapilli-serve) says it for a reader).

- **When.** After each run records its firings (`runs`, right after `Record`), and once
  before the first run for a ledger that has firings and began to work them out since
  (`catchUpRouted`: the days it has, back to `--backfill` and at most the twelve weeks
  shown, a day at a time). Judging an hour again is the same code: the cursor goes back
  to where a version of the rules took effect and the run works the hours out again. It
  reads the firings from the ledger (`FiringsOverlapping`), never the history, so it
  costs no query.
- **The hours and the lead-in.** The run's `from` to its `to`, with the firings that were
  on in the lead-in before `from` taken in as alerts: the longest `repeat_interval`
  of the routes, clamped to 1 hour to 24 hours. A firing that began before the
  lead-in is started an hour before it (`routedClamp`, more than any `group_wait`
  there should be) and is `Carried`, and the run's `Options.From` is the lead-in's start: its
  group's first flush is before `From`, so the group is taken as notified and has no `first`
  (a page on PagerDuty) at the lead-in. A repeat is counted from the group's last notification
  kept before the lead-in (`ledger.RoutedNotifiedBefore`, `Options.Notified`, looked for
  `repeat_interval + group_interval` back), or from the lead-in's start where none is
  kept. What the lead-in adds is the group's other alerts and the alerts that inhibit it.
  The notifications from `from` on replace those kept from there. What a firing reached
  is counted from all the notifications kept for it (`firing_routed_sent`, one row for
  each firing and notification, replaced for the hours a run works out and no others), never
  from one run's view: a firing on for a month, worked out by hourly runs, has what one run
  over the month works out (`TestRoutedFiringForAMonth`). Nothing is counted when it is
  written: `firing_routed` keeps only what a run says besides (grouped, the receivers that send
  nothing, the mutes), and the counts are made when read, from the rows of the hours worked
  out (`RoutedOf`; pages by hour, `RoutedPages`), so no missing row reads as none. A firing
  that needs an hour that isn't worked out (tried and not worked out, or not tried yet: the
  catch-up goes oldest first, 14 days a round) has no totals (not known), and its week is not
  known for a budget (`RoutedPagesByWeek`), whichever the reason: it needs every hour from its
  start to its end, or, one that is on, to where the ledger's firings are up to (`upTo`; the
  cover is read with the rows, in one transaction, so a write between can't make a firing known
  with too few pages). Its
  pages in the hours that were worked out count where they went: one hour not worked out makes
  that hour's week not known and no other (`TestUnknownHourKeepsLaterWeeksOfALongFiring`,
  `TestHourlyRunsWithOneFailedHour`): the repeats after a hole stay on their schedule because
  a run that finds hours of its look-back not worked out looks back as far before the first of
  them, and takes a group as having repeated on its route's `repeat_interval` plus `group_interval`
  through them (`notifiedBefore`), with a caveat on the hours it works out (a stretch longer than
  the look-back that the look-back begins in has its own, saying the counts assume that schedule and
  that a group with no notification kept is first notified at the hours' start); without it the phase
  of a group's repeats moved at the first run after the hole, and went on moving for good.
  `KeepRouted` reads no notification kept before: only the
  `firing_routed` rows of the firings it is given, 400 to a query. A clamped firing, one that
  began more than `routedClamp` before the lead-in, is the only one `Carried`: one that began
  within the hour before it is first notified when its group first flushes. The repeats are those of the kept
  notifications' times: they drift by seconds from the real ones where the flush grid of
  the lead-in's start isn't the group's own. At most 300,000 firings go in at once.
- **Bounds and the cover (a review's fixes).** A run is given `routedDeadline` (2 minutes)
  and `routedBudget` (20,000,000 work units, as `routing.DefaultBudget`: a notification 50,
  a flush 1 and the alerts it holds, an alert in a notification 1, a silence,
  an inhibition or a regular expression matched 1 and more for a long value), through
  `RunContext`. The firings of the run's own hours come first within the
  300,000 (`FiringsOverlapping` takes `own`): more in the hours themselves is "not
  worked out"; more only in the lead-in is worked out, with the caveat kept. A run that
  stops, fails, can't read the configuration, or is over re-recorded firings (or
  `--notifications` was off) records its hours as **not worked out** (`routed_cover`,
  `ok` 0, why whole), dropping what was kept of them; the catch-up (`catchUpRouted`) works
  out what isn't covered, up to 14 days a round and 2 minutes in all (`routedCatchUpDeadline`,
  across the pieces: a configuration that holds the model up holds the run loop that long
  and no longer); a piece that didn't come out is tried again after an hour, then two, four,
  doubling to a day (`routedTry`); a piece that the shared 2 minutes cut off isn't one that
  didn't come out, and is tried again at the next round, but only when it began with less than
  its own 2 minutes left: the oldest piece begins with the whole round, and when it runs out of
  its own time the round has run out with it, so it backs off as a failure (else every round
  would spend its 2 minutes on it and no later gap would be tried;
  `TestCatchUpSlowOldestPieceDoesNotBlockLaterGap`).
  `KeepRouted` replaces `at` between `from` and `to` only; marking hours not worked out also drops
  what `firing_routed` says of the firings on in them (their mutes and receivers that send
  nothing), so a firing recorded again isn't shown with what was said of it before; `routed_cover`
  has an index on `to_at` for the overlap lookups, which failed stretches piling up would make
  dear. A view reads at most 500,000 pages (`routedPageRows`), the earliest first: where there
  are more, no span reaching the time of the last one read is known (`paged.routedCut`), never
  a number counted short (`TestPagesPastTheRowLimitAreNotKnown`). Every page asks the cover:
  `paged.known(from, to)` is true when the hours from where counting starts to where the
  ledger's firings are all are known for each receiver that pages (`pagingReceivers`): the hours
  before the first one tried are what was recorded (the recording is what was sent); in an hour
  after it, a receiver that has notifications recorded in that hour is known as recorded; otherwise
  it is known as worked out if the hour is covered; otherwise, for a receiver that pages, the hour is
  not known, whatever another receiver recorded in it (a Watchdog's receiver recorded every hour
  makes no hour known for the receivers that page for firings; `TestRecordedByAnotherReceiverDoesNotMakeAnHourKnown`);
  where the configuration isn't read, which receivers page isn't known, so such an hour isn't; a week that isn't is "not known"
  (`paged.unknown`, `PagedUnknown`, expectations' `pages_not_covered`), and the first,
  counted from part-way, is partial (`counted_from`). The configuration is read from the
  pages when none is kept (`routingForView`), cached by the hash of its text, so replicas
  agree. `PruneKeeping` deletes `routed` in batches of 5,000 and trims the cover.
- **Recorded pages.** A recorded notification is a page by the model's own rule
  (`routing.PageRule`, shared): where the integration's kind `DedupsByGroup` only when its
  reason is first, unless `--paging-receivers` matches the receiver, which pages for each;
  a worked-out page is dropped where its receiver has a notification recorded in the hour it
  went in or in the hour of the minute before or after it (`recordedNear`), so a page just
  before an hour and its copy just after count once; recorded or worked out is decided for each receiver
  and hour (`RecordedHours`). A mute's silence is tied to the ledger firing it is of and matches
  on its labels and `alertname`; an inhibition's mute keeps `source_rule`.
- **Agreement.** Worked-out rows are for each integration; a receiver recorded by its bare
  name (a webhook port) is compared with its webhook integrations only, one recorded as
  `receiver/kind` with all; receivers with only recorded notifications are counted apart
  (`RecordedOnly`).
- **Schema 035.** `routed`: one row for each notification the model worked out, on each
  integration (`at`, `receiver`, `kind`, `page`, `reason`, `grp`, `firings`: how many
  firings it told of, 0 for one that told of resolves alone); `firing_routed_sent`: one
  row for each firing and notification that told of it (`firing_id`, `at`, `receiver`, `kind`,
  `page`), replaced for the hours a run works out, from which every count of a firing's
  notifications and pages is made when read (`PageAts` of `RoutedOf` is the first 200
  page times, to show, and no number is counted from it); `firing_routed`: one
  row for each firing a run saw, `ON DELETE CASCADE` with its firing (`grouped`;
  `receivers`: a JSON list of {receiver, empty} for those that send nothing;
  `muted`: {kind, by, source_rule}), kept when hours of its life aren't worked out. `routed_cover` (`from_at`, `to_at`, `ok`, `why`): the
  stretches tried, none overlapping, adjacent alike ones joined. A firing some of whose hours were tried and not worked out has no totals,
  which a page says as "not known", never as none; the pages budget is not known for a week in
  which such a firing started (`RoutedPagesByWeek`).
- **Silences from mutes.** A mute the event recorder told of, of kind `silence` (an
  inhibition is worked out, not read), is a silence of the alert's labels from ten
  seconds before the first of an hour's mutes to ten seconds after the last: the model's
  flush is a moment before Alertmanager's, and the recorder tells of a mute only as
  Alertmanager asks. An Alertmanager's first flush after it starts waits for its cluster
  to settle, which the model doesn't know: on the recordings the one flush before the
  first mute told of is notified, which the tests allow.
- **Held to what was sent.** A page is recorded where its receiver has recorded
  notifications from the hour of the page on, and worked out otherwise; the two are never
  added for the same receiver and hour. `lapilli_notifications_agreement` is, over the
  receivers with both, the sum of the smaller of the worked-out and recorded counts over
  the sum of the larger; a receiver is counted from the hour its first was recorded, a
  recorded name is the receiver's (an event's `receiver/kind[n]` cut to the configured
  receiver), and a notification counts on each integration, as the recorder posts it.
  The recordings of 0.34.2 and 0.35.0 give 0.75 or more, which `TestRoutedAgreesWithRecordings`
  holds.
- **Departures.** The pages budget counts pages by the week their firing started, the
  worked-out ones alone (the recorded ones aren't tied to a firing's week by page), and is
  not known for a week the hours worked out don't cover. A recorded page is for nothing
  when its firing caught nothing; usefulness (`--responses`) decides it for the pages worked
  out. `/api/v1/report` gives what was worked out even where notifications were recorded;
  the recorded ones are the pages', the Notifications page's and the summary's. The summary's
  `paged` and the pages cover the twelve weeks shown.
- **Grafana's simplified routing.** A Grafana rule that names its contact point
  (`notification_settings.receiver`, `Rule.Receiver`) is sent there, past the policy tree:
  Grafana's documentation says "All notifications for this alert rule are sent to this
  contact point automatically and notification policies aren't used"
  ([Create a Grafana-managed alert rule](https://grafana.com/docs/grafana/latest/alerting/alerting-rules/create-grafana-managed-rule/),
  Configure notifications), and that the settings can add "grouping or timing options"
  ([Configure notifications](https://grafana.com/docs/grafana/latest/alerting/configure-notifications/)).
  `routing.Alert.Own` carries the rule's `group_by`, `group_wait`, `group_interval`,
  `repeat_interval` and mute and active timings (read from the provisioning API's and
  Terraform's `notification_settings`: `rules.Rule.Notify`); the model makes one route
  for each distinct set of them, after the tree's, so rules with the same settings share
  a group. What the settings leave out is not read from the docs but from Grafana's code
  and is stated here as unverified against a live Grafana: grouping by `grafana_folder`
  and `alertname`, and the root policy's timings, as the generated route inherits
  them. A named contact point Grafana's don't have, or a time interval it doesn't hold,
  falls back to the policies. Tested in `internal/routing/own_test.go` and
  `internal/verdict`.

## Questions for review

1. Webhooks are messages unless `--paging-receivers` says otherwise.
2. Grafana-managed alerting has no inhibition rules that Lapilli knows
   of; if its API gives them, they are read.
3. An Alertmanager restart resets its groups; the model doesn't. Worth
   reading restarts from the history (`process_start_time_seconds` of the
   alert manager), or not?
4. The oldest Alertmanager the end-to-end run holds the model to: the docs name
   none for the model, so it is 0.27.0, an older release than the recordings'
   (0.33.1 and after) that has the matchers and time intervals the model reads.

## Sources

- Alertmanager's configuration, routes, inhibition and time intervals:
  https://prometheus.io/docs/alerting/latest/configuration/
- Alertmanager's dispatcher, inhibitor and notification pipeline:
  `dispatch/dispatch.go`, `inhibit/inhibit.go`, `notify/notify.go` and
  `config/notifiers.go` in https://github.com/prometheus/alertmanager
- Alertmanager's silences API: https://github.com/prometheus/alertmanager/blob/main/api/v2/openapi.yaml
- Grafana's notification policies, contact points and mute timings:
  https://grafana.com/docs/grafana/latest/alerting/set-up/provision-alerting-resources/http-api-provisioning/

## As built: after review

What the first review found, and what changed:

- **Pages.** Repeats aren't pages on PagerDuty, Opsgenie, VictorOps or
  incident.io (above); `Notification.Paging` is "on a paging integration",
  `Notification.Page` "rings a phone", `routing.DedupsByGroup(kind)` says
  which kinds keep an open incident for a group, and `serve` applies the
  same rule to the notifications it recorded.
- **Grafana's own group_by** is normalized as Grafana does
  (`NormalizedGroupBy`): `grafana_folder` and `alertname` always, any list
  with `...` is `[...]`; a rule's timings are `*time.Duration`, so an
  explicit `0s` group_wait is no wait.
- **Bounds** (the configuration reaches serve from Mimir tenants and Grafana
  users, re-read hourly): a work budget (`Options.Budget`, `DefaultBudget`
  20,000,000 units: a notification 50, a flush 1 and the alerts it holds, each
  alert put into a notification 1, an inhibition or silence match 1), and
  `RunContext`'s context; either ends the run with `Result.Stopped`, which is
  *not known*, not zero. A timing under one second is refused for an
  Alertmanager file ("Alertmanager would accept it, Lapilli doesn't model
  it"; a `group_wait` of 0 is fine) and read as one second, with a warning, for
  a Grafana's (which accepts `1ns`). Time zones are loaded once by name
  (cache of 1,024) and are one zone by name; the time intervals list 20,000
  ranges, weekdays, days, months and years in all; a time interval's change is
  looked for 400 days ahead at most. Silences are found by their `=` matchers
  instead of trying each, and a run takes 100,000 (a file of 100,000 is read and worked out;
  serve takes the recorded mutes that touch the run's hours and the lead-in, and when those and the
  file's are more than that the hours are "not known" with the reason). Matching them is charged to the budget: each
  silence a label set could match, and each time a flush asks which mutes it and when that changes. A matcher's name and value are
  4 KB at most, an interval's and an inhibition rule's names 1,024 bytes, a
  route's key (its matchers and its ancestors') 64 KB; a regular expression is
  compiled through `regexp/syntax` and refused over 10,000 instructions, 500,000
  in all for a configuration and for a silences file. A notification's `Group` is
  the route's short key (its place among its siblings at each level, `0.2.1`) and
  the group's labels, those cut at 1,000 bytes ending in a hash;
  `Config.GroupKey` writes it as Alertmanager does. `By` is 1,024 bytes at most;
  a route's interval lists lose their repeats.
- **A firing on since before the period** (the report clamps its start to the
  period's) is `Carried`: taken as notified at the period's start, its group's first
  flush isn't a `first`, and a repeat is due a `repeat_interval` after the start. For any
  other alert `Options.From` is exact: a group whose first flush (its first alert's start and
  `group_wait`) is at `From` or after isn't notified yet. `Page` is for a notification with an
  alert firing: one of resolved alerts alone rings no one. Grafana OnCall/IRM isn't in
  `DedupsByGroup`: the pages it reads document how an alert group is made (by a
  grouping template) but not that a later notification from Alertmanager or Grafana
  updates the group's page instead of making a new one, so each notification is a page.
- **`--changed-only`'s rules left alone** are those each side's own routing needs
  (`--alertmanager-config-before`'s for the before side).
- **Inhibited** counts a firing never notified whose every flush was muted by
  inhibition; `inhibited_by` weighs each once.
