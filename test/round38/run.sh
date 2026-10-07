#!/usr/bin/env bash
# Round 38: live against frozen, per docs/design-review-round38.md Part 1.
#   LAPILLI_CASE=<binary> [KIND=…] [LAPILLI_CRUST_GATHER=…] [HOLMES_BIN=<dir>] run.sh <repo> <out> [case...]
# Everything that touches a cluster goes through one kubeconfig written by kind for this run, named
# explicitly on every call. The default kubeconfig is never read.
set -uo pipefail
L="$1"; OUT="$2"; shift 2
CASES=("$@"); [ ${#CASES[@]} -eq 0 ] && CASES=(s1-shared-cache-exhaustion s2-periodic-saturation s3-node-local-drift)
. "$(dirname "$0")/lib.sh"

DEFAULT_BEFORE=$(shasum -a 256 "$HOME/.kube/config" 2>/dev/null | cut -c1-16)
say "round 38 run: N=$N cases=${CASES[*]} tool=$("$BIN" --version)"
rm -f "$KC"
"$KIND" create cluster --name rcabench --config "$L/scenarios/kind.yaml" --kubeconfig "$KC" >> "$OUT/log/kind.log" 2>&1 || { say "ABORT: kind could not create the cluster"; exit 1; }
[ "$(command kubectl --kubeconfig "$KC" config current-context)" = "kind-rcabench" ] || { say "ABORT: wrong context"; exit 1; }
say "default kubeconfig unchanged after create: $([ "$DEFAULT_BEFORE" = "$(shasum -a 256 "$HOME/.kube/config" 2>/dev/null | cut -c1-16)" ] && echo yes || echo NO)"

FROZEN_PIDS=()
for case in "${CASES[@]}"; do
  say "=== $case: setup"
  ( export KUBECONFIG="$KC"; "$L/scenarios/$case/setup.sh" ) >> "$OUT/log/$case-setup.log" 2>&1 || { say "ABORT: the scenario did not form its symptom"; break; }
  LIVE=(--live --kubeconfig "$KC"); FREEZE=(); PF=""
  if [ "$case" = s2-periodic-saturation ]; then
    k -n monitoring port-forward svc/prometheus 19392:9090 >/dev/null 2>&1 & PF=$!
    for i in $(seq 1 50); do curl -fs http://127.0.0.1:19392/-/ready >/dev/null 2>&1 && break; perl -e 'select(undef,undef,undef,0.2)'; done
    LIVE+=(--prom-url http://127.0.0.1:19392); FREEZE=(--metrics-url http://127.0.0.1:19392 --metrics-window 30m)
  fi
  say "=== $case: live runs"
  series claude-code live "$case" "$L/cases/$case" "${LIVE[@]}" & A=$!
  series holmes      live "$case" "$L/cases/$case" "${LIVE[@]}" & B=$!
  wait $A; wait $B
  say "=== $case: freeze"
  rm -rf "$FROZEN/$case"
  "$BIN" freeze "$L/cases/$case/case.yaml" -o "$FROZEN/$case" --kubeconfig "$KC" ${FREEZE[@]+"${FREEZE[@]}"} > "$OUT/log/$case-freeze.json" 2>> "$OUT/log/run.log"; say "freeze exit $? ($("$BIN" verify "$FROZEN/$case" | sed 's/.*: //'))"
  [ -n "$PF" ] && kill "$PF" 2>/dev/null
  ( export KUBECONFIG="$KC"; "$L/scenarios/$case/teardown.sh" ) >> "$OUT/log/$case-setup.log" 2>&1
  say "=== $case: frozen runs (in the background, while the next case is built)"
  series claude-code frozen "$case" "$FROZEN/$case" & FROZEN_PIDS+=($!)
  series holmes      frozen "$case" "$FROZEN/$case" & FROZEN_PIDS+=($!)
done
"$KIND" delete cluster --name rcabench --kubeconfig "$KC" >> "$OUT/log/kind.log" 2>&1
say "cluster deleted; waiting for the frozen runs"
for p in ${FROZEN_PIDS[@]+"${FROZEN_PIDS[@]}"}; do wait "$p"; done
say "kind clusters left: $("$KIND" get clusters 2>&1 | tr '\n' ' ')"
say "default kubeconfig unchanged at end: $([ "$DEFAULT_BEFORE" = "$(shasum -a 256 "$HOME/.kube/config" 2>/dev/null | cut -c1-16)" ] && echo yes || echo NO)"
say "spent: claude-code \$$(spent claude-code), holmes \$$(spent holmes)"
say "ROUND38-RUNS-DONE"
