# The two engines, over one Prometheus's samples

`PROM_IMAGE=prom/prometheus:v3.5.0 sweep.sh` on `test/replay-diff/prom`, with `lapilli-case` as it
is after the repairs: the fixture's Prometheus at 3.5.0, which the scenario's was until 2026-10-08,
and the frozen copy evaluated by the engine this tool links, which is 3.15's. What still differs
is what the two engines answer otherwise: on 3.15.0, where both are one engine, none of it does
(`promq.md`, `api.md`). Summarised as `before.md` is — by what was asked, how many of each, and
the first.

Of 18,137 queries 343, and of 19,287 requests 493: `first_over_time`, which the older does not
have, of each of 320 metrics; `histogram_fraction` over classic buckets, `NaN` from the older and
a number from this one, 22 times; what a parser says it expected after `up offset`; and the last
digits of `deriv`, `stdvar_over_time`, `predict_linear` and `stddev_over_time`, in 150 of what the
API sends — 120 of them `deriv`, which parts here from the eleventh digit on — and in none of what
`promq` prints, which is ten digits. How many answers part in their last digits changes with the
samples a run happens to freeze: other runs of the same had 36, 42, 46 and 51 of them, and one
had two that showed in what `promq` prints. (The path no Prometheus has is the one line of
`known-promq.txt`.)

## What `promq` prints

18137 asked: the same 17791, the same series and values in another order 0, the Prometheus's own answer changed between its two askings 0, look back further than the case's metrics reach, as they are meant to, and are the Prometheus's answer with that much taken off 3, both fail, worded differently 1, got no answer at all from one side or the other 0, differ 342.

| | how many | asked | the first of them |
|---|---|---|---|
| differ | 320 | `promq 'first_over_time(<metric>[5m])'` | line 1 of 1 and 4: live `…query failed: invalid parameter "query": 1:1: parse error: u` ¦ frozen `…ALERTS{alertname=FixAlwaysFiring,alertstate=firing,severity=` |
| differ | 9 | `promq 'histogram_fraction(0, 0.1, rate(<metric>[5m]))'` | line 1 of 9 and 9: live `…{instance=exp-a.promfix.svc:8080,job=fix,path=/a,zone=east} NaN` ¦ frozen `…{instance=exp-a.promfix.svc:8080,job=fix,path=/a,zone=east} 0.7129943503` |
| differ | 9 | `promq 'histogram_fraction(0, 0.1, sum by (…) (rate(<metric>[5m])))'` | line 1 of 1 and 1: live `…{} NaN` ¦ frozen `…{} 0.6188691029` |
| differ | 4 | `promq 'histogram_fraction(0.05, 2, rate(<metric>[5m]))' --range 10m --step 1m` | line 1 of 1 and 1: live `…{instance=localhost:9090,job=prometheus} 23:50:20=NaN 23:51:20=NaN 23:52:20=NaN 23:53:20=NaN 23:54:20=NaN 23:5` ¦ frozen `…{instance=localhost:9090,job=prometheus} 23:50:20=0.002275534475 23:51:20=0.00821 |
| both fail, worded differently | 1 | `promq '<metric> offset'` | line 1 of 1 and 1: live `…se error: unexpected end of input in offset, expected number or duration` ¦ frozen `…se error: unexpected end of input in offset, expected number, duration, step(), or range()` |

## What the API sends

19287 asked: the same 18776, the same series and values in another order 13, the Prometheus's own answer changed between its two askings 1, look back further than the case's metrics reach, as they are meant to, and are the Prometheus's answer with that much taken off 3, both fail, worded differently 2, got no answer at all from one side or the other 0, differ 492.

| | how many | asked | the first of them |
|---|---|---|---|
| differ | 320 | `GET /api/v1/query?query=first_over_time(<metric>[5m])&time=<freeze>` | line 2 of 5 and 67: live `… "error": "invalid parameter \"query\": 1:1: parse error: unkn` ¦ frozen `… "data": {` |
| differ | 120 | `GET /api/v1/query?query=deriv(<metric>[5m])&time=<freeze>` | line 914 of 1376 and 1376: live `…     "0.30900117480998585"` ¦ frozen `…     "0.30900117481005207"` |
| differ | 15 | `GET /api/v1/query?query=stdvar_over_time(<metric>[5m])&time=<freeze>` | line 10 of 35 and 35: live `…     "1.1679214672721026"` ¦ frozen `…     "1.1679214672721028"` |
| differ | 9 | `GET /api/v1/query?query=histogram_fraction(0, 0.1, rate(<metric>[5m]))&time=<freeze>` | line 8 of 113 and 113: live `…     "path": "/a"` ¦ frozen `…     "path": "/idle"` |
| differ | 9 | `GET /api/v1/query?query=histogram_fraction(0, 0.1, sum by (…) (rate(<metric>[5m])))&time=<freeze>` | line 8 of 15 and 15: live `…     "NaN"` ¦ frozen `…     "0.6188691029092686"` |
| differ | 9 | `GET /api/v1/query?query=predict_linear(<metric>[5m], 600)&time=<freeze>` | line 58 of 113 and 113: live `…     "5.5759430816361775"` ¦ frozen `…     "5.575943081636224"` |
| differ | 6 | `GET /api/v1/query?query=stddev_over_time(<metric>[5m])&time=<freeze>` | line 10 of 35 and 35: live `…     "1.080704153444458"` ¦ frozen `…     "1.0807041534444581"` |
| differ | 4 | `GET /api/v1/query_range?query=histogram_fraction(0.05, 2, rate(<metric>[5m]))&start=<freeze>-600&end=<freeze>&` | line 12 of 44 and 44: live `…      "NaN"` ¦ frozen `…      "0.0022755344753354373"` |
| both fail, worded differently | 1 | `GET /api/v1/query?query=<metric> offset&time=<freeze>` | line 2 of 5 and 5: live `…se error: unexpected end of input in offset, expected number or duration",` ¦ frozen `…se error: unexpected end of input in offset, expected number, duration, step(), or range()",` |
| both fail, worded differently | 1 | `GET /api/v1/no_such_endpoint` | line 1 of 1 and 5: live `…404 page not found` ¦ frozen `…{` |

