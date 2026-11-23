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
# The scheme may be in capitals, as curl takes it. This machine is
# 127.0.0.1, [::1] or localhost, with a port or not, and no name that only
# starts with one, such as localhost.example.com.
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
    ;;
esac
if [ -z "$proto" ]; then
  printf 'pagerduty.sh: PAGERDUTY_API is not https\n' >&2
  exit 1
fi
: "${PAGERDUTY_TOKEN_FILE:?a file holding a PagerDuty API key}"
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
out=$(mktemp)
got=$(mktemp)
heads=$(mktemp)
failed=$(mktemp)
trap 'rm -f "$all" "$ids" "$linked" "$out" "$got" "$heads" "$failed"' EXIT
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
    held -fsS --proto "$proto" --max-time 30 --max-filesize 16777216 -H @- -H 'Accept: application/vnd.pagerduty+json;version=2' "$url")
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
        held -fsS --proto "$proto" --max-time 30 --max-filesize 16777216 -H @- -H 'Accept: application/vnd.pagerduty+json;version=2' "$api/incidents/$id/alerts?limit=100&offset=$offset")
      printf '%s' "$page" | jq -c --arg id "$id" '{id: $id, alerts: [.alerts[]?]}' >>"$linked"
      [ "$(printf '%s' "$page" | jq -r '.more')" = true ] || break
      offset=$((offset + 100))
    done
  done <"$ids"
fi
jq -s -L "$here" -f "$here/pagerduty.jq" --slurpfile linked "$linked" --arg alert_labels "${ALERT_LABELS:-}" "$all" >"$out"
jq -r -L "$here" 'include "alerts"; said("pagerduty.sh"; "their details carry no alertname")' "$out" >&2
jq '{incidents}' "$out"
