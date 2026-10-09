# What the comparison of a case's metrics lets by

`promdiff.py spoil`, as the sweep runs it on its own answers (`report-spoiled.md`), of the sweep
whose reports are beside this: every frozen answer spoiled in seventeen ways, and judged again
against the same two live ones. Eleven are of any answer. Six are of the answers that look back
further than the case reaches, and leave what such an answer says as it is: they change what is
under the words, where the eleven mostly spoil the words with it. Each table says first how many
answers were judged, unspoiled, to look back and be what they then have to be.

The 31 of the fixture's that pass with their series the other way round are each, all of it, one
call at an instant of `histogram_quantile` (25), `histogram_fraction` (5) or `count_values` (1):
what an engine keeps in a map and hands over in another order each time, the Prometheus's own two
askings among them (`api.md`). Nothing else of the 5,323 answers with two series or more passes
turned round.

On the scenario the three answers that look back are judged, and the six ways say little of
them: its Prometheus is some eight minutes old and has nothing before the case, so that its own
answer to each is what a case has left — nothing of the two instants, and of the window two
points, both the case's. A way that has nothing to change is not tried (`0 | 0`).

## The fixture: what `promq` prints

Unspoiled, 3 of the 20847 look back further than the case reaches and are what they then have to be.

| the frozen answer, spoiled | still passes | of |
|---|---|---|
| replaced by nothing | 0 | 20847 |
| replaced by an empty result | 0 | 19724 |
| its last line taken off | 0 | 20847 |
| its last line written twice | 0 | 20847 |
| the last digit of its last line another | 0 | 20847 |
| its lines the other way up | 0 | 5217 |
| its series the other way round, of the API | 0 | 0 |
| its last series taken out, of the API | 0 | 0 |
| the last digit of its last value another, of the API | 0 | 0 |
| answered with another status | 0 | 20847 |
| the words that it looked back put beside it | 0 | 20847 |
| what looks back: the Prometheus's point before its first put back | 0 | 1 |
| what looks back: its first point taken off | 0 | 1 |
| what looks back: its first series taken out | 0 | 1 |
| what looks back: its series the other way round | 0 | 1 |
| what looks back: answered with what the Prometheus has, where the case has nothing | 0 | 2 |
| what looks back: said of a beginning a second later | 0 | 3 |

## The fixture: what the API sends

Unspoiled, 3 of the 22147 look back further than the case reaches and are what they then have to be.

| the frozen answer, spoiled | still passes | of |
|---|---|---|
| replaced by nothing | 0 | 22147 |
| replaced by an empty result | 0 | 22147 |
| its last line taken off | 0 | 22147 |
| its last line written twice | 0 | 22147 |
| the last digit of its last line another | 0 | 22147 |
| its lines the other way up | 0 | 22147 |
| its series the other way round, of the API | 31 | 5323 |
  - `GET /api/v1/query?query=histogram_quantile(0.5, rate(fix_request_duration_seconds_bucket[1m]))&time=<freeze>`
  - `GET /api/v1/query?query=histogram_quantile(0, rate(fix_request_duration_seconds_bucket[5m]))&time=<freeze>`
  - `GET /api/v1/query?query=histogram_quantile(1, rate(fix_request_duration_seconds_bucket[5m]))&time=<freeze>`
  - `GET /api/v1/query?query=histogram_quantile(1.5, rate(fix_request_duration_seconds_bucket[5m]))&time=<freeze>`
  - `GET /api/v1/query?query=histogram_quantile(0.9, fix_request_duration_seconds_bucket)&time=<freeze>`
| its last series taken out, of the API | 0 | 20078 |
| the last digit of its last value another, of the API | 0 | 20078 |
| answered with another status | 0 | 22147 |
| the words that it looked back put beside it | 0 | 22147 |
| what looks back: the Prometheus's point before its first put back | 0 | 1 |
| what looks back: its first point taken off | 0 | 1 |
| what looks back: its first series taken out | 0 | 1 |
| what looks back: its series the other way round | 0 | 1 |
| what looks back: answered with what the Prometheus has, where the case has nothing | 0 | 2 |
| what looks back: said of a beginning a second later | 0 | 3 |

## The scenario: what `promq` prints

Unspoiled, 3 of the 724 look back further than the case reaches and are what they then have to be.

| the frozen answer, spoiled | still passes | of |
|---|---|---|
| replaced by nothing | 0 | 724 |
| replaced by an empty result | 0 | 636 |
| its last line taken off | 0 | 724 |
| its last line written twice | 0 | 724 |
| the last digit of its last line another | 0 | 724 |
| its lines the other way up | 0 | 129 |
| its series the other way round, of the API | 0 | 0 |
| its last series taken out, of the API | 0 | 0 |
| the last digit of its last value another, of the API | 0 | 0 |
| answered with another status | 0 | 724 |
| the words that it looked back put beside it | 0 | 724 |
| what looks back: the Prometheus's point before its first put back | 0 | 0 |
| what looks back: its first point taken off | 0 | 1 |
| what looks back: its first series taken out | 0 | 1 |
| what looks back: its series the other way round | 0 | 0 |
| what looks back: answered with what the Prometheus has, where the case has nothing | 0 | 0 |
| what looks back: said of a beginning a second later | 0 | 3 |

## The scenario: what the API sends

Unspoiled, 3 of the 859 look back further than the case reaches and are what they then have to be.

| the frozen answer, spoiled | still passes | of |
|---|---|---|
| replaced by nothing | 0 | 859 |
| replaced by an empty result | 0 | 859 |
| its last line taken off | 0 | 859 |
| its last line written twice | 0 | 859 |
| the last digit of its last line another | 0 | 859 |
| its lines the other way up | 0 | 859 |
| its series the other way round, of the API | 0 | 126 |
| its last series taken out, of the API | 0 | 633 |
| the last digit of its last value another, of the API | 0 | 633 |
| answered with another status | 0 | 859 |
| the words that it looked back put beside it | 0 | 859 |
| what looks back: the Prometheus's point before its first put back | 0 | 0 |
| what looks back: its first point taken off | 0 | 1 |
| what looks back: its first series taken out | 0 | 1 |
| what looks back: its series the other way round | 0 | 0 |
| what looks back: answered with what the Prometheus has, where the case has nothing | 0 | 0 |
| what looks back: said of a beginning a second later | 0 | 3 |
