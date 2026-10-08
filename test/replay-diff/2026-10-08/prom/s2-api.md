requests: 833 | the same: 829 | the same series and values in another order: 0 | the Prometheus's own answer changed between its two askings: 0 | look back further than the case's metrics reach, as they are meant to, and are the Prometheus's answer with that much taken off: 3 | both fail, worded differently: 1 | got no answer at all from one side or the other: 0 | **differ: 0**

Of the 154 that a recorded agent had typed: the same 154, the same series and values in another order 0, the Prometheus's own answer changed between its two askings 0, look back further than the case's metrics reach, as they are meant to, and are the Prometheus's answer with that much taken off 0, both fail, worded differently 0, got no answer at all from one side or the other 0, differ 0.

Of the 833, the Prometheus answered 685 with something, 107 with an empty result, and refused 41.

### Look back further than the case's metrics reach, as they are meant to, and are the Prometheus's answer with that much taken off

- `GET /api/v1/query?query=up offset 2h&time=<freeze>`
  - frozen case: it holds no samples before 2026-10-08T14:48:07.665Z, and this query looks 1h0m0s further back than that: its Prometheus may have had more to answer from
- `GET /api/v1/query?query=up offset 90m&time=<freeze>`
  - frozen case: it holds no samples before 2026-10-08T14:48:07.665Z, and this query looks 30m0s further back than that: its Prometheus may have had more to answer from
- `GET /api/v1/query_range?query=up&start=<freeze>-10800&end=<freeze>&step=10m`
  - frozen case: it holds no samples before 2026-10-08T14:48:07.665Z, and this query looks 2h0m0s further back than that: its points before 2026-10-08T14:53:07.665Z may be missing, or come of less than its Prometheus had

### Both fail, worded differently

- `GET /api/v1/no_such_endpoint` (known: a path a Prometheus does not have, it answers as any web server does; a frozen store answers every path under /api/v1/ it does not serve in the API's own shape, saying so, because most such paths are ones a Prometheus does have)
  - line 1 of 1 and 5: live `…404 page not found` | frozen `…{`

Differing and not excused: 0.
