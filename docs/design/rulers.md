# Changing rules in a managed Prometheus ruler

Status: proposed, for v0.23.0: the second step of
[changes.md](changes.md#building-it). The rules a Mimir or Cortex ruler,
Amazon Managed Service for Prometheus or Azure's Prometheus rule groups
hold are changed through their own APIs, once a person approves. It adds a
way of reading rules and three places Lapilli writes to, so under
[GOVERNANCE.md](../../GOVERNANCE.md) it is designed first, as a pull
request. It keeps everything changes.md settles and says only what is new.
Google Cloud's rule resources stay on the git path; why is below.

It is in a document of its own because it is mostly about four APIs, how
each is read and written, and their tests; changes.md stays about
proposals, approval and safety, which this doesn't change.

What it says of the APIs was checked against Mimir 3.2.4 run locally (the
answers quoted below are its own), Cortex's API reference, Amazon's API
model (botocore's `amp`, 2020-08-01) and Azure's REST specification
(`PrometheusRuleGroups`, 2023-03-01). None of it has been run against an
AWS or Azure account.

## Why

Step 1 changes rules kept in git, by a pull request, and rules kept only in
Grafana, through its API. A managed ruler holds rules of both kinds:

- **Kept nowhere else.** Grafana's alerting UI writes a Mimir or Cortex
  data source's rules straight to its ruler (Grafana calls them data
  source-managed rules). Amazon's console edits a rule groups namespace in
  place. Azure's portal edits a rule group, and AKS's recommended alerts
  make rule groups that no repository holds.
- **Kept somewhere else.** `mimirtool rules sync`, Terraform, Bicep,
  CloudFormation and Alloy's `mimir.rules.kubernetes` write rules into a
  ruler from git or from a cluster. A change made in the ruler is undone by
  them, or fights them. For those, the git path stays the way, as
  changes.md says of any rule another tool keeps.

Today a proposal for a rule read from a ruler is for a person: a diff and a
pull request's description, to copy. This step applies the ones a person
approved, for the rules kept nowhere else, and only in the namespaces or
rule groups the operator names.

## What stays as changes.md has it

- One approval by default (`--approvals 2` for two), by an approver, on the
  proposal's page, in a browser; never through the API, MCP or a token. No
  one approves their own proposal, or one their agent made. Agents propose,
  behind `--mcp-propose`, and never approve or apply.
- Off unless `--changes`. The write credentials aren't read without it.
- One unfinished proposal for a rule at a time, of any kind, undo included.
  A proposal touches one rule.
- The limits (`--changes-limits`, five deletions a day and twenty changes
  an hour) count a change once, when it is sent. The pause (the admin's
  switch, `--changes-paused`) is looked at again just before each write.
- Before anything is sent, the rule is read again from where it is kept,
  and the backtest is run again; what it replaces is kept for undo. What
  became of a change is read back, never taken from the answer; a change
  that can't be read back stays `applying`, is read again each round, and
  is never sent again.
- Every step is kept in the ledger's changes and told: `change.applied`,
  `change.refused`, `change.failed`, `change.undone`.

## How each is read today

None of them can be written from what Lapilli reads today:

| Ruler | Read today | Namespace and group known? | Why it can't be written from that |
|---|---|---|---|
| Mimir, Cortex | `--loaded-rules`: `GET /prometheus/api/v1/rules` | The answer has it: each group's `file` is its namespace (Mimir 3.2.4: `"file":"payments"`). `rules.FromAPI` drops it, so two groups of one name in two namespaces read as one | The answer is the rules as loaded, the expression printed again: Mimir keeps `scrape_duration_seconds{job="mimir"}>0.000001` and answers `... > 0.000001`. A threshold can't be found in the text written, and what a write replaces isn't what was read |
| Amazon Managed Service for Prometheus | `--loaded-rules --auth aws` (`aps:ListRules`, whose groups have `file` too, and which pages past 100,000 rules with `groupNextToken`, not followed yet); or a file of `aws amp describe-rule-groups-namespace`'s answer, with `--rules` | Through the API, as for Mimir. From the file, the group only: the namespace's name is read, then dropped, and the rule's source is the file's path | As for Mimir; and a file is a copy, not the namespace |
| Azure's Prometheus rule groups | Files only, with `--rules`: an ARM template, or what the Azure CLI or REST API gives, saved | The rule group's name is the rule's group. Its resource group and subscription are nowhere | A file is a copy, and the rule group's resource isn't named |
| Google Cloud's `Rules`, `ClusterRules`, `GlobalRules` | Files, with `--rules`: manifests, or `kubectl get ... -o yaml` | The group, and for `Rules` the namespace, which scopes the expression | Manifests are the git path's already (below) |

So this step adds a way of reading a ruler that keeps the place: the rules
as written, from the API that writes them, one document a namespace or rule
group.

## Reading a ruler: `--ruler URL`

One flag, for `lapilli verdict` and `lapilli serve`, beside `--rules`,
`--loaded-rules` and `--grafana` (pick one). The URL's host says which API
it is:

| `--ruler` | Read with | One document is |
|---|---|---|
| Mimir: `http://mimir:8080/prometheus/config/v1/rules`; Cortex: `http://cortex:9009/api/v1/rules`; any ruler with that API, at the base it is served under | `GET URL`: a YAML map of every namespace to its groups, as written. Mimir answers `{}` when there are none; a 404 is taken as none too | a namespace: a rule file of its groups, `groups:` and the list as the ruler gave it |
| Amazon: `https://aps.REGION.amazonaws.com/workspaces/WORKSPACE_ID` (the control plane, not the `aps-workspaces` query URL, which is refused with the URL to give) | `ListRuleGroupsNamespaces` (paged by `nextToken`), then `DescribeRuleGroupsNamespace` for each namespace whose `modifiedAt` moved since the last read | a namespace: its `data`, the rule file as it was uploaded, comments and all |
| Azure: `https://management.azure.com/subscriptions/SUB` or `.../subscriptions/SUB/resourceGroups/RG` | `GET .../providers/Microsoft.AlertsManagement/prometheusRuleGroups?api-version=2023-03-01`, following `nextLink` | a rule group: the resource as listed |

- **Credentials, to read.** Mimir and Cortex: `--ruler-token-file`, a bearer
  token that may read rules (an access policy's `rules:read`), and
  `--tenant ID`, sent as `X-Scope-OrgID` to `--ruler` and `--prometheus`
  alike, for a ruler that has tenants and no gateway to set it. Amazon:
  `--auth aws`, the pod's own identity, with
  `aps:ListRuleGroupsNamespaces` and `aps:DescribeRuleGroupsNamespace`.
  Azure: `--auth azure`, with a token for Azure Resource Manager
  (`https://management.azure.com/.default`; the scope is told by the host,
  as the query endpoint's is now) and a role that reads
  `Microsoft.AlertsManagement/prometheusRuleGroups`.
- **Where a rule is.** A rule gains a namespace: the ruler's namespace
  (Mimir, Cortex, Amazon), or the rule group's resource group (Azure). Its
  group is the group (or the rule group), its source the document's: the
  namespace's URL under `--ruler`, the namespace's ARN, or the rule group's
  resource ID. A proposal keeps all three, and its page shows them.
- **Telling rules apart.** A rule's key takes its namespace only when it has
  one, so every rule read as Lapilli reads it now keeps its key, and no
  ledger sees a new version of its rules on upgrade. Rules of one name in
  two namespaces are told apart by namespace and group:
  `ScrapeSlow [payments/shop]`. The namespace is kept with each document in
  a version, so a version read back from the ledger reads alike.
- **How often.** Each `--watch`, as `--grafana` is read. A ruler keeps no
  history of its rules: `--rules-history` is for files.

`--loaded-rules` stays as it is: read only, its proposals for a person. With
Mimir, Cortex or Amazon, `--ruler` reads the same rules, as written.

## The ruler path

A proposal for a rule read through `--ruler` takes a new path, `ruler`,
beside `git`, `grafana` and `none`, when:

- `--changes-in` names its namespace (Mimir, Cortex, Amazon) or rule group
  (Azure), and
- nothing on the namespace or rule group says another tool keeps it (below),
  and
- the kind can be made there (the table below).

Otherwise it is for a person, as now, and its page says why ("its namespace
isn't one `--changes-in` names"). All three are asked again when the change
is carried out: a flag changed by a restart, or a tag added since, refuses
it then.

`--changes-in LIST`: namespaces or rule groups, comma-separated, each a name
or a prefix ending in `*` (`team-*`); an Azure rule group as `RG/NAME`, or
`NAME` when `--ruler` names one resource group, compared in any letter case
as Azure compares names. Without it nothing in a ruler is changed. This is
the opt-in the rest leans on: a ruler can't say which of its rules a tool
keeps, so the operator says which it doesn't.

## Each ruler

|  | Mimir, Cortex | Amazon | Azure |
|---|---|---|---|
| A rule is | namespace, group, `alert` | namespace, group, `alert` | resource group, rule group, `alert` |
| What is written | the group: `POST BASE/NS` with its YAML (`Content-Type: application/yaml`) | the namespace: `PutRuleGroupsNamespace` with the whole file | the rule group: `PUT` of the whole resource |
| Read again and hashed | `GET BASE/NS/GROUP` | `DescribeRuleGroupsNamespace`, its `data` | `GET` on the resource, but for what Azure sets (`id`, `name`, `type`, `systemData`) |
| A `for`, a threshold, what it says | yes | yes | yes; `for` written as Azure writes it (`PT10M`) |
| Turning off | no: the rule format has none | no | `enabled: false` on the rule |
| Retiring | deleting | deleting | turning off, then deleting four weeks later |
| A group's last rule deleted | the group: `DELETE BASE/NS/GROUP` | the group is cut from the file | refused: a person deletes the rule group |
| A namespace's last rule deleted | the group, as above | refused: a person deletes the namespace | (as above) |
| Answers | `202`; `400` with why, for a group it won't take | `202`, then the namespace is `UPDATING` until `ACTIVE` or `UPDATE_FAILED` | `200` or `201` |
| Another tool's | `--changes-in` alone | `--changes-in`; its tags | `--changes-in`; its tags |
| Write credentials | `--changes-ruler-token-file` | `--changes-aws-role` | `--changes-azure-client-id` |

A group goes with its last rule, as in a file. An Amazon namespace or an
Azure rule group never does: each is a resource with tags and permissions of
its own, which a person deletes.

### Mimir and Cortex

The API writes one group at a time and has no condition on a write: no
ETag, no version (Mimir 3.2.4 sends none). `BASE` is `--ruler`, and the
namespace and group go into the path escaped a segment each: Mimir takes a
namespace with slashes in it as `alloy%2Fpayments%2Frules`. A missing group
answers 404 (`group does not exist`), a missing namespace `{}`.

A `202` means the group is stored. The ruler loads it at its next poll
(`-ruler.poll-interval`, a minute by default); Lapilli sees it then as a new
version, and the outcomes page measures it from there. Mimir checks a
group when it is posted and answers `400` with why for one it won't take
(an expression or a template that doesn't parse, more rules than the
tenant may have); the change fails with that reason.

### Amazon Managed Service for Prometheus

The namespace is written whole, as its `data` (base64), with a
`clientToken` made of the proposal's number and the attempt, so a request
repeated by the SDK after a timeout isn't taken twice. The change is made in
the file as it was uploaded, on the lines the rule is on, as the git path
makes it, so comments and the other groups stay byte for byte.

A namespace is written only when it is `ACTIVE`. One `CREATING`,
`UPDATING` or `DELETING` waits, approved, to the next round, uncounted; one
`CREATION_FAILED` or `UPDATE_FAILED` is refused, with Amazon's
`statusReason`. After the write it is `UPDATING`: the change stays
`applying` until the namespace is `ACTIVE` and reads as written (applied),
or `UPDATE_FAILED` (failed, with the reason).

Requests are signed with SigV4 for the service `aps`, for the region in the
URL's host, which may be a VPC endpoint's
(`vpce-...aps.REGION.vpce.amazonaws.com`).

### Azure Monitor managed service for Prometheus

A rule group is an Azure resource; `PATCH` changes only its tags and the
whole group's `enabled`, so a rule is changed by a `PUT` of the whole
resource: as it was read, `location`, `tags`, `properties` (its `scopes`,
`clusterName`, `interval`, `actions` and every other rule) and all, with
the one rule changed. Leaving out `tags` would drop them. The specification
documents no ETag for this type, so, as with Mimir, nothing stops another
write landing between Lapilli's read and its `PUT`.

Turning a rule off is its own `enabled: false`. The group's `enabled` is
never touched: it turns off every rule in it.

The document is JSON, which the text edit doesn't take (it edits block
YAML), so the change is made in the decoded tree, as `rules.Check` already
makes it to compare: the same `for`, number, annotation or `enabled`, in
Azure's names (`expression`, an ISO 8601 `for`).

## Google Cloud Managed Service for Prometheus: the git path

Lapilli doesn't write `Rules`, `ClusterRules` or `GlobalRules` through the
Kubernetes API. Their changes stay pull requests:

- **Where they are kept.** They are Kubernetes resources, applied by
  `kubectl`, Helm, Argo CD, Flux or Config Sync from manifests. Google Cloud
  has no console or API that keeps them outside a cluster, as Grafana's UI,
  Amazon's console and Azure's portal keep theirs. A rule resource changed
  in the cluster is put back by the next sync, or drifts: the case
  changes.md sends to git.
- **The git path already takes them.** The edit finds `spec.groups` in any
  resource. Checked: `rules.EditDoc` writes a new `for`, and
  `rules.EditThreshold` a new number, into a `Rules` resource, its comment
  and the rest of the file as they were; no test holds that yet, and one is
  added.
- **Writing them would make Lapilli a controller.** It would need a
  Kubernetes client, a service account with RBAC to patch them, and access
  to each cluster. serve.md says it "doesn't watch the Kubernetes API and
  needs no RBAC"; ROADMAP.md doesn't plan a controller.
- **Every cloud alike.** `PrometheusRule` resources on EKS, AKS, GKE or OKE
  are not written through the Kubernetes API either. The line is the same
  on every cloud: rules in a cluster go through git; rules an API keeps go
  through that API. Google Cloud's rules that an API keeps are Cloud
  Monitoring's alerting policies with a PromQL condition, which step 3
  changes through `alertPolicies.patch`, as it does CloudWatch's PromQL
  alarms.

Rules read back from a cluster (`kubectl get ... -o yaml`) stay as they are:
a diff for a person.

When this is built, ROADMAP.md's "Not planned" line on a controller reads
"... and changes nothing in a cluster: a rule resource is changed by a pull
request", in place of "changes none but a rule resource an approved change
names". GOVERNANCE.md's principle ("never ... writes to Prometheus or a
cluster beyond the rules it was told it may change") holds as written.

## Checked again, written, read back

What a write replaces is the unit: a Mimir group, an Amazon namespace, an
Azure rule group. When a proposal is made, Lapilli keeps the rule's hash
(`before_hash`, as now), the unit's hash (`doc_hash`), the unit as the
change would make it (`document`) and the diff. The unit's hash is of its
decoded tree, written out as JSON with its keys in order, so that a ruler
writing the same group out with other indentation or quoting is the same
unit; Amazon's is of the file's bytes, since Amazon keeps them.

When it is carried out:

1. **The unit is read again** from the ruler, not from the ledger.
   - Gone: refused.
   - Its namespace or rule group no longer allowed, or now tagged as
     another tool's: refused, with why.
   - The rule doesn't hash to `before_hash`: refused as stale ("the rule
     changed in the ruler since it was proposed: propose it again").
   - The unit hashes to `doc_hash`: what is written is `document`, as
     approved.
   - Otherwise only other rules in the unit changed. The change is made
     afresh in the unit as it is now, as the git path makes it afresh in a
     file, and read back before it is sent: every other rule as it now is,
     this one as proposed. The new unit, hash and diff are kept on the
     proposal. A person approved a change to one rule; the others are
     written as the ruler held them a moment before.
2. **The backtest is run again**, as now.
3. **The pause is looked at** once more. The proposal is kept as
   `applying`, with the unit as just read (the backup) and counted against
   the limits, before anything is sent.
4. **It is sent**, to the place `--ruler` names and nowhere else (Security).
5. **It is read back**, at once and then each round until it can be:
   - the rule reads as proposed (or, deleted, isn't there), and every
     other rule as in the backup: applied;
   - the rule reads as in the backup: failed, with the ruler's answer, or
     for Amazon, still `applying` while the namespace is `UPDATING`;
   - anything else (someone else wrote between the read and the write):
     failed, saying it reads as neither, with the backup kept for a
     person;
   - unreadable: still `applying`, read again each round, never sent again.

The time between the read in step 1 and the write in step 4 is a request
or two. No ruler offers more: Mimir, Cortex and Amazon have no condition on
a write, and Azure documents none for rule groups.

## Undo

An approver undoes an applied change from its page, as now, by a proposal
of its own on the ruler path. It puts back the rule as it was kept, not the
whole unit: other rules in the group may have been changed since, by people
or by other proposals, and writing the unit back whole would undo them too.

- The rule must read as the change left it (its hash is the change's
  `after_hash`); for a deletion, no rule of its name may be in its group
  again. Otherwise it is refused ("changed since: undo it in the ruler").
- The rule's entry is taken from the backup and put in its place in the
  unit as it is now: in place of the changed one, or back where it was,
  among the rules as they are now, for a deletion. A group that went with
  its last rule (deleted in Mimir, cut from Amazon's file) is put back as it
  was kept.
- It is then checked, sent and read back as any change is: the unit as it
  was just read, but for that one rule.

The backup is kept for as long as `--keep`, as Grafana's is.

## Retiring

- **Mimir, Cortex, Amazon:** the rule format has no off, so retiring is the
  deletion, as in a rule file. Unlike a rule file, nobody reviews it again
  after the approval: it is kept for undo, counted against the five
  deletions a day, and refused when checked again if the rule caught
  something since that no other rule did, as now.
- **Azure:** turning off first (`enabled: false`); deleting no sooner than
  four weeks later, approved again, only if the rule is still off and
  nothing it would have caught was missed meanwhile, as for Grafana. A rule
  turned off by hand, in the portal, is for a person to delete there. A
  rule in a rule group that is off as a whole is off already.

## Rules another tool keeps

A change made in a ruler around the tool that keeps the rule is undone by
it at its next run, or fights it. How each ruler can tell:

- **Mimir, Cortex:** they can't. A group carries no annotations, and no
  label that marks a tool. `mimirtool rules sync` and the Terraform
  providers leave no mark. Alloy's and Grafana Agent's
  `mimir.rules.kubernetes` name their namespaces under a prefix (`alloy` by
  default, `/` between the parts) and read the ruler's state again every
  `sync_interval`, but the prefix is a setting anyone changes. So only
  `--changes-in` decides; the documentation says not to name a namespace a
  tool syncs.
- **Amazon:** a namespace has tags. One carrying a tag in
  `--changes-managed` (`KEY=VALUE`, or `KEY=*` for any value) is refused,
  and so is one with `aws:cloudformation:stack-id`, which CloudFormation
  sets on the resources it makes that take tags. Terraform sets no tag of
  its own; its
  `default_tags` are what the operator names in `--changes-managed`.
- **Azure:** a rule group has tags, as above; Bicep, ARM templates and
  Terraform set none of their own. A rule group under a deployment stack
  that denies writes, or a `ReadOnly` lock, is refused by Azure itself: the
  write fails with Azure's answer, and reads back as it was.

## Security

What a stolen Lapilli could do grows by this: rewrite or delete alert rules
in the namespaces or rule groups of one ruler. It is held so:

- **One credential a target, for writing only, read only with
  `--changes`:**
  - Mimir, Cortex: `--changes-ruler-token-file`, a token that may write
    rules (`rules:write`); reads use `--ruler-token-file`'s. Without it,
    writes are sent with no token, for a ruler reached only inside the
    cluster. A ruler's token reaches every namespace of its tenant:
    `--changes-in` is Lapilli's own check, and the documentation says so.
  - Amazon: `--changes-aws-role ARN`, a role the pod's identity assumes for
    writes only, allowed `aps:PutRuleGroupsNamespace` and
    `aps:DescribeRuleGroupsNamespace` on the ARNs of the namespaces named
    (`arn:aws:aps:REGION:ACCOUNT:rulegroupsnamespace/WORKSPACE_ID/NAME`).
    IAM then holds the line even against Lapilli's own code. Without it,
    the pod's identity, which then needs those too.
  - Azure: `--changes-azure-client-id ID`, a second user-assigned identity
    federated with the same service account, given a custom role with
    `Microsoft.AlertsManagement/prometheusRuleGroups/read` and `/write`,
    assigned on each rule group named (Azure assigns roles on a single
    resource). It needs no `delete`: Lapilli never deletes a rule group.
    Without it, the pod's identity.
- **Only where it was told.** Every request goes to the host in `--ruler`
  (and, for `--changes-aws-role`, to AWS STS, through the SDK, as the
  credential chain does now), follows no redirect (a 3xx is an error), and
  reads at most 64 MB for a list and 4 MB for a unit, within 30 seconds,
  as Grafana's are.
  - Names read from a ruler are data. Each goes into the path as one
    escaped segment; a name that is empty, `.` or `..`, has a control
    character or is longer than 1,024 bytes is refused, and an Amazon
    namespace's name longer than its API's 128.
  - Azure's `nextLink` is followed only when it is https on the same host;
    a rule group's `id` is written to only when it is under `--ruler`'s
    scope and of the form
    `/subscriptions/S/resourceGroups/RG/providers/Microsoft.AlertsManagement/prometheusRuleGroups/NAME`,
    compared in any letter case.
  - Amazon's host must be `aps` in a region (`awsHost`, which knows FIPS,
    dual-stack and VPC endpoint hosts); the region signed for is the
    host's. Azure's must be Resource Manager's: `management.azure.com`,
    `management.usgovcloudapi.net` or `management.chinacloudapi.cn`.
- **A proposal is written where it was read.** The place is worked out
  again from `--ruler` and the proposal's namespace and group, and must be
  the proposal's source: one made against another ruler, before a restart
  changed the flag, is refused.
- **Never from outside.** Nothing the notifications or incidents ports
  take becomes a write; the changes token can't see ruler proposals, which
  are no recipe's.
- **Told and kept.** Every write is a `change.applied` (or `.failed`) to
  the webhook and Slack, with what it replaced in the ledger.
- **Reviewed as an attacker would** before release, the write paths first,
  as CONTRIBUTING.md asks.

## Options

- `--ruler URL`, `--ruler-token-file FILE`, `--tenant ID`: reading, for
  both commands.
- `--changes-in LIST`: the namespaces or rule groups Lapilli may change.
- `--changes-managed KEY=VALUE`: tags that mean another tool keeps a
  namespace or rule group (repeatable), as changes.md planned; Amazon and
  Azure.
- `--changes-ruler-token-file FILE`, `--changes-aws-role ARN`,
  `--changes-azure-client-id ID`: one a target. Each needs `--changes` and
  `--ruler` of its kind, and `--changes-in` needs `--ruler`. `--ruler` with
  `--changes` and no `--changes-in` starts with a warning: nothing in the
  ruler will be changed.

The chart:

```yaml
prometheus:
  tenant: ""            # X-Scope-OrgID, for prometheus.url and rules.ruler.url
rules:
  ruler:                # a fourth place to read rules from, alone
    url: ""
    tokenSecret: {name: "", key: token}
serve:
  changes:
    in: []              # namespaces, or RG/NAME rule groups; NAME* for a prefix
    managed: []         # KEY=VALUE tags that mean another tool keeps one
    rulerTokenSecret: {name: "", key: token}   # Mimir, Cortex
    awsRole: ""         # Amazon
    azureClientId: ""   # Azure
```

Its checks: one place to read rules from; `serve.changes.in` needs
`rules.ruler.url`; the write Secret mounted only when changes are on. Its
README gives the IAM policy and the Azure role as JSON.

## Alternatives

- **Reading with `--loaded-rules` and writing the configuration API.**
  Mimir's and Amazon's `/api/v1/rules` name each group's namespace, so no
  new flag. But they give the rules as loaded, the expression printed
  again: a threshold isn't found in the text written, what a write
  replaces isn't what was read, and Azure has no such API.
- **Writing a Mimir namespace whole.** The API writes a group at a time,
  so a namespace would be several requests, with no way to make them one.
- **Refusing a change when anything else in its unit changed.** Simpler to
  say. But after the first of a week's proposals for one group is applied,
  every other one for that group is refused and must be made again, and
  for Amazon the unit is a namespace of many groups. The rule itself must
  be as proposed; the rest is written as just read.
- **Undo writing the unit back whole,** as the git path writes a file back.
  It would undo whatever was changed in the group since, by people and by
  other proposals, with no review to catch it.
- **An off for rulers, by rewriting the expression**
  (`(EXPR) unless on() vector(1)`): a rule that never fires and can be put
  back, four weeks later. It is a different rule on every page and in
  every tool that reads it. A deletion with the rule kept for undo does the
  same, and says what it is.
- **Writing Google Cloud's rule resources through the Kubernetes API.**
  Above.
- **Telling a sync tool's namespaces by name alone** (`alloy/`). A default
  anyone changes; mimirtool and Terraform leave no mark at all.

## Tests

- **A real Mimir, in CI.** `test/e2e/changes.sh` starts
  `grafana/mimir:3.2.4` (digest
  `sha256:e4ff85d922c7c0a5131576d2659a1b98b3abfd73c92448dccf8ad3a6049a7ed8`),
  in one process with storage on its filesystem, the Prometheus it already
  runs writing to it, and a third `lapilli serve` reading it with
  `--ruler`. This configuration was run: the ruler took groups, answered
  as quoted here, and wrote `ALERTS` for a rule on what Prometheus sent it.

  ```yaml
  multitenancy_enabled: false
  target: all
  server: {http_listen_port: 9009, log_level: warn}
  common:
    storage: {backend: filesystem, filesystem: {dir: /data/common}}
  blocks_storage: {storage_prefix: blocks, tsdb: {dir: /data/tsdb}}
  ruler_storage: {storage_prefix: ruler}
  ruler: {rule_path: /data/ruler-tmp, poll_interval: 10s}
  ingester: {ring: {replication_factor: 1}}
  store_gateway: {sharding_ring: {replication_factor: 1}}
  ```

- **Cortex** is the same API under another base. It is tested against a
  stand-in answering as Mimir did, at Cortex's paths.
- **Amazon and Azure** are tested against stand-ins answering as their
  documents show: Amazon's from botocore's `amp` model (paths, `202`s,
  `statusCode` values, base64 `data`, `nextToken`), Azure's from the REST
  specification (list with `nextLink`, `PUT` answering `200` or `201`).
  SigV4 is checked as `--auth aws` is now, apart from the SDK; Azure's
  token scope by host. Neither is run against an account, and the README
  says so.
- **The parsers** of the three answers join the weekly fuzzing.

## Questions for review

- A change whose unit changed elsewhere since it was proposed is made
  afresh, the rule itself as proposed. Should the ruler path refuse it
  instead, since nobody reviews it again?
- Deleting through a ruler has no review after the approval, unlike a
  deletion in a pull request. Should a deletion on this path take two
  approvals even when `--approvals` is 1?
- Google Cloud gains nothing written through an API in this step, because
  it keeps its managed Prometheus rules only in clusters. Should its
  PromQL alerting policies come forward from step 3, so that each cloud
  gains one in the same release?
- Should `--loaded-rules` keep `file` as the namespace too, so that groups
  of one name in two Mimir namespaces are told apart when only read? It
  would give every `--loaded-rules` ledger one new version on upgrade, as
  Prometheus's `file` is a path.
- Grafana Cloud's and Grafana Enterprise Metrics' gateways: does a bearer
  token do, or does `--ruler` need basic auth (an instance ID and a token)?
- Should a namespace under Alloy's or Grafana Agent's default prefix be
  refused even when `--changes-in` matches it by a prefix pattern, and let
  in only by its exact name?
- Needs an AWS account: what `data` reads as after `UPDATE_FAILED`, the old
  file or the new one (the design calls it failed either way and never
  sends it again); that IAM grants `aps:PutRuleGroupsNamespace` on one
  namespace's ARN, as the design assumes; and whether CloudFormation's own
  tags reach a rule groups namespace it makes.
- Needs an Azure account: whether a `PUT` asks the caller to read the
  workspace in `scopes` and the action groups in `actions`; whether ARM
  honours `If-Match` for this type though the specification shows no ETag;
  and whether a newer api-version than 2023-03-01 should be used.
- Is `--tenant` part of this step, or a change of its own? Any Mimir with
  tenants and no gateway needs it to be read at all.
- Should CI run Cortex too, or are the Mimir run and a stand-in at
  Cortex's paths enough?

### Settled for v0.23 (the maintainer's call, 2026-11-11)

- A unit changed elsewhere is made afresh, the rule itself as proposed, as
  the git path does; the rule itself changed since is refused as stale.
- A deletion takes the approvals `--approvals` asks for, as on the other
  paths.
- Google Cloud's PromQL alerting policies stay in step 3, with every
  cloud's own alarms.
- `--loaded-rules` keeps no namespace: only rules read with `--ruler` do,
  so no ledger gains a version on upgrade.
- `--ruler-tenant` (sent as `X-Scope-OrgID`) is part of this step; basic
  auth for Grafana Cloud's gateway isn't.
- A namespace is let in only as `--changes-in` names it; no prefix is
  refused or let in by default.
- CI runs Mimir; Cortex is tested against a stand-in at its paths.
- What needs an AWS or Azure account stays open for someone who has one.

## Building it

In this order; each step leaves the tests passing, and the write paths are
reviewed as an attacker would before the release.

1. **A rule's namespace.** `internal/rules/rules.go`: `Rule.Namespace`;
   `Key` takes it only when set, with a test that a rule without one keeps
   today's key byte for byte. `Doc.Namespace` (JSON, `omitempty`), set on
   each rule by `FromDocs`, so a version read back from the ledger reads
   alike. `internal/rules/edit.go`: `Edit.Namespace`, kept in a
   proposal's evidence. `internal/source/source.go`: `TellApart` counts the
   namespace with the group. No change to the ledger's schema: documents
   and evidence are JSON already.
2. **Reading.** `internal/source/ruler.go`, new: `RulerKind(url)` (config
   API, Amazon, Azure, or an error naming the URL to give); a reader for
   each, with one HTTP client that follows no redirect; `Read` giving the
   rules and one document a namespace or rule group; `Unit(ns, group)`
   giving one unit as raw bytes, its hash, its rules, whether it is gone,
   and Amazon's status and tags, Azure's tags. Amazon's reader keeps each
   namespace's `modifiedAt` to describe only those that moved.
   `internal/cloud/cloud.go`: Azure's scope told by host (Resource
   Manager or the query endpoint); a signer for a plain `http.Request`, not
   only a `prom.Option`; an assumed role for `--changes-aws-role`
   (`credentials/stscreds` and `service/sts`, in `go.sum` already); an
   Azure credential for a client ID. `internal/prom/prom.go`: a header
   option, for `--tenant`. Test data: Mimir 3.2.4's answers as recorded
   here, Amazon's and Azure's from their documents. Fuzz targets for the
   three answers.
3. **Flags for reading.** `cmd/lapilli/verdict.go` and `serve.go`:
   `--ruler`, `--ruler-token-file`, `--tenant`, `--ruler` excluding the
   other places to read rules from; usage texts. `internal/serve/work.go`:
   `readRules` reads `--ruler`; `internal/serve/serve.go`: the `Config`
   fields.
4. **The path.** `internal/ledger/proposals.go`: `PathRuler = "ruler"`,
   taken by `Propose`; `applying` documented for rulers too. No schema
   change: `path` is text, checked in Go. `internal/serve/changes.go`:
   `propose` gives the ruler path when allowed, and the reason
   (`evidence.NoPath`) when not; a disable and a delete after it are for
   whatever can turn a rule off (Grafana, Azure), not only a rule with a
   uid, and "off" is `Paused` or `Disabled`; `checkAgain` leaves the ruler
   path's reading to where it is applied, as Grafana's; `carry`,
   `settleApplying` and an admin's closing go by path; `seen` and
   `seenSays` pass the ruler path over, as Grafana's. `internal/serve/mcp.go`:
   the path's description in `propose`.
5. **Writing.** `internal/serve/rulerwrite.go`, new, after
   `grafanawrite.go`: `rulerApply` (step 1 to 4 above), `rulerSettle`
   (step 5), `rulerUndo`, and for each kind its requests (Mimir's `POST`
   and `DELETE`, Amazon's `PUT` with `clientToken`, Azure's `PUT`), its
   allowed check (`--changes-in`) and its managed check (tags,
   `--changes-managed`). `internal/rules`: `TreeHash`; Azure's shape in
   `change()` (`properties.rules`, `expression`, an ISO `for`, `enabled`)
   and a writer of ISO durations; `PutBack(now, was, e)`, the rule's entry
   from `was` put in its place in `now`, by lines for YAML and in the tree
   for JSON, checked by reading back.
6. **The page.** `internal/serve/changesweb.go`: undo on the ruler path
   (the backup carried, the rule put back); the ruler, namespace and group
   on a proposal's page; why one is for a person.
   `internal/serve/i18n/ko.json`: every new text.
7. **Flags for changes.** `cmd/lapilli/serve.go`: `--changes-in`,
   `--changes-managed`, `--changes-ruler-token-file`,
   `--changes-aws-role`, `--changes-azure-client-id`, and their checks.
8. **The chart.** `charts/lapilli/values.yaml` as above;
   `templates/deployment.yaml`, the arguments and the write Secret, mounted
   only when changes are on; `_helpers.tpl`, the checks; the README, with
   the IAM policy and the Azure role.
9. **Unit tests**, in `internal/source`, `internal/rules` and
   `internal/serve` (`rulerwrite_test.go`), each kind against its
   stand-in: a `for`, a threshold, what it says, turning off (Azure) and
   deleting, applied and read back; undone, the rest of the unit kept as it
   was changed since; a rule changed since, refused; another rule in its unit changed
   since, made afresh; a namespace not named, or tagged, given no path, and
   refused when it becomes so after approval; a group's last rule (Mimir:
   the group deleted, whatever else the namespace holds; Amazon: cut from
   the file), an Amazon namespace's last and an Azure rule group's last
   (refused); Amazon `UPDATING` (waits, uncounted, and settles),
   `UPDATE_FAILED` (refused before, failed after); Mimir's `400` (failed,
   with its reason); a 3xx (an error, nothing sent elsewhere); a `nextLink`
   or an `id` elsewhere (refused); unreadable after the write (`applying`,
   never sent again); paused just before the write (nothing sent); counted
   once against the limits; a rule's key without a namespace unchanged; a
   `Rules` resource edited on the git path.
10. **End to end.** `test/e2e/changes.sh`: Mimir as above (the container
    `$p-mimir`, `remote_write` added to the Prometheus it starts), and a
    third `lapilli serve` with `--ruler "$mu/prometheus/config/v1/rules"
    --prometheus "$mu/prometheus" --changes-in shop,solo` on `port+4` and
    `port+5`. Seeded with `curl`: namespace `shop` with group `shop`
    (`ScrapeSlow`, `TargetDown`, a recording rule) and group `edge`
    (`EdgeDown` alone); namespace `alloy/payments/rules/0b1c` with a group
    `shop` and a `ScrapeSlow` of its own; namespace `solo` with one group
    of one rule. Checks:
    - the two `ScrapeSlow`s are told apart, by namespace;
    - a longer `for`, applied: the namespace as Mimir gives it differs by
      that one line, and `/prometheus/api/v1/rules` has the new duration
      after the ruler's poll; undone, the namespace as it was, byte for
      byte;
    - a runbook link, applied: Mimir's rules API has the annotation;
    - `TargetDown`'s `for` changed in Mimir between proposing and approving
      a change to `ScrapeSlow`: applied, `TargetDown` as changed;
      `ScrapeSlow` itself changed: refused as stale;
    - `EdgeDown` deleted: group `edge` answers 404; undone, it is back;
    - deleting `solo`'s only rule: the group goes, as Mimir allows; the
      `alloy/...` `ScrapeSlow`'s proposal is for a person, with why.

    The comment at the top of the script says what runs; the CI job's
    time limit is raised if Mimir's start needs it.
11. **Documents.** README (where rules come from, `--ruler`; changing
    rules; what it doesn't do yet), the chart's README, serve.md (what goes
    out), SECURITY.md (writes to rulers are in scope), ROADMAP.md (step 2,
    and the controller line above), changes.md's "As built", this
    document's own "As built", and the changelog. Each says which paths
    ran against a real ruler (Mimir) and which against documents only.

## Sources

- Mimir 3.2.4, run locally on 2026-11-11: `/prometheus/config/v1/rules`
  (list, namespace, group, `POST`, `DELETE`), `/prometheus/api/v1/rules`
  (`file`, the expression printed again); `/api/v1/rules` answers 404.
- [Cortex: HTTP API, Ruler](https://cortexmetrics.io/docs/api/): the
  configuration API at `/api/v1/rules`, `Content-Type: application/yaml`,
  `X-Scope-OrgID`, `-ruler.enable-api`.
- [Amazon Managed Service for Prometheus: ListRules](https://docs.aws.amazon.com/prometheus/latest/userguide/AMP-APIReference-ListRules.html),
  and botocore's `amp` model (2020-08-01): `PutRuleGroupsNamespace`
  (`PUT /workspaces/{workspaceId}/rulegroupsnamespaces/{name}`, 202),
  `DescribeRuleGroupsNamespace`, `ListRuleGroupsNamespaces`, the status
  codes, the namespace's ARN.
- [Azure REST API specifications](https://github.com/Azure/azure-rest-api-specs),
  `PrometheusRuleGroups/stable/2023-03-01`: list by subscription and by
  resource group with `nextLink`, `PUT` (200, 201), `PATCH` (tags and the
  group's `enabled` only), `DELETE`; `scopes` and `rules` required; no
  ETag; a rule's `enabled` and ISO 8601 `for`.
- [Alloy: mimir.rules.kubernetes](https://grafana.com/docs/alloy/latest/reference/components/mimir/mimir.rules.kubernetes/):
  `mimir_namespace_prefix` (`alloy`), `mimir_namespace_separator` (`/`),
  `sync_interval`.
