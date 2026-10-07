commands: 480 | the same: 471 | the same lines in another order: 0 | both fail, worded differently: 0 | the cluster moved between the two live passes: 5 | **differ: 4**

Of the 61 that a recorded agent had typed: the same 61, another order 0, worded differently 0, the cluster moved 0, differ 0.

| question | asked | same | order | worded | moved | differ |
|---|---|---|---|---|---|---|
| `explain` | 2 | 0 | 0 | 0 | 0 | 2 |
| `cluster-info` | 1 | 0 | 0 | 0 | 0 | 1 |
| `describe secrets` | 1 | 0 | 0 | 0 | 0 | 1 |
| `describe nodes` | 2 | 1 | 0 | 0 | 1 |  |
| `logs --all-containers --tail` | 7 | 5 | 0 | 0 | 2 |  |
| `logs --tail` | 25 | 23 | 0 | 0 | 2 |  |

The same every time: 315 kinds of question, 442 commands.

### Differ

- `kubectl describe secrets bootstrap-token-abcdef -n kube-system` (known)
  - live: auth-extra-groups: 47 bytes | frozen: auth-extra-groups: 19 bytes
- `kubectl cluster-info` (known)
  - live: Kubernetes control plane is running at https://127.0.0.1:52344 | frozen: Kubernetes control plane is running at http://127.0.0.1:53489/kubernetes
- `kubectl explain pods` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource
- `kubectl explain deployment.spec.strategy` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource

### The cluster moved

- `kubectl describe nodes rcabench-control-plane`
  - live: RenewTime: Wed, 07 Oct 2026 23:17:26 +0900 | frozen: RenewTime: Wed, 07 Oct 2026 23:17:16 +0900
- `kubectl logs deployment/checkout-api -n shop --tail=3`
  - the log went by faster than the tail asked for
- `kubectl logs deployment/report-worker -n shop --tail=3`
  - the log went by faster than the tail asked for
- `kubectl logs checkout-api-58f9d6b876-9bzfz -n shop --tail=5 --all-containers`
  - the logs went by faster than the tail asked for
- `kubectl logs checkout-api-58f9d6b876-vd4sv -n shop --tail=5 --all-containers`
  - the logs went by faster than the tail asked for

Differing and not excused by known.txt: 0.
