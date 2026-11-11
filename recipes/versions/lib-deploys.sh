# shellcheck shell=sh
# Shared by argocd.sh and flux.sh: kept DEPLOYS_KEEP NEW writes the deploys
# in NEW and in the file DEPLOYS_KEEP names, if any, each revision once, at
# its earliest time, the earliest first.
kept() {
  if [ -n "${DEPLOYS_KEEP:-}" ] && [ -f "$DEPLOYS_KEEP" ]; then
    cat "$DEPLOYS_KEEP" "$1"
  else
    cat "$1"
  fi | grep -E '^[0-9a-f]{7,64} [0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$' |
    sort -k1,1 -k2,2 | awk '!seen[$1]++' | sort -k2,2 -k1,1
}
