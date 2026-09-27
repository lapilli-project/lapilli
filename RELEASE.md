# Releasing Lapilli

Who: a maintainer. Cadence: when `main` has user-visible changes worth shipping; no fixed
schedule before 1.0.

## Before the first release (once)

- [x] Repository and GHCR packages public (or at least the packages). Done 2026-09-27 — the
      org's package-creation policy had to allow public packages first.
- [x] GitHub private vulnerability reporting enabled (SECURITY.md relies on it). Done 2026-09-27.
- [x] An independent review (a human, or a non-Claude model) of the verifier of untrusted
      input. The brief is `docs/independent-review.md` (scope, threat model, invariants); the
      record — who, when, the commit reviewed, findings and dispositions — is in
      `docs/independent-review-log.md`. **Done on the model half only (2026-09-25):** GPT-5
      reviewed the packet, seven of its ten findings held and were fixed, and three fuzz
      targets ran 15.6 million executions without a crash. **No human has reviewed the
      verifier.** v0.1.0 ships on that record, stated here rather than implied otherwise; a
      human review stays on the list before v0.2.0 (`ROADMAP.md` §3).
- [x] One real signature on a real KMS, following the quickstart in `docs/kms.md`, ending in a
      bundle that passes `lapilli verify --key` with the key from `lapilli key fetch`, kept under
      `test/fixtures/kms/`. **GCP: done 2026-09-25.** AWS is verified against LocalStack only and
      is labelled that way in the README and `docs/kms.md`; the project has no AWS account and
      does not need one — the first AWS adopter's install is the real test, and the label is
      removed then. Real S3 (Object Lock) is in the same position: LocalStack only, labelled.

## Pre-releases (`vX.Y.Z-rc.N`)

A pre-release runs the same workflow and publishes the same artifacts, marked *pre-release* on
GitHub, with the `[Unreleased]` CHANGELOG section as its notes. It exists to exercise
`release.yml` and the published artifacts before the promises start: **nothing is frozen by a
pre-release** — not the fixture set under `test/fixtures/ieb/v0.1.0`, not the format, not the
compatibility table — and no version bump is made for it. Steps 2–4 below are skipped; steps
1, 5 and 6 apply.

## Every release

1. **Gate.** Locally, `scripts/release-check.sh --e2e` runs everything below (CI jobs,
   the fixture generator check, kind E2E on 1.30 and 1.37). Then run the `release-gate` workflow on `main` (kind E2E on the oldest and newest
   tested Kubernetes minors) and make sure `ci` is green, including the MSRV job, the
   fixture tests and the spec-only producer.
2. **Fixtures.** Generate this release's bundle fixtures and commit them; never touch older
   ones:
   ```sh
   cargo run -p lapilli-bundle --example gen_fixtures -- test/fixtures/ieb/vX.Y.Z test/fixtures/ieb/keys
   cargo test -p lapilli-cli --test fixtures
   ```
3. **CHANGELOG.** Move `[Unreleased]` entries under `## [X.Y.Z] - YYYY-MM-DD`; list anything
   that needs action under **Migration** (CRD changes: `kubectl apply --server-side --force-conflicts`).
4. **Versions.** Bump `version` in `Cargo.toml` and `version`/`appVersion` in
   `charts/lapilli/Chart.yaml`; update SECURITY.md's supported-versions table if it changed.
5. **Tag.** `git tag -s vX.Y.Z -m vX.Y.Z && git push origin vX.Y.Z`. The `release` workflow
   re-runs the gate, then publishes the image (with SBOM and provenance), the OCI chart,
   the CLI binaries with `SHA256SUMS`, and release notes built from the CHANGELOG. The image
   is built per platform on native runners and merged into one manifest list, so the digest
   in the release notes is the manifest list's, not either platform's.
6. **Smoke test** the published artifacts: `helm install lapilli oci://ghcr.io/lapilli-project/charts/lapilli
   --version X.Y.Z` on a fresh kind cluster, then `lapilli demo` with the downloaded CLI.
