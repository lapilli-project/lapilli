#!/usr/bin/env bash
# Is the replay right on the rules a real cluster runs? The replay checked
# against what a real Prometheus fired, on a real-world rule set rather than
# a handful written for the test: the default alert and recording rules of
# the kube-prometheus-stack chart, exactly as it ships them (155 alerting
# rules: joins, subqueries, absent(), recording rules read by alerts, `for`
# from 1m to 4h), evaluated by Prometheus on a kind cluster.
#
# (compat.sh checks --check-replay against Thanos and VictoriaMetrics over
# three rules on series made from the time alone. A public demo Prometheus
# was no better a witness: over thirty days only Watchdog fired there, and a
# check of one firing against one says nothing.)
#
# What it does:
#   - one kind cluster, named for the run, with its own kubeconfig in the
#     work directory;
#   - the chart, pinned (CHART below, from its OCI repository), with Grafana
#     and Alertmanager off (Prometheus writes ALERTS without Alertmanager),
#     everything else as it comes: the operator, kube-state-metrics,
#     node-exporter, every default rule, unedited. Scrape and evaluation
#     intervals are set to 30s, which are the chart's own defaults, to be
#     said here and passed to --step;
#   - kind doesn't expose etcd, the scheduler, the controller manager or
#     kube-proxy to Prometheus, so the chart's TargetDown fires on them by
#     itself, and the cluster's one node can't tolerate its own failure, so
#     the overcommit rules usually do too; left as they are;
#   - failures made on purpose, once Prometheus has evaluated every rule:
#       crashy    a Deployment whose container exits 1: KubePodCrashLooping
#                 (for 15m), KubePodNotReady (15m), KubeDeploymentReplicasMismatch
#                 (15m);
#       stuck     a Deployment whose pod can't be scheduled (a node selector
#                 no node has): KubePodNotReady, KubeDeploymentReplicasMismatch;
#       failing   a Job that fails: KubeJobFailed (15m).
#     After they have fired for HOLD minutes, crashy and failing are removed,
#     so those alerts resolve inside the period (a firing with a start and an
#     end); stuck stays, and so do TargetDown's, firing at its end;
#   - then `lapilli verdict --check-replay --loaded-rules`: the rules Prometheus
#     evaluated, read from /api/v1/rules, replayed over the period and set
#     beside ALERTS{alertstate="firing"}.
#
# Where the period starts, and when the failures are made: lapilli replays a
# rule that was in effect before the period from its `for` earlier, so that
# alerts already pending carry in, and over the time before a series' first
# sample absent() is true. A replayed absent() rule is then pending from
# `from - for` to the series' first sample t1, and fires where Prometheus
# didn't only if t1 - (from - for) >= for, that is, only if from <= t1. So
# the period begins after every target's first scrape (e0, with every rule
# group evaluated) and a margin of two minutes. The failures are made only
# after it has begun, so that what they fire is pending, and fires, inside
# the period: --check-replay clips a firing on since before the period to
# start at it on both sides, so one on already at its start would be alike
# for nothing. The test checks that no alert of the failures existed at
# the period's start. It ends two minutes after the last of the removed ones
# has resolved.
#
# Passes only if every firing is alike (nothing only in the replay, nothing
# only in what really fired, but for what --check-replay sets aside as too
# near the end of the period to tell), no rule that really fired went
# unreplayed, no alert really fired that no loaded rule is named, and enough
# really fired for "alike" to mean something: MIN_ALERTS distinct alerts
# besides Watchdog and InfoInhibitor, and each of the failures made on
# purpose. A summary of rules, firings and alike is printed, and added to
# GITHUB_STEP_SUMMARY as a table when that is set, with the chart's and
# Prometheus's versions.
#
# Needs Docker, Go, kubectl, helm, curl and jq. Takes about 40 minutes: ten to set up, two and one before the failures,
# sixteen for them to fire, HOLD_MINUTES, about five to resolve, two more.
# Deletes the cluster afterwards unless KEEP is set. PROMETHEUS_IMAGE runs
# another Prometheus than the chart's own.
# shellcheck disable=SC2016,SC2329 # PromQL and jq, not the shell's; cleanup runs on EXIT
set -euo pipefail
cd "$(dirname "$0")/../.."

# The chart's version and the digest of what was pulled (docs: the chart's
# releases are at https://github.com/prometheus-community/helm-charts).
chart=oci://ghcr.io/prometheus-community/charts/kube-prometheus-stack
chart_version=93.2.0 # sha256:5dd8011a7815c4fae128b93849bd890d694e1830c7a2eca82732a25429cd0fb7
node=kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed
# What the failures run: busybox for a container that exits, pause for a pod
# that is never scheduled, so that nothing is pulled for it.
busybox=busybox:1.37.0@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec
pause=registry.k8s.io/pause:3.10@sha256:ee6521f290b2168b6e0935a181d4cff9be1ac3f505666ef0e3c98fae8199917a

# Distinct alerts that must have really fired, besides Watchdog (always) and
# InfoInhibitor, for the check to mean something: the failures made on
# purpose fire three and TargetDown a fourth; five is those and the
# overcommit alerts or any other, and well under what fires in practice.
# Fewer would let a check of a handful of firings pass as one of a rule set.
min_alerts=${MIN_ALERTS:-5}
# The alerts the failures must fire, each by name.
induced=(KubePodCrashLooping KubeDeploymentReplicasMismatch KubeJobFailed)
# Minutes the failures fire before crashy and failing are removed.
hold=${HOLD_MINUTES:-3}
step=30s
every=30 # seconds, as step

cluster=lapilli-k8s-rules-$$
ns=monitoring
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
    kubectl -n "$ns" describe pod -l app.kubernetes.io/name=prometheus || true
    kubectl -n "$ns" logs "prometheus-$svc-0" -c prometheus --tail=100 || true
    kubectl -n induced get all || true
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
# until_true SECONDS COMMAND...: run it until it succeeds.
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

# PROMETHEUS_IMAGE, as REGISTRY/REPOSITORY[:TAG][@DIGEST], as the chart's
# values take it: registry, repository, tag and sha.
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

# The chart's default rules, untouched. Only what the test has no use for is
# off, and the intervals are said.
helm upgrade --install "$release" "$chart" --version "$chart_version" -n "$ns" --create-namespace \
  --set fullnameOverride="$release" \
  --set grafana.enabled=false --set alertmanager.enabled=false \
  --set prometheus.prometheusSpec.scrapeInterval="$step" --set prometheus.prometheusSpec.evaluationInterval="$step" \
  --set prometheus.prometheusSpec.retention=6h \
  ${prom_values[@]+"${prom_values[@]}"} \
  --wait --timeout 10m

papi() { kubectl get --raw "/api/v1/namespaces/$ns/services/http:$svc:http-web/proxy$1"; }
# Every rule group has been evaluated: from then on, what Prometheus
# evaluates, it evaluates every $step.
evaluated() {
  papi /api/v1/rules | jq -e '(.data.groups | length) > 0 and all(.data.groups[]; .lastEvaluation != "0001-01-01T00:00:00Z")'
}
# And every active target has been scraped, a down one too (it writes up 0).
scraped() {
  papi /api/v1/targets | jq -e '(.data.activeTargets | length) > 0 and all(.data.activeTargets[]; .lastScrape != "0001-01-01T00:00:00Z")'
}
until_true 600 evaluated
until_true 600 scraped
e0=$(now)
say "every rule group evaluated and every target scraped"

# The period begins two minutes on, on a minute; the failures, once it has.
from_s=$(((e0 + 120 + 59) / 60 * 60))
from=$(iso "$from_s")
say "the period begins $from"
until [ "$(now)" -ge $((from_s + every)) ]; do sleep 5; done

# The failures: they take their `for` (15m) to fire.
kubectl create namespace induced
kubectl apply -n induced -f - <<YAML
apiVersion: apps/v1
kind: Deployment
metadata: {name: crashy}
spec:
  replicas: 1
  selector: {matchLabels: {app: crashy}}
  template:
    metadata: {labels: {app: crashy}}
    spec:
      containers:
        - {name: main, image: "$busybox", command: [sh, -c, "exit 1"]}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: stuck}
spec:
  replicas: 1
  selector: {matchLabels: {app: stuck}}
  template:
    metadata: {labels: {app: stuck}}
    spec:
      nodeSelector: {lapilli.dev/no-such-node: "true"}
      containers:
        - {name: main, image: "$pause"}
---
apiVersion: batch/v1
kind: Job
metadata: {name: failing}
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - {name: main, image: "$busybox", command: [sh, -c, "exit 1"]}
YAML
say "crashy, stuck and failing made"

# firing NAME: whether an alert of that name is firing in the failures' namespace.
firing() {
  papi /api/v1/alerts | jq -e --arg n "$1" '[.data.alerts[] | select(.labels.alertname == $n and .labels.namespace == "induced" and .state == "firing")] | length > 0'
}
quiet() { # no alert of the failures' namespace, of these, left (pending or firing)
  papi /api/v1/alerts | jq -e --argjson names "$(printf '%s\n' "$@" | jq -R . | jq -sc .)" \
    '[.data.alerts[] | select(.labels.namespace == "induced" and (.labels.alertname | IN($names[])))] | length == 0'
}
for a in "${induced[@]}"; do
  until_true 1800 firing "$a"
  say "$a fires"
done
sleep $((hold * 60))
kubectl -n induced delete deployment crashy
kubectl -n induced delete job failing
say "crashy and failing removed"
# KubePodCrashLooping looks back five minutes, so it resolves that much later.
until_true 900 quiet KubePodCrashLooping KubeJobFailed
say "KubePodCrashLooping and KubeJobFailed resolved"
# Two evaluations more, and what the period ends on has been evaluated.
sleep 120
to_s=$(($(now) / 60 * 60))
to=$(iso "$to_s")

# Prometheus, through a port-forward on a free port.
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
"$work/lapilli" verdict --check-replay --json --loaded-rules --prometheus "$pu" --from "$from" --to "$to" --step "$step" \
  >"$work/check.json" 2>"$work/check.err" || code=$?
tail -20 "$work/check.err" || true

# What Prometheus loaded, and what the check found.
loaded=$(curl -sf "$pu/api/v1/rules" | jq -c '[.data.groups[].rules[]] | {rules: length, alerting: (map(select(.type == "alerting")) | length)}')
rules_loaded=$(jq .rules <<<"$loaded")
alerting=$(jq .alerting <<<"$loaded")
replayed=$(jq -r '.replayed // 0' "$work/check.json")
real=$(jq -r '.real // 0' "$work/check.json")
alike=$(jq -r '.alike // 0' "$work/check.json")
differing=$((replayed + real - 2 * alike))
fired=$(jq -r '[.rules[]? | select(.real > 0) | .rule] | unique | length' "$work/check.json")
names=$(jq -r '[.rules[]? | select(.real > 0 and (.rule | IN("Watchdog", "InfoInhibitor") | not)) | .rule] | unique | sort | join(", ")' "$work/check.json")
distinct=$(jq -r '[.rules[]? | select(.real > 0 and (.rule | IN("Watchdog", "InfoInhibitor") | not)) | .rule] | unique | length' "$work/check.json")
unreplayed=$(jq -r '[.rules[]? | select(.unsupported != null and .unsupported != "")] | length' "$work/check.json")
unreplayed_fired=$(jq -r '[.rules[]? | select(.unsupported != null and .unsupported != "" and .real > 0) | .rule] | join(", ")' "$work/check.json")
elsewhere=$(jq -r '(.elsewhere // []) | join(", ")' "$work/check.json")

echo "rules loaded: $rules_loaded, alerting rules: $alerting, alerts that fired: $fired ($distinct besides Watchdog and InfoInhibitor)"
echo "firings: $real really fired, $replayed replayed, $alike alike, $differing differing; $unreplayed rules not replayed"
echo "fired: $names"

fail=0
ok() { echo "ok: $1"; }
failed() { echo "FAILED: $1"; fail=1; }

if [ "$code" = 0 ]; then ok "lapilli verdict --check-replay --loaded-rules exited 0"; else failed "lapilli verdict exited $code"; fi
if [ "$replayed" -gt 0 ] && [ "$replayed" = "$real" ] && [ "$alike" = "$real" ]; then
  ok "every firing alike: $alike of $real"
else
  failed "firings: $real really fired, $replayed replayed, $alike alike"
  jq -c '.rules[]? | select(.alike != .replayed or .alike != .real)' "$work/check.json" || true
fi
if [ -z "$unreplayed_fired" ]; then
  ok "no rule that really fired went unreplayed ($unreplayed not replayed in all)"
else
  failed "rules that really fired but couldn't be replayed: $unreplayed_fired"
  jq -c '.rules[]? | select(.unsupported != null and .unsupported != "")' "$work/check.json" || true
fi
if [ -z "$elsewhere" ]; then ok "every alert that really fired is named by a rule loaded"; else failed "fired, named by no rule loaded: $elsewhere"; fi
if [ "$distinct" -ge "$min_alerts" ]; then
  ok "$distinct distinct alerts fired besides Watchdog and InfoInhibitor, at least $min_alerts"
else
  failed "$distinct distinct alerts fired besides Watchdog and InfoInhibitor, want at least $min_alerts"
fi
for a in "${induced[@]}"; do
  if jq -e --arg a "$a" '[.rules[]? | select(.rule == $a and .real > 0)] | length == 1' "$work/check.json" >/dev/null; then
    ok "$a really fired"
  else
    failed "$a, made to fire, didn't"
  fi
done
# None of the failures' alerts existed at the period's start: what they
# fired started inside it, and was compared by its start.
early=$(curl -sf "$pu/api/v1/query" --data-urlencode 'query=count(ALERTS{namespace="induced"})' --data-urlencode "time=$from_s" |
  jq -r '.data.result[0].value[1] // "0"')
if [ "$early" = 0 ]; then
  ok "no alert of the failures existed at the period's start"
else
  failed "$early alert series of the failures existed at $from, before the period: their starts were clipped to it, not compared"
fi
# A firing with a start and an end among them: the failures removed resolved.
if [ "$alike" -gt 0 ]; then
  ended=$(curl -sf "$pu/api/v1/query" --data-urlencode "query=count(count_over_time(ALERTS{alertstate=\"firing\",namespace=\"induced\"}[$((to_s - from_s))s]) unless ALERTS{alertstate=\"firing\",namespace=\"induced\"})" \
    --data-urlencode "time=$to" | jq -r '.data.result[0].value[1] // "0"')
  if [ "$ended" -gt 0 ]; then ok "$ended alerts of the failures fired and resolved within the period"; else failed "no alert of the failures resolved within the period"; fi
fi

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  {
    echo "### Kubernetes's default rules, replayed beside what Prometheus fired"
    echo
    echo "| | |"
    echo "|---|---|"
    echo "| kube-prometheus-stack | $chart_version |"
    echo "| Prometheus | $prom_version |"
    echo "| Period | $from to $to, every $step |"
    echo "| Rules loaded | $rules_loaded |"
    echo "| Alerting rules | $alerting |"
    echo "| Rules that fired | $fired ($distinct besides Watchdog and InfoInhibitor) |"
    echo "| Firings that really happened | $real |"
    echo "| Alike in the replay | $alike |"
    echo "| Differing | $differing |"
    echo "| Alerts that fired | ${names:-none} |"
    echo "| Result | $([ "$fail" = 0 ] && echo passed || echo FAILED) |"
  } >>"$GITHUB_STEP_SUMMARY"
fi
exit "$fail"
