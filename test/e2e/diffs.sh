#!/usr/bin/env bash
# diffs/ scenarios required by docs/design-review-round4.md (the timing heuristics are
# proven here, on a real cluster, not by more review rounds):
#   rollback         — reused RS: activation from the latest coalesced "from 0" event
#   scale canary     — HPA-style 1→2 and scale 0→1 of an old revision: nothing in range
#   paused           — template edited while paused: a deployment-spec-pending entry
#   recreate         — Recreate strategy: the actor is still attributed
#   statefulset      — ControllerRevision history: change, then rollback (revision re-used)
#   daemonset        — ControllerRevision history via the hash-suffix label
#   configmap        — opt-in: the template switched ConfigMap names (kustomize-style);
#                      both are diffed key by key, credentials and binaryData redacted
#
# Usage: test/e2e/diffs.sh <path-to-lapilli-binary> <scratch-dir>   (called by run.sh)
set -euo pipefail

ctrl_pod() { # the controller pod that is not terminating
  kubectl -n "$1" get pods -l app.kubernetes.io/name=lapilli \
    -o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}' | awk 'NR==1'
}

LAPILLI=$1
OUT=$2/diffs
NS=diffs-e2e
KNS=lapilli-system
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

workload() { # kind (StatefulSet|DaemonSet), name, value of A
  local extra=""
  [ "$1" = StatefulSet ] && extra="serviceName: $2
  replicas: 1
  podManagementPolicy: Parallel"
  kubectl apply --server-side --field-manager=e2e -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: $1
metadata: { name: $2, namespace: $NS }
spec:
  $extra
  selector: { matchLabels: { app: $2 } }
  template:
    metadata: { labels: { app: $2 } }
    spec:
      terminationGracePeriodSeconds: 1
      containers:
        - name: app
          image: busybox:1.36
          command: ["sh", "-c", "while true; do sleep 3600; done"]
          env: [{ name: A, value: "$3" }]
EOF
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
  # One alertname per capture: StatefulSet pods keep their name across a rollback, and the
  # webhook's dedup key {rule, cluster, ns/pod, minute} would otherwise (correctly) return
  # the earlier capture.
  ctrl=$(ctrl_pod "$KNS")
  # Fired from inside the controller pod, which holds the webhook token.
  resp=$(printf '{"alerts":[{"status":"firing","labels":{"alertname":"LapilliDiffE2E-%s","namespace":"%s","pod":"%s"}}]}' "$label" "$NS" "$pod" |
    kubectl -n $KNS exec -i "$ctrl" -c controller -- /usr/local/bin/lapilli post-alert)
  ic=$(echo "$resp" | python3 -c 'import json,sys; print(json.load(sys.stdin)["captures"][0])')
  for _ in $(seq 1 60); do
    phase=$(kubectl -n $KNS get incidentcapture "$ic" -o jsonpath='{.status.phase}' 2>/dev/null || true)
    [ "$phase" = Exported ] && break
    [ "$phase" = Failed ] && fail "$label: capture failed"
    sleep 1
  done
  [ "$phase" = Exported ] || fail "$label: capture never exported"
  bundle=$(kubectl -n $KNS get incidentcapture "$ic" -o jsonpath='{.status.bundlePath}')
  ctrl=$(ctrl_pod "$KNS")
  kubectl -n $KNS exec "$ctrl" -c controller -- /usr/local/bin/lapilli cat-bundle "$bundle" > "$OUT/$label.ieb"
  "$LAPILLI" verify "$OUT/$label.ieb" >&2 || fail "$label: bundle does not verify OK (an error entry makes it PARTIAL)"
  "$LAPILLI" unpack "$OUT/$label.ieb" "$OUT/$label"
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

# Only this step needs a narrow window, and it is narrowed only for this step. It used to be
# narrowed here and restored six steps later, which put every later step that asserts
# `in_range is True` in a race against it: the StatefulSet case failed on Kubernetes 1.37 with
# `seconds_relative_to_firing: -30` because a serial StatefulSet rollout took longer than the
# 20s window it had to fit inside. Everything else the assertion wanted was correct.
step "narrow the capture window to 20s, for this step only"
helm upgrade lapilli charts/lapilli -n $KNS --reuse-values --set profile.preSeconds=20 --wait --timeout 120s >/dev/null

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

step "restore the default window before the steps that need changes IN range"
helm upgrade lapilli charts/lapilli -n $KNS --reuse-values --set profile.preSeconds=300 --wait --timeout 120s >/dev/null

step "paused: template edited while the rollout is paused"
deploy pz 1
kubectl -n $NS rollout pause deploy/pz >/dev/null
kubectl -n $NS set env deploy/pz A=2 >/dev/null
sleep 2
D=$(capture paused "$(live_pod pz)")
check "$D" 'any(e["source"]=="deployment-spec-pending" and e["status"]=="ok" and any("1 → 2" in l for l in e.get("summary",[])) for e in entries)' \
  "the unrolled edit is reported as deployment-spec-pending (A: 1 → 2)"

step "recreate: the actor is attributed despite the Recreate delay"
# The old pod ignores SIGTERM, so it takes its full 10s grace period to stop, and Recreate
# only creates the new ReplicaSet after that: the ReplicaSet is born ~10s after the write.
kubectl apply --server-side --field-manager=e2e -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: { name: rc, namespace: $NS }
spec:
  replicas: 1
  strategy: { type: Recreate }
  selector: { matchLabels: { app: rc } }
  template:
    metadata: { labels: { app: rc } }
    spec:
      terminationGracePeriodSeconds: 10
      containers:
        - name: app
          image: busybox:1.36
          command: ["sh", "-c", "trap '' TERM; while true; do sleep 1; done"]
          env: [{ name: A, value: "1" }]
EOF
kubectl -n $NS rollout status deploy/rc --timeout=120s >/dev/null
sleep 2   # managedFields times have 1s resolution: writes in the same second tie (→ null)
kubectl -n $NS set env deploy/rc A=2 >/dev/null
kubectl -n $NS rollout status deploy/rc --timeout=120s >/dev/null
D=$(capture recreate "$(live_pod rc)")
check "$D" 'any(e.get("after",{}).get("revision")=="2" and e.get("actor")=="kubectl-set" for e in entries)' \
  "revision 2 is attributed to kubectl-set although its ReplicaSet was created ~10s later"

step "statefulset: v1 (A=1) → v2 (A=2), then rollout undo"
workload StatefulSet st 1
kubectl -n $NS rollout status statefulset/st --timeout=120s >/dev/null
sleep 2   # distinct managedFields seconds for the two template writes
kubectl -n $NS set env statefulset/st A=2 >/dev/null
kubectl -n $NS rollout status statefulset/st --timeout=120s >/dev/null
D=$(capture sts "$(live_pod st)")
check "$D" 'any(e["kind"]=="StatefulSet" and e.get("after",{}).get("revision")=="2" and e["in_range"] is True and e.get("actor")=="kubectl-set" and any("1 → 2" in l for l in e.get("summary",[])) for e in entries)' \
  "StatefulSet revision 1 → 2 (A: 1 → 2) by kubectl-set, from ControllerRevisions"
kubectl -n $NS rollout undo statefulset/st >/dev/null
kubectl -n $NS rollout status statefulset/st --timeout=120s >/dev/null
sleep 2
D=$(capture sts-undo "$(live_pod st)")
check "$D" 'any(e["kind"]=="StatefulSet" and e.get("after",{}).get("revision")=="3" and e["in_range"] is True and any("2 → 1" in l for l in e.get("summary",[])) for e in entries)' \
  "StatefulSet rollback: the re-used revision is 3 and shows A: 2 → 1"

step "daemonset: v1 (A=1) → v2 (A=2)"
workload DaemonSet ds 1
kubectl -n $NS rollout status daemonset/ds --timeout=120s >/dev/null
sleep 2   # distinct managedFields seconds for the two template writes
kubectl -n $NS set env daemonset/ds A=2 >/dev/null
kubectl -n $NS rollout status daemonset/ds --timeout=120s >/dev/null
D=$(capture ds "$(live_pod ds)")
check "$D" 'any(e["kind"]=="DaemonSet" and e.get("after",{}).get("revision")=="2" and e["in_range"] is True and any("1 → 2" in l for l in e.get("summary",[])) for e in entries)' \
  "DaemonSet revision 1 → 2 (A: 1 → 2), pod matched by its hash-suffix label"

step "configmap: kustomize-style rename cfg-v1 → cfg-v2 (opt-in diffs.configMaps)"
helm upgrade lapilli charts/lapilli -n $KNS --reuse-values --set diffs.configMaps=true \
  --set "watchNamespaces={$NS}" --wait --timeout 120s >/dev/null
cm_deploy() { # configmap name, MODE, password canary, keystore bytes (base64)
  kubectl apply --server-side --field-manager=e2e -f - >/dev/null <<EOF
apiVersion: v1
kind: ConfigMap
metadata: { name: $1, namespace: $NS }
data:
  MODE: "$2"
  application.yaml: |
    cache:
      mode: $2
    db:
      password: $3
binaryData:
  keystore.p12: $4
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: cm, namespace: $NS }
spec:
  replicas: 1
  selector: { matchLabels: { app: cm } }
  template:
    metadata: { labels: { app: cm } }
    spec:
      terminationGracePeriodSeconds: 1
      containers:
        - name: app
          image: busybox:1.36
          command: ["sh", "-c", "while true; do sleep 3600; done"]
          envFrom: [{ configMapRef: { name: $1 } }]
          volumeMounts: [{ name: cfg, mountPath: /etc/app }]
      volumes: [{ name: cfg, configMap: { name: $1 } }]
EOF
  kubectl -n $NS rollout status deploy/cm --timeout=120s >/dev/null
}
cm_deploy cfg-v1 lazy cmCanaryOld4Rt8 b2xkLWtleXN0b3JlLWJ5dGVz   # "old-keystore-bytes"
sleep 2
cm_deploy cfg-v2 eager cmCanaryNew4Rt8 bmV3LWtleXN0b3JlLWJ5dGVz  # "new-keystore-bytes"
D=$(capture configmap "$(live_pod cm)")
check "$D" 'any(e["kind"]=="ConfigMap" and e["name"]=="cfg-v2" and e["status"]=="ok" and "data[MODE]: lazy → eager" in e.get("summary",[]) for e in entries)' \
  "ConfigMap cfg-v1 → cfg-v2 diffed key by key (MODE: lazy → eager)"
if grep -rlE "cmCanary(Old|New)4Rt8|keystore-bytes|a2V5c3RvcmUtYnl0ZXM" "$D"; then
  fail "configmap: a credential or binaryData leaked into the bundle"
fi
echo "  ok: ConfigMap credentials and binaryData absent from every bundle file"
helm upgrade lapilli charts/lapilli -n $KNS --reuse-values --set diffs.configMaps=false \
  --set-json 'watchNamespaces=[]' --wait --timeout 120s >/dev/null

kubectl delete namespace $NS --wait=false >/dev/null
echo; echo "diffs scenarios OK"
