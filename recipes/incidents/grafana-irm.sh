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
#
# It reads IncidentsService.QueryIncidentPreviews, which Grafana IRM's API
# reference gives in place of QueryIncidents, deprecated there. A preview
# has every field read here; its query has no date to start from, so the
# incidents are asked for newest first (orderField createdTime, DESC) and
# the pages stop at the first that reaches before SINCE. A page that isn't
# newest first, with SINCE, writes nothing, as an incident after it might
# be left out.
#
# It writes no incident's alerts: Grafana IRM's incident API lists none,
# and its OnCall API's alert groups name no incident
# (docs/design/links.md, "Grafana IRM").
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
# PLAIN_HTTP=true takes an address over plain http, to any host, for a
# self-managed tool reached inside a cluster: the token is read by
# whatever is on the way there. Off by default.
case ${PLAIN_HTTP:-false} in
  true | false) ;;
  *)
    printf '%s: PLAIN_HTTP is true or false\n' "grafana-irm.sh" >&2
    exit 1
    ;;
esac
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
    [ "${PLAIN_HTTP:-false}" = false ] || proto='=http'
    ;;
esac
if [ -z "$proto" ]; then
  printf 'grafana-irm.sh: GRAFANA_URL is not https (PLAIN_HTTP=true takes plain http, to any host)\n' >&2
  exit 1
fi
url="${GRAFANA_URL%/}/api/plugins/grafana-irm-app/resources/api/v1/IncidentsService.QueryIncidentPreviews"
# epoch: an RFC 3339 time, or Grafana's own "2026-10-31 03:10:20.574137"
# (UTC), as seconds since 1970; null for anything else, Go's zero time
# among them.
epoch='def epoch: if type == "string" and (startswith("0001-") | not) then
  (capture("\\A(?<d>[0-9]{4}-[0-9]{2}-[0-9]{2})[T ](?<t>[0-9]{2}:[0-9]{2}:[0-9]{2})(?<f>\\.[0-9]+)?(?<z>Z|[+-][0-9]{2}:[0-9]{2})?\\z")
    | ((.d + "T" + .t + "Z") | fromdateiso8601) + (("0" + (.f // "")) | tonumber)
      - (if .z == null or .z == "Z" then 0
         else (if .z[0:1] == "-" then -1 else 1 end) * ((.z[1:3] | tonumber) * 3600 + (.z[4:6] | tonumber) * 60) end))
  // null else null end;'
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
# oldest is when the last incident read was created, in seconds, to check
# the next page goes on from it.
oldest=
while :; do
  pages=$((pages + 1))
  if [ "$pages" -gt "$max" ]; then
    printf 'grafana-irm.sh: more than %s pages; stopping, writing nothing (raise MAX_PAGES, or set SINCE later)\n' "$max" >&2
    exit 1
  fi
  body=$(jq -nc --arg next "$next" \
    '{query: {limit: 100, orderField: "createdTime", orderDirection: "DESC"}}
      + (if $next == "" then {} else {cursor: {nextValue: $next, hasMore: true}} end)')
  page=$(printf 'Authorization: Bearer %s\n' "$token" |
    held -fsS --proto "$proto" --max-time 30 --max-filesize 16777216 -H @- -H 'Content-Type: application/json; charset=utf-8' --data-binary "$body" "$url")
  # The API can answer 200 and say what went wrong.
  err=$(printf '%s' "$page" | jq -r '.error // empty')
  if [ -n "$err" ]; then
    printf 'grafana-irm.sh: Grafana IRM says %s; writing nothing\n' "$(printf '%s' "$err" | jq -Rsc .)" >&2
    exit 1
  fi
  # Those created before SINCE are left out; one whose time doesn't read
  # is kept. With SINCE, the page says whether to go on: "go" or "stop"
  # (it reaches before SINCE), or "bad" (not newest first), and when its
  # last incident was created.
  printf '%s' "$page" | jq -c --arg since "$since" "$epoch"'
    ($since | epoch) as $s
    | (.incidentPreviews // [])[]
    | select($s == null or ((.createdTime | epoch) as $c | $c == null or $c >= $s))' >>"$all"
  if [ -n "$since" ]; then
    said=$(printf '%s' "$page" | jq -r --arg since "$since" --arg oldest "$oldest" "$epoch"'
      ($since | epoch) as $s
      | [(.incidentPreviews // [])[] | .createdTime | epoch | select(. != null)] as $c
      | ((if $oldest == "" then [] else [$oldest | tonumber] end) + $c) as $all
      | if any(range(1; $all | length); $all[.] > $all[. - 1]) then "bad"
        elif ($c | length) == 0 then "go \($oldest)"
        elif $c[-1] < $s then "stop"
        else "go \($c[-1])" end')
    case $said in
      bad)
        printf 'grafana-irm.sh: Grafana IRM gave incidents that are not newest first, as asked; writing nothing (unset SINCE to read them all)\n' >&2
        exit 1
        ;;
      stop) break ;;
    esac
    oldest=${said#go}
    oldest=${oldest# }
  fi
  was=$next
  next=$(printf '%s' "$page" | jq -r 'if .cursor.hasMore then .cursor.nextValue // empty else empty end')
  [ -n "$next" ] || break
  if [ "$next" = "$was" ]; then
    printf 'grafana-irm.sh: the API gave the same page again; stopping, writing nothing\n' >&2
    exit 1
  fi
done
jq -s -f "$here/grafana-irm.jq" "$all"
