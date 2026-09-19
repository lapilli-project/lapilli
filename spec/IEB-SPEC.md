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
metrics/             # PromQL range snapshots (optional)                  [collector: metrics]
  index.json         #   queried range, step, rendered queries, per-query status
  <name>.json        #   raw Prometheus query_range response, verbatim
redaction.json       # (v0.2) policy version + hash, per-file redaction magnitude
signature/           # (optional) detached signature over manifest.json — present only if signing enabled
```

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
