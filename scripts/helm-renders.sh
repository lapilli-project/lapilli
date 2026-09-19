#!/usr/bin/env bash
# Chart lint and the supported value combinations. Used by CI (helm-lint) and
# scripts/release-check.sh, so both check the same list.
set -euo pipefail
cd "$(dirname "$0")/.."
helm lint charts/kairn >/dev/null
helm template kairn charts/kairn >/dev/null
helm template kairn charts/kairn --set watchNamespaces='{a,b}' >/dev/null
helm template kairn charts/kairn --set signing.mode=static --set signing.keySecret=k >/dev/null
helm template kairn charts/kairn --set persistence.enabled=false >/dev/null
helm template kairn charts/kairn --set metrics.prometheusUrl=http://prom:9090 | grep -q -- '- metrics'
helm template kairn charts/kairn --set diffs.configMaps=true --set 'watchNamespaces={shop}' | grep -q configmaps
helm template kairn charts/kairn --set clusterId=prod-1 --set-json \
  'export.destinations=[{"name":"e","url":"s3://bucket/p"}]' | grep -q destinations.json
# the schema rejects typos and bad values
! helm template kairn charts/kairn --set profile.colectors=x >/dev/null 2>&1
! helm template kairn charts/kairn --set redaction.mode=loose >/dev/null 2>&1
# ConfigMap diffs without a namespace list, or with kube-system, must be refused
! helm template kairn charts/kairn --set diffs.configMaps=true >/dev/null 2>&1
! helm template kairn charts/kairn --set diffs.configMaps=true --set 'watchNamespaces={kube-system}' >/dev/null 2>&1
# static signing without a key Secret must be refused
! helm template kairn charts/kairn --set signing.mode=static >/dev/null 2>&1
# the cluster id is part of every incident id: path-safe, at most 83 characters
! helm template kairn charts/kairn --set clusterId=arn:aws:eks:x:cluster/prod >/dev/null 2>&1
# export destinations need a real, path-safe cluster id
! helm template kairn charts/kairn --set-json 'export.destinations=[{"name":"e","url":"s3://bucket/p"}]' >/dev/null 2>&1
! helm template kairn charts/kairn --set clusterId=arn:x:cluster/prod --set-json \
  'export.destinations=[{"name":"e","url":"s3://bucket/p"}]' >/dev/null 2>&1
echo "helm: lint and renders OK"
