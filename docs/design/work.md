# What people keep in the ledger

Status: built, for v0.9.0; repeating windows, roles, notices and tracker
links for v0.10.0. The companion to [serve.md](serve.md), which is
about what the runs find.

## Why

A dashboard that only shows is half a tool. The verdict says a rule is
noisy, an incident went uncaught, the noise came during a deploy; what is
done about it, who does it, and why, ended up somewhere else, or nowhere.
So `lapilli serve` keeps those too, in the same ledger, and lets people
make, change and take them away from the UI and the API:

| | A row is | What it changes |
|---|---|---|
| **Teams** | a name; the rules it owns, by a name pattern or labels; the incidents it owns, by labels | the overview, the rules and the tasks shown for one team |
| **Notes on rules** | a rule's team (when not the patterns'), what is to become of it (keep, tune, retire), its runbook, a note | shown on the rule and in the rules list |
| **Quiet windows** | a time, once or every day or week, a rule pattern, labels: a deploy, maintenance, a load test, a nightly batch | firings in it that caught nothing are excused, not black |
| **Tasks** | a follow-up on a rule or an incident: kind, team, assignee, due date, status, a link to its issue in a tracker | the overview's suggestions become tasks with a press, and show they are being seen to |
| **Comments** | something said about a rule, an incident or a task | what was decided, and why, stays beside it |

## One ledger, its own tables

They live in the ledger's SQLite file, in tables of their own (schema
`003.sql`), not in a second database: one file to back up, and a quiet
window can change the stones in the same transaction that keeps it.

## Quiet windows and the stones

A firing is excused when it starts inside a quiet window, its rule's name
matches the window's pattern (any rule, when there is none), it has each
label the window names with one of its values, and it caught nothing. A
firing that caught an incident counts whatever window it is in: noise can be
expected, a catch can't be.

An excused firing is neither white nor black. The daily and weekly tallies
count it apart (`excused`), so precision, noisy rules and the dashboard read
only the firings judged; the report's `firings` leave it out, and each rule
says how many it had excused.

Keeping, changing or deleting a window notes its span, before and after, as
pending in the same write; the span is then judged again a week at a time,
each week a write of its own, so a long window doesn't hold other writes up.
That goes on whether or not whoever kept the window waits for it, and a
span isn't lost if `serve` stops halfway: the worker carries on whatever is
pending each time it wakes. A window lasts 31 days at most. A window kept
ahead of time excuses firings as the runs record them.

A window can repeat every day or every week, until a day or for good. Each
repeat starts at the same local time in `--tz`, counted on the calendar,
not in hours, so a nightly window stays at 02:00 across a change to or from
daylight saving; it is shorter than how often it repeats. A firing is in a
repeating window when it starts in the repeat it falls in, or the one
before; that is two checks, whatever the number of repeats. Keeping or
changing a repeating window judges again from its first repeat to now, or
to its last repeat's end, a week at a time as above; repeats to come excuse
firings as the runs record them.

## Ownership

A rule's note can name its team. Otherwise the first team, by name, whose
pattern matches the rule's name, or whose labels the rule has (as the latest
version of the rules gives them), owns it. A team owns an incident it has a
label value of, or that one of its rules caught. Owners are worked out once
per change to the ledger and kept until the next.

## Roles

Without `--editors` or `--admins`, anyone who reaches `serve` may change
anything, as before (and `--read-only` still refuses everything). With
either, whoever the sign-in proxy names in `--user-header`, or lists a
group of in `--groups-header`, gets the role the lists give them, and the
rest may only look:

| Role | May |
|---|---|
| viewer | look |
| editor | write incidents, tasks, comments, quiet windows and notes on rules |
| admin | also change the settings and the teams |

`*` in a list is everyone the proxy names; a request it doesn't name (no
header) is a viewer, whatever the lists say. Roles need `--user-header`:
who is who comes from the proxy, which must strip the headers from callers.
Pages offer only what the reader may do, and are kept in the cache apart
for each reader and their groups; `GET /api/v1/me` says who the reader is
and their role. A refusal is a 403 in the reader's language.

## Notices

What happens is told: a task made (`task.created`), assigned
(`task.assigned`), its status changed (`task.status`), or overdue
(`task.overdue`, once for each due day, checked each hour); an incident
written down (`incident.written`); a quiet window kept (`window.kept`); a
comment (`comment.written`). To a webhook, with `--notify-webhook-file`, as
JSON: the event, when, who, the row as the API gives it, and `url`, its page
under `--external-url`. To Slack, with `--notify-slack`, as a line in
`--lang`, with the link; `<`, `>` and `&` are escaped, so nothing in it can
mention anyone.

Each notice is written to an outbox in the same transaction as what it
tells of, one row for each place it goes, and sent after, by a loop of its
own every 5 seconds: a receiver that is slow or down holds up no write, and
a write that is rolled back tells nothing. One that fails is tried again a
minute later, then two, four, and so on, ten times in all (about eight
hours); then it is kept, unsent, until it is pruned with the rest after
`--keep`. Sending changes nothing a page shows, so it doesn't empty the
cache. `/metrics` gives `lapilli_serve_notices_total{to,result}` and
`lapilli_serve_notices_unsent`. The webhook's and Slack's URLs are read
from files, never shown, and no redirect is followed.

## A tracker

A task keeps a link to its issue (an http or https URL). Rather than speak
each tracker's API, `serve` speaks webhooks: a tracker's automation, or a
small script, makes the issue from `task.created` and puts its URL back with
`PUT /api/v1/tasks/{id}` (`link`); a `task.status` to `done` can close it.
The same works with any tracker, chat or cloud.

## The digest

The weekly Slack digest ends with how the tasks stand: open, those of them
overdue, and done in the week.

## Writes

Every write goes through the same doors as incidents do now: a same-site
POST from a page, or the API (`POST`, `PUT`, `DELETE`); `--read-only`
refuses them all; each is noted with who made it (`--user-header`) in the
changes log. A comment is changed only by who wrote it, unless it was
written anonymously; a page that offers to change one is kept apart, in
the cache, for each reader. Fields are limited in characters, not bytes, so
Korean fits as English does; a runbook is an http or https URL; a rule
pattern is a Go regular expression of 200 bytes at most, refused when it
compiles to a long program (Go's matching is linear in the text, but not in
the pattern). A refusal is shown in the reader's language. After a form,
the page goes back only to a page on the same server.

| | Page | API |
|---|---|---|
| Teams | `/teams` | `GET POST /api/v1/teams`, `PUT DELETE /api/v1/teams/{id}` |
| Notes | on `/rules/NAME` | `GET PUT /api/v1/notes` |
| Quiet windows | `/windows`, `/windows/{id}` | `GET POST /api/v1/windows`, `PUT DELETE /api/v1/windows/{id}` |
| Tasks | `/tasks`, `/tasks/{id}` | `GET POST /api/v1/tasks?status=&team=&rule=&incident=`, `GET PUT DELETE /api/v1/tasks/{id}` |
| Comments | on rules, incidents and tasks | `GET /api/v1/comments?on=&key=`, `POST`, `PUT DELETE /api/v1/comments/{id}` |
| Who am I | the page's footer | `GET /api/v1/me` |

## Keeping it fast

The rules in [serve.md](serve.md) hold. These tables are small beside the
firings: lists are by index and limited (500 tasks at most a page), and a
quiet window is judged again over its own span only. Measured over 200,000
firings, with 5 teams, 20 quiet windows and 300 tasks: keeping a window and
judging its span again takes 14 ms; the dashboard for one team 9 ms; the
tasks 2 ms; each page of these 1 ms or less. The scale test holds them to
its budgets.

## Not in this

Roles kept in the ledger rather than given by the proxy; a team's own
editors; windows that repeat by month or by cron; a tracker's API spoken
directly, or issues imported from one. Each would be a piece of its own.
