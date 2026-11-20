# Pull request backtests through serve: no metric credentials in CI

Status: accepted; steps 1, 2 and 3 built.
Merged on 2026-11-18, before its week was out, by the maintainer's
decision, with open questions 1, 5 and 6 decided, and 2, 3 and 4 on
2026-11-19 ("Open questions" below).
Under [GOVERNANCE.md](../../GOVERNANCE.md) this is a new kind of data in the
ledger (the backtests CI jobs ask for, kept a day), and a port that takes
input from outside with a token of a new kind, so it is designed first,
as a pull request, and reviewed as an attacker would
([CONTRIBUTING.md](../../CONTRIBUTING.md); "Security" below). The
companion to [expectations.md](expectations.md) (#81), whose first step
(#86, `--expect`) it carries as it is, and to
[recipes/backtest](../../recipes/backtest).

Step 1 is built: in serve, the port, the trust file, the lane and schema
023, and the chart's `serve.backtest`; in the recipes, their serve mode
(`LAPILLI_SERVE`) and the action's inputs (`serve-url`, `audience`,
`serve-wait`). Step 2 is built: Bitbucket Pipelines' ID token; Azure
Pipelines stays on the plain token. Step 3 is built: Grafana-managed rules
and a Loki ruler's, on the data sources the rules in effect read
already. Where the code had to do otherwise than this note says,
"As built (step 1)", "As built (step 2)" and "As built (step 3)" below
say so.

## Why

The backtest recipes run `lapilli verdict --rules PR --compare BASE
--changed-only --markdown` inside the CI job (`backtest()` in
`recipes/backtest/lib.sh`). Three things follow:

- **CI needs read access to the history.** For a Prometheus inside the
  network, that is a self-hosted runner there, which the recipes' README
  says to keep from forks. For a managed Prometheus, it is a cloud identity
  for the job: the README has it sign in first "with the cloud's own action
  through OpenID Connect (`id-token: write`)", which is a workload identity
  federation to set up in each cloud, for each CI, and a role that reads
  metrics given to whoever can run a job.
- **CI has no record.** Incidents come from `INCIDENTS`, a file in the
  repository read from the commit the pull request goes into, or from a
  file an earlier step wrote. Most set neither, and the comment says "No
  incidents were given: the firings are counted, not judged"
  (`verdict.CompareMarkdown`). It shows how often each rule fires, not what
  it catches; an expectation's catches are "not known" (#86).
- **Each job asks the history at its own pace.** `--max-rate` is the job's.
  Ten pull requests at once are ten jobs, each at its rate, against the
  quota serve's hourly runs need too.

`lapilli serve` already has each of these: the access (`--prometheus`,
`--auth`, `--bearer-token-file`, a tenant); one `prom.Limiter` for the
history, paced by `--max-rate` (`source.Prometheus.History`); the
incidents, the impact windows and the record kept by week; and the replay
of a change (`replayChange` in `internal/serve/work.go`), which change
reviews, proposals' backtests and MCP's `compare` use.

## What it is

A port of serve's own, `--backtest-listen`, that takes a pull request's
rules from a CI job that proves which repository it runs for, with the
CI's own short-lived ID token or, where the CI has none, a plain token;
replays them with serve's access, against serve's record; and gives back
the report `lapilli verdict --compare --changed-only --markdown` writes,
with the same summary line, and the JSON `--json` writes.

The recipes gain a mode, `LAPILLI_SERVE`, in which they send the rules
instead of installing and running Lapilli. Everything around it stays:

- The rules are read from git in CI, as `git archive` gives them, a
  symbolic link refused; `RENDER` and `RENDER_PREPARE` run in CI, and serve
  gets what they wrote. Serve runs nothing a pull request holds but its
  PromQL.
- The comment is found, edited or posted by the job, with the host's token
  (`post` in `lib.sh`). Serve holds no git host's token for this, and sends
  nothing to the host.
- `FAIL_ON_ERROR`, `FAIL_ON_EXPECTATIONS` and `EXPECT_REQUIRED` decide as
  they do, from the report's summary line (below, "How it relates to
  `--expect`").

## How it works

1. **CI reads both sides.** The recipe reads the rules of the commit the
   pull request goes into and of the pull request's (`files`, or `render`),
   and `EXPECT` from both commits as `expectations()` writes them (#86: a
   file the pull request takes away is sent as `expectations: []`).
2. **CI gets a token.** An ID token with serve's audience (below, by CI),
   or the plain token from its file.
3. **CI asks.** `POST /v1/backtests` on the backtest port, the token as a
   bearer token, the two sides as files in a JSON body (API, below).
4. **Serve checks who asks, before it reads the body** ("Who may ask").
5. **Serve reads the body and the rules.** Each side is read as `--rules`
   reads files (`rules.FromDocsContext`, with `--rules-select` when the job
   gives one, as `rules.ReadPicked` picks), then split into the rules alike
   and those that differ (`source.Split`), as `--changed-only` does. A
   request past the bounds is refused ("What the pull request's PromQL can
   do"). Its requests to the history are counted before anything is asked
   (the planner [preflight.md](preflight.md) proposes, #92, from `chunks` and
   `queries` in `internal/replay`); one that couldn't finish within
   `--backtest-timeout` at the lane's rate is refused, saying how many
   requests it needs and what period would fit.
6. **Serve queues it** in the ledger and answers 202 with an id and a key
   to read it by.
7. **The leader runs it**, the oldest first, one at a time: both sides
   replayed over the period by `replayChange`, judged against the ledger's
   incidents and impact windows by the ledger's weeks as change reviews are
   (`recordAround`, `verdict.Options.RecordWeek`), summarized
   (`Comparison.Summarize`), held to the expectations sent
   (`Comparison.Expect`, #86), the reasons rules couldn't be replayed
   scrubbed of where the history is (today `forComment` in
   `cmd/lapilli/verdict.go`, which moves to `internal/verdict` so that both
   call it), and written as `verdict.CompareMarkdown` and `verdict.JSON`
   write them.
8. **CI reads it.** `GET /v1/backtests/{id}` with the key, every 10 seconds
   until it is done; the recipe writes the Markdown where `lapilli verdict`
   would have (`$tmp/report.md`), and `compose`, `post` and the job's exit
   are as they are.

**The period** is `days`, 1 to 14 (7 when not given, the recipe's `since`
default), ending at now less `--settle`, on `--step`, as MCP's `compare`
takes it (`toolCompare`). The recipe turns `--since 14d` into it, and
refuses `--from` and `--to`: serve replays only what has settled.

**What serve decides, not the job.** Where the history is and how it is
asked (`--prometheus`, `--auth`, a tenant, `--step`, `--late`,
`--scrape-interval`), and how firings are judged (`--lead`, `--match`,
`--refire`, the noisy thresholds as the UI keeps them, `--sleep-hours`,
`--shifts`). The recipe refuses those flags in serve mode: a pull request
doesn't choose the judging that flatters it. It passes `--tz`, which only
shows times, and `--rules-select`.

## Who may ask

The port takes nothing without a token, and the token says which entry of
`--backtest-trust` it is. The file is read strictly at start, as `--config`
reads its file; but one that can't be used stops serve from starting, where
`--config` starts on the ledger as it is: a trust file half read could let
in whom it shouldn't.

```yaml
trust:
  - name: alerts                 # what the log, /metrics and Run history call it
    issuer: https://token.actions.githubusercontent.com
    audience: https://lapilli-ci.example.com
    claims:                      # each must be exactly this, as a string
      repository_id: "123456789"
      event_name: pull_request
    days: 14                     # the most one of its backtests replays
    hourly: 30                   # backtests it may start in an hour
  - name: monitoring-gitlab
    issuer: https://gitlab.example.com
    ci: gitlab                   # a GitLab of one's own isn't known by its URL
    audience: https://lapilli-ci.example.com
    claims:
      project_id: "42"
      pipeline_source: merge_request_event
  - name: monitoring-bitbucket
    issuer: https://api.bitbucket.org/2.0/workspaces/example/pipelines-config/identity/oidc
    audience: https://lapilli-ci.example.com
    claims:                      # UUIDs as Bitbucket writes them
      repositoryUuid: "{1a2b3c4d-2222-4333-8444-5555555555ee}"
      workspaceUuid: "{4b1d9c2e-7a3f-4e5b-8c6d-0e1f2a3b4c5d}"
  - name: monitoring-azure
    token_file: /etc/lapilli/backtest/azure-token   # a plain token
```

### An ID token

Checked as `internal/signin` checks a bearer token (`OIDC.Bearer`,
`verify`), against the entry whose `issuer` is the token's `iss`, byte for
byte:

- its signature, with a key from the issuer's JWKS, found through its
  discovery document, by an algorithm of a key pair (`signin.Asymmetric`):
  `none` and HMAC never;
- `aud` holding the entry's `audience`;
- `exp`, `nbf`, and `iat` not in the future nor older than an hour, whatever
  `exp` says (GitLab's lasts the job's timeout, Forgejo's an hour unless
  set otherwise);
- every claim the entry pins, compared as a string, a number by its
  decimal;
- `jti`, or the token's hash when it has none, not used before to start a
  backtest with another body (below, "Security").

`audience` is required, and should be the backtest port's URL: never a
CI's default audience, which other services that trust the same CI take
too (GitHub's default is the repository owner's URL). A token made for a
cloud's federation then can't be sent here, nor one made here sent there.

An entry of a known issuer must pin the claim that names the repository by
its id, or serve refuses to start; a generic issuer must pin `sub` or
another claim. Ids, not names: a repository deleted and made again under
its name keeps the name, not the id, and a renamed one keeps its id
(Forgejo's documentation says so of its id claims).

The claim that names the repository may be a list, for an entry that
trusts several repositories of the same CI alike: `repository_id: ["123",
"456"]`, each compared exactly, one of them enough. There are no patterns,
and no entry trusts an organization whole: `repository_owner_id` may be
pinned beside the repository's id, to narrow it, never instead of it (open
question 6, decided). Every other pinned claim is one value.

**By CI**, as their documentation says:

- **GitHub Actions.** Issuer `https://token.actions.githubusercontent.com`
  (GitHub Enterprise Server's own). The job has `permissions: id-token:
  write` and asks with `ACTIONS_ID_TOKEN_REQUEST_URL` and `&audience=`
  (the action does it, as `core.getIDToken(audience)` would). Pin
  `repository_id`, and `event_name: pull_request`; `repository_owner_id`
  and `runner_environment` (`github-hosted`, `self-hosted`) if wanted.
  `ref` is `refs/pull/N/merge` for a pull request, which is where the pull
  request's number is taken from. `workflow_ref` names the workflow file
  at the pull request's own ref, which the pull request can change, so
  pinning it keeps out no one who can push a branch.
- **GitLab CI.** Issuer the instance's URL (`https://gitlab.com`). The job
  names a token under `id_tokens:` with `aud:`; it expires with the job's
  timeout, or after five minutes. Pin `project_id` and `pipeline_source:
  merge_request_event`. In a merge request pipeline `project_id` is the
  merge request's source project's; `job_project_id`, the project the job
  runs in, is the parent's when a maintainer runs a fork's pipeline there,
  so it isn't the one to pin.
- **Bitbucket Pipelines.** One issuer for each workspace,
  `https://api.bitbucket.org/2.0/workspaces/WORKSPACE/pipelines-config/identity/oidc`,
  WORKSPACE its slug, as the repository's OpenID Connect settings show it;
  its keys at the same origin, each workspace's its own. The step has
  `oidc:` with serve's audience under `audiences:`, and the token is
  `BITBUCKET_STEP_OIDC_TOKEN`. Pin `repositoryUuid`, and `workspaceUuid`,
  each as Bitbucket writes it, lowercase in braces. No claim says the step
  is a pull request's, nor which; the token names the workspace's own
  audience beside the step's ("As built (step 2)").
- **Forgejo Actions.** Issuer `URL/api/actions`. The workflow sets
  `enable-openid-connect: true`, and asks with
  `ACTIONS_ID_TOKEN_REQUEST_URL` and an audience. Pin `repository_id` and
  `event_name: pull_request`; `sub`'s form changed in Forgejo 16, and pins
  of it would break.
- **Gitea Actions.** No ID token in the documentation read for this note
  (a proposal, go-gitea/gitea#33681): a plain token.
- **Azure Pipelines.** A job's ID token is for a service connection,
  made for Microsoft Entra's token exchange, its audience
  `api://AzureADTokenExchange`, which the job doesn't choose. A new
  service connection's is issued by Entra itself, the tenant's
  `https://login.microsoftonline.com/TENANT/v2.0`, an issuer every app of
  the tenant shares; the older Azure DevOps issuer,
  `https://vstoken.dev.azure.com/ORGANIZATION-ID` (subject
  `sc://ORG/PROJECT/CONNECTION`), retires on 2027-07-01. A plain token
  (open question 3).

The keys are fetched as `signin.Keys` fetches them ([signin.md](signin.md),
"Keys, fetched sparingly"): from the discovery document's JWKS alone,
never a URL a token names; once a minute at most for a key not held; again
after six hours; one fetch at a time, ten seconds, no redirect, 1 MB and
100 keys. The discovery document's `issuer` must be the entry's to the
byte, and its endpoints https.

### A plain token

`token_file:`, for a CI with no ID token (Gitea, Azure Pipelines, Jenkins,
anything): 32 characters at least, read for each request, both hashed and
compared in constant time, and refused while it is the same as another
token serve takes, as `tokenOK` refuses the incidents port's. It is the
long-lived secret this design is otherwise rid of: whoever holds it
backtests as that entry, from anywhere that reaches the port, until it is
rotated.

## Which port

- **Not the UI's.** With serve's own sign-in, `--oidc-bearer` takes ID
  tokens the provider gave Lapilli's client, for people, with roles; a CI's
  token is from another issuer and names no person. Taken there, a
  repository would be a person with a role, and behind a sign-in in front
  (`--user-header`, `--proxy-jwt`, an identity-aware proxy) a runner isn't
  let through at all.
- **Not `--incidents-listen`.** It is for jobs inside the network, each
  with a token of its own (`IncidentsHandler`: incidents, firings, tasks,
  changes), which a NetworkPolicy lets in. Opened to a CI provider's
  runners, it would open those paths with it.
- **Its own: `--backtest-listen ADDR`**, with nothing else on it, served as
  the other ports are (`ReadHeaderTimeout` 10 s, `ReadTimeout` 30 s,
  `MaxHeaderBytes` 16 KiB, in `Run`, `internal/serve/serve.go`). It checks
  no host name, as the notifications port doesn't: every request carries
  its own proof, which a browser doesn't add of itself. A request with an
  `Origin` is refused, as `/mcp` refuses one from a page elsewhere.

### Reaching it

Where this fits:

1. **Runners inside the network** (self-hosted runners in the cluster, or
   a network that reaches it): the port on a ClusterIP or an internal load
   balancer, nothing new exposed. What is gained is no metric credential on
   the runner, the record, and one quota.
2. **A CI provider's own runners, with the port exposed on its own**
   (`serve.backtest.ingress`, TLS at the Ingress or the load balancer),
   apart from the UI and whatever signs people in to it. The port is then
   on the internet, and "Security" below is what it does there.

Where it doesn't:

- **Serve reached only through an identity-aware proxy, a VPN or a
  private network, and a provider's runners.** The runner can't pass the
  proxy without the proxy's own credential: a Google service account's ID
  token for IAP, an access service's token. That is a cloud identity or a
  long-lived secret again, which this design was to save. There, keep the
  recipe as it is, or a runner inside.
- **Letting in runners by address.** GitHub-hosted runners come from the
  many ranges GitHub publishes in its meta API, which change; that is no
  fence. Runners with fixed addresses (self-hosted, or a provider's larger
  runners with them) can be let in so, beside the token.

## What the pull request's PromQL can do

What runs is the expressions of the rules that differ, as range queries,
with serve's access to `--prometheus` (and, of a Grafana rule on a Loki
data source or a Loki ruler's rule, to the Loki the rules in effect read,
"As built (step 3)"), not the job's. They can read any series that access
can, and the firing counts in the comment say what they read, a count at
a time (a rule `secret_series > 41` fires or doesn't).
That is what `--mcp-compare` lets an agent do ([serve.md](serve.md), "That
PromQL can learn, a count at a time, whatever that access can read"), and
why both are off unless given: no `--backtest-trust`, no port. Give serve's
history credential what the rules need and no more (one tenant, the
metrics' project).

Bounded, as MCP's `compare` is where it has a bound:

- **Prometheus-format rules**, replayed against `--prometheus`; and, from
  step 3, Grafana-managed rules and a Loki ruler's, only on the data
  sources the rules in effect read already (open question 2, decided): a
  Grafana rule each of whose queries names, by its uid exactly, a data
  source a rule in effect's replay asks, and as it asks it; a Loki
  ruler's rule only where serve has `--loki` and its rules in effect hold
  one. In the first step they were refused: Grafana's Loki queries are
  asked through Grafana's data source proxy with serve's Grafana token, of
  any data source that token reads, and Loki's with serve's Loki token.
- **50 rules that differ**, on either side; a `for` or `keep_firing_for` of
  48 hours at most and a group interval of an hour (`compareFor`,
  `compareEvery`); 1 to 14 days, and the entry's `days`.
- **Each read as the runs' are**: `--max-samples`, 50,000 series and 256
  MiB of an answer, 100,000 series and 256 MiB of labels in all
  ([serve.md](serve.md), "What a history API answers"), and a replayed
  alert's labels kept as every other's (`verdict.KeptLabels`).
- **Replayed as the rules in effect are**, with `--late` and
  `--scrape-interval` (`replayedAs`); a tenant is serve's (`X-Scope-OrgID`),
  never the pull request's. `--impact` isn't asked: the impact windows the
  ledger has are what judges.
- **Recording rules** that differ are said and not replayed, as `--compare`
  says them; the alerts that read them read what they recorded.

**Forks.** Who can get a token is who can run a job in the repository for
the event pinned, and the pins keep forks out:

- GitHub gives a fork's `pull_request` run read-only permissions, `id-token`
  among them, so it gets no ID token; unless an admin has turned on "Send
  write tokens to workflows from pull requests", and then a fork's run gets
  one no claim tells apart (`head_ref` is a branch name). Keep it off.
  `pull_request_target` runs in the base's context and gets a token; the
  pinned `event_name` refuses it.
- GitLab names a merge request pipeline's source project in `project_id`,
  so a fork's merge request carries the fork's, and is refused, run in the
  parent project too (its `job_project_id` would be the parent's).
- Bitbucket says "Pull requests from a forked repository don't trigger the
  pipeline" (its pipeline start conditions): a fork's pipelines run in the
  fork, another repository (`repositoryUuid`) and, in another workspace,
  another issuer with keys of its own, which the entry's issuer and pins
  refuse.
- Forgejo's reference says its OpenID Connect is off for a fork's
  `pull_request` ("As built (step 1)"); pin the repository, and keep
  OpenID Connect from forks' pipelines as the recipes' README keeps
  secrets from them.
- Azure Pipelines, not taken (open question 3), doesn't give a build of a
  pull request from a fork a service connection's credentials, unless
  "Make secrets available to builds of forks" is on, as it is by default
  for GitHub Enterprise Server's pipelines.

A contributor from outside is backtested once a maintainer, having read the
rules, pushes the branch into the repository, as with any job that holds a
credential today.

## Queueing and limits

A burst of pull requests mustn't hold up the hourly runs, nor use the quota
they need:

- **One lane, on the leader.** The leader runs the queue, one backtest at a
  time, beside the worker and not in it: a run waits for no backtest, and a
  backtest for no run. It doesn't take the `comparing` turn (the
  per-process lock MCP's `compare` and proposals' backtests take), which
  would refuse an agent's comparison on the leader for as long as a
  backtest runs: so the leader holds two replays at once at most, beside
  the runs' own, and its memory is sized for it
  ([preflight.md](preflight.md) says how much).
- **A share of the rate.** The lane asks the history through a Limiter of
  its own, paced at `--backtest-share` of `--max-rate` (a quarter by
  default) and by the pace the runs' Limiter keeps too, so the two together
  never pass `--max-rate`, and the runs always have the rest. Today a
  `prom.Pace` can be shared among Limiters (`NewLimiterPaced`), and a
  Limiter keeps one; the lane's needs two. Without `--max-rate`, the lane
  has a quarter of `--parallel` in flight, one at least, and backs off on
  its own when the API pushes back (HTTP 429), as each Limiter does.
- **A queue.** 20 waiting at most (`--backtest-queue`), 3 of one entry, and
  one of a pull request: a newer commit's takes the place of the one
  waiting, which is answered as superseded. Past them, 429 with
  `Retry-After`.
- **An hour.** An entry's `hourly`, 30 unless given, from a bucket every
  replica shares (`ledger.Take`, as the token ports' rates are shared,
  `shareLimits`).
- **Time.** `--backtest-timeout`, 15 minutes by default, counted from when
  it starts; one refused before it is queued when it couldn't finish in it
  (step 5). The recipe waits `SERVE_WAIT` at most (30 minutes), then says
  the backtest didn't come back, which is `trouble` in `lib.sh`.

**What it costs.** Ten rules that differ, over 7 days at a 1-minute step:
each side asks each expression for the 8 or so days its reads fall on (a day's
read at most, on days from midnight UTC, `chunks`), 80 requests a side, 160
in all; a change of `for` alone (5m to 10m) asks for the same expressions
on both sides, which share the reads their `for` doesn't move (`RunEach`):
each rule's first read starts where its `for` says, so 90. At `--max-rate
0.5` with a quarter for the lane, 0.125 a second, 160 requests take some
21 minutes, past `--backtest-timeout`'s 15: the backtest is refused before
it waits, saying that 4 days would fit. The change of `for` alone, 90
requests, takes some 12. A self-hosted Prometheus with no `--max-rate` answers either in
seconds, as the job does today. Under a quota, a backtest through serve is slower than one
in CI at the job's own rate: that is the price of one quota and no
credential in CI, and the job's log says where it is in the queue.

## Where it shows

- **On the pull request**, as today: the comment the job posts, whose
  heading says it was judged by serve's record ("How it relates to
  `--expect`").
- **In the job's log**: where the backtest is in the queue, and, while it
  runs, its requests asked of those counted, every 30 seconds, as
  `--progress` says a run's.
- **In serve**: a line of the Run history page for each, and two series in
  `/metrics` (below). Not on a rule's page, the overview, the digest, the
  outbox or MCP: a pull request's backtest is a prediction for its
  reviewers, not a fact of the deployed rules.

## What is kept

- **A table, `backtests`** (schema 023, the next after 022 when this was
  merged; whichever pull request merges first takes a number). A row is
  a backtest asked: its id (random, so that ids don't count backtests), the
  trust entry, the repository as the token's claim names it (the entry's
  name for a plain token), the pull request's number from GitHub's `ref`
  claim or as the job sent it, the commit sent, its state, when it was
  asked, started and ended, the requests it made, a hash of its key, the
  files and expectations sent (gzipped, 4 MiB at most) until it ends, and
  the result (the Markdown and the JSON, gzipped, 1 MiB at most). A row is
  deleted a day after it ends; 200 rows at most, the oldest ended first.
- **Why the ledger.** With replicas, a job's next read reaches any of them,
  and only the leader runs the lane; a backtest queued when a leader ends is
  run by the next, as its other jobs are.
- **Nothing else.** No firings, stones, versions, change reviews or
  proposals; nothing in the ledger is judged again; outcomes don't count it
  as use. The Run history page lists each: when, the entry, the repository,
  how many rules differed, the requests, how long, and how it ended; no rule
  names.
- **`/metrics`**: `lapilli_serve_backtests_total{trust,result}` and
  `lapilli_serve_backtests_waiting`, by the entry's name, which whoever
  runs serve wrote: never a repository, a branch or a rule.

## Flags and API

| Flag | Default | |
|---|---|---|
| `--backtest-listen ADDR` | off | the port; needs `--backtest-trust` |
| `--backtest-trust FILE` | | who may ask (above), read at start |
| `--backtest-share F` | 0.25 | of `--max-rate`, the lane's; 0.05 to 0.75 |
| `--backtest-queue N` | 20 | backtests waiting, at most |
| `--backtest-timeout DUR` | 15m | one backtest's time, from its start |

Refused: without rules to replay (`--alerts`, firings copied in alone), as
MCP's `compare` refuses them; under `--read-only`, which keeps the queue's
writes out. The chart: `serve.backtest.enabled`, `.trust` (a ConfigMap),
`.share`, `.service` and `.ingress` of their own, apart from the UI's;
exposed outside the cluster only with TLS.

**`POST /v1/backtests`**, the token as a bearer token, a JSON body of 4 MiB
at most, read strictly:

```json
{
  "before": [{"path": "alerts/api.yaml", "content": "groups: ..."}],
  "after":  [{"path": "alerts/api.yaml", "content": "groups: ..."},
             {"path": "logs.yaml", "content": "groups: ...", "loki": true}],
  "expect": "expectations: ...",
  "expect_before": "expectations: ...",
  "rules_select": "grafana_alert!=1",
  "days": 7,
  "tz": "Asia/Seoul",
  "pull_request": 123,
  "commit": "0123abcd..."
}
```

Answers: 202 `{"id", "key", "position", "requests"}` (the key shown once);
200 with the same id when the same token sends the same body again, a job
asked twice; 400 with what is wrong (the rules, a bound, the period); 401
for a token this port doesn't take, with `WWW-Authenticate: Bearer` and no
word of why (the log says); 409 for a token that started another backtest;
413; 429 with `Retry-After`; 503 while the rules haven't been read, or no
run is recorded yet.

**`GET /v1/backtests/{id}`**, the key as the bearer token: 202
`{"state": "waiting", "position"}` or `{"state": "running", "requests",
"asked"}`; 200 `{"state": "done", "markdown", "report"}` or
`{"state": "failed", "error"}`, the error in the words `trouble` puts in a
comment, nothing of where the history is; `superseded`. An unknown id and a
wrong key are the same 404, once a listing of the ledger's backtests begun
after the request came doesn't know them (since 0.63.2); past the
address's 20 waiting, and when no listing ends within 2 seconds, 503 with
`Retry-After`, so that a job's first poll through another replica than
the one it asked is never said not found. **`DELETE /v1/backtests/{id}`**, with the key:
a job cancelled stops waiting, or running.

The recipe: `LAPILLI_SERVE` (the port's URL, https, or http to the machine
itself as the recipes take APIs) and `LAPILLI_SERVE_AUDIENCE`; the token
from the CI (`LAPILLI_ID_TOKEN`, or `LAPILLI_SERVE_TOKEN_FILE`), sent to
`curl` on stdin as the host's token is. The action: `serve-url`, `audience`,
and `permissions: id-token: write`. `LAPILLI_VERSION` isn't needed: nothing
is installed.

## How it relates to `--expect`, and expectations' steps 2 and 3

**There is one gate, and this isn't it.** Step 1 (#86) is the gate:
`lapilli verdict --compare --expect FILE --expect-before FILE` holds the
rules that differ to the expectations (`verdict.ReadExpectations`,
`Comparison.Expect`), and writes the summary's `expected`,
`expectations_broken`, `expectations_already_broken` and `unexpected`; the
recipes' `FAIL_ON_EXPECTATIONS` and `EXPECT_REQUIRED` decide from that
summary line, in `lib.sh` (`judged`, `unmet`), not from Lapilli's exit
status. This design moves where that comparison runs, and nothing else:

- The recipe sends `expect.head.yaml` and `expect.base.yaml` as
  `expectations()` writes them now, and serve calls the same
  `ReadExpectations` and `Comparison.Expect` with them, on the same
  `Comparison`. The summary line comes back the same, and the recipe's gate
  reads it as it reads Lapilli's.
- A backtest through serve that can't be judged (refused, superseded, timed
  out, serve out of reach) is `trouble` in the recipe, and with
  `FAIL_ON_EXPECTATIONS` the job fails as one whose expectations "couldn't
  be judged", closed, as #86's second review made it.
- `--fail-expectations` and `--expect-required` aren't sent: the recipe
  decides, as it does today.

**What reads differently through serve**, and is said so in the comment's
heading ("judged by lapilli serve's record"):

- Catches are judged against serve's incidents and impact windows: a catch
  "not known" in CI with no `INCIDENTS` can be met or broken through serve.
- Firings are judged by serve's weeks (`RecordWeek`) and its settings, as
  its change reviews are, where `lapilli verdict` judges each by the two
  weeks before it; the same pull request can read differently through
  serve than in CI with a file of the same incidents.
- "While people slept" is by serve's `--sleep-hours` and `--shifts`, and
  the rule's team's own hours where it has them (since expectations' step
  2), not the job's flags.
- Scenarios (expectations' step 4, promtool test files) aren't run: a
  request carries no test file, and serve runs none a pull request names,
  whatever its own `--promtool`. Each is said as not run through lapilli
  serve, and the recipe refuses `PROMTOOL` with `LAPILLI_SERVE`.

**Step 2** (serve's checks after deploy, `expectation_checks`, a schema of
its own)
answers another question: whether a deployed rule keeps to what is expected
of it, week by week. A backtest through this port writes no
`expectation_checks`, and doesn't read serve's own expectations
(`--expect`, `--config`, step 2): the pull request's file is what is being
tested, against the rules the pull request changes. Neither gates the
other.

**Step 3** (drafts; MCP's `test_expectation` and `propose_expectation`;
agents' rewrites on a failed pull request) has an agent ask serve, over
MCP, for a person. This port is for a CI job, and is one more door to the
same work, not a second engine: MCP's `compare`, `test_expectation` and
this port call one function in serve, the replay of a change held to its
expectations (`replayChange`, then `Comparison.Expect`), so a rule's budget
is counted one way whoever asks. The port proposes nothing, keeps no draft,
and passes nothing: a rewrite an agent suggests is backtested the same way,
and the recipe's gate still decides. As built (expectations' step 3),
`test_expectation` replays the rule, and its rewrite, as the port does
(`replayCompared`, its firings kept) and holds them with
`CheckExpectationsBy`, as the port does, by whole weeks back from the
period's end; `compare` still calls `replayChange`, as it holds nothing.

**Order.** It is built once step 1 is merged, and needs nothing from steps
2 and 3; its schema is the next free one when its pull request opens.

## Security: as an attacker reviews it

What can someone send before they prove who they are?

- **Read.** The request line and headers, 16 KiB at most within ten
  seconds; never the body, whose read begins once the token verifies (the
  lesson of GHSA-ggwv-m6g7-jchx).
- **Parsed.** The token's header (`alg`, `kid`) and claims (`iss`), only to
  pick an entry: an `iss` no entry names is 401, with nothing fetched. Then
  one signature check by a key held.
- **Fetched.** Only the discovery document and JWKS of an entry's issuer,
  once a minute at most for a key not held: a made-up `kid` costs one fetch
  a minute an issuer, whatever the number of requests.
- **Told.** 401 with no word of which check failed; the log says, once a
  minute for each entry. `GET` on an id says the same 404 for an id that
  isn't and a key that's wrong, compared in constant time.

How much can a holder make Lapilli read, fetch, keep or judge?

- **Read**: 4 MiB a request, its rules read within the bounds
  `internal/rules` holds a rules input to (0.44.1).
- **Fetch**: the range queries above, in the lane's share of the rate, one
  backtest at a time; nothing from a URL in the body. Of Grafana, only the
  Loki data sources the rules in effect read through its data source proxy,
  by uid, and of Loki only `--loki`, with serve's tenant, where the rules
  in effect read it ("As built (step 3)"); never Grafana's API for what a
  data source is.
- **Keep**: a row a backtest, a day; the queue's and the hour's bounds.
- **Judge**: nothing in the ledger again.
- **A token stolen** from a job's log can start backtests until it is an
  hour old, and only with the body it first came with; it can't read any
  backtest but its own (each has its key). A plain token stolen works until
  it is rotated.

Where do the names they choose end up? A rule's name, group, labels and
expression, an expectation's `rule:` and `group:`, a catch's incident id,
a file's path:

- in the Markdown, written as code spans as `CompareMarkdown` writes them,
  where no host reads Markdown, HTML, a mention or a link; in the JSON,
  escaped;
- in the `backtests` row for a day, as data;
- nowhere else: not in serve's log (a line a backtest, with counts), the
  pages (Run history has counts and the repository), `/metrics`, Slack,
  the outbox, nor MCP.

A token's claims are the issuer's, not the pull request's; the repository
named is kept as a name a proxy gives is ([signin.md](signin.md): over 200
bytes, or with what doesn't show, it is no one's). A branch name
(`head_ref`) is the pusher's choice, and isn't kept.

The paths for the review: the port's handler (`backtestHandler`), the
token's check (`internal/signin`, `Keys`, the claims' pins), the body's
read, the planner's refusal, the lane's Limiter, the queue's writes, and
the recipe's serve mode in `lib.sh` (what it sends, and that the token goes
on stdin). From step 3: what the rules in effect read (`readsInEffect`),
what a pull request's Grafana and Loki rules are held to (`taken`,
`checkGraph`, `checkLogQL`, `rules.LogQLText`, `rules.LogQLWide`,
`rules.LogQLReach`), and the lane's Lokis (`laneLogs`).

## Alternatives

- **Use what exists.**
  - *The recipe as it is*, with the cloud's own sign-in in CI: works
    wherever CI can sign in; the record only from a file.
  - *MCP's `compare`*: replays changed rules beside the rules in effect,
    with serve's access and record. It goes as far as an agent acting for
    a person: a role (`--mcp-compare editors|admins`) through the UI's
    sign-in, which a CI job doesn't have; 25 seconds and one at a time,
    which a quota doesn't allow; "before" is serve's rules in effect, not
    the commit the pull request goes into; 20 rules given; no Markdown, no
    summary line, no expectations.
  - *The incidents API*: a CI job could read `GET /api/v1/incidents` and
    run `lapilli verdict` with them, and have the record; but that API is
    on the UI's port, behind its sign-in (`--incidents-listen` takes
    incidents in, and gives none out), and the job still needs a metric
    credential.
- **Serve posts the comment itself.** It would need every git host's token,
  and to send to hosts no one told it of. No.
- **One request that waits for the backtest.** A load balancer ends a
  request long before a quota-paced backtest ends (Google Cloud's backend
  service timeout is 30 seconds unless set), and a replica the next read
  reaches wouldn't have it.
- **The queue in memory.** No schema, but one replica only, and a restart
  loses what waits.
- **Long-lived tokens for everyone.** The fallback is there; an ID token is
  better wherever the CI gives one.

## Not in this design

- A data source no rule in effect reads, of Grafana's or a Loki's.
- Forks' pull requests.
- A check run or commit status of the pull request's own (ROADMAP.md, "Pull
  request backtests").
- `RENDER` in serve.
- Keeping a pull request's backtest as a prediction its merge is measured
  against on `/outcomes`: today the reviews of versions are.

## Three cases

1. A team on a managed Prometheus has no workload identity for CI. It
   exposes the backtest port at `lapilli-ci.example.com`, trusts its
   repository by id, and drops the cloud sign-in from the job. A pull
   request that raises a threshold gets the comment it got before, and now
   which incidents the rule would no longer catch.
2. Twelve pull requests open in an hour, from a bulk change of `for`s. The
   first runs; the rest wait, a newer commit taking an older one's place;
   the hourly runs keep three quarters of the quota, and the queue drains
   in about an hour.
3. An expectations file says a rule must catch INC-43 within 5 minutes. In
   CI with no `INCIDENTS` that was "not known"; through serve, INC-43 is in
   the ledger, the change misses it, and `FAIL_ON_EXPECTATIONS` fails the
   job.

## Open questions

1. **Forks.** Never; or a maintainer's label, with `pull_request_target`
   reading the fork's files as data and no `RENDER`? **Decided on
   2026-11-18, by the maintainer: never.** A fork's pull request is not
   backtested through serve; the pins keep it out ("Forks" above).
2. **Grafana and Loki rules.** Taken only on the data sources the rules in
   effect already use, by uid? **Decided on 2026-11-19, by the
   maintainer: take them, on those data sources alone.** A pull request
   must not have serve read, with serve's credentials, anything serve
   doesn't read already for the rules it runs. A Grafana rule is taken
   only when every data source its queries name (by `datasourceUid`, its
   expressions, `__expr__`, aside) is one a Grafana rule in effect asks
   through serve, matched by uid exactly, never by name or type: a uid
   serve's Grafana token can read but no rule in effect asks is refused. A
   Loki ruler's rule is taken only where serve has `--loki` and its rules
   in effect hold one, read with serve's tenant, never the pull request's.
   Every bound the Prometheus path has applies alike, and the refusals
   name nothing of serve's: a data source refused is one no rule in effect
   reads. Built in step 3 ("As built (step 3)").
3. **Azure Pipelines' ID token.** Take it, for a service connection made
   for Lapilli alone and trusted by no identity in Entra; or keep the plain
   token? **Decided on 2026-11-19, by the maintainer: the plain token.**
   A service connection made now has its token issued by the Microsoft
   Entra tenant, an issuer every app of the tenant shares, so only `sub`
   would tie it to the connection; its audience is Entra's token exchange,
   which the job can't choose; the older Azure DevOps issuer retires on
   2027-07-01; and there is no account to check any of it against. To
   look at again once it can be checked against a real account. (A first
   decision the same day, to take it unless its issuer were one a whole
   tenant shares, met that condition.)
   Microsoft's documentation, read on 2026-11-19:
   - New workload identity service connections use the Microsoft Entra
     issuer by default, `https://login.microsoftonline.com/TENANT/v2.0`,
     their subject `<Microsoft Entra prefix>/sc/ORGANIZATION-ID/CONNECTION-ID`
     (an `/eid1/...` form, in what others have shown of it), and a token
     that "can only be exchanged for Microsoft Entra access tokens". The
     Azure DevOps issuer, `https://vstoken.dev.azure.com/ORGANIZATION-ID`
     with `sc://ORG/PROJECT/CONNECTION`, was deprecated on 2026-07-01 and
     retires on 2027-07-01, for single-tenant apps and managed identities
     in Azure's public cloud; it stays for multitenant apps and other
     clouds.
   - The Entra issuer is the tenant's: a v2.0 token's `iss` "identifies the
     STS that constructs and returns the token, and the Microsoft Entra
     tenant", whatever the app. A managed identity of the tenant gets a
     token of that issuer for `api://AzureADTokenExchange` too, its own
     object id as `sub`. Issuer and audience would tie a token to the
     tenant; only `sub` would tie it to one service connection.
   - The audience is Entra's token exchange, not the job's to choose: the
     `oidctoken` API (`POST .../jobs/JOB/oidctoken?serviceConnectionId=ID`,
     with `System.AccessToken`, as `System.OidcRequestUri` names it) takes
     a service connection, and no audience. A post outside Microsoft says
     an Entra-issued token names the exchange by its application id,
     `fb60f99c-7a34-4190-8149-302f77469936`; Microsoft's documentation
     read doesn't, and that audience isn't refused as an entry's (a
     follow-up, once Microsoft says it).

   Built on the Azure DevOps issuer alone, it would end with it, within
   the year.
4. **Bitbucket.** No claim says a step is a pull request's, and what was
   read says nothing of forks: take its token from the first step, or once
   checked against an account? **Decided on 2026-11-19, by the maintainer:
   take it**, an entry with `ci: bitbucket`, or known from its issuer,
   pinning `repositoryUuid` (a list allowed) and `workspaceUuid`, or serve
   doesn't start. A Bitbucket backtest supersedes none, as a GitLab one
   doesn't: the token carries no pull request's number. Built in step 2,
   not yet checked against an account ("As built (step 2)").
5. **The share.** A quarter of `--max-rate`, or what is left after the runs
   need it, as [preflight.md](preflight.md) counts it? **Decided on
   2026-11-18, by the maintainer: a quarter**, `--backtest-share` 0.25 by
   default, 0.05 to 0.75; the hourly runs keep the rest.
6. **Pins by pattern.** A whole organization (`repository_owner_id`), or a
   list of repositories, in one entry? **Decided on 2026-11-18, by the
   maintainer: a list of repositories**, each by its id; an entry never
   trusts an organization whole, so a repository made in it later is not
   trusted until it is named.

## As built (step 1)

Where step 1 does otherwise than the sections above say, or says more:

- **The event is pinned too.** For the three CIs known, an entry must pin
  the event as well as the repository's id: `event_name` to
  `pull_request` or `push` (GitHub, Forgejo), `pipeline_source` to
  `merge_request_event` or `push` (GitLab). `pull_request_target`,
  `workflow_run` and the like run a fork's pull request in the
  repository's own context, so an entry naming one, or none, doesn't
  start. A CI is known by `ci:` (`github`, `gitlab`, `forgejo`), or from
  its issuer: GitHub's, `https://gitlab.com`, an issuer ending in
  `/api/actions`. Any other issuer's entry pins `sub` or another claim,
  one value each.
- **Keys at the issuer's own origin.** The JWKS a discovery document names
  is taken only when it is https at the issuer's origin; `jwks_url` names
  it otherwise. A CI's RSA keys are taken at 4096 bits at most, beside
  the 2048 bits at least every identity provider's are held to (#136),
  and a failed discovery is asked again once a minute at most, not every
  ten seconds as a sign-in's is. A token that names no key (`kid`) is
  tried against the first three of its issuer's keys alone, not each of
  a hundred (since 0.63.2): the CIs' tokens name theirs.
- **One audience.** A token naming several audiences is refused, even with
  the entry's among them: it was made for others too. `iat` may be up to
  five minutes ahead, the leeway `Bearer` gives.
- **The audiences refused are listed**: GitHub's default (under
  `https://github.com/`), one at the issuer's own origin (Forgejo's
  default is its owner's URL there), `sigstore`, `sts.amazonaws.com` and
  `*.amazonaws.com`, `api://AzureADTokenExchange`, and Google Cloud's
  `//iam.googleapis.com/`, `https://iam.googleapis.com/` and
  `https://sts.googleapis.com`.
- **A token's once is a table of its own.** Schema 023 has two tables:
  `backtests`, and `backtest_tokens`, a row a token that started one, by
  its entry and its `jti` (or its hash), with the body's hash. Its key is
  the check, so replicas given one token at once start one backtest; the
  row is kept an hour and ten minutes past `iat`, past the hour a token is
  taken for, whatever becomes of the backtest's own row. A token that
  started one is refused (409) before its body is read, whatever the body
  (the third round, below); two of one token at once, the first not yet
  kept when the second was read, are told apart by the body's hash, the
  same body answered 200 with the id and the state, without the key,
  which is given once: a token stolen from a job's log gets no key to read
  or cancel with. A plain token's requests aren't tied to a body: its hash is
  the same for every request, and would start one backtest ever.
- **The comment holds nothing a rule read.** An alert's labels are its
  series', which a rule can make of any value serve's access reads
  (`label_replace`), so they are dropped before the Markdown is written:
  its severity rows count each firing by its rule's own `severity` label,
  as the pull request wrote it. The JSON leaves out the firings (the
  counts stay), the settings it was judged by, and each incident's labels
  and an impact window's series. A reason a rule couldn't be replayed is
  the request's own words where it read so (a query that doesn't parse),
  and otherwise one of a few of Lapilli's: `query failed: the history
  couldn't be read by lapilli serve`, `query failed: the history refused
  it as written`, `too many labels on its series to replay`, `it couldn't
  be replayed`. What the history said isn't logged either: the log has a
  line a backtest, with counts.
- **Each selector one metric's.** A rule that differs whose selector names
  no metric (`{job="api"}`), or names it by a pattern or a negation
  (`{__name__=~"..."}`), is refused 400 (`rules.Wide`), and each range
  query the lane asks carries `timeout=1m`.
- **The pull request's number** is taken from GitHub's and Forgejo's
  `ref` (`refs/pull/N/merge`, `refs/pull/N/head`); a body's that
  disagrees is refused. The repository shown is `repository` (GitLab's
  `project_path`).
- **Ran out of time is failed.** A backtest that didn't finish within
  `--backtest-timeout` ends `failed`, saying so, not with a part of a
  comparison. One whose leader stopped leading while it ran waits again,
  and the next leader runs it from its start.
- **The lane looks at the queue every 5 seconds**, and at once when a
  backtest is queued on the replica that leads; and, while one runs,
  whether its job cancelled it. `GET` and `DELETE` are 20 a second at
  most, a burst of 100, on each replica; one with an `Origin` is 403.
- **Bounds of the flags.** `--backtest-queue` 1 to 1000;
  `--backtest-timeout` 1 minute to 2 hours. A backtest with nothing that
  differs is queued all the same, and answered by the lane without
  asking anything, as `lapilli verdict` answers it.
- **The Run history** shows each backtest's state, not its error, which
  may name a rule; and no pull request's number.
  `lapilli_serve_backtests_waiting` is by entry too.
- **Incidents by their ids.** Of the incidents, the comment and the JSON
  keep those the change catches differently, by their ids (and starts, as
  the comment's table has them); their titles, written in a tracker, only
  for an entry that says `incident_titles: true`, as a public
  repository's pull request is read by anyone. The JSON leaves out each
  side's incidents, the thresholds (`noisy_at`), the hours and the impact
  signals (`impact_short`). Case 3's INC-43 is said by its id.
- **The share is kept for the lane.** A share only as a ceiling left the
  lane what the runs didn't take: under load, a third of its share, and
  `fits` planned on what it didn't have. While the lane runs a backtest,
  the runs' pace slows by the share (`prom.Pace.Reserve`), and the lane
  asks at its own pace, which nothing else takes; idle, the runs have the
  whole rate. `fits` plans on that share, which the lane is sure of.
- **The hour before the body.** An entry's `hourly` is taken right after
  the token, before the body is read, and given back for one refused
  before it is queued; two bodies are read and checked at once at most,
  past them 429. (A token that started a backtest keeps it, since 0.63.2:
  "The third round" below.)
- **A token without `jti`** (Forgejo's) is known by the SHA-256 of its
  payload as it was signed, not of the string sent: base64url's bits past
  the last byte, which go-jose passes over, write one token many ways. A
  token is verified once by its issuer's keys, then held to each entry's
  audience and pins.
- **Issuers.** GitHub Enterprise Cloud's issuer of an enterprise
  (`https://token.actions.githubusercontent.com/SLUG`) and GitHub
  Enterprise Server's (`https://HOST/_services/token`) are GitHub's. An
  issuer known neither so nor by `ci:` must pin `sub` and the event; an
  event pinned under any issuer is `pull_request` or `push`
  (`merge_request_event` or `push`), never `pull_request_target`,
  `workflow_run` or `schedule`.
- **What a rule reads back.** Beside `for`, a range, a subquery or an
  offset a week back at most, a subquery of 11,000 points at most, and no
  `@` (`rules.Reach`; added up, 8 days, from the second round); no
  `info()`, which reads `target_info` beside the metric it names
  (`rules.Unnamed`, since 0.63.2). Since
  0.63.2, PromQL every history `--prometheus` takes reads alike
  (`rules.Unalike`): VictoriaMetrics' MetricsQL reads a `\` in a name, `or`
  in braces, an offset after any expression and `WITH` templates, and
  Prometheus with unquoted UTF-8 names a name that isn't ASCII, each
  otherwise than these checks; what any of them could read otherwise is
  refused.
- **A pull request superseded by its token's word alone.** Only a number
  the token names (GitHub's and Forgejo's `ref`) supersedes the backtest
  waiting for it; one the job said (GitLab, a plain token, a `push`)
  supersedes none. Nor, since 0.63.2, does a token whose repository's name
  isn't kept (past 200 bytes, or with what doesn't show: ""), as the
  backtest waiting is matched by that name: two repositories an entry pins,
  each named so, would be one. The repository's id would tell them apart,
  but isn't kept (a column of its own is a schema change). The leader takes the oldest of the entry that started
  one least lately. One started twice whose leader stopped each time
  (`attempts`, in 023) fails, saying so; one that panics fails. What ended
  a day ago goes at each of the leader's rounds, the port on or not.
- **Reads by the key.** Each process keeps the backtests' key hashes by
  id, those it queued and the ledger's, listed a second apart at most: a
  pair it knows is read whatever comes from its address; one it doesn't
  waits for a listing begun after it came, charged to the client's address
  while it waits (since 0.63.2; before, each lookup was charged to the
  address, behind an ingress everyone's). A backtest's reads are limited
  by its id too. 4,096 addresses, and as many ids, are kept so (`perKey`):
  past them, only those refilled to their burst go, a second apart at
  most, and while none can, one not kept is refused, as one past its
  bucket is (since 0.63.2; before, all went at once, the spent ones among
  them, for anyone with 4,096 addresses to begin afresh).
- **The second round.** An entry has one turn to have a body read, within
  the two of all entries, and a body comes within 5 seconds of its
  headers; the hour taken stays taken for a request refused as sent (a
  400, a body too slow), given back only when the port couldn't take one
  it could have (the reads or the queue full, the ledger out of reach, the
  same request again, a token's once). What a rule reads back is added up
  through its subqueries and offsets, 8 days at most in all, a week over
  week of a day's range; a subquery needs a step of its own, and a range
  or an offset is a duration as written (`offset (700d)`, Prometheus's
  duration expressions, refused). A rule whose expression doesn't read as
  PromQL is refused as written, alone, as lapilli verdict says it, asking
  nothing; a `rules_select` that keeps nothing on either side is refused,
  as lapilli verdict refuses it (the third round takes in a side with no
  documents). The planner's refusal says no rate.
  GitLab's ID token names a merge request's source branch (`ref`,
  `ref_path`), not its number, so a GitLab merge request's backtests don't
  supersede each other. The lane asks a quarter of `--parallel` at once,
  which a slow history holds below its share: `fits` doesn't count that,
  and the docs say so. What keeps a fork out is that its CI gives its run
  no token, not the pins ("Forks" above says the pins keep forks out): a
  run of a fork's pull request in the repository carries the repository's
  own `repository_id`. GitHub's "Send write tokens to workflows from pull
  requests" kept off, and Forgejo's reference says its OpenID Connect is
  off for a fork's `pull_request`; GitLab's `project_id` is a fork's own.
- **The third round.** What can be told without the body is asked before
  it is read (`ledger.BacktestRoom`): a token that started a backtest, and
  the queue's room for the entry, less a newer commit's pull request's
  waiting one; each refused cheaply, its hour given back, but for a token
  that started one, which keeps it (since 0.63.2): its job has its
  backtest, and the token sent again and again would cost a signature and
  the ledger's reads each time, past the entry's hourly. Once the body is
  read, a request refused keeps its hour, the queue's and the token's own
  checks at `AskBacktest` among them: an entry keeping its three waiting
  can't send heavy bodies to be read and checked, refused, again and
  again. `rules.Reach` reads past comments as Prometheus does, a quote in
  one starting no string. `rules_select` keeping nothing of a side it was
  given documents of, with no rule left on either side, is refused, the
  other side empty or not: a pull request's first rules.
- **`expect` given when it is there**, however empty, as `--expect` takes an
  empty file; read, as every expectations file is now, by the rules'
  bounded YAML reader first.
- **The chart.** `serve.backtest` takes the trust file from a ConfigMap
  (`trust.configMap`) and the plain tokens from Secrets (`tokens`, each
  mounted at `/var/run/lapilli/backtest/tokens/SECRET/KEY`), and has
  `share`, `queue`, `timeout`, `from` (the NetworkPolicy's peers for the
  port alone), and a Service and an Ingress of its own,
  `FULLNAME-backtest`. Off the cluster only over TLS: the Ingress needs
  `tls`, or `loadBalancerEndsTLS` for a certificate the load balancer in
  front holds; a `LoadBalancer` or `NodePort` Service needs
  `service.loadBalancerEndsTLS`, an internal load balancer's too, as the
  port is plain http. No HTTPRoute of its own.
- **The recipe takes plain http to a Service in the cluster too**, where
  "Flags and API" says to the machine itself: `LAPILLI_SERVE` is https, or
  http to this machine (`127.0.0.1`, `[::1]`, `localhost`) or to
  `NAME.NAMESPACE.svc` or `NAME.NAMESPACE.svc.cluster.local`, each a DNS
  label with a letter first, so that a runner in the cluster reaches
  `FULLNAME-backtest.NAMESPACE.svc:8084`; and plain http through no proxy
  (`--noproxy '*'`), which would see the token. Narrower than what serve
  takes an identity provider at (`signin.SafeURL`): a name of one label is
  looked up under the runner's search domains, which needn't be the
  cluster's, and `NAME.NAMESPACE` may be a name on the internet, a
  namespace named as a top-level domain. A host's API and a release are
  taken as before.
- **The recipe's serve mode, as built.** The files are named by their path
  under what `PATHS` names, as `--rules` reads a directory (a file it holds
  is the directory's, and what doesn't read in it is said, not passed
  over), and what `RENDER` wrote as `rules.yaml`. Hidden files (a name in
  the path beginning with a dot, `..` too) are sent too, since 0.63.2, as
  Prometheus's `rule_files` glob loads them and `--rules` reads one that
  holds rules: one that isn't UTF-8 text and has no BOM isn't, nor any
  under a `.git` directory; one in UTF-16 or UTF-32, its BOM first, is
  said, as Prometheus reads it. Two of `PATHS`
  holding a file of one name, a file that isn't UTF-8 text, and a body past
  4 MiB are said in the comment before anything is sent. `--since` is
  taken in whole days, `1d` to `14d`; every argument but it, `--tz` and
  `--rules-select` is refused, as are `INCIDENTS`, `IMPACT`, `IMPACT_FOR`
  and `MATCH`. `LAPILLI_VERSION` and the other settings of the install are
  passed over, the job's log saying so. The token is one of
  `LAPILLI_ID_TOKEN`, `LAPILLI_SERVE_TOKEN_FILE` (read for each request),
  or `LAPILLI_SERVE_AUDIENCE`, for which `github.sh`, and `gitea.sh` in
  Forgejo Actions, ask Actions for an ID token right before each request;
  it is kept in a variable of the recipe's own, out of what it runs, and
  given to curl on stdin, as the key is. `LAPILLI_SERVE_AUDIENCE` is
  refused when it is a cloud's audience or the CI's own default
  (`sts.amazonaws.com` and the rest of `amazonaws.com`,
  `api://AzureADTokenExchange`, Google Cloud's `//iam.googleapis.com/...`,
  `https://iam.googleapis.com/...` and `https://sts.googleapis.com...`,
  `sigstore`, `https://github.com/...`), which other services take a token
  for, and the job's log says when it isn't `LAPILLI_SERVE`; the action's
  `audience` is it. In GitHub Actions, `github.sh` masks the ID token it
  gets (`::add-mask::`) before anything else could log it, as
  `core.getIDToken` does; `gitea.sh` doesn't, though Forgejo's runner
  (act's) reads the command as GitHub's does (0.63.2 put right what this
  said before: that Forgejo has no such command). An expectations file of no bytes is sent as `"expect": ""`,
  given, as `--expect` takes it; the field is left out only when the
  commit has no file. `expect_before` is the file as the commit the two
  share has it, as the pull request's branch does: a pull request is held
  to the expectations in its own commit, the target's where the job
  checks out a merge. Where the runner's file system folds case, a
  commit with two paths under `PATHS` or `LOKI_PATHS` that differ only in
  case isn't read, nor anywhere one of more files than the runner's file
  system holds of it once written (since 0.63.2): a macOS or Windows
  runner writes one over the other.
  `Retry-After` is waited, 5 minutes at most, 5 seconds at least (or
  `SERVE_POLL` if longer: a `Retry-After` of 0 isn't a loop;
  `SERVE_RETRY_MIN` lowers the 5 for tests, to a second at least since
  0.63.2), and never past `SERVE_WAIT`; a read that fails is asked again, five times in a row at
  most; past `SERVE_WAIT`, and when the job ends first, the backtest is
  cancelled. `SERVE_POLL` (10 seconds) is how often it is read.
- **The recipe posts of serve's report only what lapilli writes.** Serve
  is trusted with the summary line the gates read, but not with the
  job's token on GitLab, which runs a quick action from a note, nor with
  who is pinged: the first line is kept as serve wrote it only when it is
  `[//]: # (lapilli-compare {...})` with a JSON object of numbers and
  booleans, or there is no report (a problem, in the comment); each line
  after it only when it is one `CompareMarkdown` writes (code spans closed
  on their line, with no `|` but `\|` in a table; outside them only
  Lapilli's words and punctuation, `#` only to start a heading;
  `<details><summary>N ...</summary>` and `</details>`, opened then
  closed), within its first 65,536 characters, more than `CompareMarkdown`
  writes. Any other line is left out whole, a line saying so in place of
  each run of them, and is in the job's log: lines are let through as they
  are or not at all, since what each host would make of a line changed in
  part isn't known to the recipe, and a report lapilli wrote passes byte
  for byte. A comment past 65,000 bytes even without a log is cut to the
  report's first lines, within 60,000 bytes, saying so.

## As built (step 2)

Bitbucket Pipelines' ID token, as open question 4 decided it; Azure
Pipelines stays on the plain token, as open question 3 decided. Where step
2 does otherwise
than the sections above say, or says more:

- **Known by its issuer.** `ci: bitbucket`, or an issuer
  `https://api.bitbucket.org/2.0/workspaces/WORKSPACE/pipelines-config/identity/oidc`.
  An entry pins `repositoryUuid`, which may be a list, and
  `workspaceUuid`, one value, each a UUID lowercase in braces as
  Bitbucket's tokens write them (a slug, a UUID without braces or in
  capitals, which no token would match, stops serve at start); no event,
  as its tokens say none. Every other pin is one value. No schema.
- **Two audiences, of Bitbucket's alone.** Bitbucket puts the workspace's
  own audience, `ari:cloud:bitbucket::workspace/UUID` (its UUID without
  braces), in every token, and a step's `audiences:` beside it, in one
  `aud` list. A Bitbucket entry's token may name its `audience` alone, or
  beside that of the workspace the entry pins: nothing else, no third,
  never the workspace's alone. Every other entry's names one, as before.
  The cost: a token taken here is one any service that trusts the
  workspace's own audience takes too (Bitbucket's AWS setup trusts it), so
  whoever reads it on its way to serve, or in serve, could use it there
  until it expires; serve keeps none, only its hash. Accepted on
  2026-11-19 by the maintainer, with what keeps it small: the step that
  asks for serve's audience asks for no other, and runs the recipe alone;
  and the other services that trust the workspace (an AWS role made as
  Bitbucket's guide makes it, and the like) trust an audience of their
  own, asked for under `oidc: audiences:` by the steps that need them,
  rather than the workspace's, so that a token serve is sent is of no use
  to them. An entry's `audience` under `ari:cloud:bitbucket:` is refused,
  a CI's default.
- **No `jti`, no `nbf`.** Bitbucket's discovery document lists neither:
  a token is known by the hash of its payload, as Forgejo's is, and held
  to the hour from its `iat`. Each step's token is its own (`stepUuid`).
- **No pull request.** The number is the job's word
  (`BITBUCKET_PR_ID`), so a Bitbucket backtest supersedes none. The Run
  history shows the repository by its `repositoryUuid`: the token names it
  by nothing else.
- **lapilli preflight** reads the file with `serve.ReadTrust`, as serve
  does, and says such an entry's CI as `bitbucket`; a Bitbucket issuer,
  which names the workspace, is said as the trust file's issuer N.
- **The recipe.** `bitbucket.sh` sends the step's token as
  `LAPILLI_ID_TOKEN`, the job writing
  `LAPILLI_ID_TOKEN=$BITBUCKET_STEP_OIDC_TOKEN`: the recipe doesn't take
  the step's token of itself, which, from a step that asked for no
  audience of serve's, would name the workspace's alone. `lib.sh` reads a
  `LAPILLI_ID_TOKEN`'s payload, not verified, for its `aud` alone, and
  refuses at once to send one that names Bitbucket's workspace audiences
  and nothing else (`ari:cloud:bitbucket:...`): serve would refuse it,
  and sending it would hand it over for nothing. It keeps
  `BITBUCKET_STEP_OIDC_TOKEN` out of what it runs in serve mode. The plain
  token stays.
- **What was read.** Atlassian's documentation shows a token's claims as a
  picture, and doesn't say how `aud` reads with `audiences:`; its own
  example payload, and a workspace's discovery document, say the rest:
  - <https://support.atlassian.com/bitbucket-cloud/docs/integrate-pipelines-with-resource-servers-using-oidc/>
    (the identity provider URL and audience from the repository's
    settings; `audiences:` under `oidc`, 10 at most, 150 characters each);
  - <https://support.atlassian.com/bitbucket-cloud/docs/deploy-on-aws-using-bitbucket-pipelines-openid-connect/>
    (`oidc: true`, `BITBUCKET_STEP_OIDC_TOKEN`, `sub` as
    `{REPOSITORY_UUID}[:{ENVIRONMENT_UUID}]:{STEP_UUID}`, a payload with
    `repositoryUuid` in braces and `aud` the workspace's ARI);
  - <https://www.atlassian.com/blog/bitbucket/bitbucket-pipelines-oidc-now-supports-multiple-audiences>
    (2026-02-03: audiences are a list a resource server finds its own in);
  - <https://support.atlassian.com/bitbucket-cloud/docs/pipeline-start-conditions/>
    ("Pull requests from a forked repository don't trigger the pipeline");
  - <https://docs.openfaas.com/openfaas-pro/iam/bitbucket-federation/>
    (a payload whose `aud` is the workspace's ARI and the step's
    audience, the UUIDs in braces, no `jti`);
  - a workspace's
    `.../pipelines-config/identity/oidc/.well-known/openid-configuration`
    (`claims_supported`: `sub`, `iss`, `aud`, `exp`, `iat`,
    `branchName`, `compliant`, `deploymentEnvironmentUuid`,
    `pipelineUuid`, `repositoryUuid`, `stepUuid`, `commitSha`,
    `pipelineRunUuid`, `workspaceUuid`, `isPrivateRepository`,
    `isInternalWorkspace`; RS256; `jwks_uri` `.../keys.json` at the same
    origin), and two workspaces' keys, which differ.

  And Microsoft's, for open question 3:
  - <https://learn.microsoft.com/en-us/azure/devops/pipelines/release/troubleshoot-workload-identity>
    (the two issuers and subjects side by side);
  - <https://learn.microsoft.com/en-us/azure/devops/pipelines/release/convert-service-connections>
    and
    <https://learn.microsoft.com/en-us/azure/devops/pipelines/release/configure-workload-identity>
    (the Azure DevOps issuer retired on 2027-07-01; new connections on
    Entra's);
  - <https://devblogs.microsoft.com/devops/?p=72939> (2026-06-22: the
    retirement; "an OIDC token minted by Microsoft Entra");
  - <https://learn.microsoft.com/en-us/entra/workload-id/workload-identity-federation-config-app-trust-managed-identity>
    (a managed identity's token: the tenant's issuer,
    `api://AzureADTokenExchange`, its principal id as `sub`);
  - <https://learn.microsoft.com/en-us/entra/identity-platform/access-token-claims-reference>
    (`iss`, the tenant's STS);
  - <https://learn.microsoft.com/en-us/rest/api/azure/devops/distributedtask/oidctoken/create>
    (`serviceConnectionId`, no audience);
  - <https://learn.microsoft.com/en-us/azure/devops/pipelines/repos/github>
    (forks and service connections).

## As built (step 3)

Grafana-managed rules and a Loki ruler's, as open question 2 decided. Where
step 3 does otherwise than the sections above say, or says more:

- **The rules in effect** are the version of the rules serve runs now (the
  ledger's latest), read as its runs replay it (`parse`): every rule it
  replays, of every source, whichever team it is of (the port has no team:
  an entry is a repository's). A rule it can't replay (paused, unsupported
  as read, a Loki's with no Loki to ask) reads nothing. Of a Grafana rule,
  the data sources its condition reads, which the replay asks
  (`Graph.Data`), each as it asks them: PromQL, of `--prometheus`, or
  LogQL, through `--grafana`'s data source proxy (of `--loki`, without
  `--grafana`); a query beside the condition, which isn't asked, counts
  for none. They are read when a backtest is asked, and again before the
  lane runs it: one whose data source the rules stopped reading in between
  fails, saying why.
- **By uid exactly, and asked alike.** A uid is matched as written, its
  case, its spaces and its length; and asked as the rules in effect ask
  it: a uid they read as PromQL is refused to a rule that would read it as
  LogQL, and the reverse (the type a rule writes in its model counts only
  where serve's Grafana didn't say the data source's). Grafana's own data
  sources (`grafana`, `-- Grafana --`, `-- Mixed --`, `-- Dashboard --`)
  and a uid Grafana makes none of (letters, digits, `-` and `_`, 40 at
  most, as `source.Logs` puts one in the proxy's path) are refused even
  where a rule in effect names one. A rule unsupported as read (a SQL
  expression, a data source of a type not replayed) is held to its uids
  all the same, and then asks nothing, as any unsupported rule. An
  expression's model (`__expr__`) names no data source to Lapilli, which
  works expressions out itself, and asks nothing for them; a node whose
  `datasourceUid` is `__EXPR__`, or written any other way, is a query of a
  data source, and refused.
- **One refusal.** "X reads a data source no rule in effect reads", the
  rule's name alone, whatever why: a uid no rule in effect reads, one
  read the other way, Grafana's own. Nothing of serve's is said, no uid
  and no data source. A refusal still tells, a uid at a time, whether one
  is in effect, as taking it would: the price of the decision, each
  refusal costing the entry an hour. A Loki ruler's rule is refused the
  same way whether serve has no `--loki` or no Loki ruler's rule in effect.
  A Grafana rule's query held to a bound below (a selector, what it reads
  back, LogQL's) is said by its refId, "X's query A", and nothing of it is
  quoted: what the replay asks has the macros put in, `$__rate_interval`
  with the scrape interval serve read of the data source among them, so a
  refusal that quoted it, or what it reads back worked out from it, would
  tell a pull request that interval. A Prometheus or a Loki ruler's rule,
  which has no macros, is quoted as before.
- **Nothing more of Grafana is asked.** A pull request's Grafana rule is
  read with the types and scrape intervals of its data sources serve read
  for its own rules (`replayedAs`); Grafana's API isn't asked of any uid
  for it.
- **Grafana's bounds**, beside a rule's (`for`, `keep_firing_for`, its
  group's interval, 50 rules that differ, the period, `--max-samples`, the
  series and the bytes): 8 queries of data sources and 16 queries and
  expressions in all, a rule; each query's `relativeTimeRange` from 8 days
  before the evaluation at most; each query the replay asks held as a
  rule's expression is, one metric a selector, and what it reads back,
  with the relativeTimeRange's end, or a reduce's window over LogQL, added
  up within 8 days; and each data source query a day further back again
  (since 0.63.2), as the replay asks it over the day before when it
  returns nothing (`replay`'s probe), and, in PromQL, that day's
  subquery, at the group's interval (`replay.ProbeStep`), held to 11,000
  points: an interval of 8 seconds at least. A classic condition's window
  is counted in the queries its reducers ask.
- **LogQL's bounds**, of a Loki ruler's rule and a Grafana rule's on a
  Loki data source: a metric query, never one of log lines; each stream
  selector naming its streams by a label matched with `=` to a value that
  isn't empty (`{app=~".+"}` and `{app!="x"}` alone are refused, as
  `{__name__=~"..."}` is); a range with its offset 8 days back at most,
  with a Grafana query's relativeTimeRange, each written as a duration, no
  subquery, no `@`, no offset after no range; `#` comments read past as
  Loki reads them. Loki's lexer is a copy of Go's `text/scanner`: beside
  `#`, it skips `//` and `/* */` comments outside a string and reads a `'`
  as a character, so a `#` or a quote inside one would hide what follows
  from every check (`/* # */ sum(count_over_time({app=~".+"}[30d]))`
  passed them all). A query with a `//` or a `/*` outside a string, or a
  `'` outside one, is refused as a whole (`rules.LogQLText`: "has a // or
  /* comment, or a ' outside a string"), before the rest, which read the
  same text, its `#` comments blanked; whether it is a log query is read
  past comments of every kind, opening brackets and a byte order mark, as
  Loki's lexer and grammar read past them. No metric query needs them. What Loki
  answers is held as `--loki`'s always is: 256 MiB, `--max-samples`, the
  series, and a log query's lines refused.
- **A Loki ruler's files**, from the job: `"loki": true` on a file of the
  body, read as `--loki-rules` reads one (its queries LogQL, its
  `keep_firing_for` and its group's `query_offset` dropped, as Loki 3.7.9
  runs a file); a Grafana rule, or a cloud's, in such a file is refused.
  The recipes send `LOKI_PATHS`'s files so (the action's `loki-paths`),
  beside `PATHS`, and not with `RENDER`.
- **The lane's Lokis.** The lane asks a Loki through a client of its own,
  a copy of the runs' (`prom.Client.Lane`): serve's URL, token and tenant
  (`X-Scope-OrgID`), never the request's, whose body has no field for
  one; at its share of that Loki's pace (a Grafana data source's at its
  share of the Grafana's), which the runs keep for it only while a
  backtest asking that Loki runs, as `--prometheus`'s; each request given
  up on a minute after it is sent (since 0.63.2), where a Prometheus is
  told `timeout=1m`, which Loki doesn't take; and every request of the
  lane, of every API, within a quarter of `--parallel` at once, as the
  memory estimate counts them.
- **`query_offset` is a read back.** A group's `query_offset` asks each
  query that long before its evaluation, so it is added to what a rule
  reads back, 8 days at most in all. Step 1 didn't count it: a pull
  request's Prometheus rule could read as far back as its `query_offset`
  said. Found in this step's review, and fixed with it.
- **What the port counts.** `fits` counts a backtest's requests of every
  API by the same planner, at the lane's rate. What only a Grafana rule's
  answers can say (whether a query that returned nothing returned anything
  the day before, the first numbers of a classic condition's window, a
  reduce over LogQL asked a batch of evaluations at a time) isn't counted,
  as the hourly runs' estimate doesn't count it (`replay.Planned.Unsure`):
  `--backtest-timeout` ends one that asks more.
- **What shows.** The comment, the JSON, the Run history and `/metrics` say
  a Grafana rule and a Loki ruler's as they say a Prometheus rule: by its
  name (a Grafana rule's title), its labels and its counts.
- **lapilli preflight** times the runs, while a backtest runs the whole
  hour, at `--max-rate` less the lane's share for every API they ask, a
  Loki and Grafana's data source proxy as `--prometheus`; and its
  estimate's backtest changes ten of the rules replayed, Grafana's and
  Loki's among them, each of queries of its own.
- **MCP's `compare`** takes no Grafana rule, and a rule in LogQL only as a
  change to a Loki ruler's rule in effect, asked of `--loki` in its
  namespace (`proposed`): no data source the rules in effect don't read.
  Left as it is: it holds a rule to none of the port's bounds, its PromQL
  and its LogQL alike (one metric a selector, what it reads back, a
  group's `query_offset`), so a comment has no check of its to hide
  anything from. Its callers are those `--mcp-compare` names, editors or
  admins, trusted with what serve reads; what holds it is what holds every
  replay (14 days, 25 seconds, `--max-samples`, Loki's 256 MiB and no log
  lines). Looked at again for 0.63.2: its LogQL is held as its PromQL is.

## Steps

1. The port, ID tokens of GitHub, GitLab and Forgejo, and the plain token;
   Prometheus-format rules; the lane and its share; schema 023; the
   recipes' serve mode and the action's inputs.
2. Bitbucket's token: built. Azure Pipelines stays on the plain token
   (open question 3).
3. Grafana and Loki rules: built, on the data sources the rules in effect
   read (open question 2).
