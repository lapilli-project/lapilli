# Changing rules, with a person's approval

Status: built: the first step, proposals, the git path and Grafana, for
v0.22.0 (see "As built"); the second, the managed Prometheus rulers, for
v0.23.0 ([rulers.md](rulers.md), and its "As built"); the third, a cloud's
own alarms, for v0.24.0 ([alarms.md](alarms.md), and its "As built"),
with turning a rule off or deleting it waiting while it fires, on every
path.
Under [GOVERNANCE.md](../../GOVERNANCE.md)
this changes a principle, "Lapilli only reads", and is a new kind of data in
the ledger, so it was designed first, as a pull request (#40); it was merged
on 2026-11-11, before its week was out, by the maintainer's decision, with
no comments. The maintainer settled three of its questions then: one
approval by default (`--approvals 2` for two), and a rule retired by turning
it off first and deleting it four weeks later, approved again. The
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
is open), applied, refused, failed, undone, dropped (and, as built, ready,
applying and merged). This is a new kind of
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
| Mimir or Cortex ruler ([rulers.md](rulers.md)) | `POST /prometheus/config/v1/rules/{namespace}` with the group, `DELETE` for a group | its namespace isn't one `--changes-in` names: nothing on a group says which tool keeps it |
| Amazon Managed Service for Prometheus ([rulers.md](rulers.md)) | `PutRuleGroupsNamespace` (the whole namespace, as it was read, with the change) | its namespace isn't named, as above, or is tagged as managed (CloudFormation's own tags, or one the operator names) |
| Azure Monitor managed service for Prometheus ([rulers.md](rulers.md)) | `PUT` on the `prometheusRuleGroups` resource | not named, or tagged as managed, as above |
| Google Cloud Managed Service for Prometheus | not through an API: `Rules`, `ClusterRules` and `GlobalRules` are Kubernetes resources, changed by the git path ([rulers.md](rulers.md#google-cloud-managed-service-for-prometheus-the-git-path)) | always |
| CloudWatch alarms ([alarms.md](alarms.md)) | `PutMetricAlarm` with the whole alarm as read and the change; `DeleteAlarms`; disabling is `DisableAlarmActions` | tagged as managed, as above |
| Azure Monitor alert rules ([alarms.md](alarms.md)) | `PATCH` of one field (a `PUT` where the API takes no field alone), `DELETE` on the alert rule resource; disabling is `enabled: false` | tagged as managed, as above |
| Google Cloud alerting policies ([alarms.md](alarms.md)) | `alertPolicies.patch` with an update mask; `delete`; disabling is `enabled: false` | labelled as managed, as above |
| OCI Monitoring alarms ([alarms.md](alarms.md)) | `UpdateAlarm`; `DeleteAlarm`; disabling is `isEnabled: false` | tagged as managed, as above |

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
  person's, and no one approves their own proposal. An undo of an applied
  change is the exception, as built: it is approved by the one approver
  who asks for it, whatever `--approvals` says, so that a change can be
  taken back at once (below, "Undo").
- **What they see.** The diff, the evidence, the stale check's result, and
  who or what proposed it. A proposal an agent made says so at the top. As
  built (docs/design/ui.md), the page shows the change as a diff, unified
  or side by side, around each change or the whole rule; the backtest as a
  table, now beside as proposed, with a warning above it when the change
  would miss an incident the rule catches now; and what happened to it, in
  order.
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
  the repository's own protections. For that to hold, the recipe doesn't
  take Lapilli's word for which file to write: only a file under
  `RULES_PATHS`, and never one a CI system runs (`.github/`,
  `.gitlab-ci.yml`, `azure-pipelines.yml` and the like), wherever it is. A
  workflow pushed on Lapilli's branch would run with the repository's
  secrets before anyone reviewed it. Since 0.28.1, after a review found a
  script, a Makefile, a GitLab `include: local:` file or a build's
  configuration under `RULES_PATHS` written and pushed, executable still:
  only a file of rules, as git keeps it (mode 100644, `*.yml`, `*.yaml`,
  `*.json` or `*.rules`) and by what it holds, each document read as YAML
  reads it; in it, only the rules, each other document and line as it was
  (a second review found a ClusterRoleBinding indented after a
  PrometheusRule, a ConfigMap of rules rewritten as `aws-auth`, and a
  Grafana file's contact points re-pointed, each taken for rules by their
  shape); never pushed over a branch there already, which a reviewed pull
  request may be from; at most `MAX_CHANGES` branches a run (10).
- **API: one credential a target, scoped.** Each in its own file
  (`--changes-grafana-token-file`, `--changes-aws-role`, and so on), given
  only what changing rules needs:
  - Grafana: a service account allowed to write alert rules in the folders
    named, nothing else. An Editor's changes a `for` or what a rule says;
    turning a rule off takes an Admin's: Grafana 13 answers a pause from
    an Editor as taken and leaves the rule running, which Lapilli finds
    when it reads the rule back, and says. Lapilli has no folder scope of
    its own, as `--changes-in` is a ruler's (0.28.1's review): the token
    is the scope. An Editor's token limited by folder permissions keeps a
    stolen Lapilli to those folders' rules, but can't turn a rule off; an
    Admin's turns rules off and reaches the whole organisation, folders,
    data sources and users included. Give the Admin's only where turning
    rules off is wanted, and otherwise an Editor's on the folders Lapilli
    may change.
  - AWS: `aps:PutRuleGroupsNamespace` on the namespaces named;
    `cloudwatch:PutMetricAlarm`, `DeleteAlarms` and `DisableAlarmActions` on
    alarms with a name prefix the operator chooses, in the alarms' own
    account: CloudWatch names an alarm by its name in the caller's account,
    so a write as a role of another is refused.
  - Azure: a custom role allowing write and delete on the rule resources, in
    the resource groups named.
  - Google Cloud: `monitoring.alertPolicies.update` and `delete` on the
    projects named (Google has no finer grant). Its `Rules` resources go
    by the git path, with no credentials.
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
   for Prometheus, Azure's rule groups, read with `--ruler` and written
   through their APIs; Google Cloud's rule resources by the git path.
   Designed in [rulers.md](rulers.md).
3. **A cloud's own alarms:** CloudWatch, Azure Monitor, Google Cloud
   Monitoring and OCI, read with `--alarms` and written through their
   APIs. Designed in [alarms.md](alarms.md).

Each step says, as the rest of Lapilli does, which paths were tried against
a real account and which against documented answers only.

## As built

The first step, for v0.22.0, as above, except:

- **Lapilli proposes of itself only with `--changes`,** once a week after
  the digest, ten at most, and not again what people dropped or what was
  refused: without it, proposals are made by people from a rule's page and
  by agents with `--mcp-propose`, so a ledger that was only reading doesn't
  start sending `proposal.made` to its webhook and Slack.
- **Four more states.** `ready` is a change checked again and waiting for a
  recipe; `applying` a change sent to Grafana and not yet read back;
  `merged` a pull request merged but not yet read back; `amended` one
  merged changed in review (15m made 20m), the rule read as neither it was
  nor as proposed, so the change approved isn't the one in effect: it isn't
  undone from here, or proposed again by Lapilli, and is told as
  `change.amended`. `opened`, `merged` and `applied` are as above; a pull
  request closed unmerged makes the proposal `dropped`. An editor drops one
  proposed or approved; an approver or an admin also one `ready`, which
  waits for as long as no recipe runs (a pull request a recipe opened
  meanwhile is left for a person to close). An admin also closes, saying
  why, one `opened` or `merged` (its rule moved elsewhere in review, say),
  which Lapilli then no longer follows, the pull request left as it is;
  and one `applying`, which is read back first, and closed as `failed`
  only if it still can't be and the admin says the change didn't reach
  Grafana.
- **One at a time for a rule,** of any kind, undo included: two changes to
  one rule, both merged, would leave neither as it expects the rule, and
  neither would be seen applied. A fix to what a rule says isn't kept apart:
  its hash covers when the rule fires, and a Grafana rule's digest covers
  what it says. A person, with the agents acting for them, makes twenty
  proposals an hour at most; Lapilli's own are bounded apart. An agent
  acts for the person its request to `/mcp` came from, as the sign-in says
  of that request, whichever it is; with a sign-in of lapilli's own or in
  front, a proposal for no one known is refused (until the release after
  0.37.0, such an agent's proposal was kept as "anonymous"'s: its person
  could approve it, and every agent shared one twenty an hour). The incident
  a proposal is made for is an ID the ledger has, written into the pull
  request as code. A proposal made into a task gives the task none of its
  links, so the tracker job opens the task's issue whenever the pull request
  opened: the proposal keeps the pull request's link, and the task's page
  shows it through the proposal it carries.
- **Approval** is a form on the proposal's page, signed for the person, the
  proposal as it stood and how many had approved it, with a key the
  process makes when it starts (a restart asks for the page again); it must
  come with `Sec-Fetch-Site: same-origin`, which a browser sets and a page
  elsewhere can't. Since phase 4 of the web UI, the form has a box, "I have
  read the backtest" ("the change and its evidence" for one with none),
  `required` on the page and checked by the server: an approval without it
  is refused, 400, and nothing is kept. To a reader who may not approve,
  the page shows the button disabled, with why. That keeps out the API, MCP, tokens, other sites and a
  script with a stolen key, not someone who can forge the sign-in proxy's
  headers, who could be anyone already. Nor an agent the proxy lets in as
  a person: any client that isn't a browser can send the header, read the
  page and post its form. So the proxy in front of Lapilli should let in
  only people, by a browser's sign-in, on `/proposals/`, and give an
  agent's token its own name, never an approver's. `--changes` needs `--user-header`
  and roles, `--editors` or `--admins`, since without them everyone is an
  admin, who could resume changes paused; and someone to approve,
  `--approvers` or else `--admins` (the chart checks the same). With
  `--approvers`, only those listed approve, else the admins. No one approves their own proposal, or
  one their agent made, or one twice, in any letter case: names are folded
  in Go, Unicode's cases too, before they are kept and compared. A proposal
  with the approvals `--approvals` asks for, lowered since, is approved;
  one approved with fewer than it asks for, raised since, and not yet
  sent or taken by a recipe, waits for approval again, and isn't carried
  out or handed out (an undo, asked for by one approver, goes on).
- **The backtest** is over the twelve weeks to now, or from when the runs
  began if later, within `--timeout`, one at a time (with MCP's `compare`).
  One that doesn't finish is refused when checked again. Retiring carries
  the rule's record over twelve weeks instead, and is refused when checked
  again if the rule has caught more that no other rule did.
- **Finding the file** is by the hash of the whole document Lapilli read,
  only. A rule file mounted from a ConfigMap whose manifest is in git, and
  rules read back from a cluster, aren't matched by the rules inside them:
  the recipe tells Lapilli the change failed, and why, and the proposal's
  page keeps the diff and a pull request's description, as it does for
  rules rendered from a chart. Recipes open no issues.
- **The edit** finds the rule in the YAML's tree and edits the text on the
  lines it is on, so the rest of the file stays byte for byte. What it
  writes is read back twice before it is kept: the YAML's tree compared
  whole with the change made to the old one, and the rules parsed again,
  every other rule as it was. Rules written inline (flow style), held in a
  quoted ConfigMap value, or two alike in a file aren't edited. Deleting a
  group's only rule deletes the group; a file's only rule isn't deleted. A
  change to a file whose other lines changed since is made afresh in it.
- **Grafana** takes a new `for`, turning a rule off (`isPaused`) and
  deleting it. A Grafana rule's threshold is for a person, in Grafana. The
  rule is read again through its own export and compared whole; any
  provenance (`file`, or `api` from another tool) is refused, so
  `--changes-managed` isn't built: provenance says it. Just before a change
  is sent, the pause is looked at again, and the proposal is kept as
  `applying`, with the rule as it was and counted against the limits. What
  became of it is read back, never taken from Grafana's answer: changed, it
  is applied (a delete that answered 504 too); as it was, failed;
  unreadable, it stays `applying` and is read again each round, never sent
  again. The pause is looked at again, too, just before a change is handed
  to a recipe.
- **Undo** is a proposal of its own, approved by the approver who asks
  (since phase 4 of the web UI, ticking the same box an approval takes,
  checked by the server): on the git path the file written back as it
  was, refused if the file changed since (revert it in the repository); in
  Grafana the rule as it was kept, put back whole, what was edited in
  Grafana since that doesn't change when it fires (its annotations) put
  back with it, which the page says, or made again with its uid when it
  was deleted. The page asks
  for it as "Ask to undo it", and says that asking is the approval: one
  approver's, whatever `--approvals` asks for of a change. Its page and the
  page of the change it undoes link to each other, and each says, in its
  history, what happened to it. It keeps of the change only the edit
  (where it was written, what of the rule it touched): the change's words
  aren't the asker's, and its backtest says what the change did, so the
  undo's page reads it from the change, the other way round.
- **Deleting after turning off** needs four weeks since the rule was off
  (since the disable took effect), the rule off still, and the rule as it
  was before, replayed over them, catching no incident that nothing caught.
  A rule turned on again by hand isn't proposed for deletion, or deleted. A
  Grafana rule (one that can be turned off) is never deleted outright: a
  deletion follows the disable that turned it off, the latest when none is
  named, and is refused without one; one turned off by hand is deleted in
  Grafana.
- **The limits** count each change once, when it is handed to a recipe or
  sent to Grafana, every kind; never again when it takes effect, and never
  a change a person made (path none). Deletions are those of kind delete.
  Limits of zero deletions are allowed.
- **The stop** is an admin's switch on the settings and proposals pages, or
  the file; the chart's file is a ConfigMap, `FULLNAME-changes-paused`
  (FULLNAME the release's name followed by `-lapilli`, unless it has
  `lapilli` in it already: `ops-lapilli-changes-paused` for a release named
  `ops`, `lapilli-changes-paused` for one named `lapilli`; `helm install`
  prints the command), which anyone who may make one in the namespace can
  make, since the image has no shell to touch a file with. Who turned the
  admin's switch, and which way, is noted in the changes.
- **A merge told late.** A deletion is seen applied only once it was
  merged; when the merge is told after Lapilli read the merged file, the
  rules read last are looked at again then.
- **The recipes** take as a change's only a pull request from the
  repository itself, into `BASE`, opened by the account their token is:
  one from a fork with Lapilli's branch name never takes a proposal over.
  When the host refuses to open another, as one is open from the branch,
  the one from Lapilli's branch in the repository itself, into `BASE`, is
  taken, whoever opened it: a fork can't push to that branch, and a run
  that stopped after opening one, with a token the host can't say whose
  it is, would otherwise leave the change `ready`, refused each run.
- **A fix to what it says** is a kind of its own, `says`: one annotation
  of one rule (`summary`, `description`, `runbook_url` or `runbook`, those
  the content check reads; Grafana's `__dashboardUid__` and `__panelId__`
  never) set to a text a person or an agent wrote, or taken out. Lapilli
  never writes the text, so it proposes none of itself: the rule's page
  offers a form beside each finding the text can fix (no summary or
  description, no runbook, a runbook that isn't a link), and the
  proposal carries that finding as its evidence, read again from the
  content check rather than from the form. One a person makes unprompted
  carries none. Nothing to backtest: it doesn't change when the rule
  fires. A summary or a description is 4 KB at most, with no control
  characters but a description's line breaks and tabs; a runbook is an
  https URL of 2,048 characters at most, with no user in it. Each is a
  template where the rule is kept, so it must parse in the rule's template
  language (Prometheus refuses a whole file for one that doesn't, and the
  operator a whole PrometheusRule), calling only the functions that
  language has (those every Prometheus since 2.45 has, as `toDuration`,
  `now` and `urlQueryEscape` came later; Grafana's, those with its own; a
  misspelt `humanise` is refused, as Prometheus would refuse it), and call
  no `query`, which would run PromQL each time the alert fires. Nor may it
  grow without bound when it is worked out, each time the alert fires: no
  `range`, no template defined or called, no variable set; an escape or a
  replacement (`js`, `html`, `reReplaceAll`, `graphLink` and the like)
  only on a label, a value or a text as it is, never on what another
  function gave; `printf` only with a format written in the text, its
  widths 99 at most. Twenty-two nested `range`s over four labels would be
  17 TB. All of it is checked
  when proposed, read back, and
  before Grafana is sent it. In a file it is written on a line of its own
  as a double-quoted scalar, so no text can be read as another key, and
  read back: every other byte as it was, the rule firing as it did. A rule
  in a Grafana provisioning file is compared but for its digest, which
  covers what it says, as the YAML's tree is compared whole. It is stale when what the rule says changed,
  not only when it fires; and as it makes no new version of the rules, it
  is applied when the rules read next say it. In Grafana the one
  annotation is set or taken out, the rest of the rule as Grafana gave it.
- **Not yet:** proposals in the weekly digest's tasks line, and a
  proposal carried by a task made elsewhere (a proposal can be made into a
  task).

The git path is tested against stand-ins for each host's API and local
git repositories, and Grafana's against a stand-in of its provisioning
API; since v0.22.1 both also run against real servers
(`test/e2e/changes.sh`). The managed Prometheus rulers, step 2, are built
for v0.23.0 as [rulers.md](rulers.md) says: a ruler's rules take a path of
their own, `ruler`, beside `git`, `grafana` and `none`; `applying` is a
change sent to Grafana or a ruler and not yet read back; and Azure's rules,
which can be turned off, are retired as Grafana's are.

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
