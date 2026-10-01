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

## Released

| version | date | tag points at | notes |
|---|---|---|---|
| `v0.1.0-rc.1` | 2026-09-26 | `2eb8950` | first real run of `release.yml`; nothing frozen |
| `v0.1.0` | 2026-09-28 | `12f6eb4` | first release; `lapilli.dev/ieb/v1` and the fixture set frozen |

`v0.1.0`'s first attempt, tagged at `019d084`, published the image and the chart and then stopped:
the macOS CLI leg produced a binary with no `cargo-auditable` section on the `macos-14` runner and
the packaging assertion refused it, so `publish` never ran. Nothing had consumed the half-release —
no GitHub Release existed — so the tag was deleted and re-pushed after the matrix moved to
`macos-15`. The note under step 4 is what that taught.

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
   cargo test -p lapilli --test fixtures
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

**What only a tag exercises.** `release.yml`'s CLI matrix builds on a Linux arm runner and a macOS
runner, and neither is reachable from `ci` or from `release-check.sh`. `v0.1.0`'s first attempt is
what that costs: the macOS leg produced a binary with no `cargo-auditable` `.dep-v0` section, the
packaging assertion refused to publish it, and `publish` was skipped **after** the image and the
chart had already gone out — a half-published release, and nothing before the tag could have said
so. Two things narrow it now: step 1 builds the CLI for **this host's** target with `cargo auditable`
and asserts the section, which is the only pre-tag signal the Apple path has, and the workflow prints
the binary's section table and its `_AUDITABLE_VERSION_INFO` symbol before it judges. What stays
untestable before a tag is the Linux-arm and macOS *runner images* themselves, so treat the first run
on a new one as part of the release rather than a formality — and if a leg fails after `merge` and
`chart` have run, the tag has published part of itself and the decision is a re-tag or a patch
release, not a re-run: a tag-triggered run reads the workflow from the tag's own tree.

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
8. **crates.io**, when a release changes a published crate. `0.1.0` of all five went up on
   2026-09-29. Publish **bottom-up**, because each
   crate's packaged manifest depends on the registry versions of the ones below it and
   `cargo publish` verifies by building what it uploaded:

   ```sh
   for c in lapilli-bundle lapilli-net lapilli-kms lapilli lapilli-controller; do
     cargo publish -p "$c" --locked          # wait for each to appear before the next
   done
   ```

   Two things about this that are easy to get wrong. A version on crates.io **can never be
   deleted**, only yanked, so a `--dry-run` of the two leaf crates (`lapilli-bundle`,
   `lapilli-net`) is the last honest check — the three above them cannot be dry-run until their
   dependencies are actually on the registry, which is not a flaw in the check but the shape of
   bottom-up publishing. And every crate carries its own `LICENSE` copy because `cargo package`
   cannot reach outside the crate directory; `scripts/attribution-check.sh` asserts the five copies
   are byte-identical to the root one, so a drifted or missing copy fails before a tag rather than
   after a publish nobody can take back.

   The CLI crate is `lapilli` and so is its binary: `cargo install lapilli`. Its directory is
   `crates/lapilli-cli/`, which cargo does not require to match.

9. **Scan the published image** (`grype`/`trivy`) and reconcile against
   `docs/security-scanning.md`. A new Critical, or a finding in a package that page does not
   account for, is a release note at minimum and usually a base-image digest bump first: the
   `Dockerfile` pins `rust:1-trixie` and `gcr.io/distroless/cc-debian13:nonroot` by digest, the
   two must stay on the same Debian release, and Dependabot's `docker` PRs are what move them.

10. **Artifact Hub shows what the registry carries, not what `main` carries.** It reads the chart
    package pushed to the registry, so anything added to `charts/lapilli/Chart.yaml` between
    releases is invisible until the next tag republishes the chart. `release.yml` packages with
    `--version "$VERSION" --app-version "$VERSION"`, so there is no way to ship a chart-only fix:
    a chart change rides the next release or waits.

    Added 2026-10-01, when `icon` was set after `0.2.0` had shipped and the listing kept its grey
    placeholder. Republishing all four channels — crates.io included, where a version can be
    yanked but never deleted — to carry a logo was the worse trade. **After the next release, check
    the listing actually changed:**

    ```sh
    helm pull oci://ghcr.io/lapilli-project/charts/lapilli --version X.Y.Z --untar -d /tmp/c
    grep '^icon:' /tmp/c/lapilli/Chart.yaml        # must be there before the listing can show it
    curl -s https://artifacthub.io/api/v1/packages/helm/lapilli/lapilli | \
      python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("logo_image_id"))'
    ```

    A `logo_image_id` of `None` means Artifact Hub has not taken it, whatever the chart says.
