# Changes as pull requests

With `lapilli serve --changes`, a change to a rule that a person approved in
Lapilli's UI is checked again (the rule read again, the backtest run again),
then handed to one of these scripts, which opens a pull request where the
rule is kept. The repository's own review and protections then apply, and
whatever applies the rest of the repository applies the change. Lapilli
holds no repository credentials: the script runs where they already are, a
CI job or a CronJob.

| Script | Host | Needs |
|---|---|---|
| `github.sh` | GitHub, GitHub Enterprise | `GITHUB_REPO`, `GITHUB_TOKEN_FILE`; `GITHUB_API`, `GITHUB_LOGIN` |
| `gitlab.sh` | GitLab | `GITLAB_PROJECT`, `GITLAB_TOKEN_FILE`; `GITLAB_API` |
| `bitbucket.sh` | Bitbucket Cloud | `BITBUCKET_REPO`, `BITBUCKET_TOKEN_FILE`; `BITBUCKET_API` |
| `gitea.sh` | Gitea, Forgejo | `GITEA_API`, `GITEA_REPO`, `GITEA_TOKEN_FILE` |
| `azure-devops.sh` | Azure Repos | `AZURE_DEVOPS_ORG_URL`, `AZURE_DEVOPS_PROJECT`, `AZURE_DEVOPS_REPO`, `AZURE_DEVOPS_TOKEN_FILE` |

Each also needs:

- `LAPILLI_URL`, where `--incidents-listen` is, and `LAPILLI_TOKEN_FILE`, a
  file holding the token in `--changes-token-file`. That token can read the
  changes approved for a pull request and say what became of them, and
  nothing else: it can't approve, apply, or touch anything else in Lapilli.
- `CLONE`, the repository, as `git clone` takes it. `git` pushes with the
  credentials it already has (a CI job's, a credential helper); a URL with
  credentials in it is refused, as `ps` would show them.
- `RULES_PATHS`, where in the repository the rules are: directories or
  files, space-separated (`alerts`, `rules/shop.yaml`). A change is written
  only to a rule file under one of them (below), and `RULES_PATHS` can't
  name what a CI system runs. What Lapilli sends isn't trusted to name the
  file, or to be rules: a Lapilli someone took over could otherwise push a
  workflow, a script or a build's configuration, which a push runs with
  the repository's secrets before anyone reviews it.
- `BASE`, the branch pull requests go into (default `main`); `GIT_NAME` and
  `GIT_EMAIL` for the commits (default `Lapilli`, `lapilli@localhost`).
- `MAX_CHANGES`, the most branches a run pushes (default 10): the changes
  past it wait for the next run, which the run says.

They need `sh`, `git`, `curl` and `jq`, and run every few minutes:

```sh
LAPILLI_URL=http://lapilli:8082 LAPILLI_TOKEN_FILE=/run/secrets/changes \
CLONE=https://github.com/example/monitoring.git RULES_PATHS=alerts \
GITHUB_REPO=example/monitoring GITHUB_TOKEN_FILE=/run/secrets/github \
  ./github.sh
```

What each run does:

1. Asks Lapilli for its changes. Those ready get a pull request; those with
   one open are looked at, and Lapilli is told when one was merged or
   closed. A merge isn't the change applied: it is applied when Lapilli
   reads the rules with it, as a new version, and the outcomes page measures
   it from then.
2. Finds the file the change was made against: the tracked file under
   `RULES_PATHS` whose sha256 is the hash of the document Lapilli read.
   Links are passed over. It must be a rule file, and so must the change
   Lapilli wrote, or nothing is written and Lapilli is told why:
   - a regular file, mode 100644 (never one that runs), named `*.yml`,
     `*.yaml`, `*.json` or `*.rules`;
   - not what a CI system or a tool a push sets off runs (`.github/`,
     `.gitlab-ci.yml`, `azure-pipelines.yml`, `Jenkinsfile`,
     `cloudbuild.yaml`, `buildspec.yml`, `.tekton/`, `Makefile` and the
     like), in any letter case, wherever it is;
   - holding, by the keys at the top of each document, a Prometheus rule
     file (`groups`, a list, and a ruler's `namespace`), a Grafana alerting
     provisioning file, a `PrometheusRule` (or Google Cloud's `Rules`), or a
     `ConfigMap`: what Lapilli edits, and no key a CI system's file needs.
     YAML that can be read in more than one way (a quoted, flow, merged,
     anchored or tagged key at the top, a directive, a key twice, a tab
     leading a line, a line break other than LF and CRLF) is refused.
3. Writes the change Lapilli wrote into it, commits it on a branch of
   Lapilli's own, `lapilli/LEDGER/proposal-ID`, LEDGER the ledger's name
   (`lapilli/proposal-ID` from a Lapilli before 0.28.1, which gives none),
   and opens a pull request with the evidence as its description, or finds
   the one it opened already: from the repository itself, never a fork,
   into `BASE`, opened by the account the token is. A branch is pushed
   only where there is none, never over one, which a pull request people
   reviewed, approved or set to merge may be from: one holding this very
   change is a run's that stopped, and its pull request is found or
   opened; one holding another change is left as it is, and Lapilli is
   told the change failed, and why. A `lapilli/proposal-ID` a recipe before
   0.28.1 pushed is taken when it holds this change.
   A pull request from a fork with the same branch name, into another
   branch, or opened by someone else is never taken as Lapilli's. When the
   host can't say who the token is, none is: the run opens its own. When
   the host refuses to open it, as one is open from the branch already (a
   run stopped after opening it, say), the one open from Lapilli's branch
   in the repository itself, into `BASE`, is taken, whoever opened it: a
   fork can't push to that branch.
4. Tells Lapilli its link, which the proposal keeps; a task the proposal
   was made into shows it on its page, through the proposal.

A document no file in the clone is, such as rules rendered from a Helm chart
or Kustomize, or read back from a cluster, gets no edit: Lapilli is told the
change failed, and why, and the proposal's page keeps the diff and a pull
request's description for a person to make it where the rules are written.
The same goes for a rule file mounted from a ConfigMap whose manifest is what
is in git: the file Lapilli read isn't a file in the repository.

## Safety

- Tokens are read from files and given to `curl` on stdin, never on a
  command line. An http or https `CLONE` with a user in it is refused, a
  token alone as the user (`https://TOKEN@host/...`) too.
- What Lapilli or the host answers is shown as JSON, so no escape sequence
  reaches a terminal or a CI log.
- Nothing a host or Lapilli gives is followed: a pull request is asked
  about by its number, checked to be one, at the API given; `curl` follows
  no redirects and speaks only HTTP and HTTPS.
- Ids, hashes and branch names are checked before they reach a command or a
  request; the file written is the one found in the clone by its hash, never
  a path Lapilli gives.
- Only a pull request the recipe opened is taken as a change's: from the
  repository itself, into `BASE`, by the token's own account (`GET /user`;
  Azure DevOps' `connectionData`), or, once the host refuses another from
  the branch, the one open from it there. On GitHub, a GitHub App's token
  can't ask: set `GITHUB_LOGIN` to the account it opens pull requests as,
  `NAME[bot]`.
- When the host fails, Lapilli is told nothing, and the next run tries
  again.

Give the host's token what opening pull requests needs, and no more: a
fine-grained token with pull requests (write) and contents (write) on the
one repository on GitHub; a project access token with the Developer role on
GitLab; a repository access token with pull requests (write) on Bitbucket;
`write:repository` and `read:user` on Gitea; Code (read and write) on Azure
DevOps. Protect
the base branch, so a pull request can't be merged without a review.
