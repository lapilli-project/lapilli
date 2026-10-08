commands: 546 | the same: 540 | the same lines in another order: 0 | both fail, worded differently: 0 | the cluster moved between the two live passes: 2 | **differ: 4**

Of the 60 that a recorded agent had typed: the same 60, another order 0, worded differently 0, the cluster moved 0, differ 0.

| question | asked | same | order | worded | moved | differ |
|---|---|---|---|---|---|---|
| `explain` | 2 | 0 | 0 | 0 | 0 | 2 |
| `cluster-info` | 1 | 0 | 0 | 0 | 0 | 1 |
| `describe secrets` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get events -o wide` | 3 | 2 | 0 | 0 | 1 |  |
| `get events.events.k8s.io -o wide` | 1 | 0 | 0 | 0 | 1 |  |

The same every time: 320 kinds of question, 538 commands.

### Differ

- `kubectl describe secrets bootstrap-token-abcdef -n kube-system` (known)
  - live: auth-extra-groups: 47 bytes | frozen: auth-extra-groups: 19 bytes
- `kubectl cluster-info` (known)
  - live: Kubernetes control plane is running at https://127.0.0.1:57173 | frozen: Kubernetes control plane is running at http://127.0.0.1:63185/kubernetes
- `kubectl explain pods` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource
- `kubectl explain deployment.spec.strategy` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource

### The cluster moved

- `kubectl get events -A -o wide`
  - live: ledger 4s Warning Unhealthy pod/ledger-api-866db84748-bjb5w spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503  | frozen: ledger 5s Warning Unhealthy pod/ledger-api-866db84748-bjb5w spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503 
- `kubectl get events.events.k8s.io -A -o wide`
  - live: ledger 4s Warning Unhealthy pod/ledger-api-866db84748-bjb5w spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503  | frozen: ledger 5s Warning Unhealthy pod/ledger-api-866db84748-bjb5w spec.containers{api} kubelet, rcabench-worker2 Readiness probe failed: HTTP probe failed with statuscode: 503 

Differing and not excused by known.txt: 0.
