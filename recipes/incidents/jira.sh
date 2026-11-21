#!/bin/sh
# Writes the Jira issues a search finds to stdout, as Lapilli's incidents file
# takes them. Works for Jira Service Management's incidents, and for
# anything else kept in Jira Cloud. By default an issue's start is when it
# was created, its end its resolution date, and its components the label
# service; where your incidents keep these in fields of their own, say so:
#
#   JIRA_URL          such as https://example.atlassian.net
#   JIRA_EMAIL        the account's email
#   JIRA_TOKEN_FILE   a file holding its API token
#   JIRA_JQL          such as: project = OPS AND issuetype = Incident AND created >= -30d
#   JIRA_START_FIELD  the field an incident starts at, such as
#                     customfield_10001 (default created); several,
#                     comma-separated, are tried in turn
#   JIRA_END_FIELD    the same for its end (default resolutiondate)
#   JIRA_LABELS       labels from fields (default service=components), such
#                     as service=components,team=customfield_10004; empty
#                     for none; 32 labels at most
#   MAX_PAGES         of 100 issues, at most (default 1000)
#
# For Jira Service Management's incidents, the alerts linked to each that
# Alertmanager sent, with its labels in their extra properties, are its
# alerts (docs/design/links.md): from JSM's incident API, which is in beta,
# and its operations API, as the same account.
#
#   JSM_CLOUD_ID        the site's cloud id (not the organisation's id)
#   ALERTS              yes (the default, with JSM_CLOUD_ID) to read each
#                       incident's alerts, or no
#   ALERT_LABELS        the labels an alert keeps, comma-separated, such as
#                       service,namespace (default all its extra properties
#                       give)
#   MAX_ALERT_REQUESTS  for alerts, at most (default 2000): one a page of an
#                       incident's, and one an alert
#
# A date is never taken for a time, alone or with something after it that
# isn't one (jira.jq); a later field's time counts only on its day. An issue
# whose start isn't a time is skipped; one whose end isn't is kept with no
# end; label values serve would refuse are left out. Each is said on stderr
# by its key, with a count of them by why. When every issue found is
# skipped, it writes nothing and exits 1.
set -eu
here=$(dirname "$0")
: "${JIRA_URL:?such as https://example.atlassian.net}"
: "${JIRA_EMAIL:?the email of the account}"
: "${JIRA_TOKEN_FILE:?a file holding its API token}"
: "${JIRA_JQL:?the search that finds your incidents}"
start=${JIRA_START_FIELD:-created}
end=${JIRA_END_FIELD:-resolutiondate}
labels=${JIRA_LABELS-service=components}
# The settings checked, and only the fields they name asked for.
fields=$(jq -nr -L "$here" --arg start "$start" --arg end "$end" --arg labels "$labels" -f "$here/jira.jq") || exit 1
cloud=${JSM_CLOUD_ID:-}
alerts=${ALERTS:-}
if [ -z "$alerts" ]; then
  alerts=no
  [ -z "$cloud" ] || alerts=yes
fi
maxreq=${MAX_ALERT_REQUESTS:-2000}
case $alerts in
  yes | no) ;;
  *)
    printf 'jira.sh: ALERTS is yes or no\n' >&2
    exit 1
    ;;
esac
case $alerts$cloud in
  yes)
    printf 'jira.sh: ALERTS=yes reads Jira Service Management incidents'"'"' alerts, and needs JSM_CLOUD_ID, the site'"'"'s cloud id\n' >&2
    exit 1
    ;;
esac
case $cloud in
  *[!0-9a-fA-F-]*)
    printf 'jira.sh: JSM_CLOUD_ID is a cloud id: hex digits and dashes\n' >&2
    exit 1
    ;;
esac
case $maxreq in
  '' | *[!0-9]*)
    printf 'jira.sh: MAX_ALERT_REQUESTS is a number of requests\n' >&2
    exit 1
    ;;
esac
auth=$(printf '%s:%s' "$JIRA_EMAIL" "$(cat "$JIRA_TOKEN_FILE")" | base64 | tr -d '\n')
jql=$(printf '%s' "$JIRA_JQL" | jq -sRr @uri)
all=$(mktemp)
out=$(mktemp)
ids=$(mktemp)
alertIDs=$(mktemp)
linked=$(mktemp)
got=$(mktemp)
heads=$(mktemp)
trap 'rm -f "$all" "$out" "$ids" "$alertIDs" "$linked" "$got" "$heads"' EXIT
trap 'exit 1' INT TERM HUP
max=${MAX_PAGES:-1000}
case $max in
  '' | *[!0-9]*)
    printf '%s: MAX_PAGES is a number of pages, not %s\n' "jira.sh" "$max" >&2
    exit 1
    ;;
esac
next=
pages=0
while :; do
  pages=$((pages + 1))
  if [ "$pages" -gt "$max" ]; then
    printf 'jira.sh: more than %s pages; stopping, writing nothing (raise MAX_PAGES, or narrow JIRA_JQL)\n' "$max" >&2
    exit 1
  fi
  url="$JIRA_URL/rest/api/3/search/jql?jql=$jql&maxResults=100&fields=$fields"
  [ -z "$next" ] || url="$url&nextPageToken=$(printf '%s' "$next" | jq -sRr @uri)"
  page=$(printf 'Authorization: Basic %s\n' "$auth" | curl -q -fsS --max-time 30 -H @- -H 'Accept: application/json' "$url")
  printf '%s' "$page" | jq -c '.issues[]' >>"$all"
  was=$next
  next=$(printf '%s' "$page" | jq -r 'if .isLast then empty else .nextPageToken // empty end')
  [ -n "$next" ] || break
  if [ "$next" = "$was" ]; then
    printf 'jira.sh: the API gave the same page again; stopping, writing nothing\n' >&2
    exit 1
  fi
done
if [ "$alerts" = yes ]; then
  jsm=${JSM_API:-https://api.atlassian.com}
  requests=0
  naps=0
  waited=0
  notincident=0
  gone=0
  # count: one request more for alerts, past MAX_ALERT_REQUESTS a stop.
  count() {
    requests=$((requests + 1))
    if [ "$requests" -gt "$maxreq" ]; then
      printf 'jira.sh: more than %s requests for alerts; stopping, writing nothing (raise MAX_ALERT_REQUESTS, narrow JIRA_JQL, or set ALERTS=no)\n' "$maxreq" >&2
      exit 1
    fi
  }
  # get URL: JSM's answer in $got, its status in $code. A 429 is waited
  # out as its Retry-After says, two minutes at most, five times a request
  # and ten minutes in all a run; without one, 5 seconds, then twice as
  # long each time. Called in this shell, not in $(...), which would
  # forget the code.
  get() {
    waits=0
    backoff=5
    while :; do
      code=$(printf 'Authorization: Basic %s\n' "$auth" |
        curl -q -sS --max-time 30 -H @- -H 'Accept: application/json' -D "$heads" -o "$got" -w '%{http_code}' "$1") || exit 1
      [ "$code" = 429 ] || return 0
      waits=$((waits + 1))
      wait=$(tr -d '\r' <"$heads" | awk 'tolower($1) == "retry-after:" { v = $2 } END { print v }')
      case $wait in
        '' | *[!0-9]*)
          wait=$backoff
          backoff=$((backoff * 2))
          ;;
      esac
      # Leading zeros off: $((...)) would read 08 as octal, and fail.
      wait=${wait#"${wait%%[!0]*}"}
      wait=${wait:-0}
      [ "${#wait}" -le 3 ] && [ "$wait" -le 120 ] || wait=120
      if [ "$waits" -gt 5 ] || [ $((waited + wait)) -gt 600 ]; then
        printf 'jira.sh: Jira Service Management still says too many requests after %s waits, %s seconds in all; stopping, writing nothing\n' "$naps" "$waited" >&2
        exit 1
      fi
      sleep "$wait"
      naps=$((naps + 1))
      waited=$((waited + wait))
    done
  }
  jq -r -s -L "$here" --arg start "$start" --arg end "$end" --arg labels "$labels" --argjson ids true -f "$here/jira.jq" "$all" >"$ids"
  while IFS= read -r issue; do
    # Ids go in a path: an issue's are digits, an alert's letters, digits
    # and dashes.
    case $issue in
      '' | *[!0-9]*)
        printf 'jira.sh: an issue id that is not digits; stopping, writing nothing\n' >&2
        exit 1
        ;;
    esac
    : >"$alertIDs"
    key=
    while :; do
      url="$jsm/jsm/incidents/cloudId/$cloud/v1/incident/$issue/alert"
      [ -z "$key" ] || url="$url?startKey=$key"
      count
      get "$url"
      case $code in
        2??) ;;
        400)
          # As the API says of an issue that isn't an incident.
          notincident=$((notincident + 1))
          break
          ;;
        *)
          printf 'jira.sh: Jira Service Management'"'"'s incident API answered %s for the alerts of issue %s; writing nothing (is JSM_CLOUD_ID this site'"'"'s, and can the account see its incidents?)\n' "$code" "$issue" >&2
          exit 1
          ;;
      esac
      if ! jq -e 'type == "object" and ((.values.alertIds // []) | type == "array" and all(type == "string"))' "$got" >/dev/null 2>&1; then
        printf 'jira.sh: the incident API answered something that is not a list of alert ids; writing nothing\n' >&2
        exit 1
      fi
      jq -r '(.values.alertIds // [])[]' "$got" >>"$alertIDs"
      was=$key
      key=$(jq -r '.startKey // empty' "$got")
      [ -n "$key" ] || break
      case $key in
        *[!0-9]*)
          printf 'jira.sh: the incident API gave a next page that is not a number; stopping, writing nothing\n' >&2
          exit 1
          ;;
      esac
      if [ "$key" = "$was" ]; then
        printf 'jira.sh: the incident API gave the same page of alerts again; stopping, writing nothing\n' >&2
        exit 1
      fi
    done
    while IFS= read -r alert; do
      case $alert in
        '' | *[!A-Za-z0-9-]*)
          printf 'jira.sh: an alert id that is not letters, digits and dashes; stopping, writing nothing\n' >&2
          exit 1
          ;;
      esac
      count
      get "$jsm/jsm/ops/api/$cloud/v1/alerts/$alert"
      case $code in
        2??) ;;
        404)
          # Deleted since it was linked.
          gone=$((gone + 1))
          continue
          ;;
        *)
          printf 'jira.sh: Jira Service Management'"'"'s operations API answered %s for an alert of issue %s; writing nothing (can the account see its alerts?)\n' "$code" "$issue" >&2
          exit 1
          ;;
      esac
      if ! jq -e 'type == "object"' "$got" >/dev/null 2>&1; then
        printf 'jira.sh: the operations API answered something that is not an alert; writing nothing\n' >&2
        exit 1
      fi
      jq -c --arg id "$issue" '{id: $id, alerts: [.]}' "$got" >>"$linked"
    done <"$alertIDs"
  done <"$ids"
  [ "$notincident" = 0 ] || printf 'jira.sh: %s of the issues are not incidents, as the incident API says: their alerts were not read\n' "$notincident" >&2
  [ "$gone" = 0 ] || printf 'jira.sh: left out %s alerts linked to the incidents that the operations API no longer has\n' "$gone" >&2
fi
jq -s -L "$here" --arg start "$start" --arg end "$end" --arg labels "$labels" \
  --slurpfile linked "$linked" --arg alert_labels "${ALERT_LABELS:-}" -f "$here/jira.jq" "$all" >"$out"
jq -r -L "$here" 'include "alerts"; said("jira.sh"; "their extra properties carry no alertname")' "$out" >&2
# Keys come from Jira: shown as JSON, so nothing in one acts on a terminal.
# Titles aren't shown.
jq -r 'def by: [group_by(.why)[] | "\(.[0].why) (\(length))"] | join("; ");
  (.skipped[] | "jira.sh: \(.key | tojson) skipped: \(.why)"),
  (.noend[] | "jira.sh: \(.key | tojson) kept with no end, taken to last an hour: \(.why)"),
  (.cut[] | "jira.sh: \(.key | tojson) kept with label values left out: \(.why)"),
  (select(.skipped != []) | "jira.sh: skipped \(.skipped | length) of \(.found) issues: \(.skipped | by)"),
  (select(.noend != []) | "jira.sh: kept \(.noend | length) of \(.found) issues with no end: \(.noend | by)")' "$out" >&2
if [ "$(jq '.found > 0 and .incidents == []' "$out")" = true ]; then
  printf 'jira.sh: every issue found was skipped; writing nothing (see JIRA_START_FIELD, or narrow JIRA_JQL to issues whose start is filled in)\n' >&2
  exit 1
fi
jq '{incidents}' "$out"
