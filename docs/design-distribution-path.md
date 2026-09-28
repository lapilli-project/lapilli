# Design — a distribution path: the bundle as a tool an agent can call

Status: **built; release gate green on kind 1.30 and 1.37 (2026-09-25); round 26 log in `docs/design-review-round26.md`.** Follows the market review's
cheapest experiment (vault `17 Reviews/Lapilli 시장 분석 — Review Round 1`, §5): *a human with
`kubectl` tolerates a missing log; an agent either has the evidence or invents it.* The owner's
instruction for this phase: not an evaluation — pick, focus, build, bring back a result.

## The goal this serves

A flight recorder that seals evidence must also be able to **answer questions about it**, in
its own voice: `lapilli verify` says whether the file is intact, `lapilli postmortem` renders
what it holds for a person, and `lapilli mcp` answers an investigator — a person at a laptop or
an agent in the cluster — through a standard protocol, with Lapilli's own rules about what is
served and what is refused. That is the identity sentence in `DESIGN.md` doing its work, not a
sales channel. Adoption follows from a recorder whose evidence is easy to reach; HolmesGPT,
Claude Code and any other MCP client are consumers of that, and the earlier framing of this
document — "the deliverable is a PR into HolmesGPT's toolset list" — was the market lenses
redefining the audience rather than the pitch, and is withdrawn (`docs/design-review-round27.md`).

## Candidates

| | What it is | Outcome |
|---|---|---|
| **A. k8sgpt custom analyzer** | A gRPC service k8sgpt calls during `analyze` | **Lost.** k8sgpt's model is *scan live resources, emit a finding*; a bundle is a sealed file about the past. Its own MCP server is a consumer of cluster state, not a plugin point. A separate protocol for a poor fit. Both critics agreed this axis was clean. |
| **B. HolmesGPT bash toolset** | YAML tools that are Jinja-templated shell commands over the CLI | **Kept as a second form** (`integrations/holmesgpt/toolset-lapilli.yaml`) for installs without an MCP client. A shell template is the wrong trust boundary for a file reader, so it is not the mechanism. |
| **C. `lapilli mcp`** | The CLI serves bundles over the Model Context Protocol | **Won — after two critics rewrote it.** See the next section. |

## What the first draft got wrong, and what stands now

The first draft was a **stdio** server over **local** files with `list_bundles`/`verify`/
`summary`/`postmortem`, `facts_only()`, and a promise that the agent would "read the file with
its own file tool". Two critics, from the incumbent-user and the frequency lenses, converged on
four objections; all four held and all four are built in.

**1. Serve where the bundles are.** HolmesGPT in a cluster cannot run a stdio subprocess
(its own docs: *"Stdio mode cannot run directly in the Holmes container"*), and the bundles are
on the controller's `ReadWriteOnce` PVC in a distroless image or in a bucket. No shared
filesystem exists. So `lapilli mcp --http` runs as a **second container in the controller pod**
— the same image already carries the `lapilli` binary — with the bundle volume mounted
**read-only**, a bearer token in front of every request, no Kubernetes API access of its own,
reachable at `http://<release>-mcp.<ns>.svc:8082/mcp` (chart: `mcp.enabled`). Stdio stays for a
laptop next to pulled bundles.

**2. Find by what the alert carries.** Every first-draft tool took a `path`, and the listing
exposed an opaque `<cluster>-<16 hex>` id. An agent holds a namespace, a pod and a time.
`find_bundles(namespace, pod | pod-prefix*, rule, since, until)` filters on the manifest — and
to make that possible without unpacking, `incident.target {namespace, pod}` is now in the
manifest (additive; omitted when absent, so every existing bundle and fixture is byte-identical),
and `lapilli-bundle` gained `read_manifest`, which streams a `.ieb` only as far as
`manifest.json`.

**3. Return the evidence, not a headline.** `summary` returned KSM-reconstructible facts and
withheld the only claim the market review left standing. `read_file(path, file)` now serves any
file the **verified hash tree names** — `resources/*.json` (env, args, limits, probes,
annotations, owner chain, redacted at capture), `diffs/index.json` and each diff, the timeline —
and refuses names outside the tree, names that leave the bundle, and any bundle that is not OK or
PARTIAL. An evidence tool does not hand out bytes it could not vouch for.

**4. One content policy, stated exactly.** The draft said "no workload content" and was false
for its own `postmortem`. Every tool now uses `without_log_line()`: redacted spec values, event
messages and cluster-controlled identifiers enter the agent's context — they are what the agent
is for, and the redaction policy already ran on them. The container's **log** is the one channel a
workload controls end to end and is not redacted; it is served only by name, only with
`--allow-logs` (chart `mcp.allowLogs`), and the result says `untrusted: true`.

**"The agent's context" is, in practice, a third-party model provider's API,** and this document
owes that sentence plainly, because it is the reasoning behind `mcp.allowLogs: false` and the
"turn it on knowingly" that the chart says. A client backed by a hosted model — Anthropic, OpenAI,
Google, Azure, Bedrock — sends every tool result it receives to that vendor as prompt text; a
client backed by a model the operator hosts sends none of it anywhere. The server cannot tell the
two apart: the protocol is identical, nothing in a request names the model behind it, and the
bundle leaves through an **inbound** read, so no egress allowlist on the controller pod is in that
path at all (`docs/egress.md`, "Data that leaves by being read"). What follows for the design:

- The enforcement point is **who may connect** (`mcp.networkPolicy.from`, and the bearer token),
  not what the server sends back. There is nothing downstream of the read to enforce with.
- `mcp.allowLogs: false` is the right default for a stronger reason than "untrusted input in a
  context window": the log is the only unredacted file in a bundle, and turning it on decides what
  a third party may hold, not only what an agent may see. `untrusted: true` on the result addresses
  prompt injection; it says nothing about where the bytes end up.
- Both facts belong on every surface where an operator makes this decision, not only here:
  `charts/lapilli/values.yaml` (`mcp.allowLogs`), the chart's `NOTES.txt` at the line that hands
  over the token, `integrations/holmesgpt/README.md`, and `docs/data-handling.md`, which is the
  page that states it once for all surfaces.

A third critic (security/feasibility) added what the tests must be and what the server must
refuse: **only `.ieb` files**, never unpacked directories (a directory can hold symlinks the
renderers would follow; `unpack` refuses link entries, so a staged `.ieb` is link-free by
construction); heavy work on the blocking pool behind a **semaphore of two**; the postmortem's
digest **streamed** instead of read into memory; and a test that speaks the wire protocol itself
rather than gating CI on an unpinned `npx` fetch.

## The tools

| tool | argument | returns |
|---|---|---|
| `find_bundles` | namespace, pod (`checkout-*`), rule, since, until, dir | matches newest first: incident id, cluster, rule, firing time, window, target, collectors run and deferred, path |
| `verify` | path, key? | the `lapilli.dev/verify-result/v1` document — `verdict` first |
| `read_file` | path, file | one hash-tree file from an OK/PARTIAL bundle: JSON parsed, else text; `sha256`, `untrusted`, `redacted_at_capture` |
| `summary` | path | the notification's facts without the log line |
| `postmortem` | path | the Markdown draft and the exit code it would have returned |

## What decides whether it worked

- `crates/lapilli-cli/tests/mcp.rs` — a JSON-RPC client over stdio with no dependency beyond
  serde_json: tool inventory; `verify` on the released fixtures (OK with its deferred set,
  PARTIAL, FAILED); `read_file` refusals (log without `--allow-logs`, name outside the tree,
  traversal, a FAILED bundle); `--allow-logs` flagging `untrusted`; and, on a bundle sealed
  in-test with a target, an object body and a diff — **an alert's namespace + pod prefix +
  window finds it, `resources/pod.json` comes back with the value the diff is about and the
  redaction the capture did, the diff comes back, summary and postmortem carry no log line.**
  Every line the server writes to stdout must parse as a frame.
- `test/e2e/mcp.sh` — the in-cluster shape on kind: `mcp.enabled=true`, 401 without and with a
  wrong token, five tools with the right one, the demo's crash-loop capture found by
  `lapilli-demo` + `checkout-*`, `resources/pod.json` and `diffs/index.json` read from it,
  `logs/**` refused. Reference client: `@modelcontextprotocol/inspector@2.8.0`, pinned.
- `test/mcp/check.sh` — the same over stdio with the reference client, for a laptop.

## What this does not decide

- Whether an agent, once it *can* read a bundle, produces a better investigation. That is the
  owner's discussion with the result in hand. `integrations/holmesgpt/` is one worked example of
  a consumer, kept because it is cheap and concrete; loading it into a running HolmesGPT has
  **not** been verified by this project, and a listing there is not a goal of Lapilli's.
- Per-alert profile selection, phase B, and the rest of the carried-forward list.
