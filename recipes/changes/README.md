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
  only to a file under one of them, and never to one a CI system runs
  (`.github/`, `.gitlab-ci.yml`, `azure-pipelines.yml`, `Jenkinsfile` and
  the like), wherever it is. What Lapilli sends isn't trusted to name the
  file: a Lapilli someone took over could otherwise push a workflow, which
  a push runs with the repository's secrets before anyone reviews it.
- `BASE`, the branch pull requests go into (default `main`); `GIT_NAME` and
  `GIT_EMAIL` for the commits (default `Lapilli`, `lapilli@localhost`).

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
   Links are passed over.
3. Writes the change Lapilli wrote into it, commits it on a branch of
   Lapilli's own, `lapilli/proposal-ID` (pushed again, with `--force`, if a
   run stopped half way), and opens a pull request with the evidence as its
   description, or finds the one it opened already: from the repository
   itself, never a fork, into `BASE`, opened by the account the token is.
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
  command line.
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
