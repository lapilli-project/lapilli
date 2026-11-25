#!/bin/sh
# Writes what PagerDuty says of the pages of a period to stdout, as Lapilli's
# responses file takes it (docs/design/responses.md): each high-urgency
# incident (URGENCY=all: each incident) created in the period is a page, with the alerts it was sent for
# (read as ../incidents/pagerduty.sh reads them), when it was acknowledged
# by a person, and when and by what it resolved. A person is never named.
#
#   PAGERDUTY_TOKEN_FILE  a file holding a read-only REST API key
#   SINCE, UNTIL          the period (ISO 8601 in whole seconds, such as
#                         2026-11-08T00:00:00Z; six months at most), both
#                         needed: put.sh writes the period it was given
#   MAX_PAGES             of 100 incidents, at most (default 1000)
#   ALERT_LABELS          the labels an alert keeps, comma-separated, such as
#                         service,namespace (default all its details give)
#   MAX_REQUESTS          for alerts and log entries, at most (default 4000)
#   URGENCY               high (default) or all. A low-urgency incident
#                         pages no one, so that nobody answered it isn't a
#                         noisy rule's mark; all keeps them too
#
# Who acknowledged and what resolved a page come from the incident's log
# entries (GET /incidents/{id}/log_entries) by their agent's type
# (pagerduty.jq says how).
set -eu
here=$(dirname "$0")
inc="$here/../incidents"
api=${PAGERDUTY_API:-https://api.pagerduty.com}
# The token goes where api points: over https, or to this machine, for
# tests, and curl speaks no other scheme (proto). Neither is said: a user
# and password before @ would be in the log.
# The scheme may be in capitals, as curl takes it. This machine is
# 127.0.0.1, [::1] or localhost, with a port or not, and no name that only
# starts with one, such as localhost.example.com.
# PLAIN_HTTP=true takes an address over plain http, to any host, for a
# self-managed tool reached inside a cluster: the token is read by
# whatever is on the way there. Off by default.
case ${PLAIN_HTTP:-false} in
  true | false) ;;
  *)
    printf '%s: PLAIN_HTTP is true or false\n' "pagerduty.sh" >&2
    exit 1
    ;;
esac
proto=
case $api in
  *@*)
    printf 'pagerduty.sh: PAGERDUTY_API holds a user or token before @, refused\n' >&2
    exit 1
    ;;
  [Hh][Tt][Tt][Pp][Ss]://*) proto='=https' ;;
  [Hh][Tt][Tt][Pp]://*)
    case ${api#*://} in
      127.0.0.1 | '[::1]' | [Ll][Oo][Cc][Aa][Ll][Hh][Oo][Ss][Tt]) proto='=http' ;;
      127.0.0.1[/:]* | '[::1]'[/:]* | [Ll][Oo][Cc][Aa][Ll][Hh][Oo][Ss][Tt][/:]*) proto='=http' ;;
    esac
    [ "${PLAIN_HTTP:-false}" = false ] || proto='=http'
    ;;
esac
if [ -z "$proto" ]; then
  printf 'pagerduty.sh: PAGERDUTY_API is not https (PLAIN_HTTP=true takes plain http, to any host)\n' >&2
  exit 1
fi
: "${PAGERDUTY_TOKEN_FILE:?a file holding a PagerDuty API key}"
: "${SINCE:?the start of the period, such as 2026-11-08T00:00:00Z}"
: "${UNTIL:?the end of the period, such as 2026-11-09T00:00:00Z}"
# The period checked before it reaches a query, and in seconds for the
# incidents' own times.
from=$(printf '%s' "$SINCE" | jq -sRr -L "$here" 'include "responses"; epoch // error("SINCE") | floor') || {
  printf 'pagerduty.sh: SINCE is an RFC 3339 time, such as 2026-11-08T00:00:00Z\n' >&2
  exit 1
}
to=$(printf '%s' "$UNTIL" | jq -sRr -L "$here" 'include "responses"; epoch // error("UNTIL") | floor') || {
  printf 'pagerduty.sh: UNTIL is an RFC 3339 time, such as 2026-11-09T00:00:00Z\n' >&2
  exit 1
}
if [ "$from" -ge "$to" ]; then
  printf 'pagerduty.sh: SINCE is not before UNTIL\n' >&2
  exit 1
fi
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
logs=$(mktemp)
out=$(mktemp)
got=$(mktemp)
heads=$(mktemp)
failed=$(mktemp)
trap 'rm -f "$all" "$ids" "$linked" "$logs" "$out" "$got" "$heads" "$failed"' EXIT
trap 'exit 1' INT TERM HUP
# held CURL-ARGUMENTS: curl's answer on stdout, of a 2xx alone, and 16
# MiB of it at most whatever curl's version (before 8.4.0, --max-filesize
# caps only an answer that says its length first). Past it, with curl
# failing, or of any other answer, a redirect among them, which curl
# doesn't follow, it fails, saying so. curl's stdin is its own, and it
# reads no .curlrc (-q).
held() {
  : >"$failed"
  { curl -q "$@" -D "$heads" -o - || echo x >"$failed"; } | head -c 16777217 >"$got"
  if [ "$(wc -c <"$got")" -gt 16777216 ]; then
    printf 'pagerduty.sh: an answer is more than 16 MiB; writing nothing\n' >&2
    return 1
  fi
  [ ! -s "$failed" ] || return 1
  h_code=$(sed -n 's/^HTTP\/[0-9.]* \([0-9][0-9][0-9]\).*/\1/p' "$heads" | tail -n 1)
  case $h_code in
    2??) cat "$got" ;;
    *)
      printf 'pagerduty.sh: the API answered %s; writing nothing\n' "${h_code:-nothing}" >&2
      return 1
      ;;
  esac
}
urgency=${URGENCY:-high}
case $urgency in
  high | all) ;;
  *)
    printf 'pagerduty.sh: URGENCY is high or all, not %s\n' "$urgency" >&2
    exit 1
    ;;
esac
max=${MAX_PAGES:-1000}
case $max in
  '' | *[!0-9]*)
    printf '%s: MAX_PAGES is a number of pages, not %s\n' "pagerduty.sh" "$max" >&2
    exit 1
    ;;
esac
maxreq=${MAX_REQUESTS:-4000}
case $maxreq in
  '' | *[!0-9]*)
    printf 'pagerduty.sh: MAX_REQUESTS is a number of requests\n' >&2
    exit 1
    ;;
esac
from_q=$(printf '%s' "$SINCE" | jq -sRr @uri)
to_q=$(printf '%s' "$UNTIL" | jq -sRr @uri)
offset=0
pages=0
while :; do
  pages=$((pages + 1))
  if [ "$pages" -gt "$max" ]; then
    printf 'pagerduty.sh: more than %s pages; stopping, writing nothing (raise MAX_PAGES, or shorten the period)\n' "$max" >&2
    exit 1
  fi
  url="$api/incidents?limit=100&offset=$offset&sort_by=created_at:asc&since=$from_q&until=$to_q"
  # The token goes on stdin, not the command line, where ps would show it.
  page=$(printf 'Authorization: Token token=%s\n' "$token" |
    held -fsS --proto "$proto" --max-time 30 --max-filesize 16777216 -H @- -H 'Accept: application/vnd.pagerduty+json;version=2' "$url")
  printf '%s' "$page" | jq -c '.incidents[]' >>"$all"
  [ "$(printf '%s' "$page" | jq -r '.more')" = true ] || break
  offset=$((offset + 100))
done
jq -r -s -L "$here" -L "$inc" -f "$here/pagerduty.jq" --argjson from "$from" --argjson to "$to" --arg urgency "$urgency" --argjson ids true "$all" >"$ids"
requests=0
# pages_of PATH FILE KEY ID: every page of PATH's list (an incident's
# alerts or log entries), each as {id, KEY: [...]} a line in FILE.
pages_of() {
  offset=0
  while :; do
    requests=$((requests + 1))
    if [ "$requests" -gt "$maxreq" ]; then
      printf 'pagerduty.sh: more than %s requests for alerts and log entries; stopping, writing nothing (raise MAX_REQUESTS, or shorten the period)\n' "$maxreq" >&2
      exit 1
    fi
    page=$(printf 'Authorization: Token token=%s\n' "$token" |
      held -fsS --proto "$proto" --max-time 30 --max-filesize 16777216 -H @- -H 'Accept: application/vnd.pagerduty+json;version=2' "$api/incidents/$4/$1?limit=100&offset=$offset")
    printf '%s' "$page" | jq -c --arg id "$4" --arg key "$3" '{id: $id} + {($key): [.[$key][]?]}' >>"$2"
    [ "$(printf '%s' "$page" | jq -r '.more')" = true ] || break
    offset=$((offset + 100))
  done
}
while IFS= read -r id; do
  # An id goes in a path: PagerDuty's are letters and digits.
  case $id in
    '' | *[!A-Za-z0-9]*)
      printf 'pagerduty.sh: an incident id that is not letters and digits; stopping, writing nothing\n' >&2
      exit 1
      ;;
  esac
  pages_of alerts "$linked" alerts "$id"
  pages_of log_entries "$logs" log_entries "$id"
done <"$ids"
jq -s -L "$here" -L "$inc" -f "$here/pagerduty.jq" --argjson from "$from" --argjson to "$to" --arg urgency "$urgency" \
  --slurpfile linked "$linked" --slurpfile logs "$logs" --arg alert_labels "${ALERT_LABELS:-}" "$all" >"$out"
jq -r -L "$here" -L "$inc" 'include "alerts"; said("pagerduty.sh"; "their details carry no alertname"),
  (.said | (select(.noalerts > 0) | "pagerduty.sh: left out \(.noalerts) of the \(.incidents) incidents: no alert Lapilli takes"),
    (select(.unknown > 0) | "pagerduty.sh: \(.unknown) incidents resolved by an agent the log entries do not say: kept with no resolve"))' "$out" >&2
jq '{responses}' "$out"
