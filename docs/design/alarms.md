# Changing a cloud's own alarms

Status: built, for v0.24.0 (see "As built"): the third step of
[changes.md](changes.md#building-it), after the managed Prometheus rulers of
[rulers.md](rulers.md). Amazon CloudWatch alarms, Azure Monitor's alert
rules, Google Cloud Monitoring's alerting policies and OCI Monitoring's
alarms are changed through each cloud's own API, once a person approves. It
adds a way of reading alarms, a tie between an alarm and the history its
recipe copies in, and four places Lapilli writes to, so under
[GOVERNANCE.md](../../GOVERNANCE.md) it is designed first, as a pull
request. It keeps everything changes.md and rulers.md settle, builds on the
code rulers.md adds, and says only what is new.

It is in a document of its own because it is about four more APIs, and
about what a change to an alarm Lapilli can't replay can honestly be.

What it says of the APIs was checked on 2026-11-11 against botocore's
`cloudwatch` model (2010-08-01), the Azure REST API specifications
(`metricAlert` and `activityLogAlerts_API` 2026-01-01,
`scheduledQueryRule_API` 2023-12-01 and 2026-03-01), Google Cloud
Monitoring's v3 reference (`projects.alertPolicies`, `patch`), the OCI Go
SDK's `monitoring` package (the Monitoring API, 20180401) and OCI's
Monitoring policy reference. None of it has been run against an account.
No cloud's alarms can be run locally: there is no emulator of Azure's,
Google's or OCI's, and LocalStack's CloudWatch doesn't fit (Tests).

## Why

Many teams alert from their cloud's own monitoring, and keep those alarms
there, made in the console and changed in the console. Lapilli reads them
today, judges what they fired (recorded.md), checks what they say
(content.md), and proposes changes to them: a fix to what one says, or
retiring one. Every such proposal is for a person: changes.md gave a
cloud's alarms no path, and said they come third.

Some are kept elsewhere: CloudFormation and the CDK, Terraform, Bicep,
Pulumi, Config Connector. As for rules, a change made around those is undone
by them, or fights them, and stays a pull request to where they are
written, for a person. This step applies, through each cloud's API, the
changes a person approved to the alarms kept nowhere else, and only those
the operator names.

## What stays as changes.md and rulers.md have it

- Approval: by an approver, on the proposal's page, in a browser; one by
  default, `--approvals 2` for two; never through the API, MCP or a token;
  no one approves their own proposal, or one their agent made. Agents
  propose, behind `--mcp-propose`, and never approve or apply.
- Off unless `--changes`; the write credentials aren't read without it.
- One unfinished proposal for a rule at a time, undo included. A proposal
  touches one rule.
- The limits (`--changes-limits`, five deletions a day and twenty changes
  an hour) count a change once, when it is sent. The pause is looked at
  again just before each write.
- Before anything is sent, the alarm is read again from the cloud, not the
  ledger, and checked again; what it replaces is kept for undo. What became
  of a change is read back, never taken from the answer; a change that
  can't be read back stays `applying`, is read again each round, and is
  never sent again.
- Every step is kept in the ledger's changes and told: `change.applied`,
  `change.refused`, `change.failed`, `change.undone`.
- From rulers.md: `--changes-in` as the opt-in, `--changes-managed` for
  tags, one write identity a cloud (`--changes-aws-role` and
  `--changes-azure-client-id` are the same flags), a proposal written only
  where it was read, the unit read again and the change made afresh when
  only the rest of it changed, undo putting back the rule and not the whole
  unit, and every request to a fixed host with no redirects.

## How each is read today

Every cloud's alarms are read from files only, with `--rules`:

| Cloud | Read today | Replayed | What reading a file leaves out |
|---|---|---|---|
| AWS | a file of `aws cloudwatch describe-alarms`'s answer: metric and composite alarms | PromQL alarms (`EvaluationCriteria.PromQLCriteria`): `Query`, `PendingPeriod` as `for`, `RecoveryPeriod` as `keep_firing_for`, `EvaluationInterval` | the file is a copy. Tags aren't in it. `ActionsEnabled` isn't read, so an alarm whose actions are off reads as on |
| Azure | `az monitor metrics alert list`, `scheduled-query list`, `activity-log alert list`, or the API's answer, saved | none: each metric, log search and activity log alert is listed as not PromQL | a copy. And a metric alert with a PromQL criterion, which the API has in its stable 2026-01-01 version, is read as not PromQL: it could be replayed and isn't |
| Google Cloud | `gcloud monitoring policies list`, or `alertPolicies.list`'s answer | a policy's PromQL conditions: `query`, `duration` as `for`, `evaluationInterval` | a copy |
| OCI | `oci monitoring alarm list` | none: OCI alarms are written in MQL | a copy, and without each alarm's `body`: what an OCI alarm says isn't known (`TextUnknown`) |

The history of the alarms that aren't replayed comes from
`recipes/firings`, one script a cloud, into `--firings` or, copied into
`lapilli serve` as a source, into the ledger (recorded.md).

Two things in how this works now have to change before anything is
written, and the first is wrong today:

- **An alarm Lapilli doesn't replay looks silent.** The alarm read with
  `--rules` is one rule in the ledger; its firings, copied in, are another,
  named apart (`checkout-5xx [cloudwatch-prod]`). The first has no firings
  of its own, so after twelve weeks it is among the rules that never fired,
  and with `--changes` Lapilli proposes retiring it, for a person, with a
  record of nothing. The same holds for any rule that isn't replayed, such
  as a Grafana rule on Loki, which Lapilli would propose turning off. This
  was found reading the code while designing this step (`silentRules`
  takes every name in the latest version that has no firings); no test
  covers it. A rule Lapilli can't replay has an unknown record, not an
  empty one.
- **A change needs the alarm's place.** A file is a copy; the account, the
  region, the resource group or the compartment, and the tags, are what a
  write needs and what the other-tool checks read. So, as rulers.md added
  `--ruler`, this step adds reading alarms from each cloud's API, and only
  an alarm read so is ever written.

## What can be changed, and what a backtest means here

A backtest replays a rule as it is and as proposed over twelve weeks. Only
an alarm whose condition is PromQL can be replayed: Lapilli has no
CloudWatch metric math, KQL or MQL, and the metrics most such alarms watch
are served by no PromQL API (recorded.md, "Alternatives"). So:

| The alarm | A `for`, a threshold | What it says | Retiring | Undo |
|---|---|---|---|---|
| PromQL: a CloudWatch PromQL alarm; an Azure metric alert with one static PromQL criterion; a Google policy's PromQL condition | yes, backtested, as any rule | yes | turned off, then deleted four weeks later, checked by replaying it | yes |
| Not PromQL: every other CloudWatch alarm, Azure alert rule and Google condition; every OCI alarm | no | yes | turned off; deleted four weeks later only with its history tied (below), and a weaker check, said as such | yes |

- **No `for` or threshold without a replay.** A CloudWatch metric alarm's
  `Threshold` and `EvaluationPeriods`, or an OCI alarm's `pendingDuration`,
  are fields, easy to write. Nothing Lapilli has could say what the alarm
  would have fired with them, and changes.md promises a backtest with each
  such change. They stay for a person, in the console, and the page says
  why.
- **What it says needs no backtest:** it doesn't change when the alarm
  fires, as changes.md's `says` already holds.
- **Retiring isn't backtested anywhere;** it carries the rule's record over
  twelve weeks, and is refused when checked again if the rule caught more
  that nothing else caught. For an alarm that isn't replayed, the record is
  its history, copied in. With none tied to it, it has no record, and
  retiring it isn't proposed, by Lapilli or from its page.

Every cloud gains the same: reading through its API, the path, what it
says, retiring and undo. A `for` and a threshold come wherever a cloud's
alarms are PromQL: CloudWatch, Azure, Google. OCI has no PromQL alarms, so
it gains neither; the line is the replay, the same on every cloud.

## Reading alarms: `--alarms SOURCE=URL`

One flag, for `lapilli verdict` and `lapilli serve`, repeatable, read
beside whichever of `--rules`, `--loaded-rules`, `--grafana` or `--ruler`
is given, or alone. Unlike those four, which are one place to read rules
from (pick one), a cloud's alarms are kept apart from any rules, and a
team commonly has both. A version holds the documents of every place; a
read that fails anywhere keeps the version as it was, so an API that
answers 503 doesn't read as every alarm deleted.

`SOURCE` names the alarms: the name their history is copied in under
(recorded.md: `[a-z0-9-]`, 40 at most, no dash at either end). The URL's
host says which cloud:

| Cloud | `--alarms SOURCE=URL` | Read with | One document is |
|---|---|---|---|
| AWS | `https://monitoring.REGION.amazonaws.com`, or a FIPS, dual-stack or VPC endpoint host, as `--auth aws` takes them now | `DescribeAlarms` with `AlarmTypes` `MetricAlarm` and `CompositeAlarm`, paged by `NextToken`, in CloudWatch's JSON 1.0 protocol | an alarm: `{"MetricAlarms":[ALARM]}` or `{"CompositeAlarms":[ALARM]}`, the shape the CLI's file has |
| Azure | `https://management.azure.com/subscriptions/SUB`, or `.../resourceGroups/RG` | the list of each of `metricAlerts` (api-version 2026-01-01), `scheduledQueryRules` (2026-03-01) and `activityLogAlerts` (2026-01-01), following `nextLink` | an alert rule: the resource as listed |
| Google Cloud | `https://monitoring.googleapis.com/v3/projects/PROJECT/alertPolicies` | `alertPolicies.list`, paged by `pageToken` | a policy |
| OCI | `https://telemetry.REGION.oraclecloud.com/20180401/alarms?compartmentId=OCID`, in any of Oracle's realms; with the tenancy's OCID, `&compartmentIdInSubtree=true` reads every compartment, and needs permission in the tenancy | `ListAlarms`, paged by `page`; then `GetAlarm` for each alarm that is new or reads differently in the list, and for every alarm once an hour, since the list has no `body` and no time of update | an alarm: `{"data":[ALARM]}`, the shape the CLI's file has |

- **One document an alarm,** in the shape its file already has, so
  `rules.Parse` reads both alike and no reader is written twice. Each
  document's source is the alarm's own ID: its ARN, its resource ID, its
  policy name (`projects/P/alertPolicies/ID`), its OCID.
- **Its definition, not its state.** What changes as the alarm runs is
  left out of the document: CloudWatch's `StateValue`, `StateReason`,
  `StateReasonData`, the state's times, `EvaluationState`,
  `AlarmConfigurationUpdatedTimestamp`, `ActionsSuppressedBy` and
  `ActionsSuppressedReason`; Azure's `lastUpdatedTime`, `systemData` and
  `etag`; Google's `mutationRecord`; OCI's `timeUpdated`. A version is then
  a change to an alarm, not to its state. What is left out is read again
  when a change is carried out, where it is needed.
- **Where an alarm is.** Each rule read gains rulers.md's namespace, set to
  `SOURCE`. Its key takes the namespace only when it has one, so alarms
  read from files keep their keys, and no ledger gains a version on
  upgrade from them. An alarm and a rule of one name are told apart by it:
  `HighCPU [cloudwatch-prod]`.
- **Credentials, to read.** Each URL is read with its cloud's credentials
  from the chain `--auth` uses for that cloud (the pod's identity), chosen
  by the host, whatever `--auth` says: `--auth` stays for `--prometheus`.
  AWS: SigV4 for the service `monitoring`, `cloudwatch:DescribeAlarms`
  (composite alarms are described only with it on `*`, as CloudWatch
  documents) and `cloudwatch:ListTagsForResource`. Azure: a token for
  Resource Manager (rulers.md's), and Monitoring Reader. Google:
  `roles/monitoring.viewer`. OCI: no `--auth oci` exists today, as OCI has
  no managed Prometheus; reading alarms adds OCI's credentials (OKE
  workload identity, resource principal, instance principal, or an OCI
  config file, in that order), with `inspect alarms`, `read alarms` and
  `read metrics` (`GetAlarm` asks for `METRIC_READ` in the alarm's metric
  compartment).
- **How often.** Each `--watch`, as `--ruler` and `--grafana` are read.
- **Tags** are read when they are needed, for a proposal and when it is
  carried out, not each `--watch`: CloudWatch's take a request an alarm.

What this reading adds besides changes:

- **OCI alarms say what they say.** `GetAlarm` has the `body`, so the
  content check checks it, where today it can't.
- **Azure metric alerts in PromQL are replayed.** A metric alert whose
  `criteria` is `Microsoft.Azure.Monitor.PromQLCriteria` with one criterion
  of type `StaticThresholdCriterion` is a rule like a Prometheus one: its
  `query`, `failingPeriods.for` as `for`, `evaluationFrequency` as its
  interval, and `resolveConfiguration` as for Azure's rule groups; its
  severity becomes `severity=Sev0` to `Sev4`. With a dynamic threshold, or
  several criteria (all must hold, over queries Lapilli can't join), it is
  listed, with why. It is replayed against `--prometheus`, which must be
  the query endpoint of the Azure Monitor workspace in its `scopes`; as
  for the other clouds, Lapilli can't check that. This also reads from
  files.
- **CloudWatch's actions off read as off.** An alarm with
  `ActionsEnabled: false` is `Disabled`, as an Azure alert rule with
  `enabled: false` is. Read from files too, so a ledger reading such an
  alarm from a file gains a version on upgrade; the changelog says so.

## An alarm and its history, one rule

An alarm Lapilli doesn't replay is judged by what really fired: its
history, copied in under `SOURCE`. So such an alarm, read through
`--alarms SOURCE=URL`, and the source's recorded rule of the name the
recipe gives it are one rule in the ledger:

- **The name the recipe gives it** (recipes/firings): CloudWatch the
  alarm's name; Azure the alert rule's name; Google the policy's display
  name, for a policy of one condition (a policy's alerts don't say which of
  several conditions opened them); OCI the alarm's display name.
- **One rule.** The recorded rule isn't named apart from the alarm, as it
  is today: it is the alarm's, with the alarm's definition, its page, and
  the notifications already tied to it (notifications.md). Nothing in the
  ledger's schema changes: `rules.source` and `rules.sent` say it, as now.
- **Only where the name is the alarm's alone.** A name two alarms read
  under the source share (Azure's names in two resource groups; Google's
  and OCI's display names, which needn't be unique) ties neither: each is
  read, not judged, and its page says why.
- **A replayed alarm stays replayed.** A PromQL alarm is judged by its
  replay, as now; its history, if copied in too, stays apart, named apart.
  Setting the two beside each other is the roadmap's "checking a cloud
  alarm's replay against its history", not this step.
- **A record from when its history began.** An alarm's record covers the
  weeks since its source's earliest firings; a recipe run once over what
  the cloud keeps starts it there (30 days for CloudWatch and Azure, 90 for
  OCI, 13 months for Google). Proposals that rest on twelve weeks wait for
  twelve.
- **No history, no record.** An alarm Lapilli doesn't replay with nothing
  tied to it, whether read from a file or through `--alarms`, has an
  unknown record. It is never among the rules that never fired, or spare,
  and nothing is proposed from its record. The same holds for every rule
  that isn't replayed and isn't off.

In `lapilli verdict`, `SOURCE` names where the alarms were read, and
nothing more: `--firings` has no sources.

## The alarm path

A proposal for an alarm read through `--alarms` takes a new path, `alarm`,
beside `git`, `grafana`, `ruler` and `none`, when:

- `--changes-in` names it (below), and
- nothing on it says another tool keeps it, and
- the change can be made there (the next section), and
- what the alarm's actions do allows it (CloudWatch, below).

Otherwise it is for a person, as now, and its page says why ("it was read
from a file: read it with `--alarms` to change it through the cloud"; "it
isn't one `--changes-in` names"). All of them are asked again when the
change is carried out.

`--changes-in`, as rulers.md builds it, takes alarms as `SOURCE:PATTERN`,
the source an `--alarms` names, a colon, and a name or a prefix ending in
`*`:

| Cloud | `PATTERN` is | For example |
|---|---|---|
| AWS | the alarm's name, as IAM's alarm ARNs end | `cloudwatch-prod:checkout-*` |
| Azure | `RG/NAME`, the resource group and the alert rule's name, compared in any letter case; `RG/*` for every alert rule in a resource group | `azure-prod:shop-rg/*` |
| Google Cloud | the policy's display name | `gcp-prod:checkout *` |
| OCI | the alarm's display name | `oci-prod:checkout-*` |

An entry that begins with an `--alarms` source's name and a colon is that
source's; any other is a ruler's, as rulers.md has it. Without an entry for
it, nothing in a source is changed. A display name (Google, OCI) is a
person's to change in the console; renaming an alarm into a pattern lets it
in, and out of one keeps it out, which is a person's act either way.

## Each cloud

|  | CloudWatch | Azure Monitor | Google Cloud Monitoring | OCI Monitoring |
|---|---|---|---|---|
| A rule is | an alarm, by name, in an account and a region | an alert rule: resource group, type, name | a policy's condition; a policy holds one or more | an alarm, by OCID |
| What is written | the whole alarm: `PutMetricAlarm`, as read, with the change | one field: `PATCH` on the resource; an activity log alert's description, a `PUT` of the whole resource | the fields named: `alertPolicies.patch` with `updateMask` | the fields changed: `UpdateAlarm` |
| A `for` | PromQL alarms: `PendingPeriod`, in seconds (0 to 86,400) | PromQL metric alerts: `criteria.failingPeriods.for`, ISO 8601 | PromQL conditions: `duration` (`600s`), the mask `conditions` | none |
| A threshold | the number in `Query` | the number in the criterion's `query` | the number in `query`, the mask `conditions` | none |
| What it says | `AlarmDescription`, 1,024 characters at most | `description` | `documentation.subject`, `.content`, `.links` | `body` |
| Turning off | `DisableAlarmActions`: the alarm is still evaluated, its actions don't run | `enabled: false` | `enabled: false`, a policy of one condition | `isEnabled: false` |
| Deleting | `DeleteAlarms`, one alarm | `DELETE` | `alertPolicies.delete`, a policy of one condition | `DeleteAlarm` |
| Stale | the alarm's hash; `AlarmConfigurationUpdatedTimestamp` | the rule's hash; a log search alert's `etag`, sent as `If-Match` | the policy's hash; `mutationRecord.mutateTime` | the alarm's hash; its `etag`, sent as `if-match` |
| Another tool's | `--changes-in`; tags | `--changes-in`; tags; Azure refuses under a deny assignment or a lock | `--changes-in`; labels | `--changes-in`; freeform and defined tags |
| Write identity | `--changes-aws-role` | `--changes-azure-client-id` | `--changes-google-service-account` | `--changes-oci-config` |

Only OCI refuses a write made over a change it didn't see: `if-match`
with the etag `GetAlarm` gave ("The resource will be updated or deleted
only if the etag you provide matches the resource's current etag value").
Azure's log search alerts carry an `etag` the specification describes in
general terms only. CloudWatch and Google have none: as with Mimir, nothing
stops another write landing between Lapilli's read and its own, a request
or two apart. Nor have Azure's metric and activity log alerts: their
specification (`metricAlert` 2026-01-01) gives the resource no `etag` and
`PATCH` no `If-Match` (checked again for 0.28.1). Their `PATCH` sends the
properties changed alone, so another write to other properties in that
window stands; one to the same property, or to a metric alert's
`criteria`, written whole, is written over.

### Amazon CloudWatch

Requests go to the `--alarms` host in CloudWatch's JSON 1.0 protocol
(`Content-Type: application/x-amz-json-1.0`,
`X-Amz-Target: GraniteServiceVersion20100801.ACTION`), signed with SigV4
for `monitoring`. botocore's model lists it among the service's protocols
(`smithy-rpc-v2-cbor`, `json`, `query`, CBOR first, as the SDKs now
prefer). JSON is read by the parser Lapilli has, with no AWS service client
added.

**The whole alarm, as read.** `PutMetricAlarm` "completely overwrites the
previous configuration of the alarm", and `ActionsEnabled` left out is
`TRUE`. So the request is built from the alarm just read: every field the
model's `PutMetricAlarmInput` has, as read, with the one change; those the
answer has and the input doesn't (`AlarmArn`, the state and its times,
`EvaluationState`, `AlarmConfigurationUpdatedTimestamp`) left out; `Tags`
left out, since an update ignores them. Any field in the answer, at any
depth, that the model Lapilli was built with doesn't have refuses the
change: written back without it, it would be lost ("CloudWatch gave a field
this Lapilli doesn't know: change it in the console, or upgrade"). A test
holds Lapilli's list of fields to the model file.

**Not every alarm.**

- **Actions that do more than tell.** An alarm whose actions scale (Auto
  Scaling policies, as target tracking makes), or stop, reboot, recover or
  terminate an instance (EC2 actions), is automation, and its actions are
  often kept by the service that made it. Turning its actions off turns
  the automation off. It isn't changed at all; the page says why. Actions
  that tell (SNS topics, Lambda functions, Systems Manager OpsItems and
  Incident Manager response plans) are what an alert's are.
- **A warm-up.** An alarm with a `WarmUpConfiguration` "stays in
  `INSUFFICIENT_DATA`" after it is updated, "and does not perform alarm
  actions" for that long. Rewriting it would quiet it, so it isn't
  rewritten (a `for`, a threshold, what it says); turning it off and
  deleting it are as for any alarm.
- **Composite alarms** can be turned off and on (`DisableAlarmActions`,
  `EnableAlarmActions`), nothing more: `PutCompositeAlarm` must be allowed
  on `*`, as CloudWatch documents, so the write role can't be held to the
  alarms named, and a deletion couldn't be undone within it.
- **An alarm a composite alarm watches** isn't deleted: Lapilli reads the
  source's composite alarms, and one whose `AlarmRule` names it would be
  left watching nothing.
- **Log alarms** (`LogAlarms`, `PutLogAlarm`) aren't read by Lapilli
  today, so nothing is proposed for them.

**Answers.** `PutMetricAlarm`, `DisableAlarmActions`, `EnableAlarmActions`
and `DeleteAlarms` answer 200 with no body. `DeleteAlarms` deletes the
names that exist and answers alike for those that don't, and CloudWatch
says to confirm with `DescribeAlarms`; Lapilli deletes one alarm a request
and reads it back.

### Azure Monitor

**One field, where the API takes one.** Each alert rule type has an
`Update` (`PATCH`) whose properties are all optional: a metric alert's
`MetricAlertPropertiesPatch` and a log search alert's properties take
`description` and `enabled` alone, and a metric alert's `criteria`, written
whole as read with the one change, for its `for` or threshold. An activity
log alert's `PATCH` takes `enabled` and tags only, so its description is a
`PUT` of the whole resource as read, `location` and `tags` and all, with
any field the specification doesn't have refusing it, as for CloudWatch.
`PATCH` answers 200, `PUT` 200 or 201, `DELETE` 200 or 204.

A log search alert carries an `etag`; Lapilli sends it as `If-Match`.
Whether Azure honours it for this type is for someone with an account to
find (Questions). The others carry none.

**Off and on** is the rule's own `enabled`.

**Names and places.** A rule is changed at its `id`, which must be under
the `--alarms` scope and of the form
`/subscriptions/S/resourceGroups/RG/providers/Microsoft.Insights/TYPE/NAME`
for one of the three types, compared in any letter case.

### Google Cloud Monitoring

**The fields named.** `alertPolicies.patch` sets each field in
`updateMask` to the value sent, and leaves the rest. What it says is
`documentation.subject` (`summary`), `documentation.content`
(`description`, Markdown) and `documentation.links` (`runbook_url` or
`runbook` as its first link, the others as read; three at most). A `for`
or a threshold is `conditions`: every condition as read, each with its
`name` so it keeps its ID, and the one changed. Off is `enabled`. An empty
mask would replace the whole policy, and is never sent.

**A policy of several conditions.** Its documentation and its `enabled`
are the policy's, so what one condition says, turning one off and deleting
one would touch the other conditions' rules. On such a policy only a
condition's own `for` and threshold are offered; the rest is for a person.
A condition is never taken out of a policy: under an `AND` combiner that
changes when the others fire.

**One writer.** Google asks callers to "single-thread API calls that
modify the state of alerting policies in a single project"; Lapilli sends
one change at a time.

**Deleting and making again.** A deleted policy made again (undo) is a new
policy, with a new ID. Its display name, and so its history's tie, are as
before.

### OCI Monitoring

**The fields changed.** `UpdateAlarm` (`PUT /20180401/alarms/{alarmId}`)
takes `UpdateAlarmDetails`, every field optional: Lapilli sends only `body`
or `isEnabled`. An alarm's `overrides` keep their own bodies; a change to
what an alarm with overrides says changes the base `body` only, and the
page says so.

**A write made only over what was read.** `GetAlarm` answers with an
`etag`; `UpdateAlarm` and `DeleteAlarm` send it as `if-match`, and a 412 is
a change refused as stale. `DeleteAlarm` answers 204.

**Deleting and making again.** An alarm made again (`CreateAlarm`, undo)
has a new OCID; its display name, and its tie, are as before. Its
`destinations` must still exist.

**Signed requests.** OCI signs each request with its own HTTP signature.
Lapilli uses the OCI Go SDK's `common` and `common/auth` packages for the
credentials and the signer, and builds the requests itself, as it does
for the other clouds.

## Checked again, written, read back

As rulers.md, with the alarm (Google: the policy) as the unit. When a
proposal is made, Lapilli keeps the rule's hash (`before_hash`), the unit's
hash (`doc_hash`) of what the write would rewrite, the request as it would
be sent (`document`) and the diff. `doc_hash` covers: the whole
`PutMetricAlarm` input (CloudWatch); the field patched, or for a `PUT` the
resource less what Azure sets (Azure); the fields in the mask (Google); the
fields sent (OCI).

When it is carried out:

1. **The unit is read again** from the cloud, with its state, its tags and
   its version fields.
   - Gone: refused.
   - No longer allowed by `--changes-in`, now tagged as another tool's,
     now with actions that do more than tell (CloudWatch): refused, with
     why.
   - The rule doesn't hash to `before_hash`: refused as stale ("it changed
     in the cloud since it was proposed: propose it again").
   - The unit hashes to `doc_hash`: `document` is sent, as approved.
   - Otherwise only what isn't the rule changed: a CloudWatch alarm's
     actions, say, or another condition of the policy. The change is made
     afresh in the unit as it is now, read back before it is sent, and the
     new request, hash and diff are kept on the proposal.
2. **The backtest is run again** for a `for` or a threshold; the record is
   taken again for retiring, as now.
3. **The pause is looked at** once more. The proposal is kept as
   `applying`, with the unit as just read (the backup), counted against the
   limits, before anything is sent.
4. **It is sent**, to the host `--alarms` names and nowhere else, with the
   version field where the cloud has one (OCI's `if-match`, Azure's
   `If-Match`). Just before, CloudWatch's
   `AlarmConfigurationUpdatedTimestamp` and Google's `mutateTime` are read
   once more: moved since step 1, it goes back to step 1 once, then is
   refused as stale.
5. **It is read back**, at once and each round until it can be:
   - the rule reads as proposed (or, deleted, isn't there), and the rest
     of the unit as in the backup: applied;
   - the rule reads as in the backup: read again one round later, since
     these APIs may be read a moment behind a write the cloud took; still
     so, failed, with the cloud's answer;
   - anything else (another write landed between): failed, saying it reads
     as neither, the backup kept for a person;
   - unreadable: still `applying`, read again each round, never sent
     again.

A write the cloud refused (a 4xx with why: a duration CloudWatch won't
take, a query Google won't parse, OCI's 412) fails at once, with that
answer, and is read back all the same.

## Undo

An approver undoes an applied change from its page, by a proposal of its
own on the alarm path, as rulers.md: the rule put back as it was kept, not
the whole unit, so a person's change since to its actions, or to another
condition, stays.

- The rule must read as the change left it (`after_hash`); for a deletion,
  no alarm of its name may be there again (CloudWatch's `PutMetricAlarm`
  and Azure's `PUT` would overwrite it). Otherwise it is refused ("changed
  since: undo it in the console").
- **A `for`, a threshold, what it says:** the changed fields written back
  from the backup, as the change was written: the alarm as it is now with
  them (CloudWatch), the field patched (Azure), the mask (Google), the
  fields (OCI).
- **Turned off:** on again: `EnableAlarmActions`, `enabled: true`,
  `isEnabled: true`.
- **Deleted:** made again from the backup: `PutMetricAlarm` with its tags
  (CloudWatch); a `PUT` of the resource less what Azure sets (Azure);
  `alertPolicies.create`, a new ID (Google); `CreateAlarm`, a new OCID
  (OCI). What it notifies must still exist; the cloud's answer says when it
  doesn't.

The backup is kept for as long as `--keep`.

## Retiring

Every cloud can turn an alarm off, so retiring is as for Grafana and
Azure's rule groups: off first; a deletion no sooner than four weeks later,
approved again, only if the alarm is still off and nothing it would have
caught was missed meanwhile. An alarm turned off by hand, in the console,
is for a person to delete there.

What "nothing it would have caught was missed" can mean depends on what
Lapilli has of the alarm while it was off:

- **A replay** (PromQL alarms): the alarm as it was, replayed over the four
  weeks, catching no incident that nothing caught, as now.
- **No replay:** Lapilli can't say what it would have caught. The
  incidents of those four weeks that nothing caught stand in: any that
  shares a `--match` label value with what the alarm fired in the twelve
  weeks before it was turned off (or, if it fired nothing, any at all)
  refuses the deletion. The proposal's page says this is a stand-in, in
  those words, beside the evidence.
- **And its own history while off,** where there is one. CloudWatch
  documents that an alarm with its actions off still changes state; only
  the actions don't run. Its history, copied in, goes on, and its firings
  while off are judged as before (they paged no one, as its notifications
  show). Any of them catching an incident that nothing else caught refuses
  the deletion too. The other clouds don't document whether an alarm off
  is evaluated; history copied in while it was off counts the same way if
  there is any.

A deletion is counted against the five a day, kept for undo, and needs
the record of twelve weeks before it was turned off.

## Alarms another tool keeps

Each cloud's tools leave little on an alarm:

| Tool | What it leaves |
|---|---|
| CloudFormation, the CDK, SAM | `aws:cloudformation:stack-id` and its siblings, on resources that take tags; CloudWatch alarms take tags in CloudFormation since 2024, but whether these reach an alarm isn't documented (Questions) |
| Terraform (AWS, Azure, OCI providers) | nothing of its own; `default_tags` are what the operator names |
| Terraform (Google provider) | `goog-terraform-provisioned` on resources with a standard `labels` field; a policy's are `user_labels`, and no such label is documented for them |
| Bicep, ARM templates | nothing; a deployment stack's deny settings, and locks, make Azure refuse the write itself |
| Config Connector | `managed-by-cnrm: "true"` on the resources it makes, as its documentation says in general; not checked for alerting policies |
| Pulumi | nothing by default |
| OCI Resource Manager | nothing on the alarm |

So, as rulers.md found for Mimir, `--changes-in` is what decides. A tag or
label refuses an alarm when it is one Lapilli knows
(`aws:cloudformation:stack-id`, `managed-by-cnrm=true`,
`goog-terraform-provisioned=true`) or one the operator names in
`--changes-managed`: `KEY=VALUE` or `KEY=*`; for OCI's defined tags
`NAMESPACE.KEY=VALUE`. The documentation says not to name in `--changes-in`
alarms a tool keeps.

## Security

What a stolen Lapilli could do grows by this: rewrite, turn off or delete
the alarms `--changes-in` names in the clouds it reads, within what each
write identity is allowed. It is held so.

**One write identity a cloud, read only with `--changes`,** given only
what changing those alarms needs. Without one, the pod's own identity is
used, which then needs the same.

- **AWS:** `--changes-aws-role ARN`, the role rulers.md's Amazon writes
  use, which the pod's identity assumes for writes only. Its policy gains,
  for each region and prefix named:

  ```json
  {
    "Version": "2012-10-17",
    "Statement": [
      {
        "Effect": "Allow",
        "Action": [
          "cloudwatch:PutMetricAlarm",
          "cloudwatch:DisableAlarmActions",
          "cloudwatch:EnableAlarmActions",
          "cloudwatch:DeleteAlarms",
          "cloudwatch:ListTagsForResource",
          "cloudwatch:TagResource"
        ],
        "Resource": "arn:aws:cloudwatch:eu-west-1:111122223333:alarm:checkout-*"
      },
      {
        "Effect": "Allow",
        "Action": "cloudwatch:DescribeAlarms",
        "Resource": "*"
      }
    ]
  }
  ```

  `TagResource` is for making a deleted alarm again with its tags; nothing
  else tags. `DescribeAlarms` is read only. No `PutCompositeAlarm`, no EC2,
  Auto Scaling or `iam:CreateServiceLinkedRole`: the alarms that would
  need them aren't changed.
- **Azure:** `--changes-azure-client-id ID`, rulers.md's second
  user-assigned identity, federated with the same service account. Its
  custom role gains, assigned on each alert rule named or on the resource
  group for `RG/*`:

  ```json
  {
    "Name": "Lapilli alert rule changes",
    "IsCustom": true,
    "Actions": [
      "Microsoft.Insights/metricAlerts/read",
      "Microsoft.Insights/metricAlerts/write",
      "Microsoft.Insights/metricAlerts/delete",
      "Microsoft.Insights/scheduledQueryRules/read",
      "Microsoft.Insights/scheduledQueryRules/write",
      "Microsoft.Insights/scheduledQueryRules/delete",
      "Microsoft.Insights/activityLogAlerts/read",
      "Microsoft.Insights/activityLogAlerts/write",
      "Microsoft.Insights/activityLogAlerts/delete"
    ],
    "AssignableScopes": ["/subscriptions/SUB/resourceGroups/shop-rg"]
  }
  ```

  Unlike rulers.md's, it deletes: retiring an alert rule ends in its
  deletion.
- **Google Cloud:** `--changes-google-service-account EMAIL`, a service
  account the pod's identity impersonates for writes only, through IAM
  Credentials' `generateAccessToken`, holding
  `roles/iam.serviceAccountTokenCreator` on that one account and nothing
  more. The account is given, on the project, a custom role:

  ```yaml
  title: Lapilli alerting policy changes
  stage: GA
  includedPermissions:
  - monitoring.alertPolicies.get
  - monitoring.alertPolicies.list
  - monitoring.alertPolicies.update
  - monitoring.alertPolicies.delete
  - monitoring.alertPolicies.create
  ```

  Google grants these on a project only, so `--changes-in` is Lapilli's own
  check there, and the documentation says so. `create` is for undoing a
  deletion.
- **OCI:** `--changes-oci-config FILE`, an OCI config file and the API key
  it names, for a user in a group of its own. OCI's workload identity has
  no second identity a pod can take for writes, as AWS's roles, Azure's
  federated identities and Google's impersonation give, so this one is a
  key, in a Secret mounted only when changes are on. Its policy, for each
  compartment named:

  ```
  Allow group lapilli-alarm-changes to manage alarms in compartment shop
    where any {request.permission = 'ALARM_INSPECT', request.permission = 'ALARM_READ',
               request.permission = 'ALARM_UPDATE', request.permission = 'ALARM_DELETE',
               request.permission = 'ALARM_CREATE'}
  Allow group lapilli-alarm-changes to read metrics in compartment shop
  ```

  `manage` is the only verb with `ALARM_UPDATE`; the condition leaves out
  `ALARM_MOVE`. `UpdateAlarm` asks for `METRIC_READ` in the alarm's metric
  compartment, which the second line gives. `ALARM_CREATE` is for undoing
  a deletion.

**Only where it was told.** Every request goes to the host `--alarms`
names (and to what the credential chains reach through their libraries, as
they do now: AWS STS, Google's token and IAM Credentials endpoints, OCI's
for workload identity), follows no redirect (a 3xx is an error),
and reads at most 64 MB for a list and 4 MB for an alarm, within 30
seconds, as rulers.md's.

- Hosts: AWS, `monitoring` in a region, by `awsHost` (FIPS, dual-stack,
  VPC endpoints), the region signed for being the host's. Azure, Resource
  Manager's, as rulers.md. Google, `monitoring.googleapis.com`. OCI,
  `telemetry.REGION` in one of Oracle's realms, as `ociHost` knows them
  for Notifications.
- What is read from a cloud is data. An ID is written to only when it is
  of its cloud's form and under the URL's scope: an ARN of the URL's
  region; an Azure ID under the subscription or resource group, of one of
  the three types; a policy name under the URL's project; an OCID of an
  alarm in the URL's compartment, or under the tenancy with the subtree.
  Each name goes into a path as one escaped segment; one that is empty,
  `.` or `..`, has a control character, or is longer than its cloud allows
  is refused.
- Azure's `nextLink` is followed only when it is https on the same host,
  as rulers.md. Google's `pageToken` and OCI's `page` are passed as query
  values, never as URLs.

**A proposal is written where it was read.** The place is worked out again
from `--alarms SOURCE=URL` and the alarm's ID, and must be the proposal's
source: one made against another account, region, subscription, project
or compartment, before a restart changed the flag, is refused.

**Never from outside.** Nothing the notifications or incidents ports take
becomes a write, a cloud's alarm notifications included; the changes token
can't see alarm proposals.

**Never a silence.** Lapilli doesn't write CloudWatch's alarm mute rules,
OCI's suppressions or Google's snoozes, and doesn't take notification
channels or action groups off an alarm: retiring is the alarm's own off,
approved as a change to the rule.

**Told and kept**, as every write: `change.applied` (or `.failed`) to the
webhook and Slack, what it replaced in the ledger.

**Reviewed as an attacker would** before release, the four write paths
first, as CONTRIBUTING.md asks.

## Options

- `--alarms SOURCE=URL`: reading, for both commands, repeatable.
- `--changes-in LIST`: as rulers.md, with `SOURCE:PATTERN` for alarms.
- `--changes-managed KEY=VALUE`: as rulers.md; for OCI's defined tags
  `NAMESPACE.KEY=VALUE`.
- `--changes-aws-role ARN`, `--changes-azure-client-id ID`: as rulers.md,
  for alarms too.
- `--changes-google-service-account EMAIL`, `--changes-oci-config FILE`:
  new, alarms only.

Each write identity needs `--changes` and an `--alarms` (or `--ruler`) of
its cloud; a `SOURCE:` entry in `--changes-in` needs that `--alarms`
source. `--alarms` with `--changes` and no entry for it in `--changes-in`
starts with a warning: nothing in it will be changed.

The chart:

```yaml
rules:
  alarms: []            # a cloud's own alarms, beside any other place
  # - source: cloudwatch-prod
  #   url: https://monitoring.eu-west-1.amazonaws.com
serve:
  changes:
    in: []              # and SOURCE:NAME for alarms; NAME* for a prefix
    managed: []
    awsRole: ""         # rulers' and alarms'
    azureClientId: ""   # rulers' and alarms'
    googleServiceAccount: ""
    ociConfigSecret: {name: "", configKey: config, keyKey: key.pem}
                        # its key_file: /etc/lapilli/oci/key.pem
```

Its checks: each `rules.alarms` source named once, in recorded.md's form;
`SOURCE:` entries naming a source; the OCI Secret mounted only when changes
are on. Its README gives the four policies above.

## Alternatives

- **Writing alarms read from files.** A file is a copy, without the
  alarm's place or tags; the account or project it came from is a guess.
  Reading the API gives both.
- **A `for` or a threshold for alarms that aren't PromQL**, as fields.
  Easy to write and impossible to check: no backtest, against changes.md's
  promise. They stay a person's.
- **The SDKs' clients** (`service/cloudwatch`, Azure's `armmonitor`,
  Google's `monitoring/apiv3`, OCI's `monitoring`). Typed requests, but a
  typed client drops a field it doesn't know on the way in, and writes the
  alarm back without it on the way out, unseen. Plain requests with the
  answer as a tree let Lapilli see such a field and refuse. Only
  credentials and signing come from the SDKs.
- **A whole-resource write everywhere**, as CloudWatch needs. Azure, Google
  and OCI each take the field alone; writing less is less to get wrong.
- **Retiring by taking an alarm's notifications away** (CloudWatch's alarm
  mute rules, OCI's suppressions, Google's snoozes, an empty action group
  list), so it goes on being evaluated and its history says what it would
  have caught. That is silencing, which GOVERNANCE.md rules out.
- **Deleting an alarm Lapilli can't replay without any check**, or never
  deleting one. The first trusts nothing; the second leaves every cloud
  but CloudWatch with no end to retiring. The stand-in above is between,
  and says what it is.
- **Asking each cloud's deployment record** which tool made an alarm:
  CloudFormation's `DescribeStackResources` by an alarm's name, Azure's
  deployment history in its resource group, OCI Resource Manager's stacks
  in its compartment, Google's Infrastructure Manager. More to read and to
  test, on all four alike; left as a question.
- **Tying an alarm to its history by an ID** rather than a name: the
  recipes would label each firing with the alarm's ID, as OCI's carries
  `alarm_id`. Names that two alarms share would then tie too; it changes
  every source's labels once. Left as a question.

## Tests

- **No cloud can be run locally.** Azure, Google and OCI have no emulator
  of their alarm APIs. LocalStack's CloudWatch is in its plans from Hobby
  up, so CI would need a LocalStack account and token; its documentation
  says composite alarms aren't evaluated and only SNS actions are taken,
  and says nothing of PromQL alarms (`EvaluationCriteria`) or the JSON
  protocol. A stand-in written from the model checks more of what matters
  here, so CloudWatch isn't run in CI either.
- **Stand-ins at the documented shapes:**
  - CloudWatch, from botocore's `cloudwatch` 2010-08-01 model: the JSON 1.0
    protocol and `X-Amz-Target`, `DescribeAlarms` with `NextToken` and
    both alarm types, `PutMetricAlarm`, `DisableAlarmActions`,
    `EnableAlarmActions`, `DeleteAlarms`, `ListTagsForResource`, error
    bodies. The model's alarm shapes are kept as test data, and a test
    holds Lapilli's known fields to them.
  - Azure, from the REST specifications named above: lists with
    `nextLink`, `GET`, `PATCH` (200), `PUT` (200, 201), `DELETE` (200,
    204), and a metric alert with a PromQL criterion in the 2026-01-01
    shape.
  - Google, from the v3 reference: list with `nextPageToken`, `get`,
    `patch` honouring `updateMask`, `delete`, `create`.
  - OCI, from the OCI Go SDK's `monitoring` models: `ListAlarms` with
    `opc-next-page`, `GetAlarm` with an `etag`, `UpdateAlarm` answering
    412 to a stale `if-match`, `DeleteAlarm` (204), `CreateAlarm`. Its
    request signature is checked apart from the SDK, as SigV4 is now.
- SigV4 for `monitoring` is checked as `--auth aws` is now; Azure's token
  scope by host, as rulers.md; Google's impersonated token by the
  `generateAccessToken` request a stand-in sees.
- **The parsers** of the four answers join the weekly fuzzing.
- **A budget:** a source of 5,000 alarms read, parsed and kept as a
  version within a time the test sets, since a version keeps every
  alarm's document.
- The README and the chart's README say plainly that none of it has run
  against an account.

## Questions for review

- An alarm Lapilli can't replay is deleted four weeks after it was turned
  off with incidents nothing caught standing in for what it would have
  caught. Is that honest enough, or should retiring such an alarm stop at
  off, the deletion left to a person in the console?
- An alarm and its history are tied by the name its recipe gives it.
  Should the recipes label each firing with the alarm's own ID instead, as
  OCI's `alarm_id`, so names two alarms share tie too?
- Should turning off and deleting wait while the alarm fires (CloudWatch's
  `StateValue`, an open firing in the ledger), on this path and the
  others? Taking an alarm down during an incident is what changes.md
  fears most, approved or not.
- Should Lapilli ask each cloud's deployment record which tool made an
  alarm (CloudFormation by name, Azure's deployment history, OCI Resource
  Manager, Google's Infrastructure Manager), all four in one release?
- Should who last wrote an alarm count as another tool's, where the cloud
  keeps it (Google's `mutationRecord.mutatedBy`, Azure's `systemData`,
  OCI's `Oracle-Tags.CreatedBy` default tag), against principals the
  operator names? CloudWatch keeps none outside CloudTrail.
- On CloudWatch, Azure and OCI an alarm's text is one description; a
  runbook is a link inside it. Should a runbook be a form of its own that
  replaces the first link in the description?
- Should `--alarms` be read at an interval of its own? CloudWatch's
  `DescribeAlarms` returns 100 alarms a request, and each cloud has
  request quotas.
- OCI's write identity is an API key in a Secret. Should OCI wait for a
  keyless one (an identity domain's token exchange from the pod's service
  account token)?
- Needs an AWS account: that the live endpoint answers `DescribeAlarms`
  and `PutMetricAlarm` in JSON 1.0 as the model says; which `PendingPeriod`
  values a PromQL alarm takes; whether CloudFormation's own tags reach an
  alarm it makes; that IAM allows the write actions on alarm ARNs by
  prefix.
- Needs an Azure account: whether a `PATCH` of `description` or `enabled`
  asks the caller to read the `scopes` and action groups; whether `If-Match`
  is honoured for log search alerts; whether a PromQL metric alert's
  `scopes` is the workspace `--prometheus` queries.
- Needs a Google Cloud project: whether a policy off is still evaluated;
  whether Config Connector's label reaches an alerting policy; whether
  making a deleted policy again needs read on its notification channels.
- Needs an OCI tenancy: whether an alarm off is still evaluated; that
  `request.permission` conditions apply to `manage alarms`; that OKE's
  workload identity reads alarms.

### Settled for v0.24 (the maintainer's call, 2026-11-11)

- An alarm Lapilli can't replay is retired as far as off, never deleted
  from here: nothing stands in honestly for what it would have caught. The
  page says to delete it in the console once it has been off a while.
- An alarm and its history are tied by the alarm's own ID where its recipe
  labels it, on every cloud (each recipe gains the label), and by name only
  where no ID is given.
- Turning off and deleting wait while the rule or alarm fires (an open
  firing in the ledger, or the cloud's own state), on every path: the git
  path, Grafana, the rulers and the alarms alike.
- No deployment records and no last writer are asked; tags and
  `--changes-in` decide what another tool keeps.
- A runbook is a form of its own; on a cloud that keeps one description,
  it replaces the first link in it, or is added on a line of its own.
- `--alarms` is read at an interval of its own, `--alarms-every`
  (default 10m), so the clouds' request quotas hold.
- OCI writes with an API key in a Secret for now.
- What needs an account stays open for someone who has one.

## Building it

In this order, on the code rulers.md adds; each step leaves the tests
passing, and the write paths are reviewed as an attacker would before the
release.

1. **No record for what isn't replayed.** `internal/serve/views.go`:
   `ruleLines` lists among the rules that never fired only those replayed,
   or off; `internal/serve/work.go`: `spareRules` alike;
   `internal/serve/changes.go`: `proposeFound` passes the others over, and
   `propose` refuses retiring one with no record ("it isn't replayed and
   no history is tied to it"). Tests: a CloudWatch metric alarm read from
   a file, and a Grafana rule on Loki, never proposed for retiring. This
   fixes what v0.22 does, and can ship before the rest.
2. **Reading, also from files.** `internal/rules/rules.go`: Azure's PromQL
   metric alerts (`Microsoft.Azure.Monitor.PromQLCriteria`, one
   `StaticThresholdCriterion`), with test data in the 2026-01-01 shape;
   CloudWatch's `ActionsEnabled: false` as `Disabled`. Fuzz targets.
3. **One API path, not three.** `internal/serve/rulerwrite.go` from
   rulers.md becomes `apiwrite.go`: the steps above (read again, check,
   pause, `applying` with the backup, send, read back, undo) once, over a
   target with `read`, `allowed`, `managed`, `request`, `readBack` and
   `putBack`. The ruler kinds become targets; alarms are four more. Its
   tests stay as they are.
4. **Reading through the APIs.** `internal/source/alarms.go`, new:
   `AlarmsKind(url)`; a reader for each cloud on rulers.md's HTTP client
   (no redirects, limits); one document an alarm, the state left out;
   `Unit(id)` giving the alarm with its state, tags and version fields.
   OCI's reader calls `GetAlarm` for what is new or moved, and for all
   once an hour. `internal/cloud/cloud.go`: OCI's credentials and signer
   (the OCI Go SDK's `common` and `common/auth`, a new module); Google's
   impersonation through IAM Credentials' REST API; AWS's assumed role and
   Azure's Resource Manager scope as rulers.md built them. Test data from
   the documents named; fuzz targets.
5. **Flags for reading.** `cmd/lapilli/verdict.go` and `serve.go`:
   `--alarms`, its form, read beside the one other place; usage texts.
   `internal/serve/work.go`: `readRules` reads every place, and fails
   whole. The namespace set to the source.
6. **The tie.** `internal/ledger`: an unreplayed alarm read under a source
   is that source's recorded rule of the name its recipe gives it; `yield`
   doesn't name it apart; a name two alarms share ties neither; the record
   counted from the source's earliest firings. Tests, with the recipes'
   fixtures sent through the port.
7. **The path.** `internal/ledger/proposals.go`: `PathAlarm = "alarm"`.
   `internal/serve/changes.go`: `propose` gives it when allowed, with the
   table above deciding what each alarm is offered and why not;
   `internal/serve/mcp.go`: the path's description in `propose`.
8. **Writing.** `internal/serve/alarmwrite.go`, new: the four targets.
   CloudWatch: the `PutMetricAlarm` input from the read, the model's
   fields as a list in Go held to the model by a test, the actions and
   warm-up checks, composite alarms off and on only, an alarm a composite
   watches not deleted. Azure: `PATCH` of one field, `PUT` for an activity
   log alert's description, `If-Match`. Google: the masks, a policy of one
   condition for what is the policy's. OCI: `UpdateAlarm` with
   `if-match`, 412 as stale. Each: `--changes-in`, tags, undo, the
   deletion's check after four weeks (replay, its history, the stand-in).
   `internal/rules`: the change in each cloud's tree (`PendingPeriod`,
   `failingPeriods.for`, `duration`, a query's number, a description),
   read back as rulers.md's are.
9. **The page.** `internal/serve/changesweb.go`: the source and the alarm's
   ID on a proposal; why one is for a person; the stand-in said on a
   deletion's evidence; undo. `internal/serve/i18n/ko.json`: every new
   text.
10. **Flags for changes.** `cmd/lapilli/serve.go`: `SOURCE:` entries in
    `--changes-in`, `--changes-google-service-account`,
    `--changes-oci-config`, and their checks.
11. **The chart.** `values.yaml` as above; `deployment.yaml`, the
    arguments and the OCI Secret, mounted only with changes on;
    `_helpers.tpl`, the checks; the README, with the four policies.
12. **Unit tests**, each cloud against its stand-in: a `for` and a
    threshold on each PromQL kind, applied and read back; what it says on
    all four; off, and deleted four weeks later (by replay; by
    CloudWatch's history while off; by the stand-in, refused when an
    incident nothing caught shares a label); undone, a person's change
    since to its actions kept; made again after a deletion (a new ID for
    Google and OCI, the tie kept); the rule changed since, refused; only
    its actions changed since, made afresh; an unknown CloudWatch field,
    refused; an Auto Scaling or EC2 action, a warm-up, a composite alarm,
    an alarm a composite watches; a Google policy of two conditions; an
    OCI 412; a tag, a name not in `--changes-in`, refused at proposing and
    after approval; a 3xx, a `nextLink` or an ID elsewhere; unreadable
    after the write (`applying`, never sent again); taken by the cloud but
    reading as before for a round; paused just before the write; counted
    once; a source failing to read keeping the version.
13. **Documents.** README (where alarms come from, `--alarms`; changing
    them; what it doesn't do yet), the chart's README, recipes/firings'
    README (the tie), serve.md (what goes out), SECURITY.md (writes to a
    cloud's alarms are in scope), ROADMAP.md (step 3), changes.md's "As
    built", this document's own "As built", and the changelog, which says
    that an alarm with its actions off reads as off from this release, and
    that none of the four paths has run against an account.

## As built

As above, with the answers settled for v0.24, except:

- **Step 1 was shipped in v0.22.2** (a rule that isn't replayed left out of
  the rules that never fired, and not proposed for retiring); this step
  builds on it. What a rule has for a record is one question now
  (`recordOK`): replayed, or off, has one; an alarm Lapilli doesn't replay
  has its history's, once tied and copied in over twelve weeks without a
  gap up to now: the periods each write sent are kept, run together where
  they meet or overlap (the meta table's `copied.SOURCE`, the latest 64
  spans; no schema change), and the latest span must reach back twelve
  weeks and end within `--source-quiet` (26 hours by default), as a
  source that stopped is told of. A gap is weeks the alarm may have fired
  in unseen, and CloudWatch keeps only 14 days of history to copy in
  again. A ledger from before this keeps its source's last period alone:
  copies from then on, or sent again for the weeks before, make the
  record. A job copying weekly, with `--source-quiet` left at 26 hours,
  leaves retiring refused most of the week: give it the job's 8d. Any
  other rule has none, and retiring it is refused from its page too. A
  history whose severity changed is two recorded rules of one ID: the one
  at the alarm's severity now is its, and while the other fired within
  twelve weeks the alarm has no record (its record isn't one rule's).
  Every condition of a Google policy counts when its alerts are tied,
  PromQL ones too: a policy of a PromQL condition and another has no tie.
- **Tied by ID, but kept by name.** Each recipe labels its alerts with the
  alarm's ID (`alarm_id`), and a recorded rule whose alerts carry one ID is
  tied by it, one without any by its name. But a recorded rule is kept by
  the name its recipe gives it (`rules.sent`), and the schema doesn't
  change: two alarms of one name in one source are one recorded rule, its
  alerts carrying two IDs, and it ties neither. The settled aim, that names
  two alarms share tie too, isn't reached; the ID ties an alarm renamed in
  the console (Google's and OCI's display names change), and Azure's ids
  in any letter case. Two recorded rules tied to one alarm tie neither,
  unless one is under the alarm's name now: its history since a rename.
  Telling shared names apart would take recorded rules kept by ID, a
  change to the ledger left for a design of its own.
- **Retiring stops at off** for an alarm Lapilli can't replay, as settled:
  so the deletion's stand-in check (incidents nothing caught, sharing a
  label) and the check of CloudWatch's history while off aren't built. A
  PromQL alarm is deleted four weeks after it was turned off, its replay
  checked, as any rule; its history, if copied in too, stays apart, named
  apart, and isn't consulted. OCI's alarms are never PromQL, so Lapilli
  never deletes or makes one again: its policy has no `ALARM_DELETE` or
  `ALARM_CREATE`. Of Azure's, only a PromQL metric alert is ever deleted,
  so the custom role has `delete` on `metricAlerts` alone.
- **Waiting while it fires** asks the ledger (an alert of the rule with no
  end, replayed or copied in) on every path; on the alarm path, also an
  alert with no end in the alarm's source labelled `alarm_id` with its ID
  (Azure's in any letter case), as a PromQL alarm's history is named apart
  from its replay; and each cloud's own state, read just before the write
  with the reader's identity: CloudWatch's `StateValue`, with the alarm;
  Azure's Alerts Management (`2019-03-01`, `alertRule` the rule's id,
  `monitorCondition=Fired`, the last 30 days, `pageCount=250`; an alert
  someone closed doesn't fire, and one of a stateless rule fires only
  within its evaluation window from its start: an activity log alert's,
  five minutes, as "the alert condition for all stateless alerts is always
  fired"); Google's `projects.alerts.list` (`filter=state=open`, as
  Google's documents show it, and the policy, its project named by ID or
  by number); OCI's
  `ListAlarmsStatus` (`status=FIRING`). So whoever holds the firings token
  can't lift the wait by sending a window without the open alert. A read
  the cloud throttled (429), failed itself (5xx) or that timed out waits
  too, uncounted, asked again next round; any other failure fails the
  change rather than send it. A change waiting so stays approved,
  uncounted, says why, and says once it stopped.
- **What a proposal keeps.** `document` is the alarm as the change would
  leave it (its document, as `--alarms` reads it), not the request:
  the request is worked out from it and the alarm just read, when it is
  sent, as a ruler's is. The diff is between the two documents.
- **Just before the write**, CloudWatch's and Google's alarms are read
  once more, and their version and the whole alarm compared: moved, the
  change is read and checked again from the start once, then refused.
- **One API path with the rulers'**, and so their guarantees as v0.23
  built them: one change to a unit at a time (an alarm, a Google policy),
  another waiting while one sent isn't read back; read back only where it
  was sent, a restart that points `--alarms` elsewhere leaving it applying;
  an undo put back only over the fields the change wrote as it left them
  (every key, durations as lengths of time), and nothing else with them;
  an alarm that reads as it was after a write the cloud took read again
  for two minutes from when it was sent, by the clock, as a Mimir caching
  its storage is, then failed.
- **A Google policy made again** (undoing a deletion) has a new name, kept
  on the proposal from the create's answer, and is read back there; when
  the answer is lost, by finding, in a fresh read of the project's
  policies, the one that holds what was made (its shape, its names left
  out), and only when one alone holds it. The undo is refused when a
  policy of its display name is in the project again, as a person may
  have made it in the console meanwhile.
- **CloudWatch's tags** are kept with what a write replaces (the backup),
  so a deleted alarm is made again with them (`TagResource`).
- **An alarm a composite alarm watches** is found with `DescribeAlarms`'s
  `ParentsOfAlarmName`, asked when a deletion is proposed and carried out,
  rather than from the composite alarms last read.
- **Azure**: what is patched is the properties the change touched, each
  whole; an activity log alert's description is a `PUT` of the whole rule,
  refused when it has a field the specification (activityLogAlerts_API
  2026-01-01) doesn't. Azure's deny assignments and locks refuse a write
  themselves, and the change fails with Azure's answer.
- **What it says**: in a one-description cloud, the summary is the alarm's
  name and isn't changed here (its page says to rename it in the console);
  a description longer than CloudWatch's 1,024 characters isn't written.
  On Google Cloud a runbook with no link before is added as a link named
  `Runbook`.
- **OCI's credentials** are tried as OKE workload identity, a resource
  principal (both told by the environment OCI sets), an OCI config file
  where there is one, then an instance principal: the last asks the
  instance's metadata service, and waits when there is none, so a config
  file comes before it.
- **The stand-ins** live in `internal/alarmtest`, a package only tests
  import, shared by the readers' tests and the writers': CloudWatch's at
  botocore's `cloudwatch` model (kept, its alarm shapes, as test data, and
  a test holds Lapilli's field lists to it), Azure's at the REST
  specifications, Google's at its v3 reference, OCI's at the Go SDK's
  `monitoring` models, its request signatures checked with the key's
  public half apart from the SDK.

Tested: each cloud's path against its stand-in: a `for` and a threshold on
the PromQL kinds, what it says on all four, off, a PromQL alarm deleted
four weeks later and made again (CloudWatch with its tags, Google under a
new name), undone with a person's change since kept; the rule changed
since, refused; only the rest changed since, made afresh; written between
the read and the write, read again once, then refused; Azure's and OCI's
412; an Auto Scaling action, a warm-up, a composite alarm, one a composite
watches, a field the model hasn't; a Google policy of two conditions;
CloudFormation's tag, Config Connector's label, an OCI defined tag
`--changes-managed` names, a name `--changes-in` doesn't name, an ID of
another region, a host other than `--alarms`'s, a redirect; unreadable
after the write (applying, never sent again); taken but reading as before
for a round; paused just before the write; counted once; a read failing
anywhere keeping the version; 5,000 alarms read in a budget. Each guard of
the write path was taken out, one at a time, and a test failed, but for
two early-outs a later guard repeats: `proposeFound` passing over the
deletion of an alarm not replayed (`propose` refuses it), and
`--changes-in` asked of the alarm as last read (it is asked again of the
alarm just read, so a restart before the first read refuses nothing).
None of it has run against an account.

**What the reviews found.** Two reviews, one as an attacker would and one
for correctness, read the branch before release; each finding is fixed
and held by a test that fails without it (each fix was taken out and a
test failed):

- A notification of an alarm ties to its firing by `alarm_id`: CloudWatch's
  messages (SNS and EventBridge) and Google's incidents carry the alarm's
  ID, and an alert that carries none isn't held apart by the label.
- An undo is a write as any other: refused where the change it undoes
  would be now (`--changes-in`, a tool's tag, what is offered), and, for a
  deletion undone, where the alarm it makes again would be; a deletion
  undone that reads as gone still is read again for two minutes.
- A composite alarm's parents are asked with `ParentsOfAlarmName` alone,
  paged, and an error is the cloud's, not "watched".
- A Google deletion undone makes no second policy (above).
- Waiting while it fires reads each cloud's own state, and an alarm's
  history by its ID, so the firings token alone can't lift it (above).
- A tie prefers the history at the alarm's severity now; every condition
  of a Google policy counts, PromQL ones too; a record is up to now.
- A change waiting while it fires says so once, and that it stopped once
  it is carried out, not each round.
- An Azure description added where there was none is undone with an
  empty one, as a `PATCH` merges.
- A CloudWatch alarm is written only in its own account: the role's
  (`--changes-aws-role`), or the pod's own, as STS `GetCallerIdentity`
  says; CloudWatch names an alarm by its name, in the caller's account.
- A saved Azure answer read from a file keeps the key it had (no
  resource group), so no ledger gains a version for it on upgrade.
- The chart takes an OCI URL with `compartmentId` anywhere in its query.

A verification of those fixes, against the branch, found nine more, each
fixed and held so:

- Azure keeps a stateless rule's alerts `Fired` until someone closes them
  ("the alert condition for all stateless alerts is always fired"): an
  activity log alert's, and a metric or log search alert's that doesn't
  resolve itself (`autoMitigate`, or `resolveConfiguration`'s
  `autoResolved`, false). One fires now only within the rule's
  evaluation window from its start (an activity log alert's, five
  minutes); a closed one doesn't. Alerts Management is asked of the rule
  alone (`alertRule`), 250 a page, so a large subscription doesn't run
  past the page cap.
- An alarm whose severity changed was tied at its severity now and its
  record counted for that rule alone: a firing at the other severity read
  as silence. While the history held apart fired within twelve weeks,
  it has no record. A Google policy that isn't PromQL is tied by the
  severity it says, having no label of it.
- A gap in the copies read as silence: each write's period is kept now,
  and the record is the latest span without a gap (above).
- An undo of a deletion skipped the unknown-field guard: a CloudWatch
  alarm with a field this Lapilli doesn't know isn't deleted from here,
  and such a deletion isn't undone.
- A state read the cloud throttled (429), failed (5xx) or that timed out
  failed the change: it waits a round, uncounted.
- A Google policy's name names its project by ID in one place and by
  number in another: the policy alone is compared (`alertPolicies/ID`),
  in ties, the waits, and a notification's tie to its firing.
- One rule whose API couldn't be asked stopped the week's proposals: it is
  passed over, logged, as a rule with a proposal waiting is.
- Two Google policies alike after a lost answer could be closed only by
  saying the change didn't reach the cloud: they are closed as settled in
  the console.
- Without `--changes-aws-role`, a CloudWatch write asks STS for the pod's
  account first: a cluster that can't reach STS fails every CloudWatch
  write (the chart's README says so). A job copying weekly, with
  `--source-quiet` left at 26 hours, leaves retiring refused most of the
  week.

## Sources

- botocore, `botocore/data/cloudwatch/2010-08-01/service-2.json`, read on
  2026-11-11: `protocols` (`smithy-rpc-v2-cbor`, `json`, `query`),
  `targetPrefix`; `PutMetricAlarmInput`, `MetricAlarm`, `CompositeAlarm`,
  `PutCompositeAlarmInput`, `AlarmPromQLCriteria` (`PendingPeriod` 0 to
  86,400), `EvaluationInterval`, `WarmUpConfiguration`, `AlarmDescription`
  (1,024), `AlarmType` (`LogAlarm`); the documentation of
  `PutMetricAlarm`, `DescribeAlarms`, `DeleteAlarms`,
  `DisableAlarmActions` and `PutCompositeAlarm`.
- [Azure REST API specifications](https://github.com/Azure/azure-rest-api-specs),
  `monitor/resource-manager/Microsoft.Insights/Insights/stable`:
  `2026-01-01/metricAlert.json` (`PromQLCriteria`,
  `StaticPromQLCriteria`, `DynamicPromQLCriteria`, `QueryFailingPeriods`,
  `MetricAlertPropertiesPatch`, `nextLink`), `2026-01-01/activityLogAlerts_API.json`
  (`AlertRulePatchProperties`: `enabled` only),
  `2023-12-01` and `2026-03-01/scheduledQueryRule_API.json` (`etag`,
  `PATCH`, `nextLink`).
- [Google Cloud Monitoring: projects.alertPolicies](https://docs.cloud.google.com/monitoring/api/ref_v3/rest/v3/projects.alertPolicies)
  and [patch](https://docs.cloud.google.com/monitoring/api/ref_v3/rest/v3/projects.alertPolicies/patch):
  `updateMask`, conditions keeping their `name`, "single-thread API calls",
  no etag; documentation's limits; the PromQL condition's fields.
- [OCI Go SDK](https://github.com/oracle/oci-go-sdk), `monitoring`:
  `UpdateAlarmDetails`, `UpdateAlarmRequest` (`if-match`), `AlarmSummary`
  (no `body`), the client's paths.
- [OCI: Details for Monitoring](https://docs.oracle.com/en-us/iaas/Content/Identity/Reference/monitoringpolicyreference.htm):
  the verbs for `alarms`, and the permissions each operation needs.
- [LocalStack: CloudWatch](https://docs.localstack.cloud/aws/services/cloudwatch/):
  its plans, composite alarms not evaluated, SNS actions only.
- [Amazon CloudWatch now supports tagging alarms with AWS CloudFormation](https://aws.amazon.com/about-aws/whats-new/2024/04/amazon-cloudwatch-tagging-alarms-cloudformation),
  and [Config Connector: resources](https://cloud.google.com/config-connector/docs/concepts/resources)
  (`managed-by-cnrm`).
