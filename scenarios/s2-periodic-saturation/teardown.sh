#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
. ../lib.sh

kubectl delete namespace media monitoring --wait=true --timeout=180s
