# Marked windows: a sale, a launch, a campaign, shown and never excused

Status: proposed. Under [GOVERNANCE.md](../../GOVERNANCE.md) this is a new
kind of data in the ledger (schema 022), so it is designed first, as its
own pull request. It changes no stone: a firing is judged as it is now.
The companion to [work.md](work.md) and [serveops.md](serveops.md), whose
quiet windows it sits beside, and to [noc.md](noc.md), whose deploys it
generalises.

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
  Everything is counted by kind, so the windows of one kind add up: every
  sale against the weeks around it, not one sale against the next.
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

A firing is **marked with a kind** when it started during a window of that
kind and matches it, and was judged: a firing a quiet window excused isn't
marked, as it isn't marked during a deploy (`stones`, in
`internal/ledger/judge.go`). A firing can be marked with several kinds (a
launch during a sale). An incident is during a kind when it started during
a window of it and agrees with it as a firing does; an impact window is an
incident here.

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
- **`--config`**, under a fifth key, `marked:`, with the fields above. Kept
  as the other four are ([serveops.md](serveops.md), "Declared
  configuration"): read twice before it is kept, declared windows
  read-only in the UI, given back without `--config`, exported in
  `/config.yaml`.
- **The UI and the API**, on a page of their own (below), by editors, and a
  team's editors for their team's windows, as quiet windows are made.
- **A PromQL expression**, `--mark KIND=PROMQL` (repeatable, the chart's
  `serve.mark` list), that returns something while a window is on: a
  business exporter's gauge (`shop_sale_active == 1`), a feature flag's
  metric. Read each run, as the impact signal is, over the run's period and
  the hour before; each series that returns something is a window of that
  kind, with the series' labels (less `__name__`), from its first sample to
  its last and a step, gaps of less than two steps not splitting it
  (`incident.FromImpact`'s rule, with no minimum length). Kept from the
  run's start on in place of the windows it found before, as impact windows
  are. Added, or its expression changed, it is read again over the last
  four weeks, as bundle 31 (branch `b31-incident-criteria`, not merged)
  reads a new impact criterion again. `KIND=` is read as
  [signals.md](signals.md) (#88, proposed) reads `NAME=PROMQL`. An
  expression can't say what is ahead: a sale planned for Friday is written
  in the file, the config or the UI.

## Deploys are a kind

`deploy` is a kind, the one `--deploys` brings, and a name no other source
may use. Everything below that reads kinds reads deploys as one of them:
the rules list's filter, the summary's `marked`, the overview's chart
marks, a report's `marked` section. What deploys have of their own stays:
their file and histories, `--deploy-before` and `--deploy-after`, the
`deploys` table, `firings.deploy` and the `deploy` and `deploy_white`
tallies of schema 017, and the summary's `during_deploys`, the report's
`deploys` section and `lapilli_rule_firings_during_deploys`, which
dashboards read and the summary's `schema: 1` promises.

They aren't moved into the kinds' tables in this design. Deploys are many
and short (50,000 at most, read from histories every run), and their
storage was reviewed as it is; the kinds' tables would hold the same
numbers under another name, and every reader already sees them as a kind.
A later change can move them when nothing reads the 017 columns but
through the kinds.

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
  SQLite and Postgres both read).

**Outside a kind** is the same count less the kind's: a week's firings less
those marked `sale`, its caught less the `sale` caught. Nothing is kept
for it.

**Judged as everywhere.** Precision inside or outside is of the firings
judged: a week with no record has its firings counted inside and outside,
and judged in neither ([serve.md](serve.md), "No record").

**A window beside the weeks before.** What a team reads after a sale:

- *inside*: the firings marked with its kind on the days it covers, from
  `marked_days` (two windows of one kind on one day are counted together,
  and the page says so);
- *before*: the four whole weeks before its first week, outside its kind,
  from `weeks` less `marked_weeks`;
- each with firings, caught, precision, re-fired, noisy rules, and the
  incidents and impact windows caught, caught only by noisy rules, or
  missed.

## Where it shows

- **A page of its own**, `/marked`, "Marked windows", under Monitoring
  beside Incidents: what was happening, read with the incidents, not a
  change to the verdict, as Quiet windows (under Improve) are. Its windows
  by kind, the latest first, each with inside and before; a form to keep
  one; a window's page with its firings (a link to Alert history for its
  span and kind) and incidents. Declared ones carry the `declared` badge.
- **The overview**: a tile for the latest window that is on or ended in the
  twelve weeks shown, its kind and title, precision inside beside before,
  and incidents missed in it. The charts mark the weeks with a window of
  each kind (`marks=`, which takes `deploys` and `windows` now, takes each
  kind's name too).
- **The rules list**: `during:sale` in the search box, a facet, counts each
  rule's firings inside the kind over the weeks picked, and `outside:sale`
  the rest: a rule's precision during sales, the rules noisiest during
  them. `during:deploy` is the deploys' column as a filter.
- **A rule's page**: in its numbers, a line for each kind it fired in over
  the weeks shown, inside beside outside.
- **Firings and incidents**: a chip with each kind, on Alert history, a
  rule's latest firings, the Incidents page and an incident's page.
- **The digest and reports**: a section, `marked`: each window on in the
  span, with inside and before, and the span's counts inside each kind
  beside outside. In the digest when a window was on in its week.
- **`/api/v1/summary`**: a `marked` field in each row (below).
- **`/api/v1/report`** (the report for a period): each rule's `marked`, by
  kind, beside its `during_deploys`.
- **`/metrics`**: `lapilli_rule_firings_marked{kind}` and
  `lapilli_rule_caught_marked{kind}` over the last seven days, with a rule's
  labels as the rule metrics have them, a series only for a rule and kind
  with firings; `lapilli_marked_on{kind}`, 1 while a window of the kind is
  on, for a dashboard to shade by; and
  `lapilli_serve_marked_read_timestamp_seconds`, when the file was last
  read.
- **MCP**: `firings` gives each firing's kinds, `summary` the `marked` field,
  `overview` the tile.
- **Notices**: `marked.kept` when a window is kept in the UI, as
  `window.kept` is.

In the summary, a row's `marked` holds each kind with a window in the week,
`deploy` among them when the ledger keeps deploys:

```json
"marked": {
  "sale": {"windows": 1, "firings": 31, "caught": 6, "precision": 0.19, "precision_outside": 0.42,
           "refired": 4,
           "incidents": {"total": 2, "caught": 1, "noisy_only": 0, "missed": 1},
           "impacts":   {"total": 1, "caught": 1, "noisy_only": 0, "missed": 0}},
  "deploy": {"windows": 14, "firings": 3, "caught": 1, "precision": 0.33, "precision_outside": 0.36,
             "refired": 0,
             "incidents": {"total": 0, "caught": 0, "noisy_only": 0, "missed": 0},
             "impacts":   {"total": 0, "caught": 0, "noisy_only": 0, "missed": 0}}
}
```

It is `{}` in a week with none. Shares are null as the row's are: with
nothing to divide by, or with no record. `schema` stays 1: a field added.

## In the ledger

Schema 022:

- `marked_kinds`: `id` (1 to 15), `name`, unique. A kind is kept while a
  window or an expression names it; one no longer named is let go once its
  windows' spans are judged again, and its id can be given to another.
- `marked`: a window: `kind_id`, `origin` (`file`, `config`, `ui` or
  `expr`), `title`, `start_at`, `end_at`, `every`, `repeat_on`, `until`,
  `team_id`, `labels`, `declared` and `given_back` (as `windows` has
  them), who changed it and when. Indexed by start.
- `firings.marked`, `marked_days`, `marked_weeks` (above).

Keeping, changing or deleting a window notes its span, before and after,
to be judged again, as a quiet window's is: a week at a time, the first two
seconds' worth before the write answers ([work.md](work.md)). Judging
gives every stone again over the span, as now, and only the marks can
change.

Upgrading adds the tables and a column whose default marks nothing; there
is nothing to backfill, since no window is kept. Going back takes the
ledger as it was, so a deployment keeps a copy of it (a snapshot of its
disk, or of its database) before it upgrades.

## Cost and bounds

- **Each firing judged** is set beside the windows that may mark it once
  per span, as deploys are (sorted by start, a search for those that
  started before it), and a repeating window as a quiet window is (one
  schedule lookup). The kinds are 16 bits at most on each firing.
- **Windows**: 2,000 from the file, 500 declared, 500 made in the UI (20 of
  a team's for its editors, as quiet windows), and 1,000 a run from each
  expression, eight expressions at most. A window lasts 31 days at most
  and starts no later than a year after it is kept. The file is 8 MB at
  most, as the deploys file; a window 16 labels and 4 KB of them, as a
  deploy. What breaks a bound refuses the whole file, which keeps what it
  had, as the deploys file does.
- **Tallies**: `marked_days` has a row for each day, rule and kind with a
  marked firing, so it is smaller than `days` unless every firing is
  marked. Pages read it as they read `days`: by index, never the firings.
- **A page** reads the windows of the weeks it shows and their kinds'
  tallies; "before" is four weeks of `weeks` and `marked_weeks`. It goes in
  `TestPagesScale`, and keeping a window in `TestScale`.
- **An expression** is a range query a run, read before the rules within
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
  line, shown escaped and set apart in `<bdi>`, as a quiet window's title.
  An expression's windows take their labels from series, and so from
  whatever can write metrics to that Prometheus: bounded as above, kept as
  an alert's labels are (`verdict.KeptLabels`), and shown escaped. Kinds
  are from a small alphabet, so safe in a URL, a facet and a label value,
  and escaped in each all the same.
- **Where names end up.** Pages, the summary, the digest and reports (as
  text a receiver escapes, as `digest.ready`'s), and `/metrics` labels,
  escaped, 16 kinds at most.

## Not in this design

- **Excusing.** A marked window never does; a quiet window does.
- **A reach before and after**, as deploys have (`--deploy-before`,
  `--deploy-after`). A sale's traffic rises before it opens; a window can be
  written to start earlier.
- **Off hours and asleep inside a kind.** They are counted as pages are
  read, from when each firing started ([serve.md](serve.md), "Web UI, API,
  metrics"), not kept; inside a kind is a later step.
- **`lapilli verdict`**, which takes no deploys either, takes no marked
  windows.
- **Goals over a kind**, such as "during a sale, no worse than the weeks
  before": [goals.md](goals.md) (#91, proposed).

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

## Open questions

1. **A kind's reach**, before and after, as deploys have; or a window simply
   written longer.
2. **Expressions in the UI**, by an admin, as bundle 31 puts the impact
   signal on the Settings page, with a try.
3. **Moving deploys** into the kinds' tables, and when.
4. **Budgets inside a kind.** [expectations.md](expectations.md) holds a
   rule to how often it fires in a week; a sale week may break every
   budget it should. Whether a budget may say how often during a kind, or
   leave a kind's firings out.
5. **Where the page goes.** Monitoring, as proposed, or beside Quiet windows
   under Improve, one page with two tabs.
6. **A kind's own colour** on the charts. Colours carry status
   ([ui.md](ui.md)); a kind is a name, and is shown as one.

## Steps

1. The ledger and the file: schema 022, `--marked FILE`, the marks on
   firings, the tallies, the summary's `marked`, `/metrics`.
2. The UI and `--config`: the page and its forms, roles, `marked:` in
   `--config`, the rules list's facet, a rule's lines, the overview's tile
   and chart marks, the `marked` section of reports and the digest.
3. Expressions: `--mark KIND=PROMQL`.
