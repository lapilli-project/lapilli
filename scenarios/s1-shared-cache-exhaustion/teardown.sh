#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
. ../lib.sh

kubectl delete namespace shop --wait=true --timeout=120s
