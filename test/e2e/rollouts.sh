#!/usr/bin/env bash
# Is the rollouts reader right on a real kube-state-metrics? What people did
# when a rule fired, read from the changes made to the workloads an alert is
# about (docs/design/responses.md, "What comes in"), against a real
# kube-state-metrics, scraped by a real Prometheus, on a kind cluster.
#
# (The library's tests answer its reads with a stand-in that matches PromQL by
# pattern and never evaluates it, which let a read through that Prometheus
# itself would have refused for the samples it held. This is the check that
# asks the real thing.)
#
# What it does:
#   - one kind cluster, named for the run, with its own kubeconfig in the work
#     directory, and kube-prometheus-stack (pinned, as kubernetes-rules.sh has
#     it) with Grafana and Alertmanager off; scrape and evaluation intervals
#     30s, which are the chart's own defaults, said here and passed to --step;
#   - in a namespace of its own, workloads of pause containers, so that
#     nothing is pulled but the one image:
#       web     a Deployment (1 replica): restarted (a rollout), scaled to 3
#               (a scale), rolled back to the first (a rollback);
#       db      a StatefulSet: an environment variable changed (a rollout);
#       agent   a DaemonSet: its template changed (a rollout);
#       auto    a Deployment a HorizontalPodAutoscaler targets, scaled by hand:
#               a scale the autoscaler's being there excludes;
#       zero    a Deployment scaled to 0 and back (a scale each way, and
#               never a rollback, though its ReplicaSet goes from 0 to more);
#     each change four minutes after the last, so that no firing's window holds
#     two;
#   - a --firings file of probes, a firing for each change made, over a span
#     around it, and one over a quiet span for each workload, carrying the
#     workload's namespace and its kind's label (deployment, statefulset,
#     daemonset); then `lapilli verdict --firings FILE --rollouts
#     --prometheus <port-forwarded Prometheus> --json`.
#
# Passes only if, in that report, each probe's response rests on the change of
# the kind it was made as, at a time within one step and a scrape of when it
# was made; nothing is found of the scale of the autoscaled Deployment nor of a
# rollback of the one scaled from zero (the changes read are exactly those
# made: seven); the autoscalers' targets are known; and each workload's
# changes are known over a span none was made in (the page a person
# acknowledged there is not useful, and not "unknown").
#
# Needs Docker, Go, kubectl, helm, curl and jq. Takes about 35 minutes: ten to
# set up, sixteen for the changes and the minutes after them. Deletes the
# cluster afterwards unless KEEP is set. PROMETHEUS_IMAGE runs another
# Prometheus than the chart's own.
# shellcheck disable=SC2016,SC2329 # PromQL and jq, not the shell's; cleanup runs on EXIT
set -euo pipefail
cd "$(dirname "$0")/../.."

chart=oci://ghcr.io/prometheus-community/charts/kube-prometheus-stack
chart_version=93.2.0 # sha256:5dd8011a7815c4fae128b93849bd890d694e1830c7a2eca82732a25429cd0fb7
node=kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed
pause=registry.k8s.io/pause:3.10@sha256:ee6521f290b2168b6e0935a181d4cff9be1ac3f505666ef0e3c98fae8199917a

step_s=30 # scrape and evaluation interval, as --step
step=${step_s}s
# How far past a change the step it is seen at may be: a step, and a scrape
# of what it changed, and a little for the controllers.
late=$((step_s + step_s + 10))
gap=240 # seconds between one change of a workload and the next

cluster=lapilli-rollouts-$$
ns=monitoring
workloads=rollouts
release=kps
svc=$release-prometheus
work=$(mktemp -d)
export KUBECONFIG=$work/kubeconfig
pf=

cleanup() {
  status=$?
  if [ "$status" -ne 0 ]; then
    echo "::group::what the cluster held"
    kubectl get pods -A -o wide || true
    kubectl -n "$workloads" get all,hpa || true
    kubectl -n "$ns" logs "prometheus-$svc-0" -c prometheus --tail=100 || true
    echo "::endgroup::"
  fi
  [ -z "$pf" ] || kill "$pf" 2>/dev/null || true
  if [ -z "${KEEP:-}" ]; then
    "$bin/kind" delete cluster --name "$cluster" || true
    rm -rf "$work"
  else
    echo "kept: $work, cluster $cluster (KUBECONFIG=$KUBECONFIG)"
  fi
}
bin=$(go env GOPATH)/bin
trap cleanup EXIT
trap 'echo "stopped at line $LINENO: $BASH_COMMAND" >&2' ERR

now() { date +%s; }
iso() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$1" +%Y-%m-%dT%H:%M:%SZ; }
say() { printf '%s  %s\n' "$(date -u +%T)" "$*"; }
until_true() {
  local end=$(($(date +%s) + $1))
  shift
  until "$@" >/dev/null 2>&1; do
    if [ "$(date +%s)" -ge "$end" ]; then
      echo "gave up waiting for: $*" >&2
      return 1
    fi
    sleep 3
  done
}
wait_until() { while [ "$(now)" -lt "$1" ]; do sleep 2; done; }

prom_values=()
if [ -n "${PROMETHEUS_IMAGE:-}" ]; then
  if ! [[ $PROMETHEUS_IMAGE =~ ^[A-Za-z0-9][A-Za-z0-9._/:@-]*$ ]]; then
    echo "PROMETHEUS_IMAGE is not an image reference: '$PROMETHEUS_IMAGE'" >&2
    exit 2
  fi
  ref=$PROMETHEUS_IMAGE sha='' tag='' registry=docker.io
  case $ref in *@sha256:*) sha=${ref#*@} ref=${ref%%@*} ;; esac
  case ${ref##*/} in *:*) tag=${ref##*:} ref=${ref%:*} ;; esac
  repository=$ref
  case ${ref%%/*} in
    *.* | *:* | localhost) if [[ $ref == */* ]]; then registry=${ref%%/*} repository=${ref#*/}; fi ;;
  esac
  prom_values=(--set-string "prometheus.prometheusSpec.image.registry=$registry"
    --set-string "prometheus.prometheusSpec.image.repository=$repository"
    --set-string "prometheus.prometheusSpec.image.tag=$tag" --set-string "prometheus.prometheusSpec.image.sha=$sha")
  echo "PROMETHEUS_IMAGE=$PROMETHEUS_IMAGE"
fi

go build -o "$work/lapilli" ./cmd/lapilli
go install sigs.k8s.io/kind@v0.33.0

"$bin/kind" create cluster --name "$cluster" --image "$node" --kubeconfig "$KUBECONFIG" --wait 3m

helm upgrade --install "$release" "$chart" --version "$chart_version" -n "$ns" --create-namespace \
  --set fullnameOverride="$release" \
  --set grafana.enabled=false --set alertmanager.enabled=false \
  --set prometheus.prometheusSpec.scrapeInterval="$step" --set prometheus.prometheusSpec.evaluationInterval="$step" \
  --set prometheus.prometheusSpec.retention=6h \
  ${prom_values[@]+"${prom_values[@]}"} \
  --wait --timeout 10m

papi() { kubectl get --raw "/api/v1/namespaces/$ns/services/http:$svc:http-web/proxy$1"; }
scraped() {
  papi /api/v1/targets | jq -e '(.data.activeTargets | length) > 0 and all(.data.activeTargets[]; .lastScrape != "0001-01-01T00:00:00Z")'
}
until_true 600 scraped

kubectl create namespace "$workloads"
kubectl apply -n "$workloads" -f - <<YAML
apiVersion: apps/v1
kind: Deployment
metadata: {name: web}
spec:
  replicas: 1
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      containers:
        - {name: main, image: "$pause"}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: auto}
spec:
  replicas: 1
  selector: {matchLabels: {app: auto}}
  template:
    metadata: {labels: {app: auto}}
    spec:
      containers:
        - name: main
          image: "$pause"
          resources: {requests: {cpu: 10m}}
---
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata: {name: auto}
spec:
  scaleTargetRef: {apiVersion: apps/v1, kind: Deployment, name: auto}
  minReplicas: 1
  maxReplicas: 6
  metrics:
    - type: Resource
      resource: {name: cpu, target: {type: Utilization, averageUtilization: 80}}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: zero}
spec:
  replicas: 1
  selector: {matchLabels: {app: zero}}
  template:
    metadata: {labels: {app: zero}}
    spec:
      containers:
        - {name: main, image: "$pause"}
---
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: db}
spec:
  serviceName: db
  replicas: 1
  selector: {matchLabels: {app: db}}
  template:
    metadata: {labels: {app: db}}
    spec:
      containers:
        - name: main
          image: "$pause"
          env: [{name: V, value: "1"}]
---
apiVersion: apps/v1
kind: DaemonSet
metadata: {name: agent}
spec:
  selector: {matchLabels: {app: agent}}
  template:
    metadata: {labels: {app: agent}}
    spec:
      containers:
        - name: main
          image: "$pause"
          env: [{name: V, value: "1"}]
YAML
for w in deployment/web deployment/auto deployment/zero statefulset/db daemonset/agent; do
  kubectl -n "$workloads" rollout status "$w" --timeout 5m
done
say "web, auto, zero, db and agent are up"

# kube-state-metrics has exported every one of them, and the autoscaler.
exported() {
  local n
  n=$(papi "/api/v1/query?query=$(printf '%s' "count({__name__=~\"kube_(deployment_spec_replicas|statefulset_replicas|daemonset_metadata_generation|horizontalpodautoscaler_info)\",namespace=\"$workloads\"})" | jq -sRr @uri)" | jq -r '.data.result[0].value[1] // "0"')
  [ "$n" -ge 6 ]
}
until_true 300 exported

# The period begins on a minute two scrapes on, the first change after four
# minutes more (a quiet span of each workload first, with its window).
from_s=$(((($(now)) + 2 * step_s + 59) / 60 * 60))
from=$(iso "$from_s")
say "the period begins $from"
p1=$((from_s + gap))
p2=$((p1 + gap))
p3=$((p2 + gap))

wait_until "$p1"
a_web_rollout=$(now)
kubectl -n "$workloads" rollout restart deployment/web
a_db_rollout=$(now)
kubectl -n "$workloads" set env statefulset/db V=2
a_agent_rollout=$(now)
kubectl -n "$workloads" set env daemonset/agent V=2
a_auto_scale=$(now)
kubectl -n "$workloads" scale deployment/auto --replicas 2
a_zero_down=$(now)
kubectl -n "$workloads" scale deployment/zero --replicas 0
say "first changes made"

wait_until "$p2"
a_web_scale=$(now)
kubectl -n "$workloads" scale deployment/web --replicas 3
a_zero_up=$(now)
kubectl -n "$workloads" scale deployment/zero --replicas 1
say "second changes made"

wait_until "$p3"
kubectl -n "$workloads" rollout status deployment/web --timeout 5m
a_web_rollback=$(now)
kubectl -n "$workloads" rollout undo deployment/web
say "web rolled back"

# Two evaluations past the last change's window, and a step to be sure.
wait_until $((a_web_rollback + gap))
to_s=$(($(now) / 60 * 60))
to=$(iso "$to_s")

# The probes: a firing over a span around each change made, and over a quiet
# span of each workload (its changes known, none made).
# The probes of a span no change was made in each have a page a person
# acknowledged: such a firing is not useful only where its target's changes
# are known (someone looked, and nothing changed); where they aren't, it is
# not known to be.
probes=$work/firings.jsonl
pages=$work/pages.jsonl
: >"$probes"
: >"$pages"
probe() { # RULE LABEL NAME START END [ack]
  jq -nc --arg rule "$1" --arg label "$2" --arg name "$3" --arg ns "$workloads" --arg s "$(iso "$4")" --arg e "$(iso "$5")" \
    '{rule: $rule, labels: {namespace: $ns, ($label): $name}, start: $s, end: $e}' >>"$probes"
  if [ "${6:-}" = ack ]; then
    jq -nc --arg rule "$1" --arg label "$2" --arg name "$3" --arg ns "$workloads" --arg s "$(iso $(($4 + 10)))" --arg a "$(iso $(($4 + 20)))" \
      '{id: $rule, alerts: [{rule: $rule, labels: {namespace: $ns, ($label): $name}}], sent: $s, acknowledged: $a}' >>"$pages"
  fi
}
around() { probe "$1" "$2" "$3" $(($4 - 30)) $(($4 + 100)) "${5:-}"; }
around web-rollout deployment web "$a_web_rollout"
around web-scale deployment web "$a_web_scale"
around web-rollback deployment web "$a_web_rollback"
around db-rollout statefulset db "$a_db_rollout"
around agent-rollout daemonset agent "$a_agent_rollout"
around auto-scale deployment auto "$a_auto_scale" ack
around zero-down deployment zero "$a_zero_down"
around zero-up deployment zero "$a_zero_up"
for q in "web deployment" "auto deployment" "zero deployment" "db statefulset" "agent daemonset"; do
  read -r name label <<<"$q"
  probe "quiet-$name" "$label" "$name" $((from_s + 30)) $((from_s + 90)) ack
done
jq -s '{responses: .}' "$pages" >"$work/responses.json"
say "$(wc -l <"$probes" | tr -d ' ') probes written"

kubectl -n "$ns" port-forward --address 127.0.0.1 "svc/$svc" :9090 >"$work/pf.log" 2>&1 &
pf=$!
port=
for _ in $(seq 40); do
  port=$(sed -nE 's/^Forwarding from 127\.0\.0\.1:([0-9]+) .*/\1/p' "$work/pf.log" | head -1)
  [ -z "$port" ] || break
  sleep 0.5
done
[ -n "$port" ] || { cat "$work/pf.log"; exit 1; }
pu=http://127.0.0.1:$port
until_true 60 curl -sf "$pu/-/ready"
prom_version=$(curl -sf "$pu/api/v1/status/buildinfo" | jq -r '.data.version')
echo "checking $from to $to, every $step; chart $chart_version, Prometheus $prom_version"

code=0
"$work/lapilli" verdict --firings "$probes" --responses "$work/responses.json" --rollouts --prometheus "$pu" --from "$from" --to "$to" --step "$step" --json \
  >"$work/report.json" 2>"$work/report.err" || code=$?
tail -20 "$work/report.err" || true

fail=0
ok() { echo "ok: $1"; }
failed() { echo "FAILED: $1"; fail=1; }

if [ "$code" = 0 ]; then ok "lapilli verdict --rollouts exited 0"; else failed "lapilli verdict exited $code"; fi

# changed RULE KIND ACTION_TIME: the probe's response rests on a change of
# that kind, at the time of the action or a step and a scrape past it.
changed() {
  if jq -e --arg r "$1" --arg k "$2" --argjson a "$3" --argjson late "$late" --arg w "$workloads" '
      [.firings[]? | select(.rule == $r)] | length == 1 and all(.[];
        .response.kind == "changed" and .response.change.kind == $k
        and .response.change.labels.namespace == $w
        and ((.response.change.at | fromdateiso8601) as $t | $t >= $a - 5 and $t <= $a + $late))' \
    "$work/report.json" >/dev/null; then
    ok "$1: a $2 at the time it was made"
  else
    failed "$1: want a $2 within $late s of $(iso "$3")"
    jq -c --arg r "$1" '.firings[]? | select(.rule == $r) | {rule, response}' "$work/report.json" || true
  fi
}
changed web-rollout rollout "$a_web_rollout"
changed web-scale scale "$a_web_scale"
changed web-rollback rollback "$a_web_rollback"
changed db-rollout rollout "$a_db_rollout"
changed agent-rollout rollout "$a_agent_rollout"
changed zero-down scale "$a_zero_down"
changed zero-up scale "$a_zero_up"

# No change: the page was acknowledged and nothing was changed, and that is
# known (useful is false, not null: its changes known over the firing).
none() {
  if jq -e --arg r "$1" '[.firings[]? | select(.rule == $r)] | length == 1 and all(.[]; .response.kind == "acknowledged" and .response.change == null and .response.useful == false)' "$work/report.json" >/dev/null; then
    ok "$1: no change, known"
  else
    failed "$1: want no change, and its changes known"
    jq -c --arg r "$1" '.firings[]? | select(.rule == $r) | {rule, response}' "$work/report.json" || true
  fi
}
none auto-scale
for w in web auto zero db agent; do none "quiet-$w"; done

# Exactly the changes made: web 3, db 1, agent 1, zero 2; none for auto, and
# no rollback of zero.
changes=$(jq -r '.responses.changes // -1' "$work/report.json")
if [ "$changes" = 7 ]; then ok "7 changes read, those made"; else failed "$changes changes read, want the 7 made"; fi
if jq -e '.responses.autoscalers_known == true' "$work/report.json" >/dev/null; then
  ok "the autoscalers' targets are known"
else
  failed "the autoscalers' targets are not known"
fi
if jq -e '[.firings[]? | select(.rule | startswith("zero-")) | .response.change.kind] | all(. == "scale")' "$work/report.json" >/dev/null; then
  ok "zero scaled to 0 and back, and was never rolled back"
else
  failed "zero was rolled back"
fi

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  {
    echo "### Rollouts, rollbacks and scales read from a real kube-state-metrics"
    echo
    echo "| | |"
    echo "|---|---|"
    echo "| kube-prometheus-stack | $chart_version |"
    echo "| Prometheus | $prom_version |"
    echo "| Period | $from to $to, every $step |"
    echo "| Changes read | $changes (7 made) |"
    echo "| Result | $([ "$fail" = 0 ] && echo passed || echo FAILED) |"
  } >>"$GITHUB_STEP_SUMMARY"
fi
exit "$fail"
