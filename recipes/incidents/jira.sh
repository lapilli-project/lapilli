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
# https NAME URL: the scheme curl speaks to URL, in proto. The token goes
# where URL points: over https, or to this machine, for tests. Neither is
# said: a user and password before @ would be in the log.
# The scheme may be in capitals, as curl takes it. This machine is
# 127.0.0.1, [::1] or localhost, with a port or not, and no name that only
# starts with one, such as localhost.example.com.
https() {
  proto=
  case $2 in
    *@*)
      printf 'jira.sh: %s holds a user or token before @, refused\n' "$1" >&2
      exit 1
      ;;
    [Hh][Tt][Tt][Pp][Ss]://*) proto='=https' ;;
    [Hh][Tt][Tt][Pp]://*)
      case ${2#*://} in
        127.0.0.1 | '[::1]' | [Ll][Oo][Cc][Aa][Ll][Hh][Oo][Ss][Tt]) proto='=http' ;;
        127.0.0.1[/:]* | '[::1]'[/:]* | [Ll][Oo][Cc][Aa][Ll][Hh][Oo][Ss][Tt][/:]*) proto='=http' ;;
      esac
      ;;
  esac
  if [ -z "$proto" ]; then
    printf 'jira.sh: %s is not https\n' "$1" >&2
    exit 1
  fi
}
https JIRA_URL "$JIRA_URL"
site=$proto
jsm=${JSM_API:-https://api.atlassian.com}
https JSM_API "$jsm"
jsmproto=$proto
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
# secret NAME FILE: what FILE holds, one line with no space. Atlassian
# says nothing of what its tokens are written in, and this one goes
# base64'd, with the email. Neither the token nor what else the file holds
# is said.
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
    '' | *[[:space:][:cntrl:]]*)
      printf '%s\n' "jira.sh: $1 doesn't hold a token: one line, with no space" >&2
      exit 1
      ;;
  esac
  printf '%s' "$s"
}
token=$(secret JIRA_TOKEN_FILE "$JIRA_TOKEN_FILE")
auth=$(printf '%s:%s' "$JIRA_EMAIL" "$token" | base64 | tr -d '\n')
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
# get PROTO URL WHO: the answer in $got, its status in $code, curl
# speaking PROTO alone; one over 16 MiB stops the script. A 429 is waited
# out as its Retry-After says, two minutes at most, five times a request
# and ten minutes in all a run, Jira's search and Jira Service
# Management's alike; without one, 5 seconds, then twice as long each
# time. WHO is who is said to be asking too often. Called in this shell,
# not in $(...), which would forget the code.
naps=0
waited=0
get() {
  waits=0
  backoff=5
  while :; do
    code=$(printf 'Authorization: Basic %s\n' "$auth" |
      curl -q -sS --proto "$1" --max-time 30 --max-filesize 16777216 -H @- -H 'Accept: application/json' -D "$heads" -o "$got" -w '%{http_code}' "$2") || exit 1
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
      printf 'jira.sh: %s still says too many requests after %s waits, %s seconds in all; stopping, writing nothing\n' "$3" "$naps" "$waited" >&2
      exit 1
    fi
    sleep "$wait"
    naps=$((naps + 1))
    waited=$((waited + wait))
  done
}
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
  get "$site" "$url" Jira
  case $code in
    2??) ;;
    *)
      printf 'jira.sh: Jira'"'"'s search answered %s; writing nothing\n' "$code" >&2
      exit 1
      ;;
  esac
  jq -c '.issues[]' "$got" >>"$all"
  was=$next
  next=$(jq -r 'if .isLast then empty else .nextPageToken // empty end' "$got")
  [ -n "$next" ] || break
  if [ "$next" = "$was" ]; then
    printf 'jira.sh: the API gave the same page again; stopping, writing nothing\n' >&2
    exit 1
  fi
done
if [ "$alerts" = yes ]; then
  requests=0
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
    # The keys of the issue's pages so far: one given again would go round
    # until MAX_ALERT_REQUESTS.
    keys=' '
    while :; do
      url="$jsm/jsm/incidents/cloudId/$cloud/v1/incident/$issue/alert"
      [ -z "$key" ] || url="$url?startKey=$key"
      count
      get "$jsmproto" "$url" "Jira Service Management"
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
      key=$(jq -r '.startKey // empty' "$got")
      [ -n "$key" ] || break
      case $key in
        *[!0-9]*)
          printf 'jira.sh: the incident API gave a next page that is not a number; stopping, writing nothing\n' >&2
          exit 1
          ;;
      esac
      case $keys in
        *" $key "*)
          printf 'jira.sh: the incident API gave a page of alerts it gave before; stopping, writing nothing\n' >&2
          exit 1
          ;;
      esac
      keys="$keys$key "
    done
    while IFS= read -r alert; do
      case $alert in
        '' | *[!A-Za-z0-9-]*)
          printf 'jira.sh: an alert id that is not letters, digits and dashes; stopping, writing nothing\n' >&2
          exit 1
          ;;
      esac
      count
      get "$jsmproto" "$jsm/jsm/ops/api/$cloud/v1/alerts/$alert" "Jira Service Management"
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
