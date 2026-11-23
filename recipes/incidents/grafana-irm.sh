#!/bin/sh
# Writes Grafana IRM's incidents to stdout, as Lapilli's incidents file takes
# them. See grafana-irm.jq for the names to change for your organisation.
#
#   GRAFANA_URL             your Grafana stack, such as https://example.grafana.net
#   GRAFANA_IRM_TOKEN_FILE  a file holding a service account token that can
#                           read incidents
#   SINCE                   only incidents created on or after this: a date
#                           such as 2026-10-01, or an RFC 3339 time
#   MAX_PAGES               of 100 incidents, at most (default 1000)
set -eu
here=$(dirname "$0")
: "${GRAFANA_URL:?your Grafana stack, such as https://example.grafana.net}"
: "${GRAFANA_IRM_TOKEN_FILE:?a file holding a Grafana service account token}"
# SINCE checked before it reaches a query, and written as RFC 3339.
since=
[ -z "${SINCE:-}" ] || since=$(printf '%s' "$SINCE" | jq -sRr '
  if test("\\A[0-9]{4}-[0-9]{2}-[0-9]{2}\\z") then . + "T00:00:00Z"
  elif test("\\A[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})\\z") then .
  else error("SINCE") end') || {
  printf 'grafana-irm.sh: SINCE is a date such as 2026-10-01, or an RFC 3339 time\n' >&2
  exit 1
}
# The token goes where GRAFANA_URL points: over https, or to this machine, for
# tests, and curl speaks no other scheme (proto). Neither is said: a user
# and password before @ would be in the log.
# The scheme may be in capitals, as curl takes it. This machine is
# 127.0.0.1, [::1] or localhost, with a port or not, and no name that only
# starts with one, such as localhost.example.com.
proto=
case $GRAFANA_URL in
  *@*)
    printf 'grafana-irm.sh: GRAFANA_URL holds a user or token before @, refused\n' >&2
    exit 1
    ;;
  [Hh][Tt][Tt][Pp][Ss]://*) proto='=https' ;;
  [Hh][Tt][Tt][Pp]://*)
    case ${GRAFANA_URL#*://} in
      127.0.0.1 | '[::1]' | [Ll][Oo][Cc][Aa][Ll][Hh][Oo][Ss][Tt]) proto='=http' ;;
      127.0.0.1[/:]* | '[::1]'[/:]* | [Ll][Oo][Cc][Aa][Ll][Hh][Oo][Ss][Tt][/:]*) proto='=http' ;;
    esac
    ;;
esac
if [ -z "$proto" ]; then
  printf 'grafana-irm.sh: GRAFANA_URL is not https\n' >&2
  exit 1
fi
url="${GRAFANA_URL%/}/api/plugins/grafana-irm-app/resources/api/v1/IncidentsService.QueryIncidents"
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
      printf '%s\n' "grafana-irm.sh: $1 doesn't hold a token: one line of letters, digits and - . _ ~ + / =" >&2
      exit 1
      ;;
  esac
  printf '%s' "$s"
}
token=$(secret GRAFANA_IRM_TOKEN_FILE "$GRAFANA_IRM_TOKEN_FILE")
all=$(mktemp)
got=$(mktemp)
heads=$(mktemp)
failed=$(mktemp)
trap 'rm -f "$all" "$got" "$heads" "$failed"' EXIT
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
    printf 'grafana-irm.sh: an answer is more than 16 MiB; writing nothing\n' >&2
    return 1
  fi
  [ ! -s "$failed" ] || return 1
  h_code=$(sed -n 's/^HTTP\/[0-9.]* \([0-9][0-9][0-9]\).*/\1/p' "$heads" | tail -n 1)
  case $h_code in
    2??) cat "$got" ;;
    *)
      printf 'grafana-irm.sh: the API answered %s; writing nothing\n' "${h_code:-nothing}" >&2
      return 1
      ;;
  esac
}
max=${MAX_PAGES:-1000}
case $max in
  '' | *[!0-9]*)
    printf '%s: MAX_PAGES is a number of pages, not %s\n' "grafana-irm.sh" "$max" >&2
    exit 1
    ;;
esac
next=
pages=0
while :; do
  pages=$((pages + 1))
  if [ "$pages" -gt "$max" ]; then
    printf 'grafana-irm.sh: more than %s pages; stopping, writing nothing (raise MAX_PAGES, or set SINCE later)\n' "$max" >&2
    exit 1
  fi
  body=$(jq -nc --arg from "$since" --arg next "$next" \
    '{query: ({limit: 100, orderDirection: "ASC"} + (if $from == "" then {} else {dateFrom: $from} end))}
      + (if $next == "" then {} else {cursor: {nextValue: $next, hasMore: true}} end)')
  page=$(printf 'Authorization: Bearer %s\n' "$token" |
    held -fsS --proto "$proto" --max-time 30 --max-filesize 16777216 -H @- -H 'Content-Type: application/json; charset=utf-8' --data-binary "$body" "$url")
  # The API can answer 200 and say what went wrong.
  err=$(printf '%s' "$page" | jq -r '.error // empty')
  if [ -n "$err" ]; then
    printf 'grafana-irm.sh: Grafana IRM says %s; writing nothing\n' "$(printf '%s' "$err" | jq -Rsc .)" >&2
    exit 1
  fi
  printf '%s' "$page" | jq -c '(.incidents // [])[]' >>"$all"
  was=$next
  next=$(printf '%s' "$page" | jq -r 'if .cursor.hasMore then .cursor.nextValue // empty else empty end')
  [ -n "$next" ] || break
  if [ "$next" = "$was" ]; then
    printf 'grafana-irm.sh: the API gave the same page again; stopping, writing nothing\n' >&2
    exit 1
  fi
done
jq -s -f "$here/grafana-irm.jq" "$all"
