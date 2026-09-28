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
1 and 5–8 apply, and so does step 4's `THIRD-PARTY-LICENSES.md` regeneration if `Cargo.lock` has
moved — a pre-release is a distribution like any other, and the licences of what it links travel
with it. A pre-release is in fact the *best* place to find a broken attestation, an empty SBOM or a
new base-image Critical, so do not skip 7 and 8 on the grounds that nothing is promised yet.

## Every release

1. **Gate.** Locally, `scripts/release-check.sh --e2e` runs everything below (CI jobs,
   the fixture generator check, kind E2E on 1.30 and 1.37). Then run the `release-gate` workflow on `main` (kind E2E on the oldest and newest
   tested Kubernetes minors) and make sure `ci` is green, including the MSRV job, the
   fixture tests and the spec-only producer.

   One step of `release-check.sh` is about what the published files carry rather than whether the
   code works — **third-party attribution ships in the artifacts**. Its portable half is
   `scripts/attribution-check.sh`, which the `ci` workflow runs on every pull request, because what
   breaks attribution is usually not an edit: `THIRD-PARTY-LICENSES.md` is generated from
   `Cargo.lock`, and Dependabot's `cargo` group moves the lock. Together they assert that `LICENSE`,
   `NOTICE` and `THIRD-PARTY-LICENSES.md` exist, that `LICENSE`'s appendix names a copyright
   holder, that every dependency shipping a `NOTICE` of its own has it reproduced in ours (today
   `object_store`, via Apache-2.0 §4(d)), that `THIRD-PARTY-LICENSES.md` lists *exactly* the crates
   linked into a published binary, that `release.yml` copies all three into each CLI tarball, and —
   by building the image and reading them back out with `docker cp` — that the image's copies are
   byte-identical, that its OCI labels are set, and that both binaries carry a `.dep-v0` dependency
   list from `cargo auditable`. `v0.1.0-rc.1` shipped with none of that; this step is why the next
   one cannot. `cargo deny check licenses` (the `ci` job) is a different question — whether these
   licences are compatible with ours, not whether their text reaches a downloader.
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

   That bump rewrites `Cargo.lock`, so **regenerate `THIRD-PARTY-LICENSES.md`** in the same commit
   — as after any `cargo update` or dependency change:
   ```sh
   cargo install cargo-about --locked --features cli     # once
   cargo about generate --all-features --fail about.hbs -o THIRD-PARTY-LICENSES.md
   ```
   It is a generated file (`about.toml` says which crates, targets and licences; `about.hbs` is the
   template) and step 1's gate fails if it is stale, so this is a reminder, not the enforcement.
5. **Tag.** `git tag -s vX.Y.Z -m vX.Y.Z && git push origin vX.Y.Z`. The `release` workflow
   re-runs the gate, then publishes the image (with BuildKit's SBOM and provenance — unsigned
   in-toto attestations, not Sigstore-signed SLSA), the OCI chart, the CLI binaries with
   `SHA256SUMS` and a Sigstore-signed build-provenance attestation per tarball and for
   `SHA256SUMS`, and release notes built from the CHANGELOG. The image is built per platform on
   native runners and merged into one manifest list, so the digest in the release notes is the
   manifest list's, not either platform's.

   Every published binary is built with `cargo auditable`, which embeds the resolved dependency
   list in the binary itself, and `LICENSE`, `NOTICE` and `THIRD-PARTY-LICENSES.md` go into the
   image under `/usr/local/share/doc/lapilli/` and into the top level of each CLI tarball. So the
   image's SBOM attestation now enumerates the crates (BuildKit's syft pass reads them out of the
   binaries; over a distroless filesystem of static Rust binaries it previously found nothing), and
   a downloaded tarball carries its own dependency list with no attestation needed —
   `cargo audit bin lapilli` or `syft scan file:lapilli`. What the embedded list does *not* carry is
   licence data; `THIRD-PARTY-LICENSES.md` is the licence record and the two are not substitutes.

   The workflow runs on a read-only permission floor; each job elevates itself and nothing else
   (`packages: write` for the image and chart, `contents: write` for the release,
   `id-token`/`attestations: write` for the two attestation steps). The gate holds
   `contents: read` only. If you add a job that needs to write something, give it its own
   `permissions:` block rather than raising the workflow default.
6. **Smoke test** the published artifacts: `helm install lapilli oci://ghcr.io/lapilli-project/charts/lapilli
   --version X.Y.Z` on a fresh kind cluster, then `lapilli demo` with the downloaded CLI.
7. **Verify the release the way a user would**, from the published files and nothing local — this
   is the step that catches a broken attestation, and the commands are the ones README
   §*Verifying what you downloaded* tells users to run:
   ```sh
   gh release download vX.Y.Z -R lapilli-project/lapilli
   gh attestation verify SHA256SUMS -R lapilli-project/lapilli
   gh attestation verify lapilli-vX.Y.Z-*.tar.gz -R lapilli-project/lapilli
   sha256sum -c SHA256SUMS
   docker buildx imagetools inspect ghcr.io/lapilli-project/lapilli-controller:X.Y.Z \
     --format '{{ json .Provenance }}'
   ```
   Then check the two things a published artifact used to be missing — that the attribution files
   are in it, and that its SBOM is not empty:
   ```sh
   tar tzf lapilli-vX.Y.Z-x86_64-unknown-linux-musl.tar.gz   # LICENSE, NOTICE, THIRD-PARTY-LICENSES.md
   tar xzf lapilli-vX.Y.Z-x86_64-unknown-linux-musl.tar.gz && syft scan file:lapilli-vX.Y.Z-x86_64-unknown-linux-musl/lapilli
   docker buildx imagetools inspect ghcr.io/lapilli-project/lapilli-controller:X.Y.Z \
     --format '{{ json .SBOM }}' | jq '.SPDX.packages | length'   # crates, not 1
   docker create --name lapilli-notice ghcr.io/lapilli-project/lapilli-controller:X.Y.Z \
     && docker cp lapilli-notice:/usr/local/share/doc/lapilli/ . && docker rm lapilli-notice
   docker inspect --format '{{json .Config.Labels}}' ghcr.io/lapilli-project/lapilli-controller:X.Y.Z
   ```
8. **Scan the published image** (`grype`/`trivy`) and reconcile against
   `docs/security-scanning.md`. A new Critical, or a finding in a package that page does not
   account for, is a release note at minimum and usually a base-image digest bump first: the
   `Dockerfile` pins `rust:1-trixie` and `gcr.io/distroless/cc-debian13:nonroot` by digest, the
   two must stay on the same Debian release, and Dependabot's `docker` PRs are what move them.
