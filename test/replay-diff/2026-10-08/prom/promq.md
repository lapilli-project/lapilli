queries: 20847 | the same: 20844 | the same series and values in another order: 0 | the Prometheus's own answer changed between its two askings: 0 | look back further than the case's metrics reach, as they are meant to, and are the Prometheus's answer with that much taken off: 3 | both fail, worded differently: 0 | got no answer at all from one side or the other: 0 | **differ: 0**

Of the 0 that a recorded agent had typed: the same 0, the same series and values in another order 0, the Prometheus's own answer changed between its two askings 0, look back further than the case's metrics reach, as they are meant to, and are the Prometheus's answer with that much taken off 0, both fail, worded differently 0, got no answer at all from one side or the other 0, differ 0.

Of the 20847, the Prometheus answered 19704 with something, 1123 with an empty result, and refused 20.

### Look back further than the case's metrics reach, as they are meant to, and are the Prometheus's answer with that much taken off

- `promq 'up offset 2h'`
  - frozen case: it holds no samples before 2026-10-08T22:38:15.526Z, and this query looks 1h0m0s further back than that: its Prometheus may have had more to answer from
- `promq 'up offset 90m'`
  - frozen case: it holds no samples before 2026-10-08T22:38:15.526Z, and this query looks 30m0s further back than that: its Prometheus may have had more to answer from
- `promq up --range 3h --step 10m`
  - frozen case: it holds no samples before 2026-10-08T22:38:15.526Z, and this query looks 2h0m0s further back than that: its points before 2026-10-08T22:43:15.526Z may be missing, or come of less than its Prometheus had

### As an agent asks: no instant named

The frozen store answered 20847 of the 20847 the same with no instant named as about its freeze by name.

Differing and not excused: 0.
