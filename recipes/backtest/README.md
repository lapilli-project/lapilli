# Pull request backtests

Each of these scripts backtests the alert rules a pull request (or a merge
request) changes against your metric history, and comments the report on
it: what fired before the change, what fires after, rule by rule, over the
period you name. It runs in the pull request's CI job, where the checkout
already is, and posts with a token of the host's that may comment.

| Script | Host | Needs |
|---|---|---|
| `github.sh` | GitHub, GitHub Enterprise | `GITHUB_TOKEN_FILE`; in GitHub Actions, nothing more ([`action.yml`](action.yml) runs it) |
| `gitlab.sh` | GitLab | `GITLAB_TOKEN_FILE`; in GitLab CI, nothing more |
| `bitbucket.sh` | Bitbucket Cloud | `BITBUCKET_TOKEN_FILE`, and `BITBUCKET_USER_UUID` to edit its comment (below) |
| `azure-devops.sh` | Azure Repos | `AZURE_DEVOPS_TOKEN_FILE`; in Azure Pipelines, nothing more |
| `gitea.sh` | Gitea, Forgejo | `GITEA_TOKEN_FILE`; in Gitea or Forgejo Actions, nothing more; elsewhere `GITEA_API`, `GITEA_REPO`, `PR` |

Each takes the pull request's number, the repository and the API from its
CI's own variables; outside it, set them (each script's header lists
them). An API is asked over https, or plain http to the machine itself
only: the token goes with each request. Each also needs:

- `LAPILLI_VERSION`, the Lapilli release to install, such as `v0.33.0` (one
  with `--markdown`). The archive for the machine is downloaded from the
  release and checked against the release's `SHA256SUMS` before it is
  unpacked; `LAPILLI_SHA256` pins its sum too, which a release replaced
  later can't match. `LAPILLI_RELEASES` downloads from a mirror holding the
  same files, over https and only with `LAPILLI_SHA256`: whoever serves an
  archive could serve a `SHA256SUMS` that agrees. `LAPILLI` runs a
  `lapilli` already on the machine instead.
- Where the rules are: `PATHS`, directories in the repository,
  space-separated (`alerts`), as `--rules` reads them; or `RENDER`, a
  command that writes them on its stdout, run in each commit's files
  (below). A file named in `PATHS` works too, but renamed in a pull
  request it is a file one commit hasn't: its rules read as taken away on
  one side and added on the other. A path neither commit has, any of them,
  fails the job at once: its rules would never be compared.
- The arguments: `lapilli verdict`'s flags for where the history is and
  over what period, as `lapilli verdict` takes them: `--prometheus URL` (or
  `--gcp-project`, `--auth`, `--bearer-token-file`), `--since 14d`, and any
  others, such as `--tz Asia/Seoul` or `--rules-select grafana_alert=1`.
  The recipe gives `--rules`, `--compare`, `--changed-only` and
  `--markdown` itself. A file an argument names in the checkout
  (`--incidents`, `--on-call`, a token's file) is refused: it is the pull
  request's, which could take out the incident its change misses: in any
  directory of the clone, whichever of them `REPO` is or the job runs from.

And, as wanted:

- `INCIDENTS`, a file of incidents in the repository, read from the commit
  the pull request goes into, never the pull request's own: a file, not a
  link, of 8 MiB at most. Or `--incidents` with a file from outside the
  checkout, such as one an earlier step wrote from your incident tool
  ([recipes/incidents](../incidents)).
- `TARGET`, the commit the pull request goes into (default `HEAD^1`, the
  merge commit's first parent, as GitHub checks a pull request out); or
  `TARGET_BRANCH`, a branch: the clone's `origin/BRANCH` when the checkout
  fetched it (a full clone fetches every branch), or else fetched from
  `origin`, which needs the checkout's credentials kept. The rules compared
  against are those of the commit `HEAD` and the target share last (`git
  merge-base`): what the pull request itself changes, whether the checkout
  is a merge or the pull request's own commit. Each script defaults it from
  its CI.
- `KEY`, to tell apart the comments of several jobs on one pull request,
  one per Prometheus say (default `default`).
- `FAIL_ON_ERROR=true`, to fail the job when the backtest fails (below).

They need `sh`, `git`, `curl`, `jq` and `tar`, on Linux or macOS.

## What a run does

1. Finds the two commits, and installs Lapilli, unless `LAPILLI` is given.
2. Reads the rules of the commit the pull request goes into, then of the
   pull request's, from git, as git keeps them (`git archive`, so a
   commit's `.gitattributes` neither leaves a file out nor runs a filter):
   those `PATHS` names, or all of each commit with `RENDER`. A path one
   commit hasn't is a directory with no rules, so a pull request that adds
   the first rules, or takes the last away, is compared too. A commit with
   a symbolic link where the rules are read (anywhere, with `RENDER`) isn't
   read: a link could lead to any file the job can read.
3. With `RENDER`, runs `RENDER_PREPARE`, then `RENDER`, in each commit's
   files, the base's first, and takes what `RENDER` writes as that commit's
   rules.
4. Runs `lapilli verdict --rules PULL-REQUEST --compare TARGET
   --changed-only --markdown`, with the arguments given: only the rules
   that differ are replayed. A recording rule that differs is a change, and
   the comment says it isn't replayed (the alerts that read it are,
   against what it recorded).
5. Finds the comment it posted before: the token's account's, whose first
   line is `[//]: # (lapilli-backtest KEY)`, a link reference definition,
   which no host shows. It edits that one, or posts one. When nothing that
   decides when a rule fires changed and there is no comment yet, it posts
   none; when there is one, it says that nothing changes now.

## In each CI

The recipe's two files (`lib.sh` and the host's script) are small: keep a
copy in the repository, from the tag of the release you install, or fetch
them at that tag in the job (`ci/backtest/` below). On GitHub, the action
needs neither. Run the job on pull requests that touch the rules, on a
runner that can reach your Prometheus.

### GitHub Actions

```yaml
on:
  pull_request:
    paths: ['alerts/**']
permissions:
  contents: read
  pull-requests: write
jobs:
  backtest:
    # Pull requests from forks: see below.
    if: github.event.pull_request.head.repo.full_name == github.repository
    runs-on: ubuntu-latest # one that can reach your Prometheus
    steps:
      - uses: actions/checkout@v4
        with: {fetch-depth: 2, persist-credentials: false}
      # The commit the release's tag names, which no tag moved changes.
      - uses: lapilli-project/lapilli/recipes/backtest@COMMIT-OF-THE-v0.33.0-TAG # v0.33.0
        with:
          version: v0.33.0
          sha256: SHA256-OF-lapilli-v0.33.0-linux-amd64.tar.gz # as its SHA256SUMS says
          paths: alerts
          incidents: incidents.yaml
          prometheus-url: ${{ vars.PROMETHEUS_URL }}
          since: 14d
```

`fetch-depth: 2` fetches the merge commit's parents; `persist-credentials:
false` keeps the job's token out of the checkout's git configuration,
where `RENDER` could read it. The action's inputs are `version` (required:
the release the tag is), `sha256`, `paths` or `render` and
`render-prepare`, `incidents`, `prometheus-url`, `auth`, `gcp-project`,
`aws-region`, `since` (default `7d`), `args` (more flags, split at spaces
and never expanded by a shell), `target`, `key`, `fail-on-error`, `token`
(default the job's own) and `login` (the account a GitHub App's token
comments as, `NAME[bot]`). For a cloud's managed Prometheus, sign in first
with the cloud's own action through OpenID Connect (`id-token: write`):
`aws-actions/configure-aws-credentials`, `azure/login` or
`google-github-actions/auth`, and give `auth`.

### GitLab CI

```yaml
backtest-rules:
  image: alpine:3.20
  rules:
    - if: $CI_PIPELINE_SOURCE == "merge_request_event"
      changes: [alerts/**/*]
  variables:
    GIT_DEPTH: 0
    LAPILLI_VERSION: v0.33.0
    PATHS: alerts
    INCIDENTS: incidents.yaml
    # A CI/CD variable of type File holding a project access token (role
    # Reporter, scope api), masked: GitLab gives its path.
    GITLAB_TOKEN_FILE: $BACKTEST_TOKEN
  script:
    - apk add --no-cache git curl jq
    - sh ci/backtest/gitlab.sh --prometheus "$PROMETHEUS_URL" --since 14d
```

In a merge request pipeline the script takes the merge request, its
project (`CI_MERGE_REQUEST_PROJECT_ID`) and the API from GitLab's
variables, and the target from `CI_MERGE_REQUEST_TARGET_BRANCH_SHA` (merged
results) or `CI_MERGE_REQUEST_DIFF_BASE_SHA`; `GIT_DEPTH: 0` fetches the
history they are in.

### Bitbucket Pipelines

```yaml
pipelines:
  pull-requests:
    '**':
      - step:
          name: Backtest the alert rules
          image: alpine:3.20
          clone:
            depth: full
          script:
            - apk add --no-cache git curl jq
            # BACKTEST_TOKEN: a secured repository variable, a repository
            # access token with the pull requests write scope.
            - export BITBUCKET_TOKEN_FILE=$(mktemp)
            - printf '%s' "$BACKTEST_TOKEN" > "$BITBUCKET_TOKEN_FILE"
            - export BITBUCKET_USER_UUID='{...}' # the token's account, as its first comment's run says
            - LAPILLI_VERSION=v0.33.0 PATHS=alerts INCIDENTS=incidents.yaml sh ci/backtest/bitbucket.sh --prometheus "$PROMETHEUS_URL" --since 14d
```

Bitbucket Pipelines clones the pull request's branch alone: the script
fetches the destination (`BITBUCKET_PR_DESTINATION_BRANCH`) itself, as
Bitbucket's clone lets a step fetch. A
repository access token can't say whose it is (`GET /user` needs the
account scope, which it can't have): the first run posts a comment and
says, in the job's log, the account it posted as; set
`BITBUCKET_USER_UUID` to it, and later runs edit that comment. Without it,
each run posts one. Bitbucket shows HTML in a comment as text: what
Lapilli folds in `<details>` is a bold line and a list there.

### Azure Pipelines (Azure Repos)

A pull request into an Azure Repos branch runs the pipeline its branch
policy names (Build validation). Give the project's build service
"Contribute to pull requests" on the repository.

```yaml
steps:
  - checkout: self
    fetchDepth: 0
  - script: |
      AZURE_DEVOPS_TOKEN_FILE=$(mktemp)
      printf '%s' "$SYSTEM_ACCESSTOKEN" > "$AZURE_DEVOPS_TOKEN_FILE"
      AZURE_DEVOPS_TOKEN_FILE=$AZURE_DEVOPS_TOKEN_FILE LAPILLI_VERSION=v0.33.0 PATHS=alerts INCIDENTS=incidents.yaml \
        sh ci/backtest/azure-devops.sh --prometheus "$PROMETHEUS_URL" --since 14d
      rm -f "$AZURE_DEVOPS_TOKEN_FILE"
    env:
      SYSTEM_ACCESSTOKEN: $(System.AccessToken)
      PROMETHEUS_URL: $(PROMETHEUS_URL)
```

The script compares against the branch the pull request goes into
(`SYSTEM_PULLREQUEST_TARGETBRANCH`), as the order of the merge commit's
parents isn't documented: `origin/BRANCH`, which the checkout fetches with
every other branch. The checkout drops its credentials when it is done
(`persistCredentials` is false unless set), so the script fetches nothing
itself; a checkout that leaves the branch out (a `fetchFilter`, a sparse
or shallow fetch) needs `persistCredentials: true` for the script to fetch
it. The comment is a thread of its own, posted
closed: resolved, so the policy that comments be resolved doesn't wait for
it. The token's identity (`connectionData`) and a comment's author are
told apart by their id, as Azure DevOps documents both; this hasn't been
checked against an account.

### Gitea and Forgejo Actions

```yaml
on:
  pull_request:
    paths: ['alerts/**']
jobs:
  backtest:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with: {fetch-depth: 0, persist-credentials: false}
      - run: |
          GITEA_TOKEN_FILE=$(mktemp)
          printf '%s' "$BACKTEST_TOKEN" > "$GITEA_TOKEN_FILE"
          GITEA_TOKEN_FILE=$GITEA_TOKEN_FILE LAPILLI_VERSION=v0.33.0 PATHS=alerts INCIDENTS=incidents.yaml \
            sh ci/backtest/gitea.sh --prometheus "$PROMETHEUS_URL" --since 14d
        env:
          # A bot's token: write:issue, read:repository, read:user.
          BACKTEST_TOKEN: ${{ secrets.BACKTEST_TOKEN }}
          PROMETHEUS_URL: ${{ vars.PROMETHEUS_URL }}
```

Actions checks out the pull request's own commit there: the script takes
the branch it goes into (`GITHUB_BASE_REF`) as `fetch-depth: 0` fetched
it, `origin/BRANCH`, without the credentials `persist-credentials: false`
keeps from the job, and compares from where they
parted.

## Rules rendered from a chart

`RENDER` runs in a copy of each commit's files, with `REPO` the clone's
full path, and writes the rules: a Helm chart's whole rendering, or a
Kustomize overlay's, of which only the documents of rules are read, the
rest passed over, counted, and said in the comment. Render both commits
with the same values files and flags, so that only the pull request
differs:

```yaml
      - uses: lapilli-project/lapilli/recipes/backtest@COMMIT-OF-THE-v0.33.0-TAG # v0.33.0
        with:
          version: v0.33.0
          render: helm template monitoring charts/monitoring -f charts/monitoring/values-prod.yaml
          render-prepare: helm dependency build charts/monitoring >&2
          prometheus-url: ${{ vars.PROMETHEUS_URL }}
          args: --rules-select grafana_alert=1
```

`RENDER_PREPARE` runs first, for what a commit lacks (a chart's
dependencies); what it writes goes to the job's log, not read as rules. A
rendering with no rules in it is no rules on that side, but a command that
fails is a failure: for a chart the pull request adds, which the commit it
goes into hasn't, say so in the command, `[ ! -d charts/monitoring ] ||
helm template monitoring charts/monitoring`.

`RENDER` and `RENDER_PREPARE` run what the pull request holds (a chart's
templates, a Kustomize plugin) with the job's rights. Each commit's run has
a `HOME` of its own, the base's first, and runs without the variables CIs
keep tokens in (`GITHUB_TOKEN_FILE` and the other scripts' token files,
`GITHUB_TOKEN`, `ACTIONS_ID_TOKEN_REQUEST_TOKEN`, `CI_JOB_TOKEN`,
`CI_REPOSITORY_URL`, `CI_DEPENDENCY_PROXY_PASSWORD`, `SYSTEM_ACCESSTOKEN`,
`BITBUCKET_STEP_OIDC_TOKEN` and the like), so a chart doesn't take one by
chance. That isn't a wall. What runs is the job's own user: it can read
any file the job can, the token's file among them; on Linux, the
environment every process of the job started with, the recipe's and the
CI's runner's included, under `/proc/PID/environ`, whatever was unset
since; a variable a CI names as you choose (GitLab's `id_tokens`, a
secret you export) that the list above can't know; and it can change what
the job does next. Helm's `template` runs no code of the chart's; a
Kustomize plugin, or a `RENDER` that runs the repository's scripts, does.
That is one reason forks are kept out.

## The comment

Lapilli writes it (`lapilli verdict --compare --markdown`, see the
[README](../../README.md#pull-request-backtests-in-ci)): the period and its
time zone, how many of the rules that differ couldn't be replayed, if any,
the totals before and after, the rules that differ as a table, the
recording rules that differ, those that couldn't be replayed, with why,
those that differ but fire as they did folded away, and with incidents,
those caught differently. It is cut short at 64,000 characters, saying so. When
nothing that decides when a rule fires changed, it is a heading and one
line.

When something fails (the install, reading a commit, `RENDER`, the
backtest), the comment says what, in a fenced block with the last lines of
what was said about it, and the job succeeds: a backtest that can't run
shouldn't hold a pull request up, unless you ask it to. With
`FAIL_ON_ERROR=true` the job fails too, once the comment says why: also
when a rule that differs couldn't be replayed, and when the comment can't
be posted. A rule that differs couldn't be replayed, for this, whatever
why, as the summary's `unreplayed` counts them: the history out of reach
(not let in, turned away, `--timeout` too short), or the query refused as
written, which is the pull request's to mend. `lapilli verdict`'s own
`--fail-unreplayed`, which fails only on the first, for a scheduled run of
every rule where a rule refused as written would fail each run alike, isn't
taken: the recipe decides when the job fails. A setting that is wrong (no
`PATHS`, a `PATHS` neither commit has, a `TARGET` that isn't a ref, a flag
the recipe gives or decides, a file in the checkout) fails the job at once,
whatever it says.

`lapilli verdict`'s stderr is copied to the job's log as it is written, so
a long backtest, against a Prometheus with a request quota, say, shows how
it is going every 30 seconds rather than looking hung; none of those lines
goes in the comment.

## Pull requests from forks

The rules in a pull request become queries, sent to your Prometheus with
the job's credentials, whose results show in the comment; `RENDER` runs
what the pull request holds; and a pull request can change the job
itself. Run this for pull requests from people already trusted with what
the job can reach, never for a fork's with your metric credentials or on a
runner inside your network:

- **GitHub.** `pull_request` from a fork gets no secrets and a read-only
  token, so the job can't comment; leave it so. Never run this on
  `pull_request_target` with the pull request checked out: that hands the
  fork's code your secrets and a token that writes. The `if:` above skips
  forks. Self-hosted runners that reach your Prometheus: require approval
  for workflows from outside collaborators, and don't give them to public
  repositories.
- **GitLab.** A fork's merge request pipeline runs in the fork, without
  your project's variables. Running it in the parent project (a
  maintainer's "Run pipeline") hands it them: review the merge request
  first. A protected variable reaches only pipelines of protected
  branches, which a merge request's branch usually isn't: keep the
  backtest's variables unprotected and masked, the project's merge
  requests to its members, and runners that reach your Prometheus off
  pipelines of forks.
- **Bitbucket.** Keep secured variables and self-hosted runners from
  pipelines of forks.
- **Azure Pipelines.** Keep secrets and the self-hosted agents that reach
  your Prometheus from builds of forks.
- **Gitea and Forgejo.** A fork's pull request gets no secrets; keep it
  so, and register runners that reach your Prometheus to repositories that
  don't take forks' workflows.

## Safety

- What the pull request holds is untrusted, and the comment is read by
  everyone who can read the pull request. Lapilli writes rule names,
  labels and the reasons a rule couldn't be replayed as code spans, which
  no host reads as Markdown, HTML, a mention, a reference or a link. A
  query that failed as the history couldn't be read (anything but the
  history's own answer to the query as written: not a proxy's page, nor a
  gateway's) says only that in the comment, its reason going to the job's
  log; from the rest, Lapilli takes
  out where the history is: the `--prometheus`, `--loki`, `--grafana` and
  `--ruler` URLs, the user names in them and their hosts, each as a whole
  word, any IP address and port, the tenants and a `--gcp-project`. What
  went wrong, and what `RENDER` or `lapilli` said of it, goes in a fenced
  block, nothing of it outside: its last lines only, the same taken out
  before they are cut, with no backtick (which could end the block) and no
  control or format character (which could hide or reorder what is
  shown). Of what `lapilli` said, no line about why a rule couldn't be
  replayed, nor one with what a server or the network answered (`query
  failed: `, which begins every reason a query gives, `HTTP 403: ...`,
  `dial tcp`, a TLS error), nor a progress line: those are in the job's log
  alone, as the rules that couldn't be replayed are in the report. The path of a link refused is said only in the job's log.
- What the recipe copies to the job's log is each line after a `| `,
  control characters taken out and Azure's `##vso[` made inert: GitHub's
  and Gitea's `::` commands are read only at a line's start, so none of it
  is a command, an annotation or a mask.
- The token is read from a file and given to `curl` on stdin, never on a
  command line, which `ps` shows. `curl` reads no `.curlrc`, speaks only
  HTTP and HTTPS, follows no redirect from a host's API, and a release's
  download only to HTTPS. What it downloads is capped (16 MiB from a host,
  256 MiB for a release) as it streams, not only by `--max-filesize`,
  which before curl 8.4.0 (Ubuntu 22.04 has 7.81) caps only an answer that
  says its length first.
- Only a comment the token's own account posted, whose first line is the
  recipe's, is edited: a comment someone else posted with that line is
  left alone, and a deleted one, or GitLab's own note. When the host can't
  say who the token is, none is looked for, and a new one is posted. On
  GitHub, the job's own token can't ask: in GitHub Actions it is taken as
  `github-actions[bot]`; set `GITHUB_LOGIN` (the action's `login`) for a
  GitHub App's. On Bitbucket, set `BITBUCKET_USER_UUID`.
- Nothing a host gives is followed: comments are listed page by page by
  number (Bitbucket's `next` link says only that there is a next), and
  their ids checked to be numbers before they reach a URL.

Give the token what commenting needs, and no more: GitHub Actions'
`pull-requests: write` (a fine-grained token: pull requests, write);
GitLab, a project access token with the Reporter role and the `api` scope;
Bitbucket, a repository access token with pull requests (write); Azure
DevOps, the build service's "Contribute to pull requests"; Gitea and
Forgejo, `write:issue`, `read:repository` and `read:user`.
