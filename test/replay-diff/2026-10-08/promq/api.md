requests: 725 | the same: 722 | the same series and values in another order: 0 | the Prometheus's own answer changed between its two askings: 0 | both fail, worded differently: 2 | **differ: 1**

Of the 154 that a recorded agent had typed: the same 153, the same series and values in another order 0, the Prometheus's own answer changed between its two askings 0, both fail, worded differently 0, differ 1.

Of the 725, the Prometheus answered 581 with something, 105 with an empty result, and refused 39.

### Differ

- `GET /api/v1/metadata?metric=thumb_requests_total` (known: a case does not carry what kind of metric each is, nor its help: a frozen store answers that it knows of none)
  - line 2 of 12 and 4: live `… "data": {` | frozen `… "data": {},`

### Both fail, worded differently

- `GET /api/v1/query?query=up offset&time=<freeze>` (known: the same refusal, as the API sends it)
  - line 2 of 5 and 5: live `…se error: unexpected end of input in offset, expected number or duration",` | frozen `…se error: unexpected end of input in offset, expected number, duration, step(), or range()",`
- `GET /api/v1/no_such_endpoint` (known: a path a Prometheus does not have, it answers as any web server does; a frozen store answers every path under /api/v1/ it does not serve in the API's own shape, saying so, because most such paths are ones a Prometheus does have)
  - line 1 of 1 and 5: live `…404 page not found` | frozen `…{`

Differing and not excused: 0.
