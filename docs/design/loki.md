# Replaying rules written in LogQL

Status: built, for v0.28.0 (see "As built"), on v0.27.0. Under
[GOVERNANCE.md](../../GOVERNANCE.md) a change to how a firing is found needs
a design first; this adds rules a replay finds firings for, and a kind of
history it reads them against, and judges every firing as before. Nothing
new is kept in the ledger but where a document of rules came from. Written
down beside the build, as one pull request, as
[states.md](states.md) was, and waiting its week there.

What it says of Loki and Grafana was measured against Loki 3.7.9 and
Grafana 13.3.1 run in Docker, with Prometheus 3.5.0 keeping what they fired
(the answers quoted are their own), and is kept as test data.

## Why

Many teams alert on logs as well as metrics: errors counted in the last five
minutes, a latency pulled out of a log line. Two places run such rules:

- **Grafana alert rules on a Loki data source.** The same rules Lapilli
  replays when their queries are PromQL, with LogQL in their place.
- **Loki's ruler.** Prometheus's rule format, groups of `alert:`, `expr:`,
  `for:`, with LogQL in `expr`, evaluated by Loki itself. It sends to
  Alertmanager, and, with `remote_write`, writes `ALERTS` to a Prometheus,
  as Prometheus does (seen on 3.7.9).

Neither was replayed. How each was read in v0.26.0:

- A Grafana rule whose condition reads Loki was read as if its queries were
  PromQL, and asked of `--prometheus`. Prometheus refuses most LogQL
  (`|=` doesn't parse), and the rule was "query failed". Some LogQL is also
  PromQL: `count_over_time({app="shop"}[5m])` is a range selector to
  Prometheus, matching no series, so such a rule was replayed as never
  firing, wrongly and without a word. A rule whose condition reads
  Prometheus, with a Loki query beside it, is replayed in part, as states.md
  says (F6); that stays.
- A Loki ruler's rule file given to `--rules` reads as a Prometheus rule
  file, and its LogQL went to Prometheus the same way. There was no way to
  say a file was Loki's.
- SQL and the other data sources: as Loki.

ROADMAP.md asks for Loki and SQL. This does Loki and leaves SQL out
(below).

## What a LogQL rule is, to a replay

LogQL has two kinds of query. A log query (`{app="shop"} |= "error"`)
returns log lines; a metric query (`sum(count_over_time({app="shop"}
|= "error" [5m]))`, `rate`, `bytes_over_time`, `quantile_over_time` over an
`unwrap`, `absent_over_time`, `sum by`, comparisons) returns numbers, a
series a label set, exactly as PromQL does. Only metric queries alert: Loki's
ruler refuses a log query, and Grafana can't reduce one.

Loki answers `/loki/api/v1/query_range` in Prometheus's shape: a matrix,
each series its labels and `[time, "value"]` pairs, and an evaluation at
each step from `start` to `end`, both in. Measured on 3.7.9: a range query's
value at a step is the instant query's at that time, to the digit, for
`count_over_time`, `rate`, `max_over_time` over an `unwrap`, and a
comparison. So a LogQL metric query replays as a PromQL one does: asked at
each evaluation, its series are the alerts whose condition held. What a
replay does with that, `for`, `keep_firing_for`, Grafana's states, is
already the engine's, and unchanged.

What differs from Prometheus, all measured:

- Errors are plain text, not JSON (`parse error at line 1, col 28: syntax
  error: unexpected $end`, HTTP 400), and a log query answers
  `resultType: streams`.
- Grafana's data source proxy takes only GET for Loki (`non allow-listed
  POSTs not allowed on proxied loki datasource`, 403). Every request to Loki
  is a GET.
- Loki puts a range's points on whole multiples of its step since the Unix
  epoch, the first at or before `start` and the last at or after `end`
  (from 471 every 5s it answered 470 to 770); Prometheus puts them at
  `start` and every step after. A group Loki runs 11s into its minute is
  replayed at 11s past each minute: asked of Loki every 10s, its points
  would all fall on the minute's tens, and none on 11. So a range off its
  step's grid is asked at the greatest step dividing both the step and how
  far off the grid it starts (1s, there), and only its own points kept;
  a request stays within Loki's 11,000 points by covering less of the
  period.
- Loki adds labels of its own to what a rule returns (`service_name`,
  `detected_level`, from Loki 3), in its alerts as in the replay.
- There are no subqueries in LogQL. What Lapilli asks Prometheus as one
  (Grafana's reduces over a range query, and the question of whether a
  query had returned anything the day before) is worked out another way
  (below).

## 1. Loki's ruler

New flags, for `verdict` and `serve`:

- `--loki URL`: a Loki's API, which rules written in LogQL are replayed
  against: Loki, or anything serving its query API. `--loki-token-file`
  sends a bearer token, read again for each request; `--loki-tenant` sends
  `X-Scope-OrgID`, for a Loki with tenants and no gateway to set it.
- `--loki-rules PATH`: Loki ruler rule files, or a directory of them, as
  `--rules` takes Prometheus's; repeatable for `verdict`, one for `serve`,
  which reads it again every minute.
- `--loki-loaded-rules`: the rules the Loki at `--loki` holds, from its
  ruler's API.

Either goes beside any other source of rules, or alone, as `--alarms`
does: a team has Prometheus rules and Loki rules, and one verdict should
judge both. `--prometheus` stays needed for what reads metrics: Prometheus
rules, `--impact`, `--alerts`, and `--check-replay`, which reads what Loki's
ruler fired from where its `remote_write` sends `ALERTS`.

**Read as Loki holds them.** Loki's ruler has two APIs. Its
configuration API, `/loki/api/v1/rules` (YAML, a namespace to its groups,
Cortex's shape), says each rule as Loki holds it: its expression as
written, `for`, labels and annotations. Its Prometheus-shaped
`/prometheus/api/v1/rules` (JSON) says where each group runs
(`lastEvaluation`), with each expression written again by LogQL's printer.
`--loki-loaded-rules` reads the rules from the first, through the same
reader as `--ruler` reads Mimir and Cortex, a document a namespace; and
where each group runs from the second, matched by namespace (the `file` it
says) and group name.

**What Loki drops.** A rule file with a group's `query_offset` and a rule's
`keep_firing_for` loads in Loki 3.7.9 without a word, and neither API says
them again: Loki's own rule format hasn't them. Measured, Loki runs the
rule without them: what a rule with `keep_firing_for: 1m` and one in a
group with `query_offset: 30s` fired was alike, to the evaluation, to their
replay without them; with them, the first would have fired a minute longer
each time, and the second half a minute later. So a Loki ruler's rule is read without either,
from its API and from its files alike; should a Loki honour them, its API
would say them, and the files would be read wrong (Questions for review).

**Where a group runs** is Prometheus's hash of the group's name and the
path Loki loaded it from, which no one outside Loki knows; the rules API
says it as `lastEvaluation`, as for `--loaded-rules` (states.md, section 5).
It is a fact about the ruler now, not about the rules: kept beside them,
read again each time the rules are, and put on each Loki ruler rule when it
is replayed, by namespace and group (`Replaying.Places`). A group the API
doesn't name is replayed on the grid. Rules read from files are replayed on
the grid, as Prometheus rule files are.

**A rule knows it is LogQL.** `rules.Rule.Logs` says which Loki a rule's
queries are asked of: `loki` for `--loki`; for a Grafana rule, the uid of
its Loki data source (section 2); empty for PromQL. A document of rules
read from `--loki-rules` or `--loki-loaded-rules` says so too
(`rules.Doc.Logs`), and the ledger keeps it in the version's list of
documents, beside the namespace it already keeps there: a JSON field in a
column that is JSON, so no migration. A ledger read by an older `lapilli`
would read such rules as PromQL; a ledger isn't meant to go back a version.
A Loki ruler rule's key has `Logs` in it, so a rule moved from Prometheus to
Loki with the same text is a new rule.

## 2. Grafana's rules on Loki

**Which data sources are Loki.** A rule's queries name data sources by
uid. With `--grafana`, Lapilli reads each one the rules name, as it already
reads some for their scrape interval (`GET /api/datasources/uid/UID`, which
a Viewer's token reads), and keeps its type: `loki` is Loki. A data source
whose type is known to speak PromQL is replayed against `--prometheus` as
before: `prometheus` (Prometheus, Thanos, Mimir, Cortex and Google Cloud's
managed Prometheus), `grafana-amazonprometheus-datasource`,
`grafana-azureprometheus-datasource`, `victoriametrics-metrics-datasource`.
A rule whose condition reads any other type that Grafana names is listed:
"its condition reads a mysql data source: only PromQL and LogQL are
replayed". A data source that can't be read is taken as before, as PromQL.
Without `--grafana` (Grafana's rules from provisioning files), a query whose
model says `datasource.type: loki`, as Grafana's editor writes, is Loki; any
other is taken as PromQL.

**Where Loki is asked.** Through Grafana's data source proxy,
`GET GRAFANA/api/datasources/proxy/uid/UID/loki/api/v1/query_range`, with
the token that reads the rules. It is the Loki, the tenant and the
credentials Grafana itself asked when it evaluated the rule, whatever the
data source's URL and headers are, none of which the API shows (the URL is
as Grafana sees it, inside its own network; the tenant header's value is a
secret). Measured: a Viewer's token may ask it, by GET. Without
`--grafana`, a Grafana rule on Loki is asked of `--loki`, and is listed
without either.

**One kind a rule.** A rule whose condition reads both a Prometheus and a
Loki data source is listed ("its condition reads Prometheus and Loki
together"): each rule's history is one place. One whose condition reads
Prometheus, with a Loki query beside it, stays as states.md has it:
replayed, in part.

**Queries, as Grafana's Loki data source asks them**, measured through
`/api/v1/eval` and Loki's own log of what it was asked:

- `queryType: instant` (the model's, else the query's): Loki's instant
  query, at the end of the query's window (`relativeTimeRange.to` before
  the evaluation).
- `range` (Loki's default): from `from` before the evaluation to `to`
  before it, every step, both ends in. The step is the model's `step` when
  it has one (`15s`, or seconds alone, `7`); otherwise the query's
  `intervalMs` (1s when not set) times its `resolution` (1 when not set),
  and at least the window over 11,000, rounded up to the millisecond:
  1s for 10 minutes, 1.964s for six hours. `maxDataPoints` doesn't come
  into it (100 asked 1s points of ten minutes). The points fall on whole
  multiples of the step since the Unix epoch (`…866172` for 1.964s).
- Macros: `$__auto` is the step for a range query and the window for an
  instant one (`1s`, `300s`); `$__interval` is the step, `$__interval_ms`
  in milliseconds; `$__range`, `$__range_s` and `$__range_ms` the window
  (`300s`, `300`, `300000`). Loki's data source has no `$__rate_interval`.
- A log query is listed: "query A is a log query, which returns log lines,
  not numbers".

**Reduces over a range query.** Grafana reduces the window's points to one
number for each series. Over PromQL, Lapilli asks Prometheus for that number
as a subquery. LogQL has no subqueries, so the replay asks Loki for the
query's points at Grafana's step over the stretch it replays, the window
before it included, and reduces each evaluation's window itself, with
Grafana's reducers (`last`, `mean`, `min`, `max`, `sum`, `count`), on the
same points Grafana reduced. A series with no point in a window has no
value there, as with Grafana. At a 1s step a day is 86,400 points a series,
asked 1,440 at a time; a window over three hours is asked at its own step,
as Grafana does. Where the replay's step isn't a whole number of Grafana's,
each evaluation's window is asked for alone.

**Classic conditions over Loki** are listed: their reducers are worked out
by Prometheus's subqueries (states.md, section 2), and doing them in Go too
would be a second copy of all eleven. Grafana's editor has written reduce
and threshold expressions since Grafana 9; a classic condition is left from
before, and over Loki rarer still.

**No data or no history** (states.md, section 1): a query is taken to have
returned nothing only once it has returned something, in the replay or in
the day before. For PromQL the day before is one subquery. For LogQL it is
the query over the day, at the rule's interval, a minute at most, looking
for any point: 1,441 a series, once for each set of rules replayed
together, and kept, as for PromQL.

## SQL and the rest stay out

A SQL data source's query is a statement against a database Lapilli
doesn't read, whose answer changes as rows are written, updated and
deleted: there is no history of what it returned at 10:00 to replay. A
replay would need the database as it stood then, or every answer kept.
Elasticsearch, InfluxDB, CloudWatch Logs Insights, Azure's KQL and the
rest each have a language and an API of their own; each would be a piece
as large as this, and none is asked for. They are listed, by the type
Grafana gives, so a rule that isn't replayed says why.

## Not done, and why

- **A Grafana rule reading Prometheus with a Loki query beside it** could
  have the Loki query asked, to know when it returned nothing: it would be
  replayed whole, not in part. It needs a rule's history to be in two
  places at once; left for after this.
- **Sub-second phases.** Loki's ruler, like Prometheus's, runs a group at a
  fraction of a second past a second; the replay evaluates on the second
  (states.md, section 5). Prometheus's samples are scraped seconds apart,
  so this rarely matters; a log line can come at any time, and one written
  in that fraction of a second is counted one evaluation later by the
  replay than by Loki. Measured below, with log lines written so that they
  don't fall in it, and said in Questions for review.
- **A firing on when a period starts** that ends before its group's first
  evaluation in the period: Loki decided it at the evaluation before, which
  the replay doesn't make, for Loki's groups as for Prometheus's read with
  `--loaded-rules`. `test/e2e/loki.sh` first started its check on any
  minute and found one such firing in three runs; it now starts on a minute
  no firing ends near. Replaying a placed group from its last evaluation
  before the period would see it, but asks the history for an interval
  more, which the recordings kept as test data don't hold: left for after
  this.
- **Recording rules** in Loki's ruler, as in Prometheus's, aren't alerts,
  and aren't read.
- **Changing Loki's rules** (changes.md). Loki's configuration API is
  Cortex's, which the ruler path writes; a proposal for a Loki rule is for
  a person today.

## Builder's decisions

Taken while building; each is open to the review.

- **Grafana's proxy, not the data source's URL.** The URL a data source
  shows is the one Grafana reaches, and its tenant header and credentials
  are secrets the API doesn't give: asking Loki directly would need them
  all again, per data source, and would ask a Loki other than the one
  Grafana did when one was wrong. The cost is that Grafana carries the
  replay's queries.
- **Loki rules beside the others**, as `--alarms`, not one source more to
  pick one of.
- **Where a Loki group runs is read, not stored**, with the rules each
  time; it is a fact of the ruler, as a Grafana data source's scrape
  interval is.
- **Data source types that aren't PromQL or LogQL are listed**, by a list
  of the types known to speak PromQL. A data source of a PromQL type not on
  the list says so, rather than being replayed as before: such a type
  should be added.
- **Reduces in Go only for Loki.** PromQL's stay subqueries, which
  Prometheus works out near its data.
- **Every request to Loki is a GET**, for the proxy, which a long query could
  take past a URL's length; none of those measured came near.

## Questions for review

- Should a replay evaluate Loki's ruler at the fraction of a second it
  runs at? The rules API says it to the nanosecond; Loki's range API takes
  `start` in fractions of a second. It would change the steps every
  replay is evaluated on, and how answers are shared between rules.
- Should a Loki ruler's rule file be read with the `keep_firing_for` and
  `query_offset` Loki 3.7.9 drops? A later Loki may honour them.
- Should a Grafana rule on Loki be asked of `--loki` rather than through
  Grafana when both are given? Today `--loki` is for Loki's ruler, and for
  Grafana's rules only when they are read from files.
- Is the list of types that speak PromQL right? A plugin that does and
  isn't on it is listed as not replayed.
- Loki's `max_query_series` (500 by default) refuses a query over more
  series; such a rule fails in Loki's ruler as in the replay. Should the
  replay say so apart?

## As built

As above. Measured against Loki 3.7.9, Grafana 13.3.1 and Prometheus 3.5.0
run in Docker for an hour, logs written every second, each stamped at .95
of its second and sent before it (so none was late, and none fell in the
fraction of a second a Loki group runs past its second): the shop's errors,
two a second for 40s of every three minutes; the cart's, one a second for
the first minute of every five; an api's latency, 900 for 20s of every two
minutes; and a stream there for two minutes of every ten. Nine Loki ruler
rules in four groups, at 10s, 20s, 30s and 1m, placed 2s to 14s into their
intervals: `count_over_time`, `rate`, `bytes_over_time`, `max_over_time`
and `quantile_over_time` over an `unwrap`, `absent_over_time`, `sum by`, a
`for`, and the `keep_firing_for` and `query_offset` Loki drops. Seventeen
Grafana rules on Loki: instant queries with NoData, OK and KeepLast; range
queries reduced by `last`, `max`, `mean`, `sum`, `count` and `min`, at 1s,
3s and a query's own 10s with `$__auto`; a math expression over two
queries; a `for`; series by level that come and go; and a classic
condition, a condition on Prometheus and Loki together and one on MySQL,
which are listed.

`lapilli verdict --check-replay` over forty minutes, 19:35 to 20:15:

| | v0.26.0 | as built |
|---|---|---|
| Loki's ruler (v0.26.0: its files, as Prometheus rules) | 23% (13 alike, 4 only replayed, 39 only real; 5 rules not replayed) | 100% (88 of 88) |
| Grafana on Loki | 20% (31 alike, 4 only replayed, 122 only real; 4 rules not replayed) | 100% (166 of 166) |

Each firing starts and ends at the same evaluation as Loki's or Grafana's,
but for one on when the period starts, which the replay has from its
group's first evaluation in it (above). The recording is kept as test data
(`internal/replay/testdata/loki-3.7.9`, `grafana-13.3.1-loki`) and replayed
by `TestRealLoki` and `TestRealLokiGrafana` on every change.
`test/e2e/loki.sh` runs a smaller set against real servers in CI: its
first runs found that a SQL rule said "no PromQL expr", as a rule that
couldn't be read never had its data sources' types asked (fixed: it is read
again once they are), and the firing on at a period's start (its check now
starts on a minute no firing ends near).

## Running the measurement again

Loki with its ruler on, rules from a directory, and `remote_write` to a
Prometheus run with `--web.enable-remote-write-receiver`; Grafana with
its state history written to the same Prometheus, as states.md says, and
data sources `loki` (Loki), `prom` (Prometheus) and `orders` (MySQL, which
needn't be there); a Viewer's service account token. `test/e2e/loki.sh`
sets all of it up, and writes the logs; the recording's rules are in its
testdata (`rules.yaml`: Loki's configuration API's answer, and Grafana's
export). After an hour:

```
LAPILLI_RECORD=http://127.0.0.1:9090 LAPILLI_RECORD_LOKI=http://127.0.0.1:3100 \
LAPILLI_RECORD_GRAFANA=http://127.0.0.1:3000 LAPILLI_RECORD_GRAFANA_TOKEN=FILE \
go test ./internal/replay -run TestRealLoki
```

with the periods in `real_loki_test.go` set to what was recorded.
