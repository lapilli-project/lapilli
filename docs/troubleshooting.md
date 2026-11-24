# When something goes wrong

Each message below is quoted as Lapilli prints it, with the parts that
change in capitals: `FILE`, `URL`, `RULE`, `N`. Search this page for the
part of yours that doesn't change.

A message about the flags ends with `Run "lapilli verdict -h" for the
options.`, and the command exits 2; one about what a run met exits 1.
`lapilli serve` and `lapilli preflight` read the flags `lapilli verdict`
reads, and say the same of them after `lapilli serve:` or `lapilli
preflight:`. A rule that couldn't be replayed doesn't stop a run: it is
named with its reason under `Could not replay N rules:`, after the rules'
tables and before the incidents', and in `unreplayed` in `--json`; one
whose replay failed for the run, as the history couldn't be read, is named
at the top too, under
`! N of M rules couldn't be replayed, and are left out of what follows:`.

## The command line

### `unknown command`

```
lapilli: unknown command "NAME"
```

The first word is the command: `verdict`, `serve`, `preflight`, `ledger` or
`version`. `lapilli -h` lists them.

### `unknown flag`, or `needs a value`

```
lapilli verdict: unknown flag --NAME
lapilli verdict: --NAME needs a value
```

The flag isn't one `lapilli verdict -h` lists. A flag that isn't on or off
by itself takes the next word as its value, so a misspelled flag last on
the line is said to need a value, and one before another word is unknown,
with `=VALUE` if it was written so.

### `say where the firings come from`

```
lapilli verdict: say where the firings come from: one of --rules, --loaded-rules, --grafana, --ruler, --alerts, --firings, --alarms or --loki-rules
```

Give the rules to replay, or where firings were recorded;
[How it judges](how-it-judges.md) says what each reads. `lapilli serve`
says it without `--firings`, and adds `--firings-token-file`, for firings
copied in ([Firings recorded elsewhere](serve.md#firings-recorded-elsewhere)).
`--rule` is a regexp of rule names, not where the rules are: given a path, the
message adds that `--rules` is for a file or a directory. Two of the flags
together say `--rules and --firings both say where the firings come from: give
one of them`.

### `read history: add --prometheus URL`

```
lapilli verdict: replays, --alerts, --impact and --check-replay read history: add --prometheus URL (a managed Prometheus's, with --auth aws, azure or google) or --gcp-project ID
lapilli serve: say where the history is: --prometheus URL (a managed Prometheus's, with --auth aws, azure or google) or --gcp-project ID
```

A replay asks a PromQL API for the history the rules are evaluated
against. Give its base URL, the one lapilli adds `/api/v1/query_range` to
(`http://prometheus:9090`), or a Google Cloud project for its managed
Prometheus. A cloud's managed Prometheus takes `--auth` too
([Use `lapilli verdict`](verdict.md)).

### `say what went wrong`

```
lapilli verdict: say what went wrong: --incidents FILE, --impact PROMQL, or both
```

A verdict judges firings against incidents: a file of them
([How it judges](how-it-judges.md)), a PromQL expression that returns
something while users are hurt ([Incidents nobody wrote down](impact.md)),
or both. `--check-replay` takes neither. `lapilli serve` needs neither, as
incidents can be written down in its UI.

### `want a duration`, `want an RFC 3339 time`

```
lapilli verdict: --since 3x: want a duration like 15m, 7d
lapilli verdict: --from 2026-11-01: want an RFC 3339 time like 2026-11-01T00:00:00Z
lapilli verdict: --from and --to go together
```

A duration is Go's (`90s`, `15m`, `1h30m`) or a number of days, as
Prometheus writes them (`7d`, `1d12h`). A time is RFC 3339, with `Z` or an
offset (`2026-11-01T00:00:00+09:00`). `--from` and `--to` come together,
`--to` after `--from`. Other pairs of flags that can't be given together
say so in the same words, `don't go together` or `goes with`.

### `--auth`

```
lapilli verdict: --auth gcp: want one of aws, azure, google
lapilli verdict: --auth oci: Oracle Cloud has no managed Prometheus; give --prometheus the one you run, without --auth
lapilli verdict: --auth goes with --prometheus URL
lapilli verdict: --auth aws: the URL doesn't say which region; give --aws-region
```

`--auth` signs each request to a cloud's managed Prometheus with the
credentials the machine or pod already has; the table in
[Use `lapilli verdict`](verdict.md) gives each cloud's URL and the
permission it needs. AWS's region is read from the URL; give
`--aws-region` when the URL doesn't name it. That one is found when the
run starts, not among the flags: it exits 1, without the `Run "lapilli
verdict -h"` line.

## Reaching the history

### `the history couldn't be read`

```
  RULE: query failed: the history couldn't be read: Post "URL/api/v1/query_range": dial tcp HOST:PORT: connect: connection refused
  RULE: query failed: the history couldn't be read: Post "URL/api/v1/query_range": dial tcp: lookup HOST: no such host
  RULE: query failed: the history couldn't be read: Post "URL/api/v1/query_range": http: server gave HTTP response to HTTPS client
  RULE: query failed: the history couldn't be read: Post "URL/api/v1/query_range": unsupported protocol scheme "NAME"
```

The run didn't reach `--prometheus`: nothing listens at that port, the
name doesn't resolve where lapilli runs, the URL says `https` to a server
that speaks `http`, or it has no `http://` at all. In a cluster, a
NetworkPolicy may stand between. The rule is left out of the rest of the
report, and another run, with its access mended, may replay it. The run
exits 0, as what was replayed holds; with `--fail-unreplayed` it exits 1
instead:

```
lapilli verdict: N of M rules couldn't be replayed, as the history couldn't be read or their labels on their series were too many (--fail-unreplayed)
```

[`lapilli preflight`](#historyreach) tells these causes apart.

### `HTTP 401`, `HTTP 403`

```
  RULE: query failed: the history couldn't be read: HTTP 401: not a Prometheus API response: TEXT
  RULE: query failed: the history couldn't be read: HTTP 403: TEXT
```

The history didn't let lapilli in. `TEXT` is the server's own words, with
any token lapilli sent taken out: the API's JSON error cut to 300 bytes,
or, when the answer isn't the API's JSON, its body cut to 200, after `not a
Prometheus API response:`. Give a token
with `--bearer-token-file`, or `--auth` for a cloud's managed Prometheus,
with the read permission [Use `lapilli verdict`](verdict.md) names.

### `not a Prometheus API response`

```
  RULE: query failed: the history couldn't be read: HTTP 200: not a Prometheus API response: an HTML page, "TITLE"
  RULE: query failed: the history couldn't be read: HTTP 404: not a Prometheus API response: TEXT
  RULE: query failed: the history couldn't be read: HTTP 302: not a Prometheus API response:
```

Something answered at the URL, but not the API: a UI, a sign-in page (its
title is `TITLE`), a proxy or a gateway. Lapilli follows no redirect, so a
sign-in in front, or `http` sent on to `https`, answers 30x. Give the URL
the API answers at, with the path it is served under, if any.

### `query failed: HTTP 400`

```
  RULE: query failed: HTTP 400: TEXT
```

The history refused the query as written, in its own words (HTTP 400 or
422: a parse error such as `1:6: parse error: unexpected right parenthesis
')'`, or an execution error). Every run is refused the same until the rule
changes, so `--fail-unreplayed` doesn't count it. Try the expression in the
server's own UI.

### `not finished before the timeout`

```
  RULE: not finished before the timeout
lapilli verdict: N of M rules didn't finish within --timeout DUR; raise it, or shorten the period
```

The run took longer than `--timeout` (10m), or a wait the API asked for
would have ended past it. It exits 1. Raise `--timeout`, shorten
`--since`, or judge fewer rules with `--rule`; see
[A run is slow](#a-run-is-slow-or-the-history-is-busy).

### `answered N requests with HTTP 429`

```
lapilli verdict: the history answered N requests with HTTP 429, too many requests, and was asked fewer at once after it; --max-rate keeps a run under a quota
```

The API turned requests away for a quota. Lapilli waited, asked again and
asked fewer at once, as [Use `lapilli verdict`](verdict.md) says. To stay
under a quota known beforehand, `--max-rate N` asks N requests a second at
most (`0.5` for 30 a minute). `lapilli serve` can also stop a run at the
first 429 (`--yield-on-429`, [design/quota.md](design/quota.md)).

### `more than N samples in all (--max-samples)`

```
  RULE: query failed: the answers to the range hold more than N samples in all (--max-samples): read it at a longer --step, over a shorter range (--since), or with a larger --max-samples, each 1000000 more taking some 64 MB more at the peak
  RULE: too many labels on its series to replay: its alerts would carry its N labels with each of its N series' own, past the N MB a range's samples are held to (--max-samples N)
lapilli verdict: --max-samples 999: want a number of samples from 1000 to 1000000000, like 40000000
```

A replay reads a rule's query a day at a time, on days from midnight UTC,
and one read may hold `--max-samples` samples (10,000,000 by default, some
6,900 series firing all day at a minute). Past it the rule isn't replayed:
do as the first line says; a larger `--max-samples` takes more memory,
which `lapilli preflight` estimates. Two like it say the series or their
labels were too many (`more than N series in all`, `more than N MiB of
labels in all`): read those over a shorter `--since`. The second line:
each firing keeps the rule's labels beside its series' own, and those too
are held to `--max-samples`. `--fail-unreplayed` counts that one, as a
rule's writer could add labels to keep a rule out of a backtest unseen.

## Credentials and tokens

### `credentials: open FILE`

```
  RULE: query failed: the history couldn't be read: credentials: open FILE: no such file or directory
```

The file `--bearer-token-file` names isn't there, or can't be read by the
user lapilli runs as.

### `Google credentials`

```
lapilli verdict: Google credentials: google: could not find default credentials. See https://cloud.google.com/docs/authentication/external/set-up-adc for more information
```

`--auth google` (or `--gcp-project`) found no Application Default
Credentials: none in the environment, from workload identity, the metadata
server or `gcloud`. Give the machine or pod an identity with
`roles/monitoring.viewer`.

### `Azure credentials`

```
  RULE: query failed: the history couldn't be read: credentials: Azure credentials: DefaultAzureCredential: failed to acquire a token.\u{a}Attempted credentials: ...
```

`--auth azure` found no credential: the environment, workload identity, a
managed identity and the Azure CLI were each tried, and the message says
why each found none (`\u{a}` is a line break, printed so that it can't
start a line of its own). Give the pod an identity with Monitoring Data
Reader.

### `--grafana: HTTP 401`

```
lapilli verdict: --grafana-token-file: open FILE: no such file or directory
lapilli verdict: --grafana: HTTP 401: TEXT
lapilli verdict: --grafana: HTTP 403: TEXT
lapilli verdict: Grafana's data sources UIDS weren't read: their $__rate_interval is put in with a scrape interval of 15s, Grafana's own, and their queries are taken as PromQL
lapilli: --grafana is plain http: the token goes unencrypted
```

`--grafana` reads the rules through Grafana's provisioning export API with
the service account token in `--grafana-token-file`, and the data sources
the rules name. 401 is a token Grafana doesn't take; 403, one that may not
read the rules. A Viewer's token reads both. When the data sources can't be
read, the replay goes on with Grafana's defaults, as the fourth line says;
`--scrape-interval` gives the interval instead. The last line is a
warning: the token is sent over plain http, and the run goes on.

### `--loaded-rules`, `--ruler`

```
lapilli verdict: --loaded-rules: HTTP 401: TEXT
lapilli verdict: --loaded-rules: not a rules API answer: invalid character '<' looking for beginning of value
lapilli verdict: --ruler: the ruler: HTTP 401: TEXT
```

`--loaded-rules` asks `--prometheus` for `GET /api/v1/rules`, with the
same credentials; `invalid character '<'` is an HTML page where the API's
JSON was wanted, as [above](#not-a-prometheus-api-response). A Mimir's or a
Cortex's ruler takes the token in `--ruler-token-file`, and, with tenants
and no gateway, `--ruler-tenant`; Amazon's and Azure's are read with
`--auth` ([How it judges](how-it-judges.md)).

## Files

### `no such file or directory`

```
lapilli verdict: stat FILE: no such file or directory
lapilli verdict: open FILE: no such file or directory
```

`stat` is of `--rules`, `open` of `--incidents`: the path isn't there
from where lapilli runs.

### `no incidents key`

```
lapilli verdict: FILE: no incidents key: want incidents:, with a list under it, an empty one if there are none
```

An empty file, or one without `incidents:` at its top, is refused, so that
a job that failed and left an empty file isn't read as a week with no
incidents. Write `incidents: []` for none
([Incidents kept elsewhere](incidents.md)).

### `has no id`, `has no start`, `want a value or a list of values`

```
lapilli verdict: FILE: incident N has no id
lapilli verdict: FILE: incident ID has no start
lapilli verdict: FILE: incident ID is there twice; give each its own id
lapilli verdict: FILE: incident ID ends before it starts
lapilli verdict: FILE: label NAME: want a value or a list of values
```

Each incident has an `id` of its own and a `start`; `end` is optional (an
hour when missing), and after `start`; `labels` give each label a value or
a list of them: `{service: checkout}` or `{service: [checkout, cart]}`.
`N` counts the incidents from 1.

### `parsing time`

```
lapilli verdict: FILE: parsing time "2026-11-10T12:00:00" as "2006-01-02T15:04:05Z07:00": cannot parse "" as "Z07:00"
lapilli verdict: FILE: parsing time "2026-11-10 12:00" as "2006-01-02T15:04:05Z07:00": cannot parse " 12:00" as "T"
```

An incident's times are RFC 3339: a `T` between the date and the time, and
a zone, `Z` for UTC or an offset (`2026-11-10T12:00:00+09:00`). A time
without one is refused, not guessed.

### `yaml: line N`

```
lapilli verdict: FILE: yaml: line N: TEXT
```

The file, incidents or rules, isn't YAML as written. `TEXT` is the YAML
parser's own words, such as `mapping values are not allowed in this
context`, and line `N` is where it stopped reading.

### `no alert rules`

```
lapilli verdict: FILE: no alert rules: want a Prometheus rule file, a PrometheusRule, a Grafana alerting provisioning file, a cloud's rule groups or alerting policies, or a ConfigMap holding one
lapilli verdict: DIR: no file in it holds alert rules
lapilli verdict: no alert rules to replay
```

The first: the file is empty, or isn't one of the forms
[How it judges](how-it-judges.md) lists; with `--grafana`, what answered
at the URL wasn't Grafana's rules. The second: below a directory's top,
files that don't parse are passed over, and none was left. The third: the
rules were read, but none is an alert rule (recording rules alone), or
`--rule` matched none of their names.

### `unreadable duration`

```
lapilli verdict: FILE: rule "NAME": unreadable duration "5x"
lapilli verdict: FILE: group "NAME": unreadable duration "abc"
```

A rule's `for`, or its group's `interval`, isn't a duration as Prometheus
writes one (`5m`, `1h30m`).

### `after the period starts`

```
lapilli verdict: the first rules given are from TIME, after the period starts: give the rules from before then as --rules FILE, without @TIME
lapilli verdict: --rules FILE@TIME: want FILE, or FILE@TIME with an RFC 3339 time like 2026-10-12T09:00:00Z
```

Given as `--rules FILE@TIME`, each version of the rules is in effect from
its time, and the rules before the first time are the `--rules` without
one ([How it judges](how-it-judges.md), "Rules that changed").

## The ledger

### `the ledger was written by a newer lapilli`

```
lapilli serve: --ledger: the ledger was written by a newer lapilli (schema N; this one knows M)
lapilli serve: --ledger-url-file: the ledger was written by a newer lapilli (schema N; this one knows M)
```

A newer release upgraded the ledger's schema as it started, and this one
can't open it: serve stops as it starts, and in Kubernetes its pod restarts
over and over. Run the newer release again, or go back to a copy taken
before the upgrade:
[Going back after a schema upgrade](../charts/lapilli/README.md#going-back-after-a-schema-upgrade).
[`lapilli preflight`](#ledgersqlite) says it before a deploy.

### `asks for no sslmode that keeps its password`

```
lapilli serve: --ledger-url-file: the ledger's connection string asks for no sslmode that keeps its password: to HOST, TLS that checks no certificate, and none when the server declines it (sslmode=prefer, pgx's when no sslmode is given): anything in the path can decline it and be sent the password. Ask for sslmode=verify-full (sslrootcert=FILE for the server's CA), or say sslmode=require or sslmode=disable to go on unchecked
```

To a database on another machine, the connection string says
`sslmode=verify-full`, or `require` or `disable` to go on unchecked
([The ledger in Postgres](serve.md#the-ledger-in-postgres)).

### `can't connect to the ledger's database`

```
lapilli serve: --ledger-url-file: can't connect to the ledger's database: TEXT
lapilli serve: --ledger-url-file: not a Postgres connection string (postgres://USER:PASSWORD@HOST:PORT/DATABASE?sslmode=verify-full, or keyword=value pairs)
```

The first says what the connection met, such as `dial tcp ADDRESS:
connect: connection refused`. The second: the file doesn't hold a
connection string pgx reads.

## Signing in to serve

### `lets in only those a role names`

```
lapilli serve: a sign-in of lapilli's own lets in only those a role names: give --viewers (people, group:NAME or *@DOMAIN), --editors or --admins
lapilli serve: --oidc-issuer needs --external-url: the provider sends people back to its /auth/callback
lapilli serve: --editors and --admins need a sign-in, --user-header, --oidc-issuer or --proxy-jwt: who is who comes from it
```

An identity provider signs in whoever it knows, so with `--oidc-issuer` or
`--proxy-jwt` only those a role names come in. `--external-url` is where
people reach the UI; register its `/auth/callback` as the client's redirect
URI. Roles need a sign-in to say who is who
([Keep judging](serve.md), [design/signin.md](design/signin.md)).

### `signing in isn't set up right here`

```
signing in isn't set up right here: ask whoever runs lapilli to look at its log
the identity provider can't be reached: try again in a minute
```

The first: the provider's configuration, read at start, and at a sign-in
until it reads, won't do. The log says why, after `signing in: the
identity provider's configuration`, such as `--oidc-issuer is "URL", and
the provider says its issuer is "URL": give exactly that`: the issuer is
compared to the byte, a slash at its end too. The second: the provider
didn't answer, or answered 5xx.

### `doesn't name you as lapilli needs`

```
the identity provider's answer doesn't name you as lapilli needs: ask whoever runs lapilli to look at its log
```

The ID token didn't name the person as `--user-claim` (`email`) says. The
log has `refused a sign-in: the ID token names no one lapilli takes`, and
why: `no name: the token has no email claim`, or `no name: the token says
the email "NAME" isn't verified (email_verified)`. Name another claim the
provider vouches for with `--user-claim`, such as `preferred_username` or
`sub`.

### `this sign-in didn't start here`

```
this sign-in didn't start here, was used already, or took too long: sign in again
```

The browser came back to `/auth/callback` without the cookie its sign-in
began with, or with one this process can't read. Sign in again from the
page. Behind several replicas, each signs its cookies with a key of its own
unless they share `--session-key-file`: give them one
([Several replicas](serve.md#several-replicas)).

### `whom lapilli doesn't let in`

```
you are signed in as NAME, whom lapilli doesn't let in: ask its admins to name you
```

HTTP 403: the person is signed in, but no role names them or a group of
theirs: `--viewers`, `--editors`, `--admins`, `--approvers`, or a role an
admin gave on the Roles page. Groups come from `--groups-claim`
(`groups`), or with `--user-header`, from `--groups-header`.

### `only an editor can change this`

```
only an editor can change this
only an admin can change this
no one changes anything here: lapilli serve listens beyond this machine with no roles. Give roles behind a sign-in proxy (--user-header with --editors or --admins), or --everyone-admin
no one changes anything here: lapilli serve signs people in, with no roles. Name who may make changes in --editors or --admins
```

The first two: the reader's role doesn't allow the change; the flags give
roles, and admins give editors' on the Roles page. The last two: with no
roles, everyone is an admin only when serve listens on loopback alone and
signs no one in; beyond it, as the chart listens, everyone may only look.
The log says at start which applies ([Keep judging](serve.md)).

### `doesn't answer to this host name`

```
lapilli serve doesn't answer to this host name: give it in --allowed-hosts or --external-url
```

HTTP 421, against DNS rebinding. serve answers an address, `localhost`
and names under `.localhost`; a name without dots, or under `.svc`,
`.cluster.local` or `.internal`, unless the request came over loopback (a
port-forward, a sidecar); the name in `--external-url`; and those
`--allowed-hosts` adds, such as the name a proxy passes on. `/healthz` and
`/readyz` answer whatever the name.

### Signed out after a restart

Sessions are signed with `--session-key-file`'s key, or else with one made
for the process: without the file, a restart, or another replica, signs
everyone out. Give a key of 32 characters or more, from a file every
replica mounts.

## `lapilli preflight`

Each item is `ok`, `problem` or `unknown`, with `why:` and, for a problem,
`fix:`; it exits 1 with a problem. URLs and names are shown as the flag
that gave them (`[--prometheus]`).
[Before it is deployed](serve.md#before-it-is-deployed) says what it checks.

### `history.reach`

```
problem  history.reach       [--prometheus] didn't answer vector(1), with no credentials
                             why: the connection was refused: ...
                             fix: check --prometheus's port, and a NetworkPolicy between the pod and it
```

The first line ends with how the request was signed: `with no
credentials`, `with --bearer-token-file`, or `signed for aws` (`azure`,
`google`). `why:` begins with the cause:

| `why:` | `fix:` |
|---|---|
| `the name doesn't resolve: ...` | `check --prometheus's host, and the pod's DNS` |
| `TLS: ...` | `check the server's certificate, and the CA the pod trusts` |
| `the connection was refused: ...` | `check --prometheus's port, and a NetworkPolicy between the pod and it` |
| `it didn't answer within 10s` | `check --prometheus, and the network between the pod and it` |
| `not let in (HTTP 401): ...`, `(HTTP 403)` | `check the token or the identity serve signs with`; with `--auth`, `check the identity ... finds for the pod, and its role` |
| `HTTP 429: the quota is already in use` | `give serve --max-rate, and --yield-on-429, so its runs don't take the alerts' quota (docs/design/quota.md)` |
| `no credentials: ...` | `check the token's file`; with `--auth`, `check what ... finds for the pod` |

### `ledger.sqlite`

```
problem  ledger.sqlite       schema NNN, newer than this release's MMM
                             fix: serve refuses a ledger a newer lapilli wrote: run that release, or a newer one
ok       ledger.sqlite       schema NNN: serve upgrades the ledger from NNN to MMM as it starts, and a release before MMM can't open it after: keep a copy first
problem  ledger.sqlite       --ledger DIR: PATH isn't writable by this user
                             fix: run as the user serve runs as, with its volume; serve can't keep a ledger there
```

The second is ok, and says to keep a copy first
([Going back after a schema upgrade](../charts/lapilli/README.md#going-back-after-a-schema-upgrade)).

### `rules.read`

```
problem  rules.read          no alert rules, from --rules
                             fix: serve replays nothing until there are some: check the path, --rules-select and --rule
problem  rules.read          the rules couldn't all be read, from --rules
```

The second gives each source that couldn't be read, and why, as
[the files](#files) above.

### `backtest.trust`, `backtest.issuers`

```
problem  backtest.trust      serve doesn't start: it refuses --backtest-trust
                             why: line 7, entry 2: claims: pin repository_id, the repository's id (...)
unknown  backtest.issuers    not asked: serve refuses --backtest-trust (backtest.trust)
```

```
problem  backtest.issuers    no issuer's keys read
                             why: [--backtest-trust's issuer 1] (ENTRY): the identity provider can't be reached: ...
                             fix: serve starts, and refuses these entries' tokens (401) until the issuer answers as a CI's should: check issuer and jwks_url, and that the pod reaches it
```

The first is why serve stops at start, by the line, the entry and the
key, never a value of the file's (serve's log quotes it): [Pull request
backtests](serve.md#pull-request-backtests) says what an entry needs.
The issuers then aren't asked. The second is serve's first check of a
token, for the entries of that issuer, said by its number in the file
unless it is a public CI's (GitHub's, gitlab.com, codeberg.org):
its discovery document's `issuer` must be the entry's to the byte, and its
keys at the issuer's own origin, or the entry's `jwks_url`. `no key its
JWKS holds verifies a token of ALG` is a set of keys of another type, or
curve, than the algorithms its discovery document names; `no public
keys` a set of none serve takes, an RSA key under 2048 or over 4096 bits
among them.

### `hourly run`, `memory`

```
problem  hourly run          ...
                             why: a run's requests at --max-rate take longer than --every DUR: the runs fall behind, and never catch up
                             fix: raise --max-rate, or --every, or give fewer rules to replay (--rule)
problem  memory              ...
                             fix: give the pod the request and the limit above, or lower --parallel or --max-samples
```

The estimates count the requests an hourly run makes; under `--max-rate`,
a run that takes longer than `--every` never catches up. The memory
estimate is set beside the process's memory limit, where it has one.

## Results that look wrong

### A rule has no stones

In `lapilli verdict`'s report, a rule that didn't fire is named in `N
rules never fired: …`, with a line after it for those whose condition read
no series at all in the period, and one that couldn't be replayed is under
`Could not replay N rules:`, or at the top when its replay failed, with its
reason: the history (above), `paused`, `its
condition reads a TYPE data source: only PromQL and LogQL are replayed`,
or a cloud's alarm in its own terms (`a CloudWatch metric alarm isn't
PromQL`). [What it doesn't do yet](limits.md) lists what can't be
replayed. On `lapilli serve`'s rules list, the rules that didn't fire over
the weeks shown are below the table, folded away (`N rules didn't fire in
the last N weeks`); the Silent filter lists them in the table, and Not
replayed those it couldn't replay. A rule that fired for real but not in the
replay is for `--check-replay`
([below](#the-replay-differs-from-what-really-fired)).

### "No record", "? not judged"

```
No incident was written down, or found by --impact, in the period or the two weeks before it: a firing that caught nothing can't be told from one nobody wrote down, so no rule is called noisy and no longer "for" is suggested. Give incidents with --incidents, or read users' impact over the period with --impact; in lapilli serve, write them down, or mark a firing This was real.
N of the M firings came with no incident written down in the two weeks before them, and caught none: they are counted, not judged, and are in no rule's precision.
```

A firing that caught nothing is judged only when the record was kept at
its time: an incident on in the two weeks before it, or `--impact` read
over it. Any other is counted, not judged, and a rule none of whose firings
is judged has a precision of `–`. `lapilli serve` judges by the week: its
rules list marks such a rule No record, its tables count `? Not judged`,
and a firing's stone is `? not judged`. Write incidents down, copy them in
([Incidents kept elsewhere](incidents.md)), or give `--impact`.
[How it judges](how-it-judges.md) ("No record, no verdict") and
[The numbers](numbers.md#no-record) say how it is counted.

### Every firing is black

A firing catches an incident when it starts from `--lead` (15m) before the
incident to its end, and agrees with it on each `--match` label both carry
([The numbers](numbers.md#a-white-or-a-black-stone)). When none does:

- **No incident in the period.** The report's second line says how many
  incidents were written down: those that started in the period.
  `0 incidents written down` with incidents in the file means their times
  are outside `--since`, or `--from` and `--to`.
- **The times.** An incident's `start` and `end` carry their zone: `Z` is
  UTC, so a local time written with `Z` moves the incident by the offset.
  `--tz` doesn't move it: it is the zone times are shown and hours counted
  in.
- **The labels.** With `--match service`, an incident with
  `service: checkout` agrees only with firings whose `service` is
  `checkout`, not `checkout-api`: list several values
  (`service: [checkout, checkout-api]`). A firing or an incident without
  the label isn't held to it.
- **Too soon, or too late.** A firing that started more than `--lead`
  before the incident caught nothing: it was on anyway. One whose condition
  held in the incident but fired after it ended, its `for` run out, is
  named on the incident's row, in the report and on `lapilli serve`'s
  Incidents page alike (`✗ nothing in time; after its end: RULE +3m`), and
  stays black.

### The replay differs from what really fired

`--check-replay` sets the replay beside the series that say what really
fired, rule by rule ([Is the replay right?](check-replay.md)). What it
can't replay, and why a firing can start sooner or later than it did, is
in [What it doesn't do yet](limits.md): a group without an `interval` is
evaluated every `--step`, its place within its interval is known only to
`--loaded-rules`, query errors aren't replayed, history read now may hold
points that came late (`--late`), and today's rules replayed over a month
that ran on others differ (`--rules FILE@TIME`, `--rules-history`). A rule
that still replays differently is the most useful issue there is: there is
a form for it.

### The CLI and serve give different numbers

They count the same way, over different firings
([The numbers](numbers.md#which-firings-and-incidents-a-report-holds)):

- `lapilli verdict` holds every firing on at some time in the period, one
  that started before it too; `lapilli serve` those that started in it,
  apart from those a quiet window excused.
- serve replayed each hour with the rules as they stood then, soon after
  it, and the hour before again for points that came late; verdict replays
  the rules given over the history as it is now.
- verdict judges a firing with no record by its own time; serve by its
  week.
- serve gives a firing its stones against every incident it has, so a
  firing shortly before a week starts can catch an incident in it, and counts
  a re-fire against every firing it holds.
- serve's Settings page can change `--lead`, `--match`, `--refire` and the
  noisy thresholds.

### A run is slow, or the history is busy

A run that takes longer than 15 seconds says how it is going on stderr
every 30 seconds. A replay asks for each query a day at a time, `--parallel`
(8) rules at once against each API ([Use `lapilli verdict`](verdict.md)).
To ask less: a shorter `--since`; fewer rules (`--rule`); `--extras 0`,
which skips the steps after the replays; a lower `--parallel`; or
`--max-rate N`. Where the history doesn't say its scrape interval (Mimir,
Thanos, a managed Prometheus), most of a Grafana rule's queries are asked
for at Grafana's 1s, fifteen times and more the work: give
`--scrape-interval` there ([What it doesn't do yet](limits.md)). For
`lapilli serve`, `--yield-on-429`, `--guard` and `--catch-up-hours` keep
it off the quota the alerts need ([design/quota.md](design/quota.md)), and
[`lapilli preflight`](serve.md#before-it-is-deployed) estimates the
requests and the time beforehand.

### serve's overview says "Nothing judged yet"

```
The first run replays the rules over the weeks before it started; this page fills in when it has. It can take a few minutes for a busy set of rules.
```

On its first start, serve replays `--backfill` (28d) before the overview
has anything to show. While it catches up, the overview says `Catching up:
firings are in up to TIME.`; when a run waits (`--guard`,
`--catch-up-hours`), `The first run waits: REASON.`. The Run history page
lists each run, and why one failed or waited, and the log has a `run`
line for each. Once it has run, with no incident written down over the
weeks shown, the pages call no rule noisy and show no precision
([above](#no-record--not-judged)).

### Notices, the digest or reports don't arrive

The Settings page's Notifications section, and its card on the
Administration page (`/manage`), say where what happens is told, where the
weekly digest and reports go, and whether they went: `Nothing goes out: no
webhook, Slack or place for reports is given.`, `The weekly digest goes
nowhere.`, `N notices failed in the last day.`. A notice that wasn't sent
is tried again later, further apart each time, ten times at most; each
failure is in the log:

```
time=TIME level=WARN msg="a notice wasn't sent; it is tried again later" event=NAME to=DEST attempts=N err=TEXT
```

A team's digest (`--team-digest`) isn't sent to a team nothing happened
to. `--notify-slack` needs `--slack-webhook-file`. The notifications the
alert manager sent, kept beside the firings, are another thing:
[Notifications as they were sent](notifications.md).
