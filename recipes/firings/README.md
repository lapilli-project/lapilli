# Firings from alarms Lapilli doesn't replay

Alarms kept in a cloud's own monitoring are mostly written in that cloud's
terms, against metrics no PromQL API serves, so Lapilli can't replay them;
Datadog's monitors and New Relic's alert conditions aren't read as rules at
all. What they fired, it can judge. Each script here reads one tool's alarm
history and writes it to stdout in the form `lapilli verdict --firings`
takes, one firing a line:

```json
{"rule":"checkout-5xx","labels":{"service":"checkout"},"start":"2026-11-02T03:10:00Z","end":"2026-11-02T03:40:00Z"}
```

| Script | Alarms | History kept | Needs |
|---|---|---|---|
| `cloudwatch.sh` | Amazon CloudWatch alarms, metric and composite | 30 days | `aws`; `SINCE`, `UNTIL` |
| `azure-monitor.sh` | Azure Monitor alerts: metric, log search, activity log, smart detection, Prometheus | 30 days | `az` with its `resource-graph` extension; `SINCE`, `UNTIL` (UTC), `SUBSCRIPTIONS` |
| `google-cloud.sh` | Google Cloud Monitoring alerting policies, every kind | 13 months, closed | `curl`, and `gcloud` or `GOOGLE_TOKEN_FILE`; `GOOGLE_CLOUD_PROJECT`, `SINCE`, `UNTIL` |
| `oci.sh` | OCI Monitoring alarms | 90 days | `oci`; `COMPARTMENT_ID`, `SUBTREE`, `SINCE`, `UNTIL` |
| `datadog.sh` | Datadog monitors | 15 months | `curl`; `DATADOG_API_KEY_FILE` and `DATADOG_APP_KEY_FILE`, or `DATADOG_TOKEN_FILE`; `DATADOG_SITE`, `SINCE`, `UNTIL` |
| `newrelic.sh` | New Relic alert conditions | what the account keeps of `NrAiIncident` | `curl`; `NEW_RELIC_ACCOUNT_ID`, `NEW_RELIC_API_KEY_FILE`; `NEW_RELIC_REGION`, `SINCE`, `UNTIL` |

They need `sh` and `jq` besides, and only read: each script's header says
which permissions. Where an alarm is written in PromQL (a CloudWatch PromQL
alarm, a Google Cloud policy with a PromQL condition, an Azure Prometheus rule
group), `--rules` replays it instead, which gives more: change reviews,
longer `for`s, near misses. See
[How it judges](../../docs/how-it-judges.md). Its history from these
scripts then checks the replay: `lapilli verdict --check-replay
--check-firings FILE`, or, copied into `lapilli serve` under the source
`--alarms` reads it as, the check `serve` runs each day.

```sh
SINCE=2026-10-12T00:00:00Z ./cloudwatch.sh > firings.tmp && mv firings.tmp firings.jsonl
lapilli verdict --firings firings.jsonl --since 28d --incidents incidents.yaml --match service
```

Write to another file and move it: the shell empties a file before a script
writing to it has started, and a script that fails leaves it empty.

## What each counts as a firing

The conversion is in the `.jq` file beside each script, for you to change.
Datadog and New Relic have [a section of their own](#datadog-and-new-relic).

- **When.** CloudWatch and OCI keep moves between states: a firing is from a
  move into ALARM (OCI: Firing) until the next move out of it. An alarm
  already in that state when `SINCE` comes is left out, since when it began
  isn't in the period's history; give `SINCE` a day before the period you
  judge. OCI's time is when the move was triggered, three minutes before it
  was written down, and a firing is of the period it was triggered in: give
  `oci.sh` an `UNTIL` ten minutes or more behind now, or one triggered just
  before it is found by neither that run nor the next. Each script writes
  only firings that start in `[SINCE, UNTIL)`, as `put.sh` sends them. Azure and Google Cloud keep each alert: from when it fired
  until its condition was resolved (Azure) or it closed (Google Cloud). An
  Azure alert closed by hand ends when it was closed, and an activity log,
  service health or resource health alert, fired once for an event, ends as
  it fires. One still on has no end: an Azure log search alert without
  auto-resolve stays so until someone closes it. A CloudWatch move whose
  HistoryData doesn't say its states is read from its summary, and one that
  says neither stops the script.
- **Which series.** CloudWatch and OCI history is of the alarm as a whole, in
  ALARM while any of its series is: one firing for an alarm on forty
  instances. Azure's and Google Cloud's alerts carry their dimensions or
  labels, so each series is its own firing where the alarm keeps them apart.
- **Labels.** What each cloud says about where: CloudWatch an alarm's
  dimensions; Azure the severity (`Sev0` to `Sev4`), the resource, its
  resource group and the alert's dimensions; Google Cloud the monitored
  resource's labels, the metric's, the policy's user labels where those don't
  say, and its severity; OCI the severity and the metric namespace; each,
  the alarm's own ID (below). Rename them in the `.jq` file to what
  `--match` needs, usually `service`.
- **Names.** The alarm's name; for Azure, the alert rule's; for Google Cloud,
  the policy's display name. A Google Cloud policy replayed with `--rules` is
  named by its PromQL condition's `alertRule` when it has one. Each is taken
  as `lapilli serve` takes a rule's name, since one it refused would stop
  the whole copy: the characters that don't show (a line break, a
  zero-width space) taken out, and 512 characters at most. One with nothing
  left is named by the alarm's own ID (below), on CloudWatch `alarm ARN`.
- **The alarm's own ID**, as the label `alarm_id`, on every cloud: the
  alarm's ARN (CloudWatch), the alert rule's resource id (Azure), the
  policy's name, `projects/P/alertPolicies/ID` (Google Cloud), the alarm's
  OCID (OCI). `lapilli serve` reading the same alarms with
  `--alarms SOURCE=URL`, and these firings copied in under that `SOURCE`,
  ties an alarm it doesn't replay to its history by this ID: one rule, the
  alarm's, whatever the alarm is named now. History without the label,
  copied in before it was added, ties by the name. A name two alarms share
  in one source ties neither, nor a Google Cloud policy of several
  conditions, whose alerts don't say which condition opened them. Keep the
  label when renaming the others.

## Not checked against real accounts

These are tested against each cloud's documented answers, with stand-ins for
the CLIs (`internal/firing/recipes_test.go`), not against real accounts. What
the documentation doesn't show (for Datadog and New Relic, see
[their section](#not-checked-against-real-accounts-either)):

- the key the Azure CLI gives Resource Graph's skip token under, taken either
  way;
- how the OCI CLI writes the next page of alarm history (`opc-next-page`);
- the syntax of Google Cloud's alert filter, shown only by example
  (`open_time>="..."`): the period is kept again from each alert's open time,
  so a filter read differently costs only more pages;
- whether Resource Graph's `datetime()` takes a zone other than UTC: Azure's
  times must be given in UTC.

If one doesn't work with yours, please say so.

## Datadog and New Relic

`datadog.sh` reads the events Datadog's monitors write, from the Events API
(`GET /api/v2/events`, the events of `@monitor.id:*`). `newrelic.sh` reads
New Relic's `NrAiIncident` events, an incident's open and its close, with an
NRQL query through NerdGraph. Both read from before `SINCE` (a day for
Datadog, an hour for New Relic, as below) to now, so a firing that started
in the period has its end if it has ended, and each writes only firings that
start in `[SINCE, UNTIL)`, as `put.sh` sends them.

- **When.** Datadog: a monitor's group fires from an event of status `error`
  (Alert) or `warn` (Warn) until its next of status `ok`. Another `error` or
  `warn` between them, a notification again or Warn to Alert, changes
  nothing, and other statuses are passed over. Warn counts, since it
  notifies; take `warn` out of `datadog.jq` to count Alert only. A group
  already on when `SINCE` comes is left out: the day before `SINCE` is read
  to know it, so a monitor that notifies again in the period isn't taken to
  start then. New Relic: an incident fires from its `openTime` until its
  `closeTime`; one opened before `SINCE` is left out, though it closed in
  the period. The hour before `SINCE` is read, as an incident's open event
  can be written after it opened. One still on has no end, on both.
- **Which series.** Datadog's events are each of a monitor's group, so each
  group is its own firing; a monitor without groups is one. An event naming
  two groups is taken as a group of its own. Each New Relic incident is its
  own firing, one for each signal its condition opened one for.
- **Labels.** Datadog: the group's tags (the group `env:prod,service:checkout`
  gives `env` and `service`), the monitor's priority as `severity` (`P1` to
  `P5`), and the monitor's own tags where the group's don't say. New Relic:
  the incident's priority as `severity` (`critical`, `high`, `medium`,
  `low`), the policy's name as `policy`, the entity's name as `entity` (or,
  without one, the incident's target), and the incident's tags (`tags.team`
  gives `team`). Rename them in the `.jq` file to what `--match` needs,
  usually `service`. A tag of no value is left out, and so are labels
  `lapilli serve` wouldn't take: a name or a value with a character that
  doesn't show, a value over 256 bytes, past 32 labels or 4 KB of them.
- **Names.** Datadog: the monitor's name before its template variables are
  filled in (`templated_name`), so one monitor is one rule whatever each
  group's notification says. New Relic: the condition's name; two
  conditions of one name, in two policies, are one rule unless
  `newrelic.jq` tells them apart. Both are taken as the clouds' are (above):
  one with nothing left is `monitor ID` or `condition ID`.
- **The ID**, as `alarm_id`, as the clouds' are: the monitor's (Datadog),
  the condition's (New Relic). `--alarms` doesn't read either tool, so in
  `lapilli serve` it ties nothing yet.
- **Keys.** Read from files and given to `curl` on stdin, so `ps` doesn't
  show them, and never printed. Datadog takes an API key and an
  application key (`DATADOG_API_KEY_FILE`, `DATADOG_APP_KEY_FILE`), or a
  personal or service access token (`DATADOG_TOKEN_FILE`), with
  `events_read`; `DATADOG_SITE` is your site, such as `datadoghq.eu`. New
  Relic takes a user key (`NEW_RELIC_API_KEY_FILE`) that can query the
  account; `NEW_RELIC_REGION` is `us`, `eu` or `jp`.
- **Pages.** Datadog's are 1000 events, by its cursor. NRQL has no cursor:
  each page is the next 1000 events by time, from the last one's
  millisecond, read again. Each stops past `MAX_PAGES` (1000), and so does
  more than 1000 New Relic events in one millisecond. An answer that can't be
  read, a Datadog page with warnings (it may hold part of the events) and a
  NerdGraph answer with errors (it answers 200 with them) are refused, and
  nothing is written.
- **Rate limits.** Past one, both answer 429, and the request is asked
  again after a wait: as Datadog's `X-RateLimit-Reset` says (the seconds
  until its limit resets), or a `Retry-After`; New Relic names none, so 5
  seconds, then twice as long each time. Two minutes at most a wait, five
  waits a request and ten minutes in all a run: past them, and on any
  other answer that isn't a 2xx, nothing is written.

New Relic's NerdGraph also has `aiIssues`, issues and incidents. It isn't
used: it is experimental (asked for with an opt-in header, and subject to
breaking changes), and names an incident's condition and policy only in its
description's words. `NrAiIncident` has them as attributes.

Written from each tool's documentation:

- Datadog: [Get a list of events](https://docs.datadoghq.com/api/latest/events/get-a-list-of-events/),
  [`@monitor.id` in the Event API](https://docs.datadoghq.com/api/latest/events/),
  [the API's keys](https://docs.datadoghq.com/api/latest/),
  [search syntax](https://docs.datadoghq.com/logs/explorer/search_syntax/)
  (events search is the logs'),
  [pages of 1000](https://docs.datadoghq.com/logs/guide/collect-multiple-logs-with-pagination/)
  (events are paged as logs are),
  [tags](https://docs.datadoghq.com/getting_started/tagging/) (no commas),
  [retention](https://docs.datadoghq.com/data_security/data_retention_periods/),
  [rate limits](https://docs.datadoghq.com/api/latest/rate-limits/).
- New Relic: [NerdGraph's endpoints and key](https://docs.newrelic.com/docs/apis/nerdgraph/get-started/introduction-new-relic-nerdgraph/),
  [NRQL through NerdGraph](https://docs.newrelic.com/docs/apis/nerdgraph/examples/nerdgraph-nrql-tutorial/),
  [`NrAiIncident`'s attributes](https://docs.newrelic.com/attribute-dictionary/?event=NrAiIncident),
  [NRQL's clauses](https://docs.newrelic.com/docs/nrql/nrql-syntax-clauses-functions/)
  (`LIMIT`, `ORDER BY`, `SINCE` and `UNTIL`),
  [times in milliseconds](https://docs.newrelic.com/docs/query-your-data/nrql-new-relic-query-language/get-started/query-time-range/),
  [`aiIssues`](https://docs.newrelic.com/docs/apis/nerdgraph/examples/nerdgraph-issues-api-via-github/),
  [NerdGraph's limits](https://docs.newrelic.com/docs/apis/nerdgraph/nerdgraph-usage-limits/)
  and [NRQL's](https://docs.newrelic.com/docs/nrql/using-nrql/rate-limits-nrql-queries/).

### Not checked against real accounts either

Both are tested against the answers each tool's documentation shows, with a
server standing in for its API (`internal/firing/vendor_recipes_test.go`),
not against real accounts. What the documentation doesn't show:

- Datadog: how `monitor_groups` writes a group (read as `key:value` tags,
  one an item or comma-separated); whether a notification again is an event
  of its own (if it is, one more than a day after its monitor triggered is
  taken for a trigger); which status a No Data event has (passed over unless
  `error`, `warn` or `ok`); that `page[limit]` takes 1000, as the logs' does.
- New Relic: the unit of `openTime` and `closeTime` (read as milliseconds, as
  `timestamp`; a smaller number stops the script); the names `SELECT *`
  gives tags under (read as `tags.team`).

If one doesn't work with yours, please say so.

## Every week, before the cloud forgets

CloudWatch and Azure keep 30 days. To judge a quarter, keep the firings
yourself: run a script every week over the last 30 days, keep each file, and
put them together for a verdict. A firing two runs both saw is written by each,
the later with its end, so keep one of each, the one that ended (one still on
after 30 days is never seen to end):

```sh
cat firings-*.jsonl | jq -c -s 'group_by(.rule, .labels, .start) | map(max_by(.end // ""))[]' > quarter.jsonl
```

Or let `lapilli serve` keep them: with `--firings-token-file`, `put.sh`
sends a script's output as a source's firings for the period, in place of
those the ledger had from it then. Give both the same `SINCE` and `UNTIL`:

```sh
export SINCE=2026-11-08T00:00:00Z UNTIL=2026-11-09T00:00:00Z
./cloudwatch.sh > firings.jsonl && LAPILLI_TOKEN_FILE=token ./put.sh http://lapilli:8082 cloudwatch-prod < firings.jsonl
```

Sent again as they are, they change nothing. Every hour over the last day
keeps the ledger close behind; every week over what the cloud keeps ends
the alarms that were still on. A period with no firings takes away what the
ledger had from the source then, so send a file only when the script
worked, as above. [Firings recorded elsewhere](../../docs/serve.md#firings-recorded-elsewhere)
has the rest.
