# Build the Lapilli controller + CLI and run them on a minimal glibc distroless image.
# The CLI ships in the same image so the E2E can `kubectl exec ... lapilli verify` the sealed
# bundle in-cluster (distroless has no shell, but execing a binary directly works).
#
# Both bases are pinned by digest, and to the *same* Debian release. The release has to match
# because the build glibc must not be newer than the runtime's, or the runtime cannot load the
# binary — that is why this file pins the builder at all (`rust:1-slim` moved to trixie while the
# runtime was still cc-debian12, and produced binaries the runtime refused). The pairing is the
# invariant; the release is now trixie (Debian 13, glibc 2.41) on both sides.
#
# Why trixie and not bookworm: bookworm's libssl3 3.0.20-1~deb12u2 carries CVE-2026-75803
# (Critical) whose fix, 3.0.22-1~deb12u1, does not exist in Debian 12, and bookworm's libc6
# 2.36 carries CVE-2026-5450 (Critical, "won't fix"). Neither is reachable from these binaries
# (they link no OpenSSL at all — see docs/security-scanning.md), but every adopter's Trivy,
# grype, Harbor or ECR scan reports them and CRITICAL-blocking admission policies refuse the
# image. Trixie has no Critical in either package.
#
# Digests refreshed by Dependabot (.github/dependabot.yml, `docker` ecosystem). When you bump
# one, bump the other to the same Debian release and re-run test/e2e/run.sh.
FROM rust:1-trixie@sha256:a8a5f0a1e5fe7dfe1d352591e4a1c7dd2c08fd70475cae872cf3458ba0df0546 AS build
WORKDIR /src
# cargo-auditable, installed before the sources are copied so the layer survives a code change.
# It embeds the resolved dependency list (name, version, and which are direct) into a
# `.dep-v0` section of each binary, which is the only way these artifacts carry an SBOM that
# describes them: two static Rust binaries on distroless leave no package database for a
# filesystem scanner to read, so BuildKit's `sbom: true` syft pass found nothing to enumerate
# before this. Now syft's cargo-auditable cataloger reads it out of the binary — in the image
# attestation, and equally from a CLI tarball, where there is no image to attach anything to.
# Version-pinned for the same reason the bases are digest-pinned; `--locked` so the tool itself
# is built from its own audited lock.
RUN cargo install cargo-auditable --locked --version 0.7.6
COPY Cargo.toml Cargo.lock ./
COPY crates crates
COPY spec spec
# The in-pod CLI is built without the `remote` feature: `kubectl exec` must not turn it into
# a bucket reader running with the controller's cloud identity.
# `--locked`: what ships is what the committed, audited Cargo.lock resolves to. Without it the
# image could be built from freshly resolved versions nobody reviewed.
RUN cargo auditable build --release --locked -p lapilli-controller \
 && cargo auditable build --release --locked -p lapilli --no-default-features --features mcp
# --no-default-features: the in-cluster `lapilli` links no outbound network code (`remote` is
# off; there is nothing for it to fetch). `mcp` is the exception the chart's second container
# needs: an inbound HTTP server over the bundle volume, behind a bearer token, no client stack.
# Note the two binaries embed *different* dependency lists, because they are different builds:
# the CLI's omits everything `remote` pulls in. That is the point — each describes itself.

FROM gcr.io/distroless/cc-debian13:nonroot@sha256:54df941ed0d06a1bd95ef5e0ce391fd8d9f94b64782dc9a60062727849ee3f97
# Static facts about the image, so a scanner, Scorecard or a registry UI can tie it back to this
# repository and its licence without asking a human. The build-time ones — revision, version —
# are added by the workflow that builds it (.github/workflows/release.yml), because they are not
# properties of this file. `created` is deliberately not set: it would change the config digest
# on every rebuild of identical inputs, and the provenance attestation already carries the
# timestamp.
LABEL org.opencontainers.image.title="Lapilli" \
      org.opencontainers.image.description="Kubernetes incident flight recorder: seals signed, offline-verifiable incident evidence bundles." \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.source="https://github.com/lapilli-project/lapilli" \
      org.opencontainers.image.url="https://github.com/lapilli-project/lapilli" \
      org.opencontainers.image.documentation="https://github.com/lapilli-project/lapilli/blob/main/README.md" \
      org.opencontainers.image.vendor="The Lapilli Authors"
COPY --from=build /src/target/release/lapilli-controller /usr/local/bin/lapilli-controller
COPY --from=build /src/target/release/lapilli /usr/local/bin/lapilli
# Apache-2.0 §4(d) and the ~280 crates linked into those two binaries: the licence, the NOTICE
# the Arrow `object_store` dependency requires to travel, and the full text of every third-party
# licence. Distroless has no shell, so `docker cp` or an init container is how an adopter reads
# them out; `scripts/release-check.sh` asserts they are here and match the repository's copies.
COPY LICENSE NOTICE THIRD-PARTY-LICENSES.md /usr/local/share/doc/lapilli/
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/lapilli-controller"]
