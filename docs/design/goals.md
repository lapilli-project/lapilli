# Goals: what an operations team holds its alerting to

Status: built. Steps 1 and 2 in 0.53.0, schema 020; step 3, goals over a
period, for the next release, over marked windows' schema 021 (#90), with
no schema of its own. Under [GOVERNANCE.md](../../GOVERNANCE.md)
this is a new kind of data in the ledger (the next schema at build time),
so it was designed first, as a pull request (#91); it was merged on
2026-11-16, before its week was out, by the maintainer's decision, with the
review's edits. The decisions taken are in "Open questions"; the rest stay
open until the step that needs each. What building it made plain is in
"As built", at the end. It changes no stone and judges no
firing: a goal is checked against the numbers the summary already gives.
The companion to [noc.md](noc.md), whose `/api/v1/summary` it reads; to
[expectations.md](expectations.md) (#81), which holds one rule to what it
was tested to do, and this the level above; to [work.md](work.md), whose
tasks a missed goal becomes; and to [marked windows](marked.md) (#90),
whose kinds a goal over a period is checked by.

## Why

An operations team sets goals for its alerting, such as:

- at least 80% of critical firings each week caught something;
- no incident missed;
- no alert re-fires within an hour, any week;
- a team is paged at most 40 times a week.

[noc.md](noc.md) built the numbers these need: `/api/v1/summary` gives,
for each week from Monday and for everyone or a team, critical firings and
the share caught, re-fires, firings while people slept, incidents and
impact windows caught and missed, and the median lead. What it doesn't do
is say the goal. Each Monday someone reads the numbers off a page or a
dashboard, sets them beside a list kept somewhere else, and says in a
meeting which were met. Goals aren't written where the verdict is, a week
with no record reads as a week with nothing missed, and a goal missed three
weeks running is noticed when someone remembers to look.

There is also a goal no weekly number answers: "during the sale, the alerts
did no worse than the weeks before". It is held over a span that isn't a
week, beside the weeks around it.

## Goals and expectations

[expectations.md](expectations.md) is about **one rule**: how often it may
fire in a week, which incidents it must keep catching, which `promtool`
scenarios it passes. It is written beside the rule, checked on the pull
request that changes the rule (and can fail it), and checked after deploy.

A goal is about **the alerting of everyone, or of a team**: shares of all
their firings, the incidents none of their rules caught, re-fires across
every rule. It is checked after the week, never on a pull request: whether
80% of a week's critical firings catch something depends on what went
wrong that week, which no change to a rule decides alone.

The line: what names a rule is an expectation; what names a measure of
many rules is a goal. Two consequences:

- **A team's budget** (expectations' open question 2, how often a team is
  paged in a week over all its rules) is a goal here, `critical.firings` or
  `firings`, `at_most`, for the team. It doesn't need a fourth kind of
  expectation; expectations.md says so.
- **The same firings can break both** in the same week: a rule over its
  budget and its team's goal of re-fires. Each says so in its own place,
  and makes its own task: an expectation's is about its rule, a goal's
  about the goal.

## What a goal is

Goals are declared in `--config` ([serveops.md](serveops.md), "Declared
configuration"), under a key of their own, `goals:`, the sixth after
`teams`, `windows`, `reports`, `grants` and marked windows' `marked`
([marked.md](marked.md)):

```yaml
goals:
  - name: critical-caught
    measure: critical.share_caught
    at_least: 0.8
  - name: none-missed
    measure: incidents.missed
    at_most: 0
  - name: no-refire
    team: "*"                     # each team, its own
    measure: refired.firings
    at_most: 0
  - name: paged
    team: payments
    measure: critical.firings
    critical: [critical, page]    # the severities counted as critical
    at_most: 40
  - name: sale-no-worse           # over a period: see below
    each: sale
    measure: precision
    no_worse_than: before
    by: 0.05
```

- **`name`**: `[a-z0-9][a-z0-9_-]`, 64 bytes at most, unique in the file,
  across `*` and every team: a goal is known by its name alone.
- **`team`**: left out, everyone's; a team's name, that team's; `*`, each
  team's own, checked for each team.
- **`measure`**: one of the summary's numbers (below).
- **The bound**: exactly one of `at_least`, `at_most` (a number; a share is
  from 0 to 1) and `no_worse_than: before` (a goal over a period, below).
  **`by`**, a slack, goes only with `no_worse_than`.
- **`critical`**: the severities counted as critical, only with a
  `critical.*` measure, as the summary's `critical=` takes them (`critical`
  when left out; 32 at most, 64 bytes each). The goals hold four different
  lists at most, as each is a count of its own over the week's rules.
- **`each`**: `week` when left out; or a kind of marked window. `each:
  deploy` is refused: deploys are counted by `during_deploys`, and are too
  many and too short to be a period.

Read as the rest of `--config` is: strictly, or not at all; read twice
before it is kept; kept in the ledger by the leader, so every replica's
pages say the same; 50 goals at most. A goal that breaks a rule above is
refused with its line, as a key `--config` doesn't know is. A goal naming
a team the file doesn't declare and the ledger doesn't hold is skipped and
said why, as a quiet window of a team that isn't there is.

### The measures

Each is a field of a summary row, so a goal means what the summary means,
with the settings the summary gives (`--refire` for re-fires, the noisy
thresholds for `noisy_only`, the hours for asleep and off hours):

| `measure` | The summary's | Better | Needs the record | With `each: KIND` |
|---|---|---|---|---|
| `firings` | `firings` | lower | no | yes |
| `precision` | `precision` | higher | yes | yes, and `no_worse_than` |
| `critical.firings` | `critical.firings` | lower | no | no |
| `critical.share_caught` | `critical.share_caught` | higher | yes | no |
| `refired.firings` | `refired.firings` | lower | no | yes |
| `refired.share` | `refired.share` | lower | no | no |
| `asleep.firings`, `off_hours.firings` | `asleep.firings`, `off_hours.firings` | lower | no | no |
| `asleep.nothing`, `off_hours.nothing` | `asleep.nothing`, `off_hours.nothing` | lower | yes | no |
| `incidents.missed`, `impacts.missed` | `incidents.missed`, `impacts.missed` | lower | yes | yes |
| `incidents.noisy_only` | `incidents.noisy_only` | lower | yes | yes |
| `incidents.median_lead_seconds` | `incidents.median_lead_seconds` | higher | yes | yes, and `no_worse_than` |
| `during_deploys.precision_without` | `during_deploys.precision_without` | higher | yes | no |

"With `each: KIND`" is what [marked.md](marked.md) counts inside an
occurrence of a kind (`marked` in the summary: firings, caught, precision,
re-fired, incidents and impact windows with their median lead); a goal over
a kind with another measure, or `no_worse_than` with a measure the table
doesn't give it, is refused. A measure the summary doesn't give isn't a
goal's; when the summary gains a field, it can become one, in its own
change.

## Checked each week

A goal of `each: week` is checked for each week, from Monday in `--tz`, as
the summary's rows are, for its team as the summary counts a team (its
rules' firings, the incidents it owns). Each whole week is one of:

- **met**: the value is within the goal;
- **missed**: it isn't;
- **not known**, with why: the week has no record and the measure needs one
  (`no_record`: no firing of the row was judged, so nothing tells an
  incident nobody wrote down from none); the value is null (a share with
  nothing to divide, as no critical firing; a median with nothing caught;
  asleep with no hours given); or the week isn't whole in the ledger
  (`not_whole`: the week its first firing fell in, as [noc.md](noc.md)
  counts "whole" weeks).

"No incident missed" in a week no firing of the team's was judged is not
known, never met. Neither is a goal on incidents met for a team with no
labels: a team owns an incident by a value of its labels or by catching it
(`owners.ownsIncident`), so one with none owns only what its rules caught,
and has never missed one. Such a goal is not known, with why
(`team_owns_by_catching`), and the Goals page says to give the team
labels.

**The week under way** is shown as it stands, not as a result: `open`, with
its value so far. A count over its `at_most` already is shown as over so
far (warn), since more firings can only add to it; it is decided once the
week is whole.

**When it is worked out.** Two ways, from the same function over the
summary's rows:

- **When read.** A page, the summary and MCP work goals out from the rows
  they read, as every number Lapilli shows is: an incident written down on
  Wednesday for last Friday turns last week's goal met or missed as it
  turns the week's stones.
- **After each run, by the leader**, for what is told and done: once the
  cursor passes a week's end (or the end of an occurrence of a kind some
  goal is `each:` of), and then after each run, it works out only the last
  whole week, the weeks of the goals with an open task (twelve weeks
  back at most), and the occurrences that ended since. That is where
  `goal.missed`, a task made, "missed again" and "no longer missed" come
  from (below), and what `/metrics` gives. Its cost: one read of those
  weeks' tallies and incidents, every team's row counted from it, as
  `summaryOf` counts every team from one read of a week; 13 weeks at
  most, by everyone and each team a goal names (`*`, every team); kept in
  memory, and said to the other replicas with the leader's status, as its
  other findings are.

## Over a period

"During a sale, the alerts did no worse than the weeks before" is held over
each occurrence of a marked window's kind ([marked.md](marked.md), #90),
not each week:

- **`each: KIND`**: the goal is checked for each occurrence of that kind
  that ended, and the one on now as it stands. An occurrence of a
  repeating window is one period, as #90 counts it.
- **The value** is the measure inside the occurrence: the firings marked
  with the kind on the days it covers, and the incidents during it, as #90
  counts them.
- **`no_worse_than: before`** sets it beside the same measure over the four
  whole weeks before the occurrence's first week, outside the kind, with
  `by` as slack (0 when left out): met when inside is no worse than before
  by more than `by`, "worse" as the table's "Better" says. Only `precision`
  and `incidents.median_lead_seconds` take it: a count over a three-day
  sale can't be set beside one over four weeks.
- **`at_least` and `at_most`** hold inside alone: "no incident missed during
  a sale" is `each: sale`, `incidents.missed`, `at_most: 0`.
- **Not known** as a week is: no firing judged inside, for a measure that
  needs the record, or before with nothing to divide.

A goal over a span that is neither a week nor a marked window, such as a
month, is a marked window of a kind written for it. A marked window lasts
31 days at most, so a quarter isn't one ("Open questions").

## Where it shows

- **A page of its own**, `/goals`, "Goals", under Reporting beside Outcomes:
  each goal, its team, its target, the last whole week's result and value,
  and its twelve whole weeks as cells in the status colours ([ui.md](ui.md):
  ok met, bad missed, unknown not known, no colour for the week under way),
  each with its value and, when not known, why. A goal over a period shows
  its occurrences instead. With a team picked, that team's goals and
  everyone's. Every goal carries the `declared` badge: it is changed in
  values.
- **The overview**: a Goals card, the last whole week's results for the team
  in effect (everyone's goals with none), how many met of how many. A goal
  missed is a line of "Look at these first" ("Goal critical-caught missed
  last week: 72% of critical firings caught, at least 80%"), with "Make it a
  task", as the others are.
- **The digest**: a line: how many goals met, and each missed, with its
  value and target; a team's digest, its own goals. `digest.ready` carries
  them (`goals`).
- **Reports**: a section, `goals` (`ledger.ReportSections` gains it): each
  goal over the span's whole weeks, and the occurrences of a kind that
  ended in it.
- **`/api/v1/summary`**: each row gains `goals`, the goals of its team
  (everyone's in a row for everyone; the team's own and `*` goals in a
  team's row):

  ```json
  "goals": [
    {"name": "critical-caught", "measure": "critical.share_caught", "at_least": 0.8,
     "value": 0.72, "result": "missed"},
    {"name": "none-missed", "measure": "incidents.missed", "at_most": 0,
     "value": null, "result": "unknown", "why": "no_record"}
  ]
  ```

  `result` is `met`, `missed`, `unknown` or `open` (a row that is
  `partial`). A goal over a period is in the row of the week its
  occurrence ended, with the occurrence's `from` and `to`. `schema` stays
  1: `goals` is a field added, and no field changes.
- **`/metrics`**, for the last whole week, everyone's goals without a `team`
  label and a team's with it, as `lapilli_refired_weeks` is:
  `lapilli_goal_met` 1 when met, 0 when missed, no series when not known;
  `lapilli_goal_known`, 1 or 0, so that a dashboard tells not known from a
  goal that isn't there; `lapilli_goal_value`, when there is one; and
  `lapilli_goal_weeks_met`, of the twelve whole weeks before this one,
  beside `lapilli_weeks_counted`; each with `goal` and [`team`]. They are
  the values the leader worked out after the last run (above): a scrape
  reads them and works nothing out, and never waits for the heavy turn. A
  team's Prometheus can alert on `lapilli_goal_met == 0`.
- **MCP**: `summary` gives each row's goals; `overview` gives the card. No
  tool writes a goal.
- **Notices**: `goal.missed`, once for each goal, team and week (or
  occurrence), to the webhook and Slack where notices go, when the leader
  first finds the week whole and missed.
- **i18n**: every new text in the four languages, as every page's.

## When a goal is missed

A goal missed is a decision someone has to make: tune the rules, write
the incidents down, or change the goal. It becomes a task as
[work.md](work.md)'s are:

- **With one press**, from "Look at these first": a task of kind `decide`,
  of the goal's team (none for everyone's), titled with the goal and the
  week ("Goal critical-caught missed, week of 2026-11-09"), its body the
  value, the target and a link to the goal on `/goals`, linked to the goal
  (`tasks.goal`, below).
- **Already being seen to.** A goal with an open task for its team links to
  it, as a rule or an incident with an open task does (`v.Actions` in
  `dashboard.go`), and isn't offered again.
- **By Lapilli itself**, with `--goal-tasks`: off unless turned on, as
  #81's `--expect-tasks`, since Lapilli would make tasks nobody asked for.
  One open task for a goal and team at most: another week missed while it
  is open is a comment on it ("missed again, week of 2026-11-16: 0.70"),
  not another task, and a week met again a comment too. Once it is done or
  dropped, the next week missed opens a new one. A run makes 20 tasks at
  most, a `*` goal over many teams among them; the rest are made by the
  runs after, and the Run history page says how many wait.
- **A week no longer missed** (an incident written down later): the task
  gets a comment saying so, once; it isn't closed for anyone.

`goal.missed` is told once per goal, team and week, by the outbox's
`TellOnce` keyed on them, so a week checked again, or turned met and then
missed, isn't told twice.

## What stays the same, and what is added

[numbers.md](../numbers.md) lists what keeps its name. The pull request
that builds a step adds, in the same pull request, to its table of metrics
and to `TestMetricsStable`: `lapilli_goal_met`, `lapilli_goal_known`,
`lapilli_goal_value` and `lapilli_goal_weeks_met` (`goal`, [`team`]). The
summary's `goals` is the summary's own, under its `schema` (noc.md).
Nothing listed there changes.

## In the ledger

Goals are declared, not made in the UI: the ledger keeps what the file
declares, for every replica to read, and what was done about them, not
their results, which are worked out as above. The next schema at build
time adds:

- `goals`: as the file declares each: `name` (unique), `team_id` (0 for
  everyone's, and a mark for `*`), `measure`, `at_least`, `at_most` or
  `no_worse_than`, `by`, `critical` (JSON), `each`, and `declared`. Kept to
  the file by `ledger.Reconcile` as teams and windows are, matched by
  name.
- `tasks.goal`: the goal a task is about, by name, empty for none.

**A goal the file no longer declares is deleted**, with or without
`--config-prune`: the one exception to what the file no longer declares
being given back to the UI. There is no form in the UI to give it back to,
and its weeks are worked out from the summary, so nothing is lost but its
tasks' link, which stays as text. Without `--config`, there are no goals.

Upgrading adds a table and a column, with nothing to backfill. Going back
takes the ledger as it was, so a deployment keeps a copy of it (a snapshot
of its disk, or of its database) before it upgrades.

**Schema numbers.** A schema change is the next numbered file when it is
built: `migrate` numbers the ledger's schema by how many files there are
(`internal/ledger/ledger.go`), and Postgres refuses a ledger whose last
file's number isn't SQLite's (`migratePostgres`), so a number can't be
held for a design. Of the designs accepted and not built, expectations
(#81), impact signals (#88), marked windows (#90) and goals (#91),
whichever is built first takes the next number, and the rest follow in
the order they are built. One order matters: #88 adds `signals` to the
key of `days` and `weeks`, and #90's `marked_days` and `marked_weeks` take
the same key, in #90's own schema if it is built after #88, or in #88's if
before.

## Cost and bounds

As counts and bytes, which the tests hold, not times:

- **When read**, a goal reads the summary's rows the page reads already,
  cached until the ledger changes: no read of its own. Twelve weeks for
  everyone and one team are 24 rows. A page with `*` goals for every team
  reads every team's rows, in the heavy turn, as a summary of every team
  does, and keeps them.
- **After a run**, the leader reads 13 weeks of tallies and incidents at
  most, once, and counts every team's row from them (above).
- **A goal over a period** reads #90's counts of each occurrence: its days
  of `marked_days` and four weeks of weekly tallies.
- **50 goals, four `critical` lists.** `/metrics` gives each goal for
  everyone and each named team, and a `*` goal for every team, 500 series
  of each family at most, those missed first, as the incident series are
  bounded.
- **Tasks**: 20 made a run at most.
- **Tests**: `TestPagesScale` holds the Goals page and the overview's card
  by the rows they read and their heap peak, the least of three runs
  (#99); the leader's work by the rows it reads a run.

## Security

- **Who writes.** Whoever changes `--config`, through the review that file
  gets. No page or API writes a goal; MCP has no tool that does.
- **What comes in.** The file alone. Names are from a small alphabet, 64
  bytes; measures from a fixed list; severities bounded as the summary's
  `critical=`; teams by name, as a quiet window's.
- **What it shows.** A goal says nothing a viewer can't read from the
  summary already. Its name goes into pages, the summary, `/metrics` labels
  (escaped), the digest, reports and notices (as text a receiver escapes,
  as `digest.ready`'s).

## Not in this design

- **Failing a pull request.** That is what expectations do, for a rule.
- **Goals made in the UI** (below, "Open questions").
- **A measure the summary doesn't give**, such as how fast a page was taken
  on an on-call tool.
- **Spans other than the summary's weeks**: a month only as a marked window
  written for it, and no quarter.
- **Alerting on goals.** `/metrics` gives what a team's own Prometheus needs.

## Alternatives

- **Use what exists: the summary, a dashboard and a script.**
  `/api/v1/summary` gives every number a goal reads, by week and team, and
  the chart's Grafana dashboard can draw a threshold on a panel. That is as
  far as it goes: the goal lives in a dashboard's JSON or a script, not
  where the verdict is read; the dashboard reads `/metrics`, which counts
  the last seven days, not the week from Monday the summary counts; a week
  with no record shows a share that isn't there as empty, which a
  threshold reads as nothing wrong; and nothing makes a task, or says a
  goal was missed three weeks running.
- **Recording and alerting rules over `/metrics`.** The same seven days, and
  a page each time a goal is missed: a goal is reviewed, not paged on.
- **Expectations with a team's budget** (#81's open question). Covers how
  often a team fires, not shares of what was caught, incidents missed or
  re-fires across rules, which are no rule's.
- **Goals made in the UI first.** A goal is a promise a team makes, which
  its lead changes rarely and wants reviewed; `--config` has the review,
  the history and the export already.
- **Results kept as they were told.** A week's result frozen when it is
  told wouldn't follow incidents written down later, which is how
  Lapilli's numbers work everywhere else.

## Open questions

Decided on 2026-11-16, by the maintainer:

- **A share with nothing to divide** (a week with no critical firing) is not
  known, `unknown`, never met.
- **A team's record**, as [signals.md](signals.md)'s: a row's `no_record`
  means no firing of that row was judged, and the record a written
  incident keeps for a team's row stays everyone's, as today.

Open:

1. **Goals in the UI**, by admins, or by a team's editors for their team's
   goals, as their quiet windows and reports.
2. **Months and quarters** as `each:`, without writing a marked window for
   them; a quarter is longer than a marked window may be.
3. **Weeks in a team's zone.** The summary counts weeks in `--tz`; a team in
   another zone may want its own Monday.

## Steps

1. `goals:` in `--config`, the next schema, the Goals page, the summary's
   `goals`, the leader's work after a run, `/metrics`, numbers.md's table
   and test. Built.
2. The overview's card and line, tasks (`tasks.goal`, `--goal-tasks`), the
   digest, reports' `goals` section, `goal.missed`. Built, with step 1.
3. Goals over a period (`each: KIND`), once marked windows are built.
   Built, after #90: `each:` takes a kind's name as [marked.md](marked.md)
   names one (`[a-z][a-z0-9-]`, 32 bytes at most), `deploy` refused as
   above; a measure the table doesn't count inside an occurrence is
   refused with its line, and so is `no_worse_than` but over a period, of
   precision or the median lead.

## As built

Small things the design left open, settled as they were built:

- **Not known, and its value.** A week not known gives no value (`value`
  null), as a value it can't stand by would read as one. A null value of
  a whole week with the record kept is `no_value`: a share with nothing to
  divide, a median with nothing caught, asleep or off hours with no hours
  given. The whys are tried in order: `not_whole`, `team_owns_by_catching`
  (a team with no labels, for `incidents.missed`, `impacts.missed`,
  `incidents.noisy_only` and `incidents.median_lead_seconds`), `no_record`
  (a measure that needs it, in a row whose `no_record` is true), then
  `no_value`. `not_whole` is a week at or before the one the ledger's first
  firing fell in, or with none.
- **The week under way** is `open`, its value so far, and `over: true`
  for a count whose `at_most` it is past already (a measure lower is
  better, neither a share nor a time).
- **A goal's `critical`** is its own: the summary's `critical=` changes the
  row's `critical`, not a goal's, whose list is `critical` when left out.
- **In the ledger** (020): `team_id` is 0 for everyone's and for each
  team's, `every_team` 1 marking the latter; a bound is kept as the file
  wrote it, as text, in `at_least`, `at_most` or `no_worse_than` (so that
  SQLite and Postgres keep it alike), and `by` in `slack` (`BY` is a
  word of SQL's). A team a goal names is named by something declared: the
  UI deletes or renames it no more, and a file that no longer declares it
  gives it back to the UI rather than delete it.
- **The leader's work** reads the twelve whole weeks before this one and
  this one, for everyone and each team a goal is of, after each round in
  which the ledger or the week changed: what `/metrics`' weeks met needs,
  and within the 13 weeks the design bounds it to. What it works out (500
  lines at most) is kept in the ledger's meta, written aside, only when it
  changed; the status the leader writes every few seconds says only a sum
  of it, the week, how many lines and the tasks waiting, and a replica
  that follows reads the lines from the ledger when the sum moves, for its
  `/metrics`. Kept in the status whole, it would be some 50 KB rewritten
  every two seconds. The sum said is of what the ledger holds, as last
  written, not of what a write failed to keep; a follower keeps what it
  read under the sum only when it is what the sum is of. As a term of
  leading begins, the replica lets go of what it worked out in an earlier
  one, and serves what the ledger holds, saying its sum, until its first
  round: the series stay through a failover, and a first round that works
  out the same writes nothing. As the term ends, it lets go again.
- **`goal.missed`** is told of the last whole week alone, once its cursor
  has passed, marked by the goal, each team's id (0 for everyone's) and
  the week, which only moves on: a team marked for a later week, as by a
  leader whose clock was ahead, isn't told an earlier one. A goal of each
  team's (`*`) is one notice a week, of the teams that missed it and
  weren't told of it (`teams`, 50 named with their values, `more` beyond),
  not one a team. The marks stay when a goal is taken out of the file,
  or given another team, until they are thirteen weeks old: a goal put
  back after a ConfigMap rolled back, or a serve started once without
  `--config`, tells no week again, nor makes its task again.
- **Comments on a goal's task** are `--goal-tasks`', as its tasks are:
  without it, Lapilli writes on no task, one a person made with the button
  among them. With it, a run writes 20 at most, as it makes 20 tasks; the
  runs after say the rest. A task the runs find open already, made since
  they read the tasks, counts toward the 20 as one made.
- **A value missed** is shown cut to a tenth of a percent away from its
  bound, down for `at_least` and up for `at_most`, so that it never reads
  as the bound it missed (79.9%, not 80%, of a goal of at least 80%); a
  share's bound is shown as the file wrote it, unrounded (80.04%).
- **The Goals page** takes the heavy turn by the rows it reads, as a
  summary does: of every team, or more than 53.
- **A goal's task** keeps, beside it in the ledger's meta, the week it was
  made for and what was said on it since, written in the same write as
  each comment: "Missed again", "Met" and "No longer missed", each once a
  week. A week said missed on a task is one a task was made for, so that,
  once the task is done or dropped, `--goal-tasks` makes the next for a
  later week missed, not again for that one. The task's goal is set as it
  is made, and kept through edits.
- **With one press**, Make it a task posts to `POST /goals/task`, the goal,
  its team and the week's Monday, and the server works the week out again
  and writes the title and the text: a week no longer missed is refused, a
  goal with a task open goes to it, and a team's editor makes their own
  team's alone.
- **Words.** A week missed reads "not met" on the Goals page, beside
  "met" and "not known"; a sentence about a goal still says "missed".

### Goals over a period (step 3)

Settled as step 3 was built, over [marked.md](marked.md)'s occurrences:

- **Which occurrences, and where.** An occurrence has ended once the
  ledger's cursor is past its end, as a week is whole once the cursor is
  past its Sunday; until then it is `open`, as it stands. It is in the
  summary's row of the week its end falls in (the week of its last day,
  as #90 counts its days, its end among them: a window is closed at both
  ends, so a sale ending on Monday at 00:00 is in that Monday's week's
  row and digest, not the week before's), and, while it is on, in this
  week's: so a row
  holds, besides its weekly goals, each goal over a period once for each
  occurrence of its kind placed in it, by when each ended. Its entry is a
  week's goal's with `each` (the kind), `occurrence` (its key,
  `7@2026-11-06T11:00Z`), `title`, `from` and `to` (its start and end),
  and, of `no_worse_than`, `no_worse_than`, `by` and `before`. None of
  these is a week's, so a weekly goal's entry reads as it did, byte for
  byte (`TestGoalsWeeklyUnchanged`, held to what 0.55.0 gave).
- **Counted as #90 counts.** Inside and before are worked out by the
  code the Marked windows page reads (`markedData.count`): the kind's
  firings on the days the occurrence covers, by rule; before, the four
  whole weeks before its first week, less the kind's firings, and the
  incidents of those weeks during no occurrence of the kind, of every
  window meeting them, whatever else is read: so an occurrence reads
  alike over the Goals page's thirteen weeks, its own week's digest and
  Make it a task (`readMarked` reads the windows of the weeks it reads,
  which the Marked windows pages share). A team's is
  of the rules it owns and the incidents it owns, of everyone's windows
  and its own, and judged by everyone's record, as the summary's row of a
  team is (the decision above on a team's record), not by the team's as
  the Marked windows page with a team picked judges it.
- **Not known** is tried in a week's order, then one more:
  `not_whole` (the occurrence started in the week the ledger's first
  firing fell in, or before), `team_owns_by_catching`, `no_record` (of a
  measure that needs it: the firings inside all counted and not judged,
  or, with none inside, no week it covers keeps its record, as
  `verdict.NoRecordOf` says of a week), `no_value` (precision with no
  firing judged inside, a median with nothing caught), then `no_before`
  (of `no_worse_than`: the weeks before have nothing to divide, no firing
  judged for precision, no incident caught for the median). An
  occurrence with no firing, in a week whose record is kept, is known:
  "no incident missed during the sale" is met or missed by its
  incidents. `before` is given whenever the weeks before have it, the
  occurrence known or not, as it is a number of its own.
- **No worse than before** is met when inside is at least before less
  `by` (both measures that take it are better higher), within 1e-9, as a
  quotient and a difference are that far off what they mean. Its target
  reads "no worse than before, by 5%" on the Goals page (a lead's slack
  unsigned, "by 1m"), and, of an occurrence, "no worse than the four
  weeks before, 20%, by 5%".
- **The Goals page** has a second table, "Occurrence by occurrence", of
  the goals over a period: the latest occurrence that ended, and those
  ended in the weeks shown and the one on now, the latest 13, the
  earliest first, each a cell as a week's is; how many are left out.
  The weekly table is shown when there are weekly goals. A goal of each
  team's shows its teams missed first, then not known (a team none of
  whose occurrences ended among them), then met.
- **The overview's card** counts, besides the last whole week's goals,
  the occurrences that ended since the last whole week began (so this
  week's rows are read too, when a goal of whom it is for is over a
  period); each missed is a line of What to look at, whose Make it a task
  posts the occurrence's key (`occurrence`) in place of the week, worked
  out again by `POST /goals/task`. A team with goals over a period alone
  isn't told of a week. MCP's `overview` gives `occurrences_met`,
  `occurrences_of` and `occurrences_not_known`, left out at 0.
- **The digest** of a week counts the occurrences that ended in it with
  its weekly goals, and says each missed; `digest.ready`'s `goals` has
  their entries. **A report's `goals` section** says, of a goal over a
  period, the occurrences that ended in the span, whole weeks or not (of
  the 53 latest weeks it meets), with `each` and `occurrences` in its
  numbers.
- **`/metrics`.** A goal over a period's `lapilli_goal_met`,
  `lapilli_goal_known` and `lapilli_goal_value` are of its latest
  occurrence that ended, in the thirteen weeks the leader reads, and
  `lapilli_goal_weeks_met` counts its occurrences met that ended in the
  twelve whole weeks before this one, so of a kind more often than
  weekly it can be over `lapilli_weeks_counted`. They are among the 500
  series of each family, those missed first. The HELP lines are as
  before, so that a goal of each week's read as they did.
- **`goal.missed`** is told of the occurrences that ended since the last
  whole week began, once for each goal, team and occurrence, when it is
  first found missed: one that turns missed late (an incident written
  down a day after), after a later one was told of, is told then, as a
  week turned missed while it is the last whole week is. Each is marked
  by a key of its own, the goal, the team's id and what the occurrence
  is, not its window's row: its kind, its window's team and labels, its
  start and its end, hashed (`goal-occ-missed.sale-none-missed.0.h…`),
  its value the occurrence's end, under a prefix of its own. So a window
  an expression finds again, which `KeepMarks` keeps as a row anew with
  another id (and so another `occurrence` key), is the same occurrence,
  told once; one a person renames is the same, and one whose start or
  end a person changes is another, told once more; two occurrences
  ending at once are two; and a goal changed from each week to each
  kind, or back, takes neither's marks for the other's; the keys
  are let go 13 weeks after the occurrence ended, as a week's are, so
  there are as many as the occurrences told of in 13 weeks. 20 a run at
  most, the earliest ended first, the rest told by the runs after, as 20
  tasks and 20 comments are (a goal of each week tells one a week, 50 at
  most, and isn't bounded so). A goal of each team's is one notice an
  occurrence. The notice carries `each`, `occurrence`, `title`,
  `no_worse_than`, `by` and `before`, and `from` and `to` are the
  occurrence's.
- **Tasks.** `--goal-tasks` makes one for the earliest occurrence missed
  that ended since the last whole week began, that no task was made for
  nor said missed on (`goal-occ-made.`, a key an occurrence, by what it
  is, as above),
  titled with the occurrence ("Goal sale-none-missed missed during sale
  Autumn sale, 2026-10-27", its window's title cut to fit). What is said
  on it is kept by each occurrence's key, with its end: of those that
  ended since the last whole week began as it was made, held by what
  each is as above, another missed
  ("missed again"), the one it was made for or one said missed that no
  longer is ("no longer missed"), whenever each turns so, one ending
  before the one it was made for among them; and one met that ended after
  it ("met"). A task kept as made for a week isn't said on as a period's,
  nor the other way round, once the goal's `each` changes.
- **Cost.** A summary, a page and the leader's round read the windows of
  the goals' kinds meeting the weeks once, and then, once for all of the
  occurrences placed in them, their days of `marked_days`, `marked_weeks`,
  the windows and the tallies from the four weeks before the first's week
  to the last's (seventeen weeks over thirteen, for occurrences shorter
  than a week), and their incidents; each occurrence counted once a team.
  Nothing is read with no goal over a period. `TestPagesScale` holds it:
  the rows and weeks read, the counts against occurrences times teams,
  and the heap.

