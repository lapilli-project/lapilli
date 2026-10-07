commands: 552 | the same: 543 | the same lines in another order: 1 | both fail, worded differently: 0 | the cluster moved between the two live passes: 4 | **differ: 4**

Of the 66 that a recorded agent had typed: the same 66, another order 0, worded differently 0, the cluster moved 0, differ 0.

| question | asked | same | order | worded | moved | differ |
|---|---|---|---|---|---|---|
| `explain` | 2 | 0 | 0 | 0 | 0 | 2 |
| `cluster-info` | 1 | 0 | 0 | 0 | 0 | 1 |
| `describe secrets` | 1 | 0 | 0 | 0 | 0 | 1 |
| `describe nodes` | 2 | 1 | 0 | 0 | 1 |  |
| `events` | 2 | 1 | 1 | 0 | 0 |  |
| `get events -o wide` | 3 | 1 | 0 | 0 | 2 |  |
| `get events.events.k8s.io -o wide` | 1 | 0 | 0 | 0 | 1 |  |

The same every time: 320 kinds of question, 540 commands.

### Differ

- `kubectl describe secrets bootstrap-token-abcdef -n kube-system` (known)
  - live: auth-extra-groups: 47 bytes | frozen: auth-extra-groups: 19 bytes
- `kubectl cluster-info` (known)
  - live: Kubernetes control plane is running at https://127.0.0.1:51104 | frozen: Kubernetes control plane is running at http://127.0.0.1:56414/kubernetes
- `kubectl explain pods` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource
- `kubectl explain deployment.spec.strategy` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource

### The cluster moved

- `kubectl get events -A -o wide`
  - live: ledger 1s Warning Unhealthy pod/ledger-api-866db84748-l7tvj spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503  | frozen: ledger 1s Warning Unhealthy pod/ledger-api-866db84748-l7tvj spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503 
- `kubectl get events.events.k8s.io -A -o wide`
  - live: ledger 1s Warning Unhealthy pod/ledger-api-866db84748-l7tvj spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503  | frozen: ledger 1s Warning Unhealthy pod/ledger-api-866db84748-l7tvj spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503 
- `kubectl describe nodes rcabench-worker`
  - live: RenewTime: Thu, 08 Oct 2026 08:51:34 +0900 | frozen: RenewTime: Thu, 08 Oct 2026 08:51:24 +0900
- `kubectl get events -n ledger -o wide`
  - live: 4s Warning Unhealthy pod/ledger-api-866db84748-l7tvj spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503 79s 17  | frozen: 1s Warning Unhealthy pod/ledger-api-866db84748-l7tvj spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503 66s 15 

### The same lines in another order, under --sort-by

- `kubectl events -n ledger`

Differing and not excused by known.txt: 0.
