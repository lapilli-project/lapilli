# Design Review — Round 9 (KMS signing, and who can get a bundle vouched for)

Constitution: **Loop Engineering Constitution v0.5.0**. Artifact:
[`design-kms.md`](design-kms.md) v0 → v1 → v1.1, plus the capture-refusal code fix in
`crates/lapilli-controller/src/reconcile.rs`.

**Outcome: time-boxed, not dry.** R1 had three lenses; R2 was a converge round with one
rotated critic; R3 attacked the implementation on a live cluster. Every finding is APPLIED
and exercised by the E2E or the emulator tests. No further round.

## Round 0

- **Category:** security-sensitive and public. Signing is the only source of producer
  authenticity (`lapilli verify --key`).
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
| 6 | MAJOR (operator) | Rotation and old-bundle keys: the old public key is unfetchable once the key is disabled. | **APPLY (partly):** `lapilli key fetch`, the key id logged at startup, and a rotation runbook that says to keep the old public key before disabling the key. Publishing `keys/<key_id>.pub` next to the bundles and exports: **not implemented yet** (additive, tracked for a later release). |
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
| `keys/<key_id>.pub` | **WEAK**: availability, not authenticity. | **APPLY:** stated; `lapilli verify` never picks a key from storage. |
| Honesty section | **WEAK**: the recorded digest overclaimed; pod-create rights and PVC writers were missing. | **APPLY:** the reverse-match rule, and the missing actors named. |
| Squatting by `IncidentCapture` creators | Residual: the webhook's CR name `ic-<hex>` can itself be created first (the webhook then treats the 409 as a duplicate). | **VALID-OUT-OF-SCOPE:** `IncidentCapture` create rights in the controller's namespace are admin-equivalent; documented. |

## R3 — the implementation (one critic, on a live kind cluster with LocalStack)

No forged or wrongly signed bundle: every bundle pulled from the run verified as
`signed:trusted-key` with the key from `lapilli key fetch`. But the phase machine had
defects, two of them reproduced live.

| # | Sev | Finding | Disposition |
|---|---|---|---|
| I1 | BLOCKER | "Failed, data kept" was false. Failed patches cleared `observedGeneration`, so the next reconcile collected again and wiped the staging data, including the evidence of tampering (reproduced). | **APPLY.** Every Sealing and Failed patch carries the generation. A capture that reached sealing stays Failed whatever its generation, and only `retry-seal` moves it. `run_capture` resumes from an existing seal file instead of collecting again. E2E: a planted file in a waiting capture's staging leads to `staging-modified` Failed, which stays Failed, the data is kept, and `retry-seal` fails again. |
| I2 | MAJOR | A successful seal was written in two patches; a reconcile between them saw `staging-lost` (11 of 20 captures emitted `SealFailed`). | **APPLY.** One patch writes the seal record and `Exported` together. A Sealing capture whose bundle already exists under its own claim is adopted, never signed again. The seal file is removed only after `Exported`. E2E: 5 concurrent captures, and no `SealFailed`. |
| I3 | MAJOR | `requeue(0)` after collection read the pre-patch cache and collected twice (2 of 15). | **APPLY.** The status patch's watch event drives the first attempt; resume-from-seal-file closes the race either way. |
| I4 | MAJOR | `aws-cn` with IRSA: object_store's default STS host doesn't exist in China. | **APPLY:** the China STS endpoint unless `AWS_ENDPOINT_URL_STS` is set. |
| I5 | MINOR | A packing failure after signing went to the 10 s error policy, signing again with KMS every 10 s forever. | **APPLY:** it counts against the same budget, with backoff (`seal-io-error`). |
| I6 | MINOR | `status.seal.key` kept the old key after a key change. | **APPLY:** set on success; the design text now describes rebuilding the manifest with the pinned key. |

## Verdict

**Time-boxed, not dry.**

- The capture-refusal fixes are implemented and run in the E2E on 1.30 and 1.37.
- The KMS design is v1.1. Its own new surface (the sealing phase, the preflight) is
  attacked next by the implementation's tests, not by an R3.
- The most important result of this round is a **v0.1 bug found through the KMS lens**:
  vouching for another cluster. It ships fixed in the first release.
