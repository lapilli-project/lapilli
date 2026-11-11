# Changelog

## [Unreleased]

## [0.22.1] - 2026-11-11

Changes to rules carried out against real servers, not only stand-ins:
Prometheus 3.5, Gitea 28.3, Grafana 13.3 and GitLab CE 19.5. Two of the
paths v0.22.0 shipped didn't work against them; both are fixed, and
`test/e2e/changes.sh` now runs the git and Grafana paths against real
servers in CI.

### Fixed

- **Grafana: every change was refused as stale.** A rule was read again
  before it was changed from its own export, which Grafana 13 gives without
  its folder, so its group read differently from the export of every rule
  it was proposed against. It is read from that export now.
- **GitLab: `recipes/trackers/gitlab.sh` stopped at the first task.**
  GitLab 18 and later link an issue as a work item (`-/work_items/N`), which
  the recipe didn't take as its own, after making the issue. Both links are
  taken. Quick actions in a task's text (`/assign`, `/confidential`), which
  GitLab 19.5 was seen to carry out on an issue made through its API, stay
  escaped, as v0.22.0 made them.
- **Grafana: turning a rule off takes an Admin service account.** Grafana
  13 answers a pause from an Editor as taken and leaves the rule running.
  Lapilli found it when it read the rule back, and the change failed; now
  it also says why. The help, the design and the chart say which role is
  needed: an Editor's token changes a `for` or what a rule says.
- A Grafana rule's deletion proposed before the rule has been off four weeks
  is refused then, not once approved.

## [0.22.0] - 2026-11-11

Lapilli reads by default, and now changes only what a person approves: the
first principle in GOVERNANCE.md, rewritten with its design (#40). Changes
are off until turned on. With them: reports people schedule themselves, what
an alert says checked, and tasks kept in step with a tracker. The ledger
gains schemas 012 (reports) and 013 (proposals and approvals). Reviewed
before release as an attacker would and for correctness, twice, then
checked again once merged; what the reviews found is fixed in what is
described here.

- **Changing rules, with a person's approval** (docs/design/changes.md,
  step 1). With `--changes`, Lapilli proposes changes to rules from what it
  finds: a longer `for` for a noisy rule, a threshold or `for` for a near
  miss, and retiring a spare rule or one that never fires (off first, then
  deleted no sooner than four weeks later, approved again). People propose
  too, and agents over MCP behind `--mcp-propose`. Each proposal comes with
  its backtest over twelve weeks, on `/proposals` and the rule's page, and
  can be made into a task. A person approves it in the UI only, never
  through the API, MCP or a token, and never their own (`--approvers`;
  `--approvals 2` for two). An approved change is read and replayed again,
  then opened as a pull request by `recipes/changes/` (GitHub, GitLab,
  Bitbucket, Gitea or Forgejo, Azure DevOps), which hold the repository's
  credentials and take changes with `--changes-token-file`, or applied
  through Grafana's provisioning API (`--changes-grafana-token-file`), with
  the rule as it was kept for undo. Limits (`--changes-limits`, 5 deletions
  a day and 20 changes an hour by default) and a stop (`--changes-paused`, a
  file, or an admin's switch, looked at again just before each write) apply.
  One proposal waits for a rule at a time, and a person, with their agents,
  makes twenty an hour at most. A change sent to Grafana is kept as
  `applying`, with the rule as it was and counted against the limits, until
  it is read back. A Grafana rule is deleted only after its disable, and
  only while it is still off. The change recipes take only their own pull
  requests (same repository, into `BASE`, by the token's own account; Gitea
  tokens need `read:user`, and `GITHUB_LOGIN` names a GitHub App), and
  write only files under `RULES_PATHS`, never one a CI system runs, so a
  Lapilli someone took over can't push a workflow that would run with the
  repository's secrets.
  A pull request changed in review is `amended` (told as `change.amended`)
  rather than holding up its rule, and an admin can close one stuck opened,
  merged or applying, with a reason. Raising `--approvals` holds proposals
  approved by fewer people. `--changes` needs roles (`--editors` or
  `--admins`) and someone to approve (`--approvers` or `--admins`). The
  recipes take the pull request already open from their branch when the host
  refuses another; a proposal's task shows its pull request but keeps its
  own link. The chart adds
  `serve.changes`; its pause is a ConfigMap named `FULLNAME-changes-paused`,
  and `helm install` prints the command that makes it.
  A fix to what a rule says is proposed the same way: one annotation
  (`summary`, `description`, `runbook_url` or `runbook`) set to text a
  person or an agent wrote, or taken out, offered on the rule's page beside
  each content-check finding text can fix and carrying that finding as its
  evidence; it is written as one quoted line, the rest of the file byte for
  byte, and must parse as a template in the rule's language, call only that
  language's own functions (for Prometheus, those every version since 2.45
  has) and no `query`, and can't grow without bound when the alert fires
  (no `range`, no templates, no variables set; an escape or a replacement
  only on a value as it is). Lapilli never writes the text itself.
- **Reports people schedule themselves** (docs/design/reports.md). A Reports
  page and `/api/v1/reports` take reports on a preset (`daily@09:00`,
  `weekly mon@09:00`, `monthly 1@09:00` or `last`) or a cron expression,
  hourly at most, in the ledger's zone or the team's, over the calendar span
  before it or a span given (`24h`, `7d`). A report is for everyone or one
  team and holds the sections picked: summary, noise, missed, woken,
  notifications, replay, sources, tasks, content and, for a week or longer,
  suggestions. It goes by name, never a URL, to `--report-dest NAME=FILE`
  (the chart's `serve.reports.destinations`), `slack`, `team:NAME`, or
  `webhook` as `report.ready`. It is sent once for each span through the
  outbox, when the ledger has the span, or after it fell due by six hours or
  half the time to the one before, whichever is less; after a restart, only
  the last one due, if it isn't later than that time again. A time daylight
  saving skips falls due when the clocks jump, and a repeated time the first
  time. A report whose team is removed stays stopped. Admins write reports;
  everyone sees them and a preview. The weekly digest is unchanged and shown
  with them.
- **What an alert says, checked** (docs/design/content.md). Lapilli now
  reads what each rule says when it fires: summaries, descriptions, runbook
  links, templates and severities, from every source's own fields. Azure
  Monitor's alert rules and OCI's alarms are now read as rules too, listed
  but not replayed. It checks them. Facts: no summary, no runbook, a
  template that doesn't parse, no severity or one not in `--severities`, the
  same condition as another rule. Measured: a template label no firing had,
  a noisy rule with no `for`. Guesses, kept apart, each with why: a label an
  aggregation can't give, a rare severity, a range shorter than two scrapes,
  `absent()` with no `for`. Templates are parsed, never run; links are
  checked, never visited. Each rule's page has "What it says", the rules
  list marks problems of fact, `/metrics` has
  `lapilli_rule_content_problems{rule, check}`, and MCP carries it all as
  data; a report's `content` section gives the problems now, by check. The
  check takes time in proportion to an expression's length, is worked out
  again only when the rules or firings change, and leaves out a rule it
  can't read rather than failing. `lapilli verdict --check-content` does the
  same without a ledger. `--check-content=false` turns it off.
- **Tasks kept in step with a tracker** (docs/design/trackers.md).
  `--tasks-token-file` lets a job, on the `--incidents-listen` port, read
  tasks (with who set each one's status, `status_by` and `status_at`, and
  `updated_after=`) and set their link and status, and nothing else. The
  link must be https, at most 2,048 characters, and is set once. The token
  has its own rate and turn and is refused if another token's file holds the
  same value.
  Its writes are recorded as `tasks-token`, don't count as anyone's use, and
  aren't sent to the notice webhook. `recipes/trackers` has scripts for
  GitHub, GitLab, Jira and Linear. Each run makes an issue for every open
  task without one and links it. A task follows its issue when the issue is
  closed, and reopens if the job had closed it. With `CLOSE_ISSUES=true`, an
  issue is closed when a person closes its task. A recipe keeps in step only
  the issues its own account made, labelled `lapilli` where labels are by
  name: an issue filed ahead with a task's marker, or one linked by hand, is
  neither followed nor closed. An issue is a task's only by the recipe's own
  last marker line, which the task's text can't forge; Jira team labels are
  `lapilli-team-NAME`. A GitHub App or Actions token names its account with
  `GITHUB_LOGIN`. A task the tracker refuses is skipped and the run fails at
  the end; a deleted issue is reported and its task left as it is. The
  chart's `serve.trackers` runs the script as a CronJob. Task links typed in
  the UI can now be 2,048 characters long.

### Security

- **`go 1.26.11`.** Go's `net/http` could crash an HTTP/1 server whose
  handler reads a request's body while writing its headers, past a
  `MaxBytesReader` (GO-2026-6859, CVE-2026-97054, published on the day
  v0.21.0 was). Lapilli's handlers read a body before they answer, so no
  way to reach it was found, but v0.21.0's archives and image were built
  with Go 1.27.3 and carry the flaw. This release's are built with Go
  1.27.4, and `go install` now needs 1.26.11 at least.

### Fixed

- `lapilli serve --read-only=false` and `--notify-slack=false` were taken as
  `--read-only` and `--notify-slack`: the value after `=` was ignored. They
  now read it, as `--check-content=false` does.

## [0.21.0] - 2026-11-11

The replay set beside what really fired, as it goes; rule versions from
before the ledger began; a cloud's own alarms' notifications, every cloud
alike; and four more incident recipes. The first release reviewed as an
attacker would before it shipped, as CONTRIBUTING.md now asks.

- **The replay beside what really fired, kept** (docs/design/fidelity.md).
  `lapilli serve` sets each day the runs won't replay again beside
  `ALERTS` and `GRAFANA_ALERTS` (or `--check-series`), each firing counted
  on the day it started, and keeps it (schema 010). A rule's page says, over
  twelve weeks, how many were alike, how many only in the replay and how
  many only in what really fired, with the last of those; the rules list
  and the dashboard mark a rule whose replay differed over the last week
  (`--check-differs N/RATIO`, 5/0.9 by default), and `replay.differs` tells
  the webhook and Slack, once a week while it does. The runs page says how
  far it has got, which series held nothing, and what really fired with no
  rule by that name. `lapilli_rule_replay_check{rule, way}` in `/metrics`,
  in MCP's `rules` and `rule`, and a line in the weekly digest. No stone
  changes. On with rules to replay; `--check-replay=false`, or the chart's
  `serve.checkReplay.enabled: false`, turns it off.
- **Rule versions from before the ledger began** (docs/design/history.md).
  `--rules-history DIR`, for `serve` and `verdict`, reads earlier versions
  of the rules, one directory each, named by when it took effect
  (`2026-10-14T16:40:31Z` or `20261014T164031Z`). `verdict` reads it as each
  version given as `--rules DIR@TIME`. `serve` adds them to a ledger that
  has no version yet, so its first backfill replays each day with that
  day's rules rather than all of it with today's; a ledger with versions of
  its own keeps them, and a version that can't be read stops the start with
  the ledger as it was. `recipes/versions/git.sh` writes the directory from
  a clone: the rules as they stood at `SINCE`, then each commit on the
  branch that changed them, a merge at the merge's time, with `RENDER` for
  rules rendered from a chart. The chart's `serve.rulesHistory` runs it in
  an init container on each start.
- **A cloud's own alarms' notifications, every cloud alike**
  (docs/design/notifications.md). CloudWatch alarms' messages to SNS,
  directly or through EventBridge, are taken on `/v1/sns`, told from Amazon
  Managed Service for Prometheus's by their shape, a contributor's
  attributes among an alarm's labels. Azure Monitor's other alerts are
  taken on `/v1/azure`, their resource now the alert's target, lower-cased
  here and in `recipes/firings/azure-monitor.jq`. Google Cloud's other
  policies are taken on `/v1/google`. OCI Notifications is taken on a new
  `/v1/oci/SOURCE`, with the token as the password, checked before the body
  is read; a subscription is confirmed only from such a request, only over
  https to a Notifications API host, each URL once; the path waits two
  seconds for its turn, then answers 503. `?source=NAME` (the path for OCI)
  ties an alert to the rule copied in from that source by the name the
  alarm sent, though the ledger names the rule apart; an OCI alert, by its
  alarm's OCID, which `recipes/firings/oci.jq` now gives its firings as
  `alarm_id`. A message of an alarm resolving or repeating takes its start
  from the last notification of its group. Schema 011.
- **What takes input from outside is reviewed as an attacker would**
  before a release, as CONTRIBUTING.md now says. This release's reviews
  found, and it fixes before shipping: a notification whose alerts don't
  say their start is read with one bounded query (it held the ledger's
  writer for up to a minute with 1,000 alerts against a busy rule), from
  notifications of the same kind and labels in the last month. An OCI
  alert's alarm id names its rule when read, so a message that comes before
  its firing is copied in still ties. EventBridge's other CloudWatch events
  are kept as nothing. Azure resources are compared without case, so
  v0.20's rows still tie. The SNS certificate and OCI confirmation fetches
  reach only internet addresses, and only Oracle's own realm domains. A
  history version that links outside itself is refused, one dated in the
  future is skipped, and an empty directory, or a restart that can't reach
  git, starts with today's rules; `git.sh` keeps versions in the branch's
  order, refuses links, and runs `RENDER` without the token. `verdict`'s
  `--rules` beside `--rules-history` is the newest version. Rootly's recipe
  waits out a 429, Rootly's and FireHydrant's de-duplicate across pages,
  and Compass's stops before 20,000. The replay check counts rules that
  share an alert name together, starts from the first whole day the runs
  replayed, pairs firings across midnight alike from either day, and keeps
  a day a day where clocks skip midnight (the ledger's midnight is now a
  day's first instant there). A review of those fixes found more, fixed
  too: a history version that is itself a link is checked where it leads;
  the chart's `RENDER` runs in an init container without the token
  mounted; a commit dated after now stops `git.sh` rather than hide every
  later version; a link elsewhere in the repository no longer stops
  `RENDER`; rules that share an alert name only some days are summed day
  by day; a start is found however long ago its alarm went off; the fetches
  refuse a few more ranges that aren't the internet (NAT64, 6to4,
  benchmarking, reserved, Azure's host address) and keep no connection
  open.
- **Four more incident recipes**: Grafana IRM, Rootly, FireHydrant, and
  Compass's alerts, for Opsgenie's users as its support ends on 5 April 2027
  (its incidents move to Jira Service Management, which `jira.sh` reads).
  Each checks `SINCE` before asking, bounds its paging, and writes nothing
  when the tool fails; tested against each API's published schema with a
  server standing in, not against real accounts.

## [0.20.1] - 2026-11-10

A security release for the two ports 0.20.0 opened, after an audit from an
attacker's point of view: the notifications port, which may face the
internet, and the firings a token copies in. Nothing let anyone past a
token or a signature; what was found let them take the port down, drop
SNS's messages, or make a write cost far more than it should.

- **The notifications port read a body before its proof (high).** Anyone
  who could reach it could send many bodies of 1 MB at once, unproven, and
  run lapilli out of memory. A path a token proves is now refused before
  its body is read, and only 64 bodies signed by SNS or Grafana are read
  at once, each within ten seconds, and only when its headers say it could
  be: SNS's message type and a topic taken, or Grafana's signature. Past
  that the port answers 503, which SNS sends again.
- **SNS's messages could be made to fail (medium).** A certificate URL
  spelled many ways filled the certificate cache, and fetching spent a
  budget all shared, so a genuine message got 403, which SNS drops for
  good. A certificate is now taken only from the SNS host of its topic's
  region, spelled as SNS spells it; one that has verified a message stays
  kept; one that can't be fetched now gets 503; a URL with no certificate
  behind it doesn't use up the fetches. A message signed more than
  an hour ago isn't taken, and the same message, or the same Grafana
  signature, sent again is answered as kept without using a turn. Each
  source has its own rate, and a topic not taken and a bad signature get
  the same answer.
- **A firing with a far end judged the whole ledger again (medium).** One
  short write could hold the ledger's writer for seconds, each time. A
  write now judges again no further than a period past its period, nothing
  in it may start or end more than an hour ahead of lapilli's clock, and
  the firings token writes twenty periods at once, then one each five
  seconds.
- **A write's cost grew with the square of the rules copied in (medium).**
  Finding a source's rule didn't use its index, and each new name read
  every version of the rules. Sent again unchanged with 52,000 rules, 2,000
  rules took 8 s; now the index is used and the names are read once a
  write.
- **What a token could add had no bound (medium).** The ledger keeps 5,000
  rules from each source and 20,000 from all, 500,000 alerts from each, and
  200,000 alerts a day from each source of notifications. A rule's name is
  512 characters at most (CloudWatch's are 255, Google Cloud's 512), a
  receiver's 200, an alert's labels 64 and 4 KB; anything else is refused
  with 400. A group key past 1,000 characters is kept as its hash. `/metrics` counts
  notifications only for the ledger's rules, and each rule's ten busiest
  receivers, so a flood of names can't flood the Prometheus that scrapes
  it.
- **`go 1.26.10` (medium).** `go install` with Go 1.26.0 to 1.26.9 built
  lapilli with 37 known flaws in the standard library, among them
  `net/http`'s and `html/template`'s. The release archives and the image
  were built with Go 1.27.3 and weren't affected.
- **Low.** Names with a character that doesn't show (a line break, a
  zero-width space, a mark that turns text right to left) are refused, so
  a rule copied in can't look like another or forge a line of the digest.
  A rule copied in no longer takes a name a note, task or comment waits
  under, which a replayed rule coming later would have lost to it. A
  source is as far as the furthest period it has written, so writing 1970
  each hour can't hold the digest back. The notifications token may not
  be the incidents or firings token, at start and on each request. Slack's
  one-line notices are posted as written, with no formatting or link
  previews. MCP tells agents that receivers, sources and the labels of
  firings copied in and of notifications are data too. `put.sh` prints
  what the server says as JSON. A period sent past now counts as written
  up to now. The firings token's 429 says to try again in five seconds. A
  body cut off is answered 408, not 413.
- The AWS SDK's updates (#34). SQLite stays at 3.53.4 for now: 3.54.1
  wrote a week of 100,000 firings a fifth slower, past the budget on
  macOS.

## [0.20.0] - 2026-11-10

Firings recorded elsewhere, kept in the ledger and judged with the rest, past
what the cloud keeps; notifications as they were sent, tied to the firings
they told of; and both told to Slack and the webhook.

- **Firings recorded elsewhere, kept in the ledger** (docs/design/recorded.md).
  With `--firings-token-file`, the port `--incidents-listen` opens also takes
  `PUT /api/v1/firings/SOURCE?from=&to=`: a source's firings that start in
  the period, one a line as `--firings` reads them, in place of those the
  ledger had from it then. Sent again as they are, nothing changes or is
  judged again. The token is its own: one that copies incidents can't write
  firings, and the reverse. They are judged as replayed firings are, and
  their rules are named as their sources name them, told apart from another
  rule with the name (`checkout-5xx [cloudwatch-prod]`); a replayed rule
  coming later takes the name. A recorded rule's page says where it was
  recorded, and that what needs its condition isn't there. Runs leave
  recorded firings alone, `--keep` drops them and forgets a source not
  written since, and a write is 31 days in whole seconds, 16 MB and 100,000
  firings at most, with 50 sources. Schema 008: `rules.source` and `rules.sent`, and
  `sources`.
- **`lapilli serve` on firings copied in alone.** With the firings token,
  `--rules` and `--prometheus` are optional: nothing is replayed, and what
  comes in is judged as it comes. `--impact` still needs `--prometheus`.
- **Told to Slack and the webhook.** A source nothing has been copied in
  from for `--source-quiet` (default 26h; the chart's
  `serve.firings.quietAfter`) is told of once, as `source.quiet`, to the
  webhook and, with `--notify-slack`, to Slack. The weekly digest says which
  sources firings were copied in from, how many each brought and which has
  gone quiet, and what the notifications sent came to (how many, repeats,
  while people slept, for nothing, the rules that sent the most for
  nothing); a team's digest, for its rules. The webhook's `digest.ready`
  carries both as `sources` and `notifications`.
- `recipes/firings/put.sh` sends a recipe's output as a source's firings for
  `SINCE` to `UNTIL`. The runs page lists the sources, `/api/v1/sources`
  has them, and `/metrics` has `lapilli_serve_source_last_timestamp_seconds`
  and `lapilli_serve_source_firings` by source. The chart takes
  `serve.firings.tokenSecret`, with `rules` and `prometheus` optional then.
- **Notifications as they were sent.** `lapilli serve --notify-listen ADDR`
  opens a port that takes notifications, and nothing else, a path for each
  source: `/v1/alertmanager` (a webhook, with the token in
  `--notify-token-file` as its bearer token), `/v1/grafana` (a webhook
  contact point, with the token or its HMAC signature with the secret in
  `--notify-grafana-secret-file`), `/v1/sns` (Amazon SNS's signature, from a
  topic `--notify-sns` names, for Amazon Managed Service for Prometheus; the
  subscription is confirmed at an SNS host only), `/v1/azure` (an action
  group's webhook, common alert schema, the token as the URL's password) and
  `/v1/google` (a Cloud Monitoring webhook channel, basic auth after a 401).
  Each alert is tied to its rule's firing whose labels are all among its own,
  started within two evaluations; a notification the same as one from
  another replica within a minute is kept once; repeats are as the source
  says, or the same alerts again with nothing new and nothing resolved. Rule
  pages and the rules list show notifications, repeats, those while people
  slept and those for nothing, by receiver; firings, how many told of each,
  or that one paged no one; the dashboard's "Woken for nothing" counts
  notifications once there are any. `lapilli_rule_notifications{rule,
  receiver, reason}` in `/metrics`, and in MCP's `rules` and `rule`. Schema
  009. The chart's `serve.notificationsAPI`.

## [0.19.0] - 2026-11-09

A cloud's own alarms: replayed where they are written in PromQL, on every
cloud that has such alarms, and judged from their history where they aren't.

- **CloudWatch PromQL alarms.** `--rules` reads the AWS CLI's
  `describe-alarms` answer: a PromQL alarm is replayed with its pending
  period as `for`, its recovery period as `keep_firing_for` and its
  evaluation interval; metric and composite alarms are listed as not
  replayed. `--prometheus https://monitoring.REGION.amazonaws.com --auth aws`
  queries CloudWatch's PromQL API, signed for the service `monitoring`, as
  is its dual-stack (`REGION.api.aws`) or VPC endpoint name; `aps` stays for
  Amazon Managed Service for Prometheus. A period given as NaN is refused.
- **Google Cloud alerting policies with a PromQL condition.** `--rules`
  reads them as `gcloud monitoring policies list` writes them, in YAML or
  JSON, or as the API's list answers: the condition's `duration` is `for`,
  its evaluation interval (30 s when it doesn't say) the rule's, its
  `alertRule` and `ruleGroup` the name and group (the policy's display name
  otherwise), its labels the rule's, and the policy's severity becomes
  `severity`. An alert closes at the first evaluation at least the larger of
  270 s and two evaluations after its condition last held, as Google Cloud
  closes them. A disabled policy, and one with another kind of condition, is
  listed as not replayed. Replay with `--gcp-project`, whose PromQL API
  answers for the same metrics, and `--step 30s`, of which every policy's
  interval is a multiple.
- **An answer cut short is an error.** An API that caps the series in an
  answer and says so only in a warning (CloudWatch: 500) no longer gives a
  replay of what was left: the rule's query fails, saying why.
- **Firings from a cloud's alarm history.** `recipes/firings` has a script
  for each cloud that writes its alarms' history as `--firings` takes it:
  `cloudwatch.sh` (`describe-alarm-history`), `azure-monitor.sh` (Azure
  Resource Graph), `google-cloud.sh` (`projects.alerts.list`) and `oci.sh`
  (`get-alarm-history`), each with the cloud's own CLI, tested against the
  clouds' documented answers with stand-ins for the CLIs. Times are checked
  to be times before they reach a query or a filter (UTC for Azure), the
  token goes only to https, a CloudWatch move that says neither its old nor
  its new state stops the script, and a stateless Azure alert (activity log,
  service and resource health) ends as it fires.

## [0.18.0] - 2026-11-09

For an incident nothing caught, the rules that nearly caught it by one of
their several numbers; and hours that take turns, for teams on call in
shifts.

- **Nearly caught, by one of several numbers.** A Grafana range threshold
  (`within_range`, `outside_range` and their `_included` forms) has each end
  looked at, the other as it is; PromQL joining comparisons with `and`, `or`
  or `unless` has each comparison with a number looked at, the others as
  they are, four at most. The first comparison of an `and` or `unless` is
  read by its values, as a single comparison is; any other by replaying the
  rule with nearer round numbers, the furthest within a fifth first, then
  searching back towards the rule's own for the nearest that fires during
  the incident. A comparison on the right of `unless` kept the rule quiet,
  and its number moves the other way. An expression over 16 KiB, or a
  comparison nested more than 32 levels (brackets, and, or, unless), isn't
  read for numbers. The near-miss table's `CHANGE`
  names the comparison (`rate(requests[5m]) > 10 → 8`, `unless requests < 10
  → 9`); JSON's `value` gains `of` and `unless`, and MCP the same.
- **Shifts.** For a team on call in shifts, or by turns across time zones,
  `--shifts` (the chart's `hours.shifts`), or a team's `shifts` in `lapilli
  serve` (the teams page and API; schema 007), gives hours that take turns:
  how long each turn is, in hours, days or weeks, when the first starts, and
  each turn's working hours, hours asleep and zone, those it leaves out as
  they are otherwise, `-` for a turn on them all (`every=1w
  from=2026-11-02T09:00; tz=Asia/Seoul; tz=Europe/Berlin sleep=23:00-07:00`).
  The first turn starts from 1970 to 2200. A firing is counted asleep or off
  hours by the turn it came in. A team's own hours or shifts replace the
  server's shifts. Reports name the shifts (`hours.shifts` in JSON).
- Working hours may run past midnight, as a night shift's do:
  `mon-fri@22:00-06:00` works the mornings after. They were refused.
- A near miss by value whose value wasn't read leaves out `reached`, in JSON
  and MCP; the table shows `–`.
- A firing a near miss's window made up at its start, the condition holding
  from before it, and what kept on from it through a dip by
  `keep_firing_for`, isn't taken for one that caught the incident: when it
  really started isn't known. Windows start further before the incident,
  and a near miss by `for` is checked by replaying the rule with the shorter
  `for`, as `keep_firing_for` keeps a firing on. Both were so in 0.17 too,
  with a group interval longer than the step, `keep_firing_for`, or a lead
  over a day.
- In `lapilli serve`, hours that change more than six times a day are counted
  with one read of the firings, row by row, rather than a query for each
  stretch.

## [0.17.0] - 2026-11-09

For an incident nothing caught, the rules whose value nearly reached their
threshold; near misses for incidents written down after the digest; and
working and sleeping hours for each team.

- **Nearly caught, by value.** For an incident nothing caught, a rule whose
  condition compares a value with a number (`errors > 0.9`, `0.9 < errors`, a
  Grafana threshold or a math expression with one number) nearly caught it
  when its value came within a fifth of the number for the rule's `for`. The
  report gives the round number short of what the value reached that would
  have caught it (`> 0.9 → 0.87`), when the rule would then have fired, and
  what it would cost over the period, as near misses by `for` do. A rule with
  no `for` is looked at this way too. In `lapilli verdict`'s report and
  `--json` (`value`), and in serve on the incident's page, in the digest and
  through MCP (`op`, `threshold`, `reached`, `new_threshold`).
- **Near misses for incidents written down later.** `lapilli serve` works out
  the near misses of a week again when its incidents change after the digest,
  or as they are written down in the week under way, within the hour, rather
  than at the next digest; and once more when the ledger's firings reach the
  end of an incident written down before it was over. Only the incidents that
  changed are looked at again; when the history fails, what was kept stays,
  and they are tried again later.
- **Hours for each team.** In `lapilli serve` a team can have its own working
  hours, hours asleep and time zone (the teams page, or `work_hours`,
  `sleep_hours` and `tz` in the teams API; each empty one is the server's).
  Its rules' firings are counted by them on the rules pages, the overview's
  tile, `/metrics`, `/api/v1/report`, MCP and its digest; a report says which
  teams went by their own hours (`hours.teams` in JSON). Schema 006 adds the
  columns.
- The near-miss table's `FOR` column is now `CHANGE`: `for 30m → 10m`, or
  `> 0.9 → 0.87`.
- The teams page's "Rules matching" column showed how many rules a team owns;
  it shows the pattern.
- A rule's `for`, `keep_firing_for` or group `interval` below nothing, or past
  a hundred years, is refused when the rules are read; from a rules API it is
  taken as nothing, or a hundred years.

## [0.16.0] - 2026-11-06

When the firings that caught nothing came, those that woke people first; and,
for an incident nothing caught, the rules that nearly did.

- **When the firings came.** The firings that caught nothing are counted by when
  they started: while people slept, off hours, or at work, in `--tz`.
  `--work-hours` (default `mon-fri@08:00-18:00`) and `--sleep-hours` (default
  `22:00-08:00`) say when those are, or `none` (in the chart, `hours.work` and
  `hours.sleep`). The report lists the rules that woke people for nothing first;
  `--json` has each rule's `asleep`, `asleep_black`, `off_hours` and
  `off_hours_black`; `--metrics` has `lapilli_rule_nothing_by_hours`. In
  `lapilli serve`, the dashboard has a tile for the firings that woke people for
  nothing, the rules pages and MCP have each rule's, and so does `/metrics`.
- **What nearly caught it.** For an incident nothing caught, or only noisy rules
  did, the rules whose condition held about then but not for their `for` are
  named, with the longest shorter `for` that would have caught it, when the rule
  would then have fired, and what that would have cost over the period:
  `lapilli verdict`'s report and `--json` (`near_misses`), and, worked out with
  `lapilli serve`'s weekly digest, the digest, the incident's page and MCP. A
  rule that held long enough, but fired only after the incident was over, is
  one too. The cost is of the rules as they stood, with quiet windows excusing
  what they excuse. It is bounded: twenty incidents, five rules each, ten
  replays tried, 2,000 questions, within the period; in `serve` the suggestions
  and these share one `--timeout`.
- A noisy rule's suggested longer `for` now counts, in `lapilli serve`, the
  firings a quiet window would excuse as the ledger does.
- `/api/v1/report?firings=false` over 31 days or less reads the firings again to
  count them by the hours, so it now takes its turn, one at a time, as a report
  with every firing does. Over longer periods it counts no hours.

## [0.15.0] - 2026-11-06

What each rule adds to the others, a check that the replay is right, incidents
copied in from where they are kept, and digests for each team and for a
webhook. Read "Before you upgrade": an incidents file with nothing in it is now
refused.

- **What each rule adds.** Each rule now gets the incidents it caught, those it
  caught before any other rule, and those no other rule caught, in the report,
  `--json`, `--metrics` (`lapilli_rule_incidents`,
  `lapilli_rule_incidents_first`, `lapilli_rule_incidents_alone`), the rules
  pages and MCP. A noisy rule's catch doesn't count beside one by a rule that
  isn't noisy. A rule is called spare when, for each of two incidents or more,
  another rule that can still fire caught it as soon, at the same severity or
  above; of rules tied, the first by name stays, so all the spare rules could go
  at once. The dashboard asks whether a spare rule still earns its place, after
  the incidents nothing caught, and offers a task to retire it. With `--rule` or
  `--changed-only`, only how many incidents each rule caught is given.
- **Is the replay right?** `lapilli verdict --check-replay` sets a replay beside
  what really fired, rule by rule, from `ALERTS`, `GRAFANA_ALERTS`, or the series
  `--check-series` names. It exits 1 when nothing was compared.
- **Incidents from where they are kept.** `recipes/incidents` copies them from
  PagerDuty, incident.io, Jira and GitHub issues. `lapilli serve
  --incidents-listen` and `--incidents-token-file` open a port that takes
  incidents from whoever holds the token, and does nothing else (in the chart,
  `serve.incidentsAPI`). What it copies in is the tool's (origin `copy`): the UI
  can't change it, and the token can't change what people wrote. It takes one
  write at a time, 20 a second, and keeps 20,000 copied incidents at most; they
  aren't told to the notice webhook or Slack, since the tool they come from has
  told people already.
- **An incident written again as it was changes nothing:** no change noted, no
  notice, no judging again.
- **The weekly digest goes further.** `--team-digest TEAM=FILE` posts a team its
  own rules and incidents (in the chart, `serve.teamDigests`), and
  `--notify-webhook-file` gets `digest.ready`, for everyone and for each team,
  with the numbers and the report as `text`. Webhooks that take Slack's format
  work; any other chat can take the JSON. Over twelve weeks, the digest names the
  rules that are spare. Its lines on tasks and outcomes now come right after the
  report's first lines, so a long report cut short for Slack keeps them.
  `digest.ready` has the report as written, `text`, and escaped as Slack takes
  it, `slack_text`.
- **A rule with `keep_firing_for`** is replayed from that much earlier too, so an
  alert held on across the start of a period is seen.
- The report's "never fired" no longer counts a rule that caught an incident
  with a firing from just before the period, or whose firings a quiet window
  excused.
- An incident's labels are 4 KB at most in all, as kept, and kept as written
  rather than with `<`, `>` and `&` escaped; its times are from 1970 to 9999. A
  change to an incident's title alone judges nothing again.
- An id in the incidents file that a person's incident holds is passed over and
  reported, on the status until the file reads cleanly and in
  `lapilli_serve_incidents_passed_over`; before, it kept the whole file from
  being read. One that a copy holds, the file takes.
- The README says plainly that Lapilli counts firings, not the notifications
  they become.

### Before you upgrade

- **An incidents file with no `incidents` key is refused**, by `lapilli verdict`
  and by `lapilli serve`, where the ledger then keeps the incidents it had. Before,
  such a file, an empty one among them, read as no incidents; and since a file
  owns what it holds, `serve` took every incident from it out of the ledger. A
  job that failed while writing the file leaves just that. For a period with no
  incidents, write `incidents: []`.

## [0.14.0] - 2026-11-05

A security release, after an audit from an attacker's point of view. Read
"Before you upgrade".

- **DNS rebinding (high).** A web page could point a name of its own at a
  local `lapilli serve`, or at a `kubectl port-forward`. The browser then
  took the server for the page's own site, past the cross-origin check. The
  page could read everything, change the settings, and write or delete
  incidents. `serve` now answers only these host names:
  - an address and `localhost`;
  - a name without dots, or under `.localhost`, `.internal`, `.svc` or
    `.cluster.local`;
  - `--external-url`'s name;
  - those in `--allowed-hosts` (in the chart, `serve.allowedHosts`).

  Others get 421; the health checks answer whatever the name. Over
  loopback (this machine, a port-forward, a sidecar), names without dots
  or under `.internal`, `.svc` or `.cluster.local` aren't answered either:
  someone with a hand in the company's or the cluster's DNS could point
  them here.
- **Forged headers inside a cluster (medium).** Any pod could reach `serve`
  and set the user or groups header, and so become an admin.
  - The chart now refuses `serve.userHeader` without
    `serve.networkPolicy`, unless `serve.networkPolicy.othersKeptOut` says
    something else keeps the rest out.
  - `/metrics` and the health checks are served alone on a second port:
    `--metrics-listen`, in the chart `serve.metricsPort` (9091). The probes
    and the ServiceMonitor use it, and `serve.networkPolicy.metricsFrom`
    lets Prometheus in there and nowhere else.
- **Running out of memory (medium).** A report with every firing of a busy
  month could take gigabytes, and several at once the pod. A report now
  gives 100,000 firings at most, excused or not, as do the settings tried
  over a week. Both take one turn at a time, held until the answer is
  written, with 30 seconds to write it, so a client that doesn't read can't
  hold answers in memory. The next waits 30 seconds, then gets 503.
- **A crash from a Grafana rule (medium).** A math expression naming a
  function without its parenthesis (`log`) dereferenced nil while the rules
  were read. A Grafana alert rule could crash `serve` reading Grafana, or
  `verdict --grafana`. Fuzzing found it.
- **Slack messages forged (low).** A task's assignee and a comment's key
  went into Slack lines unquoted, so newlines in them made a message of an
  editor's own under lapilli's name. Both are quoted now. An assignee is
  one line, and a comment must be on a rule, an incident or a task that is
  there.
- **A person taking a group's role (low).** In `--editors` and `--admins`,
  `user:NAME` is now a person alone and `group:NAME` a group alone. A name
  with neither is either, as before.
- **Hardening:**
  - Task and runbook links can't carry a user name.
  - The language cookie is Secure over HTTPS.
  - The MCP instructions name every field that people or the measured
    systems wrote, as data.
- **Fuzzing.** Every parser of outside input is fuzzed: rule files and
  APIs, scoping PromQL, Grafana expressions, and incident and firing files.
  CI runs each for two minutes every week.

### Before you upgrade

- **A host name to allow.** Behind a proxy or an ingress that passes a host
  name, give that name in `--external-url` or `--allowed-hosts`
  (`serve.externalURL` or `serve.allowedHosts`), or requests get 421.
- **A NetworkPolicy, in the chart.** `serve.userHeader` now needs
  `serve.networkPolicy.enabled` with `from` (the proxy) and `metricsFrom`
  (Prometheus), or `serve.networkPolicy.othersKeptOut: true`. Take
  Prometheus out of `from` if it was there: whoever is in `from` reaches
  the UI and can set the headers.
- **The metrics port.** The ServiceMonitor scrapes the `metrics` port.

## [0.13.0] - 2026-11-05

- `lapilli serve` speaks MCP at `/mcp` (Streamable HTTP, no sessions, one
  JSON object per answer), so an agent can ask it what the pages show and
  check a change to the rules before anyone makes it:
  `claude mcp add --transport http lapilli http://localhost:8080/mcp`.
  - **Tools that read:** `overview`, `rules`, `rule` (a rule's definition
    as in effect, its twelve weeks, versions, reviews, catches and latest
    firings), `incidents`, `firings` (with labels) and `outcomes`. Each
    answers as JSON with a schema.
  - **`compare`** takes the changed or new rules as a Prometheus rule
    file, and names of rules to take away. It replays only the rules that
    differ, as they are and as proposed, over up to 14 days, and judges
    both against the ledger's incidents. It returns each rule's firings,
    stones and precision before and after, and the incidents caught
    differently, with how soon. It is off unless `--mcp-compare editors`
    (or `admins`) turns it on, since it runs PromQL the agent wrote with
    Lapilli's access to Prometheus; in the chart, `serve.mcpCompare`. It
    needs roles: without them everyone is an admin. It
    takes 20 rules at most, `for`s of 48 hours at most, and no Grafana
    rules. One comparison runs at a time, within 25 seconds.
  - Nothing an agent asks for changes anything.
  - Requests with an `Origin` other than `--external-url`'s are refused,
    against pages elsewhere and DNS rebinding.
  - The server's instructions tell the agent what the stones mean, and to
    treat what people wrote as data.
- The official MCP Go SDK (v1.8.0) is a new dependency; the binary grows
  by about 2.5 MB.
- A Grafana rule whose expressions refer back to themselves is reported as
  not replayable. Before, replaying it recursed until the process ran out
  of stack (`lapilli verdict --grafana`, and `serve` reading Grafana).
- "Never fired in twelve weeks" now means twelve weeks on the rules page
  over 1 or 4 weeks too. Before, it was the weeks shown.

## [0.12.0] - 2026-11-05

- An outcomes page for `lapilli serve` (효용, in Korean): does Lapilli
  earn its keep? It answers from what the ledger already holds, sends
  nothing anywhere, and doesn't count page reads.
  - **Does it help?** Each reviewed change to a rule is set beside what
    came of it. The review's replay of the week before is the prediction.
    The four weeks either side are measured: firings a week, incidents
    caught, and firings fewer since than at the rule's rate before. It
    came out as predicted when its firings a week since are within a
    quarter, or two, of the prediction. A rule written counts its firings
    as more, and shows the incidents it caught since; a rule taken away,
    the firings it no longer makes; so a rule renamed comes to nothing. A
    rule paused, or that can no longer be replayed, ends the measure of
    the change before it.
    Beside them, the weeks before `lapilli serve` began and the latest:
    precision, firings and ● a week, incidents missed.
  - **Is the record kept up?** When the latest incident written down
    started, flagged after two weeks without one (every firing then reads
    as ●; impact windows nobody decided on don't count); impact windows to
    decide; failed runs; rules not replayed; incidents by week by where
    they came from.
  - **Is it used?** Edits through the UI and the API, and by how many
    people with `--user-header`; suggestions acted on (a task made, or the
    rule changed); tasks done and how long they took.
- The overview has an outcomes card, and the weekly digest a line of them.
- `/api/v1/outcomes` gives them as JSON. `/metrics` adds
  `lapilli_serve_firings_avoided`, `lapilli_serve_nothing_avoided`,
  `lapilli_serve_rule_changes{outcome}`,
  `lapilli_serve_incident_last_timestamp_seconds` and
  `lapilli_serve_edits`: an alert on the last can say when incidents stop
  reaching Lapilli.
- The ledger keeps when `lapilli serve` first ran on it, and indexes each
  rule's days (schema 5). A ledger opened by this release can't be opened
  by an older one.
- Charts of small counts are ticked one by one, where before 1 incident
  could be ticked 0, 0.2, 0.5, 0.8.

## [0.11.1] - 2026-11-04

- `lapilli serve` is set in Heebo, the typeface of Argo CD's UI, served from
  the binary (its Latin, 45 KB, under the SIL Open Font License 1.1, whose
  text is served beside it). Korean uses the system's font, as before. The
  serif headings are gone; code and labels use the system's monospace. The
  CSP allows fonts from the server itself and nowhere else.
- After an upgrade, browsers fetch the new stylesheet at once: the pages ask
  for `/static/...?v=` a hash of the files, kept for good, where before the
  old one could be used for up to a day.
- A page kept by a browser is no longer answered "not modified" by another
  `serve` process on the same ledger: the ledger's generation starts again
  with each process, so after a restart or an upgrade an old page could be
  shown; every ETag now carries the process too.

## [0.11.0] - 2026-11-04

- A calmer look for `lapilli serve`.
  - **Colours:** cream surfaces like river pebbles, and a muted olive for buttons, links and the current page. Caught firings are a quiet blue, firings that caught nothing a warm grey, and the heatmap of where the noise is runs from light to deep lavender.
  - **Headings:** page headings and the dashboard's numbers are set in a serif.
  - **Layout:** the header is a pill that stays at the top; cards and tiles have soft edges; task statuses are tinted pills.
  - **Dark mode:** in the same family.
  - **Checks:** chart colours were checked again for colour blindness and contrast in both modes, and all text is 4.5:1 or more. Incident statuses keep their fixed colours, each with its icon.
  - On a phone, the header stays in place and the tiles sit two to a row.

## [0.10.0] - 2026-11-03

- Quiet windows repeat every day or every week, until a day or for good:
  a nightly batch, a weekly release. Each repeat keeps its local time
  across a change to or from daylight saving.
- Notices of what happens: a task made, assigned, its status changed, or
  overdue; an incident written down; a quiet window kept; a comment. To a
  webhook, as JSON with a link to the page (`--notify-webhook-file`,
  `--external-url`), and to Slack, as a line in `--lang` (`--notify-slack`).
  They go through an outbox written with the change, a sender for each
  place, and are tried again, further apart, for about eight hours; each
  carries an `id`, the same each time it is sent. `/metrics` counts them,
  and how many are unsent.
- Roles: with `--editors` and `--admins` (people, or groups the proxy lists
  in `--groups-header`, or `*`), only editors write, only admins change the
  settings and the teams, and everyone else may only look. Pages offer only
  what the reader may do, and say who they are; `GET /api/v1/me` too.
- A task keeps a link to its issue in a tracker. `PATCH /api/v1/tasks/{id}`
  changes only the fields given, as the task is when written, so a tracker
  can put the link back without undoing a change made meanwhile; marking a
  task's status in the UI does the same.
- The weekly Slack digest says how the tasks stand: open, overdue, done in
  the week.
- The chart takes `serve.roles`, `serve.notify` and `serve.externalURL`,
  and can add a NetworkPolicy that lets in only the sign-in proxy and
  Prometheus (`serve.networkPolicy`).

## [0.9.0] - 2026-11-03

What people keep, beside what the runs find, made and changed in the UI
and the API, in the same ledger:

- Teams own rules by a name pattern or labels, and incidents by labels;
  the dashboard, the rules and the tasks can be shown for one team.
- A rule keeps a note: its team, whether to keep, tune or retire it, its
  runbook, a note.
- Quiet windows (a deploy, maintenance, a load test) excuse the firings in
  them that caught nothing: counted apart, neither white nor black, so
  expected noise doesn't weigh on precision. A firing that caught an
  incident still counts. Reports give each rule's `excused`.
- Tasks follow up on a rule or an incident, with a kind, team, assignee,
  due date and status; a suggestion on the dashboard becomes one with a
  press, and shows when one is open.
- Comments on rules, incidents and tasks; only their author changes them.
- Every write is a same-site POST or an API call, refused with
  `--read-only`, and noted with who made it.

## [0.8.0] - 2026-11-03

- `lapilli serve`'s UI speaks Korean as well as English: the browser's
  language, or the one picked at the top of the page. Each language is one
  file of translations, and a test renders every page in every language
  and fails on a text without one.
- Larger text throughout the UI, and in its charts; on a narrow screen,
  or in a narrow card, the charts' text is larger still and every other
  week's label is left out.

## [0.7.0] - 2026-11-03

- `lapilli serve` opens on a dashboard: last week's precision, firings,
  incidents missed, time to catch and noisy rules beside the week before;
  firings, precision and incidents by week; where the noise is, rule by
  week; and what to look at first. It reads the weekly tallies, so it takes
  about 10 ms over 200,000 firings.

## [0.6.0] - 2026-11-03

`lapilli serve`: keep judging, with a ledger, a web UI, an API and metrics.

- `lapilli serve` replays the rules every hour over the hours since its last
  run, and the hour before again for points that came late, and records the
  firings in a ledger (one SQLite file, through the pure-Go
  `modernc.org/sqlite`). It reads the rules every minute and records each
  change as a version, so each hour is replayed with the rules as they stood;
  a change is replayed over the week before it, as it was and as it is.
  Once a week it judges the week before, suggests longer `for`s, and posts
  to Slack. On first start it replays `--backfill` (28 days); it keeps
  `--keep` (400 days).
- The ledger keeps each firing's stones, given against every incident it
  has, and each rule's by day and week: twelve weeks read as fast as one,
  and a change gives stones again only near it. Over random weeks its
  verdict is `verdict.Judge`'s. With two million firings an hourly run
  takes 0.18 s, writing an incident 52 ms, reading twelve weeks 2 ms.
- A rule a run couldn't replay, a query failing or time running out, is
  replayed by later runs from where it was missed; a run that runs out of
  time is tried over half of what is left.
- A web UI, rendered on the server with no scripts: the stones by week, each
  rule's with its versions and change reviews, incidents (written down
  there, impact windows confirmed or dismissed, a black stone said to be
  real), the firings by day, the settings that only judge the ledger again
  (tried on the last week before saving), and the runs. An API gives
  `lapilli verdict --json`'s report for any period and takes incidents;
  `/metrics` gives the weekly metrics without a Pushgateway.
- It listens on localhost, has no sign-in of its own, records changes under
  the header a sign-in proxy sets (`--user-header`), refuses posts from
  other sites, and with `--read-only` takes no changes.
- The chart's `mode: serve` runs it with its ledger on a volume; a Grafana
  dashboard of the metrics ships with the chart.

## [0.5.1] - 2026-11-03

- Release binaries keep their symbol table, so `govulncheck -mode=binary`
  can tell which vulnerable functions one holds. Without it, v0.5.0's
  binaries are reported as using `golang.org/x/crypto/openpgp`
  (GO-2026-5932), which lapilli doesn't import; its code is not affected.
- A release is published only if govulncheck finds nothing in the code or
  in any of its binaries, and the latest release is checked again every
  day. SECURITY.md says how fixes ship and how to check the binary you have.
- Dependabot follows the Go modules and the image's base too, two weeks
  after a release, and security updates at once.
- The chart has a README, an icon, and the annotations Artifact Hub reads,
  among them the image it scans for each chart version.

## [0.5.0] - 2026-11-03

What to change, replays closer to the real thing, and rules read where they
live.

- What to change: each noisy rule is replayed again with a longer `for`, and
  the shortest that still catches every incident it caught while firing at
  most half as often is suggested, with how much later those incidents
  would be caught. Rules show their `severity` label in reports,
  comparisons (firings counted by severity before → after) and `--metrics`.
  `--noisy-firings` and `--noisy-precision` set when a rule counts as noisy.
- Rules that share a name in one set, in different groups or with
  different labels, are told apart (`HighErrorRate [severity=critical]`),
  alike in every set compared or replayed.
- `--compare --changed-only` replays only the rules that differ. A directory
  with no rules in it is an empty set when comparing.
- `keep_firing_for` holds a firing alert past its condition, a group's
  `interval` is when it is evaluated (when a whole number of `--step`s), and
  Grafana's classic conditions are replayed, written as one PromQL
  expression the way Grafana evaluates them.
- `--loaded-rules` replays the alert rules the API at `--prometheus` has
  loaded (Prometheus, Thanos, Mimir, vmalert, Amazon Managed Service for
  Prometheus), and `--grafana URL` those a Grafana holds, through its
  provisioning export API. The chart takes `rules.loaded` and
  `rules.grafana`.
- `--rules` reads each cloud's managed rule groups: an AWS rule groups
  namespace, Azure's prometheusRuleGroups (scoped to their `clusterName`),
  and Google Cloud's Rules (scoped to their namespace), ClusterRules and
  GlobalRules; `kubectl get -o yaml` lists; and the directories below a
  directory, or behind a link to one.
- The README has `--impact` expressions for Istio, Linkerd, Envoy,
  ingress-nginx, Traefik and OpenTelemetry HTTP server metrics.

## [0.4.0] - 2026-11-03

- `--compare FILE`: judge a change to the rules before it is merged. The
  rules in place and those proposed are replayed over the same period and
  judged against the same incidents; the report shows each rule that
  differs, its stones before → after, and each incident caught differently.
  Rules alike in both are replayed once. The README has a pull request
  check to copy.

- Columns line up when rule or incident names are in Chinese, Japanese or
  Korean, whose characters take two cells in a terminal.

- Oracle Cloud: it has no managed Prometheus, so on OKE lapilli reads the
  Prometheus you run there, and `--auth oci` says so. The README says which
  of a cloud's own alarms aren't read yet: CloudWatch, Azure Monitor, Google
  Cloud alerting and OCI Monitoring alike.

## [0.3.0] - 2026-11-02

- The managed Prometheus of every major cloud: `--auth aws` signs requests to
  Amazon Managed Service for Prometheus with SigV4, `--auth azure` sends a
  token for Azure Monitor managed service for Prometheus, and `--auth google`
  one for Google Cloud Managed Service for Prometheus, each from the
  credentials the machine or pod already has. `--gcp-project` stays, short
  for Google Cloud's URL and `--auth google`. The chart takes
  `prometheus.auth`, `prometheus.awsRegion` and `podLabels`, and documents
  the workload identity of EKS, AKS and GKE alike.

## [0.2.0] - 2026-11-02

Rules as they stood at each time, incidents by service, replays three times
faster, and a weekly run.

- `--rules FILE@TIME` replays the rules as they stood at each time, for a
  period in which they changed. Where a version takes over, alerts carry over
  as Prometheus and Grafana carry them over on a reload.
- A Grafana reduce over a window sees the data from before the replay starts,
  as Grafana's query would, instead of a window cut short at the start.
- A rule is listed as not replayed if any rule of that name couldn't be.
- An incident's label can list several values (`service: [a, b]`), any of
  which agrees for `--match`.
- `--impact` looks at each series on its own, and its incidents carry the
  series' labels: by service, an incident says which services were hit, so
  `--match` works for them too. An impact window must now last
  `--impact-for` in one series. In `--json`, incident labels are lists.
- Replays are faster and take less memory. Rules that need the same query
  over the same day share one request; a day's queries are asked for
  together; Grafana math pairs series by key rather than each with each; and
  `--parallel` (8) rules run at once, the busiest first. A replay is fetched
  and evaluated a day at a time, so a long period needs no more memory than a
  day.
- A Helm chart runs `lapilli verdict` every week as a CronJob
  (`oci://ghcr.io/lapilli-project/charts/lapilli`), and an image holds the
  release's Linux binaries (`ghcr.io/lapilli-project/lapilli`); both are
  attested like the archives.
- `--push URL` puts the metrics on a Pushgateway (job `lapilli`, or the group
  a URL ending `/metrics/job/...` names), and `--slack-webhook-file FILE`
  posts the report to a Slack incoming webhook whose URL is in the file; the
  URL is never shown, errors included, and redirects aren't followed.
- An incidents file with the same id twice is refused.
- `--rules DIR` reads every file with alert rules in a directory, such as a
  mounted ConfigMap.

## [0.1.0] - 2026-11-02

The first release of `lapilli verdict`, which judges alert rules against what
actually went wrong.

- Firings from replaying alert rules against a Prometheus-compatible API
  (`--rules`), from the `ALERTS` series (`--alerts`), or from a JSONL file
  (`--firings`).
- Rules from Prometheus rule files, `PrometheusRule` resources, Grafana
  alerting provisioning files, and ConfigMaps holding them. Grafana rules are
  evaluated the way Grafana does: reduce, threshold and math, with
  comparisons giving 1 or 0 and sets of series joined by labels.
- Incidents from a YAML file and from windows where an impact expression
  holds (`--impact`).
- A white stone for each firing that caught an incident, a black one for each
  that caught nothing; refired alerts; incidents no rule caught, and how long
  each took to catch.
- Catches by noisy rules (10+ firings, under 20% catching anything) are marked;
  incidents caught only by them are reported apart.
- Text, JSON (`--json`) and Prometheus metrics (`--metrics`).
- Busy APIs are asked again; answers too large are asked for in halves; a
  replay that runs out of `--timeout` still reports what finished.
