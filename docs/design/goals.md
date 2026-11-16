# Goals: what an operations team holds its alerting to

Status: proposed. Under [GOVERNANCE.md](../../GOVERNANCE.md) this is a new
kind of data in the ledger (schema 026), so it is designed first, as its
own pull request. It changes no stone and judges no firing: a goal is
checked against the numbers the summary already gives. The companion to
[noc.md](noc.md), whose `/api/v1/summary` it reads; to
[expectations.md](expectations.md) (#81), which holds one rule to what it
was tested to do, and this the level above; to [work.md](work.md), whose
tasks a missed goal becomes; and to [marked windows](marked.md) (#90,
proposed), whose kinds a goal over a period is checked by.

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

- **A team's budget** (#81's open question 2, how often a team is paged in a
  week over all its rules) is a goal here, `firings`, `at_most`, for the
  team. It doesn't need a fourth kind of expectation.
- **The same firings can break both** in the same week: a rule over its
  budget and its team's goal of re-fires. Each says so in its own place,
  and makes its own task: an expectation's is about its rule, a goal's
  about the goal.

## What a goal is

Goals are declared in `--config` ([serveops.md](serveops.md), "Declared
configuration"), under a fifth key, `goals:`, beside teams, quiet windows,
reports and roles:

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

- **`name`**: `[a-z0-9][a-z0-9_-]`, 64 bytes at most, unique for a team.
- **`team`**: left out, everyone's; a team's name, that team's; `*`, each
  team's own, checked for each team.
- **`measure`**: one of the summary's numbers (below).
- **`at_least` or `at_most`**: one of them, a number; a share is from 0 to 1.
- **`critical`**: the severities counted as critical, for `critical.*`
  measures, as the summary's `critical=` takes them (`critical` when left
  out; 32 at most, 64 bytes each).
- **`each`**: `week` when left out; or a kind of marked window (below).

Read as the rest of `--config` is: strictly, or not at all; read twice
before it is kept; kept in the ledger by the leader, so every replica's
pages say the same; 50 goals at most. A goal names a team the file
declares or the ledger holds, or it is skipped and said why, as a quiet
window of a team that isn't there is.

### The measures

Each is a field of a summary row, so a goal means what the summary means,
with the settings the summary gives (`--refire` for re-fires, the noisy
thresholds for `noisy_only`, the hours for asleep and off hours):

| `measure` | The summary's | Better | Needs the record |
|---|---|---|---|
| `firings` | `firings` | lower | no |
| `precision` | `precision` | higher | yes |
| `critical.firings` | `critical.firings` | lower | no |
| `critical.share_caught` | `critical.share_caught` | higher | yes |
| `refired.firings` | `refired.firings` | lower | no |
| `refired.share` | `refired.share` | lower | no |
| `asleep.firings`, `off_hours.firings` | `asleep.firings`, `off_hours.firings` | lower | no |
| `asleep.nothing`, `off_hours.nothing` | `asleep.nothing`, `off_hours.nothing` | lower | yes |
| `incidents.missed`, `impacts.missed` | `incidents.missed`, `impacts.missed` | lower | yes |
| `incidents.noisy_only` | `incidents.noisy_only` | lower | yes |
| `incidents.median_lead_seconds` | `incidents.median_lead_seconds` | higher | yes |
| `during_deploys.precision_without` | `during_deploys.precision_without` | higher | yes |

A measure the summary doesn't give isn't a goal's. When the summary gains
a field, it can become a measure, in its own change.

## Checked each week

A goal of `each: week` is checked for each week, from Monday in `--tz`, as
the summary's rows are, for its team as the summary counts a team (its
rules' firings, the incidents it owns). Each whole week is one of:

- **met**: the value is within the goal;
- **missed**: it isn't;
- **not known**, with why: the week has no record and the measure needs one
  (`no_record`: no incident on in it or the two weeks before, and no
  impact signal read over it, so nothing tells an incident nobody wrote
  down from none); the value is null (a share with nothing to divide, as
  no critical firing; a median with nothing caught; asleep with no hours
  given); or the week isn't whole in the ledger (`not_whole`: the week its
  first firing fell in, as [noc.md](noc.md) counts "whole" weeks).

"No incident missed" in a week no incident was written down and no impact
signal read is not known, never met. Neither is a goal on incidents met for
a team with no labels: a team owns an incident by a value of its labels or
by catching it (`owners.ownsIncident`), so one with none owns only what its
rules caught, and has never missed one. Such a goal is not known, with
why (`team_owns_by_catching`), and the Goals page says to give the team
labels.

**The week under way** is shown as it stands, not as a result: `open`, with
its value so far. A count over its `at_most` already is shown as over so
far (warn), since more firings can only add to it; it is decided once the
week is whole.

**Worked out when read.** A goal's weeks are worked out from the summary's
rows when a page, an answer or the digest needs them, as every number
Lapilli shows is: an incident written down on Wednesday for last Friday
turns last week's goal met or missed as it turns the week's stones. What
was told isn't taken back (below), and the task made for a missed week says
when the week no longer is.

## Over a period

"During a sale, the alerts did no worse than the weeks before" is held over
each marked window of a kind ([marked.md](marked.md), #90), not each week:

- **`each: KIND`**: the goal is checked for each window of that kind that
  ended, and the one on now as it stands.
- **The value** is the measure inside the window: the firings marked with
  the kind on the days it covers, and the incidents during it, as design
  40 counts them.
- **`no_worse_than: before`** sets it beside the same measure over the four
  whole weeks before the window's first week, outside the kind, with `by`
  as slack (0 when left out): met when inside is no worse than before by
  more than `by`, "worse" as the table's "Better" says. Only shares and the
  median lead take `no_worse_than`: a count over a three-day sale can't be
  set beside one over four weeks.
- **`at_least` and `at_most`** hold inside alone: "no incident missed during
  a sale" is `each: sale`, `incidents.missed`, `at_most: 0`.
- **Not known** as a week is: no record over the window's weeks for a
  measure that needs one, or before with nothing to divide.

A goal over a span that is neither a week nor a marked window, such as a
quarter, is a marked window of a kind written for it. Until #90 is
built, `each:` takes `week` alone.

## Where it shows

- **A page of its own**, `/goals`, "Goals", under Reporting beside Outcomes:
  each goal, its team, its target, the last whole week's result and value,
  and its twelve whole weeks as cells in the status colours ([ui.md](ui.md):
  ok met, bad missed, unknown not known, no colour for the week under way),
  each with its value and, when not known, why. A goal over a period shows
  its windows instead. With a team picked, that team's goals and
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
  goal over the span's whole weeks, and the windows of a kind that ended in
  it.
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
  `partial`). A goal over a period is in the row of the week its window
  ended, with the window's `from` and `to`. `schema` stays 1: `goals` is a
  field added, and no field changes.
- **`/metrics`**, for the last whole week, everyone's goals without a `team`
  label and a team's with it, as `lapilli_refired_weeks` is:
  `lapilli_goal_met{goal,team}` 1 when met, 0 when missed, no series when
  not known; `lapilli_goal_known{goal,team}`, 1 or 0, so that a dashboard
  tells not known from a goal that isn't there; `lapilli_goal_value
  {goal,team}`, when there is one; and `lapilli_goal_weeks_met{goal,team}`,
  of the twelve whole weeks before this one, beside `lapilli_weeks_counted`.
  A team's Prometheus can alert on `lapilli_goal_met == 0`.
- **MCP**: `summary` gives each row's goals; `overview` gives the card. No
  tool writes a goal.
- **Notices**: `goal.missed`, once for each goal, team and week (or window),
  to the webhook and Slack where notices go, when the week is first
  checked whole and missed.

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
  dropped, the next week missed opens a new one.
- **A week no longer missed** (an incident written down later): the task
  gets a comment saying so, once; it isn't closed for anyone.

`goal.missed` is told once per goal, team and week, by the outbox's
`TellOnce` keyed on them, so a week checked again, or turned met and then
missed, isn't told twice.

## In the ledger

Goals are declared, not made in the UI: the ledger keeps what the file
declares, for every replica to read, and what was done about them, not
their results, which are worked out when read. Schema 026:

- `goals`: as the file declares each: `name`, `team_id` (0 for everyone's,
  and a mark for `*`), `measure`, `at_least` or `at_most`, `critical`
  (JSON), `each`, `no_worse_than` and `by`, and `declared`. Kept to the
  file by `ledger.Reconcile` as teams and windows are, a goal matched by
  its name and team. A goal the file no longer declares is deleted: with
  no form in the UI, there is nothing to give it back to, and its weeks
  are worked out from the summary, so nothing is lost but its tasks'
  link, which stays as text.
- `tasks.goal`: the goal a task is about, by name, empty for none.

Upgrading adds a table and a column, with nothing to backfill. Going back
takes the ledger as it was, so a deployment keeps a copy of it (a snapshot
of its disk, or of its database) before it upgrades.

## Cost and bounds

- **A goal reads the summary's rows**, which are cached until the ledger
  changes. Twelve weeks for everyone and one team is two summaries of
  twelve rows; [noc.md](noc.md) measured 53 weeks for 57 teams over 200,000
  firings at 0.16 s. A page with `*` goals for every team reads every
  team's rows: in the heavy turn, as a summary of every team is, and kept.
- **A goal over a period** reads #90's counts of each window, which
  read `marked_days` and four weeks of weekly tallies.
- **50 goals.** `/metrics` gives each goal for everyone and each named team,
  and a `*` goal for every team, 500 series of each family at most, those
  missed first, as the incident series are bounded.
- **The digest** reads the week's rows once, for every goal and team it
  sends to.

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
- **Weeks other than the summary's**: months and quarters, but as a marked
  window of a kind written for them.
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

1. **A share with nothing to divide.** A week with no critical firing is not
   known, as proposed, or met: for an operations team it is a good week.
   A goal could say which (`when_none: met`).
2. **Goals in the UI**, by admins, or by a team's editors for their team's
   goals, as their quiet windows and reports.
3. **Months and quarters** as `each:`, without writing a marked window for
   them.
4. **Weeks in a team's zone.** The summary counts weeks in `--tz`; a team in
   another zone may want its own Monday.
5. **A team's record.** The summary's `no_record` is everyone's, also in a
   team's row; a team's goals on incidents could be held to the team's own
   record, as its pages are (`teamKeeps`).

## Steps

1. `goals:` in `--config`, schema 026, the Goals page, the summary's
   `goals`, `/metrics`.
2. The overview's card and line, tasks (`tasks.goal`, `--goal-tasks`), the
   digest, reports' `goals` section, `goal.missed`.
3. Goals over a period (`each: KIND`), once marked windows are built.
