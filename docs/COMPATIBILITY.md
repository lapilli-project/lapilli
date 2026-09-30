# Compatibility policy

Status: **v3 — hardened by loop engineering** (log:
[`design-review-round5.md`](design-review-round5.md); amended since by rounds 8
(`verify --output json`), 18 (`lapilli postmortem`), 19 (`status.message`) and 24/25
(`coverage.deferred`) — `git log -- docs/COMPATIBILITY.md`). **In effect since v0.1.0**, tagged
2026-09-28. Every item in the "Gate before v0.1.0" column exists and runs (`ci.yml`: fixtures,
spec-only producer, MSRV, CRD drift, Helm lint/render, attribution, advisories, rustdoc, DCO;
`release-gate.yml`: E2E on 1.30 and 1.37; `release.yml`: the three CLI targets with checksums;
`test/e2e/run.sh`: the documented-series step). The table is kept as the record of what was
required. Before 1.0 a minor release may still change the alpha surfaces this document names as
alpha — `0.2.0` did, and its **Migration** section in `CHANGELOG.md` says what an upgrade needs. This policy is provided under the project's Apache-2.0 license (§7–8: no warranty,
no liability); it states intent and practice, not a guarantee.
**Security fixes take precedence over everything in this document.**

## Commitments (from v0.1.0)

- **Reading bundles.** Newer releases of `lapilli verify` are intended to keep reading every
  released bundle format, using that format's own rules (§1).
- **`lapilli verify` exit codes** (§2): `0` OK · `1` FAILED · `2` PARTIAL · `3` cannot
  evaluate · `64` usage error. **Only 0 means the bundle passed.** Treat 2 as acceptable
  only if you opt in; reject on everything else:

  ```sh
  lapilli verify bundle.ieb --key producer.pub
  case $? in 0) echo ok ;; 2) echo partial ;; *) echo reject; exit 1 ;; esac
  ```
- **The `ieb/v1` verification contract** (§1): hashing, path rules, signing format.
- **Controller metrics** (§2): the series in [`metrics.md`](metrics.md) keep their names,
  labels and types; new series may be added.
- **`lapilli verify --output json`** (§2): the `lapilli.dev/verify-result/v1` document and its
  problem codes ([`spec/VERIFY-RESULT.md`](../spec/VERIFY-RESULT.md)). Scripts and SIEMs
  parse this, never the text output.

Everything else (CRDs, chart values, other CLI commands) may still change between minor
releases, always with a CHANGELOG entry and a migration note.

Compatibility here means a newer `lapilli verify` can still check a bundle's integrity as
described in [`spec/IEB-SPEC.md`](../spec/IEB-SPEC.md). It says nothing about the bundle's
fidelity, capture time, or suitability as evidence; see [`DESIGN.md` §5](../DESIGN.md).

## Summary

| Surface | Policy | Gate before v0.1.0 |
|---|---|---|
| Bundle format `ieb/v1` | released majors stay readable (§1) | v1 implemented; golden + negative fixtures checked in CI; a bundle built from the spec alone (not by Lapilli) verifies |
| `lapilli verify` exit codes | stable (above) | CLI tests over the fixtures |
| `lapilli verify --output json` (`verify-result/v1`) | additive-only within v1; problem codes pinned per fixture | CLI tests over the fixtures, in both output modes |
| Controller metrics on `/metrics` | names, labels and types stable; additions allowed | unit test of the exposition + E2E asserts every documented series |
| Other CLI commands/flags | deprecated ≥ 1 minor before removal | CHANGELOG |
| CRDs `lapilli.dev/v1alpha1` | alpha; additive-only within a served version (§3) | reviewer judgement — **nothing mechanical checks this**. The CRD drift check compares the committed manifests against the *current* generator; it has no earlier version as input, so a tightening passes it green |
| Helm chart values | deprecated ≥ 1 minor before removal (§5) | `values.schema.json` |
| Webhook `POST :8080/webhook` | stable; changes deprecated ≥ 1 minor | E2E |
| Kubernetes | tested: 1.30 and 1.37; expected to work: 1.31–1.36 (§4) | E2E on 1.37 per change, on 1.30 and 1.37 before tagging; recorded-event unit tests |
| Prebuilt `lapilli` CLI | linux amd64/arm64, macOS arm64, with checksums | release workflow |
| Building from source | MSRV 1.89, best effort | CI job on 1.89 |

Upgrade testing (previous release → this one) starts with v0.2.0, the first release that
has a predecessor.

## 1. Bundle format (IEB)

### Versions

- `manifest.json` carries `schema_version: "lapilli.dev/ieb/v<major>"`. The first released
  format is **`lapilli.dev/ieb/v1`**. `v0` existed only on unreleased development builds and
  is not supported (exit 3).
- **Additive (same major):** new files, new optional JSON fields, new informational enum
  values, new collector names (with their required files).
- **Breaking (new major):** anything in the verification contract below, the meaning of an
  existing field, removing a file or field.

### The `ieb/v1` verification contract

Frozen for `ieb/v1`, except as described under "When a primitive weakens". IEB-SPEC gives
the exact encodings:

- **Hashing:** SHA-256 of each file's contents; root = SHA-256 over `"<path>:<hex>\n"` for
  every entry in byte order of the path.
- **Container:** a zstd-compressed **plain ustar** archive; no pax or GNU extension
  records, links or special entries.
- **Paths:** relative, `/`-separated, only `[A-Za-z0-9._-]` in each segment, no empty,
  `.` or `..` segments, ≤ 256 bytes (ustar prefix + `/` + name), unique when compared
  case-insensitively, and never both a file and a directory.
- **Outside the tree:** `manifest.json`; under `signature/` `manifest.sig` and `cosign.pub`;
  and `signature/ext/`, reserved so later signature-adjacent artifacts (a sigstore bundle,
  an RFC 3161 token) stay additive. Anything else not listed in the tree is an integrity
  failure.
- **No ambiguity:** no path twice (including `manifest.json`), no case-only variants, no
  duplicate member names in `manifest.json`.
- **Signing:** the manifest declares `signing: {alg: "ecdsa-p256-sha256", key_id}` or
  `null`; `key_id` is the SHA-256 (hex) of the public key's SPKI DER with the point
  uncompressed. The signature is
  ECDSA P-256 SHA-256, DER, base64, over the literal `manifest.json` bytes.
- **Coverage:** `collectors_run` and `collectors_intended` contain no duplicates and `run ⊆
  intended` (otherwise FAILED, malformed). The optional `deferred` contains no duplicates
  and is disjoint from `collectors_intended` (otherwise FAILED, malformed); `null`, absent
  and `[]` are the same; a name in it this verifier does not know is a notice, never a
  failure. PARTIAL ⇔ some intended collector is not in run. Nothing else decides PARTIAL:
  `status` values inside index files are informational, and a producer that records an
  error there leaves that collector out of `collectors_run`; `deferred` never changes the
  verdict.
- **Required files:** each collector in `collectors_run` must have written its files
  (e.g. `changes` → `changes.json` and `diffs/index.json`; the table is in IEB-SPEC).
  This catches a producer that lost a part; it adds nothing against an attacker who can
  re-seal an unsigned bundle.
- **Redaction:** `redaction.json` is required; an unknown `mode` is treated as `off`.
- **Limits:** a producer writes at most 50,000 files and 1 GiB per bundle, and at most
  16 MiB for `manifest.json`, `redaction.json` and each `signature/` file. A verifier's
  default limits are never below that; exceeding them is exit 3 (cannot evaluate), never 1.
- **Verdict mapping:** exit codes above. Once a bundle is recognised as `v1`, anything wrong
  with it (malformed manifest, corrupt or truncated archive, bad entries) is `1`.

### Rules for readers

1. Read only files listed in a verified hash tree.
2. Treat informational enum values you don't know as "unknown".
3. Fail safe on security-relevant fields (unknown redaction `mode` = `off`; unknown
   signing `alg` = authenticity not established).
4. Check `schema_version` first and refuse a major you don't know. This field is
   necessarily read before anything is verified.

### Authenticity comes only from `--key`

Without `--key`, a bundle proves nothing against anyone who could write to it: they can
re-seal it, with or without a signature of their own. The signing declaration catches a
signature lost by accident (a copy tool dropping `signature/`), not an attacker. With
`--key`, the bundle must declare a signature whose `key_id` matches the key and that
verifies; anything else is FAILED.

### When a primitive weakens

Releases keep **reading** old bundles; what they **vouch for** follows current security
knowledge. If an algorithm or rule of a released major is found unsound:

- with `--key`, an authenticity check that relies on it is FAILED, unless the caller passes
  `--allow-deprecated-crypto` (added in the release that first deprecates something);
- without `--key`, the result is reported (`deprecated-crypto`) without changing the verdict;
- if the hash function itself is deprecated, integrity is reported as not established too.

Such changes ship in a minor release with a GitHub Security Advisory.

### Fixtures

Before each release, bundles it produces are added to `test/fixtures/ieb/<release>/` with
their expected exit codes: OK (unsigned and signed with a committed fixture key), PARTIAL,
and negative cases, including modified, missing, unlisted, unlisted under `signature/`,
stripped signature, wrong key, unknown major, v0, path traversal, link, duplicate entry,
case-colliding paths, malformed coverage, corrupt archive and over-limit archive — the full
list is `test/fixtures/ieb/v0.1.0/expected.json` (43 bundles, 47 cases: some bundles are
checked with and without a key). CI checks all of them. Fixtures are not modified after their
release ships.

Carrying every released major forever has a cost; this is reviewed at 1.0 or when a third
major would be introduced, whichever comes first.

## 2. CLI

- Exit codes are listed above. Usage errors exit `64`, so a typo can't read as PARTIAL.
- For a bucket object (`lapilli verify s3://… | gs://… | https://…`), any failure to read it
  (network, 403/404, redirect, the byte limit, a denied version listing) exits `3`, never
  `1`: it says nothing about the bundle. A key written more than once is `1` (FAILED).
- `lapilli postmortem` renders Markdown for people. Its structure, headings and wording are
  **not stable** and may change in any release; `lapilli verify --output json` is the parseable
  surface. Its **exit code is** stable, and is the verdict's: it never contradicts
  `lapilli verify` on the same bundle, which a test pins across every released fixture.
- The human output is not stable; don't parse it (use `--output json`). `lapilli verify` prints the bundle's
  `producer.version`, which bug reports should include.
- Prebuilt binaries with checksums are attached to every release, so an old bundle can be
  checked with the release that produced it as well as with the latest.
- `--output json` prints one `lapilli.dev/verify-result/v1` document on stdout and nothing on
  stderr, for exit codes 0–3 ([`spec/VERIFY-RESULT.md`](../spec/VERIFY-RESULT.md)). Within
  v1, members, problem codes and values of *open* members may be added. Nothing may be
  removed, renamed or retyped, and no condition may move to another code. Messages are not
  stable; codes are. Each fixture's set of codes is pinned in `expected.json`. Any exit
  code other than 0–3 and 64 means "no result": reject.

### Notification bodies

- A route with `format: json` POSTs one `lapilli.dev/notification/v1` object. Within v1,
  members may be **added**; nothing is removed, renamed or retyped. `verdict` is a
  human-readable line and is explicitly **not** stable — a consumer that needs a field should
  read the field, not parse the sentence.
- A route with `format: slack` POSTs Slack Block Kit. The block structure is Slack's, not
  Lapilli's, and may change with it; the guarantee Lapilli makes is narrower and does not change:
  **any string that came from an alert or a workload** is escaped (`&`, `<`, `>`) and rendered
  as `plain_text`, so it can carry neither a ping nor a link. One block — the fenced retrieval
  commands — is `mrkdwn`, and only because every value in it is admin-set or
  pattern-constrained; that is enforced by a character check at render time, and the block
  falls back to `plain_text` if it ever fails.
- The retrieval commands a message prints are documentation, not an interface: they may be
  reworded. `lapilli verify` and `lapilli cat-bundle` themselves are the stable surface —
  `verify` under the commitments above, `cat-bundle` in this section's deprecation-window sense
  (an "other CLI command": deprecated at least one minor before any change, never silently).

### Metrics

- `/metrics` on the health port (8081), Prometheus text format; the series are listed in
  [`metrics.md`](metrics.md).
- Within a major: no series is renamed, retyped or given an extra label, and no label value
  in a documented set disappears. New series and new label values may appear, so a consumer
  must tolerate both.
- Counters reset on restart (they are process counters, as usual). The state-derived gauges
  (`lapilli_captures`, `lapilli_captures_watched`, `lapilli_captures_retired`,
  `lapilli_captures_exported_unretired`, `lapilli_captures_awaiting_seal`,
  `lapilli_export_destinations`, `lapilli_exports_unsettled`) are counted from the API every
  30 s, by one poll, and are absent until the
  first poll succeeds. **Several more gauges are absent rather than zero, each for its own
  reason, and a consumer must treat all of them the same way:** `lapilli_bundle_fs_bytes`
  (absent when `statvfs` on the bundle volume could not be read — a zero there would read as
  "the disk is empty"), `lapilli_permissions_denied`, `lapilli_permissions_unknown` and
  `lapilli_permissions_asked` (absent until the first permission pass completes),
  `lapilli_notify_routes` (absent when no route is configured, so "notification is off" and
  "notification is broken" never read the same), and `lapilli_signing_key_info` (present only
  when a KMS key is pinned, `signing.mode=kms`; absent under `static` and under `none`, the
  default). The conventional
  `process_*` series come from `/proc` and are therefore **absent on any host without it** —
  they are not `lapilli_`-prefixed and are not this project's names, so their meaning is
  whatever Prometheus convention says; what this policy promises is only that they keep those
  conventional names and stay absent rather than zero when they cannot be read. `lapilli_apiserver_poll_ok` comes from the same poll but appears as soon as
  one **returns**: a first poll that fails emits `0`, because a controller that has never reached
  the API server is an outage, not a warm-up. In practice it is there immediately — the poller polls
  before it first sleeps — so the absent window is milliseconds, not an interval. `lapilli_apiserver_last_success_timestamp_seconds`
  needs an actual success and is absent until there has been one. For all of these a consumer must
  treat "absent" as "not known yet", never as `0`.
- A rule that selects on one of these series cannot fire while the series is **missing** — a
  scrape target that has gone away is not a sample of `0`. Alert on `absent()` separately;
  `lapilli_apiserver_polls_total` is emitted from process start and is the intended sentinel.

## 3. Kubernetes API (CRDs)

- `lapilli.dev/v1alpha1` is alpha. Within a served version, changes are additive only (new
  optional fields with defaults). A rename or removal gets a new version (e.g.
  `v1alpha2`) served alongside, with the storage version moved and
  `status.storedVersions` migrated, following the Kubernetes API deprecation policy.
- The served schema includes the validation the generator injects on `CaptureProfile`: a CEL rule
  that refuses a name in both `spec.collectors` and `spec.deferred`, with `maxItems: 16` and
  `maxLength: 64` on both arrays so the rule's cost is a small constant
  (`docs/design-review-round25.md` §2). Tightening it is a breaking change under the rule above;
  the CRD drift check catches only a generator/manifest mismatch, not a tightening.
- The label `lapilli.dev/retired` on an `IncidentCapture` is a control surface, not decoration:
  the controller sets it when nothing is left to do and watches with `!lapilli.dev/retired`, so a
  labelled capture is not delivered to the reconciler until the label is removed, which re-enters
  it immediately (`docs/design-capture-retirement.md`). It is additive: an older controller has no
  selector and treats the label as inert.
- Helm does not upgrade CRDs in `crds/`: upgrading starts with
  `kubectl apply --server-side --force-conflicts -f crds.json` from the release; release notes
  repeat it. `--force-conflicts` is needed because Helm created those objects with its own field
  manager, and a first server-side apply from `kubectl` over the same fields aborts without it.

## 4. Kubernetes versions

- **Tested** end to end: 1.30 and 1.37 (1.37 on every change, both before each release).
  **Expected to work:** 1.31–1.36. Behavior known to differ between versions (e.g. the
  wording of scale events used to time rollbacks) is covered by unit tests with events
  recorded on each tested version.
- The client is compiled with k8s-openapi's `v1_30` API definitions and uses only GA APIs
  (core/v1, apps/v1, events).
- Where behavior differs and Lapilli can't tell, it reports `unknown` rather than guessing.

## 5. Helm chart

- Chart version equals app version.

- `Chart.yaml` declares `kubeVersion: ">=1.30.0-0"`, which is the support claim in §4 and not the
  chart's own minimum: the manifests need only ≥ 1.21 (the projected `serviceAccountToken` volume
  and the `kube-root-ca.crt` ConfigMap in `templates/deployment.yaml`), but the client is compiled
  against k8s-openapi `v1_30` and nobody has run 1.21–1.29, so the floor follows what is supported.
  The `-0` is required: without it a semver range excludes prereleases, and managed control planes
  report versions like `v1.30.5-gke.1234`. There is no upper bound, so a newer Kubernetes installs
  rather than being refused.
- The chart's licence is published as `annotations: artifacthub.io/license` and as
  `charts/lapilli/LICENSE` inside the package. A top-level `license:` key in `Chart.yaml` is **not**
  part of Helm's `chart.Metadata`: it parsed, was ignored, and the chart published for
  `v0.1.0-rc.1` carried no licence at all.
- `values.schema.json` checks types and known keys (allowing `global` for umbrella charts).
  A renamed or removed value keeps working, with a warning in NOTES, for at least one
  minor release.
- The bundle PVC is kept on uninstall and reused on reinstall.

## 6. Integrations

- Alertmanager webhook: `POST :8080/webhook` with `Authorization: Bearer <token>` (on by
  default; see the chart NOTES), payload `version: "4"`, unknown fields ignored;
  the response (`{"captures": [...]}`) only gains fields.
- Prometheus HTTP API v1 (`/api/v1/query_range`) for `metrics/`.
- New signing algorithms are added as additional, declared signatures; every `ieb/v1`
  signed bundle keeps the ECDSA P-256 signature.

## Support window

Supported release lines and how to report vulnerabilities are in
[`SECURITY.md`](../SECURITY.md), the single source. A security fix to `lapilli verify` is
intended to ship in a release that still reads every released major; if a fix ever has to
stop reading something, the advisory says so.

A supported bug report names the Lapilli release, the Kubernetes version, and for bundle
issues the bundle's `producer.version`.

## Changing this policy

This policy is a significant change under [`GOVERNANCE.md`](../GOVERNANCE.md). Changes are
announced in the CHANGELOG at least one minor release before they take effect, which is the
window for community input; security changes are exempt and ship with an advisory. A change
never weakens reading of an already-released format major, except for security reasons,
which are disclosed.

## Not covered

Human-readable output and log messages; bundle file names and directory layout on the PVC;
RBAC and ServiceAccount names; controller environment variables; `IncidentCapture.status`
fields (alpha, see §3); the `lapilli-bundle` crate's Rust API before 1.0.
