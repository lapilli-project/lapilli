# Firings from a cloud's own alarms

Alarms kept in a cloud's own monitoring are mostly written in that cloud's
terms, against metrics no PromQL API serves, so Lapilli can't replay them.
What they fired, it can judge. Each script here reads a cloud's alarm history
and writes it to stdout in the form `lapilli verdict --firings` takes, one
firing a line:

```json
{"rule":"checkout-5xx","labels":{"service":"checkout"},"start":"2026-11-02T03:10:00Z","end":"2026-11-02T03:40:00Z"}
```

| Script | Alarms | History kept | Needs |
|---|---|---|---|
| `cloudwatch.sh` | Amazon CloudWatch alarms, metric and composite | 30 days | `aws`; `SINCE`, `UNTIL` |
| `azure-monitor.sh` | Azure Monitor alerts: metric, log search, activity log, smart detection, Prometheus | 30 days | `az` with its `resource-graph` extension; `SINCE`, `UNTIL` (UTC), `SUBSCRIPTIONS` |
| `google-cloud.sh` | Google Cloud Monitoring alerting policies, every kind | 13 months, closed | `curl`, and `gcloud` or `GOOGLE_TOKEN_FILE`; `GOOGLE_CLOUD_PROJECT`, `SINCE`, `UNTIL` |
| `oci.sh` | OCI Monitoring alarms | 90 days | `oci`; `COMPARTMENT_ID`, `SUBTREE`, `SINCE`, `UNTIL` |

They need `sh` and `jq` besides, and only read: each script's header says
which permissions. Where an alarm is written in PromQL (a CloudWatch PromQL
alarm, a Google Cloud policy with a PromQL condition, an Azure Prometheus rule
group), `--rules` replays it instead, which gives more: change reviews,
longer `for`s, near misses. See the README's "How it judges".

```sh
SINCE=2026-10-12T00:00:00Z ./cloudwatch.sh > firings.tmp && mv firings.tmp firings.jsonl
lapilli verdict --firings firings.jsonl --since 28d --incidents incidents.yaml --match service
```

Write to another file and move it: the shell empties a file before a script
writing to it has started, and a script that fails leaves it empty.

## What each counts as a firing

The conversion is in the `.jq` file beside each script, for you to change.

- **When.** CloudWatch and OCI keep moves between states: a firing is from a
  move into ALARM (OCI: Firing) until the next move out of it. An alarm
  already in that state when `SINCE` comes is left out, since when it began
  isn't in the period's history; give `SINCE` a day before the period you
  judge. OCI's time is when the move was triggered, three minutes before it
  was written down. Azure and Google Cloud keep each alert: from when it fired
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
  say, and its severity; OCI the severity and the metric namespace. Rename
  them in the `.jq` file to what `--match` needs, usually `service`.
- **Names.** The alarm's name; for Azure, the alert rule's; for Google Cloud,
  the policy's display name. A Google Cloud policy replayed with `--rules` is
  named by its PromQL condition's `alertRule` when it has one.

## Not checked against real accounts

These are tested against each cloud's documented answers, with stand-ins for
the CLIs (`internal/firing/recipes_test.go`), not against real accounts. What
the documentation doesn't show:

- the key the Azure CLI gives Resource Graph's skip token under, taken either
  way;
- how the OCI CLI writes the next page of alarm history (`opc-next-page`);
- the syntax of Google Cloud's alert filter, shown only by example
  (`open_time>="..."`): the period is kept again from each alert's open time,
  so a filter read differently costs only more pages;
- whether Resource Graph's `datetime()` takes a zone other than UTC: Azure's
  times must be given in UTC.

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

`lapilli serve` doesn't take firings yet; [docs/design](../../docs/design/) has
how it might.
