# `lapilli preflight`: what serve will meet, before it is deployed

Status: proposed. Under [GOVERNANCE.md](../../GOVERNANCE.md) a new command
is designed first, as a pull request. It adds nothing to the ledger, takes
no input from outside but the answers of the APIs serve already reads, and
changes no verdict. The companion to [serve.md](serve.md) (runs, the
ledger, the bounds on what a history answers), [replicas.md](replicas.md)
(`--max-rate`, each replica's) and [signin.md](signin.md); and, for where
its line falls, to [expectations.md](expectations.md) (#81) and `--expect`
(#86).

## Why

Before `lapilli serve` goes into production beside a busy alerting stack,
the people who run it ask:

- Will it read my rules, and how many of them can it replay?
- Can it reach the history, and does it sign in to it?
- Does the Grafana token have the role it needs: to read the rules, the
  data sources, the notification templates; to write, with `--changes`?
- How many requests will an hourly run make, and the 28-day backfill, and
  how long will the backfill take under `--max-rate`?
- How much memory should the pod ask for?

Today they find out by deploying. `lapilli serve` starts the backfill at
once, so its requests are the first to meet the quota; a token without a
scope is a line in the log after; memory is found when the pod is killed.
A schema change is applied as serve starts (`ledger.Open`, `migrate`),
after which a release before it can't open the ledger, and going back takes
the copy kept before the upgrade, if one was ([expectations.md](expectations.md)
says so of schema 020).

Some of this can be had already, and some can't ("Alternatives" below says
how far each goes). The rest needs the flags serve takes, read as serve
reads them, and a few cheap reads.

## What it is

```
lapilli preflight [serve's flags] [--json] [--fail-unknown]
```

It takes the flags `lapilli serve` takes, read by serve's own reader (the
`takes` of `runServe` and `parseFlags`): a flag serve refuses, or a
combination of them, it refuses the same way, and exits 2. Then it makes
the fewest, cheapest reads that answer each question, says of each item
**ok**, **problem** or **unknown**, with why and, for a problem, what to
change, and estimates the requests, the time and the memory from what it
read. It writes nothing, and runs no replay.

**The name.** `check` is taken: it is what `--check-replay` (`cmd/lapilli/check.go`,
`internal/serve/check.go`), `--check-content` and `--check-firings` do to
rules, and `lapilli check` would read as one of them. `lapilli serve
--dry-run` would read as serve without its writes, the backfill included.
`preflight` is what happens before a flight, and runs nothing.

## How it runs

1. **The flags**, read by serve's reader, refused as serve refuses them
   (exit 2), and the ports' addresses read for what they expose, never
   bound.
2. **The rules**, read once from where serve would read them.
3. **Each API once**: the history's probe and its ALERTS count, Grafana's
   data sources and notification reads, the sign-in's discovery and keys,
   each on-call feed; each counted as it is asked.
4. **The files and the ledger**, read as serve reads them, the ledger
   read-only.
5. **The estimates**, worked out from what 2 to 4 read, with nothing more
   asked.
6. **The report**, as text or JSON, and the exit status.

Each step goes on when one before had a problem, so one run says
everything that is wrong; an item that needs what couldn't be read (the
estimates without the rules, say) is unknown, and says what it needed.

## Flags

Every flag of `lapilli serve`, as serve takes it, and three of its own:

| Flag | |
|---|---|
| `--json` | the report as JSON (below) |
| `--fail-unknown` | exit 1 for an unknown, as for a problem |
| `--preflight-timeout DUR` | the whole of it (default 2m) |

`--timeout` stays serve's (a run's time), read for the estimates, so that
the flags a Deployment gives serve can be given here unchanged. The chart:
`serve.preflight.enabled` (below).

## What it checks

Each item has an id that stays the same between releases (`--json`). An
item that doesn't apply to the flags given isn't listed.

### The rules

| Id | Read | Says |
|---|---|---|
| `rules.read` | once, as serve's watcher does: the files (`rules.ReadPicked`), or one `GET /api/v1/rules` (`source.Loaded`), one Grafana export (`source.Grafana`), one read of the ruler, of each `--alarms`, of `--loki-rules` | how many alert and recording rules, of each kind (Prometheus, Grafana, Loki, a cloud's alarms); a source that can't be read is a problem, with what it answered |
| `rules.replayable` | nothing more | the rules that can't be replayed, and why (`Unsupported`), as the rules list would say; the names told apart (`source.TellApart`); documents `--rules-select` passed over |
| `rules.history` | `--rules-history`'s directory | its versions, and whether one is from before the backfill starts (serve warns of it at its first start) |

### The history

| Id | Read | Says |
|---|---|---|
| `history.reach` | one instant query, `vector(1)`, to `--prometheus`, signed as serve signs (`--auth`, `--bearer-token-file`, the tenant) | ok, and how long it took; a problem for a name that doesn't resolve, TLS, a refused connection, 401 or 403 (not let in), with the cloud's credential chain named; 429 says the quota is already in use |
| `history.loki` | `GET /loki/api/v1/labels` over the last five minutes, with `--loki-token-file` and the tenant | as above, for Loki |
| `history.grafana` | for each data source the Grafana rules use, `GET /api/datasources/uid/UID` (as `source.GrafanaDataSources` reads it), and `vector(1)` through its proxy for one of Prometheus's kind; 20 data sources at most | a data source the token can't read, or query, is a problem: its rules won't be replayed |
| `history.alerts` | `count by (alertname) (last_over_time(ALERTS[1d]))`, and `GRAFANA_ALERTS` for Grafana rules | the series the replay check reads (`--check-series`), and the memory estimate below: none in a day, with `--check-replay` on, is a problem (give `--check-series`, or `--check-replay=false`) |
| `history.intervals` | `/api/v1/status/config` and `/api/v1/targets?state=active`, once each, with `--check-content` and no `--scrape-interval` | ok, or unknown when not served: the range check then counts with the global interval, as serve does |

`vector(1)` reads no series: it is the cheapest query every
Prometheus-compatible API answers, and it goes through what every query
does (the name, TLS, the cloud's signature, a tenant, a gateway's quota).
The ALERTS count reads a day of one series' samples, usually few; where the
history keeps no ALERTS (ROADMAP.md says which managed ones are known to
keep it), it says so and goes on.

### Grafana's token, and writes

| Id | Read | Says |
|---|---|---|
| `grafana.notifications` | the templates, contact points and policies (`source.GrafanaNotifications`), with `--check-content` | a 403 is a problem: the template part of the content check stays as it was |
| `grafana.write` | with `--changes-grafana-token-file`: `GET /api/access-control/user/permissions`, which lists the token's own actions | Grafana Enterprise and Grafana Cloud: ok when the actions include writing alert rules, a problem when not. Grafana OSS doesn't serve it: unknown, as no API says what a token may write without its writing; the first change approved finds out |
| `changes.cloud` | with `--changes-aws-role`, AWS STS's `AssumeRole`; with `--changes-google-service-account`, IAM Credentials' `generateAccessToken` | the identity can be taken (a credential is made, nothing is written); whether it may write the rules or alarms named is unknown in the first step |
| `changes.ruler` | nothing | a Mimir's or a Cortex's `rules:write` can't be known without writing: unknown |

### Files, the ledger, the ports

| Id | Read | Says |
|---|---|---|
| `inputs.files` | `--incidents` (`incident.Load`), `--deploys` (`serve.LoadDeploys`), `--config` (`serve.ReadConfig`), each as serve reads it; `--on-call`, one fetch of each feed | a file that serve would refuse, with its line; an incidents file with no `incidents` key, which serve keeps from taking every incident away |
| `secrets.files` | each token, key and webhook file | readable; a token of 16 characters at least and each different from the others (as `tokenOK` refuses one shared); a session key of 32; a webhook file holding an https URL. Whether a webhook takes a post is unknown: knowing it would post |
| `ledger.sqlite` | the directory: there, writable by this user (`access(2)`, nothing written), its free space (`statfs`); the ledger, opened read-only, its `PRAGMA user_version` | a new ledger; the same schema as this release; or **"serve upgrades the ledger from 018 to 019 as it starts, and a release before 019 can't open it after: keep a copy first"**; a newer schema is a problem (serve refuses it) |
| `ledger.postgres` | connects with `--ledger-url-file`'s string; `SELECT version FROM schema_version`; whether the user may create in the schema; whether a serve holds the ledger's lock now (`pg_locks`) | as for SQLite, and the privileges a migration needs; never takes the lock |
| `signin` | `--oidc-issuer`'s discovery (`OIDC.Discover`), `--proxy-jwt`'s keys, once | what serve would say at start: an issuer that differs, an endpoint that isn't https |
| `access` | nothing | who may change what, in the sentence serve logs at start: with no roles, everyone may only look on a listener beyond loopback (`LoopbackOnly`); `--user-header` without a proxy in front; each port and what it takes |

## Estimates

**Requests**, counted from the rules read, without asking for any of them.
A planner, `replay.Plan`, counts what `replay.Run` would ask: each rule's
expressions (`queries`), over the days each read falls on (`chunks`, a day
at the most, on days from midnight UTC), the reads that rules alike share
counted once (`newShared`). Serve's runs are counted as `runs` and `find`
make them: each rule's replay started earlier by its margin (`margin`:
`for`, `keep_firing_for` and the group's interval, rounded up to 15m, 1h,
6h, 1d or 7d), from the cursor less `--settle`, every `--every`; and the
backfill a day at a time from `--backfill` before now, with no settle.
Each count is by the API asked (`--prometheus`, each Loki, each Grafana
data source):

- **An hourly run**: requests a run, and a day of them.
- **The backfill**: requests in all, its pieces, and the most in one.
- **Beside the runs**: reading the rules every `--watch` from an API (one
  request a minute each, to the history's API for `--loaded-rules`), each
  `--alarms` every `--alarms-every`, the replay check (some three requests
  a day for each series it reads, `checkDay`), the scrape intervals (two a
  day), Grafana's templates (three an hour), the on-call feeds.

**Time.** With `--max-rate`, the pace spaces requests evenly, so a count is
a floor: the backfill takes at least its requests over the rate. Two
problems follow from it:

- an hourly run whose requests at the rate take longer than `--every`: the
  runs fall behind, and never catch up;
- a backfill day whose requests take longer than `--timeout`: serve halves
  the piece and goes on (`runs`), so it is said, with how small the pieces
  will be, and not called a problem.

It also says what share of `--max-rate` the runs use once caught up, and,
with several replicas, that each asks at it ([replicas.md](replicas.md)).
Without `--max-rate`, how long is the API's to say: unknown, with the floor
at `--parallel` requests at once. `vector(1)`'s time is no measure of a
range read's.

**Memory.** From `history.alerts`: an alert name's series in the last day
are about those a day's read of its rule holds, and a series firing all day
at a 1-minute step is 1,440 samples, at 64 bytes each (`prom.SampleBytes`);
a day's read holds `--max-samples` at most. The peak is near the
`--parallel` largest at once, beside the cache's 32 MB and what the ledger
holds open. It says a request and a limit, and, run under a memory limit
(the cgroup's `memory.max`, as in the chart's hook below), whether that
limit is under the estimate. A rule whose day would pass `--max-samples`
is named: serve won't replay it, and says why. Without ALERTS it says the
bound the caps give, as a bound: `--parallel` times `--max-samples` times
64 bytes, some 5 GB at the defaults.

**What it finds, say.** 200 Prometheus rules, a `for` of 5 minutes or less,
the defaults: an hourly run replays each from 2h15m back (settle, every,
margin), one read each and two when it crosses midnight UTC, about 220
requests a run, 12% of `--max-rate 0.5`; the backfill, two reads a rule a
day, about 11,200, at least six hours at 0.5 a second, each day's 400 in
some 13 minutes, within `--timeout`. Three rules with `for: 1d` have a
margin of seven days, and each run reads eight or nine days of each
again: some 25 requests a run, a tenth of all, for three rules. That is
the kind of thing deploying shows a week later, on a quota's graph. The
output below is of this deployment.

## Output, and where it shows

```
lapilli preflight: 24 items: 19 ok, 1 problem, 4 unknown; 17 requests asked
ok       rules.read          206 alert rules, 40 recording rules, from /etc/lapilli/rules
ok       rules.replayable    203 can be replayed; 3 can't, each with why (below)
ok       history.reach       https://prometheus.example.com answered vector(1) in 41ms, signed for google
problem  history.alerts      no ALERTS series in the last day: --check-replay has nothing to set the
                             replay beside. Give --check-series, or --check-replay=false
unknown  grafana.write       Grafana OSS doesn't say what a token may write without its writing
ok       ledger.sqlite       schema 018; serve upgrades it to 019 as it starts, and a release before
                             019 can't open it after: keep a copy first
...
Estimates
  hourly run    245 requests to --prometheus: 14% of --max-rate 0.5
  backfill 28d  11,900 requests, 28 pieces of a day, the most 427: at least 6h37m at 0.5 a second
  memory        a peak of about 180 MB in replays: ask for 384Mi, limit 768Mi
```

- **`--json`**: `{"version": 1, "lapilli": "vX.Y.Z", "items": [{"id",
  "state", "what", "why", "fix"}], "estimates": {...}, "asked": {"requests",
  "by": {...}}}`. The ids and the fields are documented and stable; a new
  check is a new id.
- **Exit status**: 0 with no problem (unknowns are said, and pass); 1 with a
  problem, or with `--fail-unknown` an unknown; 2 when the flags are wrong,
  as serve exits.
- **Where it runs**: by hand; in CI before an upgrade; and, with the
  chart's `serve.preflight.enabled`, as a Helm `pre-install,pre-upgrade`
  hook Job with the Deployment's image, flags, service account, volumes and
  resources: a problem fails the hook, and the upgrade stops. Argo CD runs
  Helm's hooks as its own (PreSync).

## What it never does

- **Write.** Not the ledger: it never calls `ledger.Open` or
  `OpenPostgres`, which migrate and take the writer's lock; SQLite is opened
  read-only, and Postgres asked with `SELECT` alone. Not a rule, an alarm,
  Grafana, a ruler or a cloud; not Slack, a webhook, a tracker, the outbox
  or a change recipe; no lease, and no port bound.
- **Run heavy queries.** No replay and no backfill; no rule's expression,
  `--impact`, nor anything else a person wrote is evaluated, over a range or
  at an instant. The only queries are `vector(1)` and the ALERTS count over
  a day. Why not each rule's expression once, to see that the history takes
  it: 400 rules are 400 queries, minutes under a quota, and an expression's
  cost is known only once it has run. The first run says which the history
  refuses as written (Run history, the rule's page).
- **Judge.** It counts requests, not firings: no rule's firings, budget or
  catches are worked out (below).

Every request it makes is counted in its output, by the API, as `lapilli
verdict` counts its own (`prom.Counting`).

## How it relates to `--expect`

A pre-flight checks the deployment, not what the rules do. The line:

- **Pre-flight**: what can be said without the history's values over time.
  That the rules read and parse, how many can be replayed, that the history
  answers and lets serve in, what the tokens may do, how many requests the
  runs will make, how long, and in what memory.
- **`--expect` and the expectations' steps**: anything that needs a rule
  replayed over a period. How often it fires, what it catches, whether it
  keeps a budget: `lapilli verdict --compare --expect` before a change is
  merged (step 1, #86), serve after it is deployed (step 2), drafts (step
  3).

So when serve takes an expectations file of its own (`--expect`, step 2),
a pre-flight reads it as serve will, strictly (`verdict.ReadExpectations`),
and says what is wrong with the file for this deployment: an expectation
that names no rule read, a catch's incident that no incidents file holds.
It never says whether one is met, not even over the last day: that would be
a replay and a verdict, which are the other's. The ALERTS count is read
for memory alone, and isn't shown by rule, where it would read as how often
a rule fires.

With [backtests.md](backtests.md) (proposed beside this, #89), the
backtest lane's share of `--max-rate` is taken out of what the runs have,
in the estimate, and its trust file is read and its issuers' discovery
fetched; the planner here is what refuses there a backtest that couldn't
finish in its time.

## What comes from outside

It opens no port and takes no token. What it reads from outside is the
answers of the APIs serve reads, with serve's readers and their bounds
([serve.md](serve.md), "What a history API answers"; no redirect
followed), and it is run with serve's own credentials,
so it can reach nothing serve can't.

What it prints goes into CI logs and tickets:

- a secret's file is said by its path and length, never its text; a
  webhook's file by whether it holds an https URL, never the URL;
- a URL given as a flag is printed without its user name and password, as
  `forComment` takes them out (`cmd/lapilli/verdict.go`);
- rule names and data source names from an API are printed with what turns
  text around (a direction mark, a control character) as U+FFFD
  (`verdict.Clean`), and escaped in the JSON.

## Cost and bounds

A few dozen requests at most, whatever the number of rules: one probe of
each history, one ALERTS count each, two scrape reads, one read of each
rule source, 20 data sources at two each, Grafana's three templates reads
and one permissions read, a discovery document and keys, each on-call feed.
Each is asked under `--max-rate`, as serve asks, within ten seconds, and the
whole within `--preflight-timeout`. It holds the rules in memory as serve's watcher
does, and nothing else of size.

## Alternatives

- **Use what exists.**
  - *`lapilli verdict --rules ... --prometheus ... --since 1d`* reads the
    rules and the history as serve does, and, past 15 seconds, `--progress`
    says the requests, those asked again and those answered 429. That
    answers "will it read my rules" and "can it reach the history". But it
    replays: a day of every rule, about a backfill's first piece, which is
    the heavy query a pre-flight is not to make. Its count isn't serve's
    (no settle, no hourly margins, no rule reads, no replay check). It
    knows none of serve's own flags (the ledger, the sign-in, the tokens,
    `--config`, the ports), and says nothing of memory.
  - *`lapilli verdict --check-content --rules ...`* reads and checks the
    rules with no history at all: `rules.read`, no more.
  - *Deploying and watching*: `/readyz` (ready once a run is recorded), the
    Run history page, `/metrics` (runs by kind and result, how long the
    last took), the log. Each says it after the backfill has begun and the
    ledger has been migrated.
  - *serve itself*, which refuses a wrong combination of flags at start
    (exit 2) and says who may change what: the `access` item, no more.
- **`lapilli serve --dry-run`.** One command for both, but it reads as serve
  without its writes, the backfill included, and its output and exit
  status would be serve's.
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

## Open questions

1. **The name**: `lapilli preflight`, or `lapilli serve --preflight`?
2. **The chart's hook**: on by default, so an upgrade that migrates the
   ledger stops until someone says a copy is kept?
3. **Writes, where a cloud says without writing**: Google Cloud's
   `testIamPermissions` and Azure's permissions of the caller on a scope
   say what may be written; AWS needs `iam:SimulatePrincipalPolicy`, a
   permission of its own. Ask them in step 2, or leave writes unknown?
4. **Each rule's expression** asked once, behind `--queries`?
5. **In serve**: the same items on the Settings page, worked out once a
   day, for a deployment that has drifted (a token rotated without its
   scope)?

## Steps

1. The command: serve's flags, the rules, the history's probe and the
   ALERTS count, the files, the ledger read-only, the sign-in, the
   estimates of requests, time and memory, the text and JSON, the exit
   status.
2. Grafana's permissions, the clouds' that can be asked, the chart's hook.
3. The backtest lane ([backtests.md](backtests.md)), when it is built.
