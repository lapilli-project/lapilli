# What jsm.sh and compass.sh share: Jira Service Management's alerts and
# Compass's, which are the same operations API's (Opsgenie's successor), as
# GET /v1/alerts gives them, to Lapilli's responses (docs/design/
# responses.md). Run with jq -f ops.jq, --arg prefix (JSM or CP) and -L
# ../incidents for alerts.jq. It is a program, not a module: jq 1.8.2
# asserts on a function that reads $ARGS two includes down (alerts.jq's
# kept_names, through a module of this one's), so it is read once, as
# pagerduty.jq is.
#
# An alert is a page, sent when it was created. Its alert is the one its
# extra properties carry, as Alertmanager's Opsgenie notifier puts them: the
# rule is alertname, the labels the others (../incidents/jira.jq reads them
# so); one with no alertname is left out.
#
# Who acted:
#   - acknowledged: Atlassian's reference gives an alert's acknowledged as
#     "acknowledged by a team member", with ackTime. An alert with both is
#     acknowledged then. Where there is no ackTime, or the alert was
#     acknowledged and then not, its log's earliest "Alert acknowledged ..."
#     entry whose owner isn't "system" counts.
#   - resolved: only a closed alert (status closed), at its closeTime (its
#     log's close where it has none), and resolved_by from the owner of the
#     latest close entry of its log: "system" (the reference's own example
#     of an owner that isn't a user or a team) is alert, any other owner
#     person. A close entry is of logType close or says "Alert closed ...",
#     as ../incidents/compass-closed.jq reads one; the reference doesn't say
#     what a close says. An alert closed with no such entry in the pages
#     read is kept with no resolve, said by a count.
# The alert's own owner, a person's display name, is never read.
#
# --argjson from, to: the period, in seconds; created from on, before to.
# --slurpfile logs: each alert's log, as GET /v1/alerts/{id}/logs gives it
# ({id, logs: [entry, ...]} a line, entries newest first).
# With --argjson ids true, the ids of the alerts whose log is to be read:
# those acknowledged and those closed, of an alert Lapilli takes.
include "alerts";
include "responses";
def who: (.owner // "") | if type != "string" then null else gsub("\\s"; "") as $o | if $o == "" then null elif ($o | ascii_downcase) == "system" then "alert" else "person" end end;
def closes: ((.logType // "") | ascii_downcase) == "close" or ((.log // "") | test("\\A\\s*alert (is )?closed\\b"; "i"));
def acks: (.log // "") | test("\\A\\s*alert (is )?acknowledged\\b"; "i");
def alert:
  .extraProperties as $p
  | if ($p | type) != "object" then null
    else {rule: $p.alertname, labels: ($p | del(.alertname))} | as_alert end;
def main($prefix):
  (($ARGS.named.logs // []) | group_by(.id) | map({key: .[0].id, value: [.[].logs[]?]}) | from_entries) as $logs
  | [.[] | select(.createdAt | in_period($from; $to))] as $in
  | if $ARGS.named.ids then $in[] | select(alert != null) | select(.acknowledged == true or .status == "closed") | .id
    else
      [$in[]
        | ($logs[.id] // []) as $l
        | ([$l[] | select(acks and who == "person") | .logTime] | earliest) as $logack
        | (if .status == "closed" then [$l[] | select(closes) | select(.logTime | epoch != null)] | sort_by(.logTime | epoch) | last else null end) as $c
        | (if .acknowledged == true then .ackTime | time else null end) as $ackTime
        | alert as $a
        | {r: response("\($prefix)-\(.tinyId // .id)"; [$a | select(. != null)]; .createdAt; ([$ackTime, $logack] | earliest);
            (if $c == null then null else (.closeTime | time) // ($c.logTime | time) end); ($c | if . == null then null else who end)),
           noalert: ($a == null),
           unread: (.status == "closed" and ($c == null or ($c | who) == null))}] as $all
      | {responses: ($all | map(.r) | written),
         said: {alerts: ($all | length), noalert: ($all | map(select(.noalert)) | length), unread: ($all | map(select(.unread and (.noalert | not))) | length)}}
    end;
main($prefix)
