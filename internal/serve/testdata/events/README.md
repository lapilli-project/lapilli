# What a real Alertmanager's event recorder posted

Recorded on 2026-11-11 from `prom/alertmanager` 0.33.1, 0.34.2 and 0.35.0
in Docker, `--enable-feature=event-recorder`, with `alertmanager-0.35.0.yml`
(0.33 and 0.34 the same without the outputs' `name`, which they don't
take, and without `batch`). A webhook output posted each event to a small
server that kept every request's body and answered as the endpoints would:
PagerDuty's `202` with its JSON, Slack's `ok`, a webhook's `200`. Each line
of a `.jsonl` file is one request's body, in the order they came; with
`batch: true` a body is an array.

What was sent to Alertmanager, through `/api/v2/alerts` and
`/api/v2/silences`: a silence of `QueueStuck`; `DiskFull` on `db-1`
(routed to `oncall`: PagerDuty and Slack), `HighLatency` (`team=web`, to
`team-web`: a webhook) and `QueueStuck`; four seconds later `DiskFull` on
`db-2` too (new alerts in the group); after the repeat interval (65s), a
repeat; then `db-1` resolved, and then everything. Slack's integration
doesn't send resolves by default, so it has none.

| File | Version | Labels as |
|---|---|---|
| `alertmanager-0.33.1.jsonl` | 0.33.1 | `{"labels": [{"key", "value"}]}`, alerts by `hash` |
| `alertmanager-0.34.2.jsonl` | 0.34.2 | the same |
| `alertmanager-0.35.0.jsonl` | 0.35.0, the `events/v2` schema | a map, alerts by `fingerprint` |
| `alertmanager-0.35.0-batch.jsonl` | 0.35.0, `batch: true` | as above, arrays of events |

The routing key and token in `alertmanager-0.35.0.yml` are made up; the
`instance` in each event is the container's ID.
