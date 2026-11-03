# What people keep in the ledger

Status: built, for v0.9.0. The companion to [serve.md](serve.md), which is
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
| **Quiet windows** | a time, a rule pattern, labels: a deploy, maintenance, a load test | firings in it that caught nothing are excused, not black |
| **Tasks** | a follow-up on a rule or an incident: kind, team, assignee, due date, status | the overview's suggestions become tasks with a press, and show they are being seen to |
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
each week a write of its own, so a long window doesn't hold other writes up,
and a span isn't lost if `serve` stops halfway: the next write, or the
worker, carries on. A window lasts 31 days at most. A window kept ahead of
time excuses firings as the runs record them.

## Ownership

A rule's note can name its team. Otherwise the first team, by name, whose
pattern matches the rule's name, or whose labels the rule has (as the latest
version of the rules gives them), owns it. A team owns an incident it has a
label value of, or that one of its rules caught. Owners are worked out once
per change to the ledger and kept until the next.

## Writes

Every write goes through the same doors as incidents do now: a same-site
POST from a page, or the API (`POST`, `PUT`, `DELETE`); `--read-only`
refuses them all; each is noted with who made it (`--user-header`) in the
changes log. A comment is changed only by who wrote it, unless it was
written anonymously. Fields are limited in size; a runbook is an http or
https URL; a rule pattern is a Go regular expression of 200 bytes at most
(linear time, whatever it is). After a form, the page goes back only to a
page on the same server.

| | Page | API |
|---|---|---|
| Teams | `/teams` | `GET POST /api/v1/teams`, `PUT DELETE /api/v1/teams/{id}` |
| Notes | on `/rules/NAME` | `GET PUT /api/v1/notes` |
| Quiet windows | `/windows`, `/windows/{id}` | `GET POST /api/v1/windows`, `PUT DELETE /api/v1/windows/{id}` |
| Tasks | `/tasks`, `/tasks/{id}` | `GET POST /api/v1/tasks?status=&team=&rule=&incident=`, `GET PUT DELETE /api/v1/tasks/{id}` |
| Comments | on rules, incidents and tasks | `GET /api/v1/comments?on=&key=`, `POST`, `PUT DELETE /api/v1/comments/{id}` |

## Keeping it fast

The rules in [serve.md](serve.md) hold. These tables are small beside the
firings: lists are by index and limited (500 tasks at most a page), and a
quiet window is judged again over its own span only. Measured over 200,000
firings, with 5 teams, 20 quiet windows and 300 tasks: keeping a window and
judging its span again takes 14 ms; the dashboard for one team 9 ms; the
tasks 2 ms; each page of these 1 ms or less. The scale test holds them to
its budgets.

## Not in this

Roles or permissions beyond `--read-only` and comment authorship;
notifications about tasks; recurring quiet windows; importing tasks from a
tracker. Each would be a piece of its own.
