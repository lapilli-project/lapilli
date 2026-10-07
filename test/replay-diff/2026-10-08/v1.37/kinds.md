commands: 791 | the same: 784 | the same lines in another order: 0 | both fail, worded differently: 0 | the cluster moved between the two live passes: 2 | **differ: 5**

Of the 0 that a recorded agent had typed: the same 0, another order 0, worded differently 0, the cluster moved 0, differ 0.

| question | asked | same | order | worded | moved | differ |
|---|---|---|---|---|---|---|
| `describe secrets` | 2 | 0 | 0 | 0 | 0 | 2 |
| `explain` | 2 | 0 | 0 | 0 | 0 | 2 |
| `cluster-info` | 1 | 0 | 0 | 0 | 0 | 1 |
| `describe nodes` | 2 | 0 | 0 | 0 | 2 |  |

The same every time: 373 kinds of question, 784 commands.

### Differ

- `kubectl describe secrets api-key -n kinds` (known)
  - live: key: 12 bytes | frozen: key: 19 bytes
- `kubectl describe secrets basic -n kinds` (known)
  - live: password: 12 bytes | frozen: password: 19 bytes
- `kubectl cluster-info` (known)
  - live: Kubernetes control plane is running at https://127.0.0.1:52977 | frozen: Kubernetes control plane is running at http://127.0.0.1:61103/kubernetes
- `kubectl explain pods` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource
- `kubectl explain deployment.spec.strategy` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource

### The cluster moved

- `kubectl describe nodes rcabench-control-plane`
  - live: RenewTime: Thu, 08 Oct 2026 03:31:46 +0900 | frozen: RenewTime: Thu, 08 Oct 2026 03:31:36 +0900
- `kubectl describe nodes rcabench-worker`
  - live: RenewTime: Thu, 08 Oct 2026 03:31:46 +0900 | frozen: RenewTime: Thu, 08 Oct 2026 03:31:35 +0900

Differing and not excused by known.txt: 0.
