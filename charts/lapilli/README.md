# lapilli

Runs `lapilli verdict` as a CronJob, by default on Mondays at 09:00 over the
week before, or `lapilli serve` as a Deployment (below): it replays your
alert rules over the history in Prometheus and says which firings caught
something that went wrong (white stones) and which caught nothing (black
stones), which incidents had no alert, and which noisy rules a longer `for`
would quiet. The report goes to the job's log, and, if
you like, to Slack and a Pushgateway.

```sh
helm install lapilli oci://ghcr.io/lapilli-project/charts/lapilli -n monitoring \
  --set prometheus.url=http://prometheus:9090 \
  --set 'rules.configMaps={alert-rules}' \
  --set impact='sum by (service) (rate(http_requests_total{code=~"5.."}[5m])) / sum by (service) (rate(http_requests_total[5m])) > 0.01' \
  --set 'match={service}' \
  --set report.slackWebhookSecret.name=lapilli-slack \
  --set report.pushgateway=http://pushgateway:9091
```

It needs:

- `prometheus.url`: a Prometheus-compatible API (Prometheus, Thanos, Mimir,
  VictoriaMetrics), or a cloud's managed Prometheus with `prometheus.auth`
  set to `aws`, `azure` or `google` and read access given through the
  cluster's workload identity. On Oracle Cloud, give the Prometheus you run
  on OKE, with no auth.
- the rules, from one of `rules.configMaps` (ConfigMaps in the release's
  namespace holding Prometheus rule files, PrometheusRule resources, Grafana
  provisioning files or a cloud's rule groups), `rules.loaded` (those the API
  has loaded), `rules.grafana.url` (those a Grafana holds) and
  `rules.ruler.url` (those a Mimir or Cortex ruler, Amazon Managed Service
  for Prometheus or Azure's Prometheus rule groups hold, as written); and,
  beside any of them or alone, a cloud's own alarms in `rules.alarms`
  (CloudWatch's, Azure Monitor's, Google Cloud Monitoring's, OCI
  Monitoring's, each named by the source its history is copied in under);
- what went wrong: `incidents`, `impact`, or both.

With `mode: serve` it runs `lapilli serve` instead: every hour into a ledger
on a PersistentVolumeClaim, with a web UI, an API and `/metrics`, behind a
ClusterIP Service. Incidents can then be written down in the UI, so
`incidents` and `impact` are optional; `rules.configMaps` takes one
ConfigMap, read every minute, each change becoming a new version.

```sh
helm install lapilli oci://ghcr.io/lapilli-project/charts/lapilli -n monitoring \
  --set mode=serve --set prometheus.url=http://prometheus:9090 \
  --set 'rules.configMaps={alert-rules}' --set 'match={service}'
kubectl -n monitoring port-forward svc/lapilli 8080
```

The UI opens on a dashboard of the last twelve weeks, and keeps more than
the runs find:
- teams that own rules and incidents
- notes on rules
- quiet windows, once or repeating, that excuse expected noise
- tasks to follow the noisy rules and missed incidents up, with links to a
  tracker's issues
- comments

It speaks English, Spanish, Japanese and Korean (`serve.notify.lang` picks
what it posts in: `en`, `es`, `ja` or `ko`).

`serve.teamDigests` posts a team its own weekly digest. `serve.reports.destinations`
names the Slack-style webhooks, each in a Secret, that the reports people
schedule in the UI can go to; the UI picks among the names, never takes a URL. `serve.incidentsAPI`
opens a port that takes incidents, and does nothing else, from a job holding a
token: one that copies them from where they are kept. See `recipes/incidents`
in the repository. `serve.firings.tokenSecret` lets the same port take firings
recorded elsewhere, such as a cloud's own alarm history, with a token of its
own; see `recipes/firings`. With it, `rules` and `prometheus` are optional.
`serve.trackers.tokenSecret` lets it read tasks and set their link and status,
with a token of its own, and with `serve.trackers.tracker` (`github`, `gitlab`,
`jira` or `linear`), `image` (one with `sh`, `curl` and `jq`, by its digest,
`NAME@sha256:...`: a tag is refused) and `trackerTokenSecret`, a CronJob keeps
tasks in step with that tracker's issues every five minutes; see
`recipes/trackers`.
`serve.notificationsAPI` opens a port that takes notifications as the alert
manager sent them, and does nothing else:
Alertmanager's and Grafana's webhooks, Amazon SNS for Amazon Managed Service
for Prometheus and for CloudWatch alarms, Azure Monitor's action groups,
Google Cloud Monitoring's webhook channels and OCI Notifications'
subscriptions. Lapilli then counts each rule's notifications, repeats and
pages while people slept beside its firings, a cloud's own alarms' among
them.

`serve.rulesHistory` gives a first start the rules as they were before it,
from the git repository the rules in `rules.configMaps` are kept in, so its
backfill replays each day with that day's rules. One init container clones
the branch (`repository`, `branch`), with every object, and a second runs
`recipes/versions/git.sh` for `paths` since `since` (by default the backfill
and a day more), into a volume `serve` reads with `--rules-history`.
`render` is a command that writes the rules from a commit's files, for rules
rendered from a chart (the image then needs what it runs). `tokenSecret`
names a Secret holding a token for an https repository that needs one; only
the first container has it, and it goes to the repository's host alone, so
`render` never sees it. The image is `alpine/git`, pinned by digest; both
run as the pod does, with a read-only root. When the repository can't be
cloned, or the history written, the directory is left empty and the log
says so, rather than hold the pod up: `serve` then starts with today's
rules. A ledger with versions of its
own keeps them and doesn't read the directory, so this can stay on.

The init containers run git, and `render`, on what the repository holds, so
a cloud's identity is kept from them. The service account's token is never
mounted in them: the pod doesn't mount it (`automountServiceAccountToken:
false`), and lapilli's own container has it, where Kubernetes would put it,
only with `prometheus.auth`, `gcpProject` or OCI's alarms, for a workload
identity that reads it (OKE's). EKS's webhook (IRSA and Pod Identity) and
Azure Workload Identity's are told to pass the init containers over
(`eks.amazonaws.com/skip-containers`, `azure.workload.identity/skip-containers`,
added to `podAnnotations`). GKE's Workload Identity answers every container
in the pod from its metadata server, and no Kubernetes setting keeps one
container from it: on GKE, give `render` only commands that run nothing the
repository holds, or leave the rules history off.

`serve.checkReplay` sets the replay beside what really fired, a day at a
time once the runs won't replay it again, from `ALERTS` and `GRAFANA_ALERTS`
or the `series` given; a rule whose replay differs over a week (`differs`,
`5/0.9` unless given) says so on its page and in `/metrics`. It is on;
`enabled: false` turns it off.

The UI has no sign-in of its own: keep it inside the cluster, or behind a
proxy that signs people in.
- `serve.userHeader` names the header the proxy puts the person in.
- `serve.roles` limits who may change what. `editors` write; `admins` also
  change the settings and the teams; everyone else may only look. Groups
  come from `groupsHeader`.
- Without `serve.roles`, everyone may only look: lapilli serve listens on
  the pod's address, which every pod in the cluster reaches. Give roles
  behind the proxy, or set `serve.everyoneAdmin` to let everyone who
  reaches the UI or the API change anything, as an admin; it is off by
  default, and refused beside `serve.roles` or `serve.readOnly`. Before
  0.28.1, everyone was an admin without roles, `kubectl port-forward`
  included: that is now `serve.everyoneAdmin`.
- `serve.networkPolicy` lets in only the proxy (`from`), and Prometheus
  (`metricsFrom`) only to the metrics port, so the headers can't be forged
  from inside the cluster.
  - `serve.userHeader` is refused without it, unless
    `serve.networkPolicy.othersKeptOut` says something else keeps the rest
    out.
  - The probes and the ServiceMonitor use the metrics port,
    `serve.metricsPort` (9091).
  - `serve.networkPolicy.egress` limits where the pod may connect, in a
    NetworkPolicy of its own: off by default, then the cluster's DNS and
    the egress rules in `to` alone. Name each place it talks to: the
    Prometheus, the rules' API, the rules history's repository, Slack and
    the webhooks, a cloud's APIs and its identity's token endpoint. A
    cloud's metadata server (169.254.169.254) and the Kubernetes API are
    then out of reach unless named; a workload identity that answers from
    the metadata server (GKE's) needs it named, as Google's documentation
    on network policies says.
- `serve.allowedHosts` names the host your proxy or ingress passes on, if
  it isn't `serve.externalURL`'s. Other names get 421, against DNS
  rebinding.

`serve.notify` tells what happens to a webhook, whose URL is kept in a
Secret, and to Slack. The events are tasks made, assigned, done or overdue,
incidents written down, quiet windows kept, and comments. Links point under
`serve.externalURL`.

Agents reach `/mcp` (MCP) and read what the pages show. `serve.mcpCompare`
(`editors` or `admins`) lets their agents replay a change to the rules they
propose. It is off by default, since it runs PromQL the agent writes, with
lapilli's access to Prometheus, and it needs `serve.roles`.
`serve.mcpPropose` lets their agents propose a change to a rule, for a
person to approve; an agent never approves or applies one.

`lapilli serve` reads by default, and changes only what a person approves.
`serve.changes.enabled` turns changes on: a change an approver approved in
the UI is checked again, then opened as a pull request by a recipe in
[recipes/changes](https://github.com/lapilli-project/lapilli/tree/main/recipes/changes),
which holds the token in `serve.changes.tokenSecret` and reaches the
`incidentsAPI` port, or, for rules read from Grafana, applied through its
API with `serve.changes.grafanaTokenSecret`. It needs `serve.userHeader`,
roles, `serve.roles.editors` or `serve.roles.admins` (without them everyone
is an admin, who could resume changes), and someone to approve: the
admins, unless `serve.changes.approvers` says who; `approvals: 2` asks for
two people. The Secrets are mounted only
when changes are on. To stop changes at once, pause them in the UI, or make
a ConfigMap with a key `paused` named after the chart's full name: the
release's name followed by `-lapilli`, unless it has `lapilli` in it
already, then `-changes-paused`. For a release named `ops`:

```sh
kubectl create configmap ops-lapilli-changes-paused --from-literal=paused=1
```

For one named `lapilli`, `lapilli-changes-paused`. `helm install` prints
the command for the release. Deleting the ConfigMap resumes changes.

For rules read from a ruler (`rules.ruler.url`), a change is applied
through the ruler's own API, and only in the namespaces, or Azure rule
groups, `serve.changes.in` names: name none a tool keeps (mimirtool rules
sync, Terraform, Alloy's `mimir.rules.kubernetes`), as it would undo the
change. A ruler's token reaches every namespace of its tenant, so
`serve.changes.in` is Lapilli's own check. The write credentials, one a
kind of ruler, are used only for writes:

- Mimir, Cortex: `serve.changes.rulerTokenSecret`, a token that may write
  rules (`rules:write`); `rules.ruler.tokenSecret`'s reads them.
- Amazon: `serve.changes.awsRole`, a role the pod's identity assumes (its
  own role needs `sts:AssumeRole` on it, and the role's trust policy lets
  it), allowed only this, on the namespaces named:

  ```json
  {
    "Version": "2012-10-17",
    "Statement": [{
      "Effect": "Allow",
      "Action": ["aps:PutRuleGroupsNamespace", "aps:DescribeRuleGroupsNamespace"],
      "Resource": [
        "arn:aws:aps:REGION:ACCOUNT:rulegroupsnamespace/WORKSPACE_ID/shop",
        "arn:aws:aps:REGION:ACCOUNT:rulegroupsnamespace/WORKSPACE_ID/solo"
      ]
    }]
  }
  ```

  The pod's own role reads them with `aps:ListRuleGroupsNamespaces` and
  `aps:DescribeRuleGroupsNamespace` on the workspace.
- Azure: `serve.changes.azureClientId`, the client ID of a second
  user-assigned identity federated with the same service account, given
  this custom role, assigned on each rule group named:

  ```json
  {
    "Name": "Lapilli rule group writer",
    "IsCustom": true,
    "Actions": [
      "Microsoft.AlertsManagement/prometheusRuleGroups/read",
      "Microsoft.AlertsManagement/prometheusRuleGroups/write"
    ],
    "AssignableScopes": ["/subscriptions/SUB/resourceGroups/RG"]
  }
  ```

  It has no `delete`: Lapilli never deletes a rule group.

Without one, a Mimir's or a Cortex's writes are sent with no token, for a
ruler reached only inside the cluster, and Amazon's and Azure's with the
pod's own identity. The Amazon and Azure paths were tested against
stand-ins answering as their API documents show, not against an account.

A cloud's own alarms (`rules.alarms`) are read with the pod's own
identity, whatever `prometheus.auth` says, every `rules.alarmsEvery` (10m
by default):

- CloudWatch: `cloudwatch:DescribeAlarms` on `*` (composite alarms are
  described only so) and `cloudwatch:ListTagsForResource`.
- Azure: Monitoring Reader on the subscription or resource group.
- Google Cloud: `roles/monitoring.viewer` on the project.
- OCI: `inspect alarms`, `read alarms` and `read metrics` in the
  compartment (`GetAlarm` asks for `METRIC_READ` in the alarm's metric
  compartment), through OKE workload identity, a resource principal or an
  instance principal.

With changes on, an approved change to an alarm is applied through its
cloud's API, only where `serve.changes.in` names it as `SOURCE:NAME`
(CloudWatch's alarm name, Azure's `RG/NAME`, Google's policy's display
name, OCI's display name; `NAME*` for a prefix), and never to one tagged as
another tool's. Name none that CloudFormation, Terraform, Bicep, Pulumi or
Config Connector keeps. An alarm Lapilli can't replay (not PromQL) is
retired as far as off, never deleted from here. Turning an alarm off, or
deleting it, waits while it fires: CloudWatch's alarm state, Azure's
alerts (Alerts Management, `Microsoft.AlertsManagement/alerts/read`, in
Monitoring Reader), Google's alerts (`monitoring.alerts.list`, in
`roles/monitoring.viewer`), OCI's alarm status (`ListAlarmsStatus`, under
the `inspect alarms` and `read metrics` above), each read with the pod's
own identity just before the write. Each cloud's write identity, used only
for writes, is given only this:

- CloudWatch: `serve.changes.awsRole`, for each region and prefix named:

  ```json
  {
    "Version": "2012-10-17",
    "Statement": [
      {
        "Effect": "Allow",
        "Action": [
          "cloudwatch:PutMetricAlarm",
          "cloudwatch:DisableAlarmActions",
          "cloudwatch:EnableAlarmActions",
          "cloudwatch:DeleteAlarms",
          "cloudwatch:ListTagsForResource",
          "cloudwatch:TagResource"
        ],
        "Resource": "arn:aws:cloudwatch:REGION:ACCOUNT:alarm:checkout-*"
      },
      {
        "Effect": "Allow",
        "Action": "cloudwatch:DescribeAlarms",
        "Resource": "*"
      }
    ]
  }
  ```

  `DeleteAlarms` and `TagResource` are for a PromQL alarm deleted four
  weeks after it was turned off, and made again with its tags to undo it.
  No `PutCompositeAlarm`, EC2, Auto Scaling or `iam:CreateServiceLinkedRole`:
  the alarms that would need them aren't changed. CloudWatch's writes name
  an alarm by its name, in the caller's account, so a write is refused
  unless the alarm's ARN is in the role's account (without
  `serve.changes.awsRole`, the pod's own, as STS `GetCallerIdentity` says;
  it needs no permission, but it needs STS: a cluster that reaches
  CloudWatch alone, through its VPC endpoint, say, fails every write
  without the role, so give it STS's endpoint too, or set the role).
- Azure: `serve.changes.azureClientId`, a second user-assigned identity
  federated with the same service account, given this custom role on each
  alert rule named, or on the resource group for `RG/*`:

  ```json
  {
    "Name": "Lapilli alert rule changes",
    "IsCustom": true,
    "Actions": [
      "Microsoft.Insights/metricAlerts/read",
      "Microsoft.Insights/metricAlerts/write",
      "Microsoft.Insights/metricAlerts/delete",
      "Microsoft.Insights/scheduledQueryRules/read",
      "Microsoft.Insights/scheduledQueryRules/write",
      "Microsoft.Insights/activityLogAlerts/read",
      "Microsoft.Insights/activityLogAlerts/write"
    ],
    "AssignableScopes": ["/subscriptions/SUB/resourceGroups/RG"]
  }
  ```

  Only a PromQL metric alert is ever deleted, so only `metricAlerts` has
  `delete`.
- Google Cloud: `serve.changes.googleServiceAccount`, a service account the
  pod's identity impersonates (it holds
  `roles/iam.serviceAccountTokenCreator` on that one account, and nothing
  more), given on the project this custom role:

  ```yaml
  title: Lapilli alerting policy changes
  stage: GA
  includedPermissions:
  - monitoring.alertPolicies.get
  - monitoring.alertPolicies.list
  - monitoring.alertPolicies.update
  - monitoring.alertPolicies.delete
  - monitoring.alertPolicies.create
  ```

  Google grants these on a project only, so `serve.changes.in` is
  Lapilli's own check there. `create` is for a PromQL policy deleted and
  made again to undo it.
- OCI: `serve.changes.ociConfigSecret`, a Secret holding an OCI config
  file (`configKey`) and the API key it names (`keyKey`), for a user in a
  group of its own; its `key_file` is `/etc/lapilli/oci/key.pem`. OCI's
  workload identity has no second identity a pod can take for writes, so
  this one is a key, mounted only when changes are on. The group's policy,
  for each compartment named:

  ```
  Allow group lapilli-alarm-changes to manage alarms in compartment shop
    where any {request.permission = 'ALARM_INSPECT', request.permission = 'ALARM_READ',
               request.permission = 'ALARM_UPDATE'}
  Allow group lapilli-alarm-changes to read metrics in compartment shop
  ```

  `manage` is the only verb with `ALARM_UPDATE`; the condition leaves out
  the rest. OCI's alarms aren't PromQL, so Lapilli never deletes or makes
  one again.

None of the four was run against an account: each was tested against a
stand-in answering as its API documents show.

A release's chart runs its image by digest: `image.digest`, which the
release workflow writes with the digest of the image it built and attested,
as it does the chart's `lapilli.dev/image-digest` annotation. With
`image.tag` another version, give that version's `image.digest` too, or
empty it: the chart refuses another tag with its own digest, which would
run its own image under another version's name.

`serve.serviceMonitor.enabled` makes a ServiceMonitor, and
`dashboard.enabled` a ConfigMap holding a Grafana dashboard for Grafana's
sidecar.

Webhook URLs and tokens are read from Secrets and never shown.
See [values.yaml](https://github.com/lapilli-project/lapilli/blob/main/charts/lapilli/values.yaml)
for every setting and [lapilli.dev](https://lapilli.dev) for how the verdict
is reached.
