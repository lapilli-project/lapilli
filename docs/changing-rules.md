# Changing rules

Lapilli reads by default, and changes only what a person approves
([docs/design/changes.md](design/changes.md)). It proposes changes to
rules from what it finds: a longer `for` for a noisy rule, the threshold or
`for` that would have caught a near miss, retiring a spare rule or one that
never fired in twelve weeks. Each proposal is one rule, with the change
written as where the rule is kept expects it, and its evidence: the change
backtested over the twelve weeks to now (firings, incidents caught, lost or
caught later), or, for retiring, what the rule did, with the caveat that a
rule may wait for a rare incident. People propose from a rule's page too,
and agents over MCP. Proposals are on `/proposals`, on each rule's page,
and marked on the rules list; one can be made into a task. A rule has one
proposal waiting at a time, and a person, with their agents, makes twenty
an hour at most.

A fix to what a rule says, one annotation (`summary`, `description`,
`runbook_url` or `runbook`) set or taken out, is proposed the same way, with
the text written by a person or an agent: Lapilli writes none itself. The
rule's page offers one beside each finding of the content check that the
text can fix, a missing runbook, say, and the proposal carries the finding.
It doesn't change when the rule fires, so it has no backtest, and the rest
of the rule comes out as it was, byte for byte. The text must be a template
that parses in the rule's language, calling only the functions it has
(Prometheus's since 2.45; Grafana's, with its own) and never `query`:
Prometheus refuses a whole file for a function it hasn't. What a template
fills in is written as the alert's labels and values have it: in a link
(a `runbook_url` of `https://wiki.example.com/{{ .Labels.service }}`), a
value isn't URL-encoded unless the text says so (`urlquery`), so one with
a `/`, a `?` or a `#` in it changes where the link goes. Lapilli checks
that the template parses, not what it makes of a link; the content check
shows the runbook as a link only with nothing a template fills in.

A new rule is proposed too, from the proposals page's **Propose a new
rule** or an agent's `propose` (kind `add`), held to what it is to do
before anyone approves it
([docs/design/expectations.md](design/expectations.md), step 5): one
Prometheus alerting rule, named apart from every rule (one added and
applied through an API counts before the rules are read again), a place
among those serve offers (a rule file's group or a new one, a Grafana
folder's group, not of a folder whose title another folder has too, a
ruler namespace's group or a new one, CloudWatch's or Google Cloud's
alarms, with a `for` the cloud takes: whole seconds, in CloudWatch a day
at most), whom it tells among whom the rules there already tell (in
Grafana, one of their contact points where each of them names one, none
where none does; in CloudWatch, an SNS topic, an OpsItem or a response
plan, never a Lambda function or an automation; in a rule file or a ruler,
no one: its labels route it through Alertmanager), and one expectation at
least; its labels are plain text, and with its group's they say its team,
whose editors may propose it (and are shown its form) and whose
approvers, freezes and `changes_in` hold it. Lapilli writes the rule
itself where it goes, replays it over the twelve weeks with nothing
beside it, and holds it to each expectation; no one approves it while
one is broken, or its backtest didn't replay it, and it isn't carried
out once checked again if one is broken, one it met as it was approved
isn't known now, or its group's labels give it another team than as it
was proposed. It goes the way a change goes there; undone,
the rule is deleted; once added, its expectations go where an accepted
draft of them goes ([Keep judging](serve.md)).

Nothing is changed until a person approves it, on its page, in a browser:

- `--approvers` names who may (people and groups, as `--editors` does); the
  admins when it isn't given. `--changes` needs roles (`--editors` or
  `--admins`), as without them whoever may change anything is an admin,
  and someone to approve. `--approvals 2` asks for two people, of a proposal approved by
  one before it was raised too, unless it was carried out already. No one
  approves their own proposal, or one an agent made for them, in any letter
  case. An undo of an applied change is the exception: the one approver
  who asks for it approves it, whatever `--approvals` says.
- The form is signed for the person and the proposal as it stood, and comes
  from the page itself (`Sec-Fetch-Site: same-origin`). The API, MCP and
  every token can't approve.
- The page shows the change as a diff and its backtest, now beside as
  proposed; approving, and asking for an undo, takes a box ticked, "I have
  read the backtest", which the server checks too.
- What a proposal's evidence says is worked out when it is read, whatever
  version of lapilli made it: whether its `for` is one one event can't
  outlast, and whether its backtest's period had an incident written down
  to judge its firings by. With none, the backtest counts its firings and
  doesn't judge them: fewer aren't better, nor more worse.
- A proposal waiting for approval whose backtest was replayed or judged
  otherwise than this lapilli does, or whose period's record changed
  since, is stale, and isn't approved until it is backtested again:
  with `--changes`, Lapilli does it itself for those an earlier lapilli
  backtested, a few each run of its work, and an editor can
  from its page, with this lapilli, over the twelve weeks to now, the old
  backtest kept in its history and the approvals given to it withdrawn and
  told (`proposal.backtested`). `/proposals?stale=1` lists the stale ones.

`--changes` turns carrying them out on; without it Lapilli only proposes.
With it, each approved change is checked again first: the rule is read
again and refused if it changed, the backtest is run again and refused if
it says something worse than what was approved (an incident caught that
would now be missed), and the rule as it was is kept for undo. Then:

- **A rule in git** is handed to a recipe in
  [recipes/changes](../recipes/changes/README.md), which runs where the
  repository's credentials are and opens a pull request: GitHub, GitLab,
  Bitbucket, Gitea or Forgejo, or Azure DevOps. The recipe reaches
  `--incidents-listen` with the token in `--changes-token-file`, which can
  read the changes approved and say what became of each pull request, and
  nothing else. The recipe takes as the change's only a pull request it
  opened: from the repository itself, into the base branch, by its own
  account, or, when the host refuses another, the one open from Lapilli's
  branch there; never one from a fork. The change is written into the file as
  it was read, comments, order and quoting kept. A merge isn't the change
  applied: it is once Lapilli reads the rules with it, and the outcomes
  page measures it from then.
- **A rule kept only in Grafana** is changed through its provisioning API
  with the token in `--changes-grafana-token-file` (a service account that
  may write the rules Lapilli may change). One Grafana keeps from files, or
  another tool keeps through its API, is refused. The rule stays editable in
  Grafana's UI. What became of the change is read back from Grafana, not
  taken from its answer; until it can be, the change is kept as applying,
  with the rule as it was, and never sent again.
- **A rule kept in a managed Prometheus ruler**, read with `--ruler`, is
  changed through the ruler's own API, and only in the namespaces (Azure:
  the rule groups, `RG/NAME`) `--changes-in` names, a name or a prefix
  ending in `*`: a ruler can't say which of its rules a tool keeps, so the
  operator says which it doesn't. Name none that mimirtool, Terraform or
  Alloy's `mimir.rules.kubernetes` syncs. An Amazon namespace or an Azure
  rule group tagged as another tool's (CloudFormation's own tag, or one
  `--changes-managed KEY=VALUE` names) is for a person. What a write
  replaces, a Mimir or Cortex group, an Amazon namespace or an Azure rule
  group, is read again just before: the rule itself changed since is
  refused as stale; only other rules in it changed, the change is made
  afresh, theirs as they now are. It is kept for undo, then written with
  the write credential of its kind (`--changes-ruler-token-file`,
  `--changes-aws-role`, `--changes-azure-client-id`), and read back: an
  Amazon namespace stays applying while it is `UPDATING`. A group goes
  with its last rule; an Amazon namespace's or an Azure rule group's last
  rule is for a person.
- **A cloud's own alarm**, read with `--alarms`, is changed through its
  cloud's API, and only where `--changes-in` names it as `SOURCE:NAME`:
  CloudWatch's alarm name, Azure's `RG/NAME` (in any letter case), Google
  Cloud's policy's display name, OCI's display name, or a prefix ending in
  `*`. One tagged as another tool's (CloudFormation's own tag, Config
  Connector's or Terraform's label, or one `--changes-managed` names, an
  OCI defined tag as `NAMESPACE.KEY=VALUE`) is for a person; name none
  that CloudFormation, Terraform, Bicep, Pulumi or Config Connector keeps.
  A `for` and a threshold where the alarm is PromQL (CloudWatch's PromQL
  alarms, Azure's PromQL metric alerts, Google's PromQL conditions),
  backtested; what it says on every cloud, a runbook its own form (in a
  cloud that keeps one description, the first link in it, or a line of its
  own); turning it off on every cloud. What is written is the least each
  API takes: CloudWatch's whole alarm as read with the change
  (`PutMetricAlarm`), its actions alone off and on
  (`DisableAlarmActions`, `EnableAlarmActions`); Azure's properties
  changed (`PATCH`; an activity log alert's description, a `PUT` of the
  whole rule), with `If-Match` for a log search alert; Google's fields in
  `updateMask`; OCI's fields, with `if-match`. CloudWatch's alarms whose
  actions scale or act on an instance aren't changed at all, one that
  warms up isn't rewritten, a composite alarm is turned off and on alone,
  and one with a field this Lapilli's model doesn't have isn't rewritten;
  a Google policy of several conditions takes a condition's `for` and
  threshold alone. The alarm is read again just before, as a ruler's unit
  is, and CloudWatch's and Google's once more just before the write; it is
  written with its cloud's write identity (`--changes-aws-role`,
  `--changes-azure-client-id`, `--changes-google-service-account`,
  `--changes-oci-config`) and read back, a round later again where the
  cloud may be read a moment behind.
- **Anything else** (rules read with `--loaded-rules`, rendered from a chart,
  a cloud's alarms read from a file, a Grafana rule's threshold) gets no
  edit: the proposal's page has the diff and a pull request's description,
  for a person to make it where the rule is written.

Retiring a Grafana rule, an Azure one or a cloud's PromQL alarm turns it
off first; four weeks later Lapilli proposes deleting it, approved again,
and only if it is still off and nothing it would have caught was missed
meanwhile. It is never deleted outright. A cloud's alarm Lapilli can't
replay is retired as far as off, never deleted from here: its page says to
delete it in the console once it has been off a while. A rule file has no
off, nor so a Mimir, Cortex or Amazon ruler: there, retiring is the
deletion. Turning a rule off, or deleting it, waits while it fires, on
every path: while the ledger has an alert of it still on (a cloud's
alarm's, by its ID, whatever its history is named), or the cloud says it
fires: CloudWatch's `ALARM`, an Azure alert of it whose monitor condition
is `Fired`, an open Google alert of the policy, OCI's `FIRING`. Every applied change can be
undone from its page by an approver, the same way it was made; in a ruler
or a cloud, undo puts back the rule as it was kept, the rest as it is now.

At most five deletions a day and twenty changes an hour are carried out
(`--changes-limits`), each counted once, when it is handed out or sent; a
change past them waits. An admin can pause changes
in the settings, and so can whoever runs the pod, by making the file in
`--changes-paused` (the chart: a ConfigMap): nothing is carried out or
handed out while they are paused, and every page says so. A pull request
merged changed in review, its rule read as neither it was nor as
proposed, is `amended`, not applied. An admin closes, saying why, one
whose pull request is open or merged but never read back, and one sent to
Grafana or a ruler that can't be read back, once they say it didn't reach
it.
Every step is kept in the ledger's changes, and told as `proposal.made`,
`proposal.approved`, `proposal.backtested`, `change.opened`,
`change.applied`, `change.amended`, `change.refused`, `change.failed` and
`change.undone` to the webhook and Slack.

The git path, Grafana's and Mimir's run against real servers in
`test/e2e/changes.sh` (Gitea 28.3, Prometheus 3.5, Grafana 13.3, Mimir
3.2.4), and against stand-ins in the unit tests; Cortex's against a
stand-in at its paths, Amazon's and Azure's rulers, and the four clouds'
alarms, against stand-ins answering as their API documents show. None yet
against an account of any cloud.
