#!/bin/bash
# table.sh [IMAGE ...]: what lapilli concluded in each run.sh scenario, from the
# saved log and live, per distribution. A cell reads "code" when both agree,
# "log/live" when they differ; "!" marks a run that changed known_hosts.
set -euo pipefail
H="$(cd "$(dirname "$0")" && pwd)"
cd "$H/out"
dirs=()
if [ $# -gt 0 ]; then
  for img in "$@"; do dirs+=("$(echo "$img" | sed 's#.*/##; s/:/-/')"); done
else
  for d in */; do dirs+=("${d%/}"); done
fi
code() { jq -r '.code // "?"' "$1" 2> /dev/null || echo "-"; }

printf '%-26s' scenario
for d in "${dirs[@]}"; do printf ' %-22s' "$d $(grep -oE 'OpenSSH_[0-9.]+p?[0-9]*' "$d/VERSION" | sed 's/OpenSSH_//')"; done
echo
for f in "${dirs[0]}"/*.log; do
  n="$(basename "$f" .log)"
  printf '%-26s' "$n"
  for d in "${dirs[@]}"; do
    a="$(code "$d/$n.log.json")"; b="$(code "$d/$n.live.json")"
    cell="$a"; [ "$a" = "$b" ] || cell="$a/$b"
    [ -f "$d/$n.wrote" ] && cell="$cell!"
    printf ' %-22s' "$cell"
  done
  echo
done
