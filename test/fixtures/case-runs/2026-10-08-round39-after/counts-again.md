# `posthoc.py` — R1's signs and the `describe pod` count, with the verb wherever `kubectl` reads it

Made after both sets were reported, and under no rule (`docs/design-review-round39.md`, Part 4, *After the fact*).

## Round 38, agent A, frozen — what the patterns were made from

18 run records, 325 steps that did not fail; in 0 of them something stands between `kubectl` and `get`.

| what the step shows | times shown | runs |
|---|---|---|
| an empty sorted list | 11 | 9/18 |
| a pod listing asked for `-o wide`, without NODE | 9 | 8/18 |
| a listing whose whole header is `NAME AGE` | 10 | 8/18 |
| an event listing headed `LASTTIMESTAMP` | 0 | 0/18 |
| `kubectl get all` with names and no kinds | 4 | 4/18 |
| **any of them** | 34, in 29 steps | 15/18 |

`kubectl describe pod` steps that did not fail: 37, 2.06 a run; with the verb directly after `kubectl`, as `describe.py` looks for it, 37.

## Round 38, agent A, live

18 run records, 318 steps that did not fail; in 0 of them something stands between `kubectl` and `get`.

| what the step shows | times shown | runs |
|---|---|---|
| an empty sorted list | 0 | 0/18 |
| a pod listing asked for `-o wide`, without NODE | 0 | 0/18 |
| a listing whose whole header is `NAME AGE` | 0 | 0/18 |
| an event listing headed `LASTTIMESTAMP` | 0 | 0/18 |
| `kubectl get all` with names and no kinds | 0 | 0/18 |
| **any of them** | 0, in 0 steps | 0/18 |

`kubectl describe pod` steps that did not fail: 17, 0.94 a run; with the verb directly after `kubectl`, as `describe.py` looks for it, 17.

## The first set

18 run records, 93 steps that did not fail; in 0 of them something stands between `kubectl` and `get`.

| what the step shows | times shown | runs |
|---|---|---|
| an empty sorted list | 0 | 0/18 |
| a pod listing asked for `-o wide`, without NODE | 0 | 0/18 |
| a listing whose whole header is `NAME AGE` | 0 | 0/18 |
| an event listing headed `LASTTIMESTAMP` | 0 | 0/18 |
| `kubectl get all` with names and no kinds | 0 | 0/18 |
| **any of them** | 0, in 0 steps | 0/18 |

`kubectl describe pod` steps that did not fail: 2, 0.11 a run; with the verb directly after `kubectl`, as `describe.py` looks for it, 2.

## The second set

18 run records, 163 steps that did not fail; in 27 of them something stands between `kubectl` and `get`.

| what the step shows | times shown | runs |
|---|---|---|
| an empty sorted list | 0 | 0/18 |
| a pod listing asked for `-o wide`, without NODE | 0 | 0/18 |
| a listing whose whole header is `NAME AGE` | 0 | 0/18 |
| an event listing headed `LASTTIMESTAMP` | 0 | 0/18 |
| `kubectl get all` with names and no kinds | 0 | 0/18 |
| **any of them** | 0, in 0 steps | 0/18 |

`kubectl describe pod` steps that did not fail: 9, 0.50 a run; with the verb directly after `kubectl`, as `describe.py` looks for it, 5.

## The three runs after

3 run records, 25 steps that did not fail; in 0 of them something stands between `kubectl` and `get`.

| what the step shows | times shown | runs |
|---|---|---|
| an empty sorted list | 0 | 0/3 |
| a pod listing asked for `-o wide`, without NODE | 0 | 0/3 |
| a listing whose whole header is `NAME AGE` | 0 | 0/3 |
| an event listing headed `LASTTIMESTAMP` | 0 | 0/3 |
| `kubectl get all` with names and no kinds | 0 | 0/3 |
| **any of them** | 0, in 0 steps | 0/3 |

`kubectl describe pod` steps that did not fail: 1, 0.33 a run; with the verb directly after `kubectl`, as `describe.py` looks for it, 1.
