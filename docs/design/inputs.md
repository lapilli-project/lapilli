# What comes in: deliveries, on-call schedules, deploys, a cloud alarm's history, Compass's closes, trackers' comments

Status: designed and built together, for v0.27.0. Under
[GOVERNANCE.md](../../GOVERNANCE.md) two of these are new kinds of data in
the ledger (the turns of on-call schedules, and comments copied in from a
tracker), and one is a new token's power (the tasks token writing
comments), so the design is written down first, here, and stays open for a
week as a pull request before the release. Where the building settled
something this note leaves open, it says so, marked **Decided in the
building**. The companion to [notifications.md](notifications.md),
[history.md](history.md), [fidelity.md](fidelity.md),
[recorded.md](recorded.md) and [trackers.md](trackers.md), each of which
this extends.

Six things Lapilli reads from elsewhere, each of them a ROADMAP item:

1. Every delivery Alertmanager made, from its event recorder.
2. Shifts read from on-call schedules exported as iCalendar feeds.
3. When a change to the rules was deployed, from Argo CD's or Flux's
   history, rather than when it was committed.
4. A cloud alarm's replay set beside its history, copied in by the recipes.
5. When a Compass alert was closed, from its log.
6. Two more trackers, Azure Boards and Shortcut, and comments kept in step
   with every tracker's.

Each takes input from outside, so each has an attacker's section, and each
refusal has a test.

## 1. Every delivery, from Alertmanager's event recorder

### Why

The webhook Lapilli takes today (`/v1/alertmanager`) is one receiver among
an Alertmanager's receivers. It sees what is routed to it, and a team's
pages go to PagerDuty, Slack or email, routed apart. So the notifications
Lapilli counts are those a route sent to it, which is right only where
Lapilli's receiver is routed beside every other (`continue: true` at the
top).

Alertmanager 0.33.0 added an event recorder (`--enable-feature=event-recorder`)
that writes what Alertmanager did: an alert created, grouped, resolved, a
silence made, an alert muted, and, after each delivery an integration made,
a notification. That is every page, as it was sent, by which integration.

### The format, as a real Alertmanager writes it

Read from the source (`eventrecorder/`, `proto/eventrecorder/events/v2/`)
and recorded from a real Alertmanager in Docker: 0.33.1, 0.34.2 and 0.35.0,
each with a PagerDuty, a Slack and a webhook integration pointed at a server
standing in for them, an alert silenced, a group gaining an alert, a repeat
and resolves (`internal/serve/testdata/events/`).

- A webhook output (`event_recorder.webhook_outputs`) POSTs each event as
  protojson, one a request, or, with `batch: true`, a JSON array of them,
  100 by default and about 1 MiB at most. `http_config` carries a bearer
  token as the receivers' does. It tries an event three times with a
  backoff, then drops it, so a receiver must take an event sent twice.
- An event is `{"@timestamp", "instance", "data": {KIND: {...}}}`, and
  `clusterPosition` in high availability. A notification is
  `data.notification`: `alerts` (each with `details`: `name`, `labels`,
  `startsAt`, `resolved`), `firingAlerts`, `resolvedAlerts`,
  `mutedAlerts`, `groupInfo` (`groupKey`, `receiverName`, `groupUuid`),
  `reason` (`NOTIFY_REASON_FIRST_NOTIFICATION`, `..._NEW_ALERTS_IN_GROUP`,
  `..._NEW_RESOLVED_ALERTS`, `..._ALL_ALERTS_RESOLVED`,
  `..._REPEAT_INTERVAL_ELAPSED`), `flushId`, and `integration` (`name`,
  and `index` when not 0, as protojson leaves zeros out).
- **Two formats.** 0.33 and 0.34 write labels as `{"labels": [{"key",
  "value"}]}` and an alert's reference as `hash`; 0.35.0 (the `events/v2`
  schema, a breaking change in its changelog) writes labels as a map and
  the reference as `fingerprint`, and requires each output to have a
  `name`. Both are read.

### The way in

`POST /v1/alertmanager/events` on the notifications port, with the
notifications token as a bearer token, as `/v1/alertmanager` takes it:

```yaml
event_recorder:
  webhook_outputs:
    - name: lapilli            # 0.35.0 on
      url: http://lapilli-notify:8083/v1/alertmanager/events
      batch: true
      http_config:
        authorization:
          credentials_file: /etc/alertmanager/lapilli-token
```

- **Each delivery is a notification**, sent to `RECEIVER/INTEGRATION`
  (`oncall/pagerduty`, `oncall/slack`; a second integration of one kind,
  `oncall/pagerduty[1]`), so the rules list's notifications by receiver say
  which integration paged.
- **The reason** is the recorder's, mapped as the webhook's
  `notification_reason` is; one it doesn't name is worked out as today.
- **When it came** is the event's `@timestamp`, when the recorder says it
  delivered, if that is within the hour before now and five minutes after;
  otherwise now.
- **Sent again** after a failure, or the same flush of a group delivered by
  the same integration, is one notification: the message id is a hash of
  the instance, the group's `groupUuid`, the `flushId` and the receiver.
  Another replica's delivery of the same is the same notification when it
  comes within a minute, as today.
- **Every other event** (alerts created and grouped, silences, mutes,
  start and shutdown) is answered 200 with nothing kept.
- **Lapilli's own webhook receiver**, kept beside the recorder, would count
  each notification twice: once as it reached Lapilli, once as the
  recorder saw it delivered. `?skip=NAME` (up to 20) passes over the
  receivers it names. Better, drop that receiver: the recorder sees
  everything it did.

### What it doesn't do

- **Mutes.** The recorder says when a silence or an inhibition kept an
  alert from a notification (`silenceMutedAlert`, `inhibitionMutedAlert`,
  a notification's `mutedAlerts`). That answers "firings that paged no one:
  why", which today Lapilli can't. It isn't kept here: it is a new kind of
  data, and wants a design of its own (a question for review).
- **The file and Kafka outputs.** A recipe could send a JSONL file's
  notifications on later; not built.

### Attacker's look

- Nothing is read before the token: a request without it is answered 401
  before its body is read, as on every token path of the port.
- A body is 1 MB at most and 1,000 events; a notification's alerts are
  1,000 at most, with the bounds every path has on names and labels.
- Events that carry no notification are read at a rate of their own (200 a
  second, bursts of 1,000) and don't use up the rate notifications are kept
  at (20 a second, bursts of 100): the recorder sends far more of them, and
  a burst of mutes shouldn't make it drop a delivery.
- What can't be read as the recorder's (no `@timestamp`, labels that aren't
  strings, an alert without `details`, an integration with `/` or `[` in
  its name, an index past 1,000) is refused, not kept as something else:
  that event alone, the rest of a batch kept (as built, after the reviews).
- Someone with the token can post deliveries that never were, within those
  bounds, as on `/v1/alertmanager` today.

## 2. Shifts read from on-call schedules

### Why

`--shifts` and a team's shifts say whose hours a firing came in as turns of
a fixed length (`every=1w from=...; tz=Asia/Seoul; tz=Europe/Berlin`). Real
rotas aren't that regular: swaps, overrides, holidays, a new person. Where
they are kept (PagerDuty, Opsgenie and Jira Service Management, Grafana
OnCall and IRM, a shared Google Calendar), each exports a schedule as an
iCalendar feed. A turn there names a person, not hours; this reads the feed
and gives each turn that person's hours.

### What the feeds look like

From each vendor's documentation and source, and real exports kept as test
data elsewhere (none of them documents its events' layout):

| Where | Feed | The secret | A turn |
|---|---|---|---|
| PagerDuty | `webcal://SUB.pagerduty.com/private/KEY/feed/SCHEDULE` (a schedule), `.../private/KEY/feed` (a user's) | in the path | `SUMMARY:On Call - NAME - SCHEDULE`, `ATTENDEE:email` (no `mailto:`), times in UTC; a month back |
| Opsgenie | `GET https://api.opsgenie.com/v2/schedules/ID.ics` | `Authorization: GenieKey KEY` | `SUMMARY:NAME (user) is on call for schedule: S.`, no attendee |
| Jira Service Management | `GET https://api.atlassian.com/jsm/ops/api/CLOUD/v1/schedules/ID.ics` | basic (email and token) | from the start of the month, a year ahead |
| Grafana OnCall | `BASE/api/v1/schedules/ID/export?token=T` | in the query | `SUMMARY:USERNAME`, no attendee; 15 days back, 180 ahead; a removed turn stays as `STATUS:CANCELLED` |
| Grafana Cloud IRM | as OnCall | in the query | `SUMMARY:ROTATION - email`, or `[L1] email` |
| Google Calendar | the calendar's secret address in iCal format | in the path | whatever the event's title says, attendees as `mailto:`, times with `TZID=`, repeats as `RRULE`, `EXDATE`, `RECURRENCE-ID` |

### What it proposes

`--on-call FILE`, for `serve` and `verdict`: the schedules to read, and the
people in them.

```yaml
every: 30m                    # serve: how often each feed is read
schedules:
  - name: payments            # shifts name it: on-call=payments
    url_file: /secrets/payments-ics   # the feed's URL, its secret in it
    who: '^On Call - (.+?) - '        # optional: the person, in SUMMARY
  - name: platform
    url: https://api.opsgenie.com/v2/schedules/platform.ics
    token_file: /secrets/opsgenie
    auth: genie               # bearer (default), genie or basic (with user)
  - name: storage
    file: /data/storage.ics   # read again each time
    tz: Asia/Seoul            # times written without a zone
people:
  - name: Jane Doe
    also: [jane@example.com, jdoe]
    tz: Asia/Seoul
    work: mon-fri@09:00-18:00
    sleep: 23:00-07:00
  - name: Ali Kaya
    tz: Europe/Istanbul       # work and sleep as the team's
```

- **Shifts name a schedule**: `--shifts on-call=payments`, or a team's
  shifts `on-call=payments`. A firing goes by the hours of the person whose
  turn it came in: their `tz`, `work` and `sleep`, each they leave out the
  team's (or, without a team's, the server's). A time no turn covers, or a
  turn of someone not in `people`, goes by the team's hours, and the runs
  page names the people the feeds have and the file doesn't.
- **Who a turn is.** An attendee's email that is a person's, if there is
  one; else what `who` takes out of `SUMMARY` (its first group); else the
  whole `SUMMARY`. A person is found by `name` or any of `also`, without
  case.
- **Turns that overlap** (layers, overrides, two calendar events): the one
  that started last holds from its start; when it ends, the one under it
  resumes.
- **Kept in the ledger.** Feeds look back a little (PagerDuty a month,
  Grafana OnCall 15 days, Jira Service Management to the start of the
  month), and Lapilli counts twelve weeks. So `serve` keeps the turns it
  read, in a table of its own (schema 015): each read replaces the
  schedule's turns that end after the earliest start the feed now holds,
  and keeps those before it. A feed with no turns, or one that fails,
  changes nothing. Turns keep the person, not the hours: a person's hours
  changed in the file count from then, for every turn they had.
- **Counted again when turns change.** Hours are told apart by a key that
  includes the schedule's turns and people, so the weeks counted by the
  old ones are counted again, as when a team's hours change today.
- **`verdict`** reads each feed once, at its start.

### Reading a feed

- **The calendar.** Lines unfolded (CRLF or LF, then a space or a tab),
  then read as UTF-8; `VEVENT`s with `DTSTART` and `DTEND` or `DURATION`,
  in UTC, with `TZID=` an IANA name, or floating (the schedule's `tz`, or
  UTC); `VALUE=DATE` days, from midnight to midnight in that zone;
  `STATUS:CANCELLED` passed over, as are turns of no length.
- **Repeats**, which Google Calendar writes and the on-call tools don't:
  `RRULE` with `FREQ=DAILY` or `WEEKLY`, `INTERVAL`, `COUNT` or `UNTIL`, and
  `BYDAY` for weekly; `EXDATE`; and `RECURRENCE-ID` replacing one
  occurrence. Others (monthly, yearly, `BYSETPOS`) are passed over, said
  on the runs page with the event's summary. Repeats are expanded only
  over the 400 days before now and the 60 after.
- **A zone that isn't one**, such as Outlook's `Korea Standard Time`, or a
  `VTIMEZONE` of the feed's own: the event is passed over and said.
  Lapilli doesn't read zone rules from a feed.

### Attacker's look

The feed's URL is the operator's, in a file; what comes back is the
schedule's, and anyone who can edit a schedule (or a shared calendar) can
write what it says.

- **Where it goes.** Only to the URL given: `https` (`webcal://` is read as
  `https://`), or a file. No redirect is followed (PagerDuty answers a bad
  key with a redirect, and Grafana's http-to-https redirect drops its
  path), and no URL in a feed is ever fetched. 30 seconds, 8 MB, one read
  at a time.
- **The secret.** The URL holds it, so it is read from a file each time,
  and never logged or shown: errors name the schedule and the host, not
  the URL. A token is given in a header from its file.
- **What it says.** A body that doesn't start with `BEGIN:VCALENDAR` (an
  HTML sign-in page, Grafana's JSON error with `text/calendar`) is
  refused. A line is 64 KB at most unfolded, a feed 20,000 events, and
  repeats expand to 50,000 turns at most; a schedule keeps 20,000 turns.
  Names are kept as text that shows, 200 characters at most, and are
  escaped where they are shown; nothing in a feed is run or followed.
- **`who`** is the operator's regular expression, in Go's syntax, which
  runs in time linear in its input.

## 3. When a change to the rules was deployed

### Why

`recipes/versions/git.sh` names each version of the rules by its commit.
A version is in effect when it reaches Prometheus, which for a team that
deploys with Argo CD or Flux is when that tool synced a revision holding
it: minutes after a merge, or days, for a team that syncs by hand.

### What it proposes

`DEPLOYS=FILE` for `git.sh`: a file of the revisions deployed and when,
`REVISION TIME` a line, the earliest first. A version (commit C) is then
named by the first deploy of a revision that holds C, `git merge-base
--is-ancestor C REVISION`, rather than by C's time. Two recipes write the
file, read only:

- **`recipes/versions/argocd.sh`**: an Argo CD Application's
  `status.history`, from Argo CD's API (`GET /api/v1/applications/NAME`,
  with a token that may only `get` applications), or from what `kubectl get
  application NAME -o json` writes. Each entry's `revision` and
  `deployedAt`; for an application of several sources, `revisions[N]`
  (`ARGOCD_SOURCE`, 0 by default).
- **`recipes/versions/flux.sh`**: a Flux Kustomization or HelmRelease, from
  `kubectl get kustomization NAME -o json`. A Kustomization's
  `status.history` (Flux 2.7 on): each snapshot's `metadata.revision` and
  `firstReconciled`; before 2.7, its `lastAppliedRevision` and the time its
  `Ready` condition last changed, one deploy. A HelmRelease's
  `status.history` names no git revision, only a chart version, which for
  a chart built from a git source ends `+SHA`: that is taken as the
  revision, and `lastDeployed` as when.

Argo CD keeps ten entries by default, Flux five snapshots: the recipes are
to run where versions are written, each time, and keep the file
(`DEPLOYS_KEEP`, appended, the same revision kept once, at its earliest
time).

- **A version older than every deploy known** keeps its commit's time, and
  `git.sh` says how many did.
- **A version no deploy holds yet** isn't in effect: it is passed over and
  said. The rules `--rules` reads now may be ahead of it; `serve` logs the
  difference, as it does a stale directory.
- **A revision the clone doesn't have** (a deploy of another branch, a
  shallow clone) is passed over and said.

### Attacker's look

- Argo CD's address is `https` only, its token read from a file and given
  to `curl` on stdin; 30 seconds and 4 MB at most; no redirect followed.
  The application, its namespace and project are checked to be Kubernetes
  names before they reach a URL.
- A revision reaches `git` only as 7 to 64 hex digits (Flux's
  `BRANCH@sha1:HEX`, `sha256:HEX` and a chart version's `+HEX` taken apart
  first), and a time only as RFC 3339 between 1970 and now: a revision
  can't be an option or a path, and a time can't be shell.
- Someone who can write the Application's status (Argo CD's controller,
  or an admin) can say a revision was deployed when it wasn't, which moves
  when a version took effect, as a wrong commit time could today.

## 4. A cloud alarm's replay beside its history

### Why

`--check-replay`, and `serve`'s check since v0.21, set a replay beside the
series that say what really fired (`ALERTS`). A cloud's own alarms keep no
such series, so the PromQL ones Lapilli replays (CloudWatch PromQL alarms,
Google Cloud policies with a PromQL condition, Azure's PromQL metric
alerts) are never checked. Their history is there, though: the recipes in
`recipes/firings` copy it in, each firing labelled with its alarm's ID
(`alarm_id`) since v0.24.

### What it proposes

The history copied in stands in for `ALERTS`, alarm by alarm, matched the
same way (`verdict.CheckSpan`: the same alert, labels that agree where both
have them, starts within two evaluations).

- **`serve`**: an alarm read with `--alarms SOURCE=URL` and replayed is
  checked against the firings copied in under `SOURCE` whose `alarm_id` is
  its ID (a Google policy's compared by `alertPolicies/ID`, as ties are),
  renamed to the name the replay goes by. A day is checked once the
  source's copies cover it, and the matching window either side; a source
  that is behind holds the day's check back up to two days, then the
  alarm isn't checked that day. A source never copied in leaves its
  alarms unchecked, as today.
- **`verdict --check-replay --check-firings FILE`**: the same, from a
  recipe's output, for rules read from a cloud's alarm list (`--alarms`,
  or `--rules` with a saved list): by `alarm_id` where both have it, by
  name where they don't (a Google policy's display name for its PromQL
  condition).
- **An alarm as a whole.** CloudWatch's history is of the alarm, not each
  series (in `ALARM` while any series is), and its recipe gives a PromQL
  alarm's firings no labels. So a CloudWatch alarm's replayed firings are
  first put together the same way: their spans joined, one firing while any
  is on. Google Cloud's and Azure's alerts carry their series' labels, and
  are compared series by series. OCI has no PromQL alarms.
- **Shown as today**: on the rule's page, the rules list, `/metrics`, MCP
  and `replay.differs`; the runs page says which sources stand in.

### Attacker's look

Nothing new comes in: the firings were copied in through the firings
token, with its bounds, and this only reads them. Someone with that token
could make a replayed alarm look as if it differs, as they could make its
recorded history say anything today.

## 5. When a Compass alert was closed

Compass's alerts don't say when they closed (no `closedAt`, and nothing
like Opsgenie's `report.closeTime`), so `compass.sh` ends a closed alert at
its last update. Its alert log does: `GET /v1/alerts/{id}/logs`
(experimental, basic auth), `{"values": [{"logTime", "logType", "log",
"owner"}], "links": {"next": "/v1/alerts/ID/logs?after=CURSOR"}}`, 100 at
most a page.

- For each closed alert that counts as an incident, the recipe reads its
  log and ends it at the last entry saying it was closed. Compass doesn't
  document that entry's words; Opsgenie's, whose alerts Compass's are, are
  `Alert closed by ...` and `Alert closed via ...`, so an entry whose text
  starts `Alert closed`, or whose type is `close`, is a close, in
  `compass.jq` for anyone to change. With none, the last update, as today.
- One request a page of each such alert's log: `MAX_LOGS` alerts at most a
  run (500), five pages each. Past that, the rest end at their last
  update, and the recipe says how many.
- **Attacker's look.** The next page is built from the configured address,
  the alert's id (checked: letters, digits and dashes) and the cursor
  (checked: digits and `_`), never followed as given. Log text is read
  only for its first words, and goes nowhere.

## 6. More trackers, and comments kept in step

### Azure Boards and Shortcut

Beside GitHub, GitLab, Jira and Linear, alike:

- **`azure-boards.sh`**: work items in one project (`AZURE_DEVOPS_ORG`,
  `AZURE_DEVOPS_PROJECT`, `AZURE_BOARDS_TYPE`, `Task` by default),
  REST 7.1, basic auth with a personal access token. Made with JSON Patch,
  tagged `lapilli` and the kind and team; found again by WIQL (`[System.Tags]
  CONTAINS 'lapilli-task-ID'`, created by the token's own identity); the
  task's marker is the tag `lapilli-task-ID`, as Jira's label is, since a
  work item's description is HTML. Its state is read by category: a
  `Removed` state closes the task as dropped, `Done` and `Closed` (and any
  `AZURE_BOARDS_DONE`) as done; `CLOSE_ISSUES` moves it to `AZURE_BOARDS_CLOSE`
  (`Done`; `Closed` for Agile and CMMI).
- **`shortcut.sh`**: stories in one workspace, with a workflow state to make
  them in (`SHORTCUT_STATE`, its id), REST v3, `Shortcut-Token` header.
  Labelled by name; its text ends with the marker line, as GitHub's does,
  and it is found again by search (`label:lapilli requester:MENTION
  description:"lapilli-task-ID"`) among the token's own member's stories; a
  story in a `done` state closes the task as done, one archived as
  dropped; `CLOSE_ISSUES` moves it to `SHORTCUT_DONE_STATE`, or its
  workflow's first done state.

### Comments, both ways

**Decided in the building**: both ways, and only when asked
(`COMMENTS=true`), since a task's discussion split between two places is
the thing to avoid, and a copy one way leaves half of it out of each.

- **Lapilli to the tracker.** Each comment people wrote on a task in
  Lapilli (not one copied in from the tracker) is posted to its issue once,
  quoted, as "Written in Lapilli:" (who wrote it only with
  `COMMENT_NAMES=true`: the sign-in proxy's name is often an email), and
  ends with the line `Kept in step with Lapilli: lapilli-comment-ID`.
  Lapilli keeps the tracker's id for each comment posted, so none is posted
  twice; one posted by a run that failed before Lapilli noted it is found
  again by its last line among the job's own comments. With more comments
  on the issue than a run reads, nothing is posted to it (as built, after
  the reviews).
- **The tracker to Lapilli.** Each comment on the issue that isn't the
  job's own account's, isn't the tracker's own (GitLab's system notes,
  Azure Boards' deleted), and that everyone who reads the issue may read
  (not Jira's restricted ones, nor GitLab's internal notes), is copied to
  the task as a comment from the tracker: who wrote it (as the tracker
  names them), when, and its text, with the tracker's id for it, so copying
  it again changes nothing. One Lapilli refuses is passed over, and fails
  nothing.
- **Once.** A comment edited or deleted after it was copied isn't followed
  either way: what was said, was said. (A question for review.)
- **Bounded.** 200 comments a task a run, each 10,000 characters at most,
  as a comment written in Lapilli is.

The tasks token may now also, and nothing more:

- `GET /api/v1/tasks/{id}/comments`: the task's comments, each with
  `origin` (`""` for people's, `tracker` for those copied in) and `ref`;
- `POST /api/v1/tasks/{id}/comments` with `text`, `author`, `ref` and `at`:
  a comment from the tracker. `ref` is the tracker's id for it (letters,
  digits and `._:-`, 100 at most), kept once a task; `author` a name that
  shows, 100 characters at most; `at` an RFC 3339 time, not after now. Up
  to 1,000 copied comments a task. It isn't told to the notice webhook or
  Slack (the tracker told people), and it is noted as `tasks-token`;
- `PATCH /api/v1/tasks/{id}/comments/{comment}` with `posted`: the
  tracker's id for the comment it posted for one people wrote, set once.

A comment copied in shows on the task's page as the tracker's: "NAME, in
the tracker", apart from people's. Nobody can edit or delete it in the UI,
since nobody in Lapilli wrote it.

### What the text can do in each place

- **GitLab runs quick actions** in text the API is given: a line starting
  `/` in an issue's description or a note (`/close`, `/assign`, `/label`)
  is carried out as the token's account, then removed. A task's text, and
  a comment posted from Lapilli, are written by editors or agents. A
  comment is posted quoted, every line of it after `> `, and every line
  starting `/` is sent as `\/`, which Markdown shows as `/` and GitLab
  doesn't run, as the issue's text has been since v0.22.
- **GitLab's links.** From 18.x a work item's URL is
  `PROJECT/-/work_items/N`, and `/-/issues/N` redirects to it; the API may
  give either. Both are taken as the issue's own, and the task keeps the
  one given.
- **Jira** takes a comment as Atlassian Document Format: text is sent as
  text nodes, never as markup, so nothing in it becomes a link or a macro
  but what Jira makes of plain text.
- **Mentions.** A comment posted from Lapilli is quoted, so `@name` in it
  notifies whoever the tracker says; that is as a task's text is today.
- **Into Lapilli**, a comment from the tracker is text: escaped on pages,
  quoted in Slack (it isn't told), and given to agents through MCP as the
  task's comments, which the server's instructions already say are data,
  not instructions.

### Attacker's look

- Someone with the tasks token can now also add comments to any task, as
  "from the tracker", up to the bounds above. They can't edit or delete
  a comment, write one as a person, or reach anything else.
- Someone who can comment on the tracker's issue can put text on the
  task's page in Lapilli. That is what kept in step means; it is shown as
  the tracker's.
- The recipes follow no URL a tracker gives, as before: Shortcut's search
  `next` and a comment's paging are rebuilt from the configured address
  and a checked cursor or number. A comment's id must have the tracker's
  shape before it is sent to Lapilli.

## In the ledger: schema 015

```sql
CREATE TABLE on_call_turns (
	schedule TEXT NOT NULL,
	start_at INTEGER NOT NULL,
	end_at INTEGER NOT NULL,
	who TEXT NOT NULL,
	PRIMARY KEY (schedule, start_at, who)
);
ALTER TABLE comments ADD COLUMN origin TEXT NOT NULL DEFAULT '';
ALTER TABLE comments ADD COLUMN ref TEXT NOT NULL DEFAULT '';
ALTER TABLE comments ADD COLUMN author TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX comments_ref ON comments (on_kind, on_key, ref) WHERE ref != '';
```

Numbered 015, after 0.25's 014 (grants, a quiet window's team and
schedule): the migrator applies the files in their order, so a new ledger
reaches 15, and a 0.25 ledger migrates from 14 to 15, its comments kept as
people's (`TestMigrate015`; a ledger written by v0.25.0 itself was opened
and migrated too, in the rebase onto it).

## Questions for review, settled

Settled by the maintainer on 2026-11-12, after two reviews of the build:

- **Mutes** aren't kept this release: keeping them is a new table, and a
  design of its own. But an alert muted in a delivery is no delivery of
  it: the recorder of 0.35 lists muted alerts among a notification's
  alerts, and they are left out (H2, below).
- **Turns** are kept within `--keep`: what lets twelve weeks be counted
  by a feed that looks back fifteen days.
- **A person's hours** are resolved through the file as it is now, for
  every turn they had, not kept with each turn: hours are a team's policy,
  and should be correctable. People added later are found in the turns
  kept before, by the names the file gives them now (M11).
- **Comments** are copied once, each way; an edit or a deletion in either
  place isn't followed this release.
- **A HelmRelease's deploys** are taken by a chart version ending `+SHA`
  (or `+SHA.N`), as built, and the README says so.
- **A source behind** holds a day's check of its alarms two days, as
  built, and a day held is told of when it is checked (M12).
- **Deploys aren't in the chart** this release.

## What the reviews found

Two reviews of the build, before release, found these, each fixed with a
test that fails without it:

- **H1. One tracker comment Lapilli refused jammed its task's comments,
  and failed every run** (every tracker). A comment Lapilli refuses is now
  said and passed over; one with no text says so ("no text here"); one
  longer than 10,000 characters is cut, and says where the rest is; past
  the 1,000 comments a task keeps, no more are taken, quietly. Lapilli
  refuses only what turns text around in a comment (the bidirectional
  marks, embeddings, overrides and isolates, U+061C) and controls, not a
  zero-width joiner in an emoji or a soft hyphen; an author's name follows
  the ledger's rule for names (`Hidden`, since v0.25: a joiner between
  letters of one script or two emoji, nothing else that doesn't show).
- **H2. Alertmanager 0.35 counted silenced and inhibited alerts as
  delivered.** Its notification event lists muted alerts among `alerts`
  as well as in `mutedAlerts` (by `fingerprint`; 0.33 and 0.34 by
  `hash`); they are left out. Recorded from a real Alertmanager, 0.34.2 and
  0.35.0, with a silence over one alert of a group.
- **M1. Whether a comment was posted was inferred from a bounded read** of
  the issue's comments: a thousand comments by anyone pushed the job's out
  of sight, and each run posted again. Lapilli now keeps the tracker's id
  of each comment posted (`PATCH /api/v1/tasks/{id}/comments/{comment}`
  with `posted`, set once), and with more comments on an issue than a run
  reads, nothing is posted to it.
- **M2. Restricted comments were copied to every reader of Lapilli,** and
  commenters' emails posted to issues: Jira's comments restricted to a role
  or group, and GitLab's internal notes, aren't copied; a comment is posted
  as "Written in Lapilli:" unless `COMMENT_NAMES=true`; the README and the
  chart say that `COMMENTS=true` publishes what people write on tasks.
- **M3. One notification the ledger won't keep dropped the rest of a
  batch** (an alert with 5 KB of labels, which anyone who can send
  Alertmanager an alert can make). Every notification is checked first;
  the rest are kept in one write, and the answer counts what was refused.
- **M4. A storm of events refused deliveries.** A body is read within a
  budget of bytes (8 MB a second), then taken at the notifications' rate if
  it carries any and the events' if it doesn't.
- **M5, M6, M7, M8. Deploys.** A deploy is never dated before its
  revision's commit (Flux dates a snapshot a revert brought back by when
  it was first applied); each deploy is kept, a rollback a version of its
  own; failed reconciles and failed releases aren't deploys; the versions
  are walked from the last deploy at or before `SINCE`, so the rules in
  effect then are the deployed ones, and a `SINCE` after the last deploy
  writes them.
- **M9. The job's own comment closed a reopened task again:** posting
  changes when the issue last changed. A task follows its issue's state by
  when the state changed (each tracker says it), not by its last change.
- **M10. Azure Boards' Basic process read Done as dropped** with
  `AZURE_BOARDS_REMOVED=Done`; done wins.
- **M11. A person added to the file later wasn't found in turns kept:**
  turns are resolved when the rota is built, and an event with one
  attendee keeps that attendee's email as who it is.
- **M12. A source behind stopped every rule's notices of differing:** a
  day held is told of when it is checked.
- **M13. A feed could take minutes to read** (thousands of dates left out,
  daily repeats from 1970): dates left out are looked up, a thousand an
  event at most; steps before the span read are skipped; a feed's repeats
  take two million steps at most in all; a read stops when asked.
- **L1.** `WKST` is honoured (RFC 5545's own example is a test).
- **L2.** A turn whose name the ledger won't keep is passed over, not the
  whole read: one rule for names, the ledger's.
- **L3.** An event far outside the span read is no turn, and a feed's turns
  replace those kept only from the span read on.
- **L4.** A revision in `DEPLOYS` is taken only as a commit whose full name
  starts with it, never a tag named like it.
- **L5.** A feed past the turns it may make is refused, not cut short
  quietly; a turn removed at a feed's head (Grafana OnCall keeps it,
  cancelled) is removed from those kept; Jira comments' mentions, code,
  headings and attachments are read as text; the last line of `DEPLOYS` is
  read without a newline; before Flux 2.7 the time is the last
  reconcile's, and a chart from an OCIRepository names no commit
  (documented); a chart from values files (`+SHA.N`) is read.
- **L6.** Shortcut's `requested_by_id` can be set by members: documented.
- **L7, and the docs.** Events' parse cost is in M4; `serve --help` and the
  README name `/v1/alertmanager/events`, `?skip=` and the double counting
  beside Lapilli's own webhook receiver.

## As built

As above, with these settled or found in the building:

- **The event recorder's two formats** were recorded from real
  Alertmanagers (0.33.1, 0.34.2, 0.35.0, and 0.35.0 batched) with a
  PagerDuty, a Slack and a webhook integration pointed at a server standing
  in for them; the recordings are the tests' data. An `index` of 0 isn't
  written, as protojson leaves zeros out. A body that carries no
  notification gives back what it took of the notifications' rate.
- **On-call turns** are kept flattened, one row a turn (`PRIMARY KEY
  (schedule, start_at)`); a read that holds the same turns as kept writes
  nothing, so pages aren't thrown away every 30 minutes. A schedule's key,
  by which counted weeks are kept, is a hash of its turns and its people's
  hours. A team's shifts naming a schedule `--on-call` doesn't are refused.
  The chart's `serve.onCall` mounts the file from a ConfigMap and the
  feeds' URLs and tokens from a Secret.
- **Deploys** aren't in the chart's `serve.rulesHistory`: its image has
  git, not `curl` or `jq`. A question below.
- **Comments copied in** are written by `tasks-token`, the author kept
  apart (`comments.author`), so the author check that keeps people from
  changing another's comment keeps everyone from changing these: an
  admin, a team's editor (v0.25's roles), anyone the sign-in proxy names
  `tasks-token` (a name kept for the job: the proxy's header can't take
  it). Team scope is people's, not the token's: the tasks token is the
  tracker job, of no team, and copies comments to, and marks comments
  posted on, every team's tasks, as it links and moves them. A team's
  editor comments on their team's tasks, and those comments are posted as
  anyone's are.
- **Names** from a tracker (a comment's author) and from an on-call feed
  follow the ledger's rule for names, `Hidden`, as v0.25 gives it to
  people's names; a comment's text follows the narrower rule above.

## Sources

- Alertmanager's event recorder, 0.34.2 and 0.35.0:
  https://github.com/prometheus/alertmanager/blob/v0.34.2/eventrecorder/eventrecorderpb/eventrecorder.proto,
  https://github.com/prometheus/alertmanager/blob/v0.35.0/proto/eventrecorder/events/v2/events.proto,
  https://github.com/prometheus/alertmanager/blob/v0.34.2/eventrecorder/webhook.go,
  https://github.com/prometheus/alertmanager/blob/v0.34.2/docs/configuration.md (Event Recorder),
  https://github.com/prometheus/alertmanager/blob/v0.35.0/CHANGELOG.md
- RFC 5545, iCalendar: https://www.rfc-editor.org/rfc/rfc5545
- PagerDuty's schedule feeds:
  https://docs.pagerduty.com/incident-management/schedules/schedules-in-apps
- Opsgenie's schedule export: https://docs.opsgenie.com/docs/schedule-api;
  Jira Service Management's:
  https://developer.atlassian.com/cloud/jira/service-desk-ops/rest/v1/api-group-schedules/
- Grafana OnCall's export: `engine/apps/schedules/models/on_call_schedule.py`
  and `engine/apps/schedules/ical_utils.py` in https://github.com/grafana/oncall
- Google Calendar's secret address: https://support.google.com/calendar/answer/37648
- Argo CD's `RevisionHistory`:
  https://github.com/argoproj/argo-cd/blob/master/pkg/apis/application/v1alpha1/types.go,
  RBAC: https://argo-cd.readthedocs.io/en/stable/operator-manual/rbac/
- Flux's Kustomization history (kustomize-controller 1.7, Flux 2.7) and
  HelmRelease snapshots:
  https://fluxcd.io/flux/components/kustomize/kustomizations/,
  https://fluxcd.io/flux/components/helm/helmreleases/
- Compass's alert logs: https://developer.atlassian.com/cloud/compass/rest/v1/api-group-alerts/;
  Opsgenie's: https://docs.opsgenie.com/docs/alert-api-continued#list-alert-logs
- Azure Boards: https://learn.microsoft.com/en-us/rest/api/azure/devops/wit/work-items,
  https://learn.microsoft.com/en-us/rest/api/azure/devops/wit/wiql,
  https://learn.microsoft.com/en-us/rest/api/azure/devops/wit/comments,
  https://learn.microsoft.com/en-us/azure/devops/boards/work-items/workflow-and-state-categories
- Shortcut: https://developer.shortcut.com/api/rest/v3
- Comments: https://docs.github.com/en/rest/issues/comments,
  https://docs.gitlab.com/api/notes/,
  https://docs.gitlab.com/user/project/quick_actions/,
  https://docs.gitlab.com/user/work_items/,
  https://developer.atlassian.com/cloud/jira/platform/rest/v3/api-group-issue-comments/,
  https://linear.app/developers/graphql
