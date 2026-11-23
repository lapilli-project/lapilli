#!/bin/sh
# Writes the alerts Google Cloud Monitoring's alerting policies opened to
# stdout, from the Alerts API, as lapilli verdict --firings takes them: one
# firing a line.
#
#   GOOGLE_CLOUD_PROJECT  the project, the scoping project of a metrics scope
#   SINCE, UNTIL          the period (ISO 8601). Closed alerts are kept 13
#                         months.
#   GOOGLE_TOKEN_FILE     a file holding an access token; without one, gcloud
#                         auth print-access-token, signed in however gcloud is
#   MAX_PAGES             of 1000 alerts, at most (default 1000)
#
# It needs monitoring.alerts.list (roles/monitoring.viewer).
set -eu
here=$(dirname "$0")
api=${GOOGLE_MONITORING_API:-https://monitoring.googleapis.com}
# The token goes where api points: over https, or to this machine, for
# tests, and curl speaks no other scheme (proto). Neither is said: a user
# and password before @ would be in the log.
# The scheme may be in capitals, as curl takes it. This machine is
# 127.0.0.1, [::1] or localhost, with a port or not, and no name that only
# starts with one, such as localhost.example.com.
proto=
case $api in
  *@*)
    printf 'google-cloud.sh: GOOGLE_MONITORING_API holds a user or token before @, refused\n' >&2
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
  printf 'google-cloud.sh: GOOGLE_MONITORING_API is not https\n' >&2
  exit 1
fi
: "${GOOGLE_CLOUD_PROJECT:?the project}"
: "${SINCE:?the start of the period, such as 2026-11-01T00:00:00Z}"
case $GOOGLE_CLOUD_PROJECT in
  *[!a-z0-9-]* | '')
    printf 'google-cloud.sh: %s is not a project id\n' "$GOOGLE_CLOUD_PROJECT" >&2
    exit 1
    ;;
esac
for t in "$SINCE" "${UNTIL:-$SINCE}"; do
  # grep reads lines: a second line could carry anything past it.
  c=$t
  case $t in *[!0-9TZ:.+-]* | '') c=x ;; esac
  if ! printf '%s' "$c" | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$'; then
    printf '%s: %s is not a time such as 2026-11-01T00:00:00Z\n' "google-cloud.sh" "$t" >&2
    exit 1
  fi
done
# The token, from its file or as gcloud writes it: one line of the
# characters a token is written in (a Bearer token's, RFC 6750), as it
# goes in a header, where a second line would be a header of its own.
# Spaces, tabs and a carriage return after it, as an editor may leave
# them, are dropped. Neither the token nor what else came with it is said.
if [ -n "${GOOGLE_TOKEN_FILE:-}" ]; then
  token=$(cat "$GOOGLE_TOKEN_FILE")
  held="GOOGLE_TOKEN_FILE doesn't hold"
else
  token=$(gcloud auth print-access-token)
  held="gcloud auth print-access-token didn't write"
fi
while :; do
  case $token in
    *[[:space:]]) token=${token%?} ;;
    *) break ;;
  esac
done
case $token in
  '' | *[!A-Za-z0-9._~+/=-]*)
    printf '%s\n' "google-cloud.sh: $held a token: one line of letters, digits and - . _ ~ + / =" >&2
    exit 1
    ;;
esac
max=${MAX_PAGES:-1000}
case $max in
  '' | *[!0-9]*)
    printf 'google-cloud.sh: MAX_PAGES is a number of pages, not %s\n' "$max" >&2
    exit 1
    ;;
esac
filter="open_time>=\"$SINCE\""
[ -z "${UNTIL:-}" ] || filter="$filter AND open_time<\"$UNTIL\""
filter=$(printf '%s' "$filter" | jq -sRr @uri)
all=$(mktemp)
trap 'rm -f "$all"' EXIT
trap 'exit 1' INT TERM HUP
next=
pages=0
while :; do
  pages=$((pages + 1))
  if [ "$pages" -gt "$max" ]; then
    printf 'google-cloud.sh: more than %s pages; stopping, writing nothing (raise MAX_PAGES, or shorten the period)\n' "$max" >&2
    exit 1
  fi
  url="$api/v3/projects/$GOOGLE_CLOUD_PROJECT/alerts?pageSize=1000&filter=$filter"
  [ -z "$next" ] || url="$url&pageToken=$(printf '%s' "$next" | jq -sRr @uri)"
  # The token goes on stdin, not the command line, where ps would show it.
  # An answer is 16 MiB at most (max-filesize): past it, curl fails, and
  # nothing is written.
  page=$(printf 'Authorization: Bearer %s\n' "$token" | curl -q -fsS --proto "$proto" --max-time 30 --max-filesize 16777216 -H @- "$url")
  printf '%s' "$page" | jq -c '.alerts // [] | .[]' >>"$all"
  last=$next
  next=$(printf '%s' "$page" | jq -r '.nextPageToken // empty')
  [ -n "$next" ] || break
  if [ "$next" = "$last" ]; then
    printf 'google-cloud.sh: the same page again; stopping, writing nothing\n' >&2
    exit 1
  fi
done
jq -c -s --arg since "$SINCE" --arg until "${UNTIL:-}" -f "$here/google-cloud.jq" "$all"
