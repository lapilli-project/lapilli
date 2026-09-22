#!/usr/bin/env bash
# Alert-storm harness: what a node failure does to the controller.
#
# The question this exists to answer is the one that decides whether anyone runs this at all:
# **does Lapilli fall over during the incident it is supposed to be recording?** A node going
# down fires one alert rule for every pod on it, Alertmanager delivers them in ONE payload, and
# `webhook.rs` creates one IncidentCapture per firing alert with no cap. Each of those then
# collects logs, walks ReplicaSet history, queries Prometheus, hashes and compresses — inside a
# pod the chart limits to 256 MiB.
#
# Nothing in this repository has ever measured that. This prints the numbers rather than
# asserting a guess about them, and asserts only the two things that are not opinions: the
# controller must survive, and every capture that was asked for must be accounted for.
#
# Usage: test/e2e/storm.sh [N]     (N alerts in one payload; default 20)
set -euo pipefail

N=${1:-20}
KNS=lapilli-system
NS=storm-e2e
APP=storm
CLUSTER=${CLUSTER:-lapilli}

step() { echo; echo "==> storm: $*"; }
fail() { echo "FAIL (storm): $*"; kubectl -n $KNS logs "deploy/lapilli" --tail=40 || true; exit 1; }

ctrl_pod() {
  kubectl -n "$KNS" get pods -l app.kubernetes.io/name=lapilli \
    -o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}' \
    | awk 'NR==1'
}

MPORT=18085
PF=""
cleanup() { [ -n "$PF" ] && kill "$PF" 2>/dev/null || true; }
trap cleanup EXIT

scrape() { # → /metrics on stdout
  kubectl -n $KNS port-forward "deploy/lapilli" $MPORT:8081 >/dev/null 2>&1 &
  PF=$!
  for _ in $(seq 1 30); do curl -sf "localhost:$MPORT/metrics" >/dev/null 2>&1 && break; sleep 1; done
  curl -sf "localhost:$MPORT/metrics" || true
  kill "$PF" 2>/dev/null || true; wait "$PF" 2>/dev/null || true; PF=""
}
gauge() { awk -v k="$1" '$1==k{print $2}' <<<"$2"; }

# ---------------------------------------------------------- the kernel's high-water mark -----
#
# Sampling `/metrics` cannot measure a peak here and an earlier version of this script pretended
# otherwise. The storm finishes in one to two seconds; one `scrape()` costs a port-forward, a
# retry loop and a curl — over a second — and the loop breaks as soon as every capture is
# terminal, BEFORE its `sleep`. So the "peak" was one sample taken after the work was over:
# n=1, and not even during. Every number in the first version of the concurrency table was that.
#
# `memory.peak` is a high-water mark the kernel maintains itself, so it needs no sampling at all
# and cannot miss a spike. Two honest caveats, because this number is about to be quoted:
#   * it is the high-water mark of `memory.current`, which includes reclaimable page cache, so it
#     is an UPPER bound on the anonymous working set, not the working set;
#   * this kernel (6.5) cannot reset it — the write landed in 6.8 — so it is the peak since the
#     pod started. Measure on a fresh pod or the previous run's peak is still in it.
# The limit is read from `memory.max` on the same cgroup rather than assumed, because that is the
# cgroup the OOM killer acts on and the chart's limit could move.
NODE_CTR="${CLUSTER}-control-plane"
CGROUP=""
cgroup_for_pod() { # $1 = pod name → sets CGROUP, or leaves it empty
  local uid slice
  uid=$(kubectl -n "$KNS" get pod "$1" -o jsonpath='{.metadata.uid}' 2>/dev/null) || return 0
  [ -n "$uid" ] || return 0
  slice=$(docker exec "$NODE_CTR" find /sys/fs/cgroup/kubelet.slice -maxdepth 4 -type d \
    -name "*pod${uid//-/_}.slice" 2>/dev/null | head -1) || return 0
  [ -n "$slice" ] || return 0
  docker exec "$NODE_CTR" test -r "$slice/memory.peak" 2>/dev/null || return 0
  CGROUP="$slice"
}
cg_read() { docker exec "$NODE_CTR" cat "$CGROUP/$1" 2>/dev/null; }

# ------------------------------------------------------------------- the workload ----------

step "a Deployment of $N pods that all crash, so one rule fires for every one of them"
kubectl create namespace $NS >/dev/null 2>&1 || true
kubectl -n $NS apply --server-side --field-manager=storm-e2e -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: { name: $APP, namespace: $NS }
spec:
  replicas: $N
  selector: { matchLabels: { app: $APP } }
  template:
    metadata: { labels: { app: $APP } }
    spec:
      terminationGracePeriodSeconds: 1
      containers:
        - name: app
          image: busybox:1.37
          command: ["sh", "-c", "echo storm-last-words; exit 7"]
          resources: { requests: { cpu: 1m, memory: 8Mi } }
EOF
# Wait for pods to exist and have a terminated instance to collect — not for Ready, which never
# comes.
for _ in $(seq 1 60); do
  n=$(kubectl -n $NS get pods -l app=$APP -o name 2>/dev/null | wc -l | tr -d ' ')
  [ "$n" -ge "$N" ] && break; sleep 2
done
PODS=$(kubectl -n $NS get pods -l app=$APP -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
[ "$(wc -l <<<"$PODS")" -ge "$N" ] || fail "only $(wc -l <<<"$PODS") of $N pods exist"
sleep 20   # let the kubelet record a terminated instance for each

CTRL=$(ctrl_pod)
cgroup_for_pod "$CTRL"
if [ -n "$CGROUP" ]; then
  CG_IDLE=$(cg_read memory.current)
  CG_LIMIT=$(cg_read memory.max)
  echo "  kernel high-water mark available (cgroup v2, limit ${CG_LIMIT}B)"
else
  # Not a silent fallback. A guard that cannot fail reads as a pass, and the whole point of this
  # block is that the sampled number is NOT a peak — so say so in the output that gets quoted.
  echo "  NOTE: no readable memory.peak for $CTRL (not kind, or no docker access here)."
  echo "        The peak below is then a SAMPLED LOWER BOUND, not a peak. Do not quote it as one."
fi

BEFORE=$(scrape)
RSS_IDLE=$(gauge process_resident_memory_bytes "$BEFORE")
CPU_BEFORE=$(gauge process_cpu_seconds_total "$BEFORE")
SEALED_BEFORE=$(awk -F'[{}= ]+' '/^lapilli_captures_total\{result="sealed"\}/{print $NF}' <<<"$BEFORE")
FAILED_BEFORE=$(awk -F'[{}= ]+' '/^lapilli_captures_total\{result="failed"\}/{print $NF}' <<<"$BEFORE")
: "${SEALED_BEFORE:=0}"; : "${FAILED_BEFORE:=0}"
echo "  idle RSS ${RSS_IDLE}B · cpu ${CPU_BEFORE}s · sealed so far ${SEALED_BEFORE}"

# --------------------------------------------------------------------- the storm ------------

step "one payload, $N firing alerts — what Alertmanager sends when a node dies"
# The alerts have to be the SIZE Alertmanager actually sends, not the minimum this test needs.
# An earlier version posted four labels and nothing else — 154 bytes per alert, 6.9x smaller than
# a real one — so it could not see the body limit at all. A real kube-prometheus-stack
# `KubePodCrashLooping` alert measures 1,059 bytes with its annotations, `generatorURL`,
# `fingerprint` and timestamps. `startsAt` matters for more than size: without it the webhook
# falls back to `Utc::now()`, so a retry lands in a new minute bucket and creates a SECOND full
# set of captures — the harness was not exercising production's dedup.
# NOTE: the pod list comes in through the ENVIRONMENT, not stdin. A command cannot have both a
# heredoc and a herestring — the heredoc silently wins and stdin is empty — and this exact
# script shipped that bug: it generated `{"alerts": []}`, 14 bytes, and the only reason it was
# caught is that the line below prints the payload size instead of trusting it.
# `startsAt` is a variable, not a constant, because it turned out to be load-bearing. Alertmanager
# resends a still-firing alert every `repeat_interval` (4h) carrying its ORIGINAL `startsAt`, so a
# production payload routinely names a firing time hours old. STORM_STARTS_AT=now measures the
# fresh-alert case; the default measures the resend case.
: "${STORM_STARTS_AT:=2026-09-22T06:41:12.238Z}"
if [ "$STORM_STARTS_AT" = "now" ]; then
  STORM_STARTS_AT=$(date -u +%Y-%m-%dT%H:%M:%S.000Z)
fi
echo "  alerts claim they started firing at $STORM_STARTS_AT"
PAYLOAD=$(STORM_PODS="$PODS" STORM_STARTS_AT="$STORM_STARTS_AT" python3 - "$NS" <<'PYGEN'
import json, os, sys
ns = sys.argv[1]
pods = os.environ["STORM_PODS"].split()
assert pods, "no pods to build a payload from"
def alert(pod):
    return {
        "status": "firing",
        "labels": {
            "alertname": "LapilliStormE2E", "container": "app", "endpoint": "http",
            "instance": "10.244.3.17:8080", "job": "kube-state-metrics", "namespace": ns,
            "pod": pod, "prometheus": "monitoring/kube-prometheus-stack-prometheus",
            "reason": "CrashLoopBackOff", "service": "kube-prometheus-stack-kube-state-metrics",
            "severity": "warning", "uid": "3f8a1c2e-9b4d-4e7a-8c1f-2d5e6a7b8c9d",
            "lapilli.dev/export": "local",
        },
        "annotations": {
            "description": "Pod %s/%s (app) is in waiting state (reason: CrashLoopBackOff)." % (ns, pod),
            "runbook_url": "https://runbooks.prometheus-operator.dev/runbooks/kubernetes/kubepodcrashlooping",
            "summary": "Pod is crash looping.",
        },
        "startsAt": os.environ["STORM_STARTS_AT"],
        "endsAt": "0001-01-01T00:00:00Z",
        "generatorURL": "http://prometheus.monitoring.svc:9090/graph?g0.expr=max_over_time%28kube_pod_container_status_waiting_reason%7Bjob%3D%22kube-state-metrics%22%2Creason%3D%22CrashLoopBackOff%22%7D%5B5m%5D%29+%3E%3D+1&g0.tab=1",
        "fingerprint": "a1b2c3d4e5f60718",
    }
print(json.dumps({"alerts": [alert(p) for p in pods]}))
PYGEN
)
BYTES=$(printf '%s' "$PAYLOAD" | wc -c | tr -d ' ')
echo "  payload ${BYTES} bytes for $N alerts ($((BYTES / N)) per alert)"
CTRL=$(ctrl_pod)
START=$(date +%s)
RESP=$(kubectl -n $KNS exec -i "$CTRL" -c controller -- /usr/local/bin/lapilli post-alert <<<"$PAYLOAD" 2>&1) \
  || fail "the webhook refused the storm payload: $RESP"
MINE=$(grep -o 'ic-[0-9a-f]*' <<<"$RESP" | sort -u)
ASKED=$(wc -l <<<"$MINE" | tr -d ' ')
DROPPED=$(python3 -c 'import json,sys; print(json.loads(sys.argv[1]).get("dropped", -1))' "$RESP" 2>/dev/null || echo -1)
echo "  webhook accepted; distinct captures requested: $ASKED, alerts dropped: $DROPPED"

# ------------------------------------------------------- peak, while it is happening --------

step "sampling the controller while it works"
PEAK=$RSS_IDLE
for _ in $(seq 1 40); do
  M=$(scrape)
  R=$(gauge process_resident_memory_bytes "$M")
  [ -n "$R" ] && [ "$R" -gt "$PEAK" ] && PEAK=$R
  DONE=$(kubectl -n $KNS get incidentcapture -o json \
    | MINE="$MINE" python3 -c '
import json, os, sys
mine = set(os.environ["MINE"].split())
d = json.load(sys.stdin)
print(sum(1 for i in d["items"]
          if i["metadata"]["name"] in mine
          and i.get("status", {}).get("phase") in ("Exported", "Failed")))')
  [ "$DONE" -ge "$ASKED" ] && break
  sleep 5
done
END=$(date +%s)
AFTER=$(scrape)

# -------------------------------------------------------------------- the numbers -----------

RSS_AFTER=$(gauge process_resident_memory_bytes "$AFTER")
CPU_AFTER=$(gauge process_cpu_seconds_total "$AFTER")
FDS=$(gauge process_open_fds "$AFTER")
SEALED_AFTER=$(awk -F'[{}= ]+' '/^lapilli_captures_total\{result="sealed"\}/{print $NF}' <<<"$AFTER")
FAILED_AFTER=$(awk -F'[{}= ]+' '/^lapilli_captures_total\{result="failed"\}/{print $NF}' <<<"$AFTER")
: "${SEALED_AFTER:=0}"; : "${FAILED_AFTER:=0}"
RESTARTS=$(kubectl -n $KNS get pod "$CTRL" -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null || echo "?")
LIMIT=$(kubectl -n $KNS get pod "$CTRL" -o jsonpath='{.spec.containers[0].resources.limits.memory}' 2>/dev/null || echo "?")

CG_PEAK=""
if [ -n "$CGROUP" ]; then
  CG_PEAK=$(cg_read memory.peak)
  CG_NOW=$(cg_read memory.current)
fi

echo
echo "  ---- storm of $N alerts in one payload ----"
printf "  captures requested      %s\n" "$ASKED"
printf "  captures finished       %s of %s\n" "$DONE" "$ASKED"
printf "  sealed (delta)          %s\n" "$((SEALED_AFTER - SEALED_BEFORE))"
printf "  failed (delta)          %s\n" "$((FAILED_AFTER - FAILED_BEFORE))"
printf "  wall clock              %ss\n" "$((END - START))"
if [ -n "$CG_PEAK" ]; then
  printf "  cgroup idle → PEAK → now %s → %s → %s bytes   (kernel high-water, authoritative)\n" \
    "$CG_IDLE" "$CG_PEAK" "$CG_NOW"
  printf "  PEAK as %% of limit      %s (limit %sB from memory.max)\n" \
    "$(python3 -c "print(f'{$CG_PEAK/$CG_LIMIT*100:.1f}%')" 2>/dev/null || echo '?')" "$CG_LIMIT"
  printf "  sampled RSS peak        %s bytes   (cross-check only — sampling cannot see a 2s spike)\n" "$PEAK"
else
  printf "  RSS idle → sample → now %s → %s → %s bytes   (SAMPLED LOWER BOUND, not a peak)\n" \
    "$RSS_IDLE" "$PEAK" "$RSS_AFTER"
  printf "  sample as %% of limit    %s (limit %s)\n" \
    "$(python3 -c "print(f'{$PEAK/268435456*100:.1f}%')" 2>/dev/null || echo '?')" "$LIMIT"
fi
printf "  CPU seconds consumed    %s\n" "$(python3 -c "print(f'{$CPU_AFTER - $CPU_BEFORE:.2f}')" 2>/dev/null || echo '?')"
printf "  open fds                %s\n" "$FDS"
printf "  controller restarts     %s\n" "$RESTARTS"
echo "  ------------------------------------------"

# ------------------------------------------------------------------- the assertions ---------
#
# Only the things that are not opinions. The numbers above are measurements to read, not
# thresholds to guess: picking a memory budget before anyone has seen one is how the retention
# design got its first byte ceiling wrong.

step "the controller survived its own storm"
[ "$RESTARTS" = "0" ] || fail "the controller restarted $RESTARTS times — an OOM kill during an incident is the one failure that ends adoption"
PHASE_READY=$(kubectl -n $KNS get pod "$CTRL" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')
[ "$PHASE_READY" = "True" ] || fail "the controller is not Ready after the storm"
echo "  ok: 0 restarts, still Ready"

step "every alert in the payload is accounted for — captured or counted as dropped"
# This is the assertion that a silent cliff cannot survive. Before it existed, a payload over the
# body limit lost every alert in it while all four `webhook_requests_total` outcomes stayed flat:
# zero captures, zero telemetry, a 413 nobody was looking at. An identity — asked + dropped = sent
# — has no place for that to hide, and it fails whether the loss is a body limit, a filter that
# over-matches, or a future cap.
[ "$DROPPED" -ge 0 ] || fail "the webhook response carried no \`dropped\` field: $RESP"
[ "$((ASKED + DROPPED))" -eq "$N" ] \
  || fail "$N alerts went in, $ASKED captures + $DROPPED dropped came out — $((N - ASKED - DROPPED)) vanished with no record"
echo "  ok: $ASKED captured + $DROPPED dropped = $N sent"

step "every capture the webhook accepted reached a terminal phase"
[ "$DONE" -ge "$ASKED" ] || fail "only $DONE of $ASKED captures reached a terminal phase; the rest are stuck"
echo "  ok: $DONE of $ASKED reached a terminal phase"

step "cleanup"
kubectl -n $KNS delete incidentcapture --all >/dev/null 2>&1 || true
kubectl delete namespace $NS --wait=false >/dev/null 2>&1 || true
echo "  storm harness done"
