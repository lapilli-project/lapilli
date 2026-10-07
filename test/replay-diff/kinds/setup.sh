#!/usr/bin/env bash
# One of every kind, and a pod in every state. See manifest.yaml.
set -euo pipefail
cd "$(dirname "$0")"
. ../../../scenarios/lib.sh

kubectl create namespace kinds
kubectl create namespace kinds-db
kubectl apply -f crds.yaml
kubectl wait --for=condition=Established --timeout=60s crd/widgets.fixtures.lapilli.dev crd/gadgets.fixtures.lapilli.dev crd/services.fixtures.lapilli.dev
kubectl apply -f manifest.yaml
SECONDS=0   # counted from here: the kubelet's back-off is counted from a pod's first failure

kubectl -n kinds rollout status deploy/web --timeout=240s
kubectl -n kinds rollout status statefulset/db --timeout=240s
kubectl -n kinds rollout status ds/agent --timeout=240s
kubectl -n kinds wait --for=condition=complete --timeout=180s job/migrate job/batch
kubectl -n kinds wait --for=condition=failed --timeout=180s job/broken
kubectl -n kinds wait --for=condition=Ready --timeout=180s pod/sidecar pod/terminating pod/held
kubectl -n kinds wait --for=jsonpath='{.status.phase}'=Succeeded --timeout=120s pod/done
kubectl -n kinds wait --for=jsonpath='{.status.phase}'=Failed --timeout=120s pod/failed

# Two pods that are deleted and do not go (manifest.yaml says why each).
kubectl -n kinds delete pod terminating held --wait=false

# A CronJob that has run once, and is then told to stop: the cluster should not go on making Jobs
# while it is being asked.
for i in $(seq 1 90); do
  [ -n "$(kubectl -n kinds get cronjob ran -o jsonpath='{.status.lastScheduleTime}' 2>/dev/null)" ] && break
  sleep 2
done
kubectl -n kinds patch cronjob ran --type=merge -p '{"spec":{"suspend":true}}'
for i in $(seq 1 60); do
  [ -z "$(kubectl -n kinds get cronjob ran -o jsonpath='{.status.active}' 2>/dev/null)" ] && break
  sleep 2
done

kubectl apply -f late.yaml

# What fails here fails again and again, each time after a longer wait — ten seconds, twenty, forty,
# to five minutes — and the cluster is handed over when the next minutes are ones in which none of it
# changes, because it is asked three times and the three answers are compared.
#
# A container that crashes is waited for until it has been started five times and the kubelet has
# written it down as waiting to be started again: between its end and that, a listing says Error and
# then CrashLoopBackOff with nothing else having happened, and for an init container that takes the
# kubelet a minute.
#
# The image that cannot be pulled is harder, because what it does cannot be seen in a listing:
# ErrImagePull for some seconds at each try, ImagePullBackOff until the next, and no count that moves.
# A try that falls between two askings leaves both live answers the same and the frozen one, taken in
# the middle, different. Its events do not time it either — past twenty-five of them the kubelet's
# are dropped. So the tries are watched for, and the cluster is handed over after one that came at
# least 150 seconds after the one before it: the next is at least as far off.
tried=0; before=0; was=""
for i in $(seq 1 450); do
  crash=$(kubectl -n kinds get pod crash -o jsonpath='{.status.containerStatuses[0].restartCount} {.status.containerStatuses[0].state.waiting.reason}' 2>/dev/null || true)
  oom=$(kubectl -n kinds get pod oom -o jsonpath='{.status.containerStatuses[0].restartCount} {.status.containerStatuses[0].state.waiting.reason}' 2>/dev/null || true)
  init=$(kubectl -n kinds get pod init-fail -o jsonpath='{.status.initContainerStatuses[0].restartCount} {.status.initContainerStatuses[0].state.waiting.reason}' 2>/dev/null || true)
  waiting=$(kubectl -n kinds get pod bad-image -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}' 2>/dev/null || true)
  if [ "$waiting" != "$was" ]; then
    echo "  ${SECONDS}s: bad-image is ${waiting:-not yet waiting}"
    if [ "$waiting" = ErrImagePull ]; then before=$tried; tried=$SECONDS; fi
  fi
  was="$waiting"
  still=yes
  for pod in "$crash" "$oom" "$init"; do
    count="${pod%% *}"
    [ "${count:-0}" -ge 5 ] && [ "${pod#* }" = CrashLoopBackOff ] || still=no
  done
  if [ "$still" = yes ] && [ "$before" -gt 0 ] && [ $((tried - before)) -ge 150 ] && [ "$waiting" = ImagePullBackOff ]; then
    echo "fixture built after ${SECONDS}s"; kubectl -n kinds get pods -o wide; exit 0
  fi
  sleep 2
done
echo "the failing pods did not come to rest in time: crash [$crash] oom [$oom] init-fail [$init] bad-image [$waiting, tries at ${before}s and ${tried}s]"; kubectl -n kinds get pods; exit 1
