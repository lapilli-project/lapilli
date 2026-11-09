#!/bin/sh
# Writes when Amazon CloudWatch alarms were in ALARM to stdout, from their
# history, as lapilli verdict --firings takes them: one firing a line. It uses
# the AWS CLI, signed in however it is.
#
#   SINCE, UNTIL  the period (ISO 8601; UNTIL defaults to now). CloudWatch
#                 keeps 30 days of history.
#   AWS_REGION    or the AWS CLI's own configuration
#
# It needs cloudwatch:DescribeAlarmHistory and cloudwatch:DescribeAlarms.
set -eu
here=$(dirname "$0")
: "${SINCE:?the start of the period, such as 2026-11-01T00:00:00Z}"
until=${UNTIL:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
for t in "$SINCE" "${UNTIL:-$SINCE}"; do
  # grep reads lines: a second line could carry anything past it.
  c=$t
  case $t in *[!0-9TZ:.+-]* | '') c=x ;; esac
  if ! printf '%s' "$c" | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$'; then
    printf '%s: %s is not a time such as 2026-11-01T00:00:00Z\n' "cloudwatch.sh" "$t" >&2
    exit 1
  fi
done
history=$(mktemp)
alarms=$(mktemp)
trap 'rm -f "$history" "$alarms"' EXIT
trap 'exit 1' INT TERM HUP
# To files first: in a pipe, an aws that failed would go unnoticed. The CLI
# goes through every page itself.
aws cloudwatch describe-alarm-history --history-item-type StateUpdate --alarm-types MetricAlarm CompositeAlarm \
  --start-date "$SINCE" --end-date "$until" --scan-by TimestampAscending --output json >"$history"
aws cloudwatch describe-alarms --alarm-types MetricAlarm CompositeAlarm --output json >"$alarms"
jq -c -n --slurpfile history "$history" --slurpfile alarms "$alarms" -f "$here/cloudwatch.jq"
