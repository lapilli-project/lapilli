#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
. ../../../scenarios/lib.sh

# The two pods setup.sh left unable to go would hold their namespace with them: one by a finalizer,
# one by the hour it was given to stop.
kubectl -n kinds patch pod held --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
kubectl -n kinds delete pod terminating held --grace-period=0 --force --ignore-not-found >/dev/null 2>&1 || true
kubectl delete -f late.yaml --ignore-not-found --wait=false >/dev/null 2>&1 || true
kubectl delete -f manifest.yaml --ignore-not-found --wait=false >/dev/null 2>&1 || true
kubectl delete namespace kinds kinds-db --ignore-not-found --wait=true --timeout=180s || true
kubectl delete -f crds.yaml --ignore-not-found --wait=true --timeout=60s || true
# What the fixture's claims were given, and its own volume; no other volume in the cluster is touched.
for pv in $(kubectl get pv -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.claimRef.namespace}{"\n"}{end}' 2>/dev/null | awk '$1 == "fixture-pv" || $2 == "kinds" || $2 == "kinds-db" {print $1}'); do
  kubectl delete pv "$pv" --ignore-not-found --wait=false >/dev/null 2>&1 || true
done
