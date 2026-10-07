#!/usr/bin/env bash
# The same command against a cluster and against its frozen copy, output compared.
#   LAPILLI_CASE=<binary> [KIND=…] [LAPILLI_CRUST_GATHER=…] sweep.sh <repo> <out> [case...]
# Per case: build the scenario on a kind cluster, ask every command of the live cluster, freeze it,
# serve the frozen copy and ask them of that, and ask them of the live cluster again. Three answers a
# command, within half a minute: the two live ones say whether the cluster itself moved in between.
# The frozen copy is asked while the cluster still stands, because `kubectl describe` counts its ages
# from the wall clock, and a frozen answer taken a minute later is a minute older for no other reason.
#
# Everything that touches the cluster goes through one kubeconfig written by kind for this run and
# named on every call, and through the guard, which refuses anything that is not a read. The default
# kubeconfig is never read.
#
# It exits 0 only if every case was built, asked three times and compared, and nothing differed that
# known.txt does not excuse.
set -uo pipefail
L="$1"; OUT="$2"; shift 2
CASES=("$@"); [ ${#CASES[@]} -eq 0 ] && CASES=(s1-shared-cache-exhaustion s2-periodic-saturation s3-node-local-drift)
BIN="${LAPILLI_CASE:?set LAPILLI_CASE to the lapilli-case binary under test}"
KIND="${KIND:-kind}"; KC="$OUT/rcabench.kubeconfig"; HERE="$(cd "$(dirname "$0")" && pwd)"
export LAPILLI_CRUST_GATHER="${LAPILLI_CRUST_GATHER:-kubectl-crust-gather}"
RECORDED="$L/test/fixtures/case-runs/2026-10-07-round38"
mkdir -p "$OUT/log"
say() { echo "[$(date -u +%H:%M:%S)] $*" | tee -a "$OUT/log/sweep.log"; }
real_kubectl="$(command -v kubectl)"
tool() { python3 "$HERE/replaydiff.py" "$@" 2>&1 | tee -a "$OUT/log/sweep.log"; return "${PIPESTATUS[0]}"; }

DEFAULT_BEFORE=$(shasum -a 256 "$HOME/.kube/config" 2>/dev/null | cut -c1-16)
PF=""; SERVE=""; CLUSTER=""
cleanup() {   # however this ends: nothing left listening, and no cluster left behind
  [ -n "$PF" ] && kill "$PF" 2>/dev/null
  [ -n "$SERVE" ] && kill "$SERVE" 2>/dev/null
  [ -n "$CLUSTER" ] && "$KIND" delete cluster --name rcabench --kubeconfig "$KC" >> "$OUT/log/kind.log" 2>&1
  CLUSTER=""
}
trap cleanup EXIT
trap 'exit 130' INT TERM

rm -f "$KC"
"$KIND" create cluster --name rcabench --config "$L/scenarios/kind.yaml" --kubeconfig "$KC" >> "$OUT/log/kind.log" 2>&1 || { say "ABORT: kind could not create the cluster"; exit 1; }
CLUSTER=yes
[ "$(command kubectl --kubeconfig "$KC" config current-context)" = "kind-rcabench" ] || { say "ABORT: wrong context"; exit 1; }

# The guard, in front of the live cluster: what an agent's kubectl is in a live run.
mkdir -p "$OUT/live-bin"
printf '#!/bin/sh\nexec "%s" guard-kubectl "%s" "%s" "$@"\n' "$BIN" "$KC" "$real_kubectl" > "$OUT/live-bin/kubectl"; chmod +x "$OUT/live-bin/kubectl"

status=0; compared=0
for case in "${CASES[@]}"; do
  D="$OUT/$case"; mkdir -p "$D"
  say "=== $case: setup"
  # A scenario can fail to form: s3 needs the scheduler to put half its pods on one node. Once more,
  # then, from nothing — and after that it is a failure, not something to sweep around.
  if ! ( export KUBECONFIG="$KC"; "$L/scenarios/$case/setup.sh" ) >> "$OUT/log/$case-setup.log" 2>&1; then
    say "$case did not form its symptom; torn down and built once more"
    ( export KUBECONFIG="$KC"; "$L/scenarios/$case/teardown.sh" ) >> "$OUT/log/$case-setup.log" 2>&1
    ( export KUBECONFIG="$KC"; "$L/scenarios/$case/setup.sh" ) >> "$OUT/log/$case-setup.log" 2>&1 || { say "ABORT: the scenario did not form its symptom, twice"; status=1; break; }
  fi
  FREEZE=()
  if [ "$case" = s2-periodic-saturation ]; then   # its key names evidence in the metrics store, so the freeze needs the store
    command kubectl --kubeconfig "$KC" --context kind-rcabench -n monitoring port-forward svc/prometheus 19392:9090 >/dev/null 2>&1 & PF=$!
    for i in $(seq 1 50); do curl -fs http://127.0.0.1:19392/-/ready >/dev/null 2>&1 && break; perl -e 'select(undef,undef,undef,0.2)'; done
    FREEZE=(--metrics-url http://127.0.0.1:19392 --metrics-window 30m)
  fi
  tool commands "$KC" "$D/commands.json" --runs "$RECORDED/$case" --old-snapshot "$RECORDED/frozen-cases/$case/kubernetes.tar.gz" || { say "ABORT: nothing to ask"; status=1; break; }
  say "=== $case: live, before the freeze"
  tool capture "$D/commands.json" "$OUT/live-bin" "$KC" "$D/live-before.json" || { say "ABORT: the live cluster was not asked"; status=1; break; }
  say "=== $case: freeze"
  rm -rf "$D/frozen"
  "$BIN" freeze "$L/cases/$case/case.yaml" -o "$D/frozen" --kubeconfig "$KC" ${FREEZE[@]+"${FREEZE[@]}"} > "$D/freeze.json" 2>> "$OUT/log/sweep.log" || { say "ABORT: freeze failed"; status=1; break; }
  [ -n "$PF" ] && kill "$PF" 2>/dev/null; PF=""
  say "=== $case: frozen"
  "$BIN" serve "$D/frozen" > "$D/serve.out" 2> "$D/serve.err" & SERVE=$!
  for i in $(seq 1 150); do grep -q '^export PATH=' "$D/serve.out" 2>/dev/null && break; perl -e 'select(undef,undef,undef,0.2)'; done
  FROZEN_BIN="$(sed -n 's/^export PATH=\(.*\):\$PATH$/\1/p' "$D/serve.out")"; FROZEN_KC="$(sed -n 's/^export KUBECONFIG=//p' "$D/serve.out")"
  [ -x "$FROZEN_BIN/kubectl" ] || { say "ABORT: the frozen case was not served"; status=1; break; }
  tool capture "$D/commands.json" "$FROZEN_BIN" "$FROZEN_KC" "$D/frozen.json" || { say "ABORT: the frozen case was not asked"; status=1; break; }
  kill "$SERVE" 2>/dev/null; wait "$SERVE" 2>/dev/null; SERVE=""
  say "=== $case: live, after the freeze"
  tool capture "$D/commands.json" "$OUT/live-bin" "$KC" "$D/live-after.json" || { say "ABORT: the live cluster was not asked again"; status=1; break; }
  ( export KUBECONFIG="$KC"; "$L/scenarios/$case/teardown.sh" ) >> "$OUT/log/$case-setup.log" 2>&1
  python3 "$HERE/replaydiff.py" compare "$D/live-before.json" "$D/frozen.json" "$D/live-after.json" "$HERE/known.txt" > "$D/report.md" || status=1
  say "$case: $(head -1 "$D/report.md")"
  compared=$((compared + 1))
done
cleanup
[ "$compared" -eq "${#CASES[@]}" ] || status=1
say "kind clusters left: $("$KIND" get clusters 2>&1 | tr '\n' ' ')"
say "default kubeconfig unchanged: $([ "$DEFAULT_BEFORE" = "$(shasum -a 256 "$HOME/.kube/config" 2>/dev/null | cut -c1-16)" ] && echo yes || echo NO)"
say "SWEEP-DONE status=$status ($compared of ${#CASES[@]} cases compared)"
exit $status
