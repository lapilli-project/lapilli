commands: 605 | the same: 593 | the same lines in another order: 0 | both fail, worded differently: 0 | the cluster moved between the two live passes: 8 | **differ: 4**

Of the 130 that a recorded agent had typed: the same 126, another order 0, worded differently 0, the cluster moved 4, differ 0.

| question | asked | same | order | worded | moved | differ |
|---|---|---|---|---|---|---|
| `explain` | 2 | 0 | 0 | 0 | 0 | 2 |
| `cluster-info` | 1 | 0 | 0 | 0 | 0 | 1 |
| `describe secrets` | 1 | 0 | 0 | 0 | 0 | 1 |
| `describe node` | 3 | 1 | 0 | 0 | 2 |  |
| `describe nodes` | 3 | 1 | 0 | 0 | 2 |  |
| `get events --sort-by -o wide` | 1 | 0 | 0 | 0 | 1 |  |
| `get events -o wide` | 3 | 1 | 0 | 0 | 2 |  |
| `get events.events.k8s.io -o wide` | 1 | 0 | 0 | 0 | 1 |  |

The same every time: 352 kinds of question, 590 commands.

### Differ

- `kubectl describe secrets bootstrap-token-abcdef -n kube-system` (known)
  - live: auth-extra-groups: 47 bytes | frozen: auth-extra-groups: 19 bytes
- `kubectl cluster-info` (known)
  - live: Kubernetes control plane is running at https://127.0.0.1:52977 | frozen: Kubernetes control plane is running at http://127.0.0.1:57991/kubernetes
- `kubectl explain pods` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource
- `kubectl explain deployment.spec.strategy` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource

### The cluster moved

- `kubectl get events -A -o wide`
  - live: ledger 2s Warning Unhealthy pod/ledger-api-866db84748-4l66v spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503  | frozen: ledger 2s Warning Unhealthy pod/ledger-api-866db84748-4l66v spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503 
- `kubectl get events.events.k8s.io -A -o wide`
  - live: ledger 2s Warning Unhealthy pod/ledger-api-866db84748-4l66v spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503  | frozen: ledger 2s Warning Unhealthy pod/ledger-api-866db84748-4l66v spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503 
- `kubectl describe nodes rcabench-control-plane`
  - live: RenewTime: Thu, 08 Oct 2026 03:23:35 +0900 | frozen: RenewTime: Thu, 08 Oct 2026 03:23:25 +0900
- `kubectl describe nodes rcabench-worker`
  - live: RenewTime: Thu, 08 Oct 2026 03:23:36 +0900 | frozen: RenewTime: Thu, 08 Oct 2026 03:23:25 +0900
- `kubectl get events -n ledger -o wide`
  - live: 5s Warning Unhealthy pod/ledger-api-866db84748-4l66v spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503 70s 15  | frozen: 2s Warning Unhealthy pod/ledger-api-866db84748-4l66v spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503 57s 13 
- `kubectl describe node rcabench-worker2`
  - live: RenewTime: Thu, 08 Oct 2026 03:23:36 +0900 | frozen: RenewTime: Thu, 08 Oct 2026 03:23:26 +0900
- `kubectl describe node rcabench-worker rcabench-worker2`
  - live: RenewTime: Thu, 08 Oct 2026 03:23:36 +0900 | frozen: RenewTime: Thu, 08 Oct 2026 03:23:25 +0900
- `kubectl get events -n ledger --sort-by=.lastTimestamp -o wide`
  - live: 2s Warning Unhealthy pod/ledger-api-866db84748-cbq6w spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503 86s 19  | frozen: 2s Warning Unhealthy pod/ledger-api-866db84748-xcml4 spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503 62s 14 

Differing and not excused by known.txt: 0.
