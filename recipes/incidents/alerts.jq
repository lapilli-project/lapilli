# What pagerduty.jq, incident-io.jq, rootly.jq, jira.jq and firehydrant.jq
# share to write the alerts a tool linked to an incident as Lapilli takes
# them (docs/design/links.md): each {rule, labels}, the rule by the name its
# alerts carry (alertname) and its Prometheus labels, all or those
# ALERT_LABELS names. Read with include "alerts"; and jq -L DIR.

# label_name: a label's name as Prometheus writes one, 128 bytes at most.
def label_name: type == "string" and test("\\A[a-zA-Z_][a-zA-Z0-9_]{0,127}\\z");

# kept_names: ALERT_LABELS, as --arg alert_labels gives it (jira.jq's
# labels are JIRA_LABELS): the label names an alert keeps, [] for all.
def kept_names: ($ARGS.named.alert_labels // "") | split(",") | map(gsub("\\s"; "")) | map(select(. != ""));

# as_alert: of {rule, labels} worked out of one of a tool's alerts, the alert
# Lapilli takes, or null when it isn't one: no rule's name, or one past 512
# bytes, or past 32 labels. A label whose value isn't text, is empty or is
# past 256 bytes, or whose name isn't a label's, is left out, as are those
# ALERT_LABELS doesn't name.
def as_alert:
  kept_names as $names
  | if (.rule | type) != "string" or .rule == "" or (.rule | utf8bytelength) > 512 then null
    else
      .labels = ([(.labels // {}) | to_entries[]
          | select((.key | label_name) and (.value | type) == "string" and .value != "" and (.value | utf8bytelength) <= 256)
          | select(($names | length) == 0 or (.key | IN($names[])))] | from_entries)
      | if (.labels | length) > 32 then null elif .labels == {} then {rule} else {rule, labels} end
    end;

# go_bytes: the bytes of a list of alerts as Lapilli counts them
# (incident.CheckAlerts): as Go's JSON encoder writes them, <, > and & as they
# are, the newline it ends with not counted. That is not what tojson writes:
# U+2028 and U+2029 are six bytes in Go's and three in jq's, and DEL one in
# Go's and six in jq's. Counted a codepoint at a time, so no jq's way of
# writing one matters.
def go_str: explode | map(
    if . == 34 or . == 92 or . == 8 or . == 9 or . == 10 or . == 12 or . == 13 then 2
    elif . < 32 or . == 8232 or . == 8233 then 6
    elif . < 128 then 1 elif . < 2048 then 2 elif . < 65536 then 3 else 4 end) | add // 0 | . + 2;
def go_alert:
  8 + (.rule | go_str)
  + (if (.labels | length) > 0
     then 12 + ([.labels | to_entries[] | (.key | go_str) + 1 + (.value | go_str)] | add) + ((.labels | length) - 1)
     else 0 end)
  + 1;
def go_bytes: 2 + ([.[] | go_alert] | add // 0) + (if length > 0 then length - 1 else 0 end);

# linked: of an incident's alerts, each as_alert's, what Lapilli takes: each
# once, 100 at most, 16 KB as JSON in all (go_bytes); and how many were left
# out, not alerts Lapilli takes (skipped) or past those bounds (cut).
def linked:
  (map(select(. != null)) | unique) as $all
  | (reduce $all[] as $a ([]; if length < 100 and ((. + [$a]) | go_bytes) <= 16384 then . + [$a] else . end)) as $kept
  | {alerts: $kept, skipped: (map(select(. == null)) | length), cut: (($all | length) - ($kept | length))};

# said($script; $why): the lines for stderr of what was left out, of the
# incidents file's alerts_said: counts alone, nothing a tool wrote.
def said($script; $why):
  .alerts_said // {read: 0, skipped: 0, cut: 0}
  | (select(.skipped > 0) | "\($script): left out \(.skipped) of the \(.read) alerts linked to the incidents: \($why)"),
    (select(.cut > 0) | "\($script): left out \(.cut) alerts past 100 an incident, or 16 KB")
  ;

# with_alerts($by; $key): an incident as Lapilli takes it, with the alerts
# its tool linked to it ($by[$key], each as_alert's), when it has any.
def with_alerts($by; $key):
  (($by[$key] // []) | linked) as $l
  | if ($l.alerts | length) > 0 then . + {alerts: $l.alerts} else . end;

# alerts_said($by): what was read and left out over all the incidents.
def alerts_said($by):
  [$by[] | linked] as $ls
  | {read: ([$by[] | length] | add // 0), skipped: ([$ls[].skipped] | add // 0), cut: ([$ls[].cut] | add // 0)};
