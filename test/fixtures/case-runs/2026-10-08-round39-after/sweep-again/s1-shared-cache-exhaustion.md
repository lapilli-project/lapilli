commands: 545 | the same: 533 | the same lines in another order: 0 | both fail, worded differently: 0 | the cluster moved between the two live passes: 8 | **differ: 4**

Of the 91 that a recorded agent had typed: the same 90, another order 0, worded differently 0, the cluster moved 1, differ 0.

| question | asked | same | order | worded | moved | differ |
|---|---|---|---|---|---|---|
| `explain` | 2 | 0 | 0 | 0 | 0 | 2 |
| `cluster-info` | 1 | 0 | 0 | 0 | 0 | 1 |
| `describe secrets` | 1 | 0 | 0 | 0 | 0 | 1 |
| `describe nodes` | 2 | 1 | 0 | 0 | 1 |  |
| `logs --all-containers --tail` | 8 | 7 | 0 | 0 | 1 |  |
| `logs --prefix --tail -l` | 1 | 0 | 0 | 0 | 1 |  |
| `logs --tail` | 36 | 34 | 0 | 0 | 2 |  |
| `logs --tail --timestamps` | 7 | 4 | 0 | 0 | 3 |  |

The same every time: 331 kinds of question, 487 commands.

### Differ

- `kubectl describe secrets bootstrap-token-abcdef -n kube-system` (known)
  - live: auth-extra-groups: 47 bytes | frozen: auth-extra-groups: 19 bytes
- `kubectl cluster-info` (known)
  - live: Kubernetes control plane is running at https://127.0.0.1:51104 | frozen: Kubernetes control plane is running at http://127.0.0.1:51879/kubernetes
- `kubectl explain pods` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource
- `kubectl explain deployment.spec.strategy` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource

### The cluster moved

- `kubectl describe nodes rcabench-control-plane`
  - live: RenewTime: Thu, 08 Oct 2026 08:38:05 +0900 | frozen: RenewTime: Thu, 08 Oct 2026 08:37:54 +0900
- `kubectl logs deployment/checkout-api -n shop --tail=3`
  - the log went by faster than the tail asked for
- `kubectl logs deployment/report-worker -n shop --tail=3`
  - the log went by faster than the tail asked for
- `kubectl logs checkout-api-58f9d6b876-k8trf -n shop --tail=5 --all-containers`
  - the logs went by faster than the tail asked for
- `kubectl logs checkout-api-58f9d6b876-k8trf -n shop --timestamps --tail=3`
  - the log went by faster than the tail asked for
- `kubectl logs report-worker-9c8dbb6dd-nf9qg -n shop --timestamps --tail=3`
  - the log went by faster than the tail asked for
- `kubectl logs -n shop -l app=report-worker --tail=2 --prefix=true`
  - the logs went by faster than the tail asked for

…and 1 more of the same kinds.

Differing and not excused by known.txt: 0.
