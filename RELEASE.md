# Releasing Kairn

Who: a maintainer. Cadence: when `main` has user-visible changes worth shipping; no fixed
schedule before 1.0.

## Before the first release (once)

- [ ] Repository and GHCR packages public (or at least the packages).
- [ ] GitHub private vulnerability reporting enabled (SECURITY.md relies on it).
- [ ] An independent review (a human, or a non-Claude model) of the verifier of untrusted
      input: `read_ieb`, `Contents`, the manifest parse (`docs/design-review-round5.md`).

## Every release

1. **Gate.** Run the `release-gate` workflow on `main` (kind E2E on the oldest and newest
   tested Kubernetes minors) and make sure `ci` is green, including the MSRV job, the
   fixture tests and the spec-only producer.
2. **Fixtures.** Generate this release's bundle fixtures and commit them; never touch older
   ones:
   ```sh
   cargo run -p kairn-bundle --example gen_fixtures -- test/fixtures/ieb/vX.Y.Z test/fixtures/ieb/keys
   cargo test -p kairn-cli --test fixtures
   ```
3. **CHANGELOG.** Move `[Unreleased]` entries under `## [X.Y.Z] - YYYY-MM-DD`; list anything
   that needs action under **Migration** (CRD changes: `kubectl apply --server-side`).
4. **Versions.** Bump `version` in `Cargo.toml` and `version`/`appVersion` in
   `charts/kairn/Chart.yaml`; update SECURITY.md's supported-versions table if it changed.
5. **Tag.** `git tag -s vX.Y.Z -m vX.Y.Z && git push origin vX.Y.Z`. The `release` workflow
   re-runs the gate, then publishes the image (with SBOM and provenance), the OCI chart,
   the CLI binaries with `SHA256SUMS`, and release notes built from the CHANGELOG.
6. **Smoke test** the published artifacts: `helm install kairn oci://ghcr.io/jmcunst/charts/kairn
   --version X.Y.Z` on a fresh kind cluster, then `kairn demo` with the downloaded CLI.
