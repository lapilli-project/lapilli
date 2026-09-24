# Design review — round 26 (the distribution path)

Constitution: **v0.6.0**. Target: `docs/design-distribution-path.md`, first draft (uncommitted
at the time; the draft chose a stdio MCP server over local files with four tools and a
`facts_only()` content policy). Three critics in parallel, distinct lenses, the two mandatory
market lenses among them. The implementation ran alongside the wave and folded each finding as
it arrived; this log records the findings against the draft and what stands.

## 0. Gates

Category: strategy → implementation (Principle 8: a claim about one's own work is looped at the
work's tier). Break-even: the deliverable is the project's first integration surface, the thing a
maintainer of another project would look at; a wrong shape here is a first impression spent.
Independence: all Claude-family — calibration only.

## 1. Findings

| # | Lens | Sev | Objection | Disposition |
|---|---|---|---|---|
| F1 | frequency | **BLOCKER** | HolmesGPT in-cluster cannot run stdio MCP (its docs say so), and the bundles sit on the controller's RWO PVC in a distroless image or in a bucket: the per-incident path the draft was built for has **frequency zero**; only the laptop postmortem path remained | **APPLY** — `--http` transport, second container in the controller pod, volume read-only, bearer token, chart `mcp.enabled` |
| I3 | incumbent | **BLOCKER** | The root the server serves is reachable in neither deployment, and the refusal of `s3://` cited a decision the review never made and a command (`lapilli fetch`) that does not exist | **APPLY** — as F1; the false citation is gone |
| F2 / I2 | both | **MAJOR / BLOCKER** | Discovery is "list N bundles and guess": every tool takes a path, the id is opaque, and neither the manifest nor `Summary` carries the pod | **APPLY** — `find_bundles(namespace, pod*, rule, since, until)`; `incident.target` added to the manifest (additive, omitted when absent); `read_manifest` streams only the manifest |
| F3 / I1 | both | MAJOR | The tools returned what KSM and Loki already give and withheld the object body and diff — the only claim the market review left standing | **APPLY** — `read_file` over hash-tree names from OK/PARTIAL bundles |
| I4 / S1 / S2 | incumbent, security | MAJOR / HIGH | "No workload content" was false for `postmortem` (diff values, event messages pass through); the acceptance test asserted `change.field` while `facts_only()` nulls it; the tool description promised what the code withheld | **APPLY** — one policy, `without_log_line()`, stated exactly; test and descriptions match the code |
| I5 | incumbent | MAJOR | The log refusal was a wall in-cluster and a speed bump on a laptop | **APPLY (narrowed)** — `read_file` serves `logs/**` behind `--allow-logs`, flagged `untrusted: true`; the default stays off because the log is the one channel a workload controls end to end |
| S3 | security | HIGH | "That script runs in CI" was a claim ahead of its evidence: no test existed, CI has no node, the inspector is an unpinned network fetch, no fixture carries a diff | **APPLY** — `tests/mcp.rs` speaks JSON-RPC over stdio with serde_json only and seals its own captured bundle; the inspector is pinned to 2.8.0 and used by the E2E and a manual script |
| S4 | security | MEDIUM | A directory bundle under the root can carry symlinks the readers follow; TOCTOU is out of the threat model and should be said | **APPLY** — `.ieb` files only; the module doc says what the sandbox is and is not |
| S5 | security | MEDIUM | Sync handlers block tokio workers, no concurrency cap, the postmortem digest reads the whole file, temp dirs leak on kill | **APPLY** — `spawn_blocking` behind a semaphore of two; streamed digest; the leak on kill is documented, not fixed |
| F4 | frequency | MINOR | The `s3://` refusal's stated reason | **APPLY** — with I3 |
| F5 | frequency | MAJOR | The deliverable that moves the Sandbox metric is a named Lapilli entry in HolmesGPT's toolset list — a PR to another project — and the draft shipped that piece as an unverified side file | **VALID-OUT-OF-SCOPE (owner's)** — the PR is outward-facing and the owner's to send; `integrations/holmesgpt/` is the PR-shaped artifact, and its unverified status is stated in its README |

**REFUTED: 0.** Steelman attempted on I5 (*"the agent's own file tool reads the same bytes, so
the refusal protects nothing"*): true on a laptop, and exactly why the log is now servable on
request; false in-cluster, where the server *is* the only file tool — which is why the default
stays off and the result is flagged. Narrowed, not refuted.

## 2. Clean axes

- Candidate A (k8sgpt analyzer) rejected — both market critics agreed.
- Verify-first with `verdict` as the first key — held on every lens.
- The path sandbox core (`canonicalize` both sides, component-wise `starts_with`) — held; the
  finding was about what the readers did *after* it.
- Stdout discipline after the refactor — clean; the integration test now enforces it on every
  frame.
- rmcp/schemars — no conflict; schemars 1 (CLI, rmcp) and 0.8 (controller CRD derive) coexist.

## 3. What the implementation found that the critics did not

- The rmcp 3 `ServerInfo` alias is deprecated and `InitializeResult` is non-exhaustive: a
  struct literal does not compile; `ServerConfig::new` + field assignment does.
- The per-diff detail file is a JSON **array** of `{display, path_after, before, after,
  changed}`; a test bundle written in an invented object shape made `summary.change.field`
  `None` — the same "generation and verification share a blind spot" trap `summary.rs` already
  documents for its own fixture.

## 4. Verdict

**Applied and proven in-cluster.** Unit and integration suites green (7 wire-protocol tests),
chart lints and renders the container, lean build (`--no-default-features --features mcp`) is
what the image ships. `test/e2e/mcp.sh` on kind v1.37.0, third run: 401 without and with a wrong
token, five tools with the right one; the demo's crash-loop capture found by
`KubePodCrashLooping` + `lapilli-demo` + `checkout-*` with `incident.target` populated by a real
controller; `resources/pod.json` read back with `CACHE_WARMUP=eager` and the redaction the
capture did; `diffs/index.json` with the change; `logs/**` refused. The two runs before it were
the loop doing its job: the image built the CLI without the `mcp` feature (sidecar in
CrashLoopBackOff, found only by a cluster), and the script assumed the newest match was the
crash-loop when run.sh had captured an OOM after it — and a real `firing_ts` carries fractional
seconds and `+00:00`, so the window is now parsed rather than compared as text.

**Full release gate on `174b999` (2026-09-25): `E2E OK` on kind v1.30.0 and v1.37.0, every
suite — diffs, export, kms, notify, deferred, mcp — and `release-check OK`.** Three gate
runs before it failed for reasons outside the change and were each fixed with evidence: clippy
under `--no-default-features` (a field only the `mcp` feature read); quay.io answering 401 to
anonymous pulls of the pinned MinIO images (reproduced on a kept cluster; images now staged from
the host's cache and checked against the pins); and the mcp suite reading `logs/index.json`
from a perishable capture the deferred suite had left as the newest match (it now picks a
capture whose `collectors_run` includes `logs`, the field an agent would use).

The two things only a human can do — load the config into a running HolmesGPT, and open the
conversation with its maintainers — are the result to discuss.
