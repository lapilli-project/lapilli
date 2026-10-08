runs: 18 | judged by both: 18

| case | runs | pass (both judges) | judge 1 | judge 2 | all decisive evidence retrieved | mean steps | failed steps | `describe pod` steps a run | cost |
|---|---|---|---|---|---|---|---|---|---|
| s1-shared-cache-exhaustion | 6 | 5/6 | 5/6 | 5/6 | 5/6 | 10.0 | 0 | 0.3 | $0.04 |
| s2-periodic-saturation | 6 | 6/6 | 6/6 | 6/6 | 6/6 | 9.2 | 2 | 0.0 | $0.05 |
| s3-node-local-drift | 6 | 1/6 | 2/6 | 1/6 | 3/6 | 8.3 | 0 | 0.5 | $0.04 |
| all | 18 | 12/18 | 13/18 | 12/18 | 14/18 | 9.2 | 2 | 0.3 | $0.13 |

| case | evidence item | retrieved |
|---|---|---|
| s1-shared-cache-exhaustion | `CACHE_CONN_MODE` | 5/6 |
| s1-shared-cache-exhaustion | `conn-table active=40/40` | 6/6 |
| s1-shared-cache-exhaustion | `self\._held\.append` | 5/6 |
| s2-periodic-saturation | `BATCH_CONCURRENCY` | 6/6 |
| s2-periodic-saturation | `client\W{1,4}catalog-indexer[\s\S]{0,80}?\d` | 6/6 |
| s2-periodic-saturation | `thumbnail request failed` | 6/6 |
| s3-node-local-drift | `Too many open files` | 6/6 |
| s3-node-local-drift | `max_open_files=64` | 3/6 |

The two judges: 18 packets, agree on 94%, Cohen's kappa 0.87.
Disagreements: `s3-node-local-drift/frozen-claude-code-haiku-1` (judge 1 PASS, judge 2 FAIL)

Refused by the guard:

- 1× --raw asks for a path, and a path can be a proxy to a node, a pod or a service: a request 

Steps that failed otherwise, by their last line:

- 2× `Permission to use Bash has been denied because Claude Code is running in don't ask mode. IMPORTANT: You *may* `

The prediction of Part 1: `kubectl describe pod` steps a run below 2.0. It is 0.28: the prediction held.
