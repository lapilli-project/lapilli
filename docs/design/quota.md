# Sharing the history with the alerts

Status: built, on v0.47.0. This changes no firing, no stone and nothing kept
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

Each serve process counts every request it sends to a history API, since it
started:

| Series | What |
|---|---|
| `lapilli_serve_history_requests_total{api, answer}` | requests sent, by the class of the answer: `2xx`, `3xx`, `4xx`, `5xx`, or `none` (refused, dropped, timed out) |
| `lapilli_serve_history_throttled_total{api}` | answers of HTTP 429 |
| `lapilli_serve_history_retries_total{api}` | requests asked again after an answer that said to (429, 5xx) or a connection dropped |
| `lapilli_serve_history_wait_seconds_total{api, reason}` | seconds requests waited before they were sent, summed over requests: `api`, a wait the API asked for and the waits before asking again; `rate`, a turn by `--max-rate` or `--catch-up-rate` |
| `lapilli_serve_history_yields_total{api}` | times the runs yielded to the API (below) |

`api` is one of `prometheus` (`--prometheus`, `--google-project`), `loki`
(`--loki`) and `grafana` (Grafana's data source proxy, which Grafana's rules
on Loki are asked through). The labels are this fixed set: never a URL, a
host, a tenant or a data source's name, which come from the user and would
make a series each. `prometheus` is there whenever serve reads a history,
as it replays only with `--prometheus`; `loki` with `--loki`, `grafana`
with `--grafana`.

They count the leader's work (runs, reviews of changes, the digest, the
check) and what people and agents ask for (a proposal's backtest, MCP's
compare); on replicas, each process its own, so sum them. Beside the
alerts' own errors:

```promql
sum by (api) (rate(lapilli_serve_history_requests_total[5m])) * 60   # requests a minute
sum by (api) (rate(lapilli_serve_history_throttled_total[5m])) * 60  # 429s a minute
```

`lapilli verdict --progress` already says a run's requests, retries and
429s on stderr; `verdict --metrics` doesn't add these, as a run's report is
of the rules, and a CronJob's run is one burst of requests a week.

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
  the next run.
- What reads the history after the runs in the same round waits too, for a
  round whose run isn't cut short: reviews of changes, the weekly digest and
  what follows it (near misses, the weekly proposals), proposals backtested
  again, approved changes carried out (they are checked against the history
  first), and the replay set beside what really fired. Each picks up where
  it was when it runs: the digest of a week missed is worked out then. What
  reads only the ledger (reports, notices, pruning) goes on.
- A 429 to the `--impact` or `--alerts` query holds the run (below), not
  fails it.
- Other APIs are asked as before: a Loki that answers 429 doesn't stop the
  rules on Prometheus, but the run still stops after its piece.

**No hour is given up for it.** A rule missed for longer than a week is
given up by the ledger (`missedFor`). So, with `--yield-on-429`:

- A run replays the rules that start earliest first, the rules missed
  first among them, so that a 429 cuts short the newest hours, not the
  oldest.
- The cursor goes no further than a week past the earliest a rule was left
  missed from by a yield (`ledger.MissedFor`). A run there replays the rules
  missed, up to the cursor, and goes no further, until they are replayed:
  an API that answers 429 for days holds the runs at a week behind, and
  once it answers, the week is replayed, then the hours since.

`--yield-for DUR` leaves the API alone for that long after the 429, a day
at most (default 0: the next run asks again). While `prometheus` is left
alone, a run is held (below), as it would ask nothing but fail.
`Retry-After` isn't read for this: a quota by the minute is over by the
next run. What was yielded to is the process's: a new leader, or a restart,
asks afresh.

Only 429 yields. A 5xx, or a connection dropped, is asked again as before:
it is as often a restart as an overload. The digest and the other work
above, when it runs, isn't asked with the yield: it asks as before, after
the Limiter (halved, and waiting out `Retry-After`) the run's 429 left.

## 3. A guard: `--guard EXPR`

A PromQL expression, asked of `--prometheus` at the time of each run before
it, and before each day of a catch-up. While it returns any series, or can't
be read within a minute, the run waits for the next `--every`, and what
reads the history after it waits too, as above. Examples, the rate of the
evaluator's own failures, scraped into the same Prometheus (check the names
your version exposes):

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

- **Unread, it holds** rather than lets through: a guard that can't be read
  is most often a history that can't take more. Why is logged at Warn; the
  run history keeps one fixed reason, so a changing error doesn't make a
  row each hour.
- **Refused as written** (HTTP 400 or 422, a typo), it fails the run, each
  time: red in the run history, counted as failed, logged as an error. A
  guard that would hold the runs for good must be seen.
- **Answered 429**, with `--yield-on-429`, it yields, as a run's request
  would, and the run is held.

## 4. Catching up in quiet hours: `--catch-up-hours`, `--catch-up-rate`

A run is a catch-up when it is more than a day behind where the ledger's
firings are up to: the first start's `--backfill`, or a run after being
down a day or more. A run back after a shorter outage, hours behind, runs at
once, at any hour, as does one replaying again from when a version of the
rules recorded since took effect: the ledger has the firings after it, and
the replay is of what it holds already.

- `--catch-up-hours HH:MM-HH:MM`, in `--tz`, such as `01:00-06:00`: a
  catch-up runs only within them, and is held outside. Between its days, the
  hours are looked at again: a catch-up the hours end in waits for the next
  night. The hours must be at least `--every` long, so that a run can fall
  in them; give them twice `--every` at least, as a run falls anywhere in
  its `--every`. Hours a change of clocks skips (02:00-03:00 on the night it
  springs forward, in many zones) don't come that day.
- `--catch-up-rate N`: while catching up, each API is asked N requests a
  second at most, besides `--max-rate`: for a quota with room by day that
  the alerts need by the minute. One rate for all the runs of the process.

Firings are recorded in order, behind one cursor: a run replays from where
the last one ended. So while a catch-up waits for its hours, nothing newer
is recorded either, and the hourly runs resume once it has caught up: that
is why only a day behind or more waits. Recording the latest hours ahead of
a catch-up would need a second cursor, and the digest and the replay's
check, which wait for the cursor, to wait for both; that is a change to the
ledger, for a design of its own. Give `--backfill` short enough to fit in a
night when that matters.

## Held runs

A run that waits, for `--catch-up-hours`, `--guard` or a 429 yielded to, is
kept in the run history as a run of kind `held`, with why in its error,
once while the last run kept is held for the same reason. It counts in
`lapilli_serve_runs_total{result="held"}`, and
`lapilli_serve_runs_held{reason}` is 1 for why the last run waited
(`catch_up_hours`, `guard` or `yield`), 0 for the others; each wait is
logged at Warn. A held run isn't a failed one: Outcomes' failed runs leave
it out, and the overview and the Run history say it in amber, not red. The
cursor stays: nothing is lost, only later.

## What the code does

- `internal/prom`: `Counts` keeps the same counts by kind of API (`Of`); a
  client says its kind (`CountedAs`, `grafana` for the data source proxy).
  `Yield`, given a request's context (`Yielding`), turns a 429 into a
  `*Yielded` error, and ends the waits for a turn of the run's requests to
  that API. `Pacing` adds a rate apart from the Limiter's. Without them in
  the context, a request is asked exactly as before.
- `internal/serve/quota.go`: whether a run waits (`waits`), keeping a held
  run (`hold`), the context a run asks with (`asking`), how far a piece may
  go with rules missed by a yield (`missedEnd`), the order rules are
  replayed in (`replayOrder`), and the metrics. `runs` looks before each
  piece and stops after one that yielded; `jobs` leaves what reads the
  history for a later round while the runs wait (`runsWait`).
