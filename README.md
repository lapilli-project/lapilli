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
([below](#keep-judging)).

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

> Status: early, at v0.19, with one maintainer and room for more
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
  their labels; classic conditions too. `for`, `keep_firing_for` and a
  group's `interval` are honoured. `--rules FILE@TIME` gives the rules as
  they stood from a time on; see "Rules that changed" below.
- `--loaded-rules`: replay the alert rules the API at `--prometheus` has
  loaded, as `GET /api/v1/rules` lists them: Prometheus, Thanos, Mimir,
  vmalert, Amazon Managed Service for Prometheus.
- `--grafana URL`: replay the alert rules a Grafana holds, made in its UI
  or not, read through its provisioning export API with the service account
  token in `--grafana-token-file`.
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
the way back to the nearest that does (`rate(requests[5m]) > 10 → 8`). A comparison on the right of `unless`
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

Rules kept in git give their versions: `git log --format='%h %cI' -- rules.yaml`
lists when they changed, and `git show <commit>:rules.yaml` gives them as they
stood. Use the time a change reached Prometheus or Grafana, if it was later
than the commit.

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
`--auth google`. Lapilli only reads.

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
| AWS | a rule groups namespace | the AWS CLI's `describe-rule-groups-namespace` answer, as is; or `--loaded-rules --auth aws` (`aps:ListRules`) |
| AWS | CloudWatch PromQL alarms | the AWS CLI's `describe-alarms` answer, as is. The pending period is `for`, the recovery period `keep_firing_for`. Its other alarms are listed as not replayed |
| Azure | `Microsoft.AlertsManagement/prometheusRuleGroups` | an ARM template, or the JSON the Azure CLI or REST API gives for rule groups. Severity 0 to 4 becomes `severity=Sev0` to `Sev4`, and `timeToResolve` is how long an alert stays firing once its condition stops |
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
| Grafana | `GRAFANA_ALERTS{alertstate="firing"}` | once [state history](https://grafana.com/docs/grafana/latest/alerting/set-up/configure-alert-state-history/) is written to Prometheus; the default for its rules |
| Thanos, Mimir, Azure Monitor, Google Cloud | ask for `ALERTS` and see | their docs don't say; `--check-series` names another series |

When the series hold nothing for the period, or no rule could be replayed, it
says so and exits 1, since nothing was checked. If a rule replays differently from how it
really fired, please [report it](CONTRIBUTING.md): it is the most useful report
there is.

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
of PagerDuty, incident.io, Jira and GitHub issues. Each writes what `--incidents`
takes, so it works with both commands:

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
each version as it changes.

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

It isn't only to look at. Teams own rules (by a name pattern or labels)
and incidents, so the dashboard, the rules and the tasks can be shown for
one team. A rule keeps a note: its team, whether to keep, tune or retire
it, its runbook. A quiet window (a deploy, maintenance, a load test)
excuses the firings in it that caught nothing, so expected noise doesn't
weigh on a rule's precision. A suggestion on the dashboard becomes a task
with a press, with a team, an assignee, a due date, a status and a link
to the issue in your tracker, and rules, incidents and tasks take
comments. A quiet window can repeat every day or every week, for a
nightly batch or a weekly release. A team can keep its own working hours,
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
posts it, with mentions and links escaped.
Over twelve weeks, the digest also names the rules that are spare.

What happens (a task made, assigned, done or overdue, an incident written
down, a quiet window kept, a comment) can be told to a webhook, as JSON,
with `--notify-webhook-file`, and to Slack, as a line in `--lang`, with
`--notify-slack`. A tracker, a chat or a script takes the webhook; to keep
a task in step with an issue, make the issue from `task.created` and put
its URL back with `PATCH /api/v1/tasks/{id}`. Notices are written in the
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

The UI speaks English and Korean: the browser's language, or the one picked
at the top of the page. Each language is one file of translations,
`internal/serve/i18n/LANG.json`; a test fails if a text has none, so a
new language can't be left half done.

It listens on `127.0.0.1:8080` and has no sign-in of its own: keep it inside,
or put it behind the proxy that signs people in for you, and name the header
that proxy sets with `--user-header` so changes are recorded under it.
`--read-only` takes no changes. With `--editors` and `--admins`, people or
groups (`--groups-header`), only editors may write and only admins may
change the settings and the teams; the rest may only look. Write
`user:NAME` or `group:NAME` where someone could pick a name that is a
group's.

The proxy must be the only way in. The chart's `serve.networkPolicy` lets in
only the proxy, and Prometheus only to `/metrics` on a port of its own
(`--metrics-listen`). With `serve.userHeader`, the chart refuses to go
without it.

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
incidents can't write firings, and the reverse.

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
its end. A write is 31 days, 16 MB and 100,000 firings at most, and the
ledger keeps 50 sources.

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
when a job stopped copying.

With the firings token, `--rules` and `--prometheus` are optional: a team
with only cloud alarms runs `lapilli serve --incidents-listen :8082
--firings-token-file FILE`, nothing is replayed, and what comes in is
judged as it comes. `--impact` still needs `--prometheus`. In the chart,
`serve.firings.tokenSecret` names the Secret.

`lapilli serve -h` lists the rest: how often to run (`--every`), how far to
replay on the first start (`--backfill`, 28 days) and how long to keep
firings (`--keep`, 400 days).

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
| Grafana Alerting | `/v1/grafana`, a webhook contact point | the token as its Authorization header, or its HMAC signature with the secret in `--notify-grafana-secret-file` (timestamp header `X-Grafana-Alerting-Timestamp`, five minutes at most) |
| Amazon Managed Service for Prometheus | `/v1/sns`, an HTTPS subscription to the SNS topic its alert manager sends to, with AWS's JSON template for SNS | SNS's signature, from a topic `--notify-sns` names; the subscription is confirmed by fetching its `SubscribeURL`, from an SNS host only |
| Azure Monitor managed service for Prometheus | `/v1/azure`, an action group's webhook with the common alert schema | the token as the password in the URL: `https://lapilli:TOKEN@host/v1/azure` |
| Google Cloud Monitoring policies with a PromQL condition | `/v1/google`, a webhook channel with basic auth | the token as the password, sent once asked with a 401. Its webhooks reach public endpoints only |

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
`rules` and `rule` carry the same. Notifications can't be replayed for
history, so each number counts from the first that came. In the chart:
`serve.notificationsAPI`.

The port takes 1 MB and 1,000 alerts a notification at most (Alertmanager's
`max_alerts` keeps it under), 20 a second, one write at a time, and its
notifications are kept for `--keep`. It doesn't check the host name, as the
UI's port does: every request carries its own proof. Annotations aren't
kept. Who was on call, and whether they acknowledged, is for PagerDuty,
incident.io or Grafana IRM. Each path is tested against its source's
documented payloads, with a stand-in for SNS; none yet against a real
account.

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
nothing.

- **Access.** Agents come through the same sign-in proxy as the pages, and
  everyone gets the tools that read. `compare` runs PromQL the agent wrote
  with Lapilli's access to Prometheus, so only those `--mcp-compare` names
  get it, and one comparison runs at a time.
- **Browsers.** Requests from web pages are refused: a browser sends an
  `Origin`, an agent doesn't. The one exception is a request from
  `--external-url`.
- **Text people wrote.** Incident titles, notes and labels reach the agent
  as people wrote them, and the server tells it to treat them as data.

## What it doesn't do yet

- Queries to datasources that don't speak PromQL (Loki, SQL), and classic
  conditions with a reducer PromQL has no match for (`diff`,
  `percent_diff`) or `no_value`, can't be replayed; such rules are listed
  with the reason.
- No-data and error states are not replayed, only the condition.
- A group without an `interval` is evaluated every `--step` (1m): the
  global `evaluation_interval` isn't in rule files. An interval that isn't a
  whole number of steps is evaluated every step. Prometheus spreads groups
  over their interval in a way a replay can't know, so with one, a firing
  can start up to that interval sooner or later than it did.
- Grafana reduces a query's points as far apart as its `intervalMs`, or the
  data source's minimum interval; for `count` and `sum`, where that matters,
  lapilli takes `intervalMs` or 15s, the Prometheus data source's default.
- Replays see the history as it is now. A rule on a metric that arrives late,
  as a cloud's own metrics of its services often do, may have resolved and
  fired again while the latest points were missing; the replay shows it as
  one firing.
- Rules alike in name, group and labels are counted together, and so are
  firings read with `--alerts` or `--firings`, which carry no group.
- `lapilli serve` runs as one replica with one ledger file, and has no
  sign-in of its own: roles come from the proxy that signs people in. A version of the rules is in effect from when it saw
  it, up to `--watch` (1m) later, and for a mounted ConfigMap a minute or so
  more; the rules before its first start are taken to be those it found
  then. It gives a firing its stones against every incident it has, so a
  firing just before a week starts can catch an incident in that week, where
  `lapilli verdict` over that week alone sees only the week's firings.
- It counts firings, and the notifications they became only where the alert
  manager sends them to `--notify-listen`: Alertmanager's routing, grouping,
  inhibition and silences, and a Grafana notification policy, aren't read
  from their configuration. A firing that paged no one can't say why:
  silenced, inhibited and routed nowhere look the same from the receiving
  end. Notifications to PagerDuty, Slack and the like are seen only where the
  alert manager also sends them to Lapilli.
- Alarms kept in a cloud's own monitoring are replayed only where they are
  written in PromQL (CloudWatch PromQL alarms, Google Cloud policies with a
  PromQL condition, Azure's Prometheus rule groups). The rest are judged from
  their history, with `--firings` or copied into `lapilli serve`, and have
  no change reviews, longer `for`s or near misses.
- A CloudWatch PromQL alarm and a Google Cloud PromQL policy keep no series
  of what they fired, so `--check-replay` has nothing to check their replay
  against.

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
or the image `ghcr.io/lapilli-project/lapilli`.

## License

Apache 2.0. The UI's font, Heebo, is under the SIL Open Font License 1.1
([internal/serve/static/OFL-Heebo.txt](internal/serve/static/OFL-Heebo.txt)).
