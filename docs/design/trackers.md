# Tasks kept in step with a tracker

Status: proposed, for v0.22.0. Under [GOVERNANCE.md](../../GOVERNANCE.md)
this adds a token and what it may write, so the design comes first, as a
pull request open for a week. The companion to [work.md](work.md), about
tasks.

## Why

A task in Lapilli (tune a rule, write one, retire one, decide about an
incident) is where people say who does what by when. The work itself is
tracked where the team tracks everything else: GitHub, GitLab, Jira,
Linear. Today a task can hold a link to its issue, and the README says how
to keep the two in step: take `task.created` from the webhook, make the
issue, and put its link back with `PATCH /api/v1/tasks/{id}`.

Nobody can do the last step safely. The tasks API is on the UI's port,
where who someone is comes from the headers the sign-in proxy sets; a job
there would have to name itself with those headers, which is what the
incidents port exists to avoid. And there is no recipe for any tracker, so
each team writes its own, or doesn't.

## What it would answer

- Each task has an issue in the team's tracker, made when the task is, with
  the task's title, rule or incident, team, assignee and due date, and a
  link back to the task's page.
- A task's link is the issue's URL, so its page points at it.
- An issue closed in the tracker closes its task, and one reopened reopens
  it; a task done or dropped in Lapilli closes its issue, if the team wants
  that way too.

## The way in

A token of its own, on the port `--incidents-listen` opens, beside the
incidents and firings tokens: `--tasks-token-file`. It may:

- `GET /api/v1/tasks?linked=false&status=open,doing`: the tasks that have no
  issue yet, a page at a time;
- `GET /api/v1/tasks?linked=true&status=open,doing`: those that have one, to
  check against the tracker;
- `PATCH /api/v1/tasks/{id}` with `link` and `status` and nothing else: a
  link that is an `https` URL of 2,048 characters at most, set once (a
  task's link isn't moved from one issue to another by a job), and a status
  of `open`, `doing`, `done` or `dropped`.

It can't make, delete or retitle a task, or touch anything else. Like the
other tokens, it has its own rate and its own turn, it can't hold the same
value as another token, and what it does is noted in the changes as the
tracker job, which outcomes don't count as people's use.

## Recipes

`recipes/trackers/`, one script a tracker, each run by a job every few
minutes (a CronJob in the chart, `trackers`):

- `github.sh` (issues in one repository), `gitlab.sh` (issues in one
  project), `jira.sh` (issues in one project, an issue type given),
  `linear.sh` (issues in one team).
- Each makes the issues for the tasks without one, puts their links back,
  then reads the issues of the linked tasks and moves the tasks whose issues
  closed or reopened. `CLOSE_ISSUES=true` also closes an issue whose task
  was done or dropped.
- Like the incident recipes: `sh`, `curl` and `jq`; tokens from files,
  given on stdin, never on a command line; ids and times checked before
  they reach a request; paging bounded; a tracker's answer printed as JSON;
  nothing written when the tracker fails.
- A task's title and body reach the tracker as they are, and an issue's
  state comes back; nothing else from the tracker is written to Lapilli.

Labels on the issue (`lapilli`, the task's kind, its team) are given as the
tracker takes them, and a field map in each recipe's `.jq` can be changed.

## Security

What takes input from outside is reviewed as an attacker would
(CONTRIBUTING.md). Here:

- Someone with the tasks token can link a task to any URL once, and open or
  close tasks. They can't read anything but tasks, write anything but those
  two fields, or reach the UI.
- The link is shown on the task's page as a link with `rel="noopener
  noreferrer"`; only `https` URLs are taken, so no `javascript:`.
- A recipe holds the tracker's token and Lapilli's tasks token; neither
  goes on a command line, and a recipe never follows a URL the tracker or
  Lapilli gives it except the issue's own, built from ids it checked.

## Alternatives

- **A tracker built in,** one of them. Against "integrations are generic":
  a recipe for each, alike, and the webhook for the rest.
- **The webhook pushing straight to the tracker,** with no job. A tracker
  takes a webhook only in its own shape, and closing the loop needs the
  link back anyway.
- **The UI's API with a service account** behind the sign-in proxy. It would
  give a job everything an editor can do.

## Questions for review

- Should a task done in Lapilli close its issue by default, or only when
  `CLOSE_ISSUES` says so?
- Should comments be kept in step too? They are people's words, and would
  need the same care as titles, both ways.
