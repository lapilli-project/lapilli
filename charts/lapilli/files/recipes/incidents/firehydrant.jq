# FireHydrant's incidents, an array of them as GET /v1/incidents gives them,
# to Lapilli's. Archived incidents are left out. An incident starts when it
# started, or else when it was opened, and ends at its resolved milestone,
# or else its mitigated one: read from its lifecycle phases, or from the
# older milestones list. Its services become the label service. An incident
# on two pages, as numbered pages with no order can give, is one.
#
# With --slurpfile linked, each incident's alerts too ({id, alerts} a line),
# as GET /v1/incidents/{incident_id}/alerts gives them: an alert's labels
# are arbitrary pairs, and FireHydrant's Alertmanager source names the
# labels its group's alerts share alertmanager/labels-NAME (on the event
# the alert comes from). Both are read, the prefix taken off: alertname is
# the rule, the others its labels; one with no alertname is left out. With
# --argjson ids true, the ids of the incidents kept, whose alerts are to be
# read.
include "alerts";
def at($slug): first(
  (.lifecycle_phases[]?.milestones[]? | select(.slug == $slug) | .occurred_at),
  (.milestones[]? | select(.type == $slug) | .occurred_at)
  | select(. != null and . != ""));
def keep: select(.discarded_at == null);
def alert:
  .labels as $l
  | if ($l | type) != "object" then null
    else ($l | with_entries(.key |= sub("\\Aalertmanager/labels-"; ""))) as $l
      | {rule: $l.alertname, labels: ($l | del(.alertname))} | as_alert end;
(($ARGS.named.linked // []) | group_by(.id) | map({key: .[0].id, value: [.[].alerts[]? | alert]}) | from_entries) as $by
| if $ARGS.named.ids then unique_by(.id)[] | keep | .id
  else {incidents: [unique_by(.id)[]
  | keep
  | .id as $id
  | [.services[]?.name | select(. != null and . != "")] as $services
  | (at("resolved") // at("mitigated") // null) as $end
  | {
      id: "FH-\(.number // .id)",
      title: (.name // "")[:120],
      start: (.started_at // .created_at)
    }
    + (if $end then {end: $end} else {} end)
    + (if ($services | length) > 0 then {labels: {service: $services}} else {} end)
    | with_alerts($by; $id)
  ], alerts_said: alerts_said($by)}
  end
