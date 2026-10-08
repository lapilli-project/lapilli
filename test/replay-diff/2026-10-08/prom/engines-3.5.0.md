# The two engines, over one Prometheus's samples

`PROM_IMAGE=prom/prometheus:v3.5.0 sweep.sh` on `test/replay-diff/prom`, with `lapilli-case` as it
is after the repairs: the fixture's Prometheus at 3.5.0, which the scenario's was until 2026-10-08,
and the frozen copy evaluated by the engine this tool links, which is 3.15's. What still differs
is what the two engines answer otherwise: on 3.15.0, where both are one engine, none of it does
(`promq.md`, `api.md`). Summarised as `before.md` is — by what was asked, how many of each, and
the first.

Of 18,131 queries 345, and of 19,261 requests 385: `first_over_time`, which the older does not
have, of each of 320 metrics; `histogram_fraction` over classic buckets, `NaN` from the older and
a number from this one, 22 times; what a parser says it expected after `up offset`; and the last
digits of `deriv`, `stdvar_over_time`, `stddev_over_time` and `predict_linear`, in 42 of what the
API sends and — past the ten digits `promq` prints, mostly — in two of what `promq` prints. Which
answers part in a last digit changes with the samples: other runs of the same had 36, 46 and 51
of them. (The path no Prometheus has is the one line of `known-promq.txt`.)

## What `promq` prints

18131 asked: the same 17783, the same series and values in another order 0, the Prometheus's own answer changed between its two askings 0, look back further than the case's metrics reach, as they are meant to, and are the Prometheus's answer with that much taken off 3, both fail, worded differently 1, got no answer at all from one side or the other 0, differ 344.

| | how many | asked | the first of them |
|---|---|---|---|
| differ | 320 | `promq 'first_over_time(<metric>[5m])'` | line 1 of 1 and 4: live `…query failed: invalid parameter "query": 1:1: parse error: u` ¦ frozen `…ALERTS{alertname=FixAlwaysFiring,alertstate=firing,severity=` |
| differ | 9 | `promq 'histogram_fraction(0, 0.1, rate(<metric>[5m]))'` | line 1 of 9 and 9: live `…{instance=exp-a.promfix.svc:8080,job=fix,path=/a,zone=east} NaN` ¦ frozen `…{instance=exp-a.promfix.svc:8080,job=fix,path=/a,zone=east} 0.7129943503` |
| differ | 9 | `promq 'histogram_fraction(0, 0.1, sum by (…) (rate(<metric>[5m])))'` | line 1 of 1 and 1: live `…{} NaN` ¦ frozen `…{} 0.6185567033` |
| differ | 4 | `promq 'histogram_fraction(0.05, 2, rate(<metric>[5m]))' --range 10m --step 1m` | line 1 of 1 and 1: live `…{instance=localhost:9090,job=prometheus} 16:10:43=NaN 16:11:43=NaN 16:12:43=NaN 16:13:43=NaN 16:14:43=NaN 16:1` ¦ frozen `…{instance=localhost:9090,job=prometheus} 16:10:43=0.00795403453 16:11:43=0.008373 |
| differ | 2 | `promq 'deriv(<metric>[5m])'` | line 1 of 1 and 1: live `…{instance=localhost:9090,job=prometheus} 1.000004194` ¦ frozen `…{instance=localhost:9090,job=prometheus} 1.000004198` |
| both fail, worded differently | 1 | `promq '<metric> offset'` | line 1 of 1 and 1: live `…se error: unexpected end of input in offset, expected number or duration` ¦ frozen `…se error: unexpected end of input in offset, expected number, duration, step(), or range()` |

## What the API sends

19261 asked: the same 18857, the same series and values in another order 14, the Prometheus's own answer changed between its two askings 1, look back further than the case's metrics reach, as they are meant to, and are the Prometheus's answer with that much taken off 3, both fail, worded differently 2, got no answer at all from one side or the other 0, differ 384.

| | how many | asked | the first of them |
|---|---|---|---|
| differ | 320 | `GET /api/v1/query?query=first_over_time(<metric>[5m])&time=<freeze>` | line 2 of 5 and 67: live `… "error": "invalid parameter \"query\": 1:1: parse error: unkn` ¦ frozen `… "data": {` |
| differ | 18 | `GET /api/v1/query?query=deriv(<metric>[5m])&time=<freeze>` | line 40 of 236 and 236: live `…     "0.00027150900452113576"` ¦ frozen `…     "0.00027150900452113603"` |
| differ | 10 | `GET /api/v1/query?query=stdvar_over_time(<metric>[5m])&time=<freeze>` | line 27 of 1376 and 1376: live `…     "932.2941666666561"` ¦ frozen `…     "932.294166666656"` |
| differ | 9 | `GET /api/v1/query?query=histogram_fraction(0, 0.1, rate(<metric>[5m]))&time=<freeze>` | line 13 of 113 and 113: live `…     "NaN"` ¦ frozen `…     "0.0472972972972973"` |
| differ | 9 | `GET /api/v1/query?query=histogram_fraction(0, 0.1, sum by (…) (rate(<metric>[5m])))&time=<freeze>` | line 8 of 15 and 15: live `…     "NaN"` ¦ frozen `…     "0.6185567033017809"` |
| differ | 7 | `GET /api/v1/query?query=predict_linear(<metric>[5m], 600)&time=<freeze>` | line 14 of 236 and 236: live `…     "0.0433443921425297"` ¦ frozen `…     "0.04334439214252967"` |
| differ | 7 | `GET /api/v1/query?query=stddev_over_time(<metric>[5m])&time=<freeze>` | line 27 of 1376 and 1376: live `…     "30.533492539613874"` ¦ frozen `…     "30.53349253961387"` |
| differ | 4 | `GET /api/v1/query_range?query=histogram_fraction(0.05, 2, rate(<metric>[5m]))&start=<freeze>-600&end=<freeze>&` | line 12 of 40 and 40: live `…      "NaN"` ¦ frozen `…      "0.00795403453030394"` |
| both fail, worded differently | 1 | `GET /api/v1/query?query=<metric> offset&time=<freeze>` | line 2 of 5 and 5: live `…se error: unexpected end of input in offset, expected number or duration",` ¦ frozen `…se error: unexpected end of input in offset, expected number, duration, step(), or range()",` |
| both fail, worded differently | 1 | `GET /api/v1/no_such_endpoint` | line 1 of 1 and 5: live `…404 page not found` ¦ frozen `…{` |

