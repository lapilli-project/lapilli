# Google Cloud Monitoring's alerts, as projects.alerts.list gives them, to
# Lapilli's firings. Each alert is one firing, from when it opened until it
# closed; one still open has no end.
#
# The rule is the policy's display name. For a policy with a PromQL condition
# replayed with --rules, that is its name there only when the condition has no
# alertRule. The labels are the monitored resource's and the metric's, the
# policy's user labels where those don't say, and its severity (critical,
# error, warning); rename them to what --match needs, such as service. The
# policy's name (projects/P/alertPolicies/ID) is alarm_id, by which lapilli
# serve ties a policy it reads with --alarms to its history.
# Seconds since 1970 of an RFC 3339 time, its fraction and zone included, so
# times written differently still sort and compare as times.
def epoch: . as $t
  | (capture("^(?<d>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?<f>\\.[0-9]+)?(?<z>Z|[+-][0-9]{2}:?[0-9]{2})$")
     // error("not a time: \($t)"))
  | ((.d + "Z") | fromdateiso8601) + (("0" + (.f // "")) | tonumber)
    - (if .z == "Z" then 0 else (.z[0:1] + "1" | tonumber) * ((.z[1:3] | tonumber) * 3600 + (.z[-2:] | tonumber) * 60) end);
def lower: ascii_downcase;
# The API is asked for the period, and the period is kept here too: the
# filter's syntax is shown only by example.
($since | epoch) as $from | (if $until == "" then null else $until | epoch end) as $to
| .[] | (.openTime // .open_time) as $open | select($open != null)
| ($open | epoch) as $o | select($o >= $from and ($to == null or $o < $to))
| {rule: (.policy.displayName // .policy.name // "unknown"),
   labels: ((.policy.userLabels // {}) + (.resource.labels // {}) + (.metric.labels // {})
            + (if (.policy.severity // "") | IN("", "SEVERITY_UNSPECIFIED") then {} else {severity: (.policy.severity | lower)} end)
            + (if (.policy.name // "") != "" then {alarm_id: .policy.name} else {} end)
            | map_values(tostring)),
   start: $open}
  + (if (.state // "") == "CLOSED" and (.closeTime // .close_time) then {end: (.closeTime // .close_time)} else {} end)
