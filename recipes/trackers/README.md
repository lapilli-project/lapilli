# Tasks kept in step with a tracker

A task in Lapilli (tune a rule, write one, retire one, decide about an
incident) says who does what by when; the work itself is tracked where the
team tracks the rest. These scripts keep the two in step, one for each of
GitHub, GitLab, Jira and Linear, alike. Run one every few minutes. Each run:

1. makes an issue for each open task without one (`open` or `doing`), with
   the task's title, text, rule or incident, team, assignee, due date and a
   link back to its page in Lapilli (with `--external-url`), and puts the
   issue's URL back on the task, as its link;
2. reads the issue of each task that has one, and moves the task as the
   issue was moved:
   - an issue closed since the task last changed closes the task: `done`,
     or `dropped` when the issue was closed as not to be done (GitHub's
     "not planned", Jira's "Won't Do" and the like, Linear's canceled;
     GitLab closes one way, so `done`);
   - a task closed because its issue was is opened again when the issue is;
   - with `CLOSE_ISSUES=true`, an issue whose task someone closed in
     Lapilli is closed too, unless the issue changed after the task did:
     then someone is still at it, and it is left open.

Anything else is left as it is: a task reopened in Lapilli doesn't reopen
its issue, and a task whose link is another tracker's isn't read. Only an
issue the script's own account made for that task is followed or closed:
one an editor linked by hand to someone else's issue, or to the job's issue
for another task, is left alone (and the run says so).

They need `sh`, `curl` and `jq`. Lapilli's side is the port
`--incidents-listen` opens, with `--tasks-token-file`, a token of its own:

```sh
lapilli serve ... --incidents-listen :8082 --tasks-token-file tasks-token
GITHUB_REPO=example/alerts GITHUB_TOKEN_FILE=gh-token \
  LAPILLI_TOKEN_FILE=tasks-token recipes/trackers/github.sh http://lapilli:8082
```

| Script      | Settings                                                                          | The tracker's token may          |
|-------------|-----------------------------------------------------------------------------------|----------------------------------|
| `github.sh` | `GITHUB_REPO`, `GITHUB_TOKEN_FILE`; `GITHUB_API`, `GITHUB_WEB` for Enterprise      | read and write the repo's issues |
| `gitlab.sh` | `GITLAB_PROJECT` (its path), `GITLAB_TOKEN_FILE`; `GITLAB_URL` when self-managed   | `api`                            |
| `jira.sh`   | `JIRA_URL`, `JIRA_EMAIL`, `JIRA_TOKEN_FILE`, `JIRA_PROJECT`; `JIRA_ISSUE_TYPE` (Task) | browse, create, transition       |
| `linear.sh` | `LINEAR_TEAM` (its id), `LINEAR_TOKEN_FILE`                                        | read and write the team's issues |

And for each: `LAPILLI_URL` (or the first argument), `LAPILLI_TOKEN_FILE`,
`CLOSE_ISSUES` (`false`), `MAX_ISSUES` made in a run (50; the rest wait for
the next), `MAX_PAGES` of 200 tasks (20) and `SYNC_DAYS`, how long a task
done or dropped is still checked against its issue (30).

What an issue is made with is in the script's `.jq`: the title, the labels
(`lapilli`, the task's kind, its team, where the tracker takes labels by
name; Linear takes them by id, so add `labelIds` there), the issue type.
Change it there. The text ends with `Kept in step with Lapilli:
lapilli-task-ID` (Jira: the label `lapilli-task-ID`): that is how an issue
made by a run that couldn't put its link back is found again, rather than
made twice, so leave it in, and the label `lapilli` too where the tracker
takes labels by name. An issue is found again only among those the
tracker's token's own account made (and labelled `lapilli`): anyone may
write `lapilli-task-ID` in an issue of their own, and it isn't taken for
the task's. GitHub's search can take a minute to find a new issue; a run
that failed between making one and linking it is put right by a later run.

## What the token can do

The tasks token reads tasks, a page at a time
(`GET /api/v1/tasks?linked=false|true&status=open,doing,done,dropped&updated_after=TIME&after=ID&limit=N`,
each with `status_by` and `status_at`, who set its status and when), and
sets a task's `link` and `status` (`PATCH /api/v1/tasks/{id}`), and
nothing else: it can't make, delete or retitle a task, read anything but
tasks, or reach the UI. A link is an `https` URL of 2,048 characters at most
with no user name in it, set once: a job doesn't move a task from one issue
to another, and the same link again changes nothing. What it does is noted
in the changes as `tasks-token`, isn't counted as anyone's use of Lapilli,
and isn't told to the notice webhook: the tracker has told people, and
whoever holds the token can't fill the outbox. It can't hold
the same value as the incidents, firings or notifications token; Lapilli
refuses to start, and refuses each request, when it does. It has its own
rate (5 a second, bursts of 100) and its own turn.

## What the scripts trust

- Tokens are read from files and given to `curl` on stdin, never on a
  command line.
- Settings are checked before anything is asked: the repository, project
  or team is an id of the shape the tracker gives, numbers are numbers.
- What Lapilli answers is checked before it reaches a request: a task's id
  is a number, a page moves on, and no more than `MAX_PAGES` are read.
- An issue is the task's only when the token's own account made it, with
  the task's marker (and in Linear, in the team): one someone else wrote,
  or linked by hand, is neither taken, followed nor closed. The account is
  asked of the tracker once a run (GitHub's and GitLab's `/user`, Jira's
  `/myself`, Linear's `viewer`).
- A URL from the tracker is followed only as the issue's own: the
  reference (a number, a key) is taken out of a link that starts with the
  tracker's own address and checked, and the request is built from the
  settings and that reference. A link that isn't one of the tracker's
  issues is never written to Lapilli, and stops the run.
- When the tracker or Lapilli fails, the run stops there, says so, and exits
  non-zero, having written nothing for that task.

Each is tested against a stand-in for the tracker's documented API and
Lapilli's own port: `go test ./internal/serve -run TestTrackerRecipe`.

## In the chart

```yaml
serve:
  trackers:
    tokenSecret: {name: lapilli-tasks, key: token}
    tracker: github
    trackerTokenSecret: {name: github-issues-token, key: token}
    env: {GITHUB_REPO: example/alerts}
    image: YOUR-IMAGE-WITH-SH-CURL-AND-JQ@sha256:...
```

runs the script as a CronJob every five minutes (`schedule`), with the
NetworkPolicy letting it reach the incidents port alone. `tokenSecret`
alone, without `tracker`, opens the port for a job of your own, named in
`serve.incidentsAPI.from`.

## Another tracker

Write `t_me`, `t_find`, `t_create`, `t_ref`, `t_state` and `t_close` as
`lib.sh` says, set `name`, and call `sync_tasks`. Please send it here.
