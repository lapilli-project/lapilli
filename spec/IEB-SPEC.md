# Incident Evidence Bundle (IEB) — Reference Bundle Layout

> **Status: `lapilli.dev/ieb/v1`**, frozen from the first release (v0.1.0) under
> [`../docs/COMPATIBILITY.md`](../docs/COMPATIBILITY.md). The verification contract below is
> normative. This is a **reference bundle layout**, not (yet) a "standard" — that word is earned only
> when an independent producer or consumer adopts it (see
> [`../docs/design-review-round2.md`](../docs/design-review-round2.md), Path C).

The IEB is a **portable** representation of a single Kubernetes incident. Lapilli is its
reference producer; the layout is intended to be readable by other tools (e.g. HolmesGPT,
k8sgpt, homegrown scripts).

## Container

- A `.ieb` file is a **tar archive compressed with zstd**.
- All paths are relative; no absolute paths, no symlinks.

## Layout (`ieb/v1`)

```
manifest.json        # schema version, bound incident identity, hash tree, coverage, image digest
timeline.json        # normalized time-sorted events across sources        [collector: events]
events.json          # raw Kubernetes events for the target               [collector: events]
resources/           # point-in-time JSON of the pod + owner chain         [collector: resources]
  pod.json           #   Pod → ReplicaSet → Deployment
  replicaset.json
  deployment.json
  statefulset.json   #   (optional) when a StatefulSet owns the pod directly
  daemonset.json     #   (optional) when a DaemonSet owns the pod directly
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
  "expected": [ { "namespace": "lapilli-demo", "kind": "Deployment", "name": "checkout" } ],
  "entries": [ {
    "namespace": "lapilli-demo", "kind": "Deployment", "name": "checkout",
    "status": "ok", "source": "replicaset-history",
    "before": { "revision": "1", "object": "ReplicaSet/checkout-5c8f4588f5" },
    "after":  { "revision": "2", "object": "ReplicaSet/checkout-7f8b6b66f8" },
    "changed_at": "…", "changed_at_source": "creationTimestamp",
    "seconds_relative_to_firing": -2, "after_firing": false, "in_range": true,
    "actor": "demo-deployer", "actor_kind": "fieldManager (client-asserted)",
    "pod_revision_is_current": true, "kind_of_change": "spec", "warnings": [],
    "summary": ["containers[name=app].env[name=CACHE_WARMUP].value: lazy → eager"],
    "file": "diffs/lapilli-demo/Deployment/checkout/0.json" } ] }
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

`lapilli verify` prints a warning for a bundle captured with `mode: off`. A bundle **without**
`redaction.json` is **FAILED** — exit 1, problem code `manifest`, plus `integrity` when the hash
tree still lists the file — not merely noted: §7 makes the file required, and the frozen fixture
`fail-no-redaction.ieb` has pinned that verdict, with both codes, since v0.1.0.

> An earlier version of this sentence said a missing `redaction.json` was noted. It contradicted
> §7 of this same document, and the fixture had been enforcing §7 all along.

### `metrics/index.json`

```json
{ "prometheus_url": "http://prometheus.monitoring:9090",
  "start": "2026-09-19T03:46:03Z", "end": "2026-09-19T03:51:03Z",
  "end_capped_at_capture": true, "step_seconds": 5,
  "queries": [
    { "name": "memory_working_set_bytes", "query": "container_memory_working_set_bytes{namespace=\"lapilli-demo\",pod=\"checkout-…\",…}",
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

Two kinds of `unavailable` are not the same, and rule 6's producer obligation separates them.
A kubelet in-band report — HTTP 200 with a one-line error, the instance was garbage-collected —
is a fact about the workload, so the collector did its job and `logs` stays in
`collectors_run`. **Any other failure to read a log is an error in the sense of rule 6** — the
log API refusing (`403` because `pods/log` was not granted), the API server erroring, a
timeout — **and the producer MUST then leave `logs` out of `collectors_run`**, making the
bundle PARTIAL. Otherwise a bundle whose every entry is `"…403 Forbidden…"` verifies OK at
100% coverage, which is what the reference producer did before
[`../docs/design-review-round24.md`](../docs/design-review-round24.md) §3.

`lapilli verify` accepts either a `.ieb` file or an already-unpacked directory. A `.ieb` is
**streamed and never extracted**: entries are hashed as they pass, in one pass, with no seeking
and no temp directory. That is why the reader rules below are written about *entries* rather than
about files on disk, and why the traversal defence is a name check on each entry name
(`hashtree::check_path`) rather than symlink hardening on an extracted tree. `lapilli unpack`
extracts; verification does not.

Packing happens *after* sealing, so the tar byte-stream is never what the hash tree covers.

> An earlier version of this paragraph said a `.ieb` was "unpacked to a temp dir, with
> path-traversal/symlink hardening". That was never how the verifier worked — `verify.rs`'s own
> module note reads "a `.ieb` file is **not extracted**" — and it mattered, because it pointed an
> independent implementer at a different verification model with a different threat surface.

## The `ieb/v1` verification contract (normative)

Frozen for `lapilli.dev/ieb/v1`. A conforming verifier implements exactly these rules; a
conforming producer writes bundles that pass them. The words MUST/MUST NOT are normative.

### 1. Container

- A `.ieb` file is a tar archive compressed with zstd. Entry order, timestamps, owners and
  compression settings carry no meaning (contents are hashed individually, rule 3).
- The archive is **plain ustar**: entries MUST be regular files (tar types `0`, `\0`, `7`)
  or directories (`5`). Pax extended headers (`x`, `g`), GNU long-name/long-link records
  (`L`, `K`), links, sparse and other special entries make the bundle FAILED. (Extension
  records can override sizes and names, so different readers would see different files.)
- An entry's path is the ustar `prefix` + `/` + `name` (or `name` alone), so every path
  fits in ≤ 255 bytes split at a `/` into a prefix ≤ 155 and a name ≤ 100.
- One leading `./` on an entry name is stripped. A file entry whose name is empty, ends in
  `/`, starts with `/`, or has a `.` or `..` segment makes the bundle FAILED.
- **No path may occur twice, including `manifest.json` and files under `signature/`**, no
  two paths may be equal when compared ASCII-case-insensitively, and no file path may also
  be a directory of another path or a directory entry (`logs` and `logs/index.json`), so the
  archive always unpacks to the same set of files. `Manifest.json`, a
  `signature` file, or a first segment `Signature/` (any case variant of a reserved name)
  make the bundle FAILED.
- A verifier MAY also verify an unpacked directory; the same rules and limits apply, so
  both forms get the same verdict.

### 2. Paths

Every file path other than `manifest.json` and the files under `signature/` MUST:

- be relative, `/`-separated, and valid UTF-8;
- consist of segments of `[A-Za-z0-9._-]` only, none empty, `.` or `..`;
- be unique when compared ASCII-case-insensitively (so the bundle extracts identically on
  case-insensitive filesystems).

### 3. Hash tree

- For every file except `manifest.json` and those under `signature/`:
  `hash = lowercase_hex(SHA-256(file contents))`.
- `manifest.json` → `hash_tree.files` is an object `{path: hash}` that MUST list exactly
  those files: a listed file that is absent, a hash mismatch, or a file present but not
  listed makes the bundle FAILED.
- `hash_tree.root = lowercase_hex(SHA-256(concat over files, sorted by the UTF-8 bytes of
  the path, of  path + ":" + hash + "\n"))`. A root that doesn't match `files` is FAILED.

### 4. Outside the tree

- `manifest.json` (it contains the tree).
- Under `signature/`: `manifest.sig` and `cosign.pub`, both optional.
- Under `signature/ext/`: reserved for later signature-adjacent artifacts that must sit
  outside the tree because they cover the manifest (a sigstore bundle, an RFC 3161 token).
  Their paths MUST follow rule 2; an `ieb/v1` verifier reports but does not check them.
- Any other file under `signature/` makes the bundle FAILED.

### 5. Manifest

`manifest.json` is a JSON object. A duplicate member name anywhere in it makes the bundle
FAILED (otherwise one reader could use the first value and another the last). Readers MUST
ignore fields they don't know. Fields:

| Field | Meaning |
|---|---|
| `schema_version` | `"lapilli.dev/ieb/v1"`. Checked first (rule 9). |
| `incident` | `{id, cluster_id, trigger: {rule, firing_ts}, window: {start, end}, target?: {namespace, pod}}`; bound context, compared when the caller asserts it. `target` is optional and additive (a bundle sealed before it existed has none): the pod the capture was about, so a lookup by what an alert carries never unpacks a bundle. It is **not** compared by `verify`; `resources/pod.json` is the evidence, this is the index. |
| `producer` | `{version, image_digest}`; self-reported |
| `signing` | `null` (unsigned) or `{alg: "ecdsa-p256-sha256", key_id}` where `key_id = lowercase_hex(SHA-256(DER of the SubjectPublicKeyInfo with the EC point **uncompressed**))`, i.e. the 91-byte DER for P-256, whatever form the key file uses |
| `hash_tree` | rule 3 |
| `coverage` | `{collectors_run: [..], collectors_intended: [..], deferred?: [..]}` (rule 6) |
| `timing` | `{capture_started, sealed_at, capture_to_seal_ms}`; self-asserted by the producer clock |

### 6. Coverage and required files

- `collectors_run` and `collectors_intended` MUST NOT contain duplicates, and every name in
  `collectors_run` MUST be in `collectors_intended`; otherwise FAILED (malformed).
- The bundle is **PARTIAL** if and only if some name in `collectors_intended` is not in
  `collectors_run`. Nothing else decides PARTIAL: `status` values in index files are
  informational, and a producer that records an error there MUST leave that collector out
  of `collectors_run`.
- `coverage.deferred` (optional; absent, `null` and `[]` are the same) names collectors the
  producer **chose not to intend** because the data is kept elsewhere — a log shipper, an
  event exporter, Prometheus. It is neither "failed" nor "intended", and it exists so that a
  bundle that intends little cannot read as a full capture. Two rules are FAILED (malformed)
  when broken, because an unchecked declaration is worse than none:
  - `deferred` MUST NOT contain duplicates;
  - `deferred` and `collectors_intended` MUST be disjoint — a collector is deferred or
    intended, never both (and since `collectors_run ⊆ collectors_intended`, never run).

  A name in `deferred` that this verifier does not know is a `notice`, **not** FAILED:
  collector names are additive within the major (`docs/COMPATIBILITY.md`), so a verifier
  that failed a bundle for deferring a collector newer than itself would fail every future
  bundle — the same rule that lets an unknown name stand in `collectors_run` applies here.

  A non-empty `deferred` does **not** change the verdict: it is a true record of an
  operator's decision, not a defect. A verifier MUST report it (a `notice` problem naming
  the collectors, and the set itself in machine-readable output) so that OK and 100%
  coverage are never the whole of what a reader sees. A verifier SHOULD also notice an
  empty `collectors_intended` for the same reason: a score of 100% of nothing is not a
  capture.
- A collector listed in `collectors_run` MUST have written these files (else FAILED);
  collector names not in this table have no requirement:

  | Collector | Required files |
  |---|---|
  | `logs` | `logs/index.json` |
  | `resources` | `resources/pod.json` |
  | `events` | `events.json`, `timeline.json` |
  | `changes` | `changes.json`, `diffs/index.json` |
  | `metrics` | `metrics/index.json` |

### 7. Redaction record

`redaction.json` MUST be present (it is in the tree like any file). Its `mode` is one of
`default`, `strict`, `off`; a reader MUST treat any other value as `off`.

### 8. Signature

- The signature is `base64(DER(ECDSA-P256-SHA256(literal bytes of manifest.json)))` in
  `signature/manifest.sig`.
- `signing` non-null and `manifest.sig` absent → FAILED. `signing` null and `manifest.sig`
  present → FAILED.
- With a caller-supplied public key: the bundle MUST be signed, `signing.key_id` MUST equal
  the key's id, and the signature MUST verify with that key; otherwise FAILED. This is the
  only way to establish who sealed a bundle.
- Without a caller key: a present signature is checked for self-consistency against
  `signature/cosign.pub` when present (its id MUST equal `signing.key_id`); the result is
  "unpinned" and never establishes the signer. An unknown `alg` means authenticity is not
  established (FAILED only when a caller key was supplied).

### 9. Version dispatch and verdicts

- A verifier reads `schema_version` before verifying anything else. `lapilli.dev/ieb/v1` →
  these rules. `lapilli.dev/ieb/v0` (pre-release), or `lapilli.dev/ieb/v<N>` with `<N>` matching
  `[1-9][0-9]*` that it does not know → **cannot evaluate**. Anything else (missing, a
  non-Lapilli value, `v01`, surrounding whitespace), an unreadable or malformed manifest, or a
  corrupt or truncated archive → FAILED.
- Verdicts and `lapilli verify` exit codes: OK `0`, FAILED `1`, PARTIAL `2`, cannot evaluate
  `3` (also: input unreadable, over the limits); usage errors `64`. FAILED takes precedence
  over PARTIAL.
- Caller-asserted context (`--cluster`, `--incident`) that doesn't match `incident` →
  FAILED.

### 10. Limits

A producer MUST NOT write more than 50,000 files or 1 GiB (sum of file sizes), nor a
`manifest.json`, `redaction.json` or `signature/` file larger than 16 MiB. A verifier's
limits MUST NOT be lower; a bundle over them (in either form) is **cannot evaluate**.

### Why these rules (non-normative)

Contents are hashed individually and the signed payload is the literal manifest bytes, so
sealer and verifier agree on bytes without JSON canonicalization or reproducible tar: this
is exactly cosign's blob model. Cross-capture reproducibility is not required. Test vectors
for every rule are in `test/fixtures/ieb/`; `test/spec/build_from_spec.py` builds a bundle
from this section alone (it shares no code with Lapilli) and CI verifies it.

## Verification in practice

`lapilli verify <bundle> [--cluster <id>] [--incident <id>] [--key <trusted.pub>]`

| `--key` given? | bundle signed? | result |
|---|---|---|
| yes | yes, by that key | `signed:trusted-key`: integrity + producer authenticity |
| yes | by another key, or invalid | **FAILED** |
| yes | no | **FAILED** |
| no | yes | `signed:unpinned`: self-consistent only, does not affect the verdict |
| no | no | `unsigned` |

**Authenticity comes only from a key the verifier obtained out of band.** The
`signature/cosign.pub` a producer embeds is a convenience for tools like openssl and is
never trusted by `lapilli verify`: anyone able to rewrite a bundle can re-seal it with their
own key and swap that file, or re-seal it unsigned with `signing: null`. Without `--key`, a
bundle proves nothing against anyone who could write to it.

`lapilli keygen` writes a key pair in the formats this expects: `lapilli.key` (PKCS#8 PEM, for
the controller's Secret) and `lapilli.pub` (SPKI PEM, for `--key`).

**cosign interop is pinned.** When signing is enabled, the signature is cosign-compatible
ECDSA-P256 over `manifest.json`, verifiable with **cosign v2.x**:

```
cosign verify-blob --key <pub> --signature <sig> --insecure-ignore-tlog manifest.json
```

**Signature encoding (critical, get it right the first time):** cosign expects base64 of
the **ASN.1 DER** ECDSA signature (P-256, SHA-256) — **not** the fixed-width 64-byte
IEEE-P1363 `r‖s` form. With RustCrypto `ecdsa`/`p256`, `Signature::to_bytes()` yields P1363
(cosign rejects it); use **`signature.to_der()`** then base64. Producers **should** emit the
canonical **low-S** form (`s ≤ n/2`; RustCrypto's p256 does not normalize by itself, and a
KMS doesn't promise it, so normalize explicitly). Verifiers **must** accept both forms, as
openssl, Go and cosign do: `(r, n − s)` is the same signature. Private key = PKCS#8 PEM; publish the public key as
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
