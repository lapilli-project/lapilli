commands: 491 | the same: 483 | the same lines in another order: 0 | both fail, worded differently: 0 | the cluster moved between the two live passes: 4 | **differ: 4**

Of the 36 that a recorded agent had typed: the same 35, another order 0, worded differently 0, the cluster moved 1, differ 0.

| question | asked | same | order | worded | moved | differ |
|---|---|---|---|---|---|---|
| `explain` | 2 | 0 | 0 | 0 | 0 | 2 |
| `cluster-info` | 1 | 0 | 0 | 0 | 0 | 1 |
| `describe secrets` | 1 | 0 | 0 | 0 | 0 | 1 |
| `logs --prefix --tail -l` | 1 | 0 | 0 | 0 | 1 |  |
| `logs --tail` | 21 | 18 | 0 | 0 | 3 |  |

The same every time: 321 kinds of question, 465 commands.

### Differ

- `kubectl describe secrets bootstrap-token-abcdef -n kube-system` (known)
  - live: auth-extra-groups: 47 bytes | frozen: auth-extra-groups: 19 bytes
- `kubectl cluster-info` (known)
  - live: Kubernetes control plane is running at https://127.0.0.1:49408 | frozen: Kubernetes control plane is running at http://127.0.0.1:50641/kubernetes
- `kubectl explain pods` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource
- `kubectl explain deployment.spec.strategy` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource

### The cluster moved

- `kubectl logs deployment/checkout-api -n shop --tail=3`
  - the log went by faster than the tail asked for
- `kubectl logs deployment/inventory-sync -n shop --tail=3`
  - the log went by faster than the tail asked for
- `kubectl logs -n shop -l app=report-worker --tail=2 --prefix=true`
  - the logs went by faster than the tail asked for

…and 1 more of the same kinds.

Differing and not excused by known.txt: 0.
