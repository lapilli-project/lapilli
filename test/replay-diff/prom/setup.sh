#!/usr/bin/env bash
# A Prometheus with a past, a histogram, rules and more than one target. See prometheus.yaml.
#   PROM_IMAGE=prom/prometheus:v3.5.0 setup.sh    another version of Prometheus than the engine's own
set -euo pipefail
cd "$(dirname "$0")"
. ../../../scenarios/lib.sh
IMAGE="${PROM_IMAGE:-prom/prometheus:v3.15.0}"

kubectl create namespace promfix
kubectl create namespace monitoring
kubectl -n promfix create configmap fixture-code --from-file=code/
kubectl -n monitoring create configmap fixture-code --from-file=code/
kubectl apply -f manifest.yaml
for d in exp-a exp-b exp-c; do kubectl -n promfix rollout status deploy/$d --timeout=180s; done
# The targets are up before the Prometheus is, so that its head begins with them.
sed "s|prom/prometheus:v3.15.0|$IMAGE|g" prometheus.yaml | kubectl apply -f -
kubectl -n monitoring rollout status deploy/prometheus --timeout=300s

# What the fixture is for has to be there before anything is asked of it: the past in blocks, every
# target seen, the one that is gone seen to be gone, the rules evaluated, a series that has come
# and gone again since the head began — and a head older than the furthest an instant asked of it
# looks. A sweep asks of the freeze and of a minute and a half before it, and an instant looks back
# five minutes: of a head seven minutes old such a question is answered from the head alone, in the
# head's order. Of a younger one it reaches the blocks and is answered by label, as every other is.
ask() { kubectl -n monitoring exec deploy/prometheus -- wget -qO- "http://localhost:9090/api/v1/query?query=$1" 2>/dev/null; }
# What it answered is read whole before it is looked at: a reader that stops at the first match ends
# the asking, which then reads as no answer. And an asking that failed has nothing and is not without.
has() { local got; got="$(ask "$1")" || return 1; case "$got" in *'"result":[{'*) return 0 ;; esac; return 1; }
without() { local got; got="$(ask "$1")" || return 1; case "$got" in *'"result":[]'*) return 0 ;; esac; return 1; }
# This Prometheus scrapes itself, and counts its own answers by their status. A sweep asks it things it
# refuses, after the freeze; the first refusal of a kind makes a series, and a label's values are a
# head's whenever its series were made. So it is refused once of each kind before — a query that does
# not parse, and one that cannot be evaluated — and again until it has scraped itself having done so.
for i in $(seq 1 150); do
  ask 'sum(' >/dev/null || true
  ask 'up%20*%20on%20(job)%20up' >/dev/null || true
  if has 'count(up%7Bjob%3D~%22fix%7Cother%22%7D%20%3D%3D%201)%20%3D%3D%203' && has 'up%7Bjob%3D%22gone%22%7D%20%3D%3D%200' \
     && has 'count_over_time(up%7Bjob%3D%22fix%22%7D%5B1h%5D%20offset%201h)%20%3E%20100' && has 'prometheus_tsdb_blocks_loaded%20%3E%200' \
     && has 'count_over_time(up%7Binstance%3D~%22exp-d.%2B%22%7D%5B1h%5D)%20%3E%2010' && without 'up%7Binstance%3D~%22exp-d.%2B%22%7D' \
     && has 'ALERTS%7Balertname%3D%22FixWarm%22%2Calertstate%3D%22firing%22%7D' && has 'fix%3Arequests%3Arate1m' \
     && has 'changes(count(fix_job_running%20or%20vector(0))%5B4m%3A5s%5D)%20%3E%3D%202' \
     && has 'prometheus_http_requests_total%7Bcode%3D%22400%22%7D' && has 'prometheus_http_requests_total%7Bcode%3D%22422%22%7D' \
     && has 'time()%20-%20max(prometheus_tsdb_head_min_time_seconds)%20%3E%20420'; then
    echo "the fixture's Prometheus holds what it is for, $SECONDS seconds after its setup began"; exit 0
  fi
  sleep 5
done
echo "the fixture's Prometheus does not hold what it is for"; ask 'up'; exit 1
