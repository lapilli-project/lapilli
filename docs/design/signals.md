# Impact signals: several, each named, with its own `for`, `match` and start

Status: proposed. Under [GOVERNANCE.md](../../GOVERNANCE.md) this changes
how a firing is judged (an incident, and a rule beside a signal, get labels
of their own to be matched on, and a rule can be judged against some
signals only) and adds a new kind of data to the ledger (schema 025), so it
is designed first, as its own pull request. It builds on #98 (bundle 31,
branch `b31-incident-criteria`), not merged as this is written, which puts
the impact signal on the Settings page: the "Incident criteria" section,
its try on the last four weeks, the one rule the record by the signal is
kept by (read, with data, by the criterion in effect), where the data of
each selector it reads begins, and the "Incident sources" panel (its
changes to [serve.md](serve.md) and [ui.md](ui.md),
`internal/ledger/impact.go`, `internal/serve/criteria.go` and
`rules.Selectors`). Where this note says "the criterion", "read again",
"the try" or "where the data begins", it means that work's.

## Why

A team with no incident log, or a thin one, gives Lapilli the impact
signal: `lapilli serve --impact PROMQL`, an expression that returns
something while users are hurt. Each of its series that returns something
for at least `--impact-for` (5m by default) is a window, kept as an
incident nobody had to write down (`incident.FromImpact`). Windows are
judged against as written incidents are, confirmed or dismissed on the
Incidents page, and, while the runs read the signal all of a week, keep the
record of that week, so its firings are judged ([serve.md](serve.md), "No
record").

A team rarely has one signal of user harm. It has several, of different
shapes:

- the share of a service's requests that failed, by service, with a floor
  of traffic so that a quiet service's one error isn't an outage: worth a
  window after five minutes;
- a synthetic probe failing (`probe_success == 0`), by target: worth a
  window after two, as a probe runs every 15 or 30 seconds and a failure
  that lasts two minutes is no blip;
- a service's slowest requests getting slower (a 99th percentile over a
  bound), by service: worth a window after ten;
- payments succeeding at none while some are tried, with no labels: worth a
  window after one.

Today they go into one expression, joined with `or`, each tagged with
`label_replace(..., "signal", "probe", "", "")` so that its windows say
which it was. That finds windows of all of them, and stops there:

- **One `for`.** `--impact-for` is the same for every series.
- **Labels lost where windows meet.** `incident.FromImpact` makes windows
  that overlap in time one incident, keeping only the labels all of them
  have (`delete(in.Labels, k) // not all have it: no constraint`). A probe
  window (`instance`, `job`) and a 5xx window (`service`) on at once become
  one incident labelled `signal: [errors, probe]` and nothing else: with
  `--match service`, a firing of any service catches it.
- **One start.** #98 keeps the record by the signal from the first whole
  week after the latest first sample of any selector the expression reads.
  Joined, the youngest signal decides for all: a probe whose data begins
  on a Thursday takes away the record of every week before, though the
  5xx share beside it has a month of data and said, all that month, that
  nothing hurt the services it measures.
- **One `--match` for every incident.** `verdict.Catches` holds every
  incident to the same `--match` (`in.Labels.Agree(f.Labels, o.Match)`). A
  probe's `job` and `instance` name the prober and the URL it fetched,
  which no alert carries: with `job` in `--match`, nothing catches a probe
  window, since a label both have must agree.
- **One `--match` for every rule.** A label both sides have must agree, and
  for most rules that is right: a service's 5xx rate, its restarts, its
  slow requests and a load balancer's 5xx for it are the service team's
  rules, and their alerts carry the same `team` as the service's windows.
  Not so a load balancer's "no backend" rule, which the platform team owns:
  its alerts carry `team: platform`, every window it should catch carries a
  service team's `team`, and with `team` in `--match` it catches none of
  them. What ties them is the backend's name, which both carry.
- **Every rule against every signal.** A latency rule is judged against the
  5xx windows as every rule is, and a latency regression isn't a 5xx
  window: its firings catch nothing, and read as black, nothing wrong,
  week after week. With no signal of latency at all, they should read as
  no record, not as noise.
- **Nothing told apart.** The Incidents page, the summary's `impacts` and
  `/metrics` count impact windows all together.

## What a signal is

A named expression, read as the impact signal is now, with four things of
its own:

| | What it is | When not given |
|---|---|---|
| `expr` | PromQL that returns something while users are hurt | (required) |
| `for` | how long a series returns something before it is a window | `--impact-for` |
| `match` | the labels a firing must agree with its windows on | `--match` |
| `record` | whether reading it all of a week keeps that week's record | yes |

Given where `serve` starts:

```
lapilli serve \
  --impact 'errors=sum by (service, team) (rate(http_requests_total{code=~"5.."}[5m]))
      / sum by (service, team) (rate(http_requests_total[5m])) > 0.05
      and sum by (service, team) (rate(http_requests_total[5m])) > 1' \
  --impact 'probe=probe_success == 0' --impact-for probe=2m --impact-match probe=service \
  --impact 'payments=sum(rate(payments_succeeded_total[5m])) == 0
      and sum(rate(payments_tried_total[5m])) > 0' \
  --impact-for payments=1m --impact-record payments=no
```

(The probe's targets here are relabelled with the `service` they probe;
without that, `--impact-match probe=` matches on nothing, and any firing
during a probe window catches it.)

**Reading `NAME=PROMQL`.** A signal is named when `--impact`'s value starts
with a name (`[a-z][a-z0-9_-]`, 32 bytes at most) and a `=` that isn't
followed by `=` or `~`. No PromQL expression starts so: a lone `=` is
PromQL only inside a selector's braces, after a label's name, never after
a metric's; `up==0` and `up>=1` read as the expressions they are, and
`errors=rate(x[5m]) > 1` as the signal `errors`. `--impact PROMQL` with no
name stays what it is today: one signal, its windows keyed, titled and
keeping the record as now. Named and unnamed aren't mixed; serve refuses
them together and says to name each. `--impact-for` takes `DUR` (every
signal's default, as now) or `NAME=DUR`; `--impact-match` and
`--impact-record` take `NAME=...` and are refused for a name no `--impact`
gives. `lapilli verdict` takes the same flags.

**A file**, `--impact-file FILE`, says the same as the flags, and what they
can't: which rules are judged against which signals (below). The chart
renders it from `impact` when `impact` is a mapping, and passes `--impact`
when it is a string, as now:

```yaml
impact:
  signals:
    - {name: errors, expr: "...", for: 5m, match: [service, team]}
    - {name: probe, expr: "probe_success == 0", for: 2m, match: [service]}
    - {name: latency, expr: "...", for: 10m, match: [service, team]}
    - {name: payments, expr: "...", for: 1m, record: false}
  rules:                            # which signals a rule is judged against
    - rules: Slow$|Latency          # rules whose names match, or
      labels: {kind: latency}       # whose alerts carry these values
      signals: [latency]
    - rules: ^BackendMissing$       # a load balancer's "no backend" rule
      signals: [errors, probe]
      match: [backend]              # matched on these labels alone
```

Read strictly, as `--config` is: an unknown key, a signal named twice, a
rule entry naming a signal the file doesn't declare, or a file over 1 MiB
refuses the whole file, and the ledger keeps what the last good one said.

**In the UI**, #98's Incident criteria section lists the signals, a row
each, with what serve was started with, who changed it and when, and
"back to the start", as the section does for its one expression; and the
rule entries below them, as one list. An admin adds a signal, changes one
or removes one; the UI's is kept over the command line's of the same name,
as the judging settings are. The rule entries, saved in the UI, take the
place of the file's as a whole, and "back to the start" gives the file's
back. `/settings.yaml` gives both as the chart's `impact`. A signal's name
can be changed in place, which renames its windows with it (below).

**Bounds.** Eight signals at most. The expression as #98 checks it: 4 KB,
read as Lapilli reads PromQL (`rules.Scope`), then once by Prometheus,
whose parser has the last word; `for` from the step to a day; `match` 16
label names at most, each a Prometheus label name. Rule entries: 64 at
most, each a pattern as a team's or a quiet window's is (a Go regular
expression of 200 bytes, refused when it compiles to a long program), 16
labels, 8 signals.

## Which rules a signal judges

By default, every rule is judged against every signal, with each signal's
`match`: what one expression joined with `or` does now, without its lost
labels. Most rules need nothing more. A rule entry is for the rules that
do, of two kinds the default gets wrong:

- **A rule whose owner isn't the incident's.** The "no backend" rule above
  is the platform team's; the windows it should catch are the service
  teams'. Its entry says `match: [backend]`: against `errors` and `probe`
  windows, its firings are matched on `backend` alone, and `team` is left
  out. Every other rule, whose `team` is the service's, still matches on
  `team` and `service`, as the signals say.
- **A rule of another kind than a signal.** The latency rules' entry says
  `signals: [latency]`: they are judged against the `latency` windows and
  no others. A slow-request rule firing while the 5xx share is up neither
  catches the 5xx window nor is black for missing it.

What an entry changes, for the rules it names (the first entry, in order,
whose pattern matches the rule's name or whose labels its alerts carry
with one of their values, as a team owns a rule by its pattern or labels;
an entry is found for each alert, so the rule's page says which entries,
or none):

- **Which windows it can catch:** those of the entry's signals alone. A
  window of another signal is, to this rule's firings, as if it weren't
  there: not caught by them, and not missed by them either.
- **The labels it is matched on**, against those windows: the entry's
  `match` when it gives one; else the signal's; else `--match`. Against
  written incidents, nothing changes: they are matched on `--match`, or on
  the labels a confirmation carried (below), for every rule alike.
- **What keeps its record:** the entry's signals that keep the record, and
  no other signal (below). An entry whose signals are none of those in
  effect (a `latency` signal not yet added, removed, or `record: false`)
  leaves the rule with no record by impact: its firings that caught
  nothing are counted, not judged, "no record", unless an incident written
  down keeps their week, as for any rule.

**The default, for a rule no entry names:** judged against every signal's
windows, each with the signal's own `match` (or `--match`); its record
waits for the signals tied to it by those labels (below). A ledger with
no rule entries, and one with no named signals, judge as they do now.

**Written incidents are anyone's.** An incident written down, copied in or
read from the incidents file has no signal, and every rule is judged
against it by `--match`, whatever the entries say: a person wrote it, of
whatever kind it was. An entry is about signals, which measure one kind of
harm each.

## Windows: each signal's own

A run reads each signal on its own, and `incident.FromImpact` makes its
windows from its series with its `for`, merging its own windows that
overlap, as now. **Windows of two signals are never merged**: they are two
incidents, each judged with its own `match`. Three reasons, from what
merging does now:

- Merged, they keep only the labels both have (above), so the constraint
  each had is lost.
- Merged, there is one `for` and one `match` to judge by, and no right
  choice between the two signals'; nor could a rule judged against one of
  them alone be judged against the merged one.
- Deciding is about one signal: a probe window dismissed because the probe
  itself broke says nothing of the 5xx window at the same time.

So one outage two signals saw is two windows, shown together:

- **On `/incidents`**, each window's line names its signal, and the other
  signals' windows that overlap it in time ("with probe, 14:02 to 14:09"),
  three named at most and the rest counted.
- **On a window's page**, its signal with its `for` and `match`, what caught
  it, the rule entries that keep rules from it, and the other signals'
  windows at the time, each with what caught it.
- **Confirming one** writes down an incident, as now (an `L-` id, origin
  `ui`), with the window's labels, and carrying its signal and the
  signal's `match`: it is judged as the window was, rule entries included,
  so that confirming changes no rule's verdict. That incident covers each
  other signal's window it overlaps when they agree on that window's own
  `match` labels, as a written incident covers a window now
  (`incident.Merge`, in `cover`): confirmed once, the outage is one
  incident, and the other windows no longer count. "This was real" on a
  black stone writes an incident as now, from the firing's labels on
  `--match`, with no signal: a person's, as any written incident.
- **Dismissing** is of one window. The page offers to dismiss the windows
  at that time together, one press, each noted in the activity log on its
  own.

**Keys.** On main, an impact window's key is `impact-` and its start's
minute (`impact-20261110T1404Z`), and a second window with the same key is
dropped (`ON CONFLICT ... DO NOTHING` in `ledger.impact`); #98 gives a
window whose key is taken another, `-2`, `-3` after it (`freeKey`), and
keeps a key with a window found again only when it is the same one
(`sameImpact`). A named signal's windows are keyed by its name too
(`impact-probe-20261110T1404Z`), so that two signals' windows of one
minute are never taken for one another, nor one take the other's key. The
unnamed signal keeps today's keys.

**Within a signal, as now.** What was decided about a window holds for a
window found later that covers it (`share` on main, and #98's "half or
more"), and a window on at a run's start goes on from the window found
before (the `UPDATE` in `ledger.impact`, which takes the latest impact
window still on). Both look only at windows of the same signal: as
written, the second would stretch a 5xx window with a probe window's end.

**A signal renamed** with its expression and `for` unchanged is the same
criterion: its windows are renamed in one write (their signal, and their
keys, decisions, tasks and comments kept), and nothing is read again. A
name changed with either is a signal removed and another added.

## Judged by its own match

An incident gains `Match`, the labels it is matched on, nil for `--match`,
and `Signal`, the signal it came from:

- an impact window carries its signal's;
- the incident a confirmation writes carries the window's, so it is judged
  as the window was;
- every other incident (written down, copied in, from the incidents file)
  carries none, and is judged by `--match` as now.

`verdict.Catches(f, in, o)` takes, in order: a rule entry naming `f.Rule`,
when `in` has a signal (the entry's signals must hold it, or the firing
doesn't catch it; then the entry's `match`, if it gives one); else
`in.Match`; else `o.Match`. So do the ledger's `catches`, the backtests,
the try and the digest's near misses, which judge with the same function.
Whether a written incident covers a window (`incident.Merge`, in `cover`)
is between two incidents, no rule's, and takes the window's `Match`. A
ledger with no named signal and no rule entry judges as before, and
`verdict.Semantics` isn't bumped. Changing a signal's `match`, or the rule
entries, gives the firings near the windows it changes their stones again,
from `--lead` before each window's start to its end, a week at a time, as
a change to `--match` does for every firing.

## The record, signal by signal

#98 keeps, in `meta`, what the record by the signal is kept by:
`impact_since`, `impact_criterion`, `impact_again`, `impact_data` (where
the data of each selector begins, and the floor after the latest),
`impact_read` and `impact_floor`, and from them one start
(`ImpactKept.From`). Here each signal keeps its own six, in
`impact_signals` (below), and so **its own start**: from when the runs
read it with data, by #98's one rule, signal by signal. A probe whose data
begins on a Thursday starts the probe's record the next week, and moves no
other signal's.

A firing's week is kept by impact when **every signal that could have seen
the incident it would catch was alive** then: each signal tied to the
firing, among those that keep the record, had started by the week's start
(judged by week, as `serve` judges). Which signals are tied to a firing:

1. **A rule entry**: its signals, and no others. One with none in effect
   that keeps the record ties none, and the firing has no record by impact
   (above).
2. **Else, by match.** The signals whose windows the firing could catch: for
   each, it agrees, on the signal's `match`, with some series the signal
   has returned (a label the firing hasn't agrees, as in `Agree`). A
   firing of `service: search` is tied to `errors`, which has returned
   `search`, and not to `probe`, which probes only `checkout` and `cart`;
   a firing of `checkout` is tied to both, and waits for the probe's start.
   A signal with no `match`, and one whose `match` the firing carries none
   of, could have seen anything, and is tied.
3. **Else, nothing ties it** (its labels agree with no signal's series): it
   falls back to every signal that keeps the record, and waits for the
   latest start of all, as one expression's record does now.

Or an incident keeps the week, as now: one written down, or a window that
counts, on in the week or the two before it (`verdict.RecordLookback`),
for everyone, or for a team by the incidents it owns. A signal with
`record: false` makes windows that keep the record as any incident does,
and no more: the payments signal read all week says payments weren't
hurt, not that nothing was. It is never waited for.

**What a signal has returned** is kept as it is read: for each of its
`match` labels, the values its series have had (1,000 a label at most;
past that, it is taken to have returned every value, and is tied to every
firing that carries the label). A value seen later ties the firings
before it too, which then wait for that signal's start: a firing is never
judged by a signal that couldn't see it, and its record may wait longer
than it needs. Where each value's data begins, apart from the signal's, is
not looked for, as #98 looks per selector, not per series.

**Changing signals**, per signal:

- **added, or its expression or `for` changed:** read again alone over the
  last four weeks (#98's `impactAgain` and `ledger.ReadAgain`, by the
  signal's name), its windows in place of its own from then, decisions
  kept as #98 keeps them, and where its data begins looked for again. The
  other signals aren't read, and keep their starts.
- **removed:** turned off, as #98 turns off its one: its windows stay, as
  incidents, it no longer keeps the record, and no firing waits for it.
- **`match`, `record` or the rule entries changed:** nothing is read.
  `match` and the entries judge their windows' spans again (above); which
  firings are tied to which signals is worked out again, a week at a time
  (below).
- **A run that can't read one signal.** Today the impact query failing
  fails the run, and nothing is recorded. With several, one broken
  expression would stop the ledger. A signal that fails is missed instead,
  from the run's start, as a rule whose queries failed is (`Found.Missed`):
  the run records, later runs read the signal from where it was missed,
  for up to a week, and past a week it is as one #98 turns on again: its
  record starts with the run that reads it, and a read again is due. While
  it is missed, its start stands, so its
  weeks are kept before its windows are in, and judged again when it
  catches up, as a rule's late firings are. The Incident sources panel
  says it is failing, and why.

## Where it shows

- **Settings.** Incident criteria: the signals and the rule entries
  (above); the try reads the signals it would change, and shows each one's
  windows apart, and the verdict in effect and with all of them. Incident
  sources: a row a signal, with when it was last read, its windows in four
  weeks, failing with Prometheus's error, each selector and where its data
  begins, and from when the signal keeps the record.
- **A rule's page**: the entry that names it, or none; the signals it is
  judged against, each with the labels it is matched on; and the signals
  its record waits for, with the latest start among them: "no record
  before 2026-11-09: it waits for probe".
- **`/incidents`**: `?source=impact&signal=NAME`, a chip for each signal, and
  each line's signal, with the windows overlapping it (above).
- **The overview**: as now. Its counts of impact windows, and "Decide on N
  impact windows", count every signal's; the link goes to `/incidents` for
  impact windows.
- **The digest and reports**: `missed` names each missed window's signal;
  `summary`'s line of impact windows says each signal's, when there are
  several.
- **`/api/v1/summary`**: `impacts.by_signal`, `unjudged` in each row, and
  `settings.impact_signals` (below).
- **`/metrics`**: `lapilli_incident_state` and `lapilli_incident_lead_seconds`
  gain `signal` on a named signal's windows (one incident has one signal,
  so no series changes as it goes); `lapilli_impact_windows{signal,state}`,
  the last seven days' windows by signal and state;
  `lapilli_impact_signal_start_timestamp_seconds{signal}`, from when each
  keeps the record. `lapilli_incidents{state,source}` stays as it is, all
  signals summed.
- **MCP**: `incidents` gives each window's `signal`; `rule` gives its entry,
  its signals and what its record waits for; `summary` gives `by_signal`.
- **`lapilli verdict`**: `--json` gives each incident's `signal`; `--metrics`
  gives `lapilli_impact_windows`. It reads each signal over the period and
  the two weeks before, as it reads `--impact` now, and ties firings to
  signals as `serve` does, each signal's start where its data begins.

## How they are counted

A window is caught, caught only by rules noisy in its week, or missed, as
an incident is now (`tallies.state`); a rule an entry keeps from a signal
neither catches nor misses that signal's windows, so a window every rule
is kept from is missed. In each summary row:

```json
"unjudged": 14,
"impacts": {"total": 3, "caught": 2, "noisy_only": 0, "missed": 1, "median_lead_seconds": 120,
  "by_signal": {
    "errors":   {"total": 1, "caught": 1, "noisy_only": 0, "missed": 0, "median_lead_seconds": 240},
    "probe":    {"total": 1, "caught": 1, "noisy_only": 0, "missed": 0, "median_lead_seconds": 0},
    "payments": {"total": 1, "caught": 0, "noisy_only": 0, "missed": 1, "median_lead_seconds": null}}},
"settings": {"impact_signals": [{"name": "errors", "for": "5m", "match": ["service", "team"], "record": true,
                                 "start": "2026-10-05T00:00:00Z"}, "..."],
             "impact_rules": [{"rules": "^BackendMissing$", "signals": ["errors", "probe"], "match": ["backend"]}, "..."]}
```

- `total` counts each window once, every signal's. An outage two signals
  saw is two windows. A team that splits one `or` expression into signals
  sees `total` rise where windows of different series overlapped, which
  were one incident before; the summary's field means what it did (impact
  windows judged against), and the changelog says so.
- `by_signal` is `{}` with no named signal. A window of a signal since
  removed is counted under its name, as it is kept.
- **A team's row** counts its rules' firings and the windows it owns, as for
  any incident: a value of a label it owns by, or caught by one of its
  rules. The "no backend" window above is a service team's by its `team`
  label, and the platform team's too once the platform team's rule caught
  it, on `backend`: caught, in both rows. Each firing of a team's rules is
  judged or not by its own ties, so a row's precision is of the firings
  judged.
- **`no_record` and `unjudged`.** A row's `no_record` is true when none of
  its firings is judged, which, with no named signal, is what it says now
  (no incident on in the week or the two before, no signal read over it).
  `unjudged`, a field added, is how many of the row's firings were counted
  and not judged: with signals of their own starts and entries, part of a
  week can be judged and part not, as `verdict.SomeUnjudgedText` says of a
  report now.
- `settings.impact_signals` gives names, `for`, `match`, `record` and each
  start, not the expressions: the summary is for any tool that reads the
  API, and an expression can name what isn't everyone's to read.
  `settings.impact_rules` gives the entries.

`schema` stays 1: these are fields added, and `no_record` means what it
did wherever it could be said before (open question 6).

## In the ledger

Schema 025:

- `impact_signals`: `name` (unique; empty for the unnamed signal), `expr`,
  `for`, `match` (a JSON list, NULL for `--match`), `record`, who set it in
  the UI and when (none for the command line's or the file's); #98's six
  (`since`, `criterion`, `again`, `data`, `read`, `floor`), per signal; and
  `seen`, the values of its `match` labels its series have returned.
- `impact_rules`: the rule entries in effect, in order: `rules`, `labels`,
  `signals`, `match`, and whether the UI set them.
- `incidents.signal` (empty for none) and `incidents.match` (NULL for
  `--match`).
- `series.signals`: the signals each alert is tied to (a bit each, eight at
  most, and one for "falls back to all"), worked out from its labels, its
  rule's entry and what each signal has returned; and in `days` and
  `weeks`, `signals` beside `rule_id`, so a rule's firings are counted
  apart by the signals they wait for. Most rules have one such set, so
  the tallies grow by the few rules whose alerts span services that
  different signals see.

Upgrading moves #98's six from `meta`, and the UI's `impact` and
`impact-for` settings, into the unnamed signal's row; gives existing
windows no signal (they are the unnamed signal's, keyed as they are); and
ties every alert to that one signal, so the tallies are as they were.
Going back takes the ledger as it was before, so a deployment keeps a copy
of it (a snapshot of its disk, or of its database) before it upgrades, as
with every schema change.

## Cost and bounds

- **Reading.** Each signal is one range query a run, over the run's period
  and the hour before, as the impact query is now. They are read in turn,
  the one missed longest first, within the time the impact query has
  (`--timeout`); one left unread when it runs out is missed for that run.
  A run asks Prometheus for no more time than it does now. Where each
  signal's data begins is looked for as #98 looks for its one's, a
  selector at a time, 32 selectors a signal at most.
- **Reading again.** Only the signals added or changed, each over five
  weeks at most, within `--timeout` and `--max-samples`; each counts as one
  of the twelve reads an admin may ask for in an hour (#98's tries and
  saves).
- **Windows.** A signal's read keeps 1,000 windows at most; past that the
  signal is missed for the run, and the sources panel says it finds too
  many (an expression by pod, not by service). Each window's labels are
  bounded as an incident's are (4 KB, as kept).
- **Ties.** An alert's signals are worked out when its series is first kept,
  and again when a signal's `match`, what it has returned, or the entries
  change: those alerts' firings are counted again, a week at a time, by
  the worker, as a change to `--match` judges every firing again. A value
  first returned is rare after the first weeks, and ties only the alerts
  that carry it.
- **Judging.** `verdict.Catches` looks up the rule's entry once per rule
  (the entries are compiled once per change) and reads a list of names per
  incident rather than one for all; the incidents meeting a span are read
  once a judge, as now.
- **Pages** read the tallies as now, summing a rule's sets, and each set's
  record from the signals' starts: no firing is read to say whether it was
  judged. Held to `TestPagesScale`, and tying again to `TestScale`.
- **`/metrics`**: `signal` takes eight values at most.

## Security

- **Who.** Signals and rule entries are set where `serve` starts, or by an
  admin on the Settings page, as #98 sets its one. An expression runs with
  Lapilli's access to Prometheus, so it can learn, a count at a time,
  whatever that access reads, as MCP's `compare` can ([serve.md](serve.md),
  "MCP"); that is why it is an admin's. An entry decides which windows a
  rule's firings are judged against, which can make a rule look better
  (kept from windows it misses) or take its record away: an admin's too,
  and each change noted in the activity log with its before and after.
- **What comes in.** A name of 32 bytes from a small alphabet, so it is safe
  in a key, a URL and a label value, and still escaped in each; label names
  checked as Prometheus's; the expression checked as #98 checks it; rule
  patterns checked as a team's are. The series' labels become windows'
  labels, kept as an incident's are, and shown escaped; the values a
  signal has returned are bounded as above.
- **Where names go.** Into incidents' keys and pages, the summary,
  `/metrics` labels (escaped, eight values at most), the digest and
  reports, as text a receiver escapes, as it does `digest.ready`'s.

## Not in this design

- **A `--lead` for each signal.** `--lead` stays one, for every incident.
- **Signals that must agree.** A window only when two signals are on at
  once. An expression joining them with `and` does it today.
- **A signal's weight or severity.** Every window is an incident.
- **A signal's record kept for one team.** Ties are by labels and rules, not
  by team; a team's pages read its rules' ties.
- **Written incidents of a kind.** A rule entry keeps a rule from other
  signals' windows, not from incidents people wrote down.
- **Windows merged across signals** (above).
- **Signals declared in `--config`.** They are judging settings, set as the
  others are: the command line, `--impact-file` and the Settings page.

## Alternatives

- **Use what exists: one expression, `or`, `label_replace`.** Each window
  says its signal in its labels, and the Incidents page shows labels; that
  is as far as it goes. `for`, `match`, the start and the counts are one,
  and windows that meet lose their labels (above). A `for` written into
  PromQL, `min_over_time((probe_success == bool 0)[2m:15s]) == 1`, returns
  only from two minutes after the probe first failed, so each window
  starts that much late, and every lead is off by as much: `--impact-for`
  keeps a window's start where the signal first said users were hurt.
- **Use what exists: `--match` without `team`.** It lets the "no backend"
  rule catch, and every other rule then catches another team's windows of
  the same service, which is what `team` was in `--match` to stop.
- **A job that evaluates each signal and copies its windows in** through the
  incidents port, each with a prefix of its own (`PROBE-41`), which #98's
  sources panel shows apart. They would be written incidents: never to
  decide, judged by `--match` for every rule, keeping the record only near
  them, never read again when the expression changes, and the job's to
  keep running.
- **Merging windows across signals with the union of their labels.** A
  firing would have to agree with labels of both signals, which no alert
  does (a probe's URL and a service's name); and which `for` and `match`
  would judge it is still not answered.
- **One start for all, the latest** (#98's rule, over the signals). Simple,
  and the youngest signal takes every rule's record away (above).
- **Ties by rule, not by alert.** A rule tied to every signal any of its
  alerts could have seen needs no change to the tallies, and holds a rule
  over many services to the youngest signal of any of them. Proposed by
  alert instead, as the alert's labels are what a window is matched on.
- **Kinds instead of names.** Signals and rules each given a kind
  (`latency`, `availability`), a rule judged against signals of its kinds.
  One word more to keep in step; an entry naming signals says the same.
- **Match labels by team** (a team's `match`). The "no backend" case is
  about a rule, not its team: the platform team's other rules match on
  `team` well.

## Related

Another change proposes that an incident merged across services keeps
each service's own span, so that one incident over several services
doesn't hide when each was hurt. Windows of several signals aren't merged
here; should they ever be shown or counted as one outage (open question
1), each signal's window keeps its own span, by the same rule.

## Open questions

1. **An outage counted once.** Whether `impacts` should also give how many
   times any signal had a window, windows of several signals that overlap
   counted once (`impacts.outages`), for a team that reports incidents,
   not windows.
2. **A rule kept from a window, and credit.** A rule an entry keeps from the
   5xx windows doesn't catch them, even when it fired first. Whether it
   should be credited with the catch, and only kept from being black for
   missing them.
3. **Written incidents of a kind.** A written incident with a label such as
   `kind: latency` could be held to the entries as a window is.
4. **`--lead` by signal.** A probe fails before users write in; an error
   share rises with them. Whether alerts may come earlier before one than
   the other.
5. **Mixing named and unnamed.** Refused here, so that the unnamed one keeps
   today's keys and nothing else. An alternative is to call it `impact`.
6. **`no_record` in a team's row.** It is everyone's today, also in a team's
   row (`summaryOf` reads the record once, for everyone), while a team's
   pages read the team's (`teamKeeps`). With ties it is the row's own
   firings'. Whether that is a field changed in meaning, which `schema: 1`
   promises not to do, or the meaning it always had.
7. **Expectations naming an impact window.** [expectations.md](expectations.md)
   holds a rule to catching an incident by its id. An impact window's key
   is its start, which can change when its signal is read again; an
   expectation should name the incident its confirmation writes.

## Steps

1. `lapilli verdict`: an incident's own `match` and signal,
   `--impact NAME=PROMQL`, `--impact-for NAME=DUR`, `--impact-match`,
   `--impact-record`, `--impact-file` with rule entries, ties and each
   signal's start, and each incident's `signal` in `--json`. No schema
   change.
2. `serve`: schema 025; the runs read each signal, missed on its own; each
   signal's start; ties by alert, and the tallies by them; keys and
   decisions by signal; the Incidents page, a rule's page, the summary,
   `/metrics`, MCP.
3. #98's section by signal: the list and the rule entries, the try, reading
   again a signal alone, a row a signal in the sources panel.
