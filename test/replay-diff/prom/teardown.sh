#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
. ../../../scenarios/lib.sh

kubectl delete namespace promfix monitoring --ignore-not-found --wait=true --timeout=180s || true
