queries: 718 | the same: 715 | the same series and values in another order: 0 | the Prometheus's own answer changed between its two askings: 0 | look back further than the case's metrics reach, as they are meant to, and are the Prometheus's answer with that much taken off: 3 | both fail, worded differently: 0 | got no answer at all from one side or the other: 0 | **differ: 0**

Of the 130 that a recorded agent had typed: the same 130, the same series and values in another order 0, the Prometheus's own answer changed between its two askings 0, look back further than the case's metrics reach, as they are meant to, and are the Prometheus's answer with that much taken off 0, both fail, worded differently 0, got no answer at all from one side or the other 0, differ 0.

Of the 718, the Prometheus answered 613 with something, 90 with an empty result, and refused 15.

### Look back further than the case's metrics reach, as they are meant to, and are the Prometheus's answer with that much taken off

- `promq 'up offset 2h'`
  - frozen case: it holds no samples before 2026-10-08T14:48:07.665Z, and this query looks 1h0m0s further back than that: its Prometheus may have had more to answer from
- `promq 'up offset 90m'`
  - frozen case: it holds no samples before 2026-10-08T14:48:07.665Z, and this query looks 30m0s further back than that: its Prometheus may have had more to answer from
- `promq up --range 3h --step 10m`
  - frozen case: it holds no samples before 2026-10-08T14:48:07.665Z, and this query looks 2h0m0s further back than that: its points before 2026-10-08T14:53:07.665Z may be missing, or come of less than its Prometheus had

### As an agent asks: no instant named

The frozen store answered 718 of the 718 the same with no instant named as about its freeze by name.

Differing and not excused: 0.
