commands: 546 | the same: 540 | the same lines in another order: 0 | both fail, worded differently: 0 | the cluster moved between the two live passes: 2 | **differ: 4**

Of the 92 that a recorded agent had typed: the same 92, another order 0, worded differently 0, the cluster moved 0, differ 0.

| question | asked | same | order | worded | moved | differ |
|---|---|---|---|---|---|---|
| `explain` | 2 | 0 | 0 | 0 | 0 | 2 |
| `cluster-info` | 1 | 0 | 0 | 0 | 0 | 1 |
| `describe secrets` | 1 | 0 | 0 | 0 | 0 | 1 |
| `describe nodes` | 2 | 1 | 0 | 0 | 1 |  |
| `logs --tail` | 36 | 35 | 0 | 0 | 1 |  |

The same every time: 335 kinds of question, 504 commands.

### Differ

- `kubectl describe secrets bootstrap-token-abcdef -n kube-system` (known)
  - live: auth-extra-groups: 47 bytes | frozen: auth-extra-groups: 19 bytes
- `kubectl cluster-info` (known)
  - live: Kubernetes control plane is running at https://127.0.0.1:60299 | frozen: Kubernetes control plane is running at http://127.0.0.1:61067/kubernetes
- `kubectl explain pods` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource
- `kubectl explain deployment.spec.strategy` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource

### The cluster moved

- `kubectl describe nodes rcabench-worker`
  - live: RenewTime: Thu, 08 Oct 2026 09:24:43 +0900 | frozen: RenewTime: Thu, 08 Oct 2026 09:24:33 +0900
- `kubectl logs deployment/checkout-api -n shop --tail=3`
  - the log went by faster than the tail asked for

Differing and not excused by known.txt: 0.
