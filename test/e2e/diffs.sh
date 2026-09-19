#!/usr/bin/env bash
# diffs/ scenarios required by docs/design-review-round4.md (the timing heuristics are
# proven here, on a real cluster, not by more review rounds):
#   rollback         — reused RS: activation from the latest coalesced "from 0" event
#   scale canary     — HPA-style 1→2 and scale 0→1 of an old revision: nothing in range
#   paused           — template edited while paused: a deployment-spec-pending entry
#   recreate         — Recreate strategy: the actor is still attributed
#
# Usage: test/e2e/diffs.sh <path-to-kairn-binary> <scratch-dir>   (called by run.sh)
set -euo pipefail

KAIRN=$1
OUT=$2/diffs
NS=diffs-e2e
KNS=kairn-system
mkdir -p "$OUT"

step() { echo; echo "==> diffs: $*"; }
fail() { echo "FAIL (diffs): $*"; exit 1; }

deploy() { # name, value of A, [strategy]
  local strategy=${3:-RollingUpdate}
  kubectl apply --server-side --field-manager=e2e -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: { name: $1, namespace: $NS }
spec:
  replicas: 1
  strategy: { type: $strategy }
  selector: { matchLabels: { app: $1 } }
  template:
    metadata: { labels: { app: $1 } }
    spec:
      terminationGracePeriodSeconds: 1
      containers:
        - name: app
          image: busybox:1.36
          command: ["sh", "-c", "while true; do sleep 3600; done"]
          env: [{ name: A, value: "$2" }]
EOF
  kubectl -n $NS rollout status deploy/$1 --timeout=120s >/dev/null
}

live_pod() { # newest non-terminating pod of app $1
  kubectl -n $NS get pods -l app=$1 -o json | python3 -c '
import json, sys
pods = [p for p in json.load(sys.stdin)["items"] if not p["metadata"].get("deletionTimestamp")]
pods.sort(key=lambda p: p["metadata"]["creationTimestamp"])
print(pods[-1]["metadata"]["name"])'
}

capture() { # label, pod → unpacked bundle dir on stdout
  local label=$1 pod=$2 resp ic phase bundle ctrl
  resp=$(printf '{"alerts":[{"status":"firing","labels":{"alertname":"KairnDiffE2E","namespace":"%s","pod":"%s"}}]}' "$NS" "$pod" |
    kubectl create --raw "/api/v1/namespaces/$KNS/services/kairn-webhook:webhook/proxy/webhook" -f -)
  ic=$(echo "$resp" | python3 -c 'import json,sys; print(json.load(sys.stdin)["captures"][0])')
  for _ in $(seq 1 60); do
    phase=$(kubectl -n $KNS get incidentcapture "$ic" -o jsonpath='{.status.phase}' 2>/dev/null || true)
    [ "$phase" = Exported ] && break
    [ "$phase" = Failed ] && fail "$label: capture failed"
    sleep 1
  done
  [ "$phase" = Exported ] || fail "$label: capture never exported"
  bundle=$(kubectl -n $KNS get incidentcapture "$ic" -o jsonpath='{.status.bundlePath}')
  ctrl=$(kubectl -n $KNS get pod -l app.kubernetes.io/name=kairn --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')
  kubectl -n $KNS exec "$ctrl" -c controller -- /usr/local/bin/kairn cat-bundle "$bundle" > "$OUT/$label.ieb"
  "$KAIRN" verify "$OUT/$label.ieb" >&2 || fail "$label: bundle does not verify OK (an error entry makes it PARTIAL)"
  "$KAIRN" unpack "$OUT/$label.ieb" "$OUT/$label"
  echo "$OUT/$label"
}

check() { # dir, python expression over `entries` (list of diffs/index.json entries), message
  python3 - "$1/diffs/index.json" "$2" "$3" <<'PY' || exit 1
import json, sys
path, expr, msg = sys.argv[1:4]
entries = json.load(open(path))["entries"]
if not eval(expr, {"entries": entries}):
    print(f"FAIL (diffs): {msg}\n" + json.dumps(entries, indent=1)[:3000])
    sys.exit(1)
print(f"  ok: {msg}")
PY
}

step "narrow the capture window to 20s so 'old' is easy to make"
helm upgrade kairn charts/kairn -n $KNS --reuse-values --set profile.preSeconds=20 --wait --timeout 120s >/dev/null
kubectl create namespace $NS >/dev/null 2>&1 || true

step "rollback: v1 (A=1) → v2 (A=2) → rollout undo"
deploy rb 1
kubectl -n $NS set env deploy/rb A=2 >/dev/null
kubectl -n $NS rollout status deploy/rb --timeout=120s >/dev/null
kubectl -n $NS rollout undo deploy/rb >/dev/null
kubectl -n $NS rollout status deploy/rb --timeout=120s >/dev/null
sleep 2
D=$(capture rollback "$(live_pod rb)")
check "$D" 'any(e.get("after",{}).get("revision")=="3" and e["changed_at_source"]=="event" and e["in_range"] is True and any("2 → 1" in l for l in e.get("summary",[])) for e in entries)' \
  "rollback 2→3 is in range, timed by the scale event, and shows A: 2 → 1"

step "scale canary: an old revision scaled 1→2 (HPA-style), then 0→1"
deploy sc 1
sleep 25   # revision 1 is now older than the 20s window
kubectl -n $NS scale deploy/sc --replicas=2 >/dev/null
kubectl -n $NS rollout status deploy/sc --timeout=120s >/dev/null
kubectl -n $NS scale deploy/sc --replicas=0 >/dev/null
sleep 3
kubectl -n $NS scale deploy/sc --replicas=1 >/dev/null
kubectl -n $NS rollout status deploy/sc --timeout=120s >/dev/null
D=$(capture scale "$(live_pod sc)")
check "$D" 'not any(e.get("in_range") is True for e in entries)' \
  "scaling (incl. from zero) is never reported as an in-range change"

step "paused: template edited while the rollout is paused"
deploy pz 1
kubectl -n $NS rollout pause deploy/pz >/dev/null
kubectl -n $NS set env deploy/pz A=2 >/dev/null
sleep 2
D=$(capture paused "$(live_pod pz)")
check "$D" 'any(e["source"]=="deployment-spec-pending" and e["status"]=="ok" and any("1 → 2" in l for l in e.get("summary",[])) for e in entries)' \
  "the unrolled edit is reported as deployment-spec-pending (A: 1 → 2)"

step "recreate: the actor is attributed despite the Recreate delay"
deploy rc 1 Recreate
kubectl -n $NS set env deploy/rc A=2 >/dev/null
kubectl -n $NS rollout status deploy/rc --timeout=120s >/dev/null
D=$(capture recreate "$(live_pod rc)")
# v1 and v2 are written seconds apart, so both managers fall in the match window: the
# latest template write before the ReplicaSet was created is the trigger.
check "$D" 'any(e.get("after",{}).get("revision")=="2" and e.get("actor")=="kubectl-set" for e in entries)' \
  "revision 2 is attributed to kubectl-set (the write that triggered it)"

step "restore the default window"
helm upgrade kairn charts/kairn -n $KNS --reuse-values --set profile.preSeconds=300 --wait --timeout 120s >/dev/null
kubectl delete namespace $NS --wait=false >/dev/null
echo; echo "diffs scenarios OK"
