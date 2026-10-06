#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
. ../lib.sh

kubectl apply -f prometheus.yaml
kubectl create namespace media
kubectl -n media create configmap media-code --from-file=code/
kubectl apply -f manifest.yaml
kubectl -n media rollout status deploy/thumb-api --timeout=180s
kubectl -n monitoring rollout status deploy/prometheus --timeout=180s
kubectl apply -f callers.yaml
for d in web-frontend catalog-indexer email-renderer image-proxy; do kubectl -n media rollout status deploy/$d --timeout=180s; done
sleep 150
# an unrelated, recent change to thumb-api (decoy): cache size bumped, which rolls the deployment to revision 2
kubectl -n media set env deploy/thumb-api THUMB_CACHE_SIZE=512
kubectl -n media rollout status deploy/thumb-api --timeout=180s
# let at least three bursts land after the rollout so the periodicity is on record
for i in $(seq 1 90); do
  n=$(kubectl -n media logs deploy/web-frontend --since=10m | grep -c 'thumbnail request failed' || true)
  if [ "$n" -ge 30 ] && [ "$i" -ge 64 ]; then echo "symptom present: $n failed requests logged"; exit 0; fi
  sleep 5
done
echo "symptom did not form"; exit 1
