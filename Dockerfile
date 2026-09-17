# Build the Kairn controller + CLI and run them on a minimal glibc distroless image.
# The CLI ships in the same image so the E2E can `kubectl exec ... kairn verify` the sealed
# bundle in-cluster (distroless has no shell, but execing a binary directly works).
# Pin to bookworm (Debian 12) so the build glibc matches the distroless cc-debian12 runtime;
# rust:1-slim now tracks trixie (newer glibc) and produces binaries the runtime can't load.
FROM rust:1-bookworm AS build
WORKDIR /src
COPY Cargo.toml Cargo.lock ./
COPY crates crates
COPY spec spec
RUN cargo build --release -p kairn-controller -p kairn-cli

FROM gcr.io/distroless/cc-debian12:nonroot
COPY --from=build /src/target/release/kairn-controller /usr/local/bin/kairn-controller
COPY --from=build /src/target/release/kairn /usr/local/bin/kairn
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/kairn-controller"]
