# Marked windows: a sale, a launch, a campaign, shown and never excused

Status: accepted, built (schema 021, after goals' 020), but for the charts' marks of each
kind (`marks=`, below). Under [GOVERNANCE.md](../../GOVERNANCE.md)
this is a new kind of data in the ledger (the next schema at build time),
so it was designed first, as a pull request (#90); it was merged on
2026-11-16, before its week was out, by the maintainer's decision, with the
review's edits. The decisions taken are in "Open questions"; the rest stay
open until the step that needs each. What the build settled is in
"Builder's decisions", at the end. It changes no stone: a firing is
judged as it is now. The companion to [work.md](work.md) and
[serveops.md](serveops.md), whose quiet windows it sits beside, to
[noc.md](noc.md), whose deploys it generalises, and to
[signals.md](signals.md) (#88) and [goals.md](goals.md) (#91).

## Why

Some weeks aren't like the others, and a team wants to read them apart: a
big sale, a product launch, a marketing campaign, a ticket release. Traffic
is several times usual, people are on call who usually aren't, and the
alerts matter most. Afterwards the team asks what Lapilli can't answer
today: how did the alerts do during the sale, beside the weeks before? Did
anything page for nothing while it was on? Was any incident in it missed?

Lapilli has two kinds of window, and neither fits:

- **A quiet window** (`/windows`, or `windows:` in `--config`) excuses the
  firings in it that caught nothing: they are neither white nor black, and
  are in no precision ([work.md](work.md), "Quiet windows and the stones").
  That is right for maintenance, when alerts are expected and mean nothing.
  During a sale it is wrong: it hides the very firings the team wants to
  read.
- **A deploy** (`--deploys FILE`) marks the firings that came during it
  without excusing them, and every count is read with them and without
  them ([noc.md](noc.md), section 2). That is the shape a sale needs, but a
  deploy is a deploy: what it marks is counted as "during deploys"
  everywhere, it lasts a day at most, and it can't be written down ahead
  (below, "Use what exists").

## What a marked window is

A span of time, of a named **kind**, that marks the firings and incidents
that came during it and changes nothing else:

```yaml
marked:
  - kind: sale
    title: Autumn sale
    start: 2026-11-20T00:00:00+09:00
    end: 2026-11-23T00:00:00+09:00
  - kind: launch
    title: Search, new ranking
    start: 2026-11-25T10:00:00+09:00
    end: 2026-11-25T18:00:00+09:00
    labels: {service: search}
  - kind: sale
    title: Friday flash sale
    start: 2026-11-06T20:00:00+09:00
    end: 2026-11-06T22:00:00+09:00
    every: week                     # day, week, month or cron, as a quiet window repeats
    until: 2027-03-31
```

- **A kind** is a name a team chooses (`sale`, `launch`, `campaign`):
  `[a-z][a-z0-9-]`, 32 bytes at most, 16 kinds in the ledger at most.
  `deploy` is the deploys' (below) and `week` is `each: week`'s in
  [goals.md](goals.md); neither may name another kind. Everything is
  counted by kind, so the windows of one kind add up: every sale against
  the weeks around it, not one sale against the next.
- **A title**, one line, as a quiet window's.
- **When.** A start and an end, 31 days apart at most, as a quiet window;
  it may repeat as a quiet window does (every day, week, month or by a cron
  expression, until a day or for good), read by the same code
  (`internal/schedule`, `Window.Covers`), so the two can't differ. Unlike a
  deploy, it may be written down ahead, up to a year: sales are planned.
- **Labels**, optional, matched as a deploy's are (`Deploy.During`): a
  firing is marked when it agrees with the window on every label both
  have, one at least; a window that names no label marks everything. A
  launch of `{service: search}` marks search's firings and not cart's; a
  sale with no labels marks every firing while it is on.
- **A team**, optional: a team's window marks only the firings of rules the
  team owns, as a team's quiet window excuses only those
  ([serveops.md](serveops.md), "A team's quiet window").

**An occurrence is a window.** A window that repeats is, for everything
that reads windows one by one, as many windows as it has occurrences: each
occurrence has its own inside and before (below), its own line on the
page, its own result in a goal of `each: KIND` ([goals.md](goals.md)), and
its own key, the window's id and the occurrence's start
(`7@2026-11-06T11:00Z`), which notices and tasks are keyed by. An
occurrence is found from the window's schedule, never kept: the window is
one row, as a repeating quiet window is.

A firing is **marked with a kind** when it started during an occurrence of
a window of that kind and matches it, and was judged: a firing a quiet
window excused isn't marked, as it isn't marked during a deploy (`stones`,
in `internal/ledger/judge.go`). A firing can be marked with several kinds
(a launch during a sale). An incident is during a kind when it started
during an occurrence of it and agrees with it as a firing does; an impact
window is an incident here.

**Never excused.** A marked window changes no stone: not `white`, `refired`,
`excused` or `deploy`, not which incidents count, and not the record. A
team that wants a sale's firings excused writes a quiet window, which is a
decision about the verdict; marking one is not.

## Where windows come from

Four places, each owning what it brings, as the incidents file owns its
incidents and `--config` its teams:

- **A file**, `--marked FILE` (the chart's `serve.marked`), as `--deploys`
  is read: again each run, hashed, and read no further when it hashes as
  the last read did. It owns its windows: one it no longer holds is
  dropped. A file with no `marked` key is refused, and the ledger keeps
  what it had, since a job that failed leaves an empty file (as the
  incidents file is read, `incident.Load`).
- **`--config`**, under its fifth key, `marked:`, after `teams`, `windows`,
  `reports` and `grants` (`configKeys` in `internal/serve/declared.go`),
  with the fields above. Kept as the other four are
  ([serveops.md](serveops.md), "Declared configuration"): read twice
  before it is kept, declared windows read-only in the UI, given back
  without `--config`, exported in `/config.yaml`.
- **The UI and the API**, on a page of their own (below), by editors, and a
  team's editors for their team's windows, as quiet windows are made.
- **A PromQL expression**, `--mark KIND=PROMQL` (repeatable, the chart's
  `serve.mark` list), that returns something while a window is on: a
  business exporter's gauge (`shop_sale_active == 1`), a feature flag's
  metric. `KIND=` is read as [signals.md](signals.md) reads `NAME=PROMQL`.
  Read each run, as the impact signal is, over the run's period and the
  hour before. Each series is read on its own: its samples, gaps of two
  steps or fewer not splitting them (`incident.FromImpact`'s gap rule),
  make a window from the first to a step after the last, of any length.
  Windows of different series are not merged, as `FromImpact` merges
  impact windows: a mark has no labels to lose, and two series on at once
  are two windows of the kind, which mark the same firings once.
  - **Its labels are none by default**: an expression's window marks every
    firing while it is on, as a sale with no labels does, whatever labels
    its series carry. `--mark-match KIND=LABEL,...` keeps those labels of
    each series as its windows' labels, matched as above: with
    `--mark-match sale=region`, a series `{region: kr}` marks the firings
    of `region: kr` and those that agree with it.
  - **Kept** from the run's start on in place of the windows it found
    before, as impact windows are, with a key of the expression and its
    `--mark-match` (a hash of both, kept with the kind's windows): the key
    changed, the expression is read again over the last four weeks, as
    v0.49.0 reads a new impact criterion again, and the windows it found
    before are replaced.
  - An expression can't say what is ahead: a sale planned for Friday is
    written in the file, the config or the UI.

## Deploys are a kind, on the pages

`deploy` is a kind, the one `--deploys` brings. On the pages it is read as
the others are: the rules list's `during:deploy`, a rule's lines, the
chips on firings, the overview's chart marks (`marks=deploys`, as now).
What deploys have of their own stays, and is not said twice:

- their file and histories, `--deploy-before` and `--deploy-after`, the
  `deploys` table, `firings.deploy`, and the `deploy` and `deploy_white`
  tallies of schema 017;
- the summary's `during_deploys`, the report's `deploys` section and
  `lapilli_rule_firings_during_deploys`, which dashboards read and the
  summary's `schema: 1` promises. The summary's `marked` and the
  `kind`-labelled metrics leave deploys out, so no number is given twice
  under two names.

They aren't moved into the kinds' tables in this design. Deploys are many
and short (50,000 at most, read from histories every run), and their
storage was reviewed as it is; the kinds' tables would hold the same
numbers under another name. A later change can move them when nothing
reads the 017 columns but through the kinds.

## How they are counted

Every count the ledger keeps can be read inside a kind and outside it.

**Kept, not worked out when read**, as a deploy's mark is (a builder's
decision in [noc.md](noc.md)):

- `firings.marked`: the kinds a firing is marked with, one bit each (a
  kind's id in `marked_kinds`, 1 to 15; `deploy` is `firings.deploy`).
- `marked_days` and `marked_weeks`: by day (week) and rule and kind, the
  firings marked with the kind, those that caught something, and those that
  re-fired. Counted in the same `tally` as `days` and `weeks`, in one
  statement joining the kinds (`firings.marked & (1 << kind) != 0`, which
  SQLite and Postgres both read). If [signals.md](signals.md) (#88) is
  built, and `days` and `weeks` gain `signals` in their key, `marked_days`
  and `marked_weeks` take the same key, so precision inside a kind is of
  the firings judged.

**Outside a kind** is the same count less the kind's: a week's firings less
those marked `sale`, its caught less the `sale` caught. Nothing is kept
for it.

**Judged as everywhere.** Precision inside or outside is of the firings
judged: a week with no record has its firings counted inside and outside,
and judged in neither ([serve.md](serve.md), "No record").

**An occurrence beside the weeks before.** What a team reads after a sale:

- *inside*: the firings marked with its kind on the days it covers, from
  `marked_days` (two occurrences of one kind on one day are counted
  together, and the page says so);
- *before*: the four whole weeks before its first week, outside its kind,
  from `weeks` less `marked_weeks`;
- each with firings, caught, precision, re-fired, noisy rules, and the
  incidents and impact windows caught, caught only by noisy rules, or
  missed, with their median lead.

## Where it shows

- **A page of its own**, `/marked`, "Marked windows", under Monitoring
  beside Incidents: what was happening, read with the incidents, not a
  change to the verdict, as Quiet windows (under Improve) are. Its
  occurrences by kind, the latest first, each with inside and before; a
  form to keep a window; a window's page, `/marked/{id}`, with its
  occurrences, their firings (a link to Alert history for each span and
  kind) and incidents. Declared ones carry the `declared` badge.
- **The API**, as quiet windows': `GET POST /api/v1/marked`, `PUT DELETE
  /api/v1/marked/{id}`, beside `/api/v1/windows`.
- **The overview**: a tile for the latest occurrence that is on or ended in
  the twelve weeks shown, its kind and title, precision inside beside
  before, and incidents missed in it. The charts mark the weeks with an
  occurrence of each kind (`marks=`, which takes `deploys` and `windows`
  now, takes each kind's name too).
- **The rules list**: `during:sale` in the search box, a facet, counts each
  rule's firings inside the kind over the weeks picked, and `outside:sale`
  the rest: a rule's precision during sales, the rules noisiest during
  them. `during:deploy` is the deploys' column as a filter.
- **A rule's page**: in its numbers, a line for each kind it fired in over
  the weeks shown, inside beside outside.
- **Firings and incidents**: a chip with each kind, on Alert history, a
  rule's latest firings, the Incidents page and an incident's page.
- **The digest and reports**: a section, `marked`: each occurrence on in the
  span, with inside and before, and the span's counts inside each kind
  beside outside. In the digest when an occurrence was on in its week.
- **`/api/v1/summary`**: a `marked` field in each row (below).
- **`/api/v1/report`** (the report for a period): each rule's `marked`, by
  kind, beside its `during_deploys`, and each firing's `marked`.
- **`/metrics`**: `lapilli_rule_firings_marked` and
  `lapilli_rule_caught_marked`, with a rule's labels as the rule metrics
  have them and `kind`, over the last seven days, a series only for a rule
  and kind with firings; `lapilli_marked_on` (`kind`), 1 while an
  occurrence of the kind is on, for a dashboard to shade by; and
  `lapilli_serve_marked_read_timestamp_seconds`, when the file was last
  read. None has a series of `kind="deploy"`.
- **MCP**: `firings` gives each firing's kinds, `summary` the `marked` field,
  `overview` the tile.
- **Notices**: `marked.kept` when a window is kept in the UI, as
  `window.kept` is.
- **i18n**: every new text in the four languages, as every page's.

In the summary, a row's `marked` holds each kind with an occurrence in the
week, deploys left out (they are `during_deploys`):

```json
"marked": {
  "sale": {"windows": 1, "firings": 31, "caught": 6, "precision": 0.19, "precision_outside": 0.42,
           "refired": 4,
           "incidents": {"total": 2, "caught": 1, "noisy_only": 0, "missed": 1, "median_lead_seconds": 180},
           "impacts":   {"total": 1, "caught": 1, "noisy_only": 0, "missed": 0, "median_lead_seconds": -60}}
}
```

`windows` counts occurrences. It is `{}` in a week with none. Shares are
null as the row's are: with nothing to divide by, or with no record; a
median null with nothing caught. `schema` stays 1: a field added.

## What stays the same, and what is added

[numbers.md](../numbers.md) lists what keeps its name. The pull request
that builds a step adds, in the same pull request, to its tables and to
`TestReportFieldsStable` and `TestMetricsStable`: the report's
`rules[].marked`† and `firings[].marked`†; the CSV's `marked` column, at
the end (`TestCSVColumns`); and the metrics `lapilli_rule_firings_marked`
and `lapilli_rule_caught_marked` (`rule`, [`severity`], [`team`], `kind`),
`lapilli_marked_on` (`kind`) and
`lapilli_serve_marked_read_timestamp_seconds`. Nothing listed there
changes.

## In the ledger

The next schema at build time adds:

- `marked_kinds`: `id` (1 to 15), `name`, unique. A kind is kept while a
  window or an expression names it; one no longer named is let go once its
  windows' spans are judged again, and its id can be given to another.
- `marked`: a window: `kind_id`, `origin` (`file`, `config`, `ui` or
  `expr`), `title`, `start_at`, `end_at`, `every`, `repeat_on`, `until`,
  `team_id`, `labels`, `declared` and `given_back` (as `windows` has
  them), who changed it and when. Indexed by start. An expression's key is
  kept in `meta`, by kind.
- `firings.marked`, `marked_days`, `marked_weeks` (above).

Keeping, changing or deleting a window notes its span, before and after,
to be judged again, as a quiet window's is, a week at a time
([work.md](work.md)). Judging gives every stone again over the span, as
now, and only the marks can change.

Upgrading adds the tables and a column whose default marks nothing; there
is nothing to backfill, since no window is kept. Going back takes the
ledger as it was, so a deployment keeps a copy of it (a snapshot of its
disk, or of its database) before it upgrades.

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

- **Occurrences meeting a judged week**: 64 of a kind and 256 in all at
  most. A window that would make more is refused where it is kept (the
  form, the API, the file or `--config`, whole for the file as the
  deploys file is), and an expression's windows past them are dropped for
  that run and said. Repeats are worked out once for each span judged, as
  `Excusing` works out a quiet window's (`internal/ledger/work.go`): the
  times a schedule falls due in the span, then a search by each firing's
  start.
- **Windows looked up per firing**: those whose occurrences started within
  the longest occurrence before the firing's start, found by a search on
  starts, as deploys are (`during`), so at most the 256 above and, for
  windows of hours, a few. `TestScale` holds it by a counter of windows
  looked up per firing.
- **Kinds per firing**: 16 bits at most on each firing.
- **Windows kept**: 2,000 from the file, 500 declared, 500 made in the UI
  (20 of a team's for its editors, as quiet windows), and 1,000 a run from
  each expression, eight expressions at most. A window lasts 31 days at
  most and starts no later than a year after it is kept. The file is 8 MB
  at most, as the deploys file; a window 16 labels and 4 KB of them, as a
  deploy.
- **Tallies**: `marked_days` has a row for each day, rule and kind with a
  marked firing, so it is never larger than `days` times the kinds marked
  that day. Pages read it as they read `days`: by index, never the
  firings.
- **A page** reads the occurrences of the weeks it shows and their kinds'
  tallies; "before" is four weeks of `weeks` and `marked_weeks`.
  `TestPagesScale` holds the rows each page reads and its heap peak, the
  least of three runs (#99).
- **An expression** is one range query a run, read before the rules within
  the time the impact signal has, as [signals.md](signals.md) reads
  several. One that fails is missed, its windows kept as they were, and
  tried again by the next run; it holds up nothing else.

## Security

- **Who writes.** The file and the expressions are whoever runs `serve`'s;
  `--config` is whoever changes it, through its review. In the UI and the
  API, editors and admins, and a team's editors their team's windows,
  checked in the write as a quiet window is; `--read-only` refuses them all.
  Marking changes no stone, so it decides nothing about anyone's alerts,
  but it does decide what a team's numbers are set beside, so it isn't a
  viewer's.
- **What comes in.** Titles and kinds are text from people and files, one
  line, shown escaped and set apart in `<bdi>` where they are shown, as
  rule names are (a quiet window's title isn't yet, in
  `templates/work.html`). An expression's windows take their labels, when
  `--mark-match` names some, from series, and so from whatever can write
  metrics to that Prometheus: bounded as above, kept as an alert's labels
  are (`verdict.KeptLabels`), and shown escaped. Kinds are from a small
  alphabet, so safe in a URL, a facet and a label value, and escaped in
  each all the same.
- **Where names end up.** Pages, the summary, the digest and reports (as
  text a receiver escapes, as `digest.ready`'s), and `/metrics` labels,
  escaped, 15 kinds at most.

## Not in this design

- **Excusing.** A marked window never does; a quiet window does.
- **A reach before and after**, as deploys have (decided: write a longer
  window).
- **Off hours and asleep inside a kind.** They are counted as pages are
  read, from when each firing started ([serve.md](serve.md), "Web UI, API,
  metrics"), not kept; inside a kind is a later step.
- **`lapilli verdict`**, which takes no deploys either, takes no marked
  windows.

## Alternatives

- **Use what exists: a deploy with no labels.** A deploy that names no
  label is everything's (`deployOf`), so `--deploys` can mark every firing
  during a sale today, and every count is read with and without it. That
  is as far as it goes: a deploy lasts a day at most (`MaxDeployLen`), so a
  three-day sale is three entries; it starts no later than an hour after
  the file is read (`DeployAhead`), so a sale can't be written down ahead;
  it has no name, so every sale and every real deploy are counted together
  as "during deploys", and precision without deploys leaves out both; it
  comes from a file alone, with no UI and no repeat.
- **Use what exists: the report for the sale's span.** `/api/v1/report
  ?from=&to=` takes any RFC 3339 times, so the verdict over exactly the
  sale is a request away, and another for the same span a week before. By
  hand, with no labels, nothing outside, and not on the pages, the summary
  or `/metrics`.
- **A quiet window with a flag "don't excuse".** One table for two things
  that do the opposite to the verdict, and every check of a quiet window
  would have to ask which it is.
- **A label on the alerts**, `sale="on"`, joined into each rule's
  expression from a recording rule. It changes every rule, and the history
  before the label was added has none.
- **Grafana's annotations.** Drawn on a dashboard, counted nowhere.
- **An expression's windows labelled by its series by default**, as impact
  windows are. A business gauge's labels (`instance`, `job`) say where it
  was scraped, not what the sale is of; a sale marks everything unless
  told otherwise.

## Open questions

Decided on 2026-11-16, by the maintainer:

- **Where the page goes**: under Monitoring, beside Incidents.
- **A kind's reach** before and after, as deploys have: none; a window is
  written longer.

Open:

1. **Expressions in the UI**, by an admin, as v0.49.0 puts the impact
   signal on the Settings page, with a try.
2. **Moving deploys** into the kinds' tables, and when.
3. **Budgets inside a kind.** [expectations.md](expectations.md) holds a
   rule to how often it fires in a week; a sale week may break every
   budget it should. Whether a budget may say how often during a kind, or
   leave a kind's firings out.
4. **A kind's own colour** on the charts. Colours carry status
   ([ui.md](ui.md)); a kind is a name, and is shown as one.

## Steps

1. The ledger and the file: the next schema, `--marked FILE`, the marks on
   firings, the tallies, the summary's `marked`, `/metrics`, numbers.md's
   tables and tests.
2. The UI and `--config`: the page and its forms, the API, roles, `marked:`
   in `--config`, the rules list's facet, a rule's lines, the overview's
   tile and chart marks, the `marked` section of reports and the digest.
3. Expressions: `--mark KIND=PROMQL` and `--mark-match`.

## Builder's decisions

Taken while building, within what is above:

- **Fifteen kinds, and deploy.** "16 kinds in the ledger at most" counts
  `deploy`: a team names 15 (ids 1 to 15, bits 1 to 15 of
  `firings.marked`), and `deploy` is `firings.deploy`.
- **A window's labels have one value each**, as a deploy's: the forms and
  `--config` refuse two values of one label.
- **The bounds, counted.** A window that doesn't repeat counts once in
  each week it meets; one that repeats counts the most of its
  occurrences any one week meets (over the weeks its schedule takes to
  come round, a year's for a month or a cron expression), in every week
  from its first to its last. That can refuse a window a week would in
  fact take; it never takes one a week can't.
- **Each firing is set beside its own week's occurrences**, those that
  started within the longest of them before it, by a search on starts:
  so 256 at most however long the longest occurrence is (a window of 31
  days beside 255 of an hour), and, for windows of hours, a few.
  `TestScale` holds the few, `TestMarkedScale` the 256.
- **An expression's windows across reads.** A window found before that is
  on at a read's start goes on in one found from the start (within two
  steps), each found window going on from one at most, so two series on
  at once keep two windows; up to 31 days, past which a new window
  starts; one that ended before the start is let be. One going on is
  judged again from where it ended to where it ends now, not over all it
  covers. Its windows are titled by their kind. An expression no longer
  given (every one, with no `--mark`) has its key forgotten, and its
  windows are kept as they were found.
- **A changed expression waits for its read again.** A run's read with
  another key than its windows were kept with is skipped, its windows and
  key kept, until the read of the last four weeks succeeds and keeps the
  new key; a failed read again is tried again each round. A kind's first
  read, with none found before, starts it.
- **Pruned once over.** Those made in the UI and those found by an
  expression are pruned with `--keep`, a repeating one once its last
  occurrence is over (33 days past its until), so the firings it marked
  are pruned first and its kind isn't let go while they hold its bit; the
  file's and `--config`'s are theirs.
- **Shares of a week, and of the kinds.** Of the 256 occurrences a week
  meets, those made in the UI take 128 at most, and a team's windows 32;
  of the 64 of a kind, those made in the UI take 32; of the fifteen
  kinds, those made in the UI take seven, counting a kind no window names
  any more until it is let go, so that making and deleting windows faster
  than their kinds are let go takes none of the rest. So what a team's
  editors, or every editor, keep never refuses the marked file,
  `--config` or an expression below the rest. A write in the UI that would
  take more kinds than the UI's share is refused, unless it takes no more
  than the UI's already take (a new title, a window given back); one past
  a share of a week is refused only in a week it makes busier, so a new
  title or a shorter window is let be, past a share kept from before. A team's
  editors use the kinds there are: the fifteen are taken by an editor or
  an admin, a file, `--config` or an expression.
- **The kinds counted from the ledger's**, those of expressions and those
  not let go yet among them: a `--config` window of a sixteenth kind is
  skipped and said, the rest of the file kept; an expression's read of
  one is skipped, the others kept.
- **A kind is let go** once nothing is left to judge again
  (`JudgePending` finds nothing pending) and no window or expression
  names it: the firings it marked are marked no more by then.
- **Whose a window is.** The file's and an expression's are refused in
  the UI and the API (409), as `--config`'s are. A window `--config` no
  longer declares, given back, is the UI's (origin `ui`). A declared one
  is matched by its kind, title and team.
- **The page** lists the occurrences of the last twelve weeks and the
  next four, the latest 100, each kind its own table; "inside" reads the
  kind's days the occurrence covers, "before" the four whole weeks before
  its first week, outside the kind, both judged by day (by week with `--record-by-week`) as everything is.
  The overview's tile is a card: the latest occurrence on or ended in the
  weeks shown. A rule's lines are on its firings tab, beside its deploys.
- **The rules list's facet** is a row of links, each kind during and
  outside, as the counts change with it; one kind at a time.
- **The API** gives a window's occurrences in the period asked with each
  window (`GET /api/v1/marked?from=&to=`, 400 days at most), and one
  window at `GET /api/v1/marked/{id}`.
- **The CSV's `marked`** is a JSON array of kinds, `[]` for none; the
  report's `rules[].marked` is by kind: `firings`, `caught`, `refired`,
  `unjudged`†.
- **Not built yet: the charts' marks of each kind** (`marks=sale`). The
  overview's charts mark deploys and quiet windows as before.
