#!/usr/bin/env bash
# Kairn kind E2E. `kairn demo` *is* the harness (DESIGN §8): it stages a bad rollout, fires
# the Alertmanager webhook, waits for export, pulls the .ieb, and verifies it offline. On
# top of that this script asserts the negative paths the integrity claim rests on:
#   - one tampered byte        -> verify FAILED (non-zero)
#   - wrong incident context   -> verify FAILED (fail-closed), with the in-cluster binary
#
# Usage: test/e2e/run.sh            (creates & tears down a kind cluster)
#        KEEP=1 test/e2e/run.sh     (leave the cluster up for debugging)
set -euo pipefail

CLUSTER=kairn
IMAGE=kairn-controller:dev
NS=kairn-system
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

OUT="$(mktemp -d)"
cleanup() {
  rm -rf "$OUT"
  [ "${KEEP:-0}" = "1" ] || kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
}
trap cleanup EXIT

step() { echo; echo "==> $*"; }
fail() { echo "FAIL: $*"; kubectl -n "$NS" logs deploy/kairn-controller --tail=50 || true; exit 1; }

step "build kairn CLI (host)"
cargo build -q -p kairn-cli
KAIRN="$ROOT/target/debug/kairn"

step "create kind cluster"
kind create cluster --name "$CLUSTER" --wait 120s

step "build + load controller image (SHA-independent :dev tag, IfNotPresent)"
docker build -t "$IMAGE" .
kind load docker-image "$IMAGE" --name "$CLUSTER"

step "apply CRDs, RBAC, controller, default profile"
kubectl apply -f config/crd/crds.json
kubectl apply -f config/rbac/rbac.yaml
kubectl apply -f deploy/controller.yaml
kubectl -n "$NS" rollout status deploy/kairn-controller --timeout=120s
kubectl apply -f config/samples/captureprofile-default.yaml

step "kairn demo --scenario crashloop"
"$KAIRN" demo --scenario crashloop --out "$OUT/crashloop" | tee "$OUT/crashloop.txt" \
  || fail "demo crashloop exited non-zero"
grep -q "OK  hash_ok=true context_ok=true coverage=100%" "$OUT/crashloop.txt" || fail "crashloop bundle not OK/100%"
grep -q "FATAL: cache warmup failed" "$OUT/crashloop.txt" || fail "previous-instance logs not recovered"
grep -q "revision 2" "$OUT/crashloop.txt" || fail "change indicator (revision 2) missing"

step "kairn demo --scenario oomkill"
"$KAIRN" demo --scenario oomkill --out "$OUT/oomkill" | tee "$OUT/oomkill.txt" \
  || fail "demo oomkill exited non-zero"
grep -q "OK  hash_ok=true context_ok=true coverage=100%" "$OUT/oomkill.txt" || fail "oomkill bundle not OK/100%"
grep -q "OOMKilled (exit 137)" "$OUT/oomkill.txt" || fail "OOMKilled termination not in bundle"

step "negative: tamper one byte in an unpacked bundle (expect FAILED, exit 1)"
BUNDLE_DIR=$(find "$OUT/crashloop" -mindepth 1 -maxdepth 1 -type d | head -1)
printf 'x' >> "$BUNDLE_DIR/logs/app-previous.log"
if "$KAIRN" verify "$BUNDLE_DIR"; then fail "verify accepted a tampered bundle"; fi
echo "  correctly rejected the tampered bundle"

step "in-cluster kairn verify (distroless binary) — happy path + wrong context"
IC=$(kubectl -n "$NS" get incidentcapture -o jsonpath='{.items[0].metadata.name}')
BUNDLE=$(kubectl -n "$NS" get incidentcapture "$IC" -o jsonpath='{.status.bundlePath}')
CID=$(kubectl -n "$NS" get incidentcapture "$IC" -o jsonpath='{.spec.clusterId}')
IID=$(kubectl -n "$NS" get incidentcapture "$IC" -o jsonpath='{.spec.incidentId}')
POD=$(kubectl -n "$NS" get pod -l app.kubernetes.io/name=kairn -o jsonpath='{.items[0].metadata.name}')
kubectl -n "$NS" exec "$POD" -c controller -- /usr/local/bin/kairn verify "$BUNDLE" --cluster "$CID" --incident "$IID" \
  || fail "in-cluster verify rejected a good bundle"
if kubectl -n "$NS" exec "$POD" -c controller -- /usr/local/bin/kairn verify "$BUNDLE" --incident WRONG; then
  fail "verify accepted wrong incident (not fail-closed)"
fi
echo "  correctly fail-closed on wrong incident"

echo; echo "E2E OK"
