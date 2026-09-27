#!/usr/bin/env bash
# Everything the `ci` and `release-gate` workflows check, run locally before tagging
# (RELEASE.md). Usage: scripts/release-check.sh [--e2e]   (--e2e: kind E2E on 1.30 and 1.37;
# needs Docker, kind, helm).
set -euo pipefail
cd "$(dirname "$0")/.."
step() { echo; echo "==> $*"; }

step "fmt · clippy (default and --no-default-features) · tests"
cargo fmt --all --check
cargo clippy --workspace --all-targets -- -D warnings
cargo clippy -p lapilli-cli --no-default-features --all-targets -- -D warnings
cargo test --workspace -q
cargo test -p lapilli-cli --no-default-features -q

step "signing conformance (openssl)"
./scripts/verify-conformance.sh

step "MSRV (Rust 1.89)"
cargo +1.89 check --workspace --all-targets -q

step "a bundle built from the spec alone verifies"
cargo build -q -p lapilli-cli
tmp=$(mktemp -d)
python3 test/spec/build_from_spec.py "$tmp/spec-bundle"
target/debug/lapilli verify "$tmp/spec-bundle" --cluster spec-cluster --incident spec-incident
rm -rf "$tmp"

step "the fixture generator reproduces the committed expected.json"
tmp=$(mktemp -d)
for dir in test/fixtures/ieb/v*/; do
  release=$(basename "$dir")
  cargo run -q -p lapilli-bundle --example gen_fixtures -- "$tmp/$release" test/fixtures/ieb/keys >/dev/null
  python3 - "$tmp/$release/expected.json" "$dir/expected.json" <<'PY'
import json, sys
a, b = (json.load(open(p)) for p in sys.argv[1:])
sys.exit(0 if a == b else f"{sys.argv[2]} differs from what the generator produces")
PY
done
rm -rf "$tmp"
echo "expected.json matches the generator"

step "CRD manifests match the Rust types"
tmp=$(mktemp)
cargo run -q -p lapilli-controller -- crdgen > "$tmp"
diff -u config/crd/crds.json "$tmp"
diff -u charts/lapilli/crds/crds.json "$tmp"
rm -f "$tmp"

step "KMS signing against the AWS and GCP emulators"
test/kms/emulators.sh

step "helm lint and renders"
./scripts/helm-renders.sh

step "the alert rules in docs/metrics.md actually fire (promtool)"
./scripts/alert-rules-check.sh

step "lapilli mcp over stdio, driven by the reference MCP client (needs npx)"
# The in-cluster HTTP shape is proven by the E2E; this is the laptop shape, through the client
# an agent would actually use. It needs Node: without npx it is SKIPPED, and says so, rather
# than silently passing.
if command -v npx >/dev/null 2>&1; then
  test/mcp/check.sh target/debug/lapilli
else
  echo "  SKIPPED: npx not installed — test/mcp/check.sh not run (install Node to run it)"
fi

if [ "${1:-}" = "--e2e" ]; then
  for image in $(grep -oE 'kindest/node:v[0-9.]+@sha256:[0-9a-f]{64}' .github/workflows/release-gate.yml); do
    step "kind E2E on $image"
    kind delete cluster --name lapilli >/dev/null 2>&1 || true
    NODE_IMAGE="$image" test/e2e/run.sh
  done
fi
echo; echo "release-check OK"
