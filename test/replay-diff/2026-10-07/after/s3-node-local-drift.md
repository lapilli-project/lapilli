commands: 485 | the same: 474 | the same lines in another order: 2 | both fail, worded differently: 0 | the cluster moved between the two live passes: 5 | **differ: 4**

| question | asked | same | order | worded | moved | differ |
|---|---|---|---|---|---|---|
| `explain` | 2 | 0 | 0 | 0 | 0 | 2 |
| `cluster-info` | 1 | 0 | 0 | 0 | 0 | 1 |
| `describe secrets` | 1 | 0 | 0 | 0 | 0 | 1 |
| `describe node` | 3 | 2 | 0 | 0 | 1 |  |
| `get event one named --sort-by` | 1 | 0 | 1 | 0 | 0 |  |
| `get events --sort-by` | 1 | 0 | 1 | 0 | 0 |  |
| `get events one named --field-selector --no-headers --sort-by -o wide` | 1 | 0 | 0 | 0 | 1 |  |
| `get events one named --sort-by -o wide` | 1 | 0 | 0 | 0 | 1 |  |
| `get events one named -o wide` | 5 | 3 | 0 | 0 | 2 |  |

The same every time: 250 kinds of question, 469 commands.

### Differ

- `kubectl describe secrets bootstrap-token-abcdef -n kube-system` (known)
  - live: auth-extra-groups: 47 bytes | frozen: auth-extra-groups: 19 bytes
- `kubectl cluster-info` (known)
  - live: Kubernetes control plane is running at https://127.0.0.1:63190 | frozen: Kubernetes control plane is running at http://127.0.0.1:53366/kubernetes
- `kubectl explain pods` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource
- `kubectl explain deployment.spec.strategy` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource

### The cluster moved

- `kubectl get events -A -o wide`
  - live: ledger <age> Warning Unhealthy pod/ledger-api-866db84748-r8z7p spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 5 | frozen: ledger <age> Warning Unhealthy pod/ledger-api-866db84748-r8z7p spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 5
- `kubectl get events -n ledger -o wide`
  - live: <age> Warning Unhealthy pod/ledger-api-866db84748-dc266 spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503 <age | frozen: <age> Warning Unhealthy pod/ledger-api-866db84748-dc266 spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503 <age
- `kubectl describe node rcabench-worker rcabench-worker2`
  - live: RenewTime: Wed, 07 Oct 2026 20:49:59 +0900 | frozen: RenewTime: Wed, 07 Oct 2026 20:49:49 +0900
- `kubectl get events -n ledger --sort-by=.lastTimestamp -o wide`
  - live: <age> Warning Unhealthy pod/ledger-api-866db84748-dc266 spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503 <age | frozen: <age> Warning Unhealthy pod/ledger-api-866db84748-r8z7p spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503 <age
- `kubectl get events -n ledger --sort-by=.lastTimestamp --field-selector involvedObject.kind=Pod -o wide --no-headers`
  - live: <age> Warning Unhealthy pod/ledger-api-866db84748-dc266 spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503 <age | frozen: <age> Warning Unhealthy pod/ledger-api-866db84748-r8z7p spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503 <age

Differing and not listed as known: 0.
