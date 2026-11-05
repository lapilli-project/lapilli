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

Noisy rules that a longer "for" would quiet, keeping every incident they caught:
RULE                FOR       FIRED     ○ CAUGHT
ExportJob5xx        5m → 15m  118 → 31  0 → 0
CheckoutLatencyP99  5m → 10m  14 → 6    2 → 2

38 rules never fired.

INCIDENT                         START             CAUGHT BY
INC-41 Checkout errors           2026-10-28 12:01  DBPrimaryCPU +2m, CheckoutLatencyP99 +6m (noisy)
INC-42 Search timeouts           2026-10-30 09:15  ReadReplicaLag -3m
INC-43 Batch stuck               2026-10-31 02:40  ✗ nothing fired
impact-1 user impact (--impact)  2026-11-01 21:27  ✗ nothing fired

✗ 2 of 4 incidents had no alert.
```

> Status: early, at v0.11, with one maintainer and room for more
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
  each cloud's managed rule groups (see below), ConfigMaps holding any of
  them, and directories of them, below them too; below the top, files that
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
`clusterName` within that cluster. Lapilli does the same where the resource
says how: every selector in a `Rules` expression gets its namespace as a
`namespace` matcher, and in an Azure rule group with a `clusterName`, that
as a `cluster` matcher. A Google Cloud cluster's name isn't in the
resource, so with several clusters in one project, add `cluster` matchers
to the expressions, or point `--prometheus` at that cluster's data alone.

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
week before, works out longer `for`s for the noisy rules, and posts the report
to Slack.

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
precision, firings, incidents missed, time to catch and noisy rules beside
the week before; firings, precision and incidents by week; where the noise
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
nightly batch or a weekly release. All of it is in the same ledger,
through the UI or the API; see [docs/design/work.md](docs/design/work.md).

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
change the settings and the teams; the rest may only look. The proxy must
be the only way in: the chart's `serve.networkPolicy` lets in only it. Posts
from other sites are refused, and the pages run no scripts.

In the chart, `mode: serve` runs it as one replica with the ledger on a
PersistentVolumeClaim, behind a ClusterIP Service:

```sh
helm install lapilli oci://ghcr.io/lapilli-project/charts/lapilli -n monitoring \
  --set mode=serve --set prometheus.url=http://prometheus:9090 \
  --set 'rules.configMaps={alert-rules}' --set 'match={service}'
kubectl -n monitoring port-forward svc/lapilli 8080
```

`lapilli serve -h` lists the rest: how often to run (`--every`), how far to
replay on the first start (`--backfill`, 28 days) and how long to keep
firings (`--keep`, 400 days).

## Ask it from an agent

`lapilli serve` speaks [MCP](https://modelcontextprotocol.io) at `/mcp`
(Streamable HTTP), so an agent can ask it what the pages show, and check a
change to the rules before anyone makes it. The agent can be Claude Code,
Cursor, VS Code, or one of your own:

```sh
kubectl -n monitoring port-forward svc/lapilli 8080
claude mcp add --transport http lapilli http://localhost:8080/mcp
```

`compare` is off until `--mcp-compare editors` (or `admins`) turns it on.

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
- Alarms kept in a cloud's own monitoring (Amazon CloudWatch alarms, Azure
  Monitor alerts, Google Cloud alerting policies, OCI Monitoring alarms) are
  not read or replayed; only rule files are. What they fired, written as
  `--firings`, can still be judged.

## Help build it

Lapilli has one maintainer and is looking for more, from more than one
organisation. [ROADMAP.md](ROADMAP.md) lists what it doesn't do yet, area by
area, with where each lives in the code. Several areas need what the current
maintainer doesn't have: an AWS or Azure workspace, Loki alerts, an incident
tool. [MAINTAINERS.md](MAINTAINERS.md) says how contributing in an area leads
to reviewing and maintaining it, [GOVERNANCE.md](GOVERNANCE.md) how decisions
are made, and [CONTRIBUTING.md](CONTRIBUTING.md) how to build, test and send a
change. The most useful report there is: a rule that replays differently from
how it really fired.

## Install

```sh
go install lapilli.dev/cmd/lapilli@latest
```

or take a binary from the [releases](https://github.com/lapilli-project/lapilli/releases),
or the image `ghcr.io/lapilli-project/lapilli`.

## License

Apache 2.0. The UI's font, Heebo, is under the SIL Open Font License 1.1
([internal/serve/static/OFL-Heebo.txt](internal/serve/static/OFL-Heebo.txt)).
