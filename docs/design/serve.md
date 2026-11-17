# `lapilli serve`: a ledger of verdicts

Status: built, for v0.6.0. This is how it works and why, and the rules that
keep it fast as more is added.

## Why

`lapilli verdict` runs, prints its report and is gone. What it leaves is a
Slack message and a few numbers on a Pushgateway, and so it can't do three
things a team needs week after week:

- **Show a trend.** "This rule has been noisy six weeks running", or "since
  its `for` went from 5m to 15m it fires 13 times a week, not 39", needs last
  week's verdict beside this week's.
- **Take incidents written down later.** Incidents are written down hours or
  days after, if at all. Monday's run never sees the incident written on
  Tuesday, and the thinner the incident log, the worse every verdict is.
- **Replay the rules as they stood.** The weekly run replays the rules as they
  are when it runs. `--rules FILE@TIME` fixes that, but someone has to know
  when each rule changed.

`lapilli serve` keeps the urn instead of emptying it: a ledger of the
firings, the incidents and every version of the rules, recorded as they
happen, and judged for any period when someone looks.

## What stays the same

- **One binary.** `lapilli verdict` and the CronJob chart stay as they are;
  `serve` is another command in the same binary and image.
- **One engine.** `serve` replays with `internal/replay` and gives stones
  with `verdict.Catches` and `verdict.Refired`, the rules `verdict.Judge`
  follows. A test holds its verdict on random weeks to `verdict.Judge`'s.
- **Reads by default, outward.** It reads history, rules and incident files, and
  sends only the weekly digest and notices of what people did, where it is told
  to. It doesn't route alerts, page anyone or manage incidents. Since v0.22.0
  it changes rules only when turned on (`--changes`), and only what a person
  approved ([changes.md](changes.md)): a pull request through a recipe, or a
  write to the Grafana it reads (`--grafana`) or, since v0.23.0, to the
  ruler it reads (`--ruler`), in the namespaces `--changes-in` names
  ([rulers.md](rulers.md)), or, since v0.24.0, to a cloud's own alarms it
  reads (`--alarms`), those `--changes-in` names ([alarms.md](alarms.md)),
  and nowhere else.
- **Not a controller.** It reads mounted ConfigMaps and HTTP APIs. It doesn't
  watch the Kubernetes API and needs no RBAC: a rule resource in a cluster
  is changed by a pull request.
- **Every cloud alike.** The same `--auth aws`, `azure` and `google`, and on
  Oracle Cloud the Prometheus you run on OKE; a cloud's own alarms read and
  changed on all four, OCI's included.

## Shape

```
                       ┌──────────────── lapilli serve ────────────────┐
rules: files, API, ───▶│ watcher  every minute: a new version of the   │
       Grafana         │          rules when they change               │
history: Prometheus ──▶│ worker   one job at a time: hourly runs,      │──▶ web UI
incidents: file, UI ──▶│          change reviews, the weekly digest,   │──▶ /api/v1 (JSON)
                       │          judging again, pruning               │──▶ /metrics, for any Grafana
                       │ ledger   SQLite: facts, stones, tallies       │──▶ Slack, weekly
                       └───────────────────────────────────────────────┘
```

Packages: `internal/source` reads history and rules for both commands;
`internal/ledger` is the ledger; `internal/serve` the watcher, the worker,
the UI, the API and the metrics; `cmd/lapilli/serve.go` the flags.

## The ledger

One SQLite file, through the pure-Go driver `modernc.org/sqlite`: no cgo, so
the image stays distroless and static. Or a Postgres database, through pgx,
pure Go too, with the same tables and statements ([postgres.md](postgres.md)). It adds about 5 MB to the binary.
Also considered: `github.com/ncruces/go-sqlite3` (about 6 MB); bbolt (no SQL:
harder for the next maintainer to read and query); JSON files (no
transaction to replace a period's firings in).

| Table | A row is |
|---|---|
| `versions`, `docs` | a version of the rules, from when it was seen, with the documents it was read from (each kept once, gzipped) and the name each rule is judged under |
| `rules`, `series` | a rule; an alert, that is a rule and a label set |
| `firings` | an alert from start to end, and its stones: `white`, `refired`; and whether it came during a deploy (`deploy`, [noc.md](noc.md)) |
| `deploys` | when a service or namespace was deployed, as `--deploys` reads it |
| `incidents` | from the incidents file, the UI, or `--impact`; for an impact window, whether it was confirmed or dismissed, and whether it counts |
| `catches` | each rule's first firing that caught an incident |
| `days`, `weeks` | each rule's stones by the day and the week its firings started |
| `runs`, `results` | what each run did; what needed Prometheus: suggestions, change reviews |
| `settings`, `changes` | judging settings and incident criteria changed in the UI; who changed what, when |

Times are Unix seconds. Days and weeks are counted in `--tz`; opened in
another zone, the ledger counts them again. The schema changes through
numbered SQL files embedded in the binary, applied at start; a ledger
written by a newer `lapilli` is refused, not touched.

### Stones are kept, and given again only where they can change

A firing's stones depend on the incidents near its start (white) and on the
same alert's previous firing (refired). The ledger keeps them, and every
write ends by giving stones again over the span it changed, and only there:

- a run: the firings from its period on;
- an incident written, changed or deleted: the firings from `--lead` before
  its start to its end, before and after the change;
- an impact window confirmed or dismissed, or covered by an incident
  written down: the same, for the window.

For that span it gives the firings their stones, works out again which
rules caught each incident whose window meets it, and counts those days and
weeks again. Everything happens in one transaction, or none of it does.

A firing is judged against every incident the ledger has, so a firing
just before a week starts can catch an incident in that week; `lapilli
verdict` over that week alone sees only the week's firings. Inside a period,
the two agree.

A change to `--lead`, `--match` or `--refire` gives every firing its stones
again: the worker does it a week at a time, each week its own transaction,
so UI writes get their turns, and the page says how far it has got. The
noisy thresholds aren't kept in stones; they are read when stones are
counted, so changing them costs nothing.

## Runs

Every `--every` (default 1h) a run replays from the end of the last run, less
`--settle` (default 1h) for points that arrived late, to now. Each rule's
replay starts earlier by its `for`, `keep_firing_for` and group interval,
rounded up to 15m, 1h, 6h, 1d or 7d, so a firing's pending time is seen;
rules that start alike are replayed together and share their requests, the
latest first. The impact query (the criterion in effect, `--impact` or
the UI's) goes before them, with time of its own.
The run then, in one transaction:

1. ends the alerts the ledger has as still firing that the replay saw end,
   and those it no longer sees at all;
2. replaces the firings that start in its period;
3. does the same for the windows `--impact` finds, keeping what people
   decided about the windows they overlap;
4. gives stones again over the period, and moves the cursor.

Recording the same hours twice changes nothing. A firing the replay saw
start from pending is matched only to the same firing; one it saw already
on, to the alert the ledger has on.

A rule whose queries failed, or that the run had no time left for, keeps
the firings it had, and the ledger keeps where it was missed: later runs
replay that rule from there (for up to a week), so no hour of it is lost.
While catching up (the first start replays `--backfill`, 28 days; or after
being down), a run covers a day at a time; one that doesn't finish within
`--timeout` is tried over half of what is left, and the next grows again
only after one that finished.

Each hour of history is replayed about twice (once when new, once settled),
plus each rule's margin, so `serve` asks Prometheus for about two to three
times what the weekly CronJob does, spread over the week instead of all on
Monday morning.

### Versions of the rules

The watcher reads the rules every `--watch` (default 1m): the files of a
mounted ConfigMap, `/api/v1/rules`, or Grafana's export; when the
documents are as they were, that is all. When the digest of the rules (every rule's
`Key`) changes, it records a new version, in effect from then: the
`--rules FILE@TIME` a person would otherwise have to work out. A rule keeps
the name it had in the version before (by group, name, labels and uid); a
new one sharing a name is named apart alongside the rules before. The rules
are read before the first run, and runs wait until a version is recorded;
the rules before the first start are taken to be those found then.

### Change reviews

Each new version is replayed over the week before it, the rules that changed
as they were and as they are (`--compare --changed-only`), and the comparison
is kept for the rule's page and the overview. The ledger keeps the last
version reviewed, so a restart doesn't drop a review.

### The weekly digest

At `--digest` (default `mon@09:00`, in `--tz`), once the week before is all
in the ledger, `serve` judges it, replays its noisy rules with longer
`for`s for suggestions, and keeps them, once; then posts the same report the
CronJob posts to Slack, trying the post again at most hourly until it goes.
A digest missed while `serve` was down is worked out when it is back.

It also looks at the week's incidents that nothing caught, or only noisy rules
did, as `lapilli verdict` does: for each of the first twenty, it asks when each
rule in effect then held its condition, on the steps the runs replayed it on,
from the rule's `for` before the incident to as long after, within the week,
and keeps the longest shorter `for` that would have caught it, five rules at
most for each. The first ten such rules tried are replayed over the week with
it, as each version stood and with the quiet windows excusing what they
excuse, to say what it would cost. A rule the same across versions is asked
once, the questions are 2,000 at most, and the suggestions and these share one
`--timeout`; with the firings by team read after them, in a minute at most,
the digest holds the runs up for `--timeout` and a minute at most. They are
kept as a result (`near_misses`) beside the suggestions, not in a table of
their own, and shown on the incident's page, in the digest (a team's, for its
incidents) and through MCP. A rule whose condition compares a value with a
number is looked at by its value too: whether the value came within a fifth
of the number, for its `for`, and the round number short of it that would
have caught the incident, fired with its `for` and `keep_firing_for` as the
replay fires (looking back as far as `keep_firing_for`, up to a day, for a
firing kept on through a dip), and replayed over the week the same way. The history is asked
for the rule with the furthest round number within a fifth in place of its
own, so it returns no more than could come near. Of a condition with several
numbers (a Grafana range, PromQL joining comparisons with `and`, `or` or
`unless`), each number is looked at, four at most, the others as they are:
the first comparison of an `and` or `unless` by its values, which the
expression returns where the rest holds, and any other by replaying the rule
over the window with nearer round numbers. A nearer number holds less, so the
furthest within a fifth is asked first, and only when the rule with it fires
during the incident is the way back halved, five questions or so more; most
numbers cost one question. Each window starts far enough before the incident that a
firing its start makes up, the condition holding from before, fires before
the incident's lead and isn't taken for a catch; and what held from the
window's start, with what followed it within `keep_firing_for`, is left out
of what could catch it, as when it fired isn't known. A near miss by `for` is
checked by replaying the rule over the window with the shorter `for`, one
question more for each found. The comparisons are read as the single one is, by
brackets, strings, comments and the words `and`, `or`, `unless`, `on`,
`ignoring` and `bool`, with `and` and `unless` binding before `or`; each
level of brackets reads its text again, so an expression over 16 KiB, or a
comparison nested more than 32 levels (brackets, and, or, unless), isn't
read. The search for a nearer
number is on whether the rule fires during the incident at all, which only
grows as the number moves further: the nearest that does catches it, unless
it fired before the incident, and then none does.

An incident written down after the digest, or in the week under way, needn't
wait for the next one. Each round of jobs that didn't work a digest out
afresh looks at the twelve weeks kept, for the incidents to look at again:
those written down, changed (each counted incident's id and when it was last
written are kept for its week in `meta`, `near:WEEK`) or taken away since the
week's near misses were last worked out, and those whose end, and `--settle`
after, the ledger's firings have reached since. Only those are looked at,
over the week up to the ledger's cursor; what is found for them takes the
place of what was kept for them, and the week's other incidents keep theirs,
in one `near_misses` result for the week that takes the place of the last.
The incidents are read before the work, so one written down meanwhile is
looked at next time. The latest week goes first, all within one `--timeout`.
Twenty incidents nothing caught are looked at a round, the digest's
included; the rest are left unseen, for the next. When a question of the
history fails, or the time runs out, what was kept stays (an incident that
had nothing kept gets what was found for it), and the same incidents are
tried again after an hour, then two, four, up to a day, or as soon as they
change; a digest that fails so leaves its week to be worked out again. A
rule paused or turned off for part of the week has no cost to tell, which
isn't a failure. The digest and the incident's page show near misses only
for incidents still not caught. A week from before this was kept,
with no digest's result, isn't looked at. `lapilli verdict` works them out for any period.

**No record.** `serve` judges a firing by its week: a week's firings are
judged when the record of the week is kept, an incident (written down, or
an impact window that counts) on at some time in it or in the two weeks
before it, one with no end taken to last an hour, as it is caught for; or the impact signal read, by the runs, over all of it (`impact_since`
in `meta`, the earliest a run read `--impact` from, with no run since that
didn't: a run without it sets it to `none`; after an upgrade it starts
with the first run that reads it, from that run's start, as an earlier
lapilli kept no word of when its runs did: `verdict.Options.ImpactFrom`;
"The impact criterion", below). A span that isn't whole weeks
is judged by its weeks' records, whole, and has no record only when none
of its weeks has. What `serve` replays (a change's backtest, the try page,
the digest's suggestions and near misses) is judged by the same weeks
(`verdict.Options.RecordWeek`, the ledger's own weeks), given the
incidents that keep them, so that it compares like with like. Precision
everywhere is of the firings judged: the rules list's and a rule's, the
span before a rule's period, the outcomes' then and now, during deploys,
and MCP's. The incidents that keep the record are read by when they ended
(schema 018's `incidents_end`), so a page reads those near its weeks
alone. Any other week's firings
are counted, and not judged: a firing that caught nothing can't be told from
one nobody wrote down. They are in no precision and make no rule noisy, and
the stored suggestions of such a week are shown nowhere: not on the pages,
the rule's form, a report, nor the digest worked out again. `lapilli
verdict` judges each firing by the record at its own time, the two weeks
before it; `serve`, by the week, so that what the rules list and a rule's
page say of four or twelve weeks is the sum of what the overview says of
each. With a team picked, and in its digest and reports, by the incidents
it owns; a team with no labels, which owns an incident only when its rules
caught it, by everyone's, or its rules' firings that caught nothing would
never have a record. What keeps the record is read once with the weekly tallies
(`RecordIncidents`), never from the firings; a report over a span, week by
week, from the daily tallies. The digest says so once, and, for each rule
replayed that fired as often as a noisy rule with no record, the ten that
fired most, works out what those firings were (`verdict.Unrecorded`, kept
as an `unrecorded` result for the week, said for everyone's record or for a
team's): of a condition that counts events (`rules.Rule.EventWindow`, a
Grafana rule's too), each is one, a count that went up, unless one fired on
no data (a Grafana rule's, labeled `grafana_alertstate="nodata"`), which is
none, or more than 5,000 are to be read to tell (said: "Too many firings
to tell how many happened"); of another, how many had the value its
condition compares go well past its bound, from the rule replayed over the
week with its one bound a fifth further (a firing of that one overlapping
it, alike in its labels, went well past it; a condition with no one bound
that decides it, a range, `and`, `or`, `unless`, says nothing, nor does a
rule with more than 5,000 such firings); and the alerts that fired most, of
the 1,000 counted most in one grouped read (`RuleAlerts`), by the labels
that tell them apart, each with the team whose labels it has, and those
during deploys. Its reads and replays share the digest's `--timeout`. A
week whose record is kept since, by an incident written down later, no
longer shows it; a result kept that doesn't decode is logged and taken as
none.

**Firings on what a team owns.** The digest also counts each rule's
firings in the week by the team whose labels they have (the teams'
labels, the first by name, as a rule's are), from one grouped read of the
week's firings by alert (`AlertCounts`, the 20,000 alerts that fired most,
those past them no team's: a rule's `unread` says how many of its firings
were, and the digest logs it), in a time of its own after the replays',
`--timeout` or a minute, whichever is shorter, which, run out, keeps none
and logs it; a rule most of whose firings, more
than half, came from what another team owns than the one its alerts go to
(or from a team's, for a rule no team owns) is kept as a `fired_on` result.
A rule's page, the rules list with a team picked and that team's digest say
it from there.

The numbers and the text also go through the outbox ([work.md](work.md)), once
(a webhook added on a Friday doesn't hear of Monday's digest):
`digest.ready` to the webhook, for everyone and for each team something
happened to, and to each team's own webhook (`--team-digest TEAM=FILE`), its
own rules and the incidents it owns. A team's URL is in a file named on the
command line, not worked out from a team's name, which anyone who may change
teams can write. A week rarely has the incidents to call a rule spare, so the
digest says which are over twelve weeks instead. `digest.ready` carries the
report as written (`text`), which a receiver escapes for where it sends it, and
as the Slack digest posts it (`slack_text`).

### Outcomes

The reviews are predictions, and the ledger holds what came after them, so
`serve` can say whether a change did what its review said, from what it
already keeps. A rule changed at a time is measured over up to four weeks
before it (from its last change, if later) and up to four after (to its
next): firings a week, incidents caught, and firings fewer since than at
its rate before (none, for a rule written: a rename nets out). It came
out as predicted when its firings a week since are within a quarter, or
two, of the review's after, scaled by how the weeks before compare with
the review's week. Under a week either side, it is still being measured.
A rule that fires alike in the review makes no prediction, and the change
before it goes on being measured; one paused, or that can no longer be
replayed, ends that change's measure and has none of its own.

Beside the changes: the four weeks before `serve` first ran (kept in the
ledger from the first run on), or its first four with less kept from
before, and the latest four; whether the record is kept up (the latest
incident written down, flagged after two weeks without one, impact
windows to decide, failed runs, rules not replayed); and whether it is
used (edits through the UI and API, by whom with `--user-header`,
suggestions a task was made for or whose rule changed, tasks done). Page
reads aren't counted, and nothing is sent anywhere: the outcomes are on
`/outcomes`, summed up on the overview and in the digest, at
`/api/v1/outcomes` and in `/metrics`. They are worked out once for each
version of the ledger, for all of these; a rule's days are indexed by rule
so that measuring a change reads only its own.

## Incidents and settings

Incidents come from `--incidents FILE` (read again each run; the file owns
them), from the UI and API (ids `L-1`, `L-2`, … unless given), and from
`--impact`. An impact window can be confirmed, which writes it down as an
incident, or dismissed; one an incident written down covers doesn't count.
On a black stone, "this was real" writes down an incident from the firing's
start, end and `--match` labels.

An incident written again as it was, to the second, is no write: no change
noted, no notice, no judging again, and no page thrown away. That is what lets
a job copy incidents from where they are kept, every hour. An incidents file
with no `incidents` key is refused and the ledger keeps what it had, since a job
that failed leaves an empty one; the file owns what it holds, and it would
otherwise take every incident away.

Such a job has a port of its own, `--incidents-listen`, that takes
`PUT` and `DELETE /api/v1/incidents/{id}` and `POST /api/v1/incidents` from
whoever holds the token in `--incidents-token-file`, and has nothing else on it.
Letting the job reach the UI's port instead would let it name itself anyone, in
the headers the sign-in proxy sets; here they mean nothing, and what it writes
is noted as `incidents-token`, which outcomes don't count as use, and new
incidents from it aren't told: the tool has told people already, and a first
copy of months of history shouldn't go one line at a time. The name can't be
taken through the sign-in proxy's header.

What it copies in has an origin of its own, `copy`, and is the tool's, as the
file's incidents are the file's: the UI and API on the other port can't change
it, and the token can change nothing else. So whoever holds the token can't
quietly move or take away an incident a person wrote, and a tool's id that
happens to be `L-1` can't write over one. The incidents file is whoever runs
`serve`'s, so it takes an id a copy got to first; an id a person wrote, or an
impact window's, is passed over and the rest of the file read. That stays on the
status until the file reads cleanly, and `/metrics` counts it
(`lapilli_serve_incidents_passed_over`).

The port takes one write at a time, 20 a second, with bursts of 40, and answers
429 past that: each write holds the ledger's one writer, and is kept. The
ledger keeps 20,000 incidents copied in at most, and notes the token's changes
without their bodies, since the tool keeps what its incidents were. An
incident's labels are 4 KB at most in all, as they are kept, and its times from
1970 to 9999 in UTC; a change to an incident's title alone judges nothing again. The token is read for each request, so it can
be rotated, refused when shorter than 16 characters, and both are hashed before
they are compared. The port is off unless both flags are given.

**What needs Prometheus again is set where `serve` starts; what only judges
the ledger again can also be changed in the UI**: `--lead`, `--match`,
`--refire`, `--noisy-firings`, `--noisy-precision`. A change is kept with who
made it, can be tried on the last week before it is saved, and
`/settings.yaml` gives the changes as Helm values to commit.

**The impact criterion.** `--impact` and `--impact-for` can be changed in
the UI too, by an admin (ui.md, "Incident criteria"), though they read
Prometheus: they decide what an incident is, the cheapest way to verdicts
with no incident written down. They are kept in `settings` as the others
are, the UI's over the command line's, `off` for none; settings.yaml gives
them as the chart's `impact` and `--impact-for` in `extraArgs`.

The record by the impact signal follows one rule: **it holds only for
weeks that were read, with data, by the criterion in effect, and, after a
change, replaced by a successful read of that criterion.** Kept in meta,
with no schema change: `impact_since`, `impact_criterion` (the criterion
the record is kept by, the expression and `--impact-for`), `impact_again`,
`impact_data` (where its data begins), `impact_read` (what the last run
read) and `impact_floor`.

- **Read by the runs.** From `impact_since`, all along, the record is
  kept (`verdict.Options.ImpactFrom`, `ledger.ImpactKept.From`), never
  before where the data of `impact_criterion` begins, and not at all
  before that data is looked for. A run that reads another criterion
  than `impact_criterion` keeps a floor at its start (`impact_floor`):
  from there the ledger's windows are another's, and no record is kept
  by the signal, before the floor either, as the record has one start
  and no end (`verdict.Record`): what is kept is never more than the
  rule holds, and the rule is unchanged. The floor stands until a read
  again of the criterion in effect then succeeds, whatever the runs read
  meanwhile: more saves leave it where it is, a run that replays from
  before it moves it there, and saving `impact_criterion` back still
  needs it read again, its data and windows before kept as they were. A
  backtest made while a floor stands is labelled with it (`pending`,
  the floor, the criterion kept and the one last read: no criterion
  kept is that label), and is held once the read again is done, the one
  kept saved back among them. Read after a run that didn't (turned on
  again), the record is the new one's from the run's start, and a read
  again is due. A window found by a run that read another criterion
  than the run before (`impact_read`) and on past the run's start ends
  there; one of the same criterion goes on, a floor or not.
- **Saved.** A new criterion saved in the UI leaves the record as the
  ledger holds it, by the one before, until a run reads it (a floor,
  which the Settings page says) or it is read again; saving it
  needs it read in full over the last four weeks first (by the save, or by
  a try within half an hour), within 90 seconds and `--max-samples`, in
  the heavy turn, or nothing is saved. The read again it makes the
  replica that leads do counts among the saver's twelve reads an hour,
  whether a try read it lately or not.
- **Read again** (`impactAgain`, before the runs), when `againPlan` says:
  a criterion saved, or started with, since the runs read another, or
  turned on after they read none. Over the last four weeks, from the
  Monday four weeks before the cursor's week, or where the runs began, to
  the cursor, within `--timeout` and `--max-samples`. In one write
  (`ledger.ReadAgain`) its windows take the place of those found from then
  on; a window found before and on at the start ends there; what people
  decided about a window found before holds for a new one it covers half
  of or more, sharing a value of each label both have (of several, the
  one that covers most, confirmed before dismissed on a tie): half of the
  new window, so that one split or found shorter keeps what was said of
  it. A key, and the tasks and comments on it, need more: half of both. A
  window found keeps the key of one found before only when it is the same
  one (the same minute, sharing labels, the two sharing half of each),
  and gets a key no window, task or comment has otherwise; the tasks and
  comments on a window no longer there move to the new one that covers
  most of it, sharing labels and half of each, or stay on its key, listed
  on the Cleanup page. `impact_since` is the start, or where the data
  begins, later, the criterion read is `impact_criterion` and
  `impact_read`, and the floor is gone. The
  span is pended, and judged again a week a write (`JudgePending`), as
  `JudgeAll` does. The weeks before keep the windows the one before
  found, as the incidents they were when read, which keep the record of
  the weeks near them as any incident does (two weeks); the signal read
  with the one before keeps no more. One that fails is tried again an
  hour later, then two, four, up to a day; meanwhile the runs read it
  from where they are, under a floor: no record by the signal.
- **Marked as read all along**, with no read again, when the runs read
  the criterion from where a read again would begin, or before, all
  along: a new ledger's runs read it from their first. A ledger a lapilli
  before 0.49 kept has `impact_since` and no criterion: it is taken to
  have been read with the command line's criterion, and only that one is
  marked; another, saved in the UI before the first round, is read
  again. The first run after the upgrade keeps the command line's as
  `impact_criterion` (`Found.ImpactAssumed`), whatever it read: one that
  read another keeps a floor, as above.
- **Where the data begins** (`impactData`, after the runs, of
  `impact_criterion`, not while a floor stands, and in a read again). For each
  selector the expression reads (`rules.Selectors`, 32 at most, with the
  offset and the @ written after it, as they move where its data is read:
  `count(x offset 1w)`), the first sample of any of its series, as
  `count(selector)` has it at a step of an hour (coarser over a long span:
  one request a selector, within `--timeout` and `--max-samples`): a
  series added late moves nothing. An expression that reads no selector
  (`vector(1)`) has data all along: the record is kept from
  `impact_since`. Across selectors, the latest of those: the record starts no
  earlier than the first whole week after it, as only there did every
  selector have data, and not at all while one has none. A selector with
  no data, or later data, that only narrows another (`rules.Covers`: the
  same offset and @, each of the other's matchers, its metric's name among
  them, one of its own) begins where its stand-ins' data does
  (`rules.StandIns`, 32 at most, looked for as the selectors are, kept in
  `impact_data` with `for` and `of`): of each selector it narrows, that
  one's series with its metric's name and each other label it narrows them
  by with a matcher the empty value doesn't match, any value, as
  `http_requests_total{code!=""}` for `http_requests_total{code=~"5.."}`
  beside `http_requests_total`; every stand-in made of a selector with
  data of its own having data, the latest of them. A client library makes
  a 5xx series at the first 5xx, so a service that served none had nothing
  to match, not nothing to read; a code label that came later, by a
  relabelling or a rename, holds the record back to where it came. A
  stand-in made of a selector with no data of its own says nothing, and
  isn't looked for; one not looked for yet keeps no record that round,
  rather than the later start of the selector's own. Not a selector an
  `absent()` or `absent_over_time()` reads, at any depth. What it can't
  tell: values of another form (`Internal` where `5..` is matched) have
  the label, so a selector that can never match keeps the record. A ledger
  kept before stand-ins looks for them at the next round, and gives back
  their `for` to entries a lapilli before them wrote back. The settings
  page says of such a selector "nothing matched since", or "kept from",
  with its stand-in.
  It moves
  `impact_since` forward, never back, on a first start, after an upgrade
  (from the `impact_since` the ledger has), and once a criterion is read
  again; a run that moves `impact_since` back before where the data was
  looked for from (one replaying from further back than a read again
  began) forgets it, to be looked for again from there, no record kept
  until it is; a selector with data where the look began holds nothing back;
  one with none yet is looked for again a round at a time, each selector
  from where its own last look got to, one that fails passing to the
  next, those it could look at kept; once each had data, nothing is
  looked for again. The
  Settings page, the try and the Incident sources row list each selector
  and where its data begins, and name the one that holds the record back:
  an expression of its own for it would keep the weeks before. This looks
  for where data begins, per selector, not for gaps in it after, nor for
  a selector whose data ended.
- **Turned off**, the runs set `impact_since` to `none` and forget the
  criteria, the data and the floor: no week is kept by the signal, and
  the windows found stay, as incidents.

Which weeks are kept is the same rule as before (`verdict.Record`), its
input changed: `verdict.Semantics` isn't bumped. Kept or not is worked out
when a page, a proposal or a report reads it, so a record that moves needs
no stones given again. A proposal waiting for approval whose backtest's
weeks flip, kept where they weren't or not where they were, is held until
it is backtested again (`staleRecorded`, `staleUnrecorded`); one whose
backtest was judged against the windows of another criterion than the one
read now (`evidence.Impact`, kept from this release on) is held too (`staleImpact`),
read again or turned off. The try counts them as saving would. The
digest's results kept by week (the suggestions, the rules that fired as
often as a noisy one with no record) are worked out by replaying the
rules, and aren't worked out again on a save: the overview's weekly panels
that read them catch up at the next digest, which the Settings page and
the try say. A week with no record is shown no suggestion, whatever was
kept for it.

**An upgrade.** A ledger 0.41 to 0.48 kept, whose `impact_since` is
earlier than its expression's data: at the first round of the replica that
leads after the upgrade, the criterion in effect, when it is the command
line's and the runs read it from where a read again would begin, is marked
as read all along (else it is read again over the last four weeks); after
the runs, where each selector's data begins is looked for from
`impact_since` to the cursor, and `impact_since` moves forward to the
first whole week after the latest first sample; until it is looked for,
no record is kept by the signal. No stone changes; the
weeks before it have no record unless an incident keeps them, their
firings counted and not judged, and the proposals whose backtest had a
record by those weeks alone are held.

**Where incidents come from** is on the Settings page (ui.md, "Incident
sources"). The incidents port keeps when each source last wrote, unchanged
or not, by its ids' prefix (`PD-`, a Jira project's key), in meta
`x.incidents-received` (64 sources at most: past them, one not known is
kept among the others, `*`, so ids of no source can't push the known
ones out), once in
ten minutes by a replica and aside, emptying no page kept: what tells a
job that stopped from a tool with nothing new. The incidents file's and
the impact signal's last reads and failures are the leader's, in its
published status.

## Web UI, API, metrics

Pages are rendered on the server with `html/template`, embedded in the
binary with their CSS: no scripts, no JavaScript build. Charts are SVG drawn
on the server. Pages are in the reader's language (English, Spanish, Japanese or
Korean, from a choice kept in a cookie or the browser's), kept apart in the cache by it;
a language is one JSON file of translations. The frame of every page (the
sidebar, the breadcrumbs, the team and period carried on every link, the
search) and the rules list's badges and filters are in [ui.md](ui.md).

- `/`: a dashboard of the last twelve weeks, read from the weekly tallies
  and the weeks' incidents: last week's precision, firings, incidents
  missed, median time to catch, noisy rules and the firings that caught
  nothing while people slept beside the week before, each with its weeks drawn
  small; firings (caught something, caught nothing),
  precision and incidents (caught, caught only by noisy rules, missed) by
  week; the noisiest rules' black stones, rule by week; what to look at
  first; and the same numbers as a table. Its colours were checked for
  colour blindness and contrast in both modes, and every chart has a legend
  and the table, so no colour carries a meaning alone.
- `/rules`, `/rules/NAME`: each rule's stones over 1, 4 or 12 weeks, how long
  it has been noisy, its suggestion, what it adds to the others, how many of
  its firings that caught nothing came while people slept or off hours; a
  rule's numbers beside as long before, its last 84 days, and in tabs its
  versions, reviews, catches and latest firings ([ui.md](ui.md)). When firings came is
  worked out as pages are read, from when each started: by the hours given,
  `--work-hours` and `--sleep-hours`, in `--tz`, or `--shifts`, or by the
  rule's team's own (a team's `work_hours`, `sleep_hours` and `tz`, each
  taken from the server's when empty, or its `shifts`, schema 007). Shifts
  are hours that take turns: a firing goes by the hours of the turn it came
  in, worked out from the turn's length and the first one's start, with no
  table of turns kept. SQLite counts a stretch of hours at a time along the
  index on starts, and each week is kept until the ledger says that week's
  firings changed, so a run counts the last week or two again, not twelve.
  Hours that change more than six times a day, as short shifts round several
  zones do, are counted with one read of the firings instead, row by row.
  Hours are counted by schedule, not by team: each schedule in use reads the
  firings once a span, and its weeks are kept by week and schedule, so a
  team's schedule changing counts on the next page read without counting the
  server's again. `/api/v1/report` counts them for periods of 31 days
  at most, one such report at a time, as reports with every firing are.
- `/outcomes`: whether Lapilli earns its keep: the reviewed changes, measured;
  then and now; the record; the use (see Outcomes).
- `/incidents`, `/incidents/ID`, `/firings?day=`, `/settings`, `/runs`.
- `/search?q=`: rules, incidents and teams by name ([ui.md](ui.md)).
- `/api/v1/report?from=&to=`: `lapilli verdict --json`'s report for a period
  (up to 31 days with every firing, a year without: `firings=false`);
  `&format=csv`, its firings a row each, with those quiet windows excused
  and each rule's team, under the same bounds and turn
  ([docs/numbers.md](../numbers.md));
  `&redact=true` gives every name an alias, as `lapilli verdict --redact`
  does, the aliases that answer's own.
  `/api/v1/incidents` (GET, POST, PUT, DELETE), `/api/v1/rules`, `/runs`,
  `/settings`, `/outcomes`, `/summary` (each week and team, for dashboards
  and other tools: [noc.md](noc.md)).
- `/metrics`: `--metrics` over the last seven days, and `serve`'s own (the
  cursor, how long the last run took, runs by kind and result; the firings
  the reviewed changes avoided, the changes by how they came out, when the
  latest incident written down started, the edits of four weeks). The chart
  ships a Grafana dashboard of them that works on any Prometheus-compatible
  data source.

### MCP

`/mcp` serves the [Model Context Protocol](https://modelcontextprotocol.io)
over Streamable HTTP, with the official Go SDK. It keeps no sessions, and
each answer is one JSON object, so any proxy in front passes it as it
passes the API.

The tools are the pages' views turned into JSON with a schema: `overview`,
`rules`, `rule`, `incidents`, `firings`, `outcomes`. They read through the
same code as the pages, in English.

`compare` takes changed or new rules as a Prometheus rule file, and names
of rules to take away.

- **Matching.** Each rule given takes the place of the rule of its name in
  effect. Where several share a name, its group picks one. A rule matching
  none is added.
- **Replay.** It replays only the rules that differ, both ways, over up to
  14 days before now less `--settle`. This is what the review of a change
  does (`replayChange`).
- **Limits.** It takes 20 rules given and 50 taken away at most. A `for`
  or `keep_firing_for` (which start a replay that much earlier) is 48 hours
  at most, an interval an hour, and Grafana rules aren't taken. It has 25
  seconds, so that it answers within one request and within the time a
  sign-in proxy waits, and one comparison runs at a time.
- **The expression.** A rule's definition (the `rule` tool) carries its
  expression as replayed, with any scope its source adds, such as a
  namespace. The agent is told to copy it from there rather than from a
  file.

Everyone who reaches `/mcp` gets the tools that read: what they give, the
pages and the API give too, less the files rules were read from.
`compare` is off unless `--mcp-compare editors` or `admins` turns it on,
because it runs PromQL the agent wrote with Lapilli's access to Prometheus;
and the flag is refused without `--editors` or `--admins`, without which
whoever may change anything is an admin.
That PromQL can learn, a count at a time, whatever that access can read;
so can the LogQL `compare` sends to Loki for a rule kept there (since
0.28.0), with Lapilli's token, to the Loki or through Grafana's data source
proxy.
`--read-only` gives no one `compare`.

A tool's panic is turned into its error, as `net/http` does a page's.

A browser always sends `Origin` and an agent doesn't, so a request with an
`Origin` other than `--external-url`'s is refused. A page elsewhere, or a
name a page pointed at this server (DNS rebinding), can't use an agent's
access. The SDK's own localhost check is off: behind a sign-in proxy on the
same host, requests arrive at 127.0.0.1 under the proxy's name. Bodies are
1 MB at most.

The server's instructions tell the agent what the stones mean, the order
to use the tools in, and to treat incident titles and notes as data, not
as instructions.

## Security

- **Closed by default.** The binary listens on `127.0.0.1:8080`; the chart's
  Service is a ClusterIP. Reach it with `kubectl port-forward`, behind the
  sign-in proxy you have, or with a sign-in of its own (since 0.30.0,
  [signin.md](signin.md)), which makes no one an admin for loopback. With no
  roles, everyone is an admin only on a listener on loopback alone, this
  machine's users, as `--listen` says and the address it is bound to is
  (`localhost` is what `/etc/hosts` says); listening beyond it (the chart's `:8080`, which every
  pod in the cluster reaches) everyone may only look, unless
  `--everyone-admin` says otherwise (0.28.1: before, everyone was an admin
  wherever it listened, and a review found any pod in a cluster could write
  incidents and settings through the chart). The log says at start which
  applies.
- **Who.** With `--user-header`, changes are recorded under the name that
  header carries; otherwise as anonymous. Set it only behind a proxy that
  sets it and strips it from callers.
- **Writes** are POST, PUT and DELETE, checked by Go's
  `http.CrossOriginProtection`, limited to 1 MB, their fields to sane sizes.
  `--read-only` refuses them.
- **Host names.** Every route but the health checks answers only:
  - an address, `localhost`, a name without dots, or a name under
    `.localhost`, `.internal`, `.svc` or `.cluster.local`;
  - `--external-url`'s name and `--allowed-hosts`.

  A page elsewhere could otherwise point a name of its own at a local serve
  or a port-forward (DNS rebinding). The browser would then take it for the
  page's own site and let it read and write, past the cross-origin check.
  Over loopback (this machine, a port-forward, a sidecar), names without
  dots or under `.internal`, `.svc` or `.cluster.local` aren't answered
  either: someone with a hand in the company's or the cluster's DNS could
  point them here.
- **Roles from headers** hold only if nothing but the proxy can reach the
  pod. The chart refuses `serve.userHeader` without its NetworkPolicy,
  unless `serve.networkPolicy.othersKeptOut` says something else keeps the
  rest out. `/metrics` and the health checks are served alone on a second
  port (`--metrics-listen`), so Prometheus can be let in there and nowhere
  else. In `--editors` and `--admins`, `user:` and `group:` keep a person
  from taking a group's role by its name. A name the proxy gives with a
  character that doesn't show (a zero-width space, a soft hyphen, a control
  character), or over 200 bytes, is no one's: taken out, or cut, it could
  become another's.
- **Who is who is the identity provider's.** Lapilli takes the names the
  proxy gives as people: one person with two sign-ins is two people to it.
  An admin who has a second sign-in can give it an editor's role, propose a
  change from it and approve it from the first, as two names in
  `--editors` would allow; keeping one person to one name is the identity
  provider's to do ([serveops.md](serveops.md), settled 2026-11-12).
- **The incident criteria read Prometheus.** A try and a save that reads
  run within 90 seconds and `--max-samples`, in the heavy turn, twelve an
  admin an hour in all, the read again a save makes among them (kept in
  the ledger with replicas, so every replica counts the same; in the
  process without, begun again when it starts); a range or a subquery
  over a day refused, one not written as a duration, a subquery stepping
  by less than a minute, an expression of more than 32 selectors. A try is an admin's, asked
  for from the Settings page in a browser (`Sec-Fetch-Site` same-origin,
  or none for one typed): it shows the labels of the series the expression
  finds, so an admin reads, through it, any series' labels Lapilli's
  Prometheus credentials can read. That is an admin's to do; it isn't
  offered to anyone else.
- **Heavy requests.** A report gives 100,000 firings at most, excused or
  not, and so do the settings tried over a week and the incident criteria
  tried over the last four. All take one turn at a
  time, which the next waits up to 30 seconds for and then gets 503. The
  turn is held until the answer is written, with 30 seconds to write it:
  otherwise a client that doesn't read keeps one answer after another in
  memory.
- **Told to Slack.** What people wrote is quoted, so a newline can't start
  a message of their own.
- **Links.** Task and runbook links can't carry a user name, which makes a
  link look like one site's and go to another.
- **Parsers.** Rule files, rule APIs, Grafana expressions, incident and
  firing files, range answers, on-call feeds and what a sign-in is sent
  are fuzzed in CI each week.
- **What a history API answers** is read as it comes, and only what was
  asked kept: a sample outside the range asked dropped, a series of more
  samples than the range's steps unreadable, an answer of more than 50,000
  series, 5,000,000 samples or 256 MiB too big (a shorter range is asked, as
  when the API says so), one of more than 128 MiB of labels (as counted,
  some 100 bytes a label beyond its text: 50,000 series of 2.7 KB)
  unreadable, as a shorter range holds the same series, and a series taking
  more of the stream than its steps could need unreadable. The answers to
  one range (its days, and the halves of one the API finds too large) hold
  100,000 series, 256 MiB of labels and `--max-samples` samples in all at
  most (10,000,000 by default, some 600 MB at the peak, 64 bytes a sample;
  the chart's `maxSamples`); past one the range isn't read, and the error
  names the bound and how to read it all the same: a longer `--step`, a
  shorter range, a larger `--max-samples`. A hostile API's few KB on the
  wire, gzipped, were hundreds of MB of heap before 0.34.1.
- **A replayed alert's labels** are kept as every other alert's (recorded
  firings, notifications, incidents): 4 KB in all, as JSON; a name of 128
  bytes and a value of 256 at most, cut past them, with a hash of what was
  cut (`…` and twelve hex digits), what turns the text around (a
  direction override) as U+FFFD, with a hash too, and a set still past
  4 KB with its longest labels folded into one, `__lapilli_cut__`, their
  hash (an API's own label of that name is folded into it too): alerts
  apart stay apart, but for two values cut alike whose hashes' first 48
  bits are the same (`verdict.KeptLabels`).
  The replay check takes what really fired as it would be kept. A rule's
  replay in one run is kept with 20,000 alerts at most, told apart by
  their labels, each name and value counted after its length under a
  seed made as the process starts; past that, the rule is left as it was,
  as one whose query failed, and says why. The cache of ids and labels
  holds 32 MB at most.
- **Pages** send a Content-Security-Policy that allows no scripts and no
  framing; rule names, labels and incident text are escaped as the
  untrusted text they are. A rule's name, group, labels, its group's
  labels and what it says are read with what turns the text around them
  (a control character, a direction mark, embedding, override or isolate)
  as U+FFFD, as a tracker's comment is taken; its expression, whose query
  is its own, is shown so; names are set apart in `<bdi>`, those in a
  sentence too. Secrets are read from files and never shown.
- **Outbound** is what it was: queries for history and rules (a cloud's own
  alarms among them, with `--alarms`, each to its cloud's API host), and the
  digest and notices to Slack and a webhook, without following redirects;
  with `--changes`, the writes a person approved, to the host the rules or
  alarms were read from, and the credential chains' own endpoints (AWS STS,
  Google's token and IAM Credentials, OCI's for workload identity).
- **The pod** runs as non-root with a read-only root filesystem; the ledger's
  volume is the one place it writes.

## Keeping it fast

Measured on a laptop (`go test ./internal/ledger -run TestScale -v`, and
`./internal/serve -run TestPagesScale`), with 400 rules over twelve weeks:

| | 200,000 firings | 2,000,000 firings |
|---|---|---|
| an hourly run, recorded | 15 ms | 0.18 s |
| writing an incident | 8 ms | 52 ms |
| twelve weeks by rule | 2 ms | 2 ms |
| a week's report, without firings | 2 ms | 4.5 ms |
| judging everything again | 2.5 s | 15 s |
| the ledger on disk | 30 MB | 180 MB |

Pages, worked out afresh over 200,000 firings, take 1 to 21 ms. Both tests
run in CI with budgets, so a change that makes one slow fails.

What keeps it so, and what a new feature has to keep to:

1. **Read tallies or indexed ranges, never everything.** A page reads the
   day or week tallies (rules × weeks rows) or one index range, a page at a
   time (`Firings` takes a limit and a cursor); a report takes its whole
   days from the daily tallies. Nothing a person waits on reads the whole
   ledger: what a query needs often enough gets an index of its own (the
   alerts still on have one).
2. **Write where it changes.** A write gives stones again only over the span
   it changed (`judge(lo, hi)`), and a write from a person is bounded (an
   incident lasts 31 days at most). Anything that touches the whole ledger
   goes to the worker, a week per transaction.
3. **Ask Prometheus from one place.** Only the worker asks, one job at a
   time; a run's cost per hour is bounded by `--every`, `--settle` and the
   rules' margins; a run that doesn't finish in time is halved, not
   repeated; what is worked out once (a week's suggestions) isn't worked
   out again when only sending it failed.
4. **Cache by what changed.** Pages and answers are kept until the ledger
   (or what the worker is doing) changes, up to 32 MB and none over 2 MB,
   and carry an ETag, so a page read twice between runs is worked out once
   and sent once. Pages show times, never "4 minutes ago", so they stay the
   same between changes.
5. **One writer, many readers.** Writes take turns on one connection; reads
   use a pool of their own, in WAL mode, so a long run doesn't hold up a
   page.
6. **Bound what grows.** `--keep` drops old firings, runs, impact windows
   and versions of the rules a few thousand rows at a time and gives the
   space back; incidents written down are kept. A document is kept once,
   however many versions hold it.
7. **Hold it to a budget.** A new page goes in `TestPagesScale`, a new kind
   of write in `TestScale`.

What a rule adds to the others is worked out when read, from `catches` and the
week's noisy rules, so a change to what counts as noisy costs nothing. Noisy is
judged over the weeks shown, as on the rest of the page, and against every
team's rules even when one team's are shown: another team's rule catching an
incident as soon is as good. A rule the latest version no longer holds, or that
can't be replayed, covers nothing and isn't called spare. With `--rule`, only
how many incidents each caught can be said.

What people keep beside the facts (teams, notes on rules, quiet windows,
tasks, comments) is in [work.md](work.md).

## Not in this

Several replicas of a SQLite ledger (with Postgres,
[replicas.md](replicas.md)); several Prometheus
servers in one ledger; firings from Alertmanager's webhook; a cloud's own
alarms; editing rules. Each is a piece someone can take on. Alertmanager's webhook, beside the replay, would also show
where a replay differs from what really fired.
