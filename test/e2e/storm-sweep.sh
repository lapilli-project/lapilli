#!/usr/bin/env bash
# Sweep one variable across repeated storms and print the table.
#
# `storm.sh` runs ONE storm against the cluster as it is. It is the gate's assertion harness and it
# deliberately changes nothing. This script is the measurement tool: it varies a setting, gives each
# value a fresh controller pod, and prints a row per value. The two tables in
# `docs/design-trigger-and-load.md` §3 were produced with it, and without it "reproducible" was a
# word the document could not back — the concurrency rows need a `--set` and a pod roll that
# `storm.sh` does not do.
#
#   test/e2e/storm-sweep.sh size 20 50 100      # cost against the size of the storm
#   test/e2e/storm-sweep.sh conc 0 1 2 8        # cost against reconcileConcurrency, at N=20
#
#   N=50 test/e2e/storm-sweep.sh conc 2 8       # size for the conc sweep (default 20)
#   CONC=2 test/e2e/storm-sweep.sh size 20 50   # concurrency for the size sweep (default 2)
#   REPEATS=3 test/e2e/storm-sweep.sh conc 2    # run each value N times; the spread is the point
#
# THIS MUTATES THE RELEASE. It runs `helm upgrade` per row and leaves the last value in place. Do
# not run it against anything you care about, and do not run it from the release gate.
#
# Reading the output: every row is `REPEATS` runs, and with REPEATS=1 a row is a single sample. The
# peak is the kernel's `memory.peak`, which cannot be reset on kernels below 6.8 — hence the fresh
# pod — and still carries process start and the informer's first list. `storm.sh` prints the growth
# over the baseline it reads; that is the number to compare across rows, not the raw peak.
set -uo pipefail
cd "$(dirname "$0")/../.."

AXIS=${1:-}; shift 2>/dev/null || true
case "$AXIS" in
  size|conc) ;;
  *) echo "usage: $0 {size|conc} VALUE...   (see the comments at the top)"; exit 64 ;;
esac
[ $# -gt 0 ] || { echo "$0: give at least one value to sweep"; exit 64; }

KNS=${KNS:-lapilli-system}
CLUSTER=${CLUSTER:-lapilli}
REPEATS=${REPEATS:-1}
N=${N:-20}
CONC=${CONC:-2}

export KUBECONFIG="${KUBECONFIG:-$(mktemp)}"
kind export kubeconfig --name "$CLUSTER" --kubeconfig "$KUBECONFIG" >/dev/null 2>&1 || true

printf '%8s | %4s | %6s | %10s | %10s | %8s | %7s | %6s | %8s | %s\n' \
  "alerts" "conc" "actual" "baseMiB" "peakMiB" "growthMiB" "%limit" "CPUs" "finished" "restarts"
printf '%8s-+-%4s-+-%6s-+-%10s-+-%10s-+-%8s-+-%7s-+-%6s-+-%8s-+-%s\n' \
  "--------" "----" "------" "----------" "----------" "--------" "-------" "------" "--------" "--------"

for V in "$@"; do
  case "$AXIS" in
    size) ROW_N=$V; ROW_C=$CONC ;;
    conc) ROW_N=$N; ROW_C=$V ;;
  esac
  for _ in $(seq 1 "$REPEATS"); do
    kubectl -n "$KNS" delete incidentcapture --all >/dev/null 2>&1
    kubectl delete ns storm-e2e --wait=true --timeout=300s >/dev/null 2>&1
    # Force a fresh pod even when no chart value changed. memory.peak is monotonic on kernels below
    # 6.8, so a reused pod reports the PREVIOUS row's peak as this row's — which is how a sweep once
    # reported a 99 MiB "idle" for a controller that had just been started.
    kubectl -n "$KNS" patch deploy lapilli --type=merge \
      -p "{\"spec\":{\"template\":{\"metadata\":{\"annotations\":{\"lapilli.dev/sweep\":\"$AXIS-$V-$RANDOM\"}}}}}" \
      >/dev/null 2>&1
    helm upgrade lapilli charts/lapilli -n "$KNS" --reuse-values \
      --set reconcileConcurrency="$ROW_C" --wait --timeout 300s >/dev/null 2>&1 || true
    kubectl -n "$KNS" rollout status deploy/lapilli --timeout=240s >/dev/null 2>&1
    sleep 5

    # What the process says it is running, not what we asked for. A previous sweep left
    # `reconcileConcurrency: 1` in the release and three later runs measured 1 while reporting 2.
    ACTUAL=$(kubectl -n "$KNS" logs deploy/lapilli --tail=300 2>/dev/null \
      | grep -oE 'reconcile concurrency concurrency=[0-9]+' | tail -1 | grep -oE '[0-9]+$')
    : "${ACTUAL:=?}"

    OUT=$(CLUSTER="$CLUSTER" test/e2e/storm.sh "$ROW_N" 2>&1)
    BASE=$(grep -oE 'cgroup peak base → PEAK  [0-9]+' <<<"$OUT" | grep -oE '[0-9]+$')
    PEAK=$(grep -oE 'cgroup peak base → PEAK  [0-9]+ → [0-9]+' <<<"$OUT" | awk '{print $NF}')
    GROW=$(grep -oE 'GROWTH over the base *-?[0-9]+' <<<"$OUT" | awk '{print $NF}')
    PCT=$(grep -oE 'PEAK as % of limit *[0-9.]+%' <<<"$OUT" | grep -oE '[0-9.]+%')
    CPU=$(grep -oE 'CPU seconds consumed *[0-9.]+' <<<"$OUT" | awk '{print $NF}')
    RST=$(grep -oE 'controller restarts *[0-9?]+' <<<"$OUT" | awk '{print $NF}')
    FIN=$(grep -oE 'captures finished *[0-9]+ of [0-9]+' <<<"$OUT" | sed 's/captures finished *//')
    if [ -z "${PEAK:-}" ]; then
      printf '%8s | %4s | %6s | the storm produced no kernel peak:\n' "$ROW_N" "$ROW_C" "$ACTUAL"
      grep -E "^FAIL|NOTE|WARNING" <<<"$OUT" | head -3 | sed 's/^/           /'
      continue
    fi
    mib() { python3 -c "print(f'{$1/1048576:.1f}')" 2>/dev/null || echo '?'; }
    printf '%8s | %4s | %6s | %10s | %10s | %8s | %7s | %6s | %8s | %s\n' \
      "$ROW_N" "$ROW_C" "$ACTUAL" "$(mib "$BASE")" "$(mib "$PEAK")" "$(mib "$GROW")" \
      "$PCT" "$CPU" "$FIN" "$RST"
    # A row that failed its own assertions must not read as a result.
    grep -E "^FAIL|^  WARNING" <<<"$OUT" | head -2 | sed 's/^/           /'
  done
done
echo "SWEEP DONE — every row above is $REPEATS run(s); with 1, adjacent rows are not distinguishable."
