#!/bin/sh
# Writes PagerDuty's incidents to stdout, as Lapilli's incidents file takes
# them. Their services become the label service, for --match service, and
# the alerts linked to each, where Alertmanager put their labels in their
# details, its alerts (docs/design/links.md).
#
#   PAGERDUTY_TOKEN_FILE  a file holding a read-only REST API key
#   SINCE, UNTIL          the period (ISO 8601; six months at most); PagerDuty's
#                         default, the last month, when not given
#   MAX_PAGES             of 100 incidents, at most (default 1000)
#   ALERTS                yes (default) to read each incident's alerts, or no
#   ALERT_LABELS          the labels an alert keeps, comma-separated, such as
#                         service,namespace (default all its details give)
#   MAX_ALERT_REQUESTS    for alerts, at most (default 2000)
set -eu
here=$(dirname "$0")
api=${PAGERDUTY_API:-https://api.pagerduty.com}
# The token goes where api points: over https, or to this machine, for
# tests, and curl speaks no other scheme (proto). Neither is said: a user
# and password before @ would be in the log.
case $api in
  *@*)
    printf 'pagerduty.sh: PAGERDUTY_API holds a user or token before @, refused\n' >&2
    exit 1
    ;;
  https://*) proto='=https' ;;
  http://127.0.0.1:* | http://localhost:*) proto='=http' ;;
  *)
    printf 'pagerduty.sh: PAGERDUTY_API is not https\n' >&2
    exit 1
    ;;
esac
: "${PAGERDUTY_TOKEN_FILE:?a file holding a PagerDuty API key}"
# secret NAME FILE: what FILE holds, one line of the characters a token
# is written in (a Bearer token's, RFC 6750), as it goes in a header.
# Neither the token nor what else the file holds is said.
secret() {
  s=$(cat "$2")
  case $s in
    '' | *[!A-Za-z0-9._~+/=-]*)
      printf '%s\n' "pagerduty.sh: $1 doesn't hold a token: one line of letters, digits and - . _ ~ + / =" >&2
      exit 1
      ;;
  esac
  printf '%s' "$s"
}
token=$(secret PAGERDUTY_TOKEN_FILE "$PAGERDUTY_TOKEN_FILE")
all=$(mktemp)
ids=$(mktemp)
linked=$(mktemp)
out=$(mktemp)
trap 'rm -f "$all" "$ids" "$linked" "$out"' EXIT
trap 'exit 1' INT TERM HUP
max=${MAX_PAGES:-1000}
case $max in
  '' | *[!0-9]*)
    printf '%s: MAX_PAGES is a number of pages, not %s\n' "pagerduty.sh" "$max" >&2
    exit 1
    ;;
esac
alerts=${ALERTS:-yes}
maxreq=${MAX_ALERT_REQUESTS:-2000}
case $alerts in
  yes | no) ;;
  *)
    printf 'pagerduty.sh: ALERTS is yes or no\n' >&2
    exit 1
    ;;
esac
case $maxreq in
  '' | *[!0-9]*)
    printf 'pagerduty.sh: MAX_ALERT_REQUESTS is a number of requests\n' >&2
    exit 1
    ;;
esac
offset=0
pages=0
while :; do
  pages=$((pages + 1))
  if [ "$pages" -gt "$max" ]; then
    printf 'pagerduty.sh: more than %s pages; stopping, writing nothing (raise MAX_PAGES, or shorten the period)\n' "$max" >&2
    exit 1
  fi
  url="$api/incidents?limit=100&offset=$offset&sort_by=created_at:asc"
  [ -z "${SINCE:-}" ] || url="$url&since=$(printf '%s' "$SINCE" | jq -sRr @uri)"
  [ -z "${UNTIL:-}" ] || url="$url&until=$(printf '%s' "$UNTIL" | jq -sRr @uri)"
  # The token goes on stdin, not the command line, where ps would show it.
  page=$(printf 'Authorization: Token token=%s\n' "$token" |
    curl -q -fsS --proto "$proto" --max-time 30 --max-filesize 16777216 -H @- -H 'Accept: application/vnd.pagerduty+json;version=2' "$url")
  printf '%s' "$page" | jq -c '.incidents[]' >>"$all"
  [ "$(printf '%s' "$page" | jq -r '.more')" = true ] || break
  offset=$((offset + 100))
done
if [ "$alerts" = yes ]; then
  jq -r -s -L "$here" -f "$here/pagerduty.jq" --argjson ids true "$all" >"$ids"
  requests=0
  while IFS= read -r id; do
    # An id goes in a path: PagerDuty's are letters and digits.
    case $id in
      '' | *[!A-Za-z0-9]*)
        printf 'pagerduty.sh: an incident id that is not letters and digits; stopping, writing nothing\n' >&2
        exit 1
        ;;
    esac
    offset=0
    while :; do
      requests=$((requests + 1))
      if [ "$requests" -gt "$maxreq" ]; then
        printf 'pagerduty.sh: more than %s requests for alerts; stopping, writing nothing (raise MAX_ALERT_REQUESTS, shorten the period, or set ALERTS=no)\n' "$maxreq" >&2
        exit 1
      fi
      page=$(printf 'Authorization: Token token=%s\n' "$token" |
        curl -q -fsS --proto "$proto" --max-time 30 --max-filesize 16777216 -H @- -H 'Accept: application/vnd.pagerduty+json;version=2' "$api/incidents/$id/alerts?limit=100&offset=$offset")
      printf '%s' "$page" | jq -c --arg id "$id" '{id: $id, alerts: [.alerts[]?]}' >>"$linked"
      [ "$(printf '%s' "$page" | jq -r '.more')" = true ] || break
      offset=$((offset + 100))
    done
  done <"$ids"
fi
jq -s -L "$here" -f "$here/pagerduty.jq" --slurpfile linked "$linked" --arg alert_labels "${ALERT_LABELS:-}" "$all" >"$out"
jq -r -L "$here" 'include "alerts"; said("pagerduty.sh"; "their details carry no alertname")' "$out" >&2
jq '{incidents}' "$out"
