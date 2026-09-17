#!/usr/bin/env bash
# Kairn kind E2E — proves the operational spine end to end:
#   webhook POST -> IncidentCapture CR -> previous-log collector -> seal -> PVC export
#   -> in-cluster `kairn verify` (happy path OK, wrong-context fail-closed).
#
# Usage: test/e2e/run.sh            (creates & tears down a kind cluster)
#        KEEP=1 test/e2e/run.sh     (leave the cluster up for debugging)
set -euo pipefail

CLUSTER=kairn
IMAGE=kairn-controller:dev
NS=kairn-system
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

cleanup() { [ "${KEEP:-0}" = "1" ] || kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; }
trap cleanup EXIT

step() { echo; echo "==> $*"; }

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

step "deploy crash-looping workload; wait for a previous instance to exist"
kubectl apply -f test/e2e/crashloop.yaml
for i in $(seq 1 60); do
  rc=$(kubectl -n e2e get pod crasher -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null || echo 0)
  [ "${rc:-0}" -ge 1 ] && { echo "restartCount=$rc (previous instance present)"; break; }
  sleep 2
done
[ "${rc:-0}" -ge 1 ] || { echo "FAIL: crasher never restarted"; exit 1; }

step "fire Alertmanager-style webhook"
kubectl -n "$NS" port-forward svc/kairn-webhook 18080:8080 >/dev/null 2>&1 &
PF=$!
# Wait for the forwarded webhook to answer /healthz before POSTing (avoid the race).
for i in $(seq 1 30); do
  curl -sf localhost:18080/healthz >/dev/null 2>&1 && { echo "webhook ready"; break; }
  sleep 1
done
POSTED=0
for i in $(seq 1 5); do
  if curl -sf -X POST localhost:18080/webhook -H 'content-type: application/json' -d '{
      "alerts":[{"status":"firing","startsAt":"2026-09-17T02:14:33Z",
        "labels":{"alertname":"KubePodCrashLooping","namespace":"e2e","pod":"crasher"}}]}'; then
    echo; POSTED=1; break
  fi
  echo "  webhook POST retry $i"; sleep 2
done
kill $PF 2>/dev/null || true
if [ "$POSTED" != "1" ]; then
  echo "FAIL: webhook POST never succeeded; controller logs:"
  kubectl -n "$NS" logs deploy/kairn-controller --tail=50 || true
  exit 1
fi

step "wait for IncidentCapture to reach Exported"
IC=""
for i in $(seq 1 60); do
  IC=$(kubectl -n "$NS" get incidentcapture -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  [ -n "$IC" ] || { sleep 2; continue; }
  phase=$(kubectl -n "$NS" get incidentcapture "$IC" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  echo "  $IC phase=$phase"
  [ "$phase" = "Exported" ] && break
  [ "$phase" = "Failed" ] && { kubectl -n "$NS" get incidentcapture "$IC" -o jsonpath='{.status.message}'; echo; exit 1; }
  sleep 2
done
if [ "$phase" != "Exported" ]; then
  echo "FAIL: capture did not export; controller logs:"
  kubectl -n "$NS" logs deploy/kairn-controller --tail=50 || true
  exit 1
fi

BUNDLE=$(kubectl -n "$NS" get incidentcapture "$IC" -o jsonpath='{.status.bundlePath}')
CID=$(kubectl -n "$NS" get incidentcapture "$IC" -o jsonpath='{.spec.clusterId}')
IID=$(kubectl -n "$NS" get incidentcapture "$IC" -o jsonpath='{.spec.incidentId}')
POD=$(kubectl -n "$NS" get pod -l app.kubernetes.io/name=kairn -o jsonpath='{.items[0].metadata.name}')
echo "  bundle=$BUNDLE  cluster=$CID  incident=$IID"

step "in-cluster kairn verify — happy path (expect OK, exit 0)"
kubectl -n "$NS" exec "$POD" -c controller -- /usr/local/bin/kairn verify "$BUNDLE" --cluster "$CID" --incident "$IID"

step "in-cluster kairn verify — wrong context (expect FAILED, exit 1)"
if kubectl -n "$NS" exec "$POD" -c controller -- /usr/local/bin/kairn verify "$BUNDLE" --incident WRONG; then
  echo "FAIL: verify accepted wrong incident (not fail-closed)"; exit 1
else
  echo "  correctly fail-closed on wrong incident"
fi

echo; echo "E2E OK"
