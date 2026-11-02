# The image holds the same binary as the release archive for its platform:
# the release workflow builds it once and copies it in here. To build an
# image yourself:
#
#   for a in amd64 arm64; do
#     CGO_ENABLED=0 GOOS=linux GOARCH=$a go build -trimpath -o dist/image/lapilli-linux-$a ./cmd/lapilli
#   done
#   docker buildx build --platform linux/amd64,linux/arm64 -t lapilli .
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG TARGETARCH
COPY --chmod=0555 dist/image/lapilli-linux-${TARGETARCH} /usr/local/bin/lapilli
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/lapilli"]
CMD ["--help"]
