commands: 564 | the same: 555 | the same lines in another order: 0 | both fail, worded differently: 0 | the cluster moved between the two live passes: 5 | **differ: 4**

Of the 125 that a recorded agent had typed: the same 122, another order 0, worded differently 0, the cluster moved 3, differ 0.

| question | asked | same | order | worded | moved | differ |
|---|---|---|---|---|---|---|
| `explain` | 2 | 0 | 0 | 0 | 0 | 2 |
| `cluster-info` | 1 | 0 | 0 | 0 | 0 | 1 |
| `describe secrets` | 1 | 0 | 0 | 0 | 0 | 1 |
| `describe nodes` | 3 | 2 | 0 | 0 | 1 |  |
| `get events -o wide` | 3 | 1 | 0 | 0 | 2 |  |
| `logs --tail -l` | 2 | 0 | 0 | 0 | 2 |  |

The same every time: 345 kinds of question, 552 commands.

### Differ

- `kubectl describe secrets bootstrap-token-abcdef -n kube-system` (known)
  - live: auth-extra-groups: 47 bytes | frozen: auth-extra-groups: 19 bytes
- `kubectl cluster-info` (known)
  - live: Kubernetes control plane is running at https://127.0.0.1:52344 | frozen: Kubernetes control plane is running at http://127.0.0.1:59378/kubernetes
- `kubectl explain pods` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource
- `kubectl explain deployment.spec.strategy` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource

### The cluster moved

- `kubectl get events -A -o wide`
  - live: ledger 4s Warning Unhealthy pod/ledger-api-866db84748-d58v2 spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503  | frozen: ledger 4s Warning Unhealthy pod/ledger-api-866db84748-d58v2 spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503 
- `kubectl describe nodes rcabench-control-plane`
  - live: RenewTime: Wed, 07 Oct 2026 23:31:03 +0900 | frozen: RenewTime: Wed, 07 Oct 2026 23:30:52 +0900
- `kubectl get events -n ledger -o wide`
  - live: 3s Warning Unhealthy pod/ledger-api-866db84748-d58v2 spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503 73s 16  | frozen: 4s Warning Unhealthy pod/ledger-api-866db84748-d58v2 spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503 59s 13 
- `kubectl logs -n infra-agents -l app=node-agent --tail=50`
  - the logs went by faster than the tail asked for
- `kubectl logs -n infra-agents -l app=node-agent --tail=300`
  - the logs went by faster than the tail asked for

Differing and not excused by known.txt: 0.
