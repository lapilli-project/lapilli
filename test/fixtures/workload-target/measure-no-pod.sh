#!/usr/bin/env bash
# 전제 실측 2: 가리킬 파드가 없을 때, 증거가 선언된 타깃 자체에 있는가.
#
# 1차 측정은 네 모드 모두 NotReady 3/3 이었다 — 식별이 가장 쉬운 경우. 이번에는
# 그 조건이 깨지는 경로를 재다: 파드가 애초에 생성되지 못하거나, 전부 Ready 인데도
# 알림이 뜨는 경우.
#
# 판정 기준은 decision-rule-2.md 에 결과를 보기 전에 고정했다. 요점: 운영자가 읽고
# 무엇을 고칠지 알 수 있는 문자열이 Deployment 또는 ReplicaSet 에 있는가.
set -uo pipefail

CLUSTER=lapilli-nopod
NS_Q=nopod-quota
NS_P=nopod-paused
NS_T=nopod-term
OUT="${OUT:-/tmp/nopod-out}"
NODE_IMAGE=${NODE_IMAGE:-kindest/node:v1.37.0}
mkdir -p "$OUT"
log() { echo "[$(date -u +%H:%M:%S)] $*"; }

kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
log "kind 클러스터 생성"
kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --wait 120s >/dev/null
kubectl config use-context "kind-$CLUSTER" >/dev/null
for n in "$NS_Q" "$NS_P" "$NS_T"; do kubectl create namespace "$n" >/dev/null 2>&1 || true; done

# ── A. quota: ResourceQuota 가 파드 생성을 막는다 ───────────────────────────
log "A quota: 파드 1개만 허용하는 쿼터 아래에서 replicas=3 (전용 네임스페이스)"
kubectl apply -n "$NS_Q" -f - >/dev/null <<'YAML'
apiVersion: v1
kind: ResourceQuota
metadata: { name: only-one-pod }
spec:
  hard: { pods: "1" }
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: quota, labels: { app: quota } }
spec:
  replicas: 3
  selector: { matchLabels: { app: quota } }
  template:
    metadata: { labels: { app: quota } }
    spec:
      containers:
        - name: app
          image: busybox:1.37
          command: ["sh","-c","echo quota-pod up; while true; do sleep 5; done"]
YAML

# ── B. paused: 템플릿을 바꾸고 롤아웃을 멈춘다 ──────────────────────────────
log "B paused: v1 건강하게 띄운 뒤 템플릿 변경 + 일시정지 (쿼터 없는 네임스페이스)"
kubectl apply -n "$NS_P" -f - >/dev/null <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata: { name: paused, labels: { app: paused } }
spec:
  replicas: 2
  selector: { matchLabels: { app: paused } }
  template:
    metadata: { labels: { app: paused } }
    spec:
      containers:
        - name: app
          image: busybox:1.37
          env: [{ name: CACHE_WARMUP, value: lazy }]
          command: ["sh","-c","echo paused v1; while true; do sleep 5; done"]
YAML
kubectl -n "$NS_P" rollout status deploy/paused --timeout=180s >/dev/null 2>&1 || true
kubectl -n "$NS_P" rollout pause deploy/paused >/dev/null
kubectl -n "$NS_P" set env deploy/paused CACHE_WARMUP=eager >/dev/null

# ── C. terminating: finalizer 로 파드를 Terminating 에 묶는다 ───────────────
log "C terminating: finalizer 를 붙인 파드를 삭제해 Terminating 에 고정 (쿼터 없는 네임스페이스)"
kubectl apply -n "$NS_T" -f - >/dev/null <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata: { name: terming, labels: { app: terming } }
spec:
  replicas: 2
  selector: { matchLabels: { app: terming } }
  template:
    metadata:
      labels: { app: terming }
      finalizers: [ "measure.local/hold" ]
    spec:
      terminationGracePeriodSeconds: 1
      containers:
        - name: app
          image: busybox:1.37
          command: ["sh","-c","echo terming up; while true; do sleep 5; done"]
YAML
kubectl -n "$NS_T" rollout status deploy/terming --timeout=180s >/dev/null 2>&1 || true
# 파드 하나를 삭제 → finalizer 때문에 Terminating 에서 멈춘다. Deployment 는 대체본을 만든다.
VICTIM=$(kubectl -n "$NS_T" get pods -l app=terming -o jsonpath='{.items[0].metadata.name}')
log "   희생 파드: ${VICTIM}"
kubectl -n "$NS_T" delete pod "$VICTIM" --wait=false >/dev/null 2>&1 || true

T0=$(date -u +%s)

probe() {
  local when=$1 dep
  echo
  echo "############ t+${when} ############"
  for dep in quota paused terming; do
    local ns
    case "$dep" in quota) ns=$NS_Q ;; paused) ns=$NS_P ;; terming) ns=$NS_T ;; esac
    echo
    echo "--- ${dep}  (ns=${ns}) ---"
    # Deployment 의 status: 알림이 보는 숫자 그대로
    kubectl -n "$ns" get deploy "$dep" -o json 2>/dev/null | jq -r '
      "  spec.replicas=" + ((.spec.replicas//0)|tostring)
      + " status.replicas=" + ((.status.replicas//0)|tostring)
      + " available=" + ((.status.availableReplicas//0)|tostring)
      + " ready=" + ((.status.readyReplicas//0)|tostring)
      + " updated=" + ((.status.updatedReplicas//0)|tostring)
      + "\n  generation=" + ((.metadata.generation//0)|tostring)
      + " observedGeneration=" + ((.status.observedGeneration//0)|tostring)
      + " paused=" + ((.spec.paused//false)|tostring)'
    # status 조건 — 여기에 이유가 있을 수 있다
    echo "  status.conditions:"
    kubectl -n "$ns" get deploy "$dep" -o json 2>/dev/null | jq -r '
      (.status.conditions//[])[] | "    " + .type + "=" + .status
      + " reason=" + (.reason//"-") + "  msg=" + ((.message//"-")|.[0:120])'
    # 파드: NotReady 집합이 정말 비는가
    local pods n_total n_ready n_term
    pods=$(kubectl -n "$ns" get pods -l app="$dep" -o json 2>/dev/null)
    n_total=$(echo "$pods" | jq '.items|length')
    n_ready=$(echo "$pods" | jq '[.items[]|select((.status.conditions//[])[]?|select(.type=="Ready" and .status=="True"))]|length')
    n_term=$(echo "$pods" | jq '[.items[]|select(.metadata.deletionTimestamp)]|length')
    echo "  파드: 총 ${n_total}, Ready ${n_ready}, NotReady $((n_total-n_ready)), Terminating ${n_term}"
    echo "$pods" | jq -r '.items[]|"    " + .metadata.name + "  phase=" + .status.phase
      + "  ready=" + (((.status.conditions//[])[]?|select(.type=="Ready")|.status)//"-")
      + "  deleting=" + (if .metadata.deletionTimestamp then "yes" else "no" end)'
    # 증거가 타깃 자체에 있는가 — Deployment / ReplicaSet 이벤트의 reason + message
    local evj
    evj=$(kubectl -n "$ns" get events -o json 2>/dev/null)
    echo "  Deployment 이벤트:"
    echo "$evj" | jq -r --arg d "$dep" '[.items[]|select(.involvedObject.kind=="Deployment" and .involvedObject.name==$d)]
      | unique_by(.reason)[]? | "    " + .reason + ": " + ((.message//"")|.[0:130])'
    echo "  ReplicaSet 이벤트:"
    echo "$evj" | jq -r --arg d "$dep" '[.items[]|select(.involvedObject.kind=="ReplicaSet" and (.involvedObject.name|startswith($d)))]
      | unique_by(.reason)[]? | "    " + .reason + ": " + ((.message//"")|.[0:130])'
  done
}

for wait_to in 60 300 900; do
  now=$(date -u +%s); s=$(( T0 + wait_to - now )); [ "$s" -gt 0 ] && sleep "$s"
  probe "$((wait_to/60))m" 2>&1 | tee -a "$OUT/nopod.txt"
done

log "정리: finalizer 제거"
for p in $(kubectl -n "$NS_T" get pods -l app=terming -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
  kubectl -n "$NS_T" patch pod "$p" --type merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
done
log "완료: ${OUT}/nopod.txt"
kind delete cluster --name "$CLUSTER" >/dev/null 2>&1
