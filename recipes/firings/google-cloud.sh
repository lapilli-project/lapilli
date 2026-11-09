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
# The token goes where api points: over https, or to this machine, for tests.
case $api in
  https://* | http://127.0.0.1:* | http://localhost:*) ;;
  *)
    printf 'google-cloud.sh: GOOGLE_MONITORING_API %s is not https\n' "$api" >&2
    exit 1
    ;;
esac
: "${GOOGLE_CLOUD_PROJECT:?the project}"
: "${SINCE:?the start of the period, such as 2026-11-01T00:00:00Z}"
case $GOOGLE_CLOUD_PROJECT in
  *[!a-z0-9-]* | '')
    printf 'google-cloud.sh: %s is not a project id\n' "$GOOGLE_CLOUD_PROJECT" >&2
    exit 1
    ;;
esac
for t in "$SINCE" "${UNTIL:-$SINCE}"; do
  if ! printf '%s' "$t" | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$'; then
    printf '%s: %s is not a time such as 2026-11-01T00:00:00Z\n' "google-cloud.sh" "$t" >&2
    exit 1
  fi
done
if [ -n "${GOOGLE_TOKEN_FILE:-}" ]; then
  token=$(cat "$GOOGLE_TOKEN_FILE")
else
  token=$(gcloud auth print-access-token)
fi
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
  page=$(printf 'Authorization: Bearer %s\n' "$token" | curl -fsS --max-time 30 -H @- "$url")
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
