#!/bin/sh
# Writes incident.io's incidents to stdout, as Lapilli's incidents file takes
# them. See incident-io.jq for the names to change for your organisation.
# The alerts linked to each, where an attribute of theirs says alertname,
# are its alerts (docs/design/links.md).
#
#   INCIDENT_IO_TOKEN_FILE  a file holding an API key that can view incidents
#                           and alerts
#   SINCE                   only those created on or after this date,
#                           like 2026-10-01
#   MAX_PAGES               of 100 incidents, at most (default 1000)
#   ALERTS                  yes (default) to read each incident's alerts, or no
#   ALERT_LABELS            the labels an alert keeps, comma-separated, such
#                           as service,namespace (default all its attributes
#                           give)
#   MAX_ALERT_REQUESTS      for alerts, at most (default 2000): one a page
#                           of an incident's, and one an alert
set -eu
here=$(dirname "$0")
api=${INCIDENT_IO_API:-https://api.incident.io}
# The token goes where api points: over https, or to this machine, for
# tests, and curl speaks no other scheme (proto). Neither is said: a user
# and password before @ would be in the log.
# The scheme may be in capitals, as curl takes it. This machine is
# 127.0.0.1, [::1] or localhost, with a port or not, and no name that only
# starts with one, such as localhost.example.com.
proto=
case $api in
  *@*)
    printf 'incident-io.sh: INCIDENT_IO_API holds a user or token before @, refused\n' >&2
    exit 1
    ;;
  [Hh][Tt][Tt][Pp][Ss]://*) proto='=https' ;;
  [Hh][Tt][Tt][Pp]://*)
    case ${api#*://} in
      127.0.0.1 | '[::1]' | [Ll][Oo][Cc][Aa][Ll][Hh][Oo][Ss][Tt]) proto='=http' ;;
      127.0.0.1[/:]* | '[::1]'[/:]* | [Ll][Oo][Cc][Aa][Ll][Hh][Oo][Ss][Tt][/:]*) proto='=http' ;;
    esac
    ;;
esac
if [ -z "$proto" ]; then
  printf 'incident-io.sh: INCIDENT_IO_API is not https\n' >&2
  exit 1
fi
: "${INCIDENT_IO_TOKEN_FILE:?a file holding an incident.io API key}"
# secret NAME FILE: what FILE holds, one line of the characters a token
# is written in (a Bearer token's, RFC 6750), as it goes in a header.
# Neither the token nor what else the file holds is said.
secret() {
  s=$(cat "$2")
  # Spaces, tabs and a carriage return after it, as an editor may leave
  # them, are dropped.
  while :; do
    case $s in
      *[[:space:]]) s=${s%?} ;;
      *) break ;;
    esac
  done
  case $s in
    '' | *[!A-Za-z0-9._~+/=-]*)
      printf '%s\n' "incident-io.sh: $1 doesn't hold a token: one line of letters, digits and - . _ ~ + / =" >&2
      exit 1
      ;;
  esac
  printf '%s' "$s"
}
token=$(secret INCIDENT_IO_TOKEN_FILE "$INCIDENT_IO_TOKEN_FILE")
all=$(mktemp)
ids=$(mktemp)
linked=$(mktemp)
out=$(mktemp)
alertIDs=$(mktemp)
trap 'rm -f "$all" "$ids" "$linked" "$out" "$alertIDs"' EXIT
trap 'exit 1' INT TERM HUP
max=${MAX_PAGES:-1000}
case $max in
  '' | *[!0-9]*)
    printf '%s: MAX_PAGES is a number of pages, not %s\n' "incident-io.sh" "$max" >&2
    exit 1
    ;;
esac
alerts=${ALERTS:-yes}
maxreq=${MAX_ALERT_REQUESTS:-2000}
case $alerts in
  yes | no) ;;
  *)
    printf 'incident-io.sh: ALERTS is yes or no\n' >&2
    exit 1
    ;;
esac
case $maxreq in
  '' | *[!0-9]*)
    printf 'incident-io.sh: MAX_ALERT_REQUESTS is a number of requests\n' >&2
    exit 1
    ;;
esac
requests=0
# count: one request more for alerts, past MAX_ALERT_REQUESTS a stop. In
# this shell, not the one $(get ...) runs in, which forgets what it counts.
count() {
  requests=$((requests + 1))
  if [ "$requests" -gt "$maxreq" ]; then
    printf 'incident-io.sh: more than %s requests for alerts; stopping, writing nothing (raise MAX_ALERT_REQUESTS, set SINCE later, or set ALERTS=no)\n' "$maxreq" >&2
    exit 1
  fi
}
get() { printf 'Authorization: Bearer %s\n' "$token" | curl -q -fsS --proto "$proto" --max-time 30 --max-filesize 16777216 -H @- "$1"; }
after=
pages=0
while :; do
  pages=$((pages + 1))
  if [ "$pages" -gt "$max" ]; then
    printf 'incident-io.sh: more than %s pages; stopping, writing nothing (raise MAX_PAGES, or set SINCE)\n' "$max" >&2
    exit 1
  fi
  url="$api/v2/incidents?page_size=100"
  [ -z "$after" ] || url="$url&after=$(printf '%s' "$after" | jq -sRr @uri)"
  [ -z "${SINCE:-}" ] || url="$url&created_at%5Bgte%5D=$(printf '%s' "$SINCE" | jq -sRr @uri)"
  page=$(printf 'Authorization: Bearer %s\n' "$token" | curl -q -fsS --proto "$proto" --max-time 30 --max-filesize 16777216 -H @- "$url")
  printf '%s' "$page" | jq -c '.incidents[]' >>"$all"
  was=$after
  after=$(printf '%s' "$page" | jq -r '.pagination_meta.after // empty')
  [ -n "$after" ] || break
  if [ "$after" = "$was" ]; then
    printf 'incident-io.sh: the API gave the same page again; stopping, writing nothing\n' >&2
    exit 1
  fi
done
if [ "$alerts" = yes ]; then
  jq -r -s -L "$here" -f "$here/incident-io.jq" --argjson ids true "$all" >"$ids"
  while IFS= read -r id; do
    # Ids go in a query and a path: incident.io's are letters and digits.
    case $id in
      '' | *[!A-Za-z0-9]*)
        printf 'incident-io.sh: an incident id that is not letters and digits; stopping, writing nothing\n' >&2
        exit 1
        ;;
    esac
    : >"$alertIDs"
    after=
    while :; do
      url="$api/v2/incident_alerts?page_size=50&incident_id=$id"
      [ -z "$after" ] || url="$url&after=$(printf '%s' "$after" | jq -sRr @uri)"
      count
      page=$(get "$url")
      printf '%s' "$page" | jq -r '.incident_alerts[]?.alert.id' >>"$alertIDs"
      was=$after
      after=$(printf '%s' "$page" | jq -r '.pagination_meta.after // empty')
      [ -n "$after" ] || break
      if [ "$after" = "$was" ]; then
        printf 'incident-io.sh: the API gave the same page of alerts again; stopping, writing nothing\n' >&2
        exit 1
      fi
    done
    while IFS= read -r alert; do
      case $alert in
        '' | *[!A-Za-z0-9]*)
          printf 'incident-io.sh: an alert id that is not letters and digits; stopping, writing nothing\n' >&2
          exit 1
          ;;
      esac
      count
      one=$(get "$api/v2/alerts/$alert")
      printf '%s' "$one" | jq -c --arg id "$id" '{id: $id, alerts: [.alert // empty]}' >>"$linked"
    done <"$alertIDs"
  done <"$ids"
fi
jq -s -L "$here" -f "$here/incident-io.jq" --slurpfile linked "$linked" --arg alert_labels "${ALERT_LABELS:-}" "$all" >"$out"
jq -r -L "$here" 'include "alerts"; said("incident-io.sh"; "no attribute of theirs is named alertname")' "$out" >&2
jq '{incidents}' "$out"
