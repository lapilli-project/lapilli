<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/lapilli-lockup-dark.png">
  <img alt="Lapilli" src="assets/lapilli-lockup.png" width="300">
</picture>

**Which of your alerts are worth their page?** `lapilli verdict` judges every
firing against what actually went wrong: a white stone when the alert caught an
incident, a black stone when nothing was wrong. Count a rule's stones over a
week and you know whether it earns its place; an incident with no stone is one
your alerts missed.

```
$ lapilli verdict --rules alert-rules.yaml --prometheus http://prometheus:9090 \
    --incidents incidents.yaml --impact 'sum(rate(http_5xx[5m])) / sum(rate(http_total[5m])) > 0.01'
Lapilli  2026-10-26 00:00 → 2026-11-02 00:00 Asia/Seoul
148 firings, 3 incidents written down, 1 more found by --impact

RULE                SEVERITY  FIRED  ○ CAUGHT  ● NOTHING  PRECISION  REFIRED
OrderCancelTask5xx  critical  118    0         118        0%         41
CheckoutLatencyP99  warning   14     2         12         14%        3
ReadReplicaLag      warning   9      1         8          11%        0
NodeNotReady        critical  4      0         4          0%         0
DBPrimaryCPU        critical  3      2         1          67%        0

Noisy rules that a longer "for" would quiet, keeping every incident they caught:
RULE                FOR       FIRED     ○ CAUGHT
OrderCancelTask5xx  5m → 15m  118 → 31  0 → 0
CheckoutLatencyP99  5m → 10m  14 → 6    2 → 2

38 rules never fired.

INCIDENT                         START             CAUGHT BY
INC-41 Checkout errors           2026-10-28 12:01  DBPrimaryCPU +2m, CheckoutLatencyP99 +6m (noisy)
INC-42 Search timeouts           2026-10-30 09:15  ReadReplicaLag -3m
INC-43 Batch stuck               2026-10-31 02:40  ✗ nothing fired
impact-1 user impact (--impact)  2026-11-01 21:27  ✗ nothing fired

✗ 2 of 4 incidents had no alert.
```

> Status: early. Checked against one production Grafana: all 71 of its alert
> rules replay, and a four-week replay of the rules as they stood then found
> 177 firings of the noisiest rule, the number of notifications its team had
> counted by hand in their alert channels. Weekly counts of critical alerts
> came within 10% of theirs in three of the four weeks.

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

> *mos erat antiquus niveis atrisque lapillis,*
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
  each cloud's managed rule groups (see below), ConfigMaps holding any of
  them, and directories of them, below them too. Grafana rules are evaluated the way
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
  (`{"rule":"...","labels":{...},"start":"...","end":"..."}`).

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
`lapilli_rule_nothing`, `lapilli_rule_refired` (by `rule`),
`lapilli_incidents` (by `state` and `source`) and
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

`--aws-region` names the region to sign for when the URL doesn't (a VPC
endpoint, say). `--gcp-project PROJECT` is short for Google Cloud's URL and
`--auth google`. Lapilli only reads.

Oracle Cloud has no managed Prometheus to query: OCI Monitoring is queried in
its own language, MQL. On OKE, give `--prometheus` the Prometheus you run
there; it needs no `--auth`.

Each cloud keeps the alert rules of its managed Prometheus in a form of its
own, and `--rules` reads each:

| Cloud | Rules | To read them |
|---|---|---|
| AWS | a rule groups namespace | the AWS CLI's `describe-rule-groups-namespace` answer, as is; or `--loaded-rules --auth aws` (`aps:ListRules`) |
| Azure | `Microsoft.AlertsManagement/prometheusRuleGroups` | an ARM template, or the JSON the Azure CLI or REST API gives for rule groups. Severity 0 to 4 becomes `severity=Sev0` to `Sev4`, and `timeToResolve` is how long an alert stays firing once its condition stops |
| Google Cloud | `Rules`, `ClusterRules`, `GlobalRules` (`monitoring.googleapis.com/v1`) | the resources, as files or from `kubectl get ... -o yaml` |
| Oracle Cloud | no managed Prometheus | the rule files of the Prometheus you run |

Google Cloud evaluates `Rules` within their namespace and cluster, and
`ClusterRules` within their cluster; Azure, a rule group with a
`clusterName` within that cluster. Lapilli replays each expression as
written, against all the history `--prometheus` sees: add the matchers to
the expression, or point `--prometheus` at that data alone, when it matters.

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

RULE                SEVERITY     FIRED    ○ CAUGHT  ● NOTHING  PRECISION  REFIRED
OrderCancelTask5xx  critical     118 → 9  0 → 0     118 → 9    0% → 0%    41 → 2
DiskFillingUp       – → warning  – → 3    – → 1     – → 2      – → 33%    – → 0

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
during the week is judged as it is now.

## What it doesn't do yet

- Queries to datasources that don't speak PromQL (Loki, SQL), and classic
  conditions with a reducer PromQL has no match for (`diff`,
  `percent_diff`) or `no_value`, can't be replayed; such rules are listed
  with the reason.
- No-data and error states are not replayed, only the condition.
- A group without an `interval` is evaluated every `--step` (1m): the
  global `evaluation_interval` isn't in rule files. An interval that isn't a
  whole number of steps is evaluated every step.
- Replays see the history as it is now. A rule on a metric that arrives late,
  as a cloud's own metrics of its services often do, may have resolved and
  fired again while the latest points were missing; the replay shows it as
  one firing.
- Rules alike in name, group and labels are counted together, and so are
  firings read with `--alerts` or `--firings`, which carry no group.
- Alarms kept in a cloud's own monitoring (Amazon CloudWatch alarms, Azure
  Monitor alerts, Google Cloud alerting policies, OCI Monitoring alarms) are
  not read or replayed; only rule files are. What they fired, written as
  `--firings`, can still be judged.

## Install

```sh
go install lapilli.dev/cmd/lapilli@latest
```

or take a binary from the [releases](https://github.com/lapilli-project/lapilli/releases),
or the image `ghcr.io/lapilli-project/lapilli`.

## License

Apache 2.0
