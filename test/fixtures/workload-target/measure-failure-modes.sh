#!/usr/bin/env bash
# 전제 실측: 워크로드 레벨 알림에서, 알림 시점에 선언된 타깃(Deployment 등)으로부터
# "증거를 가진 파드"를 API 로 식별할 수 있는가. 그리고 그 증거가 실제로 거기 있는가.
#
# 제안서를 쓰기 전에 재는 것이 목적이므로 Lapilli 는 설치하지 않는다. 순수 kubectl 로
# 컨트롤러가 할 수 있는 것만 본다.
#
# 네 가지 실패 모드를 같이 돌린다. 전부 KubeDeploymentReplicasMismatch 를 띄우지만
# 증거의 유무가 다를 것이라는 게 가설이다:
#   A badimage   ImagePullBackOff — 파드는 있고 컨테이너는 한 번도 안 돌았다
#   B crashloop  CrashLoopBackOff — 죽은 인스턴스 로그가 있을 수도 (라운드 35: 10중 7)
#   C notready   Running 인데 readiness 실패 — 로그는 살아있다
#   D unsched    스케줄 불가 — 파드는 Pending, 노드에 간 적이 없다
set -uo pipefail

CLUSTER=lapilli-measure
NS=measure
OUT="${OUT:-/tmp/measure-out}"
NODE_IMAGE=${NODE_IMAGE:-kindest/node:v1.37.0}
mkdir -p "$OUT"

log() { echo "[$(date -u +%H:%M:%S)] $*"; }

if [ "${KEEP_CLUSTER:-0}" != "1" ]; then
  kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
  log "kind 클러스터 생성"
  kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --wait 120s >/dev/null
fi
kubectl config use-context "kind-$CLUSTER" >/dev/null

kubectl create namespace "$NS" >/dev/null 2>&1 || true

log "네 가지 실패 모드 배포"
kubectl apply -n "$NS" -f - >/dev/null <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata: { name: badimage, labels: { app: badimage } }
spec:
  replicas: 3
  selector: { matchLabels: { app: badimage } }
  template:
    metadata: { labels: { app: badimage } }
    spec:
      containers:
        - name: app
          image: registry.invalid/nope:v9
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: crashloop, labels: { app: crashloop } }
spec:
  replicas: 3
  selector: { matchLabels: { app: crashloop } }
  template:
    metadata: { labels: { app: crashloop } }
    spec:
      containers:
        - name: app
          image: busybox:1.37
          command: ["sh","-c","echo BOOT_$(date -u +%H:%M:%S); echo FATAL: cache warmup failed; sleep 2; exit 1"]
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: notready, labels: { app: notready } }
spec:
  replicas: 3
  selector: { matchLabels: { app: notready } }
  template:
    metadata: { labels: { app: notready } }
    spec:
      containers:
        - name: app
          image: busybox:1.37
          command: ["sh","-c","echo SERVING_BUT_NOT_READY; while true; do sleep 5; done"]
          readinessProbe:
            exec: { command: ["sh","-c","exit 1"] }
            periodSeconds: 5
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: unsched, labels: { app: unsched } }
spec:
  replicas: 3
  selector: { matchLabels: { app: unsched } }
  template:
    metadata: { labels: { app: unsched } }
    spec:
      containers:
        - name: app
          image: busybox:1.37
          command: ["sh","-c","sleep 3600"]
          resources: { requests: { cpu: "900", memory: "900Gi" } }
YAML

# 롤아웃이 막힌 Deployment 도 하나: v1 은 건강하게 뜨고 v2 가 깨진다.
# "오브젝트가 그때 그대로" 를 ReplicaSet 히스토리로 되찾을 수 있는지 보기 위한 케이스.
log "rollout-stuck 케이스: v1 건강 → v2 깨짐"
kubectl apply -n "$NS" -f - >/dev/null <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata: { name: rollout, labels: { app: rollout } }
spec:
  replicas: 2
  revisionHistoryLimit: 10
  selector: { matchLabels: { app: rollout } }
  template:
    metadata: { labels: { app: rollout } }
    spec:
      containers:
        - name: app
          image: busybox:1.37
          env: [{ name: CACHE_WARMUP, value: lazy }]
          command: ["sh","-c","echo v1 healthy; while true; do sleep 5; done"]
YAML
kubectl -n "$NS" rollout status deploy/rollout --timeout=120s >/dev/null 2>&1 || true
kubectl -n "$NS" patch deploy rollout --type merge -p \
  '{"spec":{"template":{"spec":{"containers":[{"name":"app","image":"busybox:1.37","env":[{"name":"CACHE_WARMUP","value":"eager"}],"command":["sh","-c","echo v2 dying; exit 1"]}]}}}}' >/dev/null

T0=$(date -u +%s)

# ── 샘플러 ───────────────────────────────────────────────────────────────────
# 컨트롤러가 선언된 타깃만 받았을 때 할 수 있는 일을 그대로 흉내낸다:
#   Deployment → (ownerReferences) ReplicaSet → (ownerReferences) Pod
# 그리고 그중 "증거를 가진 파드" 를 고를 수 있는지 본다.
sample() { # label
  local when=$1 dep
  echo
  echo "############ t+${when} ############"
  for dep in badimage crashloop notready unsched rollout; do
    echo
    echo "--- $dep ---"
    # 1. 선언된 타깃에서 파드까지 오너 체인으로 내려갈 수 있나
    local uid rs pods n_total n_ready n_notready
    uid=$(kubectl -n "$NS" get deploy "$dep" -o jsonpath='{.metadata.uid}' 2>/dev/null)
    rs=$(kubectl -n "$NS" get rs -o json 2>/dev/null \
      | jq -r --arg u "$uid" '.items[]|select(.metadata.ownerReferences[]?.uid==$u)|.metadata.name' | tr '\n' ' ')
    echo "  ReplicaSet: ${rs:-(없음)}"
    pods=$(kubectl -n "$NS" get pods -l app="$dep" -o json 2>/dev/null)
    n_total=$(echo "$pods" | jq '.items|length')
    n_ready=$(echo "$pods" | jq '[.items[]|select((.status.conditions//[])[]?|select(.type=="Ready" and .status=="True"))]|length')
    n_notready=$((n_total - n_ready))
    echo "  파드: 총 $n_total, Ready $n_ready, NotReady $n_notready"
    # 2. 증거를 가진 파드를 고를 수 있나 — NotReady 집합이 비지 않고 모호하지 않은가
    echo "$pods" | jq -r '.items[] | "    "
      + .metadata.name + "  phase=" + .status.phase
      + "  ready=" + (((.status.conditions//[])[]?|select(.type=="Ready")|.status)//"-")
      + "  restarts=" + ((((.status.containerStatuses//[])[0].restartCount)//0)|tostring)
      + "  waiting=" + ((((.status.containerStatuses//[])[0].state.waiting.reason)//"-"))
      + "  terminated=" + ((((.status.containerStatuses//[])[0].lastState.terminated.reason)//"-"))'
    # 3. 그 파드에 실제로 증거가 있나 — 현재 로그와 --previous
    local p
    for p in $(echo "$pods" | jq -r '.items[].metadata.name' | head -3); do
      local cur prev
      cur=$(kubectl -n "$NS" logs "$p" -c app --tail=2 2>&1 | head -2 | tr '\n' '|')
      prev=$(kubectl -n "$NS" logs "$p" -c app --previous --tail=2 2>&1 | head -1 | cut -c1-70)
      echo "    [$p] log=${cur:0:60}"
      echo "    [$p] prev=${prev:0:70}"
    done
    # 4. 이벤트가 아직 있나, 그리고 **이유를 들고 있나**. 판정 기준이 "Loki 가 안 갖고
    #    있는 것" 이므로 개수가 아니라 reason 이 핵심이다.
    local evj
    evj=$(kubectl -n "$NS" get events -o json 2>/dev/null)
    echo "  Deployment 이벤트: $(echo "$evj" | jq --arg d "$dep" '[.items[]|select(.involvedObject.kind=="Deployment" and .involvedObject.name==$d)]|length')건  reasons=$(echo "$evj" | jq -r --arg d "$dep" '[.items[]|select(.involvedObject.kind=="Deployment" and .involvedObject.name==$d)|.reason]|unique|join(",")')"
    echo "  ReplicaSet 이벤트: $(echo "$evj" | jq --arg d "$dep" '[.items[]|select(.involvedObject.kind=="ReplicaSet" and (.involvedObject.name|startswith($d)))]|length')건  reasons=$(echo "$evj" | jq -r --arg d "$dep" '[.items[]|select(.involvedObject.kind=="ReplicaSet" and (.involvedObject.name|startswith($d)))|.reason]|unique|join(",")')"
    echo "  파드 이벤트:      $(echo "$evj" | jq --arg d "$dep" '[.items[]|select(.involvedObject.kind=="Pod" and (.involvedObject.name|startswith($d)))]|length')건  reasons=$(echo "$evj" | jq -r --arg d "$dep" '[.items[]|select(.involvedObject.kind=="Pod" and (.involvedObject.name|startswith($d)))|.reason]|unique|join(",")')"
    echo "  파드 이벤트 메시지 샘플:"
    echo "$evj" | jq -r --arg d "$dep" '[.items[]|select(.involvedObject.kind=="Pod" and (.involvedObject.name|startswith($d)))]|unique_by(.reason)[]|"    " + .reason + ": " + (.message|.[0:100])' 2>/dev/null | head -5
    # 5. 종료 상태 — Loki 에 없는 것. 컨테이너가 돈 적 있으면 여기 남는다.
    echo "  종료 상태:"
    echo "$pods" | jq -r '[.items[]|((.status.containerStatuses//[])[0].lastState.terminated//empty)]|unique_by(.reason)[]?|"    reason=" + (.reason//"-") + " exit=" + ((.exitCode//-1)|tostring) + " finishedAt=" + (.finishedAt//"-")' 2>/dev/null | head -3
  done
  # 5. "오브젝트가 그때 그대로" — 구 ReplicaSet 의 파드 템플릿이 남아 있나
  echo
  echo "--- rollout: 리비전 히스토리로 '그때의 오브젝트'를 되찾을 수 있나 ---"
  kubectl -n "$NS" get rs -l app=rollout \
    -o custom-columns='NAME:.metadata.name,REV:.metadata.annotations.deployment\.kubernetes\.io/revision,DESIRED:.spec.replicas,READY:.status.readyReplicas,WARMUP:.spec.template.spec.containers[0].env[0].value' 2>/dev/null
}

for wait_to in 60 300 900; do
  now=$(date -u +%s); sleep_for=$(( T0 + wait_to - now ))
  [ "$sleep_for" -gt 0 ] && sleep "$sleep_for"
  sample "$((wait_to/60))m" 2>&1 | tee -a "$OUT/samples.txt"
done

log "완료. 결과: $OUT/samples.txt"
[ "${KEEP_CLUSTER:-0}" = "1" ] || kind delete cluster --name "$CLUSTER" >/dev/null 2>&1
