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
  the shortest time between two repeats in the four years from its start,
  a leap year's cycle. Every repeat lasting as long, a firing is in the
  last repeat that started at or before it (by the end of Until's day,
  when later), or in none: one schedule lookup, whatever the number of
  repeats or the gap between two. A month by day or weekday is 28 days at shortest, less the hour
  daylight saving takes; a cron expression's is worked out when the window
  is kept.
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
| approver of a team | `--config` only (`approvers:` under the team) | approve and undo a change to that team's rules |
| editor | `--editors`, or an admin in the UI | incidents, tasks, comments, quiet windows, notes on rules; proposes changes |
| editor of a team | an admin in the UI | that team's notes, tasks, quiet windows and reports, and comments on what is the team's; proposes changes to its rules |
| viewer | everyone else | nothing |

The Roles page says who may approve in effect (those `--approvers` names,
the admins, or no one, and why), and a proposal's page, to an admin, who
may approve it and when no one is left; anyone else reads only that an
approver approves it, or that no one can yet ([ui.md](ui.md), "First
use").

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
settings, teams, the digest or roles. Since 0.64.0
(below, "A team's boundaries of change"), **proposals** for a rule their
team owns: made, backtested again, and dropped when they made them;
checked in the request, as a proposal isn't a row of a team's.

### A team's quiet window

A quiet window can be a team's (`team_id`). A team's window excuses only
the firings of rules its team owns, so its editors can't quiet another
team's alerts:

- Who owns a rule is worked out in the ledger, where the stones are given,
  as the pages work it out: a note's team; or else the first team, by
  name, whose pattern matches the rule's name; or else the first whose
  labels the alert has. The rule's name decides before any label, on the
  pages as in the ledger (**maintainer's call, 2026-11-12**, after a
  review found a team owning by labels, and sorting first, quieting a rule
  another team's pattern owns): a team that owns by labels can't take a
  rule another team names. The pages read the labels the rule sets; the
  ledger, the labels the alert has, which hold those and more, as the
  ledger keeps alerts and not each version's rule labels: an alert whose
  series carries a team's label, of a rule no note or pattern names and
  that doesn't set the label, is that team's in its window. A window's page
  names the team of each firing it excused.
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

Settled, **maintainer's call, 2026-11-12**:

- A team's editors don't give their team's role to others: roles are an
  admin's.
- A team's editors don't write incidents, even those only their rules
  caught: an incident turns firings white for every team.
- Who owns a rule: the rule's name (a note, a pattern) decides before any
  label, on the pages and in the ledger; a team's window reads the alert's
  labels only for a rule no note or pattern names (above).
- "Back to the flags" stays.
- Grafana's default templates aren't followed: they print every label and
  annotation there is, and fault nothing.
- An admin with a second sign-in can make it an editor, propose from it
  and approve from the first: a matter for the identity provider, which
  says who is who, as two names in `--editors` would be. [serve.md](serve.md)
  says so.

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
  template. Routing gives up (no template findings for the rule, and its
  page says why) past 64 ways a rule can go, policies past the 2,000 or 32
  levels read, a million words of work for the rule or twenty million for
  the check, or five seconds of the check; a label Grafana gives
  every alert (`alertname`, `grafana_folder`, those starting `__`) is never
  faulted. `lapilli verdict --check-content --grafana` checks the same.
- **Reviewed as an attacker would**, and fixed: a report of a team that an
  admin sent elsewhere (to `slack`, say) was a team's editors' to redirect
  or delete; now a team's editors, and editors, change only a team's
  report sent to its own destination, before and after, checked in the
  write, and the page offers no other. `grants.who` is held to `user:` or
  `group:` by a `CHECK` too, and a grant of any other kind is passed over;
  `incidents-token` is a name kept for Lapilli. A team's editors make ten
  of its reports at most, so they can't use up the ledger's fifty.
- **What is checked before the write**, not in it: which rules a team owns
  (for a note or a comment) and which teams the reader edits are read just
  before the write. A team's patterns changed, or a role taken away, in the
  same moment can let one write through as it was a moment before; that
  needs an admin acting then, who could make the change anyway.
- **The questions for review** are settled above.

## What the reviews found

Two reviews of the built batch, one as an attacker would and one for
correctness, found no way to an admin's or an approver's role. What they
found is fixed, each with a test that fails without the fix:

- **A notification-policy tree pinned the CPU.** A tree a real Grafana
  13.3.1 accepts (1,993 policies, three levels) took 13 s a rule, holding
  the content check while pages waited. What a child policy gives is
  worked out once, not once a way; a way's contact points are a set; ways
  alike are kept once; and a rule's routing gives up past a million steps,
  as past 64 ways. Three rules now route through it in 10 ms.
- **A team's window quieted another team's rule** (above): the rule's name
  decides before any label.
- **Grafana's older matchers.** `match` and `match_re`, which 13.3.1 keeps
  and gives back as written, matched every rule; read as `=` and `=~`.
- **A rule's label written as a template** (`team: '{{ $$labels.team }}'`
  in Grafana's export) was routed on as text: it is filled in from the
  series, so a policy on it goes either way.
- **A job's regular expression** was compiled again for each job, range
  and rule (2,000 rules over 2,000 jobs, 25 s): once, now (0.3 s).
- **The digest.** A week sent went again to a destination added after; the
  outbox's guard read a key TellOnce didn't write, so every round worked
  the digest out again (also on main); set in the UI the first time after
  its week went nowhere, the week was taken for old news; and with the
  flags sending it nowhere, it wasn't on the Reports page to set.
- **The heuristic on notification templates** could never fire, Grafana
  rules having no expression of their own: it reads a rule's one query.
- **Policies past those read** (2,000, or 32 levels) were guessed to stop
  at their parent: a rule that may reach them isn't routed.
- `{{ or .CommonLabels.x "fallback" }}` is guarded.
- **Quiet windows.** The shortest gap was measured over one year, so a
  later February could make repeats meet: four years now. The query for
  the windows a span meets dropped the last day of a 31-day repeat that
  starts late on its Until's day. A window whose team was removed was given
  to everyone when its form was saved as shown.
- **Who holds a role** showed on the Run history page, everyone's: only admins see
  those changes there.
- **The reports** could be filled by editors, ten a team: the last ten are
  an admin's.
- **Names with characters that don't show** (a zero-width space, a soft
  hyphen, a word joiner) had them taken out, and became another's, an
  admin's: such a name, or one over 200 bytes, is no one's. This was so
  before this batch; it matters more with roles given by name.
- `lapilli serve -h` says what `--digest`, `--admins` and
  `--check-content` do now.

### The verification

The fixes above were verified, on a branch rebased onto 0.24.0, against
the reviews' proofs: thirteen held, five held in part, and a real 0.24
ledger migrated to schema 014 with the same stones. What it found is
fixed, each with a test that fails without it:

- **The step budget didn't bound time.** A 1,697-policy tree a real
  Grafana 13.3.1 accepted still routed, in 1.4 to 2.7 s a rule, and 500
  rules held the check twelve minutes: each step's union sorted and copied
  names, uncharged. Contact points are numbered and a way's are a bitset;
  work is charged by the word, a million a rule and twenty million a
  check; rules alike in the labels the policies match on are routed once;
  the check routes for five seconds at most. That tree gives each rule up
  in about 20 ms; 500 rules take 0.3 s. A rule given up on says so on its
  page, and why; the log says how many, once a check.
- **"Back to the flags"** didn't note the week as sent on the ways the
  flags add: Slack heard again of a week told to ops.
- **The heuristic** missed Grafana's usual query, reduce, threshold: the
  condition is followed back to its one query, and a reduce folded into a
  range query unwrapped.
- **Four years isn't every cron's cycle**: "the 31st of January, or a
  Friday in January" comes closer only in 2032. A repeat is looked up by
  the end of Until's day when the firing is later, so the gap no longer
  decides what is covered.
- **A 0.24 ledger** whose two records of the digest's last week disagree
  takes the later, not sending a week 0.24 passed over.
- **Until's day can last 25 hours**: the query for the windows a span
  meets allows two days past it, not one.
- **Names**: refusing every format character made Persian names and
  emoji, written with joiners, viewers; the joiners between two letters
  of one script, or two emoji, are kept, and the combining grapheme joiner
  and the Hangul fillers, which passed, are refused. A grant's name is
  counted in bytes, as the proxy's.
- **The new owner order** changes more than the pages: the changelog says
  what moves with a rule.

Found while checking these against Grafana 13.3.1, and left for its own
change: Grafana's export writes a `$` in a template as `$$`, so a Grafana
rule read with `--grafana` whose annotations use `{{ $labels.x }}` is
faulted for a template that doesn't parse.

## Declared configuration

Status: built, for the next release, schema 019. Teams, quiet windows,
reports and the editor roles admins give were the ledger's alone, made in
the UI: a team that runs everything through Git couldn't review them,
restore them or copy them to another cluster, and Helm's values declared
only the roles and five judging settings. Argo CD declares its projects
and repositories in the cluster; Grafana provisions dashboards and alert
rules from files and marks them provisioned, read-only in its UI. Lapilli
takes Grafana's model: what a file declares is read-only in the UI, which
says where to change it. **Maintainer's call, 2026-11-16**: no drift or
self-heal model; the file is read as it changes, not against what the UI
did meanwhile, which it can't do to what is declared.

### The file

`lapilli serve --config FILE` (`serve.config` in the chart, rendered into a
ConfigMap) is YAML of four lists, each item with the fields the UI's form
for it takes, under the names the API gives them:

```yaml
teams:
  - name: payments
    rules: ^Checkout              # rules whose names match
    labels: {team: payments}      # or whose labels have these values
    note: owns checkout and cart
    work_hours: mon-fri@09:00-18:00
    sleep_hours: 23:00-07:00
    tz: Asia/Seoul
    shifts: ""
    renamed_from: ""              # its name before: renamed in place
windows:                          # quiet windows
  - title: Nightly backup
    team: payments                # everyone's when left out
    start: 2026-11-02T02:00       # in --tz, or with an offset (RFC 3339)
    end: 2026-11-02T03:00
    every: day                    # day, week, month or cron; once when left out
    repeat_on: ""                 # month: 15, last, first sat; cron: 0 2 * * 6
    until: 2027-06-30             # the last day one starts; for good when left out
    rules: ^Backup
    labels: {env: [prod, stage]}  # a value, or a list of them
reports:
  - name: payments weekly
    team: payments
    schedule: weekly mon@09:00
    span: ""                      # a cron schedule's span, such as 24h
    zone: team                    # ledger (when left out) or team
    sections: [summary, noise, missed, woken]
    dests: [team:payments]        # --report-dest's names, slack, webhook, team:NAME
    enabled: true                 # true when left out
grants:                           # an editor's role, of every team or of one
  - group: payments-oncall
    team: payments
  - user: ana@example.com
```

A window's `on` is `repeat_on`, the ledger's column: Helm reads values as
YAML 1.1, where a key `on`, unquoted, is `true`.

A fifth list, `goals:`, declares what the alerting is held to
([goals.md](goals.md)): `name`, `team` (left out for everyone's, `"*"`
for each team's), `measure`, one of `at_least`, `at_most` and
`no_worse_than`, `by`, `critical` and `each`. Only the file declares a
goal: one it no longer declares is deleted, with or without
`--config-prune`, and without `--config` there are none. An export gives
them back as the file declares them.

A sixth key, `notify:`, declares who is told what: routes by an event's
kind, team and labels to destinations the flags name, a default, and a
destination's own words ([notifications.md](notifications.md#who-is-told-what)).
It isn't kept in the ledger: every replica reads it, as each tells of what
it writes, and puts it in force once it has read the same file twice in a
row; an export doesn't give it. A `notify:` that can't be read refuses the
file, as the rest does; each replica reads `notify:` on its own, so what is
wrong elsewhere in the file leaves the routes in force.

**Read strictly, or not at all.** A key it doesn't know, a value of
another kind, a key given twice, a second document, a file over 1 MiB (a
ConfigMap's most), more things than the ledger keeps (1,000 teams, 500
quiet windows, 50 reports, 1,000 roles, 50 goals), a goal that breaks a
rule of [goals.md](goals.md)'s, an empty file, or a key of the
four with nothing after it (`grants:` alone, which YAML reads as null) is
refused as a whole, with its line, and changes nothing: the ledger keeps
what the last file declared. An empty or truncated file, read as
Kubernetes swaps a ConfigMap, would otherwise take away everything
declared; to declare nothing, `teams: []`, and to declare no roles,
`grants: []`. So is a key of a thing with nothing after it (`team:` or
`rules:` alone): read as nothing, a role's or a quiet window's team would
be every team, and a quiet window's rules every rule; to mean that, leave
the key out. A name, a title, a user, a group or a team given as
a boolean or a number (`no`, `0123`, `1.10`), and a label's value given as
a boolean or a fraction, is refused too: see "Through Helm" below.

**Each thing, on its own.** Read, each item is checked as the UI's form is:
the ledger's checks (`checkTeam`, `checkWindow`, a report's `check`,
`grantName`) and the server's (a report's destinations, a team's
`on-call=NAME` shifts), and its team must be one the file declares or the
ledger holds. One that fails is skipped, and what the ledger held of it is
kept as it was, when what it is can be told: a quiet window of its title
and its team, a role of its holder and its team, a team of its name; one
whose team isn't there keeps nothing, so no window of another team, or
everyone's, stays declared, quieting what the file no longer says to; everything else is kept, and what the file no longer
declares is given back (or deleted) all the same: a file that takes a role
away and adds a quiet window that can't be kept still takes the role
away. A team that can't be kept is kept as it was, and so is everything in
the file that names it, each skipped and said why: the file meant the team
as it declares it, so a window, a report or a role of it isn't moved onto
the team the ledger holds under that name. The costly checks (a cron
window's gaps) run before the write.

**Limits of its own.** What a file declares isn't counted against what the
UI may make (1,000 teams, 500 quiet windows, 50 reports, 1,000 roles), nor
the UI's against it: an editor who fills the windows can't keep a file
from declaring one. Both together are bounded too, at twice each: past
it, a thing the file would add is skipped, said why. What the file no
longer declares is let go before what it adds is made, so a file that
swaps one thing for another at that most makes the new one. It is given
back to the UI only while the UI holds less than its own most; past it, it
is deleted, the activity log saying why, or, a team something names, kept
declared and said: declared and taken out again and again, nothing piles
up past what the UI may make. Without `--config`, everything is given
back, whatever the UI holds.

### Read twice before it is kept

**A file is kept to only once the same file has been read twice in a
row.** The leader reads it every 15 seconds and hashes it (SHA-256). A
hash the ledger isn't kept to is only noted on its first read; read again,
15 seconds or more after, with no other read between (another file, one
that can't be read, or the one the ledger is kept to), the ledger is kept
to it, in one write, and the next new file starts over. A write the
ledger fails doesn't count: the file is tried again after a while, still
needing its two reads in a row if another is read before then. So a file
read half written, or swapped in part as Kubernetes updates a ConfigMap,
changes nothing at all: it gives no one a role, of a team or of every
team, widens no quiet window, takes no role away and deletes nothing.
Kept to, a file is kept to as a whole: what it no longer declares is let
go at once (below). A file that can't be read as `--config` is said at
once, on its first read; what of one can't be kept is said once it is
kept to. As a term begins, at start or as another replica takes over,
the file is read once, before the rules are read, and the ledger is kept
to it at the next read, 15 seconds after it has read the rules: usually
within half a minute of the term's start, but that is no bound. Until then the ledger holds what
the last leader kept it to, and the Settings page says when a new file
was read once. A ledger write that fails is shown there too, with when it
is tried again, not as a file read once. Files other than the one
applied, read with none of them the same twice in a row (a ConfigMap
changing back and forth, or terms too short to read a file twice, which
keep the ledger from every file), are shown with since when they have
been, and logged once that has lasted five minutes; the one applied, read
the same twice in a row, ends it. Without `--config` there is no file to
read twice: what a file declared is given back at once.

A blank team or quiet window's rules, `team: ""` or `rules: ' '`, is
refused as one with nothing after it is: as nothing, either would hold for
every team or every rule.

### Keeping the ledger to it

`ledger.Reconcile` makes the ledger hold what the file says in one write.
Each thing is matched by its name:

- a team by its name, or else in any letter case when one team alone has
  it (a file's `Payments` renames the ledger's `payments`); every team by
  its name first, then, of those no team has the name of, by its
  `renamed_from` (trimmed), so the order the file lists them in changes
  nothing. A name and a `renamed_from` that are two teams, an old name
  given twice, or one another team declares by its name, is skipped and
  said, and the teams it names kept as they were. A rename is noted as one
  (`team B renamed to C`);
- a report by its name; a role by whom it is given to and its team;
- a quiet window by its title: those alike first, then a declared one of
  its team, then a declared one of another team (the file moved it), then
  one the UI made of its team. A window taken out of a file of several with
  one title leaves the others as they were.

What the UI made is taken over only of the same team: a declared window
whose title only another team's window has is made beside it, and a report
whose name the UI gave another team's report is skipped, as names are
unique. Taken over, it is counted so ("taken over") and noted `X declared
in config`, as the file says it. A new one is made, marked declared. Each
change is noted in the activity log, by `config`; a quiet window made is
told as one the UI makes is (`window.kept`), and the windows changed are
judged again. A file that says what the ledger holds writes nothing at
all: the write is taken back, and no page is worked out again.

What is declared and the file no longer names is the UI's again, as it
was, but a role. With `--config-prune` (`serve.configPrune`) it is
deleted.

**A role is taken away only when a file that was read twice no longer
lists it**, pruned or not: a file is kept to only once it is read the same
twice in a row (above), so a file read once takes no one's role. One the
UI made and the file took over is
taken away so too, each noted as a role taken (`RoleTaken`, by `config`,
with whom it was given to and its team). **Maintainer's call,
2026-11-16**: for permissions, safety first. Declaring a role gives it to
the file, as Argo CD's RBAC policy owns its roles; given back to the UI, a
role no one meant to keep would stay. Teams, quiet windows and reports keep
being given back. A role whose team the file misspells keeps none of the
person's roles as they were: one of every team the file gave before is
taken away with the rest.

**Without `--config`**, there is no file to say a role is gone:
everything a file declared before is the UI's again, roles included, none
taken away and nothing deleted, whatever the UI holds. Each is marked
given back (`given_back`): until the UI changes one, the next file that
doesn't declare it again lets it go as one it no longer declares, a role
taken away, a team, a quiet window or a report the UI's for good, or
deleted with prune; one it declares again is declared again, of the
team it says, in its own row (a quiet window or a report moved to another
team isn't made again beside it, nor a report's last send lost). The
Settings page lists them to an admin, and says how many to anyone else.
**The mark doesn't expire**: a file put back months on still lets go of
what it doesn't declare, roles taken away all together. To keep one as
the UI's, change it in the UI, or keep it on the Settings page (an
admin's: one, or all of them, each noted in the activity log); a role
can't be changed, so delete it and give it again, or keep it there. A
chart rolled back to before `serve.config` takes no one's role away, and
rolled forward again, the file takes away what it no longer gives.

A declared team something still names (a quiet window, a
report, a role, a task, a rule's note, the UI's or declared) isn't
deleted, but given back to the UI, the activity log saying why: deleted,
its windows would quiet nothing and its roles go, and the file put back
would make a new team, which they wouldn't name.

**Renaming a team.** By its name alone, a team renamed in the file is
another: the old one is given back to the UI (or, with prune, deleted
unless something names it), and what named the old one doesn't follow.
To rename one in place, keeping what names it, give the new `name` and the
old as `renamed_from` in one change, with every `team:` in the file that
named it; once it is renamed, `renamed_from` can be left out. A rename in
letter case alone needs neither.

**When.** The leader reads the file as each term it leads begins,
forgetting what an earlier term kept the ledger to (another leader with an
older copy may have since, or a failover lost a write), and again every
15 seconds, comparing its SHA-256 with the one it last kept the ledger to,
and keeps the ledger to it on the second read alike (above). The term's
first read comes before the rules are read, and the second 15 seconds
after it has read the rules: usually within half a minute of the term's
start, but with no bound. Kubernetes updates a mounted ConfigMap by swapping a link to
a new directory, which a watch on the file misses, so the content is
hashed instead. The chart mounts the directory, not a `subPath`, which the
kubelet doesn't update. No restart is needed, and none is made. A file with
things that couldn't be kept is tried again as the ledger changes (a team
it names made in the UI, or one the write itself made once others were let
go), 15 seconds after, then twice as long each time, ten minutes at most;
one refused as a whole isn't, until it changes.

**Several replicas.** Only the leader keeps the ledger to the file, in a
write its lease fences (a leader whose lease another took commits
nothing). Each replica mounts the ConfigMap, and the kubelet updates each
copy on its own clock, so for a minute two replicas can read two
versions; were each to write, they would undo each other by turns. One
writer, and the transaction, make it safe; the write is idempotent, so a
leader that takes over after another kept the ledger to the same file
writes nothing.

**What is wrong** is logged once, and shown on the Settings page (under
"Declared configuration") and the Run history page, and the Administration page's
Settings card says so; the leader says it to the other replicas with its
status. An admin is shown why, with the line; anyone else, the line and
the kind alone ("line 23: a role that can't be kept"), as why can name who
holds a role or where a report goes: so is `/api/v1/settings`. Put back as
it was, the file the ledger is kept to shows nothing wrong again. At start,
a file that isn't there or can't be read is a flag given wrong; one that
can't be used starts `lapilli serve` all the same, on the ledger as it is,
and is said as above: a pod restarted on it serves rather than loop.

### Through Helm

`serve.config` given as a text, a block scalar, is rendered as written
(with an indentation indicator, so a first line indented more than the
rest renders), and recommended; its default is `null`, which neither form
warns over:

```yaml
serve:
  config: |
    teams:
      - {name: payments, rules: ^Checkout}
```

Given as a mapping, Helm reads it first as YAML 1.1 and writes it again:
`no`, `off`, `y` and `n` become `false` and `true`, `0123` becomes `83`,
`1e3` `1000`, `0x1F` `31`, `1.10` `1.1`, and a number past 2^53 loses its
last digits. Lapilli refuses a name, a title, a user, a group or a team
it is given as a boolean or a number, and a label's value given as a
boolean or a fraction, but can't tell `83` from `0123`. Times (`09:00`,
`23:00-07:00`) stay as written either way.

### Read-only

`declared` is a column of `teams`, `windows`, `reports` and `grants`. Every
write the ledger takes to one of those rows checks it, in the write itself
(`PutTeam`, `DeleteTeam`, `PutWindow`, `DeleteWindow`, `PutScheduled`,
`DeleteScheduled`, `DeleteGrant`), and refuses a declared one with
`ErrDeclared`: "declared in config — change it in values", 409. The forms,
the JSON API and anything else reach the ledger through them, so no path
is missed; MCP has no tool that writes them. A team a declared window,
report or role names isn't deleted, nor renamed (`ErrNamedByDeclared`):
the file would name a team that isn't there. The ledger's pruning (`--keep`)
leaves a declared window that ended: the file would make it again, and
tell of it. The pages mark what is declared with a badge, and draw each
action on it with the `off` template: the button disabled, "Declared in
config — change it in values" beside it.

### Export

`GET /config.yaml`, an admin's (it names who holds which role), gives every
team, quiet window, report and role the ledger holds, declared and made in
the UI alike, as `--config` takes them: times as RFC 3339 in `--tz`, so each
is the instant it is wherever it is read; a window's last day as a date
when it is that day's start. What a file can't name is left out and said
in a comment, each name quoted, its line breaks and characters that don't
show escaped: a title kept from before titles were one line can't end the
comment and declare a role for whoever pastes the export. A quiet window's
title is one line now, with nothing that doesn't show, as a team's and a
role's holder's names are. Teams whose names differ only in letter case
are said too. Read back as `--config`, it makes, changes and deletes
nothing; it takes over what the UI made; exported again, it is the same
file; read once more, it writes nothing.

### A team's name in any letter case

A new team's name, and a team renamed, is unique in any letter case
(Unicode case folding, `ledger.FoldName`), checked in the write, which the
ledger takes one at a time, every replica's too. Schema 019 adds
`teams.name_key`, the name with ASCII letters lowered (SQLite's `lower()`,
and Postgres's under `COLLATE "C"`, fold those alone), under a unique
index. A ledger may already hold teams whose names differ only in case
(`Payments`, `payments`): the upgrade doesn't fail on them, nor choose
one. 019 keys every team but those, whose key stays NULL, which a unique
index takes any number of. Either can be changed but for its name; the
Teams page lists them to be renamed; once one is renamed or deleted, the
other is keyed. A file can't declare both: it names which to rename.

### Proposals that expire

`--proposals-expire DURATION` (`serve.proposalsExpire`), off by default (0,
`0s`, `0d`): the leader's work closes as `expired` a proposal that waited
for approval longer than that since it began to wait (it was made, or
backtested again, its approvals withdrawn, or sent back to wait for more:
its `updated`, which nothing else moves while it waits), that no one
approved: one with an approval waits on (one
of two, or the one `--approvals`, lowered, now asks for, which carrying it
out approves). A hundred a round, each in a write that moves it only while
it still waits, noted in its history and the activity log as Lapilli's;
its rule is free for another, and Lapilli doesn't propose the same again
by itself. They are closed before Lapilli backtests stale proposals again,
so no backtest is spent on one about to close, and Lapilli doesn't
backtest again one past its expiry, which someone approved: backtested
again, its approvals withdrawn, it would wait anew; one being backtested again
on the leader is left until it is done, and one backtested on another
replica, closed meanwhile, has its backtest refused as moved. A proposal's
page says when it would expire, while no one approved it.

### A team's boundaries of change

As built in 0.64.0 ([changes.md](changes.md), "A team
as the boundary of change"). A team declared in `--config` may say who of
it approves its rules' changes, and where they are written; a file may
declare change freezes:

```yaml
teams:
  - name: payments
    labels: {team: payments}
    approvers: [user:ana@example.com, group:payments-leads]
    changes_in: [payments-*]      # within --changes-in; [] for nowhere
freezes:
  - title: Black Friday
    start: 2026-11-27T00:00       # in --tz, or with an offset
    end: 2026-11-30T00:00
  - title: Payments month end
    team: payments                # everyone's when left out
    start: 2026-11-30T18:00
    end: 2026-12-01T06:00
    every: month                  # as a quiet window: day, week, month or cron
    repeat_on: last
    until: 2027-06-30
```

- **Read strictly, as the rest of the file.** An approver is
  `user:NAME`, `group:NAME` or `*@DOMAIN`, not a bare name or `*`;
  `approvers: []` is refused (leave it out for none); a `changes_in` entry
  is one `--changes-in` would take, within what `--changes-in` names (in
  the letter case its own source compares names in), or the file is
  refused as a whole, its line said; a freeze has a title, times a quiet
  window could have (31 days at most, a repeat shorter than its period, a
  month's or a cron's starting when it falls due), a team the same file
  declares, 100 freezes at most. A team's name is read as the ledger keeps
  it, without the spaces around it; two teams of one name in another
  letter case, or with spaces around it, are refused where the boundaries
  name them. A team that can't be kept, kept as it was, keeps the
  boundaries the file gives under its name; one no team has holds nothing,
  and the Settings page names it.
- **A rule's team is an admin's to move.** While the file declares any
  team's approvers or `changes_in`, or a freeze, a note that moves a rule
  into a team or out of one (its form, `PUT /api/v1/notes`, the rules
  list's "Set the team", the Cleanup page's move) is refused to anyone but
  an admin, in the ledger's write; and a rule a note moved is held by the
  boundaries of the team its name and labels give it too.
- **Kept with the rest, read twice first.** Read, they are written as one
  value in the ledger's `meta` table (`change_bounds`), in the write that
  keeps the ledger to the file, a change noted in the activity log
  ("change boundaries declared in config", by `config`): a file read half
  written gives no one an approver's role and lifts no freeze, and every
  replica reads the same. A file that says them as the ledger holds them
  writes nothing. Without `--config`, there are none: no team's approvers,
  sources or freezes, as nothing declares them; a role given in the UI is
  never an approver's.
- **Exported.** `/config.yaml` gives them back as the file takes them,
  each freeze's times as RFC 3339 in `--tz`; read back, it writes nothing.
- **Shown.** A proposal's page says its team; to an admin, who of every
  team and of its team may approve it; a freeze that holds it, and until
  when; a rule that another team owns now than when it was proposed. The
  Settings page lists the freezes, under "Changes to the rules", each
  frozen until, next, or over; the Administration page's Settings card
  says until when a freeze holds changes. Boundaries that can't be read
  are said on both pages, drawn all the same, nothing they would hold
  going ahead. Who a team's approvers are is on a proposal's page to an
  admin alone, as who holds a role is.

## Sources, the activity log, and the daily writes through the API

Status: built, in 0.64.0 (bundle 25). No schema change: what a
source's reads came to is the leader's, said to the other replicas in its
status as the impact signal's reads are; what was told of a source is kept
in `meta`, as `source.quiet` keeps its own; the activity log is the
`changes` table as it was.

### Sources

Each source `serve` reads is an object with a state, as Argo CD shows an
application: `/sources` (Administration, after Settings, as ui.md lists
it), and `GET /api/v1/sources?all=true`. A row each, by an id kept from one
release to the next:

| Id | What | Read |
|---|---|---|
| `rules` | the rules where they are kept: `--rules`, `--rules-history`, `--loaded-rules`, `--grafana` or `--ruler` | every `--watch`, counted |
| `alarms/SOURCE` | each `--alarms` | as the rules are, counted |
| `loki` | `--loki-loaded-rules` or `--loki-rules` | as the rules are, counted |
| `prometheus` | Prometheus as the runs ask it | each run that isn't held, from the runs kept, counted |
| `incidents-file` | `--incidents` | each run |
| `deploys-file`, `marked-file` | `--deploys`, `--marked` | each run, counted |
| `impact`, `signal/NAME` | the impact signal, or each named one | each run |
| `on-call/NAME` | each schedule of `--on-call` | its feed's own clock |
| `intake/PREFIX` | incidents a job copies in, by their ids' prefix | written to |
| `firings/NAME` | firings a job copies in, by source | written to |

Each has its kind, when it was last read (or written to) well, its last
error and when, how many reads in a row failed (where they are counted),
the rules the last read gave, how far behind it is where that is known
(the runs' cursor; a source's furthest period copied in) and how long its
last read took. Its state is one of the incident sources' (statusKinds'
`source/…`): working, failing (its last read failed), quiet (nothing read
or written past its bound), waiting (not read yet, the runs held by
`--guard` or a 429, or a source beside the rules held, below). The rules
and what is read beside them make one version, which a read that fails
anywhere fails whole, so the version stays as it was; each is read all the
same, its read noted, whichever of the others failed, so that one source
failing never makes another look quiet or down. One the minute the reads
get ran out on before its turn is held (`noteHeld`): not read, waiting,
and not aging, until its next read.

A source's error is said as Run history says a run's (`runsFor`): the
paths of the files `serve` was started with to an admin alone, each by its
flag to anyone else, and why the ledger's database can't be reached in a
few fixed words. On this page, the API and in a notice, what may be a
credential is `***` to everyone, an admin too: a URL's user and password,
a query's token, key, secret, password, signature or credential, and its
`code` (an Azure Function's key); such a name given as `name=value`
between spaces (a connection string's `password=`) or as a JSON string
(`"token":"…"`, escaped or not); a header such as `api-key:` or
`x-api-key:`; a bearer or basic token of 12 characters or more
(`scrubSecrets`). It goes by patterns, and takes out more than a
credential where a name only looks like one's. Where a source
is read (its path or address) is an admin's alone, scrubbed too. The page
is worked out as it is asked, never kept (what a source's reads came to is
no write a kept page would be told by), and so is the API's `all=true`;
without `all`, the answer is the sources firings are copied in from, as
before, kept as it was.

The status banner's failure, when it is a source's read (the rules, a run,
the incidents, deploys or marked file, the impact signal read again),
links to the page; so does Run history.

### Down and back

A source is down when, of those whose reads are counted, three in a row
failed (`sourceDownAfter`); of the others, when it has failed since it last
read well past its bound, or, never having read well, since this replica
first saw it failing; and when it is quiet. The bounds are the impact
signal's (`impactStale`: two runs, the hours they settle, and one more),
and for what a job writes, its own: two days for incidents through the
port (`receivedQuiet`), `--source-quiet` for firings. An incidents id
prefix nothing was written to in 14 days (`intakeForgotten`, or `--keep`
when shorter) is left out of the list, and not told down: it was an id's
one-off prefix, not a job's that stopped, and anyone with the incidents
token can make 63 of them.

The leader tells the webhook and Slack `source.down`, once, and
`source.back` once it has read well for 15 minutes since its last failure
(`sourceBackAfter`), each with `source_read` (its id, kind, name, flag,
state, last read well, reads failed in a row, and its error as anyone but
an admin is shown it). What was last told of each is kept in `meta`
(`source-told.ID`: `down`, or `up` and when), so a source stays told down
across restarts and leaders, and isn't told again while it is. What is
told is written, and kept, only while `meta` holds what the look read, in
the same write (`TellFrom`): the watch and the jobs each look, at once at
times, and tell a source back once between them. A source
told back is told down again no sooner than an hour after
(`sourceRenotify`): one that fails three reads, reads one, and fails again,
over and over, is told down once, and one that goes down every twenty
minutes once an hour at most. It is looked at after each read of the rules
and each run. Firings copied in keep `source.quiet`, as before, and are
told of by nothing else.

### The activity log

`/activity` (under Run history, which still shows the latest 100 changes,
each person's verdicts of an hour as one line, and links to it) lists every
change kept, 100 a page, the latest first, each with its kind, and its
before and after behind a `<details>` (the first 2,000 bytes; the export
has all of it). It filters by query: `who` (the name a change is noted
under, whole), `kind` (setting, incident, verdict, proposal, task, comment,
note, window, marked, report, team, role, goal, firings, cleanup, other:
told apart by how each change is noted, `ledger.ActivityKinds`), `team`
(the changes of the team itself, of its rules, a verdict's, a note's, a
comment's, a proposal's, and of its tasks), `since` and `until` (a day in
`--tz`, `until` taking it whole, or an RFC 3339 time). A page goes on from
where the last ended (`before`, its last change's time and id), so a
change made meanwhile neither repeats one nor hides one. A team's changes
are found by looking through the others: a request looks through 20,000
at most, and looks up 200 tasks and proposals at most for their team,
then says where it stopped, and the next request goes on from there; it
reads the before and after of a change it keeps alone, not of each it
looks through (but a bulk verdict's after, which says its rules).

`GET /api/v1/activity` gives the same, with the same filters: `limit` (1
to 1,000), as JSON (`activity`, each with its id, `at`, `by`, `kind`,
`what`, and `before` and `after` as the JSON they were kept as; `next`) or,
with `format=csv`, as CSV (`id, at, by, kind, what, before, after`), the
next page's link in `Link`. No cell of it starts with `=`, `+`, `-`, `@`, a
tab or a carriage return, or their full-width forms, after any spaces: such
a cell starts with `'`, which a spreadsheet shows as text (CSV injection).
Who reads Run history reads it: a viewer. The roles given and taken are an
admin's, as there (`changesSeen`), and to anyone else the paths of `serve`'s
files are their flags. Neither is kept by a cache: `Cache-Control:
no-store`. The export is worked out as a page is, a few at a time: it
takes a page's turn (`pageRenders`), 503 past the wait, and gives it back
once the answer is written out, before it goes to the network.

**Builder's decision: the activity log outlives the firings.**
`--keep-activity` (`serve.keepActivity`) is how long it is kept: unless
given, 800 days, or `--keep` when that is longer, so that an install with
a longer `--keep` starts as it did; 0 keeps it all. One given shorter than
`--keep` is refused as `serve` starts, and so is one given beside `--keep
0`, which prunes nothing, the activity log neither: refused rather than
pruned alone, which would drop who changed what before the firings it was
about.
Who changed a rule's threshold is asked long after its firings are gone,
and a change is a row of a few hundred bytes: a person writing 30
verdicts every hour of a year keeps some 260,000 rows, under 100 MB.
Prune drops `changes` at its own time (`PruneKeeping`), the rest as before.

### The daily writes through the API

What a person did each day only through a form now has a route of its own,
with the form's role, checks and limits, the ledger holding both to the
same write:

- `POST /api/v1/incidents/{id}/decide`: an impact window confirmed (as an
  incident, `title` its own unless given), dismissed (`together` dismisses
  the other named signals' windows at that time not decided yet), or
  reopened; an editor's, as the form.
- `POST /api/v1/firings/real`: the firing a report gives (`rule`,
  `labels`, `start`) written down as an incident it caught, as "This was
  real" writes it: an editor's of the last 14 days' firings, an admin's of
  any; 201 with the incident, 200 with none once one was written from it.

They take what every write on `--listen` takes: a role checked before the
body is read, a body of 1 MB, `--read-only`, and the cross-origin check
that refuses a browser's write from another site (a script's, with
neither `Sec-Fetch-Site` nor `Origin`, goes on, as every API write's
does). **Builder's decision: proposing stays off the API.** A proposal is
made on a rule's page by a person, or through MCP's `propose` by an agent
behind `--mcp-propose`, flagged as an agent's and counted apart; a script
with a person's token is an agent by another name, and the API would make
one unflagged, past that switch. Approving stays the proposal's page's
alone. The settings and the bulk forms are an admin's, or not daily, and
stay forms.

### What the review found

Read as an attacker would, once built:

- **A notice storm.** A source that flaps would tell down and back each
  minute: back waits 15 minutes of good reads, down again an hour after
  back, and down isn't told twice (`TestSourceFlapping`,
  `TestSourceDownAndBack`).
- **Credentials in an error.** A client's error can quote a URL with its
  user and password or a token in its query, which the paths' redaction
  doesn't take out: scrubbed for every reader, the notices too
  (`TestScrubSecrets`, `TestSourcesRedacted`).
- **A filter that reads the ledger for each row.** A team's changes are
  found by looking tasks and proposals up: 200 a request at most, then it
  stops and says where (`TestActivityTeamBounded`).
- **Roles in the export.** Who was given which role is an admin's on Run
  history; the page, the JSON and the CSV hold to it, and `kind=role` gives
  anyone else nothing (`TestActivityWhoReadsWhat`).
- **A formula in a name.** A name or a title is a person's: a CSV cell is
  never a formula (`TestActivityCSV`).
- **A write past its form.** The routes take the form's role, a team's
  editor refused as the form refuses them, and the ledger's checks
  (`TestAPIDecide`, `TestAPIReal`).

Read so again, once built:

- **An upgrade that wouldn't start.** `--keep-activity` was 800 days unless
  given, and refused shorter than `--keep`: a `--keep` past 800 days
  stopped `serve` starting, naming a flag no one gave. Unless given, it is
  `--keep` when longer; one given shorter is refused, and one given beside
  `--keep 0` too, as it would do nothing (`TestServeKeepActivity`).
- **One source making others look down.** The rules failing stopped what
  is read beside them being read, and the first `--alarms` failing those
  after it: five hours of broken rule files told Loki's files down, then
  back. Each is read, and noted, by itself; one the time ran out on before
  its turn is held (`TestSourceBesideAFailingOne`,
  `TestAlarmsBesideAFailingOne`).
- **An export with no turn.** `GET /api/v1/activity` took no page's turn:
  any number at once could each look through 20,000 changes, before and
  after with them. It takes one, and a team's changes are looked through
  without their before and after (`TestActivityTakesATurn`,
  `TestActivityLight`).
- **Credentials in other forms.** An Azure Function's `?code=`, a
  `password=` between spaces, a JSON `"token"`, an `x-api-key:` header
  went through as said (`TestScrubSecrets`).
- **Back told twice.** The watch and the jobs each look at the sources:
  two looks a second apart each read `down`, and each told back with its
  own time. Told only while `meta` holds what was read
  (`TestSourceBackOnce`, `TestTellFrom`).
- **One-off prefixes down for ever.** An incident id's one-off prefix,
  of the 63 the incidents token can make, was down on the page for ever:
  left out after 14 days, and never told down past it
  (`TestSourceIntakeForgotten`).
