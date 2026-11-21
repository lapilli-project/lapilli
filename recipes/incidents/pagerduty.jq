# PagerDuty's incidents, an array of them as GET /incidents gives them, to
# Lapilli's. Low-urgency incidents page no one, so they are left out.
#
# With --slurpfile linked, each incident's alerts too, as GET
# /incidents/{id}/alerts gives them ({id, alerts} a line). A PagerDuty
# alert is one Alertmanager sent for a group of alerts; its details
# (body.details, or body.cef_details.details) carry each one's labels in
# firing and resolved, as Alertmanager 0.30.0 and later send them by
# default (pagerduty_configs' details), alertname among them; or, from one
# that put alertname there itself, the rule alone, with the labels beside it
# that ALERT_LABELS names (other details may be a description or a
# runbook). Those with neither are left out. With --argjson ids true, the ids of the incidents kept, whose
# alerts are to be read.
include "alerts";
def keep: select(.urgency == "high");
def title: (.title // "")[:120];
def of_labels: {rule: .alertname, labels: del(.alertname)} | as_alert;
def alerts:
  (.body.details // .body.cef_details.details) as $d
  | if ($d | type) != "object" then [null]
    else [($d.firing, $d.resolved) | arrays | .[] | .labels | objects | of_labels] as $each
      | if ($each | length) > 0 then $each
        elif ($d.alertname | type) == "string" then
          # Its other details may be anything (a description, a runbook):
          # labels only those ALERT_LABELS names, else the rule alone.
          [{rule: $d.alertname, labels: (if (kept_names | length) > 0 then $d | del(.alertname, .firing, .resolved, .num_firing, .num_resolved) else {} end)} | as_alert]
        else [null] end
    end;
(($ARGS.named.linked // []) | group_by(.id) | map({key: .[0].id, value: [.[].alerts[]? | alerts[]]}) | from_entries) as $by
| if $ARGS.named.ids then .[] | keep | .id
  else {incidents: [.[] | keep | .id as $id | {
      id: "PD-\(.incident_number)",
      title: title,
      start: .created_at
    }
    + (if .resolved_at then {end: .resolved_at} else {} end)
    + (if .service.summary then {labels: {service: [.service.summary]}} else {} end)
    | with_alerts($by; $id)
  ], alerts_said: alerts_said($by)}
  end
