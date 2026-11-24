# `lapilli preflight`: what serve will meet, before it is deployed

Status: accepted; steps 1, 2 and 3 built. Merged on 2026-11-16, before
its week was out, by the maintainer's decision (#92), after a review whose
changes are in it. Step 2, what Grafana's token and each cloud's identity
may write, asked without writing, followed the maintainer's decision of
open question 3 on 2026-11-18. Step 3, the backtest lane, followed the
lane's own first step in serve ([backtests.md](backtests.md), #135). What
building each step found the design said otherwise than the code can do
is in "As built" below.
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
| `rules.sources` | nothing more | only when Grafana's rules name more than one data source for their PromQL (`rules.HistoryUIDs`): unknown, as each is replayed against the one `--prometheus`, and whether they are one Prometheus isn't known here; verdict warns of the same |
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
| `grafana.write` | with `--changes-grafana-token-file`: `GET /api/access-control/user/permissions`, which lists the caller's own actions, each with its scopes; Grafana OSS serves it (12.4 and 13.3, "As built (step 2)") | ok when the actions include what serve's changes send through the provisioning API (a read of the rule and the export; a change, a deletion, a rule made again): `alert.provisioning:write` (an Admin's), or `alert.rules:write`, `:delete` and `:create` with `alert.provisioning.provenance:write` in every folder (`*`, `folders:*`; an empty scope covers none, as Grafana's evaluator has it); a problem naming what it lacks (a Viewer's lacks them all); unknown for a token that writes folder by folder (an Editor's: `folders:read` is the folders its role is given), as which folder each rule is in isn't read; a token Grafana doesn't take (401) is a problem; where the path isn't served (404), unknown |
| `changes.cloud` | AWS (`--changes-aws-role`): STS's `AssumeRole`; Google Cloud (`--changes-google-service-account`): IAM Credentials' `generateAccessToken`; Azure (`--changes-azure-client-id`): a token for that identity from Microsoft Entra, as serve's credential chain gets one; OCI (`--changes-oci-config`): the config file and the API key it names, read | the identity can be taken, or the key read: a credential is made, nothing is written. What each may write is its own item, below |
| `changes.aws` | IAM's policy simulator, `SimulatePrincipalPolicy`, as the role, of the role's own policies, for the actions serve's writes send (`cloudwatch:PutMetricAlarm`, `DisableAlarmActions`, `EnableAlarmActions`, `DeleteAlarms`, `TagResource`; `aps:PutRuleGroupsNamespace`) on the ARNs of what `--changes-in` names, in the role's account: a name given whole as its ARN, a prefix as each alarm or namespace this run read under it and, for CloudWatch, one name under it no alarm read has, which a rule added makes (expectations.md, step 5), twenty at most; the region the writes are made in given as the one context entry, `aws:RequestedRegion`; one request a region for CloudWatch's, one for Amazon's namespaces | ok when every action is allowed on every name; a problem naming what is denied; unknown for a decision with a context key the simulation lacks, allowed or not, a prefix with nothing read under it, and names past twenty. An SCP's conditions on another key than the region aren't judged: the simulator doesn't say which an SCP lacks. It needs **`iam:SimulatePrincipalPolicy` on the role's own ARN**, a permission serve itself never needs: without it, IAM refuses, and the item is unknown, never a problem, and says so |
| `changes.azure` | at each resource group `--changes-in` names (a rule group named whole, at itself): the identity's permissions, `GET {scope}/providers/Microsoft.Authorization/permissions`, the deny assignments at or above it (`denyAssignments`, `$filter=atScope()`) and its locks (`locks`); the subscription's locks; and, for the alert rules named that this run read, the permissions at each resource they name (their targets, their action groups) | each action serve's writes need (`Microsoft.AlertsManagement/prometheusRuleGroups/write`; the write of each alert rule type that may be named there, `metricAlerts`, `scheduledQueryRules`, `activityLogAlerts`, and `metricAlerts/delete` where a metric alert may be) matched against each entry's `actions` less its `notActions`, `*` and letter case as Azure has them: ok, or a problem naming what isn't given; an alert rule named whole, not given on its group, is unknown (it may be given on the rule alone). A `ReadOnly` lock on the group, the subscription or a rule written is a problem; a `CanNotDelete` one where a metric alert may be deleted, a problem; a deny assignment that may refuse a write, unknown, naming it. Read on a metric alert's target or action group not given there is a problem (Azure Monitor asks for it); on another kind's, unknown |
| `changes.google` | Cloud Resource Manager's `testIamPermissions` on the project of each Google `--alarms` that `--changes-in` names a policy of, as the service account, or, without `--changes-google-service-account`, as the pod's own identity, which then writes; with a read-only token, for `monitoring.alertPolicies.create`, `.update` and `.delete` | ok when it holds all three, a problem naming those it doesn't; an error (its API not enabled, say) unknown. Whether its answer counts IAM deny policies, principal access boundaries or VPC Service Controls isn't documented: not judged, and the ok says so |
| `changes.oci` | as the user, through Identity: its groups (`ListUserGroupMemberships`, then each group), and for each alarm `--changes-in` names that this run read, the compartments from its own up to the tenancy, and from its metric's (`GetCompartment`), and the policies of each (`ListPolicies`, every page), each compartment read once | the ACTIVE statements read and judged here, of the user's ACTIVE memberships and groups: `manage alarms` (`ALARM_UPDATE`) where the alarm is, and `read metrics` (`METRIC_READ`) where its metric is, "for the metric compartment and metric namespace" as `UpdateAlarm` takes it (in the tenancy, for a metric read in the subtree): ok, or a problem naming what no policy gives; a statement with a condition, or one not understood, is never a grant: "unknown: a policy with a condition". An entry that names no alarm read is unknown. Reading them takes `inspect` of groups, users, compartments and policies in the tenancy; without it, unknown |
| `changes.ruler` | nothing | a Mimir's or a Cortex's `rules:write` can't be known without writing: unknown |
| `changes.loki` | nothing | whether Loki's ruler, read with `--loki-loaded-rules`, takes the writes `--changes-in loki:NAMESPACE` lets serve make can't be known without writing, and a Loki whose rules are local files takes none: unknown |

### Files, the ledger, the ports

| Id | Read | Says |
|---|---|---|
| `inputs.files` | `--incidents` (`incident.Load`), `--deploys` (`serve.LoadDeploys`), `--config` (`serve.ReadConfig`), each as serve reads it; `--on-call`, one fetch of each feed | a file that serve would refuse, with its line; an incidents file with no `incidents` key, which serve keeps from taking every incident away |
| `secrets.files` | each token, key and webhook file | readable; a token of 16 characters at least and each different from the others (as `tokenOK` refuses one shared); a session key of 32; a webhook file holding an https URL. Whether a webhook takes a post is unknown: knowing it would post |
| `ledger.sqlite` | the directory: there, writable by this user (`access(2)`, nothing written), its free space (`statfs`); the ledger, opened read-only, its `PRAGMA user_version` | a new ledger; the same schema as this release; or **"serve upgrades the ledger from 018 to 019 as it starts, and a release before 019 can't open it after: keep a copy first"**; a newer schema is a problem (serve refuses it). Where the ledger's volume can't be mounted (below, the chart), unknown |
| `ledger.postgres` | connects with `--ledger-url-file`'s string; `SELECT version FROM schema_version`; whether the user may create in the schema; whether a serve holds the ledger's lock now (`pg_locks`) | as for SQLite, and the privileges a migration needs; never takes the lock |
| `signin` | `--oidc-issuer`'s discovery (`OIDC.Discover`), `--proxy-jwt`'s keys, once | what serve would say at start: an issuer that differs, an endpoint that isn't https |
| `access` | nothing | who may change what, in the sentence serve logs at start: with no roles, everyone may only look on a listener beyond loopback (`LoopbackOnly`); `--user-header` without a proxy in front; each port and what it takes |

### The backtest lane

With `--backtest-listen` ([backtests.md](backtests.md)); without it, none
of these, and the report is what it was before them, to the byte ("As
built (step 3)").

| Id | Read | Says |
|---|---|---|
| `backtest.trust` | nothing more: `--backtest-trust` as serve read it in reading the flags (`serve.ReadTrust`, with the other token files serve takes), and the port's flags as `serve.New` holds them (`serve.BacktestRefused`) | what serve does at start: it takes the file, naming each entry, its CI (`github`, `gitlab`, `forgejo`, `bitbucket` or `generic`) and the claims it pins, never what a claim is pinned to; or it stops, a problem, saying why from serve's own refusal (`serve.TrustRefused`: the line, the entry, the field, what is wrong) in Lapilli's words, nothing the file holds. A plain token's file is read as serve reads it (there, 32 characters at least, no other token file's), and said to be long enough, never its text or its length. No credential is needed: with `--offline` the file is read all the same, the plain tokens' files not, and with one the item is unknown |
| `backtest.issuers` | for each issuer the file names (with its `jwks_url`, as serve asks it, once for all its entries), its discovery document, then its keys, as `signin.CI` asks them at a token's first check: its issuer to the byte, its keys at its own origin or the entry's `jwks_url`, no redirect, 1 MB and 100 keys, ten seconds; four at a time, 20 at most | said as it is when it is a public CI's (GitHub's, gitlab.com, codeberg.org), otherwise as `[--backtest-trust's issuer N]`, hidden wherever the report would say it, as a `jwks_url` is; ok when a key it holds verifies a token of an algorithm its discovery document names; a problem when either can't be read, or no key does: serve starts, and refuses those entries' tokens (401) until it does; unknown with `--offline`, past 20 issuers, or once `--preflight-timeout` is up |

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
| `estimate.backtests` | with `--backtest-listen`: the requests a backtest asks within `--backtest-timeout` at the lane's share of `--max-rate`, and what a pull request changing ten of the rules read asks over the recipe's seven days, counted as the port counts one before it is queued (`fits`), and the days of it that fit (below) |
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

**The backtest lane.** While the lane runs a backtest, its share of
`--max-rate` (`--backtest-share`) is kept for it, and the runs' requests
to `--prometheus` are spaced by the rest (`prom.Pace.Reserve`); to a Loki
and Grafana's data source proxy too, while it asks them
([backtests.md](backtests.md), "As built (step 3)"), so each API the runs
ask is timed so. So, with
`--backtest-listen`, the hourly runs are timed twice: at `--max-rate`, and
in the worst case, a backtest running the whole hour, at `--max-rate` less
the share. A run that takes longer than `--every` only then is unknown,
not a problem: the runs fall behind while backtests run back to back,
which is the pull requests' to say. And `estimate.backtests` says how much
a backtest can ask: at the lane's rate (the share of `--max-rate`'s pace,
as serve makes it, `prom.Pace.Share`), the most requests within
`--backtest-timeout`; and, counted by the port's own count
(`serve.BacktestRequests`, which `fits` calls: the rules before and after
side by side, over days ending at now less `--settle`), ten of the rules
read changed, over seven days, their expressions (the two sides asked
apart) or their `for` alone, 5 minutes longer (the two sides sharing the
reads their `for` doesn't move), and the most days of each that fit, 14 at
most; where ten don't fit seven days, how many do, and a day. Each rule is
counted with its own `for` and `keep_firing_for`, which start its reads
earlier: one that reaches back past midnight UTC asks a day more, so the
count is the preflight's hour's. The least a backtest of a rule that
differs asks (one rule added, taken away or its `for` changed, over a
day) not fitting is a problem: the port refuses every such backtest
before it is queued.

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
only when every read at once holds `--max-samples`. With
`--backtest-listen`, the lane's reads are beside the runs' (a quarter of
`--parallel` at once, one at least), of a pull request's rules, which
nothing read here sizes: they are said at their bound, each
`--max-samples`, and a memory limit the runs' peak fits and the two
don't is unknown.

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
unknown  grafana.write       --changes-grafana-token-file's token may write alert rules folder by folder:
                             it may read 12 folders
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
  and action, `AssumeRole` among them. Of what is asked of what may be
  written, the two POSTs answer and change nothing, and are held to that:
  IAM's to `Action=SimulatePrincipalPolicy` of the role's own ARN, with
  only its actions, resources, page size and one context entry,
  `aws:RequestedRegion` of a region serve writes in; Cloud Resource
  Manager's to the `:testIamPermissions` path of each project of
  `--alarms` that `--changes-in` names, with the alerting policies'
  permissions as its body. The backtest port's issuers are asked by GETs
  of exactly their discovery documents and keys, the keys a discovery
  document names allowed only at the issuer's own origin: a URL in the
  operator's trust file, and held to the list all the same. Any other
  action or path on those hosts is refused, and so is a form or a query
  that doesn't read whole (a `;` between its pairs). An answer that says "allowed" is judged, and
  nothing more is done with it.
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

With [backtests.md](backtests.md) (proposed beside this, #89; built in
serve, #135, and here as step 3), the backtest lane's share of
`--max-rate` is taken out of what the runs have, in the estimate, and its
trust file is read and its issuers' discovery fetched; the planner here
is what refuses there a backtest that couldn't finish in its time.

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
each cloud's chain, each on-call feed; and, with `--changes`, what each
identity may write: one simulation of IAM's a region named, and one for
Amazon's namespaces; one `testIamPermissions` a Google project named;
Azure's permissions, deny assignments and locks at ten scopes at most,
each subscription's locks once, the deny assignments at ten alert rules
named at most, and the permissions at ten resources the alert rules name
at most, five pages each; and OCI's memberships (a
thousand at most), twenty groups, and for ten compartments at most their
chains up to the tenancy (eight at most) and the policies of each, read
once each (five pages each, 5,000 statements all told); and, with
`--backtest-listen`, the discovery document and keys of each issuer the
trust file names, twenty at most. Each is counted in `asked.by`, as
`grafana` and `iam:aws`, `iam:azure`, `iam:google`, `iam:oci` and
`backtest`. Each is asked under `--max-rate`,
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

**Decided on 2026-11-18, by the maintainer:**

3. **Writes, where a cloud says without writing**: ask all four clouds,
   in step 2. Google Cloud's `testIamPermissions` and Azure's permissions
   of the caller on a scope say what may be written; AWS's policy
   simulator needs `iam:SimulatePrincipalPolicy`, a permission of its
   own, without which the item is unknown; OCI's policies are read and
   judged here, a statement with a condition never counted as a grant
   ("As built (step 2)").

**Open:**

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
  might not then open. A `ledger.db` that is a link is read where it
  leads, and what is beside it looked for there, as SQLite looks
  (`TestProbeSQLiteLink`): looked for beside the link, a `-wal` was
  missed, and the file read without it. A file shorter than a page with a
  `-wal` beside it isn't opened at all, and the item says it is cut short
  (`TestProbeSQLiteCutShort`): SQLite takes it for an empty database, and,
  opening it in a directory it may write, deletes the `-wal`.
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
  The ledger's schema is said as the server's answer is, so it is
  redacted alike: the search_path's first, `"$user"`, makes a schema
  named after the user the ledger's (`TestPostgresSchemaSaid`).
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
  it, once OSS is checked (it is: "As built (step 2)").
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

## As built (step 2)

What Grafana's token and each cloud's identity may write, asked of an API
that says so without anything being written (`internal/preflight`,
`writes*.go`):

- **Grafana OSS serves `/api/access-control/user/permissions`.** Measured
  on 2026-11-18 against Grafana OSS 12.4.15 (commit `22e86c4f`) and
  13.3.1 (commit `da3aa3ff`), the official darwin-arm64 tarballs from
  dl.grafana.com, their SHA-256 checked against grafana.com's download
  API and the `.sha256` file beside each, run on 127.0.0.1 on a port of
  their own with a data directory made for the run and removed after. On
  both, the path answers 200 for a Viewer's, an Editor's and an Admin's
  service account token, and for the admin user: an object of each
  action to its scopes (24, 58 and 135 actions on 12.4.15, 30, 76 and
  169 on 13.3.1). With no credential it answers 401 `auth.unauthorized`,
  and with a token Grafana doesn't know, 401 `api-key.invalid`, "Invalid
  API key". Grafana's documentation names the access control API
  Enterprise's and Cloud's; this path, the caller's own actions, is
  OSS's too.
- **What the roles hold, and what they may write, measured.** An Admin's
  token lists `alert.provisioning:read`, `:write`,
  `alert.rules.provisioning:read`, `:write`,
  `alert.provisioning.secrets:read` and
  `alert.provisioning.provenance:write`, and `folders:read` on
  `folders:*`. An Editor's lists `alert.rules:read`, `:write`, `:create`
  and `:delete` on `folders:*` and on each folder,
  `alert.provisioning.provenance:write`, and `folders:read` only on the
  folders its role is given, none of `alert.provisioning:*`. A Viewer's,
  `alert.rules:read` and `folders:read`. Through the provisioning API,
  with `X-Disable-Provenance` as serve sends it, a rule was made (POST,
  201), changed and paused (PUT, 200, read back paused) and deleted
  (DELETE, 204) by the Admin's token and by the Editor's, in a folder the
  Editor may read, on both versions; the Viewer's read (GET of the rule
  and of the export, 200) and was refused each write, 403, "Permissions
  needed: any of alert.provisioning:write, alert.rules.provisioning:write,
  all of alert.rules:create, …". An Editor whose actions Grafana had
  listed before the folder was made, and so without `folders:read` on it,
  was refused in it: 403 "user is not authorized to access rule group … in
  folder …" on 13.3.1, a 500 on 12.4.15's POST and PUT. So the folder is
  checked as the rule is written, and a token that writes folder by
  folder is unknown here. The actions are those Grafana's
  `pkg/services/ngalert/api/authorization.go` asks of each route, alike in
  12.4.15 and 13.3.1.
- **Grafana keeps each identity's list a while.** A role or a folder's
  permissions changed just before may be listed as they were; the path's
  `reloadcache` isn't asked, and the allowlist refuses any query on it.
- **What serve writes as each identity, asked once.** AWS: IAM's policy
  simulator, as the role (its credentials made once, for the write's
  service and for IAM's), at IAM's endpoint of the role's partition,
  signed for `us-east-1`, `us-gov-west-1` or `cn-north-1`, one simulation
  a region, the region given as `aws:RequestedRegion`, so that an SCP's
  region guardrail (a `StringNotEquals` on it) reads as it would for the
  write. A name given whole is asked as its ARN; a `--changes-in` prefix
  (`NAME*`) as each alarm, or namespace, this run read under it, in the
  role's account (one of another account serve doesn't write): a policy's
  `Deny` or `NotResource` for some names under a prefix never matches
  the prefix written as an ARN, so a prefix is never asked so. Twenty
  names a simulation at most; a prefix with nothing read under it, or
  names past twenty, are unknown. A decision with a context key the
  simulation lacks is unknown, allowed or not. The simulator reads the
  role's policies, its permissions boundary and the organization's SCPs,
  not resource control policies (`API_SimulatePrincipalPolicy.html`), and
  "for security reasons, SCP evaluation does not return missing context
  values": an SCP's conditions on any other key than the region aren't
  judged, as an ok says. An alarm's tags are read with the pod's own
  identity, so `cloudwatch:ListTagsForResource` isn't asked of the role.
- **Azure, at each scope, three lists.** The identity's permissions; the
  deny assignments at or above the scope (`$filter=atScope()`, "all deny
  assignments at or above the scope"), a deny that may refuse a write
  unknown, naming it, as whom it applies to isn't read (the identity's
  object id isn't known here), one that doesn't apply below its scope
  passed over; and its management locks, `ReadOnly` ("can't delete or
  update it") a problem, `CanNotDelete` a problem where a metric alert
  may be deleted. A lock applies on the scope, above it, or on a resource
  under it that serve may write (of a type written there, named by
  `--changes-in`); one on another resource is passed over. Whether a
  resource group's list holds the subscription's locks, which its
  resources inherit, isn't documented, so the subscription's list is
  read too, once, and its locks on the subscription itself judged. Each
  list's next page is followed on the same host and path alone. A name
  given whole is of the type this run read it as; one not read, or a
  prefix, may be of any type, `metricAlerts/delete` needed where a metric
  alert may be named. Reading the three lists takes
  `Microsoft.Authorization/permissions/read`, `denyAssignments/read` and
  `locks/read`; without one, unknown.
- **A deny assignment below the group.** `atScope()` gives those at or
  above the scope asked, and a deployment stack puts its deny on the
  resources it manages: one on an alert rule itself is never in its
  resource group's list. So each alert rule and rule group `--changes-in`
  names that this run read is asked too, its own `atScope()`, ten at
  most; past ten, or where an entry names nothing this run read (a
  prefix with nothing under it, a name not read), unknown. Each deny is
  said once, whichever list gives it.
- **Azure's linked reads.** "Read permission on the target resource of
  the alert rule" and "on any action group associated to the alert rule"
  (alerts-create-metric-alert-rule): for each alert rule and rule group
  `--changes-in` names that this run read, every resource its document
  names beside itself (a resource id under a resource group) is asked
  at itself, ten at most, for its provider's `.../read`. Not given there
  is a problem for a metric alert's; for another kind's, whose
  documentation says nothing of it, unknown. An id it names that isn't a
  resource under a resource group (the subscription itself, a
  management group) or is past 1 KB isn't asked, and is unknown, never
  passed over; past ten, unknown. Azure Monitor may also ask for
  `Microsoft.Insights/metricDefinitions/read` on a metric alert's target,
  which its documentation doesn't settle: not asked, so an ok doesn't
  cover it.
- **Google Cloud: one `testIamPermissions` a project named**, with a
  token of the `cloud-platform.read-only` scope, impersonated as serve's
  is, or the pod's own without a service account; it needs no permission
  of its own. Whether it counts IAM deny policies, principal access
  boundaries or VPC Service Controls isn't documented (testing-permissions
  says it gives "the set of permissions that the caller is allowed"), so
  an ok says those aren't judged.
- **Google's writes as the pod's own identity** were made with the
  reads' token, of `monitoring.read`, which a write is refused with, since
  0.24.0: without `--changes-google-service-account`, serve now makes the
  pod's write credential of its own, of the `monitoring` scope
  (`cloud.Writes`), and preflight takes and asks about that identity.
- **OCI: as the user, read through Identity in the config's region**,
  for each alarm `--changes-in` names that this run read: `manage alarms`
  where it is, and `read metrics` where its metric is
  (`metricCompartmentId`: "the compartment containing the metric being
  evaluated by the alarm"), or in the tenancy where it is read in the
  subtree (`metricCompartmentIdInSubtree` "requires the user to have
  tenancy-level permissions"). Only an `ACTIVE` membership, group and
  policy count. Groups in an identity domain other than Default aren't
  read, and a statement naming one is unknown; past twenty groups, or a
  thousand memberships, a statement of a group not read is unknown.
- **OCI's policies, read conservatively.** A statement is read as `Allow
  SUBJECTS to VERB RESOURCE-TYPE in LOCATION [where CONDITIONS]`: subjects
  `group NAME`, `group id OCID`, `group 'DOMAIN'/'NAME'`, `any-user`,
  `any-group` (and `dynamic-group` and `service`, which are never the
  user), the verbs `inspect`, `read`, `use` and `manage`, the resource
  types `alarms`, `metrics` and `all-resources`, and `in tenancy`, `in
  compartment id OCID` or `in compartment NAME` or `A:B`, from where the
  policy is attached: a path from one under it, as OCI's documentation
  has it, or the compartment itself by its name, which is unknown, as a
  compartment under it of the same name can't be ruled out without
  listing its children. Access given in a compartment is given under it. A statement with `where` is never a
  grant: what it would give is "unknown: a policy with a condition". So is
  a list of permissions in braces, a group of another domain or one past
  the twenty read, a `deny` that names what is needed, and a statement
  that doesn't read so (but for `define`, `endorse` and `admit`, which
  give the user nothing here) when it names alarms, metrics or
  all-resources. For a metric read in the subtree, only a grant in the
  tenancy covers every compartment, and the policies of its compartments
  aren't read: anything less is unknown. The chart's least-privilege
  policy for OCI has a condition, so it reads as unknown.
- **What is asked is allowed as it is learned, and counted by its own
  API.** The asks the flags say (Grafana's list, each project's, the
  role's) are allowed as `allowAll` starts; each of Azure's as it is
  asked, and its next page and OCI's compartments, groups and policies
  when learned: each before the wider reads that hold some of them too,
  so it is counted by its own API. For Azure, that allow is made from the
  same URL just before it is sent, and step 1's read of a subscription's
  `--alarms` already lets any GET under the subscription through: it
  counts, and doesn't guard. What guards Azure's asks is that each is a
  GET, to Resource Manager's host as `--alarms` or `--ruler` gives it, of
  a path that ends in one of the three `Microsoft.Authorization` lists
  (`permissions`, `denyAssignments`, `locks`), built here; the resources
  an alert rule names come from Azure's own answer, and are asked only as
  a resource's id under a resource group, 1 KB at most.
- **Bounded, by count.** Grafana's folders are counted once each, ten
  thousand at most (a list of 4 MB is some 190,000); OCI's memberships
  once each, a thousand at most, and only the twenty groups read are
  hidden.
- **Redacted, each by what it is.** The role's ARN and account, the
  service account, the client ID and `AZURE_TENANT_ID`, the OCI tenancy,
  user, groups and compartments everywhere; the role's name, the part of
  the service account before its `@`, the groups' and compartments'
  names, a project and a resource group, short names that may be words of
  the report's own, only in what a server said. What the URLs of `--alarms`
  and `--ruler` name inside them (an OCI compartment, an Azure
  subscription, an Amazon workspace; a Google project and a resource
  group in what a server said) is redacted with `--changes` or not: step
  1 printed them where an error repeated the URL with more beside it. An
  API's error is said by its code and message, before the 200 bytes cut.
  What a cloud says is redacted as the run redacts first, so that what it
  knows is said by what it is, then without any other ARN that names an
  account (the pod's own role, another account's), OCID (another
  compartment's, a dynamic group's, a policy's; since 0.63.2), GUID (an
  Azure object id), or number of ten digits or more, or written `1234-5678-9012`,
  whatever is beside it but another digit (`acct_123456789012`, a Google
  project's number): a cloud's error names those beside what was asked.
  So is an identity that couldn't be taken (STS's `AssumeRole`, Microsoft
  Entra, IAM Credentials, in `changes.cloud`). Lapilli's own words aren't
  so read.
- **`changes.cloud` is ok once the identity is taken**: what it may
  write is each cloud's item. `--offline` takes no identity, reads no
  config file of OCI's, and says each item unknown.
- **Creating a rule** (since expectations.md's step 5, adding a rule): a
  CloudWatch prefix `--changes-in` gives is also asked of one name under
  it that no alarm read has (`PREFIX` then `lapilli-added`), the alarm a
  rule added there makes, which no alarm read stands for; a name given
  whole was asked already, made or not. Grafana's `alert.rules:create`
  and Google's `monitoring.alertPolicies.create` were asked already, for
  undoing a deletion, and stand for adding too; an Amazon namespace's
  `PutRuleGroupsNamespace` is the same write. Azure's and OCI's alarms
  aren't added to, so nothing more is asked of them.

## As built (step 3)

The backtest lane, with `--backtest-listen` (`internal/preflight`,
`backtests.go`):

- **A trust file serve refuses is an item, not a flag refused.** Flags
  serve refuses, alone or together, still exit 2; the trust file is a
  file serve reads at start, as it reads `--config` and `--incidents`, so
  its refusal is `backtest.trust`'s problem, exit 1, and the rest of the
  report is said too. It is serve's refusal, not one like it: the flags
  are read by serve's reader, which reads the file with
  `serve.ReadTrust` and the other token files serve takes, and keeps the
  error for the report (`readServeAs`) where serve stops at it; a test
  runs both on each of a set of files and holds them to the same answer
  (`TestPreflightTrustAsServe`). What `serve.New` refuses of the port
  beside the file (rules to replay, a history) is said by
  `serve.BacktestRefused`, made of the check `New` makes (`backtestable`).
- **A refusal said from its structure, not from serve's text.** serve's
  message quotes what it couldn't take, a claim's value or an audience;
  scrubbing that text missed values a key held (no space after a colon),
  keys and values the other way about, aliases and merge keys from
  outside `trust:`, an audience with spaces after it, and changed
  Lapilli's own words (`days is 1 to 14` with a pin of `"1"`). So
  `ReadTrust` gives a `*TrustRefused`: serve's message as it was
  (`Error`, compared byte for byte with main's over 39 refusals while
  this was built), and
  beside it the line, the entry, one of an entry's own keys and what is
  wrong in Lapilli's words (`Said`), with no value of the file's: a claim
  is named only when it is one of Lapilli's (`repository_id`,
  `event_name`, the claims a token's check holds, those the CIs'
  documentation names), at its own line, YAML's own errors by their line
  and their kind. preflight prints `Said` alone
  (`TestReadTrustSaid`, `TestBacktestTrustHidden`).
- **An issuer's keys are counted as a token would be verified by them**:
  of the keys the set holds as a token's check holds them (`fetchSet`:
  public, for signatures, an RSA key of 2048 to 4096 bits, 100 at most),
  those of an algorithm the discovery document names, as go-jose verifies
  (an RSA key for `RS*` and `PS*`, an ECDSA key of P-256, P-384 or P-521
  for `ES256`, `ES384` or `ES512`, Ed25519 for `EdDSA`): none is a
  problem (`signin.Keys.Usable`).
- **Asked in the preflight's time.** serve's `CI.Discover` asks in ten
  seconds of its own, whatever its caller's context says, and keeps what
  it found; preflight asks the same `discover` once through
  `CI.DiscoverKeys`, under its context, and keeps nothing. An issuer not
  yet asked when `--preflight-timeout` is up, or cut short by it, is
  unknown, never a problem.
- **The keys a discovery document names are pinned here too.** signin.CI
  takes them only at the issuer's own origin; the allowlist allows them
  only so (`keysAllowed`), once read, so a discovery document can't have
  preflight fetch from anywhere else even if signin's check went.
- **What a typical pull request is.** Ten of the rules read that the
  port takes (replayed: from backtests' step 3, a Grafana rule's and a
  Loki ruler's too, whose data sources are the rules in effect's), each
  of queries of its own, the first ten in the order read, each with its own `for` and
  `keep_firing_for`: a rule in Prometheus's format asks one query a day
  its reads touch, and its `for` starts them earlier, so one whose `for`
  reaches back past midnight UTC asks a day more (`for: 2h` at 02:30 UTC:
  180 for ten, not 160, `TestEstimateBacktestsOwnFor`). Counted by
  `serve.BacktestRequests`, the count `fits` makes: an expression changed
  as twice one side, the two sharing no read; a `for` alone as the rules
  and a copy of them with their `for` 5 minutes longer, side by side, as
  `fits` plans `gone` and `came`, each rule's first read starting where
  its `for` says. The design's case: 160 requests over 7 days, 112 fit at
  0.125 a second in 15 minutes, so four days, and seven of the rules for
  the seven; the `for` alone (5m to 10m), 90, nine days
  (`TestEstimateBacktests`). An entry's own `days` isn't counted: 14 at
  most, the port's.
- **A problem only when nothing that differs fits.** At a low
  `--max-rate`, ten rules may not fit a day while one does: it says how
  many of the rules changed fit seven days and a day, and is a problem
  only when the least a backtest of a rule that differs asks, one rule
  added, taken away or its `for` changed over a day, doesn't fit. With the rules not all read
  (`--offline` with `--ruler` or `--grafana`), what a backtest of them asks
  is unknown.
- **Issuers four at a time, named by number.** Twelve that don't answer
  take their ten seconds four at a time, not one by one, out of
  `--preflight-timeout`. An issuer of no public CI's (GitHub Enterprise
  Server's host, an enterprise's slug on GitHub's, a GitLab of one's
  own) and every `jwks_url` are hidden, with their hosts, wherever the
  report would say them (`hideIssuers`), and named as `[--backtest-trust's
  issuer N]`; one on a public CI's host (an enterprise's under GitHub's)
  by its URL without the scheme, its slug hidden and not the public host,
  which other entries' issuers are on. Of such an issuer, a discovery
  document that names another issuer (the same in another case, another
  slug) or keys at another origin (another host of one's own) is said in
  Lapilli's words alone, as what it names isn't the trust file's and so
  isn't hidden (`signin.ErrOtherIssuer`, `ErrKeysElsewhere`); a public
  CI's keeps the detail. A discovery document that names an algorithm many times
  names it once (`signin.NewKeys`, which serve's checks use too), and four
  are listed at most.
- **A fetch that got no answer names no other host.** Go's client says a
  redirect it refused with where it led (another host, its query), and a
  certificate not for the host asked with the names it is for; serve's
  log said them too. `signin`'s fetch now says each by the URL asked
  alone (a redirect refused, a certificate not for the host, an authority
  not trusted): the URL asked is the trust file's, hidden as an issuer
  of no public CI's is (`TestGetSaysNoOtherHost`). A name that doesn't
  resolve, a refused connection or a time out name that URL's host, and
  are said as they are.
- **`CI.DiscoverKeys` is preflight's alone.** It skips what `keysOf`
  holds a token from anyone to (discovery once a minute after a failure,
  one ask at a time); a test reads the source and fails on a call of it
  outside `internal/preflight` (`TestDiscoverKeysPreflightAlone`).
- **The backfill and the nights aren't timed again.** A backtest is
  taken once a run is recorded, so it can run while the backfill catches
  up, and slows it as it slows the hourly runs; the backfill and the
  nights are floors already, and say so.
- **Memory**: the lane's reads at their bound beside the runs' peak
  (`backtest_bytes`); the request and the limit stay the runs'.
- **`access`** names `--backtest-listen` among the ports.
- **Byte for byte without it.** `TestPreflightOffGolden` holds the text
  and the JSON, offline (with and without `--changes`) and against
  stand-ins, to what main gave before step 3 (`cd32f629`), but for the
  directory the test runs in, how long the history took to answer and
  the free space beside the ledger.

## Steps

1. The command: serve's flags, the rules, the history's probe and the
   ALERTS query, the files, the ledger read-only, the sign-in, the
   estimates of requests, time, nights and memory, `--offline`, the text
   and JSON, redacted, the exit status; the allowlist, the read-only
   ledger, the source check and the test by count; the planner held to
   `Run` by a test.
2. Grafana's permissions, once OSS is checked; the clouds' that can be
   asked: all four (built, "As built (step 2)").
3. The backtest lane ([backtests.md](backtests.md)): its trust file, its
   issuers' discovery and keys, its share in the estimates (built, "As
   built (step 3)").
