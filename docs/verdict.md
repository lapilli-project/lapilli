# Use `lapilli verdict`

```sh
# a week of your Grafana rules, against your own Prometheus
lapilli verdict --rules alert-rules.yaml --prometheus http://prometheus:9090 --incidents incidents.yaml

# what Prometheus actually fired last month
lapilli verdict --alerts --prometheus http://prometheus:9090 --since 30d --incidents incidents.yaml

# a rule you are about to merge: would it have caught last quarter's incidents, and how often would it have paged?
lapilli verdict --rules new-rule.yaml --prometheus http://prometheus:9090 --since 90d --incidents incidents.yaml

# weekly, into node_exporter's textfile collector
lapilli verdict --rules alert-rules.yaml --prometheus http://prometheus:9090 --incidents incidents.yaml \
  --metrics > lapilli.prom.tmp && mv lapilli.prom.tmp lapilli.prom
```

## Reports as JSON, CSV and metrics

`--json` gives the whole report, every firing and its stone included;
`--csv` the firings alone, a row each (rule, severity, labels, start, end,
stone, the incidents it caught, re-fired), for a spreadsheet, a cell that
starts with `=`, `+`, `-` or `@` written with `'` before it so it isn't
run. [docs/numbers.md](numbers.md) defines every number, says how to
count each again from `--json` with jq, and lists the fields, columns and
metrics that keep their names. `--metrics` gives `lapilli_rule_firings`, `lapilli_rule_caught`,
`lapilli_rule_nothing`, `lapilli_rule_refired`, `lapilli_rule_incidents`,
`lapilli_rule_incidents_first`, `lapilli_rule_incidents_alone` (by `rule`),
`lapilli_rule_nothing_by_hours` (by `rule` and `hours`: `asleep`,
`off_hours`, `at_work`), `lapilli_rule_episodes` and
`lapilli_rule_episodes_caught` (each rule's firings counted as
[episodes](numbers.md#episodes), and those caught), `lapilli_incidents`
(by `state` and `source`) and `lapilli_incident_detection_delay_seconds`.

Grafana's rules, from files or from `--grafana`, are all replayed against
one history, `--prometheus`, whatever PromQL data source they name (only
Loki's are asked of their own). With rules on several, `verdict` warns, and
[`preflight`](design/preflight.md)'s `rules.sources` says so: replay each
data source's rules in a run of their own, against its Prometheus (`--rule`
picks them by name).

## How it asks the history

A replay asks for each query a day at a time, once for every rule that needs
it, and runs `--parallel` (8) rules at once against each API it reads:
`--prometheus`, `--loki`, each of Grafana's data sources; a Grafana is asked
`--parallel` requests at once at most for all its data sources, as it answers
for them all, shared fairly: a data source that holds places doesn't take the
last one while another holds none that asked within ten seconds, and a place
given back goes to the one waiting that holds fewest, so a slow data source
can't keep the others waiting on its answers. A server reached by two names,
as `--loki` and as a Grafana data source, or as `--prometheus` and `--loki`,
is asked as two. An API that turns a request away for now is asked again
after a wait:

- after HTTP 5xx (not 501), or a connection it dropped or reset, 2, 4, 8
  and 16 seconds;
- after HTTP 429 (too many requests), or a 503, that says how long
  (`Retry-After`, in seconds or as a date), that long, between a second and
  two minutes, plus up to a quarter, as often as it says so;
- after a 429 that doesn't, 4, 8, 16, 32, 60 and 60 seconds, a minute and a
  half to three minutes in all, past the window of a quota by the minute.

429s and 5xx are counted apart. Each wait is less up to half, or more up to
a quarter, so that requests turned away together don't come back together.
A request is asked again for five minutes at most from when it was first
pushed back, by an answer or by a wait for its turn the API asked for: the
API's answers and the waits for a turn count, and a request whose turn
doesn't come by then is given up; one already asked is waited for. A wait
that would end past `--timeout`, the API's or a turn by `--max-rate`, isn't
waited for: the rule is left unfinished then, as the deadline would leave
it, and the run exits 1 saying to raise `--timeout`.

A 429, a 503 or a connection dropped also halves how many requests may go
to that API at once: half of those in flight lately, or of the limit, if
fewer, from 64, for the whole run, down to one, once a second at most; eight
at once, as `--parallel 8` has, are four after one push back. One more may
go for every ten seconds without a push back, asked or not, so `lapilli
serve`, back an hour later, asks as many at once as before. While an API said
to wait, nothing else is asked of it: a request waiting for its turn looks
again when it wakes. Behind one Grafana, each data source backs off of its
own, as Grafana passes on what each answers. `--max-rate N` asks each API N
requests a second at most, evenly spaced, for a quota known beforehand:
`--max-rate 0.5` for one of 30 a minute; a Grafana's data sources share it.
Runs at once that share a quota should each take their part of it. This is
the same for `--prometheus` and every cloud's `--auth`, for `--loki`, its
ruler included, and Grafana's data source proxy, and for `lapilli serve`,
which takes `--max-rate` too. `--max-samples N` (default 10,000,000) is the
most samples the answers to one read of a rule's query may hold in all, some
64 bytes each at the peak. A replay reads a day at a time, on days from
midnight UTC, so the bound is a day's: 1,440 samples a series at a minute,
some 6,900 series firing all day. Past it the rule isn't replayed, and the
reason says to read it at a longer `--step`, over a shorter `--since`, or with
a larger `--max-samples`. What lapilli does without, the configuration
and the targets Prometheus is asked for, and where a Loki's ruler runs its
groups as `lapilli serve` reads it each minute, is asked once, within 30
seconds, not while the API said to wait, and its answers don't change how
the rest is asked (the configuration 2 MiB at most, the targets 32, as
each is parsed whole); `lapilli serve` keeps where the groups run as last read
when it can't be read.

## Keeping off the alerts' quota

`lapilli serve` pointed at the history the alerts are evaluated against can
keep off their quota ([docs/design/quota.md](design/quota.md)). Its
`/metrics` counts what each process asks of each API (`prometheus`, `loki`,
`grafana`): `lapilli_serve_history_requests_total` by the class of the
answer, the 429s, the requests asked again and the seconds waited, to set
beside the evaluator's own errors. Each of these is off by default:
`--yield-on-429` stops a run's requests to an API at its first 429, not
asked again, and what reads the history after the run that round (reviews
of changes, the digest, backtests, changes carried out, the replay check)
waits for a later one; the rules left are replayed first by a later run,
from where they were missed, and the runs go no further than a week past
the oldest of them, so none is given up (`--yield-for DUR`, a day at most,
leaves the API alone longer). `--guard EXPR` holds each run, and each day of
a catch-up, while a PromQL expression such as
`rate(prometheus_rule_evaluation_failures_total[5m]) > 0` returns anything
or can't be read; one refused as written fails the run. `--catch-up-hours
01:00-06:00` keeps a catch-up, more than a day behind (the first start's
`--backfill`, a run after being down that long), to those hours in `--tz`,
at `--catch-up-rate N` if given. A run that waits is kept in the run history
as held, with why, and `lapilli_serve_runs_held{reason}` says it.

## Progress while it runs

A run that takes longer than 15 seconds says on stderr, every 30 seconds,
how it is going; `--progress=false` keeps it quiet. A run the history
answered HTTP 429 says so at its end either way. stdout has the report
alone; the [backtest recipes](../recipes/backtest) copy stderr to the job's log
as it comes, and none of the progress to the comment. A comparison counts
replays: a rule changed is replayed before and after.

```
lapilli verdict: after 2m30s, 23 of 61 rules replayed, 412 requests to the history, 37 asked again after a wait, 35 answered HTTP 429
```

## When a query fails

A query fails one of two ways, and its rule's reason says which:

- `query failed: the history couldn't be read: ...`, a failure of the run:
  the history wasn't reached (a connection, a name, TLS), didn't let lapilli
  in (HTTP 401, 403, 407, credentials that couldn't be had), wasn't where it
  was said to be (404, a redirect to sign in, a page or JSON that isn't the
  API's, from a proxy or a gateway), or turned the query away to the end
  (408, 429, 5xx). Another run, or this one with its access mended, may
  replay the rule.
- `query failed: ...` otherwise, the history's answer to the query as
  written, which every run gets: refused in the API's own words (HTTP 400
  or 422: a parse error, an execution error), too large however short the
  range asked, not done (501), or an answer that can't be read.

A rule whose replay failed for the run (the history unread, or the time run
out) is named with why at the top of the report, as the rest of it leaves
the rule out, ten at most, and at its foot with the rules that can't be
replayed at all (a CloudWatch metric alarm, a rule paused, a query the API
refused as written). `--json` lists both in `unreplayed`, each with `failed`
true when its replay failed for the run and another run may replay it; in a
comparison, each with its side, `before` or `after`, but a rule alike in
both, replayed once for both, which has none. The exit stays 0, since what
was replayed holds, unless no rule was: a run in which every replay failed
exits 1, as it judges nothing (`--fail-nothing-replayed=false` exits 0
instead, and `--compare` is left as it was); `--fail-unreplayed` exits 1 for a
single rule that failed, for a check that a run that couldn't read the history shouldn't pass; so too for a rule whose
labels on each of its series are more than `--max-samples` lets a replay
hold (`too many labels on its series to replay`), which a rule's writer
could do to keep it out of a backtest unseen. It doesn't count a query
refused as written, which fails every run until the rule changes, nor a
Grafana rule replayed whose query outside its condition failed, which the
report notes below its table. A run cut short by `--timeout` exits 1 either
way. The API's text in a reason is cut to 300 bytes, without what doesn't
show, and without the token, the signature or the password lapilli sent,
should the API echo it back; `lapilli serve` keeps a reason without how often
its request was asked.

## Grafana rules judged from their history

A rule `--grafana` holds that can't be replayed, its condition on MySQL,
Elasticsearch, InfluxDB, a cloud's monitoring or TestData, is judged from
what Grafana's own alert state history says it fired, read as
[`--check-replay`](check-replay.md) reads it: in annotations, Grafana's
default from 12.4 on, or in Loki, from a day before the period, with the
token `--grafana-token-file` holds (a Viewer's). Its stones, precision,
re-firings, hours and the incidents it caught are counted as any rule's.
It stays not replayed, with why, so it has no longer `for`, near miss or
`--compare`, and the report says so under its table; `--json` gives it
`source` `grafana`. Its labels are as the history keeps them, in
annotations a colon an underscore. Where the history can't be read, holds
nothing from before the period, or doesn't tell the rule apart (passed
over changes of it, or gives its title to another rule too), it is listed
as before, and stderr says why. `--grafana-history=false` lists them all
([docs/design/grafanajudged.md](design/grafanajudged.md)).

## Rules replayed and firings recorded in one report

A team whose alerts are partly rules and partly a cloud's own alarms that
can't be replayed can have one report. Give `--firings FILE`
([a cloud's own alarms](cloud-alarms.md)) beside `--rules`, `--loaded-rules`,
`--grafana`, `--ruler`, `--alarms` or `--loki-rules`: the recorded firings are
judged with the replayed ones, over the same period, against the same
incidents, and each recorded alarm is a rule of the report. An incident only
an alarm caught is caught, not missed. Such a rule is not replayed, as a
Grafana rule judged from its history isn't: no longer `for`, near misses or
`--compare` for it, and the report says so under its table; `--json` gives it
`source` `firings`. It is no rule that couldn't be replayed, so
`--fail-unreplayed` and `--fail-nothing-replayed` count only the rules
replayed: a run in which every replay failed exits 1 though the recorded
firings were judged, the report then judging only what was recorded. A rule's firings come from one place: a name both a rule replayed
and the file have is refused, exit 2, listing up to five of them.
`--firings-prefix STR` puts `STR` before every recorded name, to keep the
two apart on purpose; alone, with `--firings` and no rules replayed, it is refused, exit 2. `--firings` doesn't go with `--alerts` or `--compare`.

## Rules kept in Terraform

Grafana alerting managed with the Grafana provider's `grafana_rule_group`
is read from the JSON Terraform writes once its values are resolved
(variables, modules and functions included), not from the HCL: `--rules`
takes it as it takes any rules, by what it holds. Of a saved plan:

```sh
terraform plan -refresh=false -out=tfplan && terraform show -json tfplan > rules.json
lapilli verdict --rules rules.json --prometheus http://prometheus:9090 --incidents incidents.yaml
```

or of the state, `terraform show -json > rules.json`. Every
`grafana_rule_group` in the root module and in its child modules is read,
as the Grafana provisioning file it describes: a rule is replayed, a
paused one is listed as paused, a recording rule is read as recorded. A
plan has the values the resources will have, so a group the plan destroys
isn't in it. A value Terraform can't know before it applies, such as a
`folder_uid` taken from a folder made in the same plan or a rule's `uid`,
is empty. `lapilli preflight` counts such rules as Grafana, from Terraform.
A pull request's backtest can render it: see
[recipes/backtest](../recipes/backtest/README.md#rules-rendered-from-terraform).

## Any Prometheus-compatible API

`--prometheus URL` takes any Prometheus-compatible API: Prometheus, Thanos,
Mimir, VictoriaMetrics, or a cloud's managed Prometheus. The CI job
`e2e-compat` (`test/e2e/compat.sh`) runs `lapilli verdict` and
`--check-replay` against Thanos 0.42.5, a sidecar and its query, and
VictoriaMetrics 1.156.0, with vmalert 1.156.0 evaluating the same rules
there: the same firings as against the Prometheus beside them, and every
firing alike with `ALERTS`. For `--loaded-rules` on vmalert, give
`--prometheus` the VictoriaMetrics that answers `/api/v1/rules` from it
(`-vmalert.proxyURL`). Its groups are replayed at the times vmalert asks
their rules at, a multiple of their interval or `eval_offset` past one,
not when it last started them; a group whose file sets `eval_alignment:
false`, which its API doesn't say, is replayed as if aligned.
`--bearer-token-file` adds a token. For a cloud's,
`--auth` sends the credentials it wants, taken from those the machine or
pod already has (environment, workload identity, instance or managed
identity, the cloud's CLI):

| Managed Prometheus | `--prometheus` | `--auth` | Read permission |
|---|---|---|---|
| Amazon Managed Service for Prometheus | `https://aps-workspaces.REGION.amazonaws.com/workspaces/WORKSPACE` | `aws` (SigV4) | `aps:QueryMetrics` |
| Azure Monitor managed service for Prometheus | the workspace's query endpoint, `https://WORKSPACE.REGION.prometheus.monitor.azure.com` | `azure` | Monitoring Data Reader |
| Google Cloud Managed Service for Prometheus | `https://monitoring.googleapis.com/v1/projects/PROJECT/location/global/prometheus` | `google` | `roles/monitoring.viewer` |
| Amazon CloudWatch, its OpenTelemetry metrics | `https://monitoring.REGION.amazonaws.com` | `aws` (SigV4) | `cloudwatch:GetMetricData`, `cloudwatch:ListMetrics` |

`--aws-region` names the region to sign for when the URL doesn't (a VPC
endpoint, say). `--gcp-project PROJECT` is short for Google Cloud's URL and
`--auth google`. Lapilli only reads from these.

Of these, the Google path has been run against a real project. The Amazon
and Azure paths are tested against SigV4 and token checks written apart from
the SDKs, not yet against a real workspace; the reading of CloudWatch's
PromQL alarms and Google Cloud's PromQL policies, and their firing recipes,
against their documentation's answers, not yet a real account.

CloudWatch answers PromQL over the metrics sent to its OpenTelemetry
endpoint, and over AWS's own metrics once their OpenTelemetry enrichment is
turned on. It answers at most 500 series a query and says so only in a
warning; Lapilli takes that as the query failing, so a rule replayed is never
one cut short. Its queries count against a daily allowance (100,000 series
and 300 million samples a day, by its documentation), so lower `--parallel`
and the period before replaying many rules over weeks. Google Cloud keeps
some of its services' metrics, Istio's and log-based metrics six weeks only,
and every point older than six weeks ten minutes apart, so a replay further
back than that is coarser there.

Oracle Cloud has no managed Prometheus to query: OCI Monitoring is queried in
its own language, MQL. On OKE, give `--prometheus` the Prometheus you run
there; it needs no `--auth`.

## Each cloud's own rules

Each cloud keeps the alert rules of its managed Prometheus, and its own
alarms written in PromQL, in a form of its own, and `--rules` reads each:

| Cloud | Rules | To read them |
|---|---|---|
| AWS | a rule groups namespace | the AWS CLI's `describe-rule-groups-namespace` answer, as is; `--loaded-rules --auth aws` (`aps:ListRules`); or every namespace, as written, with `--ruler https://aps.REGION.amazonaws.com/workspaces/ID --auth aws` |
| AWS | CloudWatch PromQL alarms | the AWS CLI's `describe-alarms` answer, as is. The pending period is `for`, the recovery period `keep_firing_for`. Its other alarms are listed as not replayed |
| Azure | `Microsoft.AlertsManagement/prometheusRuleGroups` | an ARM template, or the JSON the Azure CLI or REST API gives for rule groups; or every rule group in a subscription or resource group with `--ruler https://management.azure.com/subscriptions/SUB --auth azure`. Severity 0 to 4 becomes `severity=Sev0` to `Sev4`, and `timeToResolve` is how long an alert stays firing once its condition stops |
| Google Cloud | `Rules`, `ClusterRules`, `GlobalRules` (`monitoring.googleapis.com/v1`) | the resources, as files or from `kubectl get ... -o yaml` |
| Google Cloud | Cloud Monitoring alerting policies with a PromQL condition | `gcloud monitoring policies list`, in YAML or JSON, or the API's answer. The condition's `duration` is `for`, its `alertRule` the name; its severity becomes `severity=critical`, `error` or `warning`; an alert closes at the first evaluation at least the larger of 270 s and two evaluations after its condition last held, as Google Cloud closes them. Every policy's interval is a multiple of 30 s (30 s unless it says), so give `--step 30s`. Policies with other conditions are listed as not replayed |
| Oracle Cloud | no managed Prometheus | the rule files of the Prometheus you run |

Google Cloud evaluates `Rules` within their namespace and cluster, and
`ClusterRules` within their cluster; Azure, a rule group with a
`clusterName` within that cluster. Lapilli does the same where the resource
says how: every selector in a `Rules` expression gets its namespace as a
`namespace` matcher, and in an Azure rule group with a `clusterName`, that
as a `cluster` matcher. A Google Cloud cluster's name isn't in the
resource, so with several clusters in one project, add `cluster` matchers
to the expressions, or point `--prometheus` at that cluster's data alone.
