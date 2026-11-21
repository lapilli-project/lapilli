# A cloud's own alarms

Alarms kept in a cloud's own monitoring are written in PromQL, where Lapilli
replays them like any rule (the tables in [How it judges](how-it-judges.md)), or in that cloud's own terms:
a CloudWatch metric alarm, an Azure Monitor metric or log search alert, a
Google Cloud threshold or log-based policy, every OCI Monitoring alarm (MQL).
Those can't be replayed, but what they fired can be judged.
[recipes/firings](../recipes/firings) has a small script for each cloud that
reads its alarm history and writes it as `--firings` takes it:

```sh
SINCE=2026-10-12T00:00:00Z recipes/firings/cloudwatch.sh > firings.tmp && mv firings.tmp firings.jsonl
lapilli verdict --firings firings.jsonl --since 28d --incidents incidents.yaml --match service
```

| Cloud | Script | Its history |
|---|---|---|
| AWS | `cloudwatch.sh` (`describe-alarm-history`) | 30 days |
| Azure | `azure-monitor.sh` (Azure Resource Graph) | 30 days |
| Google Cloud | `google-cloud.sh` (`projects.alerts.list`) | 13 months, closed alerts |
| Oracle Cloud | `oci.sh` (`get-alarm-history`) | 90 days |

Such a verdict has the stones, precision, what each rule adds and when the
firings came; not what needs a rule's condition: longer `for`s, near misses,
`--compare`. CloudWatch and OCI keep the history of an alarm as a whole, so
an alarm on forty instances is one firing while any of them is in alarm.
The scripts' README says what each counts. `lapilli serve` takes them as
they come and keeps them past what the cloud keeps: see
[Firings recorded elsewhere](serve.md#firings-recorded-elsewhere).

Read through the cloud's API with `--alarms SOURCE=URL` too, an alarm
Lapilli doesn't replay and its history copied in under `SOURCE` are one
rule in `lapilli serve`: judged under the alarm's name, with its page and
what it says. The recipes label each alert with the alarm's own ID
(`alarm_id`: its ARN, resource id, policy name or OCID), which ties the
two; a history without one ties by the name the recipe gives it, and one
whose severity changed by the severity the alarm has now. A name two
alarms read under one source share ties neither, nor a Google policy of
several conditions, PromQL ones too (its alerts don't say which opened
them). Its record is its history's, copied in over twelve weeks without a
gap up to now: copies that stopped longer ago than `--source-quiet` are no
record until they run again, nor are weeks they missed until those are
copied in (each write's period is kept); a job copying weekly needs
`--source-quiet 8d`, or retiring stays refused most of the week. While its
history at another severity fired within twelve weeks, it has none. With
none tied to it, it has no record: it isn't among the rules that never
fired, and retiring it isn't proposed.

## Other monitoring tools

Datadog's monitors and New Relic's alert conditions aren't read as rules,
so they are neither replayed nor listed; what they fired is judged the same
way, from two more scripts in [recipes/firings](../recipes/firings):

| Tool | Script | Its history |
|---|---|---|
| Datadog | `datadog.sh` (the Events API, the events monitors write) | 15 months |
| New Relic | `newrelic.sh` (`NrAiIncident` events, with NRQL through NerdGraph) | what the account keeps |

Each firing is one of a monitor's groups (Datadog), or one incident of a
condition (New Relic), from when it triggered until it recovered or closed,
labelled with its tags, its priority as `severity` and the monitor's or
condition's ID as `alarm_id`. Both read their keys from files and are
copied into `lapilli serve` by `put.sh`, as the clouds' are. They are
checked against the answers each tool's documentation shows, not against
real accounts; the scripts' README says what that leaves unknown.
