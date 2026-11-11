# Changing rules, with a person's approval

Status: proposed, for v0.22.0. Under [GOVERNANCE.md](../../GOVERNANCE.md)
this changes a principle, "Lapilli only reads", and is a new kind of data in
the ledger, so it is designed first, as a pull request open for a week. The
companion to [work.md](work.md), about the tasks people keep, and to
[serve.md](serve.md), about the rules a replay reads.

## Why

Lapilli says which rules to change and by how much: a longer `for` for a
noisy rule, the threshold that would have caught an incident nothing caught,
a rule another always beat. It checks the change before anyone makes it
(`--compare`, MCP's `compare`) and measures it after (the outcomes page).
Between the two, a person opens the rule file, writes the change, opens a
pull request, waits for a review, and comes back. Most suggestions die
there: not because anyone disagreed, but because nobody had the half hour.

So the step in between should be one click by a person who may make it: the
change written as the place the rule is kept expects it, checked again, and
applied there. Lapilli has refused this so far, for good reasons that stay
true:

- **A tool that can change alerts can be made to remove them.** Taking the
  alerts down first is how an attack hides itself.
- **Rules are kept somewhere.** Most are in git, applied by Argo CD, Flux,
  Helm or Terraform; a change made around them is undone at the next sync,
  or drifts.
- **Lapilli can be wrong.** A rule that never fired in ninety days may be
  the only one that would catch a rare, large incident; incidents nobody
  wrote down make good firings look like noise.

This design lets Lapilli make changes while keeping those three in view:
nothing is applied without a person's approval; the first way is a pull
request to where the rule is kept, reviewed again there; writing is off
unless turned on, with credentials of its own; and every change is checked
against what it was proposed for, kept so it can be undone, and measured.

## What changes in the principles

GOVERNANCE.md's first principle becomes:

> **Lapilli reads by default, and changes only what a person approves.** Out
> of the box it queries APIs and reads files, and changes nothing it is
> pointed at. With changes turned on, it proposes changes to alert rules,
> and applies one only once a person with the role for it has approved it:
> as a pull request to where the rule is kept, or, for a rule kept nowhere
> else, through the API that holds it, with the rule as it was kept so the
> change can be undone. It never silences an alert, writes to Prometheus or
> a cluster beyond the rules it was told it may change, or applies what an
> agent proposes without a person's approval.

ROADMAP.md's "Not planned" loses "Writing anywhere" and gains "Changes
without a person's approval" and "Silencing alerts, or writing anything but
alert rules". "A Kubernetes controller", "Features for one cloud" and "A
model of its own" stay.

What else says "only reads", to be changed when this is built (not in this
pull request): README.md (its opening, "Not planned" and "What it doesn't
do yet"), `site/index.md`, the organisation's profile
(`lapilli-project/.github`), SECURITY.md (what a report about writes is),
`docs/design/serve.md`, and the chart's README.

## Proposals

A proposal is a change to one rule, with the evidence for it. Lapilli makes
them from what it already finds; a person or an agent can make one too.

| Kind | From | Evidence attached |
|---|---|---|
| A longer `for` | a noisy rule's suggestion (`verdict.Suggestion`) | the backtest over twelve weeks: firings before and after, incidents caught and how much later |
| A nearer threshold, or a shorter `for` | a near miss for an incident nothing caught | the incident, the value it reached, and the backtest of the new threshold over twelve weeks |
| Retire (disable, then delete) | a spare rule, or one that never fired | twelve weeks of firings, what caught each of its incidents first; and the caveat, shown, that a rule may wait for a rare incident and that incidents nobody wrote down count against it |
| A fix to what it says | the content check ([content.md](content.md)) | the finding: a missing runbook link, a template naming a label the expression drops |

**Retiring is two steps.** Where the place the rule is kept can disable a
rule (Grafana's `isPaused`, a cloud alarm's enabled flag), retiring first
disables it; a second proposal, no sooner than four weeks later and only if
nothing it would have caught was missed meanwhile, deletes it. A rule file
has no "disabled": there, retiring is the deletion, in a pull request.

**Each proposal keeps** the rule as it was when it was made (its definition
and a hash of it), the rule as proposed, the diff, the evidence, who or
what proposed it, and its state: proposed, approved, opened (a pull request
is open), applied, refused, failed, undone, dropped. This is a new kind of
data: a `proposals` table and its history in `changes`. A proposal can be
made into a task, and a task can carry one.

**They show** on the rule's page (the diff, the evidence, the state, who
did what), on a proposals page with the ones waiting for approval first, in
the weekly digest's tasks line, and in MCP's `rule` tool. A rule's page
says when the rule it shows is not the one a proposal was made against.

## Paths

Where a change goes depends on where the rule is kept. Lapilli knows how it
read a rule (a file, an API, Grafana, a cloud's list); each proposal is
given a path when it is made, and one with none is a proposal for a person:
its diff and evidence as a pull request body to copy.

### A pull request, first

Most rules are kept in git and applied from it. A change there is a pull
request: reviewed again by the people who own the repository, applied by
whatever applies the rest, and undone by a revert.

**Lapilli writes the change, a recipe opens the pull request.** Lapilli has
no git in its image and no git library, and keeping repository credentials
in it would make a stolen Lapilli a way to push to repositories. So, as
incidents and firings are copied in by recipes, a pull request is opened by
one: `recipes/changes/` has one for GitHub, GitLab, Bitbucket, Gitea or
Forgejo and Azure DevOps alike. The recipe runs where the repository's
credentials already are (a CI job, a CronJob), and:

1. asks Lapilli for the approved proposals waiting for a pull request, on
   the incidents port with a token of its own (`--changes-token-file`), the
   way the firings token works: it can read approved proposals and say what
   became of them, nothing else;
2. finds the file each was made against in its clone: by the hash of the
   document Lapilli read (the ledger keeps each version's documents), or,
   for a rule file in a ConfigMap or a PrometheusRule, of the rules inside
   it;
3. applies the change Lapilli wrote, opens a branch and a pull request with
   the evidence as its body, and tells Lapilli its link (the proposal is
   then "opened", and its task carries the link);
4. tells Lapilli when the pull request is merged or closed. A merge isn't
   "applied": the change is applied when Lapilli reads the rules with it,
   as a new version, and the outcomes page measures it from then.

**What Lapilli writes.** The change is made in the document as it was read,
by editing the YAML's tree (`yaml.v3` nodes), not by writing it out afresh,
so comments, order and quoting stay as they were; the diff is of that.
Rule files, PrometheusRule resources, ConfigMaps holding either, Grafana's
provisioning files and the clouds' rule groups as their CLIs write them can
be edited so. A document no file in the clone matches (rules rendered from
a Helm chart, Kustomize, Jsonnet, Terraform) gets no edit: the recipe opens
a pull request, or an issue, with the diff of the rendered rule and the
evidence, for a person to make the change where it is written.

### Through the API, for rules kept nowhere else

Some rules are kept only where they run: made in Grafana's UI, in a cloud's
console. For those, and only those, Lapilli applies an approved change
itself, through the API that holds the rule, with credentials of the
target's own (below).

| Kept in | How | Can't be written when |
|---|---|---|
| Grafana alerting | the provisioning API: `PUT`/`DELETE /api/v1/provisioning/alert-rules/{uid}`, with `X-Disable-Provenance` so the rule stays editable in Grafana's UI | its provenance is `file` (provisioned from files: the git path) or `api` set by another tool |
| Mimir or Cortex ruler | `POST /prometheus/config/v1/rules/{namespace}` with the group, `DELETE` for a group | the group carries a label or annotation saying a tool manages it |
| Amazon Managed Service for Prometheus | `PutRuleGroupsNamespace` (the whole namespace, as it was read, with the change) | the namespace is tagged as managed (CloudFormation's own tags, or one the operator names) |
| Azure Monitor managed service for Prometheus | `PUT` on the `prometheusRuleGroups` resource | tagged as managed, as above |
| Google Cloud Managed Service for Prometheus | patching the `Rules`, `ClusterRules` or `GlobalRules` resource | labelled as managed by Argo CD, Flux, Helm or another tool |
| CloudWatch alarms | `PutMetricAlarm` with the whole alarm as read and the change; `DeleteAlarms`; disabling is `DisableAlarmActions` | tagged as managed, as above |
| Azure Monitor alert rules | `PUT`/`DELETE` on the alert rule resource; disabling is `enabled: false` | tagged as managed, as above |
| Google Cloud alerting policies | `alertPolicies.patch` with an update mask; `delete`; disabling is `enabled: false` | labelled as managed, as above |
| OCI Monitoring alarms | `UpdateAlarm`; `DeleteAlarm`; disabling is `isEnabled: false` | tagged as managed, as above |

Prometheus itself, Thanos's ruler and VictoriaMetrics's vmalert have no API
to write a rule: their rules are files, and the git path is theirs. A rule
read with `--loaded-rules` is one of those. A rule whose place is managed by
a tool (git, Terraform, a GitOps controller) is refused through the API,
with the reason, and offered the git path: a change made around such a
tool is undone by it, or fights it. The operator can name the tags or
labels that mean "managed" for their tools, beside those Lapilli knows.

A cloud alarm that isn't PromQL, judged from the firings copied in, has no
condition Lapilli can replay: its proposals are only retiring it and fixes
to what it says, never a `for` or a threshold, which no backtest could
check.

### Before anything is applied

Whatever the path, at the moment it is applied (or handed to a recipe):

1. **The rule is read again** from where it is kept, and compared with the
   one the proposal was made against. If it changed, nothing is applied;
   the proposal is refused as stale, and can be made again.
2. **The backtest is run again** over the twelve weeks to now, and refused
   if it says something worse than the evidence the person approved: an
   incident caught before and missed now.
3. **The rule as it was is kept** in the ledger, whole, so that undo can
   write it back. For the git path, undo is a revert pull request through
   the same recipe.
4. **What it did is kept** in `changes`, and told (below).

## Approval

Always a person, in the UI.

- **Who.** A new role, approver, given by `--approvers` (users and groups,
  as `--editors` and `--admins` are); admins are approvers unless
  `--approvers` says otherwise. An editor or an agent proposes; an approver
  approves.
- **Two people,** with `--approvals 2`: the second approval must be another
  person's, and no one approves their own proposal.
- **What they see.** The diff, the evidence, the stale check's result, and
  who or what proposed it. A proposal an agent made says so at the top.
- **Only the UI.** Approving is a form on the proposal's page, refused from
  another site as every change through the UI is, and refused through the
  API, MCP, and every token. An agent may propose (MCP's `propose`, behind
  `--mcp-propose`, as `compare` is behind `--mcp-compare`), never approve or
  apply.
- **Told.** `proposal.made`, `proposal.approved`, `change.opened`,
  `change.applied`, `change.refused`, `change.failed` and `change.undone`
  go to the webhook and Slack, as notices do now.

## Safety

- **Off unless turned on.** Without `--changes`, no proposal is ever handed
  to a recipe or applied: Lapilli proposes, and the proposals page says
  changes are off. The write credentials aren't read without it.
- **Bounded.** At most five deletions a day and twenty changes an hour, of
  all kinds, unless the operator says otherwise; a change past them waits.
  A proposal touches one rule.
- **A stop.** `--changes-paused`, a file that pauses changes while it
  exists (for whoever runs the pod), and an admin's switch in the settings:
  each stops anything being applied or handed out at once, and says so on
  every page.
- **Undo.** Every applied change can be undone from its page, by an
  approver, for as long as `--keep`.
- **Measured.** A change applied is a new version of the rules, and the
  outcomes page sets what it did beside what was predicted, as it does for
  any change today.

## Security

What a stolen Lapilli can do today is read what it reads, and send what it
sends where it was told. With changes on, it could also change or delete
alerts, so this is limited:

- **Git: no credentials at all.** Lapilli never holds a repository's token.
  The worst a stolen Lapilli does on the git path is hand a recipe a
  proposal, which becomes a pull request that people review and merge, with
  the repository's own protections.
- **API: one credential a target, scoped.** Each in its own file
  (`--changes-grafana-token-file`, `--changes-aws-role`, and so on), given
  only what changing rules needs:
  - Grafana: a service account allowed to write alert rules in the folders
    named, nothing else.
  - AWS: `aps:PutRuleGroupsNamespace` on the namespaces named;
    `cloudwatch:PutMetricAlarm`, `DeleteAlarms` and `DisableAlarmActions` on
    alarms with a name prefix the operator chooses.
  - Azure: a custom role allowing write and delete on the rule resources, in
    the resource groups named.
  - Google Cloud: `monitoring.alertPolicies.update` and `delete` on the
    projects named (Google has no finer grant), and `Rules` write in the
    namespaces named.
  - OCI: `manage alarms` in the compartments named.
- **Never from outside.** The notifications and incidents ports, and every
  token, can't approve or apply; the changes token can only read approved
  proposals and report on them. Nothing an alert manager, a cloud or a job
  sends becomes a change.
- **Agents propose only.** A proposal's text from an agent is data, shown
  as such; approval is a person's, in a form an agent can't post.
- **Every outbound write** goes only to the target given, follows no
  redirects, and is told.
- **Reviewed as an attacker would** before it is released, as
  CONTRIBUTING.md asks, with the paths that write first.

## Options

- `--changes`: turns changes on.
- `--approvers USERS`, `--approvals N` (1 or 2).
- `--changes-token-file FILE`: the recipes' token, on `--incidents-listen`.
- `--changes-grafana-token-file`, and the clouds' and rulers' credentials,
  one a target.
- `--changes-managed KEY=VALUE`: tags or labels that mean a tool manages a
  rule's place, beside those Lapilli knows.
- `--changes-limits DELETIONS/DAY,CHANGES/HOUR`, default `5,20`.
- `--changes-paused FILE`.
- `--mcp-propose editors|admins`.
- The chart: `serve.changes`, with the same, its Secrets mounted only when
  it is on.

## Alternatives

- **Lapilli opening pull requests itself,** through each host's REST API.
  No recipe to run, but Lapilli would hold a token that can push to the
  repositories, and a client for five hosts' APIs. The recipe keeps the
  credentials where they already are, and is generic as the incident
  recipes are.
- **Applying straight to everything through APIs.** Faster, and wrong for
  most rules: a change around git or Terraform is undone by them.
- **Only patches, never applied.** The safest, and the half hour this is
  meant to save. It stays as what a proposal with no path becomes.
- **Approving through the API,** for automation. Then a stolen token
  approves; the point of approval is a person.

## Building it

Too much for one release; each step is built for every source alike, and
reviewed as an attacker would before it ships:

1. **Proposals and the git path:** the `proposals` table, proposals from
   suggestions, near misses, spare and silent rules and the content check,
   with their backtests; the approval page and role; `recipes/changes/` for
   the five git hosts; the stop, the limits and undo. Grafana's API, as it
   isn't a cloud and holds many teams' rules.
2. **Managed Prometheus rulers:** Mimir and Cortex, Amazon Managed Service
   for Prometheus, Azure's rule groups, Google Cloud's rule resources.
3. **A cloud's own alarms:** CloudWatch, Azure Monitor, Google Cloud
   Monitoring and OCI.

Each step says, as the rest of Lapilli does, which paths were tried against
a real account and which against documented answers only.

## Questions for review

- Five deletions a day and twenty changes an hour: too many, or too few for
  a first clean-up of a large set of rules?
- Should two approvals be the default when changes are on?
- Retiring as disable first, delete four weeks later: is four weeks right,
  and should a rule file's retirement be a commented-out rule instead of a
  deletion, so its history reads better?
- Which tags and labels should count as "managed" without being told:
  `app.kubernetes.io/managed-by`, Argo CD's and Flux's labels,
  CloudFormation's `aws:cloudformation:*` tags; Terraform sets none of its
  own.
- Should agents be able to propose at all by default, or only with
  `--mcp-propose`, as written?
