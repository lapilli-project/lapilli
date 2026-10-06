#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
. ../lib.sh

kubectl delete namespace ledger infra-agents --wait=true --timeout=180s
for n in rcabench-worker rcabench-worker2; do docker exec "$n" sh -c 'rm -rf /etc/ledger-tuning'; done
