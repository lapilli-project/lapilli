# shellcheck shell=bash
# Sourced by the end-to-end scripts that set a replay beside what a real
# server fired over a period (loki.sh, compat.sh, kubernetes-rules.sh,
# measure.sh): how many evaluations each server missed. A server on a host
# too busy to run its evaluations on time misses some, and a firing it starts
# an evaluation late is one no replay starts there: the difference is the
# host's, not the replay's (docs/design/states.md).
#
# The counters: Prometheus's prometheus_rule_group_iterations_missed_total,
# Loki's loki_prometheus_rule_group_iterations_missed_total, Grafana's
# grafana_alerting_schedule_rule_evaluations_missed_total, vmalert's
# vmalert_iteration_missed_total (app/vmalert/rule/group.go, v1.156.0).
# A counter a server doesn't show yet is 0, as it is registered with the
# first group or the first miss; a server that can't be asked says `?`.
#
# Not a script on its own: the caller has `set -euo pipefail`.

# missed_sum METRIC: a server's /metrics on stdin, METRIC summed over its
# series, as a whole number.
missed_sum() {
  awk -v n="$1" '$1 == n || index($1, n "{") == 1 {s += $NF} END {printf "%d", s}'
}

# missed_url URL METRIC: the count at URL/metrics, or ? when it didn't answer.
missed_url() {
  local m
  m=$(curl -sf "$1/metrics" | missed_sum "$2") || m='?'
  printf '%s' "$m"
}

# missed_delta BEFORE AFTER: each count of AFTER less its count in BEFORE
# (words in the same order), ? where either said ?.
missed_delta() {
  local -a a b
  local i out=
  read -ra a <<<"$1"
  read -ra b <<<"$2"
  for i in "${!b[@]}"; do
    if [ "${a[i]:-?}" = '?' ] || [ "${b[i]}" = '?' ]; then out+='? '; else out+="$((b[i] - a[i])) "; fi
  done
  printf '%s' "${out% }"
}

# missed_some DELTAS: succeeds when one of them is above 0.
missed_some() {
  local d
  for d in $1; do
    case $d in '?' | 0) ;; *) return 0 ;; esac
  done
  return 1
}

# missed_list NAMES DELTAS: "Prometheus 0, Loki 0" (NAMES, as words in the
# order of DELTAS).
missed_list() {
  local -a n d
  local i out=''
  read -ra n <<<"$1"
  read -ra d <<<"$2"
  for i in "${!n[@]}"; do out+="${out:+, }${n[i]} ${d[i]:-?}"; done
  printf '%s' "$out"
}

# missed_say NAMES DELTAS [SINCE]: one line, "evaluations missed in the
# period: Prometheus 0, Loki 0".
missed_say() {
  echo "evaluations missed ${3:-in the period}: $(missed_list "$1" "$2")"
}

# missed_explain NAMES DELTAS: for a failed comparison, when some server
# missed evaluations, says so. It explains the difference and doesn't excuse
# it: the caller still fails.
missed_explain() {
  if missed_some "$2"; then
    echo "FAILED: the servers missed evaluations in the period ($(missed_list "$1" "$2")), as no replay does: run again on a host less busy"
  fi
}

# missed_ticks CONTAINER FROM UNTIL: the "Tick dropped" lines Grafana in
# CONTAINER logged from FROM to UNTIL (epoch seconds), each reduced to its
# rule uid and the tick it dropped.
missed_ticks() {
  local n
  n=$(docker logs --since "$2" --until "$3" "$1" 2>&1 | grep -F 'Tick dropped' |
    awk '{u = ""; t = ""; for (i = 1; i <= NF; i++) { if ($i ~ /^rule_uid=/) u = $i; else if ($i ~ /^time=/) t = $i } print "  Grafana dropped a tick: " (u == "" ? "rule_uid=?" : u) " " (t == "" ? "time=?" : t)}' |
    sort | uniq -c | head -50) || true
  if [ -n "$n" ]; then echo "$n"; else echo "  (no \"Tick dropped\" line from Grafana in the period)"; fi
}
