# Sourced by run.sh with L (the repository) and OUT (where everything is written) set.
N="${N:-6}"; CAP_CLAUDE="${CAP_CLAUDE:-12}"; CAP_HOLMES="${CAP_HOLMES:-6}"
BIN="${LAPILLI_CASE:?set LAPILLI_CASE to the lapilli-case binary under test}"
KIND="${KIND:-kind}"; KC="$OUT/rcabench.kubeconfig"
HOLMES_BIN="${HOLMES_BIN:-}"                                   # the directory `holmes` is in, if not on PATH
export LAPILLI_CRUST_GATHER="${LAPILLI_CRUST_GATHER:-kubectl-crust-gather}"
RUNS="$OUT/runs"; EXCL="$OUT/excluded"; FROZEN="$OUT/frozen"; mkdir -p "$RUNS" "$EXCL" "$FROZEN" "$OUT/log"
k() { command kubectl --kubeconfig "$KC" --context kind-rcabench "$@"; }
say() { echo "[$(date -u +%H:%M:%S)] $*" | tee -a "$OUT/log/run.log"; }

PROVIDER='(HTTP[ /]*5[0-9][0-9]|\b50[0-9]\b|\b429\b|quota|rate.?limit|overloaded|UNAVAILABLE|ServiceUnavailable|RateLimitError|credit balance|insufficient_quota|InternalServerError|APIConnectionError)'

spent() { # agent -> dollars reported so far by its harness, kept and excluded runs alike
  python3 - "$RUNS" "$EXCL" "$1" <<'PY'
import glob, json, sys
total = 0.0
for root in sys.argv[1:3]:
    for f in glob.glob(root + "/*/*.json"):
        t = json.load(open(f))["transcript"]
        if t["agent"] == sys.argv[3]:
            total += float((t.get("usage") or {}).get("cost_usd") or 0)
print(f"{total:.3f}")
PY
}

newest() { ls -t "$RUNS/$1"/"$2"-*.json 2>/dev/null | head -1; }   # case, slug prefix

# one investigation; returns 0 kept, 1 excluded as a provider failure, 2 the agent is stopped
one() { # agent condition case target [extra args...]
  local agent="$1" cond="$2" case="$3" target="$4"; shift 4
  local cap="$CAP_CLAUDE" model="haiku" extra=()
  if [ "$agent" = holmes ]; then cap="$CAP_HOLMES"; model="gpt-5-mini"; extra=(--temperature 1 --pass-env OPENAI_API_KEY); fi
  if python3 -c "import sys; sys.exit(0 if float('$(spent "$agent")') >= float('$cap') else 1)"; then say "$agent: spending limit of \$$cap reached; stopped"; return 2; fi
  local before; before=$(newest "$case" "$cond-$agent")
  if [ "$agent" = holmes ]; then
    PATH="${HOLMES_BIN:+$HOLMES_BIN:}$PATH" "$BIN" run "$target" --agent "$agent" --model "$model" --runs 1 -o "$RUNS" ${extra[@]+"${extra[@]}"} "$@" >> "$OUT/log/$case-$cond-$agent.log" 2>&1
  else
    "$BIN" run "$target" --agent "$agent" --model "$model" --runs 1 -o "$RUNS" "$@" >> "$OUT/log/$case-$cond-$agent.log" 2>&1
  fi
  local rec; rec=$(newest "$case" "$cond-$agent")
  if [ -z "$rec" ] || [ "$rec" = "$before" ]; then say "$agent $cond $case: no record was written (see log)"; return 1; fi
  local verdict; verdict=$(python3 - "$rec" "$PROVIDER" <<'PY'
import json, re, sys
t = json.load(open(sys.argv[1]))["transcript"]
err = t.get("error") or ""
if t.get("answer"): print("answered")
elif re.search(sys.argv[2], err, re.I): print("provider")
else: print("no-answer")
PY
)
  if [ "$verdict" = provider ]; then
    mkdir -p "$EXCL/$case"; mv "$rec" "$EXCL/$case/$(basename "$rec" .json)-$(date -u +%H%M%S).json"
    say "$agent $cond $case: provider failure, excluded"; return 1
  fi
  say "$agent $cond $case: $(basename "$rec" .json) $verdict"; return 0
}

# N kept runs of one agent in one condition; a provider failure is run once more, two in a row stop the agent
series() { # agent condition case target [extra...]
  local agent="$1" kept=0 failed=0 rc
  while [ "$kept" -lt "$N" ]; do
    if [ -e "$OUT/stopped-$agent" ]; then return 1; fi   # stopped elsewhere: no new run is started
    one "$@"; rc=$?
    case $rc in
      0) kept=$((kept+1)); failed=0 ;;
      1) failed=$((failed+1)); if [ "$failed" -ge 2 ]; then say "$agent: two failures in a row; stopped with $kept of $N in $2 $3"; touch "$OUT/stopped-$agent"; return 1; fi ;;
      2) touch "$OUT/stopped-$agent"; return 1 ;;
    esac
    if [ -e "$OUT/stopped-$agent" ]; then return 1; fi
  done
  return 0
}

