# Scenarios

What rebuilds each case under `cases/` on a fresh kind cluster. A case nobody can re-freeze cannot be
corrected (`docs/case-format.md`).

These scripts create and delete namespaces, and `s3` changes a file on the nodes. They **refuse to
run** unless `KUBECONFIG` names one file whose current context is `kind-rcabench`, and every `kubectl`
in them is pinned to that file and that context (`lib.sh`). The default kubeconfig is never used.

```
kind create cluster --name rcabench --config scenarios/kind.yaml --kubeconfig /tmp/rcabench.kubeconfig
export KUBECONFIG=/tmp/rcabench.kubeconfig

scenarios/s1-shared-cache-exhaustion/setup.sh          # waits for the symptom; fails if it does not form
lapilli-case freeze cases/s1-shared-cache-exhaustion/case.yaml -o /tmp/s1 --kubeconfig "$KUBECONFIG"
scenarios/s1-shared-cache-exhaustion/teardown.sh

kind delete cluster --name rcabench --kubeconfig /tmp/rcabench.kubeconfig
```

`freeze` needs [`crust-gather`](https://github.com/crust-gather/crust-gather) as
`kubectl-crust-gather` on `PATH`, or its path in `LAPILLI_CRUST_GATHER`. It prints whether each
decisive evidence pattern was found in the frozen copy and exits 2 if one was not. It reads the API
and starts nothing in the cluster, unless `--node-logs` is given.

| scenario | brings up | takes | notes |
|---|---|---|---|
| `s1-shared-cache-exhaustion` | namespace `shop`: a cache server with 40 connection slots and three clients, one of which leaks | about a minute | |
| `s2-periodic-saturation` | namespaces `media` and `monitoring`: an API with four workers, four callers, a Prometheus | about eight and a half minutes | freeze the metrics too — see below |
| `s3-node-local-drift` | namespaces `ledger` and `infra-agents`, and `/etc/ledger-tuning/tuning.conf` on both workers | about three minutes | uses `docker exec` on the kind nodes `rcabench-worker` and `rcabench-worker2` |

For `s2`, the Prometheus inside the cluster is frozen through a port-forward. In another shell — where
`KUBECONFIG` is not set, so the file is named:

```
kubectl --kubeconfig /tmp/rcabench.kubeconfig --context kind-rcabench -n monitoring port-forward svc/prometheus 9090
```

and then

```
lapilli-case freeze cases/s2-periodic-saturation/case.yaml -o /tmp/s2 --kubeconfig "$KUBECONFIG" \
  --metrics-url http://127.0.0.1:9090 --metrics-window 30m
```

The answer key of each is `cases/<id>/case.yaml`; it is not repeated here.

A case re-frozen today will not be byte-identical to the sealed one — pod names, addresses and
timestamps differ — and does not need to be. The sealed cases are the ones the recorded runs were
made against (`test/fixtures/case-runs/`).

All three were rebuilt with these scripts and re-frozen on 2026-10-06 and 2026-10-07 (`s1` in 60 s,
`s2` in 509 s, `s3` in 180 s), every decisive evidence item was found each time, and for `s2` five
queries at the freeze instant returned the same values from the live Prometheus and from the frozen
copy. Those re-freezes were not kept: the cases under `cases/` are the ones the recorded runs were
made from.
