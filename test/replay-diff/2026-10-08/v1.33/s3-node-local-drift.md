commands: 579 | the same: 569 | the same lines in another order: 2 | both fail, worded differently: 0 | the cluster moved between the two live passes: 4 | **differ: 4**

Of the 130 that a recorded agent had typed: the same 128, another order 1, worded differently 0, the cluster moved 1, differ 0.

| question | asked | same | order | worded | moved | differ |
|---|---|---|---|---|---|---|
| `explain` | 2 | 0 | 0 | 0 | 0 | 2 |
| `cluster-info` | 1 | 0 | 0 | 0 | 0 | 1 |
| `describe secrets` | 1 | 0 | 0 | 0 | 0 | 1 |
| `events` | 2 | 1 | 1 | 0 | 0 |  |
| `get events --sort-by` | 5 | 4 | 1 | 0 | 0 |  |
| `get events -o wide` | 3 | 1 | 0 | 0 | 2 |  |
| `get events.events.k8s.io -o wide` | 1 | 0 | 0 | 0 | 1 |  |
| `logs --tail --timestamps` | 11 | 10 | 0 | 0 | 1 |  |

The same every time: 316 kinds of question, 553 commands.

### Differ

- `kubectl describe secrets bootstrap-token-abcdef -n kube-system` (known)
  - the cluster moved, and the frozen answer is not between its two: live: usage-bootstrap-authentication: 4 bytes | frozen: expiration: 19 bytes
- `kubectl cluster-info` (known)
  - live: Kubernetes control plane is running at https://127.0.0.1:62938 | frozen: Kubernetes control plane is running at http://127.0.0.1:54047/kubernetes
- `kubectl explain pods` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource
- `kubectl explain deployment.spec.strategy` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource

### The cluster moved

- `kubectl get events -A -o wide`
  - live: ledger 2s Warning Unhealthy pod/ledger-api-65c64bdcc5-qd65z spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503  | frozen: ledger 4s Warning Unhealthy pod/ledger-api-65c64bdcc5-qd65z spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503 
- `kubectl get events.events.k8s.io -A -o wide`
  - live: ledger 2s Warning Unhealthy pod/ledger-api-65c64bdcc5-qd65z spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503  | frozen: ledger 4s Warning Unhealthy pod/ledger-api-65c64bdcc5-qd65z spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503 
- `kubectl get events -n ledger -o wide`
  - live: 2s Warning Unhealthy pod/ledger-api-65c64bdcc5-qd65z spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503 82s 19  | frozen: 4s Warning Unhealthy pod/ledger-api-65c64bdcc5-qd65z spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503 64s 14 
- `kubectl logs ledger-api-65c64bdcc5-qwkpc -n ledger --timestamps --tail=3`
  - the log went by faster than the tail asked for

### The same lines in another order, under --sort-by

- `kubectl events -n ledger`
- `kubectl get events -A --sort-by=.lastTimestamp`

Differing and not excused by known.txt: 0.
