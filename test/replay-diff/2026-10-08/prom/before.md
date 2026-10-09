# The metrics fixture, the first time

`sweep.sh` on `test/replay-diff/prom`, with `lapilli-case` as it was on `main` before this fixture
existed (e146f14) and the fixture's Prometheus at 3.5.0, the scenarios' version: what differed, by
what was asked, how many of each, and the first of each. The reports themselves list every one —
340 kB — and are not kept; this is their summary, and `promq.md` and `api.md` beside it are the
reports after the repairs, whole.

`<metric>` stands for a metric's name, `{…}` for a selector's braces, `(…)` for the labels of a `by`.

## What `promq` prints

18129 asked: the same 17731, the same series and values in another order 0, the Prometheus's own answer changed between its two askings 0, look back further than the case's metrics reach, which the case says 0, both fail, worded differently 1, differ 397.

| | how many | asked | the first of them |
|---|---|---|---|
| differ | 320 | `promq 'first_over_time(<metric>[5m])'` | line 1 of 1 and 4: live `…query failed: invalid parameter "query": 1:1: parse error: u` ¦ frozen `…ALERTS{alertname=FixAlwaysFiring,alertstate=firing,severity=` |
| differ | 25 | `promq 'sum(<metric>)' --range 30m --step 1m` | line 1 of 1 and 1: live `…{} 10:06:57=3 10:07:57=3 10:08:57=3 10:09:57=3 10:10:57=3 10:11:57=` ¦ frozen `…{} 10:07:57=3 10:08:57=3 10:09:57=3 10:10:57=3 10:11:57=3 10:12:57=` |
| differ | 25 | `promq 'increase(<metric>[5m])' --range 1h --step 5m` | line 1 of 3 and 3: live `….promfix.svc:8080,job=fix,target=a,version=1.2.3,zone=east} 09:36:57=0 09:41:57=0 09:46:57=0 09:51:57=0 09:56:57=0 10:01` ¦ frozen `….promfix.svc:8080,job=fix,target=a,version=1.2.3,zone=east} 10:11:57=0  |
| differ | 9 | `promq 'histogram_fraction(0, 0.1, rate(<metric>[5m]))'` | line 1 of 9 and 9: live `…{instance=exp-a.promfix.svc:8080,job=fix,path=/a,zone=east} NaN` ¦ frozen `…{instance=exp-a.promfix.svc:8080,job=fix,path=/a,zone=east} 0.7134767837` |
| differ | 9 | `promq 'histogram_fraction(0, 0.1, sum by (…) (rate(<metric>[5m])))'` | line 1 of 1 and 1: live `…{} NaN` ¦ frozen `…{} 0.6189574481` |
| differ | 4 | `promq 'histogram_fraction(0.05, 2, rate(<metric>[5m]))' --range 10m --step 1m` | line 1 of 1 and 1: live `…{instance=localhost:9090,job=prometheus} 10:33:57=NaN 10:34:57=NaN 10:35:57=NaN 10:36:57=NaN` ¦ frozen `…{instance=localhost:9090,job=prometheus} 10:33:57=0.004096451569 10:34:57=0.008404932406 10:35:57=0 |
| differ | 2 | `promq 'stdvar_over_time(<metric>[5m])'` | line 1 of 1 and 1: live `…localhost:9090,job=prometheus,listener_name=http} 13754.27438` ¦ frozen `…localhost:9090,job=prometheus,listener_name=http} 13754.27437` |
| differ | 1 | `promq <metric> --range 1h` | line 1 of 5 and 5: live `…up{instance=exp-a.promfix.svc:8080,job=fix,zone=east} 09:36:57=1 09:37:12=1 09:37:27=1 09:37:42=1 09:37:57=1 09:38` ¦ frozen `…up{instance=exp-a.promfix.svc:8080,job=fix,zone=east} 10:07:12=1 10:07:27=1 1 |
| differ | 1 | `promq <metric> --range 30m --step 5s` | line 1 of 5 and 5: live `…up{instance=exp-a.promfix.svc:8080,job=fix,zone=east} 10:06:57=1 10:07:02=1 10:07:07=1 10:07:12=1 10:07:17=1 10:07:22=` ¦ frozen `…up{instance=exp-a.promfix.svc:8080,job=fix,zone=east} 10:07:02=1 10:07:07 |
| differ | 1 | `promq '<metric> offset 2h'` | line 1 of 3 and 1: live `…up{instance=exp-a.promfix.svc:8080,job=fix,zone=east} 1` ¦ frozen `…(empty result)` |
| both fail, worded differently | 1 | `promq '<metric> offset'` | line 1 of 1 and 1: live `…se error: unexpected end of input in offset, expected number or duration` ¦ frozen `…se error: unexpected end of input in offset, expected number, duration, step(), or range()` |

## What the API sends

18938 asked: the same 17981, the same series and values in another order 12, the Prometheus's own answer changed between its two askings 1, look back further than the case's metrics reach, which the case says 0, both fail, worded differently 2, differ 942.

| | how many | asked | the first of them |
|---|---|---|---|
| differ | 320 | `GET /api/v1/query?query=first_over_time(<metric>[5m])&time=<freeze>` | line 2 of 5 and 67: live `… "error": "invalid parameter \"query\": 1:1: parse error: unkn` ¦ frozen `… "data": {` |
| differ | 258 | `GET /api/v1/metadata?metric=<metric>` | line 2 of 12 and 4: live `… "data": {` ¦ frozen `… "data": {},` |
| differ | 31 | `GET /api/v1/query?query=count_over_time(<metric>[2m])&time=<freeze>` | line 8 of 236 and 236: live `…     "op": "read",` ¦ frozen `…     "op": "idle",` |
| differ | 31 | `GET /api/v1/query?query=rate(<metric>[1m])&time=<freeze>` | line 8 of 239 and 239: live `…     "op": "read",` ¦ frozen `…     "op": "idle",` |
| differ | 31 | `GET /api/v1/query?query=irate(<metric>[1m])&time=<freeze>` | line 8 of 236 and 236: live `…     "op": "read",` ¦ frozen `…     "op": "idle",` |
| differ | 31 | `GET /api/v1/query?query=delta(<metric>[2m])&time=<freeze>` | line 8 of 236 and 236: live `…     "op": "read",` ¦ frozen `…     "op": "idle",` |
| differ | 31 | `GET /api/v1/query?query=last_over_time(<metric>[1m])&time=<freeze>` | line 9 of 254 and 254: live `…     "op": "read",` ¦ frozen `…     "op": "idle",` |
| differ | 31 | `GET /api/v1/query?query=sum_over_time(<metric>[2m])&time=<freeze>` | line 8 of 236 and 236: live `…     "op": "read",` ¦ frozen `…     "op": "idle",` |
| differ | 31 | `GET /api/v1/query?query=idelta(<metric>[1m])&time=<freeze>` | line 8 of 236 and 236: live `…     "op": "read",` ¦ frozen `…     "op": "idle",` |
| differ | 25 | `GET /api/v1/query_range?query=sum(<metric>)&start=<freeze>-1800&end=<freeze>&step=1m` | line 8 of 137 and 133: live `…      "#1791454017.909",` ¦ frozen `…      "#1791454077.909",` |
| differ | 25 | `GET /api/v1/query_range?query=increase(<metric>[5m])&start=<freeze>-3600&end=<freeze>&step=5m` | line 15 of 202 and 118: live `…      "#1791452217.909",` ¦ frozen `…      "#1791454317.909",` |
| differ | 22 | `GET /api/v1/query?query=deriv(<metric>[5m])&time=<freeze>` | line 131 of 236 and 236: live `…     "0.000015491565983539412"` ¦ frozen `…     "0.000015491565983539537"` |
| differ | 15 | `GET /api/v1/query?query=predict_linear(<metric>[5m], 600)&time=<freeze>` | line 378 of 1376 and 1376: live `…     "19308.86359208151"` ¦ frozen `…     "19308.863592081463"` |
| differ | 12 | `GET /api/v1/query?query=stdvar_over_time(<metric>[5m])&time=<freeze>` | line 12 of 40 and 40: live `…     "5029.0968093727815"` ¦ frozen `…     "5029.096809372781"` |
| differ | 10 | `GET /api/v1/query?query=stddev_over_time(<metric>[5m])&time=<freeze>` | line 12 of 40 and 40: live `…     "70.91612517173215"` ¦ frozen `…     "70.91612517173213"` |
| differ | 9 | `GET /api/v1/query?query=histogram_fraction(0, 0.1, rate(<metric>[5m]))&time=<freeze>` | line 6 of 113 and 113: live `…     "instance": "exp-a.promfix.svc:8080",` ¦ frozen `…     "instance": "exp-b.promfix.svc:8080",` |
| differ | 9 | `GET /api/v1/query?query=histogram_fraction(0, 0.1, sum by (…) (rate(<metric>[5m])))&time=<freeze>` | line 8 of 15 and 15: live `…     "NaN"` ¦ frozen `…     "0.6189574481334178"` |
| differ | 4 | `GET /api/v1/query_range?query=histogram_fraction(0.05, 2, rate(<metric>[5m]))&start=<freeze>-600&end=<freeze>&` | line 12 of 32 and 32: live `…      "NaN"` ¦ frozen `…      "0.0040964515687080425"` |
| differ | 1 | `GET /api/v1/query?query=histogram_quantile(0, rate(<metric>[5m]))&time=<freeze>` | line 6 of 116 and 116: live `…     "instance": "exp-a.promfix.svc:8080",` ¦ frozen `…     "instance": "exp-c.promfix.svc:8080",` |
| differ | 1 | `GET /api/v1/query?query=histogram_quantile(1, rate(<metric>[5m]))&time=<freeze>` | line 6 of 116 and 116: live `…     "instance": "exp-b.promfix.svc:8080",` ¦ frozen `…     "instance": "exp-a.promfix.svc:8080",` |
| differ | 1 | `GET /api/v1/query_range?query=<metric>&start=<freeze>-3600&end=<freeze>&step=15s` | line 13 of 3059 and 1607: live `…      "#1791452217.909",` ¦ frozen `…      "#1791454032.909",` |
| differ | 1 | `GET /api/v1/query_range?query=<metric>&start=<freeze>-1800&end=<freeze>&step=5s` | line 13 of 4707 and 4695: live `…      "#1791454017.909",` ¦ frozen `…      "#1791454022.909",` |
| differ | 1 | `GET /api/v1/query?query=<metric> offset 2h&time=<freeze>` | line 3 of 43 and 7: live `…  "result": [` ¦ frozen `…  "result": [],` |
| differ | 1 | `GET /api/v1/query?query=<metric> offset -1m&time=<freeze>` | line 19 of 65 and 65: live `…     "instance": "exp-gone.promfix.svc:8080",` ¦ frozen `…     "instance": "exp-b.promfix.svc:8080",` |
| both fail, worded differently | 1 | `GET /api/v1/query?query=<metric> offset&time=<freeze>` | line 2 of 5 and 5: live `…se error: unexpected end of input in offset, expected number or duration",` ¦ frozen `…se error: unexpected end of input in offset, expected number, duration, step(), or range()",` |
| both fail, worded differently | 1 | `GET /api/v1/no_such_endpoint` | line 1 of 1 and 5: live `…404 page not found` ¦ frozen `…{` |
| differ | 1 | `GET /api/v1/label/alertstate/values?start=<freeze>-1800&end=<freeze>` | line 3 of 7 and 6: live `…  "firing",` ¦ frozen `…  "firing"` |
| differ | 1 | `GET /api/v1/label/code/values?start=<freeze>-1800&end=<freeze>` | line 4 of 10 and 8: live `…  "400",` ¦ frozen `…  "500",` |
| differ | 1 | `GET /api/v1/metadata` | line 2 of 1841 and 4: live `… "data": {` ¦ frozen `… "data": {},` |
| differ | 1 | `GET /api/v1/metadata?metric=` | line 2 of 1841 and 4: live `… "data": {` ¦ frozen `… "data": {},` |
| differ | 1 | `GET /api/v1/metadata?limit=-1` | line 2 of 1841 and 4: live `… "data": {` ¦ frozen `… "data": {},` |
| differ | 1 | `GET /api/v1/metadata?limit=263` | line 2 of 1841 and 4: live `… "data": {` ¦ frozen `… "data": {},` |
| differ | 1 | `GET /api/v1/metadata?limit=some` | line 2 of 5 and 4: live `… "error": "limit must be a number",` ¦ frozen `… "data": {},` |
| differ | 1 | `GET /api/v1/metadata?limit_per_metric=0` | line 2 of 1841 and 4: live `… "data": {` ¦ frozen `… "data": {},` |
| differ | 1 | `GET /api/v1/metadata?limit_per_metric=50` | line 2 of 1841 and 4: live `… "data": {` ¦ frozen `… "data": {},` |
| differ | 1 | `GET /api/v1/metadata?limit_per_metric=few` | line 2 of 5 and 4: live `… "error": "limit_per_metric must be a number",` ¦ frozen `… "data": {},` |

