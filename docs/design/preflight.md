# `lapilli preflight`: what serve will meet, before it is deployed

Status: accepted; step 1 built. Merged on 2026-11-16, before its week was
out, by the maintainer's decision (#92), after a review whose changes are in
it. What building step 1 found the design said otherwise than the code can
do is in "As built" below.
Under [GOVERNANCE.md](../../GOVERNANCE.md) a new command is designed first,
as a pull request. It adds nothing to the ledger, takes no input from
outside but the answers of the APIs serve already reads, and changes no
verdict. The companion to [serve.md](serve.md) (runs, the ledger, the
bounds on what a history answers), [quota.md](quota.md) (sharing the
history with the alerts), [replicas.md](replicas.md) (`--max-rate`, each
replica's) and [signin.md](signin.md); and, for where its line falls, to
[expectations.md](expectations.md) (#81) and `--expect` (#86).

## Why

Before `lapilli serve` goes into production beside a busy alerting stack,
the people who run it ask:

- Will it read my rules, and how many of them can it replay?
- Can it reach the history, and does it sign in to it?
- Does the Grafana token have the role it needs: to read the rules, the
  data sources, the notification templates; to write, with `--changes`?
- How many requests will an hourly run make, and the 28-day backfill, and
  how long, and how many nights, will the backfill take under `--max-rate`,
  `--catch-up-rate` and `--catch-up-hours`?
- How much memory should the pod ask for?

Today they find out by deploying. `lapilli serve` starts the backfill at
once (or in the first `--catch-up-hours`), so its requests are the first to
meet the quota; a token without a scope is a line in the log after; memory
is found when the pod is killed. A schema change is applied as serve starts
(`ledger.Open`, `migrate`), after which a release before it can't open the
ledger, and going back takes the copy kept before the upgrade, if one was
([expectations.md](expectations.md) says so of schema 020).

Some of this can be had already, and some can't ("Alternatives" below says
how far each goes). The rest needs the flags serve takes, read as serve
reads them, and a few cheap reads.

## What it is

```
lapilli preflight [serve's flags] [--json] [--fail-unknown] [--offline]
```

It takes the flags `lapilli serve` takes, read by serve's own reader (the
`takes` of `runServe` and `parseFlags`): a flag serve refuses, or a
combination of them, it refuses the same way, and exits 2. Its own flags
are taken out first, as serve refuses `--json` (one of verdict's flags it
has no use for, `parseFlags`). Then it makes the fewest, cheapest reads
that answer each question, says of each item **ok**, **problem** or
**unknown**, with why and, for a problem, what to change, and estimates the
requests, the time, the nights and the memory from what it read. It writes
nothing, and runs no replay; both are held by the code, not only said
("What it never does").

**The name** is `lapilli preflight` (decided, below). `check` is taken: it
is what `--check-replay` (`cmd/lapilli/check.go`, `internal/serve/check.go`),
`--check-content` and `--check-firings` do to rules, and `lapilli check`
would read as one of them. `lapilli serve --dry-run` would read as serve
without its writes, the backfill included.

## How it runs

1. **The flags.** Its own taken out; the rest read by serve's reader,
   refused as serve refuses them (exit 2); the ports' addresses read for
   what they expose, never bound.
2. **The rules**, read once from where serve would read them.
3. **Each API once**: the history's probe and its ALERTS count, Grafana's
   data sources and notification reads, the sign-in's discovery and keys,
   each on-call feed; each counted as it is asked, and none asked that the
   allowlist doesn't hold (below).
4. **The files and the ledger**, read as serve reads them, the ledger
   read-only.
5. **The estimates**, worked out from what 2 to 4 read, with nothing more
   asked.
6. **The report**, as text or JSON, and the exit status.

Each step goes on when one before had a problem, so one run says
everything that is wrong; an item that needs what couldn't be read (the
estimates without the rules, say) is unknown, and says what it needed.

## Flags

Every flag of `lapilli serve`, as serve takes it, and these of its own:

| Flag | |
|---|---|
| `--json` | the report as JSON (below) |
| `--fail-unknown` | exit 1 for an unknown, as for a problem |
| `--offline` | ask nothing: the items that need a request are unknown, and the rest (the flags, the rule files, the other files, the estimates of requests and time) are worked out |
| `--preflight-timeout DUR` | the whole of it (default 2m) |

`--timeout` stays serve's (a run's time), read for the estimates, so that
the flags a Deployment gives serve can be given here unchanged.

**Where to run it.** The items that need serve's credentials (the history,
Grafana's token, the clouds, the ledger) are run where serve runs: in the
cluster, with serve's service account, as a Job or `kubectl run` of the
image, never in CI with the production credentials CI would then hold
(the direction [backtests.md](backtests.md), #89, goes). In CI, run it
`--offline` against the values a pull request changes: the flags, the rule
files, `--config` and the estimates of requests and time need no
credential.

## What it checks

Each item has an id that stays the same between releases (`--json`), held
as [docs/numbers.md](../numbers.md) holds what keeps its name. An item that
doesn't apply to the flags given isn't listed.

### The rules

| Id | Read | Says |
|---|---|---|
| `rules.read` | once, as serve's watcher does: the files (`rules.ReadPicked`), or one `GET /api/v1/rules` (`source.Loaded`), one Grafana export (`source.Grafana`), one read of the ruler, of each `--alarms`, of `--loki-rules` | how many alert and recording rules, of each kind (Prometheus, Grafana, Loki, a cloud's alarms); a source that can't be read is a problem, with what it answered |
| `rules.replayable` | nothing more | the rules that can't be replayed, and why (`Unsupported`), as the rules list would say; the names told apart (`source.TellApart`); documents `--rules-select` passed over |
| `rules.history` | `--rules-history`'s directory | its versions, and whether one is from before the backfill starts (serve warns of it at its first start) |
| `expressions` | nothing: read, never sent | `--impact` and `--guard`, and the expressions proposed beside this (`--mark`, #90; impact signals, #88) when they are built, read as Lapilli reads an expression (brackets, strings and comparisons, 16 KiB and 32 levels at most, as near misses read them). Lapilli has no PromQL parser of its own, so whether the history takes one as written is unknown here; the first run says |

### The history

| Id | Read | Says |
|---|---|---|
| `history.reach` | one instant query, `vector(1)`, to `--prometheus`, signed as serve signs (`--auth`, `--bearer-token-file`, the tenant) | ok, and how long it took; a problem for a name that doesn't resolve, TLS, a refused connection, 401 or 403 (not let in), with the cloud's credential chain named; 429 says the quota is already in use |
| `history.loki` | `GET /loki/api/v1/labels` over the last five minutes, with `--loki-token-file` and the tenant | as above, for Loki |
| `history.grafana` | for each data source the Grafana rules use, `GET /api/datasources/uid/UID` (as `source.GrafanaDataSources` reads it), and `vector(1)` through its proxy for one of Prometheus's kind; 20 data sources at most | a data source the token can't read, or query, is a problem: its rules won't be replayed |
| `history.alerts` | `sum by (alertname) (count_over_time(ALERTS{alertstate="firing"}[1d]))`, the selector serve's check reads (`alertsSeries`, `internal/serve/check.go`), and `GRAFANA_ALERTS`'s for Grafana rules where Grafana writes it | the samples each alert name fired in the last day, for the memory estimate below. None is **unknown**, never a problem: ALERTS has a series for each alert, and only while it is active, so a quiet day has none, as does a history that keeps no ALERTS, and the two can't be told apart from one query |
| `history.intervals` | `/api/v1/status/config` and `/api/v1/targets?state=active`, once each, with `--check-content` and no `--scrape-interval` | ok, or unknown when not served: the range check then counts with the global interval, as serve does |

`vector(1)` reads no series: it is the cheapest query every
Prometheus-compatible API answers, and it goes through what every query
does (the name, TLS, the cloud's signature, a tenant, a gateway's quota).
The ALERTS query reads a day of the firing alerts' samples, one series an
alert; where the history keeps no ALERTS (ROADMAP.md says which managed
ones are known to keep it), it reads nothing, and the estimate falls back
to the caps' bound. Grafana's rules whose firings serve reads from
Grafana's own state history ([grafanahistory.md](grafanahistory.md)) are
unknown here: that history isn't read for an estimate.

### Grafana's token, and writes

| Id | Read | Says |
|---|---|---|
| `grafana.notifications` | the templates, contact points and policies (`source.GrafanaNotifications`), with `--check-content` | a 403 is a problem: the template part of the content check stays as it was |
| `grafana.write` | with `--changes-grafana-token-file`: `GET /api/access-control/user/permissions`, which lists the caller's own actions | where it answers, ok when the actions include writing alert rules, a problem when not; where it isn't served, unknown: no other API says what a token may write without its writing, and the first change approved finds out. Grafana's documentation says its RBAC API is Grafana Enterprise's and Grafana Cloud's; whether OSS answers this one path is checked against a real OSS Grafana before step 2 builds the item |
| `changes.cloud` | AWS (`--changes-aws-role`): STS's `AssumeRole`; Google Cloud (`--changes-google-service-account`): IAM Credentials' `generateAccessToken`; Azure (`--changes-azure-client-id`): a token for that identity from Microsoft Entra, as serve's credential chain gets one; OCI (`--changes-oci-config`): the config file and the API key it names, read | the identity can be taken, or the key read: a credential is made, nothing is written. Whether it may write the rules or alarms named is **unknown** on all four in the first step |
| `changes.ruler` | nothing | a Mimir's or a Cortex's `rules:write` can't be known without writing: unknown |

### Files, the ledger, the ports

| Id | Read | Says |
|---|---|---|
| `inputs.files` | `--incidents` (`incident.Load`), `--deploys` (`serve.LoadDeploys`), `--config` (`serve.ReadConfig`), each as serve reads it; `--on-call`, one fetch of each feed | a file that serve would refuse, with its line; an incidents file with no `incidents` key, which serve keeps from taking every incident away |
| `secrets.files` | each token, key and webhook file | readable; a token of 16 characters at least and each different from the others (as `tokenOK` refuses one shared); a session key of 32; a webhook file holding an https URL. Whether a webhook takes a post is unknown: knowing it would post |
| `ledger.sqlite` | the directory: there, writable by this user (`access(2)`, nothing written), its free space (`statfs`); the ledger, opened read-only, its `PRAGMA user_version` | a new ledger; the same schema as this release; or **"serve upgrades the ledger from 018 to 019 as it starts, and a release before 019 can't open it after: keep a copy first"**; a newer schema is a problem (serve refuses it). Where the ledger's volume can't be mounted (below, the chart), unknown |
| `ledger.postgres` | connects with `--ledger-url-file`'s string; `SELECT version FROM schema_version`; whether the user may create in the schema; whether a serve holds the ledger's lock now (`pg_locks`) | as for SQLite, and the privileges a migration needs; never takes the lock |
| `signin` | `--oidc-issuer`'s discovery (`OIDC.Discover`), `--proxy-jwt`'s keys, once | what serve would say at start: an issuer that differs, an endpoint that isn't https |
| `access` | nothing | who may change what, in the sentence serve logs at start: with no roles, everyone may only look on a listener beyond loopback (`LoopbackOnly`); `--user-header` without a proxy in front; each port and what it takes |

## Estimates

**Requests**, counted from the rules read, without asking for any of them.
A planner, `replay.Plan`, counts what `replay.Run` would ask: each rule's
expressions (`queries`), in requests of 1,440 steps each (`chunk`, a day at
a 1-minute step), on a grid from the Unix epoch so that rules alike ask for
the same pieces (`chunks`), the reads that rules alike share counted once
(`newShared`). A test holds the planner to the replay: over random rules,
steps and periods, the requests `Plan` counts are exactly those `Run`
sends to a fake history that counts them, so the estimate can't drift from
the code. Serve's runs are counted as `runs` and `find` make them: each
rule's replay started earlier by its margin (`margin`: `for`,
`keep_firing_for` and the group's interval, rounded up to 15m, 1h, 6h, 1d
or 7d), from the cursor less `--settle`, every `--every`; and the backfill
a day at a time from `--backfill` before now, with no settle. Each count is
by the API asked (`--prometheus`, each Loki, each Grafana data source):

| Id | What |
|---|---|
| `estimate.hourly` | an hourly run: its replays, one `--impact` read and one `--guard` query a run, and a day of them |
| `estimate.backfill` | the backfill: requests in all, its pieces, the most in one, and one `--guard` query before each day of it |
| `estimate.nights` | how many nights the backfill takes, with `--catch-up-hours` |
| `estimate.beside` | beside the runs: reading the rules every `--watch` from an API (one request a minute each, to the history's API for `--loaded-rules`), each `--alarms` every `--alarms-every`, the replay check (some three requests a day for each series it reads, `checkDay`), the scrape intervals (two a day), Grafana's templates (three an hour), the on-call feeds |
| `estimate.memory` | below |

**Left out of the estimate**, and said so in it: the `--alerts` mode, which
reads ALERTS and replays nothing; the weekly digest's backtests (its
suggestions and near misses); change reviews, a week of each new version
of the rules; and what people and agents ask for (a proposal's backtest,
MCP's `compare`, the settings tried). Each is bounded where it is
([serve.md](serve.md)), and counted after deploy in
`lapilli_serve_history_requests_total` ([quota.md](quota.md)).

**Time.** With `--max-rate`, the pace spaces requests evenly, so a count is
a floor: the hourly runs take at least their requests over the rate. While
catching up, `--catch-up-rate` holds each API to its rate besides
`--max-rate` ([quota.md](quota.md), 4), so the backfill's floor is its
requests over the lower of the two. Two problems follow:

- an hourly run whose requests at the rate take longer than `--every`: the
  runs fall behind, and never catch up;
- a backfill day whose requests take longer than `--timeout`: serve halves
  the piece and goes on (`runs`), so it is said, with how small the pieces
  will be, and not called a problem.

It also says what share of `--max-rate` the runs use once caught up, and,
with several replicas, that each asks at it ([replicas.md](replicas.md)).
Without either rate, how long is the API's to say: unknown, with the floor
at `--parallel` requests at once. `vector(1)`'s time is no measure of a
range read's.

**Nights.** With `--catch-up-hours`, a catch-up runs only within them: a
day of it begins only while the hours last, and one the hours end in is
the last of that night ([quota.md](quota.md), 4). The estimate counts the
nights so: from the first run in the hours (a run falls anywhere in its
`--every`), day after day at the floor above, until the hours end, then
the next night. Nothing newer than the catch-up is recorded meanwhile, so
it says too that the pages will show the ledger days behind until then;
quota.md's advice holds, a `--backfill` that fits a night where that
matters. It can't say how often the history will answer 429: with
`--yield-on-429`, each 429 ends a run's requests and holds the runs to the
next `--every` (and `--yield-for` longer), and with `--guard`, a guard that
returns something holds them too. Each makes the backfill take longer than
the estimate, which says so: the nights are a floor.

**Memory.** From `history.alerts`: an alert name's samples fired in the
last day, at the evaluation interval its rule's group has, are about what
a day's read of its rule at `--step` holds (scaled by the one over the
other; a little more, as a replay's read also holds the steps it was
pending), at 64 bytes a sample at the peak (`prom.SampleBytes`); a read
holds `--max-samples` at most. The peak is near the `--parallel` largest at
once, beside the cache's 32 MB and what the ledger holds open. It says a
request and a limit, and, run under a memory limit (the cgroup's
`memory.max`), whether that limit is under the estimate. A rule whose day
would pass `--max-samples` is named: serve won't replay it, and says why.
With no ALERTS to read, it says the bound the caps give, as a bound:
`--parallel` times `--max-samples` times 64 bytes, some 5 GB at the
defaults. A memory limit under that bound is **unknown**, not a problem:
what a run holds can't be told from nothing read, and the bound is reached
only when every read at once holds `--max-samples`.

**What it finds, say.** 200 Prometheus rules, a `for` of 5 minutes or less,
the defaults: an hourly run replays each from 2h15m back (settle, every,
margin), one request each and two when it crosses a piece's edge, about 220
a run, 12% of `--max-rate 0.5`; the backfill, two requests a rule a day,
about 11,200, at least six hours at 0.5 a second, each day's 400 in some 13
minutes, within `--timeout`. Three rules with `for: 1d` have a margin of
seven days, and each run reads eight or nine days of each again: some 25
requests a run, a tenth of all, for three rules. That is the kind of thing
deploying shows a week later, on a quota's graph. With `--catch-up-hours
01:00-06:00` and `--catch-up-rate 0.5`, a night holds some 9,000 of the
backfill's requests: two nights at least, and more for each night a 429 or
the guard holds it. The output below is of this deployment.

## Output, and where it shows

```
lapilli preflight: 26 items: 20 ok, 0 problems, 6 unknown; 17 requests asked
ok       rules.read          206 alert rules, 40 recording rules, from /etc/lapilli/rules
ok       rules.replayable    203 can be replayed; 3 can't, each with why (below)
ok       history.reach       [--prometheus] answered vector(1) in 41ms, signed for google
unknown  history.alerts      no firing alert in the last day, or no ALERTS kept: memory is the caps' bound
unknown  grafana.write       this Grafana doesn't say what a token may write without its writing
ok       ledger.sqlite       schema 018; serve upgrades it to 019 as it starts, and a release before
                             019 can't open it after: keep a copy first
...
Estimates
  hourly run    246 requests to --prometheus, the guard's among them: 14% of --max-rate 0.5
  backfill 28d  11,900 requests, 28 pieces of a day, the most 427: at least 6h37m at 0.5 a second
  nights        2 at least, in 01:00-06:00 at --catch-up-rate 0.5; each 429 or guard held, more
  memory        no ALERTS read: the caps' bound, 5.1 GB (8 x 10,000,000 x 64 bytes)
Left out: change reviews, the digest's backtests, what people and agents ask for
```

- **What it prints is redacted** as `forComment` redacts a pull request's
  comment (`cmd/lapilli/verdict.go`): the URLs given (`--prometheus`,
  `--loki`, `--grafana`, `--ruler`) as their flag's name, their hosts and
  user names, any IP address and port, the tenants and a `--gcp-project`.
  What a server answered, in a problem's why, goes through `verdict.Clean`
  and is cut at 200 bytes. It is for CI logs and tickets, which more people
  read than serve's log.
- **`--json`**: `{"version": 1, "lapilli": "vX.Y.Z", "items": [{"id",
  "state", "what", "why", "fix"}], "estimates": {...}, "left_out": [...],
  "asked": {"requests", "by": {...}}}`, redacted alike. The ids and the
  fields are documented and stable; a new check is a new id.
- **Exit status**: 0 with no problem (unknowns are said, and pass); 1 with a
  problem, or with `--fail-unknown` an unknown; 2 when the flags are wrong,
  as serve exits.
- **Where it runs**: by hand, in the cluster, with serve's service account;
  `--offline` in CI (above).
- **Not as a chart hook** (decided, below). A Helm `pre-install` or
  `pre-upgrade` Job doesn't fit the chart as it is. With one pod, the
  Deployment is `Recreate` (`deployment.yaml`), and the SQLite ledger is a
  ReadWriteOnce claim the chart makes with the release (`service.yaml`).
  At `pre-install` the claim isn't there yet. At `pre-upgrade` the running
  pod holds it, so a Job scheduled on another node can't attach it
  (Multi-Attach) and stays Pending. A hook would have to leave the ledger
  out, and `ledger.sqlite` would be unknown there. The hook is off, and
  isn't built until a design of it fits the chart. With a Postgres ledger
  the hook reaches the ledger over the network; that is one reason to
  revisit it.

## What it never does

Each is held by the code and a test, not only said:

- **No request off its list.** Every client it makes (the history,
  Grafana, Loki, a ruler, the clouds' credential chains, the sign-in's
  discovery and keys, the on-call feeds) sends through one `RoundTripper`.
  It refuses, before anything is sent, a request whose method, path and
  query aren't on an allowlist, and counts it. The two PromQL queries are
  matched by their exact text, and Loki's times by their keys and a range
  of the last ten minutes. The credential endpoints are matched by host
  and action, `AssumeRole` among them.
- **No write to the ledger.** SQLite is opened `mode=ro`, with `PRAGMA
  query_only = ON`, and so that nothing is made or written beside it (As
  built, below). Postgres is connected with
  `default_transaction_read_only = on`, so a write fails at the server,
  whatever asked it.
- **No call that opens the ledger for serve.** `internal/preflight` reads
  the ledger through a probe of its own (`ledger.Probe`: the schema
  version, the privileges, the lock's holder). A test reads the package's
  source (`go/ast`) and fails on any call of `ledger.Open`,
  `ledger.OpenPostgres`, `ledger.OpenPostgresAlone` or `serve.New`.
- **Proved by count.** One test runs it with every flag against fake
  servers: a Prometheus, a Grafana, a Loki, an OIDC provider and a cloud's
  token endpoints, beside a SQLite ledger and, with
  `LAPILLI_TEST_POSTGRES`, a Postgres one. It counts what each fake was
  sent, and fails on one request off the allowlist. It also fails on one
  write: the SQLite file's bytes changed or a `-wal` made, or Postgres's
  `pg_stat_user_tables` showing a row inserted, updated or deleted.
- **No heavy query, no replay.** No replay and no backfill; no rule's
  expression, `--impact`, `--guard`, nor anything else a person wrote is
  evaluated, over a range or at an instant (the `expressions` item reads
  them). The only queries are `vector(1)` and the ALERTS count over a day:
  the allowlist holds no other. Why not each rule's expression once, to see
  that the history takes it? 400 rules are 400 queries, minutes under a
  quota, and an expression's cost is known only once it has run. The first
  run says which the history refuses as written (Run history, the rule's
  page).
- **Nothing sent, nothing bound, nothing judged.** Not Slack, a webhook, a
  tracker, the outbox or a change recipe; no lease; no port bound. It counts
  requests, not firings: no rule's firings, budget or catches are worked
  out (below).

Every request it makes is counted in its output, by the API, as `lapilli
verdict` counts its own (`prom.Counting`).

## How it relates to `--expect`

A pre-flight checks the deployment, not what the rules do. The line:

- **Pre-flight**: what can be said without the history's values over time.
  That the rules read and parse, how many can be replayed, that the history
  answers and lets serve in, what the tokens may do, how many requests the
  runs will make, how long, over how many nights, and in what memory.
- **`--expect` and the expectations' steps**: anything that needs a rule
  replayed over a period. How often it fires, what it catches, whether it
  keeps a budget: `lapilli verdict --compare --expect` before a change is
  merged (step 1, #86), serve after it is deployed (step 2), drafts (step
  3).

So when serve takes an expectations file of its own (`--expect`, step 2),
a pre-flight reads it as serve will, strictly (`verdict.ReadExpectations`),
and says what is wrong with the file for this deployment. That is an
expectation that names no rule read, or a catch's incident that is
nowhere serve would find it: not in the incidents file, nor among the
ledger's own (the `L-` ids written in the UI and the API, and those copied
in), read from the ledger as the probe reads it. It never says whether one
is met, not even over the last day: that would be a replay and a verdict,
which are the other's. The ALERTS count is read for memory alone, and isn't
shown by rule, where it would read as how often a rule fires.

With [backtests.md](backtests.md) (proposed beside this, #89), the
backtest lane's share of `--max-rate` is taken out of what the runs have,
in the estimate, and its trust file is read and its issuers' discovery
fetched; the planner here is what refuses there a backtest that couldn't
finish in its time.

## What comes from outside

It opens no port and takes no token. What it reads from outside is the
answers of the APIs serve reads, with serve's readers and their bounds
([serve.md](serve.md), "What a history API answers"; no redirect
followed). It is run with serve's own credentials, so it can reach nothing
serve can't, and the allowlist keeps it to less. What it prints is redacted
(above): a secret's file is said by its path and length, never its text;
a webhook's file by whether it holds an https URL, never the URL; rule and
data source names from an API with what turns text around (a direction
mark, a control character) as U+FFFD (`verdict.Clean`), and escaped in the
JSON.

## Cost and bounds

A few dozen requests at most, whatever the number of rules: one probe of
each history, one ALERTS query each, two scrape reads, one read of each
rule source, 20 data sources at two each, Grafana's three templates reads
and one permissions read, a discovery document and keys, a credential from
each cloud's chain, each on-call feed. Each is asked under `--max-rate`,
as serve asks, within ten seconds, and the whole within
`--preflight-timeout`. So are the estimates, which ask nothing but count
a run's requests a run at a time over a day of runs, and the backfill's a
day at a time: a pull request's flags and rules (a run every minute, ten
years of backfill, `--backfill`'s most, thousands of rules) make that
seconds, and what isn't counted once the time is up is unknown. It holds
the rules in memory as serve's watcher does, and nothing else of size.

## Alternatives

- **Use what exists.**
  - *`lapilli verdict --rules ... --prometheus ... --since 1d`* reads the
    rules and the history as serve does, and, past 15 seconds, `--progress`
    says the requests, those asked again and those answered 429. That
    answers "will it read my rules" and "can it reach the history". But it
    replays: a day of every rule, about a backfill's first piece, which is
    the heavy query a pre-flight is not to make. Its count isn't serve's
    (no settle, no hourly margins, no rule reads, no replay check, no guard,
    no catch-up hours). It knows none of serve's own flags (the ledger, the
    sign-in, the tokens, `--config`, the ports), and says nothing of memory.
  - *`lapilli verdict --check-content --rules ...`* reads and checks the
    rules with no history at all: `rules.read`, no more.
  - *Deploying and watching*: `/readyz` (ready once a run is recorded), the
    Run history page (held runs among them), `/metrics`
    (`lapilli_serve_history_requests_total`, the runs by kind and result),
    the log. Each says it after the backfill has begun and the ledger has
    been migrated; they are how the estimate is checked after.
  - *serve itself*, which refuses a wrong combination of flags at start
    (exit 2) and says who may change what: the `access` item, no more.
- **`lapilli serve --dry-run`.** One command for both, but it reads as serve
  without its writes, the backfill included, and its output and exit status
  would be serve's.
- **A page in serve.** It needs serve deployed, which is what this is to
  come before. The same items could be shown there later (open questions).
- **Each rule's expression asked once** (an instant query each). It says
  which the history refuses as written, before the first run does; at a
  cost a pre-flight shouldn't have by default (above). An opt-in later.

## Not in this design

- How often the rules will fire, and what they catch: `--expect`, and the
  expectations' steps.
- How fast the ledger grows. Its size by firings is in serve.md's table
  ("Keeping it fast"); how many firings a deployment will have is a replay.
- Whether a webhook, Slack or a tracker takes what serve sends: knowing it
  would send.
- How often the history answers 429: the API's to say.

## Questions

**Decided, maintainer's call, 2026-11-16:**

1. **The name** is `lapilli preflight`, not `lapilli serve --preflight`.
2. **No chart hook**: it is off, and isn't built until a design of it fits
   the chart's `Recreate` Deployment and ReadWriteOnce ledger ("Output, and
   where it shows").

**Open:**

3. **Writes, where a cloud says without writing**: Google Cloud's
   `testIamPermissions` and Azure's permissions of the caller on a scope
   say what may be written; AWS needs `iam:SimulatePrincipalPolicy`, a
   permission of its own; OCI's policies are read by a policy's own API.
   Ask them in step 2, or leave writes unknown?
4. **Each rule's expression** asked once, behind `--queries`?
5. **In serve**: the same items on the Settings page, worked out once a
   day, for a deployment that has drifted (a token rotated without its
   scope)?

## As built (step 1)

Where building it found the design said more, or otherwise, than the code
can do:

- **vector(1) is a range of one step.** Lapilli's client asks
  `/api/v1/query_range`, never `/api/v1/query`, so `vector(1)` and the
  ALERTS count are asked as a range whose start is its end. The allowlist
  holds that form: the query's text exactly, the start and end within the
  last ten minutes, a step, nothing else.
- **SQLite read-only takes `immutable` too, or more.** Opened `mode=ro`
  alone, SQLite makes a `-wal` and a `-shm` beside a ledger that has none,
  as a ledger serve closed hasn't (`TestProbeSQLite` fails so); makes the
  `-shm` beside a `-wal` that has none (a serve that stopped hard and its
  `-shm` cleared, a copy of the two files); and writes in a `-shm` that is
  there, a reader's mark, or the whole index where it finds it stale. So
  the probe opens it by what is beside it (`ledger.Probe`,
  `TestProbeSQLiteBeside`): with no `-wal`, `immutable=1`; with a `-wal`
  and a `-shm`, a serve's or one left, `readonly_shm=1`, so that SQLite
  reads the index without writing it, or, where no serve holds it, builds
  one of its own in memory; with a `-wal` and no `-shm`, through a file
  system that has no write (`modernc.org/sqlite/vfs`), the index kept in
  memory (`locking_mode=EXCLUSIVE`). Each reads what the `-wal` holds, a
  migration not yet checkpointed among it; 0.53.0 to 0.55.0 made the
  `-shm` in the third case, 32 KiB, which a serve run as another user
  might not then open.
- **`history.grafana` asks through the proxy for Loki's data sources
  alone.** serve replays Grafana's rules of PromQL against `--prometheus`,
  not through Grafana's data source proxy; only a Loki data source's are
  asked through it (`source.Logs`). So the item reads each data source
  (`GET /api/datasources/uid/UID`, twenty at most), and, for each of
  Loki's kind, its labels over the last five minutes through the proxy;
  `vector(1)` through the proxy would check a path no replay takes.
- **Not every credential chain takes a client.** Every client of
  Lapilli's own goes through the allowlist (`internal/egress`,
  `TestEveryClientWrapped` reading the source), and so do AWS's
  configuration, Azure's credentials and Google's token source, which take
  one while it is set (`TestGoogleThroughGuard`, `TestAzureThroughGuard`).
  What the chains do of their own doesn't: Google's metadata server (GKE's
  Workload Identity), and whether it is there at all off Google Cloud,
  asked by `cloud.google.com/go/compute/metadata`, whose client takes no
  transport; the Azure CLI, a process, which may write its token cache;
  OCI's instance and resource principals and OKE's workload identity,
  which ask with the OCI SDK's own client, the auth service's POST among
  them; and AWS's `credential_process` and Google's executable credentials,
  which start a process. Each reads a credential, outside the allowlist's
  count; the help, the README and the changelog say so.
- **The Postgres probe sets its session read-only once connected**
  (`SET SESSION CHARACTERISTICS AS TRANSACTION READ ONLY`), as serve's
  readers do: a pooler in session mode refuses
  `default_transaction_read_only` among a connection's startup parameters.
- **What a server answered is redacted whole, then cut** at 200 bytes, so
  that a host cut in two is still found; and what the run learns is
  redacted too: the ledger's server, an on-call feed's URL, the keys a
  sign-in's discovery names. So are the ledger's user and database, as
  pgx reads them (the string, a service file, `PGUSER` and `PGDATABASE`,
  the user it runs as), which a Postgres that refuses the connection
  names; and the session that holds the ledger's lock is said by its
  number and when it connected, not its role, its name or its address.
- **Every text of every item is shown as it reads, in one place.** Not
  only what a server answered: why a rule can't be replayed, in a rule
  file's own words (a classic condition's value), is outside text too, and
  was printed as it came in 0.53.0 to 0.55.0. The report's redaction runs
  `verdict.Clean` over every text of every item and what is left out,
  before it redacts and cuts, for the text and the JSON alike; the marks
  that say where a server's answer is hold a text drawn at random as the
  process starts, so a file can't forge one.
- **The nights count the days a catch-up waits.** Outside its hours its
  cursor stands, and each day is a day more to catch up (quota.md, 4): a
  catch-up day, on average, is added for each day that passes, and a night
  that holds no more than a day of it never catches up, a problem.
- **Each API is timed of its own.** `--max-rate` is each API's: a run's or
  a day's floor is the longest of its APIs' at their rate. Without
  `--max-rate`, the days a catch-up doesn't pace aren't timed: the floor
  is the catch-up's.
- **`--offline` reads no secret.** The files of tokens, keys and webhooks,
  and `--ledger-url-file`, which serve's flags read at start, are taken as
  given: CI doesn't hold them. `secrets.files` is unknown.
- **The allowlist reads rulers and a cloud's alarms by method and path.**
  Their lists page, so a `GET` under the URL given takes any query; a
  CloudWatch read is a `POST` whose `X-Amz-Target` is `DescribeAlarms`. A
  credential endpoint is matched by host and action: STS's `AssumeRole`,
  `AssumeRoleWithWebIdentity` and `GetCallerIdentity`, Microsoft Entra's
  token and discovery paths, Google's token endpoints and IAM Credentials'
  `generateAccessToken`, the instance metadata's credential paths.
- **`grafana.write` is unknown in step 1.** It isn't asked; step 2 builds
  it, once OSS is checked.
- **An AWS load balancer's keys can't be fetched before a request.** They
  are fetched by the key each token names: `signin` is unknown for
  `--proxy-jwt aws-alb`.
- **`verdict.Clean` writes a control or format character as `\u{...}`**,
  not U+FFFD, in what a server answered.
- **A write to Postgres is told by its rows.** `pg_stat_user_tables`
  counts a session's writes when its statistics are flushed, not as it
  writes, so the tests read a digest of every row before and after
  (`ledgertest.Digest`), and a write asked on the probe's session is
  refused by the server.
- **The memory's limit is twice its request**: Go's collector lets the heap
  grow to about twice what is live before it collects (`GOGC=100`).

## Steps

1. The command: serve's flags, the rules, the history's probe and the
   ALERTS query, the files, the ledger read-only, the sign-in, the
   estimates of requests, time, nights and memory, `--offline`, the text
   and JSON, redacted, the exit status; the allowlist, the read-only
   ledger, the source check and the test by count; the planner held to
   `Run` by a test.
2. Grafana's permissions, once OSS is checked; the clouds' that can be
   asked.
3. The backtest lane ([backtests.md](backtests.md)), when it is built.
