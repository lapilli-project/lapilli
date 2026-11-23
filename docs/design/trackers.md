# Tasks kept in step with a tracker

Status: built, for v0.22.0 (see "As built" at the end). Under
[GOVERNANCE.md](../../GOVERNANCE.md) this adds a token and what it may
write, so the design came first, as a pull request (#40); it was merged on
2026-11-11, before its week was out, by the maintainer's decision, with no
comments. The companion to [work.md](work.md), about tasks.

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

## As built

As above, with these settled in the building:

- **The token.** `--tasks-token-file`, on the port `--incidents-listen`
  opens, alone or beside the incidents and firings tokens; 5 requests a
  second, bursts of 100, one at a time, each its own. `lapilli serve`
  refuses to start, and the port refuses each request, while its file holds
  the same value as the incidents, firings or notifications token's.
  Changes are noted as `tasks-token`, a name the sign-in proxy's header
  can't take, and outcomes don't count them.
- **Reading.** `GET /api/v1/tasks?linked=&status=&updated_after=&after=&limit=`
  gives tasks by id, 200 at most a page, with `next` while there may be
  more, each with its team's name and its page's URL (with
  `--external-url`), and `status_by` and `status_at`: who set the status
  it has and when, from the changes kept (the change that moved it there;
  later edits of anything else, the job's link among them, don't count;
  neither is given once that change is pruned, even while later edits at
  the same status are kept: none of those is guessed to be the move, and
  the recipes take a status no one is known to have set as not the
  job's). They are looked up by an index on the changes' `what`
  (`changes_what`, in 013), so a page reads only its own tasks' changes:
  at a million changes kept, 1.3 ms a page of 200 rather than 260 ms. It
  can also ask for
  tasks done or dropped, which the recipes check against their issues for
  `SYNC_DAYS` (30); `updated_after`, an RFC 3339 time, leaves out the
  tasks not changed since, so those closed long ago aren't paged through.
- **Writing.** `PATCH /api/v1/tasks/{id}` takes `link` and `status` and
  refuses any other field, a body over 16 KB, a link that isn't `https`,
  is longer than 2,048 characters, has a user name in it, or would replace
  another. What is so already isn't written again. A task's link written
  by a person may now be 2,048 characters too (it was 500).
- **Not told.** A status the job sets isn't told to the notice webhook or
  Slack, as copied incidents aren't: the tracker has told people, and
  whoever holds the token can't fill the outbox with statuses flipped back
  and forth. People's changes to tasks are told as before.
- **Which way a task moves.** Each run reads every linked task's issue
  (those done or dropped while changed within `SYNC_DAYS`): a task closed
  since its issue last changed isn't reopened by it; a task the job closed
  because its issue was closed is opened again when the issue is open;
  `CLOSE_ISSUES=true` closes an issue whose task a person closed, unless
  the issue changed after the task was closed; an open task whose issue
  closed after its status was set is closed. Who closed a task, and when,
  is its status's own (`status_by`, `status_at`), not its last edit: a
  person reassigning a task the job closed doesn't make it theirs, and
  the job writing a link doesn't make a person's close its own. A comment
  on an open issue therefore never reopens a task closed in Lapilli, and a
  task reopened in Lapilli doesn't reopen its issue. GitLab closes issues
  one way, so its tasks close as `done`.
- **Found again, not made twice.** An issue's text ends with the line
  `Kept in step with Lapilli: lapilli-task-ID` (Jira: a label of that
  name), searched for before an issue is made, so a run that made an issue
  but couldn't put its link back doesn't make a second. Only among the
  issues the recipe's own account made (GitHub's `/user`, or
  `GITHUB_LOGIN` for a GitHub App's or Actions' token, which can't ask it,
  searched for as `author:app/NAME`; GitLab's `/user`, Jira's `/myself` as
  the issue's creator, Linear's viewer) and, where labels are given by
  name, labelled `lapilli`: anyone who can file an issue can write
  `lapilli-task-ID` in it, and ids come one after another, so an issue
  filed ahead for the next task would otherwise be taken for its. A
  task's id is never another's again (schema 016, 0.28.1): before, a new
  task given a deleted one's id took over its issue. A ledger upgraded to
  016 gives new tasks ids past the highest it still knows was given, from
  its tasks, the changes it keeps noted of tasks, and the proposals made
  into them; a task deleted, with no task made after it, whose changes
  `--keep` has dropped since, leaves no trace: its id can be given once
  more, and its issue taken. The recipes
  don't yet hold an issue to the task's time.
- **Only the recipe's own marker.** The task's text is written into its
  issue as it is, an editor's or an agent's in a proposal, so it may name
  another task's marker, even as the very line the recipe writes. That
  line is always written after the text, and an issue is marked for a
  task only when its last line starting `Kept in step with Lapilli:` is
  that task's, exactly: a marker anywhere else is text, and lines people
  add below it in the tracker are passed over. In Jira, where the marker
  is a label, a team's label is `lapilli-team-NAME` (a team's name once
  became a label as it was, and `lapilli task 7` was a marker), and the
  issue's one label starting `lapilli-task-` must be the task's.
- **Only the job's own issues.** A linked task is followed, and its issue
  closed with `CLOSE_ISSUES=true`, only when the issue is one the job made
  for that task: its account made it, it holds that task's marker, and
  (Linear) it is in `LINEAR_TEAM`. Any other link, one an editor wrote by
  hand to someone's issue or to the job's issue for another task, is left
  alone and said so: an editor can't have the job close an issue it
  didn't make. A tracker's failure is told apart from an answer that
  isn't the job's: each answer is read only once the request has
  succeeded.
- **The field map** is each recipe's `.jq`. Linear takes labels by id, so
  its map gives none until they are added. A task's title on two lines is
  one line in a Jira summary, which takes no newlines; a team's name is
  cut to GitHub's 50 characters for a label, and is `lapilli-team-NAME`
  in Jira.
- **One task doesn't stop the rest.** An issue the tracker won't make,
  can't give or won't close, or a write Lapilli refuses, is said and that
  task skipped; the others are made, linked and moved, and the run exits
  non-zero at the end. A run stops at once only when it can't go on: the
  tracker can't say who its token is or can't be searched, gives a link
  that isn't its own, or Lapilli's answer isn't to be trusted.
- **An issue deleted** in the tracker isn't a failure, or every run would
  fail for as long as its task is linked: the tracker saying the issue
  isn't there (GitHub's 410, a 404, Linear's "Entity not found") while it
  still gives the repository, project or team is said each run, and the
  task is left as it is, neither closed nor reopened. A token that lost its
  access to the project fails the run. A person clearing the task's link
  in Lapilli has a new issue made.
- **The first question for review:** `CLOSE_ISSUES` is off unless set,
  so a run closes nothing in the tracker that people there haven't.
- **The chart.** `serve.trackers.tokenSecret` takes the token;
  with `tracker` (`github`, `gitlab`, `jira`, `linear`), `image` (one with
  `sh`, `curl` and `jq`, pinned by digest; the chart names none) and
  `trackerTokenSecret`, a CronJob runs the recipe every five minutes from a
  ConfigMap of the chart's copies (a test keeps them the same as
  `recipes/trackers`), and the NetworkPolicy lets it reach the incidents
  port alone.
- **Reviewed as an attacker would** (CONTRIBUTING.md). Someone with the
  tasks token can read every task (titles, text, assignees, rules,
  incidents, teams, links) and link each task without a link to any
  `https` URL once, and open or close tasks; they can't make, retitle or
  delete a task, write anything else, reach the UI's port, be named by
  the proxy's header, or fill the outbox. The link is shown with
  `rel="noopener noreferrer"` and only as `https` (the UI's own links
  still take `http`), so no `javascript:`. The recipes follow no URL from
  the tracker or Lapilli but the issue's own, rebuilt from the configured
  address and a reference checked against the tracker's shape; a link
  that isn't one of the tracker's issues stops the run and is never
  written. A task's title and text reach the tracker as written in
  Lapilli, mentions included: the tracker renders them as it renders its
  own, and nothing in them is taken for a marker.

## YouTrack and Jira Data Center (0.70)

Two more recipes, alike with the others, each tested against a stand-in
for the API its documentation gives (recipes/trackers/README.md names the
pages); neither is checked against a real account yet.

- **`youtrack.sh`**: issues in one project (`YOUTRACK_PROJECT`, the ID its
  issues' IDs start with), REST `/api`, a permanent token as a Bearer
  token. YouTrack answers only the attributes `fields=` names, so every
  request names them. The project's database id, which an issue is made
  in, is looked up among the projects the token reads, a thousand at most.
  An issue's text ends with the marker line, as GitHub's does, since
  YouTrack's tags are made and shared beforehand and given by id; it is
  found again by search (`project: {OPS} reporter: me "lapilli-task-ID"`)
  and taken for the job's by its reporter, which YouTrack doesn't let be
  changed, its project and its last line. An issue resolved closes its
  task, by when it was (`resolved`): `dropped` when its state is
  `YOUTRACK_DROPPED` or reads as not to be done, `done` otherwise.
  `CLOSE_ISSUES` sets the state field as a custom field is set
  (`StateIssueCustomField`), not by a command: a command is a language of
  its own, and the field's value goes in it as text. A field a
  state-machine workflow keeps is moved by its transitions alone, which
  the recipe doesn't name: YouTrack refuses the update, the run says so,
  and the issue stays open. Comments
  limited to some groups or users aren't copied.
- **`jira-dc.sh`**: Jira Data Center and Server, as `jira.sh` is Jira
  Cloud: REST API v2, a personal access token as a Bearer token (8.14 and
  later), the same labels and JQL, `/rest/api/2/search`, transitions into
  the done category. Users are by `key`, which a rename doesn't change.
  Text is wiki markup, not Atlassian Document Format, and wiki markup has
  no escape that holds everywhere: the task's text and its names go in a
  `{noformat}` block, and a posted comment is quoted in one, so a PromQL
  selector's braces aren't a macro, `[~name]` mentions no one and `!URL!`
  shows no image. Only `{noformat}` ends the block, and one in the text is
  broken by a zero-width space. The link back and the marker line are
  told from the end of what `issue_text` writes, so no line of a task's
  text is taken for them and moved out of the block. A Jira that doesn't
  give `statuscategorychangedate` is read by `resolutiondate`, as Cloud's.

Both wait out a 429, and a 503 on a read, as long as `Retry-After` says, as
every recipe does (`fetch` in `lib.sh`). The chart's `serve.trackers.tracker`
takes `youtrack` and `jira-dc`; Jira Data Center's settings are named
`JIRA_*`, as Cloud's are, without `JIRA_EMAIL`.

What an attacker could newly reach: nothing past what the other recipes
reach. Each token goes to the configured address alone; an issue's
reference is taken out of a link only when it starts with the configured
address and is `PROJECT-N`; a project's id, a comment's id and every time
are checked before they are used or sent on; the project lookup and the
comments are paged within bounds. The names a rule's source chooses can't
mention, link or show an image in either tracker.

## Edits followed (0.70)

ROADMAP said, of trackers, "Still to do: edits followed". What it meant is
in [inputs.md](inputs.md), section 6: comments are kept in step both ways
since 0.27, "once", and "a comment edited or deleted after it was copied
isn't followed either way", left as a question and settled for that
release. Nothing in the designs asked for a task's title or text to be
carried after its issue was made. So this is about comments, and it
needed deciding: which edits, which way, by what, and what is left.

**Decided here:**

- **Edits, both ways; not deletions.** A comment changed after it was
  carried is carried again. A comment deleted isn't followed, either way:
  in Lapilli a deleted comment leaves no row to say which post was its
  (following it would take a new column, a schema change), and in a
  tracker a comment no longer read can be one deleted, one made
  restricted, or one past what a run reads. Taking it away from Lapilli on
  that guess could take a person's words from where the team reads them.
- **Not a task's title or text.** An issue is the tracker's once it is
  made: people there retitle it and write under it, and the marker line
  must stay last. Carrying a task's edits would overwrite theirs; carrying
  theirs back would let anyone who can edit the issue rewrite the task.
  Neither is built, and neither is asked for.
- **By time, not by text.** Each tracker gives a comment's last change
  (GitHub's and GitLab's `updated_at`, Jira's `updated`, Linear's
  `editedAt`, Azure Boards' `modifiedDate`, Shortcut's `updated_at`,
  YouTrack's `updated`), and Lapilli keeps its own. Text alone can't
  decide: trackers give a comment back otherwise than it was sent (Jira
  as Atlassian Document Format, Azure Boards as HTML, Jira Data Center's
  posts in a block), and a difference that never goes away would edit the
  post every run.

**How.**

- **Lapilli to the tracker.** A comment people wrote here, posted to its
  issue, and changed here after the tracker last changed the job's post
  of it, has the post changed to what would be posted now (the comment
  quoted, its marker last), unless the post holds that already. Only the
  job's own post, found by the tracker's id Lapilli keeps for it; the
  tracker changes it (`PATCH`, `PUT` or `POST` to the comment, as each
  takes an edit). Once it has, the post is later than the comment, so it
  is changed once. The comment's time is Lapilli's and the post's the
  tracker's, two clocks, as `CLOSE_ISSUES` compares a task's status with
  its issue's last change: an edit made here within their difference after
  the post may be missed, and a post someone changed in the tracker after
  the last edit here isn't changed back until the comment is edited again.
- **The tracker to Lapilli.** A comment copied in, changed in the tracker
  after the time Lapilli keeps for it, is sent again with the tracker's
  time for the change, its text made to fit as when it was copied. Lapilli
  keeps a copy's times as the tracker gives them (`created` when it was
  written, and now `updated` when it last changed there), so this compares
  one clock with itself. It keeps the edit only when it is later than the
  one kept, text and time, though the text shows as the one kept does: an
  edit is taken once, the same sent again or an older one after a newer
  changes nothing. Only a comment copied in from the tracker is changed
  so; a person's is theirs.
- **Bounded.** `MAX_COMMENTS` (200) each way a task a run, as comments
  are; a post isn't edited where the issue has more comments than a run
  reads and the post isn't among those read.
- **A recipe of one's own** that gives no comment's time of change
  (`edited`, beside `at`) follows no edit, and one with no `t_edit` sends
  none to the tracker; each keeps comments in step as before.

**What the tasks token may now also do:** `PATCH
/api/v1/tasks/{id}/comments/{comment}` with `text` and `edited` (an RFC 3339
time), for a comment copied in from the task's issue, and nothing more: it
answers whether it `changed`, and whether the text was kept `as_sent`. It
is noted as `tasks-token`, and isn't told to the notice webhook or Slack:
the tracker told people. With `posted` it is as before, and the two can't
be sent together.

**No schema change.** A copy's `updated`, kept since 015 and set to when
it was written, now holds when it was last changed in the tracker.

**Attacker's look.** Someone with the tasks token can now also rewrite
the text of a comment copied in from a tracker, to what a copy may hold,
at most 10,000 characters, with what doesn't show kept as what does; not
a person's comment, not its author, not its time past now. That is no
more than copying a comment in already let them write. Someone who can
edit a comment in the tracker can change what its copy says in Lapilli:
that is what kept in step means, and it shows as the tracker's. The
recipes send an edit only to the job's own comment, by an id checked to
be the tracker's shape (digits; YouTrack's `N-N`; Linear's as a GraphQL
variable), on an issue whose reference they checked; what they send is
what they would post.

**Left to do:** deletions, either way; an edit in the tracker of a
comment no longer read (restricted since, or past the bound); checking
the edit endpoints against real accounts.
