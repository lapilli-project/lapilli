# Replaying what happens around a rule's condition

Status: built, on the branch for v0.25.0. Under
[GOVERNANCE.md](../../GOVERNANCE.md) a change to how a firing is judged
needs a design first; this changes which firings a replay finds, not how
one is judged, and nothing in the ledger, so it is written down beside the
build, as one pull request, and waits its week there. The companion to
[fidelity.md](fidelity.md), which sets a replay beside what really fired:
this is what that comparison found wrong, and what was done about it.

## Why

A replay reads a rule's condition against history. What fires is more than
the condition:

- **Grafana when a query returns nothing.** Its rule then goes to the state
  its `noDataState` names: `NoData` (the default), `Alerting`, `OK`, or
  `KeepLast`. With `NoData`, Grafana sends an alert of its own,
  `DatasourceNoData`. A rule written as a filtering query (`errors > 5`)
  returns nothing whenever all is well: left at the default, it pages
  whenever all is well. The replay saw none of it.
- **Grafana when a query fails:** `execErrState`, `Error` (an alert of its
  own, `DatasourceError`), `Alerting`, `OK` or `KeepLast`.
- **Grafana when one series of several goes:** the alert stays as it was for
  `missing_series_evals_to_resolve` evaluations, two by default, and then
  resolves. The replay resolved it at once.
- **Classic conditions** were written as PromQL, which has nothing like
  `diff`, `percent_diff` or the `no_value` evaluator: such rules weren't
  replayed. Those that were lost Grafana's no data, its null and NaN
  handling, and `logic-or`.
- **Metrics that arrive late.** A rule evaluated at 10:00 sees what had
  arrived by 10:00; a replay a day later sees what arrived since, too.
  Prometheus has `query_offset` for this; Grafana a query window that ends
  before now (`relativeTimeRange.to`). The replay read neither.
- **Where Prometheus places a group in its interval.** A group with a one
  minute interval is evaluated at the same second of every minute, set by
  a hash of its name and file. The replay evaluated on the minute.

And near misses by value, which say "with 0.87 in place of 0.9 it would
have caught INC-12", read only a single comparison. A Grafana math
expression joining comparisons with `&&` or `||`, and classic conditions,
were read as a whole and had none.

## How it was measured

Against a real Grafana 13.3.1 and Prometheus 3.5.0, run in Docker for an
hour, Grafana writing its state history to Prometheus as `GRAFANA_ALERTS`.
Every query is a function of `time()` alone, such as
`vector(1) and on() (vector(time() % 300) < 150)`: what the replay reads is
exactly what Grafana and Prometheus read when they evaluated it. Twenty-one
Grafana rules: each `noDataState` with no `for` and with `30s`, a series
that goes missing, a classic condition with each reducer PromQL has no
function for, `count_non_null`, `avg` over NaN, `median` and `count`
joined by `or`, a window ending two minutes before now, a classic condition
going no data, a math `&&`, and a query that always fails. Six Prometheus
groups: the same 40-second window every three minutes, at a one-minute
interval, in five groups placed differently, and one group with
`query_offset: 1m`.

`lapilli verdict --check-replay` over the 44 minutes recorded (57 for
Prometheus), before and after:

| | v0.22.2 | this |
|---|---|---|
| Grafana: firings alike | 56% (72 alike, 15 only replayed, 41 only real) | 100% (131 of 131) |
| Grafana: rules that couldn't be replayed | 7 | 1 (the failing query) |
| Prometheus, rules from its API: alike | 82% (19 replayed that never fired) | 100% (85 of 85) |

What was recorded is kept as test data
(`internal/replay/testdata`), with a test that replays it on every change
and wants each firing the same, starting and ending at the same
evaluation (`TestRealGrafana`, `TestRealPrometheus`). How to run it again is
at the end.

## 1. No data and errors (Grafana)

Read from Grafana's source (`pkg/services/ngalert/eval`, `.../state` in
13.3) and checked against it running.

**When an evaluation is NoData.** When any one of the rule's data source
queries returns nothing, whether the condition reads it or not: Grafana
runs every query. Then the result is one NoData result for each
data source, labelled `datasource_uid` and `ref_id` (the refIds that
returned nothing, joined by commas). Also when the condition comes to
nothing (a math join that pairs nothing) and when classic conditions say
no data; those have no labels.

**What it becomes**, by `noDataState`:

- `NoData`: an alert of its own with those labels, pending for the rule's
  `for` and then in the NoData state, which fires. The rule's other alerts
  are missing from the evaluation, and resolve as missing ones do. Grafana
  writes it to `GRAFANA_ALERTS` with `grafana_alertstate="nodata"`, and the
  replay gives its firing that label too, so the two can be told apart from
  the rule's alerts and set beside each other.
- `Alerting`: every alert Grafana has for the rule (those of the last
  evaluations, firing or not) is held as alerting, pending first for those
  that weren't; with none, an alert with the NoData labels.
- `OK`: every alert the rule has resolves; with none, nothing.
- `KeepLast`: every alert stays as it was, a pending one still counting
  towards its `for`; with none, nothing.

**A series that goes** while others stay: its alert stays as it was until
it has been missing for `missing_series_evals_to_resolve` evaluations (2
by default), then resolves; one in the NoData state, or pending because of
no data, after one.

**A value that isn't a number** (NaN, from `$A / $B` with both 0) is
Alerting, as Grafana reads any value but 0. The replay passed it over.

**To know which alerts Grafana had** when a period starts, the replay works
the states out from before it: one evaluation longer than
`missing_series_evals_to_resolve` takes, or an hour for `Alerting`, `OK`
and `KeepLast`, which act on the alerts there are. A stretch of no data
longer than that, running into the period, is replayed as if Grafana had no
alert for the rule.

**Errors aren't replayed.** History can't say when a query failed: a
Prometheus that was down leaves the same gap as a series that wasn't
there. Rather than guess, the replay says so:

- A rule whose `execErrState` is `Error` or `Alerting` is marked "replayed in
  part": in `lapilli verdict`'s output, its JSON (`unseen`), and on its page
  in `lapilli serve`.
- `--check-replay` and `serve`'s check leave Grafana's error alerts out of
  what really fired, `GRAFANA_ALERTS{grafana_alertstate!="error"}`: the
  replay can't be right or wrong about them. With `execErrState: Alerting`
  an error makes the rule's own alerts fire, which nothing tells apart, and
  those are counted as firings the replay missed.

Defaults are Grafana's provisioning defaults: `noDataState` `NoData`,
`execErrState` `Alerting`. Its exports always write both.

## 2. Classic conditions

Evaluated in Go, as Grafana's `ConditionsCmd` does, and no longer written
as PromQL. Each condition reduces every series its query returned and
compares; it holds when any series passes, and is no data when every series
reduces to none (or there is none). Conditions join in order, by their own
`and`, `or` or `logic-or`, which stops at the first that holds; no data
comes before firing. Evaluators `gt`, `lt`, `gte`, `lte`, `eq`, `ne`, the
four ranges (either way round, as Grafana takes them) and `no_value`.
`no_value` holds for a series that reduces to none; a query that returns
nothing at all has already made the evaluation NoData, as Grafana has it,
and as it was seen to do running.

Reducers are as Grafana defines them in `pkg/expr/classic/reduce.go`. What
the replay asks the server for, per condition:

| reducer | asked as | |
|---|---|---|
| `count` | `count_over_time((q)[W:R])` | every point, NaN too |
| `count_non_null`, `sum` | `…_over_time(((q) >= -Inf)[W:R])` | NaN left out (`>= -Inf` fails only NaN) |
| `avg`, `min`, `max`, `last`, `median` | `…_over_time(((q) >= -Inf)[…])` | |
| `diff`, `diff_abs`, `percent_diff`, `percent_diff_abs` | the newest number, `last_over_time`, and the number at the window's start, `last_over_time(((q) >= -Inf)[R/2:R] offset w)` | worked out in Go |

`R` is how far apart Grafana asks for the points, measured on Grafana
13.3 through its `/api/v1/eval`: the query's `interval`, else its
`intervalMs`, 1s when it has neither (the data source's scrape interval
doesn't come into it), or more for a long window (2s for six hours, 10s
for a day), as `maxDataPoints` and Prometheus's 11,000 points ask. Grafana
asks from the window's start to its end, both in; `W`, the window and half
a point, holds exactly those points whether the server's subqueries leave
out their start (Prometheus 3) or not (Prometheus 2).

A reduce whose result doesn't hang on how many points there are is asked at
`R` when the window holds no more than 600 of them (ten minutes at 1s), and
otherwise at the server's own resolution, to keep what the server works out
per step to a bound. The same now holds for a reduce folded over a range
query. At the server's resolution, a minute by default, a rule evaluated
every 10s read its newest value up to a minute old: that was most of what
differed, before.

For the diff reducers, a series with no point at the window's start (it
appeared within the window) has its first number asked for over the window
itself, at `R`, for that evaluation: Grafana takes the first in the window.

An instant query's value is compared as it is; Grafana passes the reducer
over.

What it costs: two or three subqueries for each condition, at Grafana's
resolution where it counts. Prometheus works each out for each step: 600
points a step at most, but for `count`, `count_non_null` and `sum`, which
need them all, as before.

## 3. Near misses by value

- **A math expression joining comparisons with `&&` and `||`**, in brackets
  or not: each comparison of an expression with a number is a number of the
  rule's (a `Bound`), named by what it compares (`$A`). A comparison under
  `!`, or inside arithmetic, isn't one: it decides the other way, or not
  alone. The value read for one is, at each evaluation, the value it
  compares where the condition turns on it, holding with it holding and not
  without: worked out by evaluating the condition twice, with that
  comparison said to hold and said not to, series by series. Where it holds
  either way, the value is as near as can be (±Inf); where neither, it
  isn't one. So the near miss says the number that would have caught the
  incident, how close the value came, and which comparison.
- **Classic conditions:** each `gt`, `gte`, `lt` or `lte` number, named by
  its reducer and query (`avg(A)`, or `A` for an instant query). Its value
  at each evaluation is the largest of the series' reduced values (the
  smallest, below), where the conditions' outcome turns on it, as for math.
  A range's two ends are numbers too, looked at by replaying the rule with
  nearer numbers, as PromQL's `or` operands are. `eq`, `ne` and `no_value`
  have no direction to come near from.

A proposed change to such a number backtests like any other; Grafana's
thresholds are still changed by a person, in Grafana.

## 4. Metrics that arrive late

What can be modelled is a fixed delay: each evaluation at `t` sees what had
arrived by `t`, the history up to `t − lag`. The replay asks each of the
rule's queries at `t − lag` and keeps its answer at `t`.

- **`query_offset`**, a Prometheus group's (Prometheus 2.53 and later):
  Prometheus does exactly this. Read from rule files and PrometheusRules.
  The rules API doesn't say it, so rules read from it are replayed without.
  Prometheus writes `ALERTS` at the query's time, not the evaluation's, so
  the check sets such a rule's firings that much earlier before matching.
- **A Grafana query's window ending before now** (`relativeTimeRange.to`):
  Grafana asks `[t − from, t − to]`. The replay read the window as ending at
  `t`. It reads it as Grafana does now.
- **`--late DUR`**, for `verdict` and `serve`, opt in: replay as if every
  metric arrived `DUR` after its time. `--late PATTERN=DUR` for the rules
  whose name, or the file or cloud they came from, matches; repeatable, the
  first that matches counting. The lag a query is asked at is the longest of
  `--late`, its own `query_offset` or window end: a rule that already reads
  far enough back sees everything.

What can't be modelled, and isn't claimed: a delay that varies. Prometheus
keeps a sample's time, not when it arrived, so history can't say that the
live rule saw a gap at 10:00 that was filled at 10:03, nor that an
out-of-order sample came in later. The replay of a lagged rule is exact for
a selector whose samples all arrived `DUR` late, while the lookback covers
`DUR`; a range function (`rate(x[5m])`) live saw its window cut short at
`t − DUR`, and the replay reads the whole window ending there.

**Rules on a cloud's own monitoring** (CloudWatch alarms and Google Cloud
policies read from their lists) are marked "replayed in part" unless
`--late` covers them: Google Cloud lists, for each of its metrics, how long
after sampling it can take to be visible (up to several minutes), and
CloudWatch documents data points that arrive after an alarm has evaluated.
Every cloud read alike.

## 5. Where Prometheus places a group

Prometheus evaluates a group at the same offset within each interval,
`hash(name, file) % interval`. A replay on the minute evaluates up to an
interval sooner or later; for a condition that holds for less than an
interval, whether it fires at all depends on the offset: in the
measurement above, of five groups with the same 40-second window, three
fired every three minutes and two never did; the replay on the minute fired
all five.

**Built, for rules read from an API that runs them** (`--loaded-rules`, and
`serve` with it): its rules API says when each group last ran, and the
offset is that time within the interval, to the second. Such a group, at an
interval a whole number of `--step`s, is evaluated at its own times. The
rules API of Prometheus, Thanos, Mimir and vmalert says `lastEvaluation`;
where it doesn't, the group is evaluated on the steps' grid, as before.

**Not for rule files, and no `--offset`.** The hash takes the path
Prometheus loaded the file from, which a file on Lapilli's disk doesn't
say (`/etc/prometheus/rules/prometheus-k8s-rulefiles-0/...` for the
Prometheus Operator), and the hash is Prometheus's internal hash of a
label set, which it doesn't promise to keep. A flag would ask a person for a
number per group that the rules API already gives. Thanos Ruler, Mimir,
vmalert and the managed services may place groups their own way;
`lastEvaluation` says where, whichever.

Grafana places a rule on its interval's own grid, by default, which the
replay already uses.

## Builder's decisions

Taken while building; each is open to the review.

- **The default `noDataState` is `NoData`**, as Grafana's. Rules read from
  a file that leaves it out, whose queries return nothing at times, now have
  NoData firings they didn't, which Grafana sent.
- **A NoData firing is the rule's**, with `grafana_alertstate="nodata"`.
  Grafana names its notification `DatasourceNoData`, but it comes from the
  rule, routed by the rule's labels, and wakes the same person.
- **Classic conditions all go through Go**, not only the new reducers: one
  way of reading them, with Grafana's no data and NaN.
- **600 of Grafana's points** is the most a reduce is asked for at its
  resolution when it needn't be. Ten minutes at 1s is Grafana's default
  window.
- **An hour** of states before a period, for `Alerting`, `OK` and
  `KeepLast`.
- **The phase to the second**, below: an evaluation starts just after its
  slot, and the range query API takes whole seconds.
- **Error alerts are left out of what really fired**, not counted apart:
  counting them apart would be a new column in the ledger.
- **`--late PATTERN`** matches name, file and cloud, rather than a flag per
  source: a source can hold rules that read late metrics and rules that
  don't.

## What it changes

- No ledger schema; no new command. Firings a run replays again come out as
  this replay has them; days not replayed again stay as they were.
- `serve`'s check reads `GRAFANA_ALERTS` without error states; its "series
  held nothing" note for the old selector is passed over.
- Rule keys don't change, but for a Prometheus rule with a `query_offset`,
  whose key now has it: such a rule is a new version once.

## Questions for review

- Should a rule from a file be placed where the history's own rules API
  says a group of the same name runs, when `serve` reads both? It would
  place most Prometheus rules, but a group of that name may be another.
- Should a Grafana rule whose `noDataState` is `NoData` and whose query
  filters (`x > 5`) be told, before anything is judged, that it pages
  whenever all is well? It is a fact about the rule, as the content check
  has others.
- Is an hour enough to find the alerts Grafana had? A rule that has had no
  data for longer looks as if it had no alert.
- `--late` takes one delay for a rule. Google Cloud says each metric's;
  reading them from its metric descriptors would be one cloud's feature
  unless the others say theirs.
- Error states left out of what really fired hide nothing the replay could
  have got right, but a rule that errs all day looks fine beside its
  replay. Should the check say how many it left out?

## Running the measurement again

Prometheus 3.5.0 with `--web.enable-remote-write-receiver`, and Grafana
13.3.1 with

```
GF_UNIFIED_ALERTING_STATE_HISTORY_ENABLED=true
GF_UNIFIED_ALERTING_STATE_HISTORY_BACKEND=prometheus
GF_UNIFIED_ALERTING_STATE_HISTORY_PROMETHEUS_TARGET_DATASOURCE_UID=prom
```

and its Prometheus data source provisioned with `uid: prom` and
`jsonData.prometheusType: Prometheus` (Grafana writes to `/api/v1/push`
otherwise, which Prometheus doesn't serve). The rules are in
`internal/replay/testdata`: Grafana's as `rules.json`, provisioned as they
are, and Prometheus's as `rules.yml` (the test reads them from its rules
API, `api.json`, as `--loaded-rules` does). After an hour:

```
LAPILLI_RECORD=http://127.0.0.1:9090 go test ./internal/replay -run TestReal
```

with the periods in `real_test.go` set to what was recorded.
