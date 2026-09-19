# Design Review — Round 9 (KMS signing, and who can get a bundle vouched for)

Constitution: **Loop Engineering Constitution v0.5.0**. Artifact:
[`design-kms.md`](design-kms.md) v0 → v1 → v1.1, plus the capture-refusal code fix in
`crates/kairn-controller/src/reconcile.rs`.

**Outcome: time-boxed, not dry.** R1 had three lenses; R2 was a converge round with one
rotated critic. Every R2 finding is APPLIED and exercised by the E2E, except the KMS parts,
which the implementation's tests will exercise. No R3.

## Round 0

- **Category:** security-sensitive and public. Signing is the only source of producer
  authenticity (`kairn verify --key`).
- **Break-even:** cost ≈ 4 critic runs. The downside is forged evidence accepted as
  authentic, or captures lost to a signing outage. Downside ≫ cost.
- **Sizing:** high, 3 lenses (security, mandatory; cloud-API facts; operator) × 2 rounds.
- **Independence: not achieved (calibration only).** All critics were Claude. Cloud facts
  were checked against the AWS and GCP docs, and both test stacks were run:
  - LocalStack 4.12 (no auth token): ECC_NIST_P256 `DIGEST` signing verified with openssl.
  - `blackwell-systems/gcp-kms-emulator` (Apache-2.0), built from source: P-256
    `asymmetricSign` with CRC32C, verified with openssl.

## R1 — design v0 (three lenses)

| # | Sev | Finding | Disposition |
|---|---|---|---|
| 1 | BLOCKER (security) | **Anyone who can create an `IncidentCapture` gets a sealed, and signed, bundle for any cluster.** Nothing on the seal path compared `spec.clusterId` with the controller's. This is a **v0.1 bug**, not only KMS. Found alongside it: an unsafe `incidentId` made the staging cleanup a path traversal, and a second capture with the same id overwrote the first bundle. | **APPLY, in code, now.** The controller refuses such captures, and the E2E reproduces the old behavior (on the pre-fix image, `ref-cluster` was *exported*). |
| 2 | BLOCKER (operator, API-facts) | "Retry in `Sealing`, keep staging" was impossible: collect, seal and pack ran as one step, any error was final, and a rerun wiped staging. | **APPLY** (design): sealing becomes its own persisted phase, with no re-collection. |
| 3 | MAJOR | The IAM example had `GetPublicKey` in the same statement as a `kms:SigningAlgorithm` condition, which only exists on `Sign`/`Verify`, so it was always denied. | **APPLY:** two statements. |
| 4 | MAJOR (all three lenses) | Alias ARNs: IAM can't be scoped to them, and `UpdateAlias` swaps the key under both the controller and the auditor. | **APPLY:** key ARNs only; the response `KeyId` / `name` must match. |
| 5 | MAJOR (security) | The honesty section overclaimed: the default key policy lets any IAM principal allowed `kms:Sign` sign; CloudTrail records no digest; GCP signing isn't logged by default. | **APPLY:** a required key policy, GCP Data Access logs, a dedicated key, the digest and request id recorded, and the section rewritten. |
| 6 | MAJOR (operator) | Rotation and old-bundle keys: the old public key is unfetchable once the key is disabled. | **APPLY:** `keys/<key_id>.pub` is published, `kairn key fetch`, and a rotation runbook. |
| 7 | MAJOR (operator) | The chart/CRD vocabulary didn't line up; no signing floor; rollback. | **APPLY:** under `kms`, profiles' `signing` is ignored, with no CRD change. |
| 8 | MAJOR (operator) | Preflight only at first use; no observability. | **APPLY:** a startup preflight and Events. `/metrics`: **VALID-OUT-OF-SCOPE** (the controller has no metrics endpoint yet). |
| 9 | MAJOR (API-facts, operator) | LocalStack `latest` needs an auth token; there *are* GCP emulators. | **APPLY:** LocalStack pinned to 4.12, and gcp-kms-emulator; both verified. |
| 10 | MINOR | The low-S rationale was wrong (p256 and cosign accept high-S); `normalize_s()` returns `None` when S is already low. | **APPLY:** normalize for canonical output; tests assert `!s.is_high()`. |

## R2 — converge, on design v1 and the code fix

| Fix | Verdict | Disposition |
|---|---|---|
| Refusals, owner file, `hard_link` | **NEW-BLOCKER** N1: two captures with the same id raced; the check saw only a finished `.ieb`; staging and tmp files were shared; two profiles with different `export.path` could each seal the id. | **APPLY:** an atomic `O_EXCL` claim before anything else; staging and tmp per UID; a single controller bundle root (`export-path-not-allowed`); `reserved-incident-id` for the webhook's ids. |
| Cluster id | **NEW-BLOCKER** N2 (regression): a non-path-safe cluster id made every webhook capture fail. | **APPLY:** `[A-Za-z0-9._-]{1,83}` in the chart schema and at controller start, with a migration note. |
| Readiness gated on KMS | **NEW-BLOCKER** N3: an unready pod loses webhook alerts, the opposite of fail-closed. | **APPLY:** never gate readiness; Events and backoff instead. |
| Sealing phase | **WEAK** / N4: the hash-tree recheck is self-consistency only, so a PVC writer could get a signature for another cluster. | **APPLY:** before `Sign`, check the manifest's cluster, incident and key_id; re-run the refusals on resume; `staging-lost` is terminal. |
| Profiles ignored | **WEAK**: the chart's own profile would render `mode: kms` into a two-value enum; rollback re-collects `Sealing` captures. | **APPLY:** the template renders `none` under kms; rollback note. |
| Alias refusal, response identity, IAM | **HOLDS** (response `KeyId` is the key ARN; a missing condition key evaluates false). | none |
| `keys/<key_id>.pub` | **WEAK**: availability, not authenticity. | **APPLY:** stated; `kairn verify` never picks a key from storage. |
| Honesty section | **WEAK**: the recorded digest overclaimed; pod-create rights and PVC writers were missing. | **APPLY:** the reverse-match rule, and the missing actors named. |
| Squatting by `IncidentCapture` creators | Residual: the webhook's CR name `ic-<hex>` can itself be created first (the webhook then treats the 409 as a duplicate). | **VALID-OUT-OF-SCOPE:** `IncidentCapture` create rights in the controller's namespace are admin-equivalent; documented. |

## Verdict

**Time-boxed, not dry.**

- The capture-refusal fixes are implemented and run in the E2E on 1.30 and 1.37.
- The KMS design is v1.1. Its own new surface (the sealing phase, the preflight) is
  attacked next by the implementation's tests, not by an R3.
- The most important result of this round is a **v0.1 bug found through the KMS lens**:
  vouching for another cluster. It ships fixed in the first release.
