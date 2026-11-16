# Pull request backtests through serve: no metric credentials in CI

Status: proposed. Under [GOVERNANCE.md](../../GOVERNANCE.md) this is a new
kind of data in the ledger (the backtests CI jobs ask for, kept a day), and
a port that takes input from outside with a token of a new kind, so it is
designed first, as a pull request, and reviewed as an attacker would
([CONTRIBUTING.md](../../CONTRIBUTING.md); "Security" below). The companion
to [expectations.md](expectations.md) (#81), whose first step (#86,
`--expect`) it carries as it is, and to
[recipes/backtest](../../recipes/backtest).

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
   (the planner [preflight.md](preflight.md) proposes, from `chunks` and
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
    audience: https://lapilli-ci.example.com
    claims:
      project_id: "42"
      pipeline_source: merge_request_event
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
  as the repository's OpenID Connect settings show it. The step has `oidc:
  true` and a custom audience under `oidc: audiences:`, and the token is
  `BITBUCKET_STEP_OIDC_TOKEN`. Pin `repositoryUuid`, and `workspaceUuid`.
  No claim says the step is a pull request's.
- **Forgejo Actions.** Issuer `URL/api/actions`. The workflow sets
  `enable-openid-connect: true`, and asks with
  `ACTIONS_ID_TOKEN_REQUEST_URL` and an audience. Pin `repository_id` and
  `event_name: pull_request`; `sub`'s form changed in Forgejo 16, and pins
  of it would break.
- **Gitea Actions.** No ID token in the documentation read for this note
  (a proposal, go-gitea/gitea#33681): a plain token.
- **Azure Pipelines.** A job's ID token is for a service connection
  (issuer `https://vstoken.dev.azure.com/ORGANIZATION-ID`, subject
  `sc://ORG/PROJECT/CONNECTION`), made for Microsoft Entra's token
  exchange, `api://AzureADTokenExchange` in the federations documented:
  serve could check it, but a token of that audience is one Entra would
  take too, for whatever identity trusts that connection. A plain token in
  the first step (open questions).

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
with serve's access to `--prometheus`, not the job's. They can read any
series that access can, and the firing counts in the comment say what they
read, a count at a time (a rule `secret_series > 41` fires or doesn't).
That is what `--mcp-compare` lets an agent do ([serve.md](serve.md), "That
PromQL can learn, a count at a time, whatever that access can read"), and
why both are off unless given: no `--backtest-trust`, no port. Give serve's
history credential what the rules need and no more (one tenant, the
metrics' project).

Bounded, as MCP's `compare` is where it has a bound:

- **Prometheus-format rules only**, replayed against `--prometheus`.
  Grafana-managed rules and Loki's are refused in the first step: Grafana's
  are asked through Grafana's data source proxy with serve's Grafana
  token, of any data source that token reads; Loki's with serve's Loki
  token (open questions).
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
- Bitbucket and Forgejo say nothing of forks in what was read; pin the
  repository, and keep OpenID Connect from forks' pipelines as the recipes'
  README keeps secrets from them.

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
in all; a change of `for` alone asks for the same expressions on both sides,
which share their reads (`RunEach`), 80. At `--max-rate 0.5` with a quarter
for the lane, 0.125 a second, 160 requests take some 21 minutes. A
self-hosted Prometheus with no `--max-rate` answers them in seconds, as the
job does today. Under a quota, a backtest through serve is slower than one
in CI at the job's own rate: that is the price of one quota and no
credential in CI, and the job's log says where it is in the queue.

## What is kept

- **A table, `backtests`** (schema 021: 020 is expectations.md's). A row is
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
  "after":  [{"path": "alerts/api.yaml", "content": "groups: ..."}],
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
wrong key are the same 404. **`DELETE /v1/backtests/{id}`**, with the key:
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
- "While people slept" is by serve's `--sleep-hours` and `--shifts` (and,
  once step 2 makes it so, the rule's team's own hours), not the job's
  flags.

**Step 2** (serve's checks after deploy, `expectation_checks`, schema 020)
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
and the recipe's gate still decides.

**Order.** It is built once step 1 is merged, and needs nothing from steps
2 and 3; its schema comes after 020.

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
  backtest at a time; nothing from a URL in the body, no Grafana, no Loki.
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
on stdin).

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

- Grafana-managed rules and Loki's.
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
   reading the fork's files as data and no `RENDER`?
2. **Grafana and Loki rules.** Taken only on the data sources the rules in
   effect already use, by uid?
3. **Azure Pipelines' ID token.** Take it, for a service connection made
   for Lapilli alone and trusted by no identity in Entra; or keep the plain
   token?
4. **Bitbucket.** No claim says a step is a pull request's, and what was
   read says nothing of forks: take its token from the first step, or once
   checked against an account?
5. **The share.** A quarter of `--max-rate`, or what is left after the runs
   need it, as [preflight.md](preflight.md) counts it?
6. **Pins by pattern.** A whole organization (`repository_owner_id`), or a
   list of repositories, in one entry?

## Steps

1. The port, ID tokens of GitHub, GitLab and Forgejo, and the plain token;
   Prometheus-format rules; the lane and its share; schema 021; the
   recipes' serve mode and the action's inputs.
2. Bitbucket's token, and Azure Pipelines' if decided.
3. Grafana and Loki rules, if decided.
