#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
. ../lib.sh

kubectl create namespace shop
kubectl -n shop create configmap shop-code --from-file=code/
kubectl apply -f manifest.yaml
kubectl -n shop rollout status deploy/cache --timeout=180s
# an unrelated, recent change to the cache (decoy): log level flipped, which rolls the cache to revision 2
kubectl -n shop set env deploy/cache LOG_LEVEL=debug
kubectl -n shop rollout status deploy/cache --timeout=180s
kubectl apply -f clients.yaml
for d in checkout-api inventory-sync report-worker; do kubectl -n shop rollout status deploy/$d --timeout=180s; done
for i in $(seq 1 60); do
  if kubectl -n shop logs deploy/cache --tail=3 | grep -q 'active=40/40' && kubectl -n shop logs deploy/checkout-api --tail=20 | grep -q 'timed out'; then echo "symptom present after ${i}x5s"; exit 0; fi
  sleep 5
done
echo "symptom did not form"; exit 1
