# Releasing Lapilli

Who: a maintainer. Cadence: when `main` has user-visible changes worth shipping; no fixed
schedule before 1.0.

## Before the first release (once)

- [ ] Repository and GHCR packages public (or at least the packages).
- [ ] GitHub private vulnerability reporting enabled (SECURITY.md relies on it).
- [ ] An independent review (a human, or a non-Claude model) of the verifier of untrusted
      input. The brief is `docs/independent-review.md` (scope, threat model, invariants); the
      record — who, when, the commit reviewed, findings and dispositions — goes in
      `docs/independent-review-log.md`.
- [ ] One real signature each on **AWS KMS** and **GCP Cloud KMS**, following the quickstarts in
      `docs/kms.md`, ending in a bundle that passes `lapilli verify --key` with the key from
      `lapilli key fetch`, kept under `test/fixtures/kms/`. GCP: done 2026-09-25. AWS: not yet
      (needs a personal account; see `docs/design-kms.md`).

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
   the CLI binaries with `SHA256SUMS`, and release notes built from the CHANGELOG.
6. **Smoke test** the published artifacts: `helm install lapilli oci://ghcr.io/lapilli-project/charts/lapilli
   --version X.Y.Z` on a fresh kind cluster, then `lapilli demo` with the downloaded CLI.
