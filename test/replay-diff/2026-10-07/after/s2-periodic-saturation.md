commands: 533 | the same: 528 | the same lines in another order: 0 | both fail, worded differently: 0 | the cluster moved between the two live passes: 1 | **differ: 4**

Of the 82 that a recorded agent had typed: the same 82, another order 0, worded differently 0, the cluster moved 0, differ 0.

| question | asked | same | order | worded | moved | differ |
|---|---|---|---|---|---|---|
| `explain` | 2 | 0 | 0 | 0 | 0 | 2 |
| `cluster-info` | 1 | 0 | 0 | 0 | 0 | 1 |
| `describe secrets` | 1 | 0 | 0 | 0 | 0 | 1 |
| `describe nodes` | 2 | 1 | 0 | 0 | 1 |  |

The same every time: 314 kinds of question, 527 commands.

### Differ

- `kubectl describe secrets bootstrap-token-abcdef -n kube-system` (known)
  - live: auth-extra-groups: 47 bytes | frozen: auth-extra-groups: 19 bytes
- `kubectl cluster-info` (known)
  - live: Kubernetes control plane is running at https://127.0.0.1:52344 | frozen: Kubernetes control plane is running at http://127.0.0.1:57004/kubernetes
- `kubectl explain pods` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource
- `kubectl explain deployment.spec.strategy` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource

### The cluster moved

- `kubectl describe nodes rcabench-control-plane`
  - live: RenewTime: Wed, 07 Oct 2026 23:26:38 +0900 | frozen: RenewTime: Wed, 07 Oct 2026 23:26:27 +0900

Differing and not excused by known.txt: 0.
