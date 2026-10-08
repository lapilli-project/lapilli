requests: 22121 | the same: 22089 | the same series and values in another order: 22 | the Prometheus's own answer changed between its two askings: 6 | look back further than the case's metrics reach, as they are meant to, and are the Prometheus's answer with that much taken off: 3 | both fail, worded differently: 1 | got no answer at all from one side or the other: 0 | **differ: 0**

Of the 0 that a recorded agent had typed: the same 0, the same series and values in another order 0, the Prometheus's own answer changed between its two askings 0, look back further than the case's metrics reach, as they are meant to, and are the Prometheus's answer with that much taken off 0, both fail, worded differently 0, got no answer at all from one side or the other 0, differ 0.

Of the 22121, the Prometheus answered 20940 with something, 1135 with an empty result, and refused 46.

### Look back further than the case's metrics reach, as they are meant to, and are the Prometheus's answer with that much taken off

- `GET /api/v1/query?query=up offset 2h&time=<freeze>`
  - frozen case: it holds no samples before 2026-10-08T14:56:11.661Z, and this query looks 1h0m0s further back than that: its Prometheus may have had more to answer from
- `GET /api/v1/query?query=up offset 90m&time=<freeze>`
  - frozen case: it holds no samples before 2026-10-08T14:56:11.661Z, and this query looks 30m0s further back than that: its Prometheus may have had more to answer from
- `GET /api/v1/query_range?query=up&start=<freeze>-10800&end=<freeze>&step=10m`
  - frozen case: it holds no samples before 2026-10-08T14:56:11.661Z, and this query looks 2h0m0s further back than that: its points before 2026-10-08T15:01:11.661Z may be missing, or come of less than its Prometheus had

### Both fail, worded differently

- `GET /api/v1/no_such_endpoint` (known: a path a Prometheus does not have, it answers as any web server does; a frozen store answers every path under /api/v1/ it does not serve in the API's own shape, saying so, because most such paths are ones a Prometheus does have)
  - line 1 of 1 and 5: live `…404 page not found` | frozen `…{`

### The same series and values in another order

- `GET /api/v1/query?query=histogram_quantile(0.5, rate(fix_request_duration_seconds_bucket[1m]))&time=<freeze>`
  - line 6 of 113 and 113: live `…     "instance": "exp-b.promfix.svc:8080",` | frozen `…     "instance": "exp-a.promfix.svc:8080",`
- `GET /api/v1/query?query=histogram_quantile(0, rate(fix_request_duration_seconds_bucket[5m]))&time=<freeze>`
  - line 6 of 113 and 113: live `…     "instance": "exp-a.promfix.svc:8080",` | frozen `…     "instance": "exp-b.promfix.svc:8080",`
- `GET /api/v1/query?query=histogram_quantile(1, rate(fix_request_duration_seconds_bucket[5m]))&time=<freeze>`
  - line 6 of 113 and 113: live `…     "instance": "exp-c.promfix.svc:8080",` | frozen `…     "instance": "exp-a.promfix.svc:8080",`
- `GET /api/v1/query?query=histogram_quantile(1.5, rate(fix_request_duration_seconds_bucket[5m]))&time=<freeze>`
  - line 18 of 116 and 116: live `…     "instance": "exp-b.promfix.svc:8080",` | frozen `…     "instance": "exp-c.promfix.svc:8080",`
- `GET /api/v1/query?query=histogram_quantile(0.9, fix_request_duration_seconds_bucket)&time=<freeze>`
  - line 6 of 113 and 113: live `…     "instance": "exp-b.promfix.svc:8080",` | frozen `…     "instance": "exp-c.promfix.svc:8080",`
- `GET /api/v1/query?query=histogram_fraction(0, 0.1, rate(fix_request_duration_seconds_bucket[5m]))&time=<freeze>`
  - line 8 of 113 and 113: live `…     "path": "/a",` | frozen `…     "path": "/b",`
- `GET /api/v1/query?query=histogram_quantile(0.5, rate(prometheus_engine_query_duration_histogram_seconds_bucket[1m]))&time=<freeze>`
  - line 8 of 52 and 52: live `…     "slice": "inner_eval"` | frozen `…     "slice": "queue_time"`
- `GET /api/v1/query?query=histogram_quantile(1, rate(prometheus_engine_query_duration_histogram_seconds_bucket[5m]))&time=<freeze>`
  - line 8 of 52 and 52: live `…     "slice": "inner_eval"` | frozen `…     "slice": "prepare_time"`
- `GET /api/v1/query?query=histogram_quantile(0.5, rate(prometheus_http_request_duration_seconds_bucket[1m]))&time=<freeze>`
  - line 6 of 85 and 85: live `…     "handler": "/-/ready",` | frozen `…     "handler": "/api/v1/query",`
- `GET /api/v1/query?query=histogram_quantile(0, rate(prometheus_http_request_duration_seconds_bucket[5m]))&time=<freeze>`
  - line 6 of 85 and 85: live `…     "handler": "/api/v1/labels",` | frozen `…     "handler": "/metrics",`
- `GET /api/v1/query?query=histogram_quantile(1, rate(prometheus_http_request_duration_seconds_bucket[5m]))&time=<freeze>`
  - line 6 of 85 and 85: live `…     "handler": "/api/v1/query",` | frozen `…     "handler": "/metrics",`
- `GET /api/v1/query?query=histogram_quantile(0.9, prometheus_http_request_duration_seconds_bucket)&time=<freeze>`
  - line 6 of 85 and 85: live `…     "handler": "/api/v1/metadata",` | frozen `…     "handler": "/api/v1/query",`
- `GET /api/v1/query?query=histogram_quantile(0.5, rate(prometheus_http_response_size_bytes_bucket[1m]))&time=<freeze>`
  - line 6 of 85 and 85: live `…     "handler": "/api/v1/series",` | frozen `…     "handler": "/api/v1/metadata",`
- `GET /api/v1/query?query=histogram_quantile(0, rate(prometheus_http_response_size_bytes_bucket[5m]))&time=<freeze>`
  - line 6 of 85 and 85: live `…     "handler": "/api/v1/labels",` | frozen `…     "handler": "/api/v1/query",`
- `GET /api/v1/query?query=histogram_quantile(1, rate(prometheus_http_response_size_bytes_bucket[5m]))&time=<freeze>`
  - line 6 of 85 and 85: live `…     "handler": "/-/ready",` | frozen `…     "handler": "/api/v1/labels",`
- `GET /api/v1/query?query=histogram_quantile(1.5, rate(prometheus_http_response_size_bytes_bucket[5m]))&time=<freeze>`
  - line 6 of 88 and 88: live `…     "handler": "/api/v1/labels",` | frozen `…     "handler": "/api/v1/series",`
- `GET /api/v1/query?query=histogram_quantile(0.9, prometheus_http_response_size_bytes_bucket)&time=<freeze>`
  - line 6 of 85 and 85: live `…     "handler": "/api/v1/query",` | frozen `…     "handler": "/api/v1/metadata",`
- `GET /api/v1/query?query=histogram_fraction(0, 0.1, rate(prometheus_http_response_size_bytes_bucket[5m]))&time=<freeze>`
  - line 6 of 85 and 85: live `…     "handler": "/api/v1/query",` | frozen `…     "handler": "/api/v1/label/:name/values",`
- `GET /api/v1/query?query=histogram_quantile(0, rate(prometheus_target_sync_length_histogram_seconds_bucket[5m]))&time=<freeze>`
  - line 8 of 52 and 52: live `…     "scrape_job": "other"` | frozen `…     "scrape_job": "fix"`
- `GET /api/v1/query?query=histogram_quantile(0.9, prometheus_target_sync_length_histogram_seconds_bucket)&time=<freeze>`
  - line 8 of 52 and 52: live `…     "scrape_job": "fix"` | frozen `…     "scrape_job": "prometheus"`
- `GET /api/v1/query?query=histogram_fraction(0, 0.1, rate(prometheus_target_sync_length_histogram_seconds_bucket[5m]))&time=<freeze>`
  - line 8 of 52 and 52: live `…     "scrape_job": "prometheus"` | frozen `…     "scrape_job": "fix"`
- `GET /api/v1/query?query=count_values("value", up)&time=<freeze>`
  - line 6 of 26 and 26: live `…     "value": "0"` | frozen `…     "value": "1"`

### The Prometheus's own answer changed between its two askings

- `GET /api/v1/query?query=histogram_quantile(0, rate(prometheus_engine_query_duration_histogram_seconds_bucket[5m]))&time=<freeze>`
  - line 8 of 52 and 52: live `…     "slice": "queue_time"` | frozen `…     "slice": "inner_eval"`
- `GET /api/v1/query?query=histogram_quantile(0.9, prometheus_engine_query_duration_histogram_seconds_bucket)&time=<freeze>`
  - line 8 of 52 and 52: live `…     "slice": "queue_time"` | frozen `…     "slice": "inner_eval"`
- `GET /api/v1/query?query=histogram_fraction(0, 0.1, rate(prometheus_engine_query_duration_histogram_seconds_bucket[5m]))&time=<freeze>`
  - line 8 of 52 and 52: live `…     "slice": "prepare_time"` | frozen `…     "slice": "inner_eval"`
- `GET /api/v1/query?query=histogram_quantile(0.5, rate(prometheus_target_sync_length_histogram_seconds_bucket[1m]))&time=<freeze>`
  - line 8 of 52 and 52: live `…     "scrape_job": "other"` | frozen `…     "scrape_job": "fix"`
- `GET /api/v1/query?query=histogram_quantile(1, rate(prometheus_target_sync_length_histogram_seconds_bucket[5m]))&time=<freeze>`
  - line 8 of 52 and 52: live `…     "scrape_job": "fix"` | frozen `…     "scrape_job": "gone"`
- `GET /api/v1/query?query=histogram_quantile(1.5, rate(prometheus_target_sync_length_histogram_seconds_bucket[5m]))&time=<freeze>`
  - line 8 of 55 and 55: live `…     "scrape_job": "gone"` | frozen `…     "scrape_job": "fix"`

Differing and not excused: 0.
