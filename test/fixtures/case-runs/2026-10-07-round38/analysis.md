runs: 72 | judged by both: 72 | agents: claude-code, holmes

### Counts

| agent | condition | runs | pass (both judges) | judge 1 | judge 2 | all decisive evidence retrieved | mean steps | failed steps | cited, not observed | cost |
|---|---|---|---|---|---|---|---|---|---|---|
| claude-code | live | 18 | 5/18 | 5/18 | 6/18 | 7/18 | 20.2 | 45 | 0 | $1.78 |
| claude-code | frozen | 18 | 5/18 | 5/18 | 7/18 | 8/18 | 20.9 | 51 | 0 | $1.99 |
| holmes | live | 18 | 0/18 | 0/18 | 0/18 | 0/18 | 12.6 | 11 | 0 | $0.62 |
| holmes | frozen | 18 | 0/18 | 0/18 | 0/18 | 0/18 | 12.4 | 8 | 0 | $0.60 |
| both | live | 36 | 5/36 | 5/36 | 6/36 | 7/36 | 16.4 | 56 | 0 | $2.39 |
| both | frozen | 36 | 5/36 | 5/36 | 7/36 | 8/36 | 16.7 | 59 | 0 | $2.60 |

### R1 — live minus frozen, with a 95% interval

| agent | measure | live | frozen | difference (points) | 95% interval | half-width | reading |
|---|---|---|---|---|---|---|---|
| claude-code | pass, both judges | 5/18 | 5/18 | +0 | -28 to +28 | 28 | not distinguished |
| claude-code | all decisive evidence retrieved | 7/18 | 8/18 | -6 | -34 to +24 | 29 | not distinguished |
| holmes | pass, both judges | 0/18 | 0/18 | +0 | -18 to +18 | 18 | not distinguished |
| holmes | all decisive evidence retrieved | 0/18 | 0/18 | +0 | -18 to +18 | 18 | not distinguished |
| both | pass, both judges | 5/36 | 5/36 | +0 | -17 to +17 | 17 | not distinguished |
| both | all decisive evidence retrieved | 7/36 | 8/36 | -3 | -21 to +16 | 19 | not distinguished |

### The two judges

| | packets | agree | Cohen's kappa | judge 1 passes | judge 2 passes |
|---|---|---|---|---|---|
| claude-code | 36 | 92% | 0.81 | 10 | 13 |
| holmes | 36 | 100% | nan | 0 | 0 |
| both | 72 | 96% | 0.85 | 10 | 13 |

Disagreements: `s2-periodic-saturation/frozen-claude-code-haiku-1` (judge 1 FAIL, judge 2 PASS); `s2-periodic-saturation/frozen-claude-code-haiku-3` (judge 1 FAIL, judge 2 PASS); `s2-periodic-saturation/live-claude-code-haiku-1` (judge 1 FAIL, judge 2 PASS)

### By case — reported, not claimed

| case | agent | live: pass · evidence | frozen: pass · evidence |
|---|---|---|---|
| s1-shared-cache-exhaustion | claude-code | 3/6 · 3/6 | 1/6 · 4/6 |
| s1-shared-cache-exhaustion | holmes | 0/6 · 0/6 | 0/6 · 0/6 |
| s2-periodic-saturation | claude-code | 2/6 · 4/6 | 3/6 · 3/6 |
| s2-periodic-saturation | holmes | 0/6 · 0/6 | 0/6 · 0/6 |
| s3-node-local-drift | claude-code | 0/6 · 0/6 | 1/6 · 1/6 |
| s3-node-local-drift | holmes | 0/6 · 0/6 | 0/6 · 0/6 |

### R2 — `kubectl describe pod`, output length

| case | agent | live: steps, mean bytes | frozen: steps, mean bytes | frozen / live | within 0.8–1.25 |
|---|---|---|---|---|---|
| s1-shared-cache-exhaustion | claude-code | 6, 2284 | 10, 2803 | 1.23 | yes |
| s1-shared-cache-exhaustion | holmes | 7, 2795 | 2, 2790 | — | too few steps to rule |
| s2-periodic-saturation | claude-code | 4, 2609 | 11, 2214 | 0.85 | yes |
| s3-node-local-drift | claude-code | 7, 3073 | 16, 2350 | 0.76 | **no** |
| s3-node-local-drift | holmes | 8, 3485 | 16, 3446 | 0.99 | yes |

Cells with at least three such steps in each condition: 4; outside the band: 1.

### R2 — failures only the replay can produce

| what | live | frozen |
|---|---|---|
| (none of the patterns searched for) | 0 | 0 |

Guard refusals:

| refused | live | frozen |
|---|---|---|
| (none) | 0 | 0 |

### R2 — dates after the freeze in a frozen answer

Frozen answers naming a date after the day of their freeze: 0.

### R3 — passing without all the decisive evidence

| all decisive evidence retrieved | runs | pass (both judges) |
|---|---|---|
| yes | 15 | 10 |
| no | 57 | 0 |

### Excluded as provider failures

None.
