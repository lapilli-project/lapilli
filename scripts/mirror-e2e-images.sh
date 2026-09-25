#!/usr/bin/env bash
# Copy the digest-pinned E2E images (test/e2e/images.env) into a registry the E2E can pull
# from — a GitHub runner cannot pull quay.io/minio/* since 2026-09 (login required), a laptop
# that has logged in can. crane and skopeo copy the manifest byte for byte, so the digest is
# preserved and test/e2e/export.sh's pin check keeps holding against the mirror.
#
#   docker login quay.io            # source
#   docker login ghcr.io            # target (a token with write:packages)
#   scripts/mirror-e2e-images.sh ghcr.io/lapilli-project/e2e
#
# Then, once, in the GitHub UI: give the repository lapilli-project/lapilli read access to the
# two new packages (Package settings → Manage Actions access), or make them public. ci.yml and
# release-gate.yml log in with GITHUB_TOKEN and set E2E_IMAGE_MIRROR=ghcr.io/lapilli-project/e2e.
set -euo pipefail
target=${1:?usage: $0 <target-registry-prefix>}
. "$(dirname "$0")/../test/e2e/images.env"
for pin in "$MINIO_PIN" "$MC_PIN"; do
  digest=${pin#*@}; name=${pin%@*}; name=${name##*/}
  if command -v crane >/dev/null 2>&1; then
    crane copy "$pin" "$target/$name:$(echo "$digest" | cut -c8-19)"
  elif command -v skopeo >/dev/null 2>&1; then
    skopeo copy --all --preserve-digests "docker://$pin" "docker://$target/$name:$(echo "$digest" | cut -c8-19)"
  else
    echo "need crane (github.com/google/go-containerregistry) or skopeo" >&2; exit 2
  fi
  got=$( { crane digest "$target/$name@$digest" 2>/dev/null || skopeo inspect --raw "docker://$target/$name@$digest" >/dev/null 2>&1 && echo "$digest"; } )
  [ "$got" = "$digest" ] || { echo "FAIL: $target/$name does not serve $digest after the copy" >&2; exit 1; }
  echo "mirrored $pin -> $target/$name@$digest"
done
