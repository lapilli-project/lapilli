# Reports people schedule themselves

Status: built, for v0.22.0 (see "As built" at the end). Under
[GOVERNANCE.md](../../GOVERNANCE.md) this is a new kind of data in the
ledger, so the design came first, as a pull request (#40); it was merged on
2026-11-11, before its week was out, by the maintainer's decision, with no
comments. The companion to [work.md](work.md), whose
"The digest" and "Notices" it extends, and to [content.md](content.md),
whose findings a report can hold.

## Why

`lapilli serve` judges every hour, but tells people once a week: the digest,
at `--digest DAY@HH:MM`, to `--slack-webhook-file`, the webhook, and each
team's `--team-digest`. Everything else waits on someone opening a page.

That fits a team that reviews alerts on Mondays. It doesn't fit the others:

- An on-call team wants yesterday each morning: what woke people for
  nothing last night, which incident nothing caught, which rule turned noisy.
- A lead wants a month, for a review or a report upward.
- A team in another zone wants its report at its own nine o'clock.
- Whoever runs `serve` sets the digest with a flag and a restart; the people
  who read it can't change when it comes or what it says.

This note proposes reports that people schedule themselves, in the UI and
the API: when, over what span, for whom, where to, and what each holds. The
weekly digest becomes one of them.

## What it would answer

- What happened yesterday, last week or last month, for everyone or for one
  team, without opening a page.
- What changed since the last report: rules that turned noisy, incidents
  nothing caught, firings that woke people for nothing, a replay that
  started to differ, a source gone quiet, tasks come due.
- The same numbers as JSON, for a script or another tool.

## A report

A report is a row people keep, as they keep quiet windows and tasks:

- **A name,** such as "payments daily".
- **When** it is sent:
  - a preset: `daily@09:00`, `weekly mon@09:00`, or `monthly 1@09:00` (a day
    of the month from 1 to 28, or `last`);
  - or a cron expression of five fields, for anything else
    (`0 9 * * 1-5`, weekdays at nine).

  Times are in the ledger's zone (`--tz`), or the team's when the report is
  a team's and its team has one. They are counted on the calendar, as a
  repeating quiet window is: a time daylight saving skips is sent at the
  first instant after it, and a time it repeats, once, the first time.
- **The span** it covers:
  - for a preset, the calendar span before it: the day before, the week
    before (Monday to Monday, as the digest has it), or the month before;
  - for a cron expression, the time since the one before, or a fixed span
    given with it (`24h`, `7d`).
- **For whom:** everyone, or one team. A team's report holds its rules, its
  incidents and its tasks, as a team's digest does today, counted by its
  hours.
- **Where to:** one or more named destinations (below).
- **What it holds:** the sections people pick, in this order:

| Section | What it says |
|---|---|
| `summary` | firings, caught and caught nothing, precision, beside the span before |
| `noise` | the noisiest rules, and rules newly noisy since the span before |
| `missed` | incidents that started in the span with no alert, and a near miss for each when one was found |
| `woken` | firings, or notifications once they are taken, that came while people slept and caught nothing, by rule |
| `notifications` | what the notifications sent came to: repeats, while people slept, for nothing |
| `replay` | the replay beside what really fired, and the rules that differ ([fidelity.md](fidelity.md)) |
| `sources` | firings copied in, by source, and sources gone quiet |
| `tasks` | tasks due or overdue, and done in the span |
| `content` | what the content check finds, new since the report before ([content.md](content.md)) |
| `suggestions` | longer `for`s for noisy rules, and spare rules (weekly or longer spans only) |

A report with none picked holds `summary`, `noise`, `missed` and `woken`.

**A daily report** is the preset with these, over yesterday. "Newly noisy"
over a day compares the seven days to yesterday's end with the seven days
before them, so one quiet Sunday doesn't make a rule noisy or not.

## Destinations

A destination's URL is a secret, and secrets are read from files and never
shown (GOVERNANCE.md, "Safe by default"). So the UI never takes a URL.
Whoever runs `serve` names the destinations, and people pick among the
names:

- `--report-dest NAME=FILE`, repeatable: a Slack-style incoming webhook
  whose URL is in the file. The chart's `serve.reports.destinations` mounts
  each from a Secret.
- `webhook`: `--notify-webhook-file`, as JSON (`report.ready`, below).
- `slack`: `--slack-webhook-file`.
- `team:NAME`: each `--team-digest NAME=FILE`.

A Slack-style incoming webhook is what Slack, Mattermost and Rocket.Chat
take, and what others take through their own automations, so this is no one
chat's feature.

## Sending

Through the outbox, as every notice is: written in the transaction that
decides the report is due, one row for each destination, sent by the loop
for that destination, tried again as notices are (about eight hours, ten
times). A destination down holds up nothing else.

Once for each report and span: the outbox's `TellOnce`, keyed on the report
and its span's end. A report edited after it was sent for a span isn't sent
again for it.

**When the ledger isn't caught up.** A report waits until the ledger has the
span: the runs up to the span's end (or, with no runs, every live source,
as the digest does), and, for `replay`, the check up to it. A report still
waiting six hours after it was due is sent with what there is, and says to
when the ledger had firings.

**After a restart,** a report due while `serve` was down is sent once, for
the last span due, if that span ended within the span's own length: a
daily report missed for three days sends yesterday's, not three.

**What it reads.** A report reads the ledger: what the runs judged, the near
misses already looked for, the check already made. It replays nothing,
except `suggestions`, which the weekly digest already works out and keeps;
a report shows the latest kept. So a report costs a read of its span, as a
page does.

**Text and JSON.** The text is the digest's: a report's lines in `--lang`,
escaped as the digest is for Slack. The webhook gets `report.ready`: the
report's name, team, span, sections, the numbers of each section as JSON,
and the text, as `digest.ready` does today. Every field that names a thing
is as written, and the README says to escape it, as it says of
`digest.ready`.

## The weekly digest

`--digest`, `--team-digest` and `--slack-webhook-file` keep working, as a
report the flags define: weekly at `--digest`, to `slack`, the webhook and
each `team:NAME`, with today's sections. It is shown with the others, marked
as set by flags, and changed by changing them; `digest.ready` stays its
event, so nothing that takes it today breaks. A ledger with no report of
its own and no `--digest` sends nothing, as now.

## In the ledger

Schema 012, one table:

`reports`: `id`, `name`, `team_id` (none for everyone), `schedule` (the
preset or cron text, as given), `span` (empty for the preset's, or a
duration), `zone` (`ledger` or `team`), `sections` and `dests` (JSON
lists), `enabled`, and who made and last changed it, and when.

When each was last sent, and for which span, is kept as `TellOnce` keeps
it, in `meta` (`report.ID`). A change to a report is a row of `changes`, as
every write is.

At most 50 reports, and none more often than hourly: a cron expression that
fires more often is refused. A report whose team is removed stops, and
the runs page says why.

## Roles and the UI

- **Admins** make, change and remove reports: a report sends the ledger's
  numbers somewhere, as the settings do.
- **Everyone** sees the reports, when each was last sent, and a preview: the
  report as it would be sent now, for its last span, sent nowhere.
- `--read-only` refuses changes, as it does all.

Settings gain a "Reports" page. The API is `GET/POST /api/v1/reports`,
`GET/PUT/DELETE /api/v1/reports/{id}` and `GET
/api/v1/reports/{id}/preview`. MCP's tools don't change reports.

## Options

- `--report-dest NAME=FILE`, repeatable.
- `--digest`, `--team-digest`: as above.
- The chart's `serve.reports.destinations`, a list of a name and a Secret.

## Alternatives

- **More flags,** `--daily-digest` and so on. Each change needs whoever runs
  `serve`, and a restart; the people who read a report can't make one.
- **The chart's CronJob running `lapilli verdict` daily.** It works today,
  and stays: but it replays the day again from Prometheus, and has none of
  the ledger's teams, tasks, notifications or check.
- **Letting the UI take a webhook URL.** A URL typed in a page is a secret
  shown again on every edit and kept in the ledger's backups. Named
  destinations keep secrets in files.
- **Sending without the outbox.** A destination down would lose the
  report, or hold up the jobs.

## Questions for review

- Should editors make reports for their own team? Roles aren't kept by team
  yet (ROADMAP, "Roles kept in the ledger").
- A team's report in the team's zone, or always the ledger's?
- Is six hours the right wait for a ledger behind, before sending what there
  is?
- Should a report with nothing to say (no firings, no incidents, no tasks)
  be sent, saying so, or not sent? The digest sends nothing to a team
  nothing happened to.

## As built

What was built differs from the above, or settles it, here:

- **The weekly digest** stays its own job, as it was: `digest.ready`, the
  Slack post and the team digests are unchanged. It isn't a row of
  `reports`; the Reports page and `GET /api/v1/reports` show it first, as
  the report the flags define (`"set_by": "flags"`, with no id), with when it
  was last sent.
- **Spans** are written as durations or days (`90m`, `24h`, `7d`): whole
  minutes, an hour at least, 92 days at most, and only with a cron
  expression.
- **Schedules** also refuse a cron expression that doesn't fall due every
  year (the 29th to 31st of February alone), so the last time it fell due is
  always found within a year and a day. Its text is 100 characters at most,
  a field 60 parts at most.
- **When it is sent.** A report isn't sent for a time it fell due before it
  was made or last changed, so changing a report can't send it more often
  than its schedule. It waits for the ledger six hours, or half its span
  when that is shorter: an hourly report waits half an hour, and isn't
  overtaken by the next. After a restart, only the last span due is sent,
  and only if it ended within its own length.
- **Destinations.** `webhook` is `report.ready` through the webhook's own
  outbox loop. The others share one place in the outbox (`report`), each
  notice its own receiver, as the team digests are: one that fails is tried
  again and holds up no other. A name a report picks must be one `serve`
  names when the report is saved; one that is gone later is skipped, and the
  pages say so.
- **A report whose team is removed** stops; the Reports page, the report's
  page and the runs page say why, and its preview is refused.
- **Zones.** `zone` is `ledger` or `team`; `team` uses the team's zone when
  it has one, the ledger's otherwise.
- **Sections.** `noise` over a week or longer compares with the span
  before, as `summary` does; over less, the seven days to the span's end with
  the seven before them. `missed` takes the near misses kept for the weeks
  the span overlaps. `woken` counts notifications once they were taken for
  the whole span, firings otherwise, and says so when no one's hours say
  when people sleep. `replay` is read by the ledger's days, as the check
  keeps it. `content` is the content check's ([content.md](content.md)).
- **A report with nothing to say** is still sent, saying so: one scheduled
  is expected, and silence can't be told from a report that failed.
- **The questions for review.** Only admins write reports (editors don't
  for their own team yet); a team's report is in the ledger's zone or the
  team's, as it says; the wait is six hours.
- **`/metrics`** has no series of its own for reports: what is sent is
  counted as notices are, `lapilli_serve_notices_total{to="report"}`.
