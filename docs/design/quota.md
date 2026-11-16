# Sharing the history with the alerts

Status: built, on v0.46.0. This changes no firing, no stone and nothing kept
in the ledger: no migration. It changes when `lapilli serve` asks the
history, and what it says of that. Every part is off by default; without the
flags below, serve asks as it did, and `--max-rate`, `--max-samples`,
`--timeout`, the heavy-request limit and the 429 retries are as they were.

## Why

Lapilli is most useful pointed at the history API the alerts themselves are
evaluated against: the same Prometheus, Mimir or managed Prometheus that a
ruler or Grafana asks every minute. Such an API often holds each identity to
a quota (a managed Prometheus answers HTTP 429 past it), and the alerts
share it with Lapilli when they read with the same identity, or when the
API's own capacity is the limit. Lapilli must never be why an alert wasn't
evaluated.

What serve did before:

- `--max-rate N` spaced its requests to each API, for a quota known
  beforehand.
- An answer of 429 halved how many requests went at once, and was asked
  again after the wait it said (`Retry-After`), or after waits of up to three
  minutes; then the rule was "query failed", missed, and replayed from where
  it was by a later run, for up to a week.
- A catch-up (the first start's `--backfill`, 28 days; a run after being
  down) replayed a day at a time, at once, at any hour.
- Nothing said how much serve asked of the API. An operator who saw the
  ruler fail couldn't set Lapilli's requests beside it.

## 1. Its own load, in `/metrics`

serve counts every request it sends to a history API, since it started:

| Series | What |
|---|---|
| `lapilli_serve_history_requests_total{api, answer}` | requests sent, by the class of the answer: `2xx`, `3xx`, `4xx`, `5xx`, or `none` (refused, dropped, timed out) |
| `lapilli_serve_history_throttled_total{api}` | answers of HTTP 429 |
| `lapilli_serve_history_retries_total{api}` | requests asked again after an answer that said to (429, 5xx) or a connection dropped |
| `lapilli_serve_history_wait_seconds_total{api, reason}` | seconds requests waited before they were sent, summed over requests: `api`, a wait the API asked for and the waits before asking again; `rate`, a turn by `--max-rate` or `--catch-up-rate` |
| `lapilli_serve_history_yields_total{api}` | times the runs yielded to the API (below) |

`api` is one of `prometheus` (`--prometheus`, `--gcp-project`), `loki`
(`--loki`) and `grafana` (Grafana's data source proxy, which Grafana's rules
on Loki are asked through). The labels are this fixed set: never a URL, a
host, a tenant or a data source's name, which come from the user and would
make a series each. The series of an API serve doesn't read aren't written.

They count the leader's work (runs, reviews of changes, the digest, the
check) and what people and agents ask for (a proposal's backtest, MCP's
compare); on replicas, each process its own. So, beside the alerts' own
errors:

```promql
sum by (api) (rate(lapilli_serve_history_requests_total[5m])) * 60   # requests a minute
sum by (api) (rate(lapilli_serve_history_throttled_total[5m])) * 60  # 429s a minute
```

`lapilli verdict --progress` already says a run's requests, retries and
429s on stderr; `verdict --metrics` doesn't add these, as a run's report is
of the rules, and a CronJob's run is one request burst a week.

## 2. Yielding on 429: `--yield-on-429`, `--yield-for DUR`

With `--yield-on-429`, the first answer of 429 to a run's request ends the
run's requests to that kind of API:

- The request isn't asked again. Requests of the run waiting for their turn
  to it give up at once; those not yet asked aren't; those already sent are
  let finish, as their quota is spent.
- Each fails as "query failed: the history couldn't be read: the prometheus
  API answered HTTP 429 ... lapilli yielded to it", the reason the run
  history shows for the rule. The rule keeps its firings and is missed, as
  a rule whose query failed always was: the next run replays it from where
  it was missed.
- The run is recorded, and stops: the days of a catch-up after it wait for
  the next run. Going on would move the cursor past the rules missed, and a
  rule missed for more than a week is given up.
- What follows a run and reads the same history, setting the replay beside
  what really fired, waits too.
- Other APIs are asked as before: a Loki that answers 429 doesn't stop the
  rules on Prometheus, but the run still stops after its piece.

`--yield-for DUR` leaves the API alone for that long after the 429 (default
0: the next run asks again). While `prometheus` is left alone, a run is held
(below), as it would ask nothing but fail. `Retry-After` isn't read for
this: a quota by the minute is over by the next run.

Only 429 yields. A 5xx, or a connection dropped, is asked again as before:
it is as often a restart as an overload. The digest, reviews of changes and
backtests aren't run with the yield; they ask as before, after the Limiter
(halved, and waiting out `Retry-After`) the run's 429 left.

## 3. A guard: `--guard EXPR`

A PromQL expression, asked of `--prometheus` at the time of each run before
it, and before each day of a catch-up. While it returns any series, or can't
be read within a minute, the run waits for the next `--every`. Examples, the
rate of the evaluator's own failures, scraped into the same Prometheus
(check the names your version exposes):

```promql
# Prometheus
rate(prometheus_rule_evaluation_failures_total[5m]) > 0
# Grafana's alerting
rate(grafana_alerting_rule_evaluation_failures_total[5m]) > 0
# A Mimir or Cortex ruler
sum(rate(cortex_prometheus_rule_evaluation_failures_total[5m])) > 0
```

A guard can be any condition: the API's own 429s from its gateway, a
query-frontend's queue length, an hour of the week.

An unreadable guard holds rather than lets through: a guard that can't be
read is most often a history that can't take more.

## 4. Catching up in quiet hours: `--catch-up-hours`, `--catch-up-rate`

A run is a catch-up when it is more than twice `--every` and `--settle`
behind (3h by default), as the overview's "catching up" says: the first
start's `--backfill`, a run after being down, or one replaying again from
when a version of the rules recorded since took effect.

- `--catch-up-hours HH:MM-HH:MM`, in `--tz`, such as `01:00-06:00`: a
  catch-up runs only within them, and is held outside. Between its days, the
  hours are looked at again: a catch-up the hours end in waits for the next
  night. The hours must be at least `--every` long, so that a run falls in
  them. A run that isn't behind runs at any hour.
- `--catch-up-rate N`: while catching up, each API is asked N requests a
  second at most, besides `--max-rate`: for a quota with room by day that
  the alerts need by the minute.

Firings are recorded in order, behind one cursor: a run replays from where
the last one ended. So while a catch-up waits for its hours, nothing newer
is recorded either, and the hourly runs resume once it has caught up.
Recording the latest hours ahead of a catch-up would need a second cursor,
and the digest and the replay's check, which wait for the cursor, to wait
for both; that is a change to the ledger, for a design of its own. Give
`--backfill` short enough to fit in a night when that matters.

## Held runs

A run that waits, for `--catch-up-hours`, `--guard` or `--yield-for`, is
kept in the run history as a run of kind `held`, with why in its error,
once while the last run kept is held for the same reason; it counts in
`lapilli_serve_runs_total{result="held"}`. A held run isn't a failed one:
Outcomes' failed runs leave it out, and the overview and the Run history
say it in amber, not red. The cursor stays: nothing is lost, only later.

## What the code does

- `internal/prom`: `Counts` keeps the same counts by kind of API (`Of`); a
  client says its kind (`CountedAs`, `grafana` for the data source proxy).
  `Yield`, given a request's context (`Yielding`), turns a 429 into a
  `*Yielded` error, and ends the waits for a turn of the run's requests to
  that API. `Pacing` adds a rate apart from the Limiter's. Without them in
  the context, a request is asked exactly as before.
- `internal/serve/quota.go`: whether a run waits (`waits`), keeping a held
  run (`hold`), the context a run asks with (`asking`), and the metrics.
  `runs` looks before each piece and stops after one that yielded.
