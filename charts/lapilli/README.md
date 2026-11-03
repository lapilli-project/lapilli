# lapilli

Runs `lapilli verdict` as a CronJob, by default on Mondays at 09:00 over the
week before: it replays your alert rules over the history in Prometheus and
says which firings caught something that went wrong (white stones) and which
caught nothing (black stones), which incidents had no alert, and which noisy
rules a longer `for` would quiet. The report goes to the job's log, and, if
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

The Slack webhook's URL and any tokens are read from Secrets and never shown.
See [values.yaml](https://github.com/lapilli-project/lapilli/blob/main/charts/lapilli/values.yaml)
for every setting and [lapilli.dev](https://lapilli.dev) for how the verdict
is reached.
