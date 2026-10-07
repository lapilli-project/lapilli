commands: 522 | the same: 508 | the same lines in another order: 0 | both fail, worded differently: 0 | the cluster moved between the two live passes: 10 | **differ: 4**

Of the 67 that a recorded agent had typed: the same 67, another order 0, worded differently 0, the cluster moved 0, differ 0.

| question | asked | same | order | worded | moved | differ |
|---|---|---|---|---|---|---|
| `explain` | 2 | 0 | 0 | 0 | 0 | 2 |
| `cluster-info` | 1 | 0 | 0 | 0 | 0 | 1 |
| `describe secrets` | 1 | 0 | 0 | 0 | 0 | 1 |
| `logs --all-containers --tail` | 8 | 6 | 0 | 0 | 2 |  |
| `logs --tail` | 27 | 24 | 0 | 0 | 3 |  |
| `logs --tail --timestamps` | 7 | 2 | 0 | 0 | 5 |  |

The same every time: 331 kinds of question, 476 commands.

### Differ

- `kubectl describe secrets bootstrap-token-abcdef -n kube-system` (known)
  - live: auth-extra-groups: 47 bytes | frozen: auth-extra-groups: 19 bytes
- `kubectl cluster-info` (known)
  - live: Kubernetes control plane is running at https://127.0.0.1:57173 | frozen: Kubernetes control plane is running at http://127.0.0.1:58128/kubernetes
- `kubectl explain pods` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource
- `kubectl explain deployment.spec.strategy` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource

### The cluster moved

- `kubectl logs deployment/checkout-api -n shop --tail=3`
  - the log went by faster than the tail asked for
- `kubectl logs deployment/inventory-sync -n shop --tail=3`
  - the log went by faster than the tail asked for
- `kubectl logs checkout-api-58f9d6b876-69rxk -n shop --tail=5 --all-containers`
  - the logs went by faster than the tail asked for
- `kubectl logs checkout-api-58f9d6b876-69rxk -n shop --timestamps --tail=3`
  - the log went by faster than the tail asked for
- `kubectl logs checkout-api-58f9d6b876-czkng -n shop --timestamps --tail=3`
  - the log went by faster than the tail asked for
- `kubectl logs inventory-sync-7fd8b9d79-vgwvj -n shop --tail=5 --all-containers`
  - the logs went by faster than the tail asked for

…and 4 more of the same kinds.

Differing and not excused by known.txt: 0.
