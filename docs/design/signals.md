# Impact signals: several, each named, with its own `for` and `match`

Status: proposed. Under [GOVERNANCE.md](../../GOVERNANCE.md) this changes
how a firing is judged (an incident gets labels of its own to be matched on)
and adds a new kind of data to the ledger (schema 025), so it is designed
first, as its own pull request. It builds on bundle 31 (branch
`b31-incident-criteria`), not merged as this is written, which puts the
impact signal on the Settings page: the "Incident criteria" section, its
try on the last four weeks, the criterion the record is kept by, and the
"Incident sources" panel (its changes to [serve.md](serve.md) and
[ui.md](ui.md), and `internal/ledger/impact.go`,
`internal/serve/criteria.go`). Where this note says "the criterion", "read
again" or "the try", it means that work's.

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
- payments succeeding at none while some are tried, with no labels: worth a
  window after one.

Today they go into one expression, joined with `or`, each tagged with
`label_replace(..., "signal", "probe", "", "")` so that its windows say
which it was. That finds windows of all three, and stops there:

- **One `for`.** `--impact-for` is the same for every series.
- **Labels lost where windows meet.** `incident.FromImpact` makes windows
  that overlap in time one incident, keeping only the labels all of them
  have (`delete(in.Labels, k) // not all have it: no constraint`). A probe
  window (`instance`, `job`) and a 5xx window (`service`) on at once become
  one incident labelled `signal: [errors, probe]` and nothing else: with
  `--match service`, a firing of any service catches it.
- **One `--match`.** `verdict.Catches` holds every incident to the same
  `--match` (`in.Labels.Agree(f.Labels, o.Match)`). A probe's `job` and
  `instance` name the prober and the URL it fetched, which no alert
  carries: with `job` in `--match`, nothing catches a probe window, since a
  label both have must agree.
- **Nothing told apart.** The Incidents page, the summary's `impacts` and
  `/metrics` count impact windows all together; a window is confirmed or
  dismissed on its own, but nothing says which signal is worth keeping.

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
  --impact 'errors=sum by (service) (rate(http_requests_total{code=~"5.."}[5m]))
      / sum by (service) (rate(http_requests_total[5m])) > 0.05
      and sum by (service) (rate(http_requests_total[5m])) > 1' \
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
followed by `=` or `~`. No PromQL expression starts so: a `=` on its own
is an operator only inside a selector's braces, after a label's name,
never after a metric's; `up==0` and `up>=1` read as the expressions they
are, and `errors=rate(x[5m]) > 1` as the signal `errors`. `--impact PROMQL` with no name stays what it is
today: one signal, its windows keyed, titled and keeping the record as
now. Named and unnamed aren't mixed; serve refuses them together and says
to name each. `--impact-for` takes `DUR` (every signal's default, as now)
or `NAME=DUR`; `--impact-match` and `--impact-record` take `NAME=...` and
are refused for a name no `--impact` gives. `lapilli verdict` takes the
same flags.

**In the chart**, `impact` stays a string, or becomes a list:

```yaml
impact:
  - {name: errors, expr: "...", for: 5m, match: [service]}
  - {name: probe, expr: "probe_success == 0", for: 2m, match: [service]}
  - {name: payments, expr: "...", for: 1m, record: false}
```

**In the UI**, bundle 31's Incident criteria section lists the signals, a
row each, with what serve was started with, who changed it and when, and
"back to the start", as the section does for its one expression. An admin
adds a signal, changes one or removes one; the UI's is kept over the
command line's of the same name, as the judging settings are, and
`/settings.yaml` gives the signals back as the chart's list. A signal's
name can be changed in place, which renames its windows with it (below).

**Bounds.** Eight signals at most. The expression as bundle 31 checks it:
4 KB, read as Lapilli reads PromQL (`rules.Scope`), then once by
Prometheus, whose parser has the last word; `for` from the step to a day;
`match` 16 label names at most, each a Prometheus label name.

## Windows: each signal's own

A run reads each signal on its own, and `incident.FromImpact` makes its
windows from its series with its `for`, merging its own windows that
overlap, as now. **Windows of two signals are never merged**: they are two
incidents, each judged with its own `match`. Three reasons, from what
merging does now:

- Merged, they keep only the labels both have (above), so the constraint
  each had is lost.
- Merged, there is one `for` and one `match` to judge by, and no right
  choice between the two signals'.
- Deciding is about one signal: a probe window dismissed because the probe
  itself broke says nothing of the 5xx window at the same time.

So one outage two signals saw is two windows, shown together:

- **On `/incidents`**, each window's line names its signal, and the other
  signals' windows that overlap it in time ("with probe, 14:02 to 14:09"),
  three named at most and the rest counted.
- **On a window's page**, its signal with its `for` and `match`, what caught
  it, and the other signals' windows at the time, each with what caught it.
- **Confirming one** writes down an incident, as now (an `L-` id, origin
  `ui`), with the window's labels and its signal's `match`. That incident
  covers each other signal's window it overlaps when they agree on that
  window's own `match` labels, as a written incident covers a window now
  (`incident.Merge`, in `cover`): confirmed once, the outage is one
  incident, and the other windows no longer count. A confirmation that
  names no label the others match on covers all of them.
- **Dismissing** is of one window. The page offers to dismiss the windows
  at that time together, one press, each noted in the activity log on its
  own.

**Keys.** An impact window's key is `impact-` and its start's minute
(`impact-20261110T1404Z`), unique in the ledger, and a second window kept
with the same key is dropped (`ON CONFLICT ... DO NOTHING` in
`ledger.impact`). Two signals' windows can start in the same minute, so a
named signal's key holds its name: `impact-probe-20261110T1404Z`. The
unnamed signal keeps today's keys, and links to them keep working.

**Within a signal, as now.** What was decided about a window holds for a
window found later that overlaps it and shares a value of each label both
have (`share`, in `ledger.impact`), and a window on at a run's start goes
on from the window found before (the `UPDATE` there, which takes the
latest impact window still on). Both look only at windows of the same
signal: as written, the second would stretch a 5xx window with a probe
window's end.

**A signal renamed** with its expression and `for` unchanged is the same
criterion: its windows are renamed in one write (their signal, and their
keys, decisions kept), and nothing is read again. A name changed with
either is a signal removed and another added.

## Judged by its own match

An incident gains `Match`, the labels it is matched on, nil for `--match`:

- an impact window carries its signal's;
- the incident a confirmation writes carries the window's, so it is judged
  as the window was;
- every other incident (written down, copied in, from the incidents file)
  carries none, and is judged by `--match` as now.

`verdict.Catches` takes `in.Match` when it is given, `o.Match` otherwise; so
do `incident.Merge` and the ledger's `cover`, when they decide whether a
written incident covers a window, and the backtests, the try and the
digest's near misses, which judge with the same function. A ledger with no
named signal judges as before, and `verdict.Semantics` isn't bumped.
Changing a signal's `match` gives the firings near its windows their stones
again, from `--lead` before each window's start to its end, a week at a
time, as a change to `--match` does for every firing.

## The record

Bundle 31 keeps, in `meta`, from when the runs read the impact signal all
of it (`impact_since`), with what criterion (`impact_criterion`, its
expression and `--impact-for`), and whether the weeks before were read
again with it (`impact_again`). Each signal keeps its own three, in
`impact_signals` (below). Then:

- **A week is kept by impact** when every signal that keeps the record was
  read over all of it: `verdict.Options.ImpactFrom` is the latest `since`
  of those signals, and none when any of them has none. The signals are
  one criterion, as bundle 31's one expression is: a week read without one
  of them wasn't read with the criterion in effect.
- **A signal that doesn't keep the record** (`record: false`) makes windows,
  judged against, which keep the record as any incident does, over the
  two weeks after each (`verdict.RecordLookback`), and no more. The
  payments signal read all week says payments weren't hurt, not that
  nothing was.
- **A signal added, or its expression or `for` changed,** is read again
  alone over the last four weeks (`impactAgain` and `ledger.ReadAgain`,
  with the signal's name): its windows take the place of its own from
  then, decisions kept as above, and its `since` is then. The other
  signals aren't read, and keep theirs.
- **A signal removed** is turned off, as bundle 31 turns off its one: its
  windows stay, as incidents, and it no longer keeps the record. The
  others keep theirs, so a removal can only keep more weeks, never fewer.
- **`match` or `record` changed** reads nothing: `match` judges its
  windows' spans again (above); `record` changes which weeks are kept,
  which the tallies read when a page is worked out, as now.
- **A run that can't read one signal.** Today the impact query failing
  fails the run, and nothing is recorded. With several, one broken
  expression would stop the ledger. A signal that fails is missed instead,
  from the run's start, as a rule whose queries failed is (`Found.Missed`):
  the run records, later runs read the signal from where it was missed,
  for up to a week, and past a week its `since` starts again with the run
  that reads it. While it is missed, its `since` stands, so its weeks are
  kept before its windows are in, and judged again when it catches up, as
  a rule's late firings are. The Incident sources panel says it is
  failing, and why.

## Where it shows

- **Settings.** Incident criteria: the signals (above); the try reads the
  signals it would change, and shows each one's windows apart, and the
  verdict in effect and with all of them. Incident sources: a row a
  signal, with when it was last read, its windows in four weeks, and
  failing with Prometheus's error.
- **`/incidents`**: `?source=impact&signal=NAME`, a chip for each signal, and
  each line's signal, with the windows overlapping it (above).
- **The overview**: as now. Its counts of impact windows, and "Decide on N
  impact windows", count every signal's; the link goes to `/incidents` for
  impact windows.
- **The digest and reports**: `missed` names each missed window's signal;
  `summary`'s line of impact windows says each signal's, when there are
  several.
- **`/api/v1/summary`**: `impacts.by_signal`, and `settings.impact_signals`
  (below).
- **`/metrics`**: `lapilli_incident_state` and `lapilli_incident_lead_seconds`
  gain `signal` on a named signal's windows (one incident has one signal,
  so no series changes as it goes); `lapilli_impact_windows{signal,state}`,
  the last seven days' windows by signal and state. `lapilli_incidents
  {state,source}` stays as it is, all signals summed.
- **MCP**: `incidents` gives each window's `signal`; `summary` gives
  `by_signal`.
- **`lapilli verdict`**: `--json` gives each incident's `signal`; `--metrics`
  gives `lapilli_impact_windows`.

## How they are counted

A window is caught, caught only by rules noisy in its week, or missed, as
an incident is now (`tallies.state`). In each summary row:

```json
"impacts": {"total": 3, "caught": 2, "noisy_only": 0, "missed": 1, "median_lead_seconds": 120,
  "by_signal": {
    "errors":   {"total": 1, "caught": 1, "noisy_only": 0, "missed": 0, "median_lead_seconds": 240},
    "probe":    {"total": 1, "caught": 1, "noisy_only": 0, "missed": 0, "median_lead_seconds": 0},
    "payments": {"total": 1, "caught": 0, "noisy_only": 0, "missed": 1, "median_lead_seconds": null}}},
"settings": {"impact_signals": [{"name": "errors", "for": "5m", "match": ["service"], "record": true}, "..."]}
```

- `total` counts each window once, every signal's. An outage two signals
  saw is two windows. A team that splits one `or` expression into signals
  sees `total` rise where windows of different series overlapped, which
  were one incident before; the summary's field means what it did (impact
  windows judged against), and the changelog says so.
- `by_signal` is `{}` with no named signal. A window of a signal since
  removed is counted under its name, as it is kept.
- A team's row counts the windows the team owns, as for any incident: a
  value of a label it owns by, or caught by one of its rules.
- `settings.impact_signals` gives names, `for`, `match` and `record`, not the
  expressions: the summary is for any tool that reads the API, and an
  expression can name what isn't everyone's to read.

`schema` stays 1: these are fields added.

## In the ledger

Schema 025:

- `impact_signals`: `name` (unique; empty for the unnamed signal), `expr`,
  `for`, `match` (a JSON list, NULL for `--match`), `record`, who set it in
  the UI and when (none for the command line's); and `since`, `criterion`
  and `again`, bundle 31's three, per signal. The command line's signals
  are written as a term begins; the UI's are kept over them, by name.
- `incidents.signal` (empty for none) and `incidents.match` (NULL for
  `--match`).

Upgrading moves bundle 31's `impact_since`, `impact_criterion` and
`impact_again`, and the UI's `impact` and `impact-for` settings, into the
unnamed signal's row, and gives existing windows no signal: they are the
unnamed signal's, keyed as they are. Going back takes the ledger as it was
before, so a deployment keeps a copy of it (a snapshot of its disk, or of
its database) before it upgrades, as with every schema change.

## Cost and bounds

- **Reading.** Each signal is one range query a run, over the run's period
  and the hour before, as the impact query is now. They are read in turn,
  the one missed longest first, within the time the impact query has
  (`--timeout`); one left unread when it runs out is missed for that run.
  A run asks Prometheus for no more time than it does now.
- **Reading again.** Only the signals added or changed, each over five
  weeks at most, within `--timeout` and `--max-samples`; each counts as one
  of the twelve reads an admin may ask for in an hour (bundle 31's tries
  and saves).
- **Windows.** A signal's read keeps 1,000 windows at most; past that the
  signal is missed for the run, and the sources panel says it finds too
  many (an expression by pod, not by service). Each window's labels are
  bounded as an incident's are (4 KB, as kept).
- **Judging.** `verdict.Catches` reads a list of names per incident rather
  than one for all; the incidents meeting a span are read once a judge, as
  now. Nothing is read per signal when a page is worked out but the
  incidents it already reads.
- **`/metrics`**: `signal` takes eight values at most.

## Security

- **Who.** Signals are set where `serve` starts, or by an admin on the
  Settings page, as bundle 31 sets its one. An expression runs with
  Lapilli's access to Prometheus, so it can learn, a count at a time,
  whatever that access reads, as MCP's `compare` can ([serve.md](serve.md),
  "MCP"); that is why it is an admin's.
- **What comes in.** A name of 32 bytes from a small alphabet, so it is safe
  in a key, a URL and a label value, and still escaped in each; label names
  checked as Prometheus's; the expression checked as bundle 31 checks it.
  The series' labels become windows' labels, kept as an incident's are,
  and shown escaped.
- **Where names go.** Into incidents' keys and pages, the summary,
  `/metrics` labels (escaped, eight values at most), the digest and
  reports, as text a receiver escapes, as it does `digest.ready`'s.

## Not in this design

- **A `--lead` for each signal.** `--lead` stays one, for every incident.
- **Signals that must agree.** A window only when two signals are on at
  once. An expression joining them with `and` does it today.
- **A signal's weight or severity.** Every window is an incident.
- **A signal's record kept for one team.** The record by impact is
  everyone's, as now.
- **Windows merged across signals** (above).
- **Signals declared in `--config`.** They are judging settings, set as the
  others are: the command line and the Settings page.

## Alternatives

- **Use what exists: one expression, `or`, `label_replace`.** Each window
  says its signal in its labels, and the Incidents page shows labels; that
  is as far as it goes. `for`, `match` and the counts are one, and windows
  that meet lose their labels (above). A `for` written into PromQL,
  `min_over_time((probe_success == bool 0)[2m:15s]) == 1`, returns only
  from two minutes after the probe first failed, so each window starts
  that much late, and every lead is off by as much: `--impact-for` keeps a
  window's start where the signal first said users were hurt.
- **A job that evaluates each signal and copies its windows in** through the
  incidents port, each with a prefix of its own (`PROBE-41`), which bundle
  31's sources panel shows apart. They would be written incidents: never
  to decide, judged by `--match`, keeping the record only near them, never
  read again when the expression changes, and the job's to keep running.
- **Merging windows across signals with the union of their labels.** A
  firing would have to agree with labels of both signals, which no alert
  does (a probe's URL and a service's name); and which `for` and `match`
  would judge it is still not answered.
- **A file of signals** (`--impact-file`). The flags and the chart's list
  say the same, and the UI is where bundle 31 puts them.

## Open questions

1. **An outage counted once.** Whether `impacts` should also give how many
   times any signal had a window, windows of several signals that overlap
   counted once (`impacts.outages`), for a team that reports incidents,
   not windows.
2. **A signal's record for its team.** A signal by service, read all week,
   could keep the record of the teams that own those services' labels only.
3. **`--lead` by signal.** A probe fails before users write in; an error
   share rises with them. Whether alerts may come earlier before one than
   the other.
4. **Mixing named and unnamed.** Refused here, so that the unnamed one keeps
   today's keys and nothing else. An alternative is to call it `impact`.
5. **Expectations naming an impact window.** [expectations.md](expectations.md)
   holds a rule to catching an incident by its id. An impact window's key
   is its start, which moves when its signal is read again; an expectation
   should name the incident its confirmation writes. Whether the draft
   from the history should do so for it.

## Steps

1. `lapilli verdict`: an incident's own `match`, `--impact NAME=PROMQL`,
   `--impact-for NAME=DUR`, `--impact-match`, `--impact-record`, and each
   incident's `signal` in `--json`. No schema change.
2. `serve`: schema 025; the runs read each signal, missed on its own; the
   record by the signals that keep it; keys and decisions by signal; the
   Incidents page, the summary, `/metrics`, MCP.
3. Bundle 31's section by signal: the list, the try, reading again a
   signal alone, a row a signal in the sources panel.
