# Responses from where they are kept

A page's response is what was done about it: whether a person acknowledged
it, and whether a person or its alert manager resolved it
([Responses](../../docs/design/responses.md)). The paging tools keep this.
Each script here writes it to stdout in the form `--responses` takes, for a
period:

| Script | Tool | Needs |
|---|---|---|
| `pagerduty.sh` | PagerDuty | `PAGERDUTY_TOKEN_FILE`; `SINCE`, `UNTIL`; `ALERT_LABELS`, `MAX_PAGES`, `MAX_REQUESTS` |
| `jsm.sh` | Jira Service Management alerts (Opsgenie's successor) | `JSM_CLOUD_ID`, `JIRA_EMAIL`, `JIRA_TOKEN_FILE`; `SINCE`, `UNTIL`; `JSM_QUERY`, `MAX_PAGES`, `MAX_LOGS` |
| `compass.sh` | Compass alerts | `COMPASS_CLOUD_ID`, `COMPASS_EMAIL`, `COMPASS_TOKEN_FILE`; `SINCE`, `UNTIL`; `COMPASS_QUERY`, `MAX_PAGES`, `MAX_LOGS` |
| `grafana-irm.sh` | Grafana IRM's OnCall alert groups | `GRAFANA_ONCALL_API`, `GRAFANA_URL`, `GRAFANA_IRM_TOKEN_FILE`; `SINCE`, `UNTIL`; `MAX_PAGES` |
| `put.sh` | serve | `LAPILLI_TOKEN_FILE`; `SINCE`, `UNTIL` |

They need `sh`, `curl` and `jq`, and read `alerts.jq` in `../incidents`, which
the incidents scripts share, so that directory stays beside this one. Tokens,
addresses and answers are held as the incidents scripts hold them
([Incidents from where they are kept](../incidents/README.md)): a token is read
from a file and passed to `curl` on stdin, an address is `https://` (or this
machine's over `http://`, or any host's with `PLAIN_HTTP=true`) with no user
before `@`, only a 2xx is taken, and an answer past 16 MiB stops the script
with nothing written. Give the tokens read-only access. Each script says on
stderr counts alone, never a name.

`SINCE` and `UNTIL` are RFC 3339 times in whole seconds, such as
`2026-11-08T00:00:00Z`, and both are needed: a page is in the period when it
was sent (created) from `SINCE` on and before `UNTIL`. `put.sh` is given the
same two, as `recipes/firings/put.sh` is, and what the ledger had from its
source of pages sent in that span is replaced by what it is given.

```
export SINCE=2026-11-08T00:00:00Z UNTIL=2026-11-09T00:00:00Z
./pagerduty.sh >responses.json && ./put.sh http://lapilli:8082 pagerduty-prod <responses.json
```

Each page is `id`, `alerts`, `sent`, and `acknowledged` when a person
acknowledged it, and `resolved` with `resolved_by` (`alert` or `person`) once it
was resolved. The ids are the tool's own (`PD-` and PagerDuty's incident
number, `JSM-` and an alert's tiny id, `CP-` for Compass, `IRM-` and an alert
group's id), of the characters serve takes. A page with no alert Lapilli takes
(no `alertname`) is left out, said by a count, as is one already written once.
**No person is named**: only whether one acted is read, and no name, id or
email of a user is copied.

## What a person and an alert are decided from

### PagerDuty

Each high-urgency incident created in the period (low-urgency ones page no
one), with its alerts as `../incidents/pagerduty.sh` reads them, and its log
entries.

- Calls: `GET /incidents` (`since`, `until`, `limit=100`, `offset`, paged by
  `more`), `GET /incidents/{id}/alerts` and `GET /incidents/{id}/log_entries`
  (`limit=100`, `offset`, paged by `more`). An incident's `acknowledgements`
  is empty once it is resolved, which is why the log is read.
- Acknowledged: the earliest `acknowledge_log_entry` whose `agent.type` is
  `user_reference`. An acknowledgement by a `service_reference` or an
  `integration_reference` is not a person's.
- Resolved: only for an incident whose `status` is `resolved`, at its last
  `resolve_log_entry`. `resolved_by` is `person` when the agent is a
  `user_reference`, and `alert` when it is a `service_reference` or an
  `integration_reference` (Alertmanager's resolve event, an auto-resolve).
  An agent of any other type leaves the resolve out.
- Evidence: PagerDuty's published OpenAPI document,
  [`reference/REST/openapiv3.json`](https://github.com/PagerDuty/api-schema/raw/refs/heads/main/reference/REST/openapiv3.json)
  in `PagerDuty/api-schema`: `LogEntry`'s `type` values, `AgentReference`'s
  `type` (`user_reference`, `service_reference`, `integration_reference`, the
  agent "that created or modified" the entry), `Channel`'s note that a
  service agent's channel is `auto`, `email`, `api`, `nagios` or `timeout` and a
  user's `email`, `sms`, `website`, `web_trigger` or `note`, the operation
  `GET /incidents/{id}/log_entries` and `Incident.acknowledgements`. The
  document gives an acknowledgement's type twice, `acknowledge_log_entry` in
  `LogEntry` and `acknowledgement_log_entry` in `AcknowledgeLogEntry`: both
  are taken. [Log entries](https://docs.pagerduty.com/developer/api/reference/rest/log-entries)

### Jira Service Management

Alerts created in the period, from the operations API the incidents recipe
already reads (`/jsm/ops/api/{cloudId}/v1/alerts`), as the same account, in
`JSM_API` (default `https://api.atlassian.com`).

- Calls: `GET /alerts` (`size=100`, `offset`, `sort=createdAt`, `order=asc`,
  `from`, `to` in seconds, and `JSM_QUERY` as `query` when given), and
  `GET /alerts/{id}/logs` (`size=100`, `after`) for each alert that is
  acknowledged or closed, five pages at most. Past `MAX_LOGS` (500) alerts the
  script stops and writes nothing: a page left without its log would be taken
  for one nobody answered. A 429 is waited out as its Retry-After says.
- Alert: the alert's extra properties, as Alertmanager's Opsgenie notifier
  writes them: `alertname` the rule, the others the labels.
- Acknowledged: `acknowledged` true with `ackTime`; the reference describes
  `acknowledged` as acknowledged "by a team member". Without an `ackTime`, the
  earliest "Alert acknowledged ..." entry of the log whose `owner` isn't
  `system` counts.
- Resolved: for an alert whose `status` is `closed`, at its `closeTime`, and
  `resolved_by` from the `owner` of the latest close entry of its log:
  `alert` when it is `system` (any case), `person` otherwise. A close entry is
  of `logType` `close` or says "Alert closed ..."; the reference says an
  owner "could be a user, team, or system" and doesn't say what a close says,
  so an alert whose log has none in the pages read is kept with no resolve.
  An integration's name as an owner is not taken for the alert: a close by
  one reads as a person's, which leaves a page "acknowledged" and never makes
  one "unanswered".
- Evidence: [List alerts, List alert logs](https://developer.atlassian.com/cloud/jira/service-desk-ops/rest/v1/api-group-alerts/)
  and their schemas (`AlertResponse`, `AlertLog`), from
  [`swagger.v3.json`](https://developer.atlassian.com/cloud/jira/service-desk-ops/swagger.v3.json).

### Compass

The same API, at `COMPASS_API` (default
`https://api.atlassian.com/compass/cloud/{cloudId}/ops/v1`), as
`../incidents/compass.sh` reads it: the same alerts, logs and decisions as
Jira Service Management's, ids `CP-`. Compass's alerts carry no `closeTime`
in its examples, so a closed alert resolves at its log's latest close
entry: "Alert closed via API by Alertmanager", by `System`, is `alert`;
"Closed by Ana", by a person, is `person`. A page is acknowledged by its
log's "Alert acknowledged via web", by a person, or, where the alert has an
`ackTime`, at that. The log is read for each alert that is acknowledged or
closed, five pages at most; `COMPASS_QUERY` narrows the search, and the
20,000 alerts Compass gives at most stop the script, as they do `compass.sh`.

### Grafana IRM

OnCall's alert groups, `GET {GRAFANA_ONCALL_API}/api/v1/alert_groups/` with
`started_at` (the period, UTC) and `page`, 50 to a page, until `next` is null;
`X-Grafana-URL` is `GRAFANA_URL`, as the reference has it
([Alert groups](https://grafana.com/docs/grafana-cloud/alerting-and-irm/irm/oncall/oncall-api-reference/alertgroups/)).
The reference doesn't document the `page` parameter; the engine's paginator
does (`FiftyPageSizePaginator`, a DRF page-number paginator).

- Alert: the alerts of the group's `last_alert.payload`, which is
  Alertmanager's webhook for an Alertmanager integration: each alert's
  `labels`, `alertname` the rule. A group whose last alert has none is left
  out.
- Acknowledged: `acknowledged_at`, when `acknowledged_by` is set: the engine
  writes `acknowledged_by` as the id of the user who did (`UserIdField` of
  `acknowledged_by_user`), and null for an acknowledgement by the alert
  source, which is not written.
- Resolved: only when `resolved_by` is set, which is a person's, for the same
  reason. **No group is written as resolved by the alert.** What `resolved_by`
  holds when the source resolved a group isn't in the reference, and the
  engine's serializer (`engine/apps/public_api/serializers/alert_groups.py`,
  `grafana/oncall`, now archived) shows it is null then, as it is for a group
  resolved by the last step of its escalation chain, by stopping
  maintenance or wiped (`resolved_by` in `alerts/models/alert_group.py`:
  `SOURCE`, `LAST_STEP`, `DISABLE_MAINTENANCE`, `WIPED`, none of which the
  API gives). So null doesn't say the alert source did it, and such a group
  is kept with no resolve, said by a count. So no Grafana IRM page is
  `unanswered`: only an acknowledgement or a resolve by a person is written.

## Not written, and why

- Anything the tool's published schema or reference doesn't settle: the
  Grafana IRM resolve by the alert source (above), Jira Service Management's
  and Compass's close entry's text, which of their owners are integrations.
- Names and ids of users. `owner`, `acknowledged_by`, `resolved_by`, a log
  entry's `agent` and an incident's assignees are decided on and dropped.
- incident.io: its alerts say whether they resolved, not who acknowledged
  them.

## In the chart

The chart mounts `recipes/incidents` and `recipes/changes` alone; these
scripts need `../incidents` beside them.
