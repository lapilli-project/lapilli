# Every week

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
[values.yaml](../charts/lapilli/values.yaml) for these and the rest.

Without Kubernetes, the same flags work from cron: `--push URL` puts the
metrics on a Pushgateway, and `--slack-webhook-file FILE` posts the report.
The image is `ghcr.io/lapilli-project/lapilli`.

The weekly run replays the rules as they are when it runs; a rule changed
during the week is judged as it is now. `lapilli serve` doesn't: it records
each version as it changes. It also sends reports on schedules people set
themselves, in its UI: each morning over yesterday, each month, at a team's own
nine o'clock (see [Keep judging](serve.md)). With the notifications as they
were sent, its digest and reports say what they came to, and, from
Alertmanager's event recorder, how many firings paged no one because a
silence or an inhibition muted them, and how many with no mute told of
([Why a firing paged no one](notifications.md#why-a-firing-paged-no-one)).
