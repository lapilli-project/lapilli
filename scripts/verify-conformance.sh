#!/usr/bin/env bash
# Signing conformance gate: prove Kairn's manifest signature is a standard, interoperable
# ECDSA-P256-SHA256 DER signature by verifying it with a neutral tool (openssl) — not with a
# version-moving cosign CLI (cosign v3 dropped detached verify-blob; see spec/IEB-SPEC.md).
#
# Usage: scripts/verify-conformance.sh   (builds a fresh signed bundle in a temp dir)
set -euo pipefail

cd "$(dirname "$0")/.."

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "==> generating a signed bundle"
cargo run -q -p kairn-bundle --example gen_signed -- "$TMP/bundle"

cd "$TMP/bundle"

echo "==> neutral crypto verification (openssl)"
# macOS: base64 -D -i FILE ; Linux coreutils: base64 -d FILE
if grep -q -- '-D' <<<"$(base64 --help 2>&1)"; then
  base64 -D -i signature/manifest.sig -o sig.der
else
  base64 -d signature/manifest.sig > sig.der
fi
openssl dgst -sha256 -verify signature/cosign.pub -signature sig.der manifest.json

echo "==> negative check: tampered payload must be rejected"
cp manifest.json manifest.bad && printf 'x' >> manifest.bad
if openssl dgst -sha256 -verify signature/cosign.pub -signature sig.der manifest.bad 2>/dev/null; then
  echo "FAIL: tampered payload verified — signature check is broken" >&2
  exit 1
fi
echo "    correctly rejected tampered payload"

echo "CONFORMANCE OK"
