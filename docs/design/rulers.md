# Changing rules in a managed Prometheus ruler

Status: built, for v0.23.0 (see "As built"): the second step of
[changes.md](changes.md#building-it). The rules a Mimir or Cortex ruler,
Amazon Managed Service for Prometheus or Azure's Prometheus rule groups
hold are changed through their own APIs, once a person approves. It adds a
way of reading rules and three places Lapilli writes to, so under
[GOVERNANCE.md](../../GOVERNANCE.md) it is designed first, as a pull
request. It keeps everything changes.md settles and says only what is new.
Google Cloud's rule resources stay on the git path; why is below. A Loki
ruler's rules, read with `--loki-loaded-rules`, are changed the same way
for v0.70.0 ("Loki's ruler", below).

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
version, and the Outcomes page measures it from there. Mimir checks a
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

### Loki's ruler

Added for v0.70.0. Loki's ruler is Cortex's, under Loki's path:
`/loki/api/v1/rules` lists every namespace, `GET .../NS/GROUP` reads a
group, `POST .../NS` sets one (`Content-Type: application/yaml`, `202`),
and `DELETE .../NS/GROUP` deletes one (`202`), as Loki's HTTP API reference
gives them. `--loki-loaded-rules` already reads the rules through it, a
document a namespace ([loki.md](loki.md)); a change approved to one of
them is now written there as a Mimir's is. Before, it was for a person.

- **Where it may.** Only where `--changes-in` names the namespace as
  `loki:NAMESPACE`, or `loki:PREFIX*`, in its letter case. A Loki and a
  Mimir may both have a namespace `shop`: `--changes-in shop` names the
  Mimir's alone, and `loki:shop` the Loki's alone. Without
  `--loki-loaded-rules`, `loki:shop` is `--ruler`'s namespace of that name,
  as it was; given with no `--ruler` either, it is refused. An `--alarms`
  source named `loki` is refused beside `--loki-loaded-rules`. A team's
  `changes_in` names Loki's namespaces the same way, within those
  `--changes-in` names.
- **A path of its own.** A Loki rule's proposal takes the path `loki`, not
  `ruler`. A path is fixed when the proposal is made, so a change made for
  a Loki rule is read again and written only through `--loki`'s ruler,
  never through `--ruler`, whatever a restart does to the flags. One whose
  Loki serve no longer reads is refused once approved; one sent, and
  applying, stays applying until it is read back there.
- **As a Mimir's.** One group is what a write replaces. It is read again
  just before: the rule changed since is refused as stale; only other
  rules of the group changed, the change is made afresh, theirs as Loki
  holds them. The group is kept for undo, counted against the limits, sent
  once, and read back, a group still as it was read again for two minutes.
  One change to a group at a time. A group goes with its last rule (its
  `DELETE`). Undo puts back the rule alone. The pause and a team's
  boundaries are asked again just before the write.
- **Read as LogQL.** A group read again is read as `--loki-loaded-rules`
  reads it, its queries LogQL, so the rule hashes as it did when it was
  proposed. Read as Prometheus's, a LogQL rule hashed otherwise, and every
  proposal was refused as changed since.
- **Credentials.** Reads use `--loki-token-file`'s token. Writes use
  `--changes-loki-token-file`'s, which must not be that token, nor
  `--changes-ruler-token-file`'s; without it, writes carry no token, for a
  Loki reached only in the cluster. Loki has no authorization of its own:
  a gateway in front of it checks a token. `--loki-tenant` is sent as
  `X-Scope-OrgID` on reads and writes alike, and a change is written only
  in the tenant it was proposed in, as a Mimir's is (Security, "A proposal
  is written where it was read").
- **Only where it was told.** As `--ruler`'s: every request to `--loki`'s
  host, no redirect followed, 30 seconds, 4 MB a group and 64 MB the list.
  A namespace or group whose name isn't written to (`NameOK`) is for a
  person.
- **What Loki needs.** Its ruler's API on (`enable_api`, on by default) and
  a rule store that takes writes. Loki's docs say its `local` store is "a
  read-only backend that does not support the creation and deletion of
  rules through the Ruler API". An object store does (S3, GCS, Azure,
  Swift), and so does `ruler_storage` with `backend: filesystem` once
  `storage_config.use_thanos_objstore` is true (Loki 3.7.9's
  `initRulerStorage`). `lapilli preflight` says it can't know whether
  writes are taken (`changes.loki`), as for a Mimir.

**A rule added** (v0.72.0) is offered the groups of a namespace
`--changes-in` names as `loki:NAMESPACE`, and a new group in it, as a
Mimir's are ([expectations.md](expectations.md), step 5): the place's path
is `loki`, the rule is written as LogQL, backtested in Loki and held to its
expectations, the group written as above (a group made only while it is
still gone), read back and undone by deleting the rule. Its place and the
Mimir's of the same name are two places.

A Loki ruler's rule files (`--loki-rules`) aren't written through an API;
their rules are changed by the git path, and a rule is added to them there,
as a Prometheus rule file's are ([changes.md](changes.md)). Loki's `local`
store takes no writes, so a Loki that keeps its rules in files is changed
in the repository they are kept in.

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
  as Grafana's are (64 MB, a minute, the headers within 30 seconds).
  - A list is parsed whole, its YAML some 20 to 85 times its size: one of
    more values than one every 4 bytes past its first 100,000, or 6 million
    in all (counted before it is parsed: each needs a `,`, `:`, `-`, `[`,
    `{` or a line; real lists are a value every 5 to 15 bytes, and a value
    takes some 320 bytes at the peak, so 1.9 GiB at most), or of more than
    50,000 rules (counted before they are written out again, a list that
    doesn't parse whole and is split at its namespaces too), or that holds a
    YAML alias, which no ruler writes, isn't read (split, a namespace
    holding one is kept as written and refused as it is parsed, saying so);
    a namespace with a group of more than 100,000 values is passed over, as
    one that can't be read is (so is one written whole, in flow style, of
    more), and past 50,000 rules as read none is. An Amazon namespace holds
    a rule file someone wrote, whose aliases are read: what they expand to
    is counted for the namespace, as yaml.v3 counts one document's, and
    what they make, with the rest, for every namespace together (below).
    Each group is written out again on its own (the YAML emitter holds every
    event of what it writes until it ends), as the whole would be, and a
    document's nodes are let go of as they are decoded. Before 0.34.1 a
    hostile ruler's 4 MB of tiny rules took over a GB; 132 MiB now. A unit,
    and a cloud's alarm, are decoded in linear time too.
  - What rules make beyond their own text, however they make it, is
    bounded for the input they are read from (`rules.madeLimit`): a file, a
    directory's files, a stream, a ConfigMap's files, a ruler's namespace,
    the documents of one source in a version kept in the ledger. It is the
    larger of 4 MiB and 32 times the input's size, and 256 MiB at most
    (`ExpandMin`, `ExpandTimes`, `MaxMade`), charged where a copy is made,
    before it is made:
    - what YAML aliases bring in again: the text of the values and keys,
      and 64 bytes each for the map or list entry and the interface it is
      held in (`aliasValue`), judged against the whole input's size
      (yaml.v3's own bound counts values, not their text or what holds
      them);
    - a group's labels and its name on each of its rules, worked out once
      for the group, the map and its text (`labelsCost`, a third more than
      Go allocates for a copy): a recording rule has a copy of the labels
      of its own, and an alert rule's alerts carry them, its key holds the
      name (`AlertLabels`, `Key`, `Whole`, the replay's identity and
      firings), worked out for each rule wherever it is judged, shown or
      hashed, so that what is charged when the rules are read bounds each
      of those passes too; so a Grafana group's name and folder, a rules
      API's group's name, a Google policy's name on its conditions, and a
      ruler's namespace on its rules;
    - a Google Rules' namespace and an Azure rule group's cluster, put
      into each selector of each rule's expression: what the expression
      grows by is worked out first, without writing it, and it is refused,
      or written into as much at once (written as it grew, a builder grew
      a quarter at a time, five times its room made by a refusal);
    - a document picked out of a stream and written afresh (a List some of
      whose items are left out), its aliases in full, which is what is
      kept: written by yaml.v3's encoder into a writer that refuses past
      the room the input has left, the encoder giving up as it does, and
      charged what it was written in. An estimate fell short (yaml.v3
      indents four spaces a level, and writes bytes that aren't UTF-8 as
      base64 in lines, each indented), and 820 KB were kept as 109 MB.
      What is kept so, where it is read in the file's place (`Trim`, a
      selector leaving an item out), is read as the file it is of
      (`load`): what it may make judged by the file's size, what the copy
      holds beyond the file charged as made, and the copy held to
      `MaxFile`, as a file is, a `MadeError` past it (read as a file of its
      own, the copy's size, aliases written out, set what it might make,
      and a List of 41 KB written out to 1.5 MB made its recording rules'
      labels past what the file may). A file read as it is isn't held to
      what its copy, unread, would be. A ConfigMap whose `data` an alias brings in is refused
      where the stream is picked (`pickItem`), as `Parse` refuses it:
      counted broken and kept, it was written afresh with each alias in
      full and read so, a rule file again for each key.

    A value in the shape of rules a YAML alias brings in is refused in a
    ConfigMap's `data`, naming its key, whatever its tag and wherever the
    alias is (the value, the data, a merge into it, the whole ConfigMap in
    a List): YAMLValue notes the text of each string it makes in an
    alias's expansion (an alias of a `!!str` shares its anchor's text, so
    the anchor's own is noted too; a `!!binary` is decoded afresh for each
    alias), and the ConfigMap's reader refuses a value whose text is noted
    and is in the shape of rules by its text (`textShaped`, below, of the
    text a `!!binary` decodes to); one that isn't, an env-style value
    aliased by another key, is passed over unread, as it was. An Amazon
    rule groups namespace's `data` whose text is noted is refused so, before
    it is decoded, naming the namespace: each namespace aliasing it would
    be decoded and read into rules again, and the AWS CLI writes none. A
    ConfigMap the API server keeps holds no alias; each would be read into
    rules again (10,000 rules by 16 keys made 1.2 GB, and as `!!binary`,
    past a check by where a value's text is, 1.22 GB). `binaryData` isn't read for
    rules. Inputs read together are bounded together (`rules.Together`): a
    ruler's namespaces, each an input of its own so one can't use up
    another's bound and have it passed over in its place, and the sources
    of a version read again from the ledger, each read as it was read (a
    `Namespace`'s document on its own, the others by source). What they
    make in all counts what each made, refused ones' and files passed
    over's among it, never given back; a charge refused isn't made and
    isn't counted, so the sum is the same in whatever order they are read.
    Past 256 MiB the read fails whole (`rules.OverTotal`), naming the input
    whose making took them past and the one that had made the most
    (`MadeError.Most`, by `input.name`, a namespace's source), which may be
    another, what an input makes in reading counted before it is done, and
    the version in effect stays. Each input's room is what it may make and
    what they may still make together, whichever is less.

    Why there: the copies were made for each rule before any check could
    look at the rules (a namespace of 40 KB in a Rules of 99 KB made 6.5
    GB, a group of 9,999 labels on 6,700 recording rules in a file of 286
    KB 4.2 GB, a group's name of 1 MiB on 1,000 rules 1.07 GB), and a check
    of the rules once read takes time for each rule times its group's
    labels; charged where they are made, a refusal comes at the rule that
    crosses the line, in time of the input's bytes. For the input, not each
    document: 200 documents of 21 KB, each within 4 MiB alone, made 1.9 GB
    together. Thirty-two times, not sixteen: a file sharing six labels and
    three annotations among each of its rules by YAML anchors makes some 16
    times its size. No label or name is capped: Prometheus takes them of
    any length, and a version kept in the ledger may hold one. Past the
    bound, a `MadeError` names what made it, and it is never passed over as
    a rule file that doesn't read may be: a ConfigMap's file so refused is
    an error naming its key, as is one in the shape of rules that Lapilli's
    reader refuses though yaml.v3 decodes it (only a value yaml.v3 can't
    decode isn't YAML), and a ruler's namespace is passed over saying why.
    So is a file below a directory's top in that shape, as at the top
    (`unreadAs`, where a stream is picked as where it is parsed): it was
    passed over as one that doesn't read is. In the shape of rules is a
    mapping with `groups`, a key as written whatever its tag or as its
    text is read (below), a `spec` with `groups` (a PrometheusRule, a
    Google `Rules`), or a merge; a list one of whose items is a mapping
    with `groups` or a `kind` (`rulesShaped`); and, by its text
    (`textShaped`), a document whose nodes don't tell: one `Nodes` refused,
    one an alias brought in, and the whole input a refused document is of,
    as that document may not be the one of rules (a stream whose
    Deployment has a key Lapilli refuses, beside its PrometheusRule, was
    passed over below the top). By its text is `groups` written, or written
    by double-quoted escapes (`gr\x6fups`, an escaped line break), read as
    YAML reads them, or `Z3JvdXBz`, the only base64 a `!!binary` key
    `groups` can be, however its tag is written (`!!bin%61ry`, a `%TAG`
    directive) and across whatever spaces and line breaks, as a key's
    `!!binary` is read without them. A backslash or `binary` alone isn't:
    a file of data below the top with one (585 KB of a CSV, one Windows
    path in it) refused the directory, and an env-style value with a
    Windows path, aliased by another key, its ConfigMap. Any other value is
    passed over whatever the reader says of it (a file_sd list whose labels
    have a key twice, which Go's JSON takes), and a rule file with a key
    twice, which yaml.v3 refuses too, is broken, not an error. Real sets are far inside
    it: kube-prometheus-stack's rules four times over, each group with
    labels of its own, make 1.8 MB of the 208 MB they may; 5,000 rules
    sharing six labels and three annotations by aliases, 10.4 MB of 20.

    A mapping's keys are read as text in rules inputs (`keysAsText`), as
    Prometheus decodes a rule file's into its strings: a plain scalar as
    written (`0`, `true`, `1.5`, a date), a `!!binary` key as the text it
    decodes to. Read as yaml.v3 decodes into Go, a mapping one of whose
    keys wasn't a string was a `map[any]any`, which no rule is read from:
    a ConfigMap's `data` with a key `0` hid its rule files, a rule file
    whose `groups` key was `!!binary` its rules, labels with a key `true`
    every label, unsaid. A key that isn't a scalar, a null one (which
    Prometheus passes over and Kubernetes refuses), one of another tag
    (`!x`, which Prometheus reads as its text), and two keys of one text
    (an alias of a key beside the key, a `!!binary` key beside what it
    decodes to) are refused, naming the line, as Lapilli's own refusals
    are: yaml.v3 tells keys apart as written, so Prometheus loads two of
    one text, the last set, and they aren't a key given twice, a file that
    doesn't read, passed over below a directory's top and counted broken in
    a ConfigMap.
    `YAMLValue` without it decodes as yaml.v3 does, as what compares or
    hashes a ruler's documents reads them.
  - A replayed rule's firings carry a copy of its labels with each series'
    own: what those take, 96 bytes a label, is held to what a range's
    samples are (`--max-samples`, 64 bytes a sample), past which the rule
    isn't replayed, saying so in its own words (`rules.TooWide`), which
    `--fail-unreplayed` counts (`rules.Unreplayable`): taken for a query
    refused as written, a rule's writer could keep a rule out of a
    backtest, the run passing. A rule of 9,999 labels on 1,000 series made
    1.3 GB. Wherever a replay's answer is kept or used, such a rule is one
    the replay gave no answer for (`rules.Unanswered`), as one that didn't
    finish or whose query failed: `lapilli serve` keeps its firings as they
    were and lists its run as failed, and a near miss's cost and an
    unrecorded firing's clearance are an error. Taken for a rule that
    can't be replayed, its firings were dropped and the window recorded as
    replayed empty, never tried again.
  - Durations are read as Prometheus writes them (`model.ParseDuration`'s
    years down to milliseconds) after Go's reading, which reads some
    Prometheus doesn't (`1.5h`, `1m1h`, `1us`); one past 100 years is
    refused, which Prometheus takes to some 292 years. Neither is newly
    refused.
  - What is still multiplied: each bound is a version's or a request's.
    lapilli serve holds every version of the rules in a window at once
    (`Server.versions`) and keeps the rules of up to 128 versions read, and
    each request that works out every rule's labels makes them again at
    once with others: what one version may make is multiplied by those. A
    bound on all of them is serve's to keep, not the reader's.
  - What a ruler answers with an error is said without the token or the
    signature it was asked with, which it may echo, as a history API's
    errors are (`prom.Redact`).
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
  changed the flag, is refused. From v0.70.0 the tenant is part of the
  place too. A Mimir's, a Cortex's or a Loki's proposal keeps the tenant
  its rule was read in (`--ruler-tenant`, `--loki-tenant`; none when
  none is set). One made in another tenant than serve sends now, on the
  same ruler, is refused once approved ("the rule was read in another
  tenant"), and nothing is sent; one sent and not yet read back is read
  back only in its own tenant, and stays applying, saying why, while serve
  names another. Before, a restart naming another tenant on the same ruler
  had an approved change read again and written there whenever the rule
  hashed alike. A proposal made before v0.70.0 kept no tenant, and is
  taken as made with none: it is written, and read back, only while no
  tenant is set. Where one is set, such a proposal is refused once
  approved, to be proposed again; one left applying across the upgrade
  stays applying until an admin closes it, so let changes settle before
  upgrading a ruler with tenants.
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
- **Loki** is the same API under `/loki/api/v1/rules`. It is tested
  against that stand-in at Loki's path, which takes nothing without the
  tenant (`internal/serve/lokiwrite_test.go`, and a rule added in
  `lokiadds_test.go`), and in CI against a real Loki 3.7.9 by
  `test/e2e/loki.sh`: a second Loki with its rules on the filesystem
  through the object store client, a change applied, read back and undone,
  and a rule added to its group and undone.
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

## As built

As above, with the answers settled for v0.23, except:

- **`--ruler-tenant`**, the settled name, not `--tenant`. It goes with
  `--ruler` (a Mimir's or a Cortex's), and is sent as `X-Scope-OrgID` to
  `--ruler` and `--prometheus` alike, as Reading says. The chart's is
  `rules.ruler.tenant`, beside the URL it is for, not `prometheus.tenant`.
  For the next release, the history takes a tenant of its own,
  `--prometheus-tenant` (the chart's `prometheus.tenant`), sent to
  `--prometheus` alone and in place of `--ruler-tenant` there: a Mimir or
  a Cortex with no gateway whose history is in another tenant than its
  rules, and `lapilli impact suggest`, which reads no ruler.
- **Amazon and Azure need `--auth`** of their cloud (`aws`, `azure`), with
  `--ruler`: their rules are read with the pod's identity, as the queries
  are. `--ruler-token-file` and `--ruler-tenant` are refused for them, and
  `--changes-managed` for a Mimir or a Cortex, whose groups carry no tags.
- **The order of the checks.** As Grafana's path does it, the backtest is
  run again first, then the unit is read again, the pause looked at, and
  the change kept and sent: the read and the write are a request apart,
  not a backtest apart.
- **A Mimir or Cortex unit is kept as a rule file of its one group**
  (`groups:` and the group as the ruler gave it; `groups: []` once the
  group went with its last rule), so the edit and the diff that files get
  work on it unchanged. What is posted is that group written out again;
  Mimir writes it its own way anyway. An Azure rule group is kept as JSON
  with its keys in order, indented, so a change is a change to its lines
  alone; what Azure sets (`id`, `name`, `type`, `systemData`) is left out
  of what is written, and of its hash.
- **Read back as rules, not bytes.** A unit read back is compared with what
  was written, and with what it replaced, by each alert rule's whole entry
  (every key, an Azure rule's `actions` too), its durations as lengths of
  time, and the rest of its tree with them taken out: Mimir prints a `for`
  of `90s` as `1m30s`, and a comparison of text would fail every change it
  writes so. A Mimir or Cortex group read back as it was is read again each
  round until two minutes after the change was sent, by the server's
  clock, before the change is called failed, for a ruler that caches its
  storage: a round can come at once, as approving another change starts
  one.
- **One change to a unit at a time.** A change waits, approved and
  uncounted, while another to its unit (the group, the Amazon namespace,
  the Azure rule group) is `applying`: written over it, that one would read
  back as neither. A change left `applying` is read back only from the
  ruler it was sent to; while `--ruler` names another, it stays applying.
- **Undo, as built.** The rule must be as the change left it, every key of
  it, not only those its hash covers; it is put back alone, and a group
  that went since comes back holding that one rule; what is written must
  be the unit as it is now with that one rule added, or put in place of
  its own entry.
- **Reading past what can't be read.** Mimir 3.2.4 writes an expression
  whose first line starts with spaces, in a rule, as a block YAML can't
  read back (`|4`, its lines two short). Its list is read as YAML, whole,
  as it nearly always reads, each namespace's groups written out again
  with two spaces, which read back. When it doesn't read, it is split at
  its namespaces and each group read on its own: one that can't be read is
  passed over, logged once while it lasts, and the rest kept as the ruler
  gave them. A namespace longer than 128 bytes, as Alloy's can be, is
  listed as a long key, `? NAME` and then `:` and its groups, and split as
  one. A group read but not read back as written is for a person. An
  Amazon namespace whose file doesn't read is passed over, or keeps the
  last of its files that did. Undoing a deletion in a group of many rules
  that went since cuts its other rules in one pass.
- **Amazon's `clientToken`** is `lapilli-PROPOSAL-SENT`, the proposal's
  number and the second it was sent: a change is never sent again, so the
  time stands for the attempt.
- **Names.** A Mimir or Cortex namespace or group with a name Lapilli
  doesn't write to (empty, a path segment `.`, `..` or empty, a control
  character, over 1,024 bytes) is read, and its proposals are for a
  person; an Amazon namespace so named, or longer than 128 bytes, isn't
  read. An Azure rule group whose `id` isn't under `--ruler`'s scope isn't
  read; one that answers, when read again, with another's `id` is refused.
  An Azure rule's namespace, its resource group, and its rule group's id,
  its subscription and resource group, are kept in lower case, as Azure
  may give them in another case from one read to the next.
- **Told apart by the pair.** Rules of one name are told apart by
  namespace and group counted as a pair, each written with a `/` (and
  `%`, `,`, `]`) escaped: namespace `a/b`'s group `api` is
  `Errors [a%2Fb/api]`, namespace `a`'s group `b/api` `Errors [a/b%2Fapi]`.
  A rule whose own name is one so made (an alert written
  `Errors [shop/api]`) is told apart again from it, until no two rules
  share a name but those alike in group and labels, and a proposal for
  such a name is refused.
- **What Lapilli proposes of itself** each week includes a ruler's rules,
  carried out only where `--changes-in` lets them be.

Tested: the ruler path against a real Mimir 3.2.4 in
`test/e2e/changes.sh`, in CI, and against stand-ins in the unit tests: one
answering as Mimir 3.2.4 did (four spaces, comments dropped, durations
printed again, 400 for a group it won't take), at Mimir's paths and at
Cortex's; Amazon's as botocore's `amp` model has it (`UPDATING` until it is
`ACTIVE`, SigV4 for `aps`); Azure's as its REST specification has it
(`nextLink`, `PUT` answering 200 or 201). Cortex, Amazon and Azure have
not been run against a real server or account. Each guard of the write
paths was taken out, one at a time, and a test failed.

Reviewed before release as an attacker would and for correctness, twice
apart, and the fixes verified again; what the reviews found is fixed as
above (undo bringing back a
group's other rules, or dropping keys set since; names that could
collide or escape their path; a change read back as neither while another
to its unit was applying, or from another ruler; one unreadable group or
namespace blinding the read), each with a test that fails without it.

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
