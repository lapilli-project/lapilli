#!/usr/bin/env bash
# Round 39: one agent on the frozen cases of round 38, per docs/design-review-round39.md Part 1.
#   LAPILLI_CASE=<binary> [LAPILLI_CRUST_GATHER=…] run.sh <repo> <out> [case...]
# No cluster is built and none is reached: every run is of a frozen case, served from this machine.
# The procedure for one run, and what is set aside as a provider's failure, are round 38's (lib.sh).
set -uo pipefail
L="$1"; OUT="$2"; shift 2
CASES=("$@"); [ ${#CASES[@]} -eq 0 ] && CASES=(s1-shared-cache-exhaustion s2-periodic-saturation s3-node-local-drift)
CAP_CLAUDE="${CAP_CLAUDE:-5}"
. "$L/test/round38/lib.sh"
FROM="$L/test/fixtures/case-runs/2026-10-07-round38/frozen-cases"

DEFAULT_BEFORE=$(shasum -a 256 "$HOME/.kube/config" 2>/dev/null | cut -c1-16)
say "round 39 run: N=$N cases=${CASES[*]} tool=$("$BIN" --version)"
PIDS=()
for case in "${CASES[@]}"; do
  "$BIN" verify "$FROM/$case" >> "$OUT/log/run.log" 2>&1 || { say "ABORT: $case is not the case that was sealed"; exit 1; }
  series claude-code frozen "$case" "$FROM/$case" & PIDS+=($!)
done
for p in "${PIDS[@]}"; do wait "$p"; done
say "default kubeconfig unchanged at end: $([ "$DEFAULT_BEFORE" = "$(shasum -a 256 "$HOME/.kube/config" 2>/dev/null | cut -c1-16)" ] && echo yes || echo NO)"
say "spent: claude-code \$$(spent claude-code)"
say "ROUND39-RUNS-DONE"
