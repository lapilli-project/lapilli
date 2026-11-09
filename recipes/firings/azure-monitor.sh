#!/bin/sh
# Writes the alerts Azure Monitor fired to stdout, from Azure Resource Graph,
# as lapilli verdict --firings takes them: one firing a line. It uses the Azure
# CLI, signed in however it is, with its resource-graph extension.
#
#   SINCE, UNTIL    the period (ISO 8601; UNTIL defaults to now). Azure keeps
#                   alerts for 30 days.
#   SUBSCRIPTIONS   which subscriptions, space-separated (default: all those
#                   the CLI can read)
#   MAX_PAGES       of 1000 alerts, at most (default 1000)
set -eu
here=$(dirname "$0")
: "${SINCE:?the start of the period, such as 2026-11-01T00:00:00Z}"
until=${UNTIL:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
# The times go into the query, so only a time may: in UTC, as Resource Graph's
# documentation writes them.
for t in "$SINCE" "$until"; do
  if ! printf '%s' "$t" | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z$'; then
    printf 'azure-monitor.sh: %s is not a time in UTC such as 2026-11-01T00:00:00Z\n' "$t" >&2
    exit 1
  fi
done
max=${MAX_PAGES:-1000}
case $max in
  '' | *[!0-9]*)
    printf 'azure-monitor.sh: MAX_PAGES is a number of pages, not %s\n' "$max" >&2
    exit 1
    ;;
esac
# Every column a string, and an order: Resource Graph gives no skip token for
# a query whose columns are all dynamic.
query="alertsmanagementresources
| where type =~ 'microsoft.alertsmanagement/alerts'
| extend e = properties.essentials
| where todatetime(e.startDateTime) >= datetime($SINCE) and todatetime(e.startDateTime) < datetime($until)
| project id = tostring(id), rule = tostring(e.alertRule), severity = tostring(e.severity),
    service = tostring(e.monitorService),
    condition = tostring(e.monitorCondition), state = tostring(e.alertState),
    start = tostring(e.startDateTime), resolved = tostring(e.monitorConditionResolvedDateTime),
    modified = tostring(e.lastModifiedDateTime), resource = tostring(e.targetResourceName),
    resourceGroup = tostring(e.targetResourceGroup), signal = tostring(e.signalType),
    dimensions = tostring(properties.context.context.condition.allOf[0].dimensions)
| order by start asc, id asc"
all=$(mktemp)
page=$(mktemp)
trap 'rm -f "$all" "$page"' EXIT
trap 'exit 1' INT TERM HUP
skip=
pages=0
while :; do
  pages=$((pages + 1))
  if [ "$pages" -gt "$max" ]; then
    printf 'azure-monitor.sh: more than %s pages; stopping, writing nothing (raise MAX_PAGES, or shorten the period)\n' "$max" >&2
    exit 1
  fi
  set -- --first 1000
  [ -z "$skip" ] || set -- "$@" --skip-token "$skip"
  # shellcheck disable=SC2086 # SUBSCRIPTIONS is a list, split on purpose
  [ -z "${SUBSCRIPTIONS:-}" ] || set -- "$@" --subscriptions $SUBSCRIPTIONS
  az graph query -q "$query" "$@" --output json >"$page"
  jq -c '.data[]' "$page" >>"$all"
  next=$(jq -r '.skip_token // .skipToken // ."$skipToken" // empty' "$page")
  if [ -z "$next" ]; then
    break
  fi
  if [ "$next" = "$skip" ]; then
    printf 'azure-monitor.sh: the same page again; stopping, writing nothing\n' >&2
    exit 1
  fi
  skip=$next
done
jq -c -s -f "$here/azure-monitor.jq" "$all"
