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
  has loaded) and `rules.grafana.url` (those a Grafana holds);
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

It speaks English and Korean.

`serve.teamDigests` posts a team its own weekly digest. `serve.incidentsAPI`
opens a port that takes incidents, and does nothing else, from a job holding a
token: one that copies them from where they are kept. See `recipes/incidents`
in the repository. `serve.firings.tokenSecret` lets the same port take firings
recorded elsewhere, such as a cloud's own alarm history, with a token of its
own; see `recipes/firings`. With it, `rules` and `prometheus` are optional.
`serve.trackers.tokenSecret` lets it read tasks and set their link and status,
with a token of its own, and with `serve.trackers.tracker` (`github`, `gitlab`,
`jira` or `linear`), `image` (one with `sh`, `curl` and `jq`) and
`trackerTokenSecret`, a CronJob keeps tasks in step with that tracker's issues
every five minutes; see `recipes/trackers`.
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
- `serve.networkPolicy` lets in only the proxy (`from`), and Prometheus
  (`metricsFrom`) only to the metrics port, so the headers can't be forged
  from inside the cluster.
  - `serve.userHeader` is refused without it, unless
    `serve.networkPolicy.othersKeptOut` says something else keeps the rest
    out.
  - The probes and the ServiceMonitor use the metrics port,
    `serve.metricsPort` (9091).
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

`serve.serviceMonitor.enabled` makes a ServiceMonitor, and
`dashboard.enabled` a ConfigMap holding a Grafana dashboard for Grafana's
sidecar.

Webhook URLs and tokens are read from Secrets and never shown.
See [values.yaml](https://github.com/lapilli-project/lapilli/blob/main/charts/lapilli/values.yaml)
for every setting and [lapilli.dev](https://lapilli.dev) for how the verdict
is reached.
