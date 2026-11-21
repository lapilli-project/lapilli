# Incidents judged by the alerts their tool linked to them

Status: built, for review with its implementation (schema 028). Under
[GOVERNANCE.md](../../GOVERNANCE.md) this changes how a firing is judged
and adds a kind of data to the ledger, so it is a design; the maintainer
merges it with the code that builds it. "Questions for review" at the end
are open.

## Why

A firing catches an incident when it starts between `--lead` (15m) before
the incident and its end, and agrees with it on the `--match` labels both
carry ([How it judges](../how-it-judges.md)). That is a guess from time and
labels, and it goes wrong both ways:

- **An alert about another service.** A database's alert pages the
  payments team, who open an incident "Checkout errors" with `service:
  checkout`. The alert carries `service: payments-db`, so `--match service`
  keeps it from catching the incident it opened. Without `--match`, every
  alert at the time catches it.
- **An alert at the same time.** A noisy disk alert fires in the hour of
  an outage it had nothing to do with, and agrees on `service`: it is
  credited with a catch, and looks less noisy for it.

Incident tools often know better. PagerDuty groups the alerts that opened
an incident under it (`GET /incidents/{id}/alerts`); incident.io keeps the
alerts routed to an incident (`GET /v2/incident_alerts`); Rootly lists an
incident's alerts (`GET /v1/incidents/{id}/alerts`). Where an alert carries
the rule it came from (`alertname`) and its labels, that is the incident
tool saying which firing was this incident's.

## What is built

### An incident's alerts

An incident takes an optional `alerts`: each the rule it is of, by the
name its alerts carry, and some or all of the labels it had.

```yaml
incidents:
  - id: INC-41
    start: 2026-10-28T12:01:00+09:00
    labels: {service: checkout}
    alerts:
      - {rule: PaymentsDBConnections, labels: {service: payments-db}}
      - {rule: CheckoutErrors}
```

They are read from the incidents file, taken by `POST` and `PUT
/api/v1/incidents` (the incidents port's token too), and kept in the
ledger. Labels are text: a list, a map or nothing for a value is refused
(a number, too, in JSON), and so is a key other than `rule` and `labels`,
in YAML and in JSON alike, as a misspelt `label:` would link every firing
of the rule. The alerts are kept sorted, each
once, so a tool giving them in another order the next hour changes
nothing.

### How a firing is judged

A firing of an alert the incident lists is one whose rule is the alert's
rule, or that rule told apart from others of its name (`HighErrorRate
[severity=critical]` is of `HighErrorRate`, the only name the tool knows),
and whose labels agree with the alert's, as `--match` has a label one
side lacks: a label of the alert's the firing carries too has the alert's
value; one it doesn't carry, such as an external label Prometheus added as
it sent the alert, or one the receiver added, is passed over (decided by
the maintainer, 2026-11-21: a replayed firing never has those, and the
tool's alert keeps them). Such a firing catches the incident when it starts between the lead it is
judged by before it and its end, whatever their labels say on the labels
matched on: the link is stronger evidence than the labels. It still
doesn't catch when it was on before that (it was on anyway), or when its
rule is judged by nothing.

A firing the incident doesn't list catches it by time and labels, as
before. With `--linked-only` (both commands), an incident that lists
alerts is caught by those alone; one that lists none is judged as before,
so a file of incidents from two tools, one that links alerts and one that
doesn't, can be judged together.

The same holds of "an alert after the incident": a firing the incident
lists that came after its end, its condition having held in it, is its
alert too late, labels or not; with `--linked-only`, no other firing is.

Near misses are looked for by the link too. A rule the incident lists,
the labels its alerts carry (its own over its group's) not ruling out the
alert's, is looked at whatever `--match`
says, and a stretch of it whose labels agree with the alert's nearly
caught it, by a shorter `for` or a nearer number, as a firing of it would
catch it; with `--linked-only`, an incident that lists alerts has only the
rules it lists looked at. One that lists none is looked at by labels, as
before. lapilli verdict and lapilli serve look for them alike.

What doesn't change: which rules are noisy and how (a catch by a link is
a catch, and counts toward precision as any other), the record a firing
is judged by, what each rule adds.

### In the report

A catch by a firing the incident lists is marked: `linked` on
`caught_by[]` in `--json` and `/api/v1/report`, `(linked)` after the
rule's delay in the text and in `--compare`'s Markdown (`(linked, noisy)`
with a noisy rule), and `linked` in MCP's `caught_by`. An incident's `alerts` are in the report as given, and
`settings.linked_only` says how it was judged. `--redact` gives alert
rules and label values aliases, as it gives the rules' and the labels'.
docs/numbers.md lists the fields, as `TestReportFieldsStable` holds them.

### In the ledger: schema 028

- `incidents.alerts`: the alerts as JSON, sorted, `''` for none, every
  incident's before.
- `catches.linked`: 1 when a firing the incident lists made the catch, 0
  else, every catch's before.

Nothing is backfilled: no incident listed alerts before. A copy job that
starts sending them writes its incidents again, which judges their spans
again, as any change to an incident does.

### In lapilli serve

`--linked-only` is a flag, not a setting the UI changes; the Settings
page says whether it is in effect, beside the settings that are, and
`GET /api/v1/settings` as `linked_only`. The ledger keeps
what its stones were last all given by (`x.linked-only` in `meta`); a
start with it given or taken away judges every firing again, once, as a
changed lead does. Until that is done, the Settings page says so beside
it, and `GET /api/v1/settings` as `linked_only_judging`: the pages show
what was judged before. The chart's `linkedOnly` gives it, to the CronJob and
to serve alike. The incident's page marks a linked catch "(linked in
the incident tool)", and the rule's page the incidents it caught so. The
UI's form has no alerts: an incident written in the UI and given alerts
through the API keeps them when the form changes it.

### The recipes

`pagerduty.sh`, `incident-io.sh`, `rootly.sh`, `firehydrant.sh` and, with
`JSM_CLOUD_ID`, `jira.sh` read each kept incident's alerts and write those
that carry an `alertname` as its `alerts`:

| Tool | Where the labels are | Requests |
|---|---|---|
| PagerDuty | `body.details` (or `body.cef_details.details`): each alert of the group Alertmanager sent, with its labels, in `firing` and `resolved`, as 0.30.0 and later send them by default; or, from an older one given `alertname` in `pagerduty_configs`' `details`, the rule alone, with the details `ALERT_LABELS` names as labels (other details may be a description or a runbook) | one a page of 100 of an incident's alerts |
| incident.io | the attributes the alert source takes from the payload, as `GET /v2/alerts/{id}` gives them: one named `alertname`, and the others of a literal value, named as labels are | one a page of 50 of an incident's alerts, and one an alert |
| Rootly | the alert's `labels`, of which Alertmanager's carry `alertname`, `instance`, `dc` and `job`; `status` and `version` are the webhook's, `source_name` Rootly's | one a page of 100 of an incident's alerts |
| FireHydrant | the alert's `labels` (`GET /v1/incidents/{incident_id}/alerts`), arbitrary pairs; its Alertmanager source names the labels a group's alerts share `alertmanager/labels-NAME`, read with the prefix taken off | one a page of an incident's alerts |
| Jira Service Management | the alert's `extraProperties`, which keep the `details` Alertmanager's Opsgenie notifier sends: from 0.22.0, the labels its group's alerts share. The incident API that lists an incident's alerts is in beta | one a page of an incident's alert ids, and one an alert (the operations API) |

An alert with no `alertname` is left out, and how many were is said on
stderr, as counts alone. Every label an alert carries is kept: one a
replayed firing doesn't have is passed over in judging, above.
`ALERT_LABELS` narrows them to those it names, and `ALERTS=no` reads
none. They are tested against each API's documented
answers with a server standing in, not against real accounts.

## Cost and bounds

- An incident lists 100 alerts at most, each a rule of 512 bytes at most
  and 32 labels of a name of 128 bytes and a value of 256, as a firing's
  labels are kept; 16 KB in all as JSON, counted once each is listed
  once. Past them the file, or the write, is refused: the recipes keep within them, saying what they cut.
- Judging a firing against an incident is a loop over its alerts, none
  for an incident that lists none.
- The recipes make at most `MAX_ALERT_REQUESTS` (2000) requests for alerts
  a run, and past that stop and write nothing, as `MAX_PAGES` does. At
  Rootly's five requests a minute, each incident's alerts is a request
  more: set `SINCE` close, or `ALERTS=no`.

## Attacker's look

- **Who writes alerts.** The incidents file is whoever runs Lapilli; the
  API takes them from an editor and from the incidents port's token, which
  writes only what it copied in. Linking an alert to an incident can make
  any firing of that rule in the incident's span white, so a token holder
  could make a noisy rule look better: no more than writing incidents at
  the times it fired, with its labels, already could, which the token can.
  With `--linked-only`, an incident listing an alert of no rule is caught
  by nothing: what the token copied in, it can already move or delete.
- **What they make Lapilli do.** Each firing in an incident's span is held
  to its 100 alerts at most: bounded work a write, within the 20 writes a
  second the port takes. A `{"code": 500}` value, an alert with no rule
  and an empty label are refused, not guessed at.
- **Where the names end up.** Alerts are in the API's and the report's
  JSON, and in the notice of an incident a person writes
  (`incident.written`, never of one the token copies in), escaped as JSON;
  `--redact` aliases them; no page shows their rules or labels, only
  "(linked in the incident tool)" by a rule Lapilli already knows. Nothing
  of them goes to `/metrics`; MCP's `caught_by` says `linked`, as the
  report does.
- **The recipes.** What a tool answers goes into a URL only as an id
  checked to be letters and digits (and dashes, Rootly's, FireHydrant's
  and Jira Service Management's alerts'; digits, a Jira issue's and its
  page key); the token is read from its file and sent on stdin, never on
  a command line. What they say on stderr is counts, never a name a tool
  gave.

## Questions for review

Decided on 2026-11-21, by the maintainer:

- **Which labels a recipe keeps by default**: all an alert carries, and a
  label the firing lacks is passed over in judging, as `--match` passes
  one over; `ALERT_LABELS` narrows them.

Done since:

- **Near misses by link** (above, "How a firing is judged").
- **The chart** passes `--linked-only` with `linkedOnly: true`, and the
  **Settings page** says whether it is in effect.

Open:

1. **Grafana IRM** keeps alerts beside incidents; its recipe doesn't read
   them yet. FireHydrant's and Jira Service Management's are read (the
   latter from an API in beta).
2. **`--linked-only` as a setting** of the UI, tried first as `--lead` is,
   rather than a flag.
