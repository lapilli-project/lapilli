# `lapilli serve`, run by the people who use it

Status: built, for the next release (see "As built" at the end). Under
[GOVERNANCE.md](../../GOVERNANCE.md) roles kept in the ledger and quiet
windows of a team are a new kind of data in the ledger, so the design came
first, in its own commit, and is open for review with the code. The companion to [work.md](work.md) (quiet windows, roles),
[reports.md](reports.md) (the weekly digest) and [content.md](content.md)
(the range check, Grafana's notification templates). The decisions taken
while building, where this note had to choose, are marked **Builder's
decision**; they are the first thing to review.

## Why

`lapilli serve` is set up by whoever runs it, with flags and a restart, and
used by others, who can't change what they need to:

- A quiet window repeats every day or every week. Maintenance comes on the
  first Saturday of the month, a batch on the 15th, a close on the month's
  last day; each is written down again every month, or not at all.
- The weekly digest is set by `--digest`, `--team-digest` and
  `--slack-webhook-file`. The reports people schedule are set in the UI
  (0.22), but the digest is shown there read-only: moving it to Tuesday, or
  turning it off, needs the person who runs `serve`.
- Roles come from the flags: `--editors`, `--admins`, `--approvers`. Giving
  someone an editor's role needs a restart, and there is no role between
  "may look" and "may change every team's notes, tasks and quiet windows".
  A team that keeps its own quiet windows has to be given everyone's.
- The content check's range check counts with one scrape interval,
  Prometheus's global one. A job scraped every 15 seconds under a global
  minute is faulted for `rate(x[1m])`; a job scraped every 2 minutes under
  a global 30 seconds isn't, though its `rate(x[3m])` holds one point.
- A Grafana rule's notification says what its contact point's template
  says, not only what its annotations say. A template that prints
  `{{ .CommonLabels.cluster }}` or `{{ .CommonAnnotations.runbook_url }}`
  prints nothing for a rule whose alerts have neither, and the content
  check doesn't look.

## 1. Quiet windows that repeat by month, or by a cron expression

A window can repeat every day, every week (as now), **every month**, or **by
a cron expression**. Its schedule is read by `internal/schedule`, the
reports' reader, so the two can't differ:

| `every` | `on` | Falls due |
|---|---|---|
| `day`, `week` | | as now |
| `month` | `15` (1 to 28) | the 15th, at the start's time of day |
| `month` | `last` | the month's last day |
| `month` | `first sat` (`first`, `second`, `third`, `fourth`, `last`) | the first Saturday |
| `month` | empty | the start's day of the month, 1 to 28 |
| `cron` | five cron fields, `0 2 * * 6` | as cron says, hourly at most |

- **The first repeat** is the window's start and end, as now. Its start must
  be a time its schedule falls due at; one that isn't is refused, and the
  refusal names the next one that is.
- **A repeat lasts** what the first does, 31 days at most, and less than
  the shortest time between two repeats in the year from its start, so that
  a firing is in the last repeat that started at or before it, or in none:
  one schedule lookup, whatever the number of repeats. A month by day or weekday is 28 days at
  shortest; a cron expression's is worked out when the window is kept.
- **Daylight saving** is the reports': a time the clocks skip falls due when
  they move forward past it, a time they repeat, once, the first time.
- **Until** is the last day a repeat starts on, as now.
- `internal/schedule` gains the weekday of a month, `monthly first
  sat@02:00`, which reports can use too.

Judging is as it is for windows that repeat now: keeping or changing one
judges again from its first repeat (or the oldest firing kept) to now, a
week at a time.

## 2. The weekly digest, set in the UI

The weekly digest becomes a report people change in the UI and the API,
beside the others, while the flags keep working.

**Builder's decision: the flags define the digest until an admin changes it
in the UI or the API; from then on the ledger does.** The flags then only
name where it can go (`slack`, `webhook`, `team:NAME`, and the
`--report-dest` names), as they do for every report. "Back to the flags"
gives it to the flags again. The other way round (flags always win, the UI
a layer on top) makes a change in the UI quietly undone by the next deploy
that happens to touch `--digest`; this way, which one defines it is shown
on the Reports page and in the API, and the log says so at start
("the weekly digest is set in the ledger; --digest isn't used").

What can change:

- **When:** `weekly DAY@HH:MM` only. The digest is of the week, Monday to
  Monday; its suggestions and near misses are worked out at that time,
  whether it is sent or not, as the proposals and the incident pages need
  them.
- **Where to:** any of the destinations `serve` names. A `--report-dest`
  name gets everyone's digest as text, through the outbox, as reports go;
  `slack`, `webhook` and `team:NAME` as now.
- **On or off.** Off, nothing is sent; the week's work is still done.

What can't: its sections, its span and whom it is for are the digest's. A
report people make can hold any of them already.

**Upgrades.** A ledger with nothing set is the flags' digest, as now: the
weeks sent are kept where they are (`digest-sent`, `digest-told`), so an
upgrade sends no week twice and loses none. Setting it in the UI keeps the
same record: a week sent before the change isn't sent again after it, to
the same or to new destinations, and a week due and not yet sent when it is
set goes once, to the destinations then set. A week that falls due while
it is off is passed over, as a report's is: turned on again, it goes from
the next week. The digest never goes back to a week older than the last one
worked out, whatever its day is moved to.

API: `GET`, `PUT` and `DELETE /api/v1/reports/digest` (admins change it;
`DELETE` gives it back to the flags). `GET /api/v1/reports` shows it first,
as now, with `"kind": "digest"` and `"set_by": "flags"` or `"ledger"`. In
the ledger it is one row of `meta` (`digest.set`): when, where to, on, who
set it and when. A change is a row of `changes`.

## 3. Roles kept in the ledger

Admins give roles in the UI, on top of the flags:

| Role | Given by | May change |
|---|---|---|
| admin | `--admins` only | everything an editor may, and the settings, teams, reports, the digest and roles |
| approver | `--approvers` only, or the admins | approve and undo a change to a rule |
| editor | `--editors`, or an admin in the UI | incidents, tasks, comments, quiet windows, notes on rules; proposes changes |
| editor of a team | an admin in the UI | that team's notes, tasks, quiet windows and reports, and comments on what is the team's |
| viewer | everyone else | nothing |

**Builder's decision: the UI gives editor and editor of a team, never admin
or approver.** The flags stay the floor and the ceiling: a role a flag gives
can't be taken away in the UI, and the UI can't make anyone more than an
editor. Admin and approver are who may change the settings and approve a
change to a rule; making either from a page means one stolen session makes
more admins, and an admin could approve their own proposal with a second
account. The ledger refuses any other role in the table itself (a `CHECK`),
so a bug above it can't store one.

Who is who still comes from the sign-in proxy (`--user-header`,
`--groups-header`), and the rest of [serve.md](serve.md)'s threat model
holds unchanged:

- **A grant names `user:NAME` or `group:NAME`**, never a bare name, which in
  the flags is either and lets a person whose name is a group's take the
  group's role. Not `*`: everyone is given a role by the flags only. Not
  `anonymous`, nor the names Lapilli notes its own and its tokens' writes
  under.
- **Roles in the ledger need roles on:** `--admins` given (without it there
  is no one to give them, and without `--editors` or `--admins` everyone is
  an admin already), and not `--read-only`, under which no one changes
  anything, granted or not.
- **Giving and taking away a role** is an admin's write: a same-site `POST`
  (`/roles`), or the API (`GET POST /api/v1/roles`, `DELETE
  /api/v1/roles/{id}`), through Go's cross-origin protection as every write
  is. Only admins see the roles page: who holds a role is not everyone's to
  read. `GET /api/v1/me` says the reader's role, and the teams they edit.
- **A role taken away** stops at the next request: roles are read from the
  ledger once for each change to it, as the pages are cached.
- **A team removed** takes its editors with it, in the same write: a team
  made later may be given the removed one's id, and mustn't inherit them.

### What a team's editors may change

Each is checked in the write that changes it, inside the ledger's
transaction, so a row moved from one team to another meanwhile can't be
changed by the wrong team's editors:

- **Tasks** of their team, before and after: a task can't be moved to
  another team, or taken from one.
- **Quiet windows** of their team (below). A window for everyone, or for
  another team, is an editor's.
- **Notes** on a rule their team owns, without changing which team owns it:
  a note names its team, and moving a rule in or out is an admin's or an
  editor's.
- **Reports** of their team, sent only to their team's destination
  (`team:NAME`, from `--team-digest`). Not the digest, and not `slack` or
  another team's channel. An editor, the editor of every team, may make
  any team's report so; a report for everyone, or sent elsewhere, stays an
  admin's.
- **Comments** on a rule their team owns, a task of their team, or an
  incident their team owns.

Not incidents (an incident turns firings white for every team), not
proposals, settings, teams, the digest or roles.

### A team's quiet window

A quiet window can be a team's (`team_id`). A team's window excuses only
the firings of rules its team owns, so its editors can't quiet another
team's alerts:

- Who owns a rule is worked out in the ledger, where the stones are given,
  as the pages work it out: a note's team, or else the first team, by name,
  whose pattern matches the rule's name or whose labels the alert has.
  **Builder's decision:** the pages read the labels the rule sets; the
  ledger reads the labels the alert has, which hold those and more, as the
  ledger keeps alerts and not each version's rule labels. An alert whose
  series carries a team's label, of a rule that doesn't set it, is the
  team's in its window. That reads the team's labels as an alert manager
  routes on them.
- A change to who owns what (a team kept or removed, a note's team changed)
  judges every team's window's span again, a week at a time, as a window
  kept does.
- A team removed stops its windows, as it stops its reports: their team is
  the removed one's id negated, which no team has, and they excuse nothing
  until an admin or an editor gives them a team.

### Schema

`014.sql`:

- `grants`: `id`, `who` (`user:NAME` or `group:NAME`), `role` (`editor`
  only, by a `CHECK`), `team_id` (0 for every team), who gave it and when;
  one row for each person or group and team.
- `windows.team_id` (0 for everyone's) and `windows.repeat_on` (the month's
  day or weekday, or the cron expression).

## 4. A job's own scrape interval, for the range check

`/api/v1/status/config` gives each scrape job's `scrape_interval` beside
the global one, and `/api/v1/targets` the `job` label each job's targets
end up with after relabelling: under the Prometheus Operator, job
`serviceMonitor/payments/checkout/0` is `job="checkout"`. Both are asked
once a day, as the global interval is now.

- A range in a selector that names its job (`rate(x{job="node"}[1m])`, or
  `job=~"node|db"`) is counted against that job's interval: the longest of
  the jobs it can be, since the range must hold two points of each.
- A selector that names no job, or a job Prometheus doesn't have, is
  counted against the global interval, as now; or, when every job has one
  interval, that one.
- The finding says which interval it counted with, and whose:
  `rate[1m] < 2 × 2m (job "batch")`.
- `--scrape-interval` still sets one interval for every range; with it,
  nothing is asked.
- Where `/api/v1/targets` isn't served, a job's label is taken to be its
  name, as Prometheus sets it when nothing relabels it.

No schema change: what was read is kept in memory, as the global interval is.

## 5. Grafana's notification templates

With `--grafana`, `serve` also reads, once an hour, Grafana's notification
templates (`/api/v1/provisioning/templates`), contact points
(`/api/v1/provisioning/contact-points`) and notification policies
(`/api/v1/provisioning/policies`), with the same token, and only reads
them. For each Grafana rule it works out the templates its notifications
are written with, and checks them as the content check checks a rule's
annotations:

- **Which templates.** A rule's contact point is its own
  (`notification_settings.receiver`, simplified routing), or the one the
  policy tree routes it to, by its labels, `alertname` and
  `grafana_folder`. A route matching on a label the rule doesn't set (one
  its series may carry) could go either way: both ways are followed, and a
  template counts only when every contact point the rule can reach uses
  it, so a template is never held against a rule that may never be sent
  with it. A contact point uses the templates its fields name
  (`{{ template "x" . }}`), and those they name in turn.
- **What is checked**, of each template so used, and shown on the rule's
  page beside its own findings, with the template's name:

| Check | Kind | When |
|---|---|---|
| A notification template shows an annotation the rule hasn't | fact | `.Annotations.summary`, `description` or `runbook_url` (or `.CommonAnnotations.`), not under an `if` or `with` naming it, and the rule has none |
| A notification template names a label the alert never had | measured | `.Labels.x`, `.CommonLabels.x`, `.GroupLabels.x` or `index .Labels "x"`, unguarded, and no firing of the rule in twelve weeks had `x` |
| A notification template names a label the expression can't give | heuristic | as the rule's own, for a rule that hasn't fired |

- **What is read** comes from whoever edits Grafana's alerting: each
  template is cut to 64 KB, parsed and never executed, its names bounded as
  a rule's details are. The requests go to `--grafana`'s host only, follow
  no redirect, read 8 MB at most each, and are a token's reads
  (a Viewer's service account reads them in Grafana 13.3.1). A Grafana that
  refuses them (an older one, a token without the scope) leaves the check
  as it was, and the log says once why. Grafana refuses a template that
  doesn't parse when it is saved, so that isn't a check: one read that
  doesn't parse is passed over.
- **Rules from provisioning files**, not `--grafana`, aren't checked
  against templates: the templates are wherever Grafana keeps them.

No schema change.

## Alternatives

- **A Kubernetes RBAC, or the proxy's groups only.** The groups are already
  read; what was missing is giving a role without a restart, and a role
  smaller than an editor's.
- **Team windows that only refuse a pattern outside the team's.** A pattern
  can't be checked against rules that don't exist yet, and labels can't be
  checked at all; the owner is.
- **The digest as a row of `reports`.** Its name would collide with a
  report people named "weekly digest", the report loop would have to pass
  it over, and the limit of fifty would count it. It is one setting, kept
  as the settings are.
- **Per-job intervals by the metric's targets** (`/api/v1/targets/metadata`)
  for selectors that name no job: a request for each metric a day, and
  nothing on Prometheus's own recorded series. The job matcher and the
  global fallback answer most rules.

## Questions for review

- Should a team's editors give their own team's role to others?
- Should a team's editors write incidents that only their rules caught?
  They are left out, as an incident turns firings white for every team.
- Is it right that a team's window reads the alert's labels, not the
  rule's? The other way needs each version's rule labels in the ledger.
- Should "back to the flags" be offered at all, or should setting the
  digest in the UI be for good, the flags then only naming destinations?
- Should the template check also follow Grafana's default templates? They
  print every label and annotation there is, and fault nothing.

## As built

Where the build settles what the above left open, or differs from it:

- **Schema 014**, as above: `grants`, `windows.team_id` and
  `windows.repeat_on`. Upgrading needs nothing: no window is a team's, and
  no role is given until an admin gives one.
- **Quiet windows.** `every` is `day`, `week`, `month` or `cron`, and `on`
  says when for the last two; a month's `on` left empty is kept as the
  start's day, so the window reads the same later. A refused start names
  the next time its schedule falls due. Judging a window every month or by
  cron over twelve weeks of 200,000 firings takes 1.2 s on a laptop, as long
  as one every day: the cost is the judging, not the schedule
  (`TestPagesScale`).
- **A team's window** is judged with who owns what as it is when the
  stones are given; a team kept, renamed or removed, or a note's team
  changed, notes every team's window's span to be judged again, a week at a
  time, in the same write. A window whose team was removed says so on the
  windows' pages.
- **Who sees what.** The pages offer a team's editor their teams only, and
  never "everyone's"; a viewer is offered no comment form, where before any
  reader was. `GET /api/v1/me` gives `teams`, the names of the teams the
  reader edits.
- **Refusals.** A team's editor refused is told "only an editor of its team
  can change this" (403); a team's report sent elsewhere than its own
  destination, 403 too.
- **The digest** set in the UI is kept in `meta` as above; a week passed
  over while it is off is noted as sent, to every destination, so turning
  it on and off can't send a week twice either. `GET
  /api/v1/reports/digest/preview` gives `report.ready`'s shape of the
  digest as it would be sent now.
- **Per-job intervals.** Read from `/api/v1/status/config` (each job's
  `scrape_interval`, its name its label) and `/api/v1/targets?state=active`
  (the label each job's targets end up with, and their interval), checked
  against a real Prometheus 3.5.0, whose answers are kept as test data.
  `--scrape-interval` still sets one interval for every range.
- **Grafana's notification templates.** Checked against a real Grafana
  13.3.1 (its answers, and the script that set it up, are kept as test
  data). A contact point's own fields are templates too, so a field that
  shows `{{ .CommonLabels.cluster }}` itself is checked like a named
  template. Routing gives up (no template findings for the rule) past 64
  ways a rule can go, 2,000 policies or 32 levels; a label Grafana gives
  every alert (`alertname`, `grafana_folder`, those starting `__`) is never
  faulted. `lapilli verdict --check-content --grafana` checks the same.
- **The questions for review** are left open: a team's editors don't give
  roles or write incidents; a team's window reads the alert's labels; "back
  to the flags" is offered; Grafana's default templates aren't followed.
