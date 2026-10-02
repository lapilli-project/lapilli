#!/usr/bin/env bash
# Everything the `ci` and `release-gate` workflows check, run locally before tagging
# (RELEASE.md). Usage: scripts/release-check.sh [--e2e]   (--e2e: kind E2E on 1.30 and 1.37;
# needs Docker, kind, helm).
set -euo pipefail
cd "$(dirname "$0")/.."
step() { echo; echo "==> $*"; }

step "fmt · clippy (default and --no-default-features) · tests"
cargo fmt --all --check
cargo clippy --workspace --all-targets --locked -- -D warnings
cargo clippy -p lapilli --no-default-features --all-targets --locked -- -D warnings
# On macOS these run SERIALLY on purpose. Eight tests build a `kube::Client`, which asks rustls for
# the native root CAs; macOS answers that through `trustd`, and under parallel test threads it
# returns `Os(-36)` ("I/O error") for all three trust-settings domains at once, so the client fails
# with `NoValidNativeRootCA`. The eight failures look exactly like a regression in the collector,
# notify and perms modules and are not one: the same tests pass with `--test-threads=1`, and they
# pass in parallel on Linux, which is what CI runs. Diagnosed twice before this comment existed.
TEST_FLAGS=()
[ "$(uname -s)" = "Darwin" ] && TEST_FLAGS=(-- --test-threads=1)
cargo test --workspace --locked -q "${TEST_FLAGS[@]}"
cargo test -p lapilli --no-default-features --locked -q "${TEST_FLAGS[@]}"

step "signing conformance (openssl)"
./scripts/verify-conformance.sh

step "MSRV (Rust 1.89)"
cargo +1.89 check --workspace --all-targets --locked -q

step "a bundle built from the spec alone verifies"
cargo build -q --locked -p lapilli
tmp=$(mktemp -d)
python3 test/spec/build_from_spec.py "$tmp/spec-bundle"
target/debug/lapilli verify "$tmp/spec-bundle" --cluster spec-cluster --incident spec-incident
rm -rf "$tmp"

step "the fixture generator reproduces the committed expected.json"
tmp=$(mktemp -d)
for dir in test/fixtures/ieb/v*/; do
  release=$(basename "$dir")
  cargo run -q --locked -p lapilli-bundle --example gen_fixtures -- "$tmp/$release" test/fixtures/ieb/keys >/dev/null
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
cargo run -q --locked -p lapilli-controller -- crdgen > "$tmp"
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

step "no banned dependency and no known advisory (cargo-deny, needs cargo-deny)"
# Both of these were CI-only until 2026-10-01, which is the asymmetry this project has now been
# bitten by four times: a gate that runs only on the runner makes "the local gate is green" mean
# less than it reads. `check bans` is what keeps a C crypto stack out of the shipped controller
# (docs/security-scanning.md's reachability claim rests on it) and `check advisories` is the
# advisory gate; `deny.toml` configures both.
#
# One caveat worth knowing here rather than in a surprise: cargo-deny decides YANKED from cached
# index metadata, so a stale local index reports nothing where a fresh runner reports the yank.
# This step passing is not evidence that nothing in the lock is yanked.
if command -v cargo-deny >/dev/null 2>&1; then
  cargo deny --config deny.toml --all-features check bans advisories
else
  echo "  SKIPPED: cargo-deny not installed — bans and advisories not checked locally (cargo install cargo-deny)"
fi

step "third-party attribution ships in the artifacts"
./scripts/attribution-check.sh

# 3. The workflow puts the same three files in every CLI tarball and labels the image with the
#    build-time facts. The tarball is only ever assembled in CI, so this is a check on the recipe;
#    step 4 checks the image itself, which can be built here.
cpline=$(grep -E '^ +cp .*release/lapilli' .github/workflows/release.yml || true)
for f in LICENSE NOTICE THIRD-PARTY-LICENSES.md; do
  case " $cpline " in
    *" $f "*) ;;
    *) echo "  release.yml's tarball step does not copy $f"; exit 1 ;;
  esac
done
grep -q 'org.opencontainers.image.revision=' .github/workflows/release.yml \
  || { echo "  release.yml does not label the image with its revision"; exit 1; }
grep -q 'cargo auditable build' .github/workflows/release.yml \
  || { echo "  release.yml builds the CLI without cargo auditable — the tarball would carry no SBOM"; exit 1; }
echo "  release.yml packages all three files, labels the revision, and builds auditable"

# 3b. The same claim, on THIS host's own target. Step 4 below proves it for the two Linux binaries
#     inside the image; nothing proved it for the macOS tarball, and that gap is not academic:
#     `v0.1.0`'s release refused to publish because the binary built on the `macos-14` runner had
#     no `.dep-v0` section at all. On Apple targets cargo-auditable keeps the section alive with
#     `-Wl,-u,_AUDITABLE_VERSION_INFO`, which an older `ld` treats differently, so the host a
#     maintainer runs this on is the only pre-tag signal there is for that leg. Skipped loudly
#     rather than silently when cargo-auditable is absent.
if command -v cargo-auditable >/dev/null 2>&1; then
  host=$(rustc -vV | sed -n 's/^host: //p')
  cargo auditable build -q --release --locked -p lapilli --target "$host"
  grep -aq '\.dep-v0' "target/$host/release/lapilli" \
    || { echo "  a CLI built on this host ($host) carries no .dep-v0 dependency list"; exit 1; }
  echo "  a CLI built on this host ($host) carries its .dep-v0 dependency list"
else
  echo "  SKIPPED: cargo-auditable not installed — the host CLI's dependency list was not checked"
  echo "           (cargo install cargo-auditable --locked --version 0.7.6)"
fi

# 4. The image itself: the files are in it, they are byte-identical to the repository's, the OCI
#    labels a scanner reads are set, and the binaries carry their dependency list. Distroless has
#    no shell, so this goes through `docker create` + `docker cp` rather than `docker run`.
if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
  # A cold build here is a full release build of the workspace; BuildKit's layer cache makes the
  # repeat runs cheap. Progress still goes to stderr.
  docker build -t lapilli-release-check:local . >/dev/null
  # Everything comes out of the container first, and the container is removed, before anything is
  # asserted — so a failing check below cannot leave a stopped container behind.
  cid=$(docker create lapilli-release-check:local)
  tmp=$(mktemp -d)
  mkdir "$tmp/doc"
  docker cp "$cid:/usr/local/share/doc/lapilli/." "$tmp/doc/" >/dev/null \
    || { docker rm "$cid" >/dev/null
         echo "  the image has no /usr/local/share/doc/lapilli/ — nothing attributes what it links"
         exit 1; }
  for b in lapilli-controller lapilli; do docker cp "$cid:/usr/local/bin/$b" "$tmp/$b" >/dev/null; done
  docker rm "$cid" >/dev/null

  for f in LICENSE NOTICE THIRD-PARTY-LICENSES.md; do
    diff -q "$f" "$tmp/doc/$f" >/dev/null \
      || { echo "  /usr/local/share/doc/lapilli/$f in the image is missing or differs from ./$f"; exit 1; }
  done
  echo "  the image carries LICENSE, NOTICE and THIRD-PARTY-LICENSES.md, byte-identical"
  for b in lapilli-controller lapilli; do
    # cargo-auditable stores the dependency list in an ELF section literally named `.dep-v0`;
    # the name is in the section-header string table, so grepping the file for it needs no tools.
    grep -aq '\.dep-v0' "$tmp/$b" \
      || { echo "  $b was not built with cargo auditable — it carries no dependency list, and the"
           echo "  image's SBOM attestation would enumerate the base image's packages and no crates"
           exit 1; }
  done
  echo "  both binaries carry a .dep-v0 dependency list (cargo audit bin / syft scan file: reads it)"
  rm -rf "$tmp"
  docker inspect --format '{{json .Config.Labels}}' lapilli-release-check:local | python3 -c '
import json, sys
labels = json.load(sys.stdin) or {}
# The build-time ones (revision, version) are added by release.yml, not the Dockerfile, so a
# local build does not have them and checking for them here would only ever fail. Step 3 covers
# them instead.
required = ["title", "description", "licenses", "source", "url"]
missing = [k for k in required if not labels.get("org.opencontainers.image." + k)]
if missing:
    sys.exit("  the image is missing OCI labels: " + ", ".join(missing))
if labels["org.opencontainers.image.licenses"] != "Apache-2.0":
    sys.exit("  org.opencontainers.image.licenses is not Apache-2.0")
print("  OCI labels set: " + ", ".join(sorted(k.split(".")[-1] for k in labels)))
'
else
  echo "  SKIPPED: no usable Docker — the image's copies of the attribution files, its OCI labels"
  echo "           and the binaries' embedded dependency lists were not checked"
fi

if [ "${1:-}" = "--e2e" ]; then
  for image in $(grep -oE 'kindest/node:v[0-9.]+@sha256:[0-9a-f]{64}' .github/workflows/release-gate.yml); do
    step "kind E2E on $image"
    kind delete cluster --name lapilli >/dev/null 2>&1 || true
    NODE_IMAGE="$image" test/e2e/run.sh
  done
fi
echo; echo "release-check OK"
