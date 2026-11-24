# Pull request backtests in CI

On a pull request that changes alert rules, the backtest can comment its
report on the pull request, and edit that comment on each push: what fired
before the change and what fires after, rule by rule, over the period you
name. [recipes/backtest](../recipes/backtest) has a script for each code host,
GitHub, GitLab, Bitbucket, Azure DevOps and Gitea (or Forgejo), one
library under them; and, for GitHub Actions, an action:

```yaml
on:
  pull_request:
    paths: ['alerts/**']
permissions:
  contents: read
  pull-requests: write
jobs:
  backtest:
    if: github.event.pull_request.head.repo.full_name == github.repository # never a fork's
    runs-on: ubuntu-latest # one that can reach your Prometheus
    steps:
      - uses: actions/checkout@v4
        with: {fetch-depth: 2, persist-credentials: false}
      # Pinned by the commit of the release's tag, which a tag moved can't
      # change; the action installs that release of lapilli.
      - uses: lapilli-project/lapilli/recipes/backtest@COMMIT-OF-THE-vX.Y.Z-TAG # vX.Y.Z
        with:
          paths: alerts
          prometheus-url: ${{ vars.PROMETHEUS_URL }}
          since: 14d
```

Each installs a pinned release, checked against its `SHA256SUMS` (the
action, the release it is at, which Dependabot keeps up to date with the
`uses:` line); reads the
rules of the pull request and of the commit it goes into, as files
(`paths`) or as a command renders them in each (`render: helm template
monitoring charts/monitoring`), and the incidents from the commit it goes
into, never the pull request's (`incidents`), and the times a PromQL
expression says users were hurt (`impact`, `match: service`); runs `lapilli verdict
--compare --changed-only --markdown`; and posts one comment, found again by
a hidden first line. When nothing that decides when a rule fires changed
and there is no comment yet, it posts none. When something fails, the comment says
what, and the job succeeds unless it is asked to fail. The recipes' README
has each CI's job, and what to keep from pull requests from forks: their
rules become queries sent with your credentials, and a render command runs
what they hold.

`--markdown` writes the report for a pull request or a merge request, at
most 64,000 bytes, under the 65,536 characters GitHub takes, cut short
with a note when longer:

```
[//]: # (lapilli-compare {"nothing_changed":false,"changed":1,"added":1,"removed":0,"same":42,"alike":0,"unreplayed":0,"recording_changed":0,"firings_before":148,"firings_after":42,"incidents_given":true,"incidents":3,"incidents_changed":1,"no_record":false})
### Lapilli backtest, 2026-10-26 00:00 → 2026-11-02 00:00 Asia/Seoul (+09:00)

Rules compared: **1 changed, 1 added, 0 taken away**, 42 the same.

| | Before | After |
|---|--:|--:|
| Firings | 148 | 42 |
| Severity `critical` | 125 | 16 |
| Severity `warning` | 23 | 26 |
| Incidents with no alert, of 3 | 1 | 0 |

#### Rules that differ

| Rule | Severity | Fired | ○ Caught | ● Nothing | Precision | Refired |
| --- | --- | --: | --: | --: | --: | --: |
| `ExportJob5xx` | `critical` | 118 → 9 | 0 → 0 | 118 → 9 | 0% → 0% | 41 → 2 |
| `DiskFillingUp` | – → `warning` | – → 3 | – → 1 | – → 2 | – → 33% | – → 0 |

#### Incidents caught differently

| Incident | Start | Before | After |
| --- | --- | --- | --- |
| `INC-43` `Batch stuck` | 2026-10-31 02:40 | ✗ nothing fired | `DiskFillingUp` +4m |
```

When rules that differ couldn't be replayed, a line under the totals says
how many, as `unreplayed` counts them, since the numbers leave them out;
they are listed, with why, under the table. Those that differ but fire as
they did are folded in `<details>`.

A recording rule that differs is a change too, listed apart: a replay reads
what a recording rule recorded, from the history, so the alerts that read
one are replayed against what it recorded, not what the change would
record, which the report says. Recording rules are read wherever
Prometheus's groups are (files, a `PrometheusRule`, a ConfigMap, Google
Cloud's `Rules`, an Amazon rule groups namespace, a Mimir, Cortex or Loki
ruler's file), from an Azure rule group, alone or in an ARM template, and
from a Grafana provisioning file, where a recording rule (one with
`record`) is no alert rule. A cloud's own alarms record nothing.

A group's `labels` and `limit` are part of each of its rules, as
Prometheus 3 runs them: its alerts carry the group's labels under the
rule's own, and a change to either is a change to the rule. A limit isn't
replayed: past it, Prometheus drops all of a rule's alerts, which the
report says too. A rule's name, and in `lapilli serve` its versions,
history and proposals, are as they were before Lapilli read them: a rule
told apart from another of its name is told apart by its group and its own
labels. Not every server that takes Prometheus's groups applies these:
Prometheus 2 refuses a group's `labels`; Google Cloud's `Rules`,
`ClusterRules` and `GlobalRules` have neither field; and a Mimir, Cortex or
Amazon ruler may keep them without applying them, depending on its
version. Where they aren't applied, the replay's alerts carry labels the
real ones don't.

Rule
names, labels, incidents and the reasons a rule couldn't be replayed come
from whoever wrote the rules, and a pull request's author may be anyone:
each is written as a code span, in which no host reads Markdown, HTML, a
mention (`@user`, `@org/team`), a reference (`#123`) or a link, a `|`
escaped so it can't end a table's cell, a backtick in it making the span's
fence longer, and control and format characters (bidirectional overrides,
zero-width spaces), line separators and spaces other than the space shown
as `\u{...}`. A query that failed as the history couldn't be read (as
`--fail-unreplayed` counts it: anything but the history's answer to the
query as written) says only that in the Markdown, its whole reason going to
stderr, the job's log; what is left, the history's own words on a query it
refused, has where the history and the rules are read from taken out: the
`--prometheus`, `--loki`, `--grafana` and `--ruler` URLs, the user names
in them and their hosts, each as a whole word, any IP address and port,
the tenants and a `--gcp-project`.

What a tool reads is stable:

- The report of `--json` and `/api/v1/report`, the columns of `--csv`, and
  the names and labels of `--metrics` and `/metrics`: those
  [docs/numbers.md](numbers.md) lists keep their names and meaning; a
  minor release that must change one says so in the changelog and writes
  the old beside the new for a release first.
- The first line of `--markdown` is a link reference definition, which no
  host shows, holding the summary as JSON; `--json` gives the same object
  as `"summary"`, beside the rest of the report, whose fields are as they
  were. Every field is always there:

  | Field | |
  |---|---|
  | `nothing_changed` | nothing that decides when a rule fires differs, a recording rule included: nothing was replayed |
  | `changed`, `added`, `removed`, `same` | rules in both that differ, only in `--rules`, only in `--compare`, alike in both |
  | `alike` | rules that differ but, replayed on both sides, fire as often and catch as much as they did |
  | `unreplayed` | rules that differ that couldn't be replayed, on one side or both, for any reason: those of `--json`'s `"unreplayed"` list with a side, each counted once |
  | `recording_changed` | recording rules that differ, listed as `"recording"`: what the alerts that read them see, which no replay shows |
  | `firings_before`, `firings_after` | firings on each side; of the rules that differ alone with `--changed-only` |
  | `incidents_given` | incidents were given, `--incidents` or `--impact`; when false, nothing was judged: no firing caught anything, and `incidents` is 0. The firings were judged only when it is true and `no_record` false |
  | `incidents`, `incidents_changed` | incidents in the period, and those caught differently |
  | `no_record` | incidents were given, and none was on in the period or the two weeks before it (nor `--impact` read): nothing was judged, as without them |
  | `expected` | the rules that differ were held to expectations, `--expect`, or, with `--promtool` and nothing else changed, rules alike were held to scenarios whose test files the change edited |
  | `expectations_broken` | expectations the change broke: met by the rules in place (or of a rule it adds), broken by those proposed |
  | `expectations_already_broken` | expectations broken on both sides, which aren't the change's |
  | `unexpected` | rules the change adds that no expectation holds |
  | `scenarios_unrun` | scenarios of the rules that differ that couldn't be run after the change, `--promtool` given and the rule one promtool runs: the test file unreadable, a time out, a crash. They can't be judged, as a rule that couldn't be replayed can't; through lapilli serve, which runs none, always 0 |
  | `missed_checked` | the run looked for incidents a rule caught before the change that no rule catches after it, `--fail-missed`; through lapilli serve, when the job asked (`FAIL_MISSED`, `fail_missed`) |
  | `incidents_missed` | those it found, listed as `"missed"`; 0 when `missed_checked` is false |
  | `missed_not_known` | of the incidents a rule caught before the change, those no rule replayed catches after it while a rule after it couldn't be replayed (through lapilli serve, or a rule left alone may have caught it unrecorded): not known to be missed, failing nothing; 0 when `missed_checked` is false |

- When nothing changed, `--markdown` writes that line and exactly:

  ```
  ### Lapilli backtest: nothing changed

  Nothing that decides when the 42 rules fire has changed: nothing to replay.
  ```

  but for a change that edited a scenario's test file (`--promtool`, its
  rule alike on both sides): then the Expectations section follows, as
  after the totals of a change, and `expected` is true.

- The exit status is `lapilli verdict`'s, as it was: 0 when the report is
  written, whether or not anything changed; 1 when it failed, or a rule's
  replay didn't finish within `--timeout` (the report is still written),
  or, with `--fail-unreplayed`, the history couldn't be read for one or
  its labels on its series were too many to replay, or, with
  `--fail-expectations`, the change broke an expectation (and with
  `--expect-required`, it added a rule none holds), or, with
  `--fail-missed`, it left an incident a rule caught before it caught by
  no rule after it; 2 when the flags are wrong. Nothing changing has no exit status of its
  own: a CI step takes anything but 0 as a failure, and a pull request that
  changes no rule isn't one. Read `nothing_changed`.

The header names the time zone of its times with each offset it took in
the period, `Asia/Seoul (+09:00)`, `Europe/Berlin (+01:00 → +02:00)`, or
`UTC` by any of its names: `--tz` gives one by its IANA name, and without
it, the machine's own is named as `TZ` or `/etc/localtime` says, through
every link. A zone file copied rather than linked has no name: its
abbreviations are said, `CEST (+02:00) → CET (+01:00)`.

## Stopping a merge

A comment stops nothing. To stop a merge on what the backtest finds, have
the job fail on it, and require the job, or the commit status it sets, in
the branch's rules:

- `FAIL_MISSED=true` (the action's `fail-missed`), with `INCIDENTS` or
  `IMPACT`, or through lapilli serve, by its incidents: the job fails when the change leaves an incident a rule
  caught before it caught by no rule after it (`--fail-missed`,
  [compare.md](compare.md#an-incident-no-rule-catches-after-the-change)),
  and when a rule that differs couldn't be replayed, or an incident can't
  be told caught or missed (`missed_not_known`), as a gate that can't
  judge doesn't pass. It needs no expectations file.
- `FAIL_ON_EXPECTATIONS=true` and `EXPECT_REQUIRED=true`, with `EXPECT`:
  the job fails when the change breaks an expectation, or adds a rule none
  holds. Each gate fails the job on its own.
- `STATUS=true` (the action's `status`): the job also sets a commit status
  on the pull request's head commit, `lapilli/backtest`
  (`lapilli/backtest/KEY` with a `KEY` of its own), success or failure as
  the job ends, its description saying why, linking to the job; and
  pending while it runs, from when it has found the commits, so that a
  job that dies never leaves a success: ended by a signal, such as a
  cancel, or by an error of its own, it sets failure, saying so, and
  killed outright it leaves pending, which a branch rule doesn't take as
  passing. It is a
  commit status, not a GitHub check run: one API on every host, for a
  branch rule to require by its name. The comment is posted first,
  whatever the status, and the job's exit follows the run, not the
  status: a status the host refuses is said in the job's log, and fails
  the job only with `FAIL_ON_ERROR=true`.

The status goes on the commit the CI names as the pull request's head
(`STATUS_SHA` to give another), and only when that is the commit the
backtest read, or a parent of the merge the CI made of it; with neither,
`HEAD` when it isn't a merge. A required status is set only by a job
that runs: on a pull request that changes no rule, the job posts no
comment but sets the status, so run it on every pull request, without a
`paths:` filter, if the status is required. A fork's pull request, which
the job skips (below), gets none.

A status is a commit's and its name's, not a pull request's: the same
head, opened again as a pull request into a branch its author chose that
holds the change already, would be judged against that branch, and its
success would take the place of the failure the pull request into the
default branch left. Where the CI says the repository's default branch
(GitHub's and Gitea's event, `repository.default_branch`; GitLab's
`CI_DEFAULT_BRANCH`), a pull request into another branch sets
`lapilli/backtest (into BRANCH)` instead (`lapilli/backtest/KEY (into
BRANCH)` with a `KEY`), so a rule of a branch other than the default
needs that branch's name: `lapilli/backtest (into release/1.x)`, in place
of `lapilli/backtest`, which the same head's pull request into the
default branch sets, judged against the default, and which would meet the
rule still. One into the default is named `lapilli/backtest`, as before.
A rule of a pattern (`release/*`) asks one name of every branch it
matches, and none is each branch's own: give each such branch a rule of
its own, naming its status in full, and one to each branch made later.
And require each status by its name in full, never by a pattern of names
(Gitea's and Forgejo's `lapilli/backtest*`): a pull request's author
makes a name of `lapilli/backtest (into BRANCH)` by the branch they open
it into, which such a pattern would take. A branch whose name
holds more than letters, digits and `. _ / -`, or more than 200 of them,
gets no status. In an event of no pull request (GitHub's `push` or
`workflow_run`, Gitea's `push`, GitLab's branch pipeline), the CI says
the default branch alone: the branch is the job's `TARGET_BRANCH`, named
as the CI's would be, and a job that gives `TARGET` instead, or neither,
sets no status, as one of the default's name, judged against whatever
`TARGET` is, would stand for the default's pull requests. Bitbucket
Pipelines doesn't say the default branch, and its build status is a
commit's, of its key: there two pull requests of
one branch into two destinations set the same one, the last standing for
both. Azure DevOps' status is the pull request's own. Run the job only on
pull requests into the branches a rule protects, too (`branches:` below),
so that no other pull request's run sets a status of their names.

```yaml
on:
  pull_request:
    branches: [main] # the branches whose rules require the status
permissions:
  contents: read
  pull-requests: write
  statuses: write
jobs:
  backtest:
    if: github.event.pull_request.head.repo.full_name == github.repository
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with: {fetch-depth: 2, persist-credentials: false}
      - uses: lapilli-project/lapilli/recipes/backtest@COMMIT-OF-THE-vX.Y.Z-TAG # vX.Y.Z
        with:
          paths: alerts
          incidents: incidents.yaml
          prometheus-url: ${{ vars.PROMETHEUS_URL }}
          since: 14d
          fail-missed: true
          status: true
```

A merge queue's run (GitHub's `merge_group`) isn't taken yet: its commit
is the queue's, neither a pull request's head nor GitHub's merge of one,
and the event names no pull request. `github.sh` says so and fails,
setting no status, so a branch whose rules require the job or
`lapilli/backtest` can't merge through a queue yet: leave `merge_group`
out of the workflow's `on:`, and the queue off for that branch.

Each host sets it through its own API, and the token needs more than the
comment does:

| Host | Status | The token |
|---|---|---|
| GitHub | a commit status, `POST /repos/{owner}/{repo}/statuses/{sha}`, `pending` then `success` or `failure`, on the event's `pull_request.head.sha`, when the checkout is that commit or GitHub's merge of it (`GITHUB_SHA`): of another, such as the branch checked out by name after it moved, the run is refused, the job fails, and the status is a `failure` there saying so. Into a branch other than `repository.default_branch`, named `lapilli/backtest (into BRANCH)` | GitHub Actions' `statuses: write`; a fine-grained token, commit statuses (write) |
| GitLab | a commit status, `POST /projects/:id/statuses/:sha`, `running` then `success` or `failed`, on the merge request's source commit, an external job of the pipeline when it is the pipeline's commit; into a branch other than `CI_DEFAULT_BRANCH`, named `lapilli/backtest (into BRANCH)` | the Developer role (the comment's Reporter can't), scope `api` |
| Bitbucket Cloud | a build status, `POST /repositories/{workspace}/{repo_slug}/commit/{commit}/statuses/build`, `INPROGRESS` then `SUCCESSFUL` or `FAILED`, key `lapilli-backtest` (a key's status written over by the next, a pull request's into another destination too, as Pipelines doesn't say the default branch), its `refname` the pull request's source branch | the comment's pull requests (write): the reference asks the repository scope, which it implies |
| Gitea, Forgejo | a commit status, `POST /repos/{owner}/{repo}/statuses/{sha}`, `pending` then `success` or `failure`; in Actions, into a branch other than the event's `repository.default_branch`, named `lapilli/backtest (into BRANCH)` | `write:repository` beside the comment's scopes, and an account that may write to the repository |
| Azure DevOps | a pull request status, `POST .../pullRequests/{id}/statuses`, `pending` then `succeeded` or `failed`, genre `lapilli` and name `backtest`, on the iteration at the head commit (`GET .../iterations`): none when there is no such iteration, as one on the pull request alone would stay after a later push | a personal access token's Code (status), `vso.code_status`, or the build service's Contribute to pull requests (not checked against an account) |

Each is tested against its reference's documented answers, with a server
standing in; Gitea's also in CI's run against a real Gitea
(`test/e2e/backtest.sh`).

## Through lapilli serve

A CI job can send a pull request's rules to `lapilli serve` instead of
running Lapilli itself: serve replays them with its own access to the
history, at its share of the rate, judges them by its record, and gives
back the same report, summary line and all, which the job posts as it
would its own, the lines Lapilli writes and no other ([serve.md](serve.md#pull-request-backtests); the design,
[design/backtests.md](design/backtests.md)). The job holds no credential
for the metrics, only a token that says which repository it runs for: the
CI's own ID token where it has one (GitHub Actions, GitLab CI, Forgejo
Actions), a plain token elsewhere. In the recipes, it is `LAPILLI_SERVE`,
and on GitHub Actions the action's `serve-url` and `audience`, with
`id-token: write`:

```yaml
permissions:
  contents: read
  pull-requests: write
  id-token: write
jobs:
  backtest:
    if: github.event.pull_request.head.repo.full_name == github.repository # skips a fork's run; GitHub gives one no ID token
    runs-on: ubuntu-latest # needs no access to the metrics
    steps:
      - uses: actions/checkout@v4
        with: {fetch-depth: 2, persist-credentials: false}
      - uses: lapilli-project/lapilli/recipes/backtest@COMMIT-OF-THE-vX.Y.Z-TAG # vX.Y.Z
        with:
          paths: alerts
          serve-url: https://lapilli-ci.example.com
          audience: https://lapilli-ci.example.com
          since: 14d
```

The `if:` saves a fork's run the job; it keeps no one out, as the pull
request can change it. What keeps a fork out is GitHub giving a fork's
`pull_request` run no ID token, unless, on a private repository, "Send
write tokens to workflows from pull requests" is on: keep that setting off
([serve.md](serve.md#pull-request-backtests)). The trust entry's pins
don't: a fork's run in the repository carries the repository's own
`repository_id`.

The rules are read from git, and `render` run, in the job as before;
`expect` is read from both commits and held by serve, and the job's gate
(`fail-on-expectations`, `expect-required`) reads the summary line as it
reads Lapilli's. `fail-missed` holds the change to serve's incidents: the
rules that differ are judged by the backtest, the rules left alone by
serve's record of them, which rules caught each incident, not replayed: a
catch counts only where the rule was then the rule it is after the change,
as parsed: a rule serve reads from Prometheus's rules API, which prints
its expression again (`sum by (a) (x)` for `sum(x) by (a)`, `5m` for
`300s`, its `for` in seconds), is the rule its file writes. An incident
a rule left alone may have caught with no record of it (serve doesn't run
the rule as the pull request has it, or its runs don't cover the
incident's time), or that a rule written otherwise then caught, is not
known, which fails the step as a gate that can't judge
([backtests.md](design/backtests.md#held-to-the-incidents-step-4)); so is
one a rule caught whose file gives no group `interval` while serve reads
it from an API, which says the interval it runs at, or whose expression
Lapilli compares only by its text (a Grafana rule, a Loki ruler's, what
MetricsQL reads otherwise than Prometheus).
`status` works as without it. Serve runs no scenario a pull request's expectations name
(`scenarios:`, promtool test files): each is said as not run through
lapilli serve, and `promtool` is refused with `serve-url`; run `promtool
test rules` in a step of its own. Serve decides where the history is and what judges the
firings, so the settings for those (`prometheus-url`, `incidents`,
`impact`, `match`, `--lead`, `--from`, ...) are refused with it: of the
arguments, `--since` (whole days, 1 to 14), `--tz` and `--rules-select`
are sent. Grafana-managed rules in `paths`, and a Loki ruler's in
`loki-paths` (`LOKI_PATHS`), are backtested only on the data sources the
rules serve runs read already: any other is refused, naming none of
serve's. The comment's heading says it was judged by lapilli serve's
record, and reads differently where that record does: catches judged
against the incidents and impact windows serve keeps, firings by its weeks
and settings. The recipes' README has each CI's token, what is sent, and
what is refused ([Through lapilli
serve](../recipes/backtest/README.md#through-lapilli-serve)).
