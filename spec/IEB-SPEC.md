# Incident Evidence Bundle (IEB) — Reference Bundle Layout

> **Status: DRAFT / placeholder.** The full versioned schema is authored in **Ship 1**.
> This is a **reference bundle layout**, not (yet) a "standard" — that word is earned only
> when an independent producer or consumer adopts it (see
> [`../docs/design-review-round2.md`](../docs/design-review-round2.md), Path C).

The IEB is a **portable** representation of a single Kubernetes incident. Kairn is its
reference producer; the layout is intended to be readable by other tools (e.g. HolmesGPT,
k8sgpt, homegrown scripts).

## Container

- A `.ieb` file is a **tar archive compressed with zstd**.
- All paths are relative; no absolute paths, no symlinks.

## Layout (v0)

```
manifest.json        # schema version, bound incident identity, hash tree, coverage, image digest
timeline.json        # normalized time-sorted events across sources        [collector: events]
events.json          # raw Kubernetes events for the target               [collector: events]
resources/           # point-in-time JSON of the pod + owner chain         [collector: resources]
  pod.json           #   Pod → ReplicaSet → Deployment
  replicaset.json
  deployment.json
logs/                # bounded log tails                                   [collector: logs]
  index.json         #   which instance each file came from + gaps (see below)
  <container>-current.log
  <container>-previous.log   # last-terminated instance (the timing-sensitive win)
changes.json         # change indicators (generation, managedFields incl. subresource,
                     #   revision)                                        [collector: changes]
diffs/               # before/after pod-template diffs                     [collector: changes]
  index.json         #   expected objects, one entry per revision pair, status, timing, actor
  <ns>/<Kind>/<name>/<n>.json   # field changes of one pair (pending.json: unrolled edits)
metrics/             # PromQL range snapshots (optional)                  [collector: metrics]
  index.json         #   queried range, step, rendered queries, per-query status
  <name>.json        #   raw Prometheus query_range response, verbatim
redaction.json       # redaction policy version, mode, per-file counts, dropped fields
signature/           # (optional) detached signature over manifest.json — present only if signing enabled
```

### `diffs/`

Before/after diffs of the pod template, read at capture time from the revision history
Kubernetes already keeps: ReplicaSets of a Deployment, ControllerRevisions of a StatefulSet
or DaemonSet. No watch, no stored state. The full
rules and their rationale are in [`../docs/design-change-diff.md`](../docs/design-change-diff.md).

`diffs/index.json`:

```json
{ "normalization": "v1",
  "expected": [ { "namespace": "kairn-demo", "kind": "Deployment", "name": "checkout" } ],
  "entries": [ {
    "namespace": "kairn-demo", "kind": "Deployment", "name": "checkout",
    "status": "ok", "source": "replicaset-history",
    "before": { "revision": "1", "object": "ReplicaSet/checkout-5c8f4588f5" },
    "after":  { "revision": "2", "object": "ReplicaSet/checkout-7f8b6b66f8" },
    "changed_at": "…", "changed_at_source": "creationTimestamp",
    "seconds_relative_to_firing": -2, "after_firing": false, "in_range": true,
    "actor": "demo-deployer", "actor_kind": "fieldManager (client-asserted)",
    "pod_revision_is_current": true, "kind_of_change": "spec", "warnings": [],
    "summary": ["containers[name=app].env[name=CACHE_WARMUP].value: lazy → eager"],
    "file": "diffs/kairn-demo/Deployment/checkout/0.json" } ] }
```

- `status`: `ok` | `no_change` | `before_unknown` (e.g. first revision, pruned history) |
  `unsupported_kind` (owners other than Deployment/StatefulSet/DaemonSet, e.g. Job, Argo
  Rollout) | `error`. Only `error` makes
  the bundle PARTIAL. Every object in `expected` has an entry.
- `source`: `replicaset-history`; `controllerrevision` (StatefulSet/DaemonSet: the
  revision's `.data` template, unwrapped from its `$patch: replace` wrapper); or
  `deployment-spec-pending` for a template edit the controller has not rolled out.
- **Which pairs:** every revision activated in `[window.start, capture]`, each against the
  revision before it; if none, the pod's own revision with `in_range: false`.
- **When** (`changed_at_source`): `creationTimestamp` for a ReplicaSet never reused (exact);
  `event` for a reused one (rollback): the latest "Scaled up replica set … from 0" of the
  Deployment, accepted only with a matching predecessor "… to 0"; otherwise `unknown`
  (`changed_at`, `in_range`, `after_firing` are `null`). Scaling (HPA, scale to zero and
  back) never counts as a change. For ControllerRevisions, `managedFields`: the revision's
  data is immutable and only the controller writes it, so its latest write is when it
  became live (a rollback re-uses the revision and bumps its number).
- `source: "configmap-rename"` *(opt-in, `diffs.configMaps`)*: when a pod-template diff
  changed which ConfigMap is referenced (`volumes[…].configMap.name`,
  `envFrom[…].configMapRef.name`, `…configMapKeyRef.name`), both ConfigMaps are read and
  diffed key by key (`data[key]`, `binaryData[key]`). Multi-line values also list the
  `lines_removed` / `lines_added`. Values are redacted per line (YAML, properties, `.env`,
  JSON per inner key); `binaryData` is always `"<redacted>"`. A pruned old ConfigMap gives
  `before_unknown`. `referenced_by` names the workload and field.
- `notes` on a workload entry: a changed `checksum/*` template annotation (Helm's "roll on
  config change") means a ConfigMap/Secret was overwritten in place; its previous content is
  not retained, and the note says so rather than leaving a silent gap.
- `probable_default: true` on an `add` whose value is the Kubernetes default for that field:
  ControllerRevision data is never re-defaulted, so after an API server upgrade a newer
  revision can carry defaults the older one lacks.
- **Who:** the Deployment's `f:spec.f:template` managedFields owner with the latest write in a
  window around the change time; `null` with `actor_reason` when nothing matches. It is a
  client-asserted field manager, not an authenticated identity (see the API audit log).
- `warnings`: a ReplicaSet template written by a manager other than the controller
  (a spoofable hint of an out-of-band edit).

A change file is a list of `{op, display, path_before?, path_after?, before?, after?,
changed}`. `display` is the normative identity: list elements are addressed by merge key
(`containers[name=app]`, `ports[containerPort=8080,protocol=TCP]`, with `\ ] = ,`
escaped), so reordering is not a change. `path_before`/`path_after` are RFC 6901 pointers
into each side, absent where the element does not exist. It is not an RFC 6902 patch.
Values are redacted with the same policy as `resources/`; `changed` stays true when both
sides are `"<redacted>"`.

### Redaction and `redaction.json`

Captured objects pass through redaction **before** they are written, so every file only ever
holds redacted values. Policy v1 is best-effort (see `docs/design-change-diff.md`); `strict`
mode exists for deployments that need a guarantee.

| Candidate (pod spec / pod template / object metadata) | Name rule | Value rule |
|---|:--:|:--:|
| `env[].value` (name = `env[].name`) | ✅ | ✅ per token |
| `command[]`, `args[]`, lifecycle and probe `exec.command[]` | ✅ on `--name=v`, `-Dname=v`, `NAME=v`, `-name v`, `-u user:pass` | ✅ per token |
| lifecycle/probe `httpGet.httpHeaders[].value` (name = header) | ✅ | ✅ |
| `metadata.annotations`, `spec.template.metadata.annotations` (except `*.kubernetes.io/*`, `*.k8s.io/*`) | ✅ | ✅; JSON values per inner key |
| event `message` (events.json, timeline.json) | ✅ on `name=v` tokens | ✅ per token |
| `kubectl.kubernetes.io/last-applied-configuration` | dropped | — |
| logs/, metrics/, everything else (images, ids, names, status) | not redacted | — |

- **Name rule.** The name is split into tokens on separators and camelCase. *Strong*
  tokens (`password passwd pass pwd passphrase secret credential private dsn authorization
  bearer`, glued forms like `PGPASSWORD`, pairs like `api key`, `client secret`, `access
  token`) redact the value unless it is a boolean or a duration. *Weak* tokens (`key token
  auth cert signature`) additionally let integers, short `[a-z0-9_-]` enums, absolute paths,
  and credential-free URLs through (`AUTH_ENABLED=true`, `CACHE_KEY_PREFIX=checkout`).
- **Value rule** (any name): hex ≥ 32 chars with entropy ≥ 3.0 bits/char; base64(url) ≥ 24
  chars with entropy ≥ 4.0; JWTs; known credential prefixes (`AKIA…`, `ghp_`, `glpat-`,
  `xox?-`, `sk-`, `sk_live_`, `-----BEGIN`, …). Canonical UUIDs are ids, not caught by the
  value rule (a UUID under a secret name still is).
- **URLs:** the userinfo password, credential query parameters (`password token sig
  X-Amz-Signature …`), and secret-looking path segments are redacted; scheme, host, and path
  stay readable.
- A redacted value becomes `"<redacted>"`. No hash and no length are emitted.

`redaction.json`:

```json
{ "policy_version": "v1", "mode": "default", "plaintext_names": [],
  "redacted_values": { "resources/pod.json": 2, "resources/replicaset.json": 2 },
  "dropped_fields": [],
  "not_redacted": ["logs/", "metrics/"] }
```

`kairn verify` prints a warning for a bundle captured with `mode: off`, and a note for a
bundle without `redaction.json` (pre-v0.2: env values were not redacted).

### `metrics/index.json`

```json
{ "prometheus_url": "http://prometheus.monitoring:9090",
  "start": "2026-09-19T03:46:03Z", "end": "2026-09-19T03:51:03Z",
  "end_capped_at_capture": true, "step_seconds": 5,
  "queries": [
    { "name": "memory_working_set_bytes", "query": "container_memory_working_set_bytes{namespace=\"kairn-demo\",pod=\"checkout-…\",…}",
      "status": "ok", "series": 1, "file": "metrics/memory_working_set_bytes.json" },
    { "name": "memory_limit_bytes", "query": "…", "status": "ok", "series": 0, "file": "…" },
    { "name": "custom", "query": "…", "status": "error", "error": "HTTP 400: bad_data parse error …" }
  ] }
```

- The range is `[firing − pre, min(firing + post, capture time)]`. Metrics are the only
  source that honestly reaches **before** the alert, because Prometheus kept the history.
  The post side ends at capture time, and `end_capped_at_capture` says so.
- `query` is the rendered PromQL that was actually sent, after target substitution.
- `series: 0` is a successful empty answer (e.g. kube-state-metrics not installed), which
  is different from `status: "error"`. Any error means the collector did not complete, so
  the bundle is PARTIAL.
- `<name>.json` is the raw API response, kept verbatim so any tool can re-plot it.

### `logs/index.json`

Log files alone are ambiguous, because of two kubelet behaviors observed on real clusters:

- In a fast crash loop the **current** instance is often already `terminated` as well, so
  the crash's last words can be in `-current.log`, not `-previous.log`.
- The kubelet keeps one dead instance per container, so the previous instance can be
  garbage-collected **within seconds**. The log API then returns HTTP 200 with a one-line
  error as the body. A producer must not seal that string as log content.

So each container gets an entry listing its instances:

```json
{ "containers": [ { "container": "app", "instances": [
  { "which": "current",  "file": "logs/app-current.log", "state": "terminated",
    "container_id": "containerd://be73…", "reason": "OOMKilled", "exit_code": 137,
    "finished_at": "2026-09-18T14:17:00+00:00" },
  { "which": "previous", "file": null, "state": "terminated",
    "container_id": "containerd://3201…", "reason": "OOMKilled", "exit_code": 137,
    "finished_at": "…", "unavailable": "unable to retrieve container logs for containerd://3201…" }
] } ] }
```

`file` is `null` when nothing was captured, with the reason in `unavailable`. A consumer
looking for "the crash's last words" takes the `terminated` instance with the latest
`finished_at` that has a `file`. A `previous` entry appears only if the container has
restarted at least once.

`kairn verify` accepts either a `.ieb` file (unpacked to a temp dir, with
path-traversal/symlink hardening) or an already-unpacked directory. Packing happens *after*
sealing, so the tar byte-stream is never what the hash tree covers.

## Hashing & signing — decided by construction (avoid the canonicalization trap)

These rules exist so the sealer and `kairn verify` agree on bytes **without** JSON
canonicalization (JCS) or reproducible-tar machinery:

1. **Hash file *contents* individually**, never the tar byte-stream. Tar ordering,
   timestamps, and zstd settings are therefore irrelevant to the hash tree.
2. `manifest.json.hash_tree` = a map of `path → sha256(contents)` plus a **root** =
   `sha256` over the entries **sorted by path**.
3. **The signed payload is the literal bytes of `manifest.json`.** Because the manifest
   contains the hash tree over every other file, signing it verbatim covers the whole
   bundle. This is exactly cosign's blob model — no canonicalization needed.
4. Cross-capture reproducibility is **not** required; only sealer↔verifier agreement on the
   *same* bytes matters.

## `manifest.json` (fields, to be schematized in Ship 1)

- `schema_version`
- `incident`: `{ id (unique, non-reusable), cluster_id, trigger: { rule, firing_ts },
  window: { start, end } }` — this tuple is **bound into the manifest** (and thus the
  signature, when signing is on) for replay/substitution defense.
- `producer`: `{ kairn_version, image_digest }` — image digest is **self-reported**
  (unverified) in v0.1; signed SLSA provenance binding it is v0.2.
- `hash_tree`: `{ files: {path: sha256}, root: sha256 }`
- `coverage`: `{ collectors_run[], collectors_intended[], score }`
- `timing`: `{ capture_started, sealed_at, capture_to_seal_latency }` — **self-asserted by
  the controller clock in v0.1; no independent time anchor** (see `../DESIGN.md` §5).

## Verification

`kairn verify <bundle> --cluster <id> --incident <id> [--key <trusted.pub>]`:
1. recompute per-file `sha256`, check against `manifest.hash_tree` (and the sorted root);
2. check caller-asserted `{cluster, incident}` against the manifest — **fail closed** on mismatch;
3. report coverage — **non-zero exit if `PARTIAL`**;
4. check the signature over `manifest.json`:

| `--key` given? | bundle signed? | result |
|---|---|---|
| yes | yes, by that key | `signed:trusted-key`: integrity + producer authenticity |
| yes | by another key, or invalid | **FAILED** |
| yes | no | **FAILED** |
| no | yes | `signed:unpinned`: self-consistent only, does not affect the verdict |
| no | no | `unsigned` |

**Authenticity comes only from a key the verifier obtained out of band.** The
`signature/cosign.pub` a producer embeds is a convenience for tools like openssl and is
never trusted by `kairn verify`: anyone able to rewrite a bundle can re-seal it with their
own key and swap that file. A self-consistent signature without `--key` is therefore
reported as `unpinned`, never as valid. (Earlier drafts verified against the embedded key.
That is the forgery this rule closes, and a unit test now covers it.)

`kairn keygen` writes a key pair in the formats this expects: `kairn.key` (PKCS#8 PEM, for
the controller's Secret) and `kairn.pub` (SPKI PEM, for `--key`).

**cosign interop is pinned.** When signing is enabled, the signature is cosign-compatible
ECDSA-P256 over `manifest.json`, verifiable with **cosign v2.x**:

```
cosign verify-blob --key <pub> --signature <sig> --insecure-ignore-tlog manifest.json
```

**Signature encoding (critical, get it right the first time):** cosign expects base64 of
the **ASN.1 DER** ECDSA signature (P-256, SHA-256) — **not** the fixed-width 64-byte
IEEE-P1363 `r‖s` form. With RustCrypto `ecdsa`/`p256`, `Signature::to_bytes()` yields P1363
(cosign rejects it); use **`signature.to_der()`** then base64. The crate already normalizes
to **low-S**, which verifiers expect. Private key = PKCS#8 PEM; publish the public key as
**SPKI PEM** via `VerifyingKey::to_public_key_pem(LineEnding::LF)` (that's what
`cosign --key cosign.pub` consumes).

**Conformance anchor = openssl (not cosign), decided after testing against real binaries.**
cosign **v3** (verified against 3.1.3) has **removed the detached `--signature` flag** —
`verify-blob` now requires a `--bundle` (`.sigstore.json` protobuf) even with `--key`. So a
pinned-cosign-CLI gate is a moving target. Instead the v0.1 conformance gate verifies the
signature with **openssl** — a neutral, version-stable check that the bytes are a correct
ECDSA-P256-SHA256 DER signature over `manifest.json`:

```
base64 -D -i signature/manifest.sig -o sig.der           # (Linux: base64 -d)
openssl dgst -sha256 -verify signature/cosign.pub -signature sig.der manifest.json
# -> "Verified OK"
```

This is run by `scripts/verify-conformance.sh` and gated in CI. Emitting a cosign **v3**
`.sigstore.json` bundle for direct `cosign verify-blob --bundle --key` interop is a **v0.2**
item, tied to `sigstore-rs` maturing. Our signature is already standard-correct (openssl
confirms), so v3 interop is packaging, not cryptography.

## Signing configs (see `../DESIGN.md` §5 for the full capability matrix)

| Config | Integrity | Authenticity | Independent time | Air-gap | Availability |
|---|:--:|:--:|:--:|:--:|---|
| unsigned (default) | ✅ | ❌ | ❌ | ✅ | v0.1 |
| static-key ECDSA | ✅ | ✅ | ❌ | ✅ | v0.1 (opt-in) |
| KMS ECDSA | ✅ | ✅ | ❌ | ✅ | v0.2 |
| + RFC 3161 TSA | ✅ | ✅ | ✅ | ✅ | v0.3 |
| keyless + Rekor | ✅ | ✅ | ✅ | ❌ | v0.3 (spike) |
