Post hoc: computed after the results were known. None of it is a rule of Part 1.

### Each decisive evidence item, retrieved in how many runs

| case | evidence pattern | claude-code live | claude-code frozen | holmes live | holmes frozen |
|---|---|---|---|---|---|
| s1-shared-cache-exhaustion | `CACHE_CONN_MODE` | 5/6 | 4/6 | 0/6 | 0/6 |
| s1-shared-cache-exhaustion | `conn-table active=40/40` | 6/6 | 6/6 | 6/6 | 6/6 |
| s1-shared-cache-exhaustion | `self\._held\.append` | 3/6 | 4/6 | 0/6 | 0/6 |
| s2-periodic-saturation | `BATCH_CONCURRENCY` | 6/6 | 5/6 | 0/6 | 0/6 |
| s2-periodic-saturation | `client\W{1,4}catalog-indexer[\s\S]{0,80}?\d` | 4/6 | 3/6 | 4/6 | 6/6 |
| s2-periodic-saturation | `thumbnail request failed` | 6/6 | 6/6 | 5/6 | 5/6 |
| s3-node-local-drift | `Too many open files` | 6/6 | 6/6 | 6/6 | 6/6 |
| s3-node-local-drift | `max_open_files=64` | 0/6 | 1/6 | 0/6 | 0/6 |
| **all items** | | 36/48 | 35/48 | 21/48 | 23/48 |

### Each statement of the key, conveyed in how many answers

Live and frozen together, twelve answers a cell. `expected` statements in the order of `case.yaml`; then answers that did a `must_not`.

| case | agent | judge 1: expected | judge 1: must_not done | judge 2: expected | judge 2: must_not done |
|---|---|---|---|---|---|
| s1-shared-cache-exhaustion | claude-code | 12/12 · 6/12 · 4/12 | 1/12 | 12/12 · 6/12 · 4/12 | 0/12 |
| s1-shared-cache-exhaustion | holmes | 12/12 · 0/12 · 0/12 | 0/12 | 12/12 · 0/12 · 0/12 | 0/12 |
| s2-periodic-saturation | claude-code | 9/12 · 11/12 · 7/12 | 0/12 | 9/12 · 11/12 · 11/12 | 0/12 |
| s2-periodic-saturation | holmes | 11/12 · 0/12 · 10/12 | 1/12 | 11/12 · 0/12 · 10/12 | 1/12 |
| s3-node-local-drift | claude-code | 12/12 · 1/12 | 8/12 | 12/12 · 1/12 | 10/12 |
| s3-node-local-drift | holmes | 12/12 · 0/12 | 10/12 | 12/12 · 0/12 | 10/12 |

Answers that did a `must_not` according to either judge, by condition:

| case | claude-code live | claude-code frozen | holmes live | holmes frozen |
|---|---|---|---|---|
| s1-shared-cache-exhaustion | 0/6 | 1/6 | 0/6 | 0/6 |
| s2-periodic-saturation | 0/6 | 0/6 | 1/6 | 0/6 |
| s3-node-local-drift | 5/6 | 5/6 | 5/6 | 5/6 |

### R3, for each judge alone

| judge | passes with all decisive evidence | passes without |
|---|---|---|
| judge 1 | 10 | 0 |
| judge 2 | 10 | 3 |

Where the judges disagree:

- `s2-periodic-saturation/frozen-claude-code-haiku-1` — not retrieved: `client\W{1,4}catalog-indexer[\s\S]{0,80}?\d`
  - judge 1 FAIL [True, True, False]: The ~90.3% share is built from configured rates ('Web-frontend: 2 RPS x 40s = 80 requests') on the stated basis of 'logs, ConfigMaps, and pod configurations', so it is not supported by a number taken from metrics.
  - judge 2 PASS [True, True, True]: The answer identifies catalog-indexer bursts every 120s for ~40s at concurrency 30 exhausting thumb-api's 4 render workers and causing 503s, and quantifies catalog-indexer's burst share as ~90.3% while ruling out email-renderer/image-proxy.
- `s2-periodic-saturation/frozen-claude-code-haiku-3` — not retrieved: `client\W{1,4}catalog-indexer[\s\S]{0,80}?\d`
  - judge 1 FAIL [True, True, False]: The ~89.5% share is computed from a capacity model ('4 workers × (1/0.15s per request) = 26.67 RPS') and log warning counts, not a number taken from metrics.
  - judge 2 PASS [True, True, True]: The answer identifies catalog-indexer as flooding thumb-api every 120 seconds for ~40 seconds with concurrency 30, exhausting 4 workers and causing 503s, and quantifies its burst share as “~89-90%” while ruling out image-proxy/email-renderer.
- `s2-periodic-saturation/live-claude-code-haiku-1` — not retrieved: `client\W{1,4}catalog-indexer[\s\S]{0,80}?\d`
  - judge 1 FAIL [True, True, False]: The '~88% of successful requests' share is derived from a capacity calculation ('23.47 ÷ 26.67'), not a number taken from metrics.
  - judge 2 PASS [True, True, True]: The answer identifies saturation of 4 render workers causing 503s, names catalog-indexer with 120s/40s/concurrency 30 burst settings, and quantifies its burst share as '~88% of successful requests' while ruling out image-proxy and email-renderer.

Statement by statement, the two judges differ on 7 of 312 rulings: s1-shared-cache-exhaustion, must_not 2: 1, judge 1 the stricter; s2-periodic-saturation, expected 3: 4, judge 1 the stricter; s3-node-local-drift, must_not 1: 2, judge 2 the stricter.

### `kubectl describe pod`: the same command in both conditions

The rule of Part 1 compares the mean over whatever each condition's runs happened to describe. Here only a command typed character for character the same in a live run and in a frozen run of the same case and agent is compared.

| case | agent | commands in both | frozen / live, lowest to highest | exactly the same length |
|---|---|---|---|---|
| s1-shared-cache-exhaustion | claude-code | 3 | 0.998 to 1.000 | 1 |
| s1-shared-cache-exhaustion | holmes | 2 | 1.000 to 1.000 | 2 |
| s2-periodic-saturation | claude-code | 1 | 0.999 to 0.999 | 0 |
| s3-node-local-drift | claude-code | 3 | 0.996 to 1.004 | 0 |
| s3-node-local-drift | holmes | 4 | 0.998 to 1.000 | 1 |

Commands compared: 13; with the same output length in both conditions: 4. (`describe` prints ages, which kubectl counts from the wall clock; a few bytes move with the minute.)

What the cell outside the band described, by the shape of the command:

| case | agent | condition | one named pod | several pods or a selector | piped or cut down (`| head`, `| grep`, …) |
|---|---|---|---|---|---|
| s1-shared-cache-exhaustion | claude-code | live | 5 steps, mean 2734 | — | 1 steps, mean 31 |
| s1-shared-cache-exhaustion | claude-code | frozen | 8 steps, mean 2733 | 1 steps, mean 5475 | 1 steps, mean 693 |
| s1-shared-cache-exhaustion | holmes | live | 7 steps, mean 2795 | — | — |
| s1-shared-cache-exhaustion | holmes | frozen | 2 steps, mean 2790 | — | — |
| s2-periodic-saturation | claude-code | live | 3 steps, mean 2900 | — | 1 steps, mean 1735 |
| s2-periodic-saturation | claude-code | frozen | 6 steps, mean 2800 | — | 5 steps, mean 1511 |
| s3-node-local-drift | claude-code | live | 6 steps, mean 3405 | — | 1 steps, mean 1085 |
| s3-node-local-drift | claude-code | frozen | 11 steps, mean 3381 | — | 5 steps, mean 81 |
| s3-node-local-drift | holmes | live | 8 steps, mean 3485 | — | — |
| s3-node-local-drift | holmes | frozen | 16 steps, mean 3446 | — | — |

### Failed steps, by what the output says

| what | claude-code live | claude-code frozen | holmes live | holmes frozen |
|---|---|---|---|---|
| refused by the agent's own harness | 36 | 38 | 9 | 5 |
| `kubectl top`: no metrics API | 9 | 9 | 0 | 1 |
| not found | 0 | 1 | 0 | 0 |
| unknown flag or bad usage | 0 | 2 | 0 | 0 |
| the shell: no matches, parse error | 0 | 1 | 0 | 0 |
| no result from a metrics query | 0 | 0 | 2 | 2 |
| **all** | 45 | 51 | 11 | 8 |

What the agent's own harness refused, by the first `kubectl` verb of the command (a command with no `kubectl` is `(shell)`):

| verb | claude-code live | claude-code frozen | holmes live | holmes frozen |
|---|---|---|---|---|
| `exec` | 10 | 19 | 3 | 1 |
| `rollout` | 6 | 6 | 0 | 0 |
| `debug` | 7 | 3 | 0 | 0 |
| `get` | 2 | 4 | 2 | 1 |
| `port-forward` | 5 | 4 | 0 | 0 |
| `run` | 2 | 0 | 4 | 2 |
| `logs` | 1 | 2 | 0 | 0 |
| `(shell)` | 2 | 0 | 0 | 0 |
| `describe` | 1 | 0 | 0 | 0 |
| `top` | 0 | 0 | 0 | 1 |

Of what Claude Code refused, by what the command was:

| what | live | frozen |
|---|---|---|
| `kubectl exec` | 10 | 19 |
| `kubectl debug` | 7 | 3 |
| `kubectl port-forward` | 5 | 4 |
| `kubectl run` | 2 | 0 |
| `kubectl rollout history`: a read the guard allows | 6 | 6 |
| a read with a filter in `-o custom-columns`: `[?(@.type=="Ready")]` | 1 | 4 |
| a read piped into `awk` | 1 | 2 |
| a read piped into `xargs` | 1 | 0 |
| a shell command that is not kubectl | 3 | 0 |

### The mix of commands, live against frozen

Steps of each kind, summed over the eighteen runs of a cell. `p` is a two-sided permutation test on the per-run counts (50,000 shuffles, fixed seed). These are many comparisons looked at after the fact and **not corrected for that**.

| agent | what | live | frozen | p |
|---|---|---|---|---|
| claude-code | all steps | 363 | 376 | 0.82 |
| claude-code | failed steps | 45 | 51 | 0.73 |
| claude-code | `kubectl get` | 128 | 124 | 0.87 |
| claude-code | `kubectl logs` or a log tool | 108 | 114 | 0.89 |
| claude-code | `kubectl describe`, anything | 58 | 70 | 0.34 |
| claude-code | `kubectl describe pod` | 17 | 37 | 0.01 |
| claude-code | a metrics query | 27 | 24 | 0.90 |
| claude-code | piped through a filter | 107 | 101 | 0.89 |
| holmes | all steps | 226 | 224 | 0.97 |
| holmes | failed steps | 11 | 8 | 0.83 |
| holmes | `kubectl get` | 99 | 88 | 0.67 |
| holmes | `kubectl logs` or a log tool | 69 | 65 | 0.78 |
| holmes | `kubectl describe`, anything | 20 | 23 | 0.87 |
| holmes | `kubectl describe pod` | 15 | 18 | 0.83 |
| holmes | a metrics query | 31 | 43 | 0.61 |
| holmes | piped through a filter | 47 | 45 | 0.95 |

Comparisons: 16. Smallest p: 0.013 (claude-code, `kubectl describe pod`). With 16 looks and nothing going on, at least one this small turns up about 19% of the time.

### Answers only the replay gives, that are not errors

`analyze.py` searched for errors only the replay can produce and found none. These are not errors: the step succeeds and says something a live cluster does not.

| `kubectl get … --sort-by=` | live: answered | live: "No resources found" | frozen: answered | frozen: "No resources found" |
|---|---|---|---|---|
| a field under `metadata` | 0 | 0 | 2 | 0 |
| any other field | 18 | 0 | 0 | 13 |

Runs that were given an empty sorted list: claude-code live 0/18, claude-code frozen 9/18, holmes live 0/18, holmes frozen 2/18.
- claude-code, frozen: the 9 runs given one passed 3 times and retrieved all decisive evidence 5 times; the 9 not given one, 2 and 3.
- holmes, frozen: the 2 runs given one passed 0 times and retrieved all decisive evidence 0 times; the 16 not given one, 0 and 0.

| `kubectl get pods -o wide` | claude-code live | claude-code frozen | holmes live | holmes frozen |
|---|---|---|---|---|
| listing with NODE column | 6 | 0 | 3 | 0 |
| listing without column | 0 | 9 | 0 | 1 |

The header line of `kubectl get <kind>` alone on a line, where it was typed in both conditions and the headers differ:

| kind | live | frozen |
|---|---|---|
| deployments -o wide | 5× `NAME · READY · UP-TO-DATE · AVAILABLE · AGE · CONTAINERS · IMAGES · SELECTOR` | 1× `NAME · READY · UP-TO-DATE · AVAILABLE · AGE` |
| events  | 9× `LAST SEEN · TYPE · REASON · OBJECT · MESSAGE` | 1× `LASTTIMESTAMP · TYPE · REASON · OBJECT · MESSAGE` |
| pods -o wide | 9× `NAME · READY · STATUS · RESTARTS · AGE · IP · NODE · NOMINATED NODE · READINESS GATES` | 9× `NAME · READY · STATUS · RESTARTS · AGE`<br>1× `NAME · AGE` |
| replicasets -o wide | 3× `NAME · DESIRED · CURRENT · READY · AGE · CONTAINERS · IMAGES · SELECTOR` | 2× `NAME · AGE` |

Typed in the frozen condition only, with a header a live cluster does not print: endpoints — `NAME · AGE`; replicasets — `NAME · AGE`.

Steps that show one of these, and the runs they are in:

| what the step shows | live steps | live runs | frozen steps | frozen runs |
|---|---|---|---|---|
| an empty sorted list | 0 | 0/36 | 13 | 11/36 |
| a pod listing asked for `-o wide`, without NODE | 0 | 0/36 | 10 | 9/36 |
| a listing whose whole header is `NAME AGE` | 0 | 0/36 | 10 | 8/36 |
| an event listing headed `LASTTIMESTAMP` | 0 | 0/36 | 1 | 1/36 |
| **any of them** | 0 | 0/36 | 33 | 19/36 |

`kubectl get all`: names written with their kind (`pod/…`, `deployment.apps/…`) in 1 of 1 live listings and 0 of 4 frozen ones.

`kubectl describe pod` steps a run, claude-code: live 0.9 (18 runs); frozen runs given a wide pod listing without its NODE column 2.9 (8 runs); other frozen runs 1.4 (10 runs); frozen runs given an empty sorted list 2.0, the others 2.1.

### What round 37 §9 repaired, as the agents used it

- `kubectl logs --tail=N` alone on a line: 54 live steps, 73 frozen; returning more than N lines: 0.
- steps with `--field-selector`: claude-code frozen failed: 1; holmes frozen failed: 1; holmes live answered: 1.

### Times after the freeze

- The pattern committed in `analyze.py` matches 0 dates in the 36 frozen answers: it needs a word boundary after the date, and `2026-10-07T09:17` has none. Without that, 69 dates in 20 answers, on 2026-10-07 (69).
- Times of day later than the freeze, in a frozen answer: 1.
  - `s2-periodic-saturation/frozen-holmes-gpt-5-mini-4` says 09:53:00; frozen at 09:42:36
- Runs in which a tool of the agent's own stamped its answer with the wall clock (`Query executed at:`): claude-code live 0/18, claude-code frozen 0/18, holmes live 16/18, holmes frozen 16/18.

### Time and cost

| agent | condition | mean seconds a run | mean model calls | mean cost a run |
|---|---|---|---|---|
| claude-code | live | 74 | 21.2 | $0.099 |
| claude-code | frozen | 80 | 21.9 | $0.111 |
| holmes | live | 134 | 16.8 | $0.034 |
| holmes | frozen | 128 | 16.6 | $0.033 |

Runs that ended without an answer: 0.
