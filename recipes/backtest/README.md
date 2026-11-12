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
| `bitbucket.sh` | Bitbucket Cloud | `BITBUCKET_TOKEN_FILE`; in Bitbucket Pipelines, nothing more |
| `azure-devops.sh` | Azure Repos | `AZURE_DEVOPS_TOKEN_FILE`; in Azure Pipelines, nothing more |
| `gitea.sh` | Gitea, Forgejo | `GITEA_TOKEN_FILE`; in Gitea or Forgejo Actions, nothing more; elsewhere `GITEA_API`, `GITEA_REPO`, `PR` |

Each takes the pull request's number, the repository and the API from its
CI's own variables; outside it, set them (each script's header lists
them). Each also needs:

- `LAPILLI_VERSION`, the Lapilli release to install, such as `v0.32.0` (one
  with `--markdown`). The archive for the machine is downloaded from the
  release and checked against the release's `SHA256SUMS` before it is
  unpacked; `LAPILLI_SHA256` pins its sum too, which a release replaced
  later can't match. `LAPILLI_RELEASES` downloads from a mirror holding the
  same files, over https. `LAPILLI` runs a `lapilli` already on the machine
  instead.
- Where the rules are: `PATHS`, files or directories in the repository,
  space-separated (`alerts`, `rules/shop.yaml`), as `--rules` reads them;
  or `RENDER`, a command that writes them on its stdout, run in each
  commit's files (below).
- The arguments: `lapilli verdict`'s flags for where the history is and
  over what period, as `lapilli verdict` takes them: `--prometheus URL` (or
  `--gcp-project`, `--auth`, `--bearer-token-file`), `--since 14d`, and any
  others, such as `--incidents incidents.yaml`, `--tz Asia/Seoul` or
  `--rules-select grafana_alert=1`. The recipe gives `--rules`,
  `--compare`, `--changed-only` and `--markdown` itself.

And, as wanted:

- `TARGET`, the commit the pull request goes into (default `HEAD^1`, the
  merge commit's first parent, as GitHub and Azure Pipelines check a pull
  request out); or `TARGET_BRANCH`, a branch to fetch from `origin`. The
  rules compared against are those of the commit `HEAD` and the target
  share last (`git merge-base`): what the pull request itself changes,
  whether the checkout is a merge or the pull request's own commit. Each
  script defaults it from its CI.
- `KEY`, to tell apart the comments of several jobs on one pull request,
  one per Prometheus say (default `default`).
- `FAIL_ON_ERROR=true`, to fail the job when the backtest fails (below).

They need `sh`, `git`, `curl`, `jq` and `tar`, on Linux or macOS.

## What a run does

1. Installs Lapilli, unless `LAPILLI` is given.
2. Reads the rules of the pull request's commit and of the commit it goes
   into from git, as git keeps them (`git archive`, so a checkout's
   `.gitattributes` neither leaves a file out nor runs a filter): those
   `PATHS` names, or all of each commit with `RENDER`. A path one commit
   hasn't is a directory with no rules, so a pull request that adds the
   first rules, or takes the last away, is compared too. A commit with a
   symbolic link where the rules are read (anywhere, with `RENDER`) isn't
   read: a link could lead to any file the job can read.
3. With `RENDER`, runs `RENDER_PREPARE`, then `RENDER`, in each commit's
   files, and takes what `RENDER` writes as that commit's rules.
4. Runs `lapilli verdict --rules PULL-REQUEST --compare TARGET
   --changed-only --markdown`, with the arguments given: only the rules
   that differ are replayed.
5. Finds the comment it posted before: the token's account's, whose first
   line is `[//]: # (lapilli-backtest KEY)`, a link reference definition,
   which no host shows. It edits that one, or posts one. When nothing that
   decides when a rule fires changed and there is no comment yet, it posts
   none; when there is one, it says that nothing changes now.

## In each CI

The recipe's two files (`lib.sh` and the host's script) are small: keep a
copy in the repository, from the tag of the release you install, or fetch
them at that tag in the job. On GitHub, the action needs neither. Run the
job on pull requests that touch the rules, on a runner that can reach your
Prometheus.

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
      - uses: lapilli-project/lapilli/recipes/backtest@v0.32.0
        with:
          version: v0.32.0
          paths: alerts
          prometheus-url: ${{ vars.PROMETHEUS_URL }}
          since: 14d
          args: --incidents incidents.yaml
```

`fetch-depth: 2` fetches the merge commit's parents. The action's inputs
are `version` (required: the tag it is used at), `sha256`, `paths` or
`render` and `render-prepare`, `prometheus-url`, `auth`, `gcp-project`,
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
    LAPILLI_VERSION: v0.32.0
    PATHS: alerts
    # A CI/CD variable of type File holding a project access token (role
    # Reporter, scope api): GitLab gives its path.
    GITLAB_TOKEN_FILE: $BACKTEST_TOKEN
  script:
    - apk add --no-cache git curl jq
    - sh ci/backtest/gitlab.sh --prometheus "$PROMETHEUS_URL" --since 14d
```

In a merge request pipeline the script takes the merge request, the
project and the API from GitLab's variables, and the target from
`CI_MERGE_REQUEST_TARGET_BRANCH_SHA` (merged results) or
`CI_MERGE_REQUEST_DIFF_BASE_SHA`; `GIT_DEPTH: 0` fetches the history they
are in.

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
            - LAPILLI_VERSION=v0.32.0 PATHS=alerts sh ci/backtest/bitbucket.sh --prometheus "$PROMETHEUS_URL" --since 14d
```

Bitbucket Pipelines clones the pull request's branch alone: the script
fetches the destination (`BITBUCKET_PR_DESTINATION_BRANCH`) itself.

### Azure Pipelines (Azure Repos)

A pull request into an Azure Repos branch runs the pipeline its branch
policy names (Build validation). Give the project's build service
"Contribute to pull requests" on the repository.

```yaml
steps:
  - checkout: self
    fetchDepth: 2
  - script: |
      AZURE_DEVOPS_TOKEN_FILE=$(mktemp)
      printf '%s' "$SYSTEM_ACCESSTOKEN" > "$AZURE_DEVOPS_TOKEN_FILE"
      AZURE_DEVOPS_TOKEN_FILE=$AZURE_DEVOPS_TOKEN_FILE LAPILLI_VERSION=v0.32.0 PATHS=alerts \
        sh ci/backtest/azure-devops.sh --prometheus "$PROMETHEUS_URL" --since 14d
      rm -f "$AZURE_DEVOPS_TOKEN_FILE"
    env:
      SYSTEM_ACCESSTOKEN: $(System.AccessToken)
      PROMETHEUS_URL: $(PROMETHEUS_URL)
```

The comment is a thread of its own with no status, which the policy that
comments be resolved doesn't wait for.

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
        with: {fetch-depth: 0}
      - run: |
          GITEA_TOKEN_FILE=$(mktemp)
          printf '%s' "$BACKTEST_TOKEN" > "$GITEA_TOKEN_FILE"
          GITEA_TOKEN_FILE=$GITEA_TOKEN_FILE LAPILLI_VERSION=v0.32.0 PATHS=alerts \
            sh ci/backtest/gitea.sh --prometheus "$PROMETHEUS_URL" --since 14d
        env:
          # A bot's token: write:issue, read:repository, read:user.
          BACKTEST_TOKEN: ${{ secrets.BACKTEST_TOKEN }}
          PROMETHEUS_URL: ${{ vars.PROMETHEUS_URL }}
```

Actions checks out the pull request's own commit there: the script fetches
the branch it goes into (`GITHUB_BASE_REF`), and compares from where they
parted.

## Rules rendered from a chart

`RENDER` runs in a copy of each commit's files, with `REPO` the clone's
full path, and writes the rules: a Helm chart's whole rendering, or a
Kustomize overlay's, of which only the documents of rules are read, the
rest passed over and counted. Render both commits with the same values
files and flags, so that only the pull request differs:

```yaml
      - uses: lapilli-project/lapilli/recipes/backtest@v0.32.0
        with:
          version: v0.32.0
          render: helm template monitoring charts/monitoring -f charts/monitoring/values-prod.yaml
          render-prepare: helm dependency build charts/monitoring >&2
          prometheus-url: ${{ vars.PROMETHEUS_URL }}
          args: --rules-select grafana_alert=1
```

`RENDER_PREPARE` runs first, for what a commit lacks (a chart's
dependencies); what it writes is shown, not read as rules. Each runs with
a `HOME` of its own and without the host's token's variable. A rendering
with no rules in it, as for a chart the pull request adds, is no rules on
that side.

## The comment

Lapilli writes it (`lapilli verdict --compare --markdown`, see the
[README](../../README.md#pull-request-backtests-in-ci)): the period and its
time zone, the totals before and after, the rules that differ as a table,
those that couldn't be replayed, those that differ but fire as they did
folded away, and with incidents, those caught differently. It is cut short
at 64,000 characters, saying so. When nothing that decides when a rule
fires changed, it is a heading and one line.

When something fails (the install, reading a commit, `RENDER`, the
backtest), the comment says what, with the last lines of what was said
about it, and the job succeeds: a backtest that can't run shouldn't hold a
pull request up, unless you ask it to. With `FAIL_ON_ERROR=true` the job
fails too, once the comment says why, and also when the comment can't be
posted. A setting that is wrong (no `PATHS`, a `TARGET` that isn't a ref,
a flag the recipe gives) fails the job at once, whatever it says.

## Pull requests from forks

The rules in a pull request become queries, sent to your Prometheus with
the job's credentials, whose results show in the comment; `RENDER` runs
what the pull request holds (a chart's templates, a Kustomize plugin); and
a pull request can change the job itself. Run this for pull requests from
people already trusted with what the job can reach, never for a fork's
with your metric credentials or on a runner inside your network:

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
  first. Mark the variables protected, and the runners that reach your
  Prometheus protected too.
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
  no host reads as Markdown, HTML, a mention, a reference or a link, and
  takes the `--prometheus`, `--loki`, `--grafana` and `--ruler` URLs, their
  hosts, and a `--gcp-project` out of those reasons. What `RENDER` or
  `lapilli` said when something failed goes in a fenced block, its last
  lines only, with no backtick (which could end the block), no control or
  format character (which could hide or reorder what is shown), and the
  same URLs, hosts and project taken out.
- The token is read from a file and given to `curl` on stdin, never on a
  command line, which `ps` shows; `RENDER` and `RENDER_PREPARE` run without
  the token's variable. They run what the pull request holds, which can
  read what the job can: that is why forks are kept out.
- Only a comment the token's own account posted, whose first line is the
  recipe's, is edited: a comment someone else posted with that line is
  left alone. When the host can't say who the token is, none is looked
  for, and a new one is posted. On GitHub, the job's own token can't ask:
  in GitHub Actions it is taken as `github-actions[bot]`; set
  `GITHUB_LOGIN` (the action's `login`) for a GitHub App's.
- Nothing a host gives is followed: comments are listed page by page by
  number (Bitbucket's `next` link isn't taken), and their ids checked to be
  numbers before they reach a URL. `curl` speaks only HTTP and HTTPS, and a
  release's download follows redirects to HTTPS only.

Give the token what commenting needs, and no more: GitHub Actions'
`pull-requests: write` (a fine-grained token: pull requests, write);
GitLab, a project access token with the Reporter role and the `api` scope;
Bitbucket, a repository access token with pull requests (write); Azure
DevOps, the build service's "Contribute to pull requests"; Gitea and
Forgejo, `write:issue`, `read:repository` and `read:user`.
