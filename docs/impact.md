# Incidents nobody wrote down

The incidents a team writes down are the ones it noticed; those it didn't
are the ones no alert caught. `--impact` finds them in a signal of user
impact. A per-service error ratio from what is in front of your services
works well, with `--match service` and labels renamed to `service` where
needed (`label_replace`):

| In front | `--impact` |
|---|---|
| Istio | `sum by (destination_service_name) (rate(istio_requests_total{reporter="destination", response_code=~"5.."}[5m])) / sum by (destination_service_name) (rate(istio_requests_total{reporter="destination"}[5m])) > 0.01` |
| Linkerd | `sum by (deployment) (rate(response_total{direction="inbound", classification="failure"}[5m])) / sum by (deployment) (rate(response_total{direction="inbound"}[5m])) > 0.01` |
| Envoy or Envoy Gateway | `sum by (envoy_cluster_name) (rate(envoy_cluster_upstream_rq_xx{envoy_response_code_class="5"}[5m])) / sum by (envoy_cluster_name) (rate(envoy_cluster_upstream_rq_xx[5m])) > 0.01` |
| ingress-nginx | `sum by (ingress) (rate(nginx_ingress_controller_requests{status=~"5.."}[5m])) / sum by (ingress) (rate(nginx_ingress_controller_requests[5m])) > 0.01` |
| Traefik | `sum by (service) (rate(traefik_service_requests_total{code=~"5.."}[5m])) / sum by (service) (rate(traefik_service_requests_total[5m])) > 0.01` |
| OpenTelemetry HTTP server metrics | `sum by (service_name) (rate(http_server_request_duration_seconds_count{http_response_status_code=~"5.."}[5m])) / sum by (service_name) (rate(http_server_request_duration_seconds_count[5m])) > 0.01` |

Label names depend on how each is scraped (Linkerd's `deployment`, an
OpenTelemetry service's `service_name` or `job`): check them against your
own Prometheus first. Pick the threshold your users would notice, and
`--impact-for` long enough that a blip isn't an incident.

## Which of them your history has

`lapilli impact suggest` asks your Prometheus which of the stacks above
it has the metrics of, and how many windows each one's `--impact` finds
over the period:

```sh
lapilli impact suggest --prometheus http://prometheus:9090
```

```
Lapilli impact suggest  2026-10-26 00:00 → 2026-11-02 00:00 UTC
2 of the 6 stacks have their metrics in the history: SERIES, the most at once, and WINDOWS,
those its --impact finds, 5m or longer, each an incident nobody wrote down.

STACK          SERIES  WINDOWS
Istio          412     2
ingress-nginx  36      0

Not found: Linkerd, Envoy or Envoy Gateway, Traefik, OpenTelemetry HTTP server metrics.

Istio:
  --impact 'sum by (destination_service_name) (rate(istio_requests_total{reporter="destination", response_code=~"5.."}[5m])) / sum by (destination_service_name) (rate(istio_requests_total{reporter="destination"}[5m])) > 0.01'

ingress-nginx:
  --impact 'sum by (ingress) (rate(nginx_ingress_controller_requests{status=~"5.."}[5m])) / sum by (ingress) (rate(nginx_ingress_controller_requests[5m])) > 0.01'

Label names depend on how each is scraped, and the threshold on what your users notice:
check both before giving one to lapilli verdict (lapilli.dev/docs/impact).
```

- **SERIES** counts the series of the metric each query reads every
  request by (`istio_requests_total{reporter="destination"}`), not only
  the failures, at an hour's step: one request a stack. A stack whose
  metric has none is on the "Not found" line.
- **WINDOWS** are the windows of user impact its `--impact` finds over
  the period, as `lapilli verdict --impact` would, with `--impact-for` 5m:
  its 5xx share above 1% for five minutes or more. Read for the stacks
  found alone, a day a request.

It takes `lapilli verdict`'s flags for the history (`--prometheus`,
`--auth`, `--gcp-project`, `--bearer-token-file`, `--prometheus-tenant`,
`--max-rate` and the others) and the period (`--since`, `--from`, `--to`, `--step`, `--tz`),
and `--json` for the same as JSON. It chooses none of them: they are in
the table's order, whatever they found. It writes and sends nothing;
check the labels and the threshold, then give the one you choose to
`lapilli verdict`. A stack that couldn't be asked is "Not known", with
why, and the command exits 1.
