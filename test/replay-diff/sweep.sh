#!/usr/bin/env bash
# The same command against a cluster and against its frozen copy, output compared.
#   LAPILLI_CASE=<binary> [KIND=…] [KIND_NODE_IMAGE=…] [LAPILLI_CRUST_GATHER=…] [RECORDED=…[:…] OLD_SNAPSHOTS=…] sweep.sh <repo> <out> [case...]
# A case is the name of a scenario under scenarios/, or a directory holding a setup.sh, a teardown.sh
# and a case.yaml of its own. With none given: the three scenarios, and kinds/, which has one of every
# kind the scenarios do not.
# Per case: build the scenario on a kind cluster, ask every command of the live cluster, freeze it,
# serve the frozen copy and ask them of that, and ask them of the live cluster again. Three answers a
# command, within a minute: the two live ones say whether the cluster itself moved in between.
# A case frozen with its metrics is asked what promq prints as well (promdiff.py): every query about
# the instant of the freeze, of the Prometheus, of the frozen store, and of the Prometheus again.
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
BIN="${LAPILLI_CASE:?set LAPILLI_CASE to the lapilli-case binary under test}"
KIND="${KIND:-kind}"; KC="$OUT/rcabench.kubeconfig"; HERE="$(cd "$(dirname "$0")" && pwd)"
CASES=("$@"); [ ${#CASES[@]} -eq 0 ] && CASES=(s1-shared-cache-exhaustion s2-periodic-saturation s3-node-local-drift "$HERE/kinds")
export LAPILLI_CRUST_GATHER="${LAPILLI_CRUST_GATHER:-kubectl-crust-gather}"
# The commands agents typed are taken from the runs recorded here — directories, a colon between
# them — and the pod names in them from the snapshots those runs saw: every judged run made on the
# cases frozen for round 38, unless others are named.
RUNS="$L/test/fixtures/case-runs"
RECORDED="${RECORDED:-$RUNS/2026-10-07-round38:$RUNS/2026-10-08-round39:$RUNS/2026-10-08-round39-second:$RUNS/2026-10-08-round39-after}"
OLD_SNAPSHOTS="${OLD_SNAPSHOTS:-$RUNS/2026-10-07-round38/frozen-cases}"
IFS=: read -r -a RECORDED_DIRS <<< "$RECORDED"
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

# Before anything is built: every scenario asked for has runs recorded where they are said to be.
for given in "${CASES[@]}"; do
  [ -f "$given/setup.sh" ] && continue
  for recorded in "${RECORDED_DIRS[@]}"; do
    [ -d "$recorded/$given" ] || { say "ABORT: no runs of $given are recorded under $recorded"; exit 1; }
  done
done

rm -f "$KC"
# Another version of Kubernetes than kind's own: KIND_NODE_IMAGE=kindest/node:v1.33.1
"$KIND" create cluster --name rcabench --config "$L/scenarios/kind.yaml" --kubeconfig "$KC" ${KIND_NODE_IMAGE:+--image "$KIND_NODE_IMAGE"} >> "$OUT/log/kind.log" 2>&1 || { say "ABORT: kind could not create the cluster"; exit 1; }
CLUSTER=yes
[ "$(command kubectl --kubeconfig "$KC" config current-context)" = "kind-rcabench" ] || { say "ABORT: wrong context"; exit 1; }

# The guard, in front of the live cluster: what an agent's kubectl is in a live run. And its promq.
mkdir -p "$OUT/live-bin"
printf '#!/bin/sh\nexec "%s" guard-kubectl "%s" "%s" "$@"\n' "$BIN" "$KC" "$real_kubectl" > "$OUT/live-bin/kubectl"; chmod +x "$OUT/live-bin/kubectl"
printf '#!/bin/sh\nexec "%s" promq "$@"\n' "$BIN" > "$OUT/live-bin/promq"; chmod +x "$OUT/live-bin/promq"
prom() { python3 "$HERE/promdiff.py" "$@" 2>&1 | tee -a "$OUT/log/sweep.log"; return "${PIPESTATUS[0]}"; }
LIVE_PROM=http://127.0.0.1:19392

status=0; compared=0
for given in "${CASES[@]}"; do
  if [ -f "$given/setup.sh" ] && [ -f "$given/case.yaml" ]; then   # a fixture of its own: no agent has been run on it, so nothing was typed
    FROM="$(cd "$given" && pwd)"; case="$(basename "$FROM")"; KEY="$FROM/case.yaml"; TYPED=()
  else
    FROM="$L/scenarios/$given"; case="$given"; KEY="$L/cases/$given/case.yaml"
    TYPED=(--old-snapshot "$OLD_SNAPSHOTS/$given/kubernetes.tar.gz")
    for recorded in "${RECORDED_DIRS[@]}"; do TYPED+=(--runs "$recorded/$given"); done
  fi
  D="$OUT/$case"; mkdir -p "$D"
  say "=== $case: setup"
  # A scenario can fail to form: s3 needs the scheduler to put half its pods on one node. Once more,
  # then, from nothing — and after that it is a failure, not something to sweep around.
  if ! ( export KUBECONFIG="$KC"; "$FROM/setup.sh" ) >> "$OUT/log/$case-setup.log" 2>&1; then
    say "$case did not form its symptom; torn down and built once more"
    ( export KUBECONFIG="$KC"; "$FROM/teardown.sh" ) >> "$OUT/log/$case-setup.log" 2>&1
    ( export KUBECONFIG="$KC"; "$FROM/setup.sh" ) >> "$OUT/log/$case-setup.log" 2>&1 || { say "ABORT: the scenario did not form its symptom, twice"; status=1; break; }
  fi
  FREEZE=()
  if [ "$case" = s2-periodic-saturation ]; then   # its key names evidence in the metrics store, so the freeze needs the store
    # The one case with a Prometheus, by its name: a case of another name that has one is not asked about it here.
    curl -fs "$LIVE_PROM/-/ready" >/dev/null 2>&1 && { say "ABORT: something answers at $LIVE_PROM already, and it is not this cluster's Prometheus"; status=1; break; }
    command kubectl --kubeconfig "$KC" --context kind-rcabench -n monitoring port-forward svc/prometheus 19392:9090 >/dev/null 2>&1 & PF=$!
    for i in $(seq 1 50); do curl -fs "$LIVE_PROM/-/ready" >/dev/null 2>&1 && break; perl -e 'select(undef,undef,undef,0.2)'; done
    kill -0 "$PF" 2>/dev/null && curl -fs "$LIVE_PROM/-/ready" >/dev/null 2>&1 || { say "ABORT: the cluster's Prometheus could not be reached"; status=1; break; }
    FREEZE=(--metrics-url "$LIVE_PROM" --metrics-window 30m)
    # What promq prints is half of what an agent reads from such a case: asked too (promdiff.py).
    prom queries "$LIVE_PROM" "$D/queries.json" "$D/requests.json" ${TYPED[@]+"${TYPED[@]}"} || { say "ABORT: no queries to ask"; status=1; break; }
  fi
  tool commands "$KC" "$D/commands.json" ${TYPED[@]+"${TYPED[@]}"} || { say "ABORT: nothing to ask"; status=1; break; }
  say "=== $case: live, before the freeze"
  tool capture "$D/commands.json" "$OUT/live-bin" "$KC" "$D/live-before.json" || { say "ABORT: the live cluster was not asked"; status=1; break; }
  say "=== $case: freeze"
  rm -rf "$D/frozen"
  "$BIN" freeze "$KEY" -o "$D/frozen" --kubeconfig "$KC" ${FREEZE[@]+"${FREEZE[@]}"} > "$D/freeze.json" 2>> "$OUT/log/sweep.log" || { say "ABORT: freeze failed"; status=1; break; }
  AT=""
  if [ -n "$PF" ]; then   # the live Prometheus, about the instant of the freeze: as soon after it as can be, and again at the end
    AT="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["freeze_time"])' "$D/frozen/freeze.json")"
    prom capture "$D/queries.json" "$OUT/live-bin" "$LIVE_PROM" "$D/promq-live-1.json" --at "$AT" || { say "ABORT: the live Prometheus was not asked"; status=1; break; }
    prom fetch "$D/requests.json" "$LIVE_PROM" "$AT" "$D/api-live-1.json" || { say "ABORT: the live Prometheus's API was not asked"; status=1; break; }
  fi
  say "=== $case: frozen"
  "$BIN" serve "$D/frozen" > "$D/serve.out" 2> "$D/serve.err" & SERVE=$!
  for i in $(seq 1 150); do grep -q '^export PATH=' "$D/serve.out" 2>/dev/null && break; perl -e 'select(undef,undef,undef,0.2)'; done
  FROZEN_BIN="$(sed -n 's/^export PATH=\(.*\):\$PATH$/\1/p' "$D/serve.out")"; FROZEN_KC="$(sed -n 's/^export KUBECONFIG=//p' "$D/serve.out")"
  [ -x "$FROZEN_BIN/kubectl" ] || { say "ABORT: the frozen case was not served"; status=1; break; }
  tool capture "$D/commands.json" "$FROZEN_BIN" "$FROZEN_KC" "$D/frozen.json" || { say "ABORT: the frozen case was not asked"; status=1; break; }
  if [ -n "$AT" ]; then   # the frozen store, about its freeze by name and then as an agent asks, with no instant named
    FROZEN_PROM="$(sed -n 's/^export PROM_URL=//p' "$D/serve.out")"
    [ -n "$FROZEN_PROM" ] || { say "ABORT: the frozen case serves no metrics"; status=1; break; }
    prom capture "$D/queries.json" "$FROZEN_BIN" "$FROZEN_PROM" "$D/promq-frozen.json" --at "$AT" || { say "ABORT: the frozen store was not asked"; status=1; break; }
    prom capture "$D/queries.json" "$FROZEN_BIN" "$FROZEN_PROM" "$D/promq-as-typed.json" || { say "ABORT: the frozen store was not asked as an agent asks"; status=1; break; }
    prom fetch "$D/requests.json" "$FROZEN_PROM" "$AT" "$D/api-frozen.json" || { say "ABORT: the frozen store's API was not asked"; status=1; break; }
  fi
  kill "$SERVE" 2>/dev/null; wait "$SERVE" 2>/dev/null; SERVE=""
  say "=== $case: live, after the freeze"
  tool capture "$D/commands.json" "$OUT/live-bin" "$KC" "$D/live-after.json" || { say "ABORT: the live cluster was not asked again"; status=1; break; }
  if [ -n "$AT" ]; then
    prom capture "$D/queries.json" "$OUT/live-bin" "$LIVE_PROM" "$D/promq-live-2.json" --at "$AT" || { say "ABORT: the live Prometheus was not asked again"; status=1; break; }
    prom fetch "$D/requests.json" "$LIVE_PROM" "$AT" "$D/api-live-2.json" || { say "ABORT: the live Prometheus's API was not asked again"; status=1; break; }
    kill "$PF" 2>/dev/null; PF=""
    python3 "$HERE/promdiff.py" compare "$D/promq-live-1.json" "$D/promq-frozen.json" "$D/promq-live-2.json" "$D/promq-as-typed.json" "$HERE/known-promq.txt" > "$D/report-promq.md" || status=1
    say "$case, promq: $(head -1 "$D/report-promq.md")"
    python3 "$HERE/promdiff.py" compare "$D/api-live-1.json" "$D/api-frozen.json" "$D/api-live-2.json" - "$HERE/known-promq.txt" > "$D/report-api.md" || status=1
    say "$case, the metrics API: $(head -1 "$D/report-api.md")"
  fi
  ( export KUBECONFIG="$KC"; "$FROM/teardown.sh" ) >> "$OUT/log/$case-setup.log" 2>&1
  python3 "$HERE/replaydiff.py" compare "$D/live-before.json" "$D/frozen.json" "$D/live-after.json" "$HERE/known.txt" "$D/frozen/freeze.json" > "$D/report.md" || status=1
  say "$case: $(head -1 "$D/report.md")"
  compared=$((compared + 1))
done
cleanup
[ "$compared" -eq "${#CASES[@]}" ] || status=1
say "kind clusters left: $("$KIND" get clusters 2>&1 | tr '\n' ' ')"
say "default kubeconfig unchanged: $([ "$DEFAULT_BEFORE" = "$(shasum -a 256 "$HOME/.kube/config" 2>/dev/null | cut -c1-16)" ] && echo yes || echo NO)"
say "SWEEP-DONE status=$status ($compared of ${#CASES[@]} cases compared)"
exit $status
