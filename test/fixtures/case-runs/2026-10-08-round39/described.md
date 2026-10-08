runs: 18 | judged by both: 18

| case | runs | pass (both judges) | judge 1 | judge 2 | all decisive evidence retrieved | mean steps | failed steps | `describe pod` steps a run | cost |
|---|---|---|---|---|---|---|---|---|---|
| s1-shared-cache-exhaustion | 6 | 3/6 | 3/6 | 3/6 | 4/6 | 7.8 | 8 | 0.3 | $0.03 |
| s2-periodic-saturation | 6 | 4/6 | 4/6 | 4/6 | 5/6 | 9.8 | 5 | 0.0 | $0.05 |
| s3-node-local-drift | 6 | 0/6 | 0/6 | 0/6 | 0/6 | 2.5 | 15 | 0.0 | $0.01 |
| all | 18 | 7/18 | 7/18 | 7/18 | 9/18 | 6.7 | 28 | 0.1 | $0.08 |

| case | evidence item | retrieved |
|---|---|---|
| s1-shared-cache-exhaustion | `CACHE_CONN_MODE` | 4/6 |
| s1-shared-cache-exhaustion | `conn-table active=40/40` | 4/6 |
| s1-shared-cache-exhaustion | `self\._held\.append` | 4/6 |
| s2-periodic-saturation | `BATCH_CONCURRENCY` | 6/6 |
| s2-periodic-saturation | `client\W{1,4}catalog-indexer[\s\S]{0,80}?\d` | 5/6 |
| s2-periodic-saturation | `thumbnail request failed` | 6/6 |
| s3-node-local-drift | `Too many open files` | 0/6 |
| s3-node-local-drift | `max_open_files=64` | 0/6 |

The two judges: 18 packets, agree on 100%, Cohen's kappa 1.00.

Refused by the guard:

- nothing

Steps that failed otherwise, by their last line:

- 27× `Permission to use Bash has been denied because Claude Code is running in don't ask mode. IMPORTANT: You *may* `
- 1× `error: the server doesn't have a resource type "ciliumnetworkpolicy"`

The prediction of Part 1: `kubectl describe pod` steps a run below 2.0. It is 0.11: the prediction held.
