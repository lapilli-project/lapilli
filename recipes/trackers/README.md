# Tasks kept in step with a tracker

A task in Lapilli (tune a rule, write one, retire one, decide about an
incident) says who does what by when; the work itself is tracked where the
team tracks the rest. These scripts keep the two in step, one for each of
GitHub, GitLab, Jira, Linear, Azure Boards and Shortcut, alike. Run one
every few minutes. Each run:

1. makes an issue for each open task without one (`open` or `doing`), with
   the task's title, text, rule or incident, team, assignee, due date and a
   link back to its page in Lapilli (with `--external-url`), and puts the
   issue's URL back on the task, as its link;
2. reads the issue of each task that has one, and moves the task as the
   issue was moved:
   - an issue closed since the task's status was set closes the task:
     `done`, or `dropped` when the issue was closed as not to be done
     (GitHub's "not planned", Jira's "Won't Do" and the like, Linear's
     canceled; GitLab closes one way, so `done`);
   - a task the job closed because its issue was is opened again when the
     issue is;
   - with `CLOSE_ISSUES=true`, an issue whose task someone closed in
     Lapilli is closed too, unless the issue changed after the task was
     closed: then someone is still at it, and it is left open.

   Who set a task's status, and when, is what Lapilli says
   (`status_by`, `status_at`), not who last edited the task: reassigning
   a task the job closed doesn't make its close a person's. Once the
   change that set it is pruned, Lapilli gives neither, and the task is
   taken as closed by someone other than the job: it isn't reopened with
   its issue.

Anything else is left as it is: a task reopened in Lapilli doesn't reopen
its issue, and a task whose link is another tracker's isn't read. Only an
issue the script's own account made for that task is followed or closed:
one an editor linked by hand to someone else's issue, or to the job's issue
for another task, is left alone (and the run says so). So is a task whose
issue was deleted in the tracker: the run says so each time, and doesn't
fail for it; clear the task's link in Lapilli to have a new issue made.

They need `sh`, `curl` and `jq`. Lapilli's side is the port
`--incidents-listen` opens, with `--tasks-token-file`, a token of its own:

```sh
lapilli serve ... --incidents-listen :8082 --tasks-token-file tasks-token
GITHUB_REPO=example/alerts GITHUB_TOKEN_FILE=gh-token \
  LAPILLI_TOKEN_FILE=tasks-token recipes/trackers/github.sh http://lapilli:8082
```

| Script      | Settings                                                                          | The tracker's token may          |
|-------------|-----------------------------------------------------------------------------------|----------------------------------|
| `github.sh` | `GITHUB_REPO`, `GITHUB_TOKEN_FILE`; `GITHUB_API`, `GITHUB_WEB` for Enterprise; `GITHUB_LOGIN` | read and write the repo's issues |
| `gitlab.sh` | `GITLAB_PROJECT` (its path), `GITLAB_TOKEN_FILE`; `GITLAB_URL` when self-managed   | `api`                            |
| `jira.sh`   | `JIRA_URL`, `JIRA_EMAIL`, `JIRA_TOKEN_FILE`, `JIRA_PROJECT`; `JIRA_ISSUE_TYPE` (Task) | browse, create, transition       |
| `linear.sh` | `LINEAR_TEAM` (its id), `LINEAR_TOKEN_FILE`                                        | read and write the team's issues |
| `azure-boards.sh` | `AZURE_DEVOPS_ORG`, `AZURE_DEVOPS_PROJECT`, `AZURE_DEVOPS_TOKEN_FILE`; `AZURE_DEVOPS_URL` for Azure DevOps Server; `AZURE_BOARDS_TYPE` (Task), `AZURE_BOARDS_DONE` (Done), `AZURE_BOARDS_REMOVED` (Removed) | a personal access token with Work Items (read and write) |
| `shortcut.sh` | `SHORTCUT_TOKEN_FILE`, `SHORTCUT_STATE` (the workflow state stories are made in, by id); `SHORTCUT_DONE_STATE` | an API token: read and write the workspace's stories |

A GitHub App's installation token, and Actions' `GITHUB_TOKEN`, can't ask
`GET /user` who they are: set `GITHUB_LOGIN` to the account the token
writes issues as, `NAME[bot]` (Actions: `github-actions[bot]`); its issues
are then searched for as `author:app/NAME`. A personal access token,
classic or fine-grained, needs none. GitLab's, Jira's and Linear's tokens
say who they are (GitLab's project and group access tokens as their bot
users), so need nothing more.

Azure Boards keeps a work item's description as HTML, so its marker is the
tag `lapilli-task-ID`, as Jira's is a label, and a team's tag is
`lapilli-team-NAME`; its states are read by name (`Removed` is dropped,
`Done` and `Closed` done), as its processes name them, and
`AZURE_BOARDS_DONE` and `AZURE_BOARDS_REMOVED` say which a closed item
goes to (the Basic process has no `Removed`: set it to `Done`). A Shortcut
story in a done state is done, one archived dropped; a dropped task's story
is archived.

And for each: `LAPILLI_URL` (or the first argument), `LAPILLI_TOKEN_FILE`,
`CLOSE_ISSUES` (`false`), `COMMENTS` (`false`, below), `MAX_COMMENTS` (200), `MAX_ISSUES` made in a run (50; the rest wait for
the next), `MAX_PAGES` of 200 tasks (20) and `SYNC_DAYS`, how long after
its last change a task done or dropped is still checked against its issue
(30, at most 3650). Lapilli leaves the others out of what it gives
(`updated_after`), so tasks closed long ago, however many, aren't paged
through; `MAX_PAGES` bounds the open tasks and those closed lately, each.

What an issue is made with is in the script's `.jq`: the title, the labels
(`lapilli`, the task's kind, its team, where the tracker takes labels by
name, Jira's as `lapilli-team-NAME`; Linear takes them by id, so add
`labelIds` there), the issue type. They are made to fit: a Jira summary is
one line, a GitHub label 50 characters at most.
Change it there. The text ends with `Kept in step with Lapilli:
lapilli-task-ID` (Jira: the label `lapilli-task-ID`): that is how an issue
made by a run that couldn't put its link back is found again, rather than
made twice, so leave it in, and the label `lapilli` too where the tracker
takes labels by name. An issue is found again only among those the
tracker's token's own account made (and labelled `lapilli`): anyone may
write `lapilli-task-ID` in an issue of their own, and it isn't taken for
the task's. And only by that line: the issue's last line starting `Kept in
step with Lapilli:` must be the task's, exactly. The task's own text comes
before it, so a marker written in a task (by an editor, or in an agent's
proposal) isn't taken for another task's; lines people add below it in the
tracker are passed over. In Jira the issue's one label starting
`lapilli-task-` must be the task's, and a team's label is
`lapilli-team-NAME`, so a team's name is never a marker. GitHub's search
can take a minute to find a new issue; a run that failed between making one
and linking it is put right by a later run.

## Comments, both ways

With `COMMENTS=true`, each run also keeps the comments of each linked task
in step with its issue's, every tracker alike:

- a comment people wrote on the task in Lapilli is posted to the issue
  once: who wrote it, the comment quoted line by line (so no line of it
  starts a quick action or a heading), and a last line `Kept in step with
  Lapilli: lapilli-comment-ID`, by which a later run finds it among the
  comments the token's own account wrote, and doesn't post it again;
- a comment on the issue that the token's account didn't write, and that
  isn't the tracker's own (GitLab's system notes, Azure Boards' deleted
  comments, Shortcut's), is copied to the task once, as the tracker's: who
  wrote it as the tracker names them, when, and its text. It shows on the
  task's page as written "in the tracker", and no one can change it there.

A comment edited or deleted after it was copied isn't followed, either
way. `MAX_COMMENTS` bounds how many go each way for a task in a run; the
rest go the next. Jira takes a comment as text nodes, so nothing in it
becomes a link or a macro; GitLab gets a line starting `/` escaped, as
the issue's text is. A copied comment isn't told to the notice webhook or
Slack: the tracker told people.

## What the token can do

The tasks token reads tasks, a page at a time
(`GET /api/v1/tasks?linked=false|true&status=open,doing,done,dropped&updated_after=TIME&after=ID&limit=N`,
each with `status_by` and `status_at`, who set its status and when), and
sets a task's `link` and `status` (`PATCH /api/v1/tasks/{id}`), reads its
comments (`GET /api/v1/tasks/{id}/comments`) and adds one from the tracker
(`POST /api/v1/tasks/{id}/comments` with `text`, `author`, `ref`, the
tracker's id for it, kept once a task, and `at`; 1,000 a task at most), and
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
  the task's marker as the script writes it (and in Linear, in the team):
  one someone else wrote, one whose text merely names the marker, or one
  linked by hand, is neither taken, followed nor closed. The account is
  asked of the tracker once a run (GitHub's and GitLab's `/user`, Jira's
  `/myself`, Linear's `viewer`), or given (`GITHUB_LOGIN`).
- A URL from the tracker is followed only as the issue's own: the
  reference (a number, a key) is taken out of a link that starts with the
  tracker's own address and checked, and the request is built from the
  settings and that reference. A link that isn't one of the tracker's
  issues is never written to Lapilli, and stops the run.
- When the tracker or Lapilli fails for one task (an issue the tracker
  won't make, can't give or won't close; a write Lapilli refuses), the run
  says so, writes nothing for that task, and goes on with the rest; it
  then exits non-zero. An issue the tracker says isn't there (GitHub's 410
  for one deleted, a 404, Linear's "Entity not found") is deleted only when
  the tracker still gives the repository, project or team: then it is said
  and its task left alone, and the run doesn't fail for it; a token that
  lost its access fails the run. One task the tracker refuses doesn't hold up every
  other. When it can't go on at all (the tracker can't say who its token
  is or can't be searched, a link that isn't the tracker's, an answer from
  Lapilli not to be trusted), it stops there.

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

Write `t_me`, `t_find`, `t_create`, `t_ref`, `t_state` and `t_close` (and
`t_comments` and `t_comment`, for comments) as `lib.sh` says, set `name`, and call `sync_tasks`; `lib.sh` has `fetch` for
requests, `gone` for an answer that says an issue isn't there, and the jq
`kept` for the marker line. Please send it here.
