# Impact signals: several, each named, with its own `for`, `match` and start

Status: accepted; steps 1 (`lapilli verdict`, 0.53.0, "Step 1, as built"
below), 2 (`lapilli serve`, schema 022, 0.55.0, "Step 2, as built") and 3
(the Settings page by signal, no schema change, 0.58.0, "Step 3, as built")
built. Under [GOVERNANCE.md](../../GOVERNANCE.md)
this changes how a firing is judged (an incident, and a rule beside a
signal, get labels of their own to be matched on, and a rule can be judged
against some signals only) and adds a new kind of data to the ledger (the
next schema at build time), so it was designed first, as a pull request
(#88); it was merged on 2026-11-16, before its week was out, by the
maintainer's decision, with the review's edits. The decisions taken are in
"Open questions"; the rest stay open until the step that needs each.

It builds on v0.49.0 (#98), which put the impact signal on the Settings
page: the "Incident criteria" section, its try on the last four weeks, the
one rule the record by the signal is kept by (read, with data, by the
criterion in effect), where the data of each selector it reads begins
(`rules.Selectors`), and the "Incident sources" panel, all kept in `meta`
with no schema change ([serve.md](serve.md), "The impact criterion";
[ui.md](ui.md); `internal/ledger/impact.go`,
`internal/serve/criteria.go`). Where this note says "the criterion", "read
again", "the try" or "where the data begins", it means that work's.

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
- **One start.** v0.49.0 keeps the record by the signal from the first
  whole week after the latest first sample of any selector the expression
  reads. Joined, the youngest signal decides for all: a probe whose data
  begins on a Thursday takes away the record of every week before, though
  the 5xx share beside it has a month of data and said, all that month,
  that nothing hurt the services it measures.
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
| `match` | the labels a firing must agree with its windows on | the match in effect |
| `record` | whether reading it all of a week keeps that week's record | yes |

**The match in effect** is the Settings page's, when an admin set one, else
`--match`, as for every incident now. A signal's `match` not given (nil)
follows it, and changes when it changes; one given empty
(`--impact-match probe=`, `match: []`) matches on no label, so any firing
during one of its windows catches it. The two are kept apart.

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

(The probe's targets here are relabelled with the `service` they probe.)

**Reading `NAME=PROMQL`.** A signal is named when `--impact`'s value starts
with a name (`[a-z][a-z0-9_-]`, 32 bytes at most) and a `=` that isn't
followed by `=` or `~`. No PromQL expression starts so: a lone `=` is
PromQL only inside a selector's braces, after a label's name, never after
a metric's; `up==0` and `up>=1` read as the expressions they are, and
`errors=rate(x[5m]) > 1` as the signal `errors`. `--impact PROMQL` with no
name stays what it is today: one signal, its windows keyed, titled and
keeping the record as now. Two `--impact` without a name are refused from
this change on; today the last one given wins (`verdict.go`, where
`--impact` sets `f.impact`), which the changelog says under Changed.
`--impact-for` takes `DUR` (every signal's default, as now) or `NAME=DUR`;
`--impact-match` and `--impact-record` take `NAME=...` and are refused for
a name no `--impact` gives. `lapilli verdict` takes the same flags.

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
      labels: {kind: latency}       # whose labels have these values
      signals: [latency]
    - rules: ^BackendMissing$       # a load balancer's "no backend" rule
      signals: [errors, probe]
      match: [backend]              # matched on these labels alone
```

Read strictly, as `--config` is: an unknown key, a signal named twice, a
rule entry naming a signal the file doesn't declare, or a file over 1 MiB
refuses the whole file, and the ledger keeps what the last good one said.

**`--impact-file` is the command line, not `--config`.** It says what the
flags say, in a file, so the UI's settings are kept over it as over the
flags', with "back to the start", as `--lead` and `--match` are. What
`--config` declares is what the UI makes (teams, quiet windows, reports,
grants), and is read-only in the UI. Signals and rule entries are judging
settings, which an admin tries and saves on the Settings page; a team that
wants them only in Git leaves the Settings page's untouched, and
`/settings.yaml` says when one differs.

**In the UI**, v0.49.0's Incident criteria section lists the signals, a
row each, with what serve was started with, who changed it and when, and
"back to the start", as the section does for its one expression; and the
rule entries below them, as one list. An admin adds a signal, changes one
or removes one; the UI's is kept over the command line's of the same name.
The rule entries, saved in the UI, take the place of the file's as a
whole, and "back to the start" gives the file's back. `/settings.yaml`
gives both as the chart's `impact`. A signal's name can be changed in
place, which renames its windows with it (below).

**Bounds.** Eight signals at most. The expression as v0.49.0 checks it:
4 KB, read as Lapilli reads PromQL (`rules.Scope`), then once by
Prometheus, whose parser has the last word; `for` from the step to a day;
`match` 16 label names at most, each a Prometheus label name. Rule
entries: 64 at most, each a pattern as a team's or a quiet window's is (a
Go regular expression of 200 bytes, refused when it compiles to a long
program), 16 labels, 8 signals.

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

**Which entry is a rule's** is found once per rule, as a team is
(`owners`): the first entry, in order, whose pattern matches the rule's
name; else the first whose labels the rule has, as the latest version of
the rules gives them. The name decides before any label, as it does for
teams. The rule's page says which entry, or none.

What an entry changes, for the rules it names:

- **Which windows it can catch:** those of the entry's signals alone. A
  window of another signal is, to this rule's firings, as if it weren't
  there: not caught by them, and not missed by them either.
- **The labels it is matched on**, against those windows: the entry's
  `match` when it gives one; else the signal's; else the match in effect.
  Against written incidents nothing changes: they are matched on the match
  in effect, or on the labels a confirmation carried (below), for every
  rule alike.
- **What keeps its record:** the entry's signals that keep the record, and
  no other signal (below). An entry whose signals are none of those in
  effect (a `latency` signal not yet added, removed, or `record: false`)
  leaves the rule with no record by reading: its firings that caught
  nothing are counted, not judged, "no record", unless an incident written
  down keeps their week, as for any rule, or a window of one of the
  entry's signals does.

**The default, for a rule no entry names:** judged against every signal's
windows, each with the signal's own `match` (or the match in effect); its
record waits for the signals tied to it by those labels (below). A ledger
with no rule entries, and one with no named signals, judge as they do now.

**Written incidents are anyone's.** An incident written down, copied in or
read from the incidents file has no signal, and every rule is judged
against it by the match in effect, whatever the entries say: a person
wrote it, of whatever kind it was. An entry is about signals, which
measure one kind of harm each.

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
  signal's `match`: it is caught as the window was, rule entries included,
  so that confirming changes no rule's catch. It keeps them while its
  labels are those it was confirmed with: a title, a time or a note
  changed keeps them; its labels changed make it a person's like any
  other, matched on the match in effect, which the form says before it is
  saved. That incident covers each other signal's window it overlaps when
  they agree on that window's own `match` labels, as a written incident
  covers a window now (`incident.Merge`, in `cover`): confirmed once, the
  outage is one incident, and the other windows no longer count. "This was
  real" on a black stone writes an incident as now, from the firing's
  labels on the match in effect, with no signal: a person's.
- **Dismissing** is of one window. The page offers to dismiss the windows
  at that time together, one press, each noted in the activity log on its
  own.

**Keys.** A window's key is `impact-` and its start's minute
(`impact-20261110T1404Z`); one whose key is taken gets `-2`, `-3` after it
(`freeKey` in `internal/ledger/record.go`), and a window found again keeps
its key only when it is the same one (`sameImpact`). A named signal's
windows are keyed by its name too (`impact-probe-20261110T1404Z`), so that
two signals' windows of one minute are never taken for one another, nor
one take the other's key. The unnamed signal keeps today's keys.

**Within a signal, as now.** What was decided about a window holds for a
window found later that covers half of it or more (`decidedFor`), and a
window on at a run's start goes on from the window found before (the
`UPDATE` in `ledger.impact`, which takes the latest impact window still
on). Both look only at windows of the same signal: as written, the second
would stretch a 5xx window with a probe window's end. So do `sameImpact`
and `remapImpact`, which move tasks and comments only between windows of
one signal.

**A signal renamed** with its expression and `for` unchanged is the same
criterion: its windows are renamed in one write (their signal, and their
keys, decisions, tasks and comments kept), and nothing is read again. A
name changed with either is a signal removed and another added.

## Judged by its own match

An incident gains `Match`, the labels it is matched on, nil for the match
in effect, and `Signal`, the signal it came from:

- an impact window carries its signal's;
- the incident a confirmation writes carries the window's (above);
- every other incident (written down, copied in, from the incidents file)
  carries none, and is matched on the match in effect, as now.

`verdict.Catches(f, in, o)` takes, in order: a rule entry for `f.Rule`,
when `in` has a signal (the entry's signals must hold it, or the firing
doesn't catch it; then the entry's `match`, if it gives one); else
`in.Match`; else `o.Match`. The ledger's `catches` builds each firing with
its labels and its start alone (`firing.Firing{Labels: labels, Start:
unix(start)}` in `internal/ledger/judge.go`), so it must fill `Rule` too,
from the row it reads; so must every caller that judges with an entry.
The backtests, the try and the digest's near misses judge with the same
function. Whether a written incident covers a window (`incident.Merge`, in
`cover`) is between two incidents, no rule's, and takes the window's
`Match`. A ledger with no named signal and no rule entry judges as before,
and `verdict.Semantics` isn't bumped. Changing a signal's `match`, or the
rule entries, gives the firings near the windows it changes their stones
again, from `--lead` before each window's start to its end, a week at a
time, as a change to `--match` does for every firing.

**Proposals.** A proposal's backtest keeps the criterion it was judged by
(`evidence.Impact`), and one waiting for approval is stale when it isn't
the one held now (`staleImpact`, checked in
`internal/serve/proposalsnow.go`).
That key becomes one digest over every signal's criterion, the rule
entries and the match in effect, so a change to any of them holds the
proposals it could change, as a new criterion does now.

## The record, signal by signal

v0.49.0 keeps, in `meta`, what the record by the signal is kept by:
`impact_since`, `impact_criterion`, `impact_again`, `impact_data` (where
the data of each selector begins, and the floor after the latest),
`impact_read` and `impact_floor`, and from them one start
(`ImpactKept.From`). Here each signal keeps its own six (below), and so
**its own start**: from when the runs read it with data, by v0.49.0's one
rule, signal by signal. A probe whose data begins on a Thursday starts the
probe's record the next week, and moves no other signal's.

A firing's week is kept by impact when **every signal that could have seen
the incident it would catch was alive** then: each signal tied to the
firing, among those that keep the record, had started by the week's start
(judged by week, as `serve` judges). Which signals are tied to a firing:

1. **A rule entry**: its signals, and no others. One with none in effect
   that keeps the record ties none that it waits for, and the firing has
   no record by reading (above); the windows of the entry's signals that
   don't keep the record still keep the record near them, as below.
2. **Else, by match, label by label.** A signal is tied when, for each
   label of its `match` the firing carries, the firing's value is one the
   signal has seen for that label (`seen`, below): one the series it reads
   have had, or one it returned. A label the firing hasn't ties it, as in
   `Agree`; so does a signal whose `match` is empty. A firing of `service:
   search` is tied to `errors`, which reads every service's requests,
   `search`'s among them, and not to `probe`, which probes only `checkout`
   and `cart`; a firing of `checkout` is tied to both, and waits for the
   probe's start.
3. **Else, nothing ties it** (its values are none any signal has had): it
   falls back to every signal that keeps the record, and waits for the
   latest start of all, as one expression's record does now. So it does
   when what 2 ties it to keeps no record: a signal matched on no label
   ties every firing, and one with `record: false` (payments) would
   otherwise leave a firing of a service no other signal has returned
   never judged by reading. The signals 2 tied stay tied, and their
   windows keep its record.

**Or an incident keeps the week**, as now, on in the week or the two before
it (`verdict.RecordLookback`):

- **a window keeps the record only of the firings tied to its own signal**:
  a 5xx window keeps no latency rule's week, so a latency rule with no
  latency signal stays "no record", not black, however many 5xx windows
  there were;
- **an incident written down, copied in, read from the incidents file or
  written by a confirmation keeps everyone's**, as now, and a team's by the
  incidents it owns, as now.

A signal with `record: false` is never waited for: reading it all week says
payments weren't hurt, not that nothing was. Its windows keep the record
of the firings tied to it, as any window does, and no more.

**What a signal has seen** (`seen`) comes from what it reads, not only
from what it returns (open question 6, decided below). A signal with a
threshold (`> 0.05`) returns something only while users are hurt: a
service it measured all month, never hurt, is a value it never returned,
yet it could have seen that service hurt. So, for each of its `match`
labels, `seen` holds the values of:

- **what it reads**: each selector its expression reads (`rules.Selectors`)
  counted by its `match` labels over the span the runs read it
  (`count by (service) (http_requests_total)`), one request a selector, as
  where its data begins is looked for (a step of an hour, or coarser over
  a long span, within `--max-samples` and the request caps), from where
  the last look got to, after each round's runs. A selector that only
  narrows another of the expression is counted through its stand-in
  (`rules.StandIns`: `http_requests_total{code!=""}` for
  `http_requests_total{code=~"5.."}`), as counting the 5xx would see only
  the services that had one, which is the problem itself;
- **what it returned**: the labels of each of its series that returned
  something, as each run reads it.

1,000 values a label and 256 KB a signal at most; past either, it is taken
to have seen every value of that label. Label by label, of what it reads:
the values its selectors' series have; a selector whose series lack the
label adds nothing for it (a gate, `and on() (sum(up{job="api"}) > 0)`);
and when none of its selectors' series carries the label, one the
expression makes (`label_replace`, a relabelled probe target), it is
taken to have every value of it, as it is of every label of an
expression that reads no selector (`vector(1)`) and of a name PromQL
can't count by. Of what it returned, a series that came without a label
takes it to have every value: a firing agrees with that series' windows
whatever its value (`Agree`), so the signal could have seen it. A look
that finds no series at all (its selectors with no data over the span)
learns nothing: a signal with no data saw nothing. A selector that fails
adds nothing, the others' counts kept, and the look is tried again from
the same place. A
firing so tied waits for that signal's start: never judged by a signal
that couldn't see it. A value seen later ties the firings before it too,
which then wait for that signal's start, and its record may wait longer
than it needs. Where each value's data begins, apart from the signal's,
is not looked for, as v0.49.0 looks per selector, not per series. What a
signal has seen is kept of its `match` labels in effect: one given anew,
or the match in effect changed for a signal that follows it, begins it
again, looked at from where its reads began, and so does a read again,
from the span it reads.

**Changing signals**, per signal:

- **added, or its expression or `for` changed:** read again alone over the
  last four weeks (v0.49.0's `impactAgain` and `ledger.ReadAgain`, by the
  signal's name), its windows in place of its own from then, decisions,
  keys, tasks and comments kept as v0.49.0 keeps them, and where its data
  begins looked for again. The other signals aren't read, and keep their
  starts.
- **removed:** turned off, as v0.49.0 turns off its one: its windows stay,
  as incidents, it no longer keeps the record, and no firing waits for it.
- **`match`, `record` or the rule entries changed:** nothing is read.
  `match` and the entries judge their windows' spans again (above); which
  firings are tied to which signals is worked out again, a week at a time
  (below).
- **A run that can't read one signal.** Today the impact query failing
  fails the run, and nothing is recorded. With several, one broken
  expression would stop the ledger. A signal that fails is missed instead,
  from the run's start, as a rule whose queries failed is (`Found.Missed`):
  the run records, later runs read the signal from where it was missed,
  for up to a week, and past a week it is as one v0.49.0 turns on again:
  its record starts with the run that reads it, and a read again is due.
  While it is missed, its start stands, so its weeks are kept before its
  windows are in, and judged again when it catches up, as a rule's late
  firings are. The Incident sources panel says it is failing, and why.

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
  `settings.impact_signals` and `settings.impact_rules` (below).
- **`/metrics`**: `lapilli_incident_state` and `lapilli_incident_lead_seconds`
  gain `[signal]`, on a named signal's windows (one incident has one
  signal, so no series changes as it goes); `lapilli_impact_windows`
  (`signal`, `state`), the last seven days' windows by signal and state;
  `lapilli_impact_signal_start_timestamp_seconds` (`signal`), from when
  each keeps the record. `lapilli_incidents` (`state`, `source`) stays as
  it is, all signals summed.
- **MCP**: `incidents` gives each window's `signal`; `rule` gives its entry,
  its signals and what its record waits for; `summary` gives `by_signal`.
- **`lapilli verdict`**: `--json` gives each incident's `signal`, and each
  firing's `no_record` as now, by its own ties; `--metrics` gives
  `lapilli_impact_windows`. It reads each signal over the period and the
  two weeks before, as it reads `--impact` now, and ties firings to
  signals as `serve` does, each signal's start where its data begins.
- **i18n**: every new text in the four languages, as every page's.

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
  judged; the record written incidents keep is everyone's, also in a
  team's row, as today.
- **`no_record`** means no firing of the row was judged: what it means today,
  where a week's firings are judged all or none, and so the same answer
  for a ledger with no named signal and no rule entry. **`unjudged`**, a
  field added, is how many of the row's firings were counted and not
  judged: with signals of their own starts and entries, part of a week can
  be judged and part not, as `verdict.SomeUnjudgedText` says of a report
  now.
- `settings.impact_signals` gives names, `for`, `match`, `record` and each
  start, and `settings.impact_rules` the entries; not the expressions,
  which are up to 4 KB each and no count of the summary's, and which the
  Settings page and `/settings.yaml` give.

`schema` stays 1: these are fields added, and `no_record` keeps its
meaning.

## What stays the same, and what changes

[numbers.md](../numbers.md) lists what keeps its name and its meaning. The
pull request that builds a step changes it with the code, in the same
pull request, as it says:

- **Two invariants change** and are written again there, under Changed in
  the changelog: "every firing of a week is judged, or none" becomes "by
  the signals tied to it", and "windows that overlap are one" becomes
  "windows of one signal that overlap are one".
- **Added to its tables**, and to `TestReportFieldsStable` and
  `TestMetricsStable`: the report's `incidents[].signal`† and
  `settings.impact_signals`† (each signal's name, `for`, `match`, `record`
  and start, beside `impact_from`, which stays the start of the unnamed
  signal); the metrics `lapilli_impact_windows` (`signal`, `state`) and
  `lapilli_impact_signal_start_timestamp_seconds` (`signal`), and
  `[signal]` on `lapilli_incident_state` and `lapilli_incident_lead_seconds`.
  Its recount blocks count `unjudged` by firing, as the report has it.
- **So that the report's numbers can still be counted again from it**
  (found building step 1): `settings.impact_rules`† (the entries, as the
  summary's settings give them), `rules[].impact_rule`† (the place of the
  entry that names the rule, from 1) and `firings[].signals`† (the signals
  tied to the firing). Without the entries, a catch by a rule an entry
  names can't be counted again; without the ties, which follow from what
  each signal returned over the read, which the report doesn't hold, nor
  can a firing's record.

## In the ledger

The next schema at build time adds:

- `impact_signals`: `name` (unique; empty for the unnamed signal), `expr`,
  `for`, `match` (a JSON list; NULL for the match in effect, `[]` for
  none), `record`, who set it in the UI and when (none for the command
  line's or the file's); v0.49.0's six (`since`, `criterion`, `again`,
  `data`, `read`, `floor`), per signal; and `seen`.
- `impact_rules`: the rule entries in effect, in order: `rules`, `labels`,
  `signals`, `match`, and whether the UI set them.
- `incidents.signal` (empty for none) and `incidents.match` (NULL for the
  match in effect).
- `series.signals`: the signals each alert is tied to (a bit each, eight at
  most, and one for "falls back to all"), worked out from its labels, its
  rule's entry and what each signal has returned.
- `days` and `weeks` with `signals` in their key, so a rule's firings are
  counted apart by the signals they wait for.

**Rewriting `days` and `weeks`.** They are `PRIMARY KEY (day, rule_id)
WITHOUT ROWID` and `PRIMARY KEY (week, rule_id) WITHOUT ROWID`
(`schema/001.sql`), and SQLite can't change a key in place. The migration
makes each again keyed `(day, rule_id, signals)`, copies every row in with
the one signal set every alert has before signals are named, drops the
old and renames the new, in the schema's one transaction; Postgres drops
and adds the key. It reads and writes each row once: the days and weeks
`--keep` holds, by the rules that fired in each, which 400 rules over 84
days make some 34,000 rows of `days`. Every read sums over `signals` along
the same key, which still leads with the day or the week; most rules have
one set, so the tallies grow by the few rules whose alerts span services
that different signals see.

**What follows judged-ness.** Hours (`byHours`, asleep and off hours) read
the firings they already read with their alert's `signals`, and count as
black only the judged ones. Marked windows ([marked.md](marked.md), #90)
key their `marked_days` and `marked_weeks` by `signals` too, so precision
inside a kind is of the firings judged.

Upgrading moves v0.49.0's six from `meta`, and the UI's `impact` and
`impact-for` settings, into the unnamed signal's row; gives existing
windows no signal (they are the unnamed signal's, keyed as they are); and
ties every alert to that one signal, so the tallies say what they said.
Going back takes the ledger as it was before, so a deployment keeps a copy
of it (a snapshot of its disk, or of its database) before it upgrades, as
with every schema change.

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

- **Queries a run**: one range query a signal, eight at most, over the
  run's period and the hour before, as the impact query is now, read in
  turn within the time the impact query has (`--timeout`):
  - those read last time first, the quickest before first, each with its
    share of what is left once the one whose turn it is has what it took
    before; one that took more than its share before is left to a later
    round, rather than run out its share for nothing;
  - then those missed: those that didn't run out on their last turn
    before those that did, each group the one longest without a turn
    first (the one missed longest first, when no turn is known, as after
    a restart). The first has its turn, all that is left; the others what
    is still left.

  One missed is read from where it was missed a piece at a time, each over
  a run's period and the hour before at most, as much as a run asks: on
  its turn, as many pieces as the time left holds and as one range may
  hold together (`--max-samples`, its series and labels as one range's),
  the rest on its next, which goes on before where the last piece ended by
  the signal's `for` and a step, `--settle` at most, as runs overlap: a
  window begun there, not yet on for its `for`, is found whole. What is
  left at the end of a round goes to those left to a later round for what
  they took before, the one longest without a turn first. One that runs
  out of time on its turn has twice as much kept for its next, up to
  `--timeout`; one that runs out with all of it, or near it, doesn't fit,
  or hangs, and is asked again a round later, then two, four, up to a day
  of rounds, nothing kept for its turns from the others' shares until it
  waits that long, and as before once it answers. So any number that hang
  cost the others a round each, the one in which each is found not to fit,
  and then one in a day, in each process: how the turns went isn't kept
  across a restart, nor by another replica that leads; and one whose run
  read fits in `--timeout` alone is read on its turn, which comes round to
  each one missed, and catches up. One left unread is missed for that run.
  After a round's runs, what each reads, by its match labels: one request
  a selector it counts, none for a signal matched on no label; and where
  each signal's data begins: one request a selector, 32 a signal at most,
  until it is found, as v0.49.0 looks for its one's; each look in turn as
  the reads are.
- **Reading again**: only the signals added or changed, each one query over
  five weeks at most, within `--max-samples`; each counts as one of the
  twelve reads an admin may ask for in an hour.
- **Windows**: 1,000 a signal's read keeps at most, and 2,000 a signal's in
  a week: those kept before the read began, and the read's own from where
  it reads (the hour before, read to carry a window on, isn't counted
  again); past either the signal is missed for the run, and the sources
  panel says it finds too many (an expression by pod, not by service).
  Each window's labels are bounded as an incident's are (4 KB, as kept).
- **Judging**: per firing and incident, one entry looked up from a map by
  rule, made once a change, and one list of names; the incidents meeting
  a span are read once a judge, as now. Held by a counter of entries
  looked up, one a rule a judge.
- **Ties**: worked out per alert when its series is first kept, and again
  for the alerts a change touches (a value first seen, a `match`, the
  entries): each is a lookup of at most 8 signals × 16 labels in `seen`.
  The firings of those alerts are counted again, a week at a time, by the
  worker.
- **Pages** read the tallies as now, summed over `signals`: the rows they
  read grow by the extra signal sets, and no firing is read to say whether
  it was judged. `TestPagesScale` holds the rows each page reads and its
  heap peak, the least of three runs, as `TestAnswerManySeries` takes its
  (#99); `TestScale` holds tying again by the rows it writes.
- **`/metrics`**: `signal` takes eight values at most.

## Security

- **Who.** Signals and rule entries are set where `serve` starts, or by an
  admin on the Settings page, as v0.49.0 sets its one. An expression runs
  with Lapilli's access to Prometheus, so it can learn, a count at a time,
  whatever that access reads, as MCP's `compare` can ([serve.md](serve.md),
  "MCP"); that is why it is an admin's. An entry decides which windows a
  rule's firings are judged against, which can make a rule look better
  (kept from windows it misses) or take its record away: an admin's too,
  and each change noted in the activity log with its before and after.
- **What comes in.** A name of 32 bytes from a small alphabet, so it is safe
  in a key, a URL and a label value, and still escaped in each; label names
  checked as Prometheus's; the expression checked as v0.49.0 checks it;
  rule patterns checked as a team's are. The series' labels become
  windows' labels, 64 values a label, however many series a window
  merges (an expression not summed up, over a label a workload sets,
  returns as many as there are values): a label of more is left out, as
  one not all the series have, so that it constrains nothing and the
  window is caught by more firings, never by fewer (as built in 0.57.0;
  0.55.0 and 0.56.0 kept the first 64 in order, which a firing of another
  value didn't agree with). They are kept as an incident's are, and shown
  escaped; what a signal has returned is
  bounded as above.
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
- **Signals declared in `--config`** (above).

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
  incidents port, each with a prefix of its own (`PROBE-41`), which
  v0.49.0's sources panel shows apart. They would be written incidents:
  never to decide, judged by the match in effect for every rule, keeping
  everyone's record, never read again when the expression changes, and
  the job's to keep running.
- **Merging windows across signals with the union of their labels.** A
  firing would have to agree with labels of both signals, which no alert
  does (a probe's URL and a service's name); and which `for` and `match`
  would judge it is still not answered.
- **One start for all, the latest** (v0.49.0's rule, over the signals).
  Simple, and the youngest signal takes every rule's record away (above).
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

Decided on 2026-11-16, by the maintainer:

- **Named and unnamed signals mixed.** Refused in steps 1 and 2. In the UI,
  the first named signal saved renames the unnamed one `impact`, which
  keeps its windows and their keys.
- **`no_record` in a team's row.** It means no firing of that row was
  judged, the same as today with no named signal and no rule entry;
  `unjudged` is added. The record a written incident keeps for a team's
  row stays everyone's, as today. Settled before step 2.

Decided on 2026-11-17, by the maintainer:

- **`seen` from what a signal reads, not only what it returns** (open
  question 6, found in the review of step 1). For each selector of the
  signal, its series are counted by the signal's `match` labels over the
  read, one request a selector, the same way and within the same bounds as
  the search for where its data begins. A selector that only narrows
  another of the expression is counted through its stand-in. That is
  joined with what the expression returned. A `match` label none of the
  signal's selectors carries is taken to have every value, as past the
  1,000-value cap: a firing is then tied, and waits for that signal's
  start. A selector whose series lack the label, beside others that carry
  it, adds nothing for it. Built in step 2 ("The record, signal by
  signal").

Open:

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
5. **Expectations naming an impact window.** [expectations.md](expectations.md)
   holds a rule to catching an incident by its id. An impact window's key
   can change when its signal is read again; an expectation should name the
   incident its confirmation writes.

## Step 1, as built

`lapilli verdict` with named signals, and what building it settled:

- **Flags.** `--impact NAME=PROMQL` (8 at most); `--impact-for NAME=DUR`;
  `--impact-match NAME=LABEL,LABEL`, comma-separated, `NAME=` for none;
  `--impact-record NAME=yes|no`. Each given once a name. `--impact-file`
  takes the place of all of them but `--impact-for DUR`, the "for" of a
  signal the file gives none: with `--impact` or a flag by name, it is
  refused. The file holds `signals` and `rules` at its top, or under
  `impact:`, as the chart's values will. A rule entry names its rules
  (`rules`, `labels` or both) and 1 to 8 signals.
- **`lapilli serve`** refuses named signals, the flags by name and
  `--impact-file` until step 2 keeps them; two `--impact` without a name
  are refused by both commands.
- **Windows.** A named signal's ids are `impact-NAME-1`, `impact-NAME-2`,
  its title `user impact (NAME)`; the unnamed signal's are as they were.
- **Each signal's start.** Where it is first read, two weeks before the
  period, or, of a selector with no data there, where its data begins, by
  v0.49.0's rule (`ledger.FloorBy`); `lapilli verdict` judges each firing
  by its own time, so the start is that time itself, not the first whole
  week after it. A selector with no data at all: no start, the signal
  never keeps the record; but one that only narrows another selector of
  the expression begins where its stand-in's data does (the other's
  series with each label it narrows by: `http_requests_total{code!=""}`
  for `http_requests_total{code=~"5.."}`, `rules.StandIns`), as in
  `lapilli serve`.
- **A signal that can't be read** fails the run, as `--impact` does: "missed
  for the run" is serve's runs'. The bounds on windows are serve's runs'
  too; a read is held to `--max-samples`.
- **`--redact`** gives each signal's name an alias (`signal-01`), writes a
  rule entry's pattern as `[redacted]`, and aliases its labels' values.
- **`--metrics`** gives `lapilli_impact_windows` and
  `lapilli_impact_signal_start_timestamp_seconds`; the text says each
  signal's windows and its start.
- **Not in step 1**: the summary's `impacts.by_signal` and `unjudged`,
  `settings.impact_signals` and `settings.impact_rules` in
  `/api/v1/summary`, and `[signal]` on serve's metrics, which are
  serve's (step 2).

## Step 2, as built

`lapilli serve` with named signals, in two pull requests: the ledger, the
runs, judging, the summary, `/metrics` and MCP first; the pages after it.
What building the first settled:

- **Flags and the chart.** `lapilli serve` takes the flags and the file of
  step 1, and the chart's `impact` as a mapping is that file, mounted and
  given as `--impact-file`; a string is `--impact`, as before. Refused, as
  in step 1: named and unnamed signals together, two `--impact` with no
  name, a flag by name no `--impact` gives.
- **Schema 022.** `impact_signals`, a row a signal ('' for the unnamed
  one), holds what v0.49.0 kept in meta (moved there by the migration, so
  an unnamed signal upgrades with its start, criterion, data and floor as
  they were), its bit, what it has seen (`seen`, and `seen_to`, how far its
  selectors were read for it) and from when a run missed it (`missed_at`).
  `impact_rules` holds the entries; `incidents.signal` and
  `incidents.match_labels`, a window's signal and match; `series.signals`,
  each alert's ties; `days`, `weeks`, `marked_days` and `marked_weeks` are
  keyed by `signals` too, every row copied in with 0. Some columns are
  named apart from this note's words, where a word is SQL's (`for_ns`,
  `match_labels`, `keeps_record`, `last_read`, `floor_at`, `pattern`).
- **Kept as started.** The leader keeps the signals and entries it was
  started with each round (`ledger.SyncSignals`), writing only when they
  differ: a signal added gets the lowest bit no signal has and no alert
  or tally carries, or, with none such (eight, one replaced), the lowest
  no signal has, every alert tied again and every firing judged again in
  the same write; no two signals ever share one (a unique index). One
  taken away is turned off, its row gone and its windows kept, with its
  name; one whose match or record changed, or the entries, judges every
  firing again, as `--match` does. A signal renamed on the command line is
  one taken away and one added, read again; renaming in place is step 3's,
  in the UI. Only this keeps a named signal's row: a run, or a read
  again, of a signal it doesn't keep (keeping them failed, or a replica
  started with another set than the leader) reads it not, and keeps
  nothing of it.
- **The runs.** Each named signal is read on its own, in turn within
  `--timeout` (see "Cost"). A window a read finds begun before where it
  reads goes on from the one found before with its labels that it meets,
  to the later end of the two; with none such (begun too late to be found
  then), it is new. (The unnamed signal's goes on from the last found
  before, as ever.) One that fails, or finds more than 1,000
  windows in a read or 2,000 in its week, is missed: the run records the
  rest, the signal's row keeps where it was first missed, and a later run
  reads it from there, within a week; past a week it begins again, as one
  turned on again, a read again due. Prometheus turning a signal away (a
  429 yielded to) holds the run, as it does the unnamed one, which still
  fails the run as before.
- **Each signal's own.** Its windows are put in place of its own alone,
  keyed by its name; its decisions, keys, tasks and comments carried over
  among its own; its record kept by v0.49.0's rule on its own row; read
  again alone when its criterion changes or it is added, failing and tried
  again on its own; where its data begins looked for on its own.
- **`seen`** as decided (open question 6): what it returned, kept by each
  run; what it reads, looked at after each round's runs from `seen_to`, a
  request a selector (`source.SeenFrom`, which `lapilli verdict` now reads
  too), a narrowing selector through the selector its stand-in is made of
  when that one is counted itself. What the note left open: a look that
  finds no series at all learns nothing, so a signal with no data ties
  nothing by reading; a selector that fails adds nothing, and the look is
  tried again from the same place (`lapilli verdict` says so and goes
  on); the look begins at the signal's first read, not at its start, which
  moves forward with its data; and what it has seen begins again, every
  alert it tied tied again, when its expression is read again, its own
  match changes, or the match in effect does for one that follows it.
- **Ties.** An alert made in a write is tied in it, before its tallies are
  counted (`series.signals` -1 until then), by its labels and what each
  signal has seen. A rule's entry is applied when the tallies are read, by
  rule, and isn't in `series.signals`: an entry is a rule's, found by the
  rules' labels, which a write doesn't read, and a change to the entries
  then rewrites no alert. A value first seen ties again only the alerts
  with that value, or that fell back (`verdict.FellBackBit`), writing
  those whose ties change, and their firings are counted again in the same
  round.
- **Judging.** A catch reads the firing's rule, so an entry keeps it from
  other signals' windows; a window is covered by an incident written down
  on its own match; a confirmation carries its window's signal and match
  until its labels change, and keeps everyone's record, as an incident
  written down does. Pages, the summary, the report and `/metrics` judge
  each rule's firings of a week apart by the signals they are tied to
  (`verdict.Record.JudgesTied`, of the week); a replay judged in memory (a
  backtest, the digest) ties each firing by what each signal has seen, as
  the ledger ties its alert.
- **Shown.** The report gives `settings.impact_signals` with each start,
  `settings.impact_rules`, `rules[].impact_rule`, `firings[].signals` and
  `incidents[].signal`; the summary `unjudged`, `impacts.by_signal` and
  the settings; `/metrics` `[signal]` on a named signal's windows,
  `lapilli_impact_windows` and each signal's start; MCP each incident's
  `signal`, a rule's `impact_signals` (its entry, what it is judged
  against, what its record waits for and before when) and the summary's
  `by_signal`. A proposal waiting is held when any signal's criterion
  held, its match or record, the entries or the match in effect change.
  The Settings page lists the signals as started, with no form for one
  with no name beside them, until step 3.
- **The pages** (the second pull request). `/incidents` picks a signal's
  windows (`?source=impact&signal=NAME`, a tab each), says each window's
  signal, and the other signals' windows on with it (three named, the rest
  counted). A window's page says its signal, its `for` and `match`, the
  rule entries that keep their rules from it, and the other signals'
  windows at the time with what caught each; "Dismiss it and the other
  signals' windows at the time" dismisses the undecided ones with it, each
  noted on its own. An incident a confirmation wrote says it is matched as
  its window was, and its form that other labels make it a person's. A
  rule's page says its entry, the signals it is judged against with the
  labels each is matched on, and what its record waits for: "No record by
  reading them before 2026-11-09: it waits for errors, probe." The digest
  and reports name no window's signal yet.
- **Not built here.** The UI's `impact` and `impact-for` stay in
  `settings`, the unnamed signal's, until step 3 makes the section by
  signal. Hours (asleep, off hours) count black stones by firing as
  before, judged or not, as they did of weeks with no record.

## Step 3, as built

The Settings page by signal, with no schema change. What building it
settled:

- **Where the UI's are kept.** In `settings`, as the other judging
  settings are, and as v0.49.0 keeps the UI's `impact` and `impact-for`:
  a signal under `impact-signal/NAME` (its expression, `for`, `match`,
  `null` for the match in effect, and `record`, as JSON), or `off` for one
  the command line gives that an admin took away; the rule entries under
  `impact-rules`, a JSON list, `[]` for none. Who changed each and when are
  the setting's; back to the start deletes it. The command line's copy, for
  "back to the start" and the page's "Started with", is what serve was
  started with, which every replica has; nothing of it is kept in the
  ledger. Schema 022's `impact_signals.set_by` and `set_at`, and
  `impact_rules.ui`, are kept beside the signals in effect, from the
  settings, changing no judging; no page reads them.
- **The signals in effect** are the command line's, in their order, each
  the UI's in its place when the UI set one of its name, none when it says
  `off`; then the UI's own, the earliest set first. Eight at most: a
  change that adds a ninth, or gives one back to the start past eight, is
  refused; a start with more signals than before leaves the UI's newest
  aside, and the page says which. The leader keeps them each round
  (`ledger.SyncSignalSet`), as it kept the command line's.
- **One write a change, worked out in it.** A change is kept with the
  signals in effect after it in one write (`ledger.ChangeSignals`): the
  settings, a rename, the activity log, and the bits, ties and judging
  again of `SyncSignals`, so a leader's round never reads half of one.
  The signals in effect are worked out from the settings as that write
  reads them (`ledger.SignalsFrom`), the leader's each round too
  (`SyncSignalSet`): a set read before another write is never kept over
  it, which would undo a rename (its row, record and what it has seen) or
  let two admins together pass eight. A change is saved only on the
  settings it was worked out from: one saved meanwhile, by another admin
  or on another replica, refuses it (409), to be looked at again. Each
  change is noted with its before and after (`setting impact signal
  NAME`, `setting impact rule entries`, as the signal or the entries were
  and became).
- **Judged by options read after it.** A change that judges every firing
  again counts a generation of the signals in the ledger
  (`signals_gen`), which options are read with (`verdict.Options.
  SignalsGen`, before the settings): pended spans are judged only by
  options of the generation in effect (`JudgePending` leaves them, and the
  round after judges them), and a write that judges with options read
  before (a run under way) pends what it judged, to be judged again. A
  change saved on another replica, or mid-round, is so judged by options
  read after it, whatever process saved it and across restarts. Options
  are of one view: the signals they carry are read after their
  generation, kept by a replica only for that generation and those very
  settings, never by its own writes alone; and the ledger holds them, of
  the generation in effect too, to the signals and entries it keeps.
- **A match changed judges its windows** (since 0.55.0, a fix): a signal's
  windows keep the labels they are matched on (`incidents.match_labels`),
  set from its `match` when found; changed, its windows' are changed in
  the same write, as the firings near them are judged again. A
  confirmation's incident keeps the labels it was confirmed with. The
  signal with no name's windows follow the match in effect, kept as none,
  and are left so.
- **Saved, read first.** The form posts a signal's expression back
  whatever was changed: the one serve was started with isn't the UI's to
  check, and only one an admin changed is checked. A signal added, or
  given another expression or `for`, or given back to a start of another
  criterion, is read over the last four weeks before it is saved, as
  v0.49.0's criterion is, counted among the admin's twelve reads an hour;
  what can't be read within `--timeout` (90 seconds at most) and
  `--max-samples` isn't saved. Its match, `record` or the rule entries
  changed read nothing.
- **Renamed in place.** A name changed with the expression and `for` as
  they were: the signal's row keeps its bit, what keeps its record and what
  it has seen; its windows' keys become `impact-NEW-` and the minute
  (another `-2`, `-3` should one be taken), their tasks and comments with
  them, what was decided of them kept; a confirmation's incident carries
  the new name; the rule entries that name it name it so, as the UI's,
  noted; and what this process keeps of it by name (how its last read
  went, how a read again goes, its turns) goes with it. It is read from
  where the runs had got to, as one missed: a run under way, which read it
  by its old name, keeps nothing of it (no row of that name), and the next
  catches up. With either changed, it is taken away, its windows kept with
  its name, and another added, the entries naming it renamed alike.
- **What the page refuses**, before anything is read or written: a signal
  the rule entries name, taken away (saying which entries), so that no
  rule is left judged against none, nor `/settings.yaml` gives entries
  naming a signal it doesn't declare, which `--impact-file` refuses; the
  start's entries given back while they name a signal not in effect; a
  name the ledger keeps the windows of (a signal taken away), for a signal
  added (but one given back as serve was started with it) or renamed,
  whose read again would take their place; a name set here and left
  aside; a ninth signal. Rule entries are held as `--impact-file`'s
  are, and their labels as a team's (64 values of 256 bytes a label);
  their YAML is read as written, an anchor or an alias refused, so that
  32 KB of text is never kept as megabytes, and a tag other than a
  string's, a list's or a mapping's (`!!binary`, bytes, not text); their
  labels, as a team's and a quiet window's now, are valid UTF-8 with no
  control character; kept, 128 KB at most. Saving them, or renaming a
  signal in place (which renames its windows, judges every firing again,
  and notes the entries naming it before and after), counts among the
  admin's twelve reads and saves an hour: what the activity log keeps of
  them is 3 MB an hour an admin at most, pruned with `--keep`. The
  entries the ledger keeps are compared with the start's as it keeps
  them (`ledger.SameEntries`), so that none a round keeps is written
  again, and every firing judged again, each round.
- **The first named signal in effect** (one added, or one serve was
  started with given back) beside an impact signal with no name in effect
  makes that one the signal `impact` (`impact-2`, `-3` …, when the ledger
  keeps windows of an `impact` or one is in effect), its row given the
  lowest bit free, what keeps its record and its windows' keys as they
  were: the runs and a read again know a key with no name in it as one of
  its windows (`sameImpact`). It is read from where the runs had got to,
  as one missed; a run, or a read again, of the one with no name under
  way then keeps nothing, beside named signals. The one added can't be
  named `impact` then. **The last named signal taken away**, the one with
  no name in effect again with its criterion, gives it back its row and
  windows, its record standing: nothing read again, no week lost, no
  window twice. The windows found while it was named are keyed as the one
  with no name keys them, their tasks and comments with them; a
  confirmation's incident keeps the labels it was confirmed with. A run
  of named signals under way then, which read it by name, is the one with
  no name's read, not a run that didn't read it; one it had missed, behind
  the runs, keeps no record from where it was missed until it is read
  again. Of another criterion, the one with no name is read again, as one
  turned on.
- **Named signals given on the command line** in place of `--impact
  PROMQL` (a fix, decided on 2026-11-19; 0.55.0 to 0.61.1 left the
  windows with no name, incidents with no signal that kept every rule's
  record, a rule's an entry ties only to a signal with `record: false`
  among them, which `lapilli verdict` with the same file leaves unjudged).
  The leader's round that keeps the signals in effect
  (`ledger.SyncSignalSet`) ties the windows of the one with no name the
  ledger keeps to the name `impact`, in the same write, as the Settings
  page names it, once the signals no longer given are taken away, so that
  their bits are free (eight kept, `impact` given in place of one, takes
  its bit). With `impact` in effect and no row or window of that
  name kept, it is the page's rename (`renameSignal` from ""): the row,
  its bit the lowest free, what keeps its record and the windows' keys
  kept, so that a signal `impact` of the criterion the one with no name
  was read with is read on, nothing read again; and, as no alert is tied
  to it yet, every alert is tied again and every firing judged again, as
  for a signal added. With `impact` kept already (a release before this
  one made its row, and read it again beside them), the windows alone
  take its name and its match, beside those it found; the row with no
  name stays, turned off. With no `impact` in effect, they are the
  windows of a signal no longer read, kept with its name as the page
  leaves the windows of a signal it took away; such windows are
  incidents, which keep every rule's record and which any rule may catch
  ("Step 2, as built"), so with no signal named `impact` in the file the
  rules an entry ties only to a signal with `record: false` are judged
  in their weeks still. Either way the firings that could catch them,
  from `--lead` before each to its end, are pended to be judged again,
  the activity log notes it (`lapilli`: `impact windows of the signal
  with no name tied to the signal impact`, with how many, which the Run
  history shows), and the log says it in one line (`windows of the
  impact signal with no name tied to a named signal`, with `windows`,
  `signal` and `read`). The write is the leader's, under the writers'
  turn, and reads the windows again in it: a round that finds none ties
  nothing and writes nothing, so it is done once, whatever replica leads
  next. Found none, the ledger notes it (`meta` `unnamed_tied`, in a write
  that changes no page), and the rounds after don't look again, as
  `incidents.signal` has no index; a round with no signal named forgets
  it, as the one with no name may be read again. A tie that fails (the
  database refusing the write) leaves the windows as they are: the
  signals are kept without it in a write of their own, so that options
  read after it are of the signals the ledger keeps and judge, the log
  says why (`the windows of the impact signal with no name weren't tied
  to a named signal`), and the next round, at its time, tries again;
  nothing is pended or woken for it. With no named signal, or no window
  of the one with no name, the round is as before. A signal added whose
  name the ledger keeps windows of (one taken away and given back, or
  `impact` after its windows were tied to it while it wasn't in effect)
  has them matched on its labels, as one whose match changed has.
- **Reading one again**, an admin's: the request reads it over the last
  four weeks, in the heavy turn, within `--timeout` (90 seconds at most)
  and `--max-samples`, counted among the twelve reads an hour, and puts
  its windows in place of its own (`ledger.ReadAgain`), noted in the
  activity log (`setting impact signal NAME read again`); the replica that
  leads judges the weeks it pended at its next round. The runs moving on
  meanwhile is said, to ask again. One that fails is said to the admin,
  and is no run's failure in the sources panel.
- **The try.** It reads each signal the change gives a new criterion (its
  windows, where the data of its selectors begins, what it returned and
  what it reads by its match labels), by the read the criterion with no
  name's try reads by (`readSpan`), and counts its windows by the same
  helper (`windowsTried`), which stops when the page's time is up; read
  in full, it stands for saving it within half an hour. It reads what a
  signal given a new match, or the one with no name made a named one,
  reads by its labels; the others' windows are the ledger's, each matched
  on its signal's labels with the change. Past the windows a read again
  keeps in a week (2,000), it says so, and shows none. Judged in memory, as a backtest is, it
  gives each signal's windows in effect and with the change, and from when
  each keeps the record with it; the verdict in effect and with all of
  them; and the proposals it would hold, by the digest the change would
  give. An admin's, from this site, in the heavy turn, and one of the
  twelve reads an admin may ask for in an hour, the entries' too, which
  read nothing. The weeks before the four keep no row of
  their own, as each signal's start says what they lose.
- **The sources panel** gives a row a signal in effect in place of the
  impact signal's: when it was last read, its windows in four weeks,
  failing with Prometheus's error (or missed by a run, when this replica
  hasn't the error), waiting to be read with the criterion in effect, not
  read for two runs, waiting to be read again, or with a selector with no
  data, each selector and where its data begins, and from when it keeps
  the record.
- **`/settings.yaml`**, once the Settings page changed a named signal or
  the entries, gives the signals in effect and the entries as the chart's
  `impact`, a mapping, to commit; once it took every named one away, the
  chart's `impact` string in effect.
- **Proposals** are held by the digest of step 2, now of the signals in
  effect: a change here holds the proposals it could change, as one on the
  command line does. A rename holds them too: the digest names the
  signals.
- **No named signal**: the form for the one with no name is as it was, and
  `/settings.yaml` as it was; an admin sees "Add a named impact signal"
  below it, which nothing reads until it is saved.

**Known.**

- The signal with no name is given back its own only when the named one
  made of it is the last taken away. Taken away while others stay, its row
  goes as any signal's; when the last named one goes, the one with no name
  is read again over the last four weeks as one turned on: it keeps the
  record from where that read begins (the runs' first, when later), the
  weeks before it kept only by incidents and windows, and the windows
  `impact` found in those weeks stay beside those read again, an outage
  two windows until one is dismissed.
- With `impact` kept already when the windows are tied (a release before
  this one made its row), or added after they were tied to it while it
  wasn't in effect, it keeps the record from its own start only, where
  its own read began: the row with no name, turned off by the first run
  of named signals, keeps no start to hand it. A firing tied to `impact`
  in a week before that start, with no window there, is not judged,
  where `lapilli verdict`, reading `impact` from two weeks before the
  period, judges it. Give `impact` in the file before the first start
  with named signals, so that it is made of the one with no name, its
  record kept.
- With `impact` kept already, the windows its read again over four weeks
  found when the release before added it stay beside the windows tied to
  it, with keys of their own (`impact-impact-` and the minute): one
  outage may show as two windows of `impact` until one is dismissed, as
  above.
- The windows tied to `impact` when serve was started with named signals
  aren't given back when it is started with `--impact PROMQL` again: they
  stay `impact`'s, a signal no longer read, and the one with no name may
  be read again over the last four weeks beside them, as above. Only the
  Settings page's last named signal taken away gives them back.
- A rename changes its windows' keys: a link to one under its old key, and
  a near miss the digest kept naming it, go nowhere until the next digest.
- What another replica keeps of a signal by name (its last read, a read
  again's tries, its turns) follows a rename made elsewhere at its next
  read of it.
- The signals in effect are worked out with the start of the process that
  writes: a replica started with other flags than the leader's writes its
  own, which the leader's next round keeps to its own again.
- Saving a signal and reading one again are POSTs, held by the
  cross-origin protection every form has, not to `Sec-Fetch-Site` as the
  try is, as the criterion with no name's save is. A match, or whether it
  keeps the record, changed reads nothing, and isn't counted, as `--match`
  on the same page isn't.
- The rule entries are written as YAML on the page, as `--impact-file`'s,
  not a form a field each.
- The try's bound on windows (2,000 a week) is held once the read is in
  memory, which `--max-samples` bounds, as a read again's is.

## Steps

1. `lapilli verdict`: an incident's own `match` and signal,
   `--impact NAME=PROMQL`, `--impact-for NAME=DUR`, `--impact-match`,
   `--impact-record`, `--impact-file` with rule entries, ties and each
   signal's start, each incident's `signal` in `--json`, and numbers.md's
   tables and tests. No schema change.
2. `serve`: the next schema; the runs read each signal, missed on its own;
   each signal's start; ties by alert, and the tallies by them; keys and
   decisions by signal; the Incidents page, a rule's page, the summary,
   `/metrics`, MCP; numbers.md's invariants. (Built, in two pull
   requests: "Step 2, as built".)
3. v0.49.0's section by signal: the list and the rule entries, the try,
   reading again a signal alone, a row a signal in the sources panel.
   (Built: "Step 3, as built".)
