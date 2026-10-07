# `signs.py` on round 38's records

Run before any run of round 39 existed, to see that the count finds what round 38 found.

## Agent A, frozen — `signs.py --only 'frozen-claude-code-*' <the three case directories of round 38>`

18 run records.

| what the step shows | steps | runs |
|---|---|---|
| an empty sorted list | 11 | 9/18 |
| a pod listing asked for `-o wide`, without NODE | 9 | 8/18 |
| a listing whose whole header is `NAME AGE` | 10 | 8/18 |
| an event listing headed `LASTTIMESTAMP` | 0 | 0/18 |
| `kubectl get all` with names and no kinds | 4 | 4/18 |
| **any of them** | 34 | 15/18 |

Each step:

- `test/fixtures/case-runs/2026-10-07-round38/s1-shared-cache-exhaustion/frozen-claude-code-haiku-1.json` step 0: a listing whose whole header is `NAME AGE` — `kubectl get all -n shop`
- `test/fixtures/case-runs/2026-10-07-round38/s1-shared-cache-exhaustion/frozen-claude-code-haiku-1.json` step 0: `kubectl get all` with names and no kinds — `kubectl get all -n shop`
- `test/fixtures/case-runs/2026-10-07-round38/s1-shared-cache-exhaustion/frozen-claude-code-haiku-3.json` step 0: a listing whose whole header is `NAME AGE` — `kubectl get all -n shop`
- `test/fixtures/case-runs/2026-10-07-round38/s1-shared-cache-exhaustion/frozen-claude-code-haiku-3.json` step 0: `kubectl get all` with names and no kinds — `kubectl get all -n shop`
- `test/fixtures/case-runs/2026-10-07-round38/s1-shared-cache-exhaustion/frozen-claude-code-haiku-3.json` step 8: an empty sorted list — `kubectl get events -n shop --sort-by='.lastTimestamp'`
- `test/fixtures/case-runs/2026-10-07-round38/s1-shared-cache-exhaustion/frozen-claude-code-haiku-4.json` step 14: a pod listing asked for `-o wide`, without NODE — `kubectl get pods -n shop -l app=report-worker -o wide`
- `test/fixtures/case-runs/2026-10-07-round38/s1-shared-cache-exhaustion/frozen-claude-code-haiku-5.json` step 2: an empty sorted list — `kubectl get events -n shop --sort-by='.lastTimestamp'`
- `test/fixtures/case-runs/2026-10-07-round38/s1-shared-cache-exhaustion/frozen-claude-code-haiku-6.json` step 2: an empty sorted list — `kubectl get events -n shop --sort-by='.lastTimestamp'`
- `test/fixtures/case-runs/2026-10-07-round38/s2-periodic-saturation/frozen-claude-code-haiku-1.json` step 11: a listing whose whole header is `NAME AGE` — `kubectl get endpoints -n media`
- `test/fixtures/case-runs/2026-10-07-round38/s2-periodic-saturation/frozen-claude-code-haiku-2.json` step 9: a pod listing asked for `-o wide`, without NODE — `kubectl get pods -n media -o wide`
- `test/fixtures/case-runs/2026-10-07-round38/s2-periodic-saturation/frozen-claude-code-haiku-4.json` step 1: an empty sorted list — `kubectl get events -n media --sort-by='.lastTimestamp'`
- `test/fixtures/case-runs/2026-10-07-round38/s2-periodic-saturation/frozen-claude-code-haiku-5.json` step 6: a listing whose whole header is `NAME AGE` — `kubectl get endpoints -n media`
- `test/fixtures/case-runs/2026-10-07-round38/s2-periodic-saturation/frozen-claude-code-haiku-6.json` step 3: an empty sorted list — `kubectl get events -n media --sort-by='.lastTimestamp' | tail -20`
- `test/fixtures/case-runs/2026-10-07-round38/s2-periodic-saturation/frozen-claude-code-haiku-6.json` step 14: a listing whose whole header is `NAME AGE` — `kubectl get all -n media`
- `test/fixtures/case-runs/2026-10-07-round38/s2-periodic-saturation/frozen-claude-code-haiku-6.json` step 14: `kubectl get all` with names and no kinds — `kubectl get all -n media`
- `test/fixtures/case-runs/2026-10-07-round38/s2-periodic-saturation/frozen-claude-code-haiku-6.json` step 21: a pod listing asked for `-o wide`, without NODE — `kubectl get pod -n media thumb-api-58bcf947d5-lfs9m -o wide`
- `test/fixtures/case-runs/2026-10-07-round38/s2-periodic-saturation/frozen-claude-code-haiku-6.json` step 21: a listing whose whole header is `NAME AGE` — `kubectl get pod -n media thumb-api-58bcf947d5-lfs9m -o wide`
- `test/fixtures/case-runs/2026-10-07-round38/s3-node-local-drift/frozen-claude-code-haiku-1.json` step 0: a pod listing asked for `-o wide`, without NODE — `kubectl get pods -n ledger -o wide`
- `test/fixtures/case-runs/2026-10-07-round38/s3-node-local-drift/frozen-claude-code-haiku-1.json` step 10: an empty sorted list — `kubectl get events -n ledger --sort-by='.lastTimestamp'`
- `test/fixtures/case-runs/2026-10-07-round38/s3-node-local-drift/frozen-claude-code-haiku-1.json` step 11: an empty sorted list — `kubectl get events -A --sort-by='.lastTimestamp' | grep -E "ledger|rcabench-worker2"`
- `test/fixtures/case-runs/2026-10-07-round38/s3-node-local-drift/frozen-claude-code-haiku-1.json` step 21: an empty sorted list — `kubectl get events -A --sort-by='.lastTimestamp' | grep ledger-api | head -30`
- `test/fixtures/case-runs/2026-10-07-round38/s3-node-local-drift/frozen-claude-code-haiku-3.json` step 0: a pod listing asked for `-o wide`, without NODE — `kubectl get pods -n ledger -o wide`
- `test/fixtures/case-runs/2026-10-07-round38/s3-node-local-drift/frozen-claude-code-haiku-3.json` step 17: a listing whose whole header is `NAME AGE` — `kubectl get replicaset -n ledger -o wide`
- `test/fixtures/case-runs/2026-10-07-round38/s3-node-local-drift/frozen-claude-code-haiku-4.json` step 5: a pod listing asked for `-o wide`, without NODE — `kubectl get pods -n ledger -l app=ledger-api -o wide`
- `test/fixtures/case-runs/2026-10-07-round38/s3-node-local-drift/frozen-claude-code-haiku-4.json` step 16: an empty sorted list — `kubectl get events -n ledger --sort-by='.lastTimestamp'`
- `test/fixtures/case-runs/2026-10-07-round38/s3-node-local-drift/frozen-claude-code-haiku-4.json` step 19: a listing whose whole header is `NAME AGE` — `kubectl get all -n ledger`
- `test/fixtures/case-runs/2026-10-07-round38/s3-node-local-drift/frozen-claude-code-haiku-4.json` step 19: `kubectl get all` with names and no kinds — `kubectl get all -n ledger`
- `test/fixtures/case-runs/2026-10-07-round38/s3-node-local-drift/frozen-claude-code-haiku-4.json` step 23: a listing whose whole header is `NAME AGE` — `kubectl get rs -n ledger --sort-by=.metadata.creationTimestamp`
- `test/fixtures/case-runs/2026-10-07-round38/s3-node-local-drift/frozen-claude-code-haiku-5.json` step 0: a pod listing asked for `-o wide`, without NODE — `kubectl get pods -n ledger -l app=ledger-api -o wide`
- `test/fixtures/case-runs/2026-10-07-round38/s3-node-local-drift/frozen-claude-code-haiku-5.json` step 3: an empty sorted list — `kubectl get events -n ledger --sort-by='.lastTimestamp' | grep -i ledger-api`
- `test/fixtures/case-runs/2026-10-07-round38/s3-node-local-drift/frozen-claude-code-haiku-6.json` step 0: a pod listing asked for `-o wide`, without NODE — `kubectl get pods -n ledger -o wide`
- `test/fixtures/case-runs/2026-10-07-round38/s3-node-local-drift/frozen-claude-code-haiku-6.json` step 1: a pod listing asked for `-o wide`, without NODE — `kubectl get pods -n ledger -l app=ledger-api -o wide`
- `test/fixtures/case-runs/2026-10-07-round38/s3-node-local-drift/frozen-claude-code-haiku-6.json` step 2: an empty sorted list — `kubectl get events -n ledger --sort-by='.lastTimestamp'`
- `test/fixtures/case-runs/2026-10-07-round38/s3-node-local-drift/frozen-claude-code-haiku-6.json` step 16: a listing whose whole header is `NAME AGE` — `kubectl get replicasets -n ledger -o wide`

## Agent A, live — `signs.py --only 'live-claude-code-*' …`

18 run records.

| what the step shows | steps | runs |
|---|---|---|
| an empty sorted list | 0 | 0/18 |
| a pod listing asked for `-o wide`, without NODE | 0 | 0/18 |
| a listing whose whole header is `NAME AGE` | 0 | 0/18 |
| an event listing headed `LASTTIMESTAMP` | 0 | 0/18 |
| `kubectl get all` with names and no kinds | 0 | 0/18 |
| **any of them** | 0 | 0/18 |
