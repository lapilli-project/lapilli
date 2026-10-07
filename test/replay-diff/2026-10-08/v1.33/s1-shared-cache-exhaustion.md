commands: 480 | the same: 469 | the same lines in another order: 0 | both fail, worded differently: 0 | the cluster moved between the two live passes: 7 | **differ: 4**

Of the 64 that a recorded agent had typed: the same 64, another order 0, worded differently 0, the cluster moved 0, differ 0.

| question | asked | same | order | worded | moved | differ |
|---|---|---|---|---|---|---|
| `explain` | 2 | 0 | 0 | 0 | 0 | 2 |
| `cluster-info` | 1 | 0 | 0 | 0 | 0 | 1 |
| `describe secrets` | 1 | 0 | 0 | 0 | 0 | 1 |
| `logs --all-containers --tail` | 7 | 5 | 0 | 0 | 2 |  |
| `logs --tail` | 25 | 23 | 0 | 0 | 2 |  |
| `logs --tail --timestamps` | 7 | 4 | 0 | 0 | 3 |  |

The same every time: 288 kinds of question, 437 commands.

### Differ

- `kubectl describe secrets bootstrap-token-abcdef -n kube-system` (known)
  - live: auth-extra-groups: 47 bytes | frozen: token-id: 19 bytes
- `kubectl cluster-info` (known)
  - live: Kubernetes control plane is running at https://127.0.0.1:62938 | frozen: Kubernetes control plane is running at http://127.0.0.1:64207/kubernetes
- `kubectl explain pods` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource
- `kubectl explain deployment.spec.strategy` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource

### The cluster moved

- `kubectl logs deployment/checkout-api -n shop --tail=3`
  - the log went by faster than the tail asked for
- `kubectl logs deployment/report-worker -n shop --tail=3`
  - the log went by faster than the tail asked for
- `kubectl logs checkout-api-5b76d494c9-2kd52 -n shop --tail=5 --all-containers`
  - the logs went by faster than the tail asked for
- `kubectl logs checkout-api-5b76d494c9-2kd52 -n shop --timestamps --tail=3`
  - the log went by faster than the tail asked for
- `kubectl logs checkout-api-5b76d494c9-wd4r4 -n shop --tail=5 --all-containers`
  - the logs went by faster than the tail asked for
- `kubectl logs checkout-api-5b76d494c9-wd4r4 -n shop --timestamps --tail=3`
  - the log went by faster than the tail asked for

…and 1 more of the same kinds.

Differing and not excused by known.txt: 0.
