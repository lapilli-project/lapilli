<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/lapilli-lockup-dark.png">
  <img alt="Lapilli" src="assets/lapilli-lockup.png" width="300">
</picture>

**Which of your alerts are worth their page?** `lapilli verdict` judges every
firing against what actually went wrong: a white stone when the alert caught an
incident, a black stone when nothing was wrong. Count a rule's stones over a
week and you know whether it earns its place; an incident with no stone is one
your alerts missed. `lapilli serve` keeps judging, every hour, in a ledger with
a dashboard, where teams follow the noisy rules and missed incidents up
([below](#keep-judging)). It reads by default; turned on, it also makes the
changes it proposes, each once a person has approved it, as a pull request
to where the rule is kept ([below](#changing-rules)).

```
$ lapilli verdict --rules alert-rules.yaml --prometheus http://prometheus:9090 \
    --incidents incidents.yaml --impact 'sum(rate(http_5xx[5m])) / sum(rate(http_total[5m])) > 0.01'
Lapilli  2026-10-26 00:00 → 2026-11-02 00:00 Asia/Seoul
148 firings, 3 incidents written down, 1 more found by --impact

RULE                SEVERITY  FIRED  ○ CAUGHT  ● NOTHING  PRECISION  REFIRED
ExportJob5xx        critical  118    0         118        0%         41
CheckoutLatencyP99  warning   14     2         12         14%        3
ReadReplicaLag      warning   9      1         8          11%        0
NodeNotReady        critical  4      0         4          0%         0
DBPrimaryCPU        critical  3      2         1          67%        0

Noisy rules that a longer "for" would quiet, still catching every incident they caught:
RULE                FOR       FIRED     INCIDENTS  CAUGHT LATER BY
ExportJob5xx        5m → 15m  118 → 31  0          –
CheckoutLatencyP99  5m → 10m  14 → 6    1          up to 4m

What each rule adds: the incidents it caught, those it caught before any other rule, those no other rule caught:
RULE                INCIDENTS  FIRST  ALONE
CheckoutLatencyP99  1          0      0
DBPrimaryCPU        1          1      1
ReadReplicaLag      1          1      1
A noisy rule's catch doesn't count beside one by a rule that isn't noisy.

38 rules never fired.

INCIDENT                         START             CAUGHT BY
INC-41 Checkout errors           2026-10-28 12:01  DBPrimaryCPU +2m, CheckoutLatencyP99 +6m (noisy)
INC-42 Search timeouts           2026-10-30 09:15  ReadReplicaLag -3m
INC-43 Batch stuck               2026-10-31 02:40  ✗ nothing fired
impact-1 user impact (--impact)  2026-11-01 21:27  ✗ nothing fired

✗ 2 of 4 incidents had no alert.
```

> Status: early, at v0.20, with one maintainer and room for more
> ([MAINTAINERS.md](MAINTAINERS.md)). Checked against one production Grafana:
> all 71 of its alert rules replay, and a four-week replay of the rules as
> they stood then found 177 firings of the noisiest rule, the number of
> notifications its team had counted by hand in their alert channels. Weekly
> counts of critical alerts came within 10% of theirs in three of the four
> weeks.

## Why

Alert rules pile up. One is added after each incident, few are taken away, and
nobody can say which ones still earn their page. The way teams find out is by
hand: read a week of alert channels next to the incident notes, mark each alert
as useful or noise, and look for incidents that nothing caught. It is slow, so
it rarely happens, and a rule that has paged for nothing a hundred times keeps
paging.

Lapilli does that reading. It works from what you already have: the rules, the
history in Prometheus, and whatever record of incidents you keep, even a thin
one.

## The name

> *mos erat antiquus niveis atrisque lapillis,*<br>
> *his damnare reos, illis absolvere culpa*
>
> It was the old custom to judge with white and black pebbles: the black to
> condemn the accused, the white to acquit them.
>
> — Ovid, *Metamorphoses* XV.41–42

*Lapilli* are small stones. Each firing is a vote; the urn is emptied once a
week and the stones are counted.

## How it judges

**Firings** come from one of three places:

- `--rules FILE`: replay alert rules against history, so it works on rules you
  just wrote and on history from before you had Lapilli. It reads Prometheus
  rule files, `PrometheusRule` resources, Grafana alerting provisioning files,
  each cloud's managed rule groups and its alarms written in PromQL (see
  below), ConfigMaps holding any of them, and directories of them, below them too; below the top, files that
  don't parse, such as a Helm chart's templates, are passed over. Grafana rules are evaluated the way
  Grafana evaluates them: each query fetched as PromQL, then reduce, threshold
  and math, where a comparison gives 1 or 0 and two sets of series pair up by
  their labels; classic conditions too, every reducer and evaluator, as
  Grafana's own code has them. What Grafana does when a query returns nothing
  (`noDataState`) and when one series of several goes is replayed with it,
  in Grafana's own states; query errors can't be
  ([docs/design/states.md](docs/design/states.md)). `for`,
  `keep_firing_for`, a group's `interval` and `query_offset`, and a Grafana
  query's window that ends before now are honoured. `--rules FILE@TIME` gives
  the rules as they stood from a time on; see "Rules that changed" below.
- `--loaded-rules`: replay the alert rules the API at `--prometheus` has
  loaded, as `GET /api/v1/rules` lists them: Prometheus, Thanos, Mimir,
  vmalert, Amazon Managed Service for Prometheus. Each group is evaluated at
  the second of its interval it last ran at, as the API says, where
  Prometheus placed it.
- `--grafana URL`: replay the alert rules a Grafana holds, made in its UI
  or not, read through its provisioning export API with the service account
  token in `--grafana-token-file`; and the scrape interval each data source
  says it has (`jsonData.timeInterval`), which Grafana puts
  `$__rate_interval` in with. A Viewer's token reads both.
- `--ruler URL`: replay the alert rules a managed Prometheus ruler holds,
  as written (the expression as it was written, not printed again), read
  through the API that writes them: a Mimir's or a Cortex's configuration
  API (`http://mimir:8080/prometheus/config/v1/rules`,
  `http://cortex:9009/api/v1/rules`), with a token in `--ruler-token-file`
  and, for one with tenants and no gateway, `--ruler-tenant`; Amazon Managed
  Service for Prometheus's control plane
  (`https://aps.REGION.amazonaws.com/workspaces/ID`, with `--auth aws`); or
  Azure's Prometheus rule groups in a subscription or a resource group
  (`https://management.azure.com/subscriptions/SUB`, with `--auth azure`).
  Rules of one name in two namespaces are told apart by both:
  `ScrapeSlow [payments/shop]`, a `/` within either written `%2F`
  (`ScrapeSlow [alloy%2Fpayments/shop]`). A group the ruler gives in a form
  that can't be read (Mimir 3.2.4 writes an expression whose first line
  starts with spaces so) is passed over and logged, and the rest read.
- `--alarms SOURCE=URL`: also read a cloud's own alarms through its API,
  beside any of the above or alone (repeatable): CloudWatch's
  (`https://monitoring.REGION.amazonaws.com`), Azure Monitor's alert rules
  in a subscription or resource group
  (`https://management.azure.com/subscriptions/SUB`), a Google Cloud
  project's alerting policies
  (`https://monitoring.googleapis.com/v3/projects/PROJECT/alertPolicies`),
  OCI Monitoring's alarms in a compartment
  (`https://telemetry.REGION.oraclecloud.com/20180401/alarms?compartmentId=OCID`).
  Each is read with its cloud's credentials, whatever `--auth` says.
  `SOURCE` names them, as their history is copied in; an alarm and a rule of
  one name are told apart by it: `HighCPU [cloudwatch-prod]`. PromQL
  alarms are replayed, the others listed (see "A cloud's own alarms").
  `lapilli serve` reads them every `--alarms-every` (10m).
- `--alerts`: the `ALERTS` series Prometheus keeps for its own rules.
- `--firings FILE`: firings recorded elsewhere, one JSON object a line
  (`{"rule":"...","labels":{...},"start":"...","end":"..."}`), such as the
  history of a cloud's own alarms (see "A cloud's own alarms" below).

**Incidents** come from a file, from a signal of user impact, or both:

```yaml
incidents:
  - id: INC-41
    title: Checkout errors
    start: 2026-10-28T12:01:00+09:00
    end: 2026-10-28T12:40:00+09:00   # optional; an hour when missing
    labels: {service: checkout}      # optional, for --match; [a, b] for several
```

`--impact PROMQL` turns every window where the expression returns something,
for at least `--impact-for` (5m), into an incident too. An error-rate SLI works
well: the incidents nobody wrote down are often the ones no alert caught. Each
series is looked at on its own and its windows carry its labels, so with
`sum by (service) (...)` an incident says which services were hit; windows
that overlap are one incident listing all of them.

**A firing catches an incident** when it starts between `--lead` (15m) before
the incident and its end, and agrees with it on any `--match` labels both
carry: `--match service` keeps an alert about one service from catching an
incident in another just because they came at the same time. An incident that
lists several values agrees with any of them, and an alert without the label
(a database or a node) is not held to it. An alert that was already firing before that caught nothing: it was on
anyway. A firing that comes within `--refire` (1h) of the same alert resolving
is counted as refired.

**Noisy rules.** A rule that fired 10 times or more in the period, with under
20% of its firings catching anything, fires whatever happens; when it lands on
an incident that is likely chance. Its catches are shown marked `(noisy)`, not
dropped, since a noisy rule can also fire for the right reason. An incident
caught only by noisy rules is reported as such, and its detection delay is
taken from the first rule that isn't noisy. `--noisy-firings` and
`--noisy-precision` move the line.

**What to change.** Each noisy rule replayed from `--rules` is replayed again
with a longer `for` (5m, 10m, 15m, 30m, 1h, 2h). The shortest that would have
caught every incident the rule caught, firing at most half as often, is
suggested. Rules with a `severity` label show it, and `--metrics` carries it.

**What a rule adds.** Precision doesn't say whether anyone would miss a rule:
one that is right every time, but always after another, adds nothing. Each
rule gets the incidents it caught, those it caught before any other rule, and
those no other rule caught; a noisy rule's catch doesn't count beside one by a
rule that isn't noisy. A rule that caught two incidents or more is called
spare when, for each, a rule that isn't noisy and can still fire caught it as
soon or sooner, at the same severity or above (`critical`, `warning`, `info`;
other severities only cover their own). Of rules that caught it at the same
moment, the first by name stays, so every spare rule could go at once and each
incident would still be caught as soon. This is a question to ask, not a verdict:
it says only what happened in the period, and only among the rules that could be
replayed. With `--rule` or `--changed-only`, which judge some of the rules, only
how many incidents each caught is given.

**When they came.** A firing that caught nothing at three in the morning cost
more than one at three in the afternoon. The firings that caught nothing are
counted by when they started, in `--tz`: while people slept, off hours, or at
work. `--work-hours` (`mon-fri@08:00-18:00`) and `--sleep-hours`
(`22:00-08:00`) say when those are, or `none`. The defaults are close to
PagerDuty's, which also counts 21:00 to 22:00 as both; here each minute is one
or the other. Working hours may run past midnight, as a night shift's do
(`mon-fri@22:00-06:00`: the mornings after are worked). For a team on call in
shifts, or by turns across time zones, `--shifts` gives hours that take turns
instead: how long each turn is, when the first starts, and each turn's hours,
those it leaves out as above (`every=1w from=2026-11-02T09:00; tz=Asia/Seoul;
tz=Europe/Berlin sleep=23:00-07:00`, the pager changing hands on Mondays at
09:00). Turns of days or weeks change at the same time on the clock, the
clocks changing or not. The rules that woke people for nothing come first.

**Shifts from on-call schedules.** Real rotas swap, override and skip.
PagerDuty, Opsgenie, Jira Service Management, Grafana OnCall and IRM, and a
shared Google Calendar each export a schedule as an iCalendar feed, whose
turns name people, not hours. `--on-call FILE` names the feeds and the
people in them with their hours, and `--shifts on-call=NAME` (or a team's
shifts in `lapilli serve`) counts each firing by the hours of whoever's turn
it came in; between turns, and for someone the file doesn't name, by the
hours given otherwise:

```yaml
schedules:
  - name: payments
    url_file: /secrets/payments-ics   # PagerDuty's webcal URL, its key in it
  - name: platform
    url: https://api.opsgenie.com/v2/schedules/platform.ics
    token_file: /secrets/opsgenie
    auth: genie                       # bearer (default), genie or basic
    who: '^(.+?) \(user\) is on call' # the person, out of each turn's title
people:
  - {name: Jane Doe, also: [jane@example.com, jdoe], tz: Asia/Seoul, sleep: '23:00-07:00'}
  - {name: Ali Kaya, also: [ali@example.com], tz: Europe/Istanbul}
```

A turn's person is an attendee's email that is someone's in the file, else
what `who` takes out of its title, or its title when that is someone's,
else the one attendee's email, else its title (a person added to the file
later, by any of these, is found in the turns kept before; a name is cut
to the ledger's 200 bytes). Two people in the file as an event's
attendees, or two events that start together, are said; a turn that starts
during another holds until it ends. An event that can't be read is passed
over and said, and the turns kept of whoever it names stay as they were. Times in UTC, with an IANA `TZID`, or
floating (the schedule's `tz`) are read; daily and weekly repeats are
expanded (`WKST` honoured), with `EXDATE` and occurrences replaced by
`RECURRENCE-ID`; what
can't be read (a Windows zone name, a monthly repeat) is passed over and
said. A feed's URL holds its secret, so it is read from its file, never
shown or logged, fetched over https only, with no redirect followed, 8 MB
and 30 seconds at most. `lapilli serve` reads each feed every `every` (30
minutes by default) and keeps the turns in its ledger past what the feed
looks back (PagerDuty a month, Grafana OnCall 15 days), and its runs page
says when each was last read and whom its turns name that the file
doesn't. A person's hours changed in the file count for every turn they
had.

**What nearly caught it.** For an incident nothing caught, or only noisy rules
did, each rule replayed from `--rules`, as it stood then, is looked at about
then: one whose condition held, but not for its `for`, or only long enough to
fire after the incident was over, nearly caught it. The longest shorter `for`
that would have caught it is given, with when the rule would then have fired.
Of the rest, one whose condition compares a value with a number (`errors >
0.9`, `0.9 < errors`, or a Grafana threshold or math expression with one
number) nearly caught it when the value came within a fifth of the number, for
the rule's `for`: the round number short of what it reached that would have
caught it is given (`> 0.9 → 0.87`, in steps of a tenth of the number's
leading figure), with when the rule would then have fired. A condition with
several numbers, a Grafana range (`within_range`, `outside_range`) or PromQL
joining comparisons with `and`, `or` or `unless`, has each looked at, the
others as they are. The first comparison of an `and` or `unless` returns its
own values where the rest holds, so they are read as one comparison's are; for
any other, the rule is replayed with nearer round numbers, the furthest within
a fifth first and, when the rule fires during the incident with that, halving
the way back to the nearest that does (`rate(requests[5m]) > 10 → 8`). A
Grafana math expression joining comparisons with `&&` or `||` has each
looked at, read where the rest lets it decide (`$A > 0.9 → 0.87`), and so
has each number of classic conditions, by the value its reducer gives
(`avg(A) > 5 → 4.5`); a classic range is replayed with nearer numbers. A comparison on the right of `unless`
kept the rule quiet, and its number moves the other way (`unless requests <
10 → 9`). For the first ten
tried, the report says how often the rule would have fired over the period
with that, and how often for nothing. A noisy rule, or one with a `for` longer
than a week, or with neither a `for` nor such a number, isn't looked at. It is
bounded: twenty incidents, five rules for each, 2,000 questions of the
history, within the period and `--timeout`; the report says when the time ran
out. A rule whose value never came within a fifth says nothing here: what a
change further than that would do is for `--compare`.

**Rules that share a name**, in different groups or with different labels
(a warning and a critical one, say), are told apart:
`HighErrorRate [severity=critical]`, `DiskFull [node-alerts]`.

**Rules that changed.** Rules change: one is added after an incident, a
threshold is raised. Today's rules, replayed over last month, judge a month
that ran on other rules. Give each version with the time it took effect, and
each is replayed while it was in effect:

```sh
lapilli verdict --prometheus http://prometheus:9090 --since 30d --incidents incidents.yaml \
  --rules rules-before.yaml \
  --rules rules-v2.yaml@2026-10-12T14:00:00Z \
  --rules rules-v3.yaml@2026-10-21T09:30:00Z
```

Give every file in effect from a time with that time, and the rules in effect
before the first time without one. Where a version takes over, alerts carry
over the way they would have: Prometheus keeps a rule's alerts through a
reload when its group, name and labels stay the same, pending or firing, and
applies the new `for`; Grafana keeps them when the rule (same uid) is
unchanged, and resolves them and starts afresh when it was edited or paused.

Rules kept in git give their versions. `recipes/versions/git.sh` writes them
as a directory that `--rules-history DIR` reads, one subdirectory a version,
named by when it took effect, holding the rule files as they stood then:

```sh
REPO=~/src/monitoring PATHS=alerts/ SINCE=2026-10-01 OUT=history recipes/versions/git.sh
lapilli verdict --rules-history history --prometheus http://prometheus:9090 --since 30d --incidents incidents.yaml
```

It is each version given as `--rules DIR@TIME`. The recipe takes each commit
on the branch that changed `PATHS` (a merge at the merge's time) and the
rules as they stood at `SINCE`; with `RENDER`, a command such as `helm
template`, it writes what that renders from each commit instead, for rules
rendered from a chart. A version's name is its commit's time, or, with
`DEPLOYS`, when it was deployed: `recipes/versions/argocd.sh` (an Argo CD
Application's sync history, from its API or `kubectl get application -o
json`) and `flux.sh` (a Flux Kustomization's history, 2.7 on, or a
HelmRelease's whose chart version ends `+SHA`) write the revisions deployed
and when, and the versions are the rules each deploy put in place, from the last
deploy at or before `SINCE`, a rollback too, never dated before its
commit. Commits deployed together are one version; one not deployed yet
isn't in effect. Failed reconciles aren't deploys. Argo CD keeps ten syncs and Flux five: run them
each time, with `DEPLOYS_KEEP` to keep what earlier runs saw. Any tool
can write such a directory, or a person by hand: a name is an RFC 3339 time,
`2026-10-12T14:00:00Z`, or `20261012T140000Z`.

`lapilli serve` takes the same directory, beside `--rules`, for what came
before its first start: its first backfill then replays each day with that
day's rules, rather than all of it with today's. A ledger with versions of
its own keeps them and passes the directory over, so the flag can stay. In
the chart, `serve.rulesHistory` clones the repository the rules are kept in
and runs the recipe in an init container on each start; when git can't be
reached, it leaves the directory empty rather than hold the pod up, and
`serve` starts with today's rules. A version that can't be read stops the
start, with the ledger as it was, and says which; a version from after now,
a commit dated ahead of the clock, is passed over, and so is the whole
directory once the ledger has versions of its own. A version is files: one
holding a link to outside it is refused. With `verdict`, `--rules` beside
`--rules-history` are the rules as they are now, the newest version.

## Use

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

`--json` gives the whole report, every firing and its stone included.
`--metrics` gives `lapilli_rule_firings`, `lapilli_rule_caught`,
`lapilli_rule_nothing`, `lapilli_rule_refired`, `lapilli_rule_incidents`,
`lapilli_rule_incidents_first`, `lapilli_rule_incidents_alone` (by `rule`),
`lapilli_rule_nothing_by_hours` (by `rule` and `hours`: `asleep`,
`off_hours`, `at_work`), `lapilli_incidents` (by `state` and `source`) and
`lapilli_incident_detection_delay_seconds`.

A replay asks for each query a day at a time, once for every rule that needs
it, and runs `--parallel` (8) rules at once; lower it if the API pushes back.

`--prometheus URL` takes any Prometheus-compatible API: Prometheus, Thanos,
Mimir, VictoriaMetrics, or a cloud's managed Prometheus. `--bearer-token-file`
adds a token. For a cloud's, `--auth` sends the credentials it wants, taken
from those the machine or pod already has (environment, workload identity,
instance or managed identity, the cloud's CLI):

| Managed Prometheus | `--prometheus` | `--auth` | Read permission |
|---|---|---|---|
| Amazon Managed Service for Prometheus | `https://aps-workspaces.REGION.amazonaws.com/workspaces/WORKSPACE` | `aws` (SigV4) | `aps:QueryMetrics` |
| Azure Monitor managed service for Prometheus | the workspace's query endpoint, `https://WORKSPACE.REGION.prometheus.monitor.azure.com` | `azure` | Monitoring Data Reader |
| Google Cloud Managed Service for Prometheus | `https://monitoring.googleapis.com/v1/projects/PROJECT/location/global/prometheus` | `google` | `roles/monitoring.viewer` |
| Amazon CloudWatch, its OpenTelemetry metrics | `https://monitoring.REGION.amazonaws.com` | `aws` (SigV4) | `cloudwatch:GetMetricData`, `cloudwatch:ListMetrics` |

`--aws-region` names the region to sign for when the URL doesn't (a VPC
endpoint, say). `--gcp-project PROJECT` is short for Google Cloud's URL and
`--auth google`. Lapilli only reads from these.

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

## A cloud's own alarms

Alarms kept in a cloud's own monitoring are written in PromQL, where Lapilli
replays them like any rule (the tables above), or in that cloud's own terms:
a CloudWatch metric alarm, an Azure Monitor metric or log search alert, a
Google Cloud threshold or log-based policy, every OCI Monitoring alarm (MQL).
Those can't be replayed, but what they fired can be judged.
[recipes/firings](recipes/firings) has a small script for each cloud that
reads its alarm history and writes it as `--firings` takes it:

```sh
SINCE=2026-10-12T00:00:00Z recipes/firings/cloudwatch.sh > firings.tmp && mv firings.tmp firings.jsonl
lapilli verdict --firings firings.jsonl --since 28d --incidents incidents.yaml --match service
```

| Cloud | Script | Its history |
|---|---|---|
| AWS | `cloudwatch.sh` (`describe-alarm-history`) | 30 days |
| Azure | `azure-monitor.sh` (Azure Resource Graph) | 30 days |
| Google Cloud | `google-cloud.sh` (`projects.alerts.list`) | 13 months, closed alerts |
| Oracle Cloud | `oci.sh` (`get-alarm-history`) | 90 days |

Such a verdict has the stones, precision, what each rule adds and when the
firings came; not what needs a rule's condition: longer `for`s, near misses,
`--compare`. CloudWatch and OCI keep the history of an alarm as a whole, so
an alarm on forty instances is one firing while any of them is in alarm.
The scripts' README says what each counts. `lapilli serve` takes them as
they come and keeps them past what the cloud keeps: see
[Firings recorded elsewhere](#firings-recorded-elsewhere).

Read through the cloud's API with `--alarms SOURCE=URL` too, an alarm
Lapilli doesn't replay and its history copied in under `SOURCE` are one
rule in `lapilli serve`: judged under the alarm's name, with its page and
what it says. The recipes label each alert with the alarm's own ID
(`alarm_id`: its ARN, resource id, policy name or OCID), which ties the
two; a history without one ties by the name the recipe gives it, and one
whose severity changed by the severity the alarm has now. A name two
alarms read under one source share ties neither, nor a Google policy of
several conditions, PromQL ones too (its alerts don't say which opened
them). Its record is its history's, copied in over twelve weeks without a
gap up to now: copies that stopped longer ago than `--source-quiet` are no
record until they run again, nor are weeks they missed until those are
copied in (each write's period is kept); a job copying weekly needs
`--source-quiet 8d`, or retiring stays refused most of the week. While its
history at another severity fired within twelve weeks, it has none. With
none tied to it, it has no record: it isn't among the rules that never
fired, and retiring it isn't proposed.

## Rules on logs: Loki

Rules written in LogQL are replayed against Loki as PromQL rules are
against Prometheus: each metric query (`count_over_time`, `rate`,
`bytes_over_time`, `quantile_over_time` over an `unwrap`, `sum by`, ...)
asked at every evaluation through Loki's `query_range`, and the rule's
`for` and Grafana's states as for any rule
([docs/design/loki.md](docs/design/loki.md)).

```sh
# a Loki ruler's rules, as it holds them, beside the Prometheus rules
lapilli verdict --rules rules/ --loki-loaded-rules --loki http://loki:3100 \
  --prometheus http://prometheus:9090 --since 28d --incidents incidents.yaml

# Grafana's rules on a Loki data source: nothing more to give
lapilli verdict --grafana https://grafana.example.com --grafana-token-file token \
  --prometheus http://prometheus:9090 --check-replay
```

- **Loki's ruler.** `--loki-rules PATH` reads its rule files, beside any
  other source of rules or alone; `--loki-loaded-rules` reads the rules the
  Loki at `--loki` holds, from its ruler's API, with where it runs each
  group. `--loki-token-file` and `--loki-tenant` (`X-Scope-OrgID`) are
  sent to it. `lapilli verdict` needs `--prometheus` beside them only for
  what reads metrics; `lapilli serve` always needs it, as it checks the
  replay against what really fired there. Loki 3.7.9 drops a rule file's
  `query_offset` and `keep_firing_for` and runs without them; so does the
  replay, and says which rules had them. `--check-replay` reads the `ALERTS`
  Loki's ruler writes where its `remote_write` sends them, the Prometheus at
  `--prometheus`. A rule sharing a name with another is told apart as
  `Name [loki:namespace/group]`. A change proposed for one, with its
  backtest, is for a person: Lapilli doesn't write to Loki.
- **Grafana's rules.** With `--grafana`, the type of each data source a
  rule reads is asked of Grafana; a Loki one's queries are asked through
  Grafana's data source proxy (`/api/datasources/proxy/uid/UID`, a Viewer's
  token, by GET), so the Loki, tenant and credentials are those Grafana
  used. Instant and range queries, Grafana's step and its macros
  (`$__auto`, `$__interval`, `$__range`), and its reducers over a range,
  worked out by Lapilli as LogQL has no subqueries. Read from provisioning
  files, a query whose model says `datasource.type: loki` is asked of
  `--loki`.
- **Not replayed, and listed with why:** log queries, which return lines,
  not numbers; a reduce asking Loki finer than a point a second (86,400 a
  series a day); a query Grafana can't run (over Loki's 11,000 points a
  window, or a step or `queryType` Grafana can't read); classic conditions
  over Loki; a condition reading
  Prometheus and Loki together; and every data source that is neither
  PromQL nor LogQL, SQL ones among them, by the type Grafana gives.
- **Late lines.** A log shipper sends a line a second or two after it was
  written; Loki's ruler saw what had arrived. `--late 2s` replays as if
  each line arrived that late. A range Loki refuses for its series
  (`max_query_series`) is asked again in shorter ranges, down to one
  evaluation; an evaluation over the limit fails, as it does in Loki's
  ruler. The chart takes `--loki URL --loki-loaded-rules` (and
  `--loki-tenant`) through `extraArgs`, beside its rules or alone.

Measured against Loki 3.7.9 and Grafana 13.3.1: see the design for the
recordings kept as test data. `test/e2e/loki.sh` runs the same against
real servers in Docker.

## Is the replay right?

Every verdict rests on the replay, so check it on your own rules. With
`--check-replay`, `lapilli verdict` replays the rules and sets them beside the
series that say what really fired, instead of judging. It needs no incidents.

```
$ lapilli verdict --rules alert-rules.yaml --prometheus http://prometheus:9090 --check-replay
Replay beside ALERTS{alertstate="firing"}  2026-10-26 00:00 → 2026-11-02 00:00 Asia/Seoul
139 firings alike, 2 only in the replay, 7 only in what really fired: 94% alike.
21 rules fired alike, 2 differ, 38 fired neither way.

RULE          REPLAYED  REAL  ALIKE  ONLY REPLAYED  ONLY REAL  OFF BY
ExportJob5xx  118       122   116    2              6          30s
QueueDepth    9         10    9      0              1          1m0s

Really fired, but no rule given has the name: Watchdog.
```

Two firings are alike when they are of the same alert, agree on the labels both
have, and start within two evaluations of each other, since Prometheus places a
group anywhere in its interval. Labels a store adds to the series are passed
over, and so is a rule's label with a template in it. A firing with no match too
near the end of the period to tell isn't counted. A group's `interval` is taken
from the rules; a rule file that leaves it to the global `evaluation_interval`
doesn't say, so give that as `--step` if it isn't 1m.

What really fired comes from a series:

| Rules evaluated by | Series | |
|---|---|---|
| Prometheus | `ALERTS{alertstate="firing"}` | [documented](https://prometheus.io/docs/prometheus/latest/configuration/alerting_rules/) |
| Amazon Managed Service for Prometheus | `ALERTS{alertstate="firing"}` | [documented](https://docs.aws.amazon.com/prometheus/latest/userguide/troubleshoot-rule-evaluations.html) |
| Grafana | `GRAFANA_ALERTS{alertstate="firing",grafana_alertstate!="error"}` | once [state history](https://grafana.com/docs/grafana/latest/alerting/set-up/configure-alert-state-history/) is written to Prometheus; the default for its rules. Its error alerts are left out: a query that failed can't be replayed |
| Thanos, Mimir, Azure Monitor, Google Cloud | ask for `ALERTS` and see | their docs don't say; `--check-series` names another series |

When the series hold nothing for the period, or no rule could be replayed, it
says so and exits 1, since nothing was checked. If a rule replays differently from how it
really fired, please [report it](CONTRIBUTING.md): it is the most useful report
there is.

`lapilli serve` does this itself, as it goes
([docs/design/fidelity.md](docs/design/fidelity.md)). Each day the runs
won't replay again is set beside the same series, each firing counted on the
day it started, and kept. A rule's page says, over twelve weeks, how many
were alike, how many only in the replay and how many only in what really
fired, and lists the last of those; the rules list and the dashboard mark a
rule whose replay differed over the last week (five firings either way, fewer
than nine in ten alike, unless `--check-differs N/RATIO` says otherwise), and
`replay.differs` tells the webhook and Slack once a week while it does. The
runs page says how far it has got, which series held nothing (a store may not
keep `ALERTS`; `--check-series` names another), and what really fired with no
rule given by that name. `/metrics` has `lapilli_rule_replay_check{rule,
way}` over seven days, MCP's `rules` and `rule` carry it, and the weekly
digest has a line of it. No stone changes. A cloud's own PromQL alarms
(CloudWatch, Google Cloud, Azure) keep no such series: read with `--alarms
SOURCE=URL`, their history copied in under `SOURCE` stands in, by each
firing's `alarm_id`, a day checked once the copies reach past it (waiting
two days at most); a CloudWatch alarm's replay is first put together as
CloudWatch keeps the alarm, as a whole. `lapilli verdict --check-replay`
takes the same from a recipe's output with `--check-firings FILE`. The
replay checked is the one judged, with `--late` alike: an alarm replayed
without it reads its metrics as filled in since, where the alarm saw them as
they arrived, and may differ for that. Rules that share an alert name
are counted together, as what really fired doesn't tell them apart. The
series are trusted as they are: a scraped target that exposes its own
`ALERTS` would be counted. It is on whenever there are rules
to replay; `--check-replay=false` turns it off, and so does the chart's
`serve.checkReplay.enabled`.

## What an alert says

An alert that fires at the right time can still cost the person woken: a
summary that prints `{{ $labels.instance }}` with no instance, no runbook, a
severity nobody routes on. With `--check-content`, `lapilli verdict` checks
what each rule says when it fires, and how it is written, instead of judging
([docs/design/content.md](docs/design/content.md)):

```
$ lapilli verdict --rules alert-rules.yaml --check-content --severities critical,warning --scrape-interval 1m
What 4 rules say: 3 problems in 3 rules, 3 guesses apart.
Not measured: give --prometheus to replay the rules, for the labels their alerts have, and --incidents for which are noisy.

RULE          KIND  PROBLEM                         DETAIL
DiskFull      fact  no runbook link
QueueDepth    fact  a template that doesn't parse   line 1: unclosed action
ExportJob5xx  fact  a severity not in --severities  crit

Guesses, which may be wrong:
RULE          GUESS                                  DETAIL             WHY
ExportJob5xx  a label its expression may not give    instance           read from its aggregation; it hasn't fired to tell
ExportJob5xx  a range shorter than two scrapes       rate[1m] < 2 × 1m  the scrape interval is the global one, and a job may have its own
TargetGone    absent() with no for: a restart pages                     a series gone for a moment may be what it is there for
```

| Check | Kind |
|---|---|
| No summary or description; no runbook link, or one that isn't an http or https URL | fact |
| A template that doesn't parse, in its source's own language | fact |
| No severity; one not in `--severities` | fact |
| The same expression, `for` and labels as another rule | fact |
| A template label no firing over the period had (with `--prometheus`) | measured |
| Noisy, with no `for` (with `--prometheus` and `--incidents` or `--impact`) | measured |
| A template label after an aggregation by other labels, for a rule that hasn't fired | heuristic |
| A severity fewer than two rules use, among ten or more, without `--severities` | heuristic |
| `rate`, `increase`, `irate` or `delta` over less than two scrapes | heuristic |
| `absent()` with no `for` | heuristic |

What each source says is read from its own fields: Prometheus's
annotations, as the managed services that run its rules keep them,
Grafana's, CloudWatch's `AlarmDescription`, a Google Cloud policy's
documentation, Azure Monitor's alert rules and OCI's alarms (`az monitor
metrics alert list`, `oci monitoring alarm list`, now read as rules too:
listed, not replayed). A cloud alarm's runbook is the first link in its
description; a source with no severity (CloudWatch, Azure's activity log
alerts) isn't faulted for having none. Templates are parsed, never run, with
Prometheus's and Grafana's functions stubbed; a function this version
doesn't know is taken as one a newer Prometheus has. Links are checked to be
http or https URLs, never visited. The range check counts with
`--scrape-interval`, or else with those `/api/v1/status/config` gives, when
it is served: a range over a selector that names its job
(`rate(x{job="batch"}[3m])`, or `job=~` the longest of the jobs it can be)
with that job's own interval, by the job label `/api/v1/targets` says its
targets end up with (under the Prometheus Operator, a ServiceMonitor's job
is its service's name); any other with the global one. The finding says
whose: `rate[3m] < 2 × 2m (job "batch")`. With `--grafana`, the
notification templates each Grafana rule is sent with are checked too
(below). `--json` writes it all, what each rule says included.

A Grafana rule's notification says what its contact point's templates say.
With `--grafana`, Lapilli reads Grafana's notification templates, contact
points and notification policies through the provisioning API, with the
same token (a Viewer's reads them), and works out which templates each rule
is sent with: its own contact point (`notification_settings`), or the ones
the policies route it to by its labels, `alertname` and `grafana_folder`. A
policy matching a label the rule doesn't set may match its series or not,
so both ways are followed, and a template counts only when every way uses
it. Each is checked as a rule's annotations are: an annotation it shows
(`.Annotations.summary`, `description`, `runbook_url`, or
`.CommonAnnotations.`) that the rule hasn't, as a fact; a label it names
(`.Labels.x`, `.CommonLabels.x`, `.GroupLabels.x`) that no firing had, as
measured, or that the expression can't give, as a guess. Under an `if` or a
`with` naming it, nothing is faulted. A rule whose routing would take
more than its share of the check's work or time isn't checked against
templates, and its page says why. Nothing is written to Grafana.

`lapilli serve` checks the rules as it read them last, each `--watch`, so a
new summary shows at once without making a new version. Each rule's page has
"What it says" (its summary, description and runbook, escaped, the runbook a
link only when it is an http or https URL with nothing a template fills in)
with the problems found, facts first and guesses apart, each with why it is
one; the rules list marks a rule with a problem of fact;
`lapilli_rule_content_problems{rule, check}` in `/metrics` counts the facts
and what was measured, not the guesses, so a team can alert on a rule left
with no runbook; and MCP's `rule` carries what a rule says and its problems,
`rules` how many each has. What is measured comes from the ledger: the
labels a rule's alerts had over twelve weeks, and whether it was noisy over
four. Prometheus is asked for its scrape intervals once a day, and Grafana
for its notification templates once an hour.
`--check-content=false` turns it off, and so does the chart's
`serve.checkContent.enabled`; `serve.checkContent.severities` and
`scrapeInterval` give the rest.

## Before a change is merged

`--compare` sets the rules proposed beside the rules in place: both are
replayed over the same period and judged against the same incidents, and
the report shows what the change does. Each rule that differs gets its
stones and severity before → after, firings are counted by severity, and
each incident caught differently is listed with both catches. Rules alike
in both are replayed once, for both; `--changed-only` doesn't replay them
at all, which is much faster, but then an incident shows only the changed
rules' catches.

```
$ lapilli verdict --rules proposed/ --compare alert-rules.yaml --prometheus http://prometheus:9090 \
    --incidents incidents.yaml
Lapilli  2026-10-26 00:00 → 2026-11-02 00:00 Asia/Seoul
Rules compared: 1 changed, 1 added, 0 taken away, 42 the same
Firings 148 → 42
By severity: critical 125 → 16, warning 23 → 26
Of 3 incidents: with no alert 1 → 0

RULE           SEVERITY     FIRED    ○ CAUGHT  ● NOTHING  PRECISION  REFIRED
ExportJob5xx   critical     118 → 9  0 → 0     118 → 9    0% → 0%    41 → 2
DiskFillingUp  – → warning  – → 3    – → 1     – → 2      – → 33%    – → 0

INCIDENT            START
INC-43 Batch stuck  2026-10-31 02:40
  before  ✗ nothing fired
  after   DiskFillingUp +4m
```

In CI, check out the rules as they are beside the rules proposed and post
the report on the pull request. With GitHub Actions:

```yaml
on:
  pull_request:
    paths: ['alerts/**']
permissions:
  contents: read
  pull-requests: write
jobs:
  verdict:
    runs-on: ubuntu-latest # one that can reach your Prometheus
    steps:
      - uses: actions/checkout@v4
        with: {fetch-depth: 2}
      # The merge's first parent: the rules as they are, if any.
      - run: git worktree add base HEAD^1 && mkdir -p base/alerts
      - uses: actions/setup-go@v5
        with: {go-version: stable}
      - run: go install lapilli.dev/cmd/lapilli@v0.4.0
      - run: |
          lapilli verdict --rules alerts --compare base/alerts --since 14d \
            --prometheus "$PROMETHEUS" --incidents incidents.yaml > verdict.txt
        env:
          PROMETHEUS: ${{ vars.PROMETHEUS_URL }}
      - if: ${{ !cancelled() }}
        run: |
          { echo '```'; cat verdict.txt; echo '```'; } > comment.md
          gh pr comment "$PR" --body-file comment.md --edit-last --create-if-none
        env:
          PR: ${{ github.event.pull_request.number }}
          GH_TOKEN: ${{ github.token }}
```

Rules left alone are replayed too, once, so an incident one of them catches is
caught on both sides: a longer `--since` judges against more incidents, and
takes longer. For a
cloud's managed Prometheus, sign in first with the cloud's own step
(`aws-actions/configure-aws-credentials`, `azure/login` or
`google-github-actions/auth`, through OpenID Connect with `id-token: write`)
and add `--auth`. Any CI does the same: check out both, run, post the text.

The rules in a pull request become queries to your Prometheus, sent with the
job's credentials, and their stones show in the comment; and a pull request
can change the workflow it runs. Run this where only people who may query
your Prometheus open pull requests: a private repository, or one that needs
approval before running workflows from outside contributors, above all on a
runner inside your network.

## Incidents kept elsewhere

The thinner the incident log, the worse every verdict, so copy it from where
it is kept. [recipes/incidents](recipes/incidents) has a small script for each
of PagerDuty, incident.io, Jira, GitHub issues, Grafana IRM, Rootly,
FireHydrant and Compass's alerts (for Opsgenie's users, whose incidents move to
Jira Service Management, which the Jira one reads). Each writes what
`--incidents` takes, so it works with both commands:

```sh
PAGERDUTY_TOKEN_FILE=pd-token SINCE=2026-10-01 recipes/incidents/pagerduty.sh > incidents.tmp &&
  mv incidents.tmp incidents.yaml
lapilli verdict --rules alert-rules.yaml --prometheus http://prometheus:9090 --incidents incidents.yaml
```

Move the file into place: an incidents file with nothing in it is refused, so
that a script that failed doesn't read as a week with no incidents. Write
`incidents: []` for that.

`lapilli serve` can also take them as they come. With `--incidents-listen ADDR`
and `--incidents-token-file FILE`, it opens a port that takes incidents from
whoever holds the token, and does nothing else. A job can run this every hour:
an incident written again as it was changes nothing, tells no one, and judges
nothing again. Unlike a file of the last month, it forgets nothing older.

```sh
recipes/incidents/pagerduty.sh > incidents.json &&
  LAPILLI_TOKEN_FILE=token recipes/incidents/put.sh http://lapilli:8082 < incidents.json
```

The port is its own so that a NetworkPolicy can let the job reach it alone. On
the UI's port a pod could name itself anyone, in the headers the sign-in proxy
sets; here those headers mean nothing. What the job copies in is the tool's: the
UI can't change it, and the token can't change what people wrote. It isn't
counted as anyone's use of Lapilli, and isn't told to the notice webhook, since the
tool has told people already. The port takes one write at a time, 20 a second,
and the ledger keeps 20,000 copied incidents at most.

## Incidents nobody wrote down

The incidents a team writes down are the ones it noticed; those it didn't
are the ones no alert caught. `--impact` finds them in a signal of user
impact. A per-service error ratio from what is in front of your services
works well, with `--match service` and labels renamed to `service` where
needed (`label_replace`):

| In front | `--impact` |
|---|---|
| Istio | `sum by (destination_service_name) (rate(istio_requests_total{reporter="destination", response_code=~"5.."}[5m])) / sum by (destination_service_name) (rate(istio_requests_total{reporter="destination"}[5m])) > 0.01` |
| Linkerd | `sum by (deployment) (rate(response_total{direction="inbound", classification="failure"}[5m])) / sum by (deployment) (rate(response_total{direction="inbound"}[5m])) > 0.01` |
| Envoy, Envoy Gateway | `sum by (envoy_cluster_name) (rate(envoy_cluster_upstream_rq_xx{envoy_response_code_class="5"}[5m])) / sum by (envoy_cluster_name) (rate(envoy_cluster_upstream_rq_xx[5m])) > 0.01` |
| ingress-nginx | `sum by (ingress) (rate(nginx_ingress_controller_requests{status=~"5.."}[5m])) / sum by (ingress) (rate(nginx_ingress_controller_requests[5m])) > 0.01` |
| Traefik | `sum by (service) (rate(traefik_service_requests_total{code=~"5.."}[5m])) / sum by (service) (rate(traefik_service_requests_total[5m])) > 0.01` |
| OpenTelemetry HTTP server metrics | `sum by (service_name) (rate(http_server_request_duration_seconds_count{http_response_status_code=~"5.."}[5m])) / sum by (service_name) (rate(http_server_request_duration_seconds_count[5m])) > 0.01` |

Label names depend on how each is scraped (Linkerd's `deployment`, an
OpenTelemetry service's `service_name` or `job`): check them against your
own Prometheus first. Pick the threshold your users would notice, and
`--impact-for` long enough that a blip isn't an incident.

## Every week

The Helm chart runs `lapilli verdict` as a CronJob, by default on Mondays at
09:00 over the week before, and sends the report to Slack and the metrics to
a Pushgateway:

```sh
helm install lapilli oci://ghcr.io/lapilli-project/charts/lapilli -n monitoring \
  --set prometheus.url=http://prometheus:9090 \
  --set 'rules.configMaps={alert-rules}' \
  --set impact='sum by (service) (rate(http_requests_total{code=~"5.."}[5m])) / sum by (service) (rate(http_requests_total[5m])) > 0.01' \
  --set 'match={service}' \
  --set report.slackWebhookSecret.name=lapilli-slack \
  --set report.pushgateway=http://pushgateway:9091
```

Install it in the namespace of the ConfigMaps holding your rules: each is
mounted, and every key with alert rules in it is read. Or replay the rules
where they run instead: `rules.loaded` reads those the API at
`prometheus.url` has loaded, and `rules.grafana.url` those a Grafana holds,
with a service account token from `rules.grafana.tokenSecret`. Put the Slack incoming
webhook's URL in the Secret (`kubectl create secret generic lapilli-slack
--from-literal=url=https://hooks.slack.com/services/...`); lapilli reads it
from the file and never shows it.

For a cloud's managed Prometheus, set `prometheus.auth` and give the pod
read access through the cluster's workload identity: on EKS, an IAM role
through IRSA (`serviceAccount.annotations`) or EKS Pod Identity; on AKS,
Azure Workload Identity (`podLabels` and `serviceAccount.annotations`); on
GKE, Workload Identity (`serviceAccount.annotations`). See
[values.yaml](charts/lapilli/values.yaml) for these and the rest.

Without Kubernetes, the same flags work from cron: `--push URL` puts the
metrics on a Pushgateway, and `--slack-webhook-file FILE` posts the report.
The image is `ghcr.io/lapilli-project/lapilli`.

The weekly run replays the rules as they are when it runs; a rule changed
during the week is judged as it is now. `lapilli serve` doesn't: it records
each version as it changes. It also sends reports on schedules people set
themselves, in its UI: each morning over yesterday, each month, at a team's own
nine o'clock (see [Keep judging](#keep-judging)).

## Keep judging

`lapilli serve` keeps a ledger. Every hour it replays the rules over the
hours since its last run, and the hour before that again for points that came
late, and records the firings and their stones. It reads the rules every
minute and records each change as a new version, so every hour is replayed
with the rules as they stood then, and it replays a change over the week
before it to show what the change would have done. Once a week it judges the
week before, works out longer `for`s for the noisy rules and the rules that
nearly caught what nothing caught, and posts the report to Slack. An incident
written down after that, or in the week under way, has the rules that nearly
caught it worked out within the hour.

```sh
lapilli serve --rules alert-rules/ --prometheus http://prometheus:9090 --match service \
  --impact 'sum by (service) (rate(http_requests_total{code=~"5.."}[5m])) / sum by (service) (rate(http_requests_total[5m])) > 0.01'
# then http://localhost:8080
```

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/serve-dashboard-dark.png">
  <img alt="The lapilli serve dashboard: last week's precision, firings, incidents missed, time to catch and noisy rules, then firings and precision by week" src="assets/serve-dashboard.png" width="820">
</picture>

A web UI opens on a dashboard of the last twelve weeks: last week's
precision, firings, incidents missed, time to catch, noisy rules and the
firings that woke people for nothing beside the week before; firings, precision and incidents by week; where the noise
is, rule by week; and what to look at first (a longer `for` that would quiet
a rule, an incident nothing caught, impact windows to decide on). Beyond it,
each rule over twelve weeks with its versions, the incidents and what caught
them, and each day's firings. People
write incidents down there, confirm or dismiss the windows `--impact` found,
or say a black stone was real; the stones are given again at once, for the
whole ledger, without asking Prometheus. The settings that only judge the
ledger again (`--lead`, `--match`, `--refire`, the noisy thresholds) can be
changed there and tried on the last week first; `/settings.yaml` gives the
changes as Helm values to commit. The API gives `lapilli verdict --json`'s
report for any period (`/api/v1/report?from=&to=`) and takes incidents
(`/api/v1/incidents`); `/metrics` gives `--metrics` over the last seven days,
so Prometheus scrapes it instead of a Pushgateway, and the chart has a
Grafana dashboard of them.

The ledger is one SQLite file (`--ledger DIR`). It keeps each firing's stones
and each rule's by day and by week, so reading twelve weeks costs what
reading one does, and a change gives stones again only to the firings near
it. Measured with two million firings (a thousand an hour for twelve
weeks): an hourly run 0.18 s, writing an incident 52 ms, twelve weeks by
rule 2 ms, judging everything again after a change to the settings 15 s;
the pages, worked out afresh over 200,000 firings, 1 to 21 ms.

It isn't only to look at. Teams own rules (by a note, a name pattern, or
labels, in that order: a team that owns by labels can't take a rule another
team's pattern names) and incidents, so the dashboard, the rules and the tasks can be shown for
one team. A rule keeps a note: its team, whether to keep, tune or retire
it, its runbook. A quiet window (a deploy, maintenance, a load test)
excuses the firings in it that caught nothing, so expected noise doesn't
weigh on a rule's precision. A suggestion on the dashboard becomes a task
with a press, with a team, an assignee, a due date, a status and a link
to the issue in your tracker, and rules, incidents and tasks take
comments. A quiet window can repeat every day or every week, for a
nightly batch or a weekly release; every month, on a day (`15`), the last
(`last`) or a weekday of the month (`first sat`), for maintenance; or by a
cron expression (`0 2 * * 6`), hourly at most. Its start and end are its
first repeat, which must be one of its schedule's times, and each repeat is
shorter than the time between two; daylight saving is counted on the
calendar, as the reports' is. A quiet window can also be a team's: it
excuses only the firings of rules the team owns, and its page names the
team of each firing it excused. A team can keep its own working hours,
hours asleep and time zone, or its shifts (on the teams page, or
`work_hours`, `sleep_hours`, `tz` and `shifts` in the teams API): its rules'
firings that caught nothing are counted asleep and off hours by those, every
other rule's by `--work-hours`, `--sleep-hours`, `--tz` and `--shifts`. All of it is in the same ledger,
through the UI or the API; see [docs/design/work.md](docs/design/work.md).

The weekly digest can go where each team reads it. `--team-digest TEAM=FILE`
posts a team its own rules and incidents, to the Slack-style incoming webhook
whose URL is in the file, and a team nothing happened to hears nothing.
`--notify-webhook-file` gets `digest.ready`, for everyone and for each team, with
the numbers as JSON and the report as `text`, for any chat or script to pass on.
`text` is the report as written, from rule names and incident titles others
wrote, so escape it for where it goes; `slack_text` is it as the Slack digest
posts it, with mentions and links escaped. The other fields that name things
(`noisy_rules`, `spare_rules`, `sources`, `notifications.most_for_nothing`,
`notifications.by_receiver`) are as written too: escape them as well.
Over twelve weeks, the digest also names the rules that are spare.

Reports go on schedules people set themselves, on the Reports page or with
`/api/v1/reports`: `daily@09:00`, `weekly mon@09:00`, `monthly 1@09:00` (or
`last`), or a cron expression such as `0 9 * * 1-5`, hourly at most, in the
ledger's zone or the team's. A preset covers the day, the week from Monday or
the month before it; a cron expression, the time since the one before, or a
span given with it (`24h`, `7d`). A report is for everyone or one team, and
holds the sections picked: `summary` (beside the span before), `noise` (and
what turned noisy), `missed`, `woken`, `notifications`, `replay`, `sources`,
`tasks`, `content` and, for a week or longer, `suggestions`. It goes to
destinations by name, never a URL typed in a page: `--report-dest NAME=FILE`
(repeatable, a Slack-style incoming webhook; in the chart,
`serve.reports.destinations`), `slack`, `team:NAME`, and `webhook`, which gets
`report.ready` with each section's numbers as JSON and the text as `text` and
`slack_text`; the fields that name things are as written, so escape them as
`digest.ready`'s. Each report is sent once for each span, through the outbox,
when the ledger has the span, or with what there is, saying so, once it fell
due six hours ago or half the time to the one before, whichever is less;
after a restart, only the last one due, and only while less time has passed
since it fell due than between it and the one before. A time daylight saving
skips falls due when the clocks jump; a repeated time, the first time.
Admins make and change reports; everyone sees them and a preview of each.
A team's editors, and editors, make a team's, sent to the team's own
destination (`team:NAME`).

The weekly digest is shown first among them. The flags define it until an
admin changes it on the Reports page or with `PUT /api/v1/reports/digest`
(`{"schedule": "weekly tue@10:00", "dests": ["slack", "ops"], "enabled":
true}`): from then on the ledger does, `--digest` isn't used (the log says
so at start), and the flags only name where it can go, now any destination,
a `--report-dest` too. It stays weekly, of the week from Monday, and its
suggestions are worked out at its time whether it is sent or not. A week
sent isn't sent again, before or after the change, to old destinations or
new; a week that falls due while it is off is passed over; and moving its
day never sends an older week. `DELETE /api/v1/reports/digest`, or "Back to
the flags", gives it back to the flags; see
[docs/design/reports.md](docs/design/reports.md) and
[docs/design/serveops.md](docs/design/serveops.md).

What happens (a task made, assigned, done or overdue, an incident written
down, a quiet window kept, a comment) can be told to a webhook, as JSON,
with `--notify-webhook-file`, and to Slack, as a line in `--lang`, with
`--notify-slack`. A tracker, a chat or a script takes the webhook. To keep
tasks in step with issues in GitHub, GitLab, Jira or Linear, run the
tracker's script in [recipes/trackers](recipes/trackers) every few minutes
with `--tasks-token-file` (below). Notices are written in the
same transaction as what they tell of and sent after, so a receiver that
is down holds nothing up and misses nothing it comes back for.

Whether Lapilli itself earns its keep is on the outcomes page, worked out
from the same ledger. Each change to the rules it reviewed is measured: the
replay of the week before was its prediction, and the four weeks either
side are what came of it, as firings a week, incidents caught, firings
fewer since than at the rule's rate before (a rule written counts as
more), and whether it came out as predicted. Beside them: the weeks before
`lapilli serve` began and the latest (precision, firings and ● a week,
incidents missed); whether the record is kept up (two weeks without an
incident written down is flagged, since every firing then reads as ●;
impact windows to decide; failed runs; rules not replayed); and whether it
is used (edits and by how many people,
suggestions acted on, tasks done). The overview sums it up, the digest has
a line of it, `/api/v1/outcomes` has it as JSON, and `/metrics` has
`lapilli_serve_firings_avoided`, `lapilli_serve_rule_changes`,
`lapilli_serve_incident_last_timestamp_seconds` and `lapilli_serve_edits`,
so an alert can say when the record goes quiet. Nothing is sent anywhere,
and reading a page isn't counted.

The UI is checked against WCAG 2.2 AA (axe-core over every page, and tests
for each page's structure, contrast and charts, whose numbers are in tables
too), and works at 320px and 200% zoom, with no script.

The UI speaks English, Spanish, Japanese and Korean: the browser's
language, or the one picked at the top of the page; `--lang` picks the
language of what `serve` posts to Slack and in its reports. Each language is
one file of translations, `internal/serve/i18n/LANG.json`; a test fails if a
text has none, so a new language can't be left half done. The Spanish and
Japanese were written with a machine's help, in the plain words SREs use, and
want a native speaker's review: corrections are welcome as pull requests.

It listens on `127.0.0.1:8080`. To serve it at a URL people open, sign them
in, one way of three ([docs/design/signin.md](docs/design/signin.md)):

- `--oidc-issuer`, `--oidc-client-id` and `--oidc-client-secret-file`:
  lapilli signs people in itself with any OpenID Connect provider, as
  Grafana and Argo CD do, and keeps a session in a signed cookie;
  `POST /auth/logout` ends it. Register `--external-url`'s `/auth/callback`
  as the client's redirect URI. An API client or an agent sends an ID token
  the provider gave this client as a bearer token.
- `--proxy-jwt google-iap`, `aws-alb` or `jwks`: the JWT a sign-in in front
  signs (Google Cloud's Identity-Aware Proxy, an AWS Application Load
  Balancer's sign-in, oauth2-proxy and the rest), checked against its keys
  on every request.
- `--user-header`: the plain header a proxy that signs people in sets, so
  changes are recorded under it; the proxy must then be the only way in.

With the first two, every page and API call but the health checks needs
someone signed in, and `--user-claim` (`email`) and `--groups-claim`
(`groups`) name the person and their groups. `--read-only` takes no
changes. With `--editors` and `--admins`, people or groups
(`--groups-header`, `--groups-claim`), only editors may write and only
admins may change the settings and the teams; the rest may only look.
`*` is everyone signed in. Write `user:NAME` or `group:NAME` where someone
could pick a name that is a group's.

With no roles, who may change what depends on where it listens. On
loopback alone (`127.0.0.1`, `localhost`, the default), everyone who
reaches it is an admin: that is this machine's users. Beyond it (`:8080`,
as the chart listens, a pod's address), everyone may only look, since every
pod or machine that shares its network can reach it; give roles behind the
proxy, or `--everyone-admin` to let everyone change anything anyway. The
log says at start which applies. (Before 0.28.1, everyone was an admin
wherever it listened.) With `--oidc-issuer` or `--proxy-jwt`, loopback
makes no one an admin: everyone signed in may only look until roles say
otherwise, and `--everyone-admin` is refused.

With `--admins`, admins can also give roles on the Roles page (from the
Teams page) or with `/api/v1/roles`, kept in the ledger: an editor's, of
every team or of one, to `user:NAME` or `group:NAME`. A team's editors
change that team's tasks, quiet windows and reports, notes on the rules it
owns, and comments on what is its, and nothing of another team's or of
everyone's; the ledger checks each in the write. The UI never makes an
admin or an approver, and can't take away a role the flags give: those stay
`--admins`, `--editors` and `--approvers`. Only admins see who holds a
role. A team removed takes its editors with it.

With `--user-header`, the proxy must be the only way in. The chart's
`serve.networkPolicy` lets in only the proxy (or the ingress controller,
the Gateway, the load balancer in front), and Prometheus only to
`/metrics` on a port of its own (`--metrics-listen`). With
`serve.userHeader`, the chart refuses to go without it. The chart serves
the UI at a URL with `serve.service` (a LoadBalancer or NodePort),
`serve.ingress` or `serve.httpRoute` (Gateway API), and `serve.signIn`;
its README has each cloud's load balancer.

Posts from other sites are refused, and the pages run no scripts. Only host
names it is reached by are answered, so a page elsewhere can't point a name
of its own at it (DNS rebinding):

- an address, `localhost`, and names under `.svc`, `.cluster.local` or
  `.internal`;
- the name in `--external-url`;
- what `--allowed-hosts` adds, such as the name a proxy passes on.

Others get 421.

In the chart, `mode: serve` runs it as one replica with the ledger on a
PersistentVolumeClaim, behind a ClusterIP Service:

```sh
helm install lapilli oci://ghcr.io/lapilli-project/charts/lapilli -n monitoring \
  --set mode=serve --set prometheus.url=http://prometheus:9090 \
  --set 'rules.configMaps={alert-rules}' --set 'match={service}'
kubectl -n monitoring port-forward svc/lapilli 8080
```

### Firings recorded elsewhere

A cloud's own alarms that aren't written in PromQL can't be replayed, but
what they fired can be kept and judged with the rest. With
`--firings-token-file FILE`, the port `--incidents-listen` opens takes
firings too, from a job holding that token, its own: a token that may copy
incidents can't write firings, and the reverse, and neither may be the
notifications token, which alert managers keep in webhook URLs.

```sh
export SINCE=2026-11-08T00:00:00Z UNTIL=2026-11-09T00:00:00Z
recipes/firings/cloudwatch.sh > firings.jsonl &&
  LAPILLI_TOKEN_FILE=firings-token recipes/firings/put.sh http://lapilli:8082 cloudwatch-prod < firings.jsonl
```

`put.sh` sends them as `PUT /api/v1/firings/SOURCE?from=&to=`, one a line
as `--firings` reads them. The source is a name the job picks
(`cloudwatch-prod`, `azure-shared`), and the period replaces: the source's
firings that start in `[from, to)` are those sent, the way a run replaces
the firings that start in its period. Sent again as they are, nothing
changes and nothing is judged again, so a job can send the last day every
hour, and everything the cloud keeps every week, which ends the alarms that
were still on. A firing without an end is still on until one is sent with
its end. A write is 31 days, 16 MB and 100,000 firings at most, nothing in
it starts or ends more than an hour ahead of lapilli's clock, and names have
no character that doesn't show. The ledger keeps 50 sources, 5,000 rules
from each and 20,000 from all, and 500,000 alerts from each; alerts with no
firing left go with `--keep`. The token writes twenty periods at once, for a
first copy of what the cloud keeps, then one each five seconds. A write
judges again as far as a period past its period: an alert's next firing
after one that ran on longer is judged again when a write or a run reaches
it.

Such a rule is named as its source names it, and told apart when another
rule has the name (`checkout-5xx [cloudwatch-prod]`); a replayed rule that
comes later takes the name. Its firings are judged as any are, beside the
replayed rules: stones, precision, what it adds, incidents written down
later, teams, hours, quiet windows, tasks, the digests, `/metrics` and the
agent's reading tools. What needs a rule's condition stays with the
replayed ones: change reviews, longer `for`s, near misses, `compare`. Its
page says where it was recorded and why. The runs page lists the sources,
`/api/v1/sources` has them, and `/metrics` has
`lapilli_serve_source_last_timestamp_seconds{source}`, so an alert can say
when a job stopped copying. Lapilli says so itself, too: a source nothing has
been copied in from for `--source-quiet` (26 hours unless given; give a
weekly job's 8d) is told of once, to the webhook and, with `--notify-slack`,
to Slack. The weekly digest says which sources firings were copied in from,
how many each brought that week, and which has gone quiet.

With the firings token, `--rules` and `--prometheus` are optional: a team
with only cloud alarms runs `lapilli serve --incidents-listen :8082
--firings-token-file FILE`, nothing is replayed, and what comes in is
judged as it comes. `--impact` still needs `--prometheus`. In the chart,
`serve.firings.tokenSecret` names the Secret.

`lapilli serve -h` lists the rest: how often to run (`--every`), how far to
replay on the first start (`--backfill`, 28 days) and how long to keep
firings (`--keep`, 400 days).

### Tasks kept in step with a tracker

A task says who does what by when; the work is tracked where the team tracks
the rest. With `--tasks-token-file FILE`, the port `--incidents-listen` opens
lets a job holding that token, its own, read tasks and set their link and
status, and nothing else. [recipes/trackers](recipes/trackers) has a script
for each of GitHub, GitLab, Jira, Linear, Azure Boards and Shortcut, alike: each run makes an issue
for each open task without one, with a link back to the task's page, and
puts the issue's URL on the task; then a task whose issue was closed is
closed, `done` or `dropped` as the issue was, and one closed that way is
opened again with its issue. With `CLOSE_ISSUES=true`, an issue whose task
someone closed in Lapilli is closed too, unless the issue changed since.
Only issues the script's own account made for a task are taken for it,
followed or closed, by the marker line the script ends them with: anyone
may write a task's marker in an issue, or in a task's text, and an editor
may link a task to any issue. Who closed a task, and when, comes from who
set its status (`status_by`, below), not from whoever last edited it. A
task the tracker refuses is said and skipped, and the run fails once the
rest are done; one whose issue was deleted is said and left as it is. A
GitHub App's or Actions' token can't say who it is: set `GITHUB_LOGIN`
(`NAME[bot]`). With `COMMENTS=true`, comments are kept in step too, both
ways, each once: people's comments on a task are posted to its issue,
quoted, and the issue's comments copied to the task, shown as the
tracker's, with the token's `GET` and `POST /api/v1/tasks/{id}/comments`;
an edit after that isn't followed.

```sh
GITHUB_REPO=example/alerts GITHUB_TOKEN_FILE=gh-token \
  LAPILLI_TOKEN_FILE=tasks-token recipes/trackers/github.sh http://lapilli:8082
```

The job reads tasks with `GET /api/v1/tasks?linked=&status=&updated_after=&after=&limit=`
(`updated_after`, an RFC 3339 time, leaves out the tasks not changed since),
each with `status_by` and `status_at`: who set the status it has, and when,
as the changes kept say (a later edit of anything else doesn't count;
neither is given once the change that set it is pruned). A
link is an `https` URL of 2,048 characters at most, set once; the page
shows it with `rel="noopener noreferrer"`. The token can't make, delete or
retitle a task, read anything else, or hold the same value as another token.
What it writes is noted as `tasks-token` and isn't counted as anyone's use.
In the chart, `serve.trackers` names its Secret and, with `tracker`, runs the
script as a CronJob.

## Notifications as they were sent

A rule's firings can be a poor stand-in for its pages. The alert manager
groups a rule's alerts into one page, waits out the ones that resolve
quickly, pages again every `repeat_interval` while one stays firing, and
silences, inhibits or routes others to no one. So `lapilli serve` can keep
the notifications as they were sent, beside the firings, and say how many
each rule sent, how many were repeats, how many came while people slept, and
how many were for firings that caught nothing.

With `--notify-listen ADDR` it opens a port that takes notifications, and
nothing else, each source on a path of its own:

| Where the rules run | Send to | Proved by |
|---|---|---|
| Prometheus, Thanos, Mimir, VictoriaMetrics, with an Alertmanager (on any cloud, OKE among them); Google Cloud Managed Service for Prometheus's managed Alertmanager | `/v1/alertmanager`, a `webhook_config` | the token in `--notify-token-file`, as `http_config.authorization`'s bearer token |
| An Alertmanager of 0.33.0 or later, its event recorder (`--enable-feature=event-recorder`): every delivery, PagerDuty's, Slack's and email's too, to `RECEIVER/INTEGRATION`; alerts a silence or an inhibition muted aren't counted as delivered | `/v1/alertmanager/events`, an `event_recorder` `webhook_outputs` entry (`batch: true` is fine). Drop a webhook receiver that sends to Lapilli, or name it in `?skip=NAME`: the recorder tells of its deliveries too, and each would count twice | as above |
| Grafana Alerting | `/v1/grafana`, a webhook contact point | the token as its Authorization header, or its HMAC signature with the secret in `--notify-grafana-secret-file` (timestamp header `X-Grafana-Alerting-Timestamp`, five minutes at most) |
| Amazon Managed Service for Prometheus | `/v1/sns`, an HTTPS subscription to the SNS topic its alert manager sends to, with AWS's JSON template for SNS | SNS's signature, from a topic `--notify-sns` names; the subscription is confirmed by fetching its `SubscribeURL`, from an SNS host only |
| Azure Monitor managed service for Prometheus | `/v1/azure`, an action group's webhook with the common alert schema | the token as the password in the URL: `https://lapilli:TOKEN@host/v1/azure` |
| Google Cloud Monitoring policies with a PromQL condition | `/v1/google`, a webhook channel with basic auth | the token as the password, sent once asked with a 401. Its webhooks reach public endpoints only |

A cloud's own alarms, those not written in PromQL whose firings are copied
in ([Firings recorded elsewhere](#firings-recorded-elsewhere)), are told of
on the same paths, every cloud alike:

| A cloud's alarm | Send to | Proved by |
|---|---|---|
| Amazon CloudWatch, a metric, composite or log alarm, or one on several series (each contributor's message) | `/v1/sns`, the SNS topic its alarm actions send to, directly or through EventBridge's "CloudWatch Alarm State Change" | as above; keep SNS's raw message delivery off |
| Azure Monitor, a metric, log search or activity log alert | `/v1/azure`, an action group's webhook with the common alert schema | as above |
| Google Cloud Monitoring, a policy on a threshold, an absence or a log match | `/v1/google`, a webhook channel with basic auth | as above |
| OCI Monitoring, an alarm | `/v1/oci/SOURCE`, an HTTPS (Custom URL) subscription to the alarm's Notifications topic | the token as the password in the subscription's URL: `https://lapilli:TOKEN@host/v1/oci/oci-prod`, answered 401 without it. Its subscriptions reach public endpoints only |

The URL says which source the alarm's firings are copied in as:
`?source=cloudwatch-prod`, or the path for OCI, whose subscriptions take no
query. An alert is then tied to that source's rule with the name the alarm
sent, and its severity, though the ledger names the rule apart
(`disk-full [cloudwatch-prod]`); an OCI alarm's, to the rule whose firings
carry its OCID as `alarm_id`, as `recipes/firings/oci.jq` labels them,
whatever its message's title, and whenever its firing is copied in: the
name is worked out when read. A message of an alarm resolving, or of OCI's
repeating, doesn't say when the alarm began: it takes the start of the last
notification of its alarm (or contributor, or stream) with the same labels,
of the same kind of message, over the last month, and with none, is counted
and tied to no firing. An Azure resource and its group are compared without
their case. CloudWatch sends on a change of
state only, so it has no repeats; a move between `OK` and
`INSUFFICIENT_DATA` is of no firing, and isn't kept. OCI's `REPEAT` is a
repeat, and `FIRING_TO_OK` and `RESET` resolve.

OCI Notifications signs its messages, but Oracle doesn't document what is
signed or where its certificates come from, so lapilli takes the token
instead. A new subscription is confirmed from the URL it is sent with,
only from a request with the token, only over https to a Notifications API
host (`notification.REGION.oraclecloud.com`, in any of Oracle's realms),
and only to an address on the internet, not one on this machine or a private
network (as for SNS's certificates; through a proxy, when the environment
names one), following no redirect, after the answer, each URL once; any other is left
in the log, by its host, for someone to confirm with `oci ons subscription
confirm`. Notifications gives an endpoint five seconds, so `/v1/oci` waits
two for its turn, then answers 503, which Notifications tries again. A
message tried again is kept once, by its alarm and its evaluation.

```yaml
# Alertmanager
receivers:
  - name: oncall
    webhook_configs:
      - url: http://lapilli:8083/v1/alertmanager
        max_alerts: 1000
        http_config:
          authorization:
            credentials_file: /etc/alertmanager/lapilli-token
```

A notification's alert is tied to its rule's firing whose labels are all
among the alert's (which carries external labels the replay doesn't), and
which started within two evaluations of it; one that ties to none is kept and
counted as such. That is worked out when read, since a notification comes
before the run that finds its firing. Alertmanager says why it sent each
notification since 0.32.0, and Google Cloud says which are repeats; for the
others, a notification with the same alerts as the last one in its group,
nothing new and nothing resolved, is a repeat. One with the same group,
receiver and alerts as another within a minute is the same one, sent by
another replica. Notifications don't change the stones.

Each rule's page then says how many notifications it sent over twelve weeks,
how many were repeats, how many came while people slept, how many were for
firings that caught nothing, and to which receivers; each firing, how many
told of it, or that it paged no one. The dashboard's "Woken for nothing"
counts notifications instead of firings, and says so. `/metrics` has
`lapilli_rule_notifications{rule, receiver, reason}` over seven days; MCP's
`rules` and `rule` carry the same, and the weekly digest, to Slack and the
webhook, a line of it: how many were sent, repeats, those while people
slept, those for nothing, and the rules that sent the most for nothing; a
team's digest, for its rules. Notifications can't be replayed for history,
so each number counts from the first that came. In the chart:
`serve.notificationsAPI`.

The port takes 1 MB and 1,000 alerts a notification at most (Alertmanager's
`max_alerts` keeps it under), 20 a second from each source, one write at a
time, and 200,000 alerts a day (UTC) from each source, about 200 MB, so
one sending more can't keep another out; past that it answers that source
429 until the next day, and says so in the log once. Its
notifications are kept for `--keep`. It doesn't check the host name, as the
UI's port does: every request carries its own proof.

- A path the token proves is refused before its body is read. SNS's and
  Grafana's signatures are of the body, so a body from anyone is read only
  when its headers say it could prove itself (SNS's message type and a
  topic in `--notify-sns`, which must be the body's; Grafana's signature,
  with a secret to check it by), 64 such bodies at most at once, each
  within ten seconds; past that the port answers 503, which SNS sends
  again.
- A rule's name is 512 characters at most, a receiver's 200, and neither
  may hold a character that doesn't show (a control character, a
  zero-width space, a mark that turns text right to left); a group key
  longer than 1,000 characters, or with such a character, is kept as its
  hash; an alert carries 64 labels and 4 KB of them at most. A notification that
  doesn't keep to this is refused with 400, which an alert manager doesn't
  send again; one of a batch (the event recorder's) is passed over and the
  rest kept. Each is counted in
  `lapilli_serve_notifications_refused_total{source}`, and why is logged.
  `lapilli_rule_notifications` counts only the ledger's rules,
  and a rule's ten busiest receivers, the rest as `receiver="other"`.
- SNS: a message signed more than an hour ago (SNS gives up sooner) or
  ahead of the clock isn't taken. Its certificate must be on the SNS host
  of its topic's region, spelled as SNS spells it. A certificate that has
  verified a message is kept; when one can't be fetched, or too many have
  been lately, the port answers 503 rather than 403, so SNS sends the
  message again rather than drop it. A URL with no certificate behind it
  is fetched once and doesn't count against those: someone who knows a
  topic taken can still make lapilli ask SNS's host for certificates, one
  at a time, and if that host slowed lapilli down, a certificate SNS had
  just begun to sign with would wait for it. The same message again, by its
  MessageId within the hour, is answered as kept without a turn. A topic
  not in `--notify-sns` and a signature that doesn't hold get the same
  answer. Lapilli confirms a subscription with a GET of its `SubscribeURL`,
  which leaves `AuthenticateOnUnsubscribe` off: someone who sees the
  subscription's `UnsubscribeURL` can unsubscribe it, so keep the port's
  logs, and any proxy's, to those who run it.
- Grafana: set the contact point's timestamp header. With it, a signature
  holds five minutes either side of its time; the same signature again in
  that time is answered as kept. Without it, a signature holds as long as
  the secret does, and someone who saw a request could send it again
  later.

Annotations aren't kept. Who was on call, and whether they acknowledged, is
for PagerDuty, incident.io or Grafana IRM. Each path is tested against its
source's documented payloads, a cloud's own alarms' among them, with
stand-ins for SNS and for OCI's confirmation; none yet against a real
account.

## Changing rules

Lapilli reads by default, and changes only what a person approves
([docs/design/changes.md](docs/design/changes.md)). It proposes changes to
rules from what it finds: a longer `for` for a noisy rule, the threshold or
`for` that would have caught a near miss, retiring a spare rule or one that
never fired in twelve weeks. Each proposal is one rule, with the change
written as where the rule is kept expects it, and its evidence: the change
backtested over the twelve weeks to now (firings, incidents caught, lost or
caught later), or, for retiring, what the rule did, with the caveat that a
rule may wait for a rare incident. People propose from a rule's page too,
and agents over MCP. Proposals are on `/proposals`, on each rule's page,
and marked on the rules list; one can be made into a task. A rule has one
proposal waiting at a time, and a person, with their agents, makes twenty
an hour at most.

A fix to what a rule says, one annotation (`summary`, `description`,
`runbook_url` or `runbook`) set or taken out, is proposed the same way, with
the text written by a person or an agent: Lapilli writes none itself. The
rule's page offers one beside each finding of the content check that the
text can fix, a missing runbook, say, and the proposal carries the finding.
It doesn't change when the rule fires, so it has no backtest, and the rest
of the rule comes out as it was, byte for byte. The text must be a template
that parses in the rule's language, calling only the functions it has
(Prometheus's since 2.45; Grafana's, with its own) and never `query`:
Prometheus refuses a whole file for a function it hasn't.

Nothing is changed until a person approves it, on its page, in a browser:

- `--approvers` names who may (people and groups, as `--editors` does); the
  admins when it isn't given. `--changes` needs roles (`--editors` or
  `--admins`), as without them whoever may change anything is an admin,
  and someone to approve. `--approvals 2` asks for two people, of a proposal approved by
  one before it was raised too, unless it was carried out already. No one
  approves their own proposal, or one an agent made for them, in any letter
  case.
- The form is signed for the person and the proposal as it stood, and comes
  from the page itself (`Sec-Fetch-Site: same-origin`). The API, MCP and
  every token can't approve.

`--changes` turns carrying them out on; without it Lapilli only proposes.
With it, each approved change is checked again first: the rule is read
again and refused if it changed, the backtest is run again and refused if
it says something worse than what was approved (an incident caught that
would now be missed), and the rule as it was is kept for undo. Then:

- **A rule in git** is handed to a recipe in
  [recipes/changes](recipes/changes/README.md), which runs where the
  repository's credentials are and opens a pull request: GitHub, GitLab,
  Bitbucket, Gitea or Forgejo, or Azure DevOps. The recipe reaches
  `--incidents-listen` with the token in `--changes-token-file`, which can
  read the changes approved and say what became of each pull request, and
  nothing else. The recipe takes as the change's only a pull request it
  opened: from the repository itself, into the base branch, by its own
  account, or, when the host refuses another, the one open from Lapilli's
  branch there; never one from a fork. The change is written into the file as
  it was read, comments, order and quoting kept. A merge isn't the change
  applied: it is once Lapilli reads the rules with it, and the outcomes
  page measures it from then.
- **A rule kept only in Grafana** is changed through its provisioning API
  with the token in `--changes-grafana-token-file` (a service account that
  may write the rules Lapilli may change). One Grafana keeps from files, or
  another tool keeps through its API, is refused. The rule stays editable in
  Grafana's UI. What became of the change is read back from Grafana, not
  taken from its answer; until it can be, the change is kept as applying,
  with the rule as it was, and never sent again.
- **A rule kept in a managed Prometheus ruler**, read with `--ruler`, is
  changed through the ruler's own API, and only in the namespaces (Azure:
  the rule groups, `RG/NAME`) `--changes-in` names, a name or a prefix
  ending in `*`: a ruler can't say which of its rules a tool keeps, so the
  operator says which it doesn't. Name none that mimirtool, Terraform or
  Alloy's `mimir.rules.kubernetes` syncs. An Amazon namespace or an Azure
  rule group tagged as another tool's (CloudFormation's own tag, or one
  `--changes-managed KEY=VALUE` names) is for a person. What a write
  replaces, a Mimir or Cortex group, an Amazon namespace or an Azure rule
  group, is read again just before: the rule itself changed since is
  refused as stale; only other rules in it changed, the change is made
  afresh, theirs as they now are. It is kept for undo, then written with
  the write credential of its kind (`--changes-ruler-token-file`,
  `--changes-aws-role`, `--changes-azure-client-id`), and read back: an
  Amazon namespace stays applying while it is `UPDATING`. A group goes
  with its last rule; an Amazon namespace's or an Azure rule group's last
  rule is for a person.
- **A cloud's own alarm**, read with `--alarms`, is changed through its
  cloud's API, and only where `--changes-in` names it as `SOURCE:NAME`:
  CloudWatch's alarm name, Azure's `RG/NAME` (in any letter case), Google
  Cloud's policy's display name, OCI's display name, or a prefix ending in
  `*`. One tagged as another tool's (CloudFormation's own tag, Config
  Connector's or Terraform's label, or one `--changes-managed` names, an
  OCI defined tag as `NAMESPACE.KEY=VALUE`) is for a person; name none
  that CloudFormation, Terraform, Bicep, Pulumi or Config Connector keeps.
  A `for` and a threshold where the alarm is PromQL (CloudWatch's PromQL
  alarms, Azure's PromQL metric alerts, Google's PromQL conditions),
  backtested; what it says on every cloud, a runbook its own form (in a
  cloud that keeps one description, the first link in it, or a line of its
  own); turning it off on every cloud. What is written is the least each
  API takes: CloudWatch's whole alarm as read with the change
  (`PutMetricAlarm`), its actions alone off and on
  (`DisableAlarmActions`, `EnableAlarmActions`); Azure's properties
  changed (`PATCH`; an activity log alert's description, a `PUT` of the
  whole rule), with `If-Match` for a log search alert; Google's fields in
  `updateMask`; OCI's fields, with `if-match`. CloudWatch's alarms whose
  actions scale or act on an instance aren't changed at all, one that
  warms up isn't rewritten, a composite alarm is turned off and on alone,
  and one with a field this Lapilli's model doesn't have isn't rewritten;
  a Google policy of several conditions takes a condition's `for` and
  threshold alone. The alarm is read again just before, as a ruler's unit
  is, and CloudWatch's and Google's once more just before the write; it is
  written with its cloud's write identity (`--changes-aws-role`,
  `--changes-azure-client-id`, `--changes-google-service-account`,
  `--changes-oci-config`) and read back, a round later again where the
  cloud may be read a moment behind.
- **Anything else** (rules read with `--loaded-rules`, rendered from a chart,
  a cloud's alarms read from a file, a Grafana rule's threshold) gets no
  edit: the proposal's page has the diff and a pull request's description,
  for a person to make it where the rule is written.

Retiring a Grafana rule, an Azure one or a cloud's PromQL alarm turns it
off first; four weeks later Lapilli proposes deleting it, approved again,
and only if it is still off and nothing it would have caught was missed
meanwhile. It is never deleted outright. A cloud's alarm Lapilli can't
replay is retired as far as off, never deleted from here: its page says to
delete it in the console once it has been off a while. A rule file has no
off, nor so a Mimir, Cortex or Amazon ruler: there, retiring is the
deletion. Turning a rule off, or deleting it, waits while it fires, on
every path: while the ledger has an alert of it still on (a cloud's
alarm's, by its ID, whatever its history is named), or the cloud says it
fires: CloudWatch's `ALARM`, an Azure alert of it whose monitor condition
is `Fired`, an open Google alert of the policy, OCI's `FIRING`. Every applied change can be
undone from its page by an approver, the same way it was made; in a ruler
or a cloud, undo puts back the rule as it was kept, the rest as it is now.

At most five deletions a day and twenty changes an hour are carried out
(`--changes-limits`), each counted once, when it is handed out or sent; a
change past them waits. An admin can pause changes
in the settings, and so can whoever runs the pod, by making the file in
`--changes-paused` (the chart: a ConfigMap): nothing is carried out or
handed out while they are paused, and every page says so. A pull request
merged changed in review, its rule read as neither it was nor as
proposed, is `amended`, not applied. An admin closes, saying why, one
whose pull request is open or merged but never read back, and one sent to
Grafana or a ruler that can't be read back, once they say it didn't reach
it.
Every step is kept in the ledger's changes, and told as `proposal.made`,
`proposal.approved`, `change.opened`, `change.applied`, `change.amended`,
`change.refused`, `change.failed` and `change.undone` to the webhook and
Slack.

The git path, Grafana's and Mimir's run against real servers in
`test/e2e/changes.sh` (Gitea 28.3, Prometheus 3.5, Grafana 13.3, Mimir
3.2.4), and against stand-ins in the unit tests; Cortex's against a
stand-in at its paths, Amazon's and Azure's rulers, and the four clouds'
alarms, against stand-ins answering as their API documents show. None yet
against an account of any cloud.

## Ask it from an agent

`lapilli serve` speaks [MCP](https://modelcontextprotocol.io) at `/mcp`
(Streamable HTTP), so an agent can ask it what the pages show, and check a
change to the rules before anyone makes it. The agent can be Claude Code,
Cursor, VS Code, or one of your own:

```sh
kubectl -n monitoring port-forward svc/lapilli 8080
claude mcp add --transport http lapilli http://localhost:8080/mcp
```

`compare` is off until `--mcp-compare editors` (or `admins`) turns it on,
with roles set.

Six tools read what the pages show, each answer as JSON with a schema:

- `overview`
- `rules`
- `rule`: a rule's definition and history
- `incidents`
- `firings`: with their labels
- `outcomes`

The seventh, `compare`, is the one an agent can't do by itself. The agent
writes the changed rules as a Prometheus rule file. Lapilli replays them,
as they are and as proposed, against the metrics' history over the days
before (up to 14), and judges both against the ledger's incidents. A
comparison takes 20 rules at most and answers within 25 seconds. It
answers with how often each rule would have fired, and which incidents it
would have caught, sooner or later or not at all. The agent proposes and
Lapilli measures. Once the change is merged, `outcomes` says whether it
did what `compare` said.

Nothing an agent asks for changes anything: `compare` replays and keeps
nothing. With `--mcp-propose editors` (or `admins`), an eighth tool,
`propose`, keeps a change to one rule, or to what it says, as a proposal ([above](#changing-rules)),
marked as an agent's, for someone other than the person it acts for to
approve in the UI. No tool approves or applies one.

- **Access.** Agents come through the same sign-in as the pages (with
  `--oidc-issuer`, an ID token as a bearer token), and
  everyone gets the tools that read. `compare` runs PromQL the agent wrote
  with Lapilli's access to Prometheus, and, for a rule kept in Loki, LogQL
  the agent wrote, sent to the Loki (or through Grafana's data source
  proxy) with Lapilli's token, so only those `--mcp-compare` names get it,
  and one comparison runs at a time.
- **Browsers.** Requests from web pages are refused: a browser sends an
  `Origin`, an agent doesn't. The one exception is a request from
  `--external-url`.
- **Text people wrote.** Incident titles, notes and labels reach the agent
  as people wrote them, and the server tells it to treat them as data.

## What it doesn't do yet

- Queries to data sources that are neither PromQL nor LogQL (SQL,
  Elasticsearch, InfluxDB, a cloud's own logs) can't be replayed; such rules
  are listed with the reason. A Loki ruler's group runs a fraction of a
  second past a second, which the replay evaluates on: a log line written in
  that fraction is counted an evaluation later.
- Query errors aren't replayed: history can't say when a query failed.
  `--check-replay` says how many of Grafana's error alerts it left out of
  what really fired.
- A query that has returned nothing for over a day, or never in the
  history there is, is taken to have no history, not no data: such a
  Grafana rule is replayed as quiet, where Grafana may have sent NoData.
- A group without an `interval` is evaluated every `--step` (1m): the
  global `evaluation_interval` isn't in rule files, nor is the global
  `rule_query_offset`. An interval that isn't a whole number of steps is
  evaluated every step. Prometheus places a group within its interval by a
  hash of the path it loaded the file from, which a file doesn't say: read
  from a file, a firing can start up to an interval sooner or later than it
  did. `--loaded-rules` knows where.
- Grafana reduces a query's points as far apart as its `intervalMs` (1s by
  default) or more over a long window; lapilli asks for them so for
  `count`, `count_non_null` and `sum`, and for the rest at
  `--scrape-interval`, or else at the shortest interval the series they
  read are scraped at, as `/api/v1/status/config` and `/api/v1/targets`
  say, or, where neither is served (Mimir, Thanos, a managed Prometheus),
  at Grafana's 1s, which costs the server fifteen times and more the work:
  give `--scrape-interval` there. At the scrape interval, the step is cut
  to one that divides the rule's interval and the window too, so a sample
  is seen at the first evaluation after it, as Grafana sees it (a rule
  every 10s on series scraped every 15s is asked every 5s).
- A Grafana rule's query on another data source than its condition's (a
  Loki query whose values an annotation shows) isn't replayed: when it
  returns nothing, Grafana takes the rule as having no data, which the
  replay can't see, and the rule is marked "replayed in part".
- Replays see the history as it is now. `--late DUR` replays as if metrics
  arrived DUR after their time, but a delay that varies can't be read from
  history: a rule that resolved and fired again while the latest points
  were missing is replayed as one firing. Rules on a cloud's own monitoring
  are marked "replayed in part" until `--late` covers them.
- Rules alike in name, group and labels are counted together, and so are
  firings read with `--alerts` or `--firings`, which carry no group.
- `lapilli serve` runs as one replica with one ledger file. Who is who comes
  from its sign-in (an OpenID Connect provider, or a sign-in in front), and
  roles from the flags and the ones admins give; it doesn't sign people out
  at the provider, or refresh a session past `--session-max-age`. A version of the rules is in effect from when it saw
  it, up to `--watch` (1m) later, and for a mounted ConfigMap a minute or so
  more; the rules before its first start are taken to be those it found
  then, unless `--rules-history` says how they were. It gives a firing its
  stones against every incident it has, so a firing just before a week
  starts can catch an incident in that week, where
  `lapilli verdict` over that week alone sees only the week's firings.
- It counts firings, and the notifications they became only where the alert
  manager sends them to `--notify-listen`: Alertmanager's routing, grouping,
  inhibition and silences, and a Grafana notification policy, aren't read
  from their configuration. A firing that paged no one can't say why:
  silenced, inhibited and routed nowhere look the same from the receiving
  end. Notifications to PagerDuty, Slack and the like are seen only where the
  alert manager also sends them to Lapilli, or, from Alertmanager 0.33.0 on,
  its event recorder tells of them; the recorder's mutes, which say why a
  firing paged no one, aren't kept yet.
- Alarms kept in a cloud's own monitoring are replayed only where they are
  written in PromQL (CloudWatch PromQL alarms, Google Cloud policies with a
  PromQL condition, Azure's Prometheus rule groups). The rest are judged from
  their history, with `--firings` or copied into `lapilli serve`, and have
  no change reviews, longer `for`s or near misses.
- A CloudWatch PromQL alarm and a Google Cloud PromQL policy keep no series
  of what they fired: their replay is checked against their history only
  where it is copied in (or given with `--check-firings`), and CloudWatch's
  only as the alarm as a whole.
- Changes are carried out on four paths: a pull request where the rule is
  kept in git, Grafana's API, a managed Prometheus ruler's API (Mimir,
  Cortex, Amazon's, Azure's), and a cloud's own alarms' API (CloudWatch,
  Azure Monitor, Google Cloud Monitoring, OCI Monitoring), none of the
  clouds' run against an account yet. An alarm that isn't PromQL gets no
  `for` or threshold from Lapilli (nothing could backtest it), and is
  never deleted from here. Which tool made an alarm isn't asked of a
  cloud's deployment records: its tags and `--changes-in` decide. A
  Grafana rule's threshold is changed by a person, in Grafana. Google Cloud's `Rules` resources are
  Kubernetes resources, changed by the git path. Nothing on a Mimir's or a
  Cortex's group says which tool keeps it, and no ruler has a condition on a
  write: a write landing between Lapilli's read and its own is found when
  the change is read back, and fails.
- A pull request is opened only for a file in the repository that is the
  document Lapilli read. A rule file mounted from a ConfigMap whose manifest
  is what git holds, rules rendered from a chart, and rules read back from a
  cluster get a diff for a person instead.

## Help build it

Lapilli has one maintainer and is looking for more, from more than one
organisation. [ROADMAP.md](ROADMAP.md) lists what it doesn't do yet, area by
area, with where each lives in the code. Several areas need what the current
maintainer doesn't have: an AWS or Azure workspace, Loki alerts, an incident
tool. [MAINTAINERS.md](MAINTAINERS.md) says how contributing in an area leads
to reviewing and maintaining it, [GOVERNANCE.md](GOVERNANCE.md) how decisions
are made, and [CONTRIBUTING.md](CONTRIBUTING.md) how to build, test and send a
change. The most useful report there is: a rule that replays differently from
how it really fired, which `--check-replay` finds.

## Install

```sh
go install lapilli.dev/cmd/lapilli@latest
```

or take a binary from the [releases](https://github.com/lapilli-project/lapilli/releases),
or the image `ghcr.io/lapilli-project/lapilli`. Each release's provenance is
attested, and, from v0.28.0 on, kept beside it as
`lapilli-vX.Y.Z.intoto.jsonl`, the seventh of its assets beside the four
archives, the chart and `SHA256SUMS`; SECURITY.md says how to check it,
offline too.

## License

Apache 2.0. The UI's font, Heebo, is under the SIL Open Font License 1.1
([internal/serve/static/OFL-Heebo.txt](internal/serve/static/OFL-Heebo.txt)).
