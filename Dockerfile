# Build the Lapilli controller + CLI and run them on a minimal glibc distroless image.
# The CLI ships in the same image so the E2E can `kubectl exec ... lapilli verify` the sealed
# bundle in-cluster (distroless has no shell, but execing a binary directly works).
# Pin to bookworm (Debian 12) so the build glibc matches the distroless cc-debian12 runtime;
# rust:1-slim now tracks trixie (newer glibc) and produces binaries the runtime can't load.
FROM rust:1-bookworm AS build
WORKDIR /src
COPY Cargo.toml Cargo.lock ./
COPY crates crates
COPY spec spec
# The in-pod CLI is built without the `remote` feature: `kubectl exec` must not turn it into
# a bucket reader running with the controller's cloud identity.
RUN cargo build --release -p lapilli-controller \
 && cargo build --release -p lapilli-cli --no-default-features --features mcp
# --no-default-features: the in-cluster `lapilli` links no outbound network code (`remote` is
# off; there is nothing for it to fetch). `mcp` is the exception the chart's second container
# needs: an inbound HTTP server over the bundle volume, behind a bearer token, no client stack.

FROM gcr.io/distroless/cc-debian12:nonroot
COPY --from=build /src/target/release/lapilli-controller /usr/local/bin/lapilli-controller
COPY --from=build /src/target/release/lapilli /usr/local/bin/lapilli
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/lapilli-controller"]
