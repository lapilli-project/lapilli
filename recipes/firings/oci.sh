#!/bin/sh
# Writes when OCI Monitoring alarms were firing to stdout, from their history,
# as lapilli verdict --firings takes them: one firing a line. It uses the OCI
# CLI, signed in however it is (a config file profile, instance or resource
# principal).
#
#   COMPARTMENT_ID  the compartment the alarms are in; with SUBTREE=true and
#                   the tenancy's id, every compartment
#   SINCE, UNTIL    the period (ISO 8601). OCI keeps 90 days of history.
#   MAX_PAGES       of history for one alarm, at most (default 100)
#
# It needs to read alarms (inspect alarms, read alarms).
set -eu
here=$(dirname "$0")
: "${COMPARTMENT_ID:?the compartment the alarms are in}"
: "${SINCE:?the start of the period, such as 2026-11-01T00:00:00Z}"
for t in "$SINCE" "${UNTIL:-$SINCE}"; do
  # grep reads lines: a second line could carry anything past it.
  c=$t
  case $t in *[!0-9TZ:.+-]* | '') c=x ;; esac
  if ! printf '%s' "$c" | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$'; then
    printf '%s: %s is not a time such as 2026-11-01T00:00:00Z\n' "oci.sh" "$t" >&2
    exit 1
  fi
done
max=${MAX_PAGES:-100}
case $max in
  '' | *[!0-9]*)
    printf 'oci.sh: MAX_PAGES is a number of pages, not %s\n' "$max" >&2
    exit 1
    ;;
esac
alarms=$(mktemp)
entries=$(mktemp)
page=$(mktemp)
trap 'rm -f "$alarms" "$entries" "$page"' EXIT
trap 'exit 1' INT TERM HUP
set -- --compartment-id "$COMPARTMENT_ID" --all --output json
[ "${SUBTREE:-}" != true ] || set -- "$@" --compartment-id-in-subtree true
# To files first: in a pipe, an oci that failed would go unnoticed.
oci monitoring alarm list "$@" >"$alarms"
for id in $(jq -r '(.data // [])[].id' "$alarms"); do
  next=
  pages=0
  while :; do
    pages=$((pages + 1))
    if [ "$pages" -gt "$max" ]; then
      printf 'oci.sh: more than %s pages of history for %s; stopping, writing nothing (raise MAX_PAGES, or shorten the period)\n' "$max" "$id" >&2
      exit 1
    fi
    set -- --alarm-id "$id" --alarm-historytype STATE_TRANSITION_HISTORY \
      --timestamp-greater-than-or-equal-to "$SINCE" --limit 1000 --output json
    [ -z "${UNTIL:-}" ] || set -- "$@" --timestamp-less-than "$UNTIL"
    [ -z "$next" ] || set -- "$@" --page "$next"
    oci monitoring alarm-history-collection get-alarm-history "$@" >"$page"
    jq -c --arg id "$id" '(.data.entries // [])[] | {alarm: $id} + .' "$page" >>"$entries"
    last=$next
    next=$(jq -r '."opc-next-page" // empty' "$page")
    [ -n "$next" ] || break
    if [ "$next" = "$last" ]; then
      printf 'oci.sh: the same page again for %s; stopping, writing nothing\n' "$id" >&2
      exit 1
    fi
  done
done
jq -c -n --slurpfile alarms "$alarms" --slurpfile entries "$entries" -f "$here/oci.jq"
