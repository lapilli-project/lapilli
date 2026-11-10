# The replay beside what really fired, kept

Status: built, for v0.21.0. Under [GOVERNANCE.md](../../GOVERNANCE.md)
this is a new kind of data in the ledger, so the design came first, as a
pull request (#38); it was merged on 2026-11-10, before its week was out, by
the maintainer's decision, with no comments. The companion to [serve.md](serve.md), about
the firings a replay finds.

## Why

Every stone rests on the replay. A rule that replays differently from how
it really fired is the worst kind of wrong: its precision, its catches and
the `for` it is told to take are all of firings that didn't happen, or miss
ones that did. `lapilli verdict --check-replay` sets a replay beside the
series that say what really fired (`ALERTS`, `GRAFANA_ALERTS`), once, when
someone asks.

Nobody asks every week. A replay that was right in October goes wrong
quietly: a recording rule it reads is renamed, an exporter's labels change,
Prometheus's evaluation interval differs from the rule file's, a metric
starts arriving late. `lapilli serve` already replays every hour and keeps
what it found. It can set that beside what really fired as it goes, keep the
comparison, and say when a rule starts to differ, on the rule's page, before
anyone has judged it on firings that weren't.

## What it would answer

- For each rule, over a day, a week, twelve weeks: how many firings the
  replay and the series had, how many were alike, how many only in the
  replay, how many only in what really fired, and how far apart the alike
  ones started.
- Which rules have started to differ, and since when.
- Which alerts really fired that no rule given is named (rules loaded
  beside those lapilli reads).
- Whether the series can be checked at all: a store that keeps no `ALERTS`
  says so once, rather than every rule looking as if it never really
  fired.

It changes no stone. A rule whose replay differs is judged as before; its
page says the judgement rests on a replay that differs, and by how much.

## How it is checked

The same matching as `--check-replay` (`verdict.CheckReplay`): two firings
are alike when they are of the same alert, agree on the labels both have,
and start within two of the rule's evaluations of each other. Labels a store
adds are passed over, and so is a rule's label with a template in it.

What it reads:

- **What the replay found:** the ledger's firings of replayed rules, as the
  rule versions in effect then gave them. Nothing is replayed again for the
  check.
- **What really fired:** the series, read over the same span, `ALERTS` for
  rules Prometheus evaluates and `GRAFANA_ALERTS` for Grafana's, as
  `verdict` chooses them; `--check-series` names others, as it does there.

When it reads: after a run, over the span the runs won't replay again,
everything before the last run's end less `--settle`. It checks a span
`[a, b)` at a time, counting the firings that start in it and matching them
against those starting up to the matching window either side, read from
`a` less that window to `b` plus it. So no firing is too near an end to tell,
and each is counted once. Spans are an hour while running, and a day at a
time over a first start's backfill.

Which rules: those replayed whose series are kept somewhere. Not:

- firings copied in from elsewhere (they have no replay);
- rules read from a cloud's alarm list, CloudWatch PromQL alarms and Google
  Cloud policies, which keep no series of what fired (a ROADMAP item of its
  own);
- rules that couldn't be replayed.

## In the ledger

Schema 010, two tables and a mark.

- `checked`: a rule, a day, and counts: replayed, real, alike, and the sum
  and number of the alike ones' start differences, for a median by day
  and a mean over longer spans. A rule's day is written once its spans are
  all checked, and written again only if a run replays that day again (an
  hourly run's `--settle`), which also clears that day's row for the rule
  first.
- `check_misses`: for each rule, the last twenty firings found only one
  way: which way, when it started, and its labels. Enough to see what
  differs without keeping every firing twice.
- In `meta`: how far the check has got, and for each series whether it held
  anything over the last span checked.

Alerts that really fired with no rule given are counted by name and day in
`checked` against a rule row with no source and no version, as the ledger
already keeps a rule a replay no longer has.

`--keep` drops days before it, as it does firings.

## What it shows

- **A rule's page:** "The replay beside what really fired": over twelve
  weeks, alike as a share of both, and the counts each way; the start
  differences; the last misses, each linked to the firings page at its time.
  When the rule differs, a line above its stones says so.
- **The rules list:** a mark on rules that differ.
- **The dashboard:** the share alike over the last week, beside precision.
- **The runs page:** which series are read, how far the check has got,
  a series that held nothing, and alerts that fired with no rule given.
- **`/metrics`:** `lapilli_rule_replay_check{rule, way="alike|replayed|real"}`
  over seven days, so an alert can say a replay has started to differ.
- **MCP:** `rules` and `rule` carry it.
- **The digest:** one line, for everyone's: the share alike, and the rules
  that differ.
- **The webhook and Slack:** `replay.differs`, once when a rule starts to
  differ, with its counts.

A rule differs when, over the last seven days, it had at least five firings
either way and fewer than nine in ten of them were alike. Those numbers are
a start: what a real ledger shows should set them, and they are options
from the first.

## What it costs

One range query per series per span, of an hour while running. `ALERTS` has
a series for each alert, firing or pending, so it is a small read beside
the replay's. Over a first start's backfill it is a day at a time, beside
the replay of that day. Writing is a row per rule and day.

The check reads Prometheus with lapilli's own access, as the replay does,
and only the series named; nothing an outsider sends reaches it.

## Options

- `--check-replay`, on by default when there are rules to replay;
  `--check-replay=false` turns it off.
- `--check-series SEL`, repeatable, as for `verdict`.
- `--check-differs N/RATIO`, the threshold above, `5/0.9` by default.

A store that holds nothing in a series is checked again an hour later, then
once a day while it still holds nothing, and the runs page says so.

## Alternatives

- **Keeping what really fired as firings of their own,** with stones. The
  ledger would hold twice the firings, and judge alerts that may come from
  rules lapilli never read. The counts, a few misses and the days are what
  the question needs.
- **Checking at read time,** from Prometheus. Pages would wait on a query of
  twelve weeks of `ALERTS`, and Prometheus may not keep twelve weeks.
- **Checking only when asked,** as today. That is what this moves on from.

## Questions for review

- Is seven days and nine in ten the right first threshold? A rule that fires
  twice a week would never be said to differ; one that fires a hundred
  times would at eleven misses.
- Should a rule that differs have its stones set apart in the digest and
  the outcomes page, not only marked?
- Thanos, Mimir and the managed Prometheus services may or may not keep
  `ALERTS`. Checking is how to find out; the runs page saying so is the
  way to tell the user.

## As built

Built for v0.21.0, as above, except:

- **Days, not hours.** A day is checked once the runs won't replay it
  again, with the matching window either side; an hour at a time would
  have meant rows written and written again. A first start's backfill is
  checked over a few rounds, seven days a round.
- **Each firing counted once.** `verdict.CheckSpan` counts a firing in the
  day its start is in, and an alike pair in the day of the earlier start.
  Firings are read from six hours before a day, and each alert's are
  matched from the last start with no firing either way in the window
  before it, so an alert that fired again and again across midnight pairs
  alike from either day; `--check-replay`'s `CheckReplay` is unchanged.
- **Rules that share an alert name** (named apart in the ledger, as rules in
  two groups with one name are) are checked together under the name what
  really fired goes by, as `--check-replay` does: each of them carries the
  counts of them all, marked shared, and a sum counts them once.
- **From the first whole day** the runs that replay (backfill and hourly,
  not a digest's or a review's) began; a day they replayed only in part
  would count what really fired before them as missed.
- **A day is a day** where the clocks skip midnight too: the ledger's
  midnight is now a day's first instant, which also fixes the days and
  weeks counted there.
- **"The last week"** is the seven whole days before today on the pages, in
  `/metrics` and to agents; `replay.differs` uses the seven before the day
  just checked, and is told only for a day checked within two days, so a
  first start's backfill doesn't send weeks of old news. The digest and the
  dashboard say a week's line only once the week is checked to its end.
- **By rule name,** not by a rule's row: `checked` and `check_misses` key on
  the name the ledger judges the rule under. Alerts that really fired with
  no rule given are kept apart, in `check_elsewhere`, rather than as rules
  with no version, so they never show as rules.
- **A rule whose replay failed** in a run meeting the day isn't checked
  that day, nor are the rules of a series that held nothing; what really
  fired of them isn't taken for an alert no rule is named. Alerts `--rule`
  leaves out are passed over.
- **What the store holds is trusted.** A scraped target could expose a
  series called `ALERTS`; alerts no rule is named are kept only as names
  that show, 512 characters at most, a hundred a day.
- **Rules read from a cloud's own alarm list** are marked as such
  (`rules.Rule.Cloud`) and left out.
- **The dashboard** has a line under its heading, not a figure: the last
  whole week's share alike, and the rules that differed.
- **`replay.differs`** is told at most once a week for each rule while it
  differs, rather than once when it starts to: the ledger keeps no state of
  which rules differed, and a rule that stays wrong is worth saying again.
