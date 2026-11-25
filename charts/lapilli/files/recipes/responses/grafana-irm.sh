#!/bin/sh
# Writes what Grafana IRM's OnCall says of the pages of a period to stdout, as
# Lapilli's responses file takes it (docs/design/responses.md): each alert
# group created in the period is a page, with the alerts its last alert's
# payload carries (Alertmanager's webhook), when a person acknowledged it,
# and when a person resolved it. A person is never named: acknowledged_by
# and resolved_by are read for whether they are set, and no id is written.
#
#   GRAFANA_ONCALL_API      OnCall's API URL, as its alert groups reference
#                           calls it (API_URL), such as
#                           https://oncall-prod-eu-west-0.grafana.net/oncall
#   GRAFANA_URL             your Grafana stack, such as
#                           https://example.grafana.net, which the reference
#                           has go in the X-Grafana-URL header
#   GRAFANA_IRM_TOKEN_FILE  a file holding a service account token that can
#                           read alert groups
#   SINCE, UNTIL            the period (RFC 3339 in whole seconds, such as
#                           2026-11-08T00:00:00Z), both needed: put.sh writes
#                           the period it was given
#   MAX_PAGES               of 50 alert groups, at most (default 1000)
#
# What a group resolved by its alert source looks like isn't in the
# reference: resolved_by is the id of a user, or null, and null is also what
# a group resolved by its escalation chain's last step, by stopping
# maintenance or wiped has. So only a resolve by a person is written; the
# others are kept with no resolve, and none is written as resolved by the
# alert (grafana-irm.jq says why).
set -eu
here=$(dirname "$0")
inc="$here/../incidents"
: "${GRAFANA_ONCALL_API:?the OnCall API URL, such as https://oncall-prod-eu-west-0.grafana.net/oncall}"
: "${GRAFANA_URL:?your Grafana stack, such as https://example.grafana.net}"
: "${GRAFANA_IRM_TOKEN_FILE:?a file holding a Grafana service account token}"
: "${SINCE:?the start of the period, such as 2026-11-08T00:00:00Z}"
: "${UNTIL:?the end of the period, such as 2026-11-09T00:00:00Z}"
# The period checked before it reaches a query, and in seconds.
from=$(printf '%s' "$SINCE" | jq -sRr -L "$here" 'include "responses"; epoch // error("SINCE") | floor') || {
  printf 'grafana-irm.sh: SINCE is an RFC 3339 time, such as 2026-11-08T00:00:00Z\n' >&2
  exit 1
}
to=$(printf '%s' "$UNTIL" | jq -sRr -L "$here" 'include "responses"; epoch // error("UNTIL") | floor') || {
  printf 'grafana-irm.sh: UNTIL is an RFC 3339 time, such as 2026-11-09T00:00:00Z\n' >&2
  exit 1
}
if [ "$from" -ge "$to" ] || [ "$from" -lt 0 ]; then
  printf 'grafana-irm.sh: SINCE is not before UNTIL\n' >&2
  exit 1
fi
# OnCall's started_at range, UTC alone, to the second.
range=$(jq -nr --argjson from "$from" --argjson to "$to" '"\($from | strftime("%Y-%m-%dT%H:%M:%S"))_\($to - 1 | strftime("%Y-%m-%dT%H:%M:%S"))"')
# The token goes where GRAFANA_ONCALL_API points: over https, or to this
# machine, for tests, and curl speaks no other scheme (proto). Neither is
# said: a user and password before @ would be in the log.
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
api=${GRAFANA_ONCALL_API%/}
case $api in
  *@*)
    printf 'grafana-irm.sh: GRAFANA_ONCALL_API holds a user or token before @, refused\n' >&2
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
  printf 'grafana-irm.sh: GRAFANA_ONCALL_API is not https (PLAIN_HTTP=true takes plain http, to any host)\n' >&2
  exit 1
fi
# The stack goes in a header, as it is written: a URL with no space, control
# character or user in it.
case $GRAFANA_URL in
  *@* | *[[:space:][:cntrl:]]*)
    printf 'grafana-irm.sh: GRAFANA_URL is a URL with no user, space or control character\n' >&2
    exit 1
    ;;
  [Hh][Tt][Tt][Pp][Ss]://?*) ;;
  *)
    printf 'grafana-irm.sh: GRAFANA_URL is not https\n' >&2
    exit 1
    ;;
esac
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
max=${MAX_PAGES:-1000}
case $max in
  '' | *[!0-9]*)
    printf '%s: MAX_PAGES is a number of pages, not %s\n' "grafana-irm.sh" "$max" >&2
    exit 1
    ;;
esac
all=$(mktemp)
out=$(mktemp)
got=$(mktemp)
heads=$(mktemp)
failed=$(mktemp)
trap 'rm -f "$all" "$out" "$got" "$heads" "$failed"' EXIT
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
# The pages are numbered, as the engine's paginator numbers them (page, 50
# to a page), and the last says next is null.
page=0
while :; do
  page=$((page + 1))
  if [ "$page" -gt "$max" ]; then
    printf 'grafana-irm.sh: more than %s pages; stopping, writing nothing (raise MAX_PAGES, or shorten the period)\n' "$max" >&2
    exit 1
  fi
  url="$api/api/v1/alert_groups/?started_at=$range&page=$page"
  # The token goes on stdin, not the command line, where ps would show it.
  body=$(printf 'Authorization: Bearer %s\nX-Grafana-URL: %s\n' "$token" "$GRAFANA_URL" |
    held -fsS --proto "$proto" --max-time 30 --max-filesize 16777216 -H @- -H 'Content-Type: application/json' "$url")
  # An answer with no list of alert groups is not an empty one.
  if ! printf '%s' "$body" | jq -e '.results | type == "array"' >/dev/null 2>&1; then
    printf 'grafana-irm.sh: OnCall answered with no list of alert groups; writing nothing\n' >&2
    exit 1
  fi
  printf '%s' "$body" | jq -c '.results[]' >>"$all"
  [ -n "$(printf '%s' "$body" | jq -r '.next // empty')" ] || break
done
jq -s -L "$here" -L "$inc" -f "$here/grafana-irm.jq" --argjson from "$from" --argjson to "$to" "$all" >"$out"
jq -r '.said | (select(.noalerts > 0) | "grafana-irm.sh: left out \(.noalerts) of the \(.groups) alert groups: their last alert'"'"'s payload carries no alert with an alertname"),
  (select(.unsettled > 0) | "grafana-irm.sh: \(.unsettled) resolved alert groups have no user who resolved them, which does not say the alert source did: kept with no resolve")' "$out" >&2
jq '{responses}' "$out"
