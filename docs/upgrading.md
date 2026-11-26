# Upgrading

`lapilli verdict` keeps no state: each run reads the rules and the history
afresh, and writes only where it is told to (its output, a Pushgateway
group that each push replaces, Slack). Upgrading it is running the new
binary or image; the changelog says which flags and outputs changed.

`lapilli serve` keeps a ledger, and a release may change the ledger's
schema. It upgrades the ledger as it starts, and a release from before the
change can't open it after.

## What happens as serve starts

The schema is a list of numbered files,
[internal/ledger/schema](../internal/ledger/schema), each applied once, in
order; a ledger's schema is the number of the last one applied. serve
applies the ones the ledger hasn't had as it opens it, before it serves a
page or runs anything.

- **SQLite** (`--ledger DIR`, the file `DIR/ledger.db`): the number is
  SQLite's `PRAGMA user_version`. Each file is applied in a transaction of
  its own, which sets `user_version` to its number, so a file that fails
  leaves the ledger at the schema before it, and serve stops, naming the
  file.
- **Postgres** (`--ledger-url-file FILE`): the number is in a table,
  `schema_version`. An empty database is given the schema up to 017 at once
  (a Postgres ledger, from 0.34.0, starts there), then each later file in
  turn, each in a transaction that locks `schema_version` (an empty
  database, an advisory lock) and, where replicas run, holds the writers'
  turn: replicas started at once migrate the ledger once, and a write of
  another replica waits for the migration and finds it. A step that waits
  more than 2 s for a lock on a table (a `pg_dump` holds one on each)
  gives up and is tried again every 10 s, for 5 min; then the start fails.
  A database whose schema holds a ledger's tables but no
  `schema_version` is refused, not written over. The replicas' own tables
  (`replica_*`) have a version of their own, and are changed only where
  they are older ([design/replicas.md](design/replicas.md)).

A ledger at a newer schema than the release knows is not touched. serve
stops as it starts, exits 1, and says, for a SQLite ledger:

```
lapilli serve: --ledger: the ledger was written by a newer lapilli (schema 22; this one knows 21)
```

and on Postgres the same after `--ledger-url-file:`. In a pod, it restarts
over and over. On a Postgres ledger that replicas share, a newer release
rolled onto older replicas migrates as its first pod starts; each older
replica finds the newer schema at its next read or write, writes nothing,
and stops (`the ledger was written by a newer lapilli (schema 22; this one
knows 21): stopping`), and the rollout replaces it
([Several replicas](serve.md#several-replicas)).

`lapilli ledger copy` brings the ledger it copies from to its own schema
first, as serve would. `lapilli preflight`, given serve's flags where serve
runs, opens the ledger read-only and says, from 0.53 on, which of these it
is ([Before it is deployed](serve.md#before-it-is-deployed)):

```
ok       ledger.sqlite       schema 022, this release's
ok       ledger.sqlite       schema 021: serve upgrades the ledger from 021 to 022 as it starts, and
                             a release before 022 can't open it after: keep a copy first
problem  ledger.sqlite       schema 023, newer than this release's 022
```

On Postgres the item is `ledger.postgres`.

## The ledger's schemas

Each schema, the first release that has it, and what it adds. A Postgres
ledger is given 001 to 017 from one file of its own, then each later one in
turn.

| Schema | First release | What it adds |
|---|---|---|
| 001 | 0.6.0 | The ledger: rules and their versions, alerts, firings and their stones, incidents and what caught them, each rule's tallies by day and by week, runs, settings, the changes made in the UI and the API, and results worked out |
| 002 | 0.6.0 | An index of the alerts still firing |
| 003 | 0.9.0 | Teams, notes on rules, quiet windows (and the firings they excuse), tasks and comments |
| 004 | 0.10.0 | Quiet windows that repeat; a task's link to a tracker, its overdue notice and when it was done; the outbox of notices |
| 005 | 0.12.0 | An index of each rule's days, read when a change to a rule is measured |
| 006 | 0.17.0 | A team's own working hours, hours asleep and time zone |
| 007 | 0.18.0 | A team's shifts |
| 008 | 0.20.0 | Firings recorded elsewhere: a rule's source and the name it was sent under (`rules.source`, `rules.sent`), and `sources` |
| 009 | 0.20.0 | Notifications as they were sent, and the alerts each told of |
| 010 | 0.21.0 | The replay beside what really fired, by rule and day |
| 011 | 0.21.0 | The source a notification's alerts were copied in as, the kind of message it came as, and indexes of a cloud alarm's `alarm_id` |
| 012 | 0.22.0 | Reports people schedule |
| 013 | 0.22.0 | Proposals and their approvals, and an index of the changes by what they were made to, for who set a task's status |
| 014 | 0.25.0 | Roles given in the UI (`grants`); a quiet window's team, and repeats by month or by a cron expression |
| 015 | 0.27.0 | On-call turns; `comments` made again with `origin`, `ref` and `author`, and ids never used again |
| 016 | 0.28.1 | `tasks` made again, with ids never used again |
| 017 | 0.31.0 | Deploys and their label sets, a mark on each firing that came during one, counted by day and week, and an index of re-fired firings |
| 018 | 0.41.0 | An index of when incidents ended |
| 019 | 0.44.0 | `declared` and `given_back` on teams, quiet windows, reports and roles, for `--config`; a team's name unique in any letter case |
| 020 | 0.53.0 | Goals, and the goal a task is about |
| 021 | 0.53.0 | Marked windows: their kinds, the windows, the kinds each firing is marked with, and tallies of them by day and week |
| 022 | 0.55.0 | Named impact signals and the rule entries in effect; an incident's signal and match labels; the signals each alert is tied to; and the tallies by day and week keyed by them too, every row copied in once |
| 023 | 0.61.0 | Pull request backtests (`--backtest-listen`): a row each backtest asked for, kept a day after it ends, and the tokens that started one, kept past their hour |
| 024 | 0.63.0 | A lead of a named signal's or a rule entry's own (`lead_ns`, NULL to follow), and the labels a rule entry requires (`require_labels`) |
| 025 | 0.63.0 | Episodes beside firings: each firing's episode (`episode`, the id of its first firing, and `episode_white` on a first firing), and `episodes` and `episodes_white` on the tallies by day and week, marked ones too; counted after the upgrade a week at a time, the newest first (`episodes_from` in `meta`), no stone changed |
| 026 | 0.63.0 | People's verdicts on firings (`verdicts`, kept by alert and start, each part with who, when and a note), what judging keeps of them on each firing (`person`, `unrelated`) and their tallies by day and week (`unrelated`, `unrelated_excused`, `real_white`, `real_excused`, `action_yes`, `action_no`); the firing "This was real" wrote an incident from (`real_of`), and `judged` on rule entries, reserved and unused; no stone changed until a person says something |
| 027 | 0.65.0 | What checking each rule's expectations found (`expectation_checks`): by rule, expectation (a hash of what it is held to) and week, met, broken, not known or so far, with the numbers, what was told of it and the task made of it; drafts of expectations (`expectation_drafts`); what a proposal adding a rule is held to, and what became of it once added (`proposals.expect`, `proposals.expect_kept`); nothing to backfill |
| 028 | 0.67.0 | The alerts an incident's tool linked to it (`incidents.alerts`, JSON, `''` for none), and whether a catch was by one of them (`catches.linked`); nothing to backfill |
| 029 | 0.67.0 | Mutes from Alertmanager's event recorder (`mutes`): a row an alert, a silence or an inhibiting alert, and an hour, with when it was first and last told of; nothing to backfill |
| 030 | 0.69.2 | The periods a rule judged from Grafana's own alert state history was left out of what was copied in (`left_out`): by source and rule, those that meet kept as one; nothing to backfill, as before it a day such a rule was in was a gap for every rule |
| 031 | 0.69.2 | The repository's id on a pull request's backtest (`backtests.repository_id`, as the ID token says it and its entry pins it, `''` for a plain token's and for a row from before): a newer commit's takes the place of the one waiting by it; nothing to backfill, a row with none matched by the repository's name as before |
| 032 | unreleased | Whether a catch was loose (`catches.loose`: the labels of the match in effect that the firings lacked, joined by commas, `''` when not loose); the first start after the upgrade judges every firing again, once (how long it takes scales with the ledger, as a `--linked-only` change does), so that a catch from before shows its mark |
| 033 | unreleased | The branch of a merge queue's backtest (`backtests.queue_branch`, as the queue's ref in its ID token names it, `''` for a pull request's, any other run's and a row from before), and a GitHub Actions job's workflow and run's number (`backtests.workflow`, `backtests.run_number`, from its ID token's `workflow_ref` and `run_number`, `''` and 0 otherwise): a group the queue built again takes the place of the one waiting of the same pull request and branch, never a pull request's own; a backtest takes the place only of one of another commit and, of a workflow, of an earlier run of it; nothing to backfill, a row from before taken the place of as before |
| 034 | unreleased | What people did when a rule fired (docs/design/responses.md): the pages a paging tool sent (`responses`), the changes made to the alerts' targets that each run reads from kube-state-metrics for the hours it judges (`rollouts`, `rollout_spans`, and `rollout_owners`, which says which workload a pod named by a firing belongs to), each firing's response, whether a routine change is kept with it and whether it was useful (`firings.response`, `routine`, `useful`), and the days' and weeks' counts of them (`resp_changed`, `resp_routine`, `resp_acknowledged`, `resp_unanswered`, `useful`, `not_useful`); `judgedBy` goes to `j3`, so the first start judges every firing once again, as 032 did; a ledger with no `--responses`, `--rollouts` or `--deploys` keeps every firing `none` and `unknown`, and nothing it says changes |
| 035 | unreleased | Who each firing reached, worked out from the alert manager's configuration (docs/design/routing.md, "In lapilli serve"): the notifications worked out, a row for each integration of each receiver that sent one (`routed`: when, receiver, integration kind, whether it pages, why, the group, and how many firings it told of), the notifications that told of each firing (`firing_routed_sent`: when, receiver, kind, whether a page; what a firing reached is counted from them, so a run never rewrites the notifications of hours it didn't work out), and what each firing reached (`firing_routed`: its notifications, its pages, when the first went and when each did, the receivers and integration kinds it reached, what muted it; gone with its firing), and which hours were worked out (`routed_cover`: stretches, each worked out or not, with why, so a gap reads as not known, never none). Nothing to backfill: a ledger with `--alertmanager-config` or `--grafana` works out the days it has firings for, back to `--backfill`, on its first start, and each run works out the hours it judges; without them the tables stay empty and nothing a page says changes |
| 036 | unreleased | The service graph a catch by dependency is judged by (docs/design/dependencies.md, "In the ledger"): the edges each run reads with `--depends` for the hours it judges and the lead before them (`edges`: the hour, the caller and the callee, kept as long as the firings), the spans the graph was read over (`edge_spans`) and the spans a run could not read, which the next run reads once more, or, failed twice, keeps as known gaps (`edge_retries`), and, of each catch, the way a rule caught the incident by the graph alone (`catches.dependency`, a JSON list of the services in call order, `''` for any other catch), and of each firing whether it is white only by the graph (`firings`, `days` and `weeks` `white_depends`, the report's `white_by_dependency`). The expression the edges were read with is kept as a digest of it as Lapilli reads PromQL: a real change of the expression, labels or step discards the edges the ledger holds and reads the last four weeks again, a week at a time `judgedBy` is unchanged: a ledger started with `--depends` (or given one on the Settings page) judges every firing again once, as a change of `--linked-only` does. Without `--depends` nothing is read or kept, and nothing any page says changes |

The image has no shell and no `sqlite3`, so the schema a ledger is at is
read where its data can be reached: `sqlite3 COPY/ledger.db 'PRAGMA
user_version'` on a copy of a SQLite ledger's files, or `SELECT version
FROM schema_version` in its Postgres database.

## Before an upgrade that changes the schema

- Read the changelog's entries for each release between the two. From
  0.12.0 on, the entry of a release that changes the schema says so, as
  0.55.0's does: **Ledger schema 022**, after 021.
- Take a copy of the ledger, and upgrade once it is taken: a snapshot of
  the disk behind the PersistentVolumeClaim, the files copied out with
  serve stopped, or a backup of the Postgres database. The chart's README
  says how for each, under [Going back after a schema
  upgrade](../charts/lapilli/README.md#going-back-after-a-schema-upgrade).
  Not with the new release's `lapilli ledger copy`: it brings the ledger to
  its own schema first.
- Run the new release's `lapilli preflight`, with serve's flags, against
  the ledger: for SQLite, the new binary on a machine that holds the
  copied files, with `--ledger` naming their directory; for Postgres, with
  `--ledger-url-file`, from where the database can be reached (it connects
  read-only). Its `ledger.sqlite` or `ledger.postgres` line says whether
  serve will upgrade the ledger; off the cluster, the items that need what
  only the cluster has (the history, the tokens' files) may say problem or
  unknown. The running pod's `lapilli preflight` is the old release's, and
  one run in a pod that mounts the claim read-only says the directory isn't
  writable, and reads no schema.

## Going back after a schema upgrade

A release from before the change refuses the upgraded ledger, so going back
is going back to the ledger as it was: the copy taken before the upgrade,
with the release before. Not with `helm rollback`; the steps are in [the
chart's README](../charts/lapilli/README.md#going-back-after-a-schema-upgrade).
What came in after the copy that can't be replayed isn't in it.

## From `--impact` to named signals

A ledger `lapilli serve` kept with `--impact PROMQL` holds the windows
that signal found, with no name. With named signals (`--impact
NAME=PROMQL`, `--impact-file`, or the Settings page; see [How it
judges](how-it-judges.md)), a firing is judged by the signals tied to it,
and a window with no signal would be an incident written down, which
keeps every rule's record. So the windows are given the name `impact`,
whichever way the switch is made
([design/signals.md](design/signals.md), "Step 3, as built"):

- **On the Settings page**, the first named signal saved makes the one
  with no name the signal `impact` (`impact-2` when the ledger keeps
  windows of an `impact`), read on beside it with what keeps its record,
  its windows' keys, decisions, tasks and comments kept.
- **On the command line**, serve started again with `--impact
  NAME=PROMQL` or `--impact-file` in place of `--impact PROMQL` ties
  them as its leader's first round begins, once, and logs it:

  ```
  time=TIME level=INFO msg="windows of the impact signal with no name tied to a named signal, as the Settings page ties them" windows=N signal=impact read=true
  ```

  The Run history notes it among the changes (`lapilli: impact windows
  of the signal with no name tied to the signal impact`). With a signal
  named `impact` in the file, they are its windows, keys and all; when
  the ledger keeps no `impact` yet and its expression and `for` are
  those `--impact` and `--impact-for` were, it is read on from where the
  one with no name was, nothing read again. With none named `impact`
  (`read=false`), they are the windows of a signal no longer read, kept
  as those of a signal taken away are, which judge no one (below): they
  keep no rule's record, and only the rules no rule entry names catch
  them, so rules that an entry ties only to signals with `record: false`
  are left unjudged in their weeks, as `lapilli verdict` leaves them. To
  keep those weeks' record, confirm the windows that were real outages;
  or give the file the old expression as a signal named `impact`, before
  the first start with named signals: `impact` is then made of the one
  with no name, its windows its own and its record kept. Added later, the
  windows are its own, matched on its labels, but it keeps the record
  only from its own start, as does an `impact` a release before this one
  made: a firing tied to it in a week before that, with no window there,
  is not judged, where `lapilli verdict` judges it. The firings that
  could catch the windows are judged again in the same round. A tie the
  database refuses is logged
  (`the windows of the impact signal with no name weren't tied to a named
  signal`), the windows judged as they are, and tried again at the next
  round. From 0.55.0 to 0.61.1 the windows were left with no name, and
  kept every rule's record in their weeks and the two after each.

## Windows of a signal no longer in effect

A named signal no longer in effect, taken away on the Settings page,
dropped by a start without it, or set aside at a start past eight (and
`impact` above, with none of that name in the file), keeps its windows,
with its name. From this release they judge no one
([design/judging.md](design/judging.md), item 5): they keep no rule's
record, and only the rules no rule entry names catch them, on the labels
they were found with. Before it they kept every rule's record, as
incidents written down, and any rule caught them.

On the upgrade the leader's first round judges their spans again, once,
and logs it:

```
time=TIME level=INFO msg="windows of impact signals not in effect judged again, once: they keep no rule's record, and only rules with no rule entry catch them" windows=N
```

So the weeks such a window kept, and the two after each, may show firings
counted and not judged where they were black before (a rule whose
precision, noisy badge or goal needed that record has none there), and a
rule an entry names may no longer have caught such a window, its firing
black or not judged where it was white. To keep a week's record, an
editor or an admin confirms the windows that were real outages: a
confirmed window is an incident written down, which keeps everyone's
record. A ledger that never kept a window of a signal no longer in effect
judges as before.

## A firing judged by its day

From 0.72.0, `lapilli serve` judges a firing that caught nothing by the
record of its day, in `--tz`, not of its week: a day's firings are judged
when an incident was on in the day or in the two weeks before it, or
`--impact` was read over all of it
([design/judging.md](design/judging.md), "The record, by day"). There is no
schema change, and no start judges every firing again: what the pages and
the API show is read from the ledger as they are asked, so on the upgrade,
once, the numbers move where a week had an incident part of the way
through it or its two weeks ended part of the way through it:

- firings of the days of a week before an incident started in it, which
  were judged, and may have made a rule noisy, are now counted, not judged
  (`? Not judged`), and the rule's precision, its noisy badge and what a goal
  reads of them follow;
- firings of the days after an incident's two weeks ended, in the week they
  ended in, were judged and are not now;
- every day the two weeks reach, the same as before.

A week that an incident, or its two weeks, cover whole, and one with no
incident in it or the two weeks before it, are judged as before. The
week's own numbers (its record being "kept") are the same: a week has a
record when any day of it does. A report's `settings` says which in
`record_by` (`"day"`, or `"week"`; `record_by_week` stays and is true only
by week), and so does `GET /api/v1/settings`.

To keep the numbers as they were, give `--record-by-week` (the chart's
`serve.recordByWeek: true`) where serve is started, before the upgrade or
with it. Nothing is judged again for it, now or when it is changed later:
no stone depends on whether a firing's record is of its day or its week,
which is read when a page or a report is asked. Replicas are given the
same, or they show different numbers from the same ledger.

`lapilli verdict` is unchanged: it judges by the record at the firing's own
time.

## The Helm chart

A release's chart has the release's version without the `v` (`0.59.0` for
`v0.59.0`), and the release as its `appVersion`; it runs the image that
release built, by its digest
(`ghcr.io/lapilli-project/lapilli:v0.59.0@sha256:...`). Upgrade by the
chart's version:

```sh
helm upgrade lapilli oci://ghcr.io/lapilli-project/charts/lapilli -n monitoring --version 0.59.0 -f values.yaml
```

`image.tag` set to another version needs that version's `image.digest`
too, or `image.digest` empty: the chart refuses its own image's digest
under another tag. With one replica the Deployment is `Recreate`, so the
pod before stops before the new one starts; with `serve.replicas` above 1
(Postgres only), it rolls one pod at a time. From 0.35 or before, upgrade
with `serve.replicas: 1` first, then raise it
([Several replicas](../charts/lapilli/README.md#several-replicas)). In
CronJob mode the chart runs `lapilli verdict`, which keeps nothing to
upgrade.

## A source named `grafana`

From 0.68, `lapilli serve` copies in what Grafana's own alert state
history says the rules `--grafana` holds that can't be replayed fired,
under the source `grafana`, written as `grafana-history`
([design/grafanajudged.md](design/grafanajudged.md)); the firings port
refuses that name. A ledger where a job copied firings in under
`grafana` before keeps them as they were, and serve copies nothing under
it, saying so once in its log. Send the job's firings under another name,
and let `grafana` go: a source nothing is written to for `--keep` is
forgotten. Or give `--grafana-history=false`.

## Renamed names

A flag, a route of serve's API or a chart value that is renamed is still
taken under its old name until the next major release, so a job, a
dashboard or a values file written for the old name goes on working; the
changelog entry of the release that renames it says what it is now. Take
the new name when you next touch the file; each says it is old:

- **A flag.** `--old` (or `--old=value`) is read as `--new`, and stderr says,
  once for the run, `lapilli <command>: --old is --new now; --old will go
  in a major release`. A flag given under both names is an error that names
  both. `-h` lists the new name only. No environment variable stands for a
  flag, and `--config`'s file does not name them; a key renamed in that
  file would be taken the same way, and listed here.
- **A route.** A request to the old path is answered by the new route's
  handler, with its sign-in, role, host, token and size checks, as one to
  the new path is (it is not a second route). The response carries
  `Deprecation: @<seconds>`, the day the old name was deprecated as
  [RFC 9745](https://www.rfc-editor.org/rfc/rfc9745) writes a date, and
  `Link: <new path>; rel="successor-version"`; serve's log says so once for
  each old route. A route a port doesn't have under its new name isn't
  taken under its old one there.
- **A chart value.** The old key is read as the new, and the chart's
  `NOTES.txt` lists each old key it moved. Values kept from 0.71.0 (`helm
  show values`, or `helm upgrade --reuse-values`) carry every old key at its
  old default, which is not your choice: an old key equal to its 0.71.0
  default is ignored, not moved and not listed. An old key set otherwise
  moves to the new key when the new key is at its default. A block (a map)
  set under its old key is laid over the new key's default block, key by
  key: a key it leaves out keeps the new default, and a key it holds as an
  empty value (`""`, `false`, `[]`) is kept as you gave it. Both given and
  differing is an error that names both keys: keep the new one and take the
  old out. The two keys 0.71.0 read in serve mode alone
  (`serve.checkContent.alertmanager`, `serve.checkContent.scrapeInterval`)
  are moved in serve mode alone; in cronjob mode they stay ignored, as they
  were, and `NOTES.txt` says so. A key missing from the values altogether,
  as after `--reuse-values` from a release that lacks it, is the chart's own
  default: the chart fills missing keys only when the marker `chartValues` in the
  values is not its own: a release writes its version there, in git it is
  `dev`, and an older release's values hold an older marker or none (don't
  set it, and never `--set chartValues=null`). With its own marker nothing is
  filled, and `--set key=null` removes a default as it always has. When the fill
  ran (a values file copied from git with `chartValues: "dev"`, one saved with
  `helm show values` from an earlier release, or any `--reuse-values` across
  versions, where Helm keeps the old marker), a `null` does not remove a default
  in that release, and `NOTES.txt` says so: drop `chartValues` from copied values
  files, and upgrade with `--reset-then-reuse-values` instead of
  `--reuse-values`. `--reuse-values` between two git checkouts (both `dev`) is
  not covered. The image guards below apply only to the chart's own
  `image.repository`: a mirror with its own digest and tag is not refused. A released
  chart whose `image.digest` is another release's (kept by `--reuse-values`
  or saved values) with `image.tag` empty, or equal to this chart's own
  version, fails, rather than run that image under this version's name: set
  `image.digest` to this chart's own (the message prints it), or `""` (no
  digest pinning), or give another `image.tag` too.

Renamed since 1.0's promise began, each taken under its old name until the
next major release:

| Old | New | Since |
|---|---|---|
| `--notify-listen` | `--notifications-listen` | 0.72.0 |
| `--notify-token-file` | `--notifications-token-file` | 0.72.0 |
| `--notify-grafana-secret-file` | `--notifications-grafana-secret-file` | 0.72.0 |
| `--notify-sns` | `--notifications-sns` | 0.72.0 |
| `--incidents-listen` | `--jobs-listen` | 0.72.0 |
| `--changes-paused` | `--changes-pause-file` | 0.72.0 |
| `--changes-token-file` | `--changes-recipe-token-file` | 0.72.0 |
| `--webhook-dest-secret` | `--webhook-dest-secret-file` | 0.72.0 |
| `--oidc-hosted-domain` | `--oidc-hosted-domains` | 0.72.0 |
| `--bearer-token-file` | `--prometheus-token-file` | 0.72.0 |
| `--extras` | `--extras-timeout` | 0.72.0 |
| `--expect-required` | `--fail-no-expectation` | 0.72.0 |
| `--gcp-project` | `--google-project` | 0.72.0 |
| `--redact-key` | `--redact-key-file` | 0.72.0 |
| backtest action input `gcp-project` | `google-project` | 0.72.0 |
| backtest action input `expect-required` | `fail-no-expectation` | 0.72.0 |
| chart `serve.incidentsAPI.port`, `.from`, `.copy` | `serve.jobsAPI.port`, `.from`, `.copy` | 0.72.0 |
| chart `serve.changes.tokenSecret` | `serve.changes.recipeTokenSecret` | 0.72.0 |
| chart `gcpProject` | `prometheus.googleProject` | 0.72.0 |
| chart `serve.checkContent.alertmanager` | `alertmanager` | 0.72.0 |
| chart `serve.checkContent.scrapeInterval` | `scrapeInterval` | 0.72.0 |
| chart `serve.roles.groupsHeader` | `serve.groupsHeader` | 0.72.0 |
| `GET /api/v1/expectation-drafts`, `GET /api/v1/expectation-drafts/{id}` | `GET /api/v1/expectations/drafts`, `GET /api/v1/expectations/drafts/{id}` (deprecated on 2026-11-26) | 0.72.0 |

`--changes-pause-file` (either name) takes a file's path: `true`, `false`, `1`,
`0`, `on` and `off` are refused, as they read as a switch. The flags and
the value that were new in 0.72.0 and renamed before it came out
(`--notifications` is `--notifications-worked-out`, `notifications.enabled`
is `notifications.workedOut`, `--depends-count-missed` is
`--depends-count-catches`) have no old name to take.

A name in a query or a body of the API (0.72) is taken under its old name
until the last 0.x minor release, not through 1.x
([What stays the same](numbers.md#what-stays-the-same)): `GET
/api/v1/activity`'s `since` and `until` are `from` and `to`; a window's
repeat day, `on` in the bodies of `POST` and `PUT /api/v1/windows` and
`/api/v1/marked` and in the `--marked` file, is `repeat_on`, as `--config`
has it. A name given with its old one is a 400, or an error naming both.
What the API gives back has `repeat_on`, and `on` beside it, as does the row
in a `window.kept` or `marked.kept` notice.

## Release notes

Each release has its section in [CHANGELOG.md](../CHANGELOG.md), and its
[GitHub release](https://github.com/lapilli-project/lapilli/releases) has
that section as its notes. While the version is 0.x, a minor release may
change flags or the API, and its entry says how
([GOVERNANCE.md](../GOVERNANCE.md#releases)); what keeps its name and its
meaning across releases, and how a change to it is made, is in
[What stays the same](numbers.md#what-stays-the-same). Only the latest
release gets fixes ([SECURITY.md](../SECURITY.md#versions)).

Each release's archives, `SHA256SUMS`, chart and image are attested by the
workflow that built them, with Sigstore, and from v0.28.0 the attestations
are also an asset of the release, `lapilli-vX.Y.Z.intoto.jsonl`.
[SECURITY.md](../SECURITY.md#checking-a-release) says how to check them,
offline too.
